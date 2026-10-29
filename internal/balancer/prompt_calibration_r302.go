// prompt_calibration_r302.go — R302 (2026-10-29): оценка промпта учится на
// фактических числах воркера.
//
// ЖИВОЙ ДЕФЕКТ (стенд, русский диалог в Cline). Балансер отказывал в запросе,
// который ПОМЕЩАЛСЯ в загруженное окно:
//
//	estimated = 66001 токенов (оценка балансера)
//	actual    = 17616 токенов (prompt_eval_count воркера)
//	loaded    = 32768
//
// Ошибка 3.7x. Причина — heuristic pkg/tokencount (см. ниже), который считает
// ЛЮБОЙ не-ASCII символ за один токен и добавляет по токену на пробелы и
// пунктуацию: для русского текста это завышение в разы, потому что BPE-токен
// там — это ~2-3 символа кириллицы. Итог: ложные отказы 413/503
// (`context_growth_pending`), ложные reload'ы в большее окно и «модель не
// влезает», хотя влезает.
//
// ЧТО СДЕЛАНО
//
//  1. Оценка стала калибруемой: по каждой модели хранится EMA отношения
//     actual/estimated, посчитанного по ФАКТИЧЕСКИМ токенам воркера
//     (prompt_eval_count для /api/chat|/api/generate, usage.prompt_tokens для
//     /v1/chat/completions) — см. recordTokenUsage. Оценка для решений
//     (preflight, growth guard, reload) = raw * EMA.
//  2. Калибровка переживает перезапуск: JSON рядом с state.json
//     (prompt_calibration.json), атомарная запись tmp+rename.
//  3. Запасной коэффициент, пока калибровки нет: если доля не-ASCII в тексте
//     промпта больше 0.5, оценка делится на 2.5 (потолок снижения). Для
//     латиницы поведение прежнее.
//  4. Пол: отношение НЕ опускается ниже 0.9 от лучшего наблюдённого отношения.
//     Это и есть «оценка = max(калиброванная, 0.9 * прошлый максимум)», но
//     выраженное в ОТНОШЕНИИ, а не в абсолютных токенах: абсолютный пол
//     («не меньше 0.9*MaxActual токенов») после длинной сессии раздувал бы
//     оценку КОРОТКОГО промпта выше его реального размера — то есть чинил бы
//     ложный отказ длинному запросу ценой ложного отказа короткому. Подробнее
//     в комментарии к calibrate.
//  5. Наблюдаемость: GET /api/v1/stats/prompt-calibration (ratio, samples,
//     max_ratio, max_actual, last_raw_estimated/last_actual), debug-строка на
//     каждый запрос и info-строка при заметном обновлении калибровки.
//
// ЧЕГО ЭТОТ ФАЙЛ НЕ ДЕЛАЕТ: не меняет heuristic в pkg/tokencount. Его
// осторожность (2 символа/токен для ASCII-букв, 1 для остального) остаётся
// страховкой первого запуска: недосчитать промпт опаснее, чем пересчитать —
// недосчёт приводит к обрезанному ответу, а не к отказу. Калибровка снимает
// систематическое завышение там, где есть факт.
package balancer

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"ollama-loadbalancer/pkg/logger"
)

const (
	// promptCalibVersion — версия формата файла калибровки.
	promptCalibVersion = 1

	// promptCalibFileName — имя файла рядом со state.json.
	promptCalibFileName = "prompt_calibration.json"

	// promptCalibMinSamples — сколько фактических замеров нужно, чтобы оценка
	// начала доверять калибровке. До этого работает запасной коэффициент:
	// одно наблюдение может быть случайным (кэш префикса, короткий промпт с
	// большим шаблоном), и строить на нём все решения рискованно.
	promptCalibMinSamples = 2

	// promptCalibAlphaCold — EMA на первых замерах (быстро учимся).
	promptCalibAlphaCold = 0.5
	// promptCalibAlpha — EMA после третьего замера (устойчивее к выбросам).
	promptCalibAlpha = 0.25

	// promptCalibRatioMin/Max — границы самого отношения. Нижняя не даёт
	// калибровке «схлопнуться» и разрешить заведомо невыполнимый запрос,
	// верхняя — раздуть оценку выше исходного heuristic.
	promptCalibRatioMin = 0.10
	promptCalibRatioMax = 2.0

	// promptCalibMaxRatioMinRaw — отношение попадает в MaxRatio только с
	// промптов не короче этого (токенов по raw-оценке). На коротких промптах
	// actual почти целиком состоит из шаблона чата и схем инструментов, и
	// отношение там ни о чём не говорит.
	promptCalibMaxRatioMinRaw = 512

	// promptCalibFloorFraction — пол отношения (см. calibrate).
	promptCalibFloorFraction = 0.9

	// promptCalibNonASCIIShare — с какой доли не-ASCII включается запасной
	// коэффициент.
	promptCalibNonASCIIShare = 0.5
	// promptCalibNonASCIIDivisor — во сколько раз снижать raw-оценку на
	// не-ASCII тексте, пока калибровки нет. 2.5 — потолок снижения: BPE для
	// кириллицы даёт ~2-3 символа на токен, и делить сильнее значило бы
	// систематически недосчитывать промпт.
	promptCalibNonASCIIDivisor = 2.5

	// promptCalibNoteTTL — сколько живёт «сырая» оценка в ожидании факта.
	// Дольше этого срока связывать actual с оценкой нельзя: между запросами
	// мог пройти другой запрос к той же модели.
	promptCalibNoteTTL = 10 * time.Minute

	// promptCalibSaveInterval — не чаще чем раз в столько секунд пишем файл
	// (первые замеры пишем всегда: их потеря = потеря обучения).
	promptCalibSaveInterval = 15 * time.Second

	// promptCalibLogMinChange — заметное обновление калибровки для info-лога:
	// относительное изменение отношения больше 10% и хотя бы 2 замера.
	promptCalibLogMinChange = 0.10

	// promptCalibMaxModels — потолок числа моделей в файле (защита от
	// неограниченного роста при генерации имён).
	promptCalibMaxModels = 256
)

// promptCalibEntry — калибровка одной модели.
type promptCalibEntry struct {
	// Ratio — EMA отношения actual/estimated.
	Ratio float64 `json:"ratio"`
	// MaxRatio — лучшее (наибольшее) наблюдённое отношение на промптах
	// длиннее promptCalibMaxRatioMinRaw. Нужно только для пола.
	MaxRatio float64 `json:"max_ratio"`
	// Samples — сколько фактических замеров учтено.
	Samples int `json:"samples"`
	// MaxActual — самый большой фактический промпт, который эта модель
	// считала. Диагностика и «насколько вообще бывает».
	MaxActual int `json:"max_actual"`
	// LastRaw/LastActual — последняя пара, для /api/v1/stats/prompt-calibration.
	LastRaw    int       `json:"last_raw_estimated"`
	LastActual int       `json:"last_actual"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// promptCalibFile — формат файла калибровки.
type promptCalibFile struct {
	Version int                          `json:"version"`
	Models  map[string]*promptCalibEntry `json:"models"`
}

// promptCalibPending — сырая оценка запроса, ждущая фактического числа.
type promptCalibPending struct {
	raw int
	at  time.Time
}

// promptCalibration — хранилище калибровки. Мьютекс — ЛИСТОВОЙ: под ним не
// вызывается ничего, что берёт p.mu (см. lockorder_r284_test.go), и файловые
// операции делаются на копии вне мьютекса.
type promptCalibration struct {
	mu      sync.RWMutex
	models  map[string]*promptCalibEntry
	pending map[string]promptCalibPending
	path    string
	// lastSave — когда последний раз успешно писали файл (для троттлинга).
	lastSave time.Time
}

// promptCalibrationPath — путь к файлу калибровки рядом со state.json.
// Пустой StatePath = калибровка живёт в памяти (тесты, запуск из stdin).
func promptCalibrationPath(statePath string) string {
	if statePath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(statePath), promptCalibFileName)
}

// newPromptCalibration — создать хранилище и поднять ранее сохранённое.
func newPromptCalibration(path string) *promptCalibration {
	c := &promptCalibration{
		models:  make(map[string]*promptCalibEntry),
		pending: make(map[string]promptCalibPending),
		path:    path,
	}
	c.load()
	return c
}

// load читает файл калибровки. Любая ошибка — не фатальна: работаем без
// калибровки (запасной коэффициент), потому что отказ балансера из-за
// битого вспомогательного файла был бы хуже, чем неоптимальная оценка.
func (c *promptCalibration) load() {
	if c == nil || c.path == "" {
		return
	}
	b, err := os.ReadFile(c.path)
	if err != nil {
		if !os.IsNotExist(err) {
			logger.Get().Warnw("prompt calibration: read failed, starting uncalibrated",
				"path", c.path, "error", err.Error())
		}
		return
	}
	var f promptCalibFile
	if err := json.Unmarshal(b, &f); err != nil {
		logger.Get().Warnw("prompt calibration: parse failed, starting uncalibrated",
			"path", c.path, "error", err.Error())
		return
	}
	if f.Models == nil {
		return
	}
	c.mu.Lock()
	for model, e := range f.Models {
		if e == nil || e.Samples <= 0 || e.Ratio <= 0 {
			continue
		}
		c.models[model] = e
	}
	n := len(c.models)
	c.mu.Unlock()
	logger.Get().Infow("prompt calibration: loaded from disk",
		"path", c.path, "models", n)
}

// note запоминает raw-оценку промпта этого запроса, чтобы связать её с
// фактическим числом токенов, когда воркер его вернёт.
func (c *promptCalibration) note(model string, raw int, now time.Time) {
	if c == nil || model == "" || raw <= 0 {
		return
	}
	c.mu.Lock()
	c.pending[model] = promptCalibPending{raw: raw, at: now}
	// Чистим просроченные ожидания, чтобы map не рос на моделях, чьи запросы
	// так и не дошли до воркера (ошибка соединения).
	if len(c.pending) > promptCalibMaxModels {
		for m, p := range c.pending {
			if now.Sub(p.at) > promptCalibNoteTTL {
				delete(c.pending, m)
			}
		}
	}
	c.mu.Unlock()
}

// observe записывает фактическое число токенов промпта, если для этой модели
// есть свежая raw-оценка. Возвращает новое отношение и число замеров.
func (c *promptCalibration) observe(model string, actual int, now time.Time) (float64, int, bool) {
	if c == nil || model == "" || actual <= 0 {
		return 0, 0, false
	}
	c.mu.Lock()
	pend, ok := c.pending[model]
	if ok && (pend.raw <= 0 || now.Sub(pend.at) > promptCalibNoteTTL) {
		delete(c.pending, model)
		ok = false
	}
	// Ключ, под которым учимся. Обычно это имя модели из запроса, но пути
	// учёта usage иногда называют модель иначе (алиас против загруженного
	// имени). Тогда факт привязывается к ЕДИНСТВЕННОМУ свежему ожиданию и
	// записывается под ключ ожидания — тем самым, по которому calibrate
	// читает отношение. Если ожиданий несколько, привязка была бы догадкой:
	// лучше не учиться вовсе, чем учиться не на той модели.
	key := model
	if !ok {
		if pm, pp, unique := c.uniquePendingLocked(now); unique {
			key, pend, ok = pm, pp, true
		}
	}
	if !ok {
		c.mu.Unlock()
		return 0, 0, false
	}
	delete(c.pending, key)

	sample := float64(actual) / float64(pend.raw)
	if math.IsNaN(sample) || math.IsInf(sample, 0) || sample <= 0 {
		c.mu.Unlock()
		return 0, 0, false
	}
	// Обрезаем выброс до вычисления EMA: один запрос с кэшем префикса или
	// битым usage не должен утащить отношение в бесконечность.
	sample = clampFloat(sample, 0.02, 8.0)

	e := c.models[key]
	if e == nil {
		if len(c.models) >= promptCalibMaxModels {
			c.mu.Unlock()
			return 0, 0, false
		}
		e = &promptCalibEntry{}
		c.models[key] = e
	}
	prev := e.Ratio
	alpha := promptCalibAlphaCold
	if e.Samples >= 3 {
		alpha = promptCalibAlpha
	}
	if e.Samples == 0 || e.Ratio <= 0 {
		e.Ratio = sample
	} else {
		e.Ratio = e.Ratio*(1-alpha) + sample*alpha
	}
	e.Ratio = clampFloat(e.Ratio, promptCalibRatioMin, promptCalibRatioMax)
	e.Samples++
	if pend.raw >= promptCalibMaxRatioMinRaw && sample > e.MaxRatio {
		e.MaxRatio = clampFloat(sample, 0, promptCalibRatioMax)
	}
	if actual > e.MaxActual {
		e.MaxActual = actual
	}
	e.LastRaw = pend.raw
	e.LastActual = actual
	e.UpdatedAt = now

	ratio, samples, maxRatio := e.Ratio, e.Samples, e.MaxRatio
	snapshot := c.snapshotLocked()
	save := c.shouldSaveLocked(now, samples)
	c.mu.Unlock()

	if save {
		c.persist(snapshot)
	}
	// Info-лог только на заметном изменении: иначе на каждый запрос в лог
	// уходит строка про калибровку, и шум мешает читать настоящие проблемы.
	relChange := 0.0
	if prev > 0 {
		relChange = math.Abs(ratio-prev) / prev
	}
	if samples >= promptCalibMinSamples && relChange >= promptCalibLogMinChange {
		logger.Get().Infow("prompt calibration updated",
			"model", key, "usage_model", model,
			"ratio", roundFloat(ratio, 4), "max_ratio", roundFloat(maxRatio, 4),
			"raw_estimated", pend.raw, "actual", actual, "samples", samples)
	}
	return ratio, samples, true
}

// uniquePendingLocked — единственное свежее ожидание факта (и его модель).
// Просроченные ожидания при этом удаляются. Вызывается под c.mu.
func (c *promptCalibration) uniquePendingLocked(now time.Time) (string, promptCalibPending, bool) {
	key := ""
	var found promptCalibPending
	n := 0
	for m, p := range c.pending {
		if p.raw <= 0 || now.Sub(p.at) > promptCalibNoteTTL {
			delete(c.pending, m)
			continue
		}
		n++
		if n > 1 {
			return "", promptCalibPending{}, false
		}
		key, found = m, p
	}
	if n != 1 {
		return "", promptCalibPending{}, false
	}
	return key, found, true
}

// shouldSaveLocked — писать ли файл сейчас. Вызывается под c.mu.
func (c *promptCalibration) shouldSaveLocked(now time.Time, samples int) bool {
	if c.path == "" {
		return false
	}
	if samples <= 3 {
		// Первые замеры — самое ценное, их не троттлим.
		c.lastSave = now
		return true
	}
	if now.Sub(c.lastSave) < promptCalibSaveInterval {
		return false
	}
	c.lastSave = now
	return true
}

// snapshotLocked — глубокая копия состояния под c.mu.
func (c *promptCalibration) snapshotLocked() map[string]*promptCalibEntry {
	out := make(map[string]*promptCalibEntry, len(c.models))
	for model, e := range c.models {
		cp := *e
		out[model] = &cp
	}
	return out
}

// persist — атомарная запись файла (tmp + rename). Вызывается ВНЕ c.mu.
func (c *promptCalibration) persist(models map[string]*promptCalibEntry) {
	if c == nil || c.path == "" || len(models) == 0 {
		return
	}
	b, err := json.MarshalIndent(promptCalibFile{Version: promptCalibVersion, Models: models}, "", "  ")
	if err != nil {
		logger.Get().Warnw("prompt calibration: marshal failed", "error", err.Error())
		return
	}
	dir := filepath.Dir(c.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		logger.Get().Warnw("prompt calibration: mkdir failed", "dir", dir, "error", err.Error())
		return
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		logger.Get().Warnw("prompt calibration: write failed", "path", tmp, "error", err.Error())
		return
	}
	if err := os.Rename(tmp, c.path); err != nil {
		logger.Get().Warnw("prompt calibration: rename failed", "path", c.path, "error", err.Error())
	}
}

// calibrate — сколько токенов промпта ожидать по оценке raw.
//
// Возвращает (токены, источник), где источник один из:
//   - "calibrated" — есть >= promptCalibMinSamples фактических замеров;
//   - "fallback_nonascii" — калибровки нет, текст преимущественно не-ASCII;
//   - "raw" — калибровки нет и текст ASCII (поведение до R302).
//
// ПОЧЕМУ ПОЛ ВЫРАЖЕН В ОТНОШЕНИИ, А НЕ В АБСОЛЮТНЫХ ТОКЕНАХ. Буквальная
// формула «оценка не ниже 0.9 * прошлого максимума фактических токенов»
// применяется к каждому следующему запросу, а промпты бывают разного размера:
// после сессии с промптом 17616 токенов любой короткий запрос («привет», 20
// токенов) получил бы оценку >= 15854 и был бы отклонён как не влезающий в
// окно — то есть мы починили бы ложный отказ длинному запросу ценой ложного
// отказа короткому. Пол на ОТНОШЕНИЕ делает то же самое по смыслу (не даёт
// калибровке уехать ниже 0.9 от лучшего наблюдения) и при этом не зависит от
// размера конкретного промпта.
func (c *promptCalibration) calibrate(model string, raw int, nonASCIIShare float64) (int, string) {
	if c == nil || raw <= 0 {
		return raw, "raw"
	}
	ratio, source := 1.0, "raw"
	c.mu.RLock()
	e := c.models[model]
	var ema, maxRatio float64
	var samples int
	if e != nil {
		ema, maxRatio, samples = e.Ratio, e.MaxRatio, e.Samples
	}
	c.mu.RUnlock()

	switch {
	case samples >= promptCalibMinSamples && ema > 0:
		r := ema
		if floor := promptCalibFloorFraction * maxRatio; floor > 0 && r < floor {
			r = floor
		}
		ratio, source = clampFloat(r, promptCalibRatioMin, promptCalibRatioMax), "calibrated"
	case nonASCIIShare > promptCalibNonASCIIShare:
		ratio, source = 1.0/promptCalibNonASCIIDivisor, "fallback_nonascii"
	}
	if raw <= 0 || ratio == 1.0 {
		return raw, source
	}
	out := int(math.Round(float64(raw) * ratio))
	if out < 1 {
		out = 1
	}
	return out, source
}

// PromptCalibrationView — строка ответа /api/v1/stats/prompt-calibration.
type PromptCalibrationView struct {
	Model      string     `json:"model"`
	Ratio      float64    `json:"ratio"`
	MaxRatio   float64    `json:"max_ratio"`
	Samples    int        `json:"samples"`
	MaxActual  int        `json:"max_actual"`
	LastRaw    int        `json:"last_raw_estimated"`
	LastActual int        `json:"last_actual"`
	UpdatedAt  *time.Time `json:"updated_at,omitempty"`
}

// PromptCalibrationSnapshot — калибровка всех моделей (для API и тестов).
func (p *Proxy) PromptCalibrationSnapshot() []PromptCalibrationView {
	if p == nil || p.promptCalib == nil {
		return nil
	}
	c := p.promptCalib
	c.mu.RLock()
	out := make([]PromptCalibrationView, 0, len(c.models))
	for model, e := range c.models {
		v := PromptCalibrationView{
			Model:      model,
			Ratio:      roundFloat(e.Ratio, 4),
			MaxRatio:   roundFloat(e.MaxRatio, 4),
			Samples:    e.Samples,
			MaxActual:  e.MaxActual,
			LastRaw:    e.LastRaw,
			LastActual: e.LastActual,
		}
		if !e.UpdatedAt.IsZero() {
			ts := e.UpdatedAt
			v.UpdatedAt = &ts
		}
		out = append(out, v)
	}
	c.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Model < out[j].Model })
	return out
}

// ResetPromptCalibration — забыть калибровку (все модели) и переписать файл.
// Нужен оператору после смены модели/токенизатора: старые отношения к новой
// модели не относятся.
func (p *Proxy) ResetPromptCalibration() {
	if p == nil || p.promptCalib == nil {
		return
	}
	c := p.promptCalib
	c.mu.Lock()
	c.models = make(map[string]*promptCalibEntry)
	c.pending = make(map[string]promptCalibPending)
	c.lastSave = time.Now()
	c.mu.Unlock()
	c.persist(map[string]*promptCalibEntry{})
	logger.Get().Infow("prompt calibration: reset by API")
}

// calibratedEstimateOnly — калиброванная оценка промпта БЕЗ побочных эффектов.
//
// Отличие от extractRequestMeta: не запоминает сырую оценку для обучения.
// Нужно там, где ответ содержит только текст для клиента (например, сообщение
// «контекст мал» после 413 от апстрима): фактического числа токенов в этом
// пути не будет, и pending-запись могла бы приписать будущий actual чужому
// промпту.
func (p *Proxy) calibratedEstimateOnly(body []byte, path string) int {
	meta := ExtractRequestMeta(body, path)
	if meta == nil {
		return 0
	}
	if p == nil || p.promptCalib == nil {
		return meta.EstimatedPromptTokens
	}
	out, _ := p.promptCalib.calibrate(meta.ModelName, meta.EstimatedPromptTokens, meta.PromptNonASCIIShare)
	return out
}

// extractRequestMeta — ExtractRequestMeta + калибровка оценки промпта (R302).
//
// ЭТО ЕДИНСТВЕННАЯ точка, через которую решения (preflight, growth guard,
// reload) должны читать метаданные запроса: иначе один путь будет решать по
// калиброванной оценке, а другой — по сырому heuristic, и мы снова получим
// «две правды» об одном числе (дефект, который стоил R301.3).
func (p *Proxy) extractRequestMeta(body []byte, path string) *RequestMeta {
	meta := ExtractRequestMeta(body, path)
	if meta == nil {
		return nil
	}
	p.applyPromptCalibration(meta)
	return meta
}

// applyPromptCalibration — переписать EstimatedPromptTokens на калиброванную
// оценку, сохранив сырую в EstimatedPromptTokensRaw, и запомнить её для
// обучения на фактическом числе токенов.
func (p *Proxy) applyPromptCalibration(meta *RequestMeta) {
	if meta == nil {
		return
	}
	raw := meta.EstimatedPromptTokens
	meta.EstimatedPromptTokensRaw = raw
	if raw <= 0 || p == nil || p.promptCalib == nil {
		return
	}
	calibrated, source := p.promptCalib.calibrate(meta.ModelName, raw, meta.PromptNonASCIIShare)
	// Сырую оценку запоминаем ДО того, как воркер вернёт факт: fact приходит
	// позже (recordTokenUsage) и относится именно к этому промпту.
	p.promptCalib.note(meta.ModelName, raw, time.Now())
	meta.EstimatedPromptTokens = calibrated

	logger.Get().Debugw("prompt estimate",
		"model", meta.ModelName,
		"bytes", meta.PromptBytes,
		"nonascii", roundFloat(meta.PromptNonASCIIShare, 3),
		"estimated", raw,
		"calibrated", calibrated,
		"source", source)
}

// promptTextStats — байты и доля не-ASCII символов в тексте промпта.
// Считаем по рунам: pkg/tokencount тоже считает руны, поэтому доля должна
// быть руновой, иначе суррогатные пары/эмодзи исказят картину.
func promptTextStats(text string) (bytesCount int, nonASCII float64) {
	if text == "" {
		return 0, 0
	}
	runes, non := 0, 0
	for _, r := range text {
		runes++
		if r > 0x7F {
			non++
		}
	}
	if runes == 0 {
		return len(text), 0
	}
	return len(text), float64(non) / float64(runes)
}

// clampFloat — ограничение значения отрезком [lo, hi].
func clampFloat(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// roundFloat — округление для логов/JSON.
func roundFloat(v float64, digits int) float64 {
	pow := math.Pow(10, float64(digits))
	return math.Round(v*pow) / pow
}

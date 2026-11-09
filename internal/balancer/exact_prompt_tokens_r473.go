// exact_prompt_tokens_r473.go — R473 (2026-11-06): preflight спрашивает ТОЧНОЕ
// число токенов промпта у токенизатора модели, а не только у эвристики.
//
// ЗАЧЕМ. Вся оконная арифметика балансера стоит на `pkg/tokencount` — оценке по
// символам. Живые измерения на этом стенде: эвристика давала 49731 при реальных
// 9816, 36601 при реальных ~20.7k (Cline считает по нашему же usage) — завышение
// 1.8–5×. Из завышенного числа рождались: отказы 413 (до R466), отменённый рост
// окна (до R472) и перезагрузки модели на окно БОЛЬШЕ нужного (после R472:
// грузили 40960 там, где хватало ~33k).
//
// Токенизатор есть только в воркере, а с R467 он отдаёт его наружу:
//
//	POST /api/tokenize {"model": "...", "text": "..."} →
//	{tokens, source: "tokenizer"|"estimate", model_loaded, notice}
//
// Здесь это используется так: мета запроса несёт ТОТ ЖЕ текст, который пошёл в
// эвристику (плюс сериализованные tools[], потому что они реально занимают место
// в промпте), мы отправляем его на бэкенд, где модель загружена, и при
// source=="tokenizer" заменяем оценку точным числом + небольшим запасом на
// системную преамбулу шаблона.
//
// ЧЕГО НЕ ДЕЛАЕМ:
//   - не трогаем эвристику и калибровку: они остаются фолбэком (модель не
//     загружена, воркер недоступен, таймаут, старый воркер без /api/tokenize);
//   - не обучаем калибровку по точному числу — иначе две оценки начнут
//     «подгонять» друг друга;
//   - не ходим в сеть на мелких промптах: там завышение не влияет на решения,
//     а лишний запрос на горячем пути не нужен.
package balancer

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"ollama-loadbalancer/pkg/logger"
)

// exactTokensSwitchEnvR473 — рубильник. По умолчанию ВКЛЮЧЁН: именно он убирает
// завышение, из-за которого модель грузится на лишнее окно.
const exactTokensSwitchEnvR473 = "LB_PREFLIGHT_EXACT_TOKENS"

// exactTokensMinSampleR473 — ниже этого размера образца (в байтах) точность не
// влияет на решения: 4 КБ ≈ 1–2k токенов, а пороги окон начинаются с 2–16k.
const exactTokensMinSampleR473 = 4096

// exactTokensTimeoutR473 — жёсткий предел на вызов токенизатора. Это горячий
// путь: лучше остаться с оценкой, чем держать запрос.
const exactTokensTimeoutR473 = 1500 * time.Millisecond

// exactTokensOverheadTokensR473 — запас на то, чего нет в образце: системная
// преамбула, служебные токены шаблона, разделители ролей.
const exactTokensOverheadTokensR473 = 32

// exactTokensEnabledR473 — включён ли точный подсчёт (значение по умолчанию — да).
func exactTokensEnabledR473() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(exactTokensSwitchEnvR473)))
	switch v {
	case "", "1", "true", "yes", "on", "auto":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		logger.Get().Warnw("LB_PREFLIGHT_EXACT_TOKENS: неизвестное значение, оставляю включённым",
			"value", v)
		return true
	}
}

// tokenizeResponseR473 — ответ воркера (R467), только нужные поля.
// Порядок полей — под fieldalignment (govet): указатели впереди, числа и bool
// после. На JSON не влияет (имена заданы тегами).
type tokenizeResponseR473 struct {
	Source      string `json:"source"`
	Tokens      int    `json:"tokens"`
	ModelLoaded bool   `json:"model_loaded"`
}

// exactTokensCacheTTLR473 — сколько живёт запомненный подсчёт.
//
// Зачем кэш вообще. Мету запроса собирают ДВА пути одного запроса (guard роста и
// preflight), и живой прогон показал два одинаковых вызова токенизатора на один
// запрос: `точный подсчёт токенизатором` дважды с одинаковыми числами. Плюс
// клиенты-ретраи (Cline после 503 «retry in 60 s») присылают ТО ЖЕ тело — второй
// вызов бесполезен: текст не изменился.
const exactTokensCacheTTLR473 = 2 * time.Minute

// exactTokensCacheMaxR473 — предел записей: кэш живёт в процессе, и без предела
// он превратился бы в утечку памяти на длинной сессии.
const exactTokensCacheMaxR473 = 128

type exactTokensCacheEntryR473 struct {
	at time.Time
	// seq — порядковый номер записи. Вытесняем по нему, а не по времени:
	// разрешение time.Now() на Windows ~1 мс, и десятки записей получают
	// ОДИНАКОВУЮ метку — «самая старая» по времени тогда выбирается случайно
	// (порядок обхода map), и вытеснение перестаёт быть вытеснением.
	seq    uint64
	tokens int
}

var (
	exactTokensCacheMu   sync.Mutex
	exactTokensCacheData = map[string]exactTokensCacheEntryR473{}
	exactTokensSeqR473   uint64
	// exactTokensInflightR473 — подсчёты, которые ИДУТ ПРЯМО СЕЙЧАС.
	//
	// ЗАЧЕМ. Живая проверка показала ДВА вызова токенизатора с разницей 0.8 мс
	// при HTTP-времени 1–2 мс: мету одного запроса собирают два пути (guard роста
	// и preflight) ПАРАЛЛЕЛЬНО, оба промахиваются по кэшу и оба идут в воркер.
	// Кэш помогает только последовательным вызовам, поэтому второй участник ждёт
	// результат первого (single-flight), а не дублирует работу.
	exactTokensInflightR473 = map[string]*exactInflightR473{}
)

// exactInflightR473 — один идущий подсчёт; остальные участники ждут done.
type exactInflightR473 struct {
	done   chan struct{}
	tokens int
	ok     bool
}

// exactTokensKeyR473 — ключ кэша: модель + хэш образца.
//
// Хэш sha256, а не «первые N байт»: цена (≈0.2 мс на 67 КБ) несопоставима с
// HTTP-вызовом, а совпадение ключа у РАЗНЫХ текстов дало бы чужое число токенов
// и, как следствие, неверное окно. Ключ по модели обязателен: у разных моделей
// словари разные.
func exactTokensKeyR473(model, sample string) string {
	sum := sha256.Sum256([]byte(sample))
	return model + ":" + hex.EncodeToString(sum[:16])
}

// exactTokensCacheGetR473 — запомненное число, если оно ещё годно.
func exactTokensCacheGetR473(key string) (int, bool) {
	exactTokensCacheMu.Lock()
	defer exactTokensCacheMu.Unlock()
	entry, ok := exactTokensCacheData[key]
	if !ok {
		return 0, false
	}
	if time.Since(entry.at) > exactTokensCacheTTLR473 {
		delete(exactTokensCacheData, key)
		return 0, false
	}
	return entry.tokens, true
}

// exactTokensCachePutR473 — запомнить число, вытеснив самое старое при переполнении.
func exactTokensCachePutR473(key string, tokens int) {
	exactTokensCacheMu.Lock()
	defer exactTokensCacheMu.Unlock()
	if len(exactTokensCacheData) >= exactTokensCacheMaxR473 {
		oldestKey, oldestSeq := "", uint64(0)
		for k, v := range exactTokensCacheData {
			if oldestKey == "" || v.seq < oldestSeq {
				oldestKey, oldestSeq = k, v.seq
			}
		}
		if oldestKey != "" {
			delete(exactTokensCacheData, oldestKey)
		}
	}
	exactTokensSeqR473++
	exactTokensCacheData[key] = exactTokensCacheEntryR473{
		at: time.Now(), seq: exactTokensSeqR473, tokens: tokens,
	}
}

// exactPromptTokensR473 — точное число токенов образца промпта.
//
// Возвращает (tokens, true) только когда его посчитал РЕАЛЬНЫЙ токенизатор
// загруженной модели. Во всех остальных случаях (false) вызывающий остаётся при
// эвристике — это не ошибка, а честный фолбэк.
// sourceR473 — откуда пришло число: "tokenizer" (посчитали сами), "cache"
// (запомнено), "shared" (ждали параллельного участника). Отдельный возврат
// нужен, чтобы НЕ писать Info-строку дважды на один запрос: guard роста и
// preflight собирают мету параллельно, и оба попадают в applyExactPromptTokens.
func (p *Proxy) exactPromptTokensR473(meta *RequestMeta) (tokens int, source string, ok bool) {
	if p == nil || meta == nil || meta.ModelName == "" {
		return 0, "", false
	}
	sample := meta.ExactSampleText
	if len(sample) < exactTokensMinSampleR473 {
		return 0, "", false
	}
	if !exactTokensEnabledR473() {
		return 0, "", false
	}
	key := exactTokensKeyR473(meta.ModelName, sample)
	if cached, ok := exactTokensCacheGetR473(key); ok {
		return cached, "cache", true
	}
	// R473: single-flight. Два пути одного запроса идут ПАРАЛЛЕЛЬНО, поэтому
	// проверки кэша недостаточно: второй участник обязан дождаться первого.
	if tokens, ok, shared := exactTokensJoinOrStartR473(key); shared {
		return tokens, "shared", ok
	}
	total, ok := 0, false
	defer exactTokensPublishR473(key, &total, &ok)

	backendID := p.backendWithLoadedModelR301(meta.ModelName)
	if backendID == "" {
		// Модель нигде не загружена — токенизатора нет, оценка остаётся.
		return 0, "", false
	}
	addr := p.backendHTTPAddrByID(backendID)
	if addr == "" {
		return 0, "", false
	}
	n, tokOK := p.callTokenizeR473(addr, meta.ModelName, sample)
	if !tokOK {
		return 0, "", false
	}
	total, ok = n+exactTokensOverheadTokensR473, true
	exactTokensCachePutR473(key, total)
	return total, "tokenizer", true
}

// exactTokensJoinOrStartR473 — встать в уже идущий подсчёт или начать свой.
//
// shared=true означает «результат дал другой участник»: вызывающий сразу
// возвращает его и не делает HTTP.
func exactTokensJoinOrStartR473(key string) (tokens int, ok bool, shared bool) {
	exactTokensCacheMu.Lock()
	if inf, running := exactTokensInflightR473[key]; running {
		exactTokensCacheMu.Unlock()
		<-inf.done
		return inf.tokens, inf.ok, true
	}
	exactTokensInflightR473[key] = &exactInflightR473{done: make(chan struct{})}
	exactTokensCacheMu.Unlock()
	return 0, false, false
}

// exactTokensPublishR473 — отдать результат ожидающим и снять пометку «идёт».
func exactTokensPublishR473(key string, tokens *int, ok *bool) {
	exactTokensCacheMu.Lock()
	inf, running := exactTokensInflightR473[key]
	if running {
		inf.tokens, inf.ok = *tokens, *ok
		delete(exactTokensInflightR473, key)
		close(inf.done)
	}
	exactTokensCacheMu.Unlock()
}

// callTokenizeR473 — HTTP-вызов /api/tokenize. Отдельная функция, чтобы её можно
// было проверить тестом без Proxy и без живого воркера.
func (p *Proxy) callTokenizeR473(baseURL, model, text string) (int, bool) {
	body, err := json.Marshal(map[string]string{"model": model, "text": text})
	if err != nil {
		return 0, false
	}
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(baseURL, "/")+"/api/tokenize",
		bytes.NewReader(body))
	if err != nil {
		return 0, false
	}
	req.Header.Set("Content-Type", "application/json")
	// Тот же токен, что балансер использует для остальных ручек воркера:
	// /api/tokenize закрыт authMiddleware.
	for _, key := range []string{"CPPWORKER_API_TOKEN", "LB_API_TOKEN"} {
		if tok := strings.TrimSpace(os.Getenv(key)); tok != "" {
			req.Header.Set("X-API-Token", tok)
			break
		}
	}
	client := &http.Client{Timeout: exactTokensTimeoutR473}
	resp, err := client.Do(req)
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, false
	}
	var out tokenizeResponseR473
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, false
	}
	if out.Source != "tokenizer" || out.Tokens <= 0 {
		return 0, false
	}
	return out.Tokens, true
}

// exactSampleWithToolsR473 — образец для точного подсчёта: текст сообщений плюс
// сериализованные tools[].
//
// ПОЧЕМУ С TOOLS. Схемы инструментов реально занимают место в промпте: у Cline
// их 6, у DSH — 35 (это ~12k токенов на ровном месте). Эвристика оценивала
// только текст сообщений, поэтому на агентских запросах систематически занижала
// долю tools, а калибровка потом «лечила» это общим коэффициентом. Точный
// подсчёт должен видеть то же, что увидит шаблон, иначе смысла в нём нет.
func exactSampleWithToolsR473(text string, tools any) string {
	if tools == nil {
		return text
	}
	encoded, err := json.Marshal(tools)
	if err != nil || len(encoded) == 0 || string(encoded) == "null" || string(encoded) == "[]" {
		return text
	}
	if text == "" {
		return string(encoded)
	}
	return text + "\n" + string(encoded)
}

// applyExactPromptTokensR473 — заменить калиброванную оценку точным числом, если
// оно есть. Вызывается из applyPromptCalibration — единственной точки, через
// которую решения (preflight, growth guard, reload) читают метаданные запроса.
//
// Калибровку при этом НЕ обучаем (promptCalib.note уже сделан по эвристике):
// иначе точное число и коэффициент начнут подгонять друг друга.
func (p *Proxy) applyExactPromptTokensR473(meta *RequestMeta) {
	exact, source, ok := p.exactPromptTokensR473(meta)
	if !ok {
		return
	}
	estimated := meta.EstimatedPromptTokens
	meta.EstimatedPromptTokens = exact
	meta.PromptTokensSource = "tokenizer"
	if source != "tokenizer" {
		// Не наш подсчёт: второй путь того же запроса (cache) или параллельный
		// участник (shared). Info на каждый — это два одинаковых числа в логе.
		logger.Get().Debugw("prompt tokens: число получено без нового вызова токенизатора (R473)",
			"model", meta.ModelName, "source", source,
			"estimated_tokens", estimated, "exact_tokens", exact)
		return
	}
	logger.Get().Infow("prompt tokens: точный подсчёт токенизатором (R473)",
		"request_id", meta.RequestID,
		"model", meta.ModelName,
		"estimated_tokens", estimated,
		"exact_tokens", exact,
		"sample_bytes", len(meta.ExactSampleText),
		"note", "оценка заменена фактом: решения об окне считаются по реальному числу")
}

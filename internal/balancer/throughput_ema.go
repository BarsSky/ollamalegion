// throughput_ema.go — R598 (2026-11-19): R495 S2b, второй источник для
// классификатора «во что упёрлась карта» — экспоненциальные средние скорости
// префилла и декода по завершённым запросам.
//
// ЗАЧЕМ. Классификатор R495 сегодня смотрит ТОЛЬКО на мгновенные метрики карты
// (`SM busy`, `MemBusy`, `power/limit`, throttle). Мгновенные значения врут в двух
// случаях: (а) между запросами карта почти пуста, хотя предыдущий декод упирался в
// полосу памяти; (б) порог на границе — вердикт дёргается. Скорость префилла и
// декода — это ФАКТ, полученный из завершённых запросов, а не мгновенный снимок.
//
// ЧТО СЧИТАЕТСЯ И ОТКУДА. Оба числа уже измеряются в одном месте:
// `stats_time_window.go` (`Durations()`: `PromptEval` — время промпта,
// `Eval` — время генерации) и `llamacpp_translate_resp.go:581-584`, где прямо
// записано `prompt_eval_duration = TTFT`, `eval_duration = total − TTFT`. Очередь и
// загрузка модели в «время промпта» не входят (R493-12a), поэтому скорость не
// занижается ожиданием слота — второй формулы здесь нет намеренно.
//
// ПРАВИЛА (те же, что у остальных окон проекта):
//   - нули и отрицательные в статистику НЕ попадают: ответ одним done-чанком без
//     стрима не даёт времени генерации, и «0 tok/s» испортил бы среднее навсегда;
//   - первое наблюдение становится значением EMA (без этого первая же медленная
//     генерация «прилипала» бы к нулю);
//   - α = 0.3: средняя помнит примерно 3 последних запроса — этого достаточно,
//     чтобы вердикт не дёргался, и мало, чтобы отставать от смены поведения карты;
//   - счётчики наблюдений обязательны: без них «средняя 18 tok/s» на одном запросе
//     выглядит как факт, а на тридцати — как факт другого веса.
//
// ЭТО ТОЛЬКО ДАННЫЕ. Поведение решений не меняется: значения публикуются в
// снапшоте кластера и `/api/metrics` (решение владельца — сначала «тень»).
package balancer

import (
	"sync"
	"time"
)

// emaAlphaR598 — вес нового наблюдения в экспоненциальном среднем.
const emaAlphaR598 = 0.3

// throughputSampleR598 — опубликованное среднее по одному бэкенду.
type throughputSampleR598 struct {
	PrefillTPS   float64 `json:"prefillTps"`
	DecodeTPS    float64 `json:"decodeTps"`
	PrefillCount int64   `json:"prefillSamples"`
	DecodeCount  int64   `json:"decodeSamples"`
}

// throughputEMAR598 — средние по бэкендам. Замок один на всю карту: запись идёт
// из горячего пути ответа, чтение — из снапшота кластера и `/api/metrics`.
type throughputEMAR598 struct {
	mu   sync.Mutex
	byID map[string]throughputSampleR598
}

func newThroughputEMAR598() *throughputEMAR598 {
	return &throughputEMAR598{byID: make(map[string]throughputSampleR598)}
}

// observe — учесть завершённый запрос. Неположительные скорости игнорируются
// целиком (вместе со счётчиком): «нет измерения» не равно «медленно».
func (t *throughputEMAR598) observe(backendID string, prefillTPS, decodeTPS float64) {
	if t == nil {
		return
	}
	if backendID == "" || (prefillTPS <= 0 && decodeTPS <= 0) {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	cur := t.byID[backendID]
	if prefillTPS > 0 {
		cur.PrefillTPS = emaStepR598(cur.PrefillTPS, prefillTPS, cur.PrefillCount)
		cur.PrefillCount++
	}
	if decodeTPS > 0 {
		cur.DecodeTPS = emaStepR598(cur.DecodeTPS, decodeTPS, cur.DecodeCount)
		cur.DecodeCount++
	}
	t.byID[backendID] = cur
}

// emaStepR598 — шаг EMA. count == 0 → берём наблюдение как есть.
func emaStepR598(current, sample float64, count int64) float64 {
	if count <= 0 {
		return sample
	}
	return emaAlphaR598*sample + (1-emaAlphaR598)*current
}

// snapshot — копия средних (наружу отдаём значения, а не карту под замком).
func (t *throughputEMAR598) snapshot() map[string]throughputSampleR598 {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.byID) == 0 {
		return nil
	}
	out := make(map[string]throughputSampleR598, len(t.byID))
	for k, v := range t.byID {
		out[k] = v
	}
	return out
}

// tpsFromWindowsR598 — токены в секунду из измеренных окон.
//
// tokens <= 0 или нулевая длительность → 0 («не измерено»): делить нечем, а
// выдуманное число попало бы в среднее и уже не вышло бы оттуда.
func tpsFromWindowsR598(tokens, durNanos int64) float64 {
	if tokens <= 0 || durNanos <= 0 {
		return 0
	}
	return float64(tokens) / (float64(durNanos) / float64(time.Second))
}

// throughputTrackerR598 — ленивая инициализация трекера у прокси.
func (p *Proxy) throughputTrackerR598() *throughputEMAR598 {
	if p == nil {
		return nil
	}
	p.throughputMuR598.Lock()
	defer p.throughputMuR598.Unlock()
	if p.throughputEMA == nil {
		p.throughputEMA = newThroughputEMAR598()
	}
	return p.throughputEMA
}

// recordThroughputSampleR598 — записать замер по завершённому запросу.
//
// Вызывается из того же места, где уже читаются `prompt_eval_count`/`eval_count`
// (llamacpp_transport.go), — «одно число, один источник»: скорости считаются из
// тех окон, которые клиент получает в статистике ответа.
func (p *Proxy) recordThroughputSampleR598(backendID string, promptTokens, evalTokens int64, windows statsWindowsR493) {
	if p == nil || backendID == "" {
		return
	}
	if promptTokens <= 0 && evalTokens <= 0 {
		return
	}
	d := windows.Durations()
	p.throughputTrackerR598().observe(backendID,
		tpsFromWindowsR598(promptTokens, d.PromptEval),
		tpsFromWindowsR598(evalTokens, d.Eval))
}

// throughputSnapshotR598 — опубликованные средние (для снапшота и /api/metrics).
func (p *Proxy) throughputSnapshotR598() map[string]throughputSampleR598 {
	if p == nil {
		return nil
	}
	p.throughputMuR598.Lock()
	tracker := p.throughputEMA
	p.throughputMuR598.Unlock()
	if tracker == nil {
		return nil
	}
	return tracker.snapshot()
}

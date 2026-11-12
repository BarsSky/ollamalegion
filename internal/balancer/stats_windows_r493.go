// stats_windows_r493.go — R493-12a (2026-11-12): ОКНА ВРЕМЕНИ ДЛЯ СТАТИСТИКИ
// TOKEN-Скорости.
//
// ЖАЛОБА ОПЕРАТОРА: «агент получает значения очень сильно отличающиеся от
// действительности». Разбор — docs/architecture/stats-and-context-check.ru.md.
//
// ДВА ИСКАЖЕНИЯ (оба были живыми, оба зафиксированы числами в
// stats_characterization_r493_test.go):
//
//  1. В `prompt_eval_duration` входило ОЖИДАНИЕ, а не только префилл. Окна
//     считались от `llamaStartTime` — момента, когда балансер вообще начал
//     обрабатывать запрос, то есть ДО очереди, авто-загрузки и preflight n_ctx.
//     Preflight умеет синхронно перезагружать модель (27B — минуты), и всё это
//     время записывалось в «оценку промпта»: 2000 токенов за 95 с выглядели как
//     21 t/s вместо реальных ~400 t/s.
//
//  2. В `eval_duration` не входило время РАЗМЫШЛЕНИЙ. Первый контент-чанк
//     (`firstContentTime`) ставился только на чанке CONTENT, а reasoning-дельты
//     его не двигали. При этом токены размышлений входят в `eval_count`
//     (cppworker считает их completion-токенами). Итог: 520 токенов за 5 с
//     видимого ответа = 104 t/s вместо реальных ~8 t/s — завышение в 13 раз.
//
// ПРАВИЛО ПОСЛЕ ПРАВКИ. У каждого отрезка свой источник:
//
//	RequestStart → UpstreamSent : load_duration   (очередь, авто-загрузка, preflight)
//	UpstreamSent → FirstDelta   : prompt_eval_duration (префилл)
//	FirstDelta   → End          : eval_duration   (генерация, ВКЛЮЧАЯ размышления)
//	RequestStart → End          : total_duration  (всё время запроса)
//
// `load_duration` раньше всегда отдавался нулём, то есть время загрузки модели
// пряталось внутри «оценки промпта» — ровно то, на что жаловался оператор.
// Теперь оно в своём поле, а сумма трёх отрезков равна total (до наносекунды).
package balancer

import (
	"encoding/json"
	"strings"
	"time"
)

// statsWindowsR493 — четыре момента времени, из которых считаются длительности.
//
// Нулевые значения допустимы: это пути без стрима/тесты. Каждое «неизвестно»
// имеет описанный фолбэк, а не выдуманное число.
type statsWindowsR493 struct {
	// RequestStart — начало обработки запроса балансером (до очереди и preflight).
	RequestStart time.Time
	// UpstreamSent — момент отправки запроса воркеру: после очереди, авто-загрузки
	// и preflight. Отсюда начинается префилл.
	UpstreamSent time.Time
	// FirstDelta — первый дельта-чанк ЛЮБОГО вида: content, reasoning/thinking,
	// tool_calls или text. Именно он заканчивает префилл.
	FirstDelta time.Time
	// End — момент, когда посчитана статистика (usage-чанк или done).
	End time.Time
}

// statsDurationsR493 — три отрезка в наносекундах (Ollama-формат).
type statsDurationsR493 struct {
	Total      int64
	Load       int64
	PromptEval int64
	Eval       int64
}

// Durations — посчитать отрезки и сохранить инвариант
// load + prompt_eval + eval == total.
//
// Инвариант важен не «для красоты»: клиенты (OpenWebUI, панель) показывают три
// числа рядом, и если сумма не сходится с total, оператор снова видит
// «скорости не совпадают с реальностью».
func (w statsWindowsR493) Durations() statsDurationsR493 {
	end := w.End
	if end.IsZero() {
		end = time.Now()
	}
	start := w.RequestStart
	upstream := w.UpstreamSent
	first := w.FirstDelta

	var d statsDurationsR493
	if start.IsZero() {
		// Совсем без начала запроса считать нечего: честные нули, а не «примерно».
		return d
	}
	d.Total = nonNegative(end.Sub(start))

	// Точка старта префилла: отправка воркеру, а если она неизвестна (legacy
	// вызывающий) — начало запроса. Тогда load=0 и поведение прежнее.
	prefillStart := upstream
	if prefillStart.IsZero() || prefillStart.Before(start) {
		prefillStart = start
	}
	d.Load = nonNegative(prefillStart.Sub(start))

	if first.IsZero() || first.Before(prefillStart) {
		// Первый дельта-чанк не зафиксирован (ответ пришёл одним done-чанком без
		// стрима). Всё время после отправки — «промпт», генерации не выделяем:
		// придумывать eval_duration=0 при непустом eval_count было бы ложью в
		// другую сторону, но и делить нечем. Скорость генерации в этом случае
		// клиент не получит — это честнее выдуманного числа.
		d.PromptEval = nonNegative(end.Sub(prefillStart))
		d.Eval = 0
	} else {
		d.PromptEval = nonNegative(first.Sub(prefillStart))
		d.Eval = nonNegative(end.Sub(first))
	}

	// Инвариант: сумма отрезков не может превышать total. При нормальных часах
	// это тождество; при скачках системного времени — обрезаем, чтобы не
	// показывать «prompt+eval > total».
	if sum := d.Load + d.PromptEval + d.Eval; sum > d.Total {
		over := sum - d.Total
		if d.Eval >= over {
			d.Eval -= over
		} else {
			over -= d.Eval
			d.Eval = 0
			if d.PromptEval >= over {
				d.PromptEval -= over
			} else {
				d.PromptEval = 0
			}
		}
	}
	return d
}

// nonNegative — длительность не может быть отрицательной (монотонность часов не
// гарантирована, а отрицательное время в ответе клиенту — заведомая ложь).
func nonNegative(d time.Duration) int64 {
	if d <= 0 {
		return 0
	}
	return d.Nanoseconds()
}

// firstDeltaOrContentR493 — первый дельта-чанк любого вида, а если он не был
// зафиксирован (путь без стрима) — первый ВИДИМЫЙ контент. Второй вариант хуже
// (размышления в него не попадают), но лучше нуля: иначе всё время уехало бы в
// «промпт».
func firstDeltaOrContentR493(firstDelta, firstContent time.Time) time.Time {
	if !firstDelta.IsZero() {
		return firstDelta
	}
	return firstContent
}

// statsWindowsForTransportR493 — собрать окна для транспорта.
//
// ОТДЕЛЬНАЯ ФУНКЦИЯ, А НЕ ЧЕТЫРЕ СТРОКИ В ТРАНСПОРТЕ. Здесь живёт правило выбора
// границы генерации (первый дельта-чанк ЛЮБОГО вида, а не только content), и
// именно оно было дефектом. Вынесенное правило можно проверить тестом без
// поднятия HTTP-стрима — а значит, регресс «снова считаем по контенту» ловится
// числом, а не глазами в ревью.
func statsWindowsForTransportR493(requestStart, upstreamSent, firstDelta, firstContent, end time.Time) statsWindowsR493 {
	return statsWindowsR493{
		RequestStart: requestStart,
		UpstreamSent: upstreamSent,
		FirstDelta:   firstDeltaOrContentR493(firstDelta, firstContent),
		End:          end,
	}
}

// applyStatsWindowsNativeR493 — R493-12a на НАТИВНОМ пути.
//
// ЖИВОЙ ФАКТ (стенд 2026-11-12, /api/chat → cppworker, модель грузилась ~60 с):
// клиент получил `load_duration=0`, `prompt_eval_duration=0.74`,
// `eval_duration=1.41`, `total_duration=2.15` — и `tokens_per_second` не было
// вовсе. Числа cppworker верные (его собственный отсчёт), но они не знают про
// очередь, авто-загрузку и preflight: 60 секунд ожидания для клиента не
// существовали, а сумма «отрезков» не совпадала с тем, что человек реально ждал.
//
// ПРАВИЛО: нативные отрезки cppworker (prompt/eval) остаются его — они измерены
// внутри генерации. Добавляется то, что видит ТОЛЬКО балансер: `load_duration`
// (от начала обработки до отправки воркеру) и `total_duration` = load + prompt +
// eval. Если воркер длительностей не прислал (старая сборка), считаем их нашими
// окнами — тогда оба пути дают одинаковую форму.
//
// Возвращает (строка, изменено). Не-done и не-JSON кадры не трогаем.
func applyStatsWindowsNativeR493(line string, w statsWindowsR493) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || trimmed[0] != '{' {
		return line, false
	}
	var chunk map[string]interface{}
	if err := json.Unmarshal([]byte(trimmed), &chunk); err != nil {
		return line, false
	}
	done, _ := chunk["done"].(bool)
	if !done {
		return line, false
	}
	// Окна балансера бесполезны, если он не видел ни начала обработки, ни отправки:
	// тогда оставляем кадр воркера как есть (не выдумываем время).
	if w.RequestStart.IsZero() || w.UpstreamSent.IsZero() {
		return line, false
	}
	load := nonNegative(w.UpstreamSent.Sub(w.RequestStart))

	promptNs := int64FromChunkR493(chunk, "prompt_eval_duration")
	evalNs := int64FromChunkR493(chunk, "eval_duration")
	if promptNs <= 0 && evalNs <= 0 {
		// Воркер не измерял: берём наши отрезки (prefill/eval по первому
		// дельта-чанку).
		d := w.Durations()
		promptNs, evalNs = d.PromptEval, d.Eval
	}
	total := load + promptNs + evalNs

	changed := setInt64R493(chunk, "load_duration", load)
	if setInt64R493(chunk, "prompt_eval_duration", promptNs) {
		changed = true
	}
	if setInt64R493(chunk, "eval_duration", evalNs) {
		changed = true
	}
	if setInt64R493(chunk, "total_duration", total) {
		changed = true
	}
	// tokens_per_second клиенты считают сами, но панель показывает поле: без него
	// там N/A даже при непустом eval_count.
	if evalNs > 0 {
		if count := int64FromChunkR493(chunk, "eval_count"); count > 0 {
			if setFloatR493(chunk, "tokens_per_second", float64(count)/(float64(evalNs)/1e9)) {
				changed = true
			}
		}
	}
	if !changed {
		return line, false
	}
	out, err := json.Marshal(chunk)
	if err != nil {
		return line, false
	}
	return string(out), true
}

// int64FromChunkR493 — целое из JSON-числа (float64 после Unmarshal).
func int64FromChunkR493(chunk map[string]interface{}, key string) int64 {
	if v, ok := chunk[key].(float64); ok {
		return int64(v)
	}
	return 0
}

// setInt64R493 — записать целое, вернуть true, если значение изменилось.
func setInt64R493(chunk map[string]interface{}, key string, v int64) bool {
	if cur, ok := chunk[key].(float64); ok && int64(cur) == v {
		return false
	}
	chunk[key] = v
	return true
}

// setFloatR493 — записать float, вернуть true, если значение изменилось
// (сравнение с допуском: пересчёт даёт почти то же число).
func setFloatR493(chunk map[string]interface{}, key string, v float64) bool {
	if cur, ok := chunk[key].(float64); ok {
		diff := cur - v
		if diff < 1e-6 && diff > -1e-6 {
			return false
		}
	}
	chunk[key] = v
	return true
}

// chunkHasStatsDeltaR493 — содержит ли разобранный чанк (OpenAI SSE или нативный
// Ollama) НАСТОЯЩИЙ дельта-вывод. Используется, чтобы отметить первый такой
// чанк: с него начинается генерация, и время размышлений попадает в eval, а не
// в prompt_eval.
//
// Служебные чанки (пустая delta, role-only, heartbeat с content:"") сюда НЕ
// попадают: иначе префилл «закончился» бы на прогревочном кадре.
func chunkHasStatsDeltaR493(chunk map[string]interface{}) bool {
	if len(chunk) == 0 {
		return false
	}
	// Нативный Ollama-чанк: message.content / message.thinking / message.reasoning
	// и плоский response для /api/generate.
	if msg, ok := chunk["message"].(map[string]interface{}); ok {
		if s, ok := msg["content"].(string); ok && s != "" {
			return true
		}
		if s, ok := msg["thinking"].(string); ok && s != "" {
			return true
		}
		if s, ok := msg["reasoning"].(string); ok && s != "" {
			return true
		}
		if tc, ok := msg["tool_calls"].([]interface{}); ok && len(tc) > 0 {
			return true
		}
	}
	if s, ok := chunk["response"].(string); ok && s != "" {
		return true
	}
	// OpenAI-совместимый чанк: choices[0].delta.{content,reasoning_content,
	// reasoning,thinking,tool_calls}, либо text у /v1/completions.
	choices, ok := chunk["choices"].([]interface{})
	if !ok {
		return false
	}
	for _, c := range choices {
		ch, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		if s, ok := ch["text"].(string); ok && s != "" {
			return true
		}
		delta, ok := ch["delta"].(map[string]interface{})
		if !ok {
			continue
		}
		for _, key := range []string{"content", "reasoning_content", "reasoning", "thinking"} {
			if s, ok := delta[key].(string); ok && s != "" {
				return true
			}
		}
		if tc, ok := delta["tool_calls"].([]interface{}); ok && len(tc) > 0 {
			return true
		}
	}
	return false
}

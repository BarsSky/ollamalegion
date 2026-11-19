package balancer

// generation_sample.go — R550 (T-10 аудита
// docs/plans/2026-11-19-consistency-audit.md §1.4): замер скорости генерации из
// ЖИВОГО запроса.
//
// ЖИВОЙ ДЕФЕКТ. `ModelLatencyTracker.RecordSample` вызывался ТОЛЬКО из тестов.
// Значит вся его статистика всегда была пустой, и:
//   - карточка группы («измеренная скорость декода») не могла показать число
//     никогда — только «н/д», при том что комментарий обещал «измеренное
//     значение, а если измерений ещё не было — честно н/д»;
//   - комментарии в `proxy_timeout.go` описывали 3-tier приоритет таймаутов, где
//     второй тир — «ModelLatencyTracker (автоматический расчёт на основе
//     истории)», то есть описан механизм, который не получал данных.
//
// ЧТО СДЕЛАНО. Замер пишется там, где ОБА числа реальны и уже посчитаны:
// завершение стрима (`llamacpp_transport.go`, вместе с `recordTokenUsage`) —
// число сгенерированных токенов и время старта/первого дельта. Придуманных
// значений не пишем: нулевые токены, нулевая длительность и «служебные» ответы
// короче 50 мс отбрасываются (иначе tok/s получается 10⁴–10⁵ и «измерение»
// врёт сильнее, чем отсутствие измерения).
//
// ЧЕГО НЕ ДЕЛАЕМ ОСОЗНАННО. Семплы — ДИАГНОСТИКА, а не таймауты: доктрина R83
// («таймаут — только opt-in, по умолчанию его нет») запрещает обрывать работающую
// генерацию по нашей статистике.

import "time"

// minGenerationSampleDuration — нижняя граница длительности замера.
//
// ЗАЧЕМ. Ответ из нескольких токенов отдаётся за единицы миллисекунд, и деление
// на такое время даёт десятки тысяч «токенов в секунду» — это не скорость
// модели, а шум планировщика. Такой замер испортил бы и карточку, и будущий
// анализ, поэтому он не записывается вовсе.
const minGenerationSampleDuration = 50 * time.Millisecond

// recordGenerationSample — записать замер генерации модели.
//
// completionTokens — сколько токенов РЕАЛЬНО сгенерировано (из usage/eval_count);
// start — начало апстрим-запроса; firstDelta — момент первого дельта (может быть
// нулевым, если неизвестен — тогда замер без first-byte latency).
func (p *Proxy) recordGenerationSample(model string, completionTokens int64, start, firstDelta time.Time) {
	if p == nil || p.modelLatencyTracker == nil || model == "" {
		return
	}
	if completionTokens <= 0 || start.IsZero() {
		return
	}
	elapsed := time.Since(start)
	if elapsed < minGenerationSampleDuration {
		return
	}
	sample := ModelSample{
		Timestamp:       time.Now(),
		TokensGenerated: int(completionTokens),
		DurationMs:      elapsed.Milliseconds(),
	}
	if !firstDelta.IsZero() && firstDelta.After(start) {
		sample.FirstByteLatencyMs = firstDelta.Sub(start).Milliseconds()
	}
	p.modelLatencyTracker.RecordSample(model, sample)
}

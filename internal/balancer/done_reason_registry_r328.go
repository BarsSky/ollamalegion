// done_reason_registry_r328.go — R328 (2026-10-30): ОДИН реестр допустимых
// значений done_reason и нормализация наших внутренних кодов.
//
// ЗАЧЕМ. Аудит маршрутов насчитал ≥50 точек, где присваивается done_reason, и 7
// разных значений. Контракт Ollama (docs/architecture/balancer-cppworker-contract.ru.md)
// знает четыре: stop, length, load, unload; OpenAI-поверхность — ещё content_filter.
// Наши внутренние коды ("truncated", "cancelled") клиентам не документированы:
// клиент, разбирающий done_reason, видит незнакомое значение и трактует ход как
// «модель закончила» — отсюда часть жалоб Cline на незавершённые задачи.
//
// ПОЧЕМУ НЕ МЕНЯЕМ ВСЁ СРАЗУ. Значения "truncated"/"cancelled" несут смысл для
// НАШИХ логов и метрик (R259/R297: «кто оборвал стрим»). Поэтому реестр разделяет
// два вопроса: что можно отдать клиенту на конкретной поверхности (allowed) и как
// перевести внутренний код в контрактное значение (normalize).
package balancer

// DoneReason — контрактное значение причины остановки.
type DoneReason string

const (
	DoneReasonStop         DoneReason = "stop"
	DoneReasonLength       DoneReason = "length"
	DoneReasonToolCalls    DoneReason = "tool_calls"
	DoneReasonError        DoneReason = "error"
	DoneReasonCancelled    DoneReason = "cancelled"     // Ollama: отмена генерации
	DoneReasonContentFilter DoneReason = "content_filter" // OpenAI: сработал фильтр
	DoneReasonTruncated    DoneReason = "truncated"     // НАШ внутренний код (R297)
)

// doneReasonsOllama — значения, которые может увидеть Ollama-клиент
// (/api/chat, /api/generate). Список закрытый: добавить значение можно только
// здесь, и стюард TestR328_RegistryMatchesContract это проверит.
var doneReasonsOllama = map[DoneReason]bool{
	DoneReasonStop:      true,
	DoneReasonLength:    true,
	DoneReasonToolCalls: true,
	DoneReasonError:     true,
	DoneReasonCancelled: true,
}

// doneReasonsOpenAI — значения для /v1 (finish_reason OpenAI).
var doneReasonsOpenAI = map[DoneReason]bool{
	DoneReasonStop:         true,
	DoneReasonLength:       true,
	DoneReasonToolCalls:    true,
	DoneReasonError:        true,
	DoneReasonContentFilter: true,
}

// normalizeDoneReasonForSurface — привести причину к контрактному значению для
// поверхности.
//
// Правила:
//   - контрактное значение поверхности — как есть;
//   - наш внутренний "truncated" (обрыв по НАШЕМУ таймауту/лимиту) для клиента
//     означает «ответ не дописан» — это ровно "length" в терминах Ollama/OpenAI;
//   - пустое значение — "stop" (клиент не должен получать пустую причину);
//   - незнакомое значение НЕ подменяем молча: отдаём "stop", но caller обязан
//     залогировать — иначе мы снова начнём прятать настоящую причину.
//
// Возвращает (значение, было ли значение переведено).
func normalizeDoneReasonForSurface(surface string, reason string) (string, bool) {
	if reason == "" {
		return string(DoneReasonStop), true
	}
	allowed := doneReasonsOllama
	if surface == "openai" {
		allowed = doneReasonsOpenAI
	}
	candidate := DoneReason(reason)
	if allowed[candidate] {
		return reason, false
	}
	if candidate == DoneReasonTruncated {
		// Наш внутренний код: для клиента это усечение по лимиту.
		return string(DoneReasonLength), true
	}
	// "load"/"unload" — валидные значения Ollama, но не для завершения
	// генерации; всё прочее — незнакомый код, отдаём безопасный stop.
	return string(DoneReasonStop), true
}

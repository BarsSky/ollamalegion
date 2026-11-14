// stream_line_plan_r493.go — R493, этап 6 плана «два мозга» (2026-11-14):
// ЕДИНОЕ РЕШЕНИЕ о том, что видит клиент, по одной строке апстрима.
//
// ЗАЧЕМ. Два SSE-цикла /v1 (`proxyRequestOpenAIStreamingLegacy` и
// `proxyRequestOpenAIStreamingHijacked`) делали одно и то же ПО-РАЗНОМУ, и
// расхождения были не косметикой:
//
//   - пустые строки-разделители SSE: hijack их пропускал (Round 35e — иначе
//     strict chunked-парсеры падали), legacy писал как отдельные записи;
//   - граница события `\n\n`: на chunked-транспорте её восстанавливал цикл,
//     дописывая `\n` к data-строке; на ResponseWriter она получалась сама;
//   - кадр-ошибка воркера (R294): на hijack-пути ПОСЛЕ него отдавался терминатор
//     и поток закрывался, на legacy — просто пересылался дальше, и клиент ждал
//     EOF;
//   - придержанный finish (R261), ремонт аргументов (R186), нормализация
//     формы вызова (R186/R254) и финальный `[DONE]` (R252) дублировались
//     дословно, поэтому правка в одном месте не доезжала до другого.
//
// Здесь одно место, где решается «что уходит клиенту»; транспортные отличия
// сведены к одному параметру (форма кадра), а правило одно для всех.
//
// ГРАНИЦА ЭТОГО ШАГА (честно): сведено РЕШЕНИЕ по строке для двух /v1-циклов.
// NDJSON-цикл (поверхность Ollama) и авто-стрим (R258) переводятся на это же
// решение следующим шагом — у них другой формат кадра (JSON-строка, а не SSE), и
// смешивать их в один шаг нельзя: именно так и появляются «почти одинаковые»
// реализации.
package balancer

// streamLineTransportR493 — как транспорт отдаёт клиенту один кадр. От этого
// зависят РОВНО две вещи: пропуск пустых строк и восстановление границы события.
type streamLineTransportR493 int

const (
	// streamLineTransportPlainR493 — обычный http.ResponseWriter (framing за
	// net/http): пустая строка пишется как есть, она и есть граница события.
	streamLineTransportPlainR493 streamLineTransportR493 = iota
	// streamLineTransportChunkedR493 — сырое соединение после Hijack: каждый
	// кадр — отдельный chunk, поэтому пустой кадр из одного `\n` недопустим
	// (ломает strict-парсеры), а границу события приходится дописывать в кадр.
	streamLineTransportChunkedR493
)

// streamLinePlanR493 — что сделать с одной строкой, прочитанной из апстрима.
//
// Порядок записи: Pre → Line → Post, затем, если Terminate, поток закрывается.
// Поля-срезы, а не один общий поток байт: у кадров разное происхождение
// (фильтр размышлений vs сама строка воркера), и склеивать их — значит снова
// привязать цикл к порядку внутри чужой логики.
//
// Порядок полей — под fieldalignment (линт): срезы первыми.
type streamLinePlanR493 struct {
	Pre       [][]byte
	Line      []byte
	Post      [][]byte
	Terminate bool
}

// planStreamLineR493 — единственное место, где решается «что видит клиент».
//
// egress и repair — состояние на весь стрим (передаются указателями: разметка
// размышлений и ремонт аргументов живут дольше одной строки), schemas — схемы
// инструментов из запроса клиента (нужны для приведения аргументов).
//
// Возвращает план; вызывающий обязан записать его через свой сток В ЭТОМ
// порядке и только затем закрывать поток по Terminate.
func planStreamLineR493(
	line []byte,
	egress *reasoningEgressState,
	schemas map[string]*toolParamSchema,
	repair *toolRepairState,
	tr streamLineTransportR493,
) streamLinePlanR493 {
	var plan streamLinePlanR493

	lineToWrite, _ := filterAssistantEgressForClient(line, egress)
	// Пустая строка на chunked-транспорте: пропускаем ЦЕЛИКОМ (Round 35e).
	// До этой проверки строка уже прошла фильтр размышлений: если фильтр
	// удерживал состояние, он мог превратить пустую строку в содержимое, и
	// тогда это уже не разделитель.
	if tr == streamLineTransportChunkedR493 && isEmptyStreamLineR493(lineToWrite) {
		return plan
	}
	if isOpenAISSEDataLine(lineToWrite) {
		if payload := extractSSEDataPayload(lineToWrite); payload != nil {
			// R186: извлечение вызова из текста + форма OpenAI (id/type/index) и
			// приведение аргументов к схеме инструмента из запроса клиента.
			if modified := normalizeOpenAISSEPayload(payload, schemas, repair); modified != nil {
				lineToWrite = rewriteSSEDataPayload(lineToWrite, modified)
			}
		}
	}
	// R261: finish-кадр придерживаем — объяснение пустого ответа обязано идти
	// ПЕРЕД finish_reason (строгие клиенты не читают контент после finish).
	if egress.tryHoldFinishFrame(lineToWrite) {
		return plan
	}
	// R261: [DONE] от воркера — последняя точка, где ещё можно отдать
	// придержанный finish и объяснение. Иначе они окажутся ПОСЛЕ [DONE], и клиент
	// увидит два конца потока (поймано тестом R258).
	if isSSEDoneLine(lineToWrite) {
		plan.Pre = append(plan.Pre, finalizeStreamFramesR493(egress)...)
	}
	if tr == streamLineTransportChunkedR493 && isOpenAISSEDataLine(lineToWrite) {
		// Round 32 #17: ReadBytes отдаёт `data: {...}\n`, а событие в SSE
		// разделяется пустой строкой. На chunked-транспорте пустые строки не
		// пишутся (см. выше), поэтому границу восстанавливаем в самом кадре.
		lineToWrite = append(lineToWrite, '\n')
	}
	plan.Line = lineToWrite

	// R294: кадр-ошибка воркера закрывает поток. Без этого клиент /v1 висел до
	// своего таймаута, хотя причина уже пришла. Хвост фильтра (придержанный
	// finish/объяснение) отдаётся ПОСЛЕ кадра-причины — иначе клиент прочитает
	// объяснение раньше ошибки.
	if isOpenAISSEDataLine(lineToWrite) {
		if payload := extractSSEDataPayload(lineToWrite); payload != nil && isUpstreamErrorPayload(payload) {
			plan.Post = append(plan.Post, finalizeStreamFramesR493(egress)...)
			plan.Terminate = true
		}
	}
	return plan
}

// finalizeStreamFramesR493 — кадры, которые обязаны уйти клиенту В КОНЦЕ стрима
// (и перед терминатором, если поток закрывается по кадру-ошибке): сброс
// придержанного tail'а, объяснение «ответ ушёл целиком в размышления» и
// придержанный finish (R261).
//
// Почему здесь, а не в каждом цикле: это ровно тот же набор правил, что и внутри
// плана (та же граница события R277), и в двух циклах он дублировался дословно.
// Один список кадров — одно место, где решается, что клиент увидит в конце.
func finalizeStreamFramesR493(egress *reasoningEgressState) [][]byte {
	frames := egress.finalizeFrames()
	if len(frames) == 0 {
		return nil
	}
	out := make([][]byte, 0, len(frames))
	for _, fr := range frames {
		// R277: кадр из фильтра приходит без второго перевода строки —
		// восстанавливаем границу события, иначе строгий клиент склеивает его с
		// соседним кадром и падает на разборе JSON.
		out = append(out, withSSEEventBoundary(fr))
	}
	return out
}

// isEmptyStreamLineR493 — строка, которая на chunked-транспорте не должна стать
// отдельным кадром: пустая или ровно `\n`.
//
// Условие ДОСЛОВНО прежнее (hijack-цикл, Round 35e): `len==0 || (len==1 && '\n')`.
// Расширять его (например, считать пустой и `"\r\n"`) нельзя без отдельного
// решения: это изменило бы байты CRLF-стримов, а такой правки в этом шаге нет.
func isEmptyStreamLineR493(line []byte) bool {
	return len(line) == 0 || (len(line) == 1 && line[0] == '\n')
}

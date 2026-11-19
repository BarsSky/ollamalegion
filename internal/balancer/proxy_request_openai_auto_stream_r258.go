package balancer

// proxy_request_openai_auto_stream_r258.go — R258 (2026-10-28): не-стрим
// агентские запросы не «копят молча».
//
// ЖИВОЙ ФАКТ (DSH, deepseek-harness). Часть запросов агент шлёт с `stream=false`.
// Балансер уводит их в auto-stream workaround (proxy_request_openai_auto_stream.go)
// и копит ВЕСЬ ответ до конца, отдавая клиенту один JSON. Для агентского цикла это
// ровно то, что оператор описал как «замер, потом всё сразу»: клиент не видит ни
// одного байта, пока модель не закончит, и упирается в свой таймаут на длинном
// ходе (у Cline это «The operation timed out.» через ~480 с — при том что балансер
// и воркер работали штатно).
//
// ЧТО ЗДЕСЬ СДЕЛАНО:
//  1. Если клиент ГОТОВ принимать SSE (`Accept: text/event-stream`), незачем
//     копить: отдаём тот же поток кадрами (с фильтрацией reasoning через единую
//     точку R256) и закрываем ровно одним `data: [DONE]`.
//  2. Если не готов — буферизация остаётся (это контракт non-stream), но она
//     ЧЕСТНАЯ: одна warn-строка с параметрами и понятная ошибка при превышении
//     лимитов вместо бесконечного ожидания.
//
// ПОЧЕМУ НЕ «ВСЕГДА SSE». Клиент, который просит `stream=false`, ждёт цельный
// JSON и вправе не уметь SSE: подсунуть ему поток — сломать разбор. Поэтому
// решение принимается по `Accept`, а не по нашему желанию.

import (
	"bufio"
	"fmt"
	"net/http"
	"strings"
	"time"

	"ollama-loadbalancer/internal/settings"
	"ollama-loadbalancer/pkg/logger"
)

const (
	// Предел ОБЪЁМА: 16 МиБ — заведомо больше разумного non-stream ответа (даже
	// 100k токенов текста ≈ 400 КБ, а base64-картинки в non-stream ответе не
	// появляются).
	//
	// R548 (T-4 аудита согласованности): предел ДЛИТЕЛЬНОСТИ по умолчанию
	// ВЫКЛЮЧЕН (0). До R548 здесь стояли 300 с, и это рубило ЗАКОННУЮ длинную
	// генерацию: 27B на A10 выдаёт единицы токенов в секунду, ответ на 8k токенов
	// идёт минутами, а клиенты (Cline и подобные) рвут соединение на ~480 с — то
	// есть балансер отдавал ошибку РАНЬШЕ клиента, да ещё и «свою». Плюс
	// доктрина проекта: таймаут — осознанное решение оператора, а не дефолт
	// (docs/architecture/request-lifecycle.ru.md, раздел про таймауты).
	//
	// От ЗАВИСШЕГО воркера на этом пути защищает не предел длительности, а сторож
	// ТИШИНЫ (R297), который теперь ставится и здесь: тишина ≠ работа.
	// Кому нужен именно предел — задаёт `LB_OPENAI_AUTO_STREAM_MAX_SEC`.
	defaultAutoStreamMaxSec   = 0
	defaultAutoStreamMaxBytes = 16 << 20
)

// autoStreamLimits — пределы буферизации non-stream ответа (R258).
//
// env: LB_OPENAI_AUTO_STREAM_MAX_SEC / LB_OPENAI_AUTO_STREAM_MAX_BYTES.
// Значение <= 0 отключает соответствующий предел (осознанный выбор оператора).
//
// R278: оба предела берутся из реестра настроек (override из WebUI → env →
// дефолт) и применяются без рестарта.
func autoStreamLimits() (time.Duration, int64) {
	maxSec := int64(defaultAutoStreamMaxSec)
	if v, ok := settings.HotInt("LB_OPENAI_AUTO_STREAM_MAX_SEC"); ok {
		maxSec = v
	}
	maxBytes := int64(defaultAutoStreamMaxBytes)
	if v, ok := settings.HotInt("LB_OPENAI_AUTO_STREAM_MAX_BYTES"); ok {
		maxBytes = v
	}
	return time.Duration(maxSec) * time.Second, maxBytes
}

// clientAcceptsSSE — готов ли клиент к SSE.
//
// `Accept: text/event-stream` в запросе с `stream:false` — это приглашение
// отдать поток (так делают агентские клиенты: им нужен прогресс, а не формальная
// приверженность non-stream контракту). Проверяем именно заголовок, а не
// догадку по User-Agent: догадка ошиблась бы на первом же новом клиенте.
func clientAcceptsSSE(r *http.Request) bool {
	if r == nil {
		return false
	}
	accept := strings.ToLower(r.Header.Get("Accept"))
	return strings.Contains(accept, "text/event-stream")
}

// autoStreamErrorBody — понятная ошибка вместо бесконечного ожидания.
//
// Формат — OpenAI-совместимый `{"error":{...}}`, как у orEmptyUpstreamError
// (R220): клиент показывает текст оператору, а не «пустой ответ».
func autoStreamErrorBody(kind string, detail string) []byte {
	msg := "non-stream response was not completed: " + detail +
		". Клиент просит stream=false, поэтому балансер обязан собрать ответ целиком; " +
		"на длинных ответах это упирается в лимит ожидания — используйте stream=true " +
		"(агентские клиенты так и делают) или поднимите LB_OPENAI_AUTO_STREAM_MAX_SEC/MAX_BYTES"
	return []byte(fmt.Sprintf(
		`{"error":{"message":%q,"type":"upstream_error","code":%q}}`, msg, kind))
}

// sizeBytes — сколько уже накоплено (для лимита по размеру).
//
// Считаем только то, что реально осядет в ответе: content, reasoning и
// сериализованные tool_calls. Иначе лимит срабатывал бы на служебных полях
// чанков, которые в ответ не попадают.
func (a *openAIStreamAccumulator) sizeBytes() int64 {
	if a == nil {
		return 0
	}
	n := int64(a.content.Len() + a.reasoning.Len())
	for _, tc := range a.toolCalls {
		if fn, ok := tc["function"].(map[string]interface{}); ok {
			if args, ok := fn["arguments"].(string); ok {
				n += int64(len(args))
			}
			if name, ok := fn["name"].(string); ok {
				n += int64(len(name))
			}
			continue
		}
		// Нестандартная форма tool_call — оцениваем грубо, лучше переоценить.
		n += 128
	}
	return n
}

// warnAutoStreamBuffering — одна warn-строка о том, что ответ будет собран
// целиком, с параметрами, по которым оператор поймёт, чего ждать.
//
// Уровень warn, а не info: это не «нормальная работа», а компромисс, который
// имеет цену (клиент не видит прогресса). Ровно один раз на запрос.
func warnAutoStreamBuffering(backendID, model string, reqBody []byte, hasTools bool) {
	logger.Get().Warnw("R258: non-stream agent request is buffered — client sees no progress until the end",
		"backend", backendID,
		"model", model,
		"has_tools", hasTools,
		"request_bytes", len(reqBody),
		"hint", "stream=true avoids buffering; limits: LB_OPENAI_AUTO_STREAM_MAX_SEC/MAX_BYTES")
}

// hasClientTools — есть ли в запросе инструменты (по сырому body).
//
// Отдельная функция, а не разбор модели: в warn-строке нужен именно факт
// «агентский запрос или нет» — по нему оператор отличает проблему агентов от
// обычного non-stream клиента.
func hasClientTools(body []byte) bool {
	return countClientTools(body) > 0
}

// streamUpstreamSSEToClient — R258: отдать поток клиенту кадрами вместо накопления.
//
// Здесь нет аккумулятора и нет финального JSON: клиент, приславший
// `Accept: text/event-stream` при `stream:false`, получает ровно то, что идёт от
// воркера, — но кадр проходит через ЕДИНУЮ точку выхода R256
// (filterAssistantEgressForClient), иначе размышления снова утекут в content
// (это уже случалось с self-proxy путём R253).
//
// Завершение — ровно один `data: [DONE]` (общий помощник R252): клиент обязан
// видеть конец потока, даже если воркер закрылся без маркера.
func (p *Proxy) streamUpstreamSSEToClient(
	w http.ResponseWriter,
	r *http.Request,
	upstreamResp *http.Response,
	backendID, modelName string,
	upstreamStartedAt time.Time,
) error {
	flusher, _ := w.(http.Flusher)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("X-R258-NonStream-As-SSE", "1")
	p.addModelCapabilitiesHeaders(w, modelName)
	w.WriteHeader(http.StatusOK)
	if flusher != nil {
		flusher.Flush()
	}

	// Единый вход стрима (R252): сессия активна, клиент виден в мониторе.
	stream := startStreamSession(p, r, modelName)
	defer stream.Stop()

	logger.Get().Infow("R258: buffering disabled — client accepts SSE, streaming through",
		"backend", backendID, "model", modelName)

	// R493 (этап 6): решение «что видит клиент» — общее (planStreamLineR493), запись
	// — через общий сток кадра. Своя копия правила здесь разошлась с основным
	// путём дважды: не приводилась форма tool_call (R186 — клиент не склеивал
	// фрагменты вызова) и не закрывался поток по кадру-ошибке воркера (R294).
	egress := &reasoningEgressState{}
	schemas := p.toolSchemasForRequest(r)
	repair := &toolRepairState{}
	sink := newPlainStreamSinkR493(w)
	scanner := bufio.NewScanner(upstreamResp.Body)
	// R259: тот же конфигурируемый лимит строки, что и в основном тракте —
	// иначе на большом tool-call здесь получился бы ложный «обрезанный поток».
	scanner.Buffer(make([]byte, 0, 64*1024), streamMaxLineBytes())

	frames := 0
	writeFrame := func(fr []byte) bool {
		if _, err := sink.WriteFrame(fr); err != nil {
			logger.Get().Infow("R258: client write failed while streaming",
				"backend", backendID, "error", err, "frames", frames)
			return false
		}
		frames++
		return true
	}

	for scanner.Scan() {
		select {
		case <-r.Context().Done():
			logger.Get().Infow("R258: client cancelled while streaming",
				"backend", backendID, "frames", frames,
				"elapsed_sec", time.Since(upstreamStartedAt).Seconds())
			// Клиент ушёл — дописывать [DONE] некуда и незачем.
			return nil
		default:
		}

		line := scanner.Text()
		// Этот путь отдаёт ТОЛЬКО SSE-кадры (R258): не-data строки и пустые
		// разделители сюда не доходят как кадры.
		if line == "" || !strings.HasPrefix(line, "data: ") {
			continue
		}
		plan := planStreamLineR493([]byte(line), egress, schemas, repair, streamLineTransportPlainR493)
		for _, fr := range plan.Pre {
			if !writeFrame(fr) {
				return nil
			}
		}
		if plan.Line != nil {
			// Граница события: строке из Scanner перевод строки не принадлежит.
			frame := make([]byte, 0, len(plan.Line)+2)
			frame = append(frame, plan.Line...)
			frame = append(frame, '\n', '\n')
			if !writeFrame(frame) {
				return nil
			}
			if plan.Terminator {
				// R252: факт о потоке, а не переменная цикла — иначе финальная
				// гарантия допишет второй [DONE] (поймал golden-тест этого шага).
				sink.NoteTerminator()
			}
		}
		if plan.Terminate {
			// R294: кадр-ошибка воркера закрывает поток — клиент не ждёт EOF.
			for _, fr := range plan.Post {
				_ = writeFrame(fr)
			}
			if !sink.TerminatorSent() {
				_ = writeFrame([]byte(sseDoneFrame))
				sink.NoteTerminator()
			}
			logger.Get().Infow("R258: upstream error frame — stream closed (R294)",
				"backend", backendID, "model", modelName)
			return nil
		}
	}

	if err := scanner.Err(); err != nil {
		// Это наш лимит строки или обрыв upstream — в обоих случаях клиенту нужен
		// валидный конец потока, а не молча закрытое соединение.
		logger.Get().Warnw("R258: upstream stream ended with error",
			"backend", backendID, "model", modelName, "error", err, "frames", frames)
	}
	// R261: закрываем фильтр — хвост, объяснение пустого ответа, придержанный
	// finish. Список кадров и граница события — в одном месте (finalizeStreamFramesR493).
	for _, fr := range finalizeStreamFramesR493(egress) {
		if !writeFrame(fr) {
			return nil
		}
	}
	// Ровно один [DONE]: решение — по ФАКТУ записи через сток (R252), а не по
	// последнему кадру: апстрим присылает после [DONE] пустую строку-разделитель.
	if !sink.TerminatorSent() {
		_ = writeFrame([]byte(sseDoneFrame))
		sink.NoteTerminator()
	}

	logger.Get().Infow("R258: SSE passthrough complete",
		"backend", backendID, "model", modelName, "frames", frames,
		"elapsed_sec", time.Since(upstreamStartedAt).Seconds())
	return nil
}

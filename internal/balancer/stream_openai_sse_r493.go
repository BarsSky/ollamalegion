// stream_openai_sse_r493.go — R493, этап 6 (2026-11-14): ОДИН цикл SSE-ответа
// OpenAI для обоих транспортов.
//
// ЧТО БЫЛО. `/v1/chat/completions` в стриме обслуживали ДВЕ почти одинаковые
// функции: `proxyRequestOpenAIStreamingLegacy` (ResponseWriter) и
// `proxyRequestOpenAIStreamingHijacked` (сырое соединение после Hijack). Тела
// циклов совпадали на 90 %, но каждая имела свои особенности, и они разъехались:
//
//   - сторож зависания (R297/R298) был только на hijack-пути: клиент без Hijack
//     при мёртвом, но НЕ закрытом апстриме ждал вечно;
//   - явный приказ об отмене генерации воркеру (R344) тоже был только там: без
//     него воркер продолжал жечь GPU после ухода клиента;
//   - кадр-ошибка воркера закрывал поток только там (R294) — до тех пор, пока
//     решение по строке не свели в planStreamLineR493.
//
// Теперь цикл ОДИН. Транспорт передаёт свои возможности набором замыканий
// (`sseStreamRunR493`): куда писать кадр, как отправить keepalive, как закрыть
// поток терминатором, как отменить генерацию. Решение «перехватывать соединение
// или писать в ResponseWriter» принимает сток (`openOpenAIStreamSinkR493`), а не
// вызывающий: у цикла больше нет `http.Hijacker`.
package balancer

import (
	"bufio"
	"io"
	"net/http"
	"time"

	"ollama-loadbalancer/pkg/logger"
)

// sseStreamRunR493 — всё, чем транспорты различаются в цикле SSE.
//
// Порядок полей — под fieldalignment (линт): указатели и каналы первыми, строки
// последними блоками.
type sseStreamRunR493 struct {
	// clientGone закрывается, когда клиент ушёл: на обычном транспорте это
	// r.Context(), на hijack — канал polling-горутины (она ловит разрыв за <100 мс,
	// см. proxy_request_hijack.go).
	clientGone <-chan struct{}
	// schemas — схемы инструментов из запроса клиента (R186): нужны плану, чтобы
	// привести аргументы вызова. Считаются один раз в начале запроса.
	schemas map[string]*toolParamSchema
	// cancel — приказ воркеру прекратить генерацию (R344). nil = транспорт не
	// умеет (обычный ResponseWriter полагается на отмену ctx).
	cancel func(reason string)
	// writeFrame — записать кадр клиенту (единственная точка записи транспорта).
	writeFrame func([]byte) error
	// heartbeat — отправить keepalive-комментарий в форме транспорта.
	heartbeat func() error
	// writeError — кадр-причина при ошибке чтения апстрима.
	writeError func(message string) bool
	// ensureDone — гарантировать ровно один терминатор потока (R252).
	ensureDone func(lastSent []byte) bool
	// noteTerminator — зафиксировать в стоке, что терминатор записан (факт о
	// потоке; на этом расхождении R252 дописывал второй `data: [DONE]`).
	noteTerminator func()
	// logTag — префикс записей в лог (различить транспорт при разборе).
	logTag string
	// backendID/model — для логов и отмены генерации.
	backendID string
	model     string
	// transport — форма кадра: от неё зависят пропуск пустых строк и граница
	// события (см. stream_line_plan_r493.go).
	transport streamLineTransportR493
}

// runOpenAISSEStreamR493 — цикл SSE: читает строки апстрима, пишет то, что решил
// planStreamLineR493, и закрывает поток ровно одним терминатором на любом исходе.
func (p *Proxy) runOpenAISSEStreamR493(run sseStreamRunR493, resp *http.Response) error {
	heartbeatInterval := p.getHeartbeatInterval()

	// Порядок полей — под govet fieldalignment (линт): error содержит два слова
	// (тип и данные), и если поставить его первым, префикс под сканирование — 24
	// байта вместо 40.
	type readResult struct {
		err  error
		line []byte
	}
	// R243/R256: состояние фильтра на весь стрим (размышления → reasoning_content).
	egressFilter := &reasoningEgressState{}
	dataCh := make(chan readResult, 1)
	go func() {
		reader := bufio.NewReader(resp.Body)
		for {
			line, err := reader.ReadBytes('\n')
			dataCh <- readResult{line: line, err: err}
			if err != nil {
				return
			}
		}
	}()

	done := false
	// R252: последний отданный клиенту кадр (для гарантии ровно одного [DONE]).
	var lastSSELine []byte
	// R254: состояние ремонта аргументов на этот стрим (диагностика — один раз).
	toolRepair := &toolRepairState{}
	heartbeatTicker := time.NewTicker(heartbeatInterval)
	defer heartbeatTicker.Stop()

	// R297/R298: сторож тишины создаётся ЗДЕСЬ, а не в одном из транспортов.
	//
	// История дефекта: R297 завёл сторож только в hijack-пути, и путь без Hijack
	// (HTTP/2, обёрнутый writer) мог висеть вечно на мёртвом, но не закрытом
	// апстриме. R298 поставил стюард, который требует решения для каждого
	// стримингового файла. Теперь решение одно и то же для обоих транспортов:
	// сторож живёт в общем цикле.
	//
	// Первый кадр не обрывается по умолчанию (streamFirstFrameAbortTimeout = 0):
	// prefill на CPU идёт минутами, и это честное ожидание. Порог задаётся
	// настройками, а не константой в этом файле.
	stallWatch := newStreamStallWatch(streamStallAbortTimeout(), streamFirstFrameAbortTimeout(), nil)
	stallWatch.Start()
	defer stallWatch.Stop()
	// Выключенный сторож = nil-канал: case ниже никогда не срабатывает, нулевая
	// цена на путях без сторожа.
	var stallCh <-chan stallEvent
	if stallWatch.Enabled() {
		stallCh = stallWatch.Events()
	}

	for !done {
		select {
		case <-run.clientGone:
			logger.Get().Infow(run.logTag+": client disconnected, closing upstream",
				"backend", run.backendID)
			if run.cancel != nil {
				run.cancel("client_cancel")
			}
			return nil

		case <-heartbeatTicker.C:
			// SSE-heartbeat: комментарий, который клиент игнорирует, но не закрывает
			// соединение по idle-таймауту.
			if werr := run.heartbeat(); werr != nil {
				logger.Get().Warnw(run.logTag+": heartbeat write failed",
					"backend", run.backendID, "error", werr)
				// R344: ошибка записи клиенту — его больше нет (broken pipe). Отмену
				// шлём здесь же, иначе воркер продолжает генерацию после ухода
				// клиента (класс R184) и жжёт GPU впустую.
				if run.cancel != nil {
					run.cancel("client_cancel")
				}
				return nil
			}

		case ev := <-stallCh:
			// R298: сторож тишины. Отдаём кадр-причину и РОВНО ОДИН терминатор,
			// иначе клиент видит оборванный поток. Сторож приходит своей горутиной и
			// не зависит от того, крутится ли цикл (иначе проверка не срабатывала на
			// мёртвом сокете, где keepalive «успешно» буферизовался).
			logger.Get().Warnw(run.logTag+": upstream stalled — closing stream with terminator (R298)",
				"backend", run.backendID, "model", run.model,
				"prefill", ev.prefill, "stalled_sec", int(ev.stalled.Seconds()))
			for _, fr := range abortFramesR296("/v1/chat/completions", run.model, streamAbortUpstreamStall) {
				if werr := run.writeFrame(fr); werr != nil {
					logger.Get().Warnw(run.logTag+": stall terminator write failed",
						"backend", run.backendID, "error", werr)
					return nil
				}
			}
			run.ensureDone(lastSSELine)
			// R184/R298: молчащий воркер продолжает генерировать и держит слот —
			// одного закрытия нашего чтения для этого мало, отмену надо ПРИСЛАТЬ.
			if run.cancel != nil {
				run.cancel("upstream_stall")
			}
			return nil

		case res := <-dataCh:
			// R297/R298: данные пришли — сторож зависания сбрасывается.
			stallWatch.Touch()
			if res.err != nil {
				if res.err == io.EOF {
					// Бэкенд закрыл стрим штатно (после [DONE] или финального чанка).
					done = true
					break
				}
				logger.Get().Warnw(run.logTag+": read from backend error",
					"backend", run.backendID, "error", res.err)
				// R252.1: error-кадр + РОВНО ОДИН `data: [DONE]` — иначе клиент
				// получает причину, но поток остаётся незавершённым.
				run.writeError("upstream read error")
				return nil
			}

			// R493 (этап 6): что видит клиент — решает ОДНО место
			// (planStreamLineR493), цикл только пишет решённое и закрывает поток,
			// если решено закрыть.
			plan := planStreamLineR493(res.line, egressFilter, run.schemas, toolRepair, run.transport)
			for _, fr := range plan.Pre {
				if werr := run.writeFrame(fr); werr != nil {
					logger.Get().Warnw(run.logTag+": finalize before [DONE] failed",
						"backend", run.backendID, "error", werr)
					return nil
				}
				lastSSELine = append(lastSSELine[:0], fr...)
			}
			if plan.Line == nil {
				// Строка не пишется: пустая строка-разделитель SSE (Round 35e) или
				// придержанный finish-кадр (R261).
				continue
			}
			if werr := run.writeFrame(plan.Line); werr != nil {
				logger.Get().Warnw(run.logTag+": write to client failed",
					"backend", run.backendID, "error", werr)
				// R344: клиент ушёл — это ГЛАВНЫЙ путь обнаружения на /v1 (ошибка
				// записи кадра, а не чтение сокета).
				if run.cancel != nil {
					run.cancel("client_cancel")
				}
				return nil
			}
			lastSSELine = append(lastSSELine[:0], plan.Line...)
			if plan.Terminator && run.noteTerminator != nil {
				run.noteTerminator()
			}

			// R294: кадр-ошибка воркера закрывает стрим. Сначала хвост фильтра
			// (объяснение/придержанный finish), затем терминатор, затем закрытие.
			if plan.Terminate {
				for _, fr := range plan.Post {
					_ = run.writeFrame(fr)
				}
				run.ensureDone(lastSSELine)
				logger.Get().Infow(run.logTag+": upstream error frame — stream closed (R294)",
					"backend", run.backendID)
				return nil
			}
		}
	}

	// R261: закрываем выход фильтра размышлений — сброс удержанного хвоста,
	// объяснение «пустого ответа» и придержанный finish.
	for _, fr := range finalizeStreamFramesR493(egressFilter) {
		if werr := run.writeFrame(fr); werr != nil {
			logger.Get().Warnw(run.logTag+": finalize frame write failed",
				"backend", run.backendID, "error", werr)
			return nil
		}
		lastSSELine = append(lastSSELine[:0], fr...)
	}

	// R252: гарантия завершения потока. Если последний кадр уже терминатор —
	// второго не будет (ensureDone идемпотентен на обоих транспортах).
	if run.ensureDone(lastSSELine) {
		logger.Get().Debugw(run.logTag+": synthetic [DONE] written (upstream ended without it)",
			"backend", run.backendID)
	}
	return nil
}

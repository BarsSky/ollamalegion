package balancer

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"ollama-loadbalancer/c/bridge"
	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/pkg/logger"
)

// proxyRequestLlamaCpp — основной прокси с поддержкой streaming.
// Корректно работает с SSE от llama.cpp и транслирует в NDJSON Ollama.
//
// R630 (2026-11-19): файл разложен по слоям стрима — чтение апстрима
// (`llamacpp_stream_upstream.go`), перевод кадров и буфер tool-call
// (`llamacpp_stream_translate.go`), хвост стрима (`llamacpp_stream_finalize.go`),
// «вызов апстрима» (`llamacpp_upstream_call.go`). Здесь остались сам
// `proxyRequestLlamaCpp`, его стадии preflight/retry и построение запроса к воркеру.
//
// Ключевая особенность: кросс-чанковая детекция JSON tool calls из content.
// Некоторые модели (Gemma-4) выводят JSON tool calls как обычный текст в content,
// разбитый на множество SSE чанков. Этот прокси аккумулирует content из смежных
// чанков, детектирует полный JSON, и эмитит чистый NDJSON чанк с tool_calls.
func (p *Proxy) proxyRequestLlamaCpp(w http.ResponseWriter, r *http.Request, backendID string, bodyBuf []byte) error {
	// R664 (шаг A, исправление живого прогона r261120-r679): текущий ход виден
	// планировщику роста окна.
	//
	// Preflight ниже (внутри awaitAdmissionAndLoad) решает «расти в фоне» — и
	// очередь роста обязана знать, что ЭТОТ ход ещё идёт: стримом запрос станет
	// только после ответа апстрима (beginClientStreamR271 дальше по функции), а
	// перезагрузка под ходом — это 46.9 с до первого байта (живой факт стенда).
	// endTurn снимает учёт и разбирает очередь отложенного роста: окно
	// поднимается МЕЖДУ ходами, уже после последнего байта клиенту.
	endTurn := p.beginClientTurnR664(backendID)
	defer endTurn()

	SetStageR477(r.Context(), stageUpstreamR477)

	// R540 (R2 читаемости): подготовка (бэкенд и его адрес, имя модели, схемы
	// инструментов, нативный путь, тело) — стадия prepare. nil без ошибки
	// означает «ответ отказа по семейству модели (R268) уже отправлен клиенту».
	c, err := p.prepareUpstreamCall(w, r, backendID, bodyBuf)
	if err != nil {
		return err
	}
	if c == nil {
		return nil
	}

	// R758 (2026-11-25): «модель отвечает одно и то же несколько раз» начинается
	// здесь. Если клиент присылает ТО ЖЕ тело запроса той же сессии повторно, это
	// не цикл модели, а вторая генерация по тому же промпту — и ответ обязан
	// быть похожим. Считаем и называем это в логе с числами; поведение не меняем
	// (первопричину ожидания убирают таймауты/очередь/скорость, а не блокировка).
	p.noteRequestRepeatR758(RequestSessionKey(r, bodyBuf), c.modelFromCtx, backendID, r.URL.Path, bodyBuf, 0)

	// R540 (R2 читаемости): preflight окна n_ctx и учёт нагрузки — стадия
	// preflight. true — ответ клиенту (413/404/503) уже отправлен вердиктом.
	if c.awaitAdmissionAndLoad() {
		return nil
	}

	// R54.4 (2026-08-24): AutoTune autonomous reload hook.
	// R230: выполняется ПОСЛЕ ответа (defer), а не перед форвардом. Почему:
	// запрос-триггер сразу уходил в воркер, модель становилась busy, и unload
	// отвечал «busy with active inference requests» (либо ленивая загрузка
	// перехватывала reload с профильными params). После ответа модель
	// простаивает — план применяется по-настоящему (см. также
	// reloadDedupRegistry.BeginExternalReload и autoTuneAppliedMismatch).
	// Не блокирует запрос — reload в фоне. Circuit breaker защищает от storm.
	// R530 (R2 читаемости): тело отложенной проверки AutoTune вынесено в
	// autoTuneOnReturn; имя модели передаётся указателем — оно уточняется в
	// цикле (R422), а defer читает его В МОМЕНТ выполнения.
	defer autoTuneOnReturn(p, c.backendID, &c.modelFromCtx)()

	// R540 (R2 читаемости): URL, клиент с таймаутами, контекст и сам запрос.
	cancelReq, err := c.prepareUpstreamRequest()
	if err != nil {
		return err
	}
	defer cancelReq()

	// R540 (R2 читаемости): отправка и ретраи до записи заголовков клиенту.
	if err := c.sendRequest(); err != nil {
		return err
	}
	defer c.resp.Body.Close()
	// R476/R478: бэкенд в ответ ставим ТОЛЬКО после успешного ответа апстрима.
	// Раньше заголовок ставился до запроса, и в строке трассировки мог оказаться
	// бэкенд, который запрос не обслуживал (failover/retry) — диагностическая
	// ложь, на которую указал аудит.
	w.Header().Set("X-Backend-ID", backendID)
	// R477: апстрим ответил — дальше стадия стрима (видно в request trace).
	SetStageR477(r.Context(), stageStreamR477)

	// R540 (R2 читаемости): прозрачная ленивая загрузка (503 + polling).
	if handled, loadErr := c.awaitLoadedModel(); loadErr != nil {
		return loadErr
	} else if handled {
		return nil
	}

	// Если upstream вернул не-2xx — не начинаем streaming, а возвращаем
	// структурированную ошибку в формате, который клиент сможет прочитать.
	// R538 (R2 читаемости): весь ответ на не-2xx (n_ctx-перезагрузка, passthrough
	// 4xx, кадр-ошибка 5xx для SSE и NDJSON) — в writeUpstreamErrorResponse.
	if c.resp.StatusCode < 200 || c.resp.StatusCode >= 300 {
		p.writeUpstreamErrorResponse(c.w, c.r, c.resp, c.backendID, c.modelFromCtx, c.originalPath, c.bodyBuf)
		return nil
	}

	// R540 (R2 читаемости): заголовки ответа и WriteHeader(200) — стадия
	// client_write; id генерации и время загрузки приходят из заголовков.
	upstreamRequestIDR360, workerLoadNsR493 := c.writeStreamHeaders()

	// R539 (R2 читаемости): состояние стрима — одна структура, а не сорок
	// локальных переменных: цикл и финализация вынесены в методы
	// llamaCppStreamState (см. ниже). Указатель на имя модели — потому что оно
	// уточняется в цикле (R422), а `defer` AutoTune читает его уже после возврата.
	st := &llamaCppStreamState{
		p:                     p,
		w:                     w,
		r:                     r,
		backendID:             backendID,
		originalPath:          c.originalPath,
		isStreaming:           c.isStreaming,
		nativePath:            c.nativePath,
		bodyBuf:               c.bodyBuf,
		translatedBody:        c.translatedBody,
		fullURL:               c.fullURL,
		toolSchemas:           c.toolSchemas,
		resp:                  c.resp,
		reqCtx:                c.reqCtx,
		modelFromCtx:          &c.modelFromCtx,
		modelNameNow:          c.modelNameNow,
		modelCtxAtomic:        &c.modelCtxAtomic,
		llamaStartTime:        c.llamaStartTime,
		requestStartTime:      c.requestStartTime,
		upstreamSentTime:      c.upstreamSentTime,
		upstreamRequestIDR360: upstreamRequestIDR360,
		workerLoadNsR493:      workerLoadNsR493,
		toolAccum:             make(map[int]*accumulatedToolCall),
		toolAcc:               newToolCallAccumulator(),
		toolRepair:            &toolRepairState{},
		egressFilter:          &reasoningEgressState{modelLabel: c.modelFromCtx},
	}

	// ==== R251/R252 (2026-10-28): живой стрим должен быть ВИДЕН в сессиях и мониторе ====
	//
	// ЖИВОЙ ДЕФЕКТ (Cline, длинная серия tool-call'ов). Heartbeat сессии и флаг
	// «идёт стрим» исторически ставились ТОЛЬКО на нативном Ollama-пути
	// (handleStreamingResponse, streaming.go). Для llama.cpp (/v1/chat/completions,
	// /api/chat, /api/generate) транспорт писал сессию ОДИН раз на входе и больше
	// не трогал её: сессию удалял абсолютный TTL, а запись клиента в мониторе
	// вычищалась через 60 с простоя — хотя ответ всё это время шёл.
	//
	// R252: логика вынесена в общий startStreamSession (stream_session.go),
	// чтобы пути не расходились. Stop() снимает флаги на любом выходе и
	// идемпотентен.
	streamSess := startStreamSession(p, r, c.modelFromCtx)
	defer streamSess.Stop()

	// R271 (2026-10-29): учитываем стрим по бэкенду и модели. Именно этого учёта
	// не хватало, когда перезагрузка модели по запросу ВТОРОГО клиента выгружала
	// модель под активным стримом первого — тот получал EOF без терминального
	// маркера и «stream ended by upstream before completion» (active_streams.go).
	endClientStream := beginClientStreamR271(p, backendID, c.modelFromCtx)
	defer endClientStream()

	// ==== Основной цикл SSE → NDJSON / SSE → SSE ==========
	// R479: с этого места всё, что делает транспорт, — запись клиенту.
	SetStageR477(r.Context(), stageClientR477)
	// R479: если стрим оборвался не по EOF, это сбой доставки — фиксируем
	// причину, даже когда ответ уже начат (status останется 200).
	defer func() {
		if err := r.Context().Err(); err != nil {
			MarkAbortR477(r.Context(), "client_context: "+err.Error())
		}
	}()

	// R539: чтение тела ответа, единый сток кадров и оба сторожа —
	// отмена клиента/таймаут (R60.58) и тишина upstream (R298).
	st.startUpstreamReaders()
	defer close(st.watcherStop)
	defer st.stallWatch.Stop()

	if err := st.readUpstreamStream(); err != nil {
		return err
	}
	return st.finalizeStream()
}

// errString — безопасно возвращает string(err) даже если err == nil.
func (p *Proxy) applyPreflightOutcome(
	w http.ResponseWriter,
	preflightCtx context.Context,
	backendID, modelFromCtx, originalPath string,
	nativePath bool,
	preflightBody []byte,
	preflightOK bool,
	preflightMsg string,
	preflightStatus int,
	bodyBuf, translatedBody []byte,
) (newBodyBuf, newTranslatedBody []byte, handled bool) {
	newBodyBuf, newTranslatedBody = bodyBuf, translatedBody

	if preflightOK {
		newBodyBuf = preflightBody // preflight удалил num_ctx из body (cppworker возьмёт new n_ctx)
		// R301 (2026-10-29): подменённое preflight'ом тело обязано ДОЙТИ до воркера.
		//
		// ЖИВОЙ ДЕФЕКТ (стенд fcef935c): вердикт R301 отработал верно
		// (log: served_with_loaded_window=true, required=16384, loaded=8192),
		// но воркер всё равно получил num_ctx=16384 и ответил кодом 2:
		// «requested n_ctx=16384 exceeds model's effective n_ctx=8192».
		// Причина — ВТОРОЙ источник тела: нативный путь кладёт исходные байты в
		// translatedBody (см. :84) и именно их отправляет (:259). Патч bodyBuf
		// никуда не попадал, то есть решение принималось, логировалось — и
		// терялось. На нативном пути перевода нет, поэтому достаточно взять
		// ровно то тело, что вернул preflight.
		if nativePath {
			newTranslatedBody = newBodyBuf
		} else if patched := patchNumCtxInBody(newTranslatedBody, ExtractNumCtxFromBody(newBodyBuf)); patched != nil {
			// Переведённое (OpenAI) тело — другой формат: правим num_ctx тем же
			// значением, что preflight поставил в Ollama-теле (если оно там есть).
			newTranslatedBody = patched
		}
		logger.Get().Debugw("proxyRequestLlamaCpp: preflight n_ctx reload applied",
			"backend", backendID, "model", modelFromCtx, "new_body_len", len(newBodyBuf),
			"native_path", nativePath, "upstream_body_len", len(newTranslatedBody))
	} else if preflightMsg != "" || preflightStatus != http.StatusOK {
		// R118.7 (2026-10-14): окно физически недостижимо (cppworker отвечал 422
		// n_ctx_infeasible) — клиенту понятный 413, а не «async-reload» 503,
		// который он будет повторять бесконечно.
		if preflightStatus == http.StatusRequestEntityTooLarge {
			logger.Get().Infow(i18n.T("balancer.llamacpp_transport.window_infeasible_413_info"),
				"backend", backendID, "model", modelFromCtx, "msg", preflightMsg)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-NCtx-Reload-Decision", "infeasible")
			// R664 (2026-11-20) ШАГ B: у отказа может быть ГОТОВОЕ тело (машинный
			// код + числа, см. makePromptExceedsWindowRejectR664). Тогда оно
			// пишется как есть: обёртка `{"error":<текст>,"decision":...}` съела бы
			// и код, и поля-числа, ради которых отказ и сделан.
			body := []byte(fmt.Sprintf(`{"error":%q,"decision":"n_ctx_infeasible"}`, preflightMsg))
			if trimmed := strings.TrimSpace(preflightMsg); strings.HasPrefix(trimmed, "{") &&
				json.Valid([]byte(trimmed)) {
				body = []byte(trimmed)
			}
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			_, _ = w.Write(body)
			return newBodyBuf, newTranslatedBody, true
		}
		// R119 (2026-10-15): модели НЕТ на бэкенде (cppworker ответил 404 на
		// попытку загрузки) — клиенту терминальный 404 с причиной и списком
		// доступных моделей. 503 здесь означал бы «повтори», и клиент уходил в
		// retry-цикл до своего таймаута («Internal Server Error»).
		//
		// awaitModelMissing закрывает гонку: загрузка отсутствующей модели падает
		// за миллисекунды, а запись в реестр появляется чуть позже решения
		// preflight'а — короткое ожидание делает САМЫЙ ПЕРВЫЙ запрос терминальным.
		if p.awaitModelMissing(backendID, modelFromCtx, 500*time.Millisecond) {
			reason, available, _ := p.modelMissingFor(backendID, modelFromCtx)
			logger.Get().Warnw(i18n.T("balancer.llamacpp_transport.model_missing_404_info"),
				"backend", backendID, "model", modelFromCtx,
				"msg", preflightMsg, "available_models", len(available))
			writeModelMissingResponseFor(w, originalPath, modelFromCtx, reason, available)
			return newBodyBuf, newTranslatedBody, true
		}
		// preflight вернул needsProxy=false: асинхронный reload запущен в фоне.
		// Отдаём клиенту HTTP 503 Service Unavailable + Retry-After: 30.
		// Клиент повторит запрос через 30 секунд — реалистичное время reload'а.
		// Round 23 (2026-08-04): Retry-After 5 → 30 (reload 30-60с на GPU; 5с
		// заставляло Cline/OpenWebUI повторять 10-12 раз за reload, накапливая
		// 503 и путая retry-логику).
		logger.Get().Infow("proxyRequestLlamaCpp: preflight triggered async n_ctx reload, returning 503 to client",
			"backend", backendID, "model", modelFromCtx,
			"msg", preflightMsg, "status", preflightStatus)
		// R299 (2026-10-29): тело отказа — ОДИН формат (deferredReason +
		// requiredTokens/loadedWindowTokens/activeStreams/reloadPending) вместо
		// самописного `{"decision":"async_reload","retry_after_seconds":30}`.
		// Живой факт: клиент получал тело, которого нет ни у одного другого
		// отказа, и в тексте «active streams=0» без указания, что перезагрузка
		// идёт. X-NCtx-Reload-Decision оставлен для совместимости.
		//
		// R301.2: заголовок обязан говорить правду. Этот же выход используется
		// стриминговым вердиктом R301 (окно не растёт — перезагрузка НЕ
		// запускалась); живая проверка показала «async-reload» рядом с
		// reloadPending=false. Различаем по факту: идёт ли перезагрузка.
		decision := "async-reload"
		if p.nctxReload == nil || !p.nctxReload.IsReloadPending(backendID, modelFromCtx) {
			decision = "context-growth"
		}
		w.Header().Set("X-NCtx-Reload-Decision", decision)
		writePreflightDeferredR299Ctx(w, p, preflightCtx, backendID, modelFromCtx, newBodyBuf, originalPath, preflightMsg, 30)
		return newBodyBuf, newTranslatedBody, true
	}
	return newBodyBuf, newTranslatedBody, false
}

// retryAfterNCtxError — распознать n_ctx-ошибку cppworker (code=2/3) и запустить
// адресную перезагрузку модели с большим окном (R531, R2 читаемости: вынесено из
// proxyRequestLlamaCpp дословно). true — ответ клиенту уже отправлен, вызывающая
// функция обязана выйти с nil.
func (p *Proxy) retryAfterNCtxError(w http.ResponseWriter, r *http.Request, backendID, modelFromCtx string, status int, respBody, bodyBuf []byte) bool {
	if status < 400 || p.nctxReload == nil {
		return false
	}
	nctxErr := ParseCppWorkerError(respBody, status, backendID)
	if nctxErr == nil {
		return false
	}
	if !errors.Is(nctxErr, bridge.ErrNCtxNeedsReload) && !errors.Is(nctxErr, bridge.ErrPromptTooLong) {
		return false
	}
	logger.Get().Infow("proxyRequestLlamaCpp: detected n_ctx error from cppworker, invoking auto-reload (streaming retry)",
		"backend", backendID, "model", modelFromCtx,
		"status", status,
		"is_n_ctx", errors.Is(nctxErr, bridge.ErrNCtxNeedsReload),
		"is_prompt_too_long", errors.Is(nctxErr, bridge.ErrPromptTooLong))
	p.handleNCtxReload(r.Context(), w, r, backendID, modelFromCtx, nctxErr, bodyBuf)
	return true
}

// refuseUnsupportedFamilyAndLog — отказ ДО запроса к модели, если её семейство
// не преобразуется в Ollama API (R268), и лог решения (R532, R2 читаемости:
// вынесено из proxyRequestLlamaCpp дословно). true — ответ клиенту отправлен.
func (p *Proxy) refuseUnsupportedFamilyAndLog(w http.ResponseWriter, originalPath, modelFromCtx string, hasTools, nativePath bool) bool {
	if p.refuseUnsupportedModelFamily(w, originalPath, modelFromCtx, hasTools,
		isOllamaConversionPath(originalPath) && !nativePath) {
		return true
	}
	p.logModelFamily(originalPath, modelFromCtx, hasTools)
	return false
}

// makeUpstreamPathAndBody — путь для cppworker, признак стрима и тело запроса
// (R533, R2 читаемости: вынесено из proxyRequestLlamaCpp дословно).
//
// Нативный Ollama-путь отдаётся как есть (R65d: транслятор терял options.*/
// keep_alive/format/think/images), не-нативный переводится в OpenAI-формат;
// при ошибке перевода отправляется исходное тело, как и раньше.
func makeUpstreamPathAndBody(originalPath string, nativePath bool, bodyBuf []byte) (llamacppPath string, isStreaming bool, translatedBody []byte) {
	llamacppPath = originalPath
	if !nativePath {
		llamacppPath = translatePathForLlamaCpp(originalPath)
	}
	isStreaming = isStreamingFromBody(originalPath, bodyBuf)

	translatedBody = bodyBuf
	if !nativePath {
		if tb, err := translateOllamaBodyToOpenAI(originalPath, bodyBuf); err == nil {
			translatedBody = tb
		}
	}
	return llamacppPath, isStreaming, translatedBody
}

// buildUpstreamURLAndBody — полный URL запроса к воркеру и (для
// /api/models/unload) перенос name из тела в query (R60.9: cppworker требует
// ?name= именно в query string). R534, R2 читаемости: вынесено дословно.
func buildUpstreamURLAndBody(targetURL, llamacppPath, originalPath, backendID string, translatedBody []byte) (fullURL string, body []byte) {
	fullURL = targetURL + llamacppPath
	body = translatedBody
	if originalPath == "/api/models/unload" {
		name, modBody := extractNameFromBody(body)
		if name != "" {
			body = modBody
			fullURL = fullURL + "?name=" + url.QueryEscape(name)
			logger.Get().Debugw("proxyRequestLlamaCpp: moved name to query (R60.9 unload fix)",
				"backend", backendID, "model", name)
		}
	}
	return fullURL, body
}

// upstreamIdentityFromHeaders — id генерации воркера (R360: по нему адресная
// отмена, иначе гасятся все генерации модели на узле) и время загрузки модели
// из заголовка (R493-12a: в теле ответа OpenAI-поверхности этих секунд нет).
// R535, R2 читаемости: вынесено из proxyRequestLlamaCpp дословно.
func upstreamIdentityFromHeaders(resp *http.Response) (requestID string, loadNs int64) {
	requestID = resp.Header.Get("X-Request-Id")
	if v := strings.TrimSpace(resp.Header.Get("X-Cppworker-Load-Duration-Ms")); v != "" {
		if ms, convErr := strconv.ParseInt(v, 10, 64); convErr == nil && ms > 0 {
			loadNs = ms * int64(time.Millisecond)
		}
	}
	return requestID, loadNs
}

// applyStreamResponseHeaders — заголовки стримингового ответа, которые обязаны
// быть выставлены строго ДО WriteHeader (после него они уже не уйдут).
//
// R65d (2026-09-20): проброс заголовков предупреждений. cppworker урезает
// n_predict, чтобы ответ поместился в n_ctx, и сообщает об этом заголовками
// (nctx_clamp.go:360-388); без проброса клиент получает короткий ответ без
// объяснения причины.
//
// R536 (R2 читаемости): вынесено из proxyRequestLlamaCpp дословно — состав и
// порядок заголовков прежние.
func applyStreamResponseHeaders(w http.ResponseWriter, resp *http.Response) {
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Cache-Control", "no-cache")
	forwardUpstreamWarningHeaders(w, resp)
}

// upstreamHTTPClientFor — HTTP-клиент ЭТОГО запроса вместе с его таймаутами.
//
// PF-5 FIX (2026-06-27): применяем FirstByteTimeout на уровне HTTP-Transport
// через per-request клонирование streamingTransport. По умолчанию
// ResponseHeaderTimeout=0 (off), что позволяет зависшему upstream держать
// соединение бесконечно. С FirstByteTimeout>0 upstream, не отправивший
// HTTP-заголовки за указанное время, получает от Go HTTP-клиента ошибку
// "net/http: timeout awaiting response headers" — balancer транслирует её
// в понятный клиенту ответ.
//
// Применяется ТОЛЬКО для streaming + когда задан явный FirstByteTimeout.
// Для non-streaming RequestTimeout уже покрывается контекстом запроса.
//
// R537 (R2 читаемости): вынесено из proxyRequestLlamaCpp дословно; выбор
// клиента и граница на первый байт — те же.
func (p *Proxy) upstreamHTTPClientFor(isStreaming bool, modelFromCtx string) *http.Client {
	client := p.client
	if isStreaming {
		client = p.streamingClient
	}
	if !isStreaming {
		return client
	}
	firstByte := p.getModelFirstByteTimeout(modelFromCtx)
	// R189 (2026-10-20): ГРАНИЦА НА ПЕРВЫЙ БАЙТ ПО УМОЛЧАНИЮ.
	//
	// ЖИВОЙ РАЗБОР. /api/chat с tools[] не отдавал клиенту ни байта 300 с
	// (`upstream Do() failed, duration_ms=300021`), потому что первый байт
	// ограничивался только если профиль/окружение задали FirstByteTimeout
	// явно. Клиент всё это время ждал молча. Теперь для стрима действует
	// значение по умолчанию (180 с, LB_FIRST_BYTE_TIMEOUT_DEFAULT_SEC),
	// чтобы «нет данных» превращалось в понятную ошибку, а не в тишину до
	// таймаута клиента. 0 отключает границу.
	if firstByte <= 0 {
		// R498 / аудит T-1: дефолт считается ПО РАЗМЕРУ МОДЕЛИ (эстиматор), а не
		// плоскими 180 с: холодная загрузка 16–27B идёт 140–485 с, и граница
		// обрывала её. Явная настройка (в т.ч. 0 = выключено) сильнее оценки.
		firstByte = p.defaultFirstByteTimeoutForModelR498(modelFromCtx)
	}
	if firstByte > 0 {
		client = p.newStreamingClientWithResponseHeaderTimeout(firstByte)
	}
	return client
}

// streamRequestContext — контекст запроса к воркеру: клиентский cancel плюс
// per-model таймаут стрима. Возвращённый cancel идемпотентен и безопасен для
// `defer` на обоих путях (когда таймаута нет — это no-op).
//
// R537 (R2 читаемости): вынесено из proxyRequestLlamaCpp дословно.
func (p *Proxy) streamRequestContext(r *http.Request, isStreaming bool, backendID, modelFromCtx string) (context.Context, context.CancelFunc) {
	reqCtx := r.Context()
	if !isStreaming {
		return reqCtx, func() {}
	}
	// Per-model адаптивный таймаут для streaming inference.
	streamTimeout := p.getModelStreamTimeout(modelFromCtx)
	if streamTimeout > 0 {
		ctx, cancel := context.WithTimeout(r.Context(), streamTimeout)
		logger.Get().Debugw("proxyRequestLlamaCpp: using per-model stream timeout",
			"backend", backendID, "model", modelFromCtx,
			"stream_timeout_sec", streamTimeout.Seconds())
		return ctx, cancel
	}
	// R60.55 follow-up: streamTimeout=0 means "no timeout" (LB_STREAMING_NEVER_TIMEOUT=1).
	// R60.55-pre bug: context.WithTimeout(parent, 0) creates an immediately-expired
	// context (deadline = time.Now()), causing every request to fail with
	// "context deadline exceeded" before reaching upstream. Fix: skip WithTimeout
	// entirely when streamTimeout<=0 — reqCtx stays as r.Context() (inherits client
	// cancel only, no artificial deadline).
	logger.Get().Debugw("proxyRequestLlamaCpp: stream timeout disabled (0)",
		"backend", backendID, "model", modelFromCtx)
	return reqCtx, func() {}
}

// newUpstreamRequest — запрос к воркеру: тело, заголовки клиента (кроме
// Content-Type/Accept/Content-Length/Host), id запроса R376, токен
// административных ручек R307 и адрес клиента (X-Forwarded-For/X-Real-IP).
// clientRealIP возвращается, потому что повтор после ленивой загрузки (ниже в
// proxyRequestLlamaCpp) собирает запрос заново и ставит тот же адрес.
//
// R537 (R2 читаемости): вынесено из proxyRequestLlamaCpp дословно.
func (p *Proxy) newUpstreamRequest(reqCtx context.Context, r *http.Request, fullURL string,
	translatedBody []byte, llamacppPath, backendID string) (*http.Request, string, error) {
	req, err := http.NewRequestWithContext(reqCtx, r.Method, fullURL, bytes.NewReader(translatedBody))
	if err != nil {
		return nil, "", fmt.Errorf("failed to create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for key, values := range r.Header {
		lowerKey := strings.ToLower(key)
		if lowerKey == "content-type" || lowerKey == "accept" || lowerKey == "content-length" || lowerKey == "host" {
			continue
		}
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	// R376: id клиента — в воркер (отмена по request_id, см. upstream_request_id.go).
	stampUpstreamRequestIDFromRequestR376(req, r)
	// R307: административные ручки воркера закрыты токеном. Клиентский токен
	// (для балансера) воркеру не годится — подставляем токен ЭТОГО воркера.
	applyWorkerAdminTokenR307(req.Header, llamacppPath, p.workerAdminToken(backendID))

	clientRealIP := p.getClientRealIP(r)
	if existingXFF := r.Header.Get("X-Forwarded-For"); existingXFF != "" {
		req.Header.Set("X-Forwarded-For", existingXFF)
	} else {
		req.Header.Set("X-Forwarded-For", clientRealIP)
	}
	req.Header.Set("X-Real-IP", clientRealIP)
	return req, clientRealIP, nil
}

// writeUpstreamErrorResponse — ответ клиенту, когда апстрим вернул не-2xx ДО
// начала стрима: сначала шанс адресной перезагрузки по n_ctx-ошибке, затем
// passthrough 4xx как есть и, наконец, кадр-ошибка в формате поверхности
// (SSE для /v1/chat/completions, NDJSON для Ollama-путей).
//
// R538 (R2 читаемости): вынесено из proxyRequestLlamaCpp дословно — порядок
// проверок, тексты и заголовки прежние.
func (p *Proxy) writeUpstreamErrorResponse(w http.ResponseWriter, r *http.Request, resp *http.Response,
	backendID, modelFromCtx, originalPath string, bodyBuf []byte) {
	respBody, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		respBody = []byte(fmt.Sprintf(`{"error":"upstream read error: %v"}`, readErr))
	}
	logger.Get().Warnw("proxyRequestLlamaCpp: upstream returned non-2xx",
		"backend", backendID, "status", resp.StatusCode,
		"body", string(respBody)[:min(500, len(respBody))])

	// 2026-06-24: n_ctx auto-reload — если upstream вернул HTTP 413 / 400
	// с structured NCtxError (code=2 или code=3 + bridge_info), пытаемся
	// auto-reload модель на больший n_ctx и retry запрос. БЕЗ этого стрим
	// от OpenWebUI/Cline обрывается с error в SSE-чанке — клиент видит
	// "Server Connection Error" вместо нормального ответа после reload.
	//
	// Применяем тот же паттерн, что и в proxyRequestLlamaCppNonStream.
	// bodyBuf уже буферизован (передаётся []byte в сигнатуре), используем
	// его для retry после reload.
	// R531 (R2 читаемости): разбор n_ctx-ошибки и адресная перезагрузка
	// вынесены в retryAfterNCtxError; true — ответ уже отправлен.
	if p.retryAfterNCtxError(w, r, backendID, modelFromCtx, resp.StatusCode, respBody, bodyBuf) {
		return
	}

	// R60.10 (2026-09-07): upstream 4xx — pass through body AS-IS, no SSE wrap.
	// Client hasn't started streaming yet, so returning 4xx + JSON body is
	// semantically correct (HTTP-level error, not stream-level). This fixes
	// OpenAI clients getting 502 SSE errors for things like 404 model not found.
	if resp.StatusCode < 500 {
		ct := resp.Header.Get("Content-Type")
		if ct == "" {
			ct = "application/json"
		}
		w.Header().Set("Content-Type", ct)
		w.Header().Set("Content-Length", strconv.Itoa(len(respBody)))
		if upstreamReqID := resp.Header.Get("X-Request-Id"); upstreamReqID != "" {
			w.Header().Set("X-Upstream-Request-Id", upstreamReqID)
		}
		w.WriteHeader(resp.StatusCode)
		w.Write(respBody)
		logger.Get().Debugw("proxyRequestLlamaCpp: passed through upstream 4xx (R60.10)",
			"backend", backendID, "status", resp.StatusCode,
			"original_path", originalPath, "body_len", len(respBody))
		return
	}

	if originalPath == "/v1/chat/completions" {
		w.Header().Set("X-Accel-Buffering", "no")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(resp.StatusCode)
		fmt.Fprintf(w, "data: {\"error\":\"upstream returned HTTP %d\",\"choices\":[{\"delta\":{},\"finish_reason\":\"error\"}]}\n\n", resp.StatusCode)
		fmt.Fprintf(w, "data: [DONE]\n\n")
		// Flush на error path обязателен — иначе клиент получит FIN до [DONE]
		// и aiohttp / httpx вернут TransferEncodingError.
		Flush(w)
		return
	}

	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Connection", "keep-alive")
	// Round 18 P0.1: capabilities headers (X-Model-*)
	p.addModelCapabilitiesHeaders(w, modelFromCtx)
	// R65d: переносим предупреждения cppworker (context warning / adjusted
	// n_predict / Retry-After). Без этого урезание n_predict было невидимым.
	forwardUpstreamWarningHeaders(w, resp)
	w.WriteHeader(resp.StatusCode)
	errNDJSON := buildDoneResponse(originalPath, modelFromCtx, 0)
	fmt.Fprintf(w, "%s\n", string(errNDJSON))
	Flush(w)
}

// ============================================================================
// R539 (R2 читаемости): состояние прохода, цикл чтения и финализация стрима.
//
// ДО этой правки proxyRequestLlamaCpp держал в одном теле подготовку, цикл
// чтения NDJSON/SSE (966 строк) и финализацию (465). Теперь проход разложен по
// стадиям, названным в плане читаемости (docs/plans/2026-11-19-readability-
// refactor.md, §R2):
//
//	readUpstreamStream  — цикл: нативный NDJSON и SSE→NDJSON, разбор кадра;
//	finalizeStream      — хвост: придержанный текст, авто-продолжение (R60.21),
//	                      обязательный терминатор (R231), диагностика обрыва
//	                      (R296/R298/R384), сброс счётчика n_ctx.
//
// Состояние — llamaCppStreamState. Это НЕ копия: горутины отмены, сторож тишины
// и defer-ы читают те же значения, потому что владелец состояния один —
// структура создаётся в proxyRequestLlamaCpp и передаётся по указателю.
// Поведение перенесено дословно: порядок проверок, кадров, текстов логов и
// заголовков прежний.
// ============================================================================

// llamaCppStreamState — состояние одного стрима llama.cpp.
//
// Поля-указатели только там, где значение обязано быть общим с вызывающей
// функцией: имя модели (R422) уточняется в цикле, а `defer` AutoTune читает
// его уже после возврата, поэтому оно живёт в переменной вызывающей функции.
type llamaCppStreamState struct {
	// Порядок полей — по размеру (fieldalignment): крупные и содержащие
	// указатели первыми, чтобы сборщик мусора не сканировал хвост структуры.

	// Окна статистики (R493-12a) и удержание разрезанных меток (R266.1).
	statsWindowsNative statsWindowsR493
	ndjsonHold         markerHold

	// Моменты времени: старт прохода, приём запроса, отправка воркеру, первый
	// дельта-чанк (любого вида), первый content-чанк и начало накопления буфера.
	llamaStartTime     time.Time
	requestStartTime   time.Time
	upstreamSentTime   time.Time
	firstDeltaTime     time.Time
	firstContentTime   time.Time
	contentBufferSince time.Time

	// Тела запроса: клиентское и переведённое для воркера (нужны
	// авто-продолжению R60.21 и подсчёту инструментов/лимита клиента).
	bodyBuf        []byte
	translatedBody []byte

	// Строковое состояние прохода.
	backendID               string
	originalPath            string
	fullURL                 string
	upstreamRequestIDR360   string
	lastData                string
	accumulatedPlainContent string
	upstreamDoneContent     string
	upstreamErrorText       string
	contentBuffer           string

	// Поверхности ввода-вывода и ошибки записи клиенту (R60.58).
	w               http.ResponseWriter
	reqCtx          context.Context
	sink            streamSinkR493
	firstForwardErr error
	lastForwardErr  error

	// Запрос, ответ, аккумуляторы и сторож тишины этого стрима.
	p              *Proxy
	r              *http.Request
	resp           *http.Response
	scanner        *bufio.Scanner
	stallWatch     *streamStallWatch
	toolAccum      map[int]*accumulatedToolCall
	toolSchemas    map[string]*toolParamSchema
	toolAcc        *toolCallAccumulator
	toolRepair     *toolRepairState
	egressFilter   *reasoningEgressState
	watcherStop    chan struct{}
	modelFromCtx   *string
	modelNameNow   func() string
	modelCtxAtomic *atomic.Value
	bytesForwarded atomic.Int64

	// Счётчики (не содержат указателей).
	workerLoadNsR493     int64
	nativeReasoningChars int

	// Флаги прохода.
	isStreaming                bool
	nativePath                 bool
	streamCompleted            bool
	upstreamHadFinishReason    bool
	doneEmitted                bool
	deferredDoneChunk          bool
	terminalAlreadySent        bool
	nativeModeDecided          bool
	usageChunkSeen             bool
	priorDoneEmitted           bool
	userCancelledDoneChunkR354 bool
	statsEmitted               bool
	tokenUsageRecorded         bool
	seenReasoning              bool
	contentToolCallsProcessed  bool
	contentBufferGaveUp        bool
}

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
	"os"
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
// Ключевая особенность: кросс-чанковая детекция JSON tool calls из content.
// Некоторые модели (Gemma-4) выводят JSON tool calls как обычный текст в content,
// разбитый на множество SSE чанков. Этот прокси аккумулирует content из смежных
// чанков, детектирует полный JSON, и эмитит чистый NDJSON чанк с tool_calls.
func (p *Proxy) proxyRequestLlamaCpp(w http.ResponseWriter, r *http.Request, backendID string, bodyBuf []byte) error {
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
		egressFilter:          &reasoningEgressState{},
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
	// R252: логика вынесена в общий startStreamSession (stream_session_r252.go),
	// чтобы пути не расходились. Stop() снимает флаги на любом выходе и
	// идемпотентен.
	streamSess := startStreamSession(p, r, c.modelFromCtx)
	defer streamSess.Stop()

	// R271 (2026-10-29): учитываем стрим по бэкенду и модели. Именно этого учёта
	// не хватало, когда перезагрузка модели по запросу ВТОРОГО клиента выгружала
	// модель под активным стримом первого — тот получал EOF без терминального
	// маркера и «stream ended by upstream before completion» (active_streams_r271.go).
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
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// emitTruncatedChunk — R65c (2026-09-16) helper. Эмитит финальный done-чанк
// когда stream был прерван по timeout или cancellation ДО [DONE] от upstream.
//
// Используется из двух мест:
//  1. case <-reqCtx.Done() в main loop (streamTimeout) — пишет immediately,
//     return nil чтобы handleChat не слал ещё одну error chunk поверх.
//  2. case <-r.Context().Done() в main loop (client cancelled) — аналогично.
//
// Формат:
//   - /v1/chat/completions: SSE event with finish_reason="truncated" + data: [DONE]
//   - /api/chat | /api/generate: NDJSON {"done":true, "done_reason":"truncated", ...}
//
// R297 (2026-10-29): здесь "truncated" ОСТАВЛЕН НАМЕРЕННО и это единственное
// место, где он ещё выдаётся. Причина обрыва со стороны воркера
// (upstream_eof/upstream_error etc.) идёт через abortFramesR296 и помечается
// "error" — там клиент жив и обязан прочитать причину. Здесь же два случая:
// client_cancel (клиента уже нет, кадр никому не нужен — он для лога/теста) и
// собственный таймаут стрима, то есть решение НАШЕ, а не воркера. Путать эти
// две ветки нельзя: смена "truncated" на "error" здесь означала бы, что мы
// приписываем воркеру наш собственный таймаут.
//
// После этой записи Go HTTP server должен автоматически отправить
// chunked terminator (`0\r\n\r\n`) при return handler.
func emitTruncatedChunk(w http.ResponseWriter, originalPath, modelName string, duration time.Duration, reason string) {
	// R66b (2026-09-22): диагностика. До этого функция молча писала чанк, а
	// вызывающий код возвращал nil (чтобы handleChat не дописал ВТОРОЙ error-чанк
	// поверх уже отправленного) — в итоге таймаут/обрыв стрима не попадал
	// ни в логи, ни в метрики, и разбираться приходилось по жалобам клиентов.
	var reasonText string
	switch reason {
	case "timeout":
		reasonText = "stream timeout (reqCtx done: per-model streaming timeout / reload deadline)"
	case "client_cancel":
		reasonText = "client cancelled the request"
	default:
		reasonText = "truncated"
	}
	logger.Get().Warnw("proxyRequestLlamaCpp: stream truncated, emitting terminator chunk",
		"backend_model", modelName, "api_path", originalPath,
		"reason", reasonText, "duration_ms", duration.Milliseconds())

	truncatedMsg := fmt.Sprintf("stream truncated after %v (%s)", duration, reasonText)
	if originalPath == "/v1/chat/completions" {
		// OpenAI SSE format
		truncatedChunk := map[string]interface{}{
			"id":      fmt.Sprintf("trunc-%d", time.Now().UnixNano()),
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   modelName,
			"choices": []interface{}{
				map[string]interface{}{
					"index":         0,
					"delta":         map[string]interface{}{},
					"finish_reason": "truncated",
				},
			},
			"error": map[string]interface{}{
				"message": truncatedMsg,
				"type":    "stream_truncated",
				"reason":  reason,
			},
			// reason на верхнем уровне — для симметрии с NDJSON-веткой
			// (/api/chat, /api/generate): клиенты и логи читают одно и то же поле.
			"reason": reason,
		}
		out, _ := json.Marshal(truncatedChunk)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", string(out))
		_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
	} else {
		// Ollama NDJSON format (/api/chat, /api/generate)
		truncatedChunk := map[string]interface{}{
			"model":       modelName,
			"created_at":  time.Now().UTC().Format(time.RFC3339),
			"done":        true,
			"done_reason": "truncated",
			"error":       truncatedMsg,
			"reason":      reason,
		}
		out, _ := json.Marshal(truncatedChunk)
		_, _ = fmt.Fprintf(w, "%s\n", string(out))
	}
	Flush(w)
}

// hasFinishReason — проверяет, содержит ли SSE data-чанк finish_reason (done:true).
// Используется для подавления дублирующего done-чанка от translate.
func hasFinishReason(data string) bool {
	if len(data) == 0 {
		return false
	}
	var chunk map[string]interface{}
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		return false
	}
	choices, ok := chunk["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		return false
	}
	choice, ok := choices[0].(map[string]interface{})
	if !ok {
		return false
	}
	fr, ok := choice["finish_reason"].(string)
	if !ok {
		return false
	}
	return fr != "" && fr != "null"
}

// extractUpstreamDoneChunk — извлекает content и done_reason из OpenAI SSE-чанка
// с finish_reason (например, финального чанка cppworker'а для /api/chat).
// Возвращает (true, msgMap, doneReason), если чанк содержит finish_reason.
func extractUpstreamDoneChunk(data string) (bool, map[string]interface{}, string) {
	var chunk map[string]interface{}
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		return false, nil, ""
	}
	choices, ok := chunk["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		return false, nil, ""
	}
	choice, ok := choices[0].(map[string]interface{})
	if !ok {
		return false, nil, ""
	}
	fr, ok := choice["finish_reason"].(string)
	if !ok || fr == "" || fr == "null" {
		return false, nil, ""
	}
	msg, _ := choice["message"].(map[string]interface{})
	if msg == nil {
		// Иногда content лежит в delta
		if delta, ok := choice["delta"].(map[string]interface{}); ok {
			msg = map[string]interface{}{}
			if c, ok := delta["content"].(string); ok {
				msg["content"] = c
			}
		}
	}
	return true, msg, fr
}

// getEffectiveStreamTimeoutSec — R53.6 (2026-08-24) helper для логирования.
// Возвращает фактический stream timeout в секундах, который будет применён
// к upstream запросу. Используется только в logging при streamTimeout firing
// (см. SSE read loop select case). Возвращает 0 если streaming disabled.
func getEffectiveStreamTimeoutSec(p *Proxy, modelName string, isStreaming bool) int {
	if !isStreaming {
		return 0
	}
	return int(p.getModelStreamTimeout(modelName).Seconds())
}

// stripNativeDoneContent — убрать текст из финального NDJSON-чанка (native-путь),
// оставив всю статистику.
//
// R83 §9.7 (2026-09-27). ЗАЧЕМ: cppworker в своём /api/chat кладёт в done-чанк
// ПОЛНЫЙ текст ответа, хотя стриминговые чанки его уже отдали. Настоящая Ollama
// в done-чанке отдаёт `message.content: ""` (подтверждено живым сырым логом
// OpenWebUI: `{"done":true,...,"message":{"content":"",...}}`), и клиенты,
// конкатенирующие чанки, получают ответ дважды — измерено на живом балансере:
// 63 символа в дельтах и те же 63 в done-чанке.
//
// R91 (2026-10-09). УСЛОВИЕ: `streamedContent` — текст, уже отданный дельтами
// (может быть пустым). Если дельт не было, текст done-чанка — ЕДИНСТВЕННЫЙ носитель
// ответа, и вычищать его нельзя: клиент получил бы `done:true` с пустым content
// (это ловили integration-тесты tests/llamacpp_proxy и tests). Так выглядит
// законный случай: апстрим отдал весь ответ одним финальным чанком (статический
// ответ, mock-воркер, либо наш же фолбэк cppworker, когда модель ушла в
// незакрытый reasoning-блок и видимый текст удалось отдать только в финале).
//
// ЧТО НЕ МЕНЯЕТСЯ: `done`, `done_reason`, `eval_count`, `prompt_eval_count`,
// `total_duration`, `load_duration`, `eval_duration`, `prompt_eval_duration`,
// `model`, `created_at` — всё остаётся как пришло. Не-done чанки и любая строка,
// которую не удалось разобрать, возвращаются байт-в-байт: этот код не имеет
// права испортить поток, если формат неожиданный.
// appendNativeContent — R118.17 (2026-10-15): накопить текст нативного
// NDJSON-чанка, НЕ удваивая его.
//
// ЗАЧЕМ. cppworker на нативном пути отдаёт ответ дважды: дельтами по чанкам и
// ещё раз полным текстом в финальном done-чанке (клиенту дубль не уходит — его
// снимает stripNativeDoneContent). Если приклеивать done-текст к накопленному,
// детектор обрыва R60.21 анализирует текст в двух копиях: число ``` становится
// чётным (обрыв «незакрытый блок» не виден), последняя строка выглядит полной —
// авто-продолжение не срабатывает на реально обрезанном ответе.
//
// Правила:
//   - обычный чанк (не done) — просто добавляем;
//   - done-чанк и накопленного текста ещё нет — берём его текст (апстрим отдал
//     весь ответ одним финальным чанком: см. R91, без этого ответ потерялся бы);
//   - done-чанк несёт уже отданный текст — не добавляем (это дубль);
//   - done-чанк несёт НОВЫЙ текст (апстрима сменил семантику) — добавляем, чтобы
//     не потерять его при анализе.
func appendNativeContent(acc string, chunkIsDone bool, text string) string {
	if text == "" {
		return acc
	}
	if !chunkIsDone {
		return acc + text
	}
	if strings.TrimSpace(acc) == "" {
		return text
	}
	if strings.Contains(acc, strings.TrimSpace(text)) {
		return acc
	}
	return acc + text
}

func stripNativeDoneContent(line, streamedContent string) string {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || !strings.HasPrefix(trimmed, "{") {
		return line
	}
	var chunk map[string]interface{}
	if err := json.Unmarshal([]byte(trimmed), &chunk); err != nil {
		return line
	}
	done, _ := chunk["done"].(bool)
	if !done {
		return line
	}
	msg, ok := chunk["message"].(map[string]interface{})
	if !ok {
		return line
	}
	content, _ := msg["content"].(string)
	if content == "" {
		return line
	}
	// Дельтами ничего не отдано — это единственный носитель ответа, оставляем.
	if strings.TrimSpace(streamedContent) == "" {
		// R405 (2026-11-03): диагностика для «пропала часть текста». Если ответ
		// пришёл ТОЛЬКО в done-кадре (удержание с начала: JSON/служебный токен/
		// блок размышлений), клиент, собирающий ответ из дельт, может его не
		// показать. Поведение не меняем (done-content — валидный носитель Ollama
		// API, и клиенты, читающие финал, его видят), но пишем факт в лог: без
		// него такой случай неотличим от потери на стороне клиента.
		modelName, _ := chunk["model"].(string)
		if logger.Get() != nil {
			logger.Get().Infow("proxyRequestLlamaCpp[native]: done-chunk is the only carrier of the answer (no deltas) — R405 diagnostic",
				"model", modelName, "done_len", len(content))
		}
		return line
	}
	// R279 (2026-10-28): СНИМАЕМ ТОЛЬКО ДУБЛЬ, А НЕ ВЕСЬ ТЕКСТ.
	//
	// ЖИВОЙ ДЕФЕКТ (Cline): модель сгенерировала финальный блок ответа, клиент
	// получил вместо него размышления и маркер конца, а блок исчез. Причина —
	// здесь: content done-чанка вычищался БЕЗУСЛОВНО, как только дельтами ушла
	// хоть одна строка. Если последний блок модели пришёл только в done-чанке
	// (cppworker так умеет: дельты — рассуждения/проза, а полный текст — в done),
	// вычищался не дубль, а единственная копия блока. Страховка ниже только
	// ПИСАЛА WARN о несовпадении и всё равно выбрасывала текст — то есть о
	// потере знали и теряли.
	remainder := dedupeDoneContent(content, streamedContent)
	switch {
	case remainder == "":
		// Полный дубль — прежнее поведение (клиент уже видел этот текст).
		msg["content"] = ""
	case remainder != content:
		// Часть текста уже ушла дельтами, остаток — новый. Отдаём остаток:
		// клиент не должен видеть дубль, но и терять финальный блок не должен.
		msg["content"] = remainder
		logger.Get().Infow("proxyRequestLlamaCpp[native]: done-chunk kept new content tail (R279)",
			"kept_len", len(remainder), "done_len", len(content), "streamed_len", len(streamedContent))
	default:
		// Общего с отданным нет — апстрим отдал в done-чанке ДРУГОЙ текст.
		// Отдаём его целиком: потеря реального текста хуже, чем возможный дубль,
		// а факт несовпадения виден в логе.
		logger.Get().Warnw(i18n.T("balancer.llamacpp_transport.strip_done_content_mismatch_warn"),
			"done_content_len", len(content), "streamed_len", len(streamedContent))
		msg["content"] = content
	}
	out, err := json.Marshal(chunk)
	if err != nil {
		return line
	}
	return string(out)
}

// nativeDoneContentStrippingEnabled — выключатель нормализации (по умолчанию
// ВКЛЮЧЕНА). Нужен на случай, если найдётся клиент, читающий ответ ТОЛЬКО из
// done-чанка: ему возвращают прежнее поведение переменной
// LB_OLLAMA_DONE_CONTENT=keep, не пересобирая образ.
func nativeDoneContentStrippingEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("LB_OLLAMA_DONE_CONTENT")))
	switch v {
	case "keep", "raw", "off", "0", "false", "passthrough":
		return false
	}
	return true
}

// autoTuneOnReturn — отложенная проверка AutoTune в конце хода (R529/R530).
//
// Возвращает замыкание для defer. Имя модели передаётся УКАЗАТЕЛЕМ: оно
// уточняется в цикле чтения (R422 — модель узнаётся из первого содержательного
// чанка), а эта проверка выполняется уже после возврата и обязана видеть
// уточнённое имя, иначе AutoTune получит пустое/старое.
func autoTuneOnReturn(p *Proxy, backendID string, modelName *string) func() {
	return func() {
		modelFromCtx := ""
		if modelName != nil {
			modelFromCtx = *modelName
		}
		if modelFromCtx == "" {
			return
		}
		var freeVRAM, freeRAM, totalVRAM uint64
		if p.metricsMgr != nil {
			// Берём hardware info из cluster state.
			// R230: GPUMetrics/System метрики в МБ, triggerAutoTuneReload ждёт
			// байты — конвертируем (иначе «свободно 0 GB»).
			clusterState := p.GetClusterState()
			for _, b := range clusterState.Backends {
				if b.ID == backendID {
					totalVRAM = mbToBytes(b.GPU.MemoryTotal)
					if b.GPU.MemoryFree > 0 {
						freeVRAM = mbToBytes(b.GPU.MemoryFree)
					}
					freeRAM = mbToBytes(b.System.MemoryFree)
					break
				}
			}
		}
		autoTuneRes := p.triggerAutoTuneReload(backendID, modelFromCtx, freeVRAM, freeRAM, totalVRAM)
		if autoTuneRes != nil && autoTuneRes.Triggered {
			logger.Get().Infow("proxyRequestLlamaCpp: AutoTune async reload triggered",
				"backend", backendID, "model", modelFromCtx,
				"reason", autoTuneRes.Plan.Reason)
		} else if autoTuneRes != nil && autoTuneRes.SkippedReason != "" {
			logger.Get().Debugw("proxyRequestLlamaCpp: AutoTune check",
				"backend", backendID, "model", modelFromCtx,
				"skipped_reason", autoTuneRes.SkippedReason)
		}
	}
}

// applyPreflightOutcome — применить вердикт preflight n_ctx и, если ход
// невозможен, ответить клиенту (R118.7 413, R119 404, R299/R301.2 503).
//
// Вынесено из proxyRequestLlamaCpp (R530, R2 читаемости) дословно: порядок
// проверок, тексты отказов и заголовки прежние. handled=true — ответ уже
// отправлен, вызывающая функция обязана выйти с nil.
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
			body := []byte(fmt.Sprintf(`{"error":%q,"decision":"n_ctx_infeasible"}`, preflightMsg))
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
	// R376: id клиента — в воркер (отмена по request_id, см. upstream_request_id_r376.go).
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

// startUpstreamReaders готовит чтение тела ответа и обе горутины, которые это
// чтение разблокируют: сторож отмены (R60.58) и сторож тишины (R298).
// Обязан вызываться один раз, до readUpstreamStream.
func (st *llamaCppStreamState) startUpstreamReaders() {
	// R60.58: те же буфер и лимит строки, что и раньше.
	st.scanner = bufio.NewScanner(st.resp.Body)
	st.scanner.Buffer(make([]byte, 0), streamMaxLineBytes())

	// R493-12r (этап 6 «два мозга», флаг LB_STREAM_PIPE): единый сток стрима.
	// nil при выключенном флаге — тогда цикл пишет напрямую, как раньше
	// (историческое поведение и путь отката).
	if streamPipeEnabledR493() {
		st.sink = newStreamSinkR493(st.w)
	}

	// R292: состояние «подозрительного на tool-вызов» текста.
	//   contentBufferSince — когда буфер начали копить (для grace-порога);
	//   contentBufferGaveUp — мы уже решили, что это проза: больше не копим.
	// Без второго флага следующая порция текста снова ушла бы в буфер, и клиент
	// опять увидел бы «замер» — то есть лечение было бы одноразовым.
	// (R539: обе величины и сам буфер — поля состояния, см. llamaCppStreamState.)

	// R493-12a: окна для правки НАТИВНОГО done-кадра. Запрос воркеру уже ушёл
	// (upstreamSentTime), значит «ожидание» для клиента — это отрезок до него:
	// очередь, авто-загрузка модели и preflight. Раньше он не попадал никуда.
	st.statsWindowsNative = statsWindowsR493{RequestStart: st.requestStartTime, UpstreamSent: st.upstreamSentTime}

	// R60.58 watcher goroutine: разблокирует блокирующий scanner.Scan() при cancel/timeout.
	//
	// БЕЗ ЭТОГО (R60.58 bug): bufio.Scanner.Scan() — блокирующий вызов. Если upstream
	// (cppworker) застрял на llama_decode/OOM/deadlock — balancer тоже застрянет на чтении.
	// Select с default внутри loop БЕСПОЛЕЗЕН потому что мы застряли внутри Scan(),
	// до select. Клиент получает бесконечный hang до тех пор пока upstream не вернётся
	// сам (что может быть никогда при deadlock) или connection не умрёт по network timeout.
	//
	// РЕШЕНИЕ: отдельная горутина следит за ctx.Done() и закрывает resp.Body при
	// cancel/timeout. Close прерывает блокирующий Read() в scanner → scanner.Scan()
	// возвращает false → loop выходит → emit error chunk клиенту.
	//
	// Применяется ко всем путям: /v1/chat/completions (Cline), /api/chat (OpenWebUI),
	// /api/generate (Ollama CLI).
	st.watcherStop = make(chan struct{})
	go func() {
		select {
		case <-st.r.Context().Done():
			logger.Get().Infow("proxyRequestLlamaCpp: client cancelled, aborting stream",
				"backend", st.backendID, "model", st.modelNameNow(),
				"context_error", st.r.Context().Err(),
				"bytes_forwarded", st.bytesForwarded.Load())
			// R184: закрытие СВОЕГО чтения не останавливает воркер - он продолжает
			// генерировать и держит единственный слот (живой замер: следующий клиент
			// ждал 100 429 мс). Отмену надо ПРИСЛАТЬ: POST /api/cancel.
			st.p.cancelUpstreamGeneration(st.backendID, st.modelNameNow(), st.upstreamRequestIDR360, "client_cancel")
		case <-st.reqCtx.Done():
			// R60.58: streamTimeout сработал ИЛИ cancel — в обоих случаях
			// закрываем upstream чтобы разблокировать scanner.Scan().
			// R184: таймаут тоже обязан освободить слот на воркере.
			st.p.cancelUpstreamGeneration(st.backendID, st.modelNameNow(), st.upstreamRequestIDR360, "stream_timeout")
			logger.Get().Warnw("proxyRequestLlamaCpp: streamTimeout fired, aborting stream",
				"backend", st.backendID, "model", st.modelNameNow(),
				"context_error", st.reqCtx.Err(),
				"stream_timeout_sec", getEffectiveStreamTimeoutSec(st.p, st.modelNameNow(), st.isStreaming),
				"bytes_forwarded", st.bytesForwarded.Load())
		case <-st.watcherStop:
			return
		}
		if st.resp.Body != nil {
			_ = st.resp.Body.Close()
		}
	}()

	// R298 (2026-10-29): СТОРОЖ ТИШИНЫ на блокирующем пути.
	//
	// ЖИВОЙ ДЕФЕКТ. Здесь сторож не существовал ВООБЩЕ: `scanner.Scan()` —
	// блокирующий вызов, а на стенде стоит `LB_STREAMING_NEVER_TIMEOUT=1`, то
	// есть ни deadline, ни idle-таймаута нет. Мёртвый-но-не-закрытый upstream
	// (docker pause, зависший llama_decode) оставлял и балансер, и клиента ждать
	// вечно; при этом hijack-путь (/v1) сторож имел — то есть два протокола вели
	// себя по-разному, и Cline через /api/chat не получал ни кадра-причины, ни
	// терминатора.
	//
	// Разблокировать `Scan()` можно только закрытием тела ответа, поэтому здесь
	// сторож работает НЕ через канал, а через onStall: он закрывает resp.Body,
	// чтение возвращается, и штатный post-loop ниже отдаёт кадры через
	// abortFramesR296 — ровно тот же контракт терминатора, что и на других путях.
	st.stallWatch = newStreamStallWatch(streamStallAbortTimeout(), streamFirstFrameAbortTimeout(), func(ev stallEvent) {
		// Сначала разблокируем ЧТЕНИЕ (Close), и только потом отменяем генерацию:
		// отмена — сетевой round-trip к воркеру, и если он завис (docker pause),
		// ставить её перед Close означало бы держать клиента ещё дольше.
		if st.resp.Body != nil {
			_ = st.resp.Body.Close()
		}
		logger.Get().Warnw("proxyRequestLlamaCpp: upstream stalled — body closed to unblock scan (R298)",
			"backend", st.backendID, "model", st.modelNameNow(),
			"prefill", ev.prefill,
			"stalled_sec", int(ev.stalled.Seconds()),
			"original_path", st.originalPath,
			"bytes_forwarded", st.bytesForwarded.Load())
		// R184: молчащий воркер всё равно держит слот — отменяем генерацию,
		// иначе следующий клиент получит «busy».
		st.p.cancelUpstreamGeneration(st.backendID, st.modelNameNow(), st.upstreamRequestIDR360, "upstream_stall")
	})
	st.stallWatch.Start()
}

// readUpstreamStream — цикл чтения ответа воркера: нативный NDJSON идёт клиенту
// как есть (R65d), OpenAI-SSE переводится в NDJSON (или проксируется как SSE).
// Ошибку возвращает только запись клиенту; обрыв потока обрабатывается в
// finalizeStream (кадры-причины R296).
func (st *llamaCppStreamState) readUpstreamStream() error {
	// Локальная ссылка на сканер: тело цикла остаётся буквально прежним
	// (`for scanner.Scan()`, `scanner.Text()`), и сторож R298 узнаёт
	// блокирующее чтение по имени сканера.
	scanner := st.scanner
	for scanner.Scan() {
		// R298: данные пришли — отсчёт тишины сбрасывается.
		st.stallWatch.Touch()
		// Round 23 (2026-08-04): проверяем отмену клиента ПЕРЕД чтением следующего
		// чанка: при отмене клиент уже не ждёт, и кадр-терминатор ему не нужен.
		//
		// R53.6 (2026-08-24): добавлен случай reqCtx.Done() — streamTimeout
		// (per-model profile, default 600s): без него балансер зависает на
		// генерации длинного контекста.
		//
		// R65c (2026-09-16): клиенту отдаётся truncated-кадр, чтобы парсер
		// клиента (aiohttp) увидел терминатор до tear-down соединения.
		if st.abortBeforeFrame() {
			return nil
		}

		line := scanner.Text()

		// R66c (2026-09-22): нативный passthrough валиден ТОЛЬКО если апстрим
		// реально отвечает Ollama-NDJSON. Если cppworker ответил OpenAI-SSE
		// (сборка без нативного пути, LB_OLLAMA_NATIVE_PATH=0 или путь был
		// переписан апстримом), то сырые OpenAI-чанки уходили Ollama-клиенту
		// КАК NDJSON: вместо message.content приходили choices/delta, а
		// финального done:true не было вообще — клиент зависает либо считает
		// ответ пустым ("Model returned empty response").
		//
		// Определяем по СТРОКЕ, а не по Content-Type: cppworker исторически
		// отдаёт NDJSON и с неверным/отсутствующим заголовком, поэтому
		// заголовку здесь доверять нельзя. Первая же строка с префиксом
		// "data:" означает SSE — переключаемся на трансляцию SSE→NDJSON ниже.
		st.decideNativeMode(line)

		// ==== R65d (2026-09-20): нативный путь — Ollama NDJSON passthrough ====
		//
		// cppworker's нативные обработчики (/api/chat, /api/generate) отдают
		// application/x-ndjson: одна JSON-строка на строку, БЕЗ префикса "data:".
		// Формат уже тот, который ждёт Ollama-клиент, поэтому перевод не нужен —
		// отдаём строку как есть (плюс '\n', он же был в исходной строке).
		//
		// Что здесь отслеживаем (для диагностики, не для преобразования):
		//   - streamCompleted: встретили done:true → стрим завершён штатно,
		//     downstream truncation-detection не должен срабатывать;
		//   - accumulatedPlainContent: накапливаем message.content / response
		//     для R60.21 auto-continue и для логов;
		//   - usageChunkSeen: нативный done-чанк уже содержит eval_count /
		//     total_duration, поэтому writeStreamingSSEDone не должен писать
		//     второй done-чанк.
		//
		// ВАЖНО: keepalive-строки {"keepalive":true} проходят насквозь — это
		// валидный NDJSON для Ollama-клиентов (см. handlers_chat.go:861).
		if st.nativePath {
			if err := st.handleNativeNDJSONLine(line); err != nil {
				return err
			}
			continue
		}

		if !strings.HasPrefix(line, "data:") {
			continue
		}

		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			st.streamCompleted = true
			// R60.55 (2026-09-13): DEFER writing the done-chunk until AFTER
			// we check for truncation. Otherwise OpenWebUI receives done=true
			// for the original truncated content, then sees ANOTHER done=true
			// for the combined content (after auto-continue) → displays as
			// duplicate response. We mark `deferredDoneChunk` and emit later.
			st.deferredDoneChunk = true
			break
		}

		st.lastData = data

		var chunk map[string]interface{}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			logger.Get().Debugw("proxyRequestLlamaCpp: skipping non-JSON SSE data",
				"data", data, "error", err)
			continue
		}

		if *st.modelFromCtx == "" {
			if m, ok := chunk["model"].(string); ok && m != "" {
				*st.modelFromCtx = m
			}
		}

		// R493-12a: первый дельта-чанк ЛЮБОГО вида закрывает префилл. Reasoning
		// считается так же, как content: время размышлений — это генерация, а не
		// «оценка промпта» (их токены всегда входят в eval_count).
		if st.firstDeltaTime.IsZero() && chunkHasStatsDeltaR493(chunk) {
			st.firstDeltaTime = time.Now()
		}

		// R65d (2026-09-20): агрегируем статистику токенов со streaming-пути.
		// cppworker присылает OpenAI usage-чанк ({prompt_tokens,
		// completion_tokens}) при stream_options.include_usage.
		st.recordTokenUsageFromUsageChunk(chunk)

		// Для SSE→SSE passthrough (/v1/chat/completions) — проксируем как есть,
		// дополнительно накапливая delta.content на случай если клиент захочет
		// полный контент после [DONE].
		if st.originalPath == "/v1/chat/completions" {
			if err := st.forwardOpenAISSEPassthrough(data, chunk); err != nil {
				return err
			}
			continue
		}
		if err := st.translateSSEToNDJSON(data, chunk); err != nil {
			return err
		}
	}
	return nil
}

// abortBeforeFrame — отмена клиента или таймаут стрима между кадрами.
// true — клиенту уже отдан терминатор, цикл обязан завершиться.
//
// Round 23 (2026-08-04): проверяем отмену клиента ПЕРЕД чтением следующего
// чанка. Без этого streaming-прокси ждёт [DONE] от upstream (cppworker
// генерирует токены, пока cancel не дойдёт через отдельный канал), что
// приводит к зависанию при OpenWebUI cancel — клиент закрыл соединение, но
// прокси продолжает тянуть токены с cppworker и тратить CPU/GPU.
//
// Применяется ко всем путям: /v1/chat/completions (Cline), /api/chat
// (OpenWebUI), /api/generate (Ollama CLI). При срабатывании — закрываем
// upstream body (cppworker прекратит генерацию после текущего token'а)
// и выходим из loop.
//
// R53.6 (2026-08-24): добавлен case <-reqCtx.Done() — streamTimeout (per-model
// profile, default 600s). Без этого case balancer зависает на генерации
// длинного контекста (cppworker генерирует 0 токенов, балансер ждёт [DONE]).
// При срабатывании reqCtx балансер сам отменяет cppworker раньше client timeout
// → R53.5 writeStreamErrorChunk эмитит done:true chunk клиенту → клиент
// получает "stream proxy error" вместо "Did not receive done" (TCP close).
//
// R60.58: select с default БОЛЬШЕ не нужен внутри loop — watcher goroutine
// (см. startUpstreamReaders) закрывает resp.Body при cancel/timeout,
// scanner.Scan() возвращает false, loop выходит естественно. Здесь оставлен
// только для обратной совместимости и для early-detect cancel между чанками
// (быстрее чем ждать Close).
func (st *llamaCppStreamState) abortBeforeFrame() bool {
	select {
	case <-st.r.Context().Done():
		if st.resp.Body != nil {
			_ = st.resp.Body.Close()
		}
		// R65c (2026-09-16): client cancelled mid-stream — emit truncated
		// done chunk so the client's chunked parser (aiohttp) sees terminator
		// before connection tear-down. Without this, OpenWebUI gets
		// TransferEncodingError "Not enough data to satisfy transfer length header".
		st.streamCompleted = true
		emitTruncatedChunk(st.w, st.originalPath, *st.modelFromCtx, time.Since(st.llamaStartTime), "client_cancel")
		return true
	case <-st.reqCtx.Done():
		if st.resp.Body != nil {
			_ = st.resp.Body.Close()
		}
		// R65c (2026-09-16): stream timeout — emit truncated done chunk and
		// return nil so handleChat doesn't write ANOTHER error chunk on top.
		// Pre-R65c: function returned error here, which caused handleChat's
		// writeStreamErrorChunk to fail silently (data already buffered in
		// Go's http.ResponseWriter but never flushed because connection was
		// being torn down). Result: client sees chunked stream without done
		// chunk + without terminator → TransferEncodingError в aiohttp/OpenWebUI.
		st.streamCompleted = true
		emitTruncatedChunk(st.w, st.originalPath, *st.modelFromCtx, time.Since(st.llamaStartTime), "timeout")
		return true
	default:
		return false
	}
}

// decideNativeMode — R66c: режим ответа определяется по ПЕРВОЙ содержательной
// строке, а не по Content-Type: cppworker исторически отдаёт NDJSON и с
// неверным/отсутствующим заголовком. Первая строка с префиксом "data:" означает
// SSE — переключаемся на трансляцию SSE→NDJSON.
func (st *llamaCppStreamState) decideNativeMode(line string) {
	if !st.nativePath || st.nativeModeDecided {
		return
	}
	st.nativeModeDecided = true
	firstLine := strings.TrimSpace(line)
	if strings.HasPrefix(firstLine, "data:") {
		st.nativePath = false
		logger.Get().Warnw("proxyRequestLlamaCpp: upstream answered with OpenAI SSE "+
			"while request was routed as Ollama-native — switching to SSE→NDJSON translation",
			"backend", st.backendID, "model", *st.modelFromCtx, "path", st.originalPath,
			"content_type", st.resp.Header.Get("Content-Type"))
	}
}

// handleNativeNDJSONLine — нативный passthrough (R65d): строка NDJSON уходит
// клиенту как есть, но статистика, дубль в done-кадре (R91/R331), причина
// отмены (R354), окна R493-12a и служебные метки (R266.1) правятся по дороге.
func (st *llamaCppStreamState) handleNativeNDJSONLine(line string) error {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return nil
	}
	// Терпимость к SSE-фреймингу: нативный обработчик cppworker отдаёт
	// «голый» NDJSON, но при смешанных версиях (или когда путь был
	// переписан апстримом) строка может прийти как `data: {...}`.
	// Снимаем префикс, чтобы разбор ниже работал единообразно.
	if strings.HasPrefix(trimmed, "data:") {
		trimmed = strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		if trimmed == "" || trimmed == "[DONE]" {
			// [DONE] — терминатор SSE-стрима; в NDJSON-мире его нет,
			// поэтому просто фиксируем завершение и не пересылаем
			// клиенту невалидную для NDJSON строку.
			if trimmed == "[DONE]" {
				st.streamCompleted = true
			}
			return nil
		}
	}
	// R91 (2026-10-09): текст, отданный ДЕЛЬТАМИ до этой строки. Нужен,
	// чтобы снимать дубль в done-чанке только тогда, когда ответ уже уехал
	// клиенту дельтами (см. stripNativeDoneContent).
	streamedBeforeThisLine := st.accumulatedPlainContent
	var nativeChunk map[string]interface{}
	if err := json.Unmarshal([]byte(trimmed), &nativeChunk); err != nil {
		// Не JSON (например, SSE-комментарий от старой версии
		// cppworker) — отдаём как есть, клиент сам решит.
		logger.Get().Debugw("proxyRequestLlamaCpp[native]: non-JSON NDJSON line passed through",
			"backend", st.backendID, "line", trimmed)
	} else {
		if st.accumulateNativeChunk(nativeChunk) {
			return nil
		}
	}
	outLine := st.normalizeNativeDoneLine(trimmed, streamedBeforeThisLine)
	n, errFwd := st.writeClientLine([]byte(outLine))
	if errFwd != nil {
		st.lastForwardErr = errFwd
		if st.firstForwardErr == nil {
			st.firstForwardErr = errFwd
		}
		return fmt.Errorf("write native NDJSON: %v", errFwd)
	}
	_ = n
	// R66b (2026-09-22): отмечаем, что для нативного пути апстрим уже
	// отдал хотя бы одну NDJSON-строку. Без этого lastData оставался ""
	// (он заполняется только в SSE-ветке ниже), и финальный блок
	//   if lastData == "" && originalPath != "/v1/chat/completions"
	// в конце функции досылал клиенту ЛИШНИЙ синтетический done-чанк
	// ({"done":true,"done_reason":"stop","message":{"content":""}}) уже
	// ПОСЛЕ настоящего done от cppworker.
	//
	// Симптом на реальном клиенте: Cline CLI (ollama provider) получал
	// два done-чанка — первый с tool_calls, второй пустой — и в итоге
	// сообщал "Model returned empty response"; инструмент не вызывался.
	// Ollama-клиенты на два done отвечают
	// "Did not receive done or success response in stream".
	//
	// Флаш уже сделан writeClientLine (у стока — свой, у исторической
	// ветки — тот же http.Flusher). Отдельный флаш здесь означал бы второй
	// набор правил записи — ровно то, что этап 6 и убирает.
	st.lastData = trimmed
	return nil
}

// accumulateNativeChunk — разбор одного нативного NDJSON-кадра: имя модели,
// граница префилла, накопленный текст, завершение хода и статистика токенов.
// true — кадр запустил адресную перезагрузку по n_ctx (R83): поток завершён,
// ответ клиенту уже отдан.
func (st *llamaCppStreamState) accumulateNativeChunk(nativeChunk map[string]interface{}) bool {
	if *st.modelFromCtx == "" {
		if m, ok := nativeChunk["model"].(string); ok && m != "" {
			*st.modelFromCtx = m
			st.modelCtxAtomic.Store(m)
		}
	}
	// R493-12a: первый дельта-чанк ЛЮБОГО вида (включая thinking) —
	// граница префилла. Здесь же ловятся и native-чанки, у которых
	// текст лежит в message.thinking, а не в message.content.
	if st.firstDeltaTime.IsZero() && chunkHasStatsDeltaR493(nativeChunk) {
		st.firstDeltaTime = time.Now()
	}
	// Накапливаем видимый пользователю текст.
	//   /api/chat     → message.content (reasoning идёт в message.reasoning)
	//   /api/generate → response
	//
	// R118.17 (2026-10-15): done-чанк cppworker несёт ПОЛНЫЙ текст,
	// который уже ушёл дельтами (клиенту он не отдаётся — R91 снимает
	// дубль в stripNativeDoneContent). Раньше он ПРИКЛЕИВАЛСЯ к
	// накопленному, и детектор обрыва R60.21 получал текст в двух
	// копиях: число ``` становилось чётным, последняя строка —
	// «полной», и авто-продолжение не срабатывало, хотя клиент видел
	// обрыв. Живой замер до правки: 147 символов дельт + столько же в
	// done-чанке = 294 на входе детектора, truncate_reason="".
	chunkIsDone, _ := nativeChunk["done"].(bool)
	if msg, ok := nativeChunk["message"].(map[string]interface{}); ok {
		// R321: считаем размышления по НАИБОЛЬШЕМУ из двух имён —
		// cppworker отдаёт `thinking` (канон) и `reasoning` (legacy)
		// ОДНИМ И ТЕМ ЖЕ текстом, и суммирование дало бы вдвое больше
		// символов в пояснении клиенту.
		thinkChars := 0
		if t, ok := msg["thinking"].(string); ok {
			thinkChars = len(t)
		}
		if r, ok := msg["reasoning"].(string); ok && len(r) > thinkChars {
			thinkChars = len(r)
		}
		if thinkChars > 0 {
			st.nativeReasoningChars += thinkChars
		}
		if c, ok := msg["content"].(string); ok && c != "" {
			st.accumulatedPlainContent = appendNativeContent(st.accumulatedPlainContent, chunkIsDone, c)
			if st.firstContentTime.IsZero() {
				st.firstContentTime = time.Now()
			}
		}
	}
	if rsp, ok := nativeChunk["response"].(string); ok && rsp != "" {
		st.accumulatedPlainContent = appendNativeContent(st.accumulatedPlainContent, chunkIsDone, rsp)
		if st.firstContentTime.IsZero() {
			st.firstContentTime = time.Now()
		}
	}
	if done, ok := nativeChunk["done"].(bool); ok && done {
		st.streamCompleted = true
		st.usageChunkSeen = true
		st.priorDoneEmitted = true
		// H2 (2026-10-16): нативный done-чанк отдаётся как есть, поэтому
		// статистика считается дошедшей только если cppworker реально
		// положил её в чанк (eval_count/prompt_eval_count). Если полей
		// нет — statsEmitted остаётся false, и поздний SSE usage-чанк
		// не будет потерян.
		if _, hasEval := nativeChunk["eval_count"]; hasEval {
			st.statsEmitted = true
		} else if _, hasPrompt := nativeChunk["prompt_eval_count"]; hasPrompt {
			st.statsEmitted = true
		}
		st.upstreamHadFinishReason = true
		if c, ok := nativeChunk["error"].(string); ok && c != "" {
			// R354 (2026-10-31): отмена ПОЛЬЗОВАТЕЛЕМ (кнопка Stop) — не сбой
			// воркера. Такой кадр не идёт в upstreamErrorText (по нему
			// работают auto-continue и распознавание n_ctx) и не пишется как
			// Warn: клиенту уйдёт нормальный терминатор с done_reason=cancelled.
			if isUserCancelDoneChunkR354(c) {
				st.userCancelledDoneChunkR354 = true
				logger.Get().Infow("proxyRequestLlamaCpp[native]: генерация отменена клиентом (R354)",
					"backend", st.backendID, "model", *st.modelFromCtx)
			} else {
				// R289: запоминаем текст ошибки — по нему отличим «контекст
				// мал» от обрыва до того, как сработает auto-continue.
				st.upstreamErrorText = c
				logger.Get().Warnw("proxyRequestLlamaCpp[native]: upstream done-chunk carries error",
					"backend", st.backendID, "model", *st.modelFromCtx, "error", c)

				// R83 (2026-09-29): n_ctx-ошибка в done-чанке = устаревший кэш
				// n_ctx в балансере. Запускаем тот же auto-reload, что и для
				// HTTP 4xx ниже — иначе получается ДЕДЛОК.
				//
				// Живой сценарий (лог 2026-09-29): балансер держал в
				// LastKnownNCtx 128000, а cppworker после рестарта поднял
				// модель с 32768. Preflight видел «128000 >= required» и
				// возвращал NoOp (reload не запускался), а cppworker на каждый
				// запрос отвечал done-чанком «code 2: requested n_ctx=128000
				// exceeds model's effective n_ctx=32768». Клиент (Cline)
				// получал ошибку на каждом запросе, и так до следующего
				// удачного reload.
				//
				// cppworker отдаёт это как HTTP 200 + error в финальном чанке,
				// поэтому ветка `resp.StatusCode >= 400` ниже не срабатывала.
				//
				// ВАЖНО: ParseCppWorkerError намеренно возвращает nil для
				// status < 400, поэтому распознаём n_ctx-ошибку по тексту
				// самого сообщения («code 2» / «code 3» — те же коды, что
				// bridge.ErrCodeNCtxNeedsReload / ErrCodePromptTooLong).
				if st.p.nctxReload != nil && isNCtxDoneChunkError(c) {
					reloadModel := *st.modelFromCtx
					if reloadModel == "" {
						reloadModel = st.backendID
					}
					logger.Get().Infow(i18n.T("balancer.llamacpp_transport.done_chunk_nctx_error_reload_info"),
						"backend", st.backendID, "model", reloadModel, "error", c)
					nctxErr := newNCtxErrorFromDoneChunk(c, st.backendID)
					st.p.handleNCtxReload(st.r.Context(), st.w, st.r, st.backendID, reloadModel, nctxErr, st.bodyBuf)
					return true
				}
			}
		}
		// R65d: агрегируем статистику токенов для /api/v1/stats/tokens.
		// Нативный формат Ollama: prompt_eval_count / eval_count.
		if !st.tokenUsageRecorded {
			promptTokens := int64(intFromMap(nativeChunk, "prompt_eval_count"))
			evalTokens := int64(intFromMap(nativeChunk, "eval_count"))
			if promptTokens > 0 || evalTokens > 0 {
				st.tokenUsageRecorded = true
				st.p.recordTokenUsage(*st.modelFromCtx, promptTokens, evalTokens)
			}
		}
	}
	return false
}

// normalizeNativeDoneLine — что именно уйдёт клиенту в этой строке: снятие
// дубля content в завершающем кадре (R91/R331), причина отмены (R354), окна
// статистики R493-12a и вычищение разрезанных служебных меток (R266.1).
func (st *llamaCppStreamState) normalizeNativeDoneLine(trimmed, streamedBeforeThisLine string) string {
	// R83 §9.7 (2026-09-27): нормализуем финальный чанк ПЕРЕД отдачей
	// клиенту. cppworker кладёт в done-чанк ПОЛНЫЙ текст ответа, хотя
	// дельты его уже отдали (измерено 2.00×: столько же байт в дельтах
	// и в done). Настоящая Ollama в done-чанке отдаёт `message.content:
	// ""`, и OpenWebUI с ней работает — значит дубль ломает именно
	// клиентов, конкатенирующих чанки. Оставляем все статы.
	//
	// R91 (2026-10-09): ТОЛЬКО если текст уже уехал дельтами. Раньше
	// content вычищался безусловно, и апстрим, отдавший весь ответ ОДНИМ
	// финальным чанком (mock/статический ответ/фолбэк cppworker, когда
	// модель ушла в незакрытый reasoning и текст отдан в done-чанке),
	// терял ответ целиком: клиент получал done:true с пустым content.
	// Именно это ловили tests/llamacpp_proxy (TestChatStreaming,
	// TestResponseNotMarkdownBold/streaming) и tests (TestProxyOllama_*).
	//
	// ВАЖНО: accumulatedPlainContent/upstreamDoneContent выше уже
	// накопили полный текст (для R60.21 auto-continue) — здесь правится
	// только то, что уходит по проводу.
	//
	// R331 (2026-10-30): ОДИН владелец content завершающего кадра.
	//
	// Раньше по проводу кадр правили ДВА независимых слоя подряд:
	// stripNativeDoneContent (R91: снять дубль уже отданного текста) и
	// nativeDoneHygieneR321 (R321: причина + пояснение, когда клиент не
	// видел ничего). Каждый из них не знал о другом: strip мог очистить
	// content, и тогда hygiene видела «пустой ответ» и вставляла пояснение
	// поверх нормального завершения (регрессия R321, ловил оператор).
	//
	// Теперь решение одно: finalizeNativeDoneFrameR331. Внутри — тот же
	// порядок (сначала снять дубль, потом решить про причину/пояснение), но
	// состояние считается ОДИН раз, поэтому слои не могут разойтись.
	outLine := trimmed
	if st.userCancelledDoneChunkR354 {
		// R354 (2026-10-31): ОДИН владелец кадра отмены. Правим ТОЛЬКО причину
		// (done_reason=cancelled из реестра R328, без error): model/created_at/
		// статистика остаются, клиент по ним закрывает ход и считает токены.
		//
		// Через finalizeNativeDoneFrameR331 такой кадр НЕ пускаем: тот видит
		// пустой response и вставил бы пояснение «ответа нет» (регрессия R321),
		// хотя клиент сам остановил генерацию.
		if fixed, changed := normalizeCancelDoneChunkR354(trimmed); changed {
			outLine = fixed
		}
	} else if st.originalPath == "/api/chat" || st.originalPath == "/api/generate" {
		fixed, changed := finalizeNativeDoneFrameR331(trimmed, streamedBeforeThisLine,
			st.nativeReasoningChars, nativeDoneContentStrippingEnabled())
		if changed {
			logger.Get().Infow("proxyRequestLlamaCpp[native]: done-кадр нормализован (R331)",
				"backend", st.backendID, "model", *st.modelFromCtx,
				"reasoning_chars", st.nativeReasoningChars,
				"visible_streamed_chars", len(strings.TrimSpace(streamedBeforeThisLine)))
			outLine = fixed
		} else {
			outLine = trimmed
		}
	} else {
		outLine = trimmed
	}
	// R493-12a (2026-11-12): нативный done-кадр несёт статистику cppworker,
	// и она не знает про очередь/авто-загрузку/preflight. Живой замер на
	// стенде: клиент ждал 62 с (модель грузилась), а получил total_duration
	// 2.15 с, load_duration 0 и вообще без tokens_per_second. Здесь
	// добавляется то, что видит ТОЛЬКО балансер: load_duration и
	// total = load + prompt + eval (см. applyStatsWindowsNativeR493).
	if st.originalPath == "/api/chat" || st.originalPath == "/api/generate" {
		if fixed, changed := applyStatsWindowsNativeR493(outLine, st.statsWindowsNative); changed {
			outLine = fixed
		}
	}
	// R266.1 (2026-10-28): на нативном пути cppworker уже отдаёт NDJSON,
	// и балансер писал строку клиенту как есть — поэтому служебные метки
	// шаблона (`<|assistant|>`, `</think>`) утекали в message.content
	// ровно здесь. Чистим строку перед отправкой, придерживая хвост
	// разрезанной метки (см. marker_hold_ndjson_r266_1.go).
	if st.originalPath == "/api/chat" || st.originalPath == "/api/generate" {
		outLine = filterNDJSONLineMarkers(outLine, &st.ndjsonHold)
	}
	return outLine
}

// writeClientLine — одна точка записи кадра в этом цикле (R493-12r).
// Возвращает (байт_записано, ошибка). При выключенном флаге это ровно прежний
// `fmt.Fprintf(w, "%s\n", line)` + `bytesForwarded.Add` + `Flush`.
func (st *llamaCppStreamState) writeClientLine(line []byte) (int, error) {
	if st.sink != nil {
		// Счётчик — у стока (единственный источник правды о том, сколько
		// байт ушло клиенту): сводка берёт РАЗНИЦУ, а не число из цикла,
		// иначе два счётчика снова начнут расходиться.
		before := st.sink.Written()
		_, err := st.sink.WriteLine(line)
		sent := int(st.sink.Written() - before)
		st.bytesForwarded.Add(int64(sent))
		return sent, err
	}
	n, err := fmt.Fprintf(st.w, "%s\n", string(line))
	st.bytesForwarded.Add(int64(n))
	if flusher, ok := st.w.(http.Flusher); ok {
		flusher.Flush()
	}
	return n, err
}

// flushBufferedText — отдать накопленный текст клиенту как обычный content
// (R292). Один помощник на все причины (предел буфера и «это проза»), чтобы
// ветки не разошлись в том, что клиент видит.
func (st *llamaCppStreamState) flushBufferedText(text, reason string) error {
	clean := strings.TrimSpace(stripQwenServiceMarkers(text))
	if clean == "" {
		return nil
	}
	chunk := map[string]interface{}{
		"model":      *st.modelFromCtx,
		"created_at": time.Now().UTC().Format(time.RFC3339),
		"done":       false,
		"message": map[string]interface{}{
			"role":    "assistant",
			"content": clean,
		},
	}
	out, errMarshal := json.Marshal(chunk)
	if errMarshal != nil {
		return nil
	}
	n, errWrite := st.writeClientLine(out)
	if errWrite != nil {
		st.lastForwardErr = errWrite
		if st.firstForwardErr == nil {
			st.firstForwardErr = errWrite
		}
		return fmt.Errorf("write NDJSON (buffered text flush/%s): %v", reason, errWrite)
	}
	_ = n
	st.accumulatedPlainContent += clean
	logger.Get().Infow("proxyRequestLlamaCpp: buffered text flushed to client",
		"backend", st.backendID, "model", *st.modelFromCtx,
		"reason", reason, "chars", len(clean))
	return nil
}

// recordTokenUsageFromUsageChunk — R65d: агрегируем статистику токенов со
// streaming-пути (cppworker присылает OpenAI usage-чанк при
// stream_options.include_usage).
//
// До R65d recordTokenUsage вызывался только из non-streaming путей,
// поэтому /api/v1/stats/tokens был пуст для всего реального трафика.
// cppworker присылает OpenAI usage-чанк ({prompt_tokens,
// completion_tokens}) при stream_options.include_usage (по умолчанию
// включено, handlers_openai.go:427-438). Флаг tokenUsageRecorded защищает от
// двойного учёта одного запроса: на нативном пути статистика приходит в
// done-чанке, в SSE — отдельным usage-чанком, и оба могут быть в одном стриме.
func (st *llamaCppStreamState) recordTokenUsageFromUsageChunk(chunk map[string]interface{}) {
	if st.tokenUsageRecorded {
		return
	}
	if usage, ok := chunk["usage"].(map[string]interface{}); ok {
		promptTokens := int64(intFromMap(usage, "prompt_tokens"))
		completionTokens := int64(intFromMap(usage, "completion_tokens"))
		if promptTokens > 0 || completionTokens > 0 {
			st.tokenUsageRecorded = true
			st.p.recordTokenUsage(*st.modelFromCtx, promptTokens, completionTokens)
		}
	}
}

// forwardOpenAISSEPassthrough — SSE→SSE passthrough для /v1/chat/completions:
// кадр уходит клиенту как есть, но с вычищением служебных токенов (R231) и
// приведением tool_calls к объявленной схеме (R186/R254).
func (st *llamaCppStreamState) forwardOpenAISSEPassthrough(data string, chunk map[string]interface{}) error {
	// Накапливаем plain content для /v1/chat/completions (на случай будущего использования).
	if choices, ok := chunk["choices"].([]interface{}); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]interface{}); ok {
			if delta, ok := choice["delta"].(map[string]interface{}); ok {
				if c, ok := delta["content"].(string); ok && c != "" {
					st.accumulatedPlainContent += c
				}
			}
		}
	}
	// SSE → SSE passthrough
	//
	// R231 (2026-10-24): ЗДЕСЬ БЫЛА ОШИБКА ВЫЗОВА ФИЛЬТРА, из-за которой
	// он фактически не работал. filterOpenAIStreamingLine ожидает
	// SSE-строку "data: {...}" и проверяет HasPrefix("data:"), а сюда
	// передавался ГОЛЫЙ JSON (префикс уже срезан выше) — проверка не
	// срабатывала, и служебные токены/маркеры размышлений уезжали клиенту.
	// Второй дефект: возвращаемый bool означает «модифицирован», но
	// трактовался как «shouldSkip» — чанк с вырезанным токеном просто
	// выбрасывался (терялся текст ответа).
	// Теперь: собираем SSE-строку, фильтруем, и если контент почищен —
	// продолжаем обработку уже с очищенным JSON.
	if filteredLine, modified := filterAssistantEgressForClient([]byte("data: "+data), st.egressFilter); modified {
		cleaned := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(filteredLine)), "data:"))
		if cleaned != "" {
			data = cleaned
		}
	}
	if choices, ok := chunk["choices"].([]interface{}); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]interface{}); ok {
			if delta, ok := choice["delta"].(map[string]interface{}); ok {
				accumulateToolCallsFromDelta(delta, st.toolAccum)
			}
		}
	}
	if modified := extractToolCallsFromSSEContentWithSchema([]byte(data), st.toolSchemas, st.toolRepair); modified != nil {
		n, errFwd := fmt.Fprintf(st.w, "data: %s\n\n", string(modified))
		st.bytesForwarded.Add(int64(n))
		if errFwd != nil {
			st.lastForwardErr = errFwd
			if st.firstForwardErr == nil {
				st.firstForwardErr = errFwd
			}
			return fmt.Errorf("write modified SSE (tool calls): %v", errFwd)
		}
	} else if normalized := normalizeSSEToolCallChunks([]byte(data), st.toolSchemas, st.toolRepair); normalized != nil {
		// R186: нативные вызовы от воркера тоже приводим к форме OpenAI и к
		// объявленной схеме — валидатор клиента проверяет схему независимо
		// от того, кто собрал вызов.
		n, errFwd := fmt.Fprintf(st.w, "data: %s\n\n", string(normalized))
		st.bytesForwarded.Add(int64(n))
		if errFwd != nil {
			st.lastForwardErr = errFwd
			if st.firstForwardErr == nil {
				st.firstForwardErr = errFwd
			}
			return fmt.Errorf("write normalized SSE (tool calls): %v", errFwd)
		}
	} else {
		n, errFwd := fmt.Fprintf(st.w, "data: %s\n\n", data)
		st.bytesForwarded.Add(int64(n))
		if errFwd != nil {
			st.lastForwardErr = errFwd
			if st.firstForwardErr == nil {
				st.firstForwardErr = errFwd
			}
			return fmt.Errorf("write SSE: %v", errFwd)
		}
	}
	if flusher, ok := st.w.(http.Flusher); ok {
		flusher.Flush()
	}
	return nil
}

// translateSSEToNDJSON — SSE→NDJSON для /api/chat и /api/generate: накопление
// content, кросс-чанковое распознавание tool_calls, финальный кадр от воркера,
// перевод кадра в Ollama-форму и запись клиенту.
func (st *llamaCppStreamState) translateSSEToNDJSON(data string, chunk map[string]interface{}) error {
	// ==== SSE → NDJSON: для /api/generate и /api/chat =====
	var deltaContent string
	if st.originalPath == "/api/chat" {
		deltaContent = st.accumulateChatDelta(data, chunk)
		newDelta, skip, errBuf := st.absorbToolCallBuffer(deltaContent)
		if errBuf != nil {
			return errBuf
		}
		deltaContent = newDelta
		if skip {
			return nil
		}
		// Накапливаем plain content (вне tool-call accumulation) для финального
		// done-чанка — это спасает от пустого content если upstream прислал
		// полный ответ только в done-чанке.
		//
		// НЕ накапливаем чанки с finish_reason: cppworker прислал финальный
		// чанк с content=" llama.cpp!" + finish_reason="stop". Этот content
		// уже проксируется через writeStreamingSSEDone ниже (агрегированный
		// в upstreamDoneContent / accumulatedPlainContent). Без этого guard
		// в финальный done-чанк попадал дубликат:
		// "Hello from llama.cpp! llama.cpp!" (см. PF-6 fail 2026-06-27).
		if deltaContent != "" && !st.contentToolCallsProcessed && !hasFinishReason(data) {
			st.accumulatedPlainContent += deltaContent
		}
		st.absorbChatDoneChunk(data)
	} else if st.originalPath == "/api/generate" {
		st.accumulateGenerateDelta(data, chunk)
	}
	return st.translateAndWriteFrame(data)
}

// accumulateChatDelta — дельта /api/chat: фрагменты tool_calls, текст дельты,
// время первого content-чанка и content финального кадра воркера (R60.22).
func (st *llamaCppStreamState) accumulateChatDelta(data string, chunk map[string]interface{}) string {
	var deltaContent string
	if choices, ok := chunk["choices"].([]interface{}); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]interface{}); ok {
			if delta, ok := choice["delta"].(map[string]interface{}); ok {
				accumulateToolCallsFromDelta(delta, st.toolAccum)
				if c, ok := delta["content"].(string); ok {
					deltaContent = c
					// R60.22: capture time of first non-empty content
					// chunk (TTFT) for accurate prompt_eval_duration.
					if c != "" && st.firstContentTime.IsZero() {
						st.firstContentTime = time.Now()
					}
				}
			}
		}
	}

	// Если чанк содержит finish_reason (done:true) — извлекаем content и
	// done_reason из message, чтобы сохранить полный ответ cppworker'а.
	if upstreamHasDone, msgMap, _ := extractUpstreamDoneChunk(data); upstreamHasDone {
		if c, ok := msgMap["content"].(string); ok {
			st.upstreamDoneContent = c
			// R60.22: capture time of first non-empty content (TTFT).
			// cppworker иногда посылает весь ответ в одном done-чанке
			// (final chunk has both content and finish_reason). В этом
			// случае firstContentTime отражает время получения первого
			// значимого content, даже если он пришёл в done-чанке.
			if c != "" && st.firstContentTime.IsZero() {
				st.firstContentTime = time.Now()
			}
		}
	}
	return deltaContent
}

// absorbToolCallBuffer — кросс-чанковое накопление content для JSON tool calls
// (Gemma-4/Qwen/Hermes выводят вызов обычным текстом, разбитым на чанки).
//
// Кросс-чанковое накопление content для детекции JSON tool calls.
// Поддерживаемые триггеры накопления:
//   - "[" — стандартный JSON-массив tool_calls
//   - "<tool_call>" / "<|python_tag|>" — Hermes/Qwen/Llama-3 форматы
//   - "{name" — начало одиночного JSON-объекта tool_call
//   - contentBuffer не пуст — продолжаем накапливать уже начатый tool call
//
// Возвращает (новый deltaContent, skip, ошибка). skip=true — кадр уже
// обработан этим блоком (накопление продолжается или текст отдан как проза),
// писать его штатным переводом не нужно. Ошибка — только запись клиенту.
func (st *llamaCppStreamState) absorbToolCallBuffer(deltaContent string) (string, bool, error) {
	if deltaContent == "" || st.contentToolCallsProcessed {
		return deltaContent, false, nil
	}
	// R266 (2026-10-28): служебные метки qwen3.8 (`<|tool|>`,
	// `<|assistant|>`, `<|tool_call|>`, …) в видимом тексте.
	//
	// ЖИВОЙ ФАКТ (Cline, /api/chat, qwen3.8): в ответе приходило
	// «… </think> <|tool|> tool_call_result(run_commands): [...] <|assistant|>
	// The file has ~348 lines …». Триггеры накопления ниже знали только
	// `[`, `<tool_call>`, `<|python_tag|>`, `[TOOL_CALLS]`, `{name`, поэтому
	// чанк с меткой уходил в message.content как обычный текст.
	//
	// Метки вычищаем ДО разбора: после этого остаётся либо чистый текст,
	// либо JSON вызова, который распознаётся штатным детектором.
	deltaContent = stripQwenServiceMarkers(deltaContent)
	trimmed := strings.TrimSpace(deltaContent)
	startToolCallAccum := strings.HasPrefix(trimmed, "[") ||
		strings.HasPrefix(trimmed, "<tool_call>") ||
		strings.HasPrefix(trimmed, "<|python_tag|>") ||
		strings.HasPrefix(trimmed, "[TOOL_CALLS]") ||
		strings.HasPrefix(trimmed, "{name") ||
		hasQwenServiceMarker(trimmed) ||
		// Метка разрезана между чанками: `<|tool_` + `call|>`. Хвост
		// придерживаем, иначе первая половина уедет клиенту текстом.
		splitMarkerSuffixLen(trimmed) > 0

	// R114c (2026-10-13): JSON-структура НАЧАЛАСЬ ПОСРЕДИ чанка
	// (проза + вызов в одном чанке). Раньше такой чанк уходил
	// клиенту целиком — в теле ответа оказывался обрывок вызова
	// («…task complete. [{"id":"call_submit_and_exit",…»). Проза
	// отдаётся сразу, а накопление начинается со скобки.
	if !startToolCallAccum && st.contentBuffer == "" {
		if pos, ok := jsonStructStartPos(deltaContent); ok && pos > 0 {
			prose := deltaContent[:pos]
			proseChunk := map[string]interface{}{
				"model":      *st.modelFromCtx,
				"created_at": time.Now().UTC().Format(time.RFC3339),
				"done":       false,
				"message": map[string]interface{}{
					"role":    "assistant",
					"content": prose,
				},
			}
			outProse, _ := json.Marshal(proseChunk)
			nProse, errProse := fmt.Fprintf(st.w, "%s\n", string(outProse))
			st.bytesForwarded.Add(int64(nProse))
			if errProse != nil {
				if st.firstForwardErr == nil {
					st.firstForwardErr = errProse
				}
				return deltaContent, false, fmt.Errorf("write NDJSON (prose before JSON struct): %v", errProse)
			}
			if flusher, ok := st.w.(http.Flusher); ok {
				flusher.Flush()
			}
			st.accumulatedPlainContent += prose
			if st.firstContentTime.IsZero() {
				st.firstContentTime = time.Now()
			}
			deltaContent = deltaContent[pos:]
			trimmed = strings.TrimSpace(deltaContent)
			startToolCallAccum = true
		}
	}

	if st.contentBufferGaveUp || (!startToolCallAccum && st.contentBuffer == "") {
		return deltaContent, false, nil
	}
	st.contentBuffer += deltaContent
	if st.contentBufferSince.IsZero() {
		st.contentBufferSince = time.Now()
	}

	// Кросс-чанковая склейка может оставить "лишний" символ '}' перед
	// закрывающим </tool_call> (когда JSON закрывается в одном чанке, а
	// '}'</tool_call>' приходит в следующем). Схлопываем дубликат.
	st.contentBuffer = collapseDuplicateClosingBrace(st.contentBuffer)

	// Проверяем, не накопился ли полный JSON tool call
	if detectedTC, remainingTC, found := detectAndExtractToolCallsFromContent(st.contentBuffer); found {
		st.contentToolCallsProcessed = true
		st.contentBuffer = ""
		logger.Get().Infow("proxyRequestLlamaCpp: cross-chunk tool_calls detected in content",
			"backend", st.backendID, "model", *st.modelFromCtx,
			"tool_calls_count", len(detectedTC))

		cleanRemaining := stripServiceTokens(remainingTC)
		cleanRemaining = cleanContentAfterToolCallExtraction(cleanRemaining)

		msgMap := map[string]interface{}{
			"role":       "assistant",
			"content":    cleanRemaining,
			"tool_calls": detectedTC,
		}
		ollamaChunk := map[string]interface{}{
			"model":      *st.modelFromCtx,
			"created_at": time.Now().UTC().Format(time.RFC3339),
			"done":       false,
			"message":    msgMap,
		}
		out, _ := json.Marshal(ollamaChunk)
		n, errFwd := fmt.Fprintf(st.w, "%s\n", string(out))
		st.bytesForwarded.Add(int64(n))
		if errFwd != nil {
			st.lastForwardErr = errFwd
			if st.firstForwardErr == nil {
				st.firstForwardErr = errFwd
			}
			return deltaContent, false, fmt.Errorf("write NDJSON (cross-chunk tool calls): %v", errFwd)
		}
		if flusher, ok := st.w.(http.Flusher); ok {
			flusher.Flush()
		}
		return deltaContent, true, nil
	}

	// Если JSON массив закрыт (найден ']' на правильной глубине),
	// но tool call не обнаружен — это не tool call JSON.
	// Сбрасываем буфер и даём чанку пройти через нормальный translate.
	if endPos := findMatchingClosingBracket(st.contentBuffer, 0); endPos >= 0 {
		st.contentBuffer = ""
		st.contentBufferSince = time.Time{}
		return deltaContent, false, nil
	}
	if len(st.contentBuffer) > toolCallBufferLimit() {
		// R266: раньше здесь буфер ВЫБРАСЫВАЛСЯ (жёсткие 4096 байт),
		// и вызов инструмента, который пишет файл на сотни строк,
		// терялся, а его обрывок с метками уезжал клиенту как текст.
		// Теперь текст не теряем: отдаём очищенным от служебных меток
		// как обычный content и продолжаем стрим.
		overflow := st.contentBuffer
		st.contentBuffer = ""
		st.contentBufferSince = time.Time{}
		logger.Get().Warnw("proxyRequestLlamaCpp: tool-call buffer limit reached, text flushed",
			"backend", st.backendID, "model", *st.modelFromCtx,
			"limit_bytes", toolCallBufferLimit(), "flushed_chars", len(overflow))
		if errFlush := st.flushBufferedText(overflow, "buffer_limit"); errFlush != nil {
			return deltaContent, false, errFlush
		}
		return deltaContent, true, nil
	}
	if shouldFlushToolBufferAsProseR292(st.contentBuffer, st.contentBufferSince) {
		// R292: текст лишь НАЧАЛСЯ как JSON/маркер, но признаков
		// вызова в нём нет (ни "name":, ни <|tool|>, …) и он уже
		// пересидел grace/объём. Это проза — отдаём клиенту и
		// прекращаем накопление до конца стрима.
		//
		// ЖИВОЙ СИМПТОМ, который это лечит: ответ начинался на `[`
		// (маркдаун-список/массив в тексте), детектор держал его в
		// буфере до 64 КиБ, и клиент видел «план вместе с
		// размышлениями» одним куском в конце, а не потоком.
		prose := st.contentBuffer
		st.contentBuffer = ""
		st.contentBufferSince = time.Time{}
		st.contentBufferGaveUp = true
		logger.Get().Infow("proxyRequestLlamaCpp: buffered text is prose, streaming it",
			"backend", st.backendID, "model", *st.modelFromCtx,
			"chars", len(prose), "grace_ms", toolCallProseGrace().Milliseconds())
		if errFlush := st.flushBufferedText(prose, "prose_not_toolcall"); errFlush != nil {
			return deltaContent, false, errFlush
		}
		return deltaContent, true, nil
	}
	// Всё ещё накапливаем — пропускаем этот чанк
	return deltaContent, true, nil
}

// absorbChatDoneChunk — финальный кадр cppworker на пути /api/chat: ПОЛНЫЙ
// content приходит в message.content, и tool call может прийти ЦЕЛИКОМ здесь.
func (st *llamaCppStreamState) absorbChatDoneChunk(data string) {
	// Финальный чанк cppworker (с finish_reason) присылает ПОЛНЫЙ content
	// в message.content — используем его как upstreamDoneContent. Это
	// перезаписывает накопленный accumulatedPlainContent чтобы избежать
	// дубликата ("Hello from llama.cpp!" + "Hello from llama.cpp!").
	if upstreamHasDone, msgMap, _ := extractUpstreamDoneChunk(data); upstreamHasDone {
		if c, ok := msgMap["content"].(string); ok && c != "" {
			st.upstreamDoneContent = c
		}
		// R83-фикс (2026-10-01): tool call может прийти ЦЕЛИКОМ в
		// финальном (done) чанке — ровно так делает cppworker на
		// пути /api/chat с tools: он буферизует ответ и отдаёт его
		// одним сообщением с finish_reason.
		//
		// Живой дефект (Cline → балансер → /api/chat): модель
		// выдала JSON-массив tool_calls, cppworker вернул его в
		// message.content последнего чанка, а балансер этот content
		// «вырезал» (он ждал tool call в deltaContent, где его не
		// было) и клиент получал пустой ответ:
		//   "model produced an empty response (inference succeeded
		//    but output is empty)" ← сообщение Cline.
		// Прямой запрос в cppworker при этом отдавал корректный
		// JSON, а OpenAI-путь (/v1/chat/completions) — корректный
		// tool_calls. Ломался только Ollama-путь.
		//
		// Теперь, как и для дельт, пытаемся извлечь tool calls из
		// финального content и, если получилось, отдаём их штатным
		// механизмом (writeStreamingSSEDone с toolAccum). Если не
		// получилось — content остаётся как есть и доезжает до
		// клиента: потерять ответ модели нельзя.
		if !st.contentToolCallsProcessed && st.upstreamDoneContent != "" {
			if detectedTC, remainingTC, found := detectAndExtractToolCallsFromContent(st.upstreamDoneContent); found && len(detectedTC) > 0 {
				// Кладём в тот же аккумулятор, что и дельты
				// (toolAccum — map[index]*accumulatedToolCall),
				// чтобы финальный NDJSON отдал их штатно.
				for i, raw := range detectedTC {
					tcm, ok := raw.(map[string]interface{})
					if !ok {
						continue
					}
					entry := &accumulatedToolCall{index: i, function: map[string]interface{}{}}
					if id, ok := tcm["id"].(string); ok {
						entry.id = id
					}
					if fn, ok := tcm["function"].(map[string]interface{}); ok {
						entry.function = fn
					}
					st.toolAccum[i] = entry
				}
				st.contentToolCallsProcessed = true
				st.upstreamDoneContent = cleanContentAfterToolCallExtraction(stripServiceTokens(remainingTC))
				logger.Get().Infow(i18n.T("balancer.llamacpp_transport.tool_calls_extracted_info"),
					"backend", st.backendID, "model", *st.modelFromCtx,
					"tool_calls_count", len(detectedTC),
					"remaining_content_len", len(st.upstreamDoneContent))
			}
		}
	}
}

// accumulateGenerateDelta — дельта /api/generate: текст кадра и content
// финального кадра (без дубликата " llama.cpp!").
func (st *llamaCppStreamState) accumulateGenerateDelta(data string, chunk map[string]interface{}) {
	// Для /api/generate content лежит в chunk["choices"][0].text или в done_chunk.response
	if choices, ok := chunk["choices"].([]interface{}); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]interface{}); ok {
			if t, ok := choice["text"].(string); ok && t != "" {
				// НЕ накапливаем чанки с finish_reason (аналогично /api/chat):
				// cppworker в финальном чанке шлёт ПОЛНЫЙ content=" llama.cpp!",
				// а accumulatedPlainContent уже содержит "Hello from". Без guard
				// финальный done-чанк получал дубликат ("Hello from llama.cpp! llama.cpp!").
				if !hasFinishReason(data) {
					st.accumulatedPlainContent += t
				}
			}
		}
	}
	// Финальный чанк cppworker с finish_reason: используем accumulatedPlainContent
	// (без дубликата " llama.cpp!") как upstreamDoneContent. Если cppworker
	// прислал в финальном чанке text=" llama.cpp!" И finish_reason="stop",
	// мы уже НЕ добавили его в accumulatedPlainContent (см. guard выше),
	// поэтому накопленный контент = "Hello from". Дописываем " llama.cpp!"
	// из финального чанка для полноты ответа.
	if hasFinishReason(data) {
		if choices, ok := chunk["choices"].([]interface{}); ok && len(choices) > 0 {
			if choice, ok := choices[0].(map[string]interface{}); ok {
				if t, ok := choice["text"].(string); ok && t != "" {
					st.upstreamDoneContent = st.accumulatedPlainContent + t
				}
			}
		}
		if st.upstreamDoneContent == "" {
			st.upstreamDoneContent = st.accumulatedPlainContent
		}
	}
}

// translateAndWriteFrame — общий хвост NDJSON-путей: завершение по
// finish_reason/tool_calls, вычищение меток (R266.1), единый переводщик с
// четырьмя окнами статистики (R493-12a) и запись кадра клиенту.
func (st *llamaCppStreamState) translateAndWriteFrame(data string) error {
	// Подавляем перевод только если tool_calls уже были обработаны
	// (cross-chunk или delta) — для них writeStreamingSSEDone формирует
	// финальный NDJSON с правильным message.tool_calls.
	//
	// Для чанков с finish_reason НЕ делаем continue — translateOpenAISSEDataToOllama
	// сам корректно сформирует финальный NDJSON с done:true, done_reason:stop
	// и content из текста чанка. Это и есть требуемая passthrough-семантика
	// для streaming: каждый upstream чанк → отдельный NDJSON чанк клиенту,
	// включая финальный с done:true.
	//
	// PF-6 fix (2026-06-27): cppworker НЕ отправляет [DONE] маркер в
	// streaming-режиме — он закрывает TCP сразу после чанка с finish_reason.
	// Чтобы избежать ложного "truncation" alert, помечаем streamCompleted
	// при первом чанке с finish_reason.
	if st.contentToolCallsProcessed || len(st.toolAccum) > 0 {
		// tool_calls уже были обработаны выше — writeStreamingSSEDone возьмёт
		// их из toolAccum и отправит финальный NDJSON с tool_calls.
		// upstreamHadFinishReason=false: даже если cppworker прислал finish_reason
		// в этом же чанке, tool_calls должны быть эмитированы (предыдущая ветка
		// кода обработала cross-chunk tool calls в отдельном NDJSON-чанке).
		if !st.streamCompleted {
			st.streamCompleted = true
			errFwd := writeStreamingSSEDone(st.w, st.originalPath, *st.modelFromCtx, st.toolAccum,
				st.accumulatedPlainContent, st.upstreamDoneContent, false, st.usageChunkSeen)
			if errFwd != nil {
				logger.Get().Warnw("proxyRequestLlamaCpp: writeStreamingSSEDone on tool_calls error",
					"backend", st.backendID, "error", errFwd)
				st.lastForwardErr = errFwd
				if st.firstForwardErr == nil {
					st.firstForwardErr = errFwd
				}
			}
			// R65d (2026-09-20): помечаем canonical done как отправленный.
			//
			// БЕЗ ЭТОГО: writeStreamingSSEDone уже отдал клиенту done:true
			// (с tool_calls), но priorDoneEmitted/usageChunkSeen оставались
			// false. Далее cppworker присылает свой wrapper-чанк
			// (finish_reason без content) и usage-чанк — и
			// translateUsageChunkToOllama, не видя priorDoneEmitted,
			// эмитил ВТОРОЙ done:true. Ollama-клиенты (Cline, ollama npm)
			// на два done-чанка отвечают
			// "Did not receive done or success response in stream".
			//
			// H2 (2026-10-16): writeStreamingSSEDone НЕ пишет статистику,
			// поэтому statsEmitted здесь СОЗНАТЕЛЬНО не выставляется —
			// поздний usage-чанк должен дойти до клиента строкой с
			// done:false (иначе eval_count/prompt_eval_count теряются).
			st.usageChunkSeen = true
			st.priorDoneEmitted = true
			Flush(st.w)
		}
		return nil
	}
	if hasFinishReason(data) && !st.streamCompleted {
		// cppworker закрывает TCP после этого чанка — отмечаем что стрим
		// завершён нормально, чтобы ниже не сработал stream-truncation detection.
		st.streamCompleted = true
		// PF-6 (2026-06-27) + dedup streaming fix (2026-06-30):
		// translateOpenAISSEDataToOllama сам формирует финальный NDJSON с done:true
		// и content из финального чанка. Если writeStreamingSSEDone тоже запишет
		// финальный done-чанк (например, через [DONE] маркер от SSE upstream)
		// клиент увидит ДУБЛИКАТ done-чанка с тем же content. Чтобы этого избежать
		// передаём флаг upstreamSentFinishReason — writeStreamingSSEDone пропустит
		// финальный NDJSON/SSE если upstream уже отправил finish_reason и нет tool_calls.
		st.upstreamHadFinishReason = true
	}
	// R60.49 (2026-09-12): передаём accumulatedPlainContent в translator —
	// при получении usage-чанка message.content/response теперь заполняется
	// накопленным content (а не ""), чтобы OpenWebUI показывал полный ответ.
	//
	// R60.55 (2026-09-13): передаём priorDoneEmitted — если предыдущий
	// чанк уже эмитил done:true (например finish_reason+content safety-net
	// от R51.3), usage чанк не должен эмитить второй done:true.
	//
	// H2 (2026-10-16): передаём и statsEmitted — usage-чанк подавляется
	// только если статистика УЖЕ ушла; иначе она отдаётся строкой с
	// done:false (см. translateOpenAISSEDataToOllamaWithFlags).
	// R118.20 (2026-10-15): фрагменты tool-call'ов копим и отдаём ОДИН раз
	// целиком — в Ollama-форме частичных аргументов не бывает (аудит C5).
	// R266.1 (2026-10-28): вычищаем служебные метки шаблона из ВИДИМОГО
	// content ДО перевода в NDJSON и до любых веток (обычный текст, чанк с
	// finish_reason, чанк после распознанного tool call). Клиенту уходит
	// результат перевода из этих данных, поэтому чистить надо данные, а не
	// локальную переменную: иначе часть веток остаётся без фильтра — именно
	// так `<|assistant|>` утёк после того, как tool call уже был распознан.
	if st.originalPath == "/api/chat" {
		data = filterSSEDeltaContentMarkers(data, &st.ndjsonHold)
	}

	if filtered, ok := st.toolAcc.filterToolCallDelta(data); !ok {
		return nil
	} else {
		data = filtered
	}
	// R493-12a: окна передаём четырьмя моментами — иначе в «время промпта»
	// снова попали бы очередь и загрузка, а размышления выпали бы из
	// «времени генерации» (см. stats_windows_r493.go).
	statsWindows := statsWindowsForTransportR493(st.requestStartTime, st.upstreamSentTime, st.firstDeltaTime, st.firstContentTime, time.Now())
	// R493-12a: загрузку модели знает воркер (она внутри запроса) — берём
	// её из заголовка ответа, иначе 40+ секунд не попадут никуда.
	statsWindows.WorkerLoad = st.workerLoadNsR493
	ollamaChunk := translateOpenAISSEDataToOllamaWindowsR493(st.originalPath, []byte(data), *st.modelFromCtx, &st.seenReasoning, statsWindows, st.accumulatedPlainContent, &st.priorDoneEmitted, &st.statsEmitted)
	if len(ollamaChunk) == 0 {
		return nil
	}
	// Round 53.1 (2026-08-24): detect usage chunk for writeStreamingSSEDone fallback logic.
	// translateOpenAISSEDataToOllama уже эмитил canonical done:true с полными статами
	// из usage чанка — writeStreamingSSEDone на [DONE] должен skip чтобы избежать
	// double-done. Способ детекта: usage чанк всегда имеет eval_count + done:true
	// (translateUsageChunkToOllama эмитит prompt_eval_count и eval_count).
	// Не-usage чанки с done:true (safety net для length/content_filter в одном
	// чанке с content) тоже считаем — но это OK, writeStreamingSSEDone skip безопасен.
	//
	// R60.55: трекаем priorDoneEmitted для ВСЕХ done:true чанков (не только
	// usage).
	//
	// H2 (2026-10-16): статистика и done:true учитываются РАЗДЕЛЬНО.
	//   - usageChunkSeen/priorDoneEmitted — по полю done (даже для stats-only
	//     строки с done:false priorDoneEmitted не меняется: сам факт того,
	//     что done:true уже ушёл, зафиксирован предыдущим чанком);
	//   - statsEmitted — по наличию eval_count/prompt_eval_count, независимо
	//     от done. Именно это гасит повторные usage-чанки, а не priorDoneEmitted.
	var ollamaParsed map[string]interface{}
	if err := json.Unmarshal(ollamaChunk[:len(ollamaChunk)-1], &ollamaParsed); err == nil {
		if done, _ := ollamaParsed["done"].(bool); done {
			// Любой done:true chunk = canonical done (translator эмитил).
			// writeStreamingSSEDone должен skip.
			st.usageChunkSeen = true
			st.priorDoneEmitted = true
		}
		if _, ok := ollamaParsed["eval_count"]; ok {
			// H2: статистика уехала клиенту (в done:true чанке или в
			// stats-only строке с done:false) — второй раз не отдаём.
			st.statsEmitted = true
		} else if _, ok := ollamaParsed["prompt_eval_count"]; ok {
			st.statsEmitted = true
		}
	}
	n, errFwd := st.w.Write(ollamaChunk)
	st.bytesForwarded.Add(int64(n))
	if errFwd != nil {
		st.lastForwardErr = errFwd
		if st.firstForwardErr == nil {
			st.firstForwardErr = errFwd
		}
		return fmt.Errorf("write NDJSON: %v", errFwd)
	}

	if flusher, ok := st.w.(http.Flusher); ok {
		flusher.Flush()
	}
	return nil
}

// finalizeStream — хвост стрима после цикла: придержанный текст (R266.1/R279),
// авто-продолжение обрыва (R60.21/R187), гарантированный терминатор (R231),
// диагностика обрыва (R296/R298/R384), лог ошибки сканера, сброс счётчика
// n_ctx и пустой done-кадр для Ollama-путей без единого кадра.
func (st *llamaCppStreamState) finalizeStream() error {
	st.flushHeldTail()
	if err := st.maybeAutoContinue(); err != nil {
		return err
	}

	// R231 (2026-10-24): ЗАВЕРШАЮЩИЙ ЧАНК ОБЯЗАН БЫТЬ ОТДАН ВСЕГДА.
	//
	// Живой дефект: emitDeferredDone вызывался ТОЛЬКО внутри ветки
	// `if streamCompleted && IsAutoContinueOnTruncationEnabled() && content != ""`.
	// При выключенном авто-продолжении (дефолт — env не задан) маркер `[DONE]`
	// от cppworker НЕ пересылался клиенту и синтетический done не эмитился:
	// поток завершался молча. Cline/OpenWebUI, не увидев конца стрима, ждали
	// или рвали сессию («символ конца стрима затесался/пропал»). Теперь
	// завершение эмитится безусловно (идемпотентно, ровно один раз).
	if st.streamCompleted && st.deferredDoneChunk {
		st.emitTerminalDoneOnly()
	}

	if err := st.reportPrematureEnd(); err != nil {
		return err
	}

	if err := st.scanner.Err(); err != nil {
		logger.Get().Errorw("proxyRequestLlamaCpp: scanner error", "error", err, "backend", st.backendID)
	}

	if st.p.nctxReload != nil {
		st.p.nctxReload.ResetCycleCounter(st.backendID)
	}

	if st.lastData == "" && st.originalPath != "/v1/chat/completions" && !st.terminalAlreadySent {
		done := buildDoneResponse(st.originalPath, *st.modelFromCtx, 0)
		_, errFwd := fmt.Fprintf(st.w, "%s\n", string(done))
		if errFwd != nil {
			return fmt.Errorf("write empty-done NDJSON: %v", errFwd)
		}
		if flusher, ok := st.w.(http.Flusher); ok {
			flusher.Flush()
		}
	}

	return nil
}

// flushHeldTail — R266.1/R279: придержанный хвост служебных меток, который
// оказался обычным текстом, обязан уйти клиенту, а не в накопитель.
//
// R266.1 (2026-10-28): отдаём придержанный хвост, если он оказался обычным
// текстом. Без этого ответ, закончившийся на `<` (начало метки, которая так и
// не пришла), терял бы последние байты — тот же класс дефекта, что чинили в
// R261 на другом пути.
//
// R279 (2026-10-28): ЭТОТ ХВОСТ ОБЯЗАН УЙТИ КЛИЕНТУ, А НЕ В НАКОПИТЕЛЬ.
//
// ЖИВОЙ ДЕФЕКТ: здесь стояло `accumulatedPlainContent += held` — то есть
// придержанные байты попадали в статистику и в детектор автопродолжения, а в
// поток НЕ уходили вовсе. Если хвостом оказывался обычный текст финального
// блока (а удержание держит любой суффикс, похожий на начало метки, включая
// просто «<» или «[» из кода), клиент этот текст не получал никогда.
// Теперь хвост пишется отдельным NDJSON-чанком (done:false) до терминального
// чанка: порядок «content → done» сохраняется, второй done клиент не получает.
func (st *llamaCppStreamState) flushHeldTail() {
	if st.originalPath != "/api/chat" && st.originalPath != "/api/generate" {
		return
	}
	held := st.ndjsonHold.Flush()
	if held == "" {
		return
	}
	if tail := heldTailChunk(st.originalPath, *st.modelFromCtx, held); tail != nil {
		n, errTail := fmt.Fprintf(st.w, "%s\n", tail)
		st.bytesForwarded.Add(int64(n))
		if errTail != nil {
			if st.firstForwardErr == nil {
				st.firstForwardErr = errTail
			}
			st.lastForwardErr = errTail
		} else {
			if flusher, ok := st.w.(http.Flusher); ok {
				flusher.Flush()
			}
			logger.Get().Infow("proxyRequestLlamaCpp[native]: emitted held tail to client (R279)",
				"backend", st.backendID, "path", st.originalPath, "held_len", len(held))
		}
	}
	// В накопитель тоже добавляем: он отражает то, что клиент уже видел,
	// и по нему работают детектор обрыва и автопродолжение.
	st.accumulatedPlainContent += held
}

// emitDeferredDone — отдать отложенный done-чанк (обычный успешный конец).
// R231: вызывается из нескольких веток (агентский ход, авто-продолжение,
// happy-path), поэтому идемпотентен: клиент обязан получить РОВНО ОДИН
// завершающий чанк/[DONE].
func (st *llamaCppStreamState) emitDeferredDone() {
	if st.doneEmitted {
		return
	}
	st.doneEmitted = true
	errFwd := writeStreamingSSEDone(st.w, st.originalPath, *st.modelFromCtx, st.toolAccum,
		st.accumulatedPlainContent, st.upstreamDoneContent, st.upstreamHadFinishReason, st.usageChunkSeen)
	if errFwd != nil {
		logger.Get().Warnw("proxyRequestLlamaCpp: deferred writeStreamingSSEDone error",
			"backend", st.backendID, "error", errFwd)
		st.lastForwardErr = errFwd
		if st.firstForwardErr == nil {
			st.firstForwardErr = errFwd
		}
	}
	if st.originalPath != "/v1/chat/completions" {
		Flush(st.w)
	}
}

// emitTerminalDoneOnly — R231: завершающий чанк БЕЗ повторного content.
//
// Нужен там, где дельты уже отданы клиенту, а отложенный done не был эмитчен
// (например, авто-продолжение подавлено как перегенерация). Повтор текста в
// done-чанке — это дубль у клиентов, склеивающих все чанки (живой флейк:
// исходный ответ приходил дважды). Настоящая Ollama в done-чанке отдаёт
// message.content="" — повторяем её контракт; tool_calls сохраняем.
func (st *llamaCppStreamState) emitTerminalDoneOnly() {
	if st.doneEmitted {
		return
	}
	// R51.3/R53.1: на NDJSON-путях usage-чанк уже отдал canonical done:true
	// (translateUsageChunkToOllama) — второй done клиент получит как дубль.
	if st.usageChunkSeen && st.originalPath != "/v1/chat/completions" {
		st.doneEmitted = true
		return
	}
	st.doneEmitted = true
	// R377 (2026-11-01): кадр собирает единственный владелец. Пустой content —
	// осознанное решение этого места: дельты клиенту уже отданы, повтор текста
	// в терминальном кадре даёт дубль у клиентов, склеивающих чанки.
	//
	// ДО R377 кадр собирался здесь руками и расходился с хелпером в трёх
	// местах: tool_calls уходили строкой аргументов (нарушение R295 — Ollama
	// ждёт объект), /api/generate получал поле message вместо response,
	// done_reason ставился "stop" даже без отданного текста.
	if errFwd := writeTerminalFrameR377(st.w, terminalFrameR377{
		path:      st.originalPath,
		model:     *st.modelFromCtx,
		toolCalls: st.toolAccum,
	}); errFwd != nil {
		logger.Get().Warnw("proxyRequestLlamaCpp: terminal frame write error",
			"backend", st.backendID, "path", st.originalPath, "error", errFwd)
		st.lastForwardErr = errFwd
		if st.firstForwardErr == nil {
			st.firstForwardErr = errFwd
		}
	}
	Flush(st.w)
}

// maybeAutoContinue — R60.21: доработка обрыва. Только там, где терминатор
// УДЕРЖАН (R322): после уже отданного клиенту завершения писать в поток нельзя.
//
// ==== R60.21 Auto-Continue on Truncation (2026-09-08) ====
// After the main streaming loop ends cleanly (streamCompleted=true),
// check if the accumulated content looks truncated. If so AND
// LB_AUTO_CONTINUE_ON_TRUNCATION=1, send a "continue" request to
// upstream and emit the continuation to client.
//
// Default: OFF (opt-in). See docs/api/env-vars.ru.md.
// For Qwen3-Instruct-2507-q4km which has a tendency to emit
// ChatML-EOS <|im_end|> mid-code (R60.20), this auto-recovers
// from unclosed code blocks and mid-line cutoffs.
//
// R60.34 (2026-09-10): guard against spurious fire on empty / failed
// responses. Previous behavior:
//   - TruncateReason("") → "empty" → fire R60.21
//   - empty content (e.g. n_ctx overflow, model error) was treated
//     as "truncated, continue!" → garbage request to upstream
//   - R60.33 async-load 503: handleChat returned 503, but
//     proxyRequestLlamaCpp also fired R60.21 in background → cascade
//     of unload+reload cycles
//
// Fix: skip R60.21 when:
//  1. accumulatedPlainContent is empty
//  2. streamCompleted but no chunks were received
//  3. upstream errored (any non-empty err in errMsg)
//  4. n_predict/finish_reason is missing or "length" was not the cause
func (st *llamaCppStreamState) maybeAutoContinue() error {
	// R187 (2026-10-20): АГЕНТСКИЙ ХОД НЕ ПРОДОЛЖЕН БЫТЬ ПРОДЛЁН.
	//
	// ЖИВОЙ РАЗБОР (Cline + Qwen3.8). В клиенте видно так: модель «завершает»
	// ответ (клиент показывает completed), затем появляются размышления, затем
	// поток встаёт и сессия не закрывается. Причина — здесь: эвристика «ответ
	// выглядит усечённым» (незакрытый блок кода, обрыв строки) срабатывала на
	// НОРМАЛЬНОМ конце агентского хода, и балансер:
	//   1) уже отдал клиенту finish_reason (клиент показал completed);
	//   2) тянул ВТОРОЙ запрос генерации (на 27B-модели — десятки секунд);
	//   3) дописывал продолжение ПОСЛЕ finish (клиенты его игнорируют) либо
	//      подавлял его по политике smart — и [DONE] приходил с большой
	//      задержкой или не приходил вовсе.
	// Агентский ход завершается ТЕМ, ЧЕМ ЗАВЕРШИЛСЯ: текстом, вызовом
	// инструмента или лимитом, который поставил сам клиент. Продолжать за
	// клиента нельзя: он ведёт диалог сам, а ходы модели в агентской сессии
	// почти всегда кончаются «незакрытой» строкой (это вызов инструмента, а не
	// обрыв) — то есть ложное срабатывание здесь штатное, а не исключение.
	agentTurn := !AutoContinueAllowedForTurn(countClientTools(st.bodyBuf) > 0, len(st.toolAccum))

	// R322 (2026-10-30, найдено аудитом маршрутов): после уже отданного клиенту
	// терминатора писать в поток НЕЛЬЗЯ.
	//
	// ЖИВОЙ ДЕФЕКТ. На нативном пути терминальный кадр воркера (`done:true`)
	// уходит клиенту как есть (passthrough), а блок авто-продолжения ниже всё
	// равно дорабатывал ответ: `EmitContinuationNDJSON` печатает ещё один
	// content-кадр и СВОЙ `done:true` — то есть после завершения хода в потоке
	// появлялся второй терминатор. Клиенты (Cline, OpenWebUI) такой поток
	// считают битым: часть закрывает ход на первом `done`, часть — на втором, и
	// «ответ не завершён» выглядит как незавершённая задача (жалоба про
	// COMPLETE), а часть сообщений теряется.
	//
	// Авто-продолжение имеет смысл только там, где терминальный кадр УДЕРЖАН
	// (deferredDoneChunk): тогда клиент ещё не получил завершения и продолжение
	// чинит обрыв. Если терминатор уже ушёл — доработка запрещена.
	if !(st.streamCompleted && IsAutoContinueOnTruncationEnabled() && st.accumulatedPlainContent != "" && !st.priorDoneEmitted) {
		return nil
	}
	finalContent := st.accumulatedPlainContent
	if st.upstreamDoneContent != "" {
		// If cppworker sent full content in the done chunk (not chunked),
		// use that as the source of truth.
		finalContent = st.upstreamDoneContent
	}
	if agentTurn {
		// Агентский ход: продолжаем только если клиент НЕ ограничил ответ и
		// модель не вызвала инструмент. Здесь ничего не доделываем.
		recordAutoContinueSuppressed()
		logger.Get().Infow(i18n.T("balancer.llamacpp_transport.auto_continue_agent_turn_skipped_info"),
			"backend", st.backendID, "model", *st.modelFromCtx,
			"api_path", st.originalPath,
			"tools_in_request", countClientTools(st.bodyBuf) > 0,
			"tool_calls_in_response", len(st.toolAccum),
			"content_chars", len(finalContent))
		if st.deferredDoneChunk {
			st.emitDeferredDone()
		}
		return nil
	}
	// R174 (2026-10-18, найдено сценариями клиентов): КЛИЕНТ САМ ОГРАНИЧИЛ
	// ОТВЕТ — продолжать нельзя.
	//
	// Живой замер матрицы клиентов: `num_predict: 6` на стриминговом
	// Ollama-пути → балансер считал обрыв «усечением» и слал второй запрос, воркер
	// видел промпт на 45 токенов больше прямого вызова, клиент получал ответ
	// длиннее запрошенного и платил за двойной промпт. Агентные клиенты (Cline,
	// deepseek harness, hermes) ставят max_tokens на КАЖДЫЙ запрос.
	if capped, capField := ClientOutputCapped(st.bodyBuf); capped {
		recordAutoContinueSuppressed()
		logger.Get().Infow(i18n.T("balancer.llamacpp_transport.auto_continue_client_capped_info"),
			"backend", st.backendID, "model", *st.modelFromCtx,
			"api_path", st.originalPath, "cap_field", capField,
			"content_chars", len(finalContent))
		return nil
	}
	if blockedCtx, ctxErrText := autoContinueBlockedByContext(st.upstreamErrorText, st.upstreamDoneContent, finalContent); blockedCtx {
		// R289 (2026-10-29): это НЕ обрыв, а отказ по размеру контекста.
		// Живой дефект: при окне 4096 и промпте 3791 апстрим отвечал 413
		// `prompt too long`, эвристика видела «незакрытую строку» и
		// запускала продолжение — второй запрос тоже получал 413, а клиент
		// так и не узнал настоящую причину.
		recordAutoContinueSuppressed()
		windowTokens := st.p.loadedPerSeqNCtx(st.backendID, *st.modelFromCtx)
		writeContextTooSmallNotice(st.w, st.originalPath, *st.modelFromCtx,
			st.p.calibratedEstimateOnly(st.bodyBuf, st.originalPath), windowTokens)
		logger.Get().Infow("auto-continue suppressed: context overflow is not a truncation (R289)",
			"backend", st.backendID, "model", *st.modelFromCtx,
			"api_path", st.originalPath, "upstream_error", ctxErrText)
		if st.deferredDoneChunk {
			st.emitTerminalDoneOnly()
		}
		return nil
	}
	if reason := TruncateReason(finalContent); reason != "" {
		return st.continueTruncatedResponse(finalContent, reason)
	}
	if st.deferredDoneChunk && st.streamCompleted {
		// R60.55 (2026-09-13): no truncation OR auto-continue didn't fire
		// (e.g., continuation was suppressed as regen). Emit the deferred
		// done-chunk now with accumulated content. This is the standard
		// happy-path case where the model completed normally.
		st.emitDeferredDone()
	}
	return nil
}

// continueTruncatedResponse — R60.21: запросить продолжение у воркера и отдать
// клиенту ОДИН связный ответ (без двух done-кадров).
func (st *llamaCppStreamState) continueTruncatedResponse(finalContent, reason string) error {
	logger.Get().Infow("proxyRequestLlamaCpp: R60.21 detected truncation, auto-continuing",
		"backend", st.backendID, "model", *st.modelFromCtx,
		"api_path", st.originalPath,
		"reason", reason,
		"content_chars", len(finalContent))
	// R60.25: use env-configured timeout (default 10 min, was
	// hardcoded 60s which timed out on long code generation).
	// The 60s default was the failure mode for "model emits
	// unclosed code block, auto-continue triggers, but the
	// continue request itself hits the same time limit and
	// times out before the model finishes the continuation".
	autoContTimeout := GetAutoContinueTimeout()
	continuation, contEval, contErr := PerformAutoContinue(
		st.fullURL, st.translatedBody, finalContent, reason,
		&http.Client{Timeout: autoContTimeout}, autoContTimeout)
	if contErr != nil || continuation == "" {
		recordAutoContinueFailed()
		logger.Get().Warnw("proxyRequestLlamaCpp: R60.21 auto-continue failed (emitting truncated as-is)",
			"backend", st.backendID, "model", *st.modelFromCtx,
			"reason", reason, "error", contErr,
			"url", st.fullURL)
		return nil
	}
	// R83 (2026-09-25, D3): политика != smart ОТКЛЮЧАЕТ подавление
	// перегенерации, то есть при `always` дубли ответа гарантированы.
	// Раньше это было видно только по отсутствию строки «suppressing
	// duplicate» в логе — теперь есть явное предупреждение и счётчик,
	// который уходит в /api/v1/metrics (см. auto_continue_stats.go).
	if policy := GetAutoContinueChatPolicy(); policy != "smart" {
		recordAutoContinuePolicyDisabled()
		logger.Get().Warnw(i18n.T("balancer.llamacpp_transport.regeneration_suppression_disabled_warn"),
			"backend", st.backendID, "model", *st.modelFromCtx,
			"api_path", st.originalPath, "policy", policy,
			"hint", i18n.T("balancer.llamacpp_transport.regeneration_suppression_hint"))
	}
	// R60.55 (2026-09-13): if "smart" chat-policy and continuation
	// looks like a regeneration (model restarted with greeting),
	// SUPPRESS the continuation. Sending it would create a duplicate
	// response in OpenWebUI.
	if GetAutoContinueChatPolicy() == "smart" && IsContinuationARegeneration(finalContent, continuation) {
		recordAutoContinueSuppressed()
		logger.Get().Warnw("proxyRequestLlamaCpp: R60.55 continuation looks like regeneration (chat model antipattern); suppressing duplicate",
			"backend", st.backendID, "model", *st.modelFromCtx,
			"api_path", st.originalPath,
			"policy", "smart",
			"original_chars", len(finalContent),
			"continuation_chars", len(continuation))
		// Don't emit continuation. Client already received done-chunk
		// with truncated original content. Set flag to skip downstream
		// truncation alert (we did our best).
		st.streamCompleted = true
		return nil
	}
	recordAutoContinueEmitted()
	logger.Get().Infow("proxyRequestLlamaCpp: R60.21 auto-continue succeeded",
		"backend", st.backendID, "model", *st.modelFromCtx,
		"api_path", st.originalPath,
		"continuation_chars", len(continuation),
		"cont_eval", contEval)
	// Emit continuation as a single final done-chunk with
	// combined content. This is the safest approach — the client
	// gets one coherent response (no double done-chunks).
	var flusher http.Flusher
	if f, ok := st.w.(http.Flusher); ok {
		flusher = f
	}
	totalEval := 0
	if contEval > 0 {
		totalEval = contEval
	}
	emitErr := EmitContinuationNDJSON(
		st.w, st.originalPath, *st.modelFromCtx,
		finalContent, continuation, totalEval, flusher)
	if emitErr != nil {
		logger.Get().Warnw("proxyRequestLlamaCpp: R60.21 emit continuation error",
			"backend", st.backendID, "error", emitErr)
	}
	// Mark stream as completed so downstream truncation
	// detection doesn't fire.
	st.streamCompleted = true
	// R231: если continuation реально отдана, терминальный чанк
	// уже ушёл клиенту (combined content + done). Отложенный
	// done больше не нужен — иначе в конце сработает
	// безусловная эмиссия и клиент получит ДУБЛЬ исходного
	// текста (живой флейк TestAutoContinue_Regeneration...).
	if emitErr == nil {
		st.deferredDoneChunk = false
	}
	return nil
}

// reportPrematureEnd — R384: диагностика и кадры-причины, когда поток кончился
// до терминального маркера. Возвращает ошибку только в одном случае — клиент
// ушёл сам (`client_cancel`): писать в его соединение бессмысленно.
//
// ==== Stream-truncation detection (2026-06-26) ====
// Если мы вышли из цикла for без streamCompleted (т.е. cppworker оборвал
// стрим до того, как прислал финальный [DONE] маркер), явно логируем это
// и эмитим клиенту финальный чанк с error/done:true, чтобы клиент
// (Cline/OpenWebUI/Roo Code) корректно отобразил сбой вместо "успешного
// ответа" с обрезанным content.
//
// R384 (2026-11-01): признак «терминатор уже отправлен кадрами-причиной».
//
// ЖИВОЙ ДЕФЕКТ (стенд, сторож тишины): апстрим прислал заголовки и замолчал;
// сторож закрыл тело, abortFramesR296 отдал кадр-причину (upstream_stall) и
// терминатор `{"done":true,"done_reason":"error"}`, а затем исполнение
// доходило до блока «lastData == ""» в конце функции и писало ВТОРОЙ
// терминатор — `{"done":true,"done_reason":"stop"}`. Клиент видел два `done`:
// один с ошибкой, второй как успешное завершение.
func (st *llamaCppStreamState) reportPrematureEnd() error {
	if st.streamCompleted {
		return nil
	}
	// ==== Stream-truncation detection (2026-06-26) ====
	// Сценарии:
	//   1. scanner.Err() != nil → broken pipe / context canceled / timeout
	//   2. scanner.Err() == nil && streamCompleted == false → cppworker
	//      закрыл TCP без [DONE] (редкий случай, обычно после ошибки модели).
	//
	// В обоих случаях клиент уже получил HTTP 200 + Content-Type, поэтому
	// поменять HTTP-статус нельзя. Лучшее, что мы можем — дописать в стрим
	// финальный маркер и закрыть соединение, чтобы клиент не висел.
	scannerErr := st.scanner.Err()
	// R259: КТО завершил стрим. Наш лимит строки и наши отмены — НЕ
	// «upstream truncated»; клиенту и в лог уходит правдивая причина.
	abortReason := streamAbortReasonR259(scannerErr, st.r.Context().Err())
	// R298: если поток разорвал СТОРОЖ тишины, причина — не «ошибка чтения»
	// (её мы сами и создали, закрыв тело), а именно зависание upstream.
	// Проверяем ДО эвристики R271: сторож — наш факт, а не догадка.
	stallEventR298, stalledR298 := st.stallWatch.Fired()
	if stalledR298 {
		abortReason = streamAbortUpstreamStall
		logger.Get().Warnw("proxyRequestLlamaCpp: stream aborted by the stall watchdog (R298)",
			"backend", st.backendID, "model", *st.modelFromCtx,
			"prefill", stallEventR298.prefill,
			"stalled_sec", int(stallEventR298.stalled.Seconds()),
			"original_path", st.originalPath,
			"bytes_forwarded", st.bytesForwarded.Load(),
			"elapsed_sec", time.Since(st.llamaStartTime).Seconds())
	}
	// R271 (2026-10-29): если незадолго до обрыва МЫ выгружали/перезагружали
	// эту модель на этом бэкенде — виноват не upstream.
	//
	// Живой дефект: второй клиент запросил другое окно n_ctx, балансер
	// перезагрузил модель, поток первого закрылся без маркера — и клиент
	// получил «stream ended by upstream before completion», хотя воркер был
	// жив. Теперь причина называется своим именем.
	if streamAbortBlamesUpstream(abortReason) {
		if kind, ok := st.p.recentDestructiveOpR271(st.backendID, *st.modelFromCtx, 3*time.Minute); ok {
			// R280: причина зависит от ВИДА операции (reload по чужому окну /
			// AutoTune / unload) — таблица одна, в active_streams_r271.go.
			abortReason = abortReasonForDestructiveOpR271(kind)
			logger.Get().Warnw("proxyRequestLlamaCpp: stream ended because the balancer reloaded the model",
				"reason", abortReason, "cause", kind,
				"backend", st.backendID, "model", *st.modelFromCtx,
				"original_path", st.originalPath,
				"bytes_forwarded", st.bytesForwarded.Load(),
				"elapsed_sec", time.Since(st.llamaStartTime).Seconds())
		}
	}
	logFields := []interface{}{
		"reason", abortReason,
		"backend", st.backendID, "model", *st.modelFromCtx,
		"original_path", st.originalPath,
		"bytes_forwarded", st.bytesForwarded.Load(),
		"elapsed_sec", time.Since(st.llamaStartTime).Seconds(),
		"scanner_err", errString(scannerErr),
		"first_forward_err", errString(st.firstForwardErr),
		"last_forward_err", errString(st.lastForwardErr),
	}
	if abortReason == streamAbortOurLimit {
		logFields = append(logFields, "line_limit_bytes", streamMaxLineBytes())
		logger.Get().Warnw("proxyRequestLlamaCpp: SSE line exceeded balancer buffer (our_limit, not upstream)",
			logFields...)
	} else if abortReason == streamAbortClientCancel {
		// Клиент ушёл: писать в его соединение бессмысленно.
		logger.Get().Infow("proxyRequestLlamaCpp: stream ended by client (client_cancel)", logFields...)
		return st.lastForwardErr
	} else {
		logger.Get().Warnw("proxyRequestLlamaCpp: stream ended before [DONE] marker",
			logFields...)
	}

	// Эмитим финальный чанк, чтобы клиент точно завершил стрим.
	// Пробуем дописать; если клиент уже отвалился (broken pipe) — игнорируем ошибку.
	// R296 (2026-10-29): КОНТРАКТ ТЕРМИНАТОРА. Раньше причина обрыва и
	// терминатор ехали ОДНИМ кадром (`finish_reason:"truncated"` + `error`).
	// Клиент (Cline) видел `finish_reason` — то есть «ответ завершён» — и не
	// успевал прочитать причину: выглядело как успешный, но пустой финал.
	// Теперь сначала кадр-ПРИЧИНА (без finish_reason, то есть поток жив),
	// затем ровно один терминатор. Кадры собирает abortFramesR296 (там же
	// тестируются по отдельности).
	for _, frame := range abortFramesR296(st.originalPath, *st.modelFromCtx, abortReason) {
		_, _ = st.w.Write(frame)
	}
	// R384: терминатор уже в этих кадрах — второй писать нельзя.
	st.terminalAlreadySent = true
	if st.originalPath == "/v1/chat/completions" {
		_, _ = fmt.Fprintf(st.w, "data: [DONE]\n\n")
	}
	Flush(st.w)
	return nil
}

// ============================================================================
// R540 (R2 читаемости): подготовка, preflight и отправка запроса к воркеру.
//
// После R539 coordinator всё ещё держал 483 строки: кроме вызовов стадий в нём
// жили подготовка (бэкенд, имя модели, путь/тело), preflight окна, отправка с
// ретраями и прозрачная ленивая загрузка. Здесь они разложены по стадиям,
// названным в плане (docs/plans/2026-11-19-readability-refactor.md, §R2):
//
//	prepareUpstreamCall    — что решено ДО запроса (prepare)
//	awaitAdmissionAndLoad  — preflight n_ctx и учёт нагрузки (preflight)
//	prepareUpstreamRequest — URL, клиент с таймаутами, контекст, сам запрос
//	sendRequest            — отправка и ретраи (send)
//	awaitLoadedModel       — прозрачная ленивая загрузка (503 + polling)
//	writeStreamHeaders     — заголовки ответа до WriteHeader(200)
//
// Состояние стадии — upstreamCall: не копия локальных переменных, а их
// владелец; указатель живёт до конца запроса, потому что имя модели уточняется
// в цикле стрима (R422), а `defer` AutoTune читает его после возврата.
// Поведение перенесено дословно: порядок проверок, тексты и заголовки прежние.
// ============================================================================

// upstreamCall — решение о запросе: бэкенд, имя модели, путь/тело, клиент,
// контекст и сам запрос. Порядок полей — по fieldalignment.
type upstreamCall struct {
	// Моменты времени (R493-12a): старт прохода, приём запроса, отправка воркеру.
	llamaStartTime   time.Time
	requestStartTime time.Time
	upstreamSentTime time.Time

	// Тела: клиентское и переведённое для воркера.
	bodyBuf        []byte
	translatedBody []byte

	// Что решено до запроса.
	backendID    string
	targetURL    string
	originalPath string
	modelFromCtx string
	fullURL      string
	llamacppPath string
	clientRealIP string

	// Поверхности ввода-вывода и общий контекст запроса.
	w              http.ResponseWriter
	reqCtx         context.Context
	modelCtxAtomic atomic.Value

	// Бэкенд, запрос, ответ, клиент и схемы инструментов.
	p            *Proxy
	r            *http.Request
	state        *BackendState
	req          *http.Request
	resp         *http.Response
	clientForReq *http.Client
	toolSchemas  map[string]*toolParamSchema
	modelNameNow func() string

	// Счётчик попыток (1 = первая), число EOF-повторов и флаги стадии.
	llamaAttempt    int
	llamaEOFRetries int
	nativePath      bool
	isStreaming     bool
}

// prepareUpstreamCall — стадия prepare: бэкенд и его адрес, имя модели из
// контекста (+ atomic и функция для сторожей), схемы инструментов, нативный
// путь и тело для воркера.
//
// Возвращает (nil, nil), когда ответ уже отправлен отказом по семейству модели
// (R268): координатор в этом случае просто завершает ход.
func (p *Proxy) prepareUpstreamCall(w http.ResponseWriter, r *http.Request, backendID string, bodyBuf []byte) (*upstreamCall, error) {
	state, ok := p.backends[backendID]
	if !ok {
		return nil, fmt.Errorf("backend %s not found", backendID)
	}

	c := &upstreamCall{
		p:            p,
		w:            w,
		r:            r,
		backendID:    backendID,
		state:        state,
		targetURL:    p.getBackendBaseURL(state.Backend),
		originalPath: r.URL.Path,
		bodyBuf:      bodyBuf,
		llamaAttempt: 1,
	}
	if m, ok := r.Context().Value(modelContextKey).(string); ok {
		c.modelFromCtx = m
	}
	// R422 (2026-11-03): имя модели узнаётся из первого содержательного чанка
	// (в цикле стрима) и читается ещё и сторожем/onStall-горутинами. Голый
	// string — гонка (поймана -race на TestR298): пишем в основной горутине,
	// читаем в watcher. Держим отдельное atomic-значение; основной поток
	// продолжает пользоваться полем структуры без накладных расходов.
	initialModelName := c.modelFromCtx
	if c.modelFromCtx != "" {
		c.modelCtxAtomic.Store(c.modelFromCtx)
	}
	c.modelNameNow = func() string {
		if v, ok := c.modelCtxAtomic.Load().(string); ok && v != "" {
			return v
		}
		return initialModelName
	}

	// R186 (2026-10-20): схемы инструментов из ЗАПРОСА. Нужны, чтобы привести
	// аргументы вызова, который балансер сам собрал из текста модели (ниже,
	// extractToolCallsFromSSEContent): без этого клиент (Cline) отвергал вызов
	// «expected array, received string» и повторял его до остановки по циклу.
	c.toolSchemas = toolSchemasFromBody(bodyBuf)

	// R65d (2026-09-20): Ollama-пути идут в cppworker НАТИВНО (без трансляции
	// в OpenAI-формат). См. llamacpp_native_path.go для полного обоснования:
	// транслятор терял options.* / keep_alive / format / think / images, а
	// перенесённые поля без пары в cppworker-структурах (например top_k)
	// отвергались строгим декодером с HTTP 400.
	//
	// При нативном пути:
	//   - путь НЕ переписывается (остаётся /api/chat, а не /v1/chat/completions);
	//   - тело НЕ транслируется (options.* остаются на месте);
	//   - ответ уже приходит в Ollama-формате (NDJSON), поэтому passthrough-ветка
	//     цикла отдаёт строки клиенту как есть — без SSE→NDJSON перевода.
	c.nativePath = shouldRouteOllamaNative(c.originalPath)

	// R268 (2026-10-28): семейство модели решает, умеем ли мы вообще ПРЕВРАТИТЬ
	// ответ в Ollama API. На passthrough-путях (нативный Ollama-путь, /v1/*)
	// преобразования нет — ответ идёт как есть, и отказ там запрещён.
	//
	// Отказ ставится ДО запроса к модели: незачем занимать слот и минуты
	// генерации ради ответа, который мы не сможем собрать. Текст отказа называет
	// семейство, версию, поддержанный диапазон и порт OpenAI-passthrough —
	// оператор должен понять причину и что делать, а не увидеть «ошибка».
	hasTools := len(c.toolSchemas) > 0
	// R532 (R2 читаемости): отказ по семейству модели и лог решения вынесены.
	if p.refuseUnsupportedFamilyAndLog(w, c.originalPath, c.modelFromCtx, hasTools, c.nativePath) {
		return nil, nil
	}

	// R533 (R2 читаемости): путь и тело для cppworker — одной функцией.
	c.llamacppPath, c.isStreaming, c.translatedBody = makeUpstreamPathAndBody(c.originalPath, c.nativePath, c.bodyBuf)

	// F.0.4 (2026-06-28): session F — диагностика EOF.
	// Запоминаем startTime для расчёта duration_ms при ошибках upstream.
	c.llamaStartTime = time.Now()
	// R493-12a: если вход запроса (Proxy.ServeHTTP) отметил момент приёма — считаем
	// от него. llamaStartTime ставится уже ПОСЛЕ выбора бэкенда, авто-загрузки и
	// ожидания слота, поэтому «сколько клиент ждал» из него не видно вовсе
	// (живой замер: 44 с ожидания при total_duration 2.55 с).
	c.requestStartTime = requestStartR493(r.Context())
	if c.requestStartTime.IsZero() {
		c.requestStartTime = c.llamaStartTime
	}
	// R493-12a (2026-11-12): окна времени для статистики скорости токенов.
	//
	// ДО ЭТОЙ ПРАВКИ: prompt_eval_duration считался от llamaStartTime, то есть
	// включал очередь, авто-загрузку и synchronous preflight n_ctx (на 27B —
	// минуты), а первый контент-чанк не двигался reasoning-дельтами, поэтому
	// время размышлений тоже уходило в «время промпта», хотя их токены входили
	// в eval_count. Агент получал 21 t/s вместо ~400 и 104 t/s вместо ~8.
	//
	// ТЕПЕРЬ: upstreamSentTime — момент реальной отправки запроса воркеру (после
	// очереди/загрузки/preflight); firstDeltaTime (поле состояния стрима) —
	// первый дельта-чанк ЛЮБОГО вида (content, reasoning/thinking, tool_calls).
	// Разбор окна: docs/architecture/stats-and-context-check.ru.md,
	// код: stats_windows_r493.go.
	return c, nil
}

// awaitAdmissionAndLoad — стадия preflight: окно n_ctx (R447/R479) и учёт
// num_ctx запроса для будущих решений AutoTune (R54.9). true — ответ клиенту
// уже отправлен (413/404/503 из вердикта preflight).
func (c *upstreamCall) awaitAdmissionAndLoad() bool {
	// PREFLIGHT n_ctx auto-reload: если клиент (OpenWebUI) задал options.num_ctx,
	// а loaded n_ctx на бэкенде меньше — синхронно перезагружаем модель на нужный n_ctx
	// ДО проксирования. Это критично для OpenWebUI с tools: первый запрос
	// получает reload один раз (10-30 сек), последующие — instant response.
	// R447: слот под числа вердикта R301/R385 — writer тела (ниже) обязан
	// написать те же числа, что названы в тексте причины.
	// R479: стадия preflight — видно, что ход застрял на решении об окне.
	SetStageR477(c.r.Context(), stagePreflightR477)
	preflightCtx, _ := growthNumbersSlotR447(c.r.Context())
	preflightBody, preflightOK, preflightMsg, preflightStatus := c.p.preflightNCtxReloadIfNeeded(
		preflightCtx, c.backendID, c.modelFromCtx, c.bodyBuf, c.originalPath)
	// R530 (R2 читаемости): применение вердикта preflight вынесено в
	// applyPreflightOutcome — там ответ клиенту (413/404/503) и правка тела
	// запроса. handled=true означает, что ответ уже отправлен.
	bodyBuf, translatedBody, preflightHandled := c.p.applyPreflightOutcome(
		c.w, preflightCtx, c.backendID, c.modelFromCtx, c.originalPath, c.nativePath,
		preflightBody, preflightOK, preflightMsg, preflightStatus, c.bodyBuf, c.translatedBody)
	if preflightHandled {
		return true
	}
	c.bodyBuf, c.translatedBody = bodyBuf, translatedBody

	// R54.9 (2026-08-24): Workload tracking — record num_ctx from request body
	// for future AutoTune decisions. Неблокирующий, in-memory, sliding window.
	// Использует existing ExtractNumCtxFromBody (num_ctx_resolver.go) — уже
	// покрывает Ollama options.num_ctx + OpenAI top-level + JSON float decoding.
	if c.modelFromCtx != "" && c.p.WorkloadTracker() != nil {
		if numCtx := ExtractNumCtxFromBody(c.bodyBuf); numCtx > 0 {
			c.p.WorkloadTracker().Record(c.backendID, c.modelFromCtx, numCtx)
		}
	}
	return false
}

// prepareUpstreamRequest — URL для воркера, клиент с таймаутами, контекст и сам
// запрос. Возвращает cancel контекста: `defer` обязан выполнить вызывающий,
// иначе per-model таймаут стрима снимется сразу после создания запроса.
func (c *upstreamCall) prepareUpstreamRequest() (context.CancelFunc, error) {
	// R534 (R2 читаемости): URL и перенос name в query для /api/models/unload.
	c.fullURL, c.translatedBody = buildUpstreamURLAndBody(c.targetURL, c.llamacppPath, c.originalPath, c.backendID, c.translatedBody)
	logger.Get().Infow("proxyRequestLlamaCpp: sending request",
		"backend", c.backendID, "url", c.fullURL, "stream", c.isStreaming)

	// R537 (R2 читаемости): клиент запроса и его таймауты (per-model stream
	// timeout, FirstByteTimeout) вынесены в upstreamHTTPClientFor и
	// streamRequestContext.
	c.clientForReq = c.p.upstreamHTTPClientFor(c.isStreaming, c.modelFromCtx)

	reqCtx, cancelReq := c.p.streamRequestContext(c.r, c.isStreaming, c.backendID, c.modelFromCtx)
	c.reqCtx = reqCtx

	// R537 (R2 читаемости): запрос к воркеру и его заголовки — одной функцией.
	req, clientRealIP, err := c.p.newUpstreamRequest(reqCtx, c.r, c.fullURL, c.translatedBody, c.llamacppPath, c.backendID)
	if err != nil {
		cancelReq()
		return nil, fmt.Errorf("failed to create request: %v", err)
	}
	c.req = req
	c.clientRealIP = clientRealIP
	return cancelReq, nil
}

// sendRequest — стадия send: отправка запроса воркеру и ретраи ДО записи
// заголовков клиенту (connection-level с backoff и один EOF-повтор).
func (c *upstreamCall) sendRequest() error {
	// F.0.4 (2026-06-28): session F — EOF retry перед записью заголовков клиенту.
	//
	// Проблема: cppworker может вернуть EOF (закрытое TCP-соединение без ответа) в
	// редких ситуациях — например, когда модель ещё загружается (lazy load) или
	// произошёл panic в middleware ДО хендлера. В этом случае клиент видит EOF без
	// какой-либо диагностики.
	//
	// Решение: если получена EOF-ошибка ДО WriteHeader(200), пробуем один раз тот же
	// backend через 500ms (типичный случай — cppworker перезагружал модель ровно
	// во время первого запроса). После retry возвращаем расширенный error context
	// с backend_id, attempt и duration_ms для диагностики.
	//
	// 2026-07-01: добавляем waitForBackendModelLoaded retry при получении
	// HTTP 503 "model is loading" от cppworker. Lazy load теперь прозрачен для
	// клиента (OpenWebUI/Cline) — раньше стрим обрывался на первом запросе.
	//
	// После записи заголовков retry невозможен — TCP уже начат.
	// R493-12a: фиксируем момент отправки воркеру — с него начинается префилл,
	// и только он попадает в prompt_eval_duration. Все ожидания до этой строки
	// (очередь, авто-загрузка, preflight reload) уходят в load_duration.
	if c.upstreamSentTime.IsZero() {
		c.upstreamSentTime = time.Now()
	}
	resp, err := c.clientForReq.Do(c.req)
	// Round 8 (2026-07-10): connection-level error retry with exponential backoff
	// (только для non-streaming — для streaming headers ещё не записаны,
	// поэтому retry безопасен ДО первой записи в ResponseWriter).
	//
	// cppworker может быть в transition window (docker recreate, network blip):
	//   - connection refused (port 18092 ещё не listening)
	//   - connection reset (контейнер завершает keep-alive)
	//
	// 3 retry с backoff 1s/2s/4s перед сдачей. Не применяем к streaming после
	// записи headers (этот код срабатывает до WriteHeader, проверяется через
	// isStreaming + content-type).
	if err != nil && isConnectionLevelError(err) {
		maxConnectionRetries := 3
		connectionBackoffs := []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second}
		for retryIdx := 0; retryIdx < maxConnectionRetries; retryIdx++ {
			backoff := connectionBackoffs[retryIdx]
			logger.Get().Warnw("proxyRequestLlamaCpp: connection-level error, backing off before retry",
				"backend", c.backendID,
				"model", c.modelFromCtx,
				"attempt", c.llamaAttempt,
				"retry_idx", retryIdx+1,
				"max_retries", maxConnectionRetries,
				"backoff_ms", backoff.Milliseconds(),
				"error", err)
			select {
			case <-time.After(backoff):
			case <-c.reqCtx.Done():
				durationMs := time.Since(c.llamaStartTime).Milliseconds()
				return fmt.Errorf("llama.cpp request failed [backend=%s, attempt=%d, duration_ms=%d, error_type=%s, cancelled_during_backoff]: %v",
					c.backendID, c.llamaAttempt, durationMs, determineErrorType(err, c.reqCtx), err)
			}
			c.llamaAttempt++
			resp, err = c.clientForReq.Do(c.req)
			if err == nil {
				break // success
			}
			if !isConnectionLevelError(err) {
				break // error type changed, exit retry loop
			}
		}
	}
	// EOF-retry counter (1 = первая попытка). Используется в error context и для
	// retry на тот же бэкенд после 500ms (типичный случай — cppworker перезагружал
	// модель ровно во время первого запроса).
	const maxLlamaEOFRetries = 1
	if err != nil && c.llamaEOFRetries < maxLlamaEOFRetries {
		errType := determineErrorType(err, c.reqCtx)
		if errType == "unexpected_eof" {
			logger.Get().Warnw("proxyRequestLlamaCpp: EOF from upstream, retrying once on same backend",
				"backend", c.backendID, "model", c.modelFromCtx,
				"attempt", c.llamaAttempt, "error", err)
			// Публикуем EOF event в EventPublisher (если подключён).
			c.p.publishTransportEOF(c.backendID, c.modelFromCtx, c.originalPath, err, time.Since(c.llamaStartTime))
			// Задержка 500ms перед retry — типичное время reload-операций cppworker.
			time.Sleep(c.p.getStreamingRetryDelay())
			c.llamaEOFRetries++
			c.llamaAttempt++
			resp, err = c.clientForReq.Do(c.req)
		}
	}
	if err != nil {
		durationMs := time.Since(c.llamaStartTime).Milliseconds()
		errType := determineErrorType(err, c.reqCtx)
		logger.Get().Errorw("proxyRequestLlamaCpp: upstream Do() failed",
			"backend", c.backendID,
			"model", c.modelFromCtx,
			"url", c.fullURL,
			"attempt", c.llamaAttempt,
			"duration_ms", durationMs,
			"error_type", errType,
			"is_streaming", c.isStreaming,
			"error", err)
		// Публикуем EOF event (если это EOF и мы ещё не публиковали).
		if errType == "unexpected_eof" {
			c.p.publishTransportEOF(c.backendID, c.modelFromCtx, c.originalPath, err, time.Duration(durationMs)*time.Millisecond)
		}
		// Round 8 (2026-07-10): persistent connection_refused → mark backend unhealthy.
		if errType == "connection_refused" {
			c.p.markBackendConnectionFailed(c.backendID)
		}
		return fmt.Errorf("llama.cpp request failed [backend=%s, attempt=%d, duration_ms=%d, error_type=%s]: %v",
			c.backendID, c.llamaAttempt, durationMs, errType, err)
	}
	c.resp = resp
	return nil
}

// awaitLoadedModel — R493/2026-07-01: прозрачная ленивая загрузка. cppworker на
// первый inference-запрос к незагруженной модели отвечает HTTP 503 с телом
// {"error":"model is loading: <name>","loading":true,...}. true — ответ клиенту
// уже отправлен (polling исчерпан, отдан 503).
func (c *upstreamCall) awaitLoadedModel() (bool, error) {
	// 2026-07-01: transparent lazy-load retry (см. loading_retry.go).
	//
	// Без retry OpenWebUI/Cline видел оборванный стрим. Теперь:
	//   1. Делаем 503 response body readable (нужно для анализа сигнала).
	//   2. Если 503 + loading — polling /api/models/load/progress до 30 сек.
	//   3. После успешного polling повторяем clientForReq.Do(req) с тем же body.
	// Retry возможен ТОЛЬКО до записи заголовков клиенту — что и делаем здесь.
	if c.resp.StatusCode != http.StatusServiceUnavailable {
		return false, nil
	}
	loadingBody, readErr := io.ReadAll(c.resp.Body)
	c.resp.Body.Close()
	if readErr != nil {
		loadingBody = []byte(fmt.Sprintf(`{"error":"upstream read error: %v"}`, readErr))
	}
	isLoading, loadingModelName, _ := loadingSignalFromBody(c.resp.StatusCode, loadingBody)
	if !isLoading {
		// 503 без признака loading — восстанавливаем body и идём по обычному пути.
		// respBody был прочитан в loadingBody выше, проксируем как upstream error.
		c.resp = &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Body:       io.NopCloser(bytes.NewReader(loadingBody)),
			Header:     make(http.Header),
		}
		return false, nil
	}
	// Если из body не извлекли имя модели — используем из контекста.
	if loadingModelName == "" {
		loadingModelName = c.modelFromCtx
	}
	logger.Get().Infow("proxyRequestLlamaCpp: 503 'model is loading', polling for load completion",
		"backend", c.backendID, "model", loadingModelName,
		"attempt", c.llamaAttempt, "duration_ms", time.Since(c.llamaStartTime).Milliseconds())

	loaded, waitErr := c.p.waitForBackendModelLoaded(c.reqCtx, c.state.Backend, loadingModelName, c.backendID)
	if waitErr != nil {
		logger.Get().Warnw("proxyRequestLlamaCpp: waitForBackendModelLoaded error",
			"backend", c.backendID, "model", loadingModelName, "error", waitErr)
	}
	if !loaded {
		// Polling исчерпан — отдаём 503 клиенту с понятным сообщением.
		logger.Get().Warnw("proxyRequestLlamaCpp: load wait timeout, returning 503 to client",
			"backend", c.backendID, "model", loadingModelName)
		c.w.Header().Set("Content-Type", "application/json")
		c.w.Header().Set("Retry-After", "5")
		c.w.Header().Set("X-Model-Loading-Retry", "exhausted")
		errBody, _ := json.Marshal(map[string]interface{}{
			"error":        fmt.Sprintf("model is still loading after %ds: %s", c.p.loadingRetryBudgetSeconds(loadingModelName), loadingModelName),
			"loading":      true,
			"model":        loadingModelName,
			"retryAfterMs": 5000,
		})
		c.w.Header().Set("Content-Length", fmt.Sprintf("%d", len(errBody)))
		c.w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = c.w.Write(errBody)
		return true, nil
	}

	c.llamaAttempt++
	// Пересоздаём req: тело уже могло быть прочитано в io.ReadAll выше,
	// и Body указывает на закрытый reader. bytes.NewReader(translatedBody) снова.
	req2, reqErr := http.NewRequestWithContext(c.reqCtx, c.r.Method, c.fullURL, bytes.NewReader(c.translatedBody))
	if reqErr != nil {
		return false, fmt.Errorf("failed to create retry request: %v", reqErr)
	}
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Accept", "application/json, text/event-stream")
	for key, values := range c.r.Header {
		lowerKey := strings.ToLower(key)
		if lowerKey == "content-type" || lowerKey == "accept" || lowerKey == "content-length" || lowerKey == "host" {
			continue
		}
		for _, value := range values {
			req2.Header.Add(key, value)
		}
	}
	req2.Header.Set("X-Forwarded-For", c.clientRealIP)
	req2.Header.Set("X-Real-IP", c.clientRealIP)
	stampUpstreamRequestIDFromRequestR376(req2, c.r)
	// R307: административная ручка воркера закрыта токеном — ретрай тоже.
	applyWorkerAdminTokenR307(req2.Header, c.llamacppPath, c.p.workerAdminToken(c.backendID))
	resp, err := c.clientForReq.Do(req2)
	if err != nil {
		durationMs := time.Since(c.llamaStartTime).Milliseconds()
		errType := determineErrorType(err, c.reqCtx)
		logger.Get().Errorw("proxyRequestLlamaCpp: upstream Do() failed after loading retry",
			"backend", c.backendID, "model", c.modelFromCtx, "attempt", c.llamaAttempt,
			"duration_ms", durationMs, "error_type", errType, "error", err)
		return false, fmt.Errorf("llama.cpp request failed after load retry [backend=%s, attempt=%d, duration_ms=%d, error_type=%s]: %v",
			c.backendID, c.llamaAttempt, durationMs, errType, err)
	}
	c.resp = resp
	logger.Get().Infow("proxyRequestLlamaCpp: retry after load successful",
		"backend", c.backendID, "model", c.modelFromCtx,
		"attempt", c.llamaAttempt, "duration_ms", time.Since(c.llamaStartTime).Milliseconds())
	return false, nil
}

// writeStreamHeaders — заголовки успешного стримингового ответа. Обязаны быть
// выставлены строго ДО WriteHeader(200); здесь же возвращаются id генерации и
// время загрузки модели из заголовков воркера.
func (c *upstreamCall) writeStreamHeaders() (requestID string, workerLoadNs int64) {
	// R536 (R2 читаемости): заголовки ответа, которые обязаны уйти ДО
	// WriteHeader, выставляются одной функцией (порядок и состав прежние).
	applyStreamResponseHeaders(c.w, c.resp)
	// R360 (2026-10-31): id генерации на воркере. По нему адресная отмена
	// (cancelUpstreamGeneration): без него отмена по модели гасит ВСЕ генерации этой
	// модели на узле, включая чужие (аудит abort, A5).
	// R535 (R2 читаемости): id генерации и время загрузки — из заголовков.
	requestID, workerLoadNs = upstreamIdentityFromHeaders(c.resp)
	// R332.3 (2026-10-30, поймано живой матрицей): нативный стрим обязан
	// объявлять, КТО обслужил запрос.
	//
	// generic-путь выставляет X-Backend-ID/X-Session-ID (proxy_request.go), а этот
	// нативный passthrough — нет: клиент и монитор не узнавали бэкенд, и по
	// ответу /api/chat нельзя было понять, кто отвечал. Заголовки ставим строго
	// ДО WriteHeader (после него они уже не уйдут).
	c.w.Header().Set("X-Backend-ID", c.backendID)
	if sid := c.p.getSessionIDWithModel(c.r, c.p.getClientName(c.r), c.modelFromCtx); sid != "" {
		c.w.Header().Set("X-Session-ID", sid)
	}
	if c.originalPath == "/v1/chat/completions" {
		c.w.Header().Set("Content-Type", "text/event-stream")
	} else {
		c.w.Header().Set("Content-Type", "application/x-ndjson")
	}
	c.w.Header().Set("Connection", "keep-alive")
	c.w.WriteHeader(http.StatusOK)
	return requestID, workerLoadNs
}

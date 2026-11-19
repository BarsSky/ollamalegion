// llamacpp_transport_native.go — R628 (2026-11-19): разбор нативного ответа воркера —
// завершающие кадры, усечение потока и склейка/снятие content в done-чанке.
//
// ЗАЧЕМ ВЫНЕСЕНО. `llamacpp_transport.go` — 3317 строк и 51 функция; в нём рядом
// живут транспортный цикл и мелкие разборщики кадров. Вынесена именно группа
// «разбор ответа»: `errString`, `emitTruncatedChunk`, `hasFinishReason`,
// `extractUpstreamDoneChunk`, `getEffectiveStreamTimeoutSec`, `appendNativeContent`,
// `stripNativeDoneContent`, `nativeDoneContentStrippingEnabled`, `autoTuneOnReturn`.
// Она не знает о `http.ResponseWriter` и о состоянии стрима, поэтому читается отдельно.
//
// ПОВЕДЕНИЕ НЕ МЕНЯЛОСЬ: код перенесён как есть; тесты `internal/balancer`
// (наборы про done-чанки, усечение и stats-окна) зелёные до и после.
package balancer

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/pkg/logger"
)

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

// llamacpp_native_path.go — R65d (2026-09-20): feature flag для маршрутизации
// Ollama-трафика на НАТИВНЫЕ эндпоинты cppworker.
//
// Проблема, которую это решает (аудит 2026-09-20, находки 1.1/1.2/2.6/2.7):
//
//	До R65d балансер транслировал ЛЮБОЙ /api/* запрос в OpenAI-формат:
//	  /api/chat       → /v1/chat/completions
//	  /api/generate   → /v1/completions
//	  /api/embeddings → /v1/embeddings
//	(translatePathForLlamaCpp, llamacpp_translate_req.go:11-22)
//
//	Ollama-клиенты (OpenWebUI в Ollama-режиме, `ollama run`, ollama-python,
//	Cline/Roo через ollama-provider) шлют Ollama-специфичные поля:
//	  options.{num_ctx,top_k,repeat_penalty,seed,min_p,mirostat*,...},
//	  keep_alive, format, think, images
//	Транслятор переносил из options только 5 полей (temperature, top_p, top_k,
//	num_predict, stop) и полностью терял остальные; а cppworker'овские
//	OpenAI-обработчики объявлены с `DisallowUnknownFields`
//	(pkg/types/contract_validation.go:71), поэтому любое перенесённое поле,
//	которого нет в структуре (например top_k), давало HTTP 400.
//
//	При этом cppworker УЖЕ имеет полноценные нативные обработчики
//	(cmd/cppworker/handlers_chat.go, handlers_generate.go) с полным разбором
//	Ollama options — они просто были недостижимы с пути балансера.
//
// Решение (вариант A аудита): не транслировать Ollama-пути вообще. Ollama-поля
// обрабатывает Ollama-код, OpenAI-поля — OpenAI-код, а DisallowUnknownFields
// снова становится полезным сигналом о реальной ошибке клиента, а не
// источником 400 на штатных запросах.
//
// Откат: LB_OLLAMA_NATIVE_PATH=0 возвращает прежнее поведение (трансляция).
// Флаг читается на каждый запрос (не кэшируется) — оператор может переключить
// без рестарта процесса, что важно при отладке на живом стенде.
package balancer

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
)

// IsOllamaNativePathEnabled — true (default) если Ollama-пути должны идти на
// нативные эндпоинты cppworker без трансляции в OpenAI-формат.
//
// Default: ВКЛЮЧЕНО. Значение по умолчанию — «правильное», потому что именно
// эта ветка сохраняет поля запроса; выключать имеет смысл только для
// диагностики регрессии.
//
// Явные выключатели: "0", "false", "no", "off".
func IsOllamaNativePathEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("LB_OLLAMA_NATIVE_PATH")))
	switch v {
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

// isOllamaNativePath — true для путей, у которых есть нативный обработчик в
// cppworker (cmd/cppworker/router.go:38-41) и для которых трансляция в
// OpenAI-формат ЛОМАЕТ данные.
//
// ВАЖНО: список намеренно узкий — только инференс-пути с телом Ollama-формата.
// Прочие /api/* (show, pull, delete, copy, tags, ps, models/load, ...) и так
// проксируются как есть (translatePathForLlamaCpp возвращает их без изменений),
// их добавлять не нужно и вредно — там нет трансляции, которую надо обойти.
//
// /api/embed добавлен, потому что для него НЕТ ветки в translatePathForLlamaCpp
// (уходит «как есть» в cppworker, который сам отдаёт Ollama-формат ответа) —
// т.е. он уже нативный, и это поведение надо сохранить единообразно.
func isOllamaNativePath(path string) bool {
	switch path {
	case "/api/chat", "/api/generate", "/api/embeddings", "/api/embed":
		return true
	}
	return false
}

// shouldRouteOllamaNative — единая точка решения для прокси-путей.
// Возвращает true, если запрос с этим путём надо отправить в cppworker
// нативно, без translatePathForLlamaCpp / translateOllamaBodyToOpenAI.
func shouldRouteOllamaNative(path string) bool {
	return IsOllamaNativePathEnabled() && isOllamaNativePath(path)
}

// forwardUpstreamWarningHeaders — R65d (2026-09-20): переносит заголовки
// предупреждений от cppworker в ответ клиенту.
//
// Зачем: cppworker считает, помещается ли prompt+n_predict в n_ctx, и при
// необходимости УРЕЗАЕТ n_predict, сообщая об этом заголовками
// (nctx_clamp.go:360-388, вызов handlers_openai.go:425 для OpenAI-пути и
// handlers_chat.go:226 для /api/chat):
//
//	X-Model-Context-Warning      "approaching;nCtx=...;pct=...;out=..."
//	X-Model-Context-Suggestion   человекочитаемая рекомендация
//	X-Model-Adjusted-NPredict    фактический n_predict после clamp
//
// До R65d балансер копировал из upstream только Content-Type/Length, поэтому
// урезание n_predict было для клиента НЕВИДИМЫМ: пользователь видел короткий
// ответ без объяснения причины. Заголовки ставим через Set только если
// upstream их прислал — иначе не затираем ничего своего.
//
// Capabilities (X-Model-Capabilities*) намеренно НЕ переносим: их считает сам
// балансер (addModelCapabilitiesHeaders → findModelCapabilities), и upstream
// может не знать полной картины кластера.
func forwardUpstreamWarningHeaders(w http.ResponseWriter, resp *http.Response) {
	if w == nil || resp == nil {
		return
	}
	for _, name := range []string{
		"X-Model-Context-Warning",
		"X-Model-Context-Suggestion",
		"X-Model-Adjusted-NPredict",
		"Retry-After",
	} {
		if v := resp.Header.Get(name); v != "" {
			w.Header().Set(name, v)
		}
	}
}

// intFromMap — безопасно достаёт целое из map[string]interface{}.
// JSON-числа приходят как float64, но некоторые пути кладут int/int64.
func intFromMap(m map[string]interface{}, key string) int {
	if m == nil {
		return 0
	}
	switch v := m[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	}
	return 0
}

// shadowBodySizeLimitBytes — R65d (2026-09-20): разумный потолок для JSON-тела
// inference-запроса на СТОРОНЕ БАЛАНСЕРА.
//
// Зачем: cppworker ограничивает тело через types.MaxStreamingBodyBytes (4 MB,
// pkg/types/contract_validation.go:108) и на превышение отвечает
// ErrBodyTooLarge → HTTP 413. Балансер же читал тело БЕЗ ограничения
// (io.ReadAll в llamacpp_handlers_inference.go) и часто ДВАЖДЫ (bodyBuf +
// translatedBody), то есть на запрос с большой историей и base64-картинками
// мог съесть сотни мегабайт памяти — и только потом получить 413 от upstream.
//
// Размер выбран так, чтобы:
//   - покрывать легитимные сценарии (длинная история + tools-схемы + несколько
//     изображений в base64);
//   - быть заведомо больше cppworker'овского лимита, чтобы диагностику
//     ("тело слишком большое") выдавала сторона, которая ближе к клиенту,
//     и с понятным сообщением, а не 413 после полной буферизации.
//
// Переопределяется через LB_MAX_REQUEST_BODY_MB (0 = без лимита).
const shadowDefaultMaxBodyMB = 64

// maxRequestBodyBytes возвращает действующий лимит тела в байтах.
// 0 означает «лимит отключён».
func maxRequestBodyBytes() int64 {
	raw := strings.TrimSpace(os.Getenv("LB_MAX_REQUEST_BODY_MB"))
	if raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			if n <= 0 {
				return 0 // явное отключение
			}
			return int64(n) << 20
		}
	}
	return int64(shadowDefaultMaxBodyMB) << 20
}

// readRequestBodyLimited — читает тело запроса с ограничением размера.
//
// Возвращает:
//   - body, nil            — успешно прочитано;
//   - nil, errBodyTooLarge — тело превышает лимит (caller отвечает 413);
//   - nil, err             — ошибка чтения.
//
// Используется вместо голого io.ReadAll на inference-путях, чтобы не
// буферизировать неограниченный объём перед тем, как upstream ответит 413.
func readRequestBodyLimited(r *http.Request) ([]byte, error) {
	if r == nil || r.Body == nil {
		return nil, nil
	}
	limit := maxRequestBodyBytes()
	if limit <= 0 {
		return io.ReadAll(r.Body)
	}
	// +1 байт, чтобы отличить «ровно в лимите» от «больше лимита».
	limited := io.LimitReader(r.Body, limit+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, errBodyTooLarge
	}
	return body, nil
}

// errBodyTooLarge — тело запроса превышает LB_MAX_REQUEST_BODY_MB.
var errBodyTooLarge = errors.New("request body exceeds configured limit")

// sendBodyTooLarge — формирует клиенту понятный 413 вместо невнятного обрыва.
//
// Формат ответа зависит от API, к которому обратился клиент: Ollama-клиенты
// ждут {"error": "..."}, OpenAI-клиенты — {"error": {"message": ...}}.
func sendBodyTooLarge(w http.ResponseWriter, r *http.Request) {
	limit := maxRequestBodyBytes()
	limitMB := limit >> 20
	msg := "request body too large"
	if limitMB > 0 {
		msg = "request body exceeds " + strconv.FormatInt(limitMB, 10) + " MB limit " +
			"(raise LB_MAX_REQUEST_BODY_MB to allow larger payloads)"
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusRequestEntityTooLarge)

	if r != nil && strings.HasPrefix(r.URL.Path, "/v1/") {
		body, _ := json.Marshal(map[string]interface{}{
			"error": map[string]interface{}{
				"message": msg,
				"type":    "invalid_request_error",
				"code":    http.StatusRequestEntityTooLarge,
			},
		})
		_, _ = w.Write(body)
		return
	}
	body, _ := json.Marshal(map[string]interface{}{
		"error":       msg,
		"done":        true,
		"done_reason": "error",
	})
	_, _ = w.Write(body)
}

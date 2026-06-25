// Package balancer — парсинг структурированных ошибок от cppworker (Stage 3.1).
//
// cppworker сейчас возвращает JSON-ошибки в формате (см. cmd/cppworker/main.go
// writeCppWorkerErrorWithBridgeInfo):
//
//	{
//	  "error": "stream inference failed with code 2: ...",
//	  "code":  2,
//	  "bridge_info": {
//	    "code": 2, "current_n_ctx": 4096, "required_n_ctx": 16384,
//	    "actual_tokens": 8000, "n_predict": 4096, "n_ctx_override": 16384,
//	    "max_vram_n_ctx": 77000, "message": "..."
//	  }
//	}
//
// balancer ловит sentinel-ы bridge.ErrNCtxNeedsReload / bridge.ErrPromptTooLong
// и вызывает NCtxReloadCoordinator для принятия решения о reload-у backend-а.
//
// Этот файл зеркалирует c/bridge.BridgeErrorInfo в тип balancer.NCtxBridgeError
// (без прямого импорта c/bridge, чтобы пакет balancer мог использоваться в
// stub-режиме).
package balancer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"ollama-loadbalancer/c/bridge"
)

// cppWorkerErrorBody — структура JSON-ответа об ошибке от cppworker
// (см. writeCppWorkerErrorWithBridgeInfo в cmd/cppworker/main.go).
type cppWorkerErrorBody struct {
	Error      string               `json:"error"`
	Code       int                  `json:"code"`
	BridgeInfo *cppWorkerBridgeInfo `json:"bridge_info,omitempty"`
}

// cppWorkerBridgeInfo — структурированная информация об ошибке от C-bridge.
type cppWorkerBridgeInfo struct {
	Code         int    `json:"code"`
	CurrentNCtx  int    `json:"current_n_ctx"`
	RequiredNCtx int    `json:"required_n_ctx"`
	ActualTokens int    `json:"actual_tokens"`
	NPredict     int    `json:"n_predict"`
	NCtxOverride int    `json:"n_ctx_override"`
	MaxVRAMNCtx  int    `json:"max_vram_n_ctx"`
	Message      string `json:"message"`
}

// NCtxError — обёртка для структурированной ошибки от cppworker.
// errors.Is(err, bridge.ErrNCtxNeedsReload) / bridge.ErrPromptTooLong работает.
type NCtxError struct {
	BackendID string
	// HTTPStatus — оригинальный HTTP-статус от cppworker (400/500).
	HTTPStatus int
	// Body — полный ответ (используется для логов и /metrics).
	Body string
	// BridgeInfo — структурированные поля (nil, если cppworker вернул
	// «oldschool»-ошибку без bridge_info).
	BridgeInfo *NCtxBridgeError
}

// Error возвращает человекочитаемое описание ошибки.
func (e *NCtxError) Error() string {
	if e.BridgeInfo != nil && e.BridgeInfo.Message != "" {
		return fmt.Sprintf("cppworker error: code=%d %s (backend=%s, current_n_ctx=%d, required_n_ctx=%d)",
			e.BridgeInfo.Code, e.BridgeInfo.Message, e.BackendID,
			e.BridgeInfo.CurrentNCtx, e.BridgeInfo.RequiredNCtx)
	}
	return fmt.Sprintf("cppworker error: status=%d body=%s (backend=%s)",
		e.HTTPStatus, truncateBody(e.Body, 200), e.BackendID)
}

// Unwrap позволяет errors.Is(err, bridge.ErrNCtxNeedsReload) и
// errors.Is(err, bridge.ErrPromptTooLong) корректно работать.
func (e *NCtxError) Unwrap() error {
	if e.BridgeInfo == nil {
		return nil
	}
	switch e.BridgeInfo.Code {
	case bridge.ErrCodeNCtxNeedsReload:
		return bridge.ErrNCtxNeedsReload
	case bridge.ErrCodePromptTooLong:
		return bridge.ErrPromptTooLong
	}
	return nil
}

// truncateBody — обрезает body для логов (избегаем огромных responses).
func truncateBody(body string, max int) string {
	if len(body) <= max {
		return body
	}
	return body[:max] + "...(truncated)"
}

// isNCtxRelevantCode — считается ли legacy top-level code (без bridge_info)
// n_ctx-релевантным. Только коды 2 (NCtxNeedsReload) и 3 (PromptTooLong)
// триггерят auto-reload или 413. Любой другой код (включая 0, 1, 7 и пр.)
// трактуется как generic cppworker-internal ошибка и НЕ превращается в
// NCtxError (caller пробросит оригинал клиенту).
//
// Дополнительно: code=1 (Generic) тоже считаем нерелевантным — cppworker
// использует его для широкого спектра внутренних ошибок (nullptr, OOM при
// загрузке модели и т.п.), для которых auto-reload не поможет.
// isNCtxRelevantCode — считается ли legacy top-level code (без bridge_info)
// n_ctx-релевантным.
//
// 2026-06-25: переименовано семантически — теперь «n_ctx-reloadable».
//   - ErrCodeNCtxNeedsReload (2) → reload с бОльшим n_ctx может помочь.
//   - ErrCodePromptTooLong (3) → reload с тем же n_ctx бесполезен (см. nctx_reload.go
//     DecisionReject), но парсер всё равно возвращает NCtxError, чтобы caller
//     мог дать actionable 413.
//
// 2026-06-25: ErrCodeInsufficientResources (6) и ErrCodeBadRequest (5) и
// ErrCodeGPUOOM (4) НЕ считаются reloadable:
//   - ErrCodeInsufficientResources — каскад RAM fallback → partial offload →
//     cpu-only уже исчерпан; reload с другими параметрами не поможет.
//     Caller должен пробросить 413 с actionable details клиенту.
//   - ErrCodeGPUOOM — частично reloadable (cppworker делает RAM fallback),
//     но после исчерпания лимита reloadable превращается в InsufficientResources.
//   - ErrCodeBadRequest — синтаксическая ошибка, reload не поможет.
//
// В итоге только коды 2 и 3 триггерят auto-reload/reject-через-balancer.
// Остальные пробрасываются как generic 5xx (caller не получит NCtxError).
func isNCtxRelevantCode(code int) bool {
	switch code {
	case bridge.ErrCodeNCtxNeedsReload, bridge.ErrCodePromptTooLong:
		return true
	}
	return false
}

// ParseCppWorkerError парсит JSON-ответ об ошибке от cppworker.
// Возвращает *NCtxError если:
//   - body содержит валидный JSON
//   - HTTP-статус 4xx или 5xx
//   - и/или присутствует поле "code" / "bridge_info" с n_ctx-релевантным кодом
//
// В остальных случаях возвращает nil (это нормальный ответ или не-recoverable ошибка).
func ParseCppWorkerError(body []byte, statusCode int, backendID string) error {
	if len(body) == 0 {
		return nil
	}
	if statusCode < 400 {
		// 2xx/3xx — не ошибка, даже если body содержит «error» поле.
		return nil
	}

	var parsed cppWorkerErrorBody
	if err := json.Unmarshal(body, &parsed); err != nil {
		// Не JSON. Возможно, это plain-text 500 ошибка от старой версии cppworker.
		// Не возвращаем NCtxError (т.к. без structured code) — caller просто
		// пробросит 502 клиенту.
		return nil
	}

	// Определяем, есть ли у нас structured NCtxBridgeError
	bridgeErr := convertCppWorkerBridgeInfo(parsed.BridgeInfo)
	if bridgeErr == nil && isNCtxRelevantCode(parsed.Code) {
		// Нет bridge_info, но есть top-level code (старая версия cppworker).
		// Считаем legacy code «релевантным» только если он равен одному из
		// известных n_ctx-кодов (NCtxNeedsReload=2, PromptTooLong=3). Любой
		// другой код (включая зарезервированные cppworker-internal, например
		// code=7 «model not found») — НЕ считаем NCtxError, чтобы не отдавать
		// клиенту 413 на чужие ошибки. См. TestNCtxError_Truncation.
		bridgeErr = &NCtxBridgeError{
			Code:    parsed.Code,
			Message: parsed.Error,
		}
	}

	if bridgeErr == nil {
		// Fallback: detect by string (для совсем старых cppworker без structured code).
		// Только для n_ctx-релевантных ошибок, чтобы не превращать generic 5xx
		// в NCtxError.
		errLower := strings.ToLower(parsed.Error)
		if strings.Contains(errLower, "n_ctx") {
			bridgeErr = &NCtxBridgeError{
				Code:    bridge.ErrCodeNCtxNeedsReload,
				Message: parsed.Error,
			}
		} else {
			return nil
		}
	}

	return &NCtxError{
		BackendID:  backendID,
		HTTPStatus: statusCode,
		Body:       string(body),
		BridgeInfo: bridgeErr,
	}
}

// convertCppWorkerBridgeInfo — копирует cppWorkerBridgeInfo в NCtxBridgeError
// (который используется внутри balancer; см. nctx_reload.go).
func convertCppWorkerBridgeInfo(info *cppWorkerBridgeInfo) *NCtxBridgeError {
	if info == nil {
		return nil
	}
	return &NCtxBridgeError{
		Code:         info.Code,
		CurrentNCtx:  info.CurrentNCtx,
		RequiredNCtx: info.RequiredNCtx,
		ActualTokens: info.ActualTokens,
		NPredict:     info.NPredict,
		NCtxOverride: info.NCtxOverride,
		MaxVRAMNCtx:  info.MaxVRAMNCtx,
		Message:      info.Message,
	}
}

// DrainAndParseError — helper, который читает body из resp, парсит
// структурированную ошибку и закрывает body. Возвращает:
//   - (*NCtxError, true) если ошибка n_ctx-релевантная
//   - (nil, false) если нет (например, generic 5xx) — caller должен пробросить оригинал
//   - (nil, false, "...") если body не JSON / пустой / не ошибка
//
// Используется в proxyRequestLlamaCpp, proxyRequestLlamaCppNonStream и
// handleOpenAIChatCompletions после получения HTTP-ответа от cppworker.
func DrainAndParseError(resp *http.Response, backendID string) (*NCtxError, bool, error) {
	if resp == nil || resp.Body == nil {
		return nil, false, nil
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return nil, false, fmt.Errorf("read body: %w", err)
	}
	// Восстанавливаем body для дальнейшего проксирования (если это не наш кейс).
	// Для 4xx/5xx caller всё равно не будет проксировать, так что
	// resp.Body.Close() достаточно.
	_ = body // suppress unused
	parsed := ParseCppWorkerError(body, resp.StatusCode, backendID)
	var nctxErr *NCtxError
	if errors.As(parsed, &nctxErr) {
		return nctxErr, true, nil
	}
	return nil, false, nil
}

// readBodyOnce — утилита для чтения body один раз (если нужно несколько раз парсить).
func readBodyOnce(body io.Reader, maxBytes int64) ([]byte, error) {
	if body == nil {
		return nil, nil
	}
	limited := io.LimitReader(body, maxBytes)
	return io.ReadAll(limited)
}

// bufferedBody — обёртка для восстановления body после ReadAll.
// Используется когда нужно прочитать body для парсинга, но также проксировать.
type bufferedBody struct {
	bytes.Buffer
}

func (b *bufferedBody) Close() error { return nil }

// AsBufferedBody конвертирует []byte в io.ReadCloser для передачи в upstream.
func AsBufferedBody(data []byte) io.ReadCloser {
	bb := &bufferedBody{}
	bb.Write(data)
	return bb
}

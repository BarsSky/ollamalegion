// Package balancer — n_ctx reject для streaming-клиентов (Stage 4.5).
//
// Проблема: writeNCtxRejectResponse (в nctx_reload_handlers.go) пишет HTTP 413/503
// с RejectMsg JSON. Это работает для не-streaming клиентов (Cline через /v1/chat/completions
// non-stream, или OpenWebUI с stream=false). Но для streaming клиентов (OpenWebUI
// с stream=true, Cline streaming) ситуация другая:
//
//  1. Клиент отправил POST с Accept: text/event-stream
//  2. Server начинает стримить ответ
//  3. Если приходит reject — НЕЛЬЗЯ просто отдать HTTP 413, потому что
//     клиент уже ждёт SSE-стрим. HTTP-статус можно поменять только ДО первого
//     WriteHeader (которого ещё не было).
//  4. Вместо этого нужно:
//     - сразу выставить HTTP-статус (например, 413) и заголовки
//     - отправить ОДИН SSE-чанк с error и done:true
//     - закрыть соединение
//
// Для Ollama-стрима (NDJSON) формат чанка с ошибкой:
//
//	{"model":"...","created_at":"...","done":true,"done_reason":"error",
//	 "error":"...","message":{"role":"assistant","content":""}}
//
// Этот файл реализует writeStreamingRejectNDJSON — обёртку для streaming-режима,
// которая вызывается из proxyRequestLlamaCpp когда:
//  1. upstream (cppworker) вернул ошибку до начала стрима (status >= 400)
//  2. или reload/reject решено в середине стрима
package balancer

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// writeStreamingRejectNDJSON — формирует streaming-ответ с reject-информацией
// в формате Ollama NDJSON. Используется когда клиент отправил stream=true и
// upstream (cppworker) вернул n_ctx-ошибку.
//
// Формат: один NDJSON-чанк с done:true, done_reason:"error" и полем error.
// Заголовки: Content-Type: application/x-ndjson, X-NCtx-Reload-Decision: reject.
//
// statusCode: 413 (если есть max_vram_n_ctx, можно посчитать safe_max),
//
//	503 (если CPU-only, нет VRAM info).
//
// modelName: имя модели (для NDJSON-чанка, может быть пустым).
// plan: ReloadPlan с RejectMsg/Reason (если есть).
// bridgeErr: NCtxBridgeError (если есть) для дополнительной диагностики.
func writeStreamingRejectNDJSON(w http.ResponseWriter, statusCode int, modelName string, plan *ReloadPlan, bridgeErr *NCtxBridgeError) {
	if statusCode < 400 {
		// HTTP 413 Payload Too Large (числовая константа для совместимости с go 1.24+)
		statusCode = 413
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("X-NCtx-Reload-Decision", "reject")
	if plan != nil && plan.NewNCtx > 0 {
		w.Header().Set("X-NCtx-Required", fmt.Sprintf("%d", plan.NewNCtx))
	}
	if bridgeErr != nil && bridgeErr.CurrentNCtx > 0 {
		w.Header().Set("X-NCtx-Current", fmt.Sprintf("%d", bridgeErr.CurrentNCtx))
	}
	w.WriteHeader(statusCode)

	// Структура чанка:{"model":...,"created_at":...,"done":true,"done_reason":"error","error":...,"message":{"role":"assistant","content":""}}
	rejectMessage := "n_ctx_too_large_for_backend"
	if plan != nil && plan.Reason != "" {
		rejectMessage = plan.Reason
	}
	if bridgeErr != nil && bridgeErr.Message != "" {
		rejectMessage = bridgeErr.Message
	}
	chunk := map[string]interface{}{
		"model":       modelName,
		"created_at":  time.Now().UTC().Format(time.RFC3339),
		"done":        true,
		"done_reason": "error",
		"error":       rejectMessage,
		"message": map[string]interface{}{
			"role":    "assistant",
			"content": "",
		},
	}
	// Если есть plan.RejectMsg (детальный JSON с reason/suggested), встраиваем его
	// в поле error как структурированную строку (клиент парсит если умеет).
	if plan != nil && plan.RejectMsg != "" {
		chunk["bridge_info"] = json.RawMessage(plan.RejectMsg)
	}
	out, _ := json.Marshal(chunk)
	w.Write(out)
	w.Write([]byte("\n"))
}

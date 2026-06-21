// llamacpp_transport_helpers.go — Вспомогательные функции для streaming-прокси
// llama.cpp/cppworker. Используются llamacpp_transport.go (proxyRequestLlamaCpp)
// для корректного завершения streaming-ответов и инкрементального сбора tool_calls.
package balancer

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)


// accumulatedToolCall — состояние одного tool_call, собранное из нескольких SSE-чанков.
type accumulatedToolCall struct {
	index    int
	id       string
	function map[string]interface{}
}

// accumulateToolCallsFromDelta — инкрементальный сбор tool_calls из chunks SSE-стрима.
//
// llama.cpp может разбить один tool_call на несколько SSE-чанков, когда стримит
// через /v1/chat/completions. Каждый чанк содержит delta.tool_calls[N] с частичными
// полями function.name или function.arguments. Эта функция смерживает:
//
//  1. function.name — перезаписывается (первый чанк с name выигрывает)
//  2. function.arguments — конкатенируется (добавляется к предыдущим)
//  3. id — перезаписывается (первый валидный id выигрывает)
//
// Потокобезопасность не требуется — вызывается только из proxyRequestLlamaCpp
// в однопоточном streaming-цикле.
func accumulateToolCallsFromDelta(delta map[string]interface{}, toolAccum map[int]*accumulatedToolCall) {
	if delta == nil || toolAccum == nil {
		return
	}

	rawToolCalls, ok := delta["tool_calls"]
	if !ok || rawToolCalls == nil {
		return
	}

	toolCallsList, ok := rawToolCalls.([]interface{})
	if !ok || len(toolCallsList) == 0 {
		return
	}

	for _, rawTC := range toolCallsList {
		tc, ok := rawTC.(map[string]interface{})
		if !ok {
			continue
		}

		// Определяем индекс tool_call (от OpenAI: index в delta.tool_calls[N].index)
		idx := -1
		if rawIdx, ok := tc["index"]; ok {
			switch v := rawIdx.(type) {
			case float64:
				idx = int(v)
			case int:
				idx = v
			case json.Number:
				if n, err := v.Int64(); err == nil {
					idx = int(n)
				}
			}
		}
		if idx < 0 {
			// Если индекса нет — используем следующее целое (ordered append)
			idx = len(toolAccum)
		}

		// Берём или создаём аккумулятор для этого индекса
		acc, exists := toolAccum[idx]
		if !exists {
			acc = &accumulatedToolCall{
				index:    idx,
				function: make(map[string]interface{}),
			}
			toolAccum[idx] = acc
		}

		// Извлекаем id (первый валидный выигрывает)
		if acc.id == "" {
			if rawID, ok := tc["id"].(string); ok && rawID != "" {
				acc.id = rawID
			}
		}

		// Извлекаем function
		if rawFunc, ok := tc["function"].(map[string]interface{}); ok {
			// function.name — перезаписываем
			if name, ok := rawFunc["name"].(string); ok && name != "" {
				acc.function["name"] = name
			}
			// function.arguments — конкатенируем
			if args, ok := rawFunc["arguments"].(string); ok && args != "" {
				existingArgs, _ := acc.function["arguments"].(string)
				acc.function["arguments"] = existingArgs + args
			}
		}
	}
}

// writeStreamingSSEDone — пишет финальный [DONE] / done-маркер для streaming-потока.
//
// Когда upstream (llama.cpp) присылает data: [DONE], этот обработчик пишет
// завершающий NDJSON-чанк с tool_calls (если они были накоплены) или просто
// done:true для Ollama-клиентов.
//
// Для /v1/chat/completions (SSE→SSE passthrough) — просто пишет data: [DONE]\n\n.
// Для /api/chat — пишет NDJSON с done:true, message.role, message.content="",
//
//	и если есть tool_calls — message.tool_calls.
//
// Для /api/generate — пишет NDJSON с done:true, response="".
func writeStreamingSSEDone(w http.ResponseWriter, originalPath, modelFromCtx string, toolAccum map[int]*accumulatedToolCall) error {
	if originalPath == "/v1/chat/completions" {
		// SSE→SSE passthrough — просто завершаем [DONE]
		_, err := fmt.Fprintf(w, "data: [DONE]\n\n")
		return err
	}

	// Для /api/chat и /api/generate пишем NDJSON
	if originalPath == "/api/chat" {
		msgMap := map[string]interface{}{
			"role":    "assistant",
			"content": "",
		}

		// Если есть накопленные tool_calls — добавляем их
		if len(toolAccum) > 0 {
			toolCallsArr := make([]map[string]interface{}, 0, len(toolAccum))
			for i := 0; i < len(toolAccum); i++ {
				acc, ok := toolAccum[i]
				if !ok {
					continue
				}
				tcMap := map[string]interface{}{
					"type":     "function",
					"function": acc.function,
				}
				if acc.id != "" {
					tcMap["id"] = acc.id
				}
				toolCallsArr = append(toolCallsArr, tcMap)
			}
			msgMap["tool_calls"] = toolCallsArr
		}

		modelName := modelFromCtx
		if modelName == "" {
			modelName = "unknown"
		}
		doneChunk := map[string]interface{}{
			"model":      modelName,
			"created_at": time.Now().UTC().Format(time.RFC3339),
			"done":       true,
			"message":    msgMap,
		}

		out, err := json.Marshal(doneChunk)
		if err != nil {
			return fmt.Errorf("marshal /api/chat done chunk: %v", err)
		}
		_, err = fmt.Fprintf(w, "%s\n", string(out))
		return err
	}

	// /api/generate
	modelName := modelFromCtx
	if modelName == "" {
		modelName = "unknown"
	}
	doneChunk := map[string]interface{}{
		"model":      modelName,
		"created_at": time.Now().UTC().Format(time.RFC3339),
		"done":       true,
		"response":   "",
	}

	out, err := json.Marshal(doneChunk)
	if err != nil {
		return fmt.Errorf("marshal /api/generate done chunk: %v", err)
	}
	_, err = fmt.Fprintf(w, "%s\n", string(out))
	return err
}

// buildDoneResponse — строит финальный NDJSON done-маркер для пустого streaming-потока.
//
// Используется когда SSE-поток от upstream полностью пуст (не пришло ни одного
// data:-чанка, включая [DONE]). Это fallback, чтобы клиент не висел вечно.
//
// errCode — код ошибки от bridge (0 = OK, 2 = ErrNCtxNeedsReload, 3 = ErrPromptTooLong, etc.).
// Для /api/chat возвращает {"done":true,"message":{"role":"assistant","content":""}}.
// Для /api/generate возвращает {"done":true,"response":""}.
// Для /v1/chat/completions возвращает nil (не используется — SSE→SSE не требует NDJSON).
func buildDoneResponse(originalPath, modelFromCtx string, errCode int) []byte {
	if originalPath == "/v1/chat/completions" {
		return nil
	}

	modelName := modelFromCtx
	if modelName == "" {
		modelName = "unknown"
	}

	if originalPath == "/api/chat" {
		chunk := map[string]interface{}{
			"model":      modelName,
			"created_at": time.Now().UTC().Format(time.RFC3339),
			"done":       true,
			"done_reason": func() string {
				if errCode != 0 {
					return "error"
				}
				return "stop"
			}(),
			"message": map[string]interface{}{
				"role":    "assistant",
				"content": "",
			},
		}
		if errCode != 0 {
			chunk["error"] = fmt.Sprintf("inference error code: %d", errCode)
		}
		out, _ := json.Marshal(chunk)
		return out
	}

	// /api/generate
	chunk := map[string]interface{}{
		"model":      modelName,
		"created_at": time.Now().UTC().Format(time.RFC3339),
		"done":       true,
		"done_reason": func() string {
			if errCode != 0 {
				return "error"
			}
			return "stop"
		}(),
		"response": "",
	}

	if errCode != 0 {
		chunk["error"] = fmt.Sprintf("inference error code: %d", errCode)
	}
	out, _ := json.Marshal(chunk)
	return out
}

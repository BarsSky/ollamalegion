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

// cleanFinalContent — пост-обработка накопленного контента перед отправкой
// в финальном done-чанке. Удаляет служебные токены (``, `</s>`),
// ведущие/завершающие пробелы, и нормализует переносы строк.
func cleanFinalContent(s string) string {
	return stripServiceTokens(s)
}

// writeStreamingSSEDone — пишет финальный [DONE] / done-маркер для streaming-потока.
//
// Когда upstream (llama.cpp) присылает data: [DONE], этот обработчик пишет
// завершающий NDJSON-чанк с tool_calls (если они были накоплены) или просто
// done:true для Ollama-клиентов.
//
// Семантика по originalPath:
//   - /v1/chat/completions (SSE→SSE passthrough) — пишет data: [DONE]\n\n и flush.
//   - /api/chat — пишет NDJSON с done:true, message.role, message.content (из upstreamContent
//     или накопленного буфера) и если есть tool_calls — message.tool_calls.
//   - /api/generate — пишет NDJSON с done:true, response (из upstreamContent или буфера).
//
// Bug #12 (2026-06-30): параметр upstreamSentFinishReason предотвращает дублирование
// финального NDJSON. Если upstream ПРИСЛАЛ finish_reason в одном из чанков до [DONE],
// то translateOpenAISSEDataToOllama УЖЕ записал финальный done-чанк с полным
// message.content / response. В этом случае writeStreamingSSEDone НЕ пишет второй
// чанк с тем же content, иначе клиент увидит дубликат ответа.
//
// Round 53.1 (2026-08-24): параметр usageChunkSeen разделяет два случая:
//   1. upstream ПРИСЛАЛ usage чанк (после finish_reason) — translateUsageChunkToOllama
//      уже записал canonical done:true с полным набором статов. writeStreamingSSEDone
//      ПРОПУСКАЕТ запись (skip).
//   2. upstream ПРИСЛАЛ finish_reason, но usage чанк НЕ пришёл — writeStreamingSSEDone
//      ПИШЕТ финальный done-чанк с накопленным content как fallback. Без этого
//      клиент (Cline/ollama npm) зависнет без done (regression из-за подавления
//      R51.3 wrapper-чанка в R53.1).
//
// Если upstream НЕ прислал finish_reason (только [DONE] без завершающего чанка),
// writeStreamingSSEDone пишет финальный NDJSON с накопленным content — это
// спасает от пустого content в клиенте.
//
// Если есть tool_calls — writeStreamingSSEDone ВСЕГДА пишет финальный чанк
// (translate не формирует tool_calls в финальном чанке), при этом content
// ставится пустым (tool_calls заменяют content по семантике OpenAI).
func writeStreamingSSEDone(
	w http.ResponseWriter,
	originalPath, modelFromCtx string,
	toolAccum map[int]*accumulatedToolCall,
	accumulatedContent string,
	upstreamContent string,
	upstreamSentFinishReason bool,
	usageChunkSeen bool,
) error {
	if originalPath == "/v1/chat/completions" {
		// SSE→SSE passthrough — просто завершаем [DONE]
		_, err := fmt.Fprintf(w, "data: [DONE]\n\n")
		// ВАЖНО: на быстрых моделях (Qwen3.6-35B-A3B, Llama-3.1-70B) финальный
		// [DONE] может остаться в Go HTTP буфере и не дойти до клиента до закрытия
		// TCP-сокета. Без flush клиент (aiohttp, httpx) парсит chunked-encoding
		// с ошибкой: TransferEncodingError: Not enough data to satisfy transfer length header.
		Flush(w)
		return err
	}

	// Bug #12 fix (2026-06-30): определяем итоговый контент с приоритетом:
	//   1. upstreamContent (если есть, обычно это отфильтрованный cppworker done-чанк)
	//   2. accumulatedContent (накопленный из streaming чанков)
	//   3. "" (fallback — не должно происходить в нормальной ситуации)
	finalContent := upstreamContent
	if finalContent == "" {
		finalContent = accumulatedContent
	}
	finalContent = cleanFinalContent(finalContent)

	hasToolCalls := len(toolAccum) > 0

	// Round 53.1 (2026-08-24): if translateUsageChunkToOllama already emitted the
	// canonical done:true chunk (with full stats), we MUST skip here — otherwise
	// the client sees TWO done:true chunks (R51.3 regression).
	if usageChunkSeen {
		Flush(w)
		return nil
	}

	// Round 53.1: if upstreamSentFinishReason=true (wrapper-чанк был, но мы его
	// подавили в R53.1) и usage чанка не было — writeStreamingSSEDone пишет
	// финальный done-чанк как fallback, иначе клиент зависнет.
	// (До R53.1 этот блок был: if upstreamSentFinishReason && !hasToolCalls { skip } —
	// что приводило к double-done в случае usage чанка.)
	_ = upstreamSentFinishReason // used implicitly: если false, всё равно пишем done ниже

	// Round 53.1: убран старый skip-блок "if upstreamSentFinishReason && !hasToolCalls { skip }".
	// Pre-R53.1 wrapper-чанк эмитил done:true (R51.3 fix), и writeStreamingSSEDone
	// пропускал повторную запись. Post-R53.1 wrapper подавлен, canonical done
	// эмитится ТОЛЬКО из translateUsageChunkToOllama (если usage чанк пришёл) или
	// из writeStreamingSSEDone как fallback (если usage чанка не было).
	// Случай usageChunkSeen уже обработан выше. Случай !usageChunkSeen —
	// writeStreamingSSEDone ПИШЕТ финальный done-чанк с накопленным content.

	// Для /api/chat и /api/generate пишем NDJSON
	if originalPath == "/api/chat" {
		// При наличии tool_calls content намеренно пустой (tool_calls заменяют content).
		contentForMsg := finalContent
		if hasToolCalls {
			contentForMsg = ""
		}
		msgMap := map[string]interface{}{
			"role":    "assistant",
			"content": contentForMsg,
		}

		// Если есть накопленные tool_calls — добавляем их
		if hasToolCalls {
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

		// Если есть tool_calls — done_reason должен быть "tool_calls"
		if hasToolCalls {
			doneChunk["done_reason"] = "tool_calls"
		} else if finalContent != "" {
			doneChunk["done_reason"] = "stop"
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
	// При наличии tool_calls response должен быть пустым (как и в /api/chat)
	// и done_reason = "tool_calls" (Ollama-семантика).
	responseValue := finalContent
	if hasToolCalls {
		responseValue = ""
	}
	doneChunk := map[string]interface{}{
		"model":      modelName,
		"created_at": time.Now().UTC().Format(time.RFC3339),
		"done":       true,
		"response":   responseValue,
	}
	switch {
	case hasToolCalls:
		doneChunk["done_reason"] = "tool_calls"
	case finalContent != "":
		doneChunk["done_reason"] = "stop"
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
// terminal_frame_r377.go — R377 (2026-11-01): ОДИН сборщик терминального
// кадра на транслируемом пути (OpenAI-апстрим → Ollama-клиент).
//
// ЗАЧЕМ. Аудит регрессий (P2-4) назвал дублированием то, что на одном и том же
// пути `/api/chat` terminal-content решался ДВУМЯ способами:
//
//  1. writeStreamingSSEDone восстанавливал полный текст в завершающем кадре
//     (upstreamContent → accumulatedContent → cleanFinalContent);
//  2. emitTerminalDoneOnly (в llamacpp_transport.go) собирал СВОЙ кадр руками,
//     с content:"", мимо хелпера.
//
// Второй кадр разошёлся с контрактом в трёх местах, и это не теория — каждая
// ветка была достижима:
//
//   - tool_calls уходили в OpenAI-форме (function.arguments строкой), тогда как
//     Ollama ждёт ОБЪЕКТ (R295). Клиент получал tool call, который не мог
//     разобрать;
//   - на /api/generate клалось поле "message" вместо "response" — Ollama-клиент
//     /api/generate такое поле не читает, ответ выглядел пустым;
//   - done_reason="stop" ставился даже когда клиенту ничего не отдавалось
//     (в остальных ветках он выводился из состояния).
//
// РЕШЕНИЕ. Кадр собирает ровно одна функция — writeTerminalFrameR377.
// Вызывающий решает ТОЛЬКО одно: какой видимый текст положить в кадр
// (восстановленный или пустой, если текст уже ушёл дельтами). Форма кадра,
// конвертация tool_calls и done_reason — здесь, в одном месте.
package balancer

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// terminalFrameR377 — вход для единственного сборщика терминального кадра.
//
// content — уже принятый вызывающим текст: "" означает «не повторять то, что
// клиент уже видел дельтами». При наличии tool_calls content всегда пуст
// (tool_calls заменяют текст по семантике OpenAI/Ollama).
type terminalFrameR377 struct {
	path      string
	model     string
	toolCalls map[int]*accumulatedToolCall
	content   string
}

// writeTerminalFrameR377 — единственный писатель завершающего кадра клиента на
// транслируемом пути: SSE-терминатор для /v1, NDJSON-кадр для /api/chat и
// /api/generate.
//
// flush для /v1 делает сам (иначе [DONE] может остаться в буфере Go до
// закрытия сокета — клиенты на aiohttp/httpx падают с TransferEncodingError).
// NDJSON-путь флашит вызывающий: у него после кадра бывает ещё работа
// (например авто-продолжение).
func writeTerminalFrameR377(w http.ResponseWriter, f terminalFrameR377) error {
	modelName := f.model
	if modelName == "" {
		modelName = "unknown"
	}

	if f.path == "/v1/chat/completions" {
		_, err := fmt.Fprintf(w, "data: [DONE]\n\n")
		Flush(w)
		return err
	}

	hasToolCalls := len(f.toolCalls) > 0
	// R328: значение из закрытого реестра причин. Естественный конец — "stop";
	// наличие tool_calls переопределяет причину.
	doneReason := "stop"
	if hasToolCalls {
		doneReason = "tool_calls"
	}

	if f.path == "/api/chat" {
		contentForMsg := f.content
		if hasToolCalls {
			contentForMsg = ""
		}
		msgMap := map[string]interface{}{
			"role":    "assistant",
			"content": contentForMsg,
		}
		if hasToolCalls {
			toolCallsArr := make([]map[string]interface{}, 0, len(f.toolCalls))
			for i := 0; i < len(f.toolCalls); i++ {
				acc, ok := f.toolCalls[i]
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
			// R295: Ollama ждёт function.arguments ОБЪЕКТОМ, а аккумулятор
			// держит его строкой (OpenAI-форма контракта cppworker→балансер).
			msgMap["tool_calls"] = toOllamaToolCallsObject(toInterfaceSlice(toolCallsArr))
		}

		doneChunk := map[string]interface{}{
			"model":       modelName,
			"created_at":  time.Now().UTC().Format(time.RFC3339),
			"done":        true,
			"done_reason": doneReason,
			"message":     msgMap,
		}
		out, err := json.Marshal(doneChunk)
		if err != nil {
			return fmt.Errorf("marshal /api/chat done chunk: %v", err)
		}
		_, err = fmt.Fprintf(w, "%s\n", string(out))
		return err
	}

	// /api/generate: у Ollama здесь поле response, а не message. У /api/generate
	// нет tool_calls в кадре — при вызове инструмента остаётся только причина.
	responseValue := f.content
	if hasToolCalls {
		responseValue = ""
	}
	doneChunk := map[string]interface{}{
		"model":       modelName,
		"created_at":  time.Now().UTC().Format(time.RFC3339),
		"done":        true,
		"done_reason": doneReason,
		"response":    responseValue,
	}
	out, err := json.Marshal(doneChunk)
	if err != nil {
		return fmt.Errorf("marshal /api/generate done chunk: %v", err)
	}
	_, err = fmt.Fprintf(w, "%s\n", string(out))
	return err
}

// vision_stream.go — R248/B3 (2026-10-27): стриминговый ответ для
// image-запросов.
//
// ЗАЧЕМ. Префилл с изображениями синхронный (libmtmd): потоковых токенов до
// первого чанка нет. Раньше такие запросы отбивались 400
// vision_stream_not_supported — и это блокировало включение capability для
// СТРИМИНГОВЫХ клиентов (OpenWebUI, OpenAI SDK по умолчанию), то есть фича
// оставалась выключенной для большинства.
//
// РЕШЕНИЕ. Считаем ответ синхронно и отдаём его валидным потоком из двух
// чанков: content + done. Это тот же приём, что R66b использует для JSON-режима
// (там тоже нужен весь ответ целиком). Клиент получает обычный stream, просто
// в одном чанке.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"ollama-loadbalancer/c/bridge"
)

// writeChatNDJSONFromResult — стриминговый ответ Ollama /api/chat по готовому
// результату: один чанк с content, затем финальный done.
func writeChatNDJSONFromResult(w http.ResponseWriter, r *http.Request, modelName string, result *bridge.InferenceResult) {
	// R493 (этап 7): общий писатель потока. Заголовки — те же, что у основного
	// NDJSON-пути (/api/generate): раньше здесь не было Cache-Control, и прокси
	// мог буферизовать поток; тело кадров не изменилось.
	sw, ok := newStreamWriterR493(w, r, "writeChatNDJSONFromResult", modelName, "application/x-ndjson")
	if !ok {
		return
	}
	sw.WriteHeader(http.StatusOK)

	emit := func(payload map[string]interface{}) {
		body, err := json.Marshal(payload)
		if err != nil {
			return
		}
		sw.Writef("%s\n", body)
		sw.Flush()
	}
	emit(map[string]interface{}{
		"model":      modelName,
		"created_at": time.Now().UTC().Format(time.RFC3339),
		"message":    map[string]string{"role": "assistant", "content": result.Output},
		"done":       false,
	})
	emit(map[string]interface{}{
		"model":       modelName,
		"created_at":  time.Now().UTC().Format(time.RFC3339),
		"message":     map[string]string{"role": "assistant", "content": ""},
		"done":        true,
		"done_reason": "stop",
	})
}

// writeOpenAISSEFromResult — стриминговый ответ OpenAI /v1/chat/completions по
// готовому результату: delta-чанк с содержимым, терминальный чанк finish и
// [DONE].
func writeOpenAISSEFromResult(w http.ResponseWriter, r *http.Request, modelName string, result *bridge.InferenceResult) {
	// R493 (этап 7): общий писатель потока (обрыв клиента виден диагностике).
	sw, ok := newStreamWriterR493(w, r, "writeOpenAISSEFromResult", modelName, "text/event-stream")
	if !ok {
		return
	}
	sw.WriteHeader(http.StatusOK)

	id := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	created := time.Now().Unix()

	emit := func(payload map[string]interface{}) {
		body, err := json.Marshal(payload)
		if err != nil {
			return
		}
		sw.Writef("data: %s\n\n", body)
		sw.Flush()
	}

	emit(map[string]interface{}{
		"id": id, "object": "chat.completion.chunk", "created": created, "model": modelName,
		"choices": []map[string]interface{}{{
			"index": 0,
			"delta": map[string]interface{}{"role": "assistant", "content": result.Output},
		}},
	})
	emit(map[string]interface{}{
		"id": id, "object": "chat.completion.chunk", "created": created, "model": modelName,
		"choices": []map[string]interface{}{{
			"index": 0, "delta": map[string]interface{}{}, "finish_reason": "stop",
		}},
	})
	sw.Writef("data: [DONE]\n\n")
	sw.Flush()
}

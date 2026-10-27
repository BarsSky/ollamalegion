// vision_stream_r248.go — R248/B3 (2026-10-27): стриминговый ответ для
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
func writeChatNDJSONFromResult(w http.ResponseWriter, modelName string, result *bridge.InferenceResult) {
	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)

	enc := json.NewEncoder(w)
	_ = enc.Encode(map[string]interface{}{
		"model":      modelName,
		"created_at": time.Now().UTC().Format(time.RFC3339),
		"message":    map[string]string{"role": "assistant", "content": result.Output},
		"done":       false,
	})
	if flusher != nil {
		flusher.Flush()
	}
	_ = enc.Encode(map[string]interface{}{
		"model":      modelName,
		"created_at": time.Now().UTC().Format(time.RFC3339),
		"message":    map[string]string{"role": "assistant", "content": ""},
		"done":       true,
		"done_reason": "stop",
	})
	if flusher != nil {
		flusher.Flush()
	}
}

// writeOpenAISSEFromResult — стриминговый ответ OpenAI /v1/chat/completions по
// готовому результату: delta-чанк с содержимым, терминальный чанк finish и
// [DONE].
func writeOpenAISSEFromResult(w http.ResponseWriter, modelName string, result *bridge.InferenceResult) {
	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	id := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	created := time.Now().Unix()

	emit := func(payload map[string]interface{}) {
		body, err := json.Marshal(payload)
		if err != nil {
			return
		}
		fmt.Fprintf(w, "data: %s\n\n", body)
		if flusher != nil {
			flusher.Flush()
		}
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
	fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

package balancer

// stream_abort_frames_r296.go — R296 (2026-10-29): КАДРЫ ЗАВЕРШЕНИЯ СТРИМА.
//
// ЖИВОЙ ДЕФЕКТ (Cline). При `docker kill` cppworker посреди стрима клиент получил
// ОДИН кадр, который одновременно сообщал причину и закрывал ход:
//
//	{"done":true,"done_reason":"truncated","error":"...balancer reloaded..."}
//
// Следствия два, и оба плохие:
//  1. `done:true` (для /v1 — `finish_reason`) означает «ход завершён», поэтому
//     клиент закрывал приём раньше, чем разбирал причину; в UI это выглядело
//     как «успешный, но пустой конец»;
//  2. сама причина была ЛОЖНОЙ: перезагрузки не было, воркер умер — а клиенту
//     советовали «retry», хотя retry в этот момент обречён.
//
// Здесь кадры собираются ОДНОЙ функцией (и тестируются по отдельности):
//   - сначала кадр-ПРИЧИНА, который ход НЕ закрывает (done:false /
//     finish_reason отсутствует);
//   - затем РОВНО ОДИН терминатор.
//
// Для `our_limit` причина клиенту не нужна (это наш буфер, а не сбой): отдаётся
// только терминатор с finish_reason=length/done_reason=length, как и раньше.

import (
	"encoding/json"
	"fmt"
	"time"
)

// abortFramesR296 — кадры завершения для обрыва стрима.
//
// originalPath — путь клиента ("/v1/chat/completions" = SSE, иначе NDJSON).
// Возвращает готовые к записи кадры в порядке отправки; последний кадр —
// терминатор. Для SSE терминатор НЕ включает `data: [DONE]` (его пишет
// вызывающий: маркер общий для всех путей, R252/R291).
func abortFramesR296(originalPath, model, abortReason string) [][]byte {
	frames := make([][]byte, 0, 2)

	needReason := abortReason != streamAbortOurLimit
	message := streamAbortClientMessage(abortReason)

	if originalPath == "/v1/chat/completions" {
		finishReason := "truncated"
		if abortReason == streamAbortOurLimit {
			finishReason = "length"
		}
		chunkID := fmt.Sprintf("trunc-%d", time.Now().UnixNano())
		created := time.Now().Unix()

		if needReason {
			// Причина: finish_reason ОТСУТСТВУЕТ (nil) — ход ещё не закрыт.
			reasonChunk := map[string]interface{}{
				"id":      chunkID,
				"object":  "chat.completion.chunk",
				"created": created,
				"model":   model,
				"choices": []interface{}{
					map[string]interface{}{
						"index":         0,
						"delta":         map[string]interface{}{},
						"finish_reason": nil,
					},
				},
				"error": map[string]interface{}{
					"message": message,
					"type":    "stream_truncated",
					"reason":  abortReason,
				},
			}
			if out, err := json.Marshal(reasonChunk); err == nil {
				frames = append(frames, []byte("data: "+string(out)+"\n\n"))
			}
		}

		terminal := map[string]interface{}{
			"id":      chunkID,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   model,
			"choices": []interface{}{
				map[string]interface{}{
					"index":         0,
					"delta":         map[string]interface{}{},
					"finish_reason": finishReason,
				},
			},
		}
		if out, err := json.Marshal(terminal); err == nil {
			frames = append(frames, []byte("data: "+string(out)+"\n\n"))
		}
		return frames
	}

	// NDJSON (/api/chat, /api/generate).
	doneReason := "truncated"
	if abortReason == streamAbortOurLimit {
		doneReason = "length"
	}
	createdAt := time.Now().UTC().Format(time.RFC3339)
	if needReason {
		reasonChunk := map[string]interface{}{
			"model":      model,
			"created_at": createdAt,
			"done":       false,
			"error":      message,
			"error_type": abortReason,
		}
		if out, err := json.Marshal(reasonChunk); err == nil {
			frames = append(frames, append(out, '\n'))
		}
	}
	terminal := map[string]interface{}{
		"model":       model,
		"created_at":  createdAt,
		"done":        true,
		"done_reason": doneReason,
	}
	if out, err := json.Marshal(terminal); err == nil {
		frames = append(frames, append(out, '\n'))
	}
	return frames
}

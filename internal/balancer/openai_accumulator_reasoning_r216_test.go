//go:build llama_stub

// openai_accumulator_reasoning_r216_test.go — R216 (2026-10-21).
//
// ЖИВОЙ ДЕФЕКТ (жалоба оператора). Не-стриминговый запрос через балансер
// возвращал размышления ПУСТЫМИ, хотя напрямую воркер отдаёт их в
// reasoning_content: замер на стенде — воркер 171 символ, через балансер 0.
// Причина: балансер собирает non-stream ответ из SSE-чанков
// (proxy_request_openai_auto_stream.go) и клал размышления только в `reasoning`
// (имя Ollama-поверхности). Клиенты /v1 (deepseek harness, OpenWebUI) читают
// `reasoning_content` и потому видели «размышления пропали».
package balancer

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestR216_AccumulatorKeepsReasoningContent(t *testing.T) {
	a := newOpenAIStreamAccumulator("Qwen3.8-27B-UD-Q4_K_M")
	a.addChunk(`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1234,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"думаю про 17*23"},"finish_reason":null}]}`)
	a.addChunk(`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1234,"model":"m","choices":[{"index":0,"delta":{"reasoning_content":" ... ещё"},"finish_reason":null}]}`)
	a.addChunk(`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1234,"model":"m","choices":[{"index":0,"delta":{"content":"391"},"finish_reason":"stop"}]}`)

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(a.toNonStreamResponse(false), &resp))

	choice := resp["choices"].([]interface{})[0].(map[string]interface{})
	msg := choice["message"].(map[string]interface{})

	rc, _ := msg["reasoning_content"].(string)
	assert.Equal(t, "думаю про 17*23 ... ещё", rc,
		"reasoning_content обязан быть в non-stream ответе: без него клиенты /v1 видят пустые размышления")
	// Совместимость с Ollama-клиентами: имя reasoning остаётся.
	assert.Equal(t, rc, msg["reasoning"])
	assert.Equal(t, "391", msg["content"], "видимый ответ не должен смешиваться с размышлениями")
	assert.Equal(t, "stop", choice["finish_reason"])
}

// Без размышлений поля не появляются (не выдумываем пустые ключи).
func TestR216_AccumulatorNoReasoningNoField(t *testing.T) {
	a := newOpenAIStreamAccumulator("gemma-4")
	a.addChunk(`{"choices":[{"index":0,"delta":{"content":"ок"},"finish_reason":"stop"}]}`)

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(a.toNonStreamResponse(false), &resp))
	msg := resp["choices"].([]interface{})[0].(map[string]interface{})["message"].(map[string]interface{})
	_, hasRC := msg["reasoning_content"]
	_, hasR := msg["reasoning"]
	assert.False(t, hasRC, "reasoning_content не должен появляться пустым")
	assert.False(t, hasR, "reasoning не должен появляться пустым")
}

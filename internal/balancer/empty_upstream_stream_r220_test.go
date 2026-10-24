//go:build llama_stub

// empty_upstream_stream_r220_test.go — R220 (2026-10-22).
//
// ЖИВОЙ ДЕФЕКТ. Узел раскладки умер посреди сессии: воркер убит abort'ом RPC
// (`ggml-rpc.cpp:570: Remote RPC server crashed or returned malformed response`),
// поток кончился «unexpected EOF» с 0 чанков. Балансер сформировал non-stream
// ответ из пустого аккумулятора: HTTP 200, message.content="", finish_reason="",
// id="" — оператор видел «успешный пустой ответ» вместо ошибки инфраструктуры.
//
// Контракт R220: пустой поток → тело явной ошибки (empty_upstream_stream), а не
// пустой 200.
package balancer

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestR220_EmptyUpstreamReturnsExplicitError(t *testing.T) {
	a := newOpenAIStreamAccumulator("gemma-4")
	assert.True(t, a.isEmpty(), "новый аккумулятор обязан считаться пустым")

	raw := a.orEmptyUpstreamError(errors.New("unexpected EOF"))
	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(raw, &resp))

	errObj, ok := resp["error"].(map[string]interface{})
	require.True(t, ok, "тело должно быть OpenAI-ошибкой: %s", string(raw))
	assert.Equal(t, "empty_upstream_stream", errObj["code"])
	assert.Equal(t, "upstream_error", errObj["type"])
	msg, _ := errObj["message"].(string)
	assert.True(t, strings.Contains(msg, "unexpected EOF"),
		"причина должна быть названа: %q", msg)
	assert.True(t, strings.Contains(msg, "rpc-server"),
		"подсказка оператору должна называть узел раскладки: %q", msg)
}

// Контроль: если данные были — аккумулятор не пустой, и обычный путь не меняется.
func TestR220_NonEmptyAccumulatorNotReportedAsEmpty(t *testing.T) {
	a := newOpenAIStreamAccumulator("gemma-4")
	a.addChunk(`{"choices":[{"delta":{"content":"ок"},"finish_reason":"stop"}]}`)
	assert.False(t, a.isEmpty())

	a2 := newOpenAIStreamAccumulator("gemma-4")
	a2.addChunk(`{"choices":[{"delta":{"reasoning_content":"думаю"},"finish_reason":null}]}`)
	assert.False(t, a2.isEmpty(), "одни размышления — это тоже данные, а не пустой поток")
}

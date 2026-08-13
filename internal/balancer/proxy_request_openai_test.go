package balancer

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// makeTestProxy создаёт минимальный *Proxy, достаточный для вызова
// proxyRequestOpenAIStreaming в unit-тестах. Использует реальный types.LoadBalancerConfig,
// но с дефолтами, чтобы getHeartbeatInterval/getStreamingIdleTimeout работали.
func makeTestProxy(heartbeatSec int) *Proxy {
	return &Proxy{
		config: &types.LoadBalancerConfig{
			Balancing: types.BalancingSettings{
				AdvancedTiming: types.AdvancedTimingConfig{
					HeartbeatIntervalSec: heartbeatSec,
				},
				StreamingIdleTimeout: 120,
			},
		},
	}
}

// (helper'ы newOpenAITestProxy / hijackableRecorder удалены — не нужны для unit-тестов
// writeChunkedFrame, см. конец файла.)

// TestProxyRequestOpenAIStreaming_HappyPath проверяет, что streaming-обёртка
// проксирует SSE-байты от бэкенда к клиенту БЕЗ трансляции форматов
// (в отличие от proxyRequestLlamaCpp, который транслирует SSE→NDJSON).
//
// Это критично для Roo Code / Cline / Continue.dev, которые ожидают OpenAI-формат
// и не понимают Ollama NDJSON.
func TestProxyRequestOpenAIStreaming_HappyPath(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)

		flusher, _ := w.(http.Flusher)
		chunks := []string{
			`data: {"id":"chat-1","object":"chat.completion.chunk","created":1700000000,"model":"qwen2.5","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}` + "\n\n",
			`data: {"id":"chat-1","object":"chat.completion.chunk","created":1700000000,"model":"qwen2.5","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}` + "\n\n",
			`data: {"id":"chat-1","object":"chat.completion.chunk","created":1700000000,"model":"qwen2.5","choices":[{"index":0,"delta":{"content":" world"},"finish_reason":null}` + "\n\n",
			`data: {"id":"chat-1","object":"chat.completion.chunk","created":1700000000,"model":"qwen2.5","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]` + "\n\n",
			`data: [DONE]` + "\n\n",
		}
		for _, c := range chunks {
			w.Write([]byte(c))
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer upstream.Close()

	req, _ := http.NewRequestWithContext(context.Background(), "POST", upstream.URL+"/v1/chat/completions", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("upstream request failed: %v", err)
	}

	clientRec := httptest.NewRecorder()
	clientReq := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	p := makeTestProxy(15)

	err = p.proxyRequestOpenAIStreaming(clientRec, clientReq, resp, "test-backend")
	if err != nil {
		t.Fatalf("proxyRequestOpenAIStreaming returned error: %v", err)
	}

	clientBody := clientRec.Body.String()
	expectedSubstrings := []string{
		`"role":"assistant"`,
		`"content":"Hello"`,
		`"content":" world"`,
		`"finish_reason":"stop"`,
		`[DONE]`,
	}
	for _, s := range expectedSubstrings {
		if !strings.Contains(clientBody, s) {
			t.Errorf("client body missing %q\nGot: %s", s, clientBody)
		}
	}

	if strings.Contains(clientBody, `"done":true`) {
		t.Errorf("client body should NOT contain Ollama NDJSON markers (no done:true)")
	}
}

// TestWriteChunkedFrame_EmptyData проверяет, что writeChunkedFrame с пустым
// data пишет финальный chunked terminator "0\r\n\r\n" (HTTP/1.1 spec).
//
// Это критично для strict clients (Open WebUI aiohttp): без терминатора
// они кидают "TransferEncodingError: 400, message='Not enough data to
// satisfy transfer length header.'" при чтении после EOF.
func TestWriteChunkedFrame_EmptyData(t *testing.T) {
	var buf bytes.Buffer
	bufrw := bufio.NewReadWriter(bufio.NewReader(&buf), bufio.NewWriter(&buf))

	if err := writeChunkedFrame(bufrw, nil); err != nil {
		t.Fatalf("writeChunkedFrame(nil) error: %v", err)
	}

	got := buf.String()
	want := "0\r\n\r\n"
	if got != want {
		t.Errorf("writeChunkedFrame(nil) = %q, want %q", got, want)
	}
}

// TestWriteChunkedFrame_NonEmptyData проверяет, что writeChunkedFrame
// оборачивает данные в правильный chunked format: "{hex_size}\r\n{data}\r\n".
func TestWriteChunkedFrame_NonEmptyData(t *testing.T) {
	var buf bytes.Buffer
	bufrw := bufio.NewReadWriter(bufio.NewReader(&buf), bufio.NewWriter(&buf))

	data := []byte("data: {\"x\":1}\n\n") // 15 bytes
	if err := writeChunkedFrame(bufrw, data); err != nil {
		t.Fatalf("writeChunkedFrame error: %v", err)
	}

	// Verify format: hex_size\r\n + data + \r\n
	// 15 bytes = 0xf in hex
	want := "f\r\ndata: {\"x\":1}\n\n\r\n"
	got := buf.String()
	if got != want {
		t.Errorf("writeChunkedFrame = %q, want %q", got, want)
	}
}

// TestProxyOpenAIStreamingEmptySSELinesBypass — unit-тест для логики
// "skip empty lines" из Round 35e. Симулирует main loop из
// proxyRequestOpenAIStreamingHijacked: набор строк от ReadBytes('\n')
// (включая пустые) → если строка это просто "\n" или пустая, skip.
//
// До фикса: writeChunkedFrame(bufrw, "\n") писал 1-байт chunk
// После фикса: такие строки фильтруются ДО writeChunkedFrame
func TestProxyOpenAIStreamingEmptySSELinesBypass(t *testing.T) {
	// Имитируем main loop: readBytes отдаёт строки включая пустые.
	lines := [][]byte{
		[]byte("data: {\"x\":1}\n"),
		[]byte("\n"), // empty line между events
		[]byte("data: {\"x\":2}\n"),
		[]byte("\n"),
		[]byte("data: [DONE]\n"),
		[]byte("\n"),
	}

	var buf bytes.Buffer
	bufrw := bufio.NewReadWriter(bufio.NewReader(&buf), bufio.NewWriter(&buf))

	// Имитация Round 35e фикса: skip empty lines.
	for _, line := range lines {
		if len(line) == 0 || (len(line) == 1 && line[0] == '\n') {
			continue // skip empty line
		}
		// Append \n для восстановления SSE event boundary (как в hijack main loop).
		if isOpenAISSEDataLine(line) {
			line = append(line, '\n')
		}
		if err := writeChunkedFrame(bufrw, line); err != nil {
			t.Fatalf("writeChunkedFrame: %v", err)
		}
	}
	// Terminator
	if err := writeChunkedFrame(bufrw, nil); err != nil {
		t.Fatalf("writeChunkedFrame(nil): %v", err)
	}

	got := buf.String()
	// Verify: должно быть 3 data chunks + terminator, без 1-байт \n chunks.
	// Должны присутствовать chunks:
	// - "15\r\ndata: {\"x\":1}\n\n\r\n" (0x15 = 21 байт = 13+1+1+1+1+1+1+1+1... "data: {...}" = 13 символов + \n + \n = 15 байт)
	// - "15\r\ndata: {\"x\":2}\n\n\r\n"
	// - "f\r\ndata: [DONE]\n\n\r\n" (0xf = 15 байт)
	// - "0\r\n\r\n" (terminator)
	if bytes.Contains([]byte(got), []byte("1\r\n\n\r\n")) {
		t.Errorf("found 1-byte \\n chunk in output — bug not fixed:\n%s", got)
	}
	// Проверим что нет size=1 (hex "1")
	pos := 0
	chunks := 0
	emptyChunks := 0
	for pos < len(got) {
		crlf := strings.Index(got[pos:], "\r\n")
		if crlf < 0 {
			break
		}
		sizeLine := got[pos : pos+crlf]
		size, err := strconv.ParseInt(sizeLine, 16, 64)
		if err != nil {
			t.Fatalf("bad chunk size at pos %d: %q", pos, sizeLine)
		}
		if size == 0 {
			break
		}
		chunks++
		if size == 1 {
			emptyChunks++
		}
		pos = pos + crlf + 2 + int(size) + 2
	}
	if emptyChunks > 0 {
		t.Errorf("found %d empty (1-byte) chunks — should be 0 after Round 35e fix", emptyChunks)
	}
	if chunks != 3 {
		t.Errorf("expected 3 data chunks, got %d\noutput:\n%s", chunks, got)
	}
}

// TestProxyRequestOpenAIStreaming_ClientDisconnect проверяет, что если клиент
// отвалился (закрыл соединение), proxy корректно завершается без утечки горутин.
func TestProxyRequestOpenAIStreaming_ClientDisconnect(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for i := 0; i < 100; i++ {
			fmt.Fprintf(w, `data: {"choices":[{"index":0,"delta":{"content":"tok%d"}}]}`+"\n\n", i)
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(50 * time.Millisecond)
		}
	}))
	defer upstream.Close()

	req, _ := http.NewRequestWithContext(context.Background(), "POST", upstream.URL+"/v1/chat/completions", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("upstream request failed: %v", err)
	}

	clientCtx, clientCancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer clientCancel()
	clientReq := httptest.NewRequest("POST", "/v1/chat/completions", nil).WithContext(clientCtx)
	clientRec := httptest.NewRecorder()
	p := makeTestProxy(15)

	start := time.Now()
	err = p.proxyRequestOpenAIStreaming(clientRec, clientReq, resp, "test-backend")
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("proxyRequestOpenAIStreaming returned error: %v", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("proxy took too long (%v) after client disconnect, expected < 2s", elapsed)
	}
}

// TestShouldFilterLlamaCppContent проверяет фильтрацию служебных токенов.
// Без этого Cline/Roo получают "мусорный" content (Gemma <end_of_turn>,
// Llama3 <|eot_id|> и т.д.) и интерпретируют его как tool-call-like вывод
// → "Invalid API Response".
func TestShouldFilterLlamaCppContent(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		expected bool
	}{
		// Служебные токены Gemma
		{"gemma_end_of_turn", "<end_of_turn>", true},
		{"gemma_end_of_turn_ws", "  <end_of_turn>  ", true},
		{"gemma_start_of_turn", "<start_of_turn>", true},

		// Служебные токены Llama
		{"llama_eot_id", "<|eot_id|>", true},
		{"llama_eot", "<|eot|>", true},
		{"llama_im_end", "<|im_end|>", true},
		{"llama_im_start", "<|im_start|>", true},
		{"llama_bos", "<bos>", true},
		{"llama_eos", "<eos>", true},
		{"llama_endoftext", "<endoftext>", true},
		{"llama_endoftext_pipe", "<|endoftext|>", true},
		{"llama_begin_of_text", "<|begin_of_text|>", true},
		{"llama_end_of_text", "<|end_of_text|>", true},
		{"llama_header_start", "<|start_header_id|>", true},
		{"llama_header_end", "<|end_header_id|>", true},

		// Префиксы (служебный токен + обычный текст)
		{"gemma_eot_prefix", "<end_of_turn>hello", true},
		{"llama_eot_prefix", "<|eot_id|>ok", true},

		// Обычный текст — НЕ фильтруем
		{"plain_text", "hello", false},
		{"plain_with_period", "Hello, world!", false},
		{"code_block", "function() { return 1; }", false},
		{"empty", "", false},
		{"whitespace_only", "   ", false},

		// Угловые случаи
		{"angle_bracket_text", "<user_input>", false},  // не служебный токен
		{"partial_token", "<end_of", false},             // неполный токен — не фильтруем
		{"eot_in_middle", "hello<|eot_id|>world", true}, // служебный токен в середине — фильтруем
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldFilterLlamaCppContent(tt.content)
			if got != tt.expected {
				t.Errorf("shouldFilterLlamaCppContent(%q) = %v, want %v",
					tt.content, got, tt.expected)
			}
		})
	}
}

// TestFilterOpenAIStreamingLine проверяет, что filterOpenAIStreamingLine
// корректно отфильтровывает чанки со служебными токенами.
func TestFilterOpenAIStreamingLine(t *testing.T) {
	tests := []struct {
		name          string
		input         string
		mustNotContain []string // строки, которые НЕ должны быть в результате
		mustContain    []string // строки, которые ДОЛЖНЫ быть в результате (если не пусто)
	}{
		{
			name: "gemma_end_of_turn",
			input: `data: {"choices":[{"index":0,"delta":{"content":"<end_of_turn>"}}]}` + "\n\n",
			mustNotContain: []string{"<end_of_turn>", `"content":"<end_of_turn>"`},
			mustContain:    []string{`"choices"`, `"delta"`},
		},
		{
			name: "llama_eot",
			input: `data: {"choices":[{"index":0,"delta":{"content":"<|eot_id|>"}}]}` + "\n\n",
			mustNotContain: []string{"<|eot_id|>", "eot_id"},
			mustContain:    []string{`"choices"`},
		},
		// Одиночный "<" — это НЕ служебный токен, а легитимный символ content
		// (часть HTML/кода/XML/математики). Фильтр должен его пропустить без изменений.
		// Раньше был ошибочный кейс "gemma_split_chars", который ожидал, что "<"
		// будет отфильтрован — это приводило бы к потере символа "меньше" в коде.
		{
			name: "single_angle_bracket",
			input: `data: {"choices":[{"index":0,"delta":{"content":"<"}}]}` + "\n\n",
			mustNotContain: []string{},
			mustContain:    []string{`"content":"<"`, `"choices"`, `"delta"`},
		},
		// Вкрапление служебного токена в середину обычного текста —
		// весь чанк фильтруется, даже если полезного текста больше, чем мусора.
		// Это нормальное поведение: одиночные символы Gemma/Llama эмитят
		// посимвольно, и клиенту всё равно придёт финальный нормальный чанк.
		{
			name: "eot_token_embedded_in_text",
			input: `data: {"choices":[{"index":0,"delta":{"content":"hello<|eot_id|>world"}}]}` + "\n\n",
			mustNotContain: []string{"<|eot_id|>", `"content":"hello<|eot_id|>world"`},
			mustContain:    []string{`"choices"`, `"delta"`},
		},
		{
			name: "normal_text",
			input: `data: {"choices":[{"index":0,"delta":{"content":"hello"}}]}` + "\n\n",
			mustNotContain: []string{},
			mustContain:    []string{`"content":"hello"`},
		},
		{
			name: "done_marker",
			input: `data: [DONE]` + "\n\n",
			mustNotContain: []string{},
			mustContain:    []string{`[DONE]`},
		},
		{
			name: "role_only_chunk",
			input: `data: {"choices":[{"index":0,"delta":{"role":"assistant"}}]}` + "\n\n",
			mustNotContain: []string{},
			mustContain:    []string{`"role":"assistant"`},
		},
		{
			name: "comment_line",
			input: ": keepalive" + "\n\n",
			mustNotContain: []string{},
			mustContain:    []string{": keepalive"},
		},
		{
			name: "finish_reason",
			input: `data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n",
			mustNotContain: []string{},
			mustContain:    []string{`"finish_reason":"stop"`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filtered, _ := filterOpenAIStreamingLine([]byte(tt.input))
			result := string(filtered)
			for _, s := range tt.mustNotContain {
				if strings.Contains(result, s) {
					t.Errorf("filtered output should NOT contain %q\nInput:    %q\nFiltered: %q",
						s, tt.input, result)
				}
			}
			for _, s := range tt.mustContain {
				if !strings.Contains(result, s) {
					t.Errorf("filtered output should contain %q\nInput:    %q\nFiltered: %q",
						s, tt.input, result)
				}
			}
		})
	}
}

// TestProxyRequestOpenAIStreaming_FilterGemmaTokens — интеграционный тест:
// upstream возвращает смесь из нормального контента и Gemma-токенов <end_of_turn>.
// Proxy должен отфильтровать служебные токены, оставив клиенту только нормальный
// текст "hello world".
func TestProxyRequestOpenAIStreaming_FilterGemmaTokens(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		// Имитируем реальный стрим Gemma: нормальный текст, потом служебный <end_of_turn>,
		// потом опять нормальный текст (как дублирует реальный cppworker).
		chunks := []string{
			`data: {"id":"chat-1","object":"chat.completion.chunk","created":1700000000,"model":"gemma-4","choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":null}` + "\n\n",
			`data: {"id":"chat-1","object":"chat.completion.chunk","created":1700000000,"model":"gemma-4","choices":[{"index":0,"delta":{"content":" world"},"finish_reason":null}` + "\n\n",
			// Служебные токены (должны быть отфильтрованы):
			`data: {"id":"chat-1","object":"chat.completion.chunk","created":1700000000,"model":"gemma-4","choices":[{"index":0,"delta":{"content":"<end_of_turn>"}}]}` + "\n\n",
			`data: {"id":"chat-1","object":"chat.completion.chunk","created":1700000000,"model":"gemma-4","choices":[{"index":0,"delta":{"content":"<|eot_id|>"}}]}` + "\n\n",
			// Финальный чанк:
			`data: {"id":"chat-1","object":"chat.completion.chunk","created":1700000000,"model":"gemma-4","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n",
			`data: [DONE]` + "\n\n",
		}
		for _, c := range chunks {
			w.Write([]byte(c))
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer upstream.Close()

	req, _ := http.NewRequestWithContext(context.Background(), "POST", upstream.URL+"/v1/chat/completions", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("upstream request failed: %v", err)
	}

	clientRec := httptest.NewRecorder()
	clientReq := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	p := makeTestProxy(15)

	err = p.proxyRequestOpenAIStreaming(clientRec, clientReq, resp, "test-backend")
	if err != nil {
		t.Fatalf("proxyRequestOpenAIStreaming returned error: %v", err)
	}

	clientBody := clientRec.Body.String()

	// Нормальный контент должен быть
	mustContain := []string{`"content":"hello"`, `"content":" world"`, `"finish_reason":"stop"`, `[DONE]`}
	for _, s := range mustContain {
		if !strings.Contains(clientBody, s) {
			t.Errorf("client body missing %q\nGot: %s", s, clientBody)
		}
	}

	// Служебные токены НЕ должны попасть к клиенту
	mustNotContain := []string{`<end_of_turn>`, `<|eot_id|>`, `"content":"<"`}
	for _, s := range mustNotContain {
		if strings.Contains(clientBody, s) {
			t.Errorf("client body should NOT contain %q (служебный токен Gemma)\nGot: %s",
				s, clientBody)
		}
	}
}
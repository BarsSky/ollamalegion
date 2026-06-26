// streaming_flush_reorder_test.go — регрессионные тесты на flush streaming-ответов.
//
// Проблема (2026-06-26): на быстрых моделях (Qwen3.6-35B-A3B, Llama-3.1-70B)
// балансер не вызывал http.Flusher.Flush() после записи финального
// SSE-чанка (data: [DONE]\n\n) и после NDJSON done-чанка. Из-за этого
// клиент (aiohttp, httpx, requests с stream=True) получал:
//   - последние content-чанки
//   - [DONE] / done:true
//   - TCP FIN без полного chunked terminator
// И парсил chunked-encoding с ошибкой:
//
//   Response payload is not completed:
//   <TransferEncodingError: 400, message='Not enough data to satisfy transfer length header.'>
//
// Тесты проверяют, что balancer.Flush() и balancer.NewFlushWriter() корректно
// вызывают http.Flusher.Flush() после каждой записи в ResponseWriter.
// Используется mock-upstream, который НЕ flush'ит между чанками — имитируя
// ситуацию, когда cppworker под нагрузкой буферизует вывод.

//go:build llama_stub
// +build llama_stub

package tests

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ollama-loadbalancer/internal/balancer"
)

// TestFlushHelper_FlushesHTTPResponseWriter проверяет, что balancer.Flush()
// вызывает http.Flusher.Flush() на http.ResponseWriter.
func TestFlushHelper_FlushesHTTPResponseWriter(t *testing.T) {
	rec := &flushTrackingWriter{
		header: http.Header{},
		buf:    bytes.Buffer{},
		flushed: 0,
	}

	// Записываем данные и flush'им.
	rec.Write([]byte("chunk-1"))
	balancer.Flush(rec)
	rec.Write([]byte("chunk-2"))
	balancer.Flush(rec)
	rec.Write([]byte("chunk-3"))
	balancer.Flush(rec)

	if rec.flushed != 3 {
		t.Fatalf("expected 3 flushes, got %d", rec.flushed)
	}
	if rec.buf.String() != "chunk-1chunk-2chunk-3" {
		t.Fatalf("expected chunks in order, got %q", rec.buf.String())
	}
}

// TestFlushWriter_FlushesAfterEachWrite проверяет, что NewFlushWriter оборачивает
// http.ResponseWriter так, что http.Flusher.Flush() вызывается после каждого Write.
func TestFlushWriter_FlushesAfterEachWrite(t *testing.T) {
	rec := &flushTrackingWriter{
		header: http.Header{},
		buf:    bytes.Buffer{},
		flushed: 0,
	}

	fw := balancer.NewFlushWriter(rec)

	// Через fw.Write каждый Write автоматически триггерит Flush.
	fw.Write([]byte("a"))
	fw.Write([]byte("b"))
	fw.Write([]byte("c"))

	if rec.flushed != 3 {
		t.Fatalf("expected 3 flushes (one per Write), got %d", rec.flushed)
	}
	if rec.buf.String() != "abc" {
		t.Fatalf("expected %q, got %q", "abc", rec.buf.String())
	}
}

// TestStreaming_NoFlushUpstream_ClientGetsAllData — главный регрессионный тест.
//
// Сценарий: upstream шлёт 100 чанков БЕЗ flush между ними, потом [DONE],
// и закрывает TCP. Прокси через balancer.NewFlushWriter должен flush'ить
// каждый чанк клиенту. Без flush клиент получает данные одним пакетом
// или теряет последние чанки → aiohttp / httpx парсят chunked с ошибкой.
//
// Здесь мы тестируем уровень stream_flush.go helper'а — что NewFlushWriter
// делает Flush после каждого io.Copy chunk'а (chunk в данном случае —
// размер буфера io.Copy, не SSE chunk). Это всё равно проверяет, что
// balancer.Flush работает корректно и клиент видит данные.
func TestStreaming_NoFlushUpstream_ClientGetsAllData(t *testing.T) {
	const numChunks = 100

	// Mock upstream: пишет 100 чанков одним блоком БЕЗ flush.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		var buf bytes.Buffer
		for i := 0; i < numChunks; i++ {
			chunk := map[string]interface{}{
				"id":      fmt.Sprintf("chunk-%d", i),
				"object":  "chat.completion.chunk",
				"created": 1,
				"model":   "test-model",
				"choices": []interface{}{
					map[string]interface{}{
						"index":         0,
						"delta":         map[string]interface{}{"content": fmt.Sprintf("%d ", i)},
						"finish_reason": nil,
					},
				},
			}
			data, _ := json.Marshal(chunk)
			buf.WriteString(fmt.Sprintf("data: %s\n\n", data))
		}
		buf.WriteString("data: [DONE]\n\n")
		// Один Write — без flush между чанками.
		w.Write(buf.Bytes())
	}))
	defer upstream.Close()

	// Прокси с flush через NewFlushWriter.
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req, err := http.NewRequest(r.Method, upstream.URL+r.URL.Path, r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		for k, vs := range r.Header {
			for _, v := range vs {
				req.Header.Add(k, v)
			}
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()

		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)

		// balancer.NewFlushWriter flush'ит после каждого Write.
		// io.Copy вызывает Write много раз (чанками по ~32KB).
		// Каждый Write → Flush. Это гарантирует, что клиент получит данные
		// не дожидаясь закрытия TCP.
		fw := balancer.NewFlushWriter(w)
		io.Copy(fw, resp.Body)
		balancer.Flush(w)
	}))
	defer proxy.Close()

	// Клиент: читает через bufio.Scanner, как aiohttp.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", proxy.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"test-model","stream":true}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("client do: %v", err)
	}
	defer resp.Body.Close()

	// Читаем через bufio.Scanner.
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0), 64*1024)

	gotDataChunks := 0
	gotDone := false

	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data:") {
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "[DONE]" {
				gotDone = true
				break
			}
			gotDataChunks++
		}
	}

	if err := scanner.Err(); err != nil {
		// ВОТ ЭТО И ЕСТЬ TransferEncodingError на стороне клиента в проде.
		t.Fatalf("scanner error (TransferEncodingError): %v", err)
	}

	if !gotDone {
		t.Fatalf("did not receive [DONE] marker")
	}
	if gotDataChunks != numChunks {
		t.Fatalf("expected %d chunks, got %d (TransferEncodingError scenario)", numChunks, gotDataChunks)
	}
	t.Logf("OK: received all %d chunks + [DONE] in correct order", gotDataChunks)
}

// flushTrackingWriter — http.ResponseWriter, который считает вызовы Flush().
// Нужен для тестирования, что balancer.Flush() реально вызывает http.Flusher.Flush().
type flushTrackingWriter struct {
	header  http.Header
	buf     bytes.Buffer
	flushed int
}

func (f *flushTrackingWriter) Header() http.Header {
	return f.header
}

func (f *flushTrackingWriter) Write(p []byte) (int, error) {
	return f.buf.Write(p)
}

func (f *flushTrackingWriter) WriteHeader(statusCode int) {
	// no-op
}

func (f *flushTrackingWriter) Flush() {
	f.flushed++
}
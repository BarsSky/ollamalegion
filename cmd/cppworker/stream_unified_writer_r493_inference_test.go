//go:build llama_stub

// stream_unified_writer_r493_inference_test.go — этап 7 (R493): обрыв клиента
// на ИНФЕРЕНС-поверхностях обязан попадать в диагностику.
//
// ЗАЧЕМ ОТДЕЛЬНЫЙ ТЕСТ. Статические писатели проверяются прямым вызовом
// (stream_unified_writer_r493_test.go), но основные потоки (/api/generate,
// /api/ollama/generate, /v1/completions, /v1/chat/completions) вызываются из
// HTTP-обработчиков, и обрыв клиента там виден только через ошибку записи.
// Поэтому тест идёт по живому пути: httptest-сервер + обёртка ResponseWriter,
// которая после N байт отвечает «broken pipe», и проверка, что writer записал
// snapshot (иначе оператор видит последний УСПЕШНЫЙ поток, а NaN-healer не
// узнаёт о сорванном).
//
// До правки этапа 7 у этих четырёх путей snapshot не писался вовсе — тесты
// падали на getLastStreamInfo() == nil.
package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"ollama-loadbalancer/c/bridge"
	"ollama-loadbalancer/internal/cppbackend"
)

// cutClientWriter — клиент, который отвалился после failAfter байт.
// Flusher обязателен: без него обработчик просто не начнёт поток.
type cutClientWriter struct {
	http.ResponseWriter
	failAfter int
	written   int
}

func (c *cutClientWriter) Write(p []byte) (int, error) {
	if c.written >= c.failAfter {
		return 0, os.ErrClosed
	}
	n, err := c.ResponseWriter.Write(p)
	c.written += n
	return n, err
}

func (c *cutClientWriter) Flush() {
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// setupWorkerServerWithCutClient поднимает воркер с production-роутером, но
// каждый ответ проходит через cutClientWriter: первая же запись за пределами
// failAfter байт — «обрыв».
func setupWorkerServerWithCutClient(t *testing.T, failAfter int) *httptest.Server {
	t.Helper()
	dir := t.TempDir()
	dummyPath := filepath.Join(dir, "dummy.gguf")
	if err := os.WriteFile(dummyPath, []byte("stub"), 0644); err != nil {
		t.Fatalf("failed to create dummy gguf: %v", err)
	}
	cfg := cppbackend.Config{
		ModelsDir:            dir,
		DefaultCtxSize:       4096,
		DefaultBatchSize:     512,
		DefaultGPULayers:     0,
		DefaultFlashAttnType: -1,
		DefaultNUMA:          false,
		DefaultUseMmap:       true,
	}
	backend = cppbackend.NewBackend(cfg)
	if err := backend.Init(); err != nil {
		t.Fatalf("backend init failed: %v", err)
	}
	router := setupRouter()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		router.ServeHTTP(&cutClientWriter{ResponseWriter: w, failAfter: failAfter}, r)
	}))
	t.Cleanup(func() {
		srv.Close()
		waitAsyncWorkBeforeClose()
		backend.Close()
	})
	return srv
}

// postStreamAndAbort отправляет стриминговый запрос и дожидается обрыва:
// читать тело не нужно (оно заведомо обрезано) — важно, что обработчик получил
// ошибку записи.
func postStreamAndAbort(t *testing.T, srv *httptest.Server, path string, body map[string]interface{}) {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	resp, err := http.Post(srv.URL+path, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	// Читаем — но результат не важен: поток обрезан по построению.
	_, _ = ioCopyDiscard(resp.Body)
	_ = resp.Body.Close()
}

func ioCopyDiscard(rc interface{ Read([]byte) (int, error) }) (int64, error) {
	buf := make([]byte, 1024)
	var total int64
	for {
		n, err := rc.Read(buf)
		total += int64(n)
		if err != nil {
			return total, nil
		}
	}
}

// TestR493r_NoSnapshotOnHealthyStream — контроль к предыдущему тесту: если
// клиент читает нормально, ложного «обрыва» в диагностике быть НЕ должно.
// Без этого контрольного случая тест на обрыв проходил бы и при писателе,
// который пишет snapshot на каждый поток (а такие warn'ы — шум).
func TestR493r_NoSnapshotOnHealthyStream(t *testing.T) {
	prev := bridge.SetStubEmitTokens([]string{"раз", "два", "три"})
	defer bridge.SetStubEmitTokens(prev)

	resetLastStreamInfo(t)
	srv := setupWorkerServerWithCutClient(t, 1<<20) // обрыва нет: лимит не достигается
	postStreamAndAbort(t, srv, "/api/generate", map[string]interface{}{
		"model": "dummy", "stream": true, "prompt": "hi",
	})
	if info := getLastStreamInfo(); info != nil {
		t.Fatalf("на здоровом потоке записан ложный обрыв: handler=%q reason=%q", info.Handler, info.Reason)
	}
}

// TestR493r_InferenceStreamsRecordDisconnect — таблица по четырём поверхностям.
func TestR493r_InferenceStreamsRecordDisconnect(t *testing.T) {
	prev := bridge.SetStubEmitTokens([]string{"раз", "два", "три", "четыре", "пять"})
	defer bridge.SetStubEmitTokens(prev)

	cases := []struct {
		name    string
		path    string
		body    map[string]interface{}
		handler string
	}{
		{"ollama /api/generate", "/api/generate", map[string]interface{}{
			"model": "dummy", "stream": true, "prompt": "hi",
		}, "writeStreamResponse"},
		{"ollama /api/ollama/generate", "/api/ollama/generate", map[string]interface{}{
			"model": "dummy", "stream": true, "prompt": "hi",
		}, "writeOllamaStream"},
		{"openai /v1/completions", "/v1/completions", map[string]interface{}{
			"model": "dummy", "stream": true, "prompt": "hi",
		}, "writeOpenAICompletionStream"},
		{"openai /v1/chat/completions", "/v1/chat/completions", map[string]interface{}{
			"model": "dummy", "stream": true,
			"messages": []map[string]string{{"role": "user", "content": "hi"}},
		}, "writeOpenAIChatStream"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetLastStreamInfo(t)
			// Пять байт хватает на первые заголовки/комментарий, дальше — обрыв.
			srv := setupWorkerServerWithCutClient(t, 5)
			postStreamAndAbort(t, srv, tc.path, tc.body)

			info := getLastStreamInfo()
			if info == nil {
				t.Fatalf("%s: обрыв клиента не записан в диагностику (getLastStreamInfo() == nil) — "+
					"оператор увидит последний успешный поток, RecordStreamBreak не вызван", tc.path)
			}
			if info.Handler != tc.handler {
				t.Errorf("%s: handler = %q, want %q", tc.path, info.Handler, tc.handler)
			}
			if info.WriteErrors == 0 {
				t.Errorf("%s: write_errors = 0 при обрыве клиента", tc.path)
			}
		})
	}
}

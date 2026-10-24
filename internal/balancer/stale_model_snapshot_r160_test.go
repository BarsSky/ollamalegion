//go:build llama_stub

// stale_model_snapshot_r160_test.go — R160 (2026-10-17): «модель не найдена»
// от воркера не превращается в 502.
//
// ЖИВОЙ ДЕФЕКТ. Балансер считал модель загруженной на доноре (устаревший снимок
// метрик), проксировал запрос, донор отвечал
//
//	HTTP 500 {"...":"chat generate failed: model gemma-4-E4B-it-Q4_K_M not found"}
//
// а клиент за 13 мс получал 502 «upstream returned HTTP 500: ...» — код, который
// для OpenAI-клиентов означает «сервер сломан», а не «модель поднимается».
//
// КОНТРФАКТУАЛЬНОСТЬ: на старом коде первый тест падал бы с 502 (балансер
// оборачивал любой 5xx), а второй — нет: обычная внутренняя ошибка обязана
// остаться 502 и не выдавать себя за отсутствие модели.
package balancer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// upstreamErrProxy — прокси с одним llama.cpp-бэкендом, который отвечает заданной
// ошибкой (статус + тело) на любой запрос.
func upstreamErrProxy(t *testing.T, status int, body string) (*Proxy, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse stub url: %v", err)
	}
	port, _ := strconv.Atoi(u.Port())

	p := &Proxy{
		config:     &types.LoadBalancerConfig{},
		backends:   map[string]*BackendState{},
		metricsMgr: NewMetricsManager(),
		client:     &http.Client{},
	}
	p.backends["node-1"] = &BackendState{Backend: &types.Backend{
		ID: "node-1", Host: u.Hostname(), CppWorkerPort: port,
		Status: types.StatusHealthy, Type: types.BackendTypeLlamaCpp,
	}}
	return p, srv
}

// TestR160_ModelNotFoundFromWorkerIs503 — главный контрпример: воркер не знает
// модель → 503 «повтори», а не 502 «шлюз сломан».
func TestR160_ModelNotFoundFromWorkerIs503(t *testing.T) {
	upstream := `{"error":"chat generate failed: model gemma-4-E4B-it-Q4_K_M not found","code":0}`
	p, _ := upstreamErrProxy(t, http.StatusInternalServerError, upstream)

	// Модель кладём в контекст так же, как это делает обработчик: без неё
	// транспорт не знает, о какой модели речь (реальный путь её всегда кладёт).
	body := `{"model":"gemma-4-E4B-it-Q4_K_M","messages":[],"stream":false}`
	req := httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(body)).
		WithContext(context.WithValue(context.Background(), modelContextKey, "gemma-4-E4B-it-Q4_K_M"))
	w := httptest.NewRecorder()

	if err := p.proxyRequestLlamaCppNonStream(w, req, "node-1", []byte(body)); err != nil {
		t.Fatalf("proxyRequestLlamaCppNonStream: %v", err)
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("статус = %d, want 503: устаревший снимок обязан давать «повтори», а не 502", w.Code)
	}
	if ra := w.Header().Get("Retry-After"); ra == "" {
		t.Error("нет Retry-After: клиент (Cline/OpenWebUI) пойдёт в busy-loop")
	}
	respBody := w.Body.String()
	if !strings.Contains(respBody, "stale") && !strings.Contains(respBody, "not") {
		t.Errorf("тело не объясняет причину: %s", respBody)
	}
	if strings.Contains(respBody, "upstream returned HTTP 500") {
		t.Errorf("тело повторяет сырой ответ воркера вместо объяснения: %s", respBody)
	}
}

// TestR160_GenericInternalErrorStays502 — обратная сторона: обычная внутренняя
// ошибка воркера (не про модель) остаётся 502. Иначе мы бы «объясняли» клиенту
// отсутствие модели там, где проблема в другом.
func TestR160_GenericInternalErrorStays502(t *testing.T) {
	p, _ := upstreamErrProxy(t, http.StatusInternalServerError, `{"error":"llama_decode failed: internal assert"}`)

	req := httptest.NewRequest(http.MethodPost, "/api/chat",
		strings.NewReader(`{"model":"m","messages":[]}`))
	w := httptest.NewRecorder()

	if err := p.proxyRequestLlamaCppNonStream(w, req, "node-1", []byte(`{"model":"m","messages":[]}`)); err != nil {
		t.Fatalf("proxyRequestLlamaCppNonStream: %v", err)
	}
	if w.Code != http.StatusBadGateway {
		t.Errorf("статус = %d, want 502: внутренняя ошибка воркера не должна выдаваться за отсутствие модели", w.Code)
	}
}

// TestR160_IsUpstreamModelAbsent — порог распознавания. Слишком широкий список
// маркеров заставил бы балансер перезапускать загрузку на чужих ошибках.
func TestR160_IsUpstreamModelAbsent(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"живой ответ донора", 500, `{"error":"chat generate failed: model gemma-4-E4B-it-Q4_K_M not found","code":0}`, true},
		{"model_not_loaded", 500, `{"error":"model_not_loaded"}`, true},
		{"model is not loaded", 503, `{"error":"model is not loaded and auto-load failed"}`, true},
		{"4xx про параметры", 400, `{"error":"model foo: n_ctx too large"}`, false},
		{"внутренняя ошибка", 500, `{"error":"llama_decode failed"}`, false},
		{"ошибка без слова model", 500, `{"error":"slot not found"}`, false},
		{"2xx с текстом про модель", 200, `{"error":"model not found"}`, false},
	}
	for _, c := range cases {
		if got := isUpstreamModelAbsent(c.status, []byte(c.body)); got != c.want {
			t.Errorf("%s: isUpstreamModelAbsent(%d, %s) = %v, want %v", c.name, c.status, c.body, got, c.want)
		}
	}
}

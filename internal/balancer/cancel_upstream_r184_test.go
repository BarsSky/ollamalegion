//go:build llama_stub

// cancel_upstream_r184_test.go — R184 (2026-10-19).
//
// НАЙДЕНО ЗАМЕРОМ (матрица R180 прошла 16/16, но время выдало дефект): запрос после
// обрыва клиента занимал 100 429 мс. Балансер при обрыве закрывал только СВОЁ чтение
// upstream, а воркер продолжал генерацию и держал единственный слот.
//
// Здесь фиксируется контракт отмены: воркер получает POST /api/cancel с моделью и
// токеном; вызов асинхронный, недоступный воркер не роняет обработчик.
package balancer

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// fakeBackendR184 — зарегистрировать в прокси бэкенд, указывающий на тест-сервер.
func fakeBackendR184(t *testing.T, p *Proxy, id string, srvURL string, token string) {
	t.Helper()
	hostPort := strings.TrimPrefix(srvURL, "http://")
	host := hostPort
	port := 80
	if i := strings.LastIndex(hostPort, ":"); i > 0 {
		host = hostPort[:i]
		if n, err := strconv.Atoi(hostPort[i+1:]); err == nil {
			port = n
		}
	}
	p.mu.Lock()
	if p.backends == nil {
		p.backends = map[string]*BackendState{}
	}
	p.backends[id] = &BackendState{Backend: &types.Backend{
		ID: id, Type: types.BackendTypeLlamaCpp, Host: host, CppWorkerPort: port, CppWorkerApiToken: token,
	}}
	p.mu.Unlock()
}

// TestR184_CancelUpstream_SendsModelAndToken — что именно уходит воркеру.
func TestR184_CancelUpstream_SendsModelAndToken(t *testing.T) {
	type captured struct {
		path  string
		token string
		body  map[string]interface{}
	}
	got := make(chan captured, 4)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]interface{}
		_ = json.Unmarshal(raw, &body)
		got <- captured{path: r.URL.Path, token: r.Header.Get("X-API-Token"), body: body}
		_, _ = w.Write([]byte(`{"cancelled":1,"by":"model"}`))
	}))
	t.Cleanup(srv.Close)

	p := &Proxy{}
	fakeBackendR184(t, p, "llama_test", srv.URL, "tok-184")
	p.cancelUpstreamGeneration("llama_test", "gemma-4-E4B-it-Q4_K_M", "client_cancel")

	select {
	case c := <-got:
		if c.path != "/api/cancel" {
			t.Errorf("путь = %q, want /api/cancel", c.path)
		}
		if c.token != "tok-184" {
			t.Errorf("X-API-Token = %q, want tok-184 (без токена воркер ответит 401 и генерация не встанет)", c.token)
		}
		if c.body["model"] != "gemma-4-E4B-it-Q4_K_M" {
			t.Errorf("model = %v, want имя модели: без него воркер отменит ВСЕ генерации", c.body["model"])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("воркер не получил запрос отмены — слот останется занят до конца генерации")
	}
}

// TestR184_CancelUpstream_DoesNotBlockOnDeadBackend — отмена служебная: вызов не
// ждёт воркер и не роняет обработчик при недоступном адресе.
func TestR184_CancelUpstream_DoesNotBlockOnDeadBackend(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	addr := srv.URL
	srv.Close()

	p := &Proxy{}
	fakeBackendR184(t, p, "dead", addr, "tok")

	start := time.Now()
	p.cancelUpstreamGeneration("dead", "m", "client_cancel")
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Errorf("отмена задержала вызывающий код на %v — вызов обязан быть асинхронным", elapsed)
	}
	time.Sleep(1200 * time.Millisecond) // горутина отработает; паника уронила бы тест
}

// TestR184_CancelUpstream_NilSafe — nil-получатель и пустой ID не паникуют.
func TestR184_CancelUpstream_NilSafe(t *testing.T) {
	var p *Proxy
	p.cancelUpstreamGeneration("x", "m", "test")
	(&Proxy{}).cancelUpstreamGeneration("", "m", "test")
	(&Proxy{}).cancelUpstreamGeneration("unknown", "m", "test")
}

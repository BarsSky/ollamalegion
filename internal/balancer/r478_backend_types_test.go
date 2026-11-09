//go:build llama_stub

// r478_backend_types_test.go — R478 (2026-11-09): типы бэкендов и честные ошибки.
//
// Живые жалобы, которые здесь закрываются:
//  1. «перестало работать распределение»: гейт R459 считал Ollama-кандидата
//     «холодным» ВСЕГДА (окна слота у Ollama в наших метриках нет), и сессия
//     никогда не переезжала на живую Ollama-копию.
//  2. «ответ не дошёл»: админские ручки Ollama (/api/show|pull|copy|delete|push)
//     игнорировали ошибку proxyRequest; при сбое транспорта клиент получал
//     200 с пустым телом.
package balancer

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// TestR478_R459AllowsOllamaCandidate — кандидат не-llama.cpp не должен
// отклоняться «модель не загружена»: окна слота у него мы не знаем.
func TestR478_R459AllowsOllamaCandidate(t *testing.T) {
	p := &Proxy{backends: map[string]*BackendState{
		"ollama-1": {Backend: &types.Backend{ID: "ollama-1", Host: "h", Type: types.BackendTypeOllama}},
		"cpp-1":    {Backend: &types.Backend{ID: "cpp-1", Host: "h", Type: types.BackendTypeLlamaCpp}},
	}}
	ok, why := p.sessionMoveKeepsWindowR459("ollama-1", "cpp-1", "m", 32768)
	if !ok {
		t.Fatalf("переезд на Ollama-кандидата отклонён (%s) — распределение снова окажется односторонним", why)
	}
}

// TestR478_R459StillRejectsColdLlamaCpp — для llama.cpp правило не ослаблено:
// незагруженная модель = переезд с выгрузкой/загрузкой, такой кандидат плох.
func TestR478_R459StillRejectsColdLlamaCpp(t *testing.T) {
	p := &Proxy{backends: map[string]*BackendState{
		"cpp-cold": {Backend: &types.Backend{ID: "cpp-cold", Host: "h", Type: types.BackendTypeLlamaCpp}},
		"cpp-hot":  {Backend: &types.Backend{ID: "cpp-hot", Host: "h", Type: types.BackendTypeLlamaCpp}},
	}}
	if ok, why := p.sessionMoveKeepsWindowR459("cpp-cold", "cpp-hot", "m", 32768); ok {
		t.Fatalf("холодный llama.cpp-кандидат допущен без причины (%q)", why)
	}
}

// TestR478_AdminProxyErrorIsNotEmpty200 — сбой транспорта даёт 502 JSON с
// бэкендом и request_id, а не пустой 200.
func TestR478_AdminProxyErrorIsNotEmpty200(t *testing.T) {
	p := &Proxy{backends: map[string]*BackendState{
		// Порт 1 на loopback: соединение гарантированно не откроется.
		"dead": {Backend: &types.Backend{ID: "dead", Host: "127.0.0.1", OllamaPort: 1, Type: types.BackendTypeOllama}},
	}}
	or := &OllamaRouter{proxy: p}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/show", strings.NewReader(`{"model":"m"}`))

	or.proxyAdminOrFailR478(rec, req, "dead")

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("код = %d, ожидался 502 (пустой 200 — это и есть «ответ не дошёл»)", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "dead") {
		t.Fatalf("в теле нет имени бэкенда: %s", body)
	}
}

// TestR478_AdminProxyEmptyBackendIs503 — «нет бэкенда» обязан быть 503 с
// понятным текстом, а не паникой на пустом id.
func TestR478_AdminProxyEmptyBackendIs503(t *testing.T) {
	or := &OllamaRouter{proxy: &Proxy{}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/pull", nil)
	or.proxyAdminOrFailR478(rec, req, "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("код = %d, ожидался 503", rec.Code)
	}
}

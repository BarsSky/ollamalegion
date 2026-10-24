//go:build llama_stub

// cuda_arch_refusal_r185_test.go — R185 (2026-10-20).
//
// ЖИВОЙ РАЗБОР, который закрывает этот тест: образ cppworker собран под
// CUDA_ARCH=86;120, бэкенд — Quadro RTX 4000 (sm_75). Загрузка «проходила»,
// первое GPU-ядро убивало процесс SIGABRT (no kernel image is available), а
// балансер/UI продолжали повторять загрузку без причины для клиента.
//
// Контракт: при cuda_arch_ok=false балансер ОТКАЗЫВАЕТ ДО загрузки, с текстом
// причины и указанием, что делать. Старый образ (полей нет) и недоступный
// /api/info — не блокируются: иначе предпроверка сама стала бы отказом.
package balancer

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newProxyForTestR185 — минимальный прокси: предпроверка читает только реестр.
func newProxyForTestR185(t *testing.T) *Proxy {
	t.Helper()
	return &Proxy{}
}

// hostOfR185 / portOfR185 — разбор httptest-URL на то, что ждёт предпроверка.
func hostOfR185(srvURL string) string {
	host, _, ok := strings.Cut(strings.TrimPrefix(srvURL, "http://"), ":")
	if !ok {
		return strings.TrimPrefix(srvURL, "http://")
	}
	return host
}

func portOfR185(srvURL string) int {
	hp := strings.TrimPrefix(srvURL, "http://")
	i := strings.LastIndex(hp, ":")
	if i < 0 {
		return 80
	}
	n := 0
	for _, c := range hp[i+1:] {
		if c < '0' || c > '9' {
			return 80
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// TestR185_TerminalResponse_APISurface — на нативной поверхности 400 и строка.
func TestR185_TerminalResponse_APISurface(t *testing.T) {
	rec := httptest.NewRecorder()
	writeModelArchUnsupportedResponseFor(rec, "/api/generate", "m1", "карта sm_75 не поддерживается")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("статус %d, ожидался 400 (503 невозможно: клиент уйдёт в retry-цикл)", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "" {
		t.Fatalf("Retry-After выставлен (%q): это сигнал «повтори», а отказ терминальный", got)
	}
	if got := rec.Header().Get("X-Model-Load-Decision"); got != "arch-unsupported" {
		t.Errorf("нет однозначного заголовка решения: %q", got)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("тело не JSON: %v", err)
	}
	if body["code"] != cudaArchUnsupportedCode {
		t.Errorf("code=%v, ожидался %s", body["code"], cudaArchUnsupportedCode)
	}
	if s, _ := body["error"].(string); !strings.Contains(s, "sm_75") {
		t.Errorf("в нативной форме ошибка должна быть строкой с причиной, получено %v", body["error"])
	}
}

// TestR185_TerminalResponse_OpenAISurface — на /v1/* конверт OpenAI (R183).
func TestR185_TerminalResponse_OpenAISurface(t *testing.T) {
	rec := httptest.NewRecorder()
	writeModelArchUnsupportedResponseFor(rec, "/v1/chat/completions", "m1", "карта sm_75 не поддерживается")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("статус %d, ожидался 400", rec.Code)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("тело не JSON: %v", err)
	}
	env, ok := body["error"].(map[string]interface{})
	if !ok {
		t.Fatalf("на /v1/* ожидался конверт OpenAI, получено %T: %v", body["error"], body["error"])
	}
	if env["code"] != cudaArchUnsupportedCode {
		t.Errorf("code=%v, ожидался %s", env["code"], cudaArchUnsupportedCode)
	}
	if msg, _ := env["message"].(string); !strings.Contains(msg, "sm_75") {
		t.Errorf("message потерял причину: %v", env["message"])
	}
}

// TestR185_RefusalRememberedFromLoadError — отказ доезжает до клиента с первого
// запроса: ошибка авто-загрузки опознаётся по коду и превращается в 400.
func TestR185_RefusalRememberedFromLoadError(t *testing.T) {
	p := newProxyForTestR185(t)
	loadErr := fmt.Errorf("model load failed: %w",
		fmt.Errorf("бэкенд be1 не может считать на этой карте: sm=75 [%s]", cudaArchUnsupportedCode))
	rec := httptest.NewRecorder()
	if !p.writeModelArchUnsupportedIfKnownFor(rec, "/v1/chat/completions", "be1", "m1", loadErr) {
		t.Fatal("отказ не распознан: клиент получил бы 503 «повтори»")
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("статус %d, ожидался 400", rec.Code)
	}
	// Повтор (клиент всё-таки повторил) — снова 400, из памяти, без загрузки.
	rec2 := httptest.NewRecorder()
	if !p.writeModelArchUnsupportedIfKnownFor(rec2, "/v1/chat/completions", "be1", "m1", nil) {
		t.Fatal("запомненный отказ не сработал на повторе")
	}
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("на повторе статус %d, ожидался 400", rec2.Code)
	}
	// Другая модель того же бэкенда не задета.
	if p.writeModelArchUnsupportedIfKnownFor(httptest.NewRecorder(), "/v1/chat/completions", "be1", "m2", nil) {
		t.Fatal("отказ одной модели применился к другой")
	}
	// Запись живёт ограниченно: после пересборки образа отказ обязан сняться.
	rec3, _ := p.modelArchUnsupportedFor("be1", "m1")
	rec3.At = time.Now().Add(-3 * time.Minute)
	p.mu.Lock()
	p.modelArchUnsupported[modelArchKey("be1", "m1")] = rec3
	p.mu.Unlock()
	if _, ok := p.modelArchUnsupportedFor("be1", "m1"); ok {
		t.Fatal("истёкший отказ не снят: после исправления образа клиент продолжал бы получать 400")
	}
}

// TestR185_RefusesWhenArchMismatch — главный случай: отказ + причина + что делать.
func TestR185_RefusesWhenArchMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/info" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"cuda_arch_ok":      false,
			"cuda_arch_warning": "образ собран под sm=120;86, карта sm=75",
			"cuda_arch_built":   []string{"75", "86", "120"},
		})
	}))
	defer srv.Close()

	p := newProxyForTestR185(t)
	fakeBackendR184(t, p, "be1", srv.URL, "")

	refuse, reason := p.cudaArchRefusal("be1", hostOfR185(srv.URL), portOfR185(srv.URL))
	if !refuse {
		t.Fatalf("ожидался отказ в загрузке, получено refuse=false")
	}
	for _, want := range []string{"be1", "sm=75", "75;86;120", "НЕ запущена"} {
		if !strings.Contains(reason, want) {
			t.Errorf("в причине нет %q: %s", want, reason)
		}
	}
}

// TestR185_NoRefusalWhenArchOK — совместимый образ грузится как раньше.
func TestR185_NoRefusalWhenArchOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"cuda_arch_ok":      true,
			"cuda_arch_warning": "",
			"cuda_arch_built":   []string{"75", "86", "120"},
		})
	}))
	defer srv.Close()

	p := newProxyForTestR185(t)
	fakeBackendR184(t, p, "be1", srv.URL, "")
	if refuse, reason := p.cudaArchRefusal("be1", hostOfR185(srv.URL), portOfR185(srv.URL)); refuse {
		t.Fatalf("ложный отказ при совместимых архитектурах: %s", reason)
	}
}

// TestR185_NoRefusalOnOldImage — образ без полей: поведение не меняем вслепую.
func TestR185_NoRefusalOnOldImage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok", "version": "0.7.150"})
	}))
	defer srv.Close()

	p := newProxyForTestR185(t)
	fakeBackendR184(t, p, "be1", srv.URL, "")
	if refuse, reason := p.cudaArchRefusal("be1", hostOfR185(srv.URL), portOfR185(srv.URL)); refuse {
		t.Fatalf("отказ на старом образе (полей нет) недопустим: %s", reason)
	}
}

// TestR185_NoRefusalWhenBackendDown — недоступный воркер не должен блокировать
// загрузку ДРУГИХ бэкендов; сетевую ошибку решает обычный путь загрузки.
func TestR185_NoRefusalWhenBackendDown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	host, port := hostOfR185(srv.URL), portOfR185(srv.URL)
	srv.Close() // порт закрыт

	p := newProxyForTestR185(t)
	fakeBackendR184(t, p, "be1", "http://"+host+":1", "")
	if refuse, reason := p.cudaArchRefusal("be1", host, port); refuse {
		t.Fatalf("недоступный воркер не должен давать отказ: %s", reason)
	}
}

//go:build llama_stub

// worker_admin_token_r307_test.go — R307 (2026-10-30): балансер предъявляет токен
// воркеру на административных ручках.
//
// ЗАЧЕМ. Ручки, меняющие память/диск воркера, закрыты authMiddleware. Если
// балансер не пришлёт токен, загрузка модели перестанет работать (401), а если
// пришлёт чужой — тоже. Поэтому правило одно (workerAdminToken) и путь один
// (applyWorkerAdminTokenR307), а тест держит оба конца: набор путей и наличие
// токена у бэкенда.
package balancer

import (
	"net/http"
	"os"
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

func TestR307_WorkerAdminPathMatching(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/api/models/load", true},
		{"/api/models/load-with-params", true},
		{"/load", true},
		{"/api/models/unload", true},
		{"/api/models/unload?name=gemma&force=true", true},
		{"/api/models/delete", true},
		{"/api/delete", true},
		{"/api/models/reload", true},
		// Не административные: это трафик генерации и чтения.
		{"/api/models", false},
		{"/api/chat", false},
		{"/api/generate", false},
		{"/api/models/load/progress", false},
		{"/v1/chat/completions", false},
		{"/api/models/load-extra", false},
	}
	for _, c := range cases {
		if got := workerAdminPathR307(c.path); got != c.want {
			t.Errorf("workerAdminPathR307(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

func TestR307_ApplyTokenOnlyForAdminPaths(t *testing.T) {
	h := http.Header{}
	if applyWorkerAdminTokenR307(h, "/api/chat", "tok") {
		t.Error("токен поставлен на путь генерации — он не административный")
	}
	if h.Get("X-API-Token") != "" {
		t.Error("заголовок токена не должен появляться на пути генерации")
	}

	if !applyWorkerAdminTokenR307(h, "/api/models/unload?name=m", "tok") {
		t.Fatal("административный путь обязан получить токен")
	}
	if got := h.Get("X-API-Token"); got != "tok" {
		t.Errorf("X-API-Token = %q, want tok", got)
	}
}

func TestR307_EmptyTokenSetsNothing(t *testing.T) {
	h := http.Header{}
	if applyWorkerAdminTokenR307(h, "/api/models/load", "  ") {
		t.Error("пустой токен не должен считаться применённым")
	}
	if h.Get("X-API-Token") != "" {
		t.Error("пустой токен не должен ставить заголовок")
	}
}

func TestR307_TokenComesFromBackendRecord(t *testing.T) {
	p := &Proxy{backends: map[string]*BackendState{}}
	p.backends["b1"] = &BackendState{Backend: &types.Backend{ID: "b1", CppWorkerApiToken: " sec "}}

	if got := p.workerAdminToken("b1"); got != "sec" {
		t.Errorf("workerAdminToken = %q, want sec (пробелы обрезаются)", got)
	}
	if got := p.workerAdminToken("missing"); got != "" {
		t.Errorf("неизвестный бэкенд: %q, want пусто", got)
	}
	if got := p.workerAdminToken(""); got != "" {
		t.Errorf("пустой id: %q, want пусто", got)
	}
}

// TestR307_BalancerCoversWorkerRoutes — сторож согласованности двух списков.
//
// Воркер закрывает ручку в router.go; балансер обязан считать её админской и
// прислать токен. Разъехались списки — либо ломается загрузка (воркер закрыл,
// балансер не шлёт), либо остаётся дыра (балансер шлёт, воркер открыт — этим
// занимается сторож на стороне воркера).
func TestR307_BalancerCoversWorkerRoutes(t *testing.T) {
	src, err := os.ReadFile("../../cmd/cppworker/router.go")
	if err != nil {
		t.Skipf("router.go недоступен: %v", err)
	}
	text := string(src)

	// Ищем `"<route>", authMiddleware(` и требуем, чтобы балансер знал этот путь.
	found := 0
	for _, line := range strings.Split(text, "\n") {
		idx := strings.Index(line, "authMiddleware(")
		if idx < 0 {
			continue
		}
		start := strings.Index(line, `"`)
		if start < 0 || start > idx {
			continue
		}
		end := strings.Index(line[start+1:], `"`)
		if end < 0 {
			continue
		}
		route := line[start+1 : start+1+end]
		// Воркер оборачивает и служебные ручки конфигурации — их балансер
		// вызывает своими функциями, где токен ставится явно (R133/R118).
		if !strings.HasPrefix(route, "/api/models/") && route != "/load" && route != "/api/delete" {
			continue
		}
		found++
		if !workerAdminPathR307(route) {
			t.Errorf("воркер закрыл %s, а балансер не считает путь административным — "+
				"не пришлёт токен и получит 401", route)
		}
	}
	if found == 0 {
		t.Fatal("не найдено ни одного закрытого маршрута в router.go — сторож ослеп")
	}
}

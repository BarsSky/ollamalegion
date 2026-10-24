// model_known_r193_test.go — R193 (2026-10-20).
//
// Живой дефект: неизвестная модель через балансер получала 200 и ответ ОТ
// ЗАГРУЖЕННОЙ модели (напрямую воркер отвечал 404). Тест фиксирует сверку имени
// с каталогом бэкенда и отказ блокировать, когда каталог недоступен.
package balancer

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func catalogServer(t *testing.T, models []string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/models" {
			http.NotFound(w, r)
			return
		}
		items := make([]map[string]string, 0, len(models))
		for _, n := range models {
			items = append(items, map[string]string{"name": n, "path": "models/" + n + ".gguf"})
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"models": items})
	}))
}

func TestR193_UnknownModelIsNotKnown(t *testing.T) {
	srv := catalogServer(t, []string{"gemma-4-E4B-it-Q4_K_M"})
	defer srv.Close()
	host, port := hostPortOf(t, srv.URL)
	p := &Proxy{}

	known, avail, probed := p.modelKnownOnBackend(host, port, "", "no-such-model-xyz")
	if !probed {
		t.Fatal("каталог не опрошен")
	}
	if known {
		t.Fatal("неизвестная модель признана известной — балансер снова ответит чужой моделью")
	}
	if len(avail) == 0 || !strings.Contains(strings.Join(avail, ","), "gemma") {
		t.Fatalf("список доступных моделей не заполнен: %v", avail)
	}
}

func TestR193_KnownModelAndAlias(t *testing.T) {
	srv := catalogServer(t, []string{"gemma-4-E4B-it-Q4_K_M"})
	defer srv.Close()
	host, port := hostPortOf(t, srv.URL)
	p := &Proxy{}

	for _, name := range []string{"gemma-4-E4B-it-Q4_K_M", "gemma-4-E4B-it-Q4_K_M.gguf", "models/gemma-4-E4B-it-Q4_K_M.gguf"} {
		known, _, probed := p.modelKnownOnBackend(host, port, "", name)
		if !probed || !known {
			t.Fatalf("известная модель/алиас не распознаны: %q (probed=%v known=%v)", name, probed, known)
		}
	}
}

func TestR193_NoProbeMeansNoRefusal(t *testing.T) {
	srv := catalogServer(t, nil)
	defer srv.Close()
	host, port := hostPortOf(t, srv.URL)
	p := &Proxy{}
	// Пустой каталог — не доказательство отсутствия: не блокируем.
	if _, _, probed := p.modelKnownOnBackend(host, port, "", "m"); probed {
		t.Fatal("пустой каталог не должен считаться доказательством")
	}
	// Недоступный бэкенд — тоже не блокируем.
	srv.Close()
	if _, _, probed := p.modelKnownOnBackend(host, port, "", "m"); probed {
		t.Fatal("недоступный бэкенд не должен давать отказ")
	}
}

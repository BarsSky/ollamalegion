package balancer

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"ollama-loadbalancer/pkg/types"
)

// TestDispatchRouters_SkipsNilAndReturnsTrueOnFirstHit — R59.15b: проверяет
// что dispatchRouters (а) пропускает nil-элементы, (б) возвращает true на
// первом router'е, который вернул true из Route.
func TestDispatchRouters_SkipsNilAndReturnsTrueOnFirstHit(t *testing.T) {
	p := &Proxy{} // поля routers не нужны — передаём список явно
	calls := []string{}

	noHit := &fakeRouter{name: "noHit", path: "/api/nothing", hit: false, log: &calls}
	hit := &fakeRouter{name: "hit", path: "/api/version", hit: true, log: &calls}
	afterHit := &fakeRouter{name: "afterHit", path: "/api/version", hit: true, log: &calls}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/version", nil)

	// Список: nil, noHit, hit, afterHit. nil пропускается, noHit возвращает
	// false, hit возвращает true, afterHit НЕ должен быть вызван.
	result := p.dispatchRouters([]BackendRouter{nil, noHit, hit, afterHit}, rec, req)

	assert.True(t, result, "должен вернуть true если hit router обработал")
	assert.Equal(t, []string{"noHit", "hit"}, calls,
		"afterHit не должен быть вызван после hit")
}

// TestDispatchRouters_AllReturnFalseYieldsFalse — R59.15b: все router'ы
// вернули false → общий результат false.
func TestDispatchRouters_AllReturnFalseYieldsFalse(t *testing.T) {
	p := &Proxy{}
	calls := []string{}
	a := &fakeRouter{name: "a", hit: false, log: &calls}
	b := &fakeRouter{name: "b", hit: false, log: &calls}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/whatever", nil)

	result := p.dispatchRouters([]BackendRouter{a, b}, rec, req)

	assert.False(t, result, "если ни один не сработал, возвращаем false")
	assert.Equal(t, []string{"a", "b"}, calls, "оба router'а должны быть опрошены")
}

// TestDispatchRouters_EmptyListYieldsFalse — R59.15b: пустой список
// (например, когда оба router'а nil) → false без panic.
func TestDispatchRouters_EmptyListYieldsFalse(t *testing.T) {
	p := &Proxy{}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)

	assert.NotPanics(t, func() {
		result := p.dispatchRouters(nil, rec, req)
		assert.False(t, result)
	})
}

// TestBuildRoutersForDispatch_OrdersCorrectly — R59.15b: buildRoutersForDispatch
// строит правильный приоритет в зависимости от bt и наличия бэкендов.
func TestBuildRoutersForDispatch_OrdersCorrectly(t *testing.T) {
	t.Run("locked llama.cpp mode returns only llama.cpp router", func(t *testing.T) {
		p := &Proxy{
			ollamaRouter:   NewOllamaRouter(nil),
			llamaCppRouter: NewLlamaCppRouter(nil),
		}
		list := p.buildRoutersForDispatch(types.BackendTypeLlamaCpp)
		require.Len(t, list, 1)
		assert.Equal(t, "LlamaCppRouter", list[0].Name())
	})

	t.Run("locked ollama mode returns only ollama router", func(t *testing.T) {
		p := &Proxy{
			ollamaRouter:   NewOllamaRouter(nil),
			llamaCppRouter: NewLlamaCppRouter(nil),
		}
		list := p.buildRoutersForDispatch(types.BackendTypeOllama)
		require.Len(t, list, 1)
		assert.Equal(t, "OllamaRouter", list[0].Name())
	})

	t.Run("mixed with ollama only → llama.cpp first (avoid empty aggregate)", func(t *testing.T) {
		p := newTestProxyWithBackendsByType(t, types.BackendTypeLlamaCpp)
		list := p.buildRoutersForDispatch("")
		require.Len(t, list, 2)
		assert.Equal(t, "LlamaCppRouter", list[0].Name(),
			"mixed+llama-only: llama.cpp router первый")
		assert.Equal(t, "OllamaRouter", list[1].Name())
	})

	t.Run("mixed with ollama present → ollama first", func(t *testing.T) {
		p := newTestProxyWithBackendsByType(t, types.BackendTypeOllama)
		list := p.buildRoutersForDispatch("")
		require.Len(t, list, 2)
		assert.Equal(t, "OllamaRouter", list[0].Name(),
			"mixed+ollama: ollama router первый (больше native /api/* support)")
		assert.Equal(t, "LlamaCppRouter", list[1].Name())
	})

	t.Run("mixed with no backends → ollama first as default", func(t *testing.T) {
		p := &Proxy{
			ollamaRouter:   NewOllamaRouter(nil),
			llamaCppRouter: NewLlamaCppRouter(nil),
		}
		list := p.buildRoutersForDispatch("")
		require.Len(t, list, 2)
		assert.Equal(t, "OllamaRouter", list[0].Name())
	})
}

// fakeRouter — минимальная BackendRouter-реализация для тестов dispatchRouters.
type fakeRouter struct {
	name string
	path string
	hit  bool
	log  *[]string
}

func (f *fakeRouter) Route(w http.ResponseWriter, r *http.Request) bool {
	if f.log != nil {
		*f.log = append(*f.log, f.name)
	}
	if f.hit && r.URL.Path == f.path {
		w.WriteHeader(http.StatusOK)
		return true
	}
	return false
}

func (f *fakeRouter) BackendType() types.BackendType {
	return types.BackendTypeOllama
}

func (f *fakeRouter) Name() string {
	return f.name
}

// newTestProxyWithBackendsByType — утилита для тестов buildRoutersForDispatch:
// создаёт Proxy с одним зарегистрированным backend указанного типа, чтобы
// countBackendsByType() вернул ожидаемый результат.
func newTestProxyWithBackendsByType(t *testing.T, bt types.BackendType) *Proxy {
	t.Helper()
	p := &Proxy{
		backends: map[string]*BackendState{},
		ollamaRouter:   NewOllamaRouter(nil),
		llamaCppRouter: NewLlamaCppRouter(nil),
	}
	p.backends["b1"] = &BackendState{
		Backend: &types.Backend{
			ID:     "b1",
			Type:   bt,
			Status: types.StatusHealthy,
		},
	}
	return p
}

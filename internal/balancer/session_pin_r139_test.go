package balancer

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

// R139 (2026-10-16): «backend занят моим же стримом» ≠ «backend занят чужими
// запросами».
//
// Живой дефект (deepseek harness через балансер, длинная генерация): сессия
// привязана к прогретой копии модели, её собственный стрим в полёте. Следующий
// запрос ТОЙ ЖЕ сессии видел loadRatio=1.0 на своей же копии и
// «ребалансировался» на другой backend, где модели нет — её надо грузить
// заново. Клиент видел десятки секунд тишины и падал по
// `pi-ai stream idle timeout after 300000ms`; смерть того воркера давала
// `502 ... /v1/completions: EOF`.
//
// Два теста ниже — одна и та же нагрузка (ActiveReqs=1, max=1) и разница
// только в том, ЧЕЙ это запрос. Так они проверяют именно различение, а не
// «мы просто выключили ребалансировку»: справедливость под чужой нагрузкой
// сохранена.

// TestSessionKeepsWarmCopyUnderOwnStreamR139 — слот занят СВОИМ стримом:
// остаёмся на прогретой копии и ждём свой же слот.
func TestSessionKeepsWarmCopyUnderOwnStreamR139(t *testing.T) {
	t.Parallel()

	config := createTestConfig()
	config.Balancing.SessionStickiness = true
	proxy := newProxyWithCleanup(t, config)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	proxy.sessionMgr.Set("sess-1", "backend-1", "llama3", "client1", "127.0.0.1", "test-agent")

	// Единственный слот прогретой копии занят собственным стримом сессии.
	proxy.backends["backend-1"].mu.Lock()
	proxy.backends["backend-1"].Backend.MaxConcurrentReqs = 1
	proxy.backends["backend-1"].ActiveReqs = 1
	proxy.backends["backend-1"].mu.Unlock()
	proxy.admission.markInflight("sess-1", 1)

	// Свободный, но ХОЛОДНЫЙ backend — выглядит менее загруженным.
	proxy.backends["backend-2"].mu.Lock()
	proxy.backends["backend-2"].ActiveReqs = 0
	proxy.backends["backend-2"].mu.Unlock()

	req := httptest.NewRequest(http.MethodPost, "/api/generate", nil)
	backend := proxy.resolveSessionBackend(req, "llama3", "sess-1", "client1")

	assert.Equal(t, "backend-1", backend,
		"Слот занят своим же стримом — держимся прогретой копии, а не уезжаем на холодную")
}

// TestSessionMovesUnderForeignLoadR139 — тот же единственный слот занят ЧУЖИМ
// запросом: ребалансировка (справедливость) должна работать как раньше.
func TestSessionMovesUnderForeignLoadR139(t *testing.T) {
	t.Parallel()

	config := createTestConfig()
	config.Balancing.SessionStickiness = true
	proxy := newProxyWithCleanup(t, config)
	proxy.SetQueueManagerProxy()
	defer proxy.queueMgr.Stop()

	proxy.sessionMgr.Set("sess-1", "backend-1", "llama3", "client1", "127.0.0.1", "test-agent")

	// Тот же единственный слот — но занят чужой сессией (наша не в полёте).
	proxy.backends["backend-1"].mu.Lock()
	proxy.backends["backend-1"].Backend.MaxConcurrentReqs = 1
	proxy.backends["backend-1"].ActiveReqs = 1
	proxy.backends["backend-1"].mu.Unlock()

	proxy.backends["backend-2"].mu.Lock()
	proxy.backends["backend-2"].ActiveReqs = 0
	proxy.backends["backend-2"].mu.Unlock()

	req := httptest.NewRequest(http.MethodPost, "/api/generate", nil)
	backend := proxy.resolveSessionBackend(req, "llama3", "sess-1", "client1")

	assert.Equal(t, "backend-2", backend,
		"Чужая нагрузка на единственном слоте — ребалансировка на свободный backend сохранена")
}

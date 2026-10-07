package agent

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// R-MultiHost (2026-10-07).
//
// Регистрация выполняется один раз — в Agent.Start(). Если запись бэкенда на
// балансере исчезла (её удалили из WebUI, отобрал другой узел с тем же ID или
// она потерялась при перезапуске), агент об этом не узнавал: метрики и heartbeat
// продолжали уходить в никуда, балансер отвечал 404 «Backend not found. Please
// register agent first.», и воркер пропадал со стенда НАВСЕГДА — до ручного
// `docker restart`. Наблюдалось на живой паре с imageworker.
//
// Эти тесты фиксируют самовосстановление: на 404/403 агент повторяет
// регистрацию, но не чаще registerRetryCooldown.

// TestNeedsReregistration — какие ответы балансера означают «запись не наша».
func TestNeedsReregistration(t *testing.T) {
	assert.True(t, needsReregistration(http.StatusNotFound), "404 = запись исчезла")
	assert.True(t, needsReregistration(http.StatusForbidden), "403 = запись принадлежит другому узлу")
	assert.False(t, needsReregistration(http.StatusOK))
	assert.False(t, needsReregistration(http.StatusBadRequest))
	assert.False(t, needsReregistration(http.StatusInternalServerError),
		"500 — это сбой балансера, а не потеря записи: перерегистрация не поможет")
}

// TestEnsureRegistered_ReRegistersAndRespectsCooldown — на 404 агент повторяет
// регистрацию, но не чаще, чем раз в registerRetryCooldown.
func TestEnsureRegistered_ReRegistersAndRespectsCooldown(t *testing.T) {
	var registerCalls int64

	balancerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/agents/register" {
			atomic.AddInt64(&registerCalls, 1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"success":true,"action":"created","agentId":"test-agent-1"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer balancerServer.Close()

	config := createTestAgentConfig()
	config.BalancerURL = balancerServer.URL
	a := NewAgent(config)

	// Первая неудача (404 от телеметрии) → одна повторная регистрация.
	a.ensureRegistered("metrics returned 404")
	assert.Equal(t, int64(1), atomic.LoadInt64(&registerCalls))

	// Вторая попытка сразу же — подавлена кулдауном: недоступность балансера
	// не должна превращаться в шквал регистраций.
	a.ensureRegistered("metrics returned 404")
	a.ensureRegistered("heartbeat returned 403")
	assert.Equal(t, int64(1), atomic.LoadInt64(&registerCalls),
		"повторные попытки в пределах кулдауна должны подавляться")

	// После кулдауна регистрация снова возможна.
	a.mu.Lock()
	a.lastRegisterTry = time.Now().Add(-2 * registerRetryCooldown)
	a.mu.Unlock()
	a.ensureRegistered("heartbeat returned 404")
	assert.Equal(t, int64(2), atomic.LoadInt64(&registerCalls))
}

// TestEnsureRegistered_UpdatesBackendIDFromAttach — после повторной регистрации
// агент должен знать свой backendId (режим attached), иначе метрики уйдут в
// legacy-эндпоинт и снова получат 404.
func TestEnsureRegistered_UpdatesBackendIDFromAttach(t *testing.T) {
	balancerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/agents/register" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"success":true,"action":"attached","agentId":"imageworker","backendId":"imageworker-34"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer balancerServer.Close()

	config := createTestAgentConfig()
	config.BalancerURL = balancerServer.URL
	a := NewAgent(config)
	a.config.BackendID = ""

	a.ensureRegistered("heartbeat returned 403")

	assert.Equal(t, "imageworker-34", a.config.BackendID,
		"после повторной регистрации backendId должен обновиться из ответа")
}

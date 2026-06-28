package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/pkg/types"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// F.γ tests (2026-06-28): session F — health-check UI page backend.
//
// Acceptance criteria:
// 1. healthDetailedHandler returns 200 with valid HealthReport JSON.
// 2. Method != GET → 405 Method Not Allowed.
// 3. Empty cluster → status="empty", healthScore=100, totalBackends=0.
// 4. All healthy backends → status="healthy", healthScore=100.
// 5. Partially healthy backends → status="degraded" with reduced score.
// 6. Backend with ConsecutiveFails > 0 → status="degraded".
// 7. Transport EOF event in ring buffer → recentErrors has source=transport.
// 8. Notification event in ring buffer → recentErrors has source=event.
// 9. errorsBySource map aggregated correctly.
// 10. recentErrors sorted by timestamp DESC.

// createHealthTestServer — копия createTestServer, но с включённым eventsHub (SetEventBus).
func createHealthTestServer(t *testing.T) (*httptest.Server, *Server, *balancer.Proxy) {
	t.Helper()

	config := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{
			Host:    "localhost",
			Port:    8080,
			APIPort: 8081,
		},
		Backends: []types.Backend{
			{
				ID:                "backend1",
				Name:              "Test Backend 1",
				Host:              "localhost",
				OllamaPort:        11434,
				CppWorkerPort:     18091,
				AgentPort:         9090,
				Weight:            1,
				MaxConcurrentReqs: 10,
				Status:            types.StatusHealthy,
				Type:              types.BackendTypeOllama,
			},
			{
				ID:                "backend2",
				Name:              "Test Backend 2",
				Host:              "localhost",
				OllamaPort:        11435,
				CppWorkerPort:     18092,
				AgentPort:         9091,
				Weight:            2,
				MaxConcurrentReqs: 20,
				Status:            types.StatusUnhealthy,
				Type:              types.BackendTypeLlamaCpp,
			},
		},
		Balancing: types.BalancingSettings{
			Algorithm:           types.AlgorithmResourceAware,
			HealthCheckInterval: 10,
			RequestTimeout:      30,
			QueueTimeout:        60,
			QueueMaxSize:        100,
			QueueWorkers:        4,
		},
		API: types.APISettings{
			RateLimit: 100,
			RateBurst: 200,
		},
		Auth: types.AuthConfig{
			Enabled:    false,
			Tokens:     []string{"test-token"},
			HeaderName: "X-API-Token",
		},
	}

	proxy := balancer.NewProxy(config)
	healthChecker := balancer.NewHealthChecker(proxy, 10*time.Second, 3)
	server := NewServer(proxy, config, healthChecker)
	// Включаем eventsHub для тестов F.γ (имитирует SetEventBus из main.go).
	server.SetEventBus(nil) // nil OK — collectRecentErrors проверяет eventsHub != nil

	testServer := httptest.NewServer(server)
	return testServer, server, proxy
}

// TestHealthDetailedHandler_OK — базовый сценарий: 200 OK + корректный JSON.
func TestHealthDetailedHandler_OK(t *testing.T) {
	server, _, _ := createHealthTestServer(t)
	defer server.Close()

	resp, err := http.Get(server.URL + "/api/v1/health/detailed")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))

	var report types.HealthReport
	err = json.NewDecoder(resp.Body).Decode(&report)
	require.NoError(t, err)

	// Проверяем базовые поля.
	assert.False(t, report.Timestamp.IsZero(), "Timestamp must be set")
	assert.GreaterOrEqual(t, report.HealthScore, 0)
	assert.LessOrEqual(t, report.HealthScore, 100)
	assert.GreaterOrEqual(t, report.TotalBackends, 0)
	assert.NotNil(t, report.ErrorsBySource, "errorsBySource map must be initialized (not null)")
}

// TestHealthDetailedHandler_MethodNotAllowed — POST/PUT/DELETE → 405.
func TestHealthDetailedHandler_MethodNotAllowed(t *testing.T) {
	server, _, _ := createHealthTestServer(t)
	defer server.Close()

	for _, method := range []string{"POST", "PUT", "DELETE", "PATCH"} {
		req, err := http.NewRequest(method, server.URL+"/api/v1/health/detailed", nil)
		require.NoError(t, err)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		resp.Body.Close()
		assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode, "method=%s", method)
	}
}

// TestHealthDetailedHandler_EmptyCluster — без бэкендов status="empty".
func TestHealthDetailedHandler_EmptyCluster(t *testing.T) {
	config := &types.LoadBalancerConfig{
		Backends: []types.Backend{},
		API:      types.APISettings{RateLimit: 100, RateBurst: 200},
		Auth:     types.AuthConfig{Enabled: false, Tokens: []string{"t"}, HeaderName: "X-API-Token"},
	}
	proxy := balancer.NewProxy(config)
	hc := balancer.NewHealthChecker(proxy, 10*time.Second, 3)
	server := NewServer(proxy, config, hc)
	server.SetEventBus(nil)
	testServer := httptest.NewServer(server)
	defer testServer.Close()

	resp, err := http.Get(testServer.URL + "/api/v1/health/detailed")
	require.NoError(t, err)
	defer resp.Body.Close()

	var report types.HealthReport
	err = json.NewDecoder(resp.Body).Decode(&report)
	require.NoError(t, err)

	assert.Equal(t, types.HealthLevelEmpty, report.Status)
	assert.Equal(t, 0, report.TotalBackends)
	assert.Equal(t, 0, report.HealthyBackends)
	assert.Equal(t, 100, report.HealthScore) // empty = full score (no penalties)
	assert.Empty(t, report.Backends)
}

// TestBuildHealthReport_AllHealthy — все бэкенды healthy → status=healthy, score=100.
func TestBuildHealthReport_AllHealthy(t *testing.T) {
	config := &types.LoadBalancerConfig{
		Backends: []types.Backend{
			{ID: "b1", Host: "h1", OllamaPort: 11434, Status: types.StatusHealthy, Type: types.BackendTypeOllama},
			{ID: "b2", Host: "h2", OllamaPort: 11435, Status: types.StatusHealthy, Type: types.BackendTypeOllama},
		},
		API:  types.APISettings{RateLimit: 100, RateBurst: 200},
		Auth: types.AuthConfig{Enabled: false, Tokens: []string{"t"}, HeaderName: "X-API-Token"},
	}
	proxy := balancer.NewProxy(config)
	hc := balancer.NewHealthChecker(proxy, 10*time.Second, 3)
	server := NewServer(proxy, config, hc)
	server.SetEventBus(nil)

	report := server.buildHealthReport()

	assert.Equal(t, types.HealthLevelHealthy, report.Status)
	assert.Equal(t, 2, report.TotalBackends)
	assert.Equal(t, 2, report.HealthyBackends)
	assert.Equal(t, 0, report.UnhealthyBackends)
	assert.Equal(t, 100, report.HealthScore)
	assert.Len(t, report.Backends, 2)
}

// TestBuildHealthReport_PartiallyHealthy — частично unhealthy → status=degraded.
func TestBuildHealthReport_PartiallyHealthy(t *testing.T) {
	config := &types.LoadBalancerConfig{
		Backends: []types.Backend{
			{ID: "b1", Host: "h1", OllamaPort: 11434, Status: types.StatusHealthy, Type: types.BackendTypeOllama},
			{ID: "b2", Host: "h2", OllamaPort: 11435, Status: types.StatusUnhealthy, Type: types.BackendTypeOllama},
		},
		API:  types.APISettings{RateLimit: 100, RateBurst: 200},
		Auth: types.AuthConfig{Enabled: false, Tokens: []string{"t"}, HeaderName: "X-API-Token"},
	}
	proxy := balancer.NewProxy(config)
	hc := balancer.NewHealthChecker(proxy, 10*time.Second, 3)
	server := NewServer(proxy, config, hc)
	server.SetEventBus(nil)

	report := server.buildHealthReport()

	assert.Equal(t, types.HealthLevelDegraded, report.Status)
	assert.Equal(t, 2, report.TotalBackends)
	assert.Equal(t, 1, report.HealthyBackends)
	assert.Equal(t, 1, report.UnhealthyBackends)
	assert.Less(t, report.HealthScore, 100, "score must be reduced with unhealthy backends")
}

// TestBuildHealthReport_AllUnhealthy — 0 healthy → status=unhealthy.
func TestBuildHealthReport_AllUnhealthy(t *testing.T) {
	config := &types.LoadBalancerConfig{
		Backends: []types.Backend{
			{ID: "b1", Host: "h1", OllamaPort: 11434, Status: types.StatusUnhealthy, Type: types.BackendTypeOllama},
		},
		API:  types.APISettings{RateLimit: 100, RateBurst: 200},
		Auth: types.AuthConfig{Enabled: false, Tokens: []string{"t"}, HeaderName: "X-API-Token"},
	}
	proxy := balancer.NewProxy(config)
	hc := balancer.NewHealthChecker(proxy, 10*time.Second, 3)
	server := NewServer(proxy, config, hc)
	server.SetEventBus(nil)

	report := server.buildHealthReport()

	assert.Equal(t, types.HealthLevelUnhealthy, report.Status)
	assert.Equal(t, 0, report.HealthyBackends)
}

// TestBuildHealthReport_TransportEOF — событие transport_eof в ring buffer.
func TestBuildHealthReport_TransportEOF(t *testing.T) {
	config := &types.LoadBalancerConfig{
		Backends: []types.Backend{
			{ID: "b1", Host: "h1", OllamaPort: 11434, Status: types.StatusHealthy, Type: types.BackendTypeLlamaCpp},
		},
		API:  types.APISettings{RateLimit: 100, RateBurst: 200},
		Auth: types.AuthConfig{Enabled: false, Tokens: []string{"t"}, HeaderName: "X-API-Token"},
	}
	proxy := balancer.NewProxy(config)
	hc := balancer.NewHealthChecker(proxy, 10*time.Second, 3)
	server := NewServer(proxy, config, hc)
	server.SetEventBus(nil)

	// Имитируем F.0a: publishTransportEOF заполнил eventsHub.buf.
	server.eventsHub.buf.push(types.Event{
		Type:      types.EventNotification,
		Timestamp: time.Now().Add(-30 * time.Second),
		BackendID: "b1",
		Model:     "llama3",
		Severity:  types.SeverityWarning,
		Source:    "transport",
		Message:   "EOF from upstream: broken pipe",
		Data: map[string]interface{}{
			"event_kind":  "transport_eof",
			"path":        "/v1/chat/completions",
			"duration_ms": 1234,
			"error":       "broken pipe",
		},
	})

	report := server.buildHealthReport()

	require.Len(t, report.RecentErrors, 1)
	assert.Equal(t, types.ErrorSourceTransport, report.RecentErrors[0].Source)
	assert.Equal(t, "b1", report.RecentErrors[0].BackendID)
	assert.Equal(t, "llama3", report.RecentErrors[0].Model)
	assert.Equal(t, "/v1/chat/completions", report.RecentErrors[0].Path)
	assert.Contains(t, report.RecentErrors[0].Message, "EOF")
	assert.Equal(t, 1, report.ErrorsBySource["transport"])
	// Severity warning → status=warning, не error. HealthScore: 100% healthy + 1 error penalty.
	// Score = 100 - 0% unhealthy - 2 (1 error * 2) = 98.
	assert.Equal(t, 98, report.HealthScore)
	assert.Equal(t, types.HealthLevelDegraded, report.Status, "errors present → degraded")
}

// TestBuildHealthReport_EventNotification — обычное notification событие.
func TestBuildHealthReport_EventNotification(t *testing.T) {
	config := &types.LoadBalancerConfig{
		Backends: []types.Backend{
			{ID: "b1", Host: "h1", OllamaPort: 11434, Status: types.StatusHealthy, Type: types.BackendTypeOllama},
		},
		API:  types.APISettings{RateLimit: 100, RateBurst: 200},
		Auth: types.AuthConfig{Enabled: false, Tokens: []string{"t"}, HeaderName: "X-API-Token"},
	}
	proxy := balancer.NewProxy(config)
	hc := balancer.NewHealthChecker(proxy, 10*time.Second, 3)
	server := NewServer(proxy, config, hc)
	server.SetEventBus(nil)

	// Имитируем notification от Proxy.EventBus (не transport_eof).
	server.eventsHub.buf.push(types.Event{
		Type:      types.EventNotification,
		Timestamp: time.Now(),
		BackendID: "b1",
		Severity:  types.SeverityError,
		Source:    "model_manager",
		Message:   "model load failed",
		Data: map[string]interface{}{
			"reason": "out of memory",
		},
	})

	report := server.buildHealthReport()

	require.Len(t, report.RecentErrors, 1)
	assert.Equal(t, types.ErrorSourceEvent, report.RecentErrors[0].Source)
	assert.Equal(t, "b1", report.RecentErrors[0].BackendID)
	assert.Equal(t, "error", report.RecentErrors[0].Severity)
	assert.Equal(t, 1, report.ErrorsBySource["event"])
}

// TestBuildHealthReport_InfoSeveritySkipped — info severity не попадает в errors.
func TestBuildHealthReport_InfoSeveritySkipped(t *testing.T) {
	config := &types.LoadBalancerConfig{
		Backends: []types.Backend{
			{ID: "b1", Host: "h1", OllamaPort: 11434, Status: types.StatusHealthy, Type: types.BackendTypeOllama},
		},
		API:  types.APISettings{RateLimit: 100, RateBurst: 200},
		Auth: types.AuthConfig{Enabled: false, Tokens: []string{"t"}, HeaderName: "X-API-Token"},
	}
	proxy := balancer.NewProxy(config)
	hc := balancer.NewHealthChecker(proxy, 10*time.Second, 3)
	server := NewServer(proxy, config, hc)
	server.SetEventBus(nil)

	server.eventsHub.buf.push(types.Event{
		Type:      types.EventNotification,
		Timestamp: time.Now(),
		BackendID: "b1",
		Severity:  types.SeverityInfo, // <-- info, должно быть пропущено
		Message:   "model loaded successfully",
	})

	report := server.buildHealthReport()
	assert.Empty(t, report.RecentErrors, "info severity must be filtered out")
	assert.Equal(t, types.HealthLevelHealthy, report.Status)
	assert.Equal(t, 100, report.HealthScore)
}

// TestBuildHealthReport_ErrorsBySource — мультиисточник агрегация.
func TestBuildHealthReport_ErrorsBySource(t *testing.T) {
	config := &types.LoadBalancerConfig{
		Backends: []types.Backend{
			{ID: "b1", Host: "h1", OllamaPort: 11434, Status: types.StatusHealthy, Type: types.BackendTypeLlamaCpp},
		},
		API:  types.APISettings{RateLimit: 100, RateBurst: 200},
		Auth: types.AuthConfig{Enabled: false, Tokens: []string{"t"}, HeaderName: "X-API-Token"},
	}
	proxy := balancer.NewProxy(config)
	hc := balancer.NewHealthChecker(proxy, 10*time.Second, 3)
	server := NewServer(proxy, config, hc)
	server.SetEventBus(nil)

	// 2 transport EOF, 1 event notification, 1 healthcheck.
	for i := 0; i < 2; i++ {
		server.eventsHub.buf.push(types.Event{
			Type:      types.EventNotification,
			Timestamp: time.Now().Add(-time.Duration(i) * time.Second),
			BackendID: "b1",
			Severity:  types.SeverityWarning,
			Message:   "EOF test",
			Data:      map[string]interface{}{"event_kind": "transport_eof", "path": "/v1/chat"},
		})
	}
	server.eventsHub.buf.push(types.Event{
		Type:      types.EventNotification,
		Timestamp: time.Now(),
		BackendID: "b1",
		Severity:  types.SeverityError,
		Message:   "model load failed",
	})

	report := server.buildHealthReport()

	assert.Len(t, report.RecentErrors, 3)
	assert.Equal(t, 2, report.ErrorsBySource["transport"])
	assert.Equal(t, 1, report.ErrorsBySource["event"])
	assert.Equal(t, 0, report.ErrorsBySource["healthcheck"])
}

// TestBuildHealthReport_ErrorsSortedByTimestampDESC — сортировка ошибок.
func TestBuildHealthReport_ErrorsSortedByTimestampDESC(t *testing.T) {
	config := &types.LoadBalancerConfig{
		Backends: []types.Backend{
			{ID: "b1", Host: "h1", OllamaPort: 11434, Status: types.StatusHealthy, Type: types.BackendTypeOllama},
		},
		API:  types.APISettings{RateLimit: 100, RateBurst: 200},
		Auth: types.AuthConfig{Enabled: false, Tokens: []string{"t"}, HeaderName: "X-API-Token"},
	}
	proxy := balancer.NewProxy(config)
	hc := balancer.NewHealthChecker(proxy, 10*time.Second, 3)
	server := NewServer(proxy, config, hc)
	server.SetEventBus(nil)

	now := time.Now()
	server.eventsHub.buf.push(types.Event{
		Type: types.EventNotification, Timestamp: now.Add(-30 * time.Second),
		BackendID: "b1", Severity: types.SeverityError, Message: "oldest",
	})
	server.eventsHub.buf.push(types.Event{
		Type: types.EventNotification, Timestamp: now.Add(-5 * time.Second),
		BackendID: "b1", Severity: types.SeverityError, Message: "middle",
	})
	server.eventsHub.buf.push(types.Event{
		Type: types.EventNotification, Timestamp: now,
		BackendID: "b1", Severity: types.SeverityError, Message: "newest",
	})

	report := server.buildHealthReport()

	require.Len(t, report.RecentErrors, 3)
	assert.Equal(t, "newest", report.RecentErrors[0].Message)
	assert.Equal(t, "middle", report.RecentErrors[1].Message)
	assert.Equal(t, "oldest", report.RecentErrors[2].Message)
}

// TestBuildHealthReport_BackendsSortedByID — сортировка бэкендов.
func TestBuildHealthReport_BackendsSortedByID(t *testing.T) {
	config := &types.LoadBalancerConfig{
		Backends: []types.Backend{
			{ID: "zulu", Host: "h", OllamaPort: 11434, Status: types.StatusHealthy, Type: types.BackendTypeOllama},
			{ID: "alpha", Host: "h", OllamaPort: 11435, Status: types.StatusHealthy, Type: types.BackendTypeOllama},
			{ID: "mike", Host: "h", OllamaPort: 11436, Status: types.StatusHealthy, Type: types.BackendTypeOllama},
		},
		API:  types.APISettings{RateLimit: 100, RateBurst: 200},
		Auth: types.AuthConfig{Enabled: false, Tokens: []string{"t"}, HeaderName: "X-API-Token"},
	}
	proxy := balancer.NewProxy(config)
	hc := balancer.NewHealthChecker(proxy, 10*time.Second, 3)
	server := NewServer(proxy, config, hc)
	server.SetEventBus(nil)

	report := server.buildHealthReport()

	require.Len(t, report.Backends, 3)
	assert.Equal(t, "alpha", report.Backends[0].ID)
	assert.Equal(t, "mike", report.Backends[1].ID)
	assert.Equal(t, "zulu", report.Backends[2].ID)
}

// TestBuildHealthReport_HealthCheckErrors — HealthChecker с fails → source=healthcheck.
func TestBuildHealthReport_HealthCheckErrors(t *testing.T) {
	config := &types.LoadBalancerConfig{
		Backends: []types.Backend{
			{ID: "b1", Host: "h1", OllamaPort: 11434, Status: types.StatusHealthy, Type: types.BackendTypeOllama},
		},
		API:  types.APISettings{RateLimit: 100, RateBurst: 200},
		Auth: types.AuthConfig{Enabled: false, Tokens: []string{"t"}, HeaderName: "X-API-Token"},
	}
	proxy := balancer.NewProxy(config)
	hc := balancer.NewHealthChecker(proxy, 10*time.Second, 3)
	server := NewServer(proxy, config, hc)
	server.SetEventBus(nil)

	// Напрямую модифицируем внутренний state HealthChecker (bypass HTTP call).
	hc.SetStatusForTest("b1", &balancer.HealthStatus{
		Healthy:          false,
		ConsecutiveFails: 3,
		LastError:        "connection refused",
		LastFailure:      time.Now(),
	})

	report := server.buildHealthReport()

	require.Len(t, report.RecentErrors, 1)
	assert.Equal(t, types.ErrorSourceHealthcheck, report.RecentErrors[0].Source)
	assert.Equal(t, "b1", report.RecentErrors[0].BackendID)
	assert.Equal(t, "connection refused", report.RecentErrors[0].Message)
	assert.Equal(t, 1, report.ErrorsBySource["healthcheck"])
}

// TestBuildHealthReport_RecentErrorsCapped — recentErrors ≤ 50.
func TestBuildHealthReport_RecentErrorsCapped(t *testing.T) {
	config := &types.LoadBalancerConfig{
		Backends: []types.Backend{
			{ID: "b1", Host: "h1", OllamaPort: 11434, Status: types.StatusHealthy, Type: types.BackendTypeOllama},
		},
		API:  types.APISettings{RateLimit: 100, RateBurst: 200},
		Auth: types.AuthConfig{Enabled: false, Tokens: []string{"t"}, HeaderName: "X-API-Token"},
	}
	proxy := balancer.NewProxy(config)
	hc := balancer.NewHealthChecker(proxy, 10*time.Second, 3)
	server := NewServer(proxy, config, hc)
	server.SetEventBus(nil)

	// Push 100 ошибок.
	for i := 0; i < 100; i++ {
		server.eventsHub.buf.push(types.Event{
			Type: types.EventNotification, Timestamp: time.Now().Add(-time.Duration(i) * time.Second),
			BackendID: "b1", Severity: types.SeverityError, Message: "err",
		})
	}

	report := server.buildHealthReport()
	assert.LessOrEqual(t, len(report.RecentErrors), 50, "recentErrors must be capped at 50")
}

// TestBuildHealthReport_NilHealthChecker — без healthChecker не должно падать.
func TestBuildHealthReport_NilHealthChecker(t *testing.T) {
	config := &types.LoadBalancerConfig{
		Backends: []types.Backend{
			{ID: "b1", Host: "h1", OllamaPort: 11434, Status: types.StatusHealthy, Type: types.BackendTypeOllama},
		},
		API:  types.APISettings{RateLimit: 100, RateBurst: 200},
		Auth: types.AuthConfig{Enabled: false, Tokens: []string{"t"}, HeaderName: "X-API-Token"},
	}
	proxy := balancer.NewProxy(config)
	server := NewServer(proxy, config, nil) // healthChecker = nil
	server.SetEventBus(nil)

	report := server.buildHealthReport()

	assert.Equal(t, types.HealthLevelHealthy, report.Status)
	require.Len(t, report.Backends, 1)
	assert.Equal(t, "unknown", report.Backends[0].Status, "no HC data → status=unknown")
}


package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/pkg/types"
)

// R-MultiHost (2026-10-07).
//
// Живой стенд из двух машин (192.168.13.20 + 192.168.13.34) с ОДИНАКОВЫМ
// compose: на обеих совпадали agentId ("cppworker-gpu-bundled-agent"),
// host ("cppworker-gpu") и cppWorkerPort (18092). Протокол регистрации не нёс
// ни одного признака, различающего узлы, поэтому вторая машина проходила и
// ветку dedup (FindBackendByHostPort), и ветку «backend exists», и молча
// переписывала AgentID/AgentPort/метрики записи первой машины.
//
// Наблюдалось: единственная llama_cpp-запись с URL первой машины показывала
// CPU/GPU ВТОРОЙ (i5-13420H, чужой GPU UUID) — «странное отображение» страницы
// модели, при этом вторая машина не обслуживала ни одного запроса, то есть
// выглядела как «не зарегистрировалась».
//
// Эти тесты фиксируют: узел-источник различает записи, живой владелец
// защищён (409), а осиротевшая запись освобождается (takeover).

func newGuardServer(t *testing.T) *Server {
	t.Helper()
	cfg := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "127.0.0.1", Port: 8080, APIPort: 8081},
	}
	p := balancer.NewProxy(cfg)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = p.Shutdown(ctx)
	})
	return NewServer(p, cfg, nil)
}

// registerRequest собирает POST /api/v1/agents/register с адресом узла.
func registerRequest(t *testing.T, agentID, host string, cppWorkerPort int, remoteAddr string) *http.Request {
	t.Helper()
	body := map[string]interface{}{
		"agentId":       agentID,
		"host":          host,
		"hostname":      host,
		"cppWorkerPort": cppWorkerPort,
		"backendType":   string(types.BackendTypeLlamaCpp),
		"labels":        []string{"linux", "gpu"},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/register", bytes.NewReader(raw))
	req.RemoteAddr = remoteAddr
	return req
}

func postRegister(t *testing.T, s *Server, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.agentRegisterHandler(rec, req)
	return rec
}

// TestRegistrationGuard_SecondHostRejected — вторая машина с тем же agentId,
// host и портом получает 409, а запись первой машины остаётся нетронутой.
func TestRegistrationGuard_SecondHostRejected(t *testing.T) {
	s := newGuardServer(t)

	first := postRegister(t, s, registerRequest(t,
		"cppworker-gpu-bundled-agent", "cppworker-gpu", 18092, "172.18.0.5:41234"))
	if first.Code != http.StatusOK && first.Code != http.StatusCreated {
		t.Fatalf("первая регистрация: получен %d, ожидался 200/201 (%s)", first.Code, first.Body.String())
	}

	// Агент первой машины жив: heartbeat только что.
	owner := s.proxy.GetBackend("cppworker-gpu-bundled-agent")
	if owner == nil {
		t.Fatal("бэкенд первой машины не создан")
	}
	s.proxy.TouchAgentContact("cppworker-gpu-bundled-agent")
	if got := owner.NodeAddr; got != "172.18.0.5" {
		t.Fatalf("NodeAddr первой машины = %q, ожидался 172.18.0.5", got)
	}

	second := postRegister(t, s, registerRequest(t,
		"cppworker-gpu-bundled-agent", "cppworker-gpu", 18092, "192.168.13.34:51000"))
	if second.Code != http.StatusConflict {
		t.Fatalf("регистрация второй машины: получен %d, ожидался 409 (%s)", second.Code, second.Body.String())
	}

	// Владелец записи не должен измениться.
	after := s.proxy.GetBackend("cppworker-gpu-bundled-agent")
	if after == nil {
		t.Fatal("запись владельца пропала после отказа")
	}
	if after.NodeAddr != "172.18.0.5" {
		t.Fatalf("NodeAddr после отказа = %q, ожидался 172.18.0.5 (захват не должен проходить)", after.NodeAddr)
	}
}

// TestRegistrationGuard_SameHostAllowed — повторная регистрация с ТОГО ЖЕ узла
// (штатный рестарт воркера) проходит: guard ловит только чужие узлы.
func TestRegistrationGuard_SameHostAllowed(t *testing.T) {
	s := newGuardServer(t)

	postRegister(t, s, registerRequest(t,
		"cppworker-gpu-bundled-agent", "cppworker-gpu", 18092, "172.18.0.5:41234"))
	s.proxy.TouchAgentContact("cppworker-gpu-bundled-agent")

	again := postRegister(t, s, registerRequest(t,
		"cppworker-gpu-bundled-agent", "cppworker-gpu", 18092, "172.18.0.5:41235"))
	if again.Code != http.StatusOK {
		t.Fatalf("повторная регистрация того же узла: получен %d, ожидался 200 (%s)", again.Code, again.Body.String())
	}
}

// TestRegistrationGuard_StaleOwnerTakeover — осиротевшая запись (агент молчит
// дольше окна) освобождается: пересозданный контейнер получает новый IP и
// обязан перерегистрироваться, иначе стенд заклинивает в 409 навсегда.
func TestRegistrationGuard_StaleOwnerTakeover(t *testing.T) {
	s := newGuardServer(t)

	postRegister(t, s, registerRequest(t,
		"cppworker-gpu-bundled-agent", "cppworker-gpu", 18092, "172.18.0.5:41234"))

	// Имитируем мёртвого владельца: контакт устарел за пределы окна.
	dead := s.proxy.GetBackend("cppworker-gpu-bundled-agent")
	if dead == nil {
		t.Fatal("бэкенд не создан")
	}
	dead.LastAgentContact = time.Now().Add(-10 * foreignRegistrantGrace)

	takeover := postRegister(t, s, registerRequest(t,
		"cppworker-gpu-bundled-agent", "cppworker-gpu", 18092, "192.168.13.34:51000"))
	if takeover.Code != http.StatusOK {
		t.Fatalf("takeover осиротевшей записи: получен %d, ожидался 200 (%s)", takeover.Code, takeover.Body.String())
	}
	after := s.proxy.GetBackend("cppworker-gpu-bundled-agent")
	if after.NodeAddr != "192.168.13.34" {
		t.Fatalf("NodeAddr после takeover = %q, ожидался 192.168.13.34", after.NodeAddr)
	}
}

// TestRegistrationGuard_LegacyRecordWithoutNodeAddrAllowed — записи, созданные
// до этой ревизии (NodeAddr пуст), не защищаются: поведение прежнее.
func TestRegistrationGuard_LegacyRecordWithoutNodeAddrAllowed(t *testing.T) {
	s := newGuardServer(t)

	if err := s.proxy.AddBackend(types.Backend{
		ID:               "cppworker-gpu-bundled-agent",
		Host:             "cppworker-gpu",
		CppWorkerPort:    18092,
		Type:             types.BackendTypeLlamaCpp,
		Status:           types.StatusHealthy,
		AgentID:          "cppworker-gpu-bundled-agent",
		LastAgentContact: time.Now(),
	}); err != nil {
		t.Fatalf("AddBackend: %v", err)
	}

	rec := postRegister(t, s, registerRequest(t,
		"cppworker-gpu-bundled-agent", "cppworker-gpu", 18092, "192.168.13.34:51000"))
	if rec.Code != http.StatusOK {
		t.Fatalf("регистрация в запись без NodeAddr: получен %d, ожидался 200 (%s)", rec.Code, rec.Body.String())
	}
}

// TestPeerIP_NormalizesIPv4Mapped — один и тот же узел не должен выглядеть как
// два разных из-за формы записи адреса (::ffff:… vs …).
func TestPeerIP_NormalizesIPv4Mapped(t *testing.T) {
	cases := map[string]string{
		"192.168.13.34:51000":          "192.168.13.34",
		"[::ffff:192.168.13.34]:51000": "192.168.13.34",
		"172.18.0.5:41234":             "172.18.0.5",
		"":                             "",
	}
	for addr, want := range cases {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req.RemoteAddr = addr
		if got := peerIP(req); got != want {
			t.Errorf("peerIP(%q) = %q, ожидался %q", addr, got, want)
		}
	}
}

// metricsRequest собирает POST метрик с заголовком X-Agent-ID.
func metricsRequest(t *testing.T, agentID, remoteAddr string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/metrics", bytes.NewReader([]byte("{}")))
	req.RemoteAddr = remoteAddr
	req.Header.Set("X-Agent-ID", agentID)
	return req
}

// TestTelemetryGuard_ForeignNodeRejected — метрики от чужого узла не должны
// попадать в запись. Это вторая половина фикса: агент регистрируется один раз,
// а метрики шлёт в цикле, поэтому запрет одной лишь регистрации не мешает
// чужой машине «перекрашивать» CPU/GPU чужой записи (наблюдалось на живой паре:
// значения переключались между машинами от замера к замеру).
func TestTelemetryGuard_ForeignNodeRejected(t *testing.T) {
	s := newGuardServer(t)

	postRegister(t, s, registerRequest(t,
		"cppworker-gpu-bundled-agent", "cppworker-gpu", 18092, "172.23.0.4:41234"))

	foreign := httptest.NewRecorder()
	s.agentMetricsHandler(foreign, metricsRequest(t, "cppworker-gpu-bundled-agent", "192.168.13.34:51000"))
	if foreign.Code != http.StatusForbidden {
		t.Fatalf("метрики от чужого узла: получен %d, ожидался 403 (%s)", foreign.Code, foreign.Body.String())
	}

	owner := httptest.NewRecorder()
	s.agentMetricsHandler(owner, metricsRequest(t, "cppworker-gpu-bundled-agent", "172.23.0.4:41235"))
	if owner.Code != http.StatusOK {
		t.Fatalf("метрики от узла-владельца: получен %d, ожидался 200 (%s)", owner.Code, owner.Body.String())
	}
}

// TestTelemetryGuard_ForeignHeartbeatRejected — heartbeat чужого узла не должен
// продлевать LastAgentContact: иначе запись никогда не считается осиротевшей и
// законный takeover после пересоздания контейнера невозможен.
func TestTelemetryGuard_ForeignHeartbeatRejected(t *testing.T) {
	s := newGuardServer(t)

	postRegister(t, s, registerRequest(t,
		"cppworker-gpu-bundled-agent", "cppworker-gpu", 18092, "172.23.0.4:41234"))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/heartbeat", bytes.NewReader([]byte("{}")))
	req.RemoteAddr = "192.168.13.34:51000"
	req.Header.Set("X-Agent-ID", "cppworker-gpu-bundled-agent")

	rec := httptest.NewRecorder()
	s.agentHeartbeatHandler(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("heartbeat от чужого узла: получен %d, ожидался 403 (%s)", rec.Code, rec.Body.String())
	}
}

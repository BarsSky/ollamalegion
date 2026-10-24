//go:build llama_stub

// rpc_layout_guard_r118_test.go — R118.2 (2026-10-14).
//
// ЖИВОЙ ДЕФЕКТ, который закрывает гард. Раскладка слоёв живёт в конфиге
// cppworker'а главного (rpcEnabled + список адресов rpc-server'ов). Если узел
// пропал, cppworker всё равно идёт за слоями по сети:
//
//	ggml-rpc.cpp:345: Remote RPC server crashed or returned malformed response
//	SIGABRT
//
// То есть недоступный узел роняет загрузку модели (и сам процесс cppworker)
// вместо локальной загрузки.
//
// Тесты фиксируют четыре состояния:
//   - все узлы живы        → rpcEnabled=true, полный список;
//   - часть узлов пропала  → rpcEnabled=true, только живые + причина;
//   - все узлы пропали     → rpcEnabled=false (грузим ЛОКАЛЬНО) + причина;
//   - роль «самодостаточный» → раскладку гасим, даже если узлы живы.
//
// Плюс: повторный вызов с тем же составом НЕ переписывает конфиг cppworker'а
// (иначе каждые 15 c шла бы запись файла), а вернувшийся узел включает раскладку
// обратно сам.
package balancer

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// rpcPushRecorder — подставной cppworker главного: пишет, что ему прислали.
type rpcPushRecorder struct {
	mu     sync.Mutex
	pushes []map[string]interface{}
}

func (r *rpcPushRecorder) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/api/v1/cppworker/config/update" {
			http.NotFound(w, req)
			return
		}
		raw, _ := io.ReadAll(req.Body)
		var body map[string]interface{}
		_ = json.Unmarshal(raw, &body)
		r.mu.Lock()
		r.pushes = append(r.pushes, body)
		r.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"applied":["rpcEnabled","rpcServers"]}`))
	}
}

func (r *rpcPushRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.pushes)
}

func (r *rpcPushRecorder) last() map[string]interface{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.pushes) == 0 {
		return nil
	}
	return r.pushes[len(r.pushes)-1]
}

func (r *rpcPushRecorder) servers() []string {
	last := r.last()
	if last == nil {
		return nil
	}
	raw, _ := last["rpcServers"].([]interface{})
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func (r *rpcPushRecorder) enabled() bool {
	last := r.last()
	if last == nil {
		return false
	}
	b, _ := last["rpcEnabled"].(bool)
	return b
}

// newLayoutGuardTestProxy — прокси с главным (указанным на подставной cppworker)
// и донором (его адрес = адрес подставного cppworker, чтобы все пробежки были
// предсказуемы), плюс подменяемая проверка доступности узлов.
func newLayoutGuardTestProxy(t *testing.T, mainRole, donorRole string) (*Proxy, *rpcPushRecorder) {
	t.Helper()

	rec := &rpcPushRecorder{}
	srv := httptest.NewServer(rec.handler())
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse test server url: %v", err)
	}
	port, _ := strconv.Atoi(u.Port())

	p := &Proxy{
		config:     &types.LoadBalancerConfig{},
		backends:   map[string]*BackendState{},
		metricsMgr: NewMetricsManager(),
	}
	p.backends["main-1"] = &BackendState{Backend: &types.Backend{
		ID: "main-1", Host: u.Hostname(), CppWorkerPort: port,
		Status: types.StatusHealthy, Type: types.BackendTypeLlamaCpp, Role: mainRole,
	}}
	p.backends["donor-1"] = &BackendState{Backend: &types.Backend{
		ID: "donor-1", Host: "192.0.2.77", CppWorkerPort: 18092,
		Status: types.StatusHealthy, Type: types.BackendTypeLlamaCpp, Role: donorRole,
	}}
	return p, rec
}

// setReachability — подменить проверку доступности: живыми считаются переданные
// адреса. Адрес донора — 192.0.2.77:<LB_RPC_NODE_PORT> (по умолчанию 50052).
func setReachability(p *Proxy, alive ...string) {
	aliveSet := map[string]bool{}
	for _, a := range alive {
		aliveSet[a] = true
	}
	p.rpcProbe = func(addr string, _ time.Duration) bool { return aliveSet[addr] }
}

const testDonorAddr = "192.0.2.77:50052"

// TestR118_2_LayoutAllNodesAlive_FullList — узел жив: раскладка включена полным
// списком, деградации нет.
func TestR118_2_LayoutAllNodesAlive_FullList(t *testing.T) {
	p, rec := newLayoutGuardTestProxy(t, types.BackendRoleMain, types.BackendRoleDonor)
	setReachability(p, testDonorAddr)

	degraded, reason := p.EnsureRPCLayout("main-1")
	if degraded {
		t.Fatalf("деградация при живом узле: %s", reason)
	}
	if !rec.enabled() {
		t.Fatal("раскладка не включена у главного при живом узле")
	}
	if got := rec.servers(); len(got) != 1 || got[0] != testDonorAddr {
		t.Fatalf("список узлов = %v, want [%s]", got, testDonorAddr)
	}
}

// TestR118_2_LayoutAllNodesDead_LoadsLocally — узел недоступен: раскладка
// ВЫКЛЮЧАЕТСЯ (загрузка пойдёт локально), а не остаётся с мёртвым адресом.
// Именно это роняло cppworker («Remote RPC server crashed»).
func TestR118_2_LayoutAllNodesDead_LoadsLocally(t *testing.T) {
	p, rec := newLayoutGuardTestProxy(t, types.BackendRoleMain, types.BackendRoleDonor)
	setReachability(p) // живых нет

	degraded, reason := p.EnsureRPCLayout("main-1")
	if !degraded {
		t.Fatal("нет пометки деградации, хотя узел раскладки недоступен")
	}
	if !strings.Contains(reason, testDonorAddr) {
		t.Errorf("причина не называет недоступный узел: %q", reason)
	}
	if !strings.Contains(reason, "локально") {
		t.Errorf("причина не объясняет локальную загрузку: %q", reason)
	}
	if rec.enabled() {
		t.Fatal("раскладка осталась включённой с мёртвым узлом — это и есть живой дефект")
	}
	if got := rec.servers(); len(got) != 0 {
		t.Errorf("список узлов = %v, want пусто", got)
	}

	// Повторный вызов с тем же состоянием конфиг не переписывает.
	if n := rec.count(); n != 1 {
		t.Fatalf("первый вызов сделал %d записей конфига, want 1", n)
	}
	p.EnsureRPCLayout("main-1")
	if n := rec.count(); n != 1 {
		t.Errorf("повторный вызов переписал конфиг (%d записей) — cppworker сохраняет его на диск", n)
	}
	// Состояние видно оператору.
	st := p.LayoutGuardStatus("main-1")
	if !st.Degraded || st.Reason == "" {
		t.Errorf("LayoutGuardStatus не сообщает о деградации: %+v", st)
	}
}

// TestR118_2_LayoutPartial_OnlyAliveNodes — часть узлов пропала: работаем на
// живых, но помечаем деградацию.
func TestR118_2_LayoutPartial_OnlyAliveNodes(t *testing.T) {
	p, rec := newLayoutGuardTestProxy(t, types.BackendRoleMain, types.BackendRoleDonor)
	// Второй донор — недоступный.
	p.backends["donor-2"] = &BackendState{Backend: &types.Backend{
		ID: "donor-2", Host: "192.0.2.88", CppWorkerPort: 18092,
		Status: types.StatusHealthy, Type: types.BackendTypeLlamaCpp, Role: types.BackendRoleDonor,
	}}
	setReachability(p, testDonorAddr)

	degraded, reason := p.EnsureRPCLayout("main-1")
	if !degraded {
		t.Fatal("часть узлов недоступна, но деградации нет")
	}
	if !strings.Contains(reason, "192.0.2.88:50052") {
		t.Errorf("причина не называет недоступный узел: %q", reason)
	}
	if !rec.enabled() {
		t.Fatal("раскладка выключена полностью, хотя один узел жив")
	}
	if got := rec.servers(); len(got) != 1 || got[0] != testDonorAddr {
		t.Fatalf("список узлов = %v, want только живой [%s]", got, testDonorAddr)
	}
}

// TestR118_2_LayoutRestoredWhenNodeReturns — узел вернулся: следующий проход
// включает раскладку обратно сам, без действий оператора.
func TestR118_2_LayoutRestoredWhenNodeReturns(t *testing.T) {
	p, rec := newLayoutGuardTestProxy(t, types.BackendRoleMain, types.BackendRoleDonor)
	setReachability(p)
	if degraded, _ := p.EnsureRPCLayout("main-1"); !degraded {
		t.Fatal("ожидалась деградация при мёртвом узле")
	}
	if rec.enabled() {
		t.Fatal("раскладка не выключена при мёртвом узле")
	}

	// Узел вернулся.
	setReachability(p, testDonorAddr)
	degraded, reason := p.EnsureRPCLayout("main-1")
	if degraded {
		t.Fatalf("узел вернулся, но осталась деградация: %s", reason)
	}
	if !rec.enabled() {
		t.Fatal("раскладка не включена обратно после возврата узла")
	}
	if got := rec.servers(); len(got) != 1 || got[0] != testDonorAddr {
		t.Fatalf("список узлов после возврата = %v, want [%s]", got, testDonorAddr)
	}
	if st := p.LayoutGuardStatus("main-1"); st.Degraded {
		t.Errorf("состояние осталось деградированным: %+v", st)
	}
}

// TestR118_2_StandaloneDisablesLayout — роль «самодостаточный» гасит раскладку,
// даже если узлы живы: так решил оператор. Деградацией это не считается.
func TestR118_2_StandaloneDisablesLayout(t *testing.T) {
	p, rec := newLayoutGuardTestProxy(t, types.BackendRoleStandalone, types.BackendRoleDonor)
	setReachability(p, testDonorAddr)

	degraded, reason := p.EnsureRPCLayout("main-1")
	if degraded {
		t.Fatalf("самодостаточный режим помечен деградацией: %s", reason)
	}
	if rec.enabled() {
		t.Fatal("раскладка включена у самодостаточного бэкенда")
	}
}

// TestR118_2_DonorGetsLayoutDisabled — донор не потребитель слоёв: если раскладка
// у него осталась с прежней роли, гард её гасит.
func TestR118_2_DonorGetsLayoutDisabled(t *testing.T) {
	p, rec := newLayoutGuardTestProxy(t, types.BackendRoleMain, types.BackendRoleDonor)
	// Донор указывает на тот же подставной cppworker, что и главный.
	donor := p.GetBackend("donor-1")
	main := p.GetBackend("main-1")
	donor.Host = main.Host
	donor.CppWorkerPort = main.CppWorkerPort
	setReachability(p, testDonorAddr)

	if degraded, reason := p.EnsureRPCLayout("donor-1"); degraded {
		t.Fatalf("донор помечен деградацией: %s", reason)
	}
	if rec.enabled() {
		t.Fatal("у донора осталась включённой раскладка")
	}
}

// TestR118_2_LayoutKey — ключ состояния: порядок узлов не важен, «выключено»
// всегда одно и то же.
func TestR118_2_LayoutKey(t *testing.T) {
	a := layoutKey(true, []string{"b:1", "a:2"})
	b := layoutKey(true, []string{"a:2", "b:1"})
	if a != b {
		t.Errorf("ключ зависит от порядка узлов: %q vs %q", a, b)
	}
	if layoutKey(false, []string{"a:2"}) != "off" {
		t.Error("выключенная раскладка должна давать ключ off")
	}
	if layoutKey(true, nil) != "off" {
		t.Error("раскладка без узлов должна давать ключ off")
	}
}

// TestR118_2_ProbeSplitsNodes — проверка доступности делит узлы на живые/мёртвые
// и не зависит от порядка входа.
func TestR118_2_ProbeSplitsNodes(t *testing.T) {
	probe := func(addr string, _ time.Duration) bool { return addr == "ok:1" }
	alive, dead := probeRPCNodes(probe, []string{"bad:2", "ok:1", "bad:1"})
	if len(alive) != 1 || alive[0] != "ok:1" {
		t.Errorf("живые = %v, want [ok:1]", alive)
	}
	if len(dead) != 2 || dead[0] != "bad:1" || dead[1] != "bad:2" {
		t.Errorf("мёртвые = %v, want [bad:1 bad:2]", dead)
	}
}

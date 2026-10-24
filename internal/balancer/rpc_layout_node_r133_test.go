//go:build llama_stub

// rpc_layout_node_r133_test.go — R133 (2026-10-16): донор поднимает свой узел
// раскладки по ПРИКАЗУ БАЛАНСЕРА.
//
// ЖИВОЙ ДЕФЕКТ, который закрывают эти тесты. Роль `donor` в панели существовала
// с R118 и работала только в одну сторону: главному отправлялся список узлов, а
// сам донор ничего не получал. Его rpc-server приходилось поднимать руками —
// переменной CPPWORKER_RPC_SERVER_ENABLED=true на донорской машине или тумблером
// в WebUI на месте. То есть «в панели роль есть, а узел не работает», и оператор
// должен помнить про отдельный обязательный параметр на другом хосте.
//
// Проверяется:
//  1. донору уходит приказ поднять узел (rpcServerEnabled=true + порт);
//  2. бэкенд без роли донора НЕ трогается — ручной тумблер оператора не затаптываем;
//  3. снятие роли гасит узел, но только если поднимали его МЫ;
//  4. подтверждённый воркером узел не переприказывается каждый тик (иначе на
//     каждом проходе шла бы запись файла на доноре);
//  5. «узел поднят, но с балансера недоступен» объясняется словами, а не молчанием
//     (порт не опубликован при запуске контейнера — частая причина);
//  6. недоставленный приказ назван причиной, а не выдаётся за успех.
package balancer

import (
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// newRPCNodeTestProxy — прокси с одним бэкендом, чей cppworker — подставной
// HTTP-сервер (он же записывает приказы). Роль задаётся вызывающим.
func newRPCNodeTestProxy(t *testing.T, role string) (*Proxy, *rpcPushRecorder) {
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
	p.backends["node-1"] = &BackendState{Backend: &types.Backend{
		ID: "node-1", Host: u.Hostname(), CppWorkerPort: port,
		Status: types.StatusHealthy, Type: types.BackendTypeLlamaCpp, Role: role,
	}}
	return p, rec
}

// nodePushes — приказы, адресованные УЗЛУ (в отличие от приказов главному,
// которые несут rpcEnabled/rpcServers).
func nodePushes(rec *rpcPushRecorder) []map[string]interface{} {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	var out []map[string]interface{}
	for _, push := range rec.pushes {
		if _, ok := push["rpcServerEnabled"]; ok {
			out = append(out, push)
		}
	}
	return out
}

// reportNodeUp — воркер сообщил, что узел поднят и порт слушается.
func reportNodeUp(p *Proxy, backendID string, port int) {
	p.metricsMgr.mu.Lock()
	defer p.metricsMgr.mu.Unlock()
	p.metricsMgr.llamaMetrics[backendID] = &types.LlamaCppMetrics{
		RPCNode: &types.RPCNodeStatus{Enabled: true, Running: true, Listening: true, Port: port},
	}
}

// TestR133_DonorReceivesOrderToServe — главное: роль donor приводит к приказу
// поднять узел, а не остаётся надписью в панели.
func TestR133_DonorReceivesOrderToServe(t *testing.T) {
	p, rec := newRPCNodeTestProxy(t, types.BackendRoleDonor)

	changed, reason := p.EnsureRPCNodeServing("node-1")
	if !changed {
		t.Fatalf("приказ донору не отправлен (reason=%q)", reason)
	}
	pushes := nodePushes(rec)
	if len(pushes) != 1 {
		t.Fatalf("приказов узлу = %d, want 1", len(pushes))
	}
	if enabled, _ := pushes[0]["rpcServerEnabled"].(bool); !enabled {
		t.Errorf("rpcServerEnabled = %v, want true: донор обязан ОТДАВАТЬ GPU", pushes[0]["rpcServerEnabled"])
	}
	if port, _ := pushes[0]["rpcServerPort"].(float64); int(port) != 50052 {
		t.Errorf("rpcServerPort = %v, want 50052 (LB_RPC_NODE_PORT)", pushes[0]["rpcServerPort"])
	}
	// Отчёт до отправки подтверждения честно говорит «ждём подтверждения», а не
	// выдаёт приказ за готовность.
	rep := p.RPCNodeReport("node-1")
	if !rep.Wanted {
		t.Error("RPCNodeReport.Wanted=false у донора: панель не поймёт, что узел обязан быть поднят")
	}
	if rep.Reason == "" {
		t.Error("нет причины «ждём подтверждения»: приказ выдан за готовность")
	}
}

// TestR133_NonDonorIsNotTouched — бэкенд без роли донора не получает приказов:
// ручной тумблер оператора балансер не отменяет.
func TestR133_NonDonorIsNotTouched(t *testing.T) {
	for _, role := range []string{"", types.BackendRoleStandalone, types.BackendRoleMain} {
		p, rec := newRPCNodeTestProxy(t, role)
		changed, reason := p.EnsureRPCNodeServing("node-1")
		if changed || reason != "" {
			t.Errorf("роль %q: changed=%v reason=%q, ожидалось «не трогаем»", role, changed, reason)
		}
		if n := rec.count(); n != 0 {
			t.Errorf("роль %q: отправлено %d приказов, ожидалось 0", role, n)
		}
	}
}

// TestR133_RoleRemovedTurnsNodeOffOnce — снятие роли гасит узел, который подняли
// МЫ, и только один раз (дальше помним, что гасить больше нечего).
func TestR133_RoleRemovedTurnsNodeOffOnce(t *testing.T) {
	p, rec := newRPCNodeTestProxy(t, types.BackendRoleDonor)
	if changed, _ := p.EnsureRPCNodeServing("node-1"); !changed {
		t.Fatal("подготовка: узел не поднят по роли донора")
	}

	if !p.SetBackendRole("node-1", types.BackendRoleStandalone) {
		t.Fatal("не удалось сменить роль в тестовом прокси")
	}
	changed, reason := p.EnsureRPCNodeServing("node-1")
	if !changed {
		t.Fatalf("узел не погашен после снятия роли донора (reason=%q)", reason)
	}
	pushes := nodePushes(rec)
	if len(pushes) != 2 {
		t.Fatalf("приказов узлу = %d, want 2 (поднять + погасить)", len(pushes))
	}
	if enabled, _ := pushes[1]["rpcServerEnabled"].(bool); enabled {
		t.Error("последний приказ включил узел вместо выключения")
	}

	// Повторный проход ничего не отправляет: мы уже погасили то, что поднимали.
	if changed, _ := p.EnsureRPCNodeServing("node-1"); changed {
		t.Error("повторное гашение: приказ отправлен ещё раз")
	}
	if n := len(nodePushes(rec)); n != 2 {
		t.Errorf("приказов узлу = %d, want 2 (лишняя запись файла на доноре)", n)
	}
}

// TestR133_NonDonorNodeIsNotTurnedOff — если узел поднял ОПЕРАТОР на бэкенде без
// роли донора, балансер его не гасит: это его решение, а не наше.
func TestR133_NonDonorNodeIsNotTurnedOff(t *testing.T) {
	p, rec := newRPCNodeTestProxy(t, types.BackendRoleStandalone)
	reportNodeUp(p, "node-1", 50052)

	if changed, reason := p.EnsureRPCNodeServing("node-1"); changed || reason != "" {
		t.Errorf("changed=%v reason=%q, ожидалось «не трогаем»", changed, reason)
	}
	if n := rec.count(); n != 0 {
		t.Errorf("отправлено %d приказов, ожидалось 0 (оператор поднял узел сам)", n)
	}
}

// TestR133_ConfirmedNodeIsNotRePushed — воркер подтвердил узел: приказ не
// повторяется (иначе каждые 15 c запись файла на доноре), а доступность с
// балансера проверяется и попадает в отчёт.
func TestR133_ConfirmedNodeIsNotRePushed(t *testing.T) {
	p, rec := newRPCNodeTestProxy(t, types.BackendRoleDonor)
	reportNodeUp(p, "node-1", 50052)
	setReachability(p, p.rpcNodeAddr(p.GetBackend("node-1")))

	changed, reason := p.EnsureRPCNodeServing("node-1")
	if changed {
		t.Error("узел уже поднят и подтверждён — приказ не нужен")
	}
	if reason != "" {
		t.Errorf("причина %q при работающем узле", reason)
	}
	if n := rec.count(); n != 0 {
		t.Errorf("отправлено %d приказов, ожидалось 0", n)
	}
	rep := p.RPCNodeReport("node-1")
	if !rep.Running || !rep.Listening || !rep.Reachable {
		t.Errorf("отчёт не показывает подтверждённый узел: %+v", rep)
	}
}

// TestR133_UnreachableNodeIsExplained — «поднят» и «доступен» разные вещи:
// процесс слушает внутри контейнера, но порт не опубликован — панель обязана
// сказать это словами.
func TestR133_UnreachableNodeIsExplained(t *testing.T) {
	p, _ := newRPCNodeTestProxy(t, types.BackendRoleDonor)
	reportNodeUp(p, "node-1", 50052)
	setReachability(p) // с балансера недоступен

	_, reason := p.EnsureRPCNodeServing("node-1")
	if !strings.Contains(reason, "недоступен") {
		t.Errorf("причина не объясняет недоступность: %q", reason)
	}
	if !strings.Contains(reason, "порт") {
		t.Errorf("причина не называет вероятную причину (публикация порта): %q", reason)
	}
	rep := p.RPCNodeReport("node-1")
	if rep.Reachable {
		t.Error("RPCNodeReport.Reachable=true при недоступном узле")
	}
	if rep.Reason == "" {
		t.Error("RPCNodeReport не несёт причину для панели")
	}
}

// TestR133_OrderIsNotRepeatedWhenWorkerStaysSilent — воркер не отчитывается
// (старый образ): приказ отправляется один раз, а не каждый тик.
//
// ЖИВАЯ НАХОДКА (2026-10-16): первый прогон отправлял приказ каждые 15 с, потому
// что не видел подтверждения. Воркер записывает приказ в файл на диск — повтор без
// новой информации означал бы запись каждые 15 с на донорской машине.
func TestR133_OrderIsNotRepeatedWhenWorkerStaysSilent(t *testing.T) {
	p, rec := newRPCNodeTestProxy(t, types.BackendRoleDonor)

	if changed, _ := p.EnsureRPCNodeServing("node-1"); !changed {
		t.Fatal("первый приказ не отправлен")
	}
	if changed, reason := p.EnsureRPCNodeServing("node-1"); changed {
		t.Errorf("приказ повторён сразу же (reason=%q)", reason)
	}
	if n := len(nodePushes(rec)); n != 1 {
		t.Errorf("приказов узлу = %d, want 1 (повтор без подтверждения — лишняя запись на доноре)", n)
	}
}

// TestR133_OfflineDonorIsNotHammered — узел, о котором балансер уже знает, что он
// не в сети: приказ не отправляется, но роль остаётся и причина названа.
//
// ЖИВАЯ НАХОДКА (2026-10-16): офлайн-донор держал периодический проход 20 с
// (ожидание ответа HTTP), из-за чего приказ мастеру уходил с задержкой. При этом
// доктрина R118.10 требует НЕ снимать роль: оператор может настраивать узел.
func TestR133_OfflineDonorIsNotHammered(t *testing.T) {
	p, rec := newRPCNodeTestProxy(t, types.BackendRoleDonor)
	p.backends["node-1"].Backend.Status = types.StatusUnhealthy

	changed, reason := p.EnsureRPCNodeServing("node-1")
	if changed {
		t.Error("приказ отправлен недоступному узлу — он всё равно не дойдёт")
	}
	if !strings.Contains(reason, "не в сети") {
		t.Errorf("причина не объясняет, почему узел не поднят: %q", reason)
	}
	if n := rec.count(); n != 0 {
		t.Errorf("отправлено %d запросов, ожидалось 0", n)
	}
	if rep := p.RPCNodeReport("node-1"); !rep.Wanted {
		t.Error("роль донора потеряна: оператор настраивает узел, роль снимать нельзя")
	}
}

// TestR133_FailedOrderIsReported — приказ не доставлен: это причина, а не тихий
// успех. Иначе панель покажет «донор» при выключенном узле и никто не поймёт.
func TestR133_FailedOrderIsReported(t *testing.T) {
	p, _ := newRPCNodeTestProxy(t, types.BackendRoleDonor)
	// Порт, на котором заведомо никто не слушает (порт 1 + запрет привилегий).
	p.backends["node-1"].Backend.CppWorkerPort = 1

	changed, reason := p.EnsureRPCNodeServing("node-1")
	if changed {
		t.Error("changed=true при недоставленном приказе")
	}
	if !strings.Contains(reason, "не удалось поднять узел") {
		t.Errorf("причина не называет отказ доставки: %q", reason)
	}
	if rep := p.RPCNodeReport("node-1"); rep.Reason == "" {
		t.Error("отказ доставки не попал в отчёт для панели")
	}
}

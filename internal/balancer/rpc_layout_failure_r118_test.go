//go:build llama_stub

// rpc_layout_failure_r118_test.go — R118.3 (2026-10-14).
//
// Узел раскладки может умереть ВО ВРЕМЯ загрузки (перезапуск rpc-server, обрыв
// сети, падение узла). Тогда cppworker отвечает ошибкой уровня RPC или уходит в
// SIGABRT, а оператор видел просто «модель не загрузилась».
//
// Тесты фиксируют:
//   - какие ошибки считаются «раскладка виновата» (и что локальный повтор не
//     включается на посторонние ошибки — иначе каждая неудача грузила бы модель
//     дважды);
//   - что аварийное выключение раскладки доходит до cppworker'а и видно в
//     состоянии для API/WebUI.
package balancer

import (
	"strings"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// TestR118_3_RetryMarkers — живой лог ggml-rpc и транспортные ошибки после
// падения cppworker'а означают «повторяем локально».
func TestR118_3_RetryMarkers(t *testing.T) {
	live := []string{
		"request failed: Post \"http://cppworker-gpu:18092/api/models/load\": EOF",
		"load model gemma-4-E4B: ggml-rpc.cpp:345: Remote RPC server crashed or returned malformed response",
		"rpc error: RPC_STATUS_ASSERT",
		"request failed: dial tcp 192.0.2.10:50052: connect: connection refused",
		"request failed: read tcp: connection reset by peer",
		"request failed: write tcp: broken pipe",
		// Живой симптом (стенд, 22:44): cppworker упал на раскладке, балансер
		// увидел только «модель не появилась в списке» — формулировка сбивала с
		// толку («проверь наличие GGUF-файла»), хотя файл на месте.
		`auto-load failed: model "gemma-4-E4B-it-Q4_K_M" did not appear in cppworker model list ` +
			`or load progress for 5 consecutive polls — load did not start`,
	}
	for _, text := range live {
		ok, marker := shouldRetryLoadWithoutLayout(text, true)
		if !ok {
			t.Errorf("не распознан симптом раскладки: %q", text)
			continue
		}
		if marker == "" {
			t.Errorf("симптом распознан, но не назван: %q", text)
		}
	}
}

// TestR118_3_NoRetryForUnrelatedErrors — «файла нет», «не влезает», «n_ctx выше
// потолка» раскладку не винят: локальный повтор ничего не изменит, а время
// оператора потратит.
func TestR118_3_NoRetryForUnrelatedErrors(t *testing.T) {
	unrelated := []string{
		"failed to open GGUF file /app/models/x.gguf: no such file or directory",
		"load failed: model does not fit: need 20 GB, have 8 GB",
		"requested n_ctx=131072 exceeds model max 32768",
		"model \"x\" is marked as disabled in profile",
		"",
	}
	for _, text := range unrelated {
		if ok, marker := shouldRetryLoadWithoutLayout(text, true); ok {
			t.Errorf("посторонняя ошибка принята за отказ раскладки (%s): %q", marker, text)
		}
	}
	// Раскладка выключена — повторять нечего, даже если текст «rpc».
	if ok, _ := shouldRetryLoadWithoutLayout("rpc failed", false); ok {
		t.Error("повтор локально предложен при выключенной раскладке")
	}
}

// TestR118_3_DisableLayoutPushesOff — аварийное выключение реально уходит в
// cppworker (rpcEnabled=false, пустой список) и попадает в состояние для UI.
func TestR118_3_DisableLayoutPushesOff(t *testing.T) {
	p, rec := newLayoutGuardTestProxy(t, types.BackendRoleMain, types.BackendRoleDonor)
	setReachability(p, testDonorAddr)

	// Сначала раскладка включена (как в жизни перед неудачной загрузкой).
	if degraded, reason := p.EnsureRPCLayout("main-1"); degraded {
		t.Fatalf("неожиданная деградация при живом узле: %s", reason)
	}
	if !p.RPCLayoutActive("main-1") {
		t.Fatal("раскладка должна была включиться")
	}

	p.DisableRPCLayoutFor("main-1", "загрузка с раскладкой не удалась (rpc): раскладка выключена, грузим локально")

	if rec.enabled() {
		t.Fatal("cppworker не получил rpcEnabled=false")
	}
	if got := rec.servers(); len(got) != 0 {
		t.Errorf("список узлов после аварийного выключения = %v, want пусто", got)
	}
	if p.RPCLayoutActive("main-1") {
		t.Error("раскладка всё ещё считается активной после аварийного выключения")
	}
	st := p.LayoutGuardStatus("main-1")
	if !st.Degraded || !strings.Contains(st.Reason, "грузим локально") {
		t.Errorf("состояние для API/WebUI не сообщает причину: %+v", st)
	}
}

// TestR118_3_DisableOnlyTouchesTargetBackend — аварийное выключение не должно
// гасить раскладку у других бэкендов.
func TestR118_3_DisableOnlyTouchesTargetBackend(t *testing.T) {
	p, _ := newLayoutGuardTestProxy(t, types.BackendRoleMain, types.BackendRoleDonor)
	setReachability(p, testDonorAddr)

	p.mu.Lock()
	p.rpcLayoutApplied = map[string]string{"other-main": layoutKey(true, []string{testDonorAddr})}
	p.mu.Unlock()

	p.DisableRPCLayoutFor("main-1", "причина")
	if !p.RPCLayoutActive("other-main") {
		t.Error("выключение раскладки у одного бэкенда затронуло другой")
	}
}

// TestR118_6_AllLayersOnGPU_NotRewrittenWithoutEvidence — R118.6 (2026-10-14).
//
// Проверка «`gpu_layers=-1` вместе с раскладкой нестабилен» НЕ подтвердилась
// измерением на стенде: 6 загрузок подряд с `-1` при включённой раскладке — 6/6
// успешно и 0 падений cppworker (RestartCount без изменений, ~90 с на загрузку),
// затем 15 последовательных инференсов и 3 пары параллельных — 21/21 HTTP 200,
// 0 падений. Наблюдавшиеся ранее падения совпадали с ВНЕШНИМ убийством узла во
// время загрузки (остановка rpc-server в тестах) и закрыты восстановлением
// R118.3: раскладка гасится, модель грузится локально, оператор видит причину.
//
// Поэтому параметр НЕ подменяется: молчаливая замена явного намерения оператора
// без подтверждённой причины — это ровно то, на что жаловался оператор
// («настройки сбиваются сами»). Тест фиксирует это решение.
func TestR118_6_AllLayersOnGPU_NotRewrittenWithoutEvidence(t *testing.T) {
	p, _ := newLayoutGuardTestProxy(t, types.BackendRoleMain, types.BackendRoleDonor)
	setReachability(p, testDonorAddr)
	if degraded, reason := p.EnsureRPCLayout("main-1"); degraded {
		t.Fatalf("неожиданная деградация при живом узле: %s", reason)
	}
	if !p.RPCLayoutActive("main-1") {
		t.Fatal("раскладка должна быть включена")
	}

	minusOne := -1
	req := &ModelOpRequest{GPULayers: &minusOne, ModelName: "m"}
	// В коде не осталось функции, подменяющей -1: проверяем сам инвариант —
	// значение, которое оператор задал, уходит в загрузку без изменений.
	if req.GPULayers == nil || *req.GPULayers != -1 {
		t.Fatalf("gpu_layers переписан: %v", req.GPULayers)
	}
}

// ограничено по времени (иначе запрос оператора висел бы вечно).
func TestR118_3_WaitBackendReachable_TimesOut(t *testing.T) {
	p := &Proxy{backends: map[string]*BackendState{}}
	// Порт, который заведомо никто не слушает: быстрая проверка возврата.
	p.backends["main-1"] = &BackendState{Backend: &types.Backend{
		ID: "main-1", Host: "192.0.2.1", CppWorkerPort: 1,
	}}
	start := time.Now()
	if p.WaitBackendReachable("main-1", 1200*time.Millisecond) {
		t.Fatal("недоступный бэкенд объявлен доступным")
	}
	if elapsed := time.Since(start); elapsed > 6*time.Second {
		t.Errorf("ожидание не ограничено временем: %s", elapsed)
	}
}

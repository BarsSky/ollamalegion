// nctx_reload_guard_r69_test.go — R69 (2026-09-23): обработчик 413 от cppworker
// не должен запускать reload МЕНЬШЕ уже известного n_ctx.
//
// ЖИВОЙ СТЕНД: модель загружена на 65536 (координатор это знает), но bridge_info
// пришедшего 413 сообщал current_n_ctx=8192 (effective n_ctx того запроса, до
// балансерного апгрейда). План строился как max(required, loaded*2) = 16384 →
// балансер запускал «reload вниз» 65536 → 16384, следующий запрос клиента снова
// требовал 65536 → ещё reload → качели и 503 у каждого запроса.
//
// Теперь: если LastKnownNCtx уже покрывает цель плана — загрузка не запускается,
// клиент получает понятный 503 с фактическим n_ctx.
package balancer

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestHandleNCtxReloadAsync_R69_SkipsPointlessDowngrade — known=65536,
// план=16384 → reload не запускается, ответ объясняет фактический n_ctx.
func TestHandleNCtxReloadAsync_R69_SkipsPointlessDowngrade(t *testing.T) {
	p := admissionTestProxy(t, 4)
	const modelName = "gemma"
	p.nctxReload.SetLastKnownNCtx("llama_adm", 65536)

	plan := &ReloadPlan{Decision: DecisionReload, NewNCtx: 16384, Reason: "prompt_too_long"}
	bridgeErr := &NCtxBridgeError{CurrentNCtx: 8192, RequiredNCtx: 9136, MaxVRAMNCtx: 30339}
	rec := httptest.NewRecorder()

	p.handleNCtxReloadActualAsync("llama_adm", modelName, plan, "http://127.0.0.1:1", bridgeErr, rec)

	if rec.Code != 503 {
		t.Fatalf("ожидался 503, получено %d: %s", rec.Code, rec.Body.String())
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("тело не JSON: %v (%s)", err, rec.Body.String())
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "65536") {
		t.Errorf("в ответе должно быть фактическое n_ctx=65536, получено: %s", msg)
	}
	if target, _ := body["target_n_ctx"].(float64); int(target) != 65536 {
		t.Errorf("target_n_ctx = %v, ожидалось 65536 (фактический n_ctx)", body["target_n_ctx"])
	}
	// Главное: загрузка не запущена — реестр reload'ов пуст.
	if p.nctxReload.IsReloadPending("llama_adm", modelName) {
		t.Error("reload не должен запускаться, когда known n_ctx уже покрывает цель")
	}
}

// TestHandleNCtxReloadAsync_R69_StillReloadsWhenNeeded — если known n_ctx МЕНЬШЕ
// цели, reload запускается как раньше (регрессия не внесена).
func TestHandleNCtxReloadAsync_R69_StillReloadsWhenNeeded(t *testing.T) {
	const modelName = "test-model"
	cppWorker, _, reloadCalls := makeMockCppWorkerWithReload(t, modelName, 8192)
	p := buildProxyWithMockInitialCtx(t, cppWorker.URL, modelName, 8192)
	p.nctxReload.SetLastKnownNCtx("test-backend", 8192)

	plan := &ReloadPlan{Decision: DecisionReload, NewNCtx: 65536, Reason: "prompt_too_long"}
	bridgeErr := &NCtxBridgeError{CurrentNCtx: 8192, RequiredNCtx: 30000, MaxVRAMNCtx: 66125}
	rec := httptest.NewRecorder()

	p.handleNCtxReloadActualAsync("test-backend", modelName, plan, cppWorker.URL, bridgeErr, rec)

	if rec.Code != 503 {
		t.Fatalf("ожидался 503, получено %d", rec.Code)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && reloadCalls.len() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if reloadCalls.len() == 0 {
		t.Fatal("reload должен был запуститься (known n_ctx=8192 < target=65536), но cppworker не получил запрос")
	}
}

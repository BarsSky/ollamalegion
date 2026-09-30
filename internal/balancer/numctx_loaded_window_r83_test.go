//go:build llama_stub

// numctx_loaded_window_r83_test.go — R83-fix (2026-09-30).
//
// ЖИВАЯ ЖАЛОБА (Cline + Qwen3.8-27B): «контекст уже был сохранён и помещается в
// окно, но в ответ приходит preflight prompt + n_predict exceeds n_ctx for this
// backend»; «подготовленная модель с окном 131k была выгружена в пользу меньшего
// окна от клиента».
//
// ПРИЧИНА (воспроизведено на стенде, см. логи ниже). У модели три разных числа:
//
//	запрошено при загрузке    32768
//	cppworker сообщал         31841   (свой AutoTune/feasibility)
//	llama.cpp реально имел    32256   (n_ctx_seq=16128 × 2 слота, выравнивание)
//
// Балансер считал потолком profile.contextLengthMax (32768) и отправлял
// n_ctx_override=32768 — БОЛЬШЕ, чем у модели есть. cppworker честно отвечал
//
//	400 {"code":2,"message":"requested n_ctx=32768 exceeds model's effective n_ctx=31841"}
//
// после чего балансер запускал auto-reload, упирался в операторский потолок
// auto_reload_max_n_ctx=16384 и отдавал клиенту 413 — при том что клиент просил
// всего 8192, и он в модель помещается.
//
// Тесты фиксируют инвариант: для ЗАГРУЖЕННОЙ модели балансер не просит больше её
// окна, а операторский потолок не отказывает в том, что уже помещается.
package balancer

import (
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// TestR83_ResolveNumCtx_NeverAboveLoadedWindow — клиент просит больше, чем у
// модели есть → получает ровно окно модели (не profile.contextLengthMax).
func TestR83_ResolveNumCtx_NeverAboveLoadedWindow(t *testing.T) {
	const backendID = "b-loaded"
	const model = "Qwen3-Instruct-2507-q4km"

	p := &Proxy{
		config: &types.LoadBalancerConfig{
			LlamaCppModelProfiles: map[string]types.LlamaCppModelProfile{
				// Потолок профиля ВЫШЕ фактического окна модели — именно эта
				// комбинация и порождала n_ctx_override=32768 при окне 31841.
				model: {ContextLength: 16384, ContextLengthMax: 32768},
			},
		},
		backends:   map[string]*BackendState{},
		metricsMgr: NewMetricsManager(),
	}
	p.backends[backendID] = &BackendState{Backend: &types.Backend{ID: backendID, Type: types.BackendTypeLlamaCpp}}
	p.nctxReload = NewNCtxReloadCoordinator(DefaultNCtxReloadConfig())
	// Фактическое окно загруженной модели.
	p.cacheLoadedContextLength(backendID, model, 31841)

	body := []byte(`{"model":"` + model + `","options":{"num_ctx":32768}}`)
	got := p.ResolveNumCtx(model, body, backendID)

	if got.Value > 31841 {
		t.Errorf("ResolveNumCtx вернул %d при окне модели 31841 — cppworker ответит "+
			"code=2 «exceeds model's effective n_ctx» и запрос уйдёт в reload/413", got.Value)
	}
	if got.Value != 31841 {
		t.Errorf("ResolveNumCtx = %d, want ровно окно модели 31841", got.Value)
	}
}

// TestR83_ResolveNumCtx_SmallClientWindowUpgradedToLoaded — клиент прислал
// меньше загруженного: отдаём окно модели (это делает cppworker счастливым и не
// запускает никаких reload).
func TestR83_ResolveNumCtx_SmallClientWindowUpgradedToLoaded(t *testing.T) {
	const backendID = "b-loaded-2"
	const model = "m"

	p := &Proxy{
		config:     &types.LoadBalancerConfig{},
		backends:   map[string]*BackendState{},
		metricsMgr: NewMetricsManager(),
	}
	p.backends[backendID] = &BackendState{Backend: &types.Backend{ID: backendID, Type: types.BackendTypeLlamaCpp}}
	p.nctxReload = NewNCtxReloadCoordinator(DefaultNCtxReloadConfig())
	p.cacheLoadedContextLength(backendID, model, 31841)

	body := []byte(`{"model":"` + model + `","options":{"num_ctx":8192}}`)
	got := p.ResolveNumCtx(model, body, backendID)

	if got.Value != 31841 {
		t.Errorf("ResolveNumCtx = %d, want 31841 (окно загруженной модели, "+
			"клиентское 8192 меньше и не должно приводить к меньшему окну)", got.Value)
	}
}

// TestR83_RejectPlan_NotUsedWhenRequiredWithinLoadedWindow — страховка на уровне
// решения о reload: если required ≤ текущего окна, потолок auto-reload молчит.
func TestR83_RejectPlan_NotUsedWhenRequiredWithinLoadedWindow(t *testing.T) {
	c := NewNCtxReloadCoordinator(NCtxReloadConfig{
		AutoReloadNCtx:    true,
		AutoReloadMaxNCtx: 16384, // потолок НИЖЕ окна модели — конфигурационная ловушка
	})
	bridgeErr := &NCtxBridgeError{
		Code:         NCtxErrCodeNCtxNeedsReload,
		CurrentNCtx:  31841,
		RequiredNCtx: 20000, // помещается в загруженное окно
	}
	plan := c.DecideReloadBackend("b1", bridgeErr, 20000)
	if plan.Decision != DecisionNoOp {
		t.Errorf("решение = %v при required=20000 и окне 31841; ожидался NoOp "+
			"(перезагружать нечего, отказывать нельзя)", plan.Decision)
	}
	if plan.RejectMsg != "" {
		t.Errorf("клиенту уйдёт отказ: %s", plan.RejectMsg)
	}
}

// TestR83_RejectPlan_StillRejectsAboveCap — обратная сторона: запрос, который
// ДЕЙСТВИТЕЛЬНО требует больше потолка, по-прежнему отклоняется (иначе клиент
// сможет раздувать контекст).
func TestR83_RejectPlan_StillRejectsAboveCap(t *testing.T) {
	c := NewNCtxReloadCoordinator(NCtxReloadConfig{
		AutoReloadNCtx:    true,
		AutoReloadMaxNCtx: 16384,
	})
	bridgeErr := &NCtxBridgeError{
		Code:         NCtxErrCodeNCtxNeedsReload,
		CurrentNCtx:  8192,
		RequiredNCtx: 32768,
	}
	plan := c.DecideReloadBackend("b1", bridgeErr, 32768)
	if plan.Decision != DecisionReject {
		t.Errorf("решение = %v, want DecisionReject (required 32768 > потолка 16384)", plan.Decision)
	}
	if plan.RejectMsg == "" {
		t.Error("в отказе нет сообщения для клиента")
	}
}

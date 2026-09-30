// growth_gate_r83_test.go — R83-fix (2026-09-30): preflight не перезагружает
// модель, если рост окна недостижим или достигается ценой потери GPU-слоёв.
//
// Живой дефект локального стенда (gemma-4, 65536 суммарно / 2 слота, RTX 3070
// 8 GB, запрос Cline с prompt ≈16k токенов + max_tokens=8192):
//
//  1. preflight сравнивал ОЦЕНКУ промпта (44 230) с окном слота (32 768) и решал
//     растить окно;
//  2. адаптивная стратегия, спрошенная до выгрузки, считала бюджет по свободной
//     VRAM (то есть без памяти самой перезагружаемой модели) и возвращала
//     stage=cpu_only, gpuLayers=0, nCtx=65536;
//  3. модель выгружалась и грузилась заново с тем же окном 65536 — 2–3 минуты
//     простоя, 503/ожидание у клиента и ни одного выигрыша.
package balancer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// strategyBackend отдаёт фиксированный JSON адаптивной стратегии и считает
// вызовы /api/models/reload.
func strategyBackend(t *testing.T, strategyJSON string) (*httptest.Server, *int32) {
	t.Helper()
	var reloads int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/cppworker/adaptive/strategy":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(strategyJSON))
		case r.URL.Path == "/api/models/reload":
			atomic.AddInt32(&reloads, 1)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &reloads
}

func gateCoordinator(currentGPULayers int) *NCtxReloadCoordinator {
	coord := NewNCtxReloadCoordinator(DefaultNCtxReloadConfig())
	coord.SetConfig(NCtxReloadConfig{
		AutoReloadNCtx:              true,
		AutoReloadVRAMSafetyFactor:  0.85,
		PreflightEnabled:            true,
		PreflightAsyncReload:        true,
		PreflightAsyncRetryAfterSec: 1,
	})
	if currentGPULayers > 0 {
		coord.SetReloadHintsProvider(func(backendID, modelName string) ReloadHints {
			return ReloadHints{KVCacheType: "q4_0", GPULayers: currentGPULayers}
		})
	}
	return coord
}

// TestR83Fix_GrowthAllowed_WhenStrategySaysCPUOnlyButWindowGrows — cpu_only сам
// по себе НЕ повод отказывать в росте окна.
//
// Живая проверка 2026-09-30 (cppworker gpu-r83-submodule-v36): при стратегии
// stage=cpu_only/gpuLayers=0 reload всё равно дал `new_gpu_layers:-1` →
// `gpu_layers=22`, `offloaded 22/43 layers` — потому что `gpuLayers: 0` в
// reload-запросе cppworker трактует как «взять из профиля» (sync_profile.go),
// а не как CPU-only. Отклонение такого reload'а лишило бы клиента нужного
// контекста (8192 → 65536), не защитив ни от чего.
func TestR83Fix_GrowthAllowed_WhenStrategySaysCPUOnlyButWindowGrows(t *testing.T) {
	srv, _ := strategyBackend(t, `{"gpuLayers":0,"nCtx":65536,"kvCacheType":"q4_0",`+
		`"stage":"cpu_only","gpuReduced":true,"nCtxReduced":true,"maxViableNCtx":659129,`+
		`"explanation":"cpu_only: kvType=q4_0, n_ctx=65536/131072"}`)

	coord := gateCoordinator(20)
	worth, why := coord.growthWorthReload(srv.URL, "b1", "gemma-4-E4B-it-Q4_K_M",
		65536 /* target: растём с 8192 */, 8192 /* current total */, 4096, 20)

	if !worth {
		t.Fatalf("рост окна 8192→65536 отклонён из-за cpu_only в стратегии: почему=%q — "+
			"cppworker трактует gpuLayers=0 как «из профиля», раскладка не пострадает", why)
	}
}

// TestR83Fix_GrowthSkipped_WhenWindowWouldNotGrow — главный случай живого
// дефекта: стратегия даёт то же окно, что уже загружено (131072 запрошено,
// 65536 достижимо при текущих 65536), reload бессмыслен.
func TestR83Fix_GrowthSkipped_WhenWindowWouldNotGrow(t *testing.T) {
	srv, reloads := strategyBackend(t, `{"gpuLayers":0,"nCtx":65536,"kvCacheType":"q4_0",`+
		`"stage":"cpu_only","gpuReduced":true,"nCtxReduced":true,"maxViableNCtx":659129,`+
		`"explanation":"cpu_only: kvType=q4_0, n_ctx=65536/131072"}`)

	coord := gateCoordinator(19)
	worth, why := coord.growthWorthReload(srv.URL, "b1", "gemma-4-E4B-it-Q4_K_M",
		131072 /* target total */, 65536 /* current total */, 32768 /* per-slot */, 19)

	if worth {
		t.Fatalf("рост окна признан осмысленным, хотя стратегия даёт то же суммарное окно "+
			"65536 при текущем 65536: почему=%q", why)
	}
	if why == "" {
		t.Error("причина пропуска пустая — оператор не поймёт, почему reload не сделан")
	}
	if *reloads != 0 {
		t.Errorf("/api/models/reload вызван %d раз — reload не должен запускаться", *reloads)
	}
}

// TestR83Fix_GrowthAllowed_WhenStrategyGrowsAndKeepsGPU — положительный случай:
// стратегия даёт больше окна и не меньше слоёв → reload осмыслен.
func TestR83Fix_GrowthAllowed_WhenStrategyGrowsAndKeepsGPU(t *testing.T) {
	srv, _ := strategyBackend(t, `{"gpuLayers":24,"nCtx":131072,"kvCacheType":"q4_0",`+
		`"stage":"partial_offload","maxViableNCtx":131072,"explanation":"fits"}`)

	coord := gateCoordinator(19)
	worth, why := coord.growthWorthReload(srv.URL, "b1", "gemma-4-E4B-it-Q4_K_M",
		131072, 65536, 32768, 19)

	if !worth {
		t.Fatalf("рост окна отклонён, хотя стратегия даёт 131072 суммарно и 24 слоя "+
			"против 19: почему=%q", why)
	}
}

// TestR83Fix_GrowthWorthReload_NoStrategyMeansOldBehaviour — если стратегии нет
// (cppworker не ответил/404), поведение прежнее: reload разрешён.
func TestR83Fix_GrowthWorthReload_NoStrategyMeansOldBehaviour(t *testing.T) {
	srv, _ := strategyBackend(t, `not json at all`)
	coord := gateCoordinator(19)
	if worth, why := coord.growthWorthReload(srv.URL, "b1", "m", 131072, 65536, 32768, 19); !worth {
		t.Fatalf("без стратегии reload должен оставаться разрешённым, отклонён: %q", why)
	}
}

// TestR83Fix_RunPreflight_ReturnsNoOpAndSkipsReload — сквозной контракт: запрос
// не должен получать 503/ожидание, если рост окна ничего не даёт.
func TestR83Fix_RunPreflight_ReturnsNoOpAndSkipsReload(t *testing.T) {
	t.Setenv("LB_NCTX_PREFLIGHT_WAIT_SEC", "0")

	srv, reloads := strategyBackend(t, `{"gpuLayers":0,"nCtx":65536,"kvCacheType":"q4_0",`+
		`"stage":"cpu_only","gpuReduced":true,"nCtxReduced":true,"maxViableNCtx":659129,`+
		`"explanation":"cpu_only: kvType=q4_0, n_ctx=65536/131072"}`)

	coord := gateCoordinator(19)
	meta := &RequestMeta{
		EstimatedPromptTokens: 44230, // ровно то, что посчитал preflight на стенде
		RequestedNPredict:     8192,
		ModelName:             "gemma-4-E4B-it-Q4_K_M",
		HasTools:              true,
	}
	state := &NCtxBackendState{
		BackendID:            "b1",
		CurrentNCtx:          65536,
		CurrentContextPerSeq: 32768,
		CurrentSlots:         2,
		MaxVRAMNCtx:          0,
		VRAMKnown:            true,
		ModelMaxContext:      131072,
		PhysicalMaxContext:   131072,
		AvailableVRAMMB:      4709,
	}

	loader := &DefaultNCtxReloadHTTPClient{
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
		APIToken:   "test",
		HeaderName: "X-API-Token",
	}

	res, err := coord.RunPreflight(context.Background(), "b1", srv.URL, meta, state, loader)
	if err != nil {
		t.Fatalf("RunPreflight error: %v", err)
	}
	if res.Decision != PreflightNoOp {
		t.Fatalf("decision = %v, ожидался PreflightNoOp (запрос обслуживается в текущем "+
			"окне, а не переводится в reload/503)", res.Decision)
	}
	time.Sleep(50 * time.Millisecond)
	if n := atomic.LoadInt32(reloads); n != 0 {
		t.Errorf("reload запущен %d раз, хотя роста окна не будет", n)
	}
}

// TestR83Fix_ClampStrategyToCurrentLayout — страховка в DoReload: даже если
// стратегия пришла «cpu_only», cppworker получит не меньше слоёв, чем работает.
func TestR83Fix_ClampStrategyToCurrentLayout(t *testing.T) {
	cases := []struct {
		name        string
		layersIn    int
		current     int
		wantLayers  int
		wantStage   string
		wantClamped bool
	}{
		{"cpu_only не понижает GPU", 0, 19, 19, "gpu_layers_kept", true},
		{"меньше слоёв → поднимаем до текущего", 12, 19, 19, "partial_offload", true},
		{"больше слоёв — не трогаем", 24, 19, 24, "partial_offload", false},
		{"равно — не трогаем", 19, 19, 19, "partial_offload", false},
		{"-1 (все слои) — не трогаем", -1, 19, -1, "exact_fit", false},
		{"текущие слои неизвестны — не трогаем", 0, 0, 0, "cpu_only", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := &AdaptiveStrategy{GPULayers: tc.layersIn, Stage: tc.wantStage, GPUReduced: true}
			if tc.wantStage == "gpu_layers_kept" {
				st.Stage = "cpu_only"
			}
			got := clampStrategyToCurrentLayout(st, tc.current)
			if got != tc.wantClamped {
				t.Fatalf("clamped = %v, ожидалось %v", got, tc.wantClamped)
			}
			if st.GPULayers != tc.wantLayers {
				t.Errorf("gpuLayers = %d, ожидалось %d", st.GPULayers, tc.wantLayers)
			}
			if tc.wantClamped && st.GPUReduced {
				t.Error("gpuReduced остался true — load-failure событие скажет «без GPU»")
			}
		})
	}
}

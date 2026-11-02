// primary_fixed_window_r398_test.go — R398.1 (2026-11-04).
//
// ЖИВАЯ ЖАЛОБА ОПЕРАТОРА: «стоит блок в 32768, настройки из WebUI во вкладках
// gguf models игнорируются».
//
// ПРИЧИНА. У модели в профиле primary=true (настройки зафиксированы
// администратором) — это политика, и клиентский num_ctx действительно не должен
// её менять. Но «зафиксированное окно» бралось из nctxReload.LastKnownNCtx(BACKEND),
// а это значение ЖИВЁТ НА БЭКЕНД, не на модель: после перезагрузки любой другой
// модели там оставалось 32768, и primary-модель с заявленными 131072
// «фиксировалась» на 32768 — клиенту молча подменяли num_ctx вниз.
package balancer

import (
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// TestR398_PrimaryFixedWindowIsPerModel — чужое окно с того же бэкенда не
// становится «зафиксированным» для primary-модели.
func TestR398_PrimaryFixedWindowIsPerModel(t *testing.T) {
	cfg := &types.LoadBalancerConfig{
		LlamaCppModelProfiles: map[string]types.LlamaCppModelProfile{
			"Qwen3.8-27B": {ContextLength: 131072, Primary: true},
		},
	}
	p := NewProxy(cfg)

	const backend = "cppworker-gpu-bundled-agent"
	// На бэкенде загружена ДРУГАЯ модель с окном 32768 (как gemma-4 в живом стенде).
	p.metricsMgr.mu.Lock()
	p.metricsMgr.llamaMetrics[backend] = &types.LlamaCppMetrics{
		LoadedModels: []types.LlamaCppModel{{Name: "gemma-4-E4B-it-Q4_K_M", ContextLength: 32768}},
	}
	p.metricsMgr.mu.Unlock()

	if got := p.primaryFixedNCtx(backend, "Qwen3.8-27B-UD-Q4_K_M"); got != 131072 {
		t.Fatalf("зафиксированное окно = %d, want 131072 (профиль primary-модели): "+
			"окно чужой модели с того же бэкенда не должно «фиксировать» эту", got)
	}

	// Своя модель в метриках — метрики (факт) важнее профиля.
	p.metricsMgr.mu.Lock()
	p.metricsMgr.llamaMetrics[backend] = &types.LlamaCppMetrics{
		LoadedModels: []types.LlamaCppModel{
			{Name: "gemma-4-E4B-it-Q4_K_M", ContextLength: 32768},
			{Name: "Qwen3.8-27B-UD-Q4_K_M", ContextLength: 65536},
		},
	}
	p.metricsMgr.mu.Unlock()
	if got := p.primaryFixedNCtx(backend, "Qwen3.8-27B-UD-Q4_K_M"); got != 65536 {
		t.Fatalf("зафиксированное окно = %d, want 65536 (фактически загруженное у этой модели)", got)
	}
}

// TestR398_PrimaryFixedWindowNoBackendLeak — загрузка другой модели на бэкенде
// не меняет фиксированное окно primary-модели, даже если метрик её нет.
func TestR398_PrimaryFixedWindowNoBackendLeak(t *testing.T) {
	cfg := &types.LoadBalancerConfig{
		LlamaCppModelProfiles: map[string]types.LlamaCppModelProfile{
			"big": {ContextLength: 131072, Primary: true},
		},
	}
	p := NewProxy(cfg)

	// Координатор reload'а помнит на бэкенде 32768 (от перезагрузки другой модели).
	p.nctxReload.SetLastKnownNCtx("b1", 32768)

	if got := p.primaryFixedNCtx("b1", "big"); got != 131072 {
		t.Fatalf("зафиксированное окно = %d, want 131072: LastKnownNCtx — величина НА БЭКЕНД, "+
			"для primary-модели брать её нельзя (это и давало блок 32768)", got)
	}
}

package balancer

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"ollama-loadbalancer/pkg/types"
)

// R545 (F9, жалоба №9 плана 2026-11-19-webui-and-balancer-fixes).
//
// ЖИВОЙ ДЕФЕКТ (две половины). Клиент просит окно 200k, балансер молча отдаёт
// 131k — и делает это на бэкенде, где модели ЕЩЁ НЕТ, то есть при ПЕРВИЧНОЙ
// загрузке. Мест было два, и оба срабатывали раньше, чем балансер вообще
// выяснял состояние модели:
//
//  1. `ResolveNumCtx` клампил клиентское окно потолком профиля
//     (`num_ctx_resolver.go`, «clamping request num_ctx to profile max») без
//     проверки «загружена ли модель». Значение уезжало в X-Cpp-Ctx, и
//     авто-загрузка поднимала модель ровно на этом понижённом числе.
//  2. `preflightNCtxReloadIfNeeded` отвечал 413 «выше потолка авто-reload» ДО
//     resolveLoadedWindowNCtx (`nctx_reload_handlers.go`), то есть и для пустого
//     узла.
//
// Контракт самого балансера говорит обратное: профиль — HINT, а не потолок
// (docs/architecture/context-numbers.ru.md, R68), а потолок авто-роста НЕ
// ограничивает загрузку модели — он про АВТО-reload уже работающей (R83, R255).
//
// После правки: для НЕзагруженной модели окно не понижается, а физику проверяют
// воркер (GGUF / max_vram_n_ctx) и гейт окна R522 — там отказ приходит С ЧИСЛАМИ.
// Для загруженной модели потолок остаётся (это его настоящая работа).

const modelR545 = "gemma-4-E4B-it-Q4_K_M"

// metricsWithModelR545 — метрики бэкенда с ОДНОЙ моделью в заданном состоянии.
// loaded=false + state "error" — «опрос прошёл, модели на узле нет».
func metricsWithModelR545(m types.LlamaCppMetrics) *types.LlamaCppMetrics {
	return &m
}

// TestR545_ColdLoadKeepsClientWindow — модель НЕ загружена: клиентское окно не
// понижается потолком профиля (было 131072 вместо 200000).
func TestR545_ColdLoadKeepsClientWindow(t *testing.T) {
	cfg := createTestConfig()
	cfg.LlamaCppModelProfiles = map[string]types.LlamaCppModelProfile{
		modelR545: {ContextLength: 32768, ContextLengthMax: 131072},
	}
	proxy := newProxyWithCleanup(t, cfg)

	body := []byte(`{"model":"` + modelR545 + `","messages":[{"role":"user","content":"ок"}],"options":{"num_ctx":200000}}`)
	got := proxy.ResolveNumCtx(modelR545, body, "backend-1")

	assert.Equal(t, 200000, got.Value,
		"R545: для НЕзагруженной модели окно клиента должно дойти до загрузки как есть "+
			"(профиль — хинт, физику проверяет воркер и гейт R522), получили %d (%s)", got.Value, got.Source)
	assert.Equal(t, NumCtxSourceRequest, got.Source)
}

// TestR545_LoadedModelStillCapped — модель загружена: админ-потолок профиля попрежнему
// действует (это и есть его работа — не давать клиенту поднять окно выше политики).
func TestR545_LoadedModelStillCapped(t *testing.T) {
	cfg := createTestConfig()
	cfg.LlamaCppModelProfiles = map[string]types.LlamaCppModelProfile{
		modelR545: {ContextLength: 32768, ContextLengthMax: 131072},
	}
	proxy := newProxyWithCleanup(t, cfg)

	proxy.metricsMgr.mu.Lock()
	proxy.metricsMgr.llamaMetrics["backend-1"] = metricsWithModelR545(types.LlamaCppMetrics{
		LoadedModels: []types.LlamaCppModel{{
			Name:          modelR545,
			ContextLength: 262144,
			ContextPerSeq: 262144,
			State:         "loaded",
		}},
	})
	proxy.metricsMgr.mu.Unlock()

	body := []byte(`{"model":"` + modelR545 + `","messages":[{"role":"user","content":"ок"}],"options":{"num_ctx":200000}}`)
	got := proxy.ResolveNumCtx(modelR545, body, "backend-1")

	assert.Equal(t, 131072, got.Value,
		"R545: для ЗАГРУЖЕННОЙ модели потолок профиля обязан остаться, получили %d (%s)", got.Value, got.Source)
}

// TestR545_AbsentModelNotRefusedByCeiling — модели на узле НЕТ: 413 по потолку
// авто-reload не выдаётся (было 413 «окно выше потолка» на пустом узле).
func TestR545_AbsentModelNotRefusedByCeiling(t *testing.T) {
	cfg := createTestConfig()
	proxy := newProxyWithCleanup(t, cfg)

	rcfg := proxy.nctxReload.Config()
	rcfg.AutoReloadMaxNCtx = 131072
	proxy.nctxReload.SetConfig(rcfg)

	// Опрос прошёл, модель есть в списке, но без окна и в состоянии error —
	// для балансера это «модели на узле нет, загрузка нужна».
	proxy.metricsMgr.mu.Lock()
	proxy.metricsMgr.llamaMetrics["backend-1"] = metricsWithModelR545(types.LlamaCppMetrics{
		LoadedModels: []types.LlamaCppModel{{Name: modelR545, State: "error"}},
	})
	proxy.metricsMgr.mu.Unlock()

	body := []byte(`{"model":"` + modelR545 + `","messages":[{"role":"user","content":"ок"}],"options":{"num_ctx":200000}}`)
	_, _, msg, status := proxy.preflightNCtxReloadIfNeeded(
		context.Background(), "backend-1", modelR545, body, "/api/chat")

	assert.NotEqual(t, http.StatusRequestEntityTooLarge, status,
		"R545: 413 по потолку нельзя выдавать, когда модели на узле ещё нет: %s", msg)
	assert.NotContains(t, msg, "auto_reload_max_n_ctx",
		"R545: на пустом узле потолок авто-reload не должен упоминаться как причина отказа")
}

// TestR545_GGUFByModelName — GGUF-потолок берётся по ИМЕНИ модели, а не как
// максимум по загруженным моделям узла.
func TestR545_GGUFByModelName(t *testing.T) {
	proxy := newProxyWithCleanup(t, createTestConfig())

	proxy.metricsMgr.mu.Lock()
	proxy.metricsMgr.llamaMetrics["backend-1"] = &types.LlamaCppMetrics{
		// Top-level — максимум по узлу (так его считает поллер). Для «small»
		// правильный ответ 32768, не 262144.
		GGUFMaxContext: 262144,
		LoadedModels: []types.LlamaCppModel{
			{Name: "big-27b", GGUFMaxContext: 262144},
			{Name: "small-4b", GGUFMaxContext: 32768},
		},
	}
	proxy.metricsMgr.mu.Unlock()

	assert.Equal(t, 262144, proxy.ggufMaxContextForModel("backend-1", "big-27b"),
		"GGUF большой модели")
	assert.Equal(t, 32768, proxy.ggufMaxContextForModel("backend-1", "small-4b"),
		"R545: GGUF должен быть ИМЕННО этой модели, а не максимумом по узлу")
	assert.Equal(t, 0, proxy.ggufMaxContextForModel("backend-1", "нет-такой"),
		"R545: для модели, которой нет среди загруженных, честно «неизвестно» (0), "+
			"а не число соседа")

	// Старый воркер: per-model данных нет вовсе — прежний top-level как fallback.
	proxy.metricsMgr.mu.Lock()
	proxy.metricsMgr.llamaMetrics["backend-2"] = &types.LlamaCppMetrics{GGUFMaxContext: 131072}
	proxy.metricsMgr.mu.Unlock()
	assert.Equal(t, 131072, proxy.ggufMaxContextForModel("backend-2", modelR545),
		"без per-model данных падаем на top-level, как раньше (совместимость)")
}

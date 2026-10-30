// nctx_window_cache_r327_test.go — R327 (2026-10-30).
//
// ЖИВАЯ ЖАЛОБА ОПЕРАТОРА: «новый клиент, новая сессия, запрос малый, окно
// загруженной модели 131072 — а места не хватило; впечатление, что от старой
// сессии не очищен кэш».
//
// ПРИЧИНА. После перезагрузки модели кэш метрик получал только СУММАРНОЕ окно
// (ContextLength). ContextPerSeq и MaxSlots оставались от прошлой загрузки, а
// резолвер окна читает ПЕРВЫМ именно ContextPerSeq — то есть клиент до
// следующего тика поллера работал по устаревшему окну слота.
//
// Здесь проверяется подтверждённая запись (R327): три поля разом, и
// несогласованные комбинации старого кэша не остаются.
package balancer

import (
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// proxyWithWindowCacheR327 — Proxy с кэшем метрик и одной записью модели.
func proxyWithWindowCacheR327(slots int) (*Proxy, string, string) {
	const backendID = "cpp-worker-window"
	const model = "gemma-4-E4B-it-Q4_K_M"
	p := &Proxy{metricsMgr: NewMetricsManager()}
	p.metricsMgr.llamaMetrics[backendID] = &types.LlamaCppMetrics{
		LoadedModels: []types.LlamaCppModel{
			// Прошлая загрузка: 16384 на слот при 2 слотах.
			{Name: model, State: "loaded", ContextLength: 32768, ContextPerSeq: 16384, MaxSlots: slots},
		},
	}
	return p, backendID, model
}

// TestR327_ConfirmedWindowWritesAllThreeFields — 131072 на слот × 2 слота.
func TestR327_ConfirmedWindowWritesAllThreeFields(t *testing.T) {
	p, backendID, model := proxyWithWindowCacheR327(2)

	p.cacheLoadedWindowActualR327(backendID, model, 131072)

	lm := p.metricsMgr.llamaMetrics[backendID].LoadedModels[0]
	if lm.ContextPerSeq != 131072 {
		t.Errorf("ContextPerSeq = %d, want 131072 (окно слота — его резолвер читает первым)", lm.ContextPerSeq)
	}
	if lm.MaxSlots != 2 {
		t.Errorf("MaxSlots = %d, want 2 (слоты из кэша воркера не меняются)", lm.MaxSlots)
	}
	if lm.ContextLength != 262144 {
		t.Errorf("ContextLength = %d, want 262144 (окно слота × слоты)", lm.ContextLength)
	}
}

// TestR327_UnknownSlotsMeansOneClient — слотов нет в кэше: считаем 1, а не 0.
func TestR327_UnknownSlotsMeansOneClient(t *testing.T) {
	p, backendID, model := proxyWithWindowCacheR327(0)

	p.cacheLoadedWindowActualR327(backendID, model, 65536)

	lm := p.metricsMgr.llamaMetrics[backendID].LoadedModels[0]
	if lm.MaxSlots != 1 {
		t.Errorf("MaxSlots = %d, want 1 (неизвестно → окно целиком одному клиенту)", lm.MaxSlots)
	}
	if lm.ContextPerSeq != 65536 || lm.ContextLength != 65536 {
		t.Errorf("окно записано как perSeq=%d total=%d, want 65536/65536", lm.ContextPerSeq, lm.ContextLength)
	}
}

// TestR327_MissingModelRecordIsCreated — записи в кэше нет: создаём loaded-запись.
func TestR327_MissingModelRecordIsCreated(t *testing.T) {
	const backendID = "cpp-worker-empty"
	p := &Proxy{metricsMgr: NewMetricsManager()}

	p.cacheLoadedWindowActualR327(backendID, "new-model", 32768)

	lm := p.metricsMgr.llamaMetrics[backendID]
	if lm == nil || len(lm.LoadedModels) != 1 {
		t.Fatalf("запись модели не создана: %+v", lm)
	}
	got := lm.LoadedModels[0]
	if got.State != "loaded" || got.ContextPerSeq != 32768 || got.ContextLength != 32768 {
		t.Errorf("созданная запись: %+v, want loaded/32768/32768", got)
	}
}

// TestR327_InvalidInputDoesNotCorruptCache — нули и пустые имена игнорируются.
func TestR327_InvalidInputDoesNotCorruptCache(t *testing.T) {
	p, backendID, model := proxyWithWindowCacheR327(2)
	before := p.metricsMgr.llamaMetrics[backendID].LoadedModels[0]

	p.cacheLoadedWindowActualR327(backendID, model, 0)
	p.cacheLoadedWindowActualR327(backendID, "", 32768)

	after := p.metricsMgr.llamaMetrics[backendID].LoadedModels[0]
	if after.ContextPerSeq != before.ContextPerSeq || after.ContextLength != before.ContextLength {
		t.Errorf("некорректный вход изменил кэш: было %+v, стало %+v", before, after)
	}
}

// TestR327_PlannedWriteDoesNotClaimPerSlot — обратная сторона: «намерение»
// (cacheLoadedContextLength, async reload только запущен) окно слота НЕ трогает,
// иначе резолвер начнёт отдавать ещё не достигнутое окно (контракт R83).
func TestR327_PlannedWriteDoesNotClaimPerSlot(t *testing.T) {
	p, backendID, model := proxyWithWindowCacheR327(2)

	p.cacheLoadedContextLength(backendID, model, 65536)

	lm := p.metricsMgr.llamaMetrics[backendID].LoadedModels[0]
	if lm.ContextPerSeq != 16384 {
		t.Errorf("ContextPerSeq = %d, want 16384: планируемый reload не должен менять живое окно слота "+
			"(его читает резолвер, TestR83_StaleCache_ResolverUsesLiveWindow)", lm.ContextPerSeq)
	}
}

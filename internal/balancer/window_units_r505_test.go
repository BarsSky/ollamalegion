//go:build llama_stub

// window_units_r505_test.go — R505 (2026-11-19), аудит S4: N-6, N-7, N-8.
//
// Одна величина «окно контекста» жила в двух единицах:
//   - N-6: LastKnownNCtx писали то суммарным окном (ответ reload'а), то окном на
//     слот (preflight), а читатели (preflight, резолвер) трактуют его как окно
//     СЛОТА → после перезагрузки preflight видел вдвое большее окно;
//   - N-7: подтверждённое окно писалось только в ContextLength (СУММАРНОЕ),
//     ContextPerSeq оставался от прошлой загрузки → ложное «не влезает»;
//   - N-8: клиенту объявляли СУММАРНЫЙ потолок как X-Model-Max-Context, а его
//     `num_ctx` трактуем как окно на слот → клиент ставил наше же число и получал
//     413 или вторую перезагрузку.
package balancer

import (
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// TestR505_PerSeqFromTotal — N-6: обратный пересчёт округляет ВНИЗ (реальное окно
// слота — PAD(total/slots, 256), то есть не меньше частного).
func TestR505_PerSeqFromTotal(t *testing.T) {
	cases := []struct {
		total, slots, want int
	}{
		{131072, 2, 65536},
		{131072, 1, 131072},
		{131072, 0, 131072},
		{16385, 2, 8192}, // вниз, а не вверх: пересчёт не должен обещать лишнее
		{0, 4, 0},
	}
	for _, c := range cases {
		if got := perSeqFromTotal(c.total, c.slots); got != c.want {
			t.Errorf("perSeqFromTotal(%d, %d) = %d, ожидалось %d", c.total, c.slots, got, c.want)
		}
	}
}

// TestR505_LoadedNCtxFromMetricsIsPerSlot — N-6: fallback-читатель отдаёт окно
// СЛОТА, а не суммарное.
func TestR505_LoadedNCtxFromMetricsIsPerSlot(t *testing.T) {
	p := &Proxy{metricsMgr: NewMetricsManager()}
	p.metricsMgr.llamaMetrics["b1"] = &types.LlamaCppMetrics{
		LoadedModels: []types.LlamaCppModel{{
			Name:          "m",
			ContextLength: 131072, // суммарное
			MaxSlots:      2,
		}},
	}
	if got := p.getLoadedNCtxFromMetrics("b1", "m"); got != 65536 {
		t.Fatalf("окно из метрик = %d, ожидалось 65536 (131072 суммарно / 2 слота): "+
			"иначе preflight сравнит промпт с чужим окном (аудит N-6)", got)
	}

	// Опубликованное окно слота главнее деления.
	p.metricsMgr.llamaMetrics["b1"] = &types.LlamaCppMetrics{
		LoadedModels: []types.LlamaCppModel{{
			Name:          "m",
			ContextLength: 131072,
			ContextPerSeq: 66048,
			MaxSlots:      2,
		}},
	}
	if got := p.getLoadedNCtxFromMetrics("b1", "m"); got != 66048 {
		t.Fatalf("окно из метрик = %d, ожидалось 66048 (ContextPerSeq)", got)
	}
}

// TestR505_PreflightWritesConfirmedWindowToAllFields — N-7: подтверждённое окно
// пишется во все три поля согласованно (иначе ContextPerSeq остаётся старым и
// резолвер снова считает «не влезает»).
func TestR505_PreflightWritesConfirmedWindowToAllFields(t *testing.T) {
	p := &Proxy{metricsMgr: NewMetricsManager()}
	p.metricsMgr.llamaMetrics["b1"] = &types.LlamaCppMetrics{
		LoadedModels: []types.LlamaCppModel{{
			Name:          "m",
			ContextLength: 32768, // прошлая загрузка
			ContextPerSeq: 32768,
			MaxSlots:      2,
		}},
	}
	p.cacheLoadedWindowActualR327("b1", "m", 65536) // окно НА КЛИЕНТА

	got := p.metricsMgr.llamaMetrics["b1"].LoadedModels[0]
	if got.ContextPerSeq != 65536 {
		t.Fatalf("ContextPerSeq = %d, ожидалось 65536 (окно на слот)", got.ContextPerSeq)
	}
	if got.ContextLength != 131072 {
		t.Fatalf("ContextLength = %d, ожидалось 131072 (65536 × 2 слота)", got.ContextLength)
	}
}

// TestR505_PreflightHelperUsesWindowCacheR327 — N-7 (связка): preflight-путь
// обязан писать окно через cacheLoadedWindowActualR327, а не через старый
// cacheLoadedContextLength (тот писал окно клиента в СУММАРНОЕ поле).
func TestR505_PreflightHelperUsesWindowCacheR327(t *testing.T) {
	src, err := os.ReadFile("preflight_helper.go")
	if err != nil {
		t.Fatalf("чтение preflight_helper.go: %v", err)
	}
	if strings.Contains(string(src), "cacheLoadedContextLength(") {
		t.Fatal("preflight_helper.go по-прежнему пишет окно через cacheLoadedContextLength — " +
			"это кладёт окно КЛИЕНТА в СУММАРНОЕ ContextLength и оставляет старый " +
			"ContextPerSeq (аудит N-7)")
	}
	if !strings.Contains(string(src), "cacheLoadedWindowActualR327(") {
		t.Fatal("preflight_helper.go не пишет подтверждённое окно через " +
			"cacheLoadedWindowActualR327 (аудит N-7)")
	}
}

// TestR505_DoReloadWritesPerSlotCache — N-6 (связка): после перезагрузки в
// LastKnownNCtx обязано уходить окно НА СЛОТ, а не `ContextSize` из ответа.
//
// Проверка по исходнику: DoReload требует настоящего HTTP-загрузчика, а свойство
// здесь текстовое — «суммарное значение не пишется в поле без пересчёта».
func TestR505_DoReloadWritesPerSlotCache(t *testing.T) {
	// R634: координатор перезагрузки разложен на семь файлов — читаем весь слой
	// (см. nctx_coordinator_sources_r634_test.go), иначе сторож краснел бы на переносе.
	text := readNCtxCoordinatorSourcesR634(t)
	idx := strings.Index(text, "c.SetLastKnownNCtx(backendID,")
	if idx < 0 {
		t.Fatal("не найден вызов SetLastKnownNCtx в nctx_reload.go")
	}
	end := strings.IndexByte(text[idx:], ')')
	if end < 0 {
		t.Fatal("не найдена граница вызова SetLastKnownNCtx")
	}
	call := text[idx : idx+end+1]
	// Пересчёт мог быть на предыдущей строке (`perSeq := perSeqFromTotal(...)`) —
	// проверяем и аргумент вызова, и наличие пересчёта в ближайшем контексте.
	windowStart := idx - 600
	if windowStart < 0 {
		windowStart = 0
	}
	near := text[windowStart : idx+end+1]
	if !strings.Contains(near, "perSeqFromTotal(") {
		t.Fatalf("в LastKnownNCtx уходит суммарное окно без пересчёта: %q "+
			"(аудит N-6: читатели трактуют поле как окно НА СЛОТ)", call)
	}
}

// TestR505_AdvertisedWindowIsPerSlot — N-8: клиенту объявляется окно НА СЛОТ,
// суммарный потолок — отдельным заголовком.
func TestR505_AdvertisedWindowIsPerSlot(t *testing.T) {
	p := &Proxy{
		backends:   map[string]*BackendState{},
		metricsMgr: NewMetricsManager(),
		config:     &types.LoadBalancerConfig{},
	}
	p.metricsMgr.llamaMetrics["b1"] = &types.LlamaCppMetrics{
		LoadedModels: []types.LlamaCppModel{{
			Name:           "m",
			ContextLength:  131072,
			ContextPerSeq:  65536,
			MaxSlots:       2,
			GGUFMaxContext: 131072,
			MaxContext:     131072,
		}},
	}
	p.nctxReload = &NCtxReloadCoordinator{config: NCtxReloadConfig{AutoReloadMaxNCtx: 131072}}

	rec := httptest.NewRecorder()
	p.addModelCapabilitiesHeaders(rec, "m")
	if got := rec.Header().Get("X-Model-Max-Context"); got != "65536" {
		t.Fatalf("X-Model-Max-Context = %q, ожидалось 65536 (окно на слот): "+
			"клиент, поставивший наше же число, получил бы 413 (аудит N-8)", got)
	}
	if got := rec.Header().Get("X-Model-Max-Context-Total"); got != "131072" {
		t.Fatalf("X-Model-Max-Context-Total = %q, ожидалось 131072 (суммарный потолок)", got)
	}
}

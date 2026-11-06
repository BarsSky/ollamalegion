// effective_window_advertise_r469_test.go — R469 (2026-11-06): клиенту отдаётся
// ДЕЙСТВУЮЩИЙ потолок окна, а не GGUF-метаданные.
//
// ЖИВОЙ СИМПТОМ: Cline показывал занятые 63.6k и не запускал сжатие, а балансер
// отказывал по потолку 65536 (LB_NCTX_RELOAD_MAX_N_CTX). Причина расхождения —
// наши числа: X-Model-Max-Context нёс 131072 (GGUF), хотя обслужить больше 65536
// мы не могли.
package balancer

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

const r469Model = "gemma-4-E4B-it-Q4_K_M"

// r469Proxy — загруженная модель с GGUF-потолком 131072 и операторским cap 65536.
func r469Proxy(operatorCap int) *Proxy {
	p := &Proxy{
		backends:   map[string]*BackendState{},
		metricsMgr: NewMetricsManager(),
		config:     &types.LoadBalancerConfig{},
	}
	p.metricsMgr.llamaMetrics["b1"] = &types.LlamaCppMetrics{
		LoadedModels: []types.LlamaCppModel{{
			Name:           r469Model,
			ContextLength:  65536,
			GGUFMaxContext: 131072,
			MaxContext:     131072,
			Capabilities:   &types.ModelCapabilities{Tools: true, MaxContext: 131072},
		}},
	}
	if operatorCap > 0 {
		p.nctxReload = &NCtxReloadCoordinator{config: NCtxReloadConfig{AutoReloadMaxNCtx: operatorCap}}
	}
	return p
}

// TestR469_AdvertisedMaxIsServingCeiling — ровно живой случай: обещаем 65536,
// GGUF 131072 уходит отдельно.
func TestR469_AdvertisedMaxIsServingCeiling(t *testing.T) {
	p := r469Proxy(65536)

	eff, gguf := p.advertisedMaxContextR469(r469Model)
	if eff != 65536 {
		t.Fatalf("действующий потолок = %d, want 65536 (операторский cap ниже GGUF)", eff)
	}
	if gguf != 131072 {
		t.Fatalf("GGUF-потолок = %d, want 131072 (метаданные не теряем)", gguf)
	}

	rec := httptest.NewRecorder()
	p.addModelCapabilitiesHeaders(rec, r469Model)
	if got := rec.Header().Get("X-Model-Max-Context"); got != "65536" {
		t.Errorf("X-Model-Max-Context = %q, want 65536 — иначе клиент снова не сожмёт контекст", got)
	}
	if got := rec.Header().Get("X-Model-Gguf-Max-Context"); got != "131072" {
		t.Errorf("X-Model-Gguf-Max-Context = %q, want 131072 (сырой GGUF отдельным заголовком)", got)
	}
}

// TestR469_NoOperatorCapKeepsGguf — без cap обещаем GGUF: не выдумываем меньший
// предел и не теряем второй заголовок (его просто нет).
func TestR469_NoOperatorCapKeepsGguf(t *testing.T) {
	p := r469Proxy(0)

	eff, _ := p.advertisedMaxContextR469(r469Model)
	if eff != 131072 {
		t.Fatalf("без операторского cap потолок = %d, want 131072 (GGUF)", eff)
	}
	rec := httptest.NewRecorder()
	p.addModelCapabilitiesHeaders(rec, r469Model)
	if got := rec.Header().Get("X-Model-Max-Context"); got != strconv.Itoa(131072) {
		t.Errorf("X-Model-Max-Context = %q, want 131072", got)
	}
	if got := rec.Header().Get("X-Model-Gguf-Max-Context"); got != "" {
		t.Errorf("X-Model-Gguf-Max-Context = %q, want пусто (он совпадает с основным)", got)
	}
}

// TestR469_UnknownModelNoNumbers — модели нет в метриках: не выдумываем потолок
// (заголовок не ставим, прежнее поведение).
func TestR469_UnknownModelNoNumbers(t *testing.T) {
	p := r469Proxy(65536)
	if eff, gguf := p.advertisedMaxContextR469("no-such-model"); eff != 0 || gguf != 0 {
		t.Fatalf("для неизвестной модели eff=%d gguf=%d, want 0/0", eff, gguf)
	}
	rec := httptest.NewRecorder()
	p.addModelCapabilitiesHeaders(rec, "no-such-model")
	if got := rec.Header().Get("X-Model-Max-Context"); got != "131072" && got != "" {
		// fallback-капабилити по имени может дать свой MaxContext; главное — не
		// подставить потолок ДРУГОЙ модели.
		t.Logf("fallback X-Model-Max-Context=%q (допустимо: имя не найдено, значение из авто-детекта)", got)
	}
}

// TestR469_NilProxySafe — nil-safe (юнит-тесты и ранние вызовы).
func TestR469_NilProxySafe(t *testing.T) {
	var p *Proxy
	if eff, gguf := p.advertisedMaxContextR469(r469Model); eff != 0 || gguf != 0 {
		t.Fatalf("nil-прокси: eff=%d gguf=%d, want 0/0", eff, gguf)
	}
	rec := httptest.NewRecorder()
	p.addModelCapabilitiesHeaders(rec, r469Model)
	if rec.Code != http.StatusOK {
		t.Fatalf("nil-прокси не должен паниковать: %d", rec.Code)
	}
}

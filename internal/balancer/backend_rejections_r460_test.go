// backend_rejections_r460_test.go — R460 (2026-11-06): очередь обязана
// объясняться по каждому бэкенду.
//
// ЖИВАЯ ЖАЛОБА (A10 ×2, Qwen3.8-27B): «есть свободный бэкенд, а запрос встал в
// очередь». Прежний лог знал только «admission: request queued» — отличить
// «свободен, но выключен оператором» от «свободен, но модели нет» было нельзя.
package balancer

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// r460Proxy — пять бэкендов, каждый со своей причиной отказа, плюс один
// доступный (его в списке причин быть не должно).
func r460Proxy() *Proxy {
	p := &Proxy{
		backends: map[string]*BackendState{
			"ready":  {Backend: &types.Backend{ID: "ready", MaxConcurrentReqs: 4, Status: types.StatusHealthy, Type: types.BackendTypeLlamaCpp}},
			"busy":   {Backend: &types.Backend{ID: "busy", MaxConcurrentReqs: 1, Status: types.StatusHealthy, Type: types.BackendTypeLlamaCpp}},
			"cold":   {Backend: &types.Backend{ID: "cold", MaxConcurrentReqs: 4, Status: types.StatusHealthy, Type: types.BackendTypeLlamaCpp}},
			"off":    {Backend: &types.Backend{ID: "off", MaxConcurrentReqs: 4, Status: types.StatusOffline, Type: types.BackendTypeLlamaCpp}},
			"noslot": {Backend: &types.Backend{ID: "noslot", MaxConcurrentReqs: 0, Status: types.StatusHealthy, Type: types.BackendTypeLlamaCpp}},
		},
		metricsMgr: NewMetricsManager(),
		config:     &types.LoadBalancerConfig{},
	}
	p.backends["busy"].ActiveReqs = 1

	loaded := []types.LlamaCppModel{{Name: r460Model, ContextLength: 65536}}
	for _, id := range []string{"ready", "busy", "off", "noslot"} {
		p.metricsMgr.llamaMetrics[id] = &types.LlamaCppMetrics{LoadedModels: loaded}
		p.metricsMgr.metrics[id] = &types.BackendMetrics{
			ID:       id,
			LlamaCpp: types.LlamaCppMetrics{LoadedModels: loaded},
		}
	}
	// «cold»: бэкенд жив и здоров, метрики есть, а модели на нём нет — ровно
	// живой случай «свободный бэкенд без копии» (по R459 такую копию не берём,
	// но оператор должен видеть причину).
	p.metricsMgr.metrics["cold"] = &types.BackendMetrics{ID: "cold"}
	return p
}

const r460Model = "Qwen3.8-27B"

// TestR460_EveryBackendExplainsItself — у каждого неготового бэкенда есть
// причина, доступный в списке отсутствует.
func TestR460_EveryBackendExplainsItself(t *testing.T) {
	p := r460Proxy()
	list := p.backendRejectionsR460(r460Model, "")

	got := map[string]string{}
	for _, r := range list {
		got[r.BackendID] = r.Reason
		if r.Detail == "" {
			t.Errorf("%s: причина %s без пояснения — оператору нужны числа", r.BackendID, r.Reason)
		}
	}
	want := map[string]string{
		"busy":   rejectSlotsBusyR460,
		"cold":   rejectModelAbsentR460,
		"off":    rejectNotHealthyR460,
		"noslot": rejectNoSlotsConfigR460,
	}
	for id, wantReason := range want {
		if got[id] != wantReason {
			t.Errorf("%s: причина %q, want %q (все причины: %v)", id, got[id], wantReason, got)
		}
	}
	if _, ok := got["ready"]; ok {
		t.Errorf("доступный бэкенд попал в список отказов (%q) — это ложь оператору", got["ready"])
	}
}

// TestR460_SummaryHasNumbers — строка для лога содержит id, код и числа.
func TestR460_SummaryHasNumbers(t *testing.T) {
	p := r460Proxy()
	summary := rejectionsSummaryR460(p.backendRejectionsR460(r460Model, ""))
	for _, want := range []string{"busy=" + rejectSlotsBusyR460, "1/1", "cold=" + rejectModelAbsentR460} {
		if !strings.Contains(summary, want) {
			t.Errorf("в сводке нет %q: %s", want, summary)
		}
	}
}

// TestR460_UnavailableBodyCarriesRejections — 503 по таймауту объясняет, почему
// очередь не рассосалась (это и просил оператор).
func TestR460_UnavailableBodyCarriesRejections(t *testing.T) {
	p := r460Proxy()
	rec := httptest.NewRecorder()

	writeAdmissionUnavailable(rec, p, r460Model, "sess", 2, 5*time.Second, 5*time.Second)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("код %d, want 503", rec.Code)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("тело не JSON: %v (%s)", err, rec.Body.String())
	}
	raw, ok := body["backend_rejections"].([]interface{})
	if !ok || len(raw) == 0 {
		t.Fatalf("в теле 503 нет списка причин: %s", rec.Body.String())
	}
	first := raw[0].(map[string]interface{})
	if first["backend_id"] == "" || first["reason"] == "" {
		t.Errorf("элемент списка без id/reason: %v", first)
	}
	// Без proxy поле отсутствует, а тело остаётся валидным (юнит-тесты и
	// вызывающие без Proxy не должны ломаться).
	rec2 := httptest.NewRecorder()
	writeAdmissionUnavailable(rec2, nil, r460Model, "sess", 2, 5*time.Second, 5*time.Second)
	if strings.Contains(rec2.Body.String(), "backend_rejections") {
		t.Errorf("без proxy список причин появился: %s", rec2.Body.String())
	}
}

// TestR479_RejectionsKnowLoadThreshold — «свободный» бэкенд при загрузке ≥ 80%
// получает причину over_load_threshold: селектор его не берёт, и оператор должен
// видеть why_queued, а не пустой список.
func TestR479_RejectionsKnowLoadThreshold(t *testing.T) {
	p := r460Proxy()
	loaded := []types.LlamaCppModel{{Name: r460Model, ContextLength: 65536}}
	p.backends["loaded"] = &BackendState{Backend: &types.Backend{
		ID: "loaded", MaxConcurrentReqs: 10, Status: types.StatusHealthy, Type: types.BackendTypeLlamaCpp,
	}}
	p.backends["loaded"].ActiveReqs = 9 // 90% при пороге 80%: слот есть, но не берём
	p.metricsMgr.llamaMetrics["loaded"] = &types.LlamaCppMetrics{LoadedModels: loaded}
	p.metricsMgr.metrics["loaded"] = &types.BackendMetrics{
		ID: "loaded", LlamaCpp: types.LlamaCppMetrics{LoadedModels: loaded},
	}

	list := p.backendRejectionsR460(r460Model, "")
	for _, r := range list {
		if r.BackendID != "loaded" {
			continue
		}
		if r.Reason != rejectOverLoadThresholdR460 {
			t.Fatalf("причина %q, ожидалась %q (загрузка 9/10)", r.Reason, rejectOverLoadThresholdR460)
		}
		if !strings.Contains(r.Detail, "90%") && !strings.Contains(r.Detail, "загрузка") {
			t.Fatalf("в пояснении нет числа загрузки: %q", r.Detail)
		}
		return
	}
	t.Fatalf("бэкенд с загрузкой 90%% не попал в список причин: %+v", list)
}

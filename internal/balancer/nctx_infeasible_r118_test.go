//go:build llama_stub

// nctx_infeasible_r118_test.go — R118.7 (2026-10-14).
//
// ЖИВОЙ ДЕФЕКТ (лог стенда, 23:04-23:06): копия модели загружена с окном 8192
// (4096 на слот), клиент просит 8192 на слот — preflight планирует перезагрузку на
// суммарные 16384, cppworker отвечает отказом:
//
//	{"code":"n_ctx_infeasible","error":"requested n_ctx=16384 exceeds the physically
//	  feasible maximum ...","stage":"does_not_fit","max_vram_n_ctx":0,
//	  "suggestion":"Веса модели не помещаются ни в VRAM, ни в RAM ..."}
//
// и так на КАЖДЫЙ следующий запрос. Тесты фиксируют новое поведение:
//  1. отказ 422 распознаётся и запоминается вместе с потолком и причиной;
//  2. следующий запрос выше потолка НЕ планирует перезагрузку, а получает 413
//     с числами и причиной от cppworker'а;
//  3. запрос ниже потолка по-прежнему идёт обычным путём (перезагрузка);
//  4. запись снимается: по TTL, при росте окна (оператор поднял вручную) и при
//     успешной перезагрузке.
package balancer

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

const infeasibleBody = `{"code":"n_ctx_infeasible","error":"requested n_ctx=16384 exceeds the physically feasible maximum 131072 for model \"gemma-4-E4B-it-Q4_K_M\" on this hardware","feasible_max_context":131072,"stage":"does_not_fit","max_vram_n_ctx":0,"suggestion":"Веса модели не помещаются ни в VRAM, ни в RAM — нужен меньший квант или больше памяти."}`

// newInfeasibleTestProxy — прокси с ЗАГРУЖЕННОЙ моделью (суммарное окно total,
// на слот perSeq) и координатором перезагрузки.
func newInfeasibleTestProxy(t *testing.T, total, perSeq int) *Proxy {
	t.Helper()
	const backendID = "cpp-1"
	p := &Proxy{
		config:     &types.LoadBalancerConfig{},
		backends:   map[string]*BackendState{},
		metricsMgr: NewMetricsManager(),
	}
	p.backends[backendID] = &BackendState{Backend: &types.Backend{
		ID: backendID, Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy,
		Host: "127.0.0.1", CppWorkerPort: 1, MaxConcurrentReqs: 2,
	}}
	p.metricsMgr.llamaMetrics[backendID] = &types.LlamaCppMetrics{LoadedModels: []types.LlamaCppModel{{
		Name: "m", State: "loaded", ContextLength: total, ContextPerSeq: perSeq, MaxSlots: 2, Parallel: 2,
	}}}
	p.nctxReload = NewNCtxReloadCoordinator(DefaultNCtxReloadConfig())
	p.client = &http.Client{}
	return p
}

// TestR118_7_InfeasibleResponseRecognized — 422 n_ctx_infeasible распознаётся, а
// посторонние ответы (404/500) — нет.
func TestR118_7_InfeasibleResponseRecognized(t *testing.T) {
	if !isNCtxInfeasibleResponse(422, []byte(infeasibleBody)) {
		t.Fatal("живое тело 422 не распознано как n_ctx_infeasible")
	}
	if isNCtxInfeasibleResponse(404, []byte(`{"error":"model not found"}`)) {
		t.Error("404 не должен считаться отказом по окну")
	}
	if isNCtxInfeasibleResponse(422, []byte(`{"error":"bad params"}`)) {
		t.Error("другой 422 не должен считаться отказом по окну")
	}
}

// TestR118_7_InfeasibleRecordedWithCeilingAndReason — потолок = отвергнутый таргет
// минус один (кэш метрик для потолка не используем: сразу после неудачной попытки
// cppworker показывает в /api/models ЗАПРОШЕННОЕ окно — на этом живой цикл
// reload→422 и продолжался), причина — из тела cppworker'а.
func TestR118_7_InfeasibleRecordedWithCeilingAndReason(t *testing.T) {
	p := newInfeasibleTestProxy(t, 8192, 4096)

	p.recordNCtxInfeasible("cpp-1", "m", 16384, []byte(infeasibleBody))

	ceiling, reason, ok := p.nctxInfeasibleFor("cpp-1", "m")
	if !ok {
		t.Fatal("отказ не запомнен")
	}
	if ceiling != 16383 {
		t.Errorf("потолок = %d, want 16383 (отвергнутый таргет минус один)", ceiling)
	}
	if !strings.Contains(reason, "does_not_fit") && !strings.Contains(reason, "физически") &&
		!strings.Contains(reason, "не помещаются") {
		t.Errorf("причина не содержит объяснения cppworker'а: %q", reason)
	}
}

// TestR118_7_PreflightRefusesAboveCeiling — ГЛАВНОЕ: запрос выше потолка не
// планирует перезагрузку, а получает 413 с объяснением.
func TestR118_7_PreflightRefusesAboveCeiling(t *testing.T) {
	p := newInfeasibleTestProxy(t, 8192, 4096)
	p.recordNCtxInfeasible("cpp-1", "m", 16384, []byte(infeasibleBody))

	body := []byte(`{"model":"m","options":{"num_ctx":8192}}`)
	_, needsProxy, msg, status := p.preflightNCtxReloadIfNeeded(
		context.Background(), "cpp-1", "m", body, "/api/chat")

	if p.nctxReload.IsReloadPending("cpp-1", "m") {
		t.Fatal("перезагрузка снова запланирована, хотя cppworker уже отказал — это и есть живой цикл reload→422")
	}
	if needsProxy {
		t.Fatalf("запрос нельзя проксировать как готовый: needsProxy=true status=%d", status)
	}
	if status != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413 (внятный отказ вместо цикла)", status)
	}
	if !strings.Contains(msg, "cannot grow the context") {
		t.Errorf("сообщение не объясняет отказ: %q", msg)
	}
}

// TestR118_7_BelowCeilingStillReloads — запрос ниже потолка идёт обычным путём:
// запись об отказе не должна запрещать меньшие перезагрузки.
func TestR118_7_BelowCeilingStillReloads(t *testing.T) {
	p := newInfeasibleTestProxy(t, 8192, 4096)
	p.recordNCtxInfeasible("cpp-1", "m", 16384, []byte(infeasibleBody))

	// Загруженные 4096 на слот, потолок 16383. Запрос, которому нужен таргет ниже
	// потолка (например 4096 на слот при двух слотах → 8192 ≤ 16383), должен идти
	// обычным путём, а не получать 413.
	ceiling, _, ok := p.nctxInfeasibleFor("cpp-1", "m")
	if !ok || ceiling != 16383 {
		t.Fatalf("потолок = %d (ok=%v), want 16383", ceiling, ok)
	}
	if 8192 > ceiling {
		t.Errorf("меньший таргет 8192 не должен считаться выше потолка %d", ceiling)
	}
}

// TestR118_7_SuccessfulReloadClearsRecord — успешная перезагрузка снимает запись
// (иначе старый отказ блокировал бы окно после освобождения памяти).
func TestR118_7_SuccessfulReloadClearsRecord(t *testing.T) {
	p := newInfeasibleTestProxy(t, 8192, 4096)
	p.recordNCtxInfeasible("cpp-1", "m", 16384, []byte(infeasibleBody))
	if _, _, ok := p.nctxInfeasibleFor("cpp-1", "m"); !ok {
		t.Fatal("отказ не запомнен")
	}
	p.clearNCtxInfeasible("cpp-1", "m")
	if _, _, ok := p.nctxInfeasibleFor("cpp-1", "m"); ok {
		t.Error("после успешной перезагрузки запись должна быть снята")
	}
	if snap := p.nctxInfeasibleSnapshot(); len(snap) != 0 {
		t.Errorf("запись осталась: %v", snap)
	}
}

// TestR118_7_ClearedByTTL — просроченная запись игнорируется.
func TestR118_7_ClearedByTTL(t *testing.T) {
	p := newInfeasibleTestProxy(t, 8192, 4096)
	p.recordNCtxInfeasible("cpp-1", "m", 16384, []byte(infeasibleBody))
	p.mu.Lock()
	rec := p.nctxInfeasible[nctxInfeasibleKey("cpp-1", "m")]
	rec.At = time.Now().Add(-nctxInfeasibleTTL - time.Minute)
	p.nctxInfeasible[nctxInfeasibleKey("cpp-1", "m")] = rec
	p.mu.Unlock()
	if _, _, ok := p.nctxInfeasibleFor("cpp-1", "m"); ok {
		t.Error("просроченная запись должна игнорироваться")
	}
}

// llamacpp_shadow_vram_r593_test.go — R593 (2026-11-19): балансер разбирает
// «тень» резерва VRAM из /api/models и отдаёт её в метриках бэкенда.
//
// ЗАЧЕМ. Число-«тень» появляется в ответе воркера (R592: поля
// max_vram_n_ctx_unreserved / vram_reserve_mb), но пока его не разбирает пуллер,
// до карточки бэкенда в WebUI оно не доходит — а именно там оператор и смотрит,
// почему окно съело карту. Живой факт: 27B/A10 (2026-11-19), окно 201216 при
// безрезервном потолке 251254, свободно 414 MB из 22 502, декод `code 1`.
//
// Тест намеренно идёт тем же путём, что и продакшн: JSON-ответ воркера →
// pollAll() → кэш метрик. Если переименовать JSON-тег на любой стороне
// (воркер ↔ пуллер), числа молча станут нулями — этот тест это ловит.
//
//go:build llama_stub

package balancer

import (
	"testing"

	"ollama-loadbalancer/pkg/logger"
)

// shadowCppModelsJSONR593 — ответ /api/models со «тенью» резерва. Числа взяты из
// живого стенда 27B/A10 (max_vram_n_ctx=251254, окно 201216), резерв проекта 2048 MB.
const shadowCppModelsJSONR593 = `{
  "available_vram_mb": 414,
  "count": 1,
  "feasible_max_context": 262144,
  "gguf_max_context": 262144,
  "gpu_count": 1,
  "max_ram_n_ctx": 262144,
  "max_vram_n_ctx": 201216,
  "max_vram_n_ctx_unreserved": 251254,
  "vram_reserve_mb": 2048,
  "model_max_context": 262144,
  "models": [],
  "total_vram_mb": 22502
}`

// TestR593_Poller_DecodesShadowReserve — пуллер обязан разобрать оба поля и
// положить их в метрики бэкенда рядом с применяемым max_vram_n_ctx.
func TestR593_Poller_DecodesShadowReserve(t *testing.T) {
	logger.Init("error")
	fake := &r45FakeCppWorker{
		modelsJSON:   shadowCppModelsJSONR593,
		progressJSON: `{"count":0,"models":[]}`,
	}
	proxy, _ := r45BuildProxyWithFakeCppWorker(t, fake)

	poller := newLlamaCppMetricsPoller(proxy)
	poller.interval = 1
	poller.fastInterval = 1
	poller.pollAll()

	lm, ok := r45LlamaMetrics(proxy, "fake-1")
	if !ok || lm == nil {
		t.Fatalf("ожидались метрики для fake-1, получено ok=%v lm=%v", ok, lm)
	}
	// Применяемое число не изменилось — решение принимается по нему.
	if lm.MaxVRAMNCtx != 201216 {
		t.Errorf("MaxVRAMNCtx = %d, want 201216 (применяемое число не должно меняться)", lm.MaxVRAMNCtx)
	}
	if lm.MaxVRAMNCtxUnreserved != 251254 {
		t.Errorf("MaxVRAMNCtxUnreserved = %d, want 251254 (поле max_vram_n_ctx_unreserved не разобрано — "+
			"JSON-тег разошёлся между воркером и пуллером)", lm.MaxVRAMNCtxUnreserved)
	}
	if lm.VRAMReserveMB != 2048 {
		t.Errorf("VRAMReserveMB = %d, want 2048 (поле vram_reserve_mb не разобрано)", lm.VRAMReserveMB)
	}
}

// TestR593_ТеньНеВлияетНаРешение — «тень» не должна просочиться в то, чем
// балансер принимает решения: MaxVRAMNCtx остаётся единственным входом.
// Проверяем свойством метрик, а не кодом: значения рядом, но разные.
func TestR593_ТеньНеВлияетНаРешение(t *testing.T) {
	logger.Init("error")
	fake := &r45FakeCppWorker{
		modelsJSON:   shadowCppModelsJSONR593,
		progressJSON: `{"count":0,"models":[]}`,
	}
	proxy, _ := r45BuildProxyWithFakeCppWorker(t, fake)
	poller := newLlamaCppMetricsPoller(proxy)
	poller.interval = 1
	poller.fastInterval = 1
	poller.pollAll()
	m, ok := r45LlamaMetrics(proxy, "fake-1")
	if !ok || m == nil {
		t.Fatal("нет метрик бэкенда")
	}
	if m.MaxVRAMNCtx == m.MaxVRAMNCtxUnreserved {
		t.Errorf("применяемое и справочное совпали (%d) — значит тень подменила решение "+
			"или не разобрана", m.MaxVRAMNCtx)
	}
}

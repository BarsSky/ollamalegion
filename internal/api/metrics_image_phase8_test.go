//go:build llama_stub

// metrics_image_phase8_test.go — R-Image Phase 8 (2026-10-03): блок `image` в
// GET /api/v1/metrics.
//
// ЗАЧЕМ. /api/v1/metrics собирается в API-слое СВОИМ ответом
// (handlers_metrics.go), и блок политики/гейта/лока из balancer-сборщика в него
// не попадал. Из-за этого:
//   - страница «Image-бэкенды» не могла показать фактическую политику
//     сосуществования (coexistence_policy, vram_headroom_mb, ожидание очереди и
//     предохранитель лока) — вместо неё стоял литерал типа бэкенда;
//   - внешний потребитель метрик не видел ни гейта, ни потока image-запросов,
//     хотя документация обещала их именно здесь.
//
// Тест держит контракт: блок есть, политика в нём есть, счётчики и per-backend
// карта не пропали.
package api

import (
	"testing"

	"ollama-loadbalancer/pkg/types"
)

func TestMetricsImageSummary_Phase8(t *testing.T) {
	s, cleanup := setupTestServerWithBackends(t, []types.Backend{{
		ID: "img1", Host: "localhost", ImagePort: 18093,
		Type: types.BackendTypeImage, Status: types.StatusHealthy,
	}})
	defer cleanup()

	metrics := getMetricsJSONR78(t, s)
	img, ok := metrics["image"].(map[string]interface{})
	if !ok || img == nil {
		t.Fatalf("в метриках нет блока image: %T (страница «Image-бэкенды» берёт политику именно отсюда)", metrics["image"])
	}

	// Политика: без неё страница показывает прочерк вместо фактического режима.
	if v, ok := img["coexistence_policy"]; !ok || v == "" {
		t.Errorf("coexistence_policy отсутствует или пуст: %v", v)
	}
	for _, key := range []string{
		"gate_disabled", "vram_headroom_mb", "unknown_estimate_blocked",
		"queue_wait_timeout_sec", "exclusive_lock_timeout_sec",
		"gate_allowed_total", "gate_denied_total", "gate_denied_by_reason",
		"lock_acquired_total", "lock_held", "lock_wait_total", "lock_rejected_total",
		// Phase 8: поток запросов (агрегат + per-backend карта).
		"requests", "backends",
	} {
		if _, ok := img[key]; !ok {
			t.Errorf("в блоке image нет ключа %q", key)
		}
	}

	// Агрегат запросов: нули допустимы (генераций не было), но поля обязаны быть —
	// иначе UI не отличит «нет запросов» от «метрика не собрана».
	reqs, ok := img["requests"].(map[string]interface{})
	if !ok || reqs == nil {
		t.Fatalf("нет агрегата requests: %T", img["requests"])
	}
	for _, key := range []string{"in_flight", "total", "ok", "failed", "rejected", "rps", "avg_duration_ms"} {
		if _, ok := reqs[key]; !ok {
			t.Errorf("в агрегате requests нет ключа %q", key)
		}
	}
}

// Кластер без image-бэкендов: блок image всё равно отдаётся (политика и
// настройки гейта — глобальные), но агрегат запросов пустой. Это важно для
// страницы: она показывает политику до появления первого воркера.
func TestMetricsImageSummary_WithoutImageBackends_Phase8(t *testing.T) {
	s, cleanup := setupTestServerWithBackends(t, []types.Backend{{
		ID: "llm1", Host: "localhost", CppWorkerPort: 18092,
		Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy,
	}})
	defer cleanup()

	metrics := getMetricsJSONR78(t, s)
	if _, ok := metrics["image"].(map[string]interface{}); !ok {
		t.Fatalf("блок image должен отдаваться и без image-бэкендов (политика глобальна): %T", metrics["image"])
	}
}

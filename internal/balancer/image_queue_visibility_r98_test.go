package balancer

// R98 (S8 плана 2026-10-07): видимость image-запросов в /api/v1/queue/stats.
//
// Проверяем две вещи: (1) image-нагрузка реально попадает в статистику очереди
// (агрегат, «чей GPU занят», лента запросов); (2) на стенде без image-пути блок
// НЕ появляется — нулевой image-блок на текстовом стенде выглядел бы как
// «image задействован».

import (
	"testing"

	"ollama-loadbalancer/pkg/types"
)

func imageQueueProxyR98(t *testing.T, withImageBackend bool) *Proxy {
	t.Helper()
	cfg := createTestConfig()
	cfg.Backends = []types.Backend{{
		ID: "txt-1", Host: "127.0.0.1", CppWorkerPort: 18092,
		Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy,
		MaxConcurrentReqs: 2, Weight: 1,
	}}
	if withImageBackend {
		cfg.Backends = append(cfg.Backends, types.Backend{
			ID: "img-1", Host: "127.0.0.1", ImagePort: 18093,
			Type: types.BackendTypeImage, Status: types.StatusHealthy,
			MaxConcurrentReqs: 2, Weight: 1,
		})
	}
	return newProxyWithCleanup(t, cfg)
}

func TestImageQueueVisibility_AbsentWithoutImagePath_R98(t *testing.T) {
	p := imageQueueProxyR98(t, false)

	stats := p.GetQueueStats()
	if stats.Image != nil {
		t.Fatalf("на текстовом стенде блока image быть не должно: %v", stats.Image)
	}
}

func TestImageQueueVisibility_PresentForImageBackend_R98(t *testing.T) {
	p := imageQueueProxyR98(t, true)

	stats := p.GetQueueStats()
	if stats.Image == nil {
		t.Fatal("image-бэкенд зарегистрирован — оператор должен видеть этот путь в очереди")
	}
	ids, ok := stats.Image["backends"].([]string)
	if !ok || len(ids) != 1 || ids[0] != "img-1" {
		t.Fatalf("backends = %v, ожидался [img-1]", stats.Image["backends"])
	}
	if total, _ := stats.Image["total"].(int64); total != 0 {
		t.Errorf("без трафика total = %d, ожидался 0", total)
	}
	if _, ok := stats.Image["backends_in_flight"]; ok {
		t.Error("без активных генераций backends_in_flight не нужен")
	}
}

func TestImageQueueVisibility_ShowsInFlightAndBackend_R98(t *testing.T) {
	p := imageQueueProxyR98(t, true)

	// Стенд без живого imageworker: имитируем «в полёте» через внутренний API
	// счётчиков (тот же, что использует реальный путь генерации).
	handle := p.imageRes.imageRequests().begin("img-1", types.ImageRequestBrief{
		Model: "sd:flux", Path: "/v1/images/generations", Batch: 1,
	})

	stats := p.GetQueueStats()
	if stats.Image == nil {
		t.Fatal("блок image должен появиться при активной генерации")
	}
	// ВНИМАНИЕ на типы: `in_flight` в map — int (поле ImageRequestMetrics.InFlight),
	// а `total`/`ok` — int64. Тест фиксирует это, чтобы UI/API не разъехались.
	if inFlight, ok := stats.Image["in_flight"].(int); !ok || inFlight != 1 {
		t.Errorf("in_flight = %v (%T), ожидалось 1 (int)", stats.Image["in_flight"], stats.Image["in_flight"])
	}
	perBackend, ok := stats.Image["backends_in_flight"].(map[string]int64)
	if !ok || perBackend["img-1"] != 1 {
		t.Fatalf("backends_in_flight = %v, ожидалось {img-1:1}", stats.Image["backends_in_flight"])
	}
	// «Каждое видно в статистике»: запрос попадает в ленту сразу на старте, а не
	// только после завершения — иначе долгая генерация выглядела бы как тишина.
	if recent, ok := stats.Image["recent"].([]types.ImageRequestBrief); !ok || len(recent) == 0 {
		t.Fatalf("лента запросов должна содержать активный запрос: %v", stats.Image["recent"])
	}

	// Завершение: счётчики исхода появляются, «в полёте» обнуляется.
	handle.finish(types.ImageRequestStatusOK, 200, "", "", 1)
	stats = p.GetQueueStats()
	if inFlight, _ := stats.Image["in_flight"].(int); inFlight != 0 {
		t.Errorf("после завершения in_flight = %v, ожидался 0", stats.Image["in_flight"])
	}
	if ok, _ := stats.Image["ok"].(int64); ok != 1 {
		t.Errorf("ok = %v, ожидалось 1", stats.Image["ok"])
	}
	if _, ok := stats.Image["backends_in_flight"]; ok {
		t.Error("после завершения backends_in_flight не нужен")
	}
}

func TestImageQueueVisibility_TextBackendNotReportedAsImage_R98(t *testing.T) {
	p := imageQueueProxyR98(t, true)

	// Запрос по ТЕКСТОВОМУ узлу попал в те же счётчики (общий store) — он не
	// должен выглядеть как image-генерация на этом узле.
	p.imageRes.imageRequests().begin("txt-1", types.ImageRequestBrief{Model: "m"})

	stats := p.GetQueueStats()
	if stats.Image == nil {
		t.Fatal("блок должен быть: image-узел зарегистрирован")
	}
	if perBackend, ok := stats.Image["backends_in_flight"].(map[string]int64); ok {
		if _, bad := perBackend["txt-1"]; bad {
			t.Error("текстовый узел не должен попадать в backends_in_flight image-блока")
		}
	}
}

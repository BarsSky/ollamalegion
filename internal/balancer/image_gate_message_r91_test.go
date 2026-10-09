package balancer

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// R91 (2026-10-09). Живой случай: текстовая модель (gemma-4) занимала карту,
// image-модели не хватало места, и оператор видел только числа —
// «нужно 520 MB, свободно 512 MB». Из них не следовало ни то, КТО держит память,
// ни то, что делать. Тесты фиксируют контекст хоста в отказе гейта: на общем GPU
// отказ называет соседа и указывает ручку политики; на выделенном хосте лишнего
// текста в сообщении нет.

// TestImageGate_DenialNamesTextNeighbourOnSharedHost — общий хост: в отказе есть
// сосед-текстовик и путь к политике сосуществования.
func TestImageGate_DenialNamesTextNeighbourOnSharedHost(t *testing.T) {
	// Стенд по умолчанию: image-бэкенд и llama.cpp-бэкенд на одном хосте.
	p, stub := newImgResProxy(t, types.ImageResourceSettings{})
	stub.setWorkerVRAM(700, 16000) // рабочий набор 1024 MB, свободно 700

	rec := imgResPost(t, p, "/v1/images/generations", `{"model":"dall-e-2","prompt":"cat"}`)
	if rec.Code != 503 {
		t.Fatalf("status = %d, want 503 (body=%s)", rec.Code, rec.Body.String())
	}
	fields := imgResErrFields(t, rec)
	msg := imgResStr(t, fields, "message")
	if !strings.Contains(msg, "text backend") || !strings.Contains(msg, "llm-1") {
		t.Errorf("в отказе нет соседа-текстовика по имени: %q", msg)
	}
	if !strings.Contains(msg, "short by") {
		t.Errorf("в отказе нет недостачи: %q", msg)
	}
	hint := imgResStr(t, fields, "hint")
	if !strings.Contains(hint, "image/resources") {
		t.Errorf("hint не указывает, как менять политику: %q", hint)
	}
	if !strings.Contains(hint, "offload") || !strings.Contains(hint, "dedicated") {
		t.Errorf("hint не перечисляет варианты политики: %q", hint)
	}
	// OOM-лестница движка обязана остаться: она про саму модель, а не про хост.
	if !strings.Contains(hint, "quantization") {
		t.Errorf("hint потерял OOM-лестницу: %q", hint)
	}
}

// TestTextNeighbourIDs_GPUUUIDMatchesAcrossHostNames — R91: сосед по общей карте
// определяется и по GPU UUID, а не только по совпадению строки host.
//
// Живой случай: текстовая нода регистрируется как `cppworker-gpu`, image-воркер
// как `imageworker` (имена контейнеров на одной машине), поэтому сравнение по
// host не находило соседа — сообщение гейта не называло виновника, а fallback
// свободной VRAM не видел бюджет текстовой ноды. Агенты обеих отдают один и тот
// же gpu.uuids.
func TestTextNeighbourIDs_GPUUUIDMatchesAcrossHostNames(t *testing.T) {
	cfg := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{
			Host: "127.0.0.1", Port: 18080, APIPort: 18081, OpenAIPort: 18079,
			StatePath: filepath.Join(t.TempDir(), "state.json"),
		},
		Backends: []types.Backend{
			{ID: "imageworker", Host: "imageworker", ImagePort: 18093,
				Type: types.BackendTypeImage, Status: types.StatusHealthy, MaxConcurrentReqs: 2},
			{ID: "cppworker-gpu-bundled-agent", Host: "cppworker-gpu", CppWorkerPort: 18092,
				Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy, MaxConcurrentReqs: 4},
		},
		Balancing: types.BalancingSettings{
			Algorithm: types.AlgorithmResourceAware, RequestTimeout: 10,
			QueueTimeout: 5, QueueMaxSize: 50, QueueWorkers: 2, OperatingMode: "standard",
		},
	}
	p := newProxyWithCleanup(t, cfg)
	if p.imageRes == nil {
		t.Fatal("imageResources не инициализирован")
	}
	p.imageRes.Stop()
	t.Cleanup(p.imageRes.Stop)

	// Без метрик агентов соседа не видно (разные host) — прежнее поведение.
	if ids := p.imageRes.textNeighbourIDs("imageworker"); len(ids) != 0 {
		t.Fatalf("без UUID соседей быть не должно, получено %v", ids)
	}

	// Агенты обеих нод сообщают ОДНУ карту.
	p.UpdateMetrics("imageworker", &types.BackendMetrics{
		ID: "imageworker", GPU: types.GPUMetrics{UUIDs: []string{"GPU-6f8d5f3b-test"}},
	})
	p.UpdateMetrics("cppworker-gpu-bundled-agent", &types.BackendMetrics{
		ID: "cppworker-gpu-bundled-agent", GPU: types.GPUMetrics{UUIDs: []string{"GPU-6f8d5f3b-test"}},
	})
	ids := p.imageRes.textNeighbourIDs("imageworker")
	if len(ids) != 1 || ids[0] != "cppworker-gpu-bundled-agent" {
		t.Fatalf("сосед по общей карте не найден: %v", ids)
	}
	if !p.imageRes.hostHasTextNeighbour("imageworker") {
		t.Error("hostHasTextNeighbour обязан увидеть соседа по GPU UUID")
	}

	// Другая карта — не сосед.
	p.UpdateMetrics("cppworker-gpu-bundled-agent", &types.BackendMetrics{
		ID: "cppworker-gpu-bundled-agent", GPU: types.GPUMetrics{UUIDs: []string{"GPU-other"}},
	})
	if ids := p.imageRes.textNeighbourIDs("imageworker"); len(ids) != 0 {
		t.Fatalf("разные карты не должны считаться соседями: %v", ids)
	}
}

// TestImageGate_DenialWithoutTextNeighbourStaysShort — выделенный хост (под
// image-бэкенд отдельная машина): лишнего текста про соседа быть не должно.
func TestImageGate_DenialWithoutTextNeighbourStaysShort(t *testing.T) {
	stub := newImgResStub(t)
	cfg := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{
			Host: "127.0.0.1", Port: 18080, APIPort: 18081, OpenAIPort: 18079,
			StatePath: filepath.Join(t.TempDir(), "state.json"),
		},
		Backends: []types.Backend{{
			// Хост совпадает со стендом-заглушкой (иначе воркер недоступен и гейт
			// молчит по fail-open). Текстового бэкенда в конфиге НЕТ вовсе — это и
			// есть случай «карта выделена под image».
			ID: "img-1", Name: "image worker", Host: "127.0.0.1",
			ImagePort: stub.port(t), Type: types.BackendTypeImage,
			Status: types.StatusHealthy, MaxConcurrentReqs: 2,
		}},
		Balancing: types.BalancingSettings{
			Algorithm: types.AlgorithmResourceAware, RequestTimeout: 10,
			QueueTimeout: 5, QueueMaxSize: 50, QueueWorkers: 2, OperatingMode: "standard",
		},
	}
	p := newProxyWithCleanup(t, cfg)
	if p.imageRes != nil {
		p.imageRes.Stop()
	}
	t.Cleanup(func() {
		if p.imageRes != nil {
			p.imageRes.Stop()
		}
	})
	p.UpdateBackendStatus("img-1", types.StatusHealthy)
	p.imageRes.queueWaitOverride = 150 * time.Millisecond

	stub.setWorkerVRAM(700, 16000)
	rec := imgResPost(t, p, "/v1/images/generations", `{"model":"dall-e-2","prompt":"cat"}`)
	if rec.Code != 503 {
		t.Fatalf("status = %d, want 503 (body=%s)", rec.Code, rec.Body.String())
	}
	msg := imgResStr(t, imgResErrFields(t, rec), "message")
	if strings.Contains(msg, "same host") {
		t.Errorf("на выделенном хосте в отказе не должно быть соседа: %q", msg)
	}
	if !strings.Contains(msg, "short by") {
		t.Errorf("недостача обязана быть названа и здесь: %q", msg)
	}
}

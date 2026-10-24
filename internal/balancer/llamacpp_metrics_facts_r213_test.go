// R213 (2026-10-21): факты загруженного инстанса в LoadedModels.
//
// Живая жалоба оператора: «в настройках WebUI стоит квантизация KV-cache q4_0,
// а в отображении загруженных параметров — f16». Разбор стенда показал две
// независимые причины:
//
//  1. /api/v1/gguf/backends вообще не отдавал kv_cache_type загруженной модели
//     (только name/status/contextSize), и карточка подставляла f16 как догадку.
//     Это чинится в internal/api/gguf_backends_handler.go (R213).
//  2. Поллер терял часть фактов на пути воркер → балансер: BatchSize жёстко
//     присваивался 0, а NumGPULayers брался из поля `gpu_layers`, которого
//     cppworker в /api/models НЕ отдаёт вовсе (отдаёт только раскладку
//     device_layers_exact). Итог: «0 слоёв на GPU» при 42/42 на CUDA0.
//
// Этот тест фиксирует второй пункт на реальном ответе воркера (снят со стенда
// 2026-10-21 после загрузки gemma-4-E4B-it-Q4_K_M с kvCacheType=q4_0).
package balancer

import (
	"testing"

	"ollama-loadbalancer/pkg/logger"
)

// r213LiveModelsJSON — снятый с живого контейнера ответ /api/models.
// Обратите внимание: ключа gpu_layers в нём НЕТ, kv_cache_type — есть.
const r213LiveModelsJSON = `{
  "count": 1,
  "models": [
    {
      "active_queries": 0,
      "architecture": "gemma4",
      "batch_size": 512,
      "batched_parallel": false,
      "context_per_seq": 8192,
      "context_size": 8192,
      "device_layers_exact": [
        {"device": "CPU", "from": 0, "to": 0, "count": 1},
        {"device": "CUDA0", "from": 1, "to": 42, "count": 42}
      ],
      "feasible_max_context": 131072,
      "gguf_context_length": 131072,
      "gguf_max_context": 131072,
      "kv_cache_type": "q4_0",
      "max_slots": 1,
      "n_layers": 42,
      "name": "gemma-4-E4B-it-Q4_K_M",
      "parallel": 1,
      "path": "/app/models/gemma-4-E4B-it-Q4_K_M.gguf",
      "quantization": "Q4_K_M",
      "reasoning_enabled": false,
      "size_bytes": 4215695776,
      "state": "loaded"
    }
  ]
}`

func TestR213_Poller_KeepsKvAndRuntimeFacts(t *testing.T) {
	logger.Init("error")
	fake := &r45FakeCppWorker{
		modelsJSON:   r213LiveModelsJSON,
		progressJSON: `{"count":0,"models":[]}`,
	}
	proxy, _ := r45BuildProxyWithFakeCppWorker(t, fake)

	poller := newLlamaCppMetricsPoller(proxy)
	poller.pollAll()

	lm, ok := r45LlamaMetrics(proxy, "fake-1")
	if !ok || lm == nil || len(lm.LoadedModels) != 1 {
		t.Fatalf("expected one loaded model, got ok=%v lm=%+v", ok, lm)
	}
	m := lm.LoadedModels[0]

	// kv_cache_type — именно то, что оператор видит в карточке. Пропажа этого
	// поля = карточка снова начнёт печатать догадку вместо факта.
	if m.KvCacheType != "q4_0" {
		t.Errorf("KvCacheType = %q, want q4_0 (иначе WebUI покажет f16 вместо реального значения)", m.KvCacheType)
	}
	// batch_size воркер отдаёт; до R213 поллер писал сюда 0.
	if m.BatchSize != 512 {
		t.Errorf("BatchSize = %d, want 512 (R213: было жёстко 0)", m.BatchSize)
	}
	// gpu_layers воркер не отдаёт — считаем по авторитетной раскладке.
	if m.NumGPULayers != 42 {
		t.Errorf("NumGPULayers = %d, want 42 (R213: производное от device_layers_exact, CUDA0)", m.NumGPULayers)
	}
	if m.Parallel != 1 || m.MaxSlots != 1 {
		t.Errorf("parallel/maxSlots = %d/%d, want 1/1", m.Parallel, m.MaxSlots)
	}
	if m.ContextPerSeq != 8192 {
		t.Errorf("ContextPerSeq = %d, want 8192", m.ContextPerSeq)
	}
	if m.Quantization != "Q4_K_M" {
		t.Errorf("Quantization = %q, want Q4_K_M", m.Quantization)
	}
	if len(m.DeviceLayersExact) != 2 {
		t.Errorf("DeviceLayersExact = %+v, want 2 диапазона", m.DeviceLayersExact)
	}
	if m.ReasoningEnabled == nil || *m.ReasoningEnabled != false {
		t.Errorf("ReasoningEnabled = %v, want explicit false", m.ReasoningEnabled)
	}
}

// Контроль: явный gpu_layers от воркера (>0) НЕ перетирается расчётом по
// раскладке — факт воркера главнее производной.
func TestR213_Poller_ExplicitGpuLayersWins(t *testing.T) {
	logger.Init("error")
	fake := &r45FakeCppWorker{
		modelsJSON: `{"count":1,"models":[{
			"name":"m","state":"loaded","context_size":4096,
			"gpu_layers":17,
			"device_layers_exact":[{"device":"CPU","from":0,"to":24,"count":25},{"device":"CUDA0","from":25,"to":41,"count":17}]
		}]}`,
		progressJSON: `{"count":0,"models":[]}`,
	}
	proxy, _ := r45BuildProxyWithFakeCppWorker(t, fake)
	poller := newLlamaCppMetricsPoller(proxy)
	poller.pollAll()

	lm, _ := r45LlamaMetrics(proxy, "fake-1")
	if lm == nil || len(lm.LoadedModels) != 1 {
		t.Fatalf("expected one loaded model, got %+v", lm)
	}
	if got := lm.LoadedModels[0].NumGPULayers; got != 17 {
		t.Errorf("NumGPULayers = %d, want 17 (явное значение воркера не пересчитывать)", got)
	}
}

// Контроль: без device_layers_exact и без gpu_layers остаётся 0 = «воркер не
// сообщил». Ноль здесь не выдаётся за «модель целиком на CPU».
func TestR213_Poller_NoLayersInfoStaysZero(t *testing.T) {
	logger.Init("error")
	fake := &r45FakeCppWorker{
		modelsJSON:   `{"count":1,"models":[{"name":"m","state":"loaded","context_size":4096}]}`,
		progressJSON: `{"count":0,"models":[]}`,
	}
	proxy, _ := r45BuildProxyWithFakeCppWorker(t, fake)
	poller := newLlamaCppMetricsPoller(proxy)
	poller.pollAll()

	lm, _ := r45LlamaMetrics(proxy, "fake-1")
	if lm == nil || len(lm.LoadedModels) != 1 {
		t.Fatalf("expected one loaded model, got %+v", lm)
	}
	m := lm.LoadedModels[0]
	if m.NumGPULayers != 0 || m.KvCacheType != "" {
		t.Errorf("unknown facts must stay zero: gpu=%d kv=%q", m.NumGPULayers, m.KvCacheType)
	}
}

// image_capabilities_test.go — Phase 4 (discovery): тесты агрегатора
// возможностей image-бэкендов (internal/balancer/image_capabilities.go).
//
// Покрывают обязательные утверждения задачи:
//   - объединение samplers/schedulers/loras/upscalers с сохранением источника;
//   - консервативный мёрж лимитов (max нижних границ, min верхних, сумма очередей);
//   - бэкенд без лимитов НЕ участвует в мёрже лимитов, но его возможности — да;
//   - недоступный воркер не ломает агрегат: второй бэкенд всё равно отдаётся,
//     причина ошибки видна оператору;
//   - пустой кластер (нет image_cpp) → валидный пустой агрегат, total=0;
//   - фолбэк на «голый» sd-server (/sdcpp/v1/capabilities);
//   - TTL-кэш: попадание и протухание.
package balancer

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================
// Мок image-воркера
// ============================================================

// imageCapsWorkerMock — мок sdworker: /api/image/capabilities отдаёт заданный
// JSON (или 404 — тогда проверяется фолбэк на движок), /sdcpp/v1/capabilities —
// ответ «голого» sd-server.
type imageCapsWorkerMock struct {
	server     *httptest.Server
	capsJSON   string
	engineJSON string
	capsStatus int

	capsHits   int32
	engineHits int32
}

func newImageCapsWorkerMock(t *testing.T, capsJSON string) *imageCapsWorkerMock {
	t.Helper()
	m := &imageCapsWorkerMock{capsJSON: capsJSON, capsStatus: http.StatusOK}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/image/capabilities", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&m.capsHits, 1)
		if m.capsStatus != http.StatusOK {
			w.WriteHeader(m.capsStatus)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(m.capsJSON))
	})
	mux.HandleFunc("/sdcpp/v1/capabilities", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&m.engineHits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(m.engineJSON))
	})
	m.server = httptest.NewServer(mux)
	t.Cleanup(m.server.Close)
	return m
}

func (m *imageCapsWorkerMock) port(t *testing.T) int {
	t.Helper()
	return portFromURL(t, m.server.URL)
}

// ============================================================
// Тестовые фикстуры
// ============================================================

// capsWorkerJSON_A — ответ воркера «A»: свои samplers, LoRA, апскейлер, лимиты
// 64…2048 / batch 8 / очередь 64, одна загруженная модель.
const capsWorkerJSON_A = `{
  "ready": true,
  "state": "loaded",
  "model": "m-a",
  "pid": 4242,
  "engine": {
    "samplers": ["euler", "euler_a"],
    "schedulers": ["discrete"],
    "loras": [{"name": "a-lora", "path": "/loras/a.safetensors"}],
    "upscalers": [{"name": "esrgan-x4", "model": true, "image_upscale": false}]
  },
  "models": [{
    "name": "m-a", "state": "loaded", "family": "sdxl",
    "size_bytes": 123456, "vram_estimate_mb": 4096, "active_queries": 2,
    "defaults": {"steps": 25, "cfgScale": 7, "sampler": "euler_a", "scheduler": "discrete", "width": 1024, "height": 1024, "batchCount": 1, "seed": -1}
  }],
  "limits": {
    "min_width": 64, "max_width": 2048, "min_height": 64, "max_height": 2048,
    "size_multiple": 64, "max_batch_count": 8, "min_steps": 1, "max_steps": 100,
    "max_cfg_scale": 30, "queue_size": 64, "queue_in_flight": 1,
    "completed_job_ttl_seconds": 600, "generation_timeout_seconds": 600,
    "seed_always_positive": true, "supports_response_format_url": true, "cancel_queued_only": true
  },
  "warnings": ["lora catalog truncated"]
}`

// capsWorkerJSON_B — ответ воркера «B»: другие samplers, другие лимиты
// (128…4096, batch 4, очередь 32) и вторая модель.
const capsWorkerJSON_B = `{
  "ready": false,
  "state": "not_loaded",
  "engine": {
    "samplers": ["euler_a", "dpm++2m"],
    "schedulers": ["karras"],
    "loras": [{"name": "b-lora", "path": "/loras/b.safetensors"}],
    "upscalers": [{"name": "realesrgan", "model": false, "image_upscale": true}]
  },
  "models": [{
    "name": "m-b", "state": "not_loaded", "family": "sd15",
    "size_bytes": 55, "active_queries": 0,
    "defaults": {"steps": 20, "cfgScale": 7, "sampler": "dpm++2m", "scheduler": "karras", "width": 512, "height": 512, "batchCount": 1, "seed": -1}
  }],
  "limits": {
    "min_width": 128, "max_width": 4096, "min_height": 128, "max_height": 4096,
    "size_multiple": 32, "max_batch_count": 4, "max_queue_size": 32,
    "queue_in_flight": 2, "completed_job_ttl_seconds": 300, "generation_timeout_seconds": 900
  }
}`

// newImageCapsProxy — прокси с заданными image-бэкендами.
func newImageCapsProxy(t *testing.T, backends ...types.Backend) *Proxy {
	t.Helper()
	cfg := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{
			Host:       "127.0.0.1",
			Port:       18080,
			APIPort:    18081,
			OpenAIPort: 18079,
			StatePath:  t.TempDir() + "/state.json",
		},
		Backends: backends,
		Balancing: types.BalancingSettings{
			Algorithm:      types.AlgorithmResourceAware,
			RequestTimeout: 10,
			QueueTimeout:   5,
			QueueMaxSize:   50,
			QueueWorkers:   2,
			OperatingMode:  "standard",
		},
	}
	return newProxyWithCleanup(t, cfg)
}

// imageCapsBackend — image-бэкенд на порту мока.
func imageCapsBackend(id string, port int) types.Backend {
	return types.Backend{
		ID: id, Name: "image " + id, Host: "127.0.0.1", ImagePort: port,
		Type: types.BackendTypeImage, Status: types.StatusHealthy, MaxConcurrentReqs: 2,
	}
}

// deadImagePort — порт, на котором заведомо никто не слушает.
func deadImagePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	require.NoError(t, ln.Close())
	return port
}

// aggregateImageCaps — вызов агрегатора без токена (+ сброс кэша на входе,
// чтобы тесты не влияли друг на друга через пакетный кэш).
func aggregateImageCaps(t *testing.T, p *Proxy) *ImageCapabilitiesAggregate {
	t.Helper()
	ResetImageCapabilitiesCache()
	t.Cleanup(ResetImageCapabilitiesCache)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return p.AggregateImageCapabilities(ctx, nil)
}

// ============================================================
// 1. Объединение и консервативный мёрж
// ============================================================

func TestAggregateImageCapabilities_MergesTwoBackends(t *testing.T) {
	mockA := newImageCapsWorkerMock(t, capsWorkerJSON_A)
	mockB := newImageCapsWorkerMock(t, capsWorkerJSON_B)

	p := newImageCapsProxy(t,
		imageCapsBackend("img-a", mockA.port(t)),
		imageCapsBackend("img-b", mockB.port(t)),
	)

	agg := aggregateImageCaps(t, p)

	// --- сводка опроса -------------------------------------------------------
	assert.Equal(t, 2, agg.Backends.Total)
	assert.Equal(t, 2, agg.Backends.Probed)
	assert.Equal(t, 2, agg.Backends.Answered)
	assert.Equal(t, 0, agg.Backends.Failed)
	assert.Equal(t, 0, agg.Backends.Skipped)
	assert.Empty(t, agg.Backends.Errors)
	assert.Len(t, agg.Backends.Probes, 2)
	assert.False(t, agg.Cached, "первый вызов не может быть из кэша")
	for _, probe := range agg.Backends.Probes {
		assert.True(t, probe.OK, "backend %s must be ok", probe.BackendID)
		assert.Equal(t, "worker", probe.Source)
	}

	// --- объединение с источниками -------------------------------------------
	samplers := map[string][]string{}
	for _, s := range agg.Samplers {
		samplers[s.Name] = s.Backends
	}
	assert.Equal(t, []string{"img-a"}, samplers["euler"])
	assert.Equal(t, []string{"img-a", "img-b"}, samplers["euler_a"],
		"общий sampler обязан быть привязан к обоим бэкендам")
	assert.Equal(t, []string{"img-b"}, samplers["dpm++2m"])

	schedulers := map[string][]string{}
	for _, s := range agg.Schedulers {
		schedulers[s.Name] = s.Backends
	}
	assert.Equal(t, []string{"img-a"}, schedulers["discrete"])
	assert.Equal(t, []string{"img-b"}, schedulers["karras"])

	require.Len(t, agg.Loras, 2)
	assert.Equal(t, "a-lora", agg.Loras[0].Name)
	assert.Equal(t, []string{"img-a"}, agg.Loras[0].Backends)
	assert.Equal(t, "b-lora", agg.Loras[1].Name)

	upscalers := map[string]ImageUpscalerSupport{}
	for _, u := range agg.Upscalers {
		upscalers[u.Name] = u
	}
	assert.True(t, upscalers["esrgan-x4"].Model)
	assert.True(t, upscalers["realesrgan"].ImageUpscale)

	// --- консервативный мёрж лимитов -----------------------------------------
	lim := agg.Limits
	assert.Equal(t, 128, lim.MinWidth, "min_width = MAX по бэкендам (самый высокий порог)")
	assert.Equal(t, 128, lim.MinHeight)
	assert.Equal(t, 2048, lim.MaxWidth, "max_width = MIN по бэкендам")
	assert.Equal(t, 2048, lim.MaxHeight)
	assert.Equal(t, 4, lim.MaxBatchCount, "max_batch_count = MIN")
	assert.Equal(t, 96, lim.MaxQueueSize, "max_queue_size = СУММА (64+32)")
	assert.Equal(t, 3, lim.QueueInFlight, "queue_in_flight = СУММА")
	assert.Equal(t, 1, lim.MinSteps)
	assert.Equal(t, 100, lim.MaxSteps)
	assert.Equal(t, 64, lim.SizeMultiple, "size_multiple = LCM(64,32)")
	assert.Equal(t, 300, lim.CompletedJobTTLSeconds, "TTL = MIN по бэкендам")
	assert.Equal(t, 600, lim.GenerationTimeoutSeconds, "timeout = MIN по бэкендам")
	require.NotNil(t, lim.CancelQueuedOnly)
	assert.True(t, *lim.CancelQueuedOnly, "флаг = AND: у img-b он не сообщён, значит не влияет")
	assert.Equal(t, []string{"img-a", "img-b"}, lim.SourceBackends)

	// --- модели ---------------------------------------------------------------
	require.Len(t, agg.Models, 2)
	assert.Equal(t, "img-a", agg.Models[0].BackendID)
	assert.Equal(t, "m-a", agg.Models[0].Name)
	assert.Equal(t, "sdxl", agg.Models[0].Family)
	assert.Equal(t, "loaded", agg.Models[0].State)
	assert.Equal(t, 4096, agg.Models[0].VramEstimateMB)
	assert.Equal(t, 25, agg.Models[0].Defaults.Steps)
	assert.Equal(t, "img-b", agg.Models[1].BackendID)
	assert.Equal(t, []string{"m-a", "m-b"}, agg.AllModels)

	// --- предупреждения воркера не теряются -----------------------------------
	require.NotEmpty(t, agg.Warnings)
	assert.Contains(t, agg.Warnings[0], "lora catalog truncated")
	assert.Contains(t, agg.Warnings[0], "img-a")
}

// ============================================================
// 2. Ошибка одного бэкенда не ломает агрегат
// ============================================================

func TestAggregateImageCapabilities_OneBackendDown(t *testing.T) {
	mockA := newImageCapsWorkerMock(t, capsWorkerJSON_A)

	p := newImageCapsProxy(t,
		imageCapsBackend("img-a", mockA.port(t)),
		imageCapsBackend("img-dead", deadImagePort(t)),
	)
	agg := aggregateImageCaps(t, p)

	assert.Equal(t, 2, agg.Backends.Total)
	assert.Equal(t, 2, agg.Backends.Probed)
	assert.Equal(t, 1, agg.Backends.Answered)
	assert.Equal(t, 1, agg.Backends.Failed)
	require.Len(t, agg.Backends.Errors, 1)
	assert.Equal(t, "img-dead", agg.Backends.Errors[0].BackendID)
	assert.NotEmpty(t, agg.Backends.Errors[0].Error, "причина недоступности обязана быть видна")

	// Живой бэкенд всё равно отдаётся целиком.
	require.Len(t, agg.Samplers, 2)
	assert.Equal(t, []string{"m-a"}, agg.AllModels)
	assert.Equal(t, 2048, agg.Limits.MaxWidth, "лимиты считаются только по ответившим")

	// Обе пробы присутствуют (в т.ч. упавшая) — оператор видит, кто именно молчит.
	probes := map[string]ImageBackendProbe{}
	for _, pr := range agg.Backends.Probes {
		probes[pr.BackendID] = pr
	}
	require.Contains(t, probes, "img-dead")
	assert.False(t, probes["img-dead"].OK)
	assert.True(t, probes["img-dead"].Probed)
	assert.NotEmpty(t, probes["img-dead"].Error)
}

// ============================================================
// 3. Пустой кластер
// ============================================================

func TestAggregateImageCapabilities_EmptyCluster(t *testing.T) {
	// Только текстовый бэкенд: image_cpp нет вовсе.
	p := newImageCapsProxy(t, types.Backend{
		ID: "llm-1", Host: "127.0.0.1", CppWorkerPort: 18092,
		Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy,
	})

	agg := aggregateImageCaps(t, p)

	require.NotNil(t, agg)
	assert.Equal(t, 0, agg.Backends.Total)
	assert.Equal(t, 0, agg.Backends.Probed)
	assert.Equal(t, 0, agg.Backends.Answered)
	assert.Equal(t, 0, agg.Backends.Failed)
	assert.NotNil(t, agg.Samplers, "пустые списки, а не null (JSON-контракт обещает массивы)")
	assert.Empty(t, agg.Samplers)
	assert.Empty(t, agg.Models)
	assert.Empty(t, agg.AllModels)
	assert.Empty(t, agg.Limits.SourceBackends)
}

// ============================================================
// 4. Бэкенд без лимитов не участвует в мёрже лимитов
// ============================================================

const capsWorkerJSON_NoLimits = `{
  "ready": true, "state": "loaded",
  "engine": {"samplers": ["lcm"], "schedulers": ["ays"]},
  "models": [{"name": "m-c", "state": "loaded", "family": "z_image",
              "defaults": {"steps": 8, "cfgScale": 1, "sampler": "lcm", "scheduler": "ays", "width": 512, "height": 1024, "batchCount": 1, "seed": -1}}]
}`

func TestAggregateImageCapabilities_BackendWithoutLimitsIsSkippedInLimitsMerge(t *testing.T) {
	mockPlain := newImageCapsWorkerMock(t, capsWorkerJSON_NoLimits)
	mockA := newImageCapsWorkerMock(t, capsWorkerJSON_A)

	p := newImageCapsProxy(t,
		imageCapsBackend("img-plain", mockPlain.port(t)),
		imageCapsBackend("img-a", mockA.port(t)),
	)
	agg := aggregateImageCaps(t, p)

	assert.Equal(t, 2, agg.Backends.Answered)
	// Возможности бэкенда без лимитов видны...
	assert.Equal(t, []string{"img-plain"}, findSamplerBackends(agg, "lcm"))
	assert.Contains(t, agg.AllModels, "m-c")
	// ...а в мёрже лимитов он НЕ участвует: границы целиком от img-a.
	assert.Equal(t, []string{"img-a"}, agg.Limits.SourceBackends)
	assert.Equal(t, 64, agg.Limits.MinWidth)
	assert.Equal(t, 2048, agg.Limits.MaxWidth)
	assert.Equal(t, 64, agg.Limits.MaxQueueSize)
}

// findSamplerBackends — источники sampler'а по имени.
func findSamplerBackends(agg *ImageCapabilitiesAggregate, name string) []string {
	for _, s := range agg.Samplers {
		if s.Name == name {
			return s.Backends
		}
	}
	return nil
}

// ============================================================
// 5. Фолбэк на «голый» sd-server
// ============================================================

// capsEngineJSON — ответ /sdcpp/v1/capabilities (движок без воркера): массивы
// лежат в КОРНЕ, моделей и queue_size нет вовсе.
const capsEngineJSON = `{
  "model": {"name": "sd-cpp-local", "stem": "sd-cpp-local", "path": "/models/x.gguf"},
  "samplers": ["euler", "ddim_trailing"],
  "schedulers": ["discrete"],
  "loras": [{"name": "plain-lora", "path": "/loras/p.safetensors"}],
  "upscalers": [{"name": "esrgan-x2", "model": true, "image_upscale": false}],
  "limits": {"min_width": 64, "max_width": 1024, "min_height": 64, "max_height": 1024,
             "max_batch_count": 2, "max_queue_size": 16}
}`

func TestAggregateImageCapabilities_FallsBackToEngineEndpoint(t *testing.T) {
	mock := newImageCapsWorkerMock(t, `{}`)
	mock.capsStatus = http.StatusNotFound
	mock.engineJSON = capsEngineJSON

	p := newImageCapsProxy(t, imageCapsBackend("img-engine", mock.port(t)))
	agg := aggregateImageCaps(t, p)

	assert.Equal(t, 1, agg.Backends.Answered)
	require.Len(t, agg.Backends.Probes, 1)
	assert.Equal(t, "engine", agg.Backends.Probes[0].Source,
		"воркер отдал 404 — читаем возможности прямо у движка")
	assert.Equal(t, int32(1), atomic.LoadInt32(&mock.capsHits))
	assert.Equal(t, int32(1), atomic.LoadInt32(&mock.engineHits))

	assert.Equal(t, []string{"img-engine"}, findSamplerBackends(agg, "euler"))
	assert.Equal(t, 16, agg.Limits.MaxQueueSize, "max_queue_size движка тоже учитывается")
	assert.Equal(t, 2, agg.Limits.MaxBatchCount)
	assert.Empty(t, agg.AllModels, "у «голого» движка списка моделей нет")
}

// ============================================================
// 6. Пропуск нездорового бэкенда
// ============================================================

func TestAggregateImageCapabilities_SkipsUnhealthyBackend(t *testing.T) {
	mockA := newImageCapsWorkerMock(t, capsWorkerJSON_A)
	mockSick := newImageCapsWorkerMock(t, capsWorkerJSON_B)

	sick := imageCapsBackend("img-sick", mockSick.port(t))
	sick.Status = types.StatusUnhealthy
	p := newImageCapsProxy(t, imageCapsBackend("img-a", mockA.port(t)), sick)

	agg := aggregateImageCaps(t, p)

	assert.Equal(t, 2, agg.Backends.Total)
	assert.Equal(t, 1, agg.Backends.Probed)
	assert.Equal(t, 1, agg.Backends.Skipped)
	assert.Equal(t, int32(0), atomic.LoadInt32(&mockSick.capsHits),
		"нездоровый бэкенд не опрашивается вовсе")

	var sickProbe *ImageBackendProbe
	for i := range agg.Backends.Probes {
		if agg.Backends.Probes[i].BackendID == "img-sick" {
			sickProbe = &agg.Backends.Probes[i]
		}
	}
	require.NotNil(t, sickProbe, "пропущенный бэкенд обязан быть виден оператору")
	assert.True(t, sickProbe.Skipped)
	assert.NotEmpty(t, sickProbe.Reason)
	assert.Contains(t, sickProbe.Reason, "unhealthy")
}

// ============================================================
// 7. TTL-кэш
// ============================================================

func TestAggregateImageCapabilities_TTLCache(t *testing.T) {
	mockA := newImageCapsWorkerMock(t, capsWorkerJSON_A)
	p := newImageCapsProxy(t, imageCapsBackend("img-a", mockA.port(t)))

	ResetImageCapabilitiesCache()
	t.Cleanup(ResetImageCapabilitiesCache)
	// Короткий TTL: протухание проверяем реальным sleep (без мока часов).
	imageCapabilitiesCacheTTLOverride = 50 * time.Millisecond
	t.Cleanup(func() { imageCapabilitiesCacheTTLOverride = 0 })

	ctx := context.Background()

	first := p.AggregateImageCapabilities(ctx, nil)
	require.False(t, first.Cached)
	require.Equal(t, int32(1), atomic.LoadInt32(&mockA.capsHits))

	second := p.AggregateImageCapabilities(ctx, nil)
	assert.True(t, second.Cached, "в пределах TTL агрегат отдаётся из кэша")
	assert.Equal(t, int32(1), atomic.LoadInt32(&mockA.capsHits),
		"попадание в кэш не должно ходить в воркер второй раз")
	assert.Equal(t, first.GeneratedAt, second.GeneratedAt)
	assert.Equal(t, len(first.Samplers), len(second.Samplers))

	time.Sleep(80 * time.Millisecond)
	third := p.AggregateImageCapabilities(ctx, nil)
	assert.False(t, third.Cached, "после TTL агрегат собирается заново")
	assert.Equal(t, int32(2), atomic.LoadInt32(&mockA.capsHits))
}

// TestAggregateImageCapabilities_CacheInvalidatedByTopology — кэш привязан к
// подписи кластера: смена состава/портов/статуса не отдаёт чужой агрегат.
func TestAggregateImageCapabilities_CacheInvalidatedByTopology(t *testing.T) {
	mockA := newImageCapsWorkerMock(t, capsWorkerJSON_A)
	mockB := newImageCapsWorkerMock(t, capsWorkerJSON_B)

	p := newImageCapsProxy(t, imageCapsBackend("img-a", mockA.port(t)))
	ResetImageCapabilitiesCache()
	t.Cleanup(ResetImageCapabilitiesCache)

	first := p.AggregateImageCapabilities(context.Background(), nil)
	require.Equal(t, 1, first.Backends.Total)
	require.True(t, first.Cached == false)

	// Добавили второй image-бэкенд: подпись изменилась → агрегат пересобирается
	// несмотря на свежий TTL.
	require.NoError(t, p.AddBackend(imageCapsBackend("img-b", mockB.port(t))))
	second := p.AggregateImageCapabilities(context.Background(), nil)
	assert.False(t, second.Cached)
	assert.Equal(t, 2, second.Backends.Total)
	assert.Equal(t, int32(1), atomic.LoadInt32(&mockB.capsHits))
}

// TestAggregateImageCapabilities_TokenIsForwarded — токен бэкенда уходит воркеру
// заголовком X-API-Token (read-only проба обязана проходить авторизацию, если
// она включена на воркере).
func TestAggregateImageCapabilities_TokenIsForwarded(t *testing.T) {
	var gotToken int32
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/image/capabilities" {
			atomic.AddInt32(&gotToken, 1)
			seen = r.Header.Get(types.HeaderXAPIToken)
			_, _ = w.Write([]byte(capsWorkerJSON_A))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	p := newImageCapsProxy(t, imageCapsBackend("img-a", portFromURL(t, srv.URL)))
	ResetImageCapabilitiesCache()
	t.Cleanup(ResetImageCapabilitiesCache)

	agg := p.AggregateImageCapabilities(context.Background(), func(backendID string) string {
		if backendID == "img-a" {
			return "secret-token"
		}
		return ""
	})
	require.Equal(t, 1, agg.Backends.Answered)
	assert.Equal(t, int32(1), atomic.LoadInt32(&gotToken))
	assert.Equal(t, "secret-token", seen)
}

// TestAggregateImageCapabilities_InvalidJSONIsReported — невалидный ответ
// воркера — это ошибка пробы, а не паника и не «пустой успех».
func TestAggregateImageCapabilities_InvalidJSONIsReported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html>not json</html>"))
	}))
	t.Cleanup(srv.Close)

	p := newImageCapsProxy(t, imageCapsBackend("img-broken", portFromURL(t, srv.URL)))
	ResetImageCapabilitiesCache()
	t.Cleanup(ResetImageCapabilitiesCache)

	agg := p.AggregateImageCapabilities(context.Background(), nil)
	assert.Equal(t, 1, agg.Backends.Failed)
	assert.Equal(t, 0, agg.Backends.Answered)
	require.Len(t, agg.Backends.Errors, 1)
	assert.Contains(t, agg.Backends.Errors[0].Error, "not valid JSON")
	assert.Contains(t, agg.Warnings, "no image backend answered: capabilities aggregate is empty")
}

// TestImageCapsLCM — НОК, а не max: сетка должна быть кратна шагу каждого воркера.
func TestImageCapsLCM(t *testing.T) {
	cases := []struct{ a, b, want int }{
		{64, 32, 64},
		{0, 64, 64},
		{32, 0, 32},
		{8, 12, 24},
		{64, 64, 64},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, imgCapsLCM(c.a, c.b), fmt.Sprintf("LCM(%d,%d)", c.a, c.b))
	}
}

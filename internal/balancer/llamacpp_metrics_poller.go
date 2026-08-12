package balancer

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// llamaCppMetricsPoller периодически опрашивает /api/models у каждого
// зарегистрированного llama.cpp бэкенда и обновляет кэш
// metricsMgr.llamaMetrics[backendID].LoadedModels, чтобы WebUI страница
// GGUF Models и API /api/v1/gguf/backends показывали актуальный список
// загруженных в VRAM моделей.
//
// Также опрашивает /api/models/load/progress для отслеживания текущих
// загрузок (State="loading") и заполняет LoadingModels. UI (монитор,
// вкладка бэкендов, GGUF-таб) использует LoadingModels для отображения
// спиннера и elapsed-time «Загружается model-name 25s».
//
// Без этого поллера LoadedModels заполнялись бы только когда balancer
// сам инициирует warmup (см. updateLlamaCppRunningModelInMetrics).
// Но если модель загружена напрямую через cppworker API в обход
// балансера (например, через UI cppworker, или руками), балансер об
// этом не узнает и покажет пустой список.
//
// Адаптивный интервал: при наличии loading-моделей poll идёт раз в 2 сек
// (чтобы UI обновлялся почти в реальном времени), иначе — раз в 30 сек
// (чтобы не нагружать cppworker).
type llamaCppMetricsPoller struct {
	proxy        *Proxy
	interval     time.Duration // базовый интервал (default 30s)
	fastInterval time.Duration // интервал при loading (default 2s)
	httpClient   *http.Client

	mu      sync.Mutex
	running bool
	stopCh  chan struct{}

	// lastLoadingSeen — время последнего наблюдения loading-моделей.
	// Используется для решения, какой интервал использовать в следующем цикле.
	lastLoadingSeen time.Time
}

// newLlamaCppMetricsPoller создаёт poller с интервалом по умолчанию 30 секунд.
func newLlamaCppMetricsPoller(proxy *Proxy) *llamaCppMetricsPoller {
	return &llamaCppMetricsPoller{
		proxy:        proxy,
		interval:     30 * time.Second,
		fastInterval: 2 * time.Second,
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
		},
		stopCh: make(chan struct{}),
	}
}

// Start запускает фоновый poll в отдельной горутине. Идемпотентно —
// повторный вызов игнорируется.
func (p *llamaCppMetricsPoller) Start() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.running {
		return
	}
	p.running = true
	p.stopCh = make(chan struct{})
	go p.loop()
	logger.Get().Infow("llamaCppMetricsPoller started", "interval", p.interval)
}

// Stop останавливает фоновый poll.
func (p *llamaCppMetricsPoller) Stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.running {
		return
	}
	p.running = false
	close(p.stopCh)
}

// loop — основной цикл поллера. Сначала делает немедленный poll
// (чтобы не ждать первый интервал после старта), затем спит interval.
//
// Адаптивный интервал:
//   - Если в предыдущем poll'е были loading-модели (lastLoadingSeen свежее) →
//     следующий poll через fastInterval (2s), чтобы UI обновлялся в реальном времени.
//   - Иначе → через interval (30s), чтобы не нагружать cppworker.
//
// Это позволяет:
//   - мгновенно показывать прогресс загрузки в UI (каждые 2 сек)
//   - не тратить ресурсы, когда загрузок нет (каждые 30 сек)
func (p *llamaCppMetricsPoller) loop() {
	// Немедленный первый poll (для быстрого старта после перезапуска)
	p.pollAll()

	for {
		// Выбираем интервал в зависимости от того, видели ли loading недавно.
		sleepDur := p.interval
		p.mu.Lock()
		seenRecently := !p.lastLoadingSeen.IsZero() && time.Since(p.lastLoadingSeen) < 30*time.Second
		p.mu.Unlock()
		if seenRecently {
			sleepDur = p.fastInterval
		}

		timer := time.NewTimer(sleepDur)
		select {
		case <-p.stopCh:
			timer.Stop()
			return
		case <-timer.C:
			p.pollAll()
		}
	}
}

// pollAll опрашивает все llama.cpp бэкенды параллельно.
func (p *llamaCppMetricsPoller) pollAll() {
	if p.proxy.llamaCppRouter == nil {
		logger.Get().Debugw("llamaCppMetricsPoller: router is nil, skipping")
		return
	}
	backends := p.proxy.llamaCppRouter.getLlamaCppBackends()
	if len(backends) == 0 {
		return
	}

	var wg sync.WaitGroup
	for _, b := range backends {
		wg.Add(1)
		go func(bi backendInfo) {
			defer wg.Done()
			p.pollBackend(bi)
		}(b)
	}
	wg.Wait()
}

// pollBackend опрашивает /api/models у одного бэкенда и обновляет кэш.
func (p *llamaCppMetricsPoller) pollBackend(b backendInfo) {
	url := fmt.Sprintf("http://%s:%d/api/models", b.host, b.port)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return
	}
	resp, err := p.httpClient.Do(req)
	if err != nil {
		logger.Get().Debugw("llamaCppMetricsPoller: request failed",
			"backend", b.id, "url", url, "error", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		logger.Get().Debugw("llamaCppMetricsPoller: non-2xx status",
			"backend", b.id, "url", url, "status", resp.StatusCode)
		return
	}

	var data struct {
		Count           int    `json:"count"`
		MaxVRAMNCtx     int    `json:"max_vram_n_ctx"`
		ModelMaxContext int    `json:"model_max_context"`
		AvailableVRAMMB uint64 `json:"available_vram_mb"`
		TotalVRAMMB     uint64 `json:"total_vram_mb"`
		Models []struct {
			Name             string `json:"name"`
			Path             string `json:"path,omitempty"`
			State            string `json:"state,omitempty"`
			SizeBytes        int64  `json:"sizeBytes,omitempty"`
			LoadingSizeBytes int64  `json:"loadingSizeBytes,omitempty"` // Round 18: cppworker reports this for loaded models (sizeBytes=0)
			NLayers          int    `json:"nLayers,omitempty"`
			NHeads           int    `json:"nHeads,omitempty"`
			NKvHeads         int    `json:"nKvHeads,omitempty"`     // Round 18: для оценки KV cache
			HeadDimK         int    `json:"headDimK,omitempty"`      // Round 18: для оценки KV cache
			HeadDimV         int    `json:"headDimV,omitempty"`      // Round 18: для оценки KV cache
			NEmbd            int    `json:"nEmbd,omitempty"`
			NVocab           int    `json:"nVocab,omitempty"`
			ContextSize      int    `json:"contextSize,omitempty"`
			GGUFContextLength int   `json:"ggufContextLength,omitempty"` // Round 18: макс n_ctx для модели
			GPULayers        int    `json:"gpuLayers,omitempty"`
			ActiveQueries    int    `json:"activeQueries,omitempty"`
			TotalQueries     int    `json:"totalQueries,omitempty"`
			Architecture     string `json:"architecture,omitempty"`
			Quantization     string `json:"quantization,omitempty"`
			VRAMUsage        uint64 `json:"vramUsage,omitempty"`
			RAMUsage         uint64 `json:"ramUsage,omitempty"`
			LoadedAt         string `json:"loadedAt,omitempty"`
			// Round 34 (2026-08-12) Phase 2: runtime params (kvCacheType, flashAttnType,
			// useMmap) для profile mismatch detection в preflight_nctx.go.
			KvCacheType     string `json:"kvCacheType,omitempty"`
			FlashAttnType   int    `json:"flashAttnType,omitempty"`
			UseMmap         bool   `json:"useMmap,omitempty"`
			// Round 18 P0.1 (2026-08-03): capabilities (reasoning/vision/tools).
			// cppworker теперь возвращает готовый capabilities объект в /api/models.
			Capabilities      *types.ModelCapabilities `json:"capabilities,omitempty"`
			ReasoningEnabled  bool                     `json:"reasoningEnabled,omitempty"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		logger.Get().Debugw("llamaCppMetricsPoller: decode failed",
			"backend", b.id, "error", err)
		return
	}

	// Конвертируем в LlamaCppModel
	loadedModels := make([]types.LlamaCppModel, 0, len(data.Models))
	for _, m := range data.Models {
		// Показываем модели в любом состоянии (loaded/loading/error).
		// Фильтрация по "loaded" делается в WebUI при необходимости.
		// Но если state пустое — считаем loaded (cppworker не всегда его отдаёт).
		state := m.State
		if state == "" {
			state = "loaded"
		}
		// Round 18: cppworker reports sizeBytes=0 для загруженных моделей.
		// loadingSizeBytes содержит реальный file size — fallback на него.
		size := uint64(m.SizeBytes)
		if size == 0 {
			size = uint64(m.LoadingSizeBytes)
		}
		// Round 18b (2026-07-10): cppworker reports quantization="" (баг).
		// Парсим из имени файла (path) если cppworker не вернул.
		quant := m.Quantization
		if quant == "" {
			quant = extractQuantizationFromName(m.Path)
			if quant == "" {
				// Fallback: name без .gguf
				quant = extractQuantizationFromName(m.Name + ".gguf")
			}
		}
		loadedModels = append(loadedModels, types.LlamaCppModel{
			Name:          m.Name,
			Path:          m.Path,
			Size:          size,
			VRAMUsage:     m.VRAMUsage,
			RAMUsage:      m.RAMUsage,
			ContextLength: m.ContextSize,
			BatchSize:     0, // cppworker reports batchSize too, см. ниже в более широкой структуре
			NumGPULayers:  m.GPULayers,
			Quantization:  quant,
			State:         state,
			// Round 18: architecture metadata — frontend использует для оценки
			// VRAM/RAM split по слоям (cppworker не сообщает actual per-model usage).
			Architecture: m.Architecture,
			NLayers:      m.NLayers,
			NKvHeads:     m.NKvHeads,
			NEmbd:        m.NEmbd,
			HeadDimK:     m.HeadDimK,
			HeadDimV:     m.HeadDimV,
			MaxContext:   m.GGUFContextLength,
			LoadedAt:     m.LoadedAt,
			// Round 34 (2026-08-12) Phase 2: runtime params из /api/models.
			// Используются preflight_nctx.go для paramsMatch() — если клиент
			// запрашивает другой kv_cache_type/flash_attn/use_mmap, чем
			// текущая загруженная модель, preflight trigger'ит reload.
			KvCacheType:   m.KvCacheType,
			FlashAttnType: m.FlashAttnType,
			UseMmap:       m.UseMmap,
			// Round 18 P0.1 (2026-08-03): capabilities. Если cppworker не вернул
			// (старая версия), вычисляем по имени как fallback.
			Capabilities: capabilitiesOrFallback(m.Capabilities, m.Name, m.Architecture, m.GGUFContextLength, m.ReasoningEnabled),
		})
	}

	// Обновляем кэш
	p.proxy.metricsMgr.mu.Lock()
	lm, ok := p.proxy.metricsMgr.llamaMetrics[b.id]
	if !ok {
		lm = &types.LlamaCppMetrics{}
		p.proxy.metricsMgr.llamaMetrics[b.id] = lm
	}
	lm.LoadedModels = loadedModels
	lm.MaxVRAMNCtx = data.MaxVRAMNCtx
	lm.ModelMaxContext = data.ModelMaxContext
	lm.AvailableVRAMMB = data.AvailableVRAMMB
	lm.TotalVRAMMB = data.TotalVRAMMB
	p.proxy.metricsMgr.mu.Unlock()
	logger.Get().Infow("llamaCppMetricsPoller: updated llama.cpp metrics",
		"backend", b.id, "url", url, "loaded_models", len(loadedModels))

	// Опрос loading-прогресса (если есть активные загрузки — обновим
	// LoadingModels и переключим poller на быстрый режим).
	p.pollLoadingProgress(b)
}

// pollLoadingProgress опрашивает /api/models/load/progress у бэкенда и
// обновляет LoadingModels в кэше. Если loading-моделей нет — не трогает
// кэш (оставляет предыдущее значение до следующего успешного опроса).
//
// Также обновляет lastLoadingSeen, чтобы loop() переключился на fastInterval.
func (p *llamaCppMetricsPoller) pollLoadingProgress(b backendInfo) {
	url := fmt.Sprintf("http://%s:%d/api/models/load/progress", b.host, b.port)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return
	}
	resp, err := p.httpClient.Do(req)
	if err != nil {
		logger.Get().Debugw("llamaCppMetricsPoller: loading-progress request failed",
			"backend", b.id, "url", url, "error", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// 404 — endpoint может быть не реализован в старой версии cppworker.
		// Тихо игнорируем.
		return
	}

	var data struct {
		Count  int `json:"count"`
		Models []struct {
			Name             string `json:"name"`
			State            string `json:"state"`
			LoadingStartedAt string `json:"loadingStartedAt"`
			LoadingSizeBytes int64  `json:"loadingSizeBytes"`
			ElapsedMs        int64  `json:"elapsedMs"`
			Error            string `json:"error"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		logger.Get().Debugw("llamaCppMetricsPoller: loading-progress decode failed",
			"backend", b.id, "error", err)
		return
	}

	loading := make([]types.LlamaCppModel, 0, len(data.Models))
	for _, m := range data.Models {
		state := m.State
		if state == "" {
			state = "loading"
		}
		loading = append(loading, types.LlamaCppModel{
			Name:             m.Name,
			State:            state,
			Size:             uint64(m.LoadingSizeBytes),
			LoadingStartedAt: &m.LoadingStartedAt,
			LoadingSizeBytes: m.LoadingSizeBytes,
			LoadingError:     m.Error,
		})
	}

	p.proxy.metricsMgr.mu.Lock()
	lm, ok := p.proxy.metricsMgr.llamaMetrics[b.id]
	if !ok {
		lm = &types.LlamaCppMetrics{}
		p.proxy.metricsMgr.llamaMetrics[b.id] = lm
	}
	lm.LoadingModels = loading
	p.proxy.metricsMgr.mu.Unlock()

	if len(loading) > 0 {
		// Переключаем poller на быстрый режим (если ещё не там).
		p.mu.Lock()
		p.lastLoadingSeen = time.Now()
		p.mu.Unlock()
		logger.Get().Debugw("llamaCppMetricsPoller: loading models observed",
			"backend", b.id, "count", len(loading))
	}
}

// capabilitiesOrFallback — если cppworker вернул capabilities, используем их.
// Иначе вычисляем по имени модели (fallback для старых cppworker).
//
// Round 18 P0.1 (2026-08-03).
func capabilitiesOrFallback(cppCaps *types.ModelCapabilities, name, architecture string, maxContext int, reasoningEnabled bool) *types.ModelCapabilities {
	if cppCaps != nil {
		// cppworker вернул — приоритет.
		return cppCaps
	}
	caps := types.CapabilitiesFromModelInfo(name, reasoningEnabled, architecture, maxContext, maxContext)
	return &caps
}

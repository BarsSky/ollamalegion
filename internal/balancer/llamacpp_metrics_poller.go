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

	// Round 45 (2026-08-19): JSON tags changed from camelCase to snake_case
	// to match cppworker's actual /api/models response. Previous tags caused
	// ALL per-model fields to silently decode to zero values (ContextSize, SizeBytes,
	// NLayers, NEmbd, etc.) because cppworker returns snake_case keys. This broke
	// preflightNCtxReloadIfNeeded which reads LoadedModels[].ContextLength from
	// this cache to decide whether to trigger an async reload. With the bug, the
	// cache always reported loaded_n_ctx=0, causing every /api/chat to trigger
	// a spurious 503 + reload loop.
	//
	// Note: each tag below is "snake,camel" — but Go's encoding/json only
	// honors the FIRST name, so this is effectively snake_case only. The
	// second name is documentation, not a real fallback. If a future cppworker
	// reverts to camelCase, the right fix is a custom UnmarshalJSON, not
	// relying on the comma-list (which doesn't work as expected).
	var data struct {
		Count       int `json:"count"`
		MaxVRAMNCtx int `json:"max_vram_n_ctx"`
		// R83 §9.2 (2026-09-26): cppworker сообщает, известна ли ему VRAM.
		// Вместе с max_vram_n_ctx==0 это означает «веса не влезают», а не
		// «метрик нет» — см. DecidePreflight.
		VramKnown       bool `json:"vram_known"`
		ModelMaxContext int  `json:"model_max_context"`
		// Round 37 (2026-08-18): feasible + GGUF top-level fields from cppworker.
		// cppworker exposes them since Round 37 in /api/models.
		FeasibleMaxContext int    `json:"feasible_max_context"`
		GGUFMaxContext     int    `json:"gguf_max_context"`
		AvailableVRAMMB    uint64 `json:"available_vram_mb"`
		TotalVRAMMB        uint64 `json:"total_vram_mb"`
		Models             []struct {
			Name             string `json:"name"`
			Path             string `json:"path,omitempty"`
			State            string `json:"state,omitempty"`
			SizeBytes        int64  `json:"size_bytes,sizeBytes,omitempty"`
			LoadingSizeBytes int64  `json:"loading_size_bytes,loadingSizeBytes,omitempty"`
			NLayers          int    `json:"n_layers,nLayers,omitempty"`
			NHeads           int    `json:"n_heads,nHeads,omitempty"`
			NKvHeads         int    `json:"n_kv_heads,nKvHeads,omitempty"`
			HeadDimK         int    `json:"head_dim_k,headDimK,omitempty"`
			HeadDimV         int    `json:"head_dim_v,headDimV,omitempty"`
			NEmbd            int    `json:"n_embd,nEmbd,omitempty"`
			NVocab           int    `json:"n_vocab,nVocab,omitempty"`
			// R45: ContextSize was the smoking gun. cppworker returns
			// "context_size" but the previous tag was "contextSize", so
			// LoadedModels[].ContextLength was always 0, breaking
			// preflightNCtxReloadIfNeeded's loaded >= requested check.
			ContextSize int `json:"context_size,contextSize,omitempty"`
			// R83 (2026-09-29): слоты модели. Без них балансер держал
			// вместимость 1 и сериализовал запросы, даже когда модель загружена
			// с parallel=2/3 (проверено: два одновременных запроса напрямую в
			// cppworker при parallel=2 — оба 200).
			Parallel int `json:"parallel,omitempty"`
			MaxSlots int `json:"max_slots,maxSlots,omitempty"`
			// R83-fix (2026-09-30): окно одного слота (n_ctx_seq у llama.cpp).
			// Суммарный ContextSize выше слоты делят между собой, поэтому
			// ограничивать клиентский запрос надо именно этим значением.
			ContextPerSeq     int    `json:"context_per_seq,contextPerSeq,omitempty"`
			BatchSize         int    `json:"batch_size,batchSize,omitempty"`
			GGUFContextLength int    `json:"gguf_context_length,ggufContextLength,omitempty"`
			GPULayers         int    `json:"gpu_layers,gpuLayers,omitempty"`
			ActiveQueries     int    `json:"active_queries,activeQueries,omitempty"`
			TotalQueries      int    `json:"total_queries,totalQueries,omitempty"`
			Architecture      string `json:"architecture,omitempty"`
			Quantization      string `json:"quantization,omitempty"`
			VRAMUsage         uint64 `json:"vram_usage,vramUsage,omitempty"`
			SizeVRAM          uint64 `json:"size_vram,sizeVram,omitempty"`
			RAMUsage          uint64 `json:"ram_usage,ramUsage,omitempty"`
			LoadedAt          string `json:"loaded_at,loadedAt,omitempty"`
			// Round 34 (2026-08-12) Phase 2: runtime params (kvCacheType, flashAttnType,
			// useMmap) для profile mismatch detection в preflight_nctx.go.
			KvCacheType   string `json:"kv_cache_type,kvCacheType,omitempty"`
			FlashAttnType int    `json:"flash_attn_type,flashAttnType,omitempty"`
			UseMmap       bool   `json:"use_mmap,useMmap,omitempty"`
			// R45 (2026-08-19): per-model feasible + GGUF max context for 3-tier
			// resolution in preflight_helper.go. cppworker has reported these
			// per-model since Round 37 (2026-08-18). The poller used to copy
			// them out of the per-model struct but the struct itself didn't have
			// the fields — they were silently 0. Top-level fallback (line 303)
			// masked the bug, but per-model priority is the whole point of the
			// Round 37 work, so this completes the wire.
			FeasibleMaxContext int `json:"feasible_max_context,omitempty"`
			GGUFMaxContext     int `json:"gguf_max_context,omitempty"`
			// R83 §9.2 (2026-09-26): per-model vram_known (cppworker отдаёт с R83).
			// У загруженной модели он true → preflight может отличить «веса не
			// влезают» от «метрик нет».
			VramKnown bool `json:"vram_known,omitempty"`
			// Round 18 P0.1 (2026-08-03): capabilities (reasoning/vision/tools).
			// cppworker теперь возвращает готовый capabilities объект в /api/models.
			Capabilities     *types.ModelCapabilities `json:"capabilities,omitempty"`
			ReasoningEnabled bool                     `json:"reasoning_enabled,reasoningEnabled,omitempty"`
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
		// R66.5: prefer cppworker's size_vram (bytes), fall back to vram_usage (MB legacy).
		// LlamaCppModel.VRAMUsage is in MB, so convert size_vram bytes → MB.
		vram := m.VRAMUsage
		if m.SizeVRAM > 0 {
			vram = m.SizeVRAM / (1024 * 1024)
		}
		loadedModels = append(loadedModels, types.LlamaCppModel{
			Name:          m.Name,
			Path:          m.Path,
			Size:          size,
			VRAMUsage:     vram,
			RAMUsage:      m.RAMUsage,
			ContextLength: m.ContextSize,
			Parallel:      m.Parallel, // R83: слоты модели (для синхронизации вместимости)
			MaxSlots:      m.MaxSlots, // R83: сколько слотов завёл cppworker (>=1)
			// R83-fix: окно ОДНОГО слота — по нему ограничиваем запрос клиента,
			// а не по суммарному ContextLength.
			ContextPerSeq: m.ContextPerSeq,
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
			// Round 34 (2026-08-12) Phase 2 + Round 53.2 (2026-08-24):
			// runtime params из /api/models. Pre-R53.2 использовались
			// preflight_nctx.go::paramsMatch() для trigger reload при флаговом
			// mismatch. R53.2 убрал флаговую логику — теперь ТОЛЬКО по n_ctx.
			// Поля остаются в state для отображения в WebUI (/api/v1/backends.loadedModels)
			// и для совместимости с parser (RequestedKvCacheType и т.д. парсятся,
			// но не влияют на reload decision).
			KvCacheType:   m.KvCacheType,
			FlashAttnType: m.FlashAttnType,
			UseMmap:       m.UseMmap,
			// R45 (2026-08-19): wire per-model feasible + GGUF max into the
			// LoadedModels entry so preflight_helper.collectPreflightState can
			// prefer per-model values over top-level (see preflight_helper.go
			// Round 37 per-model block). Without this, the struct held them
			// but the loop never copied them out.
			FeasibleMaxContext: m.FeasibleMaxContext,
			GGUFMaxContext:     m.GGUFMaxContext,
			VramKnown:          m.VramKnown,
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
	lm.VramKnown = data.VramKnown
	lm.ModelMaxContext = data.ModelMaxContext
	// Round 37 (2026-08-18): per-model feasible/GGUF для 3-tier resolution.
	// Per-model: prefer per-model feasible (если загружена хоть одна модель).
	// Top-level: fallback.
	if len(loadedModels) > 0 {
		for _, m := range loadedModels {
			if m.FeasibleMaxContext > lm.MaxFeasibleContext {
				lm.MaxFeasibleContext = m.FeasibleMaxContext
			}
			if m.GGUFMaxContext > lm.GGUFMaxContext {
				lm.GGUFMaxContext = m.GGUFMaxContext
			}
		}
	} else {
		lm.MaxFeasibleContext = data.FeasibleMaxContext
		lm.GGUFMaxContext = data.GGUFMaxContext
	}
	lm.AvailableVRAMMB = data.AvailableVRAMMB
	lm.TotalVRAMMB = data.TotalVRAMMB
	p.proxy.metricsMgr.mu.Unlock()

	// R83 (2026-09-29): согласуем вместимость бэкенда с числом слотов модели.
	//
	// Балансер держал MaxConcurrentReqs=1 и СЕРИАЛИЗОВАЛ запросы, хотя модель
	// может обслуживать несколько одновременно (parallel=2/3). Оператору приходилось
	// вручную выставлять вместимость, и при перезагрузке модели её значение
	// расходилось с реальностью. Теперь вместимость подтягивается из факта:
	// сколько слотов завёл cppworker (max_slots), столько запросов балансер и
	// пропускает параллельно; остальные ждут в очереди, как и раньше.
	p.adoptCapacityFromLoadedModels(b.id, loadedModels)

	logger.Get().Infow("llamaCppMetricsPoller: updated llama.cpp metrics",
		"backend", b.id, "url", url, "loaded_models", len(loadedModels))

	// Опрос loading-прогресса (если есть активные загрузки — обновим
	// LoadingModels и переключим poller на быстрый режим).
	p.pollLoadingProgress(b)
}

// adoptCapacityFromLoadedModels — R83 (2026-09-29): привести вместимость бэкенда
// в соответствие с числом слотов загруженной модели.
//
// ПОЧЕМУ. cppworker сообщает max_slots (сколько параллельных сессий заведено при
// загрузке: parallel=1 → 1, parallel=3 → 3). Балансер это значение не читал,
// поэтому всегда работал с MaxConcurrentReqs=1 и сериализовал запросы даже к
// модели с тремя слотами. Проверено напрямую: два одновременных запроса к
// cppworker с parallel=2 — оба 200, то есть модель реально готова к параллелизму.
//
// ПРАВИЛА (чтобы не сломать уже настроенные стенды):
//   - берём МАКСИМУМ по загруженным моделям: вместимость бэкенда — это его
//     способность, а не параметр одной модели;
//   - уважаем оператора: если RuntimeMaxConcurrentRequests задан явно (PUT /limits
//     или AGENT_MAX_CONCURRENT_REQUESTS), значение оператора не трогаем;
//   - меняем только когда есть что менять (иначе poller каждые 30 c писал бы
//     состояние и вызывал autosave).
func (p *llamaCppMetricsPoller) adoptCapacityFromLoadedModels(backendID string, models []types.LlamaCppModel) {
	if p == nil || p.proxy == nil || backendID == "" || len(models) == 0 {
		return
	}

	slots := 0
	for _, m := range models {
		if m.State != "loaded" {
			continue
		}
		n := m.MaxSlots
		if n <= 0 {
			n = m.Parallel
		}
		if n > slots {
			slots = n
		}
	}
	if slots <= 0 {
		// Слотов нет: либо старая сборка cppworker (не отдаёт max_slots/parallel),
		// либо модель не в состоянии loaded. Поведение прежнее.
		if logger.Get() != nil {
			logger.Get().Debugw("llamaCppMetricsPoller: слоты модели не распознаны, вместимость прежняя",
				"backend", backendID, "loaded_models", len(models),
				"enabled", types.CapacityFromModelSlotsEnabled())
		}
		return
	}

	state := p.proxy.GetBackend(backendID)
	if state == nil {
		return
	}
	if state.RuntimeModelSlots == slots {
		return
	}

	old := state.RuntimeModelSlots
	p.proxy.SetBackendModelSlots(backendID, slots)
	logger.Get().Infow("llamaCppMetricsPoller: вместимость бэкенда приведена к числу слотов модели",
		"backend", backendID, "old_model_slots", old, "new_model_slots", slots,
		"note", "значение живёт в RuntimeModelSlots: MaxConcurrentReqs переписывается перерегистрацией агента")
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

	// R65d (2026-09-20) — ИСПРАВЛЕНИЕ ИМЁН ПОЛЕЙ.
	//
	// Было: snake_case-теги (loading_started_at / loading_size_bytes / elapsed_ms)
	// с комментарием «are all snake_case in cppworker». Это НЕВЕРНО: cppworker
	// отдаёт эти поля в camelCase — см. handlers_model.go:838-845:
	//   "loadingStartedAt", "loadingSizeBytes", "elapsedMs"
	// Из-за расхождения ВСЕ три поля декодировались в нули, поэтому:
	//   - balancer не видел реального размера/времени загрузки;
	//   - lastLoadingSeen не обновлялся → поллер не переключался на fastInterval
	//     (2s) и прогресс в мониторе не обновлялся в реальном времени.
	//
	// WebUI-страница GGUF спасалась тем, что ходит напрямую через
	// /api/v1/gguf/backends/{id}/proxy/... и читает camelCase (см.
	// webui/js/modules/gguf-load-progress.js:94-96).
	//
	// Go не поддерживает несколько имён в одном теге, поэтому указываем
	// camelCase (фактический формат) и дополнительно пробуем snake_case через
	// custom fallback ниже — на случай будущего перехода cppworker.
	var data struct {
		Count  int `json:"count"`
		Models []struct {
			Name             string `json:"name"`
			State            string `json:"state"`
			LoadingStartedAt string `json:"loadingStartedAt"`
			LoadingSizeBytes int64  `json:"loadingSizeBytes"`
			ElapsedMs        int64  `json:"elapsedMs"`
			Error            string `json:"error"`

			// snake_case-алиасы (старые версии / другой сериализатор).
			LoadingStartedAtSnake string `json:"loading_started_at"`
			LoadingSizeBytesSnake int64  `json:"loading_size_bytes"`
			ElapsedMsSnake        int64  `json:"elapsed_ms"`
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
		// Приоритет camelCase (фактический формат), fallback на snake_case.
		startedAt := m.LoadingStartedAt
		if startedAt == "" {
			startedAt = m.LoadingStartedAtSnake
		}
		sizeBytes := m.LoadingSizeBytes
		if sizeBytes == 0 {
			sizeBytes = m.LoadingSizeBytesSnake
		}
		loading = append(loading, types.LlamaCppModel{
			Name:             m.Name,
			State:            state,
			Size:             uint64(sizeBytes),
			LoadingStartedAt: &startedAt,
			LoadingSizeBytes: sizeBytes,
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

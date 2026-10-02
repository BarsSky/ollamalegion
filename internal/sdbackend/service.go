package sdbackend

import (
	"context"
	"fmt"
	"os"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// ============================================================
// Service — фасад воркера
// ============================================================
//
// Собирает воедино: реестр моделей → супервизор sd-server → очередь/джобы →
// хранилище картинок → idle-unload. HTTP-слой (cmd/sdworker) знает только этот
// тип и не трогает внутренности.
//
// ЗАЧЕМ ФАСАД: (1) тесты поднимают сервис на мок-движке одной строкой;
// (2) cmd/sdworker остаётся тонким (main + router), как у cppworker.

// Service — image-воркер целиком.
type Service struct {
	Config   *Config
	Registry *Registry
	Sup      *Supervisor
	Metrics  *Metrics
	Queue    *Queue
	Store    *ImageStore
	Runner   *JobRunner
	Idle     *IdleUnloadManager

	// HF — HF-загрузка image-bundle'ов (Phase 4). Может быть nil, если сервис
	// собран вручную (тесты) без NewService: хендлеры обязаны это переживать
	// (503 «HF downloader not available»), а не паниковать.
	HF *HFManager

	// StartedAt — время старта сервиса (uptime в /health).
	StartedAt time.Time
}

// NewService — сборка сервиса без побочных эффектов (никаких spawn).
func NewService(cfg *Config) (*Service, error) {
	if cfg == nil {
		return nil, fmt.Errorf("nil config")
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.ModelsDir, 0o755); err != nil {
		return nil, fmt.Errorf("create models dir %s: %w", cfg.ModelsDir, err)
	}
	registry := NewRegistry(cfg.ModelsDir)
	if err := registry.Load(); err != nil {
		return nil, err
	}
	for _, w := range registry.Warnings() {
		sdLog().Warnw("image models registry warning", "detail", w)
	}
	metrics := NewMetrics()
	store, err := NewImageStore(cfg.ImagesDir, cfg.BaseURL)
	if err != nil {
		return nil, err
	}
	sup := NewSupervisor(cfg, registry, metrics)
	runner := NewJobRunner(cfg, registry, sup, metrics, store)

	// HF-загрузчик собираем сразу: он создаёт каталоги темп/моделей, поэтому
	// ошибка прав на запись видна на старте, а не при первом «Скачать».
	hf, err := NewHFManager(cfg, registry)
	if err != nil {
		return nil, fmt.Errorf("init HF downloader: %w", err)
	}
	// Подчищаем каталоги bundle'ов, чью регистрацию снял cleanup (удалённый
	// файл): они невидимы реестру, но занимают диск.
	if swept := hf.SweepUnregisteredBundles(); swept > 0 {
		sdLog().Infow("swept unregistered image bundle dirs at startup", "count", swept)
	}

	return &Service{
		Config:    cfg,
		Registry:  registry,
		Sup:       sup,
		Metrics:   metrics,
		Queue:     runner.Queue(),
		Store:     store,
		Runner:    runner,
		Idle:      NewIdleUnloadManager(cfg, registry, sup, metrics),
		HF:        hf,
		StartedAt: time.Now(),
	}, nil
}

// Downloader — HF-обёртка воркера (nil, если сервис собран без NewService).
func (s *Service) Downloader() *HFManager { return s.HF }

// ReloadRegistry — перечитывает каталог моделей (новые bundle'ы становятся
// видны в GET /api/image/models без рестарта воркера).
//
// Отдельный публичный метод (а не приватный вызов из HFManager): Phase 2
// синкает профили с балансера в каталог воркера — после раскладки файлов
// требуется тот же reload.
func (s *Service) ReloadRegistry() error {
	if s.Registry == nil {
		return fmt.Errorf("registry is not initialized")
	}
	return s.Registry.Load()
}

// Start запускает фоновые сервисы воркера (idle-unload, опциональный preload).
func (s *Service) Start(ctx context.Context) {
	s.Idle.Start()
	if s.Config.PreloadModel != "" {
		model := s.Config.PreloadModel
		go func() {
			sdLog().Infow("preloading image model at startup", "model", model)
			loadCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
			defer cancel()
			if _, err := s.Sup.Load(loadCtx, model, LoadOptions{}); err != nil {
				sdLog().Errorw("preload failed (worker keeps running, model will load on demand)",
					"model", model, "error", err)
			}
		}()
	}
}

// Shutdown — корректная остановка: гасим sd-server, чтобы не оставить
// процесс-сироту с занятой VRAM (особенно важно при рестарте контейнера).
func (s *Service) Shutdown(ctx context.Context) {
	s.Idle.Stop()
	// Незавершённые HF-загрузки отменяем: иначе горутина pull'а переживёт
	// HTTP-сервер и оставит .download-файлы (их подхватит resume при следующем
	// запуске — но процесс не должен «висеть» до конца 6-GB файла).
	if s.HF != nil {
		s.HF.Close()
	}
	if s.Sup == nil {
		return
	}
	if s.Sup.State() == StateLoaded || s.Sup.PID() != 0 {
		sdLog().Infow("shutting down: killing sd-server subprocess")
		if err := s.Sup.Unload(ctx); err != nil {
			sdLog().Warnw("shutdown unload failed", "error", err)
		}
	}
}

// ============================================================
// Ответы API
// ============================================================

// CapabilitiesResponse — GET /api/image/capabilities.
//
// Агрегирует: (1) capabilities движка как есть (его API молод и расширяется —
// терять незнакомые поля нельзя); (2) список локальных моделей; (3) НАШИ
// лимиты (они важнее движковых для клиента: после нормализации размер всегда
// кратен 64, batch ≤ 8, очередь — наша).
type CapabilitiesResponse struct {
	Ready        bool                   `json:"ready"`
	State        string                 `json:"state"`
	Model        string                 `json:"model,omitempty"`
	PID          int                    `json:"pid,omitempty"`
	Engine       map[string]any         `json:"engine,omitempty"`
	Models       []ModelInfo            `json:"models"`
	Limits       LimitsInfo             `json:"limits"`
	PinnedRevision string               `json:"pinned_sd_server_revision"`
	VRAM         *VRAMInfo              `json:"vram,omitempty"`
	Warnings     []string               `json:"warnings,omitempty"`
}

// LimitsInfo — лимиты, которые применяет ВОРКЕР (а не движок).
type LimitsInfo struct {
	MinWidth        int `json:"min_width"`
	MaxWidth        int `json:"max_width"`
	MinHeight       int `json:"min_height"`
	MaxHeight       int `json:"max_height"`
	SizeMultiple    int `json:"size_multiple"`
	MaxBatchCount   int `json:"max_batch_count"`
	MinSteps        int `json:"min_steps"`
	MaxSteps        int `json:"max_steps"`
	MaxCFGScale     float64 `json:"max_cfg_scale"`
	QueueSize       int `json:"queue_size"`
	QueueInFlight   int `json:"queue_in_flight"`
	QueueWaiting    int `json:"queue_waiting"`
	CompletedTTLSec int `json:"completed_job_ttl_seconds"`
	GenerationTimeoutSec int `json:"generation_timeout_seconds"`
	SeedAlwaysPositive bool `json:"seed_always_positive"`
	SupportsResponseFormatURL bool `json:"supports_response_format_url"`
	CancelQueuedOnly bool `json:"cancel_queued_only"`
}

// BuildLimits — лимиты воркера с учётом capabilities движка (если есть).
func (s *Service) BuildLimits() LimitsInfo {
	caps := s.Sup.Capabilities()
	lim := LimitsInfo{
		MinWidth: MinImageSide, MaxWidth: MaxImageSide,
		MinHeight: MinImageSide, MaxHeight: MaxImageSide,
		SizeMultiple: SizeMultiple,
		MaxBatchCount: MaxBatchCount, MinSteps: MinSteps, MaxSteps: MaxSteps,
		MaxCFGScale: maxCFGScale,
		QueueSize: s.Queue.Capacity(), QueueInFlight: s.Queue.InFlight(),
		QueueWaiting: s.Queue.Rejected(),
		CompletedTTLSec: int(jobTTL.Seconds()),
		GenerationTimeoutSec: s.Config.GenerationTimeoutSec,
		SeedAlwaysPositive: true,
		SupportsResponseFormatURL: s.Store != nil,
		// НЕ обещаем mid-flight cancel: движок его не умеет (409).
		CancelQueuedOnly: true,
	}
	if caps != nil && caps.Limits.MaxWidth > 0 {
		// Показываем РЕАЛЬНЫЕ границы движка, но не шире наших (кратность 64).
		lim.MinWidth = maxInt(lim.MinWidth, caps.Limits.MinWidth)
		lim.MinHeight = maxInt(lim.MinHeight, caps.Limits.MinHeight)
		lim.MaxWidth = minInt(lim.MaxWidth, caps.Limits.MaxWidth)
		lim.MaxHeight = minInt(lim.MaxHeight, caps.Limits.MaxHeight)
		if caps.Limits.MaxBatchCount > 0 {
			lim.MaxBatchCount = minInt(lim.MaxBatchCount, caps.Limits.MaxBatchCount)
		}
	}
	return lim
}

// Capabilities — агрегированный ответ (движок + модели + лимиты).
func (s *Service) Capabilities() CapabilitiesResponse {
	caps := s.Sup.Capabilities()
	resp := CapabilitiesResponse{
		Ready:          s.Sup.State() == StateLoaded,
		State:          s.Sup.State(),
		Model:          s.Sup.CurrentModel(),
		PID:            s.Sup.PID(),
		Models:         s.Models(),
		Limits:         s.BuildLimits(),
		PinnedRevision: types.PinnedSDServerRevision,
		Warnings:       s.Registry.Warnings(),
	}
	if caps != nil {
		if raw := caps.Raw(); raw != nil {
			resp.Engine = raw
		}
	}
	if v := QueryVRAM(context.Background()); v.Available {
		resp.VRAM = &v
	}
	return resp
}

// Models — список моделей с состояниями (контракт балансера: snake_case).
func (s *Service) Models() []ModelInfo {
	names := s.Registry.Names()
	out := make([]ModelInfo, 0, len(names))
	state := s.Sup.State()
	current := s.Sup.CurrentModel()
	active := s.Sup.InFlight()
	var lastErr string
	var loadedAt, lastUsed time.Time
	if s.Metrics != nil {
		lastErr = s.Metrics.LastError()
		loadedAt = s.Metrics.LoadedAt()
		lastUsed = s.Metrics.LastUsed()
	}
	for _, name := range names {
		p, _ := s.Registry.Profile(name)
		st := StateNotLoaded
		if name == current {
			st = state
		} else if state == StateError && name == current {
			st = StateError
		}
		errText := ""
		if name == current {
			errText = lastErr
		}
		la, lu := time.Time{}, time.Time{}
		var act int64
		if name == current {
			la, lu, act = loadedAt, lastUsed, active
		}
		out = append(out, s.Registry.Info(p, st, errText, la, lu, act))
	}
	return out
}

// FindModel — ModelInfo по имени.
func (s *Service) FindModel(name string) (ModelInfo, bool) {
	for _, m := range s.Models() {
		if m.Name == name {
			return m, true
		}
	}
	return ModelInfo{}, false
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

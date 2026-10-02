// handlers_model.go — жизненный цикл image-моделей (load/unload/reload/progress).
//
// КОНТРАКТ (совместим с cppworker, чтобы балансер и WebUI читали одинаково):
//
//	GET  /api/image/capabilities            → движок + модели + лимиты
//	GET  /api/image/models                  → список моделей со state (snake_case)
//	POST /api/image/models/load   {name}    → 202 + снимок прогресса
//	POST /api/image/models/unload           → 200 (kill субпроцесса)
//	POST /api/image/models/reload {name}    → 202 (kill + spawn)
//	GET  /api/image/models/load/progress    → снимок прогресса
//	GET  /api/image/models/load/progress/stream → SSE
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"ollama-loadbalancer/internal/sdbackend"
)

// ============================================================
// Прогресс загрузки
// ============================================================
//
// ЗАЧЕМ СВОЙ ТРЕКЕР, ЕСЛИ ЕСТЬ СОСТОЯНИЕ СУПЕРВИЗОРА: load = spawn процесса +
// загрузка 2–12 GB весов, это десятки секунд. Клиенту (WebUI, оператор) нужен
// не только state, но и «сколько уже ждём» + история шагов, иначе интерфейс
// выглядит зависшим. Плюс снимок нужен ТОЛЬКО на время загрузки — держим его
// маленьким и с явным TTL.

// loadProgress — снимок прогресса.
type loadProgress struct {
	State     string    `json:"state"`
	Model     string    `json:"model,omitempty"`
	Stage     string    `json:"stage,omitempty"`
	ElapsedMS int64     `json:"elapsed_ms"`
	Error     string    `json:"error,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
	Events    []string  `json:"events,omitempty"`
}

// progressTracker хранит последний прогресс загрузки.
type progressTracker struct {
	mu   sync.Mutex
	cur  loadProgress
	subs map[chan loadProgress]struct{}
}

func newProgressTracker() *progressTracker {
	return &progressTracker{subs: map[chan loadProgress]struct{}{}}
}

func (p *progressTracker) lock()   { p.mu.Lock() }
func (p *progressTracker) unlock() { p.mu.Unlock() }

// set — обновление снимка и рассылка подписчикам (SSE).
func (p *progressTracker) set(stage, state, model, errText string) {
	p.lock()
	p.cur.State = state
	p.cur.Model = model
	p.cur.Stage = stage
	p.cur.Error = errText
	p.cur.UpdatedAt = time.Now().UTC()
	if stage != "" {
		line := stage
		if errText != "" {
			line += ": " + errText
		}
		p.cur.Events = append(p.cur.Events, fmt.Sprintf("%s %s", time.Now().UTC().Format("15:04:05"), line))
		if len(p.cur.Events) > 20 {
			p.cur.Events = p.cur.Events[len(p.cur.Events)-20:]
		}
	}
	snap := p.cur
	subs := make([]chan loadProgress, 0, len(p.subs))
	for ch := range p.subs {
		subs = append(subs, ch)
	}
	p.unlock()
	for _, ch := range subs {
		select {
		case ch <- snap:
		default: // подписчик не успевает — не блокируем загрузку
		}
	}
}

// snapshot — текущий снимок.
func (p *progressTracker) snapshot() loadProgress {
	p.lock()
	defer p.unlock()
	return p.cur
}

// subscribe/unsubscribe — SSE-подписка.
func (p *progressTracker) subscribe() chan loadProgress {
	ch := make(chan loadProgress, 4)
	p.lock()
	p.subs[ch] = struct{}{}
	p.unlock()
	return ch
}

func (p *progressTracker) unsubscribe(ch chan loadProgress) {
	p.lock()
	delete(p.subs, ch)
	p.unlock()
	close(ch)
}

// ============================================================
// Хендлеры
// ============================================================

// handleCapabilities — GET /api/image/capabilities.
func (a *App) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	caps := a.svc.Capabilities()
	// 200 даже когда модель не загружена: это ДЕСКРИПТОР возможностей, а не
	// health-чек. Поле ready показывает, можно ли генерировать прямо сейчас.
	writeJSON(w, http.StatusOK, caps)
}

// handleListModels — GET /api/image/models.
func (a *App) handleListModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"models":           a.svc.Models(),
		"state":            a.svc.Sup.State(),
		"current_model":    a.svc.Sup.CurrentModel(),
		"warnings":         a.svc.Registry.Warnings(),
		"pinned_revision":  sdbackend.PinnedRevision(),
	})
}

// loadRequest — тело POST /api/image/models/load.
type loadRequest struct {
	Name string `json:"name"`
	// TimeoutSec — переопределение таймаута readiness (оператор знает, что
	// 12-GB FLUX на медленном диске грузится дольше дефолта).
	TimeoutSec int `json:"timeoutSec,omitempty"`
	// Wait=true — блокирующий вызов (диагностика/скрипты); по умолчанию 202.
	Wait bool `json:"wait,omitempty"`
}

// handleLoadModel — POST /api/image/models/load.
//
// АСИНХРОННО по умолчанию (202): загрузка 2–12 GB — десятки секунд, а
// HTTP-клиент балансера/WebUI не должен держать соединение. Прогресс —
// /api/image/models/load/progress (и /stream).
func (a *App) handleLoadModel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var req loadRequest
	if body := strings.TrimSpace(r.Header.Get("Content-Type")); strings.Contains(body, "application/json") {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
			return
		}
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		// Пустое имя = «загрузи модель по умолчанию» (единственную в реестре
		// либо предзагруженную конфигом). Это удобно healthcheck-скриптам.
		if a.svc.Config.PreloadModel != "" {
			name = a.svc.Config.PreloadModel
		} else if names := a.svc.Registry.Names(); len(names) == 1 {
			name = names[0]
		}
	}
	if name == "" {
		writeError(w, http.StatusBadRequest, "name is required (known models: "+strings.Join(a.svc.Registry.Names(), ", ")+")")
		return
	}
	if _, ok := a.svc.Registry.Profile(name); !ok {
		writeError(w, http.StatusNotFound, fmt.Sprintf("%s: %q", sdbackend.ErrModelNotFound, name))
		return
	}
	// Уже загружена и это не force-reload → идемпотентный 200.
	if a.svc.Sup.State() == sdbackend.StateLoaded && a.svc.Sup.CurrentModel() == name {
		writeJSON(w, http.StatusOK, map[string]any{
			"status": "already_loaded", "model": name, "progress": a.progress.snapshot(),
		})
		return
	}
	if a.progress.snapshot().State == sdbackend.StateLoading {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "loading is already in progress", "model": a.progress.snapshot().Model,
			"progress": a.progress.snapshot(),
		})
		return
	}

	opts := sdbackend.LoadOptions{}
	if req.TimeoutSec > 0 {
		opts.ReadinessTimeout = time.Duration(req.TimeoutSec) * time.Second
	}
	if req.Wait {
		ctx, cancel := context.WithTimeout(r.Context(), a.loadTimeout(opts))
		defer cancel()
		if _, err := a.svc.Sup.Load(ctx, name, opts); err != nil {
			status, code := statusForError(err)
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"error": err.Error(), "code": code, "status_code": status,
				"progress": a.progress.snapshot(),
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "loaded", "model": name, "progress": a.progress.snapshot()})
		return
	}

	// Фоновый spawn + трекинг прогресса. a.loadCtx живёт дольше запроса:
	// клиент может отвалиться, а загрузка обязана доиграть (иначе на диске
	// останется «наполовину запущенный» процесс).
	go a.loadInBackground(name, opts)
	writeJSON(w, http.StatusAccepted, map[string]any{
		"status": "loading", "model": name, "progress": a.progress.snapshot(),
	})
}

// loadTimeout — сколько ждать readiness (с запасом к конфигу).
func (a *App) loadTimeout(opts sdbackend.LoadOptions) time.Duration {
	if opts.ReadinessTimeout > 0 {
		return opts.ReadinessTimeout + 30*time.Second
	}
	cfg := a.svc.Config
	t := time.Duration(cfg.StartupTimeoutSec) * time.Second
	if t <= 0 {
		t = 180 * time.Second
	}
	return t + 30*time.Second
}

// loadInBackground — загрузка с публикацией прогресса.
func (a *App) loadInBackground(name string, opts sdbackend.LoadOptions) {
	a.progress.set("spawn", sdbackend.StateLoading, name, "")
	ctx, cancel := context.WithTimeout(a.loadCtx, a.loadTimeout(opts))
	defer cancel()
	_, err := a.svc.Sup.Load(ctx, name, opts)
	if err != nil {
		a.progress.set("failed", sdbackend.StateError, name, err.Error())
		appLog().Errorw("background model load failed", "model", name, "error", err)
		return
	}
	a.progress.set("ready", sdbackend.StateLoaded, name, "")
}

// handleUnloadModel — POST /api/image/models/unload.
func (a *App) handleUnloadModel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	if a.svc.Sup.InFlight() > 0 {
		// Выгрузка во время генерации убила бы клиента без ответа и не
		// освободила VRAM мгновенно. Честный 409 лучше «тихой» потери запроса.
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":          "generation in progress; retry after it finishes",
			"active_queries": a.svc.Sup.InFlight(),
		})
		return
	}
	model := a.svc.Sup.CurrentModel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := a.svc.Sup.Unload(ctx); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	a.progress.set("unloaded", sdbackend.StateNotLoaded, model, "")
	writeJSON(w, http.StatusOK, map[string]any{"status": "unloaded", "model": model})
}

// handleReloadModel — POST /api/image/models/reload (kill + spawn).
func (a *App) handleReloadModel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var req loadRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = a.svc.Sup.CurrentModel()
	}
	if name == "" {
		writeError(w, http.StatusBadRequest, "name is required (no model is loaded)")
		return
	}
	if _, ok := a.svc.Registry.Profile(name); !ok {
		writeError(w, http.StatusNotFound, fmt.Sprintf("%s: %q", sdbackend.ErrModelNotFound, name))
		return
	}
	go func() {
		a.progress.set("reload:unload", sdbackend.StateLoading, name, "")
		if err := a.svc.Sup.Unload(a.loadCtx); err != nil {
			appLog().Warnw("reload: unload failed, spawn anyway", "model", name, "error", err)
		}
		a.loadInBackground(name, sdbackend.LoadOptions{Force: true})
	}()
	writeJSON(w, http.StatusAccepted, map[string]any{"status": "reloading", "model": name})
}

// handleLoadProgress — GET /api/image/models/load/progress.
func (a *App) handleLoadProgress(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	snap := a.progress.snapshot()
	// Если трекер ничего не знает (воркер только стартовал), отдаём состояние
	// супервизора — контракт ответа одинаков.
	if snap.State == "" {
		snap.State = a.svc.Sup.State()
		snap.Model = a.svc.Sup.CurrentModel()
		snap.Error = a.svc.Sup.LastError()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"progress": snap,
		"state":    a.svc.Sup.State(),
		"model":    a.svc.Sup.CurrentModel(),
		"pid":      a.svc.Sup.PID(),
	})
}

// handleLoadProgressStream — GET /api/image/models/load/progress/stream (SSE).
//
// Формат — как у cppworker (handlers_model_sse.go): строки `data: {json}\n\n`,
// плюс периодический heartbeat, чтобы прокси/браузер не закрыли соединение.
func (a *App) handleLoadProgressStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	ch := a.progress.subscribe()
	defer a.progress.unsubscribe(ch)

	send := func(p loadProgress) {
		data, err := json.Marshal(p)
		if err != nil {
			return
		}
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
	}
	// Начальный снимок — клиент сразу видит состояние.
	send(a.progress.snapshot())

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case p, ok := <-ch:
			if !ok {
				return
			}
			send(p)
		case <-heartbeat.C:
			fmt.Fprint(w, ": heartbeat\n\n")
			flusher.Flush()
		}
	}
}

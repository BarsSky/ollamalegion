package main

// ============================================================
// SSE load progress stream (Round 25, 2026-08-04)
//
// Real-time прогресс загрузки модели через Server-Sent Events.
// Вместо polling каждые 1.5s (см. GgufLoadProgress.startPolling),
// WebUI подписывается на EventSource и получает push-обновления
// каждые 500ms пока state=loading, потом финальный event loaded/error.
//
// Преимущества:
//   - Мгновенный feedback при переходе loading→loaded (0ms vs 1500ms)
//   - Меньше HTTP overhead (1 долгое соединение vs N коротких)
//   - Heartbeat :keepalive каждые 15s против idle-timeout proxy
//
// Endpoint: GET /api/models/load/progress/stream?model=<name>
//   - ?model=<name>     → stream для конкретной модели
//   - ?model=* (default) → stream для всех loading-моделей
//
// Response format (text/event-stream):
//   data: {"name":"X","state":"loading","elapsedMs":1234,"loadingSizeBytes":5242880000}\n\n
//   :keepalive\n\n              (heartbeat, comment)
//   data: {"name":"X","state":"loaded","elapsedMs":87920,"loadDurationMs":87920}\n\n  (final)
//
// Клиент может прервать SSE (r.Context().Done()) — handler это увидит
// и закроет стрим чисто. Heartbeat защищает от proxy idle-timeout (5-30s).
// ============================================================

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"ollama-loadbalancer/pkg/logger"
)

// sseHeartbeatInterval — как часто шлём SSE comment для предотвращения
// idle-timeout на reverse proxies (nginx default 60s, k8s ingress 5s).
const sseHeartbeatInterval = 15 * time.Second

// sseUpdateInterval — как часто обновляем state (500ms — компромисс
// между свежестью и CPU).
const sseUpdateInterval = 500 * time.Millisecond

// handleLoadProgressStream — SSE endpoint для real-time load progress.
//
// GET /api/models/load/progress/stream?model=<name>
// GET /api/models/load/progress/stream (no model = all loading models)
func handleLoadProgressStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "use GET")
		return
	}

	// SSE headers — обязательные для корректной работы EventSource.
	// Ставим ДО backend check чтобы streaming-фоллбэк тоже отдавал
	// корректный Content-Type (EventSource парсит только этот тип).
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // nginx: не буферизировать

	// Если backend ещё не инициализирован (test setup или startup race),
	// отдаём SSE с not_ready и закрываем. Без этого — nil pointer panic
	// в GetLoadingModels / GetModel.
	if backend == nil {
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			writeSSEEvent(w, f, map[string]interface{}{
				"state":    "not_ready",
				"error":    "backend not initialized",
				"terminal": true,
			})
		}
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	name := strings.TrimSpace(r.URL.Query().Get("model"))

	w.WriteHeader(http.StatusOK)

	// Если model указана — стримим одну модель, закрываем на loaded/error.
	// Если model == "" или "*" — стримим список loading моделей (без auto-close).
	if name != "" && name != "*" {
		streamSingleModelProgress(w, r, flusher, name)
		return
	}
	streamAllModelsProgress(w, r, flusher)
}

// streamSingleModelProgress стримит прогресс одной модели до loaded/error,
// потом закрывает SSE соединение.
func streamSingleModelProgress(w http.ResponseWriter, r *http.Request,
	flusher http.Flusher, name string) {
	ticker := time.NewTicker(sseUpdateInterval)
	defer ticker.Stop()
	heartbeat := time.NewTicker(sseHeartbeatInterval)
	defer heartbeat.Stop()

	// Initial event: отправляем текущее состояние сразу (без ожидания первого tick).
	if !writeModelProgressEvent(w, flusher, name) {
		return // модель уже loaded/unloaded, connection closed by writeModelProgressEvent
	}

	for {
		select {
		case <-r.Context().Done():
			logger.Get().Debugw("SSE: client disconnected",
				"name", name)
			return
		case <-heartbeat.C:
			// Keepalive: comment line (EventSource игнорирует).
			if _, err := fmt.Fprintf(w, ":keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-ticker.C:
			// Push update. writeModelProgressEvent returns true если
			// модель всё ещё loading, false если уже terminal state —
			// тогда закрываем SSE.
			if !writeModelProgressEvent(w, flusher, name) {
				return
			}
		}
	}
}

// writeModelProgressEvent — отправляет один SSE event с текущим
// состоянием модели. Возвращает false если состояние terminal
// (loaded/error/unloaded) → клиенту достаточно одного финального event'а.
func writeModelProgressEvent(w http.ResponseWriter, flusher http.Flusher, name string) bool {
	// Check loading models first (state=loading).
	for _, lm := range backend.GetLoadingModels() {
		if lm.Name != name {
			continue
		}
		elapsed := int64(0)
		if !lm.LoadingStartedAt.IsZero() {
			elapsed = time.Since(lm.LoadingStartedAt).Milliseconds()
		}
		event := map[string]interface{}{
			"name":             lm.Name,
			"state":            lm.State,
			"loadingStartedAt": lm.LoadingStartedAt.UTC().Format(time.RFC3339Nano),
			"loadingSizeBytes": lm.LoadingSizeBytes,
			"elapsedMs":        elapsed,
		}
		if lm.LoadingError != "" {
			event["error"] = lm.LoadingError
		}
		return writeSSEEvent(w, flusher, event)
	}

	// Model not in loading list — check loaded models.
	if m, err := backend.GetModel(name); err == nil {
		elapsed := int64(0)
		loadDurationMs := int64(0)
		if !m.LoadedAt.IsZero() {
			elapsed = time.Since(m.LoadedAt).Milliseconds()
			loadDurationMs = time.Since(m.LoadedAt).Milliseconds()
		}
		// If state is "loading" but not in loading list (shouldn't happen),
		// report it. Otherwise loaded/unloaded/error.
		event := map[string]interface{}{
			"name":           m.Name,
			"state":          m.State,
			"loadedAt":       m.LoadedAt.UTC().Format(time.RFC3339Nano),
			"elapsedMs":      elapsed,
			"loadDurationMs": loadDurationMs,
			"sizeBytes":      m.SizeBytes,
			"contextSize":    m.ContextSize,
			"terminal":       true, // signal to client: stream is done
		}
		_ = writeSSEEvent(w, flusher, event)
		return false // terminal state, close stream
	}

	// Model not found at all.
	event := map[string]interface{}{
		"name":     name,
		"state":    "not_found",
		"error":    "model not found and not loading: " + name,
		"terminal": true,
	}
	_ = writeSSEEvent(w, flusher, event)
	return false
}

// streamAllModelsProgress стримит прогресс ВСЕХ loading моделей.
// Не закрывает соединение пока есть хоть одна loading модель
// (клиент может навигировать между вкладками).
func streamAllModelsProgress(w http.ResponseWriter, r *http.Request,
	flusher http.Flusher) {
	ticker := time.NewTicker(sseUpdateInterval)
	defer ticker.Stop()
	heartbeat := time.NewTicker(sseHeartbeatInterval)
	defer heartbeat.Stop()

	// Initial event.
	writeAllLoadingModelsEvent(w, flusher)

	for {
		select {
		case <-r.Context().Done():
			logger.Get().Debugw("SSE: client disconnected (all models stream)")
			return
		case <-heartbeat.C:
			if _, err := fmt.Fprintf(w, ":keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-ticker.C:
			writeAllLoadingModelsEvent(w, flusher)
		}
	}
}

func writeAllLoadingModelsEvent(w http.ResponseWriter, flusher http.Flusher) {
	loading := backend.GetLoadingModels()
	out := make([]map[string]interface{}, 0, len(loading))
	now := time.Now()
	for _, m := range loading {
		elapsed := int64(0)
		if !m.LoadingStartedAt.IsZero() {
			elapsed = now.Sub(m.LoadingStartedAt).Milliseconds()
		}
		out = append(out, map[string]interface{}{
			"name":             m.Name,
			"state":            m.State,
			"loadingStartedAt": m.LoadingStartedAt.UTC().Format(time.RFC3339Nano),
			"loadingSizeBytes": m.LoadingSizeBytes,
			"elapsedMs":        elapsed,
			"error":            m.LoadingError,
		})
	}
	event := map[string]interface{}{
		"models":    out,
		"count":     len(out),
		"timestamp": now.UTC().Format(time.RFC3339Nano),
	}
	_ = writeSSEEvent(w, flusher, event)
}

// writeSSEEvent сериализует event как JSON и пишет в формате SSE:
//
//	data: <json>\n\n
//
// Возвращает false если клиент отвалился (write error).
func writeSSEEvent(w http.ResponseWriter, flusher http.Flusher, event map[string]interface{}) bool {
	data, err := json.Marshal(event)
	if err != nil {
		logger.Get().Errorw("SSE: failed to marshal event", "error", err)
		return true // try next event
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
		// Client disconnected (broken pipe / closed connection).
		return false
	}
	flusher.Flush()
	return true
}

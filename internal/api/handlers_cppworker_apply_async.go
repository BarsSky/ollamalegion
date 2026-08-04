// Package api — async apply profile (Round 26 v0.5.13).
//
// Background apply при занятом бэкенде — Bug #1+#2 fix.
// Сценарий: пользователь в OpenWebUI генерирует ответ модели, и в это
// время хочет изменить настройки (n_ctx и т.п.). handleReloadModel на
// cppworker делает inflight.WaitZero(req.Name, 0) и блокируется на
// длительность генерации (30-60+ сек для длинных ответов).
//
// Эти endpoint'ы:
//   - handleAsyncApply — POST /api/v1/cppworker/model-profiles/{name}/apply
//     если есть busy бэкенды. Возвращает 202 + Location + apply_id, и
//     запускает reload в background goroutine.
//   - handleApplyProfileProgress — GET /api/v1/cppworker/model-profiles/{name}/apply/progress
//     SSE stream с прогрессом (busy → reloading → reloaded/error).
//   - handleApplyProfileStatus — GET /api/v1/cppworker/model-profiles/{name}/apply/status/{applyId}
//     одноразовый JSON snapshot статуса.
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// applyJobState — состояние background apply job'а.
type applyJobState string

const (
	applyStateQueued    applyJobState = "queued"    // ждём, пока in-flight запросы завершатся
	applyStateReloading applyJobState = "reloading" // cppworker handleReloadModel делает unload+load
	applyStateReloaded  applyJobState = "reloaded"  // успешно
	applyStatePartial   applyJobState = "partial"   // часть бэкендов ok, часть failed
	applyStateError     applyJobState = "error"     // полный fail
)

// applyJob — единичный background apply.
type applyJob struct {
	ApplyID   string                       `json:"applyId"`
	Model     string                       `json:"model"`
	Profile   types.LlamaCppModelProfile   `json:"profile"`
	StartedAt time.Time                    `json:"startedAt"`
	UpdatedAt time.Time                    `json:"updatedAt"`
	State     applyJobState                `json:"state"`
	Busy      int64                        `json:"initialBusy"` // busy count at start
	Backends  map[string]applyBackendState `json:"backends"`
	Terminal  bool                         `json:"terminal"`
	FinalBody *modelProfileApplyResponse   `json:"finalBody,omitempty"`
}

// applyBackendState — per-backend статус.
type applyBackendState struct {
	Status     string    `json:"status"` // "reloaded" | "skipped" | "error" | "pending"
	Message    string    `json:"message,omitempty"`
	StartedAt  time.Time `json:"startedAt,omitempty"`
	FinishedAt time.Time `json:"finishedAt,omitempty"`
}

var (
	applyJobsMu sync.RWMutex
	applyJobs   = make(map[string]*applyJob) // key: applyId
)

// submitApplyJob — добавляет job в registry и возвращает его.
// Вызывается из handleAsyncApply.
func submitApplyJob(model string, profile types.LlamaCppModelProfile, busy int64, backends []string) *applyJob {
	applyID := fmt.Sprintf("apply-%d", time.Now().UnixNano())
	job := &applyJob{
		ApplyID:   applyID,
		Model:     model,
		Profile:   profile,
		StartedAt: time.Now(),
		UpdatedAt: time.Now(),
		State:     applyStateQueued,
		Busy:      busy,
		Backends:  make(map[string]applyBackendState, len(backends)),
	}
	for _, b := range backends {
		job.Backends[b] = applyBackendState{Status: "pending"}
	}
	applyJobsMu.Lock()
	applyJobs[applyID] = job
	// Cleanup: keep last 50 jobs (LIFO)
	if len(applyJobs) > 50 {
		// Сортируем по StartedAt, удаляем самые старые
		type kv struct {
			id string
			t  time.Time
		}
		all := make([]kv, 0, len(applyJobs))
		for k, v := range applyJobs {
			all = append(all, kv{k, v.StartedAt})
		}
		sort.Slice(all, func(i, j int) bool { return all[i].t.Before(all[j].t) })
		toRemove := len(applyJobs) - 50
		for i := 0; i < toRemove; i++ {
			delete(applyJobs, all[i].id)
		}
	}
	applyJobsMu.Unlock()
	return job
}

// updateApplyJob — обновляет state job'а (для background goroutine).
func updateApplyJob(applyID string, mut func(*applyJob)) {
	applyJobsMu.Lock()
	defer applyJobsMu.Unlock()
	if job, ok := applyJobs[applyID]; ok {
		mut(job)
		job.UpdatedAt = time.Now()
	}
}

// getApplyJob — возвращает job по ID (для SSE + status endpoint'ов).
func getApplyJob(applyID string) *applyJob {
	applyJobsMu.RLock()
	defer applyJobsMu.RUnlock()
	return applyJobs[applyID]
}

// handleAsyncApply — POST /api/v1/cppworker/model-profiles/{name}/apply при
// занятом бэкенде. Возвращает 202 + Location + applyId, и запускает
// background reload. Реальный результат клиент забирает через SSE
// или status endpoint.
func (s *Server) handleAsyncApply(w http.ResponseWriter, r *http.Request, modelName string, profile types.LlamaCppModelProfile, busyBackends []string) {
	log := logger.Get()

	// Подсчитаем total busy across all busyBackends
	var totalBusy int64 = 0
	for _, b := range busyBackends {
		if count, err := s.GetActiveQueriesForModel(b, modelName); err == nil {
			totalBusy += count
		}
	}

	job := submitApplyJob(modelName, profile, totalBusy, busyBackends)

	log.Infow("applyModelProfile: async path",
		"applyId", job.ApplyID, "model", modelName,
		"busyBackends", busyBackends, "totalActive", totalBusy)

	// Запускаем background worker, который ждёт, пока in-flight запросы
	// завершатся, и делает reload. Используем polling active-queries
	// (не блокируем API сервер).
	go s.runAsyncApplyJob(job, busyBackends)

	// 202 + Location
	scheme := schemeFromRequest(r)
	host := r.Host
	progressURL := fmt.Sprintf("%s://%s/api/v1/cppworker/model-profiles/%s/apply/progress?applyId=%s",
		scheme, host, modelName, job.ApplyID)
	statusURL := fmt.Sprintf("%s://%s/api/v1/cppworker/model-profiles/%s/apply/status/%s",
		scheme, host, modelName, job.ApplyID)

	w.Header().Set("Location", progressURL)
	s.writeJSON(w, http.StatusAccepted, map[string]interface{}{
		"status":               "accepted",
		"applyId":              job.ApplyID,
		"model":                modelName,
		"profile":              profile,
		"busyBackends":         busyBackends,
		"initialActiveQueries": totalBusy,
		"progressUrl":          progressURL,
		"statusUrl":            statusURL,
		"message":              "Apply queued: model is busy. Use progressUrl for live updates.",
	})
}

// runAsyncApplyJob — background worker для handleAsyncApply.
// Алгоритм:
//  1. Poll active-queries пока не станет 0 (max 5 минут)
//  2. Запустить reload на каждом busy бэкенде
//  3. Обновить final state
func (s *Server) runAsyncApplyJob(job *applyJob, busyBackends []string) {
	log := logger.Get()
	pollInterval := 2 * time.Second
	maxWait := 5 * time.Minute
	deadline := time.Now().Add(maxWait)

	// Шаг 1: ждём, пока in-flight генерации завершатся.
	// polling active-queries для каждого busy бэкенда.
	for {
		allIdle := true
		for _, b := range busyBackends {
			count, err := s.GetActiveQueriesForModel(b, job.Model)
			if err != nil {
				log.Warnw("async apply: active-queries check failed",
					"applyId", job.ApplyID, "backend", b, "error", err)
				continue // soft-fail: предполагаем что busy, проверим на следующей итерации
			}
			if count > 0 {
				allIdle = false
			}
		}
		if allIdle {
			log.Infow("async apply: all backends idle, starting reload",
				"applyId", job.ApplyID, "model", job.Model)
			break
		}
		if time.Now().After(deadline) {
			log.Warnw("async apply: timeout waiting for in-flight to drain",
				"applyId", job.ApplyID, "model", job.Model, "waitSec", int(maxWait.Seconds()))
			updateApplyJob(job.ApplyID, func(j *applyJob) {
				j.State = applyStateError
				j.Terminal = true
				for b := range j.Backends {
					j.Backends[b] = applyBackendState{
						Status:  "error",
						Message: fmt.Sprintf("timeout %s waiting for in-flight to drain", maxWait),
					}
				}
			})
			return
		}
		time.Sleep(pollInterval)
	}

	// Шаг 2: reload каждого busy бэкенда.
	updateApplyJob(job.ApplyID, func(j *applyJob) {
		j.State = applyStateReloading
		for b := range j.Backends {
			j.Backends[b] = applyBackendState{Status: "pending", StartedAt: time.Now()}
		}
	})

	// Найдём backend objects
	allBackends := s.proxy.GetAllBackends()
	backendByID := make(map[string]types.Backend, len(allBackends))
	for _, b := range allBackends {
		backendByID[b.ID] = b
	}

	results := make([]modelProfileApplyBackendResult, 0, len(busyBackends))
	for _, bID := range busyBackends {
		backend, ok := backendByID[bID]
		if !ok {
			updateApplyJob(job.ApplyID, func(j *applyJob) {
				j.Backends[bID] = applyBackendState{Status: "error", Message: "backend not found"}
			})
			results = append(results, modelProfileApplyBackendResult{
				BackendID: bID,
				Status:    "error",
				Message:   "backend not found",
			})
			continue
		}
		if err := s.reloadModelOnCppWorker(backend, job.Model, job.Profile); err != nil {
			log.Warnw("async apply: reload failed",
				"applyId", job.ApplyID, "backend", bID, "error", err)
			updateApplyJob(job.ApplyID, func(j *applyJob) {
				j.Backends[bID] = applyBackendState{
					Status:     "error",
					Message:    err.Error(),
					FinishedAt: time.Now(),
				}
			})
			results = append(results, modelProfileApplyBackendResult{
				BackendID: bID,
				Status:    "error",
				Message:   err.Error(),
			})
			continue
		}
		log.Infow("async apply: reloaded",
			"applyId", job.ApplyID, "backend", bID, "model", job.Model)
		updateApplyJob(job.ApplyID, func(j *applyJob) {
			j.Backends[bID] = applyBackendState{
				Status:     "reloaded",
				FinishedAt: time.Now(),
			}
		})
		results = append(results, modelProfileApplyBackendResult{
			BackendID: bID,
			Status:    "reloaded",
		})
	}

	// Шаг 3: финализация
	sort.Slice(results, func(i, j int) bool { return results[i].BackendID < results[j].BackendID })
	hasErrors := false
	for _, r := range results {
		if r.Status == "error" {
			hasErrors = true
		}
	}

	finalState := applyStateReloaded
	if hasErrors {
		finalState = applyStatePartial
	}

	updateApplyJob(job.ApplyID, func(j *applyJob) {
		j.State = finalState
		j.Terminal = true
		j.FinalBody = &modelProfileApplyResponse{
			Model:    job.Model,
			Profile:  job.Profile,
			Backends: results,
		}
	})
}

// handleApplyProfileProgress — GET /api/v1/cppworker/model-profiles/{name}/apply/progress?applyId=X
// SSE stream с прогрессом apply job'а.
//
// Поведение:
//   - Если applyId указан и существует — стримит events пока job.Terminal=true
//   - Если applyId не указан — 400
//   - Применяет те же правила что и handleLoadProgressStream (Round 25):
//     heartbeat 15s, flush per event, auto-close на terminal
func (s *Server) handleApplyProfileProgress(w http.ResponseWriter, r *http.Request, modelName string) {
	if r.Method != http.MethodGet {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "use GET"})
		return
	}

	applyID := r.URL.Query().Get("applyId")
	if applyID == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "applyId required"})
		return
	}

	job := getApplyJob(applyID)
	if job == nil {
		s.writeJSON(w, http.StatusNotFound, map[string]string{
			"error":   "apply job not found",
			"applyId": applyID,
		})
		return
	}
	if job.Model != modelName {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("applyId %q does not match model %q", applyID, modelName),
		})
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "streaming not supported"})
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// Если уже terminal — emit один event и закрываем.
	if job.Terminal {
		s.emitApplySSEEvent(w, flusher, job)
		return
	}

	// Streaming loop: emit каждые 500ms, heartbeat 15s, auto-close на terminal.
	const updateInterval = 500 * time.Millisecond
	const heartbeatInterval = 15 * time.Second

	ticker := time.NewTicker(updateInterval)
	defer ticker.Stop()

	lastUpdate := job.UpdatedAt
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			cur := getApplyJob(applyID)
			if cur == nil {
				return
			}
			// Emit только если state изменился
			if !cur.UpdatedAt.Equal(lastUpdate) {
				s.emitApplySSEEvent(w, flusher, cur)
				lastUpdate = cur.UpdatedAt
			}
			if cur.Terminal {
				return
			}
		case <-time.After(heartbeatInterval):
			// heartbeat
			fmt.Fprintf(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

// emitApplySSEEvent — emit one SSE event with current job state.
func (s *Server) emitApplySSEEvent(w http.ResponseWriter, flusher http.Flusher, job *applyJob) {
	payload, _ := json.Marshal(map[string]interface{}{
		"applyId":   job.ApplyID,
		"model":     job.Model,
		"state":     string(job.State),
		"backends":  job.Backends,
		"terminal":  job.Terminal,
		"updatedAt": job.UpdatedAt,
		"elapsedMs": time.Since(job.StartedAt).Milliseconds(),
	})
	if job.Terminal && job.FinalBody != nil {
		fmt.Fprintf(w, "event: complete\ndata: %s\n\n", payload)
	} else {
		fmt.Fprintf(w, "event: progress\ndata: %s\n\n", payload)
	}
	flusher.Flush()
}

// handleApplyProfileStatus — GET /api/v1/cppworker/model-profiles/{name}/apply/status/{applyId}
// Одноразовый JSON snapshot статуса apply job'а.
// Удобно для WebUI fallback (если EventSource не работает — polling).
func (s *Server) handleApplyProfileStatus(w http.ResponseWriter, r *http.Request, modelName, applyID string) {
	if r.Method != http.MethodGet {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "use GET"})
		return
	}
	job := getApplyJob(applyID)
	if job == nil {
		s.writeJSON(w, http.StatusNotFound, map[string]string{
			"error":   "apply job not found",
			"applyId": applyID,
		})
		return
	}
	if job.Model != modelName {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("applyId %q does not match model %q", applyID, modelName),
		})
		return
	}

	resp := map[string]interface{}{
		"applyId":   job.ApplyID,
		"model":     job.Model,
		"state":     string(job.State),
		"backends":  job.Backends,
		"terminal":  job.Terminal,
		"elapsedMs": time.Since(job.StartedAt).Milliseconds(),
		"updatedAt": job.UpdatedAt,
	}
	if job.Terminal && job.FinalBody != nil {
		resp["result"] = job.FinalBody
	}
	s.writeJSON(w, http.StatusOK, resp)
}

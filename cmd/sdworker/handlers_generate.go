// handlers_generate.go — нативный API генерации и работа с джобами.
//
//	POST /api/image/generate          → 202 {id, state:"queued", ...} (async)
//	GET  /api/image/jobs              → список живых джоб
//	GET  /api/image/jobs/{id}         → состояние джобы
//	POST /api/image/jobs/{id}/cancel  → 200 (queued) | 409 (generating, как sd-server)
//	GET  /api/image/queue             → состояние очереди
//
// ПОЧЕМУ НАТИВНЫЙ ПУТЬ АСИНХРОННЫЙ, А OPENAI/A1111 — СИНХРОННЫЕ:
// клиенты OpenAI/A1111 не умеют job-polling (их SDK ждут картинку в ответе),
// а наш WebUI/балансер — умеют и хотят отдавать 202 + прогресс, чтобы не
// держать соединение минутами на медленном железе.
package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"ollama-loadbalancer/internal/sdbackend"
)

// generateRequest — тело POST /api/image/generate.
type generateRequest struct {
	Model            string   `json:"model"`
	Prompt           string   `json:"prompt"`
	NegativePrompt   string   `json:"negativePrompt"`
	Width            int      `json:"width"`
	Height           int      `json:"height"`
	Size             string   `json:"size"`
	Steps            int      `json:"steps"`
	CFG              float64  `json:"cfg"`
	Seed             *int64   `json:"seed"`
	Sampler          string   `json:"sampler"`
	Scheduler        string   `json:"scheduler"`
	Batch            int      `json:"batch"`
	N                int      `json:"n"`
	ClipSkip         int      `json:"clipSkip"`
	OutputFormat     string   `json:"outputFormat"`
	OutputCompression *int    `json:"outputCompression"`
	ResponseFormat   string   `json:"responseFormat"`
	Lora             []sdbackend.LoraRef `json:"lora"`
	// Sync=true — дождаться результата в этом же запросе.
	Sync bool `json:"sync"`
}

// handleGenerate — POST /api/image/generate.
func (a *App) handleGenerate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var req generateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Prompt) == "" {
		writeError(w, http.StatusBadRequest, "prompt is required")
		return
	}

	w0, h0 := req.Width, req.Height
	if req.Size != "" {
		sw, sh, err := sdbackend.ParseSize(req.Size)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if sw > 0 {
			w0, h0 = sw, sh
		}
	}
	batch := req.Batch
	if batch <= 0 {
		batch = req.N
	}
	provided := req.Seed != nil
	var seedVal int64
	if provided {
		seedVal = *req.Seed
	}
	model := a.resolveModel(req.Model)
	profile := a.svc.Runner.ProfileFor(model)
	norm, err := sdbackend.NormalizeGeneration(sdbackend.GenerationRequest{
		Prompt:            req.Prompt,
		NegativePrompt:    req.NegativePrompt,
		Width:             w0,
		Height:            h0,
		Steps:             req.Steps,
		CFGScale:          req.CFG,
		Seed:              seedVal,
		SeedProvided:      provided,
		Sampler:           req.Sampler,
		Scheduler:         req.Scheduler,
		BatchCount:        batch,
		ClipSkip:          req.ClipSkip,
		OutputFormat:      req.OutputFormat,
		OutputCompression: req.OutputCompression,
		Lora:              req.Lora,
	}, profile, a.limitsFor())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	urlMode := strings.EqualFold(strings.TrimSpace(req.ResponseFormat), "url")

	if req.Sync {
		res, err := a.svc.Runner.GenerateSync(r.Context(), model, urlMode, false, norm)
		if err != nil {
			status, code := statusForError(err)
			if status == http.StatusTooManyRequests {
				setRetryAfter(w, 5)
			}
			writeJSONError(w, status, code, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, generationResponse(model, res, norm.Notes, false))
		return
	}

	rec, err := a.svc.Runner.Submit(r.Context(), model, urlMode, false, norm)
	if err != nil {
		status, code := statusForError(err)
		if status == http.StatusTooManyRequests {
			// Наша очередь на 64 слота заполнена. Честный 429 + Retry-After:
			// клиент знает, что повторить, а не ждёт молча.
			setRetryAfter(w, 5)
		}
		writeJSONError(w, status, code, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"id":            rec.ID,
		"state":         rec.State,
		"model":         rec.Model,
		"queue_position": rec.QueuePosition,
		"created":       rec.Created.Unix(),
		"seed":          rec.Seed,
		"width":         rec.Width,
		"height":        rec.Height,
		"steps":         rec.Steps,
		"batch":         rec.BatchCount,
		"notes":         norm.Notes,
		"poll_url":      "/api/image/jobs/" + rec.ID,
	})
}

// generationResponse — единая форма ответа для sync-пути.
func generationResponse(model string, res *sdbackend.GenerationResultView, notes []string, async bool) map[string]any {
	data := make([]map[string]any, 0, len(res.Images))
	for _, img := range res.Images {
		item := map[string]any{"index": img.Index}
		if img.B64JSON != "" {
			item["b64_json"] = img.B64JSON
		}
		if img.URL != "" {
			item["url"] = img.URL
		}
		data = append(data, item)
	}
	return map[string]any{
		"created":       time.Now().Unix(),
		"model":         model,
		"output_format": res.OutputFormat,
		"seed":          res.Seed,
		"width":         res.Width,
		"height":        res.Height,
		"steps":         res.Steps,
		"batch":         res.BatchCount,
		"duration_ms":   res.Duration.Milliseconds(),
		"data":          data,
		"notes":         notes,
		"async":         async,
	}
}

// handleListJobs — GET /api/image/jobs.
func (a *App) handleListJobs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"jobs":  a.svc.Runner.List(),
		"count": a.svc.Runner.Visible(),
	})
}

// handleJobByID — GET /api/image/jobs/{id} и POST /api/image/jobs/{id}/cancel.
func (a *App) handleJobByID(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/image/jobs/")
	rest = strings.Trim(rest, "/")
	if rest == "" {
		writeError(w, http.StatusBadRequest, "job id is required")
		return
	}
	parts := strings.Split(rest, "/")
	id := parts[0]
	cancel := len(parts) > 1 && parts[1] == "cancel"

	if cancel {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "use POST")
			return
		}
		err := a.svc.Runner.Cancel(id)
		if err != nil {
			status, code := statusForError(err)
			writeJSONError(w, status, code, err.Error())
			return
		}
		rec := a.svc.Runner.Get(id)
		if rec == nil {
			writeJSONError(w, http.StatusNotFound, "job_not_found", "job not found: "+id)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "cancelled", "job": rec})
		return
	}

	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	rec := a.svc.Runner.Get(id)
	if rec == nil {
		writeJSONError(w, http.StatusNotFound, "job_not_found",
			"job not found (TTL завершённых джоб — "+a.jobTTLText()+"): "+id)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"job": rec})
}

// handleQueue — GET /api/image/queue.
func (a *App) handleQueue(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"capacity":  a.svc.Queue.Capacity(),
		"in_flight": a.svc.Queue.InFlight(),
		"rejected":  a.svc.Queue.Rejected(),
		"jobs":      a.svc.Runner.Visible(),
	})
}

// jobTTLText — текст TTL для сообщений об ошибке (не хардкодим «600»).
func (a *App) jobTTLText() string {
	return (10 * time.Minute).String()
}

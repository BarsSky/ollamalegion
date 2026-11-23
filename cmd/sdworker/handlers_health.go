// handlers_health.go — /health, /info, /metrics.
package main

import (
	"net/http"
	"time"

	"ollama-loadbalancer/internal/sdbackend"
	"ollama-loadbalancer/pkg/version"
)

// handleHealth — GET /health.
//
// ВАЖНО ПРО СЕМАНТИКУ: 200 отдаётся и когда модель НЕ загружена — воркер
// работоспособен (готов принять load/generate). Иначе idle-unload убивал бы
// субпроцесс, и Docker переводил бы контейнер в unhealthy между генерациями.
// Состояние модели видно в поле model_loaded, а «движок не поднялся» —
// отдельным полем engine_error (балансер для image_cpp смотрит на
// /sdcpp/v1/capabilities, а не на этот /health — см. internal/balancer/health.go:206).
func (a *App) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	state := a.svc.Sup.State()
	resp := map[string]any{
		"status":          "ok",
		"version":         version.Get().String(),
		"uptime_seconds":  int64(time.Since(a.startedAt).Seconds()),
		"state":           state,
		"model_loaded":    state == sdbackend.StateLoaded,
		"model":           a.svc.Sup.CurrentModel(),
		"sd_server_pid":   a.svc.Sup.PID(),
		"queue_in_flight": a.svc.Queue.InFlight(),
		"queue_size":      a.svc.Queue.Capacity(),
		"models_known":    len(a.svc.Registry.Names()),
	}
	if errText := a.svc.Sup.LastError(); errText != "" {
		resp["engine_error"] = errText
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleInfo — GET /info (диагностика: конфиг + ревизия контракта).
//
// R715 (D3): тело собирает infoSnapshotR715 — тот же сборщик кладёт info.json в
// диагностический архив. Два независимых дампа со временем расходились бы
// полями, и архив перестал бы совпадать с тем, что оператор видит в /info.
func (a *App) handleInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	writeJSON(w, http.StatusOK, a.infoSnapshotR715(r.Context()))
}

// handleMetrics — GET /metrics (Prometheus text format).
//
// VRAM читаем best-effort через nvidia-smi (на AMD/Intel — просто нет метрик).
func (a *App) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	vram := sdbackend.QueryVRAM(r.Context())
	text := a.svc.Metrics.PrometheusText(a.svc.Sup.State(), &vram)
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(text))
}

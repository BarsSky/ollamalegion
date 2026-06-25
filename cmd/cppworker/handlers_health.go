package main

import (
	"net/http"
	"time"
)

// ============================================================
// Health & Info handlers
// ============================================================

func handleHealth(w http.ResponseWriter, r *http.Request) {
	version := "initializing"
	if backend != nil {
		version = backend.Version()
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"status":  "ok",
		"version": version,
	})
}

func handleInfo(w http.ResponseWriter, r *http.Request) {
	status := backend.Status()
	// 2026-06-24: heartbeat reload_pending — балансировщик читает это поле
	// и НЕ пытается дёргать LoadModel API, пока reload не завершён.
	// Решает race condition: балансировщик polling'ом видел state=loading,
	// дёргал /api/models/load, оба потока упирались в TryLockLoad друг друга.
	if reloadModel := backend.GetReloadPending(); reloadModel != "" {
		startedAt := backend.ReloadStartedAt()
		reloadInfo := map[string]interface{}{
			"model":     reloadModel,
			"startedAt": startedAt.Format(time.RFC3339Nano),
			"elapsedMs": time.Since(startedAt).Milliseconds(),
		}
		status["reload_pending"] = reloadInfo
	}
	// Также прокинем in-flight counter для диагностики.
	if inflight := backend.InFlight(); inflight != nil {
		snap := inflight.Snapshot()
		if len(snap) > 0 {
			status["in_flight_requests"] = snap
		}
	}
	writeJSON(w, http.StatusOK, status)
}

func handleGPUInfo(w http.ResponseWriter, r *http.Request) {
	metrics := backend.GetGPUMetrics()
	devices := backend.GetGPUDevices()
	result := make([]map[string]interface{}, len(devices))
	for i, dev := range devices {
		result[i] = map[string]interface{}{
			"index":       dev.Index,
			"name":        dev.Name,
			"vramTotalMB": dev.VRAMTotalMB,
			"vramFreeMB":  dev.VRAMFreeMB,
		}
		if i < len(metrics) {
			for k, v := range metrics[i] {
				result[i][k] = v
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"gpuCount": backend.GetGPUCount(),
		"devices":  result,
	})
}

func handleCppWorkerVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"version": backend.Version(),
	})
}

package main

import (
	"net/http"
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

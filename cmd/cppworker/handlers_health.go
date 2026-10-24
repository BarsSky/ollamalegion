package main

import (
	"net/http"
	"time"

	"ollama-loadbalancer/internal/cppbackend"
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
	// R135 (2026-10-16): несовпадение CUDA-арок образа и карт. Причина видна
	// ЗАРАНЕЕ и одним полем, а не выясняется по стеку SIGABRT: на карте, чьей
	// архитектуры нет в образе, первое же ядро убивает процесс вместе с
	// загруженной моделью (живой случай: sm_75 против образа под sm_86/sm_120).
	// R185 (2026-10-20): несовпадение арок - ЯВНЫМИ полями, а не только текстом.
	//
	// ЗАЧЕМ. Живой разбор: образ был собран под 86;120, карта оказалась sm_75.
	// Страж предупредил в логе, но потребители (балансер/WebUI) видели только
	// свободный текст внутри одного поля и не могли ни показать значок, ни
	// ОТКАЗАТЬСЯ от загрузки заранее - в итоге клиент крутил перезагрузки, а
	// процесс падал по SIGABRT на первом ядре.
	status["cuda_arch_built"] = cppbackend.BuiltCUDAArches()
	status["cuda_arch_ok"] = backend.CUDAArchWarning() == ""
	if warn := backend.CUDAArchWarning(); warn != "" {
		status["cuda_arch_warning"] = warn
	}
	// R166 (2026-10-17): факты о сети бэкенда (интерфейс, MTU, скорость линка, если
	// ядро её отдаёт). Это НЕ замер пропускной способности: настоящую цифру даёт
	// авто-замер балансера (/api/rpc-link-probe). Панель показывает их раздельно,
	// чтобы «detected» не выдавалось за «measured».
	status["net"] = detectNetLinkFacts()
	// R167 (2026-10-17): факт и намерение по llama.cpp РАЗДЕЛЬНО. env легко
	// расходится с бинарником (на стенде так и было), а слои раскладки обязаны
	// ехать на одном коммите — пусть расхождение будет видно в панели, а не
	// выясняется отказом загрузки.
	if facts := currentLlamaBuildFacts(); facts.Binary != "" {
		status["llama_build"] = facts.Binary
		status["llama_build_env"] = facts.Env
		status["llama_build_mismatch"] = !facts.Compatible
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

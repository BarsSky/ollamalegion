// diag_summary_r476.go — R476 (2026-11-07): «всё, что нужно для разбора, в
// одном ответе».
//
// ЗАЧЕМ. Оператору, чтобы прислать данные со стенда, приходилось искать
// значения по пяти местам: /api/version, /api/v1/cluster, /api/v1/cppworker/
// model-profiles, .env стенда и логи. Один пропущенный флаг — и разбор снова
// строится на догадке. Здесь всё это сводится в один JSON, который WebUI
// показывает с кнопками копирования, а /api/v1/diag/bundle кладёт в архив.
//
// ЧЕГО ЗДЕСЬ НЕТ: секретов. Флаги идут по белому списку (токены, пароли и
// ключи в него не входят), снимок кластера секретов не содержит.
package api

import (
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/pkg/version"
)

// diagFlagWhitelistR476 — переменные, которые реально решают, как поведёт себя
// стенд. Белый список, а не всё окружение: в env есть токены и пароли.
var diagFlagWhitelistR476 = []string{
	// окно контекста и рост
	"LB_NCTX_RELOAD_ENABLED",
	"LB_NCTX_RELOAD_MAX_N_CTX",
	"LB_NCTX_RELOAD_ASYNC",
	"LB_NCTX_PREFLIGHT_ENABLED",
	"LB_NCTX_PREFLIGHT_ASYNC_RELOAD",
	"LB_NCTX_GROW_FOR_STREAM",
	"LB_NCTX_GROW_FOR_AGENTS",
	"LB_PREFLIGHT_EXACT_TOKENS",
	"LB_AGENT_WINDOW_BUDGET",
	"LB_AGENT_OUTPUT_RESERVE_TOKENS",
	"LB_PLACE_OVER_RELOAD",
	// очередь и выбор бэкенда
	"LB_ADMISSION_WAIT_SEC",
	"LB_ADMISSION_KEEPALIVE_SEC",
	"LB_QUEUE_MAX_SIZE",
	"LB_QUEUE_TIMEOUT",
	"LB_QUEUE_WORKERS",
	"LB_MODEL_AFFINITY",
	"LB_SESSION_STICKINESS",
	"LB_AUTO_LOAD_ASYNC",
	"LB_AUTO_LOAD_WAIT_SEC",
	"LB_BACKEND_WAIT_SEC",
	// логи и трассировка
	"LB_LOG_LEVEL",
	"LB_LOG_BUFFER_ENTRIES",
	"LB_REQUEST_TRACE",
	// таймауты, о которых чаще всего спрашивают
	"LB_REQUEST_TIMEOUT",
	"LB_STREAMING_TIMEOUT_SEC",
	"LB_STREAMING_IDLE_TIMEOUT_SEC",
	"LB_FIRST_BYTE_TIMEOUT_DEFAULT_SEC",
	// порты
	"LB_PORT",
	"LB_API_PORT",
	"LB_OPENAI_PORT",
	// режим
	"LB_OPERATING_MODE",
	"LB_ALGORITHM",
}

// diagFlagsSnapshotR476 — значения заданных флагов + список незаданных.
//
// Разделение важно: «переменная не задана» — это не то же самое, что «задана
// пустой». Оператор должен видеть, что значение придёт из дефолта кода, а не
// из .env, иначе разбор идёт по несуществующей настройке.
func diagFlagsSnapshotR476() map[string]interface{} {
	set := make(map[string]string, len(diagFlagWhitelistR476))
	unset := make([]string, 0, len(diagFlagWhitelistR476))
	for _, key := range diagFlagWhitelistR476 {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			set[key] = v
		} else {
			unset = append(unset, key)
		}
	}
	sort.Strings(unset)
	return map[string]interface{}{
		"set":   set,
		"unset": unset,
		"note":  "unset = переменная не задана в контейнере; действует значение по умолчанию из кода (см. docs/architecture/feature-switches.ru.md)",
	}
}

// handleDiagSummary — GET /api/v1/diag/summary.
func (s *Server) handleDiagSummary(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "GET only"})
		return
	}
	s.writeJSON(w, http.StatusOK, s.diagSummaryPayloadR476())
}

// diagSummaryPayloadR476 — общий сборщик: его отдаёт /api/v1/diag/summary и
// кладёт в архив /api/v1/diag/bundle (одна логика — два потребителя).
func (s *Server) diagSummaryPayloadR476() map[string]interface{} {
	out := map[string]interface{}{
		"generated_at": time.Now().UTC().Format(time.RFC3339),
		"version":      version.Get().Fields(),
		"version_text": version.Get().String(),
		"flags":        diagFlagsSnapshotR476(),
	}
	if s.config != nil {
		// nctxReload — конфигурация окна, всё целиком: так видно preflight,
		// async-режим, потолок и таймауты без выдёргивания по одному полю.
		out["nctx_reload"] = s.config.Balancing.NCtxReload
		out["operating_mode"] = s.config.Balancing.OperatingMode
	}
	if s.proxy != nil {
		out["cluster"] = s.proxy.GetClusterState()
		out["queue_stats"] = s.proxy.GetQueueStats()
		// Потолок окна считается той же формулой, что и в запросах — иначе
		// оператор снова увидит одно число в конфиге и другое в отказе.
		out["nctx_ceiling"] = nctxCeilingFor(s.proxy)
	}
	out["model_profiles"] = diagModelProfilesR476(s)
	// R477: последние сбои по стадиям — ответ на «какой компонент дал сбой».
	// Показываются в WebUI и попадают в архив (failures.json).
	out["recent_failures"] = balancer.RecentFailuresR477()
	return out
}

// diagModelProfilesR476 — профили моделей с действующим окном по каждой.
func diagModelProfilesR476(s *Server) map[string]interface{} {
	models := s.modelProfilesSnapshotR423()
	out := make(map[string]interface{}, len(models))
	for name, profile := range models {
		entry := map[string]interface{}{"profile": profile}
		if s.proxy != nil {
			entry["effective"] = effectiveWindowFor(s.proxy, name)
		}
		out[name] = entry
	}
	return out
}

// handlers_logs_export_r389.go — R389 (2026-11-02): ВЫГРУЗКА ЛОГОВ БАЛАНСЕРА
// файлом из WebUI.
//
// ЗАЧЕМ. В WebUI был только клиентский «Экспорт»: он сохранял то, что браузер
// успел получить по WebSocket /ws/logs с момента открытия страницы. Получить
// ПОЛНЫЙ буфер балансера (включая то, что было до открытия вкладки, и записи до
// переподключения WS) было нельзя — оператору приходилось идти в `docker logs`.
//
// Здесь отдаётся снимок ring-buffer'а логгера (logger.GetBroker().Snapshot())
// файлом: text/plain (по умолчанию) или JSON, с фильтрами и хвостом.
//
// ПОЧЕМУ ЭТО НЕ /ws/logs. WebSocket — live-tail, у него нет ни истории до
// подключения, ни фильтров на сервере, ни способа отдать файл. Экспорт —
// отдельная ручка, чтобы «пришлите логи» решалось одной кнопкой.
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"ollama-loadbalancer/pkg/logger"
)

const (
	// logsExportMaxEntriesR389 — верхняя граница выгрузки (защита от гигантского
	// файла: ring buffer ограничен, но фильтры могут отдавать всё).
	logsExportMaxEntriesR389 = 20000
	// logsExportDefaultFilenameR389 — имя файла, если оператор не задал format.
	logsExportFilePrefixR389 = "balancer-logs"
)

// handleLogsExport — GET /api/v1/logs/export.
//
// Параметры (все необязательные):
//   - format=txt|json (по умолчанию txt);
//   - level=debug|info|warn|error — только этот уровень и выше;
//   - source=<подстрока> — фильтр по источнику (без учёта регистра);
//   - q=<подстрока> — поиск по тексту сообщения;
//   - limit=N — только последние N записей (после фильтров).
func (s *Server) handleLogsExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "GET only"})
		return
	}

	var entries []logger.LogEntry
	if broker := logger.GetBroker(); broker != nil {
		entries = broker.Snapshot()
	}

	q := r.URL.Query()
	level := strings.ToLower(strings.TrimSpace(q.Get("level")))
	source := strings.ToLower(strings.TrimSpace(q.Get("source")))
	needle := strings.ToLower(strings.TrimSpace(q.Get("q")))
	limit := 0
	if raw := strings.TrimSpace(q.Get("limit")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			limit = n
		}
	}

	filtered := make([]logger.LogEntry, 0, len(entries))
	minLevel := logsLevelRankR389(level)
	for _, e := range entries {
		if level != "" && logsLevelRankR389(e.Level) < minLevel {
			continue
		}
		if source != "" && !strings.Contains(strings.ToLower(e.Source), source) {
			continue
		}
		if needle != "" && !strings.Contains(strings.ToLower(e.Message), needle) {
			continue
		}
		filtered = append(filtered, e)
	}
	if limit > 0 && len(filtered) > limit {
		filtered = filtered[len(filtered)-limit:]
	}
	if len(filtered) > logsExportMaxEntriesR389 {
		filtered = filtered[len(filtered)-logsExportMaxEntriesR389:]
	}

	format := strings.ToLower(strings.TrimSpace(q.Get("format")))
	stamp := time.Now().UTC().Format("20060102-150405")

	// Счётчик отданных записей — ДО тела: заголовки после первой записи уже не
	// уходят (тихая потеря, которую легко принять за «фича не работает»).
	w.Header().Set("X-Logs-Exported", strconv.Itoa(len(filtered)))

	switch format {
	case "json":
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Content-Disposition",
			fmt.Sprintf(`attachment; filename="%s-%s.json"`, logsExportFilePrefixR389, stamp))
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if err := enc.Encode(filtered); err != nil {
			logger.Get().Warnw("logs export: json encode failed", "error", err)
		}
	default:
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Disposition",
			fmt.Sprintf(`attachment; filename="%s-%s.txt"`, logsExportFilePrefixR389, stamp))
		for _, e := range filtered {
			src := e.Source
			if src == "" {
				src = "-"
			}
			// Формат строки стабилен и разбирается глазами/awk: время, уровень,
			// источник, сообщение. JSON-тело сообщения не трогаем — как есть.
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
				e.Time.UTC().Format(time.RFC3339Nano),
				strings.ToUpper(e.Level), src, e.Message)
		}
	}
}

// logsLevelRankR389 — сравнение уровней (debug < info < warn < error).
//
// Пустой уровень = «все»; незнакомый = rank info, чтобы фильтр не выбрасывал
// записи из-за опечатки в параметре.
func logsLevelRankR389(level string) int {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		return 10
	case "", "all":
		return 0
	case "info":
		return 20
	case "warn", "warning":
		return 30
	case "error":
		return 40
	default:
		return 20
	}
}

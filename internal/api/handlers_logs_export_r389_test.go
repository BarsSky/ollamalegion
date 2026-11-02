// handlers_logs_export_r389_test.go — R389 (2026-11-02): выгрузка логов
// балансера файлом.
package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/logger"
)

// withTestLogBrokerR389 — подменить глобальный брокер логов на тестовый.
func withTestLogBrokerR389(t *testing.T, entries []logger.LogEntry) {
	t.Helper()
	prev := logger.GetBroker()
	broker := logger.NewLogBroker(256)
	for _, e := range entries {
		broker.Publish(e)
	}
	logger.SetBroker(broker)
	t.Cleanup(func() { logger.SetBroker(prev) })
}

func logsExportTestEntriesR389() []logger.LogEntry {
	base := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
	return []logger.LogEntry{
		{Time: base, Level: "debug", Message: "подробность", Source: "balancer"},
		{Time: base.Add(time.Second), Level: "info", Message: "загружен backend", Source: "balancer"},
		{Time: base.Add(2 * time.Second), Level: "warn", Message: "узел раскладки молчит", Source: "layout"},
		{Time: base.Add(3 * time.Second), Level: "error", Message: `upstream error: "quota" exceeded`, Source: "proxy"},
	}
}

// TestR389_ExportTextAllEntries — обычная выгрузка: файл, все записи, стабильный
// формат строки (время, уровень, источник, сообщение).
func TestR389_ExportTextAllEntries(t *testing.T) {
	entries := logsExportTestEntriesR389()
	withTestLogBrokerR389(t, entries)

	srv := &Server{}
	rec := httptest.NewRecorder()
	srv.handleLogsExport(rec, httptest.NewRequest(http.MethodGet, "/api/v1/logs/export", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d, ожидался 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, ожидался text/plain", ct)
	}
	cd := rec.Header().Get("Content-Disposition")
	if !strings.Contains(cd, "attachment") || !strings.Contains(cd, "balancer-logs-") || !strings.HasSuffix(strings.TrimSpace(cd), `.txt"`) {
		t.Errorf("Content-Disposition = %q — не файл .txt для скачивания", cd)
	}
	lines := strings.Split(strings.TrimSpace(rec.Body.String()), "\n")
	if len(lines) != len(entries) {
		t.Fatalf("строк %d, ожидалось %d:\n%s", len(lines), len(entries), rec.Body.String())
	}
	// Строка 2: info-сообщение; проверяем все четыре поля.
	fields := strings.Split(lines[1], "\t")
	if len(fields) != 4 {
		t.Fatalf("полей в строке %d, ожидалось 4 (время, уровень, источник, сообщение): %q", len(fields), lines[1])
	}
	if fields[1] != "INFO" || fields[2] != "balancer" || fields[3] != "загружен backend" {
		t.Errorf("поля строки не те: %q", lines[1])
	}
	if got := rec.Header().Get("X-Logs-Exported"); got != "4" {
		t.Errorf("X-Logs-Exported = %q, ожидалось 4", got)
	}
}

// TestR389_ExportFiltersAndLimit — фильтры уровня/источника/поиска и хвост.
func TestR389_ExportFiltersAndLimit(t *testing.T) {
	withTestLogBrokerR389(t, logsExportTestEntriesR389())
	srv := &Server{}

	t.Run("уровень warn и выше", func(t *testing.T) {
		rec := httptest.NewRecorder()
		srv.handleLogsExport(rec, httptest.NewRequest(http.MethodGet, "/api/v1/logs/export?level=warn", nil))
		body := rec.Body.String()
		if strings.Contains(body, "подробность") || strings.Contains(body, "загружен backend") {
			t.Fatalf("фильтр по уровню пропустил debug/info:\n%s", body)
		}
		if !strings.Contains(body, "узел раскладки молчит") || !strings.Contains(body, "quota") {
			t.Fatalf("фильтр выбросил warn/error:\n%s", body)
		}
	})

	t.Run("источник", func(t *testing.T) {
		rec := httptest.NewRecorder()
		srv.handleLogsExport(rec, httptest.NewRequest(http.MethodGet, "/api/v1/logs/export?source=proxy", nil))
		body := rec.Body.String()
		if !strings.Contains(body, "quota") || strings.Contains(body, "загружен backend") {
			t.Fatalf("фильтр по источнику не сработал:\n%s", body)
		}
	})

	t.Run("поиск по тексту", func(t *testing.T) {
		rec := httptest.NewRecorder()
		srv.handleLogsExport(rec, httptest.NewRequest(http.MethodGet, "/api/v1/logs/export?q=узел", nil))
		body := rec.Body.String()
		if !strings.Contains(body, "узел раскладки молчит") || strings.Contains(body, "quota") {
			t.Fatalf("поиск по тексту не сработал:\n%s", body)
		}
	})

	t.Run("limit — последние записи", func(t *testing.T) {
		rec := httptest.NewRecorder()
		srv.handleLogsExport(rec, httptest.NewRequest(http.MethodGet, "/api/v1/logs/export?limit=2", nil))
		lines := strings.Split(strings.TrimSpace(rec.Body.String()), "\n")
		if len(lines) != 2 {
			t.Fatalf("строк %d, ожидалось 2 (хвост):\n%s", len(lines), rec.Body.String())
		}
		if !strings.Contains(lines[0], "узел раскладки молчит") {
			t.Errorf("limit отдал не последние записи: %q", lines[0])
		}
	})
}

// TestR389_ExportJSON — JSON-формат пригоден для машинной обработки и сохраняет
// кавычки в сообщении (частая причина «в логах не видно текста»).
func TestR389_ExportJSON(t *testing.T) {
	withTestLogBrokerR389(t, logsExportTestEntriesR389())
	srv := &Server{}

	rec := httptest.NewRecorder()
	srv.handleLogsExport(rec, httptest.NewRequest(http.MethodGet, "/api/v1/logs/export?format=json", nil))

	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, ожидался application/json", ct)
	}
	var out []logger.LogEntry
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("тело не JSON-массив: %v\n%s", err, rec.Body.String())
	}
	if len(out) != 4 {
		t.Fatalf("записей %d, ожидалось 4", len(out))
	}
	if !strings.Contains(out[3].Message, `"quota"`) {
		t.Errorf("кавычки в сообщении потерялись: %q", out[3].Message)
	}
}

// TestR389_ExportEmptyBroker — брокера нет (лёгкая сборка/тесты): пустой файл,
// а не 500 и не паника.
func TestR389_ExportEmptyBroker(t *testing.T) {
	prev := logger.GetBroker()
	logger.SetBroker(nil)
	t.Cleanup(func() { logger.SetBroker(prev) })

	srv := &Server{}
	rec := httptest.NewRecorder()
	srv.handleLogsExport(rec, httptest.NewRequest(http.MethodGet, "/api/v1/logs/export", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d, ожидался 200 с пустым файлом", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("ожидался пустой файл, получено: %q", rec.Body.String())
	}
}

// TestR389_ExportMethodNotAllowed — ручка только для чтения.
func TestR389_ExportMethodNotAllowed(t *testing.T) {
	srv := &Server{}
	rec := httptest.NewRecorder()
	srv.handleLogsExport(rec, httptest.NewRequest(http.MethodPost, "/api/v1/logs/export", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("HTTP %d, ожидался 405", rec.Code)
	}
}

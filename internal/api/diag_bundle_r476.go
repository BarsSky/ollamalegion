// diag_bundle_r476.go — R476 (2026-11-07): один архив со стенда для разбора.
//
// ЗАЧЕМ. «Пришлите логи» на практике означало несколько сообщений: логи
// балансера, сводку хода с воркера, промпт, тело запроса, конфигурацию окна.
// Часть этого лежала в docker, часть — в памяти процесса, часть — в файлах
// /app/diag. Здесь всё собирается одним запросом и отдаётся zip-файлом,
// который скачивается кнопкой из WebUI.
//
// СОСТАВ АРХИВА:
//
//	summary.json          — то же, что /api/v1/diag/summary (версии, флаги,
//	                        окно, профили, снимок кластера)
//	balancer-logs.txt     — кольцевой буфер логов балансера (LB_LOG_BUFFER_ENTRIES)
//	flags.txt             — флаги по белому списку: заданные и незаданные
//	backends/<id>.zip     — диагностический архив воркера (R476)
//	backends/<id>.info.json — /api/info воркера (сборка, GPU, модели)
//	backends/<id>.error.txt — если воркер не ответил: текст ошибки
//	README.txt            — что это и в каком порядке читать
//
// СЕКРЕТЫ: в summary.txt-only флаги (белый список), в логи секретов не пишем;
// токены воркеров используются только как заголовок запроса и в архив не
// попадают.
package api

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

const (
	// diagBundlePerBackendTimeoutR476 — сколько ждать один воркер. Загрузка
	// модели не мешает: эндпоинты диагностики не ждут её.
	diagBundlePerBackendTimeoutR476 = 20 * time.Second
	// diagBundleEntryMaxR476 — предел на вложенный архив/файл.
	diagBundleEntryMaxR476 = 16 << 20
)

// handleDiagBundle — GET /api/v1/diag/bundle.
func (s *Server) handleDiagBundle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "GET only"})
		return
	}

	host, _ := os.Hostname()
	if strings.TrimSpace(host) == "" {
		host = "balancer"
	}
	stamp := time.Now().UTC().Format("20060102-150405")
	name := fmt.Sprintf("ollamalegion-diag-%s-%s.zip", host, stamp)

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)

	// R476: `?since=30m` | RFC3339 — только записи после этого момента (свои логи
	// фильтруем здесь, параметр передаём воркеру). Нужно, чтобы архив по живому
	// дефекту не тащил сутки логов.
	sinceRaw := strings.TrimSpace(r.URL.Query().Get("since"))
	cutoff := diagBundleSinceCutoffR476(sinceRaw)

	zw := zip.NewWriter(w)
	defer func() { _ = zw.Close() }()

	// 1) Сводка: тот же JSON, что отдаёт /api/v1/diag/summary.
	s.diagBundleAddJSONR476(zw, "summary.json", s.diagSummaryPayloadR476())

	// R477: сбои по стадиям отдельным файлом — их читают первыми при разборе.
	s.diagBundleAddJSONR476(zw, "failures.json", balancer.RecentFailuresR477())

	// 2) Флаги отдельным текстовым файлом: удобно приложить к переписке.
	s.diagBundleAddTextR476(zw, "flags.txt", diagBundleFlagsTextR476())

	// 3) Логи балансера из кольцевого буфера.
	s.diagBundleAddTextR476(zw, "balancer-logs.txt", diagBundleBalancerLogsSinceR476(cutoff))

	// 4) По каждому бэкенду — диагностика воркера, если это cppworker.
	backends := s.diagBundleBackendsR476()
	for _, b := range backends {
		s.diagBundleAddBackendR476(zw, b, sinceRaw)
	}

	s.diagBundleAddTextR476(zw, "README.txt", diagBundleReadmeR476)
	logger.Get().Infow("diag/bundle: архив отдан",
		"file", name, "backends", len(backends), "remote", r.RemoteAddr)
}

// diagBundleBackendsR476 — список бэкендов для обхода (по id).
func (s *Server) diagBundleBackendsR476() []types.Backend {
	if s.proxy == nil {
		return nil
	}
	all := s.proxy.GetAllBackends()
	sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })
	return all
}

// diagBundleAddBackendR476 — вложенный архив и /api/info одного бэкенда.
func (s *Server) diagBundleAddBackendR476(zw *zip.Writer, b types.Backend, sinceRaw string) {
	base := ""
	if s.proxy != nil {
		base = s.proxy.BackendHTTPBaseURLR476(b)
	}
	if base == "" {
		base = fmt.Sprintf("http://%s:%d", b.Host, b.OllamaPort)
	}

	// /api/info есть у cppworker и у ollama — забираем у любого, ошибку пишем
	// файлом: «не ответил» — это тоже факт для разбора.
	if body, err := diagBundleHTTPGetR476(base+"/api/info", b.CppWorkerApiToken); err == nil {
		s.diagBundleAddTextR476(zw, fmt.Sprintf("backends/%s.info.json", b.ID), string(body))
	} else {
		s.diagBundleAddTextR476(zw, fmt.Sprintf("backends/%s.info.error.txt", b.ID), err.Error()+"\n")
	}

	if b.Type != types.BackendTypeLlamaCpp {
		s.diagBundleAddTextR476(zw, fmt.Sprintf("backends/%s.note.txt", b.ID),
			"бэкенд не cppworker (тип "+string(b.Type)+"): диагностический архив воркера недоступен\n")
		return
	}
	bundleURL := base + "/api/v1/cppworker/debug/bundle"
	if strings.TrimSpace(sinceRaw) != "" {
		bundleURL += "?since=" + url.QueryEscape(sinceRaw)
	}
	body, err := diagBundleHTTPGetR476(bundleURL, b.CppWorkerApiToken)
	if err != nil {
		s.diagBundleAddTextR476(zw, fmt.Sprintf("backends/%s.error.txt", b.ID), err.Error()+"\n")
		return
	}
	if len(body) > diagBundleEntryMaxR476 {
		body = body[:diagBundleEntryMaxR476]
	}
	entry, createErr := zw.Create(fmt.Sprintf("backends/%s.bundle.zip", b.ID))
	if createErr != nil {
		logger.Get().Warnw("diag/bundle: вложенный архив не создан", "backend", b.ID, "error", createErr)
		return
	}
	if _, writeErr := entry.Write(body); writeErr != nil {
		logger.Get().Warnw("diag/bundle: вложенный архив не записан", "backend", b.ID, "error", writeErr)
	}
}

// diagBundleHTTPGetR476 — GET с коротким таймаутом и токеном воркера.
//
// Зачем свой клиент: диагностика не должна ни висеть минутами, ни тянуть
// соединения из общего пула с рабочими таймаутами.
func diagBundleHTTPGetR476(url, token string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), diagBundlePerBackendTimeoutR476)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(token) != "" {
		req.Header.Set("X-API-Token", token)
	}
	client := &http.Client{Timeout: diagBundlePerBackendTimeoutR476}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	limited := http.MaxBytesReader(nil, resp.Body, diagBundleEntryMaxR476)
	body := make([]byte, 0, 64*1024)
	buf := make([]byte, 32*1024)
	for {
		n, readErr := limited.Read(buf)
		if n > 0 {
			body = append(body, buf[:n]...)
		}
		if readErr != nil {
			break
		}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("backend %s вернул %d: %s", url, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, nil
}

// diagBundleFlagsTextR476 — флаги «задано/не задано» в виде текста.
func diagBundleFlagsTextR476() string {
	snap := diagFlagsSnapshotR476()
	set, _ := snap["set"].(map[string]string)
	unset, _ := snap["unset"].([]string)
	var b strings.Builder
	b.WriteString("# Диагностические флаги балансера (R476)\n")
	b.WriteString("# unset = переменная не задана; действует значение по умолчанию из кода\n\n")
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteString(k + "=" + set[k] + "\n")
	}
	for _, k := range unset {
		b.WriteString(k + "= (unset)\n")
	}
	return b.String()
}

// diagBundleBalancerLogsR476 — снимок кольцевого буфера логов.
func diagBundleBalancerLogsR476() string {
	return diagBundleBalancerLogsSinceR476(time.Time{})
}

// diagBundleBalancerLogsSinceR476 — только записи позже cutoff (нулевое время =
// без фильтра).
func diagBundleBalancerLogsSinceR476(cutoff time.Time) string {
	broker := logger.GetBroker()
	if broker == nil {
		return "# кольцевой буфер логов не включён (LB_LOG_BUFFER_ENTRIES=0)\n"
	}
	entries := broker.Snapshot()
	var b strings.Builder
	for _, e := range entries {
		if !cutoff.IsZero() && e.Time.Before(cutoff) {
			continue
		}
		src := e.Source
		if src == "" {
			src = "balancer"
		}
		b.WriteString(fmt.Sprintf("%s %-5s %-9s %s\n",
			e.Time.Format("2006-01-02T15:04:05.000Z07:00"),
			strings.ToUpper(e.Level), src, e.Message))
	}
	if b.Len() == 0 {
		return "# буфер пуст\n"
	}
	return b.String()
}

// diagBundleSinceCutoffR476 — разобрать `since`: длительность ("30m", "2h") или
// RFC3339. Пустое/мусор = без фильтра: «не понял параметр» не должно молча
// отдавать пустой архив.
func diagBundleSinceCutoffR476(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	if d, err := time.ParseDuration(raw); err == nil && d > 0 {
		return time.Now().Add(-d)
	}
	if ts, err := time.Parse(time.RFC3339, raw); err == nil {
		return ts
	}
	logger.Get().Warnw("diag/bundle: since не разобран, фильтр не применён", "since", raw)
	return time.Time{}
}

// diagBundleAddJSONR476 — JSON-файл архива.
func (s *Server) diagBundleAddJSONR476(zw *zip.Writer, name string, value interface{}) {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		s.diagBundleAddTextR476(zw, name+".error.txt", err.Error()+"\n")
		return
	}
	s.diagBundleAddTextR476(zw, name, string(raw)+"\n")
}

// diagBundleAddTextR476 — текстовый файл архива (ошибки пишем в лог, но архив
// не роняем: лучше неполный архив, чем никакого).
func (s *Server) diagBundleAddTextR476(zw *zip.Writer, name, text string) {
	f, err := zw.Create(name)
	if err != nil {
		logger.Get().Warnw("diag/bundle: файл не создан", "file", name, "error", err)
		return
	}
	if len(text) > diagBundleEntryMaxR476 {
		text = text[:diagBundleEntryMaxR476] + "\n\n# ОБРЕЗАНО: файл больше лимита\n"
	}
	if _, err := f.Write([]byte(text)); err != nil {
		logger.Get().Warnw("diag/bundle: файл не записан", "file", name, "error", err)
	}
}

// diagBundleReadmeR476 — как читать архив.
const diagBundleReadmeR476 = `Диагностический архив OllamaLegion (R476).

Состав:
  summary.json        — версии всех компонентов, флаги, конфигурация окна,
                        профили моделей, снимок кластера (бэкенды, модели, очередь)
  balancer-logs.txt   — кольцевой буфер логов балансера
  flags.txt           — флаги по белому списку (set / unset)
  backends/<id>.bundle.zip — архив воркера: его логи, промпт, тело запроса,
                        срывы разбора вызовов, env-флаги (см. README в нём)
  backends/<id>.info.json  — /api/info воркера
  backends/<id>.error.txt  — воркер не ответил (текст ошибки)

Как связать ход клиента с логами:
  1) в balancer-logs.txt найдите строку "request trace" нужного времени;
     у неё есть request_id, path, status, duration_ms, backend;
  2) по этому request_id в backends/<id>.bundle.zip (logs.txt) найдите
     "/api/chat stream response summary" — там streamed_content_bytes,
     emitted_len, final_content_len, tool_calls, done_reason, generated_tokens,
     budget_exhausted и dumped_args_instead_of_call.

Чего в архиве НЕТ: токенов и паролей.
`

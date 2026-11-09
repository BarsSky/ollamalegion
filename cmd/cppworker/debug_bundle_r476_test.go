//go:build llama_stub

// debug_bundle_r476_test.go — R476 (2026-11-07): архив воркера.
//
// Проверяем то, ради чего архив делался: логи процесса в нём есть, промпт и
// тело запроса попадают из файлов, а секретов в env.json нет.
package main

import (
	"archive/zip"
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/logger"
)

// bundleEntriesR476 — прочитать zip из ответа в map «имя → содержимое».
func bundleEntriesR476(t *testing.T, raw []byte) map[string]string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("ответ не zip: %v", err)
	}
	out := make(map[string]string, len(zr.File))
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("открыть %s: %v", f.Name, err)
		}
		body, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatalf("прочитать %s: %v", f.Name, err)
		}
		out[f.Name] = string(body)
	}
	return out
}

// TestR476_WorkerBundleHasLogsInfoAndReadme — архив содержит сводку, логи и
// инструкцию; логи берутся из кольцевого буфера процесса.
func TestR476_WorkerBundleHasLogsInfoAndReadme(t *testing.T) {
	b := logger.NewLogBroker(100)
	logger.SetBroker(b)
	defer logger.SetBroker(nil)
	logger.SetBrokerSource("cppworker")
	b.Publish(logger.LogEntry{Level: "info", Source: "cppworker",
		Message: "stream response summary request_id=abc123 tool_calls=1"})

	rec := httptest.NewRecorder()
	handleDebugBundle(rec, httptest.NewRequest(http.MethodGet, "/api/v1/cppworker/debug/bundle", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/zip" {
		t.Fatalf("Content-Type = %q", ct)
	}
	entries := bundleEntriesR476(t, rec.Body.Bytes())

	for _, name := range []string{"info.json", "logs.txt", "env.json", "README.txt"} {
		if _, ok := entries[name]; !ok {
			t.Fatalf("в архиве нет %s (есть: %v)", name, keysR476(entries))
		}
	}
	if !strings.Contains(entries["logs.txt"], "request_id=abc123") {
		t.Fatalf("в logs.txt нет записи буфера:\n%s", entries["logs.txt"])
	}
	if !strings.Contains(entries["logs.txt"], "INFO") {
		t.Fatalf("в logs.txt нет уровня:\n%s", entries["logs.txt"])
	}
}

// TestR476_WorkerBundleExcludesSecrets — env.json по белому списку.
func TestR476_WorkerBundleExcludesSecrets(t *testing.T) {
	t.Setenv("CPPWORKER_API_TOKEN", "worker-secret-token")
	t.Setenv("CPPWORKER_LOG_LEVEL", "debug")

	snap := bundleEnvSnapshotR476()
	if snap["CPPWORKER_LOG_LEVEL"] != "debug" {
		t.Fatalf("флаг логирования не попал в снимок: %v", snap)
	}
	for key, value := range snap {
		if strings.Contains(value, "worker-secret-token") || strings.Contains(key, "TOKEN") {
			t.Fatalf("секрет в снимке env: %s=%s", key, value)
		}
	}
}

// TestR476_WorkerBundleIncludesDebugFiles — промпт и тело запроса попадают в
// архив, если включены CPPWORKER_DEBUG_* флаги; без них появляется .missing.txt,
// а не тишина.
func TestR476_WorkerBundleIncludesDebugFiles(t *testing.T) {
	dir := t.TempDir()
	promptPath := filepath.Join(dir, "prompt-last.txt")
	if err := os.WriteFile(promptPath, []byte("PROMPT-CONTENT-476"), 0o600); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	t.Setenv("CPPWORKER_DEBUG_PROMPT_FILE", promptPath)
	t.Setenv("CPPWORKER_DEBUG_REQUEST_FILE", filepath.Join(dir, "nope.json"))

	rec := httptest.NewRecorder()
	handleDebugBundle(rec, httptest.NewRequest(http.MethodGet, "/api/v1/cppworker/debug/bundle", nil))
	entries := bundleEntriesR476(t, rec.Body.Bytes())

	if !strings.Contains(entries["prompt-last.txt"], "PROMPT-CONTENT-476") {
		t.Fatalf("промпт не попал в архив: %v", keysR476(entries))
	}
	if _, ok := entries["request-last.json.missing.txt"]; !ok {
		t.Fatalf("отсутствующий файл не отмечен: %v", keysR476(entries))
	}
}

// TestR476_LogBufferCanBeDisabled — CPPWORKER_LOG_BUFFER_ENTRIES=0 выключает
// буфер: диагностика не должна незаметно занимать память, если оператор её
// выключил.
func TestR476_LogBufferCanBeDisabled(t *testing.T) {
	logger.SetBroker(nil)
	t.Setenv("CPPWORKER_LOG_BUFFER_ENTRIES", "0")
	initLogBufferR476()
	if logger.GetBroker() != nil {
		t.Fatal("буфер включён при CPPWORKER_LOG_BUFFER_ENTRIES=0")
	}
	if !strings.Contains(bundleLogsTextR476(), "не включён") {
		t.Fatalf("нет честной пометки об отключённом буфере: %s", bundleLogsTextR476())
	}
}

func keysR476(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

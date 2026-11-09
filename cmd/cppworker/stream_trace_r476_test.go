//go:build llama_stub

// stream_trace_r476_test.go — R476 (2026-11-07): доказательство отдачи стрима
// и ротация дампов диагностики.
//
// ЗАЧЕМ. Спор «клиент получил неполный ответ» без побайтового доказательства
// решается догадкой. CPPWORKER_STREAM_TRACE=1 даёт чанки, байты и sha256
// записанного. CPPWORKER_DIAG_KEEP_FILES=N сохраняет прошлые версии
// prompt-last.txt/request-last.json, иначе дефект «ход назад» уже не разобрать.
package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/logger"
)

// TestR476_StreamTraceCountsChunksAndHash — включённая трассировка считает
// чанки, байты и хеш ровно записанного.
func TestR476_StreamTraceCountsChunksAndHash(t *testing.T) {
	t.Setenv("CPPWORKER_STREAM_TRACE", "1")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/chat", nil)
	sw := newSafeStreamWriter(rec, req, "test", "m")

	if !sw.Writef("%s", "abc") {
		t.Fatal("первый чанк не записан")
	}
	if !sw.Write([]byte("def")) {
		t.Fatal("второй чанк не записан")
	}

	chunks, bytesCount, sha := sw.StreamTrace()
	if chunks != 2 {
		t.Fatalf("chunks = %d, ожидалось 2", chunks)
	}
	if bytesCount != 6 {
		t.Fatalf("bytes = %d, ожидалось 6", bytesCount)
	}
	want := sha256.Sum256([]byte("abcdef"))
	if sha != hex.EncodeToString(want[:]) {
		t.Fatalf("sha256 = %s, ожидался хеш от 'abcdef'", sha)
	}
}

// TestR476_StreamTraceOffByDefault — без флага трассировка не тратит ресурсы.
func TestR476_StreamTraceOffByDefault(t *testing.T) {
	os.Unsetenv("CPPWORKER_STREAM_TRACE")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/chat", nil)
	sw := newSafeStreamWriter(rec, req, "test", "m")
	sw.Writef("%s", "abc")

	chunks, bytesCount, sha := sw.StreamTrace()
	if chunks != 0 || bytesCount != 0 || sha != "" {
		t.Fatalf("трассировка включилась без флага: chunks=%d bytes=%d sha=%q", chunks, bytesCount, sha)
	}
}

// TestR476_DiagKeepFilesRotates — прошлые версии дампа не затираются.
func TestR476_DiagKeepFilesRotates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "prompt-last.txt")
	t.Setenv("CPPWORKER_DIAG_KEEP_FILES", "2")

	writeFileAtomicR442(path, []byte("first"))
	writeFileAtomicR442(path, []byte("second"))
	writeFileAtomicR442(path, []byte("third"))

	if got := readFileR476(t, path); got != "third" {
		t.Fatalf("текущий дамп = %q, ожидался third", got)
	}
	if got := readFileR476(t, path+".1"); got != "second" {
		t.Fatalf(".1 = %q, ожидался second", got)
	}
	if got := readFileR476(t, path+".2"); got != "first" {
		t.Fatalf(".2 = %q, ожидался first", got)
	}
}

// TestR476_DiagKeepFilesDisabledByDefault — прежнее поведение без флага:
// остаётся только последний файл.
func TestR476_DiagKeepFilesDisabledByDefault(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "request-last.json")
	os.Unsetenv("CPPWORKER_DIAG_KEEP_FILES")

	writeFileAtomicR442(path, []byte("one"))
	writeFileAtomicR442(path, []byte("two"))

	if got := readFileR476(t, path); got != "two" {
		t.Fatalf("текущий дамп = %q", got)
	}
	if _, err := os.Stat(path + ".1"); err == nil {
		t.Fatal("создана ротация при выключенном CPPWORKER_DIAG_KEEP_FILES")
	}
}

// TestR476_BundleSinceFiltersLogs — ?since= отсекает старые записи буфера,
// иначе архив по живому дефекту тащит сутки логов.
func TestR476_BundleSinceFiltersLogs(t *testing.T) {
	b := logger.NewLogBroker(50)
	logger.SetBroker(b)
	defer logger.SetBroker(nil)
	logger.SetBrokerSource("cppworker")

	b.Publish(logger.LogEntry{Time: time.Now().Add(-2 * time.Hour), Level: "info",
		Message: "OLD-ENTRY-476"})
	b.Publish(logger.LogEntry{Time: time.Now(), Level: "info", Message: "NEW-ENTRY-476"})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/cppworker/debug/bundle?since=30m", nil)
	handleDebugBundle(rec, req)

	entries := bundleEntriesR476(t, rec.Body.Bytes())
	logs, ok := entries["logs.txt"]
	if !ok {
		t.Fatalf("в архиве нет logs.txt: %v", keysR476(entries))
	}
	if strings.Contains(logs, "OLD-ENTRY-476") {
		t.Fatalf("двухчасовая запись не отфильтрована: %s", logs)
	}
	if !strings.Contains(logs, "NEW-ENTRY-476") {
		t.Fatalf("свежая запись потеряна: %s", logs)
	}
}

// TestR476_BundleSinceGarbageKeepsAll — непонятный since не должен молча
// отдавать пустой архив.
func TestR476_BundleSinceGarbageKeepsAll(t *testing.T) {
	if !bundleSinceCutoffR476("не-время").IsZero() {
		t.Fatal("мусор в since превратился в фильтр — архив станет пустым без объяснения")
	}
	if !bundleSinceCutoffR476("").IsZero() {
		t.Fatal("пустой since должен означать «без фильтра»")
	}
	if d := time.Since(bundleSinceCutoffR476("30m")); d < 29*time.Minute || d > 31*time.Minute {
		t.Fatalf("since=30m дал cutoff %v назад", d)
	}
}

// TestR476_BundleLogBufferAlias — CPPWORKER_DIAG_RING работает, если основная
// переменная не задана.
func TestR476_BundleLogBufferAlias(t *testing.T) {
	logger.SetBroker(nil)
	os.Unsetenv("CPPWORKER_LOG_BUFFER_ENTRIES")
	t.Setenv("CPPWORKER_DIAG_RING", "5")
	initLogBufferR476()
	if logger.GetBroker() == nil {
		t.Fatal("псевдоним CPPWORKER_DIAG_RING не включил буфер")
	}
}

// TestR476_BundleMainVariableWins — если заданы обе, работает основная.
func TestR476_BundleMainVariableWins(t *testing.T) {
	logger.SetBroker(nil)
	t.Setenv("CPPWORKER_LOG_BUFFER_ENTRIES", "0")
	t.Setenv("CPPWORKER_DIAG_RING", "5")
	initLogBufferR476()
	if logger.GetBroker() != nil {
		t.Fatal("псевдоним перебил основную переменную: буфер включился при CPPWORKER_LOG_BUFFER_ENTRIES=0")
	}
}

func readFileR476(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("чтение %s: %v", path, err)
	}
	return string(data)
}

// bundleEntriesRawR476 — запасной помощник для будущих проверок архива.
var _ = zip.NewReader
var _ = io.ReadAll
var _ = bytes.NewReader

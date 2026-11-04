//go:build llama_stub

// unparsed_capture_r427_test.go — R427 (2026-11-04): диагностика нераспознанных
// вызовов инструментов.
//
// Живой случай, ради которого это сделано: сессия Cline упёрлась в ход, который
// cppworker закрыл ошибкой «model returned a tool call that could not be parsed
// into tool_calls», ответ модели был выброшен, а понять, что она выдала, уже
// нельзя — preview вытеснялся из docker logs.
//
// Контракт:
//  1. срыв сохраняется в память (кольцо), в jsonl-строку и в файл с ПОЛНЫМ
//     сырым выводом (head/tail в памяти обрезаны, файл — нет);
//  2. в записи есть всё для разбора: модель, причина, длины, число инструментов;
//  3. endpoint отдаёт записи и умеет очищать кольцо (?clear=1);
//  4. при недоступном пути записи функция НЕ падает (диагностика не должна
//     ломать инференс).

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func resetUnparsedRing() {
	unparsedMu.Lock()
	unparsedRing = nil
	unparsedMu.Unlock()
}

// TestR427_CapturePersistsRawAndMeta — запись срыва в память + файлы.
func TestR427_CapturePersistsRawAndMeta(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "unparsed.jsonl")
	t.Setenv("CPPWORKER_UNPARSED_CAPTURE_FILE", path)
	resetUnparsedRing()

	raw := strings.Repeat("A", 100) + `<|tool_call>{"name":"editor","arguments":` + strings.Repeat("B", 5000)
	parsed := raw
	tools := []openAITool{
		{Type: "function", Function: openAIFunction{Name: "read_files"}},
		{Type: "function", Function: openAIFunction{Name: "editor"}},
	}

	recordUnparsedCapture("test-model", "/api/chat", "markup_unparsed", raw, parsed, 42, 777, 8192, tools)

	unparsedMu.Lock()
	if len(unparsedRing) != 1 {
		unparsedMu.Unlock()
		t.Fatalf("в кольце %d записей, ожидалась 1", len(unparsedRing))
	}
	cap := unparsedRing[0]
	unparsedMu.Unlock()

	if cap.Model != "test-model" || cap.Reason != "markup_unparsed" {
		t.Errorf("метаданные неполные: %+v", cap)
	}
	if cap.RawLen != len(raw) {
		t.Errorf("raw_len = %d, ожидалось %d", cap.RawLen, len(raw))
	}
	if cap.ToolsCount != 2 || len(cap.ToolNames) != 2 {
		t.Errorf("инструменты не записаны: %+v", cap)
	}
	if cap.GeneratedTokens != 777 || cap.NPredict != 8192 || cap.EmittedLen != 42 {
		t.Errorf("числа бюджета не записаны: gen=%d n_predict=%d emitted=%d", cap.GeneratedTokens, cap.NPredict, cap.EmittedLen)
	}
	if len(cap.RawHead) > unparsedHeadChars || len(cap.RawTail) > unparsedTailChars {
		t.Errorf("head/tail не обрезаны: %d/%d", len(cap.RawHead), len(cap.RawTail))
	}
	if cap.RawFile == "" {
		t.Fatal("полный сырой вывод не сохранён в файл")
	}
	full, err := os.ReadFile(cap.RawFile)
	if err != nil {
		t.Fatalf("RAW-файл: %v", err)
	}
	if !strings.Contains(string(full), "=== FULL OUTPUT ===") || !strings.Contains(string(full), strings.Repeat("B", 100)) {
		t.Errorf("RAW-файл не содержит полного вывода (len=%d)", len(full))
	}

	// jsonl-строка тоже записана и разбирается.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("jsonl: %v", err)
	}
	var fromFile unparsedCapture
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &fromFile); err != nil {
		t.Fatalf("jsonl не разбирается: %v\n%s", err, string(data))
	}
	if fromFile.Reason != "markup_unparsed" || fromFile.RawLen != len(raw) {
		t.Errorf("jsonl-запись расходится с памятью: %+v", fromFile)
	}
}

// TestR427_RingKeepsLastN — кольцо не растёт бесконечно.
func TestR427_RingKeepsLastN(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CPPWORKER_UNPARSED_CAPTURE_FILE", filepath.Join(dir, "u.jsonl"))
	resetUnparsedRing()

	for i := 0; i < unparsedRingMax+3; i++ {
		recordUnparsedCapture("m", "/api/chat", "markup_unparsed", "raw", "raw", 0, 0, 0, nil)
	}
	unparsedMu.Lock()
	n := len(unparsedRing)
	unparsedMu.Unlock()
	if n != unparsedRingMax {
		t.Fatalf("в кольце %d записей, ожидалось %d", n, unparsedRingMax)
	}
}

// TestR427_EndpointReturnsAndClears — endpoint отдаёт записи и очищает кольцо.
func TestR427_EndpointReturnsAndClears(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CPPWORKER_UNPARSED_CAPTURE_FILE", filepath.Join(dir, "u.jsonl"))
	resetUnparsedRing()
	recordUnparsedCapture("m", "/api/chat", "markup_stripped", "raw1", "raw1", 1, 2, 3, nil)
	recordUnparsedCapture("m", "/api/chat", "markup_unparsed", "raw2", "raw2", 1, 2, 3, nil)

	rec := httptest.NewRecorder()
	handleDebugUnparsed(rec, httptest.NewRequest(http.MethodGet, "/api/v1/cppworker/debug/unparsed", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d: %s", rec.Code, rec.Body.String())
	}
	// R429: поля с указателями (строки, срез) — первыми (govet fieldalignment).
	var resp struct {
		File  string            `json:"file"`
		Items []unparsedCapture `json:"items"`
		Count int               `json:"count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("ответ не JSON: %v", err)
	}
	if resp.Count != 2 || len(resp.Items) != 2 {
		t.Fatalf("count=%d items=%d, ожидалось 2", resp.Count, len(resp.Items))
	}
	if resp.Items[0].Reason != "markup_unparsed" {
		t.Errorf("свежие записи должны идти первыми, первая = %q", resp.Items[0].Reason)
	}
	if resp.File == "" {
		t.Error("путь к файлу диагностики не отдан")
	}

	rec = httptest.NewRecorder()
	handleDebugUnparsed(rec, httptest.NewRequest(http.MethodGet, "/api/v1/cppworker/debug/unparsed?clear=1", nil))
	var cleared struct {
		Count int `json:"count"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &cleared)
	if cleared.Count != 2 {
		t.Errorf("?clear=1 должен вернуть записи перед очисткой, вернул %d", cleared.Count)
	}
	unparsedMu.Lock()
	left := len(unparsedRing)
	unparsedMu.Unlock()
	if left != 0 {
		t.Errorf("после ?clear=1 в кольце осталось %d", left)
	}
}

// TestR427_UnwritablePathDoesNotPanic — диагностика не ломает инференс.
func TestR427_UnwritablePathDoesNotPanic(t *testing.T) {
	t.Setenv("CPPWORKER_UNPARSED_CAPTURE_FILE", filepath.Join(t.TempDir(), "no-such-dir", "deep", "u.jsonl"))
	resetUnparsedRing()
	// Не должно паниковать; запись в память остаётся (файл может не создаться).
	recordUnparsedCapture("m", "/api/chat", "markup_unparsed", "raw", "raw", 0, 0, 0, nil)
	unparsedMu.Lock()
	n := len(unparsedRing)
	unparsedMu.Unlock()
	if n != 1 {
		t.Fatalf("в кольце %d записей, ожидалась 1", n)
	}
}

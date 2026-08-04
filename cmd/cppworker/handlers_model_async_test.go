package main

// ============================================================
// Tests for Round 24 (2026-08-04) async/dynamic load timeout
// ============================================================

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestParseLoadWaitParams_Defaults — без query params: async, timeout=60s.
func TestParseLoadWaitParams_Defaults(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/api/models/load", nil)
	wait, ms := parseLoadWaitParams(r)
	if wait {
		t.Errorf("default wait: got true, want false (async by default in Round 24)")
	}
	if ms != 60000 {
		t.Errorf("default waitTimeoutMs: got %d, want 60000", ms)
	}
}

// TestParseLoadWaitParams_WaitTrue — ?wait=true → sync, default 60s.
func TestParseLoadWaitParams_WaitTrue(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/api/models/load?wait=true", nil)
	wait, ms := parseLoadWaitParams(r)
	if !wait {
		t.Errorf("wait=true: got false, want true (sync mode)")
	}
	if ms != 60000 {
		t.Errorf("default waitTimeoutMs: got %d, want 60000", ms)
	}
}

// TestParseLoadWaitParams_WaitTrueWithTimeout — ?wait=true&waitTimeoutSec=120.
func TestParseLoadWaitParams_WaitTrueWithTimeout(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost,
		"/api/models/load?wait=true&waitTimeoutSec=120", nil)
	wait, ms := parseLoadWaitParams(r)
	if !wait {
		t.Errorf("wait=true: got false, want true")
	}
	if ms != 120000 {
		t.Errorf("waitTimeoutMs: got %d, want 120000", ms)
	}
}

// TestParseLoadWaitParams_WaitFalse — ?wait=false → async.
func TestParseLoadWaitParams_WaitFalse(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/api/models/load?wait=false", nil)
	wait, _ := parseLoadWaitParams(r)
	if wait {
		t.Errorf("wait=false: got true, want false")
	}
}

// TestParseLoadWaitParams_InvalidTimeout — invalid → falls back to default.
func TestParseLoadWaitParams_InvalidTimeout(t *testing.T) {
	cases := []struct {
		name string
		qs   string
		want int
	}{
		{"zero", "?wait=true&waitTimeoutSec=0", 60000},        // 0 ignored → default
		{"negative", "?wait=true&waitTimeoutSec=-1", 60000},   // negative ignored → default
		{"too_big", "?wait=true&waitTimeoutSec=99999", 60000}, // > 3600 ignored → default
		{"non_numeric", "?wait=true&waitTimeoutSec=abc", 60000},
		{"valid_300", "?wait=true&waitTimeoutSec=300", 300000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/api/models/load"+c.qs, nil)
			_, ms := parseLoadWaitParams(r)
			if ms != c.want {
				t.Errorf("qs=%s: got %d ms, want %d", c.qs, ms, c.want)
			}
		})
	}
}

// makeSparseFile создаёт файл нужного размера БЕЗ записи данных
// (sparse file на Windows: SetFileValidData / Seek+Truncate).
// Это позволяет тестам с 5GB файлами работать за миллисекунды, не минуты.
func makeSparseFile(t *testing.T, path string, sizeBytes int64) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer f.Close()
	if err := f.Truncate(sizeBytes); err != nil {
		t.Fatalf("truncate: %v", err)
	}
}

// TestEstimateLoadTimeMs_NoFile — путь пустой / не существует → 0.
func TestEstimateLoadTimeMs_NoFile(t *testing.T) {
	if ms := estimateLoadTimeMs("", 32768); ms != 0 {
		t.Errorf("empty path: got %d, want 0", ms)
	}
	if ms := estimateLoadTimeMs("/nonexistent/path/model.gguf", 32768); ms != 0 {
		t.Errorf("nonexistent path: got %d, want 0", ms)
	}
}

// TestEstimateLoadTimeMs_FormulaConsistency — проверяет, что формула
// линейна по size и ctx, и min-floored.
//
// Использует sparse files чтобы избежать записи реальных гигабайт на диск.
func TestEstimateLoadTimeMs_FormulaConsistency(t *testing.T) {
	dir := t.TempDir()

	// 1GB + 4K ctx → base=10000, ctx=500, overhead=2000 = 12500.
	path1 := filepath.Join(dir, "1gb.gguf")
	makeSparseFile(t, path1, 1<<30) // 1GB
	ms1 := estimateLoadTimeMs(path1, 4096)
	// base = 1GB / 100MB/s * 1000 = 10240ms
	// ctxMs = (4096+4095)/4096 * 500 = 1 * 500 = 500ms
	// overhead = 2000ms
	// total = 12740ms
	want1 := int64(10240 + 500 + 2000)
	if ms1 != want1 {
		t.Errorf("1GB+4K: got %d, want %d", ms1, want1)
	}

	// 2GB файл должен быть ровно в 2 раза больше по base, чем 1GB (при том же ctx).
	path2 := filepath.Join(dir, "2gb.gguf")
	makeSparseFile(t, path2, 2<<30) // 2GB
	ms2 := estimateLoadTimeMs(path2, 4096)
	// base = 20480ms, ctx = 500ms, overhead = 2000ms
	want2 := int64(20480 + 500 + 2000)
	if ms2 != want2 {
		t.Errorf("2GB+4K: got %d, want %d", ms2, want2)
	}

	// Проверка линейности: ms2 - ms1 должен быть равен base(2GB) - base(1GB) = 10240ms.
	if got := ms2 - ms1; got != 10240 {
		t.Errorf("linearity: 2GB-1GB delta = %d, want 10240ms", got)
	}
}

// TestEstimateLoadTimeMs_Gemma4Simulated — sparse 5GB файл (как gemma-4),
// проверяем что формула даёт ~57s.
func TestEstimateLoadTimeMs_Gemma4Simulated(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gemma-4-E4B-it-Q4_K_M.gguf")
	makeSparseFile(t, path, 5*(1<<30)) // 5GB sparse — мгновенно
	ms := estimateLoadTimeMs(path, 32768)
	// base = 5GB / 100MB/s * 1000 = 51200ms
	// ctxMs = (32768+4095)/4096 * 500 = 8 * 500 = 4000ms
	// overhead = 2000ms
	want := int64(51200 + 4000 + 2000)
	if ms != want {
		t.Errorf("gemma-4 5GB+32K: got %d, want %d", ms, want)
	}
	// Sanity: should be > 50s.
	if ms < 50000 {
		t.Errorf("gemma-4 estimate too low: %d ms (want >= 50000)", ms)
	}
}

// TestEstimateLoadTimeMs_ContextFactor — фиксированный размер + разный ctx,
// проверяем что ctxMs растёт как ожидается.
//
// Формула: blocksOf4K = (ctxSize + 4095) / 4096 (integer div)
//   ctxSize=4096:   (4096+4095)/4096 = 8191/4096  = 1 → 500ms
//   ctxSize=32768:  (32768+4095)/4096 = 36863/4096 = 8 → 4000ms
//   ctxSize=262144: (262144+4095)/4096 = 266239/4096 = 64 → 32000ms
func TestEstimateLoadTimeMs_ContextFactor(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fixed.gguf")
	makeSparseFile(t, path, 5*(1<<30)) // 5GB sparse

	ctx4K := estimateLoadTimeMs(path, 4096)
	ctx32K := estimateLoadTimeMs(path, 32768)
	ctx256K := estimateLoadTimeMs(path, 262144)

	// base + overhead = 51200 + 2000 = 53200 (одинаково для всех).
	wantBaseOH := int64(53200)
	if ctx4K != wantBaseOH+500 {
		t.Errorf("ctx4K: got %d, want %d (delta 500ms)", ctx4K, wantBaseOH+500)
	}
	if ctx32K != wantBaseOH+4000 {
		t.Errorf("ctx32K: got %d, want %d (delta 4000ms)", ctx32K, wantBaseOH+4000)
	}
	if ctx256K != wantBaseOH+32000 {
		t.Errorf("ctx256K: got %d, want %d (delta 32000ms)", ctx256K, wantBaseOH+32000)
	}
}

// TestEstimateLoadTimeMs_MinFloor — крошечный файл → не меньше loadBaseMinMs.
func TestEstimateLoadTimeMs_MinFloor(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tiny.gguf")
	if err := os.WriteFile(path, []byte("GGUF\x00\x00\x00\x03"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	ms := estimateLoadTimeMs(path, 4096)
	if ms < int64(loadBaseMinMs) {
		t.Errorf("tiny file: got %d, want >= %d (loadBaseMinMs)", ms, loadBaseMinMs)
	}
}

// TestEstimateLoadTimeMs_LargeModelEstimate — проверяем что для типичных
// больших моделей оценка разумная (не слишком оптимистичная).
func TestEstimateLoadTimeMs_LargeModelEstimate(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name     string
		sizeGB   int
		ctx      int
		minMs    int64
		wantBase int64 // ожидаемый base (size/speed * 1000)
	}{
		{"qwen2.5-1.5b", 1, 32768, 10000, 10240}, // 1GB → 10240ms base
		{"qwen2.5-7b", 4, 32768, 40000, 40960},   // 4GB → 40960ms base
		{"gemma-4-9b", 5, 32768, 50000, 51200},   // 5GB → 51200ms base
		{"qwen3-22b", 13, 32768, 130000, 133120}, // 13GB → 133120ms base
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(dir, c.name+".gguf")
			makeSparseFile(t, path, int64(c.sizeGB)*(1<<30))
			ms := estimateLoadTimeMs(path, c.ctx)
			if ms < c.minMs {
				t.Errorf("%s (%dGB+%d ctx): got %d ms, want >= %d",
					c.name, c.sizeGB, c.ctx, ms, c.minMs)
			}
		})
	}
}

// TestWriteLoadAccepted — 202 + Location + body fields.
func TestWriteLoadAccepted(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/models/load", nil)
	r.Host = "localhost:18092"

	modelInfo := map[string]interface{}{
		"name":  "gemma-4",
		"path":  "/models/gemma-4.gguf",
		"state": "loading",
	}
	writeLoadAccepted(w, r, "gemma-4", "/models/gemma-4.gguf", 5368709120, 57200, modelInfo)

	if w.Code != http.StatusAccepted {
		t.Errorf("status: got %d, want %d", w.Code, http.StatusAccepted)
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "http://localhost:18092/api/models/load/progress?model=") {
		t.Errorf("Location header: got %q, want prefix %q", loc,
			"http://localhost:18092/api/models/load/progress?model=")
	}
	if !strings.Contains(loc, "gemma-4") {
		t.Errorf("Location missing model name: %q", loc)
	}
	ct := w.Header().Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type: got %q, want application/json", ct)
	}

	// Parse body and verify fields.
	var body map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["status"] != "loading" {
		t.Errorf("body.status: got %v, want 'loading'", body["status"])
	}
	if body["name"] != "gemma-4" {
		t.Errorf("body.name: got %v, want 'gemma-4'", body["name"])
	}
	if size, _ := body["loadingSizeBytes"].(float64); int64(size) != 5368709120 {
		t.Errorf("body.loadingSizeBytes: got %v, want 5368709120", body["loadingSizeBytes"])
	}
	if est, _ := body["estimatedLoadTimeMs"].(float64); int64(est) != 57200 {
		t.Errorf("body.estimatedLoadTimeMs: got %v, want 57200", body["estimatedLoadTimeMs"])
	}
	if !strings.HasPrefix(body["progressUrl"].(string), "/api/models/load/progress?model=") {
		t.Errorf("body.progressUrl: got %v, want /api/models/load/progress?model=...", body["progressUrl"])
	}
}

// TestWriteLoadAccepted_QueryEscaped — model name с пробелами и спецсимволами
// должен быть escaped в Location и progressUrl.
func TestWriteLoadAccepted_QueryEscaped(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/models/load", nil)
	r.Host = "localhost:18092"

	writeLoadAccepted(w, r, "gemma 4 special&name=test", "/m.gguf", 1000, 5000, nil)

	if w.Code != http.StatusAccepted {
		t.Errorf("status: got %d, want %d", w.Code, http.StatusAccepted)
	}
	loc := w.Header().Get("Location")
	if strings.Contains(loc, "gemma 4") {
		t.Errorf("Location contains unescaped space: %q", loc)
	}
	if strings.Contains(loc, "name=test") {
		t.Errorf("Location contains unescaped =: %q", loc)
	}
}

// TestWriteLoadWaitTimeout — 202 + location + waitedMs field.
func TestWriteLoadWaitTimeout(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost,
		"/api/models/load?wait=true&waitTimeoutSec=30", nil)
	r.Host = "localhost:18092"

	writeLoadWaitTimeout(w, r, "gemma-4", "/m.gguf", 5368709120, 30000, 57200, nil)

	if w.Code != http.StatusAccepted {
		t.Errorf("status: got %d, want %d", w.Code, http.StatusAccepted)
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "/api/models/load/progress?model=") {
		t.Errorf("Location: %q", loc)
	}
	var body map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["status"] != "loading_after_timeout" {
		t.Errorf("body.status: got %v, want 'loading_after_timeout'", body["status"])
	}
	if waited, _ := body["waitedMs"].(float64); int64(waited) != 30000 {
		t.Errorf("body.waitedMs: got %v, want 30000", body["waitedMs"])
	}
	if est, _ := body["estimatedLoadTimeMs"].(float64); int64(est) != 57200 {
		t.Errorf("body.estimatedLoadTimeMs: got %v, want 57200", body["estimatedLoadTimeMs"])
	}
}

// TestParseBoolVariants_Round24 — sanity check на то что parseBool
// понимает разные варианты записи, как ожидает parseLoadWaitParams.
func TestParseBoolVariants_Round24(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"true", true}, {"True", true}, {"TRUE", true},
		{"false", false}, {"False", false}, {"FALSE", false},
		{"1", true}, {"0", false},
	}
	for _, c := range cases {
		got, err := strconv.ParseBool(c.in)
		if err != nil {
			t.Errorf("ParseBool(%q) error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseBool(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestProgressURLConsistency — writeLoadAccepted и writeLoadWaitTimeout
// должны генерить одинаковый progressUrl (только status в body разный).
func TestProgressURLConsistency(t *testing.T) {
	w1 := httptest.NewRecorder()
	r1 := httptest.NewRequest(http.MethodPost, "/api/models/load", nil)
	r1.Host = "localhost:18092"
	writeLoadAccepted(w1, r1, "test-model", "/m.gguf", 1000, 5000, nil)

	w2 := httptest.NewRecorder()
	r2 := httptest.NewRequest(http.MethodPost, "/api/models/load?wait=true", nil)
	r2.Host = "localhost:18092"
	writeLoadWaitTimeout(w2, r2, "test-model", "/m.gguf", 1000, 30000, 5000, nil)

	loc1 := w1.Header().Get("Location")
	loc2 := w2.Header().Get("Location")
	if loc1 != loc2 {
		t.Errorf("Location differs: accepted=%q, timeout=%q", loc1, loc2)
	}

	// Sanity: same path, model name correctly query-decoded.
	parsed, _ := url.Parse(loc1)
	if parsed.Query().Get("model") != "test-model" {
		t.Errorf("Location model query: got %q, want test-model",
			parsed.Query().Get("model"))
	}
}

// TestDerefIntPtr — sanity check на helper.
func TestDerefIntPtr(t *testing.T) {
	if got := derefIntPtr(nil); got != 0 {
		t.Errorf("nil: got %d, want 0", got)
	}
	v := 42
	if got := derefIntPtr(&v); got != 42 {
		t.Errorf("&42: got %d, want 42", got)
	}
}

// TestDerefBoolPtr — sanity check на helper.
func TestDerefBoolPtr(t *testing.T) {
	if got := derefBoolPtr(nil); got != false {
		t.Errorf("nil: got %v, want false", got)
	}
	v := true
	if got := derefBoolPtr(&v); !got {
		t.Errorf("&true: got %v, want true", got)
	}
}

// BenchmarkParseLoadWaitParams — замер производительности (для info).
func BenchmarkParseLoadWaitParams(b *testing.B) {
	r := httptest.NewRequest(http.MethodPost,
		"/api/models/load?wait=true&waitTimeoutSec=120", nil)
	for i := 0; i < b.N; i++ {
		_, _ = parseLoadWaitParams(r)
	}
}

// BenchmarkEstimateLoadTimeMs — замер производительности оценки.
func BenchmarkEstimateLoadTimeMs(b *testing.B) {
	dir := b.TempDir()
	path := filepath.Join(dir, "bench.gguf")
	f, err := os.Create(path)
	if err != nil {
		b.Fatalf("create: %v", err)
	}
	if err := f.Truncate(5 * (1 << 30)); err != nil {
		b.Fatalf("truncate: %v", err)
	}
	f.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = estimateLoadTimeMs(path, 32768)
	}
}

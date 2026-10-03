package cppbackend

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// ============================================================
// Фильтр расширений (R-Image)
// ============================================================

func TestHasModelWeightExtension(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"model.gguf", true},
		{"MODEL.GGUF", true},
		{"ae.safetensors", true},
		{"flux1-dev.sft", true},
		{"old-model.ckpt", true},
		{"tokenizer.json", false},
		{"config.json", false},
		{"README.md", false},
		{"", false},
	}
	for _, c := range cases {
		if got := HasModelWeightExtension(c.name); got != c.want {
			t.Errorf("HasModelWeightExtension(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestFileFormatFromPath(t *testing.T) {
	cases := map[string]string{
		"a/b/model.gguf": "gguf",
		"ae.safetensors": "safetensors",
		"x.SFT":          "sft",
		"y.ckpt":         "ckpt",
		"tokenizer.json": "",
	}
	for path, want := range cases {
		if got := FileFormatFromPath(path); got != want {
			t.Errorf("FileFormatFromPath(%q) = %q, want %q", path, got, want)
		}
	}
}

// mockHFTreeServer — мок HF API: отдаёт tree-листинг, resolve-файлы и считает
// запросы по именам файлов (для проверки «уже скачан → не качаем»).
type mockHFTreeServer struct {
	server     *httptest.Server
	mu         sync.Mutex
	resolveHit map[string]int
	// failFiles — файлы (basename), на которые resolve отвечает 500.
	failFiles map[string]bool
	// lastRange — последний полученный Range-заголовок (для resume-теста).
	lastRange string
	// content — отдаваемое содержимое для resolve (по умолчанию testFileContent).
	content string
}

const testFileContent = "0123456789"

func newMockHFTreeServer(t *testing.T, treeFiles []map[string]interface{}) *mockHFTreeServer {
	t.Helper()
	m := &mockHFTreeServer{
		resolveHit: map[string]int{},
		failFiles:  map[string]bool{},
		content:    testFileContent,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/models/", func(w http.ResponseWriter, r *http.Request) {
		// /models/{repo}/tree/{rev}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(treeFiles)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// /{repo}/resolve/{rev}/{filename}
		base := filepath.Base(r.URL.Path)
		m.mu.Lock()
		m.resolveHit[base]++
		fail := m.failFiles[base]
		m.lastRange = r.Header.Get("Range")
		content := m.content
		m.mu.Unlock()

		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"simulated failure"}`))
			return
		}
		total := int64(len(content))
		if rng := r.Header.Get("Range"); strings.HasPrefix(rng, "bytes=") {
			var from int64
			fmt.Sscanf(strings.TrimPrefix(rng, "bytes="), "%d-", &from)
			if from > total {
				from = total
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, total-1, total))
			w.Header().Set("Content-Length", fmt.Sprintf("%d", total-from))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write([]byte(content[from:]))
			return
		}
		w.Header().Set("Content-Length", fmt.Sprintf("%d", total))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(content))
	})
	m.server = httptest.NewServer(mux)
	t.Cleanup(m.server.Close)
	return m
}

func (m *mockHFTreeServer) hits(name string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.resolveHit[name]
}

// TestListModelFilesByFormat_MultiFormat — tree-листинг больше не фильтруется
// жёстко по .gguf: для bundle нужны .safetensors/.sft/.ckpt (VAE, TE, чекпойнты).
func TestListModelFilesByFormat_MultiFormat(t *testing.T) {
	mock := newMockHFTreeServer(t, []map[string]interface{}{
		{"path": "model-Q4_0.gguf", "size": 100, "type": "file"},
		{"path": "ae.safetensors", "size": 300, "type": "file"},
		{"path": "clip_l.sft", "size": 200, "type": "file"},
		{"path": "old.ckpt", "size": 400, "type": "file"},
		{"path": "tokenizer.json", "size": 10, "type": "file"},
		{"path": "sub", "size": 0, "type": "directory"},
	})

	d := NewHuggingFaceDownloader("", mock.server.URL, t.TempDir(), t.TempDir())
	files, err := d.ListModelFiles(context.Background(), "some/bundle", "main")
	if err != nil {
		t.Fatalf("ListModelFiles failed: %v", err)
	}
	if len(files) != 4 {
		t.Fatalf("expected 4 model files, got %d (%+v)", len(files), files)
	}
	// Отсортировано по размеру возрастанию (контракт существующего API).
	if files[0].Path != "model-Q4_0.gguf" {
		t.Errorf("expected smallest first, got %q", files[0].Path)
	}

	byFormat := map[string]bool{}
	for _, f := range files {
		byFormat[f.Format] = true
	}
	for _, want := range []string{"gguf", "safetensors", "sft", "ckpt"} {
		if !byFormat[want] {
			t.Errorf("format %q missing in %+v", want, files)
		}
	}
	// IsGGUF остаётся признаком именно GGUF (обратная совместимость UI).
	for _, f := range files {
		if f.Format == "gguf" && !f.IsGGUF {
			t.Errorf("gguf file %q must have IsGGUF=true", f.Path)
		}
		if f.Format != "gguf" && f.IsGGUF {
			t.Errorf("non-gguf file %q must have IsGGUF=false", f.Path)
		}
	}
}

// TestListModelFilesByFormat_ExplicitGGUFOnly — параметризация уважается:
// явный список [".gguf"] не должен пропускать safetensors.
func TestListModelFilesByFormat_ExplicitGGUFOnly(t *testing.T) {
	mock := newMockHFTreeServer(t, []map[string]interface{}{
		{"path": "model.gguf", "size": 100, "type": "file"},
		{"path": "ae.safetensors", "size": 300, "type": "file"},
	})

	d := NewHuggingFaceDownloader("", mock.server.URL, t.TempDir(), t.TempDir())
	files, err := d.ListModelFilesByFormat(context.Background(), "some/bundle", "main", []string{".gguf"})
	if err != nil {
		t.Fatalf("ListModelFilesByFormat failed: %v", err)
	}
	if len(files) != 1 || files[0].Path != "model.gguf" {
		t.Fatalf("expected only model.gguf, got %+v", files)
	}
}

// TestListModelFiles_RecursiveAndPaged (2026-10-03) — ДВА требования к листингу,
// без которых bundle DiT-модели не собрать:
//
//  1. recursive=true: VAE и text encoder лежат в подкаталогах (vae/,
//     text_encoders/), а без рекурсии tree-API отдаёт только корень;
//  2. обход страниц: HF отдаёт до 1000 записей и ссылку rel="next" в Link —
//     следующую страницу обязаны запросить у ТОГО ЖЕ зеркала (из Link берём
//     только cursor, иначе трафик и токен ушли бы на huggingface.co).
func TestListModelFiles_RecursiveAndPaged(t *testing.T) {
	var urls []string
	mux := http.NewServeMux()
	mux.HandleFunc("/models/", func(w http.ResponseWriter, r *http.Request) {
		urls = append(urls, r.URL.String())
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("cursor") == "" {
			w.Header().Set("Link", `<https://huggingface.co/api/models/some/dit/tree/main?recursive=true&cursor=PAGE2>; rel="next"`)
			_ = json.NewEncoder(w).Encode([]map[string]interface{}{
				{"path": "qwen-image-2.1-UC-Q4_0.gguf", "size": 100, "type": "file"},
				{"path": "vae", "size": 0, "type": "directory"},
			})
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]interface{}{
			{"path": "vae/qwen_image_2.1_vae_bf16.safetensors", "size": 300, "type": "file"},
			{"path": "text_encoders/qwen3vl_8b_bf16.safetensors", "size": 200, "type": "file"},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	d := NewHuggingFaceDownloader("", srv.URL, t.TempDir(), t.TempDir())
	files, err := d.ListModelFiles(context.Background(), "some/dit", "main")
	if err != nil {
		t.Fatalf("ListModelFiles failed: %v", err)
	}
	if len(files) != 3 {
		t.Fatalf("ожидали 3 файла (включая подкаталоги), получили %d: %+v", len(files), files)
	}
	if len(urls) != 2 {
		t.Fatalf("ожидали два запроса (страницы tree-API), получили %d: %v", len(urls), urls)
	}
	if !strings.Contains(urls[0], "recursive=true") {
		t.Errorf("первый запрос без recursive=true: %s", urls[0])
	}
	if !strings.Contains(urls[1], "cursor=PAGE2") {
		t.Errorf("вторая страница запрошена без cursor из Link: %s", urls[1])
	}
	if strings.Contains(urls[1], "huggingface.co") {
		t.Errorf("вторая страница ушла на huggingface.co вместо зеркала: %s", urls[1])
	}
	for _, want := range []string{"qwen-image-2.1-UC-Q4_0.gguf", "text_encoders/qwen3vl_8b_bf16.safetensors", "vae/qwen_image_2.1_vae_bf16.safetensors"} {
		found := false
		for _, f := range files {
			if f.Path == want {
				found = true
			}
		}
		if !found {
			t.Errorf("в списке нет %q: %+v", want, files)
		}
	}
}

// ============================================================
// Bundle: атомарная регистрация
// ============================================================

func bundleFiles() []HFDownloadRequest {
	return []HFDownloadRequest{
		{Role: "diffusion", ModelID: "leejet/Z-Image-Turbo-GGUF", Filename: "z_image_turbo-Q3_K.gguf", SizeBytes: int64(len(testFileContent))},
		{Role: "vae", ModelID: "black-forest-labs/FLUX.1-schnell", Filename: "ae.safetensors", SizeBytes: int64(len(testFileContent))},
		{Role: "llm", ModelID: "unsloth/Qwen3-4B-Instruct-2507-GGUF", Filename: "Qwen3-4B-Q4_K_M.gguf", SizeBytes: int64(len(testFileContent))},
	}
}

// TestStartBundleDownload_RegistersAtomically — все файлы скачаны → манифест есть,
// прогресс 100%, файлы лежат в <modelsDir>/<bundleID>/.
func TestStartBundleDownload_RegistersAtomically(t *testing.T) {
	mock := newMockHFTreeServer(t, nil)
	modelsDir := t.TempDir()
	d := NewHuggingFaceDownloader("", mock.server.URL, t.TempDir(), modelsDir)

	progress, err := d.StartBundleDownload(context.Background(), bundleFiles(), "z-image-turbo-q3-k", "")
	if err != nil {
		t.Fatalf("StartBundleDownload failed: %v", err)
	}
	if progress.Status != "completed" {
		t.Errorf("status = %q, want completed", progress.Status)
	}
	if !progress.Registered {
		t.Error("bundle must be Registered after all files downloaded")
	}
	if progress.ProgressPct != 100 {
		t.Errorf("progressPct = %v, want 100", progress.ProgressPct)
	}
	wantTotal := int64(3 * len(testFileContent))
	if progress.TotalBytes != wantTotal || progress.Downloaded != wantTotal {
		t.Errorf("total/downloaded = %d/%d, want %d", progress.TotalBytes, progress.Downloaded, wantTotal)
	}

	dir := filepath.Join(modelsDir, "z-image-turbo-q3-k")
	for _, f := range bundleFiles() {
		path := filepath.Join(dir, filepath.Base(f.Filename))
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("expected downloaded file %s: %v", path, err)
		}
		if string(data) != testFileContent {
			t.Errorf("file %s content = %q, want %q", path, string(data), testFileContent)
		}
	}
	if !d.IsBundleRegistered("z-image-turbo-q3-k") {
		t.Error("IsBundleRegistered must be true after successful bundle download")
	}
	manifest, err := LoadBundleManifest(dir)
	if err != nil {
		t.Fatalf("LoadBundleManifest: %v", err)
	}
	if manifest.BundleID != "z-image-turbo-q3-k" || len(manifest.Files) != 3 {
		t.Errorf("unexpected manifest: %+v", manifest)
	}
}

// TestStartBundleDownload_PartialFailureNotRegistered — ГЛАВНЫЙ негативный тест:
// один файл не скачался → bundle НЕ зарегистрирован (нет манифеста), статус
// failed, ошибка называет проблемный файл, уже скачанные файлы остаются на диске
// (их подхватит resume), а «модели» в каталоге не появляется.
func TestStartBundleDownload_PartialFailureNotRegistered(t *testing.T) {
	mock := newMockHFTreeServer(t, nil)
	mock.failFiles["ae.safetensors"] = true

	modelsDir := t.TempDir()
	d := NewHuggingFaceDownloader("", mock.server.URL, t.TempDir(), modelsDir)

	progress, err := d.StartBundleDownload(context.Background(), bundleFiles(), "broken-bundle", "")
	if err == nil {
		t.Fatal("expected error when one bundle file fails")
	}
	if !strings.Contains(err.Error(), "ae.safetensors") {
		t.Errorf("error must name the failed file, got: %v", err)
	}
	if progress.Registered {
		t.Error("bundle must NOT be registered when a file failed")
	}
	if progress.Status != "failed" {
		t.Errorf("status = %q, want failed", progress.Status)
	}

	dir := filepath.Join(modelsDir, "broken-bundle")
	if _, statErr := os.Stat(filepath.Join(dir, BundleManifestName)); !os.IsNotExist(statErr) {
		t.Errorf("manifest must not exist for an incomplete bundle (stat err=%v)", statErr)
	}
	if d.IsBundleRegistered("broken-bundle") {
		t.Error("IsBundleRegistered must be false for an incomplete bundle")
	}
	// Успешные файлы на месте — их не нужно качать заново.
	if _, statErr := os.Stat(filepath.Join(dir, "z_image_turbo-Q3_K.gguf")); statErr != nil {
		t.Errorf("successfully downloaded file must be kept: %v", statErr)
	}
	// Статусы в прогрессе: completed / failed / completed.
	statuses := map[string]string{}
	for _, f := range progress.Files {
		statuses[f.Filename] = f.Status
	}
	if statuses["z_image_turbo-Q3_K.gguf"] != BundleFileStatusCompleted {
		t.Errorf("diffusion status = %q, want completed", statuses["z_image_turbo-Q3_K.gguf"])
	}
	if statuses["ae.safetensors"] != BundleFileStatusFailed {
		t.Errorf("vae status = %q, want failed", statuses["ae.safetensors"])
	}
}

// TestStartBundleDownload_SkipsAlreadyDownloaded — повторный pull не перекачивает
// файл, размер которого совпал с ожидаемым.
func TestStartBundleDownload_SkipsAlreadyDownloaded(t *testing.T) {
	mock := newMockHFTreeServer(t, nil)
	modelsDir := t.TempDir()
	d := NewHuggingFaceDownloader("", mock.server.URL, t.TempDir(), modelsDir)

	dir := filepath.Join(modelsDir, "resume-bundle")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "z_image_turbo-Q3_K.gguf"), []byte(testFileContent), 0644); err != nil {
		t.Fatal(err)
	}

	progress, err := d.StartBundleDownload(context.Background(), bundleFiles(), "resume-bundle", "")
	if err != nil {
		t.Fatalf("StartBundleDownload failed: %v", err)
	}
	if !progress.Registered {
		t.Error("bundle must be registered")
	}
	if hits := mock.hits("z_image_turbo-Q3_K.gguf"); hits != 0 {
		t.Errorf("already-downloaded file must not be re-fetched, resolve hits = %d", hits)
	}
	if hits := mock.hits("ae.safetensors"); hits != 1 {
		t.Errorf("missing file must be fetched once, resolve hits = %d", hits)
	}
}

// TestStartBundleDownload_ResumesPartialDownload — .download с N байт →
// Range-запрос, файл собирается целиком, манифест пишется.
func TestStartBundleDownload_ResumesPartialDownload(t *testing.T) {
	mock := newMockHFTreeServer(t, nil)
	modelsDir := t.TempDir()
	d := NewHuggingFaceDownloader("", mock.server.URL, t.TempDir(), modelsDir)

	dir := filepath.Join(modelsDir, "part-bundle")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	// 4 байта из 10 уже на диске (прерванный pull).
	if err := os.WriteFile(filepath.Join(dir, "z_image_turbo-Q3_K.gguf.download"), []byte(testFileContent[:4]), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := d.StartBundleDownload(context.Background(), bundleFiles()[:1], "part-bundle", "")
	if err != nil {
		t.Fatalf("StartBundleDownload failed: %v", err)
	}
	if mock.lastRange != "bytes=4-" {
		t.Errorf("expected Range request bytes=4-, got %q", mock.lastRange)
	}
	data, err := os.ReadFile(filepath.Join(dir, "z_image_turbo-Q3_K.gguf"))
	if err != nil {
		t.Fatalf("final file missing: %v", err)
	}
	if string(data) != testFileContent {
		t.Errorf("resumed file = %q, want %q", string(data), testFileContent)
	}
}

// TestStartBundleDownload_CancelLeavesUnregistered — отмена (ctx) → не
// зарегистрировано, статус cancelled.
func TestStartBundleDownload_CancelLeavesUnregistered(t *testing.T) {
	mock := newMockHFTreeServer(t, nil)
	modelsDir := t.TempDir()
	d := NewHuggingFaceDownloader("", mock.server.URL, t.TempDir(), modelsDir)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // отменяем ДО старта: детерминированно, без гонок по таймингу

	progress, err := d.StartBundleDownload(ctx, bundleFiles(), "cancelled-bundle", "")
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
	if progress.Registered {
		t.Error("cancelled bundle must not be registered")
	}
	if progress.Status != "cancelled" {
		t.Errorf("status = %q, want cancelled", progress.Status)
	}
	if d.IsBundleRegistered("cancelled-bundle") {
		t.Error("IsBundleRegistered must be false for cancelled bundle")
	}
}

// TestStartBundleDownload_InvalidRequests — fail fast с понятными ошибками
// (пустой состав, чужое расширение, дубликат basename, небезопасный bundleID).
func TestStartBundleDownload_InvalidRequests(t *testing.T) {
	d := NewHuggingFaceDownloader("", "", t.TempDir(), t.TempDir())

	cases := []struct {
		name     string
		files    []HFDownloadRequest
		bundleID string
		wantErr  string
	}{
		{
			name:     "empty files",
			files:    nil,
			bundleID: "b1",
			wantErr:  "files is required",
		},
		{
			name:     "empty bundleId",
			files:    bundleFiles()[:1],
			bundleID: "",
			wantErr:  "bundleId is required",
		},
		{
			name:     "path traversal bundleId",
			files:    bundleFiles()[:1],
			bundleID: "../escape",
			wantErr:  "path separators",
		},
		{
			name: "unsupported extension",
			files: []HFDownloadRequest{
				{ModelID: "a/b", Filename: "model.json"},
			},
			bundleID: "b2",
			wantErr:  "unsupported file type",
		},
		{
			name: "missing filename",
			files: []HFDownloadRequest{
				{ModelID: "a/b"},
			},
			bundleID: "b3",
			wantErr:  "filename is required",
		},
		{
			name: "duplicate basename",
			files: []HFDownloadRequest{
				{ModelID: "a/b", Filename: "x/ae.safetensors"},
				{ModelID: "c/d", Filename: "y/ae.safetensors"},
			},
			bundleID: "b4",
			wantErr:  "same name",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := d.StartBundleDownload(context.Background(), c.files, c.bundleID, "")
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", c.wantErr)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("error = %v, want substring %q", err, c.wantErr)
			}
		})
	}
}

// TestGetBundleProgress_History — после завершения прогресс доступен из истории.
func TestGetBundleProgress_History(t *testing.T) {
	mock := newMockHFTreeServer(t, nil)
	d := NewHuggingFaceDownloader("", mock.server.URL, t.TempDir(), t.TempDir())

	if _, err := d.StartBundleDownload(context.Background(), bundleFiles(), "hist-bundle", ""); err != nil {
		t.Fatalf("StartBundleDownload failed: %v", err)
	}
	// Активной загрузки уже нет — запись должна читаться из истории.
	if len(d.ListActiveBundles()) != 0 {
		t.Errorf("expected no active bundles, got %d", len(d.ListActiveBundles()))
	}
	progress, err := d.GetBundleProgress("hist-bundle")
	if err != nil {
		t.Fatalf("GetBundleProgress: %v", err)
	}
	if !progress.Registered || progress.Status != "completed" {
		t.Errorf("unexpected progress from history: %+v", progress)
	}

	targetDir := filepath.Join(d.BundleDir("hist-bundle"))
	if !IsBundleRegistered(targetDir) {
		t.Error("package-level IsBundleRegistered must be true")
	}
}

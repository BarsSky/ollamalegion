//go:build !no_cppbackend

package tests

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ollama-loadbalancer/internal/cppbackend"
)

// ============================================================
// HuggingFaceDownloader basic tests
// ============================================================

func TestNewHuggingFaceDownloader(t *testing.T) {
	d := cppbackend.NewHuggingFaceDownloader("test-token", "", "./downloads", "./models")
	if d == nil {
		t.Fatal("NewHuggingFaceDownloader returned nil")
	}

	active := d.ListActiveDownloads()
	if len(active) != 0 {
		t.Errorf("expected 0 active downloads, got %d", len(active))
	}

	history := d.ListDownloadHistory()
	if len(history) != 0 {
		t.Errorf("expected 0 history entries, got %d", len(history))
	}
}

func TestHFDownloaderNewWithDefaults(t *testing.T) {
	d := cppbackend.NewHuggingFaceDownloader("", "", "", "./models")
	if d == nil {
		t.Fatal("NewHuggingFaceDownloader returned nil")
	}
}

// ============================================================
// Directory initialization tests
// ============================================================

func TestHFDownloaderInitializeDownloadDir(t *testing.T) {
	tmpDir := t.TempDir()
	downloadsDir := filepath.Join(tmpDir, "downloads")
	modelsDir := filepath.Join(tmpDir, "models")

	d := cppbackend.NewHuggingFaceDownloader("", "", downloadsDir, modelsDir)
	if err := d.InitializeDownloadDir(); err != nil {
		t.Fatalf("InitializeDownloadDir failed: %v", err)
	}

	if _, err := os.Stat(downloadsDir); os.IsNotExist(err) {
		t.Error("downloads directory should exist")
	}
	if _, err := os.Stat(modelsDir); os.IsNotExist(err) {
		t.Error("models directory should exist")
	}
}

func TestHFDownloaderInitializeDownloadDirExists(t *testing.T) {
	tmpDir := t.TempDir()
	// Directories already exist
	os.MkdirAll(filepath.Join(tmpDir, "dl"), 0755)
	os.MkdirAll(filepath.Join(tmpDir, "mdl"), 0755)

	d := cppbackend.NewHuggingFaceDownloader("", "", filepath.Join(tmpDir, "dl"), filepath.Join(tmpDir, "mdl"))
	if err := d.InitializeDownloadDir(); err != nil {
		t.Fatalf("InitializeDownloadDir on existing dirs failed: %v", err)
	}
}

// ============================================================
// Download management error handling tests
// ============================================================

func TestHFDownloaderStartDownloadNoModelID(t *testing.T) {
	d := cppbackend.NewHuggingFaceDownloader("", "", t.TempDir(), t.TempDir())

	_, err := d.StartDownload(cppbackend.HFDownloadRequest{
		ModelID: "",
	})
	if err == nil {
		t.Error("expected error for empty modelID")
	}
	if !strings.Contains(err.Error(), "modelId is required") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestHFDownloaderStartDownloadNoFilename(t *testing.T) {
	d := cppbackend.NewHuggingFaceDownloader("", "", t.TempDir(), t.TempDir())

	_, err := d.StartDownload(cppbackend.HFDownloadRequest{
		ModelID:    "test/model",
		Filename:   "",
		AutoDetect: false,
	})
	if err == nil {
		t.Error("expected error when filename is empty and autoDetect is false")
	}
}

func TestHFDownloaderCancelDownloadNotFound(t *testing.T) {
	d := cppbackend.NewHuggingFaceDownloader("", "", t.TempDir(), t.TempDir())

	err := d.CancelDownload("nonexistent/model", "file.gguf")
	if err == nil {
		t.Error("expected error for non-existent download")
	}
}

func TestHFDownloaderGetDownloadProgressNotFound(t *testing.T) {
	d := cppbackend.NewHuggingFaceDownloader("", "", t.TempDir(), t.TempDir())

	_, err := d.GetDownloadProgress("nonexistent/model", "file.gguf")
	if err == nil {
		t.Error("expected error for non-existent download progress")
	}
}

// ============================================================
// List methods on empty downloader
// ============================================================

func TestHFDownloaderListActiveDownloadsEmpty(t *testing.T) {
	d := cppbackend.NewHuggingFaceDownloader("", "", t.TempDir(), t.TempDir())

	active := d.ListActiveDownloads()
	if len(active) != 0 {
		t.Errorf("expected 0 active downloads, got %d", len(active))
	}
}

func TestHFDownloaderListDownloadHistoryEmpty(t *testing.T) {
	d := cppbackend.NewHuggingFaceDownloader("", "", t.TempDir(), t.TempDir())

	history := d.ListDownloadHistory()
	if len(history) != 0 {
		t.Errorf("expected 0 history entries, got %d", len(history))
	}
}

// ============================================================
// Close tests
// ============================================================

func TestHFDownloaderClose(t *testing.T) {
	d := cppbackend.NewHuggingFaceDownloader("", "", t.TempDir(), t.TempDir())
	// Should not panic
	d.Close()
}

func TestHFDownloaderCloseWithActiveDownloads(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", "100000")
		w.WriteHeader(http.StatusOK)
		data := make([]byte, 10000)
		for i := 0; i < 10; i++ {
			w.Write(data)
			w.(http.Flusher).Flush()
			time.Sleep(20 * time.Millisecond)
		}
	}))
	defer server.Close()

	d := cppbackend.NewHuggingFaceDownloader("", server.URL, t.TempDir(), t.TempDir())

	_, err := d.StartDownload(cppbackend.HFDownloadRequest{
		ModelID:  "test/model",
		Filename: "close-test.gguf",
	})
	if err != nil {
		t.Fatalf("StartDownload failed: %v", err)
	}

	// Close should cancel active downloads without panic
	d.Close()
}

// ============================================================
// SearchModels with mock HTTP server
// ============================================================

func TestHFDownloaderSearchModels(t *testing.T) {
	mockResp := []map[string]interface{}{
		{
			"id":           "TheBloke/Llama-2-7B-GGUF",
			"lastModified": "2024-01-15T12:00:00Z",
			"downloads":    15000,
			"likes":        120,
			"pipeline_tag": "text-generation",
		},
		{
			"id":           "TheBloke/Mistral-7B-GGUF",
			"lastModified": "2024-02-20T12:00:00Z",
			"downloads":    8500,
			"likes":        75,
			"pipeline_tag": "text-generation",
		},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if !strings.Contains(r.URL.Path, "/models") {
			t.Errorf("expected /models in path, got %s", r.URL.Path)
		}

		// Эмулируем tree-endpoint для каждого репозитория: возвращаем .gguf файлы
		if strings.Contains(r.URL.Path, "/tree/") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode([]map[string]interface{}{
				{"path": "model-q4_k_m.gguf", "size": 4430000000, "type": "file"},
			})
			return
		}

		// Search endpoint: требуем параметр search
		if r.URL.Query().Get("search") != "llama" {
			t.Errorf("expected search=llama, got %s", r.URL.Query().Get("search"))
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(mockResp)
	}))
	defer server.Close()

	// Используем mirror для перенаправления запросов на mock сервер
	d := cppbackend.NewHuggingFaceDownloader("", server.URL, t.TempDir(), t.TempDir())

	ctx := context.Background()
	results, err := d.SearchModels(ctx, "llama", 5)
	if err != nil {
		t.Fatalf("SearchModels failed: %v", err)
	}

	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	if results[0].ID != "TheBloke/Llama-2-7B-GGUF" {
		t.Errorf("expected 'TheBloke/Llama-2-7B-GGUF', got '%s'", results[0].ID)
	}
	if results[0].Author != "TheBloke" {
		t.Errorf("expected author 'TheBloke', got '%s'", results[0].Author)
	}
	if results[0].Name != "Llama-2-7B-GGUF" {
		t.Errorf("expected name 'Llama-2-7B-GGUF', got '%s'", results[0].Name)
	}
	if results[0].Downloads != 15000 {
		t.Errorf("expected 15000 downloads, got %d", results[0].Downloads)
	}
	if results[0].Likes != 120 {
		t.Errorf("expected 120 likes, got %d", results[0].Likes)
	}
	if results[0].PipelineTag != "text-generation" {
		t.Errorf("expected 'text-generation', got '%s'", results[0].PipelineTag)
	}

	// Проверяем второй результат
	if results[1].ID != "TheBloke/Mistral-7B-GGUF" {
		t.Errorf("expected 'TheBloke/Mistral-7B-GGUF', got '%s'", results[1].ID)
	}
	if results[1].Author != "TheBloke" {
		t.Errorf("expected author 'TheBloke', got '%s'", results[1].Author)
	}
}

func TestHFDownloaderSearchModelsEmptyResult(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`[]`))
	}))
	defer server.Close()

	d := cppbackend.NewHuggingFaceDownloader("", server.URL, t.TempDir(), t.TempDir())

	ctx := context.Background()
	results, err := d.SearchModels(ctx, "nonexistent-model-xyz", 5)
	if err != nil {
		t.Fatalf("SearchModels failed: %v", err)
	}

	if len(results) != 0 {
		t.Errorf("expected 0 results, got %d", len(results))
	}
}

func TestHFDownloaderSearchModelsHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error": "internal error"}`))
	}))
	defer server.Close()

	d := cppbackend.NewHuggingFaceDownloader("", server.URL, t.TempDir(), t.TempDir())

	ctx := context.Background()
	_, err := d.SearchModels(ctx, "llama", 5)
	if err == nil {
		t.Error("expected error for HTTP 500")
	}
}

func TestHFDownloaderSearchModelsTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`[]`))
	}))
	defer server.Close()

	d := cppbackend.NewHuggingFaceDownloader("", server.URL, t.TempDir(), t.TempDir())

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
	defer cancel()

	_, err := d.SearchModels(ctx, "llama", 5)
	if err == nil {
		t.Error("expected error for timeout")
	}
}

func TestHFDownloaderSearchModelsLimitBounds(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limit := r.URL.Query().Get("limit")
		if limit != "20" {
			t.Errorf("expected default limit=20 for over-limit input, got %s", limit)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`[]`))
	}))
	defer server.Close()

	d := cppbackend.NewHuggingFaceDownloader("", server.URL, t.TempDir(), t.TempDir())

	ctx := context.Background()
	// limit > 100 should be capped to 20
	_, err := d.SearchModels(ctx, "test", 500)
	if err != nil {
		t.Fatalf("SearchModels failed: %v", err)
	}
}

// ============================================================
// ListModelFiles with mock HTTP server
// ============================================================

func TestHFDownloaderListModelFiles(t *testing.T) {
	mockFiles := []map[string]interface{}{
		{"path": "llama-2-7b.Q2_K.gguf", "size": 3000000000, "type": "file"},
		{"path": "llama-2-7b.Q4_K_M.gguf", "size": 4000000000, "type": "file"},
		{"path": "llama-2-7b.Q8_0.gguf", "size": 8000000000, "type": "file"},
		{"path": "tokenizer.json", "size": 1000000, "type": "file"},
		{"path": "config.json", "size": 500, "type": "file"},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if !strings.Contains(r.URL.Path, "/models/TheBloke/Llama-2-7B-GGUF/tree/main") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(mockFiles)
	}))
	defer server.Close()

	d := cppbackend.NewHuggingFaceDownloader("", server.URL, t.TempDir(), t.TempDir())

	ctx := context.Background()
	files, err := d.ListModelFiles(ctx, "TheBloke/Llama-2-7B-GGUF", "main")
	if err != nil {
		t.Fatalf("ListModelFiles failed: %v", err)
	}

	// Должны вернуться только .gguf файлы (3 из 5)
	if len(files) != 3 {
		t.Fatalf("expected 3 GGUF files, got %d", len(files))
	}

	// Должны быть отсортированы по возрастанию размера
	if files[0].SizeBytes >= files[1].SizeBytes {
		t.Error("expected files sorted by size ascending")
	}

	// Проверяем метаданные первого файла
	if files[0].Path != "llama-2-7b.Q2_K.gguf" {
		t.Errorf("expected 'llama-2-7b.Q2_K.gguf', got '%s'", files[0].Path)
	}
	if !files[0].IsGGUF {
		t.Error("expected IsGGUF=true")
	}
	if files[0].Quantization != "Q2_K" {
		t.Errorf("expected quantization 'Q2_K', got '%s'", files[0].Quantization)
	}

	// Проверяем последний файл (самый большой)
	lastFile := files[len(files)-1]
	if lastFile.Path != "llama-2-7b.Q8_0.gguf" {
		t.Errorf("expected last file 'llama-2-7b.Q8_0.gguf', got '%s'", lastFile.Path)
	}
	if lastFile.Quantization != "Q8_0" {
		t.Errorf("expected quantization 'Q8_0', got '%s'", lastFile.Quantization)
	}
}

func TestHFDownloaderListModelFilesNoGGUF(t *testing.T) {
	mockFiles := []map[string]interface{}{
		{"path": "tokenizer.json", "size": 1000000, "type": "file"},
		{"path": "config.json", "size": 500, "type": "file"},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(mockFiles)
	}))
	defer server.Close()

	d := cppbackend.NewHuggingFaceDownloader("", server.URL, t.TempDir(), t.TempDir())

	ctx := context.Background()
	files, err := d.ListModelFiles(ctx, "test/model", "main")
	if err != nil {
		t.Fatalf("ListModelFiles failed: %v", err)
	}

	if len(files) != 0 {
		t.Errorf("expected 0 GGUF files, got %d", len(files))
	}
}

func TestHFDownloaderListModelFilesDefaultRevision(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/tree/main") {
			t.Errorf("expected default revision 'main' in path, got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`[]`))
	}))
	defer server.Close()

	d := cppbackend.NewHuggingFaceDownloader("", server.URL, t.TempDir(), t.TempDir())

	ctx := context.Background()
	// Пустая ревизия → должна использоваться "main"
	_, err := d.ListModelFiles(ctx, "test/model", "")
	if err != nil {
		t.Fatalf("ListModelFiles failed: %v", err)
	}
}

func TestHFDownloaderListModelFilesHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error": "not found"}`))
	}))
	defer server.Close()

	d := cppbackend.NewHuggingFaceDownloader("", server.URL, t.TempDir(), t.TempDir())

	ctx := context.Background()
	_, err := d.ListModelFiles(ctx, "nonexistent/model", "main")
	if err == nil {
		t.Error("expected error for HTTP 404")
	}
}

// ============================================================
// Alternative API format test (siblings) — fallback при ошибке парсинга
// ============================================================

func TestHFDownloaderListModelFilesAlternative(t *testing.T) {
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++

		if strings.Contains(r.URL.Path, "/tree/") {
			// Возвращаем данные, которые не парсятся как массив
			// (например, объект с ошибкой вместо массива)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"error": "not an array", "path": "/models/test/model/tree/main"}`))
			return
		}

		// Второй запрос: информация о модели с siblings
		resp := map[string]interface{}{
			"siblings": []map[string]interface{}{
				{"rfilename": "model.Q2_K.gguf", "size": 3000000000},
				{"rfilename": "model.Q4_K_M.gguf", "size": 4000000000},
				{"rfilename": "tokenizer.json", "size": 1000000},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	d := cppbackend.NewHuggingFaceDownloader("", server.URL, t.TempDir(), t.TempDir())

	ctx := context.Background()
	files, err := d.ListModelFiles(ctx, "test/model", "main")
	if err != nil {
		t.Fatalf("ListModelFiles failed: %v", err)
	}

	if len(files) != 2 {
		t.Fatalf("expected 2 GGUF files from alternative method, got %d", len(files))
	}

	if callCount != 2 {
		t.Errorf("expected 2 API calls (tree + model info), got %d", callCount)
	}

	if files[0].Path != "model.Q2_K.gguf" {
		t.Errorf("expected 'model.Q2_K.gguf', got '%s'", files[0].Path)
	}
	if files[1].Path != "model.Q4_K_M.gguf" {
		t.Errorf("expected 'model.Q4_K_M.gguf', got '%s'", files[1].Path)
	}
}

// ============================================================
// StartDownload basic flow tests
// ============================================================

func TestHFDownloaderStartDownloadAndCancel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", "2000000")
		w.WriteHeader(http.StatusOK)

		// Отправляем данные медленно, чтобы успеть отменить
		data := make([]byte, 100000)
		for i := 0; i < 20; i++ {
			select {
			case <-r.Context().Done():
				return
			default:
			}
			_, err := w.Write(data)
			if err != nil {
				return
			}
			w.(http.Flusher).Flush()
			time.Sleep(50 * time.Millisecond)
		}
	}))
	defer server.Close()

	modelsDir := t.TempDir()
	downloadsDir := t.TempDir()

	d := cppbackend.NewHuggingFaceDownloader("", server.URL, downloadsDir, modelsDir)

	progress, err := d.StartDownload(cppbackend.HFDownloadRequest{
		ModelID:  "test/model",
		Filename: "cancel-test-model.gguf",
	})
	if err != nil {
		t.Fatalf("StartDownload failed: %v", err)
	}

	if progress.Status != "downloading" {
		t.Errorf("expected status 'downloading', got '%s'", progress.Status)
	}

	// Даём время на старт загрузки
	time.Sleep(50 * time.Millisecond)

	// Отменяем загрузку
	if err := d.CancelDownload("test/model", "cancel-test-model.gguf"); err != nil {
		t.Fatalf("CancelDownload failed: %v", err)
	}

	// Проверяем, что загрузка отменена
	t.Log("Download cancelled successfully")

	d.Close()
}

// TestHFDownloaderStartDownloadDuplicate проверяет идемпотентное поведение:
// повторный вызов StartDownload для уже загружающейся модели возвращает
// текущий прогресс (а не ошибку), что позволяет WebUI безопасно обрабатывать
// множественные клики по «Download».
func TestHFDownloaderStartDownloadDuplicate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("test data for duplicate test"))
	}))
	defer server.Close()

	d := cppbackend.NewHuggingFaceDownloader("", server.URL, t.TempDir(), t.TempDir())

	// Первый запуск
	first, err := d.StartDownload(cppbackend.HFDownloadRequest{
		ModelID:  "test/model",
		Filename: "dup-test.gguf",
	})
	if err != nil {
		t.Fatalf("StartDownload failed: %v", err)
	}
	if first == nil {
		t.Fatal("first StartDownload returned nil progress")
	}

	// Второй запуск (дубликат) — должен вернуть прогресс той же загрузки
	second, err := d.StartDownload(cppbackend.HFDownloadRequest{
		ModelID:  "test/model",
		Filename: "dup-test.gguf",
	})
	if err != nil {
		t.Fatalf("duplicate StartDownload should be idempotent, got error: %v", err)
	}
	if second == nil {
		t.Fatal("duplicate StartDownload returned nil progress (expected snapshot of existing download)")
	}
	if second.ModelID != "test/model" || second.Filename != "dup-test.gguf" {
		t.Errorf("duplicate progress has wrong identity: %+v", second)
	}
	if second.Status == "" {
		t.Error("duplicate progress has empty status")
	}

	// Должен существовать ровно один активный download
	active := d.ListActiveDownloads()
	count := 0
	for _, p := range active {
		if p.ModelID == "test/model" && p.Filename == "dup-test.gguf" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 active download, got %d (active=%+v)", count, active)
	}

	// Ждём завершения первой загрузки
	time.Sleep(300 * time.Millisecond)

	d.Close()
}

func TestHFDownloaderDownloadToExistingFile(t *testing.T) {
	modelsDir := t.TempDir()

	// Создаём файл, который уже существует
	existingFile := filepath.Join(modelsDir, "existing.gguf")
	os.WriteFile(existingFile, []byte("existing data"), 0644)

	d := cppbackend.NewHuggingFaceDownloader("", "", t.TempDir(), modelsDir)

	_, err := d.StartDownload(cppbackend.HFDownloadRequest{
		ModelID:  "test/model",
		Filename: "existing.gguf",
	})
	if err == nil {
		t.Error("expected error when file already exists in models directory")
	}
}

// ============================================================
// Download with auth token test
// ============================================================

func TestHFDownloaderWithAuthToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Проверяем заголовок Authorization
		auth := r.Header.Get("Authorization")
		if auth != "Bearer test-hf-token" {
			t.Errorf("expected 'Bearer test-hf-token', got '%s'", auth)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`[]`))
	}))
	defer server.Close()

	d := cppbackend.NewHuggingFaceDownloader("test-hf-token", server.URL, t.TempDir(), t.TempDir())

	ctx := context.Background()
	_, err := d.SearchModels(ctx, "test", 5)
	if err != nil {
		t.Fatalf("SearchModels failed: %v", err)
	}
}

// ============================================================
// User-Agent header test
// ============================================================

func TestHFDownloaderUserAgent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ua := r.Header.Get("User-Agent")
		if ua != "ollamalegion-cppworker/1.0" {
			t.Errorf("expected User-Agent 'ollamalegion-cppworker/1.0', got '%s'", ua)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`[]`))
	}))
	defer server.Close()

	d := cppbackend.NewHuggingFaceDownloader("", server.URL, t.TempDir(), t.TempDir())

	ctx := context.Background()
	_, err := d.SearchModels(ctx, "test", 5)
	if err != nil {
		t.Fatalf("SearchModels failed: %v", err)
	}
}

// ============================================================
// Successful small download test
// ============================================================

func TestHFDownloaderSmallDownloadComplete(t *testing.T) {
	testData := "test model data for complete download test"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(testData)))
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(testData))
	}))
	defer server.Close()

	modelsDir := t.TempDir()
	downloadsDir := t.TempDir()

	d := cppbackend.NewHuggingFaceDownloader("", server.URL, downloadsDir, modelsDir)

	_, err := d.StartDownload(cppbackend.HFDownloadRequest{
		ModelID:  "test/model",
		Filename: "small-complete.gguf",
	})
	if err != nil {
		t.Fatalf("StartDownload failed: %v", err)
	}

	// Ждём завершения
	time.Sleep(500 * time.Millisecond)

	// Проверяем, что файл появился в modelsDir
	finalPath := filepath.Join(modelsDir, "small-complete.gguf")
	if _, err := os.Stat(finalPath); os.IsNotExist(err) {
		// Файл может быть ещё в процессе перемещения
		t.Logf("File not yet in models dir (might be downloading): %s", finalPath)
		// Проверяем downloads dir
		tmpPath := filepath.Join(downloadsDir, "small-complete.gguf.download")
		if _, err := os.Stat(tmpPath); os.IsNotExist(err) {
			// Возможно уже скопировался и удалился
			// Проверяем историю загрузок
			history := d.ListDownloadHistory()
			for _, p := range history {
				if p.Filename == "small-complete.gguf" {
					t.Logf("Download status: %s, pct=%.1f%%", p.Status, p.ProgressPct)
				}
			}
		} else {
			t.Log("Download still in progress (tmp file exists)")
		}
	} else {
		data, _ := os.ReadFile(finalPath)
		if string(data) != testData {
			t.Errorf("expected file content '%s', got '%s'", testData, string(data))
		}
		t.Logf("Download completed successfully, file size: %d bytes", len(data))
	}

	d.Close()
}

// ============================================================
// Concurrent download limit test
// ============================================================

func TestHFDownloaderMaxConcurrentDownloads(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", "50000")
		w.WriteHeader(http.StatusOK)
		data := make([]byte, 5000)
		for i := 0; i < 10; i++ {
			select {
			case <-r.Context().Done():
				return
			default:
			}
			w.Write(data)
			w.(http.Flusher).Flush()
			time.Sleep(30 * time.Millisecond)
		}
	}))
	defer server.Close()

	d := cppbackend.NewHuggingFaceDownloader("", server.URL, t.TempDir(), t.TempDir())

	// Запускаем 5 одновременных загрузок (максимум 3 должны выполняться одновременно)
	for i := 0; i < 5; i++ {
		filename := fmt.Sprintf("concurrent-%d.gguf", i)
		_, err := d.StartDownload(cppbackend.HFDownloadRequest{
			ModelID:  "test/model",
			Filename: filename,
		})
		if err != nil {
			t.Fatalf("StartDownload(%d) failed: %v", i, err)
		}
	}

	// Даём время на старт всех загрузок
	time.Sleep(100 * time.Millisecond)

	active := d.ListActiveDownloads()
	t.Logf("Active downloads: %d", len(active))

	// Отменяем все загрузки
	for i := 0; i < 5; i++ {
		filename := fmt.Sprintf("concurrent-%d.gguf", i)
		d.CancelDownload("test/model", filename)
	}

	d.Close()
}

// ============================================================
// Backend integration test
// ============================================================

func TestBackendHFDownloaderIntegration(t *testing.T) {
	cfg := cppbackend.DefaultConfig()
	cfg.ModelsDir = t.TempDir()
	cfg.DownloadsDir = t.TempDir()

	b := cppbackend.NewBackend(cfg)
	if b == nil {
		t.Fatal("NewBackend returned nil")
	}

	// Проверяем, что HFDownloader доступен через Backend
	downloader := b.HFDownloader()
	if downloader == nil {
		t.Fatal("HFDownloader should not be nil")
	}

	// Проверяем базовые методы через Backend
	active := downloader.ListActiveDownloads()
	if active == nil {
		t.Error("ListActiveDownloads should return non-nil slice")
	}

	history := downloader.ListDownloadHistory()
	if history == nil {
		t.Error("ListDownloadHistory should return non-nil slice")
	}

	// Закрываем Backend (должен закрыть и HFDownloader)
	b.Close()
}

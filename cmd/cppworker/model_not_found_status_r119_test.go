// model_not_found_status_r119_test.go — R119 (2026-10-15).
//
// ЖИВОЙ ДЕФЕКТ (стенд). Клиент (OpenWebUI) просил модель `llama-3-8b`, которой
// нет в каталоге. cppworker отвечал **500 Internal Server Error с ПУСТЫМ телом**
// (три 500 подряд в логе с интервалом 150 мс — клиент повторял запрос), и ни
// оператор, ни клиент не могли узнать причину:
//
//	{"level":"error","caller":"cppworker/lazyload.go:397","msg":"lazy-load: model file not found",
//	 "model":"llama-3-8b","path":"models/llama-3-8b.gguf","error":"stat ...: no such file or directory"}
//	{"level":"info","caller":"cppworker/utils.go:166","msg":"HTTP request","method":"POST",
//	 "path":"/api/generate","status":500,"duration":"3.263239ms"}
//
// Причина 500: lazy-load возвращал НЕТИПИЗИРОВАННУЮ ошибку, и все хендлеры
// (/api/generate, /api/chat, /api/embeddings, /v1/*) писали одну и ту же ветку
// `writeError(w, 500, "model load failed: "+err)`.
//
// Теперь: 404 + машиночитаемое тело. Ollama для неизвестной модели тоже отдаёт
// 404 — клиенты показывают `error` как есть и НЕ уходят в retry-цикл.
//
// Тест падает до правки: writeModelLoadError не существует, а ErrModelFileNotFound
// не типизирован (errors.Is не находит).
package main

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadErrorResponse(t *testing.T, err error) (int, map[string]interface{}) {
	t.Helper()
	rec := httptest.NewRecorder()
	if !writeModelLoadError(rec, "llama-3-8b", err) {
		t.Fatalf("writeModelLoadError(%v) = false — ответ не записан (err != nil)", err)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("тело не JSON: %v (%s)", err, rec.Body.String())
	}
	return rec.Code, body
}

// TestR119_MissingModelFileIs404 — «файла модели нет» = 404, не 500.
func TestR119_MissingModelFileIs404(t *testing.T) {
	// Точно так ошибку формирует lazy-load (обёрнутая, не голая).
	err := fmt.Errorf("%w: %s", ErrModelFileNotFound, "/app/models/llama-3-8b.gguf")
	if !isModelLoadingError(err) && !strings.Contains(err.Error(), "model file not found") {
		t.Fatalf("подготовка теста: неожиданный текст ошибки %q", err.Error())
	}

	code, body := loadErrorResponse(t, err)
	if code != 404 {
		t.Errorf("status = %d, want 404: до правки неизвестная модель давала 500 "+
			"Internal Server Error с пустым телом, и клиент повторял запрос", code)
	}
	if body["code"] != "model_file_not_found" {
		t.Errorf("code = %v, want model_file_not_found (машинный признак для инструментов)", body["code"])
	}
	if body["model"] != "llama-3-8b" {
		t.Errorf("model = %v, want llama-3-8b", body["model"])
	}
	msg, _ := body["error"].(string)
	if !strings.Contains(msg, "not found") || !strings.Contains(msg, "llama-3-8b") {
		t.Errorf("error = %q: клиент должен увидеть, ЧТО не найдено", msg)
	}
	if hint, _ := body["hint"].(string); hint == "" {
		t.Error("hint пуст: оператору не сказано, что проверить")
	}
}

// TestR119_ModelLoadingIsStill503 — «модель грузится» остаётся 503 + Retry-After.
func TestR119_ModelLoadingIsStill503(t *testing.T) {
	rec := httptest.NewRecorder()
	if !writeModelLoadError(rec, "m", errModelIsLoading) {
		t.Fatal("ответ не записан для errModelIsLoading")
	}
	if rec.Code != 503 {
		t.Errorf("status = %d, want 503 (loading-путь не должен измениться)", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("нет Retry-After: клиент не знает, когда повторить")
	}
}

// TestR119_OtherErrorsStay500 — прочие поломки остаются 500 (мы не прячем баги).
func TestR119_OtherErrorsStay500(t *testing.T) {
	code, body := loadErrorResponse(t, fmt.Errorf("cudaMalloc failed: out of memory"))
	if code != 500 {
		t.Errorf("status = %d, want 500 для настоящей поломки", code)
	}
	msg, _ := body["error"].(string)
	if !strings.Contains(msg, "model load failed") {
		t.Errorf("error = %q, want прежний текст «model load failed: …»", msg)
	}
}

// TestR119_NilErrorIsNoop — nil не пишет ответ (вызывающий продолжает работу).
func TestR119_NilErrorIsNoop(t *testing.T) {
	rec := httptest.NewRecorder()
	if writeModelLoadError(rec, "m", nil) {
		t.Error("writeModelLoadError(nil) = true, want false")
	}
	if rec.Body.Len() != 0 {
		t.Errorf("для nil записано тело %q", rec.Body.String())
	}
}

// TestR119_ErrModelFileNotFoundSentinelIsUsed — lazy-load обязан ОБОРАЧИВАТЬ
// ошибку этим сентинелом, иначе 404-ветка никогда не сработает.
func TestR119_ErrModelFileNotFoundSentinelIsUsed(t *testing.T) {
	src, err := os.ReadFile(filepath.Join(".", "lazyload.go"))
	if err != nil {
		t.Fatalf("read lazyload.go: %v", err)
	}
	if !strings.Contains(string(src), "fmt.Errorf(\"%w: %s\", ErrModelFileNotFound") {
		t.Error("lazy-load больше не оборачивает ошибку ErrModelFileNotFound — " +
			"неизвестная модель снова станет 500")
	}
}

// TestR119_HandlersDoNotMapLoadErrorsTo500 — защита от отката: ни один хендлер
// не должен сам писать 500 на ошибку догрузки модели в обход writeModelLoadError.
func TestR119_HandlersDoNotMapLoadErrorsTo500(t *testing.T) {
	files, err := filepath.Glob("handlers_*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	bad := []string{}
	for _, f := range files {
		src, rerr := os.ReadFile(f)
		if rerr != nil {
			t.Fatalf("read %s: %v", f, rerr)
		}
		if strings.Contains(string(src), `"model load failed: "+err.Error()`) {
			bad = append(bad, f)
		}
	}
	if len(bad) > 0 {
		t.Errorf("хендлеры пишут 500 на любую ошибку догрузки: %v — "+
			"используйте writeModelLoadError (503/404/500)", bad)
	}
}

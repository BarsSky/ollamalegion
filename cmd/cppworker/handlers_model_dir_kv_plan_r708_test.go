// handlers_model_dir_kv_plan_r708_test.go — R708 (2026-11-23): раскладка KV в
// /api/models/files (карточка модели на ДИСКЕ).
//
// ЗАЧЕМ. R707 публикует раскладку в /api/models, но там только ЗАГРУЖЕННЫЕ
// модели: незагруженный файл на странице «Модели на диске» показывал размер и
// квантизацию, а «сколько он съест KV и почему не все слои × ширина» — ничего.
// Теперь тот же KVPlan публикуется в /api/models/files теми же именами полей,
// что в /api/models, и WebUI рисует строку прямо из ответа воркера.
//
// ЧТО ЗАФИКСИРОВАНО:
//  1. у модели с раскладкой (gemma-4) поля есть и числа совпадают с
//     KVPlanForModel — тем же источником, из которого /api/models берёт свои;
//  2. у файла без метаданных полей НЕТ вовсе (старый воркер/битый header) —
//     WebUI не должен рисовать ни строки, ни прочерка;
//  3. поля размера/квантизации (R60.3) не потеряны;
//  4. обе ручки читают ОДИН набор имён (WireKVPlanFieldsR707), а не две копии.
//go:build llama_stub

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ollama-loadbalancer/internal/cppbackend"
)

// r708FilesBackend — backend над каталогом с gemma-4 (раскладка есть) и
// «старым» файлом без метаданных (раскладки нет).
func r708FilesBackend(t *testing.T) *cppbackend.Backend {
	t.Helper()
	dir := t.TempDir()
	writeGGUFKVBlockR708(t, filepath.Join(dir, "gemma-4-E4B-it-Q4_K_M.gguf"), gemma4KeysR708())
	// «Старый воркер»: файл виден (ScanModels перечисляет все .gguf), но
	// заголовок не читается — метаданных нет, значит и раскладки нет.
	if err := os.WriteFile(filepath.Join(dir, "old-worker-model.gguf"),
		[]byte("GGUF not a real header"), 0644); err != nil {
		t.Fatalf("write old-worker gguf: %v", err)
	}

	be := cppbackend.NewBackend(cppbackend.Config{
		ModelsDir: dir, DefaultCtxSize: 32768, DefaultGPULayers: 32,
	})
	if err := be.Init(); err != nil {
		t.Logf("backend.Init() warning (expected in stub): %v", err)
	}
	mm := be.ModelManager()
	if mm == nil {
		t.Fatal("ModelManager == nil")
	}
	if _, err := mm.ScanModels(); err != nil {
		t.Fatalf("ScanModels: %v", err)
	}
	backend = be
	t.Cleanup(func() { backend = nil })
	return be
}

// filesPayloadR708 — ответ /api/models/files как есть (через настоящий handler).
func filesPayloadR708(t *testing.T) map[string]interface{} {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/models/files", nil)
	handleListModelsDir(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/models/files -> HTTP %d: %s", rec.Code, rec.Body.String())
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("json: %v", err)
	}
	return body
}

func fileEntryR708(t *testing.T, body map[string]interface{}, name string) map[string]interface{} {
	t.Helper()
	files, ok := body["files"].([]interface{})
	if !ok {
		t.Fatalf("в ответе нет files: %v", body)
	}
	for _, raw := range files {
		e, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		if e["name"] == name {
			return e
		}
	}
	t.Fatalf("в /api/models/files нет %q (файлов: %d)", name, len(files))
	return nil
}

// TestR708_ModelsDirPublishesKVPlan — главный тест: поля раскладки у модели с
// раскладкой и ИХ отсутствие у модели без метаданных.
func TestR708_ModelsDirPublishesKVPlan(t *testing.T) {
	be := r708FilesBackend(t)
	body := filesPayloadR708(t)

	plan, ok := be.KVPlanForModel("gemma-4-E4B-it-Q4_K_M.gguf")
	if !ok {
		t.Fatal("KVPlanForModel не нашёл раскладку gemma-4")
	}

	entry := fileEntryR708(t, body, "gemma-4-E4B-it-Q4_K_M.gguf")
	want := map[string]interface{}{
		"kv_plan_source":   "plan",
		"kv_plan_exact":    true,
		"kv_layers":        float64(24),
		"kv_global_layers": float64(4),
		"kv_swa_layers":    float64(20),
		"kv_swa_window":    float64(512),
		"head_dim_swa":     float64(256),
		"head_dim_swa_v":   float64(256),
		"n_kv_heads_swa":   float64(2),
		// Геометрия глобального класса — имена ровно как у /api/models (R660/R705):
		// по ним WebUI считает «почему не все слои × ширина» и Б/токен.
		"head_dim_k": float64(512),
		"head_dim_v": float64(512),
		"n_kv_heads": float64(2),
	}
	for k, w := range want {
		got, present := entry[k]
		if !present {
			t.Errorf("в /api/models/files нет поля %q — карточка модели на диске останется без раскладки", k)
			continue
		}
		if got != w {
			t.Errorf("поле %q = %v, want %v", k, got, w)
		}
	}

	// Совпадение с источником: те же поля, что публикует /api/models
	// (WireKVPlanFieldsR707), а не вторая копия тех же чисел. Сравнение через
	// JSON: в ответе числа после декодирования — float64, в KVPlan — int.
	planWire, err := json.Marshal(plan.WireKVPlanFieldsR707())
	if err != nil {
		t.Fatalf("marshal plan fields: %v", err)
	}
	var fromPlan map[string]interface{}
	if err := json.Unmarshal(planWire, &fromPlan); err != nil {
		t.Fatalf("unmarshal plan fields: %v", err)
	}
	for k, v := range fromPlan {
		if got, present := entry[k]; !present || got != v {
			t.Errorf("поле %q = %v (есть=%v), KVPlan даёт %v — источник разошёлся", k, got, present, v)
		}
	}
	// Инвариант «почему не все слои × ширина»: слоёв с кэшем МЕНЬШЕ, чем блоков.
	if nLayers, _ := entry["n_layers"].(float64); nLayers != 0 {
		t.Fatalf("n_layers не должен выдумываться в /api/models/files (got %v)", nLayers)
	}

	// R60.3 не потерян: размер и квантизация по-прежнему в ответе.
	if _, ok := entry["size"]; !ok {
		t.Error("поле size пропало из /api/models/files")
	}
	if q, ok := entry["quantization"]; !ok || q != "Q4_K_M" {
		t.Errorf("quantization = %v (есть=%v), want Q4_K_M", q, ok)
	}

	// Файл без метаданных: ключей раскладки НЕТ (не нули и не прочерк).
	old := fileEntryR708(t, body, "old-worker-model.gguf")
	for _, k := range []string{
		"kv_plan_source", "kv_plan_exact", "kv_layers", "kv_global_layers",
		"kv_swa_layers", "kv_swa_window", "head_dim_swa", "n_kv_heads_swa",
		"head_dim_k", "head_dim_v", "n_kv_heads",
	} {
		if v, present := old[k]; present {
			t.Errorf("у файла без метаданных есть поле %q=%v — WebUI нарисует ложную раскладку", k, v)
		}
	}
	// Но сам файл в списке есть: «нет раскладки» ≠ «нет файла».
	if _, ok := old["name"]; !ok {
		t.Error("файл без метаданных пропал из /api/models/files")
	}
}

// TestR708_WireNamesSharedWithModelsEndpoint — сторож: обе ручки воркера берут
// имена раскладки из ОДНОГО источника (WireKVPlanFieldsR707). Вторая копия
// списка полей — это ровно тот дефект R660/R45, из-за которого геометрия
// считалась, но не публиковалась.
func TestR708_WireNamesSharedWithModelsEndpoint(t *testing.T) {
	for _, path := range []string{
		"handlers_model_list.go", // /api/models
		"handlers_model_dir.go",  // /api/models/files
	} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("не читается %s: %v", path, err)
		}
		text := strings.Join(strings.Fields(string(raw)), " ")
		if !strings.Contains(text, "WireKVPlanFieldsR707") {
			t.Errorf("%s не использует KVPlan.WireKVPlanFieldsR707 — имена полей разъедутся", path)
		}
		if !strings.Contains(text, "KVPlanForModel") {
			t.Errorf("%s не берёт раскладку из backend.KVPlanForModel", path)
		}
	}
}

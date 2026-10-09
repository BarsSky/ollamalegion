//go:build llama_stub

// degraded_load_r83_test.go — R83 §9.4 шаг 1б, вариант B+D (2026-09-26).
//
// Проверяет две половины решения:
//
//	B — «не влезает даже в CPU-only» = отказ с числами из вердикта memfit;
//	D — «влезает, но без GPU» = загрузка + запись в регистр деградации, которую
//	    agent забирает из /api/models полем load_degraded и превращает в warning.
//
// Ключевая инварианта, которую фиксируют тесты: деградация НЕ пишется в
// load_failures (иначе балансер опубликует «не хватило памяти» и позже
// «загрузка восстановлена» — оба сообщения о работающей модели ложны).
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ollama-loadbalancer/internal/cppbackend"
	"ollama-loadbalancer/internal/memfit"
)

// TestDegradedRegistry_RecordKeyAndTTL — регистр хранит запись, отдаёт её по
// имени и latest(), а по TTL забывает.
func TestDegradedRegistry_RecordKeyAndTTL(t *testing.T) {
	r := newDegradedLoadRegistry(time.Minute)
	now := time.Now()
	r.now = func() time.Time { return now }

	d := &DegradedNotice{
		Model:  "qwen3.8-27b",
		Stage:  "cpu_only",
		Reason: "insufficient_resources",
		At:     now,
		Numbers: map[string]interface{}{
			"gpu_layers": 0,
		},
	}
	if d.IsEmpty() {
		t.Fatal("заполненная запись считается пустой — уведомление не опубликуется")
	}
	r.record(d)

	got, ok := r.get("qwen3.8-27b")
	if !ok || got.Stage != "cpu_only" {
		t.Fatalf("get() = %+v, %v; want cpu_only-запись", got, ok)
	}
	if latest, ok := r.latest(); !ok || latest.Model != "qwen3.8-27b" {
		t.Fatalf("latest() = %+v, %v", latest, ok)
	}

	// Запись без модели и стадии не должна попадать в регистр: иначе
	// /api/models покажет деградацию на пустом месте.
	r.record(&DegradedNotice{Reason: "insufficient_resources"})
	if _, ok := r.get(""); ok {
		t.Error("пустая запись попала в регистр")
	}

	// TTL: запись старше минуты забывается.
	now = now.Add(2 * time.Minute)
	if _, ok := r.get("qwen3.8-27b"); ok {
		t.Error("запись не истекла по TTL — оператор будет видеть старую деградацию вечно")
	}
}

// TestDegradedRegistry_Clear — успешная нормальная загрузка снимает запись.
func TestDegradedRegistry_Clear(t *testing.T) {
	r := newDegradedLoadRegistry(time.Minute)
	r.record(&DegradedNotice{Model: "m", Stage: "cpu_only", At: time.Now()})
	r.clear("m")
	if _, ok := r.get("m"); ok {
		t.Error("clear() не снял запись — деградация останется висеть после нормальной загрузки")
	}
}

// TestDegradedStageFromVerdict — только cpu_only считается деградацией: для
// exact_fit/partial_offload предупреждать не о чем, а does_not_fit — это отказ
// (свой путь, см. шаг 1б вариант B).
func TestDegradedStageFromVerdict(t *testing.T) {
	cases := []struct {
		stage memfit.Stage
		want  string
	}{
		{memfit.StageExactFit, ""},
		{memfit.StagePartial, ""},
		{memfit.StageCPUOnly, "cpu_only"},
		{memfit.StageDoesNotFit, ""},
	}
	for _, tc := range cases {
		v := memfit.Verdict{Stage: tc.stage}
		if got := degradedStageFromVerdict(v); got != tc.want {
			t.Errorf("degradedStageFromVerdict(%s) = %q, want %q", tc.stage, got, tc.want)
		}
	}
}

// TestInsErrFromVerdict_NumbersAndReason — 413-ответ обязан нести числа
// вердикта и конкретную причину, иначе клиент получает «не хватило памяти» без
// подсказки, что именно уменьшать.
func TestInsErrFromVerdict_NumbersAndReason(t *testing.T) {
	v := memfit.Verdict{
		Stage:       memfit.StageDoesNotFit,
		GPULayers:   0,
		TotalLayers: 64,
		Weights:     memfit.Bytes(16 << 30),
		KVTotal:     memfit.Bytes(1088 << 20),
		UsableVRAM:  memfit.Bytes(6 << 30),
		UsableRAM:   memfit.Bytes(2 << 30),
		MaxHardCtx:  199029,
		Reasons: []memfit.Reason{
			{Code: memfit.ReasonRAMShort, Detail: "не хватает 10.5 GiB"},
		},
		Suggestion: "уменьшите n_ctx",
	}
	err := insErrFromVerdict("qwen3.8-27b", v, 262144)
	if err.Model != "qwen3.8-27b" || err.RequestedNCtx != 262144 {
		t.Errorf("модель/n_ctx потеряны: %+v", err)
	}
	if err.MaxViableNCtx != 199029 {
		t.Errorf("MaxViableNCtx = %d, want 199029 (из вердикта)", err.MaxViableNCtx)
	}
	if err.AvailableRAMMB != 2048 || err.AvailableVRAMMB != 6144 {
		t.Errorf("ресурсы в ответе = RAM %d / VRAM %d, want 2048/6144 МБ",
			err.AvailableRAMMB, err.AvailableVRAMMB)
	}
	if err.Reason != "not_enough_ram" {
		t.Errorf("Reason = %q, want not_enough_ram (клиент должен знать, что уменьшать)", err.Reason)
	}
	if err.Suggestion == "" || err.MaxViableNCtx == 0 {
		t.Error("подсказка пуста — клиент не поймёт, что делать")
	}
}

// TestDegradedNoticeFromVerdict_OnlyCPUOnly — запись появляется только для
// cpu_only и несёт числа вердикта (лог, HTTP и событие не должны расходиться).
func TestDegradedNoticeFromVerdict_OnlyCPUOnly(t *testing.T) {
	base := memfit.Verdict{
		Stage:       memfit.StageCPUOnly,
		KVType:      memfit.KVQ8,
		GPULayers:   0,
		TotalLayers: 64,
		Weights:     memfit.Bytes(16 << 30),
		KVTotal:     memfit.Bytes(1088 << 20),
		UsableVRAM:  memfit.Bytes(1 << 30),
		UsableRAM:   memfit.Bytes(20 << 30),
		MaxHardCtx:  199029,
	}
	d := degradedNoticeFromVerdict("qwen3.8-27b", base, 32768)
	if d.IsEmpty() {
		t.Fatal("cpu_only не дал записи о деградации — оператор не узнает о режиме")
	}
	if d.Stage != "cpu_only" || d.Reason != "insufficient_resources" {
		t.Errorf("stage/reason = %q/%q", d.Stage, d.Reason)
	}
	if got, _ := d.Numbers["gpu_layers"].(int); got != 0 {
		t.Errorf("diagnostics.gpu_layers = %v, want 0", d.Numbers["gpu_layers"])
	}
	if _, ok := d.Numbers["usable_ram_mb"]; !ok {
		t.Error("diagnostics без usable_ram_mb — оператор не увидит, почему раскладка такая")
	}

	// Не-деградированные стадии записи не дают.
	for _, stage := range []memfit.Stage{memfit.StageExactFit, memfit.StagePartial, memfit.StageDoesNotFit} {
		v := base
		v.Stage = stage
		if d := degradedNoticeFromVerdict("m", v, 32768); d != nil {
			t.Errorf("стадия %s дала запись о деградации — ложное предупреждение", stage)
		}
	}
}

// TestForceReloadRequest_BodyAndQuery — escape-hatch обязан работать обоими
// способами: поле body (так шлёт балансер) и ?force=true (так удобнее curl).
func TestForceReloadRequest_BodyAndQuery(t *testing.T) {
	yes, no := true, false
	if !forceReloadRequest(httptest.NewRequest(http.MethodPost, "/api/models/reload", nil), &yes) {
		t.Error("force=true в теле не распознан — оператор не сможет загрузить вопреки отказу")
	}
	if forceReloadRequest(httptest.NewRequest(http.MethodPost, "/api/models/reload", nil), &no) {
		t.Error("force=false в теле трактован как согласие")
	}
	req := httptest.NewRequest(http.MethodPost, "/api/models/reload?force=true", nil)
	if !forceReloadRequest(req, nil) {
		t.Error("?force=true не распознан")
	}
	if forceReloadRequest(httptest.NewRequest(http.MethodPost, "/api/models/reload", nil), nil) {
		t.Error("без force и без query согласие не должно появляться")
	}
}

// TestSetDegradedHeader_Values — заголовки ответа: клиент видит режим сразу.
func TestSetDegradedHeader_Values(t *testing.T) {
	rec := httptest.NewRecorder()
	setDegradedHeader(rec, "cpu_only")
	if got := rec.Header().Get("X-CppWorker-Degraded"); got != "insufficient_resources" {
		t.Errorf("X-CppWorker-Degraded = %q", got)
	}
	if got := rec.Header().Get("X-CppWorker-Degraded-Stage"); got != "cpu_only" {
		t.Errorf("X-CppWorker-Degraded-Stage = %q", got)
	}
	// Пустая стадия — заголовков быть не должно (иначе каждый ответ выглядит
	// деградированным).
	rec2 := httptest.NewRecorder()
	setDegradedHeader(rec2, "")
	if rec2.Header().Get("X-CppWorker-Degraded") != "" {
		t.Error("заголовок выставлен без стадии")
	}
}

// TestDegraded_ModelsExposesLoadDegraded — ключевое звено связки: agent забирает
// режим из /api/models (поле load_degraded) тем же опросом, что и load_failure.
func TestDegraded_ModelsExposesLoadDegraded(t *testing.T) {
	oldBackend := backend
	defer func() { backend = oldBackend }()
	backend = newTestBackend()

	const model = "Qwen3.8-27B-UD-Q4_K_M"
	degradedLoads.clear(model)
	degradedLoads.record(&DegradedNotice{
		Model:  model,
		Stage:  "cpu_only",
		Reason: "insufficient_resources",
		Detail: "weights_exceed_free_vram: 12.0 GiB > 7.6 GiB",
		At:     time.Now().UTC(),
		Numbers: map[string]interface{}{
			"requested_n_ctx": 32768,
			"gpu_layers":      0,
		},
	})
	defer degradedLoads.clear(model)

	req := httptest.NewRequest(http.MethodGet, "/api/models", nil)
	rec := httptest.NewRecorder()
	handleListModels(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v", err)
	}

	raw, ok := resp["load_degraded"]
	if !ok {
		t.Fatalf("в /api/models нет load_degraded — agent не сможет показать режим загрузки")
	}
	d, ok := raw.(map[string]interface{})
	if !ok {
		t.Fatalf("load_degraded не объект: %T", raw)
	}
	if got, _ := d["stage"].(string); got != "cpu_only" {
		t.Errorf("load_degraded.stage = %q, want cpu_only", got)
	}
	if got, _ := d["model"].(string); got != model {
		t.Errorf("load_degraded.model = %q, want %q", got, model)
	}
	if got, _ := d["severity"].(string); got != "warning" {
		t.Errorf("load_degraded.severity = %q, want warning (модель работает)", got)
	}
	if _, ok := d["diagnostics"].(map[string]interface{}); !ok {
		t.Errorf("load_degraded.diagnostics отсутствует: %v", d["diagnostics"])
	}
}

// TestNoteLoadDegradation_GpuLayersClears — факт важнее плана: если модель
// загрузилась хотя бы с одним слоем на GPU, запись о деградации снимается, даже
// если вердикт до загрузки говорил cpu_only.
func TestNoteLoadDegradation_GpuLayersClears(t *testing.T) {
	oldBackend := backend
	defer func() { backend = oldBackend }()
	backend = newTestBackend()

	degradedLoads.clear("m")
	degradedLoads.record(&DegradedNotice{Model: "m", Stage: "cpu_only", At: time.Now()})

	noteLoadDegradation("m", cppbackend.LoadModelOpts{GPULayers: 22}, backend)
	if _, ok := degradedLoads.get("m"); ok {
		t.Fatal("деградация не снята при 22 слоях на GPU — оператор получит ложное предупреждение")
	}
}

// TestNoteLoadDegradation_CPUOnlyWithMetadata — cpu_only с метаданными: запись
// собирается ИЗ ВЕРДИКТА memfit, а не выдумывается, поэтому числа в /api/models
// совпадают с логом раскладки.
//
// Стенд настоящий: fake-GGUF в temp-каталоге (см. setupBackendWithModel) и
// подменённые ресурсы. Модель крошечная по файлу, но на 1 GiB VRAM в неё не
// влезает ни один слой при весах ~470 MiB + KV → вердикт обязан быть cpu_only.
// Именно этот случай НЕ должен давать 413: RAM хватает, модель отработает.
func TestNoteLoadDegradation_CPUOnlyWithMetadata(t *testing.T) {
	oldBackend := backend
	defer func() { backend = oldBackend }()

	const model = "degraded-cpu-only-model"
	dir := t.TempDir()
	path := filepath.Join(dir, model+".gguf")
	// 64 слоя, 8 голов KV по 256 → KV-кэш не крошечный, веса ~470 MiB.
	makeFakeGGUF(t, path, "qwen3", 64, 24, 8, 5120)
	if err := os.Truncate(path, 470*1024*1024); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	// 1 GiB VRAM (свободно столько же) и 20 GiB RAM: веса + KV в VRAM не влезут,
	// в RAM — влезут с запасом.
	t.Setenv("CPPWORKER_VRAM_BYTES", "1073741824")
	t.Setenv("CPPWORKER_FREE_VRAM_BYTES", "1073741824")
	t.Setenv("CPPWORKER_AVAILABLE_RAM_BYTES", "21474836480")

	be := cppbackend.NewBackend(cppbackend.Config{
		ModelsDir: dir, DefaultCtxSize: 32768, DefaultGPULayers: 32,
	})
	if err := be.Init(); err != nil {
		t.Logf("Init warning (ожидаемо в stub): %v", err)
	}
	mm := be.ModelManager()
	if mm == nil {
		t.Fatal("ModelManager недоступен")
	}
	if _, err := mm.ScanModels(); err != nil {
		t.Fatalf("ScanModels: %v", err)
	}
	if err := be.InjectLoadedModelForTest(model + ".gguf"); err != nil {
		t.Fatalf("InjectLoadedModelForTest: %v", err)
	}
	backend = be

	// Сначала убеждаемся, что вердикт действительно cpu_only — иначе тест
	// проверял бы не то (например, после правки бюджета модель влезла бы в VRAM).
	info, err := be.GetModel(model + ".gguf")
	if err != nil || info == nil {
		t.Fatalf("GetModel: %v", err)
	}
	v, ok := memfitPlanForModel(*info, "q8_0")
	if !ok {
		t.Fatal("memfit не смог судить о модели — тест не проверяет cpu_only-путь")
	}
	if v.Stage != memfit.StageCPUOnly {
		t.Fatalf("стадия = %s, want cpu_only (VRAM %s, RAM %s)",
			v.Stage, v.UsableVRAM, v.UsableRAM)
	}

	noteLoadDegradation(model+".gguf", cppbackend.LoadModelOpts{
		GPULayers: 0, ContextSize: 32768, KVCacheType: "q8_0",
	}, backend)

	d, ok := degradedLoads.get(model + ".gguf")
	if !ok {
		t.Fatal("cpu_only не записан — оператор не узнает, что модель работает без GPU")
	}
	if d.Stage != "cpu_only" {
		t.Errorf("Stage = %q, want cpu_only", d.Stage)
	}
	if d.Reason != "insufficient_resources" {
		t.Errorf("Reason = %q (этот код читает балансер)", d.Reason)
	}
	if _, hasGPU := d.Numbers["gpu_layers"]; !hasGPU {
		t.Error("в diagnostics нет gpu_layers — числа вердикта потеряны")
	}
	degradedLoads.clear(model + ".gguf")
}

// TestReload_InfeasibleIsRefusedNotLoaded — физически невыполнимый reload
// обязан получить 4xx ДО unload/load, с числами и подсказкой. Раньше такой
// запрос уходил в загрузку 5 GiB в RAM на машине с ~2 GiB доступной, и клиент
// получал таймаут вместо причины.
//
// ВАЖНО ПРО ПУТИ. В memfit-ветке гейт checkNCtxBeforeLoad стоит ПЕРВЫМ и уже
// отдаёт 422 n_ctx_infeasible, когда веса не помещаются никуда (MaxHardCtx=0).
// Поэтому сквозной тест обязан принимать любой из двух 4xx: 422 (гейт) — это
// более точный ответ, 413 (наш отказ по вердикту does_not_fit) — страховка на
// случай, когда гейт и раскладка разошлись по типу KV или n_ctx. Проверяем
// главное: загрузка НЕ началась, а ответ содержит числа.
func TestReload_InfeasibleIsRefusedNotLoaded(t *testing.T) {
	oldBackend := backend
	defer func() { backend = oldBackend }()

	const name = "nofit-model"
	be, filename := backendWithTruncatedModel(t, name, 64, 24, 8, 5120, 5*1024*1024*1024)
	backend = be

	// 5 GiB весов при 4 GiB RAM (резерв 4 GiB, доступно ≈0) и 8 GiB VRAM:
	// не влезает никуда.
	t.Setenv("CPPWORKER_AVAILABLE_RAM_BYTES", "4294967296")
	t.Setenv("CPPWORKER_VRAM_BYTES", "8589934592")
	t.Setenv("CPPWORKER_FREE_VRAM_BYTES", "8589934592")

	info, err := be.GetModel(filename)
	if err != nil || info == nil {
		t.Fatalf("GetModel: %v", err)
	}
	v, ok := memfitPlanForModel(*info, "f16")
	if !ok {
		t.Fatal("memfit не смог судить — тест не проверяет отказ")
	}
	if v.Stage != memfit.StageDoesNotFit {
		t.Fatalf("стадия = %s (VRAM %s, RAM %s), want does_not_fit",
			v.Stage, v.UsableVRAM, v.UsableRAM)
	}

	cfg := &cppbackend.Config{DefaultCtxSize: 4096, DefaultGPULayers: 22}
	withConfig(t, cfg, true) // auto_offload=true, gpuLayers не задан → путь memfit

	body := `{"name":"` + filename + `","contextSize":4096}`
	req := httptest.NewRequest(http.MethodPost, "/api/models/reload", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handleReloadModel(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge && rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 413 (наш отказ) или 422 (гейт n_ctx); загрузка не должна начинаться. body=%s",
			rec.Code, rec.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v", err)
	}
	// max_viable_n_ctx=0 здесь ожидаем: веса не помещаются никуда, и это ровно
	// то, что должен увидеть оператор (а не «загрузка идёт, подождите 90 с»).
	if _, has := resp["max_viable_n_ctx"]; !has {
		if _, hasHard := resp["hard_max_n_ctx"]; !hasHard {
			t.Errorf("в ответе нет чисел о потолке n_ctx: %v", resp)
		}
	}
	// И сам отказ обязан быть опознаваемым клиентом кодом, а не 500.
	if rec.Code == http.StatusRequestEntityTooLarge {
		if code, _ := resp["code"].(float64); int(code) != 6 {
			t.Errorf("code = %v, want 6 (insufficient_resources)", resp["code"])
		}
		bi, ok := resp["bridge_info"].(map[string]interface{})
		if !ok {
			t.Fatalf("bridge_info отсутствует — клиент не увидит чисел: %v", resp)
		}
		if got, _ := bi["reason"].(string); got == "" {
			t.Error("reason пуст — клиент не поймёт, что именно уменьшать")
		}
	}
}

// TestReload_ForceOverridesRefusal — escape-hatch: оператор, который сознательно
// хочет попробовать, ставит force и получает не 413, а запуск загрузки.
func TestReload_ForceOverridesRefusal(t *testing.T) {
	oldBackend := backend
	defer func() { backend = oldBackend }()

	const name = "nofit-force-model"
	be, filename := backendWithTruncatedModel(t, name, 64, 24, 8, 5120, 5*1024*1024*1024)
	backend = be

	t.Setenv("CPPWORKER_AVAILABLE_RAM_BYTES", "4294967296")
	t.Setenv("CPPWORKER_VRAM_BYTES", "8589934592")
	t.Setenv("CPPWORKER_FREE_VRAM_BYTES", "8589934592")

	cfg := &cppbackend.Config{DefaultCtxSize: 4096, DefaultGPULayers: 22}
	withConfig(t, cfg, true)

	body := `{"name":"` + filename + `","contextSize":4096,"force":true}`
	req := httptest.NewRequest(http.MethodPost, "/api/models/reload", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handleReloadModel(rec, req)

	if rec.Code == http.StatusRequestEntityTooLarge {
		t.Fatalf("force=true не перебил отказ: status=413, body=%s", rec.Body.String())
	}
}

// TestReload_DegradedHeaderOnCPUOnly — вариант D сквозным путём: cpu_only не
// блокирует reload, а помечает ответ заголовком, чтобы клиент узнал режим.
func TestReload_DegradedHeaderOnCPUOnly(t *testing.T) {
	oldBackend := backend
	defer func() { backend = oldBackend }()

	const name = "cpuonly-model"
	// 470 MiB весов, 64 слоя → при 1 GiB VRAM ни один слой не влезает, но RAM
	// хватает с запасом: это и есть cpu_only, а не does_not_fit.
	be, filename := backendWithTruncatedModel(t, name, 64, 24, 8, 5120, 470*1024*1024)
	backend = be

	t.Setenv("CPPWORKER_AVAILABLE_RAM_BYTES", "21474836480")
	t.Setenv("CPPWORKER_VRAM_BYTES", "1073741824")
	t.Setenv("CPPWORKER_FREE_VRAM_BYTES", "1073741824")

	info, err := be.GetModel(filename)
	if err != nil || info == nil {
		t.Fatalf("GetModel: %v", err)
	}
	v, ok := memfitPlanForModel(*info, "f16")
	if !ok || v.Stage != memfit.StageCPUOnly {
		t.Fatalf("подготовка теста: стадия = %s, ok=%v; want cpu_only", v.Stage, ok)
	}

	cfg := &cppbackend.Config{DefaultCtxSize: 4096, DefaultGPULayers: 22}
	withConfig(t, cfg, true)

	// ?wait=false (по умолчанию) — 202 сразу, фоновая загрузка. n_ctx=8192
	// больше текущего (4096), поэтому skip-check «параметры уже достаточны» не
	// срабатывает и решение о раскладке действительно принимается.
	body := `{"name":"` + filename + `","contextSize":8192}`
	req := httptest.NewRequest(http.MethodPost, "/api/models/reload", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handleReloadModel(rec, req)

	if rec.Code == http.StatusRequestEntityTooLarge {
		t.Fatalf("cpu_only отказал 413 — решение D нарушено, body=%s", rec.Body.String())
	}
	// n_ctx_infeasible сюда попасть не должен: RAM хватает, это не отказ.
	if rec.Code == http.StatusUnprocessableEntity {
		t.Fatalf("cpu_only отвергнут гейтом n_ctx: %s", rec.Body.String())
	}
	if got := rec.Header().Get("X-CppWorker-Degraded-Stage"); got != "cpu_only" {
		t.Errorf("X-CppWorker-Degraded-Stage = %q, want cpu_only (status=%d, body=%s)",
			got, rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-CppWorker-Degraded"); got != "insufficient_resources" {
		t.Errorf("X-CppWorker-Degraded = %q", got)
	}

	// R91 (2026-10-09): дождаться фоновой перезагрузки ДО возврата из теста.
	// Она пишет файлы в ModelsDir (t.TempDir), поэтому cleanup каталога падал на
	// «directory not empty» (Linux) / «The directory is not empty» (Windows) —
	// ожидание в t.Cleanup тут не помогает: TempDir-removal идёт после него, но
	// горутина успевает создать файл уже во время обхода каталога.
	if !WaitAsyncWork(15 * time.Second) {
		t.Fatal("фоновая перезагрузка не завершилась за 15 с")
	}
}

// backendWithTruncatedModel — стенд: fake-GGUF нужного размера + loaded-модель.
//
// Размер задаётся truncate, а не записью нулей: 5 GiB padding'а в CI недопустимы,
// а GGUF-заголовок читается лениво с начала файла, поэтому truncate корректен.
func backendWithTruncatedModel(t *testing.T, name string, nLayers, nHeads, nKvHeads, nEmbd uint32, size int64) (*cppbackend.Backend, string) {
	t.Helper()
	dir := t.TempDir()
	// R91 (2026-10-09): тесты на этом стенде запускают ФОНОВУЮ загрузку/перезагрузку
	// (handleReloadModel с wait=false), а она держит файлы в dir. Cleanup'ы идут
	// LIFO, поэтому ожидание, зарегистрированное ПОСЛЕ t.TempDir(), выполняется
	// РАНЬШЕ удаления каталога — иначе на Windows падало
	// «TempDir RemoveAll cleanup: The directory is not empty», а под -race ещё и
	// гонка с Close().
	t.Cleanup(func() {
		if !WaitAsyncWork(15 * time.Second) {
			t.Logf("cleanup: фоновая загрузка/перезагрузка не завершилась за 15 с")
		}
	})
	filename := name + ".gguf"
	path := filepath.Join(dir, filename)
	makeFakeGGUF(t, path, "qwen3", nLayers, nHeads, nKvHeads, nEmbd)
	if err := os.Truncate(path, size); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	be := cppbackend.NewBackend(cppbackend.Config{
		ModelsDir: dir, DefaultCtxSize: 4096, DefaultGPULayers: 22,
	})
	if err := be.Init(); err != nil {
		t.Logf("Init warning (ожидаемо в stub): %v", err)
	}
	mm := be.ModelManager()
	if mm == nil {
		t.Fatal("ModelManager недоступен")
	}
	if _, err := mm.ScanModels(); err != nil {
		t.Fatalf("ScanModels: %v", err)
	}
	if err := be.InjectLoadedModelForTest(filename); err != nil {
		t.Fatalf("InjectLoadedModelForTest: %v", err)
	}
	return be, filename
}

// TestDegraded_NotRecordedAsFailure — инварианта варианта D: деградация НЕ
// является провалом загрузки. Если бы она писалась в load_failures, балансер
// опубликовал бы «не хватило памяти» и позже «загрузка восстановлена» про
// модель, которая успешно работает.
func TestDegraded_NotRecordedAsFailure(t *testing.T) {
	const model = "degraded-invariant-model"
	for name := range loadFailures.list() {
		loadFailures.clear(name)
	}
	defer loadFailures.clear(model)
	defer degradedLoads.clear(model)

	degradedLoads.record(&DegradedNotice{
		Model:  model,
		Stage:  "cpu_only",
		Reason: "insufficient_resources",
		At:     time.Now().UTC(),
	})

	if _, ok := loadFailures.get(model); ok {
		t.Fatal("деградация записана как провал загрузки — оператор получит ложное уведомление")
	}
	// И в /api/models оба поля не должны появляться одновременно для этой модели.
	if _, ok := loadFailures.latest(); ok {
		t.Fatal("реестр провалов не пуст после очистки")
	}
}

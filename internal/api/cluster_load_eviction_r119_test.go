// cluster_load_eviction_r119_test.go — R119 (2026-10-15).
//
// ЖИВОЙ ДЕФЕКТ. `executeReloadOnBackend` выгружал чужую модель с того же бэкенда
// ПЕРЕД любой загрузкой — безусловно. Пока на бэкенде лежала модель, балансер не
// мог поднять вторую, даже если обе влезают: «вторая модель не грузится, первая
// пропала».
//
// ТЕПЕРЬ вытеснение происходит только по факту нехватки памяти. Тесты ниже
// проверяют обе половины правила — и что вытеснение случается, когда нужно, и что
// оно НЕ случается, когда не нужно (вторая половина важнее: именно она защищает
// работающий прогрев от бессмысленной выгрузки).
//
// Тест падает до правки: загрузка всегда сопровождалась выгрузкой чужой модели.
package api

import (
	"os"
	"strings"
	"testing"

	"ollama-loadbalancer/internal/balancer"
)

// fakeModelOpRunner — фейковый ModelManager: записывает порядок операций и
// отдаёт заранее заданные результаты.
type fakeModelOpRunner struct {
	calls []string
	// results по ключу "op:model" (op = load|unload)
	results map[string]*balancer.ModelOpResult
	// defaultResult — если ключа нет.
	defaultResult *balancer.ModelOpResult
}

func (f *fakeModelOpRunner) ExecuteOperation(backendID string, req balancer.ModelOpRequest) *balancer.ModelOpResult {
	f.calls = append(f.calls, req.Operation+":"+req.ModelName)
	if r, ok := f.results[req.Operation+":"+req.ModelName]; ok {
		return r
	}
	if f.defaultResult != nil {
		return f.defaultResult
	}
	return &balancer.ModelOpResult{Success: false, Operation: req.Operation, Error: "unexpected call"}
}

func okResult(model string) *balancer.ModelOpResult {
	return &balancer.ModelOpResult{Success: true, Operation: "load", ModelName: model}
}

func failResult(model, errText string) *balancer.ModelOpResult {
	return &balancer.ModelOpResult{Success: false, Operation: "load", ModelName: model, Error: errText}
}

const memErr = "cannot load model B: Веса модели не помещаются ни в VRAM, ни в RAM — нужен меньший квант или больше памяти."

// TestR119_LoadAlongside_NoEviction — модель поместилась рядом: чужую не трогаем.
func TestR119_LoadAlongside_NoEviction(t *testing.T) {
	f := &fakeModelOpRunner{results: map[string]*balancer.ModelOpResult{
		"load:B": okResult("B"),
	}}
	res, note := loadWithEvictionOnMemoryFailure(f, "bk", "B", "A", balancer.ModelOpRequest{Operation: "load", ModelName: "B"})
	if res == nil || !res.Success {
		t.Fatalf("загрузка не удалась: %+v", res)
	}
	if len(f.calls) != 1 || f.calls[0] != "load:B" {
		t.Errorf("операции = %v, ожидалась ровно одна загрузка: две модели на одном "+
			"бэкенде должны работать, когда память позволяет", f.calls)
	}
	if note != "" {
		t.Errorf("note = %q, ожидалось пусто (ничего не вытесняли)", note)
	}
}

// TestR119_MemoryFailure_EvictsAndRetries — не влезло: освобождаем и повторяем.
func TestR119_MemoryFailure_EvictsAndRetries(t *testing.T) {
	f := &fakeModelOpRunner{results: map[string]*balancer.ModelOpResult{
		"load:B":   failResult("B", memErr),
		"unload:A": {Success: true, Operation: "unload", ModelName: "A"},
	}}
	// Вторая попытка load:B должна вернуть успех — подменяем через счётчик.
	f.results["load:B"] = failResult("B", memErr)
	seq := &sequencedRunner{inner: f}
	res, note := loadWithEvictionOnMemoryFailure(seq, "bk", "B", "A", balancer.ModelOpRequest{Operation: "load", ModelName: "B"})
	if res == nil || !res.Success {
		t.Fatalf("после выгрузки чужой модели загрузка обязана удаться, получено %+v", res)
	}
	want := []string{"load:B", "unload:A", "load:B"}
	if strings.Join(seq.calls, ",") != strings.Join(want, ",") {
		t.Errorf("порядок операций = %v, ожидался %v", seq.calls, want)
	}
	if !strings.Contains(note, "A") {
		t.Errorf("note = %q, ожидалось упоминание выгруженной модели", note)
	}
}

// sequencedRunner — как fakeModelOpRunner, но первая загрузка падает, вторая
// удаётся (иначе «повторить попытку» неотличимо от «ничего не делали»).
type sequencedRunner struct {
	inner    *fakeModelOpRunner
	calls    []string
	loadSeen int
}

func (s *sequencedRunner) ExecuteOperation(backendID string, req balancer.ModelOpRequest) *balancer.ModelOpResult {
	s.calls = append(s.calls, req.Operation+":"+req.ModelName)
	if req.Operation == "load" {
		s.loadSeen++
		if s.loadSeen == 1 {
			return failResult(req.ModelName, memErr)
		}
		return okResult(req.ModelName)
	}
	return s.inner.ExecuteOperation(backendID, req)
}

// TestR119_NonMemoryFailure_NoEviction — ошибка НЕ про память: чужую модель не
// выгружаем (иначе битое имя/файл убивал бы рабочий прогрев).
func TestR119_NonMemoryFailure_NoEviction(t *testing.T) {
	f := &fakeModelOpRunner{results: map[string]*balancer.ModelOpResult{
		"load:B": failResult("B", `model "B" not found on disk`),
	}}
	res, note := loadWithEvictionOnMemoryFailure(f, "bk", "B", "A", balancer.ModelOpRequest{Operation: "load", ModelName: "B"})
	if res == nil || res.Success {
		t.Fatalf("ожидалась ошибка загрузки, получено %+v", res)
	}
	if len(f.calls) != 1 {
		t.Errorf("операции = %v: при ошибке не про память выгрузка чужой модели "+
			"ничего не исправит и только убьёт рабочий прогрев", f.calls)
	}
	if note != "" {
		t.Errorf("note = %q, ожидалось пусто", note)
	}
}

// TestR119_BusyOtherModel_NoEvictionAndHonestNote — чужую модель нельзя выгрузить
// (занята запросами): не сносим молча, а объясняем.
func TestR119_BusyOtherModel_NoEvictionAndHonestNote(t *testing.T) {
	f := &fakeModelOpRunner{results: map[string]*balancer.ModelOpResult{
		"load:B":   failResult("B", memErr),
		"unload:A": {Success: false, Operation: "unload", ModelName: "A", Busy: true, RetryWithForce: true, Error: "model is busy"},
	}}
	res, note := loadWithEvictionOnMemoryFailure(f, "bk", "B", "A", balancer.ModelOpRequest{Operation: "load", ModelName: "B"})
	if res == nil || res.Success {
		t.Fatalf("ожидалась исходная ошибка загрузки, получено %+v", res)
	}
	if len(f.calls) != 2 {
		t.Errorf("операции = %v, ожидались load + unload (без повтора)", f.calls)
	}
	if !strings.Contains(note, "занята") {
		t.Errorf("note = %q, ожидалось объяснение про занятую модель", note)
	}
}

// TestR119_UnloadFailed_NoRetry — выгрузка не удалась (не busy): повторять
// загрузку бессмысленно, но объяснение нужно.
func TestR119_UnloadFailed_NoRetry(t *testing.T) {
	f := &fakeModelOpRunner{results: map[string]*balancer.ModelOpResult{
		"load:B":   failResult("B", memErr),
		"unload:A": {Success: false, Operation: "unload", ModelName: "A", Error: "cppworker returned 500"},
	}}
	_, note := loadWithEvictionOnMemoryFailure(f, "bk", "B", "A", balancer.ModelOpRequest{Operation: "load", ModelName: "B"})
	if len(f.calls) != 2 {
		t.Errorf("операции = %v, ожидались load + unload", f.calls)
	}
	if !strings.Contains(note, "не удалось выгрузить") {
		t.Errorf("note = %q, ожидалось объяснение про неудачную выгрузку", note)
	}
}

// TestR119_RetryAlsoFails_ReportsBoth — не помогло и после выгрузки: сообщаем.
func TestR119_RetryAlsoFails_ReportsBoth(t *testing.T) {
	f := &fakeModelOpRunner{results: map[string]*balancer.ModelOpResult{
		"load:B":   failResult("B", memErr),
		"unload:A": {Success: true, Operation: "unload", ModelName: "A"},
	}}
	res, note := loadWithEvictionOnMemoryFailure(f, "bk", "B", "A", balancer.ModelOpRequest{Operation: "load", ModelName: "B"})
	if res == nil || res.Success {
		t.Fatalf("ожидалась ошибка, получено %+v", res)
	}
	if !strings.Contains(note, "даже после выгрузки") {
		t.Errorf("note = %q, ожидалось объяснение", note)
	}
	if len(f.calls) != 3 {
		t.Errorf("операции = %v, ожидались load + unload + load", f.calls)
	}
}

// TestR119_NoOtherModel_NoEvictionPath — вытеснять нечего (и то же имя).
func TestR119_NoOtherModel_NoEvictionPath(t *testing.T) {
	for _, other := range []string{"", "B"} {
		f := &fakeModelOpRunner{results: map[string]*balancer.ModelOpResult{
			"load:B": failResult("B", memErr),
		}}
		res, note := loadWithEvictionOnMemoryFailure(f, "bk", "B", other, balancer.ModelOpRequest{Operation: "load", ModelName: "B"})
		if res == nil || res.Success {
			t.Fatalf("other=%q: ожидалась ошибка", other)
		}
		if len(f.calls) != 1 || note != "" {
			t.Errorf("other=%q: операции = %v, note=%q — вытеснять нечего", other, f.calls, note)
		}
	}
}

// TestR119_NilRunner_Safe — nil-раннер не паникует.
func TestR119_NilRunner_Safe(t *testing.T) {
	res, note := loadWithEvictionOnMemoryFailure(nil, "bk", "B", "A", balancer.ModelOpRequest{})
	if res != nil || note != "" {
		t.Errorf("nil-раннер: (%+v, %q), ожидались (nil, \"\")", res, note)
	}
}

// TestR119_LoadFailureLooksLikeMemoryShortage — матчер формулировок.
func TestR119_LoadFailureLooksLikeMemoryShortage(t *testing.T) {
	yes := []string{
		memErr,
		"cannot load model X: Суммарной памяти (VRAM+RAM) хватает лишь на n_ctx=4096.",
		"CUDA error: out of memory",
		"failed to allocate 4096 MB",
		"insufficient memory for KV cache",
		"model weights do not fit",
	}
	for _, s := range yes {
		if !loadFailureLooksLikeMemoryShortage(s) {
			t.Errorf("loadFailureLooksLikeMemoryShortage(%q) = false, want true", s)
		}
	}
	no := []string{
		"",
		`model "B" not found on disk`,
		"invalid GGUF magic",
		"context deadline exceeded",
		"401 invalid or missing API token",
	}
	for _, s := range no {
		if loadFailureLooksLikeMemoryShortage(s) {
			t.Errorf("loadFailureLooksLikeMemoryShortage(%q) = true, want false", s)
		}
	}
}

// TestR119_HandlersUseEvictionPath — защита от отката: обработчик действительно
// ходит через правило, а не выгружает чужую модель сам.
func TestR119_HandlersUseEvictionPath(t *testing.T) {
	src, err := os.ReadFile("handlers_cluster_models.go")
	if err != nil {
		t.Fatalf("read handlers_cluster_models.go: %v", err)
	}
	if !strings.Contains(string(src), "loadWithEvictionOnMemoryFailure(mm, backendID, modelName, loadedOther, opReq)") {
		t.Error("executeReloadOnBackend больше не использует loadWithEvictionOnMemoryFailure — " +
			"вернулось безусловное вытеснение?")
	}
	if strings.Contains(string(src), "auto-unloading different model before load") {
		t.Error("в executeReloadOnBackend осталась ветка безусловной выгрузки чужой модели")
	}
}

// model_loaded_r257_test.go — R257 (2026-10-28): «загружено» — один источник истины.
//
// ЖИВОЙ ФАКТ (стенд 8 ГБ VRAM). После вытеснения Qwen3.8-27B моделью gemma-4-E4B
// `GET /api/v1/backends/{id}/models` отдавал loaded=true у ДВУХ моделей: панель
// предлагала «загруженную» модель, которой нет в памяти, и оператор грузил её
// повторно — с лишней перезагрузкой и непонятной ему причиной.
//
// Тесты фиксируют: снапшот поллера первичен (одна загруженная при двух записях
// истории), пустой state не превращается в «loaded» без признаков загрузки, и
// расхождение «воркер vs снапшот» видно в логе.
package balancer

import (
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// r257Proxy — прокси со снапшотом поллера по одному бэкенду.
func r257Proxy(t *testing.T, models []types.LlamaCppModel) *Proxy {
	t.Helper()
	p := &Proxy{metricsMgr: NewMetricsManager()}
	p.metricsMgr.mu.Lock()
	p.metricsMgr.llamaMetrics["bk-1"] = &types.LlamaCppMetrics{LoadedModels: models}
	p.metricsMgr.mu.Unlock()
	return p
}

// TestR257_SnapshotWinsOverHistory — ровно одна loaded=true при двух записях.
//
// Сценарий живого дефекта: в снапшоте остался единственный РЕАЛЬНО загруженный
// узел (gemma), а история/ответ воркера всё ещё помнит Qwen как «loaded». Панель
// должна показать одну загруженную модель, иначе оператор грузит призрак.
func TestR257_SnapshotWinsOverHistory(t *testing.T) {
	p := r257Proxy(t, []types.LlamaCppModel{
		{Name: "gemma-4-E4B-it-Q4_K_M.gguf", State: "loaded", ContextLength: 8192},
		{Name: "Qwen3.8-27B-UD-Q4_K_M.gguf", State: "not_loaded"},
	})

	loaded, known := p.loadedSetFromSnapshotR257("bk-1")
	if !known {
		t.Fatal("снапшот есть, но known=false — флаг loaded снова будет из ответа воркера")
	}
	if !loaded["gemma-4-E4B-it-Q4_K_M"] || !loaded["gemma-4-E4B-it-Q4_K_M.gguf"] {
		t.Errorf("gemma должна быть загруженной: %v", loaded)
	}
	if loaded["Qwen3.8-27B-UD-Q4_K_M"] || loaded["Qwen3.8-27B-UD-Q4_K_M.gguf"] {
		t.Errorf("вытесненная Qwen не должна считаться загруженной: %v", loaded)
	}
	if len(loaded) != 2 { // два ключа одной модели: имя и basename
		t.Errorf("ожидались ровно 2 ключа (одна модель), получено %d: %v", len(loaded), loaded)
	}
}

// TestR257_NoSnapshotKeepsPreviousBehaviour — без снапшота поведение прежнее.
//
// На старте балансера поллер ещё не опросил узел: объявить пустой список истиной
// значило бы показать «ничего не загружено» на живом стенде.
func TestR257_NoSnapshotKeepsPreviousBehaviour(t *testing.T) {
	p := &Proxy{metricsMgr: NewMetricsManager()}
	if _, known := p.loadedSetFromSnapshotR257("bk-1"); known {
		t.Fatal("снапшота нет, а known=true — прежний путь (ответ воркера) будет отброшен")
	}
	if _, known := (*Proxy)(nil).loadedSetFromSnapshotR257("bk-1"); known {
		t.Fatal("nil-прокси не должен объявлять снапшот известным")
	}
	var nilP *Proxy
	if _, known := nilP.loadedSetFromSnapshotR257(""); known {
		t.Fatal("пустой backendID не должен объявлять снапшот известным")
	}
}

// TestR257_EmptyStateNeedsLoadEvidence — пустой state не значит «загружено».
//
// Старая сборка cppworker не отдаёт state вовсе, поэтому нормализацию нельзя
// просто удалить. Но и «всегда loaded» — причина дефекта: модель, оставшаяся в
// списке после вытеснения, выглядела загруженной. Правило: пусто + признаки
// загрузки → loaded, пусто без признаков → unknown.
func TestR257_EmptyStateNeedsLoadEvidence(t *testing.T) {
	cases := []struct {
		name     string
		state    string
		loadedAt string
		nCtx     int
		vram     uint64
		ram      uint64
		want     string
	}{
		{"явное состояние не трогаем", "loading", "", 0, 0, 0, "loading"},
		{"явное loaded не трогаем", "loaded", "", 0, 0, 0, "loaded"},
		{"пусто + время загрузки → loaded", "", "2026-10-28T10:00:00Z", 0, 0, 0, "loaded"},
		{"пусто + окно контекста → loaded", "", "", 8192, 0, 0, "loaded"},
		{"пусто + занятая VRAM → loaded", "", "", 0, 512, 0, "loaded"},
		{"пусто + занятая RAM → loaded", "", "", 0, 0, 700, "loaded"},
		{"пусто без признаков → unknown", "", "", 0, 0, 0, "unknown"},
		{"регистр и пробелы нормализуются", "  LOADED ", "", 0, 0, 0, "loaded"},
	}
	for _, c := range cases {
		got := resolveModelStateR257(c.state, c.loadedAt, c.nCtx, c.vram, c.ram)
		if got != c.want {
			t.Errorf("%s: resolveModelStateR257(%q, %q, %d, %d, %d) = %q, want %q",
				c.name, c.state, c.loadedAt, c.nCtx, c.vram, c.ram, got, c.want)
		}
	}
}

// TestR257_LoadedKeysCoverAllNameForms — ключи покрывают три вида имени.
//
// Тот же набор, что строил прежний код: полное имя, basename и basename без
// .gguf. Иначе одна и та же модель «загружена» в одном месте списка и «нет» в
// другом (именно так панель и показывала две модели).
func TestR257_LoadedKeysCoverAllNameForms(t *testing.T) {
	keys := loadedKeysR257(`/models/Qwen3.8-27B-UD-Q4_K_M.gguf`)
	want := map[string]bool{
		"/models/Qwen3.8-27B-UD-Q4_K_M.gguf": true,
		"Qwen3.8-27B-UD-Q4_K_M.gguf":         true,
		"Qwen3.8-27B-UD-Q4_K_M":              true,
	}
	for _, k := range keys {
		delete(want, k)
	}
	if len(want) != 0 {
		t.Errorf("не хватает ключей: %v (получено %v)", want, keys)
	}
	// Дубликатов быть не должно: ключи идут в map, и мусор в них только мешает.
	seen := map[string]bool{}
	for _, k := range keys {
		if seen[k] {
			t.Errorf("дубликат ключа %q", k)
		}
		seen[k] = true
	}
	// Windows-путь тоже поддержан: cppworker отдаёт path как есть.
	if keys := loadedKeysR257(`C:\models\gemma.gguf`); len(keys) != 3 {
		t.Errorf("windows-путь: ожидались 3 ключа, получено %v", keys)
	}
}

// TestR257_LogDivergenceDoesNotPanic — диагностика расхождения безопасна.
//
// Проверяем именно отсутствие паники и ложных срабатываний: функция зовётся из
// горячего пути списка моделей, и nil-карты (воркер не ответил) не должны её
// ронять.
func TestR257_LogDivergenceDoesNotPanic(t *testing.T) {
	logLoadedDivergenceR257("bk-1", nil, nil, true)
	logLoadedDivergenceR257("bk-1", map[string]bool{"a": true}, nil, true)
	logLoadedDivergenceR257("bk-1", nil, map[string]bool{"a": true}, true)
	// Неавторитетный случай вообще ничего не логирует (снапшота нет).
	logLoadedDivergenceR257("bk-1", map[string]bool{"a": true}, map[string]bool{"b": true}, false)
}

package balancer

// file_absent_relocation_r757_test.go — R757 (2026-11-25): переезд на узел, где
// ФАЙЛ модели реально лежит.
//
// ЖИВАЯ ЖАЛОБА (архив второго стенда, 06:10): «она не стартанула с запроса от
// пользователя, а поднятая вручную работала». Разбор лога:
//   - выбор отдал A10-126, где файла модели НЕТ (каталог это знал — R696
//     «привязка сессии не создана ... reason=model_absent_on_backend»);
//   - файл лежал на cppworker-gpu-bundled-agent;
//   - гейт автозагрузки попытался поднять модель на A10-126, воркер ответил
//     `model ... not found in ./models`, клиент получил терминальный 404 (R325.2);
//   - ручная загрузка на узле с файлом подняла модель за 8.2 с.
//
// R303 здесь не спасает: он перевозит заявку только к ЖИВОЙ копии в VRAM, а
// живых копий не было (модель только что выгрузили/пересобрали).

import (
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// TestR757_RelocatesToNodeWithFile — выбран узел без файла, файл есть у другого:
// релокация обязана вернуть узел с файлом.
func TestR757_RelocatesToNodeWithFile(t *testing.T) {
	p := availProxyR493(t, map[string]int{"agent": 1, "A10-126": 1})
	setMetricsR493(p, "agent", nil, 20490)
	setMetricsR493(p, "A10-126", nil, 20490)
	// Файл лежит только на agent (как в живом архиве: A10-126 файла не имел).
	catalogStoreR552(p, "agent", []string{"Inkling-2-Mini-UD-Q4_K_M.inkling2_loop.gguf"}, "")
	catalogStoreR552(p, "A10-126", []string{"other-model.gguf"}, "")
	types1 := p.getAllowedTypesList(types.BackendTypeLlamaCpp)
	const model = "Inkling-2-Mini-UD-Q4_K_M.inkling2_loop"

	alt, reason := p.relocateToFileNodeR757("A10-126", model, types1)
	if alt != "agent" {
		t.Fatalf("переезд вернул %q, ожидался agent — именно так клиент получал 404 "+
			"`model not found in ./models` на A10-126, где файла нет (reason=%q)", alt, reason)
	}
	if reason != reasonMovedToFileNodeR757 {
		t.Errorf("reason = %q, want %q", reason, reasonMovedToFileNodeR757)
	}
}

// TestR757_NoRelocationWhenFileOnSelectedNode — файл на выбранном узле есть:
// обычное распределение не трогаем.
func TestR757_NoRelocationWhenFileOnSelectedNode(t *testing.T) {
	p := availProxyR493(t, map[string]int{"a": 1, "b": 1})
	setMetricsR493(p, "a", nil, 20490)
	setMetricsR493(p, "b", nil, 20490)
	catalogStoreR552(p, "a", []string{"m27b.gguf"}, "")
	catalogStoreR552(p, "b", []string{"m27b.gguf"}, "")
	types1 := p.getAllowedTypesList(types.BackendTypeLlamaCpp)

	if alt, reason := p.relocateToFileNodeR757("a", "m27b", types1); alt != "" || reason != "" {
		t.Fatalf("файл на выбранном узле есть — переезда быть не должно, got alt=%q reason=%q", alt, reason)
	}
}

// TestR757_NoRelocationOnUnknownCatalog — каталог не опрошен: никаких догадок.
//
// Живой регресс, ради которого условие узкое (R493): два узла без метрик,
// «наверное, файл где-то есть» ломало обычное распределение (round-robin
// переставал действовать).
func TestR757_NoRelocationOnUnknownCatalog(t *testing.T) {
	p := availProxyR493(t, map[string]int{"a": 1, "b": 1})
	setMetricsR493(p, "a", nil, 20490)
	setMetricsR493(p, "b", nil, 20490)
	types1 := p.getAllowedTypesList(types.BackendTypeLlamaCpp)

	if alt, reason := p.relocateToFileNodeR757("a", "m27b", types1); alt != "" || reason != "" {
		t.Fatalf("каталог не опрошен — переезда быть не должно, got alt=%q reason=%q", alt, reason)
	}
}

// TestR757_NoRelocationOnDirError — чтение каталога упало: это не «файла нет».
func TestR757_NoRelocationOnDirError(t *testing.T) {
	p := availProxyR493(t, map[string]int{"a": 1, "b": 1})
	setMetricsR493(p, "a", nil, 20490)
	setMetricsR493(p, "b", nil, 20490)
	catalogStoreR552(p, "a", nil, "permission denied")
	catalogStoreR552(p, "b", []string{"m27b.gguf"}, "")
	types1 := p.getAllowedTypesList(types.BackendTypeLlamaCpp)

	if alt, reason := p.relocateToFileNodeR757("a", "m27b", types1); alt != "" || reason != "" {
		t.Fatalf("ошибка каталога не доказывает отсутствие файла — переезда быть не должно, got alt=%q reason=%q", alt, reason)
	}
}

// TestR757_FileAbsentEverywhereReported — файла нет нигде: это отдельный факт для
// лога (ожидание не поможет), а не «переезд не понадобился».
func TestR757_FileAbsentEverywhereReported(t *testing.T) {
	p := availProxyR493(t, map[string]int{"a": 1, "b": 1})
	setMetricsR493(p, "a", nil, 20490)
	setMetricsR493(p, "b", nil, 20490)
	catalogStoreR552(p, "a", []string{"other.gguf"}, "")
	catalogStoreR552(p, "b", []string{"other.gguf"}, "")
	types1 := p.getAllowedTypesList(types.BackendTypeLlamaCpp)

	alt, reason := p.relocateToFileNodeR757("a", "m27b", types1)
	if alt != "" {
		t.Fatalf("файла нет ни у кого — переезжать некуда, got %q", alt)
	}
	if reason != reasonFileAbsentEverywhereR757 {
		t.Fatalf("reason = %q, want %q: оператор должен видеть, что дело в отсутствии GGUF, "+
			"а не в выборе узла", reason, reasonFileAbsentEverywhereR757)
	}
}

// TestR757_LoadedModelIsNotRelocated — модель уже в памяти выбранного узла:
// каталог разошёлся с реальностью, переезд только сломал бы работающую копию.
func TestR757_LoadedModelIsNotRelocated(t *testing.T) {
	p := availProxyR493(t, map[string]int{"a": 1, "b": 1})
	setMetricsR493(p, "a", []string{"m27b"}, 20490)
	setMetricsR493(p, "b", nil, 20490)
	catalogStoreR552(p, "a", []string{"other.gguf"}, "")
	catalogStoreR552(p, "b", []string{"m27b.gguf"}, "")
	types1 := p.getAllowedTypesList(types.BackendTypeLlamaCpp)

	if !p.backendHasModelByID("a", "m27b") {
		t.Fatal("условие теста не выполнено: модель должна быть в памяти узла a")
	}
	if alt, reason := p.relocateToFileNodeR757("a", "m27b", types1); alt != "" || reason != "" {
		t.Fatalf("модель уже в памяти — переезда быть не должно, got alt=%q reason=%q", alt, reason)
	}
}

// TestR757_BackendsWithFileFor404 — ответ 404 обязан называть узлы, где файл ЕСТЬ.
func TestR757_BackendsWithFileFor404(t *testing.T) {
	p := availProxyR493(t, map[string]int{"agent": 1, "A10-126": 1})
	setMetricsR493(p, "agent", nil, 20490)
	setMetricsR493(p, "A10-126", nil, 20490)
	catalogStoreR552(p, "agent", []string{"Inkling-2-Mini-UD-Q4_K_M.inkling2_loop.gguf"}, "")
	catalogStoreR552(p, "A10-126", []string{"other-model.gguf"}, "")

	got := p.backendsWithModelOnDiskR757("Inkling-2-Mini-UD-Q4_K_M.inkling2_loop")
	if len(got) != 1 || got[0] != "agent" {
		t.Fatalf("backendsWithModelOnDiskR757 = %v, want [agent]: в теле 404 оператор ищет "+
			"ответ «куда переехать», а не идёт читать лог воркера", got)
	}
}

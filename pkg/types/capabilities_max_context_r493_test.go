// capabilities_max_context_r493_test.go — 12e (2026-11-12, по архиву стенда):
// «размер контекста сильно отличается».
//
// ЖИВОЙ ФАКТ: на стенде A10 ×2 один и тот же файл модели отдавался так:
//
//	мастер  — contextLength=198656, capabilities.maxContext=262144 (GGUF);
//	A10-126 — contextSize=131072,   capabilities.maxContext=131072 (загруженное окно).
//
// Агент, читая capabilities.maxContext, получал разные числа для одной модели.
//
// Правило после правки: MaxContext — ФИЗИЧЕСКИЙ предел из GGUF всегда; окно
// загруженной копии — отдельное поле LoadedContextSize.
package types

import "testing"

// TestR493_MaxContextIsGGUFNotLoadedWindow — главный случай из архива.
func TestR493_MaxContextIsGGUFNotLoadedWindow(t *testing.T) {
	caps := CapabilitiesFromModelInfo("Qwen3.8-27B-UD-Q4_K_M", true, "qwen35", 131072, 262144)
	if caps.MaxContext != 262144 {
		t.Fatalf("MaxContext = %d, ожидался физический предел GGUF 262144: иначе та же "+
			"модель отдаёт разный «размер контекста» с разных узлов", caps.MaxContext)
	}
	if caps.LoadedContextSize != 131072 {
		t.Fatalf("LoadedContextSize = %d, ожидалось окно загруженной копии 131072", caps.LoadedContextSize)
	}
}

// TestR493_MaxContextFallsBackToLoadedOnlyWithoutGGUF — если GGUF-предела нет,
// отдаём загруженное окно (честная деградация, а не ноль).
func TestR493_MaxContextFallsBackToLoadedOnlyWithoutGGUF(t *testing.T) {
	caps := CapabilitiesFromModelInfo("m", false, "llama", 8192, 0)
	if caps.MaxContext != 8192 {
		t.Fatalf("MaxContext = %d, ожидалось 8192 (GGUF неизвестен → загруженное окно)",
			caps.MaxContext)
	}
	if caps.LoadedContextSize != 8192 {
		t.Fatalf("LoadedContextSize = %d, ожидалось 8192", caps.LoadedContextSize)
	}
}

// TestR493_MaxContextSameAcrossNodes — у двух узлов с одним файлом GGUF-предел
// ОДИНАКОВЫЙ, даже если окна загрузки разные. Это и есть контракт для агента.
func TestR493_MaxContextSameAcrossNodes(t *testing.T) {
	master := CapabilitiesFromModelInfo("Qwen3.8-27B-UD-Q4_K_M", true, "qwen35", 198656, 262144)
	a10 := CapabilitiesFromModelInfo("Qwen3.8-27B-UD-Q4_K_M", true, "qwen35", 131072, 262144)

	if master.MaxContext != a10.MaxContext {
		t.Fatalf("maxContext расходится между узлами: %d vs %d — агент снова увидит "+
			"«разный размер контекста»", master.MaxContext, a10.MaxContext)
	}
	if master.LoadedContextSize == a10.LoadedContextSize {
		t.Logf("предупреждение: окна загрузки совпали (%d) — тест проверяет только MaxContext",
			master.LoadedContextSize)
	}
}

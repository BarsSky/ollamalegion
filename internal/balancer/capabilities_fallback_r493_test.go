// capabilities_fallback_r493_test.go — 12e (2026-11-12, по архиву стенда):
// «агент получает размер контекста, сильно отличающийся от действительности».
//
// ЖИВОЙ ФАКТ из архива (A10 ×2, один файл Qwen3.8-27B):
//
//	мастер  — contextLength=198656, capabilities.maxContext=262144;
//	A10-126 — contextSize=131072,   capabilities.maxContext=131072.
//
// Причина: в fallback (старый cppworker без capabilities) поллер передавал
// GGUF-предел ДВАЖДЫ — как «окно загрузки» и как «предел модели». После правки
// 12e у полей разные источники, и fallback обязан их не путать: иначе на узле
// с окном 131072 клиент снова увидит подмену.
package balancer

import "testing"

// TestR493_FallbackCapabilitiesKeepsLoadedWindowSeparate — окно копии и
// GGUF-предел идут в свои поля.
func TestR493_FallbackCapabilitiesKeepsLoadedWindowSeparate(t *testing.T) {
	caps := capabilitiesOrFallback(nil, "Qwen3.8-27B-UD-Q4_K_M", "qwen35",
		131072, // окно ЗАГРУЖЕННОЙ копии на этом узле
		262144, // физический предел из GGUF
		true)
	if caps == nil {
		t.Fatal("capabilitiesOrFallback вернул nil")
	}
	if caps.MaxContext != 262144 {
		t.Fatalf("MaxContext = %d, ожидался GGUF-предел 262144: иначе та же модель "+
			"отдаёт разный «размер контекста» с разных узлов", caps.MaxContext)
	}
	if caps.LoadedContextSize != 131072 {
		t.Fatalf("LoadedContextSize = %d, ожидалось окно копии 131072 (поллер не "+
			"должен подставлять сюда GGUF-предел)", caps.LoadedContextSize)
	}
}

// TestR493_FallbackSameGgufAcrossNodes — у двух узлов с одним файлом модели
// GGUF-предел одинаков, а окна загрузки разные. Это и есть «одна модель —
// один размер контекста» для клиента.
func TestR493_FallbackSameGgufAcrossNodes(t *testing.T) {
	master := capabilitiesOrFallback(nil, "Qwen3.8-27B-UD-Q4_K_M", "qwen35", 198656, 262144, true)
	a10 := capabilitiesOrFallback(nil, "Qwen3.8-27B-UD-Q4_K_M", "qwen35", 131072, 262144, true)
	if master.MaxContext != a10.MaxContext {
		t.Fatalf("MaxContext зависит от узла: %d vs %d", master.MaxContext, a10.MaxContext)
	}
	if master.LoadedContextSize == a10.LoadedContextSize {
		t.Fatalf("LoadedContextSize не различает узлы (оба %d) — потерян факт окна копии",
			master.LoadedContextSize)
	}
}

// TestR493_FallbackWithoutGgufReportsLoaded — GGUF неизвестен: честно отдаём
// загруженное окно, а не ноль.
func TestR493_FallbackWithoutGgufReportsLoaded(t *testing.T) {
	caps := capabilitiesOrFallback(nil, "some-model", "llama", 8192, 0, false)
	if caps.MaxContext != 8192 || caps.LoadedContextSize != 8192 {
		t.Fatalf("MaxContext=%d LoadedContextSize=%d, ожидались 8192/8192 при неизвестном GGUF",
			caps.MaxContext, caps.LoadedContextSize)
	}
}

// TestR493_FallbackPrefersCppWorkerCapabilities — если воркер сам прислал
// capabilities, fallback их не перетирает.
func TestR493_FallbackPrefersCppWorkerCapabilities(t *testing.T) {
	src := capabilitiesOrFallback(nil, "m", "qwen3", 4096, 32768, true)
	got := capabilitiesOrFallback(src, "m", "qwen3", 131072, 262144, false)
	if got != src {
		t.Fatal("capabilities из cppworker перетёрты fallback-вычислением")
	}
}

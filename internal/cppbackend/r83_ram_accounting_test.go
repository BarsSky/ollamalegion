// r83_ram_accounting_test.go — R83 (живая проверка 2026-09-25): учёт RAM.
//
// Два независимых дефекта, найденных на реальном GPU (RTX 3070 8 GB + реальный
// Qwen3.8-27B-UD-Q4_K_M.gguf), оба воспроизводят симптом «модель не загрузилась,
// а причины не видно»:
//
//  1. CalculateResourceLimits для НЕзагруженной модели не резолвил внешнее имя
//     («qwen3.8:latest» → файл «Qwen3.8-27B-UD-Q4_K_M»), потому что
//     GetModelMeta делает точный lookup по карте имён файлов. Лимиты оставались
//     нулевыми → evaluateNCtxFeasibility получал Known=false (stage=unknown) →
//     пред-загрузочный гейт n_ctx fail-open: 131072 принимался как 202 Accepted
//     без отказа, без предупреждения и без строки в логе.
//
//  2. Решение о RAM-fallback сравнивало потребность CPU-части модели с
//     УСТАНОВЛЕННОЙ памятью (MemTotal), а не со свободной. На стенде это
//     одобрило загрузку 27B с n_ctx=131072 (веса 15.7 GB + KV 8.6 GB) на машине
//     с 20.5 GB свободной RAM: 19556 < 24576*0.8 = 19660, запас 105 MB.
package cppbackend

import (
	"strings"
	"testing"

	"ollama-loadbalancer/c/bridge"
)

// r83InjectQwen38 — Backend с одним «файлом на диске» и без загруженных моделей.
func r83InjectQwen38(t *testing.T) *Backend {
	t.Helper()
	b := NewBackend(Config{ModelsDir: t.TempDir(), DefaultCtxSize: 32768, DefaultBatchSize: 512})
	mm := b.ModelManager()
	if mm == nil {
		t.Fatal("ModelManager недоступен — тест не воспроизводит сценарий")
	}
	mm.ggufFiles["Qwen3.8-27B-UD-Q4_K_M"] = &GGUFModelMeta{
		Filename:      "Qwen3.8-27B-UD-Q4_K_M",
		Path:          "/app/models/Qwen3.8-27B-UD-Q4_K_M.gguf",
		SizeBytes:     16464440224, // реальный размер файла на стенде
		NLayers:       64,
		NEmbd:         5120,
		NHeads:        24,
		NKvHeads:      4,
		ContextLength: 262144,
	}
	return b
}

// TestR83_ResourceLimits_TaggedNameResolvesGGUF — лимиты обязаны считаться для
// того файла, которым реально называется модель, включая внешнее имя с тегом.
func TestR83_ResourceLimits_TaggedNameResolvesGGUF(t *testing.T) {
	b := r83InjectQwen38(t)
	// RAM-бюджет фиксируем: иначе результат зависит от свободной памяти машины,
	// на которой гоняются тесты.
	t.Setenv("CPPWORKER_AVAILABLE_RAM_BYTES", "51539607552") // 48 GiB

	for _, name := range []string{
		"Qwen3.8-27B-UD-Q4_K_M",
		"Qwen3.8-27B-UD-Q4_K_M.gguf",
		"qwen3.8:latest", // так зовёт клиент (OpenWebUI / Ollama-провайдер)
	} {
		limits := b.CalculateResourceLimits(name)
		if limits.ModelMaxContext != 262144 {
			t.Errorf("CalculateResourceLimits(%q).ModelMaxContext = %d, want 262144 — GGUF-метаданные не найдены, значит гейт n_ctx работает fail-open",
				name, limits.ModelMaxContext)
		}
		if limits.MaxRAMNCtx <= 0 {
			t.Errorf("CalculateResourceLimits(%q).MaxRAMNCtx = %d, want > 0 — нулевые лимиты означают stage=unknown и отсутствие отказа/предупреждения",
				name, limits.MaxRAMNCtx)
		}
	}
}

// TestR83_OptimalGPULayers_UsesAvailableRAMNotTotal — гард RAM-fallback должен
// опираться на СВОБОДНУЮ память, а не на установленную.
func TestR83_OptimalGPULayers_UsesAvailableRAMNotTotal(t *testing.T) {
	newBackend := func() *Backend {
		return &Backend{
			gpuDevices: []bridge.GPUDevice{
				{Index: 0, VRAMTotalMB: 8 * 1024, VRAMFreeMB: 7 * 1024},
			},
		}
	}
	const modelSize = 16 * 1024 * 1024 * 1024 // 16 GiB весов
	const ctx = 131072

	// 1. Свободно 10 GB, а нужно ~40 GB (веса на CPU + KV) → обязан быть отказ
	//    с диагностикой, а не «загружено и непригодно».
	t.Setenv("CPPWORKER_AVAILABLE_RAM_BYTES", "10737418240") // 10 GiB
	_, _, diag := newBackend().CalculateOptimalGPULayers(modelSize, 64, 24, 4, 5120, 20, ctx, "f16")
	if diag == nil || diag.Recomendation == "" {
		t.Errorf("при 10 GiB свободной RAM загрузка 16 GiB модели с n_ctx=%d должна быть отклонена с диагностикой (получено diag=%+v)", ctx, diag)
	}

	// 2. Свободно 64 GB → тот же запрос обязан пройти.
	t.Setenv("CPPWORKER_AVAILABLE_RAM_BYTES", "68719476736") // 64 GiB
	_, _, diag2 := newBackend().CalculateOptimalGPULayers(modelSize, 64, 24, 4, 5120, 20, ctx, "f16")
	if diag2 != nil && diag2.Recomendation != "" {
		t.Errorf("при 64 GiB свободной RAM загрузка должна быть одобрена, получена диагностика: %s", diag2.Recomendation)
	}

	// 3. Отказ обязан называть СВОБОДНУЮ память, а не установленную.
	t.Setenv("CPPWORKER_AVAILABLE_RAM_BYTES", "10737418240")
	_, _, diag3 := newBackend().CalculateOptimalGPULayers(modelSize, 64, 24, 4, 5120, 20, ctx, "f16")
	if diag3 == nil || !strings.Contains(diag3.Recomendation, "RAM=10240 MB") {
		t.Errorf("диагностика должна содержать свободную RAM (RAM=10240 MB), получено: %v", diag3)
	}
}

// TestR83_MaxRAMNCtx_AccountsForModelWeights — потолок RAM-контекста
// (max_ram_n_ctx → HardMaxNCtx → 422) обязан учитывать веса модели, а не только
// KV-cache.
//
// Живые числа: Qwen3.8-27B-UD-Q4_K_M = 15 691 MB весов, свободно 20 480 MB.
// Реальный бюджет под KV = 20 480 − 15 691 = 4 789 MB, то есть ~22.7k токенов
// при f16 (221 520 B/токен). Прежняя формула (MemTotal − 4096)/kvPerToken давала
// 96 943 и пропускала 131 072 как «degraded», хотя запрос физически невыполним.
func TestR83_MaxRAMNCtx_AccountsForModelWeights(t *testing.T) {
	b := r83InjectQwen38(t)
	t.Setenv("CPPWORKER_AVAILABLE_RAM_BYTES", "21474836480") // 20 GiB — как на стенде

	limits := b.CalculateResourceLimits("qwen3.8:latest")
	if limits.MaxRAMNCtx <= 0 {
		t.Fatalf("MaxRAMNCtx = %d, want > 0 (RAM-потолок не посчитан)", limits.MaxRAMNCtx)
	}
	if limits.MaxRAMNCtx >= 131072 {
		t.Errorf("MaxRAMNCtx = %d — потолок не учитывает веса модели (15 691 MB) и пропустит n_ctx=131072, который физически не влезает",
			limits.MaxRAMNCtx)
	}
	t.Logf("MaxRAMNCtx=%d (available=20480MB, weights≈15701MB → под KV ≈4779MB)", limits.MaxRAMNCtx)

	// Потолок обязан масштабироваться по свободной RAM: при 48 GiB он заметно выше.
	t.Setenv("CPPWORKER_AVAILABLE_RAM_BYTES", "51539607552") // 48 GiB
	limitsBig := b.CalculateResourceLimits("qwen3.8:latest")
	if limitsBig.MaxRAMNCtx <= limits.MaxRAMNCtx {
		t.Errorf("при большей свободной RAM потолок должен быть выше: 48GiB=%d, 20GiB=%d",
			limitsBig.MaxRAMNCtx, limits.MaxRAMNCtx)
	}

	// Модель, которая влезает в свободную VRAM, не должна терять бюджет на веса:
	// её RAM-потолок обязан быть ВЫШЕ, чем у 27B, который в VRAM не влезает.
	t.Setenv("CPPWORKER_AVAILABLE_RAM_BYTES", "21474836480") // 20 GiB
	mm := b.ModelManager()
	mm.ggufFiles["gemma-4-E4B-it-Q4_K_M"] = &GGUFModelMeta{
		Filename: "gemma-4-E4B-it-Q4_K_M", Path: "/app/models/gemma-4-E4B-it-Q4_K_M.gguf",
		SizeBytes: 4215695776, NLayers: 42, NEmbd: 2560, NHeads: 8, NKvHeads: 2, ContextLength: 131072,
	}
	limitsSmall := b.CalculateResourceLimits("gemma-4-E4B-it-Q4_K_M")
	if limitsSmall.MaxRAMNCtx <= limits.MaxRAMNCtx {
		t.Errorf("для модели 4.2 GB (влезает в VRAM) потолок занижен: gemma=%d, 27B=%d",
			limitsSmall.MaxRAMNCtx, limits.MaxRAMNCtx)
	}
}

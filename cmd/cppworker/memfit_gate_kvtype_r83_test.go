//go:build llama_stub

// memfit_gate_kvtype_r83_test.go — R83 §3.4 (2026-09-26).
//
// Симптом, который здесь ловится: гейт n_ctx считал KV-cache по дефолту
// конфига (EffectiveKVCacheType("")), а раскладка слоёв — по типу, который
// реально применит загрузка (явный параметр → профиль модели → дефолт). Для
// Qwen3.8 профиль задаёт q8_0, дефолт — q4_0: один и тот же запрос получал
// два разных потолка VRAM (расхождение 1.88×).
//
// Тест — на чистой функции feasibilityFromMemfit: без backend, без GPU, без
// HTTP. Числа взяты с живого замера (Qwen3.8-27B-UD-Q4_K_M на RTX 3070 8 GB):
// llama_kv_cache аллоцировал 576.00 MiB на 32768 токенов при q4_0, то есть
// 18 432 Б/токен = 2 × 16 слоёв × 4 KV-головы × 256 head_dim × 18/32.
package main

import (
	"testing"

	"ollama-loadbalancer/internal/cppbackend"
	"ollama-loadbalancer/internal/memfit"
)

// qwen38SpecR83 — метаданные живого файла Qwen3.8-27B-UD-Q4_K_M.gguf
// (см. internal/cppbackend/kv_layers_test.go: 16 KV-слоёв, head_dim 256).
func qwen38SpecR83() memfit.ModelSpec {
	return cppbackend.MemfitSpecFromValues(
		"qwen3.8:latest", 16464440224, 64, 24, 4, 5120, 262144, 16, 256)
}

// rtx3070BudgetTightRAMR83 — бюджет стенда: 8192 MiB VRAM (2048 MiB резерв,
// как в MemfitPolicy/VRAMReserve) и подрезанная RAM так, чтобы потолок
// упирался в память, а не в обучающий контекст модели.
func rtx3070BudgetTightRAMR83() memfit.Budget {
	return memfit.Budget{
		VRAMTotal:   memfit.MiBOf(8192),
		VRAMFree:    memfit.MiBOf(8192),
		VRAMKnown:   true,
		VRAMReserve: memfit.MiBOf(2048),
		RAMTotal:    memfit.GiBOf(28),
		RAMAvail:    memfit.GiBOf(20),
		RAMKnown:    true,
		RAMReserve:  memfit.MiBOf(4096),
	}
}

// TestR83_Gate_KVTypeChangesCeiling — тип KV-cache обязан менять потолок n_ctx.
//
// Именно этого не делал прежний гейт: он всегда считал q4_0 (дефолт конфига),
// даже когда загрузка шла с q8_0 из профиля модели.
//
// На 8 GB карте 16.4 GB модель ЦЕЛИКОМ в VRAM не влезает никогда, поэтому
// exact-fit потолок равен 0 у обоих типов: различие обязано быть видно на
// hard-потолке (RAM-граница). На просторной RAM он упирается в обучающий
// контекст и маскирует разницу — поэтому здесь RAM подрезана до ~20 GiB.
func TestR83_Gate_KVTypeChangesCeiling(t *testing.T) {
	spec := qwen38SpecR83()
	budget := rtx3070BudgetTightRAMR83()
	const requested = 131072

	fQ4, vQ4, okQ4 := feasibilityFromMemfit(spec, budget, "q4_0", requested)
	fQ8, vQ8, okQ8 := feasibilityFromMemfit(spec, budget, "q8_0", requested)
	if !okQ4 || !okQ8 {
		t.Fatalf("вердикт не построен: okQ4=%v okQ8=%v", okQ4, okQ8)
	}

	// Ожидаемые per-token размеры (те же формулы, что в memfit/kv.go):
	//   q4_0: 2 * 16 * 4 * 256 * 18/32 = 18 432 Б/токен
	//   q8_0: 2 * 16 * 4 * 256 * 34/32 = 34 816 Б/токен
	if vQ4.KVPerToken != 18432 {
		t.Errorf("q4_0 kv_per_token = %d, want 18432", vQ4.KVPerToken)
	}
	if vQ8.KVPerToken != 34816 {
		t.Errorf("q8_0 kv_per_token = %d, want 34816", vQ8.KVPerToken)
	}

	if fQ4.MaxVRAMNCtx != 0 {
		t.Errorf("q4_0: exact-fit потолок = %d, want 0 — 16.4 GB веса не влезают в 6 GiB VRAM",
			fQ4.MaxVRAMNCtx)
	}
	if fQ4.HardMaxNCtx <= 0 || fQ8.HardMaxNCtx <= 0 {
		t.Fatalf("hard-потолок = 0 (q4_0=%d q8_0=%d) — бюджет теста не воспроизводит стенд",
			fQ4.HardMaxNCtx, fQ8.HardMaxNCtx)
	}
	if fQ4.HardMaxNCtx == fQ8.HardMaxNCtx {
		t.Errorf("тип KV не влияет на потолок: q4_0=%d q8_0=%d — гейт считает KV "+
			"не тем типом, с которым пойдёт загрузка (R83 §3.4)",
			fQ4.HardMaxNCtx, fQ8.HardMaxNCtx)
	}
	if fQ8.HardMaxNCtx >= fQ4.HardMaxNCtx {
		t.Errorf("q8_0 (34 Б/элем) даёт потолок не меньше q4_0 (18 Б/элем): q8_0=%d q4_0=%d",
			fQ8.HardMaxNCtx, fQ4.HardMaxNCtx)
	}
	// Отношение потолков НЕ равно в точности отношению байт-на-элемент (34/18):
	// hard-потолок живёт на границе «CPU-часть влезает в RAM», а CPU-часть —
	// это ещё и (1−k) весов, где k = доля слоёв на GPU. Поэтому проверяем
	// направление и разумный коридор, а не точное число.
	ratio := float64(fQ4.HardMaxNCtx) / float64(fQ8.HardMaxNCtx)
	if ratio < 1.3 || ratio > 2.1 {
		t.Errorf("отношение потолков q4_0/q8_0 = %.3f, want в коридоре 1.3–2.1 "+
			"(KV различается в 34/18≈1.888)", ratio)
	}
}

// TestR83_Gate_KVTypeFlipsVerdict — расхождение типа обязано быть видно и на
// уровне вердикта, а не только в числе: на той же подрезанной RAM запрос между
// потолками q8_0 и q4_0 проходит с q4_0 и отклоняется с q8_0. Старый гейт
// (всегда q4_0) такой запрос пропускал бы.
func TestR83_Gate_KVTypeFlipsVerdict(t *testing.T) {
	spec := qwen38SpecR83()
	budget := rtx3070BudgetTightRAMR83()

	_, vQ4, _ := feasibilityFromMemfit(spec, budget, "q4_0", 1)
	_, vQ8, _ := feasibilityFromMemfit(spec, budget, "q8_0", 1)
	if vQ4.MaxHardCtx <= vQ8.MaxHardCtx {
		t.Fatalf("предусловие: потолок q4_0 (%d) должен быть выше q8_0 (%d)",
			vQ4.MaxHardCtx, vQ8.MaxHardCtx)
	}
	ctx := vQ8.MaxHardCtx + 1 // выше потолка q8_0, но ниже потолка q4_0

	fQ4, _, _ := feasibilityFromMemfit(spec, budget, "q4_0", ctx)
	fQ8, _, _ := feasibilityFromMemfit(spec, budget, "q8_0", ctx)

	if fQ4.Stage == nctxStageInfeasible {
		t.Errorf("q4_0: stage = %q при n_ctx=%d (потолок %d) — должен проходить",
			fQ4.Stage, ctx, vQ4.MaxHardCtx)
	}
	if fQ8.Stage != nctxStageInfeasible {
		t.Errorf("q8_0: stage = %q при n_ctx=%d выше потолка %d — должен быть отказ "+
			"(иначе гейт пропускает невыполнимый для q8_0 запрос)",
			fQ8.Stage, ctx, vQ8.MaxHardCtx)
	}
}

// TestR83_EffectiveKVCacheTypeForLoad — порядок разрешения типа KV тот же, что
// в хендлерах: явный параметр → профиль модели → дефолт конфига. Без этого
// гейт разошёлся бы с раскладкой ровно там, где профиль задаёт свой тип.
func TestR83_EffectiveKVCacheTypeForLoad(t *testing.T) {
	oldBackend := backend
	defer func() { backend = oldBackend }()
	// Дефолт конфига — q4_0 (как в config/cppworker-defaults.json живого стенда).
	backend = cppbackend.NewBackend(cppbackend.Config{
		ModelsDir:          t.TempDir(),
		DefaultKVCacheType: "q4_0",
	})

	// Без backend-дефолта: явный параметр запроса выигрывает у всего.
	if got := effectiveKVCacheTypeForLoad("qwen3.8:latest", "f16"); got != "f16" {
		t.Errorf("явный параметр: got %q, want f16", got)
	}
	// Пустой параметр → дефолт конфига (профиля в этом тесте нет).
	if got := effectiveKVCacheTypeForLoad("qwen3.8:latest", ""); got != "q4_0" {
		t.Errorf("дефолт конфига: got %q, want q4_0", got)
	}
	// Невалидный параметр не превращается в «нет KV» и не перебивает дефолт.
	if got := effectiveKVCacheTypeForLoad("qwen3.8:latest", "bogus"); got != "q4_0" {
		t.Errorf("невалидный параметр: got %q, want q4_0", got)
	}
	// Без backend вообще — безопасный верх f16 (наибольший KV).
	backend = nil
	if got := effectiveKVCacheTypeForLoad("qwen3.8:latest", ""); got != "f16" {
		t.Errorf("без backend: got %q, want f16", got)
	}
}

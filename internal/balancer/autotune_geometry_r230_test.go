// autotune_geometry_r230_test.go — Round 230 (2026-10-24): регрессия на
// корректный бюджет памяти AutoTune и физическую исполнимость рекомендаций.
//
// Контекст: на стенде (A10 24GB, n_ctx=131072, KV q8_0, 16 слоёв на GPU)
// анализ писал «свободно 0 GB» и советовал num_gpu_layers=-2 + q4_0, потому что
// метрики в МБ передавались как байты, а free VRAM измерялся при загруженной
// модели (reload её выгружает).
package balancer

import (
	"fmt"
	"math"
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

const r230GiB = uint64(1024 * 1024 * 1024)

func r230Type(name string, sizeBytes uint64, nLayers, nKvHeads, headDimK int) *ModelProfileInfo {
	return &ModelProfileInfo{
		Name:                 name,
		SizeBytes:            sizeBytes,
		NLayers:              nLayers,
		NKvHeads:             nKvHeads,
		HeadDimK:             headDimK,
		CurrentKVCacheType:   "q8_0",
		CurrentNumGPULayers:  16,
		CurrentContextLength: 0,
	}
}

// TestR230_MBToBytes — метрики (МБ) и API-конвертация.
func TestR230_MBToBytes(t *testing.T) {
	cases := []struct {
		mb   uint64
		want uint64
	}{
		{0, 0},
		{1, 1024 * 1024},
		{24576, 24576 * 1024 * 1024}, // A10 24GB в МБ
		{^uint64(0), ^uint64(0)},     // насыщение вместо overflow
	}
	for _, c := range cases {
		if got := MBToBytes(c.mb); got != c.want {
			t.Errorf("MBToBytes(%d)=%d, want %d", c.mb, got, c.want)
		}
	}
}

// TestR230_ReloadBudgetBytes — бюджет reload = free + usage модели, ≤ total.
func TestR230_ReloadBudgetBytes(t *testing.T) {
	total := 24 * r230GiB
	t.Run("free+usage", func(t *testing.T) {
		// 2GB свободно при загруженной модели, модель занимает 18000MB → 19.6GB бюджет.
		got := reloadBudgetBytes(2*r230GiB, total, 18000)
		want := 2*r230GiB + 18000*1024*1024
		if got != want {
			t.Fatalf("got %d, want %d", got, want)
		}
	})
	t.Run("clamp to total", func(t *testing.T) {
		if got := reloadBudgetBytes(20*r230GiB, total, 18000); got != total {
			t.Fatalf("got %d, want %d (clamped)", got, total)
		}
	})
	t.Run("no usage", func(t *testing.T) {
		if got := reloadBudgetBytes(3*r230GiB, total, 0); got != 3*r230GiB {
			t.Fatalf("got %d, want %d", got, 3*r230GiB)
		}
	})
}

// TestR230_KVBytes — размер KV cache для загруженного n_ctx и типа.
func TestR230_KVBytes(t *testing.T) {
	p := r230Type("m", 5*r230GiB, 48, 8, 128)
	p.CurrentContextLength = 131072
	// 2*2(K+V) * 48 * 8 * 128 = 196608 байт/токен для f16
	f16 := kvCacheBytesForContext(p, "f16", p.CurrentContextLength)
	q8 := kvCacheBytesForContext(p, "q8_0", p.CurrentContextLength)
	q4 := kvCacheBytesForContext(p, "q4_0", p.CurrentContextLength)
	if f16 != 196608*131072 {
		t.Fatalf("f16 KV=%d, want %d", f16, uint64(196608*131072))
	}
	if q8 != f16/2 || q4 != f16/4 {
		t.Fatalf("q8=%d (want f16/2=%d), q4=%d (want f16/4=%d)", q8, f16/2, q4, f16/4)
	}
	if kvCacheBytesForContext(p, "q8_0", 0) != 0 {
		t.Fatal("без n_ctx KV не резервируется")
	}
}

// TestR230_A10_131072_Q8_16Layers — сценарий пользователя: A10 24GB,
// n_ctx=131072, KV q8_0, 16 слоёв. Раньше: «свободно 0 GB» + -2 + q4_0.
// С корректным бюджетом рекомендации обязаны быть физически исполнимыми.
func TestR230_A10_131072_Q8_16Layers(t *testing.T) {
	// 14B-class: 48 слоёв, 8 KV heads, head_dim=128, веса ~12GB.
	// q8_0 KV при 131072 ≈ 12GiB: 12 + 12 + резерв > 24GiB — не влезает.
	p := r230Type("qwen3-14b-q5", 12*r230GiB, 48, 8, 128)
	p.CurrentContextLength = 131072
	p.CurrentKVCacheType = "q8_0"
	p.CurrentNumGPULayers = 16
	// Свежий reload-бюджет после выгрузки модели.
	p.FreeVRAMBytes = 24 * r230GiB
	p.TotalVRAMBytes = 24 * r230GiB
	p.FreeRAMBytes = 120 * r230GiB

	analysis := analyzeLoadedModel(p)
	if analysis == nil {
		t.Fatal("nil analysis")
	}
	reserve := autotuneReserveBytes(p.TotalVRAMBytes)
	// q8_0 KV (12GiB) + 9GiB весов + 1.2GiB резерва не влезают в 24GiB,
	// q4_0 (6GiB) тоже не влезают целиком → должна появиться KV-рекомендация
	// q4_0 (минимум в 4 раза меньше f16), но НЕ -2 в слоях.
	var kvRec, layersRec *AutoTuneRecommendation
	for i := range analysis.Recommendations {
		r := &analysis.Recommendations[i]
		switch r.Category {
		case "kv_cache":
			kvRec = r
		case "layers":
			layersRec = r
		}
	}
	if kvRec == nil {
		t.Fatalf("ожидалась KV-рекомендация, получено %+v", analysis.Recommendations)
	}
	if got := normalizeKVType(kvRec.RecommendedKVCache); got != "q4_0" {
		t.Errorf("KV rec=%q, want q4_0 (q8_0 KV 12GiB не влезает)", got)
	}
	if strings.Contains(kvRec.Suggestion, "0.0GB") {
		t.Errorf("в reason фигурирует 0.0GB — бюджет посчитан неверно: %s", kvRec.Suggestion)
	}
	if layersRec != nil {
		if layersRec.RecommendedNumGPULayers == -2 {
			t.Errorf("R230: рекомендация -2 вернулась, ожидалось число или отсутствие рекомендации: %s", layersRec.Suggestion)
		}
		if layersRec.RecommendedNumGPULayers > 0 {
			// веса N слоёв + резерв обязаны влезать в бюджет (KV может быть в RAM)
			perLayer := float64(p.SizeBytes) / float64(p.NLayers)
			need := perLayer*float64(layersRec.RecommendedNumGPULayers) + float64(reserve)
			if need > float64(p.FreeVRAMBytes) {
				t.Errorf("%d слоёв: %.2fGiB весов не влезают в бюджет %.2fGiB",
					layersRec.RecommendedNumGPULayers, need/float64(r230GiB), float64(p.FreeVRAMBytes)/float64(r230GiB))
			}
			if layersRec.RecommendedNumGPULayers < p.CurrentNumGPULayers {
				t.Errorf("R230: рекомендация уменьшает слои (%d < %d) — запрещено", layersRec.RecommendedNumGPULayers, p.CurrentNumGPULayers)
			}
		}
	}
	// При полном бюджете 24GiB: 12GiB весов + q4_0 KV 6GiB + 1.2GiB резерва
	// = 19.2GiB ≤ 24GiB → все слои должны поместиться.
	if analysis.OptimalNumGPULayers != -1 {
		t.Errorf("ожидалось -1 (all layers), получено %d (%s)", analysis.OptimalNumGPULayers, layersRecString(layersRec))
	}
}

// TestR230_A10_Q8_Fits — если q8_0 KV влезает в бюджет, KV не меняется,
// а слои поднимаются до -1 (это и есть «начальная загрузка была не хуже»).
func TestR230_A10_Q8_Fits(t *testing.T) {
	// 8B-class: 32 слоя, 8 KV heads, 128 → f16 KV при 131072 ≈ 16GiB (не влезает
	// вместе с 8GiB весов), q8_0 ≈ 8GiB — влезает.
	p := r230Type("qwen3-8b-q4", 8*r230GiB, 32, 8, 128)
	p.CurrentContextLength = 131072
	p.FreeVRAMBytes = 24 * r230GiB
	p.TotalVRAMBytes = 24 * r230GiB

	analysis := analyzeLoadedModel(p)
	if analysis.OptimalKVCacheType != "" {
		t.Errorf("q8_0 KV влезает в 24GiB, KV не должен меняться (rec=%q)", analysis.OptimalKVCacheType)
	}
	if analysis.OptimalNumGPULayers != -1 {
		t.Errorf("все слои влезают → ожидалось -1, получено %d", analysis.OptimalNumGPULayers)
	}
}

// TestR230_NoFreeVRAMData_NoRecommendation — неполные метрики не должны
// превращаться в рекомендации q4_0/-2.
func TestR230_NoFreeVRAMData_NoRecommendation(t *testing.T) {
	p := r230Type("m", 9*r230GiB, 48, 8, 128)
	p.CurrentContextLength = 131072
	p.CurrentNumGPULayers = 16
	p.FreeVRAMBytes = 0
	p.TotalVRAMBytes = 24 * r230GiB
	if rec, reason := computeOptimalKVCacheType(p); rec != p.CurrentKVCacheType {
		t.Errorf("без данных о VRAM KV не меняем, получено %q (%s)", rec, reason)
	}
	if rec, _ := computeOptimalNumGPULayers(p); rec != p.CurrentNumGPULayers {
		t.Errorf("без данных о VRAM слои не меняем, получено %d", rec)
	}
}

// TestR230_ConfigMatrix — таблица конфигураций 8/16/24/32GiB VRAM ×
// 16/32/64/120GiB RAM × 3 модели × 3 контекста. Инварианты:
//   - рекомендованный KV физически влезает в бюджет, если такой существует;
//   - рекомендованные слои не уменьшают текущие;
//   - все слои (-1) рекомендуются только когда веса + KV + резерв ≤ бюджет;
//   - положительное N: веса N слоёв + резерв ≤ бюджет;
//   - в reason нет «0.0GB» при бюджете > 1GiB.
func TestR230_ConfigMatrix(t *testing.T) {
	type model struct {
		name    string
		size    uint64
		nLayers int
		nKv     int
		headDim int
	}
	models := []model{
		{"4b-q4", 2_500_000_000, 36, 8, 80},
		{"8b-q4", 5 * r230GiB, 32, 8, 128},
		{"30b-q4", 18 * r230GiB, 48, 8, 128},
	}
	vrams := []uint64{8, 16, 24, 32}
	rams := []uint64{16, 32, 64, 120}
	ctxs := []int{4096, 32768, 131072}

	cases := 0
	for _, m := range models {
		for _, vramGB := range vrams {
			for _, ramGB := range rams {
				for _, ctx := range ctxs {
					cases++
					name := fmt.Sprintf("%s/vram%d/ram%d/ctx%d", m.name, vramGB, ramGB, ctx)
					t.Run(name, func(t *testing.T) {
						total := vramGB * r230GiB
						p := r230Type(m.name, m.size, m.nLayers, m.nKv, m.headDim)
						p.CurrentContextLength = ctx
						p.FreeVRAMBytes = total                          // reload-бюджет: модель будет выгружена
						p.TotalVRAMBytes = total
						p.FreeRAMBytes = ramGB * r230GiB
						p.CurrentNumGPULayers = 16

						reserve := autotuneReserveBytes(total)
						analysis := analyzeLoadedModel(p)
						if analysis == nil {
							t.Fatal("nil analysis")
						}

						effKV := normalizeKVType(p.CurrentKVCacheType)
						if analysis.OptimalKVCacheType != "" {
							effKV = normalizeKVType(analysis.OptimalKVCacheType)
						}
						kvBytes := kvCacheBytesForContext(p, effKV, ctx)

						// 1) Если рекомендуются все слои — всё обязано влезать.
						if analysis.OptimalNumGPULayers == -1 {
							need := p.SizeBytes + kvBytes + reserve
							if need > total {
								t.Fatalf("-1 рекомендовано, но веса+KV_%s+резерв = %.2fGiB > %.2fGiB",
									effKV, float64(need)/float64(r230GiB), float64(total)/float64(r230GiB))
							}
						}
						// 2) Никаких уменьшений слоёв.
						for _, r := range analysis.Recommendations {
							if r.Category == "layers" && r.RecommendedNumGPULayers > 0 &&
								r.RecommendedNumGPULayers < p.CurrentNumGPULayers {
								t.Fatalf("рекомендация уменьшает слои: %d < %d (%s)",
									r.RecommendedNumGPULayers, p.CurrentNumGPULayers, r.Suggestion)
							}
							if r.Category == "layers" && r.RecommendedNumGPULayers == -2 {
								t.Fatalf("R230: -2 не должна рекомендоваться: %s", r.Suggestion)
							}
							if total > r230GiB && strings.Contains(r.Suggestion, "бюджет 0.0GB") {
								t.Fatalf("0.0GB бюджета при total %.0fGiB: %s",
									float64(total)/float64(r230GiB), r.Suggestion)
							}
						}
						// 3) Рекомендованный KV должен влезать, если хоть один влезает.
						if analysis.OptimalKVCacheType != "" {
							availForKV := int64(total) - int64(reserve) - int64(p.SizeBytes)
							fitsAny := false
							recFits := false
							for _, kt := range []string{"f16", "q8_0", "q4_0"} {
								need := int64(kvCacheBytesForContext(p, kt, ctx))
								if availForKV >= 0 && need > 0 && need <= availForKV {
									fitsAny = true
								}
								if kt == effKV && availForKV >= 0 && need > 0 && need <= availForKV {
									recFits = true
								}
							}
							if fitsAny && !recFits {
								t.Fatalf("рекомендован KV %s (%.1fGiB), хотя в бюджет %.1fGiB влезает другой тип",
									effKV, bytesToGB(kvBytes), bytesToGB(total))
							}
						}
						// 4) Положительное N: веса N слоёв + резерв ≤ бюджет.
						if analysis.OptimalNumGPULayers > 0 && analysis.OptimalNumGPULayers < p.NLayers {
							perLayer := float64(p.SizeBytes) / float64(p.NLayers)
							need := perLayer*float64(analysis.OptimalNumGPULayers) + float64(reserve)
							if need > float64(total)+1 {
								t.Fatalf("%d слоёв: %.2fGiB весов+резерв > %.2fGiB",
									analysis.OptimalNumGPULayers, need/float64(r230GiB), float64(total)/float64(r230GiB))
							}
						}
						// 5) Санити: числа не NaN/Inf и не отрицательный бюджет.
						if math.IsNaN(float64(analysis.OptimalNumGPULayers)) || analysis.OptimalNumGPULayers < -2 {
							t.Fatalf("некорректный OptimalNumGPULayers=%d", analysis.OptimalNumGPULayers)
						}
					})
				}
			}
		}
	}
	t.Logf("R230 matrix: %d конфигураций", cases)
}

// TestR230_AnalyzeBackend_AddsModelUsageToBudget — AnalyzeBackend получает
// free VRAM при загруженной модели; рекомендации должны считаться от
// free + VRAMUsage этой модели (иначе «свободно 2GB» вместо 20GB).
func TestR230_AnalyzeBackend_AddsModelUsageToBudget(t *testing.T) {
	total := 24 * r230GiB
	free := 2 * r230GiB // модель уже заняла ~22GiB
	model := types.LlamaCppModel{
		Name:            "qwen3-14b-q4",
		State:           "loaded",
		Size:            9 * r230GiB,
		NLayers:         48,
		NKvHeads:        8,
		HeadDimK:        128,
		ContextLength:   131072,
		KvCacheType:     "q8_0",
		NumGPULayers:    16,
		VRAMUsage:       20000, // MB — как отдаёт cppworker
		RAMUsage:        4000,  // MB
	}

	report := AnalyzeBackend(nil, "b1", "llama_cpp", []types.LlamaCppModel{model}, free, 16*r230GiB, total)
	if len(report.Models) != 1 {
		t.Fatalf("ожидался 1 анализ, получено %d", len(report.Models))
	}
	a := report.Models[0]
	// С учётом usage бюджет ≈ 21.5GiB: 9GiB весов + q4_0 KV (6GiB) + резерв (1.2GiB)
	// ≤ 21.5GiB → все слои влезают, что без учёта usage было бы невозможно.
	if a.OptimalNumGPULayers != -1 {
		t.Fatalf("с reload-бюджетом ожидалось -1, получено %d (recs: %+v)", a.OptimalNumGPULayers, a.Recommendations)
	}

	// Контроль: без usage (free=2GiB) все слои физически не влезают → -1 не советуем.
	model.VRAMUsage = 0
	control := AnalyzeBackend(nil, "b1", "llama_cpp", []types.LlamaCppModel{model}, free, 16*r230GiB, total)
	if len(control.Models) != 1 {
		t.Fatalf("ожидался 1 анализ (control), получено %d", len(control.Models))
	}
	if control.Models[0].OptimalNumGPULayers == -1 {
		t.Fatalf("без учёта usage -1 физически невозможен (free=2GiB, recs: %+v)",
			control.Models[0].Recommendations)
	}
}

// TestR230_MissingArchData_NoLayerChange — live-случай: /api/models не отдаёт
// n_kv_heads/head_dim_k (gemma-4-E4B: nLayers=42, nEmbd=2560, остальное пусто).
// Без arch KV не посчитать → раскладку не трогаем (раньше рекомендовалось -2).
func TestR230_MissingArchData_NoLayerChange(t *testing.T) {
	p := &ModelProfileInfo{
		Name:                 "gemma-4-E4B-it-Q4_K_M",
		SizeBytes:            4215695776,
		NLayers:              42,
		NEmbd:                2560,
		CurrentContextLength: 12288,
		CurrentKVCacheType:   "q4_0",
		CurrentNumGPULayers:  40,
		FreeVRAMBytes:        8 * r230GiB,
		TotalVRAMBytes:       8 * r230GiB,
	}
	analysis := analyzeLoadedModel(p)
	if analysis == nil {
		t.Fatal("nil analysis")
	}
	if len(analysis.Recommendations) != 0 {
		t.Fatalf("без arch-данных рекомендаций быть не должно, получено %+v", analysis.Recommendations)
	}
	if rec, reason := computeOptimalNumGPULayers(p); rec != p.CurrentNumGPULayers {
		t.Fatalf("слои не меняем без KV-данных, получено %d (%s)", rec, reason)
	}
}

// TestR230_PlanAppliesConcreteLayers — план AutoTune применяет конкретное число
// слоёв (не -2), поэтому рекомендация исполнима.
func TestR230_PlanAppliesConcreteLayers(t *testing.T) {
	a := &AutoTuneAnalysis{
		IsSubOptimal: true,
		Recommendations: []AutoTuneRecommendation{
			{Category: "kv_cache", CurrentKVCache: "q8_0", RecommendedKVCache: "q4_0"},
			{Category: "layers", CurrentNumGPULayers: 16, RecommendedNumGPULayers: 40},
		},
	}
	cur := types.LlamaCppModel{Name: "m", NumGPULayers: 16, KvCacheType: "q8_0", ContextLength: 131072}
	plan := PlanApplyAutoTune(a, cur)
	if plan == nil {
		t.Fatal("ожидался план (KV q4_0 + 40 слоёв)")
	}
	if plan.NumGPULayers != 40 {
		t.Errorf("NumGPULayers=%d, want 40", plan.NumGPULayers)
	}
	if plan.KVCacheType != "q4_0" {
		t.Errorf("KVCacheType=%q, want q4_0", plan.KVCacheType)
	}
}

// TestR230_A10ScenarioTable — наглядная таблица для сценария пользователя:
// A10 24GiB, n_ctx=131072, KV q8_0, 16 слоёв на GPU. Для каждого размера модели
// печатается рекомендация AutoTune и проверяется её физическая исполнимость.
// Запуск с -v чтобы увидеть таблицу.
func TestR230_A10ScenarioTable(t *testing.T) {
	const vram = 24 * r230GiB
	const ctx = 131072
	reserve := autotuneReserveBytes(vram)

	models := []struct {
		name    string
		sizeGiB uint64
		nLayers int
		nKv     int
		headDim int
	}{
		{"4B-q4", 3, 36, 8, 80},
		{"8B-q4", 5, 32, 8, 128},
		{"14B-q4", 9, 48, 8, 128},
		{"14B-q5", 12, 48, 8, 128},
		{"32B-q4", 19, 64, 8, 128},
	}
	t.Logf("%-8s %-10s %-12s %-14s %-10s", "model", "weights", "KV_текущий", "KV_рекоменд.", "layers")
	for _, m := range models {
		p := r230Type(m.name, m.sizeGiB*r230GiB, m.nLayers, m.nKv, m.headDim)
		p.CurrentContextLength = ctx
		p.CurrentKVCacheType = "q8_0"
		p.CurrentNumGPULayers = 16
		p.FreeVRAMBytes = vram
		p.TotalVRAMBytes = vram
		p.FreeRAMBytes = 120 * r230GiB

		a := analyzeLoadedModel(p)
		kvRec := "оставить q8_0"
		if a.OptimalKVCacheType != "" {
			kvRec = a.OptimalKVCacheType
		}
		layers := "оставить 16"
		if a.OptimalNumGPULayers != 0 && a.OptimalNumGPULayers != 16 {
			layers = fmt.Sprintf("%d", a.OptimalNumGPULayers)
			if a.OptimalNumGPULayers == -1 {
				layers = "-1 (все)"
			}
		}
		t.Logf("%-8s %4dGiB   %-12s %-14s %-10s", m.name, m.sizeGiB, "q8_0", kvRec, layers)

		effKV := "q8_0"
		if a.OptimalKVCacheType != "" {
			effKV = a.OptimalKVCacheType
		}
		effLayers := 16
		if a.OptimalNumGPULayers != 0 {
			effLayers = a.OptimalNumGPULayers
		}
		kvBytes := kvCacheBytesForContext(p, effKV, ctx)
		perLayer := float64(p.SizeBytes) / float64(p.NLayers)
		switch {
		case effLayers == -1:
			if need := p.SizeBytes + kvBytes + reserve; need > vram {
				t.Errorf("%s: -1 (все слои) не влезает: %.2fGiB > %.2fGiB",
					m.name, float64(need)/float64(r230GiB), float64(vram)/float64(r230GiB))
			}
		case effLayers > 0:
			if need := perLayer*float64(effLayers) + float64(reserve); need > float64(vram) {
				t.Errorf("%s: %d слоёв не влезают: %.2fGiB весов > %.2fGiB",
					m.name, effLayers, need/float64(r230GiB), float64(vram)/float64(r230GiB))
			}
		}
	}
}

func layersRecString(r *AutoTuneRecommendation) string {
	if r == nil {
		return "нет слоёв-рекомендации"
	}
	return r.Suggestion
}

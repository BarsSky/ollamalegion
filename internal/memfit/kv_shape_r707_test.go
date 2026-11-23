// kv_shape_r707_test.go — R707 (2026-11-25): единая формула KV — по классам слоёв.
//
// ПРЕДМЕТ. До R707 у memfit было одно число «KV на токен» (все KV-слои × одна
// ширина) и отдельная ветка для оконных слоёв внутри Ceilings. cppbackend считал
// KV своей формулой, и для моделей со SWA эти два расчёта расходились: у gemma-4
// оконные слои шириной 256, а не 512. Теперь формула одна — KVShape, — и её
// используют и memfit (решения), и cppbackend (feasible/API).
//
// НЕ-ВАКУУМНОСТЬ. Ожидания посчитаны вручную; рядом проверяется, что классовая
// формула ОТЛИЧАЕТСЯ от «все слои одной шириной» и что MaxCtxForBudget — именно
// обратная к BytesAtCtx функция (на единицу больше — уже не влезает).
package memfit

import "testing"

// gemma4ShapeR707 — реальная раскладка gemma-4-E4B из GGUF:
// 4 глобальных слоя (K=V=512), 20 оконных (K=V=256, окно 512), 2 KV-головы.
func gemma4ShapeR707() KVShape {
	return KVShape{
		Global:    KVLayerClass{Layers: 4, HeadDimK: 512, HeadDimV: 512, KVHeads: 2},
		SWA:       KVLayerClass{Layers: 20, HeadDimK: 256, HeadDimV: 256, KVHeads: 2},
		SWAWindow: 512,
	}
}

// TestKVShapeSWAClassWidths — у классов свои ширины: 4·2·(512+512)·2 + 20·2·(256+256)·2.
func TestKVShapeSWAClassWidths(t *testing.T) {
	shape := gemma4ShapeR707()
	const wantF16 = Bytes(4*2*(512+512)*2 + 20*2*(256+256)*2) // 57344
	if got := shape.BytesPerToken(KVF16); got != wantF16 {
		t.Fatalf("BytesPerToken(f16) = %d, want %d", got, wantF16)
	}
	// НЕ-ВАКУУМНОСТЬ: если считать оконные слои шириной глобальных, вклад оконных
	// слоёв удвоится (40 960 → 81 920 Б/токен), а с ним и весь KV.
	sameWidth := shape
	sameWidth.SWA.HeadDimK, sameWidth.SWA.HeadDimV = 512, 512
	const swaPartF16 = Bytes(20 * 2 * (256 + 256) * 2) // 40960
	if got := sameWidth.BytesPerToken(KVF16); got != wantF16+swaPartF16 {
		t.Errorf("формула без классов дала %d, want %d — ширины классов не различаются",
			got, wantF16+swaPartF16)
	}
	// q4_0 — точный коэффициент 18/32 (memfit.KVBytesPerElement). На ctx=1024
	// (больше окна 512): глобальные 4608 Б/токен × 1024 + оконные 11 520 Б/токен × 512.
	const wantQ4At1024 = Bytes((4608 * 1024) + (11520 * 512))
	if got := shape.BytesAtCtx(KVQ4, 1024); got != wantQ4At1024 {
		t.Errorf("BytesAtCtx(q4_0, 1024) = %d, want %d", got, wantQ4At1024)
	}
}

// TestKVShapeSWAStopsGrowing — после окна растут только глобальные слои.
func TestKVShapeSWAStopsGrowing(t *testing.T) {
	shape := gemma4ShapeR707()
	const at = 512
	atWindow := shape.BytesAtCtx(KVF16, at)
	atDouble := shape.BytesAtCtx(KVF16, at*2)
	globalPerToken := shape.Global.BytesPerLayerPerToken(KVF16).Mul(int64(shape.Global.Layers))
	if delta := atDouble - atWindow; delta != globalPerToken.Mul(int64(at)) {
		t.Errorf("прирост окно→2×окно = %d, want %d (только глобальные слои)",
			delta, globalPerToken.Mul(int64(at)))
	}
	// Полный KV много меньше «kvPerToken × ctx» — за это и боролись: на 1024
	// оконная часть зафиксирована окном и не удваивается вместе с контекстом.
	naive := shape.BytesPerToken(KVF16).Mul(at * 2)
	if atDouble >= naive {
		t.Errorf("KV на 1024 (%d) должен быть меньше наивного (%d)", atDouble, naive)
	}
	if atDouble*3 > naive*2 {
		t.Errorf("KV на 1024 (%d) меньше наивного (%d) лишь на %d%% — окно почти не учтено",
			atDouble, naive, 100-100*atDouble/naive)
	}
}

// TestKVShapeMaxCtxRespectsWindow — MaxCtxForBudget обратна BytesAtCtx.
func TestKVShapeMaxCtxRespectsWindow(t *testing.T) {
	shape := gemma4ShapeR707()
	const budget = Bytes(4 * GiB)
	ctx := shape.MaxCtxForBudget(KVF16, budget, 0)
	if ctx <= shape.SWAWindow {
		t.Fatalf("ctx = %d ≤ окна %d — бюджет позволяет больше, расчёт не учёл окно", ctx, shape.SWAWindow)
	}
	if got := shape.BytesAtCtx(KVF16, ctx); got > budget {
		t.Errorf("на рекомендованном ctx=%d KV = %d > бюджета %d", ctx, got, budget)
	}
	if got := shape.BytesAtCtx(KVF16, ctx+1); got <= budget {
		t.Errorf("на ctx+1 = %d KV = %d всё ещё влезает — потолок занижен", ctx+1, got)
	}
	// Наивное «бюджет / KV на токен» дало бы меньше: окно даёт запас.
	naive := int(budget / shape.BytesPerToken(KVF16))
	if ctx <= naive {
		t.Errorf("с окном ctx = %d должен быть больше наивного %d", ctx, naive)
	}
	// Потолок по модели соблюдается.
	if got := shape.MaxCtxForBudget(KVF16, budget, 8192); got != 8192 {
		t.Errorf("ceiling не применён: %d, want 8192", got)
	}
}

// TestModelSpecKVShapeMatchesTotal — KVShape у ModelSpec даёт ТО ЖЕ, что
// KVTotalBytes: одна формула, два входа.
func TestModelSpecKVShapeMatchesTotal(t *testing.T) {
	m := gemma4
	for _, kv := range []KVType{KVF16, KVQ8, KVQ4} {
		for _, ctx := range []int{512, 4096, 65536} {
			if got, want := m.KVShape().BytesAtCtx(kv, ctx), KVTotalBytes(m, kv, ctx); got != want {
				t.Errorf("%s ctx=%d: KVShape %d != KVTotalBytes %d", kv, ctx, got, want)
			}
		}
	}
}

// head_dim_width_r705_test.go — R705 (2026-11-24): балансер считает KV по
// реальной ширине головы и не теряет её источник.
//
// ПРЕДМЕТ. R659/AutoTune берут геометрию модели из /api/models (types.LlamaCppModel
// → ModelProfileInfo) и по ней решают «поднимется ли модель с нужным окном на
// свободном узле». Пока head_dim_k приходил из формулы n_embd/n_heads, KV на
// токен занижался, и узел проходил проверку окна, которое физически не помещалось:
//
//	Qwen3-4B:       head_dim 80  вместо 128 → KV ×0.625 и «влезает» окно на 60 %
//	                больше реального;
//	gemma-4-E4B:    320 вместо 512;
//	Inkling-2-Mini: 96  вместо 128;
//	Qwen3.8-9B:     256 = 256 — числа не меняются.
//
// Тесты ниже фиксируют: одна и та же функция kvCacheBytesForContext и один и тот же
// вердикт coldLoadWindowFitPartsR659 дают РАЗНЫЕ ответы при 80 и 128 — то есть
// ширина действительно меняет решение, а не только поле в ответе.
package balancer

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// qwen3_4bProfileR705 — геометрия Qwen3-4B из /api/models с реальной шириной.
func qwen3_4bProfileR705(headDim int) *ModelProfileInfo {
	return &ModelProfileInfo{
		Name:     "Qwen3-4B-Instruct-2507-Q4_K_M",
		NLayers:  36,
		NEmbd:    2560,
		NKvHeads: 8,
		HeadDimK: headDim,
		HeadDimV: headDim,
	}
}

// TestR705_KVEstimateUsesRealHeadDim — KV для 16K окна, q8_0.
//
// Ручной расчёт: элементов на токен = 2 × 36 × 8 × 128 = 73 728; q8_0 = 34/32
// байта на элемент → 78 336 Б/токен; × 16 384 = 1 283 457 024 Б.
// По прежней ширине 80: 46 080 элементов → 48 960 Б/токен → 802 160 640 Б.
func TestR705_KVEstimateUsesRealHeadDim(t *testing.T) {
	const (
		wantBytesReal    = uint64(1283457024) // 2×36×8×128 × 34/32 × 16384
		wantBytesFormula = uint64(802160640)  // 2×36×8×80  × 34/32 × 16384
		nCtx             = 16384
	)
	real := kvCacheBytesForContext(qwen3_4bProfileR705(128), "q8_0", nCtx)
	if real != wantBytesReal {
		t.Fatalf("KV(128) = %d, want %d", real, wantBytesReal)
	}
	computed := kvCacheBytesForContext(qwen3_4bProfileR705(80), "q8_0", nCtx)
	if computed != wantBytesFormula {
		t.Fatalf("KV(80) = %d, want %d", computed, wantBytesFormula)
	}
	// НЕ-ВАКУУМНОСТЬ: ширина реально меняет число, и ровно в 1.6 раза (8/5).
	if real*5 != computed*8 {
		t.Errorf("отношение KV(128)/KV(80) = %.3f, ожидалось 1.6 — ширина не влияет на оценку",
			float64(real)/float64(computed))
	}
}

// TestR705_R659WindowFitDependsOnHeadDim — та же свободная VRAM, то же окно:
// с шириной 80 узел проходит проверку, с 128 — нет.
//
// Живой смысл: именно так R659 ставит модель с окном на «свободный» узел, если
// KV считался по заниженной ширине, — и загрузка упирается в OOM.
func TestR705_R659WindowFitDependsOnHeadDim(t *testing.T) {
	// Веса 2000 MB, резерв 256 MB, один слот, окно 16K → KV(128) = 1224 MB,
	// KV(80) = 765 MB. Свободно 3100 MB: 2000+1224+256 = 3480 — не влезает;
	// 2000+765+256 = 3021 — влезает.
	const (
		window   = 16384
		weights  = 2000
		freeMB   = 3100
		reserve  = 256
		slots    = 1
		wantKVRe = 1224
		wantKVCo = 765
	)

	fitReal := coldLoadWindowFitPartsR659(qwen3_4bProfileR705(128), "q8_0", window, weights, freeMB, reserve, slots)
	if !fitReal.Verified {
		t.Fatalf("вердикт по ширине 128 не верифицирован: %+v", fitReal)
	}
	if fitReal.KVMB != wantKVRe {
		t.Errorf("KVMB(128) = %d, want %d", fitReal.KVMB, wantKVRe)
	}
	if fitReal.Fits {
		t.Errorf("с шириной 128 окно 16384 признано влезающим при свободных %d MB (need=%d) — "+
			"это ровно та ошибка, из-за которой загрузка падала OOM", freeMB, fitReal.NeedMB)
	}

	fitComputed := coldLoadWindowFitPartsR659(qwen3_4bProfileR705(80), "q8_0", window, weights, freeMB, reserve, slots)
	if fitComputed.KVMB != wantKVCo {
		t.Errorf("KVMB(80) = %d, want %d", fitComputed.KVMB, wantKVCo)
	}
	if !fitComputed.Fits {
		t.Errorf("с шириной 80 окно 16384 признано не влезающим (need=%d, free=%d) — "+
			"ожидалось «влезает» (это ответ по заниженному KV)", fitComputed.NeedMB, freeMB)
	}
	// Вердикт РАЗНЫЙ — значит ширина действительно доходит до решения R659.
	if fitReal.Fits == fitComputed.Fits {
		t.Fatal("вердикт R659 одинаков при ширине 80 и 128 — изменение ширины не влияет на решение")
	}
}

// TestR705_ArchEnrichParsesSource — балансерный добор из /api/show сохраняет
// head_dim_source ("gguf" от воркера, "computed" когда ширина добрана формулой).
func TestR705_ArchEnrichParsesSource(t *testing.T) {
	srv, _, _ := archTestServer(t, map[string]interface{}{
		"n_kv_heads":      8.0,
		"n_heads":         32.0,
		"head_dim_k":      128.0,
		"head_dim_v":      128.0,
		"head_dim_source": "gguf",
		"n_embd":          2560.0,
	}, http.StatusOK)
	p := newArchPoller()
	b := backendForServer(t, srv)

	arch, ok := p.fetchArchParams(b, "Qwen3-4B-Instruct-2507-Q4_K_M")
	if !ok {
		t.Fatal("fetchArchParams не разобрал /api/show")
	}
	if arch.HeadDimK != 128 {
		t.Errorf("HeadDimK = %d, want 128", arch.HeadDimK)
	}
	if arch.HeadDimSource != "gguf" {
		t.Errorf("HeadDimSource = %q, want %q", arch.HeadDimSource, "gguf")
	}

	// Модель, где воркер прислал только n_embd/n_heads (старый cppworker):
	// добранная формулой ширина обязана быть помечена computed.
	srv2, _, _ := archTestServer(t, map[string]interface{}{
		"n_kv_heads": 8.0,
		"n_heads":    32.0,
		"n_embd":     2560.0,
	}, http.StatusOK)
	arch2, ok := p.fetchArchParams(backendForServer(t, srv2), "gemma-4-E4B-it-Q4_K_M")
	if !ok {
		t.Fatal("fetchArchParams не разобрал /api/show без head_dim")
	}
	if arch2.HeadDimK != 80 {
		t.Errorf("добранная ширина = %d, want 80 (n_embd/n_heads)", arch2.HeadDimK)
	}
	if arch2.HeadDimSource != "computed" {
		t.Errorf("источник добранной ширины = %q, want %q", arch2.HeadDimSource, "computed")
	}

	// Мост в модель: явные значения воркера не перетираются, источник приезжает.
	m := &types.LlamaCppModel{Name: "m"}
	applyArchParams(m, arch)
	if m.HeadDimK != 128 || m.HeadDimSource != "gguf" {
		t.Errorf("applyArchParams: head_dim=%d source=%q, want 128/gguf", m.HeadDimK, m.HeadDimSource)
	}
	mExplicit := &types.LlamaCppModel{Name: "m", HeadDimK: 512, HeadDimSource: "gguf"}
	applyArchParams(mExplicit, arch)
	if mExplicit.HeadDimK != 512 {
		t.Errorf("applyArchParams перетёр явную ширину воркера: %d", mExplicit.HeadDimK)
	}
}

// TestR705_WireCarriesSource — сторож провода: /api/models воркера и поллер
// балансера обязаны называть поле одинаково (head_dim_source). Переименование
// любой стороны молча вернёт «источник неизвестен».
func TestR705_WireCarriesSource(t *testing.T) {
	// Сторона воркера уже проверяется в internal/cppbackend (сторож R705), здесь —
	// потребитель: поллер читает snake_case и переносит в модель.
	raw, err := os.ReadFile("llamacpp_metrics_poller.go")
	if err != nil {
		t.Fatalf("не читается llamacpp_metrics_poller.go: %v", err)
	}
	text := strings.Join(strings.Fields(string(raw)), " ")
	if !strings.Contains(text, `head_dim_source,`) {
		t.Fatal("поллер не читает head_dim_source — R659 не узнает, оценка это или факт")
	}
	if !strings.Contains(text, "HeadDimSource: m.HeadDimSource,") {
		t.Fatal("поллер не переносит HeadDimSource в types.LlamaCppModel")
	}

	// Поле действительно сериализуется в API балансера (WebUI читает headDimK
	// оттуда же — источник едет рядом, чтобы панель могла его показать).
	blob, err := json.Marshal(types.LlamaCppModel{Name: "m", HeadDimK: 128, HeadDimSource: "gguf"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(blob), `"headDimSource":"gguf"`) {
		t.Fatalf("в JSON модели балансера нет headDimSource: %s", blob)
	}
}

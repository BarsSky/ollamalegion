package composition

// R95 (S4): оценка сетевого бюджета раскладки.
//
// Числа сверяются с §3 плана 2026-10-07: при H=4096, L=32, канал 112 МБ/с
// (1 Гбит/с) layer split даёт 8 КБ на токен (0.07 мс) и 4.2 МБ на префилл 512
// (37 мс), row split — 256 КБ (2.3 мс) и 134 МБ (1.2 с).
//
// Эти числа — не «оценка на глаз»: именно из них строится предупреждение в UI,
// поэтому расхождение с планом должно ломать тест.

import (
	"math"
	"testing"
)

const (
	testHidden = 4096
	testLayers = 32
	testBW     = 112e6 // 112 МБ/с = 1 Гбит/с полезной нагрузки
	testTPS    = 30.0
)

func approx(t *testing.T, got, want, tolerance float64, what string) {
	t.Helper()
	if math.Abs(got-want) > tolerance {
		t.Errorf("%s = %.4f, ожидалось %.4f (±%.4f)", what, got, want, tolerance)
	}
}

func TestEstimateTraffic_LayerSplitMatchesPlan_R95(t *testing.T) {
	estimate := EstimateTraffic(TrafficInput{
		Strategy:             StrategyLayer,
		HiddenSize:           testHidden,
		Layers:               testLayers,
		BytesPerElem:         2,
		PrefillTokens:        512,
		BandwidthBytesPerSec: testBW,
		TokensPerSecond:      testTPS,
	})
	if !estimate.Known {
		t.Fatalf("оценка должна считаться: %s", estimate.Reason)
	}
	if estimate.BytesPerToken != 8192 {
		t.Errorf("BytesPerToken = %d, ожидалось 8192 (8 КБ)", estimate.BytesPerToken)
	}
	if estimate.BytesPerPrefill != 4<<20 {
		t.Errorf("BytesPerPrefill = %d, ожидалось %d (4 МиБ)", estimate.BytesPerPrefill, 4<<20)
	}
	approx(t, estimate.TokenOverheadMs, 0.073, 0.005, "TokenOverheadMs")
	approx(t, estimate.PrefillMs, 37.4, 1.0, "PrefillMs")
	approx(t, estimate.TokenOverheadPercent, 0.22, 0.05, "TokenOverheadPercent")
	if estimate.TokenOverheadPercent >= 0.5 {
		t.Errorf("layer split должен быть <0.5 %% времени токена: %.3f", estimate.TokenOverheadPercent)
	}
}

func TestEstimateTraffic_RowSplitMatchesPlan_R95(t *testing.T) {
	estimate := EstimateTraffic(TrafficInput{
		Strategy:             StrategyRow,
		HiddenSize:           testHidden,
		Layers:               testLayers,
		BytesPerElem:         2,
		PrefillTokens:        512,
		BandwidthBytesPerSec: testBW,
		TokensPerSecond:      testTPS,
	})
	if estimate.BytesPerToken != 8192*testLayers {
		t.Errorf("BytesPerToken = %d, ожидалось %d (256 КБ)", estimate.BytesPerToken, 8192*testLayers)
	}
	approx(t, estimate.TokenOverheadMs, 2.34, 0.1, "TokenOverheadMs")
	approx(t, estimate.PrefillMs, 1198, 30, "PrefillMs")
	approx(t, estimate.TokenOverheadPercent, 7.0, 0.5, "TokenOverheadPercent")
}

func TestEstimateTraffic_RowIsLayersTimesLayer_R95(t *testing.T) {
	layer := EstimateTraffic(TrafficInput{
		Strategy: StrategyLayer, HiddenSize: testHidden, Layers: testLayers,
		BytesPerElem: 2, PrefillTokens: 512, BandwidthBytesPerSec: testBW,
	})
	row := EstimateTraffic(TrafficInput{
		Strategy: StrategyRow, HiddenSize: testHidden, Layers: testLayers,
		BytesPerElem: 2, PrefillTokens: 512, BandwidthBytesPerSec: testBW,
	})
	if row.BytesPerToken != layer.BytesPerToken*testLayers {
		t.Errorf("row должен быть дороже layer ровно в L раз: %d vs %d",
			row.BytesPerToken, layer.BytesPerToken*testLayers)
	}
	if row.BytesPerPrefill != layer.BytesPerPrefill*testLayers {
		t.Errorf("то же для префилла: %d vs %d", row.BytesPerPrefill, layer.BytesPerPrefill*testLayers)
	}
}

func TestEstimateTraffic_FasterLinkScalesLinearly_R95(t *testing.T) {
	// 10 Гбит/с (1120 МБ/с) — ускорение, а не требование: трафик в байтах тот
	// же, время падает в 10 раз. Архитектура не должна требовать 10 Гбит/с.
	slow := EstimateTraffic(TrafficInput{
		Strategy: StrategyRow, HiddenSize: testHidden, Layers: testLayers,
		BytesPerElem: 2, PrefillTokens: 512, BandwidthBytesPerSec: testBW,
	})
	fast := EstimateTraffic(TrafficInput{
		Strategy: StrategyRow, HiddenSize: testHidden, Layers: testLayers,
		BytesPerElem: 2, PrefillTokens: 512, BandwidthBytesPerSec: testBW * 10,
	})
	if fast.BytesPerToken != slow.BytesPerToken {
		t.Errorf("объём трафика не зависит от канала: %d vs %d", fast.BytesPerToken, slow.BytesPerToken)
	}
	approx(t, fast.TokenOverheadMs, slow.TokenOverheadMs/10, 0.01, "TokenOverheadMs(10G)")
}

func TestEstimateTraffic_F32DoublesTraffic_R95(t *testing.T) {
	f16 := EstimateTraffic(TrafficInput{
		Strategy: StrategyLayer, HiddenSize: testHidden, Layers: testLayers,
		BytesPerElem: 2, PrefillTokens: 512, BandwidthBytesPerSec: testBW,
	})
	f32 := EstimateTraffic(TrafficInput{
		Strategy: StrategyLayer, HiddenSize: testHidden, Layers: testLayers,
		BytesPerElem: 4, PrefillTokens: 512, BandwidthBytesPerSec: testBW,
	})
	if f32.BytesPerToken != f16.BytesPerToken*2 {
		t.Errorf("F32 должен быть вдвое дороже: %d vs %d", f32.BytesPerToken, f16.BytesPerToken*2)
	}
}

func TestEstimateTraffic_UnknownInputs_R95(t *testing.T) {
	cases := []struct {
		name string
		in   TrafficInput
		want string
	}{
		{
			name: "нет hidden size",
			in:   TrafficInput{Strategy: StrategyLayer, BandwidthBytesPerSec: testBW},
			want: "hidden size",
		},
		{
			name: "нет пропускной способности",
			in:   TrafficInput{Strategy: StrategyLayer, HiddenSize: testHidden},
			want: "пропускная способность",
		},
		{
			name: "row без числа слоёв",
			in: TrafficInput{
				Strategy: StrategyRow, HiddenSize: testHidden, BandwidthBytesPerSec: testBW,
			},
			want: "слоёв",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			estimate := EstimateTraffic(tc.in)
			if estimate.Known {
				t.Fatalf("оценка не должна считаться: %+v", estimate)
			}
			if !contains(estimate.Reason, tc.want) {
				t.Errorf("причина = %q, ожидалось упоминание %q", estimate.Reason, tc.want)
			}
			if estimate.TokenOverheadPercent != -1 {
				t.Errorf("доля времени токена должна быть «неизвестно» (-1), получено %v",
					estimate.TokenOverheadPercent)
			}
		})
	}
}

func TestEstimateTraffic_NoTokensPerSecond_NoPercent_R95(t *testing.T) {
	// Абсолютная задержка известна всегда; доля от времени токена — только если
	// известно само время токена.
	estimate := EstimateTraffic(TrafficInput{
		Strategy: StrategyLayer, HiddenSize: testHidden, Layers: testLayers,
		BytesPerElem: 2, PrefillTokens: 512, BandwidthBytesPerSec: testBW,
	})
	if !estimate.Known {
		t.Fatal("оценка должна считаться")
	}
	if estimate.TokenOverheadPercent != -1 {
		t.Errorf("без tokensPerSecond доля неизвестна: %v", estimate.TokenOverheadPercent)
	}
	if estimate.TokenOverheadMs <= 0 {
		t.Errorf("абсолютная задержка должна быть посчитана: %v", estimate.TokenOverheadMs)
	}
}

func TestNormalizeHelpers_R95(t *testing.T) {
	for _, in := range []string{"row", "ROW", " tensor ", "row_split"} {
		if got := NormalizeStrategy(in); got != StrategyRow {
			t.Errorf("NormalizeStrategy(%q) = %q, ожидалось row", in, got)
		}
	}
	for _, in := range []string{"", "layer", "pipeline", "layer_split"} {
		if got := NormalizeStrategy(in); got != StrategyLayer {
			t.Errorf("NormalizeStrategy(%q) = %q, ожидалось layer", in, got)
		}
	}
	for _, tc := range []struct{ in, want string }{
		{"", "ollama"},
		{"llama_cpp", "llama_cpp"},
		{"llama.cpp", "llama_cpp"},
		{"cppworker", "llama_cpp"},
		{"image_cpp", "image_cpp"},
		{"sdworker", "image_cpp"},
		{"vllm", "vllm"},
	} {
		if got := NormalizeBackendType(tc.in); got != tc.want {
			t.Errorf("NormalizeBackendType(%q) = %q, ожидалось %q", tc.in, got, tc.want)
		}
	}
}

func contains(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

func TestHumanBytes_R95(t *testing.T) {
	cases := []struct {
		lang  string
		bytes int64
		want  string
	}{
		{LangRU, 10 << 30, "10.0 ГБ"},
		{LangEN, 10 << 30, "10.0 GB"},
		{LangRU, 512 << 20, "512 МБ"},
		{LangEN, 512 << 20, "512 MB"},
		{LangRU, 0, "0 Б"},
		{LangEN, 0, "0 B"},
	}
	for _, tc := range cases {
		if got := HumanBytes(tc.lang, tc.bytes); got != tc.want {
			t.Errorf("HumanBytes(%q, %d) = %q, ожидалось %q", tc.lang, tc.bytes, got, tc.want)
		}
	}
}

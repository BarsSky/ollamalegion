package logger

// broker_fields_float_r493_test.go — R493-12k (2026-11-13): дробные поля лога
// попадают в панель/дамп ЧИСЛОМ, а не битами IEEE-754.
//
// ЖИВОЙ ФАКТ из архива второго стенда (balancer-logs.txt, каждую минуту):
//
//	backend VRAM usage above threshold backend=A10-126
//	  threshold=4635681760191971328 vram_percent=4635833999675857520
//
// 4635833999675857520 — это 87.16 % (биты float64). По такому «числу» нельзя
// принять ни одного решения: не видно ни порога (85), ни фактической занятости.
// Причина в zapcore: дробное поле лежит в Field.Integer КАК БИТЫ, а карта полей
// складывала Integer без разбора типа.

import (
	"math"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// TestR493_FloatFieldIsNumber — точные числа из архива.
func TestR493_FloatFieldIsNumber(t *testing.T) {
	fields := sweetenR493(t,
		"threshold", float64(85),
		"vram_percent", 87.16345318742401,
		"gpu_usage", float64(94),
		"load_ratio", float64(1),
	)
	got := brokerFieldsMap(fields)

	if v, ok := got["threshold"].(float64); !ok || v != 85 {
		t.Fatalf("threshold = %v (%T), ожидалось 85 (float64). Биты IEEE-754 в "+
			"панели — это дефект R493-12k: %v", got["threshold"], got["threshold"], got["threshold"])
	}
	if v, ok := got["vram_percent"].(float64); !ok || math.Abs(v-87.16345318742401) > 1e-9 {
		t.Fatalf("vram_percent = %v (%T), ожидалось 87.16345318742401", got["vram_percent"], got["vram_percent"])
	}
	if v, ok := got["gpu_usage"].(float64); !ok || v != 94 {
		t.Fatalf("gpu_usage = %v (%T), ожидалось 94", got["gpu_usage"], got["gpu_usage"])
	}
	if v, ok := got["load_ratio"].(float64); !ok || v != 1 {
		t.Fatalf("load_ratio = %v (%T), ожидалось 1.0 (раньше 4607182418800017408)",
			got["load_ratio"], got["load_ratio"])
	}
}

// TestR493_FloatFieldInTextDump — то же в текстовом дампе: там форматируется
// значение из карты, поэтому дефект был виден и в balancer-logs.txt.
func TestR493_FloatFieldInTextDump(t *testing.T) {
	got := brokerFieldsMap(sweetenR493(t, "vram_percent", 87.16345318742401, "threshold", 85.0))
	text := FieldsText(got)
	// FieldsText отдаёт ведущий пробел перед первым ключом (поля приклеиваются
	// к сообщению), поэтому сравниваем с ним.
	if want := " threshold=85 vram_percent=87.16345318742401"; text != want {
		t.Fatalf("текстовый дамп = %q, ожидалось %q", text, want)
	}
}

// TestR493_OtherZapTypes — целые, bool, строки, время и длительность не
// пострадали: тип разбирается, а не угадывается.
func TestR493_OtherZapTypes(t *testing.T) {
	fields := []zapcore.Field{
		zap.Int64("n", 12),
		zap.Uint64("u", 18446744073709551615),
		zap.Bool("b", true),
		zap.String("s", "ok"),
		zap.String("empty", ""),
		zap.Duration("took", 1500*time.Millisecond),
	}
	got := brokerFieldsMap(fields)

	if v, ok := got["n"].(int64); !ok || v != 12 {
		t.Fatalf("n = %v (%T)", got["n"], got["n"])
	}
	if v, ok := got["u"].(uint64); !ok || v != 18446744073709551615 {
		t.Fatalf("u = %v (%T): uint64 не должен становиться отрицательным int64", got["u"], got["u"])
	}
	if v, ok := got["b"].(bool); !ok || !v {
		t.Fatalf("b = %v (%T)", got["b"], got["b"])
	}
	if v, ok := got["s"].(string); !ok || v != "ok" {
		t.Fatalf("s = %v (%T)", got["s"], got["s"])
	}
	if v, ok := got["empty"].(string); !ok || v != "" {
		t.Fatalf("empty = %v (%T): пустая строка обязана остаться строкой", got["empty"], got["empty"])
	}
	if v, ok := got["took"].(time.Duration); !ok || v != 1500*time.Millisecond {
		t.Fatalf("took = %v (%T), ожидалось 1.5s", got["took"], got["took"])
	}
	if text := FieldsText(got); text == "" {
		t.Fatal("поля потерялись в текстовом дампе")
	}
}

// sweetenR493 — превратить пары key/value в []zapcore.Field так же, как это
// делает SugaredLogger: float64 → zap.Float64 (именно здесь появлялись биты).
func sweetenR493(t *testing.T, kv ...interface{}) []zapcore.Field {
	t.Helper()
	if len(kv)%2 != 0 {
		t.Fatal("нужны пары key/value")
	}
	out := make([]zapcore.Field, 0, len(kv)/2)
	for i := 0; i < len(kv); i += 2 {
		key, ok := kv[i].(string)
		if !ok {
			t.Fatalf("ключ %v не строка", kv[i])
		}
		switch v := kv[i+1].(type) {
		case float64:
			out = append(out, zap.Float64(key, v))
		case string:
			out = append(out, zap.String(key, v))
		case int:
			out = append(out, zap.Int64(key, int64(v)))
		default:
			out = append(out, zap.Any(key, v))
		}
	}
	return out
}

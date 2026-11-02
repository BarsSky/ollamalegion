package settings

// builtin_r388_test.go — R388 (2026-11-02): поведенческие выключатели в реестре.
//
// Зачем тест: панель «Горячие настройки» рисует то, что зарегистрировано.
// Если выключатель не зарегистрирован (или зарегистрирован «не тем» типом и
// HotReload), оператор не увидит его в WebUI — и снова будет «фича есть, а
// переключателя нет».

import (
	"errors"
	"testing"
)

// TestR388_BehaviorSwitchesRegistered — все пять обязаны быть в реестре с
// ожидаемыми типом/умолчанием/перезагрузкой.
func TestR388_BehaviorSwitchesRegistered(t *testing.T) {
	want := []struct {
		name     string
		def      int64
		hot      bool
		hotShort string // человеческое пояснение для отчёта об ошибке
	}{
		{"LB_LAYOUT_HEAL", 1, true, "автолечение раскладки (R383)"},
		{"LB_NCTX_GROW_FOR_STREAM", 1, true, "рост окна на стриме (R385)"},
		{"LB_OPENAI_AUTO_STREAM", 0, true, "внутренний стрим для non-stream /v1"},
		{"LB_AUTO_CONTINUE_ON_TRUNCATION", 0, true, "авто-продолжение — опция"},
		{"LB_OLLAMA_NATIVE_PATH", 1, false, "нативная поверхность: только с рестартом"},
	}
	for _, tc := range want {
		def, ok := builtinsR278(tc.name)
		if !ok {
			t.Errorf("%s (%s) НЕ зарегистрирован — в панели WebUI его не будет", tc.name, tc.hotShort)
			continue
		}
		if def.Type != TypeBool {
			t.Errorf("%s: тип %q, ожидался TypeBool (0/1)", tc.name, def.Type)
		}
		if def.Default != tc.def {
			t.Errorf("%s: умолчание %d, ожидалось %d — иначе молча меняется поведение", tc.name, def.Default, tc.def)
		}
		if def.HotReload != tc.hot {
			t.Errorf("%s: HotReload=%v, ожидалось %v (%s)", tc.name, def.HotReload, tc.hot, tc.hotShort)
		}
	}
}

// TestR388_NonHotSwitchRefusesChange — нативную поверхность нельзя переключить
// на живой сессии: Set обязан ответить ErrNotHot, а не «применилось».
func TestR388_NonHotSwitchRefusesChange(t *testing.T) {
	r := New("")
	registerBuiltinsR278(r)

	if _, err := r.Set("LB_OLLAMA_NATIVE_PATH", 0); !errors.Is(err, ErrNotHot) {
		t.Fatalf("Set(LB_OLLAMA_NATIVE_PATH) вернул %v, ожидался ErrNotHot", err)
	}
	// А горячие — меняются и действуют.
	src, err := r.Set("LB_NCTX_GROW_FOR_STREAM", 0)
	if err != nil {
		t.Fatalf("Set(LB_NCTX_GROW_FOR_STREAM) вернул ошибку: %v", err)
	}
	if src != SourceOverride {
		t.Errorf("источник после Set = %v, ожидался override", src)
	}
	if v, _, ok := r.Effective("LB_NCTX_GROW_FOR_STREAM"); !ok || v != 0 {
		t.Errorf("Effective после Set = (%d, %v), ожидалось (0, true)", v, ok)
	}
}

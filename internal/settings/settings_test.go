package settings

// settings_test.go — R278: реестр «горячих» настроек.
//
// Проверяем ровно те обещания, которые дают смысл всей затее:
// приоритет источников (override → env → default), hot-apply (значение меняется
// без рестарта), персист (переживает «рестарт» процесса), отказ для не-hot
// настроек и валидацию записи оператора. Историческая лояльность к мусору в env
// проверяется отдельно — иначе одна опечатка в .env снова сломает стенд.

import (
	"os"
	"path/filepath"
	"testing"
)

// newTestRegistry — реестр с одной настройкой и временным override-файлом.
func newTestRegistry(t *testing.T) (*Registry, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "settings-overrides.json")
	return New(path), path
}

func TestR278_SourcePriority(t *testing.T) {
	r, _ := newTestRegistry(t)
	r.Register(Setting{Name: "LB_TEST_INT", Type: TypeInt, Default: 100, HotReload: true})

	// 1. Дефолт — когда ни env, ни override нет.
	if v, src, ok := r.Effective("LB_TEST_INT"); !ok || v != 100 || src != SourceDefault {
		t.Fatalf("default: got (%d,%s,%v), want (100,default,true)", v, src, ok)
	}

	// 2. env побеждает дефолт.
	t.Setenv("LB_TEST_INT", "200")
	if v, src, _ := r.Effective("LB_TEST_INT"); v != 200 || src != SourceEnv {
		t.Fatalf("env: got (%d,%s), want (200,env)", v, src)
	}

	// 3. override побеждает env (главное правило: решение оператора из WebUI
	// важнее .env, иначе «всё, что задано в env, изменяемо из WebUI» — ложь).
	if _, err := r.Set("LB_TEST_INT", 300); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if v, src, _ := r.Effective("LB_TEST_INT"); v != 300 || src != SourceOverride {
		t.Fatalf("override: got (%d,%s), want (300,override)", v, src)
	}

	// 4. Reset возвращает к env.
	if err := r.Reset("LB_TEST_INT"); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if v, src, _ := r.Effective("LB_TEST_INT"); v != 200 || src != SourceEnv {
		t.Fatalf("after reset: got (%d,%s), want (200,env)", v, src)
	}
}

func TestR278_NotHotRejected(t *testing.T) {
	r, _ := newTestRegistry(t)
	r.Register(Setting{Name: "LB_TEST_COLD", Type: TypeInt, Default: 1, HotReload: false})
	if _, err := r.Set("LB_TEST_COLD", 2); err == nil {
		t.Fatal("Set on non-hot setting must fail: молча сохранённое значение применится «когда-нибудь»")
	}
}

func TestR278_Validation(t *testing.T) {
	r, _ := newTestRegistry(t)
	r.Register(Setting{
		Name:      "LB_TEST_VALIDATED",
		Type:      TypeBytes,
		Default:   4096,
		HotReload: true,
		Validate: func(v int64) error {
			if v < 4096 {
				return os.ErrInvalid
			}
			return nil
		},
	})
	if _, err := r.Set("LB_TEST_VALIDATED", 100); err == nil {
		t.Fatal("невалидное значение должно отклоняться")
	}
	if _, err := r.Set("LB_TEST_VALIDATED", 8192); err != nil {
		t.Fatalf("валидное значение отклонено: %v", err)
	}
	if _, err := r.Set("LB_TEST_UNKNOWN", 1); err == nil {
		t.Fatal("неизвестная настройка должна отклоняться")
	}
}

func TestR278_PersistSurvivesRestart(t *testing.T) {
	r, path := newTestRegistry(t)
	r.Register(Setting{Name: "LB_TEST_PERSIST", Type: TypeInt, Default: 1, HotReload: true})
	if _, err := r.Set("LB_TEST_PERSIST", 42); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("override-файл не создан: %v", err)
	}

	// «Рестарт»: новый экземпляр читает тот же файл.
	r2 := New(path)
	r2.Register(Setting{Name: "LB_TEST_PERSIST", Type: TypeInt, Default: 1, HotReload: true})
	if v, src, _ := r2.Effective("LB_TEST_PERSIST"); v != 42 || src != SourceOverride {
		t.Fatalf("после рестарта: got (%d,%s), want (42,override)", v, src)
	}
}

func TestR278_ResetAll(t *testing.T) {
	r, _ := newTestRegistry(t)
	r.Register(Setting{Name: "LB_TEST_A", Type: TypeInt, Default: 1, HotReload: true})
	r.Register(Setting{Name: "LB_TEST_B", Type: TypeInt, Default: 2, HotReload: true})
	if _, err := r.Set("LB_TEST_A", 10); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Set("LB_TEST_B", 20); err != nil {
		t.Fatal(err)
	}
	if err := r.ResetAll(); err != nil {
		t.Fatalf("ResetAll: %v", err)
	}
	if v, src, _ := r.Effective("LB_TEST_A"); v != 1 || src != SourceDefault {
		t.Fatalf("после ResetAll: got (%d,%s), want (1,default)", v, src)
	}
	if n := r.OverrideCount(); n != 0 {
		t.Fatalf("override count = %d, want 0", n)
	}
}

// TestR278_EnvGarbageFallsBack — историческая лояльность: мусор и «слишком
// маленькое» значение из env не ломают стенд, а дают дефолт.
func TestR278_EnvGarbageFallsBack(t *testing.T) {
	r, _ := newTestRegistry(t)
	def, ok := builtinsR278("LB_STREAM_MAX_LINE_BYTES")
	if !ok {
		t.Fatal("встроенная настройка не найдена")
	}
	r.Register(def)

	for _, raw := range []string{"мусор", "", "1024"} {
		t.Setenv("LB_STREAM_MAX_LINE_BYTES", raw)
		v, src, _ := r.Effective("LB_STREAM_MAX_LINE_BYTES")
		if v != def.Default {
			t.Errorf("env=%q: got %d, want default %d", raw, v, def.Default)
		}
		if raw != "" && src != SourceDefault {
			// для непустого мусора источник — дефолт после нормализации
			t.Errorf("env=%q: source %s, want default (нормализация)", raw, src)
		}
	}

	// Валидное значение проходит как есть.
	t.Setenv("LB_STREAM_MAX_LINE_BYTES", "1048576")
	if v, src, _ := r.Effective("LB_STREAM_MAX_LINE_BYTES"); v != 1048576 || src != SourceEnv {
		t.Fatalf("валидный env: got (%d,%s)", v, src)
	}
}

// TestR278_HotIntWithoutInit — без Init глобального реестра значение всё равно
// вычисляется по той же формуле (env → default): иначе в тестах и лёгких
// сборках появилось бы ВТОРОЕ мнение о приоритете источников.
func TestR278_HotIntWithoutInit(t *testing.T) {
	t.Setenv("LB_TOOL_CALL_BUFFER_BYTES", "131072")
	if v, ok := HotInt("LB_TOOL_CALL_BUFFER_BYTES"); !ok || v != 131072 {
		t.Fatalf("HotInt без Init: got (%d,%v), want (131072,true)", v, ok)
	}
	if _, ok := HotInt("LB_NOT_REGISTERED_ANYWHERE"); ok {
		t.Fatal("незарегистрированная настройка не должна возвращать ok")
	}
}

// TestR278_SnapshotDeterministic — порядок стабилен (по имени): API и WebUI не
// должны «перетасовывать» список между запросами.
func TestR278_SnapshotDeterministic(t *testing.T) {
	r := New("")
	registerBuiltinsR278(r)
	first := r.Snapshot()
	second := r.Snapshot()
	if len(first) == 0 {
		t.Fatal("реестр пуст")
	}
	for i := range first {
		if first[i].Name != second[i].Name {
			t.Fatalf("порядок нестабилен: %q vs %q", first[i].Name, second[i].Name)
		}
		if i > 0 && first[i-1].Name > first[i].Name {
			t.Fatalf("порядок не по имени: %q > %q", first[i-1].Name, first[i].Name)
		}
	}
}

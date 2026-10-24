//go:build llama_stub

// cuda_arch_guard_test.go — R135 (2026-10-16): образ и карта должны совпадать по
// архитектуре CUDA, иначе первое GPU-ядро убивает процесс.
//
// ЖИВОЙ ДЕФЕКТ (лог бэкенда, Quadro RTX 4000, compute capability 7.5):
//
//	[bridge] model loaded successfully (effective n_ctx=128000)
//	CUDA error: no kernel image is available for execution on the device
//	SIGABRT: abort
//
// шесть падений подряд с перезапуском контейнера. Снаружи — «backend недоступен»
// и следующая загрузка с другими параметрами; связь с архитектурой образа не
// видна ниоткуда.
package cppbackend

import (
	"strings"
	"testing"
)

// TestR135_ArchMismatchNamesDeviceAndFix — главное: текст обязан назвать карту,
// её архитектуру, архитектуры образа и команду/параметр сборки.
func TestR135_ArchMismatchNamesDeviceAndFix(t *testing.T) {
	ok, msg := CheckCUDAArchCompat([]string{"120", "86"}, "Quadro RTX 4000", 7, 5)
	if ok {
		t.Fatal("несовместимость (образ sm_86/sm_120, карта sm_75) не найдена")
	}
	for _, want := range []string{"Quadro RTX 4000", "7.5", "sm_75", "sm_86", "sm_120", "CUDA_ARCH=75"} {
		if !strings.Contains(msg, want) {
			t.Errorf("в тексте нет %q — оператор не поймёт, что делать:\n%s", want, msg)
		}
	}
	if !strings.Contains(msg, "no kernel image") {
		t.Error("нет приметы дефекта (no kernel image) — сообщение не связано с симптомом из лога")
	}
}

// TestR135_ArchMatchIsSilent — совпадающая архитектура не должна ничего писать.
func TestR135_ArchMatchIsSilent(t *testing.T) {
	cases := []struct {
		built []string
		major int
		minor int
	}{
		{[]string{"86"}, 8, 6},
		{[]string{"120", "86"}, 12, 0},
		{[]string{"75", "86", "120"}, 7, 5}, // добавление 75 и есть лечение
	}
	for _, c := range cases {
		if ok, msg := CheckCUDAArchCompat(c.built, "GPU", c.major, c.minor); !ok {
			t.Errorf("%v vs %d.%d: ложная тревога: %s", c.built, c.major, c.minor, msg)
		}
	}
}

// TestR135_UnknownImageIsNotJudged — если образ не сообщил архитектуры или собран
// под все, проверка молчит: утверждать нечего. Ложная ошибка здесь стоила бы
// доверия ко всем остальным предупреждениям.
func TestR135_UnknownImageIsNotJudged(t *testing.T) {
	if ok, msg := CheckCUDAArchCompat(nil, "GPU", 7, 5); !ok {
		t.Errorf("пустой список арок дал ошибку: %s", msg)
	}
	t.Setenv(EnvCUDAArchBuilt, "")
	if got := BuiltCUDAArches(); len(got) != 0 {
		t.Errorf("пустая переменная дала архитектуры: %v", got)
	}
	t.Setenv(EnvCUDAArchBuilt, "all")
	if got := BuiltCUDAArches(); len(got) != 0 {
		t.Errorf("all должен означать «проверять нечего», получено: %v", got)
	}
}

// TestR135_ArchesAreParsed — разбор значения сборки (CUDA_ARCH=86;120).
func TestR135_ArchesAreParsed(t *testing.T) {
	cases := map[string][]string{
		"86;120":   {"120", "86"},
		"75, 86":   {"75", "86"},
		"sm_86":    {"86"},
		"86-120":   {"120", "86"},
		"":         nil,
		"arch_all": nil,
	}
	for raw, want := range cases {
		t.Setenv(EnvCUDAArchBuilt, raw)
		got := BuiltCUDAArches()
		if len(got) != len(want) {
			t.Errorf("%q -> %v, want %v", raw, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%q -> %v, want %v", raw, got, want)
				break
			}
		}
	}
}

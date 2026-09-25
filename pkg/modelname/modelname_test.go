// modelname_test.go — R83 (2026-09-25).
//
// Живой кейс, ради которого пакет появился: клиент (Cline, Ollama-провайдер)
// просит "qwen3.8:latest", на диске "Qwen3.8-27B-UD-Q4_K_M.gguf". Из-за тега
// cppworker шёл открывать несуществующий "models/qwen3.8:latest.gguf" (llama.cpp
// → 500), а балансер считал модель незагруженной и инициировал её загрузку.
package modelname

import (
	"strings"
	"testing"
)

func TestStripTag(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"тег latest", "qwen3.8:latest", "qwen3.8"},
		{"тег квантования", "qwen3.8:q4_k_m", "qwen3.8"},
		{"тег с версией", "gemma-4:q4_k_m", "gemma-4"},
		{"тега нет", "qwen3.8", "qwen3.8"},
		{"пробелы по краям", "  qwen3.8:latest  ", "qwen3.8"},
		{"точка в теге", "model:v1.2.3", "model"},
		// Пути НЕ трогаем: у Windows-пути своё двоеточие.
		{"windows-путь", `C:\models\Qwen3.8.gguf`, `C:\models\Qwen3.8.gguf`},
		{"unix-путь", "models/qwen3.8:latest.gguf", "models/qwen3.8:latest.gguf"},
		{"относительный путь", "./models/x.gguf", "./models/x.gguf"},
		// Неправдоподобный «тег» оставляем как есть — лучше честный 404, чем
		// молча обрезанное настоящее имя.
		{"тег с пробелом", "модель: последняя", "модель: последняя"},
		{"двоеточие в конце", "model:", "model:"},
		{"только двоеточие", ":", ":"},
		{"длинный хвост", "model:" + strings.Repeat("x", MaxTagLen+1), "model:" + strings.Repeat("x", MaxTagLen+1)},
		{"пустая строка", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := StripTag(tc.in); got != tc.want {
				t.Errorf("StripTag(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestVariants(t *testing.T) {
	got := Variants("qwen3.8:latest")
	// Первым идёт ТОЧНЫЙ вариант: нормализация не должна перебивать точное имя.
	if len(got) == 0 || got[0] != "qwen3.8:latest" {
		t.Fatalf("первым должен идти точный вариант, got %v", got)
	}
	for _, want := range []string{"qwen3.8:latest", "qwen3.8", "qwen3.8.gguf"} {
		if !contains(got, want) {
			t.Errorf("нет варианта %q в %v", want, got)
		}
	}
	// Имя с расширением тоже должно давать форму без него — иначе
	// "qwen3.8.gguf" не найдёт "Qwen3.8-27B-UD-Q4_K_M.gguf" по вхождению.
	gotExt := Variants("qwen3.8.gguf")
	for _, want := range []string{"qwen3.8.gguf", "qwen3.8"} {
		if !contains(gotExt, want) {
			t.Errorf("для имени с расширением нет варианта %q в %v", want, gotExt)
		}
	}
	// Дубликатов быть не должно: список идёт в цикл поиска по файлам.
	seen := map[string]bool{}
	for _, v := range got {
		if seen[v] {
			t.Errorf("дубликат варианта %q", v)
		}
		if strings.TrimSpace(v) == "" {
			t.Errorf("пустой вариант в %v", got)
		}
		seen[v] = true
	}
	if Variants("   ") != nil {
		t.Error("пустое имя не должно давать вариантов")
	}
}

// TestCanonical — каноническая форма: без тега, расширения и регистра.
func TestCanonical(t *testing.T) {
	cases := []struct{ in, want string }{
		{"qwen3.8:latest", "qwen3.8"},
		{"Qwen3.8.Q4_K_M.GGUF", "qwen3.8.q4_k_m"},
		{"  Gemma-4  ", "gemma-4"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := Canonical(tc.in); got != tc.want {
			t.Errorf("Canonical(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestMatches — то, что использует балансер. Ключевой случай: запрошенное имя
// с тегом должно сопоставляться с реально загруженным файлом, иначе балансер
// инициирует загрузку несуществующей модели (живой баг).
func TestMatches(t *testing.T) {
	cases := []struct {
		name      string
		requested string
		candidate string
		want      bool
	}{
		{"точное совпадение", "gemma-4", "gemma-4", true},
		{"регистр", "GEMMA-4", "gemma-4", true},
		{"тег и реальное имя файла", "qwen3.8:latest", "Qwen3.8-27B-UD-Q4_K_M.gguf", true},
		{"тег и имя без расширения", "qwen3.8:latest", "Qwen3.8-27B-UD-Q4_K_M", true},
		{"расширение", "gemma-4.gguf", "gemma-4", true},
		{"разные модели", "qwen3.8:latest", "gemma-4-E4B-it-Q4_K_M.gguf", false},
		{"соседняя версия", "qwen3.5:latest", "Qwen3.8-27B-UD-Q4_K_M.gguf", false},
		// Защита от «подошло почти ко всему» на коротких именах.
		{"слишком короткое имя", "a", "gemma-4-E4B-it-Q4_K_M.gguf", false},
		{"пустые", "", "gemma-4", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Matches(tc.requested, tc.candidate); got != tc.want {
				t.Errorf("Matches(%q, %q) = %v, want %v", tc.requested, tc.candidate, got, tc.want)
			}
			// Симметричность: порядок аргументов не должен менять ответ.
			if got := Matches(tc.candidate, tc.requested); got != tc.want {
				t.Errorf("Matches(%q, %q) = %v (не симметрично), want %v",
					tc.candidate, tc.requested, got, tc.want)
			}
		})
	}
}

func TestLooksLikePath(t *testing.T) {
	for _, p := range []string{`C:\models\x.gguf`, "models/x.gguf", "a/b", `a\b`, "d:"} {
		if !LooksLikePath(p) {
			t.Errorf("LooksLikePath(%q) = false, want true", p)
		}
	}
	for _, p := range []string{"qwen3.8:latest", "gemma-4", "", "model:v1.2"} {
		if LooksLikePath(p) {
			t.Errorf("LooksLikePath(%q) = true, want false", p)
		}
	}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

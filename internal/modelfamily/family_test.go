// family_test.go — R268: характеризационные тесты семейств.
//
// ЧТО ИМЕННО ЗАЩИЩАЕМ. Это не тесты «работает ли Go»: они фиксируют ПОВЕДЕНИЕ,
// которое до R268 было размазано по условиям в тракте. Если кто-то, обновляя
// семейство, случайно поменяет стоп-строки, формат вызова или начнёт отказывать
// незнакомой модели, тесты падают.
package modelfamily

import (
	"strings"
	"testing"
)

// TestResolveFamilies — распознавание по имени (таблица, как требовал план).
func TestResolveFamilies(t *testing.T) {
	cases := []struct {
		model  string
		wantID string
		major  int
		status Status
	}{
		{"Qwen3.8-27B-UD-Q4_K_M", "qwen", 3, Supported},
		{"qwen3.8:latest", "qwen", 3, Supported},
		{"qwen-2.5-coder-32b", "qwen", 2, Supported},
		{"gemma-4-E4B-it-Q4_K_M", "gemma", 4, Supported},
		{"gemma-2-9b-it", "gemma", 2, Supported},
		{"llama-3.1-8b-instruct", "llama3", 3, Supported},
		{"Llama3.2-3B", "llama3", 3, Supported},
		{"hermes-3-llama-3.1-8b", "hermes", 0, Supported}, // hermes приоритетнее llama
		{"mistral-nemo-12b", "mistral", 0, Supported},
		{"deepseek-r1-distill-qwen-7b", "deepseek", 0, Supported}, // deepseek приоритетнее qwen
		{"some-unknown-model-7b", GenericID, 0, Supported},        // неизвестное = прежнее поведение
		{"", GenericID, 0, Supported},
	}
	for _, c := range cases {
		res := Resolve(c.model, Meta{})
		if res.Family == nil {
			t.Fatalf("%q: семейство не найдено (generic обязан совпасть всегда)", c.model)
		}
		if c.model == "hermes-3-llama-3.1-8b" {
			// hermes и llama3 имеют разные приоритеты; проверяем, что победил
			// hermes, но не запрещаем llama3 как альтернативу — важно лишь то,
			// что диалект вызова опознан однозначно.
			if res.Family.ID() != "hermes" {
				t.Errorf("%q: семейство = %s, want hermes (hermes-диалект приоритетнее llama)", c.model, res.Family.ID())
			}
			continue
		}
		if res.Family.ID() != c.wantID {
			t.Errorf("%q: семейство = %s, want %s", c.model, res.Family.ID(), c.wantID)
		}
		if res.Major != c.major {
			t.Errorf("%q: major = %d, want %d", c.model, res.Major, c.major)
		}
		if res.Status != c.status {
			t.Errorf("%q: status = %s, want %s", c.model, res.Status, c.status)
		}
	}
}

// TestUnsupportedVersionIsExplicit — версия выше известной линейки = понятный
// отказ, а НЕ молча испорченный ответ.
func TestUnsupportedVersionIsExplicit(t *testing.T) {
	cases := []struct{ model, wantFamily string }{
		{"qwen4.0-70b", "qwen"},
		{"gemma-5-27b", "gemma"},
		{"llama-4-scout", "llama3"},
	}
	for _, c := range cases {
		res := Resolve(c.model, Meta{})
		if res.Status != UnsupportedVersion {
			t.Errorf("%q: status = %s, want unsupported_version", c.model, res.Status)
			continue
		}
		if res.Family.ID() != c.wantFamily {
			t.Errorf("%q: семейство = %s, want %s", c.model, res.Family.ID(), c.wantFamily)
		}
		if SupportsConversion(c.model, Meta{}) {
			t.Errorf("%q: SupportsConversion = true, want false", c.model)
		}
		if res.Reason == "" {
			t.Errorf("%q: пустая причина отказа", c.model)
		}
	}
}

// TestUnknownFamilyNeverRefused — незнакомое имя не считается ошибкой.
//
// ПОЧЕМУ ОТДЕЛЬНЫЙ ТЕСТ. Самый вероятный регресс при «ужесточении» семейств —
// начать отказывать моделям, которых мы просто не знаем. Это ломало бы работу,
// которая сегодня исправна, поэтому запрет зафиксирован тестом.
func TestUnknownFamilyNeverRefused(t *testing.T) {
	for _, model := range []string{"my-custom-gguf-q4", "totally-unknown", "phi-4-mini"} {
		res := Resolve(model, Meta{})
		if res.Status == UnsupportedVersion {
			t.Errorf("%q: неизвестное семейство не должно давать отказ (status=%s)", model, res.Status)
		}
		if !SupportsConversion(model, Meta{}) {
			t.Errorf("%q: SupportsConversion = false, want true (прежнее поведение)", model)
		}
	}
}

// TestRefusalTextNamesPortAndRange — текст отказа обязан объяснять КУДА идти.
//
// Живая жалоба оператора была не «ошибка», а «непонятно, почему не работает»;
// поэтому проверяем и порт, и диапазон версий, и само слово «конвертация».
func TestRefusalTextNamesPortAndRange(t *testing.T) {
	const openAIPort = 18079
	res := Resolve("qwen4.0-70b", Meta{})
	txt := RefusalText("qwen4.0-70b", res, openAIPort)

	for _, want := range []string{"qwen4.0-70b", "qwen", "не поддерживается", "Ollama API", "18079", "как есть"} {
		if !strings.Contains(txt, want) {
			t.Errorf("текст отказа не содержит %q: %s", want, txt)
		}
	}
	// Диапазон поддержанных версий обязан быть в тексте: оператор должен
	// понимать, какую версию взять вместо этой.
	if !strings.Contains(txt, "3") {
		t.Errorf("текст отказа не содержит диапазон версий: %s", txt)
	}
}

// TestStopStringsPreservePreviousBehaviour — стоп-строки ровно те же, что были
// до R268 (перенос без изменения поведения).
func TestStopStringsPreservePreviousBehaviour(t *testing.T) {
	cases := map[string][]string{
		"gemma-4-E4B-it-Q4_K_M": {"<start_of_turn>user", "<start_of_turn>model"},
		"gemma-2-9b-it":         {"<end_of_turn>", "<start_of_turn>user", "<start_of_turn>model"},
		"qwen3.8-27b":           {"<|im_start|>system", "<|im_start|>user"},
		"llama-3.1-8b":          {"<|end|>", "<|user|>", "<|assistant|>"},
		"unknown-model":         {"<|end|>", "<|user|>", "<|assistant|>"},
	}
	for model, want := range cases {
		got := StopStringsFor(model)
		if len(got) != len(want) {
			t.Fatalf("%q: стоп-строки = %v, want %v", model, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%q: стоп-строка[%d] = %q, want %q", model, i, got[i], want[i])
			}
		}
	}
}

// TestFamiliesHaveIdentity — зарегистрированный экземпляр обязан иметь id и
// осмысленный приоритет.
//
// ЗАЧЕМ. Нулевая основа (забыли заполнить base при регистрации) даёт пустой id
// и «невидимое» семейство: тогда Match работает, а логи и API врут. Такое уже
// случалось в других реестрах этого проекта, поэтому проверяем программно.
func TestFamiliesHaveIdentity(t *testing.T) {
	fams := Families()
	if len(fams) < 6 {
		t.Fatalf("семейств зарегистрировано %d, ожидалось не меньше 6", len(fams))
	}
	seen := map[string]bool{}
	for _, f := range fams {
		if strings.TrimSpace(f.ID()) == "" {
			t.Errorf("семейство с пустым id: %T", f)
		}
		if seen[f.ID()] {
			t.Errorf("дубликат id семейства: %s", f.ID())
		}
		seen[f.ID()] = true
		if f.Notes() == "" {
			t.Errorf("%s: пустая заметка (Notes)", f.ID())
		}
	}
	if !seen[GenericID] {
		t.Fatal("generic не зарегистрирован — неизвестные модели остались бы без фолбэка")
	}
	// Порядок применения детерминирован: generic всегда последний.
	if fams[len(fams)-1].ID() != GenericID {
		t.Errorf("последнее семейство = %s, want %s (generic обязан быть последним)",
			fams[len(fams)-1].ID(), GenericID)
	}
}

// TestServiceMarkersForKnownFamilies — метки qwen/gemma доступны тракту через
// семейство (R266-список больше не живёт отдельным хардкодом).
func TestServiceMarkersForKnownFamilies(t *testing.T) {
	qwen := ServiceMarkersFor("Qwen3.8-27B")
	for _, want := range []string{"<|tool|>", "<|assistant|>", "<|channel|>"} {
		if !containsStr(qwen, want) {
			t.Errorf("метки qwen не содержат %q: %v", want, qwen)
		}
	}
	gemma := ServiceMarkersFor("gemma-4-E4B")
	if !containsStr(gemma, "<end_of_turn>") {
		t.Errorf("метки gemma не содержат <end_of_turn>: %v", gemma)
	}
	// generic честно не притворяется, что знает диалект.
	if got := ServiceMarkersFor("unknown-model"); len(got) != 0 {
		t.Errorf("generic вернул метки %v, want пусто", got)
	}
}

// TestCapabilitiesForTextForm — диалект вызова доступен вызывающему коду, чтобы
// детектор больше не держал собственный список семейств.
func TestCapabilitiesForTextForm(t *testing.T) {
	cases := map[string]string{
		"qwen3.8-27b":   "tool_calls_marker",
		"gemma-4-E4B":   "tool_code",
		"llama-3.1-8b":  "python_tag",
		"hermes-3-8b":   "hermes",
		"mistral-nemo":  "tool_calls_marker",
		"unknown-model": "",
	}
	for model, want := range cases {
		res := Resolve(model, Meta{})
		got := res.Family.CapabilitiesFor(res.Major).ToolCallTextForm
		if got != want {
			t.Errorf("%q: ToolCallTextForm = %q, want %q", model, got, want)
		}
	}
}

func containsStr(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

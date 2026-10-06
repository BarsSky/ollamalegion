package balancer

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestDetector_Qwen3ToolCallsPrefix — ЖИВОЙ ДЕФЕКТ (2026-10-06): Qwen3 через
// Open WebUI вернул вызов инструмента ТЕКСТОМ в content:
//
//	[TOOL_CALLS][{"id":"call_generate_image","type":"function","function":{"name":"generate_image","arguments":"{...}"}}]
//
// Клиент получил это как обычный текст, картинка не нарисовалась. Тест фиксирует,
// распознаёт ли это существующий детектор форм и извлекает ли имя инструмента.
func TestDetector_Qwen3FormatNestedFunction(t *testing.T) {
	args := `{\"prompt\":\"Сказочный лес с светящимися деревьями\",\"model\":\"stable-diffusion.cpp\",\"negative_prompt\":\"чёрные тени, ужасы\",\"steps\":30,\"width\":768,\"height\":512,\"seed\":42}`
	content := `[TOOL_CALLS][{"id":"call_generate_image","type":"function","function":{"name":"generate_image","arguments":"` + args + `"}}]`

	calls, remaining, found := detectAndExtractToolCallsFromContent(content)
	t.Logf("found=%v remaining=%q calls=%d", found, remaining, len(calls))
	for i, c := range calls {
		entry, _ := c.(map[string]interface{})
		fn, _ := entry["function"].(map[string]interface{})
		name, _ := fn["name"].(string)
		rawArgs, _ := json.Marshal(fn["arguments"])
		t.Logf("  call[%d]: id=%v name=%q args=%s", i, entry["id"], name, rawArgs)
	}

	// Через тот же путь, которым пользуется цикл: message с content.
	msg := map[string]interface{}{"role": "assistant", "content": content}
	imgCalls := extractImageToolCalls(msg)
	t.Logf("extractImageToolCalls -> %d вызов(ов)", len(imgCalls))
	for _, c := range imgCalls {
		t.Logf("  name=%q args=%q", c.Name, c.Arguments)
	}
	if len(imgCalls) == 0 {
		t.Fatalf("вызов модели не распознан — именно это увидел клиент как текст")
	}
	if imgCalls[0].Name != "generate_image" {
		t.Errorf("имя инструмента = %q", imgCalls[0].Name)
	}
	args2, err := parseImageToolArgs(imgCalls[0].Arguments)
	if err != nil {
		t.Fatalf("аргументы не разобраны: %v (%q)", err, imgCalls[0].Arguments)
	}
	if args2.Prompt == "" || args2.Width != 768 || args2.Height != 512 || args2.Steps != 30 {
		t.Errorf("аргументы разобраны неверно: %+v", args2)
	}
	if args2.Model != "stable-diffusion.cpp" {
		t.Errorf("model=%q", args2.Model)
	}
}

// TestDetector_Qwen3EqualsForm — вторая форма со стенда (2026-10-06): маркер
// [TOOL_CALLS] идёт со знаком «=» ПЕРЕД массивом:
//
//	[TOOL_CALLS]=[{"id":"call_generate_image","type":"function","function":{"name":"generate_image","arguments":"{...}"}}]
//
// Проверяем ровно это написание (пробелы вокруг «=» и без них, вложенные кавычки
// в arguments).
func TestDetector_Qwen3EqualsForm(t *testing.T) {
	args := `{\"prompt\":\"Лес в стиле живописи Кандинского\",\"model\":\"stable-diffusion.cpp\",\"negative_prompt\":\"реалистичные детали, тёмный лес\",\"steps\":30,\"width\":768,\"height\":512,\"seed\":43}`
	forms := []string{
		`[TOOL_CALLS]=[{"id":"call_generate_image","type":"function","function":{"name":"generate_image","arguments":"` + args + `"}}]`,
		`[TOOL_CALLS] = [{"id":"call_generate_image","type":"function","function":{"name":"generate_image","arguments":"` + args + `"}}]`,
		`[TOOL_CALLS][{"id":"call_generate_image","type":"function","function":{"name":"generate_image","arguments":"` + args + `"}}]`,
	}
	for i, content := range forms {
		calls, remaining, found := detectAndExtractToolCallsFromContent(content)
		t.Logf("форма %d: found=%v remaining=%q calls=%d", i, found, remaining, len(calls))
		if !found || len(calls) == 0 {
			t.Errorf("форма %d не распознана: %s", i, content)
			continue
		}
		entry, _ := calls[0].(map[string]interface{})
		fn, _ := entry["function"].(map[string]interface{})
		name, _ := fn["name"].(string)
		rawArgs, _ := json.Marshal(fn["arguments"])
		t.Logf("  name=%q args=%s", name, rawArgs)
		if name != "generate_image" {
			t.Errorf("форма %d: имя = %q", i, name)
		}
	}
}

// TestDetector_Qwen3LongRussianPrompt — ТОЧНЫЙ текст со скриншота оператора: тот
// же формат, но с длинным русским промптом, запятыми и тире внутри экранированных
// аргументов. Проверяем и распознавание вызова, и разбор аргументов до конца —
// ловушка в том, что длинный текст легко «теряется» на любом промежуточном шаге.
func TestDetector_Qwen3LongRussianPrompt(t *testing.T) {
	content := `[TOOL_CALLS]=[{"id":"call_generate_image","type":"function","function":{"name":"generate_image","arguments":"{\"prompt\":\"Лес в стиле живописи Кандинского: абстрактные формы деревьев, размытые линии и геометрические фигуры, изображающие лес. Цветовая палитра — яркие, насыщенные оттенки синего, оранжевого, зелёного и белого, смешанные в хаотичных, динамичных композициях. В центре — живое изображение светящейся тени дерева, как будто он пульсирует. Маленькие формы животных (например, олень, лиса) представлены в виде абстрактных форм и линий. Атмосфера — душевная, мистическая, с элементами волшебства. Стиль: живопись, абстракция, похоже на работы В. Кандинского\",\"model\":\"stable-diffusion.cpp\",\"negative_prompt\":\"реалистичные детали, тёмный лес, реальная земля, тяжёлые тени, скучно, монотонно\",\"steps\":30,\"width\":768,\"height\":512,\"seed\":43}"}}]`

	msg := map[string]interface{}{"role": "assistant", "content": content}
	imgCalls := extractImageToolCalls(msg)
	if len(imgCalls) == 0 {
		t.Fatalf("вызов со скриншота не распознан")
	}
	if imgCalls[0].Name != "generate_image" {
		t.Fatalf("имя = %q", imgCalls[0].Name)
	}
	args, err := parseImageToolArgs(imgCalls[0].Arguments)
	if err != nil {
		t.Fatalf("аргументы не разобраны: %v", err)
	}
	if !strings.Contains(args.Prompt, "Кандинского") {
		t.Errorf("промпт потерян: %q", args.Prompt)
	}
	if !strings.Contains(args.NegativePrompt, "монотонно") {
		t.Errorf("negative_prompt потерян: %q", args.NegativePrompt)
	}
	if args.Width != 768 || args.Height != 512 || args.Steps != 30 {
		t.Errorf("параметры разобраны неверно: %+v", args)
	}
	if args.Model != "stable-diffusion.cpp" {
		t.Errorf("model=%q", args.Model)
	}
}

// schema_test.go — схема инструмента generate_image.
//
// ЗАЧЕМ ТЕСТ. Схему читают ДВА потребителя с разными ожиданиями: агент (через
// документ контракта /api/v1/image/contract) и сам балансер (инъекция в
// /v1/chat/completions). Если границы/имена полей разъедутся с тем, что реально
// принимает воркер, модель будет формировать вызовы, которые отклоняются
// валидацией — снаружи это выглядит как «инструмент не работает».
//
// Границы сверяются с internal/sdbackend/normalize.go и границами, которые
// применяет движок sd.cpp: 64…4096, кратно 64; шаги 1…100.
package imagetool

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBuild_Shape(t *testing.T) {
	tool := Build(Defaults())

	if tool.Name != "generate_image" {
		t.Fatalf("имя инструмента %q, want %q (его ждут агенты)", tool.Name, Name)
	}
	if tool.Parameters.Type != "object" {
		t.Errorf("parameters.type=%q, want object", tool.Parameters.Type)
	}
	if len(tool.Parameters.Required) != 1 || tool.Parameters.Required[0] != "prompt" {
		t.Errorf("required=%v, want [prompt]", tool.Parameters.Required)
	}
	if tool.Parameters.AdditionalProperties {
		t.Error("additionalProperties должен быть false: лишние поля движок не принимает")
	}
	for _, field := range []string{"prompt", "negative_prompt", "width", "height", "steps", "cfg", "seed", "model"} {
		if _, ok := tool.Parameters.Properties[field]; !ok {
			t.Errorf("нет поля %q в схеме", field)
		}
	}
}

func TestBuild_EngineBounds(t *testing.T) {
	tool := Build(Defaults())
	w := tool.Parameters.Properties["width"]
	if w.Minimum != DefaultMinSide || w.Maximum != DefaultMaxSide || w.MultipleOf != DefaultSizeMultiple {
		t.Errorf("width bounds = %v…%v multiple %v, want %d…%d multiple %d",
			w.Minimum, w.Maximum, w.MultipleOf, DefaultMinSide, DefaultMaxSide, DefaultSizeMultiple)
	}
	s := tool.Parameters.Properties["steps"]
	if s.Minimum != DefaultMinSteps || s.Maximum != DefaultMaxSteps {
		t.Errorf("steps bounds = %v…%v, want %d…%d", s.Minimum, s.Maximum, DefaultMinSteps, DefaultMaxSteps)
	}
	if got := tool.Parameters.Properties["seed"].Minimum; got != nil {
		t.Errorf("seed.minimum=%v: отрицательный seed у движка легален (означает «случайный»)", got)
	}
}

// Пустой Spec (ни один воркер не ответил) обязан давать ту же валидную схему:
// иначе документ контракта ломается на пустом кластере.
func TestBuild_ZeroSpecUsesDefaults(t *testing.T) {
	tool := Build(Spec{})
	if tool.Parameters.Properties["width"].Maximum != DefaultMaxSide {
		t.Fatalf("нулевой Spec не подставил дефолты: %+v", tool.Parameters.Properties["width"])
	}
}

func TestBuild_ModelsEnumOnlyWhenKnown(t *testing.T) {
	tool := Build(Defaults())
	if e := tool.Parameters.Properties["model"].Enum; e != nil {
		t.Errorf("enum моделей при пустом списке = %v, want nil (пустой enum ломает валидацию)", e)
	}
	tool = Build(Spec{Models: []string{"sd15-q4", "flux-schnell-q4"}})
	got := tool.Parameters.Properties["model"].Enum
	if len(got) != 2 || got[0] != "sd15-q4" {
		t.Fatalf("enum моделей = %v, want список из Spec", got)
	}
	// Spec не должен разделять слайс с вызывающим: иначе правка ответа мутирует
	// снимок состояния кластера.
	got[0] = "mutated"
	if Build(Spec{Models: []string{"sd15-q4"}}).Parameters.Properties["model"].Enum[0] == "mutated" {
		t.Error("Build не копирует список моделей")
	}
}

func TestBuild_SyncDescriptionTalksAboutMarkdown(t *testing.T) {
	syncTool := Build(Spec{Sync: true})
	if !strings.Contains(syncTool.Description, "markdown") {
		t.Errorf("sync-описание должно говорить про ссылку/markdown: %s", syncTool.Description)
	}
	asyncTool := Build(Defaults())
	if !strings.Contains(asyncTool.Description, "base64") {
		t.Errorf("async-описание (инструмент исполняет клиент) должно говорить про base64: %s", asyncTool.Description)
	}
}

func TestOpenAIFunction_WireShape(t *testing.T) {
	raw, err := json.Marshal(Build(Defaults()).OpenAIFunction())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var doc map[string]interface{}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if doc["type"] != "function" {
		t.Errorf("type=%v, want function", doc["type"])
	}
	fn, ok := doc["function"].(map[string]interface{})
	if !ok {
		t.Fatalf("нет объекта function: %s", raw)
	}
	if fn["name"] != Name {
		t.Errorf("function.name=%v, want %s", fn["name"], Name)
	}
	if _, ok := fn["parameters"].(map[string]interface{}); !ok {
		t.Errorf("нет parameters в function: %s", raw)
	}
}

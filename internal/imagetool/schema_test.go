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

// --- R85: автозагрузка и каталог моделей ------------------------------------

// При выключенной автозагрузке схема обязана остаться ПРЕЖНЕЙ байт-в-байт:
// на неё опираются уже развёрнутые клиенты, а обещать «поднимем модель сами»
// при ALLOW_LOAD=off было бы ложью в описании инструмента.
func TestBuild_AllowLoadOffKeepsLegacySchema(t *testing.T) {
	legacy := Build(Spec{Sync: true})
	withFlagOff := Build(Spec{Sync: true, AllowLoad: false})

	legacyJSON, _ := json.Marshal(legacy)
	offJSON, _ := json.Marshal(withFlagOff)
	if string(legacyJSON) != string(offJSON) {
		t.Fatalf("AllowLoad=false изменил схему:\n legacy=%s\n   off=%s", legacyJSON, offJSON)
	}
	if strings.Contains(withFlagOff.Description, ListName) {
		t.Error("при ALLOW_LOAD=off описание не должно звать list_image_models: каталогом модель нельзя поднять")
	}
	if strings.Contains(withFlagOff.Parameters.Properties["model"].Description, ListName) {
		t.Error("при ALLOW_LOAD=off описание параметра model не должно ссылаться на list_image_models")
	}
}

// При включённой автозагрузке модель обязана узнать ДВА факта: чем
// отличаются модели (каталог) и что загрузка — это пауза, о которой надо
// предупредить пользователя.
func TestBuild_AllowLoadOnExplainsCatalogAndLoadPause(t *testing.T) {
	tool := Build(Spec{Sync: true, AllowLoad: true, Models: []string{"sd15-q8-0", "flux-schnell-q3-k"}})
	if !strings.Contains(tool.Description, ListName) {
		t.Errorf("описание должно звать %s: %s", ListName, tool.Description)
	}
	if !strings.Contains(tool.Description, "загрузку") {
		t.Errorf("описание должно предупреждать о времени загрузки: %s", tool.Description)
	}
	model := tool.Parameters.Properties["model"]
	if !strings.Contains(model.Description, ListName) {
		t.Errorf("описание параметра model должно ссылаться на %s: %s", ListName, model.Description)
	}
	if len(model.Enum) != 2 {
		t.Errorf("enum моделей = %v, want 2 значения", model.Enum)
	}
}

func TestBuildList_Shape(t *testing.T) {
	tool := BuildList(ListSpec{})
	if tool.Name != ListName {
		t.Fatalf("имя инструмента %q, want %q", tool.Name, ListName)
	}
	if len(tool.Parameters.Required) != 0 {
		t.Errorf("required=%v, want пусто: каталог отдаётся без параметров", tool.Parameters.Required)
	}
	if tool.Parameters.AdditionalProperties {
		t.Error("additionalProperties должен быть false")
	}
	if _, ok := tool.Parameters.Properties["family"]; !ok {
		t.Error("нет необязательного параметра family")
	}
	if e := tool.Parameters.Properties["family"].Enum; e != nil {
		t.Errorf("enum семейств при пустом списке = %v, want nil", e)
	}
	withFam := BuildList(ListSpec{Families: []string{"sd15", "flux"}})
	got := withFam.Parameters.Properties["family"].Enum
	if len(got) != 2 || got[0] != "sd15" {
		t.Fatalf("enum семейств = %v, want [sd15 flux]", got)
	}
	got[0] = "mutated"
	if BuildList(ListSpec{Families: []string{"sd15"}}).Parameters.Properties["family"].Enum[0] == "mutated" {
		t.Error("BuildList не копирует список семейств")
	}
	if !strings.Contains(tool.Description, "GPU не занимает") {
		t.Errorf("описание должно объяснять, что каталог не тратит GPU: %s", tool.Description)
	}
}

func TestOpenAITools_ListOnlyWithAllowLoad(t *testing.T) {
	// Имена берём из фактической проводной формы (marshal → JSON), а не из
	// структуры: так тест проверяет ровно то, что уйдёт в запрос к модели.
	names := func(tools []map[string]interface{}) []string {
		out := make([]string, 0, len(tools))
		for _, tool := range tools {
			raw, err := json.Marshal(tool)
			if err != nil {
				t.Fatalf("marshal tool: %v", err)
			}
			var doc map[string]interface{}
			if err := json.Unmarshal(raw, &doc); err != nil {
				t.Fatalf("unmarshal tool: %v", err)
			}
			fn, _ := doc["function"].(map[string]interface{})
			name, _ := fn["name"].(string)
			out = append(out, name)
		}
		return out
	}

	off := OpenAITools(Spec{Sync: true})
	if len(off) != 1 || names(off)[0] != Name {
		t.Fatalf("ALLOW_LOAD=off: инструменты = %v, want только %s", names(off), Name)
	}
	on := OpenAITools(Spec{Sync: true, AllowLoad: true})
	if len(on) != 2 || names(on)[0] != Name || names(on)[1] != ListName {
		t.Fatalf("ALLOW_LOAD=on: инструменты = %v, want [%s %s]", names(on), Name, ListName)
	}
}

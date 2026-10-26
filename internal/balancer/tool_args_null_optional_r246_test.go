// tool_args_null_optional_r246_test.go — R246 (2026-10-26): null у необязательного
// поля аргументов не должен доезжать до клиента.
//
// ЖИВОЙ ДЕФЕКТ (полная матрица инструментов DSH через балансер → gemma-4):
// модель вызвала ralph с аргументами {"objective":"…","maxRounds":null} при
// схеме `maxRounds: number`. Клиент (typebox) отвечает ошибкой валидации
// «expected number, received null», агент повторяет вызов и не может применить
// инструмент. При этом null в необязательном поле означает «поле не задано» —
// это и есть семантика отсутствующего поля в OpenAI tool args.
package balancer

import (
	"encoding/json"
	"testing"
)

// TestR246_NullOptionalArgDropped — null у необязательного поля удаляется.
func TestR246_NullOptionalArgDropped(t *testing.T) {
	schemas := map[string]*toolParamSchema{
		"ralph": {
			Type: "object",
			Properties: map[string]*toolParamSchema{
				"objective": {Type: "string"},
				"maxRounds": {Type: "number"},
			},
			Required: []string{"objective"},
		},
	}
	fixed, changed := repairToolArguments(`{"objective":"probe","maxRounds":null}`, schemas["ralph"])
	if !changed {
		t.Fatal("null у необязательного поля не исправлен")
	}
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(fixed), &args); err != nil {
		t.Fatalf("аргументы не JSON: %v (%q)", err, fixed)
	}
	if _, present := args["maxRounds"]; present {
		t.Errorf("maxRounds остался в аргументах: %q", fixed)
	}
	if got, _ := args["objective"].(string); got != "probe" {
		t.Errorf("objective потерян: %q", fixed)
	}
}

// TestR246_NullRequiredArgKept — null в ОБЯЗАТЕЛЬНОМ поле не трогаем: это
// честная ошибка клиента, подставлять выдуманное значение нельзя.
func TestR246_NullRequiredArgKept(t *testing.T) {
	schema := &toolParamSchema{
		Type: "object",
		Properties: map[string]*toolParamSchema{
			"file_path": {Type: "string"},
			"limit":     {Type: "number"},
		},
		Required: []string{"file_path"},
	}
	fixed, _ := repairToolArguments(`{"file_path":null,"limit":null}`, schema)
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(fixed), &args); err != nil {
		t.Fatalf("аргументы не JSON: %v (%q)", err, fixed)
	}
	if _, present := args["file_path"]; !present {
		t.Errorf("обязательное поле с null удалено (нельзя): %q", fixed)
	}
	if _, present := args["limit"]; present {
		t.Errorf("необязательный limit с null остался: %q", fixed)
	}
}

// TestR246_PluralAliasRenamed — обязательное поле в множественном написании
// переименовывается, массив из одного элемента становится строкой.
//
// ЖИВОЙ ДЕФЕКТ (полная матрица, OpenWebUI-набор через балансер → gemma-4):
// схема `web_search` требует `query: string`, модель отдала
// {"queries":["ollama"]} — клиент отвечал «нет обязательного поля query».
func TestR246_PluralAliasRenamed(t *testing.T) {
	schema := &toolParamSchema{
		Type: "object",
		Properties: map[string]*toolParamSchema{
			"query": {Type: "string"},
		},
		Required: []string{"query"},
	}
	fixed, changed := repairToolArguments(`{"queries":["ollama"]}`, schema)
	if !changed {
		t.Fatal("множественное написание обязательного поля не исправлено")
	}
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(fixed), &args); err != nil {
		t.Fatalf("аргументы не JSON: %v (%q)", err, fixed)
	}
	if got, _ := args["query"].(string); got != "ollama" {
		t.Errorf("query = %#v, ожидалась строка ollama (%q)", args["query"], fixed)
	}
	if _, present := args["queries"]; present {
		t.Errorf("старое поле queries осталось: %q", fixed)
	}
}

// TestR246_PluralAliasDoesNotEatDeclaredField — если `queries` объявлено
// отдельным полем схемы, переименование не выполняется.
func TestR246_PluralAliasDoesNotEatDeclaredField(t *testing.T) {
	schema := &toolParamSchema{
		Type: "object",
		Properties: map[string]*toolParamSchema{
			"query":   {Type: "string"},
			"queries": {Type: "array"},
		},
		Required: []string{"query"},
	}
	fixed, _ := repairToolArguments(`{"queries":["a","b"]}`, schema)
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(fixed), &args); err != nil {
		t.Fatalf("аргументы не JSON: %v (%q)", err, fixed)
	}
	if _, present := args["queries"]; !present {
		t.Errorf("объявленное отдельным полем queries съедено алиасом: %q", fixed)
	}
}

// TestR246_NullInNestedObjectDropped — вложенные объекты тоже чистятся.
func TestR246_NullInNestedObjectDropped(t *testing.T) {
	schema := &toolParamSchema{
		Type: "object",
		Properties: map[string]*toolParamSchema{
			"meta": {
				Type: "object",
				Properties: map[string]*toolParamSchema{
					"name":    {Type: "string"},
					"version": {Type: "string"},
				},
				Required: []string{"name"},
			},
		},
		Required: []string{"meta"},
	}
	fixed, changed := repairToolArguments(`{"meta":{"name":"probe","version":null}}`, schema)
	if !changed {
		t.Fatal("null во вложенном объекте не исправлен")
	}
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(fixed), &args); err != nil {
		t.Fatalf("аргументы не JSON: %v (%q)", err, fixed)
	}
	meta, _ := args["meta"].(map[string]interface{})
	if _, present := meta["version"]; present {
		t.Errorf("version остался: %q", fixed)
	}
	if got, _ := meta["name"].(string); got != "probe" {
		t.Errorf("name потерян: %q", fixed)
	}
}

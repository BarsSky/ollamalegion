// family_llama3.go — R268: семейство Llama 3.x (включая 3.1/3.2).
//
// ЧТО ЗНАЕМ: tool-call в формате `<|python_tag|>{"name":...,"parameters":{...}}`
// (диалект Llama-3), нативные tool_calls появляются только в новых шаблонах,
// поэтому детектор текстового вызова остаётся включённым. Стоп-строки — общий
// набор «по умолчанию»: Llama-3 в прежнем коде попадала именно в ветку default,
// поведение сохранено.
package modelfamily

import "strings"

func init() { Register(llamaFamily{llamaBase}) }

type llamaFamily struct{ base }

func (f llamaFamily) Match(modelName string, _ Meta) bool {
	// "llama" покрывает llama-3, llama3.1, llama-2, codellama.
	return strings.Contains(strings.ToLower(modelName), "llama")
}

func (f llamaFamily) CapabilitiesFor(major int) Capabilities {
	return Capabilities{
		NativeTools:      false,
		ReasoningChannel: false,
		ToolCallTextForm: "python_tag",
		StopStrings:      defaultStopStrings(),
		ServiceMarkers: []string{
			"<|begin_of_text|>", "<|end_of_text|>",
			"<|eot_id|>", "<|start_header_id|>", "<|end_header_id|>",
			"<|python_tag|>",
		},
	}
}

var llamaBase = base{
	id:       "llama3",
	priority: 60,
	versions: VersionRange{MinMajor: 0, MaxMajor: 3},
	tokens:   []string{"llama"},
	notes:    "tool-call в формате <|python_tag|>; стоп-строки — общий набор",
}

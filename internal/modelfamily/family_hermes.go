// family_hermes.go — R268: Hermes/Qwen2.5-совместимый диалект
// `<tool_call>{"name":…,"arguments":{…}}</tool_call>`.
//
// ЗАЧЕМ ОТДЕЛЬНЫМ СЕМЕЙСТВОМ, если похоже на qwen: имя модели "hermes-*" не
// содержит "qwen", но формат вызова — тот же hermes-диалект. Раньше это знание
// жило только в детекторе; теперь оно в одном файле и видно в логах по id
// семейства.
package modelfamily

import "strings"

func init() { Register(hermesFamily{hermesBase}) }

type hermesFamily struct{ base }

func (f hermesFamily) Match(modelName string, _ Meta) bool {
	return strings.Contains(strings.ToLower(modelName), "hermes")
}

func (f hermesFamily) CapabilitiesFor(major int) Capabilities {
	return Capabilities{
		NativeTools:      false,
		ReasoningChannel: false,
		ToolCallTextForm: "hermes",
		StopStrings:      defaultStopStrings(),
		ServiceMarkers:   []string{"<tool_call>", "</tool_call>"},
	}
}

var hermesBase = base{
	id:       "hermes",
	priority: 80,
	versions: VersionRange{}, // линейка версий нам неизвестна → не отказываем
	tokens:   []string{"hermes"},
	notes:    "tool-call в формате <tool_call>{…}</tool_call>; стоп-строки — общий набор",
}

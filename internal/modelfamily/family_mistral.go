// family_mistral.go — R268: Mistral / Mixtral / Nemo, диалект `[TOOL_CALLS][…]`.
package modelfamily

import "strings"

func init() { Register(mistralFamily{mistralBase}) }

type mistralFamily struct{ base }

func (f mistralFamily) Match(modelName string, _ Meta) bool {
	lower := strings.ToLower(modelName)
	return containsAny(lower, "mistral", "mixtral", "nemo")
}

func (f mistralFamily) CapabilitiesFor(major int) Capabilities {
	return Capabilities{
		NativeTools:      false,
		ReasoningChannel: false,
		ToolCallTextForm: "tool_calls_marker",
		StopStrings:      defaultStopStrings(),
		ServiceMarkers:   []string{"[TOOL_CALLS]", "[AVAILABLE_TOOLS]"},
	}
}

var mistralBase = base{
	id:       "mistral",
	priority: 75,
	versions: VersionRange{},
	tokens:   []string{"mistral", "mixtral", "nemo"},
	notes:    "tool-call в формате [TOOL_CALLS][…]; стоп-строки — общий набор",
}

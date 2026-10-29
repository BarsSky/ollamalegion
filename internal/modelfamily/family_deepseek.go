// family_deepseek.go — R268: DeepSeek и производные, рассуждающие в тексте
// (`[THINK]`, `</think>`, канальные метки).
//
// ВАЖНОЕ ДЛЯ ОТВЕТА КЛИЕНТУ: у этого семейства размышления приходят ТЕКСТОМ, а
// не отдельным полем, поэтому фильтр обязан отделять их по маркерам — иначе они
// «мешаются с ответом» (та самая живая жалоба Cline).
package modelfamily

import "strings"

func init() { Register(deepseekFamily{deepseekBase}) }

type deepseekFamily struct{ base }

func (f deepseekFamily) Match(modelName string, _ Meta) bool {
	return strings.Contains(strings.ToLower(modelName), "deepseek")
}

func (f deepseekFamily) CapabilitiesFor(major int) Capabilities {
	return Capabilities{
		NativeTools:      false,
		ReasoningChannel: true,
		// Особого маркера вызова у семейства нет: работает общий JSON-детектор.
		ToolCallTextForm: "",
		StopStrings:      defaultStopStrings(),
		ServiceMarkers: []string{
			"[THINK]", "[/THINK]", "</think>", "<|channel|>", "<|message|>",
			"<｜end▁of▁thinking｜>",
		},
	}
}

var deepseekBase = base{
	id:       "deepseek",
	priority: 95,
	versions: VersionRange{},
	tokens:   []string{"deepseek"},
	notes:    "размышления текстом ([THINK]/</think>); вызов — общим JSON-детектором",
}

// family_gemma.go — R268: семейство Gemma (2 и 4).
//
// ГЛАВНОЕ РАЗЛИЧИЕ ВНУТРИ СЕМЕЙСТВА — СТОП-СТРОКИ, и оно по версии:
//   - gemma-4: "<end_of_turn>" в наборе НЕЛЬЗЯ: модель цитирует его в
//     рассуждениях, генерация обрывалась на ~85 токенах (Round 32 #27), поэтому
//     остаются только маркеры НАЧАЛА следующего хода;
//   - gemma-2 и старше: "<end_of_turn>" — настоящий маркер конца хода, он нужен.
//
// Версия — параметр CapabilitiesFor, а не отдельное семейство: файл семейства
// один, обновление gemma-5 будет правкой только этого файла.
package modelfamily

import "strings"

func init() { Register(gemmaFamily{gemmaBase}) }

type gemmaFamily struct{ base }

func (f gemmaFamily) Match(modelName string, _ Meta) bool {
	return strings.Contains(strings.ToLower(modelName), "gemma")
}

func (f gemmaFamily) CapabilitiesFor(major int) Capabilities {
	stops := gemmaOldStopStrings()
	if major >= 4 {
		stops = gemma4StopStrings()
	}
	return Capabilities{
		// Нативных tool_calls у gemma нет: вызов приходит текстом —
		// `[tool_code]`/```tool_code (R246) или JSON в <start_of_turn>model.
		NativeTools:      false,
		ReasoningChannel: true, // </think> в тексте — R231/R243
		ToolCallTextForm: "tool_code",
		StopStrings:      stops,
		ServiceMarkers: []string{
			"<start_of_turn>", "<end_of_turn>", "</think>",
			"<start_of_turn>user", "<start_of_turn>model", "<start_of_turn>assistant",
		},
	}
}

var gemmaBase = base{
	id:       "gemma",
	priority: 90,
	versions: VersionRange{MinMajor: 0, MaxMajor: 4},
	tokens:   []string{"gemma"},
	notes:    "нет нативных tools: вызов текстом ([tool_code] / JSON в <start_of_turn>model); </think> в тексте; стоп-строки зависят от версии",
}

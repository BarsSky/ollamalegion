// family_qwen.go — R268: семейство Qwen (2.x/3.x, включая 3.8).
//
// ЧТО ЗНАЕМ (перенесено из прежних условий, поведение сохранено дословно):
//   - стоп-строки: маркеры НАЧАЛА следующего хода ("<|im_start|>system"/"user").
//     "<|im_end|>"/"<|end|>" УБРАНЫ в R248: модель цитирует их в тексте, а
//     проверка суффиксная — ответ обрывался посреди фразы (живой дефект Cline);
//     остановка не теряется, "<|im_end|>" — EOS-токен этой модели;
//   - tool-call приходит маркером "[TOOL_CALLS]" или каналом "<|tool|>"
//     (qwen3.8/harmonica) — разбирает детектор, семейство лишь сообщает диалект;
//   - reasoning — отдельным каналом "<|channel|>analysis", поэтому канальные
//     метки обязаны вычищаться из видимого текста (R266).
package modelfamily

import "strings"

func init() { Register(qwenFamily{qwenBase}) }

type qwenFamily struct{ base }

func (f qwenFamily) Match(modelName string, _ Meta) bool {
	lower := strings.ToLower(modelName)
	// qwen2/qwen3/qwen3.8/qwq — один ChatML-диалект.
	return containsAny(lower, "qwen", "qwq")
}

func (f qwenFamily) CapabilitiesFor(major int) Capabilities {
	return Capabilities{
		// Нативные tool_calls бывают, но НЕ у всех сборок шаблона: детектор
		// текстовых вызовов остаётся включённым, поэтому NativeTools=false
		// (это подсказка, а не запрет).
		NativeTools:      false,
		ReasoningChannel: true,
		ToolCallTextForm: "tool_calls_marker",
		StopStrings:      []string{"<|im_start|>system", "<|im_start|>user"},
		ServiceMarkers: []string{
			"<|im_start|>", "<|im_end|>", "<|endoftext|>",
			"<|tool|>", "<|tool_call|>", "<|tool_response|>",
			"<|channel|>", "<|message|>", "<|constrain|>",
			"<|assistant|>", "<|user|>", "<|system|>",
			"<｜end▁of▁thinking｜>",
		},
	}
}

// qwenBase — данные семейства. Отдельная переменная, чтобы init регистрировал
// экземпляр с заполненной основой (нулевая основа = пустой id, это ловится
// тестом TestFamiliesHaveIdentity).
var qwenBase = base{
	id:       "qwen",
	priority: 90,
	// 3.8 — текущая верхняя известная линейка. Версия выше (qwen4) означает,
	// что диалект мог измениться: лучше честный отказ с указанием порта, чем
	// молча испорченный ответ.
	versions: VersionRange{MinMajor: 0, MaxMajor: 3},
	tokens:   []string{"qwen", "qwq"},
	notes:    "ChatML-диалект; tool-call маркером [TOOL_CALLS] или каналом <|tool|>; reasoning отдельным каналом",
}

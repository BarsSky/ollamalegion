// tool_calls_repair_tail.go — R638 (2026-11-19): хвостовой ремонт: fixTrailingBraceMisorder.
//
// ЗАЧЕМ ВЫНЕСЕНО. `tool_calls.go` держал 1988 строк: кэш промпта, разбор
// девяти семейств форматов вызова, нормализацию и сборку системного промпта.
// Здесь — один из слоёв; поведение не менялось, код перенесён как есть.
package main

import "strings"

// fixTrailingBraceMisorder — Round 16 code-review fix (2026-07-30): переименована из
// misleadingly-named `stripExtraBracesInStringValues`. Оригинальное имя предполагало
// работу внутри string values, но функция делает ровно одно: переставляет trailing
// `]}` → `}]` (move `}` before `]`). Никаких "string values" внутри не трогает.
//
// Конкретный баг Qwen3-4B-Instruct (2026-07-30, live multi-tool test):
// Модель ТЕРЯЕТ `}` для закрытия последнего call-объекта и добавляет лишний `}`
// в самом конце. Точные позиции в live input (228 байт):
//   - Промежуточный call (Paris): input `..."}},"` (4 chars между string close и `,`)
//     — ПРАВИЛЬНАЯ структура (`}"` + `}}` close function + close call), НЕ трогаем.
//   - Последний call (London) + array: input `..."}"]}` (4 chars в конце)
//     — НЕПРАВИЛЬНАЯ. Модель забыла close call `}`, потом `]` array close, потом extra `}`.
//     Correct: `..."}"}]` (close function, close call, array close) — нужно ВСТАВИТЬ `}` перед `]`.
//
// Эвристика: одна точечная вставка в самом конце.
// (Идемпотентно: повторный прогон не меняет результат.)
func fixTrailingBraceMisorder(s string) string {
	prev := ""
	for iter := 0; iter < 8 && prev != s; iter++ {
		prev = s
		// Паттерн: trailing `]}` (2 chars) → `}]` (INSERT `}` BEFORE `]`)
		//   Live end:      `..."}"]}` (4 chars: function close, array close, extra `}`)
		//   Correct end:   `..."}"}]` (4 chars: function close, call close, array close)
		// Модель "потеряла" `}` для close call и "вставила" его после `]`.
		// Чиним: перемещаем trailing `}` ПЕРЕД `]`.
		if strings.HasSuffix(s, string([]byte{0x5D, 0x7D})) {
			s = s[:len(s)-2] + string([]byte{0x7D, 0x5D})
		}
	}
	return s
}

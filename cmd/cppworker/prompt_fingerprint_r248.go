// prompt_fingerprint_r248.go — R248 (2026-10-27): отпечаток запроса в логе.
//
// ЗАЧЕМ. Клиент (DeepSeek Harness) на Qwen3.8 уходит в цикл: запросы падают по
// таймауту, модель снова и снова генерирует. Снаружи не отличить две принципиально
// разные причины:
//   - клиент РЕТРАИТ тот же запрос (таймаут на клиенте) — тогда отпечаток
//     промпта повторяется, а роль tool/результат инструмента те же;
//   - результат инструмента НЕ доезжает/не рендерится — тогда на втором ходе
//     отпечаток другой, но role="tool" отсутствует или пуст, и модель
//     повторяет вызов.
//
// Отпечаток = sha256(prompt)[:12]. В лог уходит ОДНА строка: hash, длина промпта,
// распределение ролей, число и суммарный размер tool-результатов. Персональных
// данных и текста промпта в лог не попадает.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"strings"
)

// promptFingerprint — короткий стабильный отпечаток промпта для корреляции
// повторов в логе. Пустой промпт → "empty".
func promptFingerprint(prompt string) string {
	if prompt == "" {
		return "empty"
	}
	sum := sha256.Sum256([]byte(prompt))
	return hex.EncodeToString(sum[:6])
}

// toolPromptDebugTail — R248 (2026-10-27): хвост промпта для разбора, ПОЧЕМУ
// модель повторяет вызов инструмента после получения результата.
//
// Живой факт (стенд, gemma-4): второй ход с role:"tool" и результатом
// «int main(){return 0;}» снова вернул ТОТ ЖЕ tool_call и пустой content — и на
// /api/chat, и на /v1/chat/completions, то есть дело в общем построителе
// промпта, а не в поверхности. Чтобы понять, как шаблон реально отрендерил ход
// модели и результат инструмента, нужно видеть сам текст промпта.
//
// Включается ТОЛЬКО по CPPWORKER_DEBUG_TOOL_PROMPT=1 (по умолчанию выключено):
// промпт содержит пользовательские данные, поэтому в обычных логах его нет.
// Возвращает пустую строку, когда отладочный режим выключен.
func toolPromptDebugTail(prompt string) string {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CPPWORKER_DEBUG_TOOL_PROMPT"))) {
	case "1", "true", "yes", "on":
	default:
		return ""
	}
	const tail = 1500
	if len(prompt) <= tail {
		return prompt
	}
	return "…" + prompt[len(prompt)-tail:]
}

// promptSummary — распределение ролей и объём tool-результатов.
//
// Возвращает строку вида "user=2,assistant=3,tool=1" и агрегат по tool-сообщениям
// (сколько их и сколько байт контента). Именно tool-часть отвечает на вопрос
// «доехал ли результат инструмента обратно в модель».
func promptSummary(msgs []chatMessage) (roles string, toolResults int, toolResultBytes int) {
	counts := map[string]int{}
	for _, m := range msgs {
		role := strings.TrimSpace(m.Role)
		if role == "" {
			role = "?"
		}
		counts[role]++
		if role == "tool" {
			toolResults++
			toolResultBytes += len(m.Content)
		}
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, counts[k]))
	}
	return strings.Join(parts, ","), toolResults, toolResultBytes
}

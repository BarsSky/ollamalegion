// done_chunk_cancel_r354.go — R354 (2026-10-31): «клиент нажал Stop» — это НЕ ошибка.
//
// ЖИВОЙ ФАКТ (двухузловой стенд, проверка R353). Клиент запускает стрим /api/generate,
// затем жмёт Stop (балансер рассылает POST /api/cancel). Воркер честно прекращает
// генерацию и присылает финальный кадр:
//
//	{"done":true,"done_reason":"error",
//	 "error":"stream inference cancelled (code -100): bridge: inference aborted by user (BRIDGE_ERR_ABORTED)"}
//
// Балансер отдавал его клиенту КАК ЕСТЬ (finalizeNativeDoneFrameR331 правит только
// content). Следствия:
//  1. клиент видел done_reason="error" — то есть «сервер сломался», хотя это его
//     собственное действие; в UI это красная ошибка вместо обычной остановки;
//  2. в журнале балансера тот же кадр писался как Warn «upstream done-chunk carries
//     error» — оператор искал поломку воркера там, где её нет;
//  3. текст ошибки уходил в upstreamErrorText, а по нему работают эвристики
//     (auto-continue, распознавание n_ctx) — отмена могла включать чужую логику.
//
// Правило: отмена по инициативе пользователя — штатное завершение. Кадр остаётся
// ОДНИМ терминатором (инвариант R328/R51.3), но с done_reason="cancelled" из реестра
// R328 и БЕЗ поля error. Если воркер сообщил отмену не текстом, а иначе — кадр не
// трогаем: молча подменять чужие ошибки нельзя.
package balancer

import (
	"encoding/json"
	"strings"
)

// userCancelDoneChunkMarkersR354 — по каким признакам считаем, что генерацию прервал
// ПОЛЬЗОВАТЕЛЬ, а не сбой.
//
// Признаки взяты из живых текстов cppworker'а и C-моста:
//   - "BRIDGE_ERR_ABORTED" — код отмены моста (code -100);
//   - "aborted by user" — формулировка моста для cancel flag;
//   - "inference cancelled" — обёртка воркера над ошибкой моста.
//
// Сравнение регистронезависимое: текст приходит из двух разных слоёв, и его
// формулировки уже менялись.
var userCancelDoneChunkMarkersR354 = []string{
	"bridge_err_aborted",
	"aborted by user",
	"inference cancelled",
}

// isUserCancelDoneChunkR354 — текст ошибки done-кадра означает отмену пользователем.
func isUserCancelDoneChunkR354(errText string) bool {
	s := strings.ToLower(strings.TrimSpace(errText))
	if s == "" {
		return false
	}
	for _, m := range userCancelDoneChunkMarkersR354 {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// normalizeCancelDoneChunkR354 — переписать кадр отмены: done_reason="cancelled", без error.
//
// Возвращает (новая строка, true) если кадр изменён. Изменяется ТОЛЬКО поле причины и
// удаляется error: model/created_at/done/статистика остаются как пришли (клиент по ним
// закрывает ход и считает токены).
//
// Если строка не JSON-объект, не является done-кадром или в ней нет признаков отмены —
// возвращает исходную строку и false (чужие ошибки не подменяем).
func normalizeCancelDoneChunkR354(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || trimmed[0] != '{' {
		return line, false
	}
	var chunk map[string]interface{}
	if err := json.Unmarshal([]byte(trimmed), &chunk); err != nil {
		return line, false
	}
	done, _ := chunk["done"].(bool)
	if !done {
		return line, false
	}
	errText, _ := chunk["error"].(string)
	if !isUserCancelDoneChunkR354(errText) {
		return line, false
	}

	chunk["done_reason"] = string(DoneReasonCancelled)
	delete(chunk, "error")
	// Пустой response без пояснения: клиент уже видел дельты, а «ответа нет» из-за
	// отмены — ожидаемое состояние, а не повод для notice (R321 вставлял пояснение
	// именно по признаку пустого ответа, поэтому сюда его сознательно не пускаем).
	if _, ok := chunk["response"]; !ok {
		chunk["response"] = ""
	}

	out, err := json.Marshal(chunk)
	if err != nil {
		return line, false
	}
	return string(out), true
}

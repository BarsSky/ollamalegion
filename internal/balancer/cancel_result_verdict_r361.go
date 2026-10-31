// cancel_result_verdict_r361.go — R361 (2026-10-31): честный вердикт по ответу воркера
// на приказ об отмене.
//
// ЖИВОЙ ФАКТ (проверка R360 на стенде). Балансер отправил адресную отмену с
// request_id, воркер ответил **404** (генерации с таким id уже нет: он сам увидел
// закрытие соединения, выставил abort-флаг и снял запись из трекера), а в журнале
// балансера появилось:
//
//	info "cancel upstream: генерация отменена на воркере" ... status=404
//
// То есть сообщение утверждало успех там, где отменять было уже нечего. Такой лог хуже
// отсутствующего: при разборе «почему Stop не работает» он уводит в противоположную
// сторону. R361 различает три исхода явно:
//
//   - cancelled — воркер подтвердил отмену (cancelled > 0). Это успех.
//   - already_done — отменять было нечего (404 / no_active_generation / cancelled == 0).
//     Не ошибка: генерация уже завершилась или была прервана самим воркером.
//   - failed — воркер ответил ошибкой (5xx, 401) или тело нечитаемо. ВОт это уже повод
//     для Warn: отмена не дошла, воркер мог продолжить работу.
package balancer

import "strings"

// cancelVerdictR361 — итог приказа об отмене по HTTP-коду и телу ответа.
type cancelVerdictR361 string

const (
	cancelVerdictCancelled   cancelVerdictR361 = "cancelled"
	cancelVerdictAlreadyDone cancelVerdictR361 = "already_done"
	cancelVerdictFailed      cancelVerdictR361 = "failed"
)

// cancelVerdictFromResponseR361 — классифицировать ответ воркера.
//
// body — распарсенный JSON ответа (может быть nil/пустым): воркер отдаёт
// {"cancelled": N, "status": "cancelled"|"no_active_generation", ...}.
func cancelVerdictFromResponseR361(status int, body map[string]interface{}) cancelVerdictR361 {
	if status >= 500 || status == 401 || status == 403 {
		return cancelVerdictFailed
	}
	if status == 404 {
		// Воркер не нашёл генерацию по request_id: она уже завершилась и снята с учёта.
		// Это не ошибка отмены — работа уже остановлена.
		return cancelVerdictAlreadyDone
	}
	if status < 200 || status >= 300 {
		return cancelVerdictFailed
	}

	if n, ok := intFromAnyR361(body["cancelled"]); ok && n > 0 {
		return cancelVerdictCancelled
	}
	if s, ok := body["status"].(string); ok {
		if strings.EqualFold(s, "no_active_generation") {
			return cancelVerdictAlreadyDone
		}
	}
	// 2xx, но ни счётчика, ни статуса: считаем, что отменять было нечего, а не «успех».
	return cancelVerdictAlreadyDone
}

// intFromAnyR361 — число из JSON (float64 после encoding/json, но бывает int/строка).
func intFromAnyR361(v interface{}) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	case string:
		if n == "" {
			return 0, false
		}
		val := 0
		for _, r := range n {
			if r < '0' || r > '9' {
				return 0, false
			}
			val = val*10 + int(r-'0')
		}
		return val, true
	}
	return 0, false
}

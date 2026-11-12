// fields_text.go — R493-12a-dump (2026-11-12, по архиву стенда): ЧИСЛА
// СТРУКТУРИРОВАННЫХ ПОЛЕЙ В ТЕКСТОВОМ ДАМПЕ ЛОГОВ.
//
// ЖИВОЙ ФАКТ. В архиве 2026-11-12 (стенд A10 ×2) текстовые дампы логов
// (`balancer-logs.txt` и `backends/<id>.bundle/logs.txt`) содержали ТОЛЬКО
// `LogEntry.Message`, а `LogEntry.Fields` терялись. Снаружи это выглядело так:
//
//	R482: удержание вывода снято — ход снова потоковый        (без held_ms/held_bytes)
//	R83 гейт n_ctx: тип KV-cache из плана загрузки            (без значения)
//	/api/chat stream response summary                          (без content_len/tokens)
//
// То есть архив, сделанный ради разбора конкретного хода, не позволял разобрать
// ничего: причины и числа — именно в полях. Балансерная половина была починена
// в diagnostic-дампе (`internal/api/diag_bundle_r476.go`), а ВОРКЕРНАЯ осталась:
// `cmd/cppworker/debug_bundle_r476.go` писал e.Message и всё.
//
// ПОЧЕМУ ЗДЕСЬ, А НЕ В ДВУХ МЕСТАХ. Два независимых форматтера неизбежно
// расходятся (ключи в разном порядке, кавычки по-разному), и тогда diff между
// архивами перестаёт быть читаемым. Одна функция — один формат.
package logger

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// FieldValueText — значение поля в одну строку, без переводов строк.
//
// Строки с пробелами/кавычками берём в кавычки (strconv.Quote), числа и bool —
// как есть, вложенные структуры — компактным JSON (иначе поле терялось бы
// целиком), ошибка — текстом в кавычках.
func FieldValueText(v interface{}) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case string:
		if t == "" {
			return "\"\""
		}
		if strings.ContainsAny(t, " \t\"\n") {
			return strconv.Quote(t)
		}
		return t
	case error:
		return strconv.Quote(t.Error())
	case bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return fmt.Sprintf("%v", t)
	default:
		if raw, err := json.Marshal(t); err == nil {
			return string(raw)
		}
		return fmt.Sprintf("%v", t)
	}
}

// FieldsText — поля записи в виде " key=value" в СТАБИЛЬНОМ порядке ключей.
// Пустая карта даёт пустую строку (никаких висящих пробелов).
func FieldsText(fields map[string]interface{}) string {
	if len(fields) == 0 {
		return ""
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(" ")
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(FieldValueText(fields[k]))
	}
	return b.String()
}

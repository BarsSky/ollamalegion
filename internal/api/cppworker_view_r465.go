// cppworker_view_r465.go — R465 (2026-11-06): чтение вложенного раздела
// outputBudget из ответа cppworker (`GET /api/v1/cppworker/config`).
//
// ЗАЧЕМ ОТДЕЛЬНЫЙ ФАЙЛ. В cppworkerSnapshot уже есть intValue/strValue/boolValue,
// но они читают ТОЛЬКО верхний уровень Config. Политика бюджета вывода приходит
// вложенным объектом (`outputBudget`), и без явного доступа к нему WebUI снова
// показывал бы 0 — ровно та жалоба, из-за которой это и делается.
package api

// mapValue — вложенный объект по ключу (nil-safe).
func (c cppworkerSnapshot) mapValue(key string) map[string]interface{} {
	if c.Config == nil {
		return nil
	}
	raw, ok := c.Config[key]
	if !ok {
		return nil
	}
	switch m := raw.(type) {
	case map[string]interface{}:
		return m
	default:
		return nil
	}
}

// viewIntR465 — число из вложенного объекта.
func viewIntR465(m map[string]interface{}, key string) (int, bool) {
	if m == nil {
		return 0, false
	}
	switch v := m[key].(type) {
	case float64:
		return int(v), true
	case int:
		return v, true
	}
	return 0, false
}

// viewStrR465 — строка из вложенного объекта.
func viewStrR465(m map[string]interface{}, key string) string {
	if m == nil {
		return ""
	}
	if s, ok := m[key].(string); ok {
		return s
	}
	return ""
}

// viewBoolR465 — булево из вложенного объекта.
func viewBoolR465(m map[string]interface{}, key string) (bool, bool) {
	if m == nil {
		return false, false
	}
	b, ok := m[key].(bool)
	return b, ok
}

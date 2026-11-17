// validate.go — проверки СХЕМЫ хранилища: идентификаторы и имена параметров.
//
// ЧЕГО ЗДЕСЬ СОЗНАТЕЛЬНО НЕТ: границ значений. Единственный серверный
// валидатор границ — `validateModelProfile` в internal/api
// (handlers_cppworker_profiles.go:1211-1266), и он вызывается из HTTP-слоя.
// Здесь проверяется только то, чего в нём нет: имя параметра обязано быть
// именем поля `types.LlamaCppModelProfile`, а id — slug'ом.
//
// Второго списка имён полей нет: имена берутся reflection'ом по json-тегам
// `types.LlamaCppModelProfile` (§2.6, §7 п.10 документа).
package reference

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"

	"ollama-loadbalancer/pkg/types"
)

// idPattern — slug идентификатора записи/шаблона (§2.3):
// `^[a-z0-9][a-z0-9._-]{2,63}$`.
var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{2,63}$`)

// ValidateID — проверка идентификатора. Пустой/неподходящий → ошибка с причиной.
func ValidateID(id string) error {
	if id == "" {
		return errors.New("id is required")
	}
	if !idPattern.MatchString(id) {
		return fmt.Errorf("id %q must match %s", id, idPattern.String())
	}
	return nil
}

var (
	knownParamNamesOnce sync.Once
	knownParamNames     []string
	knownParamNameSet   map[string]bool
)

// loadKnownParamNames — имена полей профиля из json-тегов
// `types.LlamaCppModelProfile` (pkg/types/balancing.go:283-469).
//
// Reflection, а не список в коде: список разъедется с типом при первом же
// добавленном поле, и шаблон с новым полем молча перестанет применяться.
func loadKnownParamNames() {
	t := reflect.TypeOf(types.LlamaCppModelProfile{})
	names := make([]string, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" {
			continue // неэкспортируемое поле в JSON не попадает
		}
		tag := f.Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		name := strings.Split(tag, ",")[0]
		if name == "" {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	knownParamNameSet = make(map[string]bool, len(names))
	for _, n := range names {
		knownParamNameSet[n] = true
	}
	knownParamNames = names
}

// KnownParamNames — отсортированный список допустимых имён параметров.
// Используется в ответах 400 unknown_param (поле `allowed`).
func KnownParamNames() []string {
	knownParamNamesOnce.Do(loadKnownParamNames)
	out := make([]string, len(knownParamNames))
	copy(out, knownParamNames)
	return out
}

// IsKnownParamName — является ли имя полем профиля модели.
func IsKnownParamName(name string) bool {
	knownParamNamesOnce.Do(loadKnownParamNames)
	return knownParamNameSet[name]
}

// UnknownParamNames — какие из имён не являются полями профиля (в порядке входа).
func UnknownParamNames(names []string) []string {
	knownParamNamesOnce.Do(loadKnownParamNames)
	unknown := make([]string, 0)
	for _, n := range names {
		if !knownParamNameSet[n] {
			unknown = append(unknown, n)
		}
	}
	return unknown
}

// ParamNames — имена параметров рекомендации в порядке объявления.
func ParamNames(params []Param) []string {
	out := make([]string, 0, len(params))
	for _, p := range params {
		out = append(out, p.Name)
	}
	return out
}

// DuplicateParamName — первое повторяющееся имя параметра (пусто, если нет).
//
// Повтор имени — не «последний победил», а ошибка: иначе две строки с разными
// значениями молча давали бы непредсказуемое значение.
func DuplicateParamName(params []Param) string {
	seen := make(map[string]bool, len(params))
	for _, p := range params {
		if seen[p.Name] {
			return p.Name
		}
		seen[p.Name] = true
	}
	return ""
}

// ParamsToObject — список пар `params[]` → объект формы тела PUT профиля.
//
// Именно объект, а не своя структура: он декодируется в
// `types.LlamaCppModelProfile` тем же способом, что и тело PUT профиля, поэтому
// типы проверяет encoding/json, а не второй рукописный валидатор.
func ParamsToObject(params []Param) (map[string]json.RawMessage, error) {
	obj := make(map[string]json.RawMessage, len(params))
	for _, p := range params {
		if strings.TrimSpace(p.Name) == "" {
			return nil, errors.New("param name is required")
		}
		if len(p.Value) == 0 {
			return nil, fmt.Errorf("param %q has no value", p.Name)
		}
		if _, exists := obj[p.Name]; exists {
			return nil, fmt.Errorf("param %q is duplicated", p.Name)
		}
		obj[p.Name] = p.Value
	}
	return obj, nil
}

// DecodeParamsObject — params шаблона → объект «имя поля → значение».
//
// Принимается ТОЛЬКО JSON-объект: `null`, массив или строка — ошибка запроса,
// иначе шаблон «без параметров» применялся бы как «ничего не менять» и
// выглядел бы рабочим.
func DecodeParamsObject(raw json.RawMessage) (map[string]json.RawMessage, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil, errors.New("params is required and must be a JSON object")
	}
	if !strings.HasPrefix(trimmed, "{") {
		return nil, fmt.Errorf("params must be a JSON object, got %s", jsonKind(trimmed))
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("params must be a JSON object: %w", err)
	}
	return obj, nil
}

// ObjectNames — имена ключей объекта (в порядке возрастания).
func ObjectNames(obj map[string]json.RawMessage) []string {
	names := make([]string, 0, len(obj))
	for k := range obj {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// jsonKind — человеческое имя вида JSON-значения для сообщения об ошибке.
func jsonKind(trimmed string) string {
	switch {
	case trimmed == "":
		return "nothing"
	case strings.HasPrefix(trimmed, "["):
		return "an array"
	case strings.HasPrefix(trimmed, "\""):
		return "a string"
	case trimmed == "true" || trimmed == "false":
		return "a boolean"
	case strings.HasPrefix(trimmed, "{"):
		return "an object"
	default:
		return "a number or bare value"
	}
}

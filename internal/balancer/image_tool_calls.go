// image_tool_calls.go — R637 (2026-11-19): разбор вызовов инструмента: extractImageToolCalls, аргументы, поля документа.
//
// ЗАЧЕМ ВЫНЕСЕНО. `image_tool.go` держал 1671 строку: настройки и цель
// инструмента, схему для клиента, разбор вызовов, загрузку модели, генерацию и
// помощники. Здесь — один из слоёв; поведение не менялось.
package balancer

import (
	"encoding/json"
	"errors"
	"fmt"
	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/internal/imagetool"
	"ollama-loadbalancer/pkg/strutil"
	"strconv"
	"strings"
)

// imageToolCall — разобранный вызов одного из наших инструментов.
type imageToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// isListCall — вызов каталога (list_image_models), а не генерации.
func (c imageToolCall) isListCall() bool { return c.Name == imagetool.ListName }

// extractImageToolCalls — вызовы наших инструментов в ответе модели (R85:
// generate_image И list_image_models).
//
// cppworker нормализует вывод в message.tool_calls (в т.ч. для Gemma/Hermes/
// Llama/Mistral форматов), но полагаться только на это нельзя: у части моделей
// вызов приходит JSON-ом в content. Поэтому проверяем оба места.
func extractImageToolCalls(message map[string]interface{}) []imageToolCall {
	var calls []imageToolCall
	if raw, ok := message["tool_calls"].([]interface{}); ok {
		for _, item := range raw {
			entry, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			fn, ok := entry["function"].(map[string]interface{})
			if !ok {
				continue
			}
			name, _ := fn["name"].(string)
			if !isImageToolName(name) {
				continue
			}
			args := stringifyToolArguments(fn["arguments"])
			id, _ := entry["id"].(string)
			calls = append(calls, imageToolCall{ID: id, Name: name, Arguments: args})
		}
	}
	if len(calls) == 0 {
		content, _ := message["content"].(string)
		if content != "" {
			if detected, _, found := detectAndExtractToolCallsFromContent(content); found {
				for _, item := range detected {
					entry, ok := item.(map[string]interface{})
					if !ok {
						continue
					}
					fn, ok := entry["function"].(map[string]interface{})
					if !ok {
						continue
					}
					name, _ := fn["name"].(string)
					if !isImageToolName(name) {
						continue
					}
					id, _ := entry["id"].(string)
					calls = append(calls, imageToolCall{
						ID:        id,
						Name:      name,
						Arguments: stringifyToolArguments(fn["arguments"]),
					})
				}
			}
		}
	}
	return calls
}

// isImageToolName — имя принадлежит нашей паре инструментов.
func isImageToolName(name string) bool {
	return name == imagetool.Name || name == imagetool.ListName
}

// imageToolArgs — аргументы вызова в терминах движка.
type imageToolArgs struct {
	Prompt         string
	NegativePrompt string
	Width          int
	Height         int
	Steps          int
	CFG            float64
	Seed           *int64
	Model          string
	// Family — параметр инструмента list_image_models (фильтр каталога по
	// семейству). У generate_image его нет, но разбор общий — поле просто пустое.
	Family string
	// List — вызов инструмента каталога (list_image_models): prompt не нужен,
	// генерации не будет. Введено, чтобы у каталога и генерации был один разбор
	// аргументов (одно место правды про имена полей JSON).
	List bool
}

// parseImageToolArgs разбирает JSON-аргументы вызова.
//
// list=true — аргументы инструмента каталога: prompt не требуется (он там не
// нужен вовсе), а семейство читается из того же разбора.
func parseImageToolArgs(raw string) (imageToolArgs, error) {
	return parseImageToolArgsFor(raw, false)
}

// parseImageToolArgsFor — общий разбор аргументов обоих инструментов.
//
// УСТОЙЧИВОСТЬ К ПУСТЫМ АРГУМЕНТАМ (живой дефект, найденный на стенде R85):
// модель вызывает инструмент без параметров (list_image_models) и присылает
// arguments как ПУСТУЮ СТРОКУ ("" или пробелы) либо как нестроковый JSON
// (null, {}). Первая версия возвращала «arguments не JSON: cannot unmarshal
// string into Go value of type map[string]interface {}» — то есть инструмент,
// который специально сделан без обязательных параметров, отказывал на самом
// естественном вызове. Пустое значение трактуем как «параметров нет».
func parseImageToolArgsFor(raw string, list bool) (imageToolArgs, error) {
	doc, err := decodeToolArguments(raw)
	if err != nil {
		return imageToolArgs{}, err
	}
	args := imageToolArgs{
		List:           list,
		Prompt:         stringField(doc, "prompt"),
		NegativePrompt: firstNonEmpty(stringField(doc, "negative_prompt"), stringField(doc, "negativePrompt")),
		Width:          intField(doc, "width"),
		Height:         intField(doc, "height"),
		Steps:          intField(doc, "steps"),
		CFG:            floatField(doc, "cfg"),
		Model:          stringField(doc, "model"),
		Family:         stringField(doc, "family"),
	}
	if v, ok := doc["seed"]; ok {
		switch t := v.(type) {
		case float64:
			s := int64(t)
			args.Seed = &s
		case string:
			if n, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64); err == nil {
				args.Seed = &n
			}
		}
	}
	if list {
		return args, nil
	}
	if strings.TrimSpace(args.Prompt) == "" {
		return args, errors.New(i18n.T("balancer.image_tool.args_no_prompt"))
	}
	return args, nil
}

// decodeToolArguments — аргументы вызова как объект.
//
// Модели присылают их в четырёх видах: объектом JSON, JSON-СТРОКОЙ внутри
// строки (как того требует OpenAI), пустой строкой (вызов без параметров),
// null. Все четыре обязаны разбираться: иначе «инструмент без параметров»
// становится невызываемым, а это ровно тот случай, ради которого он делался.
func decodeToolArguments(raw string) (map[string]interface{}, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed == "null" {
		return map[string]interface{}{}, nil
	}
	var doc map[string]interface{}
	if err := json.Unmarshal([]byte(trimmed), &doc); err == nil {
		if doc == nil {
			return map[string]interface{}{}, nil
		}
		return doc, nil
	}
	// Аргументы пришли JSON-СТРОКОЙ с JSON внутри (каноничная форма OpenAI).
	var inner string
	if err := json.Unmarshal([]byte(trimmed), &inner); err == nil {
		inner = strings.TrimSpace(inner)
		if inner == "" || inner == "null" {
			return map[string]interface{}{}, nil
		}
		var nested map[string]interface{}
		if err := json.Unmarshal([]byte(inner), &nested); err == nil {
			if nested == nil {
				return map[string]interface{}{}, nil
			}
			return nested, nil
		}
	}
	return nil, fmt.Errorf(i18n.T("balancer.image_tool.args_not_json"), shortForLog(trimmed))
}

// shortForLog — обрезать аргументы для текста ошибки (они уходят модели).
func shortForLog(s string) string {
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}

func stringField(doc map[string]interface{}, key string) string {
	v, _ := doc[key].(string)
	return strings.TrimSpace(v)
}

func intField(doc map[string]interface{}, key string) int {
	switch t := doc[key].(type) {
	case float64:
		return int(t)
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(t)); err == nil {
			return n
		}
	}
	return 0
}

func floatField(doc map[string]interface{}, key string) float64 {
	switch t := doc[key].(type) {
	case float64:
		return t
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(t), 64); err == nil {
			return f
		}
	}
	return 0
}

// R507 (шаг R7): единственная реализация — pkg/strutil (см. там, почему
// вариантов три: копии расходились обработкой пробелов).
var firstNonEmpty = strutil.FirstNonEmptyRaw

func containsString(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}

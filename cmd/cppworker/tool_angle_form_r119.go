// tool_angle_form_r119.go — R119 (2026-10-15): angle-форма вызова инструмента
// разбирается в СТРУКТУРНЫЙ tool_call на стороне cppworker.
//
// ЖИВОЙ ДЕФЕКТ (скриншот оператора, DSH через балансер). Модель отдала вызов
// инструмента в angle-форме Hermes/Qwen3-Coder:
//
//	<tool_call> <function=bash> <parameter=command> pwd && ls -la </parameter>
//	<parameter=description> … </parameter> </function> </tool_call>
//
// Ни JSON-в-Hermes (`extractHermesToolCalls` ищет `{…}` внутри `<tool_call>`), ни
// gemma-native (ждал `name{args}`), ни Llama/Mistral-парсеры эту форму не знают:
// вызов уезжал клиенту ТЕКСТОМ в content, инструмент не исполнялся.
//
// Балансер такую форму уже умеет (llamacpp_toolcall_detector.go, ветка
// TOOLCALL-ANGLE) — но он лишь ВЫРЕЗАЕТ разметку из видимого текста и синтезирует
// tool_calls на транспортном слое. cppworker же обязан отдать структуру сам:
// иначе она не появится ни в non-stream ответе, ни в `/api/chat`, ни у клиентов,
// которые ходят в cppworker напрямую (мимо балансера).
//
// Тесты: tool_angle_form_r119_test.go — точная строка со скриншота, несколько
// параметров, обрезанный стрим (без </parameter>), отсутствие ложных срабатываний
// на обычном тексте.
package main

import (
	"encoding/json"
	"regexp"
	"strings"
)

// angleFunctionRegex — блок angle-формы: `<function=NAME>…` до `</function>` или
// до конца текста (модель может не закрыть блок — см. обрезанный стрим).
var angleFunctionRegex = regexp.MustCompile(`(?s)<function=([^>\s]+)\s*>(.*?)(?:</function>|$)`)

// angleParameterRegex — параметр С закрывающим тегом: `<parameter=KEY>VALUE</parameter>`.
var angleParameterRegex = regexp.MustCompile(`(?s)<parameter=([^>\s]+)\s*>(.*?)</parameter>`)

// angleParameterOpenRegex — открывающий тег параметра БЕЗ закрывающего: так
// выглядит обрезанный стрим (живой скриншот: `<parameter=command> pwd && ls -la`
// и всё, дальше вывод оборвался).
var angleParameterOpenRegex = regexp.MustCompile(`(?s)<parameter=([^>\s]+)\s*>`)

// angleMarkerFunction — единственный признак, по которому включается разбор:
// без него функция выходит сразу (горячий путь — вызывается на каждый ответ с
// инструментами).
const angleMarkerFunction = "<function="

// extractHermesAngleToolCalls разбирает angle-форму вызова в структурные
// tool_calls. Возвращает nil, если angle-разметки нет.
//
// Значения параметров — ВСЕГДА строки: так же поступает балансер (см.
// parseHermesAngleToolCall), и так их ждёт OpenAI-схема arguments. Обрамляющие
// пробелы/переводы строк снимаются — это форматирование модели, а не значение;
// внутренние сохраняются (для `pwd && ls -la` это важно).
func extractHermesAngleToolCalls(output string) []openAIToolCall {
	if !strings.Contains(output, angleMarkerFunction) {
		return nil
	}
	matches := angleFunctionRegex.FindAllStringSubmatch(output, -1)
	if len(matches) == 0 {
		return nil
	}

	var result []openAIToolCall
	for _, m := range matches {
		name := strings.TrimSpace(m[1])
		if name == "" {
			continue
		}
		args := angleArguments(m[2])
		raw, err := json.Marshal(map[string]interface{}{
			"name":      name,
			"arguments": args,
		})
		if err != nil {
			continue
		}
		// Переиспользуем штатный разбор Hermes-объекта: он же проставляет id,
		// тип function и сериализует arguments в JSON-строку.
		if call := parseHermesSingleCall(raw); call != nil {
			result = append(result, *call)
		}
	}
	if len(result) == 0 {
		return nil
	}
	normalizeToolCalls(result)
	return result
}

// angleArguments достаёт параметры из тела `<function=…>`.
//
// Сначала пробуем СТРОГИЙ разбор (с `</parameter>`). Если он не нашёл ни одного
// параметра, но открывающие теги есть — значит поток оборван, и значения берём
// «до следующего `<parameter=` или до конца»: лучше вызвать инструмент с
// обрезанным значением (клиент увидит и решит), чем не вызвать его вовсе.
func angleArguments(body string) map[string]string {
	args := map[string]string{}
	for _, m := range angleParameterRegex.FindAllStringSubmatch(body, -1) {
		key := strings.TrimSpace(m[1])
		if key == "" {
			continue
		}
		args[key] = strings.TrimSpace(m[2])
	}
	if len(args) > 0 {
		return args
	}

	opens := angleParameterOpenRegex.FindAllStringSubmatchIndex(body, -1)
	if len(opens) == 0 {
		return args
	}
	for i, idx := range opens {
		key := strings.TrimSpace(body[idx[2]:idx[3]])
		if key == "" {
			continue
		}
		valueStart := idx[1]
		valueEnd := len(body)
		if i+1 < len(opens) {
			valueEnd = opens[i+1][0]
		}
		// Отрезаем возможный хвост с закрывающими тегами.
		value := body[valueStart:valueEnd]
		if cut := strings.Index(value, "</"); cut >= 0 {
			value = value[:cut]
		}
		args[key] = strings.TrimSpace(value)
	}
	return args
}

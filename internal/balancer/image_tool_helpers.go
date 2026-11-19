// image_tool_helpers.go — R637 (2026-11-19): мелкие помощники: поля JSON, время, короткие строки.
//
// ЗАЧЕМ ВЫНЕСЕНО. `image_tool.go` держал 1671 строку: настройки и цель
// инструмента, схему для клиента, разбор вызовов, загрузку модели, генерацию и
// помощники. Здесь — один из слоёв; поведение не менялось.
package balancer

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// familySuffix — « семейства X» для текста результата (пусто, если фильтра нет).
func familySuffix(family string) string {
	if family == "" {
		return ""
	}
	return " семейства " + family
}

// rawStringField — строковое поле из сырого JSON-аргумента (для аргументов,
// которые не разбирает imageToolArgs: например family у list_image_models).
func rawStringField(raw, key string) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	var doc map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return ""
	}
	v, _ := doc[key].(string)
	return strings.TrimSpace(v)
}

// upstreamErrorMessage — короткий текст ошибки из ответа воркера.
func upstreamErrorMessage(raw []byte, status int) string {
	var doc map[string]interface{}
	if json.Unmarshal(raw, &doc) == nil {
		if s, _ := doc["error"].(string); s != "" {
			return s
		}
		if e, ok := doc["error"].(map[string]interface{}); ok {
			if s, _ := e["message"].(string); s != "" {
				return s
			}
		}
		if s, _ := doc["message"].(string); s != "" {
			return s
		}
	}
	text := strings.TrimSpace(string(raw))
	if len(text) > 300 {
		text = text[:300] + "…"
	}
	if text == "" {
		text = http.StatusText(status)
	}
	return text
}

// secondsOf — длительность в секундах: из duration_ms воркера, иначе из замера.
func secondsOf(v interface{}, fallback time.Duration) float64 {
	if ms, ok := v.(float64); ok && ms > 0 {
		return ms / 1000
	}
	return fallback.Seconds()
}

// shortPrompt — подпись для alt-текста markdown (без переводов строк).
func shortPrompt(prompt string) string {
	s := strings.Join(strings.Fields(prompt), " ")
	if len(s) > 60 {
		s = s[:60] + "…"
	}
	if s == "" {
		return "generated image"
	}
	return s
}

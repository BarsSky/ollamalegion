// autopull_errors_r340.go — R340 (2026-10-30), этап 4 глобальной проверки API:
// ошибки авто-pull в форме контракта.
//
// НАЙДЕНО РАЗБОРОМ ВЕТКИ АВТО-PULL (proxy.go): три исхода авто-pull — «pull не
// удался», «нет свободного слота на новом бэкенде», «pull прошёл, но запрос упал»
// — отдавались клиенту через http.Error, то есть ТЕКСТОМ (`text/plain`), в то
// время как весь остальной API отвечает JSON-объектом с полем error.
//
// ПОЧЕМУ ЭТО ДЕФЕКТ. Клиенты (Cline/OpenWebUI) разбирают тело ошибки как JSON;
// на текстовом ответе они показывают либо пустое сообщение, либо «invalid json».
// Оператор теряет причину («модель не найдена» против «нет слота» против
// «pull сломался»), хотя она у балансера уже есть. Плюс статус 500 на «pull прошёл,
// запрос упал» — это ошибка воркера, а не клиента.
//
// РЕШЕНИЕ. Один помощник формы ошибки для этой ветки: JSON своей поверхности
// (Ollama — error строкой, OpenAI — объект error), статус осмысленный, причина
// внутри тела.
package balancer

import (
	"encoding/json"
	"net/http"
)

// writeAutoPullErrorR340 — ошибка авто-pull в JSON-форме поверхности.
//
// reason — человекочитаемая причина; details — исходная ошибка (для оператора).
// form — "ollama" | "openai" (выбирается по пути запроса).
func writeAutoPullErrorR340(w http.ResponseWriter, form, model string, status int, reason string, err error) {
	if w == nil {
		return
	}
	detail := ""
	if err != nil {
		detail = err.Error()
	}
	body := map[string]interface{}{
		"model":  model,
		"reason": reason,
		"detail": detail,
	}
	if form == "openai" {
		body["error"] = map[string]interface{}{
			"message": reason + ": " + detail,
			"type":    "invalid_request_error",
			"code":    status,
		}
	} else {
		body["error"] = reason
		if detail != "" {
			body["error"] = reason + ": " + detail
		}
	}
	encoded, marshalErr := json.Marshal(body)
	if marshalErr != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":"internal error while formatting a JSON error"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(encoded, '\n'))
}

// autoPullErrorFormR340 — форма ошибки по пути запроса.
func autoPullErrorFormR340(path string) string {
	if len(path) >= 3 && path[:3] == "/v1" {
		return "openai"
	}
	return "ollama"
}

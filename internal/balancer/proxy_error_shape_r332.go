// proxy_error_shape_r332.go — R332 (2026-10-30): форма ошибки одинаковой
// поверхности, а не «что прислал воркер».
//
// НАЙДЕНО ЖИВОЙ МАТРИЦЕЙ (tests/api_contract/live_matrix_test.go). Запрос на
// неизвестный путь (`/api/definitely-not-a-route`, `/v1/definitely-not-a-route`)
// уходит в generic-прокси, воркер отвечает своим `http.NotFound` — то есть
// ТЕКСТОМ `404 page not found\n` и `Content-Type: text/plain`. Балансер
// пересылал это как есть.
//
// ПОЧЕМУ ЭТО ДЕФЕКТ, А НЕ КОСМЕТИКА. И Ollama-, и OpenAI-клиенты разбирают тело
// ошибки как JSON (Cline показывает `error.message`, OpenWebUI — `error`).
// Текстовый 404 они не могут показать: пользователь видит пустую ошибку или
// «invalid json», а оператор теряет диагностику. Остальные ручки обеих
// поверхностей JSON уже отдают — значит ответ зависел от того, кто именно
// сгенерировал ошибку.
//
// РЕШЕНИЕ. Ошибки 4xx/5xx, пришедшие от воркера НЕ в JSON, нормализуются на
// границе балансера в форму своей поверхности. Статус сохраняется (клиент должен
// видеть тот же код), меняются только тело и Content-Type.
package balancer

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// upstreamErrorBodyLimitR332 — сколько читаем из тела ошибки воркера для
// диагностики. Больше не нужно: это ошибка, а не ответ модели.
const upstreamErrorBodyLimitR332 = 4 << 10

// normalizeUpstreamErrorBodyR332 — привести тело ошибки воркера к JSON своей
// поверхности. Возвращает resp (тот же указатель), возможно с новым Body.
//
// Трогаем ТОЛЬКО случаи, где клиент точно не сможет разобрать ответ:
//   - статус 4xx/5xx,
//   - Content-Type не содержит "json".
//
// Всё остальное (JSON-ошибки воркера, успешные ответы, стримы) проходит как есть.
func normalizeUpstreamErrorBodyR332(path string, resp *http.Response) *http.Response {
	if resp == nil || resp.Body == nil {
		return resp
	}
	if resp.StatusCode < 400 {
		return resp
	}
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.Contains(ct, "json") || strings.Contains(ct, "event-stream") ||
		strings.Contains(ct, "ndjson") {
		return resp
	}

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, upstreamErrorBodyLimitR332))
	_ = resp.Body.Close()
	upstreamText := strings.TrimSpace(string(raw))

	// Текст воркера сохраняем в поле detail: оператор должен видеть исходную
	// причину, а не только «not found».
	message := "upstream returned " + http.StatusText(resp.StatusCode)
	if upstreamText != "" {
		message = upstreamText
	}

	var payload []byte
	if strings.HasPrefix(path, "/v1/") {
		// OpenAI-поверхность: error — объект с message/type/code.
		payload, _ = json.Marshal(map[string]interface{}{
			"error": map[string]interface{}{
				"message": message,
				"type":    "invalid_request_error",
				"code":    resp.StatusCode,
			},
		})
	} else {
		// Ollama-поверхность: error — строка (так её читают Cline/OpenWebUI).
		payload, _ = json.Marshal(map[string]interface{}{
			"error":  message,
			"status": resp.StatusCode,
			"path":   path,
		})
	}
	payload = append(payload, '\n')

	resp.Body = io.NopCloser(bytes.NewReader(payload))
	resp.ContentLength = int64(len(payload))
	resp.Header.Set("Content-Type", "application/json")
	resp.Header.Set("Content-Length", strconv.Itoa(len(payload)))
	// Тело изменилось — прежний Transfer-Encoding недействителен.
	resp.Header.Del("Transfer-Encoding")
	return resp
}


// jsonBodyLooksLikeJSON — R335.1: быстрая проверка, похоже ли тело ошибки на
// JSON-объект. Нужна, чтобы не гонять нормализацию зря и не трогать JSON воркера.
func jsonBodyLooksLikeJSON(body []byte) bool {
	trimmed := bytes.TrimSpace(body)
	return len(trimmed) > 0 && trimmed[0] == '{'
}

// originalPathOrPath — путь клиентского запроса для выбора формы ошибки.
// Берём r.URL.Path; nil-safe (в транспорте r всегда есть, но функция чистая).
func originalPathOrPath(r *http.Request) string {
	if r == nil || r.URL == nil {
		return ""
	}
	return r.URL.Path
}
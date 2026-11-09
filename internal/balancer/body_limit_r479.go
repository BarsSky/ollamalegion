// body_limit_r479.go — R479 (2026-11-09): лимит тела на generic-пути.
//
// Аудит типов и распределения показал: `LB_MAX_REQUEST_BODY_MB` действовал
// только на handler-путях (`readRequestBodyLimited` в llamacpp_native_path.go),
// а основной поток `ServeHTTP` читал тело через `io.ReadAll` без предела —
// минимум трижды (recordRecentClient, extractModel — удалён в P2-14,
// parseRequestBody). Длинная
// история с base64-картинками давала лишние копии всего запроса в памяти.
//
// ЧТО ЗДЕСЬ. Два помощника:
//   - peekBodyR479 — прочитать НАЧАЛО тела (модель/stream в первых килобайтах) и
//     вернуть остаток в поток через MultiReader: ни один байт не теряется;
//   - лимит в parseRequestBody (см. proxy.go) — тот же порог, что на
//     handler-путях, ответ 413 без отправки запроса бэкенду.
package balancer

import (
	"bytes"
	"io"
	"net/http"
)

// peekBodyR479 — прочитать до max байт тела, вернув всё прочитанное и остаток
// потока обратно в r.Body.
//
// Возвращает (начало тела, true) либо (nil, false) при ошибке чтения. Если тело
// меньше max, оно прочитано целиком и восстановлено тоже целиком.
func peekBodyR479(r *http.Request, max int64) ([]byte, bool) {
	if r == nil || r.Body == nil || max <= 0 {
		return nil, false
	}
	head, err := io.ReadAll(io.LimitReader(r.Body, max))
	if err != nil {
		// Тело частично прочитано и уже не восстановить побайтово: отдаём поток
		// как есть, но вызывающий не получит модель — это деградация диагностики,
		// а не потеря запроса.
		r.Body = io.NopCloser(bytes.NewReader(head))
		return nil, false
	}
	// R480: BOM в начале тела ломал разбор JSON (json.Unmarshal на "\xef\xbb\xbf{"
	// падает), из-за чего модель оставалась пустой, а stream-флаг терялся. BOM
	// снимаем из уже прочитанного начала и отдаём в MultiReader БЕЗ него, чтобы
	// дальше по пути тело тоже было чистым.
	head = stripUTF8BOMR480(head)
	// Остаток потока не читаем — отдаём многослойный reader, чтобы дальше
	// parseRequestBody/проксирование увидели полное тело без второй копии.
	r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(head), r.Body))
	return head, true
}

// stripUTF8BOMR480 — снять UTF-8 BOM (EF BB BF) с начала тела.
//
// ЗАЧЕМ. Живая проверка R480: тело, записанное Windows-инструментом (PowerShell
// Set-Content -Encoding utf8 в 5.1 пишет BOM), балансер принимал, но:
//   - `parseRequestBody` не разбирал JSON → model="", stream-флаг терялся;
//   - `isStreamingFromBody` возвращал false → запрос на /api/chat с "stream":true
//     уходил в НЕ-stream транспорт и клиент получал ответ одним куском;
//   - воркеру уезжало тело с BOM (для строгого JSON-декодера это мусор в начале).
//
// BOM — не часть JSON (RFC 8259 требует UTF-8 без BOM), но клиент не обязан быть
// корректным, а балансер обязан не портить запрос. Снимаем ровно три байта.
func stripUTF8BOMR480(body []byte) []byte {
	if len(body) >= 3 && body[0] == 0xEF && body[1] == 0xBB && body[2] == 0xBF {
		return body[3:]
	}
	return body
}

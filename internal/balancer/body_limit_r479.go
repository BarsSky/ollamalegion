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
	// Остаток потока не читаем — отдаём многослойный reader, чтобы дальше
	// parseRequestBody/проксирование увидели полное тело без второй копии.
	r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(head), r.Body))
	return head, true
}

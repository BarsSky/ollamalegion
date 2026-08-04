package main

import (
	"net"
	"net/http"
	"strings"
	"unicode"
)

// getUserID — извлекает идентификатор пользователя из HTTP-запроса.
//
// Round 18 P0.3 (2026-08-04). Используется для per-user admission
// (UserTracker.TryAcquire / Release) и для логирования.
//
// Приоритет (по убыванию):
//  1. X-User-Id header (после sanitization) — стандартный канал, по которому
//     балансер / API-шлюз передают identity клиента
//  2. RemoteAddr с убранным портом (например "192.168.1.1:54321" → "192.168.1.1")
//     — fallback для прямых подключений без балансера
//  3. "anonymous" — крайний fallback
//
// Sanitization защищает от:
//   - очень длинных userID (DoS через раздувание map в UserTracker)
//   - control chars (ломают логи и потенциально JSON output)
//   - non-ASCII (чтобы userID был consistent в логах и metrics)
//
// SECURITY: это НЕ authentication. cppworker TRUSTS X-User-Id header.
// В проде между клиентом и cppworker должен быть API gateway / proxy,
// который верифицирует identity и проставляет header. cppworker использует
// header только для fair-share, не для авторизации.
func getUserID(r *http.Request) string {
	// 1. X-User-Id header (highest priority)
	if h := r.Header.Get("X-User-Id"); h != "" {
		return sanitizeUserID(h)
	}
	// 2. RemoteAddr (IP only, port stripped)
	if addr := r.RemoteAddr; addr != "" {
		if host, _, err := net.SplitHostPort(addr); err == nil && host != "" {
			return sanitizeUserID(host)
		}
		// addr без порта (например "unix socket" или "pipe") — используем as-is
		return sanitizeUserID(addr)
	}
	// 3. anonymous
	return "anonymous"
}

// sanitizeUserID очищает userID для безопасного использования в map keys и логах.
//
// Правила:
//   - max length 128 chars (truncate)
//   - control chars (< 0x20, 0x7F) → '_' (защита логов и JSON)
//   - разрешены: alnum + '_' + '-' + '.' + '@' + ':' + ' '
//   - остальные non-ASCII → '_' (для consistent в логах)
func sanitizeUserID(s string) string {
	if s == "" {
		return "anonymous"
	}
	// Truncate to 128 chars
	const maxLen = 128
	if len(s) > maxLen {
		s = s[:maxLen]
	}
	// Replace control chars + non-allowed
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r < 0x20 || r == 0x7F:
			// Control char → _
			out = append(out, '_')
		case r > 0x7E:
			// Non-ASCII (multibyte UTF-8) → _
			out = append(out, '_')
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			out = append(out, r)
		case r == '_' || r == '-' || r == '.' || r == '@' || r == ':' || r == ' ':
			out = append(out, r)
		default:
			// Other ASCII (punctuation etc.) → _
			out = append(out, '_')
		}
	}
	result := strings.TrimSpace(string(out))
	if result == "" {
		return "anonymous"
	}
	return result
}

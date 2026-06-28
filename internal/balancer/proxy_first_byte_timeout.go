// proxy_first_byte_timeout.go — PF-5 fix (2026-06-27).
//
// Проблема: pre-existing test failure TestOpenAIChat_HeaderTimeout_StillWorks
// (tests/first_byte_timeout_test.go) падал с "request did not fail within 10s
// despite 2s header timeout". Причина — ResponseHeaderTimeout на streaming
// Transport был отключён (=0), а per-request контекст с FirstByteTimeout
// никогда не применялся к фазе получения HTTP-заголовков.
//
// Решение: для streaming + явный FirstByteTimeout>0 — создаём per-request
// копию streaming Transport с включённым ResponseHeaderTimeout. Keepalive
// пул соединений остаётся общим (shallow copy Transport делит connPool),
// поэтому нет лишних TCP-соединений.
package balancer

import (
	"net/http"
	"time"

	"ollama-loadbalancer/pkg/logger"
)

// newStreamingClientWithResponseHeaderTimeout создаёт per-request http.Client
// с включённым ResponseHeaderTimeout для streaming. Использует shallow copy
// streamingTransportBase — разделяет с оригинальным Transport'ом:
//
//	- idle connection pool (MaxIdleConns*, IdleConnTimeout)
//	- keepalive настройки
//	- DisableCompression (важно для SSE)
//
// но получает собственное значение ResponseHeaderTimeout (per-request).
//
// Если p.streamingTransportBase == nil (например, в устаревших unit-тестах,
// которые создают Proxy через reflect/internal bypass), возвращаем
// p.streamingClient как fallback без модификации.
func (p *Proxy) newStreamingClientWithResponseHeaderTimeout(firstByteTimeout time.Duration) *http.Client {
	if firstByteTimeout <= 0 {
		// Не валидный таймаут — возвращаем дефолтный streamingClient.
		return p.streamingClient
	}
	if p.streamingTransportBase == nil {
		// Фолбэк: нет базового Transport'а — используем streamingClient как есть.
		logger.Get().Warnw("newStreamingClientWithResponseHeaderTimeout: no base transport, using streamingClient as-is",
			"first_byte_timeout", firstByteTimeout.Seconds())
		return p.streamingClient
	}

	// Clone Transport — разделяет connPool с базовым Transport'ом (Transport.Clone()
	// делает shallow copy с реинициализацией мьютекса), но получает собственное
	// значение ResponseHeaderTimeout.
	transportCopy := p.streamingTransportBase.Clone()
	transportCopy.ResponseHeaderTimeout = firstByteTimeout

	return &http.Client{
		Timeout:   0, // Таймаут управляется через контекст запроса (streamTimeout)
		Transport: transportCopy,
	}
}
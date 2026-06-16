package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// cppWorkerProxyTimeout — общий таймаут для проксирования запросов к CppWorker.
// HF download и search могут занимать до 60-90 секунд (особенно search, который
// параллельно подгружает файлы для каждого результата).
const cppWorkerProxyTimeout = 90 * time.Second

// resolveCppWorkerURL — определение базового URL CppWorker для заданного backendID.
// Возвращает host, port, baseURL.
//
// Важно: host.docker.internal сохраняется как есть. Прокси выполняется сервер-сервер
// (balancer → CppWorker), и контейнер balancer'а корректно резолвит этот Docker-алиас
// (в отличие от браузера, для которого изначально и был сделан прокси-эндпоинт).
//
// Возвращает ("", 0, "", err) если бэкенд не найден или не llama_cpp.
func (s *Server) resolveCppWorkerURL(backendID string) (string, int, string, error) {
	if s.proxy == nil {
		return "", 0, "", fmt.Errorf("proxy unavailable")
	}
	backend := s.proxy.GetBackend(backendID)
	if backend == nil {
		return "", 0, "", fmt.Errorf("backend %q not found", backendID)
	}
	if backend.Type != types.BackendTypeLlamaCpp {
		return "", 0, "", fmt.Errorf("backend %q is not llama_cpp (type=%q)", backendID, backend.Type)
	}

	host := backend.Host
	// По умолчанию 18092 (актуальный default для современных llama.cpp CppWorker).
	port := backend.CppWorkerPort
	if port <= 0 {
		port = 18092
	}
	return host, port, fmt.Sprintf("http://%s:%d", host, port), nil
}

// proxyToCppWorker — проксирует HTTP-запрос к CppWorker конкретного бэкенда.
// path — путь относительно CppWorker (например "/api/hf/download").
// Возвращает ответ CppWorker клиенту (с прозрачной передачей status, body, headers).
//
// Прокидывает заголовок X-HF-Token (для HuggingFace API) и X-Real-IP.
func (s *Server) proxyToCppWorker(w http.ResponseWriter, r *http.Request, backendID, path string) {
	log := logger.Get()
	host, port, baseURL, err := s.resolveCppWorkerURL(backendID)
	if err != nil {
		s.writeJSON(w, http.StatusNotFound, map[string]string{
			"error":   "backend not found",
			"message": err.Error(),
		})
		return
	}

	// Строим target URL
	target := baseURL + path
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}

	// Читаем тело входящего запроса (для POST)
	var bodyBytes []byte
	if r.Body != nil {
		var readErr error
		bodyBytes, readErr = io.ReadAll(r.Body)
		if readErr != nil {
			s.writeJSON(w, http.StatusBadRequest, map[string]string{
				"error":   "failed to read request body",
				"message": readErr.Error(),
			})
			return
		}
		_ = r.Body.Close()
	}

	// Контекст с таймаутом
	ctx, cancel := context.WithTimeout(r.Context(), cppWorkerProxyTimeout)
	defer cancel()

	// Создаём прокси-запрос
	req, err := http.NewRequestWithContext(ctx, r.Method, target, bytes.NewReader(bodyBytes))
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error":   "failed to build proxy request",
			"message": err.Error(),
		})
		return
	}

	// Копируем важные заголовки от клиента
	copyProxyHeaders(req.Header, r.Header)

	// Дополнительно проставляем X-Forwarded-* (полезно для CppWorker, если он логирует реальный IP клиента)
	if realIP := r.Header.Get("X-Real-IP"); realIP != "" {
		req.Header.Set("X-Real-IP", realIP)
	} else {
		req.Header.Set("X-Real-IP", r.RemoteAddr)
	}
	req.Header.Set("X-Forwarded-For", r.Header.Get("X-Forwarded-For"))
	req.Header.Set("X-Forwarded-Proto", schemeFromRequest(r))

	log.Debugw("proxyToCppWorker",
		"backend", backendID,
		"method", r.Method,
		"path", path,
		"target", target,
		"host", host,
		"port", port,
	)

	// Делаем запрос
	httpClient := &http.Client{
		Timeout: cppWorkerProxyTimeout,
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		log.Warnw("cppworker proxy request failed",
			"backend", backendID, "path", path, "target", target, "error", err.Error())
		statusCode := http.StatusBadGateway
		msg := err.Error()
		// Понятные сообщения для типовых сетевых ошибок
		if strings.Contains(msg, "context deadline exceeded") || strings.Contains(msg, "timeout") {
			statusCode = http.StatusGatewayTimeout
			msg = fmt.Sprintf("CppWorker at %s:%d is not responding (timeout %s)", host, port, cppWorkerProxyTimeout)
		} else if strings.Contains(msg, "connection refused") {
			statusCode = http.StatusBadGateway
			msg = fmt.Sprintf("CppWorker at %s:%d is unreachable (connection refused)", host, port)
		} else if strings.Contains(msg, "no such host") {
			statusCode = http.StatusBadGateway
			msg = fmt.Sprintf("CppWorker host %s:%d cannot be resolved from the balancer", host, port)
		}
		s.writeJSON(w, statusCode, map[string]string{
			"error":   "cppworker unreachable",
			"message": msg,
			"backend": backendID,
			"target":  fmt.Sprintf("%s:%d", host, port),
		})
		return
	}
	defer resp.Body.Close()

	// Копируем response headers (фильтруем hop-by-hop)
	for key, values := range resp.Header {
		if isHopByHopHeader(key) {
			continue
		}
		for _, v := range values {
			w.Header().Add(key, v)
		}
	}
	w.Header().Set("X-Proxied-From-CppWorker", fmt.Sprintf("%s:%d", host, port))

	// Читаем тело ответа в буфер — нужно для возможной подмены ответа
	// (страховка от старого CppWorker, который возвращает 500 «already in progress»).
	respBody, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		log.Warnw("cppworker proxy: failed to read response body",
			"backend", backendID, "path", path, "error", readErr.Error())
		w.WriteHeader(http.StatusBadGateway)
		return
	}

	// Специальная обработка: если CppWorker вернул 500 с «already in progress»
	// (старая версия бинаря), переписываем в 200 + «already_in_progress» JSON.
	// UI переключается на вкладку Downloads и показывает прогресс.
	if isDuplicateDownloadResponse(r, path, resp.StatusCode, respBody) {
		log.Infow("cppworker proxy: rewriting 500 «already in progress» to 200",
			"backend", backendID, "path", path,
			"original_body", string(respBody))
		// Content-Length надо сбросить, т.к. тело будет другим
		w.Header().Set("Content-Length", "")
		w.Header().Set("Content-Type", "application/json")
		writeDuplicateDownloadJSON(w)
		return
	}

	// Статус и тело
	w.WriteHeader(resp.StatusCode)
	if _, writeErr := w.Write(respBody); writeErr != nil {
		log.Warnw("cppworker proxy: failed to write response body",
			"backend", backendID, "path", path, "error", writeErr.Error())
	}
}

// isDuplicateDownloadResponse определяет, является ли ответ CppWorker
// «уже идёт загрузка». На уровне прокси у нас нет состояния активных загрузок
// (оно живёт в CppWorker-процессе), поэтому единственный надёжный сигнал —
// текст ошибки. Это страховка для случая, когда CppWorker работает со СТАРОЙ
// версией бинаря, которая возвращает 500 «download already in progress»,
// в то время как фронтенд уже обновлён и ожидает 200 (согласно пункту 1.5 плана).
func isDuplicateDownloadResponse(r *http.Request, path string, statusCode int, body []byte) bool {
	if statusCode != http.StatusInternalServerError && statusCode != http.StatusConflict {
		return false
	}
	// Проверяем только для POST /api/hf/download
	if r == nil || r.Method != http.MethodPost || path != "/api/hf/download" {
		return false
	}
	bodyStr := string(body)
	return strings.Contains(bodyStr, "already in progress") ||
		strings.Contains(bodyStr, "download already in progress")
}

// writeDuplicateDownloadJSON формирует JSON-ответ «уже идёт», который UI
// интерпретирует как нормальный успех и переключается на вкладку Downloads.
func writeDuplicateDownloadJSON(w http.ResponseWriter) {
	progress := map[string]interface{}{
		"status":     "downloading",
		"progressPct": 0.0,
	}
	resp := map[string]interface{}{
		"status":   "already_in_progress",
		"message":  "Download already in progress (rewritten from CppWorker 500)",
		"progress": progress,
	}
	enc := json.NewEncoder(w)
	_ = enc.Encode(resp)
}

// copyProxyHeaders — копирует headers от клиента к прокси-запросу, исключая hop-by-hop.
func copyProxyHeaders(dst, src http.Header) {
	for key, values := range src {
		if isHopByHopHeader(key) {
			continue
		}
		// Content-Length пересчитывается автоматически
		if strings.EqualFold(key, "Content-Length") {
			continue
		}
		// Host задаётся автоматически http.NewRequest
		if strings.EqualFold(key, "Host") {
			continue
		}
		for _, v := range values {
			dst.Add(key, v)
		}
	}
}

// isHopByHopHeader — true для заголовков, которые не должны передаваться прокси.
// (RFC 7230, раздел 6.1)
func isHopByHopHeader(name string) bool {
	switch strings.ToLower(name) {
	case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization",
		"te", "trailers", "transfer-encoding", "upgrade":
		return true
	}
	return false
}

// schemeFromRequest — определяет http/https из r.TLS / заголовка X-Forwarded-Proto.
func schemeFromRequest(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		return proto
	}
	return "http"
}
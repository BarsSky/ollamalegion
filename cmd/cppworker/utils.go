package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"ollama-loadbalancer/c/bridge"
	"ollama-loadbalancer/pkg/logger"
)

// ============================================================
// Middleware
// ============================================================

// authMiddleware проверяет API_TOKEN для защищённых эндпоинтов (PUT/POST/DELETE)
func authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := os.Getenv("API_TOKEN")
		if token == "" {
			next(w, r)
			return
		}
		authHeader := r.Header.Get("Authorization")
		if !strings.HasPrefix(authHeader, "Bearer ") || strings.TrimPrefix(authHeader, "Bearer ") != token {
			writeError(w, http.StatusUnauthorized, "invalid or missing API token")
			return
		}
		next(w, r)
	}
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", *allowedOrigin)
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-HF-Token")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		flusher, _ := w.(http.Flusher)
		lw := &loggingResponseWriter{ResponseWriter: w, statusCode: http.StatusOK, flusher: flusher}
		next.ServeHTTP(lw, r)
		duration := time.Since(start)
		remoteIP := r.RemoteAddr
		if idx := strings.LastIndex(r.RemoteAddr, ":"); idx > 0 {
			remoteIP = r.RemoteAddr[:idx]
		}
		logger.Get().Infow("HTTP request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", lw.statusCode,
			"duration", duration.String(),
			"remote", remoteIP)
	})
}

type loggingResponseWriter struct {
	http.ResponseWriter
	statusCode int
	flusher    http.Flusher
}

func (lw *loggingResponseWriter) WriteHeader(code int) {
	lw.statusCode = code
	lw.ResponseWriter.WriteHeader(code)
}

func (lw *loggingResponseWriter) Flush() {
	if lw.flusher != nil {
		lw.flusher.Flush()
	}
}

// ============================================================
// JSON утилиты
// ============================================================

func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	body, err := json.Marshal(data)
	if err != nil {
		logger.Get().Errorw("failed to marshal JSON response", "error", err)
		body = []byte(`{"error":"internal marshal error"}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	if _, writeErr := w.Write(body); writeErr != nil {
		logger.Get().Errorw("failed to write JSON response", "error", writeErr)
	}
}

// writeCppWorkerErrorWithBridgeInfo — структурированный JSON-ответ об ошибке
func writeCppWorkerErrorWithBridgeInfo(w http.ResponseWriter, status int, prefix string, err error) {
	info := bridge.GetLastErrorInfo()
	body := map[string]interface{}{
		"error": prefix + ": " + err.Error(),
		"code":  info.Code,
		"bridge_info": map[string]interface{}{
			"code":           info.Code,
			"current_n_ctx":  info.CurrentNCtx,
			"required_n_ctx": info.RequiredNCtx,
			"actual_tokens":  info.ActualTokens,
			"n_predict":      info.NPredict,
			"n_ctx_override": info.NCtxOverride,
			"max_vram_n_ctx": info.MaxVRAMNCtx,
			"message":        info.Message,
		},
	}
	writeJSON(w, status, body)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// ============================================================
// Хелперы
// ============================================================

func defaultBoolPtr(v *bool, def bool) bool {
	if v == nil {
		return def
	}
	return *v
}

func defaultIntPtr(v *int, def int) int {
	if v == nil {
		return def
	}
	return *v
}

// isMemorySlotError — определяет, является ли ошибка llama.cpp "memory slot" leak.
func isMemorySlotError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "failed to find a memory slot") ||
		strings.Contains(msg, "memory slot") ||
		strings.Contains(msg, "no slot")
}

// maybeRestartOnMemorySlotError — если err связан с memory slot leak.
func maybeRestartOnMemorySlotError(err error, modelName string) {
	if !isMemorySlotError(err) {
		return
	}
	logger.Get().Errorw("CRITICAL: memory slot leak detected in llama.cpp; restarting process to recover",
		"model", modelName, "error", err.Error())
	time.Sleep(200 * time.Millisecond)
	os.Exit(1)
}

// isModelLoadingError — true, если модель сейчас в процессе загрузки.
func isModelLoadingError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "model is loading")
}

// loadingInfoFor — хелпер: возвращает loading-метаданные для UI из ошибки.
func loadingInfoFor(_ error) (elapsedMs int64, retryAfterMs int) {
	return 0, 3000
}

// parseInt парсит строку в int с дефолтным значением.
func parseInt(s string, defaultVal int) (int, error) {
	if s == "" {
		return defaultVal, nil
	}
	val := 0
	_, err := fmt.Sscanf(s, "%d", &val)
	if err != nil {
		return defaultVal, err
	}
	return val, nil
}

// parseBoolEnv парсит env-значение в bool.
func parseBoolEnv(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	return s == "true" || s == "1" || s == "yes" || s == "on"
}

// isFlagSet — возвращает true, если флаг был явно передан.
func isFlagSet(name string) bool {
	found := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

// convertFlashAttn converts flash-attn flag value to FlashAttnType.
func convertFlashAttn(flagVal int) int {
	if flagVal == -2 {
		if currentConfig != nil {
			return currentConfig.DefaultFlashAttnType
		}
		return -1
	}
	if flagVal < -1 {
		return -1
	}
	if flagVal > 1 {
		return 1
	}
	return flagVal
}

// runHealthCheck выполняет одноразовый probe на /health и завершает процесс.
func runHealthCheck() {
	probePort := *port
	if !isFlagSet("port") {
		if envPort := os.Getenv("CPPWORKER_PORT"); envPort != "" {
			if p, err := strconv.Atoi(envPort); err == nil && p > 0 && p <= 65535 {
				probePort = p
			}
		}
	}
	url := fmt.Sprintf("http://127.0.0.1:%d/health", probePort)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck failed: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "healthcheck failed: HTTP %d from %s\n", resp.StatusCode, url)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stdout, "healthcheck passed")
	os.Exit(0)
}

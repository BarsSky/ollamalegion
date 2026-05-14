package agent

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"
)

// appendLog — добавляет строку в кольцевой буфер логов агента
func (a *Agent) appendLog(line string) {
	a.logBufMu.Lock()
	defer a.logBufMu.Unlock()
	if len(a.logBuffer) >= a.maxLogLines {
		a.logBuffer = a.logBuffer[1:]
	}
	a.logBuffer = append(a.logBuffer, line)
}

// startHealthServer — запускает HTTP-сервер с /health эндпоинтом для Docker HEALTHCHECK
func (a *Agent) startHealthServer() {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"status":"ok"}`)
	})

	// /api/logs — возвращает буферизованные логи агента
	mux.HandleFunc("/api/logs", func(w http.ResponseWriter, r *http.Request) {
		a.logBufMu.Lock()
		buf := make([]string, len(a.logBuffer))
		copy(buf, a.logBuffer)
		a.logBufMu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"lines": buf,
			"total": len(buf),
		})
	})

	// /api/restart — инициирует перезапуск агента (exit с последующим restart через supervisor/Docker)
	mux.HandleFunc("/api/restart", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"message": "Agent restart initiated. The process will exit and be restarted by the supervisor.",
		})

		// Запускаем остановку через горутину, чтобы ответ успел отправиться
		go func() {
			time.Sleep(500 * time.Millisecond)
			a.Stop()
			fmt.Printf("[%s] Agent restart: exiting process\n", time.Now().Format(time.RFC3339))
			os.Exit(0)
		}()
	})

	addr := fmt.Sprintf(":%d", a.config.MetricsPort)
	a.healthServer = &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	go func() {
		fmt.Printf("[%s] Health server listening on %s\n", time.Now().Format(time.RFC3339), addr)
		if err := a.healthServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Printf("[%s] Health server error: %v\n", time.Now().Format(time.RFC3339), err)
		}
	}()
}
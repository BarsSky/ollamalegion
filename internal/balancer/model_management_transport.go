// model_management_transport.go — R632 (2026-11-19): общий транспорт операций: unload, запросы к Ollama/raw, учёт активных операций, доступ к менеджеру.
//
// ЗАЧЕМ ВЫНЕСЕНО. `model_management.go` держал 2172 строки: конструктор,
// операции, ожидание загрузки, отдельный 447-строчный цикл загрузки llama.cpp и
// общий транспорт. Здесь — один из слоёв; поведение не менялось, код перенесён как есть.
package balancer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"ollama-loadbalancer/pkg/logger"
	"strings"
	"time"
)

// executeUnload — выгрузка модели из памяти (через /api/generate с keep_alive=0)
func (mm *ModelManager) executeUnload(host string, port int, backendID string, req ModelOpRequest) *ModelOpResult {
	url := fmt.Sprintf("http://%s:%d/api/generate", host, port)

	body := map[string]interface{}{
		"model":      req.ModelName,
		"keep_alive": "0s",
		"prompt":     "",
		"stream":     false,
	}

	resp, err := mm.sendRawRequest("POST", url, body)
	if err != nil {
		return &ModelOpResult{
			Success:   false,
			Operation: req.Operation,
			ModelName: req.ModelName,
			BackendID: backendID,
			Error:     fmt.Sprintf("failed to unload model: %v", err),
		}
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return &ModelOpResult{
			Success:   true,
			Operation: req.Operation,
			ModelName: req.ModelName,
			BackendID: backendID,
			Message:   fmt.Sprintf("Model '%s' unloaded successfully on backend '%s'", req.ModelName, backendID),
		}
	}

	return &ModelOpResult{
		Success:   false,
		Operation: req.Operation,
		ModelName: req.ModelName,
		BackendID: backendID,
		Error:     fmt.Sprintf("ollama error (HTTP %d): %s", resp.StatusCode, string(respBody)),
	}
}

// sendOllamaRequest — общая отправка запроса к Ollama API
func (mm *ModelManager) sendOllamaRequest(method, url, backendID string, req ModelOpRequest, body map[string]interface{}) *ModelOpResult {
	resp, err := mm.sendRawRequest(method, url, body)
	if err != nil {
		return &ModelOpResult{
			Success:   false,
			Operation: req.Operation,
			ModelName: req.ModelName,
			BackendID: backendID,
			Error:     fmt.Sprintf("request failed: %v", err),
		}
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		opName := operationDisplayName(req.Operation)
		return &ModelOpResult{
			Success:   true,
			Operation: req.Operation,
			ModelName: req.ModelName,
			BackendID: backendID,
			Message:   fmt.Sprintf("Model '%s' %s on backend '%s'", req.ModelName, opName, backendID),
		}
	}

	// Парсим ошибку Ollama
	errMsg := string(respBody)
	var ollamaErr struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(respBody, &ollamaErr) == nil && ollamaErr.Error != "" {
		errMsg = ollamaErr.Error
	}

	return &ModelOpResult{
		Success:   false,
		Operation: req.Operation,
		ModelName: req.ModelName,
		BackendID: backendID,
		Error:     fmt.Sprintf("ollama error (HTTP %d): %s", resp.StatusCode, errMsg),
	}
}

// sendRawRequest — отправка HTTP запроса к Ollama.
// Дополнительно: пробрасывает X-Request-ID из контекста (если он был
// установлен в ServeHTTP) — чтобы cppworker мог логировать тот же
// request_id и можно было проследить всю цепочку по одному grep'у.
func (mm *ModelManager) sendRawRequest(method, url string, body interface{}) (*http.Response, error) {
	return mm.sendRawRequestWithTokenR307(method, url, body, "")
}

// sendRawRequestWithTokenR307 — тот же вызов с токеном административной ручки.
//
// R307: unload/delete у воркера закрыты authMiddleware. Пустой token означает
// «у бэкенда токена нет»: заголовок не ставится, воркер без токена пропустит,
// воркер с токеном ответит 401 — и это будет видно, а не проглочено.
func (mm *ModelManager) sendRawRequestWithTokenR307(method, url string, body interface{}, token string) (*http.Response, error) {
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request body: %w", err)
	}

	req, err := http.NewRequest(method, url, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if strings.TrimSpace(token) != "" {
		req.Header.Set("X-API-Token", token)
	}

	// Пробрасываем request_id из ctx, если он есть.
	if rid := RequestIDFromContext(req.Context()); rid != "" {
		req.Header.Set(requestIDHeader, rid)
	}

	// Подробное логирование: до запроса и после, с duration и status.
	start := time.Now()
	resp, err := mm.client.Do(req)
	durationMs := time.Since(start).Milliseconds()
	if err != nil {
		logger.Get().Debugw("sendRawRequest: HTTP error",
			"method", method, "url", url,
			"duration_ms", durationMs, "error", err)
		return nil, err
	}
	logger.Get().Debugw("sendRawRequest: HTTP response",
		"method", method, "url", url,
		"status_code", resp.StatusCode,
		"duration_ms", durationMs)
	return resp, nil
}

// ===== Вспомогательные методы =====

// tryAcquireOp — пытается зарегистрировать операцию. Возвращает false, если для той же
// пары (op, modelName, backendID) уже есть активная запись младше modelOpTTL.
// Записи старше modelOpTTL считаются «залипшими» и автоматически перезаписываются —
// это защищает от блокировки при network-fail, когда releaseOp() не был вызван.
func (mm *ModelManager) tryAcquireOp(operation, modelName, backendID string) bool {
	mm.mu.Lock()
	defer mm.mu.Unlock()

	now := time.Now()
	key := operation + ":" + modelName
	if backends, exists := mm.activeOps[key]; exists {
		if startedAt, running := backends[backendID]; running {
			if now.Sub(startedAt) < modelOpTTL {
				return false
			}
			// Залипшая запись — перезаписываем.
		}
		backends[backendID] = now
	} else {
		mm.activeOps[key] = map[string]time.Time{backendID: now}
	}
	return true
}

func (mm *ModelManager) releaseOp(operation, modelName, backendID string) {
	mm.mu.Lock()
	defer mm.mu.Unlock()

	key := operation + ":" + modelName
	if backends, exists := mm.activeOps[key]; exists {
		delete(backends, backendID)
		if len(backends) == 0 {
			delete(mm.activeOps, key)
		}
	}
}

func operationDisplayName(op string) string {
	switch op {
	case "pull":
		return "pulled"
	case "push":
		return "pushed"
	case "delete":
		return "deleted"
	case "load":
		return "loaded into memory"
	case "unload":
		return "unloaded from memory"
	default:
		return op + "ed"
	}
}

// GetBackendModelManager — возвращает ModelManager из Proxy
func (p *Proxy) GetModelManager() *ModelManager {
	return p.modelManager
}

// SetModelManager — устанавливает ModelManager в Proxy (вызывается при инициализации)
func (p *Proxy) SetModelManager(mm *ModelManager) {
	p.modelManager = mm
}

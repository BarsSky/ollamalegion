// nctx_reload_progress.go — R634 (2026-11-19): асинхронная загрузка: разбор ответа, URL прогресса, опрос прогресса.
//
// ЗАЧЕМ ВЫНЕСЕНО. `nctx_reload.go` держал 1865 строк: типы и конфигурацию,
// состояние координатора, решения о перезагрузке, HTTP-клиент, длинный DoReload,
// метрики и опрос прогресса. Здесь — один из слоёв; поведение не менялось.
package balancer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"ollama-loadbalancer/pkg/logger"
	"strings"
	"time"
)

// asyncLoadResponse — минимальная структура для парсинга 202 response.
type asyncLoadResponse struct {
	Model struct {
		State        string `json:"state"`
		ContextSize  int    `json:"contextSize"`
		Context_size int    `json:"context_size"`
	} `json:"model"`
	ProgressURL string `json:"progressUrl"`
	Status      string `json:"status"`
	Error       string `json:"error"`
}

// extractProgressURL парсит тело 202 response от cppworker и возвращает
// абсолютный URL progress endpoint. progressUrl может быть:
//   - абсолютный (http://...): используем как есть
//   - относительный (/api/...): склеиваем с backendAddr
func extractProgressURL(body []byte, backendAddr string) (string, error) {
	var resp asyncLoadResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("parse 202 response: %w", err)
	}
	if resp.ProgressURL == "" {
		return "", fmt.Errorf("no progressUrl in 202 body")
	}
	// Если относительный — склеить с backendAddr.
	if strings.HasPrefix(resp.ProgressURL, "http://") || strings.HasPrefix(resp.ProgressURL, "https://") {
		return resp.ProgressURL, nil
	}
	// backendAddr заканчивается на :port, не на /. progressUrl начинается с /.
	return strings.TrimRight(backendAddr, "/") + resp.ProgressURL, nil
}

// progressResponse — ответ от /api/models/load/progress.
type progressResponse struct {
	State        string `json:"state"`
	ContextSize  int    `json:"contextSize"`
	Context_size int    `json:"context_size"`
	NCtx         int    `json:"n_ctx"`
	Model        struct {
		State        string `json:"state"`
		ContextSize  int    `json:"contextSize"`
		Context_size int    `json:"context_size"`
	} `json:"model"`
	Error string `json:"error"`
}

// pollLoadProgress поллит cppworker progress endpoint пока
// state != "loading" или пока не исчерпается timeout.
//
// Возвращает body синтетического 200-ответа с моделью state,
// совместимое с downstream парсером (json с contextSize/NCtx).
func pollLoadProgress(ctx context.Context, loader NCtxReloadHTTPClient, backendAddr, progressURL string, maxWait time.Duration, backendID string) ([]byte, error) {
	pollInterval := 2 * time.Second
	deadline := time.Now().Add(maxWait)
	attempt := 0
	for {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("context cancelled during poll: %w", ctx.Err())
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("poll timeout after %v (backend %s)", maxWait, backendID)
		}
		attempt++
		// Используем GET через тот же loader (с auth header).
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, progressURL, nil)
		if err != nil {
			return nil, fmt.Errorf("poll request build: %w", err)
		}
		if loader != nil {
			if rc, ok := loader.(*DefaultNCtxReloadHTTPClient); ok {
				if rc.APIToken != "" {
					headerName := rc.HeaderName
					if headerName == "" {
						headerName = "X-API-Token"
					}
					req.Header.Set(headerName, rc.APIToken)
				}
			}
		}
		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			logger.Get().Warnw("poll: HTTP error, retrying", "backend", backendID, "error", err, "attempt", attempt)
			select {
			case <-time.After(pollInterval):
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		var pr progressResponse
		if err := json.Unmarshal(body, &pr); err != nil {
			logger.Get().Warnw("poll: body parse error, retrying", "backend", backendID, "error", err, "attempt", attempt)
			select {
			case <-time.After(pollInterval):
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		// state может быть в top-level или в model.state.
		state := pr.State
		if state == "" {
			state = pr.Model.State
		}
		// error message — только в top-level (model в progress response не имеет Error).
		errMsg := pr.Error
		if errMsg != "" {
			return nil, fmt.Errorf("cppworker load failed: %s", errMsg)
		}
		// state=loaded → success, формируем синтетический 200-ответ.
		if state == "loaded" {
			nCtx := pr.ContextSize
			if nCtx == 0 {
				nCtx = pr.Model.ContextSize
			}
			if nCtx == 0 {
				nCtx = pr.NCtx
			}
			synthetic := fmt.Sprintf(`{"status":"loaded","context_size":%d,"n_ctx":%d,"model":{"state":"loaded","contextSize":%d,"context_size":%d}}`,
				nCtx, nCtx, nCtx, nCtx)
			logger.Get().Infow("poll: model loaded",
				"backend", backendID, "n_ctx", nCtx, "attempts", attempt)
			return []byte(synthetic), nil
		}
		// state=error или что-то другое — fail.
		if state == "error" {
			return nil, fmt.Errorf("cppworker load entered error state (backend %s)", backendID)
		}
		// state=loading — продолжаем поллить.
		if attempt == 1 || attempt%10 == 0 {
			logger.Get().Debugw("poll: model still loading",
				"backend", backendID, "state", state, "attempt", attempt)
		}
		select {
		case <-time.After(pollInterval):
			continue
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// CppWorker auto-registration в балансировщике
//
// Позволяет cppworker при старте:
//  1. Зарегистрировать себя в балансировщике через POST /api/v1/backends
//  2. Поддерживать heartbeat (best-effort)
//  3. При SIGTERM — best-effort DELETE /api/v1/backends/{id}
//
// ENV-переменные:
//   CPPWORKER_BALANCER_URL            — URL балансировщика (например http://loadbalancer:18081).
//                                       Если пусто — auto-registration отключён, cppworker
//                                       работает в standalone-режиме.
//   CPPWORKER_BALANCER_TOKEN          — API-токен для авторизации (X-API-Token).
//                                       Должен совпадать с токеном в balancer config.json.
//   CPPWORKER_ADVERTISE_HOST          — Host, под которым cppworker виден из балансировщика
//                                       (по умолчанию = os.Hostname()).
//   CPPWORKER_ADVERTISE_PORT          — Port (по умолчанию = cfg.Port).
//   CPPWORKER_REGISTER_NAME           — Имя бэкенда в балансировщике (по умолчанию
//                                       "cppworker-<hostname>"). Используется как
//                                       уникальный ID в балансировщике.
//   CPPWORKER_REGISTER_RETRY_INTERVAL — Интервал retry (default 30s).
//   CPPWORKER_REGISTER_HEARTBEAT      — Интервал heartbeat (default 60s). 0 = отключён.
//   CPPWORKER_REGISTER_MAX_RETRIES    — Максимум попыток регистрации (default 0 = бесконечно).
//   CPPWORKER_REGISTER_DISABLE        — "true" | "1" — полностью отключить Go-side
//                                       auto-registration (используется bundled-стеком,
//                                       где регистрацию делает shell-script
//                                       register-with-balancer.sh, иначе получается
//                                       два бэкенда с разными id, но одним физическим
//                                       cppworker'ом). Default: false (enabled, если
//                                       CPPWORKER_BALANCER_URL задан).
//   CPPWORKER_REGISTER_GPU_MODE       — "auto" | "gpu" | "cpu" (default "auto").

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"ollama-loadbalancer/internal/cppbackend"
)

// balancerRegistration — состояние auto-registration в балансировщике.
type balancerRegistration struct {
	enabled       bool
	balancerURL   string
	balancerToken string
	// Round 7 (2026-07-09): API token cppworker'а для авторизации balancer'а
	// на /api/models/reload. cppworker пробрасывает его в balancer при
	// регистрации через поле CppWorkerApiToken в registerPayload.
	apiTokenForCppWorker string
	advertiseHost        string
	advertisePort        int
	backendID            string
	gpuMode              string
	retryInterval        time.Duration
	heartbeat            time.Duration
	maxRetries           int
	registered           atomic.Bool
	stopCh               chan struct{}
	client               *http.Client
}

// isRegisterDisabled проверяет env-флаг CPPWORKER_REGISTER_DISABLE.
// Используется bundled-стеком, где регистрацию делает shell-script
// register-with-balancer.sh, а Go-side регистрация отключается чтобы
// не плодить дубликаты бэкендов.
func isRegisterDisabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("CPPWORKER_REGISTER_DISABLE")))
	return v == "true" || v == "1" || v == "yes" || v == "on"
}

func newBalancerRegistration(cfg *cppbackend.Config) *balancerRegistration {
	url := strings.TrimRight(os.Getenv("CPPWORKER_BALANCER_URL"), "/")
	if url == "" {
		return nil
	}
	if isRegisterDisabled() {
		return nil
	}
	token := os.Getenv("CPPWORKER_BALANCER_TOKEN")
	if token == "" {
		token = os.Getenv("LB_API_TOKEN")
	}

	host := os.Getenv("CPPWORKER_ADVERTISE_HOST")
	if host == "" {
		if h, err := os.Hostname(); err == nil {
			host = h
		} else {
			host = "cppworker"
		}
	}

	port := cfg.Port
	if v := os.Getenv("CPPWORKER_ADVERTISE_PORT"); v != "" {
		var p int
		if _, err := fmt.Sscanf(v, "%d", &p); err == nil && p > 0 {
			port = p
		}
	}

	name := os.Getenv("CPPWORKER_REGISTER_NAME")
	if name == "" {
		name = fmt.Sprintf("cppworker-%s", host)
	}

	retryInterval := 30 * time.Second
	if v := os.Getenv("CPPWORKER_REGISTER_RETRY_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			retryInterval = d
		}
	}

	heartbeat := 60 * time.Second
	if v := os.Getenv("CPPWORKER_REGISTER_HEARTBEAT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			heartbeat = d
		}
	}

	maxRetries := 0
	if v := os.Getenv("CPPWORKER_REGISTER_MAX_RETRIES"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n >= 0 {
			maxRetries = n
		}
	}

	gpuMode := os.Getenv("CPPWORKER_REGISTER_GPU_MODE")
	if gpuMode == "" {
		gpuMode = "auto"
	}

	return &balancerRegistration{
		enabled:       true,
		balancerURL:   url,
		balancerToken: token,
		// Round 7: cppworker's own API_TOKEN. Это то, что cppworker ожидает
		// на защищённых endpoints (например /api/models/reload). Совпадает с
		// API_TOKEN, CPPWORKER_API_TOKEN или LB_API_TOKEN env-переменной.
		apiTokenForCppWorker: resolveAPIToken(),
		advertiseHost:        host,
		advertisePort:        port,
		backendID:            name,
		gpuMode:              gpuMode,
		retryInterval:        retryInterval,
		heartbeat:            heartbeat,
		maxRetries:           maxRetries,
		stopCh:               make(chan struct{}),
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// registerPayload — JSON-тело для POST /api/v1/backends.
type registerPayload struct {
	ID                string   `json:"id"`
	Name              string   `json:"name"`
	Host              string   `json:"host"`
	CppWorkerPort     int      `json:"cppWorkerPort"`
	BackendType       string   `json:"backendType"`
	GPUMode           string   `json:"gpuMode"`
	Weight            int      `json:"weight"`
	MaxConcurrentReqs int      `json:"maxConcurrentRequests"`
	MaxModels         int      `json:"maxModels"`
	Labels            []string `json:"labels"`
	// Round 7 (2026-07-09): cppworker пробрасывает свой API_TOKEN balancer'у,
	// чтобы тот мог авторизоваться на /api/models/reload (authMiddleware).
	// В bundled-режиме это устраняет необходимость вручную настраивать
	// CppWorkerApiToken в конфиге каждого backend'а.
	CppWorkerApiToken string `json:"cppWorkerApiToken,omitempty"`
}

// register отправляет POST /api/v1/backends.
func (r *balancerRegistration) register(ctx context.Context, log *zap.SugaredLogger) (int, error) {
	payload := registerPayload{
		ID:                r.backendID,
		Name:              r.backendID,
		Host:              r.advertiseHost,
		CppWorkerPort:     r.advertisePort,
		BackendType:       "llama_cpp",
		GPUMode:           r.gpuMode,
		Weight:            100,
		MaxConcurrentReqs: 4,
		MaxModels:         4,
		Labels:            []string{"cppworker", "auto-registered"},
		// Round 7: cppworker отдаёт свой API_TOKEN balancer'у для последующего
		// apply профилей (reloadModelOnCppWorker использует этот токен).
		// Берём из того же источника, что и для авторизации на balancer
		// (CPPWORKER_BALANCER_TOKEN → API_TOKEN → LB_API_TOKEN).
		CppWorkerApiToken: r.apiTokenForCppWorker,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("marshal: %w", err)
	}

	url := r.balancerURL + "/api/v1/backends"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if r.balancerToken != "" {
		req.Header.Set("X-API-Token", r.balancerToken)
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusConflict {
		// 409 = backend with this ID already exists. Это OK — он мог быть зарегистрирован ранее.
		// Воспринимаем как успех, balancer знает про этот backend.
		log.Infow("cppworker already registered (id conflict, treated as ok)",
			"id", r.backendID, "status", resp.StatusCode)
		return resp.StatusCode, nil
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		log.Infow("cppworker registered with balancer",
			"id", r.backendID, "status", resp.StatusCode)
		return resp.StatusCode, nil
	}

	// Прочитаем тело ответа для диагностики
	var respBody bytes.Buffer
	_, _ = respBody.ReadFrom(resp.Body)
	return resp.StatusCode, fmt.Errorf("balancer returned status=%d body=%s",
		resp.StatusCode, respBody.String())
}

// unregister — best-effort DELETE /api/v1/backends/{id}.
func (r *balancerRegistration) unregister(ctx context.Context, log *zap.SugaredLogger) {
	if !r.registered.Load() {
		return
	}
	url := fmt.Sprintf("%s/api/v1/backends/%s", r.balancerURL, r.backendID)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		log.Warnw("unregister: build request failed", "error", err)
		return
	}
	if r.balancerToken != "" {
		req.Header.Set("X-API-Token", r.balancerToken)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		log.Warnw("unregister: request failed", "id", r.backendID, "error", err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		log.Infow("cppworker unregistered from balancer", "id", r.backendID)
	} else {
		log.Warnw("unregister: non-2xx", "id", r.backendID, "status", resp.StatusCode)
	}
}

// notifyModelLoaded — best-effort callback в балансировщик после успешной
// загрузки модели. Позволяет балансировщику обновить кэш loadedModels
// немедленно, а не ждать 30-секундного /api/models poll.
//
// Endpoint: POST /api/v1/internal/llama-model-loaded
// Body: {"backendId": "...", "model": "...", "sizeBytes": 12345,
//
//	"path": "/app/models/foo.gguf", "quantization": "Q4_K_M",
//	"contextSize": 4096, "gpuLayers": -1, "kvCacheType": "f16",
//	"flashAttnType": -1, "useMmap": true}
//
// Вызывается fire-and-forget; ошибки логируются и не влияют на основной поток.
//
// Round 32 #4 fix (2026-08-10): убрал проверку `!r.registered.Load()` (раньше
// callback слался ТОЛЬКО если cppworker сам зарегистрировался в балансировщике).
// В bundled-конфиге CPPWORKER_REGISTER_DISABLE=true, cppworker не регистрируется
// сам — за него это делает Ollama-agent (`cppworker-gpu-bundled-agent` backend).
// Без этого фикса callback никогда не отправлялся → WebUI monitor ждал 30s
// poll'а от llamaCppMetricsPoller'а чтобы увидеть новую загруженную модель.
//
// Round 34 (2026-08-12) Phase 2: добавлены runtime params (kvCacheType,
// flashAttnType, useMmap) для profile mismatch detection в balancer preflight.
// Теперь callback шлётся когда указан balancerURL (независимо от
// CPPWORKER_REGISTER_DISABLE). Backend ID в payload'е (`r.backendID`)
// совпадает с ID agent-registered backend'а (тот же env var CPPWORKER_BACKEND_ID),
// так что балансировщик корректно мерджит callback в metrics.
//
// R60.4 (2026-09-04): webui meta — добавлены path (полный путь к .gguf, чтобы
// webui знал имя файла) и quantization (parseQuantization(path) — Q4_K_M и т.д.).
// До R60.4 webui карточка модели показывала пустые "Размер: -" и "Квантизация: -"
// потому что notifyModelLoaded не передавал эти поля, а /api/models (который
// balancer poll'ит) тоже их не отдавал.
func (r *balancerRegistration) notifyModelLoaded(
	modelName string,
	modelPath string,
	sizeBytes uint64,
	contextSize, gpuLayers int,
	kvCacheType string,
	flashAttnType int,
	useMmap bool,
) {
	if r == nil || r.balancerURL == "" {
		return
	}
	// R60.4 fallback: sizeBytes иногда 0 (cppbackend берёт из gguf meta).
	// os.Stat на path даёт реальный file size.
	effectiveSize := sizeBytes
	if effectiveSize == 0 && modelPath != "" {
		if fi, statErr := os.Stat(modelPath); statErr == nil {
			effectiveSize = uint64(fi.Size())
		}
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		payload := map[string]interface{}{
			"backendId":     r.backendID,
			"model":         modelName,
			"path":          modelPath,
			"sizeBytes":     effectiveSize,
			"size":          effectiveSize, // R60.4: alias for webui gguf-renderer-detail.js:232
			"quantization":  parseQuantization(modelPath), // R60.4
			"contextSize":   contextSize,
			"gpuLayers":     gpuLayers,
			"kvCacheType":   kvCacheType,
			"flashAttnType": flashAttnType,
			"useMmap":       useMmap,
		}
		body, err := json.Marshal(payload)
		if err != nil {
			//nolint:staticcheck // logger.Get() из pkg/logger — пакетный логгер приложения.
			packageLogger().Warnw("notifyModelLoaded: marshal failed", "error", err)
			return
		}

		url := r.balancerURL + "/api/v1/internal/llama-model-loaded"
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		if r.balancerToken != "" {
			req.Header.Set("X-API-Token", r.balancerToken)
		}

		resp, err := r.client.Do(req)
		if err != nil {
			packageLogger().Debugw("notifyModelLoaded: request failed",
				"model", modelName, "error", err)
			return
		}
		resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			packageLogger().Debugw("notifyModelLoaded: ok",
				"model", modelName, "sizeBytes", sizeBytes)
		} else {
			packageLogger().Debugw("notifyModelLoaded: non-2xx",
				"model", modelName, "status", resp.StatusCode)
		}
	}()
}

// notifyModelUnloaded — best-effort callback в балансировщик после выгрузки
// модели. Без этого lastKnownNCtx в NCtxReloadCoordinator остаётся прежним
// (после `idle_unload_after` 10m), preflight думает модель загружена с
// большим n_ctx, не триггерит reload → пользователь получает 502 connection
// refused от cppworker (модель на самом деле не загружена).
//
// Round 34 (2026-08-12) Phase 3.
//
// Endpoint: POST /api/v1/internal/llama-model-unloaded
// Body: {"backendId": "...", "model": "..."}
func (r *balancerRegistration) notifyModelUnloaded(modelName string) {
	if r == nil || r.balancerURL == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		payload := map[string]interface{}{
			"backendId": r.backendID,
			"model":     modelName,
		}
		body, err := json.Marshal(payload)
		if err != nil {
			packageLogger().Warnw("notifyModelUnloaded: marshal failed", "error", err)
			return
		}

		url := r.balancerURL + "/api/v1/internal/llama-model-unloaded"
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		if r.balancerToken != "" {
			req.Header.Set("X-API-Token", r.balancerToken)
		}

		resp, err := r.client.Do(req)
		if err != nil {
			packageLogger().Debugw("notifyModelUnloaded: request failed",
				"model", modelName, "error", err)
			return
		}
		resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			packageLogger().Debugw("notifyModelUnloaded: ok",
				"model", modelName)
		} else {
			packageLogger().Debugw("notifyModelUnloaded: non-2xx",
				"model", modelName, "status", resp.StatusCode)
		}
	}()
}

// heartbeatLoop — периодически обновляет статус через PUT /api/v1/backends/{id}.
// По факту используем GET (read-only) как liveness probe — балансировщик сам
// периодически опрашивает cppworker через /health.
func (r *balancerRegistration) heartbeatLoop(ctx context.Context, log *zap.SugaredLogger) {
	if r.heartbeat <= 0 {
		return
	}
	t := time.NewTicker(r.heartbeat)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.stopCh:
			return
		case <-t.C:
			if !r.registered.Load() {
				continue
			}
			// HEAD /health на наш собственный endpoint — sanity check.
			healthURL := fmt.Sprintf("http://%s:%d/health", resolveSelfHost(r.advertiseHost), r.advertisePort)
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
			if err != nil {
				continue
			}
			resp, err := r.client.Do(req)
			if err != nil {
				log.Debugw("heartbeat: local /health check failed", "error", err)
				continue
			}
			resp.Body.Close()
		}
	}
}

// resolveSelfHost — если advertiseHost = 0.0.0.0, подменяем на 127.0.0.1
// для локальных health checks.
func resolveSelfHost(host string) string {
	if host == "" || host == "0.0.0.0" || host == "::" {
		return "127.0.0.1"
	}
	return host
}

// start — запускает goroutine регистрации.
func (r *balancerRegistration) start(ctx context.Context, log *zap.SugaredLogger) {
	if !r.enabled {
		return
	}

	// Проверяем резолвится ли balancer URL — это быстрая sanity check.
	addrs, err := net.DefaultResolver.LookupHost(ctx, extractHost(r.balancerURL))
	if err != nil {
		log.Warnw("balancer host not resolvable, cppworker will work in standalone mode",
			"balancerURL", r.balancerURL, "error", err, "addresses", addrs)
	} else {
		log.Infow("balancer DNS resolved", "balancerURL", r.balancerURL, "addresses", addrs)
	}

	log.Infow("starting balancer auto-registration",
		"balancerURL", r.balancerURL,
		"backendID", r.backendID,
		"advertiseHost", r.advertiseHost,
		"advertisePort", r.advertisePort,
		"retryInterval", r.retryInterval,
		"heartbeat", r.heartbeat)

	go r.run(ctx, log)
	go r.heartbeatLoop(ctx, log)
}

func (r *balancerRegistration) run(ctx context.Context, log *zap.SugaredLogger) {
	attempts := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.stopCh:
			return
		default:
		}

		code, err := r.register(ctx, log)
		if err == nil {
			r.registered.Store(true)
			// После успешной регистрации — крутим heartbeatLoop,
			// а эта горутина уходит в сон, пока работает.
			<-r.stopCh
			return
		}

		attempts++
		if r.maxRetries > 0 && attempts >= r.maxRetries {
			log.Warnw("auto-registration: max retries exceeded, cppworker works in standalone mode",
				"attempts", attempts, "lastError", err)
			return
		}

		log.Warnw("auto-registration failed, will retry",
			"attempt", attempts,
			"retryInterval", r.retryInterval,
			"status", code,
			"error", err)

		select {
		case <-ctx.Done():
			return
		case <-r.stopCh:
			return
		case <-time.After(r.retryInterval):
		}
	}
}

func (r *balancerRegistration) stop(ctx context.Context, log *zap.SugaredLogger) {
	if r == nil {
		return
	}
	close(r.stopCh)
	r.unregister(ctx, log)
}

func extractHost(url string) string {
	url = strings.TrimPrefix(url, "http://")
	url = strings.TrimPrefix(url, "https://")
	if i := strings.Index(url, "/"); i >= 0 {
		url = url[:i]
	}
	if i := strings.Index(url, ":"); i >= 0 {
		return url[:i]
	}
	return url
}

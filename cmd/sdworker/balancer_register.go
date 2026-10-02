// balancer_register.go — авторегистрация sdworker в балансере.
//
// ОБРАЗЕЦ: cmd/cppworker/balancer_register.go (проверенный контракт
// POST /api/v1/backends + heartbeat + best-effort DELETE при SIGTERM).
//
// ОТЛИЧИЯ ДЛЯ IMAGE-БЭКЕНДА:
//   - backendType: "image_cpp" (не "llama_cpp");
//   - порт сообщается в поле imagePort (CppWorkerPort/OllamaPort не заполняем —
//     у image-воркера нет Ollama- и llama.cpp-поверхности);
//   - maxConcurrentRequests = размер НАШЕЙ очереди (64): движок всё равно
//     исполняет по одной, но воркер принимает и ставит в очередь — балансер
//     должен видеть реальную вместимость, а не 1 (иначе он сам начнёт
//     отбрасывать запросы, хотя мы готовы их принять);
//   - labels: ["sdworker", "auto-registered", "image"].
//
// ENV-контракт (§D задачи):
//
//	SDWORKER_BALANCER_URL       — URL балансера; пусто = регистрация выключена
//	SDWORKER_BALANCER_TOKEN     — токен (X-API-Token)
//	SDWORKER_BACKEND_ID         — id бэкенда в балансере (default sdworker-<host>)
//	SDWORKER_ADVERTISE_HOST     — host, под которым воркер виден балансеру
//	SDWORKER_ADVERTISE_PORT     — порт (default cfg.Port)
//	SDWORKER_REGISTER_DISABLE   — true/1/yes/on = выключить (bundled-стек)
//	SDWORKER_REGISTER_RETRY_INTERVAL / _HEARTBEAT / _MAX_RETRIES
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"ollama-loadbalancer/internal/sdbackend"
)

// registerPayload — JSON-тело POST /api/v1/backends (см. internal/api/handlers_backends.go:476).
type registerPayload struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Host  string `json:"host"`
	// ImagePort — порт image-воркера: именно его использует EffectiveImagePort().
	ImagePort         int      `json:"imagePort"`
	BackendType       string   `json:"backendType"`
	GPUMode           string   `json:"gpuMode"`
	Weight            int      `json:"weight"`
	MaxConcurrentReqs int      `json:"maxConcurrentRequests"`
	MaxModels         int      `json:"maxModels"`
	Labels            []string `json:"labels"`
}

// balancerRegistration — состояние авторегистрации.
type balancerRegistration struct {
	balancerURL   string
	balancerToken string
	advertiseHost string
	advertisePort int
	backendID     string
	gpuMode       string
	concurrent    int
	retryInterval time.Duration
	heartbeat     time.Duration
	maxRetries    int
	registered    atomic.Bool
	stopCh        chan struct{}
	client        *http.Client
}

// isRegisterDisabled — SDWORKER_REGISTER_DISABLE.
func isRegisterDisabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("SDWORKER_REGISTER_DISABLE")))
	return v == "true" || v == "1" || v == "yes" || v == "on"
}

// newBalancerRegistration — nil, если регистрация выключена/не настроена.
func newBalancerRegistration(cfg *sdbackend.Config) *balancerRegistration {
	url := strings.TrimRight(strings.TrimSpace(os.Getenv("SDWORKER_BALANCER_URL")), "/")
	if url == "" || isRegisterDisabled() {
		return nil
	}
	token := os.Getenv("SDWORKER_BALANCER_TOKEN")
	if token == "" {
		token = os.Getenv("BALANCER_API_TOKEN")
	}
	host := strings.TrimSpace(os.Getenv("SDWORKER_ADVERTISE_HOST"))
	if host == "" {
		if h, err := os.Hostname(); err == nil {
			host = h
		} else {
			host = "sdworker"
		}
	}
	port := cfg.Port
	if v := strings.TrimSpace(os.Getenv("SDWORKER_ADVERTISE_PORT")); v != "" {
		if p, err := strconv.Atoi(v); err == nil && p > 0 && p <= 65535 {
			port = p
		}
	}
	id := strings.TrimSpace(os.Getenv("SDWORKER_BACKEND_ID"))
	if id == "" {
		id = fmt.Sprintf("sdworker-%s", host)
	}
	retry := 30 * time.Second
	if v := strings.TrimSpace(os.Getenv("SDWORKER_REGISTER_RETRY_INTERVAL")); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			retry = d
		}
	}
	hb := 60 * time.Second
	if v := strings.TrimSpace(os.Getenv("SDWORKER_REGISTER_HEARTBEAT")); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			hb = d
		}
	}
	maxRetries := 0
	if v := strings.TrimSpace(os.Getenv("SDWORKER_REGISTER_MAX_RETRIES")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			maxRetries = n
		}
	}
	gpuMode := strings.TrimSpace(os.Getenv("SDWORKER_REGISTER_GPU_MODE"))
	if gpuMode == "" {
		// image-воркер почти всегда GPU-ориентирован (Vulkan/CUDA), но
		// CPU-сборка sd.cpp тоже бывает — оставляем auto.
		gpuMode = "auto"
	}
	concurrent := cfg.MaxConcurrent
	if concurrent < 1 {
		concurrent = 1
	}
	return &balancerRegistration{
		balancerURL: url, balancerToken: token,
		advertiseHost: host, advertisePort: port, backendID: id,
		gpuMode: gpuMode, concurrent: concurrent,
		retryInterval: retry, heartbeat: hb, maxRetries: maxRetries,
		stopCh: make(chan struct{}),
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

// register — POST /api/v1/backends.
func (r *balancerRegistration) register(ctx context.Context, log *zap.SugaredLogger) error {
	payload := registerPayload{
		ID:                r.backendID,
		Name:              r.backendID,
		Host:              r.advertiseHost,
		ImagePort:         r.advertisePort,
		BackendType:       "image_cpp",
		GPUMode:           r.gpuMode,
		Weight:            100,
		MaxConcurrentReqs: r.concurrent,
		MaxModels:         1, // одна модель на процесс sd-server (hot-swap нет)
		Labels:            []string{"sdworker", "auto-registered", "image"},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.balancerURL+"/api/v1/backends", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if r.balancerToken != "" {
		req.Header.Set("X-API-Token", r.balancerToken)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// 409 = бэкенд с таким ID уже есть. Для image-воркера это НОРМАЛЬНЫЙ путь
	// повторного старта: isSameBackendRegistration (internal/api:379) требует
	// непустой cppWorkerPort/ollamaPort, которых у нас нет, поэтому балансер
	// отвечает 409 и НЕ обновляет запись. Это осознанный компромисс без правок
	// в запретной зоне (см. отчёт): при смене порта/хоста оператор удаляет
	// бэкенд в WebUI (DELETE /api/v1/backends/{id}) — либо мы делаем это сами
	// перед регистрацией, если SDK сообщает конфликт.
	if resp.StatusCode == http.StatusConflict {
		log.Infow("sdworker already registered (409 conflict, treated as ok)",
			"id", r.backendID, "hint", "если изменились host/port — удалите бэкенд в балансере и перезапустите воркер")
		return nil
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		log.Infow("sdworker registered with balancer",
			"id", r.backendID, "host", r.advertiseHost, "image_port", r.advertisePort,
			"backendType", "image_cpp", "max_concurrent", r.concurrent)
		return nil
	}
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return fmt.Errorf("balancer returned status=%d body=%s", resp.StatusCode, buf.String())
}

// start — регистрация с ретраями + heartbeat.
func (r *balancerRegistration) start(ctx context.Context, log *zap.SugaredLogger) {
	go func() {
		attempts := 0
		for {
			rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			err := r.register(rctx, log)
			cancel()
			if err == nil {
				r.registered.Store(true)
				break
			}
			attempts++
			log.Warnw("sdworker registration failed, will retry",
				"attempt", attempts, "retry_in", r.retryInterval.String(), "error", err)
			if r.maxRetries > 0 && attempts >= r.maxRetries {
				log.Errorw("sdworker registration gave up", "attempts", attempts)
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(r.retryInterval):
			}
		}
		if r.heartbeat <= 0 {
			return
		}
		ticker := time.NewTicker(r.heartbeat)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-r.stopCh:
				return
			case <-ticker.C:
				hctx, cancel := context.WithTimeout(ctx, 10*time.Second)
				err := r.heartbeatOnce(hctx)
				cancel()
				if err != nil {
					log.Debugw("sdworker heartbeat failed", "error", err)
				}
			}
		}
	}()
}

// heartbeatOnce — best-effort heartbeat.
//
// Балансер (как и cppworker) не имеет отдельного /heartbeat-эндпоинта для
// самозарегистрированных бэкендов: heartbeat = повторная регистрация
// (идемпотентная) либо GET бэкенда. Используем GET — он не провоцирует
// конфликтов и обновляет LastSeen на стороне балансера.
func (r *balancerRegistration) heartbeatOnce(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		r.balancerURL+"/api/v1/backends/"+r.backendID, nil)
	if err != nil {
		return err
	}
	if r.balancerToken != "" {
		req.Header.Set("X-API-Token", r.balancerToken)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("heartbeat status=%d", resp.StatusCode)
	}
	return nil
}

// stop — best-effort DELETE /api/v1/backends/{id}.
func (r *balancerRegistration) stop(ctx context.Context, log *zap.SugaredLogger) {
	close(r.stopCh)
	if !r.registered.Load() {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		r.balancerURL+"/api/v1/backends/"+r.backendID, nil)
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
		log.Infow("sdworker unregistered from balancer", "id", r.backendID)
	} else {
		log.Warnw("unregister: non-2xx", "id", r.backendID, "status", resp.StatusCode)
	}
}

// Package balancer - Adaptive n_ctx auto-reload for llama.cpp inference.
//
// Phase 2 of the cppworker integration (see plan_mode_respond). When a
// request exceeds the model's loaded n_ctx, the balancer can either
// auto-reload the model with a larger n_ctx (at the cost of fewer GPU
// layers when VRAM is tight), or reject the request with HTTP 413 and a
// structured JSON error.
//
// Data flow:
//  1. C-bridge (c/bridge/bridge.c) detects n_ctx overflow, returns
//     BRIDGE_ERR_N_CTX_NEEDS_RELOAD (=2) and a BridgeErrorInfo payload.
//  2. Go bridge (c/bridge/bridge.go) translates this to the sentinel
//     ErrNCtxNeedsReload.
//  3. llamacpp_transport.go surfaces the sentinel in the streaming
//     path, fetches GetLastErrorInfo(), and calls DecideReloadBackend(...).
//  4. Decision = Reload: POST /api/models/reload to cppworker with new
//     n_ctx, reduced layers, then retry the original request transparently.
//  5. Decision = Reject: respond to client with 413/400 and JSON body.
//
// Decision rule (VRAM-aware, ignores small heuristics to avoid flapping):
//   - required_n_ctx <= max_vram_n_ctx * safety_factor (default 0.85) -> Reload.
//   - required_n_ctx >  max_vram_n_ctx * safety_factor                -> Reject.
//
// Concurrency safety:
//   - perBackend map guarded by sync.RWMutex.
//   - inflightMu serializes per-backend reload cycles (when 2 concurrent
//     requests hit n_ctx overflow they share one reload via a shared
//     channel, not duplicate work).
package balancer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

type NCtxBridgeError struct {
	Code         int
	CurrentNCtx  int
	RequiredNCtx int
	ActualTokens int
	NPredict     int
	NCtxOverride int
	MaxVRAMNCtx  int
	Message      string
}

const (
	NCtxErrCodeOK              = 0
	NCtxErrCodeGeneric         = 1
	NCtxErrCodeNCtxNeedsReload = 2
	NCtxErrCodePromptTooLong   = 3
	NCtxErrCodeGPUOOM          = 4
	NCtxErrCodeBadRequest      = 5
)

// ============================================================

// ============================================================

type NCtxReloadConfig struct {
	AutoReloadNCtx bool `json:"auto_reload_n_ctx" yaml:"auto_reload_n_ctx"`

	AutoReloadMaxNCtx int `json:"auto_reload_max_n_ctx" yaml:"auto_reload_max_n_ctx"`

	AutoReloadVRAMSafetyFactor float64 `json:"auto_reload_vram_safety_factor" yaml:"auto_reload_vram_safety_factor"`

	AutoReloadTimeoutSec int `json:"auto_reload_timeout_sec" yaml:"auto_reload_timeout_sec"`

	//

	AutoReloadAllowTools bool `json:"auto_reload_allow_tools" yaml:"auto_reload_allow_tools"`

	PreflightEnabled bool `json:"preflight_enabled" yaml:"preflight_enabled"`

	// Round 31 #2 (2026-08-09): async reload mode для preflight.
	// Если true — при решении PreflightReload balancer НЕ блокирует на reload,
	// а сразу отдаёт клиенту 503 + Retry-After (через PreflightAsync decision),
	// а reload запускается в фоне. Через Retry-After секунд Cline/OpenWebUI повторяет
	// запрос, и модель уже загружена с правильным n_ctx. Это убирает "truncated"
	// ошибки для длинных prompts (Cline 70K символов = ~17K токенов).
	// Default false для backward compat (старое sync-поведение).
	PreflightAsyncReload bool `json:"preflight_async_reload" yaml:"preflight_async_reload"`

	// Round 31 #2: сколько секунд balancer рекомендует клиенту подождать
	// перед retry после 503 async reload. Cline/OpenWebUI обычно это понимают.
	// Default 5 сек (reload обычно занимает 30-60s, но 5 — это нижняя граница,
	// иначе клиент будет retry-ить слишком часто).
	PreflightAsyncRetryAfterSec int `json:"preflight_async_retry_after_sec" yaml:"preflight_async_retry_after_sec"`
}

func DefaultNCtxReloadConfig() NCtxReloadConfig {
	return NCtxReloadConfig{
		AutoReloadNCtx:             true,
		AutoReloadMaxNCtx:          0,
		AutoReloadVRAMSafetyFactor: 0.85,
		AutoReloadTimeoutSec:       300,
		AutoReloadAllowTools:       true,
		PreflightEnabled:           true,
		PreflightAsyncReload:       false, // sync по умолчанию (старое поведение)
		PreflightAsyncRetryAfterSec: 5,
	}
}

func (c NCtxReloadConfig) AutoReloadAllowToolsEnabled() bool {
	return c.AutoReloadNCtx && c.AutoReloadAllowTools
}

func (c NCtxReloadConfig) effectiveSafetyFactor() float64 {
	v := c.AutoReloadVRAMSafetyFactor
	if v <= 0 || v > 1.0 {
		v = 0.85
	}
	if v < 0.1 {
		v = 0.1
	}
	return v
}

func (c NCtxReloadConfig) effectiveTimeout() time.Duration {
	t := c.AutoReloadTimeoutSec
	if t <= 0 {
		t = 300
	}
	if t < 30 {
		t = 30
	}
	if t > 600 {
		t = 600
	}
	return time.Duration(t) * time.Second
}

// effectiveAsyncRetryAfter — сколько секунд клиенту ждать после 503
// перед retry. Default 5, clamp [2, 30].
func (c NCtxReloadConfig) effectiveAsyncRetryAfter() int {
	r := c.PreflightAsyncRetryAfterSec
	if r <= 0 {
		r = 5
	}
	if r < 2 {
		r = 2
	}
	if r > 30 {
		r = 30
	}
	return r
}

// ============================================================

// ============================================================

type ReloadDecision int

const (
	DecisionNoOp ReloadDecision = iota

	DecisionReload

	DecisionReject
)

func (d ReloadDecision) String() string {
	switch d {
	case DecisionNoOp:
		return "noop"
	case DecisionReload:
		return "reload"
	case DecisionReject:
		return "reject"
	default:
		return fmt.Sprintf("unknown(%d)", int(d))
	}
}

type ReloadPlan struct {
	Decision  ReloadDecision
	NewNCtx   int
	Reason    string
	RejectMsg string
}

// ============================================================

// ============================================================

// ? llamacpp_transport.go.
type NCtxReloadCoordinator struct {
	mu         sync.RWMutex
	config     NCtxReloadConfig
	perBackend map[string]*backendReloadState

	metricsByBackend sync.Map

	// ensureModelLoadedOnBackend. nil-safe.
	reloadDedup *reloadDedupRegistry
}

type backendReloadState struct {
	inflightMu sync.Mutex
	inflight   *reloadInflight

	lastKnownNCtx int

	consecutiveCycleFailures int

	lastCycleReset time.Time
}

type reloadInflight struct {
	done chan struct{}
	err  error
}

func NewNCtxReloadCoordinator(cfg NCtxReloadConfig) *NCtxReloadCoordinator {
	return &NCtxReloadCoordinator{
		config:      cfg,
		perBackend:  make(map[string]*backendReloadState),
		reloadDedup: newReloadDedupRegistry(),
	}
}

func (c *NCtxReloadCoordinator) IsReloadPending(backendID, modelName string) bool {
	if c == nil || c.reloadDedup == nil {
		return false
	}
	return c.reloadDedup.IsReloadPending(backendID, modelName)
}

func (c *NCtxReloadCoordinator) WaitReloadDone(backendID, modelName string, timeout time.Duration) error {
	if c == nil || c.reloadDedup == nil {
		return nil
	}
	return c.reloadDedup.WaitReloadDone(backendID, modelName, timeout)
}

func (c *NCtxReloadCoordinator) Config() NCtxReloadConfig {
	return c.config
}

func (c *NCtxReloadCoordinator) SetConfig(cfg NCtxReloadConfig) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.config = cfg
}

func (c *NCtxReloadCoordinator) state(backendID string) *backendReloadState {
	c.mu.RLock()
	s, ok := c.perBackend[backendID]
	c.mu.RUnlock()
	if ok {
		return s
	}
	c.mu.Lock()
	s, ok = c.perBackend[backendID]
	if !ok {
		s = &backendReloadState{}
		c.perBackend[backendID] = s
	}
	c.mu.Unlock()
	return s
}

func (c *NCtxReloadCoordinator) SetLastKnownNCtx(backendID string, nCtx int) {
	if nCtx <= 0 {
		return
	}
	s := c.state(backendID)
	s.inflightMu.Lock()
	s.lastKnownNCtx = nCtx
	s.inflightMu.Unlock()
}

func (c *NCtxReloadCoordinator) LastKnownNCtx(backendID string) int {
	s := c.state(backendID)
	s.inflightMu.Lock()
	defer s.inflightMu.Unlock()
	return s.lastKnownNCtx
}

func (c *NCtxReloadCoordinator) RecordCycleAttempt(backendID string) {
	s := c.state(backendID)
	s.inflightMu.Lock()
	defer s.inflightMu.Unlock()
	s.consecutiveCycleFailures++
	s.lastCycleReset = time.Now()
}

func (c *NCtxReloadCoordinator) IsCycleDetected(backendID string, maxAttempts int) bool {
	s := c.state(backendID)
	s.inflightMu.Lock()
	defer s.inflightMu.Unlock()

	const cycleResetInterval = 5 * time.Minute
	if !s.lastCycleReset.IsZero() && time.Since(s.lastCycleReset) > cycleResetInterval {
		s.consecutiveCycleFailures = 0
		return false
	}
	return s.consecutiveCycleFailures >= maxAttempts
}

func (c *NCtxReloadCoordinator) ResetCycleCounter(backendID string) {
	s := c.state(backendID)
	s.inflightMu.Lock()
	defer s.inflightMu.Unlock()
	s.consecutiveCycleFailures = 0
	s.lastCycleReset = time.Now()
}

// ============================================================

// ============================================================

//

//

func (c *NCtxReloadCoordinator) DecideReloadBackend(
	backendID string,
	bridgeErr *NCtxBridgeError,
	requestedNCtxOverride int,
) *ReloadPlan {
	cfg := c.Config()

	// Kill-switch
	if !cfg.AutoReloadNCtx {
		return &ReloadPlan{
			Decision: DecisionNoOp,
			Reason:   "auto-reload disabled in config (auto_reload_n_ctx=false)",
		}
	}

	//

	//   current_n_ctx=32768, required_n_ctx=23816, n_ctx_override=16384
	//   prompt=15623 + n_predict=8192 + 1 = 23816 > 16384 (override) ? code 3
	//

	//

	if bridgeErr == nil {
		return &ReloadPlan{Decision: DecisionNoOp, Reason: "nil bridge error"}
	}

	if bridgeErr.Code == NCtxErrCodePromptTooLong {
		// BRIDGE_ERR_PROMPT_TOO_LONG (code 3) — prompt + n_predict exceeds current n_ctx.
		//
		// 2026-07-03 FIX: instead of rejecting immediately, try reload via adaptive strategy.
		// The adaptive loader can reduce gpuLayers to free VRAM for larger KV-cache,
		// enabling a larger n_ctx. Previously this was rejected because the model was
		// assumed to be at maximum n_ctx for the current gpuLayers.
		//
		// Calculate required n_ctx from actual prompt tokens + requested n_predict.
		required := bridgeErr.ActualTokens + bridgeErr.NPredict
		if required <= 0 {
			required = bridgeErr.RequiredNCtx
		}
		if required <= 0 {
			return c.makeRejectPlan(backendID, bridgeErr, required,
				"prompt_too_long: cannot determine required n_ctx from error info",
				"prompt_exceeds_context")
		}

		// Check configured max limit
		if cfg.AutoReloadMaxNCtx > 0 && required > cfg.AutoReloadMaxNCtx {
			return c.makeRejectPlan(backendID, bridgeErr, required,
				fmt.Sprintf("required n_ctx=%d exceeds configured max=%d", required, cfg.AutoReloadMaxNCtx),
				"prompt_exceeds_context")
		}

		// Allow reload even if required > max_vram_n_ctx (with current gpuLayers).
		// The adaptive strategy (queryAdaptiveStrategy in DoReload) will reduce
		// gpuLayers to free VRAM for the larger KV-cache.
		newNCtx := roundUpPow2(required)
		if cfg.AutoReloadMaxNCtx > 0 && newNCtx > cfg.AutoReloadMaxNCtx {
			newNCtx = cfg.AutoReloadMaxNCtx
		}

		return &ReloadPlan{
			Decision: DecisionReload,
			NewNCtx:  newNCtx,
			Reason: fmt.Sprintf("prompt_too_long: actual_tokens=%d n_predict=%d required=%d target=%d max_vram=%d",
				bridgeErr.ActualTokens, bridgeErr.NPredict, required, newNCtx, bridgeErr.MaxVRAMNCtx),
		}
	}

	if bridgeErr.Code != NCtxErrCodeNCtxNeedsReload {
		return &ReloadPlan{Decision: DecisionNoOp, Reason: "not a n_ctx-reload error"}

	}

	required := bridgeErr.RequiredNCtx
	if required <= 0 {
		required = requestedNCtxOverride
	}
	if required <= 0 {
		return &ReloadPlan{
			Decision: DecisionNoOp,
			Reason:   "no required n_ctx in error info (bridge bug?)",
		}
	}

	if bridgeErr.CurrentNCtx > 0 && required <= bridgeErr.CurrentNCtx {
		return &ReloadPlan{
			Decision: DecisionNoOp,
			Reason: fmt.Sprintf("required n_ctx=%d already covered by current=%d (no reload needed)",
				required, bridgeErr.CurrentNCtx),
		}
	}

	if cfg.AutoReloadMaxNCtx > 0 && required > cfg.AutoReloadMaxNCtx {
		return c.makeRejectPlan(backendID, bridgeErr, required,
			fmt.Sprintf("required n_ctx=%d exceeds configured max=%d", required, cfg.AutoReloadMaxNCtx),
			"n_ctx_too_large_for_backend")
	}

	//

	//      head_dim = n_embd / n_heads.

	//

	maxVRAMNCtx := bridgeErr.MaxVRAMNCtx
	if maxVRAMNCtx <= 0 {

		if maxVRAMNCtx == 0 && bridgeErr.CurrentNCtx > 0 {
			return c.makeRejectPlan(backendID, bridgeErr, required,
				"model fits in VRAM but no space remains for KV-cache (max_vram_n_ctx=0). "+
					"Try reducing n_gpu_layers (CPU-offload) to free VRAM for context, "+
					"or use a smaller model / increase physical VRAM",
				"n_ctx_too_large_for_backend")
		}

		return c.makeRejectPlan(backendID, bridgeErr, required,
			"backend did not report max_vram_n_ctx (CPU-only, unknown GPU, or CUDA-query failed); cannot safely auto-reload",
			"n_ctx_too_large_for_backend")
	}
	safety := cfg.effectiveSafetyFactor()
	safeMax := int(float64(maxVRAMNCtx) * safety)
	if required > safeMax {
		// Bug fix (Round 2): bridge.c рассчитывает max_vram_n_ctx для KV-cache f16
		// (4 bytes/token). q4_0 даёт 4x больше headroom (1 byte/token). Если
		// required укладывается в q4_0 + safety — пропускаем через reload:
		// cppworker /adaptive/strategy выберет q4_0 в DoReload
		// (см. nctx_reload.go:775 — queryAdaptiveStrategy).
		//
		// Также учитываем partial offload: если cfg.AutoReloadMaxNCtx настроен
		// >= required, считаем что пользователь явно готов на gpu_layers<all.
		safeMaxQ4 := int(float64(maxVRAMNCtx) * safety * 4)
		if required <= safeMaxQ4 {
			newNCtx := roundUpPow2(required)
			if newNCtx < required {
				newNCtx = required
			}
			logger.Get().Infow("nctx_reload: required exceeds f16 safe_max but fits q4_0, accepting",
				"backend", backendID,
				"required", required, "safe_max_f16", safeMax, "safe_max_q4", safeMaxQ4)
			return &ReloadPlan{
				Decision: DecisionReload,
				NewNCtx:  newNCtx,
				Reason: fmt.Sprintf("required=%d fits q4_0 (safe_max_f16=%d, q4_0=%d); adaptive loader will pick q4_0",
					required, safeMax, safeMaxQ4),
			}
		}
		return c.makeRejectPlan(backendID, bridgeErr, required,
			fmt.Sprintf("required n_ctx=%d exceeds safe VRAM limit=%d (max_vram_n_ctx=%d, safety=%.2f)",
				required, safeMax, maxVRAMNCtx, safety),
			"n_ctx_too_large_for_backend")
	}

	// 4096 ? 4096, 5000 ? 8192, 8000 ? 8192, 12000 ? 16384
	newNCtx := roundUpPow2(required)

	if newNCtx < required {
		newNCtx = required
	}

	if newNCtx > safeMax {
		newNCtx = safeMax
	}

	if cfg.AutoReloadMaxNCtx > 0 && newNCtx > cfg.AutoReloadMaxNCtx {
		newNCtx = cfg.AutoReloadMaxNCtx
	}

	return &ReloadPlan{
		Decision: DecisionReload,
		NewNCtx:  newNCtx,
		Reason: fmt.Sprintf("required=%d, current=%d, max_vram=%d, safe_max=%d, target=%d",
			required, bridgeErr.CurrentNCtx, maxVRAMNCtx, safeMax, newNCtx),
	}
}

//

//

func (c *NCtxReloadCoordinator) makeRejectPlan(
	backendID string,
	bridgeErr *NCtxBridgeError,
	required int,
	reason string,
	errorCode string,
) *ReloadPlan {
	if errorCode == "" {
		errorCode = "n_ctx_too_large_for_backend"
	}
	cfg := c.Config()
	safe := 0
	if bridgeErr.MaxVRAMNCtx > 0 {
		safe = int(float64(bridgeErr.MaxVRAMNCtx) * cfg.effectiveSafetyFactor())
	}

	suggestion := "save a model profile with a larger n_ctx and reload manually, or send a smaller options.num_ctx in the request"
	if errorCode == "prompt_exceeds_context" {
		suggestion = "reduce conversation history / tools[] / system prompt, or use a smaller model. " +
			"Reload with the same or larger n_ctx will NOT help ? the model is already loaded at maximum."
	}
	rej := map[string]interface{}{
		"error":            errorCode,
		"backend_id":       backendID,
		"reason":           reason,
		"requested_n_ctx":  required,
		"current_n_ctx":    bridgeErr.CurrentNCtx,
		"max_vram_n_ctx":   bridgeErr.MaxVRAMNCtx,
		"safe_max_n_ctx":   safe,
		"suggestion":       suggestion,
		"profile_endpoint": "/api/profiles",
		"reload_endpoint":  "/api/models/reload",
		"bridge_code":      bridgeErr.Code,
		"bridge_message":   bridgeErr.Message,
	}
	b, _ := json.Marshal(rej)
	return &ReloadPlan{
		Decision:  DecisionReject,
		Reason:    reason,
		RejectMsg: string(b),
	}
}

func roundUpPow2(v int) int {
	if v <= 0 {
		return 512
	}
	if v < 512 {
		return 512
	}
	p := 1
	for p < v {
		p <<= 1
	}
	return p
}

// ============================================================

// ============================================================

type NCtxReloadHTTPClient interface {
	PostReload(ctx context.Context, endpoint string, payload []byte) (*http.Response, error)
}

type DefaultNCtxReloadHTTPClient struct {
	HTTPClient *http.Client

	// APIToken — токен для аутентификации на cppworker.
	APIToken string

	// HeaderName — имя HTTP-заголовка для токена. Должно совпадать с
	// cfg.Auth.HeaderName балансера (по умолчанию "X-API-Token").
	// Round 25 (2026-08-05): раньше код хардкодил "Authorization: Bearer",
	// что НЕ совпадает с cppworker's auth middleware → reload получал 401
	// и auto-reload n_ctx для Cline был полностью сломан.
	HeaderName string
}

func (c *DefaultNCtxReloadHTTPClient) PostReload(ctx context.Context, endpoint string, payload []byte) (*http.Response, error) {
	client := c.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 300 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Body = io.NopCloser(bytesReader(payload))
	req.ContentLength = int64(len(payload))
	if c.APIToken != "" {
		headerName := c.HeaderName
		if headerName == "" {
			headerName = "X-API-Token" // default для совместимости
		}
		req.Header.Set(headerName, c.APIToken)
	}
	return client.Do(req)
}

type bytesReaderImpl struct {
	buf []byte
	pos int
}

func bytesReader(b []byte) io.Reader { return &bytesReaderImpl{buf: b} }

func (r *bytesReaderImpl) Read(p []byte) (int, error) {
	if r.pos >= len(r.buf) {
		return 0, io.EOF
	}
	n := copy(p, r.buf[r.pos:])
	r.pos += n
	return n, nil
}

var ErrReloadInProgress = errors.New("n_ctx reload already in progress on this backend")

func (c *NCtxReloadCoordinator) DoReload(
	ctx context.Context,
	backendID string,
	backendAddr string,
	modelName string,
	plan *ReloadPlan,
	loader NCtxReloadHTTPClient,
) error {
	if plan.Decision != DecisionReload {
		return fmt.Errorf("DoReload called with non-reload plan: %s", plan.Decision)
	}
	cfg := c.Config()
	timeout := cfg.effectiveTimeout()
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	state := c.state(backendID)
	state.inflightMu.Lock()
	if state.inflight != nil {

		waitCh := state.inflight.done
		state.inflightMu.Unlock()
		logger.Get().Debugf("[nctx_reload] backend %s: another reload in flight, waiting", backendID)
		select {
		case <-waitCh:
			return state.inflight.err
		case <-rctx.Done():
			return fmt.Errorf("timeout waiting for in-flight reload: %w", rctx.Err())
		}
	}

	state.inflight = &reloadInflight{done: make(chan struct{})}
	cur := state.inflight
	state.inflightMu.Unlock()

	defer func() {
		state.inflightMu.Lock()
		state.inflight = nil
		state.inflightMu.Unlock()
		close(cur.done)
	}()

	logger.Get().Infof("[nctx_reload] backend %s: reloading with n_ctx=%d (model=%s, reason=%s)",
		backendID, plan.NewNCtx, modelName, plan.Reason)

	// Query adaptive strategy from cppworker (kvCacheType, gpuLayers, n_ctx)
	strategy := queryAdaptiveStrategy(backendAddr, modelName, plan.NewNCtx, nil)

	// Cap target n_ctx to MaxViableNCtx from adaptive strategy.
	// This prevents auto-reload from escalating to unusable context sizes
	// on small GPUs (e.g. 131K on 8GB where even q4_0 KV-cache doesn't fit).
	targetNCtx := plan.NewNCtx
	if strategy != nil && strategy.MaxViableNCtx > 0 && targetNCtx > strategy.MaxViableNCtx {
		targetNCtx = strategy.MaxViableNCtx
		logger.Get().Infow("nctx_reload: capping target n_ctx to MaxViableNCtx from adaptive strategy",
			"model", modelName,
			"original_target", plan.NewNCtx,
			"capped_target", targetNCtx,
			"max_viable", strategy.MaxViableNCtx,
			"reason", "VRAM insufficient for requested n_ctx even with best kvCacheType")
	}

	payloadMap := map[string]interface{}{
		"name":        modelName,
		"contextSize": targetNCtx,
		"force":       true,
		"reason":      "auto-reload: client request exceeded current n_ctx",
		"gpuLayers":   -2,
		"useMmap":     true,
	}
	if strategy != nil {
		payloadMap = enrichReloadPayload(payloadMap, strategy)
		logger.Get().Infow("nctx_reload: using adaptive strategy",
			"model", modelName,
			"stage", strategy.Stage,
			"kvCacheType", strategy.KVCacheType,
			"gpuLayers", strategy.GPULayers)
	}
	payload, _ := json.Marshal(payloadMap)
	endpoint := backendAddr + "/api/models/reload"
	resp, err := loader.PostReload(rctx, endpoint, payload)
	if err != nil {
		cur.err = fmt.Errorf("reload HTTP request failed: %w", err)
		logger.Get().Errorf("[nctx_reload] backend %s: %v", backendID, cur.err)
		return cur.err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	const maxRetries = 5
	retryInterval := 5 * time.Second
	for retry := 0; retry < maxRetries && resp.StatusCode == http.StatusServiceUnavailable &&
		strings.Contains(string(body), "model is loading"); retry++ {
		logger.Get().Infof("[nctx_reload] backend %s: model still loading, retrying in %v (attempt %d/%d)",
			backendID, retryInterval, retry+1, maxRetries)
		resp.Body.Close()
		select {
		case <-time.After(retryInterval):
		case <-rctx.Done():
			cur.err = fmt.Errorf("reload context cancelled while waiting for model load: %w", rctx.Err())
			return cur.err
		}
		resp, err = loader.PostReload(rctx, endpoint, payload)
		if err != nil {
			cur.err = fmt.Errorf("reload HTTP request failed (retry %d): %w", retry+1, err)
			logger.Get().Errorf("[nctx_reload] backend %s: %v", backendID, cur.err)
			return cur.err
		}
		body, _ = io.ReadAll(resp.Body)
	}

	if resp.StatusCode != http.StatusOK {
		cur.err = fmt.Errorf("reload returned HTTP %d: %s", resp.StatusCode, string(body))
		logger.Get().Errorf("[nctx_reload] backend %s: %v", backendID, cur.err)
		return cur.err
	}

	var result struct {
		ContextSize int    `json:"context_size"`
		NCtx        int    `json:"n_ctx"`
		Status      string `json:"status"`
		Error       string `json:"error"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		cur.err = fmt.Errorf("reload response parse failed: %w (raw: %s)", err, string(body))
		logger.Get().Errorf("[nctx_reload] backend %s: %v", backendID, cur.err)
		return cur.err
	}
	if result.Error != "" {
		cur.err = fmt.Errorf("reload backend error: %s", result.Error)
		logger.Get().Errorf("[nctx_reload] backend %s: %v", backendID, cur.err)
		return cur.err
	}
	actual := result.ContextSize
	if actual == 0 {
		actual = result.NCtx
	}
	if actual == 0 {
		actual = plan.NewNCtx
	}
	c.SetLastKnownNCtx(backendID, actual)
	cur.err = nil
	logger.Get().Infof("[nctx_reload] backend %s: reload successful, new n_ctx=%d", backendID, actual)
	return nil
}

// ============================================================

// ============================================================

type perBackendMetrics struct {
	mu            sync.Mutex
	lastReloadAt  time.Time
	lastError     string
	reloadsTotal  int64
	rejectsTotal  int64
	errorsTotal   int64
	durationSumMs int64
	durationCount int64
}

func (m *perBackendMetrics) Snapshot() map[string]interface{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	avg := int64(0)
	if m.durationCount > 0 {
		avg = m.durationSumMs / m.durationCount
	}
	return map[string]interface{}{
		"last_reload_at":         m.lastReloadAt.UTC().Format(time.RFC3339),
		"last_error":             m.lastError,
		"reloads_total":          m.reloadsTotal,
		"rejects_total":          m.rejectsTotal,
		"errors_total":           m.errorsTotal,
		"reload_duration_ms_avg": avg,
		"reload_duration_ms_sum": m.durationSumMs,
		"reload_duration_count":  m.durationCount,
	}
}

// decision: "noop" / "reject" / "reload-start" / "reloaded" / "reload-failed".
func (c *NCtxReloadCoordinator) RecordDecision(backendID, decision, reason string) {
	if c == nil {
		return
	}
	s := c.perBackendState(backendID)
	s.mu.Lock()
	defer s.mu.Unlock()
	switch decision {
	case "reject":
		s.rejectsTotal++
	case "reload-start":
		s.reloadsTotal++
		s.lastReloadAt = time.Now()
	}
}

func (c *NCtxReloadCoordinator) RecordReloadDuration(backendID string, d time.Duration, err error) {
	if c == nil {
		return
	}
	s := c.perBackendState(backendID)
	s.mu.Lock()
	defer s.mu.Unlock()
	ms := d.Milliseconds()
	s.durationSumMs += ms
	s.durationCount++
	if err != nil {
		s.errorsTotal++
		s.lastError = err.Error()
	}
}

func (c *NCtxReloadCoordinator) RecordError(backendID, errMsg string) {
	if c == nil {
		return
	}
	s := c.perBackendState(backendID)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.errorsTotal++
	if errMsg != "" {
		s.lastError = errMsg
	}
}

func (c *NCtxReloadCoordinator) perBackendState(backendID string) *perBackendMetrics {
	if existing, ok := c.metricsByBackend.Load(backendID); ok {
		return existing.(*perBackendMetrics)
	}
	actual, _ := c.metricsByBackend.LoadOrStore(backendID, &perBackendMetrics{})
	return actual.(*perBackendMetrics)
}

//

func (c *NCtxReloadCoordinator) Snapshot() map[string]interface{} {
	if c == nil {
		return map[string]interface{}{
			"nctx_reloads_total":          int64(0),
			"nctx_rejects_total":          int64(0),
			"nctx_errors_total":           int64(0),
			"nctx_reload_duration_ms_avg": int64(0),
			"nctx_reload_duration_ms_sum": int64(0),
			"nctx_reload_duration_count":  int64(0),
			"nctx_per_backend":            map[string]interface{}{},
		}
	}
	var totalReloads, totalRejects, totalErrors, totalSumMs, totalCount int64
	perBackend := make(map[string]interface{})
	c.metricsByBackend.Range(func(key, value interface{}) bool {
		id := key.(string)
		m := value.(*perBackendMetrics)
		m.mu.Lock()
		totalReloads += m.reloadsTotal
		totalRejects += m.rejectsTotal
		totalErrors += m.errorsTotal
		totalSumMs += m.durationSumMs
		totalCount += m.durationCount
		m.mu.Unlock()
		perBackend[id] = m.Snapshot()
		return true
	})
	avg := int64(0)
	if totalCount > 0 {
		avg = totalSumMs / totalCount
	}
	return map[string]interface{}{
		"nctx_reloads_total":          totalReloads,
		"nctx_rejects_total":          totalRejects,
		"nctx_errors_total":           totalErrors,
		"nctx_reload_duration_ms_avg": avg,
		"nctx_reload_duration_ms_sum": totalSumMs,
		"nctx_reload_duration_count":  totalCount,
		"nctx_per_backend":            perBackend,
	}
}

//

//

func (c *NCtxReloadCoordinator) BackendMetricsFromState(backendID string) *types.BackendMetrics {
	if c == nil {
		return nil
	}
	return &types.BackendMetrics{
		ID:              backendID,
		Timestamp:       time.Now(),
		WarmingUpModels: []string{},
	}
}

//

func (c *NCtxReloadCoordinator) Shutdown() {
	if c == nil {
		return
	}

}

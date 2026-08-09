// sync_profile.go — Pull-based per-model profile sync from balancer.
//
// Why this exists (2026-08-06):
//
//	Earlier the only way for balancer's per-model profile (contextLength,
//	numGpuLayers, kvCacheType, etc.) to actually apply on cppworker was
//	via POST /api/v1/cppworker/model-profiles/{name}/apply. That endpoint
//	is run manually (or via WebUI). If cppworker is recreated, it boots
//	with env defaults (CPPWORKER_CTX_SIZE=16384 → auto-tune raises to
//	32768) and the profile in balancer is forgotten until someone hits
//	"Apply" again.
//
//	This file makes cppworker PULL its profile from balancer:
//	  1. On startup (after successful register-with-balancer)
//	  2. On every model load (POST /api/models/load-with-params)
//	  3. On every model reload (POST /api/models/reload)
//	  4. Periodically (every 5 minutes) — to catch external PUT changes
//
// Pull-based beats push-based here because:
//   - cppworker is the one with ground truth (VRAM, model loaded state)
//   - cppworker can self-heal after container recreate
//   - No race conditions with multi-instance deployments
//
// Auth: uses X-API-Token header (same as balancer_register.go). Round 24 fix
// (Authorization: Bearer → X-API-Token) is the standard pattern across
// cppworker<->balancer.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"go.uber.org/zap"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// profileSyncerT — кеш профилей, вытянутых с балансера.
//
// Жизненный цикл:
//   - newProfileSyncer(balancerURL, token, backendID) — start the background fetcher
//   - applyProfileOnLoad(modelName) — called by handlers before load/reload
//   - stop() — clean shutdown
type profileSyncerT struct {
	mu          sync.RWMutex
	cache       map[string]types.LlamaCppModelProfile // modelName → profile
	lastFetchAt time.Time
	balancerURL string
	token       string
	backendID   string
	client      *http.Client
	stopCh      chan struct{}
	logger      *zap.SugaredLogger
}

// newProfileSyncer — конструирует syncer. balancerURL может быть пустым
// (тогда syncer работает в passive mode — без фонового fetch, но applyProfileOnLoad
// продолжает работать для уже-закешированных профилей).
func newProfileSyncer(balancerURL, token, backendID string, logger *zap.SugaredLogger) *profileSyncerT {
	if balancerURL == "" {
		// Standalone mode — no balancer to pull from. Cline will use raw env defaults.
		return nil
	}
	return &profileSyncerT{
		cache:       make(map[string]types.LlamaCppModelProfile),
		balancerURL: balancerURL,
		token:       token,
		backendID:   backendID,
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
		stopCh: make(chan struct{}),
		logger: logger,
	}
}

// start — запускает background fetcher (initial + periodic).
// Round 26 (2026-08-06): initial fetch happens immediately so a fresh
// cppworker has the right profile before serving the first request.
func (ps *profileSyncerT) start(ctx context.Context) {
	if ps == nil {
		return
	}
	ps.logger.Infow("profileSyncer: starting",
		"balancerURL", ps.balancerURL, "backendID", ps.backendID)

	// Initial fetch synchronously (with short timeout) — so cppworker
	// has correct profile before the first /api/models/load-with-params.
	initCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	if err := ps.fetchOnce(initCtx); err != nil {
		ps.logger.Warnw("profileSyncer: initial fetch failed (will retry in background)",
			"error", err)
	}
	cancel()

	go ps.runLoop(ctx)
}

func (ps *profileSyncerT) stop() {
	if ps == nil {
		return
	}
	close(ps.stopCh)
}

// runLoop — periodic background fetcher (every 5 minutes).
// 5 min picked as compromise: catches external PUT profile changes
// without hammering balancer. Manual apply endpoint is still available
// for immediate updates.
func (ps *profileSyncerT) runLoop(ctx context.Context) {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ps.stopCh:
			return
		case <-t.C:
			fetchCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			if err := ps.fetchOnce(fetchCtx); err != nil {
				ps.logger.Debugw("profileSyncer: periodic fetch failed (non-fatal)",
					"error", err)
			}
			cancel()
		}
	}
}

// fetchOnce — GET /api/v1/cppworker/model-profiles → cache update.
// Тихо проглатывает ошибки — это best-effort sync, не критично для работы.
func (ps *profileSyncerT) fetchOnce(ctx context.Context) error {
	if ps == nil || ps.balancerURL == "" {
		return nil
	}
	url := ps.balancerURL + "/api/v1/cppworker/model-profiles"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if ps.token != "" {
		req.Header.Set("X-API-Token", ps.token)
	}

	resp, err := ps.client.Do(req)
	if err != nil {
		return fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status=%d", resp.StatusCode)
	}

	var body struct {
		Models map[string]types.LlamaCppModelProfile `json:"models"`
		Total  int                                   `json:"total"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return fmt.Errorf("decode: %w", err)
	}

	ps.mu.Lock()
	ps.cache = body.Models
	ps.lastFetchAt = time.Now()
	count := len(ps.cache)
	ps.mu.Unlock()

	ps.logger.Infow("profileSyncer: profiles fetched",
		"count", count, "total_reported", body.Total)
	return nil
}

// fetchNow — принудительный refresh (для тестов / startup).
func (ps *profileSyncerT) fetchNow(ctx context.Context) error {
	return ps.fetchOnce(ctx)
}

// applyProfileOnLoad — вызывается из handleLoadWithParams / handleReloadModel.
// Возвращает профиль для модели (nil если нет) и merged с effective defaults.
//
// mergePolicy (Round 26, 2026-08-06):
//   - profile.ContextSize > 0 → win
//   - profile.BatchSize > 0 → win
//   - profile.NumGPULayers != 0 → win (но -1 = all GPU)
//   - profile.FlashAttn != nil → win
//   - profile.KVCacheType != "" → win
//   - profile.Disabled == true → caller должен отказать load
//
// This is "soft merge" — if profile is missing for this model, return nil
// and let the request's own params stand.
func (ps *profileSyncerT) applyProfileOnLoad(modelName string) *types.LlamaCppModelProfile {
	if ps == nil {
		return nil
	}
	ps.mu.RLock()
	defer ps.mu.RUnlock()

	// 1) Exact match
	if p, ok := ps.cache[modelName]; ok {
		return &p
	}
	// 2) Case-insensitive substring match (e.g. profile "gemma-4" → model "gemma-4-E4B-it-Q4_K_M")
	for name, p := range ps.cache {
		if containsFold(name, modelName) || containsFold(modelName, name) {
			return &p
		}
	}
	return nil
}

// snapshotSize — для логов / metrics.
func (ps *profileSyncerT) snapshotSize() int {
	if ps == nil {
		return 0
	}
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	return len(ps.cache)
}

// containsFold — case-insensitive substring match без аллокаций.
func containsFold(a, b string) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	la, lb := len(a), len(b)
	if la > lb {
		a, b = b, a
		la, lb = lb, la
	}
	for i := 0; i+la <= lb; i++ {
		match := true
		for j := 0; j < la; j++ {
			ca, cb := a[j], b[i+j]
			if ca >= 'A' && ca <= 'Z' {
				ca += 'a' - 'A'
			}
			if cb >= 'A' && cb <= 'Z' {
				cb += 'a' - 'A'
			}
			if ca != cb {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// resolveBalancerURL — env-fallback chain для адреса балансера.
// Round 26: cppworker может ходить к балансеру за профилями даже если
// register.sh отключён (CPPWORKER_REGISTER_DISABLE=true). Потому что sync
// профилей нужен, а регистрация бэкенда — нет.
func resolveBalancerURL() string {
	for _, env := range []string{"CPPWORKER_BALANCER_URL", "BALANCER_URL", "LB_URL"} {
		if v := os.Getenv(env); v != "" {
			return v
		}
	}
	return ""
}

func resolveBalancerToken() string {
	for _, env := range []string{"CPPWORKER_BALANCER_TOKEN", "BALANCER_API_TOKEN", "LB_API_TOKEN"} {
		if v := os.Getenv(env); v != "" {
			return v
		}
	}
	return ""
}

// applyProfileToLoadRequest — применяет profile параметры поверх request.
// Round 26 (2026-08-06): soft-merge — profile дополняет, но не перезаписывает
// явно заданные параметры. Это позволяет:
//   - direct calls (с явными параметрами) — работают как раньше
//   - balancer-triggered loads — получают profile defaults
//   - bundled-stacks (load-with-params) — profile wins
//
// Параметры — указатели на поля request struct (см. types.go).
// Возвращает false если profile.Disabled=true (caller должен отказать load).
func applyProfileToLoadRequest(
	profile *types.LlamaCppModelProfile,
	contextSize, batchSize, gpuLayers *int,
	flashAttnType *int,
	kvCacheType *string,
) (allowed bool) {
	if profile == nil {
		return true
	}
	if profile.Disabled {
		return false
	}
	// profile.FlashAttn (*bool) → caller passes *int (0 = unset, 1 = on, -1 = off).
	if profile.FlashAttn != nil {
		// Конвертим bool → int: true=1, false=-1.
		var want int
		if *profile.FlashAttn {
			want = 1
		} else {
			want = -1
		}
		if flashAttnType == nil {
			flashAttnType = &want
		} else if *flashAttnType == 0 {
			*flashAttnType = want
		}
	}
	// Для остальных полей: profile.X > 0 → заполняем если caller пустой.
	if profile.ContextLength > 0 {
		if contextSize == nil {
			v := profile.ContextLength
			contextSize = &v
		} else if *contextSize == 0 {
			*contextSize = profile.ContextLength
		}
	}
	if profile.BatchSize > 0 {
		if batchSize == nil {
			v := profile.BatchSize
			batchSize = &v
		} else if *batchSize == 0 {
			*batchSize = profile.BatchSize
		}
	}
	if profile.NumGPULayers != 0 {
		// Note: 0 в profile значит "inherit"; -1 в cppworker значит "all GPU".
		// Caller несёт ответственность за трансляцию 0 → -1 если хочет all-GPU.
		if gpuLayers == nil {
			v := profile.NumGPULayers
			gpuLayers = &v
		} else if *gpuLayers == 0 {
			*gpuLayers = profile.NumGPULayers
		}
	}
	if profile.KVCacheType != "" {
		if kvCacheType == nil {
			v := profile.KVCacheType
			kvCacheType = &v
		} else if *kvCacheType == "" {
			*kvCacheType = profile.KVCacheType
		}
	}
	return true
}

// applyMaxTokensCap — clamps params.NPredict к profile.MaxTokens если задан.
// Round 26 (2026-08-06): защита от runaway generation когда Cline/OpenWebUI
// запрашивают max_tokens=32000 и gemma-4 генерит 10+ минут (асинхронно,
// нельзя отменить без Cline-initiated /api/cancel).
//
// Soft cap: только снижает. Если params.NPredict уже <= profile.MaxTokens,
// ничего не делает (сохраняет явные smaller client requests).
//
// Применяется ПОСЛЕ buildGenerationParams (когда NPredict уже выставлен)
// но ДО clampNPredictToFitContext (которое может уменьшить ещё больше
// из-за n_ctx overflow — это правильное поведение, кап тоже важен).
func applyMaxTokensCap(modelName string, nPredict *int) {
	if profileSyncer == nil || nPredict == nil {
		return
	}
	prof := profileSyncer.applyProfileOnLoad(modelName)
	if prof == nil || prof.MaxTokens <= 0 {
		return
	}
	// 0 = use default (2048 or reasoning default). Не ограничиваем default.
	if *nPredict == 0 {
		return
	}
	if *nPredict > prof.MaxTokens {
		logger.Get().Infow("applyMaxTokensCap: clamping NPredict by profile",
			"model", modelName,
			"profileMaxTokens", prof.MaxTokens,
			"oldNPredict", *nPredict,
			"newNPredict", prof.MaxTokens)
		*nPredict = prof.MaxTokens
	}
}

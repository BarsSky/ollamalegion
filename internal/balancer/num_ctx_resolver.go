// Package balancer — 3-tier resolver для n_ctx override (Шаг 2).
//
// Приоритет (от высшего к низшему):
//  1. Per-request (body: options.num_ctx для Ollama, num_ctx для OpenAI)
//  2. Per-model profile (config.LlamaCppModelProfiles[modelName])
//  3. Per-backend default (state.Backend.CppWorkerConfig.ContextLength)
//
// Header X-Cpp-Ctx (от cppworker / балансировщика) применяется ТОЛЬКО если body
// не задал num_ctx, т.к. header — это политика балансировщика, body — явный
// per-request запрос клиента. Body всегда побеждает.
package balancer

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// NumCtxSource — откуда взят n_ctx.
type NumCtxSource string

const (
	NumCtxSourceRequest     NumCtxSource = "request"      // из body (options.num_ctx или top-level num_ctx)
	NumCtxSourceProfile     NumCtxSource = "profile"      // из per-model profile в config
	NumCtxSourceBackend     NumCtxSource = "backend"      // из per-backend default CppWorkerConfig.ContextLength
	NumCtxSourceLoadedModel NumCtxSource = "loaded_model" // из реально загруженной модели на бэкенде (metrics)
	NumCtxSourceNone        NumCtxSource = "none"         // ничего не найдено (cppworker использует свой defaultCtxSize)
)

// ResolvedNumCtx — результат resolver'а.
type ResolvedNumCtx struct {
	Value  int          // 0 = ничего не найдено, cppworker возьмёт default
	Source NumCtxSource // откуда взято (для логов и debug)
}

// ExtractNumCtxFromBody — извлекает num_ctx из request body.
// Поддерживает оба формата:
//   - Ollama: {"options": {"num_ctx": N}}
//   - OpenAI / generic: {"num_ctx": N}
//
// Возвращает 0 если не задано.
func ExtractNumCtxFromBody(body []byte) int {
	if len(body) == 0 {
		return 0
	}
	var req map[string]interface{}
	if err := json.Unmarshal(body, &req); err != nil {
		return 0
	}

	// 1) Ollama: options.num_ctx
	if opts, ok := req["options"].(map[string]interface{}); ok {
		if nctxRaw, exists := opts["num_ctx"]; exists {
			if nctx, ok := asInt(nctxRaw); ok && nctx > 0 {
				return nctx
			}
		}
	}

	// 2) OpenAI / generic: top-level num_ctx
	if nctxRaw, exists := req["num_ctx"]; exists {
		if nctx, ok := asInt(nctxRaw); ok && nctx > 0 {
			return nctx
		}
	}

	return 0
}

// asInt — пытается привести interface{} (из JSON-парсинга) к int.
// JSON числа декодируются как float64; поддерживаем также прямой int.
func asInt(v interface{}) (int, bool) {
	switch x := v.(type) {
	case int:
		return x, true
	case int32:
		return int(x), true
	case int64:
		return int(x), true
	case float32:
		return int(x), true
	case float64:
		return int(x), true
	default:
		return 0, false
	}
}

// GetModelProfileNumCtx — достаёт contextLength из per-model profile в config.
// Возвращает 0 если профиля нет или contextLength не задан.
//
// Сравнение имени — case-insensitive substring match (как в containsFold),
// чтобы profile "gemma-4" матчил модель "gemma-4-E4B-it-Q4_K_M".
// Если найдено несколько совпадений — возвращается первое.
func (p *Proxy) GetModelProfileNumCtx(modelName string) int {
	if p == nil || p.config == nil || p.config.LlamaCppModelProfiles == nil {
		return 0
	}
	// Точное совпадение (fast-path)
	if profile, ok := p.config.LlamaCppModelProfiles[modelName]; ok {
		if profile.ContextLength > 0 {
			return profile.ContextLength
		}
	}
	// Case-insensitive substring match (для профиля "gemma-4" → модель "gemma-4-E4B-it-Q4_K_M")
	for name, profile := range p.config.LlamaCppModelProfiles {
		if profile.ContextLength == 0 {
			continue
		}
		if containsFold(name, modelName) || containsFold(modelName, name) {
			return profile.ContextLength
		}
	}
	return 0
}

// GetModelProfile — полный профиль (для API endpoints).
// Возвращает (profile, true) если найден, иначе (zero, false).
func (p *Proxy) GetModelProfile(modelName string) (types.LlamaCppModelProfile, bool) {
	if p == nil || p.config == nil || p.config.LlamaCppModelProfiles == nil {
		return types.LlamaCppModelProfile{}, false
	}
	profile, ok := p.config.LlamaCppModelProfiles[modelName]
	return profile, ok
}

// SetModelProfile — upsert профиля (для API endpoints).
// ВАЖНО: эта функция только обновляет in-memory config. Сохранение на диск —
// ответственность вызывающей стороны (через config.Save()).
func (p *Proxy) SetModelProfile(modelName string, profile types.LlamaCppModelProfile) {
	if p == nil || p.config == nil {
		return
	}
	if p.config.LlamaCppModelProfiles == nil {
		p.config.LlamaCppModelProfiles = make(map[string]types.LlamaCppModelProfile)
	}
	p.config.LlamaCppModelProfiles[modelName] = profile
}

// DeleteModelProfile — удаляет профиль.
// Возвращает true если профиль был, false если его не было.
func (p *Proxy) DeleteModelProfile(modelName string) bool {
	if p == nil || p.config == nil || p.config.LlamaCppModelProfiles == nil {
		return false
	}
	if _, ok := p.config.LlamaCppModelProfiles[modelName]; !ok {
		return false
	}
	delete(p.config.LlamaCppModelProfiles, modelName)
	return true
}

// GetDefaultModelProfileNumCtx — достаёт contextLength из config.DefaultModelProfile.
// Используется как fallback-потолок при clamping per-request num_ctx (Phase D.3-fix),
// когда для модели нет записи в LlamaCppModelProfiles.
//
// Возвращает 0 если defaultProfile не задан или contextLength == 0.
func (p *Proxy) GetDefaultModelProfileNumCtx() int {
	if p == nil || p.config == nil || p.config.DefaultModelProfile == nil {
		return 0
	}
	return p.config.DefaultModelProfile.ContextLength
}

// GetBackendDefaultNumCtx — достаёт contextLength из per-backend CppWorkerConfig.
// Используется как fallback в 3-tier resolver.
// Возвращает 0 если backend не llama_cpp или contextLength не задан.
func (p *Proxy) GetBackendDefaultNumCtx(backendID string) int {
	if p == nil || p.config == nil {
		return 0
	}
	for i := range p.config.Backends {
		if p.config.Backends[i].ID == backendID {
			b := &p.config.Backends[i]
			if b.Type != types.BackendTypeLlamaCpp {
				return 0
			}
			if b.CppWorkerConfig == nil {
				return 0
			}
			return b.CppWorkerConfig.ContextLength
		}
	}
	return 0
}

// maxNumCtxForModel — эффективный потолок для модели ДЛЯ CLAMPING (Round 43).
//
// Round 43 (2026-08-19) CONCEPTUAL FIX:
//
//	Pre-R43: возвращал profile.contextLength как hard cap. Cline с num_ctx=65536
//	на профиле contextLength=32768 → CLAMP до 32768 → cppworker reload с 32768
//	→ prompt overflow → 400. Это был hidden hardcode в resolver chain.
//
//	Post-R43: использует ту же resolveModelMaxContext v3 что и preflight.
//	contextLengthAuto=true → return 0 (NO clamp, let cppworker decide via
//	auto-tune). profile — HINT, не cap. Cline 65536 → проходит как есть →
//	cppworker auto-tunes gpu_layers → reload с feasible value.
//
// Приоритет (3-tier, identical to resolveModelMaxContext v3):
//  1. per-model profile (respecting contextLengthAuto + contextLengthMax)
//  2. fallback to config.DefaultModelProfile (Phase D.3-fix, backward compat)
//  3. fallback to GGUFMax (R43, was: loaded model contextSize from metrics)
//
// Возвращает 0 если ни один из источников не даёт значения — в этом случае
// clamping НЕ производится (cppworker сам решит, что делать с n_ctx из body).
//
// CRITICAL: This function MUST stay in sync with resolveModelMaxContext (in
// nctx_reload_handlers.go). The R43 fix unifies both call sites to use the
// same 3-tier logic. If you change one, change the other.
func (p *Proxy) maxNumCtxForModel(modelName string, backendID string) int {
	if p == nil || p.config == nil {
		return 0
	}

	// Tier 1: per-model profile (auto-aware via resolveModelMaxContext v3)
	if mp, ok := p.config.LlamaCppModelProfiles[modelName]; ok && mp.ContextLength > 0 {
		return p.resolveModelMaxContext(backendID, modelName, mp.ContextLength, mp.ContextLengthAuto, mp.ContextLengthMax, 0)
	}
	// Case-insensitive substring match (для профиля "gemma-4" → модель "gemma-4-E4B-it-Q4_K_M")
	for name, mp := range p.config.LlamaCppModelProfiles {
		if containsFold(name, modelName) || containsFold(modelName, name) {
			if mp.ContextLength > 0 {
				return p.resolveModelMaxContext(backendID, modelName, mp.ContextLength, mp.ContextLengthAuto, mp.ContextLengthMax, 0)
			}
		}
	}

	// Tier 2: default profile (Phase D.3-fix). R43: also auto-aware.
	if p.config.DefaultModelProfile != nil && p.config.DefaultModelProfile.ContextLength > 0 {
		dp := p.config.DefaultModelProfile
		return p.resolveModelMaxContext(backendID, modelName, dp.ContextLength, dp.ContextLengthAuto, dp.ContextLengthMax, 0)
	}

	// Tier 3: GGUFMax from metrics (R43 fix: was loaded model contextSize).
	// Loaded-model contextSize is current state (conservative if no auto-tune yet).
	// GGUFMax is the model's hard upper bound from training metadata.
	if backendID != "" {
		if v := p.getGGUFMaxContext(backendID); v > 0 {
			return v
		}
		// Final fallback: loaded model contextSize from metrics.
		if v := p.getModelLoadedCtxFromMetrics(backendID, modelName); v > 0 {
			return v
		}
	}

	return 0
}

// getModelLoadedCtxFromMetrics — извлекает реальный n_ctx загруженной модели
// из метрик балансировщика (llamaMetrics[backendID].LoadedModels[].ContextLength).
//
// CppWorker опрашивается llamaCppMetricsPoller'ом каждые 30 секунд через
// GET /api/models, который возвращает contextSize для каждой загруженной модели.
// Это динамический источник, который отражает, с каким n_ctx модель реально
// загружена на бэкенде в данный момент.
//
// Сравнение имени — case-insensitive substring match (как в IsLlamaCppModelLoaded).
//
// Thread-safe: использует RLock на metricsMgr.mu.
// Возвращает 0 если модель не найдена или contextLength не задан.
func (p *Proxy) getModelLoadedCtxFromMetrics(backendID, modelName string) int {
	if p == nil || p.metricsMgr == nil || backendID == "" || modelName == "" {
		return 0
	}
	p.metricsMgr.mu.RLock()
	defer p.metricsMgr.mu.RUnlock()

	lm, ok := p.metricsMgr.llamaMetrics[backendID]
	if !ok || lm == nil {
		return 0
	}
	for _, m := range lm.LoadedModels {
		if m.Name == modelName || containsFold(m.Name, modelName) {
			if m.ContextLength > 0 {
				logger.Get().Debugw("maxNumCtxForModel: using loaded model context from metrics",
					"model", modelName, "backend", backendID, "context_length", m.ContextLength)
				return m.ContextLength
			}
		}
	}
	return 0
}

// ResolveNumCtx — основной 3-tier resolver.
//
// Приоритет: body > profile > backend default.
//
// modelName — имя модели из запроса (e.g. "gemma-4-E4B-it-Q4_K_M").
// body — сырое тело запроса (нужно для извлечения options.num_ctx / num_ctx).
// backendID — выбранный бэкенд (после selectBackend).
//
// Clamping: body-override клампится сверху потолком, заданным в profile
// (если задан). Это защищает от ситуации, когда клиент (например, OpenWebUI)
// присылает options.num_ctx=16384, а модель загружена с effective n_ctx=4096
// (лимит VRAM). Без клампинга cppworker вернёт
// "requested n_ctx=16384 exceeds model's effective n_ctx=4096".
//
// Потолок: сначала per-model profile, иначе config.DefaultModelProfile
// (Phase D.3-fix). Если оба 0 — clamping пропускается.
//
// Возвращает ResolvedNumCtx{Value, Source}. Value=0 означает, что ни один
// источник не дал override — cppworker использует свой defaultCtxSize.
func (p *Proxy) ResolveNumCtx(modelName string, body []byte, backendID string) ResolvedNumCtx {
	// Tier 1: из request body (наивысший приоритет)
	if n := ExtractNumCtxFromBody(body); n > 0 {
		// R60.37 (2026-09-10): если loaded n_ctx покрывает required
		// (prompt + n_predict), patch body num_ctx до loaded чтобы
		// cppworker не использовал меньшее значение. Без этого fix
		// OpenWebUI шлёт options.num_ctx=2048, balancer ставит
		// X-Cpp-Ctx: 2048 header, cppworker применяет 2048 при loaded=4096.
		// Если required (prompt + n_predict) > 2048, overflow → 400.
		//
		// Stickiness: используем max(body_n_ctx, loaded_n_ctx) — НЕ
		// downgrade к body n_ctx когда loaded уже >= required.
		if p.nctxReload != nil {
			loadedNCtx := p.nctxReload.LastKnownNCtx(backendID)
			if loadedNCtx > 0 && loadedNCtx > n {
				// R60.37: loaded > body — patch body к loaded.
				// Это предотвращает cppworker overflow (loaded=4096, body=2048, prompt overflow).
				logger.Get().Infow("ResolveNumCtx: R60.37 — body num_ctx < loaded, upgrading",
					"model", modelName, "backend", backendID,
					"body_n_ctx", n, "loaded_n_ctx", loadedNCtx)
				n = loadedNCtx
			}
		}
		// Clamp к потолку из profile (если задан).
		// profile.ContextLength — это максимум, который поддерживает модель
		// после partial GPU offload (зависит от VRAM).
		// Fallback на config.DefaultModelProfile (Phase D.3-fix).
		if maxCtx := p.maxNumCtxForModel(modelName, backendID); maxCtx > 0 && n > maxCtx {
			logger.Get().Warnw("ResolveNumCtx: clamping request num_ctx to profile max",
				"model", modelName, "requested", n, "clamped_to", maxCtx, "source", NumCtxSourceRequest)
			return ResolvedNumCtx{Value: maxCtx, Source: NumCtxSourceRequest}
		}
		return ResolvedNumCtx{Value: n, Source: NumCtxSourceRequest}
	}

	// Tier 2: из per-model profile
	if n := p.GetModelProfileNumCtx(modelName); n > 0 {
		return ResolvedNumCtx{Value: n, Source: NumCtxSourceProfile}
	}

	// Tier 3: из per-backend default
	if backendID != "" {
		if n := p.GetBackendDefaultNumCtx(backendID); n > 0 {
			// R60.47b (2026-09-11): если loaded_n_ctx > backend_default, используем
			// loaded. Иначе balancer поставит X-Cpp-Ctx=backend_default (< loaded),
			// cppworker увидит "request asked for n_ctx=X but model loaded with n_ctx=Y"
			// → 400 prompt_too_long → balancer async/sync reload → cascade/empty.
			//
			// Симметрично R60.37 (Tier 1): если body < loaded → upgrade до loaded.
			// Здесь: если backend_default < loaded → upgrade до loaded.
			//
			// Когда loaded=0 (cold start, balancer ещё не знает) → возвращаем
			// backend_default как есть. После auto-load (R60.33) loaded обновится.
			if p.nctxReload != nil {
				loadedNCtx := p.nctxReload.LastKnownNCtx(backendID)
				if loadedNCtx > 0 && loadedNCtx > n {
					logger.Get().Infow("ResolveNumCtx: R60.47b — backend_default < loaded, upgrading",
						"model", backendID, "backend", backendID,
						"backend_default_n_ctx", n, "loaded_n_ctx", loadedNCtx)
					return ResolvedNumCtx{Value: loadedNCtx, Source: NumCtxSourceBackend}
				}
			}
			return ResolvedNumCtx{Value: n, Source: NumCtxSourceBackend}
		}
	}

	return ResolvedNumCtx{Value: 0, Source: NumCtxSourceNone}
}

// ApplyCppCtxHeader — резолвит num_ctx через 3-tier resolver и устанавливает
// header X-Cpp-Ctx в r.Header (если Value > 0). cppworker прочитает этот
// header через applyCppCtxHeader() и применит к params.NCtxOverride.
//
// Используется в handler'ах llama.cpp прокси-роутера (handleChat,
// handleGenerate, handleOpenAIChatCompletions) после выбора backend и
// ДО вызова proxyRequestLlamaCpp* (которые используют r.Header для
// прокидывания на upstream).
//
// Важно: этот helper мутирует r.Header, но НЕ модифицирует bodyBuf, чтобы
// при echo-back клиент получил точно свой запрос (num_ctx в body не меняется).
func (p *Proxy) ApplyCppCtxHeader(r *http.Request, modelName string, bodyBuf []byte, backendID string) ResolvedNumCtx {
	resolved := p.ResolveNumCtx(modelName, bodyBuf, backendID)
	if resolved.Value > 0 {
		if r.Header == nil {
			return resolved
		}
		r.Header.Set("X-Cpp-Ctx", strconv.Itoa(resolved.Value))
	}
	return resolved
}

// ValidateProfile — проверяет профиль на корректность перед сохранением.
// Возвращает error если какое-то поле вне допустимых границ.
func ValidateProfile(p types.LlamaCppModelProfile) error {
	if p.ContextLength != 0 {
		if p.ContextLength < 256 || p.ContextLength > 262144 {
			return fmt.Errorf("contextLength must be in [256, 262144], got %d", p.ContextLength)
		}
	}
	if p.BatchSize != 0 && p.BatchSize < 1 {
		return fmt.Errorf("batchSize must be >= 1, got %d", p.BatchSize)
	}
	if p.NumGPULayers < -1 {
		return fmt.Errorf("numGpuLayers must be >= -1 (-1 = all layers), got %d", p.NumGPULayers)
	}
	return nil
}

// IsLlamaCppModelLoaded — true если modelName загружена на backend'е (per metrics).
//
// Используется в API endpoint'е POST /api/v1/cppworker/model-profiles/{name}/apply
// для решения, нужно ли делать reload (если модель не загружена — reload не нужен,
// потому что следующий load уже подхватит новый n_ctx из профиля).
//
// Сравнение имени — case-insensitive substring match (как в findModelOnLlamaCppBackend),
// чтобы покрыть случаи "gemma-4-E4B-it-Q4_K_M" vs "gemma-4-e4b-it-q4_k_m.gguf".
//
// Thread-safe: использует RLock на metricsMgr.mu.
func (p *Proxy) IsLlamaCppModelLoaded(backendID, modelName string) bool {
	if p == nil || modelName == "" {
		return false
	}
	if p.metricsMgr == nil {
		return false
	}
	p.metricsMgr.mu.RLock()
	defer p.metricsMgr.mu.RUnlock()

	lm, ok := p.metricsMgr.llamaMetrics[backendID]
	if !ok || lm == nil {
		return false
	}
	for _, m := range lm.LoadedModels {
		if m.Name == modelName {
			return true
		}
		// Также принимаем partial match (e.g. "gemma-4" matches "gemma-4-E4B-it-Q4_K_M.gguf")
		if modelName != "" && containsFold(m.Name, modelName) {
			return true
		}
	}
	return false
}

// containsFold — case-insensitive substring match.
func containsFold(haystack, needle string) bool {
	if len(needle) > len(haystack) {
		return false
	}
	hl := len(haystack)
	nl := len(needle)
	for i := 0; i+nl <= hl; i++ {
		match := true
		for j := 0; j < nl; j++ {
			h := haystack[i+j]
			n := needle[j]
			if h >= 'A' && h <= 'Z' {
				h += 'a' - 'A'
			}
			if n >= 'A' && n <= 'Z' {
				n += 'a' - 'A'
			}
			if h != n {
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

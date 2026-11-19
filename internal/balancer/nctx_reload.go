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
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"ollama-loadbalancer/pkg/logger"
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

	// R60.6 (2026-09-07): max clamp для PreflightAsyncRetryAfterSec.
	// Раньше было hardcoded 30s (в effectiveAsyncRetryAfter). 30s мало для
	// 5GB+131072 n_ctx load (60-180s на RTX 3070 8GB) — client retry-ил
	// раньше чем reload заканчивался, triggering cascading reloads.
	// Default 120s покрывает realistic hardware. Operator может переопределить
	// через config.json: Balancing.NCtxReload.PreflightAsyncRetryAfterMaxSec
	// или ENV LB_NCTX_RELOAD_PREFLIGHT_ASYNC_RETRY_AFTER_MAX_SEC.
	PreflightAsyncRetryAfterMaxSec int `json:"preflight_async_retry_after_max_sec" yaml:"preflight_async_retry_after_max_sec"`

	// Round 35c (2026-08-13): см. pkg/types.BalancingSettings.PreflightMaxWaitSec
	// и PreflightWaitMultiplier. Эти поля читаются из config.json
	// (Balancing.NCtxReload.PreflightMaxWaitSec) или из ENV overrides
	// (LB_NCTX_PREFLIGHT_MAX_WAIT_SEC / LB_NCTX_PREFLIGHT_WAIT_MULTIPLIER).
	PreflightMaxWaitSec     int `json:"preflight_max_wait_sec" yaml:"preflight_max_wait_sec"`
	PreflightWaitMultiplier int `json:"preflight_wait_multiplier" yaml:"preflight_wait_multiplier"`
	PreflightWaitBufferSec  int `json:"preflight_wait_buffer_sec" yaml:"preflight_wait_buffer_sec"`
}

func DefaultNCtxReloadConfig() NCtxReloadConfig {
	return NCtxReloadConfig{
		AutoReloadNCtx:              true,
		AutoReloadMaxNCtx:           0,
		AutoReloadVRAMSafetyFactor:  0.85,
		AutoReloadTimeoutSec:        300,
		AutoReloadAllowTools:        true,
		PreflightEnabled:            true,
		PreflightAsyncReload:        false, // sync по умолчанию (старое поведение)
		PreflightAsyncRetryAfterSec: 5,
		// Round 35c: env-конфигурируемые polling timeouts для async load.
		// Default 900s (15 min) cap + 2x multiplier покрывает A10 с 22GB Qwen3.6
		// (est ~3min, 2*3+60=7.2min). Для 3070 / 70B+ моделей поднимите через ENV.
		PreflightMaxWaitSec:     900,
		PreflightWaitMultiplier: 2,
		PreflightWaitBufferSec:  60,
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
	// R83/v62 (2026-10-02): кап на ПЕРЕЗАГРУЗКУ модели снят по умолчанию.
	//
	// Раньше здесь был дефолт 300 с (clamp 30..600), и он работал в двух местах:
	// три context.WithTimeout в preflight_nctx.go И http.Client.Timeout для всех
	// вызовов reload (newNCtxReloadHTTPClient). Для большой модели (A10, qwen3.8
	// 15.7 ГБ) перезагрузка в 300 с не укладывается — операция умирала по таймеру,
	// а клиент получал ошибку вместо ответа. Ждём терминальное состояние
	// перезагрузки, а не время.
	//
	// Кап — только явный opt-in LB_ALLOW_NCTX_RELOAD_TIMEOUT=1 (тогда значения
	// AutoReloadTimeoutSec работают с прежним clamp 30..600).
	if !allowReloadTimeout() {
		return 0
	}
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

// allowReloadTimeout — R83/v62: рубильник оператора для капа на перезагрузку.
func allowReloadTimeout() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("LB_ALLOW_NCTX_RELOAD_TIMEOUT"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// reloadTimeoutContext — контекст операции перезагрузки. d <= 0 (по умолчанию)
// означает «без капа»: context.WithCancel, а НЕ WithTimeout(0), который истёк бы
// мгновенно. При взведённом капе печатается WARN с источником.
func reloadTimeoutContext(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		return context.WithCancel(parent)
	}
	if logger.Get() != nil {
		logger.Get().Warnw("armed nctx reload timeout (operator opt-in)",
			"timeout", d.String(), "source", "LB_ALLOW_NCTX_RELOAD_TIMEOUT")
	}
	return context.WithTimeout(parent, d)
}

// PreflightAsyncRetryAfterCeilingDefaultSec — R549 (T-6 аудита): дефолтный
// ПОТОЛОК Retry-After при async reload.
//
// ДО R549 потолок был 120 с — и расходился с собственным эстиматором балансера:
// `EstimateReloadTimeMs` для 16.4 ГБ / 131072 даёт ≈186 с
// (docs/plans/2026-11-19-webui-and-balancer-fixes.md §8.1), а живые reload'ы на
// стенде — 280–480 с. Клиент, послушавшись `Retry-After: 120`, возвращался, пока
// первый reload ещё шёл, получал «reload in progress» и запускал ВТОРОЙ — ровно
// тот cascading retry, от которого защищал R60.6.
//
// Теперь потолок равен ИЗМЕРЕННОМУ верху живых reload'ов (480 с), и это же число
// ограничивает оценку. Явная настройка оператора
// (`PreflightAsyncRetryAfterMaxSec` / env
// `LB_NCTX_RELOAD_PREFLIGHT_ASYNC_RETRY_AFTER_MAX_SEC`) по-прежнему сильнее.
const PreflightAsyncRetryAfterCeilingDefaultSec = 480

// effectiveAsyncRetryAfterCeiling — потолок Retry-After (0/пусто = дефолт R549).
func (c NCtxReloadConfig) effectiveAsyncRetryAfterCeiling() int {
	if maxRA := c.PreflightAsyncRetryAfterMaxSec; maxRA > 0 {
		return maxRA
	}
	return PreflightAsyncRetryAfterCeilingDefaultSec
}

// asyncRetryAfterFromEstimate — ЕДИНАЯ формула «сколько сказать клиенту ждать»
// (R549, T-6). estimatedMs — оценка времени reload (EstimateReloadTimeMs или
// `estimatedLoadTimeMs` из тела воркера); 0 = оценки нет, берём базовое значение.
//
// ЗАЧЕМ ОТДЕЛЬНАЯ ФУНКЦИЯ. До R549 одна и та же логика «база → оценка → потолок»
// была написана ДВАЖДЫ (здесь и в preflight_nctx.go), причём со своим числом 120
// в каждой копии — классический дефект аудита «одна величина — разные источники».
func asyncRetryAfterFromEstimate(cfg NCtxReloadConfig, estimatedMs int64) int {
	retryAfter := cfg.effectiveAsyncRetryAfter()
	if estimatedMs <= 0 {
		return retryAfter
	}
	estSec := int((estimatedMs + 999) / 1000) // вверх до целых секунд
	if estSec > retryAfter {
		retryAfter = estSec
	}
	if ceiling := cfg.effectiveAsyncRetryAfterCeiling(); retryAfter > ceiling {
		retryAfter = ceiling
	}
	return retryAfter
}

// effectiveAsyncRetryAfter — сколько секунд клиенту ждать после 503
// перед retry. Default 5, clamp [2, effectiveAsyncRetryAfterCeiling()].
//
// R60.6 (2026-09-07): max clamp 30 → 120s. Иначе client (OpenWebUI) делал
// retry через 30s пока cppworker ещё грузил 5GB+131072 n_ctx (60-180s).
// Cascading retries → несколько reload goroutines → 503 spam в логах.
// R549 (T-6): потолок по умолчанию 120 → 480 с (измеренный верх живых reload'ов);
// см. PreflightAsyncRetryAfterCeilingDefaultSec.
func (c NCtxReloadConfig) effectiveAsyncRetryAfter() int {
	r := c.PreflightAsyncRetryAfterSec
	if r <= 0 {
		r = 5
	}
	if r < 2 {
		r = 2
	}
	maxRA := c.effectiveAsyncRetryAfterCeiling()
	if r > maxRA {
		r = maxRA
	}
	return r
}

// effectivePreflightMaxWait — максимальное время polling для async load.
// Default 900s (15 min), clamp [5, 3600]. Round 35c: env-конфигурируемый
// cap чтобы покрыть медленные GPU / 70B+ модели.
//
// Специально: min снижен до 5s (раньше был 60s) чтобы unit-тесты могли
// использовать маленький cap через ENV override (LB_NCTX_PREFLIGHT_MAX_WAIT_SEC=3
// например). Production значения по умолчанию 900s всё равно покрывают A10.
func (c NCtxReloadConfig) effectivePreflightMaxWait() time.Duration {
	t := c.PreflightMaxWaitSec
	if t <= 0 {
		t = 900
	}
	if t < 5 {
		t = 5
	}
	if t > 3600 {
		t = 3600
	}
	return time.Duration(t) * time.Second
}

// effectivePreflightWaitMultiplier — множитель для оценки load time.
// Default 2, clamp [1, 6]. Round 35c: для медленных моделей с auto-offload
// (22GB Qwen3.6 на 8GB VRAM) cppworker estimation слишком оптимистичен,
// 3-4 даёт надёжный запас.
func (c NCtxReloadConfig) effectivePreflightWaitMultiplier() int {
	m := c.PreflightWaitMultiplier
	if m <= 0 {
		m = 2
	}
	if m < 1 {
		m = 1
	}
	if m > 6 {
		m = 6
	}
	return m
}

// effectivePreflightWaitBuffer — доп. буфер после `multiplier*est`.
// Default 60s (если поле <= 0 и не задано явно). Clamp [0, 600]. Round 35c.
//
// Специально: явное 0 сохраняется (тест может установить 0 через ENV), а
// не заменяется на default. Используем проверку < 0 для default.
func (c NCtxReloadConfig) effectivePreflightWaitBuffer() time.Duration {
	b := c.PreflightWaitBufferSec
	if b < 0 {
		b = 60
	}
	if b > 600 {
		b = 600
	}
	return time.Duration(b) * time.Second
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
	// EffectiveCeiling — R267: потолок авто-роста, с которым ПРИНЯТО решение
	// (0 = ограничения нет). Отдаётся наружу, чтобы причина отказа/клампа была
	// видна в ответе API, а не только в логе.
	EffectiveCeiling int
	// LimitedBy — R267: что именно связало окно: "env" (LB_NCTX_RELOAD_MAX_N_CTX),
	// "profile" (заявленное оператором окно подняло потолок), "physical"
	// (VRAM/GGUF). Пусто = ограничение не участвовало.
	LimitedBy string
}

// ============================================================

// ============================================================

// ? llamacpp_transport.go.
type NCtxReloadCoordinator struct {
	// Поля с указателями — первыми (govet fieldalignment: GC scan prefix).
	perBackend  map[string]*backendReloadState
	reloadDedup *reloadDedupRegistry
	// R69 (2026-09-23): сколько n_ctx просил КЛИЕНТ (max за окно
	// desiredNCtxTTL). Нужно, чтобы AutoTune (R54.4 over-allocation fix) не
	// «оптимизировал» n_ctx вниз под запрос клиента: на живом стенде это давало
	// пинг-понг reload 65536 → 38385 → 65536 → … и клиент (Cline) получал 503
	// «reload in progress» на каждом запросе.
	desired map[string]desiredNCtxEntry
	// R70 (2026-09-24): поставщик «хинтов» для reload'а (тип KV-cache и т.п.).
	// Ставится Proxy'ем после создания координатора; см. ReloadHints.
	reloadHints func(backendID, modelName string) ReloadHints
	// R493 (2026-11-17): последняя проверка перед стартом перезагрузки
	// (занятость бэкенда). Ставится Proxy'ем; см. SetBeforeReloadGuard.
	beforeReload func(backendID, modelName string) (bool, string)

	metricsByBackend sync.Map

	mu        sync.RWMutex
	desiredMu sync.Mutex
	config    NCtxReloadConfig
}

// desiredNCtxTTL — окно, в течение которого запрос клиента считается актуальным.
// 30 минут: сессия Cline/OpenWebUI живёт часами, но держать «желание» вечно
// нельзя — иначе AutoTune никогда не сможет освободить VRAM.
const desiredNCtxTTL = 30 * time.Minute

// desiredNCtxEntry — запомненный клиентский n_ctx. Порядок полей — под
// fieldalignment (time.Time несёт указатель на Location).
type desiredNCtxEntry struct {
	at   time.Time
	nCtx int
}

type backendReloadState struct {
	inflightMu sync.Mutex
	inflight   *reloadInflight

	// lastKnownNCtx — окно загруженной модели НА СЛОТ (R505 / аудит N-6).
	//
	// Одна единица для всех писателей и читателей: preflight сравнивает с этим
	// числом промпт одного клиента, резолвер выдаёт его как окно запроса.
	// Суммарное окно (ContextSize из /api/models, ответ reload'а) обязано быть
	// пересчитано через perSeqFromTotal ДО записи — иначе preflight видит вдвое
	// большее окно, чем есть у слота, и пропускает невлезающий промпт.
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
		desired:     make(map[string]desiredNCtxEntry),
	}
}

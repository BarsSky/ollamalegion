// Package balancer — Adaptive n_ctx auto-reload
//
// Файл реализует Stage 2 из плана «Адаптивный n_ctx auto-reload» (см.
// plan_mode_respond). Решает: можно ли auto-reload модели на бэкенде
// с большим n_ctx (если VRAM позволяет), или нужно вернуть клиенту
// HTTP 413 с подробным JSON.
//
// Архитектура:
//   1. C-bridge (c/bridge/bridge.c) при n_ctx overflow возвращает
//      BRIDGE_ERR_N_CTX_NEEDS_RELOAD (=2) с заполненным BridgeErrorInfo.
//   2. Go-обёртка (c/bridge/bridge.go) пробрасывает sentinel ErrNCtxNeedsReload.
//   3. llamacpp_transport.go ловит этот sentinel после проксирования
//      запроса на cppworker, читает GetLastErrorInfo() и зовёт
//      DecideReloadBackend(...) из этого файла.
//   4. Если решение = Reload: POST /api/models/reload на cppworker с новым n_ctx,
//      дождаться завершения, повторить исходный запрос.
//   5. Если решение = Reject: вернуть клиенту 413/400 с подробным JSON.
//
// Решение «безопасно по VRAM» (это выбранный пользователем критерий):
//   * required_n_ctx <= max_vram_n_ctx * safety_factor (default 0.85) → Reload.
//   * required_n_ctx > max_vram_n_ctx * safety_factor → Reject (HTTP 413).
//
// Потокобезопасность:
//   * perBackend map защищён sync.RWMutex
//   * inflightMu защищает от параллельных reload-ов одного бэкенда
//     (если пришло 2 запроса одновременно с n_ctx overflow — второй
//     ждёт завершения первого через shared channel, а потом
//     переиспользует результат вместо нового reload).
package balancer

// Этот пакет НЕ импортирует c/bridge напрямую (balancer работает в
// stub-режиме). Вместо этого мы определяем локальный NCtxBridgeError,
// который зеркалит поля c/bridge.BridgeErrorInfo. Заполняется он
// из llamacpp_transport.go (где уже есть импорт c/bridge).
import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// NCtxBridgeError — зеркало c/bridge.BridgeErrorInfo для использования
// в balancer (без прямого импорта c/bridge). Поля и коды должны
// оставаться синхронизированными с c/bridge/bridge.go (константы
// ErrCode*). Заполняется на стороне llamacpp_transport.go, который
// импортирует c/bridge, после чего передаётся в DecideReloadBackend.
type NCtxBridgeError struct {
	Code         int    // один из NCtxErrCode* ниже
	CurrentNCtx  int    // фактический n_ctx загруженной модели
	RequiredNCtx int    // минимальный n_ctx, который нужен для запроса
	ActualTokens int    // размер prompt в токенах
	NPredict     int    // запрошенное число генерируемых токенов
	NCtxOverride int    // значение n_ctx_override из params (0 если не задан)
	MaxVRAMNCtx  int    // оценочный максимум n_ctx для текущей VRAM (0 если неизвестно)
	Message      string // человекочитаемое описание ошибки
}

// Коды ошибок (должны совпадать с c/bridge/bridge.go).
const (
	NCtxErrCodeOK              = 0
	NCtxErrCodeGeneric         = 1
	NCtxErrCodeNCtxNeedsReload = 2
	NCtxErrCodePromptTooLong   = 3
	NCtxErrCodeGPUOOM          = 4
	NCtxErrCodeBadRequest      = 5
)

// ============================================================
// Конфигурация auto-reload
// ============================================================

// NCtxReloadConfig — фич-флаги для адаптивного n_ctx auto-reload.
// Заполняется из config.json (см. internal/config) или из ENV
// (OLLAMALEGION_NCTX_RELOAD_*). Все bool-флаги имеют безопасный default
// (false = выключено), чтобы фича не активировалась случайно.
type NCtxReloadConfig struct {
	// AutoReloadNCtx — глобальный kill-switch. Если false, balancer
	// НЕ пытается auto-reload и сразу возвращает 413 клиенту.
	AutoReloadNCtx bool `json:"auto_reload_n_ctx" yaml:"auto_reload_n_ctx"`

	// AutoReloadMaxNCtx — верхний предел n_ctx, до которого balancer
	// имеет право auto-reload. Защита от абсурдных значений
	// (например, клиент прислал num_ctx=10000000). 0 = без лимита.
	AutoReloadMaxNCtx int `json:"auto_reload_max_n_ctx" yaml:"auto_reload_max_n_ctx"`

	// AutoReloadVRAMSafetyFactor — доля от max_vram_n_ctx, которую мы
	// готовы занять KV-cache. 0.85 = 85% от теоретического максимума.
	// Остальное — overhead на аллокации, другое ПО в GPU, и т.п.
	// 0 = использовать default 0.85.
	AutoReloadVRAMSafetyFactor float64 `json:"auto_reload_vram_safety_factor" yaml:"auto_reload_vram_safety_factor"`

	// AutoReloadTimeoutSec — таймаут на сам HTTP reload-запрос
	// (cppworker может грузить модель 30-60 секунд). 0 = default 60.
	AutoReloadTimeoutSec int `json:"auto_reload_timeout_sec" yaml:"auto_reload_timeout_sec"`
}

// DefaultNCtxReloadConfig — безопасные defaults для фич-флагов.
// AutoReloadNCtx = false (выключено), пока оператор явно не включит.
func DefaultNCtxReloadConfig() NCtxReloadConfig {
	return NCtxReloadConfig{
		AutoReloadNCtx:             false,
		AutoReloadMaxNCtx:          0,
		AutoReloadVRAMSafetyFactor: 0.85,
		AutoReloadTimeoutSec:       60,
	}
}

// effectiveSafetyFactor возвращает safety factor (>= 0.1, <= 1.0)
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

// effectiveTimeout возвращает таймаут для HTTP reload (>= 5 сек, <= 600 сек)
func (c NCtxReloadConfig) effectiveTimeout() time.Duration {
	t := c.AutoReloadTimeoutSec
	if t <= 0 {
		t = 60
	}
	if t < 5 {
		t = 5
	}
	if t > 600 {
		t = 600
	}
	return time.Duration(t) * time.Second
}

// ============================================================
// Решения, возвращаемые DecideReloadBackend
// ============================================================

// ReloadDecision — что делать, когда пришёл n_ctx overflow.
type ReloadDecision int

const (
	// DecisionNoOp — ничего не делать (n_ctx_override не задан или
	// ошибка не про n_ctx). Клиент получит оригинальную ошибку как есть.
	DecisionNoOp ReloadDecision = iota
	// DecisionReload — выполнить POST /api/models/reload на бэкенде
	// с newNCtx, дождаться завершения, повторить запрос клиента.
	DecisionReload
	// DecisionReject — VRAM не позволяет. Возвращаем клиенту 413
	// с подробным JSON (suggestion, current, max_vram).
	DecisionReject
)

// String для логирования
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

// ReloadPlan — результат решения + параметры.
type ReloadPlan struct {
	Decision  ReloadDecision
	NewNCtx   int    // n_ctx, до которого reload'ить
	Reason    string // человекочитаемое объяснение (для лога)
	RejectMsg string // тело JSON для 413-ответа (если DecisionReject)
}

// ============================================================
// Глобальный реестр / координатор reload
// ============================================================

// NCtxReloadCoordinator — единый реестр на процесс. Хранит
// per-backend состояние reload-а (in-flight защита + кэш «уже
// загруженный n_ctx»). Создаётся в NewProxy и передаётся по ссылке
// в llamacpp_transport.go.
type NCtxReloadCoordinator struct {
	mu         sync.RWMutex
	config     NCtxReloadConfig
	perBackend map[string]*backendReloadState
	// metricsByBackend — счётчики метрик по каждому бэкенду (sync.Map
	// используется для ленивого создания без блокировки reload-инфраструктуры).
	metricsByBackend sync.Map
}

type backendReloadState struct {
	// inflightMu защищает in-flight reload (только один одновременно
	// на бэкенд). Если кто-то уже reload'ит — другие ждут через
	// reloadDone channel и переиспользуют результат.
	inflightMu sync.Mutex
	inflight   *reloadInflight
	// lastKnownNCtx — кэш «с каким n_ctx сейчас загружена модель».
	// Обновляется после успешного reload. Используется для принятия
	// решения «нужен ли reload вообще» (если lastKnownNCtx уже >=
	// required, можно просто повторить запрос без reload).
	lastKnownNCtx int
}

type reloadInflight struct {
	done chan struct{} // закрывается когда reload завершён (успех или ошибка)
	err  error
}

// NewNCtxReloadCoordinator создаёт координатор с переданным конфигом.
func NewNCtxReloadCoordinator(cfg NCtxReloadConfig) *NCtxReloadCoordinator {
	return &NCtxReloadCoordinator{
		config:     cfg,
		perBackend: make(map[string]*backendReloadState),
	}
}

// Config возвращает текущий конфиг (для логирования и reload из SIGHUP).
func (c *NCtxReloadCoordinator) Config() NCtxReloadConfig {
	return c.config
}

// SetConfig обновляет конфиг на лету (для hot-reload из SIGHUP).
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

// SetLastKnownNCtx обновляет кэш «текущий n_ctx на бэкенде»
// (вызывается после успешного LoadModel / reload).
func (c *NCtxReloadCoordinator) SetLastKnownNCtx(backendID string, nCtx int) {
	if nCtx <= 0 {
		return
	}
	s := c.state(backendID)
	s.inflightMu.Lock()
	s.lastKnownNCtx = nCtx
	s.inflightMu.Unlock()
}

// LastKnownNCtx возвращает кэшированный n_ctx (0 = unknown).
func (c *NCtxReloadCoordinator) LastKnownNCtx(backendID string) int {
	s := c.state(backendID)
	s.inflightMu.Lock()
	defer s.inflightMu.Unlock()
	return s.lastKnownNCtx
}

// ============================================================
// DecideReloadBackend — главная логика решения
// ============================================================

// DecideReloadBackend принимает:
//   - backendID (имя бэкенда, нужно для координации reload)
//   - bridgeErr (структурированная ошибка от C-bridge)
//   - requestedNCtxOverride (override из body клиента, 0 если не задан;
//     именно его наличие запускает n_ctx-reload логику)
//
// и возвращает ReloadPlan. План говорит, что делать дальше.
//
// Безопасность: если AutoReloadNCtx=false, всегда NoOp. Это позволяет
// оператору «выключить» фичу без перекомпиляции.
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

	// Только для BRIDGE_ERR_N_CTX_NEEDS_RELOAD решаем что-то.
	// BRIDGE_ERR_PROMPT_TOO_LONG — это hard error, не наш случай.
	if bridgeErr == nil || bridgeErr.Code != NCtxErrCodeNCtxNeedsReload {
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

	// Если backend уже загружен с n_ctx, который покрывает required,
	// никакого reload не нужно — клиент может просто повторить запрос
	// (или cppworker при следующем инференсе увидит, что n_ctx хватает).
	if bridgeErr.CurrentNCtx > 0 && required <= bridgeErr.CurrentNCtx {
		return &ReloadPlan{
			Decision: DecisionNoOp,
			Reason: fmt.Sprintf("required n_ctx=%d already covered by current=%d (no reload needed)",
				required, bridgeErr.CurrentNCtx),
		}
	}

	// Защита от абсурдных значений
	if cfg.AutoReloadMaxNCtx > 0 && required > cfg.AutoReloadMaxNCtx {
		return c.makeRejectPlan(backendID, bridgeErr, required,
			fmt.Sprintf("required n_ctx=%d exceeds configured max=%d", required, cfg.AutoReloadMaxNCtx))
	}

	// VRAM-оценка от C-bridge (bridge.c)
	//
	// Цепочка делегирования расчёта max_vram_n_ctx:
	//   1. C-bridge (bridge.c) рассчитывает оценку на основе реального VRAM
	//      и формул KV Cache с учётом GQA (Grouped Query Attention).
	//      Формула (f16): 4 * n_layers * n_kv_heads * head_dim bytes/token.
	//      head_dim = n_embd / n_heads.
	//      Для MHA (n_kv_heads == n_heads): 4 * n_layers * n_embd.
	//      Для GQA (n_kv_heads < n_heads): значительно меньше.
	//   2. Если bridge не смог получить VRAM информацию (CPU-only, не CUDA,
	//      или CUDA API ошибка) — max_vram_n_ctx = 0. В этом случае
	//      Go-сторона не берёт на себя риск и reject'ит запрос.
	//   3. Если bridge вернул max_vram_n_ctx = 0, но модель загружена с CUDA
	//      (bridge_err.CurrentNCtx > 0), это означает, что модель не влезает
	//      в VRAM полностью — свободной памяти нет даже для одного токена
	//      KV-cache. В этом случае reject + совет уменьшить n_gpu_layers
	//      (CPU-offload), чтобы освободить VRAM для контекста.
	//   4. Go-сторона (internal/cppbackend/backend.go) имеет свой GGUF-парсер
	//      readGGUFHeaderInfo() и estimateKVCacheMB() для автономного
	//      расчёта KV Cache без загрузки модели в llama.cpp. Эти функции
	//      используются для pre-load VRAM check (checkVRAMForModel),
	//      а не для nctx_reload — reload требует уже загруженной модели
	//      с известной архитектурой, о которой C-bridge знает из llama_model_*().
	//
	// "Грубую ошибку оставлять нельзя": исправленная GQA-формула в bridge.c
	// корректно обрабатывает Llama 70B (n_kv_heads=8) и другие GQA-модели.
	// Снят опасный clamp, который маскировал estimated_max=0 → max_vram_n_ctx=4096.
	maxVRAMNCtx := bridgeErr.MaxVRAMNCtx
	if maxVRAMNCtx <= 0 {
		// Если CurrentNCtx > 0, модель загружена с CUDA, но VRAM не хватает
		// на KV-cache. Предлагаем CPU-offload.
		if maxVRAMNCtx == 0 && bridgeErr.CurrentNCtx > 0 {
			return c.makeRejectPlan(backendID, bridgeErr, required,
				"model fits in VRAM but no space remains for KV-cache (max_vram_n_ctx=0). "+
					"Try reducing n_gpu_layers (CPU-offload) to free VRAM for context, "+
					"or use a smaller model / increase physical VRAM")
		}
		// Backend не сообщил (CPU-only? llama.cpp без CUDA? CUDA API failed?).
		// Безопасный fallback: reject, чтобы не вызвать OOM.
		return c.makeRejectPlan(backendID, bridgeErr, required,
			"backend did not report max_vram_n_ctx (CPU-only, unknown GPU, or CUDA-query failed); cannot safely auto-reload")
	}
	safety := cfg.effectiveSafetyFactor()
	safeMax := int(float64(maxVRAMNCtx) * safety)
	if required > safeMax {
		return c.makeRejectPlan(backendID, bridgeErr, required,
			fmt.Sprintf("required n_ctx=%d exceeds safe VRAM limit=%d (max_vram_n_ctx=%d, safety=%.2f)",
				required, safeMax, maxVRAMNCtx, safety))
	}

	// Round up до степени 2 (типичная llama.cpp рекомендация; экономит память)
	// 4096 → 4096, 5000 → 8192, 8000 → 8192, 12000 → 16384
	newNCtx := roundUpPow2(required)
	// Не меньше запрошенного
	if newNCtx < required {
		newNCtx = required
	}
	// Не больше safeMax
	if newNCtx > safeMax {
		newNCtx = safeMax
	}
	// Не больше configured max
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

// makeRejectPlan собирает ReloadPlan + детальный JSON для 413-ответа
func (c *NCtxReloadCoordinator) makeRejectPlan(
	backendID string,
	bridgeErr *NCtxBridgeError,
	required int,
	reason string,
) *ReloadPlan {
	cfg := c.Config()
	safe := 0
	if bridgeErr.MaxVRAMNCtx > 0 {
		safe = int(float64(bridgeErr.MaxVRAMNCtx) * cfg.effectiveSafetyFactor())
	}
	rej := map[string]interface{}{
		"error":            "n_ctx_too_large_for_backend",
		"backend_id":       backendID,
		"reason":           reason,
		"requested_n_ctx":  required,
		"current_n_ctx":    bridgeErr.CurrentNCtx,
		"max_vram_n_ctx":   bridgeErr.MaxVRAMNCtx,
		"safe_max_n_ctx":   safe,
		"suggestion":       "save a model profile with a larger n_ctx and reload manually, or send a smaller options.num_ctx in the request",
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

// roundUpPow2 — округление вверх до ближайшей степени 2.
// Минимум 512 (меньше llama.cpp не позволяет).
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
// Выполнение reload-а на бэкенде
// ============================================================

// NCtxReloadHTTPClient — минимальный HTTP-клиент для cppworker
// (выделено в интерфейс для удобства мокинга в тестах).
type NCtxReloadHTTPClient interface {
	PostReload(ctx context.Context, endpoint string, payload []byte) (*http.Response, error)
}

// DefaultNCtxReloadHTTPClient — реальная реализация.
type DefaultNCtxReloadHTTPClient struct {
	HTTPClient *http.Client
}

// PostReload — POST на endpoint с payload, отдаёт *http.Response
// (caller обязан закрыть body).
func (c *DefaultNCtxReloadHTTPClient) PostReload(ctx context.Context, endpoint string, payload []byte) (*http.Response, error) {
	client := c.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 90 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Body = io.NopCloser(bytesReader(payload))
	req.ContentLength = int64(len(payload))
	return client.Do(req)
}

// bytesReader — локальный helper (избегаем import bytes глобально)
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

// ErrReloadInProgress — другой запрос уже reload'ит этот бэкенд.
// Caller должен либо дождаться (через shared channel), либо вернуть 503.
var ErrReloadInProgress = errors.New("n_ctx reload already in progress on this backend")

// DoReload — выполняет план reload-а на бэкенде. Потокобезопасно:
// только один reload на бэкенд одновременно. Параллельные вызовы
// ждут завершения текущего reload-а и возвращают тот же результат
// (если он успешный) — это предотвращает «5 клиентов одновременно
// запросили reload с 8K до 32K → 5 параллельных reload-ов».
func (c *NCtxReloadCoordinator) DoReload(
	ctx context.Context,
	backendID string,
	backendAddr string,
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
		// Уже идёт reload. Ждём его завершения.
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
	// Захватываем inflight-слот
	state.inflight = &reloadInflight{done: make(chan struct{})}
	cur := state.inflight
	state.inflightMu.Unlock()

	defer func() {
		state.inflightMu.Lock()
		state.inflight = nil
		state.inflightMu.Unlock()
		close(cur.done)
	}()

	logger.Get().Infof("[nctx_reload] backend %s: reloading with n_ctx=%d (%s)",
		backendID, plan.NewNCtx, plan.Reason)

	payload, _ := json.Marshal(map[string]interface{}{
		"backend_id":   backendID,
		"context_size": plan.NewNCtx,
		"reason":       "auto-reload: client request exceeded current n_ctx",
	})
	endpoint := backendAddr + "/api/models/reload"
	resp, err := loader.PostReload(rctx, endpoint, payload)
	if err != nil {
		cur.err = fmt.Errorf("reload HTTP request failed: %w", err)
		logger.Get().Errorf("[nctx_reload] backend %s: %v", backendID, cur.err)
		return cur.err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		cur.err = fmt.Errorf("reload returned HTTP %d: %s", resp.StatusCode, string(body))
		logger.Get().Errorf("[nctx_reload] backend %s: %v", backendID, cur.err)
		return cur.err
	}

	// Парсим ответ для извлечения фактического n_ctx
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
// Публичный API для метрик (Stage 5)
// ============================================================

// perBackendMetrics — счётчики для конкретного бэкенда.
// Snapshot() в /metrics endpoint склеивает их в общую картину.
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

// Snapshot возвращает копию счётчиков для бэкенда.
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

// RecordDecision — учёт решения координатора (для метрик).
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
		s.reloadsTotal++ // increment на старте; success/failed корректируется в RecordReloadDuration
		s.lastReloadAt = time.Now()
	}
}

// RecordReloadDuration — фиксирует длительность reload-а (для avg).
// Если err != nil — увеличивает errorsTotal.
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

// RecordError — учёт ошибки (не привязанной к reload, e.g. backend not found).
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

// perBackendState — ленивое создание perBackendMetrics по backendID.
// Отдельный sync.Map (а не часть основного perBackend) чтобы не блокировать
// reload-инфраструктуру на запись метрик.
func (c *NCtxReloadCoordinator) perBackendState(backendID string) *perBackendMetrics {
	if existing, ok := c.metricsByBackend.Load(backendID); ok {
		return existing.(*perBackendMetrics)
	}
	actual, _ := c.metricsByBackend.LoadOrStore(backendID, &perBackendMetrics{})
	return actual.(*perBackendMetrics)
}

// Snapshot — снапшот всех метрик (для /api/metrics endpoint).
// Возвращает плоский map с глобальными счётчиками + вложенный map perBackend.
//
// Безопасен для nil-получателя: возвращает каркас со всеми известными
// ключами (=0), чтобы обработчики метрик могли читать их без nil-чеков
// (см. TestNCtxReloadCoordinator_Snapshot_NilSafe).
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

// BackendMetricsFromState возвращает текущее состояние backend (для
// интеграции в BackendMetrics). TODO: добавить поле в types.BackendMetrics
// в следующей итерации.
func (c *NCtxReloadCoordinator) BackendMetricsFromState(backendID string) *types.BackendMetrics {
	// Заглушка — полная интеграция с types.BackendMetrics будет позже.
	return nil
}

// Shutdown — освобождает ресурсы координатора. Используется в тестах
// (nctx_reload_test.go) и в graceful-shutdown балансера. В текущей
// реализации координатор не имеет фоновых горутин, поэтому метод
// является no-op; добавлен для будущей совместимости (если появятся
// периодические задачи / rate limiter / dedup).
//
// БезопасNoOp: метод можно вызывать на nil-получателе (nil-guard
// присутствует для совместимости с другими метриками координатора).
func (c *NCtxReloadCoordinator) Shutdown() {
	if c == nil {
		return
	}
	// На текущий момент — no-op. Резерв на будущее: c.mu.Lock(); close(c.done)
}

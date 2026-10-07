package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"ollama-loadbalancer/pkg/protocol"
	"ollama-loadbalancer/pkg/types"
)

// Agent - агент сбора метрик
type Agent struct {
	config         *types.AgentConfig
	httpClient     *http.Client
	metricsSeq     int64
	heartbeatSeq   int64
	startTime      time.Time
	mu             sync.Mutex
	currentMetrics *types.BackendMetrics
	stopChan       chan struct{}
	balancerURL    string
	registered     bool
	platformMode   types.PlatformMode
	// lastRegisterTry — R-MultiHost (2026-10-07): время последней ПОПЫТКИ
	// регистрации, включая автоматические повторные. Ограничивает частоту
	// (см. registerRetryCooldown), чтобы недоступный балансер не превращался
	// в шквал запросов.
	lastRegisterTry time.Time
	// gpuUnavailable cache вынесен в collector_gpu.go (TTL-based, не зависит от Agent).

	// Ollama статистика
	ollamaStats    *OllamaStats
	statsMu        sync.Mutex
	requestHistory []requestRecord

	// Текущие флаги Ollama (кэш)
	currentFlags types.OllamaRuntimeFlags

	// Кэш версии Ollama (TTL 5 минут)
	cachedVersion   string
	cachedVersionAt time.Time
	versionCacheTTL time.Duration

	// Llama-коллектор (для llama.cpp бэкендов)
	llamaCollector *LlamaCollector

	// Health HTTP-сервер (для Docker HEALTHCHECK)
	healthServer *http.Server

	// Log buffer for /api/logs endpoint
	logBufMu    sync.Mutex
	logBuffer   []string
	maxLogLines int

	// Pipeline decoupling: collect (CPU/IO-bound) and send (HTTP IO to
	// balancer) run in separate goroutines. R59.14 (2026-09-03) replaces
	// the previous single-goroutine model where a slow cppworker poll
	// blocked the next collect tick (and vice versa, a slow balancer
	// POST delayed the next collect). Buffer size = 1: if send is slow
	// the next collect tick drops its result (latest-wins), which is the
	// right policy for metrics — we don't want to queue stale data.
	metricsCh   chan *types.BackendMetrics
	sendStopped chan struct{}
	// R59.14: sendLoop is normally started by Start(), but tests call
	// collectAndSend() directly without Start(). sync.Once makes the
	// lazy start safe and idempotent.
	sendLoopOnce sync.Once
}

// requestRecord - запись о запросе для подсчета RPS
type requestRecord struct {
	timestamp time.Time
	duration  time.Duration
}

// NewAgent - создание нового агента
func NewAgent(config *types.AgentConfig) *Agent {
	a := &Agent{
		config: config,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		startTime:       time.Now(),
		stopChan:        make(chan struct{}),
		balancerURL:     config.BalancerURL,
		requestHistory:  make([]requestRecord, 0),
		versionCacheTTL: 5 * time.Minute,
		logBuffer:       make([]string, 0, 1000),
		maxLogLines:     1000,
		// R59.14: pipeline decoupling. Buffer size 1 means "drop oldest if
		// send is slow" — we always want the freshest metrics, not a queue
		// of stale ones. sendStopped signals the send goroutine to exit
		// when Stop() is called and the channel is closed.
		metricsCh:   make(chan *types.BackendMetrics, 1),
		sendStopped: make(chan struct{}),
		// sendLoopOnce: zero value (sync.Once{}), first call wins. Used
		// by collectAndSend() to lazy-start the send goroutine for tests
		// that don't go through Start().
	}
	// Инициализация llama-коллектора для llama.cpp бэкендов
	if config.BackendType == types.BackendTypeLlamaCpp {
		cppURL := config.CppWorkerURL
		if cppURL == "" {
			cppURL = "http://localhost:18091"
		}
		a.llamaCollector = NewLlamaCollector(cppURL)
		fmt.Printf("[%s] LlamaCollector initialized for %s\n", time.Now().Format(time.RFC3339), cppURL)
	}
	return a
}

// Start - запуск агента
func (a *Agent) Start() error {
	// Определяем режим платформы
	a.platformMode = a.detectPlatformMode()
	fmt.Printf("[%s] Platform mode detected: %s\n", time.Now().Format(time.RFC3339), a.platformMode)

	// Регистрация на балансировщике
	if err := a.register(); err != nil {
		return fmt.Errorf("registration failed: %w", err)
	}

	a.registered = true

	// Запуск pipeline: collect (CPU-bound) + send (HTTP IO) — две горутины,
	// общающиеся через metricsCh (R59.14). Раньше был один collectLoop
	// который делал collect + send последовательно; при медленном cppworker
	// или балансере следующий tick блокировался на завершении предыдущего.
	// sendLoop is also lazy-started by collectAndSend via sync.Once, so
	// it's safe even if a test calls collectAndSend() before Start().
	go a.collectLoop()
	a.startSendLoopOnce()

	// Запуск heartbeat
	go a.heartbeatLoop()

	// Запуск health HTTP-сервера (для Docker HEALTHCHECK)
	a.startHealthServer()

	return nil
}

// Stop - остановка агента
func (a *Agent) Stop() {
	close(a.stopChan)
	// R59.14: close metricsCh to signal sendLoop to drain and exit.
	// Drain semantics: any value currently buffered is sent before
	// sendLoop returns. After close, enqueueMetrics's non-blocking
	// select will panic on send-to-closed-channel — but the recover()
	// in enqueueMetrics swallows that, and we never enqueue after
	// Stop() returns, so the call sites are safe.
	close(a.metricsCh)
	// Wait for sendLoop to finish its last POST, with timeout. If
	// sendLoop was never started (e.g. test called Stop() without
	// Start()) this would deadlock; the timeout makes Stop() safe to
	// call without that prerequisite.
	select {
	case <-a.sendStopped:
		// sendLoop exited cleanly
	case <-time.After(2 * time.Second):
		// sendLoop was never started or stuck — don't block forever
	}
	// Остановка health-сервера
	if a.healthServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		a.healthServer.Shutdown(ctx)
	}
}

// detectPlatformMode - runtime автоопределение GPU/CPU режима.
//
// 2026-06-30: порядок проверок изменён, чтобы корректно работать в контейнере
// с runtime: nvidia (где нет nvidia-smi в PATH, но есть /dev/nvidia* и libnvidia-ml.so.1).
//
//  1. /dev/nvidia0 (Linux-контейнер с runtime nvidia) — самый дешёвый и надёжный признак.
//  2. /proc/driver/nvidia/version (нативный Linux с NVIDIA-драйвером, но без nvidia-smi).
//  3. nvmlAvailable() (через build tag `nvml`): NVML Init() + DeviceGetCount > 0.
//  4. nvidia-smi (последним: обычно НЕТ в base-образах nvidia/cuda, но может быть на хосте).
//
// Каждый шаг логируется с причиной успеха/неудачи, чтобы пользователь мог понять,
// почему GPU не обнаружен (vs. старый лог «No GPU detected» без объяснений).
func (a *Agent) detectPlatformMode() types.PlatformMode {
	now := time.Now().Format(time.RFC3339)

	// Если явно задан режим — уважаем override.
	if a.config.GPUMode != types.ModeAuto {
		fmt.Printf("[%s] Platform mode override: %s\n", now, a.config.GPUMode)
		return a.config.GPUMode
	}

	reasons := []string{} // накапливаем причины неудачи для финального лога

	// 1. /dev/nvidia0 — стандартный признак NVIDIA Container Toolkit с runtime: nvidia.
	if runtime.GOOS == "linux" {
		if _, err := os.Stat("/dev/nvidia0"); err == nil {
			fmt.Printf("[%s] GPU detected via /dev/nvidia0\n", now)
			return types.ModeGPU
		} else {
			reasons = append(reasons, "/dev/nvidia0: "+err.Error())
		}
	}

	// 2. /proc/driver/nvidia/version — нативный Linux без NVIDIA Container Toolkit.
	if runtime.GOOS == "linux" {
		if _, err := os.Stat("/proc/driver/nvidia/version"); err == nil {
			fmt.Printf("[%s] GPU detected via /proc/driver/nvidia/version\n", now)
			return types.ModeGPU
		} else {
			reasons = append(reasons, "/proc/driver/nvidia/version: "+err.Error())
		}
	}

	// 3. NVML (Go-nvml через CGO). Самый надёжный, если /dev/nvidia* ещё не
	// проброшены, но libnvidia-ml.so.1 уже смонтирована (редко, но бывает).
	//
	// 2026-06-30: добавлен дополнительный gate — nvmlAvailable() вызываем
	// ТОЛЬКО если /dev/nvidia0 существует (т.е. NVIDIA Container Toolkit
	// гарантированно смонтировал и устройства, и userspace-библиотеку).
	// Без этого gate'а в окружениях без libnvidia-ml.so.1 вызов nvml.Init()
	// падает с SIGSEGV (PC=0x0) из-за NULL-указателя от неудачного dlopen().
	if runtime.GOOS == "linux" {
		if _, err := os.Stat("/dev/nvidia0"); err == nil {
			if nvmlAvailable() {
				fmt.Printf("[%s] GPU detected via NVML (libnvidia-ml.so.1)\n", now)
				return types.ModeGPU
			} else {
				reasons = append(reasons, "nvml.Init() failed (см. лог NVML выше)")
			}
		} else {
			reasons = append(reasons, "/dev/nvidia0 отсутствует — NVML skip (защита от SIGSEGV)")
		}
	}

	// 4. nvidia-smi — последний шанс (на хосте без container runtime часто есть).
	if _, err := exec.LookPath("nvidia-smi"); err == nil {
		cmd := exec.Command("nvidia-smi", "--query-gpu=name", "--format=csv,noheader")
		if out, err := cmd.Output(); err == nil && len(bytes.TrimSpace(out)) > 0 {
			fmt.Printf("[%s] GPU detected via nvidia-smi\n", now)
			return types.ModeGPU
		} else {
			reasons = append(reasons, "nvidia-smi не вернул GPU")
		}
	} else {
		reasons = append(reasons, "nvidia-smi not in PATH")
	}

	// Ничего не нашли — подробный лог причин.
	fmt.Printf("[%s] No GPU detected, CPU mode. Причины: %s\n", now, strings.Join(reasons, "; "))
	return types.ModeCPU
}

// minCollectInterval — минимальный допустимый интервал сбора метрик
// для предотвращения DDoS Ollama API со стороны агента
const minCollectInterval = 10 * time.Second

// collectLoop - цикл сбора метрик
func (a *Agent) collectLoop() {
	interval := time.Duration(a.config.CollectInterval) * time.Second
	if interval < minCollectInterval {
		// Защита от слишком частого опроса: не чаще чем раз в 10 секунд
		fmt.Printf("[%s] WARNING: collect interval %v is below minimum %v, clamping to %v\n",
			time.Now().Format(time.RFC3339), interval, minCollectInterval, minCollectInterval)
		interval = minCollectInterval
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Первый сбор сразу
	a.collectAndSend()

	for {
		select {
		case <-ticker.C:
			// R59.14: collect is now decoupled from send. Slow cppworker
			// doesn't block the next tick — the send loop is in its own
			// goroutine and reads from a.metricsCh.
			a.collectAndSend()
		case <-a.stopChan:
			return
		}
	}
}

// sendLoop is the consumer half of the R59.14 pipeline. Runs in its
// own goroutine; reads *types.BackendMetrics from a.metricsCh, marshals
// to JSON, and POSTs to the balancer. Slow balancer doesn't block the
// next collect (the next tick drops the oldest value via the buffered
// channel's non-blocking send in enqueueMetrics below).
func (a *Agent) sendLoop() {
	for m := range a.metricsCh {
		a.sendOneMetrics(m)
	}
	close(a.sendStopped)
}

// startSendLoopOnce is called by collectAndSend() so that test code
// (which doesn't go through Start()) still gets the send goroutine
// running. sync.Once ensures the goroutine is spawned at most once
// per Agent, even with concurrent collectAndSend calls.
func (a *Agent) startSendLoopOnce() {
	a.sendLoopOnce.Do(func() { go a.sendLoop() })
}

// enqueueMetrics pushes a freshly-collected metrics into a.metricsCh
// with a non-blocking send. If the channel is full (send is still busy
// with the previous tick), we drop the previous value and queue the
// freshest one. This is the "latest-wins" policy: stale metrics are
// worse than missed metrics because they show the wrong "now".
func (a *Agent) enqueueMetrics(m *types.BackendMetrics) {
	// R59.14: защита от send-on-closed-channel panic. После Stop() канал
	// закрыт, но collectLoop может вызвать enqueueMetrics из последнего
	// тика (race между ticker.C и <-stopChan). Просто drop в этом случае.
	defer func() {
		_ = recover() // swallow "send on closed channel"
	}()
	select {
	case a.metricsCh <- m:
		// queued successfully
	default:
		// Channel full → previous value is being sent, drop it and
		// queue ours. This is the only place where we lose data, and
		// it's the right tradeoff: the next tick (in <CollectInterval)
		// will queue its own value.
		select {
		case <-a.metricsCh: // drain the old
		default:
		}
		select {
		case a.metricsCh <- m:
		default:
			// extremely unlikely (a goroutine just freed a slot between
			// the two selects); drop this value
		}
	}
}

// sendOneMetrics — POST one metrics payload to the balancer. Caller
// (sendLoop) is responsible for reading from the channel. This is the
// extracted "send a single metrics object" step.
func (a *Agent) sendOneMetrics(metrics *types.BackendMetrics) {
	data, err := json.Marshal(metrics)
	if err != nil {
		fmt.Printf("[%s] Failed to marshal metrics: %v\n", time.Now().Format(time.RFC3339), err)
		return
	}
	a.sendMetrics(data)
}

// collectAndSend - сбор и отправка метрик (R59.14: enqueue в pipeline,
// см. sendLoop).
func (a *Agent) collectAndSend() {
	// R59.14: lazy-start sendLoop if it hasn't been started yet. Test
	// code that calls collectAndSend() directly (without Start()) needs
	// the send goroutine running too. Use sync.Once for thread-safety.
	a.startSendLoopOnce()
	metrics := a.collectMetrics()

	// Логирование собранных метрик для диагностики
	if a.platformMode == types.ModeGPU {
		fmt.Printf("[%s] Metrics collected: CPU=%.1f%% RAM=%d/%dMB GPU=%.1f%% GPU_Mem=%d/%dMB RPS=%.1f Models=%d\n",
			time.Now().Format(time.RFC3339),
			metrics.System.CPUUsagePercent,
			metrics.System.MemoryUsed, metrics.System.MemoryTotal,
			metrics.GPU.UsagePercent,
			metrics.GPU.MemoryUsed, metrics.GPU.MemoryTotal,
			metrics.Ollama.RequestsPerSecond,
			len(metrics.Ollama.RunningModels),
		)
	} else {
		fmt.Printf("[%s] Metrics collected: CPU=%.1f%% Load=%.2f RAM=%d/%dMB RPS=%.1f Models=%d\n",
			time.Now().Format(time.RFC3339),
			metrics.System.CPUUsagePercent,
			metrics.System.CPU.LoadAverage1,
			metrics.System.MemoryUsed, metrics.System.MemoryTotal,
			metrics.Ollama.RequestsPerSecond,
			len(metrics.Ollama.RunningModels),
		)
	}

	a.mu.Lock()
	a.metricsSeq++
	seq := a.metricsSeq
	a.currentMetrics = metrics
	a.mu.Unlock()

	_ = protocol.NewMetricsMessage(a.config.AgentID, seq, metrics)

	// R59.14: enqueue для sendLoop (non-blocking, latest-wins). Раньше
	// здесь был синхронный sendMetrics(data).
	a.enqueueMetrics(metrics)
}

// registerRetryCooldown — R-MultiHost (2026-10-07): минимальный интервал между
// автоматическими повторными регистрациями.
const registerRetryCooldown = 20 * time.Second

// ensureRegistered — R-MultiHost (2026-10-07): самовосстановление регистрации.
//
// ЗАЧЕМ. Регистрация выполняется РОВНО ОДИН раз — в Agent.Start(). Если запись
// бэкенда на балансере исчезла (её удалили из WebUI, отобрал другой узел с тем
// же ID, или она потерялась при перезапуске), агент об этом никак не узнаёт:
// метрики и heartbeat продолжают уходить в никуда, а балансер отвечает
// 404 «Backend not found. Please register agent first.» Наблюдалось на живой
// паре: запись imageworker удалили, и воркер пропал со стенда НАВСЕГДА — до
// ручного `docker restart`. В WebUI это выглядело как «бэкенд то есть, то нет».
//
// Теперь на 404/403 от эндпоинтов телеметрии агент повторяет регистрацию
// (не чаще раза в registerRetryCooldown) и заново узнаёт свой backendId.
func (a *Agent) ensureRegistered(reason string) {
	a.mu.Lock()
	if !a.lastRegisterTry.IsZero() && time.Since(a.lastRegisterTry) < registerRetryCooldown {
		a.mu.Unlock()
		return
	}
	a.lastRegisterTry = time.Now()
	a.mu.Unlock()

	fmt.Printf("[%s] Re-registering with balancer (%s)\n",
		time.Now().Format(time.RFC3339), reason)

	if err := a.register(); err != nil {
		fmt.Printf("[%s] Re-registration failed: %v\n", time.Now().Format(time.RFC3339), err)
		return
	}
	fmt.Printf("[%s] Re-registration succeeded (backendId=%q)\n",
		time.Now().Format(time.RFC3339), a.config.BackendID)
}

// needsReregistration — ответ балансера означает, что запись бэкенда не наша
// или её больше нет. 404 — «Backend not found», 403 — запись принадлежит
// другому узлу (см. api/registration_guard.go).
func needsReregistration(status int) bool {
	return status == http.StatusNotFound || status == http.StatusForbidden
}

// sendMetrics - отправка метрик на балансировщик
func (a *Agent) sendMetrics(data []byte) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Round 15 (2026-07-10): если есть backendId (из register/attach), шлём на
	// /api/v1/backends/{backendId}/agent/metrics — правильный endpoint, который
	// обновляет метрики для canonical бэкенда. Иначе fallback на legacy
	// /api/v1/agents/metrics (standalone mode или старый балансер).
	var metricsURL string
	if a.config.BackendID != "" {
		metricsURL = fmt.Sprintf("%s/api/v1/backends/%s/agent/metrics", a.balancerURL, a.config.BackendID)
	} else {
		metricsURL = fmt.Sprintf("%s/api/v1/agents/metrics", a.balancerURL)
	}

	req, err := a.authedRequest(ctx, http.MethodPost, metricsURL, bytes.NewReader(data))
	if err != nil {
		fmt.Printf("[%s] Failed to create request: %v\n", time.Now().Format(time.RFC3339), err)
		return
	}

	resp, err := a.httpClient.Do(req)
	if err != nil {
		fmt.Printf("[%s] Failed to send metrics: %v\n", time.Now().Format(time.RFC3339), err)
		return
	}

	// Fallback: новый endpoint вернул 404 (старая версия балансера) → legacy
	if resp.StatusCode == http.StatusNotFound && a.config.BackendID != "" {
		resp.Body.Close()
		fmt.Printf("[%s] New metrics endpoint returned 404, falling back to legacy /agents/metrics\n",
			time.Now().Format(time.RFC3339))
		legacyURL := fmt.Sprintf("%s/api/v1/agents/metrics", a.balancerURL)
		req2, reqErr := a.authedRequest(ctx, http.MethodPost, legacyURL, bytes.NewReader(data))
		if reqErr == nil {
			resp2, err2 := a.httpClient.Do(req2)
			if err2 == nil {
				resp = resp2
			} else {
				fmt.Printf("[%s] Legacy metrics also failed: %v\n", time.Now().Format(time.RFC3339), err2)
				return
			}
		}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		fmt.Printf("[%s] Metrics send failed with status %d: %s\n",
			time.Now().Format(time.RFC3339), resp.StatusCode, string(body))
		// R-MultiHost: запись исчезла или принадлежит другому узлу —
		// восстанавливаем регистрацию, иначе воркер пропадёт со стенда
		// до ручного перезапуска контейнера.
		if needsReregistration(resp.StatusCode) {
			a.ensureRegistered(fmt.Sprintf("metrics returned %d", resp.StatusCode))
		}
	}
}

// heartbeatLoop - цикл heartbeat
func (a *Agent) heartbeatLoop() {
	interval := time.Duration(a.heartbeatInterval()) * time.Second
	if interval == 0 {
		interval = 3 * time.Second
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			a.sendHeartbeat()
		case <-a.stopChan:
			return
		}
	}
}

// heartbeatInterval - интервал heartbeat из конфига
func (a *Agent) heartbeatInterval() int {
	if a.config.HeartbeatInterval > 0 {
		return a.config.HeartbeatInterval
	}
	return 5
}

// sendHeartbeat - отправка heartbeat
func (a *Agent) sendHeartbeat() {
	uptime := int64(time.Since(a.startTime).Seconds())

	a.mu.Lock()
	a.heartbeatSeq++
	seq := a.heartbeatSeq
	status := "healthy"

	if a.currentMetrics != nil {
		if a.platformMode == types.ModeGPU {
			// GPU: degraded при высокой загрузке GPU или VRAM
			if a.currentMetrics.GPU.UsagePercent > 95 ||
				(a.currentMetrics.GPU.MemoryTotal > 0 && float64(a.currentMetrics.GPU.MemoryUsed)/float64(a.currentMetrics.GPU.MemoryTotal)*100 > 95) ||
				a.currentMetrics.GPU.Temperature > 90 {
				status = "degraded"
			}
		} else {
			// CPU: degraded при высоком load average или RAM
			cpu := a.currentMetrics.System.CPU
			ramPercent := float64(0)
			if a.currentMetrics.System.MemoryTotal > 0 {
				ramPercent = float64(a.currentMetrics.System.MemoryUsed) / float64(a.currentMetrics.System.MemoryTotal) * 100
			}
			if (cpu.CoreCount > 0 && cpu.LoadAverage1 > float64(cpu.CoreCount)*2) || ramPercent > 95 {
				status = "degraded"
			}
		}
	}
	a.mu.Unlock()

	_ = protocol.NewHeartbeatMessage(a.config.AgentID, seq, uptime, status, a.config.Weight)

	// Сериализация сообщения с локальными лимитами
	wrapper := map[string]interface{}{
		"type":                  "heartbeat",
		"agentId":               a.config.AgentID,
		"timestamp":             time.Now().UTC(),
		"uptime":                uptime,
		"sequence":              seq,
		"status":                status,
		"platform":              a.platformMode,
		"weight":                a.config.Weight,
		"maxConcurrentRequests": a.config.MaxConcurrentRequests,
		"maxModels":             a.config.MaxModels,
		"backendType":           string(a.config.BackendType),
		"nodeLabels":            a.config.NodeLabels,
		// Round 14 (2026-07-10): шлём свой agentPort чтобы cppworker-бэкенд
		// (созданный без знания про agent) получил правильный порт. Без этого
		// AttachAgentToBackend использовал backend.AgentPort=0 → UI показывал 0.
		"agentPort": a.config.MetricsPort,
	}

	data, err := json.Marshal(wrapper)
	if err != nil {
		fmt.Printf("[%s] Failed to marshal heartbeat: %v\n", time.Now().Format(time.RFC3339), err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Round 13 (2026-07-10): если есть backendId (из attach/register), шлём на новый
	// endpoint /backends/{id}/agent/heartbeat — он правильно идентифицирует
	// бэкенд (в отличие от /agents/heartbeat, который использовал agentID).
	// Fallback на legacy endpoint если новый вернёт 404 (старая версия балансера).
	var heartbeatURL string
	if a.config.BackendID != "" {
		heartbeatURL = fmt.Sprintf("%s/api/v1/backends/%s/agent/heartbeat", a.balancerURL, a.config.BackendID)
	} else {
		// Standalone режим: agentID == backendID, legacy endpoint работает.
		heartbeatURL = fmt.Sprintf("%s/api/v1/agents/heartbeat", a.balancerURL)
	}

	req, err := a.authedRequest(ctx, http.MethodPost, heartbeatURL, bytes.NewReader(data))
	if err != nil {
		fmt.Printf("[%s] Failed to create heartbeat request: %v\n", time.Now().Format(time.RFC3339), err)
		return
	}

	resp, err := a.httpClient.Do(req)
	if err != nil {
		fmt.Printf("[%s] Failed to send heartbeat: %v\n", time.Now().Format(time.RFC3339), err)
		return
	}
	// Fallback: если новый endpoint вернул 404, шлём на legacy.
	if resp.StatusCode == http.StatusNotFound && a.config.BackendID != "" {
		resp.Body.Close()
		fmt.Printf("[%s] New heartbeat endpoint returned 404, falling back to legacy /agents/heartbeat\n",
			time.Now().Format(time.RFC3339))
		legacyURL := fmt.Sprintf("%s/api/v1/agents/heartbeat", a.balancerURL)
		req2, reqErr := a.authedRequest(ctx, http.MethodPost, legacyURL, bytes.NewReader(data))
		if reqErr == nil {
			resp2, err2 := a.httpClient.Do(req2)
			if err2 == nil {
				resp = resp2
			} else {
				fmt.Printf("[%s] Legacy heartbeat also failed: %v\n", time.Now().Format(time.RFC3339), err2)
				return
			}
		}
	}
	defer resp.Body.Close()

	// R-MultiHost (2026-10-07): heartbeat — самый частый сигнал, поэтому именно
	// он быстрее всего сообщает, что запись бэкенда исчезла (404) или перешла
	// другому узлу (403). Без восстановления регистрации воркер остаётся
	// невидимым до ручного перезапуска контейнера.
	if needsReregistration(resp.StatusCode) {
		a.ensureRegistered(fmt.Sprintf("heartbeat returned %d", resp.StatusCode))
	}

	// Принимаем runtime-лимиты из ответа балансера
	if resp.Body != nil {
		var respBody map[string]interface{}
		bodyBytes, _ := io.ReadAll(resp.Body)
		resp.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))
		if err := json.Unmarshal(bodyBytes, &respBody); err == nil {
			if cfg, ok := respBody["config"].(map[string]interface{}); ok {
				if maxConcurrent, ok := cfg["maxConcurrentRequests"].(float64); ok {
					if maxConcurrent > 0 {
						a.config.MaxConcurrentRequests = int(maxConcurrent)
						fmt.Printf("[%s] Heartbeat response: applied maxConcurrentRequests=%d from balancer\n",
							time.Now().Format(time.RFC3339), int(maxConcurrent))
					} else {
						fmt.Printf("[%s] Heartbeat response: keeping local maxConcurrentRequests=%d (balancer returned %.0f)\n",
							time.Now().Format(time.RFC3339), a.config.MaxConcurrentRequests, maxConcurrent)
					}
				}
				if maxModels, ok := cfg["maxModels"].(float64); ok {
					if maxModels > 0 {
						a.config.MaxModels = int(maxModels)
					} else {
						fmt.Printf("[%s] Heartbeat response: keeping local maxModels=%d (balancer returned %.0f)\n",
							time.Now().Format(time.RFC3339), a.config.MaxModels, maxModels)
					}
				}
			}

			// Принимаем желаемую конфигурацию Ollama из ответа балансера
			if ollamaCfg, ok := respBody["ollamaConfig"].(map[string]interface{}); ok {
				applied := false
				flags := a.desiredOllamaFlags()

				if v, ok := ollamaCfg["numGpuLayers"].(float64); ok && int(v) >= 0 {
					flags.NumGPULayers = int(v)
					applied = true
				}
				if v, ok := ollamaCfg["contextLength"].(float64); ok && int(v) > 0 {
					flags.ContextLength = int(v)
					applied = true
				}
				if v, ok := ollamaCfg["numParallel"].(float64); ok && int(v) > 0 {
					flags.NumParallel = int(v)
					applied = true
				}
				if v, ok := ollamaCfg["numThreads"].(float64); ok && int(v) > 0 {
					flags.NumThreads = int(v)
					applied = true
				}
				if v, ok := ollamaCfg["batchSize"].(float64); ok && int(v) > 0 {
					flags.BatchSize = int(v)
					applied = true
				}
				if v, ok := ollamaCfg["maxLoadedModels"].(float64); ok && int(v) >= 0 {
					flags.MaxLoadedModels = int(v)
					applied = true
				}
				if v, ok := ollamaCfg["flashAttention"].(bool); ok {
					flags.FlashAttention = v
					applied = true
				}
				if v, ok := ollamaCfg["kvCacheQuant"].(string); ok && v != "" {
					flags.KVCacheQuant = v
					applied = true
				}

				if applied {
					if err := a.applyOllamaConfig(flags); err != nil {
						fmt.Printf("[%s] Heartbeat response: failed to apply ollamaConfig: %v\n",
							time.Now().Format(time.RFC3339), err)
					} else {
						fmt.Printf("[%s] Heartbeat response: applied ollamaConfig (ngl=%d, ctx=%d, parallel=%d, threads=%d, batch=%d, maxModels=%d)\n",
							time.Now().Format(time.RFC3339), flags.NumGPULayers, flags.ContextLength,
							flags.NumParallel, flags.NumThreads, flags.BatchSize, flags.MaxLoadedModels)
					}
				}
			}
		}
	}
}

// collectMetrics - сбор всех метрик
func (a *Agent) collectMetrics() *types.BackendMetrics {
	now := time.Now().UTC()

	metrics := &types.BackendMetrics{
		ID:        a.config.AgentID,
		Timestamp: now,
	}

	// Для llama.cpp бэкендов используем LlamaCollector вместо Ollama
	if a.config.BackendType == types.BackendTypeLlamaCpp && a.llamaCollector != nil {
		return a.collectLlamaMetrics(metrics)
	}

	// Сбор GPU метрик (только для GPU нод)
	if a.platformMode == types.ModeGPU {
		metrics.GPU = a.collectGPUMetrics()
	} else {
		metrics.GPU = types.GPUMetrics{}
	}

	// Сбор системных метрик
	metrics.System = a.collectSystemMetrics()

	// Сбор Ollama метрик
	metrics.Ollama = a.collectOllamaMetrics()

	return metrics
}

// collectLlamaMetrics — сбор метрик через LlamaCollector для llama.cpp бэкендов
func (a *Agent) collectLlamaMetrics(base *types.BackendMetrics) *types.BackendMetrics {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	llamaMetrics, err := a.llamaCollector.Collect(ctx)
	if err != nil {
		fmt.Printf("[%s] LlamaCollector failed: %v, falling back to system metrics\n",
			time.Now().Format(time.RFC3339), err)
		// Fallback: системные метрики
		base.System = a.collectSystemMetrics()
		base.Ollama = types.OllamaMetrics{RunningModels: []types.RunningModel{}}
		return base
	}

	// Маппим LlamaMetrics → BackendMetrics
	// GPU: приоритет — данные от CppWorker (llamaMetrics.GPUMetrics имеет актуальную информацию),
	// затем nvidia-smi/NVML (collectGPUMetrics), безусловно — даже если detectPlatformMode()
	// не смог найти драйверы на старте (NVML lib не найдена в контейнере, nvidia-smi
	// недоступен через PATH), мы всё равно пытаемся собрать метрики. Если ничего
	// не получилось — base.GPU остаётся нулевым и балансер не увидит GPU.
	if len(llamaMetrics.GPUMetrics) > 0 {
		// UUID физических карт дописываем к метрикам воркера: cppworker их не
		// отдаёт, а без них балансер удваивает память одной карты на хосте с
		// двумя бэкендами (см. mapLlamaGPUMetricsWithUUIDs).
		base.GPU = mapLlamaGPUMetricsWithUUIDs(llamaMetrics.GPUMetrics, collectGPUUUIDs())
		// R-MultiHost (2026-10-07): cppworker знает только ОБЪЁМ памяти. Всё
		// остальное (загрузка, температура, частоты, питание) в его ответе
		// отсутствует, и mapLlamaGPUMetricsWithUUIDs ставил туда нули — поэтому
		// текстовые бэкенды в WebUI показывали «TEMP 0 °C», «CLOCK —», «GPU 0.0%»,
		// тогда как image-бэкенд на ТОЙ ЖЕ карте показывал реальные 55 °C и
		// частоты (он идёт другим путём — collectGPUMetrics).
		//
		// Дополняем пропуски локальным опросом nvidia-smi/NVML ВНУТРИ контейнера
		// воркера: он видит ту же карту, что и движок.
		base.GPU = mergeGPUMetrics(base.GPU, a.collectGPUMetrics())
	} else {
		// Попытка взять GPU-метрики из nvidia-smi / NVML, безусловно.
		if gpu := a.collectGPUMetrics(); gpu.MemoryTotal > 0 || gpu.UsagePercent > 0 || gpu.Temperature > 0 {
			base.GPU = gpu
		}
	}
	base.System = a.collectSystemMetrics()

	// Конвертируем Llama-модели в RunningModel
	var runningModels []types.RunningModel
	for _, m := range llamaMetrics.Models {
		runningModels = append(runningModels, types.RunningModel{
			Name:   m.Name,
			Size:   uint64(m.SizeBytes),
			Format: "gguf",
		})
	}

	// Заполняем Ollama-метрики из llama.cpp данных
	base.Ollama = types.OllamaMetrics{
		RunningModels:     runningModels,
		RequestsPerSecond: llamaMetrics.RequestsRPS,
	}

	// Устанавливаем BackendType в метрики для корректной фильтрации
	base.BackendType = types.BackendTypeLlamaCpp
	base.Engine = types.EngineLlamaCPP

	// R83 (2026-09-25): причина последнего провала загрузки уезжает в этом же
	// push'е — балансер опубликует уведомление, WebUI покажет его оператору
	// (связка cppworker → agent → webui). Данные уже получены из /api/models,
	// дополнительных запросов нет.
	base.LoadFailure = toBackendLoadFailure(llamaMetrics.LoadFailure)
	// R83 §9.4 шаг 1б (2026-09-26): режим загрузки (cpu_only = деградация).
	// Идёт тем же push'ем и из того же /api/models (поле load_degraded), поэтому
	// дополнительных запросов к cppworker не появляется.
	base.LoadDegraded = toBackendLoadDegraded(llamaMetrics.LoadDegraded)

	return base
}

// toBackendLoadDegraded — конвертация режима загрузки из формата cppworker
// (/api/models → load_degraded) в формат метрик бэкенда.
//
// nil означает «деградации нет» (модель либо не загружена, либо загружена
// нормально): балансер тогда молчит. Пустая запись тоже трактуется как «нет» —
// иначе каждый push порождал бы событие «модель деградировала» без причины.
func toBackendLoadDegraded(ld *llamaLoadDegraded) *types.DegradedLoadInfo {
	if ld == nil || (ld.Model == "" && ld.Stage == "") {
		return nil
	}
	return &types.DegradedLoadInfo{
		Model:       ld.Model,
		Stage:       ld.Stage,
		Reason:      ld.Reason,
		Detail:      ld.Detail,
		At:          ld.At,
		Diagnostics: ld.Diagnostics,
	}
}

// toBackendLoadFailure — конвертация причины провала из формата cppworker
// (/api/models → load_failure) в формат метрик бэкенда.
//
// nil означает «провалов нет»: балансер не публикует ни уведомление, ни
// «восстановление» (восстановлением считается только переход «провал → нет»).
func toBackendLoadFailure(lm *llamaLoadFailure) *types.LoadFailureInfo {
	if lm == nil || (lm.Reason == "" && lm.Model == "" && lm.Error == "") {
		return nil
	}
	return &types.LoadFailureInfo{
		Model:       lm.Model,
		Reason:      lm.Reason,
		Error:       lm.Error,
		At:          lm.At,
		Severity:    lm.Severity,
		Diagnostics: lm.Diagnostics,
	}
}

// mergeGPUMetrics — R-MultiHost (2026-10-07): собрать GPU-метрики из двух
// источников вместо «или-или».
//
// ЗАЧЕМ. У llama.cpp-бэкенда метрики идут из cppworker (`/api/gpu`), который
// отдаёт ТОЛЬКО объём памяти. `mapLlamaGPUMetricsWithUUIDs` честно ставил в
// остальные поля нули («llama.cpp не даёт процент загрузки GPU»), и локальный
// опрос nvidia-smi/NVML не выполнялся вовсе, если cppworker вернул хоть одну
// карту. На живой паре это выглядело так: текстовый бэкенд на RTX 3070 —
// «GPU 0.0%, TEMP 0 °C, CLOCK —», а image-бэкенд НА ТОЙ ЖЕ карте — «55 °C,
// 1755 MHz» (он идёт путём collectGPUMetrics). Оператор справедливо решил, что
// у gguf-бэкендов «нет достоверных метрик».
//
// ПРАВИЛА:
//   - VRAM берём у движка (первичный источник): он знает память с учётом своих
//     резерваций. Если движок памяти не дал — берём локальную.
//   - загрузку, температуру, питание и частоты берём у локального опроса, потому
//     что движок их не отдаёт ВООБЩЕ (а не «отдаёт ноль»): ноль там означал не
//     «холодная карта», а «поля нет».
//   - локальный опрос считается состоявшимся, если он принёс хоть один ненулевой
//     признак (иначе в контейнере нет nvidia-smi/NVML, и подменять нечем).
//   - UUID — из первичных, если они есть, иначе из локальных.
func mergeGPUMetrics(primary, local types.GPUMetrics) types.GPUMetrics {
	merged := primary

	localUsable := local.MemoryTotal > 0 || local.UsagePercent > 0 || local.Temperature > 0 ||
		local.GPUClock > 0 || local.MemClock > 0 || local.PowerUsage > 0

	if merged.MemoryTotal == 0 {
		merged.MemoryTotal = local.MemoryTotal
		merged.MemoryUsed = local.MemoryUsed
		merged.MemoryFree = local.MemoryFree
	}

	if localUsable {
		merged.UsagePercent = local.UsagePercent
		merged.Temperature = local.Temperature
		merged.PowerUsage = local.PowerUsage
		merged.PowerLimit = local.PowerLimit
		merged.GPUClock = local.GPUClock
		merged.MemClock = local.MemClock
	}

	if len(merged.UUIDs) == 0 {
		merged.UUIDs = local.UUIDs
	}

	return merged
}

// mapLlamaGPUMetrics конвертирует Llama LlamaGPUInfo → types.GPUMetrics
func mapLlamaGPUMetrics(gpus []LlamaGPUInfo) types.GPUMetrics {
	return mapLlamaGPUMetricsWithUUIDs(gpus, nil)
}

// mapLlamaGPUMetricsWithUUIDs — то же, но с физическими идентификаторами карт.
//
// ЗАЧЕМ ОТДЕЛЬНАЯ ФУНКЦИЯ (R-Image follow-up, 2026-10-07). У llama.cpp-бэкендов
// GPU-метрики приходят из cppworker (`/api/gpu`), где UUID карт НЕТ — там только
// объём памяти. Без UUID балансер не может отличить «две машины по одной карте»
// от «одна машина, два бэкенда» и складывает память одной карты дважды
// (16 GB на карте 8 GB: cppworker + imageworker на одном хосте).
//
// Поэтому UUID берём из nvidia-smi (единственный источник, который их знает) и
// дописываем к метрикам, полученным от cppworker. this сохраняет приоритет
// данных воркера по памяти и добавляет к ним признак физической карты.
func mapLlamaGPUMetricsWithUUIDs(gpus []LlamaGPUInfo, uuids []string) types.GPUMetrics {
	if len(gpus) == 0 {
		return types.GPUMetrics{}
	}
	var totalVRAM uint64
	for _, g := range gpus {
		totalVRAM += uint64(g.VRAMTotalMB)
	}
	var usedVRAM uint64
	for _, g := range gpus {
		free := g.VRAMFreeMB
		if free > g.VRAMTotalMB {
			free = g.VRAMTotalMB
		}
		usedVRAM += uint64(g.VRAMTotalMB - free)
	}
	return types.GPUMetrics{
		UsagePercent: 0, // llama.cpp не даёт процент загрузки GPU
		MemoryTotal:  totalVRAM,
		MemoryUsed:   usedVRAM,
		MemoryFree:   totalVRAM - usedVRAM,
		Temperature:  0,
		PowerUsage:   0,
		PowerLimit:   0,
		GPUClock:     0,
		MemClock:     0,
		UUIDs:        uuids,
	}
}

package sdbackend

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ============================================================
// Метрики воркера
// ============================================================
//
// Требование плана (§5.6, задача Phase 3): минимум — активные генерации,
// всего сгенерировано, суммарные секунды, состояние процесса, VRAM.
//
// Почему не переиспользуем cppbackend.Metrics: тот завязан на токены/модели
// llama.cpp (n_ctx, token latency, reasoning) — у диффузии этих сущностей нет.
// Набор метрик намеренно маленький и без гистограммных бакетов: латентность
// диффузии измеряется секундами-минутами, а не миллисекундами.

// latencySamples — сколько последних длительностей держим для перцентилей.
const latencySamples = 256

// Metrics — счётчики и состояние воркера (все поля атомарные).
type Metrics struct {
	ActiveGenerations atomic.Int64 // сейчас выполняется
	TotalGenerations  atomic.Int64 // всего успешных генераций (HTTP-запросов)
	TotalImages       atomic.Int64 // всего картинок (учитывает batch/n)
	TotalFailures     atomic.Int64 // всего неуспешных
	TotalSecondsMilli atomic.Int64 // суммарное время генераций (мс)
	QueueRejections   atomic.Int64 // 429 по нашей очереди
	StartupCount      atomic.Int64 // сколько раз поднимали sd-server
	UnloadCount       atomic.Int64 // сколько раз гасили sd-server

	mu         sync.Mutex
	latencies  []int64 // мс, кольцо последних latencySamples
	latencyPos int
	loadedAt   time.Time
	lastUsedAt time.Time
	lastError  string
	startedAt  time.Time
}

// NewMetrics создаёт метрики с отметкой времени старта.
func NewMetrics() *Metrics {
	return &Metrics{
		latencies: make([]int64, 0, latencySamples),
		startedAt: time.Now(),
	}
}

// ObserveGeneration фиксирует завершённую генерацию.
func (m *Metrics) ObserveGeneration(d time.Duration, images int, err error) {
	ms := d.Milliseconds()
	m.mu.Lock()
	if len(m.latencies) < latencySamples {
		m.latencies = append(m.latencies, ms)
	} else {
		m.latencies[m.latencyPos] = ms
		m.latencyPos = (m.latencyPos + 1) % latencySamples
	}
	m.lastUsedAt = time.Now()
	if err != nil {
		m.lastError = err.Error()
		m.TotalFailures.Add(1)
	} else {
		m.lastError = ""
	}
	m.mu.Unlock()

	m.TotalSecondsMilli.Add(ms)
	if err == nil {
		m.TotalGenerations.Add(1)
		m.TotalImages.Add(int64(images))
	}
}

// MarkLoaded / MarkUnloaded — переходы жизненного цикла процесса.
func (m *Metrics) MarkLoaded() {
	m.mu.Lock()
	m.loadedAt = time.Now()
	m.lastUsedAt = time.Now()
	m.lastError = ""
	m.mu.Unlock()
	m.StartupCount.Add(1)
}

// MarkUnloaded сбрасывает отметки загрузки.
func (m *Metrics) MarkUnloaded() {
	m.mu.Lock()
	m.loadedAt = time.Time{}
	m.mu.Unlock()
	m.UnloadCount.Add(1)
}

// MarkError сохраняет последнюю ошибку (для /metrics и /api/image/models).
func (m *Metrics) MarkError(err error) {
	m.mu.Lock()
	if err != nil {
		m.lastError = err.Error()
	}
	m.mu.Unlock()
}

// LastUsed / LoadedAt — времена для idle-unload и диагностики.
func (m *Metrics) LastUsed() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastUsedAt
}

// LoadedAt — когда процесс был поднят (zero, если не загружен).
func (m *Metrics) LoadedAt() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.loadedAt
}

// LastError — текст последней ошибки.
func (m *Metrics) LastError() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastError
}

// Uptime — время работы воркера.
func (m *Metrics) Uptime() time.Duration { return time.Since(m.startedAt) }

// percentileLocked — перцентиль по сохранённым латентностям (0..1).
func (m *Metrics) percentileLocked(p float64) int64 {
	n := len(m.latencies)
	if n == 0 {
		return 0
	}
	cp := make([]int64, n)
	copy(cp, m.latencies)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	idx := int(p * float64(n-1))
	if idx < 0 {
		idx = 0
	}
	if idx >= n {
		idx = n - 1
	}
	return cp[idx]
}

// Snapshot — снимок для JSON/Prometheus.
type MetricsSnapshot struct {
	ActiveGenerations int64 `json:"active_generations"`
	TotalGenerations  int64 `json:"total_generations"`
	TotalImages       int64 `json:"total_images"`
	TotalFailures     int64 `json:"total_failures"`
	TotalSeconds      float64 `json:"total_seconds"`
	QueueRejections   int64 `json:"queue_rejections"`
	StartupCount      int64 `json:"startups_total"`
	UnloadCount       int64 `json:"unloads_total"`
	P50Ms             int64 `json:"p50_ms"`
	P95Ms             int64 `json:"p95_ms"`
	UptimeSeconds     int64 `json:"uptime_seconds"`
	LastError         string `json:"last_error,omitempty"`
	LoadedAt          string `json:"loaded_at,omitempty"`
	LastUsedAt        string `json:"last_used_at,omitempty"`
}

// Snapshot собирает согласованный снимок счётчиков.
func (m *Metrics) Snapshot() MetricsSnapshot {
	m.mu.Lock()
	p50 := m.percentileLocked(0.5)
	p95 := m.percentileLocked(0.95)
	loadedAt := m.loadedAt
	lastUsed := m.lastUsedAt
	lastErr := m.lastError
	m.mu.Unlock()

	s := MetricsSnapshot{
		ActiveGenerations: m.ActiveGenerations.Load(),
		TotalGenerations:  m.TotalGenerations.Load(),
		TotalImages:       m.TotalImages.Load(),
		TotalFailures:     m.TotalFailures.Load(),
		TotalSeconds:      float64(m.TotalSecondsMilli.Load()) / 1000.0,
		QueueRejections:   m.QueueRejections.Load(),
		StartupCount:      m.StartupCount.Load(),
		UnloadCount:       m.UnloadCount.Load(),
		P50Ms:             p50,
		P95Ms:             p95,
		UptimeSeconds:     int64(m.Uptime().Seconds()),
		LastError:         lastErr,
	}
	if !loadedAt.IsZero() {
		s.LoadedAt = loadedAt.UTC().Format(time.RFC3339)
	}
	if !lastUsed.IsZero() {
		s.LastUsedAt = lastUsed.UTC().Format(time.RFC3339)
	}
	return s
}

// PrometheusText — экспозиция в текстовом формате Prometheus 0.0.4.
//
// Имена с префиксом sdworker_ (как cppworker_* у текстового воркера), чтобы
// на одном Grafana-дашборде метрики двух типов воркеров не конфликтовали.
func (m *Metrics) PrometheusText(state string, vram *VRAMInfo) string {
	s := m.Snapshot()
	var b strings.Builder
	type metric struct {
		name, help, kind, value string
	}
	gauges := []metric{
		{"sdworker_active_generations", "Currently running generations", "gauge", strconv.FormatInt(s.ActiveGenerations, 10)},
		{"sdworker_generations_total", "Successfully completed generations", "counter", strconv.FormatInt(s.TotalGenerations, 10)},
		{"sdworker_images_total", "Generated images (accounts for batch/n)", "counter", strconv.FormatInt(s.TotalImages, 10)},
		{"sdworker_failures_total", "Failed generations", "counter", strconv.FormatInt(s.TotalFailures, 10)},
		{"sdworker_generation_seconds_total", "Cumulative generation time in seconds", "counter", strconv.FormatFloat(s.TotalSeconds, 'f', 3, 64)},
		{"sdworker_queue_rejections_total", "Requests rejected with 429 (own queue full)", "counter", strconv.FormatInt(s.QueueRejections, 10)},
		{"sdworker_sd_server_startups_total", "sd-server process spawns", "counter", strconv.FormatInt(s.StartupCount, 10)},
		{"sdworker_sd_server_unloads_total", "sd-server process kills", "counter", strconv.FormatInt(s.UnloadCount, 10)},
		{"sdworker_generation_latency_ms", "Generation latency in ms (p50/p95 over last 256 samples)", "summary", strconv.FormatInt(s.P50Ms, 10)},
		{"sdworker_uptime_seconds", "Worker uptime", "gauge", strconv.FormatInt(s.UptimeSeconds, 10)},
	}
	for _, g := range gauges {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n%s %s\n", g.name, g.help, g.name, g.kind, g.name, g.value)
	}
	// Состояние процесса — метка, а не число: Prometheus-способ отдать
	// «какая модель и в каком состоянии» без парсинга строк.
	fmt.Fprintf(&b, "# HELP sdworker_sd_server_up 1 if sd-server subprocess is running\n")
	fmt.Fprintf(&b, "# TYPE sdworker_sd_server_up gauge\n")
	if state == StateLoaded {
		fmt.Fprintf(&b, "sdworker_sd_server_up{state=%q} 1\n", state)
	} else {
		fmt.Fprintf(&b, "sdworker_sd_server_up{state=%q} 0\n", state)
	}
	if vram != nil && vram.Available {
		fmt.Fprintf(&b, "# HELP sdworker_gpu_memory_used_mb GPU memory used in MB (nvidia-smi)\n")
		fmt.Fprintf(&b, "# TYPE sdworker_gpu_memory_used_mb gauge\n")
		fmt.Fprintf(&b, "sdworker_gpu_memory_used_mb %d\n", vram.UsedMB)
		fmt.Fprintf(&b, "# HELP sdworker_gpu_memory_total_mb GPU memory total in MB (nvidia-smi)\n")
		fmt.Fprintf(&b, "# TYPE sdworker_gpu_memory_total_mb gauge\n")
		fmt.Fprintf(&b, "sdworker_gpu_memory_total_mb %d\n", vram.TotalMB)
	}
	return b.String()
}

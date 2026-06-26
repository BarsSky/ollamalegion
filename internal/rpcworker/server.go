package rpcworker

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"ollama-loadbalancer/pkg/logger"
)

// WorkerServer — HTTP-сервер для RPC worker.
//
// Регистрирует эндпоинты /rpc/{health,load,unload,infer,metrics,kv_sync,kv_fetch}.
// Содержит инстанс ModelManager для управления срезами и счётчики активных запросов.
type WorkerServer struct {
	cfg       WorkerConfig
	manager   *ModelManager
	metrics   *Metrics
	mux       *http.ServeMux
	server    *http.Server

	// Версия (выставляется из main.go).
	version string

	// Канал для graceful shutdown.
	shutdownCh chan struct{}

	// Время старта сервера (используется для uptime в /rpc/health).
	startedAtOnce sync.Once
	startedAtReal time.Time
}

// NewWorkerServer создаёт новый worker server.
func NewWorkerServer(cfg WorkerConfig, manager *ModelManager, version string) *WorkerServer {
	if manager == nil {
		manager = NewModelManager(cfg)
	}
	s := &WorkerServer{
		cfg:        cfg,
		manager:    manager,
		metrics:    NewMetrics(),
		version:    version,
		shutdownCh: make(chan struct{}),
	}
	s.startedAtOnce.Do(func() { s.startedAtReal = time.Now() })
	s.mux = http.NewServeMux()
	s.routes()
	s.server = &http.Server{
		Addr:         net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)),
		Handler:      s.middleware(s.mux),
		ReadTimeout:  cfg.ReadTimeout,
		WriteTimeout: cfg.WriteTimeout,
	}
	return s
}

// routes регистрирует все эндпоинты.
func (s *WorkerServer) routes() {
	s.mux.HandleFunc("/rpc/health", s.handleHealth)
	s.mux.HandleFunc("/rpc/load", s.handleLoad)
	s.mux.HandleFunc("/rpc/unload", s.handleUnload)
	s.mux.HandleFunc("/rpc/infer", s.handleInfer)
	s.mux.HandleFunc("/rpc/metrics", s.handleMetrics)
	s.mux.HandleFunc("/rpc/kv_sync", s.handleKvSync)
	s.mux.HandleFunc("/rpc/kv_fetch", s.handleKvFetch)
}

// middleware — оборачивает хендлеры: recover, auth, logging.
func (s *WorkerServer) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		// Recover.
		defer func() {
			if rec := recover(); rec != nil {
				logger.Get().Errorw("rpcworker panic recovered",
					"path", r.URL.Path,
					"method", r.Method,
					"recover", rec)
				writeJSONError(w, http.StatusInternalServerError, "internal_error", "internal server error")
			}
			logger.Get().Debugw("rpcworker request",
				"method", r.Method,
				"path", r.URL.Path,
				"duration_ms", time.Since(start).Milliseconds())
		}()

		// Auth (если токен задан).
		if s.cfg.AuthToken != "" {
			token := r.Header.Get("Authorization")
			if token == "" {
				token = r.Header.Get("X-API-Token")
			} else if len(token) > 7 && token[:7] == "Bearer " {
				token = token[7:]
			}
			if token != s.cfg.AuthToken {
				writeJSONError(w, http.StatusUnauthorized, "unauthorized", "invalid or missing token")
				return
			}
		}

		next.ServeHTTP(w, r)
	})
}

// ListenAndServe запускает сервер (блокирующий).
func (s *WorkerServer) ListenAndServe() error {
	logger.Get().Infow("rpcworker listening",
		"host", s.cfg.Host,
		"port", s.cfg.Port,
		"worker_id", s.cfg.WorkerID,
		"slice_layers", s.cfg.SliceLayers,
		"models_dir", s.cfg.ModelsDir,
		"stub_mode", s.cfg.StubMode)
	if err := s.server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Shutdown выполняет graceful shutdown.
func (s *WorkerServer) Shutdown(ctx context.Context) error {
	select {
	case <-s.shutdownCh:
		// уже закрыт
	default:
		close(s.shutdownCh)
	}
	for _, sl := range s.manager.ListSlices() {
		_ = s.manager.UnloadSlice(sl.ModelName)
	}
	return s.server.Shutdown(ctx)
}

// Manager возвращает model manager (для тестов).
func (s *WorkerServer) Manager() *ModelManager {
	return s.manager
}

// MiddlewareHandler возвращает HTTP handler с middleware (auth, recover, log).
// Используется в e2e-тестах через httptest.NewServer (без ListenAndServe).
func (s *WorkerServer) MiddlewareHandler() http.Handler {
	return s.middleware(s.mux)
}

// Metrics возвращает metrics (для тестов).
func (s *WorkerServer) Metrics() *Metrics {
	return s.metrics
}

// startedAt возвращает время старта сервера.
func (s *WorkerServer) startedAt() time.Time {
	s.startedAtOnce.Do(func() { s.startedAtReal = time.Now() })
	return s.startedAtReal
}

// Helper для JSON-ответов.
func writeJSON(w http.ResponseWriter, status int, payload interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if payload != nil {
		_ = json.NewEncoder(w).Encode(payload)
	}
}

func writeJSONError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]interface{}{
		"error":   code,
		"message": message,
		"status":  status,
	})
}

// readJSON — читает и парсит JSON body.
//
// Игнорирует неизвестные поля (для совместимости между разными версиями
// SliceInferRequest в WorkerClient и WorkerServer).
func readJSON(r *http.Request, dst interface{}) error {
	if r.Body == nil {
		return errors.New("empty body")
	}
	dec := json.NewDecoder(r.Body)
	// DisallowUnknownFields() отключён — WorkerClient шлёт JSON со всеми
	// полями rpccoordinator.SliceInferRequest (start_layer, end_layer, input),
	// а rpcworker читает только свои (model_name, prompt, slice_id).
	return dec.Decode(dst)
}

// CounterInt64 — обёртка atomic для удобства тестов.
type CounterInt64 struct {
	v atomic.Int64
}

func (c *CounterInt64) Inc() int64 {
	return c.v.Add(1)
}

func (c *CounterInt64) Dec() int64 {
	return c.v.Add(-1)
}

func (c *CounterInt64) Value() int64 {
	return c.v.Load()
}

func (c *CounterInt64) Add(n int64) int64 {
	return c.v.Add(n)
}

// CounterMap — map с atomic-counter'ами (для per-model метрик).
type CounterMap struct {
	mu sync.Mutex
	m  map[string]*CounterInt64
}

func NewCounterMap() *CounterMap {
	return &CounterMap{m: make(map[string]*CounterInt64)}
}

func (c *CounterMap) Inc(key string) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	cnt, ok := c.m[key]
	if !ok {
		cnt = &CounterInt64{}
		c.m[key] = cnt
	}
	return cnt.Inc()
}

func (c *CounterMap) Get(key string) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	cnt, ok := c.m[key]
	if !ok {
		return 0
	}
	return cnt.Value()
}

func (c *CounterMap) Snapshot() map[string]int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]int64, len(c.m))
	for k, v := range c.m {
		out[k] = v.Value()
	}
	return out
}
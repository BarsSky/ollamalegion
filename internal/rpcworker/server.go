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

	"ollama-loadbalancer/c/bridge"
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
	kvStore   *KVStore // B4: in-memory KV-cache
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
		kvStore:    NewKVStore(cfg.WorkerID, DefaultKVStoreConfig()),
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

// KVStore возвращает KV-store (для тестов).
func (s *WorkerServer) KVStore() *KVStore {
	return s.kvStore
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

// =====================================================================
// B5: helper-методы для net/rpc протокола (pkg/protocol.WorkerRPCService).
// =====================================================================
//
// WorkerRPCService делегирует вызовы к этим методам, не повторяя логику HTTP-хендлеров.
// Все методы — synchronous, thread-safe (используют уже thread-safe Manager/Metrics/KVStore).

// WorkerID возвращает идентификатор worker'а (cfg.WorkerID).
func (s *WorkerServer) WorkerID() string {
	return s.cfg.WorkerID
}

// Version возвращает версию бинарника, переданную в NewWorkerServer.
func (s *WorkerServer) Version() string {
	return s.version
}

// HandleInferRPC — RPC-обёртка над handleInfer (sync-режим, без SSE).
//
// Делает то же, что HTTP /rpc/infer без ?stream=true, но возвращает
// структуру напрямую, без сериализации в JSON.
//
// Возвращает *SliceInferResponse и error. nil slice = модель не загружена.
func (s *WorkerServer) HandleInferRPC(modelName, prompt string, tokens int, temperature float32) (*SliceInferResponse, error) {
	slice := s.manager.GetSlice(modelName)
	if slice == nil {
		return nil, errModelNotLoaded
	}
	if slice.Handle == nil {
		return nil, errSliceNoHandle
	}

	params := bridge.DefaultGenerationParams()
	if tokens > 0 {
		params.NPredict = tokens
	}
	if temperature > 0 {
		params.Temperature = temperature
	}

	sliceID := slice.Layers

	end := s.metrics.OnInferStart(modelName)
	start := time.Now()

	var (
		output     string
		tokensUsed int
		err        error
	)
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				err = errPanic
			}
		}()
		result, ierr := slice.Handle.Infer(prompt, params)
		if ierr != nil {
			err = ierr
			return
		}
		if result == nil {
			err = errNilResult
			return
		}
		if result.Status != 0 {
			err = errBridge(result.ErrorMsg)
			return
		}
		output = result.Output
		tokensUsed = slice.Handle.CountTokens(output)
	}()
	elapsed := time.Since(start).Milliseconds()
	end(tokensUsed, elapsed, err)

	if err != nil {
		return nil, err
	}
	return &SliceInferResponse{
		WorkerID:   s.cfg.WorkerID,
		SliceID:    sliceID,
		Output:     output,
		TokensUsed: tokensUsed,
		LatencyMs:  elapsed,
	}, nil
}

// MetricsSnapshot — сериализуемая версия metrics для net/rpc (gob).
//
// Gob не умеет кодировать map[string]interface{} (даже с пустым интерфейсом),
// поэтому используется фиксированная структура с известными типами.
// HTTP-обработчик /rpc/metrics продолжает возвращать map (более богатая
// структура), RPC — конкретный тип (gob-совместимый).
type MetricsSnapshot struct {
	ActiveRequests int64                  `json:"active_requests"`
	LoadedSlices   int64                  `json:"loaded_slices"`
	TotalInfer     int64                  `json:"total_infer"`
	TotalErrors    int64                  `json:"total_errors"`
	TotalLoadOK    int64                  `json:"total_load_ok"`
	TotalLoadErr   int64                  `json:"total_load_err"`
	AvgLatencyMs   int64                  `json:"avg_latency_ms"`
	UptimeS        int64                  `json:"uptime_s"`
	PerModelInfer  map[string]int64       `json:"per_model_infer,omitempty"`
	PerModelErrors map[string]int64       `json:"per_model_errors,omitempty"`
}

// MetricsRPC возвращает snapshot метрик в виде конкретной структуры
// (gob-сериализуемой). HTTP /rpc/metrics продолжает использовать map.
func (s *WorkerServer) MetricsRPC() MetricsSnapshot {
	if s.metrics == nil {
		return MetricsSnapshot{}
	}
	snap := s.metrics.Snapshot()
	out := MetricsSnapshot{
		LoadedSlices: toInt64(snap["loaded_slices"]),
		TotalInfer:   toInt64(snap["total_infer"]),
		TotalErrors:  toInt64(snap["total_errors"]),
		TotalLoadOK:  toInt64(snap["total_load_ok"]),
		TotalLoadErr: toInt64(snap["total_load_err"]),
		AvgLatencyMs: toInt64(snap["avg_latency_ms"]),
		UptimeS:      toInt64(snap["uptime_s"]),
	}
	// active_requests в текущей реализации выставляется отдельно.
	if ar, ok := snap["active_requests"]; ok {
		out.ActiveRequests = toInt64(ar)
	}
	if pmi, ok := snap["per_model_infer"].(map[string]int64); ok {
		out.PerModelInfer = pmi
	}
	if pme, ok := snap["per_model_errors"].(map[string]int64); ok {
		out.PerModelErrors = pme
	}
	return out
}

// toInt64 — best-effort конверсия произвольного типа в int64.
func toInt64(v interface{}) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case int:
		return int64(x)
	case int32:
		return int64(x)
	case float64:
		return int64(x)
	default:
		return 0
	}
}

// KvStoreSaveRPC — RPC-обёртка над /rpc/kv_sync.
//
// encoded — optional base64(key):base64(value) от клиента;
// если непустой, декодируется в KeyTensor/ValueTensor.
// Возвращает ошибку при превышении MaxEntries или пустом sessionID.
func (s *WorkerServer) KvStoreSaveRPC(sessionID string, seqLen int, keyTensor, valueTensor []byte, layers []int, encoded string) error {
	if sessionID == "" {
		return errors.New("session_id is required")
	}
	shard := &KVShard{
		SessionID:   sessionID,
		WorkerID:    s.cfg.WorkerID,
		SeqLen:      seqLen,
		KeyTensor:   keyTensor,
		ValueTensor: valueTensor,
		Layers:      layers,
	}
	if encoded != "" {
		decoded := DecodeShard(sessionID, s.cfg.WorkerID, seqLen, encoded)
		shard.KeyTensor = decoded.KeyTensor
		shard.ValueTensor = decoded.ValueTensor
	}
	return s.kvStore.Save(shard)
}

// KvStoreLoadRPC — RPC-обёртка над /rpc/kv_fetch.
//
// Возвращает (shard, nil) если найдено, (nil, errNotFound) если нет.
func (s *WorkerServer) KvStoreLoadRPC(sessionID string) (*KVShard, error) {
	if sessionID == "" {
		return nil, errors.New("session_id is required")
	}
	shard := s.kvStore.Load(sessionID)
	if shard == nil {
		return nil, errKVShardNotFound
	}
	return shard, nil
}

// errModelNotLoaded — модель не загружена на этом worker'е.
var errModelNotLoaded = stringError("model not loaded on this worker; call /rpc/load first")

// errSliceNoHandle — у загруженного среза нет bridge handle (не инициализирован).
var errSliceNoHandle = stringError("loaded slice has no bridge handle")

// errKVShardNotFound — KV-shard для session_id не найден.
var errKVShardNotFound = stringError("no KV shard for session")

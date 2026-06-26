// Package protocol — B5: gRPC-style бинарный протокол через net/rpc.
//
// Worker'ы могут слушать либо HTTP (legacy, B1-B4), либо net/rpc (gob).
// Оба протокола используют один и тот же ModelManager и KVStore под капотом.
//
// net/rpc выбран как B5-stub:
//   - Не требует protoc-gen-go / google.golang.org/grpc.
//   - Даёт binary wire protocol через encoding/gob.
//   - Поддерживает streaming через net/rpc.Stream.
//   - В production можно мигрировать на grpc-go позже (Phase B5.real).
package protocol

import (
	"errors"
	"net"
	"net/rpc"
	"sync"
	"time"

	"ollama-loadbalancer/internal/rpcworker"
)

// ProtocolVersion — текущая версия B5 stub.
const ProtocolVersion = "rpc-v0.1.0-stub"

// =====================================================================
// RPC args/reply types — net/rpc требует ровно 2 args: args, reply (оба указатели).
// =====================================================================

// HealthCheckArgs — пустой.
type HealthCheckArgs struct{}

// HealthCheckReply — ответ HealthCheck RPC.
type HealthCheckReply struct {
	Status         string  `json:"status"`
	WorkerID       string  `json:"worker_id"`
	Version        string  `json:"version"`
	Protocol       string  `json:"protocol"`
	LoadedSlices   int     `json:"loaded_slices"`
	UptimeSeconds  float64 `json:"uptime_seconds"`
}

// LoadSliceArgs — запрос на загрузку среза.
type LoadSliceArgs struct {
	ModelName string `json:"model_name"`
	Layers    string `json:"layers,omitempty"`
}

// LoadSliceReply — ответ на LoadSlice.
type LoadSliceReply struct {
	OK      bool   `json:"ok"`
	Model   string `json:"model"`
	Layers  string `json:"layers"`
	LoadMs  int64  `json:"load_ms"`
	Message string `json:"message,omitempty"`
}

// UnloadSliceArgs — выгрузка.
type UnloadSliceArgs struct {
	ModelName string `json:"model_name"`
}

// UnloadSliceReply — ответ Unload.
type UnloadSliceReply struct {
	OK bool `json:"ok"`
}

// InferArgs — запрос inference (B5 stub — синхронный; B5.real — streaming).
type InferArgs struct {
	ModelName   string  `json:"model_name"`
	Prompt      string  `json:"prompt"`
	SliceID     string  `json:"slice_id,omitempty"`
	Tokens      int     `json:"tokens,omitempty"`
	Temperature float32 `json:"temperature,omitempty"`
}

// InferReply — ответ inference.
type InferReply struct {
	OK         bool   `json:"ok"`
	WorkerID   string `json:"worker_id,omitempty"`
	SliceID    string `json:"slice_id,omitempty"`
	Output     string `json:"output,omitempty"`
	TokensUsed int    `json:"tokens_used,omitempty"`
	LatencyMs   int64  `json:"latency_ms,omitempty"`
	Error      string `json:"error,omitempty"`
}

// MetricsArgs — пустой.
type MetricsArgs struct{}

// MetricsReply — снимок метрик.
//
// Используется конкретный тип rpcworker.MetricsSnapshot (gob-сериализуемый),
// а не map[string]interface{} — gob не умеет кодировать interface{}.
type MetricsReply struct {
	Stats rpcworker.MetricsSnapshot `json:"stats"`
}

// KvSyncArgs — сохранение shard.
type KvSyncArgs struct {
	SessionID   string `json:"session_id"`
	SeqLen      int    `json:"seq_len"`
	KeyTensor   []byte `json:"key_tensor,omitempty"`
	ValueTensor []byte `json:"value_tensor,omitempty"`
	Layers      []int  `json:"layers,omitempty"`
	Encoded     string `json:"encoded,omitempty"`
}

// KvSyncReply — ответ KvSync.
type KvSyncReply struct {
	OK      bool   `json:"ok"`
	Message string `json:"message,omitempty"`
}

// KvFetchArgs — получение shard.
type KvFetchArgs struct {
	SessionID string `json:"session_id"`
}

// KvFetchReply — shard или ошибка.
type KvFetchReply struct {
	OK         bool   `json:"ok"`
	Shard      []byte `json:"shard,omitempty"`
	SessionID  string `json:"session_id,omitempty"`
	SeqLen     int    `json:"seq_len,omitempty"`
	Message    string `json:"message,omitempty"`
}

// =====================================================================
// WorkerRPCService — реализация RPC-методов поверх rpcworker.WorkerServer.
// =====================================================================

// WorkerRPCService — обёртка для net/rpc.
//
// Имена методов (HealthCheck, LoadSlice и т.д.) становятся доступны через
// `client.Call("WorkerRPCService.HealthCheck", ...)`.
type WorkerRPCService struct {
	mu      sync.RWMutex
	Server  *rpcworker.WorkerServer
	started time.Time
}

// NewWorkerRPCService создаёт новый сервис.
func NewWorkerRPCService(server *rpcworker.WorkerServer) *WorkerRPCService {
	return &WorkerRPCService{
		Server:  server,
		started: time.Now(),
	}
}

// HealthCheck — RPC handler для /rpc/health.
func (s *WorkerRPCService) HealthCheck(args *HealthCheckArgs, reply *HealthCheckReply) error {
	if s.Server == nil {
		return errors.New("server not initialized")
	}
	reply.Status = "ok"
	reply.WorkerID = s.Server.WorkerID()
	reply.Version = s.Server.Version()
	reply.Protocol = ProtocolVersion
	reply.LoadedSlices = len(s.Server.Manager().ListSlices())
	reply.UptimeSeconds = time.Since(s.started).Seconds()
	return nil
}

// LoadSlice — RPC handler для /rpc/load.
func (s *WorkerRPCService) LoadSlice(args *LoadSliceArgs, reply *LoadSliceReply) error {
	if args.ModelName == "" {
		reply.OK = false
		reply.Message = "model_name is required"
		return nil
	}
	loaded, err := s.Server.Manager().LoadSlice(args.ModelName, args.Layers)
	if err != nil {
		// B5: инкрементируем счётчик ошибок загрузки (для совместимости с HTTP-путём).
		s.Server.Metrics().OnLoadErr()
		reply.OK = false
		reply.Message = err.Error()
		return nil
	}
	// B5: инкрементируем счётчик успешных загрузок (для совместимости с HTTP-путём).
	s.Server.Metrics().OnLoadOk()
	reply.OK = true
	reply.Model = loaded.ModelName
	reply.Layers = loaded.Layers
	reply.LoadMs = loaded.LoadMs
	return nil
}

// UnloadSlice — RPC handler для /rpc/unload.
func (s *WorkerRPCService) UnloadSlice(args *UnloadSliceArgs, reply *UnloadSliceReply) error {
	if args.ModelName == "" {
		reply.OK = false
		return nil
	}
	// B5: инкрементируем счётчик выгрузок только если модель была загружена.
	if s.Server.Manager().GetSlice(args.ModelName) != nil {
		s.Server.Metrics().OnUnload()
	}
	_ = s.Server.Manager().UnloadSlice(args.ModelName)
	reply.OK = true
	return nil
}

// Infer — RPC handler для /rpc/infer (синхронный stub).
//
// B5: streaming через net/rpc.Stream (BidirectionalStreaming)
// ещё не реализован — это часть B5.real. Здесь синхронный вариант.
func (s *WorkerRPCService) Infer(args *InferArgs, reply *InferReply) error {
	if args.ModelName == "" {
		reply.OK = false
		reply.Error = "model_name is required"
		return nil
	}
	// Делегируем к существующему worker handleInfer.
	result, err := s.Server.HandleInferRPC(args.ModelName, args.Prompt, args.Tokens, args.Temperature)
	if err != nil {
		reply.OK = false
		reply.Error = err.Error()
		return nil
	}
	reply.OK = true
	reply.WorkerID = s.Server.WorkerID()
	reply.SliceID = result.SliceID
	reply.Output = result.Output
	reply.TokensUsed = result.TokensUsed
	reply.LatencyMs = result.LatencyMs
	return nil
}

// Metrics — RPC handler для /rpc/metrics.
func (s *WorkerRPCService) Metrics(args *MetricsArgs, reply *MetricsReply) error {
	// Возвращаем snapshot из WorkerServer.Metrics().
	reply.Stats = s.Server.MetricsRPC()
	return nil
}

// KvSync — RPC handler для /rpc/kv_sync.
func (s *WorkerRPCService) KvSync(args *KvSyncArgs, reply *KvSyncReply) error {
	if args.SessionID == "" {
		reply.OK = false
		reply.Message = "session_id is required"
		return nil
	}
	if err := s.Server.KvStoreSaveRPC(args.SessionID, args.SeqLen, args.KeyTensor, args.ValueTensor, args.Layers, args.Encoded); err != nil {
		reply.OK = false
		reply.Message = err.Error()
		return nil
	}
	reply.OK = true
	return nil
}

// KvFetch — RPC handler для /rpc/kv_fetch.
func (s *WorkerRPCService) KvFetch(args *KvFetchArgs, reply *KvFetchReply) error {
	shard, err := s.Server.KvStoreLoadRPC(args.SessionID)
	if err != nil {
		reply.OK = false
		reply.Message = err.Error()
		return nil
	}
	reply.OK = true
	reply.SessionID = shard.SessionID
	reply.SeqLen = shard.SeqLen
	reply.Shard = []byte(rpcworker.EncodeShard(shard))
	return nil
}

// =====================================================================
// RPC Server lifecycle — нативный net/rpc.Server.
// =====================================================================

// StartRPCServer запускает net/rpc server на TCP-порту и регистрирует WorkerRPCService.
//
// Возвращает listener и *rpc.Server. Listener нужно закрыть при shutdown.
func StartRPCServer(host string, port int, service *WorkerRPCService) (net.Listener, *rpc.Server, error) {
	addr := net.JoinHostPort(host, fmtInt(port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, nil, err
	}
	server := rpc.NewServer()
	if err := server.RegisterName("WorkerRPCService", service); err != nil {
		ln.Close()
		return nil, nil, err
	}
	go server.Accept(ln)
	return ln, server, nil
}

// fmtInt — helper без strconv (избегаем лишний import).
func fmtInt(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
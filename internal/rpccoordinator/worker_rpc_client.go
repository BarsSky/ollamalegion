package rpccoordinator

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/rpc"
	"sync"
	"time"

	"ollama-loadbalancer/pkg/protocol"
)

// WorkerRPCClient — net/rpc клиент для WorkerRPCService (B5 stub).
//
// Хранит постоянное соединение с worker'ом и реализует те же методы, что
// HTTP WorkerClient, но через gob-сериализацию. Это даёт нам B5-binary
// протокол с проверкой типов на этапе компиляции.
//
// Соединение lazy: устанавливается при первом вызове. На любую ошибку
// соединение сбрасывается (next call переоткроет). Для HTTP/fallback пути
// используйте обычный WorkerClient.
//
// Используется когда WorkerClient.Protocol == "rpc" или "grpc".
type WorkerRPCClient struct {
	WorkerID string
	Host     string
	Port     int

	mu        sync.Mutex
	conn      net.Conn
	client    *rpc.Client
	lastError time.Time

	// connectionTimeout — лимит на dial/handshake.
	connectionTimeout time.Duration
}

// NewWorkerRPCClient создаёт новый RPC клиент.
func NewWorkerRPCClient(workerID, host string, port int) *WorkerRPCClient {
	return &WorkerRPCClient{
		WorkerID:          workerID,
		Host:              host,
		Port:              port,
		connectionTimeout: 5 * time.Second,
	}
}

// dial открывает net/rpc соединение (lazy + reconnect).
//
// Использует gob через net/rpc. Возвращает ошибку при недоступности worker'а.
func (c *WorkerRPCClient) dial() (*rpc.Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.client != nil {
		return c.client, nil
	}

	addr := net.JoinHostPort(c.Host, fmtInt(c.Port))
	conn, err := net.DialTimeout("tcp", addr, c.connectionTimeout)
	if err != nil {
		c.lastError = time.Now()
		return nil, fmt.Errorf("rpc dial %s: %w", addr, err)
	}
	// rpc.NewClientWithCodec-style: используем стандартный NewClient (gob внутри).
	client := rpc.NewClient(conn)
	c.conn = conn
	c.client = client
	return client, nil
}

// close закрывает текущее соединение (если открыто).
func (c *WorkerRPCClient) close() {
	if c.client != nil {
		_ = c.client.Close()
		c.client = nil
	}
	if c.conn != nil {
		_ = c.conn.Close()
		c.conn = nil
	}
}

// reconnect закрывает и сразу открывает заново.
func (c *WorkerRPCClient) reconnect() error {
	c.close()
	_, err := c.dial()
	return err
}

// callWithReconnect — обёртка над client.Call с reconnect на ошибке.
func (c *WorkerRPCClient) callWithReconnect(serviceMethod string, args, reply interface{}) error {
	client, err := c.dial()
	if err != nil {
		return err
	}
	if err := client.Call(serviceMethod, args, reply); err != nil {
		// На любую RPC-ошибку закрываем соединение — следующий вызов откроет заново.
		c.close()
		return err
	}
	return nil
}

// Close закрывает соединение. WorkerClient может переиспользовать клиент.
func (c *WorkerRPCClient) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.close()
	return nil
}

// =====================================================================
// WorkerRPCService methods.
// =====================================================================

// HealthCheck вызывает WorkerRPCService.HealthCheck.
func (c *WorkerRPCClient) HealthCheck() (*protocol.HealthCheckReply, error) {
	reply := &protocol.HealthCheckReply{}
	if err := c.callWithReconnect("WorkerRPCService.HealthCheck", &protocol.HealthCheckArgs{}, reply); err != nil {
		return nil, err
	}
	return reply, nil
}

// LoadSlice вызывает WorkerRPCService.LoadSlice.
func (c *WorkerRPCClient) LoadSlice(modelName, layers string) (*protocol.LoadSliceReply, error) {
	reply := &protocol.LoadSliceReply{}
	args := &protocol.LoadSliceArgs{ModelName: modelName, Layers: layers}
	if err := c.callWithReconnect("WorkerRPCService.LoadSlice", args, reply); err != nil {
		return nil, err
	}
	if !reply.OK {
		return reply, errors.New(reply.Message)
	}
	return reply, nil
}

// UnloadSlice вызывает WorkerRPCService.UnloadSlice.
func (c *WorkerRPCClient) UnloadSlice(modelName string) (*protocol.UnloadSliceReply, error) {
	reply := &protocol.UnloadSliceReply{}
	args := &protocol.UnloadSliceArgs{ModelName: modelName}
	if err := c.callWithReconnect("WorkerRPCService.UnloadSlice", args, reply); err != nil {
		return nil, err
	}
	if !reply.OK {
		return reply, errors.New("unload failed")
	}
	return reply, nil
}

// Infer вызывает WorkerRPCService.Infer (синхронный, без streaming).
func (c *WorkerRPCClient) Infer(args *protocol.InferArgs) (*protocol.InferReply, error) {
	reply := &protocol.InferReply{}
	if err := c.callWithReconnect("WorkerRPCService.Infer", args, reply); err != nil {
		return nil, err
	}
	if !reply.OK {
		return reply, errors.New(reply.Error)
	}
	return reply, nil
}

// GetMetrics вызывает WorkerRPCService.Metrics.
func (c *WorkerRPCClient) GetMetrics() (*protocol.MetricsReply, error) {
	reply := &protocol.MetricsReply{}
	if err := c.callWithReconnect("WorkerRPCService.Metrics", &protocol.MetricsArgs{}, reply); err != nil {
		return nil, err
	}
	return reply, nil
}

// KvSync вызывает WorkerRPCService.KvSync.
func (c *WorkerRPCClient) KvSync(args *protocol.KvSyncArgs) (*protocol.KvSyncReply, error) {
	reply := &protocol.KvSyncReply{}
	if err := c.callWithReconnect("WorkerRPCService.KvSync", args, reply); err != nil {
		return nil, err
	}
	if !reply.OK {
		return reply, errors.New(reply.Message)
	}
	return reply, nil
}

// KvFetch вызывает WorkerRPCService.KvFetch.
func (c *WorkerRPCClient) KvFetch(sessionID string) (*protocol.KvFetchReply, error) {
	reply := &protocol.KvFetchReply{}
	args := &protocol.KvFetchArgs{SessionID: sessionID}
	if err := c.callWithReconnect("WorkerRPCService.KvFetch", args, reply); err != nil {
		return nil, err
	}
	if !reply.OK {
		return reply, errors.New(reply.Message)
	}
	return reply, nil
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

// compile-time проверка, что ctx приходит через вызов (не используется).
var _ = context.Background
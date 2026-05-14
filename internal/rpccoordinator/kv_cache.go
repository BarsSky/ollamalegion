// Package rpccoordinator — Distributed KV Cache Manager.
// Управляет распределённым KV cache между worker'ами для multi-turn диалогов.
package rpccoordinator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"ollama-loadbalancer/pkg/logger"
)

// DistributedKVCache управляет распределённым KV cache.
type DistributedKVCache struct {
	mu         sync.RWMutex
	shards     map[string]*KVCacheShard // sessionID → shard
	workers    map[string]*WorkerClient // workerID → client
	httpClient *http.Client
	ttl        time.Duration
	maxEntries int
}

// KVCacheShard — шард KV cache для конкретной сессии.
type KVCacheShard struct {
	SessionID   string
	WorkerID    string
	Layers      []int    // Какие слои кеширует
	KeyTensor   []byte   // Сериализованный key tensor
	ValueTensor []byte   // Сериализованный value tensor
	SeqLen      int      // Текущая длина последовательности
	LastAccess  time.Time
	mu          sync.RWMutex
}

// KVCacheConfig — конфигурация KV cache.
type KVCacheConfig struct {
	TTL           time.Duration // Время жизни кеша
	MaxEntries    int           // Максимальное количество сессий
	SyncInterval  time.Duration // Интервал синхронизации
	Compression   bool          // Сжимать тензоры
}

// NewDistributedKVCache создаёт новый распределённый KV cache.
func NewDistributedKVCache(cfg KVCacheConfig, workers map[string]*WorkerClient) *DistributedKVCache {
	if cfg.TTL <= 0 {
		cfg.TTL = 30 * time.Minute
	}
	if cfg.MaxEntries <= 0 {
		cfg.MaxEntries = 1000
	}

	return &DistributedKVCache{
		shards:     make(map[string]*KVCacheShard),
		workers:    workers,
		httpClient: &http.Client{Timeout: 10 * time.Second},
		ttl:        cfg.TTL,
		maxEntries: cfg.MaxEntries,
	}
}

// RegisterSession регистрирует новую сессию для KV cache.
func (kvc *DistributedKVCache) RegisterSession(sessionID string, workerID string, layers []int) error {
	if sessionID == "" {
		return fmt.Errorf("sessionID is required")
	}

	kvc.mu.Lock()
	defer kvc.mu.Unlock()

	// Cleanup старых записей при превышении лимита
	me := kvc.maxEntries
	if me <= 0 {
		me = 1000
	}
	if len(kvc.shards) >= me {
		kvc.evictLRU()
	}

	kvc.shards[sessionID] = &KVCacheShard{
		SessionID:  sessionID,
		WorkerID:   workerID,
		Layers:     layers,
		SeqLen:     0,
		LastAccess: time.Now(),
	}

	logger.Get().Infow("kv cache session registered",
		"session", sessionID, "worker", workerID, "layers", layers)
	return nil
}

// GetShard возвращает шард для сессии.
func (kvc *DistributedKVCache) GetShard(sessionID string) *KVCacheShard {
	kvc.mu.RLock()
	defer kvc.mu.RUnlock()
	shard := kvc.shards[sessionID]
	if shard != nil {
		shard.mu.Lock()
		shard.LastAccess = time.Now()
		shard.mu.Unlock()
	}
	return shard
}

// UpdateShard обновляет шард KV cache после inference.
func (kvc *DistributedKVCache) UpdateShard(sessionID string, seqLen int, keyTensor, valueTensor []byte) error {
	shard := kvc.GetShard(sessionID)
	if shard == nil {
		return fmt.Errorf("shard not found for session %s", sessionID)
	}

	shard.mu.Lock()
	defer shard.mu.Unlock()
	shard.SeqLen = seqLen
	if keyTensor != nil {
		shard.KeyTensor = keyTensor
	}
	if valueTensor != nil {
		shard.ValueTensor = valueTensor
	}
	shard.LastAccess = time.Now()
	return nil
}

// SyncToWorker синхронизирует KV cache на worker'а перед inference.
// Вызывается перед началом inference для сессии с историей.
func (kvc *DistributedKVCache) SyncToWorker(ctx context.Context, sessionID string) error {
	shard := kvc.GetShard(sessionID)
	if shard == nil {
		// Новая сессия — нечего синхронизировать
		return nil
	}

	shard.mu.RLock()
	workerID := shard.WorkerID
	seqLen := shard.SeqLen
	keyTensor := make([]byte, len(shard.KeyTensor))
	copy(keyTensor, shard.KeyTensor)
	valueTensor := make([]byte, len(shard.ValueTensor))
	copy(valueTensor, shard.ValueTensor)
	shard.mu.RUnlock()

	// Отправляем KV cache на worker
	worker, ok := kvc.workers[workerID]
	if !ok {
		return fmt.Errorf("worker %s not found", workerID)
	}

	syncReq := map[string]interface{}{
		"session_id":   sessionID,
		"seq_len":      seqLen,
		"key_tensor":   keyTensor,
		"value_tensor": valueTensor,
	}

	body, _ := json.Marshal(syncReq)
	url := fmt.Sprintf("%s/rpc/kv_sync", worker.BaseURL())
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := kvc.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("kv sync failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("kv sync returned %d", resp.StatusCode)
	}

	logger.Get().Debugw("kv cache synced to worker",
		"session", sessionID, "worker", workerID, "seqLen", seqLen)
	return nil
}

// FetchFromWorker забирает обновлённый KV cache с worker'а после inference.
func (kvc *DistributedKVCache) FetchFromWorker(ctx context.Context, sessionID string) error {
	shard := kvc.GetShard(sessionID)
	if shard == nil {
		return nil
	}

	worker, ok := kvc.workers[shard.WorkerID]
	if !ok {
		return fmt.Errorf("worker %s not found", shard.WorkerID)
	}

	url := fmt.Sprintf("%s/rpc/kv_fetch?session_id=%s", worker.BaseURL(), sessionID)
	resp, err := kvc.httpClient.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("kv fetch returned %d", resp.StatusCode)
	}

	var result struct {
		SeqLen      int    `json:"seq_len"`
		KeyTensor   []byte `json:"key_tensor"`
		ValueTensor []byte `json:"value_tensor"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return err
	}

	return kvc.UpdateShard(sessionID, result.SeqLen, result.KeyTensor, result.ValueTensor)
}

// DeleteSession удаляет сессию из KV cache.
func (kvc *DistributedKVCache) DeleteSession(sessionID string) {
	kvc.mu.Lock()
	defer kvc.mu.Unlock()
	delete(kvc.shards, sessionID)
	logger.Get().Infow("kv cache session deleted", "session", sessionID)
}

// CleanupExpired удаляет просроченные записи.
func (kvc *DistributedKVCache) CleanupExpired() {
	kvc.mu.Lock()
	defer kvc.mu.Unlock()

	now := time.Now()
	expired := 0
	for id, shard := range kvc.shards {
		shard.mu.RLock()
		lastAccess := shard.LastAccess
		shard.mu.RUnlock()

		if now.Sub(lastAccess) > kvc.ttl {
			delete(kvc.shards, id)
			expired++
		}
	}

	if expired > 0 {
		logger.Get().Infow("kv cache cleanup", "expired", expired, "remaining", len(kvc.shards))
	}
}

// evictLRU удаляет наименее используемую запись.
func (kvc *DistributedKVCache) evictLRU() {
	var oldestID string
	var oldestTime time.Time

	for id, shard := range kvc.shards {
		shard.mu.RLock()
		access := shard.LastAccess
		shard.mu.RUnlock()

		if oldestID == "" || access.Before(oldestTime) {
			oldestID = id
			oldestTime = access
		}
	}

	if oldestID != "" {
		delete(kvc.shards, oldestID)
		logger.Get().Infow("kv cache LRU eviction", "session", oldestID)
	}
}

// GetStats возвращает статистику KV cache.
func (kvc *DistributedKVCache) GetStats() map[string]interface{} {
	kvc.mu.RLock()
	defer kvc.mu.RUnlock()

	totalSize := 0
	for _, shard := range kvc.shards {
		shard.mu.RLock()
		totalSize += len(shard.KeyTensor) + len(shard.ValueTensor)
		shard.mu.RUnlock()
	}

	return map[string]interface{}{
		"sessions":    len(kvc.shards),
		"totalSize":   totalSize,
		"ttl":         kvc.ttl.String(),
		"maxEntries":  kvc.MaxEntries(),
	}
}

// MaxEntries возвращает максимальное количество записей.
func (kvc *DistributedKVCache) MaxEntries() int {
	if kvc.maxEntries > 0 {
		return kvc.maxEntries
	}
	return 1000 // default
}

// AddWorker добавляет worker в KV cache manager.
func (kvc *DistributedKVCache) AddWorker(workerID string, client *WorkerClient) {
	kvc.mu.Lock()
	defer kvc.mu.Unlock()
	if kvc.workers == nil {
		kvc.workers = make(map[string]*WorkerClient)
	}
	kvc.workers[workerID] = client
}

// RemoveWorker удаляет worker.
func (kvc *DistributedKVCache) RemoveWorker(workerID string) {
	kvc.mu.Lock()
	defer kvc.mu.Unlock()
	delete(kvc.workers, workerID)
}
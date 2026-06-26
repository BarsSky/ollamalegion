package rpcworker

import (
	"encoding/base64"
	"sync"
	"time"
)

// =====================================================================
// KV Store — in-memory хранилище KV-shard'ов для multi-turn сессий.
// =====================================================================
//
// B4: реализация /rpc/kv_sync (POST) и /rpc/kv_fetch (GET).
//
// Worker хранит per-session KV-кеш в памяти. После inference для сессии
// координатор может:
//   - POST /rpc/kv_sync {session_id, shard} — сохранить shard.
//   - GET  /rpc/kv_fetch?session_id=X — получить shard.
//
// На B4 (stub-режим без реального llama.cpp) shard — opaque []byte,
// сериализованный через base64 в JSON. В production это будет
// сериализованный llama.cpp cache file (bincode или safetensors).
//
// TTL: shards автоматически удаляются через TTL (default 30 минут).
// Реализация — bounded map с фоновым sweeper'ом.

// KVShard — шард KV-cache для одной сессии.
type KVShard struct {
	SessionID   string    `json:"session_id"`
	WorkerID    string    `json:"worker_id"`
	KeyTensor   []byte    `json:"key_tensor,omitempty"`   // opaque
	ValueTensor []byte    `json:"value_tensor,omitempty"` // opaque
	SeqLen      int       `json:"seq_len"`
	Layers      []int     `json:"layers,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	LastAccess  time.Time `json:"last_access"`
}

// KVStoreConfig — настройки.
type KVStoreConfig struct {
	TTL           time.Duration // default 30m
	MaxEntries    int           // default 1000
	SweepInterval time.Duration // default 1m
}

// DefaultKVStoreConfig — дефолты.
func DefaultKVStoreConfig() KVStoreConfig {
	return KVStoreConfig{
		TTL:           30 * time.Minute,
		MaxEntries:    1000,
		SweepInterval: 1 * time.Minute,
	}
}

// KVStore — thread-safe in-memory KV-хранилище.
type KVStore struct {
	mu       sync.RWMutex
	shards   map[string]*KVShard // sessionID → shard
	cfg      KVStoreConfig
	workerID string

	// Статистика.
	stats KVStoreStats
}

// KVStoreStats — счётчики.
type KVStoreStats struct {
	Saves   int64 // всего /kv_sync
	Loads   int64 // всего /kv_fetch успешных
	Misses  int64 // /kv_fetch без шарда
	Evicted int64 // удалено по TTL или LRU
	Stores  int   // текущее количество шардов
}

// NewKVStore создаёт KV-хранилище.
func NewKVStore(workerID string, cfg KVStoreConfig) *KVStore {
	if cfg.TTL <= 0 {
		cfg.TTL = DefaultKVStoreConfig().TTL
	}
	if cfg.MaxEntries <= 0 {
		cfg.MaxEntries = DefaultKVStoreConfig().MaxEntries
	}
	if cfg.SweepInterval <= 0 {
		cfg.SweepInterval = DefaultKVStoreConfig().SweepInterval
	}
	return &KVStore{
		shards:   make(map[string]*KVShard),
		cfg:      cfg,
		workerID: workerID,
	}
}

// Save сохраняет shard по session_id.
//
// Если уже существует — перезаписывает. Возвращает error только при
// превышении MaxEntries (новый шард).
func (k *KVStore) Save(shard *KVShard) error {
	if shard == nil {
		return nil
	}
	if shard.SessionID == "" {
		return nil
	}
	now := time.Now()
	shard.CreatedAt = now
	shard.LastAccess = now
	if shard.WorkerID == "" {
		shard.WorkerID = k.workerID
	}

	k.mu.Lock()
	defer k.mu.Unlock()

	// Если есть — перезаписываем (не считаем за новый).
	if _, exists := k.shards[shard.SessionID]; !exists {
		if len(k.shards) >= k.cfg.MaxEntries {
			return errKVStoreFull
		}
		k.stats.Stores++
	}
	k.shards[shard.SessionID] = shard
	k.stats.Saves++
	return nil
}

// Load возвращает shard по session_id. Возвращает nil если не найден.
//
// bumpStats обновляет LastAccess и stats (load/miss).
func (k *KVStore) Load(sessionID string) *KVShard {
	if sessionID == "" {
		k.stats.Misses++
		return nil
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	shard, ok := k.shards[sessionID]
	if !ok {
		k.stats.Misses++
		return nil
	}
	shard.LastAccess = time.Now()
	k.stats.Loads++
	return shard
}

// Delete удаляет shard по session_id.
func (k *KVStore) Delete(sessionID string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	if _, ok := k.shards[sessionID]; ok {
		delete(k.shards, sessionID)
		k.stats.Stores--
		return true
	}
	return false
}

// List возвращает список всех session_id.
func (k *KVStore) List() []string {
	k.mu.RLock()
	defer k.mu.RUnlock()
	out := make([]string, 0, len(k.shards))
	for id := range k.shards {
		out = append(out, id)
	}
	return out
}

// Stats возвращает копию статистики.
func (k *KVStore) Stats() KVStoreStats {
	k.mu.RLock()
	defer k.mu.RUnlock()
	stats := k.stats
	stats.Stores = len(k.shards)
	return stats
}

// Sweep удаляет просроченные шарды (TTL).
//
// Возвращает количество удалённых.
func (k *KVStore) Sweep() int {
	now := time.Now()
	k.mu.Lock()
	defer k.mu.Unlock()

	evicted := 0
	for id, shard := range k.shards {
		if now.Sub(shard.LastAccess) > k.cfg.TTL {
			delete(k.shards, id)
			evicted++
		}
	}
	k.stats.Evicted += int64(evicted)
	k.stats.Stores = len(k.shards)
	return evicted
}

// EncodeShard сериализует KV-shard в base64 для передачи по JSON.
//
// B4 stub: opaque bytes через base64. В production — safetensors / bincode.
func EncodeShard(shard *KVShard) string {
	if shard == nil {
		return ""
	}
	// Простая сериализация: base64(key) + ":" + base64(value).
	return base64.StdEncoding.EncodeToString(shard.KeyTensor) + ":" +
		base64.StdEncoding.EncodeToString(shard.ValueTensor)
}

// DecodeShard десериализует shard из строки, созданной EncodeShard.
//
// B4 stub: возвращает shard с заполненными KeyTensor/ValueTensor.
func DecodeShard(sessionID, workerID string, seqLen int, encoded string) *KVShard {
	if encoded == "" {
		return &KVShard{
			SessionID: sessionID,
			WorkerID:  workerID,
			SeqLen:    seqLen,
		}
	}
	parts := splitOnce(encoded, ':')
	keyB, _ := base64.StdEncoding.DecodeString(parts[0])
	valB, _ := base64.StdEncoding.DecodeString(parts[1])
	return &KVShard{
		SessionID:   sessionID,
		WorkerID:    workerID,
		KeyTensor:   keyB,
		ValueTensor: valB,
		SeqLen:      seqLen,
	}
}

// splitOnce — split на 2 части по первому разделителю.
func splitOnce(s string, sep byte) [2]string {
	for i := 0; i < len(s); i++ {
		if s[i] == sep {
			return [2]string{s[:i], s[i+1:]}
		}
	}
	return [2]string{s, ""}
}

// errKVStoreFull — sentinel ошибка при переполнении.
var errKVStoreFull = kvStoreFullError{}

type kvStoreFullError struct{}

func (kvStoreFullError) Error() string { return "kv_store: max entries reached" }

// IsKVStoreFullError — helper для проверки ошибки переполнения.
func IsKVStoreFullError(err error) bool {
	_, ok := err.(kvStoreFullError)
	return ok
}

// =====================================================================
// B8 — TP (tensor parallelism) rank-keyed KV-shards
// =====================================================================
//
// В B8 (tensor parallelism) каждый worker (rank) обслуживает только
// свой шард матриц, и KV-cache для сессии тоже делится по rank'ам.
// KVStore.SaveShard/LoadShard используют составной ключ (sessionID, rank),
// чтобы partial KV-shard одного rank'а не перетирал partial KV-shard
// другого.
//
// API обратно совместимо с /rpc/kv_sync и /rpc/kv_fetch — последние
// используют Save/Load без rank, и для backward-compat это key="".
// Новые /rpc/tp/kv_sync и /rpc/tp/kv_fetch используют SaveShard/LoadShard
// с явным rank в URL (?rank=N).

// shardKey — составной ключ (sessionID, rank) для TP shards.
func shardKey(sessionID string, rank int) string {
	return sessionID + "|" + itoaRank(rank)
}

// itoaRank — простая конверсия rank в строку без strconv (избегаем
// дополнительных allocation в hot path KVStore.SaveShard).
func itoaRank(rank int) string {
	if rank == 0 {
		return "0"
	}
	neg := rank < 0
	if neg {
		rank = -rank
	}
	var buf [20]byte
	i := len(buf)
	for rank > 0 {
		i--
		buf[i] = byte('0' + rank%10)
		rank /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// SaveShard сохраняет TP KV-shard для конкретного rank'а сессии.
//
// Внутри использует тот же bounded map, что и Save — но с ключом
// (sessionID, rank). TTL и capacity — общие (KVStoreConfig).
//
// B8 stub: opaque bytes через base64 в JSON (см. EncodeShard/DecodeShard).
// В production с реальным llama.cpp — сериализация ggml cache через
// bincode или safetensors.
func (k *KVStore) SaveShard(sessionID string, rank int, shard []byte) error {
	if sessionID == "" {
		return nil
	}
	key := shardKey(sessionID, rank)
	entry := &KVShard{
		SessionID:   sessionID,
		WorkerID:    k.workerID,
		SeqLen:      -1, // для TP shards не отслеживаем seqLen (per-rank)
		KeyTensor:   shard,
		ValueTensor: nil,
		Layers:      []int{rank}, // rank для отладки
	}
	now := time.Now()
	entry.CreatedAt = now
	entry.LastAccess = now

	k.mu.Lock()
	defer k.mu.Unlock()

	if _, exists := k.shards[key]; !exists {
		if len(k.shards) >= k.cfg.MaxEntries {
			return errKVStoreFull
		}
		k.stats.Stores++
	}
	k.shards[key] = entry
	k.stats.Saves++
	return nil
}

// LoadShard возвращает TP KV-shard для конкретного rank'а сессии.
//
// nil → не найдено (или TTL истёк, или ещё не сохранён).
func (k *KVStore) LoadShard(sessionID string, rank int) []byte {
	if sessionID == "" {
		k.stats.Misses++
		return nil
	}
	key := shardKey(sessionID, rank)
	k.mu.Lock()
	defer k.mu.Unlock()
	entry, ok := k.shards[key]
	if !ok {
		k.stats.Misses++
		return nil
	}
	entry.LastAccess = time.Now()
	k.stats.Loads++
	return entry.KeyTensor
}

// DeleteShard удаляет TP KV-shard для конкретного rank'а.
//
// Используется при полной очистке сессии (DELETE /rpc/tp/kv_sync?rank=N)
// или по TTL через Sweep (общий для всех shards).
func (k *KVStore) DeleteShard(sessionID string, rank int) bool {
	if sessionID == "" {
		return false
	}
	key := shardKey(sessionID, rank)
	k.mu.Lock()
	defer k.mu.Unlock()
	if _, ok := k.shards[key]; ok {
		delete(k.shards, key)
		k.stats.Stores--
		return true
	}
	return false
}

// CountShardsForSession — число сохранённых TP shards для сессии.
//
// Используется в /api/v1/rpc/tp/status для диагностики: если сессия
// имеет shards для всех ranks — TP inference работает корректно.
// Если < worldSize — degraded mode или сессия не использовалась.
func (k *KVStore) CountShardsForSession(sessionID string) int {
	if sessionID == "" {
		return 0
	}
	prefix := sessionID + "|"
	k.mu.RLock()
	defer k.mu.RUnlock()
	count := 0
	for key := range k.shards {
		if len(key) > len(prefix) && key[:len(prefix)] == prefix {
			count++
		}
	}
	return count
}

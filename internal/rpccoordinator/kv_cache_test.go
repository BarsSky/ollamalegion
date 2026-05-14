package rpccoordinator

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewDistributedKVCache(t *testing.T) {
	kvc := NewDistributedKVCache(KVCacheConfig{}, nil)
	require.NotNil(t, kvc)
	assert.Equal(t, 30*time.Minute, kvc.ttl)
	assert.Equal(t, 0, len(kvc.shards))
}

func TestNewDistributedKVCache_Custom(t *testing.T) {
	kvc := NewDistributedKVCache(KVCacheConfig{
		TTL:        5 * time.Minute,
		MaxEntries: 500,
	}, nil)
	require.NotNil(t, kvc)
	assert.Equal(t, 5*time.Minute, kvc.ttl)
}

func TestRegisterSession(t *testing.T) {
	kvc := NewDistributedKVCache(KVCacheConfig{}, nil)
	err := kvc.RegisterSession("session-1", "worker-1", []int{1, 2, 3})
	require.NoError(t, err)

	shard := kvc.GetShard("session-1")
	require.NotNil(t, shard)
	assert.Equal(t, "session-1", shard.SessionID)
	assert.Equal(t, "worker-1", shard.WorkerID)
	assert.Equal(t, []int{1, 2, 3}, shard.Layers)
}

func TestRegisterSession_EmptyID(t *testing.T) {
	kvc := NewDistributedKVCache(KVCacheConfig{}, nil)
	err := kvc.RegisterSession("", "w1", nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "sessionID is required")
}

func TestGetShard_NotFound(t *testing.T) {
	kvc := NewDistributedKVCache(KVCacheConfig{}, nil)
	shard := kvc.GetShard("nonexistent")
	assert.Nil(t, shard)
}

func TestUpdateShard(t *testing.T) {
	kvc := NewDistributedKVCache(KVCacheConfig{}, nil)
	_ = kvc.RegisterSession("s1", "w1", []int{1})

	err := kvc.UpdateShard("s1", 10, []byte("key-data"), []byte("value-data"))
	require.NoError(t, err)

	shard := kvc.GetShard("s1")
	require.NotNil(t, shard)
	assert.Equal(t, 10, shard.SeqLen)
	assert.Equal(t, []byte("key-data"), shard.KeyTensor)
	assert.Equal(t, []byte("value-data"), shard.ValueTensor)
}

func TestUpdateShard_NotFound(t *testing.T) {
	kvc := NewDistributedKVCache(KVCacheConfig{}, nil)
	err := kvc.UpdateShard("nonexistent", 10, nil, nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestDeleteSession(t *testing.T) {
	kvc := NewDistributedKVCache(KVCacheConfig{}, nil)
	_ = kvc.RegisterSession("s1", "w1", nil)

	kvc.DeleteSession("s1")
	assert.Nil(t, kvc.GetShard("s1"))
}

func TestCleanupExpired(t *testing.T) {
	kvc := NewDistributedKVCache(KVCacheConfig{
		TTL: 100 * time.Millisecond,
	}, nil)
	_ = kvc.RegisterSession("s1", "w1", nil)

	// Ждём истечения TTL
	time.Sleep(200 * time.Millisecond)

	kvc.CleanupExpired()
	assert.Nil(t, kvc.GetShard("s1"))
}

func TestCleanupExpired_NotExpired(t *testing.T) {
	kvc := NewDistributedKVCache(KVCacheConfig{
		TTL: 1 * time.Hour,
	}, nil)
	_ = kvc.RegisterSession("s1", "w1", nil)

	kvc.CleanupExpired()
	assert.NotNil(t, kvc.GetShard("s1"))
}

func TestEvictLRU(t *testing.T) {
	kvc := NewDistributedKVCache(KVCacheConfig{
		TTL:        1 * time.Hour,
		MaxEntries: 2,
	}, nil)

	_ = kvc.RegisterSession("s1", "w1", nil)
	time.Sleep(50 * time.Millisecond)
	_ = kvc.RegisterSession("s2", "w1", nil)
	time.Sleep(50 * time.Millisecond)
	_ = kvc.RegisterSession("s3", "w1", nil) // Должен вытеснить s1

	// s1 должен быть вытеснен
	assert.Nil(t, kvc.GetShard("s1"))
	// s2 и s3 должны остаться
	assert.NotNil(t, kvc.GetShard("s2"))
	assert.NotNil(t, kvc.GetShard("s3"))
}

func TestGetStats(t *testing.T) {
	kvc := NewDistributedKVCache(KVCacheConfig{}, nil)
	_ = kvc.RegisterSession("s1", "w1", nil)
	_ = kvc.UpdateShard("s1", 5, []byte("key1234"), []byte("value5678"))

	stats := kvc.GetStats()
	assert.Equal(t, 1, stats["sessions"])
	// len("key1234") = 7, len("value5678") = 9, total = 16
	assert.Equal(t, 16, stats["totalSize"])
}

func TestMaxEntries(t *testing.T) {
	kvc := NewDistributedKVCache(KVCacheConfig{}, nil)
	assert.Equal(t, 1000, kvc.MaxEntries())
}

func TestAddRemoveWorker(t *testing.T) {
	kvc := NewDistributedKVCache(KVCacheConfig{}, nil)
	client := &WorkerClient{WorkerID: "w1"}

	kvc.AddWorker("w1", client)
	assert.Equal(t, 1, len(kvc.workers))

	kvc.RemoveWorker("w1")
	assert.Equal(t, 0, len(kvc.workers))
}

func TestSyncToWorker_NoShard(t *testing.T) {
	// Новая сессия — нечего синхронизировать
	kvc := NewDistributedKVCache(KVCacheConfig{}, nil)
	err := kvc.SyncToWorker(context.Background(), "new-session")
	assert.NoError(t, err)
}

func TestFetchFromWorker_NoShard(t *testing.T) {
	kvc := NewDistributedKVCache(KVCacheConfig{}, nil)
	err := kvc.FetchFromWorker(context.Background(), "nonexistent")
	assert.NoError(t, err)
}

func TestKVCacheShard_ConcurrentAccess(t *testing.T) {
	kvc := NewDistributedKVCache(KVCacheConfig{}, nil)
	_ = kvc.RegisterSession("s1", "w1", nil)

	// Конкурентные обновления
	done := make(chan bool, 10)
	for i := 0; i < 10; i++ {
		go func(idx int) {
			_ = kvc.UpdateShard("s1", idx, []byte("key"), []byte("value"))
			done <- true
		}(i)
	}

	for i := 0; i < 10; i++ {
		<-done
	}

	// После всех обновлений шард должен существовать
	shard := kvc.GetShard("s1")
	assert.NotNil(t, shard)
}
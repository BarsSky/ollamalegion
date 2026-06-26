package rptensor

import (
	"bytes"
	"context"
	"testing"
	"time"
)

// TestStubTPRuntime_InferShard — детерминированный partial output.
func TestStubTPRuntime_InferShard(t *testing.T) {
	rt := NewStubTPRuntime()
	defer rt.Close()

	m, _ := NewShardedModel("m", 4096, 14336, 4, 32, 128256)
	_ = m.Partition(MegatronPartition, 4)

	// Rank 0: partial output = 3 bytes (10/4=2 rem 2 → rank 0 = 3).
	out, err := rt.InferShard(context.Background(), m, 0, []byte("abcdefghij"))
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 3 {
		t.Errorf("rank 0 output len: got %d, want 3", len(out))
	}
	for i, b := range out {
		if b != 1 {
			t.Errorf("rank 0 byte[%d]: got %d, want 1", i, b)
		}
	}

	// Rank 1: тоже 3 bytes (rank < rem=2).
	out, _ = rt.InferShard(context.Background(), m, 1, []byte("abcdefghij"))
	if len(out) != 3 || out[0] != 2 {
		t.Errorf("rank 1: got %v, want [2 2 2]", out)
	}

	// Rank 3: 2 bytes (rank >= rem).
	out, _ = rt.InferShard(context.Background(), m, 3, []byte("abcdefghij"))
	if len(out) != 2 || out[0] != 4 {
		t.Errorf("rank 3: got %v, want [4 4]", out)
	}
}

// TestStubTPRuntime_AllReduce — concat в порядке rank.
func TestStubTPRuntime_AllReduce(t *testing.T) {
	rt := NewStubTPRuntime()
	defer rt.Close()

	partials := map[int][]byte{
		0: {1, 1, 1},
		1: {2, 2, 2},
		2: {3, 3, 3},
		3: {4, 4},
	}
	out, err := rt.AllReduce(partials, 4)
	if err != nil {
		t.Fatal(err)
	}
	expected := []byte{1, 1, 1, 2, 2, 2, 3, 3, 3, 4, 4}
	if !bytes.Equal(out, expected) {
		t.Errorf("AllReduce: got %v, want %v", out, expected)
	}
}

// TestStubTPRuntime_AllReduce_DegradedMode — пропускает отсутствующие ranks.
func TestStubTPRuntime_AllReduce_DegradedMode(t *testing.T) {
	rt := NewStubTPRuntime()
	defer rt.Close()

	// Rank 1 отсутствует (failed).
	partials := map[int][]byte{
		0: {1, 1},
		2: {3, 3, 3},
		3: {4, 4},
	}
	out, err := rt.AllReduce(partials, 4)
	if err != nil {
		t.Fatal(err)
	}
	// Должен включать rank 0, 2, 3 (rank 1 пропущен).
	expected := []byte{1, 1, 3, 3, 3, 4, 4}
	if !bytes.Equal(out, expected) {
		t.Errorf("degraded AllReduce: got %v, want %v", out, expected)
	}
}

// TestStubTPRuntime_AllReduce_AllMissing — error.
func TestStubTPRuntime_AllReduce_AllMissing(t *testing.T) {
	rt := NewStubTPRuntime()
	defer rt.Close()

	_, err := rt.AllReduce(map[int][]byte{}, 4)
	if err == nil {
		t.Error("expected error on empty partials")
	}
}

// TestStubTPRuntime_Errors — nil model, invalid rank, closed runtime.
func TestStubTPRuntime_Errors(t *testing.T) {
	rt := NewStubTPRuntime()
	defer rt.Close()

	m, _ := NewShardedModel("m", 4096, 14336, 4, 32, 128256)
	_ = m.Partition(MegatronPartition, 4)

	t.Run("nil model", func(t *testing.T) {
		_, err := rt.InferShard(context.Background(), nil, 0, nil)
		if err == nil {
			t.Error("expected error for nil model")
		}
	})
	t.Run("invalid rank", func(t *testing.T) {
		_, err := rt.InferShard(context.Background(), m, 100, nil)
		if err == nil {
			t.Error("expected error for rank >= worldSize")
		}
		_, err = rt.InferShard(context.Background(), m, -1, nil)
		if err == nil {
			t.Error("expected error for negative rank")
		}
	})
}

// TestStubTPRuntime_ContextCancellation — respect ctx.Done().
func TestStubTPRuntime_ContextCancellation(t *testing.T) {
	rt := NewStubTPRuntime()
	defer rt.Close()

	m, _ := NewShardedModel("m", 4096, 14336, 4, 32, 128256)
	_ = m.Partition(MegatronPartition, 4)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := rt.InferShard(ctx, m, 0, []byte("xxx"))
	if err == nil {
		t.Error("expected context cancellation error")
	}
	if err != context.Canceled {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

// TestStubTPRuntime_Close — после Close все методы возвращают ошибку.
func TestStubTPRuntime_Close(t *testing.T) {
	rt := NewStubTPRuntime()
	m, _ := NewShardedModel("m", 4096, 14336, 4, 32, 128256)
	_ = m.Partition(MegatronPartition, 4)

	if err := rt.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if rt.Name() != "stub" {
		t.Errorf("Name after close: got %q", rt.Name())
	}
	_, err := rt.InferShard(context.Background(), m, 0, nil)
	if err == nil {
		t.Error("InferShard after close should fail")
	}
	_, err = rt.AllReduce(map[int][]byte{0: {1}}, 4)
	if err == nil {
		t.Error("AllReduce after close should fail")
	}
}

// TestStubTPRuntime_ConcurrentInfer — thread safety.
func TestStubTPRuntime_ConcurrentInfer(t *testing.T) {
	rt := NewStubTPRuntime()
	defer rt.Close()

	m, _ := NewShardedModel("m", 4096, 14336, 4, 32, 128256)
	_ = m.Partition(MegatronPartition, 4)

	done := make(chan bool, 10)
	for i := 0; i < 10; i++ {
		go func(rank int) {
			_, _ = rt.InferShard(context.Background(), m, rank%4, []byte("concurrent"))
			done <- true
		}(i)
	}
	for i := 0; i < 10; i++ {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("timeout")
		}
	}
}
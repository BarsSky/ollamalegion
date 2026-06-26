package rptensor

import (
	"bytes"
	"testing"
)

// =====================================================================
// AllReduceConcat
// =====================================================================

func TestAllReduceConcat_Basic(t *testing.T) {
	partials := map[int][]byte{
		0: []byte("aa"),
		1: []byte("bb"),
		2: []byte("cc"),
		3: []byte("dd"),
	}
	out, err := AllReduceConcat(partials, 4)
	if err != nil {
		t.Fatalf("Concat: %v", err)
	}
	if string(out) != "aabbccdd" {
		t.Errorf("got %q, want %q", out, "aabbccdd")
	}
}

func TestAllReduceConcat_MissingRank_Degraded(t *testing.T) {
	// При отсутствии rank'а AllReduceConcat работает в degraded mode —
	// пропускает отсутствующий rank и конкатенирует остальные (без ошибки).
	// Это позволяет TP inference продолжаться при отказе отдельных worker'ов.
	partials := map[int][]byte{
		0: []byte("a"),
		2: []byte("c"),
		3: []byte("d"),
	}
	out, err := AllReduceConcat(partials, 4)
	if err != nil {
		t.Fatalf("expected no error in degraded mode, got %v", err)
	}
	if string(out) != "acd" {
		t.Errorf("degraded concat: got %q, want %q", out, "acd")
	}
}

func TestAllReduceConcat_AllMissing(t *testing.T) {
	// Если все ranks отсутствуют — error.
	_, err := AllReduceConcat(map[int][]byte{}, 4)
	if err == nil {
		t.Error("expected error when all ranks missing")
	}
}

func TestAllReduceConcat_EmptyPartial(t *testing.T) {
	partials := map[int][]byte{
		0: []byte("a"),
		1: nil, // пустой partial
		2: []byte("c"),
		3: []byte("d"),
	}
	out, err := AllReduceConcat(partials, 4)
	if err != nil {
		t.Fatalf("Concat: %v", err)
	}
	if string(out) != "acd" {
		t.Errorf("got %q, want %q", out, "acd")
	}
}

func TestAllReduceConcat_InvalidWorldSize(t *testing.T) {
	if _, err := AllReduceConcat(map[int][]byte{}, 0); err == nil {
		t.Error("expected error on worldSize=0")
	}
	if _, err := AllReduceConcat(map[int][]byte{}, -1); err == nil {
		t.Error("expected error on negative worldSize")
	}
}

// =====================================================================
// AllReduceSumBytes
// =====================================================================

func TestAllReduceSumBytes_Basic(t *testing.T) {
	partials := map[int][]byte{
		0: {10, 20, 30},
		1: {1, 2, 3},
		2: {5, 5, 5},
		3: {100, 50, 0},
	}
	out, err := AllReduceSumBytes(partials, 4)
	if err != nil {
		t.Fatalf("Sum: %v", err)
	}
	// 10+1+5+100=116, 20+2+5+50=77, 30+3+5+0=38.
	expected := []byte{116, 77, 38}
	if !bytes.Equal(out, expected) {
		t.Errorf("got %v, want %v", out, expected)
	}
}

func TestAllReduceSumBytes_LengthMismatch(t *testing.T) {
	partials := map[int][]byte{
		0: {1, 2, 3},
		1: {1, 2}, // shorter
	}
	_, err := AllReduceSumBytes(partials, 2)
	if err == nil {
		t.Error("expected error on length mismatch")
	}
}

func TestAllReduceSumBytes_Overflow(t *testing.T) {
	partials := map[int][]byte{
		0: {200},
		1: {200},
	}
	out, err := AllReduceSumBytes(partials, 2)
	if err != nil {
		t.Fatal(err)
	}
	// 200+200 = 400 mod 256 = 144.
	if out[0] != 144 {
		t.Errorf("overflow: got %d, want 144", out[0])
	}
}

func TestAllReduceSumBytes_Empty(t *testing.T) {
	partials := map[int][]byte{
		0: {},
		1: {},
	}
	out, err := AllReduceSumBytes(partials, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 0 {
		t.Errorf("expected empty, got %d bytes", len(out))
	}
}

// =====================================================================
// AllReduceXorBytes
// =====================================================================

func TestAllReduceXorBytes_Basic(t *testing.T) {
	partials := map[int][]byte{
		0: {0xFF, 0x00, 0xAA},
		1: {0xFF, 0xFF, 0x00},
		2: {0x00, 0x00, 0xAA},
		3: {0x00, 0xFF, 0xFF},
	}
	out, err := AllReduceXorBytes(partials, 4)
	if err != nil {
		t.Fatalf("Xor: %v", err)
	}
	// 0xFF^0xFF^0x00^0x00 = 0, 0x00^0xFF^0x00^0xFF = 0, 0xAA^0x00^0xAA^0xFF = 0xFF.
	expected := []byte{0x00, 0x00, 0xFF}
	if !bytes.Equal(out, expected) {
		t.Errorf("got %v, want %v", out, expected)
	}
}

func TestAllReduceXorBytes_LengthMismatch(t *testing.T) {
	partials := map[int][]byte{
		0: {1, 2, 3},
		1: {1, 2},
	}
	if _, err := AllReduceXorBytes(partials, 2); err == nil {
		t.Error("expected error on length mismatch")
	}
}

// =====================================================================
// AllReduceMeanBytes
// =====================================================================

func TestAllReduceMeanBytes_Basic(t *testing.T) {
	partials := map[int][]byte{
		0: {10, 20, 30},
		1: {1, 2, 3},
		2: {5, 5, 5},
		3: {100, 50, 0},
	}
	out, err := AllReduceMeanBytes(partials, 4)
	if err != nil {
		t.Fatalf("Mean: %v", err)
	}
	// Sum=116/77/38 → mean = 116/4=29, 77/4=19, 38/4=9.
	expected := []byte{29, 19, 9}
	if !bytes.Equal(out, expected) {
		t.Errorf("got %v, want %v", out, expected)
	}
}

// =====================================================================
// PartialLenForRank
// =====================================================================

func TestPartialLenForRank(t *testing.T) {
	tests := []struct {
		inputLen, ws, rank, want int
	}{
		// 52 / 4 = 13, no remainder → все 13.
		{52, 4, 0, 13},
		{52, 4, 1, 13},
		{52, 4, 2, 13},
		{52, 4, 3, 13},
		// 53 / 4 = 13 rem 1 → rank 0 = 14, остальные = 13.
		{53, 4, 0, 14},
		{53, 4, 1, 13},
		{53, 4, 2, 13},
		{53, 4, 3, 13},
		// 56 / 4 = 14, no remainder.
		{56, 4, 0, 14},
		{56, 4, 3, 14},
		// edge cases.
		{0, 4, 0, 0},
		{10, 1, 0, 10},
		{10, 4, -1, 0},
		{10, 4, 4, 0},
		{10, 0, 0, 0},
	}
	for _, tt := range tests {
		got := PartialLenForRank(tt.inputLen, tt.ws, tt.rank)
		if got != tt.want {
			t.Errorf("PartialLenForRank(%d, %d, %d) = %d, want %d",
				tt.inputLen, tt.ws, tt.rank, got, tt.want)
		}
	}
}
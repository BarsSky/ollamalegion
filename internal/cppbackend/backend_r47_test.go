// R47 (2026-08-19): regression test for GGUF header parser's array-of-strings
// handling. Pre-R47, readGGUFHeaderInfo's case 9 (array) only skipped
// fixed-size elements via ggufTypeSize, which returns 0 for string type
// (elemType=8). The "skipBytes > 0" check then made the array skip a
// no-op, and the next iteration read garbage as the next keyLen —
// triggering the "keyLen > 8192" guard and returning an error.
//
// The live impact: any Qwen3 / Qwen3.5-MOE GGUF with `general.tags =
// ["qwen", ...]` (string array) failed /api/show for unloaded state,
// returning "unknown" for architecture.
//
// R47 fix: handle elemType=8 explicitly in the array case — read each
// string's length prefix and skip its payload. This test pins the
// behaviour with a minimal in-memory GGUF v3 file containing a string
// array in metadata.
package cppbackend

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// writeGGUFWithStringArray builds a minimal GGUF v3 file with a single
// metadata KV: a string array. Layout (little-endian):
//   4   magic "GGUF"
//   4   version 3
//   8   tensorCount 0
//   8   metadataKvCount 1
//   8   keyLen=11
//   11  key="my.stringa"   (array key — note suffix "a" not in known arch list)
//   4   valType=9 (array)
//   4   elemType=8 (string)
//   8   arrLen=3
//   8   str0Len=5
//   5   str0="alpha"
//   8   str1Len=4
//   4   str1="beta"
//   8   str2Len=6
//   6   str2="gamma_"
func writeGGUFWithStringArray(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "r47_test.gguf")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	w := binary.LittleEndian
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	mustWrite := func(n int, err error) {
		if err != nil {
			t.Fatal(err)
		}
		_ = n
	}
	must(binary.Write(f, w, []byte("GGUF")))
	must(binary.Write(f, w, uint32(3)))
	must(binary.Write(f, w, uint64(0)))  // tensorCount
	must(binary.Write(f, w, uint64(1)))  // metadataKvCount = 1
	key := "my.stringa"                  // suffix 'a' isn't in ggufFieldSuffixes
	must(binary.Write(f, w, uint64(len(key))))
	mustWrite(f.Write([]byte(key)))
	must(binary.Write(f, w, uint32(9)))  // valType = array
	must(binary.Write(f, w, uint32(8)))  // elemType = string
	must(binary.Write(f, w, uint64(3)))  // arrLen = 3
	for _, s := range []string{"alpha", "beta", "gamma_"} {
		must(binary.Write(f, w, uint64(len(s))))
		mustWrite(f.Write([]byte(s)))
	}
	return path
}

// TestR47_ReadGGUFHeader_StringArray — the core R47 fix. Pre-R47, the
// parser bails out after the string array with "keyLen exceeds 8192".
// Post-R47, it cleanly parses through and returns a non-nil info.
func TestR47_ReadGGUFHeader_StringArray(t *testing.T) {
	path := writeGGUFWithStringArray(t)

	info, err := ReadGGUFHeader(path)
	if err != nil {
		t.Fatalf("R47 REGRESSION: ReadGGUFHeader returned error: %v", err)
	}
	if info == nil {
		t.Fatal("R47: ReadGGUFHeader returned nil info")
	}
	// Architecture is empty for this synthetic GGUF (we used an unknown
	// arch key), but the function should NOT have errored. Architecture
	// being "" is acceptable — only the no-error + non-nil info is the
	// regression test.
	if info.Architecture != "" {
		t.Errorf("unexpected Architecture = %q (synthetic key uses unknown arch prefix)", info.Architecture)
	}
	if info.FileSize == 0 {
		t.Errorf("FileSize should be populated from stat, got 0")
	}
}

// TestR47_ReadGGUFHeader_AfterStringArrayCanReadMoreKVs — pre-R47, the
// parser stopped after the string array and couldn't read subsequent
// KVs. Post-R47, it should continue through the entire file. This test
// builds a GGUF with TWO KVs: a string array followed by a uint32.
// Pre-R47, the uint32 is never reached. Post-R47, it's parsed.
func TestR47_ReadGGUFHeader_AfterStringArrayCanReadMoreKVs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "r47_test2.gguf")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	w := binary.LittleEndian
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	mustWrite := func(n int, err error) {
		if err != nil {
			t.Fatal(err)
		}
		_ = n
	}
	must(binary.Write(f, w, []byte("GGUF")))
	must(binary.Write(f, w, uint32(3)))
	must(binary.Write(f, w, uint64(0)))  // tensorCount
	must(binary.Write(f, w, uint64(2)))  // metadataKvCount = 2

	// KV1: string array
	key1 := "my.tags"
	must(binary.Write(f, w, uint64(len(key1))))
	mustWrite(f.Write([]byte(key1)))
	must(binary.Write(f, w, uint32(9)))  // array
	must(binary.Write(f, w, uint32(8)))  // string elements
	must(binary.Write(f, w, uint64(2)))
	for _, s := range []string{"alpha", "beta"} {
		must(binary.Write(f, w, uint64(len(s))))
		mustWrite(f.Write([]byte(s)))
	}

	// KV2: uint32 (so we can verify the parser continued past the array).
	// Use a key that matches an existing arch prefix so ggufKeyMap maps it
	// to a field index. qwen3 is in ggufModelArchs.
	key2 := "qwen3.block_count"
	must(binary.Write(f, w, uint64(len(key2))))
	mustWrite(f.Write([]byte(key2)))
	must(binary.Write(f, w, uint32(4)))  // uint32
	must(binary.Write(f, w, uint32(42)))

	info, err := ReadGGUFHeader(path)
	if err != nil {
		t.Fatalf("R47 REGRESSION: ReadGGUFHeader failed after string array: %v", err)
	}
	if info == nil {
		t.Fatal("ReadGGUFHeader returned nil")
	}
	if info.NLayers != 42 {
		t.Errorf("NLayers = %d, want 42 (parser must continue past string array)", info.NLayers)
	}
	// Architecture is set only from "general.architecture" key, not from
	// arch-suffix keys. This test only writes the latter, so Architecture
	// stays "". That's the expected behaviour — no assertion needed.
	_ = info.Architecture
}

// TestR47_ReadGGUFHeader_BadStringLengthGuards — R47 fix must not
// regress the "keyLen > 8192" guard. A string of 10000 bytes inside
// an array must still trigger the guard cleanly.
func TestR47_ReadGGUFHeader_BadStringLengthGuards(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "r47_test3.gguf")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	w := binary.LittleEndian
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	mustWrite := func(n int, err error) {
		if err != nil {
			t.Fatal(err)
		}
		_ = n
	}
	must(binary.Write(f, w, []byte("GGUF")))
	must(binary.Write(f, w, uint32(3)))
	must(binary.Write(f, w, uint64(0)))
	must(binary.Write(f, w, uint64(1)))

	key := "my.bad"
	must(binary.Write(f, w, uint64(len(key))))
	mustWrite(f.Write([]byte(key)))
	must(binary.Write(f, w, uint32(9)))   // array
	must(binary.Write(f, w, uint32(8)))   // string
	must(binary.Write(f, w, uint64(1)))   // 1 element
	// String length = 10000, exceeds 8192 → must error
	must(binary.Write(f, w, uint64(10000)))

	_, err = ReadGGUFHeader(path)
	if err == nil {
		t.Fatal("expected error for string length > 8192 inside array, got nil")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("exceeds maximum")) {
		t.Errorf("error message should mention 'exceeds maximum', got: %v", err)
	}
}

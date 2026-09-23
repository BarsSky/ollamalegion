// gguf_header_buffered_r66d_test.go — R66d (2026-09-23).
//
// РЕГРЕСС. readGGUFHeaderInfo разбирал GGUF-заголовок поштучными чтениями:
// для массивов строк (tokenizer.ggml.tokens) он на КАЖДУЮ строку делал
// binary.Read + f.Seek. На локальном диске это терпимо (gemma-4: 262144 строки →
// ~2.4 с), а на bind-mount Docker Desktop (Windows → WSL2) каждое чтение/seek
// идёт через границу ФС, и разбор фактически зависал: goroutine застревала в
// readGGUFHeaderInfo, операция load вечно висела в статусе running, GPU
// простаивала, а модель gemma-4-E4B-it-Q4_K_M вообще нельзя было загрузить.
//
// Теперь чтения идут через bufio, а пропуски — через io.CopyN(io.Discard, ...).
// Тест собирает маленький синтетический GGUF с массивом строк и проверяет, что
// заголовок читается корректно (в т.ч. ключи ПОСЛЕ массива — это то, что ломалось
// в R47, когда массив строк пропускался неверно).

package cppbackend

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func ggufU32(b *[]byte, v uint32) { *b = binary.LittleEndian.AppendUint32(*b, v) }
func ggufU64(b *[]byte, v uint64) { *b = binary.LittleEndian.AppendUint64(*b, v) }
func ggufStr(b *[]byte, s string) {
	ggufU64(b, uint64(len(s)))
	*b = append(*b, s...)
}

// ggufKey — ключ метаданных: имя + значение (string/uint32/array-of-strings).
type ggufKey struct {
	name    string
	valType uint32
	str     string
	u32     uint32
	arr     []string
}

func buildTestGGUF(t *testing.T, keys []ggufKey) string {
	t.Helper()
	var b []byte
	b = append(b, 'G', 'G', 'U', 'F')
	ggufU32(&b, 3)                 // version
	ggufU64(&b, 0)                 // tensorCount
	ggufU64(&b, uint64(len(keys))) // kvCount

	for _, k := range keys {
		ggufStr(&b, k.name)
		ggufU32(&b, k.valType)
		switch k.valType {
		case 8: // string
			ggufStr(&b, k.str)
		case 4: // uint32
			ggufU32(&b, k.u32)
		case 9: // array
			ggufU32(&b, 8) // elemType = string (проверяем именно этот путь)
			ggufU64(&b, uint64(len(k.arr)))
			for _, s := range k.arr {
				ggufStr(&b, s)
			}
		default:
			t.Fatalf("тест не умеет писать тип %d", k.valType)
		}
	}

	path := filepath.Join(t.TempDir(), "test.gguf")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

func TestReadGGUFHeaderInfo_ArrayOfStrings_R66d(t *testing.T) {
	// Важно: general.name идёт ПОСЛЕ массива строк — если массив пропущен неверно,
	// парсер уедет и вернёт ошибку/пустую архитектуру (класс бага R47).
	path := buildTestGGUF(t, []ggufKey{
		{name: "general.architecture", valType: 8, str: "gemma4"},
		{name: "tokenizer.ggml.tokens", valType: 9, arr: []string{"a", "bb", "ccc", "dddd"}},
		{name: "general.name", valType: 8, str: "test-model"},
		{name: "gemma4.block_count", valType: 4, u32: 42},
		{name: "gemma4.attention.head_count", valType: 4, u32: 8},
		{name: "gemma4.attention.head_count_kv", valType: 4, u32: 2},
		{name: "gemma4.embedding_length", valType: 4, u32: 2560},
		{name: "gemma4.context_length", valType: 4, u32: 131072},
	})

	info, err := readGGUFHeaderInfo(path)
	if err != nil {
		t.Fatalf("readGGUFHeaderInfo: %v", err)
	}
	if info.Architecture != "gemma4" {
		t.Errorf("Architecture = %q, want gemma4", info.Architecture)
	}
	if info.NLayers != 42 || info.NHeads != 8 || info.NKvHeads != 2 || info.NEmbd != 2560 {
		t.Errorf("arch-поля прочитаны неверно: %+v", info)
	}
	if info.ContextLength != 131072 {
		t.Errorf("ContextLength = %d, want 131072", info.ContextLength)
	}
}

func TestReadGGUFHeaderInfo_NoArray_R66d(t *testing.T) {
	path := buildTestGGUF(t, []ggufKey{
		{name: "general.architecture", valType: 8, str: "qwen3"},
		{name: "qwen3.block_count", valType: 4, u32: 28},
		{name: "qwen3.attention.head_count", valType: 4, u32: 16},
	})

	info, err := readGGUFHeaderInfo(path)
	if err != nil {
		t.Fatalf("readGGUFHeaderInfo: %v", err)
	}
	if info.Architecture != "qwen3" || info.NLayers != 28 || info.NHeads != 16 {
		t.Errorf("прочитано неверно: %+v", info)
	}
}

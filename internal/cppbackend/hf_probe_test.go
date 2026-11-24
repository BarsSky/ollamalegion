// hf_probe_test.go — HTTP-путь пред-проверки: ProbeFile читает Range, а не файл.
//
// РАЗБОР ЗАГОЛОВКА ПРОВЕРЯЕТСЯ В internal/imagemodel (probe_test.go): с Фазы 4
// он cgo-свободен и переиспользуется сервисом ре-экспорта. Здесь осталось то,
// что принадлежит cppbackend: сам HTTP-запрос за заголовком (Range, Content-Range,
// лимит чтения при сервере без Range). Строители GGUF — минимальные, только для
// этих трёх проверок; полный набор строителей живёт вместе с разбором.
package cppbackend

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// --- синтетический GGUF -----------------------------------------------------

type ggufKV struct {
	key   string
	value any
}

type ggufTensor struct {
	name string
	dims []uint64
}

// buildGGUF собирает минимальный, но валидный GGUF: magic/версия/счётчики, KV-пары
// и таблицу тензоров (имя + dims + тип + offset).
func buildGGUF(t *testing.T, kvs []ggufKV, tensors []ggufTensor) []byte {
	t.Helper()
	var buf bytes.Buffer
	buf.WriteString("GGUF")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(3))
	_ = binary.Write(&buf, binary.LittleEndian, uint64(len(tensors)))
	_ = binary.Write(&buf, binary.LittleEndian, uint64(len(kvs)))
	writeStr := func(s string) {
		_ = binary.Write(&buf, binary.LittleEndian, uint64(len(s)))
		buf.WriteString(s)
	}
	for _, kv := range kvs {
		writeStr(kv.key)
		switch v := kv.value.(type) {
		case string:
			_ = binary.Write(&buf, binary.LittleEndian, uint32(8)) // string
			writeStr(v)
		case uint32:
			_ = binary.Write(&buf, binary.LittleEndian, uint32(4)) // uint32
			_ = binary.Write(&buf, binary.LittleEndian, v)
		case []uint32:
			_ = binary.Write(&buf, binary.LittleEndian, uint32(9)) // array
			_ = binary.Write(&buf, binary.LittleEndian, uint32(4)) // элементов — uint32
			_ = binary.Write(&buf, binary.LittleEndian, uint64(len(v)))
			for _, e := range v {
				_ = binary.Write(&buf, binary.LittleEndian, e)
			}
		case []string:
			_ = binary.Write(&buf, binary.LittleEndian, uint32(9)) // array
			_ = binary.Write(&buf, binary.LittleEndian, uint32(8)) // элементов — string
			_ = binary.Write(&buf, binary.LittleEndian, uint64(len(v)))
			for _, e := range v {
				writeStr(e)
			}
		}
	}
	for _, tsr := range tensors {
		writeStr(tsr.name)
		_ = binary.Write(&buf, binary.LittleEndian, uint32(len(tsr.dims)))
		for _, d := range tsr.dims {
			_ = binary.Write(&buf, binary.LittleEndian, d)
		}
		_ = binary.Write(&buf, binary.LittleEndian, uint32(0)) // тип
		_ = binary.Write(&buf, binary.LittleEndian, uint64(0)) // offset
	}
	return buf.Bytes()
}

func names(list ...string) []ggufTensor {
	out := make([]ggufTensor, 0, len(list))
	for _, n := range list {
		out = append(out, ggufTensor{name: n})
	}
	return out
}

// --- HTTP: Range-запрос и вердикт -------------------------------------------

func TestProbeFile_UsesRangeAndReturnsVerdict(t *testing.T) {
	raw := buildGGUF(t, []ggufKV{{"general.architecture", "qwen_image21"}}, names(
		"txt_in.text_norm.weight",
		"transformer_blocks.0.img_mlp.gate_layer.weight",
		"transformer_blocks.0.img_mlp.proj.weight",
		"img_in.weight",
	))
	var gotRange string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRange = r.Header.Get("Range")
		if !strings.Contains(r.URL.Path, "/resolve/main/") {
			t.Errorf("неожиданный путь: %s", r.URL.Path)
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", len(raw)-1, len(raw)+999))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(raw)
	}))
	defer srv.Close()

	d := NewHuggingFaceDownloader("", srv.URL, t.TempDir(), t.TempDir())
	res, err := d.ProbeFile(context.Background(), "abenzerps/Qwen-Image-2.1-Uncensored-GGUF", "qwen-image-2.1-UC-Q4_0.gguf", "main")
	if err != nil {
		t.Fatalf("ProbeFile: %v", err)
	}
	if gotRange == "" || !strings.HasPrefix(gotRange, "bytes=0-") {
		t.Fatalf("Range-заголовок не отправлен (%q) — иначе мы бы качали весь файл", gotRange)
	}
	if res.Verdict != HFProbeSupported {
		t.Fatalf("verdict=%q (reason: %s)", res.Verdict, res.Reason)
	}
	if res.Family != "qwen_image" || res.Version != "Qwen Image 2.1" || !res.DiT {
		t.Fatalf("family=%q version=%q dit=%v", res.Family, res.Version, res.DiT)
	}
	if res.Format != "gguf" || res.Arch != "qwen_image21" {
		t.Fatalf("format=%q arch=%q", res.Format, res.Arch)
	}
	if res.SizeBytes != int64(len(raw)+999) {
		t.Fatalf("size=%d, want %d (из Content-Range)", res.SizeBytes, len(raw)+999)
	}
}

func TestProbeFile_EmptyParams(t *testing.T) {
	d := NewHuggingFaceDownloader("", "", t.TempDir(), t.TempDir())
	if _, err := d.ProbeFile(context.Background(), "", "x.gguf", "main"); err == nil {
		t.Fatal("ожидали ошибку на пустой modelId")
	}
	if _, err := d.ProbeFile(context.Background(), "a/b", "", "main"); err == nil {
		t.Fatal("ожидали ошибку на пустой filename")
	}
}

// Заголовок должен читаться быстро и не зависеть от размера файла: проверяем,
// что при 200 (без Range-поддержки) мы всё равно не вычитываем больше лимита.
func TestProbeFile_LimitsReadWhenRangeUnsupported(t *testing.T) {
	raw := buildGGUF(t, nil, names("double_blocks.0.img_attn.qkv.weight"))
	big := make([]byte, HFProbeRangeBytes*3)
	copy(big, raw)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		for i := 0; i < len(big); i += 4096 {
			end := i + 4096
			if end > len(big) {
				end = len(big)
			}
			_, _ = w.Write(big[i:end])
		}
	}))
	defer srv.Close()

	d := NewHuggingFaceDownloader("", srv.URL, t.TempDir(), t.TempDir())
	res, err := d.ProbeFile(context.Background(), "a/b", "m.gguf", "main")
	if err != nil {
		t.Fatalf("ProbeFile: %v", err)
	}
	if res.Verdict != HFProbeSupported {
		t.Fatalf("verdict=%q (reason: %s)", res.Verdict, res.Reason)
	}
	if res.CheckedB > HFProbeRangeBytes {
		t.Fatalf("прочитано %d байт > лимита %d", res.CheckedB, HFProbeRangeBytes)
	}
}

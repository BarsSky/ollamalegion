// ggufdump — диагностика: какие ключи KV-кэша есть в GGUF-файле.
// Нужен, чтобы проверить раскладку KV (shared_kv_layers, sliding_window_pattern)
// до того, как её начнёт использовать memfit (internal/cppbackend/kv_layers.go).
package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"strings"
)

type reader struct {
	r io.Reader
	n int64
}

func (rd *reader) read(v interface{}) error {
	if err := binary.Read(rd.r, binary.LittleEndian, v); err != nil {
		return err
	}
	return nil
}

func (rd *reader) skip(n int64) error {
	_, err := io.CopyN(io.Discard, rd.r, n)
	return err
}

func (rd *reader) str() (string, error) {
	var l uint64
	if err := rd.read(&l); err != nil {
		return "", err
	}
	b := make([]byte, l)
	if _, err := io.ReadFull(rd.r, b); err != nil {
		return "", err
	}
	return string(b), nil
}

func sizeOf(t uint32) int64 {
	switch t {
	case 0, 1, 7:
		return 1
	case 2, 3:
		return 2
	case 4, 5, 6:
		return 4
	case 10, 11, 12:
		return 8
	}
	return 0
}

func main() {
	path := os.Args[1]
	f, err := os.Open(path)
	if err != nil {
		panic(err)
	}
	defer f.Close()
	rd := &reader{r: f}
	var magic [4]byte
	if _, err := io.ReadFull(f, magic[:]); err != nil {
		panic(err)
	}
	var version uint32
	var tensorCount, kvCount uint64
	_ = rd.read(&version)
	_ = rd.read(&tensorCount)
	_ = rd.read(&kvCount)
	fmt.Printf("magic=%s version=%d tensors=%d kv=%d\n", string(magic[:]), version, tensorCount, kvCount)

	for i := uint64(0); i < kvCount; i++ {
		key, err := rd.str()
		if err != nil {
			panic(err)
		}
		var vt uint32
		if err := rd.read(&vt); err != nil {
			panic(err)
		}
		switch vt {
		case 8:
			s, err := rd.str()
			if err != nil {
				panic(err)
			}
			if key == "general.architecture" {
				fmt.Printf("%s = %q\n", key, s)
			}
		case 4, 5:
			var v uint32
			_ = rd.read(&v)
			reportInt(key, int64(int32(v)), vt)
		case 10, 11:
			var v uint64
			_ = rd.read(&v)
			reportInt(key, int64(v), vt)
		case 6:
			var v float32
			_ = rd.read(&v)
		case 0, 1, 7:
			var v uint8
			_ = rd.read(&v)
			reportInt(key, int64(v), vt)
		case 2, 3:
			var v uint16
			_ = rd.read(&v)
		case 12:
			var v float64
			_ = rd.read(&v)
		case 9:
			var et uint32
			var n uint64
			_ = rd.read(&et)
			_ = rd.read(&n)
			interested := strings.Contains(key, "attention") || strings.Contains(key, "swa") || strings.Contains(key, "pattern")
			if interested {
				vals := make([]int64, 0, n)
				for j := uint64(0); j < n; j++ {
					switch et {
					case 0, 1, 7:
						var v uint8
						_ = rd.read(&v)
						vals = append(vals, int64(v))
					case 4, 5:
						var v uint32
						_ = rd.read(&v)
						vals = append(vals, int64(int32(v)))
					case 10, 11:
						var v uint64
						_ = rd.read(&v)
						vals = append(vals, int64(v))
					case 6:
						var v float32
						_ = rd.read(&v)
					case 12:
						var v float64
						_ = rd.read(&v)
					case 8:
						s, err := rd.str()
						if err != nil {
							panic(err)
						}
						if n <= 8 {
							vals = append(vals, 0)
							fmt.Printf("  (str elem %q)\n", s)
						}
					default:
						panic("bad elem type")
					}
				}
				if et != 8 {
					trues := 0
					for _, v := range vals {
						if v != 0 {
							trues++
						}
					}
					fmt.Printf("%s = [array type=%d len=%d trues=%d] %v\n", key, et, n, trues, trunc(vals))
				}
			} else if sizeOf(et) > 0 {
				_ = rd.skip(int64(n) * sizeOf(et))
			} else if et == 8 {
				for j := uint64(0); j < n; j++ {
					if _, err := rd.str(); err != nil {
						panic(err)
					}
				}
			} else {
				panic("bad array")
			}
		default:
			panic(fmt.Sprintf("type %d for %s", vt, key))
		}
	}
}

func trunc(v []int64) []int64 {
	if len(v) > 48 {
		return v[:48]
	}
	return v
}

func reportInt(key string, v int64, vt uint32) {
	if strings.Contains(key, "attention") || strings.Contains(key, "block_count") ||
		strings.Contains(key, "context_length") || strings.Contains(key, "kv") {
		fmt.Printf("%s = %d (type=%d)\n", key, v, vt)
	}
}

// gguf_test.go — парсер шапки GGUF и вердикт схемы.
//
// ЗАЧЕМ ТЕСТЫ НА ПАРСЕР. Он читает чужие файлы на 16 ГиБ и решает, будет ли
// вообще доступна кнопка «ре-экспорт». Ошибка здесь стоит дорого: «файл
// нечитаем» на живом стенде выглядит как сломанный сервис, а «схема plain» на
// loop-экспорте — как «патч не нужен» и падение загрузчика на blk.24. Поэтому
// проверяются не только счастливые значения, но и границы: выравнивание,
// неизвестный тип значения, отсутствие сигнатуры, глубина/предел обхода.
package main

import (
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// ggufValueWriter — сборка тестового GGUF-файла вручную: так тест не зависит от
// python-скрипта и работает на любой машине без зависимостей.
type ggufValueWriter struct{ buf []byte }

func (w *ggufValueWriter) u32(v uint32) {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], v)
	w.buf = append(w.buf, b[:]...)
}

func (w *ggufValueWriter) u64(v uint64) {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], v)
	w.buf = append(w.buf, b[:]...)
}

func (w *ggufValueWriter) str(s string) {
	w.u64(uint64(len(s)))
	w.buf = append(w.buf, []byte(s)...)
}

func (w *ggufValueWriter) kvString(key, val string) {
	w.str(key)
	w.u32(uint32(ggufTstring))
	w.str(val)
}

func (w *ggufValueWriter) kvUint32(key string, val uint32) {
	w.str(key)
	w.u32(uint32(ggufTuint32))
	w.u32(val)
}

func (w *ggufValueWriter) kvUint32Array(key string, vals []uint32) {
	w.str(key)
	w.u32(uint32(ggufTarray))
	w.u32(uint32(ggufTuint32))
	w.u64(uint64(len(vals)))
	for _, v := range vals {
		w.u32(v)
	}
}

func (w *ggufValueWriter) kvStringArray(key string, vals []string) {
	w.str(key)
	w.u32(uint32(ggufTarray))
	w.u32(uint32(ggufTstring))
	w.u64(uint64(len(vals)))
	for _, v := range vals {
		w.str(v)
	}
}

// tensor описывает тензор в таблице: имя + размерности (тип F32).
func (w *ggufValueWriter) tensor(name string, dims ...uint64) {
	w.str(name)
	w.u32(uint32(len(dims)))
	for _, d := range dims {
		w.u64(d)
	}
	w.u32(0) // ggml_type F32
	w.u64(0) // offset: тесту важен только порядок, не байты
}

// writeTestGGUF собирает файл: заголовок + таблица тензоров + выравнивание + данные.
func writeTestGGUF(t *testing.T, path string, alignment uint64, blkCount int, arch string) {
	t.Helper()
	w := &ggufValueWriter{}
	w.buf = append(w.buf, []byte(ggufMagic)...)
	w.u32(3) // версия
	// Количество тензоров считаем заранее: 2*blkCount «обычных» + 2 матрицы запросов.
	tensorCount := uint64(blkCount*2) + 2
	if blkCount > 0 {
		tensorCount += uint64(blkCount) // loop_msg_query на каждый блок
	}
	w.u64(tensorCount)
	// 6 KV-пар: arch, block_count, head_count, head_count_kv, loop.varying, alignment.
	w.u64(6)
	w.kvString("general.architecture", arch)
	visits := make([]uint32, blkCount*2)
	for i := range visits {
		visits[i] = 8
	}
	w.kvUint32(arch+".block_count", uint32(blkCount*2)) // ВИЗИТЫ (вдвое больше физических)
	w.kvUint32Array(arch+".attention.head_count", visits)
	w.kvUint32Array(arch+".attention.head_count_kv", visits)
	w.kvStringArray(arch+".attention.layer_names", []string{"a", "b"})
	w.kvUint32("general.alignment", uint32(alignment))
	for b := 0; b < blkCount; b++ {
		w.tensor(fmtBlk(b, "attn_q.weight"), 64, 64)
		w.tensor(fmtBlk(b, "ffn_down.weight"), 64, 64)
		w.tensor(fmtBlk(b, "loop_msg_query.weight"), 4, 64)
	}
	w.tensor("loop_res_query.weight", 64, 24)
	w.tensor("loop_kv_res_query.weight", 64, 24)
	// Выравниваем данные до alignment, как это делает gguf_write.
	for uint64(len(w.buf))%alignment != 0 {
		w.buf = append(w.buf, 0)
	}
	w.buf = append(w.buf, make([]byte, 128)...) // немного «данных»
	if err := os.WriteFile(path, w.buf, 0o644); err != nil {
		t.Fatalf("запись тестового GGUF: %v", err)
	}
}

func fmtBlk(n int, suffix string) string {
	return "blk." + itoaTest(n) + "." + suffix
}

func itoaTest(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

func TestReadGGUFHeaderFacts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "loop-export.gguf")
	writeTestGGUF(t, path, 32, 24, "inkling2")

	facts := inspectGGUF(path, "loop-export.gguf")
	if facts.ReadError != "" {
		t.Fatalf("шапка не прочитана: %s", facts.ReadError)
	}
	if facts.Arch != "inkling2" {
		t.Fatalf("arch=%q", facts.Arch)
	}
	if facts.BlockCount != 48 {
		t.Fatalf("block_count=%d, ожидалось 48 (визиты)", facts.BlockCount)
	}
	if facts.PhysicalBlocks != 24 {
		t.Fatalf("physical_blocks=%d, ожидалось 24", facts.PhysicalBlocks)
	}
	if !facts.QueryMatrices {
		t.Fatal("глобальные матрицы запросов не найдены")
	}
	if facts.MsgQueryTensors != 24 {
		t.Fatalf("loop_msg_query=%d, ожидалось 24", facts.MsgQueryTensors)
	}
	if facts.Scheme != "inkling2_loop_export" {
		// Архитектура в файле ещё inkling2: метаданные обязаны быть приведены к
		// схеме загрузчика, даже если матрицы запросов уже разложены.
		t.Fatalf("scheme=%q, ожидалось inkling2_loop_export", facts.Scheme)
	}
	if !facts.Patchable {
		t.Fatal("loop-экспорт с arch=inkling2 обязан считаться патчабельным")
	}
	if facts.NeedsFuse {
		t.Fatal("loop_msg_query уже разложены — fuse не нужен")
	}
}

func TestInspectDetectsLoopExportNeedingPatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "loop-needs-patch.gguf")
	w := &ggufValueWriter{}
	w.buf = append(w.buf, []byte(ggufMagic)...)
	w.u32(3)
	w.u64(3) // 2 обычных тензора + 1 матрица запросов
	w.u64(2) // arch + block_count
	w.kvString("general.architecture", "inkling2")
	w.kvUint32("inkling2.block_count", 48)
	for b := 0; b < 24; b++ {
		w.tensor(fmtBlk(b, "attn_q.weight"), 64, 64)
	}
	// Только одна из двух матриц: условие «есть обе» не выполнено — это тоже
	// проверяется, чтобы UI не обещал fuse без данных.
	w.tensor("loop_res_query.weight", 64, 24)
	for uint64(len(w.buf))%32 != 0 {
		w.buf = append(w.buf, 0)
	}
	w.buf = append(w.buf, make([]byte, 64)...)
	if err := os.WriteFile(path, w.buf, 0o644); err != nil {
		t.Fatalf("запись: %v", err)
	}
	facts := inspectGGUF(path, "x.gguf")
	if facts.Scheme != "inkling2_loop_export" {
		t.Fatalf("scheme=%q, ожидалось inkling2_loop_export", facts.Scheme)
	}
	if !facts.Patchable {
		t.Fatal("loop-экспорт без разложенных msg_query обязан считаться патчабельным")
	}
	if facts.QueryMatrices {
		t.Fatal("одна матрица запросов — это не «обе»: fuse не обещаем")
	}
}

func TestReadGGUFHeaderCustomAlignment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "aligned.gguf")
	writeTestGGUF(t, path, 64, 2, "llama")
	hdr, err := readGGUFHeader(path)
	if err != nil {
		t.Fatalf("чтение: %v", err)
	}
	if hdr.Alignment != 64 {
		t.Fatalf("alignment=%d, ожидалось 64", hdr.Alignment)
	}
	if hdr.HeaderBytes%64 != 0 {
		t.Fatalf("data_start=%d не выровнен по 64", hdr.HeaderBytes)
	}
	if hdr.Version != 3 {
		t.Fatalf("version=%d", hdr.Version)
	}
}

func TestReadGGUFHeaderRejectsForeignFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "not-a-model.gguf")
	if err := os.WriteFile(path, []byte("PK\x03\x04 это zip"), 0o644); err != nil {
		t.Fatalf("запись: %v", err)
	}
	if _, err := readGGUFHeader(path); err == nil {
		t.Fatal("файл без сигнатуры GGUF прочитан как модель")
	}
	facts := inspectGGUF(path, "not-a-model.gguf")
	if facts.ReadError == "" {
		t.Fatal("битый файл должен попадать в список с read_error, а не исчезать")
	}
	if facts.Scheme != "unreadable" {
		t.Fatalf("scheme=%q, ожидалось unreadable", facts.Scheme)
	}
}

func TestScanModelsDepthAndExtension(t *testing.T) {
	root := t.TempDir()
	mustWrite := func(rel string) {
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(full, []byte("GGUF"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	mustWrite("a.gguf")
	mustWrite(filepath.Join("image", "b.gguf"))
	mustWrite(filepath.Join("deep", "deeper", "too-deep.gguf"))
	mustWrite("notes.txt")
	mustWrite(filepath.Join(".hidden", "c.gguf"))

	got := scanModels(root, 2, 100)
	var rels []string
	for _, p := range got {
		r, _ := filepath.Rel(root, p)
		rels = append(rels, filepath.ToSlash(r))
	}
	if len(rels) != 2 {
		t.Fatalf("ожидалось 2 файла (a.gguf, image/b.gguf), получено %v", rels)
	}
	if rels[0] != "a.gguf" || rels[1] != "image/b.gguf" {
		t.Fatalf("неожиданный набор: %v", rels)
	}
}

func TestListModelsCachesAndInvalidates(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "m.gguf")
	writeTestGGUF(t, path, 32, 2, "llama")
	cache := newModelsCache(0)
	first := listModels(root, cache, 2, 100)
	if len(first) != 1 {
		t.Fatalf("моделей: %d", len(first))
	}
	second := listModels(root, cache, 2, 100)
	if len(second) != 1 || second[0] != first[0] {
		t.Fatal("кэш не отдал те же факты при неизменном файле")
	}
	// Файл переписан (другой размер) → кэш обязан перечитать.
	writeTestGGUF(t, path, 32, 3, "llama")
	third := listModels(root, cache, 2, 100)
	if len(third) != 1 {
		t.Fatalf("моделей: %d", len(third))
	}
	if third[0].SizeBytes == first[0].SizeBytes {
		t.Fatal("кэш не заметил изменения файла (size не обновился)")
	}
}

func TestPathWithin(t *testing.T) {
	root := filepath.Clean("/models")
	cases := []struct {
		path string
		want bool
	}{
		{filepath.Join(root, "a.gguf"), true},
		{root, true},
		{filepath.Join(root, "sub", "b.gguf"), true},
		{filepath.Join(root, "..", "etc", "passwd"), false},
		{filepath.Clean("/etc/passwd"), false},
	}
	for _, c := range cases {
		if got := pathWithin(c.path, root); got != c.want {
			t.Fatalf("pathWithin(%q, %q) = %v, ожидалось %v", c.path, root, got, c.want)
		}
	}
}

func TestFloat32ValueReadable(t *testing.T) {
	// F32-значение в шапке должно читаться, а не сдвигать разбор: этим типом
	// пишутся rope_scale и подобные ключи.
	dir := t.TempDir()
	path := filepath.Join(dir, "f32.gguf")
	w := &ggufValueWriter{}
	w.buf = append(w.buf, []byte(ggufMagic)...)
	w.u32(3)
	w.u64(0) // тензоров нет
	w.u64(2) // два KV
	w.kvString("general.architecture", "llama")
	w.str("llama.rope.freq_base")
	w.u32(uint32(ggufTfloat32))
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], math.Float32bits(10000.0))
	w.buf = append(w.buf, b[:]...)
	for uint64(len(w.buf))%32 != 0 {
		w.buf = append(w.buf, 0)
	}
	if err := os.WriteFile(path, w.buf, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	hdr, err := readGGUFHeader(path)
	if err != nil {
		t.Fatalf("чтение: %v", err)
	}
	v, ok := hdr.KV["llama.rope.freq_base"]
	if !ok {
		t.Fatal("F32-ключ потерян")
	}
	if math.Abs(v.F-10000.0) > 1e-6 {
		t.Fatalf("F32-значение=%v, ожидалось 10000", v.F)
	}
}

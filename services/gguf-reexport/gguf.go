// gguf.go — чтение заголовка GGUF на Go.
//
// ЗАЧЕМ СВОЙ ПАРСЕР, ЕСЛИ КОНВЕРТЕР ВСЁ РАВНО PYTHON. Вопрос «какая у файла схема
// и нужен ли ему патч» задаётся чаще, чем выполняется сам ре-экспорт: он нужен
// списку моделей (форма создания задачи) и `capabilities`. Гонять ради этого
// python-конвертер на каждый GET — это импорт модуля и mmap 16 ГиБ файла на
// запрос; Go читает только шапку потоком (десятки мегабайт максимум с словарём
// токенизатора) и не поднимает конвертер вообще. Веса остаются нетронутыми.
//
// ЧТО ЧИТАЕТСЯ: версия, счётчики, KV-пары (маленькие значения — целиком, большие
// массивы — пропуском без материализации) и таблица тензоров. Данные тензоров не
// читаются никогда: сервису нужны имена, размерности и смещения.
//
// ГРАНИЦА ОТВЕТСТВЕННОСТИ. Здесь нет валидации схемы модели: «патч возможен или
// нет» решает тот, кто патчит (`py/reexport_job.py` → scripts/gguf_loop_meta_patch.py,
// у него уже есть make_plan). Go-сторона даёт ФАКТЫ (архитектура, block_count,
// число физических блоков, наличие loop-ключей и матриц запросов) и предварительный
// вердикт для UI; расхождение с конвертером видно как ошибка задачи, а не как
// «форма разрешила, а сервис отказал» без объяснения.
package main

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	ggufMagic = "GGUF"

	// ggufDefaultAlignment — выравнивание данных по умолчанию (gguf.h).
	ggufDefaultAlignment = 32

	// ggufMaxHeaderBytes — предохранитель: шапка реальных моделей это единицы
	// мегабайт (словарь токенизатора), десятки — уже аномалия. Ограничение
	// защищает от «файл не тот» и от счётчика массива из мусора.
	ggufMaxHeaderBytes = 512 << 20

	// ggufMaxArrayElems — массивы длиннее не материализуем: нужные нам ключи
	// (block_count, head_count, loop.*) короткие, а tokenizer.ggml.tokens — сотни
	// тысяч строк, которые сервису не нужны вовсе.
	ggufMaxArrayElems = 1 << 20

	// ggufMaxTensors — таблица тензоров больших MoE-моделей доходит до тысяч.
	ggufMaxTensors = 200000
)

// ggufValueType — типы значений GGUF (gguf.h, enum gguf_type).
type ggufValueType uint32

const (
	ggufTuint8   ggufValueType = 0
	ggufTint8    ggufValueType = 1
	ggufTuint16  ggufValueType = 2
	ggufTint16   ggufValueType = 3
	ggufTuint32  ggufValueType = 4
	ggufTint32   ggufValueType = 5
	ggufTfloat32 ggufValueType = 6
	ggufTbool    ggufValueType = 7
	ggufTstring  ggufValueType = 8
	ggufTarray   ggufValueType = 9
	ggufTuint64  ggufValueType = 10
	ggufTint64   ggufValueType = 11
	ggufTfloat64 ggufValueType = 12
)

// ggufValue — значение KV-пары. Хранится ровно столько, сколько нужно для
// решений: скаляры и короткие массивы; длинные массивы остаются «непрочитанными»
// (Known=false), чтобы не держать в памяти словарь токенизатора.
type ggufValue struct {
	Type  ggufValueType
	U     uint64
	I     int64
	F     float64
	S     string
	Arr   []ggufValue
	Known bool
}

// asFacts — числа, которые можно сравнить без приведения типов на месте вызова.
func (v ggufValue) asUint64() (uint64, bool) {
	switch v.Type {
	case ggufTuint8, ggufTuint16, ggufTuint32, ggufTuint64, ggufTbool:
		return v.U, true
	case ggufTint8, ggufTint16, ggufTint32, ggufTint64:
		if v.I < 0 {
			return 0, false
		}
		return uint64(v.I), true
	}
	return 0, false
}

// ggufTensor — запись таблицы тензоров.
type ggufTensor struct {
	Name   string
	Dims   []uint64
	Type   ggufValueType // ggml_type (F32=0, F16=1, кванты — 2..)
	Offset uint64
}

// ggufHeader — разобранная шапка.
type ggufHeader struct {
	Version     uint32
	TensorCount uint64
	KVCount     uint64
	KV          map[string]ggufValue
	Tensors     []ggufTensor
	Alignment   uint64
	// HeaderBytes — сколько заняла шапка с выравниванием (== DataStart).
	HeaderBytes int64
	FileSize    int64
}

// ggufReadError — ошибка разбора с человеческой причиной (попадает в UI).
type ggufReadError struct{ msg string }

func (e *ggufReadError) Error() string { return e.msg }

func ggufErrf(format string, args ...interface{}) error {
	return &ggufReadError{msg: fmt.Sprintf(format, args...)}
}

// ggufReader — точный счётчик прочитанного: bufio предвыбирает данные, поэтому
// позицию в файле считаем сами, по логическим чтениям, а не по счётчику снизу.
type ggufReader struct {
	br  *bufio.Reader
	pos int64
	buf [8]byte
}

func (r *ggufReader) readFull(n int) ([]byte, error) {
	if r.pos+int64(n) > ggufMaxHeaderBytes {
		return nil, ggufErrf("заголовок больше %d МиБ — это не похоже на GGUF-модель", ggufMaxHeaderBytes>>20)
	}
	if _, err := io.ReadFull(r.br, r.buf[:n]); err != nil {
		return nil, ggufErrf("файл оборван на %d байте (заголовок неполный)", r.pos)
	}
	r.pos += int64(n)
	return r.buf[:n], nil
}

func (r *ggufReader) u32() (uint32, error) {
	b, err := r.readFull(4)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(b), nil
}

func (r *ggufReader) u64() (uint64, error) {
	b, err := r.readFull(8)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint64(b), nil
}

// str читает строку GGUF: uint64 длина + байты UTF-8. keep=false — пропустить,
// не выделяя память (имена тензоров нужны все, значения словаря — нет).
func (r *ggufReader) str(keep bool) (string, error) {
	n, err := r.u64()
	if err != nil {
		return "", err
	}
	if n > uint64(ggufMaxHeaderBytes) {
		return "", ggufErrf("строка длиной %d Б не бывает в заголовке GGUF", n)
	}
	if !keep {
		if err := r.discard(int64(n)); err != nil {
			return "", err
		}
		return "", nil
	}
	if n == 0 {
		return "", nil
	}
	buf := make([]byte, n)
	if r.pos+int64(n) > ggufMaxHeaderBytes {
		return "", ggufErrf("заголовок больше %d МиБ", ggufMaxHeaderBytes>>20)
	}
	if _, err := io.ReadFull(r.br, buf); err != nil {
		return "", ggufErrf("файл оборван посередине строки (смещение %d)", r.pos)
	}
	r.pos += int64(n)
	return string(buf), nil
}

func (r *ggufReader) discard(n int64) error {
	if n < 0 {
		return ggufErrf("отрицательная длина пропуска")
	}
	if r.pos+n > ggufMaxHeaderBytes {
		return ggufErrf("заголовок больше %d МиБ", ggufMaxHeaderBytes>>20)
	}
	if n == 0 {
		return nil
	}
	skipped, err := r.br.Discard(int(n))
	r.pos += int64(skipped)
	if err != nil {
		return ggufErrf("файл оборван (не хватает %d Б из %d)", n-int64(skipped), n)
	}
	return nil
}

// value читает значение типа vt. materialize=false — пропустить, не собирая
// массивы (нужно для tokenizer.ggml.*, где сотни тысяч строк).
func (r *ggufReader) value(vt ggufValueType, materialize bool) (ggufValue, error) {
	v := ggufValue{Type: vt, Known: materialize}
	switch vt {
	case ggufTuint8, ggufTbool:
		b, err := r.readFull(1)
		if err != nil {
			return v, err
		}
		v.U = uint64(b[0])
	case ggufTint8:
		b, err := r.readFull(1)
		if err != nil {
			return v, err
		}
		v.I = int64(int8(b[0]))
	case ggufTuint16:
		b, err := r.readFull(2)
		if err != nil {
			return v, err
		}
		v.U = uint64(binary.LittleEndian.Uint16(b))
	case ggufTint16:
		b, err := r.readFull(2)
		if err != nil {
			return v, err
		}
		v.I = int64(int16(binary.LittleEndian.Uint16(b)))
	case ggufTuint32:
		n, err := r.u32()
		if err != nil {
			return v, err
		}
		v.U = uint64(n)
	case ggufTint32:
		n, err := r.u32()
		if err != nil {
			return v, err
		}
		v.I = int64(int32(n))
	case ggufTfloat32:
		n, err := r.u32()
		if err != nil {
			return v, err
		}
		v.F = float64(math.Float32frombits(n))
	case ggufTuint64:
		n, err := r.u64()
		if err != nil {
			return v, err
		}
		v.U = n
	case ggufTint64:
		n, err := r.u64()
		if err != nil {
			return v, err
		}
		v.I = int64(n)
	case ggufTfloat64:
		n, err := r.u64()
		if err != nil {
			return v, err
		}
		v.F = math.Float64frombits(n)
	case ggufTstring:
		s, err := r.str(materialize)
		if err != nil {
			return v, err
		}
		v.S = s
	case ggufTarray:
		elemType, err := r.u32()
		if err != nil {
			return v, err
		}
		count, err := r.u64()
		if err != nil {
			return v, err
		}
		keep := materialize && count <= ggufMaxArrayElems
		if keep {
			v.Arr = make([]ggufValue, 0, int(count))
		}
		for i := uint64(0); i < count; i++ {
			elem, err := r.value(ggufValueType(elemType), keep)
			if err != nil {
				return v, err
			}
			if keep {
				v.Arr = append(v.Arr, elem)
			}
		}
		v.Known = keep
		return v, nil
	default:
		return v, ggufErrf("неизвестный тип значения GGUF: %d", vt)
	}
	return v, nil
}

// readGGUFHeader читает шапку файла. Файл открывается только на чтение: сервис
// никогда не пишет в исходную модель (патч всегда пишет новый файл — так требует
// конвертер, у него смещения тензоров меняются вместе с шапкой).
func readGGUFHeader(path string) (*ggufHeader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if st.IsDir() {
		return nil, ggufErrf("%s — каталог, а не файл модели", filepath.Base(path))
	}
	r := &ggufReader{br: bufio.NewReaderSize(f, 1<<20)}
	magic, err := r.readFull(4)
	if err != nil {
		return nil, err
	}
	if string(magic) != ggufMagic {
		return nil, ggufErrf("нет сигнатуры GGUF (первые байты %q)", string(magic))
	}
	hdr := &ggufHeader{KV: map[string]ggufValue{}, FileSize: st.Size()}
	if hdr.Version, err = r.u32(); err != nil {
		return nil, err
	}
	if hdr.TensorCount, err = r.u64(); err != nil {
		return nil, err
	}
	if hdr.KVCount, err = r.u64(); err != nil {
		return nil, err
	}
	if hdr.TensorCount > ggufMaxTensors {
		return nil, ggufErrf("тензоров %d — больше предела %d", hdr.TensorCount, ggufMaxTensors)
	}
	for i := uint64(0); i < hdr.KVCount; i++ {
		key, err := r.str(true)
		if err != nil {
			return nil, err
		}
		vt, err := r.u32()
		if err != nil {
			return nil, err
		}
		val, err := r.value(ggufValueType(vt), true)
		if err != nil {
			return nil, err
		}
		hdr.KV[key] = val
	}
	for i := uint64(0); i < hdr.TensorCount; i++ {
		name, err := r.str(true)
		if err != nil {
			return nil, err
		}
		nDims, err := r.u32()
		if err != nil {
			return nil, err
		}
		if nDims > 8 {
			return nil, ggufErrf("у тензора %s измерений %d — так не бывает", name, nDims)
		}
		t := ggufTensor{Name: name, Dims: make([]uint64, nDims)}
		for d := uint32(0); d < nDims; d++ {
			if t.Dims[d], err = r.u64(); err != nil {
				return nil, err
			}
		}
		tt, err := r.u32()
		if err != nil {
			return nil, err
		}
		t.Type = ggufValueType(tt)
		if t.Offset, err = r.u64(); err != nil {
			return nil, err
		}
		hdr.Tensors = append(hdr.Tensors, t)
	}
	hdr.Alignment = ggufDefaultAlignment
	if v, ok := hdr.KV["general.alignment"]; ok && v.U >= 1 && v.U <= 1<<16 {
		hdr.Alignment = v.U
	}
	hdr.HeaderBytes = alignUp(r.pos, int64(hdr.Alignment))
	return hdr, nil
}

func alignUp(n, alignment int64) int64 {
	if alignment <= 1 {
		return n
	}
	rem := n % alignment
	if rem == 0 {
		return n
	}
	return n + alignment - rem
}

// ---- факты о модели ---------------------------------------------------------

// modelFacts — то, что сервис знает о модели БЕЗ запуска конвертера.
type modelFacts struct {
	RelPath     string `json:"path"`
	Name        string `json:"name"`
	SizeBytes   int64  `json:"size_bytes"`
	GGUFVersion uint32 `json:"gguf_version"`
	Arch        string `json:"arch"`
	Tensors     int    `json:"tensors"`
	KVCount     int    `json:"kv_count"`
	// BlockCount — <arch>.block_count из метаданных; PhysicalBlocks — число
	// блоков, реально описанных тензорами blk.N. Разное значение — это loop-схема
	// (block_count считает ВИЗИТЫ, физических слоёв меньше).
	BlockCount     int  `json:"block_count"`
	PhysicalBlocks int  `json:"physical_blocks"`
	NumLoops       int  `json:"num_loops,omitempty"`
	LoopStart      int  `json:"loop_start,omitempty"`
	LoopEnd        int  `json:"loop_end,omitempty"`
	LoopCount      int  `json:"loop_count,omitempty"`
	HasLoopKeys    bool `json:"has_loop_keys"`
	// QueryMatrices — глобальные loop_res_query/loop_kv_res_query (их носит
	// loop-экспорт); MsgQueryTensors — уже разложенные по блокам loop_msg_query.
	QueryMatrices   bool `json:"query_matrices"`
	MsgQueryTensors int  `json:"msg_query_tensors"`
	// CoreBlocks — сколько блоков ядра получит матрицу запросов при fuse_loop_queries.
	CoreBlocks int `json:"core_blocks"`
	// NeedsFuse — патчу потребуется разложить глобальные матрицы запросов по блокам.
	// False, если loop_msg_query уже разложены (тогда fuse не нужен, а метаданные
	// всё равно надо привести к схеме загрузчика).
	NeedsFuse bool `json:"needs_fuse"`
	// Scheme — вердикт для UI: "inkling2_loop" (уже в схеме загрузчика),
	// "inkling2_loop_export" (loop-экспорт, нужен патч), "plain" (прочее).
	Scheme string `json:"scheme"`
	// Patchable — предварительный вердикт: конвертер сможет применить патч.
	// Окончательное решение всё равно за make_plan в Python (он знает больше).
	Patchable bool     `json:"patchable"`
	Notes     []string `json:"notes,omitempty"`
	ReadError string   `json:"read_error,omitempty"`
}

func (h *ggufHeader) arch() string {
	if v, ok := h.KV["general.architecture"]; ok {
		return v.S
	}
	return ""
}

// archUint — <arch>.<suffix> как целое.
func (h *ggufHeader) archUint(suffix string) (uint64, bool) {
	v, ok := h.KV[h.arch()+"."+suffix]
	if !ok {
		return 0, false
	}
	return v.asUint64()
}

// physicalBlocks — старший blk.N + 1 по таблице тензоров.
func (h *ggufHeader) physicalBlocks() int {
	max := -1
	for _, t := range h.Tensors {
		if !strings.HasPrefix(t.Name, "blk.") {
			continue
		}
		rest := t.Name[len("blk."):]
		dot := strings.IndexByte(rest, '.')
		if dot <= 0 {
			continue
		}
		n := 0
		ok := true
		for i := 0; i < dot; i++ {
			c := rest[i]
			if c < '0' || c > '9' {
				ok = false
				break
			}
			n = n*10 + int(c-'0')
		}
		if ok && n > max {
			max = n
		}
	}
	return max + 1
}

func (h *ggufHeader) countTensors(pred func(name string) bool) int {
	n := 0
	for _, t := range h.Tensors {
		if pred(t.Name) {
			n++
		}
	}
	return n
}

func (h *ggufHeader) hasTensor(name string) bool {
	for _, t := range h.Tensors {
		if t.Name == name {
			return true
		}
	}
	return false
}

// inspectGGUF читает шапку и вычисляет факты. Ошибка чтения не роняет список
// моделей: битый файл тоже надо показать (иначе «модель есть, а её нет в UI»).
func inspectGGUF(path, rel string) *modelFacts {
	facts := &modelFacts{RelPath: rel, Name: filepath.Base(rel)}
	if st, err := os.Stat(path); err == nil {
		facts.SizeBytes = st.Size()
	}
	hdr, err := readGGUFHeader(path)
	if err != nil {
		facts.ReadError = err.Error()
		facts.Scheme = "unreadable"
		return facts
	}
	facts.GGUFVersion = hdr.Version
	facts.Arch = hdr.arch()
	facts.Tensors = len(hdr.Tensors)
	facts.KVCount = len(hdr.KV)
	facts.PhysicalBlocks = hdr.physicalBlocks()
	if n, ok := hdr.archUint("block_count"); ok {
		facts.BlockCount = int(n)
	}
	if n, ok := hdr.archUint("num_loops"); ok {
		facts.NumLoops = int(n)
	}
	if n, ok := hdr.archUint("loop.core_start"); ok {
		facts.LoopStart = int(n)
	}
	if n, ok := hdr.archUint("loop.core_end"); ok {
		facts.LoopEnd = int(n)
	}
	if n, ok := hdr.archUint("loop.count"); ok {
		facts.LoopCount = int(n)
	}
	for _, key := range []string{"loop.start", "loop.end", "loop.count", "loop.gate_rank", "loop.varying"} {
		if _, ok := hdr.KV[hdr.arch()+"."+key]; ok {
			facts.HasLoopKeys = true
			break
		}
	}
	facts.QueryMatrices = hdr.hasTensor("loop_res_query.weight") && hdr.hasTensor("loop_kv_res_query.weight")
	facts.MsgQueryTensors = hdr.countTensors(func(n string) bool {
		return strings.HasSuffix(n, ".loop_msg_query.weight")
	})
	if facts.LoopEnd > facts.LoopStart {
		facts.CoreBlocks = facts.LoopEnd - facts.LoopStart
	}
	switch {
	case facts.Arch == "inkling2_loop":
		facts.Scheme = "inkling2_loop"
	case facts.Arch == "inkling2" && facts.BlockCount != 0 && facts.PhysicalBlocks != 0 &&
		facts.BlockCount != facts.PhysicalBlocks:
		// block_count считает ВИЗИТЫ, физических блоков меньше — это loop-экспорт.
		// Патч нужен ВСЕГДА (архитектура и длины массивов), а fuse — только если
		// матрицы запросов ещё не разложены по блокам.
		facts.Scheme = "inkling2_loop_export"
		facts.Patchable = true
		facts.Notes = append(facts.Notes, fmt.Sprintf(
			"block_count=%d при %d физических блоках — это loop-экспорт (визиты, не слои)",
			facts.BlockCount, facts.PhysicalBlocks))
		switch {
		case facts.MsgQueryTensors > 0:
			facts.Notes = append(facts.Notes,
				"loop_msg_query уже разложены по блокам — fuse не требуется")
		case facts.QueryMatrices:
			facts.NeedsFuse = true
			facts.Notes = append(facts.Notes,
				"есть глобальные loop_res_query/loop_kv_res_query — опция fuse_loop_queries разложит их по блокам ядра")
		default:
			facts.Notes = append(facts.Notes,
				"нет ни loop_msg_query, ни матриц запросов: патч метаданных не сделает файл загружаемым")
		}
	default:
		facts.Scheme = "plain"
	}
	if facts.Arch == "" {
		facts.Notes = append(facts.Notes, "в метаданных нет general.architecture")
	}
	return facts
}

// ---- перечисление моделей ---------------------------------------------------

// modelsCache — короткоживущий кэш фактов: форма задачи и поллинг статуса
// дергают список часто, а чтение шапок десятка файлов — это десятки мегабайт.
type modelsCache struct {
	mu     sync.Mutex
	items  map[string]cachedFacts
	loaded time.Time
	ttl    time.Duration
}

type cachedFacts struct {
	key   string // size+mtime: файл переписан — факты перечитываются
	facts *modelFacts
}

func newModelsCache(ttl time.Duration) *modelsCache {
	return &modelsCache{items: map[string]cachedFacts{}, ttl: ttl}
}

// scanModels обходит каталог моделей (с ограничением глубины и количества).
func scanModels(root string, maxDepth, maxFiles int) []string {
	var out []string
	rootDepth := strings.Count(filepath.Clean(root), string(os.PathSeparator))
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // недоступный файл/каталог не должен ломать список
		}
		if len(out) >= maxFiles {
			return filepath.SkipAll
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && (strings.HasPrefix(name, ".") || name == "node_modules") {
				return filepath.SkipDir
			}
			if strings.Count(filepath.Clean(path), string(os.PathSeparator))-rootDepth >= maxDepth {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.EqualFold(filepath.Ext(d.Name()), ".gguf") {
			out = append(out, path)
		}
		return nil
	})
	sort.Strings(out)
	return out
}

// listModels отдаёт факты по всем GGUF в каталоге моделей, кэшируя по size+mtime.
func listModels(root string, cache *modelsCache, maxDepth, maxFiles int) []*modelFacts {
	paths := scanModels(root, maxDepth, maxFiles)
	out := make([]*modelFacts, 0, len(paths))
	cache.mu.Lock()
	defer cache.mu.Unlock()
	seen := make(map[string]bool, len(paths))
	for _, p := range paths {
		rel, err := filepath.Rel(root, p)
		if err != nil {
			rel = p
		}
		rel = filepath.ToSlash(rel)
		seen[rel] = true
		st, err := os.Stat(p)
		key := ""
		if err == nil {
			key = fmt.Sprintf("%d@%d", st.Size(), st.ModTime().UnixNano())
		}
		if c, ok := cache.items[rel]; ok && c.key == key && key != "" {
			out = append(out, c.facts)
			continue
		}
		facts := inspectGGUF(p, rel)
		cache.items[rel] = cachedFacts{key: key, facts: facts}
		out = append(out, facts)
	}
	for rel := range cache.items {
		if !seen[rel] {
			delete(cache.items, rel)
		}
	}
	cache.loaded = time.Now()
	return out
}

// errNotModelFile — источник не файл (например каталог HF-чекпойнта для
// полного ре-экспорта): вызывающий сам решает, допустимо ли это для цели.
var errNotModelFile = errors.New("не файл GGUF")

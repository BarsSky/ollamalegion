package cppbackend

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"strings"
)

// GGUFHeaderInfo — метаданные, извлекаемые из GGUF header ДО загрузки модели.
//
// Round 37 (2026-08-18): добавлено ContextLength — GGUF training context.
// Используется feasible.go для ComputeFeasible и balancer для 3-tier
// resolveModelMaxContext: profile.contextLength <= GGUFContextLength <= feasibleMax.
type GGUFHeaderInfo struct {
	Architecture string `json:"architecture"`
	NLayers      int    `json:"nLayers"`
	NHeads       int    `json:"nHeads"`
	NKvHeads     int    `json:"nKvHeads"`
	HeadDimK     int    `json:"headDimK"`
	HeadDimV     int    `json:"headDimV"`
	KVCacheType  string `json:"kvCacheType"`
	NEmbd        int    `json:"nEmbd"`
	// ContextLength — max training context из GGUF metadata (*.context_length).
	// Пример: Qwen3.6-35B-A3B-UD-Q4_K_M → 262144 (262K токенов).
	// 0 = unknown (файл не GGUF или архитектура не из ggufModelArchs).
	ContextLength int   `json:"contextLength"`
	FileSize      int64 `json:"fileSize"`

	// R83 (2026-09-25): параметры, от которых зависит РЕАЛЬНЫЙ размер KV-кэша.
	// Без них KV считался как «2 × block_count × kv_heads × (n_embd/n_heads)», что
	// для гибридных моделей (qwen35/qwen3next) завышает его в разы: у
	// Qwen3.8-27B KV хранят 17 слоёв из 65, а head_dim = 256, а не 213.
	// Правило и обоснование — kv_layers.go.
	KeyLength             int  `json:"keyLength,omitempty"`             // *.attention.key_length
	ValueLength           int  `json:"valueLength,omitempty"`           // *.attention.value_length
	NextNPredictLayers    int  `json:"nextnPredictLayers,omitempty"`    // *.nextn_predict_layers (MTP)
	FullAttentionInterval int  `json:"fullAttentionInterval,omitempty"` // *.full_attention_interval
	HasRecurrentLayersKey bool `json:"hasRecurrentLayersKey,omitempty"` // в файле есть явный список рекуррентных слоёв

	// R83 (2026-10-01): KV-sharing и скользящее окно (gemma4/gemma3n).
	//
	// У gemma-4-E4B кэш держат 24 слоя из 42 (shared_kv_layers=18), из них
	// глобальных всего 4 — остальные живут в окне 512 токенов. Оценка «все слои
	// × n_ctx» завышала KV в 4-6 раз: memfit отказывал в полном оффлоаде и
	// оставлял 7 слоёв на CPU (замер стенда: 5-9 tok/s против 35/42 слоёв).
	// Правило раскладки — kv_layers.go (KVPlan).
	SharedKVLayers int    `json:"sharedKvLayers,omitempty"` // *.attention.shared_kv_layers
	SlidingWindow  int    `json:"slidingWindow,omitempty"`  // *.attention.sliding_window
	SWAKeyLength   int    `json:"swaKeyLength,omitempty"`   // *.attention.key_length_swa
	SWAPattern     string `json:"swaPattern,omitempty"`     // *.attention.sliding_window_pattern: '1'=окно, '0'=глобальный
}

// ggufFieldSuffixes — суффиксы ключей метаданных для параметров модели.
// Каждый параметр имеет вид <architecture>.<suffix>.
// Используется для генерации всех возможных ключей из ggufModelArchs.
//
// Round 37 (2026-08-18): added .context_length — это GGUF max training context.
// КРИТИЧНО для auto-adapt n_ctx: профиль может быть консервативным (32K),
// а GGUF max = 262K. Без него balancer не знает, что модель может больше.
var ggufFieldSuffixes = []string{
	".block_count",
	".attention.head_count",
	".attention.head_count_kv",
	".embedding_length",
	".context_length",
	// R83 (2026-09-25): реальный размер KV-кэша. См. kv_layers.go.
	".attention.key_length",
	".attention.value_length",
	".nextn_predict_layers",
	".full_attention_interval",
	// R83 (2026-10-01): раскладка KV моделей с KV-sharing и скользящим окном
	// (gemma4/gemma3n): сколько слоёв переиспользуют чужой кэш и какие слои
	// живут в окне. См. KVPlan в kv_layers.go.
	".attention.shared_kv_layers",
	".attention.sliding_window",
	".attention.key_length_swa",
}

// ggufModelArchs — известные архитектуры GGUF.
// Каждая архитектура может иметь префикс для ключей метаданных.
var ggufModelArchs = []string{
	"llama", "qwen2", "qwen3", "qwen3moe", "qwen3next", "qwen35", "qwen35moe",
	"gemma2", "gemma4", "gemma3",
	"starcoder2", "gpt_bigcode", "falcon", "mpt", "phi3", "bert", "nemotron",
}

// initGGUFKeyMap инициализирует реверсивный маппинг "gguf key → field index"
// один раз при старте. Это позволяет O(1) определение поля по ключу.
func initGGUFKeyMap() map[string]int {
	m := make(map[string]int)
	for _, arch := range ggufModelArchs {
		for fi, suffix := range ggufFieldSuffixes {
			key := arch + suffix
			m[key] = fi
		}
	}
	return m
}

// ggufKeyMap — глобальный map "gguf key → field index"
// Field index: 0=NLayers, 1=NHeads, 2=NKvHeads, 3=NEmbd
var ggufKeyMap = initGGUFKeyMap()

// ReadGGUFHeader — публичная обёртка над readGGUFHeaderInfo для использования
// из cmd/cppworker и других пакетов.
//
// Используется ensureModelLoaded для lazy-load архитектурных параметров модели
// (NLayers/NEmbd/NHeads/NKvHeads) ДО llama.cpp.LoadModel, чтобы корректно
// рассчитать auto-tuned n_ctx/gpu_layers в calculateLazyLoadOpts.
//
// Не падает на ошибке — caller сам решает fallback. Возвращает *GGUFHeaderInfo
// с заполненными полями если header валидный.
func ReadGGUFHeader(path string) (*GGUFHeaderInfo, error) {
	return readGGUFHeaderInfo(path)
}

// readGGUFHeaderInfo читает заголовок GGUF файла и извлекает ключевые метаданные.
// Работает без загрузки модели в llama.cpp — читает только header (metadata KV).
//
// Формат GGUF v3:
//
//	[4]byte magic = "GGUF"
//	uint32 version = 3
//	uint64 tensorCount
//	uint64 metadataKvCount
//	[]MetadataKV — пары ключ-значение с архитектурой, параметрами и т.д.
//
// Поддерживаемые GGUF metadata keys (с маппингом по архитектуре):
//   - general.architecture              (string)
//   - <arch>.block_count                (uint32) — NLayers
//   - <arch>.attention.head_count       (uint32) — NHeads
//   - <arch>.attention.head_count_kv    (uint32) — NKvHeads
//   - <arch>.embedding_length           (uint32) — NEmbd
//
// После чтения general.architecture переключается на соответствующий префикс.
func readGGUFHeaderInfo(path string) (*GGUFHeaderInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open gguf: %w", err)
	}
	defer f.Close()

	// R66d (2026-09-23): читаем GGUF-заголовок через буфер. Разбор делает тысячи
	// мелких чтений (в gemma-4 только tokenizer.ggml.tokens — 262144 строки), и на
	// bind-mount Docker Desktop (Windows → WSL2) каждое чтение/seek идёт через
	// границу ФС: поштучный пропуск строк растягивал загрузку модели на минуты,
	// goroutine застревала в readGGUFHeaderInfo, операция load вечно висела в
	// статусе running, GPU простаивала. Теперь чтения идут через bufio, а
	// пропуски — через io.CopyN(io.Discard, ...).
	br := bufio.NewReaderSize(f, 1<<20)

	// Определяем размер файла
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat gguf: %w", err)
	}

	// Читаем magic (4 байта)
	var magic [4]byte
	if _, err := io.ReadFull(br, magic[:]); err != nil {
		return nil, fmt.Errorf("read magic: %w", err)
	}
	if magic[0] != 'G' || magic[1] != 'G' || magic[2] != 'U' || magic[3] != 'F' {
		return nil, fmt.Errorf("invalid GGUF magic: %q", string(magic[:]))
	}

	// Читаем версию (uint32)
	var version uint32
	if err := binary.Read(br, binary.LittleEndian, &version); err != nil {
		return nil, fmt.Errorf("read version: %w", err)
	}
	if version < 1 || version > 3 {
		return nil, fmt.Errorf("unsupported GGUF version: %d", version)
	}

	// Читаем tensor count (uint64)
	var tensorCount uint64
	if err := binary.Read(br, binary.LittleEndian, &tensorCount); err != nil {
		return nil, fmt.Errorf("read tensor count: %w", err)
	}
	_ = tensorCount

	// Читаем metadata KV count (uint64)
	var kvCount uint64
	if err := binary.Read(br, binary.LittleEndian, &kvCount); err != nil {
		return nil, fmt.Errorf("read metadata KV count: %w", err)
	}

	info := &GGUFHeaderInfo{
		FileSize: fi.Size(),
	}

	// Читаем KV пары
	for i := uint64(0); i < kvCount; i++ {
		// Читаем длину ключа
		var keyLen uint64
		if err := binary.Read(br, binary.LittleEndian, &keyLen); err != nil {
			return nil, fmt.Errorf("read key[%d] length: %w", i, err)
		}

		// Валидация: ключ не должен быть длиннее 8192 байт (защита от corrupted GGUF)
		if keyLen > 8192 {
			return nil, fmt.Errorf("key[%d] length %d exceeds maximum 8192 (likely corrupted GGUF)", i, keyLen)
		}

		// Читаем ключ
		keyBuf := make([]byte, keyLen)

		if _, err := io.ReadFull(br, keyBuf); err != nil {
			return nil, fmt.Errorf("read key[%d]: %w", i, err)
		}
		key := string(keyBuf)

		// Читаем тип значения (uint32)
		var valType uint32
		if err := binary.Read(br, binary.LittleEndian, &valType); err != nil {
			return nil, fmt.Errorf("read key[%q] value type: %w", key, err)
		}

		// Парсим значение в зависимости от типа
		// Типы GGUF: 0=uint8, 1=int8, 2=uint16, 3=int16, 4=uint32, 5=int32,
		//            6=float32, 7=bool, 8=string, 9=array, 10=uint64, 11=int64, 12=float64
		//
		// Нас интересуют: string(8), uint32(4), int32(5), uint64(10), float32(6), array(9)

		switch valType {
		case 8: // string
			var strLen uint64
			if err := binary.Read(br, binary.LittleEndian, &strLen); err != nil {
				return nil, fmt.Errorf("read key[%q] string length: %w", key, err)
			}
			strBuf := make([]byte, strLen)
			if _, err := io.ReadFull(br, strBuf); err != nil {
				return nil, fmt.Errorf("read key[%q] string value: %w", key, err)
			}
			strVal := string(strBuf)
			switch key {
			case "general.architecture":
				info.Architecture = strVal
			}

		case 4: // uint32
			var val uint32
			if err := binary.Read(br, binary.LittleEndian, &val); err != nil {
				return nil, fmt.Errorf("read key[%q] uint32: %w", key, err)
			}
			if fi, ok := ggufKeyMap[key]; ok {
				ggufSetField(info, fi, int(val))
			}

		case 10: // uint64
			var val uint64
			if err := binary.Read(br, binary.LittleEndian, &val); err != nil {
				return nil, fmt.Errorf("read key[%q] uint64: %w", key, err)
			}
			if fi, ok := ggufKeyMap[key]; ok {
				ggufSetField(info, fi, int(val))
			}

		case 5: // int32
			var val int32
			if err := binary.Read(br, binary.LittleEndian, &val); err != nil {
				return nil, fmt.Errorf("read key[%q] int32: %w", key, err)
			}
			if fi, ok := ggufKeyMap[key]; ok {
				ggufSetField(info, fi, int(val))
			}

		case 11: // int64
			var val int64
			if err := binary.Read(br, binary.LittleEndian, &val); err != nil {
				return nil, fmt.Errorf("read key[%q] int64: %w", key, err)
			}
			if fi, ok := ggufKeyMap[key]; ok {
				ggufSetField(info, fi, int(val))
			}

		case 6: // float32
			var val float32
			if err := binary.Read(br, binary.LittleEndian, &val); err != nil {
				return nil, fmt.Errorf("read key[%q] float32: %w", key, err)
			}
			_ = val // не используем, пропускаем

		case 0: // uint8
			var val uint8
			if err := binary.Read(br, binary.LittleEndian, &val); err != nil {
				return nil, fmt.Errorf("read key[%q] uint8: %w", key, err)
			}
			_ = val

		case 7: // bool
			var val uint8
			if err := binary.Read(br, binary.LittleEndian, &val); err != nil {
				return nil, fmt.Errorf("read key[%q] bool: %w", key, err)
			}
			_ = val

		case 9: // array — пропускаем, зная тип элемента и количество
			var elemType uint32
			if err := binary.Read(br, binary.LittleEndian, &elemType); err != nil {
				return nil, fmt.Errorf("read key[%q] array elem type: %w", key, err)
			}
			var arrLen uint64
			if err := binary.Read(br, binary.LittleEndian, &arrLen); err != nil {
				return nil, fmt.Errorf("read key[%q] array len: %w", key, err)
			}
			// R83: <arch>.attention.recurrent_layers — явный список рекуррентных
			// слоёв. Значения не разбираем (нам достаточно факта наличия): при
			// наличии ключа правило по умолчанию из llama.cpp не применяется, и
			// число KV-слоёв считается верхней оценкой с exact=false (kv_layers.go).
			if strings.HasSuffix(key, ".attention.recurrent_layers") {
				info.HasRecurrentLayersKey = true
			}
			// R83 (2026-10-01): <arch>.attention.sliding_window_pattern — массив
			// из n_layer флагов: '1' = слой со скользящим окном, '0' = глобальный.
			// Нужен, чтобы отделить глобальный KV (растёт с n_ctx) от оконного:
			// у gemma-4-E4B глобальных слоёв всего 4 из 24 с кэшем.
			if strings.HasSuffix(key, ".attention.sliding_window_pattern") {
				pattern, ok := readGGUFBoolArray(br, elemType, arrLen)
				if ok {
					info.SWAPattern = pattern
					continue
				}
			}
			// Пропускаем элементы массива
			elemSize := ggufTypeSize(elemType)
			if elemSize > 0 {
				skipBytes := int64(arrLen) * int64(elemSize)
				if _, err := io.CopyN(io.Discard, br, skipBytes); err != nil {
					return nil, fmt.Errorf("skip array key[%q]: %w", key, err)
				}
			} else if elemType == 8 {
				// R47 (2026-08-19): array of strings (elemType=8). ggufTypeSize
				// returns 0 for strings (variable length), so the fixed-size
				// skip above is a no-op. We have to skip each string
				// individually: uint64 length + N bytes payload.
				// Qwen3.6-35B-A3B-UD-Q4_K_M.gguf has `general.tags = ["qwen3_5_moe",
				// "qwen", "image-text-to-text"]` and similar — without this fix,
				// the parser reads past the array, gets garbage as the next
				// keyLen, hits the "key length > 8192" guard, and returns
				// error. As a result, info.Architecture stays "" and
				// /api/show returns "unknown" for Qwen3.6.
				for j := uint64(0); j < arrLen; j++ {
					var strLen uint64
					if err := binary.Read(br, binary.LittleEndian, &strLen); err != nil {
						return nil, fmt.Errorf("read key[%q] string[%d] length: %w", key, j, err)
					}
					if strLen > 8192 {
						return nil, fmt.Errorf("read key[%q] string[%d] length %d exceeds maximum 8192", key, j, strLen)
					}
					if _, err := io.CopyN(io.Discard, br, int64(strLen)); err != nil {
						return nil, fmt.Errorf("skip key[%q] string[%d]: %w", key, j, err)
					}
				}
			}

		default:
			// Неизвестный тип — не можем пропустить, ошибка
			return nil, fmt.Errorf("unsupported GGUF metadata type %d for key %q", valType, key)
		}
	}

	return info, nil
}

// ggufSetField устанавливает поле GGUFHeaderInfo по field index.
//
// Field index (должен соответствовать ggufFieldSuffixes):
//
//	0 = NLayers              (.block_count)
//	1 = NHeads               (.attention.head_count)
//	2 = NKvHeads             (.attention.head_count_kv)
//	3 = NEmbd                (.embedding_length)
//	4 = ContextLength        (.context_length)         // Round 37: training context
//	5 = KeyLength            (.attention.key_length)   // R83: head_dim KV
//	6 = ValueLength          (.attention.value_length)
//	7 = NextNPredictLayers   (.nextn_predict_layers)   // R83: MTP-слои
//	8 = FullAttentionInterval(.full_attention_interval)
//	9 = SharedKVLayers       (.attention.shared_kv_layers)  // R83: KV-sharing
//	10 = SlidingWindow       (.attention.sliding_window)
//	11 = SWAKeyLength        (.attention.key_length_swa)
func ggufSetField(info *GGUFHeaderInfo, fieldIndex, value int) {
	switch fieldIndex {
	case 0:
		info.NLayers = value
	case 1:
		info.NHeads = value
	case 2:
		info.NKvHeads = value
	case 3:
		info.NEmbd = value
	case 4:
		info.ContextLength = value
	case 5:
		info.KeyLength = value
	case 6:
		info.ValueLength = value
	case 7:
		info.NextNPredictLayers = value
	case 8:
		info.FullAttentionInterval = value
	case 9:
		info.SharedKVLayers = value
	case 10:
		info.SlidingWindow = value
	case 11:
		info.SWAKeyLength = value
	}
}

// readGGUFBoolArray читает массив флагов GGUF и возвращает его как строку '1'/'0'.
//
// Нужен для <arch>.attention.sliding_window_pattern (gemma4/gemma3n): llama.cpp
// строит по нему is_swa(il), а от этого зависит, растёт KV-кэш слоя с n_ctx или
// ограничен окном. ok=false — тип элементов не поддержан, вызывающий обязан
// пропустить массив штатным способом.
func readGGUFBoolArray(br *bufio.Reader, elemType uint32, arrLen uint64) (string, bool) {
	switch elemType {
	case 0, 1, 7, 4, 5, 10, 11:
	default:
		return "", false
	}
	if arrLen == 0 || arrLen > 512 {
		return "", false
	}
	b := make([]byte, 0, arrLen)
	for j := uint64(0); j < arrLen; j++ {
		var v uint64
		switch elemType {
		case 0, 1, 7:
			var x uint8
			if err := binary.Read(br, binary.LittleEndian, &x); err != nil {
				return "", false
			}
			v = uint64(x)
		case 4, 5:
			var x uint32
			if err := binary.Read(br, binary.LittleEndian, &x); err != nil {
				return "", false
			}
			v = uint64(x)
		case 10, 11:
			if err := binary.Read(br, binary.LittleEndian, &v); err != nil {
				return "", false
			}
		}
		if v != 0 {
			b = append(b, '1')
		} else {
			b = append(b, '0')
		}
	}
	return string(b), true
}

// ggufTypeSize возвращает размер элемента GGUF metadata value по типу.
// Для скалярных типов — их размер, для массивов — размер одного элемента.
func ggufTypeSize(valType uint32) int64 {
	switch valType {
	case 0, 1, 7: // uint8, int8, bool
		return 1
	case 2, 3: // uint16, int16
		return 2
	case 4, 5, 6: // uint32, int32, float32
		return 4
	case 10, 11, 12: // uint64, int64, float64
		return 8
	default:
		return 0 // variable length (string, array) — не используем
	}
}

// estimateLayersFromFileSize оценивает общее число слоёв модели на основе размера GGUF файла.

// Эвристика для типичных архитектур Llama/Mistral/Qwen при Q4_K_M квантизации:
//   - < 5GB   → 7B модель (32 слоя)
//   - 5-10GB  → 13B модель (40 слоёв)
//   - 10-20GB → 30-33B модель (60 слоёв)
//   - 20-40GB → 70B модель (80 слоёв)
//   - > 40GB  → 120B+ модель (120 слоёв)
func estimateLayersFromFileSize(sizeBytes int64) int {
	sizeGB := float64(sizeBytes) / 1024 / 1024 / 1024
	switch {
	case sizeGB < 4.5:
		return 32 // 7B params
	case sizeGB < 7:
		return 42 // 7-9B class (gemma-4, mistral-7B v3, etc.)
	case sizeGB < 10:
		return 40 // 13B params
	case sizeGB < 20:
		return 60 // 30-33B params
	case sizeGB < 40:
		return 80 // 70B params
	default:
		return 120 // 120B+ params
	}
}

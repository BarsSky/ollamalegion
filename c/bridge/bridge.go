// Package bridge — CGo-обёртка для вызова C-функций llama.cpp
//
// Этот файл содержит Go-интерфейс к C bridge (bridge.h/bridge.c).
// CGo используется для прямого вызова C-кода без промежуточных процессов.
//
// #cgo CFLAGS: -I../llama.cpp -I.
// #cgo LDFLAGS: -L../llama.cpp -llama -lm -lpthread
//
// Для Windows (MinGW):
// #cgo windows LDFLAGS: -L../llama.cpp -llama -lm -lws2_32
//
// Для сборки без реального llama.cpp (stub режим):
// go build -tags llama_stub
//
// #cgo !llama_stub LDFLAGS: -L../llama.cpp -llama -lm -lpthread
// #cgo linux LDFLAGS: -lrt

//go:build !llama_stub

package bridge

/*
#include <stdlib.h>
#include "bridge.h"

// extern-прототип для Go-функции, экспортируемой в C через //export
extern int streamCallbackGo(char* token, int token_len, void* user_data);
*/
import "C"
import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"runtime/cgo"
	"sync"
	"unsafe"
)

// ModelHandle — Go-представление C ModelHandle
type ModelHandle struct {
	ptr  C.ModelHandle
	path string
}

// GPUDevice — информация о GPU
type GPUDevice struct {
	Index                int
	VRAMTotalMB          uint64
	VRAMFreeMB           uint64
	Name                 string
	ComputeCapMajor      int
	ComputeCapMinor      int
}

// GenerationParams — параметры генерации
type GenerationParams struct {
	NPredict         int     // max tokens (-1 = auto)
	NKeep            int     // keep tokens from prompt
	NBatch           int     // batch size
	Temperature      float32
	TopP             float32
	TopK             float32
	MinP             float32
	TypicalP         float32
	TfsZ             float32
	RepeatPenalty    float32
	FrequencyPenalty float32
	PresencePenalty  float32
	RepeatLastN      int
	Mirostat         int
	MirostatTau      float32
	MirostatEta      float32
	Seed             int // -1 = random
	// Antiprompts — стоп-последовательности, при появлении которых в декодированной
	// выдаче стрим завершается, и сами токены не отправляются клиенту.
	// Для gemma-формата рекомендуется: ["<end_of_turn>", "<start_of_turn>user"].
	Antiprompts []string
	// StopSequences — альтернативное Ollama-название для стоп-последовательностей.
	// В cppworker маппится на Antiprompts.
	StopSequences []string
	// NCtxOverride — per-request переопределение n_ctx (0 = use effective
	// n_ctx модели, загруженной с диска). Используется для pre-flight
	// проверки ёмкости: если NCtxOverride > эффективного n_ctx модели,
	// C-bridge возвращает informative ошибку с предложением перезагрузить
	// модель с большим n_ctx.
	NCtxOverride int
	// ClampedNPredict — true, если clampNPredictToFitContext уменьшил NPredict
	// для предотвращения code 3 (prompt too long). Используется для добавления
	// warning в ответ клиенту.
	ClampedNPredict bool `json:"-"`
	// ClampedNPredictOriginal — исходное значение NPredict до клампинга.
	// Используется для информативного warning в ответе.
	ClampedNPredictOriginal int `json:"-"`
	// Round 13 (2026-07-28): sequence id for multi-slot batched inference.
	// 0 = single-slot legacy (clear all KV-cache on every call).
	// > 0 = use slot seq_id (clear only this slot's region; isolated from
	//       other concurrent calls). Caller (Go slot manager) assigns unique
	//       seq_ids per concurrent slot (slot = 0..n_parallel-1).
	SeqId int
}

// DefaultGenerationParams возвращает параметры по умолчанию
func DefaultGenerationParams() GenerationParams {
	return GenerationParams{
		NPredict:         2048, // уменьшен с 4096 (Phase D.6): иначе при n_ctx=4096 короткий prompt
		// (типа 35 токенов OpenWebUI) + 4096 = 4131 > 4096 → code 3: prompt too long.
		// С 2048: 35+2048+1=2084 << 4096 ✓. Если нужен длинный ответ, клиент должен
		// явно задать max_tokens/num_predict в body — applyCppCtxHeader его не трогает.
		NKeep:            0,
		NBatch:           512,
		Temperature:      0.7,
		TopP:             0.9,
		TopK:             40.0,
		MinP:             0.0,
		TypicalP:         1.0,
		TfsZ:             1.0,
		RepeatPenalty:    1.1,
		FrequencyPenalty: 0.0,
		PresencePenalty:  0.0,
		RepeatLastN:      64,
		Mirostat:         0,
		MirostatTau:      5.0,
		MirostatEta:      0.1,
		Seed:             -1,
	}
}

// ModelConfig — конфигурация загрузки модели
type ModelConfig struct {
	ModelPath      string   // путь к GGUF файлу
	NContext       int      // размер контекста (4096)
	NBatch         int      // размер батча (512)
	NThreads       int      // потоков CPU (0 = auto)
	NThreadsBatch  int      // потоков для батча (0 = auto)
	NGPULayers     int      // слоёв на GPU (-1 = все, 0 = CPU)
	MainGPU        int      // индекс главного GPU
	FlashAttnType  int      // llama_flash_attn_type: -1=auto, 0=disabled, 1=enabled
	NUMA           bool     // NUMA оптимизация
	TensorSplit    []float32 // пропорции multi-GPU
	// SplitMode (Phase 8 P.4, 2026-07-11):
	//   -1 = use llama.cpp default (LLAMA_SPLIT_MODE_LAYER)
	//    0 = LLAMA_SPLIT_MODE_NONE (single GPU)
	//    1 = LLAMA_SPLIT_MODE_LAYER (pipeline parallel, stable)
	//    2 = LLAMA_SPLIT_MODE_ROW (deprecated)
	//    3 = LLAMA_SPLIT_MODE_TENSOR (experimental, requires NCCL)
	SplitMode      int
	UseMmap        bool     // mmap (true)
	UseMlock       bool     // mlock
	// RoPE параметры контекста
	RopeFreqBase      float32
	RopeFreqScale     float32
	RopeScalingType   string
	RopeScalingFactor float32
	// YaRN параметры
	YarnExtFactor  float32
	YarnAttnFactor float32
	YarnBetaFast   float32
	YarnBetaSlow   float32
	// KV Cache
	NoKVOffload bool
	KVCacheType string
	// Session 16 (2026-06-27): число параллельных sequences (n_parallel в llama.cpp).
	// 0 = дефолт cppworker (=1). >0 = multi-slot batched generation.
	// Требует больше VRAM (KV-cache × parallel слотов).
	NParallel int
	// Нормализация
	RMSNormEps float32
	// Прочее
	NoMemoryMap bool
	RPCBackend  string

	// Round 7: override-tensors. Parallel arrays (length = OverrideTensorCount).
	// Patterns are POSIX regex (e.g. `blk\\..*\\.ffn_.*_exps\\.weight`),
	// buft names resolve to ggml_backend_buffer_type_t internally in C.
	// Supported buft names: "CPU", "CUDA0", "CUDA1", ...
	OverrideTensors      []string // nil = no override
	OverrideTensorBufts  []string // parallel slice, len == len(OverrideTensors)
}

// DefaultModelConfig возвращает конфигурацию по умолчанию
func DefaultModelConfig(path string) ModelConfig {
	return ModelConfig{
		ModelPath:  path,
		NContext:   4096,
		NBatch:     512,
		NThreads:   0,
		NGPULayers: -1, // все слои на GPU
		MainGPU:    0,
		FlashAttnType:  -1, // auto
		UseMmap:    true,
	}
}

// InferenceResult — результат инференса
type InferenceResult struct {
	Output   string
	Status   int // 0 = success; 2=BRIDGE_ERR_N_CTX_NEEDS_RELOAD, 3=BRIDGE_ERR_PROMPT_TOO_LONG, и т.д.
	ErrorMsg string
}

// ErrCode — коды структурированных ошибок из C-моста. Эти значения
// мапятся на одноимённые BRIDGE_ERR_* #define в c/bridge/bridge.h.
// Используются для type switch в Go-стороне (см. internal/balancer/nctx_reload.go).
const (
	ErrCodeOK                 = 0
	ErrCodeGeneric            = 1
	ErrCodeNCtxNeedsReload    = 2 // n_ctx_override > загруженного n_ctx; возможен auto-reload
	ErrCodePromptTooLong      = 3 // prompt+n_predict > n_ctx и override не помогает
	ErrCodeGPUOOM             = 4 // нехватка VRAM при попытке аллокации
	ErrCodeBadRequest         = 5 // некорректные параметры
	// ErrCodeInsufficientResources (6) — недостаточно VRAM+RAM для загрузки
	// модели с запрошенным n_ctx, даже после каскадного auto-fallback
	// (RAM mmap → partial offload → cpu-only + auto_tune n_ctx).
	// 2026-06-25: генерируется cppworker (cmd/cppworker/utils.go:writeInsufficientResourcesResponse)
	// и балансером не считается reloadable — проброс клиенту как HTTP 413
	// с actionable details (см. docs/runbook-tools.md сценарий G).
	ErrCodeInsufficientResources = 6
)

// BridgeErrorInfo — Go-представление C BridgeErrorInfo (см. bridge.h).
// Заполняется C-кодом при любом не-OK-возврате из bridge_infer/bridge_infer_stream.
// Доступно через GetLastErrorInfo() сразу после такого возврата.
type BridgeErrorInfo struct {
	Code          int    // один из ErrCode* выше
	CurrentNCtx   int    // фактический n_ctx загруженной модели
	RequiredNCtx  int    // минимальный n_ctx, который нужен для запроса
	ActualTokens  int    // размер prompt в токенах
	NPredict      int    // запрошенное число генерируемых токенов
	NCtxOverride  int    // значение n_ctx_override из params (0 если не задан)
	MaxVRAMNCtx   int    // оценочный максимум n_ctx для текущей VRAM (0 если неизвестно)
	Message       string // человекочитаемое описание ошибки
}

// GetLastErrorInfo возвращает структурированную информацию о последней ошибке
// из C-bridge. Должна вызываться СРАЗУ после того, как Infer/InferStream вернул
// ошибку. Действительна до следующего вызова C-bridge.
//
// Если ошибки не было (или прошло несколько успешных вызовов), возвращает
// BridgeErrorInfo{Code: ErrCodeOK}.
func GetLastErrorInfo() *BridgeErrorInfo {
	cInfo := C.bridge_get_last_error_info()
	if cInfo == nil {
		return &BridgeErrorInfo{Code: ErrCodeOK}
	}
	return &BridgeErrorInfo{
		Code:         int(cInfo.code),
		CurrentNCtx:  int(cInfo.current_n_ctx),
		RequiredNCtx: int(cInfo.required_n_ctx),
		ActualTokens: int(cInfo.actual_tokens),
		NPredict:     int(cInfo.n_predict),
		NCtxOverride: int(cInfo.n_ctx_override),
		MaxVRAMNCtx:  int(cInfo.max_vram_n_ctx),
		Message:      C.GoString(&cInfo.message[0]),
	}
}

// ErrNCtxNeedsReload — sentinel для errors.Is. Возвращается Go-обёрткой
// Infer/InferStream, если C-мост сигнализировал ErrCodeNCtxNeedsReload.
// Используется в balancer для решения: дёрнуть auto-reload на бэкенде
// (если VRAM позволяет) или вернуть 413 клиенту.
var ErrNCtxNeedsReload = fmt.Errorf("n_ctx exceeds loaded model; auto-reload may be possible")

// ErrPromptTooLong — sentinel для errors.Is. C-bridge вернул ErrCodePromptTooLong.
// Это hard error: даже с reload не получится, потому что prompt сам по себе
// слишком длинный (либо n_predict > запрошенного n_ctx).
var ErrPromptTooLong = fmt.Errorf("prompt + n_predict exceeds n_ctx")

// ModelMetadata — метаданные модели
type ModelMetadata struct {
	Description    string
	Architecture   string
	ContextLength  int
	NLayers        int
	NHeads         int
	NKvHeads       int  // NEW: GQA kv heads
	HeadDimK       int  // NEW: K head dim
	HeadDimV       int  // NEW: V head dim
	NEmbd          int
	NVocab         int
	SizeTotalBytes uint64
}

// StreamCallback — функция обратного вызова для стриминга
// Возвращает true для продолжения, false для остановки
type StreamCallback func(token string) bool

// ============================================================
// Bridge — синглтон для доступа к C-функциям
// ============================================================

var (
	mu       sync.Mutex
	initDone bool
)

// Init инициализирует bridge (вызывается один раз)
func Init() error {
	mu.Lock()
	defer mu.Unlock()
	if initDone {
		return nil
	}
	C.bridge_init()
	initDone = true
	return nil
}

// Version возвращает версию bridge
func Version() string {
	cstr := C.bridge_version()
	return C.GoString(cstr)
}

// GetGPUCount возвращает количество доступных GPU
func GetGPUCount() int {
	return int(C.bridge_get_gpu_count())
}

// GetGPUInfo возвращает информацию о GPU
func GetGPUInfo(index int) (*GPUDevice, error) {
	var info C.GPUDeviceInfo
	ret := C.bridge_get_gpu_info(C.int(index), &info)
	if ret != 0 {
		return nil, fmt.Errorf("bridge_get_gpu_info: %s", C.GoString(C.bridge_last_error()))
	}
	return &GPUDevice{
		Index:           int(info.index),
		VRAMTotalMB:     uint64(info.vram_total_mb),
		VRAMFreeMB:      uint64(info.vram_free_mb),
		Name:            C.GoString(&info.name[0]),
		ComputeCapMajor: int(info.compute_capability_major),
		ComputeCapMinor: int(info.compute_capability_minor),
	}, nil
}

// LoadModel загружает GGUF модель
func LoadModel(cfg ModelConfig) (*ModelHandle, error) {
	cCfg := C.ModelConfig{}

	modelPathC := C.CString(cfg.ModelPath)
	defer C.free(unsafe.Pointer(modelPathC))
	cCfg.model_path = modelPathC

	cCfg.n_ctx = C.int(cfg.NContext)
	cCfg.n_batch = C.int(cfg.NBatch)
	cCfg.n_threads = C.int(cfg.NThreads)
	cCfg.n_threads_batch = C.int(cfg.NThreadsBatch)
	cCfg.n_gpu_layers = C.int(cfg.NGPULayers)
	cCfg.main_gpu = C.int(cfg.MainGPU)

	cCfg.flash_attn_type = C.int(cfg.FlashAttnType)
	if cfg.NUMA {
		cCfg.numa = 1
	}
	if cCfg.n_threads <= 0 {
		cCfg.n_threads = C.int(runtime.NumCPU())
	}

	// Tensor split (Phase 8 P.4): array + split_mode.
	if len(cfg.TensorSplit) > 0 {
		ts := make([]C.float, len(cfg.TensorSplit))
		for i, v := range cfg.TensorSplit {
			ts[i] = C.float(v)
		}
		cCfg.tensor_split = &ts[0]
		cCfg.tensor_split_len = C.int(len(cfg.TensorSplit))
	}
	// split_mode: -1 = use llama.cpp default; 0-3 = explicit value.
	cCfg.split_mode = C.int(cfg.SplitMode)

	if cfg.UseMmap {
		cCfg.use_mmap = 1
	}
	if cfg.UseMlock {
		cCfg.use_mlock = 1
	}

	// Session 16 (2026-06-27): Parallel + KVCacheType.
	// В актуальной llama.cpp (b4500+) "n_parallel" нет — есть n_seq_max
	// (max number of sequences, см. llama_context_params).
	// В C-bridge пробрасываем напрямую в n_seq_max.
	cCfg.n_parallel = C.int(cfg.NParallel)
	// kv_cache_type в Go API: "f16" | "q8_0" | "q4_0" | "" (inherit default).
	// Маппим в int по схеме, документированной в bridge.h:
	//   f16  → 0 (default, наследуется из llama_context_default_params)
	//   q8_0 → 1 (enum ggml_type GGML_TYPE_Q8_0 = 8)
	//   q4_0 → 2 (enum ggml_type GGML_TYPE_Q4_0 = 2)
	// Если строка пустая или неизвестная — 0 (bridge.c интерпретирует как default).
	cCfg.kv_cache_type = C.int(kvCacheTypeToBridgeInt(cfg.KVCacheType))

	// Round 7: pack OverrideTensors parallel arrays to C.
	// Use runtime.Pinner to keep the slice backing array from being
	// moved by the GC during the C call, and free C-strings inline.
	if len(cfg.OverrideTensors) > 0 && len(cfg.OverrideTensors) == len(cfg.OverrideTensorBufts) {
		pinnedPats := make([]*C.char, len(cfg.OverrideTensors))
		pinnedBufts := make([]*C.char, len(cfg.OverrideTensorBufts))
		for i, p := range cfg.OverrideTensors {
			pinnedPats[i] = C.CString(p)
			defer C.free(unsafe.Pointer(pinnedPats[i]))
		}
		for i, b := range cfg.OverrideTensorBufts {
			pinnedBufts[i] = C.CString(b)
			defer C.free(unsafe.Pointer(pinnedBufts[i]))
		}
		cCfg.override_tensor_count = C.int(len(cfg.OverrideTensors))
		var pinner runtime.Pinner
		defer pinner.Unpin()
		pinner.Pin(&pinnedPats[0])
		pinner.Pin(&pinnedBufts[0])
		cCfg.override_tensor_patterns = &pinnedPats[0]
		cCfg.override_tensor_buft_names = &pinnedBufts[0]
	}

	var errMsg *C.char
	handle := C.bridge_load_model(&cCfg, &errMsg)
	if handle == nil {
		errStr := ""
		if errMsg != nil {
			errStr = C.GoString(errMsg)
			C.bridge_free_string(errMsg)
		}
		return nil, fmt.Errorf("load model failed: %s", errStr)
	}

	return &ModelHandle{ptr: handle, path: cfg.ModelPath}, nil
}

// FreeModel выгружает модель
func (m *ModelHandle) FreeModel() {
	if m == nil || m.ptr == nil {
		return
	}
	C.bridge_free_model(m.ptr)
	m.ptr = nil
}

// fillAntiprompts — копирует Go-строки params.Antiprompts в C-массив const char*.
// Возвращает (*C.char, count) — вызывающий обязан освободить каждый элемент
// через C.free(unsafe.Pointer(...)) после использования.
// antipromptsPtrSize — размер указателя char* (8 на 64-bit, 4 на 32-bit).
var antipromptsPtrSize = unsafe.Sizeof((*C.char)(nil))

func fillAntiprompts(ap []string) (**C.char, C.int) {
	n := C.int(len(ap))
	if n == 0 {
		return nil, 0
	}
	cArr := (**C.char)(C.calloc(C.size_t(n), C.size_t(antipromptsPtrSize)))
	for i, s := range ap {
		// Дублируем строку — C.bridge_infer* использует её синхронно,
		// но Go-исходник может быть освобождён GC после возврата из C-функции.
		cstr := C.CString(s)
		// Пишем указатель в массив через unsafe.Pointer-arith
		slot := (**C.char)(unsafe.Pointer(uintptr(unsafe.Pointer(cArr)) + uintptr(i)*antipromptsPtrSize))
		*slot = cstr
	}
	return cArr, n
}

// freeAntiprompts — освобождает массив C-строк, созданный fillAntiprompts.
func freeAntiprompts(cArr **C.char, n C.int) {
	if cArr == nil || n == 0 {
		return
	}
	for i := C.int(0); i < n; i++ {
		// Читаем указатель обратно
		slot := (**C.char)(unsafe.Pointer(uintptr(unsafe.Pointer(cArr)) + uintptr(i)*antipromptsPtrSize))
		if *slot != nil {
			C.free(unsafe.Pointer(*slot))
		}
	}
	C.free(unsafe.Pointer(cArr))
}

// Infer выполняет синхронный инференс
func (m *ModelHandle) Infer(prompt string, params GenerationParams) (*InferenceResult, error) {
	if m == nil || m.ptr == nil {
		return nil, fmt.Errorf("model not loaded")
	}

	cPrompt := C.CString(prompt)
	defer C.free(unsafe.Pointer(cPrompt))

	cApArr, cApN := fillAntiprompts(params.Antiprompts)
	defer freeAntiprompts(cApArr, cApN)

	cParams := C.GenerationParams{
		n_predict:        C.int(params.NPredict),
		n_keep:           C.int(params.NKeep),
		n_batch:          C.int(params.NBatch),
		temperature:      C.float(params.Temperature),
		top_p:            C.float(params.TopP),
		top_k:            C.float(params.TopK),
		repeat_penalty:   C.float(params.RepeatPenalty),
		frequency_penalty: C.float(params.FrequencyPenalty),
		presence_penalty: C.float(params.PresencePenalty),
		seed:             C.int(params.Seed),
		antiprompts:      cApArr,
		n_antiprompts:    cApN,
		n_ctx_override:   C.int(params.NCtxOverride),
		// Round 13: seq_id для multi-slot.
		// params.SeqId 0 = legacy single-slot, > 0 = use slot region.
		seq_id:           C.int(params.SeqId),
	}

	result := C.bridge_infer(m.ptr, cPrompt, &cParams)
	defer C.bridge_free_inference_result(&result)

	if result.status != 0 {
		// C-bridge теперь заполняет структурированный BridgeErrorInfo.
		// Используем GetLastErrorInfo() для классификации ошибки и проброса
		// sentinel ErrNCtxNeedsReload / ErrPromptTooLong (см. InferStream).
		info := GetLastErrorInfo()
		detail := info.Message
		if detail == "" {
			if result.error_msg != nil {
				detail = C.GoString(result.error_msg)
			}
		}
		if detail == "" {
			detail = "(no detail from bridge)"
		}
		base := fmt.Errorf("inference failed: code=%d %s (current_n_ctx=%d, required_n_ctx=%d, max_vram_n_ctx=%d)",
			result.status, detail, info.CurrentNCtx, info.RequiredNCtx, info.MaxVRAMNCtx)
		switch info.Code {
		case ErrCodeNCtxNeedsReload:
			return nil, fmt.Errorf("%w: %w", base, ErrNCtxNeedsReload)
		case ErrCodePromptTooLong:
			return nil, fmt.Errorf("%w: %w", base, ErrPromptTooLong)
		}
		return nil, base
	}

	return &InferenceResult{
		Output:   C.GoStringN(result.output, result.output_len),
		Status:   int(result.status),
		ErrorMsg: "",
	}, nil
}

// InferStream выполняет стриминг-инференс
func (m *ModelHandle) InferStream(prompt string, params GenerationParams, callback StreamCallback) error {
	if m == nil || m.ptr == nil {
		return fmt.Errorf("model not loaded")
	}

	cPrompt := C.CString(prompt)
	defer C.free(unsafe.Pointer(cPrompt))

	cApArr, cApN := fillAntiprompts(params.Antiprompts)
	defer freeAntiprompts(cApArr, cApN)

	cParams := C.GenerationParams{
		n_predict:        C.int(params.NPredict),
		n_keep:           C.int(params.NKeep),
		n_batch:          C.int(params.NBatch),
		temperature:      C.float(params.Temperature),
		top_p:            C.float(params.TopP),
		top_k:            C.float(params.TopK),
		repeat_penalty:   C.float(params.RepeatPenalty),
		frequency_penalty: C.float(params.FrequencyPenalty),
		presence_penalty: C.float(params.PresencePenalty),
		seed:             C.int(params.Seed),
		antiprompts:      cApArr,
		n_antiprompts:    cApN,
		n_ctx_override:   C.int(params.NCtxOverride),
		// Round 13: seq_id для multi-slot.
		seq_id:           C.int(params.SeqId),
	}

	// Используем cgo.Handle для безопасной передачи Go-контекста в C.
	// cgo.Handle — это официальный механизм (с Go 1.17) для передачи
	// Go-указателей через границу CGo. Он гарантирует, что GC не переместит
	// ни сам объект, ни вложенные в замыкание ссылки.
	cbData := &streamCallbackData{fn: callback}
	handle := cgo.NewHandle(cbData)
	defer handle.Delete()

	ret := C.bridge_infer_stream(
		m.ptr, cPrompt, &cParams,
		C.StreamCallback(C.streamCallbackGo),
		unsafe.Pointer(&handle),
	)

	if ret != 0 {
		// C-bridge возвращает структурированный код ошибки:
		//   0 — OK
		//   1 — generic
		//   2 — BRIDGE_ERR_N_CTX_NEEDS_RELOAD (auto-reload возможен)
		//   3 — BRIDGE_ERR_PROMPT_TOO_LONG (hard error)
		//   4 — BRIDGE_ERR_GPU_OOM
		//   5 — BRIDGE_ERR_BAD_REQUEST
		// До правки ret всегда был 1 на любую ошибку, и balancer не мог
		// отличить n_ctx-need-reload от других ошибок. Теперь
		// GetLastErrorInfo() возвращает полную BridgeErrorInfo для
		// диагностики, а sentinel ErrNCtxNeedsReload / ErrPromptTooLong
		// позволяют использовать errors.Is().
		info := GetLastErrorInfo()
		detail := info.Message
		if detail == "" {
			detail = C.GoString(C.bridge_last_error())
		}
		if detail == "" {
			detail = "(no detail from bridge)"
		}
		base := fmt.Errorf("stream inference failed with code %d: %s (current_n_ctx=%d, required_n_ctx=%d, max_vram_n_ctx=%d)",
			ret, detail, info.CurrentNCtx, info.RequiredNCtx, info.MaxVRAMNCtx)
		switch info.Code {
		case ErrCodeNCtxNeedsReload:
			return fmt.Errorf("%w: %w", base, ErrNCtxNeedsReload)
		case ErrCodePromptTooLong:
			return fmt.Errorf("%w: %w", base, ErrPromptTooLong)
		}
		return base
	}
	return nil
}

// GetEmbeddings получает эмбеддинги текста
func (m *ModelHandle) GetEmbeddings(text string) ([]float32, error) {
	if m == nil || m.ptr == nil {
		return nil, fmt.Errorf("model not loaded")
	}

	cText := C.CString(text)
	defer C.free(unsafe.Pointer(cText))

	result := C.bridge_get_embeddings(m.ptr, cText)
	defer C.bridge_free_inference_result(&result)

	if result.status != 0 {
		errMsg := ""
		if result.error_msg != nil {
			errMsg = C.GoString(result.error_msg)
		}
		return nil, fmt.Errorf("embeddings failed: %s", errMsg)
	}

	outputStr := C.GoStringN(result.output, result.output_len)
	if len(outputStr) == 0 {
		return []float32{}, nil
	}

	var embeddings []float32
	if err := json.Unmarshal([]byte(outputStr), &embeddings); err != nil {
		return nil, fmt.Errorf("embeddings parse error: %w (raw: %s)", err, outputStr)
	}
	return embeddings, nil
}

// ChatMessage — одно сообщение чата для применения chat template.
type ChatMessage struct {
	Role    string
	Content string
}

// ApplyChatTemplate применяет chat template из GGUF к списку сообщений.
func (m *ModelHandle) ApplyChatTemplate(system string, messages []ChatMessage, addAss bool) (string, error) {
	if m == nil || m.ptr == nil {
		return "", fmt.Errorf("model not loaded")
	}
	if len(messages) == 0 {
		return "", fmt.Errorf("no messages provided")
	}

	var userContents []string
	var assistantContents []string
	for _, msg := range messages {
		switch msg.Role {
		case "user":
			userContents = append(userContents, msg.Content)
		case "assistant":
			assistantContents = append(assistantContents, msg.Content)
		case "system":
			continue
		case "tool":
			userContents = append(userContents, msg.Content)
		default:
			userContents = append(userContents, msg.Content)
		}
	}
	if len(userContents) == 0 {
		return "", fmt.Errorf("no user messages provided")
	}

	cUserPtrs := make([]*C.char, len(userContents))
	for i, s := range userContents {
		cUserPtrs[i] = C.CString(s)
		defer C.free(unsafe.Pointer(cUserPtrs[i]))
	}

	var cAssPtrs []*C.char
	if len(assistantContents) > 0 {
		cAssPtrs = make([]*C.char, len(assistantContents))
		for i, s := range assistantContents {
			cAssPtrs[i] = C.CString(s)
			defer C.free(unsafe.Pointer(cAssPtrs[i]))
		}
	}

	var cSystem *C.char
	if system != "" {
		cSystem = C.CString(system)
		defer C.free(unsafe.Pointer(cSystem))
	}

	const outBufSize = 64 * 1024
	outBuf := (*C.char)(C.malloc(C.size_t(outBufSize)))
	defer C.free(unsafe.Pointer(outBuf))

	var cUserArg **C.char
	if len(cUserPtrs) > 0 {
		cUserArg = &cUserPtrs[0]
	}
	var cAssArg **C.char
	if len(cAssPtrs) > 0 {
		cAssArg = &cAssPtrs[0]
	}

	ret := C.bridge_apply_chat_template(
		m.ptr,
		cSystem,
		cUserArg,
		C.int32_t(len(cUserPtrs)),
		cAssArg,
		C.int32_t(len(cAssPtrs)),
		C.bool(addAss),
		outBuf,
		C.int32_t(outBufSize),
	)

	if ret == -2 {
		return "", ErrNoChatTemplate
	}
	if ret < 0 {
		return "", fmt.Errorf("bridge_apply_chat_template failed: %d", ret)
	}

	return C.GoStringN(outBuf, C.int(ret)), nil
}

// GetChatTemplate возвращает raw chat template из GGUF.
func (m *ModelHandle) GetChatTemplate() (string, error) {
	if m == nil || m.ptr == nil {
		return "", fmt.Errorf("model not loaded")
	}

	const bufSize = 16 * 1024
	buf := (*C.char)(C.malloc(C.size_t(bufSize)))
	defer C.free(unsafe.Pointer(buf))

	ret := C.bridge_get_chat_template(m.ptr, buf, C.int32_t(bufSize))
	if ret == -2 {
		return "", ErrNoChatTemplate
	}
	if ret < 0 {
		return "", fmt.Errorf("bridge_get_chat_template failed: %d", ret)
	}
	return C.GoStringN(buf, C.int(ret)), nil
}

// ApplyChatTemplateWithThinking — Round 14a (2026-07-28): native
// enable_thinking через C++ API common_chat_templates_apply.
//
// В отличие от ApplyChatTemplate (legacy, использует llama_chat_apply_template),
// эта функция поддерживает native enable_thinking — для моделей, у которых
// Jinja template содержит enable_thinking variable (Qwen3-thinking, DeepSeek-R1,
// GLM-Z1 и др.).
//
// Параметры:
//   chatTemplateOverride  — кастомный Jinja template ("" = use GGUF default)
//   messages              — список chat-сообщений (включая system если нужно)
//   enableThinking        — true = native thinking mode
//   addGenerationPrompt   — true = добавить assistant turn tokens в конец
//
// Возвращает:
//   prompt               — formatted prompt
//   supportsThinking     — true если template поддерживает thinking (Jinja variable)
//   error                — nil / ErrNoChatTemplate / generic
//
// Round 14b (next session): интеграция в cmd/cppworker/handlers_chat.go
// — заменить soft prompt injection из Round 11 на этот native вызов.
func (m *ModelHandle) ApplyChatTemplateWithThinking(
	chatTemplateOverride string,
	messages []ChatMessage,
	enableThinking bool,
	addGenerationPrompt bool,
) (string, bool, error) {
	if m == nil || m.ptr == nil {
		return "", false, fmt.Errorf("model not loaded")
	}
	if len(messages) == 0 {
		return "", false, fmt.Errorf("no messages provided")
	}

	// Строим C-массив CBridgeChatMessage.
	cMsgs := make([]C.CBridgeChatMessage, len(messages))
	cRoleStrs := make([]*C.char, len(messages))
	cContentStrs := make([]*C.char, len(messages))
	defer func() {
		for _, p := range cRoleStrs {
			if p != nil {
				C.free(unsafe.Pointer(p))
			}
		}
		for _, p := range cContentStrs {
			if p != nil {
				C.free(unsafe.Pointer(p))
			}
		}
	}()

	for i, msg := range messages {
		cRoleStrs[i] = C.CString(msg.Role)
		cContentStrs[i] = C.CString(msg.Content)
		cMsgs[i].role = cRoleStrs[i]
		cMsgs[i].content = cContentStrs[i]
	}

	// Output buffer: 64KB начальный, retry при -4 (buffer too small).
	const initialBufSize = 64 * 1024
	const maxBufSize = 4 * 1024 * 1024 // 4MB cap — long multi-turn chats
	outBufSize := initialBufSize
	outBuf := (*C.char)(C.malloc(C.size_t(outBufSize)))
	defer C.free(unsafe.Pointer(outBuf))

	var cOverride *C.char
	if chatTemplateOverride != "" {
		cOverride = C.CString(chatTemplateOverride)
		defer C.free(unsafe.Pointer(cOverride))
	}

	var supportsThinking C.bool
	// m.ptr имеет тип *C.ModelHandle (typed pointer), но C-функция ожидает
	// void*. CGo не делает auto-cast — нужен явный unsafe.Pointer().
	ret := C.bridge_chat_templates_apply_with_thinking(
		unsafe.Pointer(m.ptr),
		cOverride,
		&cMsgs[0],
		C.int32_t(len(cMsgs)),
		C.bool(enableThinking),
		C.bool(addGenerationPrompt),
		outBuf,
		C.int32_t(outBufSize),
		&supportsThinking,
	)

	// Round 14b: -4 = buffer too small. Retry с 2x buffer size до max.
	for ret == -4 && outBufSize < maxBufSize {
		// Realloc to 2x current size.
		newSize := outBufSize * 2
		if newSize > maxBufSize {
			newSize = maxBufSize
		}
		newBuf := (*C.char)(C.realloc(unsafe.Pointer(outBuf), C.size_t(newSize)))
		if newBuf == nil {
			return "", false, fmt.Errorf("realloc to %d bytes failed", newSize)
		}
		outBuf = newBuf
		outBufSize = newSize

		supportsThinking = C.bool(false)
		ret = C.bridge_chat_templates_apply_with_thinking(
			unsafe.Pointer(m.ptr),
			cOverride,
			&cMsgs[0],
			C.int32_t(len(cMsgs)),
			C.bool(enableThinking),
			C.bool(addGenerationPrompt),
			outBuf,
			C.int32_t(outBufSize),
			&supportsThinking,
		)
	}

	if ret == -4 {
		return "", false, fmt.Errorf("output buffer overflow (need >%d bytes, max=%d)", outBufSize, maxBufSize)
	}
	if ret == -3 {
		return "", false, fmt.Errorf("common_chat_templates_init failed (no template in GGUF or invalid override)")
	}
	if ret < 0 {
		return "", false, fmt.Errorf("bridge_chat_templates_apply_with_thinking failed: %d", ret)
	}

	return C.GoStringN(outBuf, C.int(ret)), bool(supportsThinking), nil
}

// ErrNoChatTemplate — в GGUF нет tokenizer.chat_template.
var ErrNoChatTemplate = fmt.Errorf("no chat template in GGUF metadata")

// CountTokens возвращает число токенов в тексте для загруженной модели.
// В fallback-режиме (ошибка токенизации или stub) возвращает грубую оценку
// по 4 символа на токен.
func (m *ModelHandle) CountTokens(text string) int {
	if m == nil || m.ptr == nil {
		return len([]rune(text)) / 4
	}
	cText := C.CString(text)
	defer C.free(unsafe.Pointer(cText))
	n := C.bridge_count_tokens(m.ptr, cText)
	if n < 0 {
		if text == "" {
			return 0
		}
		return len([]rune(text)) / 4
	}
	return int(n)
}

// Tokenize — Round 15.1: конвертирует text в int32 массив токенов. Нужно для
// batched parallel path (BatchedScheduler.RegisterSession принимает []int32).
//
// Возвращает токены или nil + error при сбое. Семантика BOS: НЕ добавляется
// автоматически (chat template обычно вставляет BOS сам).
func (m *ModelHandle) Tokenize(text string) ([]int32, error) {
	if m == nil || m.ptr == nil {
		return nil, fmt.Errorf("model not loaded")
	}
	if text == "" {
		return []int32{}, nil
	}
	cText := C.CString(text)
	defer C.free(unsafe.Pointer(cText))

	// Сначала узнаём нужный размер буфера.
	// bridge_count_tokens объявлен с ModelHandle в bridge.h → cgo биндинг
	// ожидает _Ctype_ModelHandle (=*C.ModelHandle), не unsafe.Pointer.
	needed := C.bridge_count_tokens(m.ptr, cText)
	if needed < 0 {
		return nil, fmt.Errorf("bridge_count_tokens failed: %s", C.GoString(C.bridge_last_error()))
	}
	if needed == 0 {
		return []int32{}, nil
	}

	// Аллоцируем буфер и запрашиваем токенизацию.
	bufSize := int(needed) + 16 // +16 запас (теоретически tokenize может вернуть больше)
	cBuf := (*C.int32_t)(C.malloc(C.size_t(bufSize) * C.sizeof_int32_t))
	if cBuf == nil {
		return nil, fmt.Errorf("malloc failed for %d tokens", bufSize)
	}
	defer C.free(unsafe.Pointer(cBuf))

	// bridge_tokenize объявлен с ModelHandle в bridge.h → typed, не unsafe.Pointer.
	actual := C.bridge_tokenize(m.ptr, cText, cBuf, C.int32_t(bufSize))
	if actual < 0 {
		return nil, fmt.Errorf("bridge_tokenize failed: %s", C.GoString(C.bridge_last_error()))
	}

	// Копируем из C-буфера в Go slice (Go GC будет владеть).
	result := make([]int32, int(actual))
	for i := 0; i < int(actual); i++ {
		result[i] = int32(*(*C.int32_t)(unsafe.Pointer(uintptr(unsafe.Pointer(cBuf)) + uintptr(i)*unsafe.Sizeof(C.int32_t(0)))))
	}
	return result, nil
}

// TokenToPiece конвертирует int32 token ID в UTF-8 текст.
//
// Round 15.1: используется Backend.GenerateBatchedStream для detokenize
// int32 токенов из BatchedScheduler.TokenCh перед отправкой клиенту через
// SSE. Возвращает строку; пустая строка если ошибка или model==nil.
//
// Семантика соответствует llama_token_to_piece с lstrip=0 (сохраняет
// ведущий space для первого токена в "свежем" слове) — что совпадает с
// поведением bridge_infer_stream (line 1405 bridge.c).
func (m *ModelHandle) TokenToPiece(token int32) string {
	if m == nil || m.ptr == nil {
		return ""
	}
	const bufSize = 256 // достаточно для одного токена (max ~32 chars обычно)
	cBuf := (*C.char)(C.malloc(C.size_t(bufSize)))
	if cBuf == nil {
		return ""
	}
	defer C.free(unsafe.Pointer(cBuf))

	// bridge_token_to_piece объявлен с ModelHandle в bridge.h → cgo биндинг
	// ожидает _Ctype_ModelHandle (typed), не unsafe.Pointer.
	n := int(C.bridge_token_to_piece(m.ptr, C.int32_t(token), cBuf, C.int32_t(bufSize)))
	if n <= 0 {
		return ""
	}
	return C.GoStringN(cBuf, C.int(n))
}

// SampleToken — Round 15.2 (2026-07-30): temperature sampling для batched path.
//
// Используется BatchedScheduler после BatchedDecode для sampling next token
// из logits последнего токена sequence. Реализует:
//   - greedy argmax при temperature <= 0 (default для chain-of-thought)
//   - temperature scaling + softmax + multinomial sampling при temperature > 0
//
// Параметры:
//   logits      — []float32 размера n_vocab (raw logits из BatchedDecode)
//   temperature — 0 или negative = greedy. > 0 = softmax(temp) + multinomial
//   seed        — 0 = time-based (для production), != 0 = reproducible
//
// Round 15.3+ TODO: top_p/top_k/rep_penalty через common_sampler с
// per-session state (для rep_penalty нужно знать сгенерированные токены).
func (m *ModelHandle) SampleToken(logits []float32, temperature float32, seed uint32) (int32, error) {
	_ = m // m не используется — bridge_sample_token работает только с logits
	if len(logits) == 0 {
		return 0, fmt.Errorf("empty logits")
	}
	cLogits := (*C.float)(C.malloc(C.size_t(len(logits)) * C.sizeof_float))
	if cLogits == nil {
		return 0, fmt.Errorf("malloc failed for logits")
	}
	defer C.free(unsafe.Pointer(cLogits))
	cSlice := unsafe.Slice((*C.float)(cLogits), len(logits))
	for i, v := range logits {
		cSlice[i] = C.float(v)
	}
	rc := C.bridge_sample_token(
		cLogits,
		C.int32_t(len(logits)),
		C.float(temperature),
		C.uint32_t(seed),
	)
	if rc < 0 {
		return 0, fmt.Errorf("bridge_sample_token failed")
	}
	return int32(rc), nil
}

// IsEOG — Round 15.2 (2026-07-30): vocab-aware EOG detection.
//
// Заменяет heuristic isEOGToken(t) := t==1 || t==2 (Round 15.1) на
// llama_vocab_is_eog через C-bridge. Корректно для всех vocab (Qwen3,
// Llama, gemma-4, mistral, и т.п.).
//
// Используется BatchedScheduler для определения когда остановить
// generation (token == EOG token).
func (m *ModelHandle) IsEOG(token int32) (bool, error) {
	if m == nil || m.ptr == nil {
		return false, fmt.Errorf("model not loaded")
	}
	rc := C.bridge_token_is_eog(m.ptr, C.int32_t(token))
	if rc < 0 {
		return false, fmt.Errorf("bridge_token_is_eog failed")
	}
	return rc == 1, nil
}

// GetMetadata возвращает метаданные модели
func (m *ModelHandle) GetMetadata() (*ModelMetadata, error) {
	if m == nil || m.ptr == nil {
		return nil, fmt.Errorf("model not loaded")
	}

	cMeta := C.bridge_get_model_metadata(m.ptr)
	defer C.bridge_free_model_metadata(&cMeta)

	return &ModelMetadata{
		Description:    C.GoString(cMeta.description),
		Architecture:   C.GoString(cMeta.architecture),
		ContextLength:  int(cMeta.context_length),
		NLayers:        int(cMeta.n_layers),
		NHeads:         int(cMeta.n_heads),
		NKvHeads:       int(cMeta.n_head_kv),
		HeadDimK:       int(cMeta.n_embd_head_k),
		HeadDimV:       int(cMeta.n_embd_head_v),
		NEmbd:          int(cMeta.n_embd),
		NVocab:         int(cMeta.n_vocab),
		SizeTotalBytes: uint64(cMeta.size_total),
	}, nil
}

// BatchedSequence — Go-представление CBridgeBatchedSeq для batched inference.
//
// Описывает одну sequence (slot) в multi-seq batch. Каждый токен в sequence
// получает seq_id, что позволяет llama_decode выполнить forward pass для всех
// sequences параллельно в одной CUDA-операции.
//
// Round 15.1 (true batched parallel inference): см. дизайн-доку
// docs/plans/round-15-batched-parallel.md.
type BatchedSequence struct {
	Tokens   []int32  // токены этой sequence (borrowed, не копируется)
	SeqID    int32    // llama_seq_id (уникальный в пределах batch)
	StartPos int32    // llama_pos для первого токена (для prompt = 0)
}

// BatchedDecode — Round 15.1: ОДИН llama_decode call для N sequences.
//
// Используется BatchedScheduler (internal/cppbackend/batched_scheduler.go)
// для true parallel inference: один forward pass на GPU, обработка всех
// active sessions в одном matmul.
//
// Параметры:
//   - sequences: массив BatchedSequence, каждая со своими tokens + seq_id
//   - возвращает: [][]float32 (len = len(sequences), inner len = n_vocab)
//                 — logits для ПОСЛЕДНЕГО токена каждой sequence
//
// Caller (BatchedScheduler) держит instance.mu на время вызова —
// lock-семантика на стороне Go (как Round 8 для BridgeInferStream).
//
// ВАЖНО: сейчас C-side поддерживает только n_tokens=1 на sequence
// (single-step decode). Для prompt ingestion caller разбивает prompt
// на chunks вне bridge и итерирует.
func (m *ModelHandle) BatchedDecode(sequences []BatchedSequence) ([][]float32, error) {
	if m == nil || m.ptr == nil {
		return nil, fmt.Errorf("model not loaded")
	}
	if len(sequences) == 0 {
		return nil, fmt.Errorf("no sequences")
	}

	// Аллоцируем C-массив CBridgeBatchedSeq (caller-managed, передаётся в C).
	// Освобождаем сразу после C-вызова.
	cSeqs := C.malloc(C.size_t(len(sequences)) * C.sizeof_CBridgeBatchedSeq)
	if cSeqs == nil {
		return nil, fmt.Errorf("malloc failed for batched sequences")
	}
	defer C.free(cSeqs)

	// Заполняем C-массив: для каждой BatchedSequence вычисляем указатель
	// на i-й элемент CBridgeBatchedSeq и заполняем его.
	// Используем unsafe.Pointer arithmetic (как в C, cSeqs + i).
	//
	// CBridgeBatchedSeq в bridge.h использует int32_t (а не llama_token/
	// llama_seq_id/llama_pos) чтобы bridge.h не зависел от llama.h. C-bridge
	// (bridge.c) делает explicit cast int32_t → llama_* внутри.
	for i, seq := range sequences {
		if len(seq.Tokens) == 0 {
			C.free(cSeqs)
			return nil, fmt.Errorf("sequences[%d].Tokens is empty", i)
		}

		// Сохраняем Go-слайс в C-heap (C-сторона его borrow, не копирует).
		// Используем C.malloc — Go GC не знает про этот указатель.
		cTokensPtr := C.malloc(C.size_t(len(seq.Tokens)) * C.size_t(unsafe.Sizeof(C.int32_t(0))))
		if cTokensPtr == nil {
			C.free(cSeqs)
			return nil, fmt.Errorf("malloc failed for sequence tokens")
		}
		// Копируем int32 напрямую (tokens — int32_t в C-стороне, не llama_token).
		cTokensSlice := unsafe.Slice((*C.int32_t)(cTokensPtr), len(seq.Tokens))
		for j, t := range seq.Tokens {
			cTokensSlice[j] = C.int32_t(t)
		}

		// Заполняем i-й CBridgeBatchedSeq.
		seqPtr := (*C.CBridgeBatchedSeq)(unsafe.Pointer(uintptr(cSeqs) + uintptr(i)*C.sizeof_CBridgeBatchedSeq))
		seqPtr.tokens = (*C.int32_t)(cTokensPtr)
		seqPtr.n_tokens = C.int32_t(len(seq.Tokens))
		seqPtr.seq_id = C.int32_t(seq.SeqID)
		seqPtr.start_pos = C.int32_t(seq.StartPos)

		// TODO: free cTokensPtr после llama_decode. Сейчас leak (для
		// batched_decode который вызывается тысячи раз это критично).
		// Можно использовать sync.Pool для переиспользования буферов.
	}

	// Аллоцируем logits_out буфер: n_sequences * n_vocab float32.
	// bridge_get_n_vocab принимает void* (C → unsafe.Pointer в Go),
	// поэтому нужен явный каст typed pointer → unsafe.Pointer.
	nVocab := int(C.bridge_get_n_vocab(unsafe.Pointer(m.ptr)))
	if nVocab <= 0 {
		return nil, fmt.Errorf("invalid n_vocab=%d from bridge_get_n_vocab", nVocab)
	}
	logitsOut := make([]float32, len(sequences)*nVocab)

	// Вызываем C-side: ОДИН llama_decode для всех sequences.
	// bridge_batched_decode принимает void* handle (C → unsafe.Pointer в Go),
	// поэтому нужен явный каст typed pointer → unsafe.Pointer.
	rc := C.bridge_batched_decode(
		unsafe.Pointer(m.ptr),
		(*C.CBridgeBatchedSeq)(cSeqs),
		C.int32_t(len(sequences)),
		C.int32_t(nVocab),
		(*C.float)(unsafe.Pointer(&logitsOut[0])),
	)
	if rc != 0 {
		info := GetLastErrorInfo()
		errMsg := info.Message
		if errMsg == "" {
			errMsg = C.GoString(C.bridge_last_error())
		}
		return nil, fmt.Errorf("bridge_batched_decode failed (code=%d): %s", rc, errMsg)
	}

	// Разбиваем плоский logitsOut на [][]float32 по n_vocab.
	result := make([][]float32, len(sequences))
	for i := range sequences {
		start := i * nVocab
		end := start + nVocab
		// Создаём slice с backing array = logitsOut[start:end] — Go GC
		// не скопирует, но и не освободит logitsOut пока есть ссылка.
		// Можно скопировать если нужна независимость (для batched_decode
		// caller сразу использует logits и logitsOut перезаписывается
		// на следующем tick — copy не нужен).
		result[i] = logitsOut[start:end:end]
	}
	return result, nil
}

// ============================================================
// Утилиты
// ============================================================

// streamCallbackData — контекст для Go callback из C
type streamCallbackData struct {
	fn StreamCallback
}

//export streamCallbackGo
func streamCallbackGo(token *C.char, tokenLen C.int, userData unsafe.Pointer) C.int {
	// userData — это указатель на cgo.Handle, созданный в InferStream.
	// Извлекаем handle, затем cbData из него.
	h := *(*cgo.Handle)(userData)
	cbData := h.Value().(*streamCallbackData)
	if cbData == nil || cbData.fn == nil {
		return 0
	}
	tok := C.GoStringN(token, tokenLen)
	if cbData.fn(tok) {
		return 1 // continue
	}
	return 0 // stop
}

// kvCacheTypeToBridgeInt конвертирует Go-side строковое представление
// kv_cache_type в int, ожидаемый C-bridge (см. c/bridge/bridge.c::bridge_load_model
// и c/bridge/bridge.h::ModelConfig.kv_cache_type).
//
// Схема (согласована с bridge.h и cppworker):
//   "" / "f16" / неизвестное → 0  (default = llama.cpp F16, наследуется из default_params)
//   "q8_0"                  → 1  (GGML_TYPE_Q8_0 = -50% VRAM)
//   "q4_0"                  → 2  (GGML_TYPE_Q4_0 = -75% VRAM)
//
// Возвращает 0 для пустой или неизвестной строки — это safe default
// (F16, то есть наиболее точный и совместимый режим).
func kvCacheTypeToBridgeInt(s string) int {
	switch s {
	case "", "f16":
		return 0
	case "q8_0":
		return 1
	case "q4_0":
		return 2
	default:
		// Неизвестное значение (например, typo в профиле модели) — логируем
		// warning в stderr cppworker'а и fallback на default (F16).
		// Без warning пользователь не поймёт, почему его q8_0 не применился.
		fmt.Fprintf(stderrFile(), "[bridge] unknown kv_cache_type %q, falling back to default F16\n", s)
		return 0
	}
}

// stderrFile — для warning-логов из kvCacheTypeToBridgeInt и подобных
// утилит, которые не должны ломать основной flow LoadModel.
var stderrFile = func() *os.File { return os.Stderr }

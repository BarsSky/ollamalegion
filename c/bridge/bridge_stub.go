// Package bridge — заглушка для CGo bridge
//
// Собирается с тегом llama_stub, когда реальный llama.cpp не доступен.
// Возвращает "заглушечные" значения для всех операций.
//
// Build tag: llama_stub
//
// Использование:
//   go build -tags llama_stub -o cppworker ./cmd/cppworker

//go:build llama_stub
// +build llama_stub

package bridge

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// ModelHandle — заглушка
type ModelHandle struct {
	path string
}

// GPUDevice — информация о GPU (заглушка: CPU только)
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
	NPredict         int
	NKeep            int
	NBatch           int
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
	Seed             int
	Antiprompts      []string // стоп-последовательности (в stub-режиме игнорируются)
	StopSequences    []string // алиас Ollama для stop-последовательностей
	// NCtxOverride — per-request переопределение n_ctx (см. real bridge.go).
	// В stub-режиме не используется, но должен присутствовать для совместимости
	// типов между bridge.go (build tag !llama_stub) и bridge_stub.go (build tag llama_stub).
	NCtxOverride int
	// ClampedNPredict — true, если clampNPredictToFitContext уменьшил NPredict.
	// В stub-режиме не используется, но должен присутствовать для совместимости типов.
	ClampedNPredict bool `json:"-"`
	// ClampedNPredictOriginal — исходное значение NPredict до клампинга.
	// В stub-режиме не используется, но должен присутствовать для совместимости типов.
	ClampedNPredictOriginal int `json:"-"`
	// Round 13 (2026-07-28): sequence id for multi-slot batched inference.
	// 0 = single-slot legacy. > 0 = use slot seq_id. В stub-режиме не
	// используется, но должен присутствовать для совместимости типов
	// между bridge.go (build tag !llama_stub) и bridge_stub.go (build tag llama_stub).
	SeqId int
}

// DefaultGenerationParams возвращает параметры по умолчанию
func DefaultGenerationParams() GenerationParams {
	return GenerationParams{
		NPredict:         2048, // уменьшен с 4096 (Phase D.6): см. c/bridge/bridge.go
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

// ModelConfig — конфигурация загрузки модели (заглушка)
type ModelConfig struct {
	ModelPath         string
	NContext          int
	NBatch            int
	NThreads          int
	NThreadsBatch     int
	NGPULayers        int
	MainGPU           int
	FlashAttn         bool
	FlashAttnType     int
	NUMA              bool
	TensorSplit       []float32
	UseMmap           bool
	UseMlock          bool
	RopeFreqBase      float32
	RopeFreqScale     float32
	RopeScalingType   string
	RopeScalingFactor float32
	YarnExtFactor     float32
	YarnAttnFactor    float32
	YarnBetaFast      float32
	YarnBetaSlow      float32
	NoKVOffload       bool
	KVCacheType       string
	// Session 16 (2026-06-27): число параллельных sequences (n_parallel в llama.cpp).
	// В stub-режиме не используется, но должен присутствовать для совместимости
	// типов между bridge.go (build tag !llama_stub) и bridge_stub.go (build tag llama_stub).
	NParallel int
	RMSNormEps        float32
	NoMemoryMap       bool
	RPCBackend        string
	// Round 7: override-tensors (parallel slices; ignored in stub).
	OverrideTensors     []string
	OverrideTensorBufts []string
}

// DefaultModelConfig возвращает конфигурацию по умолчанию
func DefaultModelConfig(path string) ModelConfig {
	return ModelConfig{
		ModelPath:  path,
		NContext:   4096,
		NBatch:     512,
		NThreads:   0,
		NGPULayers: -1,
		MainGPU:    0,
		FlashAttn:  true,
		UseMmap:    true,
	}
}

// InferenceResult — результат инференса
type InferenceResult struct {
	Output   string
	Status   int
	ErrorMsg string
}

// ModelMetadata — метаданные модели
type ModelMetadata struct {
	Description    string
	Architecture   string
	ContextLength  int
	NLayers        int
	NHeads         int
	NKvHeads       int // NEW: GQA kv heads (added 2026-07-06)
	HeadDimK       int // NEW: K head dim (added 2026-07-06)
	HeadDimV       int // NEW: V head dim (added 2026-07-06)
	NEmbd          int
	NVocab         int
	SizeTotalBytes uint64
}

// StreamCallback — функция обратного вызова для стриминга
type StreamCallback func(token string) bool

// ChatMessage — stub-совместимая структура
type ChatMessage struct {
	Role    string
	Content string
}

// ErrNoChatTemplate — в stub режиме template недоступен
var ErrNoChatTemplate = fmt.Errorf("chat template not available in stub mode")

// ============================================================
// Структурированный error-info API (заглушка)
// ============================================================
// Эти определения должны присутствовать в обоих файлах (bridge.go и
// bridge_stub.go), чтобы balancer и cppworker собирались с любым build tag.

// ErrCode — коды структурированных ошибок (см. bridge.go). В stub-режиме
// C-bridge недоступен, но коды должны существовать для совместимости.
const (
	ErrCodeOK              = 0
	ErrCodeGeneric         = 1
	ErrCodeNCtxNeedsReload      = 2
	ErrCodePromptTooLong        = 3
	ErrCodeGPUOOM               = 4
	ErrCodeBadRequest           = 5
	ErrCodeInsufficientResources = 6
	// ErrCodeAborted (-100) — Round 31 #6: stub не возвращает abort
	// (нет реального C-bridge), но код должен существовать для совместимости
	// с bridge.go.
	ErrCodeAborted = -100
)

// ErrAborted — stub-sentinel для errors.Is() consistency с bridge.go.
var ErrAborted = fmt.Errorf("bridge: inference aborted by user (BRIDGE_ERR_ABORTED) [stub]")

// ErrNCtxNeedsReload — stub-sentinel. В stub-режиме не выбрасывается,

// BridgeErrorInfo — заглушка. В stub-режиме всегда возвращается OK.
type BridgeErrorInfo struct {
	Code         int
	CurrentNCtx  int
	RequiredNCtx int
	ActualTokens int
	NPredict     int
	NCtxOverride int
	MaxVRAMNCtx  int
	Message      string
}

// GetLastErrorInfo — stub-реализация. Всегда возвращает OK (stub не падает).
func GetLastErrorInfo() *BridgeErrorInfo {
	return &BridgeErrorInfo{Code: ErrCodeOK}
}

// ErrNCtxNeedsReload — stub-sentinel. В stub-режиме не выбрасывается,
// но должен существовать для совместимости типов.
var ErrNCtxNeedsReload = fmt.Errorf("n_ctx exceeds loaded model; auto-reload may be possible (stub)")

// ErrPromptTooLong — stub-sentinel.
var ErrPromptTooLong = fmt.Errorf("prompt + n_predict exceeds n_ctx (stub)")

// ApplyChatTemplate — stub-реализация
func (m *ModelHandle) ApplyChatTemplate(system string, messages []ChatMessage, addAss bool) (string, error) {
	return "", ErrNoChatTemplate
}

// GetChatTemplate — stub-реализация
func (m *ModelHandle) GetChatTemplate() (string, error) {
	return "", ErrNoChatTemplate
}

// ApplyChatTemplateWithThinking — Round 14a (2026-07-28): stub-реализация.
// В stub-режиме нет реального llama.cpp, поэтому всегда возвращает
// ErrNoChatTemplate. Совпадает с поведением GetChatTemplate.
func (m *ModelHandle) ApplyChatTemplateWithThinking(
	chatTemplateOverride string,
	messages []ChatMessage,
	enableThinking bool,
	addGenerationPrompt bool,
) (string, bool, error) {
	return "", false, ErrNoChatTemplate
}

// ============================================================
// Bridge initialization (stub)
// ============================================================

var initDone bool

// Init инициализирует bridge (заглушка)
func Init() error {
	initDone = true
	return nil
}

// Version возвращает версию bridge (stub)
func Version() string {
	return fmt.Sprintf("llama_stub_go%s_%s", runtime.Version()[2:], runtime.GOARCH)
}

// GetGPUCount возвращает количество GPU (stub: 0 — CPU only)
func GetGPUCount() int {
	return 0
}

// GetGPUInfo возвращает информацию о GPU (stub: всегда ошибка)
func GetGPUInfo(index int) (*GPUDevice, error) {
	return nil, fmt.Errorf("GPU not available in stub mode (llama_stub)")
}

// ============================================================
// Model management (stub)
// ============================================================

// LoadModel загружает модель (stub: всегда успех с заглушкой)
func LoadModel(cfg ModelConfig) (*ModelHandle, error) {
	return &ModelHandle{path: cfg.ModelPath}, nil
}

// FreeModel выгружает модель (stub: no-op)
func (m *ModelHandle) FreeModel() {
	// no-op
}

// ============================================================
// Round 31 #6 (2026-08-09): Abort API — stub-реализации
// ============================================================
//
// В stub-режиме нет реального C-bridge, поэтому abort — no-op.
// Сигнатуры и sentinel-семантика идентичны bridge.go для совместимости
// с cppworker кодом, который собирается с обоими build tag.

// RequestAbort — stub: no-op (нет реального C-bridge для пометки).
// Не возвращает ошибку — handlerы в stub-режиме работают без abort API.
func RequestAbort(model *ModelHandle) error {
	return nil
}

// RequestAbortAll — stub: no-op.
func RequestAbortAll() error {
	return nil
}

// IsAborted — stub: всегда false (нет abort API в stub-режиме).
func IsAborted(model *ModelHandle) bool {
	return false
}

// ============================================================
// Inference (stub)
// ============================================================

// Infer выполняет синхронный инференс (stub)
func (m *ModelHandle) Infer(prompt string, params GenerationParams) (*InferenceResult, error) {
	// Round 8 (2026-07-28): если тест задал stubInferDelay через SetStubInferDelay —
	// имитируем долгий inference (нужно для теста concurrent serialization в
	// internal/cppbackend/backend_test.go). В обычной работе delay=0.
	if d := getStubInferDelay(); d > 0 {
		time.Sleep(d)
	}
	output := fmt.Sprintf("[llama_stub] Echo: %s\n\n(Stub mode — no real llama.cpp)\n\nGenerated with %d max tokens, temperature %.2f",
		prompt[:min(len(prompt), 100)],
		params.NPredict,
		params.Temperature)

	return &InferenceResult{
		Output:   output,
		Status:   0,
		ErrorMsg: "",
	}, nil
}

// stubInferDelay — искусственная задержка в stub.Infer, нужна для теста
// concurrent serialization (Round 8 BUGFIX: Generate должен удерживать
// inst.mu на всём инференсе, иначе race в llama_decode).
//
// Доступ через SetStubInferDelay/getStubInferDelay (atomic.Int64 наносекунды).
// Используется ТОЛЬКО в тестах (build tag llama_stub).
var stubInferDelay atomic.Int64

// SetStubInferDelay — устанавливает задержку для stub.Infer.
// Возвращает предыдущее значение для восстановления через defer.
func SetStubInferDelay(d time.Duration) time.Duration {
	prev := stubInferDelay.Swap(int64(d))
	return time.Duration(prev)
}

// getStubInferDelay — текущее значение задержки (thread-safe).
func getStubInferDelay() time.Duration {
	return time.Duration(stubInferDelay.Load())
}

// stubEmptyOutput — флаг для тестов: если true, InferStream НЕ вызывает
// callback ни разу (имитирует «модель остановилась на antiprompt сразу
// или вернула 0 токенов из-за проблемы с chat template»). Используется
// в регрессионных тестах cmd/cppworker/empty_stream_response_test.go.
//
// Доступ к флагу только через SetStubEmptyOutput/GetStubEmptyOutput для
// thread-safety (atomic.Bool доступна с Go 1.19+).
var stubEmptyOutput atomic.Bool

// SetStubEmptyOutput — включает/выключает режим «пустой output» для
// stub bridge. Используется ТОЛЬКО в тестах (build tag llama_stub).
// Возвращает предыдущее значение, чтобы тесты могли восстановить его
// через defer.
func SetStubEmptyOutput(enabled bool) bool {
	return stubEmptyOutput.Swap(enabled)
}

// GetStubEmptyOutput — текущее значение флага stubEmptyOutput.
func GetStubEmptyOutput() bool {
	return stubEmptyOutput.Load()
}

// stubEmitTokens — slice токенов, которые должен эмитить stub InferStream.
// Если nil/empty — используется default stub tokens.
// Используется для имитации конкретных сценариев (например, "модель эмитит
// только <end_of_turn> и останавливается" для тестов cleanFinalContent-aware
// empty check).
//
// Доступ через SetStubEmitTokens (thread-safe через mutex).
var (
	stubEmitTokensMu sync.Mutex
	stubEmitTokens   []string
)

// SetStubEmitTokens — устанавливает токены для эмитации в stub InferStream.
// Если передать nil/empty — сбрасывает на default stub tokens.
// Используется ТОЛЬКО в тестах. Возвращает предыдущее значение.
//
// Пример: SetStubEmitTokens([]string{"<end_of_turn>"}) — имитирует,
// что модель единственным токеном сгенерировала antiprompt и остановилась.
func SetStubEmitTokens(tokens []string) []string {
	stubEmitTokensMu.Lock()
	defer stubEmitTokensMu.Unlock()
	prev := stubEmitTokens
	stubEmitTokens = tokens
	return prev
}

// getStubEmitTokens — thread-safe получение текущего набора токенов.
func getStubEmitTokens() []string {
	stubEmitTokensMu.Lock()
	defer stubEmitTokensMu.Unlock()
	if len(stubEmitTokens) == 0 {
		return nil
	}
	// Возвращаем копию, чтобы caller не мог мутировать оригинал.
	out := make([]string, len(stubEmitTokens))
	copy(out, stubEmitTokens)
	return out
}

// defaultStubTokens — стандартный набор токенов для stub (когда не задан SetStubEmitTokens).
var defaultStubTokens = []string{
	"\n[llama_stub] ",
	"Stub ",
	"mode ",
	"— ",
	"no ",
	"real ",
	"llama.cpp\n\n",
}

// InferStream выполняет стриминг-инференс (stub)
func (m *ModelHandle) InferStream(prompt string, params GenerationParams, callback StreamCallback) error {
	// Режим «пустой output» для тестов: callback не вызывается ни разу,
	// ошибка не возвращается — это имитирует случай, когда модель
	// успешно завершила генерацию (Status=0), но не выдала ни одного
	// токена (например, antiprompt сработал на первом же шаге).
	if stubEmptyOutput.Load() {
		return nil
	}

	// Если тест задал специфичные токены через SetStubEmitTokens — эмитим их.
	// Иначе используем default stub tokens.
	tokens := getStubEmitTokens()
	if tokens == nil {
		tokens = defaultStubTokens
	}

	for _, tok := range tokens {
		if !callback(tok) {
			return nil // cancelled
		}
	}
	return nil
}

// GetEmbeddings получает эмбеддинги (stub)
func (m *ModelHandle) GetEmbeddings(text string) ([]float32, error) {
	// Возвращаем пустой эмбеддинг размером 128
	return make([]float32, 128), nil
}

// CountTokens возвращает грубую оценку числа токенов в stub-режиме.
func (m *ModelHandle) CountTokens(text string) int {
	if text == "" {
		return 0
	}
	return len([]rune(text)) / 4
}

// Tokenize возвращает ошибку в stub-режиме (нет словаря).
// Round 15.1: в stub-режиме batched path не используется; сигнатура нужна
// для компиляции пакета.
func (m *ModelHandle) Tokenize(text string) ([]int32, error) {
	return nil, fmt.Errorf("Tokenize not available in llama_stub build")
}

// BatchedSequence — stub-представление CBridgeBatchedSeq (Round 15.1).
// В stub-режиме BatchedScheduler не используется, но тип нужен для компиляции.
type BatchedSequence struct {
	Tokens   []int32
	SeqID    int32
	StartPos int32
}

// BatchedDecode возвращает ошибку в stub-режиме (нет llama.cpp).
func (m *ModelHandle) BatchedDecode(sequences []BatchedSequence) ([][]float32, error) {
	return nil, fmt.Errorf("BatchedDecode not available in llama_stub build")
}

// SampleToken возвращает ошибку в stub-режиме (нет RNG/llama.cpp).
// Round 15.2: в stub-режиме BatchedScheduler не используется, но
// сигнатура нужна для компиляции пакета.
func (m *ModelHandle) SampleToken(logits []float32, temperature float32, seed uint32) (int32, error) {
	return 0, fmt.Errorf("SampleToken not available in llama_stub build")
}

// IsEOG возвращает false в stub-режиме (нет vocab). Round 15.2.
func (m *ModelHandle) IsEOG(token int32) (bool, error) {
	return false, fmt.Errorf("IsEOG not available in llama_stub build")
}

// TokenToPiece возвращает пустую строку в stub-режиме (нет словаря).
// Round 15.1: в stub-режиме batched infer path не используется
// (cppworker собирается с реальным llama.cpp через cgo), но сигнатура
// нужна чтобы пакет компилировался.
func (m *ModelHandle) TokenToPiece(token int32) string {
	return ""
}

// GetMetadata возвращает метаданные модели (stub)
func (m *ModelHandle) GetMetadata() (*ModelMetadata, error) {
	return &ModelMetadata{
		Description:    "Stub model (llama_stub build tag)",
		Architecture:   "llama_stub",
		ContextLength:  4096,
		NLayers:        32,
		NHeads:         32,
		NEmbd:          4096,
		NVocab:         32000,
		SizeTotalBytes: 0, // неизвестно
	}, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
// Package bridge — заглушка для CGo bridge
//
// Собирается с тегом llama_stub, когда реальный llama.cpp не доступен.
// Возвращает "заглушечные" значения для всех операций.
//
// Build tag: llama_stub
//
// Использование:
//   go build -tags llama_stub -o cppworker ./cmd/cppworker
//
//go:build llama_stub
// +build llama_stub

package bridge

import (
	"fmt"
	"runtime"
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
	RMSNormEps        float32
	NoMemoryMap       bool
	RPCBackend        string
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
	ErrCodeNCtxNeedsReload = 2
	ErrCodePromptTooLong   = 3
	ErrCodeGPUOOM          = 4
	ErrCodeBadRequest      = 5
)

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
// Inference (stub)
// ============================================================

// Infer выполняет синхронный инференс (stub)
func (m *ModelHandle) Infer(prompt string, params GenerationParams) (*InferenceResult, error) {
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

// InferStream выполняет стриминг-инференс (stub)
func (m *ModelHandle) InferStream(prompt string, params GenerationParams, callback StreamCallback) error {
	// Симулируем стриминг: отправляем токены по одному
	tokens := []string{
		"\n[llama_stub] ",
		"Stub ",
		"mode ",
		"— ",
		"no ",
		"real ",
		"llama.cpp\n\n",
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
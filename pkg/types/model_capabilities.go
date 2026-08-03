package types

import (
	"strconv"
	"strings"
)

// ModelCapabilities — набор возможностей модели, известных балансеру
// и cppworker. Используется для:
//  1. Auto-detect по имени модели (whitelist префиксов)
//  2. Per-model override через load-with-params
//  3. HTTP response headers (X-Model-*) для клиента
//  4. Routing decisions (e.g. reasoning → reasoning_content field)
//
// Round 18 P0.1 (2026-08-03). Заменяет ad-hoc IsReasoningModel() / HasTools
// checks разбросанные по коду. Single source of truth.
type ModelCapabilities struct {
	// Reasoning — модель нативно эмитит reasoning (<think>, <reasoning>, etc).
	// Если true, balancer парсит вывод и отдаёт reasoning_content отдельным полем.
	// Если false, может быть включён lazy-detect (Round 17 Layer 3):
	// при первом <think> в output — auto-enable.
	Reasoning bool `json:"reasoning"`

	// Tools — модель поддерживает function/tool calling.
	// OpenAI /api/chat или Ollama /api/chat с tools=[...] проксируются as-is.
	// Если false, balancer reject'ит запросы с tools (или strip'ает их).
	Tools bool `json:"tools"`

	// Vision — модель принимает изображения в input (multimodal).
	// OpenAI /v1/chat/completions с content=[{type:"image_url",...}].
	// Если false, balancer reject'ит (или strip'ает images) с предупреждением.
	Vision bool `json:"vision"`

	// Audio — модель принимает/эмитит audio (TTS/STT).
	// Пока не реализовано в cppworker (Round 22 BUG).
	Audio bool `json:"audio"`

	// Embeddings — модель может быть использована для /api/embed.
	// Большинство LLM это умеют (qwen3, llama3, etc). Encoder-only модели (bge, e5)
	// тоже. Vision LLM обычно НЕ умеют.
	Embeddings bool `json:"embeddings"`

	// MaxContext — максимальный размер контекста для этой модели.
	// Приходит из GGUF header (general.context_length).
	// 0 = unknown (fallback to cppworker default).
	MaxContext int `json:"maxContext"`

	// Architecture — название архитектуры из GGUF header (qwen3, llama, gpt2, ...).
	Architecture string `json:"architecture"`

	// Source — откуда получены capabilities. Для отладки.
	// "auto-detect", "manual-override", "cppworker-runtime", "unknown".
	Source string `json:"source"`
}

// CapabilitiesToHeaders конвертирует capabilities в HTTP-заголовки для
// ответа клиенту. Используется balancer'ом в proxyRequest.
// Формат: X-Model-Capabilities=reasoning,tools; X-Model-Max-Context=32768
func (c ModelCapabilities) Headers() map[string]string {
	h := make(map[string]string)
	caps := make([]string, 0, 4)
	if c.Reasoning {
		caps = append(caps, "reasoning")
	}
	if c.Tools {
		caps = append(caps, "tools")
	}
	if c.Vision {
		caps = append(caps, "vision")
	}
	if c.Audio {
		caps = append(caps, "audio")
	}
	if c.Embeddings {
		caps = append(caps, "embeddings")
	}
	if len(caps) > 0 {
		h["X-Model-Capabilities"] = strings.Join(caps, ",")
	} else {
		h["X-Model-Capabilities"] = "none"
	}
	if c.MaxContext > 0 {
		h["X-Model-Max-Context"] = strconv.Itoa(c.MaxContext)
	}
	if c.Architecture != "" {
		h["X-Model-Architecture"] = c.Architecture
	}
	return h
}

// DefaultCapabilitiesForModelName — auto-detect по имени модели.
// Используется когда cppworker ещё не ответил (cold start) или для
// планирования routing до фактической загрузки.
//
// Whitelist префиксов и patterns (lower-case, no .gguf extension).
// При конфликте patterns (qwen3 vs qwen3-instruct) — более специфичный
// побеждает (instruct переопределяет base).
//
// Стратегия (3 фазы):
//  1. Family defaults (qwen3 base, deepseek-r1, gemma-4) → Reasoning=true
//  2. Explicit non-reasoning suffix (-instruct, -it) → Reasoning=false
//  3. Explicit reasoning suffix (-thinking, -r1) → Reasoning=true
//
// Конфликты разрешаются: фаза 2/3 имеют приоритет над фазой 1.
func DefaultCapabilitiesForModelName(name string) ModelCapabilities {
	lower := strings.ToLower(name)
	caps := ModelCapabilities{
		Embeddings: true, // most LLMs support embeddings
		Source:     "auto-detect",
	}

	// === Phase 1: Family defaults — Reasoning=true ===
	// qwen3 (base = thinking), deepseek-r1 family, gemma-4, kimi-k2, etc.
	if hasAnyPrefix(lower, []string{
		"qwen3", "qwen3.5", "qwen3.6",
		"qwen3-thinking", "qwen3_thinking",
		"deepseek-r1", "deepseek_r1", "deepseekr1",
		"kimi-k2", "kimi_k2", "kimik2",
		"gemma4", "gemma-4", // gemma-4 family = reasoning (E4B-it, etc.)
		"seed-oss", "seedoss",
		"apriel", "smallthinker",
		"step3.5", "step-3.5",
	}) {
		caps.Reasoning = true
	}

	// === Phase 2: Explicit non-reasoning suffix (-instruct, -it) ===
	// qwen3-instruct, gemma-4-it (instruct variant) → NO reasoning
	if strings.Contains(lower, "instruct") || strings.Contains(lower, "-it") {
		caps.Reasoning = false
	}

	// === Phase 3: Explicit reasoning suffix (-thinking, -r1, -reasoning) ===
	// Forces reasoning=true even if Phase 1 missed it.
	if hasAnySuffix(lower, []string{
		"-thinking", ":thinking", "_thinking", "/thinking", ".thinking",
		"-reasoning", ":reasoning",
		"-r1", "_r1",
	}) {
		caps.Reasoning = true
	}

	// === Vision detection ===
	// Vision families: llava, moondream, minicpm-v, qwen-vl, gemma3 (NOT gemma4),
	// pixtral, internvl, llama-3.2-vision.
	if hasAnyPrefix(lower, []string{
		"llava", "moondream", "minicpm-v",
		"qwen-vl", "qwen2-vl", "qwen2.5-vl",
		"gemma3", "gemma-3", // gemma3 = vision, gemma4 = text-only reasoning
		"pixtral", "internvl", "llama-3.2-vision",
	}) {
		caps.Vision = true
	}
	// qwen3 НЕ vision (qwen2.5-vl, qwen-vl — да, но не qwen3)
	if strings.HasPrefix(lower, "qwen3") && !strings.Contains(lower, "vl") {
		caps.Vision = false
	}

	// === Tools detection ===
	// Большинство современных instruct моделей поддерживают tools.
	// Исключения: encoder-only (bge, e5) и устаревшие (gpt2, falcon).
	if !hasAnyPrefix(lower, []string{
		"bge-", "e5-", "gte-", "nomic-embed",
		"gpt2", "falcon", "mpt", "rwkv", "starcoder",
	}) {
		caps.Tools = true
	}

	// === Architecture detection (rough) ===
	switch {
	case strings.HasPrefix(lower, "qwen"):
		caps.Architecture = "qwen"
	case strings.HasPrefix(lower, "llama"):
		caps.Architecture = "llama"
	case strings.HasPrefix(lower, "gemma"):
		caps.Architecture = "gemma"
	case strings.HasPrefix(lower, "deepseek"):
		caps.Architecture = "deepseek"
	case strings.HasPrefix(lower, "mistral") || strings.HasPrefix(lower, "mixtral"):
		caps.Architecture = "mistral"
	case strings.HasPrefix(lower, "llava"):
		caps.Architecture = "llava"
	case strings.HasPrefix(lower, "phi"):
		caps.Architecture = "phi"
	case strings.HasPrefix(lower, "command") || strings.HasPrefix(lower, "cohere"):
		caps.Architecture = "cohere"
	}

	return caps
}

// CapabilitiesFromModelInfo — extract из cppworker ModelInfo (loaded state).
// Используется когда cppworker уже загрузил модель и мы знаем её реальные
// capabilities. Source = "cppworker-runtime" (highest trust).
//
// Принимает поля напрямую (не тип) чтобы избежать circular import
// между pkg/types и internal/cppbackend.
func CapabilitiesFromModelInfo(name string, reasoningEnabled bool, architecture string, contextSize, ggufContextLength int) ModelCapabilities {
	caps := DefaultCapabilitiesForModelName(name)
	caps.Reasoning = reasoningEnabled
	caps.Architecture = architecture
	caps.MaxContext = contextSize
	if caps.MaxContext == 0 {
		caps.MaxContext = ggufContextLength
	}
	caps.Source = "cppworker-runtime"
	return caps
}

// hasAnyPrefix — true если s starts with any of the given prefixes.
func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// hasAnySuffix — true если s ends with any of the given suffixes.
func hasAnySuffix(s string, suffixes []string) bool {
	for _, s2 := range suffixes {
		if strings.HasSuffix(s, s2) {
			return true
		}
	}
	return false
}

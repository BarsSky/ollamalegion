// reasoning_content.go — извлечение reasoning_content (think-блоков) из
// выхода reasoning-моделей (qwen3.5, qwen3.6 MoE, deepseek-r1, kimi-k2,
// gemma-4-thinking и т.п.).
//
// Зачем это нужно:
//   - llama.cpp (c/llama.cpp/common/chat-peg-parser.cpp) уже умеет парсить
//     think-блоки встроенным chat template engine. Однако наш cppworker
//     использует либо bridge_apply_chat_template (если в GGUF есть
//     tokenizer.chat_template), либо naive fallback, и в обоих случаях
//     получает от C-моста "сырой" текст, в котором `` остаётся
//     встроенным в общий output. Клиенты (OpenWebUI, Cline, Roo Code)
//     ожидают отдельное поле `reasoning_content` (аналог Deepseek API),
//     чтобы можно было скрыть/развернуть цепочку рассуждений.
//
//   - Признак reasoning-модели задаётся в имени файла (substring) или в
//     архитектуре из GGUF. Список — в ReasoningArchPrefixes (env:
//     CPPWORKER_REASONING_ARCHS).
//
// Используется в:
//   - writeOpenAIChatStream      — incremental SSE (delta.reasoning_content + delta.content)
//   - handleV1ChatCompletions    — non-stream (message.reasoning_content)
//   - handleOllamaChat           — Ollama /api/chat (message.reasoning)
//   - handleOllamaGenerate       — Ollama /api/generate (response.reasoning)
//   - debug snapshot             — поля reasoning_chars / reasoning_tokens
//
// Тесты: cmd/cppworker/reasoning_content_test.go
package main

import (
	"os"
	"strconv"
	"strings"
	"sync"
)

// ============================================================
// Конфигурация reasoning-архитектур
// ============================================================

// ReasoningArchPrefixes — подстроки в имени файла модели или в имени
// модели (req.Model), которые маркируют её как reasoning-модель.
// Регистр игнорируется. Применяется в дополнение к архитектуре из GGUF.
//
// IMPORTANT: голое "qwen3" НЕ включено, потому что Qwen3 выпускается и в
// thinking и в non-thinking вариантах, и имя файла часто не различает
// (например "qwen3-32b-Q4_K_M.gguf" может быть оба). По умолчанию голый
// qwen3 НЕ считается reasoning-моделью. Для включения нужно либо:
//   - Имя модели содержит ".5", ".6" (qwen3.5 / qwen3.6) — reasoning by default.
//   - Имя модели содержит "moe" / "A3B" (Qwen3-MoE) — has thinking mode enabled.
//   - Имя модели содержит явный маркер "-thinking" или ":thinking".
//   - Установить env CPPWORKER_REASONING_ARCHS=qwen3 для force-enable.
var ReasoningArchPrefixes = []string{
	"qwen3.5", "qwen3.6", "qwen3.5moe", "qwen35moe", "qwen35",
	"qwen3moe", "qwen3-thinking", "qwen3_thinking",
	// "-thinking" с ведущим дефисом — глобальный маркер любой thinking-варианта
	// модели (qwen3-32b-thinking, kimi-k2-thinking, и т.д.). Голое "thinking"
	// не используем, чтобы не ловить случайные совпадения вроде "anything".
	"-thinking", ":thinking",
	"deepseek-r1", "deepseek_r1", "deepseekr1",
	"kimi-k2", "kimi_k2", "kimik2",
	"gemma4", "gemma-4",
	"seed-oss", "seedoss",
	"apriel",
	"smallthinker",
	"step3.5", "step-3.5",
}

// reasoningArchsEnvOnce — потокобезопасная инициализация списка архитектур
// из env CPPWORKER_REASONING_ARCHS (через запятую).
var (
	reasoningArchsEnvOnce sync.Once
	reasoningArchsEnvList []string
)

// getReasoningArchList — список архитектур, расширенный env CPPWORKER_REASONING_ARCHS.
// Дополнения из env добавляются к дефолтному списку, не заменяя его.
func getReasoningArchList() []string {
	reasoningArchsEnvOnce.Do(func() {
		raw := strings.TrimSpace(os.Getenv("CPPWORKER_REASONING_ARCHS"))
		if raw == "" {
			return
		}
		for _, p := range strings.Split(raw, ",") {
			p = strings.TrimSpace(p)
			if p != "" {
				reasoningArchsEnvList = append(reasoningArchsEnvList, strings.ToLower(p))
			}
		}
	})
	if len(reasoningArchsEnvList) == 0 {
		return ReasoningArchPrefixes
	}
	out := make([]string, 0, len(ReasoningArchPrefixes)+len(reasoningArchsEnvList))
	out = append(out, ReasoningArchPrefixes...)
	out = append(out, reasoningArchsEnvList...)
	return out
}

// IsReasoningModel возвращает true, если имя модели содержит одну из
// reasoning-архитектур (см. ReasoningArchPrefixes). Регистр игнорируется.
//
// Примеры:
//   - "Qwen3.6-35B-A3B-Uncensored-HauhauCS-Aggressive-Q4_K_M.gguf" → true
//   - "deepseek-r1-distill-qwen-7b.Q4_K_M.gguf"                    → true
//   - "gemma-4-E4B-it-Q4_K_M.gguf"                                  → true (gemma-4 = thinking)
//   - "llama-3.1-8b-instruct.Q4_K_M.gguf"                           → false
func IsReasoningModel(modelName string) bool {
	if modelName == "" {
		return false
	}
	lower := strings.ToLower(modelName)
	for _, p := range getReasoningArchList() {
		if strings.Contains(lower, strings.ToLower(p)) {
			return true
		}
	}
	return false
}

// ============================================================
// Разбор think-блоков
// ============================================================

// Тег начала/конца think-блока. Qwen, DeepSeek-R1, Kimi-K2, gemma-4-thinking,
// Seed-OSS и др. используют `<think>`/`</think>`. Некоторые экспериментальные
// модели (gemma-4-it) могут использовать `<thinking>`/`</thinking>`. Поддерживаем оба.
const (
	thinkStart  = "<think>"
	thinkEnd    = "</think>"
	thinkStart2 = "<thinking>"  // альтернативный
	thinkEnd2   = "</thinking>" // альтернативный
)

// openThinkTag ищет ближайший открывающий тег в позиции ≥ from.
// Возвращает (startPos, endPos, isThinking) — startPos это индекс символа '<',
// endPos — индекс после '>'. isThinking = true для `<thinking>`, false для `<think>`.
func openThinkTag(s string, from int) (int, int, bool) {
	if from < 0 {
		from = 0
	}
	idx1 := strings.Index(s[from:], thinkStart)
	idx2 := strings.Index(s[from:], thinkStart2)
	switch {
	case idx1 < 0 && idx2 < 0:
		return -1, -1, false
	case idx1 < 0:
		return from + idx2, from + idx2 + len(thinkStart2), true
	case idx2 < 0:
		return from + idx1, from + idx1 + len(thinkStart), false
	case idx1 < idx2:
		return from + idx1, from + idx1 + len(thinkStart), false
	default:
		return from + idx2, from + idx2 + len(thinkStart2), true
	}
}

// closeThinkTag ищет соответствующий закрывающий тег в позиции ≥ from.
// Альтернативный тег `</thinking>` может закрывать только блок `<thinking>`,
// основной `</think>` — только блок `<think>` (но на практике gemma-4
// использует один из них консистентно).
func closeThinkTag(s string, from int, isThinking bool) int {
	if isThinking {
		if i := strings.Index(s[from:], thinkEnd2); i >= 0 {
			return from + i + len(thinkEnd2)
		}
		if i := strings.Index(s[from:], thinkEnd); i >= 0 {
			return from + i + len(thinkEnd)
		}
		return -1
	}
	if i := strings.Index(s[from:], thinkEnd); i >= 0 {
		return from + i + len(thinkEnd)
	}
	if i := strings.Index(s[from:], thinkEnd2); i >= 0 {
		return from + i + len(thinkEnd2)
	}
	return -1
}

// SplitReasoningContent разделяет строку на (reasoning, content) по think-блокам.
//
// Семантика:
//   - Все символы до первого открывающего тега считаются видимым контентом.
//   - Текст между `<think>` и `</think>` (или альтернативной парой) считается
//     reasoning (без самих тегов).
//   - Текст после `</think>` — content.
//   - Если модель эмитит несколько `<think>…</think>` подряд — reasoning
//     конкатенируется, content вычисляется как "всё остальное".
//   - Незакрытый `<think>` (нет соответствующего `</think>`) — текст **между**
//     `<think>` и концом строки считается reasoning (best-effort, чтобы не
//     терять данные).
//   - Если в строке нет ни одного открывающего тега — возвращается ("", s, false).
//
// Примеры:
//
//	SplitReasoningContent("hello")                                    → ("",     "hello",              false)
//	SplitReasoningContent("<think>r1</think>ans")                     → ("r1",   "ans",                true)
//	SplitReasoningContent("<think>r1</think><think>r2</think>final")  → ("r1r2", "final",              true)
//	SplitReasoningContent("<think>незакрыто")                         → ("незак-","",                  true)
//	SplitReasoningContent("<think><thinking>x</thinking></think>")    → ("x",    "",                   true)
func SplitReasoningContent(s string) (reasoning, content string, hasReasoning bool) {
	if s == "" {
		return "", "", false
	}
	var sbReasoning strings.Builder
	var sbContent strings.Builder
	pos := 0
	foundAny := false
	for pos < len(s) {
		tagStart, tagEnd, isThinking := openThinkTag(s, pos)
		if tagStart < 0 {
			sbContent.WriteString(s[pos:])
			break
		}
		// Текст до открывающего тега → content.
		sbContent.WriteString(s[pos:tagStart])
		// Ищем закрывающий тег после tagEnd.
		closePos := closeThinkTag(s, tagEnd, isThinking)
		if closePos < 0 {
			// Незакрытый блок: только тело (от tagEnd до конца) → reasoning.
			// Сам `<think>` в reasoning не включаем (это технический маркер).
			sbReasoning.WriteString(s[tagEnd:])
			foundAny = true
			break
		}
		tagLen := len(thinkEnd)
		if isThinking {
			tagLen = len(thinkEnd2)
		}
		bodyStart := tagEnd
		bodyEnd := closePos - tagLen
		sbReasoning.WriteString(s[bodyStart:bodyEnd])
		foundAny = true
		pos = closePos
	}
	return sbReasoning.String(), sbContent.String(), foundAny
}

// ============================================================
// Incremental state machine для streaming
// ============================================================

// ReasoningStreamState — потокобезопасное состояние incremental-парсера
// для streaming. cppworker создаёт один экземпляр на запрос, передаёт
// каждый chunk в Feed(), и периодически читает Snapshot().
//
// Использование:
//
//	st := NewReasoningStreamState()
//	for each token from C-bridge:
//	    reasoningDelta, contentDelta := st.Feed(token)
//	    // эмитим SSE chunk с delta.reasoning_content (если reasoningDelta!="")
//	    // и delta.content (если contentDelta!="")
//	snap := st.Snapshot() // для debug endpoint
//
// Реализация: накапливаем полный текст в `full`, на каждом Feed заново
// разделяем его через SplitReasoningContent и возвращаем только **дельту**
// относительно предыдущего разделения. Это O(N²) на длину потока, но для
// типичных 2-8K токенов абсолютно приемлемо (~64M операций на 8K токенов,
// <100ms на CPU). Зато полностью устраняет race-condition с разрезанными
// тегами и pending-буфером.
type ReasoningStreamState struct {
	mu           sync.Mutex
	full         strings.Builder // весь полученный текст
	reasoningAll string          // reasoning-часть последнего split
	contentAll   string          // content-часть последнего split
}

// NewReasoningStreamState создаёт новый incremental-парсер.
func NewReasoningStreamState() *ReasoningStreamState {
	return &ReasoningStreamState{}
}

// Feed принимает очередной chunk от C-bridge и возвращает
// (reasoningDelta, contentDelta) — что добавилось в каждой категории
// относительно последнего вызова Feed/Finalize.
func (st *ReasoningStreamState) Feed(chunk string) (reasoningDelta, contentDelta string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if chunk == "" {
		return "", ""
	}
	st.full.WriteString(chunk)
	reasoning, content, _ := SplitReasoningContent(st.full.String())
	prevRLen := len(st.reasoningAll)
	prevCLen := len(st.contentAll)
	// Безопасное вычисление дельты: reasoning/content могут стать короче
	// из-за прихода нового `</think>` (тогда content бывшего reasoning
	// перетекает в reasoning как content'овая часть после закрытия).
	// В этом случае дельта = "" (нового content нет, всё что было — повтор).
	if len(reasoning) >= prevRLen {
		reasoningDelta = reasoning[prevRLen:]
	} else {
		reasoningDelta = ""
	}
	if len(content) >= prevCLen {
		contentDelta = content[prevCLen:]
	} else {
		contentDelta = ""
	}
	st.reasoningAll = reasoning
	st.contentAll = content
	return reasoningDelta, contentDelta
}

// Finalize завершает парсинг. Вызывается после EOS. Для нашей реализации
// (с полным буфером и перерасчётом) Finalize не делает ничего сверх Feed —
// просто возвращает последние дельты (которые равны "" после Feed).
//
// Метод оставлен для совместимости с API и для будущих оптимизаций
// (например, eager-flush pending-чанков).
func (st *ReasoningStreamState) Finalize() (reasoningDelta, contentDelta string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	return "", ""
}

// ReasoningSnapshot — снимок состояния incremental-парсера для
// debug endpoint.
type ReasoningSnapshot struct {
	ReasoningChars int    `json:"reasoning_chars"`
	ContentChars   int    `json:"content_chars"`
	HasReasoning   bool   `json:"has_reasoning"`
	ReasoningHead  string `json:"reasoning_head,omitempty"` // первые 256 символов
	ContentHead    string `json:"content_head,omitempty"`   // первые 256 символов
	ReasoningTail  string `json:"reasoning_tail,omitempty"` // последние 128 символов
	ContentTail    string `json:"content_tail,omitempty"`   // последние 128 символов
	Unclosed       bool   `json:"unclosed,omitempty"`       // true если EOS пришёл до `</think>`
}

// Snapshot возвращает текущее состояние парсера.
func (st *ReasoningStreamState) Snapshot() ReasoningSnapshot {
	st.mu.Lock()
	defer st.mu.Unlock()
	return ReasoningSnapshot{
		ReasoningChars: len(st.reasoningAll),
		ContentChars:   len(st.contentAll),
		HasReasoning:   st.reasoningAll != "",
		ReasoningHead:  headString(st.reasoningAll, 256),
		ContentHead:    headString(st.contentAll, 256),
		ReasoningTail:  tailString(st.reasoningAll, 128),
		ContentTail:    tailString(st.contentAll, 128),
		Unclosed:       hasOpenThink(st.full.String()),
	}
}

// ============================================================
// Утилиты для превью
// ============================================================

func headString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func tailString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

func hasOpenThink(full string) bool {
	// Эвристика: считаем `<think>` и `</think>` в full. Если количество
	// открывающих больше количества закрывающих — есть незакрытый блок.
	open := strings.Count(full, thinkStart) + strings.Count(full, thinkStart2)
	close := strings.Count(full, thinkEnd) + strings.Count(full, thinkEnd2)
	return open > close
}

// ============================================================
// n_predict defaults для reasoning-моделей
// ============================================================

// DefaultNPredictReasoning — дефолтный n_predict для reasoning-моделей.
// Поднимаем с 2048 до 8192, чтобы хватило на think-блок + видимый ответ.
const DefaultNPredictReasoning = 8192

// ResolveNPredict возвращает effective n_predict для данной модели с учётом
// reasoning-override. Если modelName = reasoning-модель и nPredictFromRequest == 0
// (клиент не задал явно), возвращается CPPWORKER_DEFAULT_N_PREDICT_REASONING
// или DefaultNPredictReasoning.
//
// Параметры:
//   - nPredictFromRequest: значение из запроса клиента (max_tokens / num_predict / n_predict). 0 = не задано.
//   - modelName: имя модели (req.Model).
//
// Возвращает: n_predict для C-bridge.
func ResolveNPredict(nPredictFromRequest int, modelName string) int {
	if nPredictFromRequest > 0 {
		return nPredictFromRequest
	}
	if !IsReasoningModel(modelName) {
		return nPredictFromRequest // 0 = оставить как есть (C-bridge default = 2048)
	}
	if raw := strings.TrimSpace(os.Getenv("CPPWORKER_DEFAULT_N_PREDICT_REASONING")); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 {
			return v
		}
	}
	return DefaultNPredictReasoning
}
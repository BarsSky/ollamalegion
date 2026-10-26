// reasoning_content.go — извлечение reasoning_content (think-блоков) из
// выхода reasoning-моделей (qwen3.5, qwen3.6 MoE, deepseek-r1, kimi-k2,
// gemma-4-thinking и т.п.).
//
// Зачем это нужно:
//
//   - llama.cpp (c/llama.cpp/common/chat-peg-parser.cpp) уже умеет парсить
//     think-блоки встроенным chat template engine. Однако наш cppworker
//     использует либо bridge_apply_chat_template (если в GGUF есть
//     tokenizer.chat_template), либо naive fallback, и в обоих случаях
//     получает от C-моста "сырой" текст, в котором “ остаётся
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
//
// Round 17 (2026-07-31): добавлены суффикс-маркеры (с дефисом/двоеточием/точкой
// ПЕРЕД) — "-thinking", "-reasoning", "-r1", ":thinking" и т.п. Это безопасные
// паттерны (word boundary через separator) и НЕ дают false-positive для
// "anything", "everything", "gemma-4-it" (matches "gemma-4" prefix).
var ReasoningArchPrefixes = []string{
	// === Specific reasoning architectures (qwen3.5+, deepseek-r1, kimi-k2, etc.) ===
	"qwen3.5", "qwen3.6", "qwen3.5moe", "qwen35moe", "qwen35",
	"qwen3moe", "qwen3-thinking", "qwen3_thinking",
	"deepseek-r1", "deepseek_r1", "deepseekr1",
	"kimi-k2", "kimi_k2", "kimik2",
	"gemma4", "gemma-4",
	"seed-oss", "seedoss",
	"apriel",
	"smallthinker",
	"step3.5", "step-3.5",
	// === Round 17 (2026-07-31): SAFER suffix-based patterns ===
	// Анкоры через separator (- : . _ /) — не дают false-positive.
	// "qwen3-32b-thinking" matches "-thinking" → reasoning ✓
	// "llama-3.1-8b" НЕ matches anything → no reasoning ✓
	// "my-thinking-model" matches "-thinking" → reasoning ✓
	"-thinking", ":thinking", "_thinking", "/thinking", ".thinking",
	"-reasoning", ":reasoning",
	"-r1", "_r1", // DeepSeek-R1 distill, Qwen-R1
	"-instruct-r1",
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

// ============================================================
// Политика отдачи reasoning: разбор — по формату ответа, НЕ по имени модели
// ============================================================
//
// R108 (2026-10-13). Живой дефект (Cline → балансер :18080 → cppworker,
// Qwen3.8-27B): ответ приходил «перемешанным» с маркерами ` thinking…` — текст
// размышлений уезжал в message.content / delta.content и клиент показывал
// служебные теги как обычный текст ответа.
//
// Причина: пути ОТВЕТА решали «разделять ли (reasoning, content)» по имени
// модели — IsReasoningModel(name) (whitelist префиксов) или
// IsReasoningEnabledForRequest(name) (whitelist + per-model галочка). Модель,
// которой нет в списке, отдавала ответ целиком в content.
//
// Правило (одинаково для всех клиентов и обеих поверхностей — Ollama и OpenAI):
//  1. Разделение ответа — свойство ФОРМАТА, а не имени модели. Разбор всегда
//     делают SplitReasoningContent / ReasoningStreamState по тегам формата.
//     Нет тегов — весь текст остаётся content'ом (поведение не меняется).
//  2. Имя модели и per-model флаг влияют ТОЛЬКО на ЗАПРОС: просить ли у модели
//     размышления (native enable_thinking / soft prompt — см.
//     buildChatPromptWithOptions) и какой n_predict давать по умолчанию
//     (ResolveNPredict).
//  3. Явный think=false от клиента — единственное исключение: размышления не
//     отдаются вообще (ни в content, ни в reasoning) — ровно как в non-stream
//     /api/chat (handlers_chat.go, handleChat).
//
// Поэтому IsReasoningModel / IsReasoningEnabledForRequest /
// PerModelReasoningEnabled НЕ гейтят emit reasoning'а — они остаются только для
// промпта и дефолтов. Автодетект (backend.AutoEnableReasoning) на разбор тоже
// не влияет: он лишь сохраняет per-model состояние, чтобы у thinking-модели
// были подходящие дефолты (n_predict) на следующих запросах.
//
// ReasoningWantedByClient — правило 3 политики (см. блок «Политика отдачи
// reasoning» выше): отдавать ли размышления в ответе.
//
// nil (клиент не задавал think / поле отсутствует) → true: размышления отдаём
// отдельным полем, если модель их эмитит.
// Явный think=false → false: размышления не отдаются НИ в content, ни отдельным
// полем — ровно как в non-stream /api/chat (см. handleChat).
//
// Используется всеми потоковыми путями ответа, чтобы семантика think=false была
// одинаковой у стрима и non-stream.
func ReasoningWantedByClient(thinkOverride *bool) bool {
	if thinkOverride == nil {
		return true
	}
	return *thinkOverride
}

// IsReasoningModel возвращает true, если имя модели содержит одну из
// reasoning-архитектур (см. ReasoningArchPrefixes). Регистр игнорируется.
//
// ВАЖНО: это политика ПРОМПТА/дефолтов (см. блок выше), не гейт разбора ответа.
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

// IsReasoningEnabledForRequest — Round 17 (2026-07-31) — фикс bug
// docs/plans/bug-2026-07-31-reasoning-not-routed.md.
//
// R108 (2026-10-13): БОЛЬШЕ НЕ ГЕЙТИТ РАЗБОР ОТВЕТА. Раньше эти проверки решали,
// вызывать ли SplitReasoningContent/ReasoningStreamState в путях ответа; именно
// из-за этого модель вне whitelist (например Qwen3.8-27B) отдавала маркеры
// размышлений прямо в content. Теперь разбор безусловный и определяется тегами
// формата — см. блок «Политика отдачи reasoning» выше.
//
// Отвечает на вопрос «эмитит ли эта модель reasoning вообще» — для промпта
// (просить ли размышления), дефолтов n_predict и диагностики. Оставлен для
// обратной совместимости и тестов; НЕ добавляйте его в пути ОТВЕТА (за этим
// следит TestR108_ResponsePathsDoNotGateReasoningByModelName).
//
// Проверяет ОБА источника:
//  1. IsReasoningModel(name) — hardcoded whitelist префиксов (legacy).
//  2. per-model EnableReasoning, resolved в modelInstance.reasoningEnabled
//     при LoadModel (см. internal/cppbackend/backend.go:LoadModelWithOpts).
//
// Nil-safe: backend может быть nil в тестах (или при ранней инициализации).
// В этом случае fallback на whitelist (IsReasoningModel) — graceful degradation.
func IsReasoningEnabledForRequest(modelName string) bool {
	if IsReasoningModel(modelName) {
		return true
	}
	// Fallback: per-model override через resolved EnableReasoning.
	// Если backend не инициализирован (nil) или модель не загружена —
	// GetModel возвращает error, IsReasoningModel fallback решает.
	if backend == nil {
		return false
	}
	if info, err := backend.GetModel(modelName); err == nil && info != nil && info.ReasoningEnabled {
		return true
	}
	return false
}

// PerModelReasoningEnabled — R83/v67 (2026-10-02): ТОЛЬКО per-model состояние
// (inst.reasoningEnabled, resolved в LoadModel из профиля/WebUI-галочки или
// opts.EnableReasoning), без whitelist по имени модели.
//
// Зачем отдельно от IsReasoningEnabledForRequest: тот возвращает true для
// ЛЮБОЙ модели из whitelist по имени (в том числе gemma-4) и потому не годится
// как «оператор включил размышления»: он включал бы thinking всем gemma-4
// подряд, даже когда галочка снята. Здесь же — ровно то состояние, которое
// видно в /api/models как reasoning_enabled.
//
// Nil-safe: backend может быть nil (тесты, ранняя инициализация).
func PerModelReasoningEnabled(modelName string) bool {
	if backend == nil || modelName == "" {
		return false
	}
	info, err := backend.GetModel(modelName)
	if err != nil || info == nil {
		return false
	}
	return info.ReasoningEnabled
}

// ============================================================
// Разбор think-блоков
// ============================================================

// Round 17.1 (2026-07-31): расширил набор поддерживаемых tag-пар reasoning-блоков.
// Live test показал: qwen3-instruct при soft prompt "use <think> tags" реально
// использует `<reasoning>...</reasoning>` (НЕ `<think>`!). Это собственный
// convention модели, не поддаётся override через soft prompt. Решение —
// поддержать ВСЕ популярные варианты в парсере, чтобы любая модель
// (native thinking + custom fine-tunes) работала out-of-the-box.
//
// Список пар:
//   - <think>...</think>  — Qwen3-thinking, DeepSeek-R1, Kimi-K2, Seed-OSS (default)
//   - <thinking>...</thinking> — gemma-4-it, некоторые экспериментальные
//   - <reasoning>...</reasoning> — qwen3-instruct (при soft prompt "use tags")
//   - <analysis>...</analysis> — некоторые o1-style модели
//
// Round 32 (2026-08-09): добавлены gemma-4 native chat-template форматы.
// В GGUF токенизаторе gemma-4 (см. dump special tokens) есть токены
// <|think|>, <|channel>, <channel|>, <|turn>, <turn|>. Chat template
// gemma-4-E4B рендерит reasoning как:
//
//	{{- '<|channel>thought\n' + thinking_text + '\n<channel|>' -}}
//
// ВАЖНО: эти теги — single tokens, при detokenization они эмитятся
// в output как есть, БЕЗ пробелов между <|channel> и thought.
// Поддерживаем 2 варианта: с \n (chat-template канонический) и без
// (на случай если модель детектит reasoning без newline после thought).
// Также <|think>...<think|> — канонический think-блок Gemma native (без
// channel-обёртки, для совместимости с qwen-style).
//
// Каждая пара симметрична — открывающий тег имеет соответствующий закрывающий.
var thinkTagPairs = []struct {
	open  string
	close string
}{
	{"<think>", "</think>"},
	// R243: "thought"-варианты (DeepSeek и производные шаблоны). Без них
	// `<thought>…</thought>` считался видимым ответом, и клиент (Cline) видел
	// служебные маркеры в content.
	{"<thought>", "</thought>"},
	{"<|begin_of_thought|>", "<|end_of_thought|>"},
	{"<|thought|>", "<|/thought|>"},
	{"<thinking>", "</thinking>"},
	{"<reasoning>", "</reasoning>"},
	{"<analysis>", "</analysis>"},
	// Round 32 (2026-08-09): gemma-4 native channel format (chat template canonical).
	{"<|channel>thought\n", "\n<channel|>"},
	{"<|channel>thought", "<channel|>"},
	// Round 32: gemma-4 alternative analysis channel.
	{"<|channel>analysis\n", "\n<channel|>"},
	{"<|channel>analysis", "<channel|>"},
	// Round 32: Qwen-style with |...| wrapping (gemma-4 also accepts this).
	{"<|think>", "<think|>"},
	// Round 32 #8 (2026-08-10): bare <|channel>...<channel|> вариант.
	// Модель эмитит без "thought"/"analysis" суффикса. ВАЖНО: pair должен
	// быть ПОСЛЕДНИМ — openThinkTag() выбирает earliest match, и bare
	// <|channel> будет ложно сматчен до <|channel>thought если окажется
	// раньше в списке. Добавляем в конец, чтобы более специфичные теги
	// проверялись первыми.
	{"<|channel>", "<channel|>"},
}

// thinkStart/thinkEnd — backward-compat aliases (используются в тестах и ниже).
// ВСЕ новые callers должны использовать thinkTagPairs.
const (
	thinkStart  = "<think>"
	thinkEnd    = "</think>"
	thinkStart2 = "<thinking>"
	thinkEnd2   = "</thinking>"
)

// openThinkTag ищет ближайший открывающий тег в позиции ≥ from.
// Возвращает (startPos, endPos, kindIdx) — startPos это индекс символа '<',
// endPos — индекс после '>'. kindIdx — индекс в thinkTagPairs, -1 если не найдено.
//
// Round 17.1: ищем ВСЕ пары, не только первые две.
func openThinkTag(s string, from int) (int, int, int) {
	if from < 0 {
		from = 0
	}
	bestPos := -1
	bestKind := -1
	for i, pair := range thinkTagPairs {
		idx := strings.Index(s[from:], pair.open)
		if idx < 0 {
			continue
		}
		absPos := from + idx
		if bestPos < 0 || absPos < bestPos {
			bestPos = absPos
			bestKind = i
		}
	}
	if bestPos < 0 {
		return -1, -1, -1
	}
	return bestPos, bestPos + len(thinkTagPairs[bestKind].open), bestKind
}

// closeThinkTag ищет соответствующий закрывающий тег в позиции ≥ from
// для пары, найденной openThinkTag (kindIdx). Возвращает (endPos, closeTagLen)
// — endPos это индекс после закрывающего тега, closeTagLen — длина
// тега, который фактически был найден (может отличаться от pair.close
// если сработал fallback).
//
// Round 17.1: принимает kindIdx, ищет соответствующий close из thinkTagPairs.
// Round 52.3 (2026-08-24): возвращаем closeTagLen — нужно caller'у
// чтобы правильно вычислить bodyEnd когда сработал fallback (например,
// `<|channel>thought\n...<channel|>` — открывающий тег ожидает close
// `\n<channel|>` длиной 10, но реально находится `<channel|>` длиной 9).
// Без этого возврата bodyEnd обрезал последний символ reasoning.
func closeThinkTag(s string, from int, kindIdx int) (endPos int, closeTagLen int) {
	if kindIdx < 0 || kindIdx >= len(thinkTagPairs) {
		return -1, 0
	}
	// Ищем соответствующий close tag (тот же kind).
	pair := thinkTagPairs[kindIdx]
	if i := strings.Index(s[from:], pair.close); i >= 0 {
		return from + i + len(pair.close), len(pair.close)
	}
	// Fallback: для обратной совместимости — может закрываться тегом из другой пары
	// (например, кто-то открыл <think> а закрыл </thinking>). На практике не встречается,
	// но добавляем fallback чтобы не терять данные.
	for _, alt := range thinkTagPairs {
		if alt.close == pair.close {
			continue
		}
		if i := strings.Index(s[from:], alt.close); i >= 0 {
			return from + i + len(alt.close), len(alt.close)
		}
	}
	return -1, 0
}

// backward-compat wrapper для кода, использующего bool isThinking.
// kindIdx == 0 (<think>) → isThinking=false
// kindIdx == 1 (<thinking>) → isThinking=true
// остальные → деградируют на старую логику.
func openThinkTagLegacy(s string, from int) (int, int, bool) {
	pos, end, kind := openThinkTag(s, from)
	if pos < 0 {
		return -1, -1, false
	}
	return pos, end, kind == 1
}

// PromptOpensReasoning — R216 (2026-10-21): заканчивается ли prompt ВНУТРИ
// открытого блока размышлений.
//
// ЗАЧЕМ. Чат-шаблон Qwen3 при enable_thinking=true дописывает в конец промпта
// `assistant\n<think>\n` (gemma-4 — `<|channel>thought\n`). Модель продолжает
// этот блок, поэтому её вывод НАЧИНАЕТСЯ внутри размышлений и может не содержать
// открывающего маркера вовсе. SplitReasoningContent такой вывод считает видимым
// ответом: живой стенд (Qwen3.8-27B, max_tokens=16) отдал клиенту
// `The user is asking me to memorize a code "КРАСНЫЙ-` в message.content без
// reasoning_content и без закрывающего `</think>` — ровно жалоба оператора
// «размышления попали в основной блок, тело ответа не пришло».
func PromptOpensReasoning(prompt string) bool {
	if prompt == "" {
		return false
	}
	return reasoningBlockOpenAtEnd(prompt)
}

// reasoningBlockOpenAtEnd — заканчивается ли текст ВНУТРИ открытого блока
// размышлений: последний открывающий маркер позже последнего закрывающего.
//
// R231: вынесено из PromptOpensReasoning, чтобы тем же правилом отвечать на
// вопрос «поток оборвался ДО закрытия think-блока?» (Snapshot().Unclosed).
// Прежняя реализация hasOpenThink считала только `<think>`/`<thinking>` и
// игнорировала gemma-4 `<|channel>thought` — из-за этого незакрытый канал
// gemma выглядел как «закрытый», и finish_reason оставался stop при пустом
// content (клиент считал ответ полным).
func reasoningBlockOpenAtEnd(s string) bool {
	lastOpenEnd, lastCloseEnd := -1, -1
	for _, pair := range thinkTagPairs {
		if pair.open != "" {
			if i := strings.LastIndex(s, pair.open); i >= 0 {
				if e := i + len(pair.open); e > lastOpenEnd {
					lastOpenEnd = e
				}
			}
		}
		if pair.close == "" {
			continue
		}
		if i := strings.LastIndex(s, pair.close); i >= 0 {
			if e := i + len(pair.close); e > lastCloseEnd {
				lastCloseEnd = e
			}
		}
	}
	return lastOpenEnd > lastCloseEnd
}

// SplitReasoningContentFromPrompt — R216: разбор вывода с учётом состояния
// ПРОМПТА. Если prompt закончился внутри размышлений, весь вывод до первого
// закрывающего маркера — reasoning (даже когда маркера нет вовсе: тогда модель
// обрезали до `</think>`, и это НЕ видимый ответ).
//
// Отличие от SplitReasoningContent: тот применяет правило «закрывающий маркер
// без открывающего» только когда закрывающий есть. Незакрытый и неоткрытый
// вывод (обрезанные размышления) он считает content.
func SplitReasoningContentFromPrompt(prompt, output string) (reasoning, content string, hasReasoning bool) {
	if output == "" {
		return "", "", false
	}
	if !PromptOpensReasoning(prompt) {
		return SplitReasoningContent(output)
	}
	if pos, tagLen, found := firstOrphanCloseTag(output); found {
		// R219: в остатке могут быть ЕЩЁ закрывающие маркеры (модель закрыла блок
		// несколько раз). Клиенту они не нужны: живой ответ с `</think>` в теле
		// читается как «размышления попали в ответ». Вырезаем служебные маркеры,
		// текст ответа не трогаем.
		return output[:pos], stripReasoningMarkers(output[pos+tagLen:]), true
	}
	// Маркера закрытия нет: весь вывод — продолжение размышлений.
	return output, "", strings.TrimSpace(output) != ""
}

// stripReasoningMarkers удаляет служебные маркеры размышлений из видимого
// текста (R219). Нужен для implied-open вывода и для «незакрытых» случаев,
// где после первого закрывающего маркера в хвосте могут остаться другие.
func stripReasoningMarkers(s string) string {
	if s == "" {
		return s
	}
	for _, pair := range thinkTagPairs {
		if pair.open != "" {
			s = strings.ReplaceAll(s, pair.open, "")
		}
		if pair.close != "" {
			s = strings.ReplaceAll(s, pair.close, "")
		}
	}
	return s
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
		tagStart, tagEnd, kindIdx := openThinkTag(s, pos)
		if tagStart < 0 {
			sbContent.WriteString(s[pos:])
			break
		}
		// Текст до открывающего тега → content.
		sbContent.WriteString(s[pos:tagStart])
		// Ищем закрывающий тег после tagEnd (используем тот же kindIdx).
		closePos, closeTagLen := closeThinkTag(s, tagEnd, kindIdx)
		if closePos < 0 {
			// Незакрытый блок: только тело (от tagEnd до конца) → reasoning.
			// Сам `<think>` в reasoning не включаем (это технический маркер).
			sbReasoning.WriteString(s[tagEnd:])
			foundAny = true
			break
		}
		// Round 52.3 (2026-08-24): используем closeTagLen возвращённый из
		// closeThinkTag (а не len(thinkTagPairs[kindIdx].close)) — может
		// отличаться при fallback. Без этого bodyEnd обрезал последний символ.
		bodyStart := tagEnd
		bodyEnd := closePos - closeTagLen
		sbReasoning.WriteString(s[bodyStart:bodyEnd])
		foundAny = true
		pos = closePos
	}

	// R114b (2026-10-13): ЗАКРЫВАЮЩИЙ маркер БЕЗ открывающего.
	//
	// Живая жалоба оператора: «в ответе перемешка с ответом и маркером </think>».
	// Причина — prefill-канал: чат-шаблон модели открывает канал размышлений в
	// ПРОМПТЕ (`<|channel>thought\n` уже в промпте, см. комментарий к thinkTagPairs),
	// поэтому вывод модели начинается ВНУТРИ размышлений и содержит только
	// ЗАКРЫВАЮЩИЙ маркер. Парный разбор открывающий тег не находит, весь текст
	// (размышления!) уходил в content, а сам маркер оставался в нём же.
	//
	// Правило: если открывающих тегов в выводе НЕТ ВООБЩЕ, а закрывающий есть —
	// всё ДО него размышления, всё ПОСЛЕ — ответ. Если же хотя бы одна пара
	// нашлась (foundAny), правило не применяется: остаток после пары — обычный
	// content, и резать его по одинокому маркеру было бы потерей ответа.
	if !foundAny {
		content := sbContent.String()
		if closePos, closeTagLen, found := firstOrphanCloseTag(content); found && closePos > 0 {
			// R231: после первого закрывающего маркера в хвосте могут быть ЕЩЁ
			// маркеры (модель закрыла блок несколько раз) — вырезаем их, текст
			// ответа не трогаем (та же политика, что в R219).
			return content[:closePos], stripReasoningMarkers(content[closePos+closeTagLen:]), true
		}
	}
	contentOut := stripReasoningMarkers(sbContent.String())
	return sbReasoning.String(), contentOut, foundAny
}

// firstOrphanCloseTag — первый закрывающий маркер размышлений в тексте.
// Возвращает (позицию, длину маркера, найден).
func firstOrphanCloseTag(s string) (int, int, bool) {
	bestPos, bestLen := -1, 0
	for _, p := range thinkTagPairs {
		if p.close == "" {
			continue
		}
		if i := strings.Index(s, p.close); i >= 0 {
			if bestPos < 0 || i < bestPos {
				bestPos, bestLen = i, len(p.close)
			}
		}
	}
	return bestPos, bestLen, bestPos >= 0
}

// ContentFromUnclosedReasoning — R91 (2026-10-08): ответ, целиком ушедший в
// НЕЗАКРЫТЫЙ reasoning-блок, должен быть виден клиенту.
//
// ЖИВОЙ ФАКТ (стенд, gemma-4-E4B-it-Q4_K_M, max_tokens=96):
//
//	reasoning-чанков: 87, чанков content: 0, completion_tokens: 97 (= упор в кап),
//	финальный чанк: {"delta":{"content":null,"role":"assistant"},"finish_reason":"stop"}
//
// То есть модель целиком потратила бюджет на незакрытый `<reasoning>` и не
// успела дойти до ответа. Разбор по тегам для такого текста даёт
// reasoning="весь текст", content="" (см. SplitReasoningContent: «незакрытый
// блок: только тело → reasoning»), а клиенты (SillyTavern, Open WebUI, Cline)
// показывают `content` — пользователь видит ПУСТОЙ ответ при 97 сгенерированных
// токенах. С тем же промптом и max_tokens=400 ответ приходит нормально (6/6), то
// есть проблема именно в связке «малый кап + незакрытый reasoning».
//
// Поэтому когда контента нет вовсе, а reasoning есть — отдаём reasoning как
// видимый ответ. Дублирование текста в клиентах, показывающих оба канала, —
// осознанная цена: пустой ответ хуже повторённого.
func ContentFromUnclosedReasoning(reasoning, content string) (visible string, usedFallback bool) {
	if strings.TrimSpace(content) != "" {
		return content, false
	}
	if strings.TrimSpace(reasoning) == "" {
		return content, false
	}
	return reasoning, true
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
	// impliedOpen — R216: prompt закончился внутри размышлений (шаблон дописал
	// `<think>`/`<|channel>thought`), значит вывод начинается ВНУТРИ блока и
	// открывающего маркера в потоке может не быть. Раскладываем по этому правилу
	// (см. SplitReasoningContentFromPrompt).
	impliedOpen bool
}

// NewReasoningStreamState создаёт новый incremental-парсер.
func NewReasoningStreamState() *ReasoningStreamState {
	return &ReasoningStreamState{}
}

// SetImpliedOpen — R216: сказать парсеру, что вывод начинается внутри блока
// размышлений (prompt заканчивается открытым `<think>`/<|channel>thought).
func (st *ReasoningStreamState) SetImpliedOpen(on bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.impliedOpen = on
}

// split — общий разбор с учётом impliedOpen.
func (st *ReasoningStreamState) split(s string) (reasoning, content string, has bool) {
	if st.impliedOpen {
		if pos, tagLen, found := firstOrphanCloseTag(s); found {
			// R231: дубли закрывающих маркеров в хвосте вырезаем — иначе они
			// попадают в content-дельту (живой Cline: `answer</think>`).
			return s[:pos], stripReasoningMarkers(s[pos+tagLen:]), true
		}
		if strings.TrimSpace(s) == "" {
			return "", "", false
		}
		return s, "", true
	}
	return SplitReasoningContent(s)
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
	// R83/v67 (2026-10-02): НЕ отдаём наружу хвост, который может оказаться
	// началом think-тега. См. trailingTagPrefixLen: иначе недособранный маркер
	// утекает в видимый ответ. Хвост выпустит Finalize (или он разрешится
	// следующим чанком).
	full := st.full.String()
	holdBack := trailingTagPrefixLen(full)
	safe := full[:len(full)-holdBack]
	reasoning, content, _ := st.split(safe)
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

// maxThinkTagLen — длина самого длинного тега в thinkTagPairs.
func maxThinkTagLen() int {
	max := 0
	for _, p := range thinkTagPairs {
		if len(p.open) > max {
			max = len(p.open)
		}
		if len(p.close) > max {
			max = len(p.close)
		}
	}
	return max
}

// isProperTagPrefix — true, если s (непустой) является НАЧАЛОМ какого-либо
// think-тега, но короче его (то есть тег может достроиться следующим чанком).
func isProperTagPrefix(s string) bool {
	if s == "" {
		return false
	}
	for _, p := range thinkTagPairs {
		if len(s) < len(p.open) && strings.HasPrefix(p.open, s) {
			return true
		}
		if len(s) < len(p.close) && strings.HasPrefix(p.close, s) {
			return true
		}
	}
	return false
}

// trailingTagPrefixLen — длина хвоста s, который может оказаться неполным
// think-тегом (открывающим или закрывающим).
//
// ЗАЧЕМ. C-bridge отдаёт поток токенами, и маркер приходит по частям. Живой
// лог cppworker на gemma-4 (R83/v67, диагностика «размышления не применились»):
//
//	content:  "<|c"   content: "h"   content: "a"   content: "nn"
//	reasoning_content: "though" ...
//
// То есть первые 7 символов открывающего маркера `<|channel>thought` ушли
// клиенту как ВИДИМЫЙ ответ, а остальное — в reasoning_content. Парсер
// пересчитывал разбиение на каждом чанке и не мог «отозвать» уже отправленное.
//
// РЕШЕНИЕ: пока хвост накопленного текста является началом любого тега, он
// удерживается и наружу не отдаётся. Разрешится он следующим чанком (станет
// тегом или обычным текстом), а если поток закончится — его выпустит Finalize.
func trailingTagPrefixLen(s string) int {
	if s == "" {
		return 0
	}
	maxLen := maxThinkTagLen() - 1
	if maxLen > len(s) {
		maxLen = len(s)
	}
	for n := maxLen; n >= 1; n-- {
		if isProperTagPrefix(s[len(s)-n:]) {
			return n
		}
	}
	return 0
}

// Finalize завершает парсинг: выпускает УДЕРЖАННЫЙ хвост (см.
// trailingTagPrefixLen) как обычный текст, если он так и не стал тегом.
//
// R83/v67 (2026-10-02): раньше был no-op, потому что Feed отдавал всё
// немедленно. Теперь Feed удерживает потенциальный неполный маркер, поэтому
// каждый потоковый путь ОБЯЗАН вызвать Finalize после EOS и эмитить
// возвращённые дельты — иначе последние символы ответа потеряются.
//
// R231: оставлен для НЕ-потоковых путей (R91-фолбэк «незакрытый reasoning →
// content» там осознан: клиент, показывающий только content, иначе увидит
// пустой ответ). В стриме использовать FinalizeForStream.
func (st *ReasoningStreamState) Finalize() (reasoningDelta, contentDelta string) {
	return st.finalize(true)
}

// FinalizeForStream — R231 (2026-10-24): завершение ПОТОКА без R91-фолбэка.
//
// ЖИВОЙ ДЕФЕКТ (Cline → балансер :18080 → cppworker, gemma-4-E4B, stream=true,
// max_tokens=120): модель открыла канал размышлений и не успела его закрыть;
// все ~115 чанков ушли в reasoning_content, а затем Finalize отдал ТОТ ЖЕ текст
// ещё раз единственным чанком delta.content (433 символа «Thinking Process: …»).
// Cline склеивает content и видит «ответ», целиком повторяющий размышления.
// Для агентских клиентов это ещё и ломает контекст сессии.
//
// Причина: R91-фолбэк применялся и к стриму, хотя в стриме reasoning уже
// доставлен отдельным каналом — копия в content не «спасает пустой ответ», а
// дублирует. В стриме честный ответ: reasoning_content есть, content пуст,
// finish_reason=length (вызывающий ставит его по Snapshot().Unclosed).
func (st *ReasoningStreamState) FinalizeForStream() (reasoningDelta, contentDelta string) {
	return st.finalize(false)
}

// finalize — общая реализация Finalize/FinalizeForStream.
// allowUnclosedFallback=true включает R91-фолбэк (не-потоковые пути).
func (st *ReasoningStreamState) finalize(allowUnclosedFallback bool) (reasoningDelta, contentDelta string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	full := st.full.String()
	if full == "" {
		return "", ""
	}
	// Поток закончился: неопределённости больше нет, разбираем весь текст.
	reasoning, content, _ := st.split(full)
	prevRLen := len(st.reasoningAll)
	prevCLen := len(st.contentAll)
	if len(reasoning) >= prevRLen {
		reasoningDelta = reasoning[prevRLen:]
	}
	if len(content) >= prevCLen {
		contentDelta = content[prevCLen:]
	}
	// R91: поток закончился, а видимого контента не было вовсе — значит модель
	// ушла в незакрытый reasoning-блок (см. ContentFromUnclosedReasoning).
	// Отдаём весь reasoning как content, иначе клиент получает пустой ответ.
	//
	// R216 (2026-10-21): НЕ отдаём, если блок размышлений открыт САМИМ ПРОМПТОМ
	// (impliedOpen). Тогда текст — заведомо размышления, и копирование его в
	// content даёт ДУБЛЬ: живой стенд отдавал одни и те же 76 символов и в
	// reasoning_content (19 чанков), и в content (1 чанк) — ровно жалоба
	// оператора «размышления попали в основной блок». Честный ответ клиенту:
	// reasoning_content есть, content пуст, finish_reason=length.
	//
	// R231: фолбэк вообще не применяется к стриму (allowUnclosedFallback=false) —
	// дубль в content наблюдался и на модели, которая открыла блок САМА
	// (impliedOpen=false), см. FinalizeForStream.
	if allowUnclosedFallback && !st.impliedOpen {
		if visible, usedFallback := ContentFromUnclosedReasoning(reasoning, content); usedFallback {
			contentDelta = visible
		}
	}
	st.reasoningAll = reasoning
	st.contentAll = content
	return reasoningDelta, contentDelta
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
	full := st.full.String()
	unclosed := hasOpenThink(full)
	if !unclosed && st.impliedOpen && !hasReasoningCloseTag(full) {
		// R231: блок открыт промптом — в выводе открывающего маркера нет,
		// поэтому «закрыто» только если пришёл закрывающий.
		unclosed = true
	}
	return ReasoningSnapshot{
		ReasoningChars: len(st.reasoningAll),
		ContentChars:   len(st.contentAll),
		HasReasoning:   st.reasoningAll != "",
		ReasoningHead:  headString(st.reasoningAll, 256),
		ContentHead:    headString(st.contentAll, 256),
		ReasoningTail:  tailString(st.reasoningAll, 128),
		ContentTail:    tailString(st.contentAll, 128),
		Unclosed:       unclosed,
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
	if full == "" {
		return false
	}
	// R231: классические пары — по КОЛИЧЕСТВУ (устойчиво к вложенности:
	// `<think>a<think>b</think>` — блок не закрыт, R83-семантика).
	for _, p := range []struct{ open, close string }{
		{"<think>", "</think>"},
	// R243: "thought"-варианты (DeepSeek и производные шаблоны). Без них
	// `<thought>…</thought>` считался видимым ответом, и клиент (Cline) видел
	// служебные маркеры в content.
	{"<thought>", "</thought>"},
	{"<|begin_of_thought|>", "<|end_of_thought|>"},
	{"<|thought|>", "<|/thought|>"},
		{"<thinking>", "</thinking>"},
		{"<reasoning>", "</reasoning>"},
		{"<analysis>", "</analysis>"},
		{"<|think>", "<think|>"},
	} {
		if strings.Count(full, p.open) > strings.Count(full, p.close) {
			return true
		}
	}
	// gemma-4 каналы — по ПОЗИЦИИ последнего события: подсчёт ломается, потому
	// что короткий `<|channel>` входит в `<|channel>thought` и даёт лишний open.
	openIdx := strings.LastIndex(full, "<|channel>")
	if openIdx < 0 {
		return false
	}
	closeIdx := strings.LastIndex(full, "<channel|>")
	return closeIdx < 0 || closeIdx < openIdx
}

// hasReasoningCloseTag — есть ли в тексте ХОТЬ ОДИН закрывающий маркер
// размышлений (любой пары). Нужен для impliedOpen-случая: блок открыт
// ПРОМПТОМ, поэтому открывающего маркера в выводе нет, и hasOpenThink сам по
// себе сказал бы «закрыто» даже когда модель оборвалась внутри размышлений.
func hasReasoningCloseTag(s string) bool {
	if s == "" {
		return false
	}
	for _, pair := range thinkTagPairs {
		if pair.close != "" && strings.Contains(s, pair.close) {
			return true
		}
	}
	return false
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
		// Round 36.1 (2026-08-17) BUGFIX: c/bridge/bridge.go now substitutes
		// DefaultGenerationParams().NPredict (= 2048) when params.NPredict <= 0
		// (defensive, in c/bridge/bridge.go Infer and InferStream). So returning
		// 0 here is safe — the bridge layer will fill in 2048 before calling
		// C.bridge_infer*. (Previously the C-bridge fallback was hardcoded 512,
		// which silently truncated Qwen3-Instruct mid-response.)
		return nPredictFromRequest // 0 = bridge layer substitutes default (2048)
	}
	if raw := strings.TrimSpace(os.Getenv("CPPWORKER_DEFAULT_N_PREDICT_REASONING")); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 {
			return v
		}
	}
	return DefaultNPredictReasoning
}

// tool_calls.go — Parsing tool_calls from model output and building
// properly structured OpenAI-compatible responses with finish_reason="tool_calls".
package main

import (
	"container/list"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ============================================================
// Tools system prompt cache (Шаг 2 — компактный prompt + кеш по fingerprint)
// ============================================================
//
// Проблема (см. docs/plans/archive/remaining-real-plan.md, OpenWebUI tools-flow):
// При работе с OpenWebUI tools каждая итерация диалога добавляет в запрос
// один и тот же набор tool definitions (несколько KB JSON Schema на каждый
// tool). buildToolsSystemPrompt вызывался на КАЖДОМ запросе и пересобирал
// один и тот же текст. При 5-10 tools суммарно prompt занимает 3-5K токенов,
// что:
//  1. Съедает n_ctx, вызывая overflow → reload-loop (см. inference.go:ram-fallback).
//  2. Замедляет inference (длиннее prefill).
//
// Решение:
//  1. Компактный формат: только name + 1 строка description + сигнатура параметров.
//  2. Кеш по fingerprint (SHA1 от JSON сериализованных tools) — если набор
//     tools не изменился между запросами (типичный случай для OpenWebUI),
//     возвращаем уже собранный prompt.
//  3. TTL 30 мин — если tools обновились в OpenWebUI, кеш устареет.
//
// Фишка: fingerprint считается от нормализованного JSON (sorted keys), чтобы
// порядок полей в JSON OpenWebUI не влиял на ключ кеша.
const (
	// toolsPromptCacheTTL — время жизни записи в кеше. OpenWebUI обычно не
	// меняет набор tools в течение сессии, поэтому 30 мин — разумный компромисс
	// между экономией памяти и свежестью.
	toolsPromptCacheTTL = 30 * time.Minute
	// toolsPromptMaxDescriptionChars — лимит на описание одного tool.
	// Большие описания (>300 символов) обрезаются с многоточием — модель всё
	// равно не читает длинные JSDoc, для function calling достаточно сути.
	toolsPromptMaxDescriptionChars = 300
	// toolsPromptMaxTotalChars — жёсткий лимит на размер tools-prompt. Если
	// даже компактный формат превышает этот лимит, дополнительно обрезаем
	// описания пропорционально. Защита от случая, когда OpenWebUI регистрирует
	// 50+ tools.
	toolsPromptMaxTotalChars = 8000
)

// toolsPromptCacheEntry — запись кеша для конкретного fingerprint.
// Round 16 P2 fix (2026-07-30): хранит `key` для O(1) eviction — раньше
// нужен был O(N) scan map при каждом eviction (256 entries → 256 scan).
type toolsPromptCacheEntry struct {
	key       string
	prompt    string
	createdAt time.Time
}

// toolsPromptCacheMaxEntries — лимит LRU-кеша. OpenWebUI часто держит один и
// тот же набор tools на всю сессию (1-3 fingerprint'а), но при разных tooling
// profiles в рамках одной ноды кеш может расти. 256 — разумный предел:
// каждый entry ~1-10 KB → ~1-3 MB максимум при полном заполнении.
//
// Round 6 #3: было sync.Map без лимита — медленная утечка при OpenWebUI с
// динамическими tool sets (каждый чат свои tools → новый fingerprint).
const toolsPromptCacheMaxEntries = 256

// toolsPromptCache — потокобезопасный LRU-кеш для tools prompts.
// TTL + LRU eviction (Round 6 #3). sync.Map.Lock уже не используется —
// единый mutex защищает и map, и list.
//
// Round 16 P2 fix (2026-07-30): key хранится в list element (через
// toolsPromptCacheEntry.key), eviction O(1) вместо O(N) scan map.
// При MaxEntries=256 + 1000 evictions/sec → было 256K comparisons/sec,
// теперь 1000 O(1) lookups. Не критично при низком трафике, но правильнее.
type toolsPromptCacheLRU struct {
	mu    sync.Mutex
	items map[string]*list.Element // fingerprint -> doubly-linked list element
	order *list.List               // front = most-recently-used, back = LRU
}

func newToolsPromptCacheLRU() *toolsPromptCacheLRU {
	return &toolsPromptCacheLRU{
		items: make(map[string]*list.Element),
		order: list.New(),
	}
}

// Get returns cached prompt if present and not expired; updates LRU position.
func (c *toolsPromptCacheLRU) Get(fp string) (string, bool) {
	if fp == "" {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[fp]
	if !ok {
		return "", false
	}
	entry := el.Value.(*toolsPromptCacheEntry)
	if time.Since(entry.createdAt) > toolsPromptCacheTTL {
		// Expired — drop and treat as miss.
		c.order.Remove(el)
		delete(c.items, fp)
		return "", false
	}
	c.order.MoveToFront(el)
	return entry.prompt, true
}

// Put inserts/updates entry, evicting the oldest if over capacity.
func (c *toolsPromptCacheLRU) Put(fp, prompt string) {
	if fp == "" || prompt == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if el, ok := c.items[fp]; ok {
		entry := el.Value.(*toolsPromptCacheEntry)
		entry.prompt = prompt
		entry.createdAt = now
		c.order.MoveToFront(el)
		return
	}
	el := c.order.PushFront(&toolsPromptCacheEntry{key: fp, prompt: prompt, createdAt: now})
	c.items[fp] = el
	// Evict LRU until under cap. O(1) — key хранится в list element.
	for c.order.Len() > toolsPromptCacheMaxEntries {
		back := c.order.Back()
		if back == nil {
			break
		}
		backEntry := back.Value.(*toolsPromptCacheEntry)
		c.order.Remove(back)
		delete(c.items, backEntry.key)
	}
}

// Delete removes a specific fingerprint (used on expiry).
func (c *toolsPromptCacheLRU) Delete(fp string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[fp]; ok {
		c.order.Remove(el)
		delete(c.items, fp)
	}
}

// toolsPromptCache — singleton LRU-кеш.
var toolsPromptCache = newToolsPromptCacheLRU()

// toolsPromptCacheStats — счётчики попаданий/промахов для отладки.
var (
	toolsPromptCacheHits   atomic.Int64
	toolsPromptCacheMisses atomic.Int64
)

// toolsPromptFingerprint вычисляет стабильный fingerprint для набора tools.
// Используется SHA1 от canonical JSON (sorted keys).
func toolsPromptFingerprint(tools []openAITool) string {
	if len(tools) == 0 {
		return ""
	}
	// Marshal each tool separately to avoid зависимости от порядка полей.
	// Затем склеиваем через null-разделитель и хешируем.
	h := sha1.New()
	for _, t := range tools {
		if t.Type != "function" {
			continue
		}
		// Marshal Function (без Type — он всегда "function" после фильтра).
		b, err := json.Marshal(t.Function)
		if err != nil {
			// fallback: используем строку (кеш не сработает, но не упадём).
			b = []byte(t.Function.Name)
		}
		h.Write(b)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// getCachedToolsPrompt возвращает промпт из кеша или пустую строку если miss.
// Также применяет TTL — устаревшие записи удаляются сразу (Round 6 #3 LRU).
func getCachedToolsPrompt(fingerprint string) string {
	if fingerprint == "" {
		return ""
	}
	prompt, _ := toolsPromptCache.Get(fingerprint)
	return prompt
}

// putCachedToolsPrompt сохраняет промпт в кеш с текущей меткой времени.
func putCachedToolsPrompt(fingerprint, prompt string) {
	if fingerprint == "" || prompt == "" {
		return
	}
	toolsPromptCache.Put(fingerprint, prompt)
}

// trimDescription обрезает описание tool до разумной длины.
// Сохраняем целостность слов: режем по ближайшему пробелу/точке.
func trimDescription(desc string, maxChars int) string {
	if desc == "" || maxChars <= 0 {
		return ""
	}
	if len(desc) <= maxChars {
		return desc
	}
	trimmed := desc[:maxChars]
	// Ищем последний пробел для аккуратного обрезания.
	if idx := strings.LastIndexAny(trimmed, " .,:;"); idx > maxChars/2 {
		trimmed = trimmed[:idx]
	}
	return strings.TrimRight(trimmed, " .,:;") + "..."
}

# Bug Report — 2026-07-31 — Reasoning не маршрутизируется в правильное поле

> **Severity:** 🟠 MAJOR (user-facing, проявляется на production OpenWebUI)
> **Reporter:** user (тест на локальной машине)
> **Components:** `cmd/cppworker/handlers_openai.go` (reasoning parser routing), `internal/balancer/llamacpp_transport_nonstream.go` (proxy)
> **Models affected:** Любая non-reasoning модель с включённым `EnableReasoning` (например, Qwen3-Instruct при soft-prompt thinking)

## Симптомы (со слов пользователя)

1. **"ответ пришел не полный"** — OpenWebUI показывает неполный ответ
2. **"модель продолжает работу"** — модель физически работает дальше, не убита
3. **"при включенном флаге размышления у клиента никак не отобразилось что модель размышляет"** — `reasoning_content` поле НЕ показывается
4. **"ответ выглядит как размышления модели но не в соответствующей секции"** — текст "размышлений" попадает в `content`, а не в `reasoning_content`

## Root cause analysis

### Как работает reasoning pipeline сейчас

cppworker имеет ДВЕ разные настройки для reasoning:

**A. `EnableReasoning` (soft-prompt path)** — в `cfg.EnableReasoning` (config.go:99)
- Когда true → к system промпту добавляется "think step by step" инструкция
- Работает для ЛЮБОЙ instruction-tuned модели
- НЕ требует `<think>` тегов в выходе

**B. `IsReasoningModel(name)` (parser path)** — в `reasoning_content.go:107`
- Хардкод список префиксов: qwen3.5, qwen3.6, deepseek-r1, kimi-k2, gemma-4, etc.
- **qwen3-4b НЕ входит** в этот список (голое "qwen3" исключено намеренно)
- Когда true → парсер ищет `<think>...</think>` в выходе и split'ит на `reasoning_content` + `content`
- Когда false → НЕ split'ит, всё идёт в `content`

### Что происходит на production

Пользователь включает reasoning (например через WebUI → enable reasoning):
1. `cfg.EnableReasoning = true` ✓
2. SOFT prompt injection срабатывает ✓
3. Модель получает "think step by step" и **эмитит reasoning без `<think>` тегов** (Qwen3-Instruct не имеет native thinking)
4. `IsReasoningModel("qwen3-4b")` возвращает **false** (qwen3-4b не в списке)
5. **Парсер не работает** — reasoning text попадает в `content`
6. OpenWebUI видит `content` с reasoning-подобным текстом, без отдельного `reasoning_content`
7. Пользователь видит "неполный ответ" — потому что reasoning перемешан с реальным ответом, без визуального разделения

### Live verification (2026-07-31)

Сделал live test:
- Модель: `qwen3-4b` (Qwen3-Instruct-2507, 2.4GB)
- С reasoning flag (через `IsReasoningModel` = false → НЕ split)
- Запрос: "What is 2+2? Think step by step"
- Streaming response: 23K bytes, 229 lines, finish_reason="stop", 109 completion tokens
- **Response содержит reasoning-подобный текст, НО НЕ в `reasoning_content` поле**

Проверка через `IsReasoningModel("qwen3-4b")`:
- Список по умолчанию: ["qwen3.5", "qwen3.6", "qwen3moe", "qwen35moe", "qwen35", "qwen3-thinking", ...]
- "qwen3-4b" не содержит ни одну из этих подстрок → `IsReasoningModel` = false

## Дополнительные наблюдения

### Performance

При reasoning на 8GB GPU (RTX 3070) с qwen3-4b (2.4GB) + partial offload:
- Simple Q: 4-5 tok/sec
- Thinking Q (21s на 109 токенов): ~6 tok/sec
- Это медленно, но **ОЖИДАЕМО** для partial offload на 8GB

Не связано с багом, но влияет на UX — пользователь видит "зависание".

### OpenWebUI integration

OpenWebUI поддерживает `reasoning_content` поле начиная с версии 0.4+. Если `reasoning_content` отсутствует — UI показывает только `content`. Если в `content` лежит reasoning text — пользователь видит его как ответ.

## Влияние

**Severity: 🟠 MAJOR** потому что:
- Reasoning feature заявлена в CHANGELOG (Round 11/14) и в WebUI
- Но при использовании с моделями вне reasoning-list (Qwen3-Instruct, gemma-4-it, и др.) reasoning маршрутизация ломается
- Пользовательский опыт: "не работает" без видимой ошибки

**Не критично для:**
- Native reasoning моделей (qwen3.5, qwen3.6 MoE, deepseek-r1, gemma-4-thinking) — они в списке

## Временные workarounds для пользователя

1. **Force-add через env var**:
   ```bash
   CPPWORKER_REASONING_ARCHS=qwen3
   ```
   Это добавит "qwen3" в список префиксов. После рестарта cppworker — `IsReasoningModel("qwen3-4b")` = true.

2. **Use native reasoning model** (qwen3.5, qwen3.6 MoE, deepseek-r1) — там `<think>` теги эмитятся нативно.

## Предлагаемый фикс

### Корень проблемы

`IsReasoningModel` — hardcoded список префиксов. Нет механизма для:
- Per-model override через `load-with-params` (нет `enableReasoning: true` в API)
- Использования `cfg.EnableReasoning` как fallback сигнала для parser'а

### Fix (рекомендуемый)

**1. Добавить per-model `EnableReasoning *bool` в `LoadModelOpts`** (internal/cppbackend/backend.go):
```go
type LoadModelOpts struct {
    // ... existing fields ...
    // Round 17: per-model override для reasoning parser.
    //   nil (default) → use cfg.DefaultEnableReasoning
    //   *true / *false → explicit per-model choice
    EnableReasoning *bool
}
```

**2. Хранить на modelInstance** (internal/cppbackend/backend.go):
```go
type modelInstance struct {
    // ... existing fields ...
    reasoningEnabled bool  // resolved at LoadModel time
}
```

**3. Использовать в parser'е** (cmd/cppworker/handlers_openai.go):
```go
// Вместо:
rsIsReasoning := IsReasoningModel(modelName)
// Использовать:
rsIsReasoning := IsReasoningModel(modelName) || inst.reasoningEnabled
```

**4. Add `enableReasoning` в `loadWithParamsRequest`** (cmd/cppworker/types.go):
```go
type loadWithParamsRequest struct {
    // ... existing fields ...
    EnableReasoning *bool `json:"enableReasoning,omitempty"`
}
```

**5. Wire up in handler** (cmd/cppworker/handlers_model.go):
```go
opts := LoadModelOpts{
    // ... existing ...
    EnableReasoning: req.EnableReasoning,
}
```

### Альтернативный (более простой) fix

Без per-model override — просто учитывать `cfg.DefaultEnableReasoning` в parser'е:
```go
rsIsReasoning := IsReasoningModel(modelName) || cfg.DefaultEnableReasoning
```

Это решит проблему "globally enable reasoning = parser doesn't route" но без per-model control.

### Priority

**HIGH** — should be in Round 17 (next round). Workaround (env var) is available now.

## Тесты для добавления

```go
// TestReasoning_PerModelEnable_ForcesSplit — load model with enableReasoning=true,
// даже если IsReasoningModel returns false → parser splits output.
func TestReasoning_PerModelEnable_ForcesSplit(t *testing.T) {
    // Load qwen3-instruct with EnableReasoning=true
    // Verify model has reasoningEnabled=true
    // Send request
    // Verify SSE stream has delta.reasoning_content
}

// TestReasoning_GlobalConfig_AlsoSplits — cfg.DefaultEnableReasoning=true →
// parser splits for non-reasoning-list models.
func TestReasoning_GlobalConfig_AlsoSplits(t *testing.T) {
    cfg.DefaultEnableReasoning = true
    // Send request to qwen3-instruct
    // Verify response has reasoning_content field
}
```

## Альтернативные fixes (отложить)

- **Auto-detect native thinking** — detect `<think>` tags in first 100 tokens and switch parser mode dynamically. Сложно, может давать false positives.
- **Better default list** — include common instruction-tuned models in reasoning list. But "qwen3" alone is too broad.

## Plan

1. **Round 17.1**: Добавить `EnableReasoning` в `LoadModelOpts` + `loadWithParamsRequest`
2. **Round 17.1**: Хранить на `modelInstance`
3. **Round 17.1**: Использовать в parser'е (3 места: writeOpenAIChatStream, writeOpenAICompletionStream, handleV1ChatCompletions, handleV1Completions)
4. **Round 17.1**: Unit-тесты
5. **Round 17.2**: Production verify

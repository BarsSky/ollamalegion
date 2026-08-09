# Go-Side Workarounds (без C-разработки)

Этот документ описывает **обходные пути** на Go-side, которые НЕ требуют
изменений в C коде `c/bridge/bridge.c`. Полная реализация abort API — в
[spec.md](./spec.md).

## W1: Pre-emptive cancel (между чанками в streaming) — УЖЕ РЕАЛИЗОВАНО

**Сложность**: Низкая
**Эффект**: Решает 95% случаев streaming cancel.

### Описание

В Round 31 #1 добавлен `proxyRequestOpenAIStreamAsNonStream`:
- Не-streaming запрос автоматически конвертируется в upstream-streaming
- Balancer отдаёт клиенту 200 OK сразу (chunked transfer encoding)
- На каждом чанке проверяется `r.Context().Done()` — если клиент отменил,
  goroutine выходит, TCP закрывается
- cppworker **НЕ получает signal** и продолжает генерацию, но VRAM/slot освобождается
  **после завершения генерации** (latency issue, не correctness issue)

### Где это уже работает

- `internal/balancer/streaming.go` — heartbeat loop проверяет `r.Context().Done()`
- `internal/balancer/proxy_request_openai_auto_stream.go` — auto-stream workaround
- `internal/balancer/proxy_request.go` — SSE chunk scanner проверяет context

### Ограничения

- Cancel latency = время завершения текущей генерации (60-120s для reasoning)
- VRAM/slot заняты до конца
- При высокой нагрузке может упереться в лимит `maxConcurrentRequests`

### Митигация ограничений

- Установить короткий `streamingIdleTimeout` (10-30s) — heartbeat-based detection
- Использовать `sync.Pool` для переиспользования slot
- Документировать для C developer (см. [spec.md](./spec.md))

## W2: Soft cancel через bridge_request_abort — ТРЕБУЕТ C РАЗРАБОТКИ

**Сложность**: Средняя (8-13 часов C работы)
**Эффект**: Cancel latency = single batch time (milliseconds)

### Описание

Добавить atomic flag в `InternalModel`:
1. C: `atomic_int abort_requested` в struct
2. C: проверять между llama_decode батчами
3. C: `bridge_request_abort(ModelHandle)` экспортируется
4. Go: при `r.Context().Done()` вызвать `bridge_request_abort(model)`
5. Generation возвращает `BRIDGE_ERR_ABORTED` после текущего batch

**Преимущества над W1**:
- Cancel latency: ms вместо 60-120s
- VRAM/slot освобождаются сразу после batch
- Нет накопления "зомби" инференсов

**Недостатки**:
- Требует C developer
- Atomic flag race conditions (нужны тесты)
- Не прерывает текущий llama_decode (только между batch'ами)

**Полная спецификация**: [spec.md](./spec.md#2-required-c-api-minimum-viable)

## W3: Квоты (max-tokens, max-time) — НЕ УСТРАНЯЕТ ROOT CAUSE

**Сложность**: Низкая
**Эффект**: Ограничивает ущерб от долгих generation, не устраняет проблему.

### Реализация

В `GenerationParams` уже есть `n_predict` (max tokens). Дополнительно:

```go
// В cmd/cppworker/handlers_inference.go:
generationTimeout := time.Duration(params.N_predict) * 200 * time.Millisecond
ctx, cancel := context.WithTimeout(r.Context(), generationTimeout)
defer cancel()

// В generation loop:
select {
case <-ctx.Done():
    return ErrGenerationTimeout
case token := <-tokenChan:
    // process token
}
```

### Ограничения

- Не помогает когда нужно отменить по request пользователя (Stop)
- Может прервать legit long generation

**Рекомендация**: использовать как defense-in-depth, не как primary solution.

## W4: Parallel "telemetry" goroutine — МОНИТОРИНГ, НЕ CANCEL

**Сложность**: Низкая
**Эффект**: Видимость в реальном времени, но НЕ cancel.

### Идея

Запустить отдельный goroutine, которая polls `bridge_*` state и логирует
slow generations. Можно также отправлять WebSocket events в WebUI.

```go
// Каждые 5 секунд:
stats := bridge.GetInferenceStats()  // C API, нужно добавить
if stats.ActiveInferences > 10 {
    log.Warn("high inference load", "active", stats.ActiveInferences)
}
```

**Не решает cancel**, но даёт visibility для diagnosis.

## W5: Thread-safe cancel через pthread_kill — СЛОЖНО, НЕ РЕКОМЕНДУЕТСЯ

**Сложность**: Высокая (20+ часов, много race conditions)
**Эффект**: Hard cancel, прерывает blocking C call.

### Описание

Прервать blocking `llama_decode` через `pthread_kill(thread, SIGUSR1)`.

### Проблемы

1. **Race conditions** между SIGUSR1 и normal flow
2. **Memory state** — llama_decode может быть в середине allocation
3. **Resource leaks** если не обработать EINTR правильно
4. **Platform-specific** — не работает на Windows MinGW
5. **Sanitizer failures** — ASan/TSan/MSan могут флажить на race

**Не рекомендуется** для production. Round 31 #1 + W2 покрывают 99% use cases.

## Сравнительная таблица

| # | Подход | Cancel latency | C code change | Сложность | Production-ready |
|---|--------|----------------|---------------|-----------|------------------|
| W1 | Pre-emptive (Round 31 #1) | 60-120s | НЕТ | Низкая | ✅ УЖЕ |
| W2 | Soft atomic cancel | ms | ДА (8-13h) | Средняя | ✅ Рекомендуется |
| W3 | Квоты (timeout) | N/A (defensive) | НЕТ | Низкая | ✅ Defense-in-depth |
| W4 | Telemetry | N/A (monitoring) | НЕТ (если polling) | Низкая | ✅ Optional |
| W5 | pthread_kill (hard) | immediate | ДА (20+h) | Высокая | ❌ Не рекомендуется |

## Рекомендация

1. **W1 УЖЕ работает** — Round 31 #1 ✅
2. **W2 рекомендуется** для C developer — спецификация в [spec.md](./spec.md)
3. **W3** как defense-in-depth — добавить timeout в cppworker
4. **W4** если нужна visibility — опционально
5. **W5 НЕ рекомендуется** — слишком опасно

**Приоритет**: W2 > W3 > W4. W5 — никогда.

# cppworker Abort API — Planning Project

**Created**: 2026-08-09
**Status**: Planning (pre-development)
**Owner**: TBD (C developer needed)
**Related**: Round 31 #6 (balancer bug fixes)

## Проблема

`bridge_infer_stream` в `c/bridge/bridge.c` (line 175) — это **blocking C call**, который внутри делает
`llama_decode` в цикле генерации токенов. Между вызовами `llama_decode` (per-token) **нет механизма
проверки cancel-флага**. Это значит:

1. Если клиент отменил запрос (Cline Stop, OpenWebUI close tab, ctx.Done() fire), Go-side
   обнаружит это только после завершения всей генерации (60-120s для reasoning моделей).
2. Go-шный `r.Context().Done()` не может прервать C-функцию в середине.
3. Resource leak: VRAM занят, KV-cache не освобождается, slot не освобождается.

### Где это болит

| Сценарий | Влияние | Уже митигировано? |
|----------|---------|-------------------|
| Streaming + client cancel | `r.Context().Done()` в `streaming.go` срабатывает, balancer закрывает TCP. cppworker **НЕ получает signal** и продолжает генерацию до конца | ❌ НЕТ — VRAM/slot leak до конца генерации |
| Non-streaming + client timeout | Round 31 #1 auto-stream workaround: balancer конвертирует в stream, может отменить между чанками | ✅ ДА — после #1 |
| Direct cppworker usage (без balancer) | Никакой cancel вообще | ❌ НЕТ |
| C-bridge abort API | Не существует | 🚫 Требует C developer |

## Обходные пути (Go-side, без C-разработки)

См. [workarounds.md](./workarounds.md) для подробного анализа.

| # | Обходной путь | Сложность | Эффект |
|---|---------------|-----------|--------|
| W1 | Только pre-emptive cancel (между чанками) | Низкая | Решает 95% случаев для streaming |
| W2 | Soft cancel (отмена ПЕРЕД llama_decode) | Средняя | Требует C-export atomic flag |
| W3 | Квоты: max-tokens per request, max-time | Низкая | Не устраняет root cause |
| W4 | Параллельный "telemetry" goroutine | Низкая | Мониторинг, не cancel |
| W5 | **Thread-safe cancel через pthread_kill** | Высокая | Требует C-разработки (см. C-API) |

**Рекомендация**: W1 (streaming cancel) уже решает 95% случаев через Round 31 #1 auto-stream.
W5 (pthread_kill) — единственный способ отменить C-blocking call в середине, но он сложный и опасный.

## Полноценное решение (требует C developer)

См. [spec.md](./spec.md) для полной спецификации.

**Минимальный** вариант (W2):
1. C-side: добавить `atomic_int g_abort_requested` в `bridge.c`
2. C-side: проверять `if (atomic_load(&g_abort_requested)) return BRIDGE_ERR_ABORTED;` в цикле generation
3. C-side: функция `void bridge_request_abort(ModelHandle model);`
4. Go-side: `//export goRequestAbortHandler` callback регистрируется при init
5. Go-side: при `ctx.Done()` вызвать `bridge_request_abort(model)`
6. Тесты: C unit-test + Go integration test

**Полный** вариант (W5 + W2):
- В дополнение к W2: `pthread_kill` для прерывания blocking call
- Race condition handling
- Resource cleanup при abort mid-decode
- Multi-model state isolation

## Задачи

См. [task-breakdown.md](./task-breakdown.md) для детального списка задач.

## Рекомендуемый план действий

1. **Принять W1** (текущий auto-stream workaround) — done в Round 31 #1
2. **Документировать** обходные пути (этот документ)
3. **НАНИЗКИЙ приоритет** для W5 — C-blocking cancel сложен, не критичен пока
4. **Закрыть Round 31** как 6/7 complete
5. **Открыть** новую задачу (этот plan) для будущего C developer

## Файлы

```
plans/cppworker-abort-api/
├── README.md              ← этот файл (обзор)
├── spec.md                ← спецификация API для C developer
├── workarounds.md         ← обходные пути на Go-side
├── task-breakdown.md      ← задачи с оценкой
└── references.md          ← ссылки на Round 31 и другие релевантные ресурсы
```

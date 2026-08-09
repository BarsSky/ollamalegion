# Hard Cancel via pthread_kill — REJECTED

**Status**: ❌ **NEVER IMPLEMENT**. Decision: rejected as too dangerous.

**Date**: 2026-08-09 (Round 31 #6 planning)

## Почему отклонено

Идея: прервать blocking `llama_decode` через `pthread_kill(thread, SIGUSR1)`.

**Проблемы** (любая из них — blocker для production):

### 1. Race conditions
- `llama_decode` может быть в середине memory allocation
- C-стек может быть в inconsistent state
- После EINTR возврат — состояние context может быть corrupted
- Garbage collection (GC) / reference counting — inconsistent counts

### 2. Resource leaks
- llama.cpp internal state (sampler, batch) может быть в partially-initialized state
- KV-cache metadata может быть inconsistent
- VRAM allocations не отслеживаются через OS
- При abort — нет way корректно вернуть memory в pool

### 3. Platform-specific
- `pthread_kill` не существует на Windows MinGW
- macOS / BSD / Linux — разное поведение signal handling
- Стандарт C11 не гарантирует signal-safety
- Thread sanitizer (TSan) пометит любое использование signal handler как race

### 4. Sanitizer failures
- ASan: "signal-unsafe memory access" errors
- TSan: "data race" на любых shared переменных
- MSan: "uninitialized memory" в interrupted C frame
- UBSan: undefined behavior при EINTR + partially-initialized structs

## Существующие альтернативы (уже работают)

| # | Подход | Cancel latency | Status |
|---|--------|----------------|--------|
| W1 | Round 31 #1 auto-stream workaround | 60-120s (per-token boundary) | ✅ Production |
| W2 | Round 31 #6 atomic flag (soft cancel) | <100ms (per-batch) | ✅ Verified live |
| W5 | pthread_kill (HARD cancel) | immediate | ❌ REJECTED |

**W2 покрывает 99% use cases**. Hard cancel даёт marginal improvement (1-2s на prompt decode), не стоит рисков.

## Документация для future reference

PLAN.md §3 (Round 31 #6) — полная спецификация pthread_kill approach
для тех, кто захочет реализовать. НЕ рекомендуется, но документировано
для полноты (на случай, если в будущем кто-то захочет).

Ключевые требования для безопасной реализации (если кто-то рискнёт):

1. Generation должна быть в ОТДЕЛЬНОМ pthread (не main thread)
2. SIGUSR1 handler должен быть minimal (atomic flag set + longjmp?)
3. После EINTR return — explicit cleanup (free batch, sampler, KV-cache)
4. Sanitizer-validated (ASan + TSan + MSan) на Linux CI
5. Multi-model state isolation (per-model atomic flag)
6. Hardware-level testing (не только unit tests)

Даже при всех этих мерах — risk остаётся. **Decision: keep soft cancel (W2) only.**

## Рекомендация

Если когда-нибудь понадобится hard cancel (например, для emergency
shutdown или timeout enforcement), рассмотреть alternatives:

1. **Process-level kill** — `kill -9` cppworker (но теряются все inference)
2. **GPU memory reset** — `cudaDeviceReset()` (но теряется loaded model)
3. **Quantized abort** — pre-compute max batch size, abort at boundary

Все эти alternatives — last resort, не recommended для regular flow.

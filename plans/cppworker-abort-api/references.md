# References — Round 31 & Abort API Context

## Round 31 — что сделано

Полная история Round 31 в agent memory (`MEMORY.md`):
- Round 31 v31r1: profile persistence defense-in-depth
- Round 31 v31r2: NDJSON heartbeat fix
- Round 31 v31r3: SplitReasoningContent leading `\n` strip (streaming stateful)
- Round 31 v31r4: Async n_ctx reload + 503+Retry-After
- Round 31 v31r4-final: Token usage tracking
- Round 31 v31r5: Non-streaming header workaround (auto-stream)
- Round 31 #6: **C-bridge abort API** (этот документ)

## Связанные Round 31 фиксы

### Round 31 #1 (auto-stream workaround) — решает 95% cancel cases

**Файл**: `internal/balancer/proxy_request_openai_auto_stream.go`

Balancer сам конвертирует non-stream в upstream-streaming:
- `stream: true` в upstream body
- Streaming HTTP client → headers immediately
- Аккумулирует SSE чанки
- Финальный chunk → non-stream JSON клиенту
- Cancel via `r.Context().Done()` работает между чанками

**Env var**: `LB_OPENAI_AUTO_STREAM=true`

**Что осталось**: cppworker не получает signal об отмене, продолжает генерацию
до конца (но VRAM/slot освобождаются в конце, не сразу).

### Round 31 #1 + W2 (abort API) = complete cancel solution

- W1 (auto-stream): balancer-side cancel
- W2 (abort API): cppworker-side cancel в C

**С W2**:
- Latency: ms (single batch time)
- VRAM: освобождается immediately
- Multi-client: каждый cancel не влияет на других

## Архитектура cppworker

### Текущая структура

```
cppworker (Go process)
├── cmd/cppworker/main.go              — entry point
├── cmd/cppworker/handlers_inference.go — HTTP /v1/chat/completions
├── cmd/cppworker/handlers_model.go    — LoadModel
├── c/bridge/                          — CGo bindings
│   ├── bridge.h                       — public C API
│   ├── bridge.c                       — implementation
│   ├── bridge.go                      — Go bindings
│   ├── bridge_internal.h              — InternalModel struct
│   ├── bridge_stub.go                 — stub for tests (-tags llama_stub)
│   └── CMakeLists.txt
└── pkg/...                            — shared types
```

### Threading model

- Go: один goroutine per HTTP request
- C: bridge_infer_stream блокирует вызывающую goroutine
- llama_decode: single-threaded внутри (per model)
- Multi-model: параллельные inference в разных goroutines

### Текущий API (без abort)

```c
// Один public function для streaming inference:
int bridge_infer_stream(
    ModelHandle model,
    const GenerationParams* params,
    StreamCallback callback,    // C callback per token
    void* user_data,            // opaque ptr (обычно Go closure)
    InferenceResult* result     // out param
);
```

## Cancel scenarios

| Сценарий | Текущее поведение | W1 (auto-stream) | W2 (abort API) |
|----------|-------------------|------------------|----------------|
| Cline Stop на streaming | TCP close → cppworker не узнаёт, генерит до конца | ✅ Cancel между чанками (latency: 60-120s) | ✅ Cancel immediately |
| OpenWebUI close tab | Same | ✅ Same | ✅ |
| Cline non-stream timeout (120-300s) | Round 31 #1: auto-stream → balancer cancel между чанками | ✅ Latency 60-120s | ✅ Latency ms |
| Cline Cancel button на non-stream | Same | ✅ | ✅ |
| Network error mid-generation | TCP close → cppworker detect EOF, exit | ✅ (early) | ✅ |
| cppworker shutdown | OS kill | ❌ | ⚠️ bridge_request_abort_all (Phase 1.6) |
| Multi-request на одной model | Каждый infer отдельный stream | ✅ Не влияет | ✅ Per-model abort |

## Production metrics

**Cline experience без abort API**:
- Cancel latency: 60-120s (для reasoning моделей с длинной generation)
- VRAM usage peaks: до `n_concurrent × 5GB` для gemma-4
- Slot exhaustion: `maxConcurrentRequests=4` (типично) → request queue overflow

**Cline experience с abort API**:
- Cancel latency: <100ms (single batch)
- VRAM usage: constant baseline + 1 active
- Slot exhaustion: не происходит (slots освобождаются immediately)

## Build & deploy notes

### Round 31 #1 build
```bash
# Без W2 (auto-stream only):
docker buildx build --tag ollama-legion/balancer:cppworker-bundled-full-v31r5 ...
```

### Round 31 + W2 build
```bash
# После W2 (нужен C rebuild):
cd c/bridge && mkdir -p build && cd build && cmake .. && make -j
docker buildx build --tag ollama-legion/balancer:cppworker-bundled-full-v31r6 ...
```

### Image tags (cumulative)
- `ollama-legion/balancer:cppworker-bundled-full-v31r5` — Round 31 #1-#7 (auto-stream, no abort)
- `ollama-legion/balancer:cppworker-bundled-full-v31r6` — TBD (after W2)
- `ollama-legion/cppworker:gpu-86` — cppworker image (rebuild needed for W2)

## External resources

- **llama.cpp docs**: https://github.com/ggerganov/llama.cpp/blob/master/README.md
- **C11 stdatomic**: https://en.cppreference.com/w/c/atomic/atomic
- **llama_decode contract**: `llama_decode()` в `llama.cpp/llama.h` — single batch forward pass
- **Go cgo**: https://pkg.go.dev/cmd/cgo
- **Round 31 background**: см. `MEMORY.md` секции "Round 31" в agent memory

## Glossary

- **abort**: прерывание in-flight generation до естественного завершения
- **cancel**: то же самое что abort (alias)
- **pre-emptive cancel**: cancel между чанками (W1)
- **soft cancel**: cancel через atomic flag (W2)
- **hard cancel**: прерывание blocking C call через signal (W5, NOT recommended)
- **slot**: conceptual unit of concurrent inference capacity (`maxConcurrentRequests`)
- **KV-cache**: per-request key-value cache в GPU memory
- **batch**: single llama_decode call (may process multiple tokens)
- **streaming vs non-streaming**: client request format (OpenAI stream:true/false)

# Cline agent loop through the balancer: contract and verification

> **Version:** 1.0 (2026-10-13), R113
> **Applies to:** `cmd/cppworker` (paths `/api/chat`, `/v1/chat/completions`),
> `internal/cppbackend` (model state)
> **Verification tools:** [`scripts/trace-cline-turn.js`](../../scripts/trace-cline-turn.js),
> [`scripts/cline-agent-loop.js`](../../scripts/cline-agent-loop.js),
> [`scripts/extract-cline-request.js`](../../scripts/extract-cline-request.js)
> **Related documents:** [`BALANCER_CPPWORKER_API_CONTRACT.md`](../architecture/balancer-cppworker-contract.md),
> [`runbook-tools.md`](../operations/runbook-tools.md), [`cppworker-model-params.md`](cppworker-model-params.md)

The document answers three questions:

1. [What exactly goes to the client in a single turn and how to see it](#1-trace-of-a-single-turn)
2. [How to verify the LOOP (several iterations), not a single turn](#2-verifying-the-agent-loop)
3. [Which contracts must be honored (and what was broken)](#3-contracts)

---

## 1. Trace of a single turn

```powershell
# 1. Once, extract the REAL Cline request from its log (both the prompt and the
#    response parsing are set by the tool names and schemas — a "similar" payload is useless).
node scripts/extract-cline-request.js "List the files in the current working directory."

# 2. One turn with full tracing: raw chunks + parsing.
node scripts/trace-cline-turn.js --url http://127.0.0.1:18080/api/chat `
     --payload debug/cline-real-request.json
```

What the tracer prints:

| Field | Meaning |
|---|---|
| `1-й content` | time to the first content delta; `НЕТ` = the server did not stream text (the client waited in silence) |
| `content` / `thinking` | how many characters went into each field |
| `tool_calls` | names and the **shape** of the arguments (`object` — an executable call, `array`/`string` — not) |
| `done` | `done_reason`; `done:true` with zero output = the client will show "COMPLETE" without having received anything |
| `УТЕЧКИ` | call markup / reasoning markers / service tokens **in content** |

Raw chunks are saved to `debug/trace-turn-<ts>.ndjson` — this is exactly the "explicit
tracking of the response": you can see byte for byte what the client received.

Return code `1` means the turn is incorrect (a leak, arguments that are not an
object, neither content nor a call).

---

## 2. Verifying the agent loop

A single turn does not show the main thing: whether work CONTINUES after the tool
call. The harness plays the role of Cline: it receives `tool_calls`, returns the tool
result and repeats the turn; on every iteration the protocol is checked.

```powershell
node scripts/cline-agent-loop.js --url http://127.0.0.1:18080/api/chat `
     --payload debug/cline-real-request.json --iterations 4
```

**The script does NOT execute the commands the model chose** — the tool result is a
deterministic stub. Only the protocol is checked:

* call `arguments` — a JSON object consistent with the tool schema from the
  request (declared parameters, required parameters, types);
* `content` has no call markup, no reasoning markers and no service tokens;
* `done_reason` = `tool_calls` when there is a call and `stop`/empty on the final answer;
* the loop continues after the tool result instead of breaking off after 1-2 turns.

The behavior "a request arrived while the model is loading" is checked separately —
with concurrent requests:

```powershell
node scripts/parallel-turn-probe.js --url http://127.0.0.1:18080/api/chat `
     --payload debug/cline-real-request.json --count 3
```

Before R113 the second and third requests got `done:true` in 0.15 s (the client had
nothing to show); after it they wait for readiness and answer just like the first one.

A live run BEFORE R113 (a real Cline request, gemma-4-E4B, 25 tools):

```
--- итерация 1 --- tool_calls 1 | done_reason=tool_calls
  вызов: run_commands args=["dir"]                     ← arguments МАССИВ
--- итерация 2 --- content 2456 | tool_calls 1
  вызов: run_commands args=["dir"]
  УТЕЧКА: сырой JSON вызова в content: "tion":{"name":"run_"
--- итерация 3 --- tool_calls 0 | done_reason=stop     ← цикл кончился ничем
```

---

## 3. Contracts

### 3.1 Call arguments are ALWAYS a JSON object per the request schema

`message.tool_calls[].function.arguments` must be a JSON object whose keys are
declared in `tools[].function.parameters` of the request.

The model does not guarantee this. Live shapes that arrived on the bench:

| What the model returned | What went to the client | Consequence |
|---|---|---|
| `run_commands` + `["ls -A"]` | `arguments: ["ls -A"]` | the client cannot execute the call |
| `list_files` + `"c:\\Ollama\\ollamalegion"` | `arguments: "c:\\…"` | same |
| `run_commands{[…]}` (gemma native tag) | `arguments: […]` | same |

**Coercion rule** (R113, `cmd/cppworker/tool_args_schema_r113.go`) —
deterministic and driven by the schema FROM THE REQUEST, with no model names:

| Value | Where it goes |
|---|---|
| object | stays as is (a `{"args":{…}}`/`{"kwargs":{…}}` wrapper is unwrapped if the key is not declared) |
| array | the first declared array parameter (`required` takes priority): `run_commands.commands`, `read_files.files`, `search_codebase.queries` |
| scalar | the only parameter, otherwise the first `required` string one, otherwise the first one — in the order declared in the schema (`editor` → `path`) |
| a string with JSON inside | is unwrapped and processed again |
| the tool is not declared, the value is not an object | the call is **dropped** (guessing a parameter name is not allowed) |

The parameter order is taken from the client schema (a decoder, not a map) — which is
why the choice for a single value is reproducible from run to run.

### 3.2 Call markup does not leak into content

If the model emitted call markup (`<|tool_call>…<tool_call|>`, `<tool_call>…`,
`{"tool_call":…}`, `[TOOL_CALLS]`, a markdown fence with JSON) and it could not be
parsed, it **must not** be given to the client as text: an agent client treats such a
message as the final answer and marks the task as done. R113 behavior:

1. regenerate (up to `maxEmptyOutputRetries`) until not a byte has been sent to the
   client — sampling is stochastic, a retry usually yields a correct call;
2. if the markup is still there — an error chunk with `done_reason: "error"` (the client sees
   the problem, not "done");
3. in a "raw" text answer the markup is cut out (`stripToolCallMarkup`).

A separate case is **a call echoed from the history**. The client sends a second turn in which the
assistant's call is recorded as a JSON object; the bridge puts it into the prompt as text, and the
model repeats it in the answer:

```
… I will execute it again.
[[{"id":"call_1","type":"function","function":{"name":"run_commands",
   "arguments":{"commands":["dir"]}}}]]
```

Such JSON is recognized as a **call** (`recoverToolCallsFromText`): the client
receives `tool_calls` and keeps working, instead of getting a "tool in the response body" with the
loop breaking after the first iteration. If it could not be recognized — the JSON is still
cut out of the visible text.

Deltas are held back not only when the answer BEGINS with JSON/markup, but also when the
JSON form of the call appears INSIDE prose (`toolsStreamJSONCallPos`).

### 3.3 A request to a loading model WAITS for readiness

`/api/chat` (and the other client paths) does not answer with an error while the model
is loading or reloading (another client's n_ctx, auto-tune, a concurrent
request). Behavior: wait for readiness (`cppbackend.WaitForModelReady`),
the limit being `CPPWORKER_LOAD_WAIT_SEC` (900 s by default) or the client's
deadline/cancellation.

Why this matters: before, "model loaded" meant "there is an entry in `b.models`",
and the entry appears BEFORE loading finishes. Generation failed with
`model X is not loaded (state: loading)`, the streaming path turned that into
`done:true, done_reason:"error"` — and the client showed a completed turn without having received
a single token (the complaint "COMPLETE arrives before any streaming of the answer generation
even happens"). If readiness was not reached in time, the client
gets 503 + `Retry-After` (retry), not "done".

### 3.4 The tool result reaches the model

The message `{"role":"tool","tool_name":"…","content":"…"}` (and the OpenAI form with
`name`/`tool_call_id`) lands in the prompt as `tool_call_result(<name>): …`, which is why
the next turn sees the result. The `tool_name` field is accepted on par with `name`
(otherwise the second turn of the loop failed with 400 and the loop broke after the first call).

---

## 4. What to look at on the server

One request = one line in the cppworker log:

```
/api/chat stream response summary  model=… streamed_content_bytes=… 
final_content_len=… thinking_len=… tool_calls=1 tool_names=run_commands 
tool_arg_shapes=object done_reason=tool_calls raw_len=… elapsed_ms=…
```

`tool_arg_shapes=object` — the client will be able to execute the call; `array`/`string` —
a coercion defect. `streamed_content_bytes=0` during a long generation means that the
server held the output until the end (the client saw no stream).

---

## 5. Streaming contract for Cline (R231, 2026-10-24)

Cline reads OpenAI SSE and concatenates `delta.content`; anything that arrives on the wrong
channel or in the wrong place breaks the session (the agent "completes" the turn or tears the stream).
After R231 the contract is fixed and verified automatically.

**Response invariants:**

1. `reasoning_content` and `content` are not mixed. In the stream, reasoning goes
   ONLY to `reasoning_content`; `content` is the visible answer only. An unterminated
   reasoning block is not copied into `content` (that used to be a duplicate), but yields an honest
   `finish_reason=length`.
2. The visible `content` has no service markers: `</think>`, `<think>`,
   `<|channel>thought`, `<channel|>`, `<end_of_turn>`, `<|im_end|>` and the like.
   It is cleaned both in the worker (parser + deltas) and at the balancer boundary (SSE filter) —
   even if the worker is old.
3. `[DONE]` arrives **exactly once** and as the **last** event; after it there is not
   a single data event. `finish_reason` comes before `[DONE]`.
4. Errors arrive as errors: `finish_reason:"error"` + `error`, not an empty 200.

**Verification (live, bench):**

```bash
node scripts/verify-stream-r231.js
# PASS: 1. HTTP 200; 1a no markers; 1b no duplicate; 1c a single [DONE];
#       1d [DONE] is last; 1e nothing after [DONE]; 1f empty content -> length
```

**Client contract (OpenAI + Ollama, including tools):**

```bash
node scripts/clients/contract-suite.js
```

Block 6 in it is exactly this scenario (a stream without tools): markers, a duplicate,
`[DONE]` single and last. In the gate it is run as
`scripts/ci.ps1 -Live -Clients`.

**If a regression is back:** look in the worker log for
`writeOpenAIChatStream: блок размышлений не закрыт` (cause `length`) and
`translateSSEChatToOllama: stripped reasoning markers` / `stripped service tokens`
(what the balancer cleaned).

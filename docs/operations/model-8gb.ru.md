# Модель на 8 ГБ VRAM: выбор, загрузка, переключение (R415/R416)

Практика для стенда с одной картой 8 ГБ (например, RTX 3070 / A10 8 ГБ) и
`cppworker` под управлением балансера. Цифры — с живого стенда 2026-11-03
(Qwen3.8-9B-Distill-uncensored-heretic.Q4_K_M, 5.38 ГБ).

## 1. Что влезает: простая арифметика

Полный offload (все слои на GPU) возможен, если

```
веса + KV-cache + compute-буферы  ≤  memoryFree − CPPWORKER_VRAM_OVERHEAD_MB
```

- **веса**: размер `.gguf` минус тензоры, которые остаются на CPU
  (token_embd/output — единицы процентов);
- **KV-cache**: у гибридных моделей (SSM/Mamba-слои) он маленький; у обычных
  attention-моделей ≈ `2 × n_layers × n_kv_heads × head_dim × ctx × байт`;
  `q8_0`/`q4_0` уменьшают его в 2/4 раза;
- **compute-буферы**: ~0.5-0.7 ГБ при `batch=256` и flash-attention;
- **запас** (`CPPWORKER_VRAM_OVERHEAD_MB`, default 2048): вычитается из
  свободной VRAM до расчёта, memfit по нему решает, сколько слоёв отдать GPU.

На 8 ГБ карте (свободно ~7.05 ГБ после выгрузки) запас 2048 оставляет ~5.0 ГБ:
это значит, что **9B-Q4 (5.38 ГБ) не грузится целиком** — memfit выбирает
29-33 слоя из 32+ и часть считает на CPU (≈1.5 ток/с). Запас 512 даёт ~6.5 ГБ:
9B-Q4 грузится всеми слоями (33/32) и выдаёт **8-15 ток/с**.

Практические ориентиры для 8 ГБ (запас 512, ctx 16k, KV q8_0):

| Класс | Q4_K_M | Полный offload | Скорость (≈) |
|---|---|---|---|
| 4B | ~2.5 ГБ | да | 20-40 ток/с |
| 9B | ~5.4 ГБ | да (впритык) | 8-27 ток/с |
| 14B | ~9 ГБ | нет (частично, 60-70 % слоёв) | 3-6 ток/с |
| 27B | ~15 ГБ | нет (единицы слоёв) | <1 ток/с |

**Проверено живьём на Qwen3.8-9B (гибрид, KV маленький):** окно 32k и даже
**64k** грузятся всеми слоями (`gpu_layers=33/32`) при запасе 512 — для гибридных
моделей KV почти не растёт с окном. Короткий ответ на 16k ≈ 5-8 с, на 32k ≈ 9 с;
prefill ~12k токенов ≈ 30 с (≈400 ток/с). Первый запрос после idle-unload (10 мин)
платит загрузку ~90-170 с.

⚠️ **Оценка prompt в балансере консервативна для кириллицы** (1 символ ≈ 1 токен
в `pkg/tokencount`): на повторяющемся русском тексте она завышает в ~5 раз
(12k реальных токенов → 59k оценка) и может запустить лишний рост окна до
потолка. Пока это лечится запасом потолка (`LB_NCTX_RELOAD_MAX_N_CTX`), а не
оценкой; точный фикс — считать prompt токенизатором модели при приближении к
границе окна (см. план в troubleshooting §2.16).

## 2. Где лежат модели и как скачать

`/app/models` в контейнере воркера — **bind** на `D:\ollama-legion-models`.
Скачивать можно любым способом; воркер сам увидит новый `.gguf` (сканер
`/api/models/files`, TTL 30 с). Проверка:

```bash
curl -s http://127.0.0.1:18092/api/models/files | jq '.files[].name'
```

Прямая ссылка HF (пример):

```bash
curl -L -C - -o "D:\ollama-legion-models\<name>.gguf.part" \
  "https://huggingface.co/<repo>/resolve/main/<file>.gguf?download=true"
# после завершения переименовать .part -> .gguf
```

Незавершённый файл держите под именем `.part`: сканер увидит `.gguf` только
после переименования.

## 3. Профиль модели (окно, KV, слои)

Профиль задаётся через API (или карточку модели в WebUI):

```bash
curl -b cookies -X PUT \
  "http://127.0.0.1:18080/api/v1/cppworker/model-profiles/<model-name>" \
  -H "Content-Type: application/json" -H "X-CSRF-Token: <csrf>" -d '{
    "contextLength": 16384,
    "batchSize": 256,
    "numGpuLayers": -1,
    "flashAttn": true,
    "useMmap": true,
    "kvCacheType": "q8_0",
    "contextLengthAuto": true,
    "contextLengthMax": 32768,
    "parallel": 1,
    "notes": "8 GB: 16k ctx, q8_0 KV, full offload"
  }'
```

- `numGpuLayers: -1` — «все слои»; `-2` — «auto» (memfit решит сам);
- `kvCacheType: q8_0` экономит VRAM; `q4_0` — ещё экономнее, но качество ниже;
- явный `kvCacheType` в профиле **не оспаривается** AutoTune (R416);
- `contextLength: 16384` — компромисс для 8 ГБ; `contextLengthMax: 32768`
  разрешает авто-рост, если VRAM позволит.

## 4. Загрузка и переключение

Первый запрос к модели лениво её загружает:

```bash
curl -s http://127.0.0.1:18080/api/chat -H "Content-Type: application/json" -d '{
  "model":"<model-name>",
  "messages":[{"role":"user","content":"Say OK"}],
  "stream":false,"think":false,"options":{"num_ctx":16384}
}'
```

Старую модель выгрузить (чтобы освободить VRAM):

```bash
curl -X POST "http://127.0.0.1:18092/api/models/unload?name=<old-model>" \
  -H "X-API-Token: $CPPWORKER_API_TOKEN"
```

Проверка, что загрузилось всё:

```bash
curl -s http://127.0.0.1:18092/api/models | jq '.models[] | {name,state,gpu_layers_actual,n_layers,context_size,kv_cache_type}'
# gpu_layers_actual >= n_layers  → полный offload
```

В логе воркера: `model loaded … gpuLayers=33` и
`auto-adapting GPU layers … stage=exact_fit`.

## 5. Типичные ошибки

| Симптом | Причина | Что делать |
|---|---|---|
| `state=loading` после ответа, `loaded_at` меняется | AutoTune reload loop | R416+: проверить, что нет `triggering async reload`; профиль с явным `kvCacheType` |
| `gpu_layers_actual` < `n_layers`, ~1.5 ток/с | запас VRAM 2048 не даёт места | `CPPWORKER_VRAM_OVERHEAD_MB=512` и перезагрузка модели |
| загрузка падает, в логе `~llama_context` | OOM при выгрузке/загрузке (две модели в VRAM) | выгрузить старую модель, затем грузить новую |
| `413 n_ctx_infeasible` при ctx 32k | 32k на 8 ГБ не влезает с этим KV/слоями | уменьшить `contextLength`/`kvCacheType` до q4_0 |
| Cline «только размышления, видимого ответа нет» при валидных tool_calls | до R417 в content подставлялось пояснение | R417+: content пуст, вызовы исполняются |
| обрывы ответа на 4096 токенах | `n_predict` дефолт (R411/R415) | reasoning-модель должна быть в `ReasoningArchPrefixes` (qwen3.8, gemma-4 и т.д.) |

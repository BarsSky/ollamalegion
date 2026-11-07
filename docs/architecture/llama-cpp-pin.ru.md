# Версия llama.cpp в проекте: что зафиксировано и что даст обновление

**Дата:** 2026-11-07 · **Проверено по локальному клону `c/llama.cpp`** (никаких
предположений о внешнем репозитории: все числа ниже получены из git этого
сабмодуля).

---

## 1. Где мы стоим

```
$ git submodule status
 0eadefebd3f8f92a86d634a0e5b8fffc9dc792c0 c/llama.cpp (b10731)
```

| Параметр | Значение |
|---|---|
| Наш коммит | `0eadefebd` («qwen4exp: support recurrent state rollback», #28123) |
| Тег | **b10731** |
| Дата коммита | **2026-09-01** |
| Что видит воркер | `llama_build=0eadefebd`, `llama_build_date=2026-09-01` |
| Последний доступный локально тег | **b11844**, 2026-10-23 |
| Коммитов между ними | **1113** |

## 2. Что войдёт в обновление (по локальной истории)

```
1113 коммитов, b10731 → b11844 (2026-09-01 → 2026-10-23)

jinja (шаблоны чата)        21   исправления движка шаблонов
server                      66   ручки, rerank, realtime API
chat/template/tool          34   разбор вызовов и формат сообщений
kv cache                    32   кэш, слоты, память
flash-attn (fattn)          47   ядра внимания
cuda/metal/vulkan/sycl     304   бэкенды и ядра
qwen                        31   архитектура/оптимизации
gemma                       12
deepseek/glm/nemotron/…     29   новые архитектуры
tokenizer/vocab             15   токенизация
rpc                         11   раскладка на донора
```

**Почему это важно именно нам.** Наши больные места R426–R471 — это ровно те
подсистемы, которые в этих 1113 коммитах правились чаще всего:

1. **Разбор вызовов инструментов.** `chat : name tool and argument parser rules by
   index` (#30088), `chat-peg-parser: clear current_tool when pending_tool_call is
   reset` (#29942), `chat: fix jinja parser for TranslateGemma` (#30096). Наши
   R428/R430/R432/R441 — это ручная компенсация того, что разбор не всегда
   срабатывал; часть этих костылей после обновления может оказаться ненужной.
2. **Движок шаблонов jinja** — 21 исправление (`unique` filter, `tojson` для
   nan/inf и float как в Python, hex-escape, область видимости в циклах, скорость
   копирования контекста). У нас R434 — «инструменты в шаблон»: чем точнее
   шаблон, тем реже модель видит не тот формат вызова.
3. **Новые архитектуры** (gemma/qwen/deepseek/glm/nemotron/minicpm) — модели,
   которые сейчас просто не загрузятся, начнут загружаться без правок нашего кода.
4. **RPC** (11 коммитов) — раскладка слоёв на донора: наш единственный способ
   поднять 27B на двух 8-ГБ картах.
5. **KV-кэш и flash-attn** (79 коммитов) — прямое влияние на то, какое окно
   влезает в VRAM, то есть на R466/R472-арифметику.
6. **Токенизация** (15) — точность `POST /api/tokenize`, который теперь кормит
   preflight (R473): от него напрямую зависит выбор окна.

## 3. Совместимость с нашим мостом (проверено по заголовкам)

Наш C-мост использует (`c/bridge`):

```
llama_model_load_from_file / llama_model_free / llama_init_from_model
llama_model_get_vocab / llama_tokenize / llama_token_to_piece
llama_batch_init / llama_batch_get_one / llama_batch_free
llama_decode / llama_get_logits_ith / llama_get_memory
llama_memory_clear / llama_memory_seq_rm
llama_sampler_chain_init и сэмплеры
llama_chat_apply_template (C API)
common_chat_templates_init / common_chat_templates_apply / common_chat_format
```

Сверка b10731 ↔ b11844 по `include/llama.h`, `common/chat.h`, `common/common.h`:

- **сигнатуры, которые мы вызываем, не изменились**, в том числе
  `common_chat_templates_apply(..., const common_chat_templates_inputs&) →
  struct common_chat_params` и enum `common_chat_format`;
- изменения в `common/chat.h` (+166 строк) — **аддитивные**:
  `common_chat_msg_content_parts`, `common_chat_msg_diff::compute_diffs`, рост
  `common_chat_params`;
- риск переименований в `chat` API заявлялся коммитами `chat : refactor API`
  (#30210) и `chat: replace output string with parts` (#30389) — по факту
  вызываемые нами точки сохранились.

**Честно про границу проверки:** это сверка ЗАГОЛОВКОВ, а не сборка. Реальная
совместимость подтверждается только компиляцией нашего моста против b11844 —
это и есть первый шаг чек-листа ниже.

## 4. Чек-лист обновления (если решим идти)

1. `git -C c/llama.cpp checkout b11844` → `git add c/llama.cpp` (сабмодуль).
2. `docker compose -f docker-compose.stack.yml build cppworker-gpu` — три
   CUDA-архитектуры (`75;86;120`), полная сборка ~40–50 мин, после чистки кеша
   дольше.
3. Компиляция моста — ожидаемые места правок: `common_chat_params` (поля
   `prompt`/`format`/`grammar`), при необходимости — доступ к `content_parts`.
4. Регресс-набор (обязательно, до стенда):
   - `R434` — инструменты реально попадают в шаблон (`tool_format=3` на gemma);
   - `R432/R441` — нативные формы вызова разбираются (живые фикстуры);
   - `RPC` — раскладка слоёв на донора (27B: было 20/66 → 48/66);
   - `POST /api/tokenize` — число токенов совпадает с `prompt_eval_count` воркера;
   - `n_ctx`/KV-кэш — то же окно влезает в тот же VRAM (сравнить `context_size`).
5. Только после этого — образ, манифест, `.env`-теги, проверка `/api/version`
   и `llama_build` в `/api/info`.

## 5. Рекомендация

**Идти стоит, но не вместе с релизом функциональных правок.** Обновление даёт
прямую пользу в наших самых проблемных местах (разбор вызовов, шаблоны, KV/RPC) и
бесплатно расширяет список поддерживаемых моделей. Риск — несовместимость моста и
полная пересборка трёх архитектур; поэтому это отдельная ветка/PR с регрессом по
пункту 4, а не «заодно».

Пока зафиксировано b10731: релиз v0.7.235 собран и проверен на нём, и обновление
llama.cpp не требуется ни для одной из правок R448–R473.

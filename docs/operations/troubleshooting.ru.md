# Troubleshooting OllamaLegion

> **Версия:** 3.1 (2026-06-29) — добавлена §10 «Live metrics = 0 для cppworker backend» + ссылка на [`cppworker-metrics-collection.md`](../api/cppworker-metrics-collection.ru.md)  
> **Связанные документы:** [`runbook-tools.md`](runbook-tools.ru.md) — детальный runbook для tools/tool_calls, [`cppworker-model-params.md`](../features/cppworker-model-params.ru.md) — n_ctx/Per-Model Profiles, [`cppworker-metrics-collection.md`](../api/cppworker-metrics-collection.ru.md) — архитектура метрик, [`audit-2026-06.md`](../../.cline/audit-2026-06.md), [`../.clinerules`](../../.clinerules)

## Содержание

1. [Частые ошибки балансировщика](#1-частые-ошибки-балансировщика)
2. [n_ctx overflow: диагностика и решение](#2-n_ctx-overflow-диагностика-и-решение)
3. [ReloadLoopLimitError (HTTP 413)](#3-reloadlooplimiterror-http-413)
4. [Бэкенд помечается как unhealthy](#4-бэкенд-помечается-как-unhealthy)
5. [HTTP 5xx / "Server disconnected" на тяжёлых моделях](#5-http-5xx--server-disconnected-на-тяжёлых-моделях)
6. [Tools/tool_calls — сброс или пустой ответ](#6-toolstool_calls--сброс-или-пустой-ответ)
7. [TransferEncodingError при streaming](#7-transferencodingerror-при-streaming)
8. [Проблемы с NVIDIA Container Toolkit](#8-проблемы-с-nvidia-container-toolkit)
9. [Agent → Ollama: ограничения](#9-agent--ollama-ограничения)
10. [Live metrics = 0 для cppworker backend](#10-live-metrics--0-для-cppworker-backend)
11. [Логирование и отладка](#11-логирование-и-отладка)
12. [AutoTune, стрим и большая загрузка (R230–R231)](#12-autotune-стрим-и-большая-загрузка-r230r231)
13. [Связанные документы](#13-связанные-документы)

---

## 1. Частые ошибки балансировщика

### 1.1 Агент не подключается к балансировщику

**Симптомы:**
- Агент не появляется в списке бэкендов.
- В логах агента — connection refused / timeout.

**Решение:**

```bash
# 1. Проверить доступность балансировщика
curl -v http://<balancer-ip>:18081/api/v1/health

# 2. Проверить firewall
sudo ufw status  # Linux
netsh advfirewall show allprofiles  # Windows

# 3. Логи агента
docker logs deployments-agent-1

# 4. Проверить BALANCER_URL внутри контейнера
docker exec deployments-agent-1 sh -c 'echo $BALANCER_URL'
```

**Возможные причины:**
- Неправильный URL.
- Firewall блокирует 18081.
- Балансировщик не запущен.
- Docker network между контейнерами (для cppworker auto-registration).

### 1.2 Бэкенд помечается как unhealthy

**Симптомы:** `b.status === 'unhealthy'` или `'offline'` в `/api/v1/backends`.

**Решение:**

```bash
# 1. Проверить Ollama напрямую
curl http://<ollama-host>:11434/api/tags

# 2. Проверить agent (если есть)
curl http://<agent-host>:18032/health

# 3. Проверить network
ping <ollama-host>
telnet <ollama-host> 11434
```

**Подробнее:** [`agent-deployment.md`](agent-deployment.ru.md) раздел Troubleshooting.

### 1.3 503 "model is loading"

**Симптом:** сразу после первого запроса с большой моделью приходит 503.

**Причина:** cppworker блокируется на `LoadModel` (30-60 сек), следующий запрос видит `errModelIsLoading`.

**Решение:**
- Дождаться окончания загрузки (cppworker вернёт `loading: true, retryAfterMs: 3000`).
- Или увеличить `Balancing.RequestTimeout` до 300-600 сек (см. [`configuration.md`](../getting-started/configuration.ru.md)).

---

## 2. n_ctx overflow: диагностика и решение

### 2.1 Симптомы

```
"requested n_ctx=128000 exceeds model's effective n_ctx=4096"
```

или

```
HTTP 400 / 413: n_ctx too large for current load
```

### 2.2 Полный поток проблемы

```
Cline → {"model":"gemma-4-E4B-...","options":{"num_ctx":128000}}
  ↓
Балансировщик:
  1. ExtractNumCtxFromBody(body) = 128000
  2. ResolveNumCtx(model, body, backendID):
     - Tier 1 (body): 128000
     - Tier 2 (maxNumCtxForModel):
       ├─ GetModelProfileNumCtx() = 0 → skip
       ├─ GetDefaultModelProfileNumCtx() = ??? ← ⚠️ Если 4096 — clamping!
       └─ getModelLoadedCtxFromMetrics() — не достигается
     - Результат: clamped 128000 → 4096
  3. ApplyCppCtxHeader → X-Cpp-Ctx: 4096
  ↓
CppWorker:
  1. buildGenerationParams: NCtxOverride = 128000 (из body)
  2. applyCppCtxHeader: body 128000 > headerLimit 4096 → CLAMP → 4096
  3. Inference с n_ctx = 4096
  4. Prompt > 4096 → bridge code 2 (ErrNCtxNeedsReload)
  ↓
Балансировщик:
  1. ParseCppWorkerError → NCtxError
  2. handleNCtxReload → DecideReloadBackend:
     - AutoReloadNCtx = false → DecisionNoOp
  3. HTTP 413/400 клиенту
```

### 2.3 Диагностика по шагам

```bash
# 1.1 Проверить defaultModelProfile.contextLength в конфиге
curl -s http://localhost:18081/api/v1/config | jq '.defaultModelProfile.contextLength'
# Должно быть: 0 или null
# Если 4096 — конфиг не обновлён!

# 1.2 Проверить RAM fallback на cppworker
docker exec deployments-cppworker-gpu-1 sh -c 'echo $CPPWORKER_RAM_FALLBACK_N_CTX'
# Должно быть: "true"

# 1.3 Проверить n_ctx auto-reload на балансировщике
curl -s http://localhost:18081/api/v1/config | jq '.balancing.nctxReload'
# Должен быть объект с autoReloadNCtx: true

# 1.4 Прямой тест генерации с большим num_ctx
curl -X POST http://localhost:18081/api/generate \
  -H 'Content-Type: application/json' \
  -H 'X-API-Token: <token>' \
  -d '{"model":"<model.gguf>","prompt":"Hello","options":{"num_ctx":32000}}'
```

### 2.4 Решение

#### Вариант A: Per-Model Profile

```bash
# Создать профиль для конкретной модели
curl -X PUT http://localhost:18081/api/v1/cppworker/model-profiles/gemma-4-E4B-it-Q4_K_M \
  -H "Content-Type: application/json" \
  -d '{"contextLength": 32768, "batchSize": 1024, "numGpuLayers": -1}'

# Применить (reload на всех бэкендах)
curl -X POST http://localhost:18081/api/v1/cppworker/model-profiles/gemma-4-E4B-it-Q4_K_M/apply
```

Подробнее: [`cppworker-model-params.md` §3-4](../features/cppworker-model-params.ru.md).

#### Вариант B: Обновить `config.json`

Убрать `defaultModelProfile.contextLength` (или установить в 0):

```diff
- "defaultModelProfile": {
-   "contextLength": 4096,
-   ...
- }
+ "defaultModelProfile": {
+   "contextLength": 0,
+   ...
+ }
```

#### Вариант C: Включить RAM fallback + auto-reload

```env
# .env.bundled
CPPWORKER_RAM_FALLBACK_N_CTX=true
CPPWORKER_RAM_FALLBACK_MAX_N_CTX=128000
LB_NCTX_RELOAD_ENABLED=true
LB_NCTX_RELOAD_MAX_N_CTX=131072
LB_NCTX_RELOAD_VRAM_SAFETY_FACTOR=0.85
```

### 2.5 «Клиент просит 131072, а модель грузится с 32768» (R397)

**Симптом.** На пустом бэкенде запрос с `options.num_ctx=131072` поднимает
модель с окном 32768, а сам запрос получает `413 n_ctx_infeasible`:
`requested num_ctx=131072 exceeds operator limit auto_reload_max_n_ctx=32768`.

**Цепочка решений (её видно в логах балансера, одна строка каждого звена):**

```
ResolveNumCtx / maxNumCtxForModel: profile is a load hint, clamping to physical ceiling
  profile_hint_n_ctx=32768  physical_ceiling=131072
ensureModelLoadedOnBackend: auto-loading model
  requestedNumCtx=131072  contextPerSeq=...      ← окно, с которым пойдёт загрузка
preflightNCtxReload: num_ctx decision
  body_num_ctx=131072  profile_num_ctx=32768  chosen_source=body
```

Где окно может «съесться» (все четыре места проверяются по очереди):

1. **Потолок авто-роста.** `LB_NCTX_RELOAD_MAX_N_CTX` (env) — защита от того, чтобы
   клиентский запрос сам поднимал окно. Потолок поднимается заявленным окном
   оператора: `contextLength` в профиле модели. R397 добавил второй источник —
   `contextLengthMax` при `contextLengthAuto: true` (режим «auto» до этого был
   мёртв: максимум не участвовал нигде, и запрос на 131072 всегда упирался в env).
2. **Ключ профиля.** R397.1: профиль `Qwen3.8-27B` теперь находится и для модели
   `Qwen3.8-27B-UD-Q4_K_M` (раньше `GetModelProfile` требовал точного ключа, и
   потолок профиля просто не видел — загрузка профиль находила, а потолок нет).
3. **Подстановка окна профиля в загрузку.** R397: профиль заполняет окно, ТОЛЬКО
   если запрос не задал ни одного поля. Раньше `contextLength` из профиля
   подставлялся и при явном `contextPerSeq`, а затем правило R255 выбирало
   суммарное (32768) и выбрасывало per-seq — модель грузилась меньше запрошенного.
4. **`primary: true` в профиле** — «настройки модели зафиксированы оператором,
   клиентский запрос их не меняет». R401 (2026-11-05) отключил этот режим: флаг
   больше не запрещает рост окна и не подменяет клиентский `num_ctx` (он был нашим
   изобретением R83 и противоречил политике балансировки, см. §2.7). Значение в
   старом профиле остаётся, но ни на что не влияет и снимается при сохранении
   профиля из WebUI.

**Что проверить:**

```bash
# 1. Есть ли профиль именно у вашей модели (ключ может быть короче имени файла)
curl -s -H 'X-API-Token: <token>' http://localhost:18081/api/v1/cppworker/model-profiles | jq '.models["Qwen3.8-27B"]'

# 2. Какое окно уйдёт в загрузку и что связывает потолок
curl -s 'http://<host>:18092/api/models/effective?model=<model>&ctx=131072&kvCacheType=q4_0' \
  -H 'Authorization: Bearer <token>' | jq '.effective, .memory'
```

В логе после исправления загрузка выглядит так: `contextPerSeq=131072`,
`physical_ceiling=131072`, а воркер пишет `n_ctx=262144, n_ctx_seq=131072`
(при `parallel=2` суммарное окно = окно клиента × слоты).

### 2.6 «Применить и перезагрузить» не применяет / задевает другой бэкенд (R398)

**Симптомы.** Кнопка «применить и перезагрузить» отвечает успехом, но модель не
перезагрузилась; или в ответе и в диалоге появляются чужие узлы
(`cppworker-donor`, `fake-donor`, `10-126`, `ASTR-19`, `WIN-19`) с
`dial tcp ... connection refused`; настройки окна из карточки модели не
применяются.

**Причины и что сделано.**

1. `GetAllBackends()` собирает срез из map, и порядок между вызовами разный.
   Reload-цикл apply проверял фильтр по `GetAllBackends()[i]`, а грузил
   `GetAllBackends()[i]` из ВТОРОГО вызова — индекс мог указывать на другой
   узел: выбранный бэкенд не перезагружался, а перезагружался чужой. R398: один
   снимок состава на весь apply, итерация по значению.
2. Заведомо офлайн-узлы больше не дёргаются: в диалоге они помечаются `skipped`
   («бэкенд не в сети»), а не красным крестом.
3. Удаление бэкенда теперь доезжает до `config.json`. Раньше `RemoveBackend`
   чистил только runtime и `state.json`, поэтому после рестарта удалённый узел
   возвращался и его снова дёргали apply/загрузки.
4. При `primary: true` «зафиксированное окно» берётся из метрик КОНКРЕТНОЙ
   модели (иначе из её профиля). Раньше оно читалось как
   `LastKnownNCtx(backendID)` — значение на бэкенд, поэтому после перезагрузки
   другой модели primary-модель «фиксировалась» на её 32768, и клиентские
   `num_ctx` молча подменялись вниз. Сам флаг отключён в R401 (§2.7).

**Как проверить, куда ушло применение:**

```bash
curl -s -X POST -H 'X-API-Token: <token>' -H 'Content-Type: application/json' -d '{}' \
  'http://localhost:18081/api/v1/cppworker/model-profiles/<model>/apply?backend=<selected-backend-id>' | jq .backends
# ожидается РОВНО выбранный backendId со status=reloaded; чужих узлов быть не должно
```

`primary: true` — политика оператора: клиентский `num_ctx` не меняет окно
модели. Если нужно, чтобы клиент мог поднимать окно, снимите флаг в карточке
модели.
**Быстрый полный обход настроек:** `scripts/audit-webui-settings.ps1` (R399).
Прогоняет ТЕ ЖЕ HTTP-вызовы, что делает WebUI: все горячие настройки
(сохранить → прочитать → вернуть), все поля профиля модели (на реальном профиле
и на временном со всеми полями), load-defaults, метки/роль бэкенда, конфиг
кластера, кнопку «Загрузить» и «Применить и перезагрузить» (с `-Apply`).
Разрушающие операции (config import, terminate сессий, image apply) пропускаются
осознанно и печатаются как SKIP.

```bash
powershell -NoProfile -ExecutionPolicy Bypass -File scripts/audit-webui-settings.ps1 `
  -Token <CPPWORKER_API_TOKEN> -Model gemma-4-E4B-it-Q4_K_M -Apply
# ИТОГ: PASS=96 FAIL=0 SKIP=11
```

### 2.7 «Поднял окно в карточке, а балансер упирается в 131072» (R400/R401)

**Симптом.** В профиле модели окно (`contextLength`) поднято, например, до
262144, а клиент всё равно получает `413` с текстом «модель поддерживает
131072»; в логах `physical_ceiling=131072` или
`resolveModelMaxContext ... clamped to context_length_max=131072`.

**Причина.** В карточке профиля два разных поля:

| Поле | Что задаёт |
| --- | --- |
| `contextLength` (окно) | окно, с которым модель должна работать |
| `contextLengthMax` (максимальный контекст) | верхняя граница АВТО-роста |

Потолок брался ТОЛЬКО из `contextLengthMax`, поэтому «максимум», оставшийся с
прежних настроек (131072), урезал только что заявленное окно 262144 — и это
выглядело как «зашитый блок 131072». R400: потолок = `max(contextLengthMax,
contextLength)`, заявленное окно больше не может быть урезано максимумом; сам
максимум продолжает поднимать потолок авто-роста. WebUI при сохранении поднимает
максимум до окна и показывает это подсказкой под полем.

**Что проверить:**

```bash
# Потолок, который увидит клиентский запрос (профиль + env + GGUF)
curl -s -H 'X-API-Token: <token>' http://localhost:18079/api/v1/cppworker/model-profiles | jq '.effectiveContext["<model>"]'
# Если limitedBy=env и envCeiling меньше заявленного окна: R400 поднимает
# потолок до окна; проверьте, что contextLength в профиле действительно нужный.
```

**R401: флаг `primary` отключён.** Он появился в R83 (коммит `1999663a`) по
запросу оператора: клиент присылал свой `num_ctx`, и балансер молча
перезагружал модель на большее окно, а нужна была отбивка «настройки
зафиксированы администратором». Флаг запрещал рост окна, подменял клиентский
`num_ctx` на зафиксированный и отключал AutoTune. На стенде с несколькими
бэкендами это «вставляет палки в колёса»: политика теперь другая — холодный
запрос грузит модель под свои параметры, простаивающей копии окно растят, если
это влезает в VRAM/RAM, а копию с большим окном не перезагружают вниз ради
меньшего запроса. Флаг остаётся в JSON для совместимости, но не влияет ни на
одно решение и снимается при сохранении профиля из WebUI.

**Полная схема процессов и ответов:** WebUI → Настройки → Справка → «Схема
работы» (`/scheme.html`, доступна для каждого образа офлайн, с привязкой к коду);
технический документ — `docs/architecture/request-lifecycle.md`, версия с
диаграммами — `docs/architecture/request-lifecycle.ru.md`.

### 2.8 «Клиент не получает маркеров Think, потом обрыв на длинной сессии» (R404)

**Симптомы.** Cline/OpenWebUI при запросе с `tools[]` минутами не получает
ничего: ни дельт ответа, ни `thinking` (хотя в интерфейсе Cline обычно есть
пометка Think); потом ход обрывается. В логе воркера:

```
writeChatStreamResponseWithTools: reasoning отделён ...
  reasoning_len=16647  content_len=12
R188 повтор ограничен ... n_predict=4096
/api/chat stream response summary ... thinking_len=16647 final_content_len=10
  generated_tokens=4096 budget_exhausted=true done_reason=length elapsed_ms=362261
AbortWatcher: per-infer abort set ... context canceled
```

**Причина (R404).** На tools-пути старт блока размышлений включал удержание
всего вывода до ЗАКРЫТИЯ блока. Если бюджет `num_predict` заканчивался внутри
размышлений (типовой исход на 4096 токенах), блок не закрывался никогда:
клиент не получал ни байта, затем получал пустой финальный чанк и **повтор
R188** с тем же бюджетом (ещё столько же тишины). Часть клиентов падает на
разборе такого финала («text part … not found»), потому что thinking приходил
только в done-кадре, без дельт.

**Что сделано.** `thinking` теперь уходит дельтами сразу
(`message.thinking`, `done:false`) — он не может быть вызовом инструмента;
удержание осталось только для `content` (там возможна разметка tool call).
Повтор генерации не запускается, если клиенту уже ушёл хотя бы один кадр.
Если видимого ответа нет, финальный чанк несёт пояснение
(`done_reason=length`), а не пустой content.

**Что проверить:**

```bash
# в стриме должны быть кадры message.thinking ДО финального done
curl -sN -H "Authorization: Bearer <token>" -H 'Content-Type: application/json' \
  -d '{"model":"<model>","stream":true,"messages":[{"role":"user","content":"hi"}],
       "tools":[{"type":"function","function":{"name":"noop","parameters":{"type":"object"}}}]}' \
  http://localhost:18080/api/chat | head -20
# в логе воркера при исчерпании бюджета в размышлениях:
#   retry skipped — stream already started (R404)
```

Если клиент всё равно пишет «text part … not found» — это уже разбор его
собственного парсера: приложите версию клиента и точный кадр, на котором он
падает (в стриме выше видно, что thinking идёт отдельными кадрами).

---

## 3. ReloadLoopLimitError (HTTP 413)

### 3.1 Симптомы

```
"reload attempts exceeded maximum of 3 per 60 sec window"
```

или в логах cppworker:
```
RAM fallback: cycle limit reached, refusing reload
```

### 3.2 Причина

cppworker попытался сделать reload 3 раза за 60 сек, но каждый раз получал ту же ошибку (n_ctx overflow). Защита от бесконечного reload loop.

### 3.3 Сброс счётчика (R-6)

**Сейчас:** единственный способ — `docker restart deployments-cppworker-gpu-1`.

**После R-6** (в работе):

```bash
# Балансировщик (counter на стороне балансировщика):
curl -X POST http://localhost:18081/api/v1/nctx-reload/cppworker-gpu-1/reset

# CppWorker (counter на стороне cppworker):
curl -X POST http://localhost:18081/api/v1/cppworker/reset-reload-counter
```

### 3.4 Перед сбросом

1. Устранить причину (например, уменьшить `num_ctx` в запросе, увеличить VRAM, выгрузить другие модели).
2. Проверить `max_vram_n_ctx` в логах.
3. Перезапустить только после устранения.

### 3.5 422 `n_ctx_infeasible`, когда RAM меньше весов модели (mmap)

**Симптом.** Модель 27B (16.4 GB) на машине с RAM 20 GB, `n_ctx=131072`, `kvCacheType=q4_0`,
в WebUI включён `useMmap` — загрузка отклоняется:

```
422 n_ctx_infeasible: requested n_ctx=131072 exceeds the physically feasible max 82824
(max_vram_n_ctx=0, max_ram_n_ctx=82824, gguf_max_context=262144)
```

**Правило (R396, 2026-11-04).** Проверка памяти считает РЕЗИДЕНТНЫМИ в RAM:

- KV-кэш и буферы — всегда (их нельзя вытеснить);
- веса модели — только без mmap.

С `use_mmap` файл модели — это page cache: страницы грязными не становятся и
вытесняются под давлением, поэтому требовать их постоянного присутствия в RAM
нельзя. Обязательными остаются KV и буферы, а также системный резерв (`RAM − 4 GiB`).

**Цена честная.** Если RAM не держит рабочий набор страниц весов, часть весов будет
перечитываться с диска на каждом токене — ответ станет медленнее. Это осознанный
выбор оператора: он делает его, включая mmap.

**Как проверить, что режим учтён** (ручка справочная, ничего не грузит):

```bash
curl -s 'http://<host>:18092/api/models/effective?model=Qwen3.8-27B-UD-Q4_K_M&ctx=131072&kvCacheType=q4_0' \
  -H 'Authorization: Bearer <token>' | jq '.effective, .memory | .stage?, .feasible?, .max_ram_n_ctx?, .memory_breakdown.mmap_weights?'
```

В ответе `effective.use_mmap` — применённый режим (`request` / `profile` / `default`
/ `loaded`), а в `memory.memory_breakdown` есть `mmap_weights: true` и строке
`memory breakdown` — пояснение `weights via mmap (file-backed, RAM counts KV only)`.

Если `useMmap` в профиле/настройках модели действительно включён, а отказ остался —
значит, в RAM не помещаются уже KV и буферы: уменьшайте `n_ctx`, включите
`kvCacheType=q4_0` (экономит KV примерно в 2.6 раза против f16) или добавьте RAM.

---

## 4. Бэкенд помечается как unhealthy

### 4.1 Диагностика

```bash
# Статус всех бэкендов
curl -H 'X-API-Token: <token>' http://localhost:18081/api/v1/backends | jq

# Конкретный бэкенд (по ID)
curl -H 'X-API-Token: <token>' http://localhost:18081/api/v1/backends/cppworker-gpu-1
```

### 4.2 Причины и решения

| Причина | Симптом | Решение |
|---|---|---|
| Ollama не запущен | curl 11434 → connection refused | `systemctl restart ollama` |
| Firewall | timeout | открыть порт |
| Неправильный host/port | 503 на health | исправить в WebUI → Backends → Edit |
| Backend перегружен | status=healthy, но ответы 503 | уменьшить `MaxConcurrentRequests` |
| Агент не регистрируется | `b.hasAgent === false` | проверить `BALANCER_URL` |

---

## 5. HTTP 5xx / "Server disconnected" на тяжёлых моделях

### 5.1 Причина

`Balancing.RequestTimeout` (default 30 сек) бьёт раньше, чем `clampNPredictToFitContext` отрабатывает длинную генерацию.

### 5.2 Решение

```json
// config/config.json
{
  "balancing": {
    "requestTimeout": 600   // 10 минут
  }
}
```

Или через ENV: `LB_REQUEST_TIMEOUT=600`.

Также можно увеличить `CPPWORKER_WRITE_TIMEOUT`:

```env
CPPWORKER_WRITE_TIMEOUT=1800  # 30 минут
```

---

## 6. Tools/tool_calls — сброс или пустой ответ

**Это самая частая проблема при работе с OpenWebUI / Cline / Roo Code.** Подробный пошаговый runbook в [`runbook-tools.md`](runbook-tools.ru.md).

### 6.1 Краткая сводка по сценариям

| Симптом | Сценарий | Где искать |
|---|---|---|
| HTTP 413 на первом запросе с tools | **A** — RAM fallback disabled для tools | `cmd/cppworker/inference.go:491` |
| Первый запрос OK, второй пустой NDJSON | **B** — clamping n_predict с has_tools=true | `internal/balancer/nctx_clamp.go` |
| Бесконечный reload loop, 503 | **C** — `ReloadLoopLimitError` | `cmd/cppworker/inference.go` |
| Tool_call в content, но `tool_calls=null` | **D** — Hermes/Mistral detection | `internal/balancer/llamacpp_toolcall_detector.go` |
| HTTP 5xx / "reset by peer" | **E** — таймауты WriteTimeout/RequestTimeout | см. §5 |

### 6.2 Acceptance criteria (перед дебагом)

1. `CPPWORKER_VERBOSE=true` на cppworker.
2. `data/state.json` (балансер) — список бэкендов и их статусы.
3. Логи балансировщика: `parsed request`, `[BALANCER → BACKEND]`, `heartbeat write failed`.
4. Логи cppworker: `clamping n_predict`, `RAM fallback`, `bridge code N`.
5. Базовые тесты транспорта:
   ```bash
   go test ./tests -run "TestDebugOpenWebUI_ToolCalls_Scenario" -tags llama_stub -count=1 -v
   ```
6. Inference-тесты:
   ```bash
   go test ./cmd/cppworker -run "TestApplyCppCtxHeader|TestClampNPredict|TestReloadDisabledForTools|TestParseToolCalls" -tags llama_stub -count=1 -v
   ```

---

## 7. TransferEncodingError при streaming

### 7.1 Симптомы

```
Error: write tcp: ..."write: broken pipe"
или
TransferEncodingError: chunked transfer encoding failed
```

### 7.2 Причина (уже исправлена в 2026-06-08)

Раньше: cppworker возвращал JSON-ошибку для streaming-запроса, балансировщик пытался её проксировать как NDJSON, но Go добавлял `Transfer-Encoding: chunked`, что ломало клиент.

### 7.3 Текущее поведение

`internal/balancer/llamacpp_transport.go:proxyRequestLlamaCpp` проверяет `Content-Type` ответа upstream:
- Если `application/json` (ошибка) → проксировать как JSON, **не** входить в SSE-ридер.
- Если `text/event-stream` → нормальный streaming.

`resp.Header.Del("Transfer-Encoding")` удалён — Go управляет этим автоматически.

### 7.4 Если ошибка всё есть

Проверить `Content-Type` от cppworker:
```bash
curl -v -X POST http://localhost:18092/api/chat -H "Content-Type: application/json" -d '{"model":"x","stream":true}' 2>&1 | grep -E "^< Content-Type"
```

Должно быть `text/event-stream` или `application/x-ndjson`, **не** `text/plain` или отсутствовать.

---

## 8. Проблемы с NVIDIA Container Toolkit

### 8.1 GPU не виден из контейнера

```bash
# Проверка на хосте
nvidia-smi

# Проверка внутри контейнера
docker run --rm --gpus all nvidia/cuda:12.2.0-base-ubuntu22.04 nvidia-smi
```

Если вторая команда fails → не установлен NVIDIA Container Toolkit.

**Установка:** см. https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/latest/install-guide.html

### 8.2 Windows: GPU не пробрасывается

1. Docker Desktop → Settings → General → ✅ Use WSL 2.
2. Resources → WSL Integration → ✅ Enable integration.
3. Установить NVIDIA Driver для WSL2: https://developer.nvidia.com/cuda/wsl.

Подробнее: [`agent-deployment.md` §Windows](agent-deployment.ru.md).

---

## 9. Agent → Ollama: ограничения

> **Это архитектурное ограничение Ollama, не баг OllamaLegion.**

Ollama **не имеет публичного REST API** для изменения runtime config. Агент может:
- ✅ Читать метрики (`/api/ps`, `/api/tags`, `/api/show`).
- ✅ Отправлять метрики балансировщику.
- ✅ Применять лимиты через heartbeat.
- ❌ Менять `numGPU`, `num_parallel`, `num_threads` Ollama напрямую.

Единственные способы изменить Ollama config:
- Переменные окружения при старте Ollama (`OLLAMA_NUM_PARALLEL`, `OLLAMA_MAX_LOADED_MODELS`).
- Файл `~/.ollama/config.json`.
- Аргументы CLI при запуске `ollama serve`.

**Что НЕ реализовано (и не планируется):** механизм Agent → Ollama config через heartbeat.

---

## 10. Live metrics = 0 для cppworker backend

> **Это ожидаемое поведение poller'а, не баг.** Подробная архитектура: [`cppworker-metrics-collection.md`](../api/cppworker-metrics-collection.ru.md).

**Симптом:** на вкладках Dashboard / Backends / Models / GGUF Models у `cppworker-gpu` бэкенда видно `GPU Usage: 0%`, `VRAM Used: 0 MB`, `CPU: 0%`, `RAM: 0 MB`. Список загруженных моделей (LoadedModels) **может** при этом заполняться.

**Корневая причина:** poller в балансировщике (`internal/balancer/llamacpp_metrics_poller.go`) собирает **только** `GET /api/models` и `GET /api/models/load/progress`. Он не имеет NVML-доступа к GPU cppworker'а, не снимает CPU/RAM с хоста cppworker'а и не публикует `requests_per_second`. Для live-метрик нужен **agent sidecar**.

**Быстрая диагностика:**

```bash
# 1. Проверить, есть ли agent у бэкенда
curl -H "X-API-Token: $LB_TOKEN" http://localhost:18081/api/v1/backends | \
  jq '.backends[] | select(.id | contains("cppworker")) | {id, type, hasAgent}'
# Если hasAgent=false — agent не запущен, см. ниже.

# 2. Если hasAgent=true, но метрики 0:
docker exec <agent-container> nvidia-smi
# Если "command not found" — образ собран без nvidia-container-toolkit.
```

**Решение:** развернуть agent рядом с cppworker:

| Сценарий | Compose |
|---|---|
| Sidecar к существующему балансеру | `deployments/docker-compose.cppworker-with-agent.yml` |
| Standalone (всё-в-одном) | `deployments/docker-compose.cppworker-with-agent.standalone.yml` |

Подробнее про архитектуру poller vs agent, legacy `BackendType=""` и все сценарии troubleshooting — в [`cppworker-metrics-collection.md`](../api/cppworker-metrics-collection.ru.md).

**NB:** poller **не** трогать (`llamacpp_metrics_poller.go` — read-only). Это намеренное разделение ответственности: poller для моделей, agent для железа.

---

## 11. Логирование и отладка

### 11.1 Уровни логов

| ENV | Уровень |
|---|---|
| `LOG_LEVEL=debug` | максимальный |
| `LOG_LEVEL=info` (default) | базовый |
| `LOG_LEVEL=warn` | только warnings |
| `LOG_LEVEL=error` | только ошибки |

### 11.2 Формат

| ENV | Формат |
|---|---|
| `LOG_FORMAT=json` (default) | структурированный JSON |
| `LOG_FORMAT=text` | человекочитаемый |

### 11.3 Просмотр логов

```bash
# Балансировщик
docker logs -f deployments-loadbalancer-1

# CppWorker с verbose
docker logs -f deployments-cppworker-gpu-1 2>&1 | grep -E "clamping|RAM fallback|bridge code|tool_calls"

# PowerShell
docker logs -f deployments-loadbalancer-1 2>&1 | Select-String "tools|chat_id|stream|/v1/chat"
```

### 11.4 NVML отладка (Linux)

```bash
# Проверить nvidia-smi в агенте
docker exec deployments-agent-gpu-1 nvidia-smi

# Включить NVML verbose
docker exec deployments-agent-gpu-1 sh -c 'NVML_DEBUG=1 ./agent'
```

### 11.5 Полезные ENV для дебага

```bash
# Балансировщик
LOG_LEVEL=debug LOG_FORMAT=text
BALANCING_DEBUG=true  # не существует, для примера

# CppWorker
CPPWORKER_VERBOSE=true
```

---

## 12. AutoTune, стрим и большая загрузка (R230–R231)

Три живых симптома, которые выглядят «как баг модели», но имеют конкретную причину
в стеке. Все три закрыты и проверяются скриптами.

### 12.1 AutoTune пишет «свободно 0 GB» и советует `num_gpu_layers=-2` / `q4_0`

**Симптом (A10 24 ГБ, ctx 131072, KV q8_0, 16 слоёв):** в рекомендациях
`free=0.0GB`, `-2` в слоях и `q4_0`, после перезагрузки стало хуже, чем при первой
загрузке.

**Почему было:** метрики GPU/System приходят в **МБ**, а анализ ждал **байты**
(20000 МБ превращались в 0.0 ГБ); плюс бюджет считался по остатку при загруженной
модели, а не по reload-бюджету (`free + occupancy` этой модели); параметры
`n_kv_heads`/`head_dim` в `/api/models` отсутствуют, без них KV не посчитать.

**Что делать:**
1. Обновите образы до R230+ (`balancer:r230+`, `cppworker:gpu-r231+`).
2. Проверьте отчёт: `GET /api/v1/backends` → `autoTune.models[].recommendations`.
   Признаки корректности: есть «бюджет N GB» с числами, нет `-2`, нет `0.0GB`.
3. Живая проверка: `pwsh -File scripts/verify-autotune-r230.ps1`
   (PASS — бюджет считается, рекомендации объяснены, `-2` отсутствует).
4. Если рекомендаций нет вовсе при загруженной модели — проверьте, что
   `autoTuneEnabled=true` и что `/api/models` отдаёт `n_layers` (иначе анализ
   консервативно не трогает раскладку — это штатно).

**Важно:** в стриме автотюн применяется ПОСЛЕ ответа (модель простаивает), а
фактическая конфигурация сверяется с планом. При непрерывном трафике reload может
отклоняться как «busy» — это честный отказ с cool-down, а не тихий успех.

### 12.2 Cline: `</think>` в теле ответа, дубль текста, сессия обрывается

**Симптом:** в ответе видно `</think>` (или `<end_of_turn>`), текст размышлений
повторяется в основном ответе, стрим заканчивается без `[DONE]` — Cline ждёт
конца и рвёт сессию.

**Причины (R231):**
- R91-фолбэк «незакрытый reasoning → content» применялся к стриму: размышления
  уходили вторым каналом, а затем копией в `content`;
- лишние закрывающие маркеры оставались в видимом тексте;
- SSE-фильтр балансера получал голый JSON вместо `data: {...}` и фактически не
  чистил ничего; bool «модифицирован» трактовался как «выбросить чанк»;
- завершающий `[DONE]` эмитился только при включённом авто-продолжении.

**Что делать:**
1. Обновите балансер и воркер до R231+; перезапускать клиент не нужно.
2. Проверка: `node scripts/verify-stream-r231.js` — 3 прогона, критерии:
   content не дублирует reasoning, маркеров нет, `[DONE]` ровно один и последний,
   пустой content только с `finish_reason=length`.
3. Контракт клиентов (OpenAI+Ollama, включая tools): `node scripts/clients/contract-suite.js`.
4. Если после обновления симптом остался — в логе воркера ищите строку
   `блок размышлений не закрыт` (там видно, почему `length`) и проверьте, что
   клиент не включает свой «стрим-парсер» с ожиданием другого формата.

### 12.3 Загрузка 15+ ГБ модели обрывается («model load cancelled»)

**Симптом:** `runAsyncLoad: background load failed ... stopped at 0% of weights`,
в логе до этого — `загрузка модели не завершилась за отведённое время` и стек
горутин.

**Причина:** watchdog загрузки (`CPPWORKER_LOAD_STALL_SEC`, дефолт 300 с) считает
загрузку зависшей, если progress-callback долго не двигается. На медленном диске
15-ГБ GGUF читается 280–480 с, и дефолт её убивает — хотя загрузка шла нормально.

**Решение:** в `deployments/.env` выставить
`CPPWORKER_LOAD_STALL_SEC=1200` и пересоздать воркер
(`docker compose ... up -d --force-recreate cppworker-gpu`). Проверка:
`/api/models` → `state=loaded`, в логе `model loaded successfully`.

---

## 13. Связанные документы

- [`runbook-tools.md`](runbook-tools.ru.md) — **детальный пошаговый runbook для tools/tool_calls** (сценарии A-E).
- [`cppworker-model-params.md`](../features/cppworker-model-params.ru.md) — n_ctx + Per-Model Profiles.
- [`cppworker-metrics-collection.md`](../api/cppworker-metrics-collection.ru.md) — **архитектура сбора метрик cppworker: poller vs agent**.
- [`rpc-benchmark-qwen38.md`](../features/rpc-benchmark-qwen38.ru.md) — замер выигрыша RPC (методика и цифры).
- [`agent-deployment.md`](agent-deployment.ru.md) — развёртывание агента.
- [`installation.md`](../getting-started/installation.ru.md) — установка.
- [`audit-2026-06.md`](../../.cline/audit-2026-06.md) — статус реализации.
- [`../.clinerules`](../../.clinerules) §14 — частые ловушки.

---

## Проблемы с подключением

### Не удаётся подключиться к балансировщику

**Симптомы:** `curl http://localhost:18081/api/v1/health` не проходит

**Решение:**
1. Убедиться, что балансировщик запущен:
   ```bash
   docker ps | grep loadbalancer
   # или
   ps aux | grep ollama-balancer
   ```
2. Проверить логи:
   ```bash
   docker logs ollamalegion_loadbalancer_1
   ```
3. Убедиться, что порт не занят:
   ```bash
   netstat -tuln | grep -E '18080|18081'
   ```
4. Проверить правила firewall:
   ```bash
   sudo ufw status  # Ubuntu
   sudo firewall-cmd --list-all  # CentOS/RHEL
   ```

### CORS-ошибки в WebUI

**Симптомы:** в консоли браузера видны CORS-ошибки

**Решение:**
1. Заходить через nginx-прокси (не напрямую как `file://`)
2. Использовать Docker Compose, в котором nginx reverse proxy уже включён
3. При прямом доступе добавить API base:
   ```
   http://localhost:18080/?api_base=http://localhost:18081
   ```

---

## Проблемы с бэкендами

### Бэкенд показывается как unhealthy

**Симптомы:** в WebUI статус бэкенда — "unhealthy"

**Решение:**
1. Убедиться, что Ollama запущен на бэкенде:
   ```bash
   curl http://backend-ip:11434/api/tags
   ```
2. Проверить, запущен ли агент:
   ```bash
   curl http://backend-ip:18032/metrics
   ```
3. Проверить сетевую доступность:
   ```bash
   ping backend-ip
   telnet backend-ip 11434
   ```
4. Проверить логи бэкенда:
   ```bash
   docker logs ollama-agent-1
   ```

### Модели не появляются в списке

**Симптомы:** на вкладке Models нет моделей или список неполный

**Решение:**
1. Убедиться, что модели загружены на бэкендах:
   ```bash
   curl http://backend-ip:11434/api/tags
   ```
2. Перерегистрировать бэкенд:
   ```bash
   # Удалить бэкенд и добавить заново
   curl -X DELETE http://localhost:18081/api/v1/backends/backend-id
   curl -X POST http://localhost:18081/api/v1/backends -d '{"id":"...","host":"..."}'
   ```

---

## Проблемы с производительностью

### Большая глубина очереди

**Симптомы:** много запросов ждут в очереди

**Решение:**
1. Проверить, не перегружены ли бэкенды:
   ```bash
   curl http://localhost:18081/api/v1/cluster | jq '.backends[].activeRequests'
   ```
2. Форсировать ребаланс:
   ```bash
   curl -X POST http://localhost:18081/api/v1/queue/rebalance
   ```
3. Добавить бэкенды
4. Увеличить `maxConcurrentRequests` на бэкендах:
   ```bash
   curl -X PUT http://localhost:18081/api/v1/backends/backend-id \
     -d '{"maxConcurrentRequests": 16}'
   ```

### Медленные ответы

**Симптомы:** запросы идут необычно долго

**Решение:**
1. Проверить метрики GPU:
   ```bash
   curl http://localhost:18081/api/v1/cluster | jq '.backends[].gpu'
   ```
2. Убедиться, что модель загружена (а не свопится):
   ```bash
   curl http://backend-ip:11434/api/ps
   ```
3. Проверить disk I/O на бэкендах
4. Уменьшить `maxConcurrentRequests`, если GPU перегружены

---

## Проблемы с Docker

### Контейнер не стартует

**Симптомы:** `docker compose up` завершается ошибкой

**Решение:**
1. Проверить конфликты портов:
   ```bash
   netstat -tuln | grep -E '18080|18081|6379|5432'
   ```
2. Проверить версию Docker Compose (2.0+):
   ```bash
   docker compose version
   ```
3. Забрать свежие образы:
   ```bash
   docker compose pull
   ```
4. Пересобрать образы:
   ```bash
   docker compose build --no-cache
   ```

### GPU недоступен в контейнере

**Симптомы:** агент не показывает метрики GPU

**Решение:**
1. Проверить NVIDIA Container Toolkit:
   ```bash
   nvidia-ctk --version
   ```
2. Проверить доступ к GPU:
   ```bash
   docker run --rm --gpus all nvidia/cuda:12.0-base nvidia-smi
   ```
3. Проверить Docker GPU runtime:
   ```bash
   docker info | grep -i runtime
   ```
4. Добавить `--gpus all` или использовать `docker-compose.agent.gpu.yml`

### Сборка падает с `short read: expected N bytes ... unexpected EOF`

**Симптомы:** любой `docker build` образа, который начинается с CUDA-базы
(`cppworker:gpu-*`, `imageworker:cuda-*`), падает на этапе разрешения слоёв:

```
#14 ERROR: short read: expected 2343401679 bytes but got 580885939: unexpected EOF
ERROR: failed to build: failed to solve: failed to compute cache key: short read ...
```

Одинаковая ошибка на нескольких стадиях сразу — это не код и не место на диске:
так выглядит **неполный (обрезанный) слой базового образа** в локальном хранилище.
`docker pull` при этом пишет `Already exists` и падает тем же `short read` —
локальный blob короче, чем заявлено в манифесте.

**Проверка, что дело в базовом слое, а не в проекте:**

```powershell
docker pull dockerhub.timeweb.cloud/nvidia/cuda:12.2.0-devel-ubuntu22.04
# short read ... → слой действительно битый локально
```

**Решение — добрать слой с другого зеркала (дайджест тот же):**

```powershell
docker pull mirror.gcr.io/nvidia/cuda:12.2.0-devel-ubuntu22.04
# Pull complete, Digest: sha256:c4e81887e4aa...  (тот же, что просит сборка)
docker build -t ollama-legion/cppworker:gpu-<sha> -f docker/cppworker/Dockerfile.gpu .
```

После этого сборка идёт с локального слоя и в сеть за базой не ходит. Если
`mirror.gcr.io` недоступен, помогает любой другой реестр с тем же дайджестом
(`--build-arg CUDA_BASE=<реестр>/nvidia/cuda`).

### Мало места на C: — перенос диска Docker на другой диск (Windows)

**Симптомы:** `docker system df` показывает десятки ГБ, на системном диске
осталось несколько ГБ, сборки начали падать с `no space left on device`.

**Что важно знать (проверено на Docker Desktop 4.4x / engine 29.6.2):**

- Данные движка лежат в одном файле
  `%LOCALAPPDATA%\Docker\wsl\disk\docker_data.vhdx` (у нас было 44.6 ГБ).
- Настройка «Disk image location» в `settings-store.json` (`"DataFolder"`)
  **этой версией игнорируется**: ключ сохраняется, но диск монтируется по
  старому пути. Проверять надо фактом: файл на новом месте должен быть
  заблокирован запущенным Docker, а старая копия — свободна.
- Рабочий способ — **junction каталога данных** на другой диск.

**Порядок (данные сохраняются, стек возвращается сам — `restart: unless-stopped`):**

```powershell
docker desktop stop                 # оба WSL-дистрибутива должны стать Stopped
robocopy "$env:LOCALAPPDATA\Docker\wsl" "D:\Docker" /E /COPY:DAT /R:2 /W:2 /NFL /NDL /NP
Rename-Item "$env:LOCALAPPDATA\Docker\wsl\disk" "disk.old"
cmd /c mklink /J "$env:LOCALAPPDATA\Docker\wsl\disk" "D:\Docker\disk"
docker desktop start
```

Проверка, что переехало (а не осталось на C:):

```powershell
# занят должен быть файл на D:, а старая копия — свободна
docker info --format '{{.DockerRootDir}}'
docker images --format '{{.Repository}}:{{.Tag}}' | Measure-Object   # образы на месте
```

Только после этого удалять `disk.old` (иначе места на C: не прибавится).
`docker desktop start` поднимает ранее запущенные контейнеры по их политике
перезапуска; профильные сервисы (`agent`, `rpc-node`), которых в обычном
наборе нет, надо погасить: `docker stop ol-stack-agent ol-stack-rpc-node`.

**Подводные камни.** Junction переживает перезапуск Docker Desktop, но не
переустановку; после апгрейда Docker Desktop проверьте, что диск всё ещё на D:
(в свойствах каталога `LinkType: Junction`, и файл на D: заблокирован движком).
Переносить надо именно каталог `wsl\disk`, а не только `.vhdx`: Docker ищет
файл по относительному пути `disk\docker_data.vhdx`.

---

## Проблемы с WebUI

### Пустая страница или неполная отрисовка

**Симптомы:** страница WebUI пустая или загрузилась частично

**Решение:**
1. Очистить кэш браузера (Ctrl+Shift+R)
2. Посмотреть ошибки в консоли JavaScript (F12)
3. Проверить доступность API:
   ```bash
   curl http://localhost:18081/api/v1/health
   ```
4. Форсировать обновление: добавить `?v=2` к URL

### Страница Monitor показывает "No data"

**Симптомы:** на вкладке Monitor нет информации

**Решение:**
1. Дождаться первого получения данных (2-5 секунд)
2. Нажать кнопку "Retry"
3. Включить demo mode, чтобы проверить работу UI:
   - Открыть консоль браузера: `enterDemoMode()`
4. Проверить настройки CORS
5. Проверить API base URL:
   - Открыть консоль: `API_BASE`
   - Задать вручную: `localStorage.setItem('monitorApiBase', 'http://localhost:18081')`

### Тема применяется неправильно

**Симптомы:** тёмная/светлая тема не переключается

**Решение:**
1. Очистить `localStorage`:
   ```javascript
   localStorage.removeItem('ollamalegion_theme')
   ```
2. Жёстко обновить страницу (Ctrl+Shift+R)
3. Проверить, что `localStorage` включён в браузере

### Не отображаются иконки после переноса WebUI на офлайн-хост

**Симптомы:** часть или все иконки Font Awesome пустые; текст и вёрстка при этом
целы.

**Причины и проверки:**
1. **Все иконки пустые** - не скопирован каталог `webfonts/`. Шрифты лежат в
   `webui/webfonts/fa-*.woff2`, их запрашивает `css/font-awesome.min.css`.
   Копируйте рядом со страницами `webui/css`, `webui/js`, `webui/img` и
   `webui/webfonts` (Docker-образы копируют их масками каталогов). Проверка:
   ```bash
   curl -I http://<host>/webfonts/fa-solid-900.woff2   # должно быть 200
   ```
2. **Пустая одна конкретная иконка** (например HuggingFace) - в Font Awesome 6
   Free нет её брендового глифа. С R238c она рисуется локальной SVG-маской
   `webui/img/huggingface.svg`; убедитесь, что каталог `webui/img/` тоже
   скопирован.
3. Прогнать статическую проверку:
   ```bash
   python scripts/check_webui_assets.py
   ```
   Она падает на внешних CDN, отсутствующих локальных ассетах и неизвестных
   классах `fa-*` (неизвестный класс отрисуется пустой иконкой).

---

## Как получить помощь

1. **Сначала посмотреть логи:**
   ```bash
   docker compose logs loadbalancer
   docker compose logs agent
   ```

2. **Проверить health API:**
   ```bash
   curl -v http://localhost:18081/api/v1/health
   ```

3. **Включить debug-логирование:**
   ```bash
   LOG_LEVEL=debug docker compose up
   ```

4. **Сообщить о проблеме:** [GitHub Issues](https://github.com/BarsSky/ollamalegion/issues)

## Docker Desktop не стартует: кончилось место на диске данных (2026-10-31)

Живой случай на этом стенде, случился дважды за день (второй раз — во время сборки
cppworker, `failed to receive status: rpc error ... EOF`, затем
`Docker Desktop is unable to start`).

**Симптомы.** `docker ps` отвечает `request returned 500 Internal Server Error` или
`Error response from daemon: Docker Desktop is unable to start`; контейнеры стоят;
`wsl -l -v` показывает дистрибутивы `Stopped`; в
`%LOCALAPPDATA%\Docker\log\host\com.docker.backend.exe.log` идёт бесконечное
`still waiting for the engine to respond to _ping ... HTTP 503`. Перезапуск Docker
Desktop помогает НЕ всегда, `wsl --update` может упасть с `HTTP_E_STATUS_FORBIDDEN`
(нет сети/прокси).

**Причина (проверять ПЕРВОЙ).** Диск данных Docker переполнен: в этом стенде он вынесен
на `D:` через junction `%LOCALAPPDATA%\Docker\wsl\disk` → `D:\Docker\disk`, а сам диск
забит образом данных `docker_data.vhdx` (у нас 107 ГБ при 400 ГБ занятых на `D:`).
При нуле свободного места WSL-дистрибутив не может ни вырасти, ни запуститься, и движок
не поднимается — при этом сообщение «Docker Desktop is unable to start» не называет
причину.

```
Get-PSDrive C,D | Select-Object Name,@{n='freeGB';e={[math]::Round($_.Free/1GB,1)}}
Get-ChildItem D:\Docker\disk | Select-Object Name,@{n='GB';e={[math]::Round($_.Length/1GB,1)}}
```

**Что делать.**

1. Освободить место на том диске, где лежит `.vhdx` (переместить крупные файлы на другой
   том — обратимо), затем запустить Docker Desktop заново. Перезапуск БЕЗ свободного места
   не поможет: движку некуда писать.
2. Когда движок поднимется — освободить место ВНУТРИ него:
   `docker system df`, `docker builder prune -f`, `docker image prune -a -f`
   (у нас каждая сборка cppworker под CUDA добавляет несколько ГБ).
3. Сборку cppworker после освобождения места запускать с `--build-arg CUDA_ARCH=86`
   (для RTX 3070): полный набор арок собирается часами и съедает десятки ГБ.

**Побочный эффект, который стоит знать.** Если сборка образа не удалась, а
`deployments/.env` уже переключён на новый тег, `docker compose up` не найдёт образ и
воркер останется лежать. Правило: сначала успешный `docker build`, только потом правка
тега в `.env`. В этом инциденте тег вернули на последний собранный образ (`bfd4b753`).
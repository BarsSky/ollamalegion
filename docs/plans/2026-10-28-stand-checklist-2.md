# Чек-лист второго стенда (серия R251–R265)

Назначение: закрыть ровно те пункты, которые не проверяются на стенде с 8 ГБ VRAM
(`202 StillLoading`, вытеснение модели, окно 256000) и проверить агентский сценарий на
**рабочей** модели вместо `gemma-4-E4B`.

Общие обозначения:

- `API=http://127.0.0.1:18081`, `DATA=http://127.0.0.1:18080` (data-plane `/v1/*`);
- токен: `CPPWORKER_API_TOKEN` из `deployments/.env`;
- логи: `docker logs --since 30m ol-stack-balancer | Select-String ...`;
- в PowerShell для JSON удобнее `Invoke-RestMethod`, для потоков — `curl.exe`.

Для каждого пункта: **что сделать → что ожидать → что искать → PASS/FAIL**.

---

## 1. Реальный агентский сценарий: create → fill → read → patch

**Сделать.** Cline или DSH на рабочей модели (не `gemma-4-E4B`), в отдельном каталоге вне
репозитория, задача: создать файл → наполнить на 150–250 строк → прочитать → исправить →
точечно изменить (`apply_patch`/`edit`). Таймаут задачи — не меньше 20 минут.

**Ожидать.** Все шаги выполняются, итоговый файл корректен (правка применена, текст не обрезан),
в клиенте нет «Invalid input», задача доходит до конца без «session closed».

**Искать.**
- `API/api/v1/sessions`: одна строка на задачу, `id` и `createdAt` не меняются от шага к шагу;
- `API/api/v1/cluster` → `recentClients`: клиент присутствует постоянно (не пропадает);
- логи: нет `tool args repair skipped` в больших количествах (одна строка на стрим — норма);
- логи: нет `Invalid input`-подобных ошибок балансера, нет `panic`.

**FAIL, если:** файл обрезан, аргументы инструмента битые, сессия меняет `id`, клиент исчезает
из `recentClients` дольше чем на ~60 с.

---

## 2. Сжатие истории Cline не создаёт новую сессию

**Сделать.** В той же задаче дождаться компакции контекста (Cline показывает «context
condensed» / урезание истории). Либо искусственно: новая задача в **том же** workspace с тем же
system-промптом.

**Ожидать.** Сессия продолжается: `id` тот же, `createdAt` тот же, `requestCount` растёт.

**Искать.** `API/api/v1/sessions` до и после компакции (сравнить `id`, `createdAt`).
В логах — отсутствие второй строки с тем же клиентом, но новым `createdAt`.

**FAIL, если:** после компакции появилась вторая сессия (значит сработало правило «новая сессия»
вместо «продолжение»), либо `requestCount` сбросился в 1.

---

## 3. `202 StillLoading` при загрузке большой модели

**Сделать.** Загрузить модель, которая грузится дольше окна поллинга (крупная, CPU-offload).
Запрос:

```powershell
Invoke-RestMethod -Method Post -Headers @{ 'X-API-Token' = $tok } -ContentType 'application/json' `
  -Uri "$API/api/v1/cluster/models/<MODEL>/reload" -Body '{"operation":"load","backendId":"<ID>"}'
```

**Ожидать.** Либо `200` с «polling confirmed completion» (если успела), либо **`202`** со
статусом «ещё грузится» и `pollAfterSec` — но **не `500`**.

**Искать.** Код ответа; в логах `model loaded successfully` или `still loading`; затем
`API/api/v1/backends/<ID>/models` → модель `state=loaded`.

**FAIL, если:** приходит `500` / «model not ready» / «poll timeout» как ошибка, хотя воркер модель
догружает.

---

## 4. Вытеснение модели: одна `loaded=true`

**Сделать.** Загрузить модель, которая физически вытеснит предыдущую (нужна VRAM, где две не
помещаются). Проверить:

```powershell
(Invoke-RestMethod -Headers @{ 'X-API-Token' = $tok } "$API/api/v1/backends/<ID>/models") |
  Select-Object loadedModelCount, total, @{n='models';e={$_.models | % { "$($_.name): loaded=$($_.loaded) state=$($_.state)" }}}
```

**Ожидать.** `loaded=true` ровно у одной модели, `loadedModelCount=1`, `total` = числу моделей
на диске. `state` воркера и `loaded` в API совпадают.

**Искать.** Также лог поллера: `loaded_models:1, models_reported:N` (не «2 загружено»).

**FAIL, если:** `loaded=true` у двух моделей, `loadedModelCount` завышен, панель предлагает
«загруженную» модель, которой нет в памяти.

---

## 5. Окно 256000 из меню модели доезжает до загрузки

**Сделать.** В карточке модели выставить `contextLength=256000` (и **убедиться, что поле
«окно на клиента» / `contextPerSeq` пусто или согласовано** с суммарным), сохранить, загрузить.

**Ожидать.** Модель грузится с суммарным окном 256000 (или с физическим потолком карты, если
256000 не влезает — но тогда с **внятным** отказом и числами).

**Искать.** В логе одну строку `declared n_ctx window resolved` с полями
`profile_context_length`, `profile_context_per_seq`, `slots`, `effective_total/per_seq`, `причина`.
Дополнительно `effective auto-reload ceiling raised`, если потолок авто-роста поднимался.

**FAIL, если:** загрузка идёт с 32768 без сообщения о причине (значит `contextPerSeq` всё ещё
перебивает суммарное — снять старое значение в карточке).

---

## 6. Размышления не попадают в основной ответ

**Сделать.** `POST $DATA/v1/chat/completions`, `stream=true`, модель с reasoning (qwen3.8),
prompt, провоцирующий размышления (например «Think step by step, then answer: 17*23»).

**Ожидать.** Текст размышлений в `delta.reasoning_content`, в `delta.content` — только ответ.
Ровно один `data: [DONE]`.

**Искать.** В логе `stream egress finalize` с `reasoning_chars > 0`; в потоке отсутствие
`<|channel|>analysis`, `<｜end▁of▁thinking｜>`, `[THINK]`, `</think>` внутри `content`.

**FAIL, если:** маркеры или текст рассуждений видны в `content`.

---

## 7. Большой tool-call не даёт «stream truncated»

**Сделать.** Запрос `stream=true`, требующий сгенерировать большой файл (200+ строк) через
`write_file`-подобный инструмент.

**Ожидать.** Ответ доходит целиком, ровно один `[DONE]`, в клиенте нет
«stream truncated by upstream before completion».

**Искать.** В логах: `stream truncated=0`, `reason=upstream_eof=0`; поле `reason=our_limit`
допустимо только как диагностика лимита и **не** должно превращаться в ошибку клиенту.
При необходимости поднять `LB_STREAM_MAX_LINE_BYTES`.

**FAIL, если:** клиент получает «stream truncated», либо `reason=our_limit` с ошибкой в потоке.

---

## 8. Не-стрим агентский запрос идёт потоком при `Accept: text/event-stream`

**Сделать.** `POST $DATA/v1/chat/completions` с `stream=false` **и** заголовком
`Accept: text/event-stream`.

**Ожидать.** Ответ — SSE: заголовок `X-R258-Nonstream-As-Sse: 1`, `Content-Type: text/event-stream`,
ровно один `data: [DONE]`, тело не JSON. Тот же запрос **без** этого Accept — цельный JSON.

**Искать.** `curl.exe -s -D - ...` → заголовки; в логе `R258: buffering disabled — client accepts SSE`
и `SSE passthrough complete`.

**FAIL, если:** при `Accept: text/event-stream` приходит цельный JSON (значит клиент ждёт буфер) или
два `[DONE]`.

---

## 9. Выбор узла для загрузки: мёртвый не выбирается, отказ быстрый

**Сделать.** (а) `POST $API/api/v1/cluster/models/<MODEL>/reload {"operation":"load"}` **без**
`backendId`; (б) тот же вызов с явным `backendId` недоступного узла; (в) с вымышленным id.

**Ожидать.** (а) `200`, в ответе поле `skipped` со списком непригодных узлов и причиной;
операция ушла только на здоровый узел. (б) **503 за единицы секунд** (не минуты) с причиной,
адресом и `skipped`. (в) `404` быстро.

**Искать.** Логи: `model load target selected` (targets/skipped), `model load target skipped … reason=…`
(`backend is unhealthy` / `offline` / `disabled`).

**FAIL, если:** загрузка уходит на мёртвый узел, либо явный мёртвый узел даёт многоминутное
ожидание вместо быстрого отказа.

---

## Что фиксировать в отчёте по каждому пункту

1. Номер пункта и PASS/FAIL.
2. Конкретный факт: код ответа, поле API, строка лога — с числами.
3. Если FAIL — время (UTC/локальное), модель, backendId и фрагмент лога (для дальнейшего разбора).

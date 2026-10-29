# R300 (2026-10-29): «модель ещё загружается» — 202 в HTTP-слое, а не error/500

Прогон стенда `b5b45c74`. Проверка R299 на живом стенде дала два факта, которые
R299 закрыть не успел: правка жила в балансере, а HTTP-слой кластерного reload о
ней не знал.

## 1. «Это не ошибка» с кодом 500

**Живой факт.** `POST /api/v1/cluster/models/Qwen3.8-27B-UD-Q4_K_M/reload`
`{"operation":"load","backendId":"cppworker-gpu-bundled-agent"}` ждал **6m26s**
(16.4 ГБ, `max_wait=386s`) и вернул:

```json
{"model":"Qwen3.8-27B-UD-Q4_K_M","operation":"load","results":[{
  "backendId":"cppworker-gpu-bundled-agent",
  "status":"error",
  "message":"модель «Qwen3.8-27B-UD-Q4_K_M» ещё загружается на бэкенде «cppworker-gpu-bundled-agent» (ждали 6m26s): загрузка продолжается в воркере, опрос через 30s покажет итог — это не ошибка",
  "httpStatus":500,
  "reloaded":false}]}
```

Поля `error` в теле нет — значит балансер уже отдал формат R262/R299
(`StillLoading=true`, `Error` пуст). Текст говорил «это не ошибка», код — 500.
То есть ровно тот дефект, который R299 закрывал в балансере, остался живым
этажом выше.

**Причина.** `executeReloadOnBackend` (`internal/api/handlers_cluster_models.go`)
смотрел только на `opResult.Success`. Всё, что не успех и не «занято»,
превращалось в `status:"error"` + `httpStatus:500` — флаг `StillLoading` не
читался нигде в этом файле (единственное место во всём `internal/api` —
`handlers_model_management.go`, другой эндпоинт). Класс дефекта тот же, что в
серии ловили четыре раза: правило есть, но новый (здесь — соседний) слой его не
вызывает.

Кроме того, общий код ответа (`overallReloadStatus`) знал только «успех /
отложено / прочее→200», поэтому 202 всё равно вернулся бы как 200.

**Что сделано.**

- `clusterReloadModelBackendResult` — поля `stillLoading` и `pollAfterSec`.
- `stillLoadingReloadResultR300` — единый вид «ещё идёт»: `status:"started"`,
  HTTP 202, `stillLoading:true`, `pollAfterSec:30`, ни одного поля ошибки.
- `executeReloadOnBackend` — ветка `opResult.StillLoading` **до** превращения в
  error и **до** восстановления прежней модели из R297: новая загрузка ещё идёт,
  возвращать на её место старую нельзя.
- `overallReloadStatus` — 200 важнее; если успеха нет, но есть 202 — общий код
  202; иначе прежние правила (все отложены → 503).
- `modelLoadStateOnBackend` / `modelLoadStateR300` — состояние модели
  (`loaded`/`loading`/`error`), а не факт наличия записи.

## 2. «Already loaded» у модели, которая грузится

**Живой факт.** Пока шла та же загрузка (16.4 ГБ, ~6 мин), повторный
`{"operation":"load"}` вернулся за **0.22 с**:

```json
{"results":[{"backendId":"cppworker-gpu-bundled-agent","status":"ok",
 "message":"already loaded","httpStatus":200,"reloaded":false}]}
```

В тот же момент `GET /api/v1/cluster/models/loaded` показывал у этой модели
`"state":"loading"`, а `GET /api/v1/cluster/models/loading` — её же в списке
грузящихся. Лог подтверждает путь: `cluster model operation requested` и сразу
ответ, без `executeLlamaCppLoad` (первая загрузка в это время поллилась с
`max_wait=386s`).

**Причина.** `isModelLoaded()` отвечает на вопрос «есть ли запись в
`LoadedModels`», а поллер кладёт туда и грузящуюся модель (`state:"loading"`).
Ветка «уже загружено» этого не различала — и оператор получал «ok» про модель,
готовую к работе ещё через шесть минут.

**Что сделано.** Перед веткой «already loaded» стоит проверка состояния:
`loading` → 202 + `stillLoading` + совет опросить статус. Фраза «already loaded»
теперь произносится только про `state:"loaded"`.

## Тесты

`internal/api/cluster_reload_still_loading_r300_test.go`:

- `TestR300_StillLoadingResultIs202` — 202/`stillLoading`/`pollAfterSec`, пустой
  `error`, `status:"started"`, `deferred` не выставлен (отложенность — это 503).
- `TestR300_ModelLoadState` — 7 подслучаев: loaded/loading/error/нет записи/чужой
  бэкенд/нет cluster state/имя с `:tag`.
- `TestR300_OverallStatusAccepted` — 7 подслучаев общего кода: успех важнее 202,
  202 вместо 200, «все отложены» → 503, пустой список → 404.
- `TestR300_HandlersKeepStillLoadingContract` — стюард: ветка `StillLoading` в
  HTTP-слое существует, проверка `loading` стоит ВЫШЕ `result.Message =
  "already loaded"` (откат вернул бы живой дефект).

`go build -tags llama_stub ./...` — ok. `go test -tags llama_stub ./internal/api
./internal/balancer ./pkg/types ./internal/settings` — все пакеты ok.

## Что проверять на стенде

1. Повторный `load` во время загрузки: HTTP **202**, `stillLoading:true`,
   `pollAfterSec>0`, `status:"started"`, ответ за доли секунды.
2. Загрузка дольше `max_wait` (Qwen3.8-27B ≈ 6m26s): per-backend
   `httpStatus:202` + `stillLoading`, общий код ответа **202**, поля `error` нет.
3. Обычная загрузка (gemma ≈ 2 мин): по-прежнему 200, `status:"ok"`.
4. WebUI: нажатие «загрузить» поверх идущей загрузки показывает «загружается», а
   не «ошибка загрузки».

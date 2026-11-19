# Читаемость и разбиение перегруженных файлов (не путать с «двумя мозгами»)

**Дата:** 2026-11-19. **Статус:** 🟡 **R1, R4 и R7 закрыты; R2 закрыт (R531–R540); в R3 сделана часть (R506/R509/R510/R524/R525/R526/R527)** (R506 0.7.310 — стражи структуры; R509 0.7.313 — `internal/cppbackend` по ответственности; R510 0.7.314 — `handlers_openai.go` и `handlers_backends.go` разбиты по операциям; R507 0.7.311 — одна реализация хелперов; R531–R538 — выносы `proxyRequestLlamaCpp`; R539 — цикл и финализация; R540 — подготовка/preflight/отправка). Впереди: остаток R3 (`handlers_model.go`, общий модуль стрима), R5 (277 `_rNNN` → тематические имена), R6 (WebUI-модули).
**Замер на момент R506 (2026-11-19, `scripts/structure_report.ps1`):** функций > 1000 строк — 2,
> 300 — 41, ≥ 150 — 120; файлов ≥ 1500 строк — 27; `internal/cppbackend/backend.go` — 4117;
**боевых файлов с `_rNNN` — 277** (список-база `docs/architecture/structure-baseline-r506.txt`).
Эти числа — исходное состояние долга; цель §5 не изменилась.
**Номер раунда:** не закреплён (`R496` занят потоком `tool_guard`).
**Связанные документы:** `docs/architecture/refactoring-plan.ru.md` (этапы 3–8 закрыты — там про **поведение**:
один путь запроса, одно окно, одна очередь), `docs/plans/2026-11-19-consistency-audit.md` (там про **согласованность
величин**; часть находок оттуда — следствие структуры, но чинится отдельно), `CHANGELOG.md` (стиль описаний).

**Чем эта задача НЕ является.** Это не «переписать балансер» и не продолжение плана R459–R478. Тот план закрывал
расхождения поведения (три «это стрим?», четыре механизма окна, четыре очереди) — этапы 3–8 сделаны. Здесь речь
только о **физической структуре кода**: слишком большие файлы и функции, имена с номером раунда, дубли хелперов,
разросшиеся каталоги. Поведение обязано остаться неизменным, и это главное ограничение задачи.

---

## 1. Замеры (на 2026-11-19, `HEAD ≈ f0b3d67f`)

Все числа получены `Get-Content | Measure-Object -Line` по нашему коду; `c/llama.cpp` (vendored submodule),
`webui/node_modules`, `dist/`, `test-results/` исключены.

### 1.1 Самые крупные файлы

| Строк | Функций | Файл |
|---|---|---|
| 4117 | **103** | `internal/cppbackend/backend.go` |
| 3850 | — | `webui/js/modules/gguf-renderer-detail-render.js` |
| 3565 | — | `webui/js/app.js` |
| 3476 | — | `cmd/cppworker/handlers_model.go` |
| 3464 | — | `c/bridge/bridge.c` (C-мост, cgo-слой) |
| 3136 | — | `cmd/cppworker/handlers_chat.go` |
| 2829 | — | `webui/js/modules/renderers.js` |
| 2812 / 2786 | — | `webui/js/i18n/ru.js` / `en.js` |
| 2694 | 12 | `internal/balancer/llamacpp_transport.go` |
| 2369 | — | `cmd/cppworker/handlers_openai.go` |
| 2220 | — | `internal/api/handlers_backends.go` |
| 2200 | 59 | `c/bridge/bridge.go` |
| 2181 | — | `internal/balancer/proxy.go` |
| 2155 | — | `internal/balancer/nctx_reload_handlers.go` |
| 2030 | — | `internal/balancer/model_management.go` |

Файлов ≥ 800 строк — **97**. Из них 42 — в `internal/balancer`.

### 1.2 Самые длинные функции (Go, ≥150 строк — 138 штук)

| Строк | Функция | Где |
|---|---|---|
| **2426** | `proxyRequestLlamaCpp` | `internal/balancer/llamacpp_transport.go:31-2456` |
| **1426** | `writeChatStreamResponseWithTools` | `cmd/cppworker/handlers_chat.go:1836` |
| 938 | `ServeHTTP` | `internal/balancer/proxy.go:945` |
| 907 | `proxyRequestLlamaCppNonStream` | `internal/balancer/llamacpp_transport_nonstream.go:23` |
| 856 | `LoadModelWithOpts` | `internal/cppbackend/backend.go:995` |
| 742 | `preflightNCtxReloadIfNeeded` | `internal/balancer/nctx_reload_handlers.go:748` |
| 740 | `main` | `cmd/balancer/main.go:38` |
| 692 | `handleReloadModel` | `cmd/cppworker/handlers_model.go:2181` |
| 629 | `builtinListR278` | `internal/settings/builtin_r278.go:57` |
| 624 | `handleCppWorkerUpdateConfig` | `cmd/cppworker/handlers_config.go:205` |
| 611 | `proxyRequest` | `internal/balancer/proxy_request.go:159` |
| 599 | `main` | `cmd/cppworker/main.go:143` |
| 562 | `setupRoutes` | `internal/api/routes.go:9` |
| 539 | `agentRegisterHandler` | `internal/api/handlers_agents.go:22` |

`proxyRequestLlamaCpp` — 2426 строк в **одной** функции главного пути инференса: это и есть основная причина, почему
правка в стриминге требует чтения 2.4k строк и почему регрессии R459–R493 концентрировались именно здесь.

### 1.3 Раздробленность и «шрамы раундов»

- `internal/balancer`: **272** файла кода, 86 155 строк, средний файл — **317** строк. При этом 42 файла ≥500 строк
  и **16** файлов <50 строк. То есть одновременно «гиганты» и «осколки».
- Тестов в пакете — **492** файла против 272 файлов кода (1.8:1). Часть из них — стражи на 40 строк.
- Файлов с номером раунда в имени (`*_rNNN.go`), **не тестов**: **284**. Примеры: `nctx_ceiling_r267.go`,
  `admission_admitter_r493.go`, `growth_needed_rule_r330.go`. Имя файла перестало отвечать на вопрос «что здесь лежит».
- Дубли хелперов по репозиторию: `firstNonEmpty` — 4 определения, `defaultIfZero`, `containsFold`, `minInt`,
  `maxInt`, `roundUpPow2` — по 2.
- Документация: 294 `*.md` в `docs/` (331 в репозитории с README/планами). `tests/` — 73 файла, 31 265 строк.
- `.golangci.yml` **не содержит** ни `funlen`, ни `cyclop`/`gocognit`: длина функций сейчас ничем не ограничена,
  поэтому долг растёт. `max-same-issues: 3` — ужесточать линтер «в лоб» нельзя, CI покраснеет на существующем коде.

### 1.4 Как перемерить (без новых зависимостей)

```powershell
# файлы: топ-20 и «≥800 строк»
$f = Get-ChildItem -Recurse -File -Include *.go,*.c,*.h,*.js | Where-Object { $_.FullName -notmatch 'node_modules|\\c\\llama\.cpp\\|\\dist\\|test-results' }
$f | % { [pscustomobject]@{ L=(Get-Content $_ | Measure-Object -Line).Lines; P=$_.FullName.Replace($PWD.Path+'\','') } } |
  Sort-Object L -Descending | Select-Object -First 20

# функции ≥150 строк
foreach ($x in $f | ? Extension -in '.go','.h' | ? { $_.Name -notmatch '_test' }) {
  $l = Get-Content $x; $s = @(); for ($i=0; $i -lt $l.Count; $i++) { if ($l[$i] -match '^func ') { $s += $i } }
  for ($j=0; $j -lt $s.Count; $j++) { $e = if ($j+1 -lt $s.Count) { $s[$j+1] } else { $l.Count }
    if (($e-$s[$j]) -ge 150) { "{0,5} {1}:{2}" -f ($e-$s[$j]), $x.FullName.Replace($PWD.Path+'\',''), ($s[$j]+1) } }
}
```

---

## 2. Почему это мешает работать (а не «эстетика»)

1. **Ревью и правки.** Функция на 2426 строк: любая правка требует держать в голове весь путь, а `git blame`
   показывает 40+ авторов-правок в одном месте — конфликты при параллельной работе (сейчас в дереве второй поток).
2. **Поиск.** 284 файла с `_rNNN` в имени: «где живёт правило роста окна?» — ищешь по `grep`, а не по имени файла.
3. **Дубли.** `firstNonEmpty` ×4 и `roundUpPow2` ×2 — ровно тот механизм, из-за которого в этом же проекте
   расходились KV-формулы и таблицы единиц (см. §4 аудита согласованности).
4. **Тесты-осколки.** 492 тестовых файла в одном пакете: `go test ./internal/balancer` идёт минутами, а найти
   нужный страж по имени невозможно.
5. **Ограничения нет.** Без `funlen` следующая функция на 500 строк не заметит никто.

---

## 3. Принципы (нарушение любого = задача не принята)

- **Только структура, без изменения поведения.** Ни одной правки логики, констант, текстов ошибок, JSON-полей,
   заголовков и i18n в этих коммитах. Любая найденная заодно логическая ошибка — **отдельная задача**, не «попутно».
- **Один шаг = один коммит = `git mv` / чистый перенос.** Переносы — через `git mv`, чтобы `git log --follow` работал.
- **Сначала стражи, потом перенос.** Каждый шаг начинается с проверки, которая ловит случайное изменение поведения
  (см. R1), и заканчивается её зелёным прогоном.
- **Не дробить ради галочки.** Файл 300 строк с одной ответственностью не трогаем. Цель — связность, а не размер.
- **Не переименовывать публичный API пакетов** (`pkg/types`, `pkg/protocol`, `internal/settings`): это уже контракт
  для тестов, WebUI и доков.
- **Не воевать с C-мостом и vendored llama.cpp.** `c/bridge/*` (cgo) разбивается только по «швам» C-функций, если
  вообще; `c/llama.cpp` — submodule, не наш код.

---

## 4. Этапы

### R1. Стражи структуры (1 коммит, без переносов) — **СДЕЛАНО (R506)**
- `funlen` (lines: 200, statements: 100) и `gocognit` (threshold: 60) в `.golangci.yml` **с `issues.new-from-rev`**
  (только новые нарушения). Проверить, что текущий код не краснеет: если краснеет — поднять порог до фактического
  максимума и записать «долг», а не отключать линтер.
- `scripts/structure_report.ps1` (из §1.4) — печатает топ файлов/функций и число `_rNNN`; пороги в нём = критерии §6.
- Тест-страж «нет новых файлов `*_rNNN.go` в боевом коде» (как `stage_steward_r477_test.go`, сканирует дерево).
- **Проверка «поведение не изменилось»:** `go test ./... -tags llama_stub` + `-race` на `internal/balancer`,
  `internal/api`, `cmd/...`; `gofmt -l .` пусто; `go vet` чисто.
**Что именно сделано в R506:** `funlen` (200 строк / 100 операторов) и `gocognit` (60) включены в
`.golangci.yml`; job `lint` уже работает с `only-new-issues: true`, поэтому долг не краснит CI, а новые
нарушения — краснят. `scripts/structure_report.ps1` печатает все показатели §5 и сравнивает с целью
(`-FailOnRegression` → код возврата 1). Страж `tests/structure_guard_r506_test.go` держит список-базу
`docs/architecture/structure-baseline-r506.txt`: новый боевой `*_rNNN.*` краснит тест, а устаревшая запись
в списке — тоже (список обязан уменьшаться вместе с переименованиями R5).

### R2. `proxyRequestLlamaCpp` — 2426 → ≤ 250 на функцию (главный шаг)
Разбить по **стадиям**, которые уже названы в логе (`stage_r477.go`), а не по «удобным кускам»:
`подготовка тела/параметров` → `выбор клиента и таймаутов (first-byte/очередь)` → `отправка и обработка заголовков`
→ `цикл чтения NDJSON/SSE` → `декомпозиция кадра (content/reasoning/tools)` → `финализация (usage, done, ошибки)`.
Каждая стадия — функция с явным входом/выходом (`type llamaCppRun struct{ ... }` как состояние прохода), без пакетных
переменных. Поведение фиксируется golden-тестами из R1 до переноса.
**Приёмка:** ни одна функция файла не > 200 строк (это же лимит `funlen` в CI: `lines: 200`, `statements: 100` — обнаружено на R529); существующие golden/SSE-тесты зелёные без правок; трасса стадий
(`stage_r477`) печатает те же стадии в том же порядке на одном и том же запросе до/после.

**План исполнения R2 (зафиксирован по замеру AST на 0f20c053, ещё НЕ начат).**
`proxyRequestLlamaCpp` — 31..2458, 2428 строк. Границы стадий уже размечены вызовами
`SetStageR477` (строки 32, 151, 504, 864) — они и будут швами:

| Стадия | Строки | Размер | Что там |
|---|---|---|---|
| `prepare` | 32–150 | 119 | backendID→state, targetURL, имя модели (atomic для watcher), схемы инструментов, native/translated path и тело |
| `preflight` | 151–503 | 353 | `preflightNCtxReloadIfNeeded` + переписывание тела, отказы 413/503, авто-загрузка, admission/очередь, заголовки запроса к воркеру |
| `stream` | 504–863 | 360 | отправка, чтение NDJSON/SSE, разбор кадров (content/reasoning/tool_calls), ретраи EOF |
| `finalize` | 864–2458 | 1595 | запись клиенту, usage/done-кадры, ошибки, диагностика, сводки |

Порядок работ (каждый шаг — отдельный коммит, поведение не меняется):
1. `llamaCppRun` — структура состояния прохода; поля заводятся ТОЛЬКО для того, что реально
   переживает границу стадии. Никаких копий: указатели на локальные переменные, как в R526/R527.
2. `prepareUpstreamRequest` (стадия `prepare`) — чистые вычисления + тело; `SetStageR477` остаётся в
   `proxyRequestLlamaCpp` ДО вызова, чтобы порядок стадий в трассе не менялся.
3. `awaitAdmissionAndLoad` (стадия `preflight`) — вокруг уже существующего
   `preflightNCtxReloadIfNeeded`; отказы отдают тот же ответ и тот же код возврата.
4. `readUpstreamStream` (стадия `stream`) + `decodeUpstreamFrame` — цикл и разбор кадра.
5. `finalizeClientResponse` (стадия `finalize`) — но её 1595 строк режутся на подстадии:
   `usage`/`done`-кадр, `nativeDone` (стрип content), `tool_calls` из SSE, `errorContext`,
   `diag`/сводка. Целевой предел — 250 строк на функцию (приёмка R2).
6. `defer`-ы (watchdog/stall/abort, снятие метрик) остаются в `proxyRequestLlamaCpp` и в том же
   порядке: они относятся к запросу целиком, а не к стадии.

**Как доказывается неизменность (до первой правки).** В R1 уже есть golden-тесты
(`proxy_integration_test.go`, SSE-тесты `tests/llamacpp_proxy`); дополнительно снимается
трасса стадий одного запроса (`stage_r477`) до и после — она обязана совпасть по составу и
порядку. Плюс тот же приём, что в R527: тела, вырезанные текстом, сравниваются с исходником
после обратного механического преобразования (алиасы/указатели), поэтому «переписал по
памяти» не пройдёт.

### R3. `cmd/cppworker`: стрим-писатели и хендлеры — **частично сделано (R510)**
- `writeChatStreamResponseWithTools` (1426) и `writeChatStreamResponse` (519) → общий модуль стрима поверх
  `safeStreamWriter` (этап 7 плана R459–R478 сделан частично: «один писатель потока»).
- `handlers_model.go` (3476) разделить по операциям: `load` / `reload` / `unload` / `params` / `progress`.
- `main()` (599) → `buildServer()`, `registerRoutes()`, `run()`.
**Приёмка:** `go test ./cmd/cppworker/...` зелёные; «ровно один терминатор» (golden NDJSON/SSE) — байт-в-байт.
**Итог R510 (часть R3, факт).** Разбиты два файла с обработчиками — по операциям, без
правки логики: `cmd/cppworker/handlers_openai.go` 2496 → `handlers_openai.go` 306
(типы и перевод запроса) + `handlers_openai_chat.go` 595 + `handlers_openai_stream.go` 710
(писатели стрима) + `handlers_openai_completions.go` 519 + `handlers_openai_models.go` 191 +
`handlers_openai_usage.go` 90; `internal/api/handlers_backends.go` 2363 → 624 (роли и
rpc-ссылки) + `handlers_backends_list.go` 616 + `handlers_backends_mutate.go` 667 +
`handlers_backends_extra.go` 355. Файлов ≥1500 строк стало 23 (было 25).

Доказательство неизменности поведения то же, что в R4: разрезание по границам объявлений и
склейка тел в исходном порядке совпадает с исходным телом (нормализованное сравнение); плюс
`go vet` 0, `gofmt` чисто, `go test` и `-race` по `cmd/cppworker` и `internal/api` зелёные.

**Очередь выносов R2 (замер на 239e7fd4, `proxyRequestLlamaCpp` = 2291 строка).**
Каждый кандидат — самостоятельный блок, который можно вынести в функцию
≤ 200 строк за один коммит (объявление `proxyRequestLlamaCpp` обязано остаться
на строке 31, иначе `funlen` считает её «новой» и краснеет). Порядок:
отказ по семейству модели + лог; перевод пути/тела (`llamacppPath`, `isStreaming`,
`translatedBody`); сборка URL и правка тела для `/api/models/unload`; установка
заголовков ответа до `WriteHeader` (`X-Accel-Buffering`, `Cache-Control`,
`forwardUpstreamWarningHeaders`); разбор `X-Request-Id`/`X-Cppworker-Load-Duration-Ms`;
создание запроса и клиента с таймаутами; не-2xx обработка (passthrough 4xx/5xx).
Затем — главное: `readUpstreamStream` (966) и `finalizeLlamaCppStream` (465)
разложить сразу до ≤ 200 строк каждый (швы: ветки «native/NDJSON» и
«SSE→NDJSON», usage/done, сводки, кадры ошибок), каркас структур описан выше.
**Статус очереди на 2026-11-18:** пункты 1–7 сделаны (R532/R533/R534/R536/R535/
R537/R538), решающий шаг — сделан в R539 (ниже), остаток координатора — в R540.
**Итог R542 (R2, не-потоковый транспорт, факт).** `proxyRequestLlamaCppNonStream`
доведён до лимита: **907 → 125** строк (318 → 47 операторов) после выноса
`awaitLoadedModelNonStream` (89), `answerNonStreamUpstreamError` (132:
нормализация R335.1 + сброс счётчика + n_ctx R83 + passthrough 4xx R60.10 +
устаревший снимок R160 + обёртка 5xx), `answerEmptyNonStreamBody` (85, R60.40) и
`writeNonStreamResponse` (112: нативный passthrough R65d/R66, SSE, перевод
OpenAI→Ollama). Все функции файла ≤ 170 строк / ≤ 53 операторов.
**Доказательство:** набор строковых литералов файла — **0 различий** (323);
`go test ./internal/balancer/` и `-race` зелёные. Единственное изменение формы:
`defer resp.Body.Close()` для тела ответа-ретрая переехал в вызывающий (в стадии
он закрывал бы тело сразу при возврате).
**Итог R541 (R2, второй транспорт, факт).** `proxyRequestLlamaCppNonStream`
(`llamacpp_transport_nonstream.go`) **907 → 510** строк (318 → 192 оператора):
вынесены `nonStreamPreflight` (preflight окна и отказы 413/404/503),
`prepareNonStreamBody`, `newNonStreamUpstreamRequest`, `sendNonStreamRequest`
(ретраи 1s/2s/4s), `autoTuneOnReturnNonStream` (тело `defer`), и
`assembleSSEToOllamaBody` (сборка Ollama-тела из SSE, лимит строки R264);
максимум — 170 строк / 38 операторов. **Доказательство:** сверка наборов
строковых литералов файла — **0 различий** (323 литерала); `go test
./internal/balancer/` зелёный. **Остаток:** 503-загрузка, не-2xx
(R60.10/R160/R335.1) и пустое тело — после их выноса функция уложится в `funlen`.
**Итог R540 (R2, стадии подготовки/preflight/отправки, факт).** Остаток
координатора разложен по стадиям плана: `prepareUpstreamCall` (96 строк),
`awaitAdmissionAndLoad` (32), `prepareUpstreamRequest` (22), `sendRequest` (109),
`awaitLoadedModel` (96), `writeStreamHeaders` (27); состояние стадии —
`upstreamCall` (порядок полей по `fieldalignment`). `proxyRequestLlamaCpp`
**483 → 147** строк (39 операторов). **Приёмка R2 выполнена:** в файле нет ни
одной функции > 200 строк и > 100 операторов (максимум — `absorbToolCallBuffer`
165/71), `gocognit` не превышает 60 ни в одной функции. Доказательство: набор
строковых литералов файла отличается ровно одним — `modelFromCtx := ""` стал
нулевым значением поля; порядок `defer`-ов и все ранние `return` (отказ
семейства, вердикт preflight, ошибки запроса/отправки, исчерпанный polling,
не-2xx) сохранены; `go test ./internal/balancer/` и `-race` зелёные, сторожа не
правились. **R2 закрыт.**
**Итог R539 (R2, шаг решающий, факт).** Цикл и финализация разложены сразу до
лимита `funlen`: `proxyRequestLlamaCpp` **2134 → 485** строк (221 оператор),
состояние прохода — `llamaCppStreamState` (порядок полей по `fieldalignment`),
цикл — `readUpstreamStream` (120) + `startUpstreamReaders` (99) +
`handleNativeNDJSONLine` (66) + `accumulateNativeChunk` (131) +
`normalizeNativeDoneLine` (63) + `translateSSEToNDJSON` (40) +
`accumulateChatDelta` (48) + `absorbToolCallBuffer` (167) +
`absorbChatDoneChunk` (61) + `accumulateGenerateDelta` (47) +
`translateAndWriteFrame` (154); финализация — `finalizeStream` (38) +
`flushHeldTail` (34) + `emitDeferredDone` (25) + `emitTerminalDoneOnly` (44) +
`maybeAutoContinue` (105) + `continueTruncatedResponse` (91) +
`reportPrematureEnd` (95). Максимум по операторам — 97 (`absorbToolCallBuffer`)
при лимите 100. **Доказательство неизменности:** `go/scanner`-сверка наборов
строковых литералов файла до/после — 648 литералов, отличие одно (`lastData := ""`
стал нулевым значением поля); частоты идентификаторов состояния — разница ровно
−1 на поле (объявление переехало в структуру); `go test ./internal/balancer/`
зелёный (167 с) и `-race` зелёный; сторожа R298/R377/R477/R493/R299/R360/R376
не правились. **Метрика §5:** функций > 1000 строк — **2 → 0 (цель выполнена)**.
Остаток R2 — стадии подготовки/preflight/отправки (первые пункты очереди по
логике работы: `prepareUpstreamRequest`, `awaitAdmissionAndLoad`, отправка с
ретраями), они уже вынесены по частям, но координатор ещё 485 строк.
**Итог R530 (R2, шаг 2, факт).** После отката R529 — только блоки без состояния:
`applyPreflightOutcome` (97 строк: вердикт preflight, отказы 413/404/503, правка
`num_ctx` тела), `autoTuneOnReturn` (38: `defer`-тело AutoTune, имя модели
указателем — оно уточняется в цикле R422), удалён мёртвый
`extractUpstreamGenerateDoneChunk` (вызовов не было и до R529).
`proxyRequestLlamaCpp` **2428 → 2325**; новые функции 97/38 строк и 32/21 оператор
(лимиты 200/100). Проверки: build/vet чисто, `go test ./internal/balancer/`
зелёный (173 с). Большие куски (цикл 966, финализация 465) требуют разложения сразу
до ≤200 строк — следующий шаг.

**R529 (R2, шаг 1) — ОТКАЧЕНО: найдено ограничение линтера (факт, 2026-11-18).**
Разбор `proxyRequestLlamaCpp` был доведён до рабочего состояния: 2428 → `proxyRequestLlamaCpp`
887 + `finishLlamaCppStream` 1228 + `finalizeLlamaCppStream` 465; тела перенесены
дословно (доказано обратным сравнением: 1119 и 429 непустых строк), тесты и `-race`
зелёные. Но CI-линт покраснел: `funlen` в этом репозитории ограничивает И строки
(200), и вызовы (100) — любая промежуточная функция длиннее 200 строк = красный CI.

**Вывод для R2.** «Один разрез на две функции» невозможен: каждый шаг обязан сразу
давать функции ≤ 200 строк. Значит R2 — это разложение 2428 строк на ~12–15 функций
за один коммит (или серию коммитов, где КАЖДЫЙ уже укладывается в лимит), с
состоянием прохода в структуре и дословными телами. Коммит откачен (`33709bce`),
WIP сохранён в `git stash` («R529-wip-loop-extraction»): там уже готовы структуры
`llamaCppRun`/`llamaCppStreamState`/`llamaCppFinalize`, вынесены финализация и
цикл; остаётся разбить цикл (966 строк) и финализацию (465) до ≤ 200 строк каждая.

**Итог R527 (часть R3, факт).** `cmd/cppworker/handlers_chat.go`:
`writeChatStreamResponseWithTools` **1426 → 954** (AST) — вынесено **начало** стрима,
семи функциями того же файла: `startToolsStreamGeneration(run *toolsStreamRun) bool`
(100 — дельты, связывание callback, генерация и кадры ошибок), `onStreamToken` (60 —
callback: отмена/обрыв, чистка токена, thinking сразу, удержание блока размышлений),
`emitContentWindow` (145 — предохранители и нарезка окна удержания),
`releaseHoldIfOverdue` (57 — R482/R485), `writeToolsStreamGenerationError` (39 — кадры
ошибки, включая reload-loop-limit/413), `startToolsStreamKeepalive` (55 — кадр «запрос
принят» R189 + heartbeat, cleanup возвращается функцией), `logToolsStreamSummary`
(121 — сводка хода и done-кадр).
Состояние — не копия: `toolsStreamRun` держит указатели на переменные вызывающей
функции, `thinkingParser` — ячейкой (`**ReasoningStreamState`), потому что повтор R406
в финализации заменяет парсер, а callback обязан взять новый.
**Доказательство неизменности.** Перенос делался не руками: диапазоны вырезались
текстом, а идентификаторы изменяемого состояния заменялись механически (сканер токенов
`go/scanner`, поэтому комментарии и строки не тронуты). Обратная проверка: к фрагментам
`git show HEAD` применено то же преобразование, после чего тела совпали **дословно** —
`releaseHoldIfOverdue` 50 непустых строк, callback (`onStreamToken` + `emitContentWindow`)
185 строк, кадры ошибки 37 строк, `logToolsStreamSummary` 111 строк. Ожидаемые различия:
алиасы входа, пролог, и 13 отступных `return true` → `return` в `emitContentWindow`
(метод без значения). В `startToolsStreamKeepalive` изменение ровно одно: cleanup
возвращается функцией вместо `defer` внутри.
**Почему финализация НЕ выносилась.** Первый вариант (вынести 911 строк финализации в
`finishToolsStream`) собирался и проходил `go vet`, но покраснели два сторожа:
`TestR394_ToolsPathUsesBudgetAwareReason` (AST: ≥3 вызовов `toolsTerminalReasonR394`
ВНУТРИ `writeChatStreamResponseWithTools`) и `TestR404_Wiring` (точные строки
`if anyStreamed {`, `reasoningOut != "" && !thinkingStreamed`). Тесты не правились —
вынесено начало, финализация осталась там, где её проверяют сторожа.
**Первая попытка покраснела в CI (записано как есть).** Lint (`only-new-issues`):
`funlen` у `startToolsStreamGeneration` — 175 **вызовов** при лимите 100 (лимит
`statements`, не строк), `fieldalignment` у `toolsStreamRun` (432 → 384) и
`chatStreamFinish` (368 → 320), `ineffassign` на `retriedNoThink`. Исправлено:
функция разбита дальше (методы выше), поля структур переставлены ровно по совету
`fieldalignment` (порядок ни на что не влияет — конструкторы на именованных полях),
`retriedNoThink` удалён: присваивание после повтора ни на что не влияло, предикат
читал флаг выше и один раз, а блок R406 ровно один — теперь в вызове стоит `false`
с комментарием. Повторная проверка тем же линтером (`golangci-lint:v1.61.0` в Docker,
`--new-from-patch`, конфиг репозитория) — 0 находок.
Замер после R527 (`scripts/structure_report.ps1`, до → после): функций **> 1000 — 2 → 1**
(осталась `proxyRequestLlamaCpp` 2428), **> 300 — 40 → 40** (сокращённая функция в отчёте
983 всё ещё выше порога, а вынесенные этапы — уже ниже 300), **≥ 150 — 122 → 122**.
**Попутно:** `structure_report.ps1` печатал метрику «>1000» пустой, когда под неё
попадала ровно одна функция (`Where-Object` → одиночный объект без `.Count`); обёрнуто
в `@()` — на HEAD метрика печатает 2, после правки 1.
**Итог R526 (часть R3, факт).** `cmd/cppworker/handlers_chat.go`:
`writeChatStreamResponse` **519 → 283 + 236** — финализация вынесена в
`finishChatStream(in *chatStreamFinish)` в том же файле (перенос в другой файл
запрещён: `load_duration_r493_test.go` считает точную строку кадра в
`handlers_chat.go`). Изменяемые величины передаются указателями, поэтому callback
и `defer` видят те же значения; в `finishChatStream` сохранён локальный `r := in.r`,
чтобы строка `loadDurationNsR493(r.Context())` осталась буквально прежней.
**Итог R525 (часть R3, факт).** `cmd/cppworker/main.go`: `main()` 599 строк разобран
на этапы — `initWorkerLogging` (~20), `loadWorkerConfig` (~15),
`logStartupLayoutDecision` (~25), `applyEnvOverrides` (286), `initWorkerBackend` (~12),
`prepareWorkerRuntime` (~20), `serveAndWait` (173). Вызов
`applyStartupLayoutConfigR370` остался **в `main`** явной строкой: его закрепляет
сторож `startup_config_r370_test.go:141-155` («без правки тестов» → вызов не
переносим). Замер `scripts/structure_report.ps1`: функций > 300 — **41 → 40**;
> 1000 — по-прежнему 2.
**Итог R524 (часть R3, факт).** `handlers_model.go` 3664 → **2897**:
ollama-поверхность (`handleOllamaTags/PS/Show/Copy/Create/Push`,
`writeOllamaShow*`, `modelDigest`, `quantizationForPSModel`, `modelParameterSize`,
`estimateVRAMSizeForModel`, `quantizationOrUnknown`, `resolveModelPath`,
`reloadSplitMode`) перенесена дословно в `handlers_model_ollama.go` (790 строк).
Доказательство: sha256 нормализованного текста перенесённого фрагмента совпал с
версией из `git show HEAD` (762 строки до и после); `gofmt` чисто, `go build`/`go vet`
чисто, тесты `cmd/cppworker` зелёные.

**ОГРАНИЧЕНИЕ, найденное в R524 (определяет способ добить критерий).** Перенос
`writeChatStreamResponseWithTools` из `handlers_chat.go` в отдельный файл **запрещён
правилом «без правки существующих тестов»**: `load_duration_r493_test.go:64` и
`r478_reasoning_scope_test.go:47` читают `handlers_chat.go` по имени файла и ищут в
нём конкретные строки (кадры `load_duration`, тело функции по заголовку). Поэтому
критерий «0 функций >1000» для этой функции достигается только разбором ВНУТРИ файла
(вынесение этапов в функции того же файла), а не переносом. Для `handlers_model.go`
такого ограничения нет — там сторож требует лишь две строки (`defSplitMode := -1`,
`defSplitMode2 := -1`), обе остаются в `handleLoadModel`/`handleLoadWithParams`.

Сторожа, читавшие `handlers_openai.go` по имени (`load_duration_r493`, `reasoning_split_policy_r108`,
`terminal_budget_wiring_r402`), переведены на новый хелпер `readHandlerSourcesR510` — он склеивает
ВСЕ файлы `handlers_openai*.go` пакета. Ожидания не менялись (например «два измерения загрузки» —
по-прежнему 2); изменился только способ собрать текст.

**Остаток R3:** `handlers_model.go` по операциям (load/reload/unload/params/progress);
`main()` 599 → `buildServer/registerRoutes/run`; общий модуль стрима поверх
`safeStreamWriter` (R526/R527 разобрали оба писателя по отдельности, но их общая
часть — терминатор кадра — пока продублирована).

### R4. `internal/cppbackend/backend.go`: 4117 строк, 103 функции — **СДЕЛАНО (R509)**
Разделить по ответственности: жизненный цикл инстанса · загрузка/выгрузка · параметры и профили · снимки состояния
(`ListModels`/`ModelState`) · прогресс и события. Правило из R493e сохраняется: **кто пишет `inst.info`, тот держит
`b.mu`** — перенос не должен вводить новых писателей.
**Приёмка:** файл ≤ 800 строк, ни один файл пакета > 1200; `-race` на `internal/cppbackend` зелёный.
**Итог R509 (факт):** `backend.go` — **273 строки** (было 4450 в момент правки) + 15 файлов по
ответственности: `backend_load_history.go` 183, `backend_model_instance.go` 106,
`backend_lifecycle.go` 271, `backend_context.go` 58, `backend_load.go` 869 (в нём
`LoadModelWithOpts` 856 — предельная функция пакета), `backend_vram.go` 305,
`backend_limits.go` 235, `backend_unload.go` 546, `backend_inference.go` 513,
`backend_chat_template.go` 126, `backend_gpu_metrics.go` 162,
`backend_metrics_internal.go` 113, `backend_system_ram.go` 27, `backend_gguf.go` 491,
`backend_system_misc.go` 93. Заодно разбиты два файла пакета, нарушавших порог 1200:
`hf_downloader.go` 1699 → 290 + `hf_downloader_search.go` 470 + `hf_downloader_download.go` 821;
`model_manager.go` 1326 → 369 + `model_manager_scan.go` 517 + `model_manager_resolve.go` 417.
Самый большой файл пакета теперь 869 строк.

**Как доказано, что поведение не изменилось:** перенос текста без правок, и склейка тел новых
файлов в исходном порядке совпадает с телом исходного файла (проверено нормализованным
сравнением; расхождение — только пустые строки-разделители на границах). Плюс `-race` на
пакете (52 с) и полный набор тестов. Правило R493e сохранено: `inst.info` пишется только под
`b.mu`, стражи `list_models_race_r493e_test.go` зелёные.

**Единственная правка тестов:** два сторожа искали код ПО ИМЕНИ ФАЙЛА (`backend.go`), и после
разбиения «потеряли точку отсчёта». Обновлены так, что проверяемое свойство не изменилось:
R493e-сторож ищет файл с публикацией инстанса по содержимому пакета, R380-сторож разбирает
`backend_load.go` (где теперь путь загрузки). Те же якоря, те же утверждения.

### R5. `internal/balancer`: 272 файла → связные модули
- Сгруппировать по темам, а не по раундам: `selection/` (выбор и порядок), `window/` (окно и n_ctx), `queue/`
  (admission и единая очередь), `transport/` (llama.cpp и SSE), `image/`, `metrics/` (только чтение/публикация).
  Внутри Go подкаталоги допустимы: пакет не обязан оставаться одним (проверить отсутствие циклов импорта — они
  появятся, если тянуть `Proxy`; поэтому переносить **функции на общих типах**, не типы).
- Переименовать `*_rNNN.go` в тематические имена **по одной теме за коммит** (284 файла — это 10–15 коммитов).
  Исключение: файлы-стражи в тестах сохраняют `_rNNN` (по ним ищут регрессию конкретного раунда).
**Приёмка:** `internal/balancer` — 0 боевых файлов с `_rNNN`; число файлов ≤ 200 без роста строк; все тесты зелёные;
`getEffectiveTimeout`-подобные «одна функция в файле» склеены в осмысленные файлы (сейчас 16 файлов <50 строк).

### R6. WebUI (JS)
`gguf-renderer-detail-render.js` (3850), `app.js` (3565), `renderers.js` (2829) → модули по страницам/провайдерам
данных (после R493 провайдеры уже отделены — продолжить тем же швом). `ui-renderer.js` монитора — по панелям.
**Приёмка:** `npm run test:webui` зелёные; ни один `*.js` модуля > 1200 строк; i18n-файлы (`ru.js`/`en.js` 2812/2786)
остаются как есть — это данные, не код.

### R7. Дедупликация хелперов — **СДЕЛАНО (R507)**
`firstNonEmpty` ×4, `roundUpPow2` ×2, `defaultIfZero`, `containsFold`, `minInt`, `maxInt`, `clampInt` — свести в
`pkg/` (например `pkg/numutil`, `pkg/strutil`), вызывающих перевести. **Опирается на аудит согласованности:**
дубли `KV`/единиц оттуда (N-3, N-5) закрываются вместе с этим шагом.
**Итог R507 (по факту, а не по списку выше).** Сведено в `pkg/strutil` и `pkg/numutil`;
в прежних местах — алиасы (`var x = strutil.…`), поэтому вызовы и тесты не менялись.
`minInt`/`maxInt` заменены встроенными `min`/`max` (Go 1.21). Уточнения по списку плана:
`roundUpPow2` — НЕ дубль (одна реализация с полом 512 токенов + обёртка `RoundUpPow2`;
перенос под нейтральным именем сломал бы перезагрузку окна — в страж не включён);
`firstNonEmpty` имел ТРИ поведения (обрезанное значение / исходное / без обрезки вообще),
`defaultIfZero` — ДВЕ семантики (только ноль / ноль и отрицательные) — поэтому вариантов
по одному на поведение, с говорящими именами, а не одна «правильная»: рефакторинг не меняет
поведение. Страж `tests/dedup_guard_r507_test.go`; метрика в `scripts/structure_report.ps1`
(дублей — 0). Дубли KV/единиц из аудита (N-3, N-5) закрыты ранее, в R498.

---

## 5. Критерии готовности (измеримые)

| Метрика | Сейчас | Цель |
|---|---|---|
| Функций > 1000 строк | 2 | **0** |
| Функций > 300 строк | 20+ | **≤ 5** (и все — в `main`/`builtinList`) |
| Функций ≥150 строк | 138 | ≤ 60 |
| Файлов ≥ 1500 строк (наш код, без i18n/C) | 12 | **≤ 3** |
| `internal/cppbackend/backend.go` | 4117 | ≤ 800 |
| Боевых файлов с `_rNNN.go` | 284 | **0** |
| `firstNonEmpty`-подобных дублей | 12 | 0 (один пакет) |
| `funlen`/`gocognit` в CI | нет | включены с `new-from-rev` |

**Замер `scripts/structure_report.ps1` на 2026-11-18 (после R540):** функций
**> 1000 строк — 0** (критерий выполнен, R2); > 300 — 39; ≥ 150 — 126;
файлов ≥ 1500 строк — 23; боевых `_rNNN` — 277; дублей — 0;
`internal/cppbackend/backend.go` — 273. То есть из критериев §5 закрыты три
(> 1000, `backend.go`, дубли), остальные снимаются шагами R3/R5/R6.

Жёстко: **ни одного изменения поведения** — доказывается тем, что весь существующий набор (включая golden NDJSON/SSE
и `-race`) проходит без правок тестов. Если тест пришлось править — это уже не рефакторинг, а изменение контракта.

---

## 6. Риски и как их снимать

| Риск | Снятие |
|---|---|
| Перенос меняет порядок инициализации/побочные эффекты | golden-тесты + `-race` + сравнение трассы стадий до/после на одном запросе |
| Конфликты с параллельным потоком (в дереве уже `tool_guard_r496.go`) | мелкие коммиты, `git mv` без правки содержимого, rebase перед каждым шагом |
| Массовое переименование ломает `git log --follow` и внешние ссылки в доках | `git mv` + отдельный коммит на тему; ссылки в доках правит тот же коммит |
| Разбиение создаёт циклы импорта в `internal/balancer` | переносить функции, не типы; `go list -deps` в проверке |
| «Попутно починю логику» | запрещено §3: найденное — отдельная задача со ссылкой на этот документ |

---

## 7. Чего НЕ делать

- **Не переписывать** `proxyRequestLlamaCpp` «с нуля по-новому»: только извлечение стадий с сохранением порядка.
- **Не трогать** `c/llama.cpp` (submodule), `c/bridge/bridge_stub.go` (generated), `webui/js/i18n/*` (данные).
- **Не вводить** `internal/balancer/*` подкаталоги одним махом: сначала стражи, потом по одной теме.
- **Не ужесточать линтер на существующий код** — CI должен остаться зелёным в день включения.
- **Не смешивать** этот рефакторинг с правками поведения из аудита согласованности: разные коммиты, разные проверки.




# Читаемость и разбиение перегруженных файлов (не путать с «двумя мозгами»)

**Дата:** 2026-11-19. **Статус:** 🟡 **R1, R4, R5, R7 закрыты; R2 закрыт (R531–R542, `proxyRequestLlamaCpp` 2426 → 125 строк); R3 по Go закрыт; R6 начат (9 файлов ≥1500: 2 словаря данных + 2 C-моста + 5 WebUI-модулей; было 27); открыт R6 (WebUI-модули).** Состояние проверено на R628: R5 закрыт (277 → 0 боевых `_rNNN`, список-база удалён), страж «нет `_rNNN`» действует без исключений, страж размера `tests/structure_ratchet_r628_test.go` держит число гигантов (может только уменьшаться).
**Замер на момент R506 (2026-11-19, `scripts/structure_report.ps1`):** функций > 1000 строк — 2,
> 300 — 41, ≥ 150 — 120; файлов ≥ 1500 строк — 27; `internal/cppbackend/backend.go` — 4117;
**боевых файлов с `_rNNN` — 277** (на R628: **0**; список-база `structure-baseline-r506.txt` удалён,
`TestR506_NoRoundNamedProdFiles` запрещает любые `_rNNN` в боевых файлах).
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
(`-FailOnRegression` → код возврата 1). Страж `tests/structure_guard_r506_test.go` запрещает боевой
`*_rNNN.*` **без списка-исключений**: шаг R5 закрыт (277 → 0), база `structure-baseline-r506.txt` удалена
в R628 — держать список, в котором 277 устаревших записей, значит иметь красный страж, которого никто не
читает (именно это и случилось после R621).

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
- **Не считать прогон полным**, если в нём не было `./tests/...` и `node scripts/run-webui-tests.js`. Прямое следствие
  (R628): R5 переименовал 277 файлов, и после него **три** проверки были красными, а их никто не видел — страж `R506`
  (список-база из 277 исчезнувших путей), страж BOM `R245` (380 Go-файлов с BOM после перезаписей через PowerShell 5.1)
  и сторожа `R131/R132` в `internal/cppbackend` (искали функцию в файле по старому имени). Плюс два WebUI-набора читали
  Go-исходники по старым именам. «Зелено» должно означать «зелено везде, где смотрит CI», а не «зелено там, где смотрел я».
- **Не перезаписывать файлы PowerShell-ом** (`Set-Content -Encoding utf8` в PS 5.1 добавляет BOM). Файловый редактор
  BOM не добавляет; после любой массовой правки — `go test -tags llama_stub ./tests/ -run TestR245`.

**R5 — в работе (R599, 0.7.403).** Переименовано 10 боевых файлов в малых пакетах (gent, modelreplication, untimeoverrides, settings, pkg/logger, pkg/types, pkg/version): счётчик 289 → 279, scripts/structure_report.ps1 показывает 269. Тема коммита — «хвосты вне двух больших пакетов»; ссылки на старые имена в комментариях поправлены вместе с именами. Остаток: internal/balancer (155), cmd/cppworker (73), internal/api (22), internal/cppbackend (15), internal/i18n (14).

**R5 — продолжение (R600, 0.7.404).** Переименован весь пакет internal/i18n (14 файлов) по содержимому: у трёх файлов были батч-имена pi2/alancer2/alancer3 — после снятия номера они остались бы бессмысленными, поэтому имена даны по ключам (messages_model_share.go, messages_preflight_nctx.go, messages_rpc_layout.go и т.д.). Счётчик: собственный 279 → 265, structure_report.ps1 269 → 255. Следующий шаг — internal/cppbackend (15), затем internal/api (22).

**R5 — продолжение (R601, 0.7.405).** Переименован весь пакет internal/cppbackend (15 файлов) по ответственности. Ловушка шага: подстрочные совпадения с файлами других пакетов (handlers_rpc_peers_r125.go и др.) — замена сделана с границей слова, обновлено 10 файлов / 21 ссылка, остаток старых токенов 0. Счётчик: собственный 265 → 250, structure_report.ps1 255 → 240. Следующий шаг — internal/api (22).

**R5 — продолжение (R602, 0.7.406).** Переименован весь пакет internal/api (22 файла) по ответственности; ссылки обновлены с границей слова (29 файлов, остаток 0). Попутно найден и починен страж panel_static_routes_r134_test.go: он требовал GET /virtual-models.html = 200, хотя страница переехала в SPA в R595 — проверка переведена на health.html. Счётчик: собственный 250 → 228, structure_report.ps1 240 → 218. Следующие темы — cmd/cppworker (73) и internal/balancer (155).

**R5 — продолжение (R603, 0.7.407).** Тема «обработка tool-call» в cmd/cppworker — 16 файлов (	ool_*/	ools_*). Счётчик: собственный 228 → 212, structure_report.ps1 218 → 203. Остаток: internal/balancer (155) и остаток cmd/cppworker (57).

**R5 — продолжение (R604, 0.7.408).** Тема «окно контекста» в internal/balancer — 17 файлов (
ctx_*, window_*, growth_*). Два файла с одинаковым смысловым именем различены по ответственности: 
ctx_ceiling_source.go (источник потолка) и 
ctx_ceiling_cluster.go (кластерный потолок). Счётчик: собственный 212 → 195, structure_report.ps1 203 → 186. Остаток: internal/balancer (138) и cmd/cppworker (57).

**R5 — продолжение (R605, 0.7.409).** Тема «стриминг ответа» в internal/balancer — 10 файлов stream_*. Счётчик: собственный 195 → 185, structure_report.ps1 186 → 176. Остаток: internal/balancer (128) и cmd/cppworker (57).

**R5 — продолжение (R606, 0.7.410).** Две темы по 7 файлов: «раскладка» (placement_*) и «модель на бэкенде» (model_*) — двумя коммитами. Счётчик: собственный 185 → 171, structure_report.ps1 176 → 167. Остаток: internal/balancer (114) и cmd/cppworker (57).

**R5 — продолжение (R607, 0.7.411).** Темы «автотюн» (utotune_*) и «раскладка по RPC» (pc_* в internal/balancer) — по 5 файлов. Счётчик: собственный 171 → 161, structure_report.ps1 167 → 157. Остаток: internal/balancer (104) и cmd/cppworker (57).

**R5 — продолжение (R608, 0.7.412).** Темы «бэкенд» (5), «отмена запроса» (4), «сессия» (4) в internal/balancer. Счётчик: собственный 161 → 148, structure_report.ps1 157 → 145. Остаток: internal/balancer (91, мелкими кластерами по 2–4 файла) и cmd/cppworker (57).

**R5 — продолжение (R609, 0.7.413).** Темы «аргументы инструментов» (5) и «запрос/активные потоки» (5) в internal/balancer; два ctive_streams различены как ctive_streams.go и ctive_streams_registry.go. Счётчик: собственный 148 → 138, structure_report.ps1 145 → 135. Остаток: internal/balancer (81) и cmd/cppworker (57).

**R5 — продолжение (R610, 0.7.414).** Темы «образы и очередь изображений» (4) и «доступ к воркеру: токены и идентичность» (3) в internal/balancer. Счётчик: собственный 138 → 131, structure_report.ps1 135 → 130. Остаток: internal/balancer (74) и cmd/cppworker (57).

**R5 — продолжение (R611, 0.7.415).** Темы «загрузка и раскладка» (4) и «завершение ответа и очередь» (4) в internal/balancer. Счётчик: собственный 131 → 123, structure_report.ps1 130 → 122. Остаток: internal/balancer (66) и cmd/cppworker (57).

**R5 — продолжение (R612, 0.7.416).** Темы «рассуждения» (4) и «пустой ответ» (3) в cmd/cppworker; два empty_output_retry различены как механизм и бюджет. Счётчик: собственный 123 → 116, structure_report.ps1 122 → 116. Остаток: internal/balancer (66) и cmd/cppworker (50).

**R5 — продолжение (R613, 0.7.417).** Темы «поверхности API» (4: anthropic/responses/vision) и «завершение нативного ответа» (4) в internal/balancer; два ision различены как поверхность и стрим-вариант. Счётчик: собственный 116 → 108, structure_report.ps1 116 → 108. Остаток: internal/balancer (58) и cmd/cppworker (50).

**R5 — продолжение (R614, 0.7.418).** Темы «диагностика и пробы» (5) и «очередь и приём» (4); два unified_queue различены как очередь и гейт. Счётчик: собственный 108 → 99 (впервые ниже сотни), structure_report.ps1 108 → 100. Остаток: internal/balancer (49) и cmd/cppworker (50).

**R5 — продолжение (R615, 0.7.419).** Темы «визуальные входы» (3) и «Gemma/шаблон чата» (3) в cmd/cppworker. Счётчик: собственный 99 → 93, structure_report.ps1 100 → 94. Остаток: internal/balancer (49) и cmd/cppworker (44).

**R5 — продолжение (R616, 0.7.420).** Темы «загрузка и бюджет вывода» (5) и «диагностика и дампы» (5) в cmd/cppworker. Счётчик: собственный 93 → 83, structure_report.ps1 94 → 84. Остаток: internal/balancer (49) и cmd/cppworker (34).

**R5 — продолжение (R617, 0.7.421).** Темы «профили и флаги загрузки» (6) и «RPC-режим, пиры, сеть» (6) в cmd/cppworker. Счётчик: собственный 83 → 71, structure_report.ps1 84 → 72. Остаток: internal/balancer (49) и cmd/cppworker (22).

**R5 — продолжение (R618, 0.7.422).** Темы «имена и разметка вызовов» (7) и «шаблоны/токены» (4) в cmd/cppworker. Счётчик: собственный 71 → 60, structure_report.ps1 72 → 61. Остаток: internal/balancer (49) и cmd/cppworker (11).

**R5 — продолжение (R619, 0.7.423).** Темы «клиентские подсказки» (3) и «конфиг/старт воркера» (5) в cmd/cppworker. Счётчик: собственный 60 → 52, structure_report.ps1 61 → 53. В cppworker осталось 3 файла; основной остаток — internal/balancer (49).

**R5 — продолжение (R620, 0.7.424).** Тема «транспорт и границы тела» (8) в балансере и остаток cmd/cppworker (3) — **в cppworker боевых _rNNN больше нет**. Столкновение имён proxy_request_openai_auto_stream разрешено по смыслу: R258-вариант назван ..._limits.go. Счётчик: собственный 52 → 41, structure_report.ps1 53 → 42. Остаток: только internal/balancer (41).

**R5 — ЗАКРЫТ (R621, 0.7.425).** scripts/structure_report.ps1: **0 боевых файлов с _rNNN** (было 288). 37 шагов, 22 коммита; правила: одна тема — один коммит, замена ссылок с границей слова, коллизии имён разрешались по смыслу. Тестовые файлы (*_test.go, 670 шт.) осознанно НЕ переименовывались: их номер объясняет, почему тест появился.

**R3 — в работе (R622–R624, 0.7.426–0.7.428).** handlers_model.go 2782 → 1471 тремя выносами (каталог моделей на диске; загрузка с параметрами + прогресс; handleListModels). structure_report: файлов ≥1500 строк 23 → 22 (первый сдвиг счётчика на R3). Попутно сделан вывод: сторожа, читающие конкретный файл исходника, обязаны знать про перенос (pc_split_mode_wiring_r108_test.go теперь читает оба файла). Следующий — handlers_chat.go (3392).

**R3 — продолжение (R625, 0.7.429).** Из handlers_chat.go вынесен слой «сборка промпта и параметров из чата» (536 строк, без зависимости от http.ResponseWriter): файл 3529 → 2872. Осталось: ещё 1–2 выноса до порога 1500; главный вкладчик в метрику «функций >300» — writeChatStreamResponseWithTools (985 строк).

**R3 — продолжение (R626, 0.7.430).** handlers_chat.go 2977 → 1913: вынесена writeChatStreamResponseWithTools (985 строк) в handlers_chat_stream_tools.go. Четыре сторожа читали конкретный файл и покраснели — все переведены на чтение/разбор ОБОИХ файлов (класс ошибки R623). Осталось: один вынос ≈450 строк до порога 1500.

**R3 — продолжение (R627, 0.7.431).** handlers_chat.go 1913 → 1304: вынесен слой tool-генерации (631 строка). файлов ≥1500 строк 22 → 21. Добавлен единый helper для сторожей поверхности чата (chat_sources_r627_test.go: eadChatSurfaceR627/parseChatSurfaceR627), переведены 414 и 404. Остаток: 21 файл (цель ≤3), функций >300 — 39 (цель ≤5).

**R3 — продолжение + страж размера (R628, 0.7.432).** Из `internal/balancer/llamacpp_transport.go`
(3317 строк, 51 функция) вынесена группа «разбор нативного ответа» — 9 функций, 352 строки
(`errString`, `emitTruncatedChunk`, `hasFinishReason`, `extractUpstreamDoneChunk`,
`getEffectiveStreamTimeoutSec`, `appendNativeContent`, `stripNativeDoneContent`,
`nativeDoneContentStrippingEnabled`, `autoTuneOnReturn`) в `llamacpp_transport_native.go`; файл
3317 → **2835** (по метрике `structure_report.ps1`), функций в обоих ровно 51 — ничего не потеряно,
поведение не менялось. Счётчик «≥1500 строк» не сдвинулся (2835 всё ещё выше порога) — это честно.

**Главное в R628 — страж размера.** Добавлен `tests/structure_ratchet_r628_test.go` + список-база
`docs/architecture/size-baseline-r628.txt` (21 путь): новый боевой файл ≥1500 строк — красный тест,
запись без файла — тоже (счётчик обязан уменьшаться). Метрика стража = метрика отчёта (непустые строки,
`.go/.c/.h/.js`, без тестов и вендорного `llama.cpp`); это проверено тестом метрики, потому что в R506
уже был случай, когда страж и отчёт считали по-разному.

**Найденный по дороге долг R5.** `TestR506_NoNewRoundNamedProdFiles` был **красным на HEAD**: после
переименований R621 список-база сохранил 277 записей об исчезнувших файлах. Не заметили, потому что
прогоняли `./internal/...`, а пакет `./tests/` в тот круг не попал. Решение — не «обновить список»
(через неделю он снова протухнет), а удалить его: после R5 разрешённых имён нет вообще, стражу список
не нужен. Побочно вылечена BOM-нетерпимость парсера путей (PS 5.1 `Set-Content -Encoding utf8` пишет
BOM, и страж печатал «в базе есть <BOM># …»).

**R3 — продолжение (R629, 0.7.433).** `internal/balancer/proxy.go` 2422 → **1609** (непустых 1786 → 1478);
пять чистых переносов: `proxy_new.go` (352, сборка `Proxy`), `recent_clients.go` (172), `status_writer.go`
(129, `statusRecorder` + `clientAlreadyCommitted`), `parse_request_body.go` (108), `placement_autodistribute.go`
(98). Счётчик: `файлов ≥1500 строк` **21 → 20**, список-база стража размера уменьшен в том же коммите.
`ServeHTTP` (938 строк) намеренно НЕ тронут: это одна последовательная процедура с семью общими локальными
переменными, «вынос стадии» был бы правкой поведения под видом читаемости — он остаётся в счётчике
«функций >300». Импорты подбирал `gopls imports -w` по одному файлу (по нескольким сразу нельзя), лишние
в старом файле — циклом по сообщениям компилятора.

**R3 — продолжение (R630, 0.7.434).** `internal/balancer/llamacpp_transport.go` 2964 → **686**: разложен на
пять слоёв — `llamacpp_stream_upstream.go` (544, чтение апстрима), `llamacpp_stream_translate.go` (769, перевод
кадров + буфер tool-call), `llamacpp_stream_finalize.go` (574, хвост стрима), `llamacpp_upstream_call.go` (429,
вызов апстрима). Функций в пяти файлах ровно 42 (столько же, сколько было) — перенос только по границам
функций с doc-комментариями. Счётчик `файлов ≥1500 строк` **20 → 19**, список-база уменьшен в том же коммите.

**R3 — продолжение (R631, 0.7.435).** `internal/balancer/nctx_reload_handlers.go` 2316 → **719**: разложен на
пять файлов — `nctx_reload_preflight.go` (784, preflight «вырастет ли окно»), `nctx_reload_cache.go` (223, кэш
наблюдённого окна), `nctx_reload_execute.go` (340, исполнение), `nctx_reload_metrics.go` (275, метрики окна).
Функций в пяти файлах 30 (столько же). Счётчик `файлов ≥1500` **19 → 18**. Сторож R301 переведён на helper
`nctx_reload_sources_r631_test.go`; в базе сторожа R493 учтён переезд решения (3 → 2+1, сумма не изменилась).

**R3 — продолжение (R632, 0.7.436).** `internal/balancer/model_management.go` 2172 → **253**: шесть файлов —
`model_management_list.go` (401), `model_management_ops.go` (427), `model_management_load.go` (321),
`model_management_llamacpp.go` (589, включая `executeLlamaCppLoadOnce` 447 строк), `model_management_transport.go`
(211). Функций 39 (столько же). Счётчик `файлов ≥1500` **18 → 17**. Сторож R119 переведён на helper
`model_management_sources_r632_test.go`.

**R3 — продолжение (R633, 0.7.437).** `internal/balancer/image_resources.go` 2001 → **466**: семь файлов —
`image_resources_lifecycle.go` (366), `_fetch.go` (126), `_gate.go` (275), `_vram.go` (320), `_locks.go` (251),
`_snapshot.go` (233). Функций 61 (столько же), типов 10. Счётчик `файлов ≥1500` **17 → 16**. Урок процесса:
первая попытка разбиения сместила границы на один — сборка осталась зелёной (функции продублировались),
ошибку поймал замер строк, а не тест; правило «после разбиения сверяй число функций и размеры файлов».

**R3 — продолжение (R634, 0.7.438).** `internal/balancer/nctx_reload.go` 1865 → **442**: семь файлов —
`nctx_reload_coordinator.go` (286), `_decide.go` (423), `_client.go` (73), `_do.go` (363, `DoReload`),
`_stats.go` (162), `_progress.go` (152). Функций 53, типов 15 (без потерь). Счётчик `файлов ≥1500` **16 → 15**.
Сторож R505 переведён на helper `nctx_coordinator_sources_r634_test.go`, отдельный от R631-хелпера
(разные слои). Мутация «снять запись SetLastKnownNCtx» краснит сторож.

**R3 — продолжение (R635, 0.7.439).** `internal/balancer/preflight_nctx.go` 1601 → **583**: четыре файла —
`preflight_nctx_decide.go` (393), `_reject.go` (252), `_run.go` (391, `RunPreflight`). Функций 23 (столько же).
Счётчик `файлов ≥1500` **15 → 14**. Сторожа (`preflight_ceiling_r323`, `nctx_ceiling_r265`, `window_units_r505`)
правок не потребовали: они ищут разбор запроса, который остался в первом файле.

**R3 — продолжение (R636, 0.7.440).** `internal/balancer/autotune.go` 1666 → **327**: десять файлов —
`autotune_analyze.go` (188), `_report.go` (78), `_gate.go` (111), `_circuit.go` (62), `_tracker.go` (112),
`_reload.go` (371), `_apply.go` (204), `_apply_result.go` (79), `_plan.go` (188). Функций 37, типов 11 (без
потерь). Счётчик `файлов ≥1500` **14 → 13**. Правило, добавленное в процесс: «число имён = число границ + 1»,
и при разбиении печатается каждая пара (файл, диапазон строк) — первый прогон потерял один блок именно из-за
сдвига имён при зелёной сборке.

**R3 — продолжение (R637, 0.7.441).** `internal/balancer/image_tool.go` 1671 → **250**: двенадцать файлов по темам
(`image_tool_target/schema/calls/resolve/validate/load/generate/result/catalog/helpers/url.go`). Функций 53, типов 5
(без потерь). Счётчик `файлов ≥1500` **13 → 12**. Сторож R119 переведён на helper `image_tool_sources_r637_test.go`.
Урок про мутации: она обязана компилироваться и не оставлять искомую строку даже в комментарии — две первых
попытки «доказывали» ложное.

**R3 — продолжение (R638, 0.7.442).** `cmd/cppworker/tool_calls.go` 1988 → **539**: девять файлов по форматам и
слоям (`tools_prompt_cache.go` 225, `tool_calls_hermes.go` 136, `tool_calls_model_syntax.go` 452,
`tool_calls_mistral.go` 171, `tool_calls_normalize.go` 120, `tools_system_prompt.go` 178, `tool_calls_recover.go` 177,
`tool_calls_repair_tail.go` 38). Функций 42, типов 2 (без потерь). Счётчик `файлов ≥1500` **12 → 11**. Из Go
остался один файл ≥1500 — `internal/api/handlers_cluster_models.go`; дальше только JS/C (R6).

**R3 — продолжение и конец Go-части (R639, 0.7.443).** `internal/api/handlers_cluster_models.go` 1628 → **332**:
шесть файлов (`_reload.go` 225, `_result.go` 361, `_state.go` 255, `_bulk.go` 83, `_restore.go` 402). Функций 30,
типов 11 (без потерь). Счётчик `файлов ≥1500` **11 → 10**. Сторожа R119/R300/R371 переведены на helper
`cluster_models_sources_r639_test.go`; мутация `if false && opResult.StillLoading` краснит R300.

**В Go файлов ≥1500 больше НЕТ.** Остаток 10: 2 словаря i18n (данные, план их не трогает), 2 файла C-моста
(`bridge.c`, `bridge.go` — вне R3/R6), 6 WebUI-модулей (R6): `gguf-renderer-detail-render.js` 3850, `app.js` 3673,
`renderers.js` 2829, `reference-page.js` 2143, `ui-renderer.js` 1831, `image-profiles.js` 1712. При разборе всех
шести счётчик дойдёт до 4 — критерий §5 «≤3» станет недостижим без решения владельца об исключении данных/C.

**R6 — начат (R640, 0.7.444).** `webui/js/modules/image-profiles.js` 1830 → **1251**: чистый слой (контракт ролей
и семейств, `profileToForm`/`formToProfile`/`validateForm`, нормализация ответов API) вынесен в
`webui/js/modules/image-profile-pure.js` (644). Связь — `window.ImageProfilePure` + локальные ссылки в редакторе,
громкая ошибка при отсутствии слоя; порядок `<script>` проверяется стражем
`webui/tests/image-profile-pure-split-r640.test.js` (7 проверок, две мутации красные). Счётчик `файлов ≥1500`
**10 → 9**. Первая версия выноса захватила адреса API (`PATH_*`) — `image-profiles-dom.test.js` покраснел
(«нет запроса списка профилей»), адреса возвращены в редактор. WebUI: **124/124**.

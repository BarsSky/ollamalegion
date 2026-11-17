# Справка/рекомендации + шаблоны: реализация серверной части

**Статус.** Серверная часть реализована, протестирована и проверена локально.
Ничего не коммитилось и не пушилось. Живой стенд не использовался — все тесты
на `httptest` + `t.TempDir()`. Комментарии в коде и этот отчёт — по-русски.

**Что читать вместе с этим отчётом:** проектный контракт
`docs/plans/2026-11-17-reference-templates-design.md` (§1–§3, §5, §7) — он
реализован; в нём теперь §6 = РЕШЕНИЯ человека и §9 = «что реализовано» с явным
перечнем отступлений от первоначального текста.

---

## 1. Что сделано (по пунктам задания)

| Пункт | Состояние | Где |
|---|---|---|
| **A. Пакет хранения, типы, файл, путь+env, атомарная запись, чтение при старте, битый JSON → warn+пусто** | сделано | `internal/reference/` (новый пакет), инициализация — `cmd/balancer/main.go:293-296` |
| **B. Валидация без второго валидатора, id-regex, revision 409** | сделано | `internal/reference/validate.go`, `internal/api/handlers_reference.go:210-240`, ревизия — `internal/reference/store.go:255-301` |
| **C. HTTP-ручки по §3 (11 методов на 8 путях)** | сделано | `internal/api/handlers_reference.go`, `internal/api/handlers_reference_apply.go`, регистрация — `internal/api/routes.go:527-530` |
| **D. Применение только через существующие функции профилей** | сделано | `internal/api/handlers_reference_apply.go:526-548` (вызовы `s.upsertModelProfile` / `s.applyModelProfile`) |
| **E. Тесты (1)–(8)** | сделано (8 из 8), плюс бонусный тест 202-проксирования | `internal/api/handlers_reference_test.go` (9 тестов), `internal/reference/store_test.go` (5 тестов) |
| **F. Мутационный прогон 3 тестов** | сделано (3 из 3: красный → зелёный) | §4 этого отчёта |
| **G. Docs: §6 = решения, раздел «что реализовано», отступления** | сделано | `docs/plans/2026-11-17-reference-templates-design.md` §6 (строки 845-903) и §9 (строки 969-1044) |

### 1.1 Почему пакет `internal/reference`, а не код внутри `internal/api`

Хранилище не знает ни про HTTP, ни про рантайм профилей: его зависимости —
только `pkg/types` (имена полей профиля) и `pkg/logger`. Всё, что касается
«применить к профилю», живёт в `internal/api` и вызывает существующие функции
профилей. Обратное направление (хранилище → `internal/api`) запрещено, иначе
`internal/api` и `internal/reference` образовали бы цикл импортов, а вместе с ним
— соблазн завести второй путь сохранения профиля (§7 п.2 документа).
Дополнительный выигрыш: инварианты файла (путь, атомарность, битый JSON, сид)
тестируются без поднятия HTTP-сервера (`internal/reference/store_test.go`).

---

## 2. Файлы

### 2.1 Новые

| Файл | Строки | Содержимое |
|---|---|---|
| `internal/reference/reference.go` | 24-31 | `SchemaVersion=1`, `FileName="model-reference.json"`, `EnvPathOverride="LB_MODEL_REFERENCE_PATH"` |
| | 34-46 | `Doc` — `{version, recommendations, templates}` |
| | 52-64 | `AppliesTo` — glob/тип бэкенда/квантизация/границы VRAM; **все поля опциональны** (решение №1) |
| | 79-90 | `Param` — `{name, value, unit, comment}`; `unit` — только подпись (решение №5) |
| | 92-119 | `Recommendation` — `id/title/appliesTo/params/comment/sources/author/createdAt/updatedAt/revision` |
| | 121-147 | `Template` — `params` = **плоский объект в форме тела PUT профиля**; `version` (для человека) ≠ `revision` (блокировка) |
| `internal/reference/store.go` | 34-43 | `ResolvePath(statePath)`: env `LB_MODEL_REFERENCE_PATH` → каталог `LB_STATE_PATH` → `data/` |
| | 48-55 | `Store` — один `sync.RWMutex` на весь документ |
| | 100-117 | `Load()` — чтение при старте; сид только в ветке «файла нет» |
| | 131-156 | `applyFile` — битый JSON / неизвестная `version` → warn + работаем без данных |
| | 158-180 | `seed()` — однократная запись сида |
| | 255-301 | `PutRecommendation` — upsert, `revision` растёт, `createdAt` сохраняется, `RevisionConflictError` |
| | 319-365 | `PutTemplate` — то же для шаблонов |
| | 394-422 | `saveLocked` — атомарно: `MarshalIndent` → `.tmp` `0644` → `Rename`, каталог `0755` |
| `internal/reference/validate.go` | 28-39 | `idPattern` = `^[a-z0-9][a-z0-9._-]{2,63}$`, `ValidateID` |
| | 43-92 | `KnownParamNames`/`IsKnownParamName` — **reflection по json-тегам `types.LlamaCppModelProfile`** (второго списка имён нет) |
| | 94-103 | `UnknownParamNames` |
| | 134-170 | `ParamsToObject` / `DecodeParamsObject` — `params[]` ⇄ объект формы тела PUT профиля |
| `internal/reference/seed.go` | 25 | `docsTuningReference = "docs/operations/model-tuning-reference.ru.md"` |
| | 63-86 | `tuning-baseline-32k` — общая справка из таблицы ориентиров §1 (**без** `appliesTo`) |
| | 90-122 | `qwen3-8-27b-ud-q4-a10-24gb` (§3.1) |
| | 125-154 | `qwen3-4b-8gb-agent` (§3.2) |
| | 157-177 | `qwen3-30b-class` (§3.3, без `appliesTo`) |
| | 180-204 | `gemma-4-reasoning-tags` (§3.4) |
| | 207-234 | `deepseek-r1-distill` (§3.5) |
| | 237-259 | `small-models-32k-f16` (§3.6, без `appliesTo`) |
| `internal/api/handlers_reference.go` | 73-85 | `referenceStoreForRequest` — нет хранилища → 503 `store_unavailable` |
| | 102-124 | `decodeReferenceBody` — `DisallowUnknownFields`, как у PUT профиля |
| | 134-141 | `referenceProfileBaseline` — опорное окно 8192 **только для проверки границ** |
| | 210-240 | `prepareReferenceParams` — **единственная** проверка набора: имена → типы → границы `validateModelProfile` |
| | 256-297 | диспетчер рекомендаций |
| | 323-387 | `putReferenceRecommendation` (upsert + 409) |
| | 408-477 | `recommendationToTemplate` (201, 409 при занятом id) |
| | 479-523 | диспетчер шаблонов (включая `preview`/`apply`) |
| | 543-557 | `referenceTemplateUsage` — `usage.usedByProfiles` (совпадение значений) |
| | 614-666 | `putReferenceTemplate` |
| `internal/api/handlers_reference_apply.go` | 40-46 | режимы `profile-only` / `profile+reload` |
| | 146-175 | `previewReferenceTemplate` — ничего не пишет, reload не запускает |
| | 177-219 | `buildReferencePreview` — мерж + `validateModelProfile` + diff + targets + memory |
| | 271-292 | `referenceDiff` — только `name`+`value` (`unit` не участвует, решение №5) |
| | 294-327 | `referenceTargets` — проекция состава бэкендов, без нового эндпоинта (§3.5) |
| | 329-436 | `applyReferenceTemplate` — режим, `model`, `revision`, `backend_required`, dryRun, память, делегирование |
| | 457-469 | `referenceNormalizeModelName` — `.gguf` + `balancer.NormalizeModelProfileKey` |
| | 472-524 | `referenceCaptureWriter` + синтетический запрос |
| | 526-548 | `referenceRunProfileUpsert` / `referenceRunProfileApply` — **вызовы существующих обработчиков** |
| | 549-625 | переупаковка ответа 200/202 (202 проксируется как есть) |
| | 627-660 | `writeReferenceRejected` — 4xx/422 существующего пути в контракт шаблонов |

### 2.2 Изменённые

| Файл | Что изменено |
|---|---|
| `internal/api/handlers.go:83-88` | поле `referenceStore *reference.Store` |
| `internal/api/handlers.go:103-116` | `SetReferenceStore` / `ReferenceStore` |
| `internal/api/handlers.go:14` | импорт `internal/reference` |
| `internal/api/routes.go:514-530` | 4 маршрута `/api/v1/reference/...` с `AuthMiddleware(RateLimitMiddleware(...))` |
| `cmd/balancer/main.go:22` | импорт `internal/reference` |
| `cmd/balancer/main.go:284-296` | `ResolvePath` → `NewStore` → `Load()` → `apiServer.SetReferenceStore(...)` |

### 2.3 Зарегистрированные ручки

| Метод, путь | Ответ | Ошибки |
|---|---|---|
| `GET /api/v1/reference/recommendations` | `{items, total, schemaVersion}` | 503 `store_unavailable` |
| `GET /api/v1/reference/recommendations/{id}` | `{recommendation, relatedTemplates:[{id,name}]}` | 404 `recommendation_not_found` |
| `PUT /api/v1/reference/recommendations/{id}` | `{status:"ok", recommendation}` (+`warning`) | 400 `invalid_id`/`invalid_json`/`title_required`/`params_required`/`duplicate_param`/`unknown_param`/`invalid_value`, 409 `revision_conflict` |
| `DELETE /api/v1/reference/recommendations/{id}` | `{status:"ok", id}` | 404 |
| `POST /api/v1/reference/recommendations/{id}/to-template` | 201 `{status:"created", template}` | 400, 404, 409 `template_id_taken` |
| `GET /api/v1/reference/templates` | `{items, total, schemaVersion}` | 503 |
| `GET /api/v1/reference/templates/{id}` | `{template, usage:{usedByProfiles}}` | 404 `template_not_found` |
| `PUT /api/v1/reference/templates/{id}` | `{status:"ok", template}` (+`warning`) | 400 `invalid_params`/`unknown_param`/… , 409 |
| `DELETE /api/v1/reference/templates/{id}` | `{status:"ok", id}` | 404 |
| `POST /api/v1/reference/templates/{id}/preview` | `{ok, model, templateId, templateVersion, profile, valid:{ok,violations}, diff, targets, memory, warnings}` | 400 `model_required`/`invalid_params`, 404 |
| `POST /api/v1/reference/templates/{id}/apply` | 200 `{status:"applied", model, templateId, templateVersion, mode, profile, backends}`; 202 (занятый бэкенд) — **как есть** из async-пути | 400 `invalid_mode`/`model_required`/`backend_required`/`backend_not_llama_cpp`/`invalid_params`, 404 `template_not_found`/`backend_not_found`, 409 `revision_conflict`, 422 `profile_does_not_fit_memory`, 502 `reload_failed` |

---

## 3. Тесты

Все — без живого стенда: `httptest` + `t.TempDir()`. Прокси создаётся
`balancer.NewProxy` с `statePath` ВНУТРИ временного каталога — иначе fallback
персиста профилей (`internal/balancer/profiles_persistence.go:52-58`) писал бы
`data/profiles.json` в каталог репозитория на каждом прогоне
(`internal/api/handlers_reference_test.go:50-78`).

| # | Тест | Файл:строка | Что доказывает |
|---|---|---|---|
| 1 | `TestReference_NoFileEmptyState` | `handlers_reference_test.go:156` | файла нет → 200 и ПУСТЫЕ списки, не 404/500 |
| 2 | `TestReference_PutCreatesAndBumpsRevision` | `:202` | PUT создаёт (revision 1) → повторный PUT (2) → PUT с revision 1 → 409 `revision_conflict` {current:2, sent:1}; PUT с актуальной ревизией → 3 |
| 3 | `TestReference_UnknownParamRejected` | `:256` | `params[].name="nCtx"` → 400 `unknown_param`, `field="nCtx"`, `allowed` содержит `contextLength`; для шаблона — тот же код |
| 4 | `TestReference_ValueOutOfBoundsRejected` | `:304` | `parallel=12` и `contextLength=1310720` → 400 `invalid_value` с именем поля |
| 5 | `TestReference_SeedOnlyWhenFileAbsent` | `:337` | файла нет → сид (author `docs-import`, `sources[0].doc`), `store.Seeded()==true`; существующий файл — `Load()` вернул `false`, байты файла не изменились, запись оператора на месте, записей сида нет |
| 6 | `TestReference_ApplyRequiresExplicitBackend` | `:476` | `mode="profile+reload"` без `backendId` → 400 `backend_required` + текст «задевает чужие…» + профиль НЕ сохранён |
| 7 | `TestReference_ApplyProfileOnlyDoesNotReload` | `:506` | фейковый llama.cpp-бэкенд считает `POST /api/models/reload`: profile-only → счётчик 0, профиль сохранён; profile+reload с `backendId="fake-gpu"` → счётчик ≥ 1, в ответе `backends[0].backendId="fake-gpu"` |
| 8 | `TestReference_PreviewWritesNothing` | `:628` | preview отдаёт diff (`contextLength` 16384→65536) и violations (`parallel`), байты файла хранилища не изменились, профиль модели не изменился |
| + | `TestReference_ApplyBusyBackendForwardsAsync` | `:567` | занятый бэкенд (модель «загружена», active-queries=2) → 202 из существующего async-пути проксируется как есть: в теле `applyId` и `progressUrl` существующего профильного SSE |

Хранилище (`internal/reference/store_test.go`): `TestResolvePath` (env/каталог
state/`data/`), `TestLoad_CorruptJSONIsNotFatal`, `TestLoad_UnknownVersionIgnored`,
`TestSave_AtomicAndPermissions` (нет `.tmp`, версия в файле, права `0644` — только
на POSIX), `TestStore_PutKeepsCreatedAtAndBumpsRevision`.

Запуск:

```
go test -tags llama_stub ./internal/api/... -run 'TestReference_' -count=1 -v
=== RUN   TestReference_NoFileEmptyState
--- PASS: TestReference_NoFileEmptyState (0.01s)
=== RUN   TestReference_PutCreatesAndBumpsRevision
--- PASS: TestReference_PutCreatesAndBumpsRevision (0.03s)
=== RUN   TestReference_UnknownParamRejected
--- PASS: TestReference_UnknownParamRejected (0.01s)
=== RUN   TestReference_ValueOutOfBoundsRejected
    --- PASS: TestReference_ValueOutOfBoundsRejected/parallel_too_big (0.00s)
    --- PASS: TestReference_ValueOutOfBoundsRejected/context_too_big (0.00s)
--- PASS: TestReference_ValueOutOfBoundsRejected (0.01s)
=== RUN   TestReference_SeedOnlyWhenFileAbsent
    --- PASS: TestReference_SeedOnlyWhenFileAbsent/absent_file_is_seeded (0.01s)
    --- PASS: TestReference_SeedOnlyWhenFileAbsent/existing_file_not_overwritten (0.02s)
--- PASS: TestReference_SeedOnlyWhenFileAbsent (0.02s)
=== RUN   TestReference_ApplyRequiresExplicitBackend
--- PASS: TestReference_ApplyRequiresExplicitBackend (0.01s)
=== RUN   TestReference_ApplyProfileOnlyDoesNotReload
--- PASS: TestReference_ApplyProfileOnlyDoesNotReload (0.02s)
=== RUN   TestReference_ApplyBusyBackendForwardsAsync
--- PASS: TestReference_ApplyBusyBackendForwardsAsync (0.01s)
=== RUN   TestReference_PreviewWritesNothing
--- PASS: TestReference_PreviewWritesNothing (0.01s)
PASS
ok  	ollama-loadbalancer/internal/api	0.404s
```

---

## 4. Мутационный прогон (обязательный пункт F)

Каждая мутация — удаление самой проверки целиком (одна строка или один блок),
чтобы код продолжал компилироваться. После каждой — возврат и зелёный прогон.

### 4.1 `apply` без `backendId`: удалить ответ 400 `backend_required`

Файл `internal/api/handlers_reference_apply.go`, удалён блок:

```go
	if mode == referenceModeProfileReload && backendID == "" {
		s.writeJSON(w, http.StatusBadRequest, referenceErrorBody("backend_required",
			"выберите бэкенд, где загружена модель: применение на все сразу задевает чужие/недоступные узлы",
			map[string]interface{}{"model": model}))
		return
	}
```

```
go test -tags llama_stub ./internal/api/ -run 'TestReference_ApplyRequiresExplicitBackend' -count=1 -v
=== RUN   TestReference_ApplyRequiresExplicitBackend
    handlers_reference_test.go:487: HTTP 404, ожидали 400 ({
          "backendId": "",
          "error": "backend_not_found",
          "message": "бэкенд \"\" не найден среди зарегистрированных"
        }
        )
--- FAIL: TestReference_ApplyRequiresExplicitBackend (0.01s)
FAIL
FAIL	ollama-loadbalancer/internal/api	0.298s
```

После возврата:

```
go test -tags llama_stub ./internal/api/ -run 'TestReference_ApplyRequiresExplicitBackend' -count=1
ok  	ollama-loadbalancer/internal/api	0.301s
```

### 4.2 `unknown_param`: удалить проверку имён в `prepareReferenceParams`

Файл `internal/api/handlers_reference.go`, удалён блок:

```go
	unknown := reference.UnknownParamNames(reference.ObjectNames(obj))
	if len(unknown) > 0 {
		return types.LlamaCppModelProfile{}, referenceErrorBody("unknown_param", ...)
	}
```

```
go test -tags llama_stub ./internal/api/ -run 'TestReference_UnknownParamRejected' -count=1 -v
=== RUN   TestReference_UnknownParamRejected
    handlers_reference_test.go:266: error = "invalid_value", ожидали unknown_param ({
          "error": "invalid_value",
          "field": "",
          "got": null,
          "message": "json: unknown field \"nCtx\""
        }
        )
--- FAIL: TestReference_UnknownParamRejected (0.01s)
FAIL
FAIL	ollama-loadbalancer/internal/api	0.464s
```

После возврата:

```
go test -tags llama_stub ./internal/api/ -run 'TestReference_UnknownParamRejected' -count=1
ok  	ollama-loadbalancer/internal/api	0.336s
```

Заодно видно, зачем нужна отдельная проверка имён: без неё код ответа и текст
ошибки меняются (`invalid_value` вместо `unknown_param`), и клиент не получает
список `allowed`.

### 4.3 Сид: удалить `return false` в ветке «файл есть»

Файл `internal/reference/store.go`, `Load()`; удалена строка `return false`
после `s.applyFile(data)` — то есть существующий файл читается, а затем
**проваливается в сид**.

```
go test -tags llama_stub ./internal/api/ -run 'TestReference_SeedOnlyWhenFileAbsent' -count=1 -v
=== RUN   TestReference_SeedOnlyWhenFileAbsent
=== RUN   TestReference_SeedOnlyWhenFileAbsent/absent_file_is_seeded
=== RUN   TestReference_SeedOnlyWhenFileAbsent/existing_file_not_overwritten
    handlers_reference_test.go:393: Load выполнил сид поверх существующего файла — это запрещено решением №4
--- FAIL: TestReference_SeedOnlyWhenFileAbsent (0.02s)
    --- PASS: TestReference_SeedOnlyWhenFileAbsent/absent_file_is_seeded (0.01s)
    --- FAIL: TestReference_SeedOnlyWhenFileAbsent/existing_file_not_overwritten (0.01s)
FAIL
FAIL	ollama-loadbalancer/internal/api	0.367s
```

После возврата:

```
go test -tags llama_stub ./internal/api/ -run 'TestReference_' -count=1
ok  	ollama-loadbalancer/internal/api	0.478s
```

---

## 5. Проверки перед сдачей (команды и результат)

**ВАЖНО про `-tags llama_stub`.** На этой машине `go build ./...` и
`go test ./internal/api/...` БЕЗ тега падают на этапе cgo-сборки
`c/bridge`, и это состояние БЫЛО ДО моих изменений (проверено на чистом дереве):

```
go build ./...            → # ollama-loadbalancer/c/bridge
                            bridge_internal.h:22:10: fatal error: llama.h: No such file or directory
go test ./internal/api/   → FAIL ollama-loadbalancer/internal/api [build failed] (та же ошибка)
```

Поэтому все Go-команды ниже выполнены с тегом `llama_stub` — это ровно тот тег,
который использует CI-линт (`.golangci.yml:18-19`).

| Команда | Результат |
|---|---|
| `go build -tags llama_stub ./...` | `exit=0` |
| `go vet -tags llama_stub ./internal/api/... ./internal/reference/...` | `exit=0`, замечаний нет |
| `go test -tags llama_stub ./internal/reference/... -count=1` | `ok ollama-loadbalancer/internal/reference 0.946s` |
| `go test -tags llama_stub ./internal/api/... -run 'TestReference_' -count=1 -v` | все 8 тестов PASS (вывод в §3) |
| `go test -tags llama_stub ./internal/api/... -count=1` (полный пакет) | `ok ollama-loadbalancer/internal/api 62.422s` — падений нет |
| `gofmt -l` по всем затронутым файлам | пусто (после форматирования двух файлов) |

`golangci-lint` в окружении не установлен (`Get-Command golangci-lint` → ничего),
поэтому проверки `fieldalignment`/`misspell` из CI-конфига локально **не
прогонялись** — см. §7.

---

## 6. Что осталось / не сделано

1. **Живой стенд не проверялся вообще.** Задача запрещает изменения на стенде, а
   мутирующие ручки (PUT/apply) туда и не отправлялись. См. §7.
2. **UI не делался** (по заданию — другой агент): `webui/` не тронут, i18n-ключи
   `nav.reference`/`reference.*` не добавлялись. Контракт ответов для UI — §2.3.
3. **`CHANGELOG.md` не правился** (по заданию). Список для ведущего агента — §8.
4. **Отбор записей по `appliesTo` на сервере не реализован**: сервер отдаёт все
   записи, подбор/подсветка — дело UI (§6.1, §5 риска 10 в документе). Фильтра
   по `modelPattern` в API нет намеренно: он превратил бы «справку» в скрытый
   источник значений.
5. **Экспорт/импорт файла между стендами** не делался (открытый вопрос §5.2):
   файл лежит в томе `/app/data`, этого достаточно.
6. **`confirmNonFeasible` не обходит гейт памяти для `mode:"profile-only"`** — и
   не должен: гейт живёт внутри `upsertModelProfile`
   (`handlers_cppworker_profiles.go:365-382`), а второго пути сохранения профиля
   задача запрещает (§7 п.2). Для `profile+reload` флаг работает (проверка памяти
   в этом пути своя — `handlers_reference_apply.go:407-421`).
7. **Сид — снимок доков на момент реализации.** Параллельный агент позже дописал
   в `docs/operations/model-tuning-reference.ru.md` §1.1 (инвариант
   `contextLength ≤ contextLengthMax`), §3.0 (сводная таблица) и §3.7 (профиль
   Cline) — этих записей в сиде нет. Это ожидаемо: сид однократный и повторно
   доки не перечитываются (решение №4). Числа §3.1–3.6 не менялись, поэтому
   существующие записи сида актуальны; §3.7 при необходимости заводится через
   `PUT …/recommendations/{id}` или `to-template`.
8. **Пять записей сида без `appliesTo`** (`tuning-baseline-32k`, `qwen3-30b-class`,
   `small-models-32k-f16` и др.) — это «общая справка» по решению №1, а не
   забытое поле.

---

## 7. Что не проверено живьём

| Не проверено | Почему | Чем рискованно |
|---|---|---|
| **Реальный apply с запущенным cppworker** (`force:true`, `gpuLayers`, токен) | живой стенд запрещён заданием; тест 7 использует фейковый cppworker, который только считает reload'ы | путь делегируется существующим обработчикам, но фактическая перезагрузка модели и её окно после reload не наблюдались |
| **Настоящий `GET /api/models/effective` (вердикт памяти)** | cppworker в тестах фейковый | блок `memory` в preview на живом стенде не сверялся |
| **Живой SSE-прогресс async-применения** | 202-проксирование проверено фейковым бэкендом (`TestReference_ApplyBusyBackendForwardsAsync`), но реальный поток `apply/progress`/`apply/status` не наблюдался | UI может получить applyId и не увидеть прогресс, если существующий SSE сломан на стенде — это чужой код, но связка не проверена |
| **Модель загружена на ДВУХ бэкендах** (`targets` с несколькими `loaded:true`) | на тестовом прокси нет реальных метрик загрузки | ветка «применить к одному из двух узлов» не исполнялась ни разу (в документе §5.2 это тоже помечено как непроверенное) |
| **`usage.usedByProfiles` на 21 реальном профиле** | профилей стенда в тестах нет | сравнение значений проверено только на синтетическом профиле |
| **golangci-lint (fieldalignment/misspell)** | линтер не установлен | CI-джоба `lint` может дать замечания по раскладке полей новых структур |
| **Поведение с несколькими балансерами** | не разворачивалось | файл не синхронизируется между инстансами (§5.1 п.5 документа) |
| **Права 0644/0755 на POSIX-хосте** | тесты шли под Windows (там Go отдаёт 0666) | тест проверяет права только на не-Windows; на Linux-стенде поведение то же, что у `profiles.json` (`profiles_persistence.go:193`) |
| **`LB_MODEL_REFERENCE_PATH` в compose** | переменная не добавлялась в `deployments/*` (не просили) | по умолчанию путь = каталог `state.json`, то есть `/app/data/model-reference.json` на существующем томе — должно работать без правок compose |

---

## 8. Что стоит внести в `CHANGELOG.md` (делает ведущий агент)

- `feat(reference)`: новый пакет `internal/reference` — файловое хранилище
  `model-reference.json` рядом со `state.json` (override
  `LB_MODEL_REFERENCE_PATH`), атомарная запись tmp+rename, чтение при старте,
  битый JSON/неизвестная версия → warn + пусто, отсутствие файла — валидное
  пустое состояние.
- `feat(api)`: ручки `GET/PUT/DELETE /api/v1/reference/recommendations[/{id}]`,
  `POST …/recommendations/{id}/to-template`, `GET/PUT/DELETE
  /api/v1/reference/templates[/{id}]`, `POST …/templates/{id}/preview`,
  `POST …/templates/{id}/apply` (под `AuthMiddleware`+`RateLimitMiddleware`:
  viewer читает, admin правит).
- `feat(api)`: `preview` — dry-run (diff + violations + targets + вердикт памяти),
  ничего не сохраняет и reload не запускает.
- `feat(api)`: `apply` шаблона переиспользует существующие пути профилей
  (`PUT`/`POST …/apply`: `force:true`, `gpuLayers`, токен cppworker, `?backend=`,
  busy→202); `mode:"profile+reload"` требует явный `backendId` (400
  `backend_required`), «модель не загружена на узле» — `skipped`, а не отказ.
- `feat(reference)`: однократный сид 7 записей из
  `docs/operations/model-tuning-reference.ru.md` (`author:"docs-import"`,
  `sources`), только при отсутствии файла; `notes` профилей не тронуты.
- `test`: 8 тестов reference-API (`internal/api/handlers_reference_test.go`) и
  5 тестов хранилища (`internal/reference/store_test.go`), без живого стенда.
- `docs`: `2026-11-17-reference-templates-design.md` — §6 переписан под решения
  человека, добавлен §9 «что реализовано» с перечнем отступлений.

---

## 9. Границы (что НЕ трогал)

- `webui/` — не изменялся ни один файл (страницу делает другой агент).
- `internal/balancer/` — не изменялся (там работает агент по правилам роста
  окна). Правок в `balancer` для этой задачи **не потребовалось**: нормализация
  имени взята существующая (`balancer.NormalizeModelProfileKey`), загрузка
  профилей и reload — существующие функции.
- `config.json`, `deployments/*`, `/app/config` — не изменялись.
- `CHANGELOG.md` — не изменялся (список — §8).
- Секретов/PII в коде и отчёте нет: токены не запрашивались и не копировались.

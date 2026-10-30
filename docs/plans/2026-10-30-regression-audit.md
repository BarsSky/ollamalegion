# Аудит противоречий и причин регрессии после R320/R321

> **Дата:** 2026-10-30 · **Режим:** только чтение (ни один файл кода не изменён)
> **Предмет:** живой стенд, клиенты OpenWebUI (Ollama-поверхность) и Cline,
> после выката R320 (done_reason на tools-пути) и R321/R321.2 (слой
> контрактного завершения в балансере).
> **Метод:** чтение исходников и документов проекта; каждое утверждение —
> со ссылкой `файл:строка`. Где доказательства в коде нет (поведение внешнего
> клиента, живые числа конкретного стенда) — прямо написано «не проверено».

## 0. Резюме в одну таблицу

| # | Жалоба оператора | Что найдено | Класс | Приоритет |
|---|---|---|---|---|
| 1 | Cline не ловит «COMPLETE» | Терминальных кадров и причин завершения в коде **не менее 50** мест присваивания; контракт Ollama допускает 4 значения (`stop/length/load/unload`), код отдаёт ещё `tool_calls/error/cancelled/truncated`; на нативном пути при включённом авто-продолжении возможны **ДВА** терминальных `done:true`; `done_reason="length"` ставится эвристикой, дающей ложное срабатывание на нормальном конце хода | двойное завершение + ложная причина | **P0** |
| 2 | «Потерянный» запрос: не виден на мониторе, но «крутится» | Запрос может быть отклонён **до** admission-очереди тремя разными guard'ами (R288/R299/R301); строки `pending` в панели очереди больше никто не пишет (`qm.pending` мёртв с R78), видно только счётчик; стрим регистрируется только **после** admission | невидимость + отказ вместо очереди | **P0** |
| 3 | Окно 131072 не удовлетворило запрос при загруженных 131072 | Расхождение «суммарное окно модели» vs «окно слота», и `LB_NCTX_RELOAD_MAX_N_CTX` (env) по-прежнему работает **физическим** потолком в `DecidePreflight`, хотя R255/R267 объявили его только потолком авто-роста; после reload в кэше обновляется лишь суммарное окно, а `ContextPerSeq`/`MaxSlots` остаются старыми (до 30 с) | две несовместимые формулы окна + устаревший per-seq | **P0** |
| 4 | Не поднялась вторая копия и запрос не встал в очередь | Вторая копия создаётся только при наличии группы репликации (конфиг или placement-правило `replicated`) и только на бэкенде с `ActiveReqs==0`; очередь не работает, если `p.admission.wait<=0`, а это значение **фиксируется на старте** и не перечитывает «горячую» настройку | условия-выключатели | **P1** |
| 5 | В конце каждого ответа «Модель сгенерировала только размышления (1142 симв.)» | Текст живёт в 2 формулировках и вставляется из **6 точек**: R261-SSE, R261-non-stream, R321 в балансере и R314/R320 в воркере (3 места). R321.2 закрыл guard'ом только своё место; у остальных проверки «видимый текст уже уехал» нет | дубли правил подмены content | **P0** |

---

## А. `done_reason` и терминальные кадры (жалоба 1 — Cline и «COMPLETE»)

### А.1 Симптом

«Клиент Cline не ловит флаг COMPLETE (завершение задачи) от балансера с
бэкендом». В терминах самого проекта (см. `docs/features/cline-agent-loop.ru.md:39`):

> `done` — `done_reason`; `done:true` при нулевом выводе = клиент покажет
> «COMPLETE», ничего не дождавшись

То есть у оператора «COMPLETE» — это признак того, что клиент **закрыл ход**.
Жалоба допускает два взаимоисключающих прочтения, и в коде есть механизмы для
обоих:

* **(а) COMPLETE приходит, когда ход ещё не закончен** (клиент закрывает ход на
  пустом/ошибочном терминале);
* **(б) COMPLETE не приходит вовсе** (клиент не получает терминального кадра
  или не признаёт его завершающим).

Живой пример класса (а) уже был зафиксирован: `CHANGELOG.md:8304-8325` («от
модели прилетает COMPLETE до того, как вообще происходит потоковое общение
генерации ответа», причина — чанк `done:true, done_reason:"error",
error:"model X is not loaded"`).

### А.2 Механизм: сколько мест ставят `done_reason` и какие значения

Контракт (единственный документ-источник):
`docs/architecture/balancer-cppworker-contract.ru.md:202-227` и `:368-391` —
допустимы **ровно четыре** значения: `stop`, `length`, `load`, `unload`. Там же
`:408-410`: ответ `{"done": true, "done_reason": "error"}` на transient-состояние
**запрещён**, потому что клиент трактует его как завершённый ход.

Фактически в production-коде (без тестов) `done_reason` присваивается **не менее
чем в 50 точках** (только в `cmd/cppworker/handlers_chat.go` — 13, в
`internal/balancer/llamacpp_translate_resp.go` — 10, в
`internal/balancer/llamacpp_transport_helpers.go` — 6). Значения шире контракта:

| Значение | Где ставится (production) |
|---|---|
| `stop` | `internal/balancer/auto_continue.go:1114,1130`; `internal/balancer/llamacpp_transport.go:1981` (через `doneReason`); `internal/balancer/llamacpp_transport_helpers.go:254,286`; `internal/balancer/rpc_coordinator_dispatcher.go:473,482,663,696`; `internal/balancer/image_tool_loop_ollama.go:555`; `internal/balancer/llamacpp_translate_resp.go:750,1266`; `cmd/cppworker/handlers_chat.go:1517` (через `doneReason`, начальное `"stop"` на `:1487`); `cmd/cppworker/handlers_generate.go:598,848`; `cmd/cppworker/vision_stream_r248.go:47` |
| `length` | `cmd/cppworker/handlers_chat.go:1490` (`snapR320.Unclosed`), `:1604` (`toolsStopReasonR320`); `cmd/cppworker/handlers_generate.go:591,845`; `internal/balancer/native_done_hygiene_r321.go:119`; `internal/balancer/stream_abort_frames_r296.go:105` |
| `tool_calls` | `cmd/cppworker/handlers_chat.go:1601,2252`; `internal/balancer/llamacpp_transport.go:1975`; `internal/balancer/llamacpp_transport_helpers.go:252,284` |
| `error` | `cmd/cppworker/handlers_chat.go:1316,1444,1896,1916,2066,2112,2177`; `cmd/cppworker/handlers_generate.go:534,574,795,829`; `internal/balancer/stream_abort_frames_r296.go:103,124`; `internal/balancer/llamacpp_transport_nonstream.go:517,601`; `internal/balancer/llamacpp_translate_resp.go:278,288,389,468,934,1228`; `internal/balancer/ctx_reload_streaming.go:77`; `internal/balancer/llamacpp_handlers_inference.go:1241,1254`; `internal/balancer/llamacpp_native_path.go:240` |
| `cancelled` | `cmd/cppworker/handlers_chat.go:1301` |
| `truncated` | `internal/balancer/llamacpp_transport.go:2359` (единственное место; оставлено намеренно — `:2297-2304`) |
| из `finish_reason` апстрима «как есть» | `internal/balancer/llamacpp_translate_resp.go:460,509`; `internal/balancer/llamacpp_handlers_admin.go:136` |

**Почему это возможно.** Контракт написан для воркера, а на клиентском проводе
причин завершения распоряжается ещё и балансер: он же переводит
`finish_reason` → `done_reason`, он же правит причину в R321
(`native_done_hygiene_r321.go:119`), он же выдаёт свои терминаторы
(`llamacpp_transport.go:1977-1983`, `stream_abort_frames_r296.go:120-128`,
`auto_continue.go:1114`). Единого «реестра допустимых причин» в коде нет: есть
только текстовые запреты в комментариях.

### А.3 Где клиент может увидеть ПУСТОЙ content при `done:true`

Это главный механизм класса (а). Пустой content в терминальном кадре законен
для настоящей Ollama, но в этом проекте он создаётся **пятью разными правилами**,
и ни одно из них не знает об остальных:

1. **Балансер вычищает дубль** — `stripNativeDoneContent`
   (`internal/balancer/llamacpp_transport.go:2523-2582`, вызов `:1170-1172`):
   если текст уже уехал дельтами, `msg.content` обнуляется (`:2562`).
   Это прямое расхождение с контрактом, который требует
   `message.content` = конкатенация всех дельт
   (`docs/architecture/balancer-cppworker-contract.ru.md:391`).
2. **Воркер сам управляет content** на tools-пути: при `emittedLen > 0`
   обнуляет (`cmd/cppworker/handlers_chat.go:2319`), при `emittedLen == 0`
   кладёт полный текст (`:2324`), а при непустом `reasoningOut` — пояснение
   (`:2291-2294`). Три взаимоисключающих решения в одной ветке `else`.
3. **R321-hygiene** заменяет пустой content пояснением и меняет причину —
   `internal/balancer/native_done_hygiene_r321.go:101-119`.
4. **R314/R320 в воркере** кладёт пояснение в content — три места:
   `cmd/cppworker/handlers_chat.go:575` (non-stream), `:1509` (stream без tools),
   `:2291` (tools-путь).
5. **`writeStreamingSSEDone`** наоборот *восстанавливает* полный текст в
   терминальном кадре из `accumulatedContent`
   (`internal/balancer/llamacpp_transport_helpers.go:174-178, 208-215`) — то есть
   на одном и том же пути `/api/chat` терминальный content то обнуляется (п.1),
   то дублируется (п.5), в зависимости от того, был ли замечен usage-чанк
   (`:185-188`).

**Следствие для Cline.** Клиент, который читает финальный чанк как ответ (а
проект сам утверждает, что так делает OpenWebUI —
`cmd/cppworker/handlers_chat.go:2256-2263`), при пустом терминале получает
«пустой ответ»; клиент, склеивающий дельты, получает дубль (п.5). Оба класса
уже описаны в живых разборах: `llamacpp_transport.go:1158-1165` (потеря
единственной копии ответа) и `:1220-1225` (два `done` → «Model returned empty
response» / «Did not receive done or success response»).

### А.4 Где терминальный кадр может не дойти или прийти дважды

**(1) Два терминатора на нативном пути при включённом авто-продолжении.**

* Нативный путь обрабатывается в `internal/balancer/llamacpp_transport.go:1007-1231`
  и завершается `continue` (`:1230`) — то есть **не** выставляет
  `deferredDoneChunk` (он ставится только в SSE-ветке `:1245`).
* После цикла **безусловно** исполняется блок авто-продолжения
  `:1997` (`if streamCompleted && IsAutoContinueOnTruncationEnabled() && accumulatedPlainContent != ""`).
  Внутри — `EmitContinuationNDJSON` (`:2111`), который сам пишет
  `done:true` (`internal/balancer/auto_continue.go:1161-1163` и `:1114/1130`).
* Дубликат гасится только флагом `deferredDoneChunk` (`:2127`) — на нативном
  пути он всегда `false`, а собственный терминатор воркера клиент **уже**
  получил (`:1203`, строка ушла как есть).
  Итог: клиент видит **два** `done:true`. Ровно этот дефект описан в
  `llamacpp_transport.go:1212-1225` как причина «Model returned empty response»
  у Cline.
* Митигатор: `IsAutoContinueOnTruncationEnabled()` по умолчанию `false`
  (`docs/api/env-vars.ru.md:44`). Значит дефект латентный — но включение
  `LB_AUTO_CONTINUE_ON_TRUNCATION=1` (документированный переключатель) его
  включает.

**(2) `EmitContinuationNDJSON` теряет поля.** Терминатор собирается заново
(`auto_continue.go:1098-1148`): в нём **нет** `message.thinking`, **нет**
`message.tool_calls`, `total_duration = 0`. Для агентского клиента это означает
«ход закончился, инструментов нет» — то есть тот же класс «COMPLETE без
инструмента».

**(3) Случай «контент после `done`» — проверен, в норме не срабатывает.**
Придержанный маркерный хвост (`markerHold`) забирается **в терминальный кадр**
(`marker_hold_ndjson_r266_1.go:158-165` → `ApplyFinal`,
`final_block_r279.go:120-130`), поэтому на успешном терминале отдельного кадра
после `done` не появляется. Резервный эмиттер `heldTailChunk`
(`final_block_r279.go:140-159`, `done:false`) пишется **до** терминатора обрыва
(`llamacpp_transport.go:1830-1852` идут раньше `:2175`). Здесь нарушения
контракта «после `done` ничего нет» (`docs/features/cline-agent-loop.ru.md:211-212`)
не найдено — но зафиксировать это стоит: порядок держится на том, что
терминальный кадр всегда содержит `message`/`response`.

**(4) Терминатор не приходит вовсе.** Если апстрим закрыл поток без `done:true`,
балансер отдаёт кадры обрыва (`stream_abort_frames_r296.go:120-128`) — это
корректно. Но если запрос отклонён guard'ом до транспорта
(`proxy.go:1431-1437`, `llamacpp_handlers_inference.go:258-265`), клиент получает
**не-терминальный** отказ (`writeContextGrowthDeferred`,
`context_growth_r288.go:70-83`), и NDJSON-клиент не видит `done` вообще — для
него это «поток не закрылся».

### А.5 Что могло сломать определение COMPLETE именно в R320

`cmd/cppworker/handlers_chat.go:1599-1607`:

```go
func toolsStopReasonR320(fullOutput, prompt string, toolCalls int) string {
	if toolCalls > 0 { return "tool_calls" }
	if toolsReasoningUnclosedR320(fullOutput, prompt) { return "length" }
	return "stop"
}
```

а `toolsReasoningUnclosedR320` (`:1574-1582`) — **эвристика из двух условий**:

```go
if hasOpenThink(fullOutput) { return true }
return PromptOpensReasoning(prompt) && !hasReasoningCloseTag(fullOutput)
```

`PromptOpensReasoning` (`cmd/cppworker/reasoning_content.go:400-405`) истинно
всякий раз, когда **промпт** заканчивается внутри открытого reasoning-блока
(шаблон дописал `<think>` / `<|channel>thought`). Тогда любая модель, которая
ответила **не открывая** размышлений (и потому не содержит закрывающего тега),
формально попадает во второе условие → `done_reason="length"` на **нормальном
завершённом ответе**. Это и есть ложное срабатывание: «length» в контракте =
«достигнут `num_predict`» (`docs/architecture/balancer-cppworker-contract.ru.md:225`).

Для Cline это критично: `length` — признак усечения (`docs/features/cline-agent-loop.ru.md:67,
:132`), а не признак «задача выполнена». Отсюда наблюдаемое «COMPLETE не
приходит»: ход, который модель закончила, помечен как обрезанный.

Дополнительно: та же эвристика (`hasReasoningCloseTag`,
`hasOpenThink`, `reasoningBlockOpenAtEnd`, `reasoning_content.go:416-436`)
используется и в `Snapshot().Unclosed` для `/api/chat` без tools
(`handlers_chat.go:1487-1491`) и `/api/generate` (`handlers_generate.go:589-592,
843-846`). То есть **три** разных потребителя одной эвристики, и ни один не
различает «модель не думала» и «модель думала, но тег не пришёл».

### А.6 Минимальная правка (А)

1. **Один реестр причин.** Ввести в пакете balancer (и продублировать в
   cppworker) таблицу `allowedDoneReasons = {stop, length, tool_calls, error,
   cancelled, unload, load}` + функцию `normalizeDoneReason`, и пропустить
   через неё **все** точки из А.2 (минимум: `llamacpp_transport.go:1981`,
   `helpers:252-286`, `auto_continue.go:1114,1130`,
   `stream_abort_frames_r296.go:124`, `native_done_hygiene_r321.go:119`).
   `truncated` заменить на `error` + поле `reason` (как уже сделано в R297 для
   `stream_abort_frames_r296.go:49-52`) — один терминатор, одна причина.
2. **Запретить второй терминатор на нативном пути:** в
   `llamacpp_transport.go:1997` добавить `&& deferredDoneChunk` (или отдельный
   флаг `nativeTerminalSent`, выставляемый в `:1085-1099`). Это одна строка и
   она закрывает дубль `done:true`.
3. **Убрать ложный `length`:** в `toolsReasoningUnclosedR320`
   (`handlers_chat.go:1574-1582`) второе условие (`PromptOpensReasoning &&
   !hasReasoningCloseTag`) заменить на «есть **непустой** reasoning и он не
   закрыт», т.е. требовать `len(strings.TrimSpace(reasoningOut)) > 0` или
   `snap.ReasoningChars > 0` — уже посчитанный рядом факт
   (`handlers_chat.go:546-551`, `:1488`).

---

## Б. Пояснение «Модель сгенерировала только размышления» (жалоба 5)

### Б.1 Симптом

В конце **каждого** ответа OpenWebUI появляется
«Модель сгенерировала только размышления (1142 симв.)», хотя ответ пришёл.

### Б.2 Механизм: кто именно пишет этот текст

Текст существует в **двух** независимых формулировках и вставляется из **шести
точек** (в таблице пять строк: последняя объединяет два места воркера):

| Кто | Где | Точный текст | Условие |
|---|---|---|---|
| Балансер R261 (SSE) | `internal/balancer/empty_answer_r261.go:57-69`, вставка `:171` | `⚠️ Модель сгенерировала только размышления (N симв.), видимого ответа нет (finish_reason=…). …` | `st.visibleChars == 0 && st.reasoningChars > 0` (`:170`) |
| Балансер R261 (non-stream) | `empty_answer_r261.go:201-215` | тот же | `a.content.Len()==0 && a.reasoning.Len()>0` (`:205`) |
| Балансер R321 | `internal/balancer/native_done_hygiene_r321.go:101-119` | тот же (R261) | `done:true` + `content` пуст + `reasoningChars>0`; **с R321.2** ещё и `visibleStreamedChars==0` (`:70`) |
| Воркер R314 (non-stream) | `cmd/cppworker/handlers_chat.go:575`, функция `:589-594` | `Модель сгенерировала только размышления (N симв.), видимого ответа нет (бюджет ответа исчерпан). Увеличьте num_predict/max_tokens или отключите reasoning.` (`internal/i18n/messages_cppworker_r234.go:25-28`) | `content` пуст и `reasoningChars>0` |
| Воркер R314/R320 (stream) | `cmd/cppworker/handlers_chat.go:1509` и `:2291` | тот же | то же |

Различитель для оператора (по точному тексту):

* начинается с `⚠️` → это **балансер** (R261/R321);
* содержит `(finish_reason=` → балансер и `finishReason` известен;
* содержит `(бюджет ответа исчерпан)` → **воркер** (R314/R320).

Число «1142» встречается в комментарии R321.2
(`native_done_hygiene_r321.go:62`) как живой замер, то есть конкретно эта
жалоба относилась к вставке **балансера** до R321.2. Что именно увидит
оператор после выката R321.2 — зависит от того, какое из шести мест сработало;
это **не проверено** (нет живого лога стенда).

### Б.3 Почему вставка возможна, хотя ответ пришёл

**(а) Ключевое место R321.2.** `native_done_hygiene_r321.go:70`:
`if visibleStreamedChars > 0 { return line, false }`, где
`visibleStreamedChars` = `len(strings.TrimSpace(streamedBeforeThisLine))`
(`llamacpp_transport.go:1186-1187`), а `streamedBeforeThisLine` — это
`accumulatedPlainContent` **до** текущей строки (`:1032`), наполняемый только из
`msg.content` / `response` чанков (`:1072-1084`, `appendNativeContent` `:2507-2521`).

Отсюда три следствия, каждое из которых возвращает дефект:

1. **`accumulatedPlainContent` не единственный носитель ответа.** Если модель
   отдала весь текст в `message.thinking` (R256 переносит видимый текст в
   размышления — `internal/balancer/reasoning_egress_r256.go`), то
   `visibleStreamedChars == 0`, `nativeReasoningChars > 0` → пояснение
   вставляется. Для клиента, который **рисует** thinking (OpenWebUI рисует),
   «ответ пришёл» — но именно в блоке размышлений. То есть жалоба «ответ пришёл,
   а предупреждение есть» объясняется **полностью** без всякой ошибки в guard:
   ответ ушёл в thinking, а пояснение говорит правду, но в неверном месте (в
   `content`, рядом с настоящим ответом, который клиент уже показал).
2. **Порядок подмен.** В `llamacpp_transport.go` порядок такой:
   `stripNativeDoneContent` (`:1170`) → R321-hygiene (`:1186`) →
   `filterNDJSONLineMarkers` (`:1200`). Последний умеет **дописывать** content в
   терминальный кадр из придержанного хвоста
   (`internal/balancer/marker_hold_ndjson_r266_1.go:162-165`). То есть после
   hygiene content может снова стать непустым (уже другим) — три правила
   правят одно поле в трёх местах.
3. **`nativeReasoningChars` суммируется по всему стриму** (`:1062-1071`) и
   берётся как `max(len(thinking), len(reasoning))` на чанк, но **не**
   коррелирует с моментом завершения: если размышления были в начале, а ответ
   потом — счётчик остаётся положительным до конца, и любой пустой терминал
   (в т.ч. законно пустой после `stripNativeDoneContent`) получает пояснение.

**(б) Дублирующая проверка без guard'а — `cmd/cppworker/handlers_chat.go:2291`.**
Здесь условие — `content` (полный видимый текст, `:2271`), а не «сколько уже
уехало». Комментарий `:2286-2289` утверждает, что «дельты в этом случае не
отдавались». Но это верно не всегда: при `reasoningOut != ""` удержание
снимается по закрытию блока (`:1765-1775`), после чего **дельты идут**
(`:1875`), а `msg["content"]` заполняется полным текстом (`:2294`). Итог:
условие «`content` пуст» и условие «дельт не было» — **разные**, и вторая ветка
(`else if streamContent && emittedLen > 0`, `:2297-2322`) как раз обнуляет
content для случая «дельты были». Две ветки одного решения расходятся.

**(в) Расхождение поверхностей.** `/api/generate` **не имеет** пояснения R314
вовсе (`cmd/cppworker/handlers_generate.go:568-581` даёт `error` только при
полностью пустом выводе, а терминальный чанк всегда несёт `response: ""` —
`:596`). Значит один и тот же «ответ из одних размышлений» на `/api/chat`
объясняется, а на `/api/generate` — либо `error`, либо пустой `response`
(пояснение добавит уже балансер, `native_done_hygiene_r321.go:88-90`).

### Б.4 Минимальная правка (Б)

1. **Одна точка вставки.** Вынести решение в одну функцию
   (например `explainEmptyAnswer(path, content, deliveredChars, reasoningChars, reason)`)
   и вызывать её только там, где известны **все три** факта:
   `deliveredChars == 0` (видимого текста клиенту не уехало),
   `reasoningChars > 0`, `content == ""`. Сейчас эти три факта известны вместе
   только в `native_done_hygiene_r321.go:70-99`; в воркере заменить три места
   (`handlers_chat.go:575, 1509, 2291`) одним вызовом, передав `emittedLen`
   (`handlers_chat.go:1722`) как `deliveredChars`.
2. **В R321** заменить `visibleStreamedChars` на величину, которая не зависит
   от того, в каком поле уехал текст: добавить в транспорт счётчик
   `nativeEmittedContentChars` (инкремент в момент **записи** строки клиенту,
   `:1203`, по фактическому `msg.content`/`response` после всех подмен) и
   передавать его. Тогда «ответ уехал в thinking» перестанет маскироваться под
   «ответа не было».
3. **Порядок подмен зафиксировать:** гигиена/пояснение — **после**
   `filterNDJSONLineMarkers` (сейчас наоборот), чтобы финальным решением о
   content владело одно место.
4. **`/api/generate`** привести к тому же правилу (сейчас его нет).

---

## В. Окно контекста и `n_ctx` (жалоба 3)

### В.1 Симптом

OpenWebUI «не смог удовлетвориться окном 131072» при промпте на 15128 символов
и минимальном бюджете 32768, при том что загруженная модель имела 131072;
ощущение — «по старой сессии не очищен кэш».

### В.2 Где берётся «доступное окно» для клиента

Два разных ответа на один вопрос:

| Величина | Где считается | Что означает |
|---|---|---|
| **Суммарное окно модели** | `internal/balancer/num_ctx_resolver.go:350-380` (`loadedWindowNCtx`: `max(metrics, coordinator)`), `:554-603` (`loadedWindowState`), `:614-665` (`resolveLoadedWindowNCtx`) | «модель загружена с N» — то, что видит оператор в `/api/models` и в карточке |
| **Окно одного слота** | `num_ctx_resolver.go:394-430` (`loadedPerSeqNCtx`): `ContextPerSeq`, иначе `ContextPerSlot(total, maxSlots/parallel)`, иначе `total` | «сколько реально доступно ОДНОМУ запросу» |
| **Окно, которое уходит воркеру** | `num_ctx_resolver.go:881-889`: `if perSeqLoaded > 0 && n > perSeqLoaded → return perSeqLoaded` | значение заголовка `X-Cpp-Ctx` |
| **Воркер применяет как потолок** | `cmd/cppworker/nctx_clamp.go:96-114` | `X-Cpp-Ctx` **понижает** `num_ctx` из body |

То есть при `parallel=2` и суммарном 131072 клиент физически получает 65536, а
при `parallel=4` — 32768. Числа «131072» и «32768» в жалобе — это ровно
суммарное и слотовое окно одной и той же модели.

### В.3 Почему отказ возможен при загруженных 131072

**(1) `LB_NCTX_RELOAD_MAX_N_CTX` работает физическим потолком — вопреки R255/R267.**

* `internal/balancer/preflight_helper.go:410`:
  `state.PhysicalMaxContext = resolvePhysicalMaxContext(GGUFMaxContext, profileContextLengthMax, AutoReloadMaxNCtx)`,
  а `resolvePhysicalMaxContext` (`:431-447`) при пустом `contextLengthMax` берёт
  `cap = autoReloadMaxNCtx` (env).
* `internal/balancer/preflight_nctx.go:769-783` — отказ 413 «window too big» по
  `physicalCeiling()` (`:540-548`), а `:784-794` — второй отказ, уже по
  `cfg.AutoReloadMaxNCtx` **сырым** значением env.
* При этом R255 (`internal/balancer/nctx_declared_r255.go:122-155`) и R267
  (`internal/balancer/nctx_ceiling_r267.go:20-31`) объявляют env
  **только** потолком АВТО-роста: `effective = max(env, declared)`. R267 прямо
  пишет: «Формула потолка одна… Вторая формула в пакете запрещена — именно из-за
  неё правка „в одном месте“ не работала» (`nctx_ceiling_r267.go:20-24`).
* **Факт:** `DecidePreflight` (`preflight_nctx.go:659`) R267-правку **не
  получил** — он сравнивает с `cfg.AutoReloadMaxNCtx` (`:784`) и с
  `state.PhysicalMaxContext`, собранным из env (`preflight_helper.go:410`).
  Значит оператор, заявивший окно 131072 в карточке при env=32768 и
  загруженной модели 131072, получает **413** от preflight — и именно с текстом
  «модель поддерживает максимум …» (`makeWindowTooBigReject`, `:939-972`).

Это ровно тот дефект, который R267 описывает как «правка одного слоя не лечила
другой» (`nctx_ceiling_r267.go:11-18`) — только теперь слой не решатель, а
preflight.

**(2) Проверка идёт по окну СЛОТА, а клиент и оператор считают СУММАРНОЕ.**

`preflight_nctx.go:767-768`:
`slots := state.effectiveSlots(); targetTotal := perClientToTotal(reqWindow, slots)`,
где `perClientToTotal` (`:836-844`) = `PadContext(perSeq) * slots`. Запрос
`num_ctx=131072` при 2 слотах требует суммарно 262144 → превышает физический
потолок 131072 → 413 с `max_per_client_n_ctx = 65536` (`:943-959`). Оператор
видит «модель 131072, а просят 131072 и отказывают».

**(3) Устаревший/неполный кэш действительно есть — и reload обновляет его НАПОЛОВИНУ.**

* `loadedPerSeqNCtx` читает **только** `metricsMgr.llamaMetrics`
  (`num_ctx_resolver.go:394-403`) — то есть кэш поллера, который ходит раз в
  30 с (`internal/balancer/llamacpp_metrics_poller.go:81`).
* После preflight-reload вызывается `cacheLoadedContextLength`
  (`preflight_helper.go:185`, `:224`), и она пишет **только**
  `ContextLength = nCtx` (`nctx_reload_handlers.go:1495-1505`) — поля
  `ContextPerSeq` и `MaxSlots` остаются от **прежней** загрузки.
* А `loadedPerSeqNCtx` отдаёт приоритет именно `ContextPerSeq`
  (`num_ctx_resolver.go:416-418`). Итог: после перезагрузки в 131072 «окно
  слота» продолжает возвращаться старым (например 16384) до следующего тика
  поллера. Это буквально то, что оператор назвал «не очищен кэш от старой
  сессии».
* После успешного `DoReload` пишется **только** `SetLastKnownNCtx`
  (`internal/balancer/nctx_reload.go:1386`), кэш метрик там не обновляется
  вообще. `loadedWindowNCtx` этот разрыв маскирует (`max(metrics, coordinator)`,
  `num_ctx_resolver.go:372-379`), а `loadedPerSeqNCtx` — **нет**.
* Известный живой дефект того же класса: `docs/plans/2026-10-14-handoff-r118.md:85-107`
  («пустой кэш n_ctx после рестарта → лишний reload», «запросы висят до бюджета
  ожидания 900 с»).

**(4) Две разные величины записываются как «последнее известное окно».**

* `DoReload` пишет **фактическое** окно: `SetLastKnownNCtx(backendID, actual)`
  (`nctx_reload.go:1379-1386`), и отдельно предупреждает, если фактическое
  меньше запрошенного (`:1390-1405`).
* preflight пишет **целевое**: `SetLastKnownNCtx(backendID, decision.TargetNCtx)`
  — четыре места: `preflight_nctx.go:1286, 1314, 1339, 1399`.

То есть после «недороста» окна (документированный случай: просили 131072,
получили 65536 — `nctx_reload.go:1392-1396`) preflight-путь запишет в кэш
131072, которого нет. Следующий запрос посчитает окно достаточным.

**(5) `LastKnownNCtx` — значение по БЭКЕНДУ, а не по модели.**
Это прямо зафиксировано комментарием: `num_ctx_resolver.go:620-622`
(«LastKnownNCtx — значение по БЭКЕНДУ, а не по модели»).
`resolveLoadedWindowNCtx:623-630` и `:638-641` используют его как «модель
загружена с окном N» — то есть после смены модели на том же бэкенде кэш
описывает **другую** модель.

### В.4 Что вернётся OpenWebUI, если он просит `num_ctx` МЕНЬШЕ загруженного

* Резолвер **повышает** запрос клиента до окна слота:
  `num_ctx_resolver.go:840-848` (`if perSeqLoaded > 0 && perSeqLoaded > n { n = perSeqLoaded }`).
  Это противоречит и шапке файла («Body всегда побеждает», `:8-10`, `:802`), и
  комментарию воркера «Политика балансировщика НЕ повышает num_ctx — только
  ограничивает» (`nctx_clamp.go:32`). Клиент просит 32768, получает 65536 (если
  слот такой) — то есть **другой** режим памяти, чем он выбрал.
* preflight при этом вернёт `NoOp` (окно слота больше запрошенного,
  `preflight_nctx.go:749-758`) — перезагрузки не будет.
* Если клиент просит меньше, чем **суммарное**, но больше, чем слот — резолвер
  понизит до слота (`:881-889`), то есть клиент **не получит** отказ, но
  получит меньше, чем просил, без уведомления в теле ответа.

### В.5 Минимальная правка (В)

1. **Один потолок.** В `DecidePreflight` заменить `cfg.AutoReloadMaxNCtx`
   (`preflight_nctx.go:784`) и `state.PhysicalMaxContext`-из-env
   (`preflight_helper.go:410`) на то же значение, что уже считается в
   `nctx_reload_handlers.go:79-83`:
   `ReloadCeiling{Effective: p.effectiveAutoReloadCeiling(model, cfg.AutoReloadMaxNCtx), Env: cfg.AutoReloadMaxNCtx}`
   — и считать `physicalCeiling` только из `GGUF` и `profileCapCtx`.
2. **Согласовать «слот vs сумма» в тексте отказа.** `makeWindowTooBigReject`
   (`:939-972`) обязан называть **три** числа: суммарное окно модели,
   окно слота, запрошенное (сейчас `loaded_n_ctx` и `context_per_seq` есть,
   но причина пишется как «model supports max context», что читается как
   «модель не умеет 131072»).
3. **Обновлять `per-seq` после reload:** в `preflight_helper.go:185` и `:224`
   рядом с `cacheLoadedContextLength` вызвать обновление
   `ContextPerSeq/ContextLength` (или `refreshLoadedWindowNCtx`,
   `num_ctx_resolver.go:487-497`) — одна строка, устраняет 30-секундное окно
   рассинхрона.
4. **`SetLastKnownNCtx` после preflight** писать достигнутое значение, а не
   `TargetNCtx` (`preflight_nctx.go:1286/1314/1339/1399`) — либо сбрасывать
   кэш, если reload вернул меньше (`DoReload` это уже умеет, `nctx_reload.go:1379-1386`).
5. **Ключевать `LastKnownNCtx` по паре (backend, model)** — сейчас по бэкенду
   (`num_ctx_resolver.go:620-622`).

---

## Г. Очередь и репликация (жалоба 4)

### Г.1 Симптом

«При полностью доступном бэкенде НЕ поднялась вторая копия модели и в очередь на
первую модель балансер не поставил».

### Г.2 При каком условии запрос ставится в очередь

Очередь — одна (`admissionQueue`, `internal/balancer/admission_queue.go:242`,
создание `internal/balancer/proxy.go:518`), но **входов** в неё три:

| Путь | Вход | Ключ очереди | Файл |
|---|---|---|---|
| llama.cpp-хендлеры (`/api/chat`, `/api/generate`, `/v1/chat/completions`) | `acquireInferenceAdmission` | ID бэкенда | `admission_queue.go:758-809`, вызовы `llamacpp_handlers_inference.go:272, 953, 1134` |
| общая Ollama-поверхность (`Proxy.ServeHTTP`) | `waitForInferenceBackend` | строка `"any"` | `unified_queue_r73.go:112-208` (ключ `:39`, `:154`) |
| image-плоскость | свои лимиты | — | `image_resources.go:49` (комментарий) |

**Когда очередь не работает вообще:** `p.admission == nil || wait <= 0`
(`admission_queue.go:552-557`, `:603-605`) → `errAdmissionDisabled` →
немедленный 503 (`unified_queue_r73.go:130-132`, `proxy.go:1491-1492`).

**Ключевая находка:** `wait` берётся **один раз при создании Proxy**
(`proxy.go:518` → `newAdmissionQueue(AdmissionWaitTimeout())`), а
`AdmissionWaitTimeout()` — «горячая» настройка (`admission_queue.go:87-100`,
`internal/settings/builtin_r278.go:176-187`, `HotReload: true`). Функции
`setAdmissionWait` (`admission_queue.go:560-567`) в production-коде **никто не
вызывает** — только тесты (`unified_queue_r73_test.go:201`). То есть
переключатель `LB_ADMISSION_WAIT_SEC` в WebUI меняет ответ API и лог, но **не**
фактическое поведение очереди. Тест `settings_hotapply_r278_test.go:61-66`
проверяет только геттер — то есть ровно ту «полуправку», от которой сам
предостерегает (`:5-9`).

### Г.3 Что происходит, если модель занята одним слотом (`maxConcurrentReqs=1`)

1. `tryAcquireSlot` (`slot_manager.go:18-76`) сравнивает `ActiveReqs` с
   `EffectiveMaxConcurrentRequests()` (`pkg/types/backend.go:476-478`,
   резолвер `:510-524`).
2. Вместимость берётся из **фактических слотов модели** (`RuntimeModelSlots`),
   иначе из заявки узла, иначе из конфига. Отсюда известный дефект:
   `docs/plans/2026-10-14-handoff-r115-r116.md:48` — «балансер сообщает бэкенду
   `maxConcurrentRequests=2`, при этом у загруженной модели `max_slots=1`… второй
   запрос **не встаёт в очередь**, потому что вместимость завышена; сериализует
   его сам llama.cpp».
3. `tryAcquireSlot` **обходит** занятость зомби-preemption:
   `slot_manager.go:35-71` — при занятых слотах удаляет «зомби»-сессию и
   **пропускает** запрос (`return true` на `:66`). `ActiveReqs` при этом
   остаётся равен пределу (`--` на `:61`, `++` на `:64` — нетто ноль), то есть
   запрос обслужен **сверх** вместимости, а очередь не задействована. Это
   второй «детектор занятости», расходящийся с `checkResourceLimitsLocked`
   (`slot_manager.go:124-237`), где та же проверка сделана иначе
   (`:140-141`).
4. До admission стоят **три** guard'а, которые могут отказать вместо очереди:
   * R288 `contextGrowthBlocker` → `writeContextGrowthDeferred` 503:
     `context_growth_r288.go:95-152`, вызовы `llamacpp_handlers_inference.go:258, 942, 1123`;
   * R299 `growthDeferR299` → `writePreflightDeferredR299`:
     `growth_guard_r299.go:63-125`, вызов `preflight_helper.go:129-138`;
   * R301 `growthVerdictR301` → `writePreflightDeferredR299`:
     `growth_stream_guard_r301.go:97-151`, вызов `proxy.go:1431-1437`.

   Первые два срабатывают, только если есть активные стримы/идёт reload
   (`context_growth_r288.go:138-141`, `growth_guard_r299.go:79-84`), третий для
   стриминга отказывает **всегда**, когда оценка промпта не влезает в окно слота
   (`growth_stream_guard_r301.go:144-150`) — независимо от занятости.

### Г.4 Где принимается решение о второй копии и что его отключает

Механизм: `internal/modelreplication` (группа + контроллер).

| Условие | Файл:строка | Что означает |
|---|---|---|
| Менеджер создаётся только при `balancing.modelReplication.enabled=true` | `internal/balancer/rpc_modules.go:19-20` | без флага репликации нет вовсе |
| либо по требованию политики (`ensureReplicationManager`) | `placement_replication_r74.go:131-145`, вызов `:175` | включается, если есть placement-правило |
| Группа создаётся **только** для правила `strategy: replicated` или `auto.prefer` с replicated | `placement_replication_r74.go:155-166`, `:197-204` | модель без правила → группы нет → второй копии не будет |
| Маска в имени модели (`Qwen3*`) → группа **пропускается** | `placement_replication_r74.go:171-174` | «звёздочка» = нет репликации |
| `syncPlacementReplicationGroups` выходит, если `!cfg.Enabled` (placement выключен) | `:156-159` | |
| Контроллер тикает раз в 10 с и вызывает `EnsureInstances` | `internal/modelreplication/group_controller.go:26-29`, `:117-127` | реакция не мгновенная |
| `EnsureInstances` выходит, если `!IsEnabled()` | `internal/modelreplication/model_group.go:340-343` | |
| scale-up только если `loaded+inFlight < MinInstances` | `model_group.go:389-392` | при `MinInstances=1` вторая копия не появится **никогда** |
| «свободный бэкенд» = `Status==Healthy && !cannotHostModelR304 && ActiveReqs==0` | `placement_replication_r74.go:57-91` (`:62`, `:68`, `:86`) | бэкенд, на котором **идёт** запрос, не свободен: реплика не поднимется, пока модель занята |
| warmup асинхронный, инстанс `LOADED` только после подтверждения метриками | `model_group.go:597-618`, `:409-486` | «поднялась» ≠ «загружена» |

Итог по жалобе 4: **вторая копия не является автоматической реакцией на
занятость**. Она следует только из явной политики (`replicated`) и только на
бэкенде без активных запросов; при этом сама документация признаёт это
ограничением: `docs/architecture/balancing-guide.ru.md:767` («Model Affinity:
всех направляет на один бэкенд — идеальный сценарий: подгружает дубликаты при
загрузке > threshold»). Живой стенд с единственным llama.cpp-бэкендом описан в
`docs/plans/2026-10-14-handoff-r118.md:119-126`: вторую копию ставить физически
некуда. Есть ли на текущем стенде второй здоровый бэкенд — **не проверено**.

### Г.5 Почему «запрос крутится», но его не видно на мониторе (жалоба 2)

1. **Стрим регистрируется после admission.**
   `beginClientStreamR271` вызывается в транспорте
   (`llamacpp_transport.go:767`, `streaming.go:60`, `proxy_request.go:782`,
   `proxy_request_hijack.go:327`), а не при постановке в очередь. Это прямо
   зафиксировано в `context_growth_r288.go:66-69` и
   `growth_guard_r299.go:24`. Значит ожидающий и отклонённый guard'ом запрос в
   счётчиках стримов **не виден**.
2. **Строки `pending` в панели очереди не заполняются.** После R78
   `QueueManager` — только хранилище статистики
   (`internal/balancer/queue_manager.go:11-16`, `:105-120`), а
   `getPendingDTOs` (`:179-196`) читает `qm.pending`, которое **никто не
   пишет** (единственные записи — в тестах: `unload_scheduler_test.go:360-364`).
   Монитор получает только агрегат `CurrentSize` из admission
   (`cluster_state.go:409-417`), а «обрабатываемые» — из `ActiveReqs`
   (`cluster_state.go:470-489`). Отсюда буквально: запрос ждёт — счётчик
   изменился, строки нет.
3. **Сессия может не совпасть с той, что ждёт.** Ключ сессии строится из
   `X-Client-ID/IP/модель/endpoint` либо отпечатка разговора
   (`internal/balancer/client.go:285-359`), а ключ очереди — из
   `RequestSessionKey` (`admission_queue.go:148-150`, `:764`). Два резолвера
   ключа — тот же класс дефекта, что уже признан и вылечен в R118.9/R252
   (`client.go:343-359`: «сжатие истории не должно начинать новую сессию»).
4. **Двойная очередь (по бэкенду и «any»)** означает, что ожидающий на
   `/api/chat` (ключ = ID бэкенда) и ожидающий на общей поверхности (ключ
   `any`) стоят в **разных** подочередях одной структуры
   (`admission_queue.go:291-334`, `unified_queue_r73.go:154`): справедливость и
   позиции считаются раздельно, суммарная картина в статистике одна
   (`admission_queue.go:444-505`).

### Г.6 Минимальная правка (Г)

1. **Ожидание слота — «горячее»:** заменить чтение `p.admission.wait` на
   `p.admissionWaitTimeout()`-функцию, читающую реестр на каждом вызове
   (`admission_queue.go:552-557`), либо подписаться на изменение настройки и
   вызывать `setAdmissionWait`. Иначе переключатель в WebUI не работает.
2. **Регистрировать ожидание:** при `enqueue` добавлять строку в
   `pending`-представление (модель, сессия, позиция, время). Одна функция
   `aq.snapshot()` вместо мёртвого `qm.pending` — тогда монитор покажет
   ожидающих (`queue_manager.go:179-196` можно удалить).
3. **Свести ключи:** один резолвер ключа сессии для очереди и липкости
   (`client.go` + `admission_queue.go:764`), иначе ожидание и сессия расходятся.
4. **Зомби-preemption не должен пропускать сверх вместимости:**
   `slot_manager.go:60-66` — вернуть `false` (пусть запрос встанет в очередь)
   либо честно увеличить `maxReqs`, а не отдавать слот «в обход».
5. **Репликация: реагировать на занятость, а не только на минимум.**
   Добавить в `ensureGroupInstances` (`model_group.go:389-392`) правило
   «`loaded < MaxInstances` и на целевом узле `ActiveReqs >= maxReqs` дольше T →
   масштабировать», и снять `active == 0` из критерия «свободный»
   (`placement_replication_r74.go:86`) в пользу «нет **другой** модели/стрима,
   который держит VRAM» (проверка памяти, а не запросов).

---

## Д. Детерминированность: одно решение — два места

### Д.1 Таблица расхождений

| Решение | Где принимается (1) | Где ещё (2) | Расхождение |
|---|---|---|---|
| **Допустимые значения `done_reason`** | контракт: `docs/architecture/balancer-cppworker-contract.ru.md:213,223-227` (`stop/length/load/unload`) | не менее 50 мест присваивания, в т.ч. `llamacpp_transport.go:1981,2359`; `helpers:252-286`; `auto_continue.go:1114,1130`; `stream_abort_frames_r296.go:124`; `handlers_chat.go:1301,2252,2333` | контракт допускает 4, код отдаёт 7 (`+tool_calls,error,cancelled,truncated`); проверки значений нет ни в одной точке |
| **Кто владеет `message.content` в терминальном кадре** | `stripNativeDoneContent` — `llamacpp_transport.go:2523-2582` (обнуляет дубль) | `writeStreamingSSEDone` — `helpers:174-178,208-215` (восстанавливает полный текст); `marker_hold_ndjson_r266_1.go:158-165` (дописывает хвост); воркер `handlers_chat.go:1515,2319,2294` | на одном пути content то обнуляется, то дублируется, то дополняется — порядок `:1170 → :1186 → :1200` |
| **Пояснение вместо пустого ответа** | `empty_answer_r261.go:57-69` (балансер, SSE/non-stream) | `native_done_hygiene_r321.go:101-119` (балансер, NDJSON); `handlers_chat.go:575,1509,2291` (воркер) | 6 точек, 2 разных текста, guard «видимый текст уже уехал» есть только в `native_done_hygiene_r321.go:70` |
| **Потолок окна (physical)** | `resolvePhysicalMaxContext` — `preflight_helper.go:431-447` (GGUF + `contextLengthMax` + **env**) | `effectiveAutoReloadCeiling` — `nctx_declared_r255.go:139-155` (`max(env, declared)`) | env как физический предел vs env как предел только авто-роста; R267 объявляет вторую формулу запрещённой (`nctx_ceiling_r267.go:20-24`), но `preflight_nctx.go:784` использует сырой env |
| **Потолок для клиентского `num_ctx` (clamp)** | `maxNumCtxForModel` — `num_ctx_resolver.go:706-763` (физ. потолок → профиль → default → GGUF) | `resolveModelMaxContext` — `nctx_reload_handlers.go:2024-2097` | в коде прямо написано «MUST stay in sync» (`num_ctx_resolver.go:703-705`) — то есть синхронность держится комментарием, а не общим кодом |
| **Суммарное окно vs окно слота** | `loadedWindowNCtx` — `num_ctx_resolver.go:350-380` (`max(metrics, coordinator)`) | `loadedPerSeqNCtx` — `:394-430` (**только** metrics), `NCtxBackendState.effectivePerSeqNCtx` — `preflight_nctx.go:511-519` | у первой есть fallback на координатор, у второй нет → после reload окно слота «старое» до 30 с |
| **Запись «последнего известного окна»** | `DoReload` → `SetLastKnownNCtx(actual)` — `nctx_reload.go:1379-1386` | preflight → `SetLastKnownNCtx(TargetNCtx)` — `preflight_nctx.go:1286,1314,1339,1399` | фактическое vs целевое; при «недоросте» в кэш попадает недостигнутое |
| **Занятость бэкенда** | `tryAcquireSlot` (`slot_manager.go:18-76`, включает зомби-preemption `:35-71`) | `checkResourceLimitsLocked` (`slot_manager.go:124-237`, проверка `:140-141`) | первая может вернуть «свободно» при полной занятости; вторая — нет. Два ответа на один вопрос |
| **Право поставить в очередь** | `admissionQueue.enqueue` (`admission_queue.go:291`) | guard'ы R288/R299/R301 **до** очереди (`context_growth_r288.go:95-152`, `growth_guard_r299.go:63-85`, `growth_stream_guard_r301.go:97-151`) | «нужно больше окна» решается трижды; R288 считает `prompt + budget` (`context_growth_r288.go:103-118`), R301 для стрима — **только** `prompt` (`growth_stream_guard_r301.go:124-143`) |
| **Ключ очереди / сессии** | `RequestSessionKey` (`admission_queue.go:148-150,764`) | `getSessionIDWithModel` (`client.go:285-359`) | два резолвера; ожидающий и сессия могут быть разными сущностями |
| **Детектор обрыва** | `TruncateReason` (R60.21) — `llamacpp_transport.go:2048` | `streamAbortReasonR259` + `streamStallWatch` (R298) — `llamacpp_transport.go:2179-2193`; `toolsReasoningUnclosedR320` (`handlers_chat.go:1574-1582`); `Snapshot().Unclosed` (`handlers_chat.go:1487`, `handlers_generate.go:590,844`) | четыре независимых признака «обрезано», один из них (эвристика промпта) даёт ложные срабатывания |
| **Терминальный кадр** | `emitDeferredDone`/`emitTerminalDoneOnly` — `llamacpp_transport.go:1905-1995` (идемпотентно через `doneEmitted`) | `EmitContinuationNDJSON` — `auto_continue.go:1085-1165` (собственный `done:true`); `abortFramesR296` (`stream_abort_frames_r296.go:120-128`); `emitTruncatedChunk` (`llamacpp_transport.go:2308-2367`) | идемпотентность держится флагом `deferredDoneChunk`, который на нативном пути не выставляется (`:1230` не доходит до `:1245`) |
| **«Горячая» настройка очереди** | `AdmissionWaitTimeout()` читает реестр (`admission_queue.go:87-100`) | фактическое ожидание = `p.admission.wait`, зафиксированное при старте (`proxy.go:518`, `:552-557`) | изменение в WebUI не влияет на поведение; тест `settings_hotapply_r278_test.go:61-66` проверяет только геттер |

### Д.2 Почему это воспроизводимо, а не «теоретически»

Все перечисленные пары — не стилистика: каждая означает, что для **одного и
того же** запроса решение зависит от того, какой путь его обслужил
(NDJSON-нативный, SSE→NDJSON-трансляция, общий `ServeHTTP`, tools-путь
воркера). Живые примеры такого расхождения уже зафиксированы в проекте:

* `llamacpp_transport.go:1029-1056` — два накопителя текста дали чётное число
  ` ``` ` и отключили авто-продолжение;
* `nctx_ceiling_r267.go:11-18` — потолок применялся в двух слоях;
* `num_ctx_resolver.go:703-705` — «эта функция ОБЯЗАНА остаться в синхроне»;
* `handlers_chat.go:2286-2289` vs `:2297-2322` — две ветки одного решения.

---

## План исправлений

### P0 (ломает клиентов прямо сейчас)

| # | Правка | Файл:строка | Проверка |
|---|---|---|---|
| P0-1 | Запретить второй терминатор на нативном пути: `&& deferredDoneChunk` (или `nativeTerminalSent`) | `internal/balancer/llamacpp_transport.go:1997` | тест «ровно один `done:true`» для `/api/chat` при `LB_AUTO_CONTINUE_ON_TRUNCATION=1` |
| P0-2 | Убрать ложный `length` в `toolsReasoningUnclosedR320`: требовать непустые размышления | `cmd/cppworker/handlers_chat.go:1574-1582` | таблица: промпт открывает thinking, ответ без тега → `stop` |
| P0-3 | Единый `normalizeDoneReason` + реестр допустимых значений; `truncated` → `error` | `llamacpp_transport.go:1981,2359`; `helpers:252-286`; `auto_continue.go:1114,1130`; `stream_abort_frames_r296.go:124` | юнит-тест по таблице значений; живой прогон `scripts/verify-stream-r231.js` |
| P0-4 | Один владелец финального content: перенести пояснение/гигиену **после** `filterNDJSONLineMarkers` | `llamacpp_transport.go:1170,1186,1200` | тест «ответ дельтами + reasoning → пояснения нет» |
| P0-5 | Guard «видимый текст уже уехал» во все 6 точек пояснения (R261-SSE, R261-non-stream, R321 и три места воркера) | `empty_answer_r261.go:170,205`; `handlers_chat.go:575,1509,2291`; `native_done_hygiene_r321.go:70` | тест на каждый путь; текст ответа не подменяется |
| P0-6 | Единый потолок окна: `DecidePreflight` берёт `effectiveAutoReloadCeiling`, а не сырой env | `internal/balancer/preflight_nctx.go:784`; `preflight_helper.go:410` | тест: env=32768, профиль 131072, модель 131072 → NoOp, не 413 |

### P1 (деградация и «невидимость», но не порча ответа)

| # | Правка | Файл:строка |
|---|---|---|
| P1-1 | «Горячее» ожидание очереди (читать реестр на каждом вызове) | `admission_queue.go:552-557`; `proxy.go:518` |
| P1-2 | Показывать ожидающих в мониторе (заменить мёртвый `qm.pending`) | `queue_manager.go:179-196`; `cluster_state.go:409-417` |
| P1-3 | Свести три pre-admission guard'а в один вердикт с одной формулой | `context_growth_r288.go:95-152`; `growth_guard_r299.go:63-85`; `growth_stream_guard_r301.go:97-151` |
| P1-4 | Обновлять в кэше **все** окна после reload (`ContextLength`, `ContextPerSeq`, `MaxSlots`), а не только суммарное | `nctx_reload_handlers.go:1495-1505`; `preflight_helper.go:185,224`; `num_ctx_resolver.go:487-497` |
| P1-5 | `SetLastKnownNCtx` после preflight — достигнутое, а не целевое; ключ по (backend, model) | `preflight_nctx.go:1286,1314,1339,1399`; `num_ctx_resolver.go:620-630` |
| P1-6 | Текст отказа по окну: три числа + честная причина | `preflight_nctx.go:939-972` |
| P1-7 | Зомби-preemption не пропускает сверх вместимости | `slot_manager.go:60-66` |
| P1-8 | Пояснение на `/api/generate` привести к общему правилу | `handlers_generate.go:568-581,593-608` |

### P2 (гигиена и наблюдаемость)

| # | Правка | Файл:строка |
|---|---|---|
| P2-1 | Формализовать контракт причин в коде (`done_reason` + `finish_reason` таблицей), обновить документ | `docs/architecture/balancer-cppworker-contract.ru.md:213-227` |
| P2-2 | Репликация по занятости, а не только по `minInstances`; «свободный» = по памяти, не по `ActiveReqs` | `model_group.go:389-392`; `placement_replication_r74.go:86` |
| P2-3 | Один резолвер ключа сессии/очереди | `admission_queue.go:148-150,764`; `client.go:285-359` |
| P2-4 | Убрать `writeStreamingSSEDone`-восстановление content или сделать его единственным владельцем | `llamacpp_transport_helpers.go:174-178,208-215` |
| P2-5 | Явно логировать источник окна (`limited_by`) в каждом отказе 413 | `nctx_ceiling_r267.go:47-75`; `preflight_nctx.go:939-972` |

---

## Что именно проверять на стенде после правок

1. `POST /api/chat` (stream, tools, включённое reasoning): посчитать
   `done:true` — должен быть ровно один, `done_reason` ∈
   `{stop, length, tool_calls, error}`.
2. `POST /api/chat` с `LB_AUTO_CONTINUE_ON_TRUNCATION=1` и обрезанным ответом:
   один терминатор, продолжение в `done:false`-кадрах.
3. Ответ, где весь текст ушёл в `message.thinking`: пояснение **допустимо**, но
   только если в `content` за весь стрим не уехало ни байта (лог
   `stream egress finalize` — `empty_answer_r261.go:184-192`).
4. `num_ctx=131072` при модели, загруженной 131072 суммарно и `parallel=2`:
   ожидается либо обслуживание с окном слота (65536) и **честным** текстом
   причины, либо 413 с тремя числами — но не «модель поддерживает 131072».
5. Занятый бэкенд (`ActiveReqs=maxReqs`): второй запрос обязан получить
   `X-Queue-Position` (а не 503) и появиться в панели очереди строкой.
6. `LB_ADMISSION_WAIT_SEC` изменить из WebUI на 0 и обратно: поведение очереди
   обязано меняться **без рестарта** (сейчас не меняется — P1-1).

## Не проверено

* Поведение самого Cline (что именно он считает «COMPLETE»: `finish_reason`,
  `done_reason`, наличие `tool_calls` или отсутствие контента) — внешний
  клиент, исходников в этом репозитории нет. Все утверждения о Cline в
  разделе А опираются на документы проекта
  (`docs/features/cline-agent-loop.ru.md:39,67,211-213`) и на ранее
  зафиксированные живые разборы (`CHANGELOG.md:8304-8325`), а не на код Cline.
* Точная живая атрибуция жалобы 5: какая из двух формулировок (балансерная с
  `⚠️`/`finish_reason=` или воркерская с «бюджет ответа исчерпан») и какое из
  шести мест сработало. Число 1142 совпадает с замером в
  `native_done_hygiene_r321.go:62`, но живого лога текущего стенда нет.
* Точные числа жалобы 3 (15128 символов промпта, бюджет 32768, заявленное окно
  131072, фактическое `parallel`) — в коде нет привязки к этому прогону; в
  разделе В перечислены механизмы, каждый из которых даёт отказ при таких
  числах, но какой именно сработал — не проверено.
* Состав кластера на текущем стенде (есть ли второй здоровый llama.cpp-бэкенд,
  включены ли `balancing.modelReplication.enabled` и placement-правила,
  `parallel`/`max_slots` загруженной модели) — не проверено; от этого напрямую
  зависит вывод по жалобе 4 (см. `docs/plans/2026-10-14-handoff-r118.md:119-126`).
* Влияние `LB_NCTX_RELOAD_MAX_N_CTX` на текущем стенде (какое значение задано) —
  не проверено.
* Дублирование R-механизмов в `internal/cppbackend`, `internal/api` и WebUI
  (панель настроек, карточка модели) — в этот аудит не входило.

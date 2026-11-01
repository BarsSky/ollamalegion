# R366/R367. flash attention из профиля модели: правильное значение и внятный отказ

Дата: 2026-10-31 — 2026-11-01. Стенд: `cppworker-gpu`, RTX 3070 (8 ГБ),
модель `gemma-4-E4B-it-Q4_K_M`, образы `gpu-2886089e` (R366) и
`gpu-94a0abdc` (R367).

## 1. R366: профиль «выключить flash attention» не выключал

Поле `flashAttn` профиля обрабатывалось строкой внутри
`applyProfileToLoadRequest` и ошибоалось дважды.

**Ошибка семантики.** `false` превращалось в `-1`. Мост понимает поле иначе
(`c/bridge/bridge.go:176`):

```
-1 = auto, 0 = disabled, 1 = enabled
```

То есть «выключить flash attention» в профиле означало «пусть llama.cpp решит
сам», и на поддерживаемых моделях он включал обратно. Оператор видел галочку
«выключено» и работающий flash attention.

**Ошибка приоритета.** Помощник заполняет только ПУСТЫЕ поля («0 = не
задано»), а у этого поля 0 — содержательное значение. При дефолте контейнера
`CPPWORKER_FLASH_ATTN_TYPE=-1` профильное «выключить» не применялось вообще:
`-1 != 0`, то есть «клиент задал».

### Что сделано

Одно правило на все четыре пути (`cmd/cppworker/profile_flash_attn_r366.go`):

```
явное значение запроса > профиль модели > дефолт (конфиг / текущая загрузка)
```

| путь | «явное» значение | дефолт |
| --- | --- | --- |
| `handleLoadWithParams` | `req.FlashAttnType` | конфиг/env |
| `handleReloadModel` | `req.FlashAttn` | значение текущей загрузки |
| ленивая загрузка | нет (opts берётся из env-флага) | env |
| legacy `/api/models/load` | `req.FlashAttnType` (через резолвер R364) | конфиг/env |

Из `applyProfileToLoadRequest` параметр `flashAttnType` убран: с «0 = не
задано» поле корректно не выражается, а держать рядом вторую (неверную)
конверсию — значит оставить дефект жить на одном из путей.

Стюард-тест требует `profileFlashAttnToLoad` на каждом пути, который применяет
профиль. Негативная проверка выполнена (при удалении вызова из
`handleLoadWithParams` тест падает).

### Живая проверка

```
PUT профиля: flashAttn=false, kvCacheType=f16, ctx 32768
POST /api/models/load {"name":"gemma-4-E4B-it-Q4_K_M"}   (без параметров)
→ llama_context: flash_attn = disabled
→ state=loaded, ctx=32768, gpu=42/42
```

До правки здесь было бы `flash_attn = enabled` (false → -1 = auto).

## 2. R367: противоречие «flash off + квантованный KV» объясняется до загрузки

Побочный результат живой проверки R366: с профилем `flashAttn=false` и
`kvCacheType=q4_0` загрузка упала

```
POST /api/models/load → 500
"load failed: ... failed to create context"
llama_init_from_model: quantized V cache requires flash_attn to be enabled
```

llama.cpp прав (квантованный V-cache требует flash attention), но оператор
видел невнятное «failed to create context», хотя противоречие известно ДО
загрузки.

Теперь предзагрузочная проверка стоит на всех четырёх путях
(`cmd/cppworker/flash_attn_kv_guard_r367.go`) и отвечает 400 с причиной и
способом починки. Допустимые пары не тронуты: `auto (-1) + q4_0` (llama.cpp
включит flash attention сам) и `off + f16` (квантования нет).

### Живая проверка

```
PUT профиля: flashAttn=false, kvCacheType=q4_0
POST /api/models/load?wait=true → HTTP 400
{
  "error": "flash attention выключен (flashAttn=false), но kvCacheType=q4_0:
            llama.cpp требует включённый flash attention для квантованного
            KV-cache (в его логе: \"quantized V cache requires flash_attn to be
            enabled\"). Включите flashAttn в профиле модели или выберите
            kvCacheType=f16"
}
```

После возврата рабочего профиля (`flashAttn=true`, `kvCacheType=q4_0`)
модель грузится штатно: `ctx=32768, kv=q4_0, gpu=42/42`, генерация
**35.9 tok/s** (121 токен), чат через клиентский порт отвечает.

## 3. Известные ограничения (не исправлялось)

- **Профиль на воркере обновляется раз в 5 минут** (pull-sync,
  `sync_profile.go`). После `PUT` профиля на балансере воркер узнаёт о нём
  либо по `POST …/model-profiles/{name}/apply`, либо к следующему тику; в
  приёмке выше потребовался перезапуск воркера, потому что `apply` не
  обновляет кэш профилей (только применяет параметры к загрузке). Это
  отдельное поведение, не менялось.
- **Кэш KV на CPU** (`-nkvo`/`cache_type_k=v`) не проверяется: правило
  смотрит только на тип KV-cache целиком. Если llama.cpp научится квантовать
  K без flash attention, проверку придётся уточнить.
- **`useMmap`/`numa` из профиля** на legacy-пути по-прежнему не применяются
  (см. `2026-10-31-load-profile-legacy-path.md` §4).
- **R368** (сырой `/api/generate` изредка даёт `***`) не затронут и остаётся
  отдельной работой; к flash attention он отношения не имеет (проверено:
  воспроизводится при `flashAttn=true`).

## 4. Тесты

- `profile_flash_attn_r366_test.go` — матрица приоритета, живой случай
  «профиль перебивает env-дефолт -1», legacy-путь, стюард по всем путям;
- `flash_attn_kv_guard_r367_test.go` — матрица допустимых/запрещённых пар,
  текст ошибки (оба поля + способ починки), HTTP 400, стюард по всем путям;
- попутно в двух тестах ленивой загрузки (`lazyload_profile_r83_test.go`,
  `lazyload_profile_r228_test.go`) явно проставлен `FlashAttnType: -1`:
  реальный план всегда несёт значение флага/env, а ноль теперь означает
  «выключено» и с `q4_0` даёт законный отказ.

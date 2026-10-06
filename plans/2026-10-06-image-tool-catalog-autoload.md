# План: каталог image-моделей и загрузка по запросу через инструмент

**Дата:** 2026-10-06 · **Статус:** согласовано, к реализации · **Предыдущие шаги:** 0.7.9 (инструмент `generate_image`), 0.7.11 (политика сосуществования из WebUI)

## Зачем

Сейчас текстовая модель получает инструмент `generate_image` **только когда image-модель
уже загружена в VRAM** (гейт: «есть кому исполнять»). Она не знает, какие модели вообще
доступны, чем они отличаются и какие параметры под них ставить — то есть не может
осмысленно выбрать модель под запрос пользователя и поднять её сама.

Согласовано оператором: **разрешить инструменту самому загружать модель** и **отдавать
модели каталог** с параметрами и описаниями.

## Что уже есть (не переделывать)

| Возможность | Где |
| --- | --- |
| Runtime-настройки профиля из WebUI и применение к воркеру | `image-profiles.js`, `PUT /api/v1/image/model-profiles/{name}`, `POST …/{name}/apply` |
| Загрузка/выгрузка модели | `POST /api/v1/image/backends/{id}/models/load`, `…/unload`, `…/models/load/progress` |
| Снимок состояния image-бэкендов (модели, state, VRAM) | `internal/balancer/image_resources.go` |
| Схема инструмента (общая для контракта и инъекции) | `internal/imagetool/schema.go` |
| VRAM-гейт и политика сосуществования | `internal/balancer/image_resources.go`, `handlers_image_resources.go` |
| Пресеты | `config/image-model-catalog.json`, `GET /api/v1/image/model-catalog` |

## Контракты

### GET /api/v1/image/models/catalog

Один ответ по всем живым `image_cpp`-бэкендам — его же использует инструмент.

```json
{
  "backends": [{ "id": "imageworker", "status": "healthy", "state": "loaded", "currentModel": "sd15-q4" }],
  "models": [{
    "name": "sd15-q4", "backendId": "imageworker", "family": "sd15",
    "state": "not_loaded",           // loaded | loading | not_loaded | error
    "sizeBytes": 1566769658, "vramEstimateMb": 2600,
    "defaults": { "steps": 8, "cfgScale": 7, "width": 512, "height": 512, "sampler": "euler_a" },
    "strengths": "быстрая черновая генерация, 512×512 за секунды, малый VRAM",
    "notes": "SD1.5, all-in-one GGUF; для 8 ГБ — лучший выбор по скорости"
  }],
  "limits": { "minSide": 64, "maxSide": 4096, "sizeMultiple": 64, "minSteps": 1, "maxSteps": 100 }
}
```

Источники полей: профиль модели (`family`, `defaults`, `vramEstimateMb`), снимок воркера
(`state`, `sizeBytes`), каталог/профиль (`strengths`, `notes`). **Описания — данные, а не
код:** добавить в `types.ImageModelProfile` необязательное поле `notes`
(и/или `strengths`), чтобы WebUI-редактор профиля их правил, а каталог отдавал.

### Инструмент

- `generate_image`: добавить необязательный параметр `model` с `enum` из каталога и
  описанием «какую модель выбрать»; в описании инструмента — подсказка вызвать
  `list_image_models`, если модель неизвестна.
- Новый инструмент `list_image_models` (без параметров или с `family`): возвращает
  каталог в сжатом виде (имя, семейство, VRAM, время, сильные стороны, загружена ли).
- Флаг `LB_IMAGE_TOOL_ALLOW_LOAD` (по умолчанию `on`): разрешать ли вызову поднимать
  модель. При `off` — текущее поведение (только загруженная модель).

### Гейт объявления инструмента

Было: здоровый `image_cpp` **+ загруженная модель**.
Станет: здоровый `image_cpp` **+ есть что грузить** (модели в снимке) при
`LB_IMAGE_TOOL_ALLOW_LOAD=on`; при `off` — как раньше.

## Реализация

1. `pkg/types/image_model.go`: `ImageModelProfile.Notes` (+ при необходимости
   `Strengths`), тесты сериализации.
2. `internal/balancer/image_catalog.go` (новый): сборка каталога из снимков
   `imageResources` + профилей; кэш на 5 с (как `ensureFreshFor`).
3. `internal/api/handlers_image_models.go`: `GET /api/v1/image/models/catalog`
   (auth + rate-limit, как у остальных admin-путей).
4. `internal/imagetool/schema.go`: параметр `model` + описания; сборка схемы
   `list_image_models`.
5. `internal/balancer/image_tool.go`: гейт по новому правилу; `executeImageTool`
   при `state != loaded` → `POST /api/image/models/load` (sync через прогресс-поллинг),
   затем генерация; в tool-результат добавить `loadSeconds` и `modelState`.
6. `internal/balancer/image_tool_loop.go`: обработка нового инструмента
   `list_image_models` (результат — каталог, без генерации) в том же цикле.
7. `webui/js/modules/image-resources-policy.js` (или отдельная карточка): чекбокс
   «разрешить инструменту загружать модель» (через `LB_*`-настройку в `/api/v1/image/resources`),
   кнопка «Применить профиль к imageworker» + показ фактического argv `sd-server`.
8. Документация: `docs/image-generation.md` §16 «Каталог моделей и управление воркером
   из WebUI» + `docs/en/`, curl-примеры, таблица полей, сценарий «пользователь просит
   картинку → модель смотрит каталог → поднимает подходящую модель → генерирует».
9. CHANGELOG 0.7.12, README, релиз balancer+webui, живая проверка на стенде.

## Критерии приёмки

- [ ] `GET /api/v1/image/models/catalog` отдаёт модели со всех здоровых image-бэкендов с
      параметрами, VRAM-оценкой, состоянием и описанием.
- [ ] Инструмент объявляется при живом бэкенде и `ALLOW_LOAD=on` даже без загруженной
      модели; `off` возвращает прежнее поведение (проверяется тестом).
- [ ] Вызов `generate_image` с `model=X` при `X=not_loaded` поднимает модель (VRAM-гейт
      соблюдается), затем генерирует; в tool-результате есть время загрузки.
- [ ] Вызов `list_image_models` возвращает каталог и не тратит GPU.
- [ ] Живая проверка на стенде: gemma-4/Qwen3 → «нарисуй X» → модель сама выбирает и
      поднимает image-модель → картинка приходит ссылкой; в Monitor видны обе генерации
      (`surface=chat-tool`) и загрузка модели.
- [ ] Документация (ru+en) с примерами curl и таблицей полей каталога.

## Риски

- Загрузка модели занимает секунды-минуты: нужен явный таймаут
  (`LB_IMAGE_TOOL_LOAD_TIMEOUT_SEC`, по умолчанию 600) и честный текст ошибки в
  tool-сообщении, чтобы модель объяснила задержку пользователю.
- На 8 ГБ загрузка может отказать из-за VRAM-гейта — тогда в tool-результат идёт
  причина (`insufficient_vram`) и подсказка про политику/offload (см. §14.6).

# R250 — «Не использовать бэкенд» + много-групповость (план)

Статус: 🟢 пункт 1 сделан (R250), пункты 2–3 в работе. Основание: живая жалоба
оператора (R248/R249): нужно явно исключать бэкенд из выбора балансером без
остановки контейнера; нужна много-групповость «мастер ↔ доноры»; нужен
оффлайн-вход в WebUI.

## 1. Переключатель «не использовать бэкенд» — ✅ СДЕЛАНО (2026-10-28)

Что уже есть и на что опереться:
- `types.Backend` (pkg/types) — запись бэкенда; поле должно пережить рестарт,
  поэтому хранить рядом с ролями: runtime-overrides (см. `BackendRolesStore`,
  internal/api/handlers.go) или в config `balancing` (как `RPCEnabledOverride`).
- Выбор бэкенда: `internal/balancer/backend_selector.go`
  (`selectBackend`, `selectBackendExcluding`, `selectByResources`).
- Флаг RPC-тумблера как образец персиста: `internal/balancer/rpc_layout_override_r229.go`
  + `internal/api/handlers_backends.go` (`backendRPCLayout`).
- UI: `webui/js/modules/backend-merge.js` (таблица раскладки) — там уже есть
  колонка «Роль» и селект; тумблер рядом.

Шаги (сделано):
1. ✅ `types.Backend.Disabled` + `DisabledReason` + `DisabledAt`
   (`pkg/types/backend.go`); перенос в снапшот кластера —
   `types.BackendMetrics.Disabled/DisabledReason/DisabledSince`.
2. ✅ `Proxy.SetBackendDisabled` / `BackendDisabledInfo` / `LoadedModelNames`
   (`internal/balancer/backend_disable_r250.go`); фильтр в `expandCandidates`,
   `selectBackend` (включая мастера раскладки и репликацию),
   `selectByResources(Excluding)`, `findBackendWithModel`,
   `findLeastLoadedBackendWithModel`, `findLessLoadedBackend*`,
   `findFreeBackendForModelUnsafe`, `selectFreeBackendAny`,
   `findWarmingBackendForModelUnsafe`. Выключенный бэкенд НЕ исчезает из
   `GET /api/v1/backends`. Если выключены все — пустой выбор (прежнее
   поведение) + отдельная запись в лог.
3. ✅ API `GET/POST /api/v1/backends/{id}/enabled` с
   `{"enabled":false,"reason":"..."}`; ответ сразу, с `enabled`, `reason`,
   `since`, `loadedModels`, `persisted`. Персист — sidecar
   `runtime-overrides/backend-disabled.json` + восстановление в
   `LoadState` (state.json) + периодический возврат флага в пересобранные
   записи (как у ролей).
4. ✅ UI: колонка «В работе» с тумблером в таблице раскладки (выключенные
   приглушены и не выбираются), бейдж «исключён» + кнопка
   «Не использовать»/«Включить» в списке бэкендов, строка результата
   переключения (в R249 подсказка роли писалась в состояние, но не рисовалась).
5. ✅ Тесты: `internal/balancer/backend_disable_r250_test.go` (7),
   `internal/api/backend_enabled_r250_test.go` (7),
   `webui/js/modules/backend-merge-dom.test.js` (+5 JS).
   Попутно исправлен устаревший тест R95: после R249 «VRAM не хватает, но
   VRAM+RAM хватает» — это Warning `weights_need_ram_offload`, а не Error
   `vram_sum_insufficient` (`composition_r95_test.go`).

Живая проверка (2026-10-28, стенд `deployments/docker-compose.stack.yml`,
образы `balancer:r250` + `webui:r250`):
- `POST /api/v1/backends/cppworker-gpu-bundled-agent/enabled {"enabled":false}` →
  ответ за 66 мс: `enabled:false`, `disabled:true`, `persisted:true`,
  `since:2026-10-28T10:39:25Z`, `loadedModels:[]`;
- `GET /api/v1/backends` — узел НА МЕСТЕ, `disabled:true`, причина и дата видны
  рядом с `status:healthy` (именно этого не хватало: «выключен» и «сломан» —
  разные вещи);
- sidecar `/app/data/runtime-overrides/backend-disabled.json` записан;
- `POST /api/generate` в выключенном состоянии НЕ получил исполнителя: в логе
  балансера повторяется `no available backends with exclusions` (запрос ушёл в
  очередь/ожидание, а не на выключенный узел) — то есть узел действительно
  перестал получать запросы, хотя контейнер жив и в панели виден;
- `POST .../enabled {"enabled":true}` → флаг, причина и дата сняты, sidecar
  переписан на явное «включено», data plane снова 200;
- WebUI (nginx, `webui:r250`) отдаёт новый `backend-merge.js` (тумблер
  `bmDisabled`) и `polish.css` (`.bm-row-off`).
Откат: `BALANCER_TAG`/`WEBUI_TAG` в `deployments/.env` (резервная копия
`.env.bak-before-r250`) + `docker compose up -d loadbalancer webui`; образы
`balancer:r248`/`webui:r240` остались на месте.

## 2. Много-групповость «мастер ↔ доноры»

Сейчас: один список серверов раскладки на кластер (`layoutDonorServers()` в
`internal/balancer/rpc_layout_guard.go`, применение — `EnsureRPCLayout`).
Нужно: `BackendGroupConfig` уже групповой (id/model/members), но раскладка
применяется не по группе, а по кластеру.

Шаги:
1. В гарде брать доноров ИЗ группы модели (`backendGroups[].members` с ролью
   worker), а не глобальным списком.
2. `EnsureRPCLayout` — принимать groupID; при отсутствии группы вести себя как
   сейчас (обратная совместимость).
3. API `GET /api/v1/placement` уже отдаёт `backendGroups` — UI раскладки
   показать группу, к которой относится выбранный мастер.

## 3. Оффлайн-вход в WebUI по логину/паролю

Проверить: какие env читает auth-слой (`internal/api/auth_session.go`,
`webui/js/modules/auth.js`), почему работает только токен; нужен локальный
провайдер без внешнего IdP (хэш пароля из env + сессия), с тестом на пустой
внешний провайдер.

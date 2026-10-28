# R250 — «Не использовать бэкенд» + много-групповость (план)

Статус: 🟡 план. Основание: живая жалоба оператора (R248/R249): нужно явно
исключать бэкенд из выбора балансером без остановки контейнера; нужна
много-групповость «мастер ↔ доноры»; нужен оффлайн-вход в WebUI.

## 1. Переключатель «не использовать бэкенд» (первым)

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

Шаги:
1. `types.Backend.Disabled bool` + `DisabledReason string` (для аудита: почему
   выключен). Персист — рядом с ролями (тот же store, ключ `backend-disabled`).
2. `Proxy.SetBackendDisabled(id, disabled, reason) bool` + чтение в
   `selectBackend`/`selectByResources`/`expandCandidates`: выключенный бэкенд не
   кандидат, но ОСТАЁТСЯ в `GET /api/v1/backends` (иначе оператор его не найдёт).
3. API: `POST /api/v1/backends/{id}/enabled` с `{"enabled":false,"reason":"..."}`;
   ответ сразу (как R249: без блокирующих вызовов), с полями `enabled`,
   `loadedModels` (предупредить, что модель на нём сейчас работает).
4. UI: тумблер в таблице раскладки и в списке бэкендов; выключенные —
   приглушены и не выбираются; подсказка «исключён из выбора балансером».
5. Тесты: unit на селектор (выключенный не выбирается; если все выключены —
   прежнее поведение/понятная ошибка), JS — тумблер шлёт запрос и отражает ответ.

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

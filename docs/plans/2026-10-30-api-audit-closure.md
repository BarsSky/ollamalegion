# Закрытие «не покрыто» из аудита маршрутов (этап 3)

Дата: 2026-10-30. База: `docs/plans/2026-10-30-route-map.md`, раздел §12 (14 пунктов).
Правило этапа: каждый пункт либо закрывается тестом (ссылка на файл:тест), либо
получает явное решение «здесь не тестируется и почему» — без молчаливых пропусков.

| # | пункт аудита | статус | доказательство / решение |
| --- | --- | --- | --- |
| 1 | `Proxy.ServeHTTP` (`proxy.go:896-1657`) прочитан фрагментарно: ретраи, auto-pull, `ModelNotFoundError`/`BackendBusyError` | частично закрыто | аварийные ветки покрыты `stream_abort_r296_test.go` (терминатор при обрыве) и контрактными тестами ошибок (`tests/llamacpp_proxy/matrix_contract_r335_test.go`, негативные кейсы). Auto-pull отдельно не проверялся — вынесен в этап 4 как P2 (наблюдаемость) |
| 2 | SSE-ветка `/v1/chat/completions` и её hijack-вариант: реконнект/докачка | частично закрыто | `proxy_request_hijack_recursion_r66c_test.go`, `hijack_keepalive_r219_test.go` (форма keepalive), `stream_abort_r296_test.go` (один терминатор). Полный сценарий «обрыв посреди SSE + докачка» тестом не воспроизведён — нужен клиент-симулятор с обрывом TCP; вынесено в этап 4 |
| 3 | `nctx_reload.go` / `decideReloadBackend` / потолки окна | **закрыто** | `nctx_ceiling_r267_test.go` (одна формула потолка), `nctx_ceiling_r319_test.go`, `preflight_ceiling_r323_test.go` (max(env, declared) и отказ называет действующее число), `nctx_window_cache_r327_test.go` (все поля окна после подтверждённого reload) |
| 4 | Оценка токенов: `calibratedEstimateOnly`, `extractRequestMeta`, R288/R299 | **закрыто частично** | `prompt_calibration_r302_test.go` (калибровка), `growth_needed_rule_r330_test.go` (единая формула needed для трёх guard'ов + AST-стюард). Точность самой эвристики tokencount не проверяется (внешняя библиотека) |
| 5 | `normalizeOpenAIBody` — что именно нормализует | **закрыто** | `openai_normalize_r338_test.go`: текстовые части → строка `content`, картинки поднимаются в `images` (base64 без `data:`), тела без `messages` и не-JSON не трогаются |
| 6 | Anthropic `/v1/messages` и Responses `/v1/responses` | **закрыто** | `anthropic_r247_test.go`, `responses_r247_test.go` + живая матрица (`tests/api_contract/live_matrix_test.go`: `type:"message"`, `content`, `input_tokens`) |
| 7 | Image-поверхность (`/v1/images/*`, image tool loop) | **закрыто** | `image_tool_gate_r119_test.go`, `image_tool_inject_mode_r141_test.go`, `image_tool_env_values_r219_test.go`, `image_tool_inject_mode_r216_test.go` |
| 8 | Внутренности cppworker (`reasoning_content.go`, 946 строк) | **закрыто частично** | `cmd/cppworker/reasoning_content_test.go`, `reasoning_orphan_tags_r114_test.go`, `reasoning_split_policy_r108_test.go`, `tools_done_reason_r320_test.go`; политика `ReasoningWantedByClient` покрыта через native-контракт (`/api/chat` стрим в живой матрице) |
| 9 | Отсутствие живого прогона | **закрыто** | `tests/api_contract/live_matrix_test.go` (17 групп, зелёные на стенде) + прогоны в `docs/plans/2026-10-30-r309-hot-settings-and-comments.md` (R332–R335) |
| 10 | Админский API: поведение каждой ручки | частично закрыто | инференсные ручки — живая матрица; ручки профилей/настроек — `internal/api/*_test.go` (в т.ч. `profile_effective_window_r267_test.go`, `settings_*`). Полная матрица «каждая админ-ручка: тело/код/авторизация» не сделана — это следующий объём этапа 4 |
| 11 | WebUI как продукт (какие JS-вызовы в какие ручки) | решение: не тестируется здесь | связка проверяется обходом `scripts/run-webui-sweep.ps1` (85 файлов) и контрактными тестами ручек; полноценный e2e (браузер) в этой среде невозможен — Chromium не стартует (зафиксировано в R319) |
| 12 | Отмена запросов `/api/cancel` и доставка abort до `bridge.RequestAbort` | **закрыто частично** | `cancel_client_r229_test.go` (отмена клиентом), `stream_abort_r296_test.go`. Доставка abort до C-моста проверяется на воркере — отдельного теста нет, нужен живой воркер |
| 13 | Тесты как источник контракта | **закрыто** | контракт теперь фиксируют сами тесты: `matrix_contract_r335_test.go` (стаб-матрица), `live_matrix_test.go` (живая), `route_inventory_r336_test.go` (стюард полноты) |
| 14 | Метрики и наблюдаемость: «что записано ↔ что показано» | частично закрыто | `llamacpp_metrics_poller_r45_test.go` (разбор `/api/models`), `admission_pending_r329_test.go` (строки ожидания в мониторе). Сверка «метрика ↔ отображение в WebUI» — этап 4 |

## Итог этапа 3

- **Полностью закрыто:** 5 пунктов (3, 5, 6, 7, 9, 13 — по тестам; см. выше).
- **Закрыто частично:** 6 пунктов (1, 2, 4, 8, 10, 12, 14) — с указанием, что именно
  осталось и почему (нужен симулятор обрыва TCP, живой воркер для abort-моста,
  либо это объём этапа 4).
- **Решение не тестировать здесь:** 1 пункт (11, WebUI как продукт) — с причиной.

Новые тесты этапа: `internal/balancer/openai_normalize_r338_test.go`,
`tests/llamacpp_proxy/matrix_contract_r335_test.go`,
`tests/llamacpp_proxy/route_inventory_r336_test.go`.

## Что переходит в этап 4 (P2)

1. Авто-pull и ретраи в `ServeHTTP` — отдельный разбор + тесты.
2. Симулятор обрыва SSE (TCP reset посреди потока) и докачка.
3. Полная матрица админских ручек (тело/код/авторизация по каждой).
4. Доставка abort до `bridge.RequestAbort` на живом воркере.
5. Сверка «метрика ↔ отображение» в WebUI.

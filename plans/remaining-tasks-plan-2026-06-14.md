# План реализации оставшихся недоделок OllamaLegion

> **Дата:** 2026-06-14  
> **Источник:** Анализ текущей сессии, `docs/unfinished-code-audit.md`, `docs/backend-type-isolation-plan.md`, `docs/remaining-real-plan.md`, `docs/webui-gap-analysis.md`, `docs/refactoring-roadmap.md`, subagent-аудит кода.

## Легенда статусов

| Статус | Обозначение | Описание |
|--------|-------------|----------|
| ⬜ | Не начато | Задача ожидает выполнения |
| 🔄 | В процессе | Задача выполняется прямо сейчас |
| ✅ | Выполнено | Задача завершена и проверена |
| ⏸️ | Отложено | Задача отложена (блокировка, зависимость) |

---

## Сводка

| Этап | Название | Задач | Приоритет | Оценка времени | Статус |
|------|----------|-------|-----------|----------------|--------|
| 1 | Backend type isolation (верификация + доводка) | 4 | 🔴 P0 | 4–6 часов | ⬜ |
| 2 | Ollama API compatibility в CppWorker | 7 | 🔴 P0 | 10–14 часов | ⬜ |
| 3 | WebUI: управление моделями и агентами | 3 | 🟡 P1 | 4–6 часов | ⬜ |
| 4 | Тесты: устранение skip и flaky | 3 | 🟡 P1 | 3–4 часа | ⬜ |
| 5 | Windows GPU metrics (NVML/DXGI) | 1 | 🟢 P2 | 6–10 часов | ⬜ |
| 6 | Документация и skill-обновления | 2 | 🟢 P3 | 2–3 часа | ⬜ |
| **Итого** | | **20** | | **29–43 час** | |

---

## Этап 1: Backend type isolation (P0 🔴)

### Контекст
План `docs/backend-type-isolation-plan.md` утверждает, что фильтрация по `BackendType` в `selectBackend`/`expandCandidates` не реализована, однако subagent-аудит показал, что в коде уже есть `allowedTypes []types.BackendType` в `backend_selector.go` и `candidate.go`. Требуется верифицировать, что изоляция действительно работает end-to-end, и доделать защитные проверки.

### Задачи

#### 1.1 Верификация текущей фильтрации в `selectBackend`/`expandCandidates`
- **Файлы:** `internal/balancer/backend_selector.go`, `internal/balancer/candidate.go`
- **Действия:**
  - Проверить, что `selectBackend` принимает/определяет `allowedTypes` и передаёт во все подметоды.
  - Проверить, что `expandCandidates` фильтрует `p.backends` по `allowedTypes`.
  - Проверить, что llama.cpp-бэкенд не попадает в кандидаты для Ollama-запроса и наоборот.
- **Приёмка:** Unit-тест `TestExpandCandidates_FiltersByType` и `TestSelectBackend_MixedCluster_NoCrossContamination` проходят.

#### 1.2 Защита `proxyRequest()` от несовместимого типа
- **Файл:** `internal/balancer/proxy_request.go`
- **Действия:**
  - В начале `proxyRequest()` добавить проверку:
    ```go
    if !types.IsModeCompatibleWithBackendType(p.config.OperatingMode, state.Backend.Type) {
        return fmt.Errorf("backend type %s not compatible with operating mode %s", state.Backend.Type, p.config.OperatingMode)
    }
    ```
- **Приёмка:** Интеграционный тест `TestServeHTTP_OllamaRequest_RoutedToOllamaBackend` проходит.

#### 1.3 Защита `getBackendPort()` от CppWorker→Ollama fallback
- **Файл:** `internal/balancer/backend_state.go`
- **Действия:**
  - Для `EngineLlamaCPP` никогда не фоллбэчить на `OllamaPort`.
  - Если `CppWorkerPort == 0`, использовать `18091` и логировать WARN.
- **Приёмка:** Тест на backend с `EngineLlamaCPP` и непустым `OllamaPort` возвращает `18091`.

#### 1.4 Валидация типа бэкенда при CRUD
- **Файл:** `internal/api/handlers.go`
- **Действия:**
  - `handleAddBackend`: проверять совместимость `BackendType` с `OperatingMode`.
  - `handleUpdateBackend`: запретить смену типа, если `ActiveReqs > 0`.
- **Приёмка:** API возвращает 400 при попытке добавить Ollama-бэкенд в режим `virtual_router`.

#### 1.5 WebUI: бейджи и фильтры по backend-типу
- **Файлы:** `webui/js/modules/renderers.js`, `webui/js/monitor/ui-renderer.js`, `webui/css/custom.css`
- **Действия:**
  - Добавить бейджи 🦙/🦒 в `backendsTable()`, `capacityCard()`, `backendsPage()`, Monitor `renderBackendsTable()`, `renderModelsInMemory()`.
  - Добавить фильтры «Все | Ollama | llama.cpp» над таблицами Dashboard/Backends/Monitor.
- **Приёмка:** Вручную проверено в браузере.

#### 1.6 Тесты изоляции
- **Файл:** `tests/backend_type_isolation_test.go` (дополнить/создать)
- **Тесты:**
  - `TestSelectBackend_FiltersByType_OllamaOnly`
  - `TestSelectBackend_FiltersByType_LlamaCppOnly`
  - `TestSelectBackend_MixedCluster_NoCrossContamination`
  - `TestExpandCandidates_FiltersByType`
  - `TestServeHTTP_OllamaRequest_RoutedToOllamaBackend`
  - `TestServeHTTP_LlamaCppRequest_RoutedToLlamaCppBackend`
- **Приёмка:** `go test ./tests -run TestBackendType -count=1` PASS.

---

## Этап 2: Ollama API compatibility в CppWorker (P0 🔴)

### Контекст
CppWorker реализует базовый набор Ollama endpoint'ов, но `/api/generate` и `/api/chat` имеют partial-реализацию с fake-статистикой, неполным маппингом `options.*` и отсутствием `keep_alive`, `format`, `tools`, `images`.

### Задачи

#### 2.1 Достоверная статистика для `/api/generate`
- **Файл:** `cmd/cppworker/main.go`
- **Действия:**
  - Убрать хардкод `load_duration: 0`, `prompt_eval_count: 0`, `prompt_eval_duration: 0`.
  - Реализовать реальное измерение времени загрузки модели (`ensureModelLoaded`) и оценку prompt-токенов (через `backend.CountPromptTokens` или bridge).
- **Приёмка:** Ответ `/api/generate` содержит ненулевые `load_duration`/`prompt_eval_count` после первой загрузки.

#### 2.2 Полный маппинг `options.*` для `/api/generate`
- **Файл:** `cmd/cppworker/main.go` (`buildGenerationParams` / `handleOllamaGenerate`)
- **Действия:**
  - Добавить маппинг: `mirostat`, `mirostat_tau`, `mirostat_eta`, `typical_p`, `tfs_z`, `min_p`, `repeat_last_n`, `num_keep`, `num_gpu`, `main_gpu`, `stop`.
  - `seed == 0` трактовать как валидное значение (Ollama позволяет `0`).
- **Приёмка:** Тест `TestGenerateOptionsMapping` передаёт все опции и проверяет `bridge.GenerationParams`.

#### 2.3 Base64 `context` для follow-up запросов
- **Файл:** `cmd/cppworker/main.go` (`handleOllamaGenerate`)
- **Действия:**
  - Сохранять/восстанавливать KV-cache context между запросами.
  - Возвращать `context` как base64-encoded token IDs (или реальный KV-state).
- **Приёмка:** Два последовательных запроса с `context` корректно продолжают диалог.

#### 2.4 Поддержка `keep_alive`, `format`, `system`, `template`, `raw`
- **Файл:** `cmd/cppworker/main.go`
- **Действия:**
  - `keep_alive` — управление временем удержания модели в памяти.
  - `format` (json) — принудительный JSON-вывод.
  - `system`/`template`/`raw` — применять chat template из GGUF или naive fallback.
- **Приёмка:** Ollama-клиенты (OpenWebUI) корректно видят `system`-сообщения.

#### 2.5 Полная поддержка `/api/chat`
- **Файл:** `cmd/cppworker/main.go` (`handleChat`)
- **Действия:**
  - Поддержать `messages` с ролями `system`/`user`/`assistant`/`tool`.
  - Поддержать `options`, `stream`, `keep_alive`, `format`, `tools`.
  - Не конкатенировать сообщения в строку наивно — использовать `backend.ApplyChatTemplate`.
- **Приёмка:** Ollama `/api/chat` через балансировщик возвращает структурированный `message`.

#### 2.6 Улучшенный подсчёт токенов
- **Файл:** `cmd/cppworker/main.go`
- **Действия:**
  - Заменить `rune_count/4` на реальный подсчёт через bridge/llama.cpp tokenizer.
- **Приёмка:** `prompt_eval_count` близок к реальному числу токенов.

#### 2.7 Защита от повторной загрузки модели и утечки VRAM
- **Файл:** `cmd/cppworker/main.go`, `internal/cppbackend/backend.go`
- **Контекст:** При попытке протестировать загрузку модель с тем же `name` не выгружалась из VRAM, а вторая модель начинала загрузку в видеопамять поверх первой. Это приводит к OOM и неконсистентному состоянию.
- **Действия:**
  - В `handleLoadModel` перед вызовом `LoadModelWithOpts` проверять `backend.GetModel(modelName)`:
    - Если модель уже загружена и путь/параметры совпадают — вернуть `status: "already_loaded"`.
    - Если модель уже загружена, но путь или параметры отличаются — вызвать `UnloadModel` + `LoadModelWithOpts` (reload-in-place) вместо двойной загрузки.
  - В `ensureModelLoaded` добавить проверку не только по имени, но и по `Path`/`SizeBytes`/`ModTime`: если файл на диске изменился — перезагрузить модель.
  - В `LoadModelWithOpts` / `IsModelLoading` убедиться, что race-condition между двумя одновременными загрузками одной модели исключён (единый mutex/loading guard).
  - Убедиться, что `UnloadModel` действительно освобождает VRAM в C-bridge и не оставляет dangling handle.
- **Приёмка:**
  - Тест `TestLoadModel_DoubleLoadSameName_ReturnsAlreadyLoaded` — второй `/api/models/load` с тем же именем не грузит модель повторно.
  - Тест `TestLoadModel_ReplacedFile_UnloadsOldAndLoadsNew` — замена `.gguf` файла с тем же именем приводит к перезагрузке.
  - Ручная проверка: после двух последовательных `/api/models/load` с одним именем VRAM не растёт.
- **Приоритет:** 🔴 P0 (может привести к GPU OOM).

---

## Этап 3: WebUI — управление моделями и агентами (P1 🟡)

### Контекст
По `webui-gap-analysis.md` большинство WebUI реализовано. Остаются архитектурные и второстепенные gap'ы.

### Задачи

#### 3.1 Управление моделями на вкладке Models
- **Файлы:** `webui/index.html`, `webui/js/app.js`, `webui/js/modules/renderers.js`
- **Действия:**
  - Добавить кнопку «Manage Models» на вкладке Models.
  - Добавить `Load`/`Unload`/`Delete` в `modelsGrid`.
- **Приёмка:** Ручная проверка в браузере.

#### 3.2 Страница Agents
- **Файлы:** `webui/index.html`, `webui/js/modules/renderers.js`, `webui/js/modules/api.js`
- **Действия:**
  - Добавить вкладку Agents с таблицей из `/api/v1/agents/stats`.
  - Детали агента — `/api/v1/agents/{id}`.
- **Приёмка:** Вкладка открывается и показывает агентов.

#### 3.3 Agent→Ollama конфигурация (архитектурное)
- **Файлы:** `internal/agent/collector.go`, `internal/agent/ollama_config.go`, `internal/api/handlers.go`, `webui/js/modules/config-io.js`
- **Действия:**
  - В heartbeat response от балансировщика передавать желаемые Ollama runtime flags.
  - Агент применяет flags через env/файл конфигурации + restart.
  - UI в Settings для per-backend Ollama flags.
- **Приёмка:** Изменение флага в UI применяется на агенте после restart.
  - **Примечание:** Высокая сложность, может быть отложено.

---

## Этап 4: Тесты — устранение skip и flaky (P1 🟡)

### Задачи

#### 4.1 `tests/balancer_optimization_test.go:236`
- **Тест:** `TestSelectFreeBackendAny_Integration`
- **Проблема:** `t.Skip("Cannot parse mock server URLs")`
- **Действия:** Переписать на реальный proxy-интеграционный тест или удалить, если дублирует другие тесты.

#### 4.2 `tests/integration_test.go:708`
- **Тест:** `TestE2E_ProxyStreaming`
- **Проблема:** Условный skip при отсутствии порта в `httptest` URL.
- **Действия:** Сделать тест стабильным, добавить retry на соединение.

#### 4.3 `tests/proxy_streaming_test.go:638`
- **Тест:** `TestTransferEncoding_BackendReturns503ThenRecovers`
- **Проблема:** Условный skip при `connection refused`.
- **Действия:** Убрать skip, добавить wait-for-ready или deterministic mock.

#### 4.4 Windows stub тесты
- **Действия:** Добавить CI-скрипт/документацию по `go test -c ./cmd/cppworker -tags llama_stub -o cppworker_test.exe`.

---

## Этап 5: Windows GPU metrics (P2 🟢)

### Задача 5.1 Реализация NVML/DXGI для Windows
- **Файлы:** `internal/agent/nvml_windows.go`, `internal/agent/system_windows.go`
- **Действия:**
  - Реализовать `getGPUInfo()` через DXGI/WMI или `nvidia-smi`.
  - Реализовать `getNetworkIO()` — уже есть через `netstat -e` (проверить).
- **Приёмка:** Агент на Windows возвращает GPU/VRAM метрики.

---

## Этап 6: Документация и skill-обновления (P3 🟢)

### Задача 6.1 Актуализация `docs/backend-type-isolation.md`
- **Файл:** `docs/backend-type-isolation.md`
- **Действия:**
  - Описать текущую архитектуру типов, OperatingMode, flow запроса, WebUI индикаторы.
  - Отметить, что фильтрация реализована в `backend_selector.go`/`candidate.go`.

### Задача 6.2 Обновление `.clinerules`
- **Файл:** `.clinerules`
- **Действия:**
  - Добавить раздел по backend type isolation.
  - Добавить раздел по Ollama API gaps в CppWorker.
  - Добавить раздел по Windows stub тестам.
- **Статус:** Уже частично обновлён в текущей сессии.

---

## Порядок выполнения (рекомендуемый)

| Неделя | Задачи | Цель |
|--------|--------|------|
| **Неделя 1** | Этап 1.1–1.6 + Этап 4.1–4.4 | Стабильная изоляция типов + зелёные тесты |
| **Неделя 2** | Этап 2.1–2.4, 2.7 | Достоверная статистика, полный маппинг generate и защита от повторной загрузки |
| **Неделя 3** | Этап 2.5–2.6 + Этап 3.1–3.2 | Полный chat и UI для моделей/агентов |
| **Неделя 4** | Этап 5 + Этап 6 + Этап 3.3 (по возможности) | Windows metrics, документация, agent config |

---

## Связь с предыдущими планами

| Этап этого плана | Соответствие в `implementation-plan-2026-05-05-remaining.md` |
|------------------|-------------------------------------------------------------|
| 1.1–1.6 | Backend type isolation (новый план) |
| 2.1–2.7 | Расширение Ollama API для llama.cpp backend + защита от повторной загрузки |
| 3.1–3.2 | WebUI Models/Agents |
| 3.3 | Agent→Ollama config (архитектурный долг) |
| 4.1–4.4 | Исправление пропущенных тестов |
| 5.1 | Windows GPU metrics |
| 6.1–6.2 | Документация и `.clinerules` |

---

> **Примечание:** Этот план является консолидацией актуальных недоделок после реализации Ollama API в CppWorker. Первый приоритет — backend type isolation и полнота Ollama API, так как они влияют на корректность работы балансировщика с cppworker-gpu.
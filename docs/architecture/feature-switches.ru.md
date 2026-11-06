# Переключатели возможностей: что включено, что выключено, что теряется

Дата: 2026-11-02. Зачем файл: половина «пропавшего» функционала — это не
сломанный код, а **флаг, который по умолчанию выключен** или **политика,
которая режим заменяет**. Оператор видит «не работает», разработчик — «так и
задумано».

Правило: каждый `LB_*`/`CPPWORKER_*`, который читает код, обязан быть в этом
файле. Проверяет стюард
`internal/balancer/feature_switches_steward_test.go` (переменная без записи
здесь или запись без переменной в коде — падение CI).

## А. Опасные умолчания: функционал есть, но по умолчанию выключен

Если вам кажется, что «фича не работает» — сначала проверьте этот список.

| Переключатель | Умолчание | Что не работает, пока выключено |
| --- | --- | --- |
| `LB_AUTO_CONTINUE_ON_TRUNCATION` | **выкл** | авто-продолжение обрезанного ответа (модель не «дотягивает» финальный блок; клиент получает обрезанный ответ как есть) |
| `LB_OPENAI_AUTO_STREAM` | **выкл** | non-stream запросы `/v1` ждут заголовки до таймаута, вместо «внутренний стрим, клиенту один JSON» |
| `CPPWORKER_RPC_REQUIRE_VERSION` | **выкл** | узел раскладки с другим коммитом llama.cpp допускается в раскладку (протокол RPC версионно-чувствителен — риск abort'а) |
| `LB_RESOURCE_LIMITS_HARD_BLOCK` | **выкл** | жёсткая блокировка по VRAM/RAM — мягкая: предупреждение в лог, но запрос пропускается |
| `LB_STREAM_FIRST_FRAME_ABORT_SEC` | **0 = выкл** | тишина ДО первого байта не обрывается: клиент ждёт свой таймаут (а не наш) |
| `CPPWORKER_VISION_MMPROJ` | **выкл** | capability vision не публикуется, даже если mmproj загружен (картинки → 415) |
| `CPPWORKER_ENABLE_BATCHED_PARALLEL` | **выкл** | параллельные слоты сериализуются legacy-путём (`parallel=1` для клиента) |
| `LB_ALLOW_REQUEST_TIMEOUTS` | **выкл** | per-model таймауты запросов не применяются (инференс без ограничения времени) |
| `LB_ALLOW_PROFILE_TIMEOUTS` | **выкл** | таймауты из профиля модели игнорируются |
| `LB_ALLOW_NCTX_RELOAD_TIMEOUT` | **выкл** | таймаут перезагрузки окна не применяется |
| `LB_ALLOW_LOAD_WAIT_CAP_SEC` | **выкл** | верхний предел ожидания загрузки не применяется |

## Б. Что включено по умолчанию и может «мешать» оператору

| Переключатель | Умолчание | Поведение |
| --- | --- | --- |
| `LB_OLLAMA_NATIVE_PATH` | **вкл** | нативная поверхность Ollama: ответ воркера идёт как есть (без перевода OpenAI→Ollama) |
| `LB_OLLAMA_DONE_CONTENT` | **вкл** (снятие дубля) | из финального кадра снимается текст, уже отданный дельтами; `=keep` возвращает как есть |
| `LB_NCTX_GROW_FOR_AGENTS` | **вкл** | агентский запрос без `num_ctx` получает окно с резервом вывода (`LB_AGENT_OUTPUT_RESERVE_TOKENS`) |
| `LB_NCTX_GROW_FOR_STREAM` | **вкл** (R385) | стриминговый запрос с большим окном поднимает модель перезагрузкой |
| `LB_LAYOUT_HEAL` | **вкл** (R383) | автолечение вырожденной раскладки (перезапуск узла + reload) |
| `LB_AUTO_LOAD_ASYNC` | **вкл** | авто-загрузка асинхронная: клиент получает 503+Retry-After, а не ждёт загрузку |
| `LB_NCTX_RELOAD_ASYNC` | **вкл** | перезагрузка окна асинхронная (503+Retry-After) |
| `CPPWORKER_RPC_SESSION_AUTO_UNLOAD` | **вкл** | потеря сессии RPC выгружает модель на мастере (иначе висит «загружена» без узла) |
| `LB_ADMISSION_KEEPALIVE_SEC` | **вкл (5 c)** | байты keepalive, пока запрос ждёт слот; `0` — молчать |
| `LB_STREAMING_NEVER_TIMEOUT` | **вкл** | стрим без общего таймаута (защита — idle-таймаут и сторож тишины) |
| `LB_AUTO_TUNE` | **вкл** | AutoTune может перезагружать модель под план |
| `LB_RPC_PROBE_CACHE_SEC` | **вкл (20 c)** | результат пробы узлов кэшируется; `0` — пробить каждый раз |
| `LB_LOG_BUFFER_ENTRIES` | 2000 | сколько лог-записей держит брокер (для /ws/logs и выгрузки файла); больше 20000 обрезается |
| `CPPWORKER_UNPARSED_CAPTURE_FILE` | `/app/diag/unparsed-tool-calls.jsonl` (R427) | сохраняется СЫРОЙ вывод модели в нераспознанном формате вызова — для разбора «почему инструмент не вызван». Пишет файлы на диск (до 512 КБ на случай, кольцо из 5): путь задаётся здесь, содержимое смотрится через `GET /api/v1/cppworker/debug/unparsed` (`?clear=1` — очистить) |
| `CPPWORKER_DEBUG_PROMPT_FILE` | **выкл** (R442) | путь (например `/app/diag/prompt-last.txt`) — писать ПОЛНЫЙ собранный промпт последнего запроса. Нужно, чтобы доказывать по байтам, что доехало до модели (результат инструмента, дубли контента). Содержит переписку — включать адресно |
| `CPPWORKER_DEBUG_REQUEST_FILE` | **выкл** (R442) | путь (например `/app/diag/request-last.json`) — писать тело запроса клиента после декодирования (`messages`, `tools`). Пара к предыдущему: видно, что прислал клиент и что мы отправили в модель |
| `CPPWORKER_TOOLS_THINKING` | `auto` (R470) | отдавать ли клиенту РАЗМЫШЛЕНИЯ на пути с `tools[]`. `auto` — только если клиент попросил явно (`think:true`); `on` — прежнее поведение (эхо всегда), `off` — не отдавать никогда. Живой повод: Cline присылает `tools[]` без `think`, а `message.thinking` он сохраняет как ТЕКСТ сообщения ассистента — в истории следующего хода размышления предыдущего выглядят как наш ответ, и модель продолжает «планировать вслух», смешивая языки. Сам разбор вывода и промпт не меняются: размышления по-прежнему генерируются и учитываются в бюджете |
| `CPPWORKER_CLIENT_HINTS` | **вкл** (R445) | добавлять в system короткую инструкцию по ОТКАЗАМ клиента, которые уже лежат в истории (`role:"tool"`): файл существует → сначала `read_files`, затем `editor` с точным `old_text`; `new_text` больше лимита клиента (~6000) → дробить на несколько вызовов. `0` — выключить. Сами результаты инструментов не меняются, подсказка только помогает слабой модели их применить |
| `CPPWORKER_DEBUG_TOOL_ARGS` | **выкл** (R440) | `1` — писать в лог исходящие аргументы `tool_call` (имя, форма, длина, первые 400 символов). Нужно, когда клиент отвергает вызов («expected string, received undefined → at new_text»), а по сводке видна только форма аргументов. Содержит содержимое файлов пользователя — включать по необходимости |
| `CPPWORKER_TEMPLATE_TOOLS`' | **native** (R434) | `native` — tools[] уходят в chat template, и если шаблон нарисовал секцию инструментов, текстовая преамбула НЕ добавляется (модель видит формат вызова, которому обучена); `prompt` — прежнее поведение (только текстовая преамбула). Рубильник на случай шаблона, который от инструментов портит промпт |

## В. Сетевые/поведенческие пороги (значения по умолчанию)

| Переключатель | Умолчание | Смысл |
| --- | --- | --- |
| `LB_FIRST_BYTE_TIMEOUT_DEFAULT_SEC` | 180 | сколько ждать первый байт стрима, если профиль/env не задали иное |
| `LB_STREAMING_IDLE_TIMEOUT_SEC` | резолвер (120 c) | пауза между кадрами, после которой стрим считается зависшим |
| `LB_REQUEST_TIMEOUT_SEC` | 0 = без лимита | общий таймаут не-стримингового запроса |
| `LB_MAX_REQUEST_BODY_MB` | см. код | предел тела запроса |
| `LB_AUTO_LOAD_WAIT_SEC` | 900 | сколько клиент ждёт авто-загрузку модели |
| `LB_BACKEND_WAIT_SEC` | 120 | R403: сколько ждёт свободный **бэкенд** (все offline/disabled/доноры), FIFO по модели; `0` = сразу 503 |
| `LB_NCTX_PREFLIGHT_WAIT_SEC` | см. код | ожидание preflight перед ростом окна |
| `LB_AGENT_OUTPUT_RESERVE_TOKENS` | 4096 | резерв вывода при расчёте окна для агентов |
| `LB_AGENT_WINDOW_BUDGET` | `minimal` | R451: что входит в «нужное окно» при росте под агентский ход. `minimal` — только минимальный осмысленный вывод (256): `max_tokens` клиента — «сколько я готов принять», а не «сколько модели нужно места». `client` — прежнее поведение (промпт + весь бюджет клиента), платит перезагрузками (живой случай: `max_tokens=15393` раздул окно до 40134 при загруженных 32768 → unload+load 243 с, хотя промпт влезал) |
| `LB_ADMISSION_KEEPALIVE_SEC` | 5 | см. раздел Б |
| `LB_GHOST_BACKEND_TTL_SEC` | 1800 | через сколько пропавший бэкенд удаляется |
| `LB_RPC_NODE_PORT` | 50052 | порт узла раскладки |
| `CPPWORKER_RPC_READY_WAIT_SEC` | см. R215 | сколько ждать готовности узла перед загрузкой |
| `CPPWORKER_LOAD_STALL_SEC` | см. код | застой загрузки модели |
| `CPPWORKER_LOAD_FLIGHT_WAIT_SEC` | см. код | ожидание «загрузка в полёте» между запросами |
| `CPPWORKER_IDLE_UNLOAD_MINUTES` | 10 | выгрузка простаивающей модели |
| `CPPWORKER_N_PARALLEL` | 1 | слотов на модель (window делится между слотами!) |
| `CPPWORKER_MAX_PARALLEL_PER_USER` | см. код | предел параллельных запросов одного пользователя |
| `CPPWORKER_MAX_N_PREDICT` | 16384 (R463) | верхний предел бюджета ответа, который воркер поднимает САМ (для reasoning-моделей — до `CPPWORKER_DEFAULT_N_PREDICT_REASONING`, 8192). Правило одностороннее: бюджет только ПОВЫШАЕТСЯ (клиентский `num_predict`/`max_tokens` не понижается), поэтому `0`/несознанное значение не ломает не-reasoning модели. Живой повод: `max_output_tokens` исторически мог только уменьшать бюджет, и reasoning-модель на `/api/chat` обрывала размышления на 2048 токенах («модель сгенерировала только размышления») |
| `CPPWORKER_VRAM_OVERHEAD_MB` | см. код | запас VRAM, вычитаемый из бюджета |
| `LB_AUTOTUNE_APPLY` | **recommend** (R444) | может ли AutoTune применять свой план (`слои`/`KV`/`окно`) САМ. `recommend` — план только показывается (панель, `POST /api/v1/admin/autotune/{id}/apply` — вручную); `auto` — прежнее поведение: reload запускается автоматически. Живой повод: агентский цикл Cline платил 43–67 с за unload+load перед каждым ходом, потому что план строился по сиюминутной свободной VRAM |
| `LB_AUTOTUNE_KV_MODE`' | `recommend` | R419: может ли AutoTune САМ менять тип KV-cache. `recommend` — только рекомендация в панели (применение вручную); `auto` — прежнее поведение с профилем и гистерезисом R416. Меняется без рестарта |

## Г. Инфраструктурные и параметрические переменные

Две категории в одном списке:

- **сантехника** (токены, пути, порты, сборка) — значение задаётся окружением
  контейнера;
- **параметры загрузки модели** (`CPPWORKER_CTX_SIZE`, `*_GPU_LAYERS`,
  `CPPWORKER_KV_CACHE_TYPE`, `CPPWORKER_FLASH_ATTN*`, `*_N_PARALLEL`,
  `*_USE_MMAP`, `*_NO_KV_OFFLOAD`, RoPE/YaRN и т.п.) — это ДЕФОЛТ ОКРУЖЕНИЯ,
  который перекрывается профилем модели и явным параметром запроса (порядок
  R364: запрос → профиль → env). Проверять «почему окно не то» надо сверху
  вниз по этому порядку, а не только в `.env`.

Присутствие каждого имени в этом файле проверяется стюардом.

- `CPPWORKER_API_TOKEN`
- `CPPWORKER_AUTO_GPU_DIST`
- `CPPWORKER_AVAILABLE_RAM_BYTES`
- `CPPWORKER_BATCH_SIZE`
- `CPPWORKER_CACHE_DIR`
- `CPPWORKER_CONFIG_DIR`
- `CPPWORKER_CTX_SIZE`
- `CPPWORKER_DEFAULT_CTX_SIZE`
- `CPPWORKER_DEFAULT_ENABLE_REASONING`
- `CPPWORKER_DEFAULT_REASONING_BUDGET`
- `CPPWORKER_DOWNLOADS_DIR`
- `CPPWORKER_ENABLE_METRICS`
- `CPPWORKER_ENABLE_REASONING`
- `CPPWORKER_FLASH_ATTN`
- `CPPWORKER_FLASH_ATTN_TYPE`
- `CPPWORKER_GPU_LAYERS`
- `CPPWORKER_HOST`
- `CPPWORKER_KV_CACHE_TYPE`
- `CPPWORKER_LLAMA_BUILD`
- `CPPWORKER_LLAMA_BUILD_DATE`
- `CPPWORKER_MAIN_GPU`
- `CPPWORKER_METRICS_PORT`
- `CPPWORKER_MODELS_DIR`
- `CPPWORKER_N_THREADS`
- `CPPWORKER_NO_KV_OFFLOAD`
- `CPPWORKER_NO_MEMORY_MAP`
- `CPPWORKER_NUMA`
- `CPPWORKER_PORT`
- `CPPWORKER_REASONING_BUDGET`
- `CPPWORKER_RMS_NORM_EPS`
- `CPPWORKER_ROPE_FREQ_BASE`
- `CPPWORKER_ROPE_FREQ_SCALE`
- `CPPWORKER_ROPE_SCALING_FACTOR`
- `CPPWORKER_ROPE_SCALING_TYPE`
- `CPPWORKER_RPC_BACKEND`
- `CPPWORKER_RPC_EXPECT_BUILD`
- `CPPWORKER_RPC_EXTERNAL_ADDR`
- `CPPWORKER_RPC_SERVER_BIN`
- `CPPWORKER_RPC_SERVER_BIND`
- `CPPWORKER_RPC_SERVER_DEBUG`
- `CPPWORKER_RPC_SERVER_ENABLED`
- `CPPWORKER_RPC_SERVER_PORT`
- `CPPWORKER_RPC_SERVER_RESTART`
- `CPPWORKER_TENSOR_SPLIT_STRATEGY`
- `CPPWORKER_USE_MLOCK`
- `CPPWORKER_USE_MMAP`
- `CPPWORKER_YARN_ATTN_FACTOR`
- `CPPWORKER_YARN_BETA_FAST`
- `CPPWORKER_YARN_BETA_SLOW`
- `CPPWORKER_YARN_EXT_FACTOR`
- `LB_ADMIN_ENV_SYNC`
- `LB_ADMIN_PASSWORD`
- `LB_ADMIN_USER`
- `LB_API_TOKEN`
- `LB_AUTO_CONTINUE_CHAT_POLICY`
- `LB_AUTO_CONTINUE_MAX_TOKENS`
- `LB_AUTO_CONTINUE_TIMEOUT_SEC`
- `LB_LLAMA_UPSTREAM_REPO`
- `LB_PORT`
- `LB_RPC_LINK_MBPS`
- `LB_RPC_LINK_MEASURE_SEC`
- `LB_RPC_LINK_MEASURED_AT`
- `LB_RPC_LINK_SOURCE`
- `LB_SELF_BASE_URL`
- `LB_SESSIONS_FILE`
- `LB_USERS_FILE`

### cmd

- `CPPWORKER_ADVERTISE_HOST`
- `CPPWORKER_ADVERTISE_PORT`
- `CPPWORKER_AUTO_KV_CACHE`
- `CPPWORKER_AUTO_OFFLOAD`
- `CPPWORKER_AUTO_TUNE_NCTX`
- `CPPWORKER_AUTO_TUNE_NCTX_ON_LOAD`
- `CPPWORKER_BALANCER_TOKEN`
- `CPPWORKER_BALANCER_URL`
- `CPPWORKER_CPU_ONLY_MAX_NCTX`
- `CPPWORKER_DEBUG_TOOL_PROMPT`
- `CPPWORKER_DEFAULT_N_PREDICT_REASONING`
- `CPPWORKER_EMPTY_OUTPUT_RETRY`
- `CPPWORKER_FALLBACK_ESTIMATED_LAYERS`
- `CPPWORKER_FALLBACK_KV_RESERVE_MB`
- `CPPWORKER_FALLBACK_OVERHEAD_MB`
- `CPPWORKER_FALLBACK_SAFETY_FACTOR`
- `CPPWORKER_FREE_VRAM_BYTES`
- `CPPWORKER_LAZY_LOAD_TIMEOUT_SEC`
- `CPPWORKER_LOAD_WAIT_SEC`
- `CPPWORKER_RAM_FALLBACK_ALLOW_TOOLS`
- `CPPWORKER_RAM_FALLBACK_GPU_LAYERS`
- `CPPWORKER_RAM_FALLBACK_MAX_N_CTX`
- `CPPWORKER_RAM_FALLBACK_N_CTX`
- `CPPWORKER_READ_TIMEOUT`
- `CPPWORKER_REASONING_ARCHS`
- `CPPWORKER_REGISTER_DISABLE`
- `CPPWORKER_REGISTER_GPU_MODE`
- `CPPWORKER_REGISTER_HEARTBEAT`
- `CPPWORKER_REGISTER_MAX_RETRIES`
- `CPPWORKER_REGISTER_NAME`
- `CPPWORKER_REGISTER_RETRY_INTERVAL`
- `CPPWORKER_SPLIT_MODE`
- `CPPWORKER_STRICT_ARGS_RETRY`
- `CPPWORKER_TOOLS_STREAM_CONTENT`
- `CPPWORKER_TOOLS_THINKING`
- `CPPWORKER_UNPARSED_CAPTURE_FILE`
- `CPPWORKER_VRAM_BYTES`
- `CPPWORKER_WRITE_TIMEOUT`
- `LB_ALGORITHM`
- `LB_ASYNC_WORK_SHUTDOWN_WAIT_SEC`
- `LB_AUTH_TOKENS`
- `LB_DATA_DIR`
- `LB_MODEL_AFFINITY`
- `LB_SESSION_STICKINESS`
- `LB_SHUTDOWN_DRAIN_SEC`

## Д. Что уже управляется из WebUI (реестр горячих настроек)

Панель **«Горячие настройки»** (`GET/PUT /api/v1/settings`) рисует реестр
целиком: значение, источник (панель → env → умолчание) и признак «применяется
без рестарта». Поэтому выключатель, зарегистрированный в реестре, появляется в
UI автоматически — отдельного кода панели не нужно.

Зарегистрировано (на 2026-11-02): 18 настроек R278 + 5 поведенческих R388:

| Настройка | Горячая | Умолчание |
| --- | --- | --- |
| `LB_LAYOUT_HEAL` | да | 1 (вкл) |
| `LB_NCTX_GROW_FOR_STREAM` | да | 1 (вкл) |
| `LB_OPENAI_AUTO_STREAM` | да | 0 (выкл) |
| `LB_AUTO_CONTINUE_ON_TRUNCATION` | да | 0 (выкл) |
| `LB_OLLAMA_NATIVE_PATH` | **нет** (только с рестартом) | 1 (вкл) |

Остальные переключатели из разделов А–Г в реестр не входят: они читаются
`os.Getenv` при старте/на каждом запросе и меняются только через env и
пересоздание контейнера. Панель их не показывает — и это честнее, чем кнопка,
которая «не применяется».
## Е. Переменные контейнера WebUI (не балансера)

WebUI — статика + nginx-прокси; учётные данные он не читает. Ему нужны только
адрес балансера и токен:

| Переменная | Умолчание | Смысл |
| --- | --- | --- |
| `LB_API_URL` | нет | адрес балансера `http://host:18081`; R391: читается entrypoint (раньше передавался, но игнорировался) |
| `API_HOST` / `API_PORT` | `loadbalancer` / `18081` | то же по частям; имя `loadbalancer` существует только в compose-сети |
| `API_TOKEN` | пусто | токен для проверки доступа и аварийного входа; fallback `WEBUI_API_TOKEN` → `LB_API_TOKEN` → `BALANCER_API_TOKEN` |
| `LB_ADMIN_USER` / `LB_ADMIN_PASSWORD` | — | **здесь НЕ читаются**: учётки принадлежат балансеру, см. docs/operations/webui-auth.ru.md |
## Ж. Отдельный агент Ollama-бэкенда (`ollama-agent`)

Своя идентичность и порт, чтобы не пересекаться с встроенным агентом воркера
(18034) и legacy-агентом (18032): `AGENT_ID=ollama-agent`, `AGENT_PORT=18036`,
`BACKEND_TYPE=ollama`, `NODE_LABELS=gpu,ollama`. Адрес бэкенда берётся из
`OLLAMA_URL` (R392), а не из адреса агента. Подробности и проверка —
`docs/operations/ollama-agent.ru.md`.
## Как это связано с правилами оператора

`docs/architecture/operator-rules.ru.md` фиксирует **требования** (что должно
работать), а этот файл — **переключатели** (чем это можно выключить). Новый
флаг без записи здесь или правило без теста роняют CI, поэтому «потерялось»
больше не означает «никто не заметил».

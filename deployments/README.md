# deployments/ — что здесь лежит

**Канонический файл — `docker-compose.stack.yml`.** Всё остальное в этой папке
либо историческое, либо узкоспециальное. Если вы разворачиваете стек впервые —
читайте [../README.md](../README.md) и [../docs/deployment-stack.md](../docs/deployment-stack.md),
а из этой папки вам нужны только два файла:

```bash
cp .env.example .env                          # один раз
docker compose -f docker-compose.stack.yml --profile full up -d
```

## Профили stack.yml

| Профиль | Что поднимает | Когда |
|---|---|---|
| `full` | cppworker + imageworker + balancer + webui | всё на одной машине |
| `worker` | cppworker + imageworker | эта машина — только бэкенд, балансер на другой |
| `balancer` | balancer + webui | эта машина — управление, воркеры подключаются сами |
| `legacy-agent` | внешний `agent` | только для **Ollama**-бэкендов; воркерам он не нужен |

## Переменные окружения

| Файл | Для чего |
|---|---|
| `.env.example` → `.env` | **основной источник**: интерполяция `${...}` в compose, токен, теги образов |
| `.env.bundled-with-agent.example` | `env_file` для старых compose (модели, GPU, лимиты). `environment:` в compose переопределяет его — см. docs/deployment-stack.md §6 |
| `.env.bundled-full.example`, `.env.bundled.example`, `.env.llama.example`, `.env.cppworker-remote.example` | под свои исторические compose-файлы |
| `.env` и остальные `.env.*` без `.example` | **локальные**, в git не попадают (там токены). Восстанавливаются из `.example` |

## Остальные compose-файлы

Полная таблица «файл → назначение → статус» — в
[docs/deployment-stack.md §9](../docs/deployment-stack.md#9-остальные-compose-файлы-в-папке).
Коротко о том, что чаще всего ищут:

| Файл | Что это |
|---|---|
| `docker-compose.agent.yml` | только агент (в stack то же самое — `--profile legacy-agent`) |
| `docker-compose.agent.gpu.yml` | **оверлей** к `agent.yml`, не самостоятельный: `-f docker-compose.agent.yml -f docker-compose.agent.gpu.yml` |
| `docker-compose.test-stub.yml` | стаб-стенд для тестов; им пользуется `playwright.config.js` |
| `docker-compose.cppworker.yml` | ⚠️ не проходит `compose config` (пустой `networks:` у `cppworker-stub`) — исторический |
| `docker-compose.full.yml`, `…bundled-full.yml`, `…cppworker-bundled*.yml`, `…cppworker-with-agent*.yml`, `…llama.*.yml`, `…rpc.yml`, `…cocoindex.yml` | исторические |

## Локальные (не в git) файлы

| Что | Почему здесь |
|---|---|
| `data/` | рабочая директория балансера при локальном запуске; перечислена в `.gitignore` и специально исключается из релизных архивов (`scripts/release-sources.ps1`) |
| `.env*` без `.example` | ваши токены и адреса |
| `.env*.bak*` | резервные копии env; под них отдельное правило в `.gitignore`, потому что в них те же секреты |

# Запуск стека «с нуля»: один скрипт вместо прямого compose

Короткий порядок для чистой машины, где образов `ollama-legion/*` ещё нет.
Сценарии, профили и порты — в [deployment-stack.ru.md](deployment-stack.ru.md).

## Почему нельзя просто `docker compose up -d`

На машине без образов `up -d` уходит в сборку и падает на «страже идентичности»
(R486): Dockerfile'ы требуют build-arg'и `GIT_COMMIT`, `GIT_COMMIT_FULL`,
`BUILD_DATE`, а cppworker — ещё `LLAMA_BUILD` и `RPC_PROTO`. В `deployments/.env`
этих значений нет и быть не может: `env_file` на build-args не действует, а
`.git` в контекст сборки не попадает. Значения считает
`scripts/build-identity.ps1` из git, и передать их можно только окружением
процесса. Это и делает `scripts/up-stack.ps1` (bash-близнец —
`scripts/up-stack.sh`).

## Порядок

```bash
git clone --recurse-submodules <repo> ollamalegion   # сабмодуль c/llama.cpp обязателен
cd ollamalegion
cp deployments/.env.example deployments/.env         # один раз
# в deployments/.env: CPPWORKER_API_TOKEN=<свой токен> и CUDA_ARCH=<под вашу карту>
pwsh -File scripts/up-stack.ps1                      # или: ./scripts/up-stack.sh
```

Скрипт проверяет git, `deployments/.env` и docker, считает идентичность сборки
(коммит, дата, ветка, версия), показывает, каких образов нет, и запускает
`docker compose -f deployments/docker-compose.stack.yml --profile full up -d`.
Compose собирает только отсутствующие образы, поэтому повторный запуск ничего не
пересобирает.

## Проверка здоровья

```bash
docker ps --format '{{.Names}}\t{{.Status}}' | grep ol-stack
curl -s http://localhost:18081/api/v1/health
```

Клиентский API — порт `18080`, admin API и метрики — `18081`, панель WebUI — `18083`.

## Полезные варианты

| Задача | Команда |
|---|---|
| образы уже собраны — не собирать | `pwsh -File scripts/up-stack.ps1 -NoBuild` (bash: `--no-build`) |
| проверочная сборка одного сервиса | `./scripts/up-stack.sh --build-only --services webui --tag local-check-webui` |
| показать идентичность и команду | `./scripts/up-stack.sh --dry-run` |
| git недоступен (архив исходников) | `--git-commit <sha> --git-commit-full <sha>` |

## Частые отказы

- `git не найден в PATH` — поставьте git или укажите коммит флагами выше.
- `нет deployments/.env` — скопируйте `deployments/.env.example` и задайте токен.
- `CUDA_VERSION не знает sm_120` — при `CUDA_ARCH` с 100/120 задайте
  `CUDA_VERSION=12.8.0` или уберите 100/120 из `CUDA_ARCH`.

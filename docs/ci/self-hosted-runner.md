# Self-Hosted CI Runner — настройка и эксплуатация

> **Версия:** 2026-06-28 (roadmap 7.1a)
> **Целевая машина:** Windows 11 + Go 1.21+ + Docker Desktop + Node.js 18+ (для `scripts/i18n_diff.js`)
> **Репозиторий:** `BarsSky/ollamalegion`

---

## Зачем self-hosted runner

GitHub Actions self-hosted runner нужен для OllamaLegion **с самого начала CI**, а не только для GPU-тестов (7.2). Причины:

| Фактор | GitHub-hosted | Self-hosted (эта машина) |
|---|---|---|
| **CGo + C-bridge** | Нужен CMake, требует 5-10 мин cold-cache | Persistent cache, 5-10 сек incremental |
| **`c/llama.cpp` subtree** | Нет в checkout по умолчанию, нужно `submodules: recursive` | Уже на диске, не нужно тащить |
| **Windows nvml build tag** (`internal/agent/nvml_unix.go`) | Невозможно на Linux runner | ✅ Windows-native |
| **Custom GPU stack** (CUDA toolkit, MSVC) | Не установлен на GitHub-hosted | ✅ Уже стоит |
| **Disk space** | 14 GB SSD на runner, ephemeral | Полный диск |
| **Время на setup job** | 30-60 сек (apt-get install) | 1 сек (уже всё есть) |
| **Стоимость** | Бесплатно для public repo, лимиты для private | Бесплатно (ваша машина) |

**Итог:** для stub-only CI (7.1) self-hosted runner — **оптимально** уже сейчас. Без него CI будет медленным и flaky на Windows-специфичных тестах.

---

## Prerequisites

Перед запуском `setup-runner.ps1` убедитесь:

### 1. Системные требования

| Компонент | Минимум | Рекомендуется | Проверка |
|---|---|---|---|
| **ОС** | Windows 10 21H2 | Windows 11 23H2 | `winver` |
| **Go** | 1.21.0 | 1.22.x | `go version` |
| **Docker Desktop** | 4.20+ | 4.30+ | `docker --version` |
| **Node.js** | 18 LTS | 20 LTS | `node --version` |
| **Git** | 2.40+ | latest | `git --version` |
| **CMake** (для 7.2 GPU) | 3.25+ | latest | `cmake --version` |
| **RAM** | 8 GB | 16 GB+ (для llama.cpp build) | Task Manager |
| **Disk** | 50 GB свободно | 100 GB+ | Explorer |
| **PowerShell** | 5.1 | 7.x (`pwsh`) | `$PSVersionTable.PSVersion` |

### 2. Права администратора

`setup-runner.ps1` устанавливает Windows service — **нужен запуск от администратора**.

### 3. GitHub Personal Access Token (PAT)

Создайте PAT: <https://github.com/settings/tokens/new>

**Scopes (минимальные):**
- ✅ `repo` (для repo-level runner'ов)
- ✅ `admin:org` (для org-level runner'ов, если хотите шарить)
- ✅ `workflow` (для workflow файлов)

**Срок:** 90 дней (custom), затем ротация.

**⚠️ Безопасность:** не коммитьте PAT в репо. Используйте:
- `gh auth login` + `gh auth token` (более безопасно, scope из `gh` CLI)
- GitHub Actions secrets (для workflow)
- Windows Credential Manager (для локального кэша)

---

## Установка (5 минут)

### Шаг 1: Запуск от администратора

```powershell
# В PowerShell (от Administrator):
cd C:\Ollama\ollamalegion
.\scripts\setup-runner.ps1
```

Скрипт попросит:
1. GitHub PAT (или передайте через `-GitHubToken`)
2. Подтвердит установку service

### Шаг 2: Альтернативный запуск (CI-friendly)

Для CI-автоматизации (например, на новой dev-машине):

```powershell
# Передать PAT через env
$env:GITHUB_TOKEN = "ghp_xxx..."
.\scripts\setup-runner.ps1 -Unattended -GitHubToken $env:GITHUB_TOKEN
```

### Шаг 3: Проверка

```powershell
.\scripts\check-runner.ps1
```

**Ожидаемый вывод:**
```
=== 2/6 Self-hosted runner service ===
  ✅ Service: actions.runner.BarsSky-ollamalegion.knaga-ci — Status: Running
  ✅ Runner config: C:\actions-runner\.runner
    Agent: knaga-ci
    Pool:  Default
    URL:   https://github.com/BarsSky/ollamalegion
```

### Шаг 4: Проверить в GitHub UI

Откройте: <https://github.com/BarsSky/ollamalegion/settings/actions/runners>

Должен появиться runner `<hostname>-ci` с зелёной точкой и меткой `Idle`.

---

## Метки (Labels) и их использование

Self-hosted runner регистрируется с метками:

| Метка | Назначение | Workflow |
|---|---|---|
| `self-hosted` | Маркер self-hosted (стандарт GH) | Все workflow могут использовать |
| `windows` | ОС runner'а | `runs-on: windows` |
| `ollamalegion-ci` | Кастомный тег проекта | `runs-on: [self-hosted, windows, ollamalegion-ci]` |

В workflow файлах:

```yaml
jobs:
  build:
    # Только self-hosted Windows runner с проектом
    runs-on: [self-hosted, windows, ollamalegion-ci]
    steps:
      - uses: actions/checkout@v4
      - run: go build -tags llama_stub ./cmd/balancer/

  build-fallback:
    # Fallback на GitHub-hosted если self-hosted оффлайн
    runs-on: ubuntu-latest
    steps:
      - run: go build -tags llama_stub ./cmd/balancer/
```

---

## Безопасность

Self-hosted runner = код из PR'ов выполняется на **вашей машине**. Это потенциальный supply-chain риск.

### Меры защиты (применены в `setup-runner.ps1`)

1. **Запуск от администратора** — service имеет полный доступ к системе, но это неизбежно для Windows service.
2. **GitHub Token** — используется только для регистрации, не хранится на диске.
3. **Work directory isolation** — `C:\actions-runner\_work` изолирован от остальной файловой системы.
4. **No external network** — runner не открывает входящих портов, только исходящие к `github.com:443`.

### Дополнительные рекомендации

#### 1. Только для своего репо (по умолчанию)

```yaml
# В workflow — фильтр на свой репо
on:
  pull_request:
    types: [opened, synchronize]
jobs:
  build:
    if: github.event.pull_request.head.repo.full_name == 'BarsSky/ollamalegion'
    runs-on: [self-hosted, windows, ollamalegion-ci]
    steps: [...]
```

Это **не пускает** код из форков в self-hosted runner.

#### 2. Ephemeral runner (рекомендуется для CI)

Вместо persistent service — запускать runner в Docker-контейнере на каждый job:

```yaml
jobs:
  build:
    runs-on: ubuntu-latest
    container:
      image: ghcr.io/barssky/ollamalegion-ci-runner:latest
    steps:
      - uses: actions/checkout@v4
      - run: go build -tags llama_stub ./cmd/balancer/
```

Но это требует создания отдельного Docker-образа с runner'ом внутри (см. `docker/ci-runner/Dockerfile` — TODO).

#### 3. Изоляция на отдельной VM

Для production — поднять runner на отдельной VM (не на dev-машине). Стоимость: $5-20/мес на Hetzner/DigitalOcean.

---

## Обслуживание

### Обновление runner'а

GitHub выпускает обновления runner'а ~1 раз в 2-4 недели. Чтобы обновить:

```powershell
# Остановить service
Stop-Service "actions.runner.BarsSky-ollamalegion.knaga-ci"

# Скачать новую версию (замените X.Y.Z на актуальную)
cd C:\actions-runner
Invoke-WebRequest -Uri "https://github.com/actions/runner/releases/download/vX.Y.Z/actions-runner-win-x64-X.Y.Z.zip" -OutFile update.zip
Expand-Archive update.zip -DestinationPath . -Force

# Запустить service
Start-Service "actions.runner.BarsSky-ollamalegion.knaga-ci"
```

### Логи

```powershell
# Последние 20 событий
Get-EventLog -LogName Application -Source "Actions Runner" -Newest 20

# Живой поток (новые события)
Get-EventLog -LogName Application -Source "Actions Runner" -Newest 1 -Wait

# Лог runner'а напрямую
Get-Content C:\actions-runner\_diag\Runner_*.log -Tail 50 -Wait
```

### Очистка диска

Runner со временем накапливает кэш:

```powershell
# Очистка Go build cache (>1 GB обычно)
go clean -cache
go clean -modcache

# Очистка старых work directories
Get-ChildItem C:\actions-runner\_work\_tool -Directory -ErrorAction SilentlyContinue |
    Where-Object { $_.LastWriteTime -lt (Get-Date).AddDays(-30) } |
    Remove-Item -Recurse -Force
```

### Удаление runner'а

```powershell
# Через setup-runner.ps1 (если доступен)
.\scripts\setup-runner.ps1 -Uninstall  # TODO: добавить

# Или вручную
cd C:\actions-runner
.\svc.cmd stop
.\svc.cmd uninstall
.\config.cmd remove --token <REG_TOKEN>  # получите новый через API
Remove-Item C:\actions-runner -Recurse -Force
```

---

## Troubleshooting

### `Service 'actions.runner.*' не найден`

**Причина:** runner не установлен или service удалён.

**Решение:**
```powershell
.\scripts\setup-runner.ps1 -GitHubToken "<PAT>"
```

### `❌ Go : НЕ НАЙДЕН` (но `go version` работает)

**Причина:** PATH не содержит Go (часто при первом запуске после установки).

**Решение:**
```powershell
# Проверить PATH
$env:PATH -split ";" | Select-String -Pattern "Go"

# Если пусто — добавить Go в PATH
[Environment]::SetEnvironmentVariable("Path", $env:Path + ";C:\Program Files\Go\bin", "User")
# Перезапустить PowerShell
```

### `Work directory: нет прав на запись`

**Причина:** service запущен от другого пользователя.

**Решение:**
```powershell
# Проверить аккаунт service
Get-WmiObject Win32_Service -Filter "Name LIKE 'actions.runner%'" | Select-Object Name, StartName

# Переустановить service от текущего пользователя
cd C:\actions-runner
.\svc.cmd stop
.\svc.cmd uninstall
.\svc.cmd install
.\svc.cmd start
```

### `GitHub API: 401 Unauthorized`

**Причина:** rate limit (60 req/hour для unauthenticated).

**Решение:** передайте PAT в скрипт:
```powershell
.\scripts\check-runner.ps1 -GitHubToken "<PAT>"  # TODO: добавить параметр
```

### Runner зарегистрирован, но GitHub показывает "Offline"

**Причина:** service запущен, но firewall блокирует исходящие.

**Решение:**
```powershell
# Проверить, может ли runner достучаться до GitHub
Test-NetConnection -ComputerName github.com -Port 443

# Если блокировано — добавить правило firewall
New-NetFirewallRule -DisplayName "GitHub Actions Runner" -Direction Outbound -RemotePort 443 -Protocol TCP -Action Allow
```

---

## Дальнейшее развитие (TODO)

- [ ] `scripts/uninstall-runner.ps1` — полное удаление
- [ ] `docker/ci-runner/Dockerfile` — ephemeral runner в контейнере
- [ ] `docs/ci/github-hosted-fallback.md` — dual-runner конфигурация
- [ ] `docs/ci/performance-tuning.md` — кэш-стратегии, parallel jobs
- [ ] `scripts/check-runner.ps1 -GitHubToken` — авторизация для rate limit

---

## Связанные документы

- [`plans/2026-q3-roadmap.md`](../../plans/2026-q3-roadmap.md) — §7.1a, §7.1, §7.2.
- [`plans/2026-q3-production-ready-plan.md`](../../plans/2026-q3-production-ready-plan.md) — P.4 CI/CD.
- [GitHub Actions: Self-hosted runners](https://docs.github.com/en/actions/hosting-your-own-runners) — официальная документация.
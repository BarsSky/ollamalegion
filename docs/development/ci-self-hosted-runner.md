# Self-Hosted CI Runner — Setup and Operation

> **Version:** 2026-06-28 (roadmap 7.1a)
> **Target machine:** Windows 11 + Go 1.21+ + Docker Desktop + Node.js 18+ (for `scripts/i18n_diff.js`)
> **Repository:** `BarsSky/ollamalegion`

---

## Why a self-hosted runner

A GitHub Actions self-hosted runner is needed for OllamaLegion **from the very start of CI**, not only for the GPU tests (7.2). Reasons:

| Factor | GitHub-hosted | Self-hosted (this machine) |
|---|---|---|
| **CGo + C-bridge** | Needs CMake, takes 5-10 min with a cold cache | Persistent cache, 5-10 sec incremental |
| **`c/llama.cpp` subtree** | Not in the checkout by default, `submodules: recursive` is required | Already on disk, no need to pull it |
| **Windows nvml build tag** (`internal/agent/nvml_unix.go`) | Impossible on a Linux runner | ✅ Windows-native |
| **Custom GPU stack** (CUDA toolkit, MSVC) | Not installed on GitHub-hosted | ✅ Already installed |
| **Disk space** | 14 GB SSD on the runner, ephemeral | Full disk |
| **Setup job time** | 30-60 sec (apt-get install) | 1 sec (everything is already there) |
| **Cost** | Free for a public repo, limits for a private one | Free (your machine) |

**Bottom line:** for stub-only CI (7.1) a self-hosted runner is **optimal** right now. Without it, CI will be slow and flaky on Windows-specific tests.

---

## Prerequisites

Before running `setup-runner.ps1`, make sure of the following:

### 1. System requirements

| Component | Minimum | Recommended | Check |
|---|---|---|---|
| **OS** | Windows 10 21H2 | Windows 11 23H2 | `winver` |
| **Go** | 1.21.0 | 1.22.x | `go version` |
| **Docker Desktop** | 4.20+ | 4.30+ | `docker --version` |
| **Node.js** | 18 LTS | 20 LTS | `node --version` |
| **Git** | 2.40+ | latest | `git --version` |
| **CMake** (for 7.2 GPU) | 3.25+ | latest | `cmake --version` |
| **RAM** | 8 GB | 16 GB+ (for the llama.cpp build) | Task Manager |
| **Disk** | 50 GB free | 100 GB+ | Explorer |
| **PowerShell** | 5.1 | 7.x (`pwsh`) | `$PSVersionTable.PSVersion` |

### 2. Administrator rights

`setup-runner.ps1` installs a Windows service — **it must be run as administrator**.

### 3. GitHub Personal Access Token (PAT)

Create a PAT: <https://github.com/settings/tokens/new>

**Scopes (minimal):**
- ✅ `repo` (for repo-level runners)
- ✅ `admin:org` (for org-level runners, if you want to share)
- ✅ `workflow` (for workflow files)

**Lifetime:** 90 days (custom), then rotate.

**⚠️ Security:** do not commit the PAT to the repo. Use:
- `gh auth login` + `gh auth token` (safer, the scope comes from the `gh` CLI)
- GitHub Actions secrets (for workflows)
- Windows Credential Manager (for the local cache)

---

## Installation (5 minutes)

### Step 1: Run as administrator

```powershell
# In PowerShell (as Administrator):
cd C:\Ollama\ollamalegion
.\scripts\setup-runner.ps1
```

The script will ask for:
1. A GitHub PAT (or pass it via `-GitHubToken`)
2. Confirmation of the service installation

### Step 2: Alternative run (CI-friendly)

For CI automation (for example, on a new dev machine):

```powershell
# Pass the PAT via env
$env:GITHUB_TOKEN = "ghp_xxx..."
.\scripts\setup-runner.ps1 -Unattended -GitHubToken $env:GITHUB_TOKEN
```

### Step 3: Verification

```powershell
.\scripts\check-runner.ps1
```

**Expected output:**
```
=== 2/6 Self-hosted runner service ===
  ✅ Service: actions.runner.<owner>-<repo>.<runner-name> — Status: Running
  ✅ Runner config: C:\actions-runner\.runner
    Agent: <runner-name>
    Pool:  Default
    URL:   https://github.com/BarsSky/ollamalegion
```

### Step 4: Check the GitHub UI

Open: <https://github.com/BarsSky/ollamalegion/settings/actions/runners>

A runner named `<hostname>-ci` should appear with a green dot and the `Idle` label.

---

## Labels and their usage

A self-hosted runner registers with the labels:

| Label | Purpose | Workflow |
|---|---|---|
| `self-hosted` | Self-hosted marker (GH standard) | All workflows can use it |
| `windows` | The runner's OS | `runs-on: windows` |
| `ollamalegion-ci` | Custom project tag | `runs-on: [self-hosted, windows, ollamalegion-ci]` |

In workflow files:

```yaml
jobs:
  build:
    # Only a self-hosted Windows runner with the project
    runs-on: [self-hosted, windows, ollamalegion-ci]
    steps:
      - uses: actions/checkout@v4
      - run: go build -tags llama_stub ./cmd/balancer/

  build-fallback:
    # Fallback to GitHub-hosted if the self-hosted runner is offline
    runs-on: ubuntu-latest
    steps:
      - run: go build -tags llama_stub ./cmd/balancer/
```

---

## Security

A self-hosted runner = code from PRs is executed on **your machine**. This is a potential supply-chain risk.

### Protection measures (applied in `setup-runner.ps1`)

1. **Run as administrator** — the service has full access to the system, but that is unavoidable for a Windows service.
2. **GitHub Token** — used only for registration, not stored on disk.
3. **Work directory isolation** — `C:\actions-runner\_work` is isolated from the rest of the filesystem.
4. **No external network** — the runner opens no inbound ports, only outbound to `github.com:443`.

### Additional recommendations

#### 1. Own repo only (default)

```yaml
# In the workflow — filter to your own repo
on:
  pull_request:
    types: [opened, synchronize]
jobs:
  build:
    if: github.event.pull_request.head.repo.full_name == 'BarsSky/ollamalegion'
    runs-on: [self-hosted, windows, ollamalegion-ci]
    steps: [...]
```

This **does not let** code from forks onto the self-hosted runner.

#### 2. Ephemeral runner (recommended for CI)

Instead of a persistent service — run the runner in a Docker container for every job:

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

But this requires building a separate Docker image with the runner inside (see `docker/ci-runner/Dockerfile` — TODO).

#### 3. Isolation on a separate VM

For production — host the runner on a separate VM (not on the dev machine). Cost: $5-20/month on Hetzner/DigitalOcean.

---

## Maintenance

### Updating the runner

GitHub releases runner updates about once every 2-4 weeks. To update:

```powershell
# Stop the service
Stop-Service "actions.runner.<owner>-<repo>.<runner-name>"

# Download the new version (replace X.Y.Z with the current one)
cd C:\actions-runner
Invoke-WebRequest -Uri "https://github.com/actions/runner/releases/download/vX.Y.Z/actions-runner-win-x64-X.Y.Z.zip" -OutFile update.zip
Expand-Archive update.zip -DestinationPath . -Force

# Start the service
Start-Service "actions.runner.<owner>-<repo>.<runner-name>"
```

### Logs

```powershell
# The last 20 events
Get-EventLog -LogName Application -Source "Actions Runner" -Newest 20

# Live stream (new events)
Get-EventLog -LogName Application -Source "Actions Runner" -Newest 1 -Wait

# The runner log directly
Get-Content C:\actions-runner\_diag\Runner_*.log -Tail 50 -Wait
```

### Disk cleanup

Over time the runner accumulates cache:

```powershell
# Clean the Go build cache (usually >1 GB)
go clean -cache
go clean -modcache

# Clean old work directories
Get-ChildItem C:\actions-runner\_work\_tool -Directory -ErrorAction SilentlyContinue |
    Where-Object { $_.LastWriteTime -lt (Get-Date).AddDays(-30) } |
    Remove-Item -Recurse -Force
```

### Removing the runner

```powershell
# Via setup-runner.ps1 (if available)
.\scripts\setup-runner.ps1 -Uninstall  # TODO: add

# Or manually
cd C:\actions-runner
.\svc.cmd stop
.\svc.cmd uninstall
.\config.cmd remove --token <REG_TOKEN>  # get a new one via the API
Remove-Item C:\actions-runner -Recurse -Force
```

---

## Troubleshooting

### `Service 'actions.runner.*' не найден` (not found)

**Cause:** the runner is not installed or the service has been removed.

**Solution:**
```powershell
.\scripts\setup-runner.ps1 -GitHubToken "<PAT>"
```

### `❌ Go : НЕ НАЙДЕН` (not found — but `go version` works)

**Cause:** PATH does not contain Go (often on the first run after installation).

**Solution:**
```powershell
# Check PATH
$env:PATH -split ";" | Select-String -Pattern "Go"

# If empty — add Go to PATH
[Environment]::SetEnvironmentVariable("Path", $env:Path + ";C:\Program Files\Go\bin", "User")
# Restart PowerShell
```

### `Work directory: no write permission`

**Cause:** the service runs under a different user.

**Solution:**
```powershell
# Check the service account
Get-WmiObject Win32_Service -Filter "Name LIKE 'actions.runner%'" | Select-Object Name, StartName

# Reinstall the service under the current user
cd C:\actions-runner
.\svc.cmd stop
.\svc.cmd uninstall
.\svc.cmd install
.\svc.cmd start
```

### `GitHub API: 401 Unauthorized`

**Cause:** rate limit (60 req/hour for unauthenticated requests).

**Solution:** pass a PAT to the script:
```powershell
.\scripts\check-runner.ps1 -GitHubToken "<PAT>"  # TODO: add the parameter
```

### The runner is registered, but GitHub shows "Offline"

**Cause:** the service is running, but the firewall is blocking outbound traffic.

**Solution:**
```powershell
# Check whether the runner can reach GitHub
Test-NetConnection -ComputerName github.com -Port 443

# If blocked — add a firewall rule
New-NetFirewallRule -DisplayName "GitHub Actions Runner" -Direction Outbound -RemotePort 443 -Protocol TCP -Action Allow
```

---

## Future work (TODO)

- [ ] `scripts/uninstall-runner.ps1` — complete removal
- [ ] `docker/ci-runner/Dockerfile` — an ephemeral runner in a container
- [ ] `docs/ci/github-hosted-fallback.md` — dual-runner configuration
- [ ] `docs/ci/performance-tuning.md` — cache strategies, parallel jobs
- [ ] `scripts/check-runner.ps1 -GitHubToken` — authorization for the rate limit

---

## Related documents

- [`docs/plans/2026-q3-roadmap.md`](../plans/2026-q3-roadmap.md) — §7.1a, §7.1, §7.2.
- [`docs/plans/2026-q3-production-ready-plan.md`](../plans/2026-q3-production-ready-plan.md) — P.4 CI/CD.
- [GitHub Actions: Self-hosted runners](https://docs.github.com/en/actions/hosting-your-own-runners) — official documentation.

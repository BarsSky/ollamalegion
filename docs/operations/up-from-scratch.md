# Bringing the stack up from scratch: one script instead of raw compose

A short procedure for a clean machine that has no `ollama-legion/*` images yet.
Scenarios, profiles and ports — in [deployment-stack.md](deployment-stack.md).

## Why plain `docker compose up -d` is not enough

On a machine without images `up -d` starts building and fails on the identity
guard (R486): the Dockerfiles require the `GIT_COMMIT`, `GIT_COMMIT_FULL` and
`BUILD_DATE` build args, and cppworker also needs `LLAMA_BUILD` and `RPC_PROTO`.
`deployments/.env` does not contain these values and cannot: `env_file` has no
effect on build args, and `.git` never enters the build context. The values are
computed from git by `scripts/build-identity.ps1`, and they can only be passed
through the process environment. That is exactly what `scripts/up-stack.ps1`
does (the bash twin is `scripts/up-stack.sh`).

## Order

```bash
git clone --recurse-submodules <repo> ollamalegion   # the c/llama.cpp submodule is required
cd ollamalegion
cp deployments/.env.example deployments/.env         # once
# in deployments/.env: CPPWORKER_API_TOKEN=<your token> and CUDA_ARCH=<your GPU>
pwsh -File scripts/up-stack.ps1                      # or: ./scripts/up-stack.sh
```

The script checks git, `deployments/.env` and docker, computes the build identity
(commit, date, branch, version), shows which images are missing, and runs
`docker compose -f deployments/docker-compose.stack.yml --profile full up -d`.
Compose builds only the missing images, so a second run rebuilds nothing.

## Health check

```bash
docker ps --format '{{.Names}}\t{{.Status}}' | grep ol-stack
curl -s http://localhost:18081/api/v1/health
```

The client API is on port `18080`, the admin API and metrics on `18081`, the
WebUI dashboard on `18083`.

## Useful variants

| Task | Command |
|---|---|
| images already built — do not build | `pwsh -File scripts/up-stack.ps1 -NoBuild` (bash: `--no-build`) |
| test build of a single service | `./scripts/up-stack.sh --build-only --services webui --tag local-check-webui` |
| show identity and command only | `./scripts/up-stack.sh --dry-run` |
| git unavailable (source archive) | `--git-commit <sha> --git-commit-full <sha>` |

## Common failures

- `git not found in PATH` — install git or pass the commit flags above.
- `no deployments/.env` — copy `deployments/.env.example` and set the token.
- `CUDA_VERSION does not know sm_120` — with 100/120 in `CUDA_ARCH` set
  `CUDA_VERSION=12.8.0` or drop 100/120 from `CUDA_ARCH`.

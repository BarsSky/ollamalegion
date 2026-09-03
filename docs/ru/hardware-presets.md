# Hardware Presets (R58.2, 2026-09-03)

> Готовые пресеты настроек для типичных GPU. Закрывает user pain: «не удобен
> для обычного пользователя при попытке настроить для работы на другом
> железе (большем по мощности)».

## Доступные пресеты

| Preset | VRAM | sm_arch | Где | Когда использовать |
|--------|------|---------|-----|---------------------|
| `rtx30-8gb.json` | 8 GB | 86 | RTX 3050/3060/3070/3080 8GB | Маленькие модели (3B-7B), агрессивный RAM fallback |
| `rtx40-24gb.json` | 16-24 GB | 89 | RTX 4080 16GB, RTX 4090 24GB | Средние модели (7B-13B), 70B partial offload |
| `a10-24gb.json` | 24 GB | 86 | NVIDIA A10/A10G (AWS G5, Lambda, RunPod) | Cloud deploy, sm_86 = no rebuild |
| `rtx50-32gb.json` | 32 GB | 120 | RTX 5090 | Самые большие модели (70B Q4, 405B partial), нужен rebuild для sm_120 |

## Как применить preset

### Вариант 1 — ручная копипаста (5 минут)
Откройте `config/hardware-presets/<preset>.json`, скопируйте значения в
ваш `deployments/.env.bundled-with-agent` (или в свой `.env`):

```bash
# Пример для RTX 30xx 8GB:
cat config/hardware-presets/rtx30-8gb.json | \
    jq -r '.docker, .cppworker, .ram_fallback, .auto, .balancer | to_entries | .[] | "\(.key)=\(.value)"' \
    >> deployments/.env.bundled-with-agent

# Потом пересоберите образ если менялся CUDA_ARCH:
cd deployments && docker compose -f docker-compose.cppworker-bundled-with-agent.yml \
    --env-file .env.bundled-with-agent build cppworker
docker compose -f docker-compose.cppworker-bundled-with-agent.yml \
    --env-file .env.bundled-with-agent up -d
```

### Вариант 2 — через `scripts/apply-hardware-preset.py` (1 минута)

```bash
python scripts/apply-hardware-preset.py rtx30-8gb
# Generated deployments/.env.bundled-with-agent from rtx30-8gb.json
#   CUDA_ARCH=86
#   CPPWORKER_GPU_TAG=86-abort-r35
#   CPPWORKER_CTX_SIZE=32768
#   ... 15 lines
#
# Next: rebuild cppworker if CUDA_ARCH changed:
#   cd deployments && docker compose -f docker-compose.cppworker-bundled-with-agent.yml \
#       --env-file .env.bundled-with-agent build cppworker
```

### Вариант 3 — через WebUI (Phase 1.3b, ещё не реализовано)
Setup Wizard → "Apply Hardware Preset" → dropdown → select RTX 4090 →
preset values загружены в форму → Save.

## Что меняет preset

Каждый preset включает:

1. **Docker build args**:
   - `CUDA_ARCH` — для image rebuild
   - `CPPWORKER_GPU_TAG` — какой image использовать

2. **CPPWORKER runtime params**:
   - `CPPWORKER_CTX_SIZE` — max n_ctx
   - `CPPWORKER_GPU_LAYERS` — сколько слоёв на GPU (99 = all)
   - `CPPWORKER_KV_CACHE_TYPE` — f16 (быстрый) vs q4_0 (экономит VRAM)
   - `CPPWORKER_BATCH_SIZE` — размер батча

3. **RAM fallback cascade** (3-tier):
   - `CPPWORKER_RAM_FALLBACK_N_CTX` — включить каскад
   - `CPPWORKER_RAM_FALLBACK_GPU_LAYERS` — сколько слоёв оставить на GPU
   - `CPPWORKER_RAM_FALLBACK_MAX_N_CTX` — max n_ctx даже при fallback

4. **Balancer tuning**:
   - `LB_NCTX_RELOAD_MAX_N_CTX` — max n_ctx для auto-reload
   - `LB_NCTX_RELOAD_VRAM_SAFETY_FACTOR` — сколько VRAM держать свободным

## Когда НЕ использовать preset

Presets — это **точка старта**, не финальная конфигурация. Каждое железо
и use case уникальны. После применения preset:

1. Загрузите ваши модели через WebUI (`/gguf` page → `Add` → выберите GGUF)
2. AutoTune (R54.x) автоматически подстроит `n_ctx`, `kv_cache_type`,
   `num_gpu_layers` для каждой модели
3. Если модель не помещается — preset's `RAM_FALLBACK_*` активируется

## FAQ

**Q: У меня RTX 3060 12GB. Какой preset?**
A: 12GB — между 8GB и 16GB. Начните с `rtx30-8gb.json`, потом вручную
   увеличьте `CPPWORKER_GPU_LAYERS` если RAM не используется.

**Q: A100 40GB / 80GB?**
A: Эти пресеты не покрывают A100. Используйте `rtx50-32gb.json` как стартовую
   точку, измените `CUDA_ARCH=80` (A100) и увеличьте `CPPWORKER_CTX_SIZE` до 131072.

**Q: H100 80GB?**
A: Same as A100 path. `CUDA_ARCH=90` (Hopper). Preset values для KV cache
   и batch нужно удвоить vs 32GB tier.

**Q: Я хочу добавить свой preset. Как?**
A: Скопируйте `rtx40-24gb.json`, переименуйте, обновите значения.
   Скрипт `apply-hardware-preset.py` подхватит автоматически (читает из
   `config/hardware-presets/*.json`).

## Источник значений

Все значения получены из реальных деплоев:
- RTX 3070 8GB (BarsSky, 2026-08) — production baseline
- RTX 4090 24GB (тесты, 2026-08) — для batched inference моделей
- A10 24GB (Round 35c notes, 2026-08-13) — cloud deploy
- RTX 5090 32GB (спецификации + аналогия с RTX 4090) — не тестировалось в production

Если preset работает плохо на вашем железе — откройте issue с логами
`cppworker-load.log` + `balancer.log`, мы обновим preset на основе данных.

---

## R58.3: Setup Wizard dropdown (WebUI)

В R58.3 добавлен hardware preset dropdown в Setup Wizard → Step 4
(General Settings). При выборе preset'а (RTX 4090 / A10 / RTX 5090)
автозаполняются 4 поля (vramMaxUsage / gpuMaxUsage / cpuMaxUsage / ramMaxUsage)
и показывается hint с CPPWORKER_* env vars для ручного применения к
cppworker.

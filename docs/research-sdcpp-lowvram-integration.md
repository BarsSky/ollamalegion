# stable-diffusion.cpp на 4–8 GB VRAM и интеграция как image-бэкенд за балансировщиком

**Дата сбора данных:** 2026-10-02. Все факты взяты из первоисточников (код, docs, HF API, GitHub API).
**Версия upstream, по которой сверялись код и флаги:** `master-929-3f8527a` (release 2026-09-27), дерево `3f8527a46c54ecf4cb4ed6003da8e8982283c73c`.
**Дисклеймер:** проект без semver, релизы вида `master-NNN-<sha>` выходят по нескольку раз в день. Имена флагов и API меняются часто (в самом README: *"API and command-line option may change frequently"*).

---

## 0. Главное, что меняет план (прочитать первым)

1. **Бинарь называется `sd-cli`, а не `sd`.** Прежнее имя `sd` больше не используется; сервер — `sd-server`. ([README.md](https://raw.githubusercontent.com/leejet/stable-diffusion.cpp/master/README.md), [examples/cli/README.md](https://raw.githubusercontent.com/leejet/stable-diffusion.cpp/master/examples/cli/README.md))
2. **Сервер = одна модель на процесс, загрузка только флагами при старте.** В `examples/server/main.cpp` создаётся один `sd_ctx` до `svr.listen(...)`; hot-swap модели отсутствует. Смена модели = перезапуск процесса. ([examples/server/main.cpp](https://raw.githubusercontent.com/leejet/stable-diffusion.cpp/master/examples/server/main.cpp))
3. **Все три API сериализуются одним мьютексом.** `sdapi`, `openai` и `sdcpp` берут `sd_ctx_mutex`; async-воркер ровно один поток (FIFO). Параллелизма на одной карте нет. ([async_jobs.cpp](https://raw.githubusercontent.com/leejet/stable-diffusion.cpp/master/examples/server/async_jobs.cpp), [routes_sdapi.cpp](https://raw.githubusercontent.com/leejet/stable-diffusion.cpp/master/examples/server/routes_sdapi.cpp))
4. **Отмена генерации в полёте НЕ поддерживается.** `features_by_mode.img_gen.cancel_generating == false`, а `POST /sdcpp/v1/jobs/{id}/cancel` для статуса `Generating` отдаёт **409** `"job is currently generating and cannot be interrupted yet"`. Отменить можно только `Queued`. ([routes_sdcpp.cpp](https://raw.githubusercontent.com/leejet/stable-diffusion.cpp/master/examples/server/routes_sdcpp.cpp))
5. **Прогресса по шагам в job-статусе нет.** В `AsyncGenerationJob` нет полей step/fraction/preview. В C-API примитивы есть (`sd_progress_cb_t`, `sd_cancel_generation`) — не проброшены в сервер. ([async_jobs.h](https://raw.githubusercontent.com/leejet/stable-diffusion.cpp/master/examples/server/async_jobs.h))
6. **`--max-vram` и `--params-backend` — новые флаги** (автоплейсмент `--auto-fit` по умолчанию `on`). В старых сборках их нет; вместо них были только `--offload-to-cpu` / `--clip-on-cpu` / `--vae-on-cpu`. ([docs/backend.md](https://raw.githubusercontent.com/leejet/stable-diffusion.cpp/master/docs/backend.md))
7. **Готовые релизы есть, но нет Vulkan под Windows?** Vulkan-сборки есть и для Linux, и для Windows; отсутствует **macOS x86_64**. ([releases API](https://api.github.com/repos/leejet/stable-diffusion.cpp/releases/latest))

---

## 1. Модели и требования к VRAM/RAM

### 1.1 Поддерживаемые семейства (актуальный README)

Изображения: SD1.x/2.x, SD-Turbo, SDXL, SDXL-Turbo, SD3/SD3.5, FLUX.1-dev/schnell, FLUX.2-dev/FLUX.2-klein, Lens, Chroma, Chroma1-Radiance, Qwen-Image, Qwen-Image-2.1, PiD, LongCat Image, **Z-Image**, MiniT2I, SenseNova U1.5, Ovis-Image, Anima, ERNIE-Image, Boogu Image, Krea2, Mage-Flow, SeFi-Image, HiDream-O1-Image, Ideogram4, LLaDA-Image, Ming-Image, PixArt.
Edit: FLUX.1-Kontext-dev, Qwen-Image-Edit (+2509), LongCat Image Edit, Boogu Image Edit, Mage-Flow-Edit, LLaDA-Image Edit.
Видео: Wan2.1/2.2, MiniMax-H3, LTX-2.3/2.5, HunyuanVideo 1.5, LingBot-Video.
Плюс PhotoMaker, IP-Adapter (SD1.5/SDXL, включая Plus), ControlNet (SD1.5), ADetailer, LoRA, LCM/LCM-LoRA, TAESD, ESRGAN.
([README.md](https://raw.githubusercontent.com/leejet/stable-diffusion.cpp/master/README.md))

### 1.2 Число шагов и cfg по семействам (рекомендации проекта)

| Семейство | steps | cfg-scale | Источник |
|---|---|---|---|
| SD1.5 / SD2.x | 20–30 | 7.0 | [sd.md](https://raw.githubusercontent.com/leejet/stable-diffusion.cpp/master/docs/sd.md) |
| SDXL / SDXL base | 20–30 | 7.0 | [sd.md](https://raw.githubusercontent.com/leejet/stable-diffusion.cpp/master/docs/sd.md) |
| SDXL-Turbo | 1–4 | **1.0** | distilled-модель, cfg выключается |
| SD-Turbo | 1–4 | **1.0** | [distilled_sd.md](https://github.com/leejet/stable-diffusion.cpp/blob/master/docs/distilled_sd.md) |
| LCM / LCM-LoRA | 4–8 | 1.0–2.0 | [lcm.md](https://github.com/leejet/stable-diffusion.cpp/blob/master/docs/lcm.md) |
| SD3.5 Large | 28–40 | **4.5**, `--sampling-method euler` | [sd3.md](https://raw.githubusercontent.com/leejet/stable-diffusion.cpp/master/docs/sd3.md) |
| FLUX.1-dev | 20–50 | **1.0**, euler | [flux.md](https://raw.githubusercontent.com/leejet/stable-diffusion.cpp/master/docs/flux.md) |
| FLUX.1-schnell | **4** | **1.0**, euler | [flux.md](https://raw.githubusercontent.com/leejet/stable-diffusion.cpp/master/docs/flux.md) |
| Chroma | 20–30 | **4.0**, euler, `--model-args chroma_use_dit_mask=false` | [chroma.md](https://raw.githubusercontent.com/leejet/stable-diffusion.cpp/master/docs/chroma.md) |
| Qwen-Image | 20–50 | **2.5**, euler, `--flow-shift 3` | [qwen_image.md](https://raw.githubusercontent.com/leejet/stable-diffusion.cpp/master/docs/qwen_image.md) |
| Z-Image-Turbo | **4–9** (автор: 8) | **1.0** | [z_image.md](https://raw.githubusercontent.com/leejet/stable-diffusion.cpp/master/docs/z_image.md) |
| Z-Image (base) | 20–50 | **5.0** | [z_image.md](https://raw.githubusercontent.com/leejet/stable-diffusion.cpp/master/docs/z_image.md) |

### 1.3 FLUX.1-dev: измеренная память по квантизациям (официальная таблица проекта)

| Type | q8_0 | q4_0 | q4_k | q3_k | q2_k |
|---|---|---|---|---|---|
| **Memory** | 12068.09 MB | 6394.53 MB | 6395.17 MB | 4888.16 MB | 3735.73 MB |

Это **измеренный пик**, а не размер файла. Источник: [docs/flux.md](https://raw.githubusercontent.com/leejet/stable-diffusion.cpp/master/docs/flux.md).

### 1.4 SD1.x: измеренная память (официальная таблица)

txt2img 512×512:

| precision | f32 | f16 | q8_0 | q5_0 | q5_1 | q4_0 | q4_1 |
|---|---|---|---|---|---|---|---|
| Memory | ~2.8 G | ~2.3 G | ~2.1 G | ~2.0 G | ~2.0 G | ~2.0 G | ~2.0 G |
| Memory + Flash Attention | ~2.4 G | ~1.9 G | ~1.6 G | ~1.5 G | ~1.5 G | ~1.5 G | ~1.5 G |

Источник: [docs/quantization_and_gguf.md](https://raw.githubusercontent.com/leejet/stable-diffusion.cpp/master/docs/quantization_and_gguf.md).

### 1.5 Реальные размеры GGUF-файлов

> **Единицы:** ниже — **GiB** (двоичные), как их отдаёт HF API (`bytes / 1024³`). Например `6.79 GB` = 6 791 167 136 байт = **6.32 GiB**.

**FLUX.1-dev** ([city96/FLUX.1-dev-gguf](https://huggingface.co/city96/FLUX.1-dev-gguf)) — diffusion-модель одним файлом:
Q2_K 4.03 · Q3_K_S 5.23 · Q4_0 6.79 · Q4_K_S 6.81 · Q5_0 8.27 · Q6_K 9.86 · Q8_0 12.71 · F16 23.80 GiB.
**FLUX.1-schnell** ([city96/FLUX.1-schnell-gguf](https://huggingface.co/city96/FLUX.1-schnell-gguf)): Q2_K 4.01 · Q3_K_S 5.21 · Q4_0 6.77 · Q4_K_S 6.78 · Q5_0 8.25 · Q6_K 9.83 · Q8_0 12.69 · F16 23.78 GiB.
**SD1.5** ([second-state/stable-diffusion-v1-5-GGUF](https://huggingface.co/second-state/stable-diffusion-v1-5-GGUF), единый all-in-one файл, включая VAE+CLIP): Q4_0 1.57 · Q4_1 1.59 · Q5_0 1.62 · Q5_1 1.64 · Q8_0 1.76 · f16 2.13 · f32 4.27 GiB. **K-квантов для SD1.5 не существует.**
**SDXL-Turbo** ([OlegSkutte/sdxl-turbo-GGUF](https://huggingface.co/OlegSkutte/sdxl-turbo-GGUF)): единственный файл `sd_xl_turbo_1.0.q8_0.gguf` = **4.10 GB** (3.82 GiB). K-квантов нет.
**Z-Image-Turbo** ([leejet/Z-Image-Turbo-GGUF](https://huggingface.co/leejet/Z-Image-Turbo-GGUF)) — только diffusion-модель, TE и VAE отдельно:
Q2_K 2.59 · Q3_K 3.14 · Q4_0 3.68 · Q4_K 3.86 · Q5_0 4.54 · Q6_K 5.26 · Q8_0 6.58 GB.
**Qwen3-4B-Instruct-2507** (TE для Z-Image, [unsloth](https://huggingface.co/unsloth/Qwen3-4B-Instruct-2507-GGUF)): Q3_K_S 1.89 · Q3_K_M 2.08 · Q4_K_S 2.38 · **Q4_K_M 2.50** · Q4_K_XL 2.55 · Q5_K_M 2.89 · Q6_K 3.31 · Q8_0 4.28 · F16 8.05 GB.
**VAE для Z-Image/FLUX**: `ae.safetensors` = **335 MB**; в репозитории `Tongyi-MAI/Z-Image-Turbo` — `vae/diffusion_pytorch_model.safetensors` = 167.67 MB.

### 1.5.1 Дополнительные размеры (по данным параллельного ресёрча)

**SDXL / SDXL-Turbo**, all-in-one (CLIP-L+G+VAE внутри, fp16), [gpustack/stable-diffusion-xl-1.0-turbo-GGUF](https://huggingface.co/gpustack/stable-diffusion-xl-1.0-turbo-GGUF): Q4_0 **3.94** · Q4_1 4.08 · Q8_0 **5.04** · fp16 **6.94** GB.
SDXL «UNet-only» вариант ([hum-ma/SDXL-models-GGUF](https://huggingface.co/hum-ma/SDXL-models-GGUF), отдельные `clip/` и `vae/`): Q4_0 **1.49** · Q5_K_M 1.84 GB. Пик памяти (TAE+FA+tiling, CLIP-L): Q4_0 2810 MB · Q8_0 4249 MB · fp16 6946 MB — **плоско от 512 до 1024**.

**SD3 Medium** ([city96/stable-diffusion-3-medium-gguf](https://huggingface.co/city96/stable-diffusion-3-medium-gguf)): Q4_0 1.28 · Q4_K_M **1.33** · Q5_K_M 1.58 · Q6_K 1.80 · Q8_0 2.29 · f16 4.17 GB.
**SD3.5 Large** ([city96/stable-diffusion-3.5-large-gguf](https://huggingface.co/city96/stable-diffusion-3.5-large-gguf)): Q4_0 **4.77** · Q5_0 5.77 · Q8_0 8.78 · f16 16.29 GB. **K-квантов (Q4_K_M/Q6_K/Q2_K) не существует.**
**Почему нет K-квантов у SD3.5-L:** ~90 % весов лежат в тензорах, форма которых не кратна 256-элементному суперблоку K-кванта; K-квантизуема только 2-я MLP-часть (~10 % параметров), поэтому файлы **смешивают типы**. Альтернатива, сделанная именно под sd.cpp: [stduhpf/SD3.5-Large-GGUF-mixed-sdcpp](https://huggingface.co/stduhpf/SD3.5-Large-GGUF-mixed-sdcpp) — q2_k_4_0 **4.69** · q3_k_4_0 4.87 · q4_k_4_0 5.11 GB.
Пик памяти @1024 (CLIP-L only): SD3.5-L Q2_K 7271 · Q4_0 7668 · Q8_0 11489 MB; SD3.5-M Q2_K 3437 · Q4_0 3962 · Q8_0 5080 MB.

**Chroma** ([silveroxides/Chroma-GGUF](https://huggingface.co/silveroxides/Chroma-GGUF), `chroma-unlocked-v20/`): Q3_K_L **4.99** · Q4_0 5.99 · Q4_K_M **6.12** · Q5_K_S 7.07 · Q6_K 8.21 · Q8_0 **10.29** · BF16 17.80 GB. Нужны VAE (335 MB) и **только T5-XXL** — **CLIP-L не нужен**.

**Qwen-Image 20B / Edit** ([QuantStack/Qwen-Image-GGUF](https://huggingface.co/QuantStack/Qwen-Image-GGUF)): Q2_K **7.06** · Q3_K_M 9.68 · Q4_0 11.85 · Q4_K_M **13.07** · Q5_K_M 14.93 · Q8_0 **21.76** GB. TE — Qwen2.5-VL-7B (GGUF Q4_K_M **4.68**, Q8_0 8.10); для Edit/2509 дополнительно нужен `Qwen2.5-VL-7B-Instruct-mmproj-BF16.gguf` (**1.35 GB**), потому что это vision-модель.

**FLUX.2-klein-4B** ([leejet/FLUX.2-klein-4B-GGUF](https://huggingface.co/leejet/FLUX.2-klein-4B-GGUF)): всего 2 кванта — Q4_0 **2.46** и Q8_0 **4.30** GB. klein-9B: Q4_0 5.62 · Q8_0 9.98 GB. Обеим нужны flux2-VAE (336 MB) и **Qwen3-4B/8B** в качестве TE (Q4_K_M 2.50 GB).
**FLUX.2-dev** ([city96/FLUX.2-dev-gguf](https://huggingface.co/city96/FLUX.2-dev-gguf)): Q2_K **12.86** · Q4_K_M 20.08 · Q8_0 35.00 · BF16 64.45 GB + TE Mistral-Small-3.2-24B Q4_K_M **14.33 GB**. То есть **FLUX.2-dev не влезает ни в какие 4–8 GB** даже в Q2_K.

**T5-XXL** (TE для FLUX/SD3/Chroma): fp16 **9.79 GB** / fp8_e4m3fn 4.89 ([flux_text_encoders](https://huggingface.co/comfyanonymous/flux_text_encoders)); GGUF-вариант [city96/t5-v1_1-xxl-encoder-gguf](https://huggingface.co/city96/t5-v1_1-xxl-encoder-gguf) Q3_K_S 2.10 · Q4_K_M **2.90** · Q5_K_M 3.39 · Q8_0 5.06 GB. **Это главный аргумент за квантованный T5 вместо fp16.**

**Измеренные по компонентам (реальные логи):**
- Z-Image-Turbo Q4_0: diffusion **3512 MB** + TE **4076 MB** + VAE 160 MB ([#1600](https://github.com/leejet/stable-diffusion.cpp/issues/1600)).
- GTX 1060, Z-Image-base Q3_K_M: diffusion 4350 + TE 3555 + VAE 95 = **8000 MB** с offload ([#1253](https://github.com/leejet/stable-diffusion.cpp/issues/1253)).
- FLUX.2-klein-4B Q8_0 на RTX 2060: 4101 MB VRAM + 2375 MB RAM + 160 MB VAE ([#1989](https://github.com/leejet/stable-diffusion.cpp/issues/1989)).

> ⚠️ **Правило Unsloth:** суммарная доступная **RAM + VRAM должна превышать размер GGUF-файла**, иначе модель не загрузится даже с offload.

> **Практический вывод для 4 GB:** Z-Image-Turbo Q3_K (3.14 GB) + TE на CPU — единственная комбинация, где diffusion-модель реально влезает в 4 GB целиком. Для FLUX в 4 GB не влезает даже Q2_K (4.03 GB) без `--offload-to-cpu`.

### 1.6 Что реально работает на 4–6 GB

**4 GB (подтверждено сообществом, discussion #1026):**

| Что | Квант | Условие | Кто/источник |
|---|---|---|---|
| Z-Image-Turbo | **Q3_K / Q4_0** | `--offload-to-cpu --diffusion-fa`, `--clip-on-cpu` (TE на CPU) | официальная wiki проекта |
| Z-Image-Turbo | Q4_K | «just fits in 4GB with clip on CPU» | netrunnereve, RX 470 4GB Vulkan |
| Z-Image-Turbo | Q8_0 | на 8 GB карте | netrunnereve |
| SD1.5 | Q4_0…Q8_0 | без offload, ~1.5–2.1 GB пик | официальная таблица |
| SDXL-Turbo | q8_0 (4.10 GB) | **на грани**; лучше Q4 + `--offload-to-cpu` | размер файла |

**6 GB:**
- SDXL / SDXL-Turbo: fp16 safetensors напрямую (7 GB) — только с `--offload-to-cpu`; q8_0 GGUF 4.10 GB — комфортно.
- FLUX.1-schnell Q4_0 (6.77 GB) — с `--offload-to-cpu`; замер проекта: q4_0 ≈ **6.39 GB** пик, т.е. уже близко к пределу 6 GB → нужен offload.
- FLUX.1-dev q2_k (4.03 GB, пик 3.74 GB) — влезает без offload, качество заметно хуже.
- **Chroma**: проект прямо пишет — «6GB or even 4GB of VRAM, without needing to offload to RAM» ([chroma.md](https://raw.githubusercontent.com/leejet/stable-diffusion.cpp/master/docs/chroma.md)). Это самая выгодная модель для 4–6 GB после Z-Image.
- Z-Image-Turbo Q8_0 (6.58 GB) + TE Q4_K_M (2.50 GB) = ~9 GB → нужен `--offload-to-cpu` либо TE на CPU.
- Qwen-Image **не для 4–6 GB**: diffusion ~11.9–21.8 GB + Qwen2.5-VL-7B TE. Только с `--params-backend disk` и очень медленно.
- SD3.5 Large **не для 4–6 GB**: UNet ~10 GB fp16 + три текстовых энкодера (CLIP-L + CLIP-G + T5-XXL) → 16–18 GB суммарно.

### 1.7 Discussion #1026 «How to Use Z-Image on a GPU with Only 4GB VRAM» — что там конкретно

Тема создана **leejet** (владелец проекта) 2025-12-01. Тело обсуждения — это ссылка на официальную wiki, весь технический контент живёт там.
([discussion #1026](https://github.com/leejet/stable-diffusion.cpp/discussions/1026), [wiki](https://github.com/leejet/stable-diffusion.cpp/wiki/How-to-Use-Z%E2%80%90Image-on-a-GPU-with-Only-4GB-VRAM))

**Рекомендованные файлы (из wiki):**
- Diffusion: Z-Image-Turbo GGUF → `leejet/Z-Image-Turbo-GGUF`. Для 4 GB: **Q4_0 или Q3_K**; Q2_K тоже есть, но качество ниже.
- Text encoder: Qwen3-4B → `unsloth/Qwen3-4B-Instruct-2507-GGUF`, **Q4_K_M (recommended)**.
- VAE: `black-forest-labs/FLUX.1-schnell` → `ae.safetensors`. Wiki: «The original VAE / Flux.1 VAE — essentially identical».

**Пример команды из wiki:**
```
sd-cli.exe --diffusion-model z_image_turbo-Q3_K.gguf --vae ae.safetensors \
  --llm Qwen3-4B-Instruct-2507-Q4_K_M.gguf -p "..." \
  --cfg-scale 1.0 -v --offload-to-cpu --diffusion-fa -H 1024 -W 512
```

**Таблица «Recommended Flags for Low VRAM» из wiki:**

| Флаг | Что делает (формулировка wiki) |
|---|---|
| `--offload-to-cpu` | грузит веса в VRAM только во время вычисления → сильно снижает VRAM без потери скорости |
| `--diffusion-fa` | Flash Attention → быстрее и экономнее |

**Optional Optimizations (для больших разрешений):** `--vae-conv-direct` (снижает VRAM при VAE-decode), `--vae-tiling` (тайловый VAE), `--clip-on-cpu` (позволяет держать Qwen3-4B на CPU → можно взять более высокую точность TE).

**Числа из комментариев к #1026 (важно: разброс огромный):**

| Железо | Модель/квант | Скорость (512×512) | Автор |
|---|---|---|---|
| RX 580 4 GB, Vulkan, Linux | Q3_K | 20 s/it → после переустановки Mesa/Vulkan SDK **16 s/it** | stduhpf |
| RX 470, Vulkan, Linux | Q4_K | **7 s/it**; 1024×1024 — 35 s/it | netrunnereve |
| RX 470, Vulkan, Linux | Q3_K | 7.5 s/it | netrunnereve |
| RTX 2070 | Q3_K_M | **1.19 s/it** | Green-Sky |
| RTX 3050 Laptop | (не указан) | **2.17 s/it** | leejet |

Диагностика разброса: **`RADV_PERFTEST=nogttspill`** вернул stduhpf с 16–17 s/it к ~7.3 s/it. Это Mesa/RADV, а не sd.cpp. ([комментарий](https://github.com/leejet/stable-diffusion.cpp/discussions/1026#discussioncomment-15138863))
Qwen3-4B TE на CPU: граф conditioner ≈ **823–824 мс** (Green-Sky). То есть держать TE на CPU дёшево по времени.

**Проверенные настройки сэмплинга от контрибьюторов (из #1026):**
- stduhpf: `--sampling-method euler --scheduler smoothstep --cfg-scale 1 --steps 8`.
- engrtipusultan: `--sampling-method heun --scheduler smoothstep --flow-shift 2 --steps 4 --cfg-scale 1 --rng cpu --vae-conv-direct --diffusion-fa`.
- Осторожно: тот же автор сообщает, что `--scheduler smoothstep` **ломает текст в картинке** — без него буквы корректнее.
- Воспроизводимость: **всегда задавать положительный `--seed`** (при `--seed -1` результаты «плавают» между запусками).
- **Чёрные картинки на ROCm** с `unsloth/Z-Image-GGUF` Q4_K_M, при этом Vulkan работает (wbruna). Отметить как известный баг бэкенда.

### 1.8 TAESD / TAEHV — экономия на VAE decode

- `--taesd PATH` (`--tae` — алиас). Tiny AutoEncoder вместо полноценного VAE: **быстрый decode, но качество ниже** («low quality»). Модель: [madebyollin/taesd](https://huggingface.co/madebyollin/taesd) → `diffusion_pytorch_model.safetensors`.
- Для Qwen-Image и Wan — **TAEHV** вместо TAESD: заменить `--vae xxx.safetensors` на `--tae xxx.safetensors`. Веса: [taew2_1.safetensors](https://github.com/madebyollin/taehv/raw/refs/heads/main/safetensors/taew2_1.safetensors) (Qwen-Image, Wan2.1, Wan2.2-A14B) и `taew2_2.safetensors` (Wan2.2-TI2V-5B).
- Если всё ещё OOM: добавить `--vae-conv-direct`, «though might be slower».
- Источник: [docs/taesd.md](https://raw.githubusercontent.com/leejet/stable-diffusion.cpp/master/docs/taesd.md).

**Важная оговорка:** официальная таблица FLUX в `docs/flux.md` и примеры в wiki меряют VRAM **с полноценным VAE**. TAESD экономит именно пик decode (самую тяжёлую часть на больших разрешениях), но **точных MB экономии от TAESD в документации sd.cpp нет** — это пробел в данных, не проверялось в рамках этого ресёрча.

---

## 2. Форматы файлов моделей и соответствие флагам

### 2.1 Два способа упаковки

1. **Единый all-in-one файл** (`.ckpt` / `.safetensors` / `.gguf`): diffusion + VAE + text encoder(ы) в одном файле. Передаётся через `-m / --model`.
   - Пример: `second-state/stable-diffusion-v1-5-GGUF` — один `...-Q8_0.gguf` (1.76 GB) содержит всё.
   - `OlegSkutte/sdxl-turbo-GGUF` — один `sd_xl_turbo_1.0.q8_0.gguf` (4.10 GB).
2. **Раздельные компоненты** (обязательно для SD3+/FLUX/Qwen/Z-Image): отдельно diffusion-модель, VAE, и текстовые энкодеры. Передаются через `--diffusion-model`, `--vae`, `--clip_l`, `--clip_g`, `--t5xxl`, `--llm`.

### 2.2 Карта «флаг ↔ что это»

| Флаг | Компонент | Для каких моделей |
|---|---|---|
| `-m` / `--model` | полная модель одним файлом | SD1.x/2.x, SDXL, SDXL-Turbo, SD-Turbo |
| `--diffusion-model` | standalone diffusion (UNet/DiT/MMDiT) | SD3, FLUX, Chroma, Qwen-Image, Z-Image, FLUX.2 |
| `--vae` | VAE decoder | SDXL (отдельный `sdxl_vae-fp16-fix`), SD3, FLUX, Chroma, Qwen, Z-Image |
| `--clip_l` | CLIP-L text encoder | SD3/3.5, FLUX.1, Chroma (опц.) |
| `--clip_g` | CLIP-G text encoder | SD3/3.5 |
| `--t5xxl` | T5-XXL text encoder | SD3/3.5, FLUX.1, Chroma |
| `--llm` | LLM-энкодер (замена T5/CLIP) | **Qwen-Image** (Qwen2.5-VL-7B), **Z-Image** (Qwen3-4B), FLUX.2 (Mistral-Small-3.2), PiD, Lens |
| `--llm_vision` | ViT-часть LLM | Qwen-Image, Qwen-Image-Edit |
| `--taesd` / `--tae` | tiny autoencoder | любые (вместо/в дополнение к VAE) |
| `--tokenizer` | `tokenizer.json` | обязательно для PiD и Lens |
| `--uncond-diffusion-model` | безусловная ветка для CFG | Ideogram4 |
| `--high-noise-diffusion-model` | high-noise ветка | Wan2.2 MoE и подобные |

Полный список — в `SDContextParams::get_options()`: [examples/common/common.cpp](https://raw.githubusercontent.com/leejet/stable-diffusion.cpp/master/examples/common/common.cpp).

### 2.3 «Правильные» репозитории HuggingFace

> 🚨 **КРИТИЧНО: формат GGUF для FLUX различается между sd.cpp и ComfyUI.** GGUF от `city96` **не загружаются в sd.cpp** — падают с
> ```
> [ERROR] main.cpp:92 - new_sd_ctx_t failed
> ```
> `city96`-файлы рассчитаны на ноду **ComfyUI-GGUF**, а не на sd.cpp. Для sd.cpp надо брать GGUF от **`leejet`**. Это подтверждено независимым практическим отчётом (RX 580 / Vulkan): *«Using a city96 GGUF in sd-server returns: [ERROR] main.cpp:92 - new_sd_ctx_t failed»*. ([источник](https://huggingface.co/aivisionslab/ai-local-rx580-stack/raw/main/README.md))
> При этом официальный `docs/flux.md` sd.cpp сам ссылается на `leejet/FLUX.1-dev-gguf` и `leejet/FLUX.1-schnell-gguf` — то есть проект изначально указывает на свои, а не на city96.

| Модель | Репозиторий (для sd.cpp) | Что качать |
|---|---|---|
| SD1.5 | [second-state/stable-diffusion-v1-5-GGUF](https://huggingface.co/second-state/stable-diffusion-v1-5-GGUF) | 1 файл Q4_0…Q8_0 (all-in-one) |
| SDXL-Turbo | [OlegSkutte/sdxl-turbo-GGUF](https://huggingface.co/OlegSkutte/sdxl-turbo-GGUF) | 1 файл q8_0 (4.10 GB) |
| SDXL-Turbo all-in-one | [gpustack/stable-diffusion-xl-1.0-turbo-GGUF](https://huggingface.co/gpustack/stable-diffusion-xl-1.0-turbo-GGUF) | Q4_0 3.94 / Q8_0 5.04 GB. ⚠️ формат ориентирован на llama-box; проверять загрузку |
| FLUX.1-dev | **[leejet/FLUX.1-dev-gguf](https://huggingface.co/leejet/FLUX.1-dev-gguf)** ✅ (не city96!) | diffusion + `ae.safetensors` (335 MB) + `clip_l` (246 MB) + T5-XXL |
| FLUX.1-schnell | **[leejet/FLUX.1-schnell-gguf](https://huggingface.co/leejet/FLUX.1-schnell-gguf)** ✅ | то же |
| FLUX.2-dev | [city96/FLUX.2-dev-gguf](https://huggingface.co/city96/FLUX.2-dev-gguf) (в docs) | diffusion + flux2-VAE + Mistral-Small-3.2 |
| FLUX.2-klein | [leejet/FLUX.2-klein-4B-GGUF](https://huggingface.co/leejet/FLUX.2-klein-4B-GGUF), [-9B-](https://huggingface.co/leejet/FLUX.2-klein-9B-GGUF) ✅ | diffusion + flux2-VAE (336 MB) + Qwen3-4B/8B |
| Chroma | **[silveroxides/Chroma-GGUF](https://huggingface.co/silveroxides/Chroma-GGUF)** | diffusion + `ae.safetensors` + **только T5-XXL** (CLIP-L не нужен) |
| Qwen-Image | **[QuantStack/Qwen-Image-GGUF](https://huggingface.co/QuantStack/Qwen-Image-GGUF)** | diffusion + `qwen_image_vae.safetensors` (254 MB) + Qwen2.5-VL-7B (`--llm`) |
| Z-Image-Turbo | **[leejet/Z-Image-Turbo-GGUF](https://huggingface.co/leejet/Z-Image-Turbo-GGUF)** ✅ | diffusion + `ae.safetensors` + Qwen3-4B |
| Z-Image (base) | [unsloth/Z-Image-GGUF](https://huggingface.co/unsloth/Z-Image-GGUF) | то же |
| SD3.5 Large | [city96/stable-diffusion-3.5-large-gguf](https://huggingface.co/city96/stable-diffusion-3.5-large-gguf), [stduhpf/…-mixed-sdcpp](https://huggingface.co/stduhpf/SD3.5-Large-GGUF-mixed-sdcpp) | diffusion + clip_l + clip_g + t5xxl + VAE |
| T5-XXL | [city96/t5-v1_1-xxl-encoder-gguf](https://huggingface.co/city96/t5-v1_1-xxl-encoder-gguf) | Q4_K_M 2.90 GB (вместо fp16 9.79 GB!) |
| Текстовые энкодеры FLUX (fp16/fp8) | [comfyanonymous/flux_text_encoders](https://huggingface.co/comfyanonymous/flux_text_encoders) | `clip_l` 246 MB, T5 fp16 9.79 / fp8 4.89 GB |
| VAE FLUX/Z-Image | [black-forest-labs/FLUX.1-schnell](https://huggingface.co/black-forest-labs/FLUX.1-schnell) → `ae.safetensors` (335 MB) | один файл |
| SDXL VAE fix | `sdxl_vae-fp16-fix.safetensors` | [docs/sd.md](https://raw.githubusercontent.com/leejet/stable-diffusion.cpp/master/docs/sd.md) |

**Оговорки и опровержения:**
- ❌ **`ggerganov/*` для изображений НЕ СУЩЕСТВУЕТ.** У ggerganov есть только whisper.cpp/ggml-артефакты. Использовать как ориентир нельзя.
- ❌ **Не существует** `city96/stable-diffusion-xl-base-1.0-gguf`, `city96/SDXL-Turbo*-gguf`, `city96/sd-turbo-gguf`. Эти имена выглядят правдоподобно, но репозиториев нет (проверено через HF API — он отдаёт 401/gated вместо 404, что и ввело в заблуждение).
- ⚠️ **`city96/*` для FLUX и SD3 — это ComfyUI-GGUF, а не sd.cpp.** Для SD3 Medium/Large city96-файлы в sd.cpp исторически грузятся, но безопаснее использовать смешанный формат `stduhpf/SD3.5-Large-GGUF-mixed-sdcpp`, сделанный специально под sd.cpp.
- ⚠️ Правильный id для SD1.5 — именно `second-state/stable-diffusion-v1-5-GGUF` (нижний регистр + дефисы).
- ⚠️ `Comfy-Org/Qwen-Image-Edit-2509_ComfyUI` — неверное имя; правильное `Comfy-Org/Qwen-Image-Edit_ComfyUI`.
- ⚠️ HF-репозиторий `madebyollin/taehv` отдаёт **401 (gated)**; веса TAEHV лежат на [GitHub](https://github.com/madebyollin/taehv), а не на HF.
- ⚠️ Поддерживаемые в sd.cpp **раздельные компоненты — это отдельные файлы**. Единый all-in-one GGUF существует **только** для SD1.x/2.x/SD-Turbo/SDXL. **Все** GGUF для SD3+, FLUX, Chroma, Qwen, Z-Image — это **diffusion-модель отдельно**, плюс VAE и текстовые энкодеры отдельными файлами.

### 2.4 Конвертация в GGUF

```
sd-cli -M convert -m model.safetensors -o model-q8_0.gguf -v --type q8_0
```
Поддерживаются `f32, f16, q8_0, q5_0, q5_1, q4_0, q4_1`, плюс K-кванты (`q2_K, q3_K, q4_K`) — они перечислены в help для `--type`.
Конвертация с раздельными компонентами — через `convert_with_components(model, clip_l, clip_g, t5xxl, diffusion_model, vae, ...)`.
Источник: [quantization_and_gguf.md](https://raw.githubusercontent.com/leejet/stable-diffusion.cpp/master/docs/quantization_and_gguf.md), [main.cpp](https://raw.githubusercontent.com/leejet/stable-diffusion.cpp/master/examples/cli/main.cpp).

---

## 3. Ключевые CLI-флаги `sd-server`

Сервер принимает **три группы опций**: `Svr Options` + `Context Options` + `Default Generation Options`.
Источник: [examples/server/main.cpp](https://raw.githubusercontent.com/leejet/stable-diffusion.cpp/master/examples/server/main.cpp), [runtime.cpp](https://raw.githubusercontent.com/leejet/stable-diffusion.cpp/master/examples/server/runtime.cpp), [common.cpp](https://raw.githubusercontent.com/leejet/stable-diffusion.cpp/master/examples/common/common.cpp).

### 3.1 Только у сервера (Svr Options) — полный список

| Флаг | Тип | Дефолт | Назначение |
|---|---|---|---|
| `-l`, `--listen-ip` | string | `127.0.0.1` | IP прослушивания |
| `--listen-port` | int | `1234` | Порт |
| `--serve-html-path` | string | — | Внешний `index.html` вместо встроенного WebUI |
| `--color` | bool | false | Цветной лог |
| `-h`, `--help` | — | — | Справка |
| `--log-level` | enum | `info` | `debug, verbose, info, warn, error` |
| `-v`, `--verbose` | bool | — | = `--log-level verbose` |

Полный список серверных опций — **ровно эти 7**. Никаких `--max-queue`, `--threads` для HTTP и т.п. в `sd-server` нет (в отличие от `llama-server`).

### 3.2 Модель и компоненты (Context Options)

| Флаг | Что делает | Влияние на память/скорость |
|---|---|---|
| `-m`, `--model` | полная модель одним файлом | взаимоисключающ с `--diffusion-model` |
| `--diffusion-model` | standalone diffusion | базовый выбор VRAM |
| `--vae` | отдельный VAE | +0.17–0.34 GB; пик decode на 1024² может быть >8 GB |
| `--vae-format` | `auto\|flux\|sd3\|flux2\|wan` | ОБЯЗАТЕЛЬНО при нестандартном VAE; ошибка формата → шум/чёрный выход |
| `--clip_l`, `--clip_g` | CLIP-L / CLIP-G | +~0.25 GB каждый (fp16) |
| `--t5xxl` | T5-XXL | +~9.8 GB fp16 — **главный кандидат на CPU** |
| `--llm` | LLM-энкодер | Qwen3-4B Q4_K_M = 2.50 GB; Qwen2.5-VL-7B — существенно больше |
| `--llm_vision` | ViT-часть | Qwen-Image, Qwen-Image-Edit |
| `--taesd` / `--tae` | tiny autoencoder | **сильно снижает пик decode**, качество ниже |
| `--lora-model-dir` | каталог LoRA | нужен для `lora[].path` в API и для `<lora:...>` в prompt CLI |
| `--hires-upscalers-dir` | каталог ESRGAN | только **верхний уровень**, подкаталоги не сканируются; нужен для `POST /sdcpp/v1/upscale` и `hires.upscaler` |
| `--embd-dir` | каталог textual embeddings | — |
| `--control-net` | ControlNet | SD1.5 |
| `--ip-adapter` | IP-Adapter (нужен `--clip_vision`) | — |
| `--clip_vision` | CLIP-vision | для IP-Adapter/PhotoMaker |
| `--photo-maker`, `--pulid-weights` | ID-инъекция | — |
| `--upscale-model` | ESRGAN для `-M upscale` | — |
| `--motion-module` | AnimateDiff (SD1.5) | — |
| `--audio-vae`, `--audio-encoder`, `--embeddings-connectors` | аудио-ветки LTX/Wan | — |
| `--tokenizer` | `tokenizer.json` | **обязателен для PiD и Lens** |
| `--model-args` | key=value: `chroma_use_dit_mask`, `chroma_use_t5_mask`, `chroma_t5_mask_pad`, `qwen_image_zero_cond_t`, `qwen_image_2_1_prefix_cache*` | экономия/корректность для Chroma и Qwen |

### 3.3 Плейсмент памяти и устройств — **самое важное для слабых карт**

| Флаг | Что делает | Влияние |
|---|---|---|
| `--backend` | runtime-backend, с per-module: `clip=cpu,vae=cuda0,diffusion=vulkan0`; `all=`, `default=`, `*=` | решает, где **считается** |
| `--params-backend` | где **лежат** веса: `cpu`, `disk`, `diffusion=disk,te=cpu`. **Отключает `--auto-fit`** | `disk` = минимум и VRAM, и RAM, но медленнее (перечитывает файл) |
| `--max-vram` | бюджет в GiB: `6`, `cuda0=6,vulkan0=4`, `-1` (резерв 1 GiB) | управляет graph-cut / сегментацией |
| `--offload-to-cpu` | **compat-шорткат** = `--params-backend '*=cpu'` | веса в RAM, подгрузка по требованию; официально «без потери скорости» |
| `--clip-on-cpu` | **deprecated** → `--backend te=cpu` | убрать TE из VRAM |
| `--vae-on-cpu` | **deprecated** → `--backend vae=cpu` | убрать VAE из VRAM |
| `--control-net-cpu` | **deprecated** → `--backend controlnet=cpu` | — |
| `--auto-fit` | `on`(дефолт)/`off` — автоплейсмент diffusion→te→vae по GPU/RAM/другому GPU/disk | `off` нужен, если хочешь контролировать вручную |
| `--split-mode` | `layer`(дефолт)/`row`, per-module | мульти-GPU; row = CUDA only |
| `--rpc-servers` | `host:port,...` — выгрузка вычислений на ggml RPC | распределённый инференс |
| `--disable-prefetch` | выключить асинхронный prefetch следующего сегмента | диагностика |
| `--disable-segmented-compute` | форсировать монолитный граф | диагностика; приведёт к OOM, если не влезает |
| `--eager-load` | грузить все веса сразу, а не лениво | предсказуемость ценой пиковой памяти |
| `--mmap` | memory-map модели | маппинг считается в бюджет по полному размеру файла |

**Ключевая механика (docs/performance.md):** при `--offload-to-cpu` веса живут в RAM и копируются в VRAM по мере надобности; **автоматический graph-cut исполняет граф сегментами**, вытесняя веса с конца сегмента. Требуется **128 MiB headroom** для планирования и **512 MiB зарезервированной device scratch**; `--max-vram` задаёт управляемый бюджет. Отдельного флага стриминга больше **не нужно** — сегментация включается сама.

### 3.4 Внимание/скорость

| Флаг | Что делает | Влияние |
|---|---|---|
| `--fa` | Flash Attention везде | — |
| `--diffusion-fa` | FA только в diffusion | **flux 768² ≈ −600 MB; SD2 768² ≈ −1400 MB**; на CUDA обычно ещё и быстрее, на остальных часто медленнее |
| `--sage-attn` | SageAttention (CUDA, patched GGML) | ускорение на NVIDIA; при отсутствии — откат на FA/default |
| `--diffusion-conv-direct` | `ggml_conv2d_direct` в diffusion | скорость/память |
| `--vae-conv-direct` | прямые 2D/3D свёртки в VAE | **снижает VRAM при decode** (иногда медленнее) |
| `--force-sdxl-vae-conv-scale` | форс conv scale для SDXL VAE | лечение артефактов |
| `--vae-tiling` | тайловый VAE encode+decode | **главный рычаг против OOM на decode** |
| `--vae-tile-size` | `256` или `WxH`, **в пикселях изображения** (не в латентах!) | дефолт 256; миграция: старые значения были в латентах |
| `--vae-tile-overlap` | 0…0.5, дефолт 0.5 | больше overlap = меньше швов, больше времени |
| `--vae-relative-tile-size` | ≤1 = доля размера; >1 = целевое число тайлов | требует `--vae-tiling` |
| `--temporal-tiling` | временное тайлирование (видео) | независимо от пространственного |
| `--threads`, `-t` | число CPU-потоков, `-1` = физ. ядра | влияет на CPU-часть |

**Про `--vae-tile-size` — это breaking change:** раньше значения были в латентных единицах, теперь в пикселях изображения. Старый decode-tile 32 = 256 px для 8× VAE или 512 px для 16× VAE. Поля JSON переименованы `tile_size_x/y` → `tile_size_w/h`, `rel_size_x/y` → `rel_size_w/h`. **Если в старом конфиге есть `vae_tile_size`, после апгрейда он будет interpreted иначе — молча.** ([docs/performance.md](https://raw.githubusercontent.com/leejet/stable-diffusion.cpp/master/docs/performance.md))

**Авто-retry при OOM:** основной путь VAE decode сам повторяет аллокацию с меньшими тайлами даже без `--vae-tiling`; видео-VAE сначала пробуют temporal, потом spatial (до 256 px, затем деление пополам). **Encode автоматического retry НЕ имеет.** Usefull: на decode можно не указывать tiling — сервер сам сожмёт; на encode — указывать обязательно.

### 3.5 Сэмплирование и генерация (Generation Options)

| Флаг | Дефолт | Замечание |
|---|---|---|
| `-p`, `--prompt`, `--negative-prompt` | — | есть также `--prompt-file`, `--negative-prompt-file` |
| `-H`, `--height`, `-W`, `--width` | 512 | ограничения сервера: **64…4096** |
| `--steps` | зависит от модели | clamp на сервере: **1…100** |
| `--cfg-scale` | — | для turbo/schnell/`*flux*` = 1.0 |
| `--img-cfg-scale`, `--guidance`, `--slg-scale`, `--skip-layer-start/end`, `--skip-layers` | — | SLG (skip-layer guidance): слои по умолчанию `[7,8,9]` |
| `--sampling-method` | определяется моделью | `sd_get_default_sample_method`; значения: euler, euler_a, heun, dpm2, dpm++2m, dpm++2m_v2, dpm++2s_a, er_sde, lcm, ddim, res_multistep, res_2s, euler_cfg_pp, … |
| `--scheduler` | определяется моделью | `discrete`, `karras`, `exponential`, `ays`, `gits`, `smoothstep`, … |
| `--rng` | **`cuda`** | `std_default\|cuda\|cpu`; `cuda` = как sd-webui, `cpu` = как ComfyUI |
| `--sampler-rng` | = `--rng` | отдельный RNG для сэмплера |
| `--seed` | 42 в CLI / `-1` в сервере | сервер сохраняет `-1`, если был запрошен случайный |
| `--prediction` | — | `eps, v, edm_v, sd3_flow, flux_flow, sefi_flow` |
| `--flow-shift` | `INFINITY` (default модели) | Z-Image/Qwen рекомендуется 2–3 |
| `--timestep-shift`, `--eta`, `--sigmas` | — | — |
| `--strength` | 0.75 | img2img, `[0,1]` |
| `--batch-count` | 1 | серверный clamp: **1…8** |
| `--clip-skip` | -1 | ≤0 = не задано |
| `--init-img`, `--mask`, `--control-image`, `--ip-adapter-image`, `--ref-image` | — | img2img / inpaint / ControlNet / IP-Adapter / reference |
| `--cache-mode` | — | `easycache, ucache, dbcache, taylorseer, cache-dit, spectrum` — ускорение за счёт переиспользования шагов |
| `--cache-option` | — | `threshold, start, end, decay, relative, reset, Fn, Bn, warmup, w, m, lam, window, flex, stop` |
| `--lora-apply-mode` | `auto` | `auto, immediately, at_runtime`; `immediately` **не работает с row-split** |
| `--hires` + `--hires-upscaler`, `--hires-scale`, `--hires-width/height`, `--hires-steps`, `--hires-denoising-strength` | выкл | highres fix; `--hires-upscaler` = `Latent`, `Latent (nearest-exact)`, `Lanczos`, `Nearest`, `Latent (bicubic)` и т.п. |
| `--upscale-repeats`, `--upscale-tile-size` | 1 / 128 | ESRGAN |
| `--type` | тип файла | `f32, f16, q4_0, q4_1, q5_0, q5_1, q8_0, q2_K, q3_K, q4_K` |
| `--tensor-type-rules` | — | напр. `"^vae\.=f16,model\.=q8_0"` |
| `--linear-scale`, `--attn-scale` | 0 | лечение чёрных/белых картинок и NaN |
| `--increase-ref-index`, `--circular`, `--circularx`, `--circulary` | — | — |
| `--disable-image-metadata` | — | по умолчанию метаданные в PNG **включаются** |
| `--embed-image-metadata` | true | webui-совместимая строка в PNG |
| `--list-devices` | — | список доступных ggml-устройств (диагностика) |
| `-M`, `--mode` | `img_gen` | `img_gen, adetailer, vid_gen, upscale, convert, metadata` |

### 3.6 Флаги, которых может не быть в старых сборках (хронология)

| Что | Когда появилось | Чем заменить в старой сборке |
|---|---|---|
| `--params-backend` | новый (вместе с `--auto-fit`) | `--offload-to-cpu` / `--clip-on-cpu` / `--vae-on-cpu` |
| `--auto-fit on\|off` | новый | то же |
| `--max-vram` | новый | нет замены |
| `--split-mode layer\|row` | новый | нет |
| `--rpc-servers` | новый (RPC) | нет |
| `--sage-attn` | новый | `--fa` |
| `--vae-relative-tile-size` | новый | `--vae-tile-size` |
| `--vae-tile-overlap` | новый | нет |
| `--model-args` | новый | нет |
| `--hires-upscalers-dir` | новый | нет |
| `--cache-mode` / `--cache-option` | новый | нет |
| `--skip-layers` / `--slg-scale` | новый | нет |
| `--llm` (вместо `--qwen2vl`) | новый; `--qwen2vl` помечен **deprecated** | `--qwen2vl` |
| `-M upscale` | новый | — |
| `sdcpp API` (`/sdcpp/v1/*`) | новый; `sd-server` как таковой молод | `/sdapi/v1/txt2img` |
| `--serve-html-path` | новый | нет |
| `--mmap`, `--eager-load`, `--disable-prefetch`, `--disable-segmented-compute` | новые | нет |
| `--vae-format` | новый | нет |
| `--prediction` | новый | нет |
| **`--listen-ip` / `--listen-port`** | **новые; в старых сборках было `--host` / `--port`** | `--host` / `--port` |

> **Про `--listen-ip`/`--listen-port`:** практический отчёт по сборке `master-600+` явно фиксирует: *«Older builds use `--host` / `--port`. Newer builds (master-600+) use `--listen-ip` / `--listen-port`. Run `sd-server --help` to check which your build expects.»* ([источник](https://huggingface.co/aivisionslab/ai-local-rx580-stack/raw/main/README.md)) `sd-server` **аварийно завершается** при неизвестном аргументе (`error: unknown argument`), поэтому неверный флаг = мгновенное падение процесса. Враппер обязан **проверять `sd-server --help`** при первом запуске либо пиновать версию.

**Критично:** старые гайды в интернете описывают `sd` (не `sd-cli`) и не знают про `--max-vram`. Формулировка `--type` тоже изменилась — раньше это был отдельный список без K-квантов.

---

## 4. Серверные API: подтверждение и оценка

Все три семейства подтверждены **и в документации, и в коде**. Источник: [examples/server/api.md](https://raw.githubusercontent.com/leejet/stable-diffusion.cpp/master/examples/server/api.md) + чтение `routes_openai.cpp`, `routes_sdapi.cpp`, `routes_sdcpp.cpp`.

### 4.1 OpenAI-совместимый API

| Endpoint | Метод | Ответ |
|---|---|---|
| `/v1/images/generations` | POST | **синхронный**; `{created, output_format, data:[{b64_json}]}` |
| `/v1/images/edits` | POST | multipart: `prompt`, `image[]`, `image`, `mask`, `n`, `size`, `output_format` |
| `/v1/models` | GET | `data:[{id:"sd-cpp-local", object:"model", owned_by:"local"}]` |

- Поля запроса: `prompt`(обяз.), `n`, `size` (`WIDTHxHEIGHT`), `output_format` (`png|jpeg|webp`), `output_compression` (0…100).
- **Нативное расширение:** в `prompt` можно вложить `<sd_cpp_extra_args>{...}</sd_cpp_extra_args>` с полной схемой `sdcpp API`. Блок вырезается из промпта перед генерацией.
- `output_format: webp` доступен только при сборке с WebP.
- **Асинхронного job-polling в этом семействе нет** — запрос держится до конца генерации.
- `<lora:...>` в промпте **намеренно не поддерживается** во всех трёх API.

### 4.2 WebUI-совместимый API (`/sdapi/v1/*`)

Реализовано: `POST /sdapi/v1/txt2img`, `POST /sdapi/v1/img2img`, `GET /sdapi/v1/loras`, `/upscalers`, `/latent-upscale-modes`, `/samplers`, `/schedulers`, `/sd-models`, `/options`.

- **Синхронный** (ответ ждёт генерацию), ответ `{images:[b64 png], parameters, info}`.
- Поля txt2img: `prompt`, `negative_prompt`, `width`, `height`, `steps`, `cfg_scale`, `seed`, `batch_size`, `clip_skip`, `sampler_name`, `scheduler`, `lora[]`, `extra_images[]`, `enable_hr`, `hr_upscaler`, `hr_scale`, `hr_resize_x/y`, `hr_steps`, `denoising_strength`.
- img2img добавляет `init_images[]`, `mask`, `inpainting_mask_invert`, `denoising_strength`.
- Поддерживаются **алиасы сэмплеров в стиле WebUI**: `Euler a`, `k_euler`, `DPM++ 2M`, `k_dpmpp_2m`, `DDIM`, `LCM`, `k_dpmpp_2m_sde_gpu`, `euler_a_cfg_pp` и др.
- **Важно:** `GET /sdapi/v1/options` **есть**, а `POST /sdapi/v1/options` **отсутствует**. То есть классический способ A1111 «сменить `sd_model_checkpoint` через POST /sdapi/v1/options» **не работает**. `GET /sdapi/v1/sd-models` возвращает ровно одну запись с placeholder-хешами (`8888888888`).
- `/sdapi/v1/loras` **рекурсивно** сканирует `--lora-model-dir` (в отличие от upscalers, где только верхний уровень).
- LoRA-теги `<lora:...>` в prompt отключены; только структурированное поле `lora[]`.

### 4.3 Нативный асинхронный `sdcpp API` — **рекомендуемый контракт**

| Endpoint | Метод | Ответ |
|---|---|---|
| `/sdcpp/v1/capabilities` | GET | модель, режимы, дефолты, форматы, фичи, samplers, schedulers, loras, upscalers, **limits** |
| `/sdcpp/v1/img_gen` | POST | **202 Accepted** `{id, kind, status:"queued", created, poll_url}` |
| `/sdcpp/v1/jobs/{id}` | GET | `{id, kind, status, created, started, completed, queue_position, result, error}` |
| `/sdcpp/v1/jobs/{id}/cancel` | POST | 200/404/409/410 |
| `/sdcpp/v1/vid_gen` | POST | 202; видео |
| `/sdcpp/v1/upscale` | POST | **синхронно**, 200; RGB ESRGAN, без диффузии |

**Job-модель:** статусы `queued | generating | completed | failed | cancelled`. Сингл-воркер, FIFO. TTL завершённых и упавших джоб — **по 600 с** (`completed_ttl_seconds` / `failed_ttl_seconds`), после чего `GET` даёт **410 Gone**. `queue_position` считается на лету.

**Лимиты (из `/capabilities.limits`, значения — константы в коде):**

| Лимит | Значение |
|---|---|
| `min_width` / `min_height` | 64 |
| `max_width` / `max_height` | 4096 |
| `max_batch_count` | 8 |
| **`max_queue_size`** | **64** (`AsyncJobManager::max_pending_jobs = 64`) |
| `max_upscale_width/height` | 8192 |

При переполнении очереди `POST /sdcpp/v1/img_gen` отдаёт **429** `{"error":"job queue is full"}`.

**Результат completed img_gen:**
```json
{"status":"completed","result":{"output_format":"png",
 "images":[{"index":0,"b64_json":"iVBORw0KGgo..."}]},"error":null}
```

**Capabilities — что важно для враппера:**
- `features_by_mode.img_gen`: `init_image, mask_image, control_image, ip_adapter_image, ref_images, lora, vae_tiling, hires, cache, cancel_queued:true, cancel_generating:false`.
- `limits` — единственный способ узнать границы до отправки запроса.
- `upscalers[].image_upscale` — можно ли выбрать этот upscaler для `POST /sdcpp/v1/upscale`.
- `model.{name,stem,path}` — текущая загруженная модель (полезно для health-check враппера).
- `current_mode`, `supported_modes` (`img_gen` и/или `vid_gen`) — можно ли вообще генерировать видео на этой модели.
- Верхнеуровневые `defaults`, `output_formats`, `features` — **deprecated** зеркала `*_by_mode`; использовать только mode-aware поля.

### 4.4 Какой API выбрать как основной контракт для воркера за балансировщиком

**Однозначно `/sdcpp/v1/*` (нативный async).** Обоснование по каждому требованию:

| Требование | `/sdcpp/v1` | `/sdapi/v1` | `/v1/images/generations` |
|---|---|---|---|
| Очередь | **есть**, FIFO, `queue_position`, лимит 64, **429** | нет (sync) | нет (sync) |
| Отмена | **есть** для `queued`; для `generating` — нет (409) | нет | нет |
| Лимиты | `GET /capabilities.limits` | частично | нет |
| Несколько параллельных клиентов | HTTP-слой многопоточный (httplib), но **исполнение сериализовано мьютексом** | то же | то же |
| Полнота управления | полная (sample_params, guidance.slg, hires, vae_tiling, cache) | подмножество + `<sd_cpp_extra_args>` | минимум + `<sd_cpp_extra_args>` |
| Статус/прогресс | статус есть, **прогресса шагов нет** | нет | нет |
| Изображение клиенту | `result.images[].b64_json` после polling | сразу `images[]` | сразу `data[].b64_json` |

**Практический вывод для балансировщика:**
- Держать **свою** очередь в cppworker (как уже сделано для llama.cpp), а `sd-server` использовать как односоставный исполнитель: `POST /sdcpp/v1/img_gen` → poll `GET /sdcpp/v1/jobs/{id}` → забрать `b64_json`.
- `queue_position` и внутренняя очередь sd-server (64) — не использовать как основной механизм: у воркера уже есть `--max-queue`/priority и единый контракт с llama.cpp. Иначе получится двойная очередь и потеря контроля.
- **Отмена в полёте недоступна.** Варианты: (a) не обещать клиенту mid-flight cancel; (b) реализовать cancel-by-restart (убить процесс и поднять заново — цена = перезагрузка модели); (c) патчить upstream (в C-API примитивы `sd_progress_cb_t` и `sd_cancel_generation` уже есть, в сервер не проброшены).
- **Прогресс:** только через парсинг `stderr` (`-v` даёт построчный лог) либо не показывать прогресс. Формат лога не является контрактом — хрупко.
- Проверять `features_by_mode.img_gen.cancel_queued` и `limits` при старте, а не хардкодить: API молод и меняется.

### 4.5 Одна модель на процесс — подтверждено

- `sd_ctx` создаётся **один раз** в `main()` до `svr.listen()`; все хендлеры получают указатель на него через `ServerRuntime`.
- **Эндпоинта reload/switch модели нет.** `POST /sdapi/v1/options` не зарегистрирован; `GET /sdapi/v1/options.sd_model_checkpoint` только читает.
- LoRA-файлы сканируются заново на каждый запрос (`refresh_lora_cache`) — **LoRA можно добавлять на диск без рестарта**, но подхватится это только как элемент `lora[]` в конкретном запросе (LoRA применяется per-request, а не персистентно).
- `--hires-upscalers-dir` тоже пересканируется (`refresh_upscaler_cache`) — ESRGAN можно докидывать.
- Смена diffusion/VAE/TE = **рестарт процесса**.

**Следствие для враппера:** «load/unload модели» реализуется как управление дочерним процессом (spawn/kill), а не как API-вызов. Ровно как с llama.cpp.

---

## 5. Сборка и деплой

### 5.1 Готовые бинарные релизы (release `master-929-3f8527a`, 2026-09-27)

| Ассет | Размер |
|---|---|
| `sd-master-<sha>-bin-win-cuda12-x64.zip` | 337.9 MB |
| `cudart-sd-bin-win-cu12-x64.zip` (рантайм CUDA) | 563.5 MB |
| `sd-master-<sha>-bin-win-rocm-7.14.0-x64.zip` | 203.4 MB |
| `sd-master-<sha>-bin-win-vulkan-x64.zip` | **30.1 MB** |
| `sd-master-<sha>-bin-win-cpu-x64.zip` | 17.5 MB |
| `sd-master-<sha>-bin-Linux-Ubuntu-24.04-x86_64.zip` (CPU) | 26.0 MB |
| `sd-master-<sha>-bin-Linux-Ubuntu-24.04-x86_64-vulkan.zip` | **36.9 MB** |
| `sd-master-<sha>-bin-Linux-Ubuntu-24.04-x86_64-rocm-7.14.0.zip` | 278.0 MB |
| `sd-master-<sha>-bin-Darwin-macOS-26.6.2-arm64.zip` | 35.0 MB |

**Чего нет:** macOS x86_64 (Intel), Windows ARM, Linux CUDA (только ROCm/Vulkan/CPU). Для Intel Mac — своя сборка.
Источник: [releases API](https://api.github.com/repos/leejet/stable-diffusion.cpp/releases/latest).

**Вывод:** для деплоя на слабых/не-NVIDIA картах **Vulkan-сборка — 30–37 MB**, это самый лёгкий путь. CUDA-сборка тянет за собой cuDNN? — **нет**, отдельный `cudart`-архив содержит рантайм CUDA, cuDNN в зависимостях не упоминается; сборка требует CUDA Toolkit, но не cuDNN.

### 5.2 Сборка из исходников

```
git clone --recursive https://github.com/leejet/stable-diffusion.cpp
cd stable-diffusion.cpp
```
**CPU:** `cmake -B build && cmake --build build --config Release`
**CUDA:** `cmake -B build -DSD_CUDA=ON` (нужен CUDA Toolkit; рекомендуется ≥4 GB VRAM)
**Vulkan:** `cmake -B build -DSD_VULKAN=ON` (нужен Vulkan SDK; для Ubuntu — `libvulkan-dev glslc spirv-headers`)
**ROCm:** `cmake -B build -G Ninja -DCMAKE_C_COMPILER=clang -DCMAKE_CXX_COMPILER=clang++ -DSD_HIPBLAS=ON -DCMAKE_BUILD_TYPE=Release -DGPU_TARGETS=$GFX_NAME -DAMDGPU_TARGETS=$GFX_NAME -DCMAKE_BUILD_WITH_INSTALL_RPATH=ON -DCMAKE_POSITION_INDEPENDENT_CODE=ON`
**Metal:** `-DSD_METAL=ON` · **OpenCL (Adreno):** `-DSD_OPENCL=ON` · **SYCL (Intel):** `-DSD_SYCL=ON` · **MUSA:** `-DSD_MUSA=ON` · **OpenBLAS:** `-DGGML_OPENBLAS=ON`
**WebP/WebM:** включены по умолчанию (`-DSD_WEBP=OFF -DSD_WEBM=OFF` чтобы выключить; `-DSD_USE_SYSTEM_WEBP=ON` для системных пакетов)
**Встроенный WebUI:** `-DSD_SERVER_BUILD_FRONTEND=ON` (требует Node ≥20 и pnpm ≥10)
Источник: [docs/build.md](https://raw.githubusercontent.com/leejet/stable-diffusion.cpp/master/docs/build.md).

### 5.3 Как встроить рядом с llama.cpp в один CMake/репозиторий

- **Хорошая новость:** sd.cpp использует **тот же ggml** ([ggml-org/ggml](https://github.com/ggml-org/ggml)), но **патченный форк** в подмодуле `ggml/`. Есть переключатели:
  - `SD_USE_UPSTREAM_GGML=ON` + `SD_GGML_SOURCE_DIR=../ggml-upstream` — собрать против внешнего/апстримного ggml;
  - `SD_USE_SYSTEM_GGML=ON` + `ggml_DIR`/`CMAKE_PREFIX_PATH` — слинковаться с уже установленным GGML-пакетом (требует совпадения ABI, включая `GGML_MAX_NAME`).
- **Подводные камни при upstream-режиме:** FP8 safetensors будут конвертированы в F16 при загрузке (1 байт в файле → 2 в RAM/VRAM); **INT8 tensorwise/convrot отключён**, такие файлы отвергаются; **FP8 GGUF отвергаются**; часть операторов и оптимизаций патченного ggml отсутствует — CMake выдаёт предупреждение.
- **Следствие:** «собрать sd.cpp и llama.cpp одним CMake с одним ggml» реально, но **либо** вы принимаете апстрим-ggml с потерей части возможностей sd.cpp (и должны убедиться, что версия llama.cpp совместима), **либо** держите два дерева ggml и получаете риск конфликта символов/ABI при линковке в один бинарь.
- **Рекомендация для балансировщика:** не линковать sd.cpp в cppworker, а **запускать `sd-server` отдельным процессом** — ровно та же модель, что уже используется для llama.cpp. Тогда вопросы CMake/ABI/ggml-конфликтов исчезают полностью, а воркер общается по HTTP. Плюсы: изоляция падений, независимый апгрейд, независимый выбор бэкенда (CUDA для llama, Vulkan для sd), простой kill при OOM.
- **Но встраивание реально и проверено:** **KoboldCpp собирает llama.cpp + stable-diffusion.cpp + TTS.cpp в одном `CMakeLists.txt`**. Это работающий прецедент, а не гипотеза (§7.1).

### 5.4 Примеры запуска `sd-server` для 6 GB

**SDXL-Turbo, q8_0 GGUF (4.10 GB), 6 GB VRAM:**
```
sd-server.exe \
  --model sd_xl_turbo_1.0.q8_0.gguf \
  --listen-ip 127.0.0.1 --listen-port 18093 \
  --threads 8 \
  --diffusion-fa \
  --offload-to-cpu \
  --max-vram -1 \
  --steps 4 --cfg-scale 1.0 \
  --sampling-method euler \
  -v
```
Здесь: `--offload-to-cpu` = веса в RAM, подгрузка по требованию; `--diffusion-fa` снижает пик; `--max-vram -1` резервирует ~1 GiB от свободной памяти на старте; `--steps 4 --cfg-scale 1.0` — режим turbo.

**FLUX.1-schnell Q4_0 (6.77 GB), 6 GB VRAM с offload:**
```
sd-server.exe \
  --diffusion-model flux1-schnell-Q4_0.gguf \
  --vae ae.safetensors \
  --clip_l clip_l.safetensors \
  --t5xxl t5xxl_fp16.safetensors \
  --listen-ip 127.0.0.1 --listen-port 18093 \
  --diffusion-fa --offload-to-cpu --max-vram -1 \
  --backend te=cpu \
  --vae-conv-direct --vae-tiling --vae-tile-size 512 --vae-tile-overlap 0.5 \
  --steps 4 --cfg-scale 1.0 --sampling-method euler \
  -v
```
Ключевое: `--backend te=cpu` убирает T5-XXL (≈9.8 GB fp16!) из VRAM; `--vae-tiling` спасает decode на 1024². T5 на CPU — реальная цена по времени, но иначе в 6 GB не влезть.
> Альтернатива: `--t5xxl t5xxl-Q5_0.gguf` — квантованный T5 (упоминается в [flux.md](https://raw.githubusercontent.com/leejet/stable-diffusion.cpp/master/docs/flux.md) и в конфиге LocalAI).

**Z-Image-Turbo Q3_K (3.14 GB) + Q4_K_M TE (2.50 GB) на 4 GB (эталон из wiki):**
```
sd-cli.exe --diffusion-model z_image_turbo-Q3_K.gguf --vae ae.safetensors \
  --llm Qwen3-4B-Instruct-2507-Q4_K_M.gguf -p "..." \
  --cfg-scale 1.0 --offload-to-cpu --diffusion-fa -H 1024 -W 512 --steps 8
```
Для сервера — те же флаги плюс `--listen-ip/--listen-port`; `--clip-on-cpu` здесь = `--backend te=cpu`.

### 5.5 Vulkan

Vulkan официально поддерживается (`-DSD_VULKAN=ON`, готовые бинари Windows/Linux). Это **основной путь для AMD и Intel-карт**. Из обсуждения #1026 видно, что Vulkan на RDNA/Polaris работает (RX 470/580), но:
- **провал производительности из-за Mesa:** `RADV_PERFTEST=nogttspill` дал ускорение **16 s/it → 7.3 s/it** (>2×) на RX 580.
- Mesa RADV рапортует `maxMemoryAllocationSize` = 4 GiB → **большой VAE decode падает даже при свободных десятках GB**. Лечится `--vae-tiling` (это прямо задокументировано в LocalAI: *"a large decode fails there even with tens of GB free"*).
- **Известный баг:** `unsloth/Z-Image-GGUF` Q4_K_M даёт **чёрные картинки на ROCm**, при этом на Vulkan всё нормально.

---

## 6. Практика: цифры, OOM, CPU-only

### 6.1 Измеренные цифры

**Z-Image-Turbo, 512×512 (приведено в s/it из обсуждения #1026; итог посчитан для 8 шагов — рекомендованных stduhpf'ом):**

| GPU | Квант | s/it | Итоговая картинка (8 шагов) |
|---|---|---|---|
| RX 580 4 GB Vulkan (после фикса RADV) | Q3_K | 7.3 | ~58 с |
| RX 470 Vulkan | Q4_K | 7 | ~56 с |
| RX 470 Vulkan | Q3_K | 7.5 | ~60 с |
| RX 470 Vulkan @1024×1024 | Q4_K | 35 | ~4.7 мин |
| RTX 2070 | Q3_K_M | 1.19 | ~10 с |
| RTX 3050 Laptop | — | 2.17 | ~17 с |

> Формулировка «s/it» взята из обсуждения дословно; если авторы имели в виду **полный проход**, а не шаг, все итоговые цифры делятся на 8. Это **основной источник неопределённости** в таблице — при планировании закладывать диапазон 7–60 с на RX 470 при 512².

### 6.1.1 Реальные замеры wall-clock из логов (второй источник)

| GPU | Модель + квант | Разрешение | Шаги | С/картинку | Источник |
|---|---|---|---|---|---|
| **GTX 1060 6GB** | Z-Image **base** Q3_K_M + Qwen3-4B Q4_K_M | 512×1024 | 20 | **341.4** всего (16.21 s/it; sampling 324.5 + VAE 13.1) | [#1253](https://github.com/leejet/stable-diffusion.cpp/issues/1253) |
| GTX 1060 6GB | Qwen-Image-2512 Q4_K_M/Q5_K_M | 1024² | 40 | **чёрные картинки** (баг); Q5_0 работает | [#1385](https://github.com/leejet/stable-diffusion.cpp/issues/1385) |
| **RTX 2060 6GB** | FLUX.2-klein-4B Q8_0 + Qwen3-4B Q4 | 512² | 2 | **~1.4 с/шаг** | [#1989](https://github.com/leejet/stable-diffusion.cpp/issues/1989) |
| **GTX 1650 4GB** | Qwen-Image-2.1 **Q2_K** + Qwen3-VL-8B Q2_K | 512² | 4 | sampling OK, **VAE OOM** (3445 vs 2679 MB); авто-retry 16×16/9 тайлов → **~38 с** | [#2051](https://github.com/leejet/stable-diffusion.cpp/issues/2051) |
| **RTX 3060 12GB** | Z-Image-Turbo Q8_0, без offload | 688×1024 | 12 | **~29 с** (115 с за 4 картинки) | [PR#1477](https://github.com/leejet/stable-diffusion.cpp/pull/1477) |
| RTX 3060 12GB | то же + `--vae-on-cpu` | 688×1024 | 12 | **~150 с** — **штраф 5×!** | [PR#1477](https://github.com/leejet/stable-diffusion.cpp/pull/1477) |
| RTX 3060 12GB | FLUX-schnell Q4_K_S + PuLID | 512² | 4 | **9.6 с** CUDA / **11 с** Vulkan | [PR#1542](https://github.com/leejet/stable-diffusion.cpp/pull/1542) |
| RTX 3060 12GB | FLUX-dev Q4_K_S + PuLID | 1024² | 20 | **OOM**, пока не добавлен `--backend vae=cpu` | [PR#1542](https://github.com/leejet/stable-diffusion.cpp/pull/1542) |
| **RX 580 8GB Vulkan** | DreamShaper 8 (SD1.5) | 512² | 50 | **71.5 с** (sampling) | [benchmarks](https://huggingface.co/aivisionslab/ai-local-rx580-stack/raw/main/docs/benchmarks.md) |
| RX 580 8GB Vulkan | flux1-schnell **q4_k** (leejet) + T5 на CPU | 512² | 4 | ~84 с Win / **~52 с sampling** Linux RADV (~95 с всего) | [README](https://huggingface.co/aivisionslab/ai-local-rx580-stack/raw/main/README.md) |
| RX 580 8GB Vulkan | flux1-schnell q4_k | 1024² | 4 | **~14 мин** = T5 11.5 + sampling **838** + VAE 40.5 (9 тайлов) | [same](https://huggingface.co/aivisionslab/ai-local-rx580-stack/raw/main/docs/benchmarks.md) |
| RX 580 8GB Vulkan | Anima base | 1152×896 | 8 | **9.5 s/it** (коммит 19bdfe2) против **22–23 s/it** (9b0fceb) — **регрессия 2.3×** | [#1647](https://github.com/leejet/stable-diffusion.cpp/issues/1647) |
| Steam Deck iGPU | SD1.5 f16 + LCM | 512² | 8 | **~30 с** | [Steam](https://steamcommunity.com/app/1675200/discussions/0/595134792309237790/) ⚠️2024 |
| Steam Deck iGPU | SDXL 1.0 | 1024² | 8 | **≥360 с** (120 с только с lossy `--taesd`) | [same](https://steamcommunity.com/app/1675200/discussions/0/595134792309237790/) |
| 4 GB laptop + 8 GB RAM ⚠️ | FLUX-schnell **Q3** + t5-xxl-Q8_0 | 1024² | 4 | **~50 с** (вытеснение в RAM) | [Civitai](https://civitai.com/articles/10297/flux-on-4gb-vram-and-8gb-ram) |
| 4 GB NVIDIA (A1111) ⚠️2023 | SDXL-Turbo fp16 | 512² | 1 | **4.5 с** | [blog](https://ncos1.hatenablog.com/entry/2023/11/30/190000) |
| GTX 1070 ⚠️2023 | SD1.5 f16 | 512² | 20 | **164.7** всего (7.42 s/it) | [#95](https://github.com/leejet/stable-diffusion.cpp/issues/95) |

**Опорные значения it/s и s/it (для экстраполяции):**
- RX 7800 XT Vulkan, SD1.5 512²: **7.77 it/s** (→ ~4.5 с при 35 шагах); SDXL 1024²: **1.27 it/s** (→ ~27.6 с при 35 шагах) ([PR#2085](https://github.com/leejet/stable-diffusion.cpp/pull/2085)).
- RX 5700 XT, SD1.5 512²: **2.57 it/s** (→ ~13.6 с при 35 шагах).
- RTX 3080 Laptop 8GB, Z-Image-Turbo Q4_K_M 1024²: **2.39 s/it → 19.7 с при 7 шагах**. RTX 3090 Ti 1.13 s/it; RTX 4070 Ti 1.25; M4 Pro 24GB 14.5–15.1 s/it ([данные](https://raw.githubusercontent.com/miroleon/z-image-turbo-benchmark/main/assets/data/benchmarks.json)).
- RX 7900 XTX, Qwen-Image-2.1 Q8_0 1024²: **139 с** всего ([#2015](https://github.com/leejet/stable-diffusion.cpp/issues/2015)).
- **CPU-only: 80 с/шаг против ~10 с/шаг на GPU** → SD1.5 ≈ 1600 с/картинку ([#48](https://github.com/leejet/stable-diffusion.cpp/issues/48)).

**Ключевой вывод по 4 GB:** GTX 1650 4GB **смогла** прогнать Qwen-Image-2.1 Q2_K на 512² — но упёрлась в VAE и спаслась только автоматическим retry тайлинга (**~38 с** на 4 шага). Это ровно тот сценарий, где `--vae-tiling` надо ставить **заранее**, а не ждать OOM.

### 6.1.2 ⚠️ Баг с `seed = -1` (критично для сервера)

Независимо зафиксирован **integer overflow при случайном seed** (`Seed: -1`), приводящий к `generate_image returned no results` и **зависанию терминала**; обход — всегда передавать **фиксированный положительный seed** (напр. 42, 1337). ([источник, раздел Troubleshooting](https://huggingface.co/aivisionslab/ai-local-rx580-stack/raw/main/README.md))

Это согласуется с наблюдением из обсуждения #1026 (engrtipusultan получал разные результаты, пока не задал положительный seed). **Для враппера это означает:** генерировать seed на стороне воркера и всегда передавать его в `POST /sdcpp/v1/img_gen` — не полагаться на `seed: -1`.

**FLUX.1-dev память (пик, официально):** q8_0 12.07 GB · q4_0 6.39 GB · q4_K 6.40 GB · q3_K 4.89 GB · q2_K 3.74 GB.

**SD1.5 512×512 (официально):** ~2.0 GB (q4_0…q5_1), ~1.5 GB с `--diffusion-fa`.

> **Что НЕ подтверждено первоисточниками и помечается как ненадёжное:** конкретные «секунды на картинку» для SD1.5 Q8 512², SDXL-Turbo Q4/Q8 512–1024 и FLUX-schnell Q4 1024² на GTX 1060 6 GB / RTX 2060 / RTX 3060 12 GB. Опубликованных замеров именно для `sd.cpp` на этих картах в рамках ресёрча не найдено; сторонние таблицы (willitrunai и подобные) дают VRAM-оценки для ComfyUI/A1111, а не для sd.cpp, и переносить их числа напрямую нельзя.

### 6.2 Ориентиры для планирования (экстраполяция, помечено как оценка)

Известны: 1.19 s/it (RTX 2070, Q3_K_M, 512²) и 2.17 s/it (RTX 3050 Laptop, 512²). GTX 1060 6 GB по FP16-производительности примерно в 1.5–2× медленнее RTX 2070 → **ожидаемо ~2–3 s/it на 512²**, т.е. **~20–30 с** на 8-шаговую Z-Image-Turbo. Для FLUX-schnell 4 шага на 1024² активации в 4× больше → **порядка 1.5–4 минут** на GTX 1060 при Q4 с offload. Это **оценка, не замер** — обязательно перемерить на целевом железе.

### 6.3 Что делать при OOM — по порядку возрастания потерь

1. **Понизить квант** diffusion: Q8 → Q6 → Q5 → Q4_K → Q4_0 → Q3_K → Q2_K. Замеры FLUX показывают ступени 12.07 → 9.86 → 8.28 → 6.40 → 6.39 → 4.89 → 3.74 GB.
2. **`--diffusion-fa`** — flux 768² ≈ **−600 MB**, SD2 768² ≈ **−1400 MB**.
3. **`--backend te=cpu`** (бывший `--clip-on-cpu`) — убрать текстовый энкодер из VRAM. Для FLUX это ~9.8 GB T5 или ~2.5 GB Qwen3-4B. В #1026 замерено: Qwen3-4B на CPU = **~823 мс** на conditioner. Очень дешёво.
4. **`--backend vae=cpu`** (бывший `--vae-on-cpu`) — VAE decode на CPU. ⚠️ **Измеренный штраф — 5× по времени** (29 с → 150 с на RTX 3060, Z-Image-Turbo 688×1024, [PR#1477](https://github.com/leejet/stable-diffusion.cpp/pull/1477)). **Применять только если тайлинг не помог.**
5. **`--vae-tiling --vae-tile-size 512 --vae-tile-overlap 0.5`** — если падает **именно decode** (в логе `sampling completed`, затем `failed to allocate the compute buffer`). При 512×512 с дефолтным tile 256 и overlap 50% получается 3×3 тайла. Меньше тайл = меньше пик, но больше времени и возможны швы.
   > ⚠️ **Практическое подтверждение: на RX 580 для FLUX `--vae-tiling` обязателен.** Без него VAE decode даёт OOM и **роняет сервер** (в варианте с Linux — вместе с GNOME display server). Формулировка автора отчёта: *«`--vae-tiling` is not optional»*. То есть автоматический retry, описанный в `docs/performance.md`, на этом железе/бэкенде недостаточен. ([источник](https://huggingface.co/aivisionslab/ai-local-rx580-stack/raw/main/README.md))
6. **`--vae-conv-direct`** — снижает VRAM при decode (иногда медленнее).
7. **`--taesd`** — tiny autoencoder вместо VAE; быстрый и лёгкий decode ценой качества.
8. **`--offload-to-cpu`** (= `--params-backend '*=cpu'`) — веса в RAM, стриминг по сегментам. Официально «без снижения скорости»; на практике зависит от PCIe.
9. **`--params-backend diffusion=disk`** — минимум и VRAM, и RAM, но перечитывает файл на каждом сегменте → медленно.
10. **`--max-vram -1`** — резерв ~1 GiB от свободной памяти на старте (лечит OOM из-за драйверных аллокаций и фрагментации).
11. **Понизить разрешение** либо применить `--hires` после базовой генерации.

**Важно:** автоматический retry при OOM работает **только для decode VAE**, не для encode. Если падает encode (img2img), `--vae-tiling` надо ставить явно.

### 6.4 Tiny autoencoders — точные размеры

| Файл | Размер | Для каких моделей |
|---|---|---|
| `taesd` | **9.79 MB** | SD1.x / SD2.x |
| `taesdxl` | **9.79 MB** | SDXL, SDXL-Turbo |
| `taesd3` | **9.85 MB** | SD3 (требует `shift_factor=0.0`) |
| `taef1` | **9.85 MB** | FLUX.1, HiDream, **Z-Image** |
| `taef2` | **10.72 MB** | FLUX.2 |
| `taew2_1` (TAEHV) | **22.64 MB** | **Qwen-Image**, Wan2.1, Wan2.2-A14B |
| `taew2_2` (TAEHV) | 22.85 MB | Wan2.2-TI2V-5B |
| `taehv` / `taehv1_5` / `taeh3` / `taecvx` / `taeos1_3` | 22.64–22.76 MB | соответствующие видео-модели |
| `taeltx_2` / `taeltx2_3` | 23.53 MB | LTX |

**Насколько это меньше полного VAE:** SD-VAE имеет **34 163 592 параметра в энкодере и 49 490 179 в декодере**; у TAESD — **1 222 532 / 1 222 531** (≈ **1/34** от декодера). По файлу: **9.8 MB против 335 MB**.

**Измеренная экономия (TAEHV, GH200, fp16, 61 кадр @512×320):** полный Hunyuan VAE — **~2–3 с, пик ~6–9 GB**; TAEHV — **~0.5 с, пик <0.5 GB**. ([taehv](https://github.com/madebyollin/taehv))
**Единственный end-to-end замер в sd.cpp:** Steam Deck, SDXL 1024² — **360 с → 120 с** с `--taesd` (автор называет результат «lossy»).

**Использование:** `--taesd <file>`; для Qwen-Image/Wan — `--tae taew2_1.safetensors`. Если всё ещё OOM — добавить `--vae-conv-direct`. ([taesd.md](https://raw.githubusercontent.com/leejet/stable-diffusion.cpp/master/docs/taesd.md))
⚠️ HF-репозиторий `madebyollin/taehv` **gated (401)**; веса TAEHV берутся с [GitHub](https://github.com/madebyollin/taehv).

### 6.5 Есть ли смысл в CPU-only режиме

- **Технически да, режим есть и полностью поддержан** (`-DSD_CPU`-сборка по умолчанию, AVX/AVX2/AVX512). Готовые CPU-бинари: Windows 17.5 MB, Linux 26.0 MB.
- **Практически — только для SD1.5.** Официальная таблица SD1.x даёт ~2.8 GB (f32) / ~1.5–2.0 GB (q4_0) — это про память, не про время. CPU-инференс SD1.5 на 512² — десятки секунд на современном 8-ядернике.
- **Для DiT-моделей (FLUX, Z-Image, Qwen, SD3) CPU-only не имеет смысла.** Один только текстовый энкодер Qwen3-4B — 823 мс, но diffusion-проход Z-Image на CPU будет на порядки медленнее. SYCL-замер в блоге для FLUX-schnell 1024²: **~28–30 минут на картинку** даже на Intel GPU с SYCL, при 313–333 с на шаг. ([siriuth.blogspot.com](https://siriuth.blogspot.com/2026/05/stable-diffusioncpp-flux1-schnell-sycl.html)) — это GPU, CPU будет хуже.
- **Разумная гибридная схема:** diffusion на GPU, **текстовый энкодер и/или VAE на CPU** (`--backend te=cpu,vae=cpu`) — это документированный и дешёвый способ влезть в 4–6 GB.
- **`disk` params-backend на CPU-only** — единственный способ запустить Qwen-Image вообще без GPU, но это сценарий «работает, а не быстро».

---

## 7. Альтернативы (кратко)

| Движок | HTTP API | GGUF-диффузия | Hot-swap модели | Мин. VRAM | Вес/сложность |
|---|---|---|---|---|---|
| **sd.cpp / `sd-server`** | 3 семейства: `/sdcpp/v1` (async), `/sdapi/v1`, `/v1/images/*` | **да, родной** | **нет** — одна модель на процесс, смена = рестарт | 4 GB (Z-Image Q3_K / Chroma) | один статический C++ бинарь 17–37 MB (CPU/Vulkan) |
| **LocalAI** (`stablediffusion-ggml`) | `/v1/images/generations` (**OpenAI-совместимый**), `/v1/images/upscale` | **да** — это обёртка над sd.cpp | **да, на уровне API** (см. ниже) | наследует sd.cpp | Go-сервер (~один бинарь) + бэкенды; **тяжелее, но даёт ровно то, что нужно балансировщику** |
| **Ollama** (эксперим., с 2026-01) | `/api/generate` (с `width/height/steps`, base64 в `image`) + `/v1/images/generations` | **нет** — MLX, не sd.cpp; GGUF-диффузии нет | **да, нативно**: пустой prompt = load, `keep_alive:0` = unload, `GET /api/ps` + `size_vram` | — | Go; **фича УДАЛЕНА в v0.32.6**, macOS-only |
| **KoboldCpp** | A1111 `/sdapi/v1/*` + Ollama API + OpenAI `/v1` (текст) | **да** — офиц. модель `picx_real_q5_1.gguf`, `--sdquant` | частично (`autoswapmode`); per-request SD-swap не задокументирован | ~4.25 GB (нетипизир.) / офиц. требование 8 GB+ | single-file C++; **собирает llama.cpp + sd.cpp + TTS.cpp в одном CMake** — доказательство встраиваемости; AGPL |
| **mold** (Rust/candle) | `POST /api/generate`, **`POST /api/models/load`**, **`DELETE /api/models/unload`** | **да** — GGUF-тиры `:q4`/`:q8` | **да — явные load/unload** | klein:q4 ~5 GB; `--offload` → 2–4 GB пик (3–5× медленнее) | Rust; **не встраивается в C++/CMake** |
| **ComfyUI** | `POST /prompt` (API-формат workflow) + WebSocket + `/history`; локальный API для интеграции | через ComfyUI-GGUF; нативные «quantized models» | да (частичный пересчёт графа, отгрузка моделей) | **заявляет 4 GB VRAM + 8 GB RAM** на async weight streaming | Python 3.13 + torch 2.7+ + custom nodes; **официальные portable-сборки для AMD и Intel**, и отдельная cu126-сборка **для NVIDIA 10-серии и старше** |
| **A1111 WebUI** | `/sdapi/v1/*` | частично (расширения) | **да** — `POST /sdapi/v1/options` с `sd_model_checkpoint` | 6 GB на SD1.5/SDXL | Python + torch, тяжёлый; почти не развивается |
| **Forge (webui-forge)** | совместим с A1111 `/sdapi` | **да** — «GGUF Q8_0/Q5_0/Q5_1/Q4_0/Q4_1 natively supported» | **да** — был сломан, теперь починен | ниже A1111 (оптимизации памяти) | Python + torch, ~1.87 GB установщик |
| **Fooocus** | **своего generation API НЕТ** — только Gradio | нет (SDXL-only) | только per-request `base_model_name` | 4 GB NVIDIA + 8 GB RAM + swap | Python + torch; нужна обёртка |
| **Fooocus-API** (mrhan1993) | `POST /v1/generation/text-to-image`, `/v2/generation/text-to-image-with-ip`, `POST /v1/generation/stop` | нет | нет | наследует Fooocus | FastAPI-обёртка; очередь `--queue-size` 100 + webhook |
| **InvokeAI** | `/api/v2/models/*` (install/convert/**empty_model_cache**), генерация `POST /api/v1/queue/default/enqueue_batch` | **да** — GGUF-лоадеры есть (нельзя ре-энкодить) | **да** — модель-кэш, частичная загрузка слоёв | **4 GB SD1.5 / 8 GB SDXL / 8 GB Z-Image Q4_K** | Python + torch, тяжёлый |
| **SD.Next** | API вкл. по умолчанию + Swagger `/docs`; расширенный `/sdapi/v1/*` | ⚠️ **только как storage** — «not compatible with model offloading» | да, но по умолчанию **2 загрузки на запрос** | `--medvram`/`--lowvram` | Python + torch; реальные кванты — SDNQ/BnB-nf4 |
| **SwarmUI** | `POST /API/GenerateText2Image`, `/API/SelectModel`, **`/API/FreeBackendMemory`** | через ComfyUI → не подтверждено | **да** — per-request `model` | зависит от ComfyUI | .NET 8 + Python + ComfyUI + torch |
| **diffusers-FastAPI** (HF `examples/server`) | только `POST /v1/images/generations`, отдаёт **URL, не b64** | ⚠️ **«Loading GGUF checkpoints via Pipelines is currently not supported»** | **нет** — один `shared_pipeline` на старте | ~4 GB SD1.5 fp16+offload | Python + torch; максимум контроля, максимум веса |
| **sd.cpp-webui** | своего нет (Gradio); в `--server` режиме проксирует sd-server | да (наследует) | CLI-режим: **новый процесс на каждую генерацию** | наследует sd.cpp | Python + Gradio, **без PyTorch** |

**Про Ollama-подобный API:** готового работающего «Ollama для картинок» **нет**. Ollama вводил генерацию изображений в v0.14.3 и **удалил** её к v0.32.6. KoboldCpp и LocalAI переэкспонируют Ollama API, но только для текста/эмбеддингов. **Схему hot-swap у Ollama копировать стоит; саму Ollama для картинок — нет.**

#### Ollama: генерация изображений — ⚠️ ДОБАВЛЕНА И ЗАТЕМ УДАЛЕНА

- **Анонс 2026-01-20:** генерация изображений экспериментально, **только macOS**; «Windows and Linux coming soon». Модели `x/z-image-turbo` (**Z-Image-Turbo, 6B**, Apache 2.0) и `x/flux2-klein` (4B/9B). CLI: `ollama run x/z-image-turbo "prompt"`. Настройки `/set width`, `/set height`, steps, seed, negative prompt. ([blog](https://ollama.com/blog/image-generation))
- **API в v0.14.3:** `/api/generate` принимал `width`/`height`/`steps` и возвращал base64 в поле `image`; in-tree `x/imagegen/api` описан как *«Package api provides OpenAI-compatible image generation API types»* (`ImageGenerationRequest{model,prompt,n,size,response_format,stream}`).
- **❌ НО в v0.32.6 генерация изображений БЫЛА УДАЛЕНА:** release notes — *«Experimental image generation has been temporarily removed. Continue using 0.32.5»*, плюс коммит *«imagegen: remove MLX image generation code (#16615)»*. На 0.32.14+ `POST /api/generate` возвращает **HTTP 400** `"image generation models are not currently supported"`, при этом `/api/tags` **всё ещё рекламирует** `capabilities:["image"]` — то есть capability-репорт врёт. ([v0.32.6](https://github.com/ollama/ollama/releases/tag/v0.32.6), [issue #17893](https://github.com/ollama/ollama/issues/17893))
- **Только MLX, GGUF-диффузии нет.**

> **Вывод: НЕ планировать image-gen на Ollama.** Фича присутствовала в 0.14.3 и вырезана к 0.32.6; API репортит capability, которого нет. Это же — причина, по которой «Ollama-подобный API для картинок» остаётся нерешённой задачей: ни у Ollama, ни у кого-либо ещё нет работающего Ollama-shaped **image**-эндпоинта. KoboldCpp и LocalAI переэкспонируют Ollama API, но только для текста/эмбеддингов.
> При этом **сама схема hot-swap у Ollama по-прежнему образцовая** и её стоит скопировать: пустой prompt = load, `keep_alive: 0` = unload, `GET /api/ps` со `size_vram`.

**LocalAI — конкретные эндпоинты управления моделями (проверено по коду роутера):** ([core/http/routes/localai.go](https://raw.githubusercontent.com/mudler/LocalAI/master/core/http/routes/localai.go))

| Endpoint | Метод | Назначение |
|---|---|---|
| `POST /backend/load` и `POST /v1/backend/load` | POST | **явная предзагрузка модели** («inverse of /backend/shutdown») — прогрев вместо cold-start на первом запросе |
| `POST /backend/shutdown` и `/v1/backend/shutdown` | POST | выгрузка из памяти |
| `GET /backend/monitor` | GET | какие модели загружены |
| `POST /models/reload` | POST | **перечитать конфиги моделей** |
| `POST /models/apply` / `/models/delete/:name` / `/models/import` / `/models/import-uri` / `/models/edit/:name` | POST | установка/удаление/импорт/правка моделей |
| `PUT /models/toggle-state/:name/:action`, `/models/toggle-pinned/:name/:action` | PUT | вкл/выкл, пиннинг в памяти |
| `GET /api/models/:id/load-status` | GET | прогресс cold-load (доступен без admin) |
| `GET /api/models/vram-estimate` | GET | **оценка VRAM под модель** |
| `GET /.well-known/localai.json` | GET | discovery: список эндпоинтов + capabilities |
| `GET /api/failover*`, `POST /api/failover/:chain/pin` | — | цепочки failover — прямо релевантно балансировщику |

Дополнительно LocalAI поддерживает `rpc_servers` (те же `rpc-server` воркеры, что у llama.cpp) для шардинга диффузии — то есть один пул RPC-воркеров может обслуживать и текст, и картинки.

**Рекомендация по выбору:**
- Если цель — **минимальный вес и полный контроль**, а балансировщик уже умеет спавнить процессы и проксировать HTTP (как для llama.cpp) → **`sd-server` как дочерний процесс**. Ничего тяжелее 37 MB, никакого Python/torch, Vulkan для слабых карт, ядро — тот же ggml-стек.
- Если нужен **hot-swap моделей без рестарта и OpenAI-контракт «из коробки»** → **LocalAI** с бэкендом `stablediffusion-ggml`. Цена: ещё один Go-сервер в стеке и потеря прямого доступа к `/sdcpp/v1` (async/queue/cancel).
- **ComfyUI/InvokeAI/A1111/Forge** для 4–6 GB VRAM и «backend с HTTP API» — избыточны: Python+torch на порядок тяжелее, а выигрыш по качеству/функциям не компенсирует.

### 7.1 KoboldCpp — доказательство встраиваемости sd.cpp рядом с llama.cpp

**Это прямой ответ на вопрос «насколько тяжело встроить sd.cpp вторым C++ движком рядом с llama.cpp».** KoboldCpp — single-file PyInstaller-обёртка над C++, и его `CMakeLists.txt` **собирает llama.cpp + stable-diffusion.cpp + TTS.cpp в одном дереве**. То есть прецедент существует и работает в продакшене. ([KoboldCpp](https://github.com/LostRuins/koboldcpp))
- HTTP API: A1111-совместимый `POST /sdapi/v1/{txt2img,img2img,interrogate,upscale}` + `GET /sdapi/v1/{sd-models,options,samplers}`; Ollama API (с v1.117.1); OpenAI `/v1` (текст).
- **GGUF нативно:** официальная image-модель — `picx_real_q5_1.gguf`; флаг `--sdquant`; int8 convrot с v1.118.1.
- Runtime-переключение модели/конфига есть (`swapReqType`, `autoswapmode`, `--autoswapthreshold` с v1.122.1), но **явный per-request swap SD-checkpoint не задокументирован** → считать уровнем «рестарт».
- Очередь: `--multiuser [limit]`, `/api/extra/perf`. Генерация изображений занимает тот же слот, что и текст.
- Лицензия: **AGPL** (в отличие от MIT у sd.cpp) — важно, если планируется проприетарная модификация.
- ⚠️ Официальный starter-pack указывает требование **8 GB+**; бюджет sd ~9 GB XL / ~4.25 GB обычный, снижается при квантизации.

### 7.2 mold — лучшая эргономика load/unload, но Rust

- `POST /api/models/load` (загрузка/свап) и **`DELETE /api/models/unload`** — самый чистый контракт из всех рассмотренных; плюс `POST /api/generate` (+`/stream` SSE), `/api/generation-batches`, `GET /api/models`.
- GGUF-тиры: `flux2-klein:q4` **~5 GB**, `:q8` ~6 GB, `sd15:fp16` ~6 GB, `sdxl-turbo` ~8 GB, `z-image-turbo:q8` ~9 GB.
- Последовательный режим по умолчанию (−30…50 % пика); **`--offload` даёт пик 2–4 GB, но в 3–5× медленнее**.
- **Минус:** Rust/candle, **не встраивается** в C++/CMake дерево. ([mold](https://github.com/utensils/mold))

### 7.3 Важное предупреждение: GGUF в torch-UI — ловушка для low-VRAM

Собственная документация SD.Next: *«all popular T2I inference UIs (SD.Next, Forge, ComfyUI, InvokeAI etc.) are using GGUF as **storage-only** and as such usage of GGUF is **not recommended**!»*; GGUF *«not compatible with model offloading»*, и GGUF-квантизуем **только UNET/Transformer** (единого all-in-one файла нет). ([SD.Next Quantization](https://vladmandic.github.io/sdnext-docs/Quantization/))

**Практический смысл:** в torch-мире GGUF экономит диск/RAM, но **блокирует именно тот offload, который и делает возможной работу на 4–8 GB**. Реальные рычаги квантования там — SDNQ / bitsandbytes-nf4 (SD.Next) или FP8 (InvokeAI). **Только sd.cpp / KoboldCpp / LocalAI / mold превращают GGUF в реальную экономию VRAM.** Это, пожалуй, самый сильный аргумент в пользу sd.cpp для задачи «слабые карты + GGUF».

### 7.4 Прочие — кратко

| Проект | Ключевое |
|---|---|
| **Forge** | «GGUF Q8_0/Q5_0/Q5_1/Q4_0/Q4_1 natively supported» (Flux/NF4 тоже); хот-свап **был сломан и теперь починен** — `modules/sysinfo.py set_config()` вызывает `main_entry.checkpoint_change()` ([issue #1421](https://github.com/lllyasviel/stable-diffusion-webui-forge/issues/1421)) |
| **A1111** | `POST /sdapi/v1/options {"sd_model_checkpoint":…}` работает + `/unload-checkpoint`, `/reload-checkpoint`; **GGUF нативно НЕТ**; один `queue_lock` на все эндпоинты |
| **SD.Next** | API включён по умолчанию, Swagger `/docs`; дополнительные `/sdapi/v1/refresh-loras`, `/checkpoint`, `/lock-checkpoint`, `/lora`, `/loaded-loras`, `/gpu`, `/history`, `/shutdown`. BnB NF4 **1.48 it/s против fp16 0.68** на FLUX. ⚠️ `override_settings_restore_afterwards` по умолчанию true ⇒ **2 загрузки модели на запрос** |
| **InvokeAI** | **4 GB SD1.5 / 8 GB SDXL / 8 GB Z-Image Q4_K**; FLUX.2-Klein-4B 12 GB (FP8 8 GB+); частичная загрузка слоёв **включена по умолчанию** с резервом 3 GB; GGUF-лоадеры есть, но **GGUF-модели нельзя ре-энкодить**; `/api/v2/models/*` включая **`/empty_model_cache`**, генерация через `POST /api/v1/queue/default/enqueue_batch` |
| **Fooocus** | **Своего generation API НЕТ** — только Gradio. `--share` = просто ссылка gradio.live. Заявлено 4 GB NVIDIA + 8 GB RAM с виртуальным свопом. Нужна обёртка |
| **Fooocus-API** (mrhan1993) | `POST /v1/generation/text-to-image`, `/v2/generation/text-to-image-with-ip`, `POST /v1/generation/stop`, `/query-job`, `/job-history`; реальная очередь `--queue-size` 100 + webhook. BaiMoHan-версия — legacy. Очередь живёт **в обёртке**, а не в Fooocus |
| **SwarmUI** | `POST /API/GenerateText2Image` (+WS), `/API/SelectModel`, `/API/ListLoadedModels`, **`/API/FreeBackendMemory`**, `/API/RestartBackends`; per-request параметр `model`. Сверху рулит ComfyUI → наследует его вес |
| **ComfyUI** (операционные ловушки) | Нет слоя совместимости — трансляцию API-формата пишете сами; `/prompt` падает с 400 + `node_errors` при любом несовпадении custom-нод; несовпадение `Host`/`Origin` → **403** без `--enable-cors-header`; **один `prompt_worker`, один промпт за раз** (подтверждено в коде); `POST /free {"unload_models":true}` **асинхронный** и оставляет **1–2 GB** резидентно ([issue #5536](https://github.com/Comfy-Org/ComfyUI/issues/5536)) ⇒ процессы надо пересоздавать |
| **diffusers-FastAPI** (HF `examples/server`) | Только `POST /v1/images/generations`, возвращает **URL, а не b64**; **«Loading GGUF checkpoints via Pipelines is currently not supported»** (GGUF только через `GGUFQuantizationConfig` + `from_single_file`); один `shared_pipeline`, созданный на старте, **нет load/unload** |
| **sd.cpp-webui** | Своего API нет (Gradio); в CLI-режиме порождает **новый `sd-cli` на каждую генерацию** (перезагрузка модели каждый раз) |
| **stability-matrix** | ❌ как сервер **не существует** (404). Одноимённая вещь — **Stability Matrix**, лаунчер/пакетный менеджер для других UI |

**Отдельно про ComfyUI — он сильнее, чем кажется, и это главный конкурент sd.cpp.** Из официального README: *«Can run even the biggest open source models on as low as **4GB vram + 8GB ram** relatively quickly (saturating your GPU compute) using our state of the art **asynchronous weight streaming** technology»*. Также заявлены «asynchronous queueing, partial graph re-execution, smart VRAM and RAM management, model offloading, and support for quantized models» и «a local API for integrating workflows into applications». Практически важно:
- **Официальные portable-сборки под Windows**: NVIDIA (`ComfyUI_windows_portable_nvidia.7z`, 20-серия+), **NVIDIA cu126 для 10-серии и старше** (это ровно GTX 1060), **AMD**, **Intel**. Плюс desktop-приложение.
- В комплекте — `taesd_decoder.pth`, `taesdxl_decoder.pth`, `taesd3_decoder.pth`, `taef1_decoder.pth` для `--preview-method taesd` (тот же TAESD, что и в sd.cpp).
- **Чего у ComfyUI нет:** нативного GGUF для диффузии (нужен сторонний ComfyUI-GGUF), и он **не встраивается как C++ движок**. Зато он покрывает модели, которых в sd.cpp нет (Hunyuan3D, SUPIR, SAM 3, Depth Anything 3, RT-DETRv4 и др.).
- **Итого:** если критерий — «минимум веса и встраиваемость в C++», выигрывает sd.cpp. Если критерий — «широта моделей и максимально агрессивная работа с 4 GB», ComfyUI объективно сильнее, ценой Python+torch. ([README](https://raw.githubusercontent.com/comfyanonymous/ComfyUI/master/README.md))

---

## 8. Что это означает для интеграции в OllamaLegion (практический вывод)

1. **Тип бэкенда.** Ввести отдельный `type: "stable_diffusion_cpp"` (по аналогии с `llama_cpp`), с отдельным портом (напр. 18093, рядом с cppworker 18091/18092) и своим `apiStyle`. Изоляция типов уже реализована в `internal/balancer` — расширять её, а не смешивать с `llama_cpp`.
2. **Процесс-модель = как у llama.cpp.** `sd-server` спавнится как дочерний процесс с флагами модели; `/api/models/load` = spawn, `/api/models/unload` = kill, `/api/models/reload` = kill+spawn. Никакого in-process линкования.
3. **Контракт для воркера: `/sdcpp/v1/*`.** `POST /sdcpp/v1/img_gen` (202) → poll `GET /sdcpp/v1/jobs/{id}` → `result.images[].b64_json`. `GET /sdcpp/v1/capabilities` при старте — для валидации лимитов и `model.stem`.
4. **Своя очередь.** Не полагаться на 64-слотовую очередь sd-server; гейтить на стороне воркера. `queue_position` использовать только для диагностики.
5. **Отмена.** Честно сообщать `cancel_generating: false`; либо cancel-by-restart (перезапуск процесса), либо не поддерживать отмену в полёте на первом этапе.
6. **Прогресс.** Либо парсить `stderr` под `-v` (хрупко, требует smoke-теста против зафиксированной сборки), либо не показывать. Upstream-патч, пробрасывающий `sd_progress_cb_t` в job-статус, — самый чистый путь.
7. **Пиновать версию.** Релизы `master-NNN-<sha>` выходят по нескольку раз в день; API/флаги меняются. Зафиксировать конкретный тег (напр. `master-929-3f8527a`) и прогонять контрактный smoke-тест против него.
8. **Дефолтный набор флагов для слабых карт:** `--diffusion-fa --offload-to-cpu --max-vram -1 --vae-conv-direct --backend te=cpu` + `--vae-tiling --vae-tile-size 512` для разрешений ≥1024.
9. **Vulkan-сборка как дефолт для не-NVIDIA.** 30–37 MB, покрывает AMD/Intel. Для NVIDIA — CUDA-сборка (337 MB + рантайм).
10. **Осторожно с `--vae-tile-size`:** семантика сменилась с латентов на пиксели. Старые конфиги молча интерпретируются иначе.
11. **НЕ рассчитывать на Ollama для картинок.** Фича была в v0.14.3 и **удалена в v0.32.6**; `/api/tags` при этом всё ещё репортит `capabilities:["image"]`, а `/api/generate` отдаёт 400. Но **схему hot-swap у Ollama скопировать стоит** (`keep_alive`, `/api/ps`, `size_vram`).
12. **Готовый образец логики вытеснения — LocalAI.** `--max-active-backends=1` (или `--single-active-backend`, deprecated) = ровно режим «одна модель за раз», который нужен на 4–6 GB: при загрузке новой модели текущая выгружается автоматически. Плюс `--enable-watchdog-idle --watchdog-idle-timeout=10m` (выгрузка простаивающей модели), `--enable-watchdog-busy --watchdog-busy-timeout=5m` (убить зависший бэкенд), `--vram-budget=80%` (потолок аллокаций), `--lru-eviction-max-retries` / `--lru-eviction-retry-interval` (не вытеснять модель с активным запросом). Эту семантику стоит скопировать в враппер, даже если LocalAI не используется. ([localai.io/docs/advanced/vram-management](https://localai.io/docs/advanced/vram-management/))
13. **Всегда передавать явный положительный seed.** Известен integer overflow при `seed: -1` → `generate_image returned no results` и зависание. Генерировать seed на стороне воркера.
14. **Проверять загрузку GGUF до продакшена.** `city96`-GGUF для FLUX падают в sd.cpp с `new_sd_ctx_t failed`. Использовать `leejet` для FLUX/Z-Image/klein.
15. **Проверять имена флагов у своего билда.** В сборках до `master-600` было `--host`/`--port`, потом `--listen-ip`/`--listen-port`. `sd-server` падает на неизвестном аргументе. Либо пиновать версию, либо один раз снять `sd-server --help` и валидировать набор флагов на старте.
16. **`--vae-tiling` ставить заранее, а не ждать OOM.** Автоматический retry decode в sd.cpp существует, но на Vulkan/RX 580 без явного тайлинга decode **роняет сервер**. На GTX 1650 4GB авто-retry спас ситуацию, но ценой ~38 с на 4 шага.
17. **Не использовать `--vae-on-cpu` без нужды** — измеренный штраф **5×** по времени. Сначала `--vae-tiling`.

---

## 9. Открытые вопросы / противоречия

| Вопрос | Статус |
|---|---|
| Реальные с/картинку для **SD1.5 Q8 512², SDXL-Turbo Q4/Q8, FLUX-schnell Q4** на GTX 1060 / 1650 / 2060 / RX 580 | **Частично закрыто** (см. §6.1.1), но **именно этих комбинаций нет**. Есть: SD1.5 на RX 580 (71 с / 50 шагов) и GTX 1070 (164.7 с / 20 шагов, 2023); FLUX-schnell Q4_K на RX 580 (52 с @512², 14 мин @1024²) и RTX 3060 (9.6 с @512²). **SDXL-Turbo в sd.cpp не замерен вообще ни на одной карте.** |
| SD-Turbo, Chroma | **Ни одного замера wall-clock** нигде не найдено |
| Точная экономия VRAM/RAM от TAESD в sd.cpp | **Не документирована в sd.cpp.** Есть абсолютные размеры (§6.5), замер TAEHV на GH200 (пик 6–9 GB → <0.5 GB) и один end-to-end кейс (Steam Deck SDXL 1024²: 360 → 120 с) |
| **Совместимость GGUF-форматов** | ⚠️ **city96-GGUF падают в sd.cpp** (`new_sd_ctx_t failed`) — подтверждено практическим отчётом. Docs sd.cpp для FLUX указывают на `leejet`. **Проверять загрузку каждого файла до продакшена** |
| Память FLUX Q4 | ⚠️ **Разброс 6394 / 7534 / ~9000 MB** между `docs/flux.md`, Local-Diffusion и сторонними таблицами (15–40 %). У `flux.md` не указаны разрешение и точность |
| Chroma «4 GB without offload» | ⚠️ **Противоречит** минимальному файлу 4.99 GB. Заявление из `docs/chroma.md` не воспроизводится из размеров |
| `--offload-to-cpu` «without reducing generation speed» | ⚠️ **Противоречит** замеру RX 580 FLUX q4_k: **838 с на 4 шага** @1024² (~209 с/шаг) |
| RX 580 s/it на одном и том же воркфлоу | ⚠️ **9.5 против 22–23** — зависит от коммита (регрессия Vulkan 2.3×, [#1647](https://github.com/leejet/stable-diffusion.cpp/issues/1647)). **Пинать коммит обязательно** |
| `ggerganov/*` как источник GGUF для диффузии | ❌ **Не существует.** Только whisper.cpp/ggml |
| `city96/*SDXL*`, `city96/sd-turbo-gguf` | ❌ **Репозиториев не существует** (HF API отдаёт 401/gated, что маскирует отсутствие) |
| Чёрные картинки на ROCm с `unsloth/Z-Image-GGUF` Q4_K_M; Qwen-Image-2512 Q4_K_M/Q5_K_M на GTX 1060 | ⚠️ **Известные баги бэкендов**, workaround — Vulkan / другой квант |
| Прогресс/отмена в сервере | **Не реализованы**, хотя примитивы в C-API есть. Upstream-PR возможен |
| Vulkan на Windows vs CUDA на слабых NVIDIA | **Не измерено.** Vulkan-бинарь есть (30.1 MB) |
| Сторонние VRAM-таблицы (willitrunai, bestgpuforai) | ⚠️ **Низкое доверие.** Они противоречат собственным замерам sd.cpp (напр. «SD1.5 fp16 нужно 4–5 GB» против измеренных 2.3 GB; «GGUF для SDXL/SD1.5 не существует» — ложь) и описывают ComfyUI/A1111, а не sd.cpp |

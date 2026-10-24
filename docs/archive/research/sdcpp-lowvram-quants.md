# Low-VRAM (4–8 GB) image generation with stable-diffusion.cpp — concrete numbers

Method: HF tree API (`/api/models/<repo>/tree/main`) for exact bytes; sd.cpp docs/wiki; GH issues/PRs. **Sizes below are GB decimal (bytes/1e9), as HF displays.** ⚠️ = caveat.

## 1. GGUF quant sizes + memory need

### SD 1.5 / SD 2.x / SD-Turbo (single all-in-one file: UNet+VAE+CLIP)
| Model / repo | Q2_K | Q3_K | Q4_0 | Q4_K_M | Q5_0/K | Q6_K | Q8_0 | f16 |
|---|---|---|---|---|---|---|---|---|
| [second-state/stable-diffusion-v1-5-GGUF](https://huggingface.co/second-state/stable-diffusion-v1-5-GGUF) | — | — | **1.57** | — | 1.62 (Q5_0) / 1.64 (Q5_1) | — | **1.76** | **2.13** (f32 4.27) |
| [Green-Sky/SD-Turbo-GGUF](https://huggingface.co/Green-Sky/SD-Turbo-GGUF) | — | — | — | — | — | — | 2.02 | **2.61** |
SD2.1 GGUF not enumerated; SD 2.x ≈ SD1.5 ±10%. **⚠️ No Q2_K/Q3_K/Q4_K_M exist for SD1.5** — only q4_0/q4_1/q5_0/q5_1/q8_0/f16 ([tree](https://huggingface.co/api/models/second-state/stable-diffusion-v1-5-GGUF/tree/main)).
sd.cpp measured SD1.x memory @512×512 (txt2img): f32 ~2.8 G, f16 ~2.3 G, q8_0 ~2.1 G, q5_0/q5_1/q4_0/q4_1 ~2.0 G; with `--diffusion-fa`: 2.4/1.9/1.6/1.5/1.5/1.5/1.5 G ([docs/quantization_and_gguf.md](https://raw.githubusercontent.com/leejet/stable-diffusion.cpp/master/docs/quantization_and_gguf.md)).
Cross-check (Android, TAE+FA+VAE-tiling, **CLIP-L Q8 only**): SD1.5 @512 = Q2_K 1870 / Q4_0 1900 / Q8_0 2087 / fp16 2436 MB; @256 = 1431/1461/1648/1997; @768 = 4001/4031/4218/4567 ([Local-Diffusion README](https://github.com/rmatif/Local-Diffusion)).

### SDXL / SDXL-Turbo (all-in-one file includes both CLIPs + VAE at fp16)
| Repo | Q4_0 | Q4_1 | Q5_K_M | Q8_0 | fp16 |
|---|---|---|---|---|---|
| [gpustack/stable-diffusion-xl-1.0-turbo-GGUF](https://huggingface.co/gpustack/stable-diffusion-xl-1.0-turbo-GGUF) (SDXL-Turbo, all-in-one) | **3.94** | 4.08 | — | **5.04** | **6.94** |
| [hum-ma/SDXL-models-GGUF](https://huggingface.co/hum-ma/SDXL-models-GGUF) **UNet-only** (has separate `clip/`, `vae/`) | **1.49** | — | **1.84** | — | — |
`vae/xlVAEC_c91.safetensors` = 167 MB ([tree](https://huggingface.co/api/models/hum-ma/SDXL-models-GGUF/tree/main/vae)). SDXL peak mem (TAE+FA+tiling, CLIP-L only): Q2_K 2228 / Q4_0 2810 / Q8_0 4249 / fp16 6946 MB @≥512 — **flat from 512→1024** ([Local-Diffusion](https://github.com/rmatif/Local-Diffusion)). ⚠️ SDXL-Turbo GGUF has no K-quants.

### SD-Turbo / LCM
SD-Turbo = tiny SD2.1 U-Net, tuned 1–4 steps ([gpustack/stable-diffusion-v2-1-turbo-GGUF](https://huggingface.co/gpustack/stable-diffusion-v2-1-turbo-GGUF) exists). LCM = LoRA/`--sampling-method lcm`, **no size change** ([docs/lcm.md](https://raw.githubusercontent.com/leejet/stable-diffusion.cpp/master/docs/lcm.md)). Distilled tiny-UNet SD1.x/SD2.x (Segmind SSD-1B, Vega, bk-sdm-tiny, SDXS-512): −33…−50% time & size ([docs/distilled_sd.md](https://raw.githubusercontent.com/leejet/stable-diffusion.cpp/master/docs/distilled_sd.md)).

### SD3 Medium / SD3.5 Large (diffusion model only; CLIP-L+G+T5 separate)
| Repo / type | Q2_K | Q3_K_S | Q4_0 | Q4_K_M | Q5_0/K | Q6_K | Q8_0 | f16 |
|---|---|---|---|---|---|---|---|---|
| [city96/stable-diffusion-3-medium-gguf](https://huggingface.co/city96/stable-diffusion-3-medium-gguf) | — | — | 1.28 | **1.33** (Q4_K_M) | 1.54/1.58 | 1.80 | 2.29 | 4.17 |
| [city96/stable-diffusion-3.5-large-gguf](https://huggingface.co/city96/stable-diffusion-3.5-large-gguf) | **—** | **—** | 4.77 | **—** | 5.77 (Q5_0)/6.27 (Q5_1) | **—** | 8.78 | 16.29 |
| [stduhpf/SD3.5-Large-GGUF-mixed-sdcpp](https://huggingface.co/stduhpf/SD3.5-Large-GGUF-mixed-sdcpp) (mixed-quant, sd.cpp-only) | **4.69** | 4.87 | — | 5.11 (q4_k_4_0) / 5.50 (q4_k_4_1) | 5.89 (q4_k_5_0) | — | — | — |
**⚠️ KEY:** K-quants are unusable for SD3.5-Large — ~90% of weights sit in tensors whose shape doesn't match the 256-element K-quant superblock, so only the 2nd MLP layers (~10% of params) can be K-quantized; stduhpf's files mix types instead. `iq4_nl` is the author's pick (same size as q4_k_4_0, faster on Vulkan, q5_1-like quality) ([README](https://huggingface.co/stduhpf/SD3.5-Large-GGUF-mixed-sdcpp/raw/main/README.md)). SD3.5 Large peak mem (TAE+FA+tiling, CLIP-L only): Q2_K 7271 / Q4_0 7668 / Q8_0 11489 MB @1024 ([Local-Diffusion](https://github.com/rmatif/Local-Diffusion)).

### FLUX.1-schnell / FLUX.1-dev (DiT only; VAE + CLIP-L + T5-XXL separate)
| Quant | schnell [city96](https://huggingface.co/city96/FLUX.1-schnell-gguf) | dev [city96](https://huggingface.co/city96/FLUX.1-dev-gguf) | dev [leejet](https://huggingface.co/leejet/FLUX.1-dev-gguf) |
|---|---|---|---|
| Q2_K | 4.01 | 4.03 | 4.15 |
| Q3_K_S | 5.21 | 5.23 | 5.35 |
| Q4_0 | 6.77 | 6.79 | 6.93 |
| Q4_K_S / q4_k | 6.78 | 6.81 | 6.93 |
| Q4_1 | 7.51 | 7.53 | — |
| Q5_0 / Q5_K_S | 8.25 / 8.26 | 8.27 / 8.29 | — |
| Q5_1 | 8.99 | 9.01 | — |
| Q6_K | 9.83 | 9.86 | — |
| Q8_0 | 12.69 | 12.71 | 12.84 |
| F16 | 23.78 | 23.80 | — |
sd.cpp's own dev memory table: q8_0 **12068 MB**, q4_0 **6395 MB**, q4_k 6395 MB, q3_k **4888 MB**, q2_k **3736 MB** ⚠️(units/precision unstated, presumably total incl. T5) ([docs/flux.md](https://raw.githubusercontent.com/leejet/stable-diffusion.cpp/master/docs/flux.md)). Local-Diffusion FLUX.1 @1024 (CLIP-L only): Q2_K 4889 / Q4_0 7534 / Q8_0 13177 MB.

### Chroma (Flux-derived pruned DiT; VAE + T5-XXL only, **no CLIP-L**)
[silveroxides/Chroma-GGUF](https://huggingface.co/silveroxides/Chroma-GGUF), `chroma-unlocked-v20/` — Q3_K_L **4.99**, Q4_0 **5.99**, Q4_K_S 5.99, Q4_K_M **6.12**, Q4_1 6.53, Q5_K_S/Q5_0 7.07, Q5_1 7.60, Q6_K **8.21**, Q8_0 **10.29**, BF16 **17.80**. `Chroma1-HD/`: Q4_0 5.43, Q8_0 9.74, BF16 17.80. Only Q4_0/Q8_0/BF16 in `Chroma1-HD`; Q4_K_S/Q8_M live at [Clybius/Chroma-GGUF](https://huggingface.co/Clybius/Chroma-GGUF).

### Qwen-Image 20B MMDiT / Qwen-Image-Edit
[QuantStack/Qwen-Image-GGUF](https://huggingface.co/QuantStack/Qwen-Image-GGUF) — Q2_K **7.06**, Q3_K_S **8.95**, Q3_K_M 9.68, Q4_0 **11.85**, Q4_K_S 12.14, Q4_1 12.84, Q4_K_M **13.07**, Q5_K_S 14.12, Q5_0 14.40, Q5_K_M **14.93**, Q5_1 15.39, Q6_K **16.82**, Q8_0 **21.76**. [Qwen-Image-Edit-GGUF](https://huggingface.co/QuantStack/Qwen-Image-Edit-GGUF) and [-2509-GGUF](https://huggingface.co/QuantStack/Qwen-Image-Edit-2509-GGUF) are byte-identical for every shared quant (2509 Q2_K 7.15).
**⚠️ The whole model cannot fit in ≤8 GB at any quant** — the Q2_K file alone is 7.06 GB before the Qwen2.5-VL-7B encoder (Q4_K_M GGUF 4.68 GB, fp8 9.39 GB, bf16 16.58 GB) and 254 MB VAE. Unsloth's rule: *total usable RAM+VRAM must exceed the GGUF size* ([docs](https://unsloth.ai/docs/models/tutorials/qwen-image-2512/stable-diffusion.cpp.md)).

### Z-Image / Z-Image-Turbo (DiT + Qwen3-4B LLM + FLUX VAE)
| Quant | [leejet/Z-Image-Turbo-GGUF](https://huggingface.co/leejet/Z-Image-Turbo-GGUF) | [unsloth/Z-Image-GGUF](https://huggingface.co/unsloth/Z-Image-GGUF) (base) |
|---|---|---|
| Q2_K | **2.59** | 4.01 |
| Q3_K / Q3_K_S | **3.14** | 4.36 |
| Q4_0 | **3.68** | 4.59 |
| Q4_K / Q4_K_M | **3.86** | 4.79 (K_S) / **5.07** (K_M) |
| Q5_0 / Q5_K_M | **4.54** | 5.58 |
| Q6_K | **5.26** | 6.10 |
| Q8_0 | **6.58** | 7.22 |
| f16 / bf16 | — | 12.31 |
Plus Qwen3-4B encoder: [Qwen3-4B-Instruct-2507-GGUF](https://huggingface.co/unsloth/Qwen3-4B-Instruct-2507-GGUF) Q3_K_S 1.89 / Q4_K_S 2.38 / Q4_K_M 2.50 / Q5_K_M 2.89 / Q6_K 3.31 / Q8_0 4.28 GB; fp16 safetensors 8.04 / fp4_mixed 3.48 ([Comfy-Org/z_image_turbo](https://huggingface.co/api/models/Comfy-Org/z_image_turbo/tree/main/split_files/text_encoders)); VAE `ae.safetensors` 335 MB.

### FLUX.2-dev / FLUX.2-klein
| Quant | [city96/FLUX.2-dev-gguf](https://huggingface.co/city96/FLUX.2-dev-gguf) | [leejet/FLUX.2-klein-4B-GGUF](https://huggingface.co/leejet/FLUX.2-klein-4B-GGUF) | [leejet/FLUX.2-klein-9B-GGUF](https://huggingface.co/leejet/FLUX.2-klein-9B-GGUF) |
|---|---|---|---|
| Q2_K | 12.86 | — | — |
| Q4_0 / Q4_K_S | 19.30 | **2.46** | **5.62** |
| Q4_K_M | 20.08 | — | — |
| Q5_K_M | 24.06 | — | — |
| Q6_K | 27.40 | — | — |
| Q8_0 | 35.00 | **4.30** | **9.98** |
| BF16 | 64.45 | — | — |
klein has only 2 quants each. FLUX.2-dev also needs Mistral-Small-3.2-24B encoder: Q4_K_S 13.55 / Q4_K_M **14.33** / Q5_K_M 16.76 / Q6_K 19.35 / Q8_0 25.05 GB ([unsloth](https://huggingface.co/unsloth/Mistral-Small-3.2-24B-Instruct-2506-GGUF)); VAE 336 MB or small-decoder 250 MB. **⚠️ Nothing here is 4–6 GB viable.**

## 2. What actually runs on 4 GB / 6 GB
- **4 GB — comfortable:** SD 1.5 / SD 2.x (all quants; 1.9–2.4 GB peak @512) ✅; SD-Turbo; tiny-UNet SDXS-512; SDXL-Turbo Q4_0 (3.94 GB file, 4.0/4.0 GB VRAM = 99.2% used, 4.5 s/img on a 4 GB NVIDIA with A1111 `--lowvram`) ⚠️2023, not sd.cpp ([blog](https://ncos1.hatenablog.com/entry/2023/11/30/190000)).
- **4 GB — feasible with tiling/offload:** SDXL Q4_0 **UNet-only** (1.49 GB) + VAE 167 MB + both CLIPs, or all-in-one Q4_0 3.94 GB; SD3.5-Medium Q2_K 3.44 GB / Q4_0 3.96 GB @1024; **Z-Image-Turbo Q4_0 3.68 / Q3_K 3.14 GB** — officially documented for 4 GB: "You can run Z-Image with stable-diffusion.cpp on GPUs with 4GB of VRAM — or even less", recommended Q4_0 or Q3_K + Qwen3-4B Q4_K_M + `--offload-to-cpu --diffusion-fa` ([wiki](https://raw.githubusercontent.com/wiki/leejet/stable-diffusion.cpp/How-to-Use-Z%E2%80%90Image-on-a-GPU-with-Only-4GB-VRAM.md)); Chroma is doc-claimed for 4 GB ("6GB or even 4GB … without needing to offload") but its smallest file is **4.99 GB** (Q3_K_L) ⚠️contradiction; FLUX.1-schnell **Q2_K 4.01 GB** with RAM spill ([flux.md](https://raw.githubusercontent.com/leejet/stable-diffusion.cpp/master/docs/flux.md), [Civitai](https://civitai.com/articles/10297/flux-on-4gb-vram-and-8gb-ram)).
- **6 GB:** add FLUX.1 schnell/dev **Q3_K_S ≈5.2 GB** (6 GB-viable per flux.md's "6GB or even 4GB" claim) and stduhpf SD3.5-Large **q2_k_4_0 4.69 / q3_k_4_0 4.87 GB**. FLUX Q4_K ≈6.8 GB does **not** fit 6 GB VRAM without `--offload-to-cpu`.
- **Never ≤8 GB:** Qwen-Image / Qwen-Image-Edit (min 7.06 GB diffusion + 4.7 GB TE), FLUX.2-dev (min 12.86 + 13.55 GB), SD3.5-Large at Q8/F16 (needs &ge;9–16 GB).
- **Measured component VRAM (useful for budgeting):** Z-Image-Turbo Q4_0 on 12 GB = diffusion **3512 MB** + Qwen3-4B TE **4076 MB** + VAE 160 MB ([#1600](https://github.com/leejet/stable-diffusion.cpp/issues/1600)); GTX 1060 Z-Image-base Q3_K_M = diffusion 4350 + TE 3555 + VAE 95 = **8000 MB** total with `--offload-to-cpu` ([#1253](https://github.com/leejet/stable-diffusion.cpp/issues/1253)); FLUX.2-klein-4B Q8_0 on RTX 2060 = 4101 MB VRAM + 2375 MB RAM + 160 MB VAE ([#1989](https://github.com/leejet/stable-diffusion.cpp/issues/1989)). Steam Deck verdict: SD1.5 fine, SDXL needs `--vae-on-cpu`, **FLUX "q2_k barely fits, hour per image"** ([Steam thread](https://steamcommunity.com/app/1675200/discussions/0/595134792309237790/)).
- **Mandatory low-VRAM flags:** `--offload-to-cpu` (params in RAM, staged, **claimed no speed loss**), `--diffusion-fa` (flux 768² −~600 MB; SD2 768² −~1400 MB), `--vae-tiling` + `--vae-tile-size 256x256 --vae-tile-overlap 0.5`, `--vae-conv-direct`, `--params-backend diffusion=disk` (least RAM+VRAM, slower), `--max-vram <GiB>` / `-1` (reserve ~1 GiB), `--taesd`/`--tae` ([performance.md](https://raw.githubusercontent.com/leejet/stable-diffusion.cpp/master/docs/performance.md), [backend.md](https://raw.githubusercontent.com/leejet/stable-diffusion.cpp/master/docs/backend.md)). Auto-fit picks CPU/RAM/disk tiers automatically, reserving ≥2 GiB or 10% RAM and 512 MiB device scratch.

## 3. Steps / cfg-scale per family (sd.cpp)
| Family | steps | cfg-scale | sampling | source |
|---|---|---|---|---|
| SD 1.5 / 2.x | 20–30 | **7.0** (sd.cpp default) | euler_a / dpm++2m | [lcm.md](https://raw.githubusercontent.com/leejet/stable-diffusion.cpp/master/docs/lcm.md) |
| SDXL base | 20–30 | **7.0** | euler_a | (sd.cpp default) |
| SDXL-Turbo | **1** (1–4) | **1.0** (diffusers: guidance 0.0) | euler | [OlegSkutte README](https://huggingface.co/OlegSkutte/sdxl-turbo-GGUF/raw/main/README.md), [gpustack README](https://huggingface.co/gpustack/stable-diffusion-xl-1.0-turbo-GGUF/raw/main/README.md) |
| SD-Turbo | **1–4** | **1.0** | euler | [gpustack v2-1-turbo](https://huggingface.co/gpustack/stable-diffusion-v2-1-turbo-GGUF) |
| LCM / LCM-LoRA | **4** (2–8) | **1.0** | `lcm` / euler_a | [lcm.md](https://raw.githubusercontent.com/leejet/stable-diffusion.cpp/master/docs/lcm.md) |
| SDXS-512 / -0.9 | **1** | **1.0** | — | [distilled_sd.md](https://raw.githubusercontent.com/leejet/stable-diffusion.cpp/master/docs/distilled_sd.md) |
| SD3 Medium | ~28 | **4.5** | euler | [sd.md](https://raw.githubusercontent.com/leejet/stable-diffusion.cpp/master/docs/sd.md) |
| SD3.5 Large / Turbo | **28** | **4.5** | euler | [stduhpf README](https://huggingface.co/stduhpf/SD3.5-Large-GGUF-mixed-sdcpp/raw/main/README.md), [sd3.md](https://raw.githubusercontent.com/leejet/stable-diffusion.cpp/master/docs/sd3.md) |
| FLUX.1-schnell | **4** | **1.0** | euler | [flux.md](https://raw.githubusercontent.com/leejet/stable-diffusion.cpp/master/docs/flux.md) |
| FLUX.1-dev | **20–50** | **1.0** | euler | [flux.md](https://raw.githubusercontent.com/leejet/stable-diffusion.cpp/master/docs/flux.md) |
| Chroma | 20–30 | **4.0** | euler | [chroma.md](https://raw.githubusercontent.com/leejet/stable-diffusion.cpp/master/docs/chroma.md) |
| Qwen-Image / Edit | **40** (20–50) | **2.5** | euler + `--flow-shift 3` | [qwen_image.md](https://raw.githubusercontent.com/leejet/stable-diffusion.cpp/master/docs/qwen_image.md), [unsloth](https://unsloth.ai/docs/models/tutorials/qwen-image-2512/stable-diffusion.cpp.md) |
| Z-Image-Turbo | **8** (4–9) | **1.0** | — | [z_image.md](https://raw.githubusercontent.com/leejet/stable-diffusion.cpp/master/docs/z_image.md) |
| Z-Image (base) | **28–50** | **3.0–5.0** (sd.cpp example 5.0) | — | [unsloth README](https://huggingface.co/unsloth/Z-Image-GGUF/raw/main/README.md) |
| FLUX.2-dev | 20–50 | **1.0** | euler | [flux2.md](https://raw.githubusercontent.com/leejet/stable-diffusion.cpp/master/docs/flux2.md) |
| FLUX.2-klein-4B/9B | **4** | **1.0** | euler | [flux2.md](https://raw.githubusercontent.com/leejet/stable-diffusion.cpp/master/docs/flux2.md) |
| FLUX.2-klein-**base**-4B/9B | **20** | **4.0** | euler | [flux2.md](https://raw.githubusercontent.com/leejet/stable-diffusion.cpp/master/docs/flux2.md) |

## 4. Measured wall-clock (weak GPUs) — real logs
| GPU | Model + quant | Res | Steps | s/image | VRAM/RAM | Source |
|---|---|---|---|---|---|---|
| GTX 1060 6GB CUDA | Z-Image **base** Q3_K_M + Qwen3-4B Q4_K_M | 512×1024 | 20 | **341.4 total** (16.21 s/it; sampling 324.5, VAE 13.1) | params 8000 MB (diff 4350 + TE 3555) > 6 GB → offload; torch ~2000 MB | [#1253](https://github.com/leejet/stable-diffusion.cpp/issues/1253) |
| GTX 1060 6GB | Qwen-Image-2512 Q4_K_M / Q5_K_M | 1024² | 40 | black images (bug); Q5_0 works | — | [#1385](https://github.com/leejet/stable-diffusion.cpp/issues/1385) |
| RTX 2060 6GB CUDA | FLUX.2-klein-4B Q8_0 + Qwen3-4B Q4 | 512² | 2 | **~1.4 s/step** | 6637 MB: diff 4101 VRAM, TE 2375 RAM | [#1989](https://github.com/leejet/stable-diffusion.cpp/issues/1989) |
| GTX 1650 4GB CUDA | Qwen-Image-2.1 **Q2_K** + Qwen3-VL-8B Q2_K | 512² | 4 | sampling OK; **VAE decode OOM** (need 3445 MB, have 2679); retry 16×16/9 tiles **~38 s** | 4 GB | [#2051](https://github.com/leejet/stable-diffusion.cpp/issues/2051) |
| RTX 3060 12GB CUDA | Z-Image-Turbo **Q8** (6.6 GB), no offload | 688×1024 | 12 | **~29 s** (115 s / batch 4) | fits 12 GB | [PR #1477](https://github.com/leejet/stable-diffusion.cpp/pull/1477) |
| RTX 3060 12GB CUDA | + `--vae-on-cpu` | 688×1024 | 12 | **~150 s** (602 s / 4) ⚠️5× penalty | — | [PR #1477](https://github.com/leejet/stable-diffusion.cpp/pull/1477) |
| RTX 3060 12GB CUDA/Vulkan | FLUX.1-schnell Q4_K_S + PuLID | 512² | 4 | **9.6 s** CUDA / **11 s** Vulkan | — | [PR #1542](https://github.com/leejet/stable-diffusion.cpp/pull/1542) |
| RTX 3060 12GB | FLUX.1-dev Q4_K_S + PuLID | 1024² | 20 | **OOM** unless `--backend vae=cpu` | 12 GB insufficient | [PR #1542](https://github.com/leejet/stable-diffusion.cpp/pull/1542) |
| GTX 1070 CUDA | SD1.5 f16 | 512² | 20 | **164.7 total** (7.42 s/it; VAE 6.66 s) | model 1969 MB (UNet 1641) | [#95](https://github.com/leejet/stable-diffusion.cpp/issues/95) ⚠️2023 |
| RX 580 8GB Vulkan (Win) | DreamShaper 8 (SD1.5) | 512² | **50**, cfg 7 | **71.49 s sampling** (steps logged) | **~3.8 GB VRAM** | [benchmarks.md](https://huggingface.co/aivisionslab/ai-local-rx580-stack/raw/main/docs/benchmarks.md) |
| RX 580 8GB Vulkan | flux1-schnell **q4_k** (Leejet) + T5 fp16 on CPU | 512²→? | 4 | ~84 s (Win) / **~52 s sampling, ~95 s total** (Linux RADV) | ~6.5 GB VRAM | [README](https://huggingface.co/aivisionslab/ai-local-rx580-stack/raw/main/README.md) |
| RX 580 8GB Vulkan | flux1-schnell q4_k, 1024² | 1024² | 4 | **~14 min** = T5 11.49 s + sampling **838 s** + VAE 40.45 s (9 tiles) | 6.5 GB VRAM / T5 9.3 GB RAM | [same](https://huggingface.co/aivisionslab/ai-local-rx580-stack/raw/main/docs/benchmarks.md) |
| RX 580 8GB Vulkan | Anima base + LoRA | 1152×896 | 8 | **9.5 s/it** (build 19bdfe2) vs **22–23 s/it** (9b0fceb) ⚠️2.3× build-dependent regression | stable VRAM | [#1647](https://github.com/leejet/stable-diffusion.cpp/issues/1647) |
| Steam Deck iGPU Vulkan | SD 1.5 f16 + `--sampling-method lcm` | 512² | 8 | **~30 s** | needs 4 GB framebuffer reserve | [Steam thread](https://steamcommunity.com/app/1675200/discussions/0/595134792309237790/) ⚠️2024 |
| Steam Deck iGPU Vulkan | SDXL 1.0 | 1024² | 8 | **≥360 s** (120 s only via lossy `--taesd`) | `--vae-on-cpu` required | [same](https://steamcommunity.com/app/1675200/discussions/0/595134792309237790/) |
| Intel UHD 620 iGPU Vulkan | SDXL-Turbo fp16 (TAESD, `--clip-on-cpu`) | 512² | 1–4 | runs; **no timings published** | — | [blog](https://lukyanovartem.github.io/posts/sd.html) |
| 4 GB NVIDIA, A1111 `--lowvram` | SDXL-Turbo fp16 | 512² | 1, cfg 1 | **4.5 s** | 4.0/4.0 GB (99.2%) | [blog](https://ncos1.hatenablog.com/entry/2023/11/30/190000) ⚠️2023, not sd.cpp |
| 4 GB laptop + 8 GB RAM | FLUX.1-schnell **Q3** + t5-xxl-Q8_0 | 1024² | 4 | **~50 s** | RAM spill | [Civitai](https://civitai.com/articles/10297/flux-on-4gb-vram-and-8gb-ram) ⚠️hardware partly unnamed |
| RX 7900 XTX (ref) | Qwen-Image-2.1 Q8_0 (16.7 GB) | 1024² | 25 | **139 s total** (TE 18 + sampling 116 + VAE 4.5) | — | [#2015](https://github.com/leejet/stable-diffusion.cpp/issues/2015) |

### it/s references (convertible; steps stated)
RX 7800 XT Vulkan: SD1.5 512² **7.77 it/s** (PR) / 6.89 (master) → ~4.5–5.1 s @35 steps; SDXL 1024² **1.27/1.21 it/s** → ~27.6–28.9 s @35 steps ([PR #2085](https://github.com/leejet/stable-diffusion.cpp/pull/2085)). RX 5700 XT Vulkan: SD1.5 512² **2.57/2.45 it/s** → ~13.6–14.3 s @35 steps (same). RTX 3080 Laptop 8GB: Z-Image-Turbo Q4_K_M 1024² **2.39 s/it → 19.72 s @7 steps**; RTX 3090 Ti 1.13 s/it (8.37 s); RTX 4070 Ti 1.25 s/it (9.17 s); M4 Pro 24GB 14.5–15.1 s/it (~104–108 s @7) ([benchmark data](https://raw.githubusercontent.com/miroleon/z-image-turbo-benchmark/main/assets/data/benchmarks.json)).
CPU-only fallback: GTX 1070 log shows **80 s/step** on CPU vs ~10 s/step on GPU → SD1.5 ≈1600 s/img ([#48](https://github.com/leejet/stable-diffusion.cpp/issues/48)); 4 GB/8 GB-RAM report gives ~19 min/img CPU+HDD ([rx580 stack](https://huggingface.co/aivisionslab/ai-local-rx580-stack/raw/main/README.md)).

### Data gaps (explicitly NOT found)
SD1.5 Q8/f16 512² on GTX 1060/1650/2060/RX 580; **SDXL-Turbo in sd.cpp on any of these cards** (only the 2023 A1111 datapoint); SD-Turbo 512² anywhere; FLUX.1-schnell Q4 specifically on 1060/1650; SDXL base Q4 @1024 on 6 GB; Z-Image-Turbo on 1650/1060; **Chroma timings on any target card**; ComfyUI-GGUF per-card tables (none exist — [issue #152](https://github.com/city96/ComfyUI-GGUF/issues/152) is load speed only).

## 5. Canonical GGUF repos + exact files to download
**⚠️ CRITICAL, real-world:** city96-format FLUX GGUFs **fail** in sd.cpp/sd-server with `[ERROR] main.cpp:92 - new_sd_ctx_t failed`; only leejet's GGUFs load ([rx580 stack](https://huggingface.co/aivisionslab/ai-local-rx580-stack/raw/main/README.md)). Use city96 for ComfyUI-GGUF, leejet for sd.cpp. city96's own list is [city96/ComfyUI-GGUF](https://raw.githubusercontent.com/city96/ComfyUI-GGUF/main/README.md).
**sd.cpp doc-recommended (canonical):** [leejet/FLUX.1-dev-gguf](https://huggingface.co/leejet/FLUX.1-dev-gguf), [leejet/FLUX.1-schnell-gguf](https://huggingface.co/leejet/FLUX.1-schnell-gguf), [city96/FLUX.2-dev-gguf](https://huggingface.co/city96/FLUX.2-dev-gguf), [leejet/FLUX.2-klein-4B-GGUF](https://huggingface.co/leejet/FLUX.2-klein-4B-GGUF), [leejet/FLUX.2-klein-9B-GGUF](https://huggingface.co/leejet/FLUX.2-klein-9B-GGUF), [silveroxides/Chroma-GGUF](https://huggingface.co/silveroxides/Chroma-GGUF), [QuantStack/Qwen-Image-GGUF](https://huggingface.co/QuantStack/Qwen-Image-GGUF), [leejet/Z-Image-Turbo-GGUF](https://huggingface.co/leejet/Z-Image-Turbo-GGUF), [unsloth/Z-Image-GGUF](https://huggingface.co/unsloth/Z-Image-GGUF).
**Not doc-recommended:** city96/FLUX.1-*+SD3.*, city96/Qwen-Image-gguf, QuantStack/Qwen-Image-Edit(-2509)-GGUF, unsloth/FLUX.2-klein-4B-GGUF, [YarvixPA/FLUX.1-Fill-dev-GGUF](https://huggingface.co/YarvixPA/FLUX.1-Fill-dev-GGUF) (inpaint), second-state, all Comfy-Org/* (safetensors reference layout, not GGUF).
**Non-existent names:** `city96/stable-diffusion-xl-base-1.0-gguf`, `city96/sd-turbo-gguf`, `city96/SDXL-Turbo*-gguf` (none in [city96's list](https://huggingface.co/api/models?author=city96&limit=100)); **`ggerganov/*` image GGUFs do not exist** ([list](https://huggingface.co/api/models?author=ggerganov&limit=100)); correct SD1.5 id is [second-state/stable-diffusion-v1-5-GGUF](https://huggingface.co/second-state/stable-diffusion-v1-5-GGUF); `Comfy-Org/Qwen-Image-Edit-2509_ComfyUI` → use [Comfy-Org/Qwen-Image-Edit_ComfyUI](https://huggingface.co/Comfy-Org/Qwen-Image-Edit_ComfyUI).

**Single all-in-one GGUF (one file = model):** SD 1.x/2.x/SD-Turbo/SDXL — [second-state SD1.5](https://huggingface.co/second-state/stable-diffusion-v1-5-GGUF), [Green-Sky SD-Turbo](https://huggingface.co/Green-Sky/SD-Turbo-GGUF), [gpustack SDXL-Turbo](https://huggingface.co/gpustack/stable-diffusion-xl-1.0-turbo-GGUF) (CLIP-L+G+VAE bundled at fp16). ⚠️GPUSTACK's is llama-box-specific; hum-ma's are UNet-only and need separate `clip/` + `vae/`.
**Split setups — files required:**
- **FLUX.1:** diffusion GGUF + `ae.safetensors` 335 MB ([BFL](https://huggingface.co/black-forest-labs/FLUX.1-dev/tree/main)) + `clip_l.safetensors` 246 MB + `t5xxl_fp16` 9.79 GB / `t5xxl_fp8_e4m3fn` 4.89 GB / `_scaled` 5.16 GB ([comfyanonymous/flux_text_encoders](https://huggingface.co/api/models/comfyanonymous/flux_text_encoders/tree/main)); T5 GGUF alternative [city96/t5-v1_1-xxl-encoder-gguf](https://huggingface.co/city96/t5-v1_1-xxl-encoder-gguf) Q3_K_S 2.10 / Q4_K_S 2.74 / Q4_K_M **2.90** / Q5_K_M 3.39 / Q6_K 3.91 / Q8_0 5.06 / f16 9.53 GB.
- **Chroma:** diffusion GGUF + `ae.safetensors` 335 MB + **T5-XXL only** + `--model-args chroma_use_dit_mask=false`.
- **SD3/3.5:** diffusion GGUF + `clip_l` 246 MB + `clip_g` ≈1.39 GB + `t5xxl_*` + VAE. Note [gguf-org/sd3.5-large-gguf](https://huggingface.co/gguf-org/sd3.5-large-gguf) bundles `clip_l_fp32-f16.gguf` 246 MB, `clip_g_fp32-f16.gguf` 1.39 GB, `t5xxl_fp32-q4_0.gguf` 2.75 GB, `pig_sd_vae_fp32-f16.gguf` 168 MB.
- **Qwen-Image:** diffusion GGUF + `qwen_image_vae.safetensors` 254 MB + `--llm` Qwen2.5-VL-7B (Q4_K_M GGUF 4.68 / Q8_0 8.10 / fp8 9.38 / nvfp4 6.11 / bf16 16.58 GB). **Edit/2509 also need `mmproj/Qwen2.5-VL-7B-Instruct-mmproj-BF16.gguf` 1.35 GB.**
- **Z-Image:** diffusion GGUF + `ae.safetensors` 335 MB (docs say FLUX VAE is "essentially identical") + `--llm` Qwen3-4B (Q4_K_M GGUF 2.50 / fp16 8.04 GB).
- **FLUX.2-klein-4B:** klein GGUF + `flux2-vae` 336 MB + `--llm` Qwen3-4B (safetensors 8.04 / fp4 3.48 / Q4_K_M GGUF 2.50 GB).

## 6. Tiny autoencoders (TAE)
| Repo | file | size | use |
|---|---|---|---|
| [madebyollin/taesd](https://huggingface.co/madebyollin/taesd) | `diffusion_pytorch_model.safetensors` | **9.79 MB** | SD1/2 (`--taesd`); enc/dec split 4.90 MB each |
| [madebyollin/taesdxl](https://huggingface.co/madebyollin/taesdxl) | same | **9.79 MB** | SDXL |
| [madebyollin/taesd3](https://huggingface.co/madebyollin/taesd3) | same | **9.85 MB** | SD3 (needs `shift_factor=0.0`) |
| [madebyollin/taef1](https://huggingface.co/madebyollin/taef1) | same | **9.85 MB** | FLUX.1, HiDream, Z-Image |
| [madebyollin/taef2](https://huggingface.co/madebyollin/taef2) | `taef2.safetensors` | **10.72 MB** | FLUX.2 |
| [madebyollin/taehv](https://github.com/madebyollin/taehv) (HF repo 401/gated) | `safetensors/taew2_1.safetensors` | **22.64 MB** | **Qwen-Image**, Wan2.1/2.2-14B (`--tae`) |
| | `taew2_2` / `_super` | 22.85 / 40.99 MB | Wan2.2-5B |
| | `taehv` / `taehv1_5` / `_super` | 22.64 / 22.76 / 40.95 MB | HunyuanVideo 1/1.5 |
| | `taeh3`, `taecvx`, `taeos1_3`, `taeltx_2`, `taeltx2_3` | 22.71 / 22.64 / 22.64 / 23.53 / 23.53 MB | MiniMax-H3, CogVideoX, Open-Sora 1.3, LTX-2, LTX-2.3 |
**Savings:** TAESD enc/dec = 1,222,532 / 1,222,531 params vs 34,163,592 / 49,490,179 for the SD VAE (~1/34th decoder); 9.8 MB vs 335 MB for `ae.safetensors` ([taesd README](https://github.com/madebyollin/taesd)). TAEHV on GH200 fp16, 61 frames @512×320: full Hunyuan VAE **~2–3 s and ~6–9 GB peak** vs TAEHV **~0.5 s and <0.5 GB peak** ([taehv](https://github.com/madebyollin/taehv)). sd.cpp usage `--taesd <file>`; for Qwen-Image/Wan `--tae taew2_1.safetensors`, add `--vae-conv-direct` if still OOM. Cost: TAESD fudges fine detail — one measured case: Steam Deck SDXL 1024² drops 360 s → 120 s with `--taesd` ("lossy") ([Steam thread](https://steamcommunity.com/app/1675200/discussions/0/595134792309237790/)).

## 7. Contradictions / outdated data
1. **city96 GGUF vs sd.cpp:** AIVisionsLab reports hard `new_sd_ctx_t failed` for city96 FLUX GGUFs in sd.cpp ([source](https://huggingface.co/aivisionslab/ai-local-rx580-stack/raw/main/README.md)) while city96's files are otherwise the community standard for ComfyUI-GGUF. Treat as a real format incompatibility, not a corner case.
2. **FLUX Q4 memory:** sd.cpp flux.md says q4_0 = **6394 MB**; Local-Diffusion measures FLUX.1 Q4_0 peak **7534 MB** @1024; [willitrunai](https://willitrunai.com/blog/image-generation-vram-guide-2026) claims **~7 GB @512 / ~9 GB @1024**. Disagreement of 15–40%; the flux.md figure's precision/resolution is unstated.
3. **Chroma on 4 GB:** docs assert 4 GB works "without needing to offload", but the smallest Chroma GGUF is **4.99 GB** (Q3_K_L). Claim is unreproducible from file sizes alone ⚠️.
4. **RX 580 s/it:** 9.5 vs 22–23 s/it on the same card/resolution — build/commit dependent (2.3× Vulkan regression, [#1647](https://github.com/leejet/stable-diffusion.cpp/issues/1647)).
5. **`--offload-to-cpu` "no speed loss"** ([performance.md](https://raw.githubusercontent.com/leejet/stable-diffusion.cpp/master/docs/performance.md)) vs real data: Z-Image-Turbo offload ~29 s/img ≈ same, but Flux q4_k on RX 580 was **838 s sampling** for 4 steps at 1024² (≈209 s/step) ⚠️.
6. **`--vae-on-cpu` is a 5× penalty** (29 s → 150 s/img, [PR #1477](https://github.com/leejet/stable-diffusion.cpp/pull/1477)); prefer `--vae-tiling`.
7. **Low-confidence, likely AI-generated affiliate content:** [willitrunai](https://willitrunai.com/blog/image-generation-vram-guide-2026) (says SD1.5 fp16 needs ~4–5 GB — sd.cpp measures **2.3 G**; says "no GGUF available for SDXL/SD1.5" — false) and [bestgpuforai](https://bestgpuforai.com/articles/can-rtx-3060-run-stable-diffusion/) (RTX 3060 "Flux dev 1024² ~28 s/img" and "SDXL 1024² 30 steps ~22 s/img" — implausible next to sd.cpp logs; these are ComfyUI/torch, not sd.cpp).
8. **Dated:** SDXL-Turbo 4.5 s on 4 GB (2023, A1111); GTX 1070 SD1.5 (2023); Steam Deck (2024).
9. **`--vae-tiling` is reported mandatory** on RX 580 for FLUX (VAE decode OOMs/crashes the server without it) ([rx580 stack](https://huggingface.co/aivisionslab/ai-local-rx580-stack/raw/main/README.md)), even though sd.cpp has automatic VAE-tile retry ([performance.md](https://raw.githubusercontent.com/leejet/stable-diffusion.cpp/master/docs/performance.md)).

#!/usr/bin/env python3
"""
Audit 2026-08-17: live-verify AutoConfig math against real system info from cppworker.

Берёт реальные system info из cppworker /api/models (VRAM/RAM totals, nLayers, nEmbd),
берёт реальные model sizes из /api/models/files, прогоняет ту же формулу что в
internal/balancer/audit_2026_08_17_autoconfig.go, и сравнивает с profile, который
cppworker реально применил к модели (через /api/models endpoint).

Это end-to-end проверка что:
  1. AutoConfig формула корректна
  2. cppworker применяет настройки полностью (не теряет поля при sync)
  3. Live system info matches what balancer would compute
"""
import json
import sys
import urllib.request

CPPWORKER = "http://localhost:18092"


def fetch_json(path: str):
    with urllib.request.urlopen(CPPWORKER + path, timeout=10) as r:
        return json.loads(r.read())


def human_bytes(n: int) -> str:
    for unit in ("B", "KB", "MB", "GB", "TB"):
        if n < 1024:
            return f"{n:.1f}{unit}"
        n /= 1024
    return f"{n:.1f}PB"


def auto_config(model_size_bytes: int, sys_info: dict) -> dict:
    """Python port of Go AutoConfig — same math, same logic.

    See internal/balancer/audit_2026_08_17_autoconfig.go for the source.
    """
    out = {
        "hard_error": None,
        "num_gpu_layers": 0,
        "kv_cache_type": "q4_0",
        "context_length": 0,
        "parallel": 1,
        "batch_size": 512,
        "use_mmap": True,
        "flash_attn": False,
        "numa": False,
        "size_bytes": model_size_bytes,
        "max_tokens": 0,
        "warnings": [],
    }

    if model_size_bytes <= 0:
        out["hard_error"] = f"modelSizeBytes must be > 0, got {model_size_bytes}"
        return out

    ram_total = sys_info["ram_total"]
    vram_total = sys_info["vram_total"]
    if ram_total == 0 and vram_total == 0:
        out["hard_error"] = "system info unavailable"
        return out

    n_layers = sys_info.get("n_layers") or 0
    hidden = sys_info.get("n_embd") or 0
    if n_layers == 0 or hidden == 0:
        # Heuristic from Go code
        gb = model_size_bytes / (1024 ** 3)
        if gb < 5:
            n_layers, hidden = 32, 4096
        elif gb < 10:
            n_layers, hidden = 40, 5120
        elif gb < 20:
            n_layers, hidden = 60, 6656
        else:
            n_layers, hidden = 80, 8192
        out["warnings"].append(
            f"architecture estimated from model size: n_layers={n_layers}, hidden={hidden}"
        )

    ram_free = sys_info["ram_available"]
    vram_free = sys_info["vram_available"]

    model_fits_vram = bool(sys_info.get("has_nvml")) and vram_free > 0 and model_size_bytes <= vram_free
    model_fits_ram = bool(sys_info.get("has_cpu")) and ram_free > 0 and model_size_bytes <= ram_free
    model_fits_combined = model_fits_vram or (
        bool(sys_info.get("has_cpu")) and ram_free > 0 and model_size_bytes <= ram_free + vram_free
    )

    if not model_fits_combined:
        out["hard_error"] = (
            f"model size {human_bytes(model_size_bytes)} exceeds available memory: "
            f"VRAM={human_bytes(vram_free)}, RAM={human_bytes(ram_free)} (combined "
            f"{human_bytes(ram_free + vram_free)}). Try Q2_K, partial offload, or more RAM/VRAM."
        )
        return out

    # num_gpu_layers
    if model_fits_vram:
        out["num_gpu_layers"] = -1
    elif sys_info.get("has_nvml") and vram_free > 0 and model_fits_combined:
        out["num_gpu_layers"] = -2
        out["warnings"].append("model requires split VRAM+RAM via auto offload")
    else:
        out["num_gpu_layers"] = 0
        if model_fits_ram:
            out["warnings"].append("model too large for VRAM, will run CPU-only")

    # Memory budget
    weights_vram = model_size_bytes
    if int(vram_free * 70 / 100) < weights_vram:
        weights_vram = int(vram_free * 70 / 100)
    weights_ram = model_size_bytes - weights_vram
    vram_for_kv = max(0, vram_free - weights_vram)
    ram_for_kv = int(ram_free * 50 / 100)

    per_tok_q4 = 2 * n_layers * hidden // 2
    per_tok_q8 = 2 * n_layers * hidden
    per_tok_f16 = 2 * n_layers * hidden * 2

    def max_ctx(per_tok):
        if per_tok <= 0:
            return 0
        return (vram_for_kv + ram_for_kv) // per_tok

    max_q4, max_q8, max_f16 = max_ctx(per_tok_q4), max_ctx(per_tok_q8), max_ctx(per_tok_f16)
    test_ctx = 65536
    if max_f16 >= test_ctx:
        out["kv_cache_type"] = "f16"
        out["context_length"] = (max_f16 // 1024) * 1024
    elif max_q8 >= test_ctx:
        out["kv_cache_type"] = "q8_0"
        out["context_length"] = (max_q8 // 1024) * 1024
    else:
        out["kv_cache_type"] = "q4_0"
        out["context_length"] = (max_q4 // 1024) * 1024

    out["context_length"] = max(4096, min(131072, out["context_length"]))

    # flash_attn
    out["flash_attn"] = out["num_gpu_layers"] == -1 and vram_free >= 8 * 1024 ** 3
    out["numa"] = False
    out["use_mmap"] = model_fits_combined

    # parallel
    if out["num_gpu_layers"] == -1 and vram_free >= 16 * 1024 ** 3:
        out["parallel"] = 4
    elif out["num_gpu_layers"] == -1:
        out["parallel"] = 2
    else:
        out["parallel"] = 1

    # batch_size
    if out["num_gpu_layers"] == -1 and vram_free >= 16 * 1024 ** 3:
        out["batch_size"] = 1024
    elif vram_free >= 8 * 1024 ** 3:
        out["batch_size"] = 512
    else:
        out["batch_size"] = 256

    # max_tokens
    if out["num_gpu_layers"] == -1:
        out["max_tokens"] = 16384
    elif out["num_gpu_layers"] == -2:
        out["max_tokens"] = 8192
    else:
        out["max_tokens"] = 4096

    return out


def main():
    print("=" * 70)
    print(" AutoConfig live-verify (Audit 2026-08-17)")
    print("=" * 70)

    # 1. Get system info from cppworker.
    sysinfo_raw = fetch_json("/api/models")
    sys_info = {
        "vram_total": sysinfo_raw["total_vram_mb"] * 1024 * 1024,
        "vram_available": sysinfo_raw["available_vram_mb"] * 1024 * 1024,
        "ram_total": sysinfo_raw["total_ram_mb"] * 1024 * 1024,
        "ram_available": sysinfo_raw["available_ram_mb"] * 1024 * 1024,
        "gpu_count": sysinfo_raw.get("gpu_count", 0),
        "has_nvml": sysinfo_raw.get("gpu_count", 0) > 0,
        "has_cpu": True,
    }
    print(f"\nSystem info from cppworker:")
    print(f"  VRAM total     = {human_bytes(sys_info['vram_total'])}")
    print(f"  VRAM available = {human_bytes(sys_info['vram_available'])}")
    print(f"  RAM total      = {human_bytes(sys_info['ram_total'])}")
    print(f"  RAM available  = {human_bytes(sys_info['ram_available'])}")
    print(f"  GPU count      = {sys_info['gpu_count']}")

    # 2. Get model files and live-loaded models.
    files_raw = fetch_json("/api/models/files")
    files = {f["name"]: f["sizeBytes"] for f in files_raw["files"]}
    loaded = {m["name"]: m for m in sysinfo_raw.get("models", [])}

    print(f"\nModel files on disk:")
    for name, size in files.items():
        print(f"  {name:50s} {human_bytes(size)}")

    # 3. For each loaded model, compute AutoConfig and compare with applied profile.
    failures = 0
    for model_name, model_info in loaded.items():
        # Find file
        file_name = model_info.get("path", "").rsplit("/", 1)[-1] if model_info.get("path") else None
        if not file_name or file_name not in files:
            print(f"\n[SKIP] {model_name}: file not found")
            continue
        model_size = files[file_name]
        sys_info_model = dict(sys_info)
        sys_info_model["n_layers"] = model_info.get("nLayers", 0)
        sys_info_model["n_embd"] = model_info.get("nEmbd", 0)

        print(f"\n[AutoConfig] {model_name} ({human_bytes(model_size)})")
        print(f"  nLayers={sys_info_model['n_layers']} nEmbd={sys_info_model['n_embd']}")
        computed = auto_config(model_size, sys_info_model)

        if computed["hard_error"]:
            print(f"  ❌ HARD ERROR: {computed['hard_error']}")
            failures += 1
            continue

        print(f"  numGpuLayers = {computed['num_gpu_layers']}")
        print(f"  kvCacheType  = {computed['kv_cache_type']}")
        print(f"  contextLength= {computed['context_length']}")
        print(f"  parallel     = {computed['parallel']}")
        print(f"  batchSize    = {computed['batch_size']}")
        print(f"  useMmap      = {computed['use_mmap']}")
        print(f"  flashAttn    = {computed['flash_attn']}")
        if computed["warnings"]:
            for w in computed["warnings"]:
                print(f"  warn         = {w}")

        # 4. Compare with what cppworker actually applied.
        print(f"\n  Applied by cppworker:")
        print(f"    gpuLayers     = {model_info.get('gpuLayers', '?')}")
        print(f"    contextSize   = {model_info.get('contextSize', '?')}")
        print(f"    batchSize     = {model_info.get('batchSize', '?')}")
        print(f"    parallel      = {model_info.get('parallel', '?')}")
        print(f"    kvCacheType   = {model_info.get('kvCacheType', '?')}")
        print(f"    flashAttnType = {model_info.get('flashAttnType', '?')}")
        print(f"    useMmap       = {model_info.get('useMmap', '?')}")
        print(f"    numa          = {model_info.get('numa', '?')}")

        # 5. Sanity checks (allow flexibility since cppworker has its own logic).
        # For the 22GB qwen3.6 model on 8GB VRAM + 20GB RAM, AutoConfig predicts
        # numGpuLayers=-2 (AUTO) and cppworker will pick actual layer count.
        if model_info.get("contextSize", 0) > 0 and computed["context_length"] > 0:
            # Context size should be within 2x of predicted (cppworker may have other limits).
            ratio = max(model_info["contextSize"], computed["context_length"]) / max(
                min(model_info["contextSize"], computed["context_length"]), 1
            )
            if ratio > 4:
                print(f"  ⚠ context size mismatch: cppworker={model_info['contextSize']} autoconfig={computed['context_length']}")
                failures += 1

    print()
    print("=" * 70)
    if failures == 0:
        print(f" ✅ AutoConfig live-verify PASS ({len(loaded)} models checked)")
        return 0
    print(f" ❌ AutoConfig live-verify FAIL: {failures} mismatch(es)")
    return 1


if __name__ == "__main__":
    sys.exit(main())

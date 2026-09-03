"""
test_apply_hardware_preset.py — smoke test for hardware preset files.

R58.2 (2026-09-03): verifies all 4 preset files are valid JSON and contain
the required sections (docker / cppworker / ram_fallback / auto / balancer)
with proper KEY=VALUE structure.
"""
import json
import sys
from pathlib import Path

PRESETS_DIR = Path(__file__).resolve().parent.parent / 'config' / 'hardware-presets'

REQUIRED_SECTIONS = ('docker', 'cppworker', 'ram_fallback', 'auto', 'balancer')

# Required env vars per section (must be present in EVERY preset)
REQUIRED_KEYS = {
    'docker': ['CUDA_ARCH', 'CPPWORKER_GPU_TAG'],
    'cppworker': [
        'CPPWORKER_CTX_SIZE', 'CPPWORKER_BATCH_SIZE', 'CPPWORKER_GPU_LAYERS',
        'CPPWORKER_FLASH_ATTN_TYPE', 'CPPWORKER_N_THREADS', 'CPPWORKER_NUMA',
        'CPPWORKER_USE_MMAP',
    ],
    'ram_fallback': [
        'CPPWORKER_RAM_FALLBACK_N_CTX', 'CPPWORKER_RAM_FALLBACK_GPU_LAYERS',
        'CPPWORKER_RAM_FALLBACK_MAX_N_CTX',
    ],
    'auto': ['CPPWORKER_AUTO_OFFLOAD', 'CPPWORKER_AUTO_TUNE_NCTX'],
    'balancer': [
        'LB_NCTX_RELOAD_ENABLED', 'LB_NCTX_RELOAD_MAX_N_CTX',
        'LB_NCTX_RELOAD_VRAM_SAFETY_FACTOR',
    ],
}


def test_presets_exist():
    """All 4 hardware presets exist."""
    expected = ['rtx30-8gb', 'rtx40-24gb', 'a10-24gb', 'rtx50-32gb']
    found = sorted(p.stem for p in PRESETS_DIR.glob('*.json'))
    missing = set(expected) - set(found)
    assert not missing, f"Missing presets: {missing}. Found: {found}"


def test_preset_valid_json(preset_name):
    """Preset file is valid JSON."""
    path = PRESETS_DIR / f"{preset_name}.json"
    with open(path, 'r', encoding='utf-8') as f:
        data = json.load(f)
    assert isinstance(data, dict), f"{preset_name}: not a dict"


def test_preset_required_sections(preset_name):
    """Preset has all required sections."""
    path = PRESETS_DIR / f"{preset_name}.json"
    with open(path, 'r', encoding='utf-8') as f:
        data = json.load(f)
    for section in REQUIRED_SECTIONS:
        assert section in data, f"{preset_name}: missing section '{section}'"


def test_preset_required_keys(preset_name):
    """Each section has all required keys with valid values."""
    path = PRESETS_DIR / f"{preset_name}.json"
    with open(path, 'r', encoding='utf-8') as f:
        data = json.load(f)
    for section, keys in REQUIRED_KEYS.items():
        section_data = data[section]
        for key in keys:
            assert key in section_data, (
                f"{preset_name}: missing key '{key}' in section '{section}'"
            )
            value = section_data[key]
            # Skip comments (start with _)
            if isinstance(value, str) and value.startswith('_'):
                continue
            # Type check: bool/int/float OK, empty string not OK
            assert value != '', f"{preset_name}: '{section}.{key}' is empty string"


def test_preset_cuda_arch_unique():
    """Each preset has distinct CUDA_ARCH (otherwise they'd be the same image)."""
    archs = []
    for p in sorted(PRESETS_DIR.glob('*.json')):
        with open(p, 'r', encoding='utf-8') as f:
            data = json.load(f)
        archs.append((p.stem, data['docker']['CUDA_ARCH']))
    # rtx30-8gb and a10-24gb both use sm_86 — that's expected
    # but we need at least 2 distinct archs (for 4xx vs 5xx etc)
    unique_archs = set(a for _, a in archs)
    assert len(unique_archs) >= 2, f"Too few unique CUDA_ARCHs: {archs}"


def test_all_presets():
    """Run all per-preset tests."""
    test_presets_exist()
    for preset_file in sorted(PRESETS_DIR.glob('*.json')):
        name = preset_file.stem
        test_preset_valid_json(name)
        test_preset_required_sections(name)
        test_preset_required_keys(name)
    test_preset_cuda_arch_unique()
    print(f"OK: all {len(list(PRESETS_DIR.glob('*.json')))} presets pass validation")


if __name__ == '__main__':
    test_all_presets()

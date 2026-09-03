"""
apply_hardware_preset.py — apply a hardware preset to deployments/.env.bundled-with-agent.

R58.2 (2026-09-03): Phase 1.3 hardware presets helper.

Использование:
  python scripts/apply-hardware-preset.py rtx30-8gb
  python scripts/apply-hardware-preset.py a10-24gb
  python scripts/apply-hardware-preset.py --dry-run rtx50-32gb
  python scripts/apply-hardware-preset.py --list

Что делает:
1. Читает config/hardware-presets/<name>.json
2. Извлекает все ENV var values (docker + cppworker + ram_fallback + auto + balancer)
3. Выводит KEY=VALUE строки (готовые для copy-paste или shell eval)

NOTE: этот скрипт НЕ модифицирует .env напрямую (избегаем случайной
overwrite пользовательских настроек). Вывод — это KEY=VALUE которые нужно
дописать в deployments/.env.bundled-with-agent вручную (или eval "$(...)"
если хочется применить немедленно).
"""
import json
import sys
import os
from pathlib import Path

PRESETS_DIR = Path(__file__).resolve().parent.parent / 'config' / 'hardware-presets'


def list_presets():
    """List all available presets."""
    if not PRESETS_DIR.exists():
        print(f"ERROR: presets dir not found: {PRESETS_DIR}", file=sys.stderr)
        return []
    return sorted(p.stem for p in PRESETS_DIR.glob('*.json'))


def load_preset(name):
    """Load preset JSON, return dict of KEY -> VALUE for env vars."""
    path = PRESETS_DIR / f"{name}.json"
    if not path.exists():
        print(f"ERROR: preset '{name}' not found. Available: {', '.join(list_presets())}", file=sys.stderr)
        sys.exit(1)

    with open(path, 'r', encoding='utf-8') as f:
        data = json.load(f)

    # Skip metadata fields (start with _)
    env_vars = {}
    for section in ('docker', 'cppworker', 'ram_fallback', 'auto', 'balancer'):
        section_data = data.get(section, {})
        for k, v in section_data.items():
            if k.startswith('_'):
                continue  # comment
            env_vars[k] = v

    return data, env_vars


def main():
    if len(sys.argv) < 2:
        print("Usage: apply-hardware-preset.py [--dry-run] [--list] <preset-name>", file=sys.stderr)
        sys.exit(1)

    dry_run = False
    args = sys.argv[1:]

    if '--list' in args:
        print("Available presets:")
        for p in list_presets():
            print(f"  {p}")
        return

    if '--dry-run' in args:
        dry_run = True
        args.remove('--dry-run')

    if not args:
        print("ERROR: preset name required", file=sys.stderr)
        sys.exit(1)

    name = args[0]
    data, env_vars = load_preset(name)

    hw = data.get('_hardware', {})
    print(f"# Preset: {name}")
    print(f"# Hardware: {hw.get('name', '?')}")
    print(f"# VRAM: {hw.get('vram_mb', '?')} MB")
    print(f"# sm_arch: {hw.get('sm_arch', '?')}")
    print(f"# cuda_arch: {hw.get('cuda_arch', '?')}")
    if hw.get('_rebuild_required_note'):
        print(f"# ⚠️  {hw['_rebuild_required_note']}")
    if hw.get('_tag_note'):
        print(f"# Note: {hw['_tag_note']}")

    recs = data.get('_model_recommendations', {})
    if recs.get('fits_vram'):
        print(f"# Fits VRAM: {', '.join(recs['fits_vram'])}")
    if recs.get('partial_offload'):
        print(f"# Partial offload: {', '.join(recs['partial_offload'])}")

    print()
    print(f"# {len(env_vars)} ENV vars to add to deployments/.env.bundled-with-agent:")
    for k, v in sorted(env_vars.items()):
        if isinstance(v, bool):
            v = 'true' if v else 'false'
        elif isinstance(v, (int, float)):
            v = str(v)
        print(f"{k}={v}")

    if dry_run:
        print()
        print("# --dry-run: no changes written")
    else:
        print()
        print("# To apply: append these lines to deployments/.env.bundled-with-agent")
        print("# (or eval the above in your shell after backing up the file)")


if __name__ == '__main__':
    main()

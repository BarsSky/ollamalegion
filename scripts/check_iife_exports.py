"""CI guard: detect JS files that end with `const X = (function() { ... })();`
WITHOUT a trailing `if (typeof window !== 'undefined') window.X = X;` block.

The pattern has caused 6 regressions in the R59 series (R59.3
WebSocketManager, R59.5 gguf-renderer-detail-render, R59.6 showToast,
R59.7 GgufApi, R59.7 Utils, R59.7 GgufApi again). Each time the
file's IIFE returned a public API that some consumer expected at
`window.X`, but no one ever assigned to `window.X` — so the consumer
saw `undefined` and either crashed (TypeError) or silently rendered
empty (silent failure).

This script scans webui/js/ for files matching the dangerous pattern
and exits 1 if any are found. Designed to run in pre-commit or CI.

Usage:
  python scripts/check_iife_exports.py                # check all files
  python scripts/check_iife_exports.py webui/js/app.js  # check one file

Exit 0 = all IIFE-style files export to window, exit 1 = missing
exports detected.
"""
import os
import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
JS_DIR = ROOT / "webui" / "js"

# Files that LEGITIMATELY don't need window export:
# - app.js: the public API is consumed via the `ui` closure variable
#   (passed to `document.addEventListener('DOMContentLoaded', () => ui.init())`
#   on line 2536). We do not use window.ui.
# - app-core.js, app-listeners.js, app-modals.js: these mutate the
#   `window.App` namespace via `App.X = function() {...}` (direct
#   namespace mutation, not IIFE export). Exempt.
# - i18n/index.js, monitor/init.js, etc.: not IIFE.
# - setup-wizard-presets.js: uses `window.HardwarePresets = {...}`
#   explicit assignment, not IIFE.

# Pattern: `const X = (function() { ... })();` at line start (no indent),
# where the file does NOT contain `window.X = X` (or `window.X = Y` for
# any X) somewhere after the IIFE close.
# We detect the IIFE pattern with regex on the first matching line, and
# then look for the export block in the remaining file.

IIFE_PATTERN = re.compile(
    r'^\s*const\s+([A-Z][A-Za-z0-9_]*)\s*=\s*\(\s*function\s*\(',
    re.MULTILINE,
)
# Built per-name to avoid backref issues
def make_export_pattern(name):
    return re.compile(r'window\.' + re.escape(name) + r'\s*=')


def check_file(path: Path) -> list[str]:
    """Return list of IIFE names that have no window.X = X export in this file."""
    try:
        text = path.read_text(encoding="utf-8")
    except UnicodeDecodeError:
        return []

    findings = []
    for m in IIFE_PATTERN.finditer(text):
        name = m.group(1)
        # Look for `window.<name> = ...` somewhere later in the file
        after = text[m.end():]
        if not make_export_pattern(name).search(after):
            findings.append(name)
    return findings


def main():
    # Collect JS files to check
    if len(sys.argv) > 1:
        files = [Path(p).resolve() for p in sys.argv[1:]]
    else:
        files = sorted(JS_DIR.rglob("*.js"))

    if not files:
        print(f"[i] no JS files to check in {JS_DIR}", file=sys.stderr)
        return 0

    # Known exceptions — files where the IIFE pattern is intentional and
    # the public API is consumed via a different route (closure variable,
    # direct window.X = { ... } assignment, or not used externally).
    EXEMPT_PREFIXES = (
        "webui/js/app.js",                # public API via `ui` closure
        "webui/js/app-core.js",           # mutates window.App directly
        "webui/js/app-listeners.js",      # mutates window.App directly
        "webui/js/app-modals.js",         # mutates window.BackendCRUD
        "webui/js/i18n/",                 # not IIFE
        "webui/js/modules/setup-wizard-presets.js",  # window.HardwarePresets = {...}
    )

    failed = []
    for path in files:
        rel = str(path.relative_to(ROOT)).replace("\\", "/")
        if any(rel.startswith(p) for p in EXEMPT_PREFIXES):
            continue
        missing = check_file(path)
        if missing:
            for name in missing:
                failed.append((rel, name))

    if not failed:
        print(f"[PASS] all {len(files)} files export IIFE public API to window")
        return 0

    print(f"[FAIL] {len(failed)} IIFE(s) missing window.X = X export:")
    for rel, name in failed:
        print(f"  {rel}  →  {name}")
    print()
    print("FIX: append this block after the IIFE close:")
    print("  if (typeof window !== 'undefined') {")
    print("      window.<NAME> = <NAME>;")
    print("  }")
    return 1


if __name__ == "__main__":
    sys.exit(main())

#!/usr/bin/env python3
"""R60.24 — Bump webui cache-bust to force fresh browser fetch.

Background: nginx serves .js with `Cache-Control: public, immutable,
max-age=31536000` (1 year). The old ?v=16/1/17/18/19/20/24 versions
were cached by browsers. R59.x fixes (window.Utils = Utils,
window.WebSocketManager = WebSocketManager, window.Renderers = Renderers)
were never picked up by returning visitors because the URL stayed the
same. Webui dashboard showed "—" cards forever and "● Подключение..."
stuck badge.

Fix: bump ALL `?v=N` to a single new version (matching image tag).
Operators: re-run this script on every webui build that touches JS modules.

Usage:
    python scripts/bump_webui_cache_bust.py "R60.24"
    python scripts/bump_webui_cache_bust.py "R60.24" --dry-run
"""
import re
import sys
from pathlib import Path

DEFAULT_OLD_PATTERN = re.compile(r"\?v=[0-9A-Za-z._-]+")
DEFAULT_FILES = ["index.html", "monitor.html", "health.html", "rpc-status.html",
                 "tp-pipeline.html", "virtual-models.html"]


def bump_file(path: Path, new_version: str, dry_run: bool = False) -> tuple[int, str]:
    """Replace all ?v=... with ?v=NEW_VERSION. Returns (count, new_text)."""
    if not path.exists():
        return 0, ""
    text = path.read_text(encoding="utf-8")
    matches = DEFAULT_OLD_PATTERN.findall(text)
    count = len(matches)
    if count == 0:
        return 0, text
    new_text = DEFAULT_OLD_PATTERN.sub(f"?v={new_version}", text)
    if not dry_run:
        path.write_text(new_text, encoding="utf-8")
    return count, new_text


def main():
    args = [a for a in sys.argv[1:] if not a.startswith("--")]
    flags = {a for a in sys.argv[1:] if a.startswith("--")}
    if not args:
        print("Usage: python bump_webui_cache_bust.py <NEW_VERSION> [--dry-run]")
        print("Example: python bump_webui_cache_bust.py R60.24")
        sys.exit(1)
    new_version = args[0]
    dry_run = "--dry-run" in flags
    webui_dir = Path(__file__).parent.parent / "webui"
    print(f"WebUI dir: {webui_dir}")
    print(f"New version: {new_version}")
    print(f"Dry run: {dry_run}")
    print()
    total = 0
    for filename in DEFAULT_FILES:
        path = webui_dir / filename
        count, _ = bump_file(path, new_version, dry_run)
        if count > 0:
            print(f"  {filename}: {count} replacements")
            total += count
    print(f"\nTotal: {total} cache-bust updates {'(dry run)' if dry_run else ''}")


if __name__ == "__main__":
    main()

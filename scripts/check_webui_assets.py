#!/usr/bin/env python3
"""check_webui_assets.py — R67a (2026-09-23).

Гарантия «WebUI не теряет стили и иконки на машинах без сети».

Что проверяется:
  1. Ни одна страница/стиль WebUI не ссылается на ВНЕШНИЙ ресурс (CDN, шрифты
     Google, unpkg/jsdelivr/cdnjs и т.п.). Ссылки-анкоры (href="https://...")
     на документацию допускаются — это навигация, а не загрузка ассета.
  2. Каждый локальный ассет, на который ссылаются HTML и CSS, существует в
     каталоге webui/ (то есть попадёт в образ: Dockerfile копирует webui/css/,
     webui/js/, webui/img/, webui/webfonts/ и webui/*.html).
  3. Шрифты Font Awesome, объявленные в css/font-awesome.min.css как url(...),
     присутствуют локально.

Зачем: стили/иконки уже лежат в образе (внешних CDN нет), но это свойство легко
сломать одной строкой `<link href="https://cdn...">`. В CI-джобе
webui-static-checks скрипт падает с exit 1 и указывает конкретный файл/строку.

Использование:
    python scripts/check_webui_assets.py [--root webui]
"""

from __future__ import annotations

import argparse
import re
import sys
from pathlib import Path

ASSET_EXT = (".css", ".js", ".png", ".jpg", ".jpeg", ".gif", ".svg", ".ico",
             ".woff", ".woff2", ".ttf", ".eot", ".webp", ".json")
FONT_EXT = (".woff2", ".woff", ".ttf", ".otf", ".eot")

# Атрибуты, которыми браузер ЗАГРУЖАЕТ ресурс (в отличие от href-навигации).
LOADING_ATTRS = ("src", "data-src", "poster")

# Домены, которые допустимы как навигационные ссылки (анкоры), не ассеты.
ALLOWED_NAV_HOSTS = ("github.com", "fontawesome.com", "ollama.com")

ATTR_RE = re.compile(r'(\w[\w-]*)\s*=\s*"([^"]+)"')
CSS_URL_RE = re.compile(r'url\(\s*[\'"]?([^\'")]+)[\'"]?\s*\)')


def is_external(url: str) -> bool:
    return url.startswith("//") or url.startswith("http://") or url.startswith("https://")


def strip_query(url: str) -> str:
    return url.split("?", 1)[0].split("#", 1)[0]


def host_of(url: str) -> str:
    m = re.match(r"^(?:https?:)?//([^/]+)", url)
    return m.group(1) if m else ""


def missing_asset(target: Path) -> bool:
    """True, если ассета реально нет.

    Форматные fallback'и (@font-face src: woff2, затем woff/ttf/eot) не считаем
    ошибкой: если файла с нужным расширением нет, но рядом лежит файл с тем же
    именем и другим шрифтовым расширением — браузер возьмёт поддерживаемый
    формат. Именно так устроен vendored font-awesome.min.css (в репозитории
    только .woff2, а .ttf/.woff/eot указаны как fallback для старых браузеров).
    """
    if target.exists():
        return False
    if target.suffix.lower() in FONT_EXT and target.parent.is_dir():
        for sibling in target.parent.glob(target.stem + ".*"):
            if sibling.suffix.lower() in FONT_EXT:
                return False
    return True


def check_html(path: Path, root: Path, errors: list[str]) -> int:
    text = path.read_text(encoding="utf-8", errors="replace")
    checked = 0
    for m in ATTR_RE.finditer(text):
        attr, url = m.group(1), m.group(2).strip()
        if not url or url.startswith(("#", "mailto:", "data:", "javascript:")):
            continue
        if is_external(url):
            if attr in LOADING_ATTRS:
                errors.append(f"{path.name}: внешний ассет в {attr}=\"{url}\" "
                              f"(CDN недопустим: на машине без сети ассет не загрузится)")
            elif host_of(url) not in ALLOWED_NAV_HOSTS:
                # <link rel=stylesheet href=https://...> тоже загрузка
                if attr == "href" and strip_query(url).endswith(ASSET_EXT):
                    errors.append(f"{path.name}: внешняя ссылка на ассет href=\"{url}\"")
            continue
        target = strip_query(url)
        if not target.endswith(ASSET_EXT):
            continue
        checked += 1
        rel = target.lstrip("/")
        if missing_asset(root / rel):
            errors.append(f"{path.name}: ссылка на отсутствующий ассет {target} "
                          f"(нет файла webui/{rel})")
    return checked


def check_css(path: Path, root: Path, errors: list[str]) -> int:
    text = path.read_text(encoding="utf-8", errors="replace")
    checked = 0
    for m in CSS_URL_RE.finditer(text):
        url = m.group(1).strip()
        if not url or url.startswith("data:"):
            continue
        if is_external(url):
            errors.append(f"{path.relative_to(root)}: внешний ресурс в url(\"{url}\")")
            continue
        checked += 1
        target = (path.parent / strip_query(url)).resolve()
        if missing_asset(target):
            errors.append(f"{path.relative_to(root)}: url(\"{url}\") — файла нет")
    return checked


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--root", default="webui", help="каталог статики WebUI")
    args = ap.parse_args()

    root = Path(args.root)
    if not root.is_dir():
        print(f"[assets] ERROR: нет каталога {root}", file=sys.stderr)
        return 2

    errors: list[str] = []
    html_checked = css_checked = 0

    for html in sorted(root.glob("*.html")):
        html_checked += check_html(html, root, errors)
    for css in sorted((root / "css").glob("*.css")):
        css_checked += check_css(css, root, errors)

    # Шрифты Font Awesome должны быть локальными.
    fa = root / "css" / "font-awesome.min.css"
    if fa.exists():
        for m in CSS_URL_RE.finditer(fa.read_text(encoding="utf-8", errors="replace")):
            url = m.group(1).strip()
            if url.startswith("../webfonts/"):
                target = (fa.parent / url).resolve()
                if missing_asset(target):
                    errors.append(f"css/font-awesome.min.css: нет шрифта {url}")

    if errors:
        print("[assets] FAIL — внешние или отсутствующие ассеты:", file=sys.stderr)
        for e in errors:
            print(f"  * {e}", file=sys.stderr)
        return 1

    print(f"[assets] OK: {html_checked} локальных ассетов в HTML + "
          f"{css_checked} url() в CSS; внешних CDN нет, шрифты на месте")
    return 0


if __name__ == "__main__":
    sys.exit(main())

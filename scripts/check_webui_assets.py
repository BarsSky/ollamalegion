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

# R238c: иконки Font Awesome, которых нет в вендоренном бандле, рисуются пустым
# местом (content: none) — визуально это «пропавшая иконка» на странице. Именно
# так выглядел .fa-huggingface на офлайн-хосте. Собираем все fa-* токены из HTML
# и JS и сверяем с классами, объявленными в CSS (включая наши локальные
# переопределения в icons.css).
FA_CLASS_RE = re.compile(r'\.fa-([a-z0-9-]+)')
FA_TOKEN_RE = re.compile(r'\bfa-([a-z0-9-]+)\b')
FA_MODIFIERS = set('''fw lg sm xs 1x 2x 3x 4x 5x 6x 7x 8x 9x 10x spin spin-pulse pulse beat fade
rotate-05 rotate-90 rotate-180 rotate-270 flip-horizontal flip-vertical flip-both flip-inverse border
list ul-inline li stack stack-1x stack-2x inverse sr-only sr-only-focusable pull-left pull-right swap-opacity
swap-opacity-alt pull-none solid regular brands sharp light thin duotone classic'''.split())


def check_fa_icon_classes(root: Path, errors: list[str]) -> int:
    """Возвращает число проверенных fa-* токенов; ошибки — в errors."""
    css_dir = root / "css"
    if not css_dir.is_dir():
        return 0
    defined: set[str] = set()
    for css in sorted(css_dir.glob("*.css")):
        text = css.read_text(encoding="utf-8", errors="replace")
        defined.update(FA_CLASS_RE.findall(text))

    used: dict[str, set[str]] = {}
    files = list(sorted(root.glob("*.html")))
    js_dir = root / "js"
    if js_dir.is_dir():
        files += [p for p in sorted(js_dir.rglob("*.js")) if ".test." not in p.name]
    for f in files:
        text = f.read_text(encoding="utf-8", errors="replace")
        for name in FA_TOKEN_RE.findall(text):
            if name in FA_MODIFIERS:
                continue
            used.setdefault(name, set()).add(str(f.relative_to(root)))

    for name in sorted(used):
        if name not in defined:
            where = ", ".join(sorted(used[name])[:3])
            errors.append(
                f"Font Awesome: класса fa-{name} нет ни в одном CSS WebUI — "
                f"иконка отрисуется пустой (используется в {where})")
    return len(used)


def is_external(url: str) -> bool:
    return url.startswith("//") or url.startswith("http://") or url.startswith("https://")


def strip_query(url: str) -> str:
    return url.split("?", 1)[0].split("#", 1)[0]


def host_of(url: str) -> str:
    m = re.match(r"^(?:https?:)?//([^/]+)", url)
    return m.group(1) if m else ""


def missing_asset(target: Path) -> bool:
    """True, если ассета реально нет.

    R675 (2026-11-21): ШРИФТОВАЯ ПОБЛАЖКА УБРАНА — она МАСКИРОВАЛА дефект.

    ЧТО БЫЛО. Здесь стояло исключение: если файла `.ttf` нет, но рядом лежит
    `.woff2` с тем же именем, ассет считался «на месте» — подразумевалось, что
    браузер «возьмёт поддерживаемый формат». Это неверно: `src:` в @font-face —
    это НЕ «если одного нет, возьми другой». Каждый перечисленный `url(...)`
    браузер обязан загрузить в порядке приоритета, и 404 на первом же мёртвом
    кандидате означает, что шрифт не загрузился вовсе. Именно так на втором
    стенде (скриншот 1124x569) часть иконок превратилась в ПУСТЫЕ КВАДРАТЫ:
    font-awesome.min.css перечислял несуществующие `.ttf` (их нет ни в
    репозитории, ни в образе), чекер объявлял это нормой, а 404 на `/webfonts/
    fa-solid-900.ttf` отдавался ещё и как `200 + index.html` (try_files), после
    чего браузер молча выбрасывал «шрифт» из-за nosniff.

    ТЕПЕРЬ. Отсутствие ЛЮБОГО файла, который реально перечислен в CSS/HTML, —
    ошибка. Vendored font-awesome.min.css почищен (объявлен только `.woff2`),
    поэтому проверка зелёная; а если кто-то снова допишет мёртвый fallback —
    ci-джоб webui-static-checks упадёт на конкретном файле и строке.
    """
    return not target.exists()


def font_sibling_hint(target: Path) -> str:
    """R675: подсказка для шрифтов — соседний формат НЕ спасает.

    Нужна, чтобы диагностика называла причину, а не просто «файла нет»: именно
    наличие рядом `.woff2` раньше читалось как «всё в порядке».
    """
    if target.suffix.lower() not in FONT_EXT or not target.parent.is_dir():
        return ""
    siblings = sorted(p.name for p in target.parent.glob(target.stem + ".*")
                      if p.suffix.lower() in FONT_EXT)
    if not siblings:
        return ""
    return (f" (рядом есть {', '.join(siblings)}, но браузер НЕ подставит соседний "
            f"формат: каждый url() из src перечислен явно, мёртвый кандидат — 404)")


FONT_FACE_RE = re.compile(r'@font-face\s*\{([^}]*)\}')
SRC_DECL_RE = re.compile(r'src\s*:\s*([^;}]+)')


def check_font_face_src(path: Path, root: Path, errors: list[str]) -> int:
    """R675: один @font-face — ровно один src, и он указывает на существующий файл.

    ЗАЧЕМ ОТДЕЛЬНОЕ ПРАВИЛО, ЕСЛИ ЕСТЬ ОБЩАЯ ПРОВЕРКА url(). Общая проверка
    смотрит только «файл существует». Дефект R675 был в ДРУГОМ измерении: в
    одном `src:` перечислялись два файла, и «мёртвый» второй кандидат (+
    `?v=R83`, который не бампался) отправлял браузер в 404, где `try_files`
    отдавал index.html с `nosniff`. Поэтому фиксируем форму: ровно один формат
    на семейство — тогда нечему «мёртвым» кандидатом проваливать загрузку.
    """
    if not path.is_file():
        return 0
    text = path.read_text(encoding="utf-8", errors="replace")
    blocks = FONT_FACE_RE.findall(text)
    for i, block in enumerate(blocks, start=1):
        srcs = SRC_DECL_RE.findall(block)
        family = re.search(r'font-family\s*:\s*"([^"]+)"', block)
        name = family.group(1) if family else f"#{i}"
        if len(srcs) != 1:
            errors.append(f"{path.relative_to(root)}: @font-face {name} объявляет "
                          f"{len(srcs)} src — ожидается ровно один")
            continue
        urls = CSS_URL_RE.findall(srcs[0])
        if len(urls) != 1:
            errors.append(f"{path.relative_to(root)}: @font-face {name} перечисляет "
                          f"{len(urls)} url() в одном src — лишние форматы мёртвые "
                          f"(браузер не «подставит другой»): {', '.join(urls)}")
            continue
        url = strip_query(urls[0].strip())
        if not url.startswith("data:") and missing_asset((path.parent / url).resolve()):
            errors.append(f"{path.relative_to(root)}: @font-face {name} ссылается на "
                          f"отсутствующий файл {url}")
    return len(blocks)


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
            errors.append(f"{path.relative_to(root)}: url(\"{url}\") — файла нет"
                          f"{font_sibling_hint(target)}")
    return checked


def check_balancer_panel_copy(dockerfile: Path, root: Path, errors: list[str]) -> int:
    """R134 (2026-10-16): панель балансера обязана нести ТУ ЖЕ статику WebUI.

    ЖИВОЙ ДЕФЕКТ, ради которого эта проверка появилась. Образ балансера
    копирует webui-статику в /app/webui и отдаёт её сам (GET /monitor, /health,
    /js/..., /css/...). Копировались только monitor.html и health.html, поэтому:

      * `/webfonts/fa-solid-900.woff2` отдавал 404, и иконки Font Awesome не
        рисовались («не отображаются все иконки» — живая жалоба оператора);
      * `/index.html` отдавал 404, то есть дашборда с навигацией (включая
        вкладку «Агенты») в панели балансера не существовало вовсе.

    Падает, если в Dockerfile нет маски страниц (`*.html`) или каталога
    (css/js/img/webfonts): перечислять файлы поимённо нельзя — именно поимённый
    список и разошёлся с реальным набором страниц.
    """
    if not dockerfile.exists():
        errors.append(f"{dockerfile}: файла нет — нечем проверить панель балансера")
        return 0

    text = dockerfile.read_text(encoding="utf-8", errors="replace")
    joined = "\n".join(ln.strip() for ln in text.splitlines() if ln.strip().startswith("COPY"))

    checked = 0
    if "/build/webui/*.html" in joined:
        checked += 1
    else:
        listed = re.findall(r"/build/webui/([\w.-]+\.html)\b", joined)
        present = sorted(p.name for p in root.glob("*.html"))
        not_covered = [p for p in present if p not in listed]
        if not_covered:
            errors.append(
                "docker/balancer/Dockerfile: страницы панели копируются поимённо, и без "
                f"{', '.join(not_covered)} — GET /index.html (дашборд с вкладкой «Агенты») "
                "вернёт 404. Нужна маска /build/webui/*.html")
        else:
            checked += 1

    for sub in ("css", "js", "img", "webfonts"):
        if re.search(rf"/build/webui/{sub}(\s|$)", joined):
            checked += 1
        else:
            errors.append(
                f"docker/balancer/Dockerfile: каталог webui/{sub} не копируется — "
                "панель балансера отдаст 404 на этих файлах (иконки/стили/скрипты)")
    return checked


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--root", default="webui", help="каталог статики WebUI")
    ap.add_argument("--balancer-dockerfile", default="docker/balancer/Dockerfile",
                    help="Dockerfile панели балансера (проверка состава статики)")
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

    panel_checked = check_balancer_panel_copy(Path(args.balancer_dockerfile), root, errors)
    fa_checked = check_fa_icon_classes(root, errors)

    # Шрифты Font Awesome должны быть локальными.
    #
    # R675 (2026-11-21): здесь же ловится мёртвый формат. Раньше missing_asset()
    # прощал отсутствие `.ttf` при наличии `.woff2` — и 10 несуществующих
    # `url(../webfonts/fa-*.ttf)` жили в CSS (см. её docstring). Теперь каждое
    # объявление обязано указывать на существующий файл.
    fa = root / "css" / "font-awesome.min.css"
    if fa.exists():
        for m in CSS_URL_RE.finditer(fa.read_text(encoding="utf-8", errors="replace")):
            raw_url = m.group(1).strip()
            # R-Image (2026-10-02): снимаем query (?v=R83) ДО проверки.
            # Без этого Path("....ttf?v=R83").suffix == ".ttf?v=R83" — не
            # шрифтовое расширение, и проверка «файл есть?» смотрела бы на
            # буквальный путь с query.
            url = strip_query(raw_url)
            if url.startswith("../webfonts/"):
                target = (fa.parent / url).resolve()
                if missing_asset(target):
                    errors.append(f"css/font-awesome.min.css: нет шрифта {raw_url}"
                                  f"{font_sibling_hint(target)}")

    # R675: форма @font-face — один src, один url() на семейство (см. docstring).
    font_face_checked = check_font_face_src(fa, root, errors)

    if errors:
        print("[assets] FAIL — внешние, отсутствующие или мёртвые ассеты:", file=sys.stderr)
        for e in errors:
            print(f"  * {e}", file=sys.stderr)
        return 1

    print(f"[assets] OK: {html_checked} локальных ассетов в HTML + "
          f"{css_checked} url() в CSS; внешних CDN нет, шрифты на месте "
          f"({font_face_checked} @font-face по одному url()); "
          f"иконок fa-*: {fa_checked} (все есть в CSS); "
          f"панель балансера: {panel_checked} групп(ы) статики (страницы/css/js/img/webfonts)")
    return 0


if __name__ == "__main__":
    sys.exit(main())

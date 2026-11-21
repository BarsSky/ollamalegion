#!/usr/bin/env python3
"""check_webui_i18n_keys.py — R493-D4: ключи data-i18n в index.html обязаны
существовать в пакетах переводов.

ЗАЧЕМ. На странице логов кнопка «Экспорт» имела data-i18n="logs.export", а ключа
logs.export не было ни в en.js, ни в ru.js. В русской локали оператор видел
ЛИТЕРАЛ `logs.export` вместо слова «Экспорт» — и никто не падал: i18n-слой честно
подставил то, что попросили, а проверка «EN/RU сбалансированы» этого не ловит,
потому что ключа не было в ОБОИХ файлах.

ЧТО ПРОВЕРЯЕТСЯ:
  1. каждый ключ из data-i18n (и data-i18n-title/-placeholder/...) есть в ru.js и en.js;
  2. формат `[title]some.key` — префикс в квадратных скобках означает «поставить
     атрибут», сам ключ — без префикса;
  3. значения непустые и не равны самому ключу (иначе подстановка бессмысленна);
  4. R675 (2026-11-21): ДИНАМИЧЕСКИЕ страницы. Для каждого `data-page="..."` в
     index.html требуется ключ `header.<page>` в обоих пакетах.

     ЗАЧЕМ. Заголовок H1 собирается В КОДЕ, а не в разметке:
     `var titleKey = 'header.' + page` (webui/js/app.js:241), поэтому ключа нет ни
     в одном data-i18n — и проверка 1 его не видит. Живой дефект R675: страница
     «Виртуальные модели» (`data-page="virtual-models"`) в шапке показывала СЫРОЙ
     ЛИТЕРАЛ `header.virtual-models` — ключа не было ни в ru.js, ни в en.js, и ни
     один чекер не падал. Статические ключи давно под защитой; динамические —
     нет, хотя ломаются ровно так же и так же заметно.

Запуск: python scripts/check_webui_i18n_keys.py [index.html ...]
Код возврата: 0 — всё найдено, 1 — есть пропуски, 2 — файл не разобран.
"""

import io
import os
import re
import sys

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), '..'))
DEFAULT_HTML = [os.path.join('webui', 'index.html')]
PACKS = {
    'ru': os.path.join(ROOT, 'webui', 'js', 'i18n', 'ru.js'),
    'en': os.path.join(ROOT, 'webui', 'js', 'i18n', 'en.js'),
}

ATTR = re.compile(r'data-i18n(?:-[a-z-]+)?="([^"]*)"')
BRACKET = re.compile(r'^\[[a-z-]+\](.+)$')
# R675: пункт меню → <a ... href="#..." data-page="models">. href на отдельный
# файл (health.html) означает, что SPA-перехода нет (app-listeners.js:97-102
# отдаёт клик браузеру), поэтому 'header.' + page для него не строится.
DATA_PAGE = re.compile(r'data-page="([A-Za-z0-9_-]+)"')
HREF = re.compile(r'href="([^"]*)"')


def load_pack(path):
    """Ключи пакета: `"key": "value"` — по всему файлу, без исполнения JS."""
    if not os.path.exists(path):
        sys.stderr.write('i18n package not found: %s\n' % path)
        raise SystemExit(2)
    src = io.open(path, encoding='utf-8').read()
    values = {}
    for m in re.finditer(r'"((?:[^"\\]|\\.)*)"\s*:\s*"((?:[^"\\]|\\.)*)"', src):
        values[m.group(1)] = m.group(2)
    if not values:
        sys.stderr.write('i18n package parsed to zero keys: %s\n' % path)
        raise SystemExit(2)
    return values


def html_keys(path):
    """Пары (ключ, файл:строка) для всех i18n-атрибутов страницы."""
    src = io.open(path, encoding='utf-8').read()
    out = []
    for lineno, line in enumerate(src.splitlines(), start=1):
        for m in ATTR.finditer(line):
            raw = m.group(1).strip()
            if not raw:
                continue
            bm = BRACKET.match(raw)
            key = bm.group(1).strip() if bm else raw
            out.append((key, '%s:%d' % (path, lineno)))
    return out


def dynamic_pages(path):
    """R675: страницы, чей H1 собирается В КОДЕ — 'header.' + page.

    Возвращает список (page, routed, где). routed=False — пункт меню ведёт на
    отдельный файл (href="health.html"), SPA-страницы для него нет, и заголовок
    через 'header.' + page не запрашивается: требовать для него перевод значило
    бы требовать ключ, который никогда не отобразится.
    """
    src = io.open(path, encoding='utf-8').read()
    out = []
    for lineno, line in enumerate(src.splitlines(), start=1):
        for m in DATA_PAGE.finditer(line):
            page = m.group(1)
            hm = HREF.search(line)
            href = hm.group(1).strip() if hm else ''
            routed = (not href) or href.startswith('#')
            out.append((page, routed, '%s:%d' % (path, lineno)))
    return out


def check_key(key, where, packs, problems):
    """Один ключ в каждом пакете: есть, непустой, не равен самому себе."""
    for lang, values in packs.items():
        if key not in values:
            problems.append('%s: ключ %r отсутствует в %s.js — оператор увидит литерал ключа'
                            % (where, key, lang))
        elif not values[key].strip():
            problems.append('%s: ключ %r пустой в %s.js' % (where, key, lang))
        elif values[key].strip() == key:
            problems.append('%s: ключ %r в %s.js переведён сам в себя' % (where, key, lang))


def main(argv):
    pages = argv[1:] or DEFAULT_HTML
    packs = {name: load_pack(path) for name, path in PACKS.items()}

    problems = []
    checked = 0
    for page in pages:
        page_path = page if os.path.isabs(page) else os.path.join(ROOT, page)
        if not os.path.exists(page_path):
            sys.stderr.write('page not found: %s\n' % page_path)
            return 2
        for key, where in html_keys(page_path):
            checked += 1
            check_key(key, where, packs, problems)

    # R675: динамические заголовки (см. docstring, пункт 4).
    dynamic_checked = 0
    dynamic_skipped = []
    for page in pages:
        page_path = page if os.path.isabs(page) else os.path.join(ROOT, page)
        if not os.path.exists(page_path):
            continue
        for name, routed, where in dynamic_pages(page_path):
            if not routed:
                dynamic_skipped.append('%s (%s)' % (name, where))
                continue
            dynamic_checked += 1
            check_key('header.' + name, where, packs, problems)

    # Защита от «зелени по пустоте»: если разметка перестанет разбираться (сменят
    # data-page, перепишут меню), проверка выше не найдут ни одной страницы и
    # молча пройдёт — ровно то, чего мы избегаем. Требуем непустой набор.
    if not dynamic_checked:
        problems.append('ни одной динамической страницы (data-page="...") не найдено в %s — '
                        'проверка заголовков header.<page> не выполнилась'
                        % ', '.join(pages))

    print('WebUI i18n: проверено атрибутов — %d, динамических страниц — %d, пакетов — %d'
          % (checked, dynamic_checked, len(packs)))
    if dynamic_skipped:
        print('отдельные файлы (SPA-заголовок не строится): %s' % ', '.join(dynamic_skipped))
    if problems:
        print('НАЙДЕНЫ ПРОПУСКИ (%d):' % len(problems))
        for p in problems:
            print('  - ' + p)
        return 1
    print('все ключи страниц присутствуют в ru.js и en.js')
    return 0


if __name__ == '__main__':
    sys.exit(main(sys.argv))

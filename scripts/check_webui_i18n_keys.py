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
  3. значения непустые и не равны самому ключу (иначе подстановка бессмысленна).

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
            for lang, values in packs.items():
                if key not in values:
                    problems.append('%s: ключ %r отсутствует в %s.js — оператор увидит литерал ключа'
                                    % (where, key, lang))
                elif not values[key].strip():
                    problems.append('%s: ключ %r пустой в %s.js' % (where, key, lang))
                elif values[key].strip() == key:
                    problems.append('%s: ключ %r в %s.js переведён сам в себя' % (where, key, lang))

    print('WebUI i18n: проверено атрибутов — %d, пакетов — %d' % (checked, len(packs)))
    if problems:
        print('НАЙДЕНЫ ПРОПУСКИ (%d):' % len(problems))
        for p in problems:
            print('  - ' + p)
        return 1
    print('все ключи страниц присутствуют в ru.js и en.js')
    return 0


if __name__ == '__main__':
    sys.exit(main(sys.argv))

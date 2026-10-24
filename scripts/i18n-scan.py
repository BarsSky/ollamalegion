#!/usr/bin/env python3
# i18n-scan.py - R234: сколько логов и ошибок ещё не вынесено в каталог i18n.
#
# ЗАЧЕМ. Полная локализация Go-сообщений делается постепенно: сначала каталог
# (internal/i18n/messages.go) и обвязка, потом перенос строк по файлам. Без
# счётчика легко объявить «всё готово», хотя в коде остались жёстко русские
# строки. Скрипт считает такие места и сверяет их с бюджетом — бюджет можно
# только уменьшать (ratchet), рост считается ошибкой.
#
# ЧТО СЧИТАЕТСЯ НАРУШЕНИЕМ:
#   строка Go-кода с кириллицей, которая передаётся в логгер, ошибку или пишется
#   в ответ как сообщение: log.*, logger.*, fmt.Errorf/errors.New, panic,
#   http.Error, writeJSON(... "error": "..."), а также литералы в аргументах
#   i18n.T/Logf (их быть не должно — там ключи, а не текст).
#
# ЧТО НЕ СЧИТАЕТСЯ: комментарии, строки внутри test-файлов (там допустимы
# русские фикстуры), каталог сообщений internal/i18n/messages.go и docs.
#
# Запуск:
#   python scripts/i18n-scan.py            # сводка + бюджет (код возврата: 0/1)
#   python scripts/i18n-scan.py --list     # по файлам, без бюджета
#   python scripts/i18n-scan.py --update   # перезаписать бюджет текущим числом
#
# Бюджет: scripts/i18n-budget.json {"max_untranslated": N, "note": "..."}.

import io
import json
import os
import re
import sys

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), '..'))
BUDGET = os.path.join(ROOT, 'scripts', 'i18n-budget.json')
CYR = re.compile(r'[А-Яа-яЁё]')
SKIP_DIRS = {'.git', 'node_modules', '_diag', 'dist', 'c'}
SKIP_FILES = {
    'internal/i18n/messages.go',
    'internal/i18n/i18n.go',
    'internal/i18n/bootstrap.go',
    'internal/i18n/remote.go',
    'internal/i18n/catalog.go',
}
# Места, где русский литерал — это сообщение (лог или ошибка).
CALL_PATTERNS = [
    re.compile(r'\blog\.(Print|Printf|Println|Fatal|Fatalf|Panic|Panicf)\s*\('),
    re.compile(r'\blogger\.(Debug|Info|Warn|Error|Fatal|Print)[a-zA-Z]*\s*\('),
    re.compile(r'\bs\.log(ger)?\.(Debug|Info|Warn|Error)[a-zA-Z]*\s*\('),
    re.compile(r'\bfmt\.Errorf\s*\('),
    re.compile(r'\berrors\.New\s*\('),
    re.compile(r'\bpanic\s*\('),
    re.compile(r'\bhttp\.Error\s*\('),
    re.compile(r'"error"\s*:'),
    re.compile(r'\bi18n\.(T|Logf|Tf)\s*\('),
]


def is_test(path):
    return path.endswith('_test.go')


def iter_go_files():
    for dirpath, dirnames, filenames in os.walk(ROOT):
        dirnames[:] = [d for d in dirnames if d not in SKIP_DIRS]
        for fn in filenames:
            if not fn.endswith('.go'):
                continue
            path = os.path.join(dirpath, fn)
            rel = os.path.relpath(path, ROOT).replace('\\', '/')
            if rel in SKIP_FILES or is_test(rel):
                continue
            yield rel, path


def scan_file(path):
    """Возвращает список (номер строки, фрагмент) с русским литералом в сообщении."""
    hits = []
    txt = io.open(path, encoding='utf-8', errors='replace').read()
    for idx, line in enumerate(txt.split('\n'), 1):
        stripped = line.strip()
        if not stripped or stripped.startswith('//'):
            continue
        if not CYR.search(line):
            continue
        # строковые литералы с кириллицей на этой строке
        if not re.search(r'"[^"\n]*[А-Яа-яЁё][^"\n]*"', line):
            continue
        if any(p.search(line) for p in CALL_PATTERNS):
            hits.append((idx, stripped[:120]))
    return hits


def main():
    files = 0
    total = 0
    per_file = []
    for rel, path in iter_go_files():
        hits = scan_file(path)
        if hits:
            files += 1
            total += len(hits)
            per_file.append((rel, hits))
    per_file.sort(key=lambda x: -len(x[1]))

    print('Go-файлов с русскими логами/ошибками вне каталога: %d' % files)
    print('Непереведённых мест: %d' % total)
    if '--list' in sys.argv:
        print('')
        for rel, hits in per_file:
            print('%-58s %3d' % (rel, len(hits)))
        return 0

    for rel, hits in per_file[:8]:
        print('  %-56s %3d  (напр. строка %d: %s)' % (rel, len(hits), hits[0][0], hits[0][1][:60]))

    if '--update' in sys.argv:
        io.open(BUDGET, 'w', encoding='utf-8', newline='\n').write(
            json.dumps({'max_untranslated': total,
                        'note': 'R234: бюджет непереведённых логов/ошибок, '
                                'можно только уменьшать'}, ensure_ascii=False, indent=1) + '\n')
        print('\nбюджет обновлён: %d' % total)
        return 0

    budget = None
    if os.path.exists(BUDGET):
        try:
            budget = json.load(io.open(BUDGET, encoding='utf-8')).get('max_untranslated')
        except Exception as e:
            print('не читается бюджет %s: %s' % (BUDGET, e))
    if budget is None:
        print('\nбюджет не задан — выполните: python scripts/i18n-scan.py --update')
        return 1
    print('\nбюджет: %d, фактически: %d' % (budget, total))
    if total > budget:
        print('ОШИБКА: непереведённых строк стало больше (было %d, стало %d). '
              'Переносите сообщения в internal/i18n/messages.go, а не в бюджет.'
              % (budget, total))
        return 1
    print('ОК: остаток не вырос')
    return 0


if __name__ == '__main__':
    sys.exit(main())

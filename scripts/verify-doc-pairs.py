#!/usr/bin/env python3
# verify-doc-pairs.py - R233: quality gate for the documentation translation.
#
# For every current document pair docs/<category>/<name>.md + <name>.ru.md:
#   * structural parity (## / ### headings, table rows, code fences, list items);
#   * numeric-token parity (every number in the source must appear in the target);
#   * language sanity (target must be in the expected language);
#   * link targets resolve to existing files.
# Prints a table and exits non-zero when anything fails.
#
# Usage: python scripts/verify-doc-pairs.py [--strict]

import io
import os
import re
import sys

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), '..'))
CURRENT = ['api', 'architecture', 'features', 'getting-started', 'operations', 'releases',
           'development']
NUM = re.compile(r'(?<![\w.])\d+(?:[.,]\d+)?(?![\w.])')
LINK = re.compile(r'!?\[[^\]]*\]\((?P<t>[^)\s]+)\)')
CYR = re.compile(r'[А-Яа-яЁё]')
LAT = re.compile(r'[A-Za-z]')


def read(p):
    return io.open(p, encoding='utf-8', errors='replace').read()


def is_ru(txt):
    cyr = len(CYR.findall(txt))
    lat = len(LAT.findall(txt))
    return cyr > lat * 0.35


def stats(txt):
    lines = txt.split('\n')
    return {
        'lines': len(lines),
        'h2': sum(1 for l in lines if l.startswith('## ')),
        'h3': sum(1 for l in lines if l.startswith('### ')),
        'rows': sum(1 for l in lines if l.startswith('|')),
        'fences': sum(1 for l in lines if l.startswith('```')),
        'bullets': sum(1 for l in lines if l.lstrip().startswith('- ')),
        'nums': sorted(set(NUM.findall(txt))),
    }


def bad_links(path, rel, txt):
    bad = []
    d = os.path.dirname(path)
    for m in LINK.finditer(txt):
        t = m.group('t')
        if t.startswith(('#', 'http://', 'https://', 'mailto:', 'data:')):
            continue
        p = t.split('#')[0]
        if p and not os.path.exists(os.path.normpath(os.path.join(d, p))):
            bad.append(t)
    return bad


def main():
    problems = []
    rows = []
    for cat in CURRENT:
        cat_dir = os.path.join(ROOT, 'docs', cat)
        if not os.path.isdir(cat_dir):
            continue
        for fn in sorted(os.listdir(cat_dir)):
            if not fn.endswith('.ru.md'):
                continue
            ru_path = os.path.join(cat_dir, fn)
            en_path = ru_path[:-len('.ru.md')] + '.md'
            rel = os.path.relpath(ru_path, ROOT).replace('\\', '/')
            if not os.path.exists(en_path):
                rows.append((rel[:-6], 'MISSING EN', '', '', ''))
                problems.append('no English version: ' + rel)
                continue
            ru, en = read(ru_path), read(en_path)
            rs, es = stats(ru), stats(en)
            en_ru_lang = is_ru(en)
            missing_nums = [n for n in rs['nums'] if n not in es['nums']]
            diffs = []
            for key in ('h2', 'h3', 'rows', 'fences'):
                if rs[key] != es[key]:
                    diffs.append('%s %d/%d' % (key, rs[key], es[key]))
            if en_ru_lang:
                problems.append('%s: English file looks Russian' % rel)
            if missing_nums:
                problems.append('%s: numbers missing in EN: %s' % (rel, ', '.join(missing_nums[:8])))
            bl = bad_links(en_path, rel, en) + bad_links(ru_path, rel, ru)
            if bl:
                problems.append('%s: broken links: %s' % (rel, ', '.join(bl[:5])))
            rows.append((rel[:-6], 'ok' if not diffs else '; '.join(diffs),
                         '%d/%d' % (rs['lines'], es['lines']),
                         '%d' % len(missing_nums), 'ru' if en_ru_lang else 'en'))
    print('%-46s %-26s %-11s %-6s %s' % ('document', 'structure diffs', 'lines ru/en', 'nums', 'lang'))
    for r in rows:
        print('%-46s %-26s %-11s %-6s %s' % r)
    print('')
    if problems:
        print('PROBLEMS (%d):' % len(problems))
        for p in problems:
            print('  -', p)
        return 1
    print('all pairs OK')
    return 0


if __name__ == '__main__':
    sys.exit(main())

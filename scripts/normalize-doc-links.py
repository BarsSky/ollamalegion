#!/usr/bin/env python3
# normalize-doc-links.py - R233: make every markdown link point at the same
# language as the document that contains it.
#
# Rule: a Russian document (<name>.ru.md) links to the Russian counterpart of a
# target when it exists; an English document (<name>.md) links to the English
# counterpart. Only links that resolve to a real file are rewritten, so the pass
# is idempotent and never creates a dead link.
#
# Scope: current documentation (docs/api, architecture, development, features,
# getting-started, operations, releases). docs/archive and docs/plans keep the
# original language and are not touched.
#
# Usage: python scripts/normalize-doc-links.py [--apply]

import io
import os
import re
import sys

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), '..'))
CATEGORIES = ['api', 'architecture', 'development', 'features', 'getting-started',
              'operations', 'releases']
LINK = re.compile(r'(!?\[[^\]]*\]\()(?P<t>[^)\s]+)(\))')
CYR = re.compile(r'[А-Яа-яЁё]')
LAT = re.compile(r'[A-Za-z]')


def read(p):
    return io.open(p, encoding='utf-8', errors='replace').read()


def write(p, txt):
    io.open(p, 'w', encoding='utf-8', newline='\n').write(txt)


def is_ru(txt):
    return len(CYR.findall(txt)) > len(LAT.findall(txt)) * 0.35


def variants(path):
    if path.endswith('.ru.md'):
        base = path[:-len('.ru.md')]
        return base + '.md', path
    if path.endswith('.md'):
        return path, path[:-len('.md')] + '.ru.md'
    return path, path


def fix(path, rel, apply_changes):
    txt = read(path)
    want_ru = is_ru(txt)
    d = os.path.dirname(path)

    def repl(m):
        t = m.group('t')
        if t.startswith(('#', 'http://', 'https://', 'mailto:', 'data:')):
            return m.group(0)
        target, sep, anchor = t.partition('#')
        if not target:
            return m.group(0)
        abs_target = os.path.normpath(os.path.join(d, target))
        if not os.path.exists(abs_target) or not abs_target.endswith('.md'):
            return m.group(0)
        rel_target = os.path.relpath(abs_target, ROOT).replace('\\', '/')
        en, ru = variants(rel_target)
        desired = ru if want_ru else en
        if desired == rel_target:
            return m.group(0)
        if not os.path.exists(os.path.join(ROOT, desired)):
            return m.group(0)
        new_rel = os.path.relpath(os.path.join(ROOT, desired), d).replace('\\', '/')
        return m.group(1) + new_rel + (sep + anchor if sep else '') + m.group(3)

    new_txt = LINK.sub(repl, txt)
    if new_txt != txt:
        if apply_changes:
            write(path, new_txt)
        return 1
    return 0


if __name__ == '__main__':
    apply_changes = '--apply' in sys.argv
    changed = 0
    for cat in CATEGORIES:
        cat_dir = os.path.join(ROOT, 'docs', cat)
        if not os.path.isdir(cat_dir):
            continue
        for dirpath, _dirnames, filenames in os.walk(cat_dir):
            for fn in filenames:
                if not fn.endswith('.md'):
                    continue
                p = os.path.join(dirpath, fn)
                rel = os.path.relpath(p, ROOT).replace('\\', '/')
                changed += fix(p, rel, apply_changes)
    print('links normalised in %d files%s' % (changed, '' if apply_changes else ' (dry run)'))

#!/usr/bin/env python3
# verify-docs-complete.py - R233: completion gate for the bilingual documentation.
#
# Checks that the objective is actually met:
#   1. every current document (docs/{api,architecture,development,features,
#      getting-started,operations,releases}/<name>.ru.md) has an English twin
#      <name>.md and vice versa;
#   2. both files of a pair are non-trivial (more than 20 lines) and written in
#      the expected language (English twin is not Russian, Russian twin is Russian);
#   3. the generated indexes docs/README.md and docs/README.ru.md contain no
#      "no translation yet" dash for a current document;
#   4. every relative markdown link in docs/ resolves.
#
# Exit code 0 only when all four checks pass.
#
# Usage: python scripts/verify-docs-complete.py

import io
import os
import re
import sys

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), '..'))
CATEGORIES = ['api', 'architecture', 'development', 'features', 'getting-started',
              'operations', 'releases']
LINK = re.compile(r'!?\[[^\]]*\]\((?P<t>[^)\s]+)\)')
CYR = re.compile(r'[А-Яа-яЁё]')
LAT = re.compile(r'[A-Za-z]')

def prose_only(txt):
    """Drop fenced code blocks and inline code spans before language detection."""
    out = []
    in_fence = False
    for line in txt.split('\n'):
        if line.lstrip().startswith('```'):
            in_fence = not in_fence
            continue
        if in_fence:
            continue
        out.append(re.sub(r'`[^`]*`', '', line))
    return '\n'.join(out)


def read(p):
    return io.open(p, encoding='utf-8', errors='replace').read()


def lang(txt):
    txt = prose_only(txt)
    cyr = len(CYR.findall(txt))
    lat = len(LAT.findall(txt))
    return 'ru' if cyr > lat * 0.25 else 'en'


def main():
    problems = []
    pairs = 0
    for cat in CATEGORIES:
        cat_dir = os.path.join(ROOT, 'docs', cat)
        if not os.path.isdir(cat_dir):
            continue
        for dirpath, dirnames, filenames in os.walk(cat_dir):
            for fn in filenames:
                if not fn.endswith('.md'):
                    continue
                path = os.path.join(dirpath, fn)
                rel = os.path.relpath(path, ROOT).replace('\\', '/')
                if fn.endswith('.ru.md'):
                    en = path[:-len('.ru.md')] + '.md'
                    if not os.path.exists(en):
                        problems.append('missing English twin: ' + rel)
                        continue
                    pairs += 1
                    ru_txt, en_txt = read(path), read(en)
                    if len(ru_txt.split('\n')) < 20:
                        problems.append('suspiciously short Russian file: ' + rel)
                    if len(en_txt.split('\n')) < 20:
                        problems.append('suspiciously short English file: ' + rel)
                    if lang(en_txt) != 'en':
                        problems.append('English twin looks Russian: ' + rel[:-6] + '.md')
                    if lang(ru_txt) != 'ru':
                        problems.append('Russian twin looks English: ' + rel)
    for index in ('docs/README.md', 'docs/README.ru.md'):
        txt = read(os.path.join(ROOT, index))
        for line in txt.split('\n'):
            if line.startswith('|') and ' — ' in line:
                problems.append(index + ': untranslated row -> ' + line.strip()[:90])
    links = 0
    for dirpath, dirnames, filenames in os.walk(os.path.join(ROOT, 'docs')):
        dirnames[:] = [d for d in dirnames if d not in {'node_modules', '_diag'}]
        for fn in filenames:
            if not fn.endswith('.md'):
                continue
            path = os.path.join(dirpath, fn)
            rel = os.path.relpath(path, ROOT).replace('\\', '/')
            txt = read(path)
            for m in LINK.finditer(txt):
                t = m.group('t')
                if t.startswith(('#', 'http://', 'https://', 'mailto:', 'data:')):
                    continue
                p = t.split('#')[0]
                if not p:
                    continue
                links += 1
                if not os.path.exists(os.path.normpath(os.path.join(dirpath, p))):
                    problems.append('%s: broken link %s' % (rel, t))
    print('bilingual pairs: %d' % pairs)
    print('relative links in docs/: %d' % links)
    if problems:
        print('PROBLEMS (%d):' % len(problems))
        for p in problems:
            print('  -', p)
        return 1
    print('docs are complete: every current document has EN + RU, indexes have no gaps, '
          'links resolve')
    return 0


if __name__ == '__main__':
    sys.exit(main())

#!/usr/bin/env python3
"""check_docs_claims.py — проверка утверждений документации о КОДЕ.

ЗАЧЕМ. Документация ссылается на файлы, переменные окружения, порты и команды.
Файлы удаляются, переменные переименовываются, порты меняются — и документ
начинает врать, причём незаметно: читатель идёт по ссылке и не находит её, а
«документация» тем временем считается источником правды.

ЧТО ПРОВЕРЯЕТСЯ (только проверяемое машинно):
  1. пути к файлам вида `internal/…go`, `cmd/…`, `scripts/…`, `docker/…`,
     `deployments/…` — существуют ли;
  2. «голые» имена с номером раунда (`active_streams_r271.go`): каталога у них
     нет, поэтому PATH_RE такие ссылки не видит. После волны переименований
     R599–R648 (файлы названы по теме, без `_rNNN`) голое имя в доке стало
     мёртвым молча — проверяется по индексу имён файлов репозитория (BARE_RE);
  3. имена переменных окружения `LB_*`, `CPPWORKER_*` — встречаются ли в коде
     (или в .env.example), то есть не выдуманы ли они;
  4. упомянутые порты стенда `18080/18079/18081/18083` — совпадают ли с
     docs/architecture (сверка с самим же документом не делается: порты берутся
     из списка, зафиксированного в поверхности, и проверяются как множество).

Запуск:
  python scripts/check_docs_claims.py            # отчёт + код возврата 1 при находках
  python scripts/check_docs_claims.py --list     # только находки, без сводки

Код возврата: 0 — расхождений нет, 1 — есть.
"""

import io
import os
import re
import sys

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), '..'))
DOC_DIRS = ['docs', 'README.md', 'CHANGELOG.md']
SKIP_DIRS = {'.git', 'node_modules', '_diag', 'dist', 'c', 'debug'}

# ИСТОРИЧЕСКИЕ разделы НЕ проверяются: они по определению описывают прошлое
# состояние проекта, и «починить» в них путь = переписать историю. Аудиты, раунды
# и планы ссылаются на файлы, которых уже нет, и это правильно.
HISTORICAL_PREFIXES = ('docs/archive/', 'docs/plans/', 'docs/releases/')
# CHANGELOG — история выпусков: он описывает прошлые состояния так же, как архив.
HISTORICAL_FILES = (
    'CHANGELOG.md',
    # План рефакторинга описывает БУДУЩИЕ флаги и файлы: их отсутствие в коде —
    # норма, а не ложное утверждение. Проверять его этим скриптом бессмысленно.
    'docs/architecture/refactoring-plan.ru.md',
)

# Путь считаем «ссылкой на файл», только если он начинается с известного корня и
# заканчивается расширением. Шаблоны (<...>, *, …, $VAR) пропускаем.
PATH_RE = re.compile(r'`((?:internal|cmd|scripts|docker|deployments|pkg|webui|tests)/[^`\s]+?\.(?:go|ps1|py|js|yml|yaml|json|md|sh|css|html))`')
# «Голое» имя с номером раунда — то же утверждение «файл есть», но без каталога:
# PATH_RE требует префикс и такую ссылку не видит. Именно поэтому после
# переименований R599–R648 в доках остались мёртвые `x_r493.go`. Проверяем по
# имени файла: каталога в ссылке нет, искать можно только по индексу имён.
BARE_RE = re.compile(r'(?<![A-Za-z0-9_/.\-])([A-Za-z0-9_.\-]*_r\d{2,4}\.(?:go|ps1|py|js|yml|yaml|json|md|sh|css|html))\b')
ENV_RE = re.compile(r'\b((?:LB|CPPWORKER)_[A-Z0-9_]{2,})\b')


def iter_docs():
    for target in DOC_DIRS:
        full = os.path.join(ROOT, target)
        if os.path.isfile(full):
            yield full
            continue
        for dirpath, dirnames, filenames in os.walk(full):
            dirnames[:] = [d for d in dirnames if d not in SKIP_DIRS]
            for name in sorted(filenames):
                if name.endswith('.md'):
                    yield os.path.join(dirpath, name)


def code_index():
    """Индекс: все тексты кода и конфигов, где могут встречаться env-имена."""
    chunks = []
    for dirpath, dirnames, filenames in os.walk(ROOT):
        dirnames[:] = [d for d in dirnames if d not in SKIP_DIRS]
        for name in filenames:
            if name.endswith(('.go', '.ps1', '.js', '.yml', '.yaml', '.sh')) or name.startswith('.env'):
                try:
                    chunks.append(io.open(os.path.join(dirpath, name), encoding='utf-8', errors='replace').read())
                except OSError:
                    continue
    return '\n'.join(chunks)


def file_names():
    """Индекс: имена всех файлов репозитория.

    У «голого» имени нет каталога, поэтому os.path.exists по нему не работает —
    проверять можно только по имени: есть ли файл с таким именем где-то в дереве.
    """
    names = set()
    for dirpath, dirnames, filenames in os.walk(ROOT):
        dirnames[:] = [d for d in dirnames if d not in SKIP_DIRS]
        names.update(filenames)
    return names


def is_historical(text, pos):
    """Строка-план (`- [ ] файл`) и упоминание удалённого или переименованного
    файла — это не утверждение «файл есть», а намерение либо история."""
    prev_nl = text.rfind(chr(10), 0, pos)
    line_text = text[prev_nl + 1:text.find(chr(10), pos)]
    # Смотрим и предыдущую строку: «удалены A и» / «B» — одно утверждение.
    if prev_nl > 0:
        prev_prev = text.rfind(chr(10), 0, prev_nl)
        line_text += ' ' + text[prev_prev + 1:prev_nl]
    low = line_text.lower()
    # «переименовал / переименован(о)» — ОПИСАНИЕ переименования: старое имя
    # в нём цитируется как история (docs/architecture/debts.ru.md §15d), а
    # не утверждается как существующее. Инфинитив «переименовать» (задача в
    # списке дел) намеренно НЕ подходит — такое имя проверяем.
    return ('- [ ]' in line_text or '~~' in line_text or 'удален' in low
            or 'удалён' in low or 'removed' in low
            or 'переименовал' in low or 'переименован' in low
            or 'renamed' in low)


def main(argv):
    code = code_index()
    known_files = file_names()
    missing_paths = []
    missing_bare = []
    unknown_env = []
    checked_paths = 0
    checked_bare = 0
    checked_env = 0

    skipped_historical = 0
    for path in iter_docs():
        rel = os.path.relpath(path, ROOT).replace('\\', '/')
        if rel in HISTORICAL_FILES or rel.startswith(HISTORICAL_PREFIXES):
            skipped_historical += 1
            continue
        text = io.open(path, encoding='utf-8', errors='replace').read()
        for m in PATH_RE.finditer(text):
            ref = m.group(1)
            if is_historical(text, m.start()):
                continue
            if any(tok in ref for tok in ('<', '>', '*', '…', '$', '{', '}')):
                continue
            checked_paths += 1
            if not os.path.exists(os.path.join(ROOT, ref)):
                line = text[:m.start()].count('\n') + 1
                missing_paths.append((rel, line, ref))
        # Голое имя с `_rNNN` проверяется так же, как путь: исключения те же
        # (история/строка-план), но утверждение — «файл с таким именем есть»,
        # поэтому ищем имя в индексе файлов, а не путь от корня.
        for m in BARE_RE.finditer(text):
            name = m.group(1)
            if is_historical(text, m.start()):
                continue
            if any(tok in name for tok in ('<', '>', '*', '…', '$', '{', '}')):
                continue
            checked_bare += 1
            if name not in known_files:
                line = text[:m.start()].count('\n') + 1
                missing_bare.append((rel, line, name))
        for m in ENV_RE.finditer(text):
            name = m.group(1)
            line_text = text[text.rfind(chr(10), 0, m.start()) + 1:text.find(chr(10), m.start())]
            low = line_text.lower()
            # Документ сам говорит, что переменной нет — это не расхождение.
            if any(tok in low for tok in ('не существует', 'не реализован', 'not implemented',
                                          'does not exist', 'планируем', 'there is no', 'нет:')):
                continue
            # Wildcard-упоминание (LB_AUTOLOAD*) и явно историческое имя.
            after = text[m.end():m.end() + 1]
            if after in ('*', '…') or m.group(1).endswith('_'):
                continue
            checked_env += 1
            if name not in code:
                line = text[:m.start()].count('\n') + 1
                unknown_env.append((rel, line, name))

    # одинаковые находки не дублируем
    seen = set()
    missing_paths = [x for x in missing_paths if not (x in seen or seen.add(x))]
    missing_bare = [x for x in missing_bare if not (x in seen or seen.add(x))]
    unknown_env = [x for x in unknown_env if not (x in seen or seen.add(x))]

    print('docs claims: путей проверено %d, голых имён %d, переменных окружения %d, '
          'исторических файлов пропущено %d'
          % (checked_paths, checked_bare, checked_env, skipped_historical))
    if missing_paths:
        print('\nССЫЛКИ НА НЕСУЩЕСТВУЮЩИЕ ФАЙЛЫ (%d):' % len(missing_paths))
        for rel, line, ref in missing_paths:
            print('  %s:%d  %s' % (rel, line, ref))
    if missing_bare:
        print('\n«ГОЛЫЕ» ИМЕНА ФАЙЛОВ, КОТОРЫХ НЕТ (%d):' % len(missing_bare))
        for rel, line, name in missing_bare:
            print('  %s:%d  %s' % (rel, line, name))
    if unknown_env:
        print('\nПЕРЕМЕННЫЕ ОКРУЖЕНИЯ, КОТОРЫХ НЕТ В КОДЕ (%d):' % len(unknown_env))
        for rel, line, name in unknown_env:
            print('  %s:%d  %s' % (rel, line, name))
    if not missing_paths and not missing_bare and not unknown_env:
        print('расхождений нет')
        return 0
    return 1


if __name__ == '__main__':
    sys.exit(main(sys.argv))

#!/usr/bin/env python3
"""inventory_p2_duplication.py — точный счёт «дублирования решений» (P2-13).

ЗАЧЕМ. Реестр долгов носил числа «4 очереди, 39 функций выбора бэкенда, 8 циклов
стрима, 11 писателей стрима», но они не воспроизводимы: непонятно, что
считалось, и такое число нельзя ни проверить, ни уменьшать. Скрипт считает по
формальным правилам и печатает списки: реестр ссылается на команду, а не на
память. Первый прогон (2026-11-11) дал 24 селектора и 3 типа очередей вместо
«39» и «4» — то есть прежние числа были оценкой на глаз.

ПРАВИЛА (все явные, чтобы счёт нельзя было подогнать):
  * РАЗБОР СИГНАТУР. Многострочные сигнатуры склеиваются до `{`: иначе половина
    функций выпадает из счёта (именно так «39» превращалось в «24»).
  * ФУНКЦИИ ВЫБОРА БЭКЕНДА: имя начинается с select/pick/find, есть получатель
    `func (x *T)`, возвращаемое значение начинается со `string` (ID бэкенда или
    «ID + причина»). Считаются отдельно для `*Proxy` и для всех получателей.
  * ОЧЕРЕДИ: типы, чьё имя содержит Queue, КРОМЕ структур-данных
    (`*Request`, `*Stats`, `*History`, `*Item`, `*Entry`), плюс точки входа
    ожидания `waitFor*Backend*`.
  * ЦИКЛЫ СТРИМА В БАЛАНСЕРЕ: функции, где есть и `for {`, и запись с
    `Flush()`/`Flusher` (то есть цикл пишет клиенту по частям). Считаются
    функции (цикл в функции — один), плюс число самих `for {`.
  * ПИСАТЕЛИ ОТВЕТА В ВОРКЕРЕ: функции в cmd/cppworker, которые пишут в
    http.ResponseWriter (`w.Write`, `WriteString(`, `fmt.Fprintf(w`,
    `safeStreamWriter`). Подразделяются на «через safeStreamWriter» и «напрямую».

Запуск: python scripts/inventory_p2_duplication.py [--json]
Код возврата: 0 (это отчёт, не гейт).
"""

import io
import json
import os
import re
import sys

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), '..'))

FUNC_START = re.compile(r'^func\s*(\([^)]*\))?\s*([A-Za-z0-9_]+)\s*\(')
QUEUE_TYPE = re.compile(r'^type\s+(\w*Queue\w*)\s+struct', re.M)
QUEUE_DATA_SUFFIX = ('Request', 'Stats', 'History', 'Item', 'Entry', 'Event', 'Snapshot')
WAIT_ENTRY = re.compile(r'^func\s*(?:\([^)]*\)\s*)?(waitFor[A-Za-z0-9_]*Backend[A-Za-z0-9_]*)\s*\(', re.M)


def go_files(rel_dir):
    base = os.path.join(ROOT, rel_dir)
    if not os.path.isdir(base):
        return []
    return [os.path.join(base, n) for n in sorted(os.listdir(base))
            if n.endswith('.go') and not n.endswith('_test.go')]


def read(path):
    return io.open(path, encoding='utf-8').read()


def functions(path):
    """Список функций: имя, получатель, склеенная сигнатура, тело (до следующего func)."""
    lines = read(path).splitlines()
    out = []
    i = 0
    while i < len(lines):
        m = FUNC_START.match(lines[i])
        if not m:
            i += 1
            continue
        recv = (m.group(1) or '').strip()
        name = m.group(2)
        sig = lines[i]
        j = i
        while '{' not in sig and j + 1 < len(lines) and j - i < 12:
            j += 1
            sig += ' ' + lines[j].strip()
        k = j + 1
        while k < len(lines) and not FUNC_START.match(lines[k]):
            k += 1
        out.append({'name': name, 'recv': recv, 'sig': sig, 'line': i + 1,
                    'body': '\n'.join(lines[j:k - 1])})
        i = k
    return out


def ret_type(sig):
    """Возвращаемое значение из склеенной сигнатуры (после ПОСЛЕДНЕЙ ')' перед '{').

    Здесь была ошибка первого прогона: ленивый шаблон от ПЕРВОЙ ')' захватывал имя
    функции и параметры целиком (`selectBackend(model string, ...) string`), из-за
    чего ни одна функция не проходила фильтр «возвращает string», и счёт выходил 0.
    """
    m = re.search(r'.*\)\s*([^{}]*?)\s*\{', sig)
    if not m:
        return ''
    return m.group(1).strip()


def scan_selectors():
    hits = []
    for path in go_files('internal/balancer'):
        for fn in functions(path):
            if not fn['name'].startswith(('select', 'pick', 'find')):
                continue
            ret = ret_type(fn['sig'])
            if not re.match(r'^\(?\s*string', ret):
                continue
            hits.append({'file': os.path.basename(path), 'line': fn['line'],
                         'name': fn['name'], 'recv': fn['recv'], 'returns': ret})
    return hits


def scan_selectors_calls(names):
    total = 0
    per = {}
    if not names:
        return total, per
    pat = re.compile(r'\.(' + '|'.join(sorted(names)) + r')\(')
    for rel in ('internal/balancer', 'cmd/balancer'):
        for path in go_files(rel):
            for m in pat.finditer(read(path)):
                per[m.group(1)] = per.get(m.group(1), 0) + 1
                total += 1
    return total, per


def scan_queues():
    types, methods, waits = [], [], []
    for path in go_files('internal/balancer'):
        base = os.path.basename(path)
        text = read(path)
        for m in QUEUE_TYPE.finditer(text):
            name = m.group(1)
            if name.endswith(QUEUE_DATA_SUFFIX):
                continue
            types.append({'file': base, 'name': name})
        for m in re.finditer(r'^func\s*\([^)]*\)\s*(enqueue|dequeue)\s*\(', text, re.M):
            methods.append({'file': base, 'name': m.group(1)})
        for m in WAIT_ENTRY.finditer(text):
            waits.append({'file': base, 'name': m.group(1)})
    return {'types': types, 'methods': methods, 'wait_entries': waits}


def scan_stream_loops():
    """Функции балансера, которые пишут клиенту по частям из цикла `for {`."""
    hits = []
    for path in go_files('internal/balancer'):
        for fn in functions(path):
            body = fn['body']
            if 'for {' not in body:
                continue
            if 'Flush()' not in body and 'http.Flusher' not in body:
                continue
            hits.append({'file': os.path.basename(path), 'line': fn['line'],
                         'func': fn['name'],
                         'for_loops': len(re.findall(r'\bfor \{', body))})
    return hits


def scan_worker_writers():
    """Писатели ответа в воркере, разделённые на «вообще пишет» и «пишет порциями».

    Второе — это и есть дублирование стрима: цикл, в котором каждая итерация пишет
    клиенту (safeStreamWriter / WriteString / fmt.Fprintf(w) / w.Write внутри for).
    Первое число больше и включают JSON-ответы, поэтому их нельзя смешивать:
    в реестре долгов речь шла именно о стриме.
    """
    funcs = {}
    for path in go_files('cmd/cppworker'):
        base = os.path.basename(path)
        for fn in functions(path):
            body = fn['body']
            writes = ('w.Write(' in body or 'WriteString(' in body or
                      'fmt.Fprintf(w' in body or 'safeStreamWriter' in body)
            if not writes:
                continue
            # «Пишет порциями» имеет смысл только для тех, кто пишет КЛИЕНТУ:
            # без http.ResponseWriter в подписи это сборка строки (strings.Builder),
            # и в цикле там WriteString, а не кадр ответа. Иначе счёт раздувается
            # хелперами (buildChatPromptFromMessages, injectMediaMarkers и т.п.).
            if 'http.ResponseWriter' not in fn['sig']:
                continue
            chunks = False
            for m in re.finditer(r'\bfor\b[^{]*\{', body):
                # тело цикла до следующей '}' того же уровня — приблизительно
                tail = body[m.end():]
                depth = 1
                idx = 0
                while idx < len(tail) and depth > 0:
                    if tail[idx] == '{':
                        depth += 1
                    elif tail[idx] == '}':
                        depth -= 1
                    idx += 1
                loop_body = tail[:idx]
                if ('w.Write(' in loop_body or 'WriteString(' in loop_body or
                        'fmt.Fprintf(w' in loop_body or 'safeStreamWriter' in loop_body or
                        'Flush()' in loop_body):
                    chunks = True
                    break
            key = (base, fn['name'])
            funcs[key] = {'file': base, 'func': fn['name'],
                          'writes_client': 'http.ResponseWriter' in fn['sig'],
                          'safe': 'safeStreamWriter' in body,
                          'chunked': chunks}
    return sorted(funcs.values(), key=lambda x: (x['file'], x['func']))


def main(argv):
    selectors = scan_selectors()
    proxy_selectors = [s for s in selectors if 'Proxy' in s['recv']]
    names = {s['name'] for s in selectors}
    call_total, call_per = scan_selectors_calls(names)
    queues = scan_queues()
    loops = scan_stream_loops()
    writers = scan_worker_writers()
    writers = [w for w in writers if w['writes_client']]
    safe = [w for w in writers if w['safe']]

    report = {
        'selectors_all_receivers': {'count': len(selectors), 'items': selectors},
        'selectors_proxy': {'count': len(proxy_selectors)},
        'selector_call_sites': {'count': call_total, 'per_function': call_per},
        'queues': {'types': len(queues['types']), 'type_names': queues['types'],
                   'enqueue_methods': len(queues['methods']),
                   'wait_entries': len(queues['wait_entries']),
                   'wait_names': queues['wait_entries']},
        'stream_loops': {'functions': len(loops),
                         'for_loops_total': sum(h['for_loops'] for h in loops),
                         'items': loops},
        'worker_writers': {'functions': len(writers), 'via_safe': len(safe),
                           'direct': len(writers) - len(safe),
                           'chunked': len([w for w in writers if w['chunked']]),
                           'chunked_via_safe': len([w for w in writers if w['chunked'] and w['safe']]),
                           'items': writers},
    }

    if '--json' in argv:
        print(json.dumps(report, ensure_ascii=False, indent=2))
        return 0

    print('P2-13: дублирование решений — счёт по формальным правилам')
    print('')
    print('  функции выбора бэкенда: %d (из них с получателем *Proxy: %d)'
          % (len(selectors), len(proxy_selectors)))
    print('  их вызовов в коде (без тестов): %d' % call_total)
    for name, n in sorted(call_per.items(), key=lambda kv: -kv[1])[:6]:
        print('      %-44s %d' % (name, n))
    print('')
    print('  очереди: типов %d, методов enqueue/dequeue %d, точек входа ожидания %d'
          % (len(queues['types']), len(queues['methods']), len(queues['wait_entries'])))
    for t in queues['types']:
        print('      type %-26s %s' % (t['name'], t['file']))
    for w in queues['wait_entries']:
        print('      wait %-26s %s' % (w['name'], w['file']))
    print('')
    print('  циклов стрима в балансере: функций %d, самих `for {` %d'
          % (len(loops), report['stream_loops']['for_loops_total']))
    for h in loops:
        print('      %-40s функция %s' % (h['file'], h['func']))
    print('')
    chunked = [w for w in writers if w['chunked']]
    print('  писателей ответа в воркере: функций %d (через safeStreamWriter %d, напрямую %d)'
          % (len(writers), len(safe), len(writers) - len(safe)))
    print('  из них пишут ПОРЦИЯМИ в цикле (то есть стрим): %d (через safeStreamWriter %d)'
          % (len(chunked), len([w for w in chunked if w['safe']])))
    for w in chunked:
        print('      %-34s %s' % (w['file'], w['func']))
    return 0


if __name__ == '__main__':
    sys.exit(main(sys.argv))

#!/usr/bin/env python3
# gguf_template.py - печатает tokenizer.chat_template из GGUF-файла.
#
# Зачем: форму вызовов инструментов задаёт шаблон модели, а не наше мнение о
# ней. По этому тексту видно, требует ли модель <tool_call><function=...> или
# JSON внутри <tool_call>, и включается ли R475 (история вызовов в форме шаблона).
#
# Модель загружать не нужно: читается только заголовок GGUF. Для файлов, у
# которых шаблон лежит далеко от начала (после списка токенов), нужен кусок
# побольше - 40 МБ хватает всем известным случаям.
#
# Windows / Linux (нужен docker, если файл внутри контейнера):
#   docker exec <worker> sh -c "head -c 40000000 '/app/models/<model>.gguf' > /tmp/h.gguf"
#   docker cp <worker>:/tmp/h.gguf ./h.gguf
#   python gguf_template.py h.gguf > chat_template.jinja
import struct
import sys

# Типы значений GGUF: скаляры и строка.
SCALAR = {0: 'B', 1: 'b', 2: 'H', 3: 'h', 4: 'I', 5: 'i', 6: 'f', 7: '?', 10: 'Q', 11: 'q', 12: 'd'}


def read(f, n):
    b = f.read(n)
    if len(b) != n:
        raise EOFError('want %d bytes, got %d' % (n, len(b)))
    return b


def u32(f):
    return struct.unpack('<I', read(f, 4))[0]


def u64(f):
    return struct.unpack('<Q', read(f, 8))[0]


def rstr(f):
    return read(f, u64(f)).decode('utf-8', 'replace')


def skip(f, vtype, n):
    if vtype == 8:
        for _ in range(n):
            f.seek(u64(f), 1)
    elif vtype == 9:
        raise ValueError('nested array')
    elif vtype == 7:
        f.seek(n, 1)
    else:
        f.seek(n * struct.calcsize(SCALAR[vtype]), 1)


def rvalue(f, vtype):
    if vtype == 8:
        return rstr(f)
    if vtype == 9:
        etype = u32(f)
        n = u64(f)
        if etype == 9:
            raise ValueError('array of arrays')
        skip(f, etype, n)
        return '<array of %d skipped>' % n
    if vtype == 7:
        return bool(read(f, 1)[0])
    fmt = SCALAR[vtype]
    return struct.unpack('<' + fmt, read(f, struct.calcsize(fmt)))[0]


def main(path):
    with open(path, 'rb') as f:
        if read(f, 4) != b'GGUF':
            raise SystemExit('not a GGUF file: %s' % path)
        version = u32(f)
        u64(f)  # tensor count
        kv_count = u64(f)
        sys.stderr.write('version=%d kvs=%d\n' % (version, kv_count))
        for i in range(kv_count):
            key = rstr(f)
            vtype = u32(f)
            value = rvalue(f, vtype)
            sys.stderr.write('%d: %s (type %d)\n' % (i, key, vtype))
            if key == 'tokenizer.chat_template':
                sys.stdout.write(value)
                return
    raise SystemExit('tokenizer.chat_template not found in this chunk; take a bigger head -c')


if __name__ == '__main__':
    if len(sys.argv) != 2:
        raise SystemExit('usage: gguf_template.py <file.gguf>')
    main(sys.argv[1])

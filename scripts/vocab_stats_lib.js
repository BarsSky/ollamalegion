// scripts/vocab_stats_lib.js — минимальный читатель GGUF-метаданных.
//
// Выделен отдельно, потому что используется двумя диагностическими скриптами:
//   - scripts/vocab_stats.js      — статистика словаря по скриптам;
//   - scripts/vocab_bpe_probe.js  — оценка реальной эффективности BPE на тексте.
//
// Формат: https://github.com/ggml-org/ggml/blob/master/docs/gguf.md
// Читаются ТОЛЬКО метаданные (kv-секция). Тензоры не читаются — для словаря
// они не нужны, поэтому файл можно разбирать даже для vocab-only GGUF.

'use strict';

const fs = require('fs');

/** Типы значений GGUF (см. gguf.md, enum gguf_type). */
const T = {
  UINT8: 0, INT8: 1, UINT16: 2, INT16: 3, UINT32: 4, INT32: 5,
  FLOAT32: 6, BOOL: 7, STRING: 8, ARRAY: 9, UINT64: 10, INT64: 11, FLOAT64: 12,
};

/**
 * Читает GGUF и возвращает { version, tensorCount, metadata: Map<string, value> }.
 * Бросает Error с понятным текстом, если файл не GGUF или обрезан.
 */
function readGguf(path) {
  const buf = fs.readFileSync(path);
  let off = 0;

  const need = (n) => {
    if (off + n > buf.length) {
      throw new Error(`файл обрезан: нужно ${n} байт по смещению ${off}, доступно ${buf.length - off}`);
    }
  };
  const u8 = () => { need(1); const v = buf.readUInt8(off); off += 1; return v; };
  const u16 = () => { need(2); const v = buf.readUInt16LE(off); off += 2; return v; };
  const u32 = () => { need(4); const v = buf.readUInt32LE(off); off += 4; return v; };
  const u64 = () => { need(8); const v = buf.readBigUInt64LE(off); off += 8; return v; };
  const i8 = () => { need(1); const v = buf.readInt8(off); off += 1; return v; };
  const i16 = () => { need(2); const v = buf.readInt16LE(off); off += 2; return v; };
  const i32 = () => { need(4); const v = buf.readInt32LE(off); off += 4; return v; };
  const i64 = () => { need(8); const v = buf.readBigInt64LE(off); off += 8; return v; };
  const f32 = () => { need(4); const v = buf.readFloatLE(off); off += 4; return v; };
  const f64 = () => { need(8); const v = buf.readDoubleLE(off); off += 8; return v; };
  const bool = () => u8() !== 0;
  const str = () => {
    const n = Number(u64());
    need(n);
    const s = buf.toString('utf8', off, off + n);
    off += n;
    return s;
  };

  const magic = buf.toString('ascii', 0, 4);
  off = 4;
  if (magic !== 'GGUF') {
    throw new Error(`не GGUF (magic=${JSON.stringify(magic)})`);
  }
  const version = u32();
  const tensorCount = Number(u64());
  const kvCount = Number(u64());

  const readValue = (type) => {
    switch (type) {
      case T.UINT8: return u8();
      case T.INT8: return i8();
      case T.UINT16: return u16();
      case T.INT16: return i16();
      case T.UINT32: return u32();
      case T.INT32: return i32();
      case T.FLOAT32: return f32();
      case T.BOOL: return bool();
      case T.STRING: return str();
      case T.UINT64: return u64();
      case T.INT64: return i64();
      case T.FLOAT64: return f64();
      case T.ARRAY: {
        const elemType = u32();
        const n = Number(u64());
        const out = new Array(n);
        for (let i = 0; i < n; i++) out[i] = readValue(elemType);
        return out;
      }
      default:
        throw new Error(`неизвестный тип значения GGUF: ${type} (смещение ${off})`);
    }
  };

  const metadata = new Map();
  for (let i = 0; i < kvCount; i++) {
    const key = str();
    const type = u32();
    metadata.set(key, readValue(type));
  }

  return { version, tensorCount, metadata };
}

module.exports = { readGguf, T };

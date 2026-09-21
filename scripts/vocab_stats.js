// scripts/vocab_stats.js — измерение статистики словаря GGUF по скриптам.
//
// ЗАЧЕМ. Фолбэк-эвристика подсчёта токенов (`len(runes)/4`) применяется, когда
// реальный токенизатор недоступен. Для латиницы ~4 символа/токен — норм, но
// для кириллицы и CJK это занижает счёт. Чтобы заменить константу 4 на
// обоснованные коэффициенты, нужно ЗНАТЬ, сколько байт в среднем приходится
// на токен в каждом скрипте. Этот скрипт читает реальный словарь из GGUF и
// считает эту статистику — без llama.cpp, без сборки, без модели.
//
// Запуск: node scripts/vocab_stats.js c/llama.cpp/models/ggml-vocab-qwen2.gguf
//
// Что считается: для каждого скрипта (latin/cyrillic/cjk/other) — число
// токенов, суммарная длина в байтах UTF-8 и в рунах, среднее число байт и рун
// на токен. Плюс «покрытие»: доля рун скрипта во всём словаре — она показывает,
// насколько словарь вообще приспособлен к этому скрипту (у чисто английской
// модели кириллических токенов почти нет, и текст будет резаться на байты).

'use strict';

const { readGguf } = require('./vocab_stats_lib.js');

// ------------------------------------------------------------- script classes

// Диапазоны Unicode, релевантные для токенизации.
function classify(cp) {
  // ASCII: печатные символы латиницы/цифры/пунктуация.
  if (cp < 0x80) {
    if ((cp >= 0x41 && cp <= 0x5a) || (cp >= 0x61 && cp <= 0x7a)) return 'latin';
    return 'ascii-other';
  }
  if (cp >= 0x00c0 && cp <= 0x024f) return 'latin'; // Latin-1 Supplement + Extended A/B
  if (cp >= 0x0370 && cp <= 0x03ff) return 'greek';
  if (cp >= 0x0400 && cp <= 0x04ff) return 'cyrillic'; // Cyrillic
  if (cp >= 0x0500 && cp <= 0x052f) return 'cyrillic'; // Cyrillic Supplement
  if (cp >= 0x2de0 && cp <= 0x2dff) return 'cyrillic'; // Cyrillic Extended-A
  if (cp >= 0xa640 && cp <= 0xa69f) return 'cyrillic'; // Cyrillic Extended-B
  if (cp >= 0x3040 && cp <= 0x30ff) return 'cjk'; // Hiragana + Katakana
  if (cp >= 0x3400 && cp <= 0x4dbf) return 'cjk'; // CJK Ext A
  if (cp >= 0x4e00 && cp <= 0x9fff) return 'cjk'; // CJK Unified
  if (cp >= 0xf900 && cp <= 0xfaff) return 'cjk'; // CJK Compat
  if (cp >= 0xac00 && cp <= 0xd7af) return 'cjk'; // Hangul syllables
  if (cp >= 0x20000 && cp <= 0x3ffff) return 'cjk'; // CJK Ext B+
  return 'other';
}

/** Классифицирует токен: берём скрипт большинства «буквенных» рун. */
function classifyToken(tok) {
  if (tok === '') return 'empty';
  const counts = new Map();
  for (const ch of tok) {
    const cls = classify(ch.codePointAt(0));
    if (cls === 'ascii-other') continue; // пробелы/пунктуация — не определяют скрипт
    counts.set(cls, (counts.get(cls) || 0) + 1);
  }
  if (counts.size === 0) return 'ascii-other';
  let best = null, bestN = -1;
  for (const [cls, n] of counts) {
    if (n > bestN) { best = cls; bestN = n; }
  }
  return best;
}

// --------------------------------------------------------------- measurement

function measure(path) {
  const { version, metadata } = readGguf(path);
  const tokens = metadata.get('tokenizer.ggml.tokens');
  if (!Array.isArray(tokens)) {
    throw new Error(`${path}: нет tokenizer.ggml.tokens (version=${version})`);
  }

  const stats = new Map(); // cls -> {tokens, bytes, runes}
  const bump = (cls, bytes, runes) => {
    let s = stats.get(cls);
    if (!s) { s = { tokens: 0, bytes: 0, runes: 0, minRunes: Infinity, maxRunes: 0 }; stats.set(cls, s); }
    s.tokens += 1;
    s.bytes += bytes;
    s.runes += runes;
    if (runes < s.minRunes) s.minRunes = runes;
    if (runes > s.maxRunes) s.maxRunes = runes;
  };

  for (const tok of tokens) {
    if (typeof tok !== 'string') continue;
    const runes = [...tok].length; // code points
    bump(classifyToken(tok), Buffer.byteLength(tok, 'utf8'), runes);
  }

  return {
    path,
    version,
    vocabSize: tokens.length,
    arch: metadata.get('general.architecture') || '?',
    stats,
  };
}

// -------------------------------------------------------------------- output

function fmt(n, d = 2) { return Number(n).toFixed(d); }

const paths = process.argv.slice(2);
if (paths.length === 0) {
  console.error('usage: node scripts/vocab_stats.js <vocab.gguf> [...]');
  process.exit(2);
}

const interesting = ['latin', 'cyrillic', 'cjk', 'greek', 'other'];

for (const p of paths) {
  let r;
  try {
    r = measure(p);
  } catch (e) {
    console.error(`!! ${p}: ${e.message}`);
    continue;
  }

  console.log(`\n=== ${p}`);
  console.log(`    GGUF v${r.version}  arch=${r.arch}  vocab=${r.vocabSize}`);

  // Суммарное число рун по всем скриптам — для «покрытия».
  let totalRunes = 0;
  for (const s of r.stats.values()) totalRunes += s.runes;

  console.log('    script      tokens   rune-share   avg-bytes/tok   avg-runes/tok');
  for (const cls of interesting) {
    const s = r.stats.get(cls);
    if (!s) continue;
    const share = totalRunes > 0 ? (100 * s.runes) / totalRunes : 0;
    console.log(
      `    ${cls.padEnd(11)} ${String(s.tokens).padStart(6)}   ${fmt(share, 2).padStart(8)}%   ` +
      `${fmt(s.bytes / s.tokens).padStart(12)}   ${fmt(s.runes / s.tokens).padStart(13)}`,
    );
  }

  // Ключевой вывод: сколько рун скрипта приходится на 1 токен, если считать
  // «идеальную упаковку» словарём. Это и есть коэффициент для фолбэка.
  const lat = r.stats.get('latin');
  const cyr = r.stats.get('cyrillic');
  if (lat) {
    console.log(`    → latin:   1 токен ≈ ${fmt(lat.runes / lat.tokens)} рун ` +
      `(${fmt(lat.bytes / lat.tokens)} байт)`);
  }
  if (cyr) {
    console.log(`    → cyrillic: 1 токен ≈ ${fmt(cyr.runes / cyr.tokens)} рун ` +
      `(${fmt(cyr.bytes / cyr.tokens)} байт)`);
  }
}

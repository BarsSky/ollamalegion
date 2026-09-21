// scripts/vocab_bpe_probe.js — оценка РЕАЛЬНОЙ эффективности BPE-словаря на тексте.
//
// ЗАЧЕМ. scripts/vocab_stats.js меряет среднюю длину токенов В СЛОВАРЕ. Это
// НЕ то же самое, что эффективность токенизации текста: BPE собирает текст из
// самых ДЛИННЫХ доступных merge-токенов, поэтому реальное число токенов на
// символ всегда ВЫШЕ, чем «средняя длина токена в словаре» (там лежит много
// односимвольных fallback-токенов).
//
// Этот скрипт воспроизводит жадный BPE поверх реального словаря из GGUF:
// на каждом шаге берётся самый длинный токен словаря, совпадающий с началом
// остатка строки. Это не побайтово точная копия llama.cpp (там merge-ранги, а
// не «самый длинный»), но оценка сверху на достижимую плотность и достаточная,
// чтобы понять порядок величин для фолбэк-эвристики.
//
// Запуск: node scripts/vocab_bpe_probe.js <vocab.gguf>
//
// ВАЖНО про границы применимости: результат зависит от того, что словарь
// содержит длинные токены нужного скрипта. Он НЕ учитывает, что модель
// обучена на этих токенах (это влияет на качество, не на длину).

'use strict';

const { readGguf } = require('./vocab_stats_lib.js');
const { decodeByteLevelToken, detectByteLevel } = require('./vocab_byte_decoder.js');

// ------------------------------------------------------------------ samples

// Образцы текста. Для каждой пары (скрипт, текст) считаем длину в рунах и
// число токенов при жадном разборе.
const SAMPLES = {
  latin:
    'The quick brown fox jumps over the lazy dog. ' +
    'Please summarize the following document and explain the main risks. ' +
    'function calculateTotal(items) { return items.reduce((a, b) => a + b.price, 0); }',
  cyrillic:
    'Быстрая коричневая лиса прыгает через ленивую собаку. ' +
    'Пожалуйста, перескажи следующий документ и объясни основные риски. ' +
    'Сегодня хорошая погода, и мы пошли гулять в парк после обеда.',
  cjk:
    '敏捷的棕色狐狸跳过了懒惰的狗。请总结以下文件并解释主要风险。' +
    '今天天气很好，午饭后我们去公园散步。',
  mixed:
    'Ошибка: connection refused при вызове API endpoint /v1/chat/completions. ' +
    'Проверьте timeout=30s и повторите запрос.',
};

// -------------------------------------------------------------- greedy BPE

/**
 * Жадный разбор строки по словарю: на каждой позиции — самый длинный
 * совпадающий токен. Возвращает число токенов.
 *
 * tokensByFirst — Map<первый-символ, массив токенов, отсортированный по
 * убыванию длины>. Так мы не сканируем все 150k токенов на каждый шаг.
 */
function greedyTokenize(text, tokensByFirst) {
  let pos = 0;
  let n = 0;
  const runes = [...text];
  while (pos < runes.length) {
    const candidates = tokensByFirst.get(runes[pos]);
    let matched = null;
    if (candidates) {
      for (const tok of candidates) {
        const tokRunes = [...tok];
        if (tokRunes.length > runes.length - pos) continue;
        let ok = true;
        for (let i = 0; i < tokRunes.length; i++) {
          if (runes[pos + i] !== tokRunes[i]) { ok = false; break; }
        }
        if (ok) { matched = tokRunes.length; break; }
      }
    }
    if (matched === null) matched = 1; // байтовый fallback (в llama.cpp — <0xXX>)
    pos += matched;
    n += 1;
  }
  return n;
}

function buildIndex(tokens) {
  const byFirst = new Map();
  let decoded = 0;
  let skipped = 0;
  for (const raw of tokens) {
    if (typeof raw !== 'string' || raw === '') continue;
    const { text } = decodeByteLevelToken(raw);
    // Токены, которые не декодируются в валидный UTF-8 (одиночные байты
    // многобайтовых последовательностей), в текстовом разборе бесполезны —
    // llama.cpp собирает из них символ по частям, мы это не воспроизводим.
    // Пропускаем: так оценка остаётся консервативной (не занижает).
    if (text === '' || text.includes('\uFFFD')) { skipped++; continue; }
    const first = [...text][0];
    let arr = byFirst.get(first);
    if (!arr) { arr = []; byFirst.set(first, arr); }
    arr.push(text);
    decoded++;
  }
  // Длинные раньше — чтобы первое совпадение было самым длинным.
  for (const arr of byFirst.values()) {
    arr.sort((a, b) => [...b].length - [...a].length);
  }
  return { byFirst, decoded, skipped };
}

// -------------------------------------------------------------------- main

function r3(n) { return Math.round(n * 1000) / 1000; }

const paths = process.argv.slice(2);
if (paths.length === 0) {
  console.error('usage: node scripts/vocab_bpe_probe.js <vocab.gguf> [...]');
  process.exit(2);
}

for (const p of paths) {
  let meta;
  try {
    meta = readGguf(p).metadata;
  } catch (e) {
    console.error(`!! ${p}: ${e.message}`);
    continue;
  }
  const tokens = meta.get('tokenizer.ggml.tokens');
  if (!Array.isArray(tokens)) {
    console.error(`!! ${p}: нет tokenizer.ggml.tokens`);
    continue;
  }

  const detection = detectByteLevel(tokens);
  const { byFirst, decoded, skipped } = buildIndex(tokens);
  console.log(`\n=== ${p}`);
  console.log(`    vocab=${tokens.length} arch=${meta.get('general.architecture') || '?'} ` +
    `byte-level=${detection.byteLevel} (${(detection.ratio * 100).toFixed(1)}%) ` +
    `decoded=${decoded} skipped=${skipped}`);
  console.log('    script     runes   tokens   runes/token   tok/rune   vs len(runes)/4');

  for (const [name, text] of Object.entries(SAMPLES)) {
    const runes = [...text].length;
    const n = greedyTokenize(text, byFirst);
    const perToken = runes / n;
    const perRune = n / runes;
    const heuristic = Math.floor(runes / 4);
    const err = heuristic > 0 ? ((n - heuristic) / heuristic) * 100 : 0;
    console.log(
      `    ${name.padEnd(9)} ${String(runes).padStart(6)} ${String(n).padStart(8)} ` +
      `${r3(perToken).toString().padStart(13)} ${r3(perRune).toString().padStart(10)} ` +
      `${(err >= 0 ? '+' : '') + err.toFixed(0)}%`,
    );
  }
}

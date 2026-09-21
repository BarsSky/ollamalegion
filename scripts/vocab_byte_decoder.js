// scripts/vocab_byte_decoder.js — GPT-2 byte-level ↔ unicode маппинг.
//
// ЗАЧЕМ. Byte-level BPE словари (GPT-2, Qwen2, Llama-BPE, DeepSeek, Command-R)
// хранят токены НЕ как обычный текст, а через обратимый маппинг «байт →
// unicode-символ»: каждый из 256 возможных байтов отображается в печатный
// unicode-символ, чтобы словарь оставался валидным UTF-8. Из-за этого:
//
//   - пробел хранится как 'Ġ' (U+0120), а не как ' ';
//   - кириллица хранится как последовательность символов вида 'Ð', '°' и т.п.
//     (это «переодетые» байты UTF-8), а не как 'А';
//   - прямой поиск токена 'Привет' в таком словаре даёт НОЛЬ совпадений, даже
//     если словарь прекрасно умеет кириллицу.
//
// Без этого декодера любой анализ словаря (включая подсчёт эффективности)
// молча получает мусор: сравнение идёт по «переодетым» строкам, совпадений нет,
// и результат вырождается в «один токен на символ», что выглядит как реальное
// измерение, но им не является. Именно на этом сначала сломался
// scripts/vocab_bpe_probe.js.
//
// Маппинг — из эталонной реализации GPT-2 (encoder.py, bytes_to_unicode):
// печатные диапазоны остаются собой, остальные байты сдвигаются в 256+.

'use strict';

/** Возвращает массив из 256 символов: байт (индекс) → unicode-символ. */
function buildByteToChar() {
  const bs = [];
  // Печатные диапазоны, которые остаются «как есть».
  for (let i = 0x21; i <= 0x7e; i++) bs.push(i);          // '!'..'~'
  for (let i = 0xa1; i <= 0xac; i++) bs.push(i);          // '¡'..'¬'
  for (let i = 0xae; i <= 0xff; i++) bs.push(i);          // '®'..'ÿ'

  const inBs = new Set(bs);
  const cs = bs.slice();
  // Остальные байты сдвигаются в 0x100+ — так словарь остаётся валидным UTF-8.
  // Пробел (0x20) → U+0120 'Ġ', перевод строки (0x0a) → U+010A 'Ċ' и т.д.
  let n = 0;
  for (let b = 0; b < 256; b++) {
    if (!inBs.has(b)) {
      bs.push(b);
      cs.push(256 + n);
      n++;
    }
  }

  const map = new Array(256);
  for (let i = 0; i < bs.length; i++) {
    // ВАЖНО: именно String.fromCodePoint — в cs лежат ЧИСЛА (кодпоинты),
    // а не строки. Первая версия клала в map число, из-за чего весь маппинг
    // молча не работал (CHAR_TO_BYTE оказывался пустым).
    map[bs[i]] = String.fromCodePoint(cs[i]);
  }
  return map;
}

const BYTE_TO_CHAR = buildByteToChar();

/** char → byte (обратный маппинг). */
const CHAR_TO_BYTE = new Map();
for (let b = 0; b < 256; b++) CHAR_TO_BYTE.set(BYTE_TO_CHAR[b], b);

/**
 * Декодирует токен в реальную строку, АВТОМАТИЧЕСКИ выбирая схему.
 *
 * Проблема: по одному токену нельзя надёжно понять, byte-level словарь или нет.
 * У Qwen2 токен 'Ġhello' — это byte-level ('Ġ'=пробел), а токен 'Привет' может
 * лежать как настоящая кириллица. Первая версия этой функции делала вывод по
 * первому же «непонятному» символу и для кириллических токенов Qwen2 решала,
 * что словарь не byte-level — после чего кириллица перестала находиться.
 *
 * Правильный критерий — по РЕЗУЛЬТАТУ: пробуем снять byte-level кодировку.
 * Если получилась валидная строка без replacement-символов, считаем попытку
 * успешной. Для ASCII и для обычного UTF-8-текста byte-level декодирование
 * даёт ровно ту же строку (печатные байты отображаются в себя), поэтому
 * «попробовать» безопасно: ложных срабатываний не даёт.
 */
function decodeByteLevelToken(tok) {
  const bytes = [];
  for (const ch of tok) {
    const cp = ch.codePointAt(0);
    if (cp < 0x100 && CHAR_TO_BYTE.has(ch)) {
      bytes.push(CHAR_TO_BYTE.get(ch));
    } else {
      // Символ вне маппинга — дописываем его UTF-8 байты как есть. Это
      // покрывает словари, где часть токенов лежит в «настоящем» виде.
      for (const b of Buffer.from(ch, 'utf8')) bytes.push(b);
    }
  }
  const buf = Buffer.from(bytes);
  const text = buf.toString('utf8');
  if (text.includes('\uFFFD')) {
    // Не сошлось — токен уже был обычным текстом.
    return { text: tok, bytes: Buffer.from(tok, 'utf8'), byteLevel: false };
  }
  return { text, bytes: buf, byteLevel: true };
}

/**
 * Определяет, является ли словарь byte-level, по доле декодируемых токенов.
 * Возвращает { byteLevel: boolean, ratio: number }.
 */
function detectByteLevel(tokens) {
  let ok = 0;
  let checked = 0;
  for (const tok of tokens) {
    if (typeof tok !== 'string' || tok === '') continue;
    checked++;
    if (checked > 5000) break;
    let all = true;
    for (const ch of tok) {
      if (!(ch.codePointAt(0) < 0x100 && CHAR_TO_BYTE.has(ch))) { all = false; break; }
    }
    if (all) ok++;
  }
  const ratio = checked > 0 ? ok / checked : 0;
  return { byteLevel: ratio > 0.9, ratio };
}

module.exports = { BYTE_TO_CHAR, CHAR_TO_BYTE, decodeByteLevelToken, detectByteLevel };

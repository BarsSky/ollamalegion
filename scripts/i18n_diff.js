#!/usr/bin/env node
/**
 * i18n_diff.js — сравнивает два i18n-файла OllamaLegion (en.js / ru.js)
 * и выводит:
 *   - ключи, которые есть только в первом файле (отсутствуют во втором)
 *   - ключи, которые есть только во втором файле
 *   - ключи с пустым значением
 *   - общую статистику: всего ключей, уникальных, общих
 *
 * Использование:
 *   node scripts/i18n_diff.js webui/js/i18n/en.js webui/js/i18n/ru.js
 *   node scripts/i18n_diff.js --missing-in ru en       # ключи, которых нет в ru
 *
 * Код возврата:
 *   0 — расхождений нет
 *   1 — есть расхождения (если --strict)
 *   2 — ошибка (файл не найден / битый)
 */

'use strict';

const fs = require('fs');
const path = require('path');

function parseI18nFile(filePath) {
  if (!fs.existsSync(filePath)) {
    throw new Error(`Файл не найден: ${filePath}`);
  }
  const src = fs.readFileSync(filePath, 'utf8');

  // Извлекаем первый `window.I18N_XX = { ... };` блок
  // Используем ленивый Function-конструктор вместо eval
  const match = src.match(/window\.I18N_[A-Z]+\s*=\s*({[\s\S]*?})\s*;?\s*$/m);
  if (!match) {
    throw new Error(`Не удалось распарсить window.I18N_* в файле ${filePath}`);
  }

  let dict;
  try {
    // eslint-disable-next-line no-new-func
    dict = (new Function(`return (${match[1]});`))();
  } catch (e) {
    throw new Error(`Синтаксическая ошибка в ${filePath}: ${e.message}`);
  }

  return dict;
}

function findEmptyValues(dict) {
  const empty = [];
  for (const [k, v] of Object.entries(dict)) {
    if (v === null || v === undefined || (typeof v === 'string' && v.trim() === '')) {
      empty.push(k);
    }
  }
  return empty.sort();
}

function diffKeys(setA, setB) {
  const onlyInA = [];
  const onlyInB = [];
  const inBoth = [];
  for (const k of setA) {
    if (setB.has(k)) inBoth.push(k);
    else onlyInA.push(k);
  }
  for (const k of setB) {
    if (!setA.has(k)) onlyInB.push(k);
  }
  return {
    onlyInA: onlyInA.sort(),
    onlyInB: onlyInB.sort(),
    inBoth: inBoth.sort(),
  };
}

function formatValue(dict, key) {
  const v = dict[key];
  if (typeof v !== 'string') return JSON.stringify(v);
  if (v.length > 80) return JSON.stringify(v.slice(0, 77) + '...');
  return JSON.stringify(v);
}

function main() {
  const argv = process.argv.slice(2);
  if (argv.length < 2) {
    console.error('Использование: node scripts/i18n_diff.js <en.js> <ru.js> [--strict] [--missing-in <base|target>]');
    console.error('  --strict: exit code 1 при любых расхождениях');
    console.error('  --missing-in <base|target>: показать ключи, которых нет в указанном файле');
    process.exit(2);
  }

  const enPath = path.resolve(argv[0]);
  const ruPath = path.resolve(argv[1]);
  const strict = argv.includes('--strict');

  let missingInFilter = null;
  const missingIdx = argv.indexOf('--missing-in');
  if (missingIdx !== -1 && argv[missingIdx + 1]) {
    const which = argv[missingIdx + 1].toLowerCase();
    if (which === 'ru' || which === 'target') missingInFilter = 'target';
    else if (which === 'en' || which === 'base') missingInFilter = 'base';
    else {
      console.error(`--missing-in: ожидалось en|ru|base|target, получено ${which}`);
      process.exit(2);
    }
  }

  let enDict, ruDict;
  try {
    enDict = parseI18nFile(enPath);
    ruDict = parseI18nFile(ruPath);
  } catch (e) {
    console.error(`ОШИБКА: ${e.message}`);
    process.exit(2);
  }

  const enKeys = new Set(Object.keys(enDict));
  const ruKeys = new Set(Object.keys(ruDict));
  const allKeys = new Set([...enKeys, ...ruKeys]);

  const { onlyInA, onlyInB, inBoth } = diffKeys(enKeys, ruKeys);
  const enEmpty = findEmptyValues(enDict);
  const ruEmpty = findEmptyValues(ruDict);

  console.log('=== i18n_diff summary ===');
  console.log(`  EN: ${enKeys.size} ключей (${enPath})`);
  console.log(`  RU: ${ruKeys.size} ключей (${ruPath})`);
  console.log(`  Общих: ${inBoth.length}`);
  console.log(`  Только в EN: ${onlyInA.length}`);
  console.log(`  Только в RU: ${onlyInB.length}`);
  console.log(`  Всего уникальных: ${allKeys.size}`);
  console.log(`  Пустых значений в EN: ${enEmpty.length}`);
  console.log(`  Пустых значений в RU: ${ruEmpty.length}`);

  if (missingInFilter) {
    // Ключи, которые есть в "другом" файле, но отсутствуют в "указанном"
    const missing = missingInFilter === 'target' ? onlyInB : onlyInA;
    const sourceDict = missingInFilter === 'target' ? enDict : ruDict;
    const targetName = missingInFilter === 'target' ? 'RU' : 'EN';
    console.log(`\n=== Ключи, отсутствующие в ${targetName} (${missing.length}) ===`);
    if (missing.length === 0) {
      console.log('  (нет)');
    } else {
      for (const k of missing) {
        console.log(`  ${k} = ${formatValue(sourceDict, k)}`);
      }
    }
  } else {
    if (onlyInA.length > 0) {
      console.log(`\n=== Только в EN (${onlyInA.length}) ===`);
      for (const k of onlyInA) {
        console.log(`  ${k} = ${formatValue(enDict, k)}`);
      }
    }
    if (onlyInB.length > 0) {
      console.log(`\n=== Только в RU (${onlyInB.length}) ===`);
      for (const k of onlyInB) {
        console.log(`  ${k} = ${formatValue(ruDict, k)}`);
      }
    }
    if (enEmpty.length > 0) {
      console.log(`\n=== Пустые значения в EN (${enEmpty.length}) ===`);
      for (const k of enEmpty) console.log(`  ${k}`);
    }
    if (ruEmpty.length > 0) {
      console.log(`\n=== Пустые значения в RU (${ruEmpty.length}) ===`);
      for (const k of ruEmpty) console.log(`  ${k}`);
    }
  }

  const diffs = onlyInA.length + onlyInB.length + enEmpty.length + ruEmpty.length;
  if (strict && diffs > 0) process.exit(1);
  if (diffs === 0) process.exit(0);
  process.exit(0);
}

main();
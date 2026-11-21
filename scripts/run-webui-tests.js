#!/usr/bin/env node
/**
 * run-webui-tests.js — ОДНА команда, запускающая ВСЕ тесты WebUI.
 *
 * ЗАЧЕМ ОНА ПОЯВИЛАСЬ (R493, 2026-11-17).
 *
 * Тестов WebUI в репозитории 102 файла, а запускалось 28: список файлов жил
 * руками в `package.json → scripts.test:webui` и от него отстал. Проверено
 * пересчётом: в списке 28 путей, на диске 102 файла `*.test.js` — 74 набора
 * не запускались НИКОГДА, ни локально, ни в CI. Среди них ровно те, которые
 * писались под жалобу «кнопки обновить не работают и их несколько»:
 *
 *   - `webui/js/modules/data-refresh.test.js` — 13 проверок самого механизма
 *     обновления (реестр провайдеров, индикатор свежести, пауза/авто);
 *   - `webui/tests/no-dead-refresh-buttons.test.js` — статическая проверка
 *     «нет мёртвых кнопок обновления»;
 *   - `webui/js/modules/gguf-refresh-resilience.test.js` и ещё 71 набор.
 *
 * CI при этом не запускал `npm run test:webui` вообще: в `.github/workflows`
 * не было ни одного упоминания (`grep -c 'test:webui' .github/workflows/*.yml`
 * = 0). Отсюда и «тесты есть, а дефект живой»: набор, который не запускают,
 * не отличается от отсутствующего.
 *
 * ЧТО ДЕЛАЕТ: обходит `webui/` и запускает КАЖДЫЙ файл `*.test.js` отдельным
 * процессом node (как это делали руками). Список больше не ведётся руками, чтобы
 * новый тест нельзя было «забыть добавить»: единственное правило — имя файла.
 *
 * R675 (2026-11-21): тем же правилом запускаются и СТАТИЧЕСКИЕ чекеры. Отдельного
 * списка «ещё вот это» здесь сознательно нет: чекер живёт самостоятельным скриптом
 * (`scripts/webui_ctx_contract_check.js` — контракт фабрик `window.X = function
 * (ctx)`), а в набор его вводит тонкая обёртка `webui/tests/ctx-contract-r675.test.js`.
 * Плюсы именно такой связки: (1) чекер можно запустить одной командой в CI и
 * локально, без npm install; (2) раннер остаётся с одним правилом — иначе рядом с
 * «всеми *.test.js» появляется второй список, который отстаёт (это уже случалось:
 * до R493 в package.json висел ручной список из 28 наборов против 102 на диске);
 * (3) обёртка проверяет не только «зелено», но и что правило СРАБАТЫВАЕТ на
 * испорченной копии — чекер, который всегда возвращает 0, выглядит зелёным.
 *
 * ЧЕМ ОТЛИЧАЕТСЯ ОТ `node --test`. Наборы WebUI написаны как самодостаточные
 * скрипты (require модуля, свой `check()`, `process.exit(1)` при ошибке), а не в
 * формате node:test. Поэтому их надо запускать по одному процессу на файл:
 * глобальные объекты (`window`, `document`, `localStorage`) каждый набор
 * создаёт себе сам и не должен делить с соседом.
 *
 * ЗАПУСК:
 *   node scripts/run-webui-tests.js                 # все наборы
 *   node scripts/run-webui-tests.js --filter=refresh  # только совпадающие по пути
 *   node scripts/run-webui-tests.js --quiet          # только итог и падения
 *
 * Код возврата: 0 — все зелёные, 1 — есть падения (так его вызывает CI).
 */
'use strict';

const fs = require('fs');
const path = require('path');
const { spawnSync } = require('child_process');

const ROOT = path.join(__dirname, '..');
const WEBUI = path.join(ROOT, 'webui');

// Каталоги, в которых тестов быть не может: node_modules (зависимости) и
// webfonts (шрифты Font Awesome — там .js нет, но обход не должен туда ходить).
const SKIP_DIRS = new Set(['node_modules', 'webfonts', 'img']);

// Один набор не должен висеть вечно: 120 с — с запасом больше любого текущего
// (самый долгий ui-renderer-smoke.test.js ~4 с). Значение нужно, чтобы CI
// падал с внятной строкой, а не висел до timeout-minutes джобы.
const TIMEOUT_MS = 120000;

const args = process.argv.slice(2);
const filterArg = args.find((a) => a.startsWith('--filter='));
const filter = filterArg ? filterArg.slice('--filter='.length) : '';
const quiet = args.includes('--quiet');

function discover(dir, out) {
    for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
        const full = path.join(dir, entry.name);
        if (entry.isDirectory()) {
            if (SKIP_DIRS.has(entry.name)) continue;
            discover(full, out);
        } else if (entry.name.endsWith('.test.js')) {
            out.push(full);
        }
    }
    return out;
}

const files = discover(WEBUI, [])
    .map((f) => path.relative(ROOT, f).split(path.sep).join('/'))
    .filter((rel) => (filter ? rel.includes(filter) : true))
    .sort();

if (files.length === 0) {
    console.error('run-webui-tests: не найдено ни одного *.test.js' + (filter ? ' по фильтру ' + filter : ''));
    process.exit(1);
}

const failures = [];
let passed = 0;

for (const rel of files) {
    const res = spawnSync(process.execPath, [path.join(ROOT, rel)], {
        cwd: ROOT,
        encoding: 'utf8',
        timeout: TIMEOUT_MS,
    });
    const ok = res.status === 0;
    if (ok) {
        passed++;
        if (!quiet) console.log('  \u2713 ' + rel);
        continue;
    }
    const out = ((res.stdout || '') + (res.stderr || '')).trim();
    const tail = out.split(/\r?\n/).slice(-12).join('\n');
    failures.push({ rel, tail, status: res.status, signal: res.signal });
    console.log('  \u2717 ' + rel);
}

console.log('\nНаборов: ' + files.length + ', зелёных: ' + passed + ', красных: ' + failures.length);
if (failures.length) {
    console.log('\nПадения:');
    for (const f of failures) {
        const how = f.signal ? 'сигнал ' + f.signal : 'код ' + f.status;
        console.log('\n--- ' + f.rel + ' (' + how + ')');
        if (f.tail) console.log(f.tail);
    }
    process.exit(1);
}
console.log('ИТОГ: все наборы WebUI зелёные');

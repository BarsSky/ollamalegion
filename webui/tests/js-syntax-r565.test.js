// js-syntax-r565.test.js — R565: каждый боевой JS-модуль WebUI обязан РАЗБИРАТЬСЯ.
//
// ЗАЧЕМ ЭТОТ СТРАЖ (найдено живой ошибкой в R564). Я удалял мёртвые функции режима из
// `settings-ui.js` по эвристике «до первой строки `}`» и срезал функцию по внутренней
// скобке — файл стал синтаксически невалидным (`updateVariantSections(mode);` осталась
// висеть после обрезанного `showModeFields`). При этом `node scripts/run-webui-tests.js`
// показал **120/120 зелёных**: ни один набор не загружает `settings-ui.js` исполнением,
// его только читают текстом (`read()` + regex). То есть «все наборы зелёные» НЕ значит
// «файлы валидны», и сломанный модуль уехал бы в образ.
//
// ЧТО ДЕЛАЕТ. Для каждого `webui/js/**/*.js` (кроме самих тестов) — попытка
// скомпилировать источник: `new Function(source)`. Это ловит обрезанные скобки, висящие
// выражения, случайные обрывки — но НЕ ловит логические ошибки (для них есть наборы).
//
// ЧЕГО НЕ ПРОВЕРЯЕТ. Файлы в формате ES-модулей (`import`/`export`) через `new Function`
// не компилируются — они пропускаются с пометкой, а не «проходят молча».
//
// Запуск: node webui/tests/js-syntax-r565.test.js
'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');

const root = path.join(__dirname, '..');

let passed = 0;
const failures = [];
function check(name, fn) {
    try {
        fn();
        passed++;
        console.log('  \u2713 ' + name);
    } catch (e) {
        failures.push(name + ': ' + (e && e.message));
        console.error('  \u2717 ' + name + ' — ' + (e && e.message));
    }
}

// Собираем все боевые .js под webui/js (тесты исключаем: у них свой запуск).
function collect(dir, out) {
    for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
        const full = path.join(dir, entry.name);
        if (entry.isDirectory()) {
            if (entry.name === 'node_modules') continue;
            collect(full, out);
            continue;
        }
        if (!entry.name.endsWith('.js')) continue;
        if (entry.name.endsWith('.test.js')) continue;
        out.push(full);
    }
    return out;
}

const files = collect(path.join(root, 'js'), []);
const skipped = [];
const broken = [];

check('модули WebUI найдены', function () {
    assert.ok(files.length > 20, 'ожидалось больше 20 модулей, найдено ' + files.length);
});

for (const file of files) {
    const rel = path.relative(root, file).replace(/\\/g, '/');
    const src = fs.readFileSync(file, 'utf8');
    if (/^\s*(import|export)\s/m.test(src)) {
        skipped.push(rel);
        continue;
    }
    try {
        // Компиляция без исполнения: тело функции не вызывается, побочных эффектов нет.
        // eslint-disable-next-line no-new-func
        new Function(src);
    } catch (e) {
        broken.push(rel + ': ' + e.message);
    }
}

check('каждый боевой модуль разбирается как JS', function () {
    assert.deepStrictEqual(broken, [],
        'синтаксически сломанные модули (наборы WebUI их не загружают исполнением, поэтому ' +
        'такой файл проходит тесты и уезжает в образ): ' + broken.join('; '));
});

check('ES-модулей в webui/js нет (иначе они не проверяются этим стражем)', function () {
    assert.deepStrictEqual(skipped, [],
        'найдены ES-модули, не покрытые проверкой синтаксиса: ' + skipped.join(', '));
});

console.log('\n' + passed + ' проверок зелёные, ' + failures.length + ' красных (файлов проверено: ' + files.length + ')');
if (failures.length > 0) {
    failures.forEach(function (f) { console.error('  FAIL: ' + f); });
    process.exit(1);
}

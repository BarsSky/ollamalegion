// selection-ui-r493.test.js — R493 (долг 12n): в WebUI не должно остаться
// «выбора алгоритма», которого нет.
//
// ЖИВОЙ ДЕФЕКТ, который здесь закрыт. В Настройках был select с пятью
// вариантами: `least-connections`, `round-robin`, `weighted` сервер отвергал с
// 400 («Invalid algorithm. Valid: roundrobin, leastconn, resource-aware,
// model-affinity»), а два принимаемых имени выбрать было нельзя. Сам выбор при
// этом ничего не переключал. Теперь поля нет ни в конфиге, ни в API, ни в UI —
// вместо него показывается ФАКТ (unified-multistage).
//
// Проверка статическая: страницу без браузера не обойти, а вот «остался select
// или обработчик» видно. Запуск: node webui/tests/selection-ui-r493.test.js
'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');

const root = path.join(__dirname, '..');
const read = (rel) => fs.readFileSync(path.join(root, rel), 'utf8');

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

const html = read('index.html');
const jsFiles = [
    'js/app.js',
    'js/modules/config-io.js',
    'js/modules/setup-wizard.js',
    'js/modules/demo-mode.js',
];

check('в разметке нет select «Алгоритм балансировки»', function () {
    assert.strictEqual(html.indexOf('id="balancingAlgorithm"'), -1,
        'select #balancingAlgorithm вернулся: он предлагал варианты, которые сервер отвергал');
    // «Красивые» значения, которые сервер отвергал. round-robin НЕ проверяем:
    // такое значение есть у другого, живого поля — #gpuStrategy (раскладка
    // llama.cpp), и запрещать его нельзя.
    ['least-connections', 'weighted'].forEach(function (v) {
        assert.strictEqual(html.indexOf('value="' + v + '"'), -1,
            'в разметке снова есть value="' + v + '" — значение, которого нет в API');
    });
});

check('вместо выбора показан факт: единый многоступенчатый селектор', function () {
    assert.ok(html.indexOf('id="selectionAlgorithmValue"') !== -1,
        'нет блока с фактическим способом выбора');
    assert.ok(html.indexOf('unified-multistage') !== -1,
        'на странице не назван реальный селектор');
});

check('JS не читает и не отправляет algorithm', function () {
    for (const rel of jsFiles) {
        const src = read(rel);
        // Именно КОД, а не упоминание в комментарии (в setup-wizard.js id
        // остался в историческом комментарии про дубли id).
        const needles = [
            "getElementById('balancingAlgorithm')",
            "getFieldValue('balancingAlgorithm'",
            "setFieldValue('balancingAlgorithm'",
            'id="wiz-balancingAlgorithm"',
        ];
        needles.forEach(function (needle) {
            assert.strictEqual(src.indexOf(needle), -1,
                rel + ': остался обработчик/чтение ' + needle);
        });
        // Поле в теле PUT/экспорте: ищем только как свойство объекта.
        assert.ok(!/^\s*algorithm:\s*/m.test(src),
            rel + ': в теле запроса/экспорта снова есть поле algorithm');
    }
});

check('i18n: новые ключи есть в обоих пакетах, старых обещаний нет', function () {
    const ru = read('js/i18n/ru.js');
    const en = read('js/i18n/en.js');
    ['settings.selection_title', 'settings.selection_single', 'settings.selection_hint'].forEach(function (k) {
        assert.ok(ru.indexOf('"' + k + '"') !== -1, 'ru.js: нет ключа ' + k);
        assert.ok(en.indexOf('"' + k + '"') !== -1, 'en.js: нет ключа ' + k);
    });
    ['settings.balancing_mode', 'settings.balancing_least_conn', 'wizard.tooltip.balancing_algorithm'].forEach(function (k) {
        assert.strictEqual(ru.indexOf('"' + k + '"'), -1, 'ru.js: остался ключ ' + k + ' (обещает несуществующий выбор)');
        assert.strictEqual(en.indexOf('"' + k + '"'), -1, 'en.js: остался ключ ' + k);
    });
});

check('демо-режим не притворяется, что отдаёт алгоритм', function () {
    const demo = read('js/modules/demo-mode.js');
    assert.ok(!/^\s*algorithm:\s*/m.test(demo), 'demo-mode.js: мок снова отдаёт поле algorithm');
});

console.log('\n' + (failures.length ? 'FAILED: ' + failures.length : 'ИТОГ: все проверки пройдены (' + passed + ')'));
if (failures.length) {
    failures.forEach(function (f) { console.error(' - ' + f); });
    process.exit(1);
}

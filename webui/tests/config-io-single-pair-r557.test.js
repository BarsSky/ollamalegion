// config-io-single-pair-r557.test.js — R557 (W4 плана
// 2026-11-19-webui-pages-and-setup, §6): «одна пара Export/Import».
//
// ЖИВОЙ ДЕФЕКТ. В «Настройках» было ДВЕ пары кнопок импорта/экспорта: одна вверху
// страницы (#exportSettingsBtn / #importSettingsBtn), вторая — в отдельной секции
// «Импорт / Экспорт» (#exportSettingsBtn2 / #importSettingsBtn2). Обе делали одно и
// то же, разница была только в позиции: оператор видел два одинаковых действия и не
// понимал, чем они отличаются.
//
// ЧТО СТАЛО: секция «Импорт / Экспорт» удалена целиком, осталась одна пара (верхняя,
// канонические id). Обработчики в app.js — тоже по одному: копия обработчика без
// кнопки была мёртвым кодом.
//
// Проверка статическая. Запуск: node webui/tests/config-io-single-pair-r557.test.js
'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');

const root = path.join(__dirname, '..');
const html = fs.readFileSync(path.join(root, 'index.html'), 'utf8');
const appJs = fs.readFileSync(path.join(root, 'js/app.js'), 'utf8');

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

function countOccurrences(haystack, needle) {
    let n = 0;
    let i = 0;
    while ((i = haystack.indexOf(needle, i)) !== -1) { n++; i += needle.length; }
    return n;
}

check('в разметке ровно одна кнопка экспорта и одна импорта', function () {
    assert.strictEqual(countOccurrences(html, 'id="exportSettingsBtn"'), 1,
        'кнопок экспорта должно быть ровно одна');
    assert.strictEqual(countOccurrences(html, 'id="importSettingsBtn"'), 1,
        'кнопок импорта должно быть ровно одна');
});

check('дублирующих кнопок (*2) больше нет', function () {
    ['exportSettingsBtn2', 'importSettingsBtn2'].forEach(function (id) {
        assert.strictEqual(html.indexOf(id), -1,
            'кнопка #' + id + ' вернулась: это вторая пара того же действия — оператор не понимает, чем они отличаются');
    });
});

check('секции «Импорт / Экспорт» больше нет', function () {
    assert.strictEqual(html.indexOf('id="section-io"'), -1,
        'секция #section-io вернулась: она дублировала верхнюю пару кнопок');
});

check('в app.js по одному обработчику на действие', function () {
    assert.strictEqual(countOccurrences(appJs, "getElementById('exportSettingsBtn')"), 1,
        'обработчиков экспорта должно быть ровно один');
    assert.strictEqual(countOccurrences(appJs, "getElementById('importSettingsBtn')"), 1,
        'обработчиков импорта должно быть ровно один');
    ['exportSettingsBtn2', 'importSettingsBtn2'].forEach(function (id) {
        assert.strictEqual(appJs.indexOf(id), -1,
            'в app.js остался обработчик #' + id + ' — мёртвый код без кнопки');
    });
});

check('сбалансированные скобки вокруг обработчиков (не осталось хвостов)', function () {
    // Дешёвая проверка «после удаления блока не осталось висящих });»: считаем
    // баланс круглых скобок в файле — он обязан быть нулевым.
    let depth = 0;
    for (const ch of appJs) {
        if (ch === '(') depth++;
        else if (ch === ')') depth--;
        if (depth < 0) break;
    }
    assert.strictEqual(depth, 0, 'баланс скобок в app.js нарушен — при удалении блока остался лишний фрагмент');
});

console.log('\n' + passed + ' проверок зелёные, ' + failures.length + ' красных');
if (failures.length > 0) {
    failures.forEach(function (f) { console.error('  FAIL: ' + f); });
    process.exit(1);
}

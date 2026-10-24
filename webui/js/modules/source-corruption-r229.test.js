// source-corruption-r229.test.js — R229 (2026-10-22).
//
// Запуск: node webui/js/modules/source-corruption-r229.test.js
//
// ЖИВОЙ ДЕФЕКТ, который фиксирует тест. Правка двух строк PowerShell'ом
// (`-replace` с шаблоном, где `` `n `` попал в строку как ДВА СИМВОЛА: обратный
// апостроф + «n») склеила комментарий со следующей строкой кода:
//
//   // R228: скоуп — только выбранный бэкенд...`n                let url = ...
//
// Всё после «//» — комментарий, поэтому `let url` и `if (backendID)` ПРОПАЛИ из
// кода: применение профиля из WebUI падало с ReferenceError (url is not defined),
// а «применить и перезагрузить» молча не вызывало applyProfileWithProgress.
// Тест ловит именно этот класс порчи: литерал «`n» с отступом в 4+ пробела.
//
// Проверяются ВСЕ .js в webui/js (включая модули, которые не покрыты другими
// тестами): склейка комментария с кодом — это молчаливая потеря функциональности,
// её не видно ни компилятору, ни браузеру.

'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');

const BT = String.fromCharCode(96); // обратный апостроф
// Шаблон порчи: «`n» + отступ 4+ пробелов (в коде так не пишут; в строке — только
// артефакт замены).
const BAD = new RegExp(BT + 'n\\s{4,}');

const ROOT = path.join(__dirname, '..'); // webui/js

function walk(dir) {
    const out = [];
    let entries;
    try {
        entries = fs.readdirSync(dir, { withFileTypes: true });
    } catch (e) {
        return out;
    }
    for (const e of entries) {
        const full = path.join(dir, e.name);
        if (e.isDirectory()) out.push.apply(out, walk(full));
        // Себя не проверяем: в тесте есть ПРИМЕР артефакта в комментарии.
        else if (e.isFile() && e.name.endsWith('.js') && e.name !== 'source-corruption-r229.test.js') {
            out.push(full);
        }
    }
    return out;
}

const files = walk(ROOT);
assert.ok(files.length > 50, 'ожидалось много JS-файлов WebUI, найдено ' + files.length);

const bad = [];
for (const f of files) {
    const text = fs.readFileSync(f, 'utf8');
    text.split(/\r?\n/).forEach(function (line, i) {
        if (BAD.test(line)) bad.push(path.relative(ROOT, f) + ':' + (i + 1) + ': ' + line.trim().slice(0, 160));
    });
}

if (bad.length) {
    console.error('Артефакт «`n» (склейка комментария и кода) найден в ' + bad.length + ' месте(ах):');
    bad.forEach(function (b) { console.error('  ' + b); });
    process.exit(1);
}

console.log('Проверено файлов: ' + files.length + ', артефактов склейки: 0');

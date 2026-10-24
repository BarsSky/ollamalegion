// browser-compat-r136.test.js — R136 (2026-10-16).
//
// Запуск: node webui/js/modules/browser-compat-r136.test.js
//
// ЖИВОЙ ДЕФЕКТ. В консоли страницы WebUI (проверено через Playwright на живом
// стенде) сыпалось `ReferenceError: global is not defined`:
//
//   pageerror: global is not defined
//   [monitor] render placementPolicy failed: ReferenceError: global is not defined
//       at render (js/modules/monitor-groups.js:178)
//
// `global` — это Node-изм: в браузере его нет, поэтому ЦЕЛИКОМ падала отрисовка
// окон «Объединение бэкендов» (backend-merge.js), «План раскладки» (setup-plan.js)
// и карточки групп размещения (monitor-groups.js). В Node-тестах `global` есть,
// поэтому тесты этого класса молчали, а в браузере модуль не работал.
//
// Тест — СТАТИЧЕСКИЙ, потому что воспроизвести «браузер без global» из Node нельзя
// (в Node он всегда есть). Он падает, если в браузерном коде снова появится
// обращение к `global.`: правильная форма — `globalThis.`, она есть и в браузере,
// и в Node.
'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');

const ROOTS = [
    path.join(__dirname),                 // webui/js/modules
    path.join(__dirname, '..'),           // webui/js
    path.join(__dirname, '..', 'monitor') // webui/js/monitor
];

function collect(dir, out) {
    for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
        const p = path.join(dir, entry.name);
        if (entry.isDirectory()) { collect(p, out); continue; }
        if (!entry.name.endsWith('.js')) continue;
        if (entry.name.endsWith('.test.js')) continue;   // тесты живут в Node и global имеют право
        out.push(p);
    }
    return out;
}

let checks = 0;
function check(name, fn) { fn(); checks++; console.log('  ✓ ' + name); }

check('R136: в браузерном коде нет обращений к global. (Node-изм)', function () {
    const files = [];
    ROOTS.forEach(r => { if (fs.existsSync(r)) collect(r, files); });
    const offenders = [];
    for (const f of files) {
        const src = fs.readFileSync(f, 'utf8');
        const m = src.match(/(?<![\w.$])global\./g);
        if (m && m.length) offenders.push(`${path.relative(path.join(__dirname, '..', '..'), f)} (${m.length})`);
    }
    assert.deepStrictEqual(offenders, [],
        'обращение к global. в браузере роняет модуль целиком (ReferenceError); используйте globalThis.: ' +
        offenders.join(', '));
});

check('R136: модули, которые падали, теперь используют globalThis', function () {
    ['backend-merge.js', 'monitor-groups.js', 'setup-plan.js'].forEach(function (name) {
        const src = fs.readFileSync(path.join(__dirname, name), 'utf8');
        assert.ok(src.indexOf('globalThis.') !== -1, name + ': нет ни одного globalThis.');
        assert.strictEqual(src.match(/(?<![\w.$])global\./g), null, name + ': осталось обращение к global.');
    });
});

console.log('\nbrowser-compat-r136: ' + checks + ' проверок пройдено');

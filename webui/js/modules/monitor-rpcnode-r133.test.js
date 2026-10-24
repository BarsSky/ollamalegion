// monitor-rpcnode-r133.test.js — R133 (2026-10-16).
//
// Запуск: node webui/js/modules/monitor-rpcnode-r133.test.js
//
// ЧТО ФИКСИРУЕТ ТЕСТ. Роль бэкенда в панели — это ПРИКАЗ. До R133 в строке не
// было видно исполнения: поднялся ли у донора rpc-server, слушается ли порт, и
// доступен ли он С БАЛАНСЕРА (порт мог не опубликоваться при запуске
// контейнера). «Выбрал донора — ничего не произошло» было неотличимо от
// «работает». Плюс отдельная подпись для источника вклада узлов (R132):
// «0 МБ узлов» и «узлы не измерялись» снаружи выглядели одинаково.
//
// Проверяется:
//   1. донор, узел поднят и доступен → зелёная подпись с портом;
//   2. донор, узел поднят, но недоступен с балансера → оранжевая подпись и
//      причина в заголовке (это разные состояния, и лечится по-разному);
//   3. донор, узел не поднят → красная подпись и причина;
//   4. НЕ донор с поднятым узлом → «вручную»: это факт, а не требование балансера;
//   5. нет данных rpcNode (старый балансер) → пусто, без выдумывания статуса;
//   6. источник вклада узлов: none / cache / probed — три разные подписи.

'use strict';

const assert = require('assert');

global.window = global;
global.MonitorApp = {
    esc: function (s) { return String(s === null || s === undefined ? '' : s); },
    fmtDur: function () { return '-'; }
};
global.I18N = {
    t: function (key) {
        const map = {
            'monitor.common.noData': 'Нет данных',
            'monitor.rpcnode.up': 'узел раскладки поднят',
            'monitor.rpcnode.unreachable': 'узел раскладки недоступен с балансера',
            'monitor.rpcnode.down': 'узел раскладки не поднят',
            'monitor.rpcnode.manual': 'узел раскладки поднят вручную',
            'monitor.layout.source_none': 'узлы раскладки не измерялись',
            'monitor.layout.source_cache': 'узлы раскладки — из последнего замера',
            'monitor.layout.source_probed': 'узлы раскладки измерены'
        };
        return Object.prototype.hasOwnProperty.call(map, key) ? map[key] : key;
    }
};

function makeEl(id) {
    return {
        id: id, innerHTML: '', textContent: '', value: '', style: {}, dataset: {},
        classList: { add() {}, remove() {}, contains() { return false; } },
        addEventListener() {}, querySelectorAll() { return []; },
        setAttribute() {}, getAttribute() { return null; }, appendChild() {}
    };
}
const stubTbody = { innerHTML: '', querySelectorAll: function () { return []; } };
global.document = {
    querySelector: function (sel) { return sel === '#backendsTable tbody' ? stubTbody : null; },
    getElementById: function () { return null; },
    querySelectorAll: function () { return []; },
    createElement: function () { return makeEl(''); },
    addEventListener: function () {}
};
global.Utils = { showToast: function () {}, escapeHtml: function (s) { return String(s); } };

const fs = require('fs');
const path = require('path');
const NodeModule = require('module');
const SRC = path.join(__dirname, 'monitor-backends.js');
const m = new NodeModule(SRC, module);
m._compile(fs.readFileSync(SRC, 'utf8') + '\nmodule.exports = MonitorBackends;', SRC);
const MB = m.exports;
global.window.MonitorBackends = MB;

let checks = 0;
function check(name, fn) { fn(); checks++; console.log('  ✓ ' + name); }

function backend(over) {
    return Object.assign({ id: 'cppworker-cyborg-bundled-agent', type: 'llama_cpp', status: 'healthy' }, over || {});
}

check('R133: донор с поднятым и доступным узлом — зелёная подпись с портом', function () {
    const html = MB.rpcNodeBadge(backend({
        role: 'donor',
        rpcNode: { role: 'donor', wanted: true, enabled: true, running: true, listening: true, reachable: true, port: 50052 }
    }));
    assert.ok(/узел раскладки поднят/.test(html), 'нет подписи «поднят»: ' + html);
    assert.ok(/:50052/.test(html), 'нет порта узла: ' + html);
});

check('R133: донор, узел поднят, но недоступен с балансера — отдельная подпись и причина', function () {
    const html = MB.rpcNodeBadge(backend({
        role: 'donor',
        rpcNode: {
            role: 'donor', wanted: true, enabled: true, running: true, listening: true, reachable: false, port: 50052,
            reason: 'узел раскладки поднят на 192.0.2.10:50052, но с балансера недоступен (проверьте публикацию порта)'
        }
    }));
    assert.ok(/недоступен с балансера/.test(html), 'нет подписи о недоступности: ' + html);
    assert.ok(/публикацию порта/.test(html), 'причина не показана в заголовке: ' + html);
});

check('R133: донор, узел не поднят — красная подпись и причина', function () {
    const html = MB.rpcNodeBadge(backend({
        role: 'donor',
        rpcNode: { role: 'donor', wanted: true, enabled: true, running: false, listening: false, reachable: false, reason: 'не удалось поднять узел раскладки: connection refused' }
    }));
    assert.ok(/не поднят/.test(html), 'нет подписи «не поднят»: ' + html);
    assert.ok(/connection refused/.test(html), 'причина отказа не показана: ' + html);
});

check('R133: не донор с поднятым узлом — «вручную», это факт, а не требование', function () {
    const html = MB.rpcNodeBadge(backend({
        role: 'standalone',
        rpcNode: { role: 'standalone', wanted: false, enabled: true, running: true, listening: true, reachable: true, port: 50052 }
    }));
    assert.ok(/вручную/.test(html), 'нет подписи «вручную»: ' + html);
    assert.ok(!/не поднят/.test(html), 'у не-донора не должно быть требования «не поднят»');
});

check('R133: без данных rpcNode статус не выдумывается', function () {
    assert.strictEqual(MB.rpcNodeBadge(backend({ role: 'donor' })), '',
        'старый балансер (нет rpcNode) — подпись должна отсутствовать, а не показывать «не поднят»');
    assert.strictEqual(MB.rpcNodeBadge(backend({ role: 'standalone', rpcNode: { wanted: false, running: false } })), '',
        'узел не поднят и роль не донор — показывать нечего');
});

check('R132/R133: источник вклада узлов — три разные подписи', function () {
    const none = MB.vramRemoteSourceNote(backend({ vramRemoteSource: 'none' }));
    const cache = MB.vramRemoteSourceNote(backend({ vramRemoteSource: 'cache' }));
    const probed = MB.vramRemoteSourceNote(backend({ vramRemoteSource: 'probed' }));
    assert.ok(/не измерялись/.test(none), 'нет подписи «не измерялись»: ' + none);
    assert.ok(/последнего замера/.test(cache), 'нет подписи «из замера»: ' + cache);
    assert.ok(/измерены/.test(probed), 'нет подписи «измерены»: ' + probed);
    assert.strictEqual(MB.vramRemoteSourceNote(backend({})), '', 'без поля источник не выдумывается');
});

check('R133: строка бэкенда содержит подпись узла (видно, что приказ исполнен)', function () {
    const row = MB.renderRow(backend({
        role: 'donor',
        rpcNode: { role: 'donor', wanted: true, enabled: true, running: true, listening: true, reachable: true, port: 50052 },
        vramRemoteSource: 'cache'
    }));
    assert.ok(/узел раскладки поднят/.test(row), 'в строке нет состояния узла: ' + row);
    assert.ok(/последнего замера/.test(row), 'в строке нет источника вклада узлов: ' + row);
});

console.log('\nmonitor-rpcnode-r133: ' + checks + ' проверок пройдено');

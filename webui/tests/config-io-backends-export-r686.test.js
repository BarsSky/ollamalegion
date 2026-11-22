// config-io-backends-export-r686.test.js — R686 (2026-11-22): экспорт
// конфигурации обязан содержать бэкенды, а формат экспорта меняться не должен.
//
// ЖИВОЙ ДЕФЕКТ. collectFullConfig() собирал бэкенды через
//   if (window.ui && window.ui.data && window.ui.data.backends)
// но поля `data` в объекте `ui` НЕТ (ui — возвращаемый объект IIFE app.js с
// другим набором полей). Ветка была мёртвой, и `backends` в скачанном файле
// ВСЕГДА был пустым массивом: список бэкендов терялся молча.
//
// ЧТО СТАЛО. Источник передаёт владелец состояния явной привязкой
// ConfigIO.bindBackendsSource({getBackends}) (app.js), по образцу
// BackendTypeFilter.bindPage (R680). Формат файла прежний: те же шесть
// ключей верхнего уровня и те же шесть полей у каждого бэкенда.
//
// Запуск: node webui/tests/config-io-backends-export-r686.test.js
'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');

const root = path.join(__dirname, '..');
const MODULE = path.join(root, 'js/modules/config-io.js');
const APP_JS = path.join(root, 'js/app.js');
const src = fs.readFileSync(MODULE, 'utf8');
const appJs = fs.readFileSync(APP_JS, 'utf8');

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

// --- минимальное окружение: модуль — синглтон, DOM нужен только функциям ----
global.window = global;
global.Utils = { downloadFile: function () {} };
global.showToast = function () {};
global.SettingsUI = { collectModeConfig: function () { return { mode: 'standard', config: {} }; } };
global.document = {
    querySelectorAll: function () { return []; },
    getElementById: function () { return null; }
};

function loadConfigIO() {
    // Модуль — IIFE, экспортирует window.ConfigIO; отдельный экземпляр на тест.
    delete global.ConfigIO;
    new Function(src)();
    assert.ok(global.ConfigIO, 'config-io.js не экспортировал window.ConfigIO');
    return global.ConfigIO;
}

const BACKENDS = [
    { id: 'b1', name: 'GPU 1', host: '10.0.0.1', ollamaPort: 11434, agentPort: 18032, weight: 3 },
    { id: 'b2', host: '10.0.0.2' }
];

check('модуль публикует явную привязку источника (bindBackendsSource)', function () {
    const io = loadConfigIO();
    assert.strictEqual(typeof io.bindBackendsSource, 'function',
        'нет ConfigIO.bindBackendsSource — app.js не может передать список бэкендов');
});

check('без привязки экспорт не падает и остаётся прежней структуры', function () {
    const io = loadConfigIO();
    const cfg = io.collectFullConfig();
    assert.deepStrictEqual(Object.keys(cfg).sort(),
        ['backends', 'exportedAt', 'mode_config', 'operating_mode', 'settings', 'version'],
        'структура экспорта изменилась');
    assert.deepStrictEqual(cfg.backends, [], 'без источника backends должен быть пустым массивом');
});

check('с привязкой экспорт содержит бэкенды с теми же полями', function () {
    const io = loadConfigIO();
    io.bindBackendsSource({ getBackends: function () { return BACKENDS; } });
    const cfg = io.collectFullConfig();
    // Формат верхнего уровня не изменился — появились только данные.
    assert.deepStrictEqual(Object.keys(cfg).sort(),
        ['backends', 'exportedAt', 'mode_config', 'operating_mode', 'settings', 'version'],
        'структура экспорта изменилась');
    assert.strictEqual(cfg.version, '1.0', 'version экспорта изменился');
    assert.strictEqual(cfg.operating_mode, 'standard', 'operating_mode экспорта изменился');
    assert.strictEqual(cfg.backends.length, 2, 'бэкенды не попали в экспорт: ' + JSON.stringify(cfg.backends));
    assert.deepStrictEqual(cfg.backends[0], {
        id: 'b1', name: 'GPU 1', host: '10.0.0.1', ollamaPort: 11434, agentPort: 18032, weight: 3
    }, 'поля бэкенда собраны неверно');
    assert.deepStrictEqual(cfg.backends[1], {
        id: 'b2', name: 'b2', host: '10.0.0.2', ollamaPort: 11434, agentPort: 18032, weight: 1
    }, 'дефолты (name/ports/weight) не применены');
    Object.keys(cfg.backends[0]).forEach(function (k) {
        assert.notStrictEqual(cfg.backends[0][k], undefined, 'поле ' + k + ' ушло в JSON как undefined');
    });
    // JSON.stringify отбрасывает undefined — проверяем, что после сериализации
    // поля на месте (это и есть «формат файла не изменился»).
    const parsed = JSON.parse(JSON.stringify(cfg));
    assert.deepStrictEqual(Object.keys(parsed.backends[0]).sort(),
        ['agentPort', 'host', 'id', 'name', 'ollamaPort', 'weight'],
        'формат записи бэкенда в файле изменился');
});

check('источник вызывается каждый раз (функция, а не снимок массива)', function () {
    const io = loadConfigIO();
    let list = [{ id: 'old' }];
    io.bindBackendsSource({ getBackends: function () { return list; } });
    assert.strictEqual(io.collectFullConfig().backends[0].id, 'old');
    // app-listeners.js:186 ПЕРЕЗАПИСЫВАЕТ data.backends — экспорт обязан
    // увидеть новый массив, а не сохранённую копию.
    list = [{ id: 'new-1' }, { id: 'new-2' }];
    assert.deepStrictEqual(io.collectFullConfig().backends.map(function (b) { return b.id; }), ['new-1', 'new-2'],
        'экспорт показывает состояние первого рендера: передана копия, а не живой источник');
});

check('в модуле не осталось обращения к несуществующему window.ui.data', function () {
    assert.strictEqual(src.indexOf('window.ui'), -1,
        'в config-io.js снова читается window.ui (такого поля в ui нет — ветка будет мёртвой)');
});

check('app.js действительно привязывает источник к ConfigIO', function () {
    assert.ok(/ConfigIO\.bindBackendsSource\(\{/.test(appJs),
        'app.js не вызывает ConfigIO.bindBackendsSource — привязка в модуле осталась бы инертной');
    assert.ok(/getBackends:\s*function\s*\(\)\s*\{\s*return data\.backends;/.test(appJs),
        'геттер не читает data.backends (нужен живой источник, а не снимок)');
});

console.log('\n' + passed + ' проверок зелёные, ' + failures.length + ' красных');
if (failures.length > 0) {
    failures.forEach(function (f) { console.error('  FAIL: ' + f); });
    process.exit(1);
}

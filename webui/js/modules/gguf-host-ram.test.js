// gguf-host-ram.test.js — R91 (2026-10-08).
//
// Запуск: node webui/js/modules/gguf-host-ram.test.js
//
// ЧТО ПРОВЕРЯЕМ И ПОЧЕМУ.
//
// Запрос оператора: «в метрики, что отдают агенты, не хватает информации об
// оперативной памяти для полной сводки по состоянию бэкенда».
//
// RAM агенты отдают давно (types.SystemMetrics.MemoryTotal/Used/Free, МБ —
// pkg/types/metrics.go), и GET /api/v1/backends/{id} возвращает её в поле
// `system`. Но на странице llama.cpp-бэкенда (панель «Инфо») рисовались только
// GPU и версия воркера: refreshDetail складывал весь ответ в
// state.backendRuntime, а `system` оттуда НЕ ЧИТАЛСЯ НИКОГДА — данные
// приезжали и молча терялись.
//
// Тест фиксирует три вещи:
//   1) hostSystemStats корректно разбирает system-метрики (в т.ч. snake_case и
//      «0 = датчика нет» вместо «0 B»/«0°C»);
//   2) renderHostSystemCard реально печатает RAM хоста вместе с VRAM-картой;
//   3) единицы не перепутаны: 25044 МБ RAM — это «24.5 GB», а НЕ «25044 MB» и
//      не «24.5 KB» (ровно этот класс ошибки был с VRAM: formatFileSize ждёт
//      байты, а метрики приходят в МБ).
//
// Тест грузит модуль панели с минимальными заглушками: `_()` переводит через
// словарь-объект, I18N/Utils/Renderers не нужны для этого блока.

'use strict';

const assert = require('assert');

global.window = global;
global.GgufModule = {};

require('./gguf-renderer-helpers.js');
const M = global.GgufModule;

// Заглушка i18n: возвращает сам ключ, если перевода нет (как I18N.t в webui).
const RU = {
    'gguf.host_info': 'Хост (система)',
    'metrics.ram_usage': 'Оперативная память',
    'metrics.cpu_usage': 'Загрузка CPU',
    'metrics.cpu_temp': 'Температура CPU',
    'gguf.disk_usage': 'Диск (всего)',
    'common.free': 'Свободно',
    'renderers.ram_unknown': 'RAM хоста неизвестна'
};
M._ = function (key) { return Object.prototype.hasOwnProperty.call(RU, key) ? RU[key] : key; };
M.stripGGUF = function (n) { return n; };
M.showToast = function () {};
M.state = {
    backendRuntime: null,
    gpuInfo: null,
    workerInfo: null,
    localModels: [],
    loadedModels: [],
    selectedBackendId: 'cppworker-gpu'
};
M.currentBackend = function () { return { id: 'cppworker-gpu', host: '192.0.2.10' }; };

global.Utils = { escapeHtml: function (s) { return String(s === undefined || s === null ? '' : s); } };
global.I18N = { t: M._ };

require('./gguf-renderer-detail-render.js');

let checks = 0;
function check(name, fn) {
    fn();
    checks++;
    console.log('  ✓ ' + name);
}

// --- 1. Разбор system-метрик -------------------------------------------------

check('hostSystemStats: живые значения со стойки (1817/25044 МБ, 7.3%)', function () {
    const st = M.hostSystemStats({ memoryTotal: 25044, memoryUsed: 1817, memoryFree: 23227, cpuUsagePercent: 3.5 });
    assert.strictEqual(st.ramTotalMb, 25044);
    assert.strictEqual(st.ramUsedMb, 1817);
    assert.strictEqual(st.ramFreeMb, 23227);
    assert.strictEqual(st.ramPercent, 7.3);
    assert.strictEqual(st.cpuPercent, 3.5);
    assert.strictEqual(st.known, true);
});

check('hostSystemStats: snake_case принимается (второй агент отдаёт иначе)', function () {
    const st = M.hostSystemStats({ memory_total: 7792, memory_used: 997, memory_free: 6795 });
    assert.strictEqual(st.ramTotalMb, 7792);
    assert.strictEqual(st.ramUsedMb, 997);
    assert.strictEqual(st.ramPercent, 12.8);
});

check('hostSystemStats: без total процент не считается (а не NaN%)', function () {
    const st = M.hostSystemStats({ memoryUsed: 512 });
    assert.strictEqual(st.ramTotalMb, null);
    assert.strictEqual(st.ramPercent, null);
});

check('hostSystemStats: 0 у агента = «неизвестно», а не «0 B» и не «0°C»', function () {
    const st = M.hostSystemStats({ memoryTotal: 0, memoryUsed: 0, memoryFree: 0, cpuTemperature: 0, diskTotal: 0 });
    assert.strictEqual(st.ramTotalMb, null);
    assert.strictEqual(st.cpuTempC, null);
    assert.strictEqual(st.diskTotalMb, null);
});

check('hostSystemStats: пустой/битый payload не роняет вызывающий код', function () {
    [null, undefined, {}, 'abc', 42, []].forEach(function (bad) {
        const st = M.hostSystemStats(bad);
        assert.strictEqual(st.known, false, 'для ' + JSON.stringify(bad) + ' known должен быть false');
        assert.strictEqual(st.ramPercent, null);
    });
});

check('hostSystemStats: температура читается и из вложенного cpu.temperature', function () {
    assert.strictEqual(M.hostSystemStats({ cpu: { temperature: 47 } }).cpuTempC, 47);
    assert.strictEqual(M.hostSystemStats({ cpuTemperature: 52 }).cpuTempC, 52);
});

check('hostSystemStats: строковые числа из JSON принимаются', function () {
    const st = M.hostSystemStats({ memoryTotal: '25044', memoryUsed: '1817' });
    assert.strictEqual(st.ramUsedMb, 1817);
    assert.strictEqual(st.ramPercent, 7.3);
});

// --- 2. Разметка карточки ----------------------------------------------------

check('renderHostSystemCard: RAM хоста попадает в разметку как «занято / всего (%)»', function () {
    M.state.backendRuntime = { system: { memoryTotal: 25044, memoryUsed: 1817, memoryFree: 23227, cpuUsagePercent: 3.5 } };
    const html = M.renderHostSystemCard();
    assert.ok(html.indexOf('1.8 GB / 24.5 GB (7.3%)') !== -1, 'ожидалась строка RAM, получено: ' + html);
    // Заголовок блока и подписи переведены, а не выведены ключами.
    assert.ok(html.indexOf('Оперативная память') !== -1, 'подпись RAM должна быть переведена');
    assert.ok(html.indexOf('metrics.ram_usage') === -1, 'сырой i18n-ключ в разметке недопустим');
});

check('renderHostSystemCard: свободная RAM показана отдельной строкой', function () {
    M.state.backendRuntime = { system: { memoryTotal: 25044, memoryUsed: 1817, memoryFree: 23227 } };
    const html = M.renderHostSystemCard();
    assert.ok(html.indexOf('22.7 GB') !== -1, 'ожидалось «22.7 GB» свободной RAM, получено: ' + html);
    assert.ok(html.indexOf('Свободно') !== -1);
});

check('renderHostSystemCard: MB не путаются с байтами («24.5 GB», не «24.5 KB»)', function () {
    M.state.backendRuntime = { system: { memoryTotal: 25044, memoryUsed: 1817, memoryFree: 23227 } };
    const html = M.renderHostSystemCard();
    assert.ok(html.indexOf('KB') === -1, 'в блоке RAM не должно быть килобайтов: ' + html);
    assert.ok(html.indexOf('25044 MB') === -1, 'сырые мегабайты без форматирования недопустимы');
});

check('renderHostSystemCard: CPU и диск рисуются только когда есть данные', function () {
    M.state.backendRuntime = { system: { memoryTotal: 25044, memoryUsed: 1817, memoryFree: 23227 } };
    let html = M.renderHostSystemCard();
    assert.ok(html.indexOf('Загрузка CPU') === -1, 'без cpuUsagePercent блока CPU быть не должно');

    M.state.backendRuntime = { system: { memoryTotal: 25044, memoryUsed: 1817, memoryFree: 23227, cpuUsagePercent: 12.5, cpu: { temperature: 47 }, diskTotal: 500000, diskFree: 120000 } };
    html = M.renderHostSystemCard();
    assert.ok(html.indexOf('12.5%') !== -1, 'загрузка CPU должна быть в разметке');
    assert.ok(html.indexOf('47°C') !== -1, 'температура CPU должна быть в разметке');
    assert.ok(html.indexOf('488.3 GB') !== -1, 'объём диска в GB, получено: ' + html);
});

check('renderHostSystemCard: без system-метрик — объяснение, а не пустая карточка', function () {
    M.state.backendRuntime = { system: null };
    const html = M.renderHostSystemCard();
    assert.ok(html.indexOf('RAM хоста неизвестна') !== -1, 'нужно объяснить причину прочерка: ' + html);
});

check('hostSystemPayload: берёт system из backendRuntime, а при его отсутствии — из бэкенда', function () {
    M.state.backendRuntime = { system: { memoryTotal: 25044 } };
    assert.deepStrictEqual(M.hostSystemPayload(), { memoryTotal: 25044 });
    M.state.backendRuntime = { system: null };
    M.currentBackend = function () { return { id: 'cppworker-gpu', system: { memoryTotal: 1 } }; };
    assert.deepStrictEqual(M.hostSystemPayload(), { memoryTotal: 1 });
    M.currentBackend = function () { return { id: 'cppworker-gpu' }; };
    assert.strictEqual(M.hostSystemPayload(), null);
});

// --- 3. Панель «Инфо» --------------------------------------------------------

check('renderAboutPane: блок «Хост (система)» с RAM присутствует в панели «Инфо»', function () {
    M.currentBackend = function () { return { id: 'cppworker-gpu', host: '192.0.2.10', url: 'http://192.0.2.10:18092' }; };
    M.state.backendRuntime = { system: { memoryTotal: 25044, memoryUsed: 1817, memoryFree: 23227 } };
    M.state.gpuInfo = { devices: [{ name: 'RTX 3070', memoryTotal: 8192, memoryFree: 7097, usagePercent: 4 }] };
    M.state.workerInfo = { version: 'b4500' };
    const html = M.renderAboutPane();
    assert.ok(html.indexOf('Хост (система)') !== -1, 'заголовок блока хоста обязателен');
    assert.ok(html.indexOf('Оперативная память') !== -1, 'RAM хоста должна быть в панели «Инфо»');
    assert.ok(html.indexOf('8.0 GB') !== -1, 'карточка GPU не должна поехать');
});

console.log('\nOK: ' + checks + ' checks passed');

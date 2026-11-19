// first-run-card-r596.test.js — R596 (2026-11-19): карточка первого запуска.
//
// Запуск: node webui/tests/first-run-card-r596.test.js
//
// ЗАЧЕМ. Решение владельца (2026-11-19): мастер `setup-wizard.js` (739 строк, 4 шага)
// заменить карточкой. Мастер спрашивал то, чего в продукте нет (режим — снят W1,
// тип бэкенда — выводится из состава, R195), и при этом писал ЧЕТЫРЕ порога, из
// которых один (`vramMaxUsage`) не читает ни одно решение: единственное
// упоминание на сервере — pass-through в `handlers_cluster.go`, а пороги
// gpu/cpu/ram читает `slot_manager.go`. То есть мастер годами создавал ложную
// картину «настройка применена».
//
// Что проверяется:
//   1. payload = РОВНО три влияющих порога + `initialized`, без мёртвого
//      `vramMaxUsage` и без снятых `operatingMode`/`backendEngine`;
//   2. значения вне 1..100 не записываются (иначе 0 отключил бы учёт занятости,
//      а 1000 разрешил бы всё) — берётся дефолт;
//   3. карточка рисует три поля, «Готово» и ссылку «настроить подробно» на #settings;
//   4. мастера и его семейства больше нет (файлы, тег скрипта, guard
//      `setupWizardModal`), а карточка подключена и вызывается из app.js.

'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');

const ROOT = path.join(__dirname, '..', '..');
const MODULES = path.join(ROOT, 'webui', 'js', 'modules');
const CARD = path.join(MODULES, 'first-run-card.js');
const INDEX = path.join(ROOT, 'webui', 'index.html');
const APP = path.join(ROOT, 'webui', 'js', 'app.js');

// --- DOM-заглушка: карточка монтируется в #firstRunHost
const host = { innerHTML: '', addEventListener: function () {} };
const inputs = {};
global.window = global;
global.I18N = { t: function (k) { return 'I18N:' + k; } };
global.localStorage = {
    _v: {},
    getItem: function (k) { return this._v[k] === undefined ? null : this._v[k]; },
    setItem: function (k, v) { this._v[k] = String(v); },
    removeItem: function (k) { delete this._v[k]; }
};
global.document = {
    getElementById: function (id) {
        if (id === 'firstRunHost') return host;
        if (id === 'firstRunCard') return host.innerHTML ? { parentNode: { removeChild: function () { host.innerHTML = ''; } } } : null;
        return inputs[id] || null;
    },
    querySelectorAll: function () { return []; },
    addEventListener: function () {}
};

const NodeModule = require('module');
const m = new NodeModule(CARD, module);
m._compile(fs.readFileSync(CARD, 'utf8'), CARD);
const FRC = global.window.FirstRunCard;

let checks = 0;
const failures = [];
function check(name, fn) {
    try { fn(); checks++; console.log('  ✓ ' + name); } catch (e) { failures.push(name); console.error('  ✗ ' + name + ' — ' + e.message); }
}

check('R596: payload — ровно три влияющих порога + initialized', function () {
    const p = FRC.buildPayload({ gpuMaxUsage: '85', cpuMaxUsage: '70', ramMaxUsage: '75' });
    assert.deepStrictEqual(Object.keys(p).sort(), ['cpuMaxUsage', 'gpuMaxUsage', 'initialized', 'ramMaxUsage'],
        'состав payload изменился: ' + Object.keys(p).join(', '));
    assert.strictEqual(p.gpuMaxUsage, 85);
    assert.strictEqual(p.cpuMaxUsage, 70);
    assert.strictEqual(p.ramMaxUsage, 75);
    assert.strictEqual(p.initialized, true);
});

check('R596: мёртвый vramMaxUsage не пишется (нет потребителя на сервере)', function () {
    const p = FRC.buildPayload({ gpuMaxUsage: 90, vramMaxUsage: 85 });
    assert.strictEqual(Object.prototype.hasOwnProperty.call(p, 'vramMaxUsage'), false,
        'vramMaxUsage вернулся в payload: сервер его не читает ни в одном решении');
});

check('R596: снятые поля (operatingMode, backendEngine) не возвращаются', function () {
    const p = FRC.buildPayload({ operatingMode: 'standard', backendEngine: 'llama_cpp' });
    assert.strictEqual(Object.prototype.hasOwnProperty.call(p, 'operatingMode'), false);
    assert.strictEqual(Object.prototype.hasOwnProperty.call(p, 'backendEngine'), false);
});

check('R596: пустые и мусорные значения → дефолт 90/80/85', function () {
    const p = FRC.buildPayload({ gpuMaxUsage: '', cpuMaxUsage: 'abc', ramMaxUsage: undefined });
    assert.deepStrictEqual([p.gpuMaxUsage, p.cpuMaxUsage, p.ramMaxUsage], [90, 80, 85]);
});

check('R596: значения вне 1..100 не записываются', function () {
    assert.strictEqual(FRC.clampPercent('0', 90), 90, '0 отключил бы учёт занятости');
    assert.strictEqual(FRC.clampPercent('1000', 90), 90, '1000 разрешил бы всё');
    assert.strictEqual(FRC.clampPercent('-5', 90), 90);
    assert.strictEqual(FRC.clampPercent('100', 90), 100, '100 — допустимая граница');
    assert.strictEqual(FRC.clampPercent('1', 90), 1, '1 — допустимая граница');
});

check('R596: карточка рисует три поля, «Готово» и ссылку на настройки', function () {
    const html = FRC.render({ gpuMaxUsage: 90, cpuMaxUsage: 80, ramMaxUsage: 85 });
    assert.ok(/id="firstRunCard"/.test(html), 'нет карточки');
    ['gpuMaxUsage', 'cpuMaxUsage', 'ramMaxUsage'].forEach(function (f) {
        assert.ok(html.indexOf('data-field="' + f + '"') !== -1, 'нет поля ' + f);
    });
    assert.ok(/id="firstRunDone"/.test(html), 'нет кнопки «Готово»');
    assert.ok(/href="#settings"/.test(html), 'нет ссылки «настроить подробно» на #settings');
    assert.strictEqual(html.indexOf('vramMaxUsage'), -1, 'в карточке снова спрашивают мёртвый порог');
});

check('R596: show/hide — одна карточка, host очищается', function () {
    host.innerHTML = '';
    assert.strictEqual(FRC.show(), true, 'карточка не показалась');
    assert.ok(host.innerHTML.indexOf('id="firstRunCard"') !== -1, 'разметка не попала в host');
    assert.strictEqual(FRC.show(), false, 'вторая карточка не должна создаваться');
    FRC.hide();
    assert.strictEqual(host.innerHTML, '', 'host не очищен');
});

check('R596: мастера больше нет — ни файла, ни тега, ни guard-а', function () {
    assert.strictEqual(fs.existsSync(path.join(MODULES, 'setup-wizard.js')), false, 'setup-wizard.js вернулся');
    assert.strictEqual(fs.existsSync(path.join(MODULES, 'setup-plan.js')), false,
        'setup-plan.js вернулся без потребителя (мастер был единственным местом монтирования)');
    const html = fs.readFileSync(INDEX, 'utf8');
    assert.strictEqual(/<script[^>]*setup-wizard\.js/.test(html), false, 'index.html снова грузит мастер');
    assert.strictEqual(/<script[^>]*setup-wizard-presets\.js/.test(html), false, 'index.html снова грузит пресеты мастера');
    assert.ok(/id="firstRunHost"/.test(html), 'нет хоста карточки');
    assert.ok(/js\/modules\/first-run-card\.js/.test(html), 'модуль карточки не подключён');
    const app = fs.readFileSync(APP, 'utf8');
    assert.strictEqual(/window\.SetupWizard/.test(app), false, 'app.js снова зовёт модуль мастера');
    assert.strictEqual(/setupWizardModal/.test(app), false, 'guard applyServerConfig снова про мастер');
    assert.ok(/FirstRunCard\.check/.test(app), 'app.js не запускает карточку первого запуска');
});

if (failures.length) {
    console.error('\nИТОГ: ' + failures.length + ' провалов из ' + (checks + failures.length));
    process.exit(1);
}
console.log('\n' + checks + ' проверок, все зелёные');

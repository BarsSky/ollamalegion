// data-refresh.test.js — R84 (2026-10-03): единая точка обновления данных WebUI.
//
// Запуск: node webui/js/modules/data-refresh.test.js
//
// ЧТО ПРОВЕРЯЕМ (и почему именно это):
//   1. РЕЕСТР ПРОВАЙДЕРОВ: страница регистрирует свой источник, общая кнопка
//      дёргает провайдеров АКТИВНОЙ страницы (+глобальные). Раньше у каждой
//      страницы была своя кнопка «Обновить», и часть из них обновляла только
//      локальную копию данных — со стороны это выглядело как «кнопка не работает».
//   2. ИНДИКАТОР СВЕЖЕСТИ: «Обновлено 2 с назад · авто» / «пауза» / ошибка.
//      Без него нельзя отличить «данные свежие» от «обновление сломалось».
//   3. ОШИБКА ПРОВАЙДЕРА не ломает остальных и попадает в индикатор.
//   4. АВТО/ПАУЗА: переключатель в шапке + запоминание в localStorage.
//   5. МОНТИРОВАНИЕ: клик по индикатору = обновить сейчас (App.refreshCurrentPage),
//      клик по иконке = пауза/возобновление.
'use strict';

const assert = require('assert');

// --- mocks: DOM -------------------------------------------------------------

function makeElement(id) {
    const classes = new Set();
    const listeners = {};
    const attrs = {};
    const el = {
        id: id,
        style: {},
        value: '',
        textContent: '',
        innerHTML: '',
        className: '',
        _html: '',
        getAttribute: function (n) { return Object.prototype.hasOwnProperty.call(attrs, n) ? attrs[n] : null; },
        setAttribute: function (n, v) { attrs[n] = v; },
        addEventListener: function (type, fn) { (listeners[type] = listeners[type] || []).push(fn); },
        removeEventListener: function () { },
        dispatch: function (type, ev) { (listeners[type] || []).forEach(function (fn) { fn(ev || {}); }); },
        classList: {
            add: function (c) { classes.add(c); },
            remove: function (c) { classes.delete(c); },
            contains: function (c) { return classes.has(c); },
            toggle: function (c, on) { if (on === undefined) { classes.has(c) ? classes.delete(c) : classes.add(c); } else if (on) { classes.add(c); } else { classes.delete(c); } },
        },
    };
    return el;
}

const elements = {};
function getEl(id) {
    if (!Object.prototype.hasOwnProperty.call(elements, id)) elements[id] = makeElement(id);
    return elements[id];
}

global.window = global;
global.document = {
    getElementById: getEl,
    addEventListener: function () { },
    visibilityState: 'visible',
};
const store = {};
global.localStorage = {
    getItem: function (k) { return Object.prototype.hasOwnProperty.call(store, k) ? store[k] : null; },
    setItem: function (k, v) { store[k] = String(v); },
    removeItem: function (k) { delete store[k]; },
};

const RU = {
    'refresh.updated_ago': 'Обновлено {age} назад',
    'refresh.updated_just_now': 'Обновлено только что',
    'refresh.just_now': 'только что',
    'refresh.sec_ago': '{n} с',
    'refresh.min_ago': '{n} мин',
    'refresh.hour_ago': '{n} ч',
    'refresh.never': 'нет данных',
    'refresh.auto': 'авто',
    'refresh.paused': 'пауза',
    'refresh.running': 'обновляю…',
    'refresh.error': 'не удалось обновить: {error}',
    'refresh.toggle_auto_pause': 'Приостановить авто-обновление',
    'refresh.toggle_auto_resume': 'Возобновить авто-обновление',
};
global.I18N = {
    t: function (k, vars) {
        var s = Object.prototype.hasOwnProperty.call(RU, k) ? RU[k] : k;
        if (vars) Object.keys(vars).forEach(function (p) { s = String(s).replace('{' + p + '}', vars[p]); });
        return s;
    },
};

let refreshCalls = 0;
global.App = { refreshCurrentPage: function () { refreshCalls++; return Promise.resolve(true); } };

require('./data-refresh.js');
const DR = global.DataRefresh;
assert.ok(DR, 'window.DataRefresh должен быть экспортирован (см. check_iife_exports.py)');

// --- мини-раннер ------------------------------------------------------------

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

/**
 * checkAsync — то же, но с await: асинхронные проверки обязаны завершаться ДО
 * следующей, иначе синхронная проверка увидит незавершённое обновление
 * (индикатор «обновляю…», lastError ещё не выставлен).
 */
async function checkAsync(name, fn) {
    try {
        await fn();
        passed++;
        console.log('  \u2713 ' + name);
    } catch (e) {
        failures.push(name + ': ' + (e && e.message));
        console.error('  \u2717 ' + name + ' — ' + (e && e.message));
    }
}
const sleep = function (ms) { return new Promise(function (r) { setTimeout(r, ms); }); };

// Все проверки — внутри async main(): асинхронные обязаны дожидаться, а
// top-level await в CommonJS-файле Node не принимает (require + await).
(async function main() {

// ============================================================================
// 1. Подпись свежести
// ============================================================================

check('formatAge: нет данных / только что / секунды / минуты / часы', function () {
    assert.strictEqual(DR.formatAge(-1), 'нет данных');
    assert.strictEqual(DR.formatAge(0), 'только что');
    assert.strictEqual(DR.formatAge(4), 'только что');
    assert.strictEqual(DR.formatAge(45), '45 с');
    assert.strictEqual(DR.formatAge(125), '2 мин');
    assert.strictEqual(DR.formatAge(7200), '2 ч');
});

check('freshnessLabel: «Обновлено … назад · авто» / «· пауза» / ошибка', function () {
    assert.strictEqual(DR.freshnessLabel(2, true, ''), 'Обновлено только что · авто');
    assert.strictEqual(DR.freshnessLabel(30, false, ''), 'Обновлено 30 с назад · пауза');
    assert.strictEqual(DR.freshnessLabel(30, true, 'HTTP 502'), 'не удалось обновить: HTTP 502');
    // Без данных шаблон не должен давать «Обновлено нет данных назад».
    assert.strictEqual(DR.freshnessLabel(-1, true, ''), 'нет данных · авто');
});

check('ageSeconds: до первого обновления — «нет данных», после — считается', function () {
    const st = DR._state;
    const saved = st.lastOkAt;
    st.lastOkAt = 0;
    assert.strictEqual(DR.ageSeconds(), -1);
    st.lastOkAt = Date.now() - 7000;
    const age = DR.ageSeconds();
    assert.ok(age >= 6 && age <= 8, 'возраст должен быть ~7 с, получено ' + age);
    st.lastOkAt = saved;
});

// ============================================================================
// 2. Реестр провайдеров
// ============================================================================

await checkAsync('register: провайдер страницы вызывается только для своей страницы', async function () {
    let calls = [];
    DR.register('unit-a', function () { calls.push('a'); return Promise.resolve(); });
    DR.register('unit-b', function () { calls.push('b'); return Promise.resolve(); });
    const ok = await DR.refresh('unit-a');
    assert.strictEqual(ok, true);
    assert.deepStrictEqual(calls, ['a'], 'должен вызваться только провайдер своей страницы');
});

await checkAsync('registerGlobal: глобальный провайдер работает на любой странице', async function () {
    let called = 0;
    DR.registerGlobal(function () { called++; return Promise.resolve(); });
    await DR.refresh('unit-b');
    assert.strictEqual(called, 1, 'глобальный провайдер должен вызываться на каждой странице');
});

await checkAsync('refresh: все провайдеры страницы вызываются параллельно', async function () {
    let done = [];
    DR.register('unit-par', function () { return sleep(20).then(function () { done.push(1); }); });
    DR.register('unit-par', function () { return sleep(20).then(function () { done.push(2); }); });
    const t0 = Date.now();
    await DR.refresh('unit-par');
    const dt = Date.now() - t0;
    assert.strictEqual(done.length, 2);
    assert.ok(dt < 45, 'провайдеры должны идти параллельно, а не последовательно (' + dt + ' мс)');
});

await checkAsync('refresh: ошибка провайдера → false + текст в индикаторе, остальные не отменяются', async function () {
    let second = 0;
    DR.register('unit-err', function () { return Promise.reject(new Error('HTTP 502')); });
    DR.register('unit-err', function () { second++; return Promise.resolve(); });
    const ok = await DR.refresh('unit-err');
    assert.strictEqual(ok, false, 'ошибка должна вернуть false');
    assert.strictEqual(second, 1, 'падение одного провайдера не должно отменять остальные');
    assert.strictEqual(DR._state.lastError, 'HTTP 502');
    DR.markFresh();
    assert.strictEqual(DR._state.lastError, '', 'markFresh сбрасывает ошибку');
});

await checkAsync('refresh: страница без провайдеров — не ошибка (живёт на общем опросе)', async function () {
    const ok = await DR.refresh('нет-такой-страницы');
    assert.strictEqual(ok, true);
});

// ============================================================================
// 3. DOM: индикатор и переключатель
// ============================================================================

check('render: пишет подпись в #dataFreshnessText и красит иконку по состоянию', function () {
    DR.markFresh();
    DR.render();
    const text = getEl('dataFreshnessText');
    assert.ok(/Обновлено/.test(text.textContent), 'нет подписи: ' + text.textContent);
    assert.ok(/авто/.test(text.textContent), 'нет режима авто: ' + text.textContent);
    assert.strictEqual(getEl('dataFreshnessIcon').className.indexOf('fa-circle-check') !== -1, true, 'свежие данные — зелёная галочка');

    DR.markError('boom');
    assert.ok(/не удалось обновить/.test(getEl('dataFreshnessText').textContent), 'ошибка должна быть видна');
    assert.strictEqual(getEl('dataFreshness').classList.contains('is-error'), true, 'ошибка подсвечивает индикатор');

    DR.markFresh();
    DR.setAuto(false);
    assert.ok(/пауза/.test(getEl('dataFreshnessText').textContent), 'пауза должна быть видна');
    assert.strictEqual(getEl('dataFreshnessIcon').className.indexOf('fa-pause') !== -1, true, 'пауза — иконка паузы');
    assert.strictEqual(getEl('dataAutoToggleIcon').className.indexOf('fa-play') !== -1, true, 'переключатель предлагает возобновить');
});

check('setAuto/toggleAuto: состояние переживает перезагрузку (localStorage)', function () {
    DR.setAuto(true);
    assert.strictEqual(DR.isAuto(), true);
    assert.strictEqual(store['ollamalegion_auto_refresh'], '1');
    assert.strictEqual(DR.toggleAuto(), false);
    assert.strictEqual(store['ollamalegion_auto_refresh'], '0');
    assert.strictEqual(DR.isAuto(), false);
    DR.setAuto(true);
});

check('onAutoChange: владелец таймера узнаёт о паузе/возобновлении', function () {
    const seen = [];
    DR.onAutoChange(function (on) { seen.push(on); });
    DR.setAuto(false);
    DR.setAuto(true);
    assert.deepStrictEqual(seen, [false, true]);
});

check('shouldPoll: единое правило опроса — пауза и скрытая вкладка', function () {
    DR.setAuto(true);
    global.document.visibilityState = 'visible';
    assert.strictEqual(DR.shouldPoll(), true, 'авто + видимая вкладка = опрашиваем');
    global.document.visibilityState = 'hidden';
    assert.strictEqual(DR.shouldPoll(), false, 'скрытая вкладка = не опрашиваем (24/7-дашборд не молотит API)');
    global.document.visibilityState = 'visible';
    DR.setAuto(false);
    assert.strictEqual(DR.shouldPoll(), false, 'пауза авто-обновления = не опрашиваем');
    DR.setAuto(true);
});

check('mount: клик по индикатору = обновить сейчас, клик по иконке = пауза', function () {
    DR.mount();
    refreshCalls = 0;
    getEl('dataFreshness').dispatch('click', {});
    assert.strictEqual(refreshCalls, 1, 'клик по индикатору должен запускать обновление страницы');
    const before = DR.isAuto();
    getEl('dataAutoToggle').dispatch('click', { preventDefault: function () { } });
    assert.strictEqual(DR.isAuto(), !before, 'клик по иконке должен переключать авто-обновление');
    DR.setAuto(true);
});

console.log('\n' + (failures.length ? 'FAILED: ' + failures.length : 'ИТОГ: все проверки пройдены (' + passed + ')'));
if (failures.length) {
    failures.forEach(function (f) { console.error(' - ' + f); });
}
// mount() ставит секундный тикер индикатора — это живой handle, поэтому выходим
// явно (иначе node ждёт вечно и тест «висит» в CI).
process.exit(failures.length ? 1 : 0);
})();

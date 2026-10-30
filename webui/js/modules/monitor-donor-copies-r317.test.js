// monitor-donor-copies-r317.test.js — R317 (этап 5 рефакторинга настроек WebUI):
// исключение R304 в панели ролей раскладки («донор может держать свои копии»).
//
// Запуск: node webui/js/modules/monitor-donor-copies-r317.test.js
//
// ЧТО БЫЛО НЕ ТАК ДО ЭТОЙ ПРАВКИ. Флаг LB_DONOR_ALLOW_OWN_COPIES — исключение из
// доктрины R304 «бэкенд стал донором, значит своих копий моделей он больше не
// поднимает» — жил ТОЛЬКО в общем списке горячих настроек на странице
// «Настройки», среди 17 строк и вдали от ролей раскладки. Оператор искал его
// там, где назначает главного и доноров (страница «Монитор», панель «Раскладка
// слоёв (RPC)»), не находил — и правил env с перезапуском стенда, хотя флаг
// горячий и применяется сразу.
//
// ЧТО ФИКСИРУЕТ ТЕСТ (муляж fetch, без сети и без браузера):
//   1. монитор.html содержит тумблер с подписью-исключением и пояснение про
//      «только память мастера» (R304), а модуль реестра подключён ДО панели;
//   2. значение читается из GET /api/v1/settings тем же модулем
//      (settings-registry.js), который владеет состоянием: своей копии у панели нет;
//   3. переключение уходит как PUT {"name","value":1} (bool → 0/1) и строка
//      обновляется ИЗ ОТВЕТА сервера;
//   4. «Сбросить» — POST /api/v1/settings/reset {"name"}, после которого панель
//      перечитывает реестр (значение после сброса знает только сервер);
//   5. viewer не может ни переключить, ни сбросить: тумблер погашен, запрос НЕ
//      уходит, причина названа (лишний 403 оператору не нужен);
//   6. без модуля реестра (старый образ) панель честно говорит «недоступно», а не
//      рисует живой на вид тумблер.

'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');

let passed = 0;
const failures = [];
async function check(name, fn) {
    try {
        await fn();
        passed++;
        console.log('  \u2713 ' + name);
    } catch (e) {
        failures.push(name + ': ' + (e && e.message));
        console.error('  \u2717 ' + name + ' \u2014 ' + (e && e.message));
    }
}

const NAME = 'LB_DONOR_ALLOW_OWN_COPIES';

// --- DOM-заглушка -----------------------------------------------------------
function makeEl(id) {
    return {
        id: id || '', innerHTML: '', textContent: '', value: '', checked: false,
        disabled: false, title: '', className: '', style: {}, dataset: {},
        classList: { add() {}, remove() {}, contains() { return false; } },
        listeners: {},
        addEventListener(ev, fn) { (this.listeners[ev] = this.listeners[ev] || []).push(fn); },
        removeEventListener() {},
        appendChild() {},
        getAttribute(k) { return this[k] === undefined ? null : this[k]; },
        setAttribute(k, v) { this[k] = v; },
        querySelector() { return null; },
        querySelectorAll() { return []; },
    };
}

const ELS = [
    'layoutDonorOwnCopiesBox', 'layoutDonorOwnCopies', 'layoutDonorOwnCopiesReset',
    'layoutDonorOwnCopiesState', 'layoutDonorOwnCopiesMsg', 'layoutDonorOwnCopiesHint',
    'layoutMainSelect', 'layoutDonorList', 'layoutStatus', 'layoutStateBadge',
    'layoutApplyBtn', 'layoutLocalBtn',
];
const DOM = {};
ELS.forEach(function (id) { DOM[id] = makeEl(id); });

const domListeners = {};
global.document = {
    // 'loading' — панель НЕ инициализируется сама: сеть в тесте только от явных вызовов.
    readyState: 'loading',
    getElementById(id) { return DOM[id] || null; },
    createElement() { return makeEl(''); },
    querySelector() { return null; },
    querySelectorAll() { return []; },
    addEventListener(ev, fn) { domListeners[ev] = fn; },
};
global.window = global;

// --- i18n: НАСТОЯЩИЕ словари (сырой ключ в интерфейсе = баг) -----------------
require('../i18n/ru.js');
require('../i18n/en.js');
const RU = global.I18N_RU;
const EN = global.I18N_EN;
assert.ok(RU && EN, 'ru.js и en.js должны быть загружены');

const missingKeys = [];
global.I18N = {
    getLang() { return 'ru'; },
    t(key, vars) {
        let s = RU[key];
        if (s === undefined) { if (missingKeys.indexOf(key) === -1) { missingKeys.push(key); } s = key; }
        if (vars) {
            Object.keys(vars).forEach(function (p) { s = String(s).split('{' + p + '}').join(String(vars[p])); });
        }
        return s;
    },
};

// MonitorApp.T — второй (страничный) вход к тем же словарям.
global.MonitorApp = {
    esc(s) { return String(s === null || s === undefined ? '' : s); },
    T(key) { const v = RU[key]; return v === undefined ? key : v; },
};

// --- роль оператора ---------------------------------------------------------
let isAdmin = true;
global.Auth = { isAdmin() { return isAdmin; } };

// --- сеть: мок fetch по ключу "METHOD /path" --------------------------------
const BASE = 'http://balancer.test:18081';
global.WEBUI_CONFIG = { API_BASE: BASE, API_TOKEN: 'tok-317' };

const calls = [];
function mkRes(status, body) {
    const text = JSON.stringify(body === undefined ? null : body);
    return {
        ok: status >= 200 && status < 300, status, statusText: 'OK',
        headers: { get() { return 'application/json'; } },
        text() { return Promise.resolve(text); },
        json() { return Promise.resolve(JSON.parse(text)); },
    };
}

// Серверная «правда» о флаге: тест меняет её так же, как это сделал бы балансер.
const registry = {
    name: NAME, type: 'bool', group: 'nctx',
    description: '0 = донор держит только память мастера; 1 = разрешить донору свои копии.',
    hotReload: true, default: 0, effective: 0, source: 'default',
};

global.fetch = function (url, opts) {
    const method = ((opts && opts.method) || 'GET').toUpperCase();
    const p = String(url).replace(BASE, '');
    const body = (opts && opts.body) ? JSON.parse(opts.body) : null;
    calls.push({ method: method, path: p, body: body });

    if (method === 'GET' && p === '/api/v1/settings') {
        return Promise.resolve(mkRes(200, {
            count: 1, overrides: registry.source === 'override' ? 1 : 0,
            sourcePriority: ['override', 'env', 'default'],
            settings: [Object.assign({}, registry)],
        }));
    }
    if (method === 'PUT' && p === '/api/v1/settings') {
        registry.effective = Number(body.value);
        registry.source = 'override';
        return Promise.resolve(mkRes(200, {
            status: 'updated', name: body.name, value: body.value,
            effective: body.value, source: 'override', applied: 'override', hotReload: true,
        }));
    }
    if (method === 'POST' && p === '/api/v1/settings/reset') {
        registry.effective = 0;
        registry.source = 'default';
        return Promise.resolve(mkRes(200, { status: 'reset', name: body.name, effective: 0, source: 'default' }));
    }
    if (method === 'GET' && p === '/api/v1/backends') {
        return Promise.resolve(mkRes(200, { backends: [] }));
    }
    return Promise.resolve(mkRes(404, { error: 'no route: ' + method + ' ' + p }));
};

global.Utils = { showToast() {} };
global.MonitorApiXhr = {
    fetchBackends() { return Promise.resolve({ backends: [] }); },
    setBackendRole() { return Promise.resolve(true); },
};

// --- модули: api.js → settings-registry.js (владелец реестра) → layout-panel.js
require('./api.js');
require('./settings-registry.js');
const SR = global.SettingsRegistry;
assert.ok(SR, 'settings-registry.js должен публиковать SettingsRegistry');

require('../monitor/layout-panel.js');
const ML = global.MonitorLayout;
assert.ok(ML && typeof ML.setDonorOwnCopies === 'function',
    'MonitorLayout должен публиковать setDonorOwnCopies (R317)');

function puts() { return calls.filter(function (c) { return c.method === 'PUT' && c.path === '/api/v1/settings'; }); }
function resets() { return calls.filter(function (c) { return c.method === 'POST' && c.path === '/api/v1/settings/reset'; }); }

const cb = DOM['layoutDonorOwnCopies'];
const badge = DOM['layoutDonorOwnCopiesState'];
const resetBtn = DOM['layoutDonorOwnCopiesReset'];
const msg = DOM['layoutDonorOwnCopiesMsg'];

const MONITOR_HTML = fs.readFileSync(path.join(__dirname, '..', '..', 'monitor.html'), 'utf8');

(async function main() {
    console.log('monitor-donor-copies-r317.test.js — R317 (исключение R304 в панели ролей)\n');

    await check('monitor.html: тумблер, подпись-исключение и пояснение R304 на месте', function () {
        assert.ok(MONITOR_HTML.indexOf('id="layoutDonorOwnCopies"') >= 0,
            'в панели раскладки нет тумблера #layoutDonorOwnCopies');
        assert.ok(MONITOR_HTML.indexOf('data-i18n="monitor.layout.donorOwnCopies.label"') >= 0,
            'у тумблера нет подписи «донор может держать свои копии моделей (исключение)»');
        const hint = MONITOR_HTML.indexOf('data-i18n="monitor.layout.donorOwnCopies.hint"');
        assert.ok(hint >= 0, 'нет пояснения, что по умолчанию донор держит только память мастера');
        const hintBlock = MONITOR_HTML.slice(hint, hint + 700);
        assert.ok(/ТОЛЬКО память мастера/.test(hintBlock) && /R304/.test(hintBlock),
            'пояснение обязано называть доктрину R304 и «только память мастера»');
        // Тумблер стоит В ПАНЕЛИ РОЛЕЙ (раскладки), а не в отдельном блоке страницы.
        const layoutPanel = MONITOR_HTML.indexOf('id="panelLayout"');
        const box = MONITOR_HTML.indexOf('id="layoutDonorOwnCopiesBox"');
        assert.ok(layoutPanel >= 0 && box > layoutPanel, 'тумблер обязан жить внутри панели раскладки');
    });

    await check('monitor.html: грузит модуль реестра ДО панели (иначе значение взять негде)', function () {
        // Ищем ТЕГИ (src=...), а не упоминания в комментариях рядом с ними.
        const apiJs = MONITOR_HTML.indexOf('src="js/modules/api.js');
        const registryJs = MONITOR_HTML.indexOf('src="js/modules/settings-registry.js');
        const panelJs = MONITOR_HTML.indexOf('src="js/monitor/layout-panel.js');
        assert.ok(apiJs >= 0, 'monitor.html не подключает js/modules/api.js (Api.hotSettings не будет)');
        assert.ok(registryJs >= 0, 'monitor.html не подключает settings-registry.js');
        assert.ok(apiJs < registryJs, 'api.js обязан грузиться ДО settings-registry.js');
        assert.ok(registryJs < panelJs, 'settings-registry.js обязан грузиться ДО layout-panel.js');
    });

    await check('значение читается из реестра: «по умолчанию: выкл», сброс погашен', async function () {
        calls.length = 0;
        await ML.bindDonorOwnCopies();
        assert.strictEqual(puts().length, 0, 'при чтении не должно быть записи');
        assert.strictEqual(cb.checked, false, 'тумблер не отражает effective=0');
        assert.strictEqual(cb.disabled, false, 'администратору тумблер обязан быть доступен');
        assert.ok(badge.textContent.indexOf('по умолчанию') >= 0, 'бейдж не называет источник: ' + badge.textContent);
        assert.ok(badge.textContent.indexOf('выкл') >= 0, 'бейдж не называет действующее значение: ' + badge.textContent);
        assert.strictEqual(resetBtn.disabled, true, 'сбрасывать нечего (источник default) — кнопка обязана быть погашена');
    });

    await check('переключение: PUT {"name","value":1} и строка из ОТВЕТА сервера', async function () {
        calls.length = 0;
        isAdmin = true;
        const ok = await ML.setDonorOwnCopies(true);
        assert.strictEqual(ok, true, 'setDonorOwnCopies не подтвердил успех');
        assert.strictEqual(puts().length, 1, 'ожидался ровно один PUT /api/v1/settings');
        assert.deepStrictEqual(puts()[0].body, { name: NAME, value: 1 },
            'bool обязан уходить как 1/0, а не true/false: ' + JSON.stringify(puts()[0].body));
        assert.strictEqual(cb.checked, true, 'тумблер не обновился из ответа');
        assert.ok(badge.textContent.indexOf('задано в панели') >= 0, 'источник не стал override: ' + badge.textContent);
        assert.strictEqual(resetBtn.disabled, false, 'у override сброс обязан быть доступен');
        assert.ok(msg.textContent.length === 0 || /[Пп]рименено/.test(msg.textContent),
            'панель не подтвердила применение: ' + msg.textContent);
    });

    await check('состояние ОДНО: панель видит ту же запись реестра, что и страница настроек', function () {
        const entry = SR.get(NAME);
        assert.ok(entry, 'settings-registry.get() не отдаёт строку — панель держала бы свою копию');
        assert.strictEqual(entry.source, 'override', 'источник в реестре не совпал с панелью');
        assert.strictEqual(entry.effective, 1, 'effective в реестре не совпал с панелью');
    });

    await check('«Сбросить»: POST reset {"name"} и перечитывание реестра (GET после POST)', async function () {
        calls.length = 0;
        const ok = await ML.resetDonorOwnCopies();
        assert.strictEqual(ok, true, 'resetDonorOwnCopies не подтвердил успех');
        assert.strictEqual(resets().length, 1, 'ожидался ровно один POST /api/v1/settings/reset');
        assert.deepStrictEqual(resets()[0].body, { name: NAME }, 'reset ушёл не с {"name"}');
        const order = calls.filter(function (c) { return c.path.indexOf('/api/v1/settings') === 0; })
            .map(function (c) { return c.method; });
        assert.deepStrictEqual(order, ['POST', 'GET'],
            'после сброса панель обязана перечитать реестр: ' + order.join(','));
        assert.strictEqual(cb.checked, false, 'после сброса тумблер обязан вернуться в «выкл»');
        assert.ok(badge.textContent.indexOf('по умолчанию') >= 0, 'после сброса источник обязан стать default: ' + badge.textContent);
        assert.strictEqual(resetBtn.disabled, true, 'после сброса сбрасывать нечего');
        assert.ok(/[Сс]брошено/.test(msg.textContent), 'оператор не видит подтверждения сброса: ' + msg.textContent);
    });

    await check('viewer: тумблер и сброс погашены, запрос НЕ уходит, причина названа', async function () {
        isAdmin = false;
        calls.length = 0;
        ML.renderDonorOwnCopies();
        assert.strictEqual(cb.disabled, true, 'viewer не должен иметь живой тумблер (сервер ответит 403)');
        assert.ok(/администратор/i.test(cb.title), 'причина недоступности не названа: ' + cb.title);
        assert.strictEqual(resetBtn.disabled, true, 'сброс у viewer обязан быть погашен');
        const ok = await ML.setDonorOwnCopies(true);
        assert.strictEqual(ok, false, 'viewer не должен получать «успех»');
        assert.strictEqual(puts().length, 0, 'запрос от viewer ушёл на сервер (лишний 403)');
        assert.ok(/администратор/i.test(msg.textContent), 'viewer не видит, почему менять нельзя: ' + msg.textContent);
        isAdmin = true;
    });

    await check('без модуля реестра (старый образ): честное «недоступно», без падения', async function () {
        const saved = global.SettingsRegistry;
        // Панель обязана уметь без него: на старом образе monitor.html может
        // прийти без нового registry — тумблер не должен выглядеть рабочим.
        delete global.SettingsRegistry;
        try {
            const res = await ML.loadDonorOwnCopies(true);
            assert.strictEqual(res, null);
            assert.ok(/недоступ/i.test(msg.textContent), 'нет честного «недоступно»: ' + msg.textContent);
        } finally {
            global.SettingsRegistry = saved;
        }
    });

    await check('все ключи панели переведены (ru.js/en.js), сырых ключей нет', function () {
        assert.deepStrictEqual(missingKeys, [], 'ключи без перевода: ' + missingKeys.join(', '));
        const used = [
            'monitor.layout.donorOwnCopies.label',
            'monitor.layout.donorOwnCopies.hint',
            'monitor.layout.donorOwnCopies.adminOnly',
            'monitor.layout.donorOwnCopies.unavailable',
            'monitor.layout.donorOwnCopies.missing',
            'monitor.layout.donorOwnCopies.saved',
            'monitor.layout.donorOwnCopies.resetDone',
            'monitor.layout.donorOwnCopies.failed',
            'monitor.layout.donorOwnCopies.reset',
        ];
        used.forEach(function (k) {
            assert.ok(RU[k] !== undefined, 'нет в ru.js: ' + k);
            assert.ok(EN[k] !== undefined, 'нет в en.js: ' + k);
        });
    });

    console.log('\nmonitor-donor-copies: ' + passed + ' проверок пройдено' +
        (failures.length ? ', ПРОВАЛОВ: ' + failures.length : ''));
    if (failures.length) { process.exit(1); }
})().catch(function (e) {
    console.error('monitor-donor-copies-r317.test.js упал вне проверки:', e);
    process.exit(1);
});

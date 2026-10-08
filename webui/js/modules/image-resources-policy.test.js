// image-resources-policy.test.js — R84 (2026-10-03): форма политики
// сосуществования image-генерации с текстом (балансер: balancing.image).
//
// Запуск: node webui/js/modules/image-resources-policy.test.js
//
// ЧТО ПРОВЕРЯЕМ:
//   1. чтение действующих значений из ответа API (effective → форма) и подсказок
//      политик, которые приходят С СЕРВЕРА (UI не дублирует тексты);
//   2. клиентская валидация ДО отправки и её границы из limits сервера;
//   3. предупреждения: offload (нужен реальный offload воркера), dedicated,
//      «гейт выключен» — оператор обязан видеть риск, а не только поля;
//   4. отправка: PUT с телом формы, DELETE для сброса, отдельный путь для ошибок;
//   5. сводка «действует сейчас» отражает все непустые поля.
'use strict';

const assert = require('assert');

// --- mocks ------------------------------------------------------------------

function makeElement(id) {
    const listeners = {};
    const el = {
        id: id,
        style: {},
        value: '',
        checked: false,
        innerHTML: '',
        textContent: '',
        hidden: false,
        getAttribute: function () { return null; },
        querySelector: function () { return null; },
        setAttribute: function () { },
        addEventListener: function (type, fn) { (listeners[type] = listeners[type] || []).push(fn); },
        dispatch: function (type, ev) { (listeners[type] || []).forEach(function (fn) { fn(ev || {}); }); },
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
};
global.WEBUI_CONFIG = { API_BASE: 'http://balancer.test:18081', API_TOKEN: 'test-token' };
global.Api = { getAuthHeaders: function () { return { 'Content-Type': 'application/json', 'X-API-Token': 'test-token' }; } };

const RU = {
    'imagePolicy.warn_offload': 'Политика offload разрешает совместную работу с текстом.',
    'imagePolicy.warn_dedicated': 'Политика dedicated снимает ограничения.',
    'imagePolicy.warn_gate_off': 'Гейт VRAM выключен.',
    'imagePolicy.err_headroom': 'Резерв VRAM: 0…{max} МБ',
    'imagePolicy.err_wait': 'Ожидание GPU: 0…{max} с',
    'imagePolicy.err_fuse': 'Предохранитель лока: 0…{max} с',
    'imagePolicy.err_policy': 'Неизвестная политика: {value}',
    'imagePolicy.current': 'Действует сейчас',
    'imagePolicy.file': 'файл',
    'imagePolicy.headroom': 'Резерв VRAM, МБ',
};
global.I18N = {
    t: function (k, vars) {
        var s = Object.prototype.hasOwnProperty.call(RU, k) ? RU[k] : k;
        if (vars) Object.keys(vars).forEach(function (p) { s = String(s).replace('{' + p + '}', vars[p]); });
        return s;
    },
};

const requests = [];
let responseFor = function () { return { status: 200, body: {} }; };
global.fetch = function (url, init) {
    const rec = { url: String(url), method: (init && init.method) || 'GET', body: init && init.body ? JSON.parse(init.body) : null };
    requests.push(rec);
    const r = responseFor(rec);
    return Promise.resolve({
        ok: r.status < 400,
        status: r.status,
        text: function () { return Promise.resolve(r.body === null ? '' : JSON.stringify(r.body)); },
    });
};

const toasts = [];
// Мок страницы бэкендов: после сохранения политики её колонка обязана обновиться
// принудительно (у неё TTL кэш), иначе оператор видит старое значение.
const policyReloads = [];
global.ImageBackendsPage = { _actions: { loadPolicy: function (force) { policyReloads.push(force); } } };
global.Toast = { show: function (o) { toasts.push(o || {}); } };

require('./image-resources-policy.js');
const P = global.ImageResourcesPolicy;
assert.ok(P, 'window.ImageResourcesPolicy должен быть экспортирован (см. check_iife_exports.py)');

// --- раннер -----------------------------------------------------------------

let passed = 0;
const failures = [];
async function check(name, fn) {
    try {
        await fn();
        passed++;
        console.log('  \u2713 ' + name);
    } catch (e) {
        failures.push(name + ': ' + (e && e.message));
        console.error('  \u2717 ' + name + ' — ' + (e && e.message));
    }
}

const API_RESPONSE = {
    effective: {
        coexistence: 'exclusive',
        vramHeadroomMb: 512,
        queueWaitTimeoutSec: 30,
        exclusiveLockTimeoutSec: 600,
        blockOnUnknownVramEstimate: false,
        gateDisabled: false,
    },
    defaults: { coexistence: 'exclusive' },
    overridden: false,
    source: 'config',
    path: '/app/data/image-resources.json',
    limits: { maxVramHeadroomMb: 65536, maxQueueWaitTimeoutSec: 3600, maxExclusiveLockFuseSec: 86400,
        minToolLoadTimeoutSec: 1, maxToolLoadTimeoutSec: 86400 },
    // R86-follow-up: ожидание загрузки модели из вызова инструмента.
    toolLoadTimeout: { effectiveSec: 600, overridden: false, env: 'LB_IMAGE_TOOL_LOAD_TIMEOUT_SEC', hint: 'сколько ждать' },
    allowToolLoad: { effective: true, overridden: false, env: 'LB_IMAGE_TOOL_ALLOW_LOAD', hint: 'автозагрузка' },
    policies: [
        { value: 'exclusive', hint: 'владеет картой' },
        { value: 'offload', hint: 'нужен offload воркера' },
        { value: 'dedicated', hint: 'отдельная GPU' },
    ],
};

(async function main() {
    // --- pure -----------------------------------------------------------------

    await check('formFromEffective: значения API попадают в форму, пустые поля — в 0/false', function () {
        const f = P.pure.formFromEffective(API_RESPONSE);
        assert.strictEqual(f.coexistence, 'exclusive');
        assert.strictEqual(f.vramHeadroomMb, 512);
        assert.strictEqual(f.queueWaitTimeoutSec, 30);
        assert.strictEqual(f.exclusiveLockTimeoutSec, 600);
        assert.strictEqual(f.blockOnUnknownVramEstimate, false);
        assert.strictEqual(f.gateDisabled, false);
        // R85: незаданное значение = автозагрузка разрешена (дефолт on).
        assert.strictEqual(f.allowToolLoad, true, 'allowToolLoad по умолчанию должен быть включён');
        // Явный false из конфига обязан снимать галочку.
        const off = P.pure.formFromEffective({ effective: { allowToolLoad: false } });
        assert.strictEqual(off.allowToolLoad, false);
        // Пустой ответ не должен давать NaN: форма обязана остаться валидной.
        const empty = P.pure.formFromEffective({});
        assert.strictEqual(empty.coexistence, 'exclusive');
        assert.strictEqual(empty.vramHeadroomMb, 0);
    });

    await check('policyHint: подсказка берётся из ответа сервера', function () {
        assert.strictEqual(P.pure.policyHint(API_RESPONSE, 'offload'), 'нужен offload воркера');
        assert.strictEqual(P.pure.policyHint(API_RESPONSE, 'nonsense'), '');
        assert.strictEqual(P.pure.policyHint(null, 'exclusive'), '');
    });

    await check('validate: границы берутся из limits сервера', function () {
        const limits = { maxVramHeadroomMb: 4096, maxQueueWaitTimeoutSec: 120, maxExclusiveLockFuseSec: 900,
            minToolLoadTimeoutSec: 1, maxToolLoadTimeoutSec: 7200 };
        const ok = P.pure.validate({ coexistence: 'exclusive', vramHeadroomMb: 4096, queueWaitTimeoutSec: 120, exclusiveLockTimeoutSec: 900, toolLoadTimeoutSec: 7200 }, limits);
        assert.strictEqual(ok, '', 'граничные значения валидны');
        // Каждый кейс: все поля валидны, кроме одного — иначе ошибка прилетит от
        // первого же поля, и проверка поймает не то сообщение.
        const base = { coexistence: 'exclusive', vramHeadroomMb: 0, queueWaitTimeoutSec: 0, exclusiveLockTimeoutSec: 0, toolLoadTimeoutSec: 600 };
        assert.ok(/4096/.test(P.pure.validate(Object.assign({}, base, { vramHeadroomMb: 4097 }), limits)), 'превышение headroom');
        assert.ok(/120/.test(P.pure.validate(Object.assign({}, base, { queueWaitTimeoutSec: 121 }), limits)), 'превышение wait');
        assert.ok(/900/.test(P.pure.validate(Object.assign({}, base, { exclusiveLockTimeoutSec: 901 }), limits)), 'превышение fuse');
        assert.ok(/nonsense/.test(P.pure.validate(Object.assign({}, base, { coexistence: 'nonsense' }), limits)), 'неизвестная политика');
        assert.ok(P.pure.validate(Object.assign({}, base, { vramHeadroomMb: -1 }), limits) !== '', 'отрицательный резерв невалиден');
        // R86-follow-up: ожидание загрузки модели — 0 («не ждать») и превышение
        // верхней границы отклоняются, границы берутся из limits сервера.
        assert.ok(/7200/.test(P.pure.validate(Object.assign({}, base, { toolLoadTimeoutSec: 7201 }), limits)), 'превышение ожидания загрузки');
        assert.ok(/1/.test(P.pure.validate(Object.assign({}, base, { toolLoadTimeoutSec: 0 }), limits)), 'нулевое ожидание невалидно');
    });

    await check('warningFor: offload/dedicated/гейт-выкл предупреждают, exclusive — нет', function () {
        assert.strictEqual(P.pure.warningFor({ coexistence: 'exclusive' }), '');
        assert.ok(/offload/.test(P.pure.warningFor({ coexistence: 'offload' })));
        assert.ok(/dedicated/.test(P.pure.warningFor({ coexistence: 'dedicated' })));
        assert.ok(/Гейт VRAM/.test(P.pure.warningFor({ coexistence: 'exclusive', gateDisabled: true })));
    });

    await check('summaryText: сводка отражает все непустые поля', function () {
        const s = P.pure.summaryText(API_RESPONSE);
        assert.ok(s.indexOf('exclusive') !== -1 && s.indexOf('headroom 512 MB') !== -1, s);
        assert.ok(s.indexOf('wait 30s') !== -1 && s.indexOf('fuse 600s') !== -1, s);
        assert.strictEqual(P.pure.summaryText({ effective: { coexistence: 'exclusive', vramHeadroomMb: 0, queueWaitTimeoutSec: 0, exclusiveLockTimeoutSec: 0 } }), 'exclusive');
    });

    await check('escapeHtml: подписи политик экранируются', function () {
        assert.strictEqual(P.pure.escapeHtml('<b>"x"</b>'), '&lt;b&gt;&quot;x&quot;&lt;/b&gt;');
        assert.strictEqual(P.pure.escapeHtml(null), '');
    });

    // --- сеть -----------------------------------------------------------------

    await check('load: GET /api/v1/image/resources и рендер формы', async function () {
        requests.length = 0;
        responseFor = function () { return { status: 200, body: API_RESPONSE }; };
        P.mount();
        await P._actions.load(true);
        const rec = requests.filter(function (r) { return r.url.indexOf('/api/v1/image/resources') !== -1; })[0];
        assert.ok(rec, 'запрос политики не ушёл');
        assert.strictEqual(rec.method, 'GET');
        assert.ok(rec.url.indexOf('http://balancer.test:18081') === 0, 'неверный base URL: ' + rec.url);
        const html = getEl('imPolicyHost').innerHTML;
        assert.ok(html.indexOf('imPolicyCoexistence') !== -1, 'нет селекта политики');
        assert.ok(html.indexOf('imPolicyHeadroom') !== -1, 'нет поля резерва VRAM');
        assert.ok(html.indexOf('imPolicyGateOff') !== -1, 'нет чекбокса «выключить гейт»');
        assert.ok(html.indexOf('imPolicyAllowLoad') !== -1, 'нет чекбокса «разрешить инструменту загружать модель» (R85)');
        assert.ok(html.indexOf('владеет картой') !== -1, 'нет подсказки сервера для выбранной политики');
        assert.strictEqual(getEl('imPolicyBadge').style.display, 'none', 'без переопределения бейджа быть не должно');
    });

    await check('save: PUT с телом формы и обновление бейджа', async function () {
        requests.length = 0;
        responseFor = function () { return { status: 200, body: Object.assign({}, API_RESPONSE, { overridden: true, source: 'override', effective: Object.assign({}, API_RESPONSE.effective, { coexistence: 'offload', vramHeadroomMb: 1024 }) }) }; };
        getEl('imPolicyCoexistence').value = 'offload';
        getEl('imPolicyHeadroom').value = '1024';
        getEl('imPolicyWait').value = '30';
        getEl('imPolicyFuse').value = '600';
        getEl('imPolicyBlockUnknown').checked = true;
        getEl('imPolicyGateOff').checked = false;
        getEl('imPolicyAllowLoad').checked = false;
        // R86-follow-up: оператор поднял ожидание загрузки модели (большая модель
        // на медленном диске) — значение обязано уйти в теле PUT.
        getEl('imPolicyLoadTimeout').value = '1800';
        const ok = await P._actions.save();
        assert.strictEqual(ok, true);
        const rec = requests.filter(function (r) { return r.method === 'PUT'; })[0];
        assert.ok(rec, 'PUT не ушёл');
        assert.strictEqual(rec.body.coexistence, 'offload');
        assert.strictEqual(rec.body.vramHeadroomMb, 1024);
        assert.strictEqual(rec.body.blockOnUnknownVramEstimate, true);
        // Снятая галочка автозагрузки обязана уйти как явный false (иначе сервер
        // не отличит «выключил» от «поля нет» и оставит прежнее значение).
        assert.strictEqual(rec.body.allowToolLoad, false, 'allowToolLoad должен уходить в теле PUT');
        assert.strictEqual(rec.body.toolLoadTimeoutSec, 1800, 'ожидание загрузки должно уходить в теле PUT');
        assert.strictEqual(getEl('imPolicyBadge').style.display, '', 'после сохранения бейдж переопределения виден');
        assert.ok(toasts.some(function (x) { return /сохранена/.test(x.message || ''); }), 'нет тоста об успехе');
        assert.deepStrictEqual(policyReloads, [true], 'колонка политики должна перечитаться принудительно (force)');
    });

    await check('save: невалидное значение не уходит на сервер', async function () {
        requests.length = 0;
        getEl('imPolicyCoexistence').value = 'nonsense';
        const ok = await P._actions.save();
        assert.strictEqual(ok, false);
        assert.strictEqual(requests.filter(function (r) { return r.method === 'PUT'; }).length, 0, 'невалидное значение не должно отправляться');
        assert.ok(/nonsense/.test(getEl('imPolicyHost').innerHTML), 'ошибка должна быть видна в форме');
        getEl('imPolicyCoexistence').value = 'exclusive';
    });

    await check('reset: DELETE и возврат встроенных значений', async function () {
        requests.length = 0;
        responseFor = function () { return { status: 200, body: API_RESPONSE }; };
        const ok = await P._actions.reset();
        assert.strictEqual(ok, true);
        const rec = requests.filter(function (r) { return r.method === 'DELETE'; })[0];
        assert.ok(rec, 'DELETE не ушёл');
        assert.strictEqual(getEl('imPolicyBadge').style.display, 'none', 'после сброса бейджа быть не должно');
        assert.ok(toasts.some(function (x) { return /встроенные/.test(x.message || ''); }), 'нет тоста о сбросе');
    });

    await check('ошибка сервера показывается в форме и тостом', async function () {
        requests.length = 0;
        responseFor = function () { return { status: 400, body: { error: 'unknown coexistence policy "bad"' } }; };
        const ok = await P._actions.save();
        assert.strictEqual(ok, false);
        assert.ok(getEl('imPolicyHost').innerHTML.indexOf('unknown coexistence policy') !== -1, 'текст ошибки сервера не показан');
        assert.ok(toasts.some(function (x) { return /Не удалось сохранить/.test(x.message || ''); }), 'нет тоста об ошибке');
    });


    await check('change в форме не сбрасывает выбор оператора (живой дефект)', async function () {
        requests.length = 0;
        responseFor = function () { return { status: 200, body: API_RESPONSE }; };
        await P._actions.load(true);
        getEl('imPolicyCoexistence').value = 'offload';
        // Событие change (его шлёт браузер при выборе в селекте) обязано обновить
        // только подсказку/предупреждение, а не перерисовать форму из старых данных.
        getEl('imPolicyHost').dispatch('change', {});
        assert.strictEqual(getEl('imPolicyCoexistence').value, 'offload', 'выбор сброшен перерисовкой формы');

        responseFor = function () { return { status: 200, body: Object.assign({}, API_RESPONSE, { overridden: true, effective: Object.assign({}, API_RESPONSE.effective, { coexistence: 'offload' }) }) }; };
        await P._actions.save();
        const rec = requests.filter(function (r) { return r.method === 'PUT'; })[0];
        assert.ok(rec, 'PUT не ушёл');
        assert.strictEqual(rec.body.coexistence, 'offload', 'на сервер ушло не то, что выбрал оператор');
        getEl('imPolicyCoexistence').value = 'exclusive';
    });

    // Живая жалоба: «постоянно обновляется форма настроек, приходится успевать
    // нажать Сохранить». Страница перерисовывает табы на каждом тике обновления
    // (renderTabs → ImageResourcesPolicy.render), а render() собирает форму заново
    // из state.data — то есть возвращает сохранённые значения прямо под руками.
    await check('периодический render НЕ стирает ввод, пока оператор правит форму', async function () {
        requests.length = 0;
        responseFor = function () { return { status: 200, body: API_RESPONSE }; };
        await P._actions.load(true);

        // Оператор набирает таймаут ожидания загрузки.
        getEl('imPolicyLoadTimeout').value = '1234';
        getEl('imPolicyHost').dispatch('input', {});

        // Три тика автообновления страницы подряд.
        P.render({ backendId: 'img-1', tab: 'overview' });
        P.render({ backendId: 'img-1', tab: 'overview' });
        P.render({ backendId: 'img-1', tab: 'overview' });

        assert.strictEqual(getEl('imPolicyLoadTimeout').value, '1234',
            'ввод стёрт автообновлением формы');
        assert.strictEqual(P._state.dirty, true, 'форма должна считаться «правленой»');
        assert.strictEqual(getEl('imPolicyDirty').hidden, false, 'нет маркера о несохранённых правках');

        // Сохранение применяет введённое и снимает флаг.
        responseFor = function () {
            return {
                status: 200,
                body: Object.assign({}, API_RESPONSE, {
                    toolLoadTimeout: { configuredSec: 1234, effectiveSec: 1234, source: 'config', env: '' }
                })
            };
        };
        await P._actions.save();
        const put = requests.filter(function (r) { return r.method === 'PUT'; })[0];
        assert.ok(put, 'PUT не ушёл');
        assert.strictEqual(put.body.toolLoadTimeoutSec, 1234, 'на сервер ушло не то, что ввёл оператор');
        assert.strictEqual(P._state.dirty, false, 'после сохранения флаг должен сняться');
    });

    // Переключили вкладку и вернулись: панель вкладки пересобрана, наших полей в
    // DOM больше нет, поэтому «правленая» форма не должна блокировать отрисовку
    // навсегда. Мок создаёт элементы по требованию, поэтому потерю DOM
    // эмулируем явно: getElementById перестаёт отдавать поля формы.
    await check('после потери DOM флаг правки сбрасывается и форма рисуется снова', async function () {
        requests.length = 0;
        responseFor = function () { return { status: 200, body: API_RESPONSE }; };
        await P._actions.load(true);
        getEl('imPolicyLoadTimeout').value = '999';
        getEl('imPolicyHost').dispatch('input', {});
        assert.strictEqual(P._state.dirty, true, 'подготовка: форма должна считаться правленой');

        const realGetById = global.document.getElementById;
        global.document.getElementById = function (id) {
            // Панель вкладки заменена целиком: полей формы в документе нет,
            // а хост отрисовки — новый пустой элемент.
            if (id === 'imPolicyCoexistence' || id === 'imPolicyLoadTimeout') return null;
            return realGetById(id);
        };
        getEl('imPolicyHost').innerHTML = '';
        try {
            P.render({ backendId: 'img-1', tab: 'overview' });
        } finally {
            global.document.getElementById = realGetById;
        }

        assert.strictEqual(P._state.dirty, false, 'флаг должен сброситься, иначе форма не отрисуется уже никогда');
        assert.ok(getEl('imPolicyHost').innerHTML.length > 0, 'форма должна быть отрисована заново');
    });

    console.log('\n' + (failures.length ? 'FAILED: ' + failures.length : 'ИТОГ: все проверки пройдены (' + passed + ')'));
    if (failures.length) failures.forEach(function (f) { console.error(' - ' + f); });
    process.exit(failures.length ? 1 : 0);
})();

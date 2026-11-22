// image-role-label-r695.test.js — R695: подпись роли `vae` и подсказка
// «(предложено: …)» на табе Image-модели → HuggingFace.
//
// ДВА СМЕЖНЫХ ДЕФЕКТА, один источник.
//
// ДЕФЕКТ 1 (сырой id у роли vae). В селекте роли файла 11 ролей из 12 показывали
// человеческую подпись из i18n («CLIP-L (текстовый энкодер)», «T5-XXL (… текстовый
// энкодер FLUX/SD3)»), а `vae` — сырое «vae»: значение пакета
// `imageModels.role_vae` СОВПАДАЛО с самим id (R694 добавлял ключи десяти ролям и
// сознательно не менял два существующих). То же значение видел и таб «Модели на
// диске» (image-page.js:657 собирает ключ динамически). Для оператора это не
// подпись: смысл роли — декодер латентов в пиксели (pkg/types/image_model.go:34,
// флаг --vae), у FLUX/SD3/Qwen/Z-Image он лежит отдельным файлом ae.safetensors.
//
// ДЕФЕКТ 2 (подсказка серверным id). Рядом с селектом при расхождении выбора и
// серверной подсказки выводилось «(предложено: clip_l)» — это `suggestedRole`
// из GET /api/hf/files, то есть технический id, хотя селект рядом уже показывал
// человеческую подпись. Теперь подсказка берёт подпись из ТОЙ ЖЕ статической
// карты ROLE_LABEL_KEYS (никакой склейки 'imageModels.role_' + role), а роль вне
// карты показывается id как есть — без падения и без «undefined».
//
// ПОЧЕМУ ЗНАЧЕНИЯ БЕРУТСЯ ИЗ НАСТОЯЩИХ ПАКЕТОВ. Тест с самодельным словарём был
// бы вакуумным: он проходил бы и на коде, где ключ не переведён. Здесь ru/en
// читаются из webui/js/i18n/*.js, а строки селекта и подсказки — из реального
// рендера модуля после реального GET /hf/files.
//
// НЕ-ВАКУУМНОСТЬ. `WEBUI_ROOT=<старая статика>` (например копия
// `git show 4613df08:webui`) обязан дать КРАСНЫЙ результат: старого значения «vae»
// в пакетах и подсказки с подписью там нет.
//
// Запуск: node webui/tests/image-role-label-r695.test.js
//         WEBUI_ROOT=<dir> node webui/tests/image-role-label-r695.test.js
'use strict';

const assert = require('assert');
const path = require('path');

// WEBUI_ROOT — корень статики под проверкой (по умолчанию — webui/ репозитория).
// Нужен ровно для доказательства, что набор краснеет на СТАРОМ коде, а не только
// зеленеет на новом.
const WEBUI = process.env.WEBUI_ROOT
    ? path.resolve(process.env.WEBUI_ROOT)
    : path.join(__dirname, '..');

let failures = 0;
let passed = 0;
function check(name, fn) {
    try { fn(); passed++; console.log('  \u2713 ' + name); }
    catch (e) { failures++; console.error('  \u2717 ' + name + '\n    ' + e.message); }
}
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// --- mocks: DOM -------------------------------------------------------------

function makeNode(tag, opts) {
    opts = opts || {};
    const attrs = Object.assign({}, opts.attrs || {});
    const listeners = {};
    const node = {
        tagName: String(tag || 'div').toUpperCase(),
        id: opts.id || '',
        style: {},
        value: '',
        checked: false,
        textContent: '',
        parentNode: opts.parentNode || null,
        nextSibling: opts.nextSibling || null,
        childNodes: [],
        _html: '',
        _htmlSets: 0,
        getAttribute: function (n) { return Object.prototype.hasOwnProperty.call(attrs, n) ? attrs[n] : null; },
        setAttribute: function (n, v) { attrs[n] = v; },
        hasAttribute: function (n) { return Object.prototype.hasOwnProperty.call(attrs, n); },
        addEventListener: function (t, fn) { (listeners[t] = listeners[t] || []).push(fn); },
        removeEventListener: function (t, fn) {
            const a = listeners[t] || [];
            const i = a.indexOf(fn);
            if (i !== -1) a.splice(i, 1);
        },
        dispatch: function (t, ev) { (listeners[t] || []).slice().forEach(function (fn) { fn(ev || {}); }); },
        dispatchEvent: function (ev) { return node.dispatch(ev && ev.type, ev); },
        classList: {
            _set: new Set(),
            add: function (c) { this._set.add(c); },
            remove: function (c) { this._set.delete(c); },
            contains: function (c) { return this._set.has(c); },
            toggle: function (c, on) { if (on === undefined) { this._set.has(c) ? this._set.delete(c) : this._set.add(c); } else if (on) { this._set.add(c); } else { this._set.delete(c); } },
        },
        appendChild: function (child) {
            if (child.parentNode && typeof child.parentNode.removeChild === 'function') child.parentNode.removeChild(child);
            child.parentNode = node;
            node.childNodes.push(child);
            return child;
        },
        removeChild: function (child) {
            const i = node.childNodes.indexOf(child);
            if (i !== -1) node.childNodes.splice(i, 1);
            if (child.parentNode === node) child.parentNode = null;
            return child;
        },
        insertBefore: function (child, ref) {
            if (child.parentNode && typeof child.parentNode.removeChild === 'function') child.parentNode.removeChild(child);
            const i = ref ? node.childNodes.indexOf(ref) : -1;
            if (i === -1) node.childNodes.push(child); else node.childNodes.splice(i, 0, child);
            child.parentNode = node;
            return child;
        },
        contains: function (other) {
            let n = other;
            while (n) { if (n === node) return true; n = n.parentNode; }
            return false;
        },
        querySelector: function () { return null; },
        querySelectorAll: function () { return []; },
    };
    Object.defineProperty(node, 'innerHTML', {
        get: function () { return this._html; },
        set: function (v) { this._html = String(v); this._htmlSets++; },
    });
    return node;
}

const elements = {};
function getEl(id) {
    if (!Object.prototype.hasOwnProperty.call(elements, id)) elements[id] = makeNode('div', { id: id });
    return elements[id];
}

const tabHost = makeNode('div', { attrs: { id: 'imTabHf' } });
const filesCard = makeNode('div', { id: 'imHfFilesCard' });
tabHost.appendChild(filesCard);
const filesBody = makeNode('div', { id: 'imHfFilesBody' });
filesCard.appendChild(filesBody);
elements['imHfFilesCard'] = filesCard;
elements['imHfFilesBody'] = filesBody;

const slots = {};
function slotFor(repo) {
    if (!slots[repo]) slots[repo] = makeNode('div', { attrs: { 'data-imh-files-slot': repo } });
    return slots[repo];
}

const winListeners = {};
const documentElement = { attrs: {}, setAttribute: function (k, v) { this.attrs[k] = v; } };

global.window = global;
global.addEventListener = function (t, fn) { (winListeners[t] = winListeners[t] || []).push(fn); };
global.removeEventListener = function (t, fn) {
    const a = winListeners[t] || [];
    const i = a.indexOf(fn);
    if (i !== -1) a.splice(i, 1);
};
global.dispatchEvent = function (ev) {
    (winListeners[ev && ev.type] || []).slice().forEach(function (fn) { fn(ev); });
    return true;
};
global.document = {
    documentElement: documentElement,
    getElementById: getEl,
    querySelector: function (sel) {
        const m = /^\[data-imh-files-slot="(.*)"\]$/.exec(String(sel || ''));
        if (!m) return null;
        const repo = m[1].replace(/\\(["\\])/g, '$1');
        const html = getEl('imHfSearchResults').innerHTML;
        if (html.indexOf('data-imh-files-slot="' + repo + '"') === -1) return null;
        return slotFor(repo);
    },
    querySelectorAll: function () { return []; },
    createElement: function (tag) { return makeNode(tag); },
    addEventListener: function () { },
};
const store = {};
global.localStorage = {
    getItem: function (k) { return Object.prototype.hasOwnProperty.call(store, k) ? store[k] : null; },
    setItem: function (k, v) { store[k] = String(v); },
    removeItem: function (k) { delete store[k]; },
};
global.WEBUI_CONFIG = { API_BASE: 'http://balancer.test:18081', API_TOKEN: 'test-token' };
global.Api = { getAuthHeaders: function () { return { 'Content-Type': 'application/json', 'X-API-Token': 'test-token' }; } };

// --- НАСТОЯЩИЕ пакеты и НАСТОЯЩИЙ i18n -------------------------------------

require(path.join(WEBUI, 'js', 'i18n', 'ru.js'));
require(path.join(WEBUI, 'js', 'i18n', 'en.js'));
require(path.join(WEBUI, 'js', 'i18n', 'index.js'));
const RU_PACK = global.I18N_RU;
const EN_PACK = global.I18N_EN;
assert.ok(RU_PACK && EN_PACK, 'пакеты i18n не загрузились из ' + WEBUI);
const I18N = global.I18N;
assert.ok(I18N && typeof I18N.setLang === 'function', 'I18N не инициализировался');

// --- mocks: fetch -----------------------------------------------------------

const requests = [];
// Ровно те файлы, на которых виден дефект: vae (подпись), clip_l (подпись),
// diffusion (подпись) и роль ВНЕ контракта — проверка «не падаем, показываем id».
const FILES = [
    { path: 'flux/flux1-dev-Q4_0.gguf', sizeBytes: 6000000000, suggestedRole: 'diffusion' },
    { path: 'flux/ae.safetensors', sizeBytes: 300000000, suggestedRole: 'vae' },
    { path: 'flux/clip_l.safetensors', sizeBytes: 246000000, suggestedRole: 'clip_l' },
    { path: 'flux/mystery.safetensors', sizeBytes: 1000, suggestedRole: 'mystery_role' },
];
global.fetch = function (url, init) {
    const rec = { url: String(url), method: (init && init.method) || 'GET' };
    requests.push(rec);
    const body = (function () {
        if (rec.url.indexOf('/hf') === -1) return {};
        if (rec.url.indexOf('/hf/search') !== -1) {
            return {
                count: 1,
                results: [{ id: 'leejet/FLUX.1-schnell-gguf', downloads: 10, likes: 2, lastModified: '2026-11-01' }],
            };
        }
        if (rec.url.indexOf('/hf/files') !== -1) return { count: FILES.length, files: FILES };
        if (rec.url.indexOf('/hf/plan') !== -1) return { verdict: 'unknown', roles: [], steps: [] };
        if (rec.url.indexOf('/hf/probe') !== -1) return { verdict: 'unknown', reason: 'not read' };
        return {};
    })();
    return Promise.resolve({ ok: true, status: 200, text: function () { return Promise.resolve(JSON.stringify(body)); } });
};

require(path.join(WEBUI, 'js', 'modules', 'image-models-hf.js'));
const Hf = global.ImageModelsHf;
assert.ok(Hf, 'window.ImageModelsHf должен быть экспортирован');
const P = Hf.pure;

const ctx = { apiBase: 'http://balancer.test:18081', backendId: 'img-1', tab: 'hf', headers: {} };
const REPO = 'leejet/FLUX.1-schnell-gguf';

function clickButton(repo) {
    const btn = makeNode('button', { attrs: { 'data-imh-pick': repo }, parentNode: slotFor(repo) });
    getEl('image-page').dispatch('click', { target: btn });
}
function filesHtml() { return filesBody.innerHTML; }
function optionLabel(html, role) {
    const re = new RegExp('<option value="' + role + '"[^>]*>([^<]*)</option>');
    const m = re.exec(html);
    return m ? m[1] : null;
}

(async function main() {
    I18N.setLang('ru');
    assert.strictEqual(I18N.getLang(), 'ru');

    Hf.render(ctx);
    getEl('imHfQuery').value = 'flux';
    await Hf._actions.search();
    await sleep(20);
    clickButton(REPO);
    await sleep(80); // /hf/files + /hf/plan + автопред-проверка главного файла

    // --- ДЕФЕКТ 1: подпись роли vae в пакетах ------------------------------
    check('1a. оба пакета дают роли vae ЧЕЛОВЕЧЕСКУЮ подпись, а не сырой id', function () {
        const ru = RU_PACK['imageModels.role_vae'];
        const en = EN_PACK['imageModels.role_vae'];
        assert.ok(ru, 'нет ru-подписи imageModels.role_vae');
        assert.ok(en, 'нет en-подписи imageModels.role_vae');
        assert.notStrictEqual(ru, 'vae', 'ru-подпись роли vae равна сырому id: ' + ru);
        assert.notStrictEqual(en, 'vae', 'en-подпись роли vae равна сырому id: ' + en);
        assert.notStrictEqual(ru, en, 'ru/en подписи совпали — это не перевод: ' + ru);
        // Смысл роли (pkg/types/image_model.go:34, --vae): автоэнкодер.
        assert.ok(/VAE/.test(ru) && /автоэнкодер/i.test(ru), 'ru-подпись не про автоэнкодер: ' + ru);
        assert.ok(/VAE/.test(en) && /autoencoder/i.test(en), 'en-подпись не про автоэнкодер: ' + en);
    });

    check('1b. НИ ОДНА из 12 ролей не показывается сырым id (общий контракт)', function () {
        assert.strictEqual(P.IMAGE_ROLES.length, 12, 'ролей в контракте не 12: ' + P.IMAGE_ROLES.length);
        P.IMAGE_ROLES.forEach(function (role) {
            assert.notStrictEqual(RU_PACK['imageModels.role_' + role], role, 'ru: сырой id ' + role);
            assert.notStrictEqual(EN_PACK['imageModels.role_' + role], role, 'en: сырой id ' + role);
        });
    });

    check('1c. селект роли (roleOptions) отдаёт подпись пакета для vae', function () {
        const html = P.roleOptions('vae');
        assert.strictEqual(optionLabel(html, 'vae'), RU_PACK['imageModels.role_vae'],
            'подпись vae в селекте не из пакета: ' + optionLabel(html, 'vae'));
        assert.notStrictEqual(optionLabel(html, 'vae'), 'vae', 'в селекте сырой id vae');
        assert.ok(/value="vae" selected/.test(html), 'выбранная роль не отмечена');
    });

    check('1d. в отрисованных строках файлов роль vae — подпись, а не id', function () {
        const html = filesHtml();
        assert.ok(html.indexOf('flux/ae.safetensors') !== -1, 'файл vae не отрисован');
        assert.strictEqual(optionLabel(html, 'vae'), RU_PACK['imageModels.role_vae']);
        assert.notStrictEqual(optionLabel(html, 'vae'), 'vae', 'в строках файлов сырой id vae');
    });

    // --- ДЕФЕКТ 2: подсказка «(предложено: …)» ----------------------------
    check('2a. подсказка берёт подпись ТОЙ ЖЕ карты (vae и clip_l)', function () {
        // Селект выбора у этих файлов ставим на другую роль — только тогда UI
        // показывает расхождение «выбрано ≠ предложено» и подсказку.
        Hf._actions.setSelection('flux/ae.safetensors', { role: 'clip_l' });
        Hf._actions.setSelection('flux/clip_l.safetensors', { role: 'vae' });
        Hf.render(ctx); // перерисовка из state (тот же путь, что i18n:changed)
        const html = filesHtml();
        assert.ok(html.indexOf('(предложено: ' + RU_PACK['imageModels.role_vae'] + ')') !== -1,
            'подсказка для vae не взяла подпись из карты: ' + html.slice(0, 400));
        assert.ok(html.indexOf('(предложено: ' + RU_PACK['imageModels.role_clip_l'] + ')') !== -1,
            'подсказка для clip_l не взяла подпись из карты');
        // Ни одного серверного id в подсказках: ровно это и было дефектом.
        assert.strictEqual(html.indexOf('(предложено: vae)'), -1, 'в подсказке остался серверный id vae');
        assert.strictEqual(html.indexOf('(предложено: clip_l)'), -1, 'в подсказке остался серверный id clip_l');
    });

    check('2b. роль вне карты показывается id как есть (нет падения и «undefined»)', function () {
        Hf._actions.setSelection('flux/mystery.safetensors', { role: 'vae' });
        Hf.render(ctx);
        const html = filesHtml();
        assert.ok(html.indexOf('(предложено: mystery_role)') !== -1,
            'неизвестная серверная роль не показана сырым id: ' + html.slice(0, 400));
        assert.strictEqual(html.indexOf('(предложено: undefined)'), -1, 'в подсказке undefined');
    });

    check('2c. подсказка переводится вместе с интерфейсом (EN — подпись EN)', function () {
        I18N.setLang('en');
        const html = filesHtml();
        assert.ok(html.indexOf('(suggested: ' + EN_PACK['imageModels.role_vae'] + ')') !== -1,
            'после переключения на EN подсказка не на английском: ' + html.slice(0, 400));
        assert.strictEqual(html.indexOf('(suggested: vae)'), -1,
            'после переключения на EN в подсказке остался серверный id vae');
    });

    console.log('\n' + (failures ? 'FAILED: ' + failures : 'OK: ' + passed + ' checks passed'));
    if (failures) process.exit(1);
})().catch((e) => {
    console.error('FATAL', e && e.stack || e);
    process.exit(1);
});

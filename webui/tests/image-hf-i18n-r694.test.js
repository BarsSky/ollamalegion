// image-hf-i18n-r694.test.js — R694: два смежных i18n-дефекта таба
// Image-модели → HuggingFace.
//
// ДЕФЕКТ 1 (сырые id ролей). В селекте роли файла оператор видел `clip_l`,
// `t5xxl`, `ip_adapter` и т.п., потому что ключей `imageModels.role_<role>` не
// было в пакетах ru/en: локальная обёртка t(key, fallback) возвращала fallback —
// сам id. Консоль при этом сыпала `[i18n] Missing translation key:
// imageModels.role_*` на каждый файл × 12 ролей × каждый рендер.
//
// ДЕФЕКТ 2 (карточки не перерисовываются при смене языка). Модуль не слушал
// `i18n:changed`, поэтому после переключения RU/EN выдача HF оставалась на
// прежнем языке до повторного поиска — хотя комментарий в image-page.js:1015
// утверждал обратное.
//
// ЧТО ПРОВЕРЯЕТСЯ (и почему именно так):
//   1. ЗНАЧЕНИЯ БЕРУТСЯ ИЗ НАСТОЯЩИХ пакетов `webui/js/i18n/*.js`, а не из
//      заглушки. Тест с самодельным словарём был бы вакуумным: он проходил бы и
//      на коде, где ключи не добавлены. Здесь селект роли обязан отдать
//      человеческую подпись пакета и НЕ отдать сырой id.
//   2. `i18n:changed` диспатчится настоящим `I18N.setLang` (webui/js/i18n/
//      index.js), а подписка модуля на `window` — настоящая. Проверяется, что
//      после смены языка карточки и селекты перерисовались БЕЗ повторного
//      поиска: новых GET /hf/search и /hf/files ноль.
//   3. Раскрытие R693 не теряется: раскрытая карточка остаётся раскрытой,
//      карточка файлов — в её слоте, отметки файлов — на месте.
//   4. Подписка идемпотентна и снимается: unmount() убирает обработчик (смена
//      языка больше не рисует), повторный mount() не задваивает перерисовку
//      (одно событие = одна пересборка HTML).
//
// Запуск: node webui/tests/image-hf-i18n-r694.test.js
'use strict';

const assert = require('assert');
const path = require('path');

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

// Разметка «страницы»: карточка файлов лежит в хосте таба, как в index.html.
const tabHost = makeNode('div', { attrs: { id: 'imTabHf' } });
const filesCard = makeNode('div', { id: 'imHfFilesCard' });
tabHost.appendChild(filesCard);
const filesBody = makeNode('div', { id: 'imHfFilesBody' });
filesCard.appendChild(filesBody);
elements['imHfFilesCard'] = filesCard;
elements['imHfFilesBody'] = filesBody;

// Слоты раскрытых карточек: заглушка ищет их по ПОСЛЕДНЕЙ отрисованной разметке
// (ровно как браузер — узел существует только если он в HTML).
const slots = {};
function slotFor(repo) {
    if (!slots[repo]) slots[repo] = makeNode('div', { attrs: { 'data-imh-files-slot': repo } });
    return slots[repo];
}

// window-события: i18n/index.js диспатчит i18n:changed, модуль подписывается.
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
        // Слот существует, только пока раскрытая карточка нарисована.
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
// Никакой самодельной мини-таблицы переводов: именно её отсутствие в пакетах и
// было дефектом 1. Порядок важен — I18N index.js регистрирует пакеты при загрузке.
require(path.join(__dirname, '..', 'js', 'i18n', 'ru.js'));
require(path.join(__dirname, '..', 'js', 'i18n', 'en.js'));
require(path.join(__dirname, '..', 'js', 'i18n', 'index.js'));
const RU_PACK = global.I18N_RU;
const EN_PACK = global.I18N_EN;
assert.ok(RU_PACK && EN_PACK, 'пакеты i18n не загрузились');
const I18N = global.I18N;
assert.ok(I18N && typeof I18N.setLang === 'function', 'I18N не инициализировался');

// Сбор предупреждений «Missing translation key: imageModels.role_*» — прямой
// счётчик дефекта 1 (в браузере он сыпался в консоль на каждый рендер).
const missingRoleWarnings = [];
const realWarn = console.warn.bind(console);
console.warn = function () {
    const text = Array.prototype.map.call(arguments, function (a) { return String(a); }).join(' ');
    if (text.indexOf('Missing translation key') !== -1 && text.indexOf('imageModels.role_') !== -1) {
        missingRoleWarnings.push(text);
    }
    return realWarn.apply(null, arguments);
};

// --- mocks: fetch -----------------------------------------------------------

const requests = [];
const FILES = [
    { path: 'flux/flux1-dev-Q4_0.gguf', sizeBytes: 6000000000, suggestedRole: 'diffusion' },
    { path: 'flux/ae.safetensors', sizeBytes: 300000000, suggestedRole: 'vae' },
    { path: 'flux/clip_l.safetensors', sizeBytes: 246000000, suggestedRole: 'clip_l' },
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

require(path.join(__dirname, '..', 'js', 'modules', 'image-models-hf.js'));
const Hf = global.ImageModelsHf;
assert.ok(Hf, 'window.ImageModelsHf должен быть экспортирован');
const P = Hf.pure;

const ctx = { apiBase: 'http://balancer.test:18081', backendId: 'img-1', tab: 'hf', headers: {} };
const REPO = 'leejet/FLUX.1-schnell-gguf';

function clickButton(repo) {
    const btn = makeNode('button', { attrs: { 'data-imh-pick': repo }, parentNode: slotFor(repo) });
    getEl('image-page').dispatch('click', { target: btn });
}
function filesRequests() { return requests.filter((r) => r.url.indexOf('/hf/files') !== -1); }
function searchRequests() { return requests.filter((r) => r.url.indexOf('/hf/search') !== -1); }
function resultsHtml() { return getEl('imHfSearchResults').innerHTML; }
function filesHtml() { return filesBody.innerHTML; }
function optionLabel(html, role) {
    const re = new RegExp('<option value="' + role + '"[^>]*>([^<]*)</option>');
    const m = re.exec(html);
    return m ? m[1] : null;
}

(async function main() {
    // Стартуем с русского: дефект 1 виден именно там (fallback = сырой id),
    // а переключение RU→EN — сценарий дефекта 2.
    I18N.setLang('ru');
    assert.strictEqual(I18N.getLang(), 'ru');

    Hf.render(ctx);
    getEl('imHfQuery').value = 'flux';
    await Hf._actions.search();
    await sleep(20);
    clickButton(REPO);
    await sleep(80); // /hf/files + /hf/plan + автопред-проверка главного файла

    check('подготовка: выдача HF отрисована, файлы раскрыты (R693)', function () {
        assert.ok(resultsHtml().indexOf(REPO) !== -1, 'нет карточки модели');
        assert.ok(resultsHtml().indexOf('data-imh-files-slot="' + REPO + '"') !== -1, 'карточка не раскрыта');
        assert.strictEqual(Hf._state.expandedRepo, REPO);
        assert.ok(filesHtml().indexOf('flux/flux1-dev-Q4_0.gguf') !== -1, 'файлы не отрисованы');
        assert.strictEqual(filesCard.parentNode, slotFor(REPO), 'карточка файлов не в слоте карточки модели');
    });

    // --- ДЕФЕКТ 1: селект ролей отдаёт подпись из i18n, а не сырой id ------
    // Ровно те роли, которым ключа не хватало (см. шапку файла и список в
    // задаче). diffusion/vae ключи имели и раньше, их значения не менялись.
    const ADDED_ROLES = ['clip_l', 'clip_g', 't5xxl', 'llm', 'clip_vision', 'taesd',
        'lora', 'upscaler', 'controlnet', 'ip_adapter'];

    check('1a. оба пакета содержат человеческую подпись КАЖДОЙ роли контракта', function () {
        const roles = P.IMAGE_ROLES;
        assert.strictEqual(roles.length, 12, 'ролей в контракте не 12: ' + roles.length);
        roles.forEach(function (role) {
            const ru = RU_PACK['imageModels.role_' + role];
            const en = EN_PACK['imageModels.role_' + role];
            assert.ok(ru, 'нет ru-подписи для роли ' + role + ' (в селекте будет сырой id)');
            assert.ok(en, 'нет en-подписи для роли ' + role);
            if (ADDED_ROLES.indexOf(role) !== -1) {
                assert.notStrictEqual(ru, role, 'ru-подпись роли ' + role + ' равна сырому id');
                assert.notStrictEqual(en, role, 'en-подпись роли ' + role + ' равна сырому id');
                assert.notStrictEqual(ru, en, 'ru/en подписи роли ' + role + ' совпали — перевод не сделан: ' + ru);
            }
        });
    });

    check('1b. ROLE_LABEL_KEYS покрывает IMAGE_ROLES и не собирается динамикой', function () {
        assert.ok(P.ROLE_LABEL_KEYS, 'нет статической карты ROLE_LABEL_KEYS (измеритель i18n её не увидит)');
        P.IMAGE_ROLES.forEach(function (role) {
            assert.strictEqual(P.ROLE_LABEL_KEYS[role], 'imageModels.role_' + role,
                'для роли ' + role + ' нет статического ключа подписи');
        });
        assert.strictEqual(typeof P.roleOptions, 'function', 'roleOptions не экспортирован для проверки');
    });

    check('1c. roleOptions отдаёт подписи пакета RU и НЕ отдаёт сырые id', function () {
        const html = P.roleOptions('clip_l');
        P.IMAGE_ROLES.forEach(function (role) {
            const label = optionLabel(html, role);
            assert.strictEqual(label, RU_PACK['imageModels.role_' + role],
                'подпись роли ' + role + ' не из пакета: ' + label);
            if (ADDED_ROLES.indexOf(role) !== -1) {
                assert.notStrictEqual(label, role, 'в селекте сырой id ' + role);
            }
        });
        assert.ok(/value="clip_l" selected/.test(html), 'выбранная роль не отмечена');
    });

    check('1d. селекты ролей в отрисованных строках файлов — человеческие подписи', function () {
        const html = filesHtml();
        assert.strictEqual(optionLabel(html, 'diffusion'), RU_PACK['imageModels.role_diffusion']);
        assert.strictEqual(optionLabel(html, 'vae'), RU_PACK['imageModels.role_vae']);
        assert.strictEqual(optionLabel(html, 'clip_l'), RU_PACK['imageModels.role_clip_l']);
        ADDED_ROLES.forEach(function (role) {
            assert.notStrictEqual(optionLabel(html, role), role, 'в строках файлов сырой id ' + role);
        });
    });

    check('1e. за сессию работы с HF нет предупреждений Missing translation key: imageModels.role_*', function () {
        assert.deepStrictEqual(missingRoleWarnings, [],
            'консоль получила ' + missingRoleWarnings.length + ' предупреждений, первое: ' + missingRoleWarnings[0]);
    });

    // --- ДЕФЕКТ 2: смена языка перерисовывает выдачу без запросов ----------
    const filesBefore = filesRequests().length;
    const searchBefore = searchRequests().length;
    const rendersBefore = getEl('imHfSearchResults')._htmlSets;
    const checkedBefore = Hf._state.selection['flux/flux1-dev-Q4_0.gguf'].checked;
    assert.strictEqual(checkedBefore, true, 'подготовка: diffusion-файл должен быть отмечен');

    I18N.setLang('en');

    check('2a. карточки и подписи перерисованы на EN (без повторного поиска)', function () {
        const html = resultsHtml();
        assert.ok(html.indexOf(EN_PACK['imageModels.hf_hide_files']) !== -1,
            'подпись кнопки раскрытой карточки осталась прежней — модуль не слушает i18n:changed: ' + html.slice(0, 300));
        assert.strictEqual(html.indexOf(RU_PACK['imageModels.hf_hide_files']), -1,
            'в выдаче остался русский текст после переключения на EN');
        assert.ok(filesHtml().indexOf(EN_PACK['imageModels.role_diffusion']) !== -1,
            'селекты ролей остались на старом языке');
        assert.strictEqual(optionLabel(filesHtml(), 'clip_l'), EN_PACK['imageModels.role_clip_l'],
            'подпись clip_l не переведена на EN');
    });

    check('2b. перерисовка ровно одна (одна подписка), сети ноль', function () {
        assert.strictEqual(getEl('imHfSearchResults')._htmlSets, rendersBefore + 1,
            'ожидалась одна пересборка списка, факт: ' + (getEl('imHfSearchResults')._htmlSets - rendersBefore));
        assert.strictEqual(filesRequests().length, filesBefore,
            'смена языка повторила GET /hf/files: ' + JSON.stringify(filesRequests().slice(filesBefore).map((r) => r.url)));
        assert.strictEqual(searchRequests().length, searchBefore, 'смена языка повторила GET /hf/search');
    });

    check('2c. R693 цел: раскрытие, файлы и отметки пережили перерисовку', function () {
        assert.strictEqual(Hf._state.expandedRepo, REPO, 'раскрытая карточка потерялась');
        assert.ok(resultsHtml().indexOf('data-imh-files-slot="' + REPO + '"') !== -1, 'слот раскрытой карточки исчез');
        assert.strictEqual(filesCard.parentNode, slotFor(REPO), 'карточка файлов выпала из карточки модели');
        assert.strictEqual(filesCard.style.display, '', 'карточка файлов скрыта');
        assert.ok(filesHtml().indexOf('flux/flux1-dev-Q4_0.gguf') !== -1, 'файлы не перерисованы');
        assert.strictEqual(Hf._state.selection['flux/flux1-dev-Q4_0.gguf'].checked, true,
            'отметки файлов сброшены перерисовкой');
        assert.strictEqual(Hf._state.files.length, 3, 'состояние файлов потеряно');
    });

    // --- отписка и идемпотентность подписки --------------------------------
    check('2d. unmount() снимает подписку: смена языка больше не перерисовывает', function () {
        assert.strictEqual(typeof Hf.unmount, 'function', 'нет симметричного unmount (подписка не снимается)');
        assert.strictEqual(Hf.unmount(), true);
        const sets = getEl('imHfSearchResults')._htmlSets;
        I18N.setLang('ru');
        assert.strictEqual(getEl('imHfSearchResults')._htmlSets, sets,
            'после unmount смена языка всё ещё перерисовывает выдачу — подписка осталась');
    });

    check('2e. повторный mount не задваивает перерисовку', function () {
        Hf.render(ctx); // mount() снова
        const sets = getEl('imHfSearchResults')._htmlSets;
        I18N.setLang('en');
        assert.strictEqual(getEl('imHfSearchResults')._htmlSets, sets + 1,
            'одно i18n:changed дало ' + (getEl('imHfSearchResults')._htmlSets - sets) +
            ' пересборки — подписок больше одной');
    });

    console.warn = realWarn;
    console.log('\n' + (failures ? 'FAILED: ' + failures : 'OK: ' + passed + ' checks passed'));
    if (failures) process.exit(1);
})().catch((e) => {
    console.warn = realWarn;
    console.error('FATAL', e && e.stack || e);
    process.exit(1);
});

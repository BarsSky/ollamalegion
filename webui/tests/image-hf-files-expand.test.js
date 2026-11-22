// image-hf-files-expand.test.js — R693 (2026-11-24): кнопка «Показать файлы»
// на вкладке Image-модели → HuggingFace обязана РАСКРЫВАТЬ файлы модели.
//
// ЖИВОЙ ДЕФЕКТ, который стережёт этот набор (замер на образе webui r692, образ
// 18084, вход skyadmin, поиск qwen-image-edit):
//   кнопка нажата на y≈577 (окно 768 px), запрос GET .../hf/files → 200, в DOM
//   отрисованы 16 строк — но В ОБЩУЮ карточку #imHfFilesCard, которая стоит в
//   разметке ПОСЛЕ всего списка найденных моделей: её top = 2995 px, scrollY не
//   меняется. В окне оператора не меняется НИЧЕГО → «кнопка не работает».
//
// ЧТО ПРОВЕРЯЕТСЯ (и почему так):
//   1. клик по кнопке (через то же делегирование на #image-page, что и в браузере)
//      инициирует запрос файлов и РАСКРЫТИЕ: карточка файлов переезжает в слот
//      карточки нажатой модели, подпись кнопки меняется, aria-expanded=true;
//   2. повторный клик СВОРАЧИВАЕТ и не делает ни одного запроса;
//   3. повторное раскрытие идёт из кэша репозитория — запросов нет, галочки не
//      сбрасываются;
//   4. у модели без файлов — ПОНЯТНОЕ пустое состояние в раскрытой панели, а не
//      «ничего не произошло»;
//   5. файлы другого репозитория — свои (подмена modelId ловится);
//   6. клик по галочке ВНУТРИ раскрытой панели не сворачивает её (панель лежит в
//      карточке, а карточка кликабельна целиком).
//
// Запуск: node webui/tests/image-hf-files-expand.test.js
//
// ДОМ-ЗАГЛУШКА. Полного DOM в node нет, поэтому заглушка моделирует ТОЛЬКО то,
// что нужно контракту: id-элементы, строковый innerHTML у контейнера результатов
// и поиск слота по селектору [data-imh-files-slot="<repo>"] — по последней
// отрисованной разметке (так же, как это делает браузер). appendChild/
// insertBefore/contains у заглушек ведут себя как настоящие (parentNode-цепочка),
// поэтому «переезд узла» проверяется по факту, а не по вызову.
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
        style: opts.style ? {} : {},
        value: '',
        checked: false,
        textContent: '',
        parentNode: opts.parentNode || null,
        nextSibling: opts.nextSibling || null,
        childNodes: [],
        _html: '',
        getAttribute: function (n) { return Object.prototype.hasOwnProperty.call(attrs, n) ? attrs[n] : null; },
        setAttribute: function (n, v) { attrs[n] = v; },
        hasAttribute: function (n) { return Object.prototype.hasOwnProperty.call(attrs, n); },
        addEventListener: function (t, fn) { (listeners[t] = listeners[t] || []).push(fn); },
        removeEventListener: function () { },
        dispatch: function (t, ev) { (listeners[t] || []).forEach(function (fn) { fn(ev || {}); }); },
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
            node._appended = (node._appended || 0) + 1;
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
            node._inserted = (node._inserted || 0) + 1;
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
        set: function (v) { this._html = String(v); },
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

global.window = global;
global.document = {
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

const RU = {
    'gguf.search_btn': 'Найти',
    'gguf.no_results': 'Результатов не найдено',
    'gguf.results_count': 'Найдено моделей: {n}',
    'gguf.downloads': 'Загрузок',
    'gguf.likes': 'Лайки',
    'gguf.last_updated': 'Обновлён',
    'gguf.search_title': 'HuggingFace Hub',
    'common.loading': 'Загрузка...',
    'imageModels.hf_pick_files': 'Показать файлы',
    'imageModels.hf_hide_files': 'Свернуть файлы',
    'imageModels.hf_no_files': 'В этом репозитории нет файлов для скачивания',
    'imageModels.hf_no_files_hint': 'Возьмите GGUF-квант: добавьте «-GGUF» к запросу.',
    'imageModels.hf_files_failed': 'Не удалось прочитать файлы репозитория',
    'imageModels.hf_selected_count': 'Выбрано файлов: {n}',
    'imageModels.hf_suggested': 'предложено',
    'imageModels.role_diffusion': 'diffusion (веса)',
    'imageModels.role_vae': 'vae',
    'imageModels.role_clip_l': 'clip_l',
    'imageModels.repo_verdict': 'Оценка репозитория',
    'imageModels.probe_not_checked': 'не проверено',
    'imageModels.probe_btn': 'Проверить',
    'imageModels.hf_files_title': 'Файлы репозитория',
    'imageModels.hf_select_suggested': 'Отметить по ролям',
    'imageModels.hf_clear_selection': 'Снять отметки',
};
global.I18N = {
    t: function (k, vars) {
        const s = Object.prototype.hasOwnProperty.call(RU, k) ? RU[k] : k;
        return vars ? String(s).replace(/\{(\w+)\}/g, function (m, p) { return p in vars ? vars[p] : m; }) : s;
    },
    getLang: function () { return 'ru'; },
};

// --- mocks: fetch -----------------------------------------------------------

const requests = [];
const FILES_A = [
    { path: 'flux/flux1-dev-Q4_0.gguf', sizeBytes: 6000000000, suggestedRole: 'diffusion' },
    { path: 'flux/ae.safetensors', sizeBytes: 300000000, suggestedRole: 'vae' },
];
const FILES_B = [
    { path: 'sd15/sd15-Q8_0.gguf', sizeBytes: 7000000000, suggestedRole: 'diffusion' },
];
const FILES_SLOW = [
    { path: 'slow/one.gguf', sizeBytes: 1000, suggestedRole: 'diffusion' },
];
const fileSets = {
    'leejet/FLUX.1-schnell-gguf': FILES_A,
    'leejet/SD15-gguf': FILES_B,
    'empty/no-files': [],
    'slow/repo': FILES_SLOW,
};
// Задержка ответа по репозиторию — для проверки гонки «клик по другой карточке,
// пока грузится первая».
const delays = {};
global.fetch = function (url, init) {
    const rec = { url: String(url), method: (init && init.method) || 'GET' };
    requests.push(rec);
    const body = (function () {
        if (rec.url.indexOf('/hf/search') !== -1) {
            return {
                count: 4,
                results: [
                    { id: 'leejet/FLUX.1-schnell-gguf', downloads: 10, likes: 2, lastModified: '2026-11-01' },
                    { id: 'leejet/SD15-gguf', downloads: 5, likes: 1, lastModified: '2026-10-01' },
                    { id: 'empty/no-files', downloads: 0, likes: 0, lastModified: '' },
                    { id: 'slow/repo', downloads: 1, likes: 0, lastModified: '2026-09-01' },
                ],
            };
        }
        if (rec.url.indexOf('/hf/files') !== -1) {
            const m = /modelId=([^&]+)/.exec(rec.url);
            const repo = decodeURIComponent(m ? m[1] : '');
            const files = fileSets[repo] || [];
            return { count: files.length, files: files };
        }
        if (rec.url.indexOf('/hf/plan') !== -1) return { verdict: 'unknown', roles: [], steps: [] };
        if (rec.url.indexOf('/hf/probe') !== -1) return { verdict: 'unknown', reason: 'заголовок не прочитан' };
        return {};
    })();
    const repoMatch = /modelId=([^&]+)/.exec(rec.url);
    const repo = repoMatch ? decodeURIComponent(repoMatch[1]) : '';
    const delay = (rec.url.indexOf('/hf/files') !== -1 && delays[repo]) || 0;
    const answer = { ok: true, status: 200, text: function () { return Promise.resolve(JSON.stringify(body)); } };
    return delay
        ? new Promise(function (resolve) { setTimeout(function () { resolve(answer); }, delay); })
        : Promise.resolve(answer);
};

require(path.join(__dirname, '..', 'js', 'modules', 'image-models-hf.js'));
const Hf = global.ImageModelsHf;
assert.ok(Hf, 'window.ImageModelsHf должен быть экспортирован');

const ctx = { apiBase: 'http://balancer.test:18081', backendId: 'img-1', tab: 'hf', headers: {} };

/** Клик «как в браузере»: событие всплывает от кнопки до #image-page. */
function clickButton(repo) {
    const btn = makeNode('button', { attrs: { 'data-imh-pick': repo }, parentNode: slotFor(repo) });
    getEl('image-page').dispatch('click', { target: btn });
}
function filesRequests() {
    return requests.filter((r) => r.url.indexOf('/hf/files') !== -1);
}
function probeRequests() {
    return requests.filter((r) => r.url.indexOf('/hf/probe') !== -1);
}

(async function main() {
    Hf.render(ctx);
    getEl('imHfQuery').value = 'flux';
    await Hf._actions.search();
    await sleep(20);

    check('поиск отрисовал карточки (подготовка сценария)', function () {
        const html = getEl('imHfSearchResults').innerHTML;
        assert.ok(html.indexOf('leejet/FLUX.1-schnell-gguf') !== -1, 'нет карточки модели');
        assert.ok(html.indexOf('data-imh-pick="leejet/FLUX.1-schnell-gguf"') !== -1, 'нет кнопки у карточки');
        assert.strictEqual(html.indexOf('data-imh-files-slot'), -1, 'до клика ни одна карточка не раскрыта');
    });

    // --- 1. клик РАСКРЫВАЕТ файлы в своей карточке -------------------------
    requests.length = 0;
    clickButton('leejet/FLUX.1-schnell-gguf');
    await sleep(30);

    check('1a. клик по «Показать файлы» уходит в GET /hf/files этого репозитория', function () {
        const rec = filesRequests()[0];
        assert.ok(rec, 'запрос файлов не ушёл после клика: ' + JSON.stringify(requests.map((r) => r.url)));
        assert.ok(rec.url.indexOf('modelId=leejet%2FFLUX.1-schnell-gguf') !== -1, 'не тот репозиторий: ' + rec.url);
        assert.strictEqual(rec.method, 'GET');
    });
    check('1b. клик РАСКРЫВАЕТ карточку: в разметке появился слот этой модели', function () {
        const html = getEl('imHfSearchResults').innerHTML;
        assert.ok(html.indexOf('data-imh-files-slot="leejet/FLUX.1-schnell-gguf"') !== -1,
            'нет слота раскрытой карточки — файлы снова ушли бы в общую карточку внизу: ' + html.slice(0, 300));
        assert.ok(html.indexOf('imh-expanded') !== -1, 'карточка не помечена раскрытой');
    });
    check('1c. карточка файлов ПЕРЕЕХАЛА в слот раскрытой карточки (а не осталась внизу страницы)', function () {
        const slot = slotFor('leejet/FLUX.1-schnell-gguf');
        assert.ok(slot.childNodes.indexOf(filesCard) !== -1,
            'карточка файлов не внутри раскрытой карточки — оператор её не увидит');
        assert.strictEqual(filesCard.parentNode, slot);
        assert.notStrictEqual(filesCard.parentNode, tabHost);
        assert.strictEqual(filesCard.style.display, '', 'карточка файлов должна быть показана');
    });
    check('1d. файлы отрисованы внутри раскрытой панели (пути и роли)', function () {
        const html = filesBody.innerHTML;
        assert.ok(html.indexOf('flux/flux1-dev-Q4_0.gguf') !== -1, 'нет файла весов: ' + html.slice(0, 300));
        assert.ok(html.indexOf('flux/ae.safetensors') !== -1, 'нет VAE');
        assert.ok(html.indexOf('data-imh-role="flux/ae.safetensors"') !== -1, 'нет селекта роли');
        assert.strictEqual(Hf._state.expandedRepo, 'leejet/FLUX.1-schnell-gguf');
        assert.strictEqual(Hf._state.files.length, 2);
    });
    check('1e. кнопка переключилась в состояние «Свернуть файлы» (aria-expanded=true)', function () {
        const html = getEl('imHfSearchResults').innerHTML;
        assert.ok(html.indexOf('Свернуть файлы') !== -1, 'подпись не сменилась: ' + html.slice(0, 300));
        assert.ok(html.indexOf('aria-expanded="true"') !== -1, 'нет aria-expanded=true');
    });

    // --- 2. повторный клик СВОРАЧИВАЕТ без запросов -------------------------
    const beforeCollapse = requests.length;
    clickButton('leejet/FLUX.1-schnell-gguf');
    await sleep(30);
    check('2a. повторный клик сворачивает панель и не делает НИ ОДНОГО запроса', function () {
        assert.strictEqual(requests.length, beforeCollapse, 'сворачивание дёрнуло сеть: ' +
            JSON.stringify(requests.slice(beforeCollapse).map((r) => r.url)));
        assert.strictEqual(Hf._state.expandedRepo, '', 'состояние «раскрыто» не сброшено');
        assert.strictEqual(filesCard.style.display, 'none', 'панель не скрыта');
        assert.strictEqual(filesCard.parentNode, tabHost, 'панель не вернулась на своё место в разметке');
        assert.strictEqual(getEl('imHfSearchResults').innerHTML.indexOf('data-imh-files-slot'), -1,
            'слот раскрытой карточки остался в разметке');
    });

    // --- 3. повторное раскрытие: кэш, без запросов -------------------------
    const beforeReexpand = requests.length;
    clickButton('leejet/FLUX.1-schnell-gguf');
    await sleep(30);
    check('3a. повторное раскрытие показывает те же файлы из кэша (запросов нет)', function () {
        assert.strictEqual(requests.length, beforeReexpand, 'повторное раскрытие повторило запросы: ' +
            JSON.stringify(requests.slice(beforeReexpand).map((r) => r.url)));
        assert.strictEqual(filesCard.style.display, '', 'панель не показана повторно');
        assert.strictEqual(slotFor('leejet/FLUX.1-schnell-gguf').childNodes.indexOf(filesCard) !== -1, true,
            'панель не вернулась в слот карточки');
        assert.ok(filesBody.innerHTML.indexOf('flux/flux1-dev-Q4_0.gguf') !== -1, 'файлы не отрисованы');
    });
    check('3b. отметки файлов переживают сворачивание/раскрытие', function () {
        const beforeCount = Object.keys(Hf._state.selection).length;
        assert.strictEqual(beforeCount, 2, 'выбор файлов потерян: ' + JSON.stringify(Hf._state.selection));
        assert.strictEqual(Hf._state.selection['flux/flux1-dev-Q4_0.gguf'].checked, true);
    });

    // --- 5. другая карточка: свои файлы ------------------------------------
    requests.length = 0;
    clickButton('leejet/SD15-gguf');
    await sleep(30);
    check('5. клик по другой карточке раскрывает ЕЁ файлы (свой modelId, свой список)', function () {
        const rec = filesRequests()[0];
        assert.ok(rec && rec.url.indexOf('modelId=leejet%2FSD15-gguf') !== -1, 'не тот репозиторий: ' + JSON.stringify(rec));
        assert.strictEqual(Hf._state.expandedRepo, 'leejet/SD15-gguf');
        const html = filesBody.innerHTML;
        assert.ok(html.indexOf('sd15/sd15-Q8_0.gguf') !== -1, 'нет файлов второго репозитория');
        assert.strictEqual(html.indexOf('flux/flux1-dev-Q4_0.gguf'), -1, 'в панели остались файлы прошлой модели');
        assert.strictEqual(slotFor('leejet/SD15-gguf').childNodes.indexOf(filesCard) !== -1, true);
    });

    // --- 6. клик ВНУТРИ панели не сворачивает ------------------------------
    const beforeInside = requests.length;
    const checkboxInside = makeNode('input', { attrs: { 'data-imh-check': 'sd15/sd15-Q8_0.gguf' }, parentNode: filesBody });
    getEl('image-page').dispatch('click', { target: checkboxInside });
    await sleep(20);
    check('6. клик по галочке внутри раскрытой панели не сворачивает её', function () {
        assert.strictEqual(Hf._state.expandedRepo, 'leejet/SD15-gguf', 'панель свернулась от клика по галочке');
        assert.strictEqual(filesCard.style.display, '', 'панель спряталась');
        assert.strictEqual(requests.length, beforeInside, 'клик внутри панели дёрнул сеть');
    });

    // --- 4. модель без файлов: понятное пустое состояние -------------------
    requests.length = 0;
    clickButton('empty/no-files');
    await sleep(30);
    check('4. модель без файлов: панель раскрыта и показывает ПОНЯТНОЕ пустое состояние', function () {
        assert.ok(filesRequests().length === 1, 'ожидался один запрос файлов, было: ' + filesRequests().length);
        assert.strictEqual(Hf._state.files.length, 0);
        assert.strictEqual(filesCard.style.display, '', 'панель должна быть показана — иначе «ничего не произошло»');
        assert.strictEqual(slotFor('empty/no-files').childNodes.indexOf(filesCard) !== -1, true,
            'пустое состояние обязано быть В карточке модели');
        const html = filesBody.innerHTML;
        assert.ok(html.indexOf('нет файлов для скачивания') !== -1, 'нет объяснения пустого состояния: ' + html.slice(0, 300));
        assert.ok(html.indexOf('-GGUF') !== -1, 'нет подсказки, что делать дальше');
    });

    // --- 7. гонка: клик по другой карточке, пока грузится первая -----------
    delays['slow/repo'] = 40;
    clickButton('slow/repo');
    await sleep(5);
    clickButton('leejet/FLUX.1-schnell-gguf');       // из кэша, мгновенно
    await sleep(120);
    check('7. ответ «медленной» модели не подменяет файлы той, что открыта сейчас', function () {
        assert.strictEqual(Hf._state.expandedRepo, 'leejet/FLUX.1-schnell-gguf',
            'раскрыта не та карточка, по которой был последний клик');
        assert.strictEqual(Hf._state.filesLoading, false);
        const paths = Hf._state.files.map(function (f) { return f.path; });
        assert.deepStrictEqual(paths, ['flux/flux1-dev-Q4_0.gguf', 'flux/ae.safetensors'],
            'состояние описывает файлы другой модели: ' + JSON.stringify(paths));
        assert.ok(filesBody.innerHTML.indexOf('flux/flux1-dev-Q4_0.gguf') !== -1);
        assert.strictEqual(filesBody.innerHTML.indexOf('slow/one.gguf'), -1, 'в панели чужие файлы');
        // Ответ устаревшей попытки всё равно попал в кэш своего репозитория.
        assert.ok(Hf._state.repoCache['slow/repo'], 'нет кэша для slow/repo');
        assert.strictEqual(Hf._state.repoCache['slow/repo'].files.length, 1);
    });

    console.log('\n' + (failures ? 'FAILED: ' + failures : 'OK: ' + passed + ' checks passed'));
    if (failures) process.exit(1);
})().catch((e) => {
    console.error('FATAL', e && e.stack || e);
    process.exit(1);
});

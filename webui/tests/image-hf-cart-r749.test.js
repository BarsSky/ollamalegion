// image-hf-cart-r749.test.js — R749 (2026-11-25): корзина загрузки наверху
// вкладки Image-модели → HuggingFace.
//
// ЖИВАЯ ЖАЛОБА ОПЕРАТОРА (скриншот): «внизу делается сборка — непонятно, что
// скачается, а вверху только галочки». Имя bundle, семейство, HF-токен и кнопка
// скачивания жили в ОТДЕЛЬНОЙ карточке ПОД списком файлов; при 16+ файлах
// оператор не видел ни КУДА скачает, ни ЧТО в наборе, ни ЧЕГО уже не хватает на
// узле. R749 переносит всё это в липкую корзину НАД списком.
//
// ЧТО ПРОВЕРЯЕТСЯ:
//   1. РАЗМЕТКА: #imHfCartCard стоит первой в #imTabHf (выше поиска и файлов),
//      в ней цель (имя/семейство/токен), состав, итог и кнопки «Скачать
//      выбранное», «Скачать недостающее», «Проверить совместимость»; в карточке
//      файлов второй кнопки скачивания нет; sticky-правило в CSS.
//   2. ЦЕЛЬ: строка «Куда» называет имя набора, семейство, бэкенд и репозиторий;
//      пустое имя честно помечено «имя не задано».
//   3. СОСТАВ И СОСТОЯНИЕ: по каждому отмеченному файлу — роль, размер и
//      состояние на узле («есть на узле» / «нет на узле»); сверка с реестром
//      узла (GET .../models) идёт по полному пути, по localPath и по имени файла.
//   4. ИТОГ: «в корзине N · X, на узле K, не хватает M · Y»; когда всё на узле —
//      это сказано словами, а кнопка «Скачать недостающее» отключена.
//   5. «СКАЧАТЬ НЕДОСТАЮЩЕЕ»: реестр перечитывается перед решением; в
//      POST /hf/bundle уходят РОВНО отсутствующие файлы; если отсутствующих нет,
//      запроса нет вообще.
//   6. i18n: каждый новый ключ есть в ru.js и en.js (иначе у оператора сырой
//      литерал вместо подписи).
//
// Запуск: node webui/tests/image-hf-cart-r749.test.js
'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');

let failures = 0;
let passed = 0;
async function check(name, fn) {
    try { await fn(); passed++; console.log('  \u2713 ' + name); }
    catch (e) { failures++; console.error('  \u2717 ' + name + '\n    ' + e.message); }
}
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// --- DOM-заглушка (как в r696: id-элементы + слоты раскрытой карточки) ------

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
        disabled: false,
        textContent: '',
        parentNode: opts.parentNode || null,
        nextSibling: null,
        childNodes: [],
        _html: '',
        getAttribute: function (n) { return Object.prototype.hasOwnProperty.call(attrs, n) ? attrs[n] : null; },
        setAttribute: function (n, v) { attrs[n] = v; },
        hasAttribute: function (n) { return Object.prototype.hasOwnProperty.call(attrs, n); },
        addEventListener: function (t, fn) { (listeners[t] = listeners[t] || []).push(fn); },
        removeEventListener: function () { },
        dispatch: function (t, ev) { (listeners[t] || []).forEach(function (fn) { fn(ev || {}); }); },
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
        set: function (v) { this._html = String(v); },
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
const filesBody = makeNode('div', { id: 'imHfFilesBody' });
filesCard.appendChild(filesBody);
tabHost.appendChild(filesCard);
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
// R749: подпись бэкенда в строке «Куда» берётся со страницы «Модели на диске».
global.ImagePage = {
    _state: {
        backendId: 'img-1',
        backends: [{ id: 'img-1', name: 'GPU image', host: 'gpu', imagePort: 18093 }],
    },
};

const toasts = [];
global.Toast = { show: function (o) { toasts.push(o); } };

// Переводы: реальные значения новых ключей (иначе t() вернул бы сам ключ и
// проверки на слова стали бы вакуумными).
const RU = {
    'gguf.search_btn': 'Найти',
    'gguf.results_count': 'Найдено моделей: {n}',
    'gguf.downloads': 'Загрузок',
    'gguf.likes': 'Лайки',
    'gguf.last_updated': 'Обновлён',
    'common.loading': 'Загрузка...',
    'imageModels.hf_pick_files': 'Показать файлы',
    'imageModels.hf_hide_files': 'Свернуть файлы',
    'imageModels.hf_files_title': 'Файлы репозитория',
    'imageModels.hf_select_suggested': 'Отметить по ролям',
    'imageModels.hf_clear_selection': 'Очистить',
    'imageModels.hf_selected_count': 'Выбрано файлов: {n}',
    'imageModels.hf_selected_summary': 'Выбрано: {n} файлов · {size}',
    'imageModels.hf_nothing_selected': 'Ничего не выбрано — скачается только отмеченное',
    'imageModels.hf_download_selected': 'Скачать выбранное',
    'imageModels.hf_verify_compat': 'Проверить совместимость',
    'imageModels.hf_verify_running': 'Проверяю заголовки выбранных файлов…',
    'imageModels.hf_verify_failed': 'Не удалось проверить набор: {error}',
    'imageModels.hf_detected_role': 'по заголовку: {role}',
    'imageModels.hf_suggested': 'предложено',
    'imageModels.hf_no_files': 'В этом репозитории нет файлов для скачивания',
    'imageModels.hf_no_files_hint': 'Возьмите GGUF-квант.',
    'imageModels.hf_files_failed': 'Не удалось прочитать файлы репозитория',
    'imageModels.hf_none_selected': 'Отметьте хотя бы один файл модели',
    'imageModels.role_diffusion': 'diffusion (веса)',
    'imageModels.role_vae': 'VAE (автоэнкодер)',
    'imageModels.role_clip_l': 'CLIP-L (текстовый энкодер)',
    'imageModels.role_clip_g': 'CLIP-G (текстовый энкодер)',
    'imageModels.role_t5xxl': 'T5-XXL (текстовый энкодер)',
    'imageModels.role_llm': 'LLM-энкодер (Qwen-Image)',
    'imageModels.repo_verdict': 'Оценка репозитория',
    'imageModels.probe_not_checked': 'не проверено',
    'imageModels.probe_btn': 'Проверить',
    'imageModels.plan_mode': 'Режим запуска',
    'imageModels.compat_verdict_yes': 'imageworker сможет загрузить: да',
    'imageModels.compat_verdict_no': 'imageworker НЕ сможет загрузить',
    'imageModels.compat_verdict_missing': 'не хватает: {roles}',
    'imageModels.hf_selected_count_plural': 'Выбрано файлов: {n}',
    'imageModels.cart_title': 'Корзина загрузки',
    'imageModels.cart_target': 'Куда',
    'imageModels.cart_family': 'Семейство',
    'imageModels.cart_backend': 'Бэкенд',
    'imageModels.cart_repo': 'Репозиторий',
    'imageModels.cart_name_unset': 'имя не задано',
    'imageModels.cart_repo_unset': 'не выбран',
    'imageModels.cart_empty': 'Корзина пуста: найдите репозиторий и отметьте файлы — здесь появится роль, размер и состояние каждого файла на узле.',
    'imageModels.cart_download_missing': 'Скачать недостающее',
    'imageModels.cart_present': 'есть на узле',
    'imageModels.cart_absent': 'нет на узле',
    'imageModels.cart_unknown': 'узел не прочитан',
    'imageModels.cart_unknown_hint': 'Список файлов узла не прочитан ({error}).',
    'imageModels.cart_totals': 'В корзине: {n} файлов · {size}',
    'imageModels.cart_on_node': 'на узле: {n}',
    'imageModels.cart_missing_count': 'не хватает: {n} · {size}',
    'imageModels.cart_all_present': 'Всё выбранное уже есть на узле',
    'imageModels.cart_scan': 'Перечитать узел',
    'imageModels.cart_missing_toast': 'Скачиваю недостающее: {n} файлов',
    'imageModels.cart_scan_failed': 'Не удалось прочитать список файлов узла: {error}',
    'imageModels.cart_extra_source': 'из {repo}',
    'imageModels.cart_registry': 'в реестре узла: {n} моделей',
    'image.bundle_started': 'Загрузка bundle начата',
    'image.bundle_failed': 'Загрузка bundle не удалась',
};
global.I18N = {
    t: function (k, vars) {
        const s = Object.prototype.hasOwnProperty.call(RU, k) ? RU[k] : k;
        return vars ? String(s).replace(/\{(\w+)\}/g, function (m, p) { return p in vars ? vars[p] : m; }) : s;
    },
    getLang: function () { return 'ru'; },
};

// --- fetch-заглушка ---------------------------------------------------------

const requests = [];
const REPO = 'leejet/Qwen-Image-Test-GGUF';
const FILES = [
    { path: 'Qwen-Image-Q4_K.gguf', sizeBytes: 5287517120, suggestedRole: 'diffusion' },
    // VAE и CLIP-L отмечены по умолчанию (AUTO_ROLES) - на них проверяем статусы.
    { path: 'vae/qwen_image_vae.safetensors', sizeBytes: 675509688, suggestedRole: 'vae' },
    { path: 'text_encoders/qwen3vl.safetensors', sizeBytes: 9350798360, suggestedRole: 'clip_l' },
];
const DIFFUSION_FILE = FILES[0].path;
const VAE_FILE = FILES[1].path;
const CLIPL_FILE = FILES[2].path;

// Реестр узла: сначала лежит только diffusion-файл, затем - весь набор.
function modelsOnlyDiffusion() {
    return {
        models: [{
            name: 'qwen-image-test', family: 'qwen_image', state: 'not_loaded',
            files: [{ role: 'diffusion', filename: DIFFUSION_FILE, localPath: 'qwen-image-test/' + DIFFUSION_FILE }],
        }],
    };
}
function modelsAllFiles() {
    return {
        models: [{
            name: 'qwen-image-test', family: 'qwen_image', state: 'not_loaded',
            files: [
                { role: 'diffusion', filename: DIFFUSION_FILE, localPath: 'qwen-image-test/' + DIFFUSION_FILE },
                // localPath в подкаталоге, а filename - без каталога: сверка обязана
                // найти файл и по полному пути, и по имени (R749).
                { role: 'vae', filename: 'qwen_image_vae.safetensors', localPath: 'qwen-image-test/vae/qwen_image_vae.safetensors' },
                { role: 'clip_l', filename: 'text_encoders/qwen3vl.safetensors', localPath: 'qwen-image-test/text_encoders/qwen3vl.safetensors' },
            ],
        }],
    };
}
let MODELS = modelsOnlyDiffusion();

global.fetch = function (url, init) {
    const rec = { url: String(url), method: (init && init.method) || 'GET', body: init && init.body };
    requests.push(rec);
    const body = (function () {
        if (rec.url.indexOf('/hf/search') !== -1) {
            return { count: 1, results: [{ id: REPO, downloads: 100, likes: 5, lastModified: '2026-11-20' }] };
        }
        if (rec.url.indexOf('/hf/files') !== -1) return { count: FILES.length, files: FILES };
        if (rec.url.indexOf('/hf/plan') !== -1) return { verdict: 'ready', roles: [], steps: [] };
        if (rec.url.indexOf('/hf/probe') !== -1) return { verdict: 'unknown', reason: 'не проверено' };
        if (rec.url.indexOf('/hf/verify') !== -1) {
            return { compat: { verdict: 'ready', runnable: true, family: 'qwen_image', missing: [], files: [] } };
        }
        if (rec.url.indexOf('/hf/progress') !== -1) {
            return { bundleId: REPO, status: 'completed', downloaded: 1, totalBytes: 1, files: [] };
        }
        if (rec.url.indexOf('/hf/downloads') !== -1) return { active: [], history: [], bundles: [] };
        if (rec.url.indexOf('/hf/bundle') !== -1) return { status: 'started' };
        if (rec.url.indexOf('/models') !== -1) return MODELS;
        return {};
    })();
    return Promise.resolve({
        ok: true, status: 200,
        text: function () { return Promise.resolve(JSON.stringify(body)); },
    });
};

require(path.join(__dirname, '..', 'js', 'modules', 'image-models-hf.js'));
const Hf = global.ImageModelsHf;
assert.ok(Hf, 'window.ImageModelsHf должен быть экспортирован');
const P = Hf.pure;
const ctx = { apiBase: 'http://balancer.test:18081', backendId: 'img-1', tab: 'hf', headers: {} };

function bundleRequests() { return requests.filter((r) => r.url.indexOf('/hf/bundle') !== -1); }
function modelRequests() { return requests.filter((r) => r.url.indexOf('/models') !== -1); }
function cartHtml() { return getEl('imHfCartList').innerHTML; }
function targetText() { return getEl('imHfCartTarget').innerHTML; }
function totalsText() { return getEl('imHfCartTotals').innerHTML; }
function countOccurrences(text, needle) {
    return String(text).split(needle).length - 1;
}

(async function main() {
    // --- 0. чистые помощники сверки с реестром узла -------------------------
    await check('0a. buildBackendFileIndex: путь, localPath и имя файла (регистр не важен)', function () {
        const idx = P.buildBackendFileIndex({
            models: [{
                name: 'm1', files: [{
                    role: 'vae', filename: 'ae.safetensors',
                    localPath: 'models/m1/vae/ae.safetensors',
                }],
            }],
        });
        assert.ok(P.presenceLookup(idx, 'ae.safetensors'), 'не найдено по filename');
        assert.ok(P.presenceLookup(idx, 'AE.SAFETENSORS'), 'сверка зависит от регистра');
        assert.ok(P.presenceLookup(idx, 'models/m1/vae/ae.safetensors'), 'не найдено по localPath');
        assert.strictEqual(P.presenceLookup(idx, 'other.safetensors'), null, 'чужой файл считается своим');
        assert.strictEqual(idx.models, 1, 'число моделей в реестре посчитано неверно');
    });

    // --- 1. подготовка: поиск, раскрытие, корзина ---------------------------
    Hf.render(ctx);
    getEl('imHfQuery').value = 'qwen';
    await Hf._actions.search();
    await Hf._actions.pickRepo(REPO);
    await sleep(60);

    await check('1. цель корзины: КУДА (имя, семейство, бэкенд) и откуда (репозиторий)', function () {
        const html = targetText();
        assert.ok(html.indexOf('Куда') !== -1, 'нет метки «Куда»: ' + html);
        assert.ok(html.indexOf(REPO) !== -1, 'в цели нет репозитория: ' + html);
        assert.ok(html.indexOf('Семейство') !== -1, 'нет семейства: ' + html);
        assert.ok(html.indexOf('Бэкенд') !== -1, 'нет бэкенда: ' + html);
        assert.ok(html.indexOf('GPU image') !== -1, 'подпись бэкенда не взята со страницы моделей: ' + html);
        // имя подставлено из репозитория (applyBundleName) - значит, показано.
        assert.strictEqual(getEl('imgBundleName').value, 'qwen-image-test-gguf', 'имя bundle не подставлено');
        assert.ok(html.indexOf('qwen-image-test-gguf') !== -1, 'в цели нет имени набора: ' + html);
    });

    await check('2. состав: роль, размер и состояние каждого файла на узле', function () {
        const html = cartHtml();
        [DIFFUSION_FILE, VAE_FILE, CLIPL_FILE].forEach(function (p) {
            assert.ok(html.indexOf(p) !== -1, 'в составе нет файла ' + p);
        });
        assert.ok(html.indexOf('diffusion (веса)') !== -1, 'в составе нет роли diffusion');
        assert.ok(html.indexOf('VAE (автоэнкодер)') !== -1, 'в составе нет роли VAE');
        assert.ok(html.indexOf('GB') !== -1, 'в составе нет размера файла');
        // В реестре узла лежит только diffusion-файл.
        assert.strictEqual(countOccurrences(html, 'есть на узле'), 1,
            'ровно один файл обязан быть «есть на узле»: ' + html);
        assert.strictEqual(countOccurrences(html, 'нет на узле'), 2,
            'два файла обязаны быть «нет на узле»: ' + html);
    });

    await check('3. итог: на узле K, не хватает M, кнопки подписаны счётчиками', function () {
        const totals = totalsText();
        assert.ok(totals.indexOf('В корзине') !== -1, 'нет строки итога: ' + totals);
        assert.ok(totals.indexOf('на узле: 1') !== -1, 'нет счёта «на узле: 1»: ' + totals);
        assert.ok(totals.indexOf('не хватает: 2') !== -1, 'нет счёта «не хватает: 2»: ' + totals);
        const dl = getEl('imHfDownloadSelected');
        const dm = getEl('imHfDownloadMissing');
        assert.strictEqual(dl.disabled, false, '«Скачать выбранное» отключена при непустой корзине');
        assert.ok(/\(3/.test(dl.innerHTML), 'в «Скачать выбранное» нет счётчика: ' + dl.innerHTML);
        assert.strictEqual(dm.disabled, false, '«Скачать недостающее» отключена при двух отсутствующих');
        assert.ok(/\(2/.test(dm.innerHTML), 'в «Скачать недостающее» нет счётчика недостающих: ' + dm.innerHTML);
        assert.ok(getEl('imHfCartNodeState').textContent.indexOf('в реестре узла: 1 моделей') !== -1,
            'нет строки реестра узла: ' + getEl('imHfCartNodeState').textContent);
    });

    // --- 2. «Скачать недостающее»: реестр перечитывается, уходят только ---
    // --- отсутствующие файлы ------------------------------------------------
    await check('4. «Скачать недостающее» отправляет РОВНО отсутствующие файлы', async function () {
        const before = bundleRequests().length;
        const ok = await Hf._actions.downloadMissing();
        assert.strictEqual(ok, true, 'downloadMissing не запустился');
        const calls = bundleRequests();
        assert.strictEqual(calls.length, before + 1, 'запрос bundle не отправлен');
        const payload = JSON.parse(calls[calls.length - 1].body);
        const sent = payload.files.map(function (f) { return f.filename; }).sort();
        assert.deepStrictEqual(sent, [CLIPL_FILE, VAE_FILE].sort(),
            'в bundle ушли не ровно отсутствующие файлы: ' + JSON.stringify(sent));
        assert.strictEqual(sent.indexOf(DIFFUSION_FILE), -1, 'уже лежащий на узле файл скачивается повторно');
        assert.ok(toasts.some(function (o) { return o.message.indexOf('Скачиваю недостающее: 2 файлов') !== -1; }),
            'нет уведомления о числе недостающих: ' + JSON.stringify(toasts));
        assert.ok(modelRequests().length >= 2, 'реестр узла не перечитан перед решением');
    });

    // --- 3. всё на узле: честная надпись и отключённая кнопка ---------------
    await check('5. всё на узле: «Всё выбранное уже есть на узле», кнопка отключена', async function () {
        MODELS = modelsAllFiles();
        await Hf._actions.refreshBackendFiles();
        const totals = totalsText();
        assert.ok(totals.indexOf('Всё выбранное уже есть на узле') !== -1, 'нет итога «всё на узле»: ' + totals);
        assert.strictEqual(getEl('imHfDownloadMissing').disabled, true,
            'кнопка «Скачать недостающее» активна, хотя скачивать нечего');
        assert.ok(/\(0/.test(getEl('imHfDownloadMissing').innerHTML),
            'счётчик недостающих не обнулился: ' + getEl('imHfDownloadMissing').innerHTML);
        assert.strictEqual(countOccurrences(cartHtml(), 'есть на узле'), 3,
            'все три файла обязаны быть «есть на узле»: ' + cartHtml());
    });

    await check('6. при отсутствии недостающих запросов нет (и это сказано словами)', async function () {
        const before = bundleRequests().length;
        const ok = await Hf._actions.downloadMissing();
        assert.strictEqual(ok, false, 'downloadMissing не должен стартовать пустое скачивание');
        assert.strictEqual(bundleRequests().length, before, 'улетел POST /hf/bundle без недостающих файлов');
        assert.ok(toasts.some(function (o) { return o.message.indexOf('Всё выбранное уже есть на узле') !== -1; }),
            'нет уведомления «всё уже на узле»: ' + JSON.stringify(toasts));
    });

    // --- 4. очистка: корзина пуста, обе кнопки отключены --------------------
    await check('7. «Очистить»: корзина пуста, кнопки отключены, итог пустой', function () {
        Hf._actions.clearSelection();
        assert.ok(cartHtml().indexOf('Корзина пуста') !== -1, 'нет пустого состояния состава: ' + cartHtml());
        assert.ok(totalsText().indexOf('Корзина пуста') !== -1, 'нет пустого состояния итога: ' + totalsText());
        assert.strictEqual(getEl('imHfDownloadSelected').disabled, true, '«Скачать выбранное» активна при пустой корзине');
        assert.strictEqual(getEl('imHfDownloadMissing').disabled, true, '«Скачать недостающее» активна при пустой корзине');
        assert.ok(getEl('imHfSelectionSummarySticky').textContent.indexOf('Ничего не выбрано') !== -1,
            'сводка выбора не обнулилась: ' + getEl('imHfSelectionSummarySticky').textContent);
    });

    await check('8. «Скачать выбранное» по-прежнему отправляет ВСЕ отмеченные файлы', async function () {
        Hf._actions.selectBySuggested();
        const before = bundleRequests().length;
        const ok = await Hf._actions.startBundle();
        assert.strictEqual(ok, true, 'startBundle не запустился');
        const calls = bundleRequests();
        assert.strictEqual(calls.length, before + 1, 'запрос bundle не отправлен');
        const payload = JSON.parse(calls[calls.length - 1].body);
        assert.strictEqual(payload.files.length, 3, 'в bundle ушли не все отмеченные файлы');
    });

    // --- 5. СТАТИКА: разметка, CSS, i18n ------------------------------------
    await check('9. разметка: корзина ПЕРВАЯ и выше поиска/файлов, файлы без кнопок', function () {
        const html = fs.readFileSync(path.join(__dirname, '..', 'index.html'), 'utf8');
        const tabStart = html.indexOf('id="imTabHf"');
        const cartStart = html.indexOf('id="imHfCartCard"');
        const searchStart = html.indexOf('id="imHfQuery"');
        const filesStart = html.indexOf('id="imHfFilesCard"');
        assert.ok(tabStart !== -1 && cartStart !== -1 && searchStart !== -1 && filesStart !== -1,
            'нет одной из карточек вкладки HF');
        assert.ok(cartStart > tabStart, 'корзина не внутри вкладки HF');
        assert.ok(cartStart < searchStart, 'корзина стоит НИЖЕ поиска');
        assert.ok(cartStart < filesStart, 'корзина стоит НИЖЕ карточки файлов');
        const cartHtml = html.slice(cartStart, searchStart);
        ['imHfCartTarget', 'imHfCartList', 'imHfCartTotals', 'imHfDownloadSelected',
            'imHfDownloadMissing', 'imgBundleName', 'imgBundleFamily', 'imgHfToken',
            'imHfSelectionSummarySticky', 'imHfVerifyVerdict']
            .forEach(function (id) {
                assert.ok(cartHtml.indexOf('id="' + id + '"') !== -1, 'в корзине нет #' + id);
            });
        ['download', 'download-missing', 'verify', 'clear'].forEach(function (act) {
            assert.ok(cartHtml.indexOf('data-imh-action="' + act + '"') !== -1,
                'в корзине нет действия ' + act);
        });
        const filesHtml = html.slice(filesStart, html.indexOf('id="imTabLoaded"', filesStart));
        assert.strictEqual(filesHtml.indexOf('data-imh-action="download"'), -1,
            'в карточке файлов осталась вторая кнопка скачивания');
        assert.strictEqual(html.indexOf('id="imgBundleStart"'), -1,
            'в разметке остался старый id imgBundleStart (вторая точка скачивания)');
    });

    await check('10. CSS: .imh-cart липкая, старых .imh-files-actions больше нет', function () {
        const css = fs.readFileSync(path.join(__dirname, '..', 'css', 'pages.css'), 'utf8');
        assert.ok(/\.imh-cart\s*\{[\s\S]*?position:\s*sticky/.test(css), 'нет position: sticky у .imh-cart');
        assert.ok(/\.imh-cart-item\s*\{[\s\S]*?grid-template-columns/.test(css), 'нет сетки строки корзины');
        assert.strictEqual(css.indexOf('.imh-files-actions'), -1, 'в CSS остались правила старой панели');
        assert.ok(/#imTabHf\s*>\s*\.card\s*\{\s*[\s\S]*?overflow:\s*visible/.test(css),
            'нет overflow: visible у карточки-обёртки (sticky не сработает)');
    });

    await check('11. i18n: все ключи корзины есть в ru.js и en.js', function () {
        const ru = fs.readFileSync(path.join(__dirname, '..', 'js', 'i18n', 'ru.js'), 'utf8');
        const en = fs.readFileSync(path.join(__dirname, '..', 'js', 'i18n', 'en.js'), 'utf8');
        const keys = Object.keys(RU).filter(function (k) { return k.indexOf('imageModels.cart_') === 0; });
        assert.ok(keys.length >= 15, 'в сценарии меньше 15 ключей корзины: ' + keys.length);
        keys.forEach(function (k) {
            assert.ok(ru.indexOf('"' + k + '":') !== -1, 'нет ru-перевода ' + k);
            assert.ok(en.indexOf('"' + k + '":') !== -1, 'нет en-перевода ' + k);
        });
        assert.ok(ru.indexOf('"imageModels.hf_clear_selection": "Очистить"') !== -1,
            'кнопка очистки корзины подписана не «Очистить»');
    });

    await check('12. модуль: режим «только недостающее» и перечитывание реестра', function () {
        const src = fs.readFileSync(path.join(__dirname, '..', 'js', 'modules', 'image-models-hf.js'), 'utf8');
        ["act === 'download-missing'", "act === 'scan-backend'", 'opts.onlyMissing',
            'refreshBackendFiles()', 'presenceOf(', 'buildBackendFileIndex(']
            .forEach(function (tok) {
                assert.ok(src.indexOf(tok) !== -1, 'в модуле нет ' + tok);
            });
    });

    // Опрос прогресса bundle (setInterval) держал бы процесс Node живым.
    Hf._actions.stopPolling();
    Hf.unmount();
    console.log('\n' + (failures > 0 ? 'FAILED: ' + failures : 'OK: ' + passed + ' checks passed'));
    process.exit(failures > 0 ? 1 : 0);
})().catch((e) => {
    console.error('FATAL', e && e.stack || e);
    process.exit(1);
});

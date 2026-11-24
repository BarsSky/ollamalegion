// image-hf-sticky-selection-r696.test.js — R696 (2026-11-23): видимая сводка
// выбора и кнопка «Скачать выбранное» на вкладке Image-модели → HuggingFace.
//
// ЖИВАЯ ЖАЛОБА ОПЕРАТОРА (дословно): «при скачивании с HuggingFace приходится
// весь список прокручивать вниз, не хватает подсказки, что выкачивается всё, что
// выбрано галочками».
//
// ЧТО ПРОВЕРЯЕТСЯ:
//   1. сводка «выбрано N файлов · X» стоит РЯДОМ с кнопкой скачивания и
//      обновляется СРАЗУ при клике по галочке (без сети и без перерисовки списка);
//   2. кнопка подписана «Скачать выбранное (N · X)» и отключается при пустом выборе;
//   3. скачивание отправляет РОВНО отмеченные файлы (роли — из селектов);
//   4. для списка 18+ строк (больше экрана) счётчик и кнопка существуют независимо
//      от того, где в списке файл (липкая панель — разметка index.html; геометрия
//      замеряется отдельно через CDP/Playwright);
//   5. вердикт совместимости набора до скачивания: POST /hf/verify получает
//      выбранные файлы, ответ показывается текстом «сможет/НЕ сможет».
//
// Запуск: node webui/tests/image-hf-sticky-selection-r696.test.js
'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');

let failures = 0;
let passed = 0;
async function check(name, fn) {
    try { await fn(); passed++; console.log('  ✓ ' + name); }
    catch (e) { failures++; console.error('  ✗ ' + name + '\\n    ' + e.message); }
}
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// --- DOM-заглушка (минимальная: id-элементы + слоты раскрытой карточки) ------

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

// Карточка файлов лежит в хосте таба, как в index.html (переносится в слот).
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

global.window = global;
global.document = {
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

// Переводы: только используемые модулем ключи (недостающие возвращаются id).
const RU = {
    'gguf.search_btn': 'Найти',
    'gguf.no_results': 'Результатов не найдено',
    'gguf.results_count': 'Найдено моделей: {n}',
    'gguf.downloads': 'Загрузок',
    'gguf.likes': 'Лайки',
    'gguf.last_updated': 'Обновлён',
    'common.loading': 'Загрузка...',
    'imageModels.hf_pick_files': 'Показать файлы',
    'imageModels.hf_hide_files': 'Свернуть файлы',
    'imageModels.hf_files_title': 'Файлы репозитория',
    'imageModels.hf_select_suggested': 'Отметить по ролям',
    'imageModels.hf_clear_selection': 'Снять отметки',
    'imageModels.hf_selected_count': 'Выбрано файлов: {n}',
    'imageModels.hf_selected_summary': 'Выбрано: {n} файлов · {size}',
    'imageModels.hf_nothing_selected': 'Ничего не выбрано — скачается только отмеченное',
    'imageModels.hf_download_selected': 'Скачать выбранное',
    'imageModels.hf_verify_compat': 'Проверить совместимость',
    'imageModels.hf_verify_running': 'Проверяю заголовки выбранных файлов…',
    'imageModels.hf_verify_failed': 'Не удалось проверить набор: {error}',
    'imageModels.hf_detected_role': 'по заголовку: {role}',
    'imageModels.hf_suggested': 'предложено',
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
    'imageModels.hf_no_files': 'В этом репозитории нет файлов для скачивания',
    'imageModels.hf_no_files_hint': 'Возьмите GGUF-квант.',
    'imageModels.hf_files_failed': 'Не удалось прочитать файлы репозитория',
    'image.bundle_started': 'Загрузка bundle начата',
    'image.bundle_failed': 'Загрузка bundle не удалась',
    'gguf.loading_indicator': 'Загрузка',
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
const REPO = 'AtomicChat/Qwen-Image-2.1-Turbo-Uncensored-GGUF';
// 18 файлов: главный плюс кванты-дубликаты, VAE и энкодер (живая структура).
const FILES = [
    { path: 'Qwen-Image-2.1-Turbo-Uncensored-AD-Q4_K.gguf', sizeBytes: 5287517120, suggestedRole: 'diffusion' },
    { path: 'Qwen-Image-2.1-Turbo-Uncensored-AD-Q6_K.gguf', sizeBytes: 7490550720, suggestedRole: 'diffusion' },
    { path: 'Qwen-Image-2.1-Turbo-Uncensored-AD-Q5_K.gguf', sizeBytes: 6281292736, suggestedRole: 'diffusion' },
    { path: 'Qwen-Image-2.1-Turbo-Uncensored-AD-Q3_K.gguf', sizeBytes: 4184140736, suggestedRole: 'diffusion' },
    { path: 'Qwen-Image-2.1-Turbo-Uncensored-AD-Q2_K.gguf', sizeBytes: 3336289216, suggestedRole: 'diffusion' },
    { path: 'Qwen-Image-2.1-Turbo-Uncensored-Q8_0.gguf', sizeBytes: 8709520096, suggestedRole: 'diffusion' },
    { path: 'Qwen-Image-2.1-Turbo-Uncensored-BF16.gguf', sizeBytes: 16388045536, suggestedRole: 'diffusion' },
    { path: 'mmproj-Qwen-Image-2.1-Turbo-Uncensored-F16.gguf', sizeBytes: 1159029984, suggestedRole: 'llm' },
    { path: 'mmproj-Qwen-Image-2.1-Turbo-Uncensored-BF16.gguf', sizeBytes: 1162568928, suggestedRole: 'llm' },
    { path: 'vae/qwen_image_2.1_vae_bf16.safetensors', sizeBytes: 675509688, suggestedRole: 'vae' },
    { path: 'text_encoders/qwen3vl_8b_bf16.safetensors', sizeBytes: 17534334616, suggestedRole: 'llm' },
    { path: 'text_encoders/qwen3vl_8b_int8_convrot.safetensors', sizeBytes: 9350798360, suggestedRole: 'llm' },
    { path: 'qwen-image-2.1-uncensored-lora.safetensors', sizeBytes: 33586704, suggestedRole: 'lora' },
    { path: 'qwen-image-2.1-UC-MLX-4bit.safetensors', sizeBytes: 4002363741, suggestedRole: 'diffusion' },
    { path: 'qwen-image-2.1-UC-NVFP4.safetensors', sizeBytes: 4197574696, suggestedRole: 'diffusion' },
    { path: 'qwen-image-2.1-UC-fp8.safetensors', sizeBytes: 7116818183, suggestedRole: 'diffusion' },
    { path: 'qwen-image-2.1-UC-int8_convrot.safetensors', sizeBytes: 7256796840, suggestedRole: 'diffusion' },
    { path: 'qwen-image-2.1-UC-BF16.gguf', sizeBytes: 14230272800, suggestedRole: 'diffusion' },
];
assert.ok(FILES.length >= 16, 'сценарий должен быть «длинный список» (16+ файлов)');

let verifyAnswer = {
    compat: {
        verdict: 'unsupported',
        runnable: false,
        engineMode: 'unknown',
        family: 'other',
        reason: 'файл "Qwen-Image-2.1-Turbo-Uncensored-AD-Q4_K.gguf" — это text encoder (LLM) (заголовок: qwen3vl), а не diffusion-модель',
        missing: ['diffusion'],
        files: [{
            filename: 'Qwen-Image-2.1-Turbo-Uncensored-AD-Q4_K.gguf',
            role: 'diffusion', detectedRole: 'llm', match: false,
        }],
    },
};

global.fetch = function (url, init) {
    const rec = { url: String(url), method: (init && init.method) || 'GET', body: init && init.body };
    requests.push(rec);
    const body = (function () {
        if (rec.url.indexOf('/hf/search') !== -1) {
            return { count: 1, results: [{ id: REPO, downloads: 624779, likes: 883, lastModified: '2026-11-20' }] };
        }
        if (rec.url.indexOf('/hf/files') !== -1) return { count: FILES.length, files: FILES };
        if (rec.url.indexOf('/hf/plan') !== -1) return { verdict: 'incomplete', summary: 'не хватает: VAE', engineMode: 'unknown', roles: [], steps: [] };
        if (rec.url.indexOf('/hf/probe') !== -1) return { verdict: 'unknown', role: 'llm', reason: 'это text encoder' };
        if (rec.url.indexOf('/hf/verify') !== -1) return verifyAnswer;
        if (rec.url.indexOf('/hf/bundle') !== -1) return { status: 'started' };
        if (rec.url.indexOf('/hf/downloads') !== -1) return { active: [], history: [], bundles: [] };
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
const ctx = { apiBase: 'http://balancer.test:18081', backendId: 'img-1', tab: 'hf', headers: {} };

function verifyRequests() {
    return requests.filter((r) => r.url.indexOf('/hf/verify') !== -1);
}
function bundleRequests() {
    return requests.filter((r) => r.url.indexOf('/hf/bundle') !== -1);
}
function openRepo() {
    return Hf._actions.pickRepo(REPO);
}

(async function main() {
    Hf.render(ctx);
    getEl('imHfQuery').value = 'qwen';
    await Hf._actions.search();
    await openRepo();
    await sleep(30);

    const summary = () => getEl('imHfSelectionSummarySticky').textContent;
    const headerSummary = () => getEl('imHfSelectionSummary').textContent;
    const downloadBtn = () => getEl('imHfDownloadSelected');

    await check('1. сводка выбора видна рядом с кнопкой и называет число и объём', function () {
        const text = summary();
        assert.ok(/\d+/.test(text), 'в сводке нет числа: ' + text);
        assert.ok(/GB|MB|B/.test(text), 'в сводке нет объёма: ' + text);
        assert.ok(getEl('imHfSelectionSummary').textContent.indexOf('файлов') !== -1,
            'сводка в шапке карточки пропала: ' + headerSummary());
    });

    await check('2. кнопка подписана «Скачать выбранное» и содержит счётчик', function () {
        const html = downloadBtn().innerHTML;
        assert.ok(html.indexOf('Скачать выбранное') !== -1, 'нет подписи кнопки: ' + html);
        assert.ok(/\(\d+/.test(html), 'в кнопке нет счётчика: ' + html);
        assert.strictEqual(downloadBtn().disabled, false, 'при выбранных файлах кнопка не должна быть disabled');
    });

    await check('3. клик по галочке обновляет счётчик СРАЗУ (без сети)', async function () {
        const before = Hf._actions.selectedBytes();
        const beforeCount = (summary().match(/(\d+)/) || [])[1];
        const path = 'qwen-image-2.1-UC-BF16.gguf';
        getEl('image-page').dispatch('change', {
            target: {
                getAttribute: function (n) { return n === 'data-imh-check' ? path : null; },
                checked: true,
            },
        });
        const afterCount = (summary().match(/(\d+)/) || [])[1];
        assert.strictEqual(Number(afterCount), Number(beforeCount) + 1,
            'счётчик не вырос после отметки файла: ' + beforeCount + ' → ' + afterCount);
        assert.ok(Hf._actions.selectedBytes() > before, 'объём выбора не вырос');
        assert.ok(/\(\d+ · /.test(downloadBtn().innerHTML), 'счётчик кнопки не обновился: ' + downloadBtn().innerHTML);
    });

    await check('4. «Снять отметки» → ноль, кнопка disabled и подсказка', function () {
        Hf._actions.clearSelection();
        assert.strictEqual(downloadBtn().disabled, true, 'при пустом выборе кнопка должна быть disabled');
        assert.ok(summary().indexOf('Ничего не выбрано') !== -1, 'нет подсказки о пустом выборе: ' + summary());
        const html = downloadBtn().innerHTML;
        assert.ok(/\(0/.test(html), 'счётчик не обнулился: ' + html);
    });

    await check('5. «Отметить по ролям» возвращает выбор и счётчик', function () {
        Hf._actions.selectBySuggested();
        assert.ok(/\(\d+/.test(downloadBtn().innerHTML), 'после «Отметить по ролям» нет счётчика');
        assert.strictEqual(downloadBtn().disabled, false, 'кнопка осталась disabled');
    });

    await check('6. вердикт набора до скачивания: POST /hf/verify с отмеченными файлами', async function () {
        await sleep(600); // debounce проверки
        const calls = verifyRequests();
        assert.ok(calls.length >= 1, 'модуль не проверил совместимость набора');
        const last = calls[calls.length - 1];
        const payload = JSON.parse(last.body);
        assert.strictEqual(payload.family, 'sd15', 'семейство не передано');
        assert.ok(Array.isArray(payload.files) && payload.files.length > 0, 'не переданы файлы');
        const checked = Object.keys(Hf._state.selection).filter(function (p) {
            return Hf._state.selection[p].checked;
        }).sort();
        const sent = payload.files.map(function (f) { return f.filename; }).sort();
        assert.deepStrictEqual(sent, checked, 'в проверку ушли не ровно отмеченные файлы');
        const node = getEl('imHfVerifyVerdict');
        assert.strictEqual(node.style.display, '', 'вердикт не показан');
        assert.ok(node.innerHTML.indexOf('НЕ сможет загрузить') !== -1, 'вердикт не показан словами: ' + node.innerHTML);
        assert.ok(node.innerHTML.indexOf('text encoder') !== -1, 'в вердикте нет причины: ' + node.innerHTML);
        assert.ok(node.innerHTML.indexOf('не хватает') !== -1, 'в вердикте нет недостающих ролей: ' + node.innerHTML);
    });

    await check('7. скачивание отправляет РОВНО отмеченные файлы', async function () {
        getEl('imgBundleName').value = 'qwen-te-set';
        getEl('imgBundleFamily').value = 'qwen_image';
        Hf._state.bundleFamily = 'qwen_image';
        const ok = await Hf._actions.startBundle();
        assert.strictEqual(ok, true, 'startBundle не запустился');
        const calls = bundleRequests();
        assert.ok(calls.length === 1, 'запрос bundle не отправлен');
        const payload = JSON.parse(calls[0].body);
        assert.strictEqual(payload.family, 'qwen_image');
        const checked = Object.keys(Hf._state.selection).filter(function (p) {
            return Hf._state.selection[p].checked;
        }).sort();
        const sent = payload.files.map(function (f) { return f.filename; }).sort();
        assert.deepStrictEqual(sent, checked, 'скачиваются не ровно отмеченные файлы');
        assert.ok(payload.files.every(function (f) { return !!f.role; }), 'у файлов нет ролей');
    });

    await check('8. полный набор → вердикт «сможет загрузить: да»', async function () {
        verifyAnswer = {
            compat: {
                verdict: 'ready', runnable: true, engineMode: 'diffusion-model',
                family: 'qwen_image', reason: 'движок узнаёт «Qwen Image 2.1» и запустит набор',
                missing: [], files: [],
            },
        };
        await Hf._actions.verifySelection(true);
        const html = getEl('imHfVerifyVerdict').innerHTML;
        assert.ok(html.indexOf('сможет загрузить: да') !== -1, 'нет положительного вердикта: ' + html);
    });

    await check('9. длинный список (18 строк): сводка не зависит от позиции файла', function () {
        assert.ok(Hf._state.files.length >= 16, 'в сценарии меньше 16 файлов');
        // Сводка/кнопка — отдельные узлы панели, а не строка списка: их текст не
        // зависит от того, какой файл отрисован последним.
        const before = summary();
        Hf._state.selection['qwen-image-2.1-UC-BF16.gguf'].checked = true;
        Hf._actions.renderSelectionSummary();
        assert.notStrictEqual(summary(), before, 'сводка не обновилась после изменения выбора');
    });

    // 10. СТАТИКА: панель обязана быть в РАЗМЕТКЕ, а sticky-правило — в CSS.
    // R749 (2026-11-25): панель переехала из карточки файлов в КОРЗИНУ наверху
    // вкладки (единственная точка управления скачиванием), поэтому проверяем
    // новый адрес: #imHfCartCard стоит ДО карточки файлов и содержит кнопки.
    // Без этого модуль может «работать» в тесте DOM-заглушки, а у оператора
    // панели просто нет (именно так выглядел исходный дефект R696: кнопка была
    // в ОТДЕЛЬНОЙ карточке ниже списка).
    await check('10. разметка: корзина выше карточки файлов, sticky в CSS', function () {
        const html = fs.readFileSync(path.join(__dirname, '..', 'index.html'), 'utf8');
        const cartStart = html.indexOf('id="imHfCartCard"');
        assert.ok(cartStart !== -1, 'в index.html нет корзины #imHfCartCard');
        const cardStart = html.indexOf('id="imHfFilesCard"', cartStart);
        assert.ok(cardStart > cartStart, 'корзина обязана стоять выше карточки файлов');
        const cartHtml = html.slice(cartStart, cardStart);
        ['imHfDownloadSelected', 'imHfSelectionSummarySticky', 'imHfVerifyVerdict',
            'imgBundleName', 'imgBundleFamily', 'imHfCartTarget', 'imHfCartList']
            .forEach(function (id) {
                assert.ok(cartHtml.indexOf('id="' + id + '"') !== -1,
                    'в корзине нет #' + id + ' (панель выбора не видна у оператора)');
            });
        assert.ok(cartHtml.indexOf('data-imh-action="download"') !== -1,
            'кнопка корзины не привязана к действию скачивания');
        assert.ok(cartHtml.indexOf('data-imh-action="verify"') !== -1,
            'в корзине нет кнопки проверки совместимости');
        // В карточке файлов кнопок скачивания быть не должно: две точки
        // управления - ровно тот дефект, который чинит R749.
        const filesHtml = html.slice(cardStart, html.indexOf('id="imTabLoaded"', cardStart));
        assert.strictEqual(filesHtml.indexOf('data-imh-action="download"'), -1,
            'в карточке файлов осталась вторая кнопка скачивания');
        const css = fs.readFileSync(path.join(__dirname, '..', 'css', 'pages.css'), 'utf8');
        const rule = /\.imh-cart\s*\{[\s\S]*?position:\s*sticky/.exec(css);
        assert.ok(rule, 'в pages.css нет position: sticky у .imh-cart');
        // sticky не работает внутри предка с overflow:hidden — карточка-обёртка
        // вкладки HF обязана разрешать overflow (замер CDP: иначе панель уезжает).
        assert.ok(/#imTabHf\s*>\s*\.card\s*\{\s*[\s\S]*?overflow:\s*visible/.test(css),
            'нет правила overflow: visible для карточки-обёртки вкладки HF (sticky не сработает)');
    });

    // Опрос прогресса bundle (setInterval) держал бы процесс Node живым: гасим
    // его и снимаем подписки, иначе файл-тест висит до таймаута раннера.
    Hf._actions.stopPolling();
    Hf.unmount();
    console.log('\n' + (failures > 0 ? 'FAILED: ' + failures : 'OK: ' + passed + ' checks passed'));
    process.exit(failures > 0 ? 1 : 0);
})();

#!/usr/bin/env node
/*
 * image-model-files-note-r757.test.js — R757 (2026-11-25)
 *
 * ЖИВАЯ ЖАЛОБА: «во вкладке по image-моделям в скачанных моделях у той, что не
 * хватало элементов для запуска, кнопка „докачать необходимое“ никак не
 * реагировала».
 *
 * ЧТО БЫЛО. Кнопка рисовалась с атрибутом `disabled`, а объяснение жило только в
 * `title` (всплывающая подсказка). Нажатие disabled-кнопки не порождает события
 * вообще — «никак не реагировала» было буквально верно. Хуже того, самый частый
 * случай даже не про файлы: не хватающие для запуска роли (VAE / text encoder)
 * есть в вердикте совместимости (`compat.missing`), но файлов для них в профиле
 * НЕТ — их добавляют компонентами из каталога (R710). Панель об этом молчала.
 *
 * ЧТО ПРОВЕРЯЕТСЯ:
 *   1. кнопка НЕ disabled — нажатие всегда даёт событие;
 *   2. причина видна ТЕКСТОМ в панели (не только в title);
 *   3. при нехватке ролей причина называет роли и говорит, где их добавить;
 *   4. при неизвестном источнике причина называет это;
 *   5. когда всё на узле — так и написано;
 *   6. нажатие в состоянии «нечего» даёт тост с той же причиной (реакция есть);
 *   7. когда есть что докачивать — поведение прежнее (очередь, без note).
 *
 * ЗАПУСК: node webui/tests/image-model-files-note-r757.test.js
 */
'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');

const REPO_ROOT = path.resolve(__dirname, '..', '..');

let passed = 0, failures = 0;
function check(name, fn) {
    return Promise.resolve()
        .then(fn)
        .then(function () { passed++; console.log('  ✓ ' + name); })
        .catch(function (e) {
            failures++;
            console.error('  ✗ ' + name + ' — ' + (e && e.message));
        });
}
function sleep(ms) { return new Promise(function (r) { setTimeout(r, ms); }); }

// --- минимальный DOM ---------------------------------------------------------
function makeNode(tag) {
    return {
        tagName: String(tag || 'div').toUpperCase(),
        innerHTML: '', textContent: '', className: '', hidden: false,
        style: {}, _listeners: {},
        addEventListener: function (ev, fn) { this._listeners[ev] = fn; },
        click: function () { if (this._listeners.click) this._listeners.click({}); },
        closest: function () { return null; },
    };
}
const elements = Object.create(null);
function getEl(id) {
    if (!Object.prototype.hasOwnProperty.call(elements, id)) elements[id] = makeNode('div');
    return elements[id];
}

global.window = global;
global.document = {
    getElementById: getEl,
    querySelector: function () { return null; },
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
global.WEBUI_CONFIG = { API_BASE: 'http://balancer.test:18081', API_TOKEN: '' };
global.Api = { getAuthHeaders: function () { return { 'Content-Type': 'application/json' }; } };
global.addEventListener = function () { };
global.removeEventListener = function () { };

const toasts = [];
global.Toast = { show: function (o) { toasts.push(o); } };
global.ImageModelsHf = { pure: { roleLabel: function (role) { return 'ROLE:' + role; } } };
const requests = [];
global.ImagePage = {
    _state: { models: [] },
    _actions: {
        refresh: function () { return Promise.resolve(true); },
        closeModelFiles: function () { }
    }
};
global.fetch = function (url, init) {
    requests.push({ url: String(url), method: (init && init.method) || 'GET', body: init && init.body });
    return Promise.resolve({
        ok: true, status: 200,
        text: function () { return Promise.resolve(JSON.stringify({})); },
    });
};

require(path.join(REPO_ROOT, 'webui', 'js', 'modules', 'image-model-files.js'));
const Mod = global.ImageModelFiles;
assert.ok(Mod, 'window.ImageModelFiles должен быть экспортирован');

function clickAction(action) {
    const attrs = { 'data-imf-action': action };
    const target = {
        closest: function () {
            return { getAttribute: function (n) { return Object.prototype.hasOwnProperty.call(attrs, n) ? attrs[n] : null; } };
        },
    };
    Mod._state.host.onclick({ target: target });
}

function panelHtml() { return getEl('imModelFilesHost').innerHTML; }

function openModel(model) {
    Mod._state.busy = false;
    Mod._state.queue = [];
    Mod._state.status = '';
    Mod.render({ backendId: 'imageworker', model: model, hostId: 'imModelFilesHost' });
}

// Набор, у которого не хватает ролей для запуска: файл diffusion на узле, а
// VAE и text encoder в профиле ОТСУТСТВУЮТ как файлы (самый частый живой случай).
const MODEL_MISSING_ROLES = {
    name: 'qwen-edit-2511',
    family: 'qwen_image',
    size_bytes: 4096,
    bundle_path: '/models/qwen-edit-2511',
    compat: {
        verdict: 'incomplete', runnable: false,
        missing: ['VAE', 'text encoder (LLM)'],
        reason: 'набор неполный',
    },
    files: [
        { role: 'diffusion', repo: 'acme/qwen', filename: 'q4.gguf', revision: 'main', sizeBytes: 4096, present: true, diskSizeBytes: 4096, partialBytes: 0 },
    ],
};

// Все файлы профиля на узле, но роли для запуска хватает не полностью — вердикт
// тоже может так сказать; проверяем ветку «всё на узле».
const MODEL_ALL_PRESENT = {
    name: 'sd15-q4-0',
    family: 'sd15',
    size_bytes: 2048,
    bundle_path: '/models/sd15-q4-0',
    compat: { verdict: 'ok', runnable: true, missing: [], reason: 'ok' },
    files: [
        { role: 'diffusion', repo: 'acme/sd', filename: 'sd15-q4.gguf', revision: 'main', sizeBytes: 2048, present: true, diskSizeBytes: 2048, partialBytes: 0 },
    ],
};

// Отсутствующий файл без источника (в profile.json нет repo).
const MODEL_NO_SOURCE = {
    name: 'orphan-group',
    family: 'sdxl',
    size_bytes: 1024,
    bundle_path: '/models/orphan-group',
    compat: { verdict: 'incomplete', runnable: false, missing: [], reason: 'набор неполный' },
    files: [
        { role: 'diffusion', repo: 'acme/x', filename: 'x.gguf', revision: 'main', sizeBytes: 1024, present: true, diskSizeBytes: 1024, partialBytes: 0 },
        { role: 'vae', repo: '', filename: 'vae.safetensors', revision: '', sizeBytes: 512, present: false, partialBytes: 0 },
    ],
};

// Есть что докачивать: файл роли отсутствует, источник известен.
const MODEL_DOWNLOADABLE = {
    name: 'flux-schnell',
    family: 'flux',
    size_bytes: 8192,
    bundle_path: '/models/flux-schnell',
    compat: { verdict: 'incomplete', runnable: false, missing: ['VAE'], reason: 'набор неполный' },
    files: [
        { role: 'diffusion', repo: 'acme/flux', filename: 'flux.gguf', revision: 'main', sizeBytes: 4096, present: true, diskSizeBytes: 4096, partialBytes: 0 },
        { role: 'vae', repo: 'acme/flux', filename: 'ae.safetensors', revision: 'main', sizeBytes: 4096, present: false, partialBytes: 0 },
    ],
};

(async function main() {
    await check('1. нечего докачивать: кнопка НЕ disabled (нажатие даёт событие)', function () {
        openModel(MODEL_MISSING_ROLES);
        const html = panelHtml();
        const btnStart = html.indexOf('data-imf-action="download-missing"');
        assert.ok(btnStart !== -1, 'кнопки «Докачать недостающее» нет в панели: ' + html);
        // Атрибут disabled на этой кнопке — ровно то, из-за чего она «не реагировала».
        const btnTag = html.slice(html.lastIndexOf('<button', btnStart), html.indexOf('>', btnStart) + 1);
        assert.ok(btnTag.indexOf('disabled') === -1,
            'кнопка disabled — нажатие не порождает события, это и была живая жалоба: ' + btnTag);
    });

    await check('2. причина видна ТЕКСТОМ в панели (раньше — только в title)', function () {
        const html = panelHtml();
        assert.ok(html.indexOf('imf-note') !== -1, 'нет блока причины под сводкой: ' + html);
        assert.ok(html.indexOf('Не хватает ролей') !== -1, 'причина не названа текстом: ' + html);
    });

    await check('3. названы недостающие роли и где их добавить', function () {
        const html = panelHtml();
        assert.ok(html.indexOf('VAE') !== -1 && html.indexOf('text encoder') !== -1,
            'в причине нет конкретных ролей: ' + html);
        assert.ok(html.indexOf('компонент') !== -1,
            'не сказано, ЧТО делать (добавить компоненты из каталога / R710): ' + html);
    });

    await check('4. нажатие в состоянии «нечего» даёт тост с причиной (реакция есть)', async function () {
        toasts.length = 0;
        requests.length = 0;
        clickAction('download-missing');
        await sleep(20);
        assert.ok(toasts.length >= 1, 'нажатие не дало ни одного сообщения — это и есть «никак не реагировала»');
        const msg = String(toasts[toasts.length - 1].message || '');
        assert.ok(msg.indexOf('VAE') !== -1, 'в тосте нет причины (какие роли не хватают): ' + msg);
        const posts = requests.filter(function (r) { return r.method === 'POST'; });
        assert.strictEqual(posts.length, 0, 'POST не должен уходить, когда докачивать нечего: ' + JSON.stringify(posts));
    });

    await check('5. всё на узле: так и написано, и нажатие объясняет', async function () {
        openModel(MODEL_ALL_PRESENT);
        const html = panelHtml();
        assert.ok(html.indexOf('все файлы профиля уже на узле') !== -1, 'нет причины «всё на узле»: ' + html);
        assert.ok(html.indexOf('Докачивать нечего') !== -1, 'подпись кнопки не отражает состояние: ' + html);
        toasts.length = 0;
        clickAction('download-missing');
        await sleep(20);
        assert.ok(toasts.length >= 1, 'нажатие не дало сообщения');
        assert.ok(String(toasts[0].message).indexOf('все файлы профиля') !== -1,
            'тост не объясняет причину: ' + toasts[0].message);
    });

    await check('6. отсутствующий файл без источника: причина называет это', function () {
        openModel(MODEL_NO_SOURCE);
        const html = panelHtml();
        assert.ok(html.indexOf('неизвестен источник') !== -1,
            'нет причины «неизвестен источник» (в profile.json нет repo): ' + html);
    });

    await check('7. есть что докачивать: прежнее поведение, note нет', async function () {
        openModel(MODEL_DOWNLOADABLE);
        const html = panelHtml();
        assert.ok(html.indexOf('Докачать недостающее') !== -1, 'подпись кнопки не прежняя: ' + html);
        assert.ok(html.indexOf('(1)') !== -1, 'кнопка не считает очередь: ' + html);
        assert.ok(html.indexOf('imf-note') === -1, 'note не должен появляться, когда есть что докачивать: ' + html);
        Mod._state.busy = false;
        Mod._state.queue = [];
        requests.length = 0;
        clickAction('download-missing');
        await sleep(30);
        // Очередь обрабатывается по одному: первый файл сразу уходит в POST, и
        // очередь пустеет — поэтому проверяем сам запрос, а не длину массива.
        const posts = requests.filter(function (r) { return r.method === 'POST' && r.url.indexOf('/files') !== -1; });
        assert.strictEqual(posts.length, 1, 'POST на докачку отсутствующего файла не ушёл: ' +
            JSON.stringify(requests.map(function (r) { return r.method + ' ' + r.url; })));
        const body = JSON.parse(posts[0].body);
        assert.strictEqual(body.role, 'vae', 'докачивается не та роль: ' + posts[0].body);
        assert.strictEqual(body.filename, 'ae.safetensors', 'докачивается не тот файл: ' + posts[0].body);
        Mod._state.busy = false;
        Mod._state.queue = [];
        Mod._actions.close();
    });

    await check('8. ключи причины есть в ru.js и en.js (симметрия)', function () {
        const ru = fs.readFileSync(path.join(REPO_ROOT, 'webui', 'js', 'i18n', 'ru.js'), 'utf8');
        const en = fs.readFileSync(path.join(REPO_ROOT, 'webui', 'js', 'i18n', 'en.js'), 'utf8');
        ['imageModels.files_download_missing_none', 'imageModels.files_note_missing_roles',
            'imageModels.files_note_unknown_source', 'imageModels.files_note_all_present',
            'imageModels.files_nothing_missing'].forEach(function (k) {
            assert.ok(ru.indexOf('"' + k + '":') !== -1, 'нет ru-перевода ' + k);
            assert.ok(en.indexOf('"' + k + '":') !== -1, 'нет en-перевода ' + k);
        });
    });

    console.log('');
    if (failures > 0) {
        console.log('ИТОГ: провалено проверок — ' + failures);
        process.exit(1);
    }
    console.log('ИТОГ: все проверки пройдены (' + passed + ')');
})().catch(function (e) {
    console.error('FATAL', e && e.stack || e);
    process.exit(1);
});

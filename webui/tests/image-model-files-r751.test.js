// image-model-files-r751.test.js — R751 (2026-11-25): панель файлов группы на
// странице «Image-модели» → «Модели на диске».
//
// ЖИВОЙ ДЕФЕКТ. Управлять файлами ВНУТРИ скачанной группы было нельзя: только
// удалить bundle целиком. Недокачанный/удалённый файл оператор видел лишь по
// «модель не загрузилась»; заменить квант diffusion — только перекачать всю
// группу; «докачать недостающее» относилось лишь к выбору на вкладке HF.
//
// ЧТО ПРОВЕРЯЕТСЯ:
//   1. панель рисует ВЕСЬ состав: роль, имя, размер, состояние (на узле / файла
//      нет / частично / состояние не сообщено / вне профиля);
//   2. «Докачать» шлёт POST .../files с ролью/репо/файлом и replace=false;
//   3. «Перекачать» — replace=true (после подтверждения);
//   4. «Удалить с узла» — DELETE с filename в query (путь не ломает URL);
//   5. «Докачать недостающее» ставит в очередь отсутствующие файлы с известным
//      источником;
//   6. страница (image-page.js) сохраняет files/compat из ответа воркера и имеет
//      кнопку «Файлы»;
//   7. i18n: каждый ключ панели есть в ru.js и en.js.
//
// Запуск: node webui/tests/image-model-files-r751.test.js
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

// --- DOM-заглушка -----------------------------------------------------------

function makeNode(tag) {
    const listeners = {};
    const node = {
        tagName: String(tag || 'div').toUpperCase(),
        style: {},
        textContent: '',
        _html: '',
        parentNode: null,
        onclick: null,
        addEventListener: function (t, fn) { (listeners[t] = listeners[t] || []).push(fn); },
        removeEventListener: function () { },
        getAttribute: function () { return null; },
        closest: function () { return null; },
    };
    Object.defineProperty(node, 'innerHTML', {
        get: function () { return this._html; },
        set: function (v) { this._html = String(v); },
    });
    return node;
}

const elements = {};
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
// Подписи ролей — из общего модуля (одна таблица переводов в проекте).
global.ImageModelsHf = { pure: { roleLabel: function (role) { return 'ROLE:' + role; } } };

const requests = [];
let progressAnswer = { status: 'downloading', progressPct: 10, downloaded: 409, totalBytes: 4096 };
let deleteAnswer = { status: 'deleted', bytesFreed: 4096, profileChanged: true };
let refreshCalls = 0;
let freshModels = null;
global.ImagePage = {
    _state: { models: [] },
    _actions: {
        refresh: function () {
            refreshCalls++;
            if (freshModels) global.ImagePage._state.models = freshModels;
            return Promise.resolve(true);
        },
        closeModelFiles: function () { global.ImagePage._state.openFilesModel = ''; }
    }
};

global.fetch = function (url, init) {
    const rec = { url: String(url), method: (init && init.method) || 'GET', body: init && init.body };
    requests.push(rec);
    let body = {};
    if (rec.url.indexOf('/files?') !== -1 && rec.method === 'DELETE') body = deleteAnswer;
    else if (rec.url.indexOf('/files') !== -1 && rec.method === 'POST') body = { status: 'started', trackingId: 'modelfile-qwen-edit-2511-vae' };
    else if (rec.url.indexOf('/hf/progress') !== -1) body = progressAnswer;
    return Promise.resolve({
        ok: true, status: 200,
        text: function () { return Promise.resolve(JSON.stringify(body)); },
    });
};

require(path.join(__dirname, '..', 'js', 'modules', 'image-model-files.js'));
const Mod = global.ImageModelFiles;
assert.ok(Mod, 'window.ImageModelFiles должен быть экспортирован');

const MODEL = {
    name: 'qwen-edit-2511',
    family: 'qwen_image',
    size_bytes: 5 * 1024 * 1024 * 1024,
    bundle_path: '/models/qwen-edit-2511',
    compat: {
        verdict: 'incomplete', runnable: false,
        missing: ['VAE', 'text encoder (LLM)'],
        reason: 'набор неполный',
    },
    files: [
        {
            role: 'diffusion', repo: 'acme/qwen', filename: 'qwen-edit-2511-Q4_K_M.gguf',
            revision: 'main', sizeBytes: 4 * 1024 * 1024 * 1024,
            present: true, diskSizeBytes: 4 * 1024 * 1024 * 1024, partialBytes: 0,
        },
        {
            role: 'vae', repo: 'acme/qwen', filename: 'qwen_image_vae.safetensors',
            revision: 'main', sizeBytes: 96 * 1024, present: false, partialBytes: 0,
        },
        {
            role: 'llm', repo: '', filename: 'qwen3vl.safetensors',
            revision: 'main', sizeBytes: 1024, present: false, partialBytes: 512,
        },
        {
            role: '', repo: '', filename: 'old-quant-Q8_0.gguf',
            revision: '', sizeBytes: 7, present: true, diskSizeBytes: 7, unregistered: true,
        },
    ],
};

const ctx = { backendId: 'imageworker', model: MODEL, hostId: 'imModelFilesHost' };

function panelHtml() { return getEl('imModelFilesHost').innerHTML; }
function clickAction(action, file, extra) {
    const attrs = { 'data-imf-action': action };
    if (file) attrs['data-imf-file'] = file;
    if (extra) Object.keys(extra).forEach(function (k) { attrs[k] = extra[k]; });
    const target = {
        closest: function () {
            return { getAttribute: function (n) { return Object.prototype.hasOwnProperty.call(attrs, n) ? attrs[n] : null; } };
        },
    };
    Mod._state.host.onclick({ target: target });
}

(async function main() {
    // --- 1. состав панели ---------------------------------------------------
    await check('1. панель: заголовок, вердикт, сводка и строки всего состава', function () {
        assert.strictEqual(Mod.render(ctx), true, 'render вернул false');
        const html = panelHtml();
        assert.ok(html.indexOf('Файлы группы') !== -1, 'нет заголовка панели: ' + html);
        assert.ok(html.indexOf('qwen-edit-2511') !== -1, 'нет имени группы');
        assert.ok(html.indexOf('набор неполный') !== -1, 'нет причины вердикта');
        assert.ok(html.indexOf('не хватает') !== -1, 'нет недостающих ролей');
        assert.ok(html.indexOf('каталог: /models/qwen-edit-2511') !== -1, 'нет каталога группы');
        assert.ok(html.indexOf('файлов: 4') !== -1, 'сводка неверна: ' + html);
        assert.ok(html.indexOf('на узле: 1') !== -1, 'сводка не считает файлы на узле (unregistered считается отдельно): ' + html);
        assert.ok(html.indexOf('нет: 1') !== -1, 'сводка не считает отсутствующие');
        assert.ok(html.indexOf('частично: 1') !== -1, 'сводка не считает частичные');
        assert.ok(html.indexOf('вне профиля: 1') !== -1, 'сводка не считает файлы вне профиля');
        // Роли: из общего модуля (ROLE:diffusion), у unregistered — «-».
        assert.ok(html.indexOf('ROLE:diffusion') !== -1, 'подпись роли не из общего модуля');
        assert.ok(html.indexOf('ROLE:vae') !== -1, 'нет роли VAE');
        assert.ok(html.indexOf('>-' + '<') !== -1 || html.indexOf('>-</span>') !== -1,
            'у файла вне профиля должна быть роль «-»');
        // Состояния.
        assert.ok(html.indexOf('на узле') !== -1, 'нет статуса «на узле»');
        assert.ok(html.indexOf('файла нет') !== -1, 'нет статуса «файла нет»');
        assert.ok(html.indexOf('частично') !== -1, 'нет статуса «частично»');
        assert.ok(html.indexOf('вне профиля') !== -1, 'нет статуса «вне профиля»');
        // Размеры: ожидаемый у отсутствующего, «have из total» у частичного.
        assert.ok(html.indexOf('ожидается') !== -1, 'нет ожидаемого размера у отсутствующего');
        assert.ok(html.indexOf('из') !== -1 && html.indexOf('512 B') !== -1, 'нет размера частичной загрузки');
        // Кнопки по состояниям.
        assert.ok(html.indexOf('data-imf-action="download"') !== -1, 'нет «Докачать»');
        assert.ok(html.indexOf('data-imf-action="redownload"') !== -1, 'нет «Перекачать»');
        assert.ok(html.indexOf('data-imf-action="delete"') !== -1, 'нет «Удалить с узла»');
        assert.ok(html.indexOf('data-imf-action="download-missing"') !== -1, 'нет «Докачать недостающее»');
        assert.ok(html.indexOf('data-admin-only') !== -1, 'мутирующие кнопки не помечены data-admin-only');
        // Файл без repo: кнопки докачки нет, но подсказка об источнике есть.
        assert.ok(html.indexOf('источник неизвестен') !== -1, 'нет подсказки «источник неизвестен»');
    });

    await check('2. fileState/sizeText: null ≠ false, unregistered отдельно', function () {
        const P = Mod.pure;
        assert.strictEqual(P.fileState({ role: 'vae', present: true }).key, 'present');
        assert.strictEqual(P.fileState({ role: 'vae', present: false }).key, 'missing');
        assert.strictEqual(P.fileState({ role: 'vae', present: false, partialBytes: 50, sizeBytes: 100 }).key, 'partial');
        assert.strictEqual(P.fileState({ role: 'vae', present: null }).key, 'unknown');
        assert.strictEqual(P.fileState({ role: '', unregistered: true, present: true }).key, 'unregistered');
        assert.ok(P.sizeText({ present: false, sizeBytes: 1024 }).indexOf('ожидается') !== -1);
        assert.ok(P.sizeText({ present: false, sizeBytes: 1024, partialBytes: 512 }).indexOf('512') !== -1);
    });

    // --- 2. действия --------------------------------------------------------
    await check('3. «Докачать»: POST .../files с ролью/репо/файлом, replace=false', async function () {
        requests.length = 0;
        toasts.length = 0;
        Mod._state.busy = false;
        Mod._state.queue = [];
        clickAction('download', 'qwen_image_vae.safetensors',
            { 'data-imf-role': 'vae', 'data-imf-repo': 'acme/qwen' });
        await sleep(30);
        const post = requests.filter((r) => r.method === 'POST' && r.url.indexOf('/files') !== -1)[0];
        assert.ok(post, 'POST /files не отправлен: ' + JSON.stringify(requests.map((r) => r.method + ' ' + r.url)));
        const body = JSON.parse(post.body);
        assert.strictEqual(body.role, 'vae');
        assert.strictEqual(body.repo, 'acme/qwen');
        assert.strictEqual(body.filename, 'qwen_image_vae.safetensors');
        assert.strictEqual(body.replace, false, 'докачка не должна удалять файл');
        assert.ok(post.url.indexOf('/proxy/api/image/models/qwen-edit-2511/files') !== -1,
            'неверный путь воркера: ' + post.url);
        // Прогресс опрашивается существующей ручкой раз в POLL_MS (1500 мс).
        await sleep(1600);
        assert.ok(requests.some((r) => r.url.indexOf('/hf/progress?bundleId=modelfile-') !== -1),
            'прогресс не опрашивается существующей ручкой');
    });

    await check('4. завершение загрузки: тост, перечитывание списка моделей и статус', async function () {
        progressAnswer = { status: 'completed', progressPct: 100 };
        freshModels = [Object.assign({}, MODEL, {
            files: MODEL.files.map(function (f) {
                return f.filename === 'qwen_image_vae.safetensors'
                    ? Object.assign({}, f, { present: true, diskSizeBytes: 96 * 1024 }) : f;
            })
        })];
        await sleep(1800); // один тик опроса прогресса (POLL_MS = 1500)
        assert.ok(refreshCalls >= 1, 'список моделей не перечитан после загрузки');
        assert.ok(toasts.some((o) => o.message.indexOf('Готово') !== -1),
            'нет уведомления о завершении: ' + JSON.stringify(toasts));
        assert.ok(panelHtml().indexOf('на узле') !== -1, 'панель не перерисована после загрузки');
        Mod._state.busy = false;
    });

    await check('5. «Перекачать»: confirm и replace=true', async function () {
        requests.length = 0;
        global.confirm = function () { return true; };
        clickAction('redownload', 'qwen-edit-2511-Q4_K_M.gguf',
            { 'data-imf-role': 'diffusion', 'data-imf-repo': 'acme/qwen' });
        await sleep(30);
        const post = requests.filter((r) => r.method === 'POST')[0];
        assert.ok(post, 'POST не отправлен');
        assert.strictEqual(JSON.parse(post.body).replace, true, 'перекачка обязана слать replace=true');
        Mod._state.busy = false;
        Mod._actions.close();
    });

    await check('6. «Удалить с узла»: DELETE с filename в query', async function () {
        requests.length = 0;
        toasts.length = 0;
        Mod.render(ctx);
        global.confirm = function () { return true; };
        clickAction('delete', 'qwen_image_vae.safetensors');
        await sleep(40);
        const del = requests.filter((r) => r.method === 'DELETE')[0];
        assert.ok(del, 'DELETE не отправлен');
        assert.ok(del.url.indexOf('/files?filename=qwen_image_vae.safetensors') !== -1,
            'filename не в query: ' + del.url);
        assert.ok(toasts.some((o) => o.message.indexOf('Удалено') !== -1), 'нет уведомления об удалении');
    });

    await check('7. «Докачать недостающее»: в очередь только отсутствующие с источником', async function () {
        Mod._state.busy = false;
        Mod._state.queue = [];
        Mod.render(ctx);
        // Имитируем активную операцию: очередь только собирается, но не стартует.
        Mod._state.busy = true;
        Mod._actions.startMissingQueue();
        // У VAE есть repo и файла нет → в очередь; у LLM repo нет → не берём.
        assert.strictEqual(Mod._state.queue.length, 1,
            'в очереди должен быть 1 файл (VAE); got ' + Mod._state.queue.length);
        assert.strictEqual(Mod._state.queue[0].filename, 'qwen_image_vae.safetensors');
        Mod._state.busy = false;
        Mod._state.queue = [];
        Mod._actions.close();
    });

    // --- 3. статика страницы и i18n ----------------------------------------
    await check('8. страница: кнопка «Файлы» и сохранение files/compat', function () {
        const pageSrc = fs.readFileSync(path.join(__dirname, '..', 'js', 'modules', 'image-page.js'), 'utf8');
        assert.ok(pageSrc.indexOf('data-img-action="model-files"') !== -1, 'в таблице нет кнопки «Файлы»');
        assert.ok(pageSrc.indexOf('state.openFilesModel') !== -1, 'нет состояния раскрытой панели');
        assert.ok(pageSrc.indexOf('window.ImageModelFiles.render') !== -1, 'страница не рисует панель');
        // normalizeModels обязан сохранять files и compat (R751): на них работают
        // и вердикт в строке модели, и кнопки R710, и панель файлов.
        const idx = pageSrc.indexOf('function normalizeModels');
        const end = pageSrc.indexOf('function normalizeBackends', idx);
        const body = pageSrc.slice(idx, end);
        assert.ok(/files:\s*files\.map/.test(body), 'normalizeModels теряет files');
        assert.ok(/compat:\s*m\.compat/.test(body), 'normalizeModels теряет compat');
        assert.ok(/present:/.test(body), 'normalizeModels теряет present файла');
        const html = fs.readFileSync(path.join(__dirname, '..', 'index.html'), 'utf8');
        assert.ok(html.indexOf('js/modules/image-model-files.js') !== -1, 'модуль не подключён в index.html');
    });

    await check('9. CSS: панель и строки файлов оформлены токенами темы', function () {
        const css = fs.readFileSync(path.join(__dirname, '..', 'css', 'pages.css'), 'utf8');
        ['.imf-panel', '.imf-row', '.imf-state.ok', '.imf-actions', '.im-files-row'].forEach(function (sel) {
            assert.ok(css.indexOf(sel) !== -1, 'нет правила ' + sel);
        });
        assert.ok(/\.imf-row\s*\{[\s\S]*?grid-template-columns/.test(css), 'строки файлов не в сетке');
    });

    await check('10. i18n: все ключи панели есть в ru.js и en.js', function () {
        const ru = fs.readFileSync(path.join(__dirname, '..', 'js', 'i18n', 'ru.js'), 'utf8');
        const en = fs.readFileSync(path.join(__dirname, '..', 'js', 'i18n', 'en.js'), 'utf8');
        const mod = fs.readFileSync(path.join(__dirname, '..', 'js', 'modules', 'image-model-files.js'), 'utf8');
        const keys = new Set();
        const re = /imageModels\.files_[a-z_]+/g;
        let m;
        while ((m = re.exec(mod)) !== null) keys.add(m[0]);
        re.lastIndex = 0;
        while ((m = re.exec(fs.readFileSync(path.join(__dirname, '..', 'js', 'modules', 'image-page.js'), 'utf8'))) !== null) {
            keys.add(m[0]);
        }
        assert.ok(keys.size >= 20, 'в сценарии меньше 20 ключей панели: ' + keys.size);
        keys.forEach(function (k) {
            assert.ok(ru.indexOf('"' + k + '":') !== -1, 'нет ru-перевода ' + k);
            assert.ok(en.indexOf('"' + k + '":') !== -1, 'нет en-перевода ' + k);
        });
    });

    Mod._actions.close();
    console.log('\n' + (failures ? 'FAILED: ' + failures : 'OK: ' + passed + ' checks passed'));
    process.exit(failures ? 1 : 0);
})().catch((e) => {
    console.error('FATAL', e && e.stack || e);
    process.exit(1);
});

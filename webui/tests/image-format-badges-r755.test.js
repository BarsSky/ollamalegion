// image-format-badges-r755.test.js — R755 (2026-11-25): бейджи форматов файлов
// «что движок читает» и предупреждение об MLX-квантах.
//
// ЖИВОЙ ДЕФЕКТ. В списке файлов HF оператор видел и .gguf, и .safetensors, но не
// знал, какой формат движок прочитает в выбранной роли: для Qwen-Image
// safetensors-диффузия не читается (нужен GGUF), а VAE/text encoder в
// safetensors — читаются; MLX-квант .safetensors падает с unsupported dtype U32
// уже после скачивания гигабайтов.
//
// ЧТО ПРОВЕРЯЕТСЯ:
//   1. resolve: точная роль приоритетнее правила «*»; MLX в имени сильнее правила;
//      неизвестное расширение — «нет данных», а не «читается»;
//   2. бейдж берёт подписи/подсказки из i18n и классы уровня (ok/needs/warn/bad);
//   3. строка «Форматы: …» и предупреждение MLX собираются из ответа воркера;
//   4. панель файлов группы (image-model-files.js) рисует бейджи и запрашивает
//      матрицу ОДИН раз на семейство;
//   5. страница HF использует те же бейджи/сводку/предупреждение (проверка
//      исходника — DOM-сценарий HF уже покрыт R696/R749/R751);
//   6. CSS-классы и порядок загрузки скриптов; все ключи i18n есть в ru/en.
//
// Запуск: node webui/tests/image-format-badges-r755.test.js
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

// --- DOM / i18n -------------------------------------------------------------

function makeNode(tag) {
    const node = {
        tagName: String(tag || 'div').toUpperCase(),
        style: {},
        textContent: '',
        _html: '',
        onclick: null,
        parentNode: null,
        getAttribute: function () { return null; },
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
const toasts = [];
global.Toast = { show: function (o) { toasts.push(o); } };
global.ImageModelsHf = { pure: { roleLabel: function (role) { return 'ROLE:' + role; } } };

// Реальные пакеты: проверяем не самодельный словарь, а то, что увидит оператор.
require(path.join(__dirname, '..', 'js', 'i18n', 'ru.js'));
const RU = global.I18N_RU;
assert.ok(RU, 'пакет ru не загрузился');
global.I18N = {
    t: function (k, vars) {
        let s = Object.prototype.hasOwnProperty.call(RU, k) ? RU[k] : k;
        if (vars) s = String(s).replace(/\{(\w+)\}/g, function (m, p) { return p in vars ? vars[p] : m; });
        return s;
    },
};

const requests = [];
const MATRIX = {
    engine: 'stable-diffusion.cpp',
    family: 'qwen_image',
    familyKnown: true,
    dit: true,
    allInOne: false,
    rules: [
        { format: 'gguf', role: '*', verdict: 'reads' },
        { format: 'sft', role: '*', verdict: 'reads' },
        { format: 'safetensors', role: 'vae', verdict: 'reads' },
        { format: 'safetensors', role: 'llm', verdict: 'reads' },
        { format: 'safetensors', role: 'diffusion', verdict: 'needs_gguf' },
        { format: 'ckpt', role: 'diffusion', verdict: 'needs_gguf' },
    ],
    nameWarnings: [{ contains: 'mlx', verdict: 'unsupported', code: 'mlx_quantized' }],
};
global.fetch = function (url, init) {
    const rec = { url: String(url), method: (init && init.method) || 'GET', body: init && init.body };
    requests.push(rec);
    let body = {};
    if (rec.url.indexOf('/formats') !== -1) body = MATRIX;
    return Promise.resolve({
        ok: true, status: 200,
        text: function () { return Promise.resolve(JSON.stringify(body)); },
    });
};

require(path.join(__dirname, '..', 'js', 'modules', 'image-format-badges.js'));
require(path.join(__dirname, '..', 'js', 'modules', 'image-model-files.js'));
const Fmt = global.ImageFormatBadges;
const Panel = global.ImageModelFiles;
assert.ok(Fmt && Panel, 'модули бейджей/панели должны экспортироваться');

(async function main() {
    // --- 1. resolve ---------------------------------------------------------
    await check('1. resolve: точная роль > «*», MLX-имя > правило, неизвестное расширение = null', function () {
        assert.strictEqual(Fmt.resolve(MATRIX, 'model.gguf', 'diffusion').verdict, 'reads');
        assert.strictEqual(Fmt.resolve(MATRIX, 'vae/ae.safetensors', 'vae').verdict, 'reads');
        assert.strictEqual(Fmt.resolve(MATRIX, 'model.safetensors', 'diffusion').verdict, 'needs_gguf');
        const mlx = Fmt.resolve(MATRIX, 'qwen-MLX-4bit.safetensors', 'vae');
        assert.strictEqual(mlx.verdict, 'unsupported', 'MLX-имя обязано быть сильнее правила роли');
        assert.strictEqual(mlx.byName, true);
        assert.strictEqual(Fmt.resolve(MATRIX, 'model.bin', 'diffusion'), null);
        assert.strictEqual(Fmt.resolve(null, 'model.gguf', 'diffusion'), null);
    });

    // --- 2. бейдж -----------------------------------------------------------
    await check('2. бейдж: подпись из i18n, класс уровня и подсказка', function () {
        const reads = Fmt.badgeHtml(MATRIX, 'vae/ae.safetensors', 'vae');
        assert.ok(reads.indexOf('ifb-ok') !== -1, 'VAE обязана быть зелёной: ' + reads);
        assert.ok(reads.indexOf(RU['imageModels.format_chip_reads']) !== -1, 'нет подписи «читается»: ' + reads);
        assert.ok(reads.indexOf('safetensors') !== -1, 'нет расширения в бейдже');

        const gguf = Fmt.badgeHtml(MATRIX, 'model.safetensors', 'diffusion');
        assert.ok(gguf.indexOf('ifb-needs') !== -1, 'safetensors-диффузия обязана быть «нужен GGUF»: ' + gguf);
        assert.ok(gguf.indexOf(RU['imageModels.format_chip_needs_gguf']) !== -1);
        assert.ok(gguf.indexOf(RU['imageModels.format_hint_needs_gguf'].slice(0, 20)) !== -1,
            'в title нет подсказки из i18n');

        const mlx = Fmt.badgeHtml(MATRIX, 'model-MLX-4bit.safetensors', 'diffusion');
        assert.ok(mlx.indexOf('ifb-bad') !== -1, 'MLX-квант обязан быть красным: ' + mlx);
        assert.ok(mlx.indexOf(RU['imageModels.format_chip_mlx']) !== -1);
    });

    await check('3. summaryText и mlxWarningHtml собираются из матрицы', function () {
        const text = Fmt.summaryText(MATRIX);
        assert.ok(text.indexOf('GGUF') !== -1, 'нет GGUF в сводке: ' + text);
        assert.ok(text.indexOf(RU['imageModels.format_summary_safetensors_needs_gguf']) !== -1,
            'нет «safetensors diffusion — нужен GGUF»: ' + text);
        assert.ok(text.indexOf(RU['imageModels.format_summary_components']) !== -1,
            'нет строки про VAE/энкодеры: ' + text);
        const warn = Fmt.mlxWarningHtml(MATRIX, ['model.gguf', 'qwen-MLX-4bit.safetensors']);
        assert.ok(warn.indexOf('ifb-warning') !== -1, 'нет предупреждения: ' + warn);
        assert.ok(warn.indexOf('MLX') !== -1);
        assert.strictEqual(Fmt.mlxWarningHtml(MATRIX, ['model.gguf']), '', 'ложное предупреждение');
    });

    // --- 3. панель файлов группы -------------------------------------------
    await check('4. панель файлов: бейджи в строках, один запрос матрицы на семейство', async function () {
        requests.length = 0;
        const model = {
            name: 'qwen-edit-2511', family: 'qwen_image', size_bytes: 100,
            bundle_path: '/models/qwen-edit-2511',
            compat: { verdict: 'incomplete', runnable: false, missing: ['VAE'], reason: 'нет VAE' },
            files: [
                { role: 'diffusion', repo: 'acme/q', filename: 'qwen-MLX-4bit.safetensors', present: false, sizeBytes: 100 },
                { role: 'diffusion', repo: 'acme/q', filename: 'qwen-plain.safetensors', present: false, sizeBytes: 100 },
                { role: 'vae', repo: 'acme/q', filename: 'ae.safetensors', present: true, diskSizeBytes: 100 },
                { role: 'diffusion', repo: 'acme/q', filename: 'qwen-Q4_K_M.gguf', present: true, diskSizeBytes: 100 },
            ],
        };
        const ok = Panel.render({ backendId: 'imageworker', model: model, hostId: 'imModelFilesHost' });
        assert.strictEqual(ok, true, 'render вернул false');
        await sleep(40);
        const html = getEl('imModelFilesHost').innerHTML;
        assert.ok(html.indexOf('ifb-badge') !== -1, 'в панели нет бейджей форматов: ' + html.slice(0, 400));
        assert.ok(html.indexOf('ifb-bad') !== -1, 'MLX-файл не помечен красным');
        assert.ok(html.indexOf('ifb-needs') !== -1, 'safetensors-диффузия не помечена «нужен GGUF»');
        assert.ok(html.indexOf('ifb-ok') !== -1, 'VAE/GGUF не помечены «читается»');
        assert.ok(html.indexOf('ifb-warning') !== -1, 'нет предупреждения MLX в панели');
        assert.ok(html.indexOf(RU['imageModels.format_summary_safetensors_needs_gguf']) !== -1,
            'нет строки форматов в панели');
        const formatsCalls = requests.filter((r) => r.url.indexOf('/formats') !== -1);
        assert.strictEqual(formatsCalls.length, 1, 'матрица запрошена не один раз: ' + formatsCalls.length);
        // Повторный рендер (refresh таблицы) не должен дёргать воркер снова.
        Panel.render({ backendId: 'imageworker', model: model, hostId: 'imModelFilesHost' });
        await sleep(20);
        assert.strictEqual(requests.filter((r) => r.url.indexOf('/formats') !== -1).length, 1,
            'повторный рендер повторно запросил матрицу');
        Panel._actions.close();
    });

    // --- 4. страница HF ------------------------------------------------------
    await check('5. страница HF использует общие бейджи, сводку и MLX-предупреждение', function () {
        const src = fs.readFileSync(path.join(__dirname, '..', 'js', 'modules', 'image-models-hf.js'), 'utf8');
        ["backendUrl('formats?family=", 'window.ImageFormatBadges', 'formatSummaryHtml', 'formatMlxWarningHtml',
            'ensureFormatMatrix']
            .forEach(function (tok) {
                assert.ok(src.indexOf(tok) !== -1, 'в image-models-hf.js нет ' + tok);
            });
        assert.ok(/function formatBadgeHtml/.test(src), 'нет функции бейджа');
        // Бейдж вставляется в строку файла рядом с probe-бейджем.
        assert.ok(src.indexOf('formatBadgeHtml(path, role)') !== -1, 'бейдж не рисуется в строке файла');
    });

    // --- 5. статика/CSS/i18n -------------------------------------------------
    await check('6. CSS: уровни бейджей и предупреждение', function () {
        const css = fs.readFileSync(path.join(__dirname, '..', 'css', 'pages.css'), 'utf8');
        ['.ifb-badge', '.ifb-badge.ifb-ok', '.ifb-badge.ifb-needs', '.ifb-badge.ifb-bad',
            '.ifb-summary', '.ifb-warning']
            .forEach(function (sel) {
                assert.ok(css.indexOf(sel) !== -1, 'нет правила ' + sel);
            });
    });

    await check('7. index.html: модуль бейджей загружен ДО страниц image', function () {
        const html = fs.readFileSync(path.join(__dirname, '..', 'index.html'), 'utf8');
        const badges = html.indexOf('js/modules/image-format-badges.js');
        const hf = html.indexOf('js/modules/image-models-hf.js');
        const files = html.indexOf('js/modules/image-model-files.js');
        assert.ok(badges !== -1, 'модуль бейджей не подключён');
        assert.ok(badges < hf && badges < files, 'бейджи обязаны грузиться раньше модулей, которые их зовут');
    });

    await check('8. i18n: все ключи бейджей есть в ru.js и en.js', function () {
        const ru = fs.readFileSync(path.join(__dirname, '..', 'js', 'i18n', 'ru.js'), 'utf8');
        const en = fs.readFileSync(path.join(__dirname, '..', 'js', 'i18n', 'en.js'), 'utf8');
        const badges = fs.readFileSync(path.join(__dirname, '..', 'js', 'modules', 'image-format-badges.js'), 'utf8');
        const keys = new Set();
        const re = /imageModels\.format_[a-z_]+/g;
        let m;
        while ((m = re.exec(badges)) !== null) keys.add(m[0]);
        assert.ok(keys.size >= 17, 'в сценарии меньше 17 ключей: ' + keys.size);
        keys.forEach(function (k) {
            assert.ok(ru.indexOf('"' + k + '":') !== -1, 'нет ru-перевода ' + k);
            assert.ok(en.indexOf('"' + k + '":') !== -1, 'нет en-перевода ' + k);
        });
    });

    Panel._actions.close();
    console.log('\n' + (failures ? 'FAILED: ' + failures : 'OK: ' + passed + ' checks passed'));
    process.exit(failures ? 1 : 0);
})().catch((e) => {
    console.error('FATAL', e && e.stack || e);
    process.exit(1);
});

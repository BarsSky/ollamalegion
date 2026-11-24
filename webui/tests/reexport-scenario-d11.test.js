// reexport-scenario-d11.test.js — D11 (2026-11-25): UI-сценарий модуля
// ре-экспорта GGUF (страница моделей, блок «Ре-экспорт GGUF»).
//
// ЧТО ИМЕННО ЗАКРЕПЛЯЕТСЯ И ПОЧЕМУ.
//
// 1. СЦЕНАРИЙ, А НЕ ОТДЕЛЬНЫЕ КНОПКИ. Оператору нужен один связный путь:
//    увидеть модель и вердикт схемы → проверить, что изменит нормализация →
//    нормализовать → пересканировать каталог → проверить, что файл грузится.
//    На HEAD fe6bafdc блока с шагами нет вовсе (там форма «выбери цель и создай
//    задачу» с полями mode/dry_run), поэтому этот набор на старом коммите
//    красный: шагов data-step="inspect|normalize|rescan|load" в разметке нет.
//
// 2. НАСТОЯЩИЕ ТЕКСТЫ. i18n грузится из боевых пакетов ru.js/en.js, а не из
//    заглушки: тест проверяет слова, которые видит оператор («Нормализовать
//    (loop-схема)», «проверка завершена», «уже в схеме загрузчика»), и
//    симметрию ru/en. Ключ, забытый в одном пакете, краснеет здесь же.
//
// 3. ПРОВОД, А НЕ ВНЕШНИЙ ВИД. Тела запросов (dry_run у проверки, fuse у
//    нормализации, DELETE при отмене, операция load у балансера) сверяются с
//    контрактом сервиса services/gguf-reexport: отдельного поля mode у него нет
//    (POST /api/reexport/jobs разбирается с DisallowUnknownFields), поэтому
//    «проверить» = dry_run той же цели, и это видно в теле POST.
//
// 4. ОТСУТСТВИЕ МОДУЛЯ — ЭТО НОРМА. capabilities не ответил → в хосте не должно
//    остаться ни блока, ни подписей: страница без профиля `reexport` обязана
//    выглядеть ровно как раньше.
//
// 5. ПРИЧИНА ОТКАЗА ДОХОДИТ ДО ОПЕРАТОРА. Ошибка задачи (model_not_patchable на
//    чужой архитектуре) показывается и кодом, и ТЕКСТОМ сервиса, а не «HTTP 500».
//
// Запуск: node webui/tests/reexport-scenario-d11.test.js
'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');

let checks = 0;
const failures = [];
function check(name, fn) {
    try {
        fn();
        checks++;
        console.log('  \u2713 ' + name);
    } catch (e) {
        failures.push(name + ': ' + (e && e.message));
        console.error('  \u2717 ' + name + ' — ' + (e && e.message));
    }
}
const asyncChecks = [];
function checkAsync(name, fn) { asyncChecks.push([name, fn]); }

// --- окружение -------------------------------------------------------------
// Минимальный DOM: узел на каждый селектор, устойчивые обработчики. Ровно то,
// что модуль спрашивает у страницы (querySelector/innerHTML/textContent/style/
// addEventListener), и ничего сверх — иначе тест начнёт проверять сам DOM-шим.
global.window = global;
global.Utils = {
    escapeHtml(s) {
        return String(s === null || s === undefined ? '' : s)
            .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
            .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
    }
};

// Боевые пакеты переводов: тест обязан проверять слова, а не ключи.
require('../js/i18n/ru.js');
require('../js/i18n/en.js');
const RU = global.window.I18N_RU;
const EN = global.window.I18N_EN;

function translate(pack, key, vars) {
    let s = pack[key];
    if (s === undefined || s === null) s = key;
    if (vars && typeof vars === 'object') {
        Object.keys(vars).forEach(function (p) {
            s = s.split('{' + p + '}').join(String(vars[p]));
        });
    }
    return s;
}
let lang = 'ru';
global.I18N = { t: (key, vars) => translate(lang === 'ru' ? RU : EN, key, vars) };

function makeEl(sel) {
    return {
        id: String(sel || '').replace('#', ''),
        innerHTML: '',
        textContent: '',
        value: '',
        checked: false,
        disabled: false,
        style: {},
        _listeners: {},
        _attrs: {},
        addEventListener(type, fn) { (this._listeners[type] = this._listeners[type] || []).push(fn); },
        removeEventListener() {},
        setAttribute(k, v) { this._attrs[k] = v; },
        getAttribute(k) { return Object.prototype.hasOwnProperty.call(this._attrs, k) ? this._attrs[k] : null; },
        querySelector(s) { return node(s); },
        querySelectorAll() { return []; },
        contains() { return false; },
        closest() { return null; }
    };
}
const nodes = {};
function node(sel) {
    const key = String(sel);
    if (!nodes[key]) nodes[key] = makeEl(key);
    return nodes[key];
}
const hostEl = makeEl('#ggufReexportHost');
hostEl.querySelector = function (sel) { return node(sel); };

global.document = {
    getElementById(id) { return id === 'ggufReexportHost' ? hostEl : null; },
    addEventListener() {},
    hidden: false
};

const calls = [];
let fetchImpl = null;
global.fetch = function (url, opts) {
    const call = { url: String(url), opts: opts || {} };
    calls.push(call);
    return fetchImpl(call);
};
function jsonResponse(status, body) {
    return Promise.resolve({
        ok: status >= 200 && status < 300,
        status,
        text() { return Promise.resolve(body === undefined ? '' : JSON.stringify(body)); }
    });
}

// Как на живой странице: слой задач грузится ПЕРЕД главным модулем
// (webui/index.html: gguf-reexport-jobs.js → gguf-reexport.js).
require('../js/modules/gguf-reexport-jobs.js');
require('../js/modules/gguf-reexport.js');
const R = global.window.GgufReexport;
const wait = (ms) => new Promise((r) => setTimeout(r, ms));

// --- фикстуры (значения — как у живого сервиса на стенде) ------------------
const CAPS = {
    service: 'gguf-reexport',
    available: true,
    version: { build_version: 'r261121-r709' },
    converter: { name: 'llama.cpp', revision: '5bacb7c868', python: '3.12.16', torch_state: 'absent' },
    architectures: [
        { id: 'inkling2', scheme: 'inkling2_loop', target: 'inkling2_loop', label: 'Inkling-2 (loop)', note: 'визиты' }
    ],
    targets: [
        { id: 'inkling2_loop', label: 'Inkling-2 loop (схема inkling2_loop)', kind: 'metadata_patch', available: true,
          options: [{ id: 'fuse_loop_queries', type: 'bool', default: true }] },
        { id: 'hf_to_gguf', label: 'HF-чекпойнт в GGUF', kind: 'convert', available: false, reason: 'образ собран без torch' }
    ],
    limits: { max_concurrent: 1, models_dir: '/app/models' }
};

const MODELS = {
    models: [
        // Ровно тот файл, ради которого модуль написан: 24 физических блока при
        // block_count=48 (loop-экспорт), матрицы запросов ещё не разложены.
        { path: 'out/inkling-orig.gguf', name: 'inkling-orig.gguf', size_bytes: 16610543328, arch: 'inkling2',
          block_count: 48, physical_blocks: 24, scheme: 'inkling2_loop_export', patchable: true,
          has_loop_keys: true, query_matrices: true, msg_query_tensors: 0 },
        // Уже пропатченный файл: схема загрузчика, нормализация не требуется.
        { path: 'out/inkling2-loop.gguf', name: 'inkling2-loop.gguf', size_bytes: 16610544736, arch: 'inkling2_loop',
          block_count: 24, physical_blocks: 24, scheme: 'inkling2_loop', patchable: false,
          has_loop_keys: true, query_matrices: false, msg_query_tensors: 12 },
        // Чужая архитектура: honest отказ model_not_patchable, а не «HTTP 422».
        { path: 'Qwen3-4B.gguf', name: 'Qwen3-4B.gguf', size_bytes: 2497281120, arch: 'qwen3',
          block_count: 36, physical_blocks: 36, scheme: 'plain', patchable: false,
          has_loop_keys: false, query_matrices: false, msg_query_tensors: 0 }
    ]
};

const INSPECT_JOB = {
    id: 'rj-1', status: 'succeeded', target: 'inkling2_loop', model_dir: 'out/inkling-orig.gguf',
    options: { dry_run: true, fuse_loop_queries: true },
    progress: { phase: 'copy', percent: 100 },
    result: {
        duration_ms: 576, src_bytes: 16610543328, dst_bytes: 0,
        facts: { arch: 'inkling2', block_count: 48, physical_blocks: 24, visits: 48, core: [6, 18],
                 passes: 3, gate_rank: 64, tensors: 734, msg_query: 12 },
        changes: ['general.architecture: inkling2 -> inkling2_loop',
                  'inkling2.block_count: 48 -> 24 (физические слои)'],
        verify: { dry_run: true }
    }
};

const NORMALIZE_JOB_OK = {
    id: 'rj-2', status: 'succeeded', target: 'inkling2_loop', model_dir: 'out/inkling-orig.gguf',
    out_path: '/app/models/out/inkling-orig.inkling2_loop.gguf',
    progress: { phase: 'done', percent: 100 },
    result: {
        duration_ms: 1377344, src_bytes: 16610543328, dst_bytes: 16610544736,
        facts: { arch: 'inkling2', block_count: 48, physical_blocks: 24 },
        verify: { arch: 'inkling2_loop', block_count: 24, visits: 48, tensors: 744 }
    }
};

const NORMALIZE_JOB_FAIL = {
    id: 'rj-3', status: 'failed', target: 'inkling2_loop', model_dir: 'Qwen3-4B.gguf',
    // runner.go переводит код возврата драйвера в общую формулировку, а конкретная
    // причина остаётся последней строкой лога задачи (живой ответ сервиса).
    error: { code: 'model_not_patchable', message: 'файл не подходит под патч: make_plan не нашёл loop-схему (см. хвост лога)' },
    log_tail: ['старт: цель=inkling2_loop источник=/app/models/Qwen3-4B.gguf',
               "ошибка: ожидался general.architecture=inkling2, получено 'qwen3'"],
    progress: { phase: 'plan', percent: 0 }
};

function standardFetch(overrides) {
    const o = overrides || {};
    return (call) => {
        if (call.url.indexOf('/capabilities') >= 0) return o.caps ? o.caps(call) : jsonResponse(200, CAPS);
        if (call.url.indexOf('/models') >= 0 && call.opts.method !== 'POST') {
            return o.models ? o.models(call) : jsonResponse(200, MODELS);
        }
        if (call.url.indexOf('/jobs') >= 0) {
            if (call.opts.method === 'POST') return o.create ? o.create(call) : jsonResponse(202, INSPECT_JOB);
            if (o.job) return o.job(call);
            return jsonResponse(200, { jobs: [] });
        }
        return jsonResponse(404, {});
    };
}

/** mountWith — mount() на заданном фетче + ожидание загрузки списка моделей. */
async function mountWith(fetchFn) {
    fetchImpl = fetchFn;
    const ok = await R.mount();
    await wait(30);
    return ok;
}

// --- синхронные проверки ---------------------------------------------------
checkAsync('без capabilities модуля нет: в хосте не остаётся ни блока, ни подписей', async () => {
    hostEl.innerHTML = 'x';
    fetchImpl = () => jsonResponse(424, { error: { code: 'module_unavailable', message: 'нет модуля' } });
    const ok = await R.mount();
    assert.strictEqual(ok, false, 'mount() обязан вернуть false без модуля');
    assert.strictEqual(hostEl.innerHTML, '', 'в разметке не должно остаться ни блока, ни подписей');
    assert.strictEqual(R._state.available, false);
});

check('вердикт по модели: patchable с числами и объяснением (и без выдумок)', function () {
    const patchable = R._verdictFor(MODELS.models[0]);
    assert.strictEqual(patchable.kind, 'patchable');
    assert.ok(patchable.text.indexOf('patchable') >= 0, 'в вердикте нет слова patchable: ' + patchable.text);
    assert.ok(patchable.text.indexOf('24') >= 0 && patchable.text.indexOf('48') >= 0,
        'в вердикте нет чисел 24/48: ' + patchable.text);
    const normalized = R._verdictFor(MODELS.models[1]);
    assert.strictEqual(normalized.kind, 'normalized');
    assert.ok(normalized.text.indexOf('нормализация не нужна') >= 0, normalized.text);
    const plain = R._verdictFor(MODELS.models[2]);
    assert.strictEqual(plain.kind, 'plain');
    assert.ok(plain.text.indexOf('qwen3') >= 0, 'в вердикте нет архитектуры: ' + plain.text);
    const broken = R._verdictFor({ path: 'x.gguf', scheme: 'unreadable', read_error: 'нет сигнатуры GGUF' });
    assert.strictEqual(broken.kind, 'unreadable');
    assert.ok(broken.text.indexOf('нет сигнатуры GGUF') >= 0, broken.text);
});

check('имя модели для воркера: basename без .gguf (иначе статус загрузки не найдётся)', function () {
    assert.strictEqual(R._modelHandle({ name: 'inkling2-loop.gguf' }), 'inkling2-loop');
    assert.strictEqual(R._modelHandle({ path: 'out/sub/Model.Q4_K_M.gguf' }), 'Model.Q4_K_M');
    assert.strictEqual(R._modelHandle({ name: 'already-loaded' }), 'already-loaded');
});

check('предупреждение о размере: RAM важнее VRAM, обе ветки различаются текстом', function () {
    const big = 16610543328; // 15.5 GiB
    const warnVram = R._loadWarning(big, { ramTotalMb: 25044, vramTotalMb: 8192 });
    assert.ok(warnVram && warnVram.kind === 'vram', 'модель больше VRAM обязана дать предупреждение vram');
    const warnRam = R._loadWarning(big, { ramTotalMb: 4096, vramTotalMb: 8192 });
    assert.ok(warnRam && warnRam.kind === 'ram', 'модель больше RAM обязана дать предупреждение ram');
    assert.strictEqual(R._loadWarning(1024 * 1024, { ramTotalMb: 25044, vramTotalMb: 8192 }), null,
        'маленькая модель не должна давать предупреждений');
    assert.strictEqual(R._loadWarning(big, null), null, 'без данных узла предупреждение не выдумывается');
});

check('стадии из API переведены: copy не показывается сырым словом', function () {
    assert.strictEqual(R._phaseLabel('copy'), 'копирование данных');
    assert.strictEqual(R._phaseLabel('verify'), 'сверка результата');
    assert.strictEqual(R._phaseLabel('weird'), 'weird', 'неизвестная стадия показывается как есть, а не теряется');
});

check('ошибка задачи: код, формулировка сервиса и конкретная причина из лога', function () {
    const text = R._jobErrorText(NORMALIZE_JOB_FAIL.error);
    assert.ok(text.indexOf('model_not_patchable') >= 0, 'нет кода: ' + text);
    assert.ok(text.indexOf('make_plan') >= 0, 'формулировка сервиса потеряна: ' + text);
    assert.ok(text.indexOf('reexport.err_model_not_patchable') < 0, 'в UI попал сырой ключ словаря: ' + text);
    const withLog = R._jobErrorText(NORMALIZE_JOB_FAIL.error, NORMALIZE_JOB_FAIL);
    assert.ok(withLog.indexOf("получено 'qwen3'") >= 0,
        'конкретная причина из лога задачи потеряна: ' + withLog);
    assert.ok(withLog.indexOf('из лога задачи') >= 0, 'лог не помечен как источник: ' + withLog);
});

check('i18n: все ключи блока есть в ru и en, значения непустые', function () {
    const ruKeys = Object.keys(RU).filter((k) => k.indexOf('reexport.') === 0).sort();
    const enKeys = Object.keys(EN).filter((k) => k.indexOf('reexport.') === 0).sort();
    assert.ok(ruKeys.length >= 100, 'подозрительно мало ключей reexport в ru: ' + ruKeys.length);
    assert.deepStrictEqual(ruKeys, enKeys, 'наборы ключей ru и en разошлись');
    const bad = ruKeys.filter((k) => !RU[k] || RU[k] === k || !EN[k] || EN[k] === k);
    assert.deepStrictEqual(bad, [], 'пустые/непереведённые ключи: ' + bad.join(', '));
    // Кнопки называются действиями и в обеих локалях, без эмодзи.
    assert.strictEqual(RU['reexport.btn_inspect'], 'Проверить');
    assert.strictEqual(RU['reexport.btn_normalize'], 'Нормализовать (loop-схема)');
    assert.strictEqual(RU['reexport.btn_load'], 'Проверить загрузку');
    assert.strictEqual(EN['reexport.btn_normalize'], 'Normalize (loop schema)');
    const emoji = /[\u{1F300}-\u{1FAFF}\u{2600}-\u{27BF}]/u;
    const withEmoji = ruKeys.filter((k) => emoji.test(RU[k]) || emoji.test(EN[k]));
    assert.deepStrictEqual(withEmoji, [], 'в подписях появились эмодзи: ' + withEmoji.join(', '));
});

// --- асинхронные проверки (сценарий) ---------------------------------------
checkAsync('модуль ответил → отрисованы все шаги сценария и кнопки-действия', async () => {
    const ok = await mountWith(standardFetch());
    assert.strictEqual(ok, true);
    const html = hostEl.innerHTML;
    ['model', 'inspect', 'normalize', 'rescan', 'load'].forEach(function (step) {
        assert.ok(html.indexOf('data-step="' + step + '"') >= 0, 'нет шага data-step="' + step + '"');
    });
    assert.ok(html.indexOf('id="rxInspect"') >= 0, 'нет кнопки «Проверить»');
    assert.ok(html.indexOf('id="rxNormalize"') >= 0, 'нет кнопки «Нормализовать»');
    assert.ok(html.indexOf('id="rxRescan"') >= 0, 'нет кнопки «Пересканировать модели»');
    assert.ok(html.indexOf('id="rxCheckLoad"') >= 0, 'нет кнопки «Проверить загрузку»');
    assert.ok(html.indexOf('Проверить') >= 0, 'подпись кнопки проверки не из словаря');
    assert.ok(html.indexOf('Нормализовать (loop-схема)') >= 0, 'подпись нормализации не из словаря');
    // Шаги идут в порядке сценария.
    const order = ['data-step="model"', 'data-step="inspect"', 'data-step="normalize"',
        'data-step="rescan"', 'data-step="load"'].map((m) => html.indexOf(m));
    const sorted = order.slice().sort((a, b) => a - b);
    assert.deepStrictEqual(order, sorted, 'шаги идут не в порядке сценария: ' + order.join(','));
    R._stopPolling();
});

checkAsync('список моделей: строка на модель, вердикт patchable с объяснением', async () => {
    await mountWith(standardFetch());
    const html = node('#rxModelsBody').innerHTML;
    assert.ok(html.indexOf('inkling-orig.gguf') >= 0, 'нет модели с loop-экспортом');
    assert.ok(html.indexOf('inkling2-loop.gguf') >= 0, 'нет уже нормализованной модели');
    assert.ok(html.indexOf('Qwen3-4B.gguf') >= 0, 'нет модели чужой архитектуры');
    assert.ok(html.indexOf('patchable') >= 0, 'в списке нет вердикта patchable: ' + html);
    assert.ok(html.indexOf('физических блоков 24 при block_count 48') >= 0,
        'в вердикте нет объяснения «24 при 48»: ' + html);
    assert.ok(html.indexOf('ре-экспорт не нужен') >= 0, 'нет вердикта для обычного GGUF');
    assert.ok(html.indexOf('уже в схеме') >= 0, 'нет вердикта для уже нормализованной модели');
    R._stopPolling();
});

checkAsync('шаг «Проверить»: задача с dry_run, показ фактов и плана изменений', async () => {
    calls.length = 0;
    await mountWith(standardFetch({ create: () => jsonResponse(202, INSPECT_JOB), job: () => jsonResponse(200, INSPECT_JOB) }));
    R._state.selectedModel = 'out/inkling-orig.gguf';
    const click = (node('#rxInspect')._listeners.click || [])[0];
    assert.ok(typeof click === 'function', 'на кнопке «Проверить» нет обработчика');
    click({});
    await wait(60);

    const post = calls.filter((c) => c.url.indexOf('/api/reexport/jobs') >= 0 && c.opts.method === 'POST')[0];
    assert.ok(post, 'POST задачи проверки не отправлен');
    const body = JSON.parse(post.opts.body);
    assert.strictEqual(body.model_dir, 'out/inkling-orig.gguf');
    assert.strictEqual(body.target, 'inkling2_loop');
    assert.strictEqual(body.options.dry_run, true, 'проверка обязана быть dry_run: файл не пишется');
    assert.strictEqual(body.mode, undefined, 'поля mode у сервиса нет: POST разбирается с DisallowUnknownFields');

    const status = node('#rxInspectStatus').textContent;
    assert.ok(status.indexOf('проверка завершена') >= 0, 'статус проверки не показан: ' + status);
    const result = node('#rxInspectResult').innerHTML;
    assert.ok(result.indexOf('архитектура') >= 0, 'нет факта архитектуры: ' + result);
    assert.ok(result.indexOf('визитов') >= 0, 'нет числа визитов: ' + result);
    assert.ok(result.indexOf('loop-ключи') >= 0, 'нет наличия loop-ключей: ' + result);
    assert.ok(result.indexOf('матрицы запросов') >= 0, 'нет наличия матриц запросов: ' + result);
    assert.ok(result.indexOf('Что будет сделано при нормализации') >= 0, 'нет плана изменений: ' + result);
    assert.ok(result.indexOf('inkling2_loop') >= 0, 'план не называет целевую схему: ' + result);
    assert.ok(result.indexOf('пробный прогон, файл не писался') >= 0, 'не сказано, что файл не писался: ' + result);
    R._stopPolling();
});

checkAsync('шаг «Нормализовать»: прогресс по стадиям, отмена и итог задачи', async () => {
    calls.length = 0;
    await mountWith(standardFetch({ create: () => jsonResponse(202, NORMALIZE_JOB_OK), job: () => jsonResponse(200, NORMALIZE_JOB_OK) }));
    R._state.selectedModel = 'out/inkling-orig.gguf';
    node('#rxFuse').checked = true;
    // Прогресс рисуется из стадий API: проверяем на живой форме задачи.
    R._state.normalize = {
        status: 'running', jobId: 'rj-2', error: '', message: '',
        job: { id: 'rj-2', status: 'running', progress: { phase: 'copy', percent: 42.5, bytes_done: 1024, bytes_total: 4096 } }
    };
    R._renderNormalize(hostEl);
    const progress = node('#rxNormalizeProgress').innerHTML;
    assert.ok(progress.indexOf('42.5%') >= 0, 'нет процента прогресса: ' + progress);
    assert.ok(progress.indexOf('копирование данных') >= 0, 'стадия API не показана словами: ' + progress);
    assert.notStrictEqual(node('#rxCancelJob').style.display, 'none', 'у активной задачи обязана быть отмена');

    const click = (node('#rxNormalize')._listeners.click || [])[0];
    assert.ok(typeof click === 'function', 'на кнопке «Нормализовать» нет обработчика');
    click({});
    await wait(60);
    const post = calls.filter((c) => c.url.indexOf('/api/reexport/jobs') >= 0 && c.opts.method === 'POST')[0];
    assert.ok(post, 'POST нормализации не отправлен');
    const body = JSON.parse(post.opts.body);
    assert.strictEqual(body.options.dry_run, undefined, 'нормализация не должна быть dry_run');
    assert.strictEqual(body.options.fuse_loop_queries, true, 'fuse включён по умолчанию: без него загрузчик упадёт');

    const status = node('#rxNormalizeStatus').textContent;
    assert.ok(status.indexOf('нормализация завершена') >= 0, 'статус не показан: ' + status);
    const result = node('#rxNormalizeResult').innerHTML;
    assert.ok(result.indexOf('/app/models/out/inkling-orig.inkling2_loop.gguf') >= 0, 'нет пути результата: ' + result);
    assert.ok(result.indexOf('15.5 GiB') >= 0, 'нет размера результата: ' + result);
    assert.ok(result.indexOf('inkling2_loop') >= 0, 'нет архитектуры результата: ' + result);
    assert.ok(result.indexOf('block_count') >= 0, 'нет block_count результата: ' + result);
    R._stopPolling();
});

checkAsync('пустой список задач НЕ гасит ожидание активного шага (таймеры раздельные)', async () => {
    // ЖИВОЙ ДЕФЕКТ (поймано прогоном в браузере). Таблица задач перестаёт
    // поллиться, как только активных задач нет, — то есть сразу после завершения
    // задачи. Если этот стоп гасит ВСЕ таймеры модуля, ожидание статуса шага
    // умирает за миг до следующего опроса, и шаг навсегда остаётся «проверка
    // выполняется», хотя сервис уже отдал succeeded и итог пришёл в том же ответе.
    R._stopPolling();
    let fired = 0;
    R._state.jobs = []; // активных задач нет
    R._state.inspectTimer = setTimeout(() => { fired++; R._state.inspectTimer = null; }, 40);
    const timerBefore = R._state.inspectTimer;
    R._schedulePolling(); // обновление таблицы задач с пустым списком
    assert.strictEqual(R._state.inspectTimer, timerBefore,
        'поллинг списка задач погасил таймер ожидания шага');
    await wait(80);
    assert.strictEqual(fired, 1, 'ожидание шага не дожило до своего опроса');
    assert.strictEqual(R._state.inspectTimer, null, 'таймер не обнулился после срабатывания');
    R._stopPolling();
});

checkAsync('шаг «Нормализовать» для уже пропатченного файла: сразу «уже нормализовано», без задачи', async () => {
    calls.length = 0;
    await mountWith(standardFetch());
    R._state.selectedModel = 'out/inkling2-loop.gguf';
    const click = (node('#rxNormalize')._listeners.click || [])[0];
    click({});
    await wait(40);
    assert.strictEqual(calls.filter((c) => c.opts.method === 'POST').length, 0,
        'для уже нормализованного файла задача не создаётся (иначе это минуты копирования впустую)');
    const status = node('#rxNormalizeStatus').textContent;
    assert.ok(status.indexOf('уже в схеме загрузчика') >= 0, 'ответ «уже нормализовано» не показан: ' + status);
    R._stopPolling();
});

checkAsync('отказ задачи: model_not_patchable с текстом сервиса в UI', async () => {
    await mountWith(standardFetch({
        create: () => jsonResponse(202, { ...NORMALIZE_JOB_FAIL, status: 'queued', error: null }),
        job: () => jsonResponse(200, NORMALIZE_JOB_FAIL)
    }));
    R._state.selectedModel = 'Qwen3-4B.gguf';
    const click = (node('#rxNormalize')._listeners.click || [])[0];
    click({});
    await wait(60);
    const status = node('#rxNormalizeStatus').textContent;
    const result = node('#rxNormalizeResult').innerHTML;
    assert.ok(status.indexOf('нормализация не выполнена') >= 0, 'нет статуса отказа: ' + status);
    assert.ok(result.indexOf('model_not_patchable') >= 0, 'нет кода отказа: ' + result);
    assert.ok(result.indexOf('другая архитектура') >= 0, 'нет расшифровки кода для оператора: ' + result);
    assert.ok(result.indexOf('make_plan') >= 0, 'нет формулировки сервиса: ' + result);
    assert.ok(result.indexOf('получено') >= 0 && result.indexOf('qwen3') >= 0,
        'нет конкретной причины из лога задачи: ' + result);
    R._stopPolling();
});

checkAsync('шаг «Пересканировать модели»: зовёт существующий механизм и обновляет список', async () => {
    calls.length = 0;
    let rescanned = 0;
    global.GgufModule = {
        state: { selectedBackendId: 'cppworker-gpu' },
        rescanAllBackendModels() { rescanned++; return Promise.resolve({ ok: true }); }
    };
    await mountWith(standardFetch());
    const click = (node('#rxRescan')._listeners.click || [])[0];
    assert.ok(typeof click === 'function', 'на кнопке пересканирования нет обработчика');
    click({});
    await wait(60);
    assert.strictEqual(rescanned, 1, 'пересканирование воркеров не вызвано (правило должно быть одно)');
    const refresh = calls.filter((c) => c.url.indexOf('/api/reexport/models') >= 0 && c.url.indexOf('refresh=1') >= 0);
    assert.ok(refresh.length > 0, 'кэш фактов сервиса не сброшен (?refresh=1)');
    assert.ok(node('#rxRescanStatus').textContent.indexOf('модели пересканированы') >= 0,
        'итог пересканирования не показан: ' + node('#rxRescanStatus').textContent);
    R._stopPolling();
    delete global.GgufModule;
});

checkAsync('шаг «Проверить загрузку»: loading → loaded, offload_degraded и предупреждение о размере', async () => {
    calls.length = 0;
    const workerBefore = { models: [], total_ram_mb: 25044, total_vram_mb: 8192, available_ram_mb: 21623, available_vram_mb: 7097 };
    const workerAfter = {
        models: [{ name: 'inkling2-loop', state: 'loaded', offload_degraded: true,
                   offload_note: 'offloaded 6/49 layers to GPU', gpu_layers_actual: 6 }],
        total_ram_mb: 25044, total_vram_mb: 8192
    };
    let workerCalls = 0;
    let loads = 0;
    global.GgufApi = {
        getBackendId() { return 'cppworker-gpu-bundled-agent'; },
        requestViaBackend() { workerCalls++; return Promise.resolve(workerCalls === 1 ? workerBefore : workerAfter); },
        getLoadProgressViaBackend() {
            return Promise.resolve({ models: [{ name: 'inkling2-loop', state: 'loading' }] });
        },
        manageModel(id, op, name) { loads++; return Promise.resolve({ success: true, name: name }); }
    };
    await mountWith(standardFetch());
    R._state.selectedModel = 'out/inkling2-loop.gguf';
    const click = (node('#rxCheckLoad')._listeners.click || [])[0];
    assert.ok(typeof click === 'function', 'на кнопке проверки загрузки нет обработчика');
    click({});
    await wait(80);
    assert.strictEqual(loads, 1, 'загрузка через балансер не вызвана');
    // Предупреждение о размере: 15.5 GiB против VRAM узла 8192 МБ.
    const warn = node('#rxLoadWarn').innerHTML;
    assert.ok(warn.indexOf('больше VRAM узла') >= 0, 'нет предупреждения о размере относительно VRAM: ' + warn);
    assert.ok(warn.indexOf('offload_degraded') >= 0, 'предупреждение не называет последствие offload_degraded: ' + warn);
    assert.ok(warn.indexOf('RAM') >= 0, 'предупреждение не называет память узла: ' + warn);
    const status = node('#rxLoadStatus').textContent;
    assert.ok(status.indexOf('загружено') >= 0, 'итоговый статус загрузки не показан: ' + status);
    assert.ok(status.indexOf('cppworker-gpu-bundled-agent') >= 0, 'не назван узел загрузки: ' + status);
    const result = node('#rxLoadResult').innerHTML;
    assert.ok(result.indexOf('offload_degraded') >= 0, 'нет факта offload_degraded из /api/models: ' + result);
    assert.ok(result.indexOf('offloaded 6/49 layers to GPU') >= 0, 'нет пояснения узла: ' + result);
    R._stopPolling();
    delete global.GgufApi;
});

checkAsync('шаг «Проверить загрузку»: отказ узла показывается причиной, а не «неизвестной ошибкой»', async () => {
    const workerPayload = { models: [], total_ram_mb: 25044, total_vram_mb: 8192 };
    global.GgufApi = {
        getBackendId() { return 'cppworker-gpu-bundled-agent'; },
        requestViaBackend() { return Promise.resolve(workerPayload); },
        getLoadProgressViaBackend() { return jsonResponse(404, { error: 'model not found and not loading: inkling-orig' }); },
        manageModel() {
            return Promise.resolve({ success: false, status: 409,
                error: "load model failed: model_schema_unsupported: general.architecture=inkling2 requires inkling2_loop" });
        }
    };
    await mountWith(standardFetch());
    R._state.selectedModel = 'out/inkling-orig.gguf';
    const click = (node('#rxCheckLoad')._listeners.click || [])[0];
    click({});
    await wait(80);
    const status = node('#rxLoadStatus').textContent;
    assert.ok(status.indexOf('отказ') >= 0, 'нет статуса отказа: ' + status);
    const result = node('#rxLoadResult').innerHTML;
    assert.ok(result.indexOf('model_schema_unsupported') >= 0, 'причина отказа узла потеряна: ' + result);
    R._stopPolling();
    delete global.GgufApi;
});

checkAsync('шаг «Проверить загрузку» без выбранного узла: честный отказ, а не мнимая загрузка', async () => {
    global.GgufApi = { getBackendId() { return ''; } };
    await mountWith(standardFetch());
    R._state.selectedModel = 'out/inkling2-loop.gguf';
    const click = (node('#rxCheckLoad')._listeners.click || [])[0];
    click({});
    await wait(40);
    const status = node('#rxLoadStatus').textContent;
    const result = node('#rxLoadResult').innerHTML;
    assert.ok(status.indexOf('отказ') >= 0, 'нет отказа без узла: ' + status);
    assert.ok(result.indexOf('не выбран узел') >= 0, 'нет подсказки, что выбрать бэкенд: ' + result);
    R._stopPolling();
    delete global.GgufApi;
});

check('статическая проверка: index.html грузит слой задач ДО главного модуля', function () {
    // Порядок тегов — часть контракта: главный модуль берёт фабрику слоя задач
    // при первом обращении. Если файл подключён после него (или не подключён),
    // на живой странице это выглядит как «кнопки не работают» (класс R668/R669).
    const html = fs.readFileSync(path.join(__dirname, '..', 'index.html'), 'utf8');
    const jobsTag = html.indexOf('js/modules/gguf-reexport-jobs.js');
    const mainTag = html.indexOf('js/modules/gguf-reexport.js');
    const rendererTag = html.indexOf('js/modules/gguf-renderer.js');
    assert.ok(jobsTag > 0, 'в index.html нет тега js/modules/gguf-reexport-jobs.js');
    assert.ok(mainTag > 0, 'в index.html нет тега js/modules/gguf-reexport.js');
    assert.ok(jobsTag < mainTag, 'слой задач обязан грузиться ДО главного модуля: ' + jobsTag + ' vs ' + mainTag);
    assert.ok(mainTag < rendererTag, 'главный модуль обязан грузиться ДО gguf-renderer.js (он зовёт mount)');
});

checkAsync('слой задач не загружен: блок жив, шаги честно недоступны (без исключений)', async () => {
    // Страница могла приехать со старым набором скриптов. Падать из-за этого
    // нельзя: шаги модели/вердикта и проверки загрузки работают, а проверка и
    // нормализация говорят, чего не хватает, вместо ReferenceError.
    const saved = window.GgufReexportJobs;
    const warn = console.warn;
    const warnings = [];
    console.warn = function () { warnings.push(Array.prototype.join.call(arguments, ' ')); };
    try {
        delete window.GgufReexportJobs;
        R._resetJobsLayer();
        await mountWith(standardFetch());
        assert.strictEqual(warnings.length, 1, 'об отсутствии слоя обязано быть ровно одно предупреждение: ' + warnings.length);
        assert.ok(warnings[0].indexOf('gguf-reexport-jobs.js') >= 0, 'предупреждение не называет файл: ' + warnings[0]);
        assert.ok(hostEl.innerHTML.indexOf('data-step="model"') >= 0, 'блок обязан отрисоваться и без слоя задач');
        assert.ok(hostEl.innerHTML.indexOf('data-step="load"') >= 0, 'проверка загрузки не зависит от слоя задач');
        const tbody = node('#rxJobsBody').innerHTML;
        assert.ok(tbody.indexOf('Слой задач не загружен') >= 0, 'нет честного статуса в таблице задач: ' + tbody);
        assert.strictEqual(node('#rxInspect').disabled, true, 'кнопка проверки обязана быть выключена');
        assert.strictEqual(node('#rxNormalize').disabled, true, 'кнопка нормализации обязана быть выключена');
        calls.length = 0;
        const click = (node('#rxNormalize')._listeners.click || [])[0];
        click({});
        await wait(40);
        assert.strictEqual(calls.filter((c) => c.opts.method === 'POST').length, 0,
            'без слоя задач задача не создаётся');
        assert.ok(node('#rxError').textContent.indexOf('Слой задач не загружен') >= 0,
            'шаг обязан сказать причину: ' + node('#rxError').textContent);
    } finally {
        console.warn = warn;
        window.GgufReexportJobs = saved;
        R._resetJobsLayer();
    }
    await mountWith(standardFetch());
    assert.ok(node('#rxJobsBody').innerHTML.indexOf('Слой задач не загружен') < 0, 'после возврата слоя таблица должна ожить');
    assert.strictEqual(node('#rxInspect').disabled, false, 'кнопка проверки снова доступна');
    R._stopPolling();
});

(async () => {
    for (const [name, fn] of asyncChecks) {
        try {
            await fn();
            checks++;
            console.log('  \u2713 ' + name);
        } catch (e) {
            failures.push(name + ': ' + (e && e.message));
            console.error('  \u2717 ' + name + ' — ' + (e && e.message));
        }
    }
    R._stopPolling();
    console.log('\n' + checks + ' проверок зелёные, ' + failures.length + ' красных');
    if (failures.length > 0) {
        failures.forEach(function (f) { console.error('  FAIL: ' + f); });
        process.exit(1);
    }
})();

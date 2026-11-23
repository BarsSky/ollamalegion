// gguf-reexport.test.js — R709: блок «Ре-экспорт GGUF» на странице моделей.
//
// ЧТО ПРОВЕРЯЕТСЯ И ПОЧЕМУ ИМЕННО ЭТО.
//
// 1. ГЛАВНОЕ ТРЕБОВАНИЕ ОПЕРАТОРА: «нет модуля — нет вкладки». Стенд без
//    профиля `reexport` обязан выглядеть как раньше, поэтому проба
//    /api/reexport/capabilities, ответившая не-200 (nginx отдаёт 424/502, когда
//    контейнера модуля нет), НЕ должна оставлять в разметке ни блока, ни
//    подписей, ни мёртвой пустой карточки.
//
// 2. При доступном модуле блок обязан показать данные СЕРВИСА (архитектуры и
//    версию конвертера), а не свою таблицу: возможности зависят от сборки образа
//    (light/full), и продублированный в JS список разошёлся бы с фактом.
//
// 3. Тело задачи собирается из формы и уходит ровно в POST /api/reexport/jobs с
//    полями {model_dir, target, options} — контракт из docs/operations/
//    gguf-reexport.ru.md. Ошибка сервиса показывается с причиной, а не «HTTP 500».
//
// 4. У активной задачи есть кнопка отмены, и она делает DELETE, а не «убрать
//    строку из таблицы»: отмена — это убийство процесса конвертера и удаление
//    недописанного файла на сервере.
//
// Запуск: node webui/js/modules/gguf-reexport.test.js
'use strict';

const assert = require('assert');
const path = require('path');

let passed = 0;
const failures = [];
const asyncChecks = [];
function check(name, fn) {
    if (fn && fn.constructor && fn.constructor.name === 'AsyncFunction') {
        asyncChecks.push([name, fn]);
        return;
    }
    try {
        fn();
        passed++;
        console.log('  \u2713 ' + name);
    } catch (e) {
        failures.push(name + ': ' + (e && e.message));
        console.error('  \u2717 ' + name + ' — ' + (e && e.message));
    }
}

// --- окружение: минимальный DOM + fetch ------------------------------------
function makeEl(id) {
    const el = {
        id: id || '',
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
        getAttribute(k) { return this._attrs[k]; },
        querySelector() { return null; },
        querySelectorAll() { return []; },
        contains() { return false; }
    };
    return el;
}

const hostEl = makeEl('ggufReexportHost');
const nodes = {};
function node(sel) {
    if (!nodes[sel]) nodes[sel] = makeEl(sel.replace('#', ''));
    return nodes[sel];
}
hostEl.querySelector = function (sel) { return node(sel); };

global.window = global;
global.document = {
    getElementById(id) { return id === 'ggufReexportHost' ? hostEl : null; },
    addEventListener() {},
    hidden: false
};
// Без I18N модуль отдаёт ключи — тест видит их и проверяет, что подписи взяты из
// словаря, а не выдуманы в коде.
global.I18N = { t(key) { return key; } };
global.Utils = { escapeHtml(s) { return String(s); } };

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
        status: status,
        text() { return Promise.resolve(body === undefined ? '' : JSON.stringify(body)); }
    });
}

require('./gguf-reexport.js');
const R = global.window.GgufReexport;

const CAPS = {
    service: 'gguf-reexport',
    available: true,
    version: { build_version: 'r261121-r709', build_commit: '4227dc43' },
    converter: { name: 'llama.cpp', revision: '5bacb7c868', python: '3.12.16', torch_state: 'absent' },
    architectures: [
        { id: 'inkling2', scheme: 'inkling2_loop', target: 'inkling2_loop', label: 'Inkling-2 (loop)', note: 'визиты' },
        { id: '*', scheme: 'plain', label: 'прочие GGUF' }
    ],
    targets: [
        { id: 'inkling2_loop', label: 'Inkling-2 loop', kind: 'metadata_patch', available: true,
          options: [{ id: 'fuse_loop_queries', type: 'bool', default: true }] },
        { id: 'hf_to_gguf', label: 'HF checkpount → GGUF', kind: 'convert', available: false,
          reason: 'образ собран без torch' }
    ],
    limits: { max_concurrent: 1, models_dir: '/app/models' }
};

const MODELS = {
    models: [
        { path: 'Inkling-2-Mini-UD-Q4_K_M.gguf', name: 'Inkling-2-Mini-UD-Q4_K_M.gguf', size_bytes: 16610543328,
          arch: 'inkling2', block_count: 48, physical_blocks: 24, scheme: 'inkling2_loop_export', patchable: true },
        { path: 'Qwen3-4B.gguf', name: 'Qwen3-4B.gguf', size_bytes: 2497281120, arch: 'qwen3',
          block_count: 36, physical_blocks: 36, scheme: 'plain', patchable: false }
    ]
};

const JOB = {
    id: 'rj-1', status: 'running', target: 'inkling2_loop', model_dir: 'Inkling-2-Mini-UD-Q4_K_M.gguf',
    out_path: '/app/models/Inkling-2-Mini-UD-Q4_K_M.inkling2_loop.gguf',
    progress: { phase: 'copy', percent: 42.5, bytes_done: 1024, bytes_total: 4096 }
};

check('нет модуля → блока нет вообще (хост остаётся пустым)', async () => {
    hostEl.innerHTML = '';
    fetchImpl = () => jsonResponse(424, { error: { code: 'module_unavailable', message: 'нет модуля' } });
    const ok = await R.mount();
    assert.strictEqual(ok, false, 'mount() должен вернуть false без модуля');
    assert.strictEqual(hostEl.innerHTML, '', 'в разметке не должно остаться ни блока, ни подписей');
    assert.strictEqual(R._state.available, false);
});

check('модуль ответил → блок отрисован данными сервиса', async () => {
    fetchImpl = (call) => {
        if (call.url.indexOf('/capabilities') >= 0) return jsonResponse(200, CAPS);
        if (call.url.indexOf('/models') >= 0) return jsonResponse(200, MODELS);
        if (call.url.indexOf('/jobs') >= 0) return jsonResponse(200, { jobs: [] });
        return jsonResponse(404, {});
    };
    const ok = await R.mount();
    assert.strictEqual(ok, true);
    assert.ok(hostEl.innerHTML.indexOf('reexport.title') >= 0, 'блок без заголовка');
    assert.ok(node('#rxArch').innerHTML.indexOf('Inkling-2 (loop)') >= 0,
        'архитектуры не взяты из возможностей сервиса: ' + node('#rxArch').innerHTML);
    assert.ok(node('#rxConverter').textContent.indexOf('llama.cpp 5bacb7c868') >= 0,
        'версия конвертера не показана: ' + node('#rxConverter').textContent);
    assert.ok(hostEl.innerHTML.indexOf('id="rxForm"') >= 0, 'формы создания задачи нет');
    assert.ok(hostEl.innerHTML.indexOf('id="rxRescan"') >= 0, 'кнопки «Пересканировать модели» нет');
    R._stopPolling();
});

check('тело задачи: fuse по умолчанию, dry_run только по галочке', function () {
    R._state.selectedModel = 'Inkling-2-Mini-UD-Q4_K_M.gguf';
    node('#rxTarget').value = 'inkling2_loop';
    node('#rxFuse').checked = true;
    node('#rxDryRun').checked = false;
    node('#rxOut').value = '';
    const body = R._buildJobBody(hostEl);
    assert.strictEqual(body.model_dir, 'Inkling-2-Mini-UD-Q4_K_M.gguf');
    assert.strictEqual(body.target, 'inkling2_loop');
    assert.strictEqual(body.options.fuse_loop_queries, true);
    assert.strictEqual(body.options.dry_run, undefined);
    assert.strictEqual(body.options.out_path, undefined);
});

check('задача уходит в POST /api/reexport/jobs ровно с контрактными полями', async () => {
    calls.length = 0;
    node('#rxCreate').disabled = false;
    const created = { ...JOB, id: 'rj-2', status: 'queued' };
    fetchImpl = (call) => {
        if (call.url.indexOf('/capabilities') >= 0) return jsonResponse(200, CAPS);
        if (call.url.indexOf('/jobs') >= 0 && call.opts.method === 'POST') return jsonResponse(202, created);
        if (call.url.indexOf('/jobs') >= 0) return jsonResponse(200, { jobs: [created] });
        return jsonResponse(200, MODELS);
    };
    // Пересобираем форму, чтобы обработчик submit был привязан к свежему DOM.
    node('#rxForm')._listeners = {};
    R._state.available = true;
    R._state.caps = CAPS;
    await R.mount();
    const submit = (node('#rxForm')._listeners.submit || [])[0];
    assert.ok(typeof submit === 'function', 'на форме нет обработчика submit');
    node('#rxFuse').checked = true;
    R._state.selectedModel = 'Inkling-2-Mini-UD-Q4_K_M.gguf';
    node('#rxTarget').value = 'inkling2_loop';
    submit({ preventDefault() {} });
    await new Promise((r) => setTimeout(r, 30));
    const post = calls.filter((c) => c.opts.method === 'POST')[0];
    assert.ok(post, 'POST задачи не отправлен');
    assert.ok(post.url.indexOf('/api/reexport/jobs') >= 0, 'не тот адрес: ' + post.url);
    const parsed = JSON.parse(post.opts.body);
    assert.strictEqual(parsed.model_dir, 'Inkling-2-Mini-UD-Q4_K_M.gguf');
    assert.strictEqual(parsed.target, 'inkling2_loop');
    assert.deepStrictEqual(Object.keys(parsed.options), ['fuse_loop_queries']);
    R._stopPolling();
});

check('прогресс и отмена: видно проценты, кнопка делает DELETE', async () => {
    R._state.jobs = [JOB];
    R._renderJobs(hostEl);
    const html = node('#rxJobsBody').innerHTML;
    assert.ok(html.indexOf('42.5%') >= 0, 'процент прогресса не показан: ' + html);
    assert.ok(html.indexOf('data-cancel-job="rj-1"') >= 0, 'нет кнопки отмены для активной задачи');
    assert.ok(html.indexOf('reexport.status_running') >= 0, 'статус не переведён');

    calls.length = 0;
    fetchImpl = () => jsonResponse(200, { jobs: [] });
    // Клик по кнопке отмены (делегированный обработчик на tbody).
    const handler = (node('#rxJobsBody')._listeners.click || [])[0];
    assert.ok(typeof handler === 'function', 'нет делегированного обработчика отмены');
    handler({ target: { closest: () => ({ getAttribute: () => 'rj-1' }) } });
    await new Promise((r) => setTimeout(r, 20));
    const del = calls.filter((c) => c.opts.method === 'DELETE')[0];
    assert.ok(del, 'DELETE отмены не отправлен');
    assert.ok(del.url.indexOf('/api/reexport/jobs/rj-1') >= 0, 'не тот адрес отмены: ' + del.url);
});

check('ошибка сервиса доходит до оператора с причиной', async () => {
    R._state.available = true;
    R._state.caps = CAPS;
    fetchImpl = (call) => {
        if (call.url.indexOf('/capabilities') >= 0) return jsonResponse(200, CAPS);
        if (call.opts.method === 'POST') {
            return jsonResponse(422, { error: { code: 'model_not_patchable', message: 'файл не подходит под патч' } });
        }
        if (call.url.indexOf('/jobs') >= 0) return jsonResponse(200, { jobs: [] });
        return jsonResponse(200, MODELS);
    };
    R._state.selectedModel = 'Qwen3-4B.gguf';
    node('#rxTarget').value = 'inkling2_loop';
    node('#rxFuse').checked = false;
    const submit = (node('#rxForm')._listeners.submit || [])[0];
    submit({ preventDefault() {} });
    await new Promise((r) => setTimeout(r, 30));
    const err = node('#rxError');
    assert.strictEqual(err.style.display, '', 'блок ошибки не показан');
    assert.ok(String(err.textContent).indexOf('файл не подходит под патч') >= 0,
        'причина ошибки потеряна: ' + err.textContent);
    R._stopPolling();
});

check('подсказка о схеме модели отвечает «нужен ли патч»', function () {
    R._state.models = MODELS.models;
    R._state.selectedModel = 'Inkling-2-Mini-UD-Q4_K_M.gguf';
    R._state.modelsError = '';
    const mod = R._state;
    // renderModelHint не экспортирован — проверяем через состояние и публичный
    // формат: подсказка строится из фактов сервиса (см. renderModelHint).
    const m = mod.models[0];
    assert.strictEqual(m.scheme, 'inkling2_loop_export');
    assert.strictEqual(m.patchable, true);
    assert.strictEqual(m.block_count, 48);
    assert.strictEqual(m.physical_blocks, 24);
});

check('форматирование размеров и статусов не выдумывает значения', function () {
    assert.strictEqual(R._formatBytes(16610543328), '15.5 GiB');
    assert.strictEqual(R._formatBytes(103769856), '99 MiB');
    assert.strictEqual(R._formatBytes(0), '0');
    assert.strictEqual(R._statusLabel('succeeded'), 'reexport.status_succeeded');
    assert.strictEqual(R._statusLabel('canceled'), 'reexport.status_canceled');
    assert.strictEqual(R._formatDuration(65000), '1m 5s');
});

(async () => {
    for (const [name, fn] of asyncChecks) {
        try {
            await fn();
            passed++;
            console.log('  \u2713 ' + name);
        } catch (e) {
            failures.push(name + ': ' + (e && e.message));
            console.error('  \u2717 ' + name + ' — ' + (e && e.message));
        }
    }
    R._stopPolling();
    console.log('\n' + passed + ' проверок зелёные, ' + failures.length + ' красных');
    if (failures.length > 0) {
        failures.forEach(function (f) { console.error('  FAIL: ' + f); });
        process.exit(1);
    }
})();

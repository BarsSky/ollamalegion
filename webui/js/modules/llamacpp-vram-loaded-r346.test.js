// llamacpp-vram-loaded-r346.test.js — R346 (2026-10-30).
//
// Запуск: node webui/js/modules/llamacpp-vram-loaded-r346.test.js
//
// ОСНОВАНИЕ. docs/plans/2026-10-30-metrics-visibility-decisions.md, часть «что
// осталось сделать»: поля, которые балансер ПИШЕТ, а UI не показывал
// (availableVramMb/totalVramMb, loadedAt, loadingSizeBytes), плюс мёртвые пути
// чтения (b.llamaCppMetrics), из-за которых фолбэки выглядели работающими.
//
// ЧТО БЫЛО НЕ ТАК ДО R346 (каждый пункт — живой дефект, а не косметика):
//   1. Раскрытая строка llama.cpp-бэкенда не показывала загруженные модели:
//      секция «Loaded Models» читалась из backend.ollama.runningModels, а у
//      llama.cpp-бэкенда ollama == nil (метрики в llamaCpp) — секция не
//      отрисовывалась НИКОГДА, хотя список лежит в llamaCpp.loadedModels.
//   2. Свободная VRAM узла («почему не поднялась вторая копия») не выводилась:
//      availableVramMb/totalVramMb писал поллер, читал только сырой JSON.
//   3. «Когда модель поднялась» (loadedAt) не было видно: при «модель вдруг
//      стала медленной» это первое, что нужно.
//   4. Размер загружаемого файла (loadingSizeBytes) не попадал в бейдж загрузки:
//      «загрузка 3 минуты» без размера не отличает норму от зависания.
//   5. Путь b.llamaCppMetrics.loadingModels — мёртвый (такого ключа не отдаёт ни
//      один эндпоинт); из-за него отсутствие данных выглядело как «источник есть,
//      но моделей нет».
//
// Проверяется:
//   1. VRAM-секция с total/free из llamaCpp;
//   2. RPC-узлы показываются ТОЛЬКО при rpcEnabled (иначе это нули и шум);
//   3. секция Loaded Models рисуется из llamaCpp.loadedModels (регресс №1);
//   4. loadedAt выводится читаемым временем, мусорная строка не рисуется;
//   5. loadingSizeBytes попадает в подсказку бейджа загрузки;
//   6. мёртвого пути llamaCppMetrics в исходниках больше нет (сторож).

'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');

// --- минимальный DOM (как в donor-mark-r304.test.js) -------------------------
function makeEl(id) {
    const el = {
        id: id || '',
        innerHTML: '',
        textContent: '',
        value: '',
        hidden: false,
        style: {},
        dataset: {},
        children: [],
        classList: { add() {}, remove() {}, contains() { return false; } },
        setAttribute() {},
        getAttribute() { return null; },
        appendChild() {},
        removeChild() {},
        remove() {},
        addEventListener() {},
        querySelector() { return null; },
        querySelectorAll() { return []; },
    };
    return el;
}

const byId = {};
global.document = {
    createElement() {
        let text = '';
        const el = makeEl('');
        Object.defineProperty(el, 'textContent', { get() { return text; }, set(v) { text = String(v); } });
        Object.defineProperty(el, 'innerHTML', {
            get() {
                return text.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
            },
        });
        return el;
    },
    getElementById(id) {
        if (id === 'toastContainer') return null;
        return byId[id] || (byId[id] = makeEl(id));
    },
    querySelector() { return null; },
    querySelectorAll() { return []; },
    addEventListener() {},
    removeEventListener() {},
    body: { appendChild() {} },
};
global.window = global;

// --- i18n --------------------------------------------------------------------
// Подписи — как в ru.js: если ключ исчезнет из словаря, тест покажет сырой ключ
// (в UI оператор увидел бы именно его).
const I18N_MAP = {
    'renderers.section_vram': '🎛️ VRAM',
    'renderers.vram_total': 'Всего VRAM',
    'renderers.free_vram': 'Свободно VRAM',
    'renderers.vram_remote_free': 'Свободно VRAM (RPC-узлы)',
    'renderers.vram_remote_nodes': 'узлов',
    'renderers.loaded_at': 'Загружена в',
    'renderers.section_loaded_models': '🤖 Загруженные модели',
    'renderers.section_llama_cpp_params': 'Параметры llama.cpp',
    'renderers.context': 'Контекст',
    'renderers.gpu_layers': 'GPU Layers',
    'renderers.threads': 'Threads',
    'renderers.batch_size': 'Batch Size',
    'renderers.quantization': 'Quantization',
    'renderers.section_disk_network': 'Диск и сеть',
    'renderers.disk_total': 'Всего диска',
    'gguf.loading_indicator': 'Загрузка',
    'gguf.col_loading_count': 'Загрузка: {count}',
};

function tr(key, vars) {
    let s = Object.prototype.hasOwnProperty.call(I18N_MAP, key) ? I18N_MAP[key] : key;
    if (vars && typeof vars === 'object') {
        Object.keys(vars).forEach(function (k) {
            s = s.split('{' + k + '}').join(String(vars[k]));
        });
    }
    return s;
}

global.window.I18N = { t: tr, getLang() { return 'ru'; } };

require('./utils.js');
global.Utils = global.window.Utils;
require('./renderers.js');
const R = global.window.Renderers;
assert.ok(R && typeof R.backendsPage === 'function', 'renderers.js должен экспортировать Renderers.backendsPage');

let passed = 0;
function check(name, fn) {
    fn();
    passed++;
    console.log('  \u2713 ' + name);
}

function renderBackendsPage(backends) {
    byId['backendsManageBody'] = makeEl('backendsManageBody');
    R.backendsPage(backends);
    return byId['backendsManageBody'].innerHTML;
}

// Бэкенд llama.cpp в том виде, в каком его отдаёт /api/v1/cluster:
// метрики — в llamaCpp, поля ollama НЕТ (именно на этом ломался №1).
function cppBackend(overrides) {
    return Object.assign({
        id: 'cppworker-gpu-bundled-agent',
        backendType: 'llama_cpp',
        type: 'llama_cpp',
        status: 'healthy',
        host: '192.0.2.10',
        cppWorkerPort: 18092,
        llamaCpp: {
            contextLength: 32768,
            nGpuLayers: 99,
            totalVramMb: 8192,
            availableVramMb: 5200,
            loadedModels: [],
        },
    }, overrides || {});
}

// Время «сегодня в 12:34» — так проверка не зависит от часового пояса машины:
// берём локальную дату и обратно разбираем её же.
function todayAt(hours, minutes) {
    const now = new Date();
    return new Date(now.getFullYear(), now.getMonth(), now.getDate(), hours, minutes).toISOString();
}

console.log('R346: VRAM/loadedAt/loadingSizeBytes в панели (llama.cpp)');

// --- 1. VRAM ----------------------------------------------------------------
check('Раскрытая строка llama.cpp: свободно/всего VRAM из метрик поллера', function () {
    const html = renderBackendsPage([cppBackend()]);
    assert.ok(html.indexOf('🎛️ VRAM') !== -1, 'нет секции VRAM: ' + html.slice(0, 800));
    assert.ok(html.indexOf('Всего VRAM') !== -1, 'нет подписи «всего VRAM»');
    assert.ok(html.indexOf('8.0 GB') !== -1, 'нет totalVramMb (8192 MB → 8.0 GB): ' + html.slice(0, 1200));
    assert.ok(html.indexOf('5.1 GB') !== -1, 'нет availableVramMb (5200 MB → 5.1 GB): ' + html.slice(0, 1200));
});

check('VRAM-секции нет, когда поллер ничего не прислал (нули не выдаём за данные)', function () {
    const b = cppBackend({ llamaCpp: { contextLength: 32768, loadedModels: [] } });
    const html = renderBackendsPage([b]);
    assert.strictEqual(html.indexOf('🎛️ VRAM'), -1, 'при нулевых метриках секция VRAM не рисуется');
});

// --- 2. RPC-узлы ------------------------------------------------------------
check('VRAM удалённых узлов — только при rpcEnabled (иначе шум из нулей)', function () {
    const off = renderBackendsPage([cppBackend({
        llamaCpp: { totalVramMb: 8192, availableVramMb: 5200, vramRemoteFreeMb: 6000, vramRemoteNodes: 1, loadedModels: [] },
    })]);
    assert.strictEqual(off.indexOf('RPC-узлы'), -1, 'без rpcEnabled RPC-строки быть не должно');

    const on = renderBackendsPage([cppBackend({
        llamaCpp: { totalVramMb: 8192, availableVramMb: 5200, rpcEnabled: true, vramRemoteFreeMb: 6000, vramRemoteNodes: 1, loadedModels: [] },
    })]);
    assert.ok(on.indexOf('RPC-узлы') !== -1, 'при rpcEnabled строка обязана появиться: ' + on.slice(0, 1200));
    assert.ok(on.indexOf('5.9 GB') !== -1, 'нет свободной VRAM RPC-узлов (6000 MB → 5.9 GB)');
    assert.ok(on.indexOf('1 узлов') !== -1, 'нет числа узлов');
});

// --- 3. Loaded Models из llamaCpp (регресс №1) ------------------------------
check('Loaded Models рисуется из llamaCpp.loadedModels (без поля ollama)', function () {
    const b = cppBackend({
        llamaCpp: {
            totalVramMb: 8192, availableVramMb: 5200,
            loadedModels: [{ name: 'gemma-4-E4B-it-Q4_K_M', size: 5 * 1024 * 1024 * 1024 }],
        },
        // b.loadedModels (/api/v1/backends) в этом снимке нет — как при недоступном
        // REST-эндпоинте, когда страница живёт на одном /api/v1/cluster.
    });
    const html = renderBackendsPage([b]);
    assert.ok(html.indexOf('🤖 Загруженные модели') !== -1,
        'секция «Загруженные модели» обязана рисоваться: ' + html.slice(0, 1200));
    assert.ok(html.indexOf('gemma-4-E4B-it-Q4_K_M') !== -1, 'имя загруженной модели потерялось');
    assert.ok(html.indexOf('5.0 GB') !== -1, 'размер загруженной модели потерялся');
});

check('Загруженные модели берутся из backend.loadedModels, если llamaCpp пуст', function () {
    const b = cppBackend({
        llamaCpp: { contextLength: 32768, loadedModels: [] },
        loadedModels: [{ name: 'qwen3-14b-q4', size: 8 * 1024 * 1024 * 1024 }],
    });
    const html = renderBackendsPage([b]);
    assert.ok(html.indexOf('qwen3-14b-q4') !== -1,
        'фолбэк на /api/v1/backends обязан работать: ' + html.slice(0, 1200));
});

// --- 4. loadedAt ------------------------------------------------------------
check('loadedAt выводится читаемым временем рядом с моделью', function () {
    const b = cppBackend({
        llamaCpp: {
            totalVramMb: 8192, availableVramMb: 5200,
            loadedModels: [{ name: 'gemma-4-E4B-it-Q4_K_M', size: 1024 * 1024 * 1024, loadedAt: todayAt(12, 34) }],
        },
    });
    const html = renderBackendsPage([b]);
    assert.ok(html.indexOf('⏱') !== -1, 'нет маркера времени загрузки: ' + html.slice(0, 1600));
    assert.ok(html.indexOf('12:34') !== -1, 'время загрузки обязано быть в локальном HH:MM: ' + html.slice(0, 1600));
    assert.ok(html.indexOf('Загружена в') !== -1, 'подсказка «загружена в» потерялась');
});

check('Мусорный loadedAt не рисуется и не роняет отрисовку', function () {
    const b = cppBackend({
        llamaCpp: {
            totalVramMb: 8192, availableVramMb: 5200,
            loadedModels: [{ name: 'gemma-4-E4B-it-Q4_K_M', size: 1024 * 1024 * 1024, loadedAt: 'не дата' }],
        },
    });
    let html = '';
    assert.doesNotThrow(function () { html = renderBackendsPage([b]); }, 'битая дата не должна ронять страницу');
    assert.strictEqual(html.indexOf('⏱'), -1, 'битую дату не показываем (иначе «Invalid Date» в UI)');
});

// --- 5. loadingSizeBytes ----------------------------------------------------
check('Размер загружаемого файла попадает в подсказку бейджа загрузки', function () {
    const b = cppBackend({
        loadingModels: [{
            name: 'gemma-4-E4B-it-Q4_K_M',
            state: 'loading',
            loadingStartedAt: new Date().toISOString(),
            loadingSizeBytes: 5 * 1024 * 1024 * 1024,
        }],
        loadingModelCount: 1,
    });
    const html = renderBackendsPage([b]);
    assert.ok(html.indexOf('Загрузка: gemma-4-E4B-it-Q4_K_M · 5.0 GB') !== -1,
        'подсказка бейджа обязана содержать размер файла: ' + html.slice(0, 1600));
});

// --- 6. сторож: мёртвый путь llamaCppMetrics --------------------------------
check('Мёртвого пути b.llamaCppMetrics в исходниках не осталось', function () {
    // Комментарии вырезаем: пояснение «почему путь удалён» обязано называть поле
    // по имени, а проверяем мы КОД. Иначе сторож запрещал бы документировать
    // собственную правку.
    // Файлы в репозитории с CRLF, поэтому строковый комментарий режем по
    // [^\r\n]*: `.` в JS не матчит \r, и `/\/\/.*$/` на CRLF-файле не срабатывает.
    function stripComments(src) {
        return src
            .replace(/\/\*[\s\S]*?\*\//g, '')
            .replace(/\/\/[^\r\n]*/g, '');
    }
    const files = ['renderers.js', 'monitor-backends.js'];
    files.forEach(function (f) {
        const src = stripComments(fs.readFileSync(path.join(__dirname, f), 'utf8'));
        assert.strictEqual(/b\.llamaCppMetrics/.test(src), false,
            f + ': обращение к b.llamaCppMetrics должно быть удалено (ключа не отдаёт ни один эндпоинт)');
    });
});

console.log('\nR346: ' + passed + ' проверок пройдено');

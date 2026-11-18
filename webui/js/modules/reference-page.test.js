// reference-page.test.js — R493 (2026-11-17): логика страницы «Справка и шаблоны»
// без браузера и без DOM-библиотек (как gguf-*.test.js: стабы global.window,
// global.document, window.Api).
//
// ЧТО ПРОВЕРЯЕМ И ПОЧЕМУ ИМЕННО ЭТО:
//   1. Карточка записи показывает ВСЁ, что просил оператор: заголовок, «к чему
//      применимо», таблицу параметров С КОММЕНТАРИЯМИ, источники, автора, дату и
//      revision. Данные экранируются (их правит человек).
//   2. Фильтр по модели: запись без appliesTo — «общая справка» (решение
//      человека №1) и остаётся в выдаче, а чужая модель отсекается.
//   3. «Применить» НЕАКТИВНА, если загруженных целей нет, и если не выбран
//      бэкенд: сервер на пустой backendId в mode="profile+reload" отвечает 400
//      backend_required (решение человека №3), а «применить на все узлы» задевает
//      чужие/недоступные узлы.
//   4. Коды ошибок контракта превращаются в человеческий текст: 400
//      backend_required — подсказка выбрать бэкенд, 503 store_unavailable —
//      «хранилище не инициализировано», 409 revision_conflict — «перечитайте»,
//      422 profile_does_not_fit_memory — вердикт памяти. Никаких «что-то пошло
//      не так».
//   5. Формат diff — «contextLength: 16384 → 32768», отсутствующее значение «—».
//   6. Список «где загружена модель» строится из cluster/models/loaded и
//      нечувствителен к регистру и суффиксу .gguf.
//   7. Редактор берёт метаданные полей из MODEL_PARAM_SPECS (тип, границы,
//      варианты) — своей таблицы полей у страницы нет.
//   8. Провайдер 'reference' зарегистрирован, при вызове обновляет список и
//      цели и возвращает Promise; mount() идемпотентен (тик авто-обновления не
//      пересобирает страницу).
//
// Запуск: node webui/js/modules/reference-page.test.js
'use strict';

const assert = require('assert');

// ---------------------------------------------------------------------------
// Окружение: минимальный DOM + Api + I18N
// ---------------------------------------------------------------------------

function makeElement(id) {
    const classes = new Set();
    return {
        id: id,
        hidden: false,
        innerHTML: '',
        textContent: '',
        className: '',
        style: {},
        classList: {
            add(c) { classes.add(c); },
            remove(c) { classes.delete(c); },
            contains(c) { return classes.has(c); },
            toggle(c, on) { if (on) classes.add(c); else classes.delete(c); }
        },
        addEventListener() {},
        querySelector() { return null; },
        querySelectorAll() { return []; },
        getAttribute() { return null; },
        setAttribute() {},
        appendChild() {},
        insertAdjacentHTML() {},
        scrollIntoView() {}
    };
}

const elements = {};
function getEl(id) {
    if (!Object.prototype.hasOwnProperty.call(elements, id)) elements[id] = makeElement(id);
    return elements[id];
}

global.window = global;
global.document = {
    getElementById: getEl,
    addEventListener() {},
    querySelectorAll() { return []; },
    visibilityState: 'visible'
};

// I18N: как настоящий пакет без перевода — возвращает КЛЮЧ. Значит любая
// подпись, которую видит оператор, пришла из fallback-аргумента t(key, fallback).
// Так тест проверяет не только код, но и то, что fallback'и написаны по-русски.
global.window.I18N = {
    t: function (key) { return key; }
};

// Тексты, которые проверяются ниже: русские fallback'и, а не данные.
const RU_FALLBACK = {
    backend_required: /бэкенд/i,
    store_unavailable: /хранилище не инициализировано/i,
    revision_conflict: /перечит/i,
    not_feasible: /не помещается/i,
    not_found: /не найдена/i
};

// --- модель-спецификации: то, что в приложении даёт gguf-renderer-detail-render.js
global.window.GgufModule = {
    MODEL_PARAM_SPECS: [
        { field: 'contextLength', type: 'int', min: 256, max: 1048576, step: 256, label: 'gguf.ctx_size' },
        { field: 'parallel', type: 'int', min: 0, max: 32, step: 1, label: 'gguf.parallel' },
        {
            field: 'kvCacheType', type: 'enum', options: ['f16', 'q8_0', 'q4_0'],
            label: 'gguf.kv_cache_type'
        },
        { field: 'flashAttn', type: 'bool', label: 'gguf.flash_attn' }
    ]
};

// --- реестр провайдеров data-refresh.js (его же читает статический тест)
const providers = {};
global.window.DataRefresh = {
    register: function (page, fn) {
        (providers[page] = providers[page] || []).push(fn);
        return true;
    }
};

// --- HTTP-стабы: страница обязана ходить ТОЛЬКО через Api.reference
const calls = { recommendations: 0, templates: 0, loaded: 0 };
let failNext = null;

const REC_A = {
    id: 'qwen3-4b-8gb-agent',
    title: '4B Q4 на 8 ГБ VRAM: окно 32K',
    appliesTo: { modelPattern: 'Qwen3-4B-Instruct-2507*', backendType: 'llama_cpp', vramMinMB: 8192 },
    params: [
        { name: 'contextLength', value: 32768, unit: 'tokens', comment: 'окно держит сессию Cline' },
        { name: 'kvCacheType', value: 'q8_0' }
    ],
    comment: 'Агентский цикл: размышления едят бюджет хода.',
    sources: [{ doc: 'docs/operations/model-tuning-reference.ru.md', section: '3.2' }],
    author: 'operator',
    createdAt: '2026-11-17T10:00:00Z',
    updatedAt: '2026-11-17T10:00:00Z',
    revision: 3
};
const REC_GENERAL = {
    id: 'tuning-baseline-32k',
    title: 'Ориентиры 32K',
    params: [{ name: 'contextLength', value: 32768 }],
    author: 'docs-import',
    revision: 1
};
const REC_OTHER = {
    id: 'gemma-4-reasoning-tags',
    title: 'Gemma 4: теги размышлений',
    appliesTo: { modelPattern: 'gemma-4*' },
    params: [{ name: 'contextLength', value: 65536 }],
    revision: 1
};
const TPL = {
    id: 'agent-8gb',
    name: 'Агент на 8 ГБ VRAM',
    version: 2,
    params: { contextLength: 32768, kvCacheType: 'q8_0', parallel: 1 },
    comment: 'Годится для 4B–8B Q4 на 8 ГБ.',
    basedOn: 'qwen3-4b-8gb-agent',
    revision: 4
};

let loadedModels = [{ name: 'Qwen3-4B-Instruct-2507-Q4_K_M', backendId: 'cppworker-gpu', backendType: 'llama_cpp' }];

global.window.Api = {
    reference: {
        list: function (kind) {
            if (failNext) { const err = failNext; failNext = null; return Promise.reject(err); }
            if (kind === 'recommendations') {
                calls.recommendations++;
                return Promise.resolve({ items: [REC_A, REC_GENERAL, REC_OTHER], total: 3, schemaVersion: 1 });
            }
            calls.templates++;
            return Promise.resolve({ items: [TPL], total: 1, schemaVersion: 1 });
        },
        get: function () { return Promise.resolve({}); },
        upsert: function () { return Promise.resolve({ status: 'ok', recommendation: { revision: 5 } }); },
        remove: function () { return Promise.resolve({ status: 'ok' }); },
        toTemplate: function () { return Promise.resolve({ status: 'created' }); },
        preview: function () { return Promise.resolve({ ok: true, valid: { ok: true, violations: [] }, diff: [], targets: [], memory: {}, warnings: [] }); },
        apply: function () { return Promise.resolve({ _status: 200, status: 'applied', backends: [] }); }
    },
    clusterModels: {
        loaded: function () {
            calls.loaded++;
            return Promise.resolve({ count: loadedModels.length, models: loadedModels.slice() });
        }
    }
};

// confirm/prompt в тестах не должны открывать диалоги.
global.window.confirm = function () { return true; };
global.window.prompt = function () { return null; };

require('./reference-page.js');
const RP = global.window.ReferencePage;
assert.ok(RP, 'window.ReferencePage должен быть экспортирован (см. check_iife_exports.py)');

// ---------------------------------------------------------------------------
// Мини-раннер (как в остальных наборах WebUI)
// ---------------------------------------------------------------------------

let passed = 0;
const failures = [];
async function check(name, fn) {
    try {
        await fn();
        passed++;
        console.log('  \u2713 ' + name);
    } catch (e) {
        failures.push(name + ': ' + (e && e.message));
        console.error('  \u2717 ' + name + ' — ' + (e && e.message));
    }
}

(async function main() {

    // =======================================================================
    // 1. Карточка записи
    // =======================================================================

    await check('карточка записи: заголовок, appliesTo, параметры с комментарием, источники, автор, revision', function () {
        const html = RP.renderRecommendationRow(REC_A);
        assert.ok(html.indexOf('qwen3-4b-8gb-agent') !== -1, 'нет id записи');
        assert.ok(html.indexOf('4B Q4 на 8 ГБ VRAM') !== -1, 'нет заголовка');
        assert.ok(html.indexOf('Qwen3-4B-Instruct-2507*') !== -1, 'нет appliesTo.modelPattern');
        assert.ok(html.indexOf('VRAM') !== -1, 'нет границы VRAM');
        assert.ok(html.indexOf('contextLength') !== -1, 'нет имени параметра');
        assert.ok(html.indexOf('32768') !== -1, 'нет значения параметра');
        assert.ok(html.indexOf('окно держит сессию Cline') !== -1,
            'комментарий параметра не показан — именно он отличает справку от голых чисел');
        assert.ok(html.indexOf('docs/operations/model-tuning-reference.ru.md') !== -1, 'нет источника');
        assert.ok(html.indexOf('operator') !== -1, 'нет автора');
        assert.ok(html.indexOf('revision 3') !== -1, 'нет revision');
        assert.ok(html.indexOf('Агентский цикл') !== -1, 'нет обоснования записи');
    });

    await check('карточка записи: мутирующие кнопки помечены data-admin-only', function () {
        const html = RP.renderRecommendationRow(REC_A);
        const buttons = html.match(/<button[^>]*>/g) || [];
        const mutating = buttons.filter((b) => /data-ref-action="(edit-rec|delete-rec|to-template)"/.test(b));
        assert.strictEqual(mutating.length, 3, 'ожидались 3 мутирующие кнопки, найдено ' + mutating.length);
        mutating.forEach((b) => {
            assert.ok(b.indexOf('data-admin-only') !== -1,
                'мутирующая кнопка без data-admin-only: ' + b);
        });
    });

    await check('карточка записи: данные экранируются (их правит человек, а не сервер)', function () {
        const html = RP.renderRecommendationRow({
            id: 'xss', title: '<img src=x onerror=alert(1)>', comment: '<b>злой</b>',
            params: [{ name: 'contextLength', value: 1, comment: '"quoted"' }], revision: 1
        });
        assert.strictEqual(html.indexOf('<img'), -1, 'HTML из заголовка попал в разметку как тег');
        assert.ok(html.indexOf('&lt;img') !== -1, 'заголовок должен быть экранирован');
        assert.strictEqual(html.indexOf('<b>'), -1, 'HTML из обоснования попал в разметку как тег');
    });

    await check('карточка записи без appliesTo — «общая справка», а не пустое место', function () {
        const html = RP.renderRecommendationRow(REC_GENERAL);
        assert.ok(/общая справка/i.test(html), 'запись без appliesTo должна называться общей справкой');
    });

    // =======================================================================
    // 2. Фильтр по модели и поиск
    // =======================================================================

    await check('фильтр по модели: подходящая запись + общая справка, чужой шаблон отсечён', function () {
        const visible = RP.filterRecommendations([REC_A, REC_GENERAL, REC_OTHER], {
            model: 'Qwen3-4B-Instruct-2507-Q4_K_M', search: ''
        }).map((r) => r.id);
        assert.deepStrictEqual(visible.sort(), ['qwen3-4b-8gb-agent', 'tuning-baseline-32k'],
            'ожидались подходящая запись и общая справка, получено: ' + visible.join(','));
    });

    await check('фильтр по модели: glob и подстрока без «*»', function () {
        assert.strictEqual(RP.globMatch('Qwen3-4B*', 'Qwen3-4B-Instruct-2507-Q4_K_M'), true);
        assert.strictEqual(RP.globMatch('Qwen3-4B', 'Qwen3-4B-Instruct-2507-Q4_K_M'), true,
            'без звёздочек сравнение подстрочное: «Qwen3-4B» находит полное имя');
        assert.strictEqual(RP.globMatch('gemma-4*', 'Qwen3-4B-Instruct-2507-Q4_K_M'), false);
        assert.strictEqual(RP.globMatch('', 'любая'), true, 'пустой pattern = применимо ко всему');
    });

    await check('поиск идёт по заголовку, обоснованию и параметрам', function () {
        const byComment = RP.filterRecommendations([REC_A, REC_GENERAL, REC_OTHER], { search: 'размышления' });
        assert.deepStrictEqual(byComment.map((r) => r.id), ['qwen3-4b-8gb-agent']);
        const byParam = RP.filterRecommendations([REC_A, REC_GENERAL, REC_OTHER], { search: 'kvCacheType' });
        assert.deepStrictEqual(byParam.map((r) => r.id), ['qwen3-4b-8gb-agent']);
    });

    // =======================================================================
    // 3. «Применить»: гейт по загруженной цели и бэкенду
    // =======================================================================

    // R516: цель `loaded:false` — это узел, где файл модели ЛЕЖИТ НА ДИСКЕ
    // (каталог назвал его backendId). Сервер такое применение разрешает и грузит
    // модель сам (handlers_reference_apply.go:294-324,368-391), поэтому кнопка
    // активна, но с предупреждением «начнётся с загрузки». Запрет остаётся для
    // случая «целей нет вообще» — см. проверку profile+reload с пустым списком.
    await check('гейт: цель «файл на диске» разрешена с предупреждением (R516)', function () {
        const gate = RP.applyGate({
            model: 'Qwen3-4B-Instruct-2507-Q4_K_M', backendId: 'cppworker-gpu',
            mode: 'profile+reload', targets: [{ backendId: 'cppworker-gpu', loaded: false }]
        });
        assert.strictEqual(gate.disabled, false, 'узел с файлом на диске сервер разрешает — кнопка не должна быть disabled');
        assert.strictEqual(gate.warn, 'will_load', 'оператор обязан увидеть, что применение начнётся с загрузки');
    });

    await check('гейт: без выбранного бэкенда «Применить» запрещена (сервер ответит 400 backend_required)', function () {
        const gate = RP.applyGate({
            model: 'Qwen3-4B-Instruct-2507-Q4_K_M', backendId: '',
            mode: 'profile+reload', targets: [{ backendId: 'cppworker-gpu', loaded: true }]
        });
        assert.strictEqual(gate.disabled, true,
            'без явного бэкенда применять нельзя: «на все узлы» задевает чужие/недоступные');
        assert.strictEqual(gate.reason, 'backend_required');
    });

    await check('гейт: выбранный бэкенд обязан быть среди загруженных целей', function () {
        const wrong = RP.applyGate({
            model: 'M', backendId: 'cppworker-cyborg',
            mode: 'profile+reload', targets: [{ backendId: 'cppworker-gpu', loaded: true }]
        });
        assert.strictEqual(wrong.reason, 'backend_not_loaded');
        const ok = RP.applyGate({
            model: 'M', backendId: 'cppworker-gpu',
            mode: 'profile+reload', targets: [{ backendId: 'cppworker-gpu', loaded: true }]
        });
        assert.strictEqual(ok.disabled, false, 'модель загружена на выбранном узле — применять можно');
    });

    // R516 (2026-11-19): РЕШЕНИЕ ИЗМЕНЕНО по плану оператора §2.7.
    //
    // Было (решение человека №3): «если загруженных целей нет — „Применить" неактивна
    // в любом режиме». Стало: для profile-only бэкенд не нужен вовсе — сервер пишет
    // профиль по имени модели и reload не запускает, то есть «применять некуда» здесь
    // неправда. Для profile+reload запрет сохранён: там действительно нужен узел,
    // и «применить на все узлы» задевало бы чужие (см. проверку ниже).
    await check('гейт: profile-only бэкенда не требует и без загруженной цели разрешён (R516)', function () {
        const withTarget = RP.applyGate({
            model: 'M', backendId: '', mode: 'profile-only',
            targets: [{ backendId: 'cppworker-gpu', loaded: true }]
        });
        assert.strictEqual(withTarget.disabled, false, 'profile-only не перезагружает модель — бэкенд не нужен');
        const noTarget = RP.applyGate({ model: 'M', backendId: '', mode: 'profile-only', targets: [] });
        assert.strictEqual(noTarget.disabled, false,
            'profile-only пишет профиль по имени модели — пустой список целей не повод блокировать');
        const reloadNoTarget = RP.applyGate({ model: 'M', backendId: '', mode: 'profile+reload', targets: [] });
        assert.strictEqual(reloadNoTarget.disabled, true,
            'для profile+reload без целей «Применить» обязана быть неактивна');
        assert.strictEqual(reloadNoTarget.reason, 'no_loaded_target');
        assert.strictEqual(RP.applyGate({ model: '', mode: 'profile-only' }).reason, 'model_required',
            'модель нужна в любом режиме: без неё неизвестно, какой профиль менять');
    });

    await check('карточка шаблона: кнопка «Применить» disabled без загруженной цели и объясняет почему', function () {
        const state = {
            id: 'agent-8gb', model: 'Qwen3-4B-Instruct-2507-Q4_K_M', backendId: 'cppworker-gpu',
            mode: 'profile+reload', preview: null, error: null, busy: false
        };
        const html = RP.renderTemplateDetail(TPL, { tplState: state, models: ['Qwen3-4B-Instruct-2507-Q4_K_M'], targets: [] });
        assert.ok(/data-ref-action="apply" disabled/.test(html), 'кнопка «Применить» обязана быть disabled');
        assert.ok(/нигде не загружена/i.test(html), 'рядом должна быть причина, а не молчаливая неактивная кнопка');
    });

    await check('карточка шаблона: кнопка «Применить» активна при выбранной загруженной цели', function () {
        const state = {
            id: 'agent-8gb', model: 'Qwen3-4B-Instruct-2507-Q4_K_M', backendId: 'cppworker-gpu',
            mode: 'profile+reload', preview: null, error: null, busy: false
        };
        const targets = [{ backendId: 'cppworker-gpu', loaded: true }];
        const html = RP.renderTemplateDetail(TPL, { tplState: state, models: ['Qwen3-4B-Instruct-2507-Q4_K_M'], targets });
        assert.ok(/data-ref-action="apply">/.test(html), 'кнопка должна быть активна');
        assert.ok(html.indexOf('<option value="cppworker-gpu" selected>') !== -1,
            'бэкенд должен быть показан явно, а не «применить везде»');
        assert.ok(/data-admin-only data-ref-action="apply"/.test(html), 'применение — мутация, только admin');
    });

    // =======================================================================
    // 4. Коды ошибок → человеческий текст
    // =======================================================================

    await check('ошибка 400 backend_required: подсказка выбрать бэкенд', function () {
        const text = RP.errorText({ status: 400, body: { error: 'backend_required', model: 'Qwen3' } });
        assert.ok(RU_FALLBACK.backend_required.test(text), 'нет слова «бэкенд»: ' + text);
        assert.ok(/выберите/i.test(text), 'нет призыва выбрать бэкенд: ' + text);
        assert.strictEqual(RP.errorInfo({ status: 400, body: { error: 'backend_required' } }).code,
            'backend_required');
    });

    await check('ошибка 503 store_unavailable: «хранилище не инициализировано» + куда смотреть', function () {
        const info = RP.errorInfo({ status: 503, body: { error: 'store_unavailable' } });
        assert.ok(RU_FALLBACK.store_unavailable.test(info.title), 'нет внятной причины: ' + info.title);
        assert.ok(/LB_MODEL_REFERENCE_PATH/.test(info.hint), 'нет подсказки, что проверять: ' + info.hint);
    });

    await check('ошибка 409 revision_conflict: «перечитайте», с числами current/sent', function () {
        const info = RP.errorInfo({ status: 409, body: { error: 'revision_conflict', current: 5, sent: 3 } });
        assert.ok(RU_FALLBACK.revision_conflict.test(info.title), 'нет требования перечитать: ' + info.title);
        assert.deepStrictEqual(info.details, ['current=5', 'sent=3'],
            'оператор должен видеть, какая ревизия у него и какая на сервере');
    });

    await check('ошибка 422 profile_does_not_fit_memory: показывается вердикт памяти', function () {
        const info = RP.errorInfo({
            status: 422,
            body: {
                error: 'profile_does_not_fit_memory',
                message: 'не хватает VRAM: нужно 9100 МБ, свободно 6959 МБ',
                memory: { stage: 'vram', freeMiB: 6959, needMiB: 9100 }
            }
        });
        assert.ok(RU_FALLBACK.not_feasible.test(info.title), 'нет заголовка про память: ' + info.title);
        assert.ok(/не хватает VRAM/.test(info.hint), 'вердикт памяти не показан: ' + info.hint);
        assert.ok(info.details.indexOf('stage=vram') !== -1, 'этап проверки памяти не показан: ' + info.details.join(','));
        assert.ok(info.details.indexOf('freeMiB=6959') !== -1, 'числа вердикта не показаны');
    });

    await check('ошибка 404: «возможно, её удалили», а не код', function () {
        const info = RP.errorInfo({ status: 404, body: { error: 'template_not_found' } });
        assert.ok(RU_FALLBACK.not_found.test(info.title), 'нет человеческого текста: ' + info.title);
    });

    await check('ошибки контракта с готовым message показывают message, а не код', function () {
        const info = RP.errorInfo({
            status: 400, body: { error: 'invalid_value', field: 'parallel', message: 'parallel must be in [0, 8], got 12' }
        });
        assert.ok(/parallel must be in/.test(info.hint), 'текст сервера потерян: ' + info.hint);
        assert.ok(info.details.indexOf('field=parallel') !== -1, 'поле нарушения не показано');
    });

    await check('сетевая ошибка/таймаут не превращаются в «что-то пошло не так»', function () {
        const timeout = RP.errorInfo({ code: 'timeout', message: 'Таймаут чтения (20 с): GET /api/v1/reference/templates не ответил.' });
        assert.ok(/не ответил/i.test(timeout.title), 'нет причины таймаута: ' + timeout.title);
        const network = RP.errorInfo({ status: 0, message: 'Network error: Failed to fetch' });
        assert.ok(/запрос/i.test(network.title) && /Failed to fetch/.test(network.hint),
            'сетевая ошибка должна объяснять причину: ' + network.title + ' / ' + network.hint);
    });

    // =======================================================================
    // 5. Формат diff и предпросмотр
    // =======================================================================

    await check('diff: «contextLength: 16384 → 32768», отсутствующее значение — «—»', function () {
        assert.strictEqual(RP.formatDiffRow({ field: 'contextLength', current: 16384, next: 32768 }),
            'contextLength: 16384 → 32768');
        assert.strictEqual(RP.formatDiffRow({ field: 'parallel', current: null, next: 1 }),
            'parallel: — → 1');
        assert.strictEqual(RP.formatDiffValue(true), 'да');
        const html = RP.renderDiff([{ field: 'kvCacheType', current: 'f16', next: 'q8_0' }]);
        assert.ok(/kvCacheType: f16 → q8_0/.test(html), 'diff не отрисован: ' + html);
        assert.ok(/Отличий от действующего профиля нет/.test(RP.renderDiff([])),
            'пустой diff должен говорить «отличий нет», а не показывать пустую таблицу');
    });

    await check('предпросмотр: diff + нарушения + вердикт памяти + цели', function () {
        const html = RP.renderPreview({
            ok: false,
            model: 'Qwen3-4B-Instruct-2507-Q4_K_M',
            valid: { ok: false, violations: [{ field: 'parallel', message: 'parallel must be in [0, 8], got 12' }] },
            diff: [{ field: 'contextLength', current: 16384, next: 32768 }],
            memory: { checked: true, feasible: false, degraded: false, stage: 'vram', message: 'не хватает VRAM' },
            targets: [
                { backendId: 'cppworker-gpu', loaded: true, contextLength: 16384, activeQueries: 0 },
                { backendId: 'cppworker-cyborg', loaded: false }
            ],
            warnings: ['профиля этой модели ещё нет']
        });
        assert.ok(/contextLength: 16384 → 32768/.test(html), 'нет diff');
        assert.ok(/parallel must be in \[0, 8\]/.test(html), 'нет нарушения');
        assert.ok(/НЕ помещается/.test(html), 'нет вердикта памяти');
        assert.ok(/не хватает VRAM/.test(html), 'нет сообщения вердикта памяти');
        assert.ok(/cppworker-gpu/.test(html) && /cppworker-cyborg/.test(html), 'нет списка целей');
        assert.ok(/профиля этой модели ещё нет/.test(html), 'нет предупреждений');
        assert.ok(/есть нарушения/.test(html), 'нет итогового вердикта предпросмотра');
    });

    await check('вердикт памяти: «проверить не удалось» — это не «помещается»', function () {
        const html = RP.renderMemory({ checked: false, feasible: false, message: '' });
        assert.ok(/проверить не удалось/.test(html), 'непроверенный вердикт обязан называться честно: ' + html);
    });

    // =======================================================================
    // 6. Список «где загружена модель»
    // =======================================================================

    await check('цели берутся из cluster/models/loaded: регистр и .gguf не создают второй модели', function () {
        const loaded = [
            { name: 'Qwen3-4B-Instruct-2507-Q4_K_M', backendId: 'cppworker-gpu' },
            { name: 'qwen3-4b-instruct-2507-q4_k_m.gguf', backendId: 'cppworker-cyborg' },
            { name: 'gemma-4-E4B-it-Q4_K_M', backendId: 'cppworker-gpu' }
        ];
        const targets = RP.loadedTargetsFor(loaded, 'Qwen3-4B-Instruct-2507-Q4_K_M');
        assert.deepStrictEqual(targets.map((x) => x.backendId), ['cppworker-gpu', 'cppworker-cyborg']);
        assert.strictEqual(targets[0].loaded, true);
        assert.deepStrictEqual(RP.loadedTargetsFor(loaded, 'нет-такой'), [],
            'чужой модели в целях быть не должно');
        assert.deepStrictEqual(RP.loadedTargetsFor(loaded, ''), [], 'без модели целей нет');
    });

    // =======================================================================
    // 7. Редактор: метаданные полей из MODEL_PARAM_SPECS
    // =======================================================================

    await check('редактор берёт тип/границы/варианты из MODEL_PARAM_SPECS (своей таблицы нет)', function () {
        RP._state.editor = {
            isNew: false, id: 'qwen3-4b-8gb-agent', revision: 3,
            title: 'Заголовок', comment: 'Обоснование',
            applies: { modelPattern: 'Qwen3*', backendType: '', quantization: '', vramMinMB: '', vramMaxMB: '' },
            params: [
                { name: 'contextLength', raw: '32768', unit: 'tokens', comment: '' },
                { name: 'flashAttn', raw: 'true', unit: '', comment: '' },
                { name: 'kvCacheType', raw: 'q8_0', unit: '', comment: '' }
            ],
            sources: [{ doc: 'docs/x.md', section: '§1' }]
        };
        RP.renderEditor();
        const html = getEl('referenceEditor').innerHTML;
        assert.ok(html.indexOf('<option value="contextLength" selected>') !== -1,
            'поле параметра должно быть select из MODEL_PARAM_SPECS');
        assert.ok(html.indexOf('min="256"') !== -1,
            'граница поля пришла не из MODEL_PARAM_SPECS: ' + html.indexOf('min="256"'));
        assert.ok(html.indexOf('max="1048576"') !== -1, 'верхняя граница не из MODEL_PARAM_SPECS');
        assert.ok(html.indexOf('type="number"') !== -1, 'целочисленное поле должно быть number-инпутом');
        assert.ok(/<option value="q8_0" selected>/.test(html), 'enum-поле должно быть select с вариантами из спецификации');
        assert.ok(/да<\/option>/.test(html) && /нет<\/option>/.test(html),
            'bool-поле должно быть select «да/нет»');
        assert.ok(html.indexOf('data-ref-edit="paramComment"') !== -1, 'нет построчного комментария параметра');
        assert.ok(html.indexOf('docs/x.md') !== -1, 'нет источника');
    });

    await check('редактор закрывается и не оставляет разметку', function () {
        RP._state.editor = null;
        RP.renderEditor();
        assert.strictEqual(getEl('referenceEditor').hidden, true);
    });

    // =======================================================================
    // 8. Провайдер обновления и идемпотентное монтирование
    // =======================================================================

    await check('провайдер reference зарегистрирован и обновляет список и цели', async function () {
        assert.ok(providers.reference && providers.reference.length === 1,
            'нет провайдера страницы reference — кнопка/тик в шапке её не обновят');
        const before = { recs: calls.recommendations, tpls: calls.templates, loaded: calls.loaded };
        const result = await providers.reference[0]();
        assert.ok(result, 'провайдер обязан вернуть Promise с результатом');
        assert.strictEqual(calls.recommendations, before.recs + 1, 'список рекомендаций не перечитан');
        assert.strictEqual(calls.templates, before.tpls + 1, 'список шаблонов не перечитан');
        assert.strictEqual(calls.loaded, before.loaded + 1, 'список загруженных моделей не перечитан');
    });

    await check('после загрузки список нарисован, фильтр по модели работает на живом DOM-стабе', async function () {
        await RP.load();
        const html = getEl('referenceRecList').innerHTML;
        assert.ok(html.indexOf('4B Q4 на 8 ГБ VRAM') !== -1, 'список рекомендаций пуст: ' + html.slice(0, 120));
        assert.ok(html.indexOf('Ориентиры 32K') !== -1, 'общая справка пропала из списка');
        assert.ok(getEl('referenceTplList').innerHTML.indexOf('Агент на 8 ГБ VRAM') !== -1,
            'список шаблонов не нарисован');
        assert.ok(getEl('referenceModelFilter').innerHTML.indexOf('Qwen3-4B-Instruct-2507-Q4_K_M') !== -1,
            'в фильтре нет загруженной модели');
    });

    await check('ошибка загрузки показывается человеческим текстом и пробрасывается индикатору', async function () {
        failNext = { status: 503, body: { error: 'store_unavailable' } };
        let rejected = false;
        await RP.load().catch(() => { rejected = true; });
        assert.strictEqual(rejected, true, 'load обязан пробрасывать ошибку: индикатор свежести в шапке должен её показать');
        const status = getEl('referenceStatus');
        assert.strictEqual(status.hidden, false, 'ошибка не показана на странице');
        assert.ok(/хранилище не инициализировано/i.test(status.innerHTML),
            'на странице нет причины: ' + status.innerHTML);
    });

    await check('mount() идемпотентен: тик авто-обновления не пересобирает страницу', async function () {
        const first = RP.mount();
        const second = RP.mount();
        assert.strictEqual(first, true, 'первый mount должен смонтировать страницу');
        assert.strictEqual(second, false, 'повторный mount обязан быть no-op, иначе тик пересоберёт DOM');
        await Promise.resolve();
    });

    // =======================================================================
    // Итог
    // =======================================================================

    console.log('\n' + (failures.length ? 'FAILED: ' + failures.length : 'ИТОГ: все проверки пройдены (' + passed + ')'));
    if (failures.length) {
        failures.forEach((f) => console.error(' - ' + f));
    }
    process.exit(failures.length ? 1 : 0);
})();

// webui/tests/gguf-model-profile-key-r692.test.js — R692 (2026-11-23).
//
// ЖИВОЙ ДЕФЕКТ (замер на стенде: webui:r261120-r691, balancer:r261120-r681,
// вход skyadmin, вкладка GGUF → «Модели»). У КАЖДОЙ модели на диске колонка
// «В профиле» показывала «— не задано —», поля «Задать» были пусты, кнопка
// «Применить и перезагрузить» сохранила бы пустоту. При этом профили есть:
// на стенде 21 профиль, `hasProfile=true` у всех трёх файлов в каталоге.
//
// ПРИЧИНА. Профиль искали ПОЛНЫМ именем файла, а ключи профилей балансера —
// без расширения (`balancer.NormalizeModelProfileKey`,
// internal/balancer/num_ctx_resolver.go:148). Живые пары стенда:
//
//   Qwen3-4B-Instruct-2507-Q4_K_M.gguf        ↔ Qwen3-4B-Instruct-2507-Q4_K_M
//   Qwen3.8-9B-Distill-uncensored-heretic…gguf↔ Qwen3.8-…-heretic.Q4_K_M
//   gemma-4-E4B-it-Q4_K_M.gguf                ↔ gemma-4-E4B-it-Q4_K_M
//
// ЧТО ПРОВЕРЯЕТСЯ (на настоящих модулях, не на копиях правил):
//   1. правило «имя модели → ключ профиля» — зеркало Go-функции балансера
//      (реестр кейсов взят из internal/balancer/model_profile_key_r136_test.go);
//   2. поиск профиля по полному имени файла находит профиль без расширения, а
//      ключ, реально сохранённый С `.gguf` (записи до R136), тоже находится;
//   3. модель без профиля остаётся без профиля: «похожий» ключ (префикс) НЕ
//      подставляется — чужой профиль хуже пустого;
//   4. поведение редактора параметров: после ответа model-profiles ячейки
//      «В профиле» заполнены значениями и «предел окна» нарисован, а у модели
//      без профиля — по-прежнему «— не задано —»;
//   5. чипы карточки загруженной модели (R155) берут намерение тем же правилом.
//
// ТЕСТ НЕ-ВАКУУМНЫЙ: на коде до правки (git show b39f10a2:webui/js/modules/…)
// он КРАСНЕЕТ. Проверено запуском: в `gguf-detail-params.js` до правки профиль
// ищется `profiles[model]` — при имени файла hydration оставляет «— не задано —»,
// и проверки (2) и (4) падают; в модуле нет ни `window.GgufModelKey`, ни
// `profileOf`.
//
// Запуск: node webui/tests/gguf-model-profile-key-r692.test.js
'use strict';

const assert = require('assert');
const path = require('path');

const SRC = path.join(__dirname, '..');

// ── окружение: ровно то, что нужно настоящим модулям (как в браузере) ────────

function stripTags(html) {
    return String(html === null || html === undefined ? '' : html).replace(/<[^>]*>/g, '');
}

function makeLeaf(attrs, tag) {
    const el = {
        tagName: String(tag || 'DIV').toUpperCase(),
        _attrs: Object.assign({}, attrs || {}),
        _html: '',
        className: '',
        textContent: '',
        value: '',
        placeholder: '',
        type: 'text',
        children: [],
        getAttribute: function (n) { return Object.prototype.hasOwnProperty.call(this._attrs, n) ? this._attrs[n] : null; },
        setAttribute: function (n, v) { this._attrs[n] = v; },
        removeAttribute: function (n) { delete this._attrs[n]; },
        appendChild: function (c) { this.children.push(c); return c; },
        insertBefore: function (c) { this.children.push(c); return c; },
        contains: function (x) { return x === el; }
    };
    // Модуль пишет ячейку «В профиле» через innerHTML — фейковый узел обязан
    // отражать текст так же, как настоящий (иначе тест проверял бы свою заглушку).
    Object.defineProperty(el, 'innerHTML', {
        configurable: true,
        get: function () { return el._html; },
        set: function (v) { el._html = String(v === null || v === undefined ? '' : v); el.textContent = stripTags(el._html); }
    });
    return el;
}

/**
 * makeEditor — узел «параметры запуска этой модели» со всеми селекторами,
 * которые читает/пишет настоящий fillModelParamEditor/paintWindowLimitNote.
 */
function makeEditor(model) {
    const editor = makeLeaf({ 'data-model': model }, 'DIV');
    editor._model = model;
    editor._cells = {};
    editor._fields = {};
    editor._limit = null;
    editor._hydrated = false;
    editor.getAttribute = function (n) {
        if (n === 'data-model') return model;
        if (n === 'data-hydrated') return editor._hydrated ? '1' : null;
        return null;
    };
    editor.setAttribute = function (n, v) {
        if (n === 'data-hydrated') { editor._hydrated = String(v) === '1'; return; }
        editor._attrs[n] = String(v);
    };
    editor.removeAttribute = function (n) { if (n === 'data-hydrated') { editor._hydrated = false; } };
    editor.querySelector = function (sel) {
        let m = /^\[data-profile-field="([^"]+)"\]$/.exec(sel);
        if (m) {
            if (!editor._cells[m[1]]) editor._cells[m[1]] = makeLeaf({}, 'TD');
            return editor._cells[m[1]];
        }
        m = /^\.gguf-mp-field\[data-field="([^"]+)"\]$/.exec(sel);
        if (m) {
            if (!editor._fields[m[1]]) {
                const f = makeLeaf({ 'data-field': m[1] }, 'INPUT');
                f.value = '';
                f.checked = false;
                f.placeholder = '';
                editor._fields[m[1]] = f;
            }
            return editor._fields[m[1]];
        }
        if (sel === '.gguf-mp-limit') return editor._limit;
        if (sel === '.gguf-mp-actions') return null;
        return null;
    };
    editor.querySelectorAll = function () { return []; };
    editor.appendChild = function (c) {
        if (c && String(c.className || '').indexOf('gguf-mp-limit') !== -1) editor._limit = c;
        editor.children.push(c);
        return c;
    };
    editor.insertBefore = function (c) { return editor.appendChild(c); };
    return editor;
}

const editors = [];

global.window = global;
global.Utils = {
    escapeHtml: function (s) {
        return String(s === null || s === undefined ? '' : s)
            .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
    }
};
global.MonitorApp = { esc: function (s) { return String(s == null ? '' : s); } };
global.I18N = { getLang: function () { return 'ru'; }, t: function (k, f) { return f === undefined ? k : f; } };
global.document = {
    readyState: 'complete',
    activeElement: null,
    getElementById: function () { return null; },
    querySelector: function (sel) {
        const l = document.querySelectorAll(sel);
        return l.length ? l[0] : null;
    },
    querySelectorAll: function (sel) {
        const s = String(sel || '').trim();
        if (s === '.gguf-mp-editor[data-model]' || s === '.gguf-mp-editor[data-model]:not([data-hydrated="1"])') {
            const onlyFresh = s.indexOf(':not(') !== -1;
            return editors.filter(function (e) {
                return !onlyFresh || e.getAttribute('data-hydrated') !== '1';
            });
        }
        return [];
    },
    createElement: function (tag) { return makeLeaf({}, tag); },
    addEventListener: function () {},
    removeEventListener: function () {}
};

// Живое состояние страницы: модули берут его ОДИН раз при загрузке, поэтому
// подменять объект нельзя — только свойства (класс ошибок из R162-теста).
const STATE = {
    selectedBackendId: 'cppworker-gpu-bundled-agent',
    detailPane: 'models',
    registeredBackends: [{ id: 'cppworker-gpu-bundled-agent', backendType: 'llama_cpp', status: 'healthy' }],
    localModels: [],
    loadedModels: [],
    runtimeModels: {},
    diskInventory: {},
    modelProfiles: {},
    modelEffectiveWindow: {},
    nctxCeiling: null,
    loadDefaults: null
};
global.GgufModule = {
    state: STATE,
    _: function (k, f) { return f === undefined ? k : f; },
    showToast: function () {},
    currentBackend: function () { return { id: STATE.selectedBackendId, url: 'http://b1' }; }
};

// Настоящие модули в порядке index.html: helpers (там живёт правило) → таб
// «Модели» → параметры → настройки → отрисовка правой панели.
require(path.join(SRC, 'js', 'modules', 'gguf-renderer-helpers.js'));
require(path.join(SRC, 'js', 'modules', 'gguf-detail-models.js'));
require(path.join(SRC, 'js', 'modules', 'gguf-detail-params.js'));
require(path.join(SRC, 'js', 'modules', 'gguf-detail-settings.js'));
require(path.join(SRC, 'js', 'modules', 'gguf-renderer-detail-render.js'));

const M = global.GgufModule;
const Key = global.GgufModelKey;

// ── живые данные стенда (сняты с r691: /api/v1/cppworker/model-profiles и
//    /api/v1/gguf/backends/…/proxy/api/models/files) ──────────────────────────
const DISK_FILES = [
    'Qwen3-4B-Instruct-2507-Q4_K_M.gguf',
    'Qwen3.8-9B-Distill-uncensored-heretic.Q4_K_M.gguf',
    'gemma-4-E4B-it-Q4_K_M.gguf'
];
const LIVE_PROFILES = {
    // Рядом лежат ДВА профиля, и префикс короче точного ключа — на этом и
    // проверяется «чужой профиль не подставляем».
    'Qwen3-4B-Instruct-2507': { contextLength: 16384 },
    'Qwen3-4B-Instruct-2507-Q4_K_M': { contextLength: 32768, batchSize: 512, parallel: 1, kvCacheType: 'f16', numGpuLayers: -1, enableReasoning: false },
    'Qwen3.8-9B-Distill-uncensored-heretic.Q4_K_M': { contextLength: 32768, batchSize: 512, parallel: 1, kvCacheType: 'f16' },
    'gemma-4-E4B-it-Q4_K_M': { contextLength: 32768, batchSize: 512, kvCacheType: 'q4_0', enableReasoning: true }
};
const LIVE_EFFECTIVE = {
    'Qwen3-4B-Instruct-2507': { effectiveContextLength: 65536, limitedBy: 'env', limitReason: 'профиль' },
    'Qwen3-4B-Instruct-2507-Q4_K_M': { effectiveContextLength: 65536, limitedBy: 'env', limitReason: 'окно модели задано 32768' },
    'gemma-4-E4B-it-Q4_K_M': { effectiveContextLength: 65536, limitedBy: 'env', limitReason: 'окно модели задано 32768' }
};
const LIVE_CEILING = { envCeiling: 65536, limitedBy: 'env', reason: 'потолок авто-роста окна LB_NCTX_RELOAD_MAX_N_CTX=65536' };

let profileCalls = 0;
global.Api = {
    cppworkerModelProfiles: {
        list: function () {
            profileCalls++;
            return Promise.resolve({
                models: LIVE_PROFILES,
                effectiveContext: LIVE_EFFECTIVE,
                nctxCeiling: LIVE_CEILING
            });
        }
    },
    cppworkerLoadDefaults: {
        get: function () { return Promise.resolve({ effective: {}, source: {} }); }
    }
};

// ── каркас проверок ─────────────────────────────────────────────────────────

let passed = 0;
const failures = [];
function check(name, fn) {
    try {
        fn();
        passed++;
        console.log('  \u2713 ' + name);
    } catch (e) {
        failures.push(name + ': ' + (e && e.message));
        console.error('  \u2717 ' + name + ' — ' + (e && e.message));
    }
}
function cellText(editor, field) {
    const cell = editor._cells[field];
    return cell ? String(cell.textContent || '') : null;
}
function fieldValue(editor, field) {
    const f = editor._fields[field];
    return f ? String(f.value || '') : null;
}
const NOT_SET = '— не задано —';

// ── 1. ПРАВИЛО: зеркало balancer.NormalizeModelProfileKey ────────────────────

check('R692: правило нормализации — зеркало Go-функции балансера', function () {
    assert.ok(Key, 'window.GgufModelKey не загружен — правило «имя модели → ключ профиля» не существует');
    // Кейсы взяты из internal/balancer/model_profile_key_r136_test.go (тот же
    // реестр), чтобы webui и сервер не разъехались молча.
    const cases = [
        ['gemma-4-E4B-it-Q4_K_M.gguf', 'gemma-4-E4B-it-Q4_K_M'],
        ['gemma-4-E4B-it-Q4_K_M', 'gemma-4-E4B-it-Q4_K_M'],
        ['/app/models/gemma-4.gguf', 'gemma-4'],
        ['D:\\models\\gemma-4.gguf', 'gemma-4'],
        ['GEMMA-4.GGUF', 'GEMMA-4'],
        ['  gemma-4.gguf  ', 'gemma-4'],
        ['', ''],
        ['gguf', 'gguf']
    ];
    cases.forEach(function (pair) {
        assert.strictEqual(Key.key(pair[0]), pair[1], 'key(' + JSON.stringify(pair[0]) + ')');
    });
    // Регистр НЕ приводится: балансер хранит ключи как есть, и «похожий по
    // регистру» профиль — это отдельный ключ, а не тот же самый.
    assert.notStrictEqual(Key.key('GEMMA-4.GGUF'), 'gemma-4', 'регистр не должен меняться в key()');
    // Никакой обрезки квантования/`-it`: на стенде рядом живут `Qwen3.8-27B` и
    // `Qwen3.8-27B-UD-Q4_K_M` — это РАЗНЫЕ профили.
    assert.strictEqual(Key.key('Qwen3.8-27B-UD-Q4_K_M.gguf'), 'Qwen3.8-27B-UD-Q4_K_M');
});

// ── 2. ПОИСК: полное имя файла ↔ ключ без расширения ────────────────────────

check('R692: поиск по полному имени файла находит профиль без расширения', function () {
    assert.ok(Key, 'window.GgufModelKey не загружен');
    DISK_FILES.forEach(function (file) {
        const hit = Key.find(LIVE_PROFILES, file);
        assert.ok(hit, 'по имени файла ' + file + ' профиль не найден (ключи: ' + Object.keys(LIVE_PROFILES).join(', ') + ')');
        const want = file.slice(0, -'.gguf'.length);
        assert.strictEqual(hit.key, want, 'найден не тот ключ: ' + hit.key);
        assert.ok(hit.value && hit.value.contextLength > 0, 'значение профиля пустое для ' + file);
    });
    // Точный ключ тоже должен работать (загруженная карточка называет модель
    // без расширения) — иначе починка сломала бы прежний путь.
    assert.strictEqual(Key.find(LIVE_PROFILES, 'gemma-4-E4B-it-Q4_K_M').key, 'gemma-4-E4B-it-Q4_K_M');
    // Путь и имя со слэшами — тот же результат.
    assert.strictEqual(Key.find(LIVE_PROFILES, '/app/models/gemma-4-E4B-it-Q4_K_M.gguf').key, 'gemma-4-E4B-it-Q4_K_M');
    assert.strictEqual(Key.find(LIVE_PROFILES, 'D:\\models\\gemma-4-E4B-it-Q4_K_M.gguf').key, 'gemma-4-E4B-it-Q4_K_M');
});

check('R692: ключ, сохранённый С `.gguf` (записи до R136), тоже находится', function () {
    assert.ok(Key, 'window.GgufModelKey не загружен');
    // Так тоже бывает: профиль назван расширением (GetModelProfile балансера
    // специально читает такой legacy-ключ). Обратный случай — имя БЕЗ
    // расширения при ключе С ним — тоже обязан находиться.
    const legacy = { 'legacy-model.gguf': { contextLength: 4096 } };
    const byFile = Key.find(legacy, 'legacy-model.gguf');
    assert.ok(byFile && byFile.key === 'legacy-model.gguf' && byFile.matched === 'exact',
        'профиль с .gguf в ключе не найден по имени файла: ' + JSON.stringify(byFile));
    const byBare = Key.find(legacy, 'legacy-model');
    assert.ok(byBare && byBare.key === 'legacy-model.gguf',
        'профиль с .gguf в ключе не найден по имени модели без расширения: ' + JSON.stringify(byBare));
});

check('R692: модель без профиля — чужой (похожий) профиль НЕ подставляется', function () {
    assert.ok(Key, 'window.GgufModelKey не загружен');
    // Только префиксный профиль `Qwen3-4B-Instruct-2507`, модель — более
    // длинная `…-Q4_K_M`. Поиск «по подстроке» подставил бы 16384 чужого
    // профиля; правило этого не делает.
    const onlyPrefix = { 'Qwen3-4B-Instruct-2507': { contextLength: 16384 } };
    assert.strictEqual(Key.find(onlyPrefix, 'Qwen3-4B-Instruct-2507-Q4_K_M.gguf'), null,
        'подставлен ЧУЖОЙ профиль (префикс совпал по подстроке)');
    assert.strictEqual(Key.find(LIVE_PROFILES, 'some-other-model.gguf'), null,
        'неизвестной модели достался чужой профиль');
    assert.strictEqual(Key.find(LIVE_PROFILES, ''), null, 'пустое имя не должно ничего находить');
    assert.strictEqual(Key.find(null, 'm.gguf'), null, 'без карты — null, а не падение');
    // И обратная сторона: точный профиль для длинного имени всё равно главнее.
    assert.strictEqual(Key.find(LIVE_PROFILES, 'Qwen3-4B-Instruct-2507-Q4_K_M.gguf').value.contextLength, 32768);
});

// ── 3. ПОВЕДЕНИЕ: карточки «Модели» (гидратация редактора) ──────────────────

const editorA = makeEditor('Qwen3-4B-Instruct-2507-Q4_K_M.gguf'); // профиль есть
const editorB = makeEditor('some-other-model.gguf');              // профиля нет
editors.push(editorA, editorB);

(async function runBehavior() {
    M.state.loadDefaults = { effective: { contextLength: 65536 }, source: {} };
    assert.strictEqual(typeof M.hydrateModelParamEditors, 'function',
        'gguf-detail-params.js не экспортирует hydrateModelParamEditors');
    M.hydrateModelParamEditors();
    // Ждём ответ model-profiles (модуль сам решает, что делать с промисом).
    for (let i = 0; i < 40 && profileCalls === 0; i++) {
        await new Promise(function (r) { setTimeout(r, 5); });
    }
    await new Promise(function (r) { setTimeout(r, 20); });

    check('R692: карточка модели на диске показывает значения профиля, а не «— не задано —»', function () {
        const ctx = cellText(editorA, 'contextLength');
        assert.ok(ctx && ctx.indexOf('32768') !== -1,
            '«В профиле» для ' + editorA._model + ' = ' + JSON.stringify(ctx) +
            ' — значения профиля не подтянулись (до правки здесь «— не задано —»)');
        assert.notStrictEqual(ctx, NOT_SET, 'ячейка осталась «не задано»');
        const kv = cellText(editorA, 'kvCacheType');
        assert.ok(kv && kv.indexOf('f16') !== -1, 'kv не из профиля: ' + JSON.stringify(kv));
        const parallel = cellText(editorA, 'parallel');
        assert.ok(parallel && parallel.indexOf('1') !== -1, 'parallel не из профиля: ' + JSON.stringify(parallel));
        // Поле «Задать» тоже заполнено: оператор должен видеть, что применит.
        assert.strictEqual(fieldValue(editorA, 'contextLength'), '32768', 'поле contextLength пустое');
        assert.strictEqual(fieldValue(editorA, 'batchSize'), '512', 'поле batchSize пустое');
    });

    check('R692: «предел окна» нарисован для того же имени файла', function () {
        assert.ok(editorA._limit, 'блок «предел окна» не создан');
        const text = String(editorA._limit.textContent || '');
        assert.ok(/Предел окна:\s*65536/.test(text), 'в блоке нет предела: ' + text);
    });

    check('R692: модель без профиля по-прежнему «— не задано —» (чужой профиль не подставлен)', function () {
        const ctx = cellText(editorB, 'contextLength');
        assert.strictEqual(ctx, NOT_SET, 'модели без профиля досталось значение: ' + JSON.stringify(ctx));
        assert.strictEqual(fieldValue(editorB, 'contextLength'), '', 'поле заполнено чужым значением');
    });

    // ── 4. Статическая обвязка: правило подключено там, где живут потребители ──
    //
    // Класс R668/R669/R674: модуль есть, но не подключён (или подключён после
    // потребителя) — и дефект выглядит как «данных нет». Здесь это проверяется
    // по index.html, а не «на глаз».
    check('R692: index.html грузит helpers (с правилом) ДО модулей-потребителей', function () {
        const fs = require('fs');
        const html = fs.readFileSync(path.join(SRC, 'index.html'), 'utf8');
        const at = function (rel) {
            const i = html.indexOf('js/modules/' + rel);
            assert.ok(i > 0, 'index.html не подключает js/modules/' + rel);
            return i;
        };
        const helpers = at('gguf-renderer-helpers.js');
        assert.ok(helpers < at('gguf-detail-models.js'), 'gguf-renderer-helpers.js должен грузиться ДО gguf-detail-models.js');
        assert.ok(helpers < at('gguf-detail-params.js'), 'gguf-renderer-helpers.js должен грузиться ДО gguf-detail-params.js');
        assert.ok(helpers < at('cppworker-params.js'), 'gguf-renderer-helpers.js должен грузиться ДО cppworker-params.js');
        // Правило объявлено ровно в одном месте: в модуле-владельце stripGGUF.
        const helpersSrc = fs.readFileSync(path.join(SRC, 'js', 'modules', 'gguf-renderer-helpers.js'), 'utf8');
        assert.ok(/window\.GgufModelKey\s*=/.test(helpersSrc),
            'window.GgufModelKey не объявлен в gguf-renderer-helpers.js');
        // И потребители не держат своей копии правила (именно этот распад и был
        // причиной дефекта: две нормализации — починили одну).
        ['gguf-detail-params.js', 'gguf-detail-models.js'].forEach(function (file) {
            const src = fs.readFileSync(path.join(SRC, 'js', 'modules', file), 'utf8');
            assert.ok(src.indexOf('GgufModelKey') !== -1, file + ' не использует общее правило window.GgufModelKey');
        });
    });

    // ── 5. Чипы карточки ЗАГРУЖЕННОЙ модели: намерение — тем же правилом ────
    check('R692: чип ctx загруженной модели берёт окно профиля по имени файла', function () {
        M.state.modelProfiles = { 'gemma-4-E4B-it-Q4_K_M': { contextLength: 8192 } };
        const flags = M.buildLoadedFlags({
            // Воркер перечисляет загруженную модель ИМЕНЕМ ФАЙЛА — на живом
            // стенде окно инстанса (22016) больше профиля (8192), и R155 обязан
            // показать стрелку и оба числа.
            name: 'gemma-4-E4B-it-Q4_K_M.gguf',
            path: '/app/models/gemma-4-E4B-it-Q4_K_M.gguf',
            context_size: 22016,
            gpu_layers: 42,
            n_layers: 42
        }, {});
        const ctxChip = flags.filter(function (f) { return f.label === 'ctx'; })[0];
        assert.ok(ctxChip, 'чип ctx не построен: ' + JSON.stringify(flags.map(function (f) { return f.label; })));
        assert.ok(String(ctxChip.value).indexOf('22016') === 0, 'значение ctx: ' + ctxChip.value);
        assert.ok(String(ctxChip.value).indexOf('↑') !== -1,
            'R155: сравнение с профилем не сработало (нет стрелки) — ' + ctxChip.value);
        assert.ok(String(ctxChip.title).indexOf('8192') !== -1,
            'в подсказке нет значения профиля: ' + ctxChip.title);
        // И модель без профиля не получает «намерения» из воздуха.
        const noProf = M.buildLoadedFlags({ name: 'other.gguf', context_size: 4096 }, {});
        const noChip = noProf.filter(function (f) { return f.label === 'ctx'; })[0];
        assert.ok(noChip && String(noChip.value).indexOf('↑') < 0,
            'стрелка без профиля — домысел: ' + (noChip && noChip.value));
    });

    console.log('');
    if (failures.length) {
        console.error('Провалов: ' + failures.length);
        failures.forEach(function (f) { console.error('  - ' + f); });
        process.exit(1);
    }
    console.log('Пройдено проверок: ' + passed);
})().catch(function (e) {
    console.error('ТЕСТ УПАЛ ДО ПРОВЕРОК: ' + ((e && e.stack) || e));
    process.exit(1);
});

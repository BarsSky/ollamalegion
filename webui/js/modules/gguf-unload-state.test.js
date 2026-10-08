// gguf-unload-state.test.js — состояние «выгружается» на странице «GGUF модели».
//
// Run: node webui/js/modules/gguf-unload-state.test.js
//
// ЗАЧЕМ (жалоба оператора): «при выгрузке модели желательно показывать, что
// происходит выгрузка — на больших моделях это не быстро и непонятно, какое
// сейчас состояние». Проверяем ровно это:
//
//   1. Нажатие «Выгрузить» СРАЗУ рисует состояние: модель попадает в
//      state.unloadingModels с отметкой времени, запускается секундный таймер.
//   2. Повторное нажатие не запускает вторую выгрузку (cppworker вернул бы
//      «model not found», и оператор увидел бы ошибку там, где всё идёт).
//   3. Отказ (busy/force) и ошибка снимают состояние — «Выгружается…» не должно
//      висеть на живой модели.
//   4. Успех снимает состояние, останавливает таймер и перечитывает данные.
//   5. Рендер панели «Загруженные» для выгружаемой модели показывает
//      «Выгружается…» с таймером и БЕЗ кнопки выгрузки.
//   6. Если cppworker уже убрал модель из реестра (он делает это в начале
//      выгрузки), карточка выгрузки всё равно видна — отдельным блоком.

'use strict';

const assert = require('assert');

// --- mocks: window / GgufModule ---------------------------------------------

global.window = global;
window.Utils = {
    escapeHtml: function (s) { return s === null || s === undefined ? '' : String(s); },
};
window.I18N = { t: function (k) { return k; } };

const BACKEND = { id: 'cppworker-gpu-bundled-agent', host: 'cppworker-gpu', cppWorkerPort: 18092 };

window.GgufModule = {
    state: {
        selectedBackendId: BACKEND.id,
        registeredBackends: [BACKEND],
        // renderBackendsList() без этого флага отдаёт заглушку «загружаю список
        // бэкендов» — в живом UI флаг ставит refreshBackends() после ответа API.
        backendDataLoaded: true,
        detailPane: 'models',
        loadedModels: [{ model: 'qwen3-8b', handle: 'qwen3-8b' }],
        loadingModels: {},
        unloadingModels: {},
        _unloadingTimer: null,
        runtimeModels: {},
        localModels: [],
        activeQueries: {},
    },
    formatFileSize: function (n) { return String(n) + ' B'; },
    // R91: renderAboutPane строит блок «Хост (система)» через hostSystemStats из
    // gguf-renderer-helpers.js. Этот тест про выгрузку, поэтому вместо всего
    // модуля helpers достаточно одной заглушки (нужное поведение самой функции
    // проверяет gguf-host-ram.test.js).
    hostSystemStats: function () { return { known: false }; },
    stripGGUF: function (name) { return name; },
};
const M = window.GgufModule;
M.currentBackend = function () { return BACKEND; };
// Мини-словарь вместо заглушки: проверки текста должны ловить и «ключ не
// переведён» (I18N.t вернул бы сам ключ), и потерю fallback в модуле.
const RU = {
    'gguf.unloading': 'Выгружается…',
    'gguf.unloading_hint': 'Освобождаю память (VRAM). На больших моделях это занимает десятки секунд.',
    'gguf.unload_in_progress': 'Выгрузка этой модели уже идёт',
    'gguf.elapsed': 'Прошло',
    'gguf.model_unloaded': 'Модель выгружена',
    'gguf.unload_model': 'Выгрузить',
    'gguf.load_model': 'Загрузить',
};
M._ = function (key, fallback) { return RU[key] || fallback || key; };
M.showToast = function () {};
M.refreshDetailPane = function () { M._repaints = (M._repaints || 0) + 1; };
M.updateBackendsList = function () { M._listRepaints = (M._listRepaints || 0) + 1; };
M.refreshDetail = function () { M._detailRefreshes = (M._detailRefreshes || 0) + 1; return Promise.resolve(); };
M.refreshBackends = function () { M._backendRefreshes = (M._backendRefreshes || 0) + 1; return Promise.resolve(); };
// Список бэкендов (gguf-renderer-list.js) берёт видимость из state-модуля;
// в тесте достаточно «все зарегистрированные видимы».
M.visibleBackends = function () { return M.state.registeredBackends; };

const toasts = [];
M.showToast = function (msg, kind) { toasts.push({ msg: msg, kind: kind }); };

// Управляемая выгрузка: промис, который тест завершает сам.
let pendingResolve = null;
let unloadCalls = [];
global.GgufApi = {
    unloadModel: function (backendId, handle, options) {
        unloadCalls.push({ backendId: backendId, handle: handle, options: options });
        return new Promise(function (resolve) { pendingResolve = resolve; });
    },
};

require('./gguf-renderer-actions.js');
require('./gguf-renderer-detail-render.js');
require('./gguf-renderer-list.js');

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
const sleep = function (ms) { return new Promise(function (r) { setTimeout(r, ms); }); };

(async function run() {
    // --- 1. состояние появляется сразу и запускает таймер -------------------
    M.unloadOnSelectedBackend('qwen3-8b');
    const list = M.unloadingList(BACKEND.id);
    check('нажатие «Выгрузить» сразу помечает модель как выгружаемую', function () {
        assert.strictEqual(list.length, 1, 'в state.unloadingModels нет записи');
        assert.strictEqual(list[0].handle, 'qwen3-8b');
        assert.ok(list[0].startedAt > 0, 'нет отметки времени начала выгрузки');
        assert.strictEqual(M.isUnloading(BACKEND.id, 'qwen3-8b'), true);
    });
    check('запускается секундный таймер перерисовки (счётчик времени растёт)', function () {
        assert.ok(M.state._unloadingTimer, 'таймер выгрузки не запущен');
    });
    check('панель перерисована сразу, без ожидания ответа сервера', function () {
        assert.ok((M._repaints || 0) >= 1, 'refreshDetailPane не вызван');
        assert.ok((M._listRepaints || 0) >= 1, 'updateBackendsList не вызван');
    });

    // --- 5. рендер: карточка модели показывает выгрузку ---------------------
    check('рендер «Загруженные»: «Выгружается…» с таймером и без кнопки выгрузки', function () {
        const html = M.renderLoadedPane();
        assert.ok(html.indexOf('Выгружается') !== -1, 'нет подписи о выгрузке: ' + html.slice(0, 400));
        assert.ok(html.indexOf('Прошло') !== -1, 'нет таймера выгрузки');
        assert.strictEqual(html.indexOf('gguf-unload-btn'), -1,
            'кнопка выгрузки осталась активной во время выгрузки (повторный клик вернул бы «model not found»)');
    });

    // --- 2. повторное нажатие не запускает вторую выгрузку ------------------
    unloadCalls = [];
    toasts.length = 0;
    M.unloadOnSelectedBackend('qwen3-8b');
    check('повторное нажатие: второго запроса нет, есть пояснение', function () {
        assert.strictEqual(unloadCalls.length, 0, 'ушёл повторный запрос на выгрузку');
        assert.ok(toasts.some(function (t) { return t.msg.indexOf('уже идёт') !== -1; }),
            'нет пояснения, что выгрузка уже идёт: ' + JSON.stringify(toasts));
    });

    // --- 4. успех: состояние снимается, таймер остановлен -------------------
    pendingResolve({ success: true });
    await sleep(10);
    check('успех: состояние выгрузки снято, таймер остановлен, данные перечитаны', function () {
        assert.strictEqual(M.unloadingList(BACKEND.id).length, 0, 'запись о выгрузке осталась');
        assert.strictEqual(M.state._unloadingTimer, null, 'таймер выгрузки не остановлен');
        assert.ok((M._detailRefreshes || 0) >= 1, 'refreshDetail не вызван после выгрузки');
        assert.ok((M._backendRefreshes || 0) >= 1, 'refreshBackends не вызван после выгрузки');
        assert.ok(toasts.some(function (t) { return t.msg.indexOf('Модель выгружена') !== -1; }), 'нет тоста об успехе');
    });

    // --- 3a. отказ busy без подтверждения: состояние снимается --------------
    M.unloadOnSelectedBackend('qwen3-8b');
    pendingResolve({ success: false, busy: true, status: 409 });
    global.confirm = function () { return false; };
    await sleep(10);
    check('отказ «занята» + отказ оператора: состояние снято, повторного запроса нет', function () {
        assert.strictEqual(M.unloadingList(BACKEND.id).length, 0,
            '«Выгружается…» осталось висеть на живой модели');
        assert.strictEqual(M.state._unloadingTimer, null, 'таймер не остановлен');
    });

    // --- 3b. busy + подтверждение: повтор с force и снова индикатор ---------
    unloadCalls = [];
    M.unloadOnSelectedBackend('qwen3-8b');
    global.confirm = function () { return true; };
    pendingResolve({ success: false, busy: true, status: 409 });
    await sleep(10);
    check('отказ «занята» + подтверждение: повтор с force=true и индикатор снова показан', function () {
        assert.strictEqual(unloadCalls.length, 2, 'нет повторного запроса с force');
        assert.deepStrictEqual(unloadCalls[1].options, { force: true });
        assert.strictEqual(M.isUnloading(BACKEND.id, 'qwen3-8b'), true,
            'состояние выгрузки не восстановлено после force-повтора');
    });
    pendingResolve({ success: true });
    await sleep(10);

    // --- 3c. ошибка сети: состояние снимается -------------------------------
    global.GgufApi.unloadModel = function () { return Promise.reject(new Error('network down')); };
    toasts.length = 0;
    M.unloadOnSelectedBackend('qwen3-8b');
    await sleep(10);
    check('ошибка сети: состояние снято, показан текст ошибки', function () {
        assert.strictEqual(M.unloadingList(BACKEND.id).length, 0, 'состояние осталось после ошибки');
        assert.strictEqual(M.state._unloadingTimer, null, 'таймер не остановлен');
        assert.ok(toasts.some(function (t) { return t.msg.indexOf('network down') !== -1; }), 'нет текста ошибки');
    });

    // --- 6. модель уже исчезла из реестра, но выгрузка идёт -----------------
    check('модель убрана сервером из реестра: карточка выгрузки всё равно видна', function () {
        M.state.loadedModels = []; // cppworker удаляет запись В НАЧАЛЕ выгрузки
        M.markUnloading(BACKEND.id, 'qwen3-8b');
        const html = M.renderLoadedPane();
        assert.ok(html.indexOf('Выгружается') !== -1, 'нет карточки выгрузки: ' + html.slice(0, 300));
        assert.ok(html.indexOf('gguf-loaded-unloading-block') !== -1, 'карточка не выделена в отдельный блок');
        M.clearUnloading(BACKEND.id, 'qwen3-8b');
        M.state.loadedModels = [{ model: 'qwen3-8b', handle: 'qwen3-8b' }];
    });

    // --- 7. сайдбар бэкендов тоже показывает выгрузку -----------------------
    check('список бэкендов: строка «Выгружается» с таймером', function () {
        M.markUnloading(BACKEND.id, 'qwen3-8b');
        const html = M.renderBackendsList();
        assert.ok(html.indexOf('gguf-backend-unloading-row') !== -1, 'в сайдбаре нет строки выгрузки');
        assert.ok(html.indexOf('Выгружается') !== -1, 'нет подписи выгрузки в сайдбаре');
        M.clearUnloading(BACKEND.id, 'qwen3-8b');
    });

    // --- 8. без выгрузок панель выглядит как раньше -------------------------
    check('без выгрузок кнопка «Выгрузить» на месте и таймера нет', function () {
        const html = M.renderLoadedPane();
        assert.ok(html.indexOf('gguf-unload-btn') !== -1, 'кнопка выгрузки пропала');
        assert.strictEqual(html.indexOf('gguf-loaded-unloading-block'), -1, 'остался блок выгрузки');
        assert.strictEqual(M.state._unloadingTimer, null, 'таймер остался запущенным');
    });

    // --- 9. «О бэкенде»: подключение клиентов (2026-10-03) ------------------
    // Ключ и эндпоинты теперь и в панели детали GGUF: оператор настраивает
    // клиента, глядя на конкретный бэкенд. Блок рисует общий модуль
    // client-access.js — здесь проверяем только факт его встраивания.
    check('панель «О бэкенде» содержит блок «Подключение клиентов»', function () {
        window.ClientAccess = {
            render: function (b) { return '<div data-client-access-stub="' + (b && b.id) + '"></div>'; },
            mount: function () {},
        };
        try {
            M.state.detailPane = 'about';
            const html = M.renderDetailPane();
            assert.ok(html.indexOf('data-client-access-stub="' + BACKEND.id + '"') !== -1,
                'блок подключения не встроен в «О бэкенде»: ' + html.slice(0, 300));
        } finally {
            delete window.ClientAccess;
        }
    });

    // Секундный таймер выгрузки держит event loop живым — гасим его и выходим
    // явно, иначе `node <тест>` не завершится (и вывод не отдастся в pipe).
    if (M.state._unloadingTimer) { clearInterval(M.state._unloadingTimer); M.state._unloadingTimer = null; }
    console.log('');
    if (failures.length) {
        console.log('ИТОГ: есть провалы (' + failures.length + ')');
        process.exit(1);
    }
    console.log('ИТОГ: все проверки пройдены (' + passed + ')');
    process.exit(0);
})();

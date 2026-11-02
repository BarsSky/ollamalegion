// gguf-loading-state-r395.test.js — R395 (2026-11-04).
//
// ЖИВАЯ ЖАЛОБА ОПЕРАТОРА: «если загружать модель в gguf models WebUI, то
// повисает состояние загрузки модели со счётчиком и остаётся, даже если модель
// уже загружена».
//
// Два дефекта одного симптома:
//   1. markLoadingModel() клал запись в state.loadingModels, и её никто и никогда
//      не удалял: ни успешная загрузка, ни приход фактов с сервера. Панель
//      рисовала «Загрузка… Прошло: Ns», и N рос при каждой перерисовке.
//   2. Балансер на «ещё грузится» отвечает 202 +
//      {success:false, stillLoading:true, pollAfterSec:30} (R262) — это НЕ
//      ошибка, но WebUI звал markLoadFailed и показывал «Load failed:
//      unknown error», оставляя запись навсегда.
//
// Run: node webui/js/modules/gguf-loading-state-r395.test.js

const assert = require('assert');

global.window = global;

const BACKEND = 'cppworker-gpu-bundled-agent';

window.GgufModule = {
    state: {
        selectedBackendId: BACKEND,
        registeredBackends: [{ id: BACKEND, host: 'cppworker-gpu', cppWorkerPort: 18092 }],
        loadingModels: {},
        unloadingModels: {},
        localModels: [],
        loadedModels: [],
        _loadOptionsCustom: {},
        loadOptions: {},
    },
    _: function (k, d) { return d === undefined ? k : d; },
    stripGGUF: function (name) {
        if (!name) return '';
        return (name.length > 5 && name.slice(-5).toLowerCase() === '.gguf') ? name.slice(0, -5) : name;
    },
};

const calls = { toasts: [], manageModel: null };
global.GgufApi = {
    manageModel: function (backendId, operation, modelName, options) {
        calls.manageModel = { backendId: backendId, operation: operation, modelName: modelName, options: options };
        return Promise.resolve({ success: true });
    },
};

require('./gguf-renderer-actions.js');
const M = window.GgufModule;
M.showToast = function (msg, kind) { calls.toasts.push({ msg: msg, kind: kind }); };
// refreshBackends/refreshDetail в этом тесте не нужны (их делает app.js), но
// действия вызывают их через typeof-проверку — поэтому не определяем.

function lastToast() { return calls.toasts[calls.toasts.length - 1]; }
function records() { return (M.state.loadingModels && M.state.loadingModels[BACKEND]) || []; }
function tick() { return new Promise(function (resolve) { setTimeout(resolve, 0); }); }
// backdate — «запись висит уже давно»: именно этот случай обязан сниматься
// по факту с сервера (свежие записи не трогаем, см. PRUNE_FRESH_MS_L395).
function backdate(seconds) {
    records().forEach(function (r) {
        r.loadingStartedAt = new Date(Date.now() - (seconds || 30) * 1000).toISOString();
    });
}

async function main() {
    // --- 1. Повторный клик не плодит вторую запись ------------------------
    M.markLoadingModel(BACKEND, 'Qwen3.8-27B-UD-Q4_K_M.gguf', '/app/models/Qwen3.8-27B-UD-Q4_K_M.gguf');
    M.markLoadingModel(BACKEND, 'Qwen3.8-27B-UD-Q4_K_M.gguf', '/app/models/Qwen3.8-27B-UD-Q4_K_M.gguf');
    assert.strictEqual(records().length, 1,
        'две записи «грузится» на одну модель: ' + JSON.stringify(records()));

    // --- 2. clearLoadingModel сравнивает имена канонически ----------------
    M.markLoadingModel(BACKEND, 'Other-Model.gguf');
    assert.strictEqual(records().length, 2, 'ожидались две разные модели в loading');
    const removed = M.clearLoadingModel(BACKEND, 'Other-Model');
    assert.strictEqual(removed, 1, 'clearLoadingModel не убрал запись по имени без .gguf');
    assert.strictEqual(records().length, 1, 'после очистки должна остаться одна запись');

    // --- 3. pruneLoadingModels: сервер сказал «загружено» -----------------
    M.markLoadingModel(BACKEND, 'NotLoadedYet.gguf');
    backdate(30);
    const pruned = M.pruneLoadingModels(BACKEND, [{ model: 'Qwen3.8-27B-UD-Q4_K_M', state: 'loaded' }]);
    assert.strictEqual(pruned, 1, 'pruneLoadingModels не убрал модель, которую сервер отдаёт загруженной');
    const left = records().map(function (r) { return r.name; });
    assert.deepStrictEqual(left, ['NotLoadedYet.gguf'],
        'prune не должен трогать модели, которые ещё грузятся: ' + JSON.stringify(left));

    // --- 3b. Свежую запись prune не снимает (перезагрузка модели) ---------
    // ПРИЧИНА: при перезагрузке с другими параметрами воркер сначала выгружает
    // старый инстанс, а refreshBackends вызывается прямо из markLoadingModel —
    // без предела бейдж исчезал бы сразу после нажатия на загруженную модель.
    M.markLoadingModel(BACKEND, 'Reloading.gguf');
    const freshPrune = M.pruneLoadingModels(BACKEND, [{ name: 'Reloading' }]);
    assert.strictEqual(freshPrune, 0, 'свежую запись снимать нельзя: сервер ещё может отдавать старый инстанс');
    backdate(30);
    assert.strictEqual(M.pruneLoadingModels(BACKEND, [{ name: 'Reloading' }]), 1,
        'повзрослевшую запись prune обязан снять');
    M.clearLoadingModel(BACKEND, 'NotLoadedYet.gguf');
    assert.strictEqual(records().length, 0, 'тестовое состояние должно быть пустым перед шагом 4');

    // --- 4. Успех manageModel убирает запись (главный симптом) -----------
    M.markLoadingModel(BACKEND, 'ModelA.gguf');
    M.loadOnSelectedBackend({ name: 'ModelA.gguf', path: '/app/models/ModelA.gguf' });
    await tick();
    const namesAfterLoad = records().map(function (r) { return r.name; });
    assert.ok(namesAfterLoad.indexOf('ModelA.gguf') === -1,
        'после успешной загрузки запись «грузится» осталась: ' + JSON.stringify(namesAfterLoad) +
        ' — это и был висящий счётчик рядом с загруженной моделью');
    assert.strictEqual(lastToast().kind, 'success', 'успех загрузки должен давать success-тост');

    // --- 5. 202 «ещё грузится» — НЕ ошибка --------------------------------
    calls.manageModel = null;
    global.GgufApi.manageModel = function (backendId, operation, modelName) {
        calls.manageModel = { backendId: backendId, operation: operation, modelName: modelName };
        return Promise.resolve({ success: false, stillLoading: true, pollAfterSec: 5, status: 202 });
    };
    M.loadOnSelectedBackend({ name: 'SlowModel.gguf', path: '/app/models/SlowModel.gguf' });
    await tick();
    const slow = records().filter(function (r) { return r.name === 'SlowModel.gguf'; });
    assert.strictEqual(slow.length, 1, 'запись «грузится» должна остаться: загрузка продолжается');
    assert.strictEqual(slow[0].state, 'loading',
        'stillLoading не должен переводить запись в error (было «Load failed: unknown error»)');
    assert.strictEqual(lastToast().kind, 'info',
        'на 202 ожидался info-тост, получен ' + JSON.stringify(lastToast()));
    assert.ok(M.state._loadWatchR395 && Object.keys(M.state._loadWatchR395).length > 0,
        'после 202 обязано быть включено наблюдение за завершением загрузки');

    // --- 6. Настоящая ошибка остаётся ошибкой ----------------------------
    global.GgufApi.manageModel = function () {
        return Promise.resolve({ success: false, error: 'auto-load failed: no such file' });
    };
    M.loadOnSelectedBackend({ name: 'BrokenModel.gguf' });
    await tick();
    const broken = records().filter(function (r) { return r.name === 'BrokenModel.gguf'; });
    assert.strictEqual(broken.length, 1, 'запись об ошибке нужна оператору');
    assert.strictEqual(broken[0].state, 'error', 'настоящая ошибка обязана остаться error');
    assert.ok(broken[0].finishedAt, 'момент ошибки должен быть зафиксирован');
    assert.strictEqual(lastToast().kind, 'error', 'ошибка обязана быть error-тостом');

    // --- 7. Наблюдение за «ещё грузится» останавливается само -------------
    // Сервер догрузил модель: prune убирает запись, таймер на следующем тике
    // видит, что записи нет, и не перепланируется.
    backdate(30);
    M.pruneLoadingModels(BACKEND, [{ name: 'SlowModel' }]);
    const slowLeft = records().filter(function (r) { return r.name === 'SlowModel.gguf'; });
    assert.strictEqual(slowLeft.length, 0, 'запись догруженной модели должна быть снята серверным фактом');
    const watchKeys = Object.keys(M.state._loadWatchR395 || {});
    assert.strictEqual(watchKeys.length, 1, 'ожидался один активный наблюдатель');
    const pendingTimer = M.state._loadWatchR395[watchKeys[0]];
    if (pendingTimer && typeof pendingTimer.unref === 'function') pendingTimer.unref();

    console.log('gguf-loading-state-r395.test.js: OK');
}

main().catch(function (err) {
    console.error('gguf-loading-state-r395.test.js: FAIL');
    console.error(err && err.stack ? err.stack : err);
    process.exitCode = 1;
});

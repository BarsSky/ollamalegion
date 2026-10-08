// app-listeners-config-cache.test.js — R88 (2026-10-08).
//
// Запуск: node webui/tests/app-listeners-config-cache.test.js
//
// ЖИВАЯ ЖАЛОБА: «в какой-то тик есть пометка автотюна, в какой-то нет; происходит
// при обновлении страницы». Разбор: WebSocket-тики (clusterState/legacy) приносят
// список бэкендов БЕЗ конфигурационных полей (autoTune/weight/labels/maxModels —
// они есть только в GET /api/v1/backends), и app-listeners.js → updateBackends
// подменял ими обогащённый REST-список. Карточка AutoTune исчезала до следующего
// REST-тика и появлялась снова — мигание.
//
// Правка: updateBackends обязан применять конфигурацию из кэша
// (window.App.applyBackendConfig) к ЛЮБОМУ источнику. Тест фиксирует проводку:
// hook вызывается, его результат попадает в состояние, а при отсутствии hook
// ничего не падает (обратная совместимость со старой сборкой).
'use strict';

const assert = require('assert');
const path = require('path');

// --- минимальные глобалы, нужные app-listeners.js на этапе загрузки ----------
global.window = global;
global.document = {
    getElementById() { return null; },
    querySelectorAll() { return []; },
    addEventListener() {},
};
if (!global.localStorage) {
    global.localStorage = { getItem() { return null; }, setItem() {}, removeItem() {} };
}
// NB: global.navigator в Node 22 — getter-only, подменять его нельзя
// (тот же обход, что в image-test-page.test.js).
global.App = {}; // app-listeners.js сам создаёт window.App и вешает setup*-функции

require(path.join(__dirname, '..', 'js', 'app-listeners.js'));

const App = global.App;
assert.ok(typeof App.updateBackends === 'function', 'App.updateBackends должен быть определён');

let passed = 0;
function check(name, fn) {
    fn();
    passed++;
    console.log('  \u2713 ' + name);
}

function contextWith(backends) {
    return {
        data: { backends: backends || [] },
        currentPage: 'backends', // не dashboard — чтобы не дёргать render
        refreshPage() {},
    };
}

// 1. Hook применяется: WS-пейлоад без autoTune получает его из кэша.
check('updateBackends применяет конфигурацию из кэша к «сырому» WS-списку', function () {
    let hookCalls = 0;
    App.applyBackendConfig = function (list) {
        hookCalls++;
        return list.map(function (b) {
            // Имитация кэша app.js: конфигурационные поля есть только там.
            return Object.assign({}, b, {
                autoTune: { autoTuneEnabled: true, overallSeverity: 'ok', models: [] },
                weight: 1,
            });
        });
    };
    App.context = contextWith([]);

    App.updateBackends([{ id: 'cppworker-gpu-bundled-agent', status: 'healthy', gpu: { usagePercent: 5 } }]);

    assert.strictEqual(hookCalls, 1, 'hook должен вызываться ровно один раз');
    const got = App.context.data.backends[0];
    assert.ok(got.autoTune, 'autoTune из кэша обязан попасть в состояние (иначе карточка AutoTune мигает)');
    assert.strictEqual(got.autoTune.autoTuneEnabled, true);
    assert.strictEqual(got.weight, 1);
});

// 2. Сортировка по id сохранена (её делал updateBackends до правки).
check('updateBackends по-прежнему сортирует бэкенды по id', function () {
    App.applyBackendConfig = function (list) { return list; };
    App.context = contextWith([]);
    App.updateBackends([{ id: 'b-second' }, { id: 'a-first' }]);
    assert.deepStrictEqual(
        App.context.data.backends.map(function (b) { return b.id; }),
        ['a-first', 'b-second']);
});

// 3. Обратная совместимость: без hook (старая сборка app.js) ничего не падает.
check('без App.applyBackendConfig обновление работает как раньше', function () {
    delete App.applyBackendConfig;
    App.context = contextWith([]);
    App.updateBackends([{ id: 'only-one' }]);
    assert.strictEqual(App.context.data.backends.length, 1);
    assert.strictEqual(App.context.data.backends[0].id, 'only-one');
});

// 4. Контекст не готов — тихий выход (как было).
check('без App.context вызов не падает', function () {
    App.applyBackendConfig = function (list) { return list; };
    App.context = null;
    App.updateBackends([{ id: 'x' }]);
    App.context = contextWith([]);
});

console.log('\napp-listeners-config-cache: ' + passed + ' проверок пройдено');
// app-listeners.js на этапе загрузки заводит таймеры/WS-хуки, поэтому процесс
// надо завершить явно — иначе Node ждёт их вечно (тот же приём, что в
// webui/js/modules/image-test-page.test.js).
process.exit(0);

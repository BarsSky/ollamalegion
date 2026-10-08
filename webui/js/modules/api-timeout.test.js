// api-timeout.test.js — R91 (2026-10-08).
//
// Запуск: node webui/js/modules/api-timeout.test.js
//
// ЗАЧЕМ. Живая жалоба оператора: «с большой задержкой стали открываться настройки
// бэкендов… пропала вообще вся информация кроме инфо». Причина — зависший запрос к
// балансеру: у fetch без AbortController таймаута нет вообще, поэтому
//
//   * панель ждала ответа/ошибки бесконечно (спиннер настроек упирался во внешний
//     15-секундный предел, «настройки открываются с большой задержкой»);
//   * страница GGUF снимает флаг «обновление уже идёт» только в then/catch, так
//     что единственный незавершившийся промис НАВСЕГДА отключал обновление
//     выбранного бэкенда — вкладки «Модели»/«Настройки» оставались пустыми
//     до перезагрузки страницы.
//
// Тест фиксирует три свойства таймаута чтения:
//   1. зависший GET/HEAD обрывается и даёт ПОНЯТНУЮ ошибку (code='timeout');
//   2. запись (POST) таймаутом НЕ обрывается — через неё идут длительные операции
//      (применение профиля с reload модели, перенос модели);
//   3. вызывающий может отключить таймаут (timeoutMs: 0) и привести свой signal —
//      тогда мы не отбираем у него управление отменой.

'use strict';

const assert = require('assert');

// Таймаут чтения берётся из WEBUI_CONFIG — задаём 80 мс, иначе тест ждал бы 20 с.
global.window = global;
global.WEBUI_CONFIG = { API_READ_TIMEOUT_MS: 80 };

// Подменяем fetch: «зависший» запрос резолвится только по abort, как реальный
// сокет к неотвечающему серверу.
let fetchCalls = [];
global.fetch = function (url, options) {
    const opts = options || {};
    fetchCalls.push({ url: String(url), method: opts.method || 'GET', hasSignal: !!opts.signal });
    return new Promise(function (resolve, reject) {
        if (opts.signal) {
            if (opts.signal.aborted) {
                const e = new Error('aborted'); e.name = 'AbortError'; reject(e); return;
            }
            opts.signal.addEventListener('abort', function () {
                const e = new Error('The user aborted a request.'); e.name = 'AbortError'; reject(e);
            });
        }
        // Иначе — «висим вечно» и ничего не резолвим.
    });
};

require('./api.js');
const Api = global.Api || global.window.Api;
assert.ok(Api && typeof Api.getBackend === 'function', 'api.js должен экспортировать window.Api');

let checks = 0;
function check(name, fn) {
    fn();
    checks++;
    console.log('  ✓ ' + name);
}

const sleep = (ms) => new Promise(function (r) { setTimeout(r, ms); });

(async function run() {
    // --- 1. Зависший GET обрывается с понятной ошибкой ------------------------
    {
        const started = Date.now();
        let err = null;
        try {
            await Api.getBackend('cppworker-gpu-bundled-agent');
        } catch (e) {
            err = e;
        }
        const elapsed = Date.now() - started;
        check('зависший GET обрывается по таймауту, а не висит вечно', function () {
            assert.ok(err, 'ожидалась ошибка, а промис не завершился');
            assert.strictEqual(err.code, 'timeout', 'ожидался code=timeout, получено: ' + (err && err.code));
            assert.ok(elapsed >= 60 && elapsed < 2000, 'обрыв должен произойти около таймаута, а не через ' + elapsed + ' мс');
        });
        check('текст ошибки объясняет причину и что проверить', function () {
            assert.ok(/Таймаут чтения/.test(err.message), 'в сообщении нет «Таймаут чтения»: ' + err.message);
            assert.ok(/ol-stack-balancer/.test(err.message), 'в сообщении нет подсказки, что проверять: ' + err.message);
            assert.ok(!/user aborted/i.test(err.message), 'сырое «user aborted» не объясняет причину: ' + err.message);
        });
    }

    // --- 2. POST таймаутом не обрывается --------------------------------------
    {
        fetchCalls = [];
        const p = Api.post('/api/v1/cppworker/model-profiles/gemma-4/apply', { nCtx: 32768 });
        let settled = false;
        p.then(function () { settled = true; }, function () { settled = true; });
        await sleep(250); // вчетверо больше таймаута чтения
        check('долгая запись (POST) не обрывается таймаутом чтения', function () {
            assert.strictEqual(settled, false, 'POST был оборван — это сломало бы apply/reload и перенос модели');
            assert.strictEqual(fetchCalls.length, 1);
            assert.strictEqual(fetchCalls[0].hasSignal, false, 'для POST не должно быть нашего signal');
        });
    }

    // --- 3. Явное отключение таймаута и чужой signal --------------------------
    {
        fetchCalls = [];
        const ctrl = new AbortController();
        const p = Api.post('/api/v1/cluster/models/gemma-4/reload', { x: 1 }, { signal: ctrl.signal });
        let settled = false;
        p.then(function () { settled = true; }, function () { settled = true; });
        await sleep(150);
        check('signal вызывающего не подменяется нашим AbortController', function () {
            assert.strictEqual(settled, false);
            assert.strictEqual(fetchCalls[0].hasSignal, true, 'signal вызывающего потерялся');
        });
    }

    console.log('\nOK: ' + checks + ' checks passed');
    process.exit(0);
})();

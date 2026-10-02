// check_api_busy.js — проверка логики Api.backendModelOperation на 409 busy.
//
// Живой дефект (R83/v60): request() бросает исключение на любой !response.ok,
// поэтому {busy:true, retryWithForce:true} из тела 409 не доезжали до UI, и
// страница Models показывала тупиковое «HTTP 409» вместо предложения выгрузить
// модель с force. Здесь api.js выполняется в node с подменённым fetch.
const fs = require('fs');
const path = require('path');

function loadApiWith(fetchImpl) {
    const src = fs.readFileSync(path.join(__dirname, '..', '..', 'webui', 'js', 'modules', 'api.js'), 'utf8');
    const sandbox = {
        window: { WEBUI_CONFIG: { API_BASE: 'http://balancer.test', API_TOKEN: 'test-token' } },
        fetch: fetchImpl,
        console,
        AbortController,
        setTimeout,
        clearTimeout,
        Object,
        JSON,
        Error,
        Promise
    };
    const fn = new Function('window', 'fetch', 'console', 'AbortController', 'setTimeout', 'clearTimeout',
        src + '\n;return Api;');
    return fn(sandbox.window, sandbox.fetch, sandbox.console, sandbox.AbortController, sandbox.setTimeout, sandbox.clearTimeout);
}

const busyBody = { success: false, busy: true, retryWithForce: true, error: "model 'x' is busy with active inference requests" };

(async () => {
    let calls = [];

    // 1. 409 busy: должен вернуться структурный ответ, а не исключение.
    const apiBusy = loadApiWith(async (url, opts) => {
        calls.push({ url, body: opts && opts.body });
        return {
            ok: false, status: 409, statusText: 'Conflict',
            text: async () => JSON.stringify(busyBody)
        };
    });
    let result;
    try {
        result = await apiBusy.backendModelOperation('bk', 'unload', 'gemma-4');
    } catch (e) {
        console.log('FAIL: 409 бросил исключение: ' + e.message);
        process.exit(1);
    }
    if (!result || result.busy !== true || result.retryWithForce !== true || result.success !== false) {
        console.log('FAIL: потеряны признаки busy/retryWithForce: ' + JSON.stringify(result));
        process.exit(1);
    }
    const sent = JSON.parse(calls[0].body);
    if (sent.operation !== 'unload' || sent.modelName !== 'gemma-4') {
        console.log('FAIL: тело запроса неверное: ' + calls[0].body);
        process.exit(1);
    }
    console.log('OK  409 busy -> ' + JSON.stringify(result));

    // 2. force:true должен уезжать в теле запроса как есть.
    const apiForce = loadApiWith(async (url, opts) => {
        calls.push({ url, body: opts && opts.body });
        return { ok: true, status: 200, statusText: 'OK', json: async () => ({ success: true, operation: 'unload' }) };
    });
    calls = [];
    const okResult = await apiForce.backendModelOperation('bk', 'unload', 'gemma-4', { force: true });
    const forcedBody = JSON.parse(calls[0].body);
    if (forcedBody.force !== true) {
        console.log('FAIL: force не уехал в тело: ' + calls[0].body);
        process.exit(1);
    }
    console.log('OK  force:true -> body ' + calls[0].body + ' -> ' + JSON.stringify(okResult));

    // 3. Прочие ошибки (500) по-прежнему бросаются — их обрабатывает .catch().
    const apiErr = loadApiWith(async () => ({
        ok: false, status: 500, statusText: 'Internal Server Error',
        text: async () => JSON.stringify({ error: 'boom' })
    }));
    try {
        await apiErr.backendModelOperation('bk', 'unload', 'gemma-4');
        console.log('FAIL: 500 не бросил исключение');
        process.exit(1);
    } catch (e) {
        console.log('OK  500 -> исключение (status=' + e.status + ')');
    }
})();

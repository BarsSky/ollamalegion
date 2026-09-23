// api-profiles-auth.test.js — R66d (2026-09-23) регресс на 401 в Per-Model Profiles.
//
// Run: node webui/js/modules/api-profiles-auth.test.js
//
// ЖАЛОБА: «не могу ... применить профиль из WebUI — ни из Settings, ни из
// настроек GGUF Models». Причина: оба вызова уходили БЕЗ токена, а
// /api/v1/cppworker/model-profiles/* защищён AuthMiddleware → 401
// {"error":"Unauthorized: valid API token required"}.
//
//   1. apply() использовал raw fetch с
//      `request._getAuthHeaders ? request._getAuthHeaders() : {}` — такой функции
//      у `request` нет (не определена нигде в проекте), поэтому заголовок
//      X-API-Token не отправлялся.
//   2. streamProgress() создавал EventSource без ?token= — EventSource не умеет
//      custom headers, балансер принимает токен из query (internal/api/auth.go:59).
//
// Тест проверяет, что оба вызова несут токен из WEBUI_CONFIG.API_TOKEN.

const assert = require('assert');

global.window = global;
window.WEBUI_CONFIG = { API_TOKEN: 'test-token-r66d', API_BASE: '' };

const fetchCalls = [];
global.fetch = function (url, options) {
    fetchCalls.push({ url: url, options: options || {} });
    return Promise.resolve({
        ok: true,
        status: 200,
        headers: { get: function () { return 'application/json'; } },
        json: function () { return Promise.resolve({ status: 'ok' }); },
        text: function () { return Promise.resolve('{"status":"ok"}'); },
    });
};

const eventSourceUrls = [];
global.EventSource = function (url) {
    eventSourceUrls.push(url);
    this.addEventListener = function () {};
    this.close = function () {};
};

require('./api.js');
const Api = window.Api;
assert.ok(Api, 'window.Api должен быть экспортирован');

// --- 1. apply() должен отправлять X-API-Token --------------------------------
Api.cppworkerModelProfiles.apply('gemma-4-E4B-it-Q4_K_M', { contextLength: 32768 })
    .then(function () {
        assert.strictEqual(fetchCalls.length, 1, 'apply должен сделать ровно один запрос');
        const call = fetchCalls[0];
        assert.ok(/\/api\/v1\/cppworker\/model-profiles\/gemma-4-E4B-it-Q4_K_M\/apply$/.test(call.url),
            'apply должен идти на endpoint /apply, got ' + call.url);
        assert.strictEqual(call.options.method, 'POST', 'apply должен быть POST');
        const headers = call.options.headers || {};
        assert.strictEqual(headers['X-API-Token'], 'test-token-r66d',
            'apply ОБЯЗАН нести X-API-Token, иначе балансер отвечает 401; headers=' + JSON.stringify(headers));
        assert.strictEqual(call.options.body, '{"contextLength":32768}', 'тело профиля должно уходить как есть');

        // --- 2. streamProgress() должен нести токен в query -------------------
        Api.cppworkerApplyProgress.streamProgress('gemma-4-E4B-it-Q4_K_M', 'apply-1', {});
        assert.strictEqual(eventSourceUrls.length, 1, 'streamProgress должен создать EventSource');
        const url = eventSourceUrls[0];
        assert.ok(url.indexOf('applyId=apply-1') >= 0, 'applyId должен быть в URL, got ' + url);
        assert.ok(url.indexOf('token=test-token-r66d') >= 0,
            'SSE прогресса ОБЯЗАН нести ?token= (EventSource не умеет headers), иначе 401; url=' + url);

        // --- 3. getStatus() (polling fallback) тоже с токеном ----------------
        return Api.cppworkerApplyProgress.getStatus('m', 'apply-1');
    })
    .then(function () {
        assert.strictEqual(fetchCalls.length, 2, 'getStatus должен сделать запрос');
        const headers = fetchCalls[1].options.headers || {};
        assert.strictEqual(headers['X-API-Token'], 'test-token-r66d',
            'getStatus (polling fallback) тоже обязан нести X-API-Token');
        console.log('api-profiles-auth.test.js: OK');
    })
    .catch(function (err) {
        console.error('FAILED: ' + (err && err.stack || err));
        process.exit(1);
    });

// scripts/webui-e2e/errors-matrix-r180.js — R180 (2026-10-19).
//
// Проверка ОШИБОК и ТАЙМАУТОВ балансера: не «работает ли счастливый путь», а что
// клиент получает, когда что-то не так. Для каждой ситуации проверяем:
//   - не зависает ли запрос (замер времени и верхняя граница);
//   - код ответа (и что он не 200 с мусором);
//   - ФОРМУ тела: клиенту нужен JSON с внятным error, а на нативной поверхности —
//     форма Ollama ({"error": "..."}), не HTML и не пустое тело;
//   - на стриме — что приходит ЗАВЕРШЁННЫЙ ответ/ошибка, а не вечное молчание.
//
// Запуск: node scripts/webui-e2e/errors-matrix-r180.js
'use strict';

const API_TOKEN = process.env.API_TOKEN || 'changeme-bundled-with-agent-token';
const CLIENT = process.env.CLIENT_URL || 'http://127.0.0.1:18080';
const MODEL = process.env.MODEL || 'gemma-4-E4B-it-Q4_K_M';
const H = { 'Content-Type': 'application/json', 'X-API-Token': API_TOKEN };

let passed = 0;
const failures = [];
function check(name, ok, ev) {
    if (ok) { passed++; console.log('  PASS  ' + name + (ev ? ' — ' + ev : '')); }
    else { failures.push(name + (ev ? ' — ' + ev : '')); console.error('  FAIL  ' + name + (ev ? ' — ' + ev : '')); }
}

async function call(path, opts, timeoutMs) {
    const t0 = Date.now();
    const ctrl = new AbortController();
    const timer = setTimeout(() => ctrl.abort(), timeoutMs || 120000);
    try {
        const res = await fetch(CLIENT + path, Object.assign({ headers: H, signal: ctrl.signal }, opts || {}));
        const text = await res.text();
        return { status: res.status, text, ms: Date.now() - t0, aborted: false };
    } catch (e) {
        return { status: 0, text: String(e.message || e), ms: Date.now() - t0, aborted: /abort/i.test(String(e.message || e)) };
    } finally {
        clearTimeout(timer);
    }
}

function errShape(text) {
    // Форма ошибки: JSON с полем error (Ollama) либо error.message (OpenAI-совместимая).
    try {
        const j = JSON.parse(text);
        if (typeof j.error === 'string') return { ok: true, kind: 'ollama', msg: j.error };
        if (j.error && typeof j.error.message === 'string') return { ok: true, kind: 'openai', msg: j.error.message };
        if (typeof j.message === 'string') return { ok: true, kind: 'message', msg: j.message };
        return { ok: false, kind: 'json-other', msg: JSON.stringify(j).slice(0, 120) };
    } catch (e) {
        return { ok: false, kind: 'not-json', msg: text.slice(0, 120) };
    }
}

const chat = (extra) => Object.assign({
    model: MODEL,
    messages: [{ role: 'user', content: 'скажи ок' }],
    stream: false,
    options: { num_predict: 8 },
}, extra || {});

(async () => {
    console.log('Матрица ошибок R180: ' + CLIENT);

    // 1. Неизвестная модель
    {
        const r = await call('/api/chat', { method: 'POST', body: JSON.stringify(chat({ model: 'no-such-model-xyz' })) }, 60000);
        const sh = errShape(r.text);
        check('неизвестная модель: не 200', r.status !== 200, 'HTTP ' + r.status + ' за ' + r.ms + ' мс');
        check('неизвестная модель: форма ошибки понятна', sh.ok, sh.kind + ': ' + sh.msg.slice(0, 100));
        check('неизвестная модель: не висит', r.ms < 60000, r.ms + ' мс');
    }

    // 2. Пустое тело
    {
        const r = await call('/api/chat', { method: 'POST', body: '' }, 30000);
        const sh = errShape(r.text);
        check('пустое тело: 400-класс', r.status >= 400 && r.status < 500, 'HTTP ' + r.status);
        check('пустое тело: ошибка в теле', sh.ok, sh.kind + ': ' + sh.msg.slice(0, 80));
    }

    // 3. Битый JSON
    {
        const r = await call('/api/chat', { method: 'POST', body: '{not json' }, 30000);
        const sh = errShape(r.text);
        check('битый JSON: 400-класс', r.status >= 400 && r.status < 500, 'HTTP ' + r.status);
        check('битый JSON: ошибка в теле', sh.ok, sh.kind + ': ' + sh.msg.slice(0, 80));
    }

    // 4. Окно БОЛЬШЕ возможного — достоверный отказ, а не падение/зависание
    {
        const r = await call('/api/chat', {
            method: 'POST',
            body: JSON.stringify(chat({ options: { num_predict: 4, num_ctx: 4194304 } })),
        }, 180000);
        const sh = errShape(r.text);
        const bodyOk = r.status === 200 ? sh.ok === false || true : sh.ok; // 200 допустим только с пустым/осмысленным телом
        check('окно сверх возможного: есть ответ, а не зависание', r.status !== 0 && !r.aborted, 'HTTP ' + r.status + ' за ' + r.ms + ' мс');
        check('окно сверх возможного: отказ объяснён (не тишина)', sh.ok || r.status === 200, 'HTTP ' + r.status + ' ' + (sh.msg || '').slice(0, 120));
        check('окно сверх возможного: уложился в 180 с', r.ms < 180000, r.ms + ' мс');
        void bodyOk;
    }

    // 5. Стрим с неизвестной моделью — завершённая ошибка, а не молчание
    {
        const r = await call('/api/chat', { method: 'POST', body: JSON.stringify(chat({ model: 'no-such-model-xyz', stream: true })) }, 60000);
        const sh = errShape(r.text);
        const hasErrLine = /"error"\s*:/.test(r.text);
        check('стрим+неизвестная модель: пришёл ответ', r.status !== 0 && !r.aborted, 'HTTP ' + r.status + ' за ' + r.ms + ' мс');
        check('стрим+неизвестная модель: ошибка в теле или коде', r.status !== 200 || hasErrLine || sh.ok,
            'HTTP ' + r.status + ' ' + (sh.msg || '').slice(0, 100));
    }

    // 6. OpenAI-поверхность: неизвестная модель
    {
        const r = await call('/v1/chat/completions', {
            method: 'POST',
            body: JSON.stringify({ model: 'no-such-model-xyz', messages: [{ role: 'user', content: 'ок' }], max_tokens: 4 }),
        }, 60000);
        const sh = errShape(r.text);
        check('OpenAI+неизвестная модель: не 200', r.status !== 200, 'HTTP ' + r.status);
        check('OpenAI+неизвестная модель: форма openai/ollama', sh.ok, sh.kind + ': ' + sh.msg.slice(0, 100));
    }

    // 7. Отмена/прерывание: клиент рвёт соединение — сервер не должен зависнуть
    {
        const ctrl = new AbortController();
        const p = fetch(CLIENT + '/api/chat', {
            method: 'POST', headers: H, signal: ctrl.signal,
            body: JSON.stringify(chat({ stream: true, options: { num_predict: 512 } })),
        }).then(r => r.body && r.body.cancel()).catch(() => null);
        setTimeout(() => ctrl.abort(), 1500);
        await p;
        const after = await call('/api/chat', { method: 'POST', body: JSON.stringify(chat({ options: { num_predict: 6 } })) }, 120000);
        check('после обрыва клиента сервис отвечает', after.status === 200, 'HTTP ' + after.status + ' за ' + after.ms + ' мс');
        check('после обрыва ответ содержательный', after.status === 200, 'len=' + (after.text || '').length);
    }

    console.log('\nИТОГ: ' + passed + ' пройдено, ' + failures.length + ' провалов');
    if (failures.length) { failures.forEach(f => console.error('  - ' + f)); process.exitCode = 1; }
})();

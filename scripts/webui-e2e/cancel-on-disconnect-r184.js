// scripts/webui-e2e/cancel-on-disconnect-r184.js — R184 (2026-10-19).
//
// НАЙДЕНО ЗАМЕРОМ в матрице ошибок R180: запрос, отправленный сразу после обрыва
// клиента, занимал 100 429 мс. Причина: балансер закрывал только СВОЁ чтение
// upstream, а воркер продолжал генерацию и держал единственный слот.
//
// Этот инструмент измеряет именно эффект, а не факт «200»:
//   1. запускает ДОЛГУЮ стриминговую генерацию и обрывает её через ~1.5 с;
//   2. замеряет, за сколько воркер освободил слот (active_queries вернулся к 0);
//   3. замеряет время обычного запроса сразу после обрыва.
//
// Запуск: node scripts/webui-e2e/cancel-on-disconnect-r184.js
'use strict';

const API_TOKEN = process.env.API_TOKEN || 'changeme-bundled-with-agent-token';
const CLIENT = process.env.CLIENT_URL || 'http://127.0.0.1:18080';
const WORKER = process.env.WORKER_URL || 'http://127.0.0.1:18092';
const MODEL = process.env.MODEL || 'gemma-4-E4B-it-Q4_K_M';
const H = { 'Content-Type': 'application/json', 'X-API-Token': API_TOKEN };
const sleep = ms => new Promise(r => setTimeout(r, ms));

async function activeQueries() {
    try {
        const r = await fetch(WORKER + '/api/models', { headers: { 'X-API-Token': API_TOKEN } });
        const j = await r.json();
        const m = (j.models || []).find(x => x.name === MODEL);
        return m ? Number(m.active_queries || 0) : 0;
    } catch (e) {
        return -1;
    }
}

(async () => {
    console.log('R184: обрыв клиента -> освобождение слота. Модель=' + MODEL);

    const before = await activeQueries();
    console.log('  активных генераций до опыта: ' + before);

    // 1. Долгая стрим-генерация, обрываем через 1.5 с.
    const ctrl = new AbortController();
    const t0 = Date.now();
    const p = fetch(CLIENT + '/api/chat', {
        method: 'POST', headers: H, signal: ctrl.signal,
        body: JSON.stringify({
            model: MODEL,
            messages: [{ role: 'user', content: 'Напиши длинный список чисел от 1 до 200' }],
            stream: true,
            options: { num_predict: 512 },
        }),
    }).then(r => (r.body ? r.body.cancel() : null)).catch(() => null);
    await sleep(1500);
    ctrl.abort();
    await p;
    const abortAt = Date.now() - t0;
    console.log('  клиент оборван через ' + abortAt + ' мс');

    // 2. Сколько времени воркер держит слот после обрыва.
    let freedMs = null;
    const deadline = Date.now() + 120000;
    while (Date.now() < deadline) {
        const n = await activeQueries();
        if (n === 0) { freedMs = Date.now() - t0; break; }
        await sleep(250);
    }
    console.log('  слот освобождён: ' + (freedMs === null ? 'НЕ освободился за 120 с' : 'через ' + freedMs + ' мс после старта'));

    // 3. Обычный запрос сразу после обрыва — главный показатель для клиента.
    const t1 = Date.now();
    let next = { status: 0, text: '' };
    try {
        const r = await fetch(CLIENT + '/api/chat', {
            method: 'POST', headers: H,
            body: JSON.stringify({ model: MODEL, messages: [{ role: 'user', content: 'скажи ок' }], stream: false, options: { num_predict: 8 } }),
        });
        next = { status: r.status, text: await r.text() };
    } catch (e) {
        next = { status: 0, text: String(e.message || e) };
    }
    const nextMs = Date.now() - t1;
    console.log('  следующий запрос: HTTP ' + next.status + ' за ' + nextMs + ' мс');

    const ok = [];
    const bad = [];
    const chk = (name, cond, ev) => {
        (cond ? ok : bad).push(name + (ev ? ' (' + ev + ')' : ''));
        console.log((cond ? '  PASS  ' : '  FAIL  ') + name + (ev ? ' — ' + ev : ''));
    };
    chk('слот освободился после обрыва', freedMs !== null, freedMs === null ? 'не освободился' : freedMs + ' мс');
    chk('освободился быстро (меньше 30 с, а не по концу генерации)', freedMs !== null && freedMs < 30000,
        freedMs === null ? 'нет данных' : freedMs + ' мс');
    chk('следующий запрос не ждёт чужую генерацию', next.status === 200 && nextMs < 30000, 'HTTP ' + next.status + ' за ' + nextMs + ' мс');

    console.log('\nИТОГ: ' + ok.length + ' пройдено, ' + bad.length + ' провалов');
    if (bad.length) { bad.forEach(f => console.error('  - ' + f)); process.exitCode = 1; }
})();

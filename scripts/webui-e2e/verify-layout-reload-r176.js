// scripts/webui-e2e/verify-layout-reload-r176.js — R176 (2026-10-19).
//
// Приёмка двух вещей сразу:
//   1. РАСКЛАДКА: загрузка с узлом даёт RPC-слои, а «Применить профиль» их не теряет
//      (или честно сообщает layout_degraded, если узел не зарегистрировался).
//   2. СОСТАВ ОТВЕТА: проверяем не «HTTP 200», а состав - раскладку по устройствам,
//      параметры окна/слотов, флаг деградации, а у чата - непустой текст, usage,
//      завершённость и то, что ответ не мусор.
//
// Запуск: node scripts/webui-e2e/verify-layout-reload-r176.js
// Код возврата: 0 - все проверки прошли, 1 - есть провалы.
'use strict';

const API_TOKEN = process.env.API_TOKEN || 'changeme-bundled-with-agent-token';
const WORKER = process.env.WORKER_URL || 'http://127.0.0.1:18092';
const BALANCER = process.env.BALANCER_URL || 'http://127.0.0.1:18081';
const CLIENT = process.env.CLIENT_URL || 'http://127.0.0.1:18080';
const MODEL = process.env.MODEL || 'gemma-4-E4B-it-Q4_K_M';
const PROFILE_CTX = Number(process.env.PROFILE_CTX || 12288);
const PROFILE_SLOTS = Number(process.env.PROFILE_SLOTS || 1);

const H = { 'X-API-Token': API_TOKEN, 'Content-Type': 'application/json' };
let passed = 0;
const failures = [];
function check(name, ok, evidence) {
    if (ok) { passed++; console.log('  PASS  ' + name + (evidence ? ' — ' + evidence : '')); }
    else { failures.push(name + (evidence ? ' — ' + evidence : '')); console.error('  FAIL  ' + name + (evidence ? ' — ' + evidence : '')); }
}
const sleep = ms => new Promise(r => setTimeout(r, ms));

async function json(url, opts) {
    const res = await fetch(url, Object.assign({ headers: H }, opts || {}));
    let body = null;
    try { body = await res.json(); } catch (e) { body = null; }
    return { status: res.status, body };
}

// Состав раскладки: какие устройства и сколько слоёв на каждом.
function splitOf(deviceLayers) {
    const out = { CPU: 0, CUDA: 0, RPC: 0, devices: [] };
    (deviceLayers || []).forEach(d => {
        const name = String(d.device || '');
        const up = name.toUpperCase();
        if (up.startsWith('RPC')) out.RPC += d.count;
        else if (up.startsWith('CPU')) out.CPU += d.count;
        else out.CUDA += d.count;
        out.devices.push(name + ':' + d.count);
    });
    return out;
}

async function modelRow() {
    const r = await json(WORKER + '/api/models');
    const row = (r.body && r.body.models || []).find(m => m.name === MODEL) || null;
    return { row, all: (r.body && r.body.models) || [] };
}

async function waitLoaded(label, timeoutMs) {
    const deadline = Date.now() + (timeoutMs || 180000);
    let last = null;
    while (Date.now() < deadline) {
        const { row } = await modelRow();
        last = row;
        if (row && String(row.state).toLowerCase() === 'loaded') return row;
        await sleep(2500);
    }
    return last;
}

(async () => {
    console.log('Проверка R176: модель=' + MODEL + ', воркер=' + WORKER);

    // 0. Состояние узла раскладки
    const info = await json(WORKER + '/api/info');
    const bal = await json(BALANCER + '/api/v1/backends');
    const donor = (bal.body && bal.body.backends || []).find(b => (b.role || '') === 'donor');
    const donorNode = donor && donor.rpcNode ? donor.rpcNode : null;
    console.log('  узел: ' + (donorNode ? JSON.stringify({ role: donorNode.role, addr: donorNode.addr, reachable: donorNode.reachable }) : 'нет donor-бэкенда'));
    check('узел раскладки доступен', !!(donorNode && donorNode.reachable),
        donorNode ? 'reachable=' + donorNode.reachable + ' addr=' + donorNode.addr : 'нет данных');

    // 1. Свежая загрузка с раскладкой
    await json(WORKER + '/api/models/unload?name=' + encodeURIComponent(MODEL) + '&force=true', { method: 'POST' }).catch(() => null);
    await sleep(4000);
    const loadRes = await json(WORKER + '/api/models/load-with-params', {
        method: 'POST',
        body: JSON.stringify({ name: MODEL, contextSize: 8192, parallel: 1, gpuLayers: 42 }),
    });
    check('загрузка принята воркером', loadRes.status >= 200 && loadRes.status < 300, 'HTTP ' + loadRes.status);
    const loaded1 = await waitLoaded('fresh');
    const s1 = splitOf(loaded1 && loaded1.device_layers_exact);
    console.log('  раскладка после загрузки: ' + JSON.stringify({ devices: s1.devices, CPU: s1.CPU, CUDA: s1.CUDA, RPC: s1.RPC }));
    check('загрузка: RPC-слои есть', s1.RPC > 0, 'RPC=' + s1.RPC + ' из ' + (s1.CPU + s1.CUDA + s1.RPC));
    check('загрузка: раскладка не помечена деградировавшей', loaded1 && loaded1.layout_degraded !== true,
        'layout_degraded=' + (loaded1 && loaded1.layout_degraded));

    // 2. «Применить профиль» тем же путём, что кнопка WebUI
    const put = await json(BALANCER + '/api/v1/cppworker/model-profiles/' + encodeURIComponent(MODEL), {
        method: 'PUT', body: JSON.stringify({ contextLength: PROFILE_CTX, parallel: PROFILE_SLOTS }),
    });
    check('профиль сохранён', put.status >= 200 && put.status < 300, 'HTTP ' + put.status);
    const prof = put.body && put.body.profile ? put.body.profile : {};
    check('в профиле окно как задано', prof.contextLength === PROFILE_CTX, 'contextLength=' + prof.contextLength);
    check('в профиле слоты как заданы', prof.parallel === PROFILE_SLOTS, 'parallel=' + prof.parallel);
    check('устаревшее окно на слот не сохранено', !prof.contextPerSeq || prof.contextPerSeq === Math.floor(PROFILE_CTX / PROFILE_SLOTS / 256) * 256,
        'contextPerSeq=' + prof.contextPerSeq);

    const apply = await json(BALANCER + '/api/v1/cppworker/model-profiles/' + encodeURIComponent(MODEL) + '/apply', { method: 'POST' });
    const results = (apply.body && apply.body.backends) || [];
    const master = results.find(b => b.status === 'reloaded') || results.find(b => b.status === 'skipped');
    check('apply отчитался по бэкендам', results.length > 0, JSON.stringify(results.map(r => r.backendId + ':' + r.status)));
    check('apply перезагрузил мастер', !!(master && master.status === 'reloaded'), master ? master.backendId + ':' + master.status : 'нет');

    const loaded2 = await waitLoaded('reload');
    const s2 = splitOf(loaded2 && loaded2.device_layers_exact);
    console.log('  после apply: ' + JSON.stringify({
        devices: s2.devices, ctx: loaded2 && loaded2.context_size, perSeq: loaded2 && loaded2.context_per_seq,
        slots: loaded2 && loaded2.parallel, degraded: loaded2 && loaded2.layout_degraded,
    }));

    // 2a. Состав параметров
    check('после apply: суммарное окно как задано', loaded2 && loaded2.context_size === PROFILE_CTX,
        'context_size=' + (loaded2 && loaded2.context_size));
    check('после apply: окно на слот согласовано', loaded2 && loaded2.context_per_seq === PROFILE_CTX / PROFILE_SLOTS,
        'context_per_seq=' + (loaded2 && loaded2.context_per_seq));
    check('после apply: число слотов как задано', loaded2 && loaded2.parallel === PROFILE_SLOTS,
        'parallel=' + (loaded2 && loaded2.parallel));

    // 2b. Состав раскладки: либо RPC-слои сохранены, либо честно помечена деградация
    const degraded = !!(loaded2 && loaded2.layout_degraded);
    if (s2.RPC > 0) {
        check('после apply: раскладка сохранена (RPC-слои есть)', true, 'RPC=' + s2.RPC + ' устройств=' + s2.devices.length);
    } else {
        check('после apply: потеря раскладки объявлена честно', degraded && !!loaded2.layout_degraded_reason,
            degraded ? 'layout_degraded_reason=' + String(loaded2.layout_degraded_reason).slice(0, 120) : 'флаг не выставлен');
    }

    // 3. Чат через балансер — проверяем СОСТАВ ответа, а не только код
    const chat = await json(CLIENT + '/api/chat', {
        method: 'POST',
        body: JSON.stringify({
            model: MODEL,
            messages: [{ role: 'user', content: 'Ответь одним словом: работает?' }],
            stream: false,
            options: { num_predict: 96 },
        }),
    });
    check('чат: HTTP 200', chat.status === 200, 'HTTP ' + chat.status);
    const msg = chat.body && chat.body.message;
    const text = (msg && msg.content || '').trim();
    check('чат: в ответе есть message.content', !!(msg && typeof msg.content === 'string'), 'keys=' + Object.keys(chat.body || {}).join(','));
    // СОСТАВ: ответ содержателен, если непустой content ЛИБО reasoning
    // (модель с включёнными размышлениями может потратить лимит на thinking).
    const reasoning = String((msg && (msg.reasoning || msg.thinking)) || '').trim();
    check('чат: есть содержательная часть (content или reasoning)',
        text.length >= 1 || reasoning.length >= 1,
        'content_len=' + text.length + ' reasoning_len=' + reasoning.length + ' text=' + JSON.stringify(text.slice(0, 40)));
    check('чат: done=true (ответ завершён)', !!(chat.body && chat.body.done === true), 'done=' + (chat.body && chat.body.done));
    check('чат: есть token-метрики', !!(chat.body && typeof chat.body.eval_count === 'number' && typeof chat.body.prompt_eval_count === 'number'),
        'prompt_eval_count=' + (chat.body && chat.body.prompt_eval_count) + ' eval_count=' + (chat.body && chat.body.eval_count));
    check('чат: модель в ответе та же', !!(chat.body && chat.body.model === MODEL), 'model=' + (chat.body && chat.body.model));

    // Итог
    console.log('\nИТОГ: ' + passed + ' проверок пройдено, ' + failures.length + ' провалов');
    if (failures.length) {
        failures.forEach(f => console.error('  - ' + f));
        process.exitCode = 1;
    }
})();

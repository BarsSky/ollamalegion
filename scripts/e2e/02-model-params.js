#!/usr/bin/env node
/**
 * e2e/02-model-params.js — R134: сквозная проверка выгрузки/загрузки/ПЕРЕзагрузки
 * модели с новыми параметрами — тем же путём, которым это делает WebUI.
 *
 * ЗАЧЕМ. Живая жалоба оператора: «при тестировании механизма перезагрузки модели
 * в бэкенде с новыми параметрами не получилось загрузить, механизм дал сбой,
 * написав что бэкенд недоступен, при этом загрузка пошла и модель загрузилась, а
 * также после пошла перезагрузка с совсем другими параметрами — возможно
 * дефолтными, не теми что указывал в полях».
 *
 * Инструмент отвечает на вопрос «что РЕАЛЬНО применилось»: после каждой операции
 * он читает ФАКТ загруженного инстанса (runtime-конфиг воркера) и сверяет поле за
 * полем с тем, что было задано. Расхождения печатаются таблицей, а не «вроде
 * применилось».
 *
 * ПОЧЕМУ БЕЗ «ПРОСТО ПОДОЖДАТЬ N СЕКУНД». Каждая фаза ждёт СОСТОЯНИЯ (модель
 * loaded/выгружена, apply завершён) с опросом и печатает таймлайн с временем
 * каждой переходной точки — иначе повисание неотличимо от медленной загрузки.
 *
 * Запуск:
 *   node scripts/e2e/02-model-params.js
 *   MODEL=gemma-4-E4B-it-Q4_K_M BACKEND=cppworker-gpu-bundled-agent node scripts/e2e/02-model-params.js
 *
 * Профиль модели на балансере сохраняется до прогона и восстанавливается после —
 * инструмент не оставляет стенд в чужой конфигурации.
 */
'use strict';

const { execSync } = require('child_process');

const BALANCER = process.env.BALANCER_URL || 'http://127.0.0.1:18081';
const WORKER = process.env.WORKER_URL || 'http://127.0.0.1:18092';
const TOKEN = process.env.DSH_API_TOKEN || 'changeme-bundled-with-agent-token';
const MODEL = process.env.MODEL || 'gemma-4-E4B-it-Q4_K_M';
const BACKEND = process.env.BACKEND || 'cppworker-gpu-bundled-agent';
const WORKER_CONTAINER = process.env.WORKER_CONTAINER || 'ol-stack-cppworker-gpu';

const H = { 'X-API-Token': TOKEN, 'Content-Type': 'application/json' };
const t0 = Date.now();
const log = [];
function mark(phase, note) {
    const line = '[+' + String(((Date.now() - t0) / 1000).toFixed(1)).padStart(6) + ' с] ' + phase + ': ' + note;
    console.log(line);
    log.push(line);
}

async function api(method, url, body, timeoutMs) {
    const ctl = new AbortController();
    const timer = setTimeout(function () { ctl.abort(); }, timeoutMs || 600000);
    try {
        const res = await fetch(url, {
            method: method, headers: H,
            body: body === undefined ? undefined : JSON.stringify(body),
            signal: ctl.signal
        });
        const text = await res.text();
        let json = null;
        try { json = text ? JSON.parse(text) : null; } catch (e) { /* не JSON */ }
        return { status: res.status, json: json, text: text };
    } finally {
        clearTimeout(timer);
    }
}

// runtimeFacts — ФАКТ загруженного инстанса (что реально применилось).
async function runtimeFacts() {
    const r = await api('GET', BALANCER + '/api/v1/gguf/backends/' + encodeURIComponent(BACKEND) +
        '/proxy/api/v1/cppworker/config/runtime', undefined, 30000);
    const list = (r.json && r.json.loaded_models) || [];
    const m = list.filter(function (x) { return x.name === MODEL; })[0] || null;
    return m;
}

async function waitLoaded(phase, want, maxSec) {
    // Дедлайн СЧИТАЕТСЯ ОТ НАЧАЛА ЭТОЙ ФАЗЫ, а не от старта инструмента: иначе
    // вторая фаза получает уже истёкший срок и «не дожидается» мгновенно —
    // ровно этот баг был в первой версии инструмента и дал ложные расхождения.
    const started = Date.now();
    const limit = (maxSec || 300) * 1000;
    let last = null;
    while (Date.now() - started < limit) {
        const m = await runtimeFacts().catch(function () { return null; });
        last = m;
        const loaded = !!(m && m.state === 'loaded');
        if (loaded === want) {
            mark(phase, 'состояние=' + (m ? m.state : 'нет модели') + (loaded ? ' (загружена)' : ' (не загружена)') +
                ' за ' + ((Date.now() - started) / 1000).toFixed(1) + ' с');
            return m;
        }
        await new Promise(function (r) { setTimeout(r, 3000); });
    }
    mark(phase, 'НЕ ДОЖДАЛИСЬ состояния ' + (want ? 'loaded' : 'выгружено') + ' за ' + maxSec + ' с' +
        (last ? ' (последнее: ' + last.state + ')' : ' (модель отсутствует в runtime)'));
    return null;
}

// proxyPath — путь к воркеру ЧЕРЕЗ БАЛАНСЕР: тот же маршрут, которым ходит WebUI.
function proxyPath(rest) {
    return BALANCER + '/api/v1/gguf/backends/' + encodeURIComponent(BACKEND) + '/proxy' + rest;
}

// applyProfile — ровно то, что делает кнопка WebUI «Применить и перезагрузить»:
// PUT профиля, затем POST .../apply (синхронный 200 или 202 + прогресс).
async function applyProfile(phase, profile) {
    const put = await api('PUT', BALANCER + '/api/v1/cppworker/model-profiles/' + encodeURIComponent(MODEL), profile, 60000);
    mark(phase, 'PUT профиля -> ' + put.status);
    const started = Date.now();
    // ВАЖНО (сверено с webui/js/modules/api.js, cppworkerModelProfiles.apply):
    // тело apply — САМ ОБЪЕКТ ПРОФИЛЯ, а не обёртка {profile}. Обёртка даёт
    // 400 {"error":"invalid JSON body","message":"json: unknown field \"profile\""}
    // — это первое, на чём споткнулась первая версия инструмента.
    const apply = await api('POST', BALANCER + '/api/v1/cppworker/model-profiles/' + encodeURIComponent(MODEL) + '/apply',
        profile, 900000);
    const secs = ((Date.now() - started) / 1000).toFixed(1);
    if (apply.status === 202) {
        mark(phase, 'apply -> 202 (асинхронно), applyId=' + (apply.json && apply.json.applyId) + ', ответ за ' + secs + ' с');
        const applyId = apply.json && apply.json.applyId;
        const deadline = Date.now() + 900000;
        while (Date.now() < deadline) {
            await new Promise(function (r) { setTimeout(r, 3000); });
            const st = await api('GET', BALANCER + '/api/v1/cppworker/model-profiles/' + encodeURIComponent(MODEL) +
                '/apply/status/' + encodeURIComponent(applyId), undefined, 30000).catch(function () { return null; });
            const s = st && st.json ? (st.json.status || st.json.state) : null;
            if (s && s !== 'in_progress' && s !== 'running' && s !== 'pending') {
                mark(phase, 'apply завершён: ' + s + ' (всего ' + ((Date.now() - started) / 1000).toFixed(1) + ' с)');
                return s;
            }
        }
        mark(phase, 'apply НЕ завершился за 900 с');
        return 'timeout';
    }
    mark(phase, 'apply -> ' + apply.status + ' за ' + secs + ' с' + (apply.json ? ' (' + JSON.stringify(apply.json).slice(0, 160) + ')' : ''));
    return String(apply.status);
}

// Сверка «задано vs фактически применено».
//
// ОСОБЫЙ СЛУЧАЙ: numGpuLayers задаётся НАМЕРЕНИЕМ (-1 = все слои на GPU,
// -2 = авто по VRAM), а факт — это ИТОГОВОЕ число слоёв, которое llama.cpp
// реально положил на карту (адаптация по свободной памяти). Поэтому положительный
// факт при заданном -1/-2 — не расхождение, а адаптация; инструмент обязан
// сказать это словами, иначе оператор будет искать несуществующую ошибку.
const FIELDS = [
    { profile: 'contextLength', fact: 'context_size', label: 'Контекст (суммарный)' },
    { profile: 'parallel', fact: 'parallel', label: 'Слотов (parallel)' },
    { profile: 'kvCacheType', fact: 'kv_cache_type', label: 'Тип KV-кэша' },
    { profile: 'enableReasoning', fact: 'reasoning_enabled', label: 'Размышления' },
    { profile: 'batchSize', fact: 'batch_size', label: 'Батч' },
    { profile: 'numGpuLayers', fact: 'gpu_layers', label: 'Слои GPU', intent: true }
];

function compare(phase, wanted, facts) {
    const rows = [];
    let bad = 0;
    FIELDS.forEach(function (f) {
        if (wanted[f.profile] === undefined) return;
        const want = wanted[f.profile];
        let got = facts ? facts[f.fact] : undefined;
        if (typeof want === 'boolean') got = (got === undefined ? undefined : !!got);
        let ok = String(got) === String(want);
        let note = '';
        if (!ok && f.intent && (want === -1 || want === -2) && typeof got === 'number' && got >= 0) {
            ok = true;
            note = ' (адаптация по VRAM: задано "' + want + '" = ' +
                (want === -1 ? 'все слои' : 'авто') + ', фактически на GPU ' + got + ')';
        }
        if (!ok) bad++;
        rows.push('    ' + (ok ? '✓' : '✗') + ' ' + f.label.padEnd(22) +
            ' задано=' + String(want).padEnd(8) + ' факт=' + String(got) + note);
    });
    console.log('  ' + phase + ': сверка заданного с фактическим');
    rows.forEach(function (r) { console.log(r); });
    if (facts) {
        console.log('    · доп. факт: context_per_seq=' + facts.context_per_seq +
            ' use_mmap=' + facts.use_mmap + ' flash_attn=' + facts.flash_attn);
    } else {
        console.log('    ✗ модель не загружена — сверять нечего');
        bad++;
    }
    return bad;
}

// Хвост лога воркера вокруг фазы — чтобы повисание объяснялось строками, а не догадками.
function workerLogSince(phase, seconds) {
    try {
        const out = execSync('docker logs --since ' + seconds + 's ' + WORKER_CONTAINER + ' 2>&1', { encoding: 'utf8', maxBuffer: 64 * 1024 * 1024 });
        const interesting = out.split('\n').filter(function (l) {
            return /reload|lazy-load|load with new params|already loaded|infrastructure|CUDA error|SIGABRT|abort|slot manager|context n_ctx|model loaded|status":(4\d\d|5\d\d)/.test(l);
        });
        console.log('  ' + phase + ': значимые строки лога воркера за последние ' + seconds + ' с (' + interesting.length + ')');
        interesting.slice(-14).forEach(function (l) { console.log('    | ' + l.slice(0, 220)); });
    } catch (e) {
        console.log('  ' + phase + ': лог воркера недоступен (' + (e && e.message) + ')');
    }
}

const CONFIG_A = {
    contextLength: 8192, parallel: 2, kvCacheType: 'q4_0',
    enableReasoning: true, batchSize: 512, numGpuLayers: -1
};
const CONFIG_B = {
    contextLength: 16384, parallel: 1, kvCacheType: 'q8_0',
    enableReasoning: false, batchSize: 256, numGpuLayers: -1
};

(async function main() {
    console.log('=== E2E параметров модели: ' + MODEL + ' на ' + BACKEND + ' ===');
    let saved = null;
    let problems = 0;

    // 0. Сохраняем текущий профиль, чтобы вернуть стенд как было.
    const before = await api('GET', BALANCER + '/api/v1/cppworker/model-profiles/' + encodeURIComponent(MODEL), undefined, 30000);
    saved = before.json && (before.json.profile || null);
    mark('0. подготовка', 'профиль до прогона: ' + (saved ? JSON.stringify(saved).slice(0, 200) : '(нет)'));

    try {
        // 1. Чистый старт: выгружаем, если загружена (через прокси балансера —
        // тот же путь, которым это делает WebUI).
        await api('POST', proxyPath('/api/models/unload?name=' + encodeURIComponent(MODEL)), undefined, 120000).catch(function () {});
        await waitLoaded('1. выгрузка', false, 60);

        // 2. Конфигурация A.
        mark('2. конфиг A', JSON.stringify(CONFIG_A));
        await applyProfile('2. apply A', CONFIG_A);
        const factsA = await waitLoaded('2. ожидание загрузки A', true, 420);
        problems += compare('2. конфиг A', CONFIG_A, factsA);
        workerLogSince('2. лог A', 300);

        // 3. Конфигурация B — ПЕРЕзагрузка с другими параметрами.
        mark('3. конфиг B', JSON.stringify(CONFIG_B));
        await applyProfile('3. apply B', CONFIG_B);
        const factsB = await waitLoaded('3. ожидание перезагрузки B', true, 420);
        problems += compare('3. конфиг B', CONFIG_B, factsB);
        workerLogSince('3. лог B', 300);

        // 4. Выгрузка.
        const un = await api('POST', proxyPath('/api/models/unload?name=' + encodeURIComponent(MODEL)), undefined, 180000);
        mark('4. выгрузка', 'HTTP ' + un.status);
        await waitLoaded('4. ожидание выгрузки', false, 90);
    } finally {
        // 5. Возвращаем профиль и состояние стенда.
        if (saved) {
            const back = await api('PUT', BALANCER + '/api/v1/cppworker/model-profiles/' + encodeURIComponent(MODEL), saved, 60000);
            mark('5. возврат профиля', 'HTTP ' + back.status);
        }
        const st = await runtimeFacts().catch(function () { return null; });
        mark('5. итог', 'модель ' + (st ? st.state : 'не загружена'));
        console.log('\n' + (problems === 0
            ? 'ИТОГ: все заданные параметры подтверждены фактом загрузки'
            : 'ИТОГ: расхождений — ' + problems + ' (см. сверки выше)'));
        process.exit(problems === 0 ? 0 : 1);
    }
})().catch(function (e) {
    console.error('ОШИБКА инструмента:', e && e.message);
    process.exit(2);
});

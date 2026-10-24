#!/usr/bin/env node
/**
 * params-e2e.js — R136 (2026-10-16): сквозная проверка параметров модели ОТ ЛИЦА
 * WebUI, а не через API в обход интерфейса.
 *
 * ЗАЧЕМ. Живая жалоба оператора: «не получилось перезагрузить модель с новыми
 * параметрами — бэкенд дал сбой, написав что бэкенд недоступен, при этом загрузка
 * пошла и модель загрузилась, а после пошла перезагрузка с совсем другими
 * параметрами». Проверить такое можно только глазами клиента: открыть карточку,
 * выставить значения, нажать «Применить и перезагрузить» и сверить, что
 * ПРИМЕНИЛОСЬ именно то, что выставлено, а процесс не перезапустился.
 *
 * Сценарии:
 *   card        — открыть карточку модели и напечатать обе таблицы (что действует
 *                 сейчас и что лежит в профиле);
 *   reload      — выставить параметры в форме, применить (перезагрузка) и сверить
 *                 КАЖДЫЙ параметр с фактом от воркера + отсутствие перезапуска
 *                 процесса (uptime не сбрасывался);
 *   unload-load — выгрузить и загрузить модель тем же путём;
 *   interrupt   — во время перезагрузки отправить запрос клиента: должен быть
 *                 понятный ответ (а не вечное ожидание), а после перезагрузки —
 *                 успешный ответ на повтор;
 *   ctx-ceiling — запросить окно БОЛЬШЕ, чем модель/бэкенд может дать: клиент
 *                 обязан получить достоверный отказ, а не падение и не зависание.
 *
 * Запуск:
 *   node scripts/webui-e2e/params-e2e.js --scenario card
 *   node scripts/webui-e2e/params-e2e.js --scenario reload --params '{"contextLength":8192,"parallel":1}'
 *   node scripts/webui-e2e/params-e2e.js --scenario interrupt --headed
 *
 * Коды возврата: 0 — всё сошлось, 1 — расхождение/сбой, 2 — ошибка запуска.
 */
'use strict';

const { chromium } = require('playwright');
// R148 (2026-10-16): чистая логика сравнения/разбора — в модуле, покрытом тестами
// (scripts/webui-e2e/tests/params-compare.test.js). Здесь только вызовы.
const pc = require('./lib/params-compare.js');

// ── аргументы ────────────────────────────────────────────────────────────────
function parseArgs(argv) {
    const out = {
        scenario: 'card',
        url: 'http://127.0.0.1:18083',        // WebUI (браузер)
        clientUrl: 'http://127.0.0.1:18080',  // балансер, клиентский порт: /api/chat, /api/models, runtime
        adminUrl: 'http://127.0.0.1:18081',   // балансер, админ: профили, /api/v1/backend...
        workerUrl: 'http://127.0.0.1:18092',  // воркер: uptime и факты загруженного инстанса
        model: 'gemma-4-E4B-it-Q4_K_M'
    };
    for (let i = 2; i < argv.length; i++) {
        const a = argv[i];
        if (a === '--headed') { out.headed = true; continue; }
        if (a === '--keep-open') { out.keepOpen = true; continue; }
        if (a.startsWith('--')) {
            const key = a.slice(2).replace(/-([a-z])/g, (m, c) => c.toUpperCase());
            out[key] = argv[++i];
        }
    }
    out.url = out.url.replace(/\/+$/, '');
    if (typeof out.params === 'string') {
        try { out.params = JSON.parse(out.params); } catch (e) {
            console.error('--params должен быть JSON:', e.message); process.exit(2);
        }
    }
    return out;
}

const args = parseArgs(process.argv);
const LIGHT_PARAMS = {
    contextLength: 8192,
    contextPerSeq: 4096,
    numGpuLayers: -1,
    batchSize: 512,
    parallel: 2,
    kvCacheType: 'q4_0',
    flashAttn: true,
    useMmap: true,
    enableReasoning: true
};
const wanted = args.params || LIGHT_PARAMS;
const deadlineMs = Number(args.timeoutSec || 600) * 1000;

const results = [];
function record(ok, name, detail) {
    results.push({ ok, name, detail: detail || '' });
    console.log(`  ${ok ? 'PASS' : 'FAIL'}  ${name}${detail ? ' — ' + detail : ''}`);
}
function info(msg) { console.log('  · ' + msg); }

// ── API-помощники (через тот же origin, что и браузер) ───────────────────────
let API_TOKEN = '';
async function api(path, opts) {
    const res = await fetch(args.adminUrl + path, {
        method: (opts && opts.method) || 'GET',
        headers: Object.assign({ 'X-API-Token': API_TOKEN, 'Content-Type': 'application/json' },
            (opts && opts.headers) || {}),
        body: opts && opts.body ? JSON.stringify(opts.body) : undefined
    });
    const text = await res.text();
    let json = null;
    try { json = text ? JSON.parse(text) : null; } catch (e) { /* не JSON — вернём текстом */ }
    return { status: res.status, json, text };
}
// ФАКТЫ БЕРЁМ С САМОГО ВОРКЕРА (не через балансер): uptime — детектор перезапуска
// процесса, а /api/models — то, что РЕАЛЬНО применилось у загруженного инстанса.
// Клиентский путь (то, что делает клиент вроде OpenWebUI/Cline): через клиентский
// порт балансера — /api/chat, /api/models, runtime-конфиг.
async function clientApi(path, opts) {
    const res = await fetch(args.clientUrl + path, {
        method: (opts && opts.method) || 'GET',
        headers: { 'X-API-Token': API_TOKEN, 'Content-Type': 'application/json' },
        body: opts && opts.body ? JSON.stringify(opts.body) : undefined
    });
    const text = await res.text();
    let json = null;
    try { json = text ? JSON.parse(text) : null; } catch (e) {}
    return { status: res.status, json, text };
}

// Факты для сверки — из ТОГО ЖЕ источника, что читает карточка:
// /api/v1/cppworker/config/runtime (балансер обогащает ответ воркера).
//
// R148 (2026-10-16): выбор строки и маппинг полей вынесены в модуль
// lib/params-compare.js — там же их покрывают тесты (tests/params-compare.test.js).
// Логика ровно та, что выстрадана живыми дефектами: балансер отдаёт массив в
// `loaded_models` (а не models/data), а пока модель грузится — ПЛЕЙСХОЛДЕР с
// нулями, сравнивать с которым нельзя.
// R149 (2026-10-16): второй аргумент — backendID, к которому относится проверка.
// ЖИВОЙ СЛУЧАЙ: когда одну модель держат две копии (мастер и донор), балансер
// отдаёт в loaded_models ОБЕ строки, и инструмент брал первую попавшуюся — мог
// сравнить факт донора с параметрами, применёнными в карточке мастера, и выдать
// ложный FAIL (contextPerSeq 8192 вместо 4096, parallel 1 вместо 2).
async function runtimeFacts(model, backendID) {
    const r = await clientApi('/api/v1/cppworker/config/runtime');
    const list = (r.json && (r.json.loaded_models || r.json.models || r.json.data)) || [];
    const picked = pc.selectLoadedRow(list, model, backendID);
    if (!picked.row) {
        return { row: null, source: 'runtime:' + r.status + ' (состояние: ' + picked.states + ')' };
    }
    // Подпись источника называет бэкенд и, если копий несколько, перечисляет их:
    // оператор должен видеть, что факт может быть не от той копии, а не гадать.
    const where = (picked.backend ? picked.backend + '/' : '') + picked.states +
        (picked.ambiguous ? '; копий: ' + picked.backends.length + ' [' + picked.backends.join(', ') + ']' : '');
    return {
        source: 'runtime.loaded_models[' + where + ']',
        row: pc.mapRuntimeRow(picked.row),
        backend: picked.backend,
        ambiguous: !!picked.ambiguous,
        backends: picked.backends,
        // R151: копия НЕ того бэкенда, что просили -> сравнение не авторитетно.
        backendMatched: picked.backendMatched !== false,
        wantBackend: backendID || ''
    };
}

// selectedBackendId — бэкенд, ВЫБРАННЫЙ в карточке (то же состояние, что читает
// сценарий dump). R150 (2026-10-16): нужен, чтобы сверять факты с ТОЙ копией
// модели, к которой применялись параметры. Когда одну модель держат мастер и донор,
// балансер отдаёт в runtime обе строки, и без этого инструмент сравнивал чужую
// копию — живой ложный FAIL: contextPerSeq 8192 против 4096, parallel 1 против 2.
async function selectedBackendId(page) {
    if (!page) return null;
    try {
        return await page.evaluate(() => {
            const st = (window.GgufModule && window.GgufModule.state) ? window.GgufModule.state : null;
            return st ? (st.selectedBackendId || null) : null;
        });
    } catch (e) {
        return null;
    }
}

// loadedHandleName — имя ЗАГРУЖЕННОГО ИНСТАНСА (handle) для выгрузки.
//
// R145 (2026-10-16): выгрузка требует имя инстанса, а карточка отдаёт имя ФАЙЛА
// (с .gguf). Живой 404: {"error":"model gemma-4-E4B-it-Q4_K_M.gguf not found"}.
// Это тот же класс, что чинили в WebUI (loadedHandleFor): имя модели на диске и
// имя загруженного инстанса — разные. Берём имя из runtime-записи, а если её нет —
// отбрасываем расширение.
async function loadedHandleName(model) {
    const r = await clientApi('/api/v1/cppworker/config/runtime');
    const list = (r.json && (r.json.loaded_models || r.json.models || r.json.data)) || [];
    const base = String(model).replace(/\.gguf$/, '');
    const row = list.find(m => String(m.name || '').replace(/\.gguf$/, '') === base &&
        String(m.state || '').toLowerCase() === 'loaded');
    if (row && row.name) return String(row.name);
    return base;
}

async function workerApi(path) {
    const res = await fetch(args.workerUrl + path, { headers: { 'X-API-Token': API_TOKEN } });
    const text = await res.text();
    let json = null;
    try { json = text ? JSON.parse(text) : null; } catch (e) {}
    return { status: res.status, json, text };
}
async function workerUptime() {
    const r = await workerApi('/api/v1/cppworker/config');
    return (r.json && r.json.uptime) || null;
}

// R143 (2026-10-16): uptime воркера приходит СТРОКОЙ вида "1h51m9.765022814s",
// а проверка «процесс не перезапускался» сравнивала строки напрямую — то есть
// ЛЕКСИКОГРАФИЧЕСКИ. Живой случай: "1h51m58s" >= "1h51m9s" даёт false, потому что
// '5' < '9', и исправный воркер объявлялся перезапущенным (прошлый прогон
// проходил лишь по совпадению цифр: '7' < '8'). Разбираем в секунды.
const uptimeSeconds = pc.uptimeSeconds; // R148: реализация и тесты — в lib/params-compare.js
async function modelRow(model) {
    const r = await workerApi('/api/models');
    const rows = (r.json && (r.json.models || r.json.data)) || [];
    const base = String(model).replace(/\.gguf$/, '');
    return rows.find(m => String(m.name || '').replace(/\.gguf$/, '') === base) || null;
}
async function waitFor(pred, ms, everyMs) {
    const t0 = Date.now();
    let last = null;
    while (Date.now() - t0 < ms) {
        last = await pred();
        if (last && last.done) return last;
        await new Promise(r => setTimeout(r, everyMs || 1500));
    }
    return last || { done: false, note: 'таймаут ожидания' };
}

// ── UI-помощники ─────────────────────────────────────────────────────────────
async function openCard(page, model) {
    await page.goto(args.url + '/index.html', { waitUntil: 'domcontentloaded' });
    await page.waitForFunction(() => !!document.querySelector('.nav-item[data-page="gguf"]'), null, { timeout: 30000 });
    await page.click('.nav-item[data-page="gguf"]');
    await page.waitForSelector('.gguf-backend-item', { timeout: 30000 });
    // Берём первый llama.cpp-бэкенд (у image-воркеров раскладки и параметров нет).
    const items = await page.$$('.gguf-backend-item');
    for (const it of items) {
        const txt = (await it.innerText()).toLowerCase();
        if (txt.includes('llama') || txt.includes('cppworker')) { await it.click(); break; }
    }
    await page.waitForSelector('.gguf-detail-tab', { timeout: 30000 });
    await page.click('[data-detail-tab="models"]');

    // ИМЯ В СПИСКЕ НА ДИСКЕ И В КАРТОЧКЕ ЗАГРУЖЕННОЙ МОДЕЛИ МОЖЕТ ОТЛИЧАТЬСЯ
    // (`gemma-4-...gguf` против `gemma-4-...`): ищем карточку по имени БЕЗ
    // расширения, а каноническое имя берём из самой формы (data-model) — им же
    // пользуются кнопка «Применить» и API профиля.
    const base = String(model).replace(/\.gguf$/, '');
    await page.waitForSelector('details.gguf-mp-acc', { state: 'attached', timeout: 90000 });
    const found = await page.evaluate((b) => {
        const list = Array.from(document.querySelectorAll('details.gguf-mp-acc'));
        const idx = list.findIndex(d => String(d.getAttribute('data-model-acc') || '').replace(/\.gguf$/, '') === b);
        if (idx < 0) return null;
        const acc = list[idx];
        const ed = acc.querySelector('.gguf-mp-editor');
        if (!ed) return null;
        const key = ed.getAttribute('data-model');
        return { key: key, index: idx, open: acc.open };
    }, base);
    if (!found) throw new Error(`карточка параметров для ${base} не найдена в списке моделей на диске`);
    info(`каноническое имя модели в форме: ${found.key}`);
    // ПРОФИЛЬ ПОДТЯГИВАЕТСЯ НАСТОЯЩИМ КЛИКОМ ПО АККОРДЕОНУ (R162).
    //
    // Раньше аккордеон раскрывался программно (`acc.open = true`), а приложение
    // вешает hydrateOnOpen() на КЛИК по summary — обработчик не срабатывал,
    // профили не запрашивались, и карточка показывала пустые «действующие значения»
    // (а чип «окно больше профиля» не мог сравнить факт с намерением оператора).
    //
    // Почему именно клик, а не прямой вызов M.hydrateOnOpen(): прямой вызов
    // работал, но давал ошибку страницы
    //   Failed to execute 'querySelector' on 'Document': '[object Object]' is not
    //   a valid selector
    // — приложение ожидает событие, а не вызов в обход обработчика. Инструмент
    // обязан ходить путём пользователя.
    const details = page.locator('details.gguf-mp-acc').nth(found.index);
    await details.scrollIntoViewIfNeeded().catch(() => {});
    if (!found.open) {
        await details.locator('summary.gguf-mp-summary').first().click({ timeout: 8000 }).catch(async () => {
            // Клик не прошёл (элемент перекрыт/перерисован) — раскрываем программно
            // и зовём хук приложения: данные те же, просто без события.
            info('клик по аккордеону не прошёл — раскрываю программно и зову hydrateOnOpen');
            await page.evaluate((idx) => {
                const list = document.querySelectorAll('details.gguf-mp-acc');
                const acc = list[idx];
                if (acc && !acc.open) acc.open = true;
                const mod = window.GgufModule;
                if (acc && mod && typeof mod.hydrateOnOpen === 'function') mod.hydrateOnOpen(acc);
            }, found.index);
        });
    }
    // Ждём гидратации профиля (значения из профиля подставляются запросом).
    // ВНИМАНИЕ: editorSel — async (возвращает Promise). Передать её вызов прямо в
    // waitForFunction нельзя: в страницу уедет объект, и приложение упадёт с
    //   Failed to execute 'querySelector' on 'Document': '[object Object]'
    // (это и была ошибка страницы, которую поймал инструмент, когда стал печатать
    // стек к pageerror).
    const editorSelector = await editorSel(found.key);
    await page.waitForFunction(
        sel => { const el = document.querySelector(sel); return el && el.getAttribute('data-hydrated') === '1'; },
        editorSelector, { timeout: 30000 }).catch(() => {});
    await page.waitForTimeout(500);
    return found.key;
}

async function editorSel(model) {
    return `.gguf-mp-editor[data-model="${model}"]`;
}

async function readActiveTable(page, model) {
    const editor = `.gguf-mp-editor[data-model="${model}"]`;
    return page.evaluate(sel => {
        const root = document.querySelector(sel);
        if (!root) return null;
        const rows = Array.from(root.querySelectorAll('.gguf-mp-block')[0].querySelectorAll('tbody tr'));
        return rows.map(tr => {
            const tds = tr.querySelectorAll('td');
            return {
                param: tds[0] ? tds[0].innerText.split('\n')[0].trim() : '',
                value: tds[1] ? tds[1].innerText.trim() : '',
                source: tds[2] ? tds[2].innerText.trim() : ''
            };
        });
    }, editor);
}

async function fillParams(page, model, params) {
    const editor = `.gguf-mp-editor[data-model="${model}"]`;
    // ПАНЕЛЬ ПЕРЕРИСОВЫВАЕТСЯ ЦИКЛОМ ОБНОВЛЕНИЯ (REFRESH_INTERVAL ~5 с), поэтому
    // держать ссылки на элементы нельзя: они становятся detached. Перед каждым
    // полем: раскрываем карточку заново и ищем элемент ПО СЕЛЕКТОРУ в этот момент.
    await page.evaluate(sel => {
        const root = document.querySelector(sel);
        if (!root) return;
        const acc = root.closest('details');
        if (acc) acc.open = true;
    }, editor).catch(() => {});
    for (const [field, value] of Object.entries(params)) {
        const sel = `${editor} [data-field="${field}"]`;
        try {
            const loc = page.locator(sel).first();
            await loc.waitFor({ state: 'visible', timeout: 15000 });
            const tag = await loc.evaluate(e => e.tagName.toLowerCase());
            if (tag === 'select') {
                await loc.selectOption(String(value));
            } else {
                await loc.fill('');
                await loc.fill(String(value));
            }
        } catch (e) {
            record(false, `поле ${field}`, 'не удалось заполнить: ' + String(e.message).split('\n')[0]);
        }
    }
    // Проверяем, что значения реально стоят в форме (панель могла перерисоваться).
    const got = await page.evaluate(([sel, fields]) => {
        const root = document.querySelector(sel);
        if (!root) return null;
        const out = {};
        fields.forEach(f => {
            const el = root.querySelector('[data-field="' + f + '"]');
            if (el) out[f] = el.value;
        });
        return out;
    }, [editor, Object.keys(params)]);
    return got;
}

function factsFrom(row) {
    if (!row) return {};
    const pick = (...keys) => {
        for (const k of keys) if (row[k] !== undefined && row[k] !== null) return row[k];
        return undefined;
    };
    return {
        contextLength: pick('context_size', 'contextSize'),
        contextPerSeq: pick('context_per_seq', 'contextPerSeq'),
        numGpuLayers: pick('gpu_layers', 'gpuLayers'),
        batchSize: pick('batch_size', 'batchSize'),
        parallel: pick('parallel', 'max_slots'),
        kvCacheType: pick('kv_cache_type', 'kvCacheType'),
        flashAttn: pick('flash_attn', 'flashAttn'),
        useMmap: pick('use_mmap', 'useMmap'),
        enableReasoning: pick('reasoningEnabled', 'reasoning_enabled', 'reasoning')
    };
}

const normalize = pc.normalize;         // R148: реализация и тесты — в lib/params-compare.js

// Сравнение «что просили» и «что применилось».
//
// ВАЖНО РАЗЛИЧАТЬ ДВА СЛУЧАЯ (иначе инструмент врёт):
//   * воркер сообщил ДРУГОЕ значение  -> это дефект применения (FAIL);
//   * воркер НЕ СООБЩИЛ параметр      -> это пробел наблюдаемости (WARN), а не
//     доказательство, что параметр не применился.
//
// R143 (2026-10-16): раньше здесь стояло утверждение «часть полей воркер не
// отдаёт (gpu_layers/use_mmap/flash_attn)». Это оказалось НЕВЕРНЫМ: поля есть в
// runtime-конфиге, инструмент читал не тот ключ массива (см. runtimeFacts) и
// молча брал запасной источник. Плюс два поля требуют разбора семантики:
//   * numGpuLayers=-1 в карточке — это «все слои на GPU (авто)», а воркер
//     сообщает ФАКТИЧЕСКОЕ число слоёв; сравнивать их напрямую нельзя, но и
//     молчать нельзя: живой факт — запрошено -1, фактически 19 из 42.
//   * flashAttn — три-состояние (-1 авто / 0 / 1), см. normalize.
const compareWanted = pc.compareWanted; // R148: реализация и тесты — в lib/params-compare.js

// ── сценарии ─────────────────────────────────────────────────────────────────
async function main() {
    const browser = await chromium.launch({ channel: 'msedge', headless: !args.headed });
    const page = await browser.newPage({ viewport: { width: 1600, height: 1000 } });
    const consoleErrors = [];
    page.on('pageerror', e => {
        // R162: стек, а не только текст — иначе причина ошибки страницы не находится.
        const st = String((e && e.stack) || '').split('\n').slice(0, 4).join(' | ');
        consoleErrors.push('pageerror: ' + e.message + (st ? '  << ' + st : ''));
    });
    page.on('console', m => { if (m.type() === 'error') consoleErrors.push('console: ' + m.text()); });

    try {
        await page.goto(args.url + '/index.html', { waitUntil: 'domcontentloaded' });
        API_TOKEN = await page.evaluate(() => {
            const c = window.WEBUI_CONFIG || {};
            return c.API_TOKEN || c.TOKEN || '';
        });
        if (!API_TOKEN) { console.error('нет API_TOKEN в WEBUI_CONFIG — проверьте ' + args.url); process.exit(2); }
        info(`WebUI: ${args.url}, модель: ${args.model}, сценарий: ${args.scenario}`);

        const uptimeBefore = await workerUptime();
        info(`uptime воркера до: ${uptimeBefore}`);

        if (args.scenario === 'dump') {
            await page.goto(args.url + '/index.html', { waitUntil: 'domcontentloaded' });
            await page.click('.nav-item[data-page="gguf"]');
            await page.waitForSelector('.gguf-backend-item', { timeout: 30000 });
            const items = await page.$$eval('.gguf-backend-item', els => els.map(e => ({
                id: e.getAttribute('data-backend-id'), text: e.innerText.replace(/\s+/g, ' ').slice(0, 120)
            })));
            console.log('\n  БЭКЕНДЫ В ПАНЕЛИ:');
            items.forEach(i => console.log(`    ${i.id}  |  ${i.text}`));
            await page.click('.gguf-backend-item');   // выбираем первый (здоровый) бэкенд
            await page.waitForSelector('.gguf-detail-tab', { timeout: 30000 });
            const tabs = await page.$$eval('.gguf-detail-tab', els => els.map(e => e.getAttribute('data-detail-tab')));
            console.log('  ВКЛАДКИ ДЕТАЛИ: ' + tabs.join(', '));
            await page.click('[data-detail-tab="models"]');
            await page.waitForTimeout(5000);
            const pane = await page.evaluate(() => {
                const box = document.querySelector('.gguf-detail-pane, .gguf-detail-content, .gguf-local-list, .gguf-empty-state');
                const details = Array.from(document.querySelectorAll('details.gguf-mp-acc')).map(d => d.getAttribute('data-model-acc'));
                const editors = Array.from(document.querySelectorAll('.gguf-mp-editor')).map(e => e.getAttribute('data-model'));
                const st = (window.GgufModule && window.GgufModule.state) ? window.GgufModule.state : null;
                return {
                    details, editors,
                    selectedBackendId: st ? st.selectedBackendId : null,
                    localModels: st ? (st.localModels || []).map(m => m.name || m.filename) : null,
                    diskInventoryKeys: st ? Object.keys(st.diskInventory || {}) : null,
                    detailPane: st ? st.detailPane : null,
                    paneText: box ? box.innerText.slice(0, 500) : null
                };
            });
            console.log('  details(data-model-acc): ' + JSON.stringify(pane.details));
            console.log('  editors(data-model): ' + JSON.stringify(pane.editors));
            console.log('  selectedBackendId: ' + pane.selectedBackendId + ', detailPane: ' + pane.detailPane);
            console.log('  localModels: ' + JSON.stringify(pane.localModels));
            console.log('  diskInventoryKeys: ' + JSON.stringify(pane.diskInventoryKeys));
            console.log('  текст панели: ' + String(pane.paneText).replace(/\s+/g, ' ').slice(0, 400));
        }

        if (args.scenario === 'card') {
            const key = await openCard(page, args.model);
            const active = await readActiveTable(page, key);
            console.log('\n  ДЕЙСТВУЮЩИЕ ЗНАЧЕНИЯ (из карточки):');
            (active || []).forEach(r => console.log(`    ${r.param.padEnd(34)} ${String(r.value).padEnd(14)} ${r.source}`));

            // R155/R158: чипы карточки (ctx/parallel/kv/flash/mmap/раскладка) с их
            // подсказками. Живут они НЕ в таблице действующих значений, а в табе
            // «loaded» (renderLoadedPane → .gguf-loaded-model-item → .gguf-flag).
            //
            // ЧТО БЫЛО НЕ ТАК (2026-10-17). Инструмент читал чипы, находясь на табе
            // «models», где их в DOM просто нет, — печатал 0 и создавал ложное
            // впечатление, что панель не рендерит чипы. Проверка R155/R158 из-за
            // этого всё время оставалась «написано, но не показано».
            await page.click('[data-detail-tab="loaded"]').catch(() => {});
            await page.waitForSelector('.gguf-loaded-model-item, .gguf-loaded-empty', { timeout: 30000 }).catch(() => {});
            // R162: чип «ctx … ↑» сравнивает факт инстанса с НАМЕРЕНИЕМ оператора,
            // а намерение приходит из профиля модели, который подтягивается
            // отдельным запросом при монтировании форм параметров. Первый рендер
            // панели «loaded» успевает пройти ДО этого — тогда стрелки нет, хотя
            // расхождение есть. Поэтому: ждём, пока профили окажутся в state, и
            // ещё один цикл перерисовки панели (REFRESH_INTERVAL ~5 с).
            await page.waitForFunction(() => {
                const mod = window.GgufModule;
                const st = mod && mod.state;
                return !!(st && st.modelProfiles && Object.keys(st.modelProfiles).length > 0);
            }, null, { timeout: 20000 }).catch(() => {
                info('профили моделей в state не появились за 20 с — чипы покажут факт без намерения');
            });
            await page.waitForTimeout(6000);

            const loaded = await page.$$eval('.gguf-loaded-model-item', els => els.map(el => {
                const nameEl = el.querySelector('.gguf-loaded-model-name');
                const infoEl = el.querySelector('.gguf-loaded-model-info');
                return {
                    name: nameEl ? (nameEl.innerText || '').split('\n')[0].trim() : '',
                    info: infoEl ? (infoEl.innerText || '').replace(/\s+/g, ' ').trim() : '',
                    chips: Array.from(el.querySelectorAll('.gguf-flag')).map(c => ({
                        text: (c.innerText || '').trim(),
                        title: c.getAttribute('title') || '',
                        color: c.style.color || ''
                    }))
                };
            }));

            if (loaded.length === 0) {
                console.log('\n  ЧИПЫ КАРТОЧКИ: загруженных моделей нет — чипов нет по определению.');
                console.log('    (чтобы проверить подписи, поднимите модель: --scenario reload,');
                console.log('     либо POST /api/models/load-with-params с нужными параметрами)');
            }
            loaded.forEach(m => {
                console.log('\n  ЗАГРУЖЕННАЯ МОДЕЛЬ: ' + m.name);
                if (m.info) console.log('    ' + m.info);
                console.log('    ЧИПЫ (' + m.chips.length + '):');
                m.chips.forEach(c => console.log('      ' + c.text +
                    (c.title ? '\n        [' + c.title.slice(0, 220) + ']' : '')));
            });
        }

        if (args.scenario === 'reload' || args.scenario === 'interrupt') {
            // ПОДГОТОВКА СЦЕНЫ (не тестируемая часть): модель должна быть загружена,
            // иначе «применить и перезагрузить» проверяет загрузку, а не перезагрузку.
            const pre = await modelRow(args.model);
            if (!pre || String(pre.state).toLowerCase() !== 'loaded') {
                info('подготовка: гружу модель через API воркера (это не проверяемая часть)');
                await fetch(args.workerUrl + '/api/models/load-with-params', {
                    method: 'POST', headers: { 'X-API-Token': API_TOKEN, 'Content-Type': 'application/json' },
                    body: JSON.stringify({ name: args.model, contextSize: 8192, parallel: 1 })
                }).catch(() => null);
                const w0 = await waitFor(async () => {
                    const row = await modelRow(args.model);
                    return { done: !!row && String(row.state).toLowerCase() === 'loaded', row, note: row && row.state };
                }, deadlineMs, 3000);
                record(!!w0.done, 'подготовка: модель загружена', w0.note || '');
            } else {
                info('подготовка: модель уже загружена');
            }
            const key = await openCard(page, args.model);
            // R151: бэкенд фиксируем СРАЗУ после открытия карточки — именно на нём
            // оператор нажмёт «применить». Читать его потом нельзя: выделение в
            // панели могло смениться (живой случай: карточка была на доноре, факт
            // искали у мастера, и сверка получалась не по той копии).
            const factBackend = await selectedBackendId(page);
            info('бэкенд из карточки для сверки фактов: ' + (factBackend || 'не определён'));
            const filled = await fillParams(page, key, wanted);
            info('в форме стоит: ' + JSON.stringify(filled));
            if (filled) {
                for (const [f, want] of Object.entries(wanted)) {
                    const got = filled[f];
                    const norm = (v) => String(v === true ? 'true' : (v === false ? 'false' : v));
                    if (norm(got) !== norm(want)) {
                        record(false, 'форма: ' + f, 'в поле ' + JSON.stringify(got) + ', ждали ' + JSON.stringify(want));
                    }
                }
            }
            info('форма заполнена: ' + JSON.stringify(wanted));
            const t0 = Date.now();
            // R196 (2026-10-20): ПАНЕЛЬ ПЕРЕРИСОВЫВАЕТСЯ ЦИКЛОМ ОБНОВЛЕНИЯ (~5 с) и
            // аккордеон может закрыться между заполнением формы и кликом — кнопка
            // становится невидимой, Playwright отказывался кликать, и весь сценарий
            // падал с "0 из 0 проверок". Перед кликом заново раскрываем карточку и
            // ждём видимости кнопки (несколько попыток, потому что перерисовка идёт
            // асинхронно).
            {
                const applySel = `[data-model-params-apply="${key}"]`;
                let clicked = false;
                for (let attempt = 1; attempt <= 4 && !clicked; attempt++) {
                    await page.evaluate(sel => {
                        const btn = document.querySelector(sel);
                        if (!btn) return;
                        const acc = btn.closest('details');
                        if (acc) acc.open = true;
                        const host = btn.closest('.gguf-mp-editor, .gguf-detail-pane, .gguf-detail-content');
                        if (host && host.scrollIntoView) host.scrollIntoView({ block: 'center' });
                        else if (btn.scrollIntoView) btn.scrollIntoView({ block: 'center' });
                    }, applySel).catch(() => {});
                    try {
                        await page.locator(applySel).first().waitFor({ state: 'visible', timeout: 5000 });
                        await page.locator(applySel).first().click({ timeout: 5000 });
                        clicked = true;
                    } catch (e) {
                        info(`кнопка «применить» не видна (попытка ${attempt}/4): ` + String(e.message).split('\n')[0]);
                    }
                }
                if (!clicked) record(false, 'кнопка «применить и перезагрузить»', 'не удалось нажать: элемент невидим');
            }

            if (args.scenario === 'interrupt') {
                // Запрос КЛИЕНТА ровно в момент перезагрузки (как делает балансер).
                const r = await clientApi('/api/chat', {
                    method: 'POST',
                    body: { model: args.model, messages: [{ role: 'user', content: 'скажи ок' }], stream: false, options: { num_predict: 8, num_ctx: 4096 } }
                });
                const ms = Date.now() - t0;
                info(`запрос во время перезагрузки: HTTP ${r.status} за ${ms} мс`);
                console.log('    тело: ' + String(r.text).slice(0, 300).replace(/\s+/g, ' '));
                record(r.status !== 0 && r.status < 600, 'во время перезагрузки пришёл ОТВЕТ (не вечное ожидание)',
                    `HTTP ${r.status} за ${ms} мс`);
            }

            // R174 (2026-10-18): ЖДАТЬ НАЧАЛО перезагрузки, а не «сразу loaded».
            //
            // Живой дефект инструмента: клик по «Применить» запускает reload
            // асинхронно, а проверка ниже спрашивала состояние НЕМЕДЛЕННО. Модель
            // ещё числилась loaded (доли секунды до перехода в loading), ожидание
            // завершалось за 0 с, факты читались старые — и сценарий печатал
            // «применилось 32768, ждали 8192», хотя reload просто не успел начаться.
            // Ложный FAIL хуже отсутствия проверки: он дискредитирует инструмент.
            const startedTransition = await waitFor(async () => {
                const row = await modelRow(key);
                const st = row && String(row.state || '').toLowerCase();
                return { done: st !== '' && st !== 'loaded', row, note: st };
            }, 45000, 500);
            if (!startedTransition.done) {
                info('перезагрузка не началась за 45 с (модель так и осталась loaded) — ' +
                    'apply не запустил reload; факты ниже будут старыми');
            }

            const w = await waitFor(async () => {
                const row = await modelRow(key);
                const st = row && String(row.state || '').toLowerCase();
                return { done: st === 'loaded', row, note: st };
            }, deadlineMs, 2000);
            record(!!w.done, 'модель загружена после применения', w.note || '');
            const ms = Date.now() - t0;
            info(`перезагрузка заняла ~${Math.round(ms / 1000)} с`);

            const uptimeAfter = await workerUptime();
            // Сравниваем СЕКУНДЫ, не строки: см. uptimeSeconds.
            const upBefore = uptimeSeconds(uptimeBefore);
            const upAfter = uptimeSeconds(uptimeAfter);
            record(upBefore !== null && upAfter !== null && upAfter >= upBefore,
                'процесс воркера НЕ перезапускался',
                `uptime до ${uptimeBefore} (${upBefore} с), после ${uptimeAfter} (${upAfter} с)`);

            // R143: факты берём ТОЛЬКО из runtime-конфига и ждём, пока модель в нём
            // появится ЗАГРУЖЕННОЙ. Раньше здесь стоял молчаливый запасной источник
            // (factsFrom(w.row) из воркерского /api/models) — ДРУГОЙ по семантике:
            // живой случай, когда runtime ещё не видел модель (гонка с
            // перезагрузкой), инструмент подставлял оттуда parallel=1 и
            // contextPerSeq=8192 и выдавал FAIL на верно применённые параметры.
            //
            // Вторая гонка (тоже живая): сразу после apply перезагрузка ещё идёт, и
            // в loaded_models стоит ПЛЕЙСХОЛДЕР (state=loading, нули) — сравнение с
            // ним давало ложные FAIL. Поэтому runtimeFacts отдаёт только loaded, а
            // ожидание здесь длинное: перезагрузка на стенде занимает ~65 с.
            // Бэкенд для сверки (factBackend) зафиксирован ВЫШЕ, сразу после открытия
            // карточки — до применения параметров.
            let rf = await runtimeFacts(args.model, factBackend);
            if (!rf.row) {
                const got = await waitFor(async () => {
                    const cur = await runtimeFacts(args.model, factBackend);
                    return { done: !!cur.row, row: cur, note: cur.source };
                }, 120000, 3000);
                rf = got.row || rf;
            }
            if (rf.row) {
                const fact = rf.row;
                info('факт (' + rf.source + '): ' + JSON.stringify(fact));
                // R151: если факт пришёл НЕ от той копии, что выбрана в карточке —
                // сравнивать нельзя: значения могли совпасть случайно, а параметры
                // применялись к другой копии. Это НЕ «параметр не применился», это
                // «проверить не удалось», и инструмент говорит именно так.
                if (!rf.backendMatched) {
                    record(false, 'сверка с копией из карточки',
                        'факты пришли от ' + (rf.backend || '?') + ', а карточка работала с ' +
                        (rf.wantBackend || '?') + ' — сравнение не авторитетно');
                } else {
                const cmp = compareWanted(wanted, fact);
                const diffs = cmp.diffs;
                if (cmp.unreported.length) info('не подтверждено замерами (наблюдаемость): ' + cmp.unreported.join(', '));
                if (cmp.adaptations.length) info('применено с АДАПТАЦИЕЙ по памяти: ' + cmp.adaptations.join('; '));
                // Название проверки называет факт адаптации: «применились именно
                // заданные параметры» было бы неправдой, если загрузчик понизил
                // число слоёв, а FAIL — неправдой вдвойне (это штатная подгонка).
                record(diffs.length === 0,
                    cmp.adaptations.length
                        ? 'применились заданные параметры (кроме адаптированных под память)'
                        : 'применились ИМЕННО заданные параметры',
                    diffs.join('; '));
                }
            } else {
                // Честно: фактов нет — сравнение не выполняем и НЕ подменяем источник.
                info('факт (' + rf.source + '): runtime-конфиг не содержит эту модель — сравнение пропущено');
                record(false, 'применились ИМЕННО заданные параметры',
                    'runtime-конфиг не отдал модель (' + rf.source + '); сравнение с ДРУГИМ источником дало бы ложный вывод');
            }

            const prof = await api(`/api/v1/cppworker/model-profiles/${encodeURIComponent(key)}`);
            const p = (prof.json && prof.json.profile) || {};
            const pcmp = compareWanted(wanted, {
                contextLength: p.contextLength, contextPerSeq: p.contextPerSeq, numGpuLayers: p.numGpuLayers,
                batchSize: p.batchSize, parallel: p.parallel, kvCacheType: p.kvCacheType,
                flashAttn: p.flashAttn, useMmap: p.useMmap, enableReasoning: p.enableReasoning
            });
            record(pcmp.diffs.length === 0, 'в профиле сохранено то же', pcmp.diffs.join('; '));

            if (args.scenario === 'interrupt') {
                const r2 = await clientApi('/api/chat', {
                    method: 'POST',
                    body: { model: args.model, messages: [{ role: 'user', content: 'скажи ок' }], stream: false, options: { num_predict: 8 } }
                });
                record(r2.status === 200, 'повторный запрос после перезагрузки прошёл', `HTTP ${r2.status}`);
            }
        }

        if (args.scenario === 'unload-load') {
            // R145 (2026-10-16): сценарий был НЕРАБОЧИМ и врал сразу двумя способами.
            //
            //  1. `key` здесь не определялся (он живёт в ветке reload/interrupt) —
            //     сценарий падал с ReferenceError на первом же ожидании.
            //  2. Выгрузка шла через `api()` (admin-порт 18081), где маршрута
            //     /api/models/unload ВООБЩЕ НЕТ (живая проверка: 404 page not
            //     found), и с телом {"model":...}, тогда как балансер переводит
            //     в query-параметр воркера поле ИМЕННО `name` (R60.9,
            //     extractNameFromBody). Живой 400 воркера: "name query parameter
            //     is required".
            //
            // Верная форма подтверждена живьём: POST на клиентский порт
            // /api/models/unload с телом {"name":"<модель>"} → 200
            // {"status":"unloaded"}, затем /api/models/load-with-params → 202.
            const key = await openCard(page, args.model);
            // R151: бэкенд фиксируем сразу — он же понадобится для сверки фактов
            // после цикла выгрузка→загрузка (выделение в панели могло смениться).
            const factBackend2 = await selectedBackendId(page);
            info('бэкенд из карточки для сверки фактов: ' + (factBackend2 || 'не определён'));

            // Выгружаем по имени ИНСТАНСА (handle), а не по имени файла из формы:
            // см. loadedHandleName и живой 404 с ".gguf not found".
            const handle = await loadedHandleName(args.model);
            info('имя для выгрузки (handle): ' + handle + '  [имя файла в форме: ' + key + ']');
            const u = await clientApi('/api/models/unload', { method: 'POST', body: { name: handle } });
            record(u.status === 200, 'выгрузка', `HTTP ${u.status} ${String(u.text || '').slice(0, 120)}`);

            const gone = await waitFor(async () => {
                const row = await modelRow(key);
                return { done: !row, row };
            }, 120000, 1500);
            record(!!gone.done, 'модель исчезла из списка', gone.note || '');

            // Сверяем только то, что РЕАЛЬНО отправляем этой загрузкой: иначе
            // инструмент честно, но бессмысленно сообщает «не подтверждено» про
            // параметр, которого в запросе не было (flashAttn живёт в профиле).
            const wantedForLoad = {
                contextLength: wanted.contextLength,
                contextPerSeq: wanted.contextPerSeq,
                parallel: wanted.parallel,
                enableReasoning: wanted.enableReasoning
            };
            const l = await clientApi('/api/models/load-with-params', {
                method: 'POST',
                body: Object.assign({ name: handle }, {
                    contextSize: wantedForLoad.contextLength,
                    contextPerSeq: wantedForLoad.contextPerSeq,
                    parallel: wantedForLoad.parallel,
                    enableReasoning: wantedForLoad.enableReasoning
                })
            });
            record(l.status === 200 || l.status === 202, 'загрузка', `HTTP ${l.status}`);

            const w = await waitFor(async () => {
                const row = await modelRow(key);
                const st = row && String(row.state || '').toLowerCase();
                return { done: st === 'loaded', row, note: st };
            }, deadlineMs, 2000);
            record(!!w.done, 'модель загружена', w.note || '');

            // Подтверждение, что после ЦИКЛА выгрузка→загрузка применились те же
            // параметры (источник тот же, что у reload — runtime-факт загруженного
            // инстанса, см. R143). Бэкенд (factBackend2) зафиксирован выше, до цикла.
            let rf2 = await runtimeFacts(key, factBackend2);
            if (!rf2.row) {
                const got2 = await waitFor(async () => {
                    const cur = await runtimeFacts(key, factBackend2);
                    return { done: !!cur.row, row: cur, note: cur.source };
                }, 120000, 3000);
                rf2 = got2.row || rf2;
            }
            if (rf2.row) {
                info('факт после цикла (' + rf2.source + '): ' + JSON.stringify(rf2.row));
                const c2 = compareWanted(wantedForLoad, rf2.row);
                if (c2.unreported.length) info('не подтверждено замерами: ' + c2.unreported.join(', '));
                if (c2.adaptations.length) info('применено с АДАПТАЦИЕЙ по памяти: ' + c2.adaptations.join('; '));
                record(c2.diffs.length === 0,
                    c2.adaptations.length
                        ? 'после цикла применились запрошенные параметры (кроме адаптированных под память)'
                        : 'после цикла применились ИМЕННО запрошенные параметры',
                    c2.diffs.join('; '));
            } else {
                record(false, 'после цикла применились запрошенные параметры',
                    'runtime-конфиг не отдал загруженную модель (' + rf2.source + ')');
            }
        }

        if (args.scenario === 'ctx-ceiling') {
            const models = await api('/api/models');
            const j = models.json || {};
            const ceiling = Math.max(j.model_max_context || 0, j.gguf_max_context || 0, j.max_vram_n_ctx || 0);
            info(`потолок окна по данным воркера: ${ceiling}`);
            const ask = Math.max(ceiling * 2, 262144);
            const t0 = Date.now();
            const r = await clientApi('/api/chat', {
                method: 'POST',
                body: { model: args.model, messages: [{ role: 'user', content: 'ок' }], stream: false, options: { num_predict: 8, num_ctx: ask } }
            });
            const ms = Date.now() - t0;
            info(`запрос num_ctx=${ask}: HTTP ${r.status} за ${ms} мс`);
            console.log('    тело: ' + String(r.text).slice(0, 400).replace(/\s+/g, ' '));
            record(r.status >= 400 && r.status < 500, 'клиенту дан ОТКАЗ (4xx), а не падение/вечность', `HTTP ${r.status}`);
            record(/context|n_ctx|окн|context window|maximum/i.test(r.text || ''), 'в отказе названа причина и предел',
                String(r.text).slice(0, 160).replace(/\s+/g, ' '));
            const good = await clientApi('/api/chat', {
                method: 'POST',
                body: { model: args.model, messages: [{ role: 'user', content: 'ок' }], stream: false, options: { num_predict: 8 } }
            });
            record(good.status === 200, 'модель после отказа продолжает работать', `HTTP ${good.status}`);
        }

        info(`ошибок консоли страницы: ${consoleErrors.length}`);
        if (consoleErrors.length) console.log('    ' + consoleErrors.slice(0, 5).join('\n    '));

        const failed = results.filter(r => !r.ok);
        console.log(`\nИТОГ: ${results.length - failed.length} из ${results.length} проверок пройдено`);
        if (failed.length) {
            console.log('НЕ ПРОШЛИ:');
            failed.forEach(f => console.log('  - ' + f.name + (f.detail ? ': ' + f.detail : '')));
        }
        if (args.keepOpen) { info('окно остаётся открытым 10 минут (--keep-open)'); await page.waitForTimeout(600000); }
        await browser.close();
        process.exit(failed.length ? 1 : 0);
    } catch (e) {
        console.error('ОШИБКА запуска сценария: ' + (e && e.stack ? e.stack : e));
        try { await browser.close(); } catch (_) {}
        process.exit(2);
    }
}

main();

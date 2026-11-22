// verify-gguf-async-blocks-r691.js — R691: ЖИВОЙ замер «пересборка панели
// возвращает асинхронные блоки в заглушку «Загрузка…»».
//
// ЖИВАЯ ЖАЛОБА (из отчёта R689, подтверждение и уточнение): при РЕАЛЬНОМ
// изменении данных правая панель GGUF пересобирается, и блоки, которые рисуются
// ПОСЛЕ панели по ответам API, снова становятся заглушкой «Загрузка…» до
// повторного клика по вкладке/модели. Блоки:
//   * «что получат новые модели»  — [data-role="cpp-load-defaults"],
//   * «модели в папке»            — [data-role="cpp-model-catalog"],
//   * список профилей моделей     — #cppProfilesList,
//   * «параметры запуска модели»  — .gguf-mp-editor[data-model] (значения профиля
//     приходят отдельным запросом: fillModelParamEditor/hydrateModelParamEditors).
//
// ЧТО МЕРЯЕТ (числами):
//   A. вкладка «Настройки»: дождались асинхронной отрисовки, затем РЕАЛЬНО
//      изменили данные (статус бэкенда в шапке) и вызвали явную пересборку
//      (refreshDetailPanel без {auto:true} — как «Обновить»):
//        - сколько блоков в момент после пересборки показывают заглушку;
//        - через сколько секунд каждый блок вернул содержимое (или не вернул);
//        - сколько запросов профилей/дефолтов/каталога ушло в фоне;
//        - сохранены ли значение в поле, scrollTop и раскрытые секции (R688/R689).
//   A2. переключение вкладки «Настройки» → «Модели» → «Настройки»: сколько раз
//      блоки были заглушкой за это время (моргание) и через сколько пришло
//      содержимое.
//   B. 20 с БЕЗ ВВОДА и без изменения данных: число подмен DOM панели и ΔscrollTop
//      (регресс R689: обязано быть 0/0).
//   C. вкладка «Модели»: раскрыли карточку параметров (гидратация профиля),
//      затем явная пересборка при реальном изменении данных: сколько полей
//      профиля и «пределов окна» потеряно.
//
// ЗАПУСК:
//   node scripts/verify-gguf-async-blocks-r691.js http://127.0.0.1:18084 --expect=defect
//   node scripts/verify-gguf-async-blocks-r691.js http://127.0.0.1:18084 --expect=fixed
//
// Логин — LB_ADMIN_USER/LB_ADMIN_PASSWORD из deployments/.env (в вывод не
// печатается). Браузер — установленный Edge (channel:'msedge').
'use strict';

const { chromium } = require('playwright');
const fs = require('fs');
const path = require('path');

const BASE = (process.argv[2] || 'http://127.0.0.1:18084').replace(/\/+$/, '');
const EXPECT = (process.argv.find((a) => a.startsWith('--expect=')) || '--expect=report').slice('--expect='.length);
const WINDOW_MS = Number(process.env.GGUF_WINDOW_MS || 20000);
const RESTORE_WAIT_MS = Number(process.env.GGUF_RESTORE_WAIT_MS || 20000);

const ROOT = path.join(__dirname, '..');
const ARTIFACT_DIR = path.join(ROOT, 'test-results', 'gguf-async-blocks-r691-' +
    new Date().toISOString().replace(/[:.]/g, '-'));
fs.mkdirSync(ARTIFACT_DIR, { recursive: true });

function envValue(key) {
    try {
        const line = fs.readFileSync(path.join(ROOT, 'deployments', '.env'), 'utf8')
            .split(/\r?\n/).find((l) => l.startsWith(key + '='));
        return line ? line.slice(key.length + 1).trim() : '';
    } catch (e) { return ''; }
}

const report = { url: BASE, expect: EXPECT, windowMs: WINDOW_MS, checks: [], runA: {}, runA2: {}, runB: {}, runC: {} };
let exitCode = 0;
const user = process.env.ADMIN_USER || envValue('LB_ADMIN_USER') || 'skyadmin';
const pass = process.env.ADMIN_PASSWORD || envValue('LB_ADMIN_PASSWORD');

function check(name, ok, detail) {
    report.checks.push({ name, ok, detail: detail || '' });
    console.log(`  ${ok ? 'PASS' : 'FAIL'} ${name}${detail ? ' — ' + detail : ''}`);
    if (!ok) exitCode = 1;
}
function note(text) { console.log('  · ' + text); }

// --- helpers, вставляемые в страницу -----------------------------------------

// BLOCKS_JS — описание измеряемых блоков + снимок их состояния.
const BLOCKS_JS = `
    window.__blocks = function () {
        return [
            { id: 'defaults', sel: '[data-role="cpp-load-defaults"]', holder: '#ggufDetailPanel' },
            { id: 'catalog',  sel: '[data-role="cpp-model-catalog"]', holder: '#ggufDetailPanel' },
            { id: 'profiles', sel: '#cppProfilesList', holder: '#ggufDetailPanel' }
        ];
    };
    window.__blockState = function (el) {
        if (!el) return { present: false, len: 0, placeholder: false, empty: true, text: '' };
        var txt = (el.innerText || '').replace(/\\s+/g, ' ').trim();
        var ph = /Загрузка|Loading/i.test(txt) && txt.length < 80;
        return {
            present: true,
            len: txt.length,
            placeholder: ph,
            empty: txt.length === 0,
            text: txt.slice(0, 100)
        };
    };
    window.__snapshotBlocks = function () {
        var panel = document.getElementById('ggufDetailPanel');
        var out = {};
        window.__blocks().forEach(function (b) {
            var el = panel ? panel.querySelector(b.sel) : null;
            out[b.id] = window.__blockState(el);
        });
        return out;
    };
    window.__panelSnapshot = function () {
        var panel = document.getElementById('ggufDetailPanel');
        var sc = document.scrollingElement || document.documentElement;
        var ctx = document.getElementById('ggufOptCtxSize');
        return {
            height: panel ? Math.round(panel.getBoundingClientRect().height) : 0,
            scrollTop: sc ? sc.scrollTop : 0,
            ctxValue: ctx ? ctx.value : null,
            openSections: Array.from(document.querySelectorAll('#ggufDetailPanel details[open]'))
                .map(function (d) { return d.getAttribute('data-acc') || d.getAttribute('data-model-acc') || '?'; }),
            blocks: window.__snapshotBlocks()
        };
    };
    window.__placeholderCount = function (snap) {
        var n = 0;
        Object.keys(snap).forEach(function (k) { if (snap[k].placeholder) n++; });
        return n;
    };
    // Пробы "моргания": сэмплируем состояние блоков каждые 50 мс.
    window.__watchBlocks = function (ms) {
        return new Promise(function (resolve) {
            var samples = [];
            var t0 = performance.now();
            var timer = setInterval(function () {
                var snap = window.__snapshotBlocks();
                samples.push({
                    t: Math.round(performance.now() - t0),
                    placeholders: window.__placeholderCount(snap),
                    lens: Object.keys(snap).reduce(function (acc, k) { acc[k] = snap[k].len; return acc; }, {}),
                    ph: Object.keys(snap).reduce(function (acc, k) { acc[k] = snap[k].placeholder; return acc; }, {})
                });
                if (performance.now() - t0 >= ms) { clearInterval(timer); resolve(samples); }
            }, 50);
        });
    };
    window.__editorSnapshot = function () {
        var editors = Array.from(document.querySelectorAll('.gguf-mp-editor[data-model]'));
        return editors.map(function (el) {
            var fields = Array.from(el.querySelectorAll('.gguf-mp-field'));
            var filled = fields.filter(function (f) {
                if (f.type === 'checkbox') return !!f.checked;
                return String(f.value === undefined || f.value === null ? '' : f.value).trim() !== '';
            });
            var limit = el.querySelector('.gguf-mp-limit');
            var cells = Array.from(el.querySelectorAll('[data-profile-field]'));
            return {
                model: el.getAttribute('data-model') || '',
                hydrated: el.getAttribute('data-hydrated') === '1',
                filled: filled.length,
                total: fields.length,
                limit: limit ? String(limit.textContent || '').trim().slice(0, 80) : '',
                profileCells: cells.length,
                profileCellsSet: cells.filter(function (c) {
                    return (c.textContent || '').indexOf('не задано') === -1 && String(c.textContent || '').trim() !== '';
                }).length
            };
        });
    };
`;

// --- открытие страницы -------------------------------------------------------

async function openGguf(page) {
    await page.goto(BASE + '/', { waitUntil: 'domcontentloaded', timeout: 30000 });
    const login = await page.evaluate(async ({ u, p }) => {
        const r = await fetch('/api/v1/auth/login', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ name: u, password: p })
        });
        return r.status;
    }, { u: user, p: pass });
    check('вход администратора', login === 200, 'HTTP ' + login);

    await page.goto(BASE + '/?page=gguf', { waitUntil: 'domcontentloaded', timeout: 30000 });
    await page.waitForSelector('#ggufDetailPanel', { timeout: 30000 });
    await page.waitForTimeout(2500);
    await page.evaluate(BLOCKS_JS);

    const picked = await page.evaluate(() => {
        const items = Array.from(document.querySelectorAll('#ggufBackendList [data-backend-id]'));
        const target = items.find((el) => el.getAttribute('data-backend-id')) || items[0];
        if (!target) return '';
        target.click();
        return target.getAttribute('data-backend-id') || '';
    });
    if (!picked) throw new Error('в левом списке нет ни одного бэкенда');
    check('бэкенд выбран', true, picked);
    await page.waitForTimeout(2500);
    return picked;
}

/** Открыть вкладку панели (клик как у оператора). */
async function openTab(page, tab) {
    const ok = await page.evaluate((t) => {
        const el = document.querySelector('#ggufDetailPanel [data-detail-tab="' + t + '"]');
        if (!el) return false;
        el.click();
        return true;
    }, tab);
    check('вкладка «' + tab + '» открыта', ok);
    return ok;
}

/** Дождаться, пока асинхронные блоки настроек отрисуются впервые. */
async function waitAsyncBlocks(page, timeoutMs) {
    return page.waitForFunction(() => {
        const panel = document.getElementById('ggufDetailPanel');
        if (!panel) return false;
        if (!document.getElementById('ggufOptCtxSize')) return false;
        const isAlive = (el) => {
            if (!el) return false;
            const t = (el.innerText || '').replace(/\s+/g, ' ').trim();
            return t.length > 40 && !/Загрузка|Loading/i.test(t);
        };
        return isAlive(panel.querySelector('[data-role="cpp-load-defaults"]')) &&
            isAlive(panel.querySelector('[data-role="cpp-model-catalog"]'));
    }, null, { timeout: timeoutMs || 25000 }).then(() => true).catch(() => false);
}

/** Реальное изменение данных + явная пересборка панели (как «Обновить»/таб). */
async function rebuildWithRealChange(page) {
    return page.evaluate(() => {
        const st = window.GgufModule.state;
        const be = (st.registeredBackends || []).find((x) => x.id === st.selectedBackendId);
        const statusBefore = be ? be.status : '';
        if (be) be.status = (be.status === 'healthy') ? 'offline' : 'healthy';
        const t0 = performance.now();
        const rebuilt = window.GgufModule.refreshDetailPanel();
        const ms = Math.round(performance.now() - t0);
        const blocks = window.__snapshotBlocks();
        const panel = window.__panelSnapshot();
        return {
            rebuilt: rebuilt === true,
            rebuildMs: ms,
            statusBefore: statusBefore,
            statusAfter: be ? be.status : '',
            blocks: blocks,
            placeholders: window.__placeholderCount(blocks),
            panel: panel
        };
    });
}

/** Вернуть тестовое изменение статуса (стенд не трогаем — только state в памяти). */
async function restoreStatus(page, original) {
    await page.evaluate((s) => {
        const st = window.GgufModule.state;
        const be = (st.registeredBackends || []).find((x) => x.id === st.selectedBackendId);
        if (be && s) be.status = s;
    }, original);
}

/** Дождаться возврата содержимого блоков, замеряя время и моргание. */
async function waitBlocksBack(page, baseline, timeoutMs) {
    const t0 = Date.now();
    return page.evaluate(async ({ base, limit }) => {
        const started = performance.now();
        const samples = [];
        const re = { defaults: base.defaults.len, catalog: base.catalog.len, profiles: base.profiles.len };
        function alive(snap) {
            return Object.keys(re).every(function (k) {
                const want = re[k];
                if (want <= 0) return !snap[k].placeholder;
                return !snap[k].placeholder && snap[k].len >= Math.max(20, Math.min(want - 2, want * 0.8));
            });
        }
        return await new Promise(function (resolve) {
            const timer = setInterval(function () {
                const snap = window.__snapshotBlocks();
                const ph = window.__placeholderCount(snap);
                samples.push({ t: Math.round(performance.now() - started), placeholders: ph });
                if (alive(snap) || performance.now() - started >= limit) {
                    clearInterval(timer);
                    resolve({
                        ms: Math.round(performance.now() - started),
                        restored: alive(snap),
                        samples: samples.length,
                        placeholderSamples: samples.filter((s) => s.placeholders > 0).length,
                        final: snap
                    });
                }
            }, 50);
        });
    }, { base: baseline, limit: timeoutMs }).then((r) => r);
}

// --- основной сценарий -------------------------------------------------------

(async () => {
    if (!pass) throw new Error('нет LB_ADMIN_PASSWORD в deployments/.env');

    const browser = await chromium.launch({ channel: 'msedge', headless: true });
    const context = await browser.newContext({ viewport: { width: 1600, height: 1000 } });
    const page = await context.newPage();
    const requests = [];
    page.on('request', (r) => requests.push({ url: r.url(), t: Date.now() }));
    // Ошибки страницы: дефект «блок в заглушке» нередко прячет исключение в
    // консоли, а после правки их быть не должно вовсе.
    const pageErrors = [];
    page.on('pageerror', (err) => pageErrors.push(String((err && err.message) || err)));
    page.on('console', (msg) => {
        if (msg.type() === 'error') pageErrors.push('console: ' + msg.text());
    });
    const countRequests = (pattern) => requests.filter((r) => pattern.test(r.url)).length;
    const PROFILES_RE = /\/api\/v1\/cppworker\/model-profiles(\?|$)/;
    const DEFAULTS_RE = /\/api\/v1\/cppworker\/load-defaults(\?|$)/;
    const CATALOG_RE = /\/api\/v1\/cppworker\/model-catalog(\?|$)/;

    // ================= A. вкладка «Настройки» ================================
    console.log('\n[A] «Настройки»: реальное изменение → явная пересборка панели');
    const backendId = await openGguf(page);
    await openTab(page, 'settings');
    const hydrated = await waitAsyncBlocks(page, 25000);
    check('асинхронные блоки отрисованы впервые', hydrated,
        hydrated ? '' : 'блоки не наполнились — замер потерял точку отсчёта');
    // Оператор ввёл значение и ушёл из поля: R688 обязан его перенести.
    await page.evaluate(() => {
        const el = document.getElementById('ggufOptCtxSize');
        if (el) { el.value = '9999'; }
        const sc = document.scrollingElement || document.documentElement;
        sc.scrollTop = 400;
    });
    await page.waitForTimeout(400);
    const baseline = await page.evaluate(() => {
        const snap = window.__panelSnapshot();
        const panel = document.getElementById('ggufDetailPanel');
        panel.setAttribute('data-r691-baseline', '1');
        return {
            blocks: snap.blocks,
            panel: { height: snap.height },
            scrollTop: snap.scrollTop,
            openSections: snap.openSections,
            ctxValue: snap.ctxValue
        };
    });
    note('базовый снимок: defaults=' + baseline.blocks.defaults.len + ' симв., catalog=' +
        baseline.blocks.catalog.len + ', profiles=' + baseline.blocks.profiles.len +
        ', поле=' + baseline.ctxValue + ', scrollTop=' + baseline.scrollTop);

    const reqBefore = {
        profiles: countRequests(PROFILES_RE),
        defaults: countRequests(DEFAULTS_RE),
        catalog: countRequests(CATALOG_RE)
    };
    // Реальное изменение данных + явная пересборка.
    const rebuild = await rebuildWithRealChange(page);
    const rebuildReqBase = {
        profiles: countRequests(PROFILES_RE),
        defaults: countRequests(DEFAULTS_RE),
        catalog: countRequests(CATALOG_RE)
    };
    note('пересборка: rebuilt=' + rebuild.rebuilt + ' за ' + rebuild.rebuildMs + ' мс, заглушек сразу после: ' +
        rebuild.placeholders + ' (' + Object.keys(rebuild.blocks).filter((k) => rebuild.blocks[k].placeholder).join(',') + ')');
    // Моргание: сэмплируем 1.5 с сразу после пересборки.
    const blink = await page.evaluate(() => window.__watchBlocks(1500));
    // Дождаться возврата содержимого (в дефекте — не возвращается).
    const back = await waitBlocksBack(page, rebuild.blocks && baseline.blocks, RESTORE_WAIT_MS);
    await page.waitForTimeout(2500); // фоновое обновление должно успеть прийти
    const afterSnapshot = await page.evaluate(() => window.__panelSnapshot());
    const reqAfter = {
        profiles: countRequests(PROFILES_RE),
        defaults: countRequests(DEFAULTS_RE),
        catalog: countRequests(CATALOG_RE)
    };
    // «Не моргать»: считаем подмены DOM внутри блоков уже ПОСЛЕ отрисовки.
    const blockMutations = await page.evaluate(() => new Promise((resolve) => {
        const panel = document.getElementById('ggufDetailPanel');
        let n = 0;
        const obs = new MutationObserver((recs) => {
            for (const r of recs) if (r.type === 'childList') n += r.addedNodes.length + r.removedNodes.length;
        });
        if (panel) obs.observe(panel, { childList: true, subtree: true });
        setTimeout(() => { obs.disconnect(); resolve(n); }, 2000);
    }));
    report.runA = {
        backendId,
        baseline: { blocks: baseline.blocks, ctxValue: baseline.ctxValue, scrollTop: baseline.scrollTop, openSections: baseline.openSections },
        rebuild: { rebuilt: rebuild.rebuilt, rebuildMs: rebuild.rebuildMs, placeholders: rebuild.placeholders, blocks: rebuild.blocks, height: rebuild.panel.height, scrollTop: rebuild.panel.scrollTop, ctxValue: rebuild.panel.ctxValue },
        blink: { samples: blink.length, placeholderSamples: blink.filter((s) => s.placeholders > 0).length, maxPlaceholders: Math.max.apply(null, blink.map((s) => s.placeholders)) },
        restore: back,
        after: afterSnapshot,
        requests: {
            before: reqBefore,
            afterRebuild: rebuildReqBase,
            after: reqAfter,
            delta: { profiles: reqAfter.profiles - reqBefore.profiles, defaults: reqAfter.defaults - reqBefore.defaults, catalog: reqAfter.catalog - reqBefore.catalog }
        },
        blockMutationsAfterPaint: blockMutations
    };
    note('содержимое вернулось: ' + (back.restored ? 'да, за ' + back.ms + ' мс' : 'НЕТ за ' + back.ms + ' мс') +
        '; сэмплов с заглушкой: ' + report.runA.blink.placeholderSamples + '/' + report.runA.blink.samples);
    note('фоновые запросы за окно: profiles +' + report.runA.requests.delta.profiles +
        ', defaults +' + report.runA.requests.delta.defaults + ', catalog +' + report.runA.requests.delta.catalog);
    note('R688/R689 после пересборки: поле=' + afterSnapshot.ctxValue + ', scrollTop=' + afterSnapshot.scrollTop +
        ', секции=' + JSON.stringify(afterSnapshot.openSections) + ', высота ' + baseline.panel.height + '→' + afterSnapshot.height);
    note('подмены DOM в панели за 2 с ПОСЛЕ отрисовки: ' + blockMutations);

    // ================= A2. переключение вкладки ==============================
    console.log('\n[A2] вкладка «Настройки» → «Модели» → «Настройки»: моргание блоков');
    await openTab(page, 'models');
    await page.waitForTimeout(800);
    const tabWatch = await page.evaluate(() => window.__watchBlocks(1));
    await openTab(page, 'settings');
    const tabSamples = await page.evaluate(() => window.__watchBlocks(2500));
    const tabBack = await page.evaluate(() => window.__snapshotBlocks());
    report.runA2 = {
        samples: tabSamples.length,
        placeholderSamples: tabSamples.filter((s) => s.placeholders > 0).length,
        maxPlaceholders: Math.max.apply(null, tabSamples.map((s) => s.placeholders)),
        firstSamplePh: tabSamples.length ? tabSamples[0].placeholders : -1,
        final: tabBack
    };
    note('после клика по «Настройки»: сэмплов с заглушкой ' + report.runA2.placeholderSamples + '/' + report.runA2.samples +
        ', максимум заглушек ' + report.runA2.maxPlaceholders);

    // ================= B. 20 с без ввода и без изменений =====================
    console.log('\n[B] ' + (WINDOW_MS / 1000) + ' с БЕЗ ИЗМЕНЕНИЙ: подмены панели и scrollTop (регресс R689)');
    await restoreStatus(page, rebuild.statusBefore);
    // Даём статусу вернуться и панели успокоиться.
    await page.waitForTimeout(6000);
    await page.evaluate(() => {
        const panel = document.getElementById('ggufDetailPanel');
        const diag = window.__diagB = { panelSwaps: 0, paneSwaps: 0, scrollStart: 0, scrollEnd: 0, sumAbsDelta: 0 };
        const sc = document.scrollingElement || document.documentElement;
        const scAny = (function () {
            let n = panel;
            while (n && n !== document.body && n !== document.documentElement) {
                const cs = window.getComputedStyle(n);
                if (/(auto|scroll)/.test(cs.overflowY) && n.scrollHeight > n.clientHeight + 2) return n;
                n = n.parentElement;
            }
            return sc;
        })();
        const maxTop = Math.max(0, scAny.scrollHeight - scAny.clientHeight);
        scAny.scrollTop = Math.max(0, maxTop - 120);
        diag.scrollStart = scAny.scrollTop;
        diag.scrollEnd = scAny.scrollTop;
        new MutationObserver((recs) => {
            for (const r of recs) if (r.type === 'childList' && (r.addedNodes.length || r.removedNodes.length)) diag.panelSwaps++;
        }).observe(panel, { childList: true });
        window.__diagBTimer = setInterval(() => {
            diag.scrollEnd = scAny.scrollTop;
        }, 200);
    });
    await page.waitForTimeout(WINDOW_MS);
    const diagB = await page.evaluate(() => {
        clearInterval(window.__diagBTimer);
        const d = window.__diagB;
        return { panelSwaps: d.panelSwaps, scrollStart: d.scrollStart, scrollEnd: d.scrollEnd };
    });
    const bSnapshot = await page.evaluate(() => window.__panelSnapshot());
    report.runB = {
        panelSwaps: diagB.panelSwaps,
        scrollStart: diagB.scrollStart,
        scrollEnd: diagB.scrollEnd,
        blocks: bSnapshot.blocks,
        placeholders: Object.keys(bSnapshot.blocks).filter((k) => bSnapshot.blocks[k].placeholder)
    };
    note('подмен панели: ' + diagB.panelSwaps + '; scrollTop ' + diagB.scrollStart + ' → ' + diagB.scrollEnd +
        '; заглушек: ' + report.runB.placeholders.length);

    // ================= C. вкладка «Модели»: параметры модели =================
    console.log('\n[C] «Модели»: раскрыли параметры модели, затем пересборка при изменении данных');
    await openTab(page, 'models');
    await page.waitForSelector('#ggufDetailPanel .gguf-local-card', { timeout: 20000 }).catch(() => {});
    const cardCount = await page.evaluate(() => document.querySelectorAll('#ggufDetailPanel .gguf-local-card').length);
    check('карточки моделей на диске есть', cardCount > 0, 'карточек: ' + cardCount);
    // Раскрываем первую карточку — как оператор (гидратация профиля по клику).
    await page.evaluate(() => {
        const s = document.querySelector('#ggufDetailPanel .gguf-mp-summary');
        if (s) s.click();
    });
    await page.waitForFunction(() => {
        const el = document.querySelector('.gguf-mp-editor[data-model][data-hydrated="1"]');
        return !!el;
    }, null, { timeout: 20000 }).catch(() => {});
    await page.waitForTimeout(1200);
    const editorBefore = await page.evaluate(() => window.__editorSnapshot());
    const hydratedEditors = editorBefore.filter((e) => e.hydrated);
    const filledBefore = hydratedEditors.reduce((a, e) => a + e.filled, 0);
    const limitsBefore = editorBefore.filter((e) => e.limit).length;
    const cellsSetBefore = editorBefore.reduce((a, e) => a + e.profileCellsSet, 0);
    note('до пересборки: редакторов=' + editorBefore.length + ', гидратировано=' + hydratedEditors.length +
        ', заполненных полей=' + filledBefore + ', «предел окна»=' + limitsBefore + ', ячеек профиля=' + cellsSetBefore);

    const rebuildC = await page.evaluate(() => {
        const st = window.GgufModule.state;
        const be = (st.registeredBackends || []).find((x) => x.id === st.selectedBackendId);
        const statusBefore = be ? be.status : '';
        if (be) be.status = (be.status === 'healthy') ? 'offline' : 'healthy';
        const rebuilt = window.GgufModule.refreshDetailPanel();
        return { rebuilt: rebuilt === true, statusBefore: statusBefore, editors: window.__editorSnapshot() };
    });
    const editorAfter = rebuildC.editors;
    const filledAfter = editorAfter.filter((e) => e.hydrated).reduce((a, e) => a + e.filled, 0);
    const limitsAfter = editorAfter.filter((e) => e.limit).length;
    const cellsSetAfter = editorAfter.reduce((a, e) => a + e.profileCellsSet, 0);
    // Ждём фонового обновления и смотрим, не «вернулось ли» само (проверка «никогда»).
    await page.waitForTimeout(4000);
    const editorLater = await page.evaluate(() => window.__editorSnapshot());
    const filledLater = editorLater.filter((e) => e.hydrated).reduce((a, e) => a + e.filled, 0);
    const limitsLater = editorLater.filter((e) => e.limit).length;
    await restoreStatus(page, rebuildC.statusBefore);
    report.runC = {
        editors: editorBefore.length,
        hydrated: hydratedEditors.length,
        filledBefore: filledBefore,
        filledAfterRebuild: filledAfter,
        filledAfter4s: filledLater,
        limitsBefore: limitsBefore,
        limitsAfterRebuild: limitsAfter,
        limitsAfter4s: limitsLater,
        cellsSetBefore: cellsSetBefore,
        cellsSetAfterRebuild: cellsSetAfter,
        rebuilt: rebuildC.rebuilt,
        models: hydratedEditors.slice(0, 3).map((e) => ({ model: e.model, filled: e.filled, limit: e.limit.slice(0, 40) }))
    };
    report.pageErrors = pageErrors.slice(0, 20);
    note('после пересборки: заполненных полей=' + filledAfter + ' (было ' + filledBefore + '), «предел окна»=' +
        limitsAfter + ' (было ' + limitsBefore + '), ячеек профиля=' + cellsSetAfter + ' (было ' + cellsSetBefore + ')');
    note('через 4 с без клика: заполненных полей=' + filledLater + ', «предел окна»=' + limitsLater);

    await page.screenshot({ path: path.join(ARTIFACT_DIR, 'panel.png'), fullPage: false });

    // ============================== вывод ==================================
    if (EXPECT === 'defect') {
        check('дефект воспроизведён: после пересборки блоки ушли в «Загрузка…»',
            rebuild.placeholders >= 2,
            'заглушек сразу после пересборки: ' + rebuild.placeholders + ' (' +
            Object.keys(rebuild.blocks).filter((k) => rebuild.blocks[k].placeholder).join(',') + ')');
        check('дефект воспроизведён: сами блоки не вернулись (или вернулись не сразу)',
            !back.restored || back.ms > 3000,
            back.restored ? 'вернулись за ' + back.ms + ' мс' : 'НЕ вернулись за ' + RESTORE_WAIT_MS + ' мс');
        check('дефект воспроизведён: фоновых запросов блоков после пересборки нет',
            report.runA.requests.delta.profiles === 0 &&
            report.runA.requests.delta.defaults === 0 &&
            report.runA.requests.delta.catalog === 0,
            JSON.stringify(report.runA.requests.delta));
        check('дефект воспроизведён: параметры модели потеряли значения профиля',
            hydratedEditors.length === 0 || filledAfter < filledBefore || limitsAfter < limitsBefore,
            'полей ' + filledBefore + ' → ' + filledAfter + ', пределов ' + limitsBefore + ' → ' + limitsAfter);
    } else if (EXPECT === 'fixed') {
        check('(1) сразу после пересборки ни одного блока в «Загрузка…»',
            rebuild.placeholders === 0,
            'заглушек: ' + rebuild.placeholders + ' (' +
            Object.keys(rebuild.blocks).filter((k) => rebuild.blocks[k].placeholder).join(',') + ')');
        check('(1) содержимое блоков из кэша не короче прежнего',
            Object.keys(baseline.blocks).every((k) =>
                !baseline.blocks[k].present || rebuild.blocks[k].len >= Math.max(20, baseline.blocks[k].len - 2)),
            JSON.stringify(Object.keys(baseline.blocks).reduce((a, k) => {
                a[k] = baseline.blocks[k].len + '→' + rebuild.blocks[k].len; return a;
            }, {})));
        check('(1) ни одного сэмпла с заглушкой за 1.5 с после пересборки',
            report.runA.blink.placeholderSamples === 0,
            'сэмплов с заглушкой: ' + report.runA.blink.placeholderSamples + '/' + report.runA.blink.samples);
        check('(1) фоновое обновление пришло: профили/дефолты/каталог перезапрошены',
            report.runA.requests.delta.profiles >= 1 &&
            report.runA.requests.delta.defaults >= 1 &&
            report.runA.requests.delta.catalog >= 1,
            JSON.stringify(report.runA.requests.delta));
        check('(1) R688 цел: введённое значение, scrollTop и секции после пересборки',
            afterSnapshot.ctxValue === '9999' && afterSnapshot.scrollTop === baseline.scrollTop,
            'поле=' + afterSnapshot.ctxValue + ', scrollTop ' + baseline.scrollTop + '→' + afterSnapshot.scrollTop +
            ', секции=' + JSON.stringify(afterSnapshot.openSections));
        check('(5) после отрисовки блоки не перерисовываются (нет моргания)',
            blockMutations === 0,
            'подмен DOM за 2 с: ' + blockMutations);
        check('(2) переключение вкладки не показывает заглушку',
            report.runA2.placeholderSamples === 0,
            'сэмплов с заглушкой: ' + report.runA2.placeholderSamples + '/' + report.runA2.samples);
        check('(4) R689 цел: ' + (WINDOW_MS / 1000) + ' с без изменений — 0 подмен, scrollTop цел, блоки живы',
            report.runB.panelSwaps === 0 && report.runB.scrollStart === report.runB.scrollEnd &&
            report.runB.placeholders.length === 0,
            'подмен=' + report.runB.panelSwaps + ', scroll ' + report.runB.scrollStart + '→' + report.runB.scrollEnd +
            ', заглушки=' + JSON.stringify(report.runB.placeholders));
        check('(1) параметры модели сохранили значения профиля после пересборки',
            hydratedEditors.length === 0 ||
            (filledAfter === filledBefore && limitsAfter === limitsBefore && cellsSetAfter === cellsSetBefore),
            'полей ' + filledBefore + '→' + filledAfter + ', пределов ' + limitsBefore + '→' + limitsAfter +
            ', ячеек ' + cellsSetBefore + '→' + cellsSetAfter);
        check('(0) ошибок JS на странице нет (правка не сломала рендер)',
            pageErrors.length === 0,
            pageErrors.slice(0, 3).join(' | '));
    }

    await browser.close();
})().catch((e) => {
    check('выполнение замера', false, String((e && e.stack) || e));
}).finally(() => {
    fs.writeFileSync(path.join(ARTIFACT_DIR, 'report.json'), JSON.stringify(report, null, 2));
    console.log('\nОтчёт: ' + path.relative(ROOT, path.join(ARTIFACT_DIR, 'report.json')));
    console.log('ИТОГ: ' + (exitCode === 0 ? 'ожидания выполнены' : 'ожидания НЕ выполнены'));
    process.exit(exitCode);
});

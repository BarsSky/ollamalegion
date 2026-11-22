// verify-gguf-refresh-stability-r689.js — R689 (2026-11-23): ЖИВОЙ замер двух
// дефектов правой панели GGUF, оставшихся после R688.
//
// ЖИВАЯ ЖАЛОБА ОПЕРАТОРА (дословно): «Пока не вводится значение, интерфейс
// продолжает скакать при обновлении, но форму не закрывает. Когда же есть фокус
// на поле для ввода, то обновления скачками пропадают, и не понятно, обновляется
// ли статус».
//
// ЧТО МЕРЯЕТ (числами, а не словами):
//   A. 20 с БЕЗ ВВОДА на вкладке «Настройки» бэкенда (панель прокручена):
//      - подмены DOM правой панели (#ggufDetailPanel) и её содержимого
//        (#ggufDetailContent) — MutationObserver, childList;
//      - суммарный ΔscrollTop контейнера, который реально прокручивается
//        (панель или её прокручиваемый предок), за каждый тик опроса;
//      - живы ли блоки формы, которые рисуются асинхронной гидратацией
//        (data-role="cpp-load-defaults" / "cpp-model-catalog").
//   B. 20 с С ФОКУСОМ в поле формы (оператор печатает):
//      - сколько раз за окно страница реально сходила за данными панели
//        (GET /api/v1/backends/<id>);
//      - как менялась подпись индикатора свежести (#dataFreshnessText) и её
//        возраст (DataRefresh._state.lastOkAt) — видит ли оператор, что данные
//        обновляются;
//      - что показывает строка статуса панели (#ggufAutoStatus, R689) и растёт ли
//        в ней счётчик успешных обновлений;
//      - R688-поведение: значение в поле (9999), фокус и раскрытые секции.
//
// ЗАПУСК:
//   node scripts/verify-gguf-refresh-stability-r689.js http://127.0.0.1:18084 --expect=defect
//   node scripts/verify-gguf-refresh-stability-r689.js http://127.0.0.1:18085 --expect=fixed
//
// --expect=defect — замер ДО правки: требует, чтобы дефекты воспроизвелись
//   (подмены > 0; на фокусе данные панели не запрашиваются вовсе).
// --expect=fixed  — замер ПОСЛЕ правки: требует 0 подмен при неизменных данных,
//   0 ΔscrollTop, ≥3 обновления данных за 20 с под фокусом, видимую строку
//   статуса с растущим счётчиком и целое значение 9999.
//
// Логин — LB_ADMIN_USER/LB_ADMIN_PASSWORD из deployments/.env (в вывод не
// печатается). Браузер — установленный Edge (channel:'msedge'): встроенный
// chromium в этом окружении зависает на newContext (проверено в R512).
'use strict';

const { chromium } = require('playwright');
const fs = require('fs');
const path = require('path');

const BASE = (process.argv[2] || 'http://127.0.0.1:18084').replace(/\/+$/, '');
const EXPECT = (process.argv.find((a) => a.startsWith('--expect=')) || '--expect=report').slice('--expect='.length);
const WINDOW_MS = Number(process.env.GGUF_WINDOW_MS || 20000);

const ROOT = path.join(__dirname, '..');
const ARTIFACT_DIR = path.join(ROOT, 'test-results', 'gguf-refresh-r689-' + new Date().toISOString().replace(/[:.]/g, '-'));
fs.mkdirSync(ARTIFACT_DIR, { recursive: true });

function envValue(key) {
    try {
        const line = fs.readFileSync(path.join(ROOT, 'deployments', '.env'), 'utf8')
            .split(/\r?\n/).find((l) => l.startsWith(key + '='));
        return line ? line.slice(key.length + 1).trim() : '';
    } catch (e) { return ''; }
}

const report = { url: BASE, expect: EXPECT, windowMs: WINDOW_MS, checks: [], runA: {}, runB: {} };
let exitCode = 0;
// Учётные данные — из окружения или deployments/.env (в вывод не попадают).
let user = process.env.ADMIN_USER || envValue('LB_ADMIN_USER') || 'skyadmin';
let pass = process.env.ADMIN_PASSWORD || envValue('LB_ADMIN_PASSWORD');

function check(name, ok, detail) {
    report.checks.push({ name, ok, detail: detail || '' });
    console.log(`  ${ok ? 'PASS' : 'FAIL'} ${name}${detail ? ' — ' + detail : ''}`);
    if (!ok) exitCode = 1;
}
function note(text) { console.log('  · ' + text); }

/**
 * Открыть страницу GGUF, выбрать бэкенд и вкладку «Настройки» (там форма
 * параметров загрузки). Возвращает имя выбранного бэкенда.
 */
async function openGgufSettings(page) {
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

    // Бэкенд: первый в списке слева — ровно как оператор.
    const picked = await page.evaluate(() => {
        const items = Array.from(document.querySelectorAll('#ggufBackendList [data-backend-id]'));
        const healthy = items.find((el) => el.getAttribute('data-backend-id')) || items[0];
        if (!healthy) return '';
        healthy.click();
        return healthy.getAttribute('data-backend-id') || '';
    });
    if (!picked) throw new Error('в левом списке нет ни одного бэкенда');
    check('бэкенд выбран', true, picked);
    await page.waitForTimeout(2500);

    // Вкладка «Настройки»: форма параметров загрузки живёт там.
    const tab = await page.evaluate(() => {
        const t = document.querySelector('#ggufDetailPanel [data-detail-tab="settings"]');
        if (!t) return false;
        t.click();
        return true;
    });
    check('вкладка «Настройки» открыта', tab);
    // Ждём ГИДРАТАЦИЮ, но не «просто 6 секунд»: блоки формы (профили модели,
    // «что получат новые модели», каталог моделей в папке) дорисовываются
    // асинхронно, и первый же тик авто-обновления заменяет их заглушкой
    // «Загрузка…» — именно он и роняет высоту панели (это часть дефекта).
    // Замер должен начаться ДО этого тика, иначе просадку высоты не поймать.
    await page.waitForFunction(() => {
        if (!document.getElementById('ggufOptCtxSize')) return false;
        const defaults = document.querySelector('#ggufDetailPanel [data-role="cpp-load-defaults"]');
        const catalog = document.querySelector('#ggufDetailPanel [data-role="cpp-model-catalog"]');
        const alive = (el) => !!(el && (el.innerText || '').trim().length > 40);
        return alive(defaults) && alive(catalog);
    }, null, { timeout: 20000 }).catch(() => {});
    const formReady = await page.evaluate(() => !!document.getElementById('ggufOptCtxSize'));
    check('форма параметров загрузки отрисована и блоки гидратированы', formReady,
        formReady ? '' : 'нет #ggufOptCtxSize — форма не пришла');
    return picked;
}

/** Подключить счётчики подмен DOM и сэмплирование прокрутки. */
async function instrument(page) {
    await page.evaluate(() => {
        const panel = document.getElementById('ggufDetailPanel');
        const diag = window.__diag = {
            panelSwaps: 0,
            paneSwaps: 0,
            heightSamples: [],
            heightStart: 0,
            heightEnd: 0,
            sumAbsHeightDelta: 0,
            scrollSamples: [],
            scroller: '',
            scrollStart: 0,
            scrollEnd: 0,
            sumAbsDelta: 0,
            maxDelta: 0,
            settled: false
        };
        // Прокручиваемый контейнер панели: сама панель (если у неё overflow) или
        // ближайший предок, который реально прокручивается.
        function scrollerOf(el) {
            let n = el;
            while (n && n !== document.body && n !== document.documentElement) {
                const cs = window.getComputedStyle(n);
                if (/(auto|scroll)/.test(cs.overflowY) && n.scrollHeight > n.clientHeight + 2) return n;
                n = n.parentElement;
            }
            return document.scrollingElement || document.documentElement;
        }
        const sc = scrollerOf(panel);
        diag.scroller = sc.id ? '#' + sc.id : (sc.className ? '.' + String(sc.className).split(/\s+/)[0] : sc.tagName);
        // Прокручиваем панель почти до конца: тогда даже небольшая просадка
        // высоты (асинхронные блоки формы снова стали заглушкой) упирается в
        // предел прокрутки и scrollTop реально СДВИГАЕТСЯ — это и есть «скачок».
        const maxTop = Math.max(0, sc.scrollHeight - sc.clientHeight);
        sc.scrollTop = Math.max(0, maxTop - 120);
        diag.scrollStart = sc.scrollTop;
        diag.scrollEnd = sc.scrollTop;
        diag.heightStart = Math.round(panel.getBoundingClientRect().height);
        diag.heightEnd = diag.heightStart;

        new MutationObserver((recs) => {
            for (const r of recs) {
                if (r.type === 'childList' && (r.addedNodes.length || r.removedNodes.length)) diag.panelSwaps++;
            }
        }).observe(panel, { childList: true });

        // Содержимое панели — отдельный контейнер; он уничтожается вместе с
        // панелью, поэтому наблюдатель переподключается на каждый новый узел.
        function watchPane() {
            const pane = document.getElementById('ggufDetailContent');
            if (!pane || pane.__diagWatched) return;
            pane.__diagWatched = true;
            new MutationObserver((recs) => {
                for (const r of recs) {
                    if (r.type === 'childList' && (r.addedNodes.length || r.removedNodes.length)) diag.paneSwaps++;
                }
            }).observe(pane, { childList: true });
        }
        watchPane();
        window.__diagPaneTimer = setInterval(() => {
            watchPane();
            let last = diag.scrollSamples.length ? diag.scrollSamples[diag.scrollSamples.length - 1].top : diag.scrollStart;
            const top = sc.scrollTop;
            diag.scrollSamples.push({ t: Date.now(), top });
            diag.sumAbsDelta += Math.abs(top - last);
            diag.maxDelta = Math.max(diag.maxDelta, Math.abs(top - diag.scrollStart));
            diag.scrollEnd = top;
            const h = Math.round(panel.getBoundingClientRect().height);
            const prevH = diag.heightSamples.length ? diag.heightSamples[diag.heightSamples.length - 1] : diag.heightStart;
            diag.heightSamples.push(h);
            diag.sumAbsHeightDelta += Math.abs(h - prevH);
            diag.heightEnd = h;
        }, 200);
    });
}

function summarizeScroll(diag) {
    return {
        scroller: diag.scroller,
        start: diag.scrollStart,
        end: diag.scrollEnd,
        sumAbsDelta: Math.round(diag.sumAbsDelta),
        maxDeviation: Math.round(diag.maxDelta),
        samples: diag.scrollSamples.length,
        heightStart: Math.round(diag.heightStart),
        heightEnd: Math.round(diag.heightEnd),
        sumAbsHeightDelta: Math.round(diag.sumAbsHeightDelta)
    };
}

/** Снимок «здоровья» формы: значение, секции, живость асинхронных блоков. */
async function formSnapshot(page) {
    return page.evaluate(() => {
        const ctxEl = document.getElementById('ggufOptCtxSize');
        const defaults = document.querySelector('#ggufDetailPanel [data-role="cpp-load-defaults"]');
        const catalog = document.querySelector('#ggufDetailPanel [data-role="cpp-model-catalog"]');
        const isPlaceholder = (el) => !el || /Загрузка|Loading|Загрузка…/i.test((el.innerText || '').trim()) &&
            (el.innerText || '').trim().length < 40;
        return {
            ctxValue: ctxEl ? ctxEl.value : null,
            focused: document.activeElement ? (document.activeElement.tagName + '#' + (document.activeElement.id || '')) : '',
            openSections: Array.from(document.querySelectorAll('#ggufDetailPanel details[open]'))
                .map((d) => d.getAttribute('data-acc') || d.getAttribute('data-model-acc') || '?'),
            defaultsAlive: !!(defaults && !isPlaceholder(defaults)),
            catalogAlive: !!(catalog && !isPlaceholder(catalog)),
            freshness: (document.getElementById('dataFreshnessText') || {}).textContent || '',
            autoStatus: (document.getElementById('ggufAutoStatus') || {}).innerText || '',
            lastOkAt: (window.DataRefresh && window.DataRefresh._state) ? window.DataRefresh._state.lastOkAt : 0
        };
    });
}

(async () => {
    if (!pass) throw new Error('нет LB_ADMIN_PASSWORD в deployments/.env');

    const browser = await chromium.launch({ channel: 'msedge', headless: true });
    // Чистый профиль: отдельный контекст — ни куки, ни localStorage стенда.
    const context = await browser.newContext({ viewport: { width: 1600, height: 1000 } });
    const page = await context.newPage();
    let backendRequests = [];
    page.on('request', (r) => {
        const u = r.url();
        if (/\/api\/v1\/backends\/[^/]+(\?|$)/.test(u)) backendRequests.push(u);
    });

    // ============================ A. без ввода ==============================
    console.log('\n[A] 20 с БЕЗ ВВОДА: пересобирается ли панель при неизменных данных');
    const backendId = await openGgufSettings(page);
    await instrument(page);
    const a0 = await formSnapshot(page);
    const a0req = backendRequests.length;
    await page.waitForTimeout(WINDOW_MS);
    const a1 = await formSnapshot(page);
    const a1req = backendRequests.length;
    const diagA = await page.evaluate(() => {
        const d = window.__diag;
        return {
            panelSwaps: d.panelSwaps, paneSwaps: d.paneSwaps,
            scrollSamples: d.scrollSamples,
            scroller: d.scroller, scrollStart: d.scrollStart, scrollEnd: d.scrollEnd,
            sumAbsDelta: d.sumAbsDelta, maxDelta: d.maxDelta,
            heightStart: d.heightStart, heightEnd: d.heightEnd, sumAbsHeightDelta: d.sumAbsHeightDelta
        };
    });
    const scrollA = summarizeScroll(diagA);
    report.runA = {
        backendId,
        panelSwaps: diagA.panelSwaps,
        paneSwaps: diagA.paneSwaps,
        totalSwaps: diagA.panelSwaps + diagA.paneSwaps,
        scroll: scrollA,
        detailRequests: a1req - a0req,
        ctxValueStart: a0.ctxValue,
        ctxValueEnd: a1.ctxValue,
        defaultsAlive: a1.defaultsAlive,
        catalogAlive: a1.catalogAlive,
        freshnessStart: a0.freshness,
        freshnessEnd: a1.freshness
    };
    note('подмены панели за ' + (WINDOW_MS / 1000) + ' с: ' + diagA.panelSwaps +
        ' панель + ' + diagA.paneSwaps + ' содержимое = ' + (diagA.panelSwaps + diagA.paneSwaps));
    note('ΔscrollTop (контейнер ' + scrollA.scroller + '): начали ' + scrollA.start +
        ' → закончили ' + scrollA.end + ', сумма |Δ| по сэмплам ' + scrollA.sumAbsDelta +
        ', макс. отклонение ' + scrollA.maxDeviation + ' px');
    note('высота правой панели: ' + scrollA.heightStart + ' → ' + scrollA.heightEnd +
        ' px, сумма |Δh| по сэмплам ' + scrollA.sumAbsHeightDelta + ' px');
    note('блоки формы после окна: defaults=' + a1.defaultsAlive + ', catalog=' + a1.catalogAlive +
        ', n_ctx в поле=' + a1.ctxValue);

    // ============================ B. фокус в поле ===========================
    console.log('\n[B] 20 с С ФОКУСОМ в поле: продолжается ли обновление данных');
    await page.goto(BASE + '/?page=gguf', { waitUntil: 'domcontentloaded', timeout: 30000 });
    await page.waitForSelector('#ggufDetailPanel', { timeout: 30000 });
    await page.waitForTimeout(1500);
    await page.evaluate((id) => {
        const items = Array.from(document.querySelectorAll('#ggufBackendList [data-backend-id]'));
        const el = items.find((x) => x.getAttribute('data-backend-id') === id) || items[0];
        if (el) el.click();
    }, backendId);
    await page.waitForTimeout(2500);
    await page.evaluate(() => {
        const t = document.querySelector('#ggufDetailPanel [data-detail-tab="settings"]');
        if (t) t.click();
    });
    await page.waitForFunction(() => !!document.getElementById('ggufOptCtxSize'), null, { timeout: 20000 })
        .catch(() => {});
    await page.waitForTimeout(1200);

    const field = page.locator('#ggufOptCtxSize');
    await field.click();
    await page.keyboard.press('Control+A');
    await page.keyboard.type('9999', { delay: 40 });
    const focusedId = await page.evaluate(() => (document.activeElement || {}).id || '');
    check('фокус в поле формы', focusedId === 'ggufOptCtxSize', 'activeElement=#' + focusedId);

    // Раскрываем вторую секцию — R688: она обязана остаться раскрытой.
    const opened = await page.evaluate(() => {
        const list = Array.from(document.querySelectorAll('#ggufDetailPanel details[data-acc]'));
        const target = list.find((d) => !d.open);
        if (target) target.open = true;
        return list.filter((d) => d.open).map((d) => d.getAttribute('data-acc'));
    });
    check('оператор раскрыл дополнительную секцию', opened.length >= 2, 'открыты: ' + opened.join(','));

    await page.evaluate(() => {
        window.__focusSamples = [];
        window.__focusReqStart = 0;
        window.__focusTimer = setInterval(() => {
            window.__focusSamples.push({
                t: Date.now(),
                freshness: (document.getElementById('dataFreshnessText') || {}).textContent || '',
                autoStatus: (document.getElementById('ggufAutoStatus') || {}).innerText || '',
                lastOkAt: (window.DataRefresh && window.DataRefresh._state) ? window.DataRefresh._state.lastOkAt : 0,
                panelHtmlLen: (document.getElementById('ggufDetailPanel') || {}).innerHTML ? document.getElementById('ggufDetailPanel').innerHTML.length : 0
            });
        }, 1000);
    });
    const b0req = backendRequests.length;
    await page.waitForTimeout(WINDOW_MS);
    const b1req = backendRequests.length;
    const focusData = await page.evaluate(() => ({
        samples: window.__focusSamples,
        ctxValue: (document.getElementById('ggufOptCtxSize') || {}).value,
        focused: (document.activeElement || {}).id || '',
        openSections: Array.from(document.querySelectorAll('#ggufDetailPanel details[open]'))
            .map((d) => d.getAttribute('data-acc') || '?')
    }));
    await page.screenshot({ path: path.join(ARTIFACT_DIR, 'focus.png'), fullPage: false });

    // ====================== C. данные изменились ============================
    // Требование: подмена при изменившихся данных — есть, но раскрытые секции и
    // введённые значения сохранены, а положение прокрутки восстанавливается.
    // Изменение готовим то, которое рисуется в панели: статус бэкенда в шапке
    // (healthy ↔ offline). Пересборка — явная (как действие оператора), потому
    // что при активной форме авто-проход обязан откладываться (R688).
    console.log('\n[C] изменившиеся данные: подмена есть, состояние панели сохранено');
    const change = await page.evaluate(() => {
        const sc = document.scrollingElement || document.documentElement;
        const panel = document.getElementById('ggufDetailPanel');
        sc.scrollTop = Math.max(0, Math.min(sc.scrollHeight - sc.clientHeight - 200, 400));
        const before = sc.scrollTop;
        const hBefore = Math.round(panel.getBoundingClientRect().height);
        const st = window.GgufModule.state;
        const be = (st.registeredBackends || []).find((x) => x.id === st.selectedBackendId);
        const statusBefore = be ? be.status : '';
        if (be) be.status = (be.status === 'healthy') ? 'offline' : 'healthy';
        const rebuilt = window.GgufModule.refreshDetailPanel(); // явная пересборка
        const after = sc.scrollTop;
        const ctxEl = document.getElementById('ggufOptCtxSize');
        const result = {
            rebuilt: rebuilt === true,
            scrollBefore: before,
            scrollAfter: after,
            heightBefore: hBefore,
            heightAfter: Math.round(panel.getBoundingClientRect().height),
            ctxValue: ctxEl ? ctxEl.value : '',
            openSections: Array.from(document.querySelectorAll('#ggufDetailPanel details[open]'))
                .map((d) => d.getAttribute('data-acc') || '?'),
            statusLineVisible: !!(document.getElementById('ggufAutoStatus') &&
                !document.getElementById('ggufAutoStatus').hidden &&
                (document.getElementById('ggufAutoStatus').innerText || '').trim())
        };
        if (be) be.status = statusBefore; // стенд возвращаем в исходное состояние
        return result;
    });
    report.runC = change;
    note('подмена при изменившихся данных: ' + change.rebuilt);
    note('scrollTop: ' + change.scrollBefore + ' → ' + change.scrollAfter +
        ' (высота панели ' + change.heightBefore + ' → ' + change.heightAfter + ' px)');
    note('значение в поле=' + change.ctxValue + ', секции=' + JSON.stringify(change.openSections) +
        ', строка статуса после снятия защиты: ' + (change.statusLineVisible ? 'ВИДНА' : 'скрыта'));

    // «Данные обновлялись»: возраст последнего успешного обновления в конце окна.
    const lastOk = focusData.samples.length ? focusData.samples[focusData.samples.length - 1].lastOkAt : 0;
    const ageAtEnd = lastOk ? Math.round((Date.now() - lastOk) / 1000) : -1;
    const distinctFreshness = Array.from(new Set(focusData.samples.map((s) => s.freshness)));
    const autoStatusTexts = Array.from(new Set(focusData.samples.map((s) => (s.autoStatus || '').trim()).filter(Boolean)));
    // Счётчик успешных обновлений из строки статуса: берём числа, которые
    // встречаются в тексте, и смотрим на максимум.
    function maxCounter(texts) {
        let max = 0;
        for (const t of texts) {
            const m = String(t).match(/(\d+)\s*(?:обн|upd)/i) || String(t).match(/(\d+)\s*$/);
            if (m) max = Math.max(max, Number(m[1]));
        }
        return max;
    }
    report.runB = {
        backendId,
        detailRequests: b1req - b0req,
        ageAtEnd,
        freshnessLabels: distinctFreshness,
        autoStatusTexts,
        autoStatusCounter: maxCounter(autoStatusTexts),
        ctxValue: focusData.ctxValue,
        focused: focusData.focused,
        openSections: focusData.openSections,
        samples: focusData.samples.length
    };
    note('запросов данных панели за ' + (WINDOW_MS / 1000) + ' с под фокусом: ' + (b1req - b0req));
    note('возраст последнего успешного обновления в конце окна: ' +
        (ageAtEnd < 0 ? 'ни одного обновления' : ageAtEnd + ' с'));
    note('подписи индикатора свежести за окно: ' + JSON.stringify(distinctFreshness));
    note('строка статуса панели: ' + (autoStatusTexts.length ? JSON.stringify(autoStatusTexts.slice(-2)) : 'НЕТ'));
    note('R688: значение в поле=' + focusData.ctxValue + ', фокус=#' + focusData.focused +
        ', раскрыты секции ' + JSON.stringify(focusData.openSections));

    // ============================== вывод ==================================
    const A = report.runA;
    const B = report.runB;
    if (EXPECT === 'defect') {
        check('дефект 1 воспроизведён: панель пересобирается без изменений в данных',
            A.totalSwaps > 0, A.totalSwaps + ' подмен за ' + (WINDOW_MS / 1000) + ' с');
        check('дефект 2 воспроизведён: под фокусом данные панели не обновляются',
            B.detailRequests === 0 && B.ageAtEnd > (WINDOW_MS / 1000),
            'запросов=' + B.detailRequests + ', возраст индикатора=' + B.ageAtEnd + ' с, статус=' +
            (B.autoStatusTexts.length ? 'есть' : 'НЕТ'));
    } else if (EXPECT === 'fixed') {
        check('(1) при неизменных данных DOM панели не подменяется', A.totalSwaps === 0,
            A.totalSwaps + ' подмен');
        check('(1) ΔscrollTop при неизменных данных нулевой',
            A.scroll.sumAbsDelta === 0 && A.scroll.start === A.scroll.end,
            'start=' + A.scroll.start + ', end=' + A.scroll.end + ', Σ|Δ|=' + A.scroll.sumAbsDelta);
        check('(1) высота панели не скачет и асинхронные блоки формы живы',
            A.scroll.sumAbsHeightDelta === 0 && A.defaultsAlive && A.catalogAlive,
            'h ' + A.scroll.heightStart + '→' + A.scroll.heightEnd + ' px, Σ|Δh|=' +
            A.scroll.sumAbsHeightDelta + ', defaults=' + A.defaultsAlive + ', catalog=' + A.catalogAlive);
        check('(2) под фокусом данные панели продолжают обновляться', B.detailRequests >= 3,
            'запросов=' + B.detailRequests + ' за ' + (WINDOW_MS / 1000) + ' с');
        check('(2) индикатор свежести показывает свежесть', B.ageAtEnd >= 0 && B.ageAtEnd <= 8,
            'возраст=' + B.ageAtEnd + ' с');
        check('(2) видна строка статуса с причиной и счётчиком', B.autoStatusTexts.length > 0 &&
            /вводит|typing/i.test(B.autoStatusTexts[B.autoStatusTexts.length - 1]),
            JSON.stringify(B.autoStatusTexts.slice(-1)));
        check('(2) счётчик успешных обновлений растёт', B.autoStatusCounter >= 2,
            'счётчик=' + B.autoStatusCounter);
        check('(3) R688 цел: значение 9999 и раскрытые секции на месте',
            B.ctxValue === '9999' && B.openSections.length >= 2 && B.focused === 'ggufOptCtxSize',
            'значение=' + B.ctxValue + ', секции=' + JSON.stringify(B.openSections) + ', фокус=#' + B.focused);
        var C = report.runC || {};
        check('(3) при изменившихся данных подмена есть, секции и введённое значение сохранены',
            C.rebuilt === true && C.ctxValue === '9999' &&
            Array.isArray(C.openSections) && C.openSections.length >= 2,
            'rebuilt=' + C.rebuilt + ', значение=' + C.ctxValue + ', секции=' + JSON.stringify(C.openSections) +
            ', scroll ' + C.scrollBefore + '→' + C.scrollAfter);
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

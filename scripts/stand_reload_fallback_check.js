// stand_reload_fallback_check.js — R517 (2026-11-19): живая проверка того, что
// «применить профиль» на узел, где модель только ЛЕЖИТ НА ДИСКЕ, реально её
// загружает (fallback reload→load), а не отделывается «skipped».
//
// ЗАЧЕМ ОТДЕЛЬНЫЙ СКРИПТ. Go-тест (internal/api/profile_apply_reload_fallback_r517_test.go)
// доказывает логику на подставном воркере. Здесь проверяется ЖИВОЙ путь:
// браузерная сессия → реальный балансер → реальный воркер, и результат виден в
// /api/models (модель появилась в loaded). Стенд специально пустой: 0 загруженных
// моделей — до R517 такой apply возвращал skipped и ничего не грузил.
//
// ВАЖНО: после проверки модель выгружается отдельно (см. команду в отчёте),
// скрипт лишь печатает имя модели и backend, чтобы это можно было сделать.
//
// ЗАПУСК: node scripts/stand_reload_fallback_check.js [baseUrl]
const { chromium } = require('playwright');
const fs = require('fs');
const path = require('path');

const BASE_URL = process.argv[2] || 'http://127.0.0.1:18083';
const ARTIFACT_DIR = path.join(process.cwd(), 'test-results',
    'stand-reload-' + new Date().toISOString().replace(/[:.]/g, '-'));
fs.mkdirSync(ARTIFACT_DIR, { recursive: true });

const results = { url: BASE_URL, timestamp: new Date().toISOString(), steps: [], model: '', backendId: '' };
let exitCode = 0;
function record(name, ok, detail) {
    results.steps.push({ name, ok, detail: detail || '' });
    console.log(`  ${ok ? 'PASS' : 'FAIL'} ${name}${detail ? ' — ' + detail : ''}`);
    if (!ok) exitCode = 1;
}
function envValue(key) {
    try {
        const line = fs.readFileSync(path.join(__dirname, '..', 'deployments', '.env'), 'utf8')
            .split(/\r?\n/).find((l) => l.startsWith(key + '='));
        return line ? line.slice(key.length + 1).trim() : '';
    } catch (e) { return ''; }
}
const ADMIN_USER = process.env.ADMIN_USER || envValue('LB_ADMIN_USER') || 'admin';
const ADMIN_PASSWORD = process.env.ADMIN_PASSWORD || envValue('LB_ADMIN_PASSWORD');

async function main() {
    if (!ADMIN_PASSWORD) throw new Error('не найден пароль администратора (LB_ADMIN_PASSWORD в deployments/.env)');
    const browser = await chromium.launch({ channel: 'msedge', headless: true });
    const context = await browser.newContext({ viewport: { width: 1400, height: 900 }, defaultTimeout: 30000 });
    const page = await context.newPage();

    await page.goto(BASE_URL + '/', { waitUntil: 'domcontentloaded', timeout: 30000 });
    const loginStatus = await page.evaluate(async ({ user, pass }) => {
        const res = await fetch('/api/v1/auth/login', {
            method: 'POST', headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ name: user, password: pass })
        });
        return res.status;
    }, { user: ADMIN_USER, pass: ADMIN_PASSWORD });
    record('вход администратора', loginStatus === 200, 'HTTP ' + loginStatus);

    // Перезагрузка: webui/js/modules/auth.js не отправляет запросы, пока /auth/me
    // не подтвердил вход в самой странице.
    await page.goto(BASE_URL + '/?page=reference', { waitUntil: 'domcontentloaded', timeout: 30000 });
    await page.waitForTimeout(2000);

    const session = await page.evaluate(async () => {
        const me = await fetch('/api/v1/auth/me').then((r) => r.json()).catch(() => ({}));
        const cfg = window.WEBUI_CONFIG || {};
        return { csrf: me.csrf || '', token: cfg.API_TOKEN || '' };
    });

    const facts = await page.evaluate(async (token) => {
        const headers = token ? { 'X-API-Token': token } : {};
        const [cat, loaded, tpls] = await Promise.all([
            fetch('/api/v1/cppworker/model-catalog', { headers }).then((r) => r.json()).catch(() => ({})),
            fetch('/api/v1/cluster/models/loaded', { headers }).then((r) => r.json()).catch(() => ({})),
            fetch('/api/v1/reference/templates', { headers }).then((r) => r.json()).catch(() => ({}))
        ]);
        return {
            backendId: cat.backendId || '',
            files: (cat.files || []).map((f) => f.name),
            loadedCount: loaded.count || 0,
            loadedNames: (loaded.models || []).map((m) => m.name),
            // R518: берём шаблон с наименьшим окном — на 8-гиговой карте он
            // загружается быстро и не уводит стенд в partial_offload на минуты.
            templateId: ((tpls.items || []).filter(function (it) {
                return it && it.params && !Array.isArray(it.params);
            }).sort(function (a, b) {
                var ca = Number((a.params || {}).contextLength || 1e9);
                var cb = Number((b.params || {}).contextLength || 1e9);
                return ca - cb;
            })[0] || {}).id || ''
        };
    }, session.token);

    record('каталог называет узел', !!facts.backendId, 'backendId=' + facts.backendId);
    record('на стенде 0 загруженных моделей (самый честный случай)', facts.loadedCount === 0, 'загружено: ' + facts.loadedCount);
    const loadedNorm = new Set(facts.loadedNames.map((n) => String(n).toLowerCase().replace(/\.gguf$/, '')));
    const model = facts.files.map((n) => n.replace(/\.gguf$/i, '')).filter((n) => !loadedNorm.has(n.toLowerCase()))[0] || '';
    record('есть модель только на диске и шаблон для применения', !!model && !!facts.templateId,
        'модель=' + model + ', шаблон=' + facts.templateId);
    results.model = model;
    results.backendId = facts.backendId;
    if (!model || !facts.templateId) throw new Error('нечего применять: нет модели на диске или шаблона');

    // Применение профиля на узел, где модель не загружена. Это мутация — нужен CSRF.
    const apply = await page.evaluate(async ({ templateId, model, backendId, csrf, token }) => {
        const headers = { 'Content-Type': 'application/json' };
        if (token) headers['X-API-Token'] = token;
        if (csrf) headers['X-CSRF-Token'] = csrf;
        const res = await fetch('/api/v1/reference/templates/' + encodeURIComponent(templateId) + '/apply', {
            method: 'POST', headers,
            body: JSON.stringify({ model, backendId, mode: 'profile+reload', confirmNonFeasible: true })
        });
        const text = await res.text();
        let body = {};
        try { body = JSON.parse(text); } catch (e) { body = { raw: text.slice(0, 400) }; }
        return { status: res.status, body };
    }, { templateId: facts.templateId, model, backendId: facts.backendId, csrf: session.csrf, token: session.token });

    const backends = (apply.body && apply.body.backends) || [];
    const messages = backends.map((b) => b.backendId + '=' + b.status + ':' + (b.message || '')).join(' | ');
    record('apply принят (200/202)', apply.status === 200 || apply.status === 202, 'HTTP ' + apply.status + ' ' + messages.slice(0, 300));
    record('бэкенд не «skipped» — загрузка запущена', backends.some((b) => b.status !== 'skipped' && b.status !== 'error'),
        messages || JSON.stringify(apply.body).slice(0, 300));

    // Факт загрузки: /api/models у воркера через балансер (нужен токен).
    let loaded = false;
    let loadedInfo = '';
    for (let i = 0; i < 30; i++) {
        await page.waitForTimeout(5000);
        // Источник факта — тот же, что у страницы: /api/v1/cluster/models/loaded
        // (у балансера нет «своего» /api/v1/models; этот путь отдаёт воркер).
        const cluster = await page.evaluate(async (token) => {
            const headers = token ? { 'X-API-Token': token } : {};
            return fetch('/api/v1/cluster/models/loaded', { headers }).then((r) => r.json()).catch(() => ({}));
        }, session.token);
        const list = (cluster && (cluster.models || [])) || [];
        const hit = list.filter((m) => String(m.name || '').toLowerCase().replace(/\.gguf$/, '') === model.toLowerCase())[0];
        if (hit) {
            loaded = true;
            loadedInfo = hit.name + ' на ' + (hit.backendId || '?') + ', ctx=' + (hit.contextLength || '?');
            break;
        }
        loadedInfo = 'ждём: в кластере загружено ' + list.length;
    }
    record('модель РЕАЛЬНО загрузилась (fallback reload→load)', loaded, loadedInfo);
    await page.screenshot({ path: path.join(ARTIFACT_DIR, 'apply-result.png') });

    await browser.close();
    fs.writeFileSync(path.join(ARTIFACT_DIR, 'report.json'), JSON.stringify(results, null, 2));
    const failed = results.steps.filter((s) => !s.ok).length;
    console.log(`\nИТОГ: ${results.steps.length - failed} из ${results.steps.length} PASS; артефакты: ${ARTIFACT_DIR}`);
    console.log('ВЫГРУЗИТЬ МОДЕЛЬ: docker exec ol-stack-cppworker-gpu wget -q -O- --post-data= "http://127.0.0.1:18092/api/models/unload?name=' + results.model + '" || true');
    process.exit(exitCode);
}

main().catch((e) => {
    console.error('FATAL: ' + (e && e.stack || e));
    process.exit(2);
});

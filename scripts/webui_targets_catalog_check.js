// webui_targets_catalog_check.js — R516 (2026-11-19): живая проверка того, что
// «где поднять» больше не ждёт загруженную модель.
//
// ЖАЛОБА ОПЕРАТОРА (§2.7 разбора): селектор целей брал только
// `/api/v1/cluster/models/loaded`, поэтому при пустой VRAM все кнопки
// «Применить» были неактивны, хотя сервер разрешает применение на узел, где файл
// модели ЛЕЖИТ НА ДИСКЕ (handlers_reference_apply.go:294-324,368-391).
//
// Проверяется на живом стенде с НУЛЁМ загруженных моделей — это самый честный
// случай: раньше он гарантированно давал `no_loaded_target`.
//   1. каталог называет узел (R516 в ответе API) и отдаёт файлы;
//   2. страница предлагает модель, которой нет в /cluster/models/loaded;
//   3. узел из каталога есть в списке бэкендов с пометкой «файл на диске»;
//   4. «Применить» активна и предупреждает «начнётся с её загрузки».
//
// ЗАПУСК: node scripts/webui_targets_catalog_check.js [baseUrl]
const { chromium } = require('playwright');
const fs = require('fs');
const path = require('path');

const BASE_URL = process.argv[2] || 'http://127.0.0.1:18083';
const ARTIFACT_DIR = path.join(process.cwd(), 'test-results',
    'webui-targets-' + new Date().toISOString().replace(/[:.]/g, '-'));
fs.mkdirSync(ARTIFACT_DIR, { recursive: true });

const results = { url: BASE_URL, timestamp: new Date().toISOString(), steps: [] };
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
    const context = await browser.newContext({ viewport: { width: 1600, height: 1000 } });
    const page = await context.newPage();
    page.on('dialog', function (d) { d.accept(); });

    await page.goto(BASE_URL + '/', { waitUntil: 'domcontentloaded', timeout: 30000 });
    const loginStatus = await page.evaluate(async ({ user, pass }) => {
        const res = await fetch('/api/v1/auth/login', {
            method: 'POST', headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ name: user, password: pass })
        });
        return res.status;
    }, { user: ADMIN_USER, pass: ADMIN_PASSWORD });
    record('вход администратора', loginStatus === 200, 'HTTP ' + loginStatus);

    // ВАЖНО: webui/js/modules/auth.js оборачивает fetch и НЕ отправляет запрос,
    // пока состояние входа в самой странице не подтверждено (/auth/me). Поэтому
    // после логина перезагружаем документ: иначе запросы ниже получают локальный
    // 401 «запрос не отправлен без сессии» и проверки читают пустоту.
    await page.goto(BASE_URL + '/', { waitUntil: 'domcontentloaded', timeout: 30000 });
    await page.waitForTimeout(1500);

    // Админские API требуют токен из WEBUI_CONFIG: одна сессия (cookie) их не
    // открывает — это защита R223, а не отсутствие прав у нашего логина.
    const apiToken = await page.evaluate(() => (window.WEBUI_CONFIG && window.WEBUI_CONFIG.API_TOKEN) || '');
    const facts = await page.evaluate(async (token) => {
        const headers = token ? { 'X-API-Token': token } : {};
        const [cat, loaded] = await Promise.all([
            fetch('/api/v1/cppworker/model-catalog', { headers }).then((r) => r.json()).catch(() => ({})),
            fetch('/api/v1/cluster/models/loaded', { headers }).then((r) => r.json()).catch(() => ({}))
        ]);
        return {
            catalogBackendId: cat.backendId || '',
            files: (cat.files || []).map((f) => f.name),
            loadedCount: loaded.count || 0,
            loadedNames: (loaded.models || []).map((m) => m.name)
        };
    }, apiToken);
    record('каталог называет узел (R516)', !!facts.catalogBackendId, 'backendId=' + facts.catalogBackendId);
    record('каталог отдаёт файлы моделей', facts.files.length > 0, 'файлов: ' + facts.files.length);
    record('на стенде нет загруженных моделей (самый честный случай)', facts.loadedCount === 0,
        'загружено: ' + facts.loadedCount);

    const loadedNorm = new Set(facts.loadedNames.map((n) => String(n).toLowerCase().replace(/\.gguf$/, '')));
    const diskModel = facts.files.map((n) => n.replace(/\.gguf$/i, ''))
        .filter((n) => !loadedNorm.has(n.toLowerCase()))[0] || '';

    await page.goto(BASE_URL + '/?page=reference', { waitUntil: 'domcontentloaded', timeout: 30000 });
    await page.waitForTimeout(2500);
    if ((await page.locator('#reference-page.active').count()) === 0) throw new Error('#reference-page не активна');
    await page.locator('[data-ref-tab="templates"]').first().click();
    await page.waitForTimeout(800);

    const toggle = page.locator('[data-ref-action="toggle-tpl"]').first();
    if ((await toggle.count()) === 0) throw new Error('на вкладке «Шаблоны» нет ни одного шаблона — проверять нечего');
    await toggle.click();
    await page.waitForTimeout(900);

    const modelSelect = page.locator('[data-ref-tpl="model"]').first();
    const modelOptions = await modelSelect.locator('option').allTextContents();
    record('в выборе модели есть модель, которой нет в /cluster/models/loaded',
        !!diskModel && modelOptions.some((o) => o.trim() === diskModel),
        'модель=' + diskModel + ', вариантов: ' + modelOptions.length);

    if (diskModel) {
        await modelSelect.selectOption(diskModel);
        await page.waitForTimeout(1200);
        const backendSelect = page.locator('[data-ref-tpl="backendId"]').first();
        await backendSelect.selectOption(facts.catalogBackendId);
        await page.waitForTimeout(1200);
        const optionLabels = await page.locator('[data-ref-tpl="backendId"] option').allTextContents();
        record('узел каталога подписан «файл на диске»',
            optionLabels.some((o) => o.indexOf(facts.catalogBackendId + ' (') !== -1 && o.indexOf('не загружена') !== -1),
            optionLabels.join(' | ').slice(0, 200));

        const applyBtn = page.locator('[data-ref-action="apply"]').first();
        const isDisabled = await applyBtn.isDisabled();
        record('«Применить» активна на узле с файлом на диске', !isDisabled, 'disabled=' + isDisabled);
        const hintCount = await page.locator('.reference-apply-hint').count();
        const hint = hintCount ? (await page.locator('.reference-apply-hint').first().innerText()).trim() : '';
        record('предупреждение «начнётся с загрузки» показано', /начнётся с её загрузки/.test(hint), hint || 'подсказки нет');
        await page.screenshot({ path: path.join(ARTIFACT_DIR, 'targets-catalog.png'), fullPage: false });
    }

    await browser.close();
    fs.writeFileSync(path.join(ARTIFACT_DIR, 'report.json'), JSON.stringify(results, null, 2));
    const failed = results.steps.filter((s) => !s.ok).length;
    console.log(`\nИТОГ: ${results.steps.length - failed} из ${results.steps.length} PASS; артефакты: ${ARTIFACT_DIR}`);
    process.exit(exitCode);
}

main().catch((e) => {
    console.error('FATAL: ' + (e && e.stack || e));
    process.exit(2);
});

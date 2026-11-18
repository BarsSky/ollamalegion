// webui_router_check.js — R513 (2026-11-19): факты о трёх правках WebUI после
// пересборки образа: адрес страницы (F5), отсутствие дубля «Диагностики» и
// ширина формы настроек.
//
// ЗАЧЕМ ОТДЕЛЬНЫЙ СКРИПТ. Статические тесты (webui/tests/*r513*.test.js) доказывают,
// что код и разметка такие, как задумано, но не доказывают, что ОБРАЗ собрался из
// этого кода и что браузер ведёт себя как ожидается. Три жалобы оператора были
// именно про поведение в браузере.
//
// ЗАПУСК:
//   node scripts/webui_router_check.js [baseUrl]
// Логин — из deployments/.env (LB_ADMIN_USER/LB_ADMIN_PASSWORD); пароль не печатается.
// ВЫХОД: test-results/webui-router-<ts>/{report.json,*.png}; код 1 при провале.
//
// Браузер: установленный Edge (channel:'msedge') — встроенный chromium в этом
// окружении зависает на newContext (проверено ранее).
const { chromium } = require('playwright');
const fs = require('fs');
const path = require('path');

const BASE_URL = process.argv[2] || 'http://127.0.0.1:18083';
const ARTIFACT_DIR = path.join(process.cwd(), 'test-results',
    'webui-router-' + new Date().toISOString().replace(/[:.]/g, '-'));
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
        const envFile = path.join(__dirname, '..', 'deployments', '.env');
        const line = fs.readFileSync(envFile, 'utf8').split(/\r?\n/).find((l) => l.startsWith(key + '='));
        return line ? line.slice(key.length + 1).trim() : '';
    } catch (e) { return ''; }
}

const ADMIN_USER = process.env.ADMIN_USER || envValue('LB_ADMIN_USER') || 'admin';
const ADMIN_PASSWORD = process.env.ADMIN_PASSWORD || envValue('LB_ADMIN_PASSWORD');

async function main() {
    if (!ADMIN_PASSWORD) throw new Error('не найден пароль администратора (LB_ADMIN_PASSWORD в deployments/.env)');
    const browser = await chromium.launch({ channel: 'msedge', headless: true });
    const context = await browser.newContext({ viewport: { width: 1920, height: 1080 } });
    const page = await context.newPage();

    // --- Вход (same-origin, чтобы cookie сессии попала в контекст) ------------
    await page.goto(BASE_URL + '/', { waitUntil: 'domcontentloaded', timeout: 30000 });
    const loginStatus = await page.evaluate(async ({ user, pass }) => {
        const res = await fetch('/api/v1/auth/login', {
            method: 'POST', headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ name: user, password: pass })
        });
        return res.status;
    }, { user: ADMIN_USER, pass: ADMIN_PASSWORD });
    record('вход администратора', loginStatus === 200, 'HTTP ' + loginStatus);

    // --- F2: один пункт «Диагностика» ---------------------------------------
    await page.goto(BASE_URL + '/', { waitUntil: 'domcontentloaded', timeout: 30000 });
    await page.waitForTimeout(1500);
    const diag = await page.evaluate(() => {
        const all = Array.from(document.querySelectorAll('.nav-item[data-page="diagnostics"]'));
        const footer = all.filter((el) => el.classList.contains('sidebar-diag-link'));
        return { total: all.length, footer: footer.length, visible: all.filter((el) => el.offsetParent !== null).length };
    });
    record('«Диагностика» объявлена один раз', diag.total === 1, `пунктов: ${diag.total}`);
    record('единственная «Диагностика» видна без прокрутки', diag.footer === 1 && diag.visible === 1,
        `ярлыков в подвале: ${diag.footer}, видимых: ${diag.visible}`);

    // --- F3: адрес страницы и F5 --------------------------------------------
    await page.goto(BASE_URL + '/?page=queue', { waitUntil: 'domcontentloaded', timeout: 30000 });
    await page.waitForTimeout(1500);
    const fromUrl = await page.evaluate(() => {
        const el = document.getElementById('queue-page');
        return { active: !!(el && el.classList.contains('active')), href: location.href };
    });
    record('ссылка ?page=queue открывает «Очередь»', fromUrl.active, fromUrl.href);

    await page.locator('.nav-item[data-page="logs"]').first().click();
    await page.waitForTimeout(800);
    const afterClick = await page.evaluate(() => ({
        active: (document.getElementById('logs-page') || {}).classList
            ? document.getElementById('logs-page').classList.contains('active') : false,
        page: new URLSearchParams(location.search).get('page')
    }));
    record('клик по «Логам» пишет страницу в адрес', afterClick.active && afterClick.page === 'logs',
        '?page=' + afterClick.page);

    // Настоящий F5: перезагрузка документа и проверка, что страница осталась.
    await page.reload({ waitUntil: 'domcontentloaded', timeout: 30000 });
    await page.waitForTimeout(1500);
    const afterReload = await page.evaluate(() => {
        const el = document.getElementById('logs-page');
        return { active: !!(el && el.classList.contains('active')), page: new URLSearchParams(location.search).get('page') };
    });
    record('F5 возвращает на ту же страницу', afterReload.active && afterReload.page === 'logs',
        'после перезагрузки: ?page=' + afterReload.page + ', active=' + afterReload.active);
    await page.screenshot({ path: path.join(ARTIFACT_DIR, 'after-f5-logs.png') });

    // --- F1: форма настроек занимает карточку -------------------------------
    await page.goto(BASE_URL + '/?page=settings', { waitUntil: 'domcontentloaded', timeout: 30000 });
    await page.waitForTimeout(2000);
    const width = await page.evaluate(() => {
        const form = document.querySelector('#settings-page .settings-form') || document.getElementById('settingsForm');
        if (!form) return null;
        const card = form.closest('.card');
        return {
            form: Math.round(form.getBoundingClientRect().width),
            card: card ? Math.round(card.getBoundingClientRect().width) : 0,
            viewport: window.innerWidth
        };
    });
    // На 1920 карточка ≈1612 px, содержимое было 600 px (≈1012 px пустоты).
    // Требование: форма занимает не меньше 85 % карточки.
    const ratio = width && width.card ? width.form / width.card : 0;
    record('форма настроек занимает карточку (нет узкой полосы)', !!width && ratio >= 0.85,
        width ? `форма ${width.form} px из карточки ${width.card} px (${Math.round(ratio * 100)} %)` : 'форма не найдена');
    await page.screenshot({ path: path.join(ARTIFACT_DIR, 'settings-width.png'), fullPage: false });

    await browser.close();
    fs.writeFileSync(path.join(ARTIFACT_DIR, 'report.json'), JSON.stringify(results, null, 2));
    const failed = results.steps.filter((s) => !s.ok).length;
    console.log(`\nИТОГ: ${results.steps.length - failed} из ${results.steps.length} PASS; артефакты: ${ARTIFACT_DIR}`);
    if (exitCode !== 0) console.error(`ПРОВАЛОВ: ${failed}`);
    process.exit(exitCode);
}

main().catch((e) => {
    console.error('FATAL: ' + (e && e.stack || e));
    process.exit(2);
});

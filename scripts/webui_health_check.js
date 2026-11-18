// webui_health_check.js — R514 (2026-11-19): живая проверка страницы «Состояние»
// после пересборки образа: рамки карточек и секций РИСУЮТСЯ, страница отдаёт
// содержимое (не пустая), переменные оформления не «висят» неопределёнными.
//
// ЗАЧЕМ ОТДЕЛЬНО ОТ СТАТИЧЕСКОГО ТЕСТА. webui/tests/health-css-tokens.test.js
// доказывает, что разметка ссылается только на определённые переменные. Здесь
// проверяется ФАКТИВНЫЙ рендер: браузер вычислил border-width/border-color, а не
// отбросил объявление (как было с var(--border-color) — рамок не было вовсе).
//
// ЗАПУСК: node scripts/webui_health_check.js [baseUrl]
// Логин берётся из deployments/.env (LB_ADMIN_USER/LB_ADMIN_PASSWORD); пароль не печатается.
// ВЫХОД: test-results/webui-health-<ts>/{report.json,*.png}; код 1 при провале.
const { chromium } = require('playwright');
const fs = require('fs');
const path = require('path');

const BASE_URL = process.argv[2] || 'http://127.0.0.1:18083';
const ARTIFACT_DIR = path.join(process.cwd(), 'test-results',
    'webui-health-' + new Date().toISOString().replace(/[:.]/g, '-'));
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
    const context = await browser.newContext({ viewport: { width: 1600, height: 1000 } });
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

    const resp = await page.goto(BASE_URL + '/health.html', { waitUntil: 'domcontentloaded', timeout: 30000 });
    await page.waitForTimeout(2500);
    record('страница health.html отдаётся', !!resp && resp.status() === 200, 'HTTP ' + (resp ? resp.status() : '?'));

    // Ждём появления карточек: страница рисует их после fetch.
    const cardCount = await page.locator('.health-card').count();
    record('карточки отрисованы', cardCount > 0, 'найдено .health-card: ' + cardCount);

    const styles = await page.evaluate(() => {
        const out = {};
        const probe = (sel) => {
            const el = document.querySelector(sel);
            if (!el) return null;
            const cs = getComputedStyle(el);
            return {
                borderTopWidth: cs.borderTopWidth,
                borderTopStyle: cs.borderTopStyle,
                borderTopColor: cs.borderTopColor,
                background: cs.backgroundColor
            };
        };
        out.card = probe('.health-card');
        out.section = probe('.health-section');
        out.text = (document.body.innerText || '').trim().length;
        out.borderToken = getComputedStyle(document.documentElement).getPropertyValue('--border').trim();
        return out;
    });

    const borderDrawn = (s) => !!s && s.borderTopWidth === '1px' && s.borderTopStyle === 'solid' &&
        /^rgb(a)?\(/.test(s.borderTopColor) && s.borderTopColor !== 'rgba(0, 0, 0, 0)';
    record('у карточки нарисована рамка', borderDrawn(styles.card),
        styles.card ? `width=${styles.card.borderTopWidth}, style=${styles.card.borderTopStyle}, color=${styles.card.borderTopColor}` : 'карточки нет');
    record('у секции нарисована рамка', borderDrawn(styles.section),
        styles.section ? `width=${styles.section.borderTopWidth}, style=${styles.section.borderTopStyle}, color=${styles.section.borderTopColor}` : 'секции нет');
    record('токен --border определён на странице', styles.borderToken.length > 0, '--border=' + styles.borderToken);
    record('на странице есть содержимое', styles.text > 200, 'текста: ' + styles.text + ' символов');

    await page.screenshot({ path: path.join(ARTIFACT_DIR, 'health.png'), fullPage: true });
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

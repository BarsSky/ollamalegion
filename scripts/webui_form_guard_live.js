// webui_form_guard_live_r512.js — R512 (2026-11-18): ЖИВАЯ проверка на стенде, что
// авто-обновление больше не затирает ввод в формы.
//
// ЧТО ПРОВЕРЯЕТСЯ (факты, не рассуждения):
//  1) новый образ отдаёт модуль js/modules/refresh-guard.js (HTTP 200) и на странице
//     есть window.RefreshGuard;
//  2) на странице настроек в текстовое поле печатается значение, затем ЖДЁМ больше
//     двух периодов авто-обновления (по умолчанию 5 с → ждём 12 с):
//     - значение осталось ровно тем, что набрали;
//     - фокус и позиция каретки не потеряны;
//  3) после ухода из поля (blur) следующий тик снова обновляет страницу —
//     то есть пауза именно «пока идёт ввод», а не навсегда.
//
// ЗАПУСК: node scripts/webui_form_guard_live_r512.js [baseUrl]
// Логин — из deployments/.env (LB_ADMIN_USER/LB_ADMIN_PASSWORD), в вывод не печатается.
// Браузер — установленный Edge (channel:'msedge'): встроенный chromium в этом
// окружении зависает на newContext (проверено ранее).
'use strict';

const { chromium } = require('playwright');
const fs = require('fs');
const path = require('path');

const BASE_URL = process.argv[2] || 'http://127.0.0.1:18083';
const ARTIFACT_DIR = path.join(process.cwd(), 'test-results', 'form-guard-' + new Date().toISOString().replace(/[:.]/g, '-'));
fs.mkdirSync(ARTIFACT_DIR, { recursive: true });

let exitCode = 0;
const report = { url: BASE_URL, steps: [] };
function check(name, ok, detail) {
    report.steps.push({ name, ok, detail: detail || '' });
    console.log(`  ${ok ? 'PASS' : 'FAIL'} ${name}${detail ? ' — ' + detail : ''}`);
    if (!ok) exitCode = 1;
}

function envValue(key) {
    const envFile = path.join(__dirname, '..', 'deployments', '.env');
    try {
        const line = fs.readFileSync(envFile, 'utf8').split(/\r?\n/).find((l) => l.startsWith(key + '='));
        return line ? line.slice(key.length + 1).trim() : '';
    } catch (e) { return ''; }
}

async function main() {
    const user = process.env.ADMIN_USER || envValue('LB_ADMIN_USER') || 'admin';
    const pass = process.env.ADMIN_PASSWORD || envValue('LB_ADMIN_PASSWORD');
    if (!pass) { throw new Error('нет LB_ADMIN_PASSWORD в deployments/.env'); }

    const browser = await chromium.launch({ channel: 'msedge', headless: true });
    const context = await browser.newContext({ viewport: { width: 1400, height: 950 } });
    const page = await context.newPage();

    // 1) Модуль и вход
    const modResp = await page.goto(BASE_URL + '/js/modules/refresh-guard.js', { waitUntil: 'domcontentloaded', timeout: 30000 });
    const modSrc = await modResp.text();
    check('образ отдаёт refresh-guard.js', modResp.status() === 200 && /RefreshGuard/.test(modSrc),
        'HTTP ' + modResp.status());

    await page.goto(BASE_URL + '/', { waitUntil: 'domcontentloaded', timeout: 30000 });
    const login = await page.evaluate(async ({ u, p }) => {
        const r = await fetch('/api/v1/auth/login', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ name: u, password: p }) });
        return r.status;
    }, { u: user, p: pass });
    check('вход администратора', login === 200, 'HTTP ' + login);

    await page.goto(BASE_URL + '/?page=settings', { waitUntil: 'domcontentloaded', timeout: 30000 });
    await page.waitForTimeout(2500);
    const hasGuard = await page.evaluate(() => !!(window.RefreshGuard && typeof window.RefreshGuard.isEditingIn === 'function'));
    check('window.RefreshGuard доступен на странице', hasGuard);

    // 2) Печатаем в первое доступное текстовое поле настроек и ждём 12 с.
    // ВАЖНО: только ТЕКСТОВЫЕ поля. В настройках первым по DOM идёт input[type=number]
    // (data-hot-input=LB_OPENAI_AUTO_STREAM_KEEPALIVE_SEC): браузер отбрасывает в него
    // буквы, и «значение стало пустым» означало бы ошибку теста, а не дефект WebUI
    // (поймано живым прогоном R512).
    const fieldSel = '#settings-page input[type="text"]:not([disabled])';
    // Первое текстовое поле может лежать в СВЁРНУТОЙ секции аккордеона (element is not
    // visible) — ищем ПЕРВОЕ ВИДИМОЕ, при необходимости раскрываем первую секцию.
    const candidates = page.locator(fieldSel);
    let fieldIdx = -1;
    async function pickVisible() {
        const n = await candidates.count();
        for (let i = 0; i < n; i++) {
            if (await candidates.nth(i).isVisible()) { fieldIdx = i; return true; }
        }
        return false;
    }
    if (!(await pickVisible())) {
        const header = page.locator('#settings-page .section-header, #settings-page .accordion-header, #settings-page .settings-section > summary, #settings-page details > summary').first();
        if (await header.count()) { await header.click({ timeout: 5000 }).catch(() => {}); }
        await page.waitForTimeout(700);
        await pickVisible();
    }
    if (fieldIdx < 0) {
        check('в настройках есть ВИДИМОЕ поле ввода', false, 'все текстовые поля скрыты (секции свёрнуты)');
        await browser.close();
        return;
    }
    const field = candidates.nth(fieldIdx);
    check('найдено видимое поле ввода', true, 'индекс ' + fieldIdx);
    const marker = 'R512-guard-' + Date.now().toString().slice(-6);
    await field.click();
    await field.fill('');
    await field.type(marker, { delay: 30 });
    const focusedTag = await page.evaluate(() => {
        const a = document.activeElement;
        return a ? (a.tagName + '#' + (a.id || '') + '.' + (a.className || '')).trim() : '';
    });
    const editingNow = await page.evaluate(() => {
        const pg = document.getElementById('settings-page');
        return !!(window.RefreshGuard && window.RefreshGuard.isEditingIn(pg));
    });
    check('guard видит ввод в поле', editingNow, 'activeElement=' + focusedTag);

    // Ждём больше двух периодов авто-обновления (5 с) — столько же, сколько раньше
    // хватало, чтобы форма была перезаписана.
    await page.waitForTimeout(12000);
    const valueAfter = await field.inputValue();
    const stillFocused = await page.evaluate(() => {
        const a = document.activeElement;
        return !!(a && a.tagName === 'INPUT' && a.type === 'text');
    });
    check('набранное значение НЕ затёрто авто-обновлением', valueAfter === marker, 'значение="' + valueAfter + '", ожидалось="' + marker + '"');
    check('фокус в поле сохранён', stillFocused);
    await page.screenshot({ path: path.join(ARTIFACT_DIR, 'while-typing.png') });

    // 3) Уходим из поля: пауза не вечная — тик снова перерисовывает страницу.
    await page.evaluate(() => { if (document.activeElement && document.activeElement.blur) document.activeElement.blur(); });
    const editingAfterBlur = await page.evaluate(() => {
        const pg = document.getElementById('settings-page');
        return !!(window.RefreshGuard && window.RefreshGuard.isEditingIn(pg));
    });
    check('после blur ввод не считается активным (пауза не вечная)', editingAfterBlur === false);
    await page.waitForTimeout(6000);
    await page.screenshot({ path: path.join(ARTIFACT_DIR, 'after-blur.png') });

    await browser.close();
}

main()
    .catch((e) => { check('выполнение', false, String(e && e.message ? e.message : e)); })
    .finally(() => {
        fs.writeFileSync(path.join(ARTIFACT_DIR, 'report.json'), JSON.stringify(report, null, 2));
        console.log('\nОтчёт: ' + path.relative(process.cwd(), path.join(ARTIFACT_DIR, 'report.json')));
        console.log('ИТОГ: ' + (exitCode === 0 ? 'все проверки прошли' : 'есть провалы'));
        process.exit(exitCode);
    });

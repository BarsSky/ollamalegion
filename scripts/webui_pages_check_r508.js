// webui_pages_check_r508.js — R508 (2026-11-19): факты о страницах WebUI после
// пересборки образа.
//
// ЗАЧЕМ. Задача требует не «я посмотрел — работает», а фактов: страницы
// «Диагностика», «Справка и шаблоны», «Виртуальные модели», «Состояние» обязаны
// отображаться И не исчезать после загрузки. Ранее уже был живой дефект, когда
// `navModeDependent`/`navModeIndependent` в settings-ui.js прятали часть пунктов при
// переключении режима, и оператор видел «страница пропала».
//
// ЗАПУСК:
//   node scripts/webui_pages_check_r508.js [baseUrl]
// Логин берётся из deployments/.env (LB_ADMIN_USER / LB_ADMIN_PASSWORD) или из
// ADMIN_USER/ADMIN_PASSWORD; в отчёте и логе пароль НЕ печатается.
//
// ВЫХОД: test-results/webui-pages-<ts>/{report.json, *.png}; код 1 при провале.
//
// ВАЖНО про браузер: используется установленный Edge (channel: 'msedge').
// Встроенный chromium в этом окружении зависает на newContext — проверено ранее.

const { chromium } = require('playwright');
const fs = require('fs');
const path = require('path');

const BASE_URL = process.argv[2] || 'http://127.0.0.1:18083';
const ARTIFACT_DIR = path.join(
    process.cwd(),
    'test-results',
    'webui-pages-' + new Date().toISOString().replace(/[:.]/g, '-')
);
fs.mkdirSync(ARTIFACT_DIR, { recursive: true });

// Страницы, которые обязаны быть в навигации (data-page → подпись из i18n).
const REQUIRED_PAGES = [
    { page: 'diagnostics', label: 'Диагностика' },
    { page: 'reference', label: 'Справка и шаблоны' },
    { page: 'virtual-models', label: 'Виртуальные модели' },
    { page: 'health', label: 'Состояние' },
];

const results = { url: BASE_URL, timestamp: new Date().toISOString(), steps: [], errors: [], navVisibleAtEnd: [] };
let exitCode = 0;

function record(name, ok, detail) {
    results.steps.push({ name, ok, detail: detail || '' });
    console.log(`  ${ok ? 'PASS' : 'FAIL'} ${name}${detail ? ' — ' + detail : ''}`);
    if (!ok) exitCode = 1;
}

function envValue(key) {
    const envFile = path.join(__dirname, '..', 'deployments', '.env');
    try {
        const line = fs.readFileSync(envFile, 'utf8').split(/\r?\n/).find((l) => l.startsWith(key + '='));
        return line ? line.slice(key.length + 1).trim() : '';
    } catch (e) {
        return '';
    }
}

const ADMIN_USER = process.env.ADMIN_USER || envValue('LB_ADMIN_USER') || 'admin';
const ADMIN_PASSWORD = process.env.ADMIN_PASSWORD || envValue('LB_ADMIN_PASSWORD');

async function main() {
    if (!ADMIN_PASSWORD) {
        throw new Error('не найден пароль администратора (LB_ADMIN_PASSWORD в deployments/.env)');
    }

    const browser = await chromium.launch({ channel: 'msedge', headless: true });
    const context = await browser.newContext({ viewport: { width: 1400, height: 900 } });
    const page = await context.newPage();

    // 1) Вход: тот же путь, что у оператора (POST /api/v1/auth/login из страницы,
    //    чтобы cookie сессии выставилась в контексте браузера).
    await page.goto(BASE_URL + '/', { waitUntil: 'domcontentloaded', timeout: 30000 });
    const loginStatus = await page.evaluate(async ({ user, pass }) => {
        const res = await fetch('/api/v1/auth/login', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ name: user, password: pass }),
        });
        return res.status;
    }, { user: ADMIN_USER, pass: ADMIN_PASSWORD });
    record('вход администратора', loginStatus === 200, 'HTTP ' + loginStatus);

    // 2) Перезагрузка после входа: страница строится заново — здесь и проявлялся
    //    дефект «пункт виден на миг и исчезает».
    await page.goto(BASE_URL + '/', { waitUntil: 'domcontentloaded', timeout: 30000 });
    await page.waitForTimeout(1500);

    // 3) Пункты навигации: есть и ВИДИМЫ (не display:none / не нулевой размер).
    for (const item of REQUIRED_PAGES) {
        const el = page.locator(`.nav-item[data-page="${item.page}"]`);
        const count = await el.count();
        if (count === 0) {
            record(`пункт «${item.label}» существует`, false, 'нет .nav-item[data-page="' + item.page + '"]');
            continue;
        }
        const visible = await el.first().isVisible();
        const text = (await el.first().innerText()).trim();
        record(`пункт «${item.label}» виден`, visible && text.length > 0, 'текст: ' + text);
        if (text && item.label !== text) {
            record(`подпись «${item.label}»`, false, 'на странице: «' + text + '»');
        }
    }

    // 4) Внутренние страницы (hash-секции) открываются и содержат текст.
    for (const item of REQUIRED_PAGES.filter((i) => ['diagnostics', 'reference'].includes(i.page))) {
        // Прямая ссылка ?page=… (app.js:237 pageFromURL) — это документированный путь
        // «дать коллеге ссылку», тот же список известных страниц, что у навигации.
        await page.goto(BASE_URL + '/?page=' + item.page, { waitUntil: 'domcontentloaded', timeout: 30000 });
        await page.waitForTimeout(2500);
        // Секции WebUI — это `<div class="page" id="<page>-page">` (router app.js:196
        // switchPage переключает класс active). Проверяем именно её: видима и с текстом.
        const sectionInfo = await page.evaluate((pageName) => {
            const el = document.getElementById(pageName + '-page');
            if (!el) return { found: false, active: false, text: '' };
            return {
                found: true,
                active: el.classList.contains('active'),
                text: el.innerText.trim().slice(0, 200),
            };
        }, item.page);
        const sectionText = sectionInfo.text || '';
        record(`секция «${item.label}» активна`, sectionInfo.found && sectionInfo.active,
            sectionInfo.found ? ('класс active: ' + sectionInfo.active) : 'нет #' + item.page + '-page');
        record(`секция «${item.label}» отрисована`, sectionText.length > 20,
            sectionText ? sectionText.replace(/\s+/g, ' ').slice(0, 80) : 'текста нет');
        await page.screenshot({ path: path.join(ARTIFACT_DIR, `section-${item.page}.png`) });
    }

    // 3b) Ни один пункт меню не скрыт (дефект «пункт виден на миг и исчезает»
    //     лечился именно здесь: navModeDependent/navModeIndependent в settings-ui.js).
    const navStats = await page.evaluate(() => {
        const all = Array.from(document.querySelectorAll('.nav-item[data-page]'));
        const visible = all.filter((el) => el.offsetParent !== null);
        return { total: all.length, visible: visible.length };
    });
    record('все пункты меню видимы', navStats.total > 0 && navStats.visible === navStats.total,
        `видимо ${navStats.visible} из ${navStats.total}`);
    results.navStats = navStats;
    // 4b) Путь оператора: КЛИК по пункту меню. Именно здесь ранее проявлялся дефект
    //     «пункт виден, а страница пустая/не открылась».
    for (const item of REQUIRED_PAGES.filter((i) => ['diagnostics', 'reference'].includes(i.page))) {
        await page.goto(BASE_URL + '/', { waitUntil: 'domcontentloaded', timeout: 30000 });
        await page.waitForTimeout(1500);
        await page.locator(`.nav-item[data-page="${item.page}"]`).first().click();
        await page.waitForTimeout(600);
        const state = await page.evaluate((pageName) => {
            const el = document.getElementById(pageName + '-page');
            const nav = document.querySelector(`.nav-item[data-page="${pageName}"]`);
            return {
                active: !!(el && el.classList.contains('active')),
                navActive: !!(nav && nav.classList.contains('active')),
                textLen: el ? el.innerText.trim().length : 0,
            };
        }, item.page);
        record(`клик по «${item.label}» открывает страницу`,
            state.active && state.navActive && state.textLen > 20,
            `секция active=${state.active}, пункт active=${state.navActive}, текста ${state.textLen}`);
    }
    // 5) Отдельные страницы: virtual-models.html и health.html (открываются в новой
    //    вкладке по href, поэтому проверяем их напрямую и логинимся в каждой).
    for (const item of REQUIRED_PAGES.filter((i) => ['virtual-models', 'health'].includes(i.page))) {
        const p2 = await context.newPage();
        const resp = await p2.goto(`${BASE_URL}/${item.page}.html`, { waitUntil: 'domcontentloaded', timeout: 30000 });
        await p2.waitForTimeout(800);
        const textLen = (await p2.evaluate(() => document.body.innerText.trim().length)) || 0;
        record(`страница «${item.label}» (${item.page}.html)`, resp && resp.status() === 200 && textLen > 50,
            `HTTP ${resp ? resp.status() : '?'}, текста ${textLen} символов`);
        await p2.screenshot({ path: path.join(ARTIFACT_DIR, `page-${item.page}.png`) });
        await p2.close();
    }

    // 6) «Не исчезают после загрузки»: перезагрузка и ожидание 3 с, затем пункты
    //    обязаны быть на месте (дефект был именно в позднем перерисовывании).
    await page.goto(BASE_URL + '/', { waitUntil: 'domcontentloaded', timeout: 30000 });
    await page.waitForTimeout(3000);
    for (const item of REQUIRED_PAGES) {
        const el = page.locator(`.nav-item[data-page="${item.page}"]`);
        const visible = (await el.count()) > 0 && (await el.first().isVisible());
        results.navVisibleAtEnd.push({ page: item.page, visible });
        record(`после перезагрузки виден «${item.label}»`, visible);
    }
    await page.screenshot({ path: path.join(ARTIFACT_DIR, 'after-reload.png'), fullPage: false });

    await browser.close();
}

main()
    .catch((err) => {
        results.errors.push(String(err && err.message ? err.message : err));
        console.log('ОШИБКА: ' + results.errors[results.errors.length - 1]);
        exitCode = 1;
    })
    .finally(() => {
        fs.writeFileSync(path.join(ARTIFACT_DIR, 'report.json'), JSON.stringify(results, null, 2));
        console.log(`\nОтчёт: ${path.relative(process.cwd(), path.join(ARTIFACT_DIR, 'report.json'))}`);
        console.log(`ИТОГ: ${exitCode === 0 ? 'все проверки прошли' : 'есть провалы'}`);
        process.exit(exitCode);
    });

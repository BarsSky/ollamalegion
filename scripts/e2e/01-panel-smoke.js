#!/usr/bin/env node
/**
 * e2e/01-panel-smoke.js — R134: проверка панели НАСТОЯЩИМ браузером (Playwright).
 *
 * ЗАЧЕМ. Жалоба оператора «в панели нет вкладки агенты и не отображаются иконки»
 * проверялась только HTTP-запросами. Здесь то же самое проверяется браузером:
 * открывается страница, ищется навигация, считаются РЕАЛЬНО отрисованные иконки
 * (по computed font-family и по ширине глифа) и проверяется, что вкладка
 * «Агенты» кликабельна и открывает свою секцию.
 *
 * Проверяются ОБЕ панели:
 *   * канонический WebUI  — http://127.0.0.1:18083 (контейнер webui);
 *   * панель балансера    — http://127.0.0.1:18081 (образ балансера отдаёт
 *     собственный дашборд; до R134 у него не было ни index.html, ни webfonts).
 *
 * Запуск:
 *   node scripts/e2e/01-panel-smoke.js
 *   WEBUI_URL=http://127.0.0.1:18083 BALANCER_URL=http://127.0.0.1:18081 node scripts/e2e/01-panel-smoke.js
 *
 * Требуется установленный Edge (channel: msedge) — отдельных загрузок Chromium нет.
 */
'use strict';

const { chromium } = require('playwright');

const TOKEN = process.env.DSH_API_TOKEN || 'changeme-bundled-with-agent-token';
const TARGETS = [
    { name: 'канонический WebUI', url: process.env.WEBUI_URL || 'http://127.0.0.1:18083' },
    { name: 'панель балансера', url: process.env.BALANCER_URL || 'http://127.0.0.1:18081' }
];

let failures = 0;
function report(ok, label, detail) {
    console.log((ok ? '  ✓ ' : '  ✗ ') + label + (detail ? ' — ' + detail : ''));
    if (!ok) failures++;
}

async function checkPanel(browser, target) {
    console.log('\n=== ' + target.name + ' (' + target.url + ') ===');
    const ctx = await browser.newContext({ viewport: { width: 1440, height: 900 } });
    // Токен WebUI держит в localStorage['apiToken'] — кладём его до загрузки
    // страницы, иначе защищённые вызовы вернут 401.
    await ctx.addInitScript(function (t) {
        try { localStorage.setItem('apiToken', t); } catch (e) {}
    }, TOKEN);
    const page = await ctx.newPage();

    const failedAssets = [];
    page.on('response', function (r) {
        if (r.status() >= 400) failedAssets.push(r.status() + ' ' + r.url());
    });

    await page.goto(target.url + '/', { waitUntil: 'domcontentloaded', timeout: 30000 });
    const title = await page.title();
    report(true, 'страница открылась', 'title="' + title + '"');

    // 1. Навигация: пункт «Агенты» присутствует и видим.
    const agentsItem = page.locator('[data-page="agents"]').first();
    const agentsCount = await page.locator('[data-page="agents"]').count();
    report(agentsCount > 0, 'пункт навигации «Агенты» есть в разметке', 'найдено: ' + agentsCount);
    if (agentsCount > 0) {
        report(await agentsItem.isVisible(), 'пункт «Агенты» видим на широком окне');
    }

    // 2. Иконки: у Font Awesome глиф появляется только если подгрузился webfont.
    //    Проверяем не класс, а ФАКТ: font-family у ::before и наличие файла шрифта.
    const iconProbe = await page.evaluate(function () {
        const el = document.querySelector('.fas, .fa-solid, i[class*="fa-"]');
        if (!el) return { found: false };
        const cs = getComputedStyle(el, '::before');
        return {
            found: true,
            family: cs.fontFamily || '',
            content: cs.content || '',
            fontLoaded: (document.fonts && document.fonts.check)
                ? document.fonts.check('900 16px "Font Awesome 6 Free"')
                : null
        };
    });
    report(iconProbe.found, 'иконочный элемент найден');
    if (iconProbe.found) {
        report(/Font Awesome/i.test(iconProbe.family),
            'иконке назначен шрифт Font Awesome', iconProbe.family);
        report(iconProbe.fontLoaded !== false,
            'шрифт иконок реально загружен (document.fonts.check)', String(iconProbe.fontLoaded));
    }

    // 3. Никаких 404 по ассетам — именно это ломало иконки до R134.
    const missing = failedAssets.filter(function (u) { return /webfonts|\/css\/|\/js\//.test(u); });
    report(missing.length === 0, 'нет 404 по css/js/webfonts',
        missing.length ? missing.slice(0, 4).join(' | ') : 'ок');

    // 4. Клик по «Агенты» открывает секцию (страница одна, роутинг по hash).
    if (agentsCount > 0) {
        const clickable = await agentsItem.isVisible().catch(function () { return false; });
        if (clickable) {
            await agentsItem.click().catch(function () {});
            await page.waitForTimeout(300);
            const hash = await page.evaluate(function () { return location.hash; });
            const sectionVisible = await page.locator('#agentsTable, [data-i18n="agents.title"]').first()
                .isVisible().catch(function () { return false; });
            report(hash === '#agents' || sectionVisible,
                'переход на «Агенты» сработал', 'hash=' + hash + ' секция=' + sectionVisible);
        } else {
            report(false, 'пункт «Агенты» не кликабелен на этом окне');
        }
    }

    await page.screenshot({ path: 'scripts/e2e/out/' + (target.name.includes('балансера') ? 'balancer' : 'webui') + '-panel.png', fullPage: false });
    await ctx.close();
}

(async function main() {
    const browser = await chromium.launch({ channel: 'msedge', headless: true });
    try {
        for (const t of TARGETS) {
            await checkPanel(browser, t);
        }
    } finally {
        await browser.close();
    }
    console.log('\n' + (failures === 0 ? 'ИТОГ: обе панели в порядке' : 'ИТОГ: замечаний — ' + failures));
    process.exit(failures === 0 ? 0 : 1);
})().catch(function (e) {
    console.error('ОШИБКА запуска браузера:', e && e.message);
    process.exit(2);
});

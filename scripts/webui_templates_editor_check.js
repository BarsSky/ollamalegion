// webui_templates_editor_check.js — R515 (2026-11-19): живая проверка того, что
// шаблоны в «Справке и шаблонах» можно создать, изменить и удалить ИЗ UI.
//
// ЖАЛОБА ОПЕРАТОРА (§2.6 разбора): «шаблоны нельзя править». Ограничение было
// только в UI — API PUT/DELETE /api/v1/reference/templates/{id} работал.
// Скрипт проходит путь оператора в браузере (Playwright+Edge, как остальные
// живые проверки), а результат СВЕРЯЕТ С API: после «Сохранить» шаблон обязан
// быть в GET /api/v1/reference/templates, после «Удалить» — исчезнуть. Скриншоты
// формы и карточки складываются в test-results/webui-templates-<ts>/.
//
// ЗАПУСК: node scripts/webui_templates_editor_check.js [baseUrl]
// Пароль — из deployments/.env (LB_ADMIN_USER/LB_ADMIN_PASSWORD), не печатается.
const { chromium } = require('playwright');
const fs = require('fs');
const path = require('path');

const BASE_URL = process.argv[2] || 'http://127.0.0.1:18083';
const ARTIFACT_DIR = path.join(process.cwd(), 'test-results',
    'webui-templates-' + new Date().toISOString().replace(/[:.]/g, '-'));
fs.mkdirSync(ARTIFACT_DIR, { recursive: true });
const TPL_ID = 'stand-ui-r515-check';
const TPL_ID_RE = TPL_ID.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');

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

async function apiList(page) {
    return page.evaluate(async () => {
        const res = await fetch('/api/v1/reference/templates');
        if (!res.ok) return { ok: false, status: res.status, items: [] };
        const body = await res.json();
        return { ok: true, status: res.status, items: body.items || [] };
    });
}
async function apiTemplate(page, id) {
    return page.evaluate(async (tid) => {
        const res = await fetch('/api/v1/reference/templates/' + encodeURIComponent(tid));
        if (!res.ok) return { ok: false, status: res.status };
        const body = await res.json();
        return { ok: true, status: res.status, template: body.template, usage: body.usage };
    }, id);
}
async function apiDelete(page, id) {
    return page.evaluate(async (tid) => {
        const res = await fetch('/api/v1/reference/templates/' + encodeURIComponent(tid), { method: 'DELETE' });
        return res.status;
    }, id);
}

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

    // Чистим возможный мусор от предыдущих прогонов, чтобы шаги были воспроизводимы.
    await apiDelete(page, TPL_ID);

    await page.goto(BASE_URL + '/?page=reference', { waitUntil: 'domcontentloaded', timeout: 30000 });
    await page.waitForTimeout(2500);
    record('страница «Справка и шаблоны» открыта', await page.locator('#reference-page.active').count() > 0,
        'активная секция #reference-page');

    await page.locator('[data-ref-tab="templates"]').first().click();
    await page.waitForTimeout(600);
    record('вкладка «Шаблоны» открыта', await page.locator('#referenceTplPane').isVisible(),
        'панель #referenceTplPane');

    // --- создание ---------------------------------------------------------
    await page.locator('[data-ref-action="new-tpl"]').first().click();
    await page.waitForTimeout(400);
    const editorVisible = await page.locator('#referenceTplEditor').isVisible();
    record('кнопка «Новый шаблон» открывает форму', editorVisible, 'форма #referenceTplEditor');
    await page.screenshot({ path: path.join(ARTIFACT_DIR, '01-editor-new.png') });
    if (!editorVisible) throw new Error('форма редактора не появилась — дальше идти нельзя');

    await page.fill('#referenceTplEditor [data-ref-tpledit="id"]', TPL_ID);
    await page.fill('#referenceTplEditor [data-ref-tpledit="name"]', 'R515: живая проверка');
    await page.fill('#referenceTplEditor [data-ref-tpledit="comment"]', 'создан скриптом webui_templates_editor_check.js');
    // paramName: выбор перерисовывает строку (контрол значения зависит от типа),
    // поэтому значение заполняем ПОСЛЕ перерисовки.
    await page.selectOption('#referenceTplEditor [data-ref-tpledit="paramName"]', 'contextLength');
    await page.waitForTimeout(300);
    await page.fill('#referenceTplEditor [data-ref-tpledit="paramValue"]', '32768');
    await page.screenshot({ path: path.join(ARTIFACT_DIR, '02-editor-filled.png') });

    await page.locator('[data-ref-action="tpl-editor-save"]').first().click();
    await page.waitForTimeout(2500);
    const afterCreate = await apiList(page);
    const created = afterCreate.items.filter((t) => t.id === TPL_ID)[0];
    record('«Сохранить» создаёт шаблон через API', !!created,
        created ? 'id=' + created.id + ', params=' + JSON.stringify(created.params) : 'шаблона нет в GET /templates');
    record('параметр доехал значением нужного типа', !!(created && created.params && created.params.contextLength === 32768),
        created ? 'contextLength=' + JSON.stringify(created.params && created.params.contextLength) : '—');
    record('форма закрылась после сохранения', !(await page.locator('#referenceTplEditor').isVisible()), 'редактор скрыт');

    // --- подсказка usage --------------------------------------------------
    await page.locator('[data-ref-action="toggle-tpl"][data-ref-id="' + TPL_ID + '"]').first().click();
    await page.waitForTimeout(1200);
    const usageCount = await page.locator('.reference-tpl-usage').count();
    const usageText = usageCount ? (await page.locator('.reference-tpl-usage').first().innerText()).trim() : '';
    record('карточка показывает usage.usedByProfiles', usageCount > 0 && usageText.length > 0,
        usageText || 'блока .reference-tpl-usage нет');

    // --- правка -----------------------------------------------------------
    await page.locator('[data-ref-action="edit-tpl"][data-ref-id="' + TPL_ID + '"]').first().click();
    await page.waitForTimeout(500);
    const editShown = await page.locator('#referenceTplEditor').isVisible();
    record('кнопка «Изменить» открывает форму правки', editShown, 'форма #referenceTplEditor');
    if (editShown) {
        const idField = await page.locator('#referenceTplEditor [data-ref-tpledit="id"]').first();
        const isReadonly = await idField.getAttribute('readonly');
        record('id существующего шаблона нельзя менять', isReadonly !== null, 'readonly=' + isReadonly);
        await page.fill('#referenceTplEditor [data-ref-tpledit="comment"]', 'изменено из UI (R515)');
        await page.locator('[data-ref-action="tpl-editor-save"]').first().click();
        await page.waitForTimeout(2500);
        const afterEdit = await apiTemplate(page, TPL_ID);
        record('правка сохраняется в шаблон', !!(afterEdit.ok && afterEdit.template &&
            afterEdit.template.comment === 'изменено из UI (R515)'),
            afterEdit.ok ? 'comment=' + JSON.stringify(afterEdit.template && afterEdit.template.comment) : 'HTTP ' + afterEdit.status);
        record('API отдаёт usage для раскрытого шаблона', !!(afterEdit.usage && Array.isArray(afterEdit.usage.usedByProfiles)),
            'usedByProfiles=' + JSON.stringify(afterEdit.usage && afterEdit.usage.usedByProfiles));
    }

    // --- удаление ---------------------------------------------------------
    await page.locator('[data-ref-action="delete-tpl"][data-ref-id="' + TPL_ID + '"]').first().click();
    await page.waitForTimeout(2500);
    const afterDelete = await apiList(page);
    record('«Удалить» убирает шаблон', !afterDelete.items.some((t) => t.id === TPL_ID),
        'items=' + afterDelete.items.length + ', наш: ' + afterDelete.items.some((t) => t.id === TPL_ID));
    const inList = await page.locator('[data-ref-tpl-card="' + TPL_ID + '"]').count();
    record('карточка удалённого шаблона исчезла со страницы', inList === 0, 'карточек: ' + inList);
    await page.screenshot({ path: path.join(ARTIFACT_DIR, '03-after-delete.png') });

    // Страховка: если что-то пошло не так — не оставляем мусор в справочнике.
    await apiDelete(page, TPL_ID);

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

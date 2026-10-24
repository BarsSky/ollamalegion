// check-auth-roles-r222.js — R222: живая проверка входа и ролей в WebUI.
//
// Что проверяет (то, из-за чего жаловались: «обвязки доступа нет»):
//   1. без входа виден экран входа, а админские действия (data-admin-only) скрыты;
//   2. вход администратора убирает экран входа, показывает админские действия,
//      чип «администратор» и раздел «Пользователи» со списком;
//   3. вход гостем (viewer) — админских действий не видно ни одного, раздел
//      пользователей скрыт, страница при этом работает без ошибок JS;
//   4. страница не уносит API-токен (в config.js он пуст).
//
// Запуск (из корня репозитория):
//   node scripts/webui-e2e/check-auth-roles-r222.js
// Переменные: WEBUI (по умолчанию http://127.0.0.1:18083), ADMIN_USER/ADMIN_PASSWORD
// Переменные: WEBUI (по умолчанию http://127.0.0.1:18083), ADMIN_USER/ADMIN_PASSWORD
// (иначе LB_ADMIN_USER/LB_ADMIN_PASSWORD из deployments/.env; имя оператора в коде
// не хардкодится — личные данные не хранятся в репозитории).
const fs = require('fs');
const path = require('path');
const { chromium } = require('playwright');

const WEBUI = process.env.WEBUI || 'http://127.0.0.1:18083';
const GUEST_USER = process.env.GUEST_USER || 'ui-guest-r222';
const GUEST_PASS = process.env.GUEST_PASS || 'Guest-Pass-R222';

function envValueFromDeployments(key) {
  try {
    const envFile = path.join(__dirname, '..', '..', 'deployments', '.env');
    const line = fs.readFileSync(envFile, 'utf8').split(/\r?\n/).find(l => l.startsWith(key + '='));
    return line ? line.slice(key.length + 1).trim() : '';
  } catch (e) { return ''; }
}

const ADMIN_USER = process.env.ADMIN_USER || envValueFromDeployments('LB_ADMIN_USER') || 'admin';

function adminPassword() {
  if (process.env.ADMIN_PASSWORD) return process.env.ADMIN_PASSWORD;
  const envFile = path.join(__dirname, '..', '..', 'deployments', '.env');
  const text = fs.readFileSync(envFile, 'utf8');
  const line = text.split(/\r?\n/).find(l => l.startsWith('LB_ADMIN_PASSWORD='));
  if (!line) throw new Error('LB_ADMIN_PASSWORD не найден в deployments/.env');
  return line.slice('LB_ADMIN_PASSWORD='.length).trim();
}

const fails = [];
function check(ok, what, detail) {
  console.log((ok ? 'PASS ' : 'FAIL ') + what + (detail ? ' :: ' + detail : ''));
  if (!ok) fails.push(what + (detail ? ' :: ' + detail : ''));
}

async function rest(pathname, opts) {
  const res = await fetch(WEBUI + pathname, opts);
  const body = await res.text();
  let json = {};
  try { json = JSON.parse(body); } catch (e) { /* not json */ }
  return { status: res.status, json: json, body: body, headers: res.headers };
}

async function adminSession() {
  const login = await rest('/api/v1/auth/login', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ name: ADMIN_USER, password: adminPassword() }),
  });
  if (login.status !== 200) throw new Error('вход администратора через API: HTTP ' + login.status);
  const cookie = (login.headers.get('set-cookie') || '').split(';')[0];
  return { cookie: cookie, csrf: login.json.csrf };
}

async function snapshot(page) {
  return page.evaluate(() => {
    const nodes = Array.prototype.slice.call(document.querySelectorAll('[data-admin-only]'));
    const visible = nodes.filter(function (el) {
      if (el.hasAttribute('data-admin-hidden')) return false;
      if (el.hasAttribute('disabled')) return false;
      if (el.style && el.style.display === 'none') return false;
      return true;
    });
    const overlay = document.getElementById('authOverlay');
    const chip = document.getElementById('authUserChip');
    const users = document.getElementById('section-users');
    return {
      authenticated: !!(window.Auth && window.Auth.state().authenticated),
      role: window.Auth && window.Auth.role ? window.Auth.role() : '',
      isAdmin: !!(window.Auth && window.Auth.isAdmin && window.Auth.isAdmin()),
      overlayVisible: !!(overlay && overlay.style.display === 'flex'),
      adminOnlyTotal: nodes.length,
      adminOnlyVisible: visible.length,
      chipText: chip ? String(chip.textContent) : '',
      usersSectionVisible: !!(users && users.style.display !== 'none'),
      // R225: активные сессии в том же разделе.
      sessionsRows: (function () {
        var tb = document.getElementById('sessionsTableBody');
        if (!tb) { return 0; }
        // Плейсхолдер «Загрузка...» не считаем: у гостя список не грузится вовсе.
        return Array.prototype.filter.call(tb.querySelectorAll('tr'), function (tr) {
          var text = tr.textContent.trim();
          return text !== '' && text.indexOf('Загрузка') < 0;
        }).length;
      })(),
      bodyClass: document.body.className,
    };
  });
}

(async () => {
  console.log('R222: вход и роли WebUI\n');

  // 0. Страница не уносит токен.
  const cfg = await rest('/js/modules/config.js');
  check(cfg.status === 200 && /API_TOKEN:\s*''/.test(cfg.body), 'config.js отдаёт пустой API_TOKEN',
    'status=' + cfg.status);
  check(cfg.body.indexOf('ol_break_glass_token') >= 0, 'аварийный токен берётся из sessionStorage вкладки');

  const browser = await chromium.launch({ channel: 'msedge', headless: true });

  // 1. Гость без входа.
  const guestCtx = await browser.newContext();
  const guestPage = await guestCtx.newPage();
  const guestErrors = [];
  guestPage.on('pageerror', e => guestErrors.push(e.message));
  const anonUnauthorized = [];
  guestPage.on('response', r => { if (r.status() === 401 && r.url().indexOf('/api/') >= 0) anonUnauthorized.push(r.url()); });
  await guestPage.goto(WEBUI + '/index.html', { waitUntil: 'domcontentloaded' });
  await guestPage.waitForTimeout(2500);
  const anon = await snapshot(guestPage);
  check(anon.overlayVisible, 'без входа показан экран входа');
  check(!anon.authenticated, 'без входа сессии нет', 'authenticated=' + anon.authenticated);
  check(anonUnauthorized.length === 0, 'до входа страница не дёргает /api/ (нет 401 в консоли и логах)',
    JSON.stringify(anonUnauthorized.slice(0, 4)));
  check(anon.adminOnlyVisible === 0, 'без входа ни одно админское действие не видно',
    'видно ' + anon.adminOnlyVisible + ' из ' + anon.adminOnlyTotal);
  await guestCtx.close();

  // 2. Администратор.
  const adminCtx = await browser.newContext();
  const adminPage = await adminCtx.newPage();
  const adminErrors = [];
  const unauthorized = [];
  adminPage.on('pageerror', e => adminErrors.push(e.message));
  adminPage.on('response', r => { if (r.status() === 401) unauthorized.push(r.url()); });
  await adminPage.goto(WEBUI + '/index.html', { waitUntil: 'domcontentloaded' });
  await adminPage.waitForTimeout(2500);
  await adminPage.fill('#authName', ADMIN_USER);
  await adminPage.fill('#authPassword', adminPassword());
  await adminPage.click('#authSubmit');
  await adminPage.waitForTimeout(4000);
  const admin = await snapshot(adminPage);
  check(!admin.overlayVisible, 'после входа экран входа скрыт');
  check(admin.authenticated && admin.role === 'admin', 'сессия администратора', 'role=' + admin.role);
  check(admin.isAdmin, 'Auth.isAdmin() = true');
  check(admin.chipText.indexOf('администратор') >= 0, 'в шапке чип «администратор»', admin.chipText.trim().slice(0, 60));
  check(admin.adminOnlyVisible > 0, 'админские действия видны', 'видно ' + admin.adminOnlyVisible + ' из ' + admin.adminOnlyTotal);
  check(admin.usersSectionVisible, 'раздел «Пользователи» виден администратору');
  await adminPage.waitForTimeout(1500);
  const adminAfter = await snapshot(adminPage);
  check(adminAfter.sessionsRows > 0, 'R225: активные сессии видны администратору', 'строк: ' + adminAfter.sessionsRows);
  await adminPage.evaluate(() => {
    const sec = document.getElementById('section-users');
    if (sec && sec.scrollIntoView) sec.scrollIntoView();
  });
  await adminPage.waitForTimeout(1200);
  const usersRows = await adminPage.evaluate(() => {
    const tb = document.getElementById('usersTableBody');
    return tb ? Array.prototype.map.call(tb.querySelectorAll('tr'), tr => tr.textContent.trim()).filter(Boolean) : [];
  });
  check(usersRows.some(r => r.indexOf(ADMIN_USER) >= 0), 'в разделе «Пользователи» есть администратор',
    usersRows.join(' | ').slice(0, 120));
  console.log('   401 у администратора: ' + JSON.stringify(unauthorized.slice(0, 5)));
  check(adminErrors.length === 0, 'нет ошибок JS у администратора', adminErrors.slice(0, 2).join('; '));

  // 3. Гость с ролью viewer (создаём через REST от имени администратора).
  const adm = await adminSession();
  const created = await rest('/api/v1/auth/users', {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': adm.csrf, Cookie: adm.cookie },
    body: JSON.stringify({ name: GUEST_USER, password: GUEST_PASS, role: 'viewer' }),
  });
  check(created.status === 200, 'гость создан через /auth/users', 'HTTP ' + created.status);

  const viewerCtx = await browser.newContext();
  const viewerPage = await viewerCtx.newPage();
  const viewerErrors = [];
  const viewerUnauthorized = [];
  viewerPage.on('pageerror', e => viewerErrors.push(e.message));
  viewerPage.on('response', r => { if (r.status() === 401) viewerUnauthorized.push(r.url()); });
  await viewerPage.goto(WEBUI + '/index.html', { waitUntil: 'domcontentloaded' });
  await viewerPage.waitForTimeout(2500);
  await viewerPage.fill('#authName', GUEST_USER);
  await viewerPage.fill('#authPassword', GUEST_PASS);
  await viewerPage.click('#authSubmit');
  await viewerPage.waitForTimeout(4000);
  const viewer = await snapshot(viewerPage);
  check(viewer.authenticated && viewer.role === 'viewer', 'гость вошёл с ролью viewer', 'role=' + viewer.role);
  check(!viewer.isAdmin, 'гость не считается администратором');
  check(viewer.chipText.indexOf('только чтение') >= 0, 'в шапке чип «только чтение»', viewer.chipText.trim().slice(0, 60));
  check(viewer.adminOnlyVisible === 0, 'гость не видит админских действий',
    'видно ' + viewer.adminOnlyVisible + ' из ' + viewer.adminOnlyTotal);
  check(!viewer.usersSectionVisible, 'раздел «Пользователи» скрыт от гостя');
  check(viewer.sessionsRows === 0, 'R225: гость не видит активные сессии', 'строк: ' + viewer.sessionsRows);
  console.log('   401 у гостя: ' + JSON.stringify(viewerUnauthorized.slice(0, 5)));
  check(viewerErrors.length === 0, 'нет ошибок JS у гостя', viewerErrors.slice(0, 2).join('; '));

  // Гость не может мутировать (сервер, а не только UI).
  const viewerLogin = await rest('/api/v1/auth/login', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ name: GUEST_USER, password: GUEST_PASS }),
  });
  const vCookie = (viewerLogin.headers.get('set-cookie') || '').split(';')[0];
  const vMutation = await rest('/api/v1/cppworker/model-profiles/r222-guest-violation', {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': viewerLogin.json.csrf, Cookie: vCookie },
    body: '{}',
  });
  check(vMutation.status === 403, 'мутация гостя отклонена сервером', 'HTTP ' + vMutation.status);

  // 4. Уборка: удаляем гостя.
  const del = await rest('/api/v1/auth/users?name=' + encodeURIComponent(GUEST_USER), {
    method: 'DELETE',
    headers: { 'X-CSRF-Token': adm.csrf, Cookie: adm.cookie },
  });
  check(del.status === 200, 'гость удалён', 'HTTP ' + del.status);

  await viewerCtx.close();
  await adminCtx.close();
  await browser.close();

  console.log('\nИТОГ: ' + (fails.length ? 'ПРОВАЛ (' + fails.length + ')' : 'OK — вход и роли работают'));
  for (const f of fails) console.log('  - ' + f);
  process.exit(fails.length ? 1 : 0);
})().catch(e => {
  console.log('FATAL: ' + e.message);
  process.exit(1);
});

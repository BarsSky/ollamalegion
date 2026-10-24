// probe-session-persist-r223.js — R223: вход в WebUI переживает рестарт балансера.
//
// ЗАЧЕМ ОТДЕЛЬНЫЙ ПРОБНИК. Юнит-тест проверяет запись/чтение файла сессий, но не
// проверяет, что РЕАЛЬНЫЙ балансер читает этот файл при старте и что браузерная
// cookie остаётся валидной. Здесь ровно этот путь: вход → рестарт контейнера →
// тот же cookie снова авторизован, CSRF принимается на мутации.
//
// Запуск (на хосте со стендом):
//   node scripts/probe-session-persist-r223.js
// Переменные:
//   BALANCER    базовый URL админ-API (по умолчанию http://127.0.0.1:18081)
//   CONTAINER   контейнер балансера (по умолчанию ol-stack-balancer)
//   ADMIN_USER  пользователь (иначе LB_ADMIN_USER из deployments/.env, затем "admin")
//   ADMIN_PASSWORD  пароль (иначе LB_ADMIN_PASSWORD из deployments/.env)
//   SESSIONS_FILE   путь файла сессий внутри контейнера (/app/data/sessions.json)
//
// Код возврата: 0 — сессия пережила рестарт, 1 — нет.
const fs = require('fs');
const path = require('path');
const { execSync } = require('child_process');

const BALANCER = process.env.BALANCER || 'http://127.0.0.1:18081';
const CONTAINER = process.env.CONTAINER || 'ol-stack-balancer';
const SESSIONS_FILE = process.env.SESSIONS_FILE || '/app/data/sessions.json';

function envValueFromDeployments(key) {
  try {
    const envFile = path.join(__dirname, '..', 'deployments', '.env');
    const line = fs.readFileSync(envFile, 'utf8').split(/\r?\n/).find(l => l.startsWith(key + '='));
    return line ? line.slice(key.length + 1).trim() : '';
  } catch (e) { return ''; }
}

// Имя администратора — из окружения или из deployments/.env (а не хардкод в коде:
// логин оператора — не часть проекта и в репозитории не хранится).
const ADMIN_USER = process.env.ADMIN_USER || envValueFromDeployments('LB_ADMIN_USER') || 'admin';

function adminPassword() {
  if (process.env.ADMIN_PASSWORD) return process.env.ADMIN_PASSWORD;
  const envFile = path.join(__dirname, '..', 'deployments', '.env');
  const line = fs.readFileSync(envFile, 'utf8').split(/\r?\n/).find(l => l.startsWith('LB_ADMIN_PASSWORD='));
  if (!line) throw new Error('LB_ADMIN_PASSWORD не найден в deployments/.env');
  return line.slice('LB_ADMIN_PASSWORD='.length).trim();
}

const fails = [];
function check(ok, what, detail) {
  console.log((ok ? 'PASS ' : 'FAIL ') + what + (detail ? ' :: ' + detail : ''));
  if (!ok) fails.push(what + (detail ? ' :: ' + detail : ''));
}

function shell(cmd) {
  try {
    return { ok: true, out: execSync(cmd, { encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] }).trim() };
  } catch (e) {
    return { ok: false, out: ((e.stdout || '') + (e.stderr || '')).trim() };
  }
}

async function request(pathname, opts) {
  const res = await fetch(BALANCER + pathname, opts);
  const text = await res.text();
  let json = {};
  try { json = JSON.parse(text); } catch (e) { /* не json */ }
  return { status: res.status, json, text, headers: res.headers };
}

function cookieOf(res) {
  return (res.headers.get('set-cookie') || '').split(';')[0];
}

async function waitHealthy(timeoutMs) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    try {
      const r = await request('/health');
      if (r.status === 200) return true;
    } catch (e) { /* балансер ещё поднимается */ }
    await new Promise(r => setTimeout(r, 1000));
  }
  return false;
}

(async () => {
  console.log('R223: вход в WebUI переживает рестарт балансера\n');

  const login = await request('/api/v1/auth/login', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ name: ADMIN_USER, password: adminPassword() }),
  });
  check(login.status === 200, 'вход администратора', 'HTTP ' + login.status);
  const cookie = cookieOf(login);
  const csrf = login.json.csrf || '';
  check(!!cookie && !!csrf, 'сессия и CSRF получены');

  const me1 = await request('/api/v1/auth/me', { headers: { Cookie: cookie } });
  check(me1.json.authenticated === true, 'до рестарта сессия активна');

  const fileBefore = shell(`docker exec ${CONTAINER} ls -l ${SESSIONS_FILE}`);
  check(fileBefore.ok, 'файл сессий существует в контейнере', fileBefore.out.split(/\s+/).slice(0, 1).join(''));
  if (fileBefore.ok) {
    check(/^-rw-------/.test(fileBefore.out), 'права файла сессий 0600', fileBefore.out.trim().slice(0, 40));
  }

  console.log('\n-- рестарт балансера: docker restart ' + CONTAINER);
  const restarted = shell(`docker restart ${CONTAINER}`);
  check(restarted.ok, 'балансер перезапущен');
  check(await waitHealthy(120000), 'балансер снова отвечает /health');

  const me2 = await request('/api/v1/auth/me', { headers: { Cookie: cookie } });
  check(me2.json.authenticated === true, 'после рестарта та же cookie авторизована',
    'user=' + ((me2.json.user && me2.json.user.name) || '-'));

  // CSRF тоже обязан пережить рестарт, иначе первая же мутация вернёт 403.
  const probeName = 'r223-persist-probe';
  const put = await request('/api/v1/cppworker/model-profiles/' + probeName, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json', Cookie: cookie, 'X-CSRF-Token': csrf },
    body: '{}',
  });
  check(put.status === 200, 'мутация с прежним CSRF принимается', 'HTTP ' + put.status);

  // Уборка: удаляем пробный профиль тем же CSRF.
  const del = await request('/api/v1/cppworker/model-profiles/' + probeName, {
    method: 'DELETE',
    headers: { Cookie: cookie, 'X-CSRF-Token': csrf },
  });
  check(del.status === 200 || del.status === 404, 'пробный профиль убран', 'HTTP ' + del.status);

  const logout = await request('/api/v1/auth/logout', {
    method: 'POST',
    headers: { Cookie: cookie, 'X-CSRF-Token': csrf },
  });
  check(logout.status === 200, 'выход', 'HTTP ' + logout.status);
  const me3 = await request('/api/v1/auth/me', { headers: { Cookie: cookie } });
  check(me3.json.authenticated !== true, 'после выхода сессия недействительна');

  console.log('\nИТОГ: ' + (fails.length ? 'ПРОВАЛ (' + fails.length + ')' : 'OK — вход пережил рестарт, файл сессий приватный'));
  for (const f of fails) console.log('  - ' + f);
  process.exit(fails.length ? 1 : 0);
})().catch(e => {
  console.log('FATAL: ' + e.message);
  process.exit(1);
});

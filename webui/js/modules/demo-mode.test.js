// demo-mode.test.js — R232 (2026-10-24): демо-режим WebUI.
//
// Запуск: node webui/js/modules/demo-mode.test.js
//
// ЧТО ПРОВЕРЯЕМ:
//   1. Демо включается ТОЛЬКО флагом DEMO_MODE (из env через config.js):
//      при false модуль не перехватывает fetch и ничего не делает;
//   2. сценарий идёт по фазам: загрузка модели → клиенты → очередь →
//      назначение донора (22 слоя RPC0) → AutoTune (рекомендация, reload,
//      проверка) → optimal;
//   3. API-роутер отдаёт синтетику нужной формы: /auth/me (гость viewer, без
//      входа), /cluster (мастер+донор), очередь, сессии, autotune;
//   4. мутации в демо запрещены (403 read-only) — гость не может ничего менять.

'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');
const NodeModule = require('module');

const SRC = path.join(__dirname, 'demo-mode.js');

let passed = 0;
const failures = [];
function check(name, fn) {
  try { fn(); passed++; console.log('  \u2713 ' + name); }
  catch (e) { failures.push(name + ': ' + (e && e.message)); console.error('  \u2717 ' + name + ' - ' + (e && e.message)); }
}

// loadDemo — модуль читает window.WEBUI_CONFIG при загрузке, поэтому для
// каждого сценария грузим его заново (как отдельную страницу).
function loadDemo(flagOn, opts) {
  opts = opts || {};
  delete global.DemoMode;
  global.window = global;
  global.WEBUI_CONFIG = { DEMO_MODE: flagOn, API_BASE: '' };
  if (opts.noLocation) { delete global.location; } else { global.location = { origin: 'http://demo.local' }; }
  if (opts.noDocument) { delete global.document; } else { delete global.document; }
  const origFetch = function () { return Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve({}) }); };
  global.fetch = origFetch;
  // В браузере WebSocket есть всегда; в Node 22 он тоже есть, но тест
  // подставляет явную заглушку, чтобы installWebSocket() имел что заменить.
  global.WebSocket = function DemoNativeWebSocket() {};
  // EventSource в Node нет — подставляем «нативный», чтобы проверить подмену
  // (notifications.js открывает SSE /api/v1/events, и он не идёт через fetch).
  global.EventSource = function DemoNativeEventSource() { this.url = 'native'; };
  delete global.__demoFetchInstalled;
  delete global.__demoWsInstalled;
  delete global.__demoEventSourceInstalled;

  const m = new NodeModule(SRC, module);
  m._compile(fs.readFileSync(SRC, 'utf8'), SRC);
  return global.DemoMode;
}

console.log('R232: демо-режим WebUI');

// --- 1. Выключенный режим ------------------------------------------------
check('без DEMO_MODE модуль ничего не включает и не трогает fetch', function () {
  const dm = loadDemo(false);
  assert.strictEqual(dm.enabled, false, 'enabled должен быть false');
  assert.strictEqual(global.__demoFetchInstalled, undefined, 'fetch не должен перехватываться');
  assert.strictEqual(global.__demoWsInstalled, undefined, 'WebSocket не должен подменяться');
  // Роутер всё равно доступен для тестов/диагностики, но страница его не использует.
  assert.strictEqual(typeof dm.route, 'function');
});

// --- 2. Включённый режим: фазы сценария ----------------------------------
const dm = loadDemo(true, { noDocument: true });
assert.strictEqual(dm.enabled, true, 'DEMO_MODE=true должен включать демо');
assert.ok(global.__demoFetchInstalled === true, 'fetch должен быть перехвачен');
assert.ok(global.__demoWsInstalled === true, 'WebSocket должен быть заглушён');
assert.ok(global.__demoEventSourceInstalled === true, 'EventSource должен быть заглушён');

const s2 = dm.state(2);
const s5 = dm.state(5);
const s20 = dm.state(20);
const s50 = dm.state(50);
const s85 = dm.state(85);
const s100 = dm.state(100);

check('фаза boot: модель грузится, клиентов ещё почти нет', function () {
  assert.strictEqual(s5.phase.id, 'boot');
  assert.strictEqual(s5.loading, true);
  assert.ok(s5.loadProgress >= 0 && s5.loadProgress <= 100);
  assert.ok(s5.clientCount <= 1, 'на старте (5 с) успевает подключиться максимум 1 клиент');
});

check('клиенты подключаются и появляются запросы', function () {
  assert.ok(s20.clientCount >= 2, 'к 20 с должно быть ≥2 клиента, got ' + s20.clientCount);
  assert.ok(s20.requests.length > 0, 'запросы должны идти');
  assert.ok(s20.queueStats.current_size > 0, 'до донора слот один — очередь обязана быть');
});

check('очередь: ожидающие запросы имеют позицию и время ожидания', function () {
  const pending = s20.queueAll.filter(q => q.status === 'pending');
  assert.ok(pending.length > 0, 'ожидающие должны быть в queueDetails');
  assert.ok(pending[0].position >= 1, 'у ожидающего есть позиция');
  assert.ok(pending[0].waitMs >= 0, 'у ожидающего есть время ожидания');
  const stats = s20.queueStats;
  assert.strictEqual(stats.pending_count, s20.pending.length);
  assert.strictEqual(stats.current_size, s20.pending.length);
});

check('балансер распределяет: у назначенных запросов есть backend', function () {
  const assigned = s20.requests.filter(r => r.start <= s20.t);
  assert.ok(assigned.length > 0, 'должны быть назначенные запросы');
  assert.ok(assigned.every(r => r.backend === 'master'), 'до донора назначение только на мастер');
});

check('назначение донора: 22 слоя RPC0 и роль donor', function () {
  assert.strictEqual(s50.donorOn, true);
  assert.strictEqual(s50.rpcLayers, 22);
  assert.strictEqual(s50.gpuLayers, 48);
  const rpc = s50.layers.filter(l => l.device === 'RPC0');
  assert.strictEqual(rpc.length, 1);
  assert.strictEqual(rpc[0].count, 22);
  assert.ok(rpc[0].node, 'у RPC-слоя должен быть адрес узла');
  const donor = s50.backends.find(b => b.id === 'rpc-donor');
  assert.ok(donor, 'донор должен быть в списке бэкендов');
  assert.strictEqual(donor.role, 'donor');
  assert.ok(s50.requests.some(r => r.backend === 'donor'), 'часть запросов должна идти на донора');
});

check('AutoTune: рекомендация KV q4_0 → f16 до применения', function () {
  const at = s85.autoTune;
  assert.strictEqual(at.overallSeverity, 'warning');
  const rec = at.models[0].recommendations.find(r => r.category === 'kv_cache');
  assert.ok(rec, 'должна быть рекомендация kv_cache');
  assert.strictEqual(rec.recommendedKvCache, 'f16');
  assert.ok(/бюджет/i.test(rec.suggestion), 'в рекомендации должен быть расчёт бюджета');
});

check('AutoTune: после reload анализ optimal', function () {
  assert.strictEqual(s100.tuned, true);
  assert.strictEqual(s100.autoTune.overallSeverity, 'ok');
  assert.strictEqual(s100.autoTune.models[0].isSubOptimal, false);
  assert.strictEqual(s100.model.kvCacheType, 'f16');
});

check('лента событий содержит клиентов, очередь, донора и AutoTune', function () {
  const kinds20 = s20.events.map(e => e.kind);
  const kinds50 = s50.events.map(e => e.kind);
  const kinds100 = s100.events.map(e => e.kind);
  assert.ok(kinds20.includes('client'), 'нет события подключения клиента');
  assert.ok(kinds20.includes('queue'), 'нет события очереди (в фазе очереди)');
  assert.ok(kinds50.includes('donor'), 'нет события назначения донора (в фазе донора)');
  assert.ok(kinds100.includes('client'), 'нет сводки по клиентам в steady-фазе');
  assert.ok(kinds100.includes('autotune'), 'нет события AutoTune');
  assert.ok(s100.events[0].at >= s100.events[s100.events.length - 1].at, 'лента отсортирована от новых к старым');
});

// --- 3. API-роутер --------------------------------------------------------
check('/auth/me — гостевой viewer без входа', function () {
  const r = dm.responseBody('/api/v1/auth/me', 'GET', s20);
  assert.strictEqual(r.status, 200);
  assert.strictEqual(r.body.authenticated, true);
  assert.strictEqual(r.body.usersConfigured, true);
  assert.strictEqual(r.body.user.role, 'viewer');
  assert.ok(!r.body.tokenAuth, 'демо не должно выдавать админский токен');
});

check('/cluster — мастер, донор и image-воркер', function () {
  const r = dm.responseBody('/api/v1/cluster', 'GET', s50);
  assert.strictEqual(r.status, 200);
  assert.strictEqual(r.body.backends.length, 3);
  const ids = r.body.backends.map(b => b.id);
  assert.ok(ids.includes('cppworker-gpu-bundled-agent'));
  assert.ok(ids.includes('rpc-donor'));
  const master = r.body.backends.find(b => b.id === 'cppworker-gpu-bundled-agent');
  assert.ok(master.gpu.memoryTotal > 0, 'у мастера есть метрики GPU');
  assert.ok(master.loadedModels.length > 0, 'у мастера должна быть загруженная модель');
});

check('/backends — autoTune и rpcLayout для WebUI', function () {
  const r = dm.responseBody('/api/v1/backends', 'GET', s85);
  assert.strictEqual(r.status, 200);
  const master = r.body.backends.find(b => b.id === 'cppworker-gpu-bundled-agent');
  assert.ok(master.autoTune, 'autoTune должен быть в ответе (иначе бейдж не нарисуется)');
  assert.ok(Array.isArray(master.autoTune.models));
  assert.ok(master.rpcLayout && master.rpcLayout.enabled, 'rpcLayout должен быть включён после донора');
});

check('/queue/stats и /queue/details соответствуют состоянию', function () {
  const stats = dm.responseBody('/api/v1/queue/stats', 'GET', s20);
  assert.strictEqual(stats.status, 200);
  assert.strictEqual(stats.body.current_size, s20.pending.length);
  const details = dm.responseBody('/api/v1/queue/details', 'GET', s20);
  assert.strictEqual(details.status, 200);
  assert.ok(Array.isArray(details.body.all));
  assert.ok(details.body.all.length >= s20.pending.length);
});

check('/sessions — клиенты как сессии с протоколом', function () {
  const r = dm.responseBody('/api/v1/sessions', 'GET', s20);
  assert.strictEqual(r.status, 200);
  assert.ok(r.body.sessions.length >= 2);
  const cline = r.body.sessions.find(s => s.clientName === 'Cline');
  assert.ok(cline, 'должна быть сессия Cline');
  assert.ok(['openai', 'ollama'].includes(cline.protocol));
});

check('/admin/autotune/history — применённая конфигурация', function () {
  const r = dm.responseBody('/api/v1/admin/autotune/history', 'GET', s100);
  assert.strictEqual(r.status, 200);
  assert.ok(r.body.events.some(e => e.type === 'reload_succeeded'), 'история должна содержать подтверждённый reload');
});

check('мутации в демо запрещены (read-only 403)', function () {
  ['POST', 'PUT', 'DELETE', 'PATCH'].forEach(function (m) {
    const r = dm.responseBody('/api/v1/config', m, s20);
    assert.strictEqual(r.status, 403, m + ' должен быть запрещён');
    assert.ok(r.body.error.indexOf('read-only') >= 0);
  });
});

check('неизвестная ручка не ломает страницу (200 demo)', function () {
  const r = dm.responseBody('/api/v1/unknown-xyz', 'GET', s20);
  assert.strictEqual(r.status, 200);
  assert.strictEqual(r.body.demo, true);
});

// R232: иначе setup-wizard.js открывает «Мастер настройки» первого запуска и
// закрывает сценарий — гость видит форму вместо демонстрации.
check('/cluster/models/loaded показывает загруженную модель в сценарии', function () {
  const r = dm.responseBody('/api/v1/cluster/models/loaded', 'GET', s50);
  assert.strictEqual(r.status, 200);
  assert.strictEqual(r.body.count, 1, 'после загрузки модель должна считаться загруженной');
  assert.strictEqual(r.body.models[0].name, 'Qwen3.8-27B-UD-Q4_K_M');
  assert.strictEqual(r.body.models[0].kvCacheType, 'q4_0', 'до AutoTune кэш ещё q4_0');
  const tuned = dm.responseBody('/api/v1/cluster/models/loaded', 'GET', s100);
  assert.strictEqual(tuned.body.models[0].kvCacheType, 'f16', 'после AutoTune кэш f16');
  const boot = dm.responseBody('/api/v1/cluster/models/loaded', 'GET', s2);
  assert.strictEqual(boot.body.count, 0, 'пока модель грузится, загруженных ещё нет');
  const loading = dm.responseBody('/api/v1/cluster/models/loading', 'GET', s2);
  assert.strictEqual(loading.body.count, 1);
});

check('/cluster/config помечает стенд настроенным (без мастера первого запуска)', function () {
  ['/api/v1/config', '/api/v1/cluster/config'].forEach(function (p) {
    const r = dm.responseBody(p, 'GET', s20);
    assert.strictEqual(r.status, 200, p);
    assert.strictEqual(r.body.initialized, true, p + ': initialized должен быть true');
    assert.strictEqual(r.body.backendType, 'llama_cpp', p);
  });
});

// R232: monitor/api.js склеивает АБСОЛЮТНЫЙ url (location.origin + '/api/...').
// Раньше stripBase падал на неопределённом ORIGIN (ReferenceError ловился
// catch → null), и такие запросы уходили на реальный сервер: гость видел 401
// и пустые панели.
check('router понимает абсолютный URL своего origin', function () {
  // loadDemo подставляет location.origin = 'http://demo.local'.
  const r = dm.route('GET', 'http://demo.local/api/v1/cluster?x=1', s50);
  assert.ok(r, 'абсолютный URL своего origin должен попадать в демо-роутер');
  assert.strictEqual(r.status, 200);
  assert.strictEqual(r.body.backends.length, 3);
  assert.ok(dm.route('GET', 'http://example.invalid/api/v1/cluster', s50) === null,
    'чужой origin трогать нельзя');
  assert.ok(dm.route('GET', '/healthz', s50) === null, 'не-/api/ пути не перехватываются');
});

// --- 4. Парсинг флага -----------------------------------------------------
check('DEMO_MODE понимает 1/true/on и НЕ включает на 0/false', function () {
  [1, true, '1', 'true', 'TRUE', 'yes', 'on'].forEach(function (v) {
    const d = loadDemo(v, { noDocument: true });
    assert.strictEqual(d.enabled, true, 'должно включиться: ' + v);
  });
  [0, false, '0', 'false', 'off', undefined, null, ''].forEach(function (v) {
    const d = loadDemo(v, { noDocument: true });
    assert.strictEqual(d.enabled, false, 'должно быть выключено: ' + v);
  });
});

console.log('');
if (failures.length) {
  console.error('ИТОГ: FAIL (' + failures.length + ')');
  failures.forEach(f => console.error(' - ' + f));
  process.exit(1);
}
console.log('ИТОГ: PASS (' + passed + ' проверок)');

// verify-lazy-profile-r229.js — R229: живая проверка «профиль модели применяется
// при ЛЕНИВОЙ загрузке» (92d0852/R228 + R229-лестница окна).
//
// Что делает:
//   1. выгружает модель на воркере (чтобы следующая загрузка была ленивой);
//   2. отправляет запрос ЧЕРЕЗ БАЛАНСЕР (тот же путь, что у клиента);
//   3. читает /api/models воркера и сверяет фактическое окно/тип KV с профилем.
//
// Живой дефект до R228/R229 (образ gpu-r227-stall, лог воркера):
//   lazy-loading model from filesystem model=gemma-4-E4B-it-Q4_K_M
//     load_n_ctx=32768 n_ctx_source="env CPPWORKER_CTX_SIZE"
//   lazy-load: adaptive SelectStrategy applied ... n_ctx:32768, kv_cache_type:"f16"
// при профиле модели: contextLength=12288, kvCacheType=q4_0. То есть нагрузка шла
// на env-дефолтах, а не на профиле.
//
// Запуск: node scripts/verify-lazy-profile-r229.js
// Переменные: MODEL, WANT_NCTX, WANT_KV, WORKER, BAL, TOKEN.
'use strict';
const http = require('http');

const MODEL = process.env.MODEL || 'gemma-4-E4B-it-Q4_K_M';
const WANT_NCTX = Number(process.env.WANT_NCTX || 12288);
const WANT_KV = process.env.WANT_KV || 'q4_0';
const WORKER = process.env.WORKER || 'http://127.0.0.1:18092';
const BAL = process.env.BAL || 'http://127.0.0.1:18080';
const TOKEN = process.env.TOKEN || 'changeme-bundled-with-agent-token';
const LOAD_TIMEOUT_MS = Number(process.env.LOAD_TIMEOUT_MS || 300000);
// VIA=balancer (по умолчанию) или VIA=worker — прямой вызов воркера, когда
// балансер уводит запрос на другой бэкенд (донор).
const VIA = process.env.VIA || 'balancer';

function call(method, base, path, body) {
  const m = /^http:\/\/([^:/]+):(\d+)/.exec(base);
  if (!m) throw new Error('bad url ' + base);
  return new Promise(resolve => {
    const data = body === undefined ? null : Buffer.from(JSON.stringify(body));
    const headers = { 'X-API-Token': TOKEN };
    if (data) { headers['Content-Type'] = 'application/json'; headers['Content-Length'] = data.length; }
    const t0 = Date.now();
    const req = http.request({ host: m[1], port: Number(m[2]), path, method, headers, agent: false }, res => {
      let text = '';
      res.on('data', d => { text += d; });
      res.on('end', () => resolve({ status: res.statusCode, text, ms: Date.now() - t0 }));
    });
    req.on('error', e => resolve({ status: 0, text: '', err: e.code || e.message, ms: Date.now() - t0 }));
    req.setTimeout(LOAD_TIMEOUT_MS, () => req.destroy(new Error('timeout')));
    if (data) req.write(data);
    req.end();
  });
}

const fails = [];
function check(ok, what, detail) {
  console.log((ok ? 'PASS ' : 'FAIL ') + what + (detail ? ' :: ' + detail : ''));
  if (!ok) fails.push(what + (detail ? ' :: ' + detail : ''));
}

function psOf(text) {
  try { return (JSON.parse(text).models || []).find(x => x.name === MODEL) || null; }
  catch (e) { return null; }
}

(async () => {
  console.log('R229: ленивая загрузка должна применять профиль модели (' + MODEL + ')\n');

  const health = await call('GET', WORKER, '/api/health');
  check(health.status === 200, 'воркер отвечает /api/health', 'HTTP ' + health.status + (health.err ? ' ' + health.err : ''));
  if (health.status !== 200) process.exit(2);

  // 1. Выгрузка — следующая загрузка обязательно будет ленивой.
  const un = await call('POST', WORKER, '/api/models/unload?name=' + encodeURIComponent(MODEL) + '&force=true');
  check(un.status === 200 || un.status === 404, 'модель выгружена (или уже была выгружена)', 'HTTP ' + un.status);
  if (un.status === 200) {
    const after = psOf((await call('GET', WORKER, '/api/models')).text);
    if (after) console.log('  ВНИМАНИЕ: после unload модель всё ещё в /api/models');
  }

  // 2. Запрос через балансер — путь клиента. Холодная загрузка + генерация.
  console.log('  путь: ' + (VIA === 'worker' ? 'напрямую в воркер' : 'через балансер'));
  const chat = await call('POST', VIA === 'worker' ? WORKER : BAL, '/api/chat', {
    model: MODEL, stream: false,
    messages: [{ role: 'user', content: 'Ответь одним словом: ок.' }],
    options: { num_predict: 8 },
  });
  check(chat.status === 200, 'запрос (lazy-load + генерация) через ' + (VIA === 'worker' ? 'воркер' : 'балансер'), 'HTTP ' + chat.status + ' за ' + chat.ms + 'мс');

  // 3. Факт воркера: окно и KV из /api/models.
  const ps = psOf((await call('GET', WORKER, '/api/models')).text);
  if (!ps) {
    check(false, 'модель видна в /api/models воркера', 'нет записи ' + MODEL);
    process.exit(1);
  }
  check(ps.context_size === WANT_NCTX, 'окно модели взято из профиля (' + WANT_NCTX + ')',
    'факт context_size=' + ps.context_size + ' (env-дефолт 32768 — это и есть дефект)');
  check(ps.kv_cache_type === WANT_KV, 'kvCacheType из профиля (' + WANT_KV + ')', 'факт ' + ps.kv_cache_type);
  check(ps.parallel === 1, 'parallel из профиля (1)', 'факт ' + ps.parallel);

  const layout = Array.isArray(ps.device_layers_exact) && ps.device_layers_exact.length
    ? ps.device_layers_exact.map(d => d.device + ':' + (d.count || 0)).join(' ')
    : 'не отдан';
  console.log('\nИтог: context_size=' + ps.context_size + ' kv=' + ps.kv_cache_type +
    ' parallel=' + ps.parallel + ' n_layers=' + ps.n_layers + ' layout=' + layout);
  console.log(fails.length === 0 ? 'РЕЗУЛЬТАТ: профиль применён при ленивой загрузке'
    : 'РЕЗУЛЬТАТ: провалов ' + fails.length);
  process.exit(fails.length === 0 ? 0 : 1);
})();

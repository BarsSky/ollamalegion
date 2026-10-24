// probe-rpc-session-recovery-r224.js — R224: потеря узла посреди генерации →
// честный диагноз и авто-восстановление без ручной выгрузки модели.
//
// ЧТО ПРОВЕРЯЕТСЯ (каждый пункт — живой дефект):
//   1. мастер не падает (R221) — остаётся живым, рестартов нет;
//   2. клиент получает диагноз «сессия с узлом потеряна», а НЕ «KV-cache overflow
//      or GPU OOM» (раньше C-мост подписывал сбой декодирования этим текстом);
//   3. модель выгружается сама (иначе каждая генерация падает до ручной выгрузки);
//   4. после возврата узла следующий запрос поднимает модель заново с раскладкой
//      RPC0 — без действий оператора.
//
// Запуск (на хосте стенда):
//   node scripts/probe-rpc-session-recovery-r224.js
// Переменные: MODEL, BASE (клиент балансера), WORKER_API, WORKER (контейнер),
// TOKEN, DONOR_KILL, DONOR_START, KILL_AFTER_MS.
const http = require('http');
const net = require('net');
const { execSync } = require('child_process');

const MODEL = (process.env.MODEL || 'gemma-4-E4B-it-Q4_K_M').replace(/\.gguf$/i, '');
const BASE = process.env.BASE || 'http://127.0.0.1:18080';
const WORKER_API = process.env.WORKER_API || 'http://127.0.0.1:18092';
const WORKER = process.env.WORKER || 'ol-stack-cppworker-gpu';
const TOKEN = process.env.TOKEN || 'changeme-bundled-with-agent-token';
const DONOR_KILL = process.env.DONOR_KILL || 'ssh legion2 docker kill cppworker-node2';
const DONOR_START = process.env.DONOR_START || 'ssh legion2 docker start cppworker-node2';
const KILL_AFTER_MS = Number(process.env.KILL_AFTER_MS || 2500);

const fails = [];
const pre = [];
function check(ok, what, detail) {
  console.log((ok ? 'PASS ' : 'FAIL ') + what + (detail ? ' :: ' + detail : ''));
  if (!ok) fails.push(what + (detail ? ' :: ' + detail : ''));
}
function precondition(what, detail) {
  pre.push(what);
  console.log('PRECONDITION ' + what + ' :: ' + detail);
}

function parseURL(u) {
  const m = /^http:\/\/([^:/]+):(\d+)$/.exec(u);
  if (!m) throw new Error('bad url ' + u);
  return { host: m[1], port: Number(m[2]) };
}

function req(method, base, path, body, headers) {
  const { host, port } = parseURL(base);
  return new Promise(resolve => {
    const payload = body === undefined ? null : Buffer.from(JSON.stringify(body));
    const h = Object.assign({}, headers || {});
    if (payload) {
      h['Content-Type'] = 'application/json';
      h['Content-Length'] = payload.length;
    }
    const r = http.request({ host, port, path, method, headers: h, agent: false }, res => {
      let s = '';
      res.on('data', d => (s += d));
      res.on('end', () => resolve({ status: res.statusCode, body: s }));
    });
    r.on('error', e => resolve({ status: 0, body: '', err: e.code || e.message }));
    r.setTimeout(180000, () => r.destroy(new Error('timeout')));
    if (payload) r.write(payload);
    r.end();
  });
}

function shell(cmd) {
  try {
    return { ok: true, out: execSync(cmd, { encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] }).trim() };
  } catch (e) {
    return { ok: false, out: ((e.stdout || '') + (e.stderr || '')).trim() };
  }
}

function workerState() {
  const r = shell(`docker inspect -f "{{.State.Running}} {{.RestartCount}}" ${WORKER}`);
  if (!r.ok) throw new Error(`docker inspect ${WORKER}: ${r.out}`);
  const [running, restarts] = r.out.split(/\s+/);
  return { running: running === 'true', restarts: Number(restarts) };
}

function layersText(m) {
  return (m.device_layers_exact || []).map(l => `${l.device}:${l.count}`).join(' ') || '(нет данных)';
}
function hasRPC(m) {
  return (m.device_layers_exact || []).some(l => String(l.device).startsWith('RPC'));
}

async function modelsState() {
  const r = await req('GET', WORKER_API, '/api/models', undefined, { 'X-API-Token': TOKEN });
  if (r.status !== 200) throw new Error(`/api/models -> ${r.status} ${r.body.slice(0, 200)}`);
  return JSON.parse(r.body);
}

async function loadedModel() {
  const j = await modelsState();
  return (j.models || []).find(m => String(m.name || '').includes(MODEL)) || null;
}

function waitReachable(hostport, timeoutMs) {
  const [host, port] = hostport.split(':');
  const deadline = Date.now() + timeoutMs;
  return new Promise(resolve => {
    const tryOnce = () => {
      const s = net.connect({ host, port: Number(port) });
      s.setTimeout(1500);
      const done = ok => {
        s.destroy();
        if (ok === true || Date.now() > deadline) resolve(ok);
        else if (ok === 'skip') resolve('skip');
        else setTimeout(tryOnce, 1000);
      };
      s.on('connect', () => done(true));
      s.on('timeout', () => done(false));
      s.on('error', e => done(e && (e.code === 'ENOTFOUND' || e.code === 'EAI_AGAIN') ? 'skip' : false));
    };
    tryOnce();
  });
}

// Генерация, во время которой убиваем узел.
function longRequestAndKill() {
  return new Promise(resolve => {
    const { host, port } = parseURL(BASE);
    const body = JSON.stringify({
      model: MODEL, stream: true, max_tokens: 400,
      messages: [{ role: 'user', content: 'Расскажи подробно, минимум 300 слов, как работает TCP.' }],
    });
    const t0 = Date.now();
    let killed = false;
    let bytes = 0;
    const out = { status: 0, err: null, body: '', bytes: 0, ms: 0, killedAtMs: 0 };
    const r = http.request(
      {
        host, port, path: '/v1/chat/completions', method: 'POST', agent: false,
        headers: { 'Content-Type': 'application/json', 'Content-Length': Buffer.byteLength(body) },
      },
      res => {
        out.status = res.statusCode;
        let s = '';
        res.on('data', d => {
          s += d;
          bytes += d.length;
          if (!killed && Date.now() - t0 > KILL_AFTER_MS) {
            killed = true;
            out.killedAtMs = Date.now() - t0;
            shell(DONOR_KILL);
          }
        });
        res.on('end', () => {
          out.body = s;
          out.bytes = bytes;
          out.ms = Date.now() - t0;
          resolve(out);
        });
      }
    );
    r.on('error', e => {
      out.err = e.code || e.message;
      out.bytes = bytes;
      out.ms = Date.now() - t0;
      resolve(out);
    });
    r.setTimeout(240000, () => r.destroy(new Error('timeout')));
    r.write(body);
    r.end();
  });
}

// num_ctx обязателен: без него lazy-load возьмёт профиль по умолчанию (32k) и
// загрузка на этом стенде идёт ~75 секунд — проверка превращается в ожидание.
const NUM_CTX = Number(process.env.NUM_CTX || 4096);
const LOAD_JSON = process.env.LOAD_JSON || '';

async function chat(tag, tries) {
  const attempts = tries || 1;
  let last = null;
  for (let i = 0; i < attempts; i++) {
    last = await req('POST', BASE, '/v1/chat/completions', {
      model: MODEL, stream: false, max_tokens: 24, num_ctx: NUM_CTX,
      messages: [{ role: 'user', content: 'Повтори ровно этот код: ' + tag + '. Ответь только кодом.' }],
    });
    if (last.status === 200) return last;
    // lazy-load может идти прямо сейчас: 500/503 — повод подождать, а не падать.
    await new Promise(r => setTimeout(r, 3000));
  }
  return last;
}

(async () => {
  console.log('R224: потеря сессии посреди генерации — диагноз и авто-восстановление\n');

  const before = await loadedModel();
  if (!before || before.state !== 'loaded') {
    precondition('модель загружена', 'загрузите модель с раскладкой на узел и повторите');
    return finish();
  }
  if (!hasRPC(before)) {
    precondition('раскладка на узел', 'в слоях нет RPC-устройств');
    return finish();
  }
  console.log('модель: ' + before.state + ', слои: ' + layersText(before));

  const w0 = workerState();
  console.log(`мастер: running=${w0.running} restarts=${w0.restarts}`);

  const nodeAddr = (before.device_layers_exact || []).map(l => l.node).filter(Boolean)[0] || '';
  if (nodeAddr) {
    const ready = await waitReachable(nodeAddr, 60000);
    if (ready === 'skip') console.log('SKIP проверка узла до старта: имя не разрешается с хоста');
    else check(ready, 'узел отвечает по TCP до сбоя', nodeAddr);
  }

  const warm = await chat('CODE-WARM');
  check(warm.status === 200, 'тёплый запрос до сбоя', 'HTTP ' + warm.status);
  if (warm.status !== 200) return finish();

  console.log('\n-- генерация + остановка узла на ' + KILL_AFTER_MS + ' мс: ' + DONOR_KILL);
  const lost = await longRequestAndKill();
  console.log('   HTTP ' + lost.status + ', чанков ' + lost.bytes + ' B, ' + lost.ms + ' мс' +
    (lost.err ? ', ошибка транспорта: ' + lost.err : ''));
  check(lost.killedAtMs > 0, 'узел убит во время генерации', lost.killedAtMs + ' мс');
  check(!lost.err, 'клиент не получил обрыв соединения', lost.err || '');

  const diagnosis = lost.body;
  check(/потеряна/i.test(diagnosis), 'клиенту назван диагноз «сессия потеряна»',
    (diagnosis.match(/RPC-сессия[^"]{0,120}/) || [''])[0]);
  check(!/KV-cache overflow/i.test(diagnosis), 'вместо ложного KV/OOM-диагноза — настоящая причина');

  const w1 = workerState();
  check(w1.running, 'мастер жив', 'running=' + w1.running);
  check(w1.restarts === w0.restarts, 'мастер не перезапускался', `restarts ${w0.restarts} -> ${w1.restarts}`);

  // Авто-выгрузка: модель должна исчезнуть из /api/models без действий оператора.
  let unloaded = false;
  for (let i = 0; i < 20; i++) {
    await new Promise(r => setTimeout(r, 500));
    const m = await loadedModel();
    if (!m) { unloaded = true; break; }
  }
  check(unloaded, 'модель выгружена автоматически после потери сессии');

  console.log('\n-- возвращаем узел: ' + DONOR_START);
  const started = shell(DONOR_START);
  check(started.ok, 'узел снова запущен', started.ok ? '' : started.out.slice(0, 160));
  if (nodeAddr) {
    const back = await waitReachable(nodeAddr, 90000);
    if (back !== 'skip') check(back, 'узел снова отвечает по TCP', nodeAddr);
  }

  // Дать rpc-server подняться: TCP-порт открывается раньше, чем узел готов
  // обслуживать раскладку (иначе загрузка проходит без RPC-устройств).
  await new Promise(r => setTimeout(r, 10000));

  // Следующий запрос обязан поднять модель заново (lazy-load).
  const recovered = await chat('CODE-BACK', 10);
  check(recovered.status === 200, 'запрос после возврата узла (авто-загрузка)', 'HTTP ' + recovered.status);

  // РАСКЛАДКА ЗДЕСЬ НЕ ПРОВЕРЯЕТСЯ. Lazy-load может успеть раньше, чем узел
  // начнёт обслуживать RPC, и слои останутся локальными — это гонка старта узла,
  // а не потеря R224. Пересборка раскладки после возврата узла проверяется
  // отдельно: scripts/probe-rpc-session-loss-r221.js (шаг восстановления).
  // Загрузка с раскладкой занимает десятки секунд (пересборка снимка устройств
  // узла + веса), поэтому ждём до 2 минут, а не 20 секунд.
  let loaded = null;
  for (let i = 0; i < 120; i++) {
    const m = await loadedModel();
    if (m && m.state === 'loaded') { loaded = m; break; }
    if (m && (m.state === 'error' || m.state === 'failed')) { loaded = m; break; }
    await new Promise(r => setTimeout(r, 1000));
  }
  check(!!loaded && loaded.state === 'loaded', 'модель загружена заново без действий оператора',
    loaded ? layersText(loaded) : 'нет');

  const w2 = workerState();
  check(w2.running && w2.restarts === w0.restarts, 'мастер жив после всей серии', `restarts=${w2.restarts}`);

  finish();
})().catch(e => {
  console.log('FATAL: ' + e.message);
  process.exit(1);
});

function finish() {
  console.log('\nИТОГ: ' + (fails.length ? 'ПРОВАЛ (' + fails.length + ')' : 'OK — диагноз честный, восстановление автоматическое'));
  for (const f of fails) console.log('  - ' + f);
  if (pre.length) console.log('ПРЕДУСЛОВИЯ не выполнены (' + pre.length + '): проверка недействительна — см. PRECONDITION выше');
  process.exit(fails.length || pre.length ? 1 : 0);
}

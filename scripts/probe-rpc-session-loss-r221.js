// probe-rpc-session-loss-r221.js — R221: узел раскладки пропадает ПОСРЕДИ генерации.
//
// ВАЖНО, ЧЕМ ЭТОТ ПРОБНИК ОТЛИЧАЕТСЯ ОТ ПРОСТОГО «убить узел и спросить».
// Воркер уже умеет отсекать заведомо мёртвый узел ДО генерации (TCP-гейт,
// ответ `rpc_node_unreachable`). Такой прогон зелёный и без патча R221, поэтому
// он ничего не проверяет. Здесь узел убивается в момент, когда поток уже идёт:
// сессия RPC установлена, llama_decode работает, и потеря узла всплывает внутри
// декодирования — ровно тот случай, где мастер раньше падал по RPC_STATUS_ASSERT.
//
// Запуск (на мастере, узел управляется по ssh или локальным docker):
//   node scripts/probe-rpc-session-loss-r221.js
// Переменные окружения:
//   MODEL            имя модели без .gguf (по умолчанию gemma-4-E4B-it-Q4_K_M)
//   BASE             клиентский адрес балансера (по умолчанию http://127.0.0.1:18080)
//   WORKER           контейнер мастера (по умолчанию ol-stack-cppworker-gpu)
//   WORKER_API       адрес API мастера (по умолчанию http://127.0.0.1:18092)
//   TOKEN            X-API-Token воркера
//   DONOR_KILL       команда остановки узла (по умолчанию: ssh legion2 docker kill cppworker-node2)
//   DONOR_START      команда запуска узла (по умолчанию: ssh legion2 docker start cppworker-node2)
//   KILL_AFTER_MS    через сколько после первого чанка убивать узел (по умолчанию 2500)
//   LOAD_JSON        тело /api/models/load для шага восстановления (иначе шаг пропускается)
//
// Код возврата: 0 — все проверки прошли, 1 — провал (печатается причина).
const http = require('http');
const net = require('net');
const fs = require('fs');
const { execSync } = require('child_process');

const MODEL = (process.env.MODEL || 'gemma-4-E4B-it-Q4_K_M').replace(/\.gguf$/i, '');
const BASE = process.env.BASE || 'http://127.0.0.1:18080';
const WORKER_API = process.env.WORKER_API || 'http://127.0.0.1:18092';
const WORKER = process.env.WORKER || 'ol-stack-cppworker-gpu';
const TOKEN = process.env.TOKEN || 'changeme-bundled-with-agent-token';
const DONOR_KILL = process.env.DONOR_KILL || 'ssh legion2 docker kill cppworker-node2';
const DONOR_START = process.env.DONOR_START || 'ssh legion2 docker start cppworker-node2';
const KILL_AFTER_MS = Number(process.env.KILL_AFTER_MS || 2500);
const LOAD_JSON = process.env.LOAD_JSON || '';

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

// waitReachable — узел считается готовым, когда его порт принимает TCP.
// Фиксированный sleep здесь неверен: rpc-server после `docker start` поднимается
// секунды, и «тёплый» запрос попадал в TCP-гейт воркера (503 rpc_node_unreachable),
// то есть проба проверяла гейт, а не потерю сессии.
function waitReachable(hostport, timeoutMs) {
  const [host, port] = hostport.split(':');
  const deadline = Date.now() + timeoutMs;
  return new Promise(resolve => {
    const tryOnce = () => {
      const s = net.connect({ host, port: Number(port) });
      s.setTimeout(1500);
      const done = ok => {
        s.destroy();
        if (ok || Date.now() > deadline) resolve(ok);
        else setTimeout(tryOnce, 1000);
      };
      s.on('connect', () => done(true));
      s.on('timeout', () => done(false));
      // ENOTFOUND/EAI_AGAIN: имя живёт только внутри docker-сети мастера, с хоста
      // его не разрешить. Это не отказ узла — проверку делает сам воркер.
      s.on('error', e => done(e && (e.code === 'ENOTFOUND' || e.code === 'EAI_AGAIN') ? 'skip' : false));
    };
    tryOnce();
  });
}

function shell(cmd) {
  try {
    return { ok: true, out: execSync(cmd, { encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] }).trim() };
  } catch (e) {
    return { ok: false, out: ((e.stdout || '') + (e.stderr || '')).trim(), code: e.status };
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

// ensureWorkingSession — добивается, чтобы генерация на текущей модели прошла.
// Перезагрузка модели пересобирает раскладку и заново открывает сессию с узлом.
async function ensureWorkingSession() {
  const warmBody = {
    model: MODEL, stream: false, max_tokens: 16,
    messages: [{ role: 'user', content: 'Ответь одним словом: ок.' }],
  };
  for (let attempt = 0; attempt < 6; attempt++) {
    const r = await req('POST', BASE, '/v1/chat/completions', warmBody);
    if (r.status === 200) return true;
    console.log('   сессия не готова (HTTP ' + r.status + '), перезагружаю модель');
    if (!LOAD_JSON) return false;
    await req('POST', WORKER_API, '/api/models/unload?name=' + encodeURIComponent(MODEL), undefined, { 'X-API-Token': TOKEN });
    await req('POST', WORKER_API, '/api/models/load', JSON.parse(fs.readFileSync(LOAD_JSON, 'utf8')), { 'X-API-Token': TOKEN });
    for (let i = 0; i < 30; i++) {
      await new Promise(res => setTimeout(res, 5000));
      const m = await modelState();
      if (m && m.state !== 'loading') break;
    }
  }
  return false;
}

async function modelState() {
  const r = await req('GET', WORKER_API, '/api/models', undefined, { 'X-API-Token': TOKEN });
  if (r.status !== 200) throw new Error(`/api/models -> ${r.status} ${r.body.slice(0, 200)}`);
  const list = JSON.parse(r.body).models || [];
  return list.find(m => String(m.name || '').includes(MODEL)) || list[0] || null;
}

// Длинная генерация через балансер. Узел убиваем на первом чанке после
// KILL_AFTER_MS — то есть уже внутри декодирования.
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
    const out = { status: 0, err: null, bytes: 0, ms: 0, tail: '', killedAtMs: 0 };
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
            const k = shell(DONOR_KILL);
            if (!k.ok) console.log('   не удалось остановить узел: ' + k.out.slice(0, 160));
          }
        });
        res.on('end', () => {
          out.bytes = bytes;
          out.ms = Date.now() - t0;
          out.tail = s.slice(-600);
          out.body = s;
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

(async () => {
  console.log('R221: потеря узла раскладки ПОСРЕДИ генерации\n');

  const before = await modelState();
  console.log('модель: ' + (before ? before.state + ', слои: ' + layersText(before) : 'нет'));
  if (!before || before.state !== 'loaded') {
    precondition('модель загружена', 'загрузите модель с раскладкой на узел и повторите');
    return finish();
  }
  if (!hasRPC(before)) {
    precondition('раскладка на узел', 'в слоях нет RPC-устройств: включите раскладку и перезагрузите модель');
    return finish();
  }

  const w0 = workerState();
  const logsBefore = shell(`docker logs --since 15m ${WORKER} 2>&1`);
  const logHitsBefore = logsBefore.ok ? (logsBefore.out.match(/RPC session lost/g) || []).length : 0;
  console.log(`мастер: running=${w0.running} restarts=${w0.restarts}`);

  // Узел обязан быть готов ДО первого запроса: иначе проверяется TCP-гейт воркера,
  // а не потеря сессии посреди генерации.
  const nodeAddr = (before.device_layers_exact || []).map(l => l.node).filter(Boolean)[0];
  if (nodeAddr) {
    const ready = await waitReachable(nodeAddr, 90000);
    if (ready === 'skip') console.log('SKIP узел раскладки ' + nodeAddr + ': имя не разрешается с хоста (адрес docker-сети)');
    else check(ready, 'узел раскладки отвечает по TCP до запроса', nodeAddr);
  }

  // Живая сессия ДО эксперимента. После возврата узла мастер может ещё держать
  // потерянную сессию (генерация вернёт 503 rpc_node_unreachable) — перезагружаем
  // модель, иначе убийство узла «посреди генерации» не случится вовсе.
  const warm = await ensureWorkingSession();
  check(warm, 'тёплый запрос до потери узла (сессия живая)', warm ? 'HTTP 200' : 'сессия не поднялась');
  if (!warm) return finish();

  console.log('\n-- генерация + остановка узла на ' + KILL_AFTER_MS + ' мс: ' + DONOR_KILL);
  const lost = await longRequestAndKill();
  try { fs.writeFileSync('_diag/probe-rpc-loss-body.txt', lost.body || ''); } catch (e) {}
  console.log('   HTTP ' + lost.status + ', чанков ' + lost.bytes + ' B, ' + lost.ms + ' мс' +
    (lost.killedAtMs ? ', узел убит на ' + lost.killedAtMs + ' мс' : ', узел НЕ убит') +
    (lost.err ? ', ошибка транспорта: ' + lost.err : ''));

  check(lost.killedAtMs > 0, 'узел убит во время генерации (а не до неё)', lost.killedAtMs + ' мс');
  check(!lost.err, 'клиент не получил обрыв соединения (socket hang up)', lost.err || '');
  check(lost.bytes > 0, 'поток начался до потери узла (сессия была живой)', lost.bytes + ' B');
  const explicit = lost.status >= 400 || /"error"/.test(lost.body) || /"finish_reason"\s*:\s*"error"/.test(lost.body);
  check(explicit, 'клиент получил явную ошибку, а не молчаливый обрыв', 'HTTP ' + lost.status);

  const w1 = workerState();
  check(w1.running, 'мастер продолжает работать', 'running=' + w1.running);
  check(w1.restarts === w0.restarts, 'мастер не перезапускался (нет SIGABRT)', `restarts ${w0.restarts} -> ${w1.restarts}`);

  const logs = shell(`docker logs --since 15m ${WORKER} 2>&1`);
  if (logs.ok) {
    const hits = (logs.out.match(/RPC session lost/g) || []).length;
    console.log('   в логах мастера "RPC session lost": ' + hits + ' (было ' + logHitsBefore + ')');
    check(hits > logHitsBefore, 'мастер зафиксировал потерю сессии именно в этом прогоне', 'hits ' + logHitsBefore + ' -> ' + hits);
  }

  console.log('\n-- возвращаем узел: ' + DONOR_START);
  const started = shell(DONOR_START);
  check(started.ok, 'узел снова запущен', started.ok ? '' : started.out.slice(0, 160));
  if (nodeAddr) {
    const back = await waitReachable(nodeAddr, 90000);
    if (back === 'skip') console.log('SKIP проверка узла ' + nodeAddr + ': имя не разрешается с хоста');
    else check(back, 'узел снова отвечает по TCP', nodeAddr);
  }

  if (!LOAD_JSON) {
    precondition('восстановление раскладки', 'LOAD_JSON не задан — шаг перезагрузки пропущен');
    return finish();
  }

  const unload = await req('POST', WORKER_API, '/api/models/unload?name=' + encodeURIComponent(MODEL), undefined, { 'X-API-Token': TOKEN });
  // 404 — модель уже выгружена воркером после сбоя, это не провал.
  check(unload.status === 200 || unload.status === 404, 'выгрузка потерявшей сессию модели', 'HTTP ' + unload.status);

  const loadBody = JSON.parse(fs.readFileSync(LOAD_JSON, 'utf8'));
  const load = await req('POST', WORKER_API, '/api/models/load', loadBody, { 'X-API-Token': TOKEN });
  // 200 — загружено сразу, 202 — загрузка принята и идёт в фоне (обычный ответ воркера).
  check(load.status === 200 || load.status === 202, 'повторная загрузка после возврата узла', 'HTTP ' + load.status);

  let loaded = null;
  for (let i = 0; i < 40; i++) {
    await new Promise(r => setTimeout(r, 5000));
    const m = await modelState();
    if (m && m.state !== 'loading') {
      loaded = m;
      break;
    }
  }
  check(!!loaded && loaded.state === 'loaded', 'модель загружена заново', loaded ? layersText(loaded) : 'нет состояния');
  if (loaded) check(hasRPC(loaded), 'раскладка снова на узел', layersText(loaded));

  const recovered = await req('POST', BASE, '/v1/chat/completions', {
    model: MODEL, stream: false, max_tokens: 16, messages: [{ role: 'user', content: 'Ответь одним словом: ок.' }],
  });
  check(recovered.status === 200, 'генерация после восстановления', 'HTTP ' + recovered.status);

  const w2 = workerState();
  check(w2.running && w2.restarts === w0.restarts, 'мастер жив после всей серии', `restarts=${w2.restarts}`);

  finish();
})().catch(e => {
  console.log('FATAL: ' + e.message);
  process.exit(1);
});

function finish() {
  console.log('\nИТОГ: ' + (fails.length ? 'ПРОВАЛ (' + fails.length + ')' : 'OK — мастер пережил потерю узла посреди генерации'));
  for (const f of fails) console.log('  - ' + f);
  if (pre.length) console.log('ПРЕДУСЛОВИЯ не выполнены (' + pre.length + '): проверка недействительна — см. PRECONDITION выше');
  process.exit(fails.length || pre.length ? 1 : 0);
}

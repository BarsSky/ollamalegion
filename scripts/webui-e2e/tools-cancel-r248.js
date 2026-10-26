// scripts/webui-e2e/tools-cancel-r248.js — R248, часть A: отмена upstream при
// разрыве клиента в буферизированном tools-пути.
//
// ЖИВОЙ ДЕФЕКТ (R247): клиент ушёл во время буферизированной генерации
// (`/v1/chat/completions`, stream + tools). Балансер отменял свой запрос, но
// cppworker продолжал генерацию: `active_queries=1`, следующий клиент ждал до
// рестарта воркера.
//
// Что проверяет пробник:
//   1. слот освобождается в пределах DOF_SEC после разрыва соединения;
//   2. следующий запрос обслуживается, не дожидаясь «мёртвой» генерации.
//
// Запуск:
//   node scripts/webui-e2e/tools-cancel-r248.js
//   BASE=http://127.0.0.1:18080 WORKER=http://127.0.0.1:18092 DOF_SEC=5 node ...
'use strict';
const http = require('http');

function env(name, def) { return process.env[name] || def; }
const BASE = new URL(env('BASE', 'http://127.0.0.1:18080'));
const WORKER = new URL(env('WORKER', 'http://127.0.0.1:18092'));
const TOKEN = env('API_TOKEN', 'changeme-bundled-with-agent-token');
const MODEL = env('MODEL', 'gemma-4-E4B-it-Q4_K_M');
const DOF_SEC = parseInt(env('DOF_SEC', '5'), 10);
const NEXT_LIMIT_SEC = parseInt(env('NEXT_LIMIT_SEC', '30'), 10);
const KILL_AFTER_MS = parseInt(env('KILL_AFTER_MS', '2500'), 10);

const READ = {
  type: 'function',
  function: { name: 'read', description: 'Read a file.', parameters: { type: 'object', properties: { file_path: { type: 'string' } }, required: ['file_path'] } },
};

function json(target, method, path, body, timeoutMs) {
  return new Promise((resolve) => {
    const payload = body === undefined ? null : JSON.stringify(body);
    const headers = { 'X-API-Token': TOKEN };
    if (payload) { headers['Content-Type'] = 'application/json'; headers['Content-Length'] = Buffer.byteLength(payload); }
    const req = http.request({ host: target.hostname, port: target.port, path, method, headers }, (res) => {
      let data = '';
      res.on('data', (c) => { data += c; });
      res.on('end', () => resolve({ status: res.statusCode, body: data }));
    });
    req.on('error', (e) => resolve({ status: 0, body: '', error: e.message }));
    if (timeoutMs) req.setTimeout(timeoutMs, () => req.destroy(new Error('timeout')));
    if (payload) req.write(payload);
    req.end();
  });
}

// startLongToolsRequest — открыть потоковый tools-запрос и бросить его.
function startLongToolsRequest() {
  const payload = JSON.stringify({
    model: MODEL, stream: true, temperature: 0.2, max_tokens: 1500,
    messages: [{ role: 'user', content: 'Write a long detailed essay about the history of computing. No tool calls, just prose.' }],
    tools: [READ],
  });
  const req = http.request({
    host: BASE.hostname, port: BASE.port, path: '/v1/chat/completions', method: 'POST',
    headers: { 'Content-Type': 'application/json', 'X-API-Token': TOKEN, 'Content-Length': Buffer.byteLength(payload) },
  });
  const state = { bytes: 0, closed: false, destroyedAt: 0, error: null };
  req.on('response', (res) => {
    res.on('data', (c) => { state.bytes += c.length; });
    res.on('error', () => {});
  });
  req.on('error', (e) => { state.error = e.message; });
  req.write(payload);
  req.end();
  return {
    state,
    kill() {
      state.closed = true;
      state.destroyedAt = Date.now();
      req.destroy(new Error('probe: client disconnect'));
    },
  };
}

async function activeQueries() {
  const r = await json(WORKER, 'GET', '/api/models', undefined, 5000);
  if (r.status !== 200) return -1;
  try { return Number(JSON.parse(r.body).models[0].active_queries); } catch (e) { return -1; }
}

function sleep(ms) { return new Promise((r) => setTimeout(r, ms)); }

(async () => {
  const problems = [];
  console.log('=== R248/A: отмена upstream при разрыве клиента в tools-пути ===');
  console.log('модель ' + MODEL + ', база ' + BASE.origin + ', воркер ' + WORKER.origin);

  const before = await activeQueries();
  console.log('active_queries до запроса: ' + before);

  const live = startLongToolsRequest();
  // Ждём, пока генерация реально начнётся: буферизированный tools-путь байтов
  // не отдаёт, поэтому ориентир — active_queries=1 на воркере.
  const startedAt = Date.now();
  while (Date.now() - startedAt < 15000) {
    if (await activeQueries() === 1) break;
    await sleep(200);
  }
  const startedIn = Date.now() - startedAt;
  console.log('поток начался через ' + startedIn + ' мс, байт ' + live.state.bytes);
  await sleep(KILL_AFTER_MS);
  live.kill();
  console.log('клиент разорвал соединение (было байт ' + live.state.bytes + ')');
  const killAt = Date.now();

  // 1. Ждём освобождения слота.
  let freedIn = -1;
  while (Date.now() - killAt < (DOF_SEC + 20) * 1000) {
    const n = await activeQueries();
    if (n === 0) { freedIn = Date.now() - killAt; break; }
    await sleep(300);
  }
  if (freedIn < 0) {
    problems.push('слот не освободился за ' + (DOF_SEC + 20) + ' с');
    console.log('FAIL  слот не освободился за ' + (DOF_SEC + 20) + ' с');
  } else {
    console.log('слот освобождён через ' + freedIn + ' мс после разрыва');
    if (freedIn > DOF_SEC * 1000) problems.push('слот освобождался ' + freedIn + ' мс (порог ' + DOF_SEC + ' с)');
  }

  // 2. Следующий запрос не должен ждать «мёртвую» генерацию.
  const t0 = Date.now();
  const next = await json(BASE, 'POST', '/v1/chat/completions', {
    model: MODEL, stream: false, max_tokens: 8, temperature: 0,
    messages: [{ role: 'user', content: 'Say OK.' }],
  }, (NEXT_LIMIT_SEC + 10) * 1000);
  const nextMs = Date.now() - t0;
  console.log('следующий запрос: HTTP ' + next.status + ' за ' + nextMs + ' мс');
  if (next.status !== 200) problems.push('следующий запрос HTTP ' + next.status);
  if (nextMs > NEXT_LIMIT_SEC * 1000) problems.push('следующий запрос ждал ' + nextMs + ' мс (порог ' + NEXT_LIMIT_SEC + ' с)');

  const after = await activeQueries();
  console.log('active_queries после: ' + after);
  if (after !== 0) problems.push('active_queries после = ' + after + ' (ожидался 0)');

  console.log('\nИТОГ: ' + (problems.length === 0 ? 'PASS' : 'FAIL ') + (problems.length ? problems.join(' | ') : 'слот свободен, следующий запрос обслужен'));
  process.exit(problems.length ? 1 : 0);
})();


// probe-concurrent-resets-r219.js — R219: почему под параллельной нагрузкой
// один из запросов получает ECONNRESET (жалоба «ответ не пришёл»).
//
// Запуск: node scripts/probe-concurrent-resets-r219.js [rounds] [parallel]
// Печатает по каждому запросу: код, длительность, ошибку; в конце статистику.
const http = require('http');

const ROUNDS = Number(process.argv[2] || 4);
const PARALLEL = Number(process.argv[3] || 3);
const MODEL = (process.env.MODEL || 'gemma-4-E4B-it-Q4_K_M.gguf').replace(/\.gguf$/i, '');
const PORT = 18080;

function one(i) {
  return new Promise(resolve => {
    const t0 = Date.now();
    const body = JSON.stringify({
      model: MODEL, stream: false, max_tokens: 64,
      messages: [{ role: 'user', content: 'Повтори ровно этот код: CODE-' + i + '. Ответь только кодом.' }],
    });
    const req = http.request({
      host: '127.0.0.1', port: PORT, path: '/v1/chat/completions', method: 'POST',
      headers: { 'Content-Type': 'application/json', 'Content-Length': Buffer.byteLength(body) },
      agent: false, // новое соединение на каждый запрос: исключаем переиспользование сокетов
    }, res => {
      let s = '';
      res.on('data', d => (s += d));
      res.on('end', () => resolve({ i, status: res.statusCode, ms: Date.now() - t0, len: s.length, err: null }));
    });
    req.on('error', e => resolve({ i, status: 0, ms: Date.now() - t0, len: 0, err: e.code || e.message }));
    req.setTimeout(300000, () => req.destroy(new Error('timeout')));
    req.write(body);
    req.end();
  });
}

(async () => {
  const results = [];
  for (let r = 0; r < ROUNDS; r++) {
    const batch = await Promise.all(Array.from({ length: PARALLEL }, (_, i) => one(r * 100 + i)));
    results.push(...batch);
    const line = batch.map(b => b.status + (b.err ? ':' + b.err : '') + '/' + b.ms + 'ms').join('  ');
    console.log('round ' + r + ': ' + line);
    await new Promise(res => setTimeout(res, 300));
  }
  const bad = results.filter(r => r.status !== 200);
  console.log('\nИТОГ: ' + results.length + ' запросов, ошибок ' + bad.length);
  if (bad.length) {
    const byErr = {};
    for (const b of bad) byErr[b.err || ('HTTP ' + b.status)] = (byErr[b.err || ('HTTP ' + b.status)] || 0) + 1;
    console.log('ошибки:', JSON.stringify(byErr));
  }
})().catch(e => { console.log('FATAL', e.message); process.exit(1); });

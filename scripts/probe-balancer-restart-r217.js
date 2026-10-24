// probe-balancer-restart-r217.js — R217: почему сразу после рестарта балансера
// клиент получает socket hang up.
//
// Скрипт бьёт по /v1/models каждые 400 мс и печатает каждую неудачу с меткой
// времени, статусом и текстом ошибки. Запускать ДО рестарта контейнера, рестарт
// делать отдельной командой; скрипт сам работает N секунд.
//
//   node scripts/probe-balancer-restart-r217.js 90
const http = require('http');

const SECONDS = Number(process.argv[2] || 60);
const PATH = process.env.PROBE_PATH || '/v1/models';
const start = Date.now();
const results = [];

function once() {
  return new Promise(resolve => {
    const t0 = Date.now();
    const req = http.request({
      host: '127.0.0.1', port: 18080, path: PATH, method: 'GET', timeout: 5000,
    }, res => {
      let s = '';
      res.on('data', d => (s += d));
      res.on('end', () => resolve({
        ms: Date.now() - t0, status: res.statusCode, t: Date.now() - start, body: s.slice(0, 60),
      }));
    });
    req.on('timeout', () => { req.destroy(new Error('timeout')); });
    req.on('error', e => resolve({ ms: Date.now() - t0, error: e.code || e.message, t: Date.now() - start }));
    req.end();
  });
}

(async () => {
  const deadline = Date.now() + SECONDS * 1000;
  while (Date.now() < deadline) {
    const r = await once();
    results.push(r);
    if (r.error || (r.status && r.status >= 500)) {
      console.log('FAIL t=' + r.t + 'ms ' + (r.error || ('HTTP ' + r.status)) + ' (' + r.ms + 'ms)');
    }
    await new Promise(res => setTimeout(res, 400));
  }
  const ok = results.filter(r => r.status === 200).length;
  const errs = results.filter(r => r.error || (r.status && r.status >= 500));
  console.log('ИТОГ: запросов=' + results.length + ' 200=' + ok + ' ошибок=' + errs.length);
  if (errs.length) {
    console.log('первая ошибка на t=' + errs[0].t + 'мс, последняя на t=' + errs[errs.length - 1].t + 'мс');
  }
})().catch(e => { console.log('FATAL', e.message); process.exit(1); });

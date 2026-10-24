// probe-stream-drain-r217.js — R217: переживает ли активный SSE-стрим рестарт
// балансера (graceful shutdown / drain in-flight).
//
// Запуск: node scripts/probe-stream-drain-r217.js
// Скрипт открывает /v1/chat/completions?stream=true и печатает время каждого
// чанка. Рестарт балансера нужно сделать ВРУЧНУЮ через 3-4 секунды после старта
// (см. команду в отчёте). Итог: завершился ли поток, пришёл ли [DONE], сколько
// чанков пришло ПОСЛЕ рестарта.
const http = require('http');

const MODEL = process.env.MODEL || 'gemma-4-E4B-it-Q4_K_M';
const MAX_TOKENS = Number(process.env.MAX_TOKENS || 400);
const PROMPT = process.env.PROBE_PROMPT || 'Перечисли числа от 1 до 60 словами, каждое с новой строки.';
// RESTART_AFTER_MS — скрипт сам рестартует балансер через N мс после старта.
// Так делается детерминированная проверка: активный стрим + рестарт. Если 0 —
// рестарт не делаем (проверяем только стрим).
const RESTART_AFTER_MS = Number(process.env.RESTART_AFTER_MS || 0);

const { execFileSync } = require('child_process');
const path = require('path');

const start = Date.now();
let chunks = 0, content = '', lastT = 0, done = false, error = null;
let restartAtMs = null;

const body = JSON.stringify({
  model: MODEL,
  stream: true,
  max_tokens: MAX_TOKENS,
  messages: [{ role: 'user', content: PROMPT }],
});

const req = http.request({
  host: '127.0.0.1', port: 18080, path: '/v1/chat/completions', method: 'POST',
  headers: { 'Content-Type': 'application/json', 'Content-Length': Buffer.byteLength(body) },
}, res => {
  let buf = '';
  res.on('data', d => {
    buf += d.toString();
    let idx;
    while ((idx = buf.indexOf('\n')) >= 0) {
      const line = buf.slice(0, idx).trim();
      buf = buf.slice(idx + 1);
      if (!line.startsWith('data: ')) continue;
      const payload = line.slice(6);
      if (payload === '[DONE]') { done = true; continue; }
      try {
        const j = JSON.parse(payload);
        const d2 = ((j.choices || [])[0] || {}).delta || {};
        if (d2.content) { content += d2.content; chunks++; lastT = Date.now() - start; }
      } catch (e) {}
    }
  });
  res.on('end', () => {
    console.log(JSON.stringify({
      done, chunks, contentLen: content.length, lastChunkAtMs: lastT, restartAtMs,
      totalMs: Date.now() - start, error,
      contentTail: content.slice(-80).replace(/\s+/g, ' '),
    }, null, 1));
  });
  res.on('error', e => { error = e.message; });
});
req.on('error', e => { error = e.code || e.message; console.log('REQ ERROR', error, 'at', Date.now() - start, 'ms'); });

if (RESTART_AFTER_MS > 0) {
  setTimeout(() => {
    restartAtMs = Date.now() - start;
    console.log('RESTART loadbalancer at ' + restartAtMs + 'ms');
    try {
      execFileSync('docker', ['compose', '-f', 'docker-compose.stack.yml', '--profile', 'full', 'restart', 'loadbalancer'], {
        cwd: path.join(__dirname, '..', 'deployments'), stdio: 'ignore',
      });
      console.log('RESTART done at ' + (Date.now() - start) + 'ms');
    } catch (e) {
      console.log('RESTART failed: ' + e.message);
    }
  }, RESTART_AFTER_MS);
}
req.setTimeout(600000, () => { req.destroy(new Error('timeout')); });
req.write(body);
req.end();

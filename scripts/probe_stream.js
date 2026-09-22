#!/usr/bin/env node
/*
 * probe_stream.js — стриминговый зонд NDJSON/SSE: печатает каждую строку с
 * отметкой времени и интервалом от предыдущей строки.
 *
 * Запуск: node scripts/probe_stream.js <url> <bodyFile> [timeoutSec]
 */

const http = require('http');
const fs = require('fs');

const url = process.argv[2] || 'http://127.0.0.1:18080/api/chat';
const bodyFile = process.argv[3];
const timeoutSec = parseInt(process.argv[4] || '300', 10);
const body = fs.readFileSync(bodyFile);
const u = new URL(url);

const started = Date.now();
let last = started;
let buf = '';
let n = 0;

const req = http.request(
  {
    hostname: u.hostname,
    port: u.port,
    path: u.pathname + u.search,
    method: 'POST',
    headers: { 'Content-Type': 'application/json', 'Content-Length': body.length },
  },
  (res) => {
    console.log(`[${((Date.now() - started) / 1000).toFixed(2)}s] HTTP ${res.statusCode} ct=${res.headers['content-type']}`);
    res.setEncoding('utf8');
    res.on('data', (chunk) => {
      buf += chunk;
      let idx;
      while ((idx = buf.indexOf('\n')) >= 0) {
        const line = buf.slice(0, idx);
        buf = buf.slice(idx + 1);
        if (!line.trim()) continue;
        n++;
        const now = Date.now();
        const dt = ((now - last) / 1000).toFixed(2);
        last = now;
        const short = line.length > 220 ? line.slice(0, 220) + `... (len=${line.length})` : line;
        console.log(`[${((now - started) / 1000).toFixed(2)}s +${dt}s] LINE ${n}: ${short}`);
      }
    });
    res.on('end', () => {
      console.log(`[${((Date.now() - started) / 1000).toFixed(2)}s] END: ${n} lines, total ${((Date.now() - started) / 1000).toFixed(1)}s`);
    });
  }
);

req.setTimeout(timeoutSec * 1000, () => req.destroy(new Error('timeout')));
req.on('error', (e) => console.error('ERR ' + e.message));
req.end(body);

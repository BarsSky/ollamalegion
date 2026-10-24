#!/usr/bin/env node
// scripts/verify-stream-r231.js — R231 (2026-10-24): живая проверка стрима
// OpenAI-поверхности глазами Cline.
//
// ЧТО ЛОВИТ (все три дефекта воспроизводились живьём):
//  1. ДУБЛЬ: размышления уходят отдельным каналом, а затем ТОТ ЖЕ текст
//     приходит ещё раз в content (R91-фолбэк в стриме). Cline видит «ответ»,
//     целиком повторяющий размышления.
//  2. МАРКЕРЫ: `</think>`, `<|channel>thought`, `<end_of_turn>` и подобные
//     служебные токены попадают в видимый content.
//  3. КОНЕЦ СТРИМА: `[DONE]` отсутствует (поток завершается молча — Cline ждёт
//     и рвёт сессию), приходит дважды или не последним событием.
//
// Также проверяется честный finish_reason: если модель не успела выйти из
// размышлений (content пуст), причина должна быть length, а не stop.
//
// Запуск:
//   node scripts/verify-stream-r231.js
//   BASE=http://127.0.0.1:18080 MODEL=gemma-4-E4B-it-Q4_K_M RUNS=3 node scripts/verify-stream-r231.js
//
// Код возврата: 0 — PASS, 1 — FAIL.

const http = require('http');

const BASE = process.env.BASE || 'http://127.0.0.1:18080';
const MODEL = process.env.MODEL || 'gemma-4-E4B-it-Q4_K_M';
const RUNS = parseInt(process.env.RUNS || '3', 10);
const MAX_TOKENS = parseInt(process.env.MAX_TOKENS || '120', 10);
const TOKEN = process.env.CPPWORKER_API_TOKEN || process.env.TOKEN || '';

const MARKERS = ['</think>', '<think>', '</thinking>', '<thinking>', '<|channel>', '<channel|>',
  '<|im_start|>', '<|im_end|>', '<end_of_turn>', '<|eot_id|>', '</reasoning>', '<reasoning>'];

let fails = 0;
function check(ok, what, detail) {
  console.log((ok ? 'PASS ' : 'FAIL ') + what + (detail ? ' :: ' + detail : ''));
  if (!ok) fails++;
}

function streamOnce(i) {
  return new Promise((resolve) => {
    const body = JSON.stringify({
      model: MODEL, stream: true, temperature: 0, max_tokens: MAX_TOKENS,
      messages: [
        { role: 'system', content: 'You are Cline, a coding agent. Answer briefly in Russian.' },
        { role: 'user', content: `Задача ${i}: напиши функцию на Go, которая складывает два числа. Только код, без объяснений.` },
      ],
    });
    const url = new URL('/v1/chat/completions', BASE);
    const headers = { 'Content-Type': 'application/json', 'Content-Length': Buffer.byteLength(body) };
    if (TOKEN) headers['X-API-Token'] = TOKEN;
    const t0 = Date.now();
    const req = http.request(url, { method: 'POST', headers }, (res) => {
      let raw = '';
      res.setEncoding('utf8');
      res.on('data', (d) => (raw += d));
      res.on('end', () => {
        const events = raw.split('\n\n').filter((b) => b.trim() !== '');
        let content = '', reasoning = '', finish = '';
        let doneCount = 0, doneIndex = -1, badAfterDone = 0;
        const payloads = [];
        for (const ev of events) {
          for (const line of ev.split('\n')) {
            if (!line.startsWith('data:')) continue;
            const payload = line.slice(5).trim();
            if (payload === '[DONE]') {
              payloads.push('[DONE]');
              doneCount++; doneIndex = payloads.length - 1;
              continue;
            }
            payloads.push(payload);
            if (doneCount > 0) badAfterDone++;
            try {
              const j = JSON.parse(payload);
              const ch = j.choices && j.choices[0];
              if (!ch) continue;
              if (ch.finish_reason) finish = ch.finish_reason;
              const d = ch.delta || {};
              if (typeof d.content === 'string') content += d.content;
              if (typeof d.reasoning_content === 'string') reasoning += d.reasoning_content;
            } catch (e) { /* keepalive/comment */ }
          }
        }
        resolve({
          i, status: res.statusCode, ms: Date.now() - t0,
          content, reasoning, finish, doneCount, doneIndex, badAfterDone,
          totalEvents: payloads.length,
        });
      });
    });
    req.on('error', (e) => resolve({ i, error: String(e) }));
    req.write(body);
    req.end();
  });
}

const norm = (s) => (s || '').replace(/\s+/g, ' ').trim();

(async () => {
  console.log(`R231: живая проверка стрима (${BASE}, модель ${MODEL}, прогонов ${RUNS})\n`);
  for (let i = 1; i <= RUNS; i++) {
    const r = await streamOnce(i);
    console.log(`--- прогон ${i}: HTTP ${r.status} за ${r.ms}мс, событий ${r.totalEvents}, content=${(r.content || '').length} симв., reasoning=${(r.reasoning || '').length} симв., finish=${r.finish || '-'}`);
    if (r.error) { check(false, `${i}. запрос`, r.error); continue; }
    check(r.status === 200, `${i}. HTTP 200`, 'HTTP ' + r.status);

    const leaked = MARKERS.filter((m) => r.content.includes(m));
    check(leaked.length === 0, `${i}а. в content нет маркеров размышлений/конца хода`,
      leaked.length ? 'утекло: ' + leaked.join(',') + ' :: ' + JSON.stringify(r.content.slice(0, 160)) : 'ok');

    let dup = false;
    if (r.reasoning.length > 40 && r.content.length > 40) {
      const head = norm(r.reasoning).slice(0, 60);
      dup = norm(r.content).startsWith(norm(r.reasoning).slice(0, 40)) || norm(r.content).includes(head);
    }
    check(!dup, `${i}б. content не дублирует reasoning_content`,
      dup ? 'content повторяет размышления: ' + JSON.stringify(r.content.slice(0, 120)) : 'ok');

    check(r.doneCount === 1, `${i}в. [DONE] ровно один`, 'найдено ' + r.doneCount);
    check(r.doneIndex === r.totalEvents - 1 && r.doneIndex >= 0,
      `${i}г. [DONE] — последнее событие`, `index ${r.doneIndex} из ${r.totalEvents}`);
    check(r.badAfterDone === 0, `${i}д. после [DONE] данных нет`, 'после [DONE] событий: ' + r.badAfterDone);

    if (norm(r.content) === '' && norm(r.reasoning) !== '') {
      check(r.finish === 'length', `${i}е. пустой content из-за незакрытых размышлений → finish_reason=length`,
        'finish_reason=' + (r.finish || 'нет'));
    } else {
      check(r.finish !== '', `${i}е. finish_reason пришёл`, 'finish_reason=' + (r.finish || 'нет'));
    }
  }
  console.log('');
  if (fails === 0) {
    console.log('ИТОГ: PASS');
    process.exit(0);
  }
  console.log(`ИТОГ: FAIL (${fails})`);
  process.exit(1);
})();

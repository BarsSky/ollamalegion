// scripts/webui-e2e/probe-cline-stream.js - R243 (2026-10-26).
//
// ЖИВАЯ ПРОВЕРКА SSE-ПОТОКА ДЛЯ CLINE (OpenAI-совместимый /v1/chat/completions).
//
// Что ловим (жалобы оператора):
//   1. в delta.content приходят служебные маркеры размышлений (<think>, </think>,
//      <|channel>thought и т.п.) - клиент показывает их как текст ответа;
//   2. клиент получает терминатор потока ([DONE] / finish_reason) РАНЬШЕ, чем
//      модель закончила: ответ оборван, а транспорт говорит "всё";
//   3. мусор после терминатора: контент после [DONE], второй [DONE], второй
//      finish_reason.
//
// Скрипт ничего не меняет: делает один streaming-запрос и печатает разбор потока
// по событиям. Код возврата 1, если найдены утечки/нарушения порядка.
//
// Запуск:
//   BALANCER=http://127.0.0.1:18080 API_TOKEN=... node scripts/webui-e2e/probe-cline-stream.js
//   MODEL=gemma-4-E4B-it-Q4_K_M PROMPT="реши 17*23 по шагам" MAX_TOKENS=400 node ...
//   LONG=4000 node ...   # длинный промпт (проверка preflight/обрыва на длинной сессии)
//
// Переменные: BALANCER, API_TOKEN, MODEL, PROMPT, MAX_TOKENS, LONG, TIMEOUT_MS.

'use strict';

const BALANCER = (process.env.BALANCER || 'http://127.0.0.1:18080').replace(/\/+$/, '');
const API_TOKEN = process.env.API_TOKEN || process.env.LB_API_TOKEN || 'changeme-bundled-with-agent-token';
const MODEL = process.env.MODEL || 'gemma-4-E4B-it-Q4_K_M';
const MAX_TOKENS = Number(process.env.MAX_TOKENS || 400);
const TIMEOUT_MS = Number(process.env.TIMEOUT_MS || 180000);
const LONG = Number(process.env.LONG || 0);

let PROMPT = process.env.PROMPT ||
  'Solve 17*23 step by step. Think first, then answer with the final number only.';

if (LONG > 0) {
  // Длинный контекст: повторяем строку, чтобы упереться в окно модели и задеть
  // preflight/пересчёт n_ctx - именно там поток может закрыться раньше ответа.
  const block = 'The quick brown fox jumps over the lazy dog. 1234567890. ';
  PROMPT = (block.repeat(Math.ceil((LONG * 4) / block.length))).slice(0, LONG * 4) +
    '\n\nAnswer in one short sentence: what animal is mentioned?';
}

// Маркеры, которые не должны попадать в видимый content.
const LEAK_RE = /<\/?(?:think|thinking|reasoning|analysis|channel)\b[^>]{0,24}>|<\|?(?:think|channel)[^>]{0,24}>/i;

function now() { return Date.now(); }

(async () => {
  const started = now();
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), TIMEOUT_MS);

  const body = {
    model: MODEL,
    messages: [{ role: 'user', content: PROMPT }],
    stream: true,
    max_tokens: MAX_TOKENS,
    temperature: 0,
  };

  const events = [];
  let content = '';
  let reasoning = '';
  const leaks = [];
  let finishReason = null;
  let doneCount = 0;
  let contentAfterDone = 0;
  let sseBadJson = 0;
  let sseOther = 0;
  let firstContentAt = null;
  let lastContentAt = null;

  let res;
  try {
    res = await fetch(BALANCER + '/v1/chat/completions', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', 'X-API-Token': API_TOKEN },
      body: JSON.stringify(body),
      signal: controller.signal,
    });
  } catch (e) {
    console.error('НЕ УДАЛОСЬ ПОДКЛЮЧИТЬСЯ: ' + (e && e.message));
    process.exit(2);
  }

  console.log('POST ' + BALANCER + '/v1/chat/completions stream=true');
  console.log('  model=' + MODEL + ' max_tokens=' + MAX_TOKENS +
    (LONG ? ' long_prompt_chars=' + PROMPT.length : ''));
  console.log('  HTTP ' + res.status + ' content-type=' + (res.headers.get('content-type') || ''));

  if (!res.ok) {
    const text = await res.text().catch(() => '');
    console.error('  ответ не 200: ' + text.slice(0, 400));
    process.exit(2);
  }

  const reader = res.body.getReader();
  const decoder = new TextDecoder('utf-8');
  let buffer = '';

  const handlePayload = (payload) => {
    const t = now() - started;
    if (payload === '[DONE]') {
      doneCount++;
      events.push({ t, kind: 'done' });
      return;
    }
    let json;
    try { json = JSON.parse(payload); } catch (e) { sseBadJson++; events.push({ t, kind: 'bad-json', payload }); return; }
    const choice = (json.choices && json.choices[0]) || {};
    const delta = choice.delta || {};
    const piece = typeof delta.content === 'string' ? delta.content : '';
    const rpiece = typeof delta.reasoning_content === 'string' ? delta.reasoning_content
      : (typeof delta.reasoning === 'string' ? delta.reasoning : '');
    if (rpiece) {
      reasoning += rpiece;
      events.push({ t, kind: 'reasoning', len: rpiece.length });
    }
    if (piece) {
      if (!firstContentAt) { firstContentAt = t; }
      lastContentAt = t;
      if (doneCount > 0) { contentAfterDone += piece.length; }
      content += piece;
      if (LEAK_RE.test(piece)) {
        leaks.push({ t, piece: piece.slice(0, 160), index: content.length });
      }
      events.push({ t, kind: 'content', len: piece.length });
    }
    if (choice.finish_reason) {
      finishReason = finishReason || choice.finish_reason;
      events.push({ t, kind: 'finish', reason: choice.finish_reason });
    }
    if (json.usage) { events.push({ t, kind: 'usage', usage: json.usage }); }
    if (!piece && !rpiece && !choice.finish_reason && !json.usage) {
      sseOther++;
    }
  };

  while (true) {
    const { value, done } = await reader.read();
    if (done) { break; }
    buffer += decoder.decode(value, { stream: true });
    // SSE-события разделены пустой строкой; обрабатываем и \r\n, и \n.
    let idx;
    while ((idx = buffer.indexOf('\n\n')) >= 0) {
      const rawEvent = buffer.slice(0, idx);
      buffer = buffer.slice(idx + 2);
      rawEvent.split('\n').forEach((line) => {
        const trimmed = line.replace(/\r$/, '').trim();
        if (!trimmed || trimmed.startsWith(':')) { return; }
        if (!trimmed.startsWith('data:')) { return; }
        handlePayload(trimmed.slice(5).trim());
      });
    }
  }
  clearTimeout(timer);

  const durationMs = now() - started;
  const contentMarkers = (content.match(new RegExp(LEAK_RE.source, 'gi')) || []);
  const reasoningMarkers = (reasoning.match(new RegExp(LEAK_RE.source, 'gi')) || []);

  console.log('');
  console.log('=== ПОТОК ===');
  console.log('  событий: ' + events.length + ', битых JSON: ' + sseBadJson + ', пустых чанков: ' + sseOther);
  console.log('  content: ' + content.length + ' символов, reasoning: ' + reasoning.length + ' символов');
  console.log('  finish_reason: ' + (finishReason || '<нет>') + ', [DONE]: ' + doneCount + ' шт.');
  console.log('  первый content через ' + (firstContentAt === null ? '-' : firstContentAt) + ' мс, ' +
    'последний через ' + (lastContentAt === null ? '-' : lastContentAt) + ' мс, всего ' + durationMs + ' мс');
  console.log('  порядок событий (хвост): ' +
    events.slice(-6).map((e) => e.kind + (e.reason ? ':' + e.reason : '')).join(' -> '));

  let failed = 0;
  function ok(cond, msg, detail) {
    if (cond) { console.log('  OK      ' + msg); }
    else { console.log('  ПРОБЛЕМА ' + msg + (detail ? ' :: ' + detail : '')); failed++; }
  }

  ok(leaks.length === 0, 'в delta.content нет маркеров размышлений',
    leaks.length ? leaks.map((l) => JSON.stringify(l.piece)).join(' | ') : '');
  ok(contentMarkers.length === 0, 'в склеенном content нет маркеров',
    contentMarkers.length ? contentMarkers.join(', ') : '');
  ok(reasoningMarkers.length === 0, 'в reasoning_content нет маркеров',
    reasoningMarkers.length ? reasoningMarkers.join(', ') : '');
  ok(finishReason !== null, 'finish_reason пришёл', 'без него клиент не знает причину остановки');
  ok(doneCount <= 1, 'не больше одного [DONE]', 'их ' + doneCount);
  ok(contentAfterDone === 0, 'после [DONE] нет контента', contentAfterDone + ' символов');
  ok(content.trim().length > 0, 'ответ не пустой', 'content=' + content.length + ' символов');

  console.log('');
  if (failed) {
    console.log('ИТОГ: FAIL (' + failed + ')');
    process.exit(1);
  }
  console.log('ИТОГ: PASS');
})().catch((e) => {
  console.error('ПРОБА УПАЛА: ' + (e && e.stack || e));
  process.exit(2);
});

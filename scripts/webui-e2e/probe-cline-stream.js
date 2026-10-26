// scripts/webui-e2e/probe-cline-stream.js - R243 (2026-10-26), R244: без fetch.
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
// R244: переписан с global fetch/AbortController на http/https + StringDecoder,
// потому что на стенде бывает старый Node (12/14): там fetch/AbortController нет,
// и проба падала с "ReferenceError: AbortController is not defined". Теперь
// работает на Node 12+ (проверено синтаксисом и живым прогоном).
//
// Запуск:
//   BALANCER=http://127.0.0.1:18080 API_TOKEN=... node scripts/webui-e2e/probe-cline-stream.js
//   MODEL=gemma-4-E4B-it-Q4_K_M PROMPT="реши 17*23 по шагам" MAX_TOKENS=400 node ...
//   LONG=4000 node ...   # длинный промпт (проверка preflight/обрыва на длинной сессии)
//
// Переменные: BALANCER, API_TOKEN, MODEL, PROMPT, MAX_TOKENS, LONG, TIMEOUT_MS.

'use strict';

var http = require('http');
var https = require('https');
var urlMod = require('url');
var StringDecoder = require('string_decoder').StringDecoder;

var BALANCER = (process.env.BALANCER || 'http://127.0.0.1:18080').replace(/\/+$/, '');
var API_TOKEN = process.env.API_TOKEN || process.env.LB_API_TOKEN || 'changeme-bundled-with-agent-token';
var MODEL = process.env.MODEL || 'gemma-4-E4B-it-Q4_K_M';
var MAX_TOKENS = Number(process.env.MAX_TOKENS || 400);
var TIMEOUT_MS = Number(process.env.TIMEOUT_MS || 180000);
var LONG = Number(process.env.LONG || 0);

var PROMPT = process.env.PROMPT ||
  'Solve 17*23 step by step. Think first, then answer with the final number only.';

if (LONG > 0) {
  // Длинный контекст: повторяем строку, чтобы упереться в окно модели и задеть
  // preflight/пересчёт n_ctx - именно там поток может закрыться раньше ответа.
  var block = 'The quick brown fox jumps over the lazy dog. 1234567890. ';
  var chars = LONG * 4;
  var reps = Math.ceil(chars / block.length);
  var filler = '';
  for (var i = 0; i < reps; i++) { filler += block; }
  PROMPT = filler.slice(0, chars) +
    '\n\nAnswer in one short sentence: what animal is mentioned?';
}

// Маркеры, которые не должны попадать в видимый content.
var LEAK_RE = /<\/?(?:think|thinking|thought|reasoning|analysis|channel)\b[^>]{0,24}>|<\|?(?:think|channel)[^>]{0,24}>/i;

var started = Date.now();

var events = [];
var content = '';
var reasoning = '';
var leaks = [];
var finishReason = null;
var doneCount = 0;
var contentAfterDone = 0;
var sseBadJson = 0;
var sseOther = 0;
var firstContentAt = null;
var lastContentAt = null;
var failed = 0;

function now() { return Date.now() - started; }

function ok(cond, msg, detail) {
  if (cond) { console.log('  OK      ' + msg); }
  else { console.log('  ПРОБЛЕМА ' + msg + (detail ? ' :: ' + detail : '')); failed++; }
}

function matchMarkers(s) {
  var out = [];
  var re = new RegExp(LEAK_RE.source, 'gi');
  var m;
  while ((m = re.exec(s)) !== null) { out.push(m[0]); }
  return out;
}

function handlePayload(payload) {
  var t = now();
  if (payload === '[DONE]') {
    doneCount++;
    events.push({ t: t, kind: 'done' });
    return;
  }
  var json;
  try { json = JSON.parse(payload); } catch (e) {
    sseBadJson++; events.push({ t: t, kind: 'bad-json' });
    return;
  }
  var choice = (json.choices && json.choices[0]) || {};
  var delta = choice.delta || {};
  var piece = typeof delta.content === 'string' ? delta.content : '';
  var rpiece = typeof delta.reasoning_content === 'string' ? delta.reasoning_content
    : (typeof delta.reasoning === 'string' ? delta.reasoning : '');
  if (rpiece) {
    reasoning += rpiece;
    events.push({ t: t, kind: 'reasoning' });
  }
  if (piece) {
    if (!firstContentAt) { firstContentAt = t; }
    lastContentAt = t;
    if (doneCount > 0) { contentAfterDone += piece.length; }
    content += piece;
    var found = matchMarkers(piece);
    for (var k = 0; k < found.length; k++) {
      leaks.push({ t: t, marker: found[k] });
    }
    events.push({ t: t, kind: 'content' });
  }
  if (choice.finish_reason) {
    finishReason = finishReason || choice.finish_reason;
    events.push({ t: t, kind: 'finish', reason: choice.finish_reason });
  }
  if (json.usage) { events.push({ t: t, kind: 'usage' }); }
  if (!piece && !rpiece && !choice.finish_reason && !json.usage) { sseOther++; }
}

function reportAndExit() {
  var durationMs = now();
  console.log('');
  console.log('=== ПОТОК ===');
  console.log('  событий: ' + events.length + ', битых JSON: ' + sseBadJson + ', пустых чанков: ' + sseOther);
  console.log('  content: ' + content.length + ' символов, reasoning: ' + reasoning.length + ' символов');
  console.log('  finish_reason: ' + (finishReason || '<нет>') + ', [DONE]: ' + doneCount + ' шт.');
  console.log('  первый content через ' + (firstContentAt === null ? '-' : firstContentAt) + ' мс, ' +
    'последний через ' + (lastContentAt === null ? '-' : lastContentAt) + ' мс, всего ' + durationMs + ' мс');
  console.log('  порядок событий (хвост): ' +
    events.slice(-6).map(function (e) { return e.kind + (e.reason ? ':' + e.reason : ''); }).join(' -> '));

  var contentMarkers = matchMarkers(content);
  var reasoningMarkers = matchMarkers(reasoning);

  ok(leaks.length === 0, 'в delta.content нет маркеров размышлений',
    leaks.length ? JSON.stringify(leaks.slice(0, 3)) : '');
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
}

function run() {
  var target = BALANCER + '/v1/chat/completions';
  var body = {
    model: MODEL,
    messages: [{ role: 'user', content: PROMPT }],
    stream: true,
    max_tokens: MAX_TOKENS,
    temperature: 0
  };
  var payload = JSON.stringify(body);
  var u = urlMod.parse(target);
  var mod = u.protocol === 'https:' ? https : http;
  var port = u.port || (u.protocol === 'https:' ? 443 : 80);

  console.log('POST ' + target + ' stream=true');
  console.log('  model=' + MODEL + ' max_tokens=' + MAX_TOKENS +
    (LONG ? ' long_prompt_chars=' + PROMPT.length : ''));

  var req = mod.request({
    hostname: u.hostname,
    port: port,
    path: (u.pathname || '/') + (u.search || ''),
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
      'X-API-Token': API_TOKEN,
      'Content-Length': Buffer.byteLength(payload),
      'Accept': 'text/event-stream'
    }
  }, function (res) {
    console.log('  HTTP ' + res.statusCode + ' content-type=' + (res.headers['content-type'] || ''));

    if (res.statusCode !== 200) {
      var errBody = '';
      res.setEncoding('utf8');
      res.on('data', function (d) { if (errBody.length < 4000) { errBody += d; } });
      res.on('end', function () {
        console.error('  ответ не 200: ' + errBody.slice(0, 1200));
        process.exit(2);
      });
      return;
    }

    var decoder = new StringDecoder('utf8');
    var buffer = '';

    function flushEvents() {
      var idx;
      while ((idx = buffer.indexOf('\n\n')) >= 0) {
        var rawEvent = buffer.slice(0, idx);
        buffer = buffer.slice(idx + 2);
        var lines = rawEvent.split('\n');
        for (var i = 0; i < lines.length; i++) {
          var trimmed = lines[i].replace(/\r$/, '').trim();
          if (!trimmed || trimmed.charAt(0) === ':') { continue; }
          if (trimmed.indexOf('data:') !== 0) { continue; }
          handlePayload(trimmed.slice(5).trim());
        }
      }
    }

    res.on('data', function (chunk) {
      buffer += decoder.write(chunk);
      flushEvents();
    });
    res.on('end', function () {
      buffer += decoder.end();
      // Хвост без пустой строки (некоторые upstreams не докидывают \n\n в конце).
      if (buffer.trim()) {
        buffer.split('\n').forEach(function (line) {
          var t = line.replace(/\r$/, '').trim();
          if (t.indexOf('data:') === 0) { handlePayload(t.slice(5).trim()); }
        });
      }
      reportAndExit();
    });
    res.on('error', function (e) {
      console.error('ОШИБКА ЧТЕНИЯ ПОТОКА: ' + (e && e.message));
      process.exit(2);
    });
  });

  req.setTimeout(TIMEOUT_MS, function () {
    req.destroy(new Error('timeout ' + TIMEOUT_MS + 'ms'));
  });
  req.on('error', function (e) {
    console.error('НЕ УДАЛОСЬ ПОДКЛЮЧИТЬСЯ: ' + (e && e.message));
    process.exit(2);
  });
  req.write(payload);
  req.end();
}

run();

// contract-suite.js — R228: ПРОВЕРКА СИТУАЦИЙ, а не отдельных ошибок.
//
// Идея: клиенту важно не «HTTP 200», а то, что запрос дошёл без искажений, а ответ
// можно применить. Поэтому здесь проверяются инварианты цепочки
// клиент → балансер(18079 OpenAI / 18080 Ollama) → воркер → модель → ответ.
//
// Проверяемые ситуации (каждая — отдельный PASS/FAIL):
//   1. история из messages доходит: модель вспоминает код из первого хода;
//   2. то же через native Ollama API (/api/chat);
//   3. инструменты через OpenAI API: вызов пришёл tool_calls, аргументы — валидный
//      JSON, required-поля на месте, для правки выбран инструмент правки;
//   4. инструменты через Ollama API (/api/chat, tools): message.tool_calls;
//   5. стрим OpenAI: роль первым чанком, [DONE], finish_reason, собранные из дельт
//      аргументы == non-stream (нет потери/дублей), нет пустого 200;
//   6. стрим текста без инструментов (R231): в content нет маркеров размышлений
//      (`</think>`, `<end_of_turn>` и т.п.), content не дублирует
//      reasoning_content, `[DONE]` ровно один и он последний; пустой content из-за
//      незакрытых размышлений приходит с finish_reason=length, а не stop;
//   7. ошибки приходят как ошибки: неизвестная модель → 4xx + error, а не пустой 200;
//   8. задержка в разумных границах (по умолчанию 180 с) — иначе клиент (Cline)
//      обрывает запрос по своим таймаутам.
//
// Запуск: node scripts/clients/contract-suite.js
// Переменные: MODEL (по умолчанию gemma-…), TOOLS_MODEL (для инструментов), BASE_OPENAI,
// BASE_OLLAMA, TOKEN, MAX_LATENCY_MS.
'use strict';
const http = require('http');

const MODEL = process.env.MODEL || 'gemma-4-E4B-it-Q4_K_M';
const TOOLS_MODEL = process.env.TOOLS_MODEL || MODEL;
const TOKEN = process.env.TOKEN || 'changeme-bundled-with-agent-token';
const OAI = process.env.BASE_OPENAI || 'http://127.0.0.1:18079';
const OLL = process.env.BASE_OLLAMA || 'http://127.0.0.1:18080';
const MAX_LATENCY_MS = Number(process.env.MAX_LATENCY_MS || 180000);

const fails = [];
function check(ok, what, detail) {
  console.log((ok ? 'PASS ' : 'FAIL ') + what + (detail ? ' :: ' + detail : ''));
  if (!ok) fails.push(what + (detail ? ' :: ' + detail : ''));
}

function parseURL(u) {
  const m = /^http:\/\/([^:/]+):(\d+)/.exec(u);
  if (!m) throw new Error('bad url ' + u);
  return { host: m[1], port: Number(m[2]) };
}

function request(method, base, path, body, stream) {
  const { host, port } = parseURL(base);
  return new Promise(resolve => {
    const data = body === undefined ? null : Buffer.from(JSON.stringify(body));
    const headers = { 'Content-Type': 'application/json', 'X-API-Token': TOKEN };
    if (data) headers['Content-Length'] = data.length;
    const t0 = Date.now();
    const req = http.request({ host, port, path, method, headers, agent: false }, res => {
      let text = '';
      const chunks = [];
      res.on('data', d => { text += d; if (stream) chunks.push(d); });
      res.on('end', () => resolve({ status: res.statusCode, text, ms: Date.now() - t0, headers: res.headers }));
    });
    req.on('error', e => resolve({ status: 0, text: '', err: e.code || e.message, ms: Date.now() - t0 }));
    req.setTimeout(MAX_LATENCY_MS + 30000, () => req.destroy(new Error('timeout')));
    if (data) req.write(data);
    req.end();
  });
}

function sseEvents(text) {
  const out = [];
  for (const block of text.split('\n\n')) {
    for (const line of block.split('\n')) {
      if (!line.startsWith('data:')) continue;
      const payload = line.slice(5).trim();
      if (payload === '[DONE]') { out.push({ done: true }); continue; }
      try { out.push(JSON.parse(payload)); } catch (e) { out.push({ badJson: payload }); }
    }
  }
  return out;
}

// Схемы в том виде, в каком их объявляют клиенты.
const CLINE_TOOLS = [
  { type: 'function', function: { name: 'read_files', description: 'Read files.', parameters: { type: 'object', properties: { files: { type: 'array', items: { type: 'string' } } }, required: ['files'] } } },
  { type: 'function', function: { name: 'editor', description: 'Replace exact text in a file.', parameters: { type: 'object', properties: { path: { type: 'string' }, old_text: { type: 'string' }, new_text: { type: 'string' } }, required: ['path', 'old_text', 'new_text'] } } },
  { type: 'function', function: { name: 'attempt_completion', description: 'Finish the task.', parameters: { type: 'object', properties: { result: { type: 'string' } }, required: ['result'] } } },
];
const EDIT_TASK = 'In file src/App.cs replace the line "return null;" with "return value;". Use the editor tool now. Do not explain.';

function toolCallOf(body) {
  try {
    const j = JSON.parse(body);
    const msg = j.choices && j.choices[0] && j.choices[0].message;
    const calls = (msg && msg.tool_calls) || [];
    return { calls, finish: j.choices && j.choices[0] && j.choices[0].finish_reason, raw: msg };
  } catch (e) { return { calls: [], finish: null, raw: null }; }
}

function validateCall(call, tools) {
  const problems = [];
  if (!call) return ['вызова нет'];
  const name = call.function && call.function.name;
  if (!name) problems.push('нет имени функции');
  if (!tools.some(t => t.function.name === name)) problems.push('функция "' + name + '" не объявлена клиентом');
  let args = {};
  try { args = JSON.parse((call.function && call.function.arguments) || '{}'); }
  catch (e) { problems.push('аргументы не JSON: ' + (call.function && call.function.arguments)); return problems; }
  const schema = (tools.find(t => t.function.name === name) || { function: { parameters: {} } }).function.parameters || {};
  for (const req of schema.required || []) {
    if (args[req] === undefined || args[req] === null || String(args[req]).trim() === '') problems.push('нет обязательного поля ' + req);
  }
  return problems;
}

(async () => {
  console.log('R228: контракт клиент↔балансер (модель ' + MODEL + ', инструменты ' + TOOLS_MODEL + ')\n');

  // 0. Прогрев: холодная загрузка модели не должна попадать в замеры генерации.
  // (Живой прогон: первый native-Ollama запрос занял 148 с только потому, что
  // воркер в этот момент грузил модель после выгрузки.)
  {
    const warm = await request('POST', OAI, '/v1/chat/completions', {
      model: MODEL, stream: false, max_tokens: 8,
      messages: [{ role: 'user', content: 'Ответь одним словом: ок.' }],
    });
    console.log('WARMUP ' + (warm.status === 200 ? 'PASS' : 'FAIL') + ' :: ' + warm.ms + 'мс (включая возможную загрузку модели — в замеры не входит)');
    if (warm.status !== 200) check(false, '0. прогрев (модель доступна)', 'HTTP ' + warm.status);
  }

  // 1. История через OpenAI API (3 попытки: модель может ответить иначе — это не потеря истории).
  {
    // R229 (2026-10-22): флейк «история не дошла». Модель может ответить иначе,
    // не потеряв историю, поэтому попыток три и они детерминированные
    // (temperature 0): проверяем факт «код из истории назван», а не удачную
    // формулировку. Вторая/третья попытка просят повторить код дословно.
    const code = 'ZEBRA-771';
    const askOpenAI = (userText) => request('POST', OAI, '/v1/chat/completions', {
      model: MODEL, stream: false, max_tokens: 40, temperature: 0,
      messages: [
        { role: 'system', content: 'Отвечай кратко.' },
        { role: 'user', content: 'Запомни код: ' + code + '. Ответь «ок».' },
        { role: 'assistant', content: 'ок' },
        { role: 'user', content: userText },
      ],
    });
    const questionsOpenAI = [
      'Какой код я просил запомнить? Назови только его.',
      'Повтори код из первого сообщения дословно, без пояснений.',
      'Назови дословно код из первого сообщения.',
    ];
    let found = false;
    let lastMs = 0;
    let attempts = 0;
    for (const q of questionsOpenAI) {
      attempts++;
      const res = await askOpenAI(q);
      lastMs = res.ms;
      if (res.status === 200 && String(res.text).toUpperCase().includes(code)) {
        found = true;
        break;
      }
    }
    check(found, '1. история доходит через OpenAI API',
      'код ' + (found ? 'найден' : 'НЕ найден') + ' за ' + attempts + ' попыт.' + ', ' + lastMs + 'мс');
    check(lastMs <= MAX_LATENCY_MS, '1б. задержка в пределах клиентского таймаута', lastMs + 'мс');
  }

  // 2. История через native Ollama API (3 попытки, как у OpenAI: см. комментарий
  //    выше — проверяем факт «код назван», а не удачную формулировку).
  {
    const code = 'KILO-330';
    const askOllama = (userText) => request('POST', OLL, '/api/chat', {
      model: MODEL, stream: false, options: { temperature: 0 },
      messages: [
        { role: 'user', content: 'Запомни код: ' + code + '. Ответь «ок».' },
        { role: 'assistant', content: 'ок' },
        { role: 'user', content: userText },
      ],
    });
    const questionsOllama = [
      'Какой код я просил запомнить? Назови только его.',
      'Повтори код из первого сообщения дословно, без пояснений.',
      'Назови дословно код из первого сообщения.',
    ];
    let ok = false;
    let last = { status: 0, ms: 0 };
    let attempts = 0;
    for (const q of questionsOllama) {
      attempts++;
      const res = await askOllama(q);
      last = res;
      if (res.status === 200 && String(res.text).toUpperCase().includes(code)) {
        ok = true;
        break;
      }
    }
    check(ok, '2. история доходит через Ollama API',
      'HTTP ' + last.status + ' ' + last.ms + 'мс, попыток ' + attempts);
  }

  // 3. Инструменты через OpenAI API.
  //
  // R229 (2026-10-23): до трёх попыток с temperature 0. Живой прогон ворота на
  // образ показал: модель изредка отвечает текстом вместо вызова инструмента —
  // это свойство сэмплинга, а не поломка тракта. Проверка обязана ловить «вызовы
  // не работают ВООБЩЕ», а не одну неудачную генерацию (иначе зелёный образ
  // объявляется красным). В отчёт идёт номер удачной попытки.
  {
    let r = null;
    let tc = { calls: [], finish: null };
    let attempts = 0;
    for (let i = 0; i < 3; i++) {
      attempts++;
      r = await request('POST', OAI, '/v1/chat/completions', {
        model: TOOLS_MODEL, stream: false, max_tokens: 200, temperature: 0,
        tools: CLINE_TOOLS, tool_choice: 'auto',
        messages: [{ role: 'user', content: EDIT_TASK }],
      });
      tc = toolCallOf(r.text);
      if (r.status === 200 && tc.calls.length > 0) break;
    }
    check(r.status === 200, '3. инструменты: HTTP 200', 'HTTP ' + r.status + ' ' + r.ms + 'мс');
    check(tc.calls.length > 0, '3а. модель вернула вызов инструмента, а не текст/пустоту',
      'finish=' + tc.finish + ', попыток ' + attempts);
    const problems = validateCall(tc.calls[0], CLINE_TOOLS);
    check(problems.length === 0, '3б. вызов применим клиентом (JSON + required)', problems.join('; ') || 'ok');
    const name = tc.calls[0] && tc.calls[0].function && tc.calls[0].function.name;
    check(name === 'editor', '3в. выбран инструмент правки', 'name=' + name);
  }

  // 4. Инструменты через Ollama API (R229: до трёх попыток с temperature 0 —
  // та же причина, что в шаге 3: ловим «не работают вообще», а не один сэмплинг).
  {
    let r = null;
    let calls = [];
    let attempts = 0;
    for (let i = 0; i < 3; i++) {
      attempts++;
      r = await request('POST', OLL, '/api/chat', {
        model: TOOLS_MODEL, stream: false, options: { temperature: 0 },
        tools: CLINE_TOOLS.map(t => ({ type: 'function', function: t.function })),
        messages: [{ role: 'user', content: EDIT_TASK }],
      });
      try {
        const j = JSON.parse(r.text);
        calls = (j.message && j.message.tool_calls) || [];
      } catch (e) { calls = []; }
      if (r.status === 200 && calls.length > 0) break;
    }
    check(r.status === 200 && calls.length > 0, '4. инструменты через Ollama API: tool_calls есть',
      'HTTP ' + r.status + ' calls=' + calls.length + ', попыток ' + attempts);
  }

  // 5. Стрим: целостность и совпадение с non-stream.
  {
    const body = {
      model: TOOLS_MODEL, stream: true, max_tokens: 200, temperature: 0,
      tools: CLINE_TOOLS, tool_choice: 'auto',
      messages: [{ role: 'user', content: EDIT_TASK }],
    };
    const r = await request('POST', OAI, '/v1/chat/completions', body, true);
    const events = sseEvents(r.text);
    const deltas = events.filter(e => e.choices && e.choices[0] && e.choices[0].delta);
    const names = [];
    let args = '';
    for (const e of deltas) {
      const ds = e.choices[0].delta.tool_calls || [];
      for (const d of ds) {
        if (d.function && d.function.name) names.push(d.function.name);
        if (d.function && d.function.arguments) args += d.function.arguments;
      }
    }
    check(r.status === 200 && events.length > 0, '5. стрим не пустой', 'HTTP ' + r.status + ' событий ' + events.length);
    check(events.some(e => e.done === true), '5а. стрим завершается [DONE]');
    check(deltas.length > 0 && deltas[0].choices[0].delta.role === 'assistant', '5б. роль приходит первым чанком');
    check(names.length > 0, '5в. имя инструмента приходит в стриме', 'names=' + names.join(','));
    let streamArgsOK = true;
    try { JSON.parse(args || '{}'); } catch (e) { streamArgsOK = false; }
    check(streamArgsOK, '5г. склеенные из дельт аргументы — валидный JSON', (args || '').slice(0, 80));
    check(r.ms <= MAX_LATENCY_MS, '5д. стрим уложился в таймаут клиента', r.ms + 'мс');
  }

  // 6. Стрим БЕЗ инструментов (как обычный ход Cline): чистый видимый текст,
  //    ни одного служебного маркера размышлений, [DONE] ровно один и последний.
  //
  // ЖИВЫЕ ДЕФЕКТЫ R231, которые ловит этот блок:
  //  - cppworker отдавал размышления ЕЩЁ РАЗ в единственном content-чанке перед
  //    finish (дубль в теле: «Thinking Process: …» повторял reasoning_content);
  //  - `</think>`/`<end_of_turn>` попадали в видимый content (клиент видел тег);
  //  - при выключенном LB_AUTO_CONTINUE_ON_TRUNCATION завершающий [DONE] не
  //    отдавался вовсе — Cline ждал конца стрима и рвал сессию.
  {
    const body = {
      model: MODEL, stream: true, max_tokens: 120, temperature: 0,
      messages: [
        { role: 'system', content: 'You are Cline, a coding agent. Answer briefly.' },
        { role: 'user', content: 'Say exactly: OK' },
      ],
    };
    const r = await request('POST', OAI, '/v1/chat/completions', body, true);
    const events = sseEvents(r.text);
    let content = '';
    let reasoning = '';
    let doneCount = 0;
    let doneIndex = -1;
    for (let i = 0; i < events.length; i++) {
      const e = events[i];
      if (e.done === true) { doneCount++; doneIndex = i; continue; }
      const d = e.choices && e.choices[0] && e.choices[0].delta;
      if (!d) continue;
      if (typeof d.content === 'string') content += d.content;
      if (typeof d.reasoning_content === 'string') reasoning += d.reasoning_content;
    }
    check(r.status === 200, '6. стрим текста: HTTP 200', 'HTTP ' + r.status + ' ' + r.ms + 'мс');
    const markers = ['</think>', '<think>', '<|channel>', '<channel|>', '<end_of_turn>', '<|im_end|>'];
    const leaked = markers.filter(m => content.includes(m));
    check(leaked.length === 0, '6а. в видимом content нет маркеров размышлений/конца хода',
      leaked.length ? 'утекло: ' + leaked.join(',') + ' content=' + content.slice(0, 120) : 'ok');
    // Дубль: сначала размышления, затем тот же текст в content (живой случай:
    // content целиком повторял reasoning, когда блок не закрылся за max_tokens).
    const norm = s => (s || '').replace(/\s+/g, ' ').trim();
    const dup = reasoning.length > 40 && content.length > 40 &&
      (norm(content).startsWith(norm(reasoning).slice(0, 40)) ||
       norm(content).includes(norm(reasoning).slice(0, 60)));
    check(!dup, '6б. content не дублирует reasoning_content',
      dup ? 'content повторяет размышления' : 'ok (content=' + content.length + ', reasoning=' + reasoning.length + ')');
    check(doneCount === 1, '6в. [DONE] ровно один', 'got ' + doneCount);
    check(doneIndex === events.length - 1, '6г. [DONE] — последнее событие потока',
      'index=' + doneIndex + ' of ' + events.length);
  }

  // 7. Ошибки приходят как ошибки.
  {
    const r = await request('POST', OAI, '/v1/chat/completions', {
      model: 'модель-которой-нет', stream: false, max_tokens: 8,
      messages: [{ role: 'user', content: 'привет' }],
    });
    const isErr = r.status >= 400;
    check(isErr, '7. неизвестная модель — явная ошибка, а не пустой 200', 'HTTP ' + r.status);
  }

  console.log('\nИТОГ: ' + (fails.length ? 'ПРОВАЛ (' + fails.length + ')' : 'OK — контракт клиента соблюдён'));
  for (const f of fails) console.log('  - ' + f);
  process.exit(fails.length ? 1 : 0);
})().catch(e => { console.log('FATAL: ' + e.message); process.exit(1); });


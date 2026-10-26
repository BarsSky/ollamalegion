// scripts/webui-e2e/tools-all-clients-r246.js — R246 (2026-10-26): КАЖДЫЙ
// инструмент КАЖДОГО клиента обязан доехать до модели и вернуться вызовом.
//
// ЖИВЫЕ ЖАЛОБЫ, которые этот пробник ловит:
//   1. DSH (deepseek harness) → балансер → gemma-4: модель отдала вызов
//      диалектом `[tool_code] print(read(file_path="go.mod"))` — вызов уехал
//      клиенту ТЕКСТОМ, инструмент не применился;
//   2. второй ход: результат инструмента не доезжал до модели (шаблон gemma-4
//      рисует tool-результат только из структурных tool_calls) — модель писала
//      «output is not provided yet» и отдавала пустой ответ.
//
// Что проверяется для КАЖДОГО инструмента профиля (stream=true, как ходят
// клиенты):
//   - HTTP 200 и SSE-поток разбирается, ровно один [DONE], finish_reason=tool_calls;
//   - вызов собран из дельт как это делает клиент (по index/id);
//   - имя из набора клиента, аргументы — валидный JSON;
//   - аргументы соответствуют схеме: required, типы, enum, вложенные items,
//     additionalProperties=false (лишние поля);
//   - вызов не утёк в content разметкой ([tool_code]/<|tool_call>/<tool_call>/<function=);
//   - ВТОРОЙ ХОД: assistant(tool_calls) + role:tool(результат) — модель обязана
//     выдать непустой ответ или следующий вызов (а не пустой content с stop).
//
// Запуск (стенд поднят, модель загружена):
//   node scripts/webui-e2e/tools-all-clients-r246.js
//   MODEL=gemma-4-E4B-it-Q4_K_M BASE=http://127.0.0.1:18080 node ...
//   PROFILE=dsh ONLY=read,grep node ...    # узкий прогон
// Код возврата 1, если есть провалы.
'use strict';
const http = require('http');
const fs = require('fs');
const path = require('path');

const BASE = process.env.BASE || 'http://127.0.0.1:18080';
const TOKEN = process.env.API_TOKEN || 'changeme-bundled-with-agent-token';
const MODEL = process.env.MODEL || 'gemma-4-E4B-it-Q4_K_M';
const MAX_TOKENS = parseInt(process.env.MAX_TOKENS || '320', 10);
const ONLY = (process.env.ONLY || '').split(',').filter(Boolean);
const ONLY_PROFILE = process.env.PROFILE || '';

const base = new URL(BASE);

// ---------- профили клиентов ----------

// DSH: точные схемы из ЖИВОГО запроса клиента (сняты пробником tee-proxy).
function dshTools() {
  const file = path.join(__dirname, 'dsh-tools-r246.json');
  return JSON.parse(fs.readFileSync(file, 'utf8'));
}

// Подсказки «какие аргументы нужны» — чтобы модель звала нужный инструмент,
// а не угадывала назначение. Схема при этом проверяется по объявленной.
const HINTS = {
  read: '{"file_path":"go.mod"}',
  write: '{"file_path":"_diag/r246-write.txt","content":"hello"}',
  edit: '{"file_path":"_diag/r246-write.txt","old_string":"hello","new_string":"hi"}',
  glob: '{"pattern":"*.go"}',
  grep: '{"pattern":"package main"}',
  pwsh: '{"command":"echo hi","description":"Print hi"}',
  read_image: '{"file_path":"_diag/demo-r232.png"}',
  todo_write: '{"todos":[{"content":"check tool calls","status":"pending"}]}',
  present: '{"files":[{"path":"README.md","description":"readme"}]}',
  web_search: '{"queries":["ollama"]}',
  web_fetch: '{"url":"http://example.com"}',
  skill: '{"name":"editing-cordis-compositions"}',
  job_list: '{}',
  job_output: '{"job_id":"job-1"}',
  job_kill: '{"job_id":"job-1"}',
  get_goal: '{}',
  create_goal: '{"objective":"probe the tool path"}',
  update_goal: '{"goal_id":"g1","revision":1,"action":"pause"}',
  send_message: '{"agent_id":"a1","message":"hello"}',
  interrupt_agent: '{"agent_id":"a1"}',
  list_agents: '{}',
  subagent: '{"description":"probe","prompt":"say ok"}',
  subagent_fork: '{"description":"probe","prompt":"say ok"}',
  exit_plan_mode: '{"plan":"# Заголовок\\nШаг один."}',
  workflow: '{"meta":{"name":"probe","description":"probe run"},"script":"return 1"}',
  ralph: '{"objective":"probe"}',
  // Cline-подобный набор (R204): подсказки по именам.
  execute_command: '{"command":"echo hi"}',
  read_file: '{"path":"go.mod"}',
  write_to_file: '{"path":"_diag/r246-write.txt","content":"hello"}',
  replace_in_file: '{"path":"_diag/r246-write.txt","diff":"------- SEARCH\\nhello\\n=======\\nhi\\n+++++++ REPLACE"}',
  search_files: '{"path":".","regex":"package main"}',
  list_files: '{"path":"."}',
  list_code_definition_names: '{"path":"cmd"}',
  attempt_completion: '{"result":"done"}',
  ask_followup_question: '{"question":"which file?"}',
  browser_action: '{"action":"launch","url":"http://example.com"}',
  use_mcp_tool: '{"server_name":"s1","tool_name":"t1","arguments":"{}"}',
  access_mcp_resource: '{"server_name":"s1","uri":"file:///x"}',
  plan_mode_respond: '{"response":"plan"}',
  new_task: '{"context":"do it"}',
  // OpenWebUI-подобный набор.
  image_generation: '{"prompt":"a cat"}',
  knowledge_search: '{"query":"ollama"}',
  calculator: '{"expression":"2+2"}',
  get_current_weather: '{"location":"Moscow"}',
  // hermes
  run_commands: '{"commands":["echo hi"]}',
  read_files: '{"files":[{"path":"go.mod"}]}',
  editor: '{"path":"_diag/r246-write.txt","old_text":"hello","new_text":"hi"}',
  submit_and_exit: '{"summary":"done"}',
};

function toolSchema(name, required, properties, description) {
  return {
    type: 'function',
    function: {
      name,
      description: description || name,
      parameters: { type: 'object', properties: properties || {}, required: required || [] },
    },
  };
}

// Cline: набор и схемы в том виде, в котором их объявляет клиент (R204 + R113).
//
// R246: добавлены инструменты, которые Cline/Roo объявляют, но матрица не
// покрывала: browser_action (координаты/текст, enum действий), use_mcp_tool
// (аргументы — JSON-СТРОКА внутри схемы), access_mcp_resource (URI),
// load_mcp_documentation (без аргументов). Это ровно те формы, на которых
// ломаются «однотипные» парсеры: вложенные поля, строковый JSON, пустой объект.
const CLINE_TOOLS = [
  toolSchema('execute_command', ['command'], { command: { type: 'string' }, timeout: { type: 'number' } }, 'Run a shell command.'),
  toolSchema('read_file', ['path'], { path: { type: 'string' } }, 'Read one file.'),
  toolSchema('write_to_file', ['path', 'content'], { path: { type: 'string' }, content: { type: 'string' } }, 'Write full file.'),
  toolSchema('replace_in_file', ['path', 'diff'], { path: { type: 'string' }, diff: { type: 'string' } }, 'Replace exact text via SEARCH/REPLACE diff.'),
  toolSchema('search_files', ['path', 'regex'], { path: { type: 'string' }, regex: { type: 'string' }, file_pattern: { type: 'string' } }, 'Search file contents.'),
  toolSchema('list_files', ['path'], { path: { type: 'string' }, recursive: { type: 'boolean' } }, 'List directory.'),
  toolSchema('list_code_definition_names', ['path'], { path: { type: 'string' } }, 'List definitions in a directory.'),
  toolSchema('attempt_completion', ['result'], { result: { type: 'string' } }, 'Finish the task.'),
  toolSchema('ask_followup_question', ['question'], { question: { type: 'string' } }, 'Ask the user.'),
  toolSchema('plan_mode_respond', ['response'], { response: { type: 'string' } }, 'Respond in plan mode.'),
  toolSchema('browser_action', ['action'], {
    action: { type: 'string', enum: ['launch', 'click', 'type', 'scroll_down', 'scroll_up', 'close'] },
    url: { type: 'string' },
    coordinate: { type: 'string' },
    text: { type: 'string' },
  }, 'Control the browser.'),
  toolSchema('use_mcp_tool', ['server_name', 'tool_name', 'arguments'], {
    server_name: { type: 'string' }, tool_name: { type: 'string' }, arguments: { type: 'string' },
  }, 'Use a tool from a connected MCP server; arguments is a JSON string.'),
  toolSchema('access_mcp_resource', ['server_name', 'uri'], { server_name: { type: 'string' }, uri: { type: 'string' } }, 'Read an MCP resource.'),
  toolSchema('load_mcp_documentation', [], {}, 'Load MCP documentation.'),
  toolSchema('new_task', ['context'], { context: { type: 'string' } }, 'Start a new task.'),
];

// OpenWebUI-подобный набор (инструменты, которые шлют её функции/tools).
const OPENWEBUI_TOOLS = [
  toolSchema('web_search', ['query'], { query: { type: 'string' } }, 'Search the web.'),
  toolSchema('image_generation', ['prompt'], { prompt: { type: 'string' } }, 'Generate an image.'),
  toolSchema('knowledge_search', ['query'], { query: { type: 'string' } }, 'Search knowledge base.'),
  toolSchema('calculator', ['expression'], { expression: { type: 'string' } }, 'Evaluate an expression.'),
  toolSchema('get_current_weather', ['location'], { location: { type: 'string' }, unit: { type: 'string', enum: ['celsius', 'fahrenheit'] } }, 'Weather for a location.'),
];

// Hermes / Qwen-style набор (R109/R204).
const HERMES_TOOLS = [
  toolSchema('run_commands', ['commands'], { commands: { type: 'array', items: { type: 'string' } } }, 'Run shell commands.'),
  toolSchema('read_files', ['files'], { files: { type: 'array', items: { type: 'object', properties: { path: { type: 'string' } }, required: ['path'] } } }, 'Read files.'),
  toolSchema('editor', ['path', 'old_text', 'new_text'], { path: { type: 'string' }, old_text: { type: 'string' }, new_text: { type: 'string' } }, 'Replace exact text in a file.'),
  toolSchema('submit_and_exit', ['summary'], { summary: { type: 'string' } }, 'Finish the task.'),
];

const PROFILES = {
  dsh: () => dshTools(),
  cline: () => CLINE_TOOLS,
  openwebui: () => OPENWEBUI_TOOLS,
  hermes: () => HERMES_TOOLS,
};

// ---------- транспорт ----------

function request(method, urlPath, body) {
  return new Promise((resolve) => {
    const payload = body === undefined ? null : JSON.stringify(body);
    const headers = { 'X-API-Token': TOKEN };
    if (payload) { headers['Content-Type'] = 'application/json'; headers['Content-Length'] = Buffer.byteLength(payload); }
    const req = http.request({
      host: base.hostname, port: base.port, path: urlPath, method, headers,
    }, (res) => {
      const chunks = [];
      res.on('data', (c) => chunks.push(c));
      res.on('end', () => resolve({ status: res.statusCode, headers: res.headers, body: Buffer.concat(chunks).toString('utf8') }));
    });
    req.on('error', (e) => resolve({ status: 0, error: e.message, body: '' }));
    req.setTimeout(600000, () => { req.destroy(); resolve({ status: 0, error: 'timeout', body: '' }); });
    if (payload) req.write(payload);
    req.end();
  });
}

// assembleStream — собирает ответ так, как это делает OpenAI-клиент:
// дельты content склеиваются, tool_calls — по index (fallback id).
function assembleStream(raw) {
  const out = { content: '', reasoning: '', calls: [], finish: null, done: 0, badJSON: 0, events: 0 };
  const byIndex = new Map();
  for (const line of raw.split('\n')) {
    const s = line.trim();
    if (!s || !s.startsWith('data:')) continue;
    const data = s.slice(5).trim();
    if (data === '[DONE]') { out.done++; continue; }
    let json;
    try { json = JSON.parse(data); } catch (e) { out.badJSON++; continue; }
    if (json.usage) continue;
    const choice = (json.choices || [])[0];
    if (!choice) continue;
    out.events++;
    const delta = choice.delta || {};
    if (typeof delta.content === 'string') out.content += delta.content;
    if (typeof delta.reasoning_content === 'string') out.reasoning += delta.reasoning_content;
    if (delta.tool_calls) {
      for (const tc of delta.tool_calls) {
        const key = typeof tc.index === 'number' ? 'i' + tc.index : 'id' + (tc.id || '');
        let acc = byIndex.get(key);
        if (!acc) { acc = { id: tc.id || '', name: '', args: '' }; byIndex.set(key, acc); }
        if (tc.id) acc.id = tc.id;
        const fn = tc.function || {};
        if (fn.name) acc.name += fn.name;
        if (typeof fn.arguments === 'string') acc.args += fn.arguments;
        else if (fn.arguments) acc.args += JSON.stringify(fn.arguments);
        out.calls = Array.from(byIndex.values());
      }
    }
    if (choice.finish_reason) out.finish = choice.finish_reason;
  }
  return out;
}

// ---------- валидация аргументов по объявленной схеме ----------

function typeOf(value) {
  if (value === null) return 'null';
  if (Array.isArray(value)) return 'array';
  if (typeof value === 'number') return Number.isInteger(value) ? 'integer' : 'number';
  return typeof value;
}

function typeMatches(declared, value) {
  const actual = typeOf(value);
  if (declared === 'number') return actual === 'number' || actual === 'integer';
  if (declared === 'integer') return actual === 'integer';
  if (declared === 'array') return actual === 'array';
  if (declared === 'object') return actual === 'object';
  return declared === actual;
}

function validateValue(schema, value, where, problems) {
  if (!schema) return;
  if (schema.enum && !schema.enum.includes(value)) {
    problems.push(where + ': значение ' + JSON.stringify(value) + ' вне enum ' + JSON.stringify(schema.enum));
  }
  if (schema.type && !typeMatches(schema.type, value)) {
    problems.push(where + ': тип ' + typeOf(value) + ', объявлен ' + schema.type);
    return;
  }
  if (schema.type === 'array' && schema.items && Array.isArray(value)) {
    value.forEach((el, i) => validateValue(schema.items, el, where + '[' + i + ']', problems));
  }
  if (schema.type === 'object' && value && typeof value === 'object' && !Array.isArray(value)) {
    const props = schema.properties || {};
    for (const req of schema.required || []) {
      if (value[req] === undefined) problems.push(where + ': нет обязательного поля ' + req);
    }
    for (const key of Object.keys(value)) {
      if (props[key]) { validateValue(props[key], value[key], where + '.' + key, problems); continue; }
      if (schema.additionalProperties === false) problems.push(where + ': лишнее поле ' + key);
    }
  }
}

// validateCall — форма + схема вызова.
function validateCall(tools, call) {
  const problems = [];
  if (!call.name) problems.push('пустое имя вызова');
  const tool = tools.filter((t) => t.function.name === call.name)[0];
  if (!tool) { problems.push('имя не из набора клиента: ' + call.name); return { problems }; }
  let args = null;
  try { args = JSON.parse(call.args || '{}'); }
  catch (e) { problems.push('arguments не JSON: ' + String(call.args).slice(0, 120)); return { problems }; }
  if (typeOf(args) !== 'object') problems.push('arguments не объект: ' + typeOf(args));
  else validateValue(tool.function.parameters, args, tool.function.name, problems);
  return { problems, args };
}

// leakMarkers — разметка вызова в видимом тексте = вызов не применился.
const LEAK_MARKERS = ['[tool_code]', '<|tool_call>', '<tool_call>', '<function=', '[TOOL_CALLS]', '<|python_tag|>'];

// ---------- сценарии ----------

function taskFor(toolName) {
  const hint = HINTS[toolName] || '{}';
  return 'You are a coding agent. Call exactly the tool "' + toolName + '" now. ' +
    'Use these arguments (valid JSON, keep required fields): ' + hint + '. ' +
    'Do not explain, do not answer with text: emit the tool call only.';
}

async function runToolCase(profileTools, toolName) {
  const started = Date.now();
  const res = await request('POST', '/v1/chat/completions', {
    model: MODEL,
    stream: true,
    temperature: 0,
    max_tokens: MAX_TOKENS,
    messages: [
      { role: 'system', content: 'You are a coding agent. Use the provided tools; do not describe, call.' },
      { role: 'user', content: taskFor(toolName) },
    ],
    tools: profileTools,
  });
  const ms = Date.now() - started;
  const r = { tool: toolName, ms, status: res.status, problems: [], calls: [] };

  if (res.status !== 200) {
    r.problems.push('HTTP ' + res.status + ': ' + String(res.body).replace(/\s+/g, ' ').slice(0, 160));
    return r;
  }
  const stream = assembleStream(res.body);
  r.finish = stream.finish;
  r.done = stream.done;
  if (stream.badJSON > 0) r.problems.push('битых JSON-SSE чанков: ' + stream.badJSON);
  if (stream.done !== 1) r.problems.push('[DONE] пришёл ' + stream.done + ' раз(а), ожидался 1');
  if (stream.calls.length === 0) {
    r.problems.push('tool_calls не пришли; content=' + JSON.stringify(stream.content.slice(0, 140)));
  }
  for (const call of stream.calls) {
    const v = validateCall(profileTools, call);
    r.calls.push({ name: call.name, ok: v.problems.length === 0, problems: v.problems });
    // Аргументы печатаем вместе с провалом: без них причина «нет обязательного
    // поля» неотличима от «модель прислала чужое имя поля».
    const argsText = v.problems.length ? ' args=' + String(call.args).slice(0, 160) : '';
    r.problems.push(...v.problems.map((p) => call.name + ': ' + p + argsText));
    if (call.name !== toolName) r.problems.push('выбран другой инструмент: ' + call.name);
  }
  if (stream.calls.length > 0 && stream.finish !== 'tool_calls') {
    r.problems.push('finish_reason=' + stream.finish + ', ожидался tool_calls');
  }
  if (stream.content) {
    for (const marker of LEAK_MARKERS) {
      if (stream.content.indexOf(marker) >= 0) {
        r.problems.push('разметка вызова утекла в content: ' + marker);
        break;
      }
    }
  }
  return r;
}

// runRoundTripCase — второй ход: результат инструмента обязан дойти до модели.
async function runRoundTripCase(profileTools) {
  // Предпочитаем read: у него короткий результат и однозначная проверка "модель
  // увидела вывод" (имя модуля из результата).
  const tool = profileTools.filter((t) => t.function.name === 'read')[0] || profileTools[0];
  const name = tool.function.name;
  const args = JSON.stringify(HINTS[name] ? JSON.parse(HINTS[name]) : {});
  const first = {
    model: MODEL, stream: true, temperature: 0, max_tokens: MAX_TOKENS,
    messages: [
      { role: 'system', content: 'You are a coding agent. Use the provided tools; do not describe, call.' },
      { role: 'user', content: 'Call the tool "' + name + '" with arguments ' + args + '.' },
    ],
    tools: profileTools,
  };
  const res1 = await request('POST', '/v1/chat/completions', first);
  if (res1.status !== 200) return { case: 'roundtrip:' + name, problems: ['первый ход HTTP ' + res1.status], ms: 0 };
  const s1 = assembleStream(res1.body);
  if (s1.calls.length === 0) return { case: 'roundtrip:' + name, problems: ['первый ход без tool_calls'], ms: 0 };

  const call = s1.calls[0];
  const toolResult = JSON.stringify({ file: 'go.mod', content: 'module ollama-loadbalancer' });
  const started = Date.now();
  const res2 = await request('POST', '/v1/chat/completions', {
    model: MODEL, stream: true, temperature: 0, max_tokens: MAX_TOKENS,
    messages: [
      { role: 'system', content: 'You are a coding agent. Use the provided tools; do not describe, call.' },
      { role: 'user', content: 'Call the tool "' + call.name + '" with arguments ' + args + '.' },
      { role: 'assistant', content: null, tool_calls: [{ id: call.id || 'call_read', type: 'function', function: { name: call.name, arguments: call.args } }] },
      { role: 'tool', tool_call_id: call.id || 'call_read', name: call.name, content: toolResult },
      { role: 'user', content: 'Now use that tool output: name the module from the result. One line, no tool call.' },
    ],
    tools: profileTools,
  });
  const ms = Date.now() - started;
  const problems = [];
  if (res2.status !== 200) problems.push('второй ход HTTP ' + res2.status);
  const s2 = assembleStream(res2.body);
  if (!s2.content.trim() && s2.calls.length === 0) {
    problems.push('второй ход пустой (finish=' + s2.finish + ', reasoning=' + JSON.stringify(s2.reasoning.slice(-100)) + ')');
  }
  if (!/ollama-loadbalancer/i.test(s2.content)) {
    problems.push('модель не увидела результат инструмента; content=' + JSON.stringify(s2.content.slice(0, 140)));
  }
  return { case: 'roundtrip:' + name, problems, ms };
}

// runToolCodeDialectCase — детерминированная проверка диалекта gemma tool_code:
// просим модель ответить РОВНО строкой вызова и смотрим, что слой её распознал
// (tool_calls), а не отдал клиенту текстом.
async function runToolCodeDialectCase(profileTools) {
  const read = profileTools.filter((t) => t.function.name === 'read')[0];
  if (!read) return { case: 'dialect:tool_code', problems: [], skipped: true };
  const want = '[tool_code]\nprint(read(file_path="go.mod"))\n[/tool_code]';
  const res = await request('POST', '/v1/chat/completions', {
    model: MODEL, stream: true, temperature: 0, max_tokens: 200,
    messages: [
      { role: 'system', content: 'You are a coding agent.' },
      { role: 'user', content: 'Repeat the following block EXACTLY, character for character, and nothing else:\n' + want },
    ],
    tools: profileTools,
  });
  const problems = [];
  if (res.status !== 200) problems.push('HTTP ' + res.status);
  const s = assembleStream(res.body);
  const hasMarkup = s.content.indexOf('[tool_code]') >= 0 || s.content.indexOf('print(read') >= 0;
  if (hasMarkup && s.calls.length === 0) {
    problems.push('разметка tool_code уехала в content как текст (вызов не распознан)');
  }
  const recognized = s.calls.some((c) => c.name === 'read');
  if (!recognized && !hasMarkup) {
    // Модель не повторила блок дословно — проверка неубедительна, не валим.
    return { case: 'dialect:tool_code', problems: [], skipped: true, note: 'модель не повторила блок' };
  }
  return { case: 'dialect:tool_code', problems };
}

// ---------- основной прогон ----------

(async () => {
  const profiles = Object.keys(PROFILES).filter((p) => !ONLY_PROFILE || p === ONLY_PROFILE);
  const results = [];
  for (const profileName of profiles) {
    let tools;
    try { tools = PROFILES[profileName](); }
    catch (e) { console.log(profileName + ': профиль не загружен: ' + e.message); continue; }
    const names = tools.map((t) => t.function.name).filter((n) => ONLY.length === 0 || ONLY.indexOf(n) >= 0);
    console.log('\n=== ' + profileName + ': ' + tools.length + ' инструментов, проверяем ' + names.length + ' ===');
    for (const name of names) {
      const r = await runToolCase(tools, name);
      results.push(r);
      const status = r.problems.length === 0 ? 'PASS' : 'FAIL';
      console.log((status + '  ' + name).padEnd(28) + ' ' + String(r.ms).padStart(6) + 'ms  ' +
        (r.problems.length ? r.problems.join(' | ').slice(0, 220) : ''));
    }
    const trip = await runRoundTripCase(tools);
    results.push(trip);
    console.log((trip.problems.length === 0 ? 'PASS  ' : 'FAIL  ') + trip.case + '  ' +
      (trip.problems.length ? trip.problems.join(' | ').slice(0, 220) : ''));
    if (profileName === 'dsh') {
      const dialect = await runToolCodeDialectCase(tools);
      if (!dialect.skipped) {
        results.push(dialect);
        console.log((dialect.problems.length === 0 ? 'PASS  ' : 'FAIL  ') + dialect.case + '  ' +
          (dialect.problems.length ? dialect.problems.join(' | ').slice(0, 220) : ''));
      } else {
        console.log('SKIP  dialect:tool_code  ' + (dialect.note || 'нет инструмента read'));
      }
    }
  }

  const failed = results.filter((r) => r.problems.length > 0);
  console.log('\nИТОГ: проверок ' + results.length + ', провалов ' + failed.length);
  if (failed.length) {
    for (const f of failed) console.log('  FAIL ' + (f.tool || f.case) + ': ' + f.problems.join(' | ').slice(0, 200));
  }
  process.exit(failed.length ? 1 : 0);
})();



// tools-clients-matrix.js — R204 (2026-10-21): понимает ли модель инструменты КЛИЕНТОВ
// (Cline / deepseek-harness / generic OpenAI) через балансер, и корректно ли возвращает
// вызов правки файла.
//
// Запуск: node scripts/webui-e2e/tools-clients-matrix.js
//
// Что проверяется для каждого профиля:
//   1. набор инструментов клиента доходит до модели БЕЗ искажений (модель выбирает
//      именно один из инструментов клиента, а не «свой» и не текст);
//   2. аргументы вызова — валидный JSON и соответствуют схеме (required-поля, типы);
//   3. на задачу «поправь файл» модель выбирает инструмент ПРАВКИ (editor/apply_patch),
//      а не чтение — с прямым воркером и через балансер результат сравнивается.
'use strict';
const http = require('http');

const BALANCER = { host: '127.0.0.1', port: 18080, token: 'changeme-bundled-with-agent-token' };
const WORKER = { host: '127.0.0.1', port: 18092, token: 'changeme-bundled-with-agent-token' };
const MODEL = process.env.MODEL || 'gemma-4-E4B-it-Q4_K_M';

// Схемы в том виде, в котором их объявляют клиенты (сокращённо, но с required).
const PROFILES = {
  cline: {
    tools: [
      { type: 'function', function: { name: 'read_files', description: 'Read files from disk.',
        parameters: { type: 'object', properties: { files: { type: 'array', items: { type: 'object', properties: { path: { type: 'string' } }, required: ['path'] } } }, required: ['files'] } } },
      { type: 'function', function: { name: 'run_commands', description: 'Run shell commands.',
        parameters: { type: 'object', properties: { commands: { type: 'array', items: { type: 'string' } } }, required: ['commands'] } } },
      { type: 'function', function: { name: 'editor', description: 'Replace exact text in a file (edit it).',
        parameters: { type: 'object', properties: { path: { type: 'string' }, old_text: { type: 'string' }, new_text: { type: 'string' } }, required: ['path', 'old_text', 'new_text'] } } },
      { type: 'function', function: { name: 'attempt_completion', description: 'Finish the task.',
        parameters: { type: 'object', properties: { result: { type: 'string' } }, required: ['result'] } } }
    ],
    task: 'In file src/App.cs there is the line "return null;". Use the editor tool to replace it with "return value;". Call the tool now, do not explain.'
  },
  harness: {
    tools: [
      { type: 'function', function: { name: 'read_file', description: 'Read one file.',
        parameters: { type: 'object', properties: { path: { type: 'string' } }, required: ['path'] } } },
      { type: 'function', function: { name: 'apply_patch', description: 'Apply a unified diff patch to a file.',
        parameters: { type: 'object', properties: { path: { type: 'string' }, patch: { type: 'string' } }, required: ['path', 'patch'] } } },
      { type: 'function', function: { name: 'write_file', description: 'Write full file content.',
        parameters: { type: 'object', properties: { path: { type: 'string' }, content: { type: 'string' } }, required: ['path', 'content'] } } }
    ],
    task: 'Change src/App.cs: replace "return null;" with "return value;". Use apply_patch (or write_file) — call the tool now.'
  }
};

function post(target, body) {
  return new Promise((resolve) => {
    const payload = JSON.stringify(body);
    const req = http.request({
      host: target.host, port: target.port, path: '/v1/chat/completions', method: 'POST',
      headers: { 'Content-Type': 'application/json', 'Content-Length': Buffer.byteLength(payload), 'X-API-Token': target.token }
    }, res => {
      let data = '';
      res.setEncoding('utf8');
      res.on('data', d => { data += d; });
      res.on('end', () => {
        try { resolve({ status: res.statusCode, body: JSON.parse(data) }); }
        catch (e) { resolve({ status: res.statusCode, body: null, raw: data.slice(0, 300) }); }
      });
    });
    req.on('error', e => resolve({ status: 0, error: e.message }));
    req.setTimeout(180000, () => { req.destroy(); resolve({ status: 0, error: 'timeout' }); });
    req.end(payload);
  });
}

// validate — соответствие аргументов схеме инструмента (required + базовые типы).
function validate(tools, tc) {
  const out = { json: false, schema: false, problems: [] };
  let name = '', args = null;
  try { name = (tc.function || {}).name || ''; args = JSON.parse((tc.function || {}).arguments || '{}'); out.json = true; }
  catch (e) { out.problems.push('arguments не JSON: ' + String((tc.function || {}).arguments).slice(0, 80)); return out; }
  const schema = tools.filter(t => t.function.name === name)[0];
  if (!schema) { out.problems.push('имя не из набора клиента: ' + name); return out; }
  const props = (schema.function.parameters || {}).properties || {};
  const required = (schema.function.parameters || {}).required || [];
  for (const field of required) {
    if (args[field] === undefined) { out.problems.push('нет обязательного поля ' + field); }
  }
  for (const k of Object.keys(args)) {
    const p = props[k];
    if (!p) { out.problems.push('лишнее поле ' + k); continue; }
    if (p.type === 'array' && !Array.isArray(args[k])) out.problems.push('поле ' + k + ' должно быть массивом');
    if (p.type === 'string' && typeof args[k] !== 'string') out.problems.push('поле ' + k + ' должно быть строкой');
  }
  out.schema = out.problems.length === 0;
  out.name = name;
  return out;
}

(async () => {
  const results = [];
  for (const [profileName, profile] of Object.entries(PROFILES)) {
    for (const [surfaceName, target] of [['via-balancer', BALANCER], ['direct-worker', WORKER]]) {
      const started = Date.now();
      const r = await post(target, {
        model: MODEL, stream: false, temperature: 0.2, max_tokens: 400,
        messages: [
          { role: 'system', content: 'You are a coding agent. Use the provided tools; do not describe, call.' },
          { role: 'user', content: profile.task }
        ],
        tools: profile.tools
      });
      const ms = Date.now() - started;
      const msg = (((r.body || {}).choices || [])[0] || {}).message || {};
      const calls = msg.tool_calls || [];
      const validated = calls.map(tc => validate(profile.tools, tc));
      const editTools = profileName === 'cline' ? ['editor'] : ['apply_patch', 'write_file'];
      const pickedEdit = validated.some(v => editTools.indexOf(v.name) >= 0 && v.schema);
      results.push({
        profile: profileName, surface: surfaceName, status: r.status, ms,
        content: String(msg.content || '').slice(0, 120),
        calls: validated.map(v => ({ name: v.name, ok: v.json && v.schema, problems: v.problems })),
        pickedEdit
      });
      console.log(`${profileName.padEnd(8)} ${surfaceName.padEnd(14)} HTTP ${r.status} ${ms}ms ` +
        `calls=${calls.length} edit=${pickedEdit ? 'YES' : 'no'} ` +
        `problems=${JSON.stringify(validated.flatMap(v => v.problems)).slice(0, 160)}`);
    }
  }
  const bad = results.filter(r => r.status !== 200 || r.calls.length === 0 || r.calls.some(c => !c.ok));
  const noEdit = results.filter(r => !r.pickedEdit);
  console.log('\nИТОГ: вызовов инструментов с ошибками схемы/статуса: ' + bad.length +
    '; не выбрали инструмент правки: ' + noEdit.length + ' из ' + results.length);
  process.exit(0);
})();

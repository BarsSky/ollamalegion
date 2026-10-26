// scripts/webui-e2e/clients-compat-r246.js — R246, диагностика: какие Open-API-клиенты переживут
// наш слой. Проверяем НЕ инструменты одной модели, а ФОРМУ запросов, которую
// шлют разные агентские клиенты (strict-схемы, вынужденный tool_choice,
// лишние поля, usage в стриме, content-части массивами, Ollama-нативный путь,
// параллельные вызовы, legacy functions/function_call).
'use strict';
const http = require('http');

const HOST = '127.0.0.1';
const PORT = parseInt(process.env.PORT || '18080', 10);
const OLLAMA_PORT = parseInt(process.env.OLLAMA_PORT || '18080', 10);
const TOKEN = process.env.API_TOKEN || 'changeme-bundled-with-agent-token';
const MODEL = process.env.MODEL || 'gemma-4-E4B-it-Q4_K_M';

function req(path, body, port) {
  return new Promise((resolve) => {
    const payload = JSON.stringify(body);
    const r = http.request({
      host: HOST, port: port || PORT, path, method: 'POST',
      headers: { 'Content-Type': 'application/json', 'X-API-Token': TOKEN, 'Content-Length': Buffer.byteLength(payload) },
    }, (res) => {
      const chunks = [];
      res.on('data', (c) => chunks.push(c));
      res.on('end', () => resolve({ status: res.statusCode, body: Buffer.concat(chunks).toString('utf8') }));
    });
    r.on('error', (e) => resolve({ status: 0, error: e.message, body: '' }));
    r.setTimeout(300000, () => { r.destroy(); resolve({ status: 0, error: 'timeout', body: '' }); });
    r.write(payload);
    r.end();
  });
}

function parseStream(raw) {
  const out = { content: '', calls: [], finish: null, usage: null, done: 0, bad: 0 };
  for (const line of raw.split('\n')) {
    const s = line.trim();
    if (!s.startsWith('data:')) continue;
    const d = s.slice(5).trim();
    if (d === '[DONE]') { out.done++; continue; }
    let j;
    try { j = JSON.parse(d); } catch (e) { out.bad++; continue; }
    if (j.usage) { out.usage = j.usage; continue; }
    const ch = (j.choices || [])[0];
    if (!ch) continue;
    if (typeof (ch.delta || {}).content === 'string') out.content += ch.delta.content;
    (ch.delta && ch.delta.tool_calls || []).forEach((tc) => {
      out.calls.push({ index: tc.index, id: tc.id, name: (tc.function || {}).name, args: (tc.function || {}).arguments });
    });
    if (ch.finish_reason) out.finish = ch.finish_reason;
  }
  return out;
}

const READ = { type: 'function', function: { name: 'read', description: 'Read a file.', parameters: { type: 'object', properties: { file_path: { type: 'string' } }, required: ['file_path'] } } };
const GREP = { type: 'function', function: { name: 'grep', description: 'Search file contents.', parameters: { type: 'object', properties: { pattern: { type: 'string' } }, required: ['pattern'] } } };

const results = [];
function report(name, ok, detail) {
  results.push({ name, ok, detail });
  console.log((ok ? 'PASS  ' : 'FAIL  ') + name.padEnd(24) + ' ' + detail);
}

(async () => {
  // A. Лишние поля, которые шлют разные клиенты.
  {
    const r = await req('/v1/chat/completions', {
      model: MODEL, stream: false, max_tokens: 16, temperature: 0,
      messages: [{ role: 'user', content: 'Say OK.' }],
      tools: [READ], tool_choice: 'auto',
      stream_options: { include_usage: true }, parallel_tool_calls: true,
      response_format: { type: 'text' }, frequency_penalty: 0, presence_penalty: 0,
      seed: 42, user: 'probe-user', metadata: { a: 1 }, reasoning_effort: 'low',
      top_p: 1, stop: ['\n\n'], n: 1, logprobs: false, service_tier: 'auto',
      store: false, max_completion_tokens: 16,
    });
    const ok = r.status === 200;
    report('extra_fields', ok, 'HTTP ' + r.status + (ok ? '' : ': ' + String(r.body).replace(/\s+/g, ' ').slice(0, 200)));
  }

  // B. Вынужденный tool_choice (forced function) — так делают плагины/фреймворки.
  // Бюджет 400: требование «вызови сейчас» gemma тратит часть токенов на
  // рассуждение перед вызовом; при 120 ответ обрывался по length без вызова
  // (проверено: с 400 — finish_reason=tool_calls, вызов read).
  {
    const r = await req('/v1/chat/completions', {
      model: MODEL, stream: true, max_tokens: 400, temperature: 0,
      messages: [{ role: 'user', content: 'Hello.' }],
      tools: [READ, GREP],
      tool_choice: { type: 'function', function: { name: 'read' } },
    });
    const s = parseStream(r.body);
    const ok = r.status === 200 && s.calls.some((c) => c.name === 'read');
    report('forced_tool_choice', ok, 'HTTP ' + r.status + ' finish=' + s.finish + ' calls=' + JSON.stringify(s.calls.map((c) => c.name)));
  }

  // C. strict-схема (additionalProperties:false, enum, вложенность) — OpenAI SDK.
  {
    const strictTool = {
      type: 'function',
      function: {
        name: 'set_mode', description: 'Set mode.', strict: true,
        parameters: {
          type: 'object', additionalProperties: false,
          properties: {
            mode: { type: 'string', enum: ['fast', 'safe'] },
            nested: { type: 'object', additionalProperties: false, properties: { level: { type: 'integer' } }, required: ['level'] },
          },
          required: ['mode'],
        },
      },
    };
    const r = await req('/v1/chat/completions', {
      model: MODEL, stream: true, max_tokens: 120, temperature: 0,
      messages: [{ role: 'user', content: 'Call the tool set_mode with mode="safe". Call only.' }],
      tools: [strictTool], tool_choice: 'auto',
    });
    const s = parseStream(r.body);
    const call = s.calls[0] || {};
    let args = null;
    try { args = JSON.parse(call.args || '{}'); } catch (e) { /* ignore */ }
    const problems = [];
    if (r.status !== 200) problems.push('HTTP ' + r.status);
    if (!call.name) problems.push('нет вызова');
    if (call.name && call.name !== 'set_mode') problems.push('выбран ' + call.name);
    if (args && args.mode !== 'safe' && !args.mode) problems.push('нет обязательного mode');
    if (args && args.mode && ['fast', 'safe'].indexOf(args.mode) < 0) problems.push('mode вне enum: ' + args.mode);
    report('strict_schema', problems.length === 0, problems.join(' | ') || ('args=' + JSON.stringify(args)));
  }

  // D. stream_options.include_usage — так считают токены Cline/OpenWebUI.
  {
    const r = await req('/v1/chat/completions', {
      model: MODEL, stream: true, max_tokens: 24, temperature: 0,
      messages: [{ role: 'user', content: 'Say OK.' }],
      stream_options: { include_usage: true },
    });
    const s = parseStream(r.body);
    const ok = r.status === 200 && !!s.usage && s.done === 1 && s.bad === 0;
    report('stream_usage', ok, 'usage=' + JSON.stringify(s.usage) + ' done=' + s.done + ' bad=' + s.bad);
  }

  // E. content-части массивами (мультимодальная запись) во всех ролях.
  {
    const r = await req('/v1/chat/completions', {
      model: MODEL, stream: true, max_tokens: 120, temperature: 0,
      messages: [
        { role: 'system', content: [{ type: 'text', text: 'You are a coding agent.' }] },
        { role: 'user', content: [{ type: 'text', text: 'Read go.mod.' }] },
        { role: 'assistant', content: null, tool_calls: [{ id: 'call_read', type: 'function', function: { name: 'read', arguments: '{"file_path":"go.mod"}' } }] },
        { role: 'tool', tool_call_id: 'call_read', name: 'read', content: [{ type: 'text', text: '1: module ollama-loadbalancer' }] },
        { role: 'user', content: [{ type: 'text', text: 'Name the module from the tool result. One line.' }] },
      ],
      tools: [READ],
    });
    const s = parseStream(r.body);
    const ok = r.status === 200 && /ollama-loadbalancer/i.test(s.content);
    report('content_parts', ok, 'HTTP ' + r.status + ' content=' + JSON.stringify(s.content.slice(0, 90)));
  }

  // F. Ollama-нативный путь (/api/chat) с tools и вторым ходом.
  {
    const r1 = await req('/api/chat', {
      model: MODEL, stream: false,
      messages: [{ role: 'user', content: 'Read go.mod. Call the read tool.' }],
      tools: [READ, GREP],
      options: { num_predict: 120, temperature: 0 },
    });
    let first = null;
    try { first = JSON.parse(r1.body); } catch (e) { /* ignore */ }
    const calls = (first && first.message && first.message.tool_calls) || [];
    const call = calls[0] || {};
    const callName = (call.function && call.function.name) || '';
    const rawArgs = call.function ? call.function.arguments : undefined;
    const argsShape = typeof rawArgs === 'object' && rawArgs !== null ? 'object' : typeof rawArgs;
    let ok = r1.status === 200 && callName === 'read';
    let detail = 'HTTP ' + r1.status + ' name=' + callName + ' args=' + argsShape;
    if (ok) {
      const r2 = await req('/api/chat', {
        model: MODEL, stream: false,
        messages: [
          { role: 'user', content: 'Read go.mod.' },
          { role: 'assistant', content: '', tool_calls: [call] },
          { role: 'tool', tool_name: 'read', content: '1: module ollama-loadbalancer' },
          { role: 'user', content: 'Name the module from the tool result. One line.' },
        ],
        tools: [READ],
        options: { num_predict: 120, temperature: 0 },
      });
      let second = null;
      try { second = JSON.parse(r2.body); } catch (e) { /* ignore */ }
      const text = (second && second.message && second.message.content) || '';
      ok = r2.status === 200 && /ollama-loadbalancer/i.test(text);
      detail += ' | turn2 HTTP ' + r2.status + ' content=' + JSON.stringify(text.slice(0, 80));
    }
    report('ollama_native', ok, detail);
  }

  // G. Legacy functions/function_call (старые плагины).
  {
    const r = await req('/v1/chat/completions', {
      model: MODEL, stream: false, max_tokens: 16, temperature: 0,
      messages: [{ role: 'user', content: 'Say OK.' }],
      functions: [{ name: 'read', description: 'Read a file.', parameters: { type: 'object', properties: { file_path: { type: 'string' } } } }],
      function_call: 'auto',
    });
    report('legacy_functions', r.status === 200, 'HTTP ' + r.status + (r.status === 200 ? ' (принято, вызов не ожидался)' : ': ' + String(r.body).replace(/\s+/g, ' ').slice(0, 160)));
  }

  // H. Параллельные вызовы: два инструмента в одном ответе.
  {
    const r = await req('/v1/chat/completions', {
      model: MODEL, stream: true, max_tokens: 220, temperature: 0,
      messages: [{ role: 'user', content: 'Read TWO files with ONE answer: call read twice — go.mod and README.md. Do not explain.' }],
      tools: [READ],
      parallel_tool_calls: true,
    });
    const s = parseStream(r.body);
    const problems = [];
    if (r.status !== 200) problems.push('HTTP ' + r.status);
    if (s.calls.length === 0) problems.push('вызовов нет');
    const ids = new Set(s.calls.map((c) => c.id).filter(Boolean));
    const idx = new Set(s.calls.map((c) => c.index));
    if (s.calls.length > 1 && ids.size !== s.calls.length) problems.push('id не уникальны: ' + JSON.stringify(s.calls.map((c) => c.id)));
    if (s.calls.length > 1 && idx.size !== s.calls.length) problems.push('index не уникальны: ' + JSON.stringify(s.calls.map((c) => c.index)));
    if (s.calls.some((c) => c.index === undefined)) problems.push('нет index у фрагмента');
    report('parallel_calls', problems.length === 0, 'calls=' + s.calls.length + ' ' + (problems.join(' | ') || 'ok'));
  }

  const failed = results.filter((r) => !r.ok);
  console.log('\nИТОГ: проверок ' + results.length + ', провалов ' + failed.length);
  process.exit(failed.length ? 1 : 0);
})();


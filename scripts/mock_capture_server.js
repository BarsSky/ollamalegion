#!/usr/bin/env node
/*
 * mock_capture_server.js — ловушка для payload'ов LLM-клиентов (Cline и др.).
 *
 * Зачем: чтобы понять, ЧТО именно клиент шлёт в cppworker (Ollama-native
 * /api/chat или OpenAI /v1/chat/completions), не гоняя каждый раз реальную
 * модель. Сервер логирует полные тела запросов в файл и отвечает валидным
 * ответом с tool_call, чтобы клиент продолжил цикл (и прислал второй запрос
 * с результатом инструмента).
 *
 * Запуск:
 *   node scripts/mock_capture_server.js [port] [logFile]
 * По умолчанию: port=18999, logFile=_diag/cline-capture.log
 */

const http = require('http');
const fs = require('fs');
const path = require('path');

const port = parseInt(process.argv[2] || '18999', 10);
const logFile = process.argv[3] || path.join(__dirname, '..', '_diag', 'cline-capture.log');

fs.mkdirSync(path.dirname(logFile), { recursive: true });
fs.writeFileSync(logFile, '');

let chatCall = 0;

function log(obj) {
  fs.appendFileSync(logFile, JSON.stringify(obj, null, 2) + '\n' + '-'.repeat(80) + '\n');
}

function readBody(req) {
  return new Promise((resolve) => {
    let data = '';
    req.on('data', (c) => (data += c));
    req.on('end', () => resolve(data));
  });
}

const TOOL_CALL_OLLAMA = {
  function: {
    name: 'execute_command',
    arguments: { command: 'echo MOCK_TOOL_OK' },
  },
};

const TOOL_CALL_OPENAI = {
  id: 'call_mock_1',
  type: 'function',
  function: {
    name: 'execute_command',
    arguments: JSON.stringify({ command: 'echo MOCK_TOOL_OK' }),
  },
};

const server = http.createServer(async (req, res) => {
  const body = await readBody(req);
  let parsed = null;
  try { parsed = JSON.parse(body); } catch (e) { /* keep raw */ }

  log({
    method: req.method,
    url: req.url,
    headers: {
      'content-type': req.headers['content-type'],
      authorization: req.headers['authorization'] ? '<set>' : undefined,
      'x-api-token': req.headers['x-api-token'] ? '<set>' : undefined,
      'user-agent': req.headers['user-agent'],
    },
    bodyRaw: body.length > 20000 ? body.slice(0, 20000) + '...[truncated]' : body,
    bodyParsed: parsed,
    topLevelKeys: parsed && typeof parsed === 'object' && !Array.isArray(parsed) ? Object.keys(parsed) : null,
  });

  const json = (code, obj) => {
    const payload = JSON.stringify(obj);
    res.writeHead(code, { 'Content-Type': 'application/json', 'Content-Length': Buffer.byteLength(payload) });
    res.end(payload);
  };

  if (req.method === 'GET' && req.url.startsWith('/api/tags')) {
    return json(200, {
      models: [{
        name: 'mock-model', model: 'mock-model',
        modified_at: new Date().toISOString(), size: 1000, digest: 'sha256:mock',
        details: { family: 'mock', format: 'gguf', parameter_size: '1B', quantization_level: 'Q4_K_M' },
      }],
    });
  }
  if (req.method === 'GET' && req.url.startsWith('/api/version')) return json(200, { version: '0.0.0-mock' });
  if (req.method === 'GET' && req.url.startsWith('/api/ps')) return json(200, { models: [] });
  if (req.method === 'GET' && req.url.startsWith('/api/show')) {
    return json(200, { details: { family: 'mock', format: 'gguf' }, model_info: {}, capabilities: ['tools'] });
  }
  if (req.method === 'GET' && (req.url.startsWith('/v1/models') || req.url.startsWith('/api/models'))) {
    return json(200, { object: 'list', data: [{ id: 'mock-model', object: 'model', owned_by: 'mock' }] });
  }

  if (req.method === 'POST' && req.url.startsWith('/api/chat')) {
    chatCall++;
    const useTool = chatCall === 1;
    const stream = parsed && parsed.stream !== false;
    if (stream) {
      // NDJSON-поток: сначала tool_call, затем done.
      res.writeHead(200, { 'Content-Type': 'application/x-ndjson' });
      const base = { model: (parsed && parsed.model) || 'mock-model', created_at: new Date().toISOString() };
      if (useTool) {
        res.write(JSON.stringify({ ...base, message: { role: 'assistant', content: '', tool_calls: [TOOL_CALL_OLLAMA] }, done: false }) + '\n');
      } else {
        res.write(JSON.stringify({ ...base, message: { role: 'assistant', content: 'MOCK_FINAL_ANSWER' }, done: false }) + '\n');
      }
      res.write(JSON.stringify({ ...base, message: { role: 'assistant', content: '' }, done: true, done_reason: 'stop' }) + '\n');
      return res.end();
    }
    if (useTool) {
      return json(200, { model: (parsed && parsed.model) || 'mock-model', created_at: new Date().toISOString(), message: { role: 'assistant', content: '', tool_calls: [TOOL_CALL_OLLAMA] }, done: true, done_reason: 'stop' });
    }
    return json(200, { model: (parsed && parsed.model) || 'mock-model', created_at: new Date().toISOString(), message: { role: 'assistant', content: 'MOCK_FINAL_ANSWER' }, done: true, done_reason: 'stop' });
  }

  if (req.method === 'POST' && req.url.startsWith('/v1/chat/completions')) {
    chatCall++;
    const useTool = chatCall === 1;
    const stream = parsed && parsed.stream === true;
    const mk = (msg, finish) => ({
      id: 'chatcmpl-mock', object: 'chat.completion.chunk', created: Math.floor(Date.now() / 1000),
      model: (parsed && parsed.model) || 'mock-model', choices: [{ index: 0, delta: msg, finish_reason: finish }],
    });
    if (stream) {
      res.writeHead(200, { 'Content-Type': 'text/event-stream' });
      if (useTool) {
        res.write('data: ' + JSON.stringify(mk({ role: 'assistant', tool_calls: [{ index: 0, ...TOOL_CALL_OPENAI }] }, null)) + '\n\n');
        res.write('data: ' + JSON.stringify(mk({}, 'tool_calls')) + '\n\n');
      } else {
        res.write('data: ' + JSON.stringify(mk({ role: 'assistant', content: 'MOCK_FINAL_ANSWER' }, null)) + '\n\n');
        res.write('data: ' + JSON.stringify(mk({}, 'stop')) + '\n\n');
      }
      res.write('data: [DONE]\n\n');
      return res.end();
    }
    return json(200, {
      id: 'chatcmpl-mock', object: 'chat.completion', created: Math.floor(Date.now() / 1000),
      model: (parsed && parsed.model) || 'mock-model',
      choices: [{ index: 0, finish_reason: useTool ? 'tool_calls' : 'stop', message: useTool ? { role: 'assistant', content: null, tool_calls: [TOOL_CALL_OPENAI] } : { role: 'assistant', content: 'MOCK_FINAL_ANSWER' } }],
      usage: { prompt_tokens: 1, completion_tokens: 1, total_tokens: 2 },
    });
  }

  return json(200, { ok: true, mock: true, note: 'unhandled route captured' });
});

server.listen(port, () => {
  console.log(`[mock] listening on http://127.0.0.1:${port} (all interfaces, log: ${logFile})`);
});

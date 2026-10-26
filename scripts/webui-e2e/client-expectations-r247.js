// scripts/webui-e2e/client-expectations-r247.js — R247, этап E: КОНТУР ОЖИДАНИЙ
// КЛИЕНТОВ.
//
// Отличие от tools-all-clients-r246.js и clients-compat-r246.js:
//   - r246-матрица проверяет, что ИНСТРУМЕНТ вызван и аргументы валидны;
//   - compat-r246 проверяет, что ФОРМА запроса принята;
//   - ЭТОТ пробник проверяет, что ОТВЕТ модели соответствует ОЖИДАНИЮ конкретного
//     клиента: обязательные поля и типы, порядок/терминаторы, чего быть не должно,
//     и что второй ход (assistant tool_calls + результат) доводит до ответа.
//
// Профиль — декларация «что клиент считает правильным ответом». Провал печатает
// профиль, правило и фактическое значение, чтобы диагноз был без догадок.
//
// Запуск:
//   node scripts/webui-e2e/client-expectations-r247.js            # все профили
//   ONLY=cline-openai-stream node scripts/webui-e2e/client-expectations-r247.js
//
// Профили Anthropic (/v1/messages) и Responses (/v1/responses) присутствуют как
// SKIP с причиной: их поверхности ещё не реализованы (этапы B и C плана R247).
'use strict';
const http = require('http');

const HOST = '127.0.0.1';
const PORT = parseInt(process.env.PORT || '18080', 10);
const TOKEN = process.env.API_TOKEN || 'changeme-bundled-with-agent-token';
const MODEL = process.env.MODEL || 'gemma-4-E4B-it-Q4_K_M';
const ONLY = (process.env.ONLY || '').split(',').filter(Boolean);
const MAX_TOKENS = parseInt(process.env.MAX_TOKENS || '400', 10);

// ---------- транспорт ----------

function post(path, body, port) {
  return new Promise((resolve) => {
    const payload = JSON.stringify(body);
    const req = http.request({
      host: HOST, port: port || PORT, path, method: 'POST',
      headers: { 'Content-Type': 'application/json', 'X-API-Token': TOKEN, 'Content-Length': Buffer.byteLength(payload) },
    }, (res) => {
      const chunks = [];
      res.on('data', (c) => chunks.push(c));
      res.on('end', () => resolve({
        status: res.statusCode,
        contentType: String(res.headers['content-type'] || ''),
        headers: res.headers,
        body: Buffer.concat(chunks).toString('utf8'),
      }));
    });
    req.on('error', (e) => resolve({ status: 0, contentType: '', body: '', error: e.message }));
    req.setTimeout(600000, () => { req.destroy(); resolve({ status: 0, contentType: '', body: '', error: 'timeout' }); });
    req.write(payload);
    req.end();
  });
}

// ---------- сборка потока «как клиент» ----------

function assembleSSE(raw) {
  const out = { chunks: [], content: '', reasoning: '', calls: [], finish: null, done: 0, bad: 0, firstRole: null };
  const byIndex = new Map();
  for (const line of raw.split('\n')) {
    const s = line.trim();
    if (!s || !s.startsWith('data:')) continue;
    const data = s.slice(5).trim();
    if (data === '[DONE]') { out.done++; continue; }
    let json;
    try { json = JSON.parse(data); } catch (e) { out.bad++; continue; }
    if (json.usage) { out.usage = json.usage; continue; }
    const choice = (json.choices || [])[0];
    if (!choice) continue;
    out.chunks.push(json);
    const delta = choice.delta || {};
    if (out.firstRole === null && delta.role) out.firstRole = delta.role;
    if (typeof delta.content === 'string') out.content += delta.content;
    if (typeof delta.reasoning_content === 'string') out.reasoning += delta.reasoning_content;
    for (const tc of delta.tool_calls || []) {
      const key = typeof tc.index === 'number' ? 'i' + tc.index : 'id' + (tc.id || '');
      let acc = byIndex.get(key);
      if (!acc) { acc = { index: tc.index, id: '', name: '', args: '' }; byIndex.set(key, acc); }
      if (tc.id) acc.id = tc.id;
      const fn = tc.function || {};
      if (fn.name) acc.name += fn.name;
      if (typeof fn.arguments === 'string') acc.args += fn.arguments;
      else if (fn.arguments) acc.args += JSON.stringify(fn.arguments);
      out.calls = Array.from(byIndex.values());
    }
    if (choice.finish_reason) out.finish = choice.finish_reason;
  }
  return out;
}

// assembleNDJSON — сборка Ollama NDJSON (см. ниже).
function parseResponsesSSE(raw) {
  const out = { list: [], bad: 0 };
  for (const block of raw.split('\n\n')) {
    const lines = block.split('\n').map((l) => l.trim()).filter(Boolean);
    const dataLine = lines.filter((l) => l.startsWith('data:')).pop();
    if (!dataLine) continue;
    const payload = dataLine.slice(5).trim();
    if (!payload) continue;
    let j;
    try { j = JSON.parse(payload); } catch (e) { out.bad++; continue; }
    const eventName = (lines.filter((l) => l.startsWith('event:')).pop() || '').slice(6).trim();
    if (!j.type && eventName) j.type = eventName;
    out.list.push(j);
  }
  return out;
}

function parseAnthropicSSE(raw) {
  const out = { list: [], bad: 0 };
  // Формат: "event: <name>\ndata: <json>\n\n" (бывают комментарии/ping).
  for (const block of raw.split('\n\n')) {
    const lines = block.split('\n').map((l) => l.trim()).filter(Boolean);
    const dataLine = lines.filter((l) => l.startsWith('data:')).pop();
    if (!dataLine) continue;
    const payload = dataLine.slice(5).trim();
    if (!payload) continue;
    let j;
    try { j = JSON.parse(payload); } catch (e) { out.bad++; continue; }
    const eventName = (lines.filter((l) => l.startsWith('event:')).pop() || '').slice(6).trim();
    if (eventName && !j.type) j.type = eventName;
    out.list.push(j);
  }
  return out;
}

function assembleNDJSON(raw) {
  const out = { objects: [], content: '', calls: [], done: false, doneReason: null, thinking: '', bad: 0 };
  // Ollama-поток может присылать tool_calls ФРАГМЕНТАМИ (имя в одном чанке,
  // аргументы в следующем) — склеиваем по позиции в массиве, как это делает
  // клиент, а не конкатенируем массивы (иначе в историю уйдут обрывки).
  const byPos = new Map();
  for (const line of raw.split('\n')) {
    const s = line.trim();
    if (!s) continue;
    let j;
    try { j = JSON.parse(s); } catch (e) { out.bad++; continue; }
    out.objects.push(j);
    if (j.error) { out.error = j.error; continue; }
    const m = j.message || {};
    if (typeof m.content === 'string') out.content += m.content;
    if (typeof m.thinking === 'string') out.thinking += m.thinking;
    if (Array.isArray(m.tool_calls) && m.tool_calls.length) {
      m.tool_calls.forEach((tc, i) => {
        let acc = byPos.get(i);
        if (!acc) { acc = { function: { name: '', arguments: {} } }; byPos.set(i, acc); }
        const fn = tc.function || {};
        if (fn.name) acc.function.name = fn.name;
        if (fn.arguments && typeof fn.arguments === 'object') acc.function.arguments = fn.arguments;
        else if (typeof fn.arguments === 'string' && fn.arguments) {
          acc.function.arguments = (typeof acc.function.arguments === 'string' ? acc.function.arguments : '') + fn.arguments;
        }
        if (tc.id) acc.id = tc.id;
      });
      out.calls = Array.from(byPos.values());
    }
    if (j.done) { out.done = true; out.doneReason = j.done_reason || null; }
  }
  return out;
}

// ---------- проверки ----------

const MARKUP_LEAKS = ['[tool_code]', '<|tool_call>', '<tool_call>', '<function=', '[TOOL_CALLS]', '<|python_tag|>'];

function checkProfile(profile) {
  const problems = [];
  const say = (rule, detail) => problems.push(rule + (detail ? ' :: ' + detail : ''));
  return { problems, say };
}

const READ = {
  type: 'function',
  function: { name: 'read', description: 'Read a file.', parameters: { type: 'object', properties: { file_path: { type: 'string' } }, required: ['file_path'] } },
};

const STRICT_TOOL = {
  type: 'function',
  function: {
    name: 'set_mode', description: 'Set mode.', strict: true,
    parameters: {
      type: 'object', additionalProperties: false,
      properties: { mode: { type: 'string', enum: ['fast', 'safe'] }, level: { type: 'integer' } },
      required: ['mode'],
    },
  },
};

const SECOND_TURN_USER = 'Use the tool output above: name the module. One line, no tool call.';

// ---------- профили ----------

const PROFILES = [
  {
    id: 'cline-openai-stream',
    client: 'Cline 3.x / Roo / Kilo (OpenAI provider)',
    surface: 'POST /v1/chat/completions (stream)',
    async run() {
      const { problems, say } = checkProfile();
      const r = await post('/v1/chat/completions', {
        model: MODEL, stream: true, temperature: 0, max_tokens: MAX_TOKENS,
        messages: [{ role: 'user', content: 'Read the file go.mod. Call the read tool now, no prose.' }],
        tools: [READ],
      });
      if (r.status !== 200) { say('status 200', 'получено ' + r.status); return problems; }
      if (!/text\/event-stream/.test(r.contentType)) say('content-type SSE', r.contentType);
      const s = assembleSSE(r.body);
      if (s.bad > 0) say('валидный JSON в каждом data-чанке', 'битых: ' + s.bad);
      if (s.done !== 1) say('ровно один [DONE]', 'получено ' + s.done);
      if (s.firstRole !== 'assistant') say('первый delta.role=assistant', String(s.firstRole));
      if (s.calls.length === 0) { say('tool_calls присутствуют', 'нет вызовов'); return problems; }
      s.calls.forEach((c, i) => {
        if (typeof c.index !== 'number') say(`tool_calls[${i}].index`, 'отсутствует (клиент не склеит фрагменты)');
        if (!c.id) say(`tool_calls[${i}].id`, 'пустой');
        if (!c.name) say(`tool_calls[${i}].function.name`, 'пустое');
        if (typeof c.args !== 'string' || c.args.trim() === '') say(`tool_calls[${i}].function.arguments`, 'не строка/пусто: ' + JSON.stringify(c.args));
        try { JSON.parse(c.args || '{}'); } catch (e) { say(`tool_calls[${i}].arguments JSON`, String(c.args).slice(0, 80)); }
      });
      if (s.finish !== 'tool_calls') say('finish_reason=tool_calls', String(s.finish));
      for (const marker of MARKUP_LEAKS) if (s.content.includes(marker)) say('без разметки вызова в content', marker);
      if (s.reasoning && s.content.includes(s.reasoning.slice(0, 40))) say('reasoning не в content', 'текст размышлений попал в content');
      // второй ход: клиент возвращает assistant(tool_calls) + результат
      const c = s.calls[0];
      const r2 = await post('/v1/chat/completions', {
        model: MODEL, stream: true, temperature: 0, max_tokens: MAX_TOKENS,
        messages: [
          { role: 'user', content: 'Read the file go.mod.' },
          { role: 'assistant', content: null, tool_calls: [{ id: c.id, type: 'function', function: { name: c.name, arguments: c.args } }] },
          { role: 'tool', tool_call_id: c.id, name: c.name, content: '1: module ollama-loadbalancer' },
          { role: 'user', content: SECOND_TURN_USER },
        ],
        tools: [READ],
      });
      const s2 = assembleSSE(r2.body);
      if (!/ollama-loadbalancer/i.test(s2.content)) say('второй ход видит результат', 'content=' + JSON.stringify(s2.content.slice(0, 100)));
      return problems;
    },
  },
  {
    id: 'roo-openai-nonstream',
    client: 'Roo Code / Kilo (non-stream provider)',
    surface: 'POST /v1/chat/completions (stream=false)',
    async run() {
      const { problems, say } = checkProfile();
      const r = await post('/v1/chat/completions', {
        model: MODEL, stream: false, temperature: 0, max_tokens: MAX_TOKENS,
        messages: [{ role: 'user', content: 'Read go.mod with the read tool. Call it only.' }],
        tools: [READ],
      });
      if (r.status !== 200) { say('status 200', String(r.status)); return problems; }
      let j;
      try { j = JSON.parse(r.body); } catch (e) { say('ответ — JSON', String(r.body).slice(0, 100)); return problems; }
      if (j.object !== 'chat.completion') say('object=chat.completion', String(j.object));
      const ch = (j.choices || [])[0] || {};
      const msg = ch.message || {};
      if (ch.finish_reason !== 'tool_calls') say('finish_reason=tool_calls', String(ch.finish_reason));
      const calls = msg.tool_calls || [];
      if (!calls.length) { say('message.tool_calls', 'нет вызовов; content=' + JSON.stringify(String(msg.content || '').slice(0, 100))); return problems; }
      calls.forEach((c, i) => {
        if (!c.id) say(`tool_calls[${i}].id`, 'пустой');
        if (c.type !== 'function') say(`tool_calls[${i}].type=function`, String(c.type));
        if (!(c.function || {}).name) say(`tool_calls[${i}].function.name`, 'пустое');
        if (typeof (c.function || {}).arguments !== 'string') say(`tool_calls[${i}].arguments — СТРОКА`, typeof (c.function || {}).arguments);
      });
      if (j.usage && (!j.usage.total_tokens || j.usage.total_tokens < j.usage.completion_tokens)) say('usage.total_tokens согласован', JSON.stringify(j.usage));
      return problems;
    },
  },
  {
    id: 'dsh-openai-stream',
    client: 'DeepSeek Harness',
    surface: 'POST /v1/chat/completions (stream, 25 tools)',
    async run() {
      const { problems, say } = checkProfile();
      const tools = JSON.parse(require('fs').readFileSync(require('path').join(__dirname, 'dsh-tools-r246.json'), 'utf8'));
      const r = await post('/v1/chat/completions', {
        model: MODEL, stream: true, temperature: 0, max_tokens: MAX_TOKENS,
        messages: [{ role: 'user', content: 'Read the file go.mod with the read tool. Do not explain.' }],
        tools,
      });
      if (r.status !== 200) { say('status 200', String(r.status)); return problems; }
      const s = assembleSSE(r.body);
      if (s.bad > 0) say('валидный JSON чанков', String(s.bad));
      if (s.done !== 1) say('ровно один [DONE]', String(s.done));
      if (s.calls.length === 0) say('есть вызов', 'нет вызовов');
      for (const marker of MARKUP_LEAKS) if (s.content.includes(marker)) say('без разметки в content', marker);
      if (s.reasoning && s.content.includes(s.reasoning.slice(0, 40))) say('reasoning отдельным полем, не в content', 'утечка');
      // контракт второго хода: assistant(tool_calls)+tool → модель называет модуль
      if (s.calls.length) {
        const c = s.calls[0];
        const r2 = await post('/v1/chat/completions', {
          model: MODEL, stream: true, temperature: 0, max_tokens: MAX_TOKENS,
          messages: [
            { role: 'user', content: 'Read go.mod.' },
            { role: 'assistant', content: null, tool_calls: [{ id: c.id, type: 'function', function: { name: c.name, arguments: c.args } }] },
            { role: 'tool', tool_call_id: c.id, name: c.name, content: '1: module ollama-loadbalancer' },
            { role: 'user', content: SECOND_TURN_USER },
          ],
          tools,
        });
        const s2 = assembleSSE(r2.body);
        if (!/ollama-loadbalancer/i.test(s2.content)) say('второй ход видит результат', 'content=' + JSON.stringify(s2.content.slice(0, 100)));
      }
      return problems;
    },
  },
  {
    id: 'openwebui-ollama-ndjson',
    client: 'Open WebUI (нативный Ollama /api/chat)',
    surface: 'POST /api/chat (stream)',
    async run() {
      const { problems, say } = checkProfile();
      const r = await post('/api/chat', {
        model: MODEL, stream: true,
        messages: [{ role: 'user', content: 'Read go.mod with the read tool. Call it only.' }],
        tools: [READ],
        options: { temperature: 0, num_predict: MAX_TOKENS },
      });
      if (r.status !== 200) { say('status 200', String(r.status)); return problems; }
      const s = assembleNDJSON(r.body);
      if (s.bad > 0) say('валидный NDJSON в каждой строке', String(s.bad));
      if (!s.done) say('финальный объект done=true', 'нет');
      if (s.calls.length === 0) { say('message.tool_calls', 'нет вызовов; content=' + JSON.stringify(s.content.slice(0, 100))); return problems; }
      s.calls.forEach((c, i) => {
        const fn = c.function || {};
        if (!fn.name) say(`tool_calls[${i}].function.name`, 'пустое');
        if (typeof fn.arguments !== 'object' || fn.arguments === null) {
          say(`tool_calls[${i}].arguments — ОБЪЕКТ (Ollama-форма)`, typeof fn.arguments);
        }
      });
      // второй ход с tool_name (как шлёт Open WebUI)
      const r2 = await post('/api/chat', {
        model: MODEL, stream: true,
        messages: [
          { role: 'user', content: 'Read go.mod.' },
          { role: 'assistant', content: '', tool_calls: s.calls },
          { role: 'tool', tool_name: (s.calls[0].function || {}).name, content: '1: module ollama-loadbalancer' },
          { role: 'user', content: SECOND_TURN_USER },
        ],
        tools: [READ],
        options: { temperature: 0, num_predict: MAX_TOKENS },
      });
      const s2 = assembleNDJSON(r2.body);
      if (!/ollama-loadbalancer/i.test(s2.content)) say('второй ход видит результат', 'content=' + JSON.stringify(s2.content.slice(0, 100)));
      return problems;
    },
  },
  {
    id: 'openwebui-openai-usage',
    client: 'Open WebUI (OpenAI mode) / Cline (usage tracking)',
    surface: 'POST /v1/chat/completions + stream_options.include_usage',
    async run() {
      const { problems, say } = checkProfile();
      const r = await post('/v1/chat/completions', {
        model: MODEL, stream: true, temperature: 0, max_tokens: 32,
        messages: [{ role: 'user', content: 'Say OK.' }],
        stream_options: { include_usage: true },
      });
      if (r.status !== 200) { say('status 200', String(r.status)); return problems; }
      const s = assembleSSE(r.body);
      if (s.done !== 1) say('ровно один [DONE]', String(s.done));
      if (!s.usage) { say('usage-чанк присутствует', 'нет'); return problems; }
      for (const key of ['prompt_tokens', 'completion_tokens', 'total_tokens']) {
        if (typeof s.usage[key] !== 'number') say('usage.' + key, typeof s.usage[key]);
      }
      if (s.usage.total_tokens !== s.usage.prompt_tokens + s.usage.completion_tokens) {
        say('usage.total = prompt + completion', JSON.stringify(s.usage));
      }
      return problems;
    },
  },
  {
    id: 'continue-langchain-strict',
    client: 'Continue.dev / LangChain structured output (strict:true)',
    surface: 'POST /v1/chat/completions, strict-схема',
    async run() {
      const { problems, say } = checkProfile();
      const r = await post('/v1/chat/completions', {
        model: MODEL, stream: true, temperature: 0, max_tokens: MAX_TOKENS,
        messages: [{ role: 'user', content: 'Call the tool set_mode with mode="safe". Call it only.' }],
        tools: [STRICT_TOOL],
      });
      if (r.status !== 200) { say('status 200', String(r.status)); return problems; }
      const s = assembleSSE(r.body);
      if (!s.calls.length) { say('вызов пришёл', 'нет'); return problems; }
      const call = s.calls[0];
      if (call.name !== 'set_mode') say('выбран set_mode', call.name);
      let args = null;
      try { args = JSON.parse(call.args || '{}'); } catch (e) { say('arguments — валидный JSON', String(call.args).slice(0, 80)); return problems; }
      for (const key of Object.keys(args)) {
        if (!['mode', 'level'].includes(key)) say('additionalProperties=false: лишнее поле', key);
      }
      if (args.mode === undefined) say('required: mode', 'нет');
      else if (!['fast', 'safe'].includes(args.mode)) say('enum mode', String(args.mode));
      if (args.level !== undefined && !Number.isInteger(args.level)) say('level — integer', typeof args.level);
      return problems;
    },
  },
  {
    id: 'langchain-forced-tool',
    client: 'LangChain / MCP-мост (tool_choice = function)',
    surface: 'POST /v1/chat/completions, forced tool_choice',
    async run() {
      const { problems, say } = checkProfile();
      const r = await post('/v1/chat/completions', {
        model: MODEL, stream: true, temperature: 0, max_tokens: MAX_TOKENS,
        messages: [{ role: 'user', content: 'Hello.' }],
        tools: [READ],
        tool_choice: { type: 'function', function: { name: 'read' } },
      });
      if (r.status !== 200) { say('status 200', String(r.status)); return problems; }
      const s = assembleSSE(r.body);
      if (!s.calls.length) { say('вынужденный вызов исполнен', 'вызовов нет (finish=' + s.finish + ')'); return problems; }
      if (s.calls[0].name !== 'read') say('вызван именно read', s.calls[0].name);
      if (s.finish !== 'tool_calls') say('finish_reason=tool_calls', String(s.finish));
      return problems;
    },
  },
  {
    id: 'legacy-functions',
    client: 'старые плагины (functions/function_call)',
    surface: 'POST /v1/chat/completions, legacy поля',
    async run() {
      const { problems, say } = checkProfile();
      const r = await post('/v1/chat/completions', {
        model: MODEL, stream: false, temperature: 0, max_tokens: 24,
        messages: [{ role: 'user', content: 'Say OK.' }],
        functions: [{ name: 'read', description: 'Read.', parameters: { type: 'object', properties: { file_path: { type: 'string' } } } }],
        function_call: 'auto',
      });
      if (r.status !== 200) { say('status 200 (легаси-поля не отбиваются)', String(r.status)); return problems; }
      let j;
      try { j = JSON.parse(r.body); } catch (e) { say('ответ — JSON', String(r.body).slice(0, 80)); return problems; }
      const ch = (j.choices || [])[0] || {};
      if (!ch.message) say('choices[0].message присутствует', 'нет');
      return problems;
    },
  },
  {
    id: 'anthropic-sdk',
    client: 'Anthropic SDK / Claude Code',
    surface: 'POST /v1/messages (stream)',
    async run() {
      const { problems, say } = checkProfile();
      const tool = {
        name: 'read', description: 'Read a file.',
        input_schema: { type: 'object', properties: { file_path: { type: 'string' } }, required: ['file_path'] },
      };
      const r = await post('/v1/messages', {
        model: MODEL, max_tokens: MAX_TOKENS, stream: true, temperature: 0,
        system: 'You are a coding agent.',
        messages: [{ role: 'user', content: 'Read go.mod. Call the read tool now, no prose.' }],
        tools: [tool],
        tool_choice: { type: 'auto' },
      });
      if (r.status !== 200) { say('status 200', r.status + ' ' + String(r.body).slice(0, 120)); return problems; }
      if (!/text\/event-stream/.test(r.contentType)) say('content-type SSE', r.contentType);
      const events = parseAnthropicSSE(r.body);
      if (events.bad > 0) say('валидный JSON в событиях', 'битых: ' + events.bad);
      const types = events.list.map((e) => e.type);
      if (types[0] !== 'message_start') say('первое событие message_start', String(types[0]));
      if (types[types.length - 1] !== 'message_stop') say('последнее событие message_stop', String(types[types.length - 1]));
      const start = events.list[0] || {};
      const msg = start.message || {};
      if (msg.role !== 'assistant') say('message_start.message.role=assistant', String(msg.role));
      if (!String(msg.id || '').startsWith('msg_')) say('id в форме msg_', String(msg.id));
      const toolUse = events.list.filter((e) => e.type === 'content_block_start' && (e.content_block || {}).type === 'tool_use');
      if (toolUse.length === 0) {
        say('блок tool_use присутствует', 'нет; типы: ' + types.join(','));
        return problems;
      }
      const block = toolUse[0].content_block;
      if (!block.id) say('tool_use.id', 'пустой');
      if (block.name !== 'read') say('tool_use.name=read', String(block.name));
      if (typeof block.input !== 'object' || block.input === null || Array.isArray(block.input)) {
        say('tool_use.input — ОБЪЕКТ', typeof block.input);
      }
      const deltas = events.list.filter((e) => e.type === 'content_block_delta' && (e.delta || {}).type === 'input_json_delta');
      if (deltas.length === 0) say('input_json_delta для вызова', 'нет');
      const md = events.list.filter((e) => e.type === 'message_delta').pop() || {};
      if (((md.delta || {}).stop_reason) !== 'tool_use') say('stop_reason=tool_use', String((md.delta || {}).stop_reason));
      if (!md.usage || typeof md.usage.output_tokens !== 'number') say('message_delta.usage.output_tokens', JSON.stringify(md.usage));
      // второй ход: Anthropic-история с tool_use + tool_result
      const callID = block.id;
      const r2 = await post('/v1/messages', {
        model: MODEL, max_tokens: MAX_TOKENS, stream: true, temperature: 0,
        system: 'You are a coding agent.',
        messages: [
          { role: 'user', content: 'Read go.mod.' },
          { role: 'assistant', content: [{ type: 'tool_use', id: callID, name: 'read', input: { file_path: 'go.mod' } }] },
          { role: 'user', content: [
            { type: 'tool_result', tool_use_id: callID, content: [{ type: 'text', text: '1: module ollama-loadbalancer' }] },
            { type: 'text', text: 'Name the module from the tool result. One line, no tool call.' },
          ] },
        ],
        tools: [tool],
      });
      const ev2 = parseAnthropicSSE(r2.body);
      const text2 = ev2.list.filter((e) => e.type === 'content_block_delta' && (e.delta || {}).type === 'text_delta')
        .map((e) => e.delta.text).join('');
      if (!/ollama-loadbalancer/i.test(text2)) say('второй ход видит tool_result', JSON.stringify(text2.slice(0, 120)));
      return problems;
    },
  },
  {
    id: 'anthropic-error-shape',
    client: 'Anthropic SDK (ошибки)',
    surface: 'POST /v1/messages, невалидный запрос',
    async run() {
      const { problems, say } = checkProfile();
      const r = await post('/v1/messages', { max_tokens: 10, messages: [] });
      if (r.status !== 400) say('status 400', String(r.status));
      let j = null;
      try { j = JSON.parse(r.body); } catch (e) { say('ответ — JSON', String(r.body).slice(0, 80)); return problems; }
      if (j.type !== 'error') say('type=error', String(j.type));
      const err = j.error || {};
      if (err.type !== 'invalid_request_error') say('error.type=invalid_request_error', String(err.type));
      if (!err.message) say('error.message присутствует', 'пусто');
      return problems;
    },
  },
  {
    id: 'strict-loop',
    client: 'Continue/LangChain strict-схема (флейк-цикл)',
    surface: 'POST /v1/chat/completions × N',
    async run() {
      // R247/F: один провал схемы — ещё не дефект слоя (стохастика модели).
      // Цикл отделяет флейк от системной ошибки. По умолчанию 3 прогона;
      // приёмка по плану — R247_LOOP=20.
      const loops = parseInt(process.env.R247_LOOP || '3', 10);
      const { problems, say } = checkProfile();
      const bad = [];
      for (let i = 0; i < loops; i++) {
        const r = await post('/v1/chat/completions', {
          model: MODEL, stream: true, temperature: 0, max_tokens: MAX_TOKENS,
          messages: [{ role: 'user', content: 'Call the tool set_mode with mode="safe". Call it only.' }],
          tools: [STRICT_TOOL],
        });
        if (r.status !== 200) { bad.push('прогон ' + i + ': HTTP ' + r.status); continue; }
        const s = assembleSSE(r.body);
        if (!s.calls.length) { bad.push('прогон ' + i + ': вызова нет'); continue; }
        const call = s.calls[0];
        let args = null;
        try { args = JSON.parse(call.args || '{}'); } catch (e) { bad.push('прогон ' + i + ': arguments не JSON'); continue; }
        if (call.name !== 'set_mode') bad.push('прогон ' + i + ': вызван ' + call.name);
        else if (args.mode === undefined) bad.push('прогон ' + i + ': нет mode');
        else if (!['fast', 'safe'].includes(args.mode)) bad.push('прогон ' + i + ': mode вне enum');
      }
      if (bad.length) say('все ' + loops + ' прогонов валидны', bad.join('; '));
      return problems;
    },
  },
  {
    id: 'vision-honest',
    client: 'Мультимодальные клиенты (OpenAI + Anthropic)',
    surface: 'POST /v1/chat/completions и /v1/messages с картинкой',
    async run() {
      const { problems, say } = checkProfile();
      const image = { type: 'image_url', image_url: { url: 'data:image/png;base64,iVBORw0KGgo=' } };
      const r = await post('/v1/chat/completions', {
        model: MODEL, stream: false, max_tokens: 16, temperature: 0,
        messages: [{ role: 'user', content: [{ type: 'text', text: 'Что на картинке?' }, image] }],
      });
      if (r.status !== 415) say('OpenAI: status 415 (честный отказ)', String(r.status));
      if (r.headers && r.headers['x-vision-supported'] !== 'false') {
        say('OpenAI: X-Vision-Supported=false', String(r.headers && r.headers['x-vision-supported']));
      }
      let j = null;
      try { j = JSON.parse(r.body); } catch (e) { say('OpenAI: ответ — JSON', String(r.body).slice(0, 80)); }
      const err = (j && j.error) || {};
      if (err.code !== 'vision_not_supported') say('OpenAI: error.code=vision_not_supported', String(err.code));
      if (!/not supported/i.test(String(err.message || ''))) say('OpenAI: сообщение объясняет причину', String(err.message).slice(0, 100));
      // Anthropic-поверхность: блок image
      const r2 = await post('/v1/messages', {
        model: MODEL, max_tokens: 16, stream: false,
        messages: [{ role: 'user', content: [
          { type: 'text', text: 'Что на картинке?' },
          { type: 'image', source: { type: 'base64', media_type: 'image/png', data: 'iVBORw0KGgo=' } },
        ] }],
      });
      if (r2.status !== 415) say('Anthropic: status 415', String(r2.status));
      let a = null;
      try { a = JSON.parse(r2.body); } catch (e) { say('Anthropic: ответ — JSON', String(r2.body).slice(0, 80)); }
      if (!a || a.type !== 'error') say('Anthropic: type=error', JSON.stringify(a).slice(0, 100));
      else if (!/not supported/i.test(String((a.error || {}).message || ''))) {
        say('Anthropic: сообщение объясняет причину', String((a.error || {}).message).slice(0, 100));
      }
      return problems;
    },
  },
  {
    id: 'codex-responses',
    client: 'Codex-style (OpenAI Responses)',
    surface: 'POST /v1/responses (stream)',
    async run() {
      const { problems, say } = checkProfile();
      const tools = [{
        type: 'function', name: 'read', description: 'Read a file.',
        parameters: { type: 'object', properties: { file_path: { type: 'string' } }, required: ['file_path'] },
      }];
      const r = await post('/v1/responses', {
        model: MODEL, input: 'Read go.mod. Call the read tool now, no prose.',
        instructions: 'You are a coding agent.', max_output_tokens: MAX_TOKENS,
        stream: true, temperature: 0, tools,
      });
      if (r.status !== 200) { say('status 200', r.status + ' ' + String(r.body).slice(0, 120)); return problems; }
      if (!/text\/event-stream/.test(r.contentType)) say('content-type SSE', r.contentType);
      const events = parseResponsesSSE(r.body);
      if (events.bad > 0) say('валидный JSON в событиях', 'битых: ' + events.bad);
      const types = events.list.map((e) => e.type);
      if (types[0] !== 'response.created') say('первое событие response.created', String(types[0]));
      if (types[types.length - 1] !== 'response.completed') say('последнее событие response.completed', String(types[types.length - 1]));
      if (!events.list.every((e) => typeof e.sequence_number === 'number')) say('sequence_number у всех событий', 'нет');
      const fcDone = events.list.filter((e) => e.type === 'response.output_item.done' && (e.item || {}).type === 'function_call');
      if (fcDone.length === 0) {
        say('item function_call присутствует', 'типы: ' + types.join(','));
        return problems;
      }
      const item = fcDone[0].item;
      if (item.name !== 'read') say('function_call.name=read', String(item.name));
      if (!item.call_id) say('function_call.call_id', 'пустой');
      if (typeof item.arguments !== 'string') say('function_call.arguments — СТРОКА', typeof item.arguments);
      try { JSON.parse(item.arguments || '{}'); } catch (e) { say('arguments — JSON', String(item.arguments).slice(0, 80)); }
      const completed = events.list.filter((e) => e.type === 'response.completed').pop() || {};
      const resp = completed.response || {};
      if (resp.status !== 'completed') say('response.completed.status=completed', String(resp.status));
      if (!resp.usage || typeof resp.usage.input_tokens !== 'number') say('response.completed.usage', JSON.stringify(resp.usage));
      // второй ход: items function_call + function_call_output
      const r2 = await post('/v1/responses', {
        model: MODEL, instructions: 'You are a coding agent.', max_output_tokens: MAX_TOKENS,
        stream: true, temperature: 0, tools,
        input: [
          { type: 'message', role: 'user', content: [{ type: 'input_text', text: 'Read go.mod' }] },
          { type: 'function_call', call_id: item.call_id, name: 'read', arguments: item.arguments },
          { type: 'function_call_output', call_id: item.call_id, output: '1: module ollama-loadbalancer' },
          { type: 'message', role: 'user', content: [{ type: 'input_text', text: 'Name the module from the tool output. One line, no tool call.' }] },
        ],
      });
      const ev2 = parseResponsesSSE(r2.body);
      const text2 = ev2.list.filter((e) => e.type === 'response.output_text.delta').map((e) => e.delta).join('');
      if (!/ollama-loadbalancer/i.test(text2)) say('второй ход видит function_call_output', JSON.stringify(text2.slice(0, 120)));
      // non-stream форма
      const r3 = await post('/v1/responses', {
        model: MODEL, input: 'Say OK.', max_output_tokens: 24, temperature: 0, tools,
      });
      if (r3.status !== 200) { say('non-stream status 200', String(r3.status)); return problems; }
      let obj = null;
      try { obj = JSON.parse(r3.body); } catch (e) { say('non-stream ответ — JSON', String(r3.body).slice(0, 80)); return problems; }
      if (obj.object !== 'response' || obj.status !== 'completed') say('non-stream object=response, status=completed', JSON.stringify({ object: obj.object, status: obj.status }));
      if (!Array.isArray(obj.output)) say('non-stream output[]', typeof obj.output);
      if (typeof obj.output_text !== 'string') say('non-stream output_text', typeof obj.output_text);
      return problems;
    },
  },
  {
    id: 'responses-error-shape',
    client: 'OpenAI Responses (ошибки)',
    surface: 'POST /v1/responses, previous_response_id',
    async run() {
      const { problems, say } = checkProfile();
      const r = await post('/v1/responses', { model: MODEL, input: 'x', previous_response_id: 'resp_123' });
      if (r.status !== 400) say('status 400', String(r.status));
      let j = null;
      try { j = JSON.parse(r.body); } catch (e) { say('ответ — JSON', String(r.body).slice(0, 80)); return problems; }
      if (!j.error || !j.error.message) say('error.message присутствует', JSON.stringify(j).slice(0, 120));
      if (!/previous_response_id/i.test(String((j.error || {}).message))) {
        say('сообщение объясняет причину', String((j.error || {}).message).slice(0, 100));
      }
      return problems;
    },
  },
];

// ---------- прогон ----------

(async () => {
  const selected = PROFILES.filter((p) => ONLY.length === 0 || ONLY.indexOf(p.id) >= 0);
  let pass = 0;
  let fail = 0;
  let skip = 0;
  for (const p of selected) {
    if (p.skip) {
      skip++;
      console.log('SKIP  ' + p.id.padEnd(26) + ' ' + p.skip);
      continue;
    }
    const started = Date.now();
    const problems = await p.run();
    const ms = Date.now() - started;
    if (problems.length === 0) {
      pass++;
      console.log('PASS  ' + p.id.padEnd(26) + ' ' + String(ms).padStart(7) + 'ms  ' + p.client);
    } else {
      fail++;
      console.log('FAIL  ' + p.id.padEnd(26) + ' ' + String(ms).padStart(7) + 'ms  ' + p.client);
      for (const problem of problems) console.log('        - ' + problem);
    }
  }
  console.log('\nИТОГ: профилей ' + selected.length + ', PASS ' + pass + ', FAIL ' + fail + ', SKIP ' + skip);
  process.exit(fail ? 1 : 0);
})();


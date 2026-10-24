// R216: живая проверка разделения reasoning на /v1/chat/completions.
// Отправляет запрос через балансер (порт 18080) и печатает:
//   - finish_reason, длину/начало content и reasoning_content;
//   - для stream: сколько чанков в каждом канале и есть ли <think> в content.
//
// PROMPT через UTF-8: тело собирается в JS, поэтому кириллица не портится
// (PowerShell Out-File -Encoding ascii её заменяет на '?').
const http = require('http');

const PORT = Number(process.env.PORT || 18080);
const MODEL = process.env.MODEL || 'Qwen3.8-27B-UD-Q4_K_M';
const STREAM = process.env.STREAM !== '0';
const TOOLS = process.env.TOOLS === '1';
const MAX_TOKENS = Number(process.env.MAX_TOKENS || 400);
const PROMPT = process.env.PROBE_PROMPT || 'Сколько будет 17*23? Ответь числом.';

function post(body) {
  return new Promise((resolve, reject) => {
    const data = JSON.stringify(body);
    const r = http.request({
      host: '127.0.0.1', port: PORT, path: '/v1/chat/completions', method: 'POST',
      headers: { 'Content-Type': 'application/json', 'Content-Length': Buffer.byteLength(data) },
    }, res => {
      let s = '';
      res.on('data', d => (s += d));
      res.on('end', () => resolve({ status: res.statusCode, body: s }));
    });
    r.on('error', reject);
    r.write(data);
    r.end();
  });
}

function analyzeStream(raw) {
  let content = '', reasoning = '', contentChunks = 0, reasoningChunks = 0, finish = null, toolCalls = 0;
  for (const line of raw.split('\n')) {
    const t = line.trim();
    if (!t.startsWith('data: ')) continue;
    const payload = t.slice(6);
    if (payload === '[DONE]') continue;
    let j;
    try { j = JSON.parse(payload); } catch (e) { continue; }
    const ch = (j.choices || [])[0] || {};
    const d = ch.delta || {};
    if (typeof d.content === 'string' && d.content !== '') { contentChunks++; content += d.content; }
    if (typeof d.reasoning_content === 'string' && d.reasoning_content !== '') { reasoningChunks++; reasoning += d.reasoning_content; }
    if (Array.isArray(d.tool_calls)) toolCalls += d.tool_calls.length;
    if (ch.finish_reason) finish = ch.finish_reason;
  }
  return { content, reasoning, contentChunks, reasoningChunks, finish, toolCalls };
}

(async () => {
  const body = {
    model: MODEL,
    stream: STREAM,
    max_tokens: MAX_TOKENS,
    messages: [{ role: 'user', content: PROMPT }],
  };
  if (TOOLS) {
    body.tools = [{
      type: 'function',
      function: { name: 'read_file', description: 'read a file', parameters: { type: 'object' } },
    }];
  }
  const started = Date.now();
  const res = await post(body);
  if (res.status !== 200) {
    console.log('HTTP', res.status, res.body.slice(0, 400));
    process.exit(1);
  }
  if (!STREAM) {
    const j = JSON.parse(res.body);
    const ch = (j.choices || [])[0] || {};
    const m = ch.message || {};
    console.log(JSON.stringify({
      stream: false, tools: TOOLS, ms: Date.now() - started,
      finish: ch.finish_reason,
      contentLen: (m.content || '').length, reasoningLen: (m.reasoning_content || '').length,
      contentHead: (m.content || '').slice(0, 140),
      reasoningHead: (m.reasoning_content || '').slice(0, 140),
      usage: j.usage,
    }, null, 1));
    return;
  }
  const a = analyzeStream(res.body);
  console.log(JSON.stringify({
    stream: true, tools: TOOLS, ms: Date.now() - started,
    finish: a.finish, toolCalls: a.toolCalls,
    contentChunks: a.contentChunks, reasoningChunks: a.reasoningChunks,
    contentLen: a.content.length, reasoningLen: a.reasoning.length,
    contentHasThink: /<\/?think>|<\|channel>|<\/?reasoning>/.test(a.content),
    contentHead: a.content.slice(0, 140),
    reasoningHead: a.reasoning.slice(0, 140),
  }, null, 1));
})().catch(e => { console.log('FATAL', e.message); process.exit(1); });

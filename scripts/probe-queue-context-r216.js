// probe-queue-context-r216.js — R216 (2026-10-21).
//
// Две живые проверки через балансер (порт 18080):
//  1) ОЧЕРЕДЬ: несколько одновременных запросов с РАЗНЫМИ маркерами в промпте.
//     Клиент жаловался: «не мог ли балансер поставить ответ в очередь и выдать
//     после работы другого запроса как продолжение». Проверяем, что каждый
//     ответ содержит СВОЙ маркер и что ответы не перепутаны.
//  2) КОНТЕКСТ: два хода в одном диалоге. Первый задаёт код, второй (с историей)
//     просит его вспомнить. Если контекст не доезжает, модель кода не знает.
//
// Промпты собираются в JS (UTF-8), PowerShell их не портит.
const http = require('http');

const PORT = Number(process.env.PORT || 18080);
const MODEL = process.env.MODEL || 'gemma-4-E4B-it-Q4_K_M';
const TOKEN = process.env.TOKEN || '';

function post(body) {
  return new Promise((resolve, reject) => {
    const data = JSON.stringify(body);
    const r = http.request({
      host: '127.0.0.1', port: PORT, path: '/v1/chat/completions', method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        'Content-Length': Buffer.byteLength(data),
        ...(TOKEN ? { 'X-API-Token': TOKEN } : {}),
      },
    }, res => {
      let s = '';
      res.on('data', d => (s += d));
      res.on('end', () => {
        let parsed = null;
        try { parsed = JSON.parse(s); } catch (e) {}
        resolve({ status: res.statusCode, raw: s, json: parsed, headers: res.headers });
      });
    });
    r.on('error', reject);
    r.write(data);
    r.end();
  });
}

function answerText(res) {
  if (res.json) {
    const ch = (res.json.choices || [])[0] || {};
    const m = ch.message || {};
    return (m.content || '') + '\n' + (m.reasoning_content || '');
  }
  // stream: склеиваем content + reasoning
  let out = '';
  for (const line of res.raw.split('\n')) {
    const t = line.trim();
    if (!t.startsWith('data: ')) continue;
    const p = t.slice(6);
    if (p === '[DONE]') continue;
    try {
      const j = JSON.parse(p);
      const d = ((j.choices || [])[0] || {}).delta || {};
      if (d.content) out += d.content;
      if (d.reasoning_content) out += d.reasoning_content;
    } catch (e) {}
  }
  return out;
}

(async () => {
  // ---- 1) очередь: 3 одновременных запроса с разными маркерами ----
  const codes = ['ALFA-111', 'BRAVO-222', 'CHARLIE-333'];
  const started = Date.now();
  const responses = await Promise.all(codes.map(code => post({
    model: MODEL,
    stream: false,
    max_tokens: 120,
    messages: [{
      role: 'user',
      content: 'Повтори ровно этот код и больше ничего: ' + code,
    }],
  })));
  const ms = Date.now() - started;

  const results = codes.map((code, i) => {
    const text = answerText(responses[i]);
    const own = text.includes(code);
    const foreign = codes.filter(c => c !== code && text.includes(c));
    return {
      code, status: responses[i].status,
      ownMarker: own, foreignMarkers: foreign,
      len: text.length, preview: text.slice(0, 90).replace(/\s+/g, ' '),
    };
  });
  console.log('QUEUE (concurrent, ' + ms + ' ms):');
  for (const r of results) console.log('  ', JSON.stringify(r));
  const queueOK = results.every(r => r.status === 200 && r.foreignMarkers.length === 0 && r.ownMarker);
  console.log('  QUEUE OK:', queueOK);

  // ---- 2) контекст: два хода в одном диалоге ----
  const code = 'КРАСНЫЙ-47';
  const first = await post({
    model: MODEL,
    stream: false,
    max_tokens: 200,
    messages: [{ role: 'user', content: 'Запомни код: ' + code + '. Ответь одним словом: принято.' }],
  });
  const history = [
    { role: 'user', content: 'Запомни код: ' + code + '. Ответь одним словом: принято.' },
    { role: 'assistant', content: answerText(first).slice(0, 400) || 'принято' },
    { role: 'user', content: 'Какой код ты запомнил? Назови его одной строкой.' },
  ];
  const second = await post({ model: MODEL, stream: false, max_tokens: 200, messages: history });
  const secondText = answerText(second);
  console.log('CONTEXT:');
  console.log('  first status', first.status, 'prompt_tokens', (first.json && first.json.usage) || null);
  console.log('  second status', second.status, 'prompt_tokens', (second.json && second.json.usage) || null);
  console.log('  recall:', secondText.includes(code), JSON.stringify(secondText.slice(0, 160)));
  console.log('  CONTEXT OK:', second.status === 200 && secondText.includes(code));
})().catch(e => { console.log('FATAL', e.message); process.exit(1); });

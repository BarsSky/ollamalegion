// live-regression-r219.js — R219: живые проверки поведения балансера/бэкенда,
// которые ловят регрессии класса, найденного в сессии R214-R218.
//
// Что проверяется (и какую регрессию ловит):
//  1. /v1/models: у загруженной модели обязательны context_length и
//     max_context_length (R216: клиент подбирал историю по потолку).
//  2. Заголовок X-Model-Context-Length на ответе модели (R216).
//  3. Размышления: content без маркеров <think>/<|channel>; reasoning и content
//     не дублируют друг друга; ответ не пустой (R216/R219).
//  4. Очередь: 3 параллельных запроса с разными маркерами — каждый ответ
//     содержит СВОЙ маркер, чужих нет (проверка «ответ от другого запроса»).
//  5. История диалога доезжает до модели (два хода, recall кода).
//  6. Инъекция image-инструмента соответствует флагу (--inject=off|always):
//     off — заголовка X-Image-Tool нет; always — есть.
//
// Запуск:
//   node scripts/live-regression-r219.js --model=gemma-4-E4B-it-Q4_K_M.gguf --inject=off
// Опции: --base=http://127.0.0.1:18080, --token=..., --timeout=180
//
// Стенд должен быть поднят; модель с таким именем должна быть загружена
// (скрипт не грузит её сам: загрузка — отдельный тест, см. probe-webui-load-r218).
const http = require('http');

const args = {};
for (const a of process.argv.slice(2)) {
  const m = a.match(/^--([^=]+)=(.*)$/);
  if (m) args[m[1]] = m[2];
}
const BASE = new URL(args.base || 'http://127.0.0.1:18080');
const TOKEN = args.token || '';
const MODEL = (args.model || 'gemma-4-E4B-it-Q4_K_M.gguf').replace(/\.gguf$/i, '');
const INJECT = (args.inject || '').toLowerCase(); // off | always | auto | ''
const TIMEOUT_MS = Number(args.timeout || 180) * 1000;

let failures = 0;
function ok(name, detail) { console.log('PASS ' + name + (detail ? ' :: ' + detail : '')); }
function fail(name, detail) { failures++; console.log('FAIL ' + name + ' :: ' + detail); }

// R221: «модель не загружена» — это НЕ регрессия кода, а неподготовленный стенд.
// Раньше такой прогон давал FAIL про context_length с текстом «клиент снова
// подберёт историю по потолку», и это читалось как поломка R216.
let preconditions = 0;
function precondition(name, detail) { preconditions++; console.log('PRECONDITION ' + name + ' :: ' + detail); }

function post(path, body, stream) {
  return new Promise((resolve, reject) => {
    const data = JSON.stringify(body);
    const req = http.request({
      host: BASE.hostname, port: BASE.port, path, method: 'POST',
      headers: {
        'Content-Type': 'application/json', 'Content-Length': Buffer.byteLength(data),
        ...(TOKEN ? { 'X-API-Token': TOKEN } : {}),
      },
    }, res => {
      let s = '';
      res.on('data', d => (s += d));
      res.on('end', () => resolve({ status: res.statusCode, headers: res.headers, body: s, stream }));
    });
    req.on('error', reject);
    req.setTimeout(TIMEOUT_MS, () => req.destroy(new Error('timeout')));
    req.write(data);
    req.end();
  });
}

function get(path) {
  return new Promise((resolve, reject) => {
    const req = http.request({ host: BASE.hostname, port: BASE.port, path, method: 'GET' }, res => {
      let s = '';
      res.on('data', d => (s += d));
      res.on('end', () => { try { resolve({ status: res.statusCode, json: JSON.parse(s) }); } catch (e) { resolve({ status: res.statusCode, json: null }); } });
    });
    req.on('error', reject);
    req.end();
  });
}

function parseSSE(raw) {
  let content = '', reasoning = '', finish = null, done = false, chunks = 0;
  for (const line of raw.split('\n')) {
    const t = line.trim();
    if (!t.startsWith('data: ')) continue;
    const p = t.slice(6);
    if (p === '[DONE]') { done = true; continue; }
    let j;
    try { j = JSON.parse(p); } catch (e) { continue; }
    const ch = (j.choices || [])[0] || {};
    const d = ch.delta || {};
    if (typeof d.content === 'string' && d.content !== '') { content += d.content; chunks++; }
    if (typeof d.reasoning_content === 'string' && d.reasoning_content !== '') reasoning += d.reasoning_content;
    if (ch.finish_reason) finish = ch.finish_reason;
  }
  return { content, reasoning, finish, done, chunks };
}

function answerText(res) {
  if (res.stream) return parseSSE(res.body);
  try {
    const j = JSON.parse(res.body);
    const ch = (j.choices || [])[0] || {};
    const m = ch.message || {};
    return { content: m.content || '', reasoning: m.reasoning_content || m.reasoning || '', finish: ch.finish_reason, done: true, chunks: 0 };
  } catch (e) { return { content: '', reasoning: '', finish: null, done: false, chunks: 0 }; }
}

const REASONING_MARKERS = /<\/?think>|<\/?thinking>|<\/?reasoning>|<\|channel>|<channel\|>/;

(async () => {
  // 1. /v1/models: окно контекста у загруженной модели.
  const models = await get('/v1/models');
  const list = (models.json && models.json.data) || [];
  const entry = list.find(m => m.id === MODEL) || null;
  if (!entry) {
    precondition('v1.models.contains_model', 'модель ' + MODEL + ' отсутствует в /v1/models — загрузите её и повторите (это не регрессия кода)');
    failures++;
  } else if (!entry.context_length) {
    // R221: пустое окно у уже загруженной модели — регрессия; у незагруженной —
    // стенд не подготовлен (балансер отдаёт запись без окна).
    if (!entry.max_context_length) {
      precondition('v1.models.context_length', 'модель ' + MODEL + ' не загружена: окна контекста нет, проверки окна недействительны — загрузите модель и повторите');
    } else {
      fail('v1.models.context_length', 'у ' + MODEL + ' нет context_length — клиент снова подберёт историю по потолку');
    }
  } else {
    ok('v1.models.context_length', 'context_length=' + entry.context_length + ' max=' + entry.max_context_length);
  }

  // 2-3. Один непустой ответ: заголовок контекста + размышления.
  const prompt = 'Сколько будет 17*23? Ответь числом.';
  const tiny = await post('/v1/chat/completions', {
    model: MODEL, stream: true, max_tokens: 24, messages: [{ role: 'user', content: prompt }],
  }, true);
  if (tiny.status !== 200) {
    fail('v1.stream.status', 'HTTP ' + tiny.status + ' ' + tiny.body.slice(0, 200));
  } else {
    const answer = answerText(tiny);
    if (!tiny.headers['x-model-context-length']) {
      fail('headers.x_model_context_length', 'нет заголовка X-Model-Context-Length');
    } else {
      ok('headers.x_model_context_length', tiny.headers['x-model-context-length']);
    }
    if (REASONING_MARKERS.test(answer.content)) {
      fail('reasoning.no_markers_in_content', 'content содержит маркеры размышлений: ' + answer.content.slice(0, 120));
    } else {
      ok('reasoning.no_markers_in_content', 'contentLen=' + answer.content.length + ' reasoningLen=' + answer.reasoning.length);
    }
    if (answer.reasoning && answer.content && answer.reasoning.trim() === answer.content.trim()) {
      fail('reasoning.not_duplicated', 'reasoning и content совпадают целиком — размышления попали в ответ');
    } else {
      ok('reasoning.not_duplicated');
    }
    if (answer.content.trim() === '' && answer.reasoning.trim() === '') {
      fail('answer.not_empty', 'пустой ответ: ни content, ни reasoning');
    } else {
      ok('answer.not_empty');
    }
    if (!answer.done) fail('stream.done', 'поток не завершён [DONE]');
    else ok('stream.done', 'finish=' + answer.finish);
  }

  // 4. Очередь: параллельные запросы не должны путать ответы.
  const codes = ['ALFA-111', 'BRAVO-222', 'CHARLIE-333'];
  // Ошибка одного запроса не должна валить весь прогон: иначе ECONNRESET на
  // одном соединении скрывает результаты остальных проверок.
  const parallel = await Promise.all(codes.map(code => post('/v1/chat/completions', {
    model: MODEL, stream: false, max_tokens: 120,
    messages: [{ role: 'user', content: 'Повтори ровно этот код и больше ничего: ' + code }],
  }, false).catch(e => ({ status: 0, headers: {}, body: '', stream: false, error: e.code || e.message }))));
  codes.forEach((code, i) => {
    const a = answerText(parallel[i]);
    const text = a.content + '\n' + a.reasoning;
    const foreign = codes.filter(c => c !== code && text.includes(c));
    if (parallel[i].status !== 200) {
      fail('queue.' + code + '.status', 'HTTP ' + parallel[i].status + ' ' + (parallel[i].error || ''));
    } else if (foreign.length) fail('queue.' + code + '.crosstalk', 'в ответе чужие маркеры ' + foreign.join(','));
    else if (!text.includes(code)) fail('queue.' + code + '.own_marker', 'свой маркер не найден: ' + text.slice(0, 120));
    else ok('queue.' + code);
  });

  // 5. История диалога доезжает до модели.
  const code = 'КРАСНЫЙ-47';
  const first = await post('/v1/chat/completions', {
    model: MODEL, stream: false, max_tokens: 200,
    messages: [{ role: 'user', content: 'Запомни код: ' + code + '. Ответь одним словом: принято.' }],
  }, false);
  const firstText = answerText(first).content;
  const second = await post('/v1/chat/completions', {
    model: MODEL, stream: false, max_tokens: 200,
    messages: [
      { role: 'user', content: 'Запомни код: ' + code + '. Ответь одним словом: принято.' },
      { role: 'assistant', content: firstText || 'принято' },
      { role: 'user', content: 'Какой код ты запомнил? Назови его одной строкой.' },
    ],
  }, false);
  const secondText = answerText(second).content + '\n' + answerText(second).reasoning;
  if (!secondText.includes(code)) fail('context.history_recall', 'модель не вспомнила код: ' + secondText.slice(0, 160));
  else ok('context.history_recall');

  // 6. Инъекция image-инструмента соответствует флагу.
  if (INJECT === 'off' || INJECT === 'always') {
    const probe = await post('/v1/chat/completions', {
      model: MODEL, stream: false, max_tokens: 16, messages: [{ role: 'user', content: 'Ответь одним словом: ок.' }],
    }, false);
    const has = !!probe.headers['x-image-tool'];
    if (INJECT === 'off' && has) fail('image_tool.inject_off', 'при off объявлен инструмент: ' + probe.headers['x-image-tool']);
    else if (INJECT === 'always' && !has) fail('image_tool.inject_always', 'при always инструмент не объявлен');
    else ok('image_tool.inject_' + INJECT);
  } else {
    console.log('SKIP image_tool.inject (не задан --inject=off|always)');
  }

  console.log(failures === 0 ? '\nLIVE REGRESSION: OK' : '\nLIVE REGRESSION: ' + failures + ' провал(ов)');
  if (preconditions > 0) console.log('ПРЕДУСЛОВИЕ не выполнено ('+preconditions+'): стенд не подготовлен, часть проверок недействительна — см. PRECONDITION выше');
  process.exit(failures === 0 ? 0 : 1);
})().catch(e => { console.log('FATAL', e.message); process.exit(1); });

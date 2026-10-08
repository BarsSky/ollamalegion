// loadtest.mjs — нагрузочный клиент балансера Ollama Legion (R91, 2026-10-08).
//
// ЗАЧЕМ. Нужно проверить балансер РЕАЛЬНЫМИ запросами с НЕСКОЛЬКИХ клиентов
// одновременно: текстовая генерация (gemma-4) и генерация картинок (лёгкая
// модель sd15-q4 на sd.cpp). Синтетика «curl в цикле» для этого не годится:
// она не даёт ни конкурентности, ни честных перцентилей, ни одновременной
// текстово-картиночной нагрузки на одну GPU.
//
// ЧТО ДЕЛАЕТ.
//   1. Поднимает N текстовых и M image-«клиентов». Каждый клиент — отдельная
//      СЕССИЯ балансера (заголовки X-User-Id/X-Session-Id): именно так балансер
//      различает клиентов для слотов и справедливости (см.
//      internal/balancer/autoload_wait.go: RequestSessionKey).
//   2. Текстовый клиент шлёт потоковый POST /v1/chat/completions и измеряет
//      TTFT (время до первого токена) и полное время; считает токены ответа.
//      Потоковый режим выбран намеренно: не-потоковый на этом стенде
//      конвертируется балансером внутри (LB_OPENAI_AUTO_STREAM=true) и, как
//      показала проверка 2026-10-08, скрывает обрыв апстрима — клиент получает
//      HTTP 200 с ПУСТЫМ content. Поток виден клиенту целиком.
//   3. Image-клиент шлёт POST /v1/images/generations, проверяет, что вернулся
//      настоящий PNG нужного размера (а не пустой ответ), и меряет задержку.
//   4. Параллельно раз в секунду снимает состояние кластера с admin-API:
//      /api/v1/metrics (активные запросы, VRAM, по бэкендам) и
//      /api/v1/queue/stats (очередь). Это позволяет отличить «балансер держит
//      нагрузку» от «балансер отдал 200, потому что всё попало в очередь».
//
// ЗАПУСК (примеры):
//   node scripts/loadtest/loadtest.mjs --text-clients 4 --duration 60
//   node scripts/loadtest/loadtest.mjs --image-clients 4 --image-requests 3
//   node scripts/loadtest/loadtest.mjs --text-clients 4 --image-clients 2 --duration 90
//
// Выход: сводка в stdout + полный отчёт в JSON (--out).

import { writeFileSync } from 'node:fs';

// ─────────────────────────── аргументы ───────────────────────────

function parseArgs(argv) {
  const out = {
    base: 'http://127.0.0.1:18079',
    admin: 'http://127.0.0.1:18081',
    token: process.env.LB_API_TOKEN || 'changeme-bundled-with-agent-token',
    model: 'gemma-4-E4B-it-Q4_K_M',
    prompt: 'Кратко объясни, что такое балансировка нагрузки, в двух предложениях.',
    maxTokens: 96,
    imageModel: 'sd15-q4',
    imageSize: '512x512',
    imageSteps: 8,
    imagePrompt: 'a red apple on a wooden table, photo',
    textClients: 0,
    imageClients: 0,
    duration: 45,
    reqCount: 0,
    requests: 0,
    imageRequests: 0,
    errorBackoffMs: 1000,
    maxErrorBackoffMs: 5000,
    timeoutMs: 900000,
    out: '',
    label: '',
    warmup: true
  };
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    const val = () => argv[++i];
    switch (a) {
      case '--base': out.base = val(); break;
      case '--admin': out.admin = val(); break;
      case '--token': out.token = val(); break;
      case '--model': out.model = val(); break;
      case '--prompt': out.prompt = val(); break;
      case '--max-tokens': out.maxTokens = Number(val()); break;
      case '--image-model': out.imageModel = val(); break;
      case '--image-size': out.imageSize = val(); break;
      case '--image-steps': out.imageSteps = Number(val()); break;
      case '--image-prompt': out.imagePrompt = val(); break;
      case '--text-clients': out.textClients = Number(val()); break;
      case '--image-clients': out.imageClients = Number(val()); break;
      case '--duration': out.duration = Number(val()); break;
      case '--requests': out.requests = Number(val()); break;
      case '--image-requests': out.imageRequests = Number(val()); break;
      case '--timeout-ms': out.timeoutMs = Number(val()); break;
      case '--error-backoff-ms': out.errorBackoffMs = Number(val()); break;
      case '--max-error-backoff-ms': out.maxErrorBackoffMs = Number(val()); break;
      case '--out': out.out = val(); break;
      case '--label': out.label = val(); break;
      case '--no-warmup': out.warmup = false; break;
      default:
        if (a.startsWith('--')) throw new Error('неизвестный аргумент: ' + a);
    }
  }
  if (!out.textClients && !out.imageClients) {
    out.textClients = 4;
    out.duration = 30;
  }
  return out;
}

const cfg = parseArgs(process.argv.slice(2));

// ─────────────────────────── утилиты ───────────────────────────

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

function percentile(sorted, p) {
  if (!sorted.length) return null;
  const idx = Math.min(sorted.length - 1, Math.max(0, Math.ceil((p / 100) * sorted.length) - 1));
  return sorted[idx];
}

function summarize(values) {
  const v = values.filter((x) => typeof x === 'number' && isFinite(x)).sort((a, b) => a - b);
  if (!v.length) return { n: 0 };
  const sum = v.reduce((a, b) => a + b, 0);
  return {
    n: v.length,
    min: v[0],
    p50: percentile(v, 50),
    p90: percentile(v, 90),
    p99: percentile(v, 99),
    max: v[v.length - 1],
    mean: Math.round((sum / v.length) * 10) / 10
  };
}

// Обрезка тела ошибки: в отчёт нужна ПРИЧИНА, а не мегабайты.
function brief(text, n = 400) {
  const s = String(text == null ? '' : text).replace(/\s+/g, ' ').trim();
  return s.length > n ? s.slice(0, n) + '…' : s;
}

// errorCodeFromBody — короткий КОД причины из конверта ошибки балансера.
//
// Балансер отдаёт две формы: {"error":{"code","message","hint"}} (OpenAI-путь) и
// {"error":"строка","code":...} (плоская). Для гистограммы нужен код, а не текст:
// по тексту «insufficient_vram» и «no healthy backend» слились бы в одну строку
// с описанием.
function errorCodeFromBody(text) {
  try {
    const j = JSON.parse(text);
    if (j && j.error && typeof j.error === 'object' && j.error.code) return String(j.error.code);
    if (j && typeof j.code === 'string' && j.code) return j.code;
    if (j && typeof j.error === 'string' && j.error) return brief(j.error, 60);
  } catch { /* не JSON — вернём пусто */ }
  return '';
}

// ─────────────────────── клиент: текст ───────────────────────

async function textRequest(clientId, sessionKey, seq) {
  const body = {
    model: cfg.model,
    messages: [{ role: 'user', content: cfg.prompt }],
    max_tokens: cfg.maxTokens,
    stream: true
  };
  const started = Date.now();
  const rec = {
    kind: 'text', client: clientId, seq, startedAt: new Date(started).toISOString(),
    status: 0, ttftMs: null, totalMs: null, contentChars: 0, completionTokens: null,
    promptTokens: null, finishReason: null, streamId: null, error: ''
  };
  const ac = new AbortController();
  const timer = setTimeout(() => ac.abort(), cfg.timeoutMs);
  try {
    const res = await fetch(cfg.base + '/v1/chat/completions', {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        // Разные клиенты = разные сессии балансера (не «один клиент в N потоков»).
        'X-User-Id': 'loadtest-' + clientId,
        'X-Session-Id': 'loadtest-' + sessionKey + '-' + seq
      },
      body: JSON.stringify(body),
      signal: ac.signal
    });
    rec.status = res.status;
    if (!res.ok) {
      const body = await res.text().catch(() => '');
      rec.errorCode = errorCodeFromBody(body);
      rec.error = 'HTTP ' + res.status + ': ' + brief(body);
      rec.totalMs = Date.now() - started;
      return rec;
    }
    const reader = res.body.getReader();
    const dec = new TextDecoder();
    let buf = '';
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      buf += dec.decode(value, { stream: true });
      let sep;
      while ((sep = buf.indexOf('\n\n')) !== -1) {
        const chunk = buf.slice(0, sep);
        buf = buf.slice(sep + 2);
        for (const line of chunk.split('\n')) {
          if (!line.startsWith('data:')) continue;
          const payload = line.slice(5).trim();
          if (!payload || payload === '[DONE]') continue;
          let ev;
          try { ev = JSON.parse(payload); } catch { continue; }
          if (!rec.streamId && ev.id) rec.streamId = ev.id;
          const ch = (ev.choices && ev.choices[0]) || {};
          const delta = ch.delta || {};
          // TTFT считаем по ПЕРВОМУ непустому контенту: пустая роль-roledelta
          // приходит раньше и «время до первого токена» ею измерять нельзя.
          if (typeof delta.content === 'string' && delta.content.length > 0) {
            if (rec.ttftMs === null) rec.ttftMs = Date.now() - started;
            rec.contentChars += delta.content.length;
          }
          if (ch.finish_reason) rec.finishReason = ch.finish_reason;
          if (ev.usage) {
            rec.completionTokens = ev.usage.completion_tokens;
            rec.promptTokens = ev.usage.prompt_tokens;
          }
        }
      }
    }
    rec.totalMs = Date.now() - started;
    if (rec.contentChars === 0) {
      // Именно этот случай 2026-10-08 маскировался под успех: HTTP 200,
      // finish_reason=stop, а текста нет. Помечаем явно.
      rec.error = 'пустой ответ: ни одного токена контента (finish_reason=' + rec.finishReason + ')';
    }
    return rec;
  } catch (e) {
    rec.totalMs = Date.now() - started;
    rec.error = (e && e.name === 'AbortError') ? 'timeout ' + cfg.timeoutMs + 'ms' : String((e && e.message) || e);
    return rec;
  } finally {
    clearTimeout(timer);
  }
}

// ─────────────────────── клиент: картинки ───────────────────────

function pngInfo(buf) {
  if (buf.length < 24) return null;
  const sig = [0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a];
  for (let i = 0; i < 8; i++) if (buf[i] !== sig[i]) return null;
  const be = (o) => (buf[o] << 24) | (buf[o + 1] << 16) | (buf[o + 2] << 8) | buf[o + 3];
  return { width: be(16), height: be(20) };
}

async function imageRequest(clientId, sessionKey, seq) {
  const [w, h] = String(cfg.imageSize).split('x').map((n) => Number(n));
  const body = {
    model: cfg.imageModel,
    prompt: cfg.imagePrompt + ' #' + seq,
    size: cfg.imageSize,
    steps: cfg.imageSteps,
    n: 1,
    response_format: 'b64_json'
  };
  const started = Date.now();
  const rec = {
    kind: 'image', client: clientId, seq, startedAt: new Date(started).toISOString(),
    status: 0, totalMs: null, bytes: 0, width: null, height: null, error: ''
  };
  const ac = new AbortController();
  const timer = setTimeout(() => ac.abort(), cfg.timeoutMs);
  try {
    const res = await fetch(cfg.base + '/v1/images/generations', {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        'X-User-Id': 'loadtest-img-' + clientId,
        'X-Session-Id': 'loadtest-img-' + sessionKey + '-' + seq
      },
      body: JSON.stringify(body),
      signal: ac.signal
    });
    rec.status = res.status;
    if (!res.ok) {
      const body = await res.text().catch(() => '');
      rec.errorCode = errorCodeFromBody(body);
      rec.error = 'HTTP ' + res.status + ': ' + brief(body);
      rec.totalMs = Date.now() - started;
      return rec;
    }
    const json = await res.json();
    const item = (json && json.data && json.data[0]) || {};
    const b64 = item.b64_json || '';
    if (!b64) {
      rec.error = 'в ответе нет b64_json';
    } else {
      const buf = Buffer.from(b64, 'base64');
      rec.bytes = buf.length;
      const info = pngInfo(buf);
      if (!info) {
        rec.error = 'ответ не PNG (' + buf.length + ' байт)';
      } else {
        rec.width = info.width;
        rec.height = info.height;
        if (info.width !== w || info.height !== h) {
          rec.error = 'размер PNG ' + info.width + 'x' + info.height + ', ожидался ' + w + 'x' + h;
        }
      }
    }
    rec.totalMs = Date.now() - started;
    return rec;
  } catch (e) {
    rec.totalMs = Date.now() - started;
    rec.error = (e && e.name === 'AbortError') ? 'timeout ' + cfg.timeoutMs + 'ms' : String((e && e.message) || e);
    return rec;
  } finally {
    clearTimeout(timer);
  }
}

// ─────────────────────── сэмплер метрик ───────────────────────

function makeSampler() {
  const samples = [];
  let stop = false;
  const adm = (path) => fetch(cfg.admin + path, { headers: { 'X-API-Token': cfg.token } })
    .then((r) => (r.ok ? r.json() : null)).catch(() => null);

  async function tick() {
    const [metrics, queue] = await Promise.all([adm('/api/v1/metrics'), adm('/api/v1/queue/stats')]);
    const backends = (metrics && (metrics.backends || (metrics.cluster && metrics.cluster.backends))) || [];
    const sample = { at: new Date().toISOString(), };
    if (queue) {
      sample.queue = {
        pending: queue.pending ?? queue.queueLength ?? queue.total ?? null,
        active: queue.active ?? queue.processing ?? null
      };
    }
    if (backends.length) {
      sample.backends = backends.map((b) => ({
        id: b.id || b.agentId,
        status: b.status,
        active: b.ollama ? b.ollama.activeRequests : (b.activeRequests ?? null),
        total: b.ollama ? b.ollama.totalRequests : (b.totalRequests ?? null),
        vramUsed: b.gpu ? b.gpu.memoryUsed : null,
        vramTotal: b.gpu ? b.gpu.memoryTotal : null,
        gpuUtil: b.gpu ? b.gpu.usagePercent : null,
        ramUsed: b.system ? b.system.memoryUsed : null,
        loaded: b.llamaCpp && b.llamaCpp.loadedModels ? b.llamaCpp.loadedModels.map((m) => m.name) : []
      }));
    }
    samples.push(sample);
  }

  const loop = (async () => {
    while (!stop) {
      await tick();
      await sleep(1000);
    }
  })();

  return { samples, async finish() { stop = true; await loop; } };
}

// ─────────────────────── раннер ───────────────────────

async function runPool(kind, clients, records, deadline) {
  const tasks = [];
  for (let i = 0; i < clients; i++) {
    const clientId = kind + '-' + (i + 1);
    tasks.push((async () => {
      let seq = 0;
      let consecutiveErrors = 0;
      while (Date.now() < deadline) {
        seq++;
        const rec = kind === 'text'
          ? await textRequest(clientId, 's' + (i + 1), seq)
          : await imageRequest(clientId, 's' + (i + 1), seq);
        records.push(rec);
        // Откат при ошибке обязателен. Без него отклоняющий запросы балансер
        // (например image-гейт VRAM отдаёт 503 за ~2 мс) превращает клиента в
        // генератор тысяч отказов в секунду: в прогоне 2026-10-08 так набралось
        // 23 396 «запросов» за минуту, и это забивало лог балансера, а не
        // измеряло его возможности. Реальная нагрузка так себя не ведёт.
        if (rec.error) {
          consecutiveErrors++;
          const backoff = Math.min(cfg.errorBackoffMs * consecutiveErrors, cfg.maxErrorBackoffMs);
          await sleep(backoff);
        } else {
          consecutiveErrors = 0;
        }
        const limit = kind === 'text' ? cfg.requests : cfg.imageRequests;
        if (limit > 0 && seq >= limit) break;
      }
    })());
  }
  await Promise.all(tasks);
}

function report(records, samples, meta) {
  const text = records.filter((r) => r.kind === 'text');
  const image = records.filter((r) => r.kind === 'image');
  const okText = text.filter((r) => !r.error);
  const okImage = image.filter((r) => !r.error);
  const errors = {};
  for (const r of records) {
    if (!r.error) continue;
    const key = r.kind + ' | ' +
      (r.status ? 'HTTP ' + r.status + ' ' : '') +
      (r.errorCode ? r.errorCode + ' ' : '') +
      r.error.replace(/^HTTP \d+: /, '').split(':')[0].slice(0, 70);
    errors[key] = (errors[key] || 0) + 1;
  }
  const totalWall = (Date.now() - meta.startedAt) / 1000;
  return {
    label: cfg.label,
    startedAt: new Date(meta.startedAt).toISOString(),
    wallSeconds: Math.round(totalWall * 10) / 10,
    config: {
      textClients: cfg.textClients, imageClients: cfg.imageClients,
      model: cfg.model, imageModel: cfg.imageModel, imageSize: cfg.imageSize,
      imageSteps: cfg.imageSteps, maxTokens: cfg.maxTokens,
      durationSec: cfg.duration, requestsPerClient: cfg.requests, imageRequestsPerClient: cfg.imageRequests
    },
    text: {
      sent: text.length,
      ok: okText.length,
      failed: text.length - okText.length,
      rps: Math.round((okText.length / totalWall) * 100) / 100,
      ttftMs: summarize(okText.map((r) => r.ttftMs)),
      totalMs: summarize(okText.map((r) => r.totalMs)),
      completionTokens: summarize(okText.map((r) => r.completionTokens).filter((x) => x !== null)),
      emptyAnswers: text.filter((r) => !r.error && r.contentChars === 0).length
    },
    image: {
      sent: image.length,
      ok: okImage.length,
      failed: image.length - okImage.length,
      rpm: Math.round((okImage.length / totalWall) * 60 * 100) / 100,
      totalMs: summarize(okImage.map((r) => r.totalMs)),
      bytes: summarize(okImage.map((r) => r.bytes))
    },
    errors,
    samples,
    records
  };
}

async function warmupText() {
  // Прогрев нужен, чтобы в перцентили не попала загрузка модели (десятки секунд)
  // — иначе первый «клиент» получает задержку в 100 раз больше остальных.
  process.stdout.write('прогрев: первый текстовый запрос (загрузка модели, если её нет)… ');
  const rec = await textRequest('warmup', 'warmup', 1);
  console.log(rec.error ? 'ОШИБКА: ' + rec.error : 'ок, ' + rec.totalMs + ' мс, ttft ' + rec.ttftMs + ' мс');
  return rec;
}

async function warmupImage() {
  process.stdout.write('прогрев: первая генерация картинки (загрузка модели, если её нет)… ');
  const rec = await imageRequest('warmup', 'warmup', 0);
  console.log(rec.error ? 'ОШИБКА: ' + rec.error : 'ок, ' + rec.totalMs + ' мс, ' + rec.bytes + ' байт');
  return rec;
}

(async () => {
  const meta = { startedAt: Date.now() };
  console.log('=== нагрузочный тест балансера ===');
  console.log('  балансер:   ' + cfg.base);
  console.log('  admin:      ' + cfg.admin);
  console.log('  текст:      ' + cfg.textClients + ' клиентов, модель ' + cfg.model +
    ', max_tokens ' + cfg.maxTokens + ', ' + cfg.duration + ' c' +
    (cfg.requests ? ' или ' + cfg.requests + ' запросов на клиента' : ''));
  console.log('  картинки:   ' + cfg.imageClients + ' клиентов, модель ' + cfg.imageModel +
    ', ' + cfg.imageSize + ', ' + cfg.imageSteps + ' шагов');
  console.log('');

  if (cfg.warmup && cfg.textClients) await warmupText();
  if (cfg.warmup && cfg.imageClients) await warmupImage();

  const sampler = makeSampler();
  const records = [];
  const deadline = Date.now() + cfg.duration * 1000;
  console.log('\n--- нагрузка ---');
  await Promise.all([
    cfg.textClients ? runPool('text', cfg.textClients, records, deadline) : Promise.resolve(),
    cfg.imageClients ? runPool('image', cfg.imageClients, records, deadline) : Promise.resolve()
  ]);
  await sampler.finish();

  const rep = report(records, sampler.samples, meta);

  console.log('\n--- ТЕКСТ ---');
  if (rep.text.sent) {
    console.log('  запросов: ' + rep.text.sent + '  ок: ' + rep.text.ok + '  ошибок: ' + rep.text.failed +
      '  пустых ответов: ' + rep.text.emptyAnswers + '  ' + rep.text.rps + ' rps');
    console.log('  TTFT, мс:   ' + JSON.stringify(rep.text.ttftMs));
    console.log('  всего, мс:  ' + JSON.stringify(rep.text.totalMs));
    console.log('  токенов:    ' + JSON.stringify(rep.text.completionTokens));
  } else {
    console.log('  не запускался');
  }

  console.log('\n--- КАРТИНКИ ---');
  if (rep.image.sent) {
    console.log('  запросов: ' + rep.image.sent + '  ок: ' + rep.image.ok + '  ошибок: ' + rep.image.failed +
      '  ' + rep.image.rpm + ' картинок/мин');
    console.log('  время, мс:  ' + JSON.stringify(rep.image.totalMs));
    console.log('  размер, Б:  ' + JSON.stringify(rep.image.bytes));
  } else {
    console.log('  не запускался');
  }

  if (Object.keys(rep.errors).length) {
    console.log('\n--- ОШИБКИ ---');
    for (const [k, n] of Object.entries(rep.errors).sort((a, b) => b[1] - a[1])) {
      console.log('  ' + n + '×  ' + k);
    }
  } else {
    console.log('\n--- ОШИБКИ ---\n  нет');
  }

  const be = rep.samples.filter((s) => s.backends).slice(-1)[0];
  if (be) {
    console.log('\n--- КЛАСТЕР В КОНЦЕ ---');
    for (const b of be.backends) {
      console.log('  ' + (b.id || '?') + '  status=' + b.status + '  active=' + b.active +
        '  total=' + b.total + '  VRAM=' + b.vramUsed + '/' + b.vramTotal + '  GPU=' + b.gpuUtil + '%' +
        '  RAM=' + b.ramUsed + '  loaded=[' + (b.loaded || []).join(',') + ']');
    }
  }

  if (cfg.out) {
    writeFileSync(cfg.out, JSON.stringify(rep, null, 2), 'utf8');
    console.log('\nотчёт: ' + cfg.out);
  }
  process.exit(0);
})().catch((e) => {
  console.error('фатальная ошибка нагрузочного теста:', e);
  process.exit(1);
});

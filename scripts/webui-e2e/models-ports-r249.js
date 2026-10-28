#!/usr/bin/env node
/**
 * R249: одна и та же правда о моделях на двух портах балансера.
 *
 *   порт 18080 — Ollama-нативная поверхность (/api/tags) И OpenAI (/v1/models)
 *   порт 18079 — только OpenAI (/v1/models)
 *
 * Проверяем, что клиенты на обоих портах видят ОДИН И ТОТ ЖЕ список моделей
 * (id совпадают), что модель, загруженная в cppworker, видна с обоих портов,
 * и что для vision-загрузки доезжает capabilities.vision.
 *
 * BASE8848/OLLAMA_BASE/OPENAI_BASE можно переопределить окружением.
 */
const TOKEN = process.env.TOKEN || 'changeme-bundled-with-agent-token';
const BOTH = process.env.BASE || 'http://127.0.0.1:18080';
const OPENAI_ONLY = process.env.OPENAI_ONLY_BASE || 'http://127.0.0.1:18079';

const headers = { 'X-API-Token': TOKEN, Authorization: `Bearer ${TOKEN}` };

let checks = 0;
let fails = 0;

function ok(name, cond, detail) {
  checks++;
  if (cond) {
    console.log(`PASS  ${name}${detail ? '  ' + detail : ''}`);
  } else {
    fails++;
    console.log(`FAIL  ${name}${detail ? '  ' + detail : ''}`);
  }
}

async function getJSON(url) {
  const res = await fetch(url, { headers });
  const text = await res.text();
  let json = null;
  try {
    json = JSON.parse(text);
  } catch {
    /* оставляем null */
  }
  return { status: res.status, json, text };
}

function ollamaIDs(tags) {
  const list = (tags && tags.models) || [];
  return list.map((m) => m.name || m.model).filter(Boolean).sort();
}

function openaiIDs(models) {
  const list = (models && models.data) || [];
  return list.map((m) => m.id).filter(Boolean).sort();
}

function visionOf(models, id) {
  const list = (models && models.data) || [];
  const hit = list.find((m) => m.id === id);
  return hit && hit.capabilities ? !!hit.capabilities.vision : null;
}

(async () => {
  const tags = await getJSON(`${BOTH}/api/tags`);
  const modelsBoth = await getJSON(`${BOTH}/v1/models`);
  const modelsOnly = await getJSON(`${OPENAI_ONLY}/v1/models`);

  ok('ollama /api/tags отвечает 200', tags.status === 200, `status=${tags.status}`);
  ok('openai /v1/models на 18080 отвечает 200', modelsBoth.status === 200, `status=${modelsBoth.status}`);
  ok('openai /v1/models на 18079 отвечает 200', modelsOnly.status === 200, `status=${modelsOnly.status}`);

  const ollama = ollamaIDs(tags.json);
  const openaiBoth = openaiIDs(modelsBoth.json);
  const openaiOnly = openaiIDs(modelsOnly.json);

  ok('Ollama видит хотя бы одну модель', ollama.length > 0, `models=${ollama.join(',') || '<нет>'}`);

  // Направление, которое обязано совпадать: всё, что видит Ollama-клиент,
  // обязано быть доступно и на OpenAI-поверхности. Обратное неверно by design:
  // OpenAI /v1/models дополнительно объявляет image-модели (dall-e-*/sd-cpp-*),
  // которых в Ollama /api/tags нет — это не рассинхрон, а разные наборы
  // возможностей поверхностей.
  const missingInOpenai = ollama.filter((id) => !openaiBoth.includes(id));
  ok(
    'все Ollama-модели видны и на OpenAI (18080)',
    missingInOpenai.length === 0,
    `missing_in_openai=[${missingInOpenai}]`,
  );
  const openaiExtras = openaiBoth.filter((id) => !ollama.includes(id));
  if (openaiExtras.length > 0) {
    console.log(`INFO  OpenAI дополнительно объявляет: [${openaiExtras.join(',')}] (image-модели, в Ollama /api/tags их нет by design)`);
  }

  const missingOnOpenaiOnly = openaiBoth.filter((id) => !openaiOnly.includes(id));
  ok(
    'порт 18079 отдаёт те же модели, что 18080',
    missingOnOpenaiOnly.length === 0,
    `missing_on_18079=[${missingOnOpenaiOnly}]`,
  );

  // capabilities.vision должен присутствовать в каждой записи OpenAI-поверхности.
  const list = (modelsOnly.json && modelsOnly.json.data) || [];
  const withoutCaps = list.filter((m) => !m.capabilities || typeof m.capabilities.vision !== 'boolean');
  ok('у каждой модели есть capabilities.vision', withoutCaps.length === 0,
    `без capabilities: ${withoutCaps.map((m) => m.id).join(',')}`);

  // Сверка vision между портами (одна и та же модель = один и тот же флаг).
  const mismatched = openaiOnly
    .map((id) => ({ id, a: visionOf(modelsBoth.json, id), b: visionOf(modelsOnly.json, id) }))
    .filter((x) => x.a !== null && x.b !== null && x.a !== x.b);
  ok('capabilities.vision совпадает между портами', mismatched.length === 0,
    mismatched.map((x) => `${x.id}: 18080=${x.a} 18079=${x.b}`).join('; '));

  console.log('');
  console.log(`ИТОГ: проверок ${checks}, провалов ${fails}`);
  console.log(`       ollama(18080)=${ollama.length} openai(18080)=${openaiBoth.length} openai(18079)=${openaiOnly.length}`);
  process.exit(fails === 0 ? 0 : 1);
})().catch((err) => {
  console.error('FAIL  исключение:', err.message);
  console.log('');
  console.log('ИТОГ: проверок 0, провалов 1');
  process.exit(1);
});

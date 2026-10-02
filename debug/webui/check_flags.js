// check_flags.js — проверка логики buildLoadedFlags из
// webui/js/modules/gguf-renderer-detail-render.js без браузера.
//
// Функция чистая (не использует Utils/_/DOM), поэтому её можно вырезать из
// модуля и выполнить в Node на реальных данных стенда.
const fs = require('fs');
const path = 'webui/js/modules/gguf-renderer-detail-render.js';
const src = fs.readFileSync(path, 'utf8');

const start = src.indexOf('function buildLoadedFlags');
if (start < 0) {
  console.error('buildLoadedFlags не найдена в ' + path);
  process.exit(1);
}
// Ищем закрывающую скобку функции по балансу.
let depth = 0, end = -1;
for (let i = src.indexOf('{', start); i < src.length; i++) {
  if (src[i] === '{') depth++;
  else if (src[i] === '}') { depth--; if (depth === 0) { end = i + 1; break; } }
}
const fnSrc = src.slice(start, end);
const buildLoadedFlags = new Function(fnSrc + '; return buildLoadedFlags;')();

function show(title, m, rt) {
  const flags = buildLoadedFlags(m, rt);
  console.log(title + ': ' + flags.map(f => f.label + '=' + f.value + (f.tone !== 'normal' ? '[' + f.tone + ']' : '')).join('  '));
}

// 1. Реальные данные стенда (балансер отдаёт всё в camelCase).
show('стенд (parallel=1, kv=q4_0, reasoning on)', {
  name: 'gemma-4-E4B-it-Q4_K_M',
  contextLength: 65536,
  context_per_seq: 65536,
  parallel: 1,
  maxSlots: 1,
  kvCacheType: 'q4_0',
  numGpuLayers: 0,
  nLayers: 42,
  reasoningEnabled: true,
  quantization: 'Q4_K_M',
  capabilities: { reasoning: true },
}, { gpu_layers: 42, n_layers: 42, flash_attn_type: 1, use_mmap: true, batched_parallel: false });

// 2. KV-cache не задан → обязан быть warn (жалоба «всегда грузит с f16»).
show('kv не задан', { name: 'm', contextLength: 32768, parallel: 2 }, null);

// 3. Reasoning выключен → off, не on.
show('reasoning off', { name: 'm', reasoningEnabled: false, kvCacheType: 'f16' }, null);

// 4. Частичный offload → warn у gpu_layers.
show('partial offload', { name: 'm', numGpuLayers: 20, nLayers: 42, kvCacheType: 'q8_0' }, { gpu_layers: 20, n_layers: 42 });

// 5. Пустые данные → не должно падать.
show('пусто', {}, null);

// Ассерты (красный/зелёный результат для отчёта).
let fails = 0;
function assert(cond, msg) { if (!cond) { console.log('FAIL: ' + msg); fails++; } }

const f1 = buildLoadedFlags({
  contextLength: 65536, context_per_seq: 32768, parallel: 2, maxSlots: 2,
  kvCacheType: 'q4_0', reasoningEnabled: true, nLayers: 42, quantization: 'Q4_K_M',
}, { gpu_layers: 42, flash_attn_type: 1, use_mmap: true });
const by = {}; f1.forEach(f => by[f.label] = f);
assert(by.ctx && by.ctx.value === '65536 (32768/slot)', 'ctx должен показывать окно на слот');
assert(by.parallel && by.parallel.value === '2 (max 2)', 'parallel/maxSlots');
assert(by.kv && by.kv.value === 'q4_0' && by.kv.tone === 'normal', 'kv из балансера');
assert(by.reasoning && by.reasoning.value === 'on' && by.reasoning.tone === 'ok', 'reasoning=on');
assert(by.gpu_layers && by.gpu_layers.value === '42/42', 'gpu_layers/N');

const f2 = buildLoadedFlags({ contextLength: 8192 }, null);
const by2 = {}; f2.forEach(f => by2[f.label] = f);
assert(by2.kv && by2.kv.value === 'f16' && by2.kv.tone === 'warn', 'kv по умолчанию = f16 + warn');
assert(!by2.reasoning, 'reasoning отсутствует, если состояние неизвестно');

const f3 = buildLoadedFlags({ reasoningEnabled: false }, null);
assert(f3.find(f => f.label === 'reasoning').tone === 'off', 'reasoning off = приглушённый');

console.log(fails === 0 ? 'OK: все проверки пройдены' : ('ПРОВАЛЕНО проверок: ' + fails));
process.exit(fails === 0 ? 0 : 1);

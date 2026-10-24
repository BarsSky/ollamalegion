// scripts/webui-e2e/tests/params-compare.test.js — R148 (2026-10-16).
//
// Запуск: node scripts/webui-e2e/tests/params-compare.test.js
//
// ПОЧЕМУ ЭТИ ТЕСТЫ. Каждый блок ниже — реальный дефект, из-за которого инструмент
// проверки параметров ВРАЛ на живом стенде (и оператор видел ложный FAIL/PASS):
//
//   1. uptime сравнивался СТРОКАМИ: "1h51m58s" >= "1h51m9s" лексикографически
//      даёт false ('5' < '9'), и исправный воркер объявлялся перезапущенным.
//   2. Пока модель грузится, балансер отдаёт ПЛЕЙСХОЛДЕР (state=loading, нули);
//      сравнение с ним давало ложные FAIL сразу после применения параметров.
//   3. Факты читались из ключа models/data, а балансер отдаёт loaded_models —
//      список был пуст, и инструмент молча уходил на запасной источник, где нет
//      gpu_layers/flash_attn/use_mmap, объявляя это «воркер не сообщает».
//   4. numGpuLayers=-1 — это «все слои (авто)», а воркер сообщает ФАКТИЧЕСКОЕ
//      число (memfit понижает под свободную VRAM); flashAttn — три-состояние
//      (-1 авто / 0 / 1). Прямое сравнение давало и ложный PASS (flashAttn=true
//      против -1), и ложный FAIL (numGpuLayers), и ложный FAIL на профиле.
'use strict';

const assert = require('assert');
const pc = require('../lib/params-compare.js');

let checks = 0;
function ok(cond, label, detail) {
    checks++;
    assert.ok(cond, label + (detail ? ' :: ' + detail : ''));
    console.log('  PASS  ' + label + (detail ? ' — ' + detail : ''));
}
function eq(actual, expected, label) {
    checks++;
    assert.deepStrictEqual(actual, expected, label + ' :: получено ' + JSON.stringify(actual));
    console.log('  PASS  ' + label);
}

// ── 1. uptime: строки сравнивать нельзя ──────────────────────────────────────
console.log('=== 1. uptimeSeconds: разбор строки воркера ===');
const before = '1h51m9.765022814s';
const after = '1h51m58.240859429s';
eq(pc.uptimeSeconds('47m51s'), 2871, 'минуты без часов');
eq(pc.uptimeSeconds('2h0m1s'), 7201, 'часы и минуты');
eq(pc.uptimeSeconds(123.5), 123.5, 'число проходит как есть');
eq(pc.uptimeSeconds(null), null, 'null -> null');
eq(pc.uptimeSeconds('ерунда'), null, 'мусор -> null');
ok(pc.uptimeSeconds(after) > pc.uptimeSeconds(before),
    'после перезапуска uptime БОЛЬШЕ до',
    pc.uptimeSeconds(after) + ' > ' + pc.uptimeSeconds(before));
// Ловушка, ради которой всё это: та же пара строк при сравнении строками даёт
// ЛОЖНОЕ «после меньше до», то есть исправный воркер «перезапускался».
ok(!(after >= before),
    'ловушка подтверждена: сравнение строками дало бы ложный «перезапуск»',
    JSON.stringify(after) + ' >= ' + JSON.stringify(before) + ' === false');

// ── 2. Выбор строки runtime-конфига (плейсхолдер загрузки) ───────────────────
console.log('\n=== 2. selectLoadedRow: загруженный инстанс, а не плейсхолдер ===');
const withPlaceholder = [
    { name: 'gemma-4-E4B-it-Q4_K_M', state: 'loading', context_size: 0, n_layers: 0 },
    { name: 'gemma-4-E4B-it-Q4_K_M', state: 'loaded', context_size: 8192, n_layers: 42 },
];
const picked = pc.selectLoadedRow(withPlaceholder, 'gemma-4-E4B-it-Q4_K_M');
ok(picked.row && picked.row.state === 'loaded' && picked.row.context_size === 8192,
    'выбрана строка loaded с непустым окном', JSON.stringify(picked.states));
const onlyPlaceholder = pc.selectLoadedRow([withPlaceholder[0]], 'gemma-4-E4B-it-Q4_K_M');
ok(onlyPlaceholder.row === null, 'только плейсхолдер -> фактов нет (сравнение пропускается)',
    onlyPlaceholder.states);
ok(pc.selectLoadedRow(withPlaceholder, 'gemma-4-E4B-it-Q4_K_M.gguf').row !== null,
    'имя с .gguf находит инстанс без расширения');

// ── 2b. Две КОПИИ одной модели (мастер + донор) ─────────────────────────────
console.log('\n=== 2b. selectLoadedRow: копий несколько ===');
const twoCopies = [
    { name: 'gemma-4-E4B-it-Q4_K_M', backend: 'cppworker-cyborg-bundled-agent', state: 'loaded', context_size: 8192, context_per_seq: 8192, parallel: 1, n_layers: 42 },
    { name: 'gemma-4-E4B-it-Q4_K_M', backend: 'cppworker-gpu-bundled-agent', state: 'loaded', context_size: 8192, context_per_seq: 4096, parallel: 2, n_layers: 42 },
];
const amb = pc.selectLoadedRow(twoCopies, 'gemma-4-E4B-it-Q4_K_M');
ok(amb.ambiguous === true, 'неоднозначность ВИДНА наружу (а не спрятана)',
    'копий: ' + amb.backends.length + ' [' + amb.backends.join(', ') + ']');
const own = pc.selectLoadedRow(twoCopies, 'gemma-4-E4B-it-Q4_K_M', 'cppworker-gpu-bundled-agent');
eq(own.row.backend, 'cppworker-gpu-bundled-agent', 'с BackendID берётся СВОЯ копия');
eq(own.row.context_per_seq, 4096, 'и её параметры (per-seq 4096, а не 8192)');
ok(own.ambiguous === false, 'своя копия одна -> неоднозначности нет');
const other = pc.selectLoadedRow(twoCopies, 'gemma-4-E4B-it-Q4_K_M', 'cppworker-cyborg-bundled-agent');
eq(other.row.parallel, 1, 'для донора берётся копия донора');
// Живой дефект: без фильтра инструмент мог сравнить факт донора (8192/1) с
// параметрами карточки мастера (4096/2) и выдать ложный FAIL — показываем, что
// теперь копии различимы.
ok(amb.row.context_per_seq !== own.row.context_per_seq,
    'копии различаются по параметрам — значит сравнение без фильтра даёт ложный вывод',
    'первая попавшаяся=' + amb.row.context_per_seq + ', копия мастера=' + own.row.context_per_seq);

// ── 3. Маппинг полей воркера (snake_case -> факты карточки) ─────────────────
console.log('\n=== 3. mapRuntimeRow: поля, которые раньше «не сообщались» ===');
const fact = pc.mapRuntimeRow({
    name: 'gemma-4-E4B-it-Q4_K_M', state: 'loaded',
    context_size: 8192, context_per_seq: 4096, gpu_layers: 19, n_layers: 42,
    device_layers_exact: [{ device: 'CPU', count: 24 }, { device: 'CUDA0', count: 19 }],
    batch_size: 512, parallel: 2, kv_cache_type: 'q4_0',
    flash_attn: -1, use_mmap: true, reasoning_enabled: true,
});
eq(fact.numGpuLayers, 19, 'gpu_layers -> numGpuLayers');
eq(fact.nLayers, 42, 'n_layers -> nLayers (нужно для «все слои или частично»)');
eq(fact.flashAttnRaw, -1, 'сырое три-состояние flash_attn сохранено');
eq(fact.useMmap, true, 'use_mmap -> useMmap');
eq(fact.contextPerSeq, 4096, 'context_per_seq -> contextPerSeq');
ok(Array.isArray(fact.deviceLayers) && fact.deviceLayers.length === 2,
    'device_layers_exact -> deviceLayers (раскладка по устройствам)');

// ── 4. Сравнение: три категории и их границы ────────────────────────────────
// R151: копия НЕ того бэкенда, что запрошен, обязана быть видна наружу.
const notMine = pc.selectLoadedRow(twoCopies, 'gemma-4-E4B-it-Q4_K_M', 'cppworker-nonexistent');
ok(notMine.backendMatched === false, 'чужая копия помечена как НЕсовпадение с запрошенным бэкендом',
    'просили cppworker-nonexistent, получили ' + notMine.backend);
ok(pc.selectLoadedRow(twoCopies, 'gemma-4-E4B-it-Q4_K_M', 'cppworker-cyborg-bundled-agent').backendMatched === true,
    'своя копия -> совпадение');
ok(pc.selectLoadedRow(twoCopies, 'gemma-4-E4B-it-Q4_K_M').backendMatched === true,
    'без фильтра по бэкенду претензий нет (совпадение не требуется)');
console.log('\n=== 4. compareWanted: FAIL / WARN / адаптация ===');
const wanted = { contextLength: 8192, contextPerSeq: 4096, numGpuLayers: -1, parallel: 2, flashAttn: true, useMmap: true };

let r = pc.compareWanted(wanted, {
    contextLength: 8192, contextPerSeq: 4096, numGpuLayers: 42, nLayers: 42,
    parallel: 2, flashAttn: 1, useMmap: true,
});
eq(r.diffs.length, 0, 'полное совпадение -> без расхождений');
eq(r.adaptations.length, 0, 'все слои на GPU (42/42) -> без «адаптации»');

r = pc.compareWanted(wanted, {
    contextLength: 8192, contextPerSeq: 4096, numGpuLayers: 19, nLayers: 42,
    deviceLayers: [{ device: 'CPU', count: 24 }, { device: 'CUDA0', count: 19 }],
    parallel: 2, flashAttn: 1, useMmap: true,
});
eq(r.diffs.length, 0, 'частичная выгрузка — НЕ дефект применения');
eq(r.adaptations.length, 1, 'частичная выгрузка попадает в отдельную категорию');
ok(/19 из 42/.test(r.adaptations[0]) && /CPU: 24/.test(r.adaptations[0]),
    'в адаптации названы числа и раскладка', r.adaptations[0]);

// Регрессия: профиль хранит -1, сравнивать её с фактом -1 надо как совпадение.
r = pc.compareWanted({ numGpuLayers: -1 }, { numGpuLayers: -1 });
eq(r.diffs.length, 0, 'профиль -1 против -1 -> совпадение (раньше ложный FAIL)');
eq(r.adaptations.length, 0, 'и это не «адаптация»');

// Регрессия: flashAttn=true против «авто» (-1) — это НЕ подтверждение.
r = pc.compareWanted({ flashAttn: true }, { flashAttn: -1, flashAttnRaw: -1 });
eq(r.diffs.length, 0, 'flashAttn «авто» — не FAIL');
eq(r.unreported.length, 1, 'flashAttn «авто» — не подтверждение (раньше ложный PASS)');
ok(/авто/.test(r.unreported[0]), 'в причине сказано «авто»', r.unreported[0]);

// Настоящее расхождение обязано остаться FAIL.
r = pc.compareWanted({ contextPerSeq: 4096, parallel: 2 }, { contextPerSeq: 8192, parallel: 1 });
eq(r.diffs.length, 2, 'реальное расхождение -> два FAIL');
ok(/contextPerSeq/.test(r.diffs[0]) || /contextPerSeq/.test(r.diffs[1]), 'назван параметр', r.diffs.join('; '));

// bool-поля из профиля/строки и мусор.
eq(pc.normalize('useMmap', 'true'), true, 'строка "true" -> true');
eq(pc.normalize('useMmap', 0), false, 'число 0 -> false');
eq(pc.normalize('flashAttn', 'auto'), null, 'строка "auto" -> не подтверждение');
eq(pc.normalize('contextLength', '8192'), 8192, 'числовая строка -> число');

// ── 5. Несколько копий модели: чей промпт сверять ───────────────────────────
console.log('\n=== 5. pickNewestPrompt: свежий снапшот после начала запроса ===');
const t0 = Date.parse('2026-10-16T15:00:00Z');
const snaps = [
    { backend: 'master', timestamp: '2026-10-16T14:59:30Z', prompt_chars: 80 },   // до запроса
    { backend: 'donor', timestamp: '2026-10-16T15:00:02Z', prompt_chars: 1068 },  // ответил он
    { backend: 'master', timestamp: '2026-10-16T14:59:55Z', prompt_chars: 357 },
];
const newest = pc.pickNewestPrompt(snaps, t0);
ok(newest && newest.backend === 'donor', 'выбран воркер, который реально обслужил',
    newest ? newest.backend + ' (' + newest.prompt_chars + ' симв)' : 'null');
eq(newest.prompt_chars, 1068, 'и именно его промпт');
eq(pc.pickNewestPrompt([], t0), null, 'нет снапшотов -> null (честное «не знаю»)');
eq(pc.pickNewestPrompt([{ backend: 'm', timestamp: '2026-10-16T14:00:00Z' }], t0), null,
    'все снапшоты старше запроса -> null, а не вердикт по чужому запросу');

// ── 6. Привязка промпта к СВОЕМУ запросу по метке ───────────────────────────
console.log('\n=== 6. pickPromptWithMarker: чужой запрос не подходит ===');
const withMarker = [
    { __worker: 'master', prompt_head: '<|turn>system Probe-R150-7 скажи ок', prompt_chars: 345 },
    { __worker: 'donor', prompt_head: '<|turn>system Probe-R150-9 скажи ок', prompt_chars: 80 },
];
const mine = pc.pickPromptWithMarker(withMarker, 'Probe-R150-9');
ok(mine && mine.__worker === 'donor', 'найден снапшот ИМЕННО нашего запроса',
    mine ? mine.__worker + ' / ' + mine.prompt_chars : 'null');
ok(pc.pickPromptWithMarker(withMarker, 'Probe-R150-7').__worker === 'master',
    'метка различает кейсы с одинаковой формой запроса');
eq(pc.pickPromptWithMarker(withMarker, 'Probe-R150-none'), null,
    'метки нет ни у кого -> null (паритет НЕ проверяется, а не сравнение чужих)');
eq(pc.pickPromptWithMarker(withMarker, ''), null, 'пустая метка -> null');

console.log('\nИТОГ: проверок ' + checks + ', провалов 0');

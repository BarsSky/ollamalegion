// monitor-aggregate-vram.test.js — R-Image follow-up (2026-10-07).
//
// ЗАЧЕМ ЭТОТ ТЕСТ. На одной рабочей машине подняты и cppworker (текст), и
// imageworker (картинки). Оба агента читают nvidia-smi ОДНОЙ видеокарты и
// присылают балансеру ОДНИ И ТЕ ЖЕ memoryTotal/memoryUsed. Наивная сумма по
// бэкендам показывала удвоенную память: в шапке Monitor «VRAM 14.5/16G» при
// реальных 8 GB на карте, и баннер «VRAM заполнена» срабатывал на здоровой
// системе.
//
// ЧТО ПРОВЕРЯЕМ. Хелпер window.MonitorAggregates.aggregateVRAM:
//   1. два бэкенда с ОДНИМ UUID карты считаются одной картой (не суммируются);
//   2. два бэкенда с РАЗНЫМИ UUID — это две карты, память складывается;
//   3. без UUID (старый агент) два бэкенда одного хоста = одна карта (дедуп по
//      хосту), разные хосты — складываются;
//   4. несколько карт у одной машины (несколько UUID в одном бэкенде) дедупятся
//      согласованно с другим бэкендом той же машины;
//   5. бэкенд без метрик VRAM не влияет на итог.
//
// Запуск: node webui/js/modules/monitor-aggregate-vram.test.js
// (включён в `npm run test:webui`)

'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');
const vm = require('vm');

const SRC = path.join(__dirname, 'monitor-metrics.js');

// Загружаем модуль в изолированный контекст: нужен только window.MonitorAggregates.
// WebSocket в Node нет — подставляем заглушку, сам модуль к сети не подключается
// (connect() вызывается из MonitorApp, а не на верхнем уровне).
const sandbox = {
  console: console,
  setTimeout: setTimeout,
  clearTimeout: clearTimeout,
  WebSocket: function () {},
  window: {},
};
sandbox.window.location = { protocol: 'http:', host: 'localhost' };
vm.createContext(sandbox);
vm.runInContext(fs.readFileSync(SRC, 'utf8'), sandbox, { filename: SRC });

const agg = sandbox.window.MonitorAggregates && sandbox.window.MonitorAggregates.aggregateVRAM;
assert.strictEqual(typeof agg, 'function',
  'monitor-metrics.js обязан экспортировать window.MonitorAggregates.aggregateVRAM');

const UUID_A = 'GPU-6f8d5f3b-1dde-cbcf-8c26-4a1d0dbead34';
const UUID_B = 'GPU-aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee';

// Бэкенд с метриками одной карты. host — как его видит балансер (имя контейнера).
function backend(id, host, totalGB, usedGB, uuids) {
  const gpu = { memoryTotal: totalGB * 1024, memoryUsed: usedGB * 1024 };
  if (uuids) gpu.uuids = uuids;
  return { id: id, host: host, vram: { totalGB: totalGB, usedGB: usedGB }, gpu: gpu };
}

// --- 1. Одна физическая карта, два бэкенда: НЕ складывать -------------------
{
  const res = agg([
    backend('cppworker-gpu-bundled-agent', 'cppworker-gpu', 8, 6.9, [UUID_A]),
    backend('imageworker', 'imageworker', 8, 6.5, [UUID_A]),
  ]);
  assert.strictEqual(res.groups, 1, 'два бэкенда с одним UUID = одна карта');
  assert.strictEqual(res.totalGB, 8, 'всего VRAM должно быть 8 GB, а не 16');
  // Максимум в группе: агенты читают один счётчик и могут разойтись на МБ.
  assert.strictEqual(res.usedGB, 6.9, 'занято — максимум в группе, а не сумма');
  assert.strictEqual(res.deduped, true, 'факт дедупа обязан быть виден');
  console.log('ok 1 — один UUID на двух бэкендах: 8 GB, а не 16');
}

// --- 2. Разные карты: складывать ------------------------------------------
{
  const res = agg([
    backend('llm-1', 'host-a', 8, 6, [UUID_A]),
    backend('llm-2', 'host-b', 8, 2, [UUID_B]),
  ]);
  assert.strictEqual(res.groups, 2, 'разные UUID = две карты');
  assert.strictEqual(res.totalGB, 16, 'две карты по 8 GB складываются');
  assert.strictEqual(res.usedGB, 8, 'занято суммируется по разным картам');
  console.log('ok 2 — разные UUID: 16 GB, память складывается');
}

// --- 3. Без UUID: дедуп по хосту -----------------------------------------
{
  const same = agg([
    backend('llm', 'workstation', 8, 7, null),
    backend('img', 'workstation', 8, 1, null),
  ]);
  assert.strictEqual(same.groups, 1, 'без UUID два бэкенда одного хоста = одна карта');
  assert.strictEqual(same.totalGB, 8, 'память одной карты не удваивается и без UUID');

  const diff = agg([
    backend('llm', 'host-a', 8, 7, null),
    backend('img', 'host-b', 8, 1, null),
  ]);
  assert.strictEqual(diff.totalGB, 16, 'разные хосты без UUID — разные машины');
  console.log('ok 3 — без UUID дедуп по хосту');
}

// --- 4. Несколько карт у машины ------------------------------------------
{
  const res = agg([
    backend('llm', 'host-a', 16, 10, [UUID_A, UUID_B]),
    backend('img', 'host-a', 16, 4, [UUID_A, UUID_B]),
  ]);
  assert.strictEqual(res.groups, 1, 'один и тот же набор UUID = одна машина');
  assert.strictEqual(res.totalGB, 16, 'набор из двух карт считается один раз');
  console.log('ok 4 — набор UUID дедупится как одна машина');
}

// --- 5. Бэкенд без VRAM-метрик не влияет --------------------------------
{
  const res = agg([
    backend('llm', 'host-a', 8, 6, [UUID_A]),
    { id: 'cpu-only', host: 'host-c', vram: { totalGB: 0, usedGB: 0 }, gpu: {} },
    { id: 'no-metrics', host: 'host-d' },
  ]);
  assert.strictEqual(res.groups, 1, 'бэкенды без VRAM в группы не попадают');
  assert.strictEqual(res.totalGB, 8, 'нулевые метрики не добавляют памяти');
  console.log('ok 5 — бэкенды без метрик VRAM игнорируются');
}

// --- 6. Пустой и невалидный вход -----------------------------------------
{
  assert.strictEqual(agg([]).totalGB, 0, 'пустой список — 0');
  assert.strictEqual(agg(null).totalGB, 0, 'null — 0, без исключения');
  assert.strictEqual(agg(undefined).totalGB, 0, 'undefined — 0, без исключения');
  console.log('ok 6 — пустой/невалидный вход не роняет Monitor');
}

console.log('\nmonitor-aggregate-vram: все проверки пройдены');

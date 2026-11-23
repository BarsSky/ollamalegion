#!/usr/bin/env node
/*
 * gate-notices-queue-r700.test.js — R700 (2026-11-24)
 *
 * ЗАЧЕМ. Причина ожидания заявки (диск / RAM / VRAM / окно / слоты) до R700
 * жила только в логе балансера (`why_queued`) и в теле 503. На странице
 * «Очередь» её не было: оператор видел «Ждёт слот» и шёл искать причину в
 * docker logs. Живой архив второго стенда: `free_disk_mb=9480 < min=10240` —
 * свободный узел исключён, а страница об этом молчала.
 *
 * ЧТО ПРОВЕРЯЕТСЯ:
 *   1. queuePage рисует карточку причин из /api/v1/queue/stats → gates.notices:
 *      гейт, узел и ТЕКСТ С ЧИСЛАМИ (9480/10240) — без чисел причина
 *      бесполезна: непонятно, сколько освободить;
 *   2. причина видна РЯДОМ С ПОЗИЦИЕЙ ожидающего (строка статуса), а не только
 *      в отдельной карточке;
 *   3. нет причин — карточка скрыта (пустая таблица читалась бы как «причина
 *      есть, но её не показали»);
 *   4. счётчик «×N» показывает, сколько наблюдений схлопнуто (дедуп на сервере);
 *   5. ключи подписей существуют в ru.js и en.js (иначе в панели появился бы
 *      литерал `queue.gates_title`).
 *
 * НЕ-ВАКУУМНОСТЬ (проверено вручную, хук ниже):
 *   git show 0b5656b6:webui/js/modules/renderers-logs.js > /tmp/old-logs.js
 *   RENDERERS_LOGS_TARGET=/tmp/old-logs.js node webui/tests/gate-notices-queue-r700.test.js
 *   → тест КРАСНЕЕТ: старая страница причин не знает (queueGatesBody пуст).
 *
 * ЗАПУСК: node webui/tests/gate-notices-queue-r700.test.js
 */
'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');

const REPO_ROOT = path.resolve(__dirname, '..', '..');
const MODULES = path.join(REPO_ROOT, 'webui', 'js', 'modules');
const TARGET = process.env.RENDERERS_LOGS_TARGET
  ? path.resolve(process.env.RENDERERS_LOGS_TARGET)
  : path.join(MODULES, 'renderers-logs.js');

let failures = 0;
function check(name, fn) {
  try {
    fn();
    console.log('  ✓ ' + name);
  } catch (e) {
    failures++;
    console.error('  ✗ ' + name + ' — ' + (e && e.message));
  }
}
function assert(cond, msg) {
  if (!cond) throw new Error(msg);
}

// --- минимальный DOM (тот же приём, что в renderers-queue.test.js) -----------
function makeEl(id) {
  return {
    id: id || '',
    innerHTML: '',
    textContent: '',
    value: '',
    hidden: false,
    style: {},
    dataset: {},
    classList: { add() {}, remove() {}, contains() { return false; } },
    setAttribute() {},
    getAttribute() { return null; },
    appendChild() {},
    removeChild() {},
    addEventListener() {},
    querySelector() { return null; },
    querySelectorAll() { return []; },
  };
}
const byId = {};
global.document = {
  createElement() {
    let text = '';
    const el = makeEl('');
    Object.defineProperty(el, 'textContent', { get() { return text; }, set(v) { text = String(v); } });
    Object.defineProperty(el, 'innerHTML', {
      get() {
        return text.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
      },
    });
    return el;
  },
  getElementById(id) { return byId[id] || (byId[id] = makeEl(id)); },
  querySelector() { return null; },
  querySelectorAll() { return []; },
  addEventListener() {},
  removeEventListener() {},
};
global.window = global;

// --- i18n: берём НАСТОЯЩИЙ ru-пакет (проверка 5 — ключи существуют) ---------
vm.runInThisContext(fs.readFileSync(path.join(REPO_ROOT, 'webui', 'js', 'i18n', 'ru.js'), 'utf8'));
const RU = global.window.I18N_RU;
global.window.I18N = {
  t(k, vars) {
    let s = Object.prototype.hasOwnProperty.call(RU, k) ? RU[k] : k;
    if (vars) Object.keys(vars).forEach(function (n) { s = s.split('{' + n + '}').join(String(vars[n])); });
    return s;
  },
  getLang() { return 'ru'; },
};

// --- загрузка рендереров в порядке index.html -------------------------------
require(path.join(MODULES, 'utils.js'));
global.Utils = global.window.Utils;
require(path.join(MODULES, 'renderers-params.js'));
require(path.join(MODULES, 'renderers-pages.js'));
if (TARGET === path.join(MODULES, 'renderers-logs.js')) {
  require(TARGET);
} else {
  // Хук не-вакуумности: тот же код-путь, но произвольная ревизия файла.
  vm.runInThisContext(fs.readFileSync(TARGET, 'utf8'));
}
require(path.join(MODULES, 'renderers-cluster.js'));
require(path.join(MODULES, 'renderers.js'));
const R = global.window.Renderers;
assert(R && typeof R.queuePage === 'function', 'Renderers.queuePage должен быть экспортирован');

// --- данные стенда ----------------------------------------------------------
// Живой архив второго стенда: A10-126 свободен по слотам, но диск 9480 < 10240;
// ASTRA-I9 не может поднять 27B (VRAM), окно на втором узле меньше заявленного.
function livePayload(notices) {
  return {
    admission: {
      enabled: true,
      waiting: 2,
      waiting_by_backend: { 'A10-126': 1, 'A10-127': 1 },
      waiting_detail: [
        { session: 'sess-a', sessionKnown: true, protocol: '/api/chat', backend: 'A10-126', waitedMs: 4000, position: 1 },
        { session: 'sess-b', sessionKnown: true, protocol: '/v1/chat/completions', backend: 'A10-127', waitedMs: 2000, position: 2 },
      ],
      served_total: 10, timeout_total: 1, avg_wait_ms: 3000, wait_max_sec: 300,
      active_sessions: 1,
    },
    current_size: 2,
    processed_total: 10,
    max_size: 100,
    gates: notices ? { notices: notices, min_free_disk_mb: 10240, wait_max_sec: 300 } : undefined,
  };
}

const diskNotice = {
  gate: 'disk',
  backend_id: 'A10-126',
  severity: 'warning',
  message: 'Запрос ждёт: свободный узел A10-126 заблокирован по диску (свободно 9480 МБ, минимум 10240 МБ) — освободите место или снизьте порог в настройках',
  action: 'освободите место на узле или снизьте порог «Min Free Disk» в «Настройках → Лимиты ресурсов»',
  count: 3,
  numbers: { free_mb: 9480, min_free_mb: 10240 },
  first_seen: '2026-11-24T10:00:00Z',
  last_seen: '2026-11-24T10:01:00Z',
};
const windowNotice = {
  gate: 'window',
  backend_id: 'A10-127',
  severity: 'warning',
  message: 'Ожидание: окно свободной копии на узле A10-127 меньше заявленного (65536 < 131072) — заявка ждёт слот своей копии',
  count: 1,
  numbers: { window_have: 65536, window_need: 131072 },
};

// ===========================================================================
check('карточка причин: гейт, узел и ЧИСЛА из gates.notices', function () {
  R.queuePage(livePayload([diskNotice, windowNotice]), [], []);
  const card = byId['queueGateCard'];
  assert(card && card.hidden === false, 'карточка причин скрыта, хотя gates.notices не пуст');
  const html = byId['queueGatesBody'].innerHTML;
  assert(html.indexOf('9480') !== -1 && html.indexOf('10240') !== -1,
    'в причинах нет чисел диска (9480/10240): причина без чисел не говорит, сколько освободить — ' + html);
  assert(html.indexOf('A10-126') !== -1 && html.indexOf('A10-127') !== -1,
    'в причинах не названы узлы: ' + html);
  assert(html.indexOf('65536') !== -1 && html.indexOf('131072') !== -1,
    'в причинах нет чисел окна (65536 < 131072): ' + html);
  assert(html.indexOf('×3') !== -1,
    'нет счётчика схлопнутых наблюдений «×3» (дедуп на сервере должен быть виден): ' + html);
  // Подпись гейта переведена, а не машинный код.
  assert(html.indexOf('диск') !== -1, 'подпись гейта не переведена (ожидалось «диск»): ' + html);
  assert(html.indexOf('>disk<') === -1 && html.indexOf('queue.gates_title') === -1,
    'в разметку попал машинный код или непереведённый ключ: ' + html);
  assert(String(byId['queueGateCount'].textContent) === '2',
    'счётчик карточки = ' + byId['queueGateCount'].textContent + ', want 2');
});

check('причина видна рядом с позицией ожидающего, а не только в карточке', function () {
  R.queuePage(livePayload([diskNotice, windowNotice]), [], []);
  const rows = byId['queueTasksBody'].innerHTML;
  assert(rows.indexOf('9480') !== -1 && rows.indexOf('10240') !== -1,
    'в строке ожидающего нет причины с числами диска — оператору придётся сопоставлять таблицы вручную: ' + rows);
  assert(rows.indexOf('65536') !== -1,
    'у позиции второго ожидающего нет причины по окну: ' + rows);
});

check('нет причин — карточка скрыта и не рисует строк', function () {
  R.queuePage(livePayload(null), [], []);
  assert(byId['queueGateCard'].hidden === true,
    'карточка причин показана при пустом gates — пустая таблица выглядит как «причина есть, но её не показали»');
  assert(byId['queueGatesBody'].innerHTML === '',
    'при пустых причинах в таблицу что-то попало: ' + byId['queueGatesBody'].innerHTML);
  assert(String(byId['queueGateCount'].textContent) === '0', 'счётчик причин должен быть 0');
});

check('мусор в gates не роняет отрисовку', function () {
  R.queuePage({ admission: { enabled: true, waiting: 0, waiting_detail: [], waiting_by_backend: {} }, gates: { notices: [null, {}, { gate: 'disk' }] } }, [], []);
  const html = byId['queueGatesBody'].innerHTML;
  assert(html.indexOf('undefined') === -1, 'в разметку попало undefined: ' + html);
});

check('ключи подписей есть в ru.js и en.js (симметрия)', function () {
  const ru = fs.readFileSync(path.join(REPO_ROOT, 'webui', 'js', 'i18n', 'ru.js'), 'utf8');
  const en = fs.readFileSync(path.join(REPO_ROOT, 'webui', 'js', 'i18n', 'en.js'), 'utf8');
  ['queue.gates_title', 'queue.gates_hint', 'queue.gates_col_gate', 'queue.gates_col_reason',
    'queue.gate.disk', 'queue.gate.ram', 'queue.gate.vram', 'queue.gate.window', 'queue.gate.slots'].forEach(function (key) {
    const re = new RegExp('"' + key.replace(/\./g, '\\.') + '"\\s*:');
    assert(re.test(ru), 'ключа ' + key + ' нет в ru.js');
    assert(re.test(en), 'ключа ' + key + ' нет в en.js');
  });
});

// ===========================================================================
if (failures > 0) {
  console.error('\nFAIL: ' + failures + ' проверок красные (файл: ' + TARGET + ')');
  process.exit(1);
}
console.log('\nOK: все проверки причин ожидания зелёные');

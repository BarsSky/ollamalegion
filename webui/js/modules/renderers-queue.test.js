// renderers-queue.test.js — R91 (2026-10-08).
//
// Запуск: node webui/js/modules/renderers-queue.test.js
//
// ЖАЛОБА ОПЕРАТОРА: «при большом количестве сессий на два бэкенда совершенно не
// отображается правильно состояние очереди, от чего возникает вопрос — работает
// ли она». Разбор на живом стенде:
//
//   GET /api/v1/queue/details → {"all":[],"pending":[],"processing":[],"total":0}
//   GET /api/v1/queue/stats   → admission{"enabled":true,"served_total":42,
//                                "waited_total":42,"avg_wait_ms":13926,"waiting":2}
//
// То есть 42 запроса прождали в среднем 13.9 с, а страница «Очередь» рисовала
// ЛЕГАСИ-очередь (current_size/processed_total) и показывала «Очередь пуста»,
// «Выполнено 0», «Среднее ожидание —». Отсюда и вопрос, работает ли очередь.
//
// Тест фиксирует:
//   1. queueView берёт цифры из admission, когда он включён (а не из легаси-полей);
//   2. при выключенном admission остаются легаси-цифры (обратная совместимость);
//   3. поимённый список ожидающих и разбивка по бэкендам согласованы;
//   4. queuePage переключает подписи плиток и рисует строки ожидающих.

'use strict';

const assert = require('assert');

// --- минимальный DOM (тот же приём, что в renderers-image.test.js) ------------
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
    // Utils.escapeHtml устроен как div.textContent = s; return div.innerHTML —
    // поэтому createElement обязан эмулировать эту связку, иначе экранирование
    // вернёт пустую строку и тест «пройдёт» на пустой разметке.
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

const I18N_MAP = {
    'queue.position': 'Позиция',
    'queue.max_size': 'Максимум',
    'queue.completed': 'Завершено',
    'queue.workers': 'Воркеры',
    'queue.estimated_wait': 'Ожидаемое время',
    'queue.timeouts': 'Таймауты ожидания',
    'queue.admission_waiting': 'Ждут сейчас',
    'queue.admission_wait_max': 'Предел ожидания, с',
    'queue.admission_served': 'Обслужено (с ожиданием)',
    'queue.admission_active': 'Активных сессий',
    'queue.admission_status': 'Ждёт слот',
    'queue.admission_idle': 'Сейчас никто не ждёт слот.',
    'queue.admission_note': 'Ожидание слота: обслужено {served}, таймаутов {timeouts}, предел ожидания {seconds} с.',
    'queue.no_tasks': 'Очередь пуста',
    'queue.col_client': 'Клиент (сессия)',
    'sessions.model': 'Модель',
    'renderers.seconds': ' с',
    'renderers.queue_avg_wait': '~{seconds} с',
};
global.window.I18N = {
    t(k, vars) {
        let s = Object.prototype.hasOwnProperty.call(I18N_MAP, k) ? I18N_MAP[k] : k;
        if (vars) Object.keys(vars).forEach(function (n) { s = s.split('{' + n + '}').join(String(vars[n])); });
        return s;
    },
    getLang() { return 'ru'; },
};

require('./utils.js');
global.Utils = global.window.Utils;
require('./renderers.js');
const R = global.window.Renderers;
assert.ok(R && typeof R.queueView === 'function', 'Renderers.queueView должен быть экспортирован');

let checks = 0;
function check(name, fn) {
    fn();
    checks++;
    console.log('  ✓ ' + name);
}

// Живой ответ стенда (сокращённо).
const LIVE = {
    current_size: 0,
    max_size: 100,
    processed_total: 0,
    avg_wait_time_ms: 0,
    workers: 4,
    admission: {
        enabled: true,
        waiting: 2,
        served_total: 42,
        waited_total: 42,
        timeout_total: 1,
        avg_wait_ms: 13926,
        wait_max_sec: 300,
        active_sessions: 3,
        waiting_by_backend: { 'cppworker-gpu-bundled-agent': 2 },
        waiting_detail: [
            { session: 'X-User-Id:alice', backend: 'cppworker-gpu-bundled-agent', waited_ms: 4200 },
            { session: 'X-User-Id:bob', backend: 'cppworker-gpu-bundled-agent', waited_ms: 1100 },
        ],
    },
};

check('включённый admission — источник цифр, а не легаси-поля', function () {
    const v = R.queueView(LIVE);
    assert.strictEqual(v.mode, 'admission');
    assert.strictEqual(v.waiting, 2, 'должно быть admission.waiting, а не current_size=0');
    assert.strictEqual(v.processed, 42, 'должно быть served_total, а не processed_total=0');
    assert.strictEqual(v.avgWaitMs, 13926, 'должно быть avg_wait_ms, а не avg_wait_time_ms=0');
    assert.strictEqual(v.waitMaxSec, 300);
    assert.strictEqual(v.timeouts, 1);
    assert.strictEqual(v.activeSessions, 3);
});

check('легаси-режим (admission выключен) не ломает прежнее поведение', function () {
    const v = R.queueView({
        current_size: 5, max_size: 50, processed_total: 7, workers: 2, avg_wait_time_ms: 800,
        admission: { enabled: false, waiting: 99, served_total: 99 },
    });
    assert.strictEqual(v.mode, 'legacy');
    assert.strictEqual(v.waiting, 5);
    assert.strictEqual(v.processed, 7);
    assert.strictEqual(v.avgWaitMs, 800);
    assert.strictEqual(v.workers, 2);
    assert.strictEqual(v.legacyMax, 50);
});

check('поимённый список ожидающих разбирается без потерь', function () {
    const v = R.queueView(LIVE);
    assert.strictEqual(v.detail.length, 2);
    assert.strictEqual(v.detail[0].session, 'X-User-Id:alice');
    assert.strictEqual(v.detail[0].backend, 'cppworker-gpu-bundled-agent');
    assert.strictEqual(v.detail[0].waitedMs, 4200);
});

check('разбивка по бэкендам согласована со списком и отсортирована', function () {
    const v = R.queueView({
        admission: {
            enabled: true,
            waiting: 4,
            waiting_by_backend: { bk_small: 1, bk_big: 3 },
            waiting_detail: [{ session: 'a', backend: 'bk_big', waited_ms: 1 }],
        },
    });
    assert.deepStrictEqual(v.byBackend, [
        { backend: 'bk_big', count: 3 },
        { backend: 'bk_small', count: 1 },
    ]);
});

check('если карты по бэкендам нет — считаем по списку (счётчик не расходится с таблицей)', function () {
    const v = R.queueView({
        admission: {
            enabled: true, waiting: 2,
            waiting_detail: [
                { session: 'a', backend: 'bk1', waited_ms: 1 },
                { session: 'b', backend: 'bk1', waited_ms: 2 },
            ],
        },
    });
    assert.deepStrictEqual(v.byBackend, [{ backend: 'bk1', count: 2 }]);
});

check('мусор и пустые данные не роняют отрисовку', function () {
    [null, undefined, {}, { admission: null }, { admission: {} }].forEach(function (bad) {
        const v = R.queueView(bad);
        assert.ok(v && typeof v.waiting === 'number', 'queueView упал на ' + JSON.stringify(bad));
        assert.ok(Array.isArray(v.detail) && Array.isArray(v.byBackend));
    });
});

check('queuePage переключает подписи плиток в admission-режиме', function () {
    R.queuePage(LIVE, [], []);
    assert.strictEqual(byId.queueLabelPosition.textContent, 'Ждут сейчас');
    assert.strictEqual(byId.queueLabelMax.textContent, 'Предел ожидания, с');
    assert.strictEqual(byId.queueLabelProcessed.textContent, 'Обслужено (с ожиданием)');
    assert.strictEqual(byId.queueLabelWorkers.textContent, 'Активных сессий');
    assert.strictEqual(byId.queueLabelAvgWait.textContent, 'Ожидаемое время');
});

check('queuePage показывает реальные цифры и ожидающих', function () {
    R.queuePage(LIVE, [], []);
    assert.strictEqual(String(byId.queueCurrentSize.textContent), '2');
    assert.strictEqual(String(byId.queueMaxSize.textContent), '300');
    assert.strictEqual(String(byId.queueProcessed.textContent), '42');
    assert.strictEqual(String(byId.queueTimeouts.textContent), '1');
    // Визуализация — по бэкендам, а не мёртвая полоса 0..100.
    assert.ok(/queue-adm-row/.test(byId.queueVisualization.innerHTML), 'нет строк по бэкендам');
    assert.ok(/cppworker-gpu-bundled-agent/.test(byId.queueVisualization.innerHTML));
    assert.ok(/Ожидание слота: обслужено 42/.test(byId.queueVisualization.innerHTML),
        'нет пояснения режима: ' + byId.queueVisualization.innerHTML);
    // Таблица — поимённо, с сессией клиента и статусом ожидания.
    const rows = byId.queueTasksBody.innerHTML;
    assert.ok(/X-User-Id:alice/.test(rows), 'нет ожидающего в таблице: ' + rows);
    assert.ok(/Ждёт слот/.test(rows), 'нет статуса ожидания: ' + rows);
    assert.strictEqual(byId.queueColModel.textContent, 'Клиент (сессия)');
});

check('пустая admission-очередь объясняется словами, а не нулём', function () {
    R.queuePage({ admission: { enabled: true, waiting: 0, waiting_by_backend: {}, waiting_detail: [] } }, [], []);
    assert.ok(/Сейчас никто не ждёт слот/.test(byId.queueVisualization.innerHTML),
        'нет объяснения пустой очереди: ' + byId.queueVisualization.innerHTML);
    assert.strictEqual(byId.queueColModel.textContent, 'Клиент (сессия)');
});

check('легаси-режим возвращает прежние подписи и полосу', function () {
    R.queuePage({ current_size: 3, max_size: 100, processed_total: 5, workers: 4, avg_wait_time_ms: 0 }, [], []);
    assert.strictEqual(byId.queueLabelPosition.textContent, 'Позиция');
    assert.strictEqual(byId.queueLabelMax.textContent, 'Максимум');
    assert.strictEqual(String(byId.queueCurrentSize.textContent), '3');
    assert.ok(/queue-bar/.test(byId.queueVisualization.innerHTML), 'нет легаси-полосы');
    assert.strictEqual(byId.queueColModel.textContent, 'Модель');
});

console.log('\nOK: ' + checks + ' checks passed');

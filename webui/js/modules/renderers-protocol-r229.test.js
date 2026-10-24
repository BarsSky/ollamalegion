// renderers-protocol-r229.test.js — R229 (2026-10-22).
//
// Запуск: node webui/js/modules/renderers-protocol-r229.test.js
//
// ЖИВЫЕ ЖАЛОБЫ ОПЕРАТОРА: «Cline жалуется на инструменты», «Open WebUI получает не
// тот n_ctx», «сессия висит» — почти все относятся к одному из двух путей
// (OpenAI /v1/... или Ollama /api/...), но в сессиях и очереди пути были
// неразличимы. Тест фиксирует бейдж и то, что протокол доходит до строк очереди.
//
// Проверяется:
//   1. protocolBadge: openai → OpenAI, ollama → Ollama, пусто/мусор → нет бейджа
//      (выдуманный протокол вводил бы в заблуждение);
//   2. queueView переносит protocol из waiting_detail в модель строки;
//   3. sessionsPage рисует бейдж рядом с моделью.

'use strict';

const assert = require('assert');

function makeEl(id) {
    return {
        id: id || '', innerHTML: '', textContent: '', value: '', hidden: false,
        style: {}, dataset: {},
        classList: { add() {}, remove() {}, contains() { return false; } },
        setAttribute() {}, getAttribute() { return null; }, appendChild() {},
        removeChild() {}, addEventListener() {},
        querySelector() { return null; }, querySelectorAll() { return []; }
    };
}
const byId = {};
global.document = {
    createElement() {
        let text = '';
        const el = makeEl('');
        Object.defineProperty(el, 'textContent', { get() { return text; }, set(v) { text = String(v); } });
        Object.defineProperty(el, 'innerHTML', {
            get() { return text.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;'); }
        });
        return el;
    },
    getElementById(id) { return byId[id] || (byId[id] = makeEl(id)); },
    querySelector() { return null; }, querySelectorAll() { return []; },
    addEventListener() {}, removeEventListener() {}
};
global.window = global;
global.window.I18N = { t(k, vars) { return vars ? k + JSON.stringify(vars) : k; }, getLang() { return 'ru'; } };

require('./utils.js');
global.Utils = global.window.Utils;
require('./renderers.js');
const R = global.window.Renderers;

let checks = 0;
function check(name, fn) { fn(); checks++; console.log('  ✓ ' + name); }

check('R229: protocolBadge различает OpenAI и Ollama, остальное не выдумывает', function () {
    assert.ok(R.protocolBadge, 'protocolBadge не экспортирован');
    const oai = R.protocolBadge('openai');
    assert.ok(/OpenAI/.test(oai), 'нет бейджа OpenAI: ' + oai);
    assert.ok(/protocol-openai/.test(oai), 'нет класса openai: ' + oai);
    const oll = R.protocolBadge('ollama');
    assert.ok(/Ollama/.test(oll), 'нет бейджа Ollama: ' + oll);
    assert.ok(/protocol-ollama/.test(oll), 'нет класса ollama: ' + oll);
    assert.strictEqual(R.protocolBadge(''), '', 'пустой протокол дал бейдж');
    assert.strictEqual(R.protocolBadge(undefined), '', 'undefined дал бейдж');
    assert.strictEqual(R.protocolBadge('grpc'), '', 'неизвестный протокол дал бейдж');
});

check('R229: queueView переносит протокол ожидающего', function () {
    const v = R.queueView({
        admission: {
            enabled: true, waiting: 2, served_total: 1, avg_wait_ms: 10, wait_max_sec: 300,
            waiting_detail: [
                { session: 's-openai', backend: 'b1', waited_ms: 500, protocol: 'openai' },
                { session: 's-ollama', backend: 'b1', waited_ms: 700, protocol: 'ollama' }
            ]
        }
    });
    assert.strictEqual(v.detail[0].protocol, 'openai', 'протокол openai не дошёл до строки');
    assert.strictEqual(v.detail[1].protocol, 'ollama', 'протокол ollama не дошёл до строки');
    // unknown protocol must stay empty, not invented
    const v2 = R.queueView({
        admission: { enabled: true, waiting: 1, waiting_detail: [{ session: 's', backend: 'b', waited_ms: 1 }] }
    });
    assert.strictEqual(v2.detail[0].protocol, '', 'протокол выдуман для строки без поля');
});

check('R229: sessionsPage рисует бейдж протокола рядом с моделью', function () {
    R.sessionsPage([
        { id: 'sess-openai-1', backendId: 'b1', model: 'gemma', requestCount: 1, protocol: 'openai' },
        { id: 'sess-ollama-1', backendId: 'b1', model: 'gemma', requestCount: 1, protocol: 'ollama' }
    ]);
    const html = global.document.getElementById('sessionsTableBody').innerHTML;
    assert.ok(/protocol-openai/.test(html), 'нет бейджа OpenAI в таблице сессий: ' + html);
    assert.ok(/protocol-ollama/.test(html), 'нет бейджа Ollama в таблице сессий: ' + html);
});

console.log('\nOK: ' + checks + ' checks passed');

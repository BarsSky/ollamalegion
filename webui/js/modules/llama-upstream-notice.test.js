// llama-upstream-notice.test.js — R269: уведомление о свежести llama.cpp.
//
// ЗАПУСК: node webui/js/modules/llama-upstream-notice.test.js
//
// Тест проверяет ЧИСТУЮ функцию принятия решения (computeNotice) на данных,
// которые реально присылает /api/v1/llama-version. Здесь нет DOM-стабов и
// поддельного fetch, поэтому «пройти на моках» невозможно: подмена входа
// меняет и ожидание.

'use strict';

const assert = require('assert');
const path = require('path');

// Модуль читает T()/document при загрузке — подставляем минимальный i18n и
// отсутствующий document (авто-монтаж не сработает, нам нужна чистая функция).
const store = {};
global.localStorage = {
    getItem: function (k) { return Object.prototype.hasOwnProperty.call(store, k) ? store[k] : null; },
    setItem: function (k, v) { store[k] = String(v); },
    removeItem: function (k) { delete store[k]; },
};
global.I18N = {
    getLang: function () { return 'ru'; },
    t: function (key, vars) {
        const map = {
            'llamaUpstream.title': 'Доступна более свежая стабильная версия llama.cpp',
            'llamaUpstream.message': 'доступна {latest}; узлы на устаревшей сборке: {nodes}',
            'llamaUpstream.open': 'Открыть релиз',
            'llamaUpstream.unknownNodes': 'неизвестно',
        };
        let s = Object.prototype.hasOwnProperty.call(map, key) ? map[key] : key;
        if (vars) {
            Object.keys(vars).forEach(function (p) { s = s.replace('{' + p + '}', vars[p]); });
        }
        return s;
    },
};

const M = require(path.join(__dirname, 'llama-upstream-notice.js'));

let passed = 0;
const failures = [];
function check(name, fn) {
    try { fn(); passed++; console.log('  \u2713 ' + name); }
    catch (e) { failures.push(name + ': ' + (e && e.message)); console.error('  \u2717 ' + name + ' - ' + (e && e.message)); }
}

// 1. behind → уведомление показывается, текст содержит версию и узлы.
check('behind → уведомление с версией и списком узлов', function () {
    const n = M.computeNotice({
        enabled: true, relation: 'behind', latestTag: 'b5000',
        behindBackends: ['cppworker-gpu-bundled-agent'], repo: 'ggml-org/llama.cpp',
    });
    assert.strictEqual(n.show, true);
    assert.ok(n.text.indexOf('b5000') >= 0, 'в тексте нет версии: ' + n.text);
    assert.ok(n.text.indexOf('cppworker-gpu-bundled-agent') >= 0, 'в тексте нет узла: ' + n.text);
    assert.strictEqual(n.url, 'https://github.com/ggml-org/llama.cpp/releases');
});

// 2. up_to_date / unknown / ahead → НЕ показываем (главный анти-ложный случай).
['up_to_date', 'unknown', 'ahead'].forEach(function (rel) {
    check(rel + ' → уведомления нет', function () {
        const n = M.computeNotice({ enabled: true, relation: rel, latestTag: 'b5000', behindBackends: [] });
        assert.strictEqual(n.show, false, 'ложное уведомление при relation=' + rel);
    });
});

// 3. Проверка выключена → нет уведомления, даже если relation=behind.
check('enabled=false → уведомления нет', function () {
    const n = M.computeNotice({ enabled: false, relation: 'behind', latestTag: 'b5000' });
    assert.strictEqual(n.show, false);
});

// 4. Неполные данные не ломают панель.
check('пустой/битый ответ → нет уведомления и нет исключения', function () {
    [null, undefined, {}, { enabled: true }, { enabled: true, relation: 'behind' }].forEach(function (d) {
        const n = M.computeNotice(d);
        assert.strictEqual(n.show, false, 'на входе ' + JSON.stringify(d));
    });
});

// 5. Нет repo → нет ссылки, но уведомление есть (деградация не отменяет смысл).
check('без repo → уведомление есть, ссылки нет', function () {
    const n = M.computeNotice({ enabled: true, relation: 'behind', latestTag: 'b5000', behindBackends: [] });
    assert.strictEqual(n.show, true);
    assert.strictEqual(n.url, '');
});

// 6. renderHtml экранирует данные (версия приходит из сети).
check('renderHtml экранирует подозрительные символы', function () {
    const html = M.renderHtml(M.computeNotice({
        enabled: true, relation: 'behind', latestTag: '<script>x</script>',
        behindBackends: [], repo: 'ggml-org/llama.cpp',
    }));
    assert.ok(html.indexOf('<script>') < 0, 'неэкранированный тег попал в разметку: ' + html);
    assert.ok(html.indexOf('&lt;script&gt;') >= 0, 'версия не экранирована: ' + html);
});

// 7. Без показа renderHtml пуст.
check('renderHtml пуст, когда показывать нечего', function () {
    assert.strictEqual(M.renderHtml({ show: false }), '');
    assert.strictEqual(M.renderHtml(null), '');
});

console.log('\n' + passed + ' passed, ' + failures.length + ' failed');
if (failures.length) {
    failures.forEach(function (f) { console.error('  - ' + f); });
    process.exit(1);
}

#!/usr/bin/env node
/*
 * notice-bar-r758.test.js — R758 (2026-11-25)
 *
 * ЖИВАЯ ЖАЛОБА. «По поведению при загрузке обнаружил, что проблема была в малом
 * месте на диске, но это удалось поймать только в строке уведомлений у
 * колокольчика, никаких больше упоминаний не увидел — что не удобно».
 *
 * Колокольчик — ВЫПАДАЮЩИЙ список: событие видно, только пока он открыт, а
 * бейдж говорит «сколько», не «что». Для ресурсного отказа (нет места на
 * диске, не хватило RAM, провал загрузки) это неверно: состояние ДЛИТСЯ, пока
 * причину не устранили, поэтому предупреждение обязано висеть на странице до
 * явного «Понятно».
 *
 * ЧТО ПРОВЕРЯЕТСЯ:
 *   1. warning+ непрочитанные показываются, info — нет;
 *   2. порядок — свежие сверху, не больше лимита;
 *   3. прочитанное (markAsRead) исчезает и НЕ возвращается после перезагрузки
 *      (readIds хранится в localStorage менеджера);
 *   4. «Понятно» на строке гасит только её;
 *   5. «Скрыть все» гасит все показанные;
 *   6. без предупреждений панель скрыта (hidden), пустой рамки нет;
 *   7. текст экранируется (сообщение приходит из бэкенда).
 *
 * ЗАПУСК: node webui/tests/notice-bar-r758.test.js
 */
'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');

const REPO_ROOT = path.resolve(__dirname, '..', '..');

let passed = 0, failures = 0;
function check(name, fn) {
    return Promise.resolve()
        .then(fn)
        .then(function () { passed++; console.log('  ✓ ' + name); })
        .catch(function (e) {
            failures++;
            console.error('  ✗ ' + name + ' — ' + (e && e.message));
        });
}

// --- минимальный DOM ---------------------------------------------------------
function makeNode(tag) {
    const node = {
        tagName: String(tag || 'div').toUpperCase(),
        innerHTML: '', textContent: '', hidden: false, id: '',
        _listeners: {},
        addEventListener: function (ev, fn) { this._listeners[ev] = fn; },
        removeEventListener: function () {},
        closest: function () { return null; },
    };
    return node;
}
const elements = Object.create(null);
function getEl(id) {
    if (!Object.prototype.hasOwnProperty.call(elements, id)) {
        elements[id] = makeNode('div');
        elements[id].id = id;
    }
    return elements[id];
}

global.window = global;
global.document = {
    getElementById: getEl,
    querySelector: function () { return null; },
    querySelectorAll: function () { return []; },
    createElement: function (tag) { return makeNode(tag); },
    addEventListener: function () {},
};
const store = {};
global.localStorage = {
    getItem: function (k) { return Object.prototype.hasOwnProperty.call(store, k) ? store[k] : null; },
    setItem: function (k, v) { store[k] = String(v); },
    removeItem: function (k) { delete store[k]; },
};
global.console = console;

require(path.join(REPO_ROOT, 'webui', 'js', 'modules', 'notifications.js'));
require(path.join(REPO_ROOT, 'webui', 'js', 'modules', 'notice-bar.js'));
const mgr = global.notifications;
const NB = global.NoticeBar;
assert.ok(mgr && NB, 'notifications и NoticeBar должны быть экспортированы');

// Подкладываем события прямо в буфер: SSE здесь не нужен (проверяется панель).
function pushEvent(ev) {
    mgr._handleEvent(Object.assign({ type: 'notification' }, ev));
}

// mount() навешивает обработчик клика (как в app.js): без него кнопки
// «Понятно»/«Скрыть все» не работают — это часть контракта панели.
NB.mount();

function reset() {
    mgr.clear();
    mgr.unreadCount = 0;
    mgr.readIds.clear();
    delete store['notifications.readIds'];
    getEl('noticeBar').innerHTML = '';
    getEl('noticeBar').hidden = true;
}

const bar = function () { return getEl('noticeBar'); };

(async function main() {
    await check('1. warning+ показываются, info — нет', function () {
        reset();
        pushEvent({ id: 'w1', severity: 'warning', message: 'Мало места на диске', timestamp: new Date().toISOString() });
        pushEvent({ id: 'i1', severity: 'info', message: 'Загрузка восстановлена', timestamp: new Date().toISOString() });
        NB.render();
        const html = bar().innerHTML;
        assert.ok(html.indexOf('Мало места на диске') !== -1, 'warning не показан: ' + html);
        assert.ok(html.indexOf('Загрузка восстановлена') === -1, 'info не должен попадать в панель: ' + html);
        assert.strictEqual(bar().hidden, false, 'панель должна быть видна');
    });

    await check('2. порядок «свежие сверху» и лимит строк', function () {
        reset();
        const base = Date.now();
        pushEvent({ id: 'e1', severity: 'error', message: 'старое', timestamp: new Date(base - 300000).toISOString() });
        pushEvent({ id: 'e2', severity: 'error', message: 'среднее', timestamp: new Date(base - 120000).toISOString() });
        pushEvent({ id: 'e3', severity: 'critical', message: 'новое', timestamp: new Date(base).toISOString() });
        pushEvent({ id: 'e4', severity: 'warning', message: 'самое старое', timestamp: new Date(base - 900000).toISOString() });
        NB.render();
        const html = bar().innerHTML;
        const posNew = html.indexOf('новое');
        const posMid = html.indexOf('среднее');
        const posOld = html.indexOf('старое');
        assert.ok(posNew !== -1 && posMid !== -1 && posOld !== -1, 'не все строки отрисованы: ' + html);
        assert.ok(posNew < posMid && posMid < posOld, 'порядок не «свежие сверху»: ' + html);
        assert.ok(html.indexOf('самое старое') === -1, 'четвёртая строка выходит за лимит 3: ' + html);
    });

    await check('3. прочитанное исчезает и не возвращается после перезагрузки', function () {
        reset();
        pushEvent({ id: 'd1', severity: 'error', message: 'Нет места на диске', timestamp: new Date().toISOString() });
        NB.render();
        assert.ok(bar().innerHTML.indexOf('Нет места на диске') !== -1, 'строка не отрисована');

        // «Понятно» на строке.
        NB.dismiss('d1');
        assert.ok(bar().innerHTML.indexOf('Нет места на диске') === -1, 'после «Понятно» строка осталась');
        assert.strictEqual(bar().hidden, true, 'без предупреждений панель обязана скрыться');

        // Имитация перезагрузки страницы: читаем readIds из localStorage заново.
        const saved = store['notifications.readIds'];
        assert.ok(saved && saved.indexOf('d1') !== -1, 'скрытие не сохранено в localStorage: ' + saved);
        mgr.readIds = new Set(JSON.parse(saved));
        NB.render();
        assert.ok(bar().innerHTML.indexOf('Нет места на диске') === -1,
            'после перезагрузки скрытая строка вернулась — оператор не сможет её убрать');
    });

    await check('4. «Понятно» гасит только свою строку', function () {
        reset();
        pushEvent({ id: 'a1', severity: 'error', message: 'ошибка А', timestamp: new Date().toISOString() });
        pushEvent({ id: 'b1', severity: 'warning', message: 'предупреждение Б', timestamp: new Date().toISOString() });
        NB.render();
        NB.dismiss('a1');
        const html = bar().innerHTML;
        assert.ok(html.indexOf('ошибка А') === -1, 'строка А осталась');
        assert.ok(html.indexOf('предупреждение Б') !== -1, 'строка Б исчезла вместе с А');
    });

    await check('5. «Скрыть все» гасит все показанные строки', function () {
        reset();
        pushEvent({ id: 'x1', severity: 'error', message: 'первое', timestamp: new Date().toISOString() });
        pushEvent({ id: 'x2', severity: 'warning', message: 'второе', timestamp: new Date().toISOString() });
        NB.render();
        bar()._listeners.click({ target: { id: 'noticeBarDismissAll', closest: function () { return null; } }, preventDefault: function () {} });
        assert.strictEqual(bar().hidden, true, 'после «Скрыть все» панель должна скрыться');
        assert.ok(mgr.isRead('x1') && mgr.isRead('x2'), 'не все строки помечены прочитанными');
    });

    await check('6. текст уведомления экранируется', function () {
        reset();
        const evil = '<img src=x onerror=alert(1)>';
        pushEvent({ id: 'evil1', severity: 'error', message: evil, timestamp: new Date().toISOString() });
        NB.render();
        const html = bar().innerHTML;
        assert.ok(html.indexOf('<img') === -1, 'разметка из сообщения попала в HTML: ' + html);
        assert.ok(html.indexOf('&lt;img') !== -1, 'текст не экранирован: ' + html);
    });

    await check('7. i18n: ключи панели есть в ru.js и en.js', function () {
        const ru = fs.readFileSync(path.join(REPO_ROOT, 'webui', 'js', 'i18n', 'ru.js'), 'utf8');
        const en = fs.readFileSync(path.join(REPO_ROOT, 'webui', 'js', 'i18n', 'en.js'), 'utf8');
        ['notice.title', 'notice.dismiss', 'notice.dismiss_all'].forEach(function (k) {
            assert.ok(ru.indexOf('"' + k + '":') !== -1, 'нет ru-перевода ' + k);
            assert.ok(en.indexOf('"' + k + '":') !== -1, 'нет en-перевода ' + k);
        });
    });

    await check('8. панель подключена в index.html и app.js', function () {
        const html = fs.readFileSync(path.join(REPO_ROOT, 'webui', 'index.html'), 'utf8');
        assert.ok(html.indexOf('id="noticeBar"') !== -1, 'в разметке нет контейнера noticeBar');
        assert.ok(html.indexOf('js/modules/notice-bar.js') !== -1, 'модуль не подключён в index.html');
        const app = fs.readFileSync(path.join(REPO_ROOT, 'webui', 'js', 'app.js'), 'utf8');
        assert.ok(app.indexOf('NoticeBar.mount') !== -1, 'app.js не монтирует панель');
    });

    console.log('');
    if (failures > 0) {
        console.log('ИТОГ: провалено проверок — ' + failures);
        process.exit(1);
    }
    console.log('ИТОГ: все проверки пройдены (' + passed + ')');
})().catch(function (e) {
    console.error('FATAL', e && e.stack || e);
    process.exit(1);
});

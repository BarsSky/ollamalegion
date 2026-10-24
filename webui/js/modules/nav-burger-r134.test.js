// nav-burger-r134.test.js — R134 (2026-10-16).
//
// Запуск: node webui/js/modules/nav-burger-r134.test.js
//
// ЖИВАЯ ЖАЛОБА ОПЕРАТОРА: «нет вкладки агенты». Вкладка в разметке есть
// (data-page="agents"), но на ширине <=768px сайдбар уезжает за край экрана, а
// ОТКРЫТЬ ЕГО БЫЛО НЕЧЕМ: переключателя не существовало. То есть на узком окне
// терялась ВСЯ навигация.
//
// Проверяется поведение (внешний вид — responsive.css):
//   1. клик по кнопке открывает сайдбар (body.nav-open);
//   2. повторный клик закрывает;
//   3. клик по пункту меню закрывает панель (иначе она перекрывает содержимое);
//   4. окно стало шире порога — состояние «открыт» снимается;
//   5. без кнопки в разметке модуль молча ничего не делает (не падает).

'use strict';

const assert = require('assert');

let classes = {};
let btnHandler = null;
let navHandler = null;
let resizeHandler = null;

global.window = {
    innerWidth: 1200,
    addEventListener: function (ev, fn) { if (ev === 'resize') resizeHandler = fn; }
};

const button = { addEventListener: function (ev, fn) { if (ev === 'click') btnHandler = fn; } };
const nav = { addEventListener: function (ev, fn) { if (ev === 'click') navHandler = fn; } };

global.document = {
    readyState: 'complete',
    body: {
        classList: {
            add: function (c) { classes[c] = true; },
            remove: function (c) { delete classes[c]; },
            toggle: function (c) { if (classes[c]) { delete classes[c]; } else { classes[c] = true; } },
            contains: function (c) { return !!classes[c]; }
        }
    },
    getElementById: function (id) { return id === 'navBurger' ? button : null; },
    querySelector: function (sel) { return sel === '.sidebar .nav' ? nav : null; },
    addEventListener: function () {}
};

require('./nav-burger.js');

let checks = 0;
function check(name, fn) { fn(); checks++; console.log('  ✓ ' + name); }

check('R134: клик по кнопке открывает сайдбар', function () {
    assert.strictEqual(typeof btnHandler, 'function', 'модуль не навесил обработчик на кнопку');
    btnHandler();
    assert.ok(classes['nav-open'], 'сайдбар не открылся: на узком окне навигация недоступна');
});

check('R134: повторный клик закрывает сайдбар', function () {
    btnHandler();
    assert.ok(!classes['nav-open'], 'сайдбар не закрылся повторным кликом');
});

check('R134: клик по пункту меню закрывает панель', function () {
    btnHandler();
    assert.ok(classes['nav-open'], 'подготовка: панель должна быть открыта');
    navHandler({ target: { closest: function (sel) { return sel === '.nav-item' ? {} : null; } } });
    assert.ok(!classes['nav-open'], 'после перехода панель осталась открытой и перекрывает содержимое');
});

check('R134: клик мимо пункта меню панель не закрывает', function () {
    btnHandler();
    navHandler({ target: { closest: function () { return null; } } });
    assert.ok(classes['nav-open'], 'панель закрылась от клика вне пункта меню');
    btnHandler();
});

check('R134: порог 768px и снятие состояния при широком окне', function () {
    assert.strictEqual(global.window.NavBurger.BREAKPOINT, 768, 'порог должен совпадать с responsive.css');
    assert.strictEqual(global.window.NavBurger.shouldCloseOnResize(1200), true, 'широкое окно должно снимать состояние');
    assert.strictEqual(global.window.NavBurger.shouldCloseOnResize(700), false, 'узкое окно состояние не снимает');
    assert.strictEqual(typeof resizeHandler, 'function', 'нет обработчика resize');
    btnHandler();
    global.window.innerWidth = 1200;
    resizeHandler();
    assert.ok(!classes['nav-open'], 'после расширения окна состояние «открыт» осталось');
});

check('R134: без кнопки в разметке модуль не падает', function () {
    const savedGet = global.document.getElementById;
    global.document.getElementById = function () { return null; };
    delete require.cache[require.resolve('./nav-burger.js')];
    require('./nav-burger.js');
    global.document.getElementById = savedGet;
    assert.ok(true, 'модуль упал без кнопки в разметке');
});

console.log('\nnav-burger-r134: ' + checks + ' проверок пройдено');

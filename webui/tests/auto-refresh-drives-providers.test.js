// auto-refresh-drives-providers.test.js — R493 (2026-11-17): авто-обновление
// обязано делать то же, что кнопка «Обновить».
//
// ЖИВОЙ ДЕФЕКТ, ради которого этот набор существует. Оператор: «автообновление
// отображаемой информации не работает», «на странице GGUF информация по
// загруженным моделям появляется только после обновления страницы».
//
// Причина: в `app.js → startPeriodicRefresh` тик дёргал ОБЩИЕ запросы (cluster,
// очередь, сессии, агенты), но НЕ вызывал провайдеров активной страницы
// (js/modules/data-refresh.js) и не перерисовывал страницу. Провайдеров вызывала
// только кнопка. Поэтому собственные источники страниц (GGUF-список и панель
// деталей, image-модели и загрузки, диагностика) стояли замороженными до F5,
// хотя индикатор в шапке бодро показывал «Обновлено только что».
//
// Проверка статическая (как и no-dead-refresh-buttons): поведение таймера без
// браузера не воспроизвести, а вот «в тике нет вызова провайдеров» — видно.
//
// Запуск: node webui/tests/auto-refresh-drives-providers.test.js
'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');

const root = path.join(__dirname, '..');
const appJs = fs.readFileSync(path.join(root, 'js', 'app.js'), 'utf8');

let passed = 0;
const failures = [];
function check(name, fn) {
    try {
        fn();
        passed++;
        console.log('  \u2713 ' + name);
    } catch (e) {
        failures.push(name + ': ' + (e && e.message));
        console.error('  \u2717 ' + name + ' — ' + (e && e.message));
    }
}

/** Тело тика периодического обновления (от setInterval до `}, interval);`). */
function tickBody() {
    const start = appJs.indexOf('refreshTimer = setInterval(');
    assert.ok(start !== -1, 'в app.js нет периодического цикла (refreshTimer = setInterval)');
    const end = appJs.indexOf('}, interval);', start);
    assert.ok(end !== -1, 'не найден конец тела периодического цикла');
    return appJs.slice(start, end);
}

check('тик авто-обновления вызывает провайдеров активной страницы', function () {
    const body = tickBody();
    assert.ok(/\.refresh\(currentPage\)/.test(body),
        'тик не вызывает DataRefresh.refresh(currentPage) — источники страницы (GGUF, image, диагностика) останутся старыми до ручного обновления');
});

check('тик авто-обновления перерисовывает активную страницу', function () {
    const body = tickBody();
    assert.ok(/refreshPage\(currentPage\)/.test(body),
        'тик не перерисовывает страницу: данные обновятся в памяти, а на экране останутся прежние числа');
});

check('тик не дублирует общий набор запросов на страницах со своими источниками', function () {
    const body = tickBody();
    assert.ok(/hasProvider/.test(body),
        'тик не спрашивает hasProvider: либо провайдеры не вызываются, либо на каждой странице идут двойные запросы (общий набор + провайдер)');
});

check('пауза авто-обновления и скрытая вкладка по-прежнему останавливают тик', function () {
    const body = tickBody();
    assert.ok(/periodicRefreshPaused\(\)/.test(body), 'тик не проверяет скрытую вкладку');
    assert.ok(/isAuto\(\)/.test(body), 'тик не проверяет паузу авто-обновления');
});

check('у data-refresh.js есть hasProvider (иначе решение таймера не на чем строить)', function () {
    const dr = fs.readFileSync(path.join(root, 'js', 'modules', 'data-refresh.js'), 'utf8');
    assert.ok(/function hasProvider\(page\)/.test(dr), 'data-refresh.js не реализует hasProvider');
    assert.ok(/hasProvider: hasProvider/.test(dr), 'hasProvider не экспортирован — app.js его не увидит');
});

console.log('\n' + (failures.length ? 'FAILED: ' + failures.length : 'ИТОГ: все проверки пройдены (' + passed + ')'));
if (failures.length) {
    failures.forEach(function (f) { console.error(' - ' + f); });
    process.exit(1);
}

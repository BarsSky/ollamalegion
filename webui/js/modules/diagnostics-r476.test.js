// diagnostics-r476.test.js — R476 (2026-11-07): страница «Диагностика».
//
// ЗАЧЕМ ТЕСТ. Пользователь просил: «организуй модуль в WebUI, чтобы ключевые
// данные можно было сразу скопировать, а не искать по местам упоминания». Такая
// страница ломается тихо: пропал <script>, переименовали id, страница не
// зарегистрирована в роутере — и оператор снова ищет значения по пяти местам.
// Поэтому проверяем ровно швы, на которых она держится:
//   1. страница и пункт меню есть в index.html, скрипт подключён;
//   2. роутер знает страницу (иначе #diagnostics откроет пустой экран);
//   3. Api умеет и сводку, и скачивание архива (эндпоинты совпадают с Go);
//   4. модуль экспортируется в window.Diagnostics (сторож IIFE) и копирует
//      значение по клику;
//   5. в Go есть маршруты /api/v1/diag/summary и /api/v1/diag/bundle, у воркера —
//      /api/v1/cppworker/debug/bundle;
//   6. ключи i18n есть в обоих словарях.
//
// Запуск: node webui/js/modules/diagnostics-r476.test.js

'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');

const repo = path.resolve(__dirname, '..', '..', '..');
const read = function (rel) {
    return fs.readFileSync(path.join(repo, rel), 'utf8');
};

let passed = 0;
const failures = [];
function check(name, fn) {
    try {
        fn();
        passed++;
        console.log('  \u2713 ' + name);
    } catch (e) {
        failures.push(name + ': ' + (e && e.message));
        console.log('  \u2717 ' + name + ' — ' + (e && e.message));
    }
}

const indexHtml = read('webui/index.html');
const appJs = read('webui/js/app.js');
const apiJs = read('webui/js/modules/api.js');
const diagJs = read('webui/js/modules/diagnostics.js');
const routesGo = read('internal/api/routes.go');
const workerRouter = read('cmd/cppworker/router.go');
const ruI18n = read('webui/js/i18n/ru.js');
const enI18n = read('webui/js/i18n/en.js');

check('R476: страница диагностики есть в index.html', function () {
    assert.ok(indexHtml.indexOf('id="diagnostics-page"') > 0,
        'нет div#diagnostics-page — страница не откроется');
});

check('R476: пункт меню ведёт на diagnostics', function () {
    assert.ok(/data-page="diagnostics"/.test(indexHtml),
        'нет пункта меню data-page="diagnostics"');
});

check('R476: модуль подключён скриптом', function () {
    assert.ok(/modules\/diagnostics\.js/.test(indexHtml),
        'index.html не подключает js/modules/diagnostics.js');
});

check('R476: роутер знает страницу (refreshPage + известные страницы)', function () {
    assert.ok(appJs.indexOf("case 'diagnostics'") > 0,
        'в refreshPage нет case diagnostics — страница откроется пустой');
    assert.ok(/known = \[[^\]]*'diagnostics'/.test(appJs),
        'страница не добавлена в pageFromURL: ссылка #diagnostics не откроется');
});

check('R476: Api читает сводку с того же пути, что и балансер', function () {
    assert.ok(apiJs.indexOf('/api/v1/diag/summary') > 0,
        'Api не запрашивает /api/v1/diag/summary');
    assert.ok(routesGo.indexOf('/api/v1/diag/summary') > 0,
        'балансер не зарегистрировал /api/v1/diag/summary');
});

check('R476: Api скачивает архив с того же пути, что и балансер', function () {
    assert.ok(apiJs.indexOf('/api/v1/diag/bundle') > 0,
        'Api не запрашивает /api/v1/diag/bundle');
    assert.ok(routesGo.indexOf('/api/v1/diag/bundle') > 0,
        'балансер не зарегистрировал /api/v1/diag/bundle');
});

check('R476: у воркера есть свой архив диагностики', function () {
    assert.ok(workerRouter.indexOf('/api/v1/cppworker/debug/bundle') > 0,
        'cppworker не зарегистрировал /debug/bundle — архив балансера будет без воркера');
});

check('R476: модуль экспортируется в window.Diagnostics', function () {
    assert.ok(/window\.Diagnostics\s*=/.test(diagJs),
        'нет window.Diagnostics — сторож IIFE не пропустит, страница не загрузится');
    assert.ok(/const Diagnostics = \(function \(\)/.test(diagJs),
        'модуль не оформлен как IIFE по конвенции WebUI');
});

check('R476: значение копируется по клику, блок — по кнопке', function () {
    assert.ok(diagJs.indexOf('data-diag-value') > 0,
        'нет кликабельных значений (data-diag-value)');
    assert.ok(diagJs.indexOf('data-diag-copy') > 0,
        'нет кнопок «копировать блок» (data-diag-copy)');
    assert.ok(diagJs.indexOf('navigator.clipboard') > 0,
        'копирование не использует clipboard API');
    assert.ok(diagJs.indexOf('execCommand') > 0,
        'нет fallback копирования (http-стенд без secure context)');
});

check('R476: на странице есть кнопки обновления, копирования и архива', function () {
    ['diagRefresh', 'diagCopyAll', 'diagDownloadBundle'].forEach(function (id) {
        assert.ok(indexHtml.indexOf('id="' + id + '"') > 0, 'нет кнопки #' + id);
        assert.ok(diagJs.indexOf(id) > 0, 'модуль не обрабатывает #' + id);
    });
});

check('R476: разделы диаграммы данных присутствуют', function () {
    ['diagVersions', 'diagWindow', 'diagFlags', 'diagBackends'].forEach(function (id) {
        assert.ok(indexHtml.indexOf('id="' + id + '"') > 0, 'нет контейнера #' + id);
        assert.ok(diagJs.indexOf(id) > 0, 'модуль не заполняет #' + id);
    });
});

check('R476: флаги разделены на «заданы» и «не заданы»', function () {
    assert.ok(diagJs.indexOf("diag.flags_unset") > 0,
        'страница не показывает незаданные флаги — снова придётся гадать, откуда значение');
    const go = read('internal/api/diag_summary_r476.go');
    assert.ok(go.indexOf('"unset"') > 0,
        'сводка балансера не различает set и unset');
});

check('R476: секреты не попадают в список флагов', function () {
    const go = read('internal/api/diag_summary_r476.go');
    ['LB_API_TOKEN', 'LB_ADMIN_PASSWORD', 'LB_AUTH_TOKENS'].forEach(function (secret) {
        assert.ok(go.indexOf('"' + secret + '"') < 0,
            'секрет ' + secret + ' попал в белый список диагностики');
    });
});

check('R476: ключи i18n есть в обоих словарях', function () {
    ['nav.diagnostics', 'diag.title', 'diag.copy_all', 'diag.download_bundle', 'diag.flags_unset']
        .forEach(function (key) {
            assert.ok(ruI18n.indexOf('"' + key + '"') > 0, 'нет ' + key + ' в ru.js');
            assert.ok(enI18n.indexOf('"' + key + '"') > 0, 'нет ' + key + ' в en.js');
        });
});

console.log('');
if (failures.length) {
    console.log('FAILED ' + failures.length + ' of ' + (passed + failures.length));
    failures.forEach(function (f) { console.log('  - ' + f); });
    process.exit(1);
}
console.log('OK: ' + passed + ' проверок');

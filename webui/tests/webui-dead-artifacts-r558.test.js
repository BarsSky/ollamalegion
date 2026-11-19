// webui-dead-artifacts-r558.test.js — R558 (W7 плана
// 2026-11-19-webui-pages-and-setup, §6 и §4.7): в репозитории не должно быть
// артефактов WebUI, на которые ничего не ссылается.
//
// ЧТО БЫЛО. Три «мёртвых» файла жили рядом с рабочими и выглядели частью проекта:
//   - `webui/tests/webui_field_tests.html` (и генератор `generate_tests.py`) —
//     самодостаточная страница ручных проверок полей, не подключённая ни к одной
//     странице и не запускаемая ни одним скриптом. Внутри неё проверялись поля,
//     которых уже нет (например #apiToken и поля агента — удалены в R544);
//   - `webui/js/i18n/ru.json` и `en.json` — словари в JSON, тогда как WebUI грузит
//     `ru.js`/`en.js`. Правка «в json» не влияла ни на одну надпись.
//
// ЧЕМ ЭТО ВРЕДНО. Мёртвый файл не краснеет в CI и не мешает тестам — он просто
// вводит в заблуждение: оператор (и агент) правит не тот словарь и ищет дефект в
// месте, которое ничего не решает.
//
// Проверка статическая: «на файл никто не ссылается». Запуск:
// node webui/tests/webui-dead-artifacts-r558.test.js
'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');

const root = path.join(__dirname, '..');

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

const DEAD = [
    'tests/webui_field_tests.html',
    'tests/generate_tests.py',
    'js/i18n/ru.json',
    'js/i18n/en.json',
];

check('мёртвых артефактов WebUI нет', function () {
    DEAD.forEach(function (rel) {
        assert.strictEqual(fs.existsSync(path.join(root, rel)), false,
            'файл webui/' + rel + ' вернулся: на него никто не ссылается, и правка в нём ничего не меняет');
    });
});

check('словари существуют ровно в одном формате (.js)', function () {
    assert.ok(fs.existsSync(path.join(root, 'js/i18n/ru.js')), 'нет js/i18n/ru.js');
    assert.ok(fs.existsSync(path.join(root, 'js/i18n/en.js')), 'нет js/i18n/en.js');
});

check('index.html грузит словари .js, а не .json', function () {
    const html = fs.readFileSync(path.join(root, 'index.html'), 'utf8');
    ['js/i18n/ru.js', 'js/i18n/en.js'].forEach(function (needle) {
        assert.ok(html.indexOf(needle) !== -1, 'index.html не подключает ' + needle);
    });
    assert.strictEqual(html.indexOf('i18n/ru.json'), -1, 'index.html снова ссылается на ru.json');
    assert.strictEqual(html.indexOf('i18n/en.json'), -1, 'index.html снова ссылается на en.json');
});

console.log('\n' + passed + ' проверок зелёные, ' + failures.length + ' красных');
if (failures.length > 0) {
    failures.forEach(function (f) { console.error('  FAIL: ' + f); });
    process.exit(1);
}

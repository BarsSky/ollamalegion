// image-profile-pure-split-r640.test.js — R640 (2026-11-19): страж разреза
// редактора профилей image-моделей на два файла: чистый слой и DOM-редактор.
//
// ЗАЧЕМ. Разрез держится на двух вещах, которые легко сломать молча:
//   1. ПОРЯДОК ЗАГРУЗКИ. `image-profiles.js` берёт `window.ImageProfilePure` и
//      раздаёт имена локальными ссылками. Если <script> переставить или новый файл
//      забыть подключить, модуль упадёт с честной ошибкой — но увидит это только
//      браузер, то есть оператор. Тест ловит это до выкатки.
//   2. «ЧИСТОТА» СЛОЯ. Если в image-profile-pure.js снова появится DOM или fetch,
//      смысл разреза теряется: файл перестанет быть тем, что можно покрывать
//      юнит-тестами без мока документа.
//
// Запуск: node webui/tests/image-profile-pure-split-r640.test.js
'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');

let passed = 0;
const failures = [];
function check(name, fn) {
    try {
        fn();
        passed++;
        console.log('  \u2713 ' + name);
    } catch (e) {
        failures.push(name + ': ' + (e && e.message));
        console.error('  \u2717 ' + name + ' \u2014 ' + (e && e.message));
    }
}

const ROOT = path.join(__dirname, '..', '..');
const read = (p) => fs.readFileSync(path.join(ROOT, p), 'utf8');

const INDEX_HTML = read('webui/index.html');
const PROFILES = read('webui/js/modules/image-profiles.js');
const PURE = read('webui/js/modules/image-profile-pure.js');

check('R640: image-profile-pure.js подключён в index.html', function () {
    assert.ok(INDEX_HTML.indexOf('js/modules/image-profile-pure.js') > 0,
        'нет <script src="js/modules/image-profile-pure.js">');
});

check('R640: чистый слой грузится ПЕРЕД редактором', function () {
    // Ищем именно ТЕГ, а не упоминание пути: в разметке есть комментарий со словами
    // webui/js/modules/image-profiles.js, и поиск подстроки ловил его (тест краснел
    // на корректном порядке — поймано его же первым прогоном).
    const pureAt = INDEX_HTML.indexOf('src="js/modules/image-profile-pure.js');
    const editorAt = INDEX_HTML.indexOf('src="js/modules/image-profiles.js');
    assert.ok(pureAt > 0 && editorAt > 0, 'оба <script> обязаны быть в index.html');
    assert.ok(pureAt < editorAt,
        'порядок обратный: редактор берёт window.ImageProfilePure и без него падает');
});

check('R640: редактор берёт чистый слой и падает громко, если его нет', function () {
    assert.ok(PROFILES.indexOf('window.ImageProfilePure') > 0,
        'редактор не читает window.ImageProfilePure');
    assert.ok(/throw new Error\(/.test(PROFILES),
        'нет громкой ошибки при отсутствии чистого слоя: страница будет «тихо пустой»');
});

check('R640: чистые правила больше НЕ определены в редакторе', function () {
    // Имена выбраны так, чтобы покрыть каждый смысловой блок чистого слоя:
    // контракт, разбор формы, сборка тела, нормализация ответов.
    ['validateForm', 'profileToForm', 'formToProfile', 'normalizeApplyResponse', 'normalizeProfileList']
        .forEach(function (fn) {
            assert.ok(PURE.indexOf('function ' + fn + '(') > 0,
                'чистый слой не содержит ' + fn);
            assert.strictEqual(PROFILES.indexOf('function ' + fn + '('), -1,
                fn + ' снова определён в редакторе — разрез откатили, но оставили копию');
        });
});

check('R640: чистый слой остался чистым (нет DOM и сети)', function () {
    assert.strictEqual(PURE.indexOf('document.'), -1, 'в чистом слое появился document.');
    assert.strictEqual(PURE.indexOf('fetch('), -1, 'в чистом слое появился fetch(');
    assert.strictEqual(PURE.indexOf('localStorage'), -1, 'в чистом слое появился localStorage');
});

check('R640: чистый слой экспортирует себя в window', function () {
    assert.ok(PURE.indexOf('window.ImageProfilePure = pure;') > 0,
        'нет экспорта window.ImageProfilePure');
});

check('R640: публичная точка входа осталась одной', function () {
    // Только НАЗНАЧЕНИЕ в коде: в шапке файла есть строка «Экспорт: window.ImageProfiles =
    // { mount, refresh, pure, _state, _actions }», и подсчёт «любых вхождений» давал
    // ложные 2 (поймано первым прогоном этого же теста).
    const count = (PROFILES.match(/^\s*window\.ImageProfiles = \{/gm) || []).length;
    assert.strictEqual(count, 1, 'window.ImageProfiles должен присваиваться ровно один раз, а не ' + count);
});

if (failures.length) {
    console.error('\nimage-profile-pure-split-r640: ' + failures.length + ' провал(ов)');
    process.exit(1);
}
console.log('\nOK: ' + passed + ' проверок (R640)');

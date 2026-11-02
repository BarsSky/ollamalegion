// scheme-canvas.test.js — R402 (2026-11-05).
//
// ЗАЧЕМ. Схема жизненного цикла запроса должна открываться из WebUI для ТОЙ
// сборки, которая сейчас работает (Настройки → Справка → «Схема работы»).
// Тест держит три вещи, которые ломаются молча:
//   1. docs/scheme.json — валидный граф: все рёбра ссылаются на существующие
//      узлы, у каждого узла есть код и раздел техдокумента;
//   2. схема не тянет внешние CDN (WebUI собирается офлайн) и её файлы на месте;
//   3. ссылка из «Справки» и переводы существуют в ru/en.
//
// Run: node webui/js/modules/scheme-canvas.test.js

'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');

const ROOT = path.resolve(__dirname, '..', '..', '..');
const WEBUI = path.join(ROOT, 'webui');
const SCHEME_JSON = path.join(WEBUI, 'docs', 'scheme.json');
const SCHEME_HTML = path.join(WEBUI, 'scheme.html');
const INDEX_HTML = path.join(WEBUI, 'index.html');
const JS = path.join(WEBUI, 'js', 'modules', 'scheme-canvas.js');
const CSS = path.join(WEBUI, 'css', 'scheme.css');

let passed = 0;
const failures = [];
function check(name, fn) {
    try {
        fn();
        passed++;
        console.log('  \u2713 ' + name);
    } catch (e) {
        failures.push(name + ': ' + (e && e.message));
        console.error('  \u2717 ' + name + ' - ' + (e && e.message));
    }
}

function read(p) {
    assert.ok(fs.existsSync(p), 'нет файла ' + path.relative(ROOT, p));
    return fs.readFileSync(p, 'utf8');
}

const raw = read(SCHEME_JSON);
const scheme = JSON.parse(raw);

check('scheme.json: стадии и узлы не пусты', function () {
    assert.ok(Array.isArray(scheme.stages) && scheme.stages.length >= 6,
        'должно быть минимум 6 стадий конвейера');
    assert.ok(Array.isArray(scheme.nodes) && scheme.nodes.length >= 12,
        'должно быть минимум 12 узлов схемы');
});

check('scheme.json: рёбра ссылаются на существующие узлы', function () {
    const ids = new Set(scheme.nodes.map(n => n.id));
    (scheme.edges || []).forEach(e => {
        assert.ok(ids.has(e.from), 'ребро из несуществующего узла: ' + e.from);
        assert.ok(ids.has(e.to), 'ребро в несуществующий узел: ' + e.to);
    });
});

check('scheme.json: у каждого узла есть стадия, код и раздел техдокумента', function () {
    const stages = new Set(scheme.stages.map(s => s.id));
    scheme.nodes.forEach(n => {
        assert.ok(stages.has(n.stage), 'узел ' + n.id + ' ссылается на неизвестную стадию ' + n.stage);
        assert.ok(Array.isArray(n.code) && n.code.length > 0, 'у узла ' + n.id + ' нет привязки к коду');
        assert.ok(typeof n.doc === 'string' && n.doc.length > 0, 'у узла ' + n.id + ' нет раздела техдокумента');
        assert.ok(Array.isArray(n.detail) && n.detail.length > 0, 'у узла ' + n.id + ' нет описания');
    });
});

check('scheme.json: оба документа существуют в репозитории', function () {
    [scheme.docs.tech, scheme.docs.human].forEach(function (rel) {
        const p = path.resolve(WEBUI, rel);
        assert.ok(fs.existsSync(p), 'нет документа ' + rel + ' (разрешилось в ' + p + ')');
    });
});

check('scheme.html: локальные ассеты и никаких CDN', function () {
    const html = read(SCHEME_HTML);
    assert.ok(html.indexOf('js/modules/scheme-canvas.js') >= 0, 'страница не подключает scheme-canvas.js');
    assert.ok(html.indexOf('css/scheme.css') >= 0, 'страница не подключает scheme.css');
    assert.ok(!/https?:\/\//.test(html.replace(/xmlns="http:\/\/www\.w3\.org\/2000\/svg"/g, '')),
        'в схеме появились внешние URL: WebUI собирается офлайн');
    assert.ok(fs.existsSync(JS), 'нет scheme-canvas.js');
    assert.ok(fs.existsSync(CSS), 'нет scheme.css');
});

check('index.html: ссылка «Схема работы» в Справке', function () {
    const html = read(INDEX_HTML);
    assert.ok(html.indexOf('href="scheme.html"') >= 0, 'в настройках нет ссылки на scheme.html');
    assert.ok(html.indexOf('data-i18n="help.scheme"') >= 0, 'у ссылки нет ключа help.scheme');
});

check('переводы help.scheme / help.scheme_desc есть в ru.js и en.js', function () {
    const ru = read(path.join(WEBUI, 'js', 'i18n', 'ru.js'));
    const en = read(path.join(WEBUI, 'js', 'i18n', 'en.js'));
    ['help.scheme', 'help.scheme_desc'].forEach(function (k) {
        assert.ok(ru.indexOf('"' + k + '"') >= 0, 'нет ' + k + ' в ru.js');
        assert.ok(en.indexOf('"' + k + '"') >= 0, 'нет ' + k + ' в en.js');
    });
});

check('Dockerfile копирует схему и документы в образ', function () {
    const df = read(path.join(ROOT, 'docker', 'webui', 'Dockerfile'));
    assert.ok(df.indexOf('COPY webui/docs/') >= 0, 'в образ не копируется webui/docs (scheme.json)');
    assert.ok(df.indexOf('request-lifecycle.md') >= 0, 'в образ не копируется техдокумент схемы');
});

console.log('');
if (failures.length) {
    console.error('scheme-canvas: ' + failures.length + ' FAILED, ' + passed + ' passed');
    failures.forEach(f => console.error('  - ' + f));
    process.exit(1);
}
console.log('scheme-canvas: OK (' + passed + ' checks passed)');

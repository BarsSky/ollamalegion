// monitor-iframe-guest-gate-r686.test.js — R686 (2026-11-22): iframe монитора
// НЕ начинает работу, пока панель не инициализирована.
//
// ЖИВОЙ ДЕФЕКТ (CDP-замер на стенде, гостевая загрузка, чистый профиль):
//   * панель НЕ инициализирована: boot-гейт app.js не позвал init() (вход не
//     выполнен), поэтому syncMonitorIframePause не вызывался ни разу;
//   * при этом iframe `monitor.html` из разметки уже работал: ~58 попыток /api/
//     за 10 с, ВСЕ 401 (auth.js R223 отвечает синтетическим 401 без сети), и
//     45 предупреждений `[monitor] …: 401` в консоли.
//
// ЧТО СТАЛО: `src` монитора лежит в `data-src` (разметка), а применяется в
// init() — то есть у гостя монитора в документе нет вовсе. `?v=` остаётся в
// HTML, потому что общий cache-токен статики подставляет сборка
// (docker/webui/Dockerfile), и ссылка, переехавшая в JS, перестала бы
// обновляться.
//
// Проверка статическая: поведение загрузки iframe без браузера не
// воспроизвести, а «src снова в разметке»/«init() не создаёт монитор» видно.
//
// Запуск: node webui/tests/monitor-iframe-guest-gate-r686.test.js
'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');

const root = path.join(__dirname, '..');
const html = fs.readFileSync(path.join(root, 'index.html'), 'utf8');
const appJs = fs.readFileSync(path.join(root, 'js/app.js'), 'utf8');

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

/** Тело init() — от «function init()» до следующей функции верхнего уровня. */
function initBody() {
    const start = appJs.indexOf('function init() {');
    assert.ok(start !== -1, 'в app.js нет init()');
    const end = appJs.indexOf('function switchPage(', start);
    assert.ok(end !== -1, 'не найден конец init()');
    return appJs.slice(start, end);
}

check('iframe монитора в разметке НЕ имеет src (иначе грузится до входа)', function () {
    const tag = /<iframe[^>]*id="monitorFrame"[^>]*>/.exec(html);
    assert.ok(tag, 'в index.html нет iframe#monitorFrame');
    assert.strictEqual(/(^|\s)src\s*=/.test(tag[0]), false,
        'у iframe снова есть src: на гостевой загрузке монитор начнёт опрос до init() (~58 запросов/10 с, все 401)');
    assert.strictEqual(/(?:^|[^-a-z])src="monitor\.html/.test(html), false,
        'index.html снова ссылается на monitor.html через src (а не data-src)');
});

check('адрес монитора остался в разметке — в data-src с ?v=', function () {
    const tag = /<iframe[^>]*id="monitorFrame"[^>]*>/.exec(html);
    assert.ok(/\sdata-src="monitor\.html\?v=[A-Za-z0-9._-]+"/.test(tag[0]),
        'нет data-src="monitor.html?v=…": сборка подставляет общий cache-токен в HTML, и ссылка в JS его потеряет');
});

check('src монитора выставляется ВНУТРИ init() — то есть только у инициализированной панели', function () {
    const body = initBody();
    assert.ok(/getAttribute\(\s*['"]data-src['"]\s*\)/.test(body),
        'init() не читает data-src — iframe останется пустым (монитор не заработает)');
    assert.ok(/setAttribute\(\s*['"]src['"]\s*,\s*monitorSrc\s*\)/.test(body),
        'init() не назначает src монитору — адрес из data-src нигде не применяется');
    assert.strictEqual(/monitorFrameEl\.src\s*=/.test(appJs), false,
        'src назначается где-то ещё: единственная точка запуска монитора — init()');
});

check('обработчик load регистрируется ДО назначения src (пауза R679 ловится)', function () {
    const body = initBody();
    const addIdx = body.indexOf("addEventListener('load'");
    const setIdx = body.indexOf('setAttribute');
    assert.ok(addIdx !== -1, 'в init() нет обработчика load монитора');
    assert.ok(setIdx !== -1, 'в init() нет назначения src');
    assert.ok(addIdx < setIdx,
        'src назначается раньше регистрации load-обработчика: событие может пройти мимо, и монитор на скрытой странице останется с опросом (замер до правки — 55 опросов за 10 с)');
});

check('на load отправляется sendMonitorConfig (он заканчивается syncMonitorIframePause)', function () {
    const body = initBody();
    const start = body.indexOf("addEventListener('load'");
    assert.ok(start !== -1, 'нет обработчика load');
    const handler = body.slice(start, start + 260);
    assert.ok(/sendMonitorConfig\(\)/.test(handler),
        'на load не отправляется конфиг+пауза: монитор не узнает ни API base, ни состояние паузы');
});

console.log('\n' + passed + ' проверок зелёные, ' + failures.length + ' красных');
if (failures.length > 0) {
    failures.forEach(function (f) { console.error('  FAIL: ' + f); });
    process.exit(1);
}

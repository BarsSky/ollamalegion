// reexport-nginx-proxy-r709.test.js — R709: контракт «WebUI видит модуль ре-экспорта».
//
// ЖИВОЙ РИСК, КОТОРЫЙ ЗДЕСЬ ЗАКРЕПЛЁН. Блок «Ре-экспорт» в WebUI появляется
// только если nginx панели умеет донести /api/reexport/* до опционального
// сервиса. У этой связки три места, которые обязаны совпадать:
//
//   1. webui/nginx.conf — шаблон с ${REEXPORT_HOST}:${REEXPORT_PORT};
//   2. docker/webui/entrypoint.sh — sed-подстановки этих переменных;
//   3. index.html + compose — модуль подключён, профиль объявлен.
//
// Если шаблон и entrypoint разъедутся, nginx увидит литерал ${REEXPORT_HOST} как
// НЕИЗВЕСТНУЮ переменную и не стартует — то есть отсутствие опционального модуля
// уронило бы всю панель. Ровно этот класс («static proxy_pass на имя, которого нет
// в сети») уже ловили в R229 на /api/ и /api/v1/events, поэтому проверка
// фиксирует и способ обхода: upstream ДОЛЖЕН быть переменной.
//
// Запуск: node webui/tests/reexport-nginx-proxy-r709.test.js
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
        console.error('  \u2717 ' + name + ' — ' + (e && e.message));
    }
}

const ROOT = path.join(__dirname, '..', '..');
const nginx = fs.readFileSync(path.join(ROOT, 'webui', 'nginx.conf'), 'utf8');
const entrypoint = fs.readFileSync(path.join(ROOT, 'docker', 'webui', 'entrypoint.sh'), 'utf8');
const indexHtml = fs.readFileSync(path.join(ROOT, 'webui', 'index.html'), 'utf8');
const compose = fs.readFileSync(path.join(ROOT, 'deployments', 'docker-compose.stack.yml'), 'utf8');

/** placeholders — все ${VAR} шаблона nginx (их подставляет entrypoint). */
function placeholders(text) {
    const out = new Set();
    const re = /\$\{([A-Za-z_][A-Za-z0-9_]*)\}/g;
    let m;
    while ((m = re.exec(text)) !== null) out.add(m[1]);
    return out;
}

check('каждый ${VAR} шаблона nginx подставляется entrypoint', function () {
    const vars = placeholders(nginx);
    assert.ok(vars.size >= 8, 'в шаблоне подозрительно мало переменных: ' + vars.size);
    const missing = [];
    vars.forEach(function (v) {
        const sub = new RegExp('s\\|\\\\\\$\\{' + v + '\\}\\|');
        if (!sub.test(entrypoint)) missing.push(v);
    });
    assert.deepStrictEqual(missing, [],
        'эти переменные останутся в nginx.conf литералами и nginx не стартует: ' + missing.join(', '));
});

check('entrypoint задаёт дефолты REEXPORT_HOST/PORT (иначе панель не поднимется без модуля)', function () {
    assert.ok(/: "\$\{REEXPORT_HOST:=gguf-reexport\}"/.test(entrypoint),
        'нет дефолта REEXPORT_HOST — пустое значение даст upstream ":18095" и nginx упадёт');
    assert.ok(/: "\$\{REEXPORT_PORT:=18095\}"/.test(entrypoint),
        'нет дефолта REEXPORT_PORT');
    assert.ok(/export REEXPORT_HOST REEXPORT_PORT/.test(entrypoint),
        'переменные не экспортированы — escape_sed_repl их не увидит');
});

check('location /api/reexport/ существует и проксирует через ПЕРЕМЕННУЮ', function () {
    const m = nginx.match(/location\s+\/api\/reexport\/\s*\{([\s\S]*?)\n\s{4}\}/);
    assert.ok(m, 'нет location /api/reexport/ — WebUI не узнает о модуле');
    const body = m[1];
    assert.ok(/set \$reexport_upstream "\$\{REEXPORT_HOST\}:\$\{REEXPORT_PORT\}";/.test(body),
        'upstream не задан переменной');
    assert.ok(/proxy_pass http:\/\/\$reexport_upstream/.test(body),
        'proxy_pass должен идти через переменную (R229: статический upstream не даёт nginx стартовать без сервиса)');
    assert.ok(/proxy_read_timeout\s+\d+s;/.test(body), 'нет таймаута чтения для списка моделей');
});

check('отсутствие модуля объясняется своим обработчиком, а не «балансер недоступен»', function () {
    const m = nginx.match(/location\s+@reexport_unavailable\s*\{([\s\S]*?)\n\s{4}\}/);
    assert.ok(m, 'нет @reexport_unavailable');
    assert.ok(/return 424/.test(m[1]),
        'должен возвращаться код, которого нет в error_page этого location (иначе подменится фразой про балансер)');
    assert.ok(/gguf-reexport/.test(m[1]), 'в сообщении не назван модуль');
    assert.ok(/--profile reexport/.test(m[1]), 'в сообщении нет способа включить модуль');
});

check('модуль подключён в index.html ДО рендерера страницы', function () {
    const mod = indexHtml.indexOf('js/modules/gguf-reexport.js');
    const renderer = indexHtml.indexOf('js/modules/gguf-renderer.js');
    assert.ok(mod > 0, 'в index.html нет тега gguf-reexport.js — блок не появится никогда');
    assert.ok(renderer > 0, 'нет gguf-renderer.js');
    assert.ok(mod < renderer, 'модуль должен грузиться ДО gguf-renderer.js (рендерер зовёт mount)');
});

check('хост блока создаётся рендерером пустым (без модуля страница не меняется)', function () {
    const renderer = fs.readFileSync(path.join(ROOT, 'webui', 'js', 'modules', 'gguf-renderer.js'), 'utf8');
    assert.ok(/id="ggufReexportHost"><\/div>/.test(renderer),
        'нет пустого хоста #ggufReexportHost в разметке страницы моделей');
    assert.ok(/window\.GgufReexport\.mount\(\)/.test(renderer),
        'рендерер не вызывает GgufReexport.mount()');
});

check('сервис объявлен в compose под ОТДЕЛЬНЫМ профилем reexport', function () {
    assert.ok(/gguf-reexport:\n\s+image:.*GGUF_REEXPORT_TAG/.test(compose),
        'нет сервиса gguf-reexport с параметризованным тегом');
    const block = compose.match(/\n  gguf-reexport:\n([\s\S]*?)\n\n/);
    assert.ok(block, 'не найден блок сервиса gguf-reexport');
    assert.ok(/profiles: \["reexport"\]/.test(block[1]),
        'сервис обязан быть только под профилем reexport (иначе он появится в full/worker)');
    assert.ok(/MODELS_DIR/.test(block[1]), 'том моделей не подключён');
    assert.ok(/healthcheck:/.test(block[1]), 'нет healthcheck');
    assert.ok(/127\.0\.0\.1:\$\{GGUF_REEXPORT_PORT_HOST/.test(block[1]),
        'порт наружу публиковаться не должен — только loopback');
});

console.log('\n' + passed + ' проверок зелёные, ' + failures.length + ' красных');
if (failures.length > 0) {
    failures.forEach(function (f) { console.error('  FAIL: ' + f); });
    process.exit(1);
}

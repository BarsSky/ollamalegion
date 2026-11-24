// reexport-token-boundary-r743.test.js — R743: внутренний токен модуля
// ре-экспорта подставляет ГРАНИЦА (nginx), а не браузер.
//
// ЖИВОЙ ДЕФЕКТ, ЗАКРЕПЛЁННЫЙ ЗДЕСЬ. С R222 панель больше не вшивает API-токен
// в страницу: браузер ходит сессией (cookie + X-CSRF-Token), а
// WEBUI_CONFIG.API_TOKEN пуст. Модуль `gguf-reexport` на изменяющие запросы
// требует `X-API-Token`, если у него задан `REEXPORT_API_TOKEN` — и «Проверить»/
// «Нормализовать» возвращали:
//
//   401 {"code":"unauthorized","message":"нужен заголовок X-API-Token (REEXPORT_API_TOKEN)"}
//
// при том, что чтение (capabilities/models/jobs) работало: блок и список моделей
// видны, кнопка не работает. «Нормализовано» это так:
//
//   1. источник токена ОДИН — `CPPWORKER_API_TOKEN` в deployments/.env;
//   2. webui-контейнер получает его как API_TOKEN (compose), nginx подставляет
//      `X-API-Token` только на `location /api/reexport/`;
//   3. модуль получает тот же токен как REEXPORT_API_TOKEN (compose);
//   4. в страницу токен по-прежнему НЕ вшивается — R222 не откатывается.
//
// Запуск: node webui/tests/reexport-token-boundary-r743.test.js
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
const compose = fs.readFileSync(path.join(ROOT, 'deployments', 'docker-compose.stack.yml'), 'utf8');

/** locationBody — тело `location /api/reexport/ { ... }` шаблона nginx. */
function locationBody() {
    const m = nginx.match(/location\s+\/api\/reexport\/\s*\{([\s\S]*?)\n\s{4}\}/);
    assert.ok(m, 'нет location /api/reexport/ — WebUI не узнает о модуле');
    return m[1];
}

check('nginx подставляет X-API-Token для /api/reexport/ (иначе кнопки дают 401)', function () {
    const body = locationBody();
    assert.ok(/set \$reexport_api_token "\$\{API_TOKEN\}";/.test(body),
        'нет set $reexport_api_token "${API_TOKEN}" — значение заголовка неоткуда взять');
    assert.ok(/proxy_set_header X-API-Token \$reexport_api_token;/.test(body),
        'нет proxy_set_header X-API-Token — модуль с заданным REEXPORT_API_TOKEN ответит 401');
});

check('entrypoint подставляет ${API_TOKEN} в шаблон nginx и экспортирует его', function () {
    assert.ok(/export REEXPORT_HOST REEXPORT_PORT API_TOKEN/.test(entrypoint),
        'API_TOKEN не экспортирован — escape_sed_repl его не увидит');
    assert.ok(/s\|\\\$\{API_TOKEN\}\|/.test(entrypoint),
        'нет sed-правила для ${API_TOKEN}: nginx получит литерал и не стартует');
});

check(' источник токена ОДИН: и панель, и модуль берут CPPWORKER_API_TOKEN', function () {
    // webui-контейнер: API_TOKEN из CPPWORKER_API_TOKEN (обязательный).
    assert.ok(/^\s*-\s*API_TOKEN=\$\{CPPWORKER_API_TOKEN:\?/m.test(compose),
        'webui не получает API_TOKEN из CPPWORKER_API_TOKEN');
    // модуль: REEXPORT_API_TOKEN из того же CPPWORKER_API_TOKEN.
    assert.ok(/^\s*-\s*REEXPORT_API_TOKEN=\$\{CPPWORKER_API_TOKEN:-\}/m.test(compose),
        'REEXPORT_API_TOKEN модуля берётся не из CPPWORKER_API_TOKEN — источники могут разъехаться');
});

check('в страницу токен по-прежнему НЕ вшивается (R222 не откатываем)', function () {
    assert.ok(/API_TOKEN: '',/.test(entrypoint),
        'config.js снова вшивает API_TOKEN — любой, кто открыл панель, действует как админ (регресс R222)');
    assert.ok(!/API_TOKEN: '\$\{API_TOKEN\}'/.test(entrypoint),
        'подстановка токена в config.js вернулась');
});

check('документация называет границу токена (иначе диагноз снова будет «401 и почему-то»)', function () {
    const doc = fs.readFileSync(path.join(ROOT, 'docs', 'operations', 'gguf-reexport.ru.md'), 'utf8');
    assert.ok(/X-API-Token/.test(doc) && /nginx/i.test(doc),
        'в docs/operations/gguf-reexport.ru.md нет объяснения, кто подставляет X-API-Token');
});

if (failures.length) {
    console.error('\nFAIL: ' + failures.length + ' проверок, passed=' + passed);
    process.exit(1);
}
console.log('\nOK: ' + passed + ' проверок (R743)');

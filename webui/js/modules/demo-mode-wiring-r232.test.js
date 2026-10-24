// demo-mode-wiring-r232.test.js — R232 (2026-10-24): демо-режим ПОДКЛЮЧЁН.
//
// Запуск: node webui/js/modules/demo-mode-wiring-r232.test.js
//
// Демо-режим включается только через env, поэтому кроме логики (demo-mode.test.js)
// надо проверить саму проводку:
//   1. скрипт demo-mode.js и стили demo.css подключены в index.html и monitor.html,
//      причём demo-mode.js идёт после config.js и ДО auth.js (иначе auth.js
//      успевает сходить за /auth/me на сервер и панель получает 401);
//   2. entrypoint webui читает WEBUI_DEMO_MODE и пишет DEMO_MODE в config.js;
//   3. дефолт config.js — DEMO_MODE:false (страница без env не в демо);
//   4. compose прокидывает WEBUI_DEMO_MODE в контейнер webui;
//   5. документация для оператора существует и описывает включение.

'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');

const ROOT = path.join(__dirname, '..', '..', '..');
let passed = 0;
const failures = [];
function check(name, fn) {
  try { fn(); passed++; console.log('  \u2713 ' + name); }
  catch (e) { failures.push(name + ': ' + (e && e.message)); console.error('  \u2717 ' + name + ' - ' + (e && e.message)); }
}
function read(rel) { return fs.readFileSync(path.join(ROOT, rel), 'utf8'); }

console.log('R232: подключение демо-режима');

check('index.html подключает demo-mode.js и demo.css', function () {
  const html = read('webui/index.html');
  assert.ok(/<script[^>]+src="js\/modules\/demo-mode\.js/.test(html), 'нет script demo-mode.js');
  assert.ok(/<link[^>]+href="css\/demo\.css/.test(html), 'нет css demo.css');
  // Демо-скрипт обязан идти ПОСЛЕ config.js (иначе DEMO_MODE ещё не прочитан)
  // и ДО auth.js (иначе перехват fetch не будет внешним слоем и auth.js
  // сходит за /auth/me на реальный сервер — гость увидит 401 в консоли).
  assert.ok(html.indexOf('js/modules/config.js') < html.indexOf('js/modules/demo-mode.js'),
    'demo-mode.js должен подключаться после config.js');
  assert.ok(html.indexOf('js/modules/demo-mode.js') < html.indexOf('js/modules/auth.js'),
    'demo-mode.js должен подключаться до auth.js');
});

check('monitor.html подключает demo-mode.js и demo.css', function () {
  const html = read('webui/monitor.html');
  assert.ok(/<script[^>]+src="js\/modules\/demo-mode\.js/.test(html), 'нет script demo-mode.js');
  assert.ok(/<link[^>]+href="css\/demo\.css/.test(html), 'нет css demo.css');
  assert.ok(html.indexOf('js/modules/config.js') < html.indexOf('js/modules/demo-mode.js'),
    'demo-mode.js должен подключаться после config.js');
  assert.ok(html.indexOf('js/modules/demo-mode.js') < html.indexOf('js/modules/auth.js'),
    'demo-mode.js должен подключаться до auth.js');
});

check('auth.js не ставит свой 401-перехват в демо-режиме', function () {
  const auth = read('webui/js/modules/auth.js');
  assert.ok(/WEBUI_CONFIG\.DEMO_MODE/.test(auth),
    'auth.js должен пропускать установку fetch-обёртки при DEMO_MODE');
});

check('entrypoint читает WEBUI_DEMO_MODE и пишет DEMO_MODE в config.js', function () {
  const sh = read('docker/webui/entrypoint.sh');
  assert.ok(/WEBUI_DEMO_MODE/.test(sh), 'entrypoint не читает WEBUI_DEMO_MODE');
  assert.ok(/DEMO_MODE/.test(sh), 'entrypoint не пишет DEMO_MODE');
  assert.ok(/DEMO_MODE:\s*\$\{DEMO_MODE\}/.test(sh), 'config.js не получает DEMO_MODE');
  // Булево преобразование: 1/true/yes/on → true, всё остальное → false.
  assert.ok(/1\|true\|yes\|on\)/.test(sh), 'нет разбора значений 1/true/yes/on');
});

check('config.js по умолчанию DEMO_MODE:false', function () {
  const cfg = read('webui/js/modules/config.js');
  assert.ok(/DEMO_MODE:\s*false/.test(cfg), 'дефолт должен быть false');
});

check('compose прокидывает WEBUI_DEMO_MODE в webui', function () {
  const yml = read('deployments/docker-compose.stack.yml');
  assert.ok(/WEBUI_DEMO_MODE=\$\{WEBUI_DEMO_MODE:-0\}/.test(yml),
    'в environment сервиса webui нет WEBUI_DEMO_MODE');
});

check('env-примеры документируют WEBUI_DEMO_MODE', function () {
  assert.ok(/WEBUI_DEMO_MODE=0/.test(read('deployments/.env.example')), 'deployments/.env.example');
  assert.ok(/WEBUI_DEMO_MODE=0/.test(read('webui/.env.example')), 'webui/.env.example');
});

check('документация демо-режима существует и объясняет включение', function () {
  const doc = read('docs/features/webui-demo.ru.md');
  assert.ok(/WEBUI_DEMO_MODE=1/.test(doc), 'документ не объясняет включение через env');
  assert.ok(/гост/i.test(doc), 'документ не описывает гостевой доступ');
  const readme = read('README.md');
  assert.ok(readme.indexOf('docs/features/webui-demo.ru.md') >= 0, 'README не ссылается на docs/features/webui-demo.ru.md');
});

console.log('');
if (failures.length) {
  console.error('ИТОГ: FAIL (' + failures.length + ')');
  failures.forEach(f => console.error(' - ' + f));
  process.exit(1);
}
console.log('ИТОГ: PASS (' + passed + ' проверок)');

// gguf-loaded-num-predict-r465.test.js — R463-R465 (2026-11-06).
//
// ЖИВАЯ ЖАЛОБА ОПЕРАТОРА: «num_predict в env выставлен, а в WebUI его значения
// не увидеть». Число существовало в воркере, но терялось в двух местах:
//   1. балансер декодировал `/api/v1/cppworker/config/runtime` в СВОЮ структуру
//      и пересобирал ответ для WebUI — полей default_num_predict /
//      output_budget_ceiling в ней не было, поэтому до интерфейса они не
//      доезжали (класс R134);
//   2. в строке флагов загруженной модели (`buildLoadedFlags`) не было чипа
//      num_predict.
//
// ЧТО ПРОВЕРЯЕТСЯ (по исходникам — это дешёвый сторож контракта, живой ответ
// проверяется на стенде отдельно):
//   1. структура балансера содержит оба json-поля;
//   2. воркер отдаёт эти ключи в runtime;
//   3. в WebUI есть чип num_predict, который их читает;
//   4. воркер в том же ответе отдаёт политику бюджета верхним уровнем
//      (outputBudget) — чтобы «16384» не приходилось искать в исходниках.
//
// Запуск: node webui/js/modules/gguf-loaded-num-predict-r465.test.js

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

const balancerRuntime = read('internal/balancer/llamacpp_runtime_config.go');
const workerConfig = read('cmd/cppworker/handlers_config.go');
const renderer = read('webui/js/modules/gguf-renderer-detail-render.js');

check('R465: структура балансера несёт default_num_predict', function () {
    assert.ok(/json:"default_num_predict/.test(balancerRuntime),
        'в runtimeModelR118 нет поля default_num_predict — WebUI снова получит прочерк');
});

check('R465: структура балансера несёт output_budget_ceiling', function () {
    assert.ok(/json:"output_budget_ceiling/.test(balancerRuntime),
        'в runtimeModelR118 нет поля output_budget_ceiling — потолок не виден');
});

check('R465: воркер отдаёт оба ключа в runtime', function () {
    assert.ok(workerConfig.indexOf('"default_num_predict"') > 0,
        'cppworker не отдаёт default_num_predict в /config/runtime');
    assert.ok(workerConfig.indexOf('"output_budget_ceiling"') > 0,
        'cppworker не отдаёт output_budget_ceiling в /config/runtime');
});

check('R465: воркер отдаёт политику бюджета верхним уровнем', function () {
    assert.ok(workerConfig.indexOf('"outputBudget"') > 0,
        'в ответе /config/runtime нет верхнеуровневого outputBudget');
});

check('R465: в строке флагов загруженной модели есть чип num_predict', function () {
    assert.ok(/label:\s*'num_predict'/.test(renderer),
        'buildLoadedFlags не рисует num_predict — оператор не видит бюджет ответа');
});

check('R465: чип читает ОБА поля (default и потолок)', function () {
    assert.ok(renderer.indexOf('default_num_predict') > 0,
        'чип num_predict не читает default_num_predict');
    assert.ok(renderer.indexOf('output_budget_ceiling') > 0,
        'чип num_predict не читает output_budget_ceiling');
});

check('R465: чип не выдумывает значение, когда полей нет', function () {
    // Условие обязательно проверяет null: иначе при отсутствии данных чип
    // показывал бы «?» как факт.
    const idx = renderer.indexOf("label: 'num_predict'");
    assert.ok(idx > 0, 'чип num_predict не найден');
    const around = renderer.slice(Math.max(0, idx - 1200), idx);
    assert.ok(/npDefault !== null \|\| npCeiling !== null/.test(around),
        'чип num_predict не проверяет наличие данных перед отрисовкой');
});

console.log('');
if (failures.length) {
    console.log('FAILED ' + failures.length + ' of ' + (passed + failures.length));
    failures.forEach(function (f) { console.log('  - ' + f); });
    process.exit(1);
}
console.log('OK: ' + passed + ' проверок');

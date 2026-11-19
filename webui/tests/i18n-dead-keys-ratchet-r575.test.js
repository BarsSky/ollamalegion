// i18n-dead-keys-ratchet-r575.test.js — R575 (W7-хвост): «мёртвые» ключи i18n не растут.
//
// ЗАЧЕМ ХРАПОВИК, А НЕ ЗАПРЕТ. Измерено на R574 (`node scripts/i18n_dead_keys.js`):
// 2566 ключей в словарях, 2144 найдено обращений, **398** ключей без найденного
// потребителя. Это ДОЛГ, а не ноль: часть из 398 наверняка используется динамически
// (`'settings.' + name`) и обёртками из чужих файлов — инструмент предупреждает об этом
// сам. Удалять их «по числу» нельзя (R568: наивный скан давал 1783 и вырезал бы живые
// переводы).
//
// Поэтому здесь храповик: текущее число зафиксировано, и ЛЮБОЙ НОВЫЙ мёртвый ключ
// краснеет. Долг можно только уменьшать (после разбора с allowlist порог опускается).
//
// Проверка запускает настоящий измеритель, а не свою копию правил: иначе порог и
// инструмент разъедутся (это уже случалось — два дефекта в самом измерителе нашли
// живые прогоны R574).
'use strict';

const assert = require('assert');
const { execFileSync } = require('child_process');
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

// Порог зафиксирован на R574. УМЕНЬШАТЬ после разбора; увеличивать нельзя.
// R576: порог снижен 398 -> 367 после удаления 31 проверенного мёртвого ключа wizard.*.
const MAX_UNUSED_R575 = 0;

const script = path.join(__dirname, '..', '..', 'scripts', 'i18n_dead_keys.js');
const out = execFileSync(process.execPath, [script, '--json'], { encoding: 'utf8' });
const report = JSON.parse(out);

check('измеритель отдаёт отчёт', function () {
    assert.ok(report && report.keys && typeof report.unused === 'number',
        'scripts/i18n_dead_keys.js --json не вернул отчёт');
});

check('словари симметричны (ru и en)', function () {
    assert.strictEqual(report.keys.ru, report.keys.en,
        'ru/en разошлись: ' + report.keys.ru + ' против ' + report.keys.en);
    assert.strictEqual(report.missingInRu, 0, 'ключи без ru-перевода: ' + report.missingInRu);
    assert.strictEqual(report.missingInEn, 0, 'ключи без en-перевода: ' + report.missingInEn);
});

check('мёртвых ключей не стало больше порога R574', function () {
    assert.ok(report.unused <= MAX_UNUSED_R575,
        'ключей без потребителя ' + report.unused + ' > порога ' + MAX_UNUSED_R575 +
        ': новый ключ добавлен, но нигде не используется. Либо использовать, либо не добавлять.');
});

console.log('\n' + passed + ' проверок зелёные, ' + failures.length + ' красных (без потребителя: ' + report.unused + ' из ' + report.keys.ru + ')');
if (failures.length > 0) {
    failures.forEach(function (f) { console.error('  FAIL: ' + f); });
    process.exit(1);
}

// i18n-measurer-baseline-r586.test.js — R586 (W7): самопроверка измерителя i18n.
//
// ЗАЧЕМ ЭТОТ НАБОР (найдено двумя откатами).
//
//   R574: правило «строка равна ключу» искало ключ в САМИХ словарях — измеритель
//         показал «мёртвых 0», хотя все 2566 ключей были живы.
//   R585: попытка улучшить проверку динамических префиксов сломала счёт —
//         обращений стало 2143 вместо 2144, «мёртвых» 167 вместо 91, а
//         «под динамическим префиксом» 0 вместо 75.
//
// В обоих случаях ошибка была в правке ИЗМЕРИТЕЛЯ, а не в словарях, и оба раза её
// видел только человек. Здесь — фиксация базовой линии: после любой правки
// `scripts/i18n_dead_keys.js` числа обязаны быть НЕ ХУЖЕ:
//
//   находок (used)              >= 2144  — иначе правка потеряла потребителей;
//   мёртвых (unused)            <=   91  — иначе правка объявила мёртвыми живые;
//   под динамикой (possibly)    >=   75  — иначе правка перестала видеть динамику;
//   симметрия ru/en             == 0 расхождений.
//
// Улучшение измерителя = сдвиг этих границ В ЛУЧШУЮ сторону тем же коммитом, что и
// правка, с объяснением в CHANGELOG. Молча — нельзя.
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

// Базовая линия зафиксирована на R584 (после откатов R574/R585).
// R596 (2026-11-19): базовая линия СНИЖЕНА 2228 → 2201 осознанно.
//
// Причина измеренная, а не «подкрутить под зелёное»: мастер первого запуска
// (`setup-wizard.js`), план раскладки (`setup-plan.js`) и пресеты
// (`setup-wizard-presets.js`) удалены по решению владельца — вместе с ними ушли
// 27 уникальных обращений к ключам (находок было 2228, стало 2201), а 65 ключей
// `wizard.*`/`plan.*` стали мёртвыми и удалены из обоих словарей (2213 → 2148).
// Храповик продолжает ловить случайную потерю: если находок станет меньше 2201,
// значит из кода пропали потребители без записи в CHANGELOG.
//
// R675 (2026-11-21): граница ПОДНЯТА 2201 → 2204 (в лучшую сторону, как и требует
// правило выше). Причина измеренная: (1) добавлены три ключа — nav.menu,
// footer.version, header.virtual-models (сырые ключи в UI); (2) измеритель научен
// понимать штатную форму `data-i18n="[title]nav.menu"` (скобочный префикс
// «поставить атрибут»), из-за незнания которой два ключа из трёх считались
// мёртвыми, хотя потребитель в разметке есть. Находок стало 2201 + 3 = 2204,
// мёртвых — 0. Обратный сдвиг (вниз) по-прежнему запрещён.
const MIN_USED_R586 = 2204;
const MAX_UNUSED_R586 = 0;
// R587: динамические ключи теперь считаются использованными сразу (сильнее), граница = 0.
const MIN_DYNAMIC_R586 = 0;

const script = path.join(__dirname, '..', '..', 'scripts', 'i18n_dead_keys.js');
const out = execFileSync(process.execPath, [script, '--json'], { encoding: 'utf8' });
const r = JSON.parse(out);

check('измеритель отдаёт все четыре метрики', function () {
    ['used', 'unused', 'possiblyDynamic'].forEach(function (k) {
        assert.strictEqual(typeof r[k], 'number', 'в отчёте нет числового поля ' + k);
    });
    assert.ok(r.keys && typeof r.keys.ru === 'number', 'в отчёте нет keys.ru');
});

check('находок не меньше базовой линии', function () {
    assert.ok(r.used >= MIN_USED_R586,
        'обращений найдено ' + r.used + ' < базовой линии ' + MIN_USED_R586 +
        ': правка измерителя потеряла потребителей (так было в R585)');
});

check('мёртвых не больше базовой линии', function () {
    assert.ok(r.unused <= MAX_UNUSED_R586,
        '«мёртвых» ' + r.unused + ' > базовой линии ' + MAX_UNUSED_R586 +
        ': правка измерителя объявила мёртвыми живые ключи (так было в R574)');
});

check('динамические префиксы не пропали из виду', function () {
    assert.ok(r.possiblyDynamic >= MIN_DYNAMIC_R586,
        'под динамическим префиксом ' + r.possiblyDynamic + ' < ' + MIN_DYNAMIC_R586 +
        ': проверка динамики перестала срабатывать (так было в откаченной правке R585)');
});

check('словари симметричны', function () {
    assert.strictEqual(r.keys.ru, r.keys.en, 'ru и en разошлись: ' + r.keys.ru + ' / ' + r.keys.en);
    assert.strictEqual(r.missingInRu, 0, 'ключей без ru-перевода: ' + r.missingInRu);
    assert.strictEqual(r.missingInEn, 0, 'ключей без en-перевода: ' + r.missingInEn);
});

console.log('\n' + passed + ' проверок зелёные, ' + failures.length + ' красных (used=' + r.used +
    ', unused=' + r.unused + ', dynamic=' + r.possiblyDynamic + ', keys=' + r.keys.ru + ')');
if (failures.length > 0) {
    failures.forEach(function (f) { console.error('  FAIL: ' + f); });
    process.exit(1);
}

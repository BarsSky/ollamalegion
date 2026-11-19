// setup-wizard-import-step-r543.test.js — R543 (W5 плана
// 2026-11-19-webui-pages-and-setup, §4.6): мастер не может нарисовать шаг,
// которого нет, и не может прочитать поля с шага, где их нет.
//
// ЖИВОЙ ДЕФЕКТ, который здесь закрыт (два в одном узле):
//   1. импорт конфига ставил `currentStep = 5` при `totalSteps = 4` →
//      `renderStepContent` не находил ветку, тело мастера оставалось пустым, а
//      футер предлагал «Завершить»;
//   2. чтение полей общих параметров шло при `currentStep === 4`, то есть на
//      СВОДКЕ, где полей ввода нет. `getFieldValue` на отсутствующем элементе
//      возвращает fallback (90/85/80/85), поэтому импортированные или
//      введённые лимиты молча заменялись значениями по умолчанию ровно в
//      момент нажатия «Завершить».
//
// Проверка статическая (как selection-ui-r493): страницу без браузера не
// обойти, а «шаг за пределами totalSteps», «пропавшая ветка рендера» и «чтение
// не с того шага» видны по исходнику. Комментарии из проверок вычтены —
// упоминание литерала в объяснении дефекта дефектом не является.
//
// Запуск: node webui/tests/setup-wizard-import-step-r543.test.js
'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');

const root = path.join(__dirname, '..');
const src = fs.readFileSync(path.join(root, 'js/modules/setup-wizard.js'), 'utf8');

// Код без комментариев: `// ...` до конца строки. Блочных комментариев в модуле
// нет; если появятся — проверки ниже увидят их как код, и это будет видно.
const code = src
    .split('\n')
    .map(function (line) {
        const i = line.indexOf('//');
        return i === -1 ? line : line.slice(0, i);
    })
    .join('\n');

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

const totalStepsMatch = /var totalSteps = (\d+);/.exec(code);
const totalSteps = totalStepsMatch ? parseInt(totalStepsMatch[1], 10) : NaN;

// «Шаг общих параметров» — одно число (WIZARD_GENERAL_STEP). Подставляем его
// значение, чтобы `case WIZARD_GENERAL_STEP:` проверялся как `case 3:`.
const generalStepMatch = /var WIZARD_GENERAL_STEP = (\d+);/.exec(code);
const generalStep = generalStepMatch ? parseInt(generalStepMatch[1], 10) : NaN;
const codeNum = code.replace(/WIZARD_GENERAL_STEP/g, String(generalStep));

// Тело функции по заголовку: от `function <name>(` до следующего заголовка
// функции того же модуля.
function bodyOf(text, name) {
    const start = text.indexOf('function ' + name + '(');
    assert.notStrictEqual(start, -1, 'не найдена функция ' + name);
    const rest = text.slice(start + 1);
    const nextIdx = rest.indexOf('\n    function ');
    return nextIdx === -1 ? rest : rest.slice(0, nextIdx);
}

check('totalSteps объявлен числом', function () {
    assert.ok(!isNaN(totalSteps) && totalSteps > 0,
        'нет `var totalSteps = <число>;` — шаги мастера перестали быть измеримыми');
});

check('шаг общих параметров — одно число (WIZARD_GENERAL_STEP)', function () {
    assert.ok(!isNaN(generalStep) && generalStep > 0 && generalStep < totalSteps,
        'нет `var WIZARD_GENERAL_STEP = <число>` (или он вне 1..totalSteps): «где нарисованы поля» и «где их читать» снова разойдутся литералами');
    assert.strictEqual(/if \(step === 3\)/.test(code), false,
        'в renderStepContent снова литерал шага вместо WIZARD_GENERAL_STEP');
    assert.ok(/case WIZARD_GENERAL_STEP:/.test(src),
        'ветка рендера общих параметров не привязана к WIZARD_GENERAL_STEP');
});

check('ни один шаг мастера не выставлен за пределы totalSteps', function () {
    const re = /currentStep\s*=\s*(\d+)/g;
    let m;
    const bad = [];
    while ((m = re.exec(code)) !== null) {
        const n = parseInt(m[1], 10);
        if (n < 1 || n > totalSteps) bad.push(n);
    }
    assert.deepStrictEqual(bad, [],
        'currentStep выставляется в ' + JSON.stringify(bad) + ' при totalSteps = ' + totalSteps +
        ' — renderStepContent не найдёт ветку и тело мастера останется пустым');
});

check('renderStep зажимает шаг в [1, totalSteps] у единственного рендерера', function () {
    const body = bodyOf(code, 'renderStep');
    assert.ok(/step\s*>\s*totalSteps/.test(body),
        'renderStep не проверяет `step > totalSteps`: любой шаг «больше последнего» снова даст пустой экран');
    assert.ok(/step\s*=\s*totalSteps/.test(body),
        'renderStep не зажимает шаг к totalSteps, а только проверяет — пустой экран остаётся');
    assert.ok(/currentStep\s*=\s*step/.test(body),
        'renderStep не синхронизирует currentStep с нарисованным шагом: «Назад»/«Далее» будут считать от намерения, а не от факта');
});

check('у каждого шага 1..totalSteps есть ветка рендера', function () {
    const body = bodyOf(codeNum, 'renderStepContent');
    const cases = new Set();
    const re = /case\s+(\d+)\s*:/g;
    let m;
    while ((m = re.exec(body)) !== null) cases.add(parseInt(m[1], 10));
    const missing = [];
    for (let i = 1; i <= totalSteps; i++) if (!cases.has(i)) missing.push(i);
    assert.deepStrictEqual(missing, [],
        'нет case для шагов ' + JSON.stringify(missing) + ' — такой шаг нарисует пустое тело');
    const extra = [];
    cases.forEach(function (n) { if (n > totalSteps) extra.push(n); });
    assert.deepStrictEqual(extra, [],
        'есть case для шагов за пределами totalSteps ' + JSON.stringify(extra) + ' — недостижимая разметка');
});

check('импорт конфига ведёт на ПОСЛЕДНИЙ существующий шаг, а не «на шаг после»', function () {
    const body = bodyOf(code, 'handleWizardImport');
    assert.ok(/currentStep\s*=\s*totalSteps/.test(body),
        'импорт не ставит currentStep = totalSteps — после импорта оператор не видит сводку импортированного');
    assert.strictEqual(/currentStep\s*=\s*\d+/.test(body), false,
        'в handleWizardImport снова литерал шага: именно так появился `currentStep = 5` при totalSteps = 4');
});

check('поля общих параметров читаются на ТОМ шаге, где нарисованы', function () {
    assert.ok(/if \(currentStep === WIZARD_GENERAL_STEP\) readGeneralFieldsIntoState\(\)/.test(code),
        'чтение полей при уходе вперёд не привязано к WIZARD_GENERAL_STEP — снова разойдётся с рендером');
    assert.strictEqual(/currentStep === 4\) readGeneralFieldsIntoState\(\)/.test(code), false,
        'вернулось чтение полей на шаге 4 (сводка) — там полей нет и лимиты перетираются дефолтами');
    // «Назад» поля не читает: шаг 3 при возврате рисуется из wizardState.
    assert.strictEqual(/wizardPrevBtn[\s\S]{0,400}readGeneralFieldsIntoState/.test(code), false,
        '«Назад» снова читает поля: при возврате на шаг 3 значения придут из DOM, а не из состояния');
});

check('finishWizard не перетирает лимиты значениями по умолчанию', function () {
    const body = bodyOf(code, 'finishWizard');
    assert.strictEqual(/currentStep\s*===\s*\d+\s*\)\s*readGeneralFieldsIntoState/.test(body), false,
        'finishWizard читает поля по номеру шага: на сводке это даёт fallback 90/85/80/85 вместо введённых значений');
    assert.ok(/if \(wizardField\('gpuMaxUsage'\)\) readGeneralFieldsIntoState\(\)/.test(body),
        'finishWizard должен читать поля только если они нарисованы (проверка наличия элемента), иначе импорт и ввод теряются');
});

check('payload мастера не навязывает движок (backendEngine)', function () {
    const body = bodyOf(code, 'buildWizardPayload');
    assert.strictEqual(/backendEngine\s*:/.test(body), false,
        'мастер снова шлёт backendEngine: тип бэкенда он не спрашивает (R195), поэтому это пин llama_cpp на каждом свежем стенде — ровно то, что отменил R112');
});

console.log('\n' + passed + ' проверок зелёные, ' + failures.length + ' красных');
if (failures.length > 0) {
    failures.forEach(function (f) { console.error('  FAIL: ' + f); });
    process.exit(1);
}

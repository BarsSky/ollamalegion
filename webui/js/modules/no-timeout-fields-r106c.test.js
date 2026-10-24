// no-timeout-fields-r106c.test.js — R106c (2026-10-12).
//
// ТРЕБОВАНИЕ ОПЕРАТОРА: «в пресетах стоят таймауты, а мы договорились их убрать;
// надо пройти по всей форме WebUI: если есть выставление таймаутов на работу
// модели — убрать, как и таймауты для балансера. Работа должна чётко идти по
// состоянию, и если ошибка — сразу реагировать, а не дожидаться эфимерного
// времени».
//
// ЧТО ЗАЩИЩАЕТ ЭТОТ ТЕСТ. Таймауты возвращаются в UI незаметно: кто-то добавляет
// «ещё одно поле на всякий случай», и оператор снова получает обрыв длинной
// генерации по часам вместо честной ошибки. Тест сканирует исходники WebUI и
// падает, если в разметке появился ЛЮБОЙ input с id, заканчивающимся на Timeout.
//
// ЧТО ИСКЛЮЧЕНО СОЗНАТЕЛЬНО:
//   * метрики вида id="queueTimeouts" / id="admTimeouts" — это ЧИСЛА в панели
//     монитора, а не настройки (оканчиваются на «Timeouts», правило их не ловит);
//   * таймауты арбитража ресурсов image-воркера (imPolicyWait — ожидание GPU,
//     imPolicyFuse — предохранитель лока): это не «время на работу модели», а
//     защита от зависшего эксклюзивного лока;
//   * серверные значения (Go) остались как были: 0 = авто/по состоянию. Убран
//     только орган управления в UI.
//
// Run: node webui/js/modules/no-timeout-fields-r106c.test.js

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
        console.error('  \u2717 ' + name + ' - ' + (e && e.message));
    }
}

const WEBUI = path.join(__dirname, '..', '..');

/** Собирает все файлы WebUI (.js/.html), которые могут содержать формы. */
function webuiFiles() {
    const out = [];
    const walk = (dir) => {
        for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
            if (entry.name === 'node_modules' || entry.name === 'tests' || entry.name === '.git') continue;
            const full = path.join(dir, entry.name);
            if (entry.isDirectory()) {
                walk(full);
            } else if (/\.(js|html)$/.test(entry.name) && !/\.test\.js$/.test(entry.name)) {
                out.push(full);
            }
        }
    };
    walk(WEBUI);
    return out;
}

const files = webuiFiles();
assert.ok(files.length > 50, 'подозрительно мало файлов WebUI: ' + files.length);

// Правило: input/select с id, заканчивающимся ровно на "Timeout".
// "queueTimeouts"/"admTimeouts" (метрики) под него не попадают — там "Timeouts".
const TIMEOUT_ID_RE = /id="([A-Za-z0-9_]*Timeout)"/g;

check('в формах WebUI нет ни одного поля с id="…Timeout"', function () {
    const hits = [];
    for (const f of files) {
        const src = fs.readFileSync(f, 'utf8');
        let m;
        TIMEOUT_ID_RE.lastIndex = 0;
        while ((m = TIMEOUT_ID_RE.exec(src)) !== null) {
            hits.push(path.relative(WEBUI, f) + ': id="' + m[1] + '"');
        }
    }
    assert.strictEqual(hits.length, 0,
        'найдены поля-таймауты, которые оператор просил убрать (работа идёт по состоянию, ' +
        'а не по таймеру):\n    ' + hits.join('\n    '));
});

check('per-model мастер: таймауты и «Reasoning mode (×3 таймаут)» убраны', function () {
    const src = fs.readFileSync(path.join(WEBUI, 'js', 'modules', 'cppworker-params.js'), 'utf8');
    // Ищем ВЫЗОВЫ/идентификаторы, а не упоминания в комментариях (в них имена
    // оставлены намеренно — чтобы следующая сессия понимала, что здесь было).
    for (const gone of ['wizStreamingTimeout', 'wizStreamingIdleTimeout', 'wizRequestTimeout',
        'wizFirstByteTimeout', 'wizReasoningMode',
        'estimateStreamTimeoutGB(', 'estimateIdleTimeoutGB(', 'getModelSizeBytesForName(',
        'state.reasoningMode']) {
        assert.ok(src.indexOf(gone) === -1,
            'в cppworker-params.js остался артефакт таймаутов: ' + gone);
    }
    // Но управление размышлениями (R106b) обязано остаться — это не таймаут.
    assert.ok(/id="wizEnableReasoning"/.test(src),
        'поле «Размышления» у конкретной модели пропало — его убирать не просили');
});

check('страница «Настройки»: таймауты балансера/агента убраны из формы', function () {
    const html = fs.readFileSync(path.join(WEBUI, 'index.html'), 'utf8');
    for (const gone of ['rpcCoordinatorTimeout', 'virtualModelsTimeout', 'agentTimeout']) {
        assert.ok(html.indexOf(gone) === -1, 'в index.html остался input: ' + gone);
    }
    const app = fs.readFileSync(path.join(WEBUI, 'js', 'app.js'), 'utf8');
    for (const gone of ["'rpcCoordinatorTimeout'", "'virtualModelsTimeout'", "'agentTimeout'"]) {
        assert.ok(app.indexOf(gone) === -1, 'app.js всё ещё сохраняет список полей с ' + gone);
    }
});

check('страница Monitor: «Pull Timeout» убран, чтение поля null-safe', function () {
    const html = fs.readFileSync(path.join(WEBUI, 'monitor.html'), 'utf8');
    assert.ok(html.indexOf('autoPullPullTimeout') === -1,
        'в monitor.html остался input autoPullPullTimeout');
    const initJs = fs.readFileSync(path.join(WEBUI, 'js', 'monitor', 'init.js'), 'utf8');
    assert.ok(initJs.indexOf("getElementById('autoPullPullTimeout').value") === -1,
        'init.js читает несуществующее поле — будет TypeError при сохранении auto-pull');
    const ui = fs.readFileSync(path.join(WEBUI, 'js', 'monitor', 'ui-renderer.js'), 'utf8');
    assert.ok(ui.indexOf('pullTimeoutEl') === -1,
        'ui-renderer.js всё ещё обращается к pullTimeoutEl (элемента больше нет)');
});

check('image policy: ожидание загрузки модели убрано из формы', function () {
    const src = fs.readFileSync(path.join(WEBUI, 'js', 'modules', 'image-resources-policy.js'), 'utf8');
    // INPUT в разметке формы быть не должно…
    assert.ok(src.indexOf('id="imPolicyLoadTimeout"') === -1,
        'в форме image-политики остался input ожидания загрузки модели');
    // …а вот null-safe читатель readForm() обязан остаться: при отсутствии поля
    // берётся ДЕЙСТВУЮЩЕЕ значение, иначе сохранение формы сбросило бы политику.
    assert.ok(/byId\('imPolicyLoadTimeout'\)/.test(src) && /toolLoadTimeoutSec:\s*loadSec/.test(src),
        'readForm потерял null-safe fallback на действующее значение toolLoadTimeoutSec');
});

check('настройки моделей не дублируются в «Настройках» главного меню', function () {
    // Оператор (R106c): «в отдельную вкладку Настройки вынесены профили и настройки
    // моделей, что никак не даёт удобно настраивать загружаемые модели; в этих
    // настройках нет смысла, и они мешают». Настройка живёт на странице GGUF Models.
    const html = fs.readFileSync(path.join(WEBUI, 'index.html'), 'utf8');
    for (const gone of ['cppProfilesList', 'cppProfileAddBtn', 'cppLoadDefaults', 'cppModelCatalog']) {
        assert.ok(html.indexOf(gone) === -1,
            'в index.html остался блок ' + gone + ' — он должен быть только на странице GGUF');
    }
    const gguf = fs.readFileSync(path.join(WEBUI, 'js', 'modules', 'gguf-renderer-detail-render.js'), 'utf8');
    assert.ok(/id="cppProfilesList"/.test(gguf), 'на странице GGUF пропал список профилей моделей');
    assert.ok(/data-role="cpp-load-defaults"/.test(gguf), 'на странице GGUF пропал блок «что получат новые модели»');
    assert.ok(/data-role="cpp-model-catalog"/.test(gguf), 'на странице GGUF пропал блок «модели в папке»');
});

console.log('');
if (failures.length) {
    console.error('no-timeout-fields-r106c: ' + failures.length + ' FAILED, ' + passed + ' passed');
    failures.forEach(function (f) { console.error('  - ' + f); });
    process.exit(1);
}
console.log('no-timeout-fields-r106c: OK (' + passed + ' checks passed)');

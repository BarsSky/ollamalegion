// ctx-contract-r675.test.js — R675 (2026-11-21): контракт фабрик `window.X = function (ctx)`.
//
// ЗАЧЕМ ЗДЕСЬ ОБЁРТКА, А НЕ ЛОГИКА. Правило живёт в
// `scripts/webui_ctx_contract_check.js` — самостоятельном чекере без зависимостей:
// его можно запустить одной командой (в CI-джобе статических проверок), он не
// требует npm install и не тащит за собой `window`/DOM. Тест — тонкая обёртка по
// двум причинам:
//
//   1. Раннер WebUI (`scripts/run-webui-tests.js`) запускает ВСЁ, что лежит в
//      `webui/**` и названо `*.test.js`. Никакого списка файлов руками (R493):
//      значит, новый чекер нельзя «забыть добавить» — достаточно имени файла.
//   2. Тест обязан доказать, что правило СРАБАТЫВАЕТ, а не только что репозиторий
//      зелёный. Чекер, который молча всегда возвращает 0, выглядит зелёным и
//      бесполезен; поэтому вторая половина набора делает копию `webui/js` во
//      временном каталоге, ЛОМАЕТ её известным дефектом класса R668/R672 и
//      требует КРАСНОГО результата с конкретным сообщением.
//
// ЗАЧЕМ ЭТОТ КЛАСС ПРОВЕРЯЕТСЯ ВООБЩЕ. R668/R669/R671/R671b/R672/R674 — один и
// тот же дефект: модуль вынесли в фабрику, а зависимости в ctx не сверили.
// В браузере это «кнопка не работает», «таб не прожимается», «сохранено, а данные
// старые» — то есть выглядит как сломанный UI, а не как ошибка в одном литерале.
'use strict';

const assert = require('assert');
const { execFileSync } = require('child_process');
const fs = require('fs');
const os = require('os');
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
const CHECKER = path.join(ROOT, 'scripts', 'webui_ctx_contract_check.js');
const WEBUI = path.join(ROOT, 'webui');

/** Запуск чекера: {status, out} без падения процесса на ненулевом коде. */
function runChecker(args) {
    try {
        const out = execFileSync(process.execPath, [CHECKER].concat(args || []), {
            encoding: 'utf8',
            stdio: ['ignore', 'pipe', 'pipe'],
        });
        return { status: 0, out };
    } catch (e) {
        return {
            status: typeof e.status === 'number' ? e.status : 1,
            out: ((e.stdout || '') + (e.stderr || '')).toString(),
        };
    }
}

/** Копия боевых (не тестовых) скриптов WebUI во временный каталог. */
function copyWebuiJs(dest) {
    const jsRoot = path.join(WEBUI, 'js');
    const outRoot = path.join(dest, 'webui', 'js');
    fs.mkdirSync(outRoot, { recursive: true });
    let copied = 0;
    (function walk(src, dst) {
        for (const entry of fs.readdirSync(src, { withFileTypes: true })) {
            if (entry.name === 'node_modules') continue;
            const from = path.join(src, entry.name);
            const to = path.join(dst, entry.name);
            if (entry.isDirectory()) {
                fs.mkdirSync(to, { recursive: true });
                walk(from, to);
                continue;
            }
            if (!entry.name.endsWith('.js')) continue;
            if (entry.name.endsWith('.test.js')) continue;
            fs.copyFileSync(from, to);
            copied++;
        }
    })(jsRoot, outRoot);
    return { root: path.join(dest, 'webui'), copied };
}

check('чекер существует и разбирается как JS', function () {
    assert.ok(fs.existsSync(CHECKER), 'нет scripts/webui_ctx_contract_check.js');
    const src = fs.readFileSync(CHECKER, 'utf8');
    // Без зависимостей: ни одного require внешнего пакета.
    const requires = (src.match(/require\(['"]([^'"]+)['"]\)/g) || [])
        .map((s) => s.replace(/^require\(['"]|['"]\)$/g, ''));
    for (const r of requires) {
        assert.ok(r === 'fs' || r === 'path' || r.startsWith('.'), 'внешняя зависимость: ' + r);
    }
});

check('текущий код: контракты фабрик сходятся (exit 0)', function () {
    const res = runChecker([]);
    assert.strictEqual(res.status, 0, 'чекер красный:\n' + res.out);
    assert.ok(/фабрик window\.X = function \(ctx\) проверено: 1[0-9]/.test(res.out),
        'чекер не нашёл фабрики (проверка прошла «по пустоте»):\n' + res.out);
});

check('правило СРАБАТЫВАЕТ: потерянное имя и инертный ключ краснеют', function () {
    const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'r675-ctx-'));
    try {
        const { root, copied } = copyWebuiJs(tmp);
        assert.ok(copied > 40, 'скопировано подозрительно мало файлов: ' + copied);

        // (1) Дефект класса R673: имя пришло в ctx у вызывающего, но привязку в
        // фабрике забыли → тело падает на ReferenceError.
        const modelsFile = path.join(root, 'js', 'modules', 'app-models.js');
        let models = fs.readFileSync(modelsFile, 'utf8');
        const bindRe = /[ \t]*var fetchClusterState = ctx\.fetchClusterState;\n/;
        assert.ok(bindRe.test(models), 'в app-models.js нет привязки fetchClusterState — тест устарел');
        models = models.replace(bindRe, '');
        fs.writeFileSync(modelsFile, models);

        // (2) Дефект класса R671b: фабрика читает ctx.badge, вызывающий не передаёт.
        const renderersFile = path.join(root, 'js', 'modules', 'renderers.js');
        let renderers = fs.readFileSync(renderersFile, 'utf8');
        const badgeRe = /[ \t]*badge: badge,\n/;
        assert.ok(badgeRe.test(renderers), 'в renderers.js нет ключа badge: badge — тест устарел');
        renderers = renderers.replace(badgeRe, '');
        fs.writeFileSync(renderersFile, renderers);

        const res = runChecker(['--root=' + root]);
        assert.strictEqual(res.status, 1, 'сломанный код остался зелёным:\n' + res.out);
        assert.ok(/fetchClusterState/.test(res.out),
            'потерянное имя fetchClusterState не названо:\n' + res.out);
        assert.ok(/ключ badge передан|ctx\.badge/.test(res.out),
            'непереданная зависимость badge не названа:\n' + res.out);
    } finally {
        fs.rmSync(tmp, { recursive: true, force: true });
    }
});

check('исключения не превратились в свалку: их ровно четыре и они проверяются', function () {
    const src = fs.readFileSync(CHECKER, 'utf8');
    const blocks = src.split('const KNOWN_FRAGILE = [')[1] || '';
    const body = blocks.split('\n];')[0];
    const entries = (body.match(/\n    \{/g) || []).length;
    assert.strictEqual(entries, 4,
        'исключений стало ' + entries + ': список должен расти только с разбором ' +
        '(showToast ×2, _t, затенение ctx) — иначе он разрешает всё подряд');
    // Каждое исключение обязано иметь причину и (для guarded-optional) проверку формы.
    assert.ok(/name: 'ctx'/.test(body), 'нет исключения для затенения ctx');
    assert.ok(/pinnedShadows: 3/.test(body), 'не зафиксировано число затенений ctx');
});

check('предпосылка Dockerfile: боевой файл не называется *.test.js', function () {
    // Dockerfile-слой `find … -name '*.test.js' -delete` безопасен только если
    // ни один прод-файл так не назван и ни одна страница на такие файлы не
    // ссылается (иначе из образа вычистилось бы нужное).
    const html = fs.readdirSync(WEBUI).filter((n) => n.endsWith('.html'));
    for (const page of html) {
        const src = fs.readFileSync(path.join(WEBUI, page), 'utf8');
        const refs = src.match(/["'][^"']*\.test\.js["']/g);
        assert.strictEqual(refs, null, page + ' ссылается на тестовый файл: ' + refs);
    }
    const dockerfile = fs.readFileSync(path.join(ROOT, 'docker', 'webui', 'Dockerfile'), 'utf8');
    assert.ok(/\-name '\*\.test\.js' \-delete/.test(dockerfile),
        'в docker/webui/Dockerfile нет удаления *.test.js из образа');
});

console.log('\n' + passed + ' проверок зелёные, ' + failures.length + ' красных');
if (failures.length > 0) {
    failures.forEach(function (f) { console.error('  FAIL: ' + f); });
    process.exit(1);
}

// autotune-selection-survives-r680.test.js — R680 (2026-11-21): выбор оператора
// в списке моделей AutoTune переживает пересборку списка.
//
// ЖИВОЙ ДЕФЕКТ. `renderPerModelList()` собирал список моделей строкой и писал её
// в `list.innerHTML` (`autotune_settings.js`, до правки). Список — это `<select>`
// на каждую модель (inherit / ON / OFF), и `innerHTML` создавал НОВЫЕ узлы на
// каждый проход: `render()` зовётся тиком авто-обновления (`DataRefresh.register
// ('settings', …)`), кнопкой «Обновить», и после «Сохранить». Заслон R513
// (RefreshGuard) спасал только тот случай, когда фокус СЕЙЧАС внутри списка;
// стоило увести фокус — пересборка возвращала селектор к СЕРВЕРНОМУ значению.
// Следствие: «Сохранить» отправлял `perModel`, которого оператор не выбирал, —
// молча, без ошибки в консоли.
//
// ЧТО ПРОВЕРЯЕТСЯ (поведенчески, на настоящем модуле):
//   1. выбор оператора (OFF→ON для одной модели) переживает пересборку, и это
//      именно ПЕРЕСБОРКА (узел новый), а не «ничего не произошло»;
//   2. соседняя модель, которую оператор не трогал, остаётся на серверном
//      значении (перенос не «залил» все селекторы одним значением);
//   3. «Сохранить» отправляет ВЫБОР ОПЕРАТОРА (а не серверное значение) —
//      то есть дефект закрыт по последствию, а не только по DOM;
//   4. заслон R513 не ослаблен: пока фокус внутри списка, узлы не подменяются;
//   5. статически: список больше не собирается через innerHTML, а пересборка
//      идёт DocumentFragment'ом + SafeForm.collectInputs/restoreInputs.
//
// ДОМ в тесте — минимальный (createElement/createDocumentFragment/select-value),
// потому что весь смысл правки в поведении DOM при замене содержимого списка.
//
// Запуск: node webui/tests/autotune-selection-survives-r680.test.js
'use strict';

const assert = require('assert');
const fs = require('fs');
const path = require('path');

const root = path.join(__dirname, '..');
const read = (rel) => fs.readFileSync(path.join(root, rel), 'utf8');
const autotuneSrc = read('js/modules/autotune_settings.js');

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

/** codeOf — исходник без комментариев (в шапке модуля дефект ЦИТИРУЕТСЯ). */
function codeOf(src) {
    const noBlock = src.replace(/\/\*[\s\S]*?\*\//g, '');
    return noBlock.split('\n').map(function (line) {
        const i = line.indexOf('//');
        return i === -1 ? line : line.slice(0, i);
    }).join('\n');
}

// ===========================================================================
// МИНИМАЛЬНЫЙ DOM
// ===========================================================================

function isFragment(node) {
    return !!(node && node.__fragment === true);
}

function FakeNode(tagName) {
    this.tagName = String(tagName || 'div').toUpperCase();
    this.childNodes = [];
    this.parentNode = null;
    this.attributes = Object.create(null);
    this.className = '';
    this.id = '';
    this.type = '';
    this.checked = false;
    this.selected = false;
    this._text = '';
    this.value = '';
    // dataset — как в браузере: data-атрибуты видны как camelCase-поля
    // (saveConfig читает cb.dataset.model, setupHandlers — saveBtn.dataset.bound).
    this.dataset = Object.create(null);
}
FakeNode.prototype.appendChild = function (node) {
    if (isFragment(node)) {
        node.childNodes.slice().forEach(this.appendChild, this);
        node.childNodes.length = 0;
        return node;
    }
    if (node.parentNode) node.parentNode.removeChild(node);
    node.parentNode = this;
    this.childNodes.push(node);
    return node;
};
FakeNode.prototype.removeChild = function (node) {
    const i = this.childNodes.indexOf(node);
    if (i >= 0) {
        this.childNodes.splice(i, 1);
        node.parentNode = null;
    }
    return node;
};
FakeNode.prototype.setAttribute = function (k, v) {
    this.attributes[k] = String(v);
    if (String(k).indexOf('data-') === 0) {
        const camel = String(k).slice(5).replace(/-([a-z])/g, function (m, c) { return c.toUpperCase(); });
        this.dataset[camel] = String(v);
    }
};
FakeNode.prototype.getAttribute = function (k) {
    return Object.prototype.hasOwnProperty.call(this.attributes, k) ? this.attributes[k] : null;
};
// Геттеры/сеттеры — только через defineProperty: `prototype.get x() {}` не парсится.
Object.defineProperties(FakeNode.prototype, {
    children: { get: function () { return this.childNodes.slice(); } },
    firstChild: { get: function () { return this.childNodes[0] || null; } },
    textContent: {
        get: function () { return this._text; },
        set: function (v) {
            this._text = String(v);
            this.childNodes.length = 0;
        }
    },
    /**
     * innerHTML — МИНИ-РАЗБОР той разметки, которую умеет писать этот модуль.
     *
     * ЗАЧЕМ. Тест обязан ловить дефект на СТАРОМ коде (сборка списка строкой
     * `list.innerHTML = html`), а не падать на «в моке DOM нет парсера». Поэтому
     * присваивание HTML-строки порождает те же узлы (`<select>` с `<option>`),
     * что и браузер: тогда «выбор оператора потерян» — это провал ПО СМЫСЛУ, а не
     * по отсутствию узлов. Разбор сознательно узкий: только select/option/hint,
     * других присваиваний innerHTML в autotune_settings.js нет.
     */
    innerHTML: {
        get: function () { return this._html || ''; },
        set: function (html) {
            const src = String(html === undefined || html === null ? '' : html);
            this._html = src;
            this._text = '';
            this.childNodes.length = 0;
            const chunks = src.split('<select').slice(1);
            if (chunks.length === 0) {
                const hint = /<div class="hint">([\s\S]*?)<\/div>/.exec(src);
                if (hint) {
                    const div = new FakeNode('div');
                    div.className = 'hint';
                    div.textContent = hint[1];
                    this.appendChild(div);
                }
                return;
            }
            chunks.forEach(function (chunk) {
                const openEnd = chunk.indexOf('>');
                const openTag = chunk.slice(0, openEnd);
                const rest = chunk.slice(openEnd + 1);
                const closeAt = rest.indexOf('</select>');
                const inner = rest.slice(0, closeAt);
                const model = /data-model="([^"]*)"/.exec(openTag);
                const select = new FakeSelect();
                select.className = 'autotune-per-model-override';
                if (model) select.setAttribute('data-model', model[1]);
                const optRe = /<option value="([^"]*)"([^>]*)>([^<]*)<\/option>/g;
                let m;
                while ((m = optRe.exec(inner))) {
                    const option = new FakeNode('option');
                    option.value = m[1];
                    option.textContent = m[3];
                    option.selected = /\bselected\b/.test(m[2]);
                    select.appendChild(option);
                }
                this.appendChild(select);
            }, this);
        }
    }
});
FakeNode.prototype.contains = function (node) {
    let el = node;
    while (el) {
        if (el === this) return true;
        el = el.parentNode;
    }
    return false;
};
FakeNode.prototype.descendants = function () {
    const out = [];
    (function walk(n) {
        n.childNodes.forEach(function (c) { out.push(c); walk(c); });
    })(this);
    return out;
};
FakeNode.prototype.querySelectorAll = function (selector) {
    return this.descendants().filter(function (n) { return matches(n, selector); });
};

/** FakeSelect — то, ради чего всё: у `<select>` есть ПОВЕДЕНИЕ `.value`. */
function FakeSelect() {
    FakeNode.call(this, 'select');
    this._value = '';   // у select нет собственного поля value — есть выбранный option
    delete this.value;
}
FakeSelect.prototype = Object.create(FakeNode.prototype);
FakeSelect.prototype.constructor = FakeSelect;
FakeSelect.prototype.options = function () {
    return this.childNodes.filter(function (n) { return n.tagName === 'OPTION'; });
};
Object.defineProperty(FakeSelect.prototype, 'value', {
    get: function () {
        const opts = this.options();
        const selected = opts.filter(function (o) { return o.selected; })[0];
        if (selected) return String(selected.value);
        // Как в браузере: у select-one без явного selected выбран первый option.
        return opts.length ? String(opts[0].value) : '';
    },
    set: function (v) {
        const want = String(v === undefined || v === null ? '' : v);
        const opts = this.options();
        let hit = false;
        opts.forEach(function (o) {
            const on = String(o.value) === want;
            o.selected = on;
            if (on) hit = true;
        });
        this._value = hit ? want : '';
    }
});

function matches(node, selector) {
    const sel = String(selector || '').trim();
    if (!sel) return false;
    if (sel[0] === '[' && sel[sel.length - 1] === ']') {
        return node.getAttribute(sel.slice(1, -1)) !== null;
    }
    if (sel[0] === '#') return node.id === sel.slice(1);
    const parts = sel.split('.');
    if (parts[0] && node.tagName !== parts[0].toUpperCase()) return false;
    const classes = String(node.className || '').split(/\s+/);
    return parts.slice(1).every(function (c) { return classes.indexOf(c) !== -1; });
}

function makeFragment() {
    const frag = new FakeNode('#fragment');
    frag.__fragment = true;
    return frag;
}

function makeDocument() {
    const body = new FakeNode('body');
    const byId = Object.create(null);
    const doc = {
        body: body,
        activeElement: null,
        createElement: function (tag) {
            return String(tag).toLowerCase() === 'select' ? new FakeSelect() : new FakeNode(tag);
        },
        createDocumentFragment: makeFragment,
        getElementById: function (id) { return byId[id] || null; },
        querySelector: function () { return null; },
        querySelectorAll: function (sel) { return body.querySelectorAll(sel); },
        addEventListener: function () {},
        __byId: byId,
        __body: body
    };
    return doc;
}

/** register — положить узел в документ и в индекс по id (как разметка index.html). */
function register(doc, id) {
    const el = new FakeNode('div');
    el.id = id;
    doc.__body.appendChild(el);
    doc.__byId[id] = el;
    return el;
}

// ===========================================================================
// ПЕСОЧНИЦА: настоящие safe-form.js + autotune_settings.js
// ===========================================================================

const CONFIG_ENDPOINT = '/api/v1/admin/autotune/config';
const LIST_ENDPOINT = '/api/v1/admin/autotune';

function buildEnvironment(serverConfig) {
    const doc = makeDocument();

    // #autotune-global-enabled — тумблер (R676: пишется через SafeForm.setChecked)
    const globalCb = new FakeNode('input');
    globalCb.type = 'checkbox';
    globalCb.id = 'autotune-global-enabled';
    globalCb.checked = true;
    doc.__byId['autotune-global-enabled'] = globalCb;
    doc.__body.appendChild(globalCb);

    register(doc, 'autotune-save-status');
    register(doc, 'autotune-per-model-list');

    // #autotune-save-btn — кнопка «Сохранить» (обработчик вешает setupHandlers)
    const saveBtn = new FakeNode('button');
    saveBtn.id = 'autotune-save-btn';
    saveBtn.listeners = Object.create(null);
    saveBtn.addEventListener = function (ev, fn) { this.listeners[ev] = fn; };
    doc.__byId['autotune-save-btn'] = saveBtn;
    doc.__body.appendChild(saveBtn);

    // Сервер: GET отдаёт сохранённое, PUT сохраняет — как настоящий балансер.
    const state = {
        globalEnabled: serverConfig.globalEnabled !== false,
        perModel: Object.assign({}, serverConfig.perModel || {})
    };
    const models = serverConfig.models || [];
    const requests = [];
    function jsonResponse(payload, ok) {
        return {
            ok: ok !== false,
            status: ok === false ? 500 : 200,
            json: async function () { return payload; },
            text: async function () { return JSON.stringify(payload); }
        };
    }
    const fetchMock = async function (url, opts) {
        requests.push({ url: url, method: (opts && opts.method) || 'GET', body: opts && opts.body });
        if (url === CONFIG_ENDPOINT) {
            if (opts && opts.method === 'PUT') {
                const body = JSON.parse(opts.body);
                state.globalEnabled = body.globalEnabled;
                state.perModel = body.perModel || {};
                return jsonResponse(state);
            }
            return jsonResponse(state);
        }
        if (url === LIST_ENDPOINT) {
            return jsonResponse({
                backends: models.map(function (m) { return { plan: { ModelName: m } }; })
            });
        }
        return jsonResponse({}, false);
    };

    // Подставное окружение — ДО загрузки модуля.
    global.window = global;
    global.document = doc;
    global.fetch = fetchMock;
    global.console = console;
    const providers = Object.create(null);
    global.DataRefresh = {
        register: function (name, fn) { providers[name] = fn; }
    };
    // R513: заслон. По умолчанию «фокус вне списка» (именно этот случай и терял
    // выбор); тест про фокус включает его явно.
    const focus = { inside: false };
    global.RefreshGuard = {
        isEditingIn: function (el) { return focus.inside && el === doc.__byId['autotune-per-model-list']; },
        isEditable: function () { return true; }
    };

    // Модули: сначала SafeForm (его зовёт пересборка), затем AutoTune.
    delete require.cache[require.resolve('./../js/modules/safe-form.js')];
    delete require.cache[require.resolve('./../js/modules/autotune_settings.js')];
    require('../js/modules/safe-form.js');
    require('../js/modules/autotune_settings.js');
    assert.ok(global.SafeForm, 'safe-form.js не выставил window.SafeForm');
    assert.ok(global.AutoTuneSettings, 'autotune_settings.js не выставил window.AutoTuneSettings');

    const list = doc.__byId['autotune-per-model-list'];
    return {
        doc: doc,
        list: list,
        state: state,
        requests: requests,
        providers: providers,
        focus: focus,
        saveBtn: saveBtn,
        globalCb: globalCb,
        selectFor: function (model) {
            const nodes = list.querySelectorAll('select.autotune-per-model-override');
            return nodes.filter(function (n) { return n.getAttribute('data-model') === model; })[0] || null;
        },
        async refreshByTick() {
            const tick = providers['settings'];
            assert.strictEqual(typeof tick, 'function',
                'модуль не зарегистрировал провайдер страницы «Настройки» в DataRefresh');
            await tick();
        },
        async clickSave() {
            const handler = saveBtn.listeners.click;
            assert.strictEqual(typeof handler, 'function', 'на #autotune-save-btn не навешен обработчик');
            await handler();
        }
    };
}

// ===========================================================================
// ПРОВЕРКИ
// ===========================================================================
//
// Асинхронные шаги (render/тик/«Сохранить») выполняются ДО проверок, а сами
// проверки — синхронные `check()`: так падение называет дефект, а не «тест упал
// на подготовке». Снимки узлов/запросов снимаются по ходу сценария.

const main = async function () {
    // --- сценарий 1: оператор меняет override и уходит из списка ------------
    const env = buildEnvironment({
        globalEnabled: true,
        // Сервер: model-a явно OFF, model-b наследует глобальное (ON).
        perModel: { 'model-a': false },
        models: ['model-a', 'model-b']
    });

    await global.AutoTuneSettings.init();

    const snap = {
        firstA: env.selectFor('model-a'),
        firstB: env.selectFor('model-b')
    };
    // Значения снимаем СРАЗУ: узлы живут дальше, а «первый рендер» — это снимок.
    snap.firstAValue = snap.firstA ? snap.firstA.value : null;
    snap.firstBValue = snap.firstB ? snap.firstB.value : null;
    // Оператор выбрал ON для model-b и УШЁЛ из списка (фокус вне — заслон R513
    // такой случай не покрывает), затем прошёл тик авто-обновления.
    snap.firstB.value = 'true';
    env.focus.inside = false;
    await env.refreshByTick();
    snap.secondB = env.selectFor('model-b');
    snap.secondA = env.selectFor('model-a');

    // «Сохранить» — что реально уходит на сервер.
    await env.clickSave();
    snap.puts = env.requests.filter(function (r) { return r.method === 'PUT'; });
    snap.sent = snap.puts.length ? JSON.parse(snap.puts[0].body) : null;
    snap.thirdB = env.selectFor('model-b');
    snap.thirdBase = global.SafeForm.baseOf(snap.thirdB);

    check('R680/2: первый рендер рисует СЕРВЕРНЫЕ значения', function () {
        assert.ok(snap.firstA && snap.firstB, 'первый рендер не нарисовал селекторы моделей');
        assert.strictEqual(snap.firstAValue, 'false', 'model-a нарисована не серверным значением (OFF)');
        assert.strictEqual(snap.firstBValue, 'null', 'model-b нарисована не серверным значением (inherit)');
    });

    check('R680/2: выбор оператора переживает пересборку списка моделей AutoTune', function () {
        assert.notStrictEqual(snap.secondB, snap.firstB,
            'узел селектора не заменён: пересборки не было, проверка ничего не доказывает');
        assert.strictEqual(snap.secondB.value, 'true',
            'пересборка списка вернула серверное значение — выбор оператора (ON) потерян: ' +
            'ровно живой дефект R680');
        assert.strictEqual(snap.secondA.value, 'false',
            'перенос выбора «залил» и нетронутую модель: серверное значение OFF потеряно');
    });

    check('R680/2: «Сохранить» отправляет ВЫБОР ОПЕРАТОРА, а не серверное значение', function () {
        assert.strictEqual(snap.puts.length, 1, 'PUT конфига AutoTune не ушёл после «Сохранить»');
        assert.strictEqual(snap.sent.perModel['model-b'], true,
            'на сервер ушло не то, что выбрал оператор: ' + JSON.stringify(snap.sent.perModel));
        assert.strictEqual(snap.sent.perModel['model-a'], false,
            'на сервер ушёл неверный override соседней модели: ' + JSON.stringify(snap.sent.perModel));
        assert.strictEqual(env.state.perModel['model-b'], true,
            'серверное состояние не совпало с отправленным');
    });

    check('R680/2: после сохранения выбор на месте, поле не «заморожено» навсегда', function () {
        assert.strictEqual(snap.thirdB.value, 'true',
            'после сохранения селектор потерял выбранное значение');
        assert.strictEqual(snap.thirdBase, 'true',
            'базой нового узла осталось старое серверное значение — поле считается ' +
            'изменённым оператором навсегда');
    });

    // --- сценарий 2: заслон R513 не ослаблен --------------------------------
    const env2 = buildEnvironment({
        globalEnabled: true,
        perModel: {},
        models: ['model-a']
    });
    await global.AutoTuneSettings.init();
    const before = env2.selectFor('model-a');
    before.value = 'false';
    env2.focus.inside = true;         // оператор ВНУТРИ списка — пересборки быть не должно
    await env2.refreshByTick();
    const after = env2.selectFor('model-a');

    check('R680/2: пока фокус внутри списка, узел не подменяется (заслон R513 цел)', function () {
        assert.strictEqual(after, before, 'список пересобран под фокусом — заслон R513 ослаблен');
        assert.strictEqual(after.value, 'false', 'выбор потерян при включённом заслоне');
    });

    // --- статические проверки ---------------------------------------------
    const code = codeOf(autotuneSrc);
    check('R680/2: список не собирается через innerHTML', function () {
        assert.ok(!/innerHTML/.test(code),
            'в autotune_settings.js снова есть innerHTML: пересборка `<select>`-ов через ' +
            'разметку уничтожает выбор оператора (корень R680)');
    });
    check('R680/2: пересборка — DocumentFragment + collect/restore через SafeForm', function () {
        assert.ok(/document\.createDocumentFragment\(\)/.test(code),
            'список снова собирается без DocumentFragment');
        assert.ok(/(?:SafeForm|SF)\.collectInputs\(/.test(code),
            'перед пересборкой не снимается выбор оператора (SafeForm.collectInputs)');
        assert.ok(/(?:SafeForm|SF)\.restoreInputs\(/.test(code),
            'после пересборки не возвращается выбор оператора (SafeForm.restoreInputs)');
        assert.ok(/data-model/.test(code),
            'у селектора нет ключа data-model — переносить выбор не по чему');
    });

    if (failures.length) {
        console.error('\n' + failures.length + ' проверок провалено, ' + passed + ' пройдено');
        process.exit(1);
    }
    console.log('\nвсе ' + passed + ' проверок пройдены');
};

main().catch(function (e) {
    console.error('  \u2717 сценарий не выполнился: ' + (e && e.stack || e));
    process.exit(1);
});

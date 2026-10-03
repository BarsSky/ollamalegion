// client-access.test.js — блок «Подключение клиентов» рядом с бэкендом.
//
// Run: node webui/js/modules/client-access.test.js
//
// ЗАЧЕМ (запрос оператора): «под OpenAPI обязательно требуется ключ — можно его
// как-то выводить рядом с бэкендом, что cppworker, что imageworker?». Клиенты
// требуют непустое поле «API key», а оператор не знал, какое значение вписывать.
// Проверяем:
//   1. ключ берётся из WEBUI_CONFIG.API_TOKEN (тот же, что уходит в X-API-Token)
//      и по умолчанию МАСКИРОВАН (не светится в скриншотах);
//   2. эндпоинты строятся по типу бэкенда: image_cpp → /v1/images/generations и
//      /sdapi/v1/txt2img на порту OpenAI-поверхности; llama_cpp/ollama → текст на
//      порту балансера;
//   3. curl-пример содержит и ключ, и правильный путь для этого типа;
//   4. кнопки «показать/копировать/копировать curl» действительно переключают
//      маску и кладут нужный текст в буфер;
//   5. эндпоинты воркера НЕ выдаются за клиентские (порт воркера в клиентских
//      URL не попадает — раньше оператор шёл на 18093/18092 и не находил клиента).

'use strict';

const assert = require('assert');

// --- mocks: window / Utils / I18N / clipboard -------------------------------

const copied = [];
global.window = global;
window.location = { hostname: 'legion.host', origin: 'http://legion.host:18083' };
window.WEBUI_CONFIG = { API_TOKEN: 'test-token-abc123', API_BASE: '' };
window.Utils = {
    escapeHtml: function (s) {
        return s === null || s === undefined ? '' : String(s)
            .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
            .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
    },
};
const RU = {
    'clientAccess.title': 'Подключение клиентов',
    'clientAccess.key_label': 'Ключ (X-API-Token / Bearer)',
    'clientAccess.show': 'Показать',
    'clientAccess.hide': 'Скрыть',
    'clientAccess.copy': 'Копировать',
    'clientAccess.copied': 'Ключ скопирован',
    'clientAccess.copy_curl': 'Копировать curl',
    'clientAccess.copied_curl': 'curl скопирован',
    'clientAccess.key_note_optional': 'На /v1/images/* ключ не проверяется',
};
window.I18N = { t: function (k) { return Object.prototype.hasOwnProperty.call(RU, k) ? RU[k] : k; } };
// В Node 22 `global.navigator` — getter-only свойство, поэтому подменяем именно
// clipboard (через defineProperty), а не сам navigator.
const clipboardStub = { writeText: function (t) { copied.push(t); return Promise.resolve(); } };
try {
    Object.defineProperty(global.navigator, 'clipboard', { value: clipboardStub, configurable: true, writable: true });
} catch (e) {
    Object.defineProperty(global, 'navigator', { value: { clipboard: clipboardStub }, configurable: true, writable: true });
}

// Минимальный DOM для mount(): делегирование кликов + close/querySelector.
const listeners = {};
global.document = {
    addEventListener: function (type, fn) { (listeners[type] = listeners[type] || []).push(fn); },
    createElement: function () { return { style: {}, setAttribute: function () {}, select: function () {}, value: '' }; },
    body: { appendChild: function () {}, removeChild: function () {} },
    execCommand: function () { return true; },
};
const toasts = [];
window.showToast = function (msg, kind) { toasts.push({ msg: msg, kind: kind }); };

require('./client-access.js');
const CA = window.ClientAccess;
assert.ok(CA, 'window.ClientAccess должен быть экспортирован (см. check_iife_exports.py)');
const P = CA.pure;

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
const sleep = function (ms) { return new Promise(function (r) { setTimeout(r, ms); }); };

const IMAGE_BACKEND = { id: 'imageworker', host: 'legion.host', type: 'image_cpp', imagePort: 18093 };
const CPP_BACKEND = { id: 'cppworker-gpu-bundled-agent', host: 'legion.host', type: 'llama_cpp', cppWorkerPort: 18092 };
const OLLAMA_BACKEND = { id: 'ollama-1', host: 'legion.host', ollamaPort: 11434 };

(async function run() {
    // --- 1. ключ -------------------------------------------------------------
    check('ключ берётся из WEBUI_CONFIG.API_TOKEN', function () {
        assert.strictEqual(P.clientKey(), 'test-token-abc123');
    });

    // --- 2. тип и эндпоинты --------------------------------------------------
    check('тип бэкенда определяется по type/backendType/image_cpp', function () {
        assert.strictEqual(P.backendKind(IMAGE_BACKEND), 'image_cpp');
        assert.strictEqual(P.backendKind(CPP_BACKEND), 'llama_cpp');
        assert.strictEqual(P.backendKind(OLLAMA_BACKEND), 'ollama');
    });
    check('хост клиентских URL — адрес, по которому открыт WebUI, а не имя docker-сервиса', function () {
        // В docker-стенде backend.host = имя сервиса (`imageworker`), вне
        // compose-сети оно не резолвится: клиент должен ходить на тот же хост,
        // что и браузер.
        const dockerBackend = { id: 'imageworker', host: 'imageworker', type: 'image_cpp', imagePort: 18093 };
        assert.strictEqual(P.backendHost(dockerBackend), 'legion.host', 'взят host бэкенда вместо hostname браузера');
        const urls = P.endpoints(dockerBackend).rows.map(function (r) { return r.url; }).join(' ');
        assert.ok(urls.indexOf('http://legion.host:18079/v1/images/generations') !== -1, urls);
        assert.strictEqual(urls.indexOf('http://imageworker:'), -1, 'в клиентские URL попал docker-хост: ' + urls);
        // Внутренний адрес при этом доступен отдельно — с пометкой в UI.
        assert.strictEqual(P.internalAddress(dockerBackend), 'imageworker:18093');
        const html = CA.render(dockerBackend);
        assert.ok(html.indexOf('Внутренний адрес воркера') !== -1, 'нет строки внутреннего адреса');
        assert.ok(html.indexOf('imageworker:18093') !== -1, 'внутренний адрес не показан');
    });
    check('PUBLIC_HOST из конфига имеет приоритет над hostname браузера', function () {
        const saved = window.WEBUI_CONFIG.PUBLIC_HOST;
        window.WEBUI_CONFIG.PUBLIC_HOST = 'legion.example.com';
        try {
            assert.strictEqual(P.backendHost(CPP_BACKEND), 'legion.example.com');
        } finally {
            if (saved === undefined) { delete window.WEBUI_CONFIG.PUBLIC_HOST; } else { window.WEBUI_CONFIG.PUBLIC_HOST = saved; }
        }
    });
    check('image_cpp: клиентские пути картинок на порту OpenAI-поверхности, НЕ порт воркера', function () {
        const ep = P.endpoints(IMAGE_BACKEND);
        const urls = ep.rows.map(function (r) { return r.url; }).join(' ');
        assert.ok(urls.indexOf('http://legion.host:18079/v1/images/generations') !== -1, 'нет OpenAI-пути: ' + urls);
        assert.ok(urls.indexOf('http://legion.host:18079/sdapi/v1/txt2img') !== -1, 'нет A1111-пути: ' + urls);
        assert.ok(urls.indexOf(':18093') === -1, 'порт воркера попал в клиентские URL: ' + urls);
    });
    check('llama_cpp: текстовые пути на порту балансера', function () {
        const urls = P.endpoints(CPP_BACKEND).rows.map(function (r) { return r.url; }).join(' ');
        assert.ok(urls.indexOf('http://legion.host:18080/v1/chat/completions') !== -1, 'нет чата: ' + urls);
        assert.ok(urls.indexOf('http://legion.host:18080/v1/models') !== -1, 'нет списка моделей: ' + urls);
        assert.ok(urls.indexOf(':18092') === -1, 'порт cppworker попал в клиентские URL: ' + urls);
    });
    check('ollama: нативные пути Ollama API + OpenAI-чат через балансер', function () {
        const urls = P.endpoints(OLLAMA_BACKEND).rows.map(function (r) { return r.url; }).join(' ');
        assert.ok(urls.indexOf('http://legion.host:11434/api/chat') !== -1, 'нет ollama chat: ' + urls);
        assert.ok(urls.indexOf('/v1/chat/completions') !== -1, 'нет OpenAI-чата: ' + urls);
    });

    // --- 3. curl -------------------------------------------------------------
    check('curl для картинок: путь, ключ и JSON генерации', function () {
        const c = P.curlExample(IMAGE_BACKEND);
        assert.ok(c.indexOf('http://legion.host:18079/v1/images/generations') !== -1, c);
        assert.ok(c.indexOf('Bearer test-token-abc123') !== -1, 'нет ключа в curl: ' + c);
        assert.ok(c.indexOf('"size":"512x512"') !== -1, c);
    });
    check('curl для текста: chat/completions и ключ', function () {
        const c = P.curlExample(CPP_BACKEND);
        assert.ok(c.indexOf('http://legion.host:18080/v1/chat/completions') !== -1, c);
        assert.ok(c.indexOf('Bearer test-token-abc123') !== -1, c);
    });

    // --- 4. рендер и кнопки --------------------------------------------------
    const html = CA.render(IMAGE_BACKEND);
    check('рендер: ключ МАСКИРОВАН (значение только в data-key), есть кнопки', function () {
        assert.ok(html.indexOf('data-client-access-block="1"') !== -1, 'нет разметки блока');
        assert.ok(html.indexOf(P.KEY_MASK) !== -1, 'нет маски ключа');
        assert.strictEqual(html.indexOf('>test-token-abc123<'), -1, 'ключ отрисован открытым текстом');
        assert.ok(html.indexOf('data-key="test-token-abc123"') !== -1, 'ключ не сохранён для кнопки «показать»');
        assert.ok(html.indexOf('data-client-access="toggle-key"') !== -1, 'нет кнопки «показать»');
        assert.ok(html.indexOf('data-client-access="copy-key"') !== -1, 'нет кнопки «копировать»');
        assert.ok(html.indexOf('data-client-access="copy-curl"') !== -1, 'нет кнопки «копировать curl»');
        assert.ok(html.indexOf('Подключение клиентов') !== -1, 'нет заголовка блока');
    });
    check('рендер: пометка, где ключ проверяется, а где нет', function () {
        assert.ok(html.indexOf('ключ не проверяется') !== -1, 'нет пометки про клиентские поверхности');
    });

    // Делегированный обработчик: собираем минимальные DOM-узлы блока.
    CA.mount();
    const clickHandlers = listeners.click || [];
    check('mount: делегированный обработчик кликов зарегистрирован один раз', function () {
        CA.mount();
        assert.strictEqual((listeners.click || []).length, clickHandlers.length, 'обработчик зарегистрирован повторно');
        assert.ok(clickHandlers.length === 1, 'ожидался один обработчик на документ');
    });

    function makeBlock() {
        const keyEl = {
            _text: P.KEY_MASK,
            _attrs: { 'data-key': 'test-token-abc123', 'data-masked': '1' },
            getAttribute: function (n) { return this._attrs[n]; },
            setAttribute: function (n, v) { this._attrs[n] = v; },
            get textContent() { return this._text; },
            set textContent(v) { this._text = v; },
        };
        const curlEl = { textContent: 'curl -sS -X POST http://x/v1/images/generations' };
        const buttons = {};
        const block = {
            querySelector: function (sel) {
                if (sel === '[data-client-key-value]') return keyEl;
                if (sel === '[data-client-curl]') return curlEl;
                return null;
            },
        };
        function mkBtn(action) {
            const b = {
                _attrs: { 'data-client-access': action },
                getAttribute: function (n) { return this._attrs[n]; },
                parentNode: block,
                closest: function () { return block; },
                textContent: '',
            };
            buttons[action] = b;
            return b;
        }
        return { keyEl: keyEl, curlEl: curlEl, buttons: buttons, make: mkBtn };
    }

    const dom = makeBlock();
    dom.make('toggle-key');
    check('кнопка «показать»: снимает маску и меняет подпись, повторный клик — возвращает', function () {
        clickHandlers[0]({ target: dom.buttons['toggle-key'] });
        assert.strictEqual(dom.keyEl.textContent, 'test-token-abc123', 'ключ не показан');
        assert.strictEqual(dom.keyEl.getAttribute('data-masked'), '0');
        assert.strictEqual(dom.buttons['toggle-key'].textContent, 'Скрыть', 'подпись кнопки не сменилась');
        clickHandlers[0]({ target: dom.buttons['toggle-key'] });
        assert.strictEqual(dom.keyEl.textContent, P.KEY_MASK, 'ключ не замаскирован обратно');
    });

    copied.length = 0;
    dom.make('copy-key');
    clickHandlers[0]({ target: dom.buttons['copy-key'] });
    await sleep(10);
    check('кнопка «копировать»: в буфере ключ и тост об успехе', function () {
        assert.deepStrictEqual(copied, ['test-token-abc123'], 'в буфер ушло не то: ' + JSON.stringify(copied));
        assert.ok(toasts.some(function (t) { return t.msg === 'Ключ скопирован'; }), 'нет тоста: ' + JSON.stringify(toasts));
    });

    copied.length = 0;
    dom.make('copy-curl');
    clickHandlers[0]({ target: dom.buttons['copy-curl'] });
    await sleep(10);
    check('кнопка «копировать curl»: в буфере curl-пример', function () {
        assert.strictEqual(copied.length, 1);
        assert.ok(copied[0].indexOf('/v1/images/generations') !== -1, 'скопирован не тот curl: ' + copied[0]);
    });

    check('без ключа в конфиге блок честно об этом пишет', function () {
        const saved = window.WEBUI_CONFIG.API_TOKEN;
        window.WEBUI_CONFIG.API_TOKEN = '';
        try {
            const h = CA.render(CPP_BACKEND);
            assert.ok(h.indexOf('data-client-key-value') === -1, 'рисуется кнопка показа при отсутствии ключа');
            assert.ok(h.indexOf('Ключ не задан') !== -1, 'нет пояснения про отсутствующий ключ');
        } finally {
            window.WEBUI_CONFIG.API_TOKEN = saved;
        }
    });

    console.log('');
    if (failures.length) {
        console.log('ИТОГ: есть провалы (' + failures.length + ')');
        process.exit(1);
    }
    console.log('ИТОГ: все проверки пройдены (' + passed + ')');
})();

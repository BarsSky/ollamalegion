// js/modules/safe-form.js — R676 (2026-11-21): единственная безопасная точка
// записи значения в поле формы WebUI.
//
// ЖИВАЯ ЖАЛОБА (третья редакция одной и той же). «Постоянное обновление стирает
// то, что я ввёл в формы». R512 защитил ФОКУС (пока курсор в поле — тик не
// работает), R662 — «недавнюю правку» страницы (TTL 2 минуты), R666/R670 —
// перерисовку по таймеру и первый рендер. Все три закрывали ПЕРЕРИСОВКУ
// страницы, а значения заливались в поля прямыми `el.value = …` — 50 таких мест
// в 6 модулях. Поведенческое доказательство на стенде: набрал 9999, кликнул мимо
// (фокус ушёл), один проход applyServerConfig → в поле снова 2048, причём
// FormDirty.isDirtyPage('settings') === true. То есть защита была, а запись шла
// мимо неё.
//
// ЧТО ДЕЛАЕТ МОДУЛЬ. Каждая ПРОГРАММНАЯ запись в поле идёт через SafeForm.
// Он помнит в WeakMap «базовое» значение — то, которое последним записал САМ, —
// и пропускает запись только когда выполнены ВСЕ три условия:
//
//   1) элемент не document.activeElement — оператор не печатает в него сейчас;
//   2) текущее значение элемента равно базовому — оператор поле не менял;
//   3) страница элемента не помечена FormDirty — на странице есть свежая
//      несохранённая правка (см. js/modules/form-dirty.js).
//
// Если хоть одно условие нарушено, поле НЕ трогаем, но рендер продолжается:
// таблицы, счётчики, метрики и бейджи обновляются как обычно. В этом и смысл —
// раньше приходилось глушить перерисовку страницы ЦЕЛИКОМ (и данные «вставали»
// на две минуты). Набранное значение остаётся на экране. Если оператор вернул
// поле к базовому (или сохранил форму, сняв FormDirty), SafeForm снова им
// управляет — «заморозки навсегда» нет.
//
// ПОЧЕМУ WeakMap, А НЕ data-АТРИБУТ. Базовое значение — состояние, а не разметка.
// Атрибут пережил бы пересборку `innerHTML` (и переехал бы в НОВЫЙ узел, у
// которого база другая), попал бы в проверки разметки и в снимки экрана. WeakMap
// привязана к самому узлу: узел выбросили — запись ушла вместе с ним, память не
// течёт.
//
// Тесты: webui/tests/page-contract-r676.test.js (контракт фабрик и порядок
// подключения), webui/tests/form-dirty-guard.test.js и
// webui/tests/settings-form-guard-r513.test.js (поведение на реальных модулях).
(function () {
    'use strict';

    // Базовое значение по узлу: el -> то, что последним записал SafeForm.
    var bases = new WeakMap();

    // Счётчики для диагностики (живая проверка через CDP и тесты): сколько
    // записей прошло, сколько отклонено. Без них «защита работает» и «защита
    // выключена» выглядят одинаково — поле просто не меняется.
    var stats = { written: 0, blocked: 0 };

    // --- разбор значения ----------------------------------------------------

    function typeOf(el) {
        return String((el && el.type) || '').toLowerCase();
    }

    function isCheckable(el) {
        var t = typeOf(el);
        return t === 'checkbox' || t === 'radio';
    }

    /** readValue — значение поля в том виде, в каком его видит оператор. */
    function readValue(el) {
        if (!el) { return ''; }
        if (isCheckable(el)) { return el.checked === true; }
        return (el.value === undefined || el.value === null) ? '' : String(el.value);
    }

    function sameValue(a, b) {
        if (typeof a === 'boolean' || typeof b === 'boolean') {
            return (a === true) === (b === true);
        }
        return String(a) === String(b);
    }

    /** elOf — элемент по узлу или по id (в модулях есть и то, и другое). */
    function elOf(target) {
        if (!target) { return null; }
        if (typeof target === 'object') { return target; }
        if (typeof document === 'undefined' || !document || typeof document.getElementById !== 'function') {
            return null;
        }
        return document.getElementById(String(target));
    }

    // --- страница узла (тот же контракт, что у form-dirty.js) ----------------

    /**
     * pageIdOf — id страницы-контейнера для узла (`settings-page` и т.п.).
     *
     * Ключ совпадает с ключом FormDirty и с контейнером, который ищет тик
     * авто-обновления (`<page>-page`), поэтому одно и то же имя не может
     * означать разные страницы.
     */
    function pageIdOf(node) {
        var el = node;
        var guard = 0;
        while (el && guard < 100) {
            guard++;
            if (el.id && /-page$/.test(String(el.id))) { return String(el.id); }
            el = el.parentNode || null;
        }
        return '';
    }

    /** pageIsDirty — есть ли на странице узла свежая несохранённая правка. */
    function pageIsDirty(el) {
        var g = (typeof window !== 'undefined') ? window : null;
        var FD = g && g.FormDirty;
        if (!FD || typeof FD.isDirtyPage !== 'function') { return false; }
        var id = pageIdOf(el);
        if (!id) { return false; }
        return FD.isDirtyPage(id) === true;
    }

    function isActive(el) {
        return typeof document !== 'undefined' && !!document && document.activeElement === el;
    }

    // --- правила ------------------------------------------------------------

    /**
     * blockedReason — почему запись в поле сейчас запрещена ('' — разрешена).
     *
     * Возвращает имена правил ('active' | 'edited' | 'dirty'), а не boolean:
     * в живой проверке и в отчётах нужно видеть ПРИЧИНУ, иначе «поле не
     * обновилось» снова неотличимо от «обновлять было нечего».
     *
     * Базовое значение отсутствует (поле ещё ни разу не писал SafeForm) —
     * правило (2) не применяется: сравнивать не с чем, а правку оператора в этом
     * случае ловит правило (3) и, дальше, сама запись установит базу.
     */
    function blockedReason(target, opts) {
        var el = elOf(target);
        if (!el) { return 'missing'; }
        if (opts && opts.force) { return ''; }
        if (isActive(el)) { return 'active'; }
        if (bases.has(el) && !sameValue(readValue(el), bases.get(el))) { return 'edited'; }
        if (pageIsDirty(el)) { return 'dirty'; }
        return '';
    }

    // --- запись -------------------------------------------------------------

    /**
     * setValue — записать значение в поле ввода, не стирая ввод оператора.
     *
     * opts.force — запись, которая ЯВЛЯЕТСЯ ответом на действие оператора
     * (кнопка «пресет», автоподстановка порта, импорт конфига): правила
     * пропускаются, но база всё равно записывается, иначе следующая же
     * перерисовка сочтёт значение «правкой оператора».
     *
     * @returns {boolean} true — запись выполнена (или значение уже было таким).
     */
    function setValue(target, value, opts) {
        var el = elOf(target);
        if (!el) { return false; }
        if (blockedReason(el, opts)) {
            stats.blocked++;
            return false;
        }
        var next = (value === undefined || value === null) ? '' : String(value);
        if (String(readValue(el)) !== next) { el.value = next; }
        // База — то, что РЕАЛЬНО оказалось в поле: браузер нормализует значение
        // («1.0» → «1» у input[type=number]), и база «1.0» навсегда объявила бы
        // поле «изменённым оператором» (то есть заморозила бы его).
        bases.set(el, readValue(el));
        stats.written++;
        return true;
    }

    /** setChecked — то же для checkbox/radio (значение приводится к boolean). */
    function setChecked(target, value, opts) {
        var el = elOf(target);
        if (!el) { return false; }
        if (blockedReason(el, opts)) {
            stats.blocked++;
            return false;
        }
        var next = !!value;
        if (el.checked !== next) { el.checked = next; }
        bases.set(el, el.checked === true);
        stats.written++;
        return true;
    }

    /**
     * markBase — объявить текущее значение поля базовым (или задать базу явно).
     *
     * Нужен там, где узел нарисован НЕ через SafeForm (разметка из innerHTML /
     * DocumentFragment, значение пришло атрибутом `value="…"`). Без этого
     * первая же неудачная запись навсегда осталась бы «правкой оператора».
     */
    function markBase(target, value) {
        var el = elOf(target);
        if (!el) { return false; }
        if (arguments.length > 1) {
            bases.set(el, isCheckable(el) ? (value === true) : String(value));
        } else {
            bases.set(el, readValue(el));
        }
        return true;
    }

    /** baseOf — базовое значение поля (undefined, если SafeForm им не владел). */
    function baseOf(target) {
        var el = elOf(target);
        if (!el) { return undefined; }
        return bases.get(el);
    }

    /**
     * keyOf — ключ узла для переноса значений при пересборке.
     *
     * keySpec — имя data-атрибута или функция (нужна таблице лимитов, где ключ
     * составной: бэкенд + тип лимита).
     */
    function keyOf(node, keySpec) {
        if (!node) { return ''; }
        if (typeof keySpec === 'function') {
            try { return String(keySpec(node) || ''); } catch (e) { return ''; }
        }
        var attr = keySpec || 'data-hot-input';
        if (typeof node.getAttribute !== 'function') { return ''; }
        return node.getAttribute(attr) || '';
    }

    /** selectorOf — селектор полей: явный или по keySpec-атрибуту. */
    function selectorOf(keySpec, selector) {
        if (selector) { return selector; }
        if (typeof keySpec === 'function') { return '[data-form-key]'; }
        return '[' + (keySpec || 'data-hot-input') + ']';
    }

    /**
     * collectInputs — снять значения, которые оператор УЖЕ изменил, до пересборки.
     *
     * ЗАЧЕМ ДЛЯ ПЕРЕСБОРОК. Панель горячих настроек и таблица лимитов рисуются
     * через `innerHTML`: узлы уничтожаются вместе с набранными числами. Порядок
     * безопасной пересборки — снять (collect) → нарисовать заново → вернуть
     * (restore). Берутся ТОЛЬКО поля, чьё значение разошлось с базовым: остальные
     * и так нарисуются серверными значениями.
     *
     * @returns {Object} key -> значение оператора ('' / {} — переносить нечего).
     */
    function collectInputs(root, keySpec, selector) {
        var out = Object.create(null);
        if (!root || typeof root.querySelectorAll !== 'function') { return out; }
        var nodes = root.querySelectorAll(selectorOf(keySpec, selector));
        for (var i = 0; i < nodes.length; i++) {
            var node = nodes[i];
            var key = keyOf(node, keySpec);
            if (!key) { continue; }
            var base = bases.get(node);
            var current = readValue(node);
            // Оператор не менял поле (или SafeForm им не владел) — переносить нечего.
            if (base === undefined || sameValue(current, base)) { continue; }
            out[key] = current;
        }
        return out;
    }

    /**
     * restoreInputs — вернуть значения оператора в свежеотрисованные поля.
     *
     * Делает ДВЕ вещи, и обе нужны:
     *   1) КАЖДОМУ полю поддерева объявляет базой то, что в нём нарисовано
     *      (серверное значение). Без этого у полей, которые SafeForm никогда не
     *      писал (таблица лимитов, панель реестра рисуются через innerHTML),
     *      базы не было бы вовсе, и первая же правка оператора не попала бы в
     *      следующий collectInputs — то есть терялась бы при пересборке.
     *   2) Тем, чьё значение оператор менял, возвращает ЕГО значение. База
     *      остаётся серверной: следующий проход снова увидит расхождение и не
     *      перетрёт набранное, а вернул поле к серверному — расхождения нет,
     *      SafeForm снова управляет.
     *
     * @returns {number} сколько полей сохранило значение оператора.
     */
    function restoreInputs(root, values, keySpec, selector) {
        var moved = 0;
        if (!root || typeof root.querySelectorAll !== 'function') { return moved; }
        var nodes = root.querySelectorAll(selectorOf(keySpec, selector));
        for (var i = 0; i < nodes.length; i++) {
            var node = nodes[i];
            var serverValue = readValue(node);
            var key = keyOf(node, keySpec);
            if (key && values && Object.prototype.hasOwnProperty.call(values, key)) {
                if (isCheckable(node)) { node.checked = values[key] === true; } else { node.value = String(values[key]); }
                moved++;
            }
            bases.set(node, serverValue);
        }
        return moved;
    }

    /** stats() — снимок счётчиков (диагностика; в боевом коде не читается). */
    function statsSnapshot() {
        return { written: stats.written, blocked: stats.blocked };
    }

    /** resetStats — обнулить счётчики (живая проверка и тесты). */
    function resetStats() {
        stats.written = 0;
        stats.blocked = 0;
    }

    var api = {
        setValue: setValue,
        setChecked: setChecked,
        markBase: markBase,
        baseOf: baseOf,
        collectInputs: collectInputs,
        restoreInputs: restoreInputs,
        blockedReason: blockedReason,
        stats: statsSnapshot,
        resetStats: resetStats
    };
    if (typeof window !== 'undefined') {
        window.SafeForm = api;
    }
    if (typeof module !== 'undefined' && module.exports) {
        module.exports = api; // node-тесты (webui/tests) без DOM
    }
})();

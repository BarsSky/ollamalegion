// js/modules/gguf-detail-form-state.js — R688 (2026-11-22): пересборка правой
// панели GGUF не должна стирать незавершённый ввод оператора.
//
// ЖИВАЯ ЖАЛОБА (дословно): «Форма у модели (в которой задаёшь параметры для
// загрузки) всё ещё скрывается и происходит постоянное обновление страницы gguf
// models, отчего нет возможности самому задать параметры через веб-интерфейс».
//
// ПРИЧИНА. Панель деталей рисуется через `innerHTML` в двух местах
// (js/modules/gguf-renderer-refresh.js → refreshDetailPanel / refreshDetailPane), и
// эти две функции вызываются ВСЕМИ автоматическими проходами:
//   * провайдер страницы (тик авто-обновления, REFRESH_INTERVAL = 5 с);
//   * refreshDetail → refreshDetailPanel (5 с) и fetchDiskModelsAsync →
//     refreshDetailPanel (ещё раз, по ответу /api/models/files);
//   * опрос активных запросов (3 с) и опрос загрузок (2 с) → refreshDetailPane.
// Каждая такая пересборка делает две вещи, обе ломают заполнение формы:
//   1) пересоздаёт форму из кэшированного HTML (state._settingsFormHtmlByBackend)
//      с СЕРВЕРНЫМИ значениями — набранные оператором числа исчезают;
//   2) пересоздаёт секции-аккордеоны `<details class="gguf-acc">` — раскрытые
//      оператором группы снова свёрнуты (первая открыта, остальные закрыты).
// Внешне это и есть «форма скрывается и страница постоянно обновляется»: замер
// на стенде (CDP, образ r684) — 12 пересборок правой панели за 20 с, раскрытая
// секция схлопнулась, набранное 9999 заменено серверным 65536.
//
// ЧТО ДЕЛАЕТ МОДУЛЬ. Одна обёртка вокруг пересборки DOM, три правила:
//
//   1) ОТКЛАДЫВАТЬ, ЕСЛИ ОПЕРАТОР В ФОРМЕ. Пока фокус в поле панели
//      (RefreshGuard.isEditingIn) или хотя бы одно поле панели разошлось с
//      базовым значением (SafeForm: что программа записала последней), авто-
//      проход НЕ пересобирает панель. Данные при этом продолжают обновляться:
//      заслон стоит на DOM-записи, а не на запросах — список бэкендов, статусы и
//      метрики обновляют свои функции (refreshBackends и пр.) независимо от
//      панели. Ровно то же разделение сделано для «Настроек» в R676
//      (SAFE_FORM_PAGES): защита поля/панели, а не глушение страницы.
//
//   2) ПЕРЕНОСИТЬ ЗНАЧЕНИЯ, ЕСЛИ ПЕРЕСБОРКА ВСЁ РАВНО ПРОИСХОДИТ. Смена бэкенда,
//      клик по табу, ответ WS — законные поводы перерисовать панель. Тогда
//      значения полей переносятся: SafeForm.collectInputs ДО подмены innerHTML и
//      restoreInputs ПОСЛЕ (тот же приём, что swapPanel в settings-registry.js и
//      replaceRowsPreserving в app-models.js). Так введённое переживает ОДИН
//      цикл пересборки, даже если она вызвана другой причиной.
//
//   3) ПОМНИТЬ РАСКРЫТЫЕ СЕКЦИИ. Состояние `<details>` запоминается по стабильному
//      ключу (data-acc — секции формы параметров загрузки; data-model-acc —
//      карточки параметров модели) и восстанавливается после пересборки. Ключ не
//      включает бэкенд: структура секций у всех бэкендов одна, и оператор,
//      раскрывший «KV cache» на одном, ожидает её же раскрытой на другом.
//
// ПОЧЕМУ НЕ «ПРОСТО НЕ ОБНОВЛЯТЬ СТРАНИЦУ». Этот путь уже пройден в R662/R666:
// заслон по FormDirty на весь refreshPage останавливал ВСЕ данные страницы на две
// минуты, и жалоба «обновление не показывает новое» была второй половиной той же
// болезни. Замер на стенде до правки: пока страница «грязная», за 20 с НИ ОДНОГО
// запроса страницы (список моделей и статусы замерли), а после истечения TTL —
// 12 пересборок панели и потерянное значение. Здесь обе половины разделены:
// данные идут всегда, DOM панели подменяется только когда это безопасно.
//
// ЧЕГО МОДУЛЬ НЕ ДЕЛАЕТ. Не пишет значения в поля сервера (это забота SafeForm) и
// не решает, когда перерисовывать — решение остаётся за вызывающим: авто-проходы
// передают `{auto: true}`, явные действия оператора (таб, кнопка «Обновить»,
// выбор бэкенда) пересобирают панель всегда, но тоже с переносом значений.
//
// Тесты: webui/tests/gguf-form-guard-r688.test.js (RED на коде до R688).
(function () {
    'use strict';

    // Страница GGUF: id контейнера в index.html. Ключ FormDirty совпадает с ним.
    var PAGE_FALLBACK_ID = 'gguf-page';

    // Поля, значения которых переносятся через пересборку. id — самый устойчивый
    // ключ: id полей формы параметров загрузки и редактора модели уникальны на
    // странице (у редактора модели id = ggufMp_<модель>_<параметр>).
    var FIELD_SELECTOR = 'input[id], select[id], textarea[id]';

    // Состояние раскрытых секций: key -> boolean. Живёт в памяти вкладки: это
    // состояние ПРЕДСТАВЛЕНИЯ, а не настройка, и переживать F5 ему не нужно
    // (после перезагрузки форма один раз откроет первую секцию, как задумано).
    var openState = Object.create(null);

    function win() {
        return (typeof window !== 'undefined') ? window : null;
    }

    function safeForm() {
        var w = win();
        return (w && w.SafeForm) || null;
    }

    function formDirty() {
        var w = win();
        return (w && w.FormDirty) || null;
    }

    function refreshGuard() {
        var w = win();
        return (w && w.RefreshGuard) || null;
    }

    function ggufState() {
        var w = win();
        var M = w && w.GgufModule;
        return (M && M.state && typeof M.state === 'object') ? M.state : {};
    }

    /**
     * pageIdOf — id страницы-контейнера для узла (`gguf-page`).
     *
     * Контракт тот же, что у FormDirty/SafeForm: ближайший элемент с id на
     * `-page`. Свой обход, а не FormDirty.pageIdOf, — чтобы модуль работал и там,
     * где FormDirty ещё не загружен (порядок defer-скриптов).
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

    function pageIsDirty(root) {
        var FD = formDirty();
        if (!FD || typeof FD.isDirtyPage !== 'function') { return false; }
        var id = pageIdOf(root) || PAGE_FALLBACK_ID;
        return FD.isDirtyPage(id) === true;
    }

    /** isEditingIn — оператор печатает прямо сейчас внутри панели. */
    function isEditingIn(root) {
        var RG = refreshGuard();
        if (!RG || typeof RG.isEditingIn !== 'function' || !root) { return false; }
        return RG.isEditingIn(root) === true;
    }

    function fieldKey(node) {
        if (!node) { return ''; }
        if (node.id) { return String(node.id); }
        if (typeof node.getAttribute !== 'function') { return ''; }
        return String(node.getAttribute('id') || node.getAttribute('name') || '');
    }

    /**
     * collect — снять значения, которые оператор уже изменил относительно базы.
     *
     * null означает «SafeForm недоступен»: тогда переносить значения нечем, и
     * защита опирается только на FormDirty страницы (см. protectsPanel).
     */
    function collect(root) {
        var SF = safeForm();
        if (!SF || typeof SF.collectInputs !== 'function') { return null; }
        if (!root || typeof root.querySelectorAll !== 'function') { return null; }
        return SF.collectInputs(root, fieldKey, FIELD_SELECTOR);
    }

    /** restore — вернуть значения оператора в свежеотрисованные поля. */
    function restore(root, values) {
        var SF = safeForm();
        if (!SF || typeof SF.restoreInputs !== 'function') { return 0; }
        if (!root || typeof root.querySelectorAll !== 'function') { return 0; }
        return SF.restoreInputs(root, values || Object.create(null), fieldKey, FIELD_SELECTOR);
    }

    /** markBases — объявить текущие значения полей базовыми (после перерисовки). */
    function markBases(root) {
        var SF = safeForm();
        if (!SF || typeof SF.markBase !== 'function') { return 0; }
        if (!root || typeof root.querySelectorAll !== 'function') { return 0; }
        var nodes = root.querySelectorAll(FIELD_SELECTOR);
        var marked = 0;
        for (var i = 0; i < nodes.length; i++) {
            if (SF.markBase(nodes[i])) { marked++; }
        }
        return marked;
    }

    /** hasUnsavedEdits — в панели есть поля, изменённые оператором без сохранения. */
    function hasUnsavedEdits(root) {
        var pending = collect(root);
        if (pending === null) { return pageIsDirty(root); }
        var keys = Object.keys(pending);
        return keys.length > 0;
    }

    /**
     * protectsPanel — надо ли отложить пересборку панели.
     *
     * Проверяются ДВА состояния, потому что оператор заполняет форму не только
     * набором: он мог набрать значение и уйти из поля (фокус уже не там), и мог
     * оставить несохранённые правки в уже отрисованной форме.
     */
    function protectsPanel(root) {
        if (isEditingIn(root)) { return true; }
        return hasUnsavedEdits(root);
    }

    // --- секции <details> ---------------------------------------------------

    /**
     * detailsKey — стабильный ключ секции.
     *
     * data-acc и data-model-acc — собственные идентификаторы разметки GGUF;
     * id/detailsClassName — запасной вариант для будущих раскрываемых блоков.
     * Без ключа узел не участвует в переносе (иначе одно и то же состояние
     * доставалось бы всем подряд).
     */
    function detailsKey(node) {
        if (!node || typeof node.getAttribute !== 'function') { return ''; }
        var own = node.getAttribute('data-acc');
        var model = node.getAttribute('data-model-acc');
        var id = node.id || node.getAttribute('id') || '';
        var kind = String(node.className || node.getAttribute('class') || '').split(/\s+/).filter(function (c) {
            return c.indexOf('gguf-') === 0;
        })[0] || '';
        if (model) { return kind + '|model:' + String(model); }
        if (own) { return kind + '|acc:' + String(own); }
        if (id) { return kind + '|id:' + String(id); }
        return '';
    }

    /** rememberOpen — запомнить, какие секции поддерева раскрыты. */
    function rememberOpen(root) {
        if (!root || typeof root.querySelectorAll !== 'function') { return 0; }
        var nodes = root.querySelectorAll('details');
        var saved = 0;
        for (var i = 0; i < nodes.length; i++) {
            var key = detailsKey(nodes[i]);
            if (!key) { continue; }
            openState[key] = nodes[i].open === true;
            saved++;
        }
        return saved;
    }

    /**
     * applyOpen — восстановить раскрытые секции после пересборки.
     *
     * Узел без записи в состоянии остаётся таким, каким его нарисовала разметка
     * (первая секция формы открыта по умолчанию) — иначе свежая страница
     * выглядела бы «всё раскрыто».
     */
    function applyOpen(root) {
        if (!root || typeof root.querySelectorAll !== 'function') { return 0; }
        var nodes = root.querySelectorAll('details');
        var moved = 0;
        for (var i = 0; i < nodes.length; i++) {
            var key = detailsKey(nodes[i]);
            if (!key) { continue; }
            if (!Object.prototype.hasOwnProperty.call(openState, key)) { continue; }
            var want = openState[key] === true;
            if (nodes[i].open !== want) {
                nodes[i].open = want;
                moved++;
            }
        }
        return moved;
    }

    /**
     * rebuild — безопасно заменить содержимое узла.
     *
     * @param {Element} root   узел, чей innerHTML будет заменён;
     * @param {Function|string} render  разметка (функция вызывается ПОСЛЕ снятия
     *                                  значений: рендер видит актуальное состояние);
     * @param {Object} [opts]  {auto:true} — автоматический проход: при активной
     *                         форме пересборка ОТКЛАДЫВАЕТСЯ. Явные действия
     *                         оператора флаг не передают — пересборка происходит,
     *                         но с переносом значений и раскрытых секций.
     * @returns {boolean} true — DOM заменён, false — пересборка отложена/нечем.
     */
    function rebuild(root, render, opts) {
        if (!root) { return false; }
        var auto = !!(opts && opts.auto);
        if (auto && protectsPanel(root)) { return false; }
        var pending = collect(root);
        rememberOpen(root);
        var html = (typeof render === 'function') ? render() : render;
        if (html === null || html === undefined) { return false; }
        root.innerHTML = String(html);
        restore(root, pending);
        applyOpen(root);
        return true;
    }

    /**
     * afterRender — обновить базы после ПРЯМОЙ перерисовки поддерева (не через
     * rebuild): так делают загрузка формы параметров бэкенда и гидратация
     * профилей. Без этого следующие значения, записанные программой, выглядели
     * бы «правкой оператора» и навсегда блокировали пересборку панели.
     */
    function afterRender(root) {
        if (!root) { return false; }
        markBases(root);
        applyOpen(root);
        return true;
    }

    /**
     * noteSaved — штатное снятие защиты после успешного сохранения.
     *
     * FormDirty.noteSave убирает «свежую правку страницы», markBases объявляет
     * текущие значения базовыми: форма больше не считается изменённой, и
     * авто-обновление снова ею управляет (в том числе подставляет серверные
     * значения после ответа на сохранение).
     */
    function noteSaved(target) {
        var FD = formDirty();
        var id = (typeof target === 'string') ? target : (pageIdOf(target) || PAGE_FALLBACK_ID);
        if (FD && typeof FD.noteSave === 'function') { FD.noteSave(id); }
        if (target && typeof target === 'object') { markBases(target); }
        return id;
    }

    var api = {
        PAGE_ID: PAGE_FALLBACK_ID,
        PAGE_ID_FALLBACK: PAGE_FALLBACK_ID,
        pageIdOf: pageIdOf,
        isEditingIn: isEditingIn,
        hasUnsavedEdits: hasUnsavedEdits,
        protectsPanel: protectsPanel,
        collect: collect,
        restore: restore,
        markBases: markBases,
        rememberOpen: rememberOpen,
        applyOpen: applyOpen,
        detailsKey: detailsKey,
        rebuild: rebuild,
        afterRender: afterRender,
        noteSaved: noteSaved,
        _openState: function () { return openState; }
    };
    if (typeof window !== 'undefined') {
        window.GgufDetailFormState = api;
    }
    if (typeof module !== 'undefined' && module.exports) {
        module.exports = api; // node-тесты без DOM
    }
})();

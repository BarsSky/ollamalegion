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
// ---------------------------------------------------------------------------
// R689 (2026-11-23): ПАНЕЛЬ ПЕРЕСОБИРАЛАСЬ ДАЖЕ КОГДА ДАННЫЕ НЕ МЕНЯЛИСЬ.
//
// ЖИВАЯ ЖАЛОБА (вторая половина той же истории): «Пока не вводится значение,
// интерфейс продолжает скакать при обновлении, но форму не закрывает. Когда же
// есть фокус на поле для ввода, то обновления скачками пропадают, и не понятно,
// обновляется ли статус».
//
// ЗАМЕР ДО ПРАВКИ (CDP, образ r688, вкладка «Настройки» бэкенда, 20 с без ввода):
// 12 подмен DOM правой панели (8 панели + 4 содержимого) при НЕИЗМЕННЫХ данных.
// Каждая подмена — это и есть «скачок»: innerHTML пересоздаёт те же узлы, на
// мгновение меняет высоту/раскладку, сбрасывает положение прокрутки и заставляет
// перерисоваться всё поддерево. При этом блоки формы, которые дорисовываются
// асинхронно (data-role="cpp-load-defaults"/"cpp-model-catalog"), после подмены
// остаются заглушкой «Загрузка…», пока оператор не кликнет по табу заново.
//
// ПРАВИЛО R689. Пересборка начинается со сравнения: новая разметка против ТОГО,
// ЧТО МОДУЛЬ ЗАПИСАЛ В ЭТОТ УЗЕЛ ПРОШЛЫЙ РАЗ (WeakMap по узлу). Совпало —
// DOM не трогаем ВООБЩЕ (ни innerHTML, ни восстановлений: трогать нечего).
// Не совпало — подмена происходит, но с сохранением scrollTop/scrollLeft (самого
// узла и всех прокручиваемых предков), раскрытых <details>, введённых значений и
// позиции каретки.
//
// ПОЧЕМУ СРАВНИВАЕМ СО СНИМКОМ, А НЕ С ЖИВЫМ innerHTML. Часть панели дорисовывают
// ЧУЖИЕ модули (форма параметров загрузки, cpp-load-defaults, cpp-model-catalog,
// список профилей), причём напрямую в DOM. Живой innerHTML поэтому НИКОГДА не
// совпадёт со свежим рендером: сравнение с ним всегда давало бы «изменилось» и
// возвращало бы ровно то поведение, на которое жалуются. Снимок же отвечает на
// правильный вопрос: «изменились ли ДАННЫЕ, из которых этот узел нарисован».
//
// R689 также даёт оператору обратную связь, когда пересборка отложена: модуль
// зовёт window.GgufRefreshStatus (см. js/modules/gguf-refresh-status.js), и рядом
// с панелью видно «данные обновлены 3 с назад · перерисовка приостановлена, пока
// вы вводите». Данные при этом продолжают идти: заслон стоит на DOM, а не на
// запросах.
//
// ---------------------------------------------------------------------------
// R691 (2026-11-23): ПЕРЕСБОРКА БОЛЬШЕ НЕ ВОЗВРАЩАЕТ ЗАГЛУШКУ «Загрузка…».
//
// R689 убрал пустые пересборки, но когда данные РЕАЛЬНО меняются, подмена DOM
// нужна — и вместе с ней уничтожалось всё, что дорисовали чужие модули по
// ответам API (профили моделей, «что получат новые модели», каталог, параметры
// модели). В свежей разметке на их месте снова заглушка, а повторный запрос не
// запускался до клика по вкладке. Замер на стенде (r690): 2 блока в «Загрузка…»
// сразу после пересборки, 0 фоновых запросов за 20 с, содержимое не вернулось;
// высота панели 4268 → 1935 px.
//
// Теперь рядом с пересборкой стоит КЭШ содержимого (js/modules/gguf-async-cache.js):
// сразу после подмены DOM блоки рисуются из него (синхронно — заглушка не успевает
// появиться), а фоном догружаются свежие данные. Сам этот модуль остаётся
// владельцем правил «когда пересобирать» и «что переносить»; кэш он только
// инвалидирует при сохранении (noteSaved ниже), потому что сохранение — новый
// базовый срез и для данных блоков тоже.
//
// Тесты: webui/tests/gguf-form-guard-r688.test.js (RED на коде до R688),
//        webui/tests/gguf-auto-refresh-stability-r689.test.js (RED на коде до R689),
//        webui/tests/gguf-async-blocks-cache-r691.test.js (RED на коде до R691).
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

    // --- R689: снимок разметки, прокрутка, каретка, обратная связь -----------

    /**
     * lastWritten — что модуль записал в каждый узел в прошлый раз.
     *
     * WeakMap по узлу: узел выбросили (сменилась страница/бэкенд) — запись ушла
     * вместе с ним, памяти не течёт и чужой узел со старым снимком не сравнить.
     */
    var lastWritten = (typeof WeakMap === 'function') ? new WeakMap() : null;

    /**
     * normalizeHtml — сравнимая форма разметки.
     *
     * Между тегами отступы не влияют на картинку, поэтому переводы строк и
     * пробелы вокруг «><» схлопываем: иначе косметическая разница форматирования
     * считалась бы изменением данных (и давала бы тот самый «скачок» на ровном
     * месте). Содержимое текстовых узлов не нормализуется — реальные изменения
     * (цифры, статусы) видны и различаются.
     */
    function normalizeHtml(html) {
        if (html === null || html === undefined) { return ''; }
        return String(html)
            .replace(/\r\n?/g, '\n')
            .replace(/>[\t ]*\n[\t ]*</g, '><')
            .replace(/^[\t\n\r ]+|[\t\n\r ]+$/g, '');
    }

    /** pushScroll — запомнить положение прокрутки узла (если оно не нулевое). */
    function pushScroll(out, el) {
        if (!el) { return; }
        var top = el.scrollTop;
        var left = el.scrollLeft;
        if (typeof top !== 'number') { return; }
        if (!top && !left) { return; }
        out.push({ el: el, top: top, left: (typeof left === 'number') ? left : 0 });
    }

    /**
     * captureScroll — положение прокрутки узла И его прокручиваемых предков.
     *
     * ЗАЧЕМ ПРЕДКИ. Сам .gguf-detail-panel не прокручивается: прокрутка живёт на
     * контейнере страницы/документе. innerHTML панели на мгновение уменьшает
     * высоту содержимого (асинхронные блоки формы ещё не дорисованы), браузер
     * упирает scrollTop в новый предел — и после перерисовки оператор оказывается
     * в другом месте. Это и есть «прыгает прокрутка» из жалобы, поэтому чиним не
     * только сам узел, но и путь к нему.
     */
    function captureScroll(root) {
        var out = [];
        if (!root) { return out; }
        pushScroll(out, root);
        var el = root.parentNode;
        var guard = 0;
        while (el && guard < 60) {
            guard++;
            pushScroll(out, el);
            el = el.parentNode || null;
        }
        if (typeof document !== 'undefined' && document.scrollingElement) {
            pushScroll(out, document.scrollingElement);
        }
        return out;
    }

    /** restoreScroll — вернуть запомненное положение прокрутки. @returns {number} */
    function restoreScroll(saved) {
        var moved = 0;
        if (!saved || !saved.length) { return 0; }
        for (var i = 0; i < saved.length; i++) {
            var rec = saved[i];
            if (!rec || !rec.el) { continue; }
            try {
                if (rec.el.scrollTop !== rec.top) { rec.el.scrollTop = rec.top; moved++; }
                if (typeof rec.left === 'number' && rec.el.scrollLeft !== rec.left) {
                    rec.el.scrollLeft = rec.left;
                }
            } catch (e) { /* узел отрезан от документа — восстанавливать нечего */ }
        }
        return moved;
    }

    /**
     * captureCaret — поле с фокусом и позиция каретки.
     *
     * Авто-проход не доходит сюда с фокусом в поле (панель защищена), но явная
     * пересборка может случиться в момент, когда оператор просто смотрит в поле
     * (например, после «Повторить» на плашке ошибки). Возвращаем фокус тому же
     * полю по id — идентичность узлов innerHTML всё равно уничтожает.
     */
    function captureCaret(root) {
        if (typeof document === 'undefined' || !document.activeElement) { return null; }
        var active = document.activeElement;
        if (!active || active === root) { return null; }
        if (root && typeof root.contains === 'function' && !root.contains(active)) { return null; }
        var id = active.id || (typeof active.getAttribute === 'function' ? active.getAttribute('id') : '') || '';
        if (!id) { return null; }
        var caret = { id: String(id), start: null, end: null };
        try {
            if (typeof active.selectionStart === 'number') {
                caret.start = active.selectionStart;
                caret.end = active.selectionEnd;
            }
        } catch (e) { /* number/checkbox не поддерживают выделение */ }
        return caret;
    }

    /** restoreCaret — вернуть фокус и выделение полю с тем же id. */
    function restoreCaret(caret) {
        if (!caret || !caret.id) { return false; }
        if (typeof document === 'undefined' || typeof document.getElementById !== 'function') { return false; }
        var el = document.getElementById(caret.id);
        if (!el || typeof el.focus !== 'function') { return false; }
        try {
            el.focus({ preventScroll: true });
            if (caret.start !== null && typeof el.setSelectionRange === 'function') {
                el.setSelectionRange(caret.start, caret.end === null ? caret.start : caret.end);
            }
            return true;
        } catch (e) { return false; }
    }

    /** refreshStatus — модуль видимой обратной связи (может быть не подключён). */
    function refreshStatus() {
        var w = win();
        return (w && w.GgufRefreshStatus) || null;
    }

    function statusNotify(method, arg) {
        var S = refreshStatus();
        if (S && typeof S[method] === 'function') {
            try { S[method](arg); } catch (e) { /* индикатор не должен ломать рендер */ }
        }
    }

    /** forgetSnapshot — заставить следующий проход пересобрать узел (и предков). */
    function forgetSnapshot(root) {
        if (!lastWritten || !root) { return 0; }
        var el = root;
        var guard = 0;
        var dropped = 0;
        while (el && guard < 20) {
            guard++;
            if (lastWritten.has(el)) {
                lastWritten.delete(el);
                dropped++;
            }
            el = el.parentNode || null;
        }
        return dropped;
    }

    /**
     * rebaseline — объявить снимок узла равным текущему рендеру.
     *
     * ЗАЧЕМ. Часть панели дорисовывается НАПРЯМУЮ (форма параметров загрузки,
     * профили модели, каталог): после этой дорисовки рендер панели впервые
     * содержит форму вместо заглушки «Загрузка…». Без rebaseline следующий
     * авто-проход увидел бы «разметка другая», пересобрал панель — и уничтожил
     * только что приехавшие блоки, вернув их в состояние заглушки (замер: высота
     * панели 4268 → 1935 px, defaults=false, catalog=false). rebaseline говорит:
     * «то, что сейчас показывает страница, и есть актуальный вид», поэтому
     * пересборка не нужна ни на этом проходе, ни на следующих.
     *
     * Вызывает тот, кто ЗНАЕТ рендер-функции панели (gguf-detail-settings.js →
     * rebaselinePanel), а не сам модуль: он не должен зависеть от рендереров.
     */
    function rebaseline(root, html) {
        if (!lastWritten || !root) { return false; }
        if (html === null || html === undefined) { return false; }
        lastWritten.set(root, String(html));
        return true;
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
        if (auto && protectsPanel(root)) {
            // R689(2): пересборка отложена — оператор должен видеть, что данные
            // всё равно обновляются и почему панель не перерисовывается.
            statusNotify('noteDeferred');
            return false;
        }
        // С этого места пересборка разрешена: «приостановлено» больше не правда.
        statusNotify('noteResumed');
        // Порядок как в R688: снимаем значения ДО рендера — рендер видит
        // актуальное состояние. Все снятия ниже — только ЧТЕНИЕ DOM.
        var pending = collect(root);
        rememberOpen(root);
        var scroll = captureScroll(root);
        var caret = captureCaret(root);
        var html = (typeof render === 'function') ? render() : render;
        if (html === null || html === undefined) { return false; }
        var next = String(html);
        // R689(1): данные не изменились — DOM не трогаем ВООБЩЕ. Именно эта
        // ветка убирает «скачки»: 12 подмен за 20 с при неизменных данных.
        var prev = lastWritten ? lastWritten.get(root) : undefined;
        if (prev !== undefined && normalizeHtml(prev) === normalizeHtml(next)) {
            return false;
        }
        root.innerHTML = next;
        if (lastWritten) { lastWritten.set(root, next); }
        restore(root, pending);
        applyOpen(root);
        restoreScroll(scroll);
        restoreCaret(caret);
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
        if (target && typeof target === 'object') {
            markBases(target);
            // R689: сохранение — новый базовый срез. Снимок «что мы писали в DOM»
            // больше не описывает то, что должно быть на экране (в форме остались
            // значения оператора, а сервер подтвердил свои), поэтому следующий
            // проход обязан пересобрать поддерево: снимок забываем.
            forgetSnapshot(target);
        }
        // R691 (2026-11-23): сохранение — новый повод не доверять кэшу
        // асинхронных блоков: их содержимое описывает состояние ДО записи.
        // Помечаем записи устаревшими (не стираем): до ответа на перезапрос
        // оператор видит прежнее содержимое, а не заглушку «Загрузка…».
        // Через noteSaved проходят ВСЕ штатные сохранения страницы —
        // saveBackendOptions, promoteBackendOptionsToDefaults и applyModelParams.
        var C = win() && win().GgufAsyncCache;
        if (C && typeof C.invalidate === 'function') {
            try { C.invalidate('saved'); } catch (e) { /* кэш не должен мешать сохранению */ }
        }
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
        rebaseline: rebaseline,
        normalizeHtml: normalizeHtml,
        captureScroll: captureScroll,
        restoreScroll: restoreScroll,
        _openState: function () { return openState; },
        // Тесты R689: снимок, записанный для узла, и сброс (новый узел — чистый
        // лист, но в тесте узлы переиспользуются между проверками).
        _snapshotOf: function (root) { return lastWritten ? lastWritten.get(root) : undefined; },
        _forget: function (root) { if (lastWritten && root) { lastWritten.delete(root); } }
    };
    if (typeof window !== 'undefined') {
        window.GgufDetailFormState = api;
    }
    if (typeof module !== 'undefined' && module.exports) {
        module.exports = api; // node-тесты без DOM
    }
})();

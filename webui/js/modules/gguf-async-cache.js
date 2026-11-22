// js/modules/gguf-async-cache.js — R691 (2026-11-23): пересборка правой панели
// GGUF больше не возвращает асинхронно дорисованные блоки в заглушку «Загрузка…».
//
// ЖИВАЯ ЖАЛОБА (из отчёта R689, подтверждена замером на стенде). Правая панель
// GGUF пересобирается ТОЛЬКО когда данные реально изменились (R689), но часть её
// содержимого дорисовывается ПОСЛЕ панели по ответам API:
//
//   * «что получат новые модели»   — [data-role="cpp-load-defaults"]
//                                    (cppworker-params.js → loadAndRenderDefaults);
//   * «модели в папке»             — [data-role="cpp-model-catalog"]
//                                    (cppworker-params.js → loadAndRenderCatalog);
//   * список профилей моделей      — #cppProfilesList
//                                    (cppworker-params.js → loadAndRender);
//   * «параметры запуска модели»   — .gguf-mp-editor[data-model]
//                                    (gguf-detail-params.js → fillModelParamEditor /
//                                     hydrateModelParamEditors), значения профиля
//                                     приходят отдельным запросом.
//
// Пересборка панели (innerHTML) уничтожает всё, что дорисовали эти модули:
// в свежем рендере панели на их месте опять заглушка, а повторный запрос/отрисовка
// НЕ запускаются, пока оператор не переключит вкладку/модель заново. Замер ДО
// правки (CDP, образ r690, вкладка «Настройки», явная пересборка после реального
// изменения данных): 2 блока в «Загрузка…» сразу после пересборки, 0 фоновых
// запросов за 20 с, содержимое не вернулось вообще; высота панели 4268 → 1935 px;
// раскрытая карточка параметров модели потеряла 3 пометки «предел окна».
//
// ЧТО ДЕЛАЕТ МОДУЛЬ. Держит ПОСЛЕДНЕЕ УСПЕШНО ОТРИСОВАННОЕ содержимое (данные
// ответа, а не готовый HTML) каждого асинхронного блока и отдаёт его обратно
// при пересборке — синхронно, из памяти вкладки:
//
//   1) `restore(root)` вызывается ПОСЛЕ успешной подмены DOM панели
//      (gguf-renderer-refresh.js → renderIntoDetail). Для каждого блока,
//      зарегистрированного своим модулем, он находит элементы в НОВОМ DOM и
//      рисует их из кэша — заглушка не успевает появиться даже на кадр.
//      Затем запускается фоновое обновление (`revalidate`), которое заменяет
//      содержимое свежим ответом.
//
//   2) Данные, а не HTML. Модуль хранит ответ API (или его разобранную часть) и
//      отдаёт его РЕНДЕРЕРУ блока. Поэтому обработчики кнопок внутри блока
//      навешиваются заново штатной функцией блока: после пересборки кнопки
//      «Изменить»/«Настроить» работают сразу, а не после фонового обновления.
//
//   3) Ничего не моргает. Отрисовка идёт через `setRendered(el, render)`: блок
//      сначала рисуется в ЗАГЛУШКУ-узел (detached div), его разметка сравнивается
//      с текущей по нормализованному виду (тот же приём, что в
//      gguf-detail-form-state.js, — там это сделано для панели целиком) и только
//      при отличии рисуется в живой узел. Совпало — DOM не трогается вообще.
//
//   4) Кэш привязан к КОНТЕКСТУ: страница + бэкенд + вкладка/секция + модель.
//      Ключ пересчитывается при каждом обращении, поэтому смена бэкенда, вкладки
//      или модели — это промах (чужое содержимое не подставляется даже на кадр).
//
//   5) Инвалидация: после сохранения (noteSaved), явного «Обновить»/«Reload» и
//      при смене страницы записи помечаются устаревшими (или стираются).
//      Устаревшая запись ЕЩЁ может быть нарисована, чтобы не показывать заглушку
//      поверх только что показанного содержимого, но она никогда не считается
//      свежей, и первый же успешный ответ её заменяет. Ответы, начатые ДО
//      инвалидации/нового запроса, не применяются вовсе (`begin`/`commit`:
//      номер запроса + поколение кэша — устаревший ответ не «перебивает» свежий).
//
//   6) Заглушка «Загрузка…» остаётся только там, где данных в этом контексте ещё
//      ни разу не было (блок без записи в кэше и без рисунка остаётся таким,
//      каким его нарисовал рендер панели).
//
// ЗАВИСИМОСТИ. Ноль обязательных: GgufModule.state читается как источник
// контекста, window.GgufDetailFormState.normalizeHtml используется только для
// сравнения (если подключён — иначе локальная копия нормализации). Модуль грузится
// ДО gguf-renderer-refresh.js и ДО модулей, которые регистрируют блоки.
//
// Тест: webui/tests/gguf-async-blocks-cache-r691.test.js (RED на коде до R691).
// Живой замер: scripts/verify-gguf-async-blocks-r691.js.
(function () {
    'use strict';

    var VERSION = 'R691';

    function win() {
        return (typeof window !== 'undefined') ? window : null;
    }

    function ggufState() {
        var w = win();
        var M = w && w.GgufModule;
        return (M && M.state && typeof M.state === 'object') ? M.state : {};
    }

    function doc() {
        return (typeof document !== 'undefined') ? document : null;
    }

    /**
     * currentPage — открытая страница SPA.
     *
     * Первый источник — window.App.state.currentPage (app.js выставляет её в
     * switchPage), второй — активный .page-контейнер. Оба доступны без правки
     * app.js: смена страницы определяется ЛЕНИВО, при первом же обращении к кэшу.
     */
    function currentPage() {
        var w = win();
        var page = w && w.App && w.App.state && w.App.state.currentPage;
        if (page) { return String(page); }
        var d = doc();
        if (d && typeof d.querySelector === 'function') {
            var active = d.querySelector('.page.active');
            if (active && active.id) { return String(active.id); }
        }
        return '';
    }

    // --- состояние кэша ------------------------------------------------------

    // entries[key] = { kind, page, backend, pane, model, data, at, stale, reason }
    var entries = Object.create(null);
    // inflight[key] = true — запрос за этим блоком уже идёт (дубли не плодим).
    var inflight = Object.create(null);
    // latestReq[key] = seq последнего запроса (устаревший ответ не применяется).
    var latestReq = Object.create(null);
    // Поколение кэша: растёт на каждой инвалидации. Тикет, выданный до неё,
    // не имеет права ни записать кэш, ни обновить DOM.
    var generation = 0;
    var seqCounter = 0;
    // Последняя увиденная страница: по смене — инвалидация (см. ensurePage).
    var lastPage = null;
    // Зарегистрированные блоки: { kind, pane, scope, selector, modelOf, render, revalidate }
    var blocks = [];
    // Очередь фонового обновления: одна микрозадержка, чтобы панель сначала
    // дорисовалась, а уже потом пошли запросы.
    var revalidateQueue = [];
    var revalidateTimer = null;

    var stats = {
        writes: 0,
        reads: 0,
        hits: 0,
        staleHits: 0,
        misses: 0,
        invalidations: 0,
        dropped: 0,
        restored: 0,
        painted: 0,
        skippedSame: 0,
        rejectedStale: 0,
        revalidated: 0
    };

    function numericNow() { return Date.now(); }

    /**
     * scopeOf — контекст записи.
     *
     * page/backend/pane берутся из живого состояния, model — из аргумента
     * (у блоков параметров модели она своя у каждого редактора). Явно
     * переданные поля важнее живых: так тест (и revalidate уже начатого запроса)
     * называет контекст тот, в котором запрос начинался.
     */
    function scopeOf(scope) {
        var st = ggufState();
        scope = scope || {};
        return {
            page: (scope.page !== undefined) ? String(scope.page) : currentPage(),
            backend: (scope.backend !== undefined) ? String(scope.backend) : String(st.selectedBackendId || ''),
            pane: (scope.pane !== undefined) ? String(scope.pane) : String(st.detailPane || ''),
            model: String(scope.model || '')
        };
    }

    function keyOf(kind, scope) {
        var s = scopeOf(scope);
        return [s.page, s.backend, s.pane, s.model, String(kind || '')].join('|');
    }

    /**
     * ensurePage — ленивое определение смены страницы.
     *
     * ПОЧЕМУ НЕ ПОДПИСКА НА switchPage. Роутер живёт в app.js, и трогать его
     * ради кэша одного блока — лишняя связность (плюс тесты SPA гоняют app.js в
     * песочнице). Достаточно сравнивать текущее значение при обращении: любой
     * код, который читает/пишет кэш или перерисовывает панель, сначала проходит
     * здесь, поэтому после «ушли на другую страницу и вернулись» кэш уже пуст.
     *
     * @returns {boolean} true — страница сменилась и записи стёрты.
     */
    function ensurePage() {
        var page = currentPage();
        if (lastPage === null) { lastPage = page; return false; }
        if (page !== lastPage) {
            lastPage = page;
            // Смена страницы — самый сильный повод забыть всё: данные могли
            // измениться, пока страница была закрыта, а оператор вернулся.
            invalidate('page', { hard: true });
            return true;
        }
        return false;
    }

    // --- нормализация и сравнение разметки -----------------------------------

    /**
     * normalizeHtml — сравнимая форма разметки блока.
     *
     * Тот же приём, что в gguf-detail-form-state.js (R689) для панели целиком:
     * отступы между тегами не влияют на картинку, поэтому схлопываем переводы
     * строк/пробелы вокруг «><». Содержимое текстовых узлов не нормализуется —
     * реальные изменения (цифры, статусы) должны различаться.
     */
    function normalizeHtml(html) {
        var w = win();
        var G = w && w.GgufDetailFormState;
        if (G && typeof G.normalizeHtml === 'function') {
            return G.normalizeHtml(html);
        }
        if (html === null || html === undefined) { return ''; }
        return String(html)
            .replace(/\r\n?/g, '\n')
            .replace(/>[\t ]*\n[\t ]*</g, '><')
            .replace(/^[\t\n\r ]+|[\t\n\r ]+$/g, '');
    }

    /** isPlaceholderHtml — разметка-заглушка «Загрузка…» (данных ещё не было). */
    function isPlaceholderHtml(html) {
        var text = String(html === null || html === undefined ? '' : html)
            .replace(/<[^>]*>/g, ' ')
            .replace(/\s+/g, ' ')
            .trim();
        if (text.length > 80) { return false; }
        return /Загрузка|Loading|Загрузка…/i.test(text);
    }

    /**
     * setRendered — отрисовать блок, не трогая DOM, если разметка та же.
     *
     * ЗАЧЕМ ЗАГЛУШКА-УЗЕЛ. Рендерер блока сам навешивает обработчики на СВОИ
     * узлы (кнопки «Изменить»/«Настроить»). Поэтому «сравнить» и «нарисовать» —
     * два отдельных прохода: первый в detached div (обработчики не нужны),
     * второй — в живой узел, только если разметка отличается. Тогда при
     * неизменных данных живой DOM не пересоздаётся вообще (требование «не
     * моргать»), а при изменённых — обработчики навешивает штатный рендерер.
     *
     * @param {Element} el       живой узел блока;
     * @param {Function} render  render(node) — рисовать в переданный узел;
     * @param {Object} [opts]    {force:true} — рисовать без сравнения.
     * @returns {boolean} true — живой DOM заменён.
     */
    function setRendered(el, render, opts) {
        if (!el || typeof render !== 'function') { return false; }
        var force = !!(opts && opts.force);
        if (!force) {
            // Пока оператор печатает ВНУТРИ блока — блок не трогаем: это
            // состояние формы, а не данные. (Открытая форма «Изменить» в
            // настройках по умолчанию — ровно этот случай.)
            var d = doc();
            var active = d && d.activeElement;
            if (active && typeof el.contains === 'function' && el.contains(active)) {
                stats.skippedSame++;
                return false;
            }
        }
        var probe = null;
        if (!force) {
            try {
                var d2 = doc();
                if (d2 && typeof d2.createElement === 'function') { probe = d2.createElement('div'); }
            } catch (e) { probe = null; }
        }
        if (!probe) {
            render(el);
            stats.painted++;
            return true;
        }
        render(probe);
        var next = probe.innerHTML;
        if (normalizeHtml(el.innerHTML) === normalizeHtml(next)) {
            stats.skippedSame++;
            return false;
        }
        render(el);
        stats.painted++;
        return true;
    }

    // --- чтение/запись -------------------------------------------------------

    /**
     * read — последнее успешное содержимое блока в ТЕКУЩЕМ контексте.
     *
     * @param {string} kind
     * @param {Object} [scope]
     * @param {Object} [opts] {allowStale:true} — вернуть и помеченную устаревшей
     *                        запись (мост, чтобы не показывать заглушку).
     * @returns {*} данные ответа либо undefined (данных в этом контексте не было).
     */
    function read(kind, scope, opts) {
        ensurePage();
        var allowStale = !!(opts && opts.allowStale);
        var key = keyOf(kind, scope);
        stats.reads++;
        var entry = entries[key];
        if (!entry) { stats.misses++; return undefined; }
        if (entry.stale && !allowStale) { stats.misses++; return undefined; }
        if (entry.stale) { stats.staleHits++; } else { stats.hits++; }
        return entry.data;
    }

    /** has — есть ли запись (свежая или устаревшая) в текущем контексте. */
    function has(kind, scope) {
        return read(kind, scope, { allowStale: true }) !== undefined;
    }

    function write(kind, data, scope) {
        ensurePage();
        var s = scopeOf(scope);
        var key = keyOf(kind, scope);
        entries[key] = {
            kind: String(kind || ''),
            page: s.page,
            backend: s.backend,
            pane: s.pane,
            model: s.model,
            data: data,
            at: numericNow(),
            stale: false,
            reason: ''
        };
        stats.writes++;
        return key;
    }

    /**
     * begin — начать загрузку блока.
     *
     * Один запрос на блок/контекст: второй вызов, пока ответ не пришёл, получает
     * null (вызывающий просто не дублирует работу). Тикет несёт номер запроса и
     * поколение кэша — по ним `commit` решает, имеет ли ответ право на запись.
     *
     * @returns {{key:string,kind:string,seq:number,gen:number,scope:Object}|null}
     */
    function begin(kind, scope) {
        ensurePage();
        var key = keyOf(kind, scope);
        if (inflight[key]) { return null; }
        inflight[key] = true;
        var seq = ++seqCounter;
        latestReq[key] = seq;
        return { key: key, kind: String(kind || ''), seq: seq, gen: generation, scope: scopeOf(scope) };
    }

    /**
     * commit — применить ответ загрузки.
     *
     * false означает «ответ устарел»: пока он шёл, началась другая загрузка или
     * кэш инвалидировали (сохранение/Reload/смена страницы). Такой ответ НЕ
     * пишется в кэш и НЕ рисуется — иначе старые данные «перебили» бы свежие.
     */
    function commit(ticket, data) {
        if (!ticket || !ticket.key) { return false; }
        inflight[ticket.key] = false;
        if (ticket.gen !== generation || latestReq[ticket.key] !== ticket.seq) {
            stats.rejectedStale++;
            return false;
        }
        var entry = {
            kind: ticket.kind,
            page: ticket.scope.page,
            backend: ticket.scope.backend,
            pane: ticket.scope.pane,
            model: ticket.scope.model,
            data: data,
            at: numericNow(),
            stale: false,
            reason: ''
        };
        entries[ticket.key] = entry;
        stats.writes++;
        return true;
    }

    /** abort — снять «в полёте» после ошибки (кэш и DOM не трогаем). */
    function abort(ticket) {
        if (!ticket || !ticket.key) { return false; }
        inflight[ticket.key] = false;
        return true;
    }

    /** drop — забыть блок в текущем контексте (без пометки «устарело»). */
    function drop(kind, scope) {
        var key = keyOf(kind, scope);
        if (entries[key]) { delete entries[key]; stats.dropped++; return true; }
        return false;
    }

    /**
     * invalidate — кэш больше не считается свежим.
     *
     * @param {string} reason  'saved' | 'refresh' | 'reload' | 'page' | ...
     * @param {Object} [opts]  {hard:true} — стереть записи; {kinds:[...]} —
     *                         только эти блоки.
     * @returns {number} сколько записей затронуто.
     */
    function invalidate(reason, opts) {
        opts = opts || {};
        var kinds = Array.isArray(opts.kinds) ? opts.kinds : null;
        var touched = 0;
        Object.keys(entries).forEach(function (key) {
            var entry = entries[key];
            if (!entry) { return; }
            if (kinds && kinds.indexOf(entry.kind) === -1) { return; }
            if (opts.hard) {
                delete entries[key];
                stats.dropped++;
            } else {
                entry.stale = true;
                entry.reason = String(reason || '');
            }
            touched++;
        });
        // Поколение растёт всегда: даже «мягкая» инвалидация означает, что
        // ответы, начатые до неё, уже не описывают актуальное состояние.
        generation++;
        // Начатые до инвалидации загрузки больше не «держат» блок: их ответ
        // отвергнет commit по поколению, а новый запрос должен начаться сразу
        // (иначе после Reload блок ждал бы ответа, который всё равно выбросят).
        inflight = Object.create(null);
        stats.invalidations++;
        return touched;
    }

    // --- блоки ---------------------------------------------------------------

    /**
     * register — зарегистрировать асинхронный блок.
     *
     * Обязательные поля:
     *   kind     — имя кэша блока ('cppLoadDefaults' и т.п.);
     *   selector — как найти блок в DOM панели;
     *   render   — render(el, data): рисовать блок из данных ответа;
     *   revalidate — (необязательно) заново запросить данные блока.
     * Необязательные:
     *   pane     — вкладка панели, на которой блок живёт ('settings');
     *   scope    — 'model' — контекст включает модель элемента (modelOf);
     *   modelOf  — (el) => имя модели.
     *
     * Регистрация идемпотентна по kind: повторный вызов (перезагрузка фабрики в
     * тесте) заменяет описание, а не плодит второе.
     */
    function register(def) {
        if (!def || !def.kind || !def.selector || typeof def.render !== 'function') { return false; }
        for (var i = 0; i < blocks.length; i++) {
            if (blocks[i].kind === def.kind) { blocks[i] = def; return true; }
        }
        blocks.push(def);
        return true;
    }

    function blocksOf() { return blocks.slice(); }

    function matchesPane(def) {
        if (!def.pane || def.pane === '*') { return true; }
        return String(def.pane) === String(ggufState().detailPane || '');
    }

    function targetsOf(root, def) {
        if (!root || typeof root.querySelectorAll !== 'function') { return []; }
        try {
            return Array.prototype.slice.call(root.querySelectorAll(def.selector) || []);
        } catch (e) { return []; }
    }

    function scopeForElement(def, el) {
        if (def.scope !== 'model') { return {}; }
        var model = '';
        if (typeof def.modelOf === 'function') {
            try { model = String(def.modelOf(el) || ''); } catch (e) { model = ''; }
        }
        return { model: model };
    }

    /**
     * dataForElement — данные для конкретного элемента блока.
     *
     * Сначала свежая запись, затем (мост) устаревшая. Если записей нет вовсе —
     * undefined: блок остаётся таким, каким его нарисовал рендер панели
     * (заглушка «Загрузка…» — только в этом случае).
     */
    function dataForElement(def, el) {
        var scope = scopeForElement(def, el);
        var data = read(def.kind, scope);
        if (data !== undefined) { return data; }
        return read(def.kind, scope, { allowStale: true });
    }

    function paintElement(def, el) {
        var data = dataForElement(def, el);
        if (data === undefined) { return false; }
        try {
            def.render(el, data);
        } catch (e) {
            console.warn('[gguf-async-cache] render ' + def.kind + ' failed:', e);
            return false;
        }
        stats.restored++;
        return true;
    }

    function panelRoot() {
        var d = doc();
        if (!d) { return null; }
        return (typeof d.getElementById === 'function') ? d.getElementById('ggufDetailPanel') : null;
    }

    /**
     * restore — вернуть содержимое асинхронных блоков сразу после пересборки.
     *
     * Вызывается из gguf-renderer-refresh.js, когда подмена DOM панели РЕАЛЬНО
     * произошла. Рисует блоки из кэша синхронно (поэтому заглушка не попадает
     * даже в один кадр) и ставит фоновое обновление в очередь.
     *
     * @param {Element} [root] узел панели (по умолчанию #ggufDetailPanel).
     * @returns {{painted:number, blocks:number, scheduled:number}}
     */
    function restore(root) {
        ensurePage();
        root = root || panelRoot();
        if (!root) { return { painted: 0, blocks: 0, scheduled: 0 }; }
        var paintedBlocks = 0;
        var scheduled = [];
        for (var i = 0; i < blocks.length; i++) {
            var def = blocks[i];
            if (!matchesPane(def)) { continue; }
            var targets = targetsOf(root, def);
            if (!targets.length) { continue; }
            var paintedAny = false;
            for (var j = 0; j < targets.length; j++) {
                if (paintElement(def, targets[j])) { paintedAny = true; }
            }
            if (paintedAny) { paintedBlocks++; }
            // Фоновое обновление нужно в ЛЮБОМ случае: если кэша не было —
            // именно оно наполнит блок (и только тогда появится заглушка —
            // требование «заглушка только там, где данных ещё не было»).
            scheduled.push(def);
        }
        if (scheduled.length) { scheduleRevalidate(scheduled); }
        return { painted: paintedBlocks, blocks: blocks.length, scheduled: scheduled.length };
    }

    function scheduleRevalidate(defs) {
        for (var i = 0; i < defs.length; i++) {
            if (revalidateQueue.indexOf(defs[i]) === -1) { revalidateQueue.push(defs[i]); }
        }
        if (revalidateTimer !== null) { return; }
        revalidateTimer = setTimeout(function () {
            revalidateTimer = null;
            var list = revalidateQueue;
            revalidateQueue = [];
            runRevalidate(list);
        }, 0);
    }

    function runRevalidate(list) {
        for (var i = 0; i < list.length; i++) {
            var def = list[i];
            stats.revalidated++;
            try {
                if (typeof def.revalidate === 'function') { def.revalidate(); }
                else if (typeof def.fetch === 'function') { fetchBlock(def); }
            } catch (e) {
                console.warn('[gguf-async-cache] revalidate ' + def.kind + ' failed:', e);
            }
        }
    }

    /** fetchBlock — загрузка блока силами самого кэша (если у блока есть fetch). */
    function fetchBlock(def) {
        var ticket = begin(def.kind, {});
        if (!ticket) { return; }
        Promise.resolve(def.fetch())
            .then(function (data) {
                if (!commit(ticket, data)) { return; }
                var entry = entries[ticket.key];
                if (!entry) { return; }
                var targets = targetsOf(panelRoot(), def);
                for (var i = 0; i < targets.length; i++) {
                    setRendered(targets[i], function (node) { def.render(node, entry.data); });
                }
            })
            .catch(function (err) {
                abort(ticket);
                console.warn('[gguf-async-cache] fetch ' + def.kind + ' failed:', err);
            });
    }

    /** revalidateAll — заново запросить все блоки текущей вкладки (явный Reload). */
    function revalidateAll() {
        var list = [];
        for (var i = 0; i < blocks.length; i++) {
            if (matchesPane(blocks[i])) { list.push(blocks[i]); }
        }
        scheduleRevalidate(list);
        return list.length;
    }

    var api = {
        VERSION: VERSION,
        // контекст и ключи
        ctxt: function () { return scopeOf({}); },
        keyOf: keyOf,
        // данные
        read: read,
        has: has,
        write: write,
        begin: begin,
        commit: commit,
        abort: abort,
        drop: drop,
        invalidate: invalidate,
        // блоки
        register: register,
        blocks: blocksOf,
        restore: restore,
        revalidateAll: revalidateAll,
        // отрисовка без моргания
        setRendered: setRendered,
        normalizeHtml: normalizeHtml,
        isPlaceholderHtml: isPlaceholderHtml,
        // диагностика/тесты
        _stats: function () { return Object.assign({}, stats); },
        _reset: function () {
            entries = Object.create(null);
            inflight = Object.create(null);
            latestReq = Object.create(null);
            generation = 0;
            seqCounter = 0;
            lastPage = null;
            blocks = [];
            revalidateQueue = [];
            if (revalidateTimer !== null) { clearTimeout(revalidateTimer); revalidateTimer = null; }
            Object.keys(stats).forEach(function (k) { stats[k] = 0; });
            return true;
        },
        _pending: function () { return { queued: revalidateQueue.length, timer: revalidateTimer !== null, inflight: Object.keys(inflight).filter(function (k) { return inflight[k]; }).length }; }
    };
    if (typeof window !== 'undefined') {
        window.GgufAsyncCache = api;
    }
    if (typeof module !== 'undefined' && module.exports) {
        module.exports = api; // node-тесты без DOM
    }
})();

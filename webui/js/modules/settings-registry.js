// settings-registry.js — R305 (этап 3 рефакторинга настроек WebUI): реестр
// «горячих» настроек балансера на странице «Настройки».
//
// ЗАЧЕМ. Балансер (R278) держит реестр настроек, которые применяются БЕЗ
// рестарта: значение берётся по приоритету override → env → дефолт, живёт в
// отдельном override-файле и НЕ входит в config.json. Это 17 штук, которые
// оператор меняет чаще всего: LB_STREAM_STALL_ABORT_SEC, LB_PLACE_OVER_RELOAD,
// LB_DONOR_ALLOW_OWN_COPIES, LB_LIVE_COPY_PROBE, LB_TOOLCALL_PROSE_*,
// LB_ADMISSION_WAIT_SEC, … История болезни: `grep 'api/v1/settings' webui/**`
// давал РОВНО 0 совпадений — панель эти настройки не показывала и не меняла
// вовсе. Оператор видел только «старые» поля config.json, правил env,
// перезапускал стенд и не понимал, почему значение ни на что не влияет: его
// перекрывал override, записанный ранее, или наоборот — env перекрывал дефолт,
// а панель об этом молчала.
//
// ЧТО ЗДЕСЬ ЕСТЬ. Одна страница-строка = одна настройка реестра: имя (моно),
// описание с сервера, ПРИМЕНЁННОЕ значение (effective), бейдж источника
// (по умолчанию / из env / задано в панели), поле ввода и «Применить», рядом
// «Сбросить» (только для source=override: сброс снимает запись и возвращает
// env-значение, если оно есть), плюс пояснение, почему сброс недоступен.
//
// ГРАНИЦА ОТВЕТСТВЕННОСТИ (требование этапа 3). Состояние реестра — что
// загружено, что набрано, что ответил сервер — живёт ТОЛЬКО в этом модуле.
// `index.html` предоставляет пустой контейнер #hotSettingsPanel, `app.js` лишь
// просит модуль обновиться при переходе на страницу. Поэтому второй копии
// «какие настройки есть» нигде нет и разойтись с сервером она не может.
//
// КОНТРАКТ (internal/api/handlers_settings_r278.go, обёртка — Api.hotSettings):
//   GET  /api/v1/settings        -> {settings:[{name,type,group,description,
//                                  hotReload,default,effective,source}],count,
//                                  overrides,sourcePriority}
//   PUT  /api/v1/settings        <- {name, value}      (целое: bool=0/1,
//                                  duration_sec=секунды) → обновлённая запись
//   POST /api/v1/settings/reset  <- {name} (или {} — все) → возврат к env/дефолту
//
// ЧЕСТНОСТЬ. Старый балансер (404 на /api/v1/settings либо ответ без поля
// settings) — это НЕ «настроек нет»: блок пишет «реестр недоступен (старый
// балансер)» и объясняет, что делать. Пустой список выглядел бы как «здесь
// нечего настраивать», и оператор снова искал бы эти ключи в config.json.
//
// Тесты: webui/js/modules/settings-registry.test.js (mock DOM + mock fetch).

(function () {
    'use strict';

    var MOUNT_ID = 'hotSettingsPanel';
    var ENDPOINT = '/api/v1/settings';
    var RESET_ENDPOINT = '/api/v1/settings/reset';

    // Порядок групп в панели: сначала то, что оператор крутит чаще (стриминг и
    // окно контекста), затем очередь и внешний llama.cpp. Незнакомую группу
    // (новый ключ из будущей версии балансера) не выбрасываем — рисуем в конце.
    var GROUP_ORDER = ['streaming', 'nctx', 'queue', 'upstream'];

    // state — единственная копия состояния реестра в панели (см. шапку модуля).
    var state = {
        root: null,
        loaded: false,
        unavailable: false,
        error: '',
        settings: [],
        priority: ['override', 'env', 'default'],
        drafts: {},    // name -> сырой текст, набранный, но ещё не применённый
        messages: {},  // name -> {kind: 'ok'|'error', text} последний результат строки
        busy: {}       // name -> true, пока по строке идёт PUT/POST
    };

    // --- i18n ---------------------------------------------------------------

    function t(key, vars) {
        try {
            if (typeof window !== 'undefined' && window.I18N && typeof window.I18N.t === 'function') {
                return window.I18N.t(key, vars);
            }
            if (typeof globalThis !== 'undefined' && globalThis.I18N && typeof globalThis.I18N.t === 'function') {
                return globalThis.I18N.t(key, vars);
            }
        } catch (e) { /* i18n ещё не загружен — покажем ключ, это видно в тесте */ }
        return key;
    }

    // --- сеть ---------------------------------------------------------------
    //
    // Заголовки НЕ собираем здесь: оба меняющих запроса — админские, и повторная
    // сборка заголовков уже один раз кончилась 401 (R66d, cppworker-params).
    // Всё идёт через Api.hotSettings → getJson()/request() из api.js:
    // X-API-Token (break-glass) ставит getOptions(), X-CSRF-Token для сессионных
    // мутаций — общая обёртка window.fetch в auth.js (R209).

    function registryApi() {
        var g = (typeof globalThis !== 'undefined') ? globalThis : {};
        var A = g.Api || (typeof window !== 'undefined' ? window.Api : null);
        return (A && A.hotSettings) ? A.hotSettings : null;
    }

    function errorText(err) {
        if (!err) { return ''; }
        if (err.body && typeof err.body === 'object' && (err.body.error || err.body.message)) {
            return String(err.body.error || err.body.message);
        }
        return String(err.message || err);
    }

    // --- чистые функции (тестируются напрямую) ------------------------------

    function escapeHtml(s) {
        return String(s === undefined || s === null ? '' : s)
            .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
            .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
    }

    function normalizeType(type) {
        var s = String(type || '').toLowerCase();
        if (s === 'bool' || s === 'duration_sec') { return s; }
        return 'int';
    }

    /**
     * coerceValue — привести ввод оператора к целому числу типа настройки.
     *
     * ЗАЧЕМ: API принимает только целые (bool = 0/1, duration_sec = секунды).
     * Раньше такой проверки не было нигде, поэтому опечатка уходила на сервер и
     * возвращалась общим «invalid json»/400 — оператор не понимал, что не так с
     * «30s» вместо «30». Здесь мусор отсекается ДО запроса, с понятной причиной.
     *
     * @returns {{ok: true, value: number}|{ok: false, key: string}}
     */
    function coerceValue(type, raw) {
        var s = String(raw === undefined || raw === null ? '' : raw).trim();
        if (s === '') { return { ok: false, key: 'settings.hot.error_empty' }; }
        if (!/^-?\d+$/.test(s)) { return { ok: false, key: 'settings.hot.error_not_int' }; }
        var n = parseInt(s, 10);
        if (!isFinite(n)) { return { ok: false, key: 'settings.hot.error_not_int' }; }
        if (normalizeType(type) === 'bool' && n !== 0 && n !== 1) {
            return { ok: false, key: 'settings.hot.error_bool' };
        }
        return { ok: true, value: n };
    }

    function groupTitle(group) {
        // Литеральные t('...') — так линт i18n_keys_lint_test.go видит эти ключи:
        // вычисленный t('settings.hot.group.' + group) он пропустил бы целиком.
        switch (group) {
            case 'streaming': return t('settings.hot.group.streaming');
            case 'nctx': return t('settings.hot.group.nctx');
            case 'queue': return t('settings.hot.group.queue');
            case 'upstream': return t('settings.hot.group.upstream');
            default: return t('settings.hot.group.other');
        }
    }

    function groupIcon(group) {
        switch (group) {
            case 'streaming': return 'fas fa-water';
            case 'nctx': return 'fas fa-window-maximize';
            case 'queue': return 'fas fa-hourglass-half';
            case 'upstream': return 'fas fa-plug';
            default: return 'fas fa-sliders-h';
        }
    }

    function sourceLabel(source) {
        if (source === 'override') { return t('settings.hot.source.override'); }
        if (source === 'env') { return t('settings.hot.source.env'); }
        return t('settings.hot.source.default');
    }

    function badgeClass(source) {
        // Классы бейджей — штатные из components.css (.badge + .badge-*).
        // override → success (действует выбор оператора), env → warning (значение
        // пришло не из панели и вернётся после сброса), default → info.
        if (source === 'override') { return 'badge badge-success'; }
        if (source === 'env') { return 'badge badge-warning'; }
        return 'badge badge-info';
    }

    function unitLabel(type) {
        return normalizeType(type) === 'duration_sec' ? t('settings.hot.sec') : '';
    }

    function effectiveText(item) {
        if (normalizeType(item.type) === 'bool') {
            return String(item.effective) + ' (' + (item.effective ? t('settings.hot.bool_on') : t('settings.hot.bool_off')) + ')';
        }
        var unit = unitLabel(item.type);
        return unit ? (String(item.effective) + ' ' + unit) : String(item.effective);
    }

    function hintFor(item) {
        var parts = [];
        if (item.source === 'env') {
            // Ровно та подсказка, которой не хватало: env никуда не исчезает,
            // он снова начнёт действовать после «Сбросить».
            parts.push(t('settings.hot.env_hint'));
        } else if (item.source === 'override') {
            parts.push(t('settings.hot.override_hint'));
        }
        if (item.source !== 'default') {
            var def = String(item.default);
            var unit = unitLabel(item.type);
            parts.push(t('settings.hot.default_line', { value: unit ? (def + ' ' + unit) : def }));
        }
        return parts.join(' ');
    }

    // --- разметка -----------------------------------------------------------

    function renderControl(item) {
        var name = escapeHtml(item.name);
        var type = normalizeType(item.type);
        var draft = state.drafts[item.name];
        if (type === 'bool') {
            // Переключатель — именно select 0/1, а не checkbox: реестр принимает
            // числа, и «Применить» обязан отправить 0 или 1, а не true/false.
            var cur = (draft !== undefined) ? String(draft) : String(item.effective ? 1 : 0);
            var off = (cur === '0') ? ' selected' : '';
            var on = (cur === '1') ? ' selected' : '';
            return '<select class="form-control" data-hot-input="' + name + '" data-admin-only ' +
                'aria-label="' + name + '" title="' + name + '">' +
                '<option value="0"' + off + '>' + escapeHtml(t('settings.hot.bool_off')) + '</option>' +
                '<option value="1"' + on + '>' + escapeHtml(t('settings.hot.bool_on')) + '</option>' +
                '</select>';
        }
        var value = (draft !== undefined) ? draft : String(item.effective);
        return '<input type="number" step="1" class="form-control" data-hot-input="' + name + '" ' +
            'data-admin-only aria-label="' + name + '" title="' + name + '" value="' + escapeHtml(value) + '">';
    }

    function renderRow(item) {
        var name = escapeHtml(item.name);
        var msg = state.messages[item.name];
        // busy — строка ждёт ответа сервера: кнопки гасим, иначе двойной клик
        // отправит второй PUT и оператор увидит противоречивые ответы.
        var busy = !!state.busy[item.name];
        var msgHtml = msg
            ? '<span class="hot-setting-msg ' + (msg.kind === 'ok' ? 'is-ok' : (msg.kind === 'error' ? 'is-error' : '')) + '">' +
              escapeHtml(msg.text) + '</span>'
            : '<span class="hot-setting-msg"></span>';
        var canReset = item.source === 'override';
        // «Сбросить» намеренно БЕЗ data-admin-only: auth.js у администратора
        // снимает disabled со всех data-admin-only, и кнопка сброса стала бы
        // активной там, где сбрасывать нечего. Право решает источник значения.
        var resetBtn = '<button type="button" class="btn btn-secondary" data-hot-action="reset" ' +
            'data-hot-name="' + name + '"' + ((canReset && !busy) ? '' : ' disabled') +
            ' title="' + escapeHtml(canReset ? t('settings.hot.reset_hint') : t('settings.hot.reset_disabled')) + '">' +
            escapeHtml(t('settings.hot.reset')) + '</button>';
        var hint = hintFor(item);
        return '<div class="hot-setting-row" data-hot-row="' + name + '" data-hot-source="' + escapeHtml(item.source) + '">' +
            '<div class="hot-setting-head">' +
                '<code class="hot-setting-name">' + name + '</code>' +
                '<span class="' + badgeClass(item.source) + '" data-hot-badge="' + name + '">' +
                    escapeHtml(sourceLabel(item.source)) + '</span>' +
                '<span class="hot-setting-effective" data-hot-effective="' + name + '" title="' +
                    escapeHtml(t('settings.hot.effective')) + '">' + escapeHtml(effectiveText(item)) + '</span>' +
            '</div>' +
            '<p class="hot-setting-desc">' + escapeHtml(item.description) + '</p>' +
            '<div class="hot-setting-controls">' +
                renderControl(item) +
                (unitLabel(item.type) ? '<span class="hot-setting-unit">' + escapeHtml(unitLabel(item.type)) + '</span>' : '') +
                '<button type="button" class="btn btn-primary" data-hot-action="apply" ' +
                    'data-hot-name="' + name + '" data-admin-only' + (busy ? ' disabled' : '') + '>' +
                    escapeHtml(t('settings.hot.apply')) + '</button>' +
                resetBtn +
                msgHtml +
            '</div>' +
            (hint ? '<p class="hot-setting-hint">' + escapeHtml(hint) + '</p>' : '') +
            '</div>';
    }

    function groupItems() {
        var map = {};
        var order = [];
        state.settings.forEach(function (item) {
            var g = item.group || 'other';
            if (!map[g]) { map[g] = []; order.push(g); }
            map[g].push(item);
        });
        order.sort(function (a, b) {
            var ia = GROUP_ORDER.indexOf(a);
            var ib = GROUP_ORDER.indexOf(b);
            if (ia < 0) { ia = GROUP_ORDER.length; }
            if (ib < 0) { ib = GROUP_ORDER.length; }
            return ia - ib;
        });
        return order.map(function (g) { return { group: g, items: map[g] }; });
    }

    // renderSummaries — сводка: сколько значений задано в панели и в каком
    // порядке сервер разрешает источники (sourcePriority из ответа, не догадка).
    function renderSummaries() {
        var overrides = state.settings.filter(function (i) { return i.source === 'override'; }).length;
        var path = state.priority.map(function (s) {
            // Литеральные t('...') — см. groupTitle: их обязан видеть i18n-линт.
            if (s === 'override') { return t('settings.hot.source.override'); }
            if (s === 'env') { return t('settings.hot.source.env'); }
            return t('settings.hot.source.default');
        }).join(' → ');
        return '<p class="hot-settings-priority" data-hot-summary="1">' +
            escapeHtml(t('settings.hot.summary', {
                overrides: String(overrides),
                total: String(state.settings.length)
            })) + ' ' + escapeHtml(t('settings.hot.priority', { path: path })) + '</p>';
    }

    function renderPanel() {
        // «Загрузка…» показываем до ПЕРВОГО ответа (и во время него). После
        // успешной загрузки повторный GET не стирает строки — оператор не видит
        // мигания «пусто → строки» на каждом входе на страницу.
        if (!state.loaded && !state.unavailable && !state.error) {
            return '<div class="hot-settings-state" data-hot-state="loading">' +
                escapeHtml(t('settings.hot.loading')) + '</div>';
        }
        if (state.unavailable) {
            return '<div class="hot-settings-state hot-settings-state-warn" data-hot-state="unavailable" role="status">' +
                '<strong>' + escapeHtml(t('settings.hot.unavailable')) + '</strong>' +
                '<p>' + escapeHtml(t('settings.hot.unavailable_hint')) + '</p>' +
                '</div>';
        }
        if (state.error) {
            return '<div class="hot-settings-state hot-settings-state-error" data-hot-state="error" role="status">' +
                escapeHtml(t('settings.hot.error_load', { error: state.error })) +
                '</div>';
        }
        if (!state.settings.length) {
            return '<div class="hot-settings-state" data-hot-state="empty">' +
                escapeHtml(t('settings.hot.empty')) + '</div>';
        }
        var out = [renderSummaries()];
        groupItems().forEach(function (bucket) {
            out.push('<div class="hot-settings-group" data-hot-group="' + escapeHtml(bucket.group) + '">' +
                '<h5 class="settings-subheader">' +
                    '<i class="' + groupIcon(bucket.group) + '" aria-hidden="true"></i> ' +
                    escapeHtml(groupTitle(bucket.group)) +
                '</h5>');
            bucket.items.forEach(function (item) { out.push(renderRow(item)); });
            out.push('</div>');
        });
        return out.join('');
    }

    // render — единственная точка отрисовки: страница не рисует реестр сама.
    function render() {
        var el = state.root || containerEl();
        if (!el) { return false; }
        state.root = el;
        el.innerHTML = renderPanel();
        // R66b-reapply: секция — аккордеон с inline max-height, посчитанным по
        // высоте содержимого. Строки реестра приходят ПОСЛЕ первого расчёта, и
        // без пересчёта блок остался бы обрезанным («видно две настройки из
        // семнадцати»). Тот же приём, что app.js применяет при показе страницы.
        try {
            if (typeof window !== 'undefined' && window.SettingsUI &&
                typeof window.SettingsUI.reapplyAccordionHeights === 'function') {
                window.SettingsUI.reapplyAccordionHeights();
            }
        } catch (e) { /* высоты — не повод не показать настройки */ }
        return true;
    }

    function containerEl() {
        if (typeof document === 'undefined' || !document.getElementById) { return null; }
        return document.getElementById(MOUNT_ID);
    }

    // --- действия -----------------------------------------------------------

    function itemByName(name) {
        for (var i = 0; i < state.settings.length; i++) {
            if (state.settings[i].name === name) { return state.settings[i]; }
        }
        return null;
    }

    function findInput(name) {
        var root = state.root || containerEl();
        if (!root || typeof root.querySelector !== 'function') { return null; }
        try { return root.querySelector('[data-hot-input="' + name + '"]'); } catch (e) { return null; }
    }

    function readRawValue(name) {
        var el = findInput(name);
        if (el && typeof el.value !== 'undefined' && String(el.value) !== '') { return el.value; }
        if (state.drafts[name] !== undefined) { return state.drafts[name]; }
        var item = itemByName(name);
        return item ? String(item.effective) : '';
    }

    /**
     * applyServerEntry — обновить строку ИЗ ОТВЕТА сервера (не перезагрузкой).
     *
     * PUT отвечает не полной записью реестра, а полями результата
     * ({status,effective,source,...}), поэтому description/type/default остаются
     * прежними, а перезаписываются только effective и source — то, что сервер
     * реально применил (он мог нормализовать значение своим валидатором).
     */
    function applyServerEntry(item, res) {
        if (!item || !res || typeof res !== 'object') { return; }
        if (typeof res.effective === 'number') {
            item.effective = res.effective;
        } else if (typeof res.value === 'number') {
            item.effective = res.value;
        }
        if (res.source === 'override' || res.source === 'env' || res.source === 'default') {
            item.source = res.source;
        }
    }

    /**
     * apply(name, rawValue) — «Применить»: проверка ввода и PUT /api/v1/settings.
     *
     * rawValue передаётся тестами напрямую; в браузере берётся из поля строки.
     * @returns {Promise<{ok: boolean, reason?: string, response?: Object, error?: Error}>}
     */
    function apply(name, rawValue) {
        var item = itemByName(name);
        if (!item) { return Promise.resolve({ ok: false, reason: 'unknown' }); }
        var raw = (rawValue === undefined) ? readRawValue(name) : rawValue;
        var parsed = coerceValue(item.type, raw);
        if (!parsed.ok) {
            // Мусор НЕ уходит на сервер: смысл проверки — не получить общий
            // «HTTP 400» на опечатку, а показать причину рядом со строкой.
            state.messages[name] = { kind: 'error', text: t(parsed.key) };
            render();
            return Promise.resolve({ ok: false, reason: 'invalid' });
        }
        var api = registryApi();
        if (!api) {
            // api.js без Api.hotSettings — реестр менять нечем: честное
            // «недоступен», а не молчание кнопки.
            showUnavailable();
            return Promise.resolve({ ok: false, reason: 'unavailable' });
        }
        state.busy[name] = true;
        state.messages[name] = { kind: 'pending', text: t('settings.hot.applying') };
        render();
        return api.set(name, parsed.value).then(function (res) {
            delete state.busy[name];
            delete state.drafts[name];
            applyServerEntry(item, res);
            state.messages[name] = { kind: 'ok', text: t('settings.hot.applied') };
            render();
            return { ok: true, response: res };
        }).catch(function (err) {
            delete state.busy[name];
            state.messages[name] = { kind: 'error', text: t('settings.hot.error_apply', { error: errorText(err) }) };
            render();
            return { ok: false, error: err };
        });
    }

    /**
     * reset(name) — «Сбросить»: POST /api/v1/settings/reset, затем ПЕРЕЧИТАТЬ
     * реестр. Значение после сброса считает сервер (env, если он есть, иначе
     * дефолт), поэтому брать его из PUT-подобного ответа нельзя — только из
     * следующего GET.
     */
    function reset(name) {
        var api = registryApi();
        var item = itemByName(name);
        if (!item) { return Promise.resolve({ ok: false, reason: 'unknown' }); }
        if (item.source !== 'override') { return Promise.resolve({ ok: false, reason: 'not-override' }); }
        if (!api) {
            showUnavailable();
            return Promise.resolve({ ok: false, reason: 'unavailable' });
        }
        state.busy[name] = true;
        state.messages[name] = { kind: 'pending', text: t('settings.hot.applying') };
        render();
        return api.reset(name).then(function () {
            delete state.busy[name];
            delete state.drafts[name];
            // Значение после сброса считает СЕРВЕР, поэтому сначала перечитываем
            // реестр (load() чистит старые сообщения), и только потом пишем в
            // строку «сброшено» — иначе оператор не увидит подтверждения.
            return load().then(function () {
                state.messages[name] = { kind: 'ok', text: t('settings.hot.reset_done') };
                render();
                return { ok: true };
            });
        }).catch(function (err) {
            delete state.busy[name];
            state.messages[name] = { kind: 'error', text: t('settings.hot.error_apply', { error: errorText(err) }) };
            render();
            return { ok: false, error: err };
        });
    }

    // --- загрузка -----------------------------------------------------------

    function normalizeEntry(e) {
        var src = (e && e.source === 'override') ? 'override' : ((e && e.source === 'env') ? 'env' : 'default');
        return {
            name: String((e && e.name) || ''),
            type: normalizeType(e && e.type),
            group: String((e && e.group) || 'other'),
            description: String((e && e.description) || ''),
            hotReload: !(e && e.hotReload === false),
            default: Number((e && e.default) || 0),
            effective: Number((e && e.effective) || 0),
            source: src
        };
    }

    function showUnavailable() {
        state.loaded = true;
        state.unavailable = true;
        state.error = '';
        state.settings = [];
        render();
        return null;
    }

    function load() {
        var api = registryApi();
        if (!api) { return Promise.resolve(showUnavailable()); }
        // Первая загрузка рисует «Загрузка…» (см. renderPanel). Перезагрузка —
        // без затирания строк, чтобы панель не мигала на каждом входе.
        if (!state.loaded) { render(); }
        return api.list().then(function (data) {
            // Нет массива settings — это не «пустой реестр», а старый балансер
            // (или чужая реализация эндпоинта). Говорим правду, а не рисуем 0 строк.
            if (!data || !Array.isArray(data.settings)) { return showUnavailable(); }
            state.unavailable = false;
            state.error = '';
            state.loaded = true;
            state.priority = Array.isArray(data.sourcePriority) && data.sourcePriority.length
                ? data.sourcePriority.slice()
                : ['override', 'env', 'default'];
            state.settings = data.settings.map(normalizeEntry);
            // Перечитывание реестра — повод забыть прошлые результаты строк
            // («применено» десятиминутной давности рядом с новым значением
            // сбивает с толку). Подтверждение reset ставится уже ПОСЛЕ load().
            state.messages = {};
            render();
            return state.settings;
        }).catch(function (err) {
            // 404/501 — эндпоинта нет вовсе (старый образ балансера). Это не
            // сетевая авария: подсказка про «реестр недоступен» точнее.
            if (err && (err.status === 404 || err.status === 501)) { return showUnavailable(); }
            state.loaded = false;
            state.unavailable = false;
            state.error = errorText(err);
            render();
            return null;
        });
    }

    // --- обработчики --------------------------------------------------------

    function closestAction(el) {
        if (!el) { return null; }
        if (typeof el.closest === 'function') {
            try { return el.closest('[data-hot-action]'); } catch (e) { return null; }
        }
        var node = el;
        while (node) {
            if (typeof node.getAttribute === 'function' && node.getAttribute('data-hot-action')) { return node; }
            node = node.parentNode || null;
        }
        return null;
    }

    function onClick(ev) {
        var btn = closestAction(ev && ev.target);
        if (!btn) { return; }
        var action = btn.getAttribute('data-hot-action');
        var name = btn.getAttribute('data-hot-name');
        if (!name) { return; }
        if (action === 'apply') { apply(name); }
        else if (action === 'reset') { reset(name); }
    }

    function onInput(ev) {
        var target = ev && ev.target;
        if (!target || typeof target.getAttribute !== 'function') { return; }
        var name = target.getAttribute('data-hot-input');
        if (!name) { return; }
        // Черновик: перерисовка после ответа сервера не должна стирать то, что
        // оператор набрал в ДРУГИХ строках, но ещё не применил.
        state.drafts[name] = target.value;
    }

    /**
     * mount(root) — привязать панель к контейнеру #hotSettingsPanel.
     *
     * Обработчики вешаются НА КОНТЕЙНЕР (делегирование): строки перерисовываются
     * целиком после каждого ответа, и per-button-обработчики терялись бы вместе
     * со старым HTML — «нажми, а оно молчит» на второй попытке.
     */
    function mount(root) {
        var el = (root && typeof root === 'object') ? root : containerEl();
        if (!el) { return false; }
        state.root = el;
        if (typeof el.addEventListener === 'function' && !el.__hotSettingsBound) {
            el.addEventListener('click', onClick);
            el.addEventListener('input', onInput);
            el.addEventListener('change', onInput);
            el.__hotSettingsBound = true;
        }
        render();
        return true;
    }

    function init() {
        if (!mount()) { return; }
        // Ждём разрешения входа (R223). До этого неизвестно, отдаст ли сервер
        // реестр: у гостя запрос получит 401, и блок показал бы ошибку под
        // экраном входа. Тот же приём уже применён в layout-layers-info.js и
        // users-admin.js — не заводим третий способ ждать сессию.
        if (typeof window !== 'undefined' && typeof window.__authReady === 'function') {
            window.__authReady().then(load).catch(load);
        } else {
            load();
        }
    }

    if (typeof document !== 'undefined' && typeof document.getElementById === 'function') {
        // Ждём DOMContentLoaded и при readyState 'interactive': deferred-скрипты
        // выполняются ДО этого события, то есть раньше, чем auth.js успевает
        // обернуть fetch и определить состояние входа.
        if (document.readyState === 'loading' || document.readyState === 'interactive') {
            document.addEventListener('DOMContentLoaded', init);
        } else {
            init();
        }
    }

    // Смена языка перерисовывает панель из уже загруженного состояния — сеть не
    // нужна (строки собраны на сервере, локализуются подписи в модуле).
    if (typeof window !== 'undefined' && typeof window.addEventListener === 'function') {
        window.addEventListener('i18n:changed', function () {
            if (state.loaded) { render(); }
        });
    }

    var SettingsRegistry = {
        mount: mount,
        load: load,
        apply: apply,
        reset: reset,
        render: render,
        renderPanel: renderPanel,
        coerceValue: coerceValue,
        groupTitle: groupTitle,
        sourceLabel: sourceLabel,
        effectiveText: effectiveText,
        badgeClass: badgeClass,
        endpoint: ENDPOINT,
        resetEndpoint: RESET_ENDPOINT,
        state: state
    };
    try { window.SettingsRegistry = SettingsRegistry; } catch (e) { /* нет window (тесты) */ }
})();

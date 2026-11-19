/**
 * reference-page-helpers.js — R642 (2026-11-19): состояние страницы «Справка и
 * шаблоны» и её чистые помощники (i18n/DOM/API-обёртки, форматирование, отбор
 * рекомендаций, рендер строк списка).
 *
 * ЗАЧЕМ ВЫНЕСЕНО. `reference-page.js` держал 2246 строк: состояние страницы,
 * помощники, рендер обеих вкладок, редакторы и применение шаблона. Помощники
 * покрыты отдельным набором (`reference-page.test.js`) и меняются редко; держать
 * их вперемешку с редактором — значит искать «как форматируется параметр»
 * прокруткой 2000 строк.
 *
 * КАК СВЯЗАНО. Файл обязан грузиться ПЕРЕД `reference-page.js` (`index.html`):
 * тот берёт `window.ReferencePageHelpers`, раздаёт имена локальными ссылками и
 * работает с ТЕМ ЖЕ объектом `state` (он экспортируется по ссылке, а не копией —
 * переприсваивания `state = …` в модуле нет, только правка полей).
 *
 * ПОВЕДЕНИЕ НЕ МЕНЯЛОСЬ: код перенесён как есть.
 */
(function () {
    'use strict';
    // =====================================================================
    // Состояние — единственный владелец данных страницы
    // =====================================================================

    var state = {
        mounted: false,
        root: null,
        // Активная вкладка: 'recommendations' | 'templates'.
        tab: 'recommendations',
        // Один флаг «данные уже пришли» — от него зависит первый рендер.
        loaded: false,
        loading: false,
        // Ошибка последней загрузки/действия (Error как есть — текст собирает
        // errorText, чтобы коды сервера не превращались в «что-то пошло не так»).
        error: null,
        schemaVersion: 0,
        recommendations: [],
        templates: [],
        // Фильтр списка рекомендаций: поиск по тексту + «применимо к модели».
        filter: { search: '', model: '' },
        // Загруженные модели кластера (GET /api/v1/cluster/models/loaded):
        // единственный честный источник привязки «модель → бэкенды».
        loadedModels: [],
        loadedByModel: {},
        // R516: каталог файлов узла (GET /api/v1/cppworker/model-catalog):
        // {backendId, files:[{name,sizeBytes}]}. backendId обязателен - без него
        // страница не имеет права предлагать узел «где файл лежит на диске».
        catalog: { backendId: '', files: [] },
        // Состояние вкладки «Шаблоны»: какой шаблон раскрыт и что выбрано.
        tpl: {
            id: '',
            model: '',
            backendId: '',
            mode: 'profile+reload',
            confirm: false,
            preview: null,
            error: null,
            busy: false,
            // R515: `usage.usedByProfiles` из GET …/templates/{id} — подсказка
            // «у каких профилей значения уже совпадают с шаблоном».
            usedByProfiles: []
        },
        // Открытый редактор рекомендации (null = закрыт).
        editor: null,
        // R515: открытый редактор шаблона (null = закрыт).
        tplEditor: null
    };

    // =====================================================================
    // Мелкие помощники: i18n, DOM, API
    // =====================================================================

    function t(key, fallback, vars) {
        var api = window.I18N;
        if (api && typeof api.t === 'function') {
            var value;
            try {
                value = vars ? api.t(key, vars) : api.t(key);
            } catch (e) {
                value = undefined;
            }
            if (value && value !== key) return value;
        }
        var text = String(fallback === undefined || fallback === null ? key : fallback);
        if (vars) {
            text = text.replace(/\{(\w+)\}/g, function (m, name) {
                return Object.prototype.hasOwnProperty.call(vars, name) ? String(vars[name]) : m;
            });
        }
        return text;
    }

    /**
     * esc — экранирование значения для вставки в HTML.
     *
     * Данные справочника редактирует человек (заголовок, комментарий, пути к
     * докам), поэтому «просто вставить» их в разметку нельзя. Используем общий
     * Utils.escapeHtml, если модуль загружен, и локальный дубль-минимум — в
     * тестах и при частичной загрузке страницы.
     */
    function esc(value) {
        if (value === undefined || value === null) return '';
        if (window.Utils && typeof window.Utils.escapeHtml === 'function') {
            try { return window.Utils.escapeHtml(String(value)); } catch (e) { /* падать из-за esc нельзя */ }
        }
        return String(value)
            .replace(/&/g, '&amp;')
            .replace(/</g, '&lt;')
            .replace(/>/g, '&gt;')
            .replace(/"/g, '&quot;')
            .replace(/'/g, '&#39;');
    }

    function byId(id) {
        if (typeof document === 'undefined' || !document.getElementById) return null;
        return document.getElementById(id);
    }

    /** api — обёртка HTTP берётся В МОМЕНТ ВЫЗОВА (порядок скриптов не важен). */
    function api() {
        return (window.Api && window.Api.reference) || null;
    }

    function modelsApi() {
        return (window.Api && window.Api.clusterModels) || null;
    }

    /**
     * paramSpecs — ЕДИНСТВЕННЫЙ реестр метаданных параметров профиля.
     *
     * Берём его из gguf-renderer-detail-render.js (`MODEL_PARAM_SPECS`): там
     * `field`/`type`/`min`/`max`/`step`/`options`/`label`. Своей таблицы полей
     * на этой странице быть не должно (design §4.3, §7 п.10) — иначе границы и
     * подписи разъедутся с карточкой модели.
     */
    function paramSpecs() {
        var gguf = window.GgufModule;
        if (gguf && Object.prototype.toString.call(gguf.MODEL_PARAM_SPECS) === '[object Array]') {
            return gguf.MODEL_PARAM_SPECS;
        }
        return [];
    }

    function paramSpec(name) {
        var specs = paramSpecs();
        for (var i = 0; i < specs.length; i++) {
            if (specs[i] && specs[i].field === name) return specs[i];
        }
        return null;
    }

    function specOrder(name) {
        var specs = paramSpecs();
        for (var i = 0; i < specs.length; i++) {
            if (specs[i] && specs[i].field === name) return i;
        }
        return 999;
    }

    function specLabel(spec) {
        if (!spec) return '';
        return spec.label ? t(spec.label, spec.field) : spec.field;
    }

    // =====================================================================
    // Чистые помощники (их проверяет reference-page.test.js)
    // =====================================================================

    /** formatParamValue — значение параметра для человека. */
    function formatParamValue(value) {
        if (value === undefined || value === null || value === '') return '—';
        if (typeof value === 'boolean') {
            return value ? t('reference.value.yes', 'да') : t('reference.value.no', 'нет');
        }
        if (Object.prototype.toString.call(value) === '[object Array]') {
            return value.length ? value.join(', ') : '—';
        }
        return String(value);
    }

    /** formatDiffValue — значение в diff; отсутствие значения честно «—». */
    function formatDiffValue(value) {
        return formatParamValue(value);
    }

    /** formatDiffRow — строка diff: «contextLength: 16384 → 32768». */
    function formatDiffRow(entry) {
        if (!entry) return '';
        return String(entry.field || '') + ': ' +
            formatDiffValue(entry.current) + ' → ' + formatDiffValue(entry.next);
    }

    /** renderDiff — таблица diff из ответа preview (только name/value, §6.5). */
    function renderDiff(diff) {
        var list = Object.prototype.toString.call(diff) === '[object Array]' ? diff : [];
        if (!list.length) {
            return '<div class="reference-empty">' +
                esc(t('reference.preview.diff_empty', 'Отличий от действующего профиля нет')) + '</div>';
        }
        return '<ul class="reference-diff">' + list.map(function (entry) {
            var changed = JSON.stringify(entry.current) !== JSON.stringify(entry.next);
            return '<li class="' + (changed ? 'is-changed' : '') + '">' +
                esc(formatDiffRow(entry)) + '</li>';
        }).join('') + '</ul>';
    }

    /**
     * globMatch — glob по имени модели для подбора/подсветки.
     *
     * Единственный метасимвол — `*` (design §7 п.7: regex в appliesTo не
     * вводим, иначе появляются необъяснимые «почему показали/не показали»).
     * Без звёздочек сравнение подстрочное — так «Qwen3-4B» находит
     * «Qwen3-4B-Instruct-2507-Q4_K_M».
     */
    function globMatch(pattern, text) {
        var raw = String(pattern === undefined || pattern === null ? '' : pattern).trim().toLowerCase();
        var value = String(text === undefined || text === null ? '' : text).trim().toLowerCase();
        if (!raw) return true;
        if (raw.indexOf('*') === -1) return value.indexOf(raw) >= 0;
        var source = raw.replace(/[.+?^${}()|[\]\\]/g, '\\$&').replace(/\*/g, '.*');
        try {
            return new RegExp('^' + source + '$').test(value);
        } catch (e) {
            return value.indexOf(raw.replace(/\*/g, '')) >= 0;
        }
    }

    /**
     * matchRecommendation — подходит ли запись под фильтр.
     *
     * Поиск — подстрока по заголовку, обоснованию, id, именам/комментариям
     * параметров и путям источников. Модель — `appliesTo.modelPattern`.
     * Запись БЕЗ `appliesTo` — «общая справка» (решение человека №1): она
     * применима к любой модели, поэтому под фильтром по модели остаётся.
     */
    function matchRecommendation(rec, filter) {
        if (!rec) return false;
        var f = filter || {};
        var search = String(f.search || '').trim().toLowerCase();
        if (search) {
            var haystack = [rec.id, rec.title, rec.comment];
            (rec.params || []).forEach(function (p) {
                haystack.push(p && p.name, p && p.comment, p && p.unit);
            });
            (rec.sources || []).forEach(function (s) {
                haystack.push(s && s.doc, s && s.section);
            });
            if (haystack.join(' ').toLowerCase().indexOf(search) === -1) return false;
        }
        var model = String(f.model || '').trim();
        if (model) {
            if (!rec.appliesTo) return true;
            return globMatch(rec.appliesTo.modelPattern, model);
        }
        return true;
    }

    function filterRecommendations(list, filter) {
        var items = Object.prototype.toString.call(list) === '[object Array]' ? list : [];
        return items.filter(function (rec) { return matchRecommendation(rec, filter); });
    }

    /**
     * appliesToChips — «к чему применима запись» человеческим языком.
     * Пустой массив = общая справка (appliesTo не задан).
     */
    function appliesToChips(appliesTo) {
        if (!appliesTo) return [];
        var chips = [];
        if (appliesTo.modelPattern) chips.push(t('reference.applies.model', 'модель: {value}', { value: appliesTo.modelPattern }));
        if (appliesTo.backendType) chips.push(t('reference.applies.backend', 'бэкенд: {value}', { value: appliesTo.backendType }));
        if (appliesTo.quantization) chips.push(t('reference.applies.quant', 'квант: {value}', { value: appliesTo.quantization }));
        if (appliesTo.vramMinMB) chips.push(t('reference.applies.vram_min', 'VRAM ≥ {value} МБ', { value: appliesTo.vramMinMB }));
        if (appliesTo.vramMaxMB) chips.push(t('reference.applies.vram_max', 'VRAM ≤ {value} МБ', { value: appliesTo.vramMaxMB }));
        return chips;
    }

    /**
     * loadedTargetsFor — бэкенды, где загружена модель (из cluster/models/loaded).
     *
     * Это единственный честный источник привязки модель→бэкенд (design §3.5):
     * `GET /api/v1/models` для llama.cpp отдаёт `models:null`. Сравнение имён
     * нечувствительно к регистру и к суффиксу `.gguf` — у сервера на границе
     * профилей та же нормализация.
     */
    function loadedTargetsFor(loadedModels, model, catalog) {
        var wanted = normalizeModelName(model);
        var out = [];
        if (!wanted) return out;
        (Object.prototype.toString.call(loadedModels) === '[object Array]' ? loadedModels : []).forEach(function (m) {
            if (!m || !m.backendId) return;
            if (normalizeModelName(m.name) !== wanted) return;
            if (out.some(function (x) { return x.backendId === m.backendId; })) return;
            out.push({
                backendId: String(m.backendId),
                name: String(m.name || ''),
                backendType: String(m.backendType || ''),
                loaded: true,
                contextLength: Number(m.contextLength || 0),
                activeQueries: (m.activeQueries === undefined ? null : m.activeQueries)
            });
        });
        // R516 (§2.7 разбора): цель «файл есть на диске, модель не загружена».
        //
        // ЖИВАЯ ЖАЛОБА: «селектор „где поднять" ждёт бэкенд с загруженной
        // моделью». Сервер для применения шаблона требует лишь существования
        // бэкенда и файла (handlers_reference_apply.go:294-324,368-391), а UI
        // брал цели ТОЛЬКО из cluster/models/loaded — то есть запрещал то, что
        // сервер разрешает. Каталог `/api/v1/cppworker/model-catalog` отдаёт
        // файлы папки моделей и (с R516) имя узла, у которого они прочитаны.
        //
        // Узел НЕ выдумываем: если каталог не назвал backendId, дополнительных
        // целей не появляется — иначе оператор выбрал бы узел, где файла нет.
        var cat = catalog || {};
        var catBackend = String(cat.backendId || '');
        var files = Object.prototype.toString.call(cat.files) === '[object Array]' ? cat.files : [];
        if (catBackend && !out.some(function (x) { return x.backendId === catBackend; })) {
            var hasFile = files.some(function (f) {
                return f && normalizeModelName(f.name) === wanted;
            });
            if (hasFile) {
                out.push({
                    backendId: catBackend,
                    name: String(model || ''),
                    backendType: 'llama_cpp',
                    loaded: false,
                    fromCatalog: true,
                    contextLength: 0,
                    activeQueries: null
                });
            }
        }
        return out;
    }

    function normalizeModelName(name) {
        return String(name === undefined || name === null ? '' : name)
            .trim()
            .replace(/\.gguf$/i, '')
            .toLowerCase();
    }

    /**
     * applyGate — можно ли применять шаблон прямо сейчас.
     *
     * Правила (design §4.2 + решение человека №3):
     *   * модель обязательна — без неё считать diff и цели не из чего;
     *   * если загруженных целей нет — применять НЕКУДА, кнопка неактивна в
     *     ЛЮБОМ режиме (требование оператора: «не быть активной, если в targets
     *     нет ни одной загруженной цели — иначе нажмёшь и получишь „некуда“»);
     *   * mode="profile-only" бэкенд НЕ требует (reload не запускается);
     *   * mode="profile+reload" требует явный бэкенд И этот бэкенд обязан быть
     *     среди загруженных целей: сервер отвечает 400 backend_required, а
     *     «применить на все узлы» задевает чужие/недоступные.
     */
    function applyGate(opts) {
        var o = opts || {};
        var mode = o.mode === 'profile-only' ? 'profile-only' : 'profile+reload';
        var model = String(o.model || '').trim();
        var targets = Object.prototype.toString.call(o.targets) === '[object Array]' ? o.targets : [];
        if (!model) return { disabled: true, reason: 'model_required' };
        // R516: режим «только профиль» бэкенда не требует ВООБЩЕ — reload не
        // запускается, сервер пишет профиль по имени модели. Раньше здесь
        // стояла общая проверка «есть загруженные цели», и profile-only был
        // заблокирован при пустом списке, хотя серверу бэкенд не нужен (§2.7).
        if (mode === 'profile-only') return { disabled: false, reason: '', warn: '' };
        if (!targets.length) return { disabled: true, reason: 'no_loaded_target' };
        var backendId = String(o.backendId || '').trim();
        if (!backendId) return { disabled: true, reason: 'backend_required' };
        var chosen = null;
        targets.forEach(function (x) {
            if (x && String(x.backendId) === backendId) chosen = x;
        });
        if (!chosen) return { disabled: true, reason: 'backend_not_loaded' };
        // Цель «файл на диске» разрешена сервером (handlers_reference_apply.go:
        // 294-324,368-391), поэтому кнопка активна, но оператор предупреждён:
        // модель будет загружаться, это минуты, а не мгновенное применение.
        if (chosen.loaded === false) return { disabled: false, reason: '', warn: 'will_load' };
        return { disabled: false, reason: '', warn: '' };
    }

    /** gateText — человеческий текст причины, почему «Применить» неактивна. */
    function gateText(reason) {
        if (reason === 'no_loaded_target') {
            return t('reference.gate.no_loaded_target',
                'Модель нигде не загружена и файла на диске нет — применять некуда. Загрузите модель на бэкенде и обновите страницу.');
        }
        if (reason === 'will_load') {
            return t('reference.gate.will_load',
                'Модель на этом узле ещё не загружена — применение начнётся с её загрузки (это минуты, а не секунды).');
        }
        if (reason === 'backend_required') {
            return t('reference.gate.backend_required',
                'Выберите бэкенд, где загружена модель: применение «на все сразу» задевает чужие и недоступные узлы.');
        }
        if (reason === 'backend_not_loaded') {
            return t('reference.gate.backend_not_loaded',
                'На выбранном бэкенде эта модель не загружена — выберите другой узел.');
        }
        if (reason === 'model_required') {
            return t('reference.gate.model_required', 'Сначала выберите модель.');
        }
        return '';
    }

    // =====================================================================
    // Ошибки: коды сервера → человеческий текст (design §3, §3.4)
    // =====================================================================

    /**
     * errorInfo — разбор ошибки HTTP в {code, status, title, hint, details}.
     *
     * ПОЧЕМУ НЕ «err.message». `Api.request` для не-2xx бросает
     * `HTTP 422: Unprocessable Entity`, а тело с настоящей причиной лежит в
     * `err.body`. Коды контракта перечислены в design §3.4 / отчёте §2.3:
     * 503 store_unavailable, 400 backend_required, 409 revision_conflict,
     * 422 profile_does_not_fit_memory, 502 reload_failed — у каждого свой текст.
     */
    function errorInfo(err) {
        var status = (err && Number(err.status)) || 0;
        var body = err && err.body;
        if (typeof body === 'string') {
            try { body = JSON.parse(body); } catch (e) { body = null; }
        }
        if (!body || typeof body !== 'object') body = null;
        var code = body && body.error ? String(body.error) : '';
        var serverMessage = body && body.message ? String(body.message) : '';
        var details = [];

        function memoryDetail(memory) {
            if (!memory || typeof memory !== 'object') return [];
            var out = [];
            Object.keys(memory).forEach(function (key) {
                var value = memory[key];
                if (value === null || typeof value === 'object') return;
                out.push(key + '=' + String(value));
            });
            return out.slice(0, 8);
        }

        if (code === 'store_unavailable') {
            return {
                code: code, status: status,
                title: t('reference.error.store_unavailable',
                    'Справочник недоступен: хранилище не инициализировано'),
                hint: t('reference.error.store_unavailable_hint',
                    'Файл model-reference.json не читается и не пишется: проверьте LB_MODEL_REFERENCE_PATH и каталог state.json (LB_STATE_PATH).'),
                details: []
            };
        }
        if (code === 'backend_required') {
            return {
                code: code, status: status,
                title: t('reference.error.backend_required',
                    'Выберите бэкенд, где загружена модель'),
                hint: t('reference.error.backend_required_hint',
                    'Применение на все узлы сразу задевает чужие и недоступные — выберите конкретный бэкенд в карточке шаблона.'),
                details: body && body.model ? ['model=' + String(body.model)] : []
            };
        }
        if (code === 'revision_conflict') {
            var conflict = [];
            if (body && body.current !== undefined) conflict.push('current=' + String(body.current));
            if (body && body.sent !== undefined) conflict.push('sent=' + String(body.sent));
            return {
                code: code, status: status,
                title: t('reference.error.revision_conflict',
                    'Запись изменена другим оператором — перечитайте её'),
                hint: t('reference.error.revision_conflict_hint',
                    'Правка не сохранена, чтобы не затереть чужую. Обновите страницу (кнопка «Обновить» в шапке) и повторите.'),
                details: conflict
            };
        }
        if (code === 'profile_does_not_fit_memory') {
            return {
                code: code, status: status,
                title: t('reference.error.not_feasible', 'Набор не помещается в память'),
                hint: serverMessage || t('reference.error.not_feasible_hint',
                    'Вердикт проверки памяти cppworker: применение отменено, чтобы не получить частичную загрузку.'),
                details: memoryDetail(body && body.memory)
            };
        }
        if (code === 'reload_failed') {
            var backends = (body && Object.prototype.toString.call(body.backends) === '[object Array]') ? body.backends : [];
            return {
                code: code, status: status,
                title: t('reference.error.reload_failed',
                    'Модель не перезагрузилась ни на одном бэкенде'),
                hint: serverMessage || t('reference.error.reload_failed_hint',
                    'Профиль сохранён, но reload не удался: смотрите сообщения по бэкендам ниже.'),
                details: backends.map(function (b) {
                    return String((b && b.backendId) || '?') + ': ' + String((b && (b.message || b.status)) || '');
                })
            };
        }
        if (code === 'recommendation_not_found' || code === 'template_not_found' || code === 'backend_not_found') {
            return {
                code: code, status: status,
                title: t('reference.error.not_found', 'Запись не найдена: возможно, её удалили'),
                hint: serverMessage || t('reference.error.not_found_hint',
                    'Обновите страницу — список мог измениться в другом браузере.'),
                details: []
            };
        }
        if (code) {
            // Остальные коды контракта (invalid_id, invalid_json, invalid_value,
            // invalid_params, unknown_param, title_required, params_required,
            // duplicate_param, model_required, invalid_mode) несут готовый
            // русский текст `message` — показываем его, а не код.
            return {
                code: code, status: status,
                title: t('reference.error.rejected', 'Сервер отклонил запрос'),
                hint: serverMessage || code,
                details: body && body.field ? ['field=' + String(body.field)] : []
            };
        }
        if (err && err.code === 'timeout') {
            return {
                code: 'timeout', status: status,
                title: t('reference.error.timeout', 'Балансер не ответил вовремя'),
                hint: err.message || '',
                details: []
            };
        }
        return {
            code: code || 'network', status: status,
            title: t('reference.error.generic', 'Не удалось выполнить запрос'),
            hint: serverMessage || (err && err.message) || (status ? 'HTTP ' + status : ''),
            details: []
        };
    }

    /** errorText — одна строка из errorInfo (для индикатора и тостов). */
    function errorText(err) {
        var info = errorInfo(err);
        var parts = [info.title];
        if (info.hint) parts.push(info.hint);
        return parts.join(' — ');
    }

    function errorHtml(err) {
        var info = errorInfo(err);
        var html = '<div class="reference-error-title">' + esc(info.title) + '</div>';
        if (info.hint) html += '<div class="reference-error-hint">' + esc(info.hint) + '</div>';
        if (info.details && info.details.length) {
            html += '<ul class="reference-error-details">' + info.details.map(function (d) {
                return '<li>' + esc(d) + '</li>';
            }).join('') + '</ul>';
        }
        return html;
    }

    // =====================================================================
    // Рендер: рекомендации
    // =====================================================================

    function paramRows(params) {
        var rows = [];
        if (Object.prototype.toString.call(params) === '[object Array]') {
            params.forEach(function (p) {
                if (!p || !p.name) return;
                rows.push({
                    name: String(p.name),
                    value: p.value,
                    unit: p.unit || '',
                    comment: p.comment || ''
                });
            });
            return rows;
        }
        if (params && typeof params === 'object') {
            Object.keys(params).forEach(function (name) {
                var spec = paramSpec(name);
                rows.push({
                    name: name,
                    value: params[name],
                    unit: (spec && spec.unit) || '',
                    comment: ''
                });
            });
            rows.sort(function (a, b) { return specOrder(a.name) - specOrder(b.name); });
        }
        return rows;
    }

    /**
     * renderParamsTable — таблица параметров записи ИЛИ шаблона.
     *
     * Принимает и массив `params[]` рекомендации, и плоский объект шаблона:
     * второй структуры «набор параметров» в проекте нет (design §2.4).
     */
    function renderParamsTable(params) {
        var rows = paramRows(params);
        if (!rows.length) {
            return '<div class="reference-empty">' +
                esc(t('reference.params.empty', 'Параметры не заданы')) + '</div>';
        }
        var head = '<thead><tr>' +
            '<th>' + esc(t('reference.col.param', 'Параметр')) + '</th>' +
            '<th>' + esc(t('reference.col.value', 'Значение')) + '</th>' +
            '<th>' + esc(t('reference.col.unit', 'Единица')) + '</th>' +
            '<th>' + esc(t('reference.col.comment', 'Комментарий')) + '</th>' +
            '</tr></thead>';
        var body = rows.map(function (row) {
            var spec = paramSpec(row.name);
            var label = spec ? specLabel(spec) : row.name;
            return '<tr data-ref-param="' + esc(row.name) + '">' +
                '<td><code>' + esc(row.name) + '</code>' +
                (label && label !== row.name ? '<span class="reference-param-label"> ' + esc(label) + '</span>' : '') +
                '</td>' +
                '<td>' + esc(formatParamValue(row.value)) + '</td>' +
                '<td>' + esc(row.unit) + '</td>' +
                '<td>' + esc(row.comment) + '</td>' +
                '</tr>';
        }).join('');
        return '<table class="data-table reference-params">' + head + '<tbody>' + body + '</tbody></table>';
    }

    function sourcesHtml(sources) {
        var list = Object.prototype.toString.call(sources) === '[object Array]' ? sources : [];
        if (!list.length) {
            return '<span class="reference-muted">' +
                esc(t('reference.rec.no_sources', 'источники не указаны')) + '</span>';
        }
        return list.map(function (src) {
            if (!src || !src.doc) return '';
            var label = src.doc + (src.section ? ' ' + src.section : '');
            return '<span class="badge badge-info reference-source" data-ref-source="' + esc(src.doc) + '">' +
                esc(label) + '</span>';
        }).join(' ');
    }

    function recMetaHtml(rec) {
        var bits = [];
        if (rec.author) bits.push(t('reference.rec.author', 'автор: {value}', { value: rec.author }));
        if (rec.createdAt) bits.push(t('reference.rec.created', 'создано: {value}', { value: rec.createdAt }));
        if (rec.updatedAt && rec.updatedAt !== rec.createdAt) {
            bits.push(t('reference.rec.updated', 'изменено: {value}', { value: rec.updatedAt }));
        }
        if (rec.revision) bits.push(t('reference.rec.revision', 'revision {value}', { value: rec.revision }));
        return bits.map(function (b) {
            return '<span class="reference-meta-item">' + esc(b) + '</span>';
        }).join('');
    }

    /**
     * renderRecommendationRow — карточка записи-рекомендации.
     *
     * Показываем ВСЁ, что оператор просил видеть: заголовок, «к чему
     * применимо», таблицу параметров с построчными комментариями, обоснование,
     * источники, автора/дату/revision. Мутирующие кнопки помечены
     * `data-admin-only` (сервер всё равно проверит права).
     */
    function renderRecommendationRow(rec) {
        if (!rec) return '';
        var id = String(rec.id || '');
        var chips = appliesToChips(rec.appliesTo);
        var appliesHtml = chips.length
            ? chips.map(function (chip) { return '<span class="badge reference-chip">' + esc(chip) + '</span>'; }).join(' ')
            : '<span class="badge reference-chip reference-chip-general">' +
                esc(t('reference.applies.general', 'общая справка')) + '</span>';
        return '<div class="reference-card" data-ref-rec="' + esc(id) + '">' +
            '<div class="reference-card-head">' +
                '<div class="reference-card-headline">' +
                    '<h4 class="reference-card-title">' + esc(rec.title || id) + '</h4>' +
                    '<div class="reference-card-badges"><code class="reference-id">' + esc(id) + '</code> ' + appliesHtml + '</div>' +
                '</div>' +
                '<div class="reference-card-actions">' +
                    '<button type="button" class="btn btn-secondary btn-sm" data-admin-only data-ref-action="edit-rec" data-ref-id="' + esc(id) + '">' +
                        esc(t('reference.rec.edit', 'Изменить')) + '</button>' +
                    '<button type="button" class="btn btn-secondary btn-sm" data-admin-only data-ref-action="to-template" data-ref-id="' + esc(id) + '">' +
                        esc(t('reference.rec.to_template', 'Создать шаблон')) + '</button>' +
                    '<button type="button" class="btn btn-danger btn-sm" data-admin-only data-ref-action="delete-rec" data-ref-id="' + esc(id) + '">' +
                        esc(t('reference.rec.delete', 'Удалить')) + '</button>' +
                '</div>' +
            '</div>' +
            (rec.comment ? '<p class="reference-card-comment">' + esc(rec.comment) + '</p>' : '') +
            renderParamsTable(rec.params) +
            '<div class="reference-card-foot">' +
                '<div class="reference-sources">' + sourcesHtml(rec.sources) + '</div>' +
                '<div class="reference-meta">' + recMetaHtml(rec) + '</div>' +
            '</div>' +
            '</div>';
    }

    function renderRecommendationList() {
        var host = byId('referenceRecList');
        if (!host) return;
        if (!state.loaded && state.loading) {
            host.innerHTML = '<div class="reference-empty">' +
                esc(t('reference.loading', 'Загружаю справочник…')) + '</div>';
            return;
        }
        var visible = filterRecommendations(state.recommendations, state.filter);
        if (!visible.length) {
            var why = state.recommendations.length
                ? t('reference.rec.no_match', 'Под фильтр ничего не подходит — измените поиск или модель.')
                : t('reference.rec.empty', 'Записей пока нет. Администратор может создать первую кнопкой «Новая запись».');
            host.innerHTML = '<div class="reference-empty">' + esc(why) + '</div>';
            return;
        }
        host.innerHTML = visible.map(renderRecommendationRow).join('');
    }

    // =====================================================================
    // Рендер: шаблоны
    // =====================================================================

    function templateById(id) {
        for (var i = 0; i < state.templates.length; i++) {
            if (state.templates[i] && String(state.templates[i].id) === String(id)) return state.templates[i];
        }
        return null;
    }

    /**
     * modelChoices — из чего выбирать модель.
     *
     * Только загруженные модели: применять шаблон к модели, которой нет в
     * памяти, нечего (design §4.2). Текущий выбор добавляем, даже если модель
     * выгрузили — иначе select «терял» бы выбор и кнопка врала бы.
     */
    function modelChoices(selection) {
        var out = state.loadedModels.slice();
        // R516: модель, чей файл лежит на диске, но которая ещё не загружена,
        // тоже должна выбираться — иначе «где поднять» по-прежнему упирается в
        // «сначала загрузите модель». Сравниваем без суффикса .gguf и регистра,
        // как это делает сервер на границе профилей.
        var seen = {};
        out.forEach(function (n) { seen[normalizeModelName(n)] = true; });
        catalogModelNames().forEach(function (n) {
            var key = normalizeModelName(n);
            if (seen[key]) return;
            seen[key] = true;
            out.push(n);
        });
        var sel = String(selection || '');
        if (sel && out.indexOf(sel) === -1) out.push(sel);
        return out;
    }

    function backendChoices(model, selection) {
        var targets = loadedTargetsFor(state.loadedModels, model, state.catalog);
        var out = targets.map(function (t) { return t.backendId; });
        var sel = String(selection || '');
        if (sel && out.indexOf(sel) === -1) out.push(sel);
        return out;
    }

    /**
     * catalogModelNames — имена моделей, чьи файлы есть на диске узла каталога.
     *
     * Возвращаем имя файла БЕЗ `.gguf` и без приведения регистра: приводить его к
     * нижнему — значит показать оператору `qwen3-14b-instruct-2507` вместо
     * `Qwen3-14B-Instruct-2507`. Сравнение с загруженными идёт по нормализованной
     * форме (там регистр и суффикс не важны).
     */
    function catalogModelNames() {
        var files = Object.prototype.toString.call(state.catalog.files) === '[object Array]' ? state.catalog.files : [];
        return files.map(function (f) {
            return String((f && f.name) || '').trim().replace(/\.gguf$/i, '');
        }).filter(function (n) { return !!n; });
    }

    // R642: экспорт для reference-page.js (там — локальные ссылки на эти имена).
    window.ReferencePageHelpers = {
        state: state,
        t: t,
        esc: esc,
        byId: byId,
        api: api,
        modelsApi: modelsApi,
        paramSpecs: paramSpecs,
        paramSpec: paramSpec,
        specOrder: specOrder,
        specLabel: specLabel,
        formatParamValue: formatParamValue,
        formatDiffValue: formatDiffValue,
        formatDiffRow: formatDiffRow,
        renderDiff: renderDiff,
        globMatch: globMatch,
        matchRecommendation: matchRecommendation,
        filterRecommendations: filterRecommendations,
        appliesToChips: appliesToChips,
        loadedTargetsFor: loadedTargetsFor,
        normalizeModelName: normalizeModelName,
        applyGate: applyGate,
        gateText: gateText,
        errorInfo: errorInfo,
        errorText: errorText,
        errorHtml: errorHtml,
        paramRows: paramRows,
        renderParamsTable: renderParamsTable,
        sourcesHtml: sourcesHtml,
        recMetaHtml: recMetaHtml,
        renderRecommendationRow: renderRecommendationRow,
        renderRecommendationList: renderRecommendationList,
        templateById: templateById,
        modelChoices: modelChoices,
        backendChoices: backendChoices,
        catalogModelNames: catalogModelNames
    };
})();

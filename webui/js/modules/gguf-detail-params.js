// gguf-detail-params.js — R658 (2026-11-20): редактор параметров модели и
// связанные чистые функции вынесены из gguf-detail-settings.js (цель §5 — < 1500).
//
// ЗАЧЕМ ВЫНЕСЕНО. Это отдельная тема внутри таба Settings: спецификация параметров
// (MODEL_PARAM_SPECS), значения по умолчанию/наследование (R126/R134/R229),
// «чем ограничено окно» (R319), ввод и гидратация редакторов, применение к модели.
// Слой ВАЛИДИРУЕТ и приводит значения, а не только рисует.
//
// ПОЧЕМУ ФАБРИКА. Слой берёт M/state, Utils, PARAM_SOURCE_LABELS, MODEL_PARAM_SPECS,
// paramFact, collapseSections (список посчитан scripts/webui_extract_deps.js).
// showToast резолвится лениво — он глобальный (M.showToast/window.showToast).
//
// ПОВЕДЕНИЕ НЕ МЕНЯЛОСЬ: тела перенесены как есть, отступ сохранён.
(function () {
    'use strict';

    window.GgufDetailParams = function (ctx) {
        var M = ctx.M;
        var state = ctx.state;
        var _ = ctx._;
        var Utils = ctx.Utils;
        var PARAM_SOURCE_LABELS = ctx.PARAM_SOURCE_LABELS;
        var MODEL_PARAM_SPECS = ctx.MODEL_PARAM_SPECS;
        var paramFact = ctx.paramFact;
        var collapseSections = ctx.collapseSections;
        var showToast = function () {
            var t = ctx.showToast
                || (M && M.showToast)
                || (typeof window !== 'undefined' && window.showToast)
                || (typeof globalThis !== 'undefined' && globalThis.showToast);
            if (typeof t === 'function') return t.apply(null, arguments);
        };

    function paramOrigin(spec, m, rt) {
        var prov = (m && m.parameters) || (rt && rt.parameters) || {};
        var keys = [spec.loadKey, spec.field].concat(spec.factKeys || []);
        for (var i = 0; i < keys.length; i++) {
            var entry = prov[keys[i]];
            if (entry && typeof entry === 'object') {
                return { value: entry.value, source: entry.source || '' };
            }
        }
        return null;
    }

    // loadedParamsFor — предварительные значения формы для ЗАГРУЖЕННОЙ модели (R126/R134).
    //
    // Приоритет: происхождение (кто поставил значение) → факт воркера. Профиль
    // подгружается отдельно (hydrateModelParamEditors) и, если он есть, заменяет
    // предзаполнение: форма редактирует ПРОФИЛЬ, а не текущий инстанс.
    function loadedParamsFor(m, rt) {
        m = m || {};
        rt = rt || {};
        var out = {};
        MODEL_PARAM_SPECS.forEach(function (spec) {
            var origin = paramOrigin(spec, m, rt);
            var value = (origin && origin.value !== undefined && origin.value !== null)
                ? origin.value
                : paramFact(spec, m, rt);
            if (value !== undefined && value !== null && value !== '') out[spec.field] = value;
        });
        // Совместимость с прежним именем поля (R126 отдавал gpuLayers): форма
        // теперь читает numGpuLayers, но старые вызовы не должны терять значение.
        if (out.gpuLayers !== undefined && out.numGpuLayers === undefined) {
            out.numGpuLayers = out.gpuLayers;
        }
        return out;
    }

    // formatParamValue — человекочитаемое значение для таблицы «действующие».
    function formatParamValue(spec, value) {
        if (value === null || value === undefined || value === '') return null;
        if (spec.type === 'bool') {
            // Flash Attention у воркера ТРЁХЗНАЧНЫЙ: -1 = авто, 0 = выкл, 1 = вкл.
            // Печатать -1 как «выключено» нельзя: оператор видит выключенным то,
            // что работает в автоматическом режиме (находка R136).
            if (spec.field === 'flashAttn' && typeof value === 'number') {
                if (value < 0) return _('gguf.mp_auto', 'авто');
                return value === 0 ? _('gguf.mp_off', 'выключено') : _('gguf.mp_on', 'включено');
            }
            var on = value === true || value === 'true' || value === 1 || value === '1';
            return on ? _('gguf.mp_on', 'включено') : _('gguf.mp_off', 'выключено');
        }
        if (spec.type === 'list') {
            return Array.isArray(value) ? value.join(', ') : String(value);
        }
        return String(value);
    }

    // paramInput — элемент управления для таблицы «можно задать».
    //
    // Пустое значение = «наследуется»: в профиль пустое поле не попадёт
    // (collectModelParams), поэтому форма не может молча отключить наследование.
    //
    // R134 (доп., 2026-10-16): БУЛЕВЫ ПАРАМЕТРЫ — ТРЁХЗНАЧНЫЕ. Раньше это была
    // галочка, а collectModelParams записывал `!!checked` ВСЕГДА — то есть
    // «Применить», не тронув ни одной галочки, клал в профиль flashAttn=false,
    // useMmap=false и enableReasoning=false. Следствие ровно то, на что жаловался
    // оператор: у модели с включёнными размышлениями после открытия карточки и
    // применения профиль начинал говорить «выключено», и строка модели расходилась
    // с карточкой. Теперь у bool три состояния: наследуется / включено / выключено.
    // ===== R229 (2026-10-22): «наследуется» показывает РЕАЛЬНОЕ значение =====
    //
    // ЖИВАЯ ЖАЛОБА ОПЕРАТОРА: поле «наследуется» не говорило, ЧТО именно
    // унаследуется, и приходилось угадывать («в профиле пусто — наверное,
    // выключено»). Ответ у балансера уже есть: GET /api/v1/cppworker/load-defaults
    // отдаёт effective (что применится) и source (default_profile / env воркера).
    // Теперь метка наследования несёт фактическое значение.
    //
    // Правило преобразования — ОДНА функция на все три места показа (bool-селект,
    // enum-селект, placeholder числового поля): иначе «включено/1/on» разъедутся,
    // как это уже было с трёхзначными флажками в R134.
    function inheritedValueOf(spec, effective) {
        if (!spec || !effective) return null;
        var key = spec.defaultKey;
        if (!key || !Object.prototype.hasOwnProperty.call(effective, key)) return null;
        var raw = effective[key];
        if (raw === null || raw === undefined || raw === '') return null;
        if (spec.type === 'bool') {
            if (raw === true || raw === 1 || raw === '1' || raw === 'true') return true;
            if (raw === false || raw === -1 || raw === '0' || raw === 'false') return false;
            return null;
        }
        if (spec.type === 'int') {
            var n = Number(raw);
            return isFinite(n) ? n : null;
        }
        return String(raw);
    }

    // inheritedLabel — текст метки наследования. Если фактическое значение
    // неизвестно (воркер недоступен), остаётся честное «наследуется»: пустая
    // скобка вводила бы в заблуждение сильнее.
    function inheritedLabel(spec, effective) {
        var v = inheritedValueOf(spec, effective);
        if (v === null) return _('gguf.mp_inherit', 'наследуется');
        var shown = spec.type === 'bool'
            ? (v ? _('gguf.mp_on', 'включено') : _('gguf.mp_off', 'выключено'))
            : String(v);
        return _('gguf.mp_inherit', 'наследуется') + ' (' + shown + ')';
    }

    // applyInheritedLabels — обновить метки в уже отрисованной форме (значения по
    // умолчанию приходят отдельным запросом, после первой отрисовки).
    function applyInheritedLabels(editorEl, effective) {
        if (!editorEl || !effective || typeof editorEl.querySelector !== 'function') return;
        MODEL_PARAM_SPECS.forEach(function (spec) {
            var input = editorEl.querySelector('.gguf-mp-field[data-field="' + spec.field + '"]');
            if (!input) return;
            var text = inheritedLabel(spec, effective);
            if (input.options || input.tagName === 'SELECT') {
                var opt = (typeof input.querySelector === 'function')
                    ? input.querySelector('option[value=""]') : null;
                if (opt) opt.textContent = text;
            } else if ('placeholder' in input) {
                input.placeholder = text;
            }
        });
    }

    function paramInput(spec, idBase, value, effective) {
        var id = idBase + '_' + spec.field;
        var common = 'id="' + id + '" class="form-control form-control-sm gguf-mp-field" data-field="' + spec.field + '"';
        if (spec.type === 'bool') {
            var cur = (value === undefined || value === null || value === '') ? ''
                : ((value === true || value === 'true') ? 'true' : 'false');
            var boolOpts = [
                { v: '', t: inheritedLabel(spec, effective) },
                { v: 'true', t: _('gguf.mp_on', 'включено') },
                { v: 'false', t: _('gguf.mp_off', 'выключено') }
            ].map(function (o) {
                return '<option value="' + o.v + '"' + (o.v === cur ? ' selected' : '') + '>' +
                    Utils.escapeHtml(o.t) + '</option>';
            }).join('');
            return '<select ' + common + ' data-type="bool">' + boolOpts + '</select>';
        }
        if (spec.type === 'enum') {
            var cur = (value === undefined || value === null) ? '' : String(value);
            var opts = ['<option value=""' + (cur === '' ? ' selected' : '') + '>' +
                Utils.escapeHtml(inheritedLabel(spec, effective)) + '</option>'];
            (spec.options || []).forEach(function (o) {
                opts.push('<option value="' + Utils.escapeHtml(o) + '"' + (cur === o ? ' selected' : '') + '>' +
                    Utils.escapeHtml(o) + '</option>');
            });
            return '<select ' + common + '>' + opts.join('') + '</select>';
        }
        var v = (value === undefined || value === null) ? '' : String(value);
        if (spec.type === 'list') {
            return '<input type="text" ' + common + ' value="' + Utils.escapeHtml(v) +
                '" placeholder="' + Utils.escapeHtml(_('gguf.mp_list_placeholder', 'через запятую')) + '">';
        }
        return '<input type="number" ' + common + ' value="' + Utils.escapeHtml(v) +
            '" min="' + spec.min + '" max="' + spec.max + '" step="' + (spec.step || 1) +
            '" placeholder="' + Utils.escapeHtml(inheritedLabel(spec, effective)) + '">';
    }

    // renderModelParamsEditor — ДВЕ таблицы параметров модели (R134).
    //
    // ЖИВАЯ ЖАЛОБА ОПЕРАТОРА: «параметры разбросаны по области — неудобно, и если
    // их будет больше, воспринимать неудобно; лучше два списка-таблицы: активные и
    // отдельно те, что можно задать для загрузки/перезагрузки; и параметры должны
    // быть одинаковы и в едином стиле для моделей на диске и для загруженных».
    //
    //   Таблица 1 «Действующие значения» — ФАКТ: что реально у модели сейчас (от
    //     воркера) и кто это поставил (request / profile / default_profile /
    //     backend). Только чтение: правка идёт в профиль, а не в живой инстанс.
    //   Таблица 2 «Задать для загрузки / перезагрузки» — ПРОФИЛЬ: значения, которые
    //     применятся при следующей загрузке. Пусто = «наследуется».
    //
    // Один и тот же компонент рисуется и для модели на диске, и для загруженной:
    // разный только набор известных фактов (у модели на диске их нет).
    function renderModelParamsEditor(modelName, values, opts) {
        values = values || {};
        opts = opts || {};
        var activeSource = isLoaded;
        var safeName = String(modelName == null ? '' : modelName);
        var idBase = 'ggufMp_' + safeName.replace(/[^A-Za-z0-9_.-]/g, '_');
        var isLoaded = opts.source === 'loaded';
        var model = opts.model || {};
        var runtime = opts.runtime || {};
        // R229: значения по умолчанию («что унаследует пустое поле») — карта effective
        // из GET /api/v1/cppworker/load-defaults (обёртка defaults.effective).
        var defaultsEffective = (opts.defaults && opts.defaults.effective) || null;

        var activeSource = isLoaded;
        var rowsHtml = function (specs, active) {
            return specs.map(function (spec) {
                var label = Utils.escapeHtml(_(spec.label, spec.field));
                var hint = spec.hint
                    ? '<div class="gguf-mp-hint">' + Utils.escapeHtml(_(spec.hint, '')) + '</div>'
                    : '';
                if (active) {
                    var fact = paramFact(spec, model, runtime);
                    var origin = paramOrigin(spec, model, runtime);
                    var shown = formatParamValue(spec, fact !== null ? fact : (origin ? origin.value : null));
                    // Подписи источников — те же, что в блоке происхождения R119
                    // (PARAM_SOURCE_LABELS): один словарь на страницу, без второй копии.
                    var src = (origin && PARAM_SOURCE_LABELS[origin.source]) || _('gguf.mp_src_unknown', '—');
                    return '<tr>' +
                        '<td>' + label + hint + '</td>' +
                        '<td class="gguf-mp-val">' + (shown === null
                            ? '<span class="gguf-mp-none">' + Utils.escapeHtml(_('gguf.mp_unknown', '—')) + '</span>'
                            : Utils.escapeHtml(shown)) + '</td>' +
                        '<td class="gguf-mp-src">' + Utils.escapeHtml(src) + '</td>' +
                    '</tr>';
                }
                // ФАКТ РЯДОМ С ФОРМОЙ. Форма редактирует ПРОФИЛЬ, и до R136 оператор
                // видел только его: «в профиле не задано» читалось как «выключено»,
                // хотя у загруженной модели размышления работали. Теперь под контролом
                // видно, что действует СЕЙЧАС (источник — факт воркера).
                var nowHint = '';
                if (activeSource && spec.type !== 'list') {
                    var nowVal = formatParamValue(spec, paramFact(spec, model, runtime));
                    if (nowVal !== null) {
                        nowHint = '<div class="gguf-mp-fact">' +
                            Utils.escapeHtml(_('gguf.mp_now', 'сейчас')) + ': ' + Utils.escapeHtml(nowVal) + '</div>';
                    }
                }
                return '<tr' + (spec.advanced ? ' class="gguf-mp-adv-row"' : '') + '>' +
                    '<td>' + label + hint + '</td>' +
                    '<td class="gguf-mp-profile" data-profile-field="' + spec.field + '">' +
                        '<span class="gguf-mp-none">' + Utils.escapeHtml(_('gguf.mp_not_in_profile', '— не задано —')) + '</span>' +
                    '</td>' +
                    '<td class="gguf-mp-input">' + paramInput(spec, idBase, values[spec.field], defaultsEffective) + nowHint + '</td>' +
                '</tr>';
            }).join('');
        };

        var mainSpecs = MODEL_PARAM_SPECS.filter(function (s) { return !s.advanced; });
        var advSpecs = MODEL_PARAM_SPECS.filter(function (s) { return s.advanced; });
        var activeHint = isLoaded
            ? _('gguf.mp_active_hint_loaded', 'Что реально действует у загруженной модели и кто это поставил.')
            : _('gguf.mp_active_hint_disk', 'Модель не загружена — действующих значений нет. Ниже задайте параметры для следующей загрузки.');

        return '<div class="gguf-mp-editor" data-model="' + Utils.escapeHtml(safeName) + '" data-source="' + (isLoaded ? 'loaded' : 'disk') + '">' +
            '<div class="gguf-mp-block">' +
                '<div class="gguf-mp-title">' + Utils.escapeHtml(_('gguf.mp_table_active', 'Действующие значения')) +
                    '<span class="gguf-mp-sub">' + Utils.escapeHtml(activeHint) + '</span></div>' +
                '<table class="gguf-mp-table"><thead><tr>' +
                    '<th>' + Utils.escapeHtml(_('gguf.mp_col_param', 'Параметр')) + '</th>' +
                    '<th>' + Utils.escapeHtml(_('gguf.mp_col_value', 'Значение')) + '</th>' +
                    '<th>' + Utils.escapeHtml(_('gguf.mp_col_source', 'Источник')) + '</th>' +
                '</tr></thead><tbody>' + rowsHtml(mainSpecs, true) + '</tbody></table>' +
            '</div>' +
            '<div class="gguf-mp-block">' +
                '<div class="gguf-mp-title">' + Utils.escapeHtml(_('gguf.mp_table_set', 'Задать для загрузки / перезагрузки')) +
                    '<span class="gguf-mp-sub">' + Utils.escapeHtml(_('gguf.mp_set_hint',
                        'Пустое поле = наследуется (профиль модели → настройки по умолчанию → окружение контейнера). Применение перезагрузит модель.')) + '</span></div>' +
                '<table class="gguf-mp-table"><thead><tr>' +
                    '<th>' + Utils.escapeHtml(_('gguf.mp_col_param', 'Параметр')) + '</th>' +
                    '<th>' + Utils.escapeHtml(_('gguf.mp_col_profile', 'В профиле')) + '</th>' +
                    '<th>' + Utils.escapeHtml(_('gguf.mp_col_set', 'Задать')) + '</th>' +
                '</tr></thead><tbody>' + rowsHtml(mainSpecs, false) +
                '<tr class="gguf-mp-adv-head"><td colspan="3">' +
                    Utils.escapeHtml(_('gguf.mp_advanced', 'Дополнительно: per-tensor override для MoE-моделей')) + '</td></tr>' +
                rowsHtml(advSpecs, false) + '</tbody></table>' +
            '</div>' +
            '<div class="gguf-mp-actions">' +
                '<button class="btn btn-primary btn-sm gguf-mp-apply" data-admin-only data-model-params-apply="' + Utils.escapeHtml(safeName) + '">' +
                    '<i class="fas fa-rotate"></i> ' + Utils.escapeHtml(_('gguf.mp_apply', 'Применить и перезагрузить')) +
                '</button>' +
                '<small class="form-text gguf-mp-warn">' + Utils.escapeHtml(_('gguf.mp_apply_hint',
                    'Применение сохранит профиль модели и перезагрузит её: текущая сессия прервётся, загрузка займёт время.')) + '</small>' +
            '</div>' +
        '</div>';
    }

    // hydrateModelParamEditors — подставить в формы значения ПРОФИЛЯ (R134).
    //
    // ЗАЧЕМ ОТДЕЛЬНЫМ ШАГОМ. Карточка рисуется синхронно, а профиль приходит
    // запросом. Из-за этого модель на диске открывалась с пустыми полями
    // («не задано»), хотя значения в профиле были, — и оператор не мог понять, что
    // же применится при загрузке. Профили запрашиваются ОДНИМ списком и раздаются
    // всем формам на странице.
    function hydrateModelParamEditors() {
        if (typeof document === 'undefined' || !document.querySelectorAll) return;
        var editors = document.querySelectorAll('.gguf-mp-editor[data-model]:not([data-hydrated="1"])');
        if (!editors || !editors.length) return;
        var api = (typeof window !== 'undefined') && (window.Api || window.BalancerApi);
        if (!api || !api.cppworkerModelProfiles || typeof api.cppworkerModelProfiles.list !== 'function') return;
        Array.prototype.forEach.call(editors, function (el) { el.setAttribute('data-hydrated', '1'); });
        api.cppworkerModelProfiles.list().then(function (res) {
            var profiles = (res && (res.models || res.profiles)) || {};
            // R162 (2026-10-17): кладём профили в state — это ЕДИНСТВЕННЫЙ
            // источник НАМЕРЕНИЯ ОПЕРАТОРА (contextLength и т.п.) для чипов
            // карточки загруженной модели. Без него R155 («окно ПОДНЯТО по
            // запросу клиента») не срабатывал вообще: сравнивать фактическое окно
            // инстанса было не с чем, и стрелка не появлялась никогда.
            if (M.state && typeof M.state === 'object') {
                M.state.modelProfiles = profiles;
                // R319 (2026-10-30): причина ограничения окна. Бэкендер УЖЕ
                // считал её (R267) и клал в этот же ответ, но фронт забирал
                // только models — `effectiveContext` уходил в корзину, а
                // `nctxCeiling` (потолок без привязки к профилю) не запрашивался
                // вовсе. Оператор видел в форме своё число и не видел ни одного
                // места, где сказано, что окно режет env.
                M.state.modelEffectiveWindow = (res && res.effectiveContext) || {};
                M.state.nctxCeiling = (res && res.nctxCeiling) || null;
            }
            Array.prototype.forEach.call(editors, function (el) {
                fillModelParamEditor(el, profiles[el.getAttribute('data-model')] || null);
                paintWindowLimit(el);
            });
            // R229 (2026-10-22): подписать «наследуется» РЕАЛЬНЫМ значением.
            applyInheritedDefaults(editors);
        }).catch(function () {
            // Профили не получены: формы остаются с тем, что уже известно.
        });
    }

    // windowLimitNote — R319 (2026-10-30): ОДНО место, которое объясняет, ЧЕМ
    // ограничено окно модели, и делает это ТОЛЬКО из того, что сказал балансер.
    //
    // ЗАЧЕМ. Живая жалоба повторялась: «модель обучена на 131072, VRAM даёт
    // 131072 — почему окно 32768?». В интерфейсе были два первых числа
    // (feasible/GGUF, R83) и ни одного упоминания третьего — потолка авто-роста
    // LB_NCTX_RELOAD_MAX_N_CTX. Бэкендер отдаёт причину в GET model-profiles
    // (`effectiveContext[<модель>].limitReason` + `nctxCeiling`), поэтому текст
    // берём из ответа, а не сочиняем: если причина не пришла — это видно по
    // тексту, а не подменяется догадкой.
    //
    // Параметры:
    //   entry    — effectiveContext[<модель>] или null/undefined;
    //   ceiling  — nctxCeiling из того же ответа или null.
    // Возвращает { tone: 'normal'|'warn', text }.
    function windowLimitNote(entry, ceiling) {
        var envCeil = ceiling && typeof ceiling.envCeiling === 'number' ? ceiling.envCeiling : 0;
        if (entry && typeof entry === 'object' && entry.effectiveContextLength !== undefined) {
            var reason = entry.limitReason ? String(entry.limitReason) : '';
            var byEnv = entry.limitedBy === 'env';
            var title = byEnv
                ? 'Ограничивает env (LB_NCTX_RELOAD_MAX_N_CTX=' + (envCeil > 0 ? envCeil : '?') + ')'
                : 'Ограничивает профиль модели';
            var text = 'Предел окна: ' + String(entry.effectiveContextLength) + '. ' + title;
            if (reason) text += '. ' + reason;
            return { tone: byEnv ? 'warn' : 'normal', text: text };
        }
        if (envCeil > 0) {
            return {
                tone: 'normal',
                text: 'Предел окна: ' + envCeil + ' (env LB_NCTX_RELOAD_MAX_N_CTX). ' +
                    ((ceiling && ceiling.reason) || 'Профиля у модели нет — действует потолок авто-роста.')
            };
        }
        return {
            tone: 'normal',
            text: (ceiling && ceiling.reason) ||
                'Предел окна не задан: профиля нет и LB_NCTX_RELOAD_MAX_N_CTX пуст — ' +
                'предел проверит загрузка (VRAM/GGUF).'
        };
    }

    // modelEffectiveEntry — найти запись effectiveContext для модели.
    //
    // Ключ карты — имя модели балансера, а карточка может держать имя файла
    // (`X.gguf`) или путь: перебираем те же варианты, что intentOf (R162), иначе
    // причина ограничения молча не находилась бы ровно у загруженной модели.
    function modelEffectiveEntry(modelName, effectiveMap) {
        if (!effectiveMap || typeof effectiveMap !== 'object') return null;
        // Расширение снимаем локально, а не через stripGGUF из соседнего модуля:
        // функция должна работать и там, где helpers ещё не загружен (и в тестах),
        // иначе причина «не нашлась» ровно у имени файла — самого частого написания.
        const noExt = function (v) { return String(v || '').replace(/\.gguf$/i, ''); };
        const full = String(modelName || '');
        const base = full ? full.split(/[\\/]/).pop() : '';
        const keys = [full, base, noExt(full), noExt(base)];
        for (let i = 0; i < keys.length; i++) {
            const k = keys[i];
            if (k && Object.prototype.hasOwnProperty.call(effectiveMap, k)) return effectiveMap[k];
        }
        return null;
    }

    // paintWindowLimit — R319: подписать форму «чем ограничено окно».
    //
    // Данные приходят асинхронно ПОСЛЕ отрисовки формы (hydrateModelParamEditors),
    // поэтому элемент создаётся здесь, а не в шаблоне. Несколько форм на странице
    // — у каждой свой data-model, поэтому ищем строго внутри своей формы.
    function paintWindowLimit(editorEl) {
        if (!editorEl || typeof document === 'undefined') return;
        var state = (M.state && typeof M.state === 'object') ? M.state : null;
        if (!state || (!state.modelEffectiveWindow && !state.nctxCeiling)) return;
        var modelName = editorEl.getAttribute('data-model') || '';
        var entry = modelEffectiveEntry(modelName, state.modelEffectiveWindow);
        var note = windowLimitNote(entry, state.nctxCeiling || null);
        var box = editorEl.querySelector('.gguf-mp-limit');
        if (!box) {
            box = document.createElement('div');
            box.className = 'gguf-mp-limit';
            var actions = editorEl.querySelector('.gguf-mp-actions');
            if (actions && actions.parentNode === editorEl) {
                editorEl.insertBefore(box, actions);
            } else {
                editorEl.appendChild(box);
            }
        }
        box.className = 'gguf-mp-limit' + (note.tone === 'warn' ? ' gguf-mp-limit-warn' : '');
        box.setAttribute('data-nctx-limit', modelName);
        box.setAttribute('title', note.text);
        box.textContent = note.text;
    }

    // applyInheritedDefaults — R229: значения по умолчанию (что именно унаследует
    // пустое поле) приходят отдельным запросом GET /api/v1/cppworker/load-defaults:
    // effective — фактические значения, source — кто их поставил (default_profile
    // или env воркера). Кэшируем в состоянии страницы: один запрос на все открытые
    // формы. Недоступность не критична — остаётся честное «наследуется» без скобки.
    function applyInheritedDefaults(editors) {
        if (!editors || !editors.length) return;
        var api = (typeof window !== 'undefined') && (window.Api || window.BalancerApi);
        var cached = (M.state && M.state.loadDefaults) || null;
        var paint = function (d) {
            var effective = d && d.effective ? d.effective : null;
            if (!effective) return;
            Array.prototype.forEach.call(editors, function (el) {
                applyInheritedLabels(el, effective);
            });
        };
        if (cached && cached.effective) {
            paint(cached);
            return;
        }
        if (!api || !api.cppworkerLoadDefaults || typeof api.cppworkerLoadDefaults.get !== 'function') return;
        api.cppworkerLoadDefaults.get().then(function (d) {
            if (M.state && typeof M.state === 'object') M.state.loadDefaults = d || null;
            paint(d);
        }).catch(function () {
            // Значения по умолчанию не получены — метки остаются без значений.
        });
    }

    // hydrateOnOpen — подтянуть профиль при раскрытии карточки параметров.
    //
    // Значения профиля могли измениться (их правит и эта же форма), поэтому при
    // каждом раскрытии снимаем отметку «уже подставлено» и запрашиваем список
    // заново: список профилей — один недорогой запрос на все открытые формы.
    function hydrateOnOpen(target) {
        var box = (target && target.closest) ? target.closest('.gguf-mp-acc') : null;
        var scope = box || (typeof document !== 'undefined' ? document : null);
        if (scope && scope.querySelectorAll) {
            var editors = scope.querySelectorAll('.gguf-mp-editor[data-model]');
            Array.prototype.forEach.call(editors, function (el) { el.removeAttribute('data-hydrated'); });
        }
        hydrateModelParamEditors();
    }

    // fillModelParamEditor — разложить значения профиля по форме и по колонке
    // «В профиле». Пустое значение профиля остаётся «не задано» (наследуется).
    function fillModelParamEditor(editorEl, profile) {
        if (!editorEl) return;
        profile = profile || {};
        MODEL_PARAM_SPECS.forEach(function (spec) {
            var raw = profile[spec.field];
            var cell = editorEl.querySelector('[data-profile-field="' + spec.field + '"]');
            var input = editorEl.querySelector('.gguf-mp-field[data-field="' + spec.field + '"]');
            var known = !(raw === undefined || raw === null || raw === '');
            if (cell) {
                cell.innerHTML = known
                    ? Utils.escapeHtml(formatParamValue(spec, raw) || String(raw))
                    : '<span class="gguf-mp-none">' + Utils.escapeHtml(_('gguf.mp_not_in_profile', '— не задано —')) + '</span>';
            }
            if (!input) return;
            if (input.type === 'checkbox') {
                input.checked = known && (raw === true || raw === 'true');
            } else if (known) {
                input.value = Array.isArray(raw) ? raw.join(', ') : String(raw);
            } else {
                input.value = '';
            }
        });
    }

    // collectModelParams — собрать значения ИЗ формы редактора (R126).
    //
    // Пустое поле = «не задано» и в профиль НЕ попадает: иначе мы записали бы в
    // профиль модели пустоту и отключили наследование (профиль → настройки по
    // умолчанию → env), то есть «применить» ухудшило бы конфигурацию.
    function collectModelParams(modelName) {
        var root = document.querySelector('.gguf-mp-editor[data-model="' + String(modelName).replace(/"/g, '\\"') + '"]');
        if (!root) return null;
        var out = {};
        var fields = root.querySelectorAll('.gguf-mp-field');
        for (var i = 0; i < fields.length; i++) {
            var el = fields[i];
            var field = el.getAttribute('data-field');
            if (!field) continue;
            // R134 (доп.): bool — ТРЁХЗНАЧНЫЙ выбор ("" / "true" / "false").
            // Пустое значение означает «наследуется» и в профиль НЕ попадает:
            // иначе применение формы молча выключало бы параметр, которого
            // оператор не касался (живой класс жалобы «в карточке выключено,
            // хотя модель работает с включённым»).
            var dtype = el.getAttribute('data-type') || '';
            if (dtype === 'bool') {
                var braw = String(el.value || '').trim();
                if (braw === '') continue;
                out[field] = (braw === 'true');
                continue;
            }
            if (el.type === 'checkbox') {
                // Совместимость: старые чекбоксы (если где-то остались) собираем
                // только отмеченными — «не отмечено» больше не значит «выключить».
                if (el.checked) out[field] = true;
                continue;
            }
            var raw = String(el.value || '').trim();
            if (raw === '') continue; // не задано → не фиксируем
            // Строковые значения (enum) определяем по data-type, а НЕ по tagName:
            // иначе любой select сохранялся бы строкой, в том числе числовой.
            if (dtype === 'enum' || field === 'kvCacheType') {
                out[field] = raw;
            } else {
                var num = Number(raw);
                if (Number.isFinite(num)) out[field] = num;
            }
        }
        return out;
    }

    // applyModelParams — сохранить профиль модели и перезагрузить её (R126).
    //
    // Порядок ровно как у существующего пути применения профилей:
    //   PUT  .../model-profiles/{name}       — сохранить значения
    //   POST .../model-profiles/{name}/apply — save+reload (с прогрессом)
    // Второй шаг делегируем window.CppWorkerParams.applyProfileWithProgress, если
    // он доступен: там уже есть модалка прогресса и SSE-обновления. Если модуль
    // не загружен — говорим об этом честно, а не делаем вид, что применили.
    async function applyModelParams(modelName) {
        var backend = M.currentBackend();
        if (!backend) {
            showToast(_('gguf.mp_no_backend', 'Бэкенд не выбран'), 'error');
            return false;
        }
        var body = collectModelParams(modelName);
        if (body === null) {
            showToast(_('gguf.mp_no_editor', 'Форма параметров модели не найдена'), 'error');
            return false;
        }
        try {
            // ПРОФИЛИ МОДЕЛЕЙ ЖИВУТ НА БАЛАНСЕРЕ, а не в воркере: живая проверка
            // показала 404 на прокси-путь через бэкенд (cppworker такой ручки не
            // имеет вовсе), а на балансере PUT /api/v1/cppworker/model-profiles/{name}
            // отвечает 200. Поэтому используем документированный модуль
            // Api.cppworkerModelProfiles (webui/js/modules/api.js).
            if (!(window.Api && window.Api.cppworkerModelProfiles &&
                  typeof window.Api.cppworkerModelProfiles.upsert === 'function')) {
                showToast(_('gguf.mp_api_missing',
                    'Модуль профилей моделей не загружен — сохранить параметры нельзя'), 'error');
                return false;
            }
            await window.Api.cppworkerModelProfiles.upsert(modelName, body);
        } catch (e) {
            showToast(_('gguf.mp_save_failed', 'Не удалось сохранить профиль') + ': ' +
                (e && e.message ? e.message : e), 'error');
            return false;
        }
        if (window.CppWorkerParams && typeof window.CppWorkerParams.applyProfileWithProgress === 'function') {
            // applyProfileWithProgress сам показывает прогресс и результат перезагрузки.
            // R228: применяем ТОЛЬКО к выбранному бэкенду, а не ко всем, где модель загружена.
            await window.CppWorkerParams.applyProfileWithProgress(modelName, backend.id);
            return true;
        }
        showToast(_('gguf.mp_apply_manual',
            'Профиль сохранён. Применение с перезагрузкой недоступно: модуль прогресса не загружен — перезагрузите модель вручную.'),
            'warning');
        return false;
    }

        return {
            paramOrigin: paramOrigin,
            loadedParamsFor: loadedParamsFor,
            formatParamValue: formatParamValue,
            inheritedValueOf: inheritedValueOf,
            inheritedLabel: inheritedLabel,
            applyInheritedLabels: applyInheritedLabels,
            paramInput: paramInput,
            renderModelParamsEditor: renderModelParamsEditor,
            hydrateModelParamEditors: hydrateModelParamEditors,
            windowLimitNote: windowLimitNote,
            modelEffectiveEntry: modelEffectiveEntry,
            paintWindowLimit: paintWindowLimit,
            applyInheritedDefaults: applyInheritedDefaults,
            hydrateOnOpen: hydrateOnOpen,
            fillModelParamEditor: fillModelParamEditor,
            collectModelParams: collectModelParams,
            applyModelParams: applyModelParams
        };
    };
})();

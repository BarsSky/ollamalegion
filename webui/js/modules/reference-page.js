/**
 * reference-page.js — страница WebUI «Справка и шаблоны» (R493, 2026-11-17).
 *
 * ЗАЧЕМ СТРАНИЦА. Рекомендации по параметрам загрузки моделей и «готовые
 * наборы» жили только в markdown-доках (`docs/operations/model-tuning-reference.ru.md`)
 * и в разрозненных JS-пресетах, не связанных с профилями моделей: у них не было
 * ни id, ни обоснования, ни версии, и применить их к загруженной модели было
 * нечем. Серверная часть уже реализована (`internal/reference` +
 * `/api/v1/reference/...`, отчёт `docs/plans/2026-11-17-reference-api-implementation.md`),
 * здесь — только UI и НИ ОДНОГО нового способа записи/перезагрузки.
 *
 * ЧТО ДЕЛАЕТ СТРАНИЦА (две вкладки):
 *   1. «Рекомендации» — записи-справка: заголовок, «к чему применимо», таблица
 *      параметров с построчными комментариями, обоснование, источники, автор,
 *      дата и revision. Фильтр по поиску и по модели. Редактор (создание,
 *      правка, удаление) — только admin.
 *   2. «Шаблоны» — именованные наборы параметров. Для выбранного шаблона: выбор
 *      МОДЕЛИ и БЭКЕНДА (только те бэкенды, где модель реально загружена —
 *      `Api.clusterModels.loaded()`), «Предпросмотр» (dry-run: diff + нарушения +
 *      вердикт памяти) и «Применить».
 *
 * ЧЕГО ЗДЕСЬ НАМЕРЕННО НЕТ (правила проекта, нарушение = дефект):
 *   * СВОЕЙ КНОПКИ «ОБНОВИТЬ». Кнопка одна — #refreshBtn в шапке. Страница
 *     регистрирует провайдера `DataRefresh.register('reference', load)`, поэтому
 *     и кнопка, и авто-тик обновляют ровно её источники (data-refresh.js).
 *   * ВТОРОГО МЕХАНИЗМА RELOAD. Применение шаблона — существующий путь профилей
 *     (сервер: `applyReferenceTemplate` → `applyModelProfile`), а прогресс и SSE
 *     берутся из существующей модалки `CppWorkerParams.applyProfileWithProgress`
 *     (ей передаётся функция применения — третий аргумент, добавленный R493).
 *   * ВТОРОЙ КОПИИ ЗНАНИЙ О ПОЛЯХ. Метаданные параметров (тип, границы, шаг,
 *     варианты, подписи) берутся из `window.GgufModule.MODEL_PARAM_SPECS`
 *     (gguf-renderer-detail-render.js) — там же, где их читает карточка модели.
 *   * ВТОРОЙ ОБЁРТКИ HTTP. Все запросы идут через `Api.reference` (api.js):
 *     заголовки X-API-Token/X-CSRF-Token и таймауты уже решены там.
 *
 * ГРАНИЦЫ ОТВЕТСТВЕННОСТИ. Модуль — ВЛАДЕЛЕЦ СОСТОЯНИЯ страницы (как
 * settings-registry.js): разметка-каркас статическая (index.html), содержимое
 * рисует он; при обновлении данных DOM не пересобирается целиком, значения
 * полей живут в состоянии, а не в DOM.
 *
 * РЕШЕНИЯ ЧЕЛОВЕКА, зафиксированные контрактом (design §6):
 *   * `appliesTo` необязателен — запись без него «общая справка»;
 *   * мутации только admin (`data-admin-only` + серверная проверка);
 *   * mode="profile+reload" требует ЯВНЫЙ backendId (400 backend_required) —
 *     поэтому «Применить» неактивна, пока не выбраны модель и бэкенд;
 *   * единицы параметров — канонические единицы поля профиля.
 *
 * Экспорт: window.ReferencePage = { mount, load, selectTab, registerProvider,
 *   renderRecommendationRow, renderParamsTable, renderTemplateCard,
 *   renderTemplateDetail, filterRecommendations, matchRecommendation, globMatch,
 *   appliesToChips, loadedTargetsFor, applyGate, formatParamValue,
 *   formatDiffValue, formatDiffRow, renderDiff, renderPreview, errorInfo,
 *   errorText, paramSpecs, _state }.
 */
(function () {
    'use strict';


    // =====================================================================
    // R642: состояние и помощники — в reference-page-helpers.js, он грузится
    // ПЕРЕД этим файлом. `state` берём ПО ССЫЛКЕ (переприсваивания нет).
    // =====================================================================

    var helpers = window.ReferencePageHelpers;
    if (!helpers) {
        // Без помощников страница не отрисуется: лучше громкая ошибка, чем пустая
        // вкладка без объяснения (оператор искал бы причину в API).
        throw new Error('reference-page.js: window.ReferencePageHelpers не загружен — проверьте порядок <script> в index.html');
    }
    var state = helpers.state;
    var t = helpers.t;
    var esc = helpers.esc;
    var byId = helpers.byId;
    var api = helpers.api;
    var modelsApi = helpers.modelsApi;
    var paramSpecs = helpers.paramSpecs;
    var paramSpec = helpers.paramSpec;
    var specOrder = helpers.specOrder;
    var specLabel = helpers.specLabel;
    var formatParamValue = helpers.formatParamValue;
    var formatDiffValue = helpers.formatDiffValue;
    var formatDiffRow = helpers.formatDiffRow;
    var renderDiff = helpers.renderDiff;
    var globMatch = helpers.globMatch;
    var matchRecommendation = helpers.matchRecommendation;
    var filterRecommendations = helpers.filterRecommendations;
    var appliesToChips = helpers.appliesToChips;
    var loadedTargetsFor = helpers.loadedTargetsFor;
    var normalizeModelName = helpers.normalizeModelName;
    var applyGate = helpers.applyGate;
    var gateText = helpers.gateText;
    var errorInfo = helpers.errorInfo;
    var errorText = helpers.errorText;
    var errorHtml = helpers.errorHtml;
    var paramRows = helpers.paramRows;
    var renderParamsTable = helpers.renderParamsTable;
    var sourcesHtml = helpers.sourcesHtml;
    var recMetaHtml = helpers.recMetaHtml;
    var renderRecommendationRow = helpers.renderRecommendationRow;
    var renderRecommendationList = helpers.renderRecommendationList;
    var templateById = helpers.templateById;
    var modelChoices = helpers.modelChoices;
    var backendChoices = helpers.backendChoices;
    var catalogModelNames = helpers.catalogModelNames;


    function renderTargets(targets) {
        var list = Object.prototype.toString.call(targets) === '[object Array]' ? targets : [];
        if (!list.length) {
            return '<div class="reference-empty">' +
                esc(t('reference.targets.none', 'Ни на одном llama.cpp-бэкенде модели нет')) + '</div>';
        }
        return '<ul class="reference-targets">' + list.map(function (tg) {
            var stateText = tg.loaded
                ? t('reference.targets.loaded', 'загружена')
                : t('reference.targets.not_loaded', 'не загружена');
            var extra = [];
            if (tg.loaded && tg.contextLength) extra.push('ctx=' + String(tg.contextLength));
            if (tg.activeQueries !== undefined && tg.activeQueries !== null) extra.push('activeQueries=' + String(tg.activeQueries));
            return '<li><code>' + esc(tg.backendId) + '</code> — ' + esc(stateText) +
                (extra.length ? ' <span class="reference-muted">(' + esc(extra.join(', ')) + ')</span>' : '') +
                '</li>';
        }).join('') + '</ul>';
    }

    function renderMemory(memory) {
        var mem = memory && typeof memory === 'object' ? memory : null;
        if (!mem) return '';
        var verdict = mem.checked
            ? (mem.feasible
                ? t('reference.memory.feasible', 'память: помещается')
                : t('reference.memory.not_feasible', 'память: НЕ помещается'))
            : t('reference.memory.not_checked', 'память: проверить не удалось (cppworker не ответил)');
        var cls = !mem.checked ? 'reference-memory-unknown' : (mem.feasible ? 'reference-memory-ok' : 'reference-memory-bad');
        var extras = [];
        if (mem.stage) extras.push(t('reference.memory.stage', 'этап: {value}', { value: mem.stage }));
        if (mem.degraded) extras.push(t('reference.memory.degraded', 'часть слоёв уйдёт в RAM'));
        return '<div class="reference-memory ' + cls + '">' +
            '<strong>' + esc(verdict) + '</strong>' +
            (mem.message ? ' <span>' + esc(mem.message) + '</span>' : '') +
            (extras.length ? ' <span class="reference-muted">(' + esc(extras.join('; ')) + ')</span>' : '') +
            '</div>';
    }

    /**
     * renderPreview — отчёт предпросмотра: diff, нарушения, память, цели.
     *
     * Ничего не сохраняет (dry-run на сервере): это ответ `POST …/preview`.
     */
    function renderPreview(preview) {
        var p = preview && typeof preview === 'object' ? preview : null;
        if (!p) return '';
        var valid = p.valid || { ok: true, violations: [] };
        var violations = Object.prototype.toString.call(valid.violations) === '[object Array]' ? valid.violations : [];
        var okBadge = (p.ok && valid.ok)
            ? '<span class="badge badge-success">' + esc(t('reference.preview.ok', 'можно применять')) + '</span>'
            : '<span class="badge badge-danger">' + esc(t('reference.preview.not_ok', 'есть нарушения')) + '</span>';
        var html = '<div class="reference-preview" data-ref-preview="1">' +
            '<div class="reference-preview-head">' +
                '<strong>' + esc(t('reference.preview.title', 'Предпросмотр (ничего не сохранено)')) + '</strong> ' +
                '<span class="reference-muted">' + esc(p.model || '') + '</span> ' + okBadge +
            '</div>' +
            '<div class="reference-preview-section"><span class="reference-preview-label">' +
                esc(t('reference.preview.diff_title', 'Что изменится')) + ':</span>' + renderDiff(p.diff) + '</div>';
        if (violations.length) {
            html += '<div class="reference-preview-section"><span class="reference-preview-label">' +
                esc(t('reference.preview.violations', 'Нарушения проверки')) + ':</span>' +
                '<ul class="reference-violations">' + violations.map(function (v) {
                    return '<li>' + esc((v && v.field ? v.field + ': ' : '') + ((v && v.message) || '')) + '</li>';
                }).join('') + '</ul></div>';
        }
        html += '<div class="reference-preview-section">' + renderMemory(p.memory) + '</div>';
        html += '<div class="reference-preview-section"><span class="reference-preview-label">' +
            esc(t('reference.preview.targets', 'Где применимо')) + ':</span>' + renderTargets(p.targets) + '</div>';
        var warnings = Object.prototype.toString.call(p.warnings) === '[object Array]' ? p.warnings : [];
        if (warnings.length) {
            html += '<ul class="reference-warnings">' + warnings.map(function (w) {
                return '<li>' + esc(w) + '</li>';
            }).join('') + '</ul>';
        }
        return html + '</div>';
    }

    /**
     * renderTemplateDetail — управление применением выбранного шаблона.
     *
     * `ctx` (необязательный) позволяет проверить функцию тестом без DOM:
     * {tplState, models, targets}. В приложении ctx не передаётся и значения
     * берутся из состояния.
     */
    function renderTemplateDetail(tpl, ctx) {
        var c = ctx || {};
        var tplState = c.tplState || state.tpl;
        var models = Object.prototype.toString.call(c.models) === '[object Array]'
            ? c.models : modelChoices(tplState.model);
        var targets = Object.prototype.toString.call(c.targets) === '[object Array]'
            ? c.targets : loadedTargetsFor(state.loadedModels, tplState.model, state.catalog);
        var gate = applyGate({
            model: tplState.model,
            backendId: tplState.backendId,
            mode: tplState.mode,
            targets: targets
        });
        var modelOptions = ['<option value="">' + esc(t('reference.tpl.pick_model', '— выберите модель —')) + '</option>']
            .concat(models.map(function (name) {
                return '<option value="' + esc(name) + '"' +
                    (String(name) === String(tplState.model) ? ' selected' : '') + '>' + esc(name) + '</option>';
            })).join('');
        var backendOptions = ['<option value="">' + esc(t('reference.tpl.pick_backend', '— выберите бэкенд —')) + '</option>']
            .concat(targets.map(function (tg) {
                // R516: узел, где файл есть, а модель не загружена, помечаем —
                // иначе оператор не понимает, почему применение «зависло».
                var label = tg.loaded === false
                    ? tg.backendId + ' ' + t('reference.tpl.on_disk', '(файл на диске, не загружена)')
                    : tg.backendId;
                return '<option value="' + esc(tg.backendId) + '"' +
                    (String(tg.backendId) === String(tplState.backendId) ? ' selected' : '') + '>' + esc(label) + '</option>';
            })).join('');
        var applyDisabled = gate.disabled ? ' disabled' : '';
        var previewDisabled = String(tplState.model || '') ? '' : ' disabled';

        return '<div class="reference-tpl-detail" data-ref-tpl-detail="' + esc(tpl && tpl.id) + '">' +
            '<div class="reference-tpl-controls">' +
                '<label class="reference-field"><span>' + esc(t('reference.tpl.model', 'Модель (загруженная в кластере)')) + '</span>' +
                    '<select class="form-control" data-ref-tpl="model">' + modelOptions + '</select></label>' +
                '<label class="reference-field"><span>' + esc(t('reference.tpl.backend', 'Бэкенд, где модель загружена')) + '</span>' +
                    '<select class="form-control" data-ref-tpl="backendId">' + backendOptions + '</select></label>' +
                '<label class="reference-field"><span>' + esc(t('reference.tpl.mode', 'Режим применения')) + '</span>' +
                    '<select class="form-control" data-ref-tpl="mode">' +
                        '<option value="profile+reload"' + (tplState.mode === 'profile+reload' ? ' selected' : '') + '>' +
                            esc(t('reference.tpl.mode_reload', 'профиль + reload (на выбранном бэкенде)')) + '</option>' +
                        '<option value="profile-only"' + (tplState.mode === 'profile-only' ? ' selected' : '') + '>' +
                            esc(t('reference.tpl.mode_profile_only', 'только профиль (без reload)')) + '</option>' +
                    '</select></label>' +
            '</div>' +
            '<div class="reference-tpl-actions">' +
                // Предпросмотр — POST (dry-run), а сервер по контракту требует
                // admin на любой не-read-only метод; поэтому кнопка тоже admin-only.
                '<button type="button" class="btn btn-secondary" data-admin-only data-ref-action="preview"' + previewDisabled + '>' +
                    esc(t('reference.tpl.preview', 'Предпросмотр')) + '</button>' +
                '<button type="button" class="btn btn-primary" data-admin-only data-ref-action="apply"' + applyDisabled + '>' +
                    esc(t('reference.tpl.apply', 'Применить')) + '</button>' +
                // R516: предупреждение «будет загрузка» — не отказ, кнопка активна.
                ((gate.disabled || gate.warn)
                    ? '<span class="reference-apply-hint' + (gate.disabled ? '' : ' is-warning') + '">' +
                        esc(gateText(gate.disabled ? gate.reason : gate.warn)) + '</span>'
                    : '') +
            '</div>' +
            (tplState.error ? '<div class="reference-status reference-status-error">' + errorHtml(tplState.error) + '</div>' : '') +
            (tplState.busy ? '<div class="reference-empty">' + esc(t('reference.tpl.busy', 'Считаю предпросмотр…')) + '</div>' : '') +
            // R515: подсказка «кто уже совпадает» — API отдаёт usage.usedByProfiles
            // в GET …/templates/{id}, а страница его не показывала вовсе.
            (typeof tplState.usedByProfiles !== 'undefined' && tplState.usedByProfiles
                ? '<div class="reference-tpl-usage">' +
                    (tplState.usedByProfiles.length
                        ? esc(t('reference.tpl_editor.usage_some',
                            'Значения шаблона уже совпадают с профилями: {value}',
                            { value: tplState.usedByProfiles.join(', ') }))
                        : esc(t('reference.tpl_editor.usage_none',
                            'Ни один профиль модели пока не совпадает со значениями шаблона.'))) +
                  '</div>'
                : '') +
            renderPreview(tplState.preview) +
            '</div>';
    }

    /** renderTemplateCard — карточка шаблона: параметры, comment, деталь по требованию. */
    function renderTemplateCard(tpl) {
        if (!tpl) return '';
        var id = String(tpl.id || '');
        var expanded = String(state.tpl.id) === id;
        var rows = paramRows(tpl.params);
        var related = tpl.basedOn
            ? '<span class="badge badge-info">' + esc(t('reference.tpl.based_on', 'из записи: {value}', { value: tpl.basedOn })) + '</span>'
            : '';
        return '<div class="reference-card' + (expanded ? ' is-expanded' : '') + '" data-ref-tpl-card="' + esc(id) + '">' +
            '<div class="reference-card-head">' +
                '<div class="reference-card-headline">' +
                    '<h4 class="reference-card-title">' + esc(tpl.name || id) + '</h4>' +
                    '<div class="reference-card-badges">' +
                        '<code class="reference-id">' + esc(id) + '</code> ' +
                        '<span class="badge reference-chip">' + esc(t('reference.tpl.version', 'версия {value}', { value: tpl.version || 1 })) + '</span> ' +
                        '<span class="badge reference-chip">' + esc(t('reference.tpl.params_count', 'параметров: {value}', { value: rows.length })) + '</span> ' +
                        related +
                    '</div>' +
                '</div>' +
                '<div class="reference-card-actions">' +
                    '<button type="button" class="btn btn-secondary btn-sm" data-ref-action="toggle-tpl" data-ref-id="' + esc(id) + '">' +
                        esc(expanded ? t('reference.tpl.collapse', 'Свернуть') : t('reference.tpl.expand', 'Открыть')) + '</button>' +
                    // R515: правка и удаление шаблона. Сервер их умел всегда
                    // (PUT/DELETE …/templates/{id}), а в UI кнопок не было —
                    // на это и была жалоба «шаблоны нельзя править».
                    '<button type="button" class="btn btn-secondary btn-sm" data-admin-only data-ref-action="edit-tpl" data-ref-id="' + esc(id) + '">' +
                        esc(t('reference.tpl_editor.edit_short', 'Изменить')) + '</button>' +
                    '<button type="button" class="btn btn-danger btn-sm" data-admin-only data-ref-action="delete-tpl" data-ref-id="' + esc(id) + '">' +
                        esc(t('reference.tpl_editor.delete_short', 'Удалить')) + '</button>' +
                '</div>' +
            '</div>' +
            (tpl.comment ? '<p class="reference-card-comment">' + esc(tpl.comment) + '</p>' : '') +
            renderParamsTable(tpl.params) +
            (expanded ? renderTemplateDetail(tpl) : '') +
            '</div>';
    }

    function renderTemplateList() {
        var host = byId('referenceTplList');
        if (!host) return;
        if (!state.loaded && state.loading) {
            host.innerHTML = '<div class="reference-empty">' +
                esc(t('reference.loading', 'Загружаю справочник…')) + '</div>';
            return;
        }
        if (!state.templates.length) {
            host.innerHTML = '<div class="reference-empty">' + esc(t('reference.tpl.empty',
                'Шаблонов пока нет. Откройте запись на вкладке «Рекомендации» и нажмите «Создать шаблон».')) + '</div>';
            return;
        }
        host.innerHTML = state.templates.map(renderTemplateCard).join('');
    }

    // =====================================================================
    // Рендер: вкладки, статус, фильтр моделей
    // =====================================================================

    function renderTabs() {
        var root = state.root || byId('reference-page');
        if (!root || !root.querySelectorAll) return;
        var tabs = root.querySelectorAll('[data-ref-tab]');
        Array.prototype.forEach.call(tabs, function (tab) {
            var name = tab.getAttribute('data-ref-tab');
            if (name === state.tab) tab.classList.add('active');
            else tab.classList.remove('active');
        });
        var recPane = byId('referenceRecPane');
        var tplPane = byId('referenceTplPane');
        if (recPane) recPane.hidden = state.tab !== 'recommendations';
        if (tplPane) tplPane.hidden = state.tab !== 'templates';
    }

    function renderModelFilter() {
        var select = byId('referenceModelFilter');
        if (!select) return;
        var options = ['<option value="">' + esc(t('reference.filter.all_models', 'все модели')) + '</option>'];
        modelChoices(state.filter.model).forEach(function (name) {
            options.push('<option value="' + esc(name) + '"' +
                (String(name) === String(state.filter.model) ? ' selected' : '') + '>' + esc(name) + '</option>');
        });
        select.innerHTML = options.join('');
    }

    function renderStatus() {
        var host = byId('referenceStatus');
        if (!host) return;
        if (state.error) {
            host.hidden = false;
            host.className = 'reference-status reference-status-error';
            host.innerHTML = errorHtml(state.error);
            return;
        }
        if (state.loading && !state.loaded) {
            host.hidden = false;
            host.className = 'reference-status';
            host.textContent = t('reference.loading', 'Загружаю справочник…');
            return;
        }
        host.hidden = true;
        host.className = 'reference-status';
        host.innerHTML = '';
    }

    function showStatus(kind, text) {
        var host = byId('referenceStatus');
        if (!host) return;
        host.hidden = false;
        host.className = 'reference-status reference-status-' + (kind === 'error' ? 'error' : 'ok');
        host.textContent = String(text || '');
    }

    function renderAll() {
        renderStatus();
        renderTabs();
        renderModelFilter();
        renderRecommendationList();
        renderTemplateList();
        // R515: редактор шаблона перерисовывается вместе со списком: после
        // load() он мог ссылаться на только что удалённый шаблон.
        renderTplEditor();
    }

    // =====================================================================
    // Редактор рекомендации (создание / правка / удаление)
    // =====================================================================

    function emptyEditor(id) {
        return {
            isNew: !id,
            id: id || '',
            revision: 0,
            title: '',
            comment: '',
            applies: { modelPattern: '', backendType: '', quantization: '', vramMinMB: '', vramMaxMB: '' },
            params: [],
            sources: []
        };
    }

    function editorFromRecord(rec) {
        var e = emptyEditor(rec.id);
        e.isNew = false;
        e.revision = Number(rec.revision || 0);
        e.title = rec.title || '';
        e.comment = rec.comment || '';
        var a = rec.appliesTo || {};
        e.applies = {
            modelPattern: a.modelPattern || '',
            backendType: a.backendType || '',
            quantization: a.quantization || '',
            vramMinMB: (a.vramMinMB === undefined ? '' : String(a.vramMinMB)),
            vramMaxMB: (a.vramMaxMB === undefined ? '' : String(a.vramMaxMB))
        };
        e.params = (rec.params || []).map(function (p) {
            return {
                name: String(p.name || ''),
                // Сырое текстовое значение: тип приводим при сохранении по
                // метаданным поля (MODEL_PARAM_SPECS), а не «на глаз».
                raw: (p.value === undefined || p.value === null) ? '' : String(p.value),
                unit: p.unit || '',
                comment: p.comment || ''
            };
        });
        e.sources = (rec.sources || []).map(function (s) {
            return { doc: s.doc || '', section: s.section || '' };
        });
        return e;
    }

    function paramInputHtml(row, index) {
        var spec = paramSpec(row.name);
        var type = (spec && spec.type) || 'text';
        var attrs = ' class="form-control" data-ref-edit="paramValue" data-ref-index="' + index + '"';
        if (type === 'bool') {
            var currentTrue = String(row.raw).toLowerCase() === 'true';
            return '<select' + attrs + '>' +
                '<option value="' + (currentTrue ? 'false' : '') + '"' + (currentTrue ? '' : ' selected') + '>' +
                    esc(t('reference.value.no', 'нет')) + '</option>' +
                '<option value="true"' + (currentTrue ? ' selected' : '') + '>' + esc(t('reference.value.yes', 'да')) + '</option>' +
                '</select>';
        }
        if (type === 'enum' && spec && Object.prototype.toString.call(spec.options) === '[object Array]') {
            return '<select' + attrs + '>' + spec.options.map(function (option) {
                return '<option value="' + esc(option) + '"' +
                    (String(row.raw) === String(option) ? ' selected' : '') + '>' + esc(option) + '</option>';
            }).join('') + '</select>';
        }
        var extra = '';
        if (type === 'int') {
            if (spec && spec.min !== undefined) extra += ' min="' + esc(spec.min) + '"';
            if (spec && spec.max !== undefined) extra += ' max="' + esc(spec.max) + '"';
            if (spec && spec.step !== undefined) extra += ' step="' + esc(spec.step) + '"';
            extra += ' type="number"';
        } else {
            extra += ' type="text"';
        }
        return '<input' + attrs + extra + ' value="' + esc(row.raw) + '">';
    }

    /**
     * renderEditor — форма записи-рекомендации.
     *
     * Список полей берётся из MODEL_PARAM_SPECS: подпись, тип, границы, шаг,
     * варианты. Своей таблицы полей здесь нет — иначе форма разошлась бы с
     * карточкой модели и с серверным `validateModelProfile`.
     */
    function renderEditor() {
        var host = byId('referenceEditor');
        if (!host) return;
        var e = state.editor;
        if (!e) {
            host.hidden = true;
            host.innerHTML = '';
            return;
        }
        host.hidden = false;
        var specs = paramSpecs();
        var paramRowsHtml = e.params.map(function (row, index) {
            var selects = '<select class="form-control" data-ref-edit="paramName" data-ref-index="' + index + '">' +
                '<option value="">' + esc(t('reference.editor.pick_param', '— поле профиля —')) + '</option>' +
                specs.map(function (spec) {
                    return '<option value="' + esc(spec.field) + '"' +
                        (String(row.name) === String(spec.field) ? ' selected' : '') + '>' +
                        esc(spec.field) + ' — ' + esc(specLabel(spec)) + '</option>';
                }).join('') +
                // Поле, которого нет в реестре (файл правили руками) — показываем
                // как есть, чтобы правка не потеряла данные; сервер его отклонит.
                (row.name && !paramSpec(row.name)
                    ? '<option value="' + esc(row.name) + '" selected>' + esc(row.name) + '</option>' : '') +
                '</select>';
            return '<tr>' +
                '<td>' + selects + '</td>' +
                '<td>' + paramInputHtml(row, index) + '</td>' +
                '<td><input class="form-control" type="text" data-ref-edit="paramUnit" data-ref-index="' + index + '" value="' + esc(row.unit) + '"></td>' +
                '<td><input class="form-control" type="text" data-ref-edit="paramComment" data-ref-index="' + index + '" value="' + esc(row.comment) + '"></td>' +
                '<td><button type="button" class="btn btn-danger btn-sm" data-ref-action="editor-del-param" data-ref-index="' + index + '">×</button></td>' +
                '</tr>';
        }).join('');
        var sourceRows = e.sources.map(function (src, index) {
            return '<div class="reference-source-row">' +
                '<input class="form-control" type="text" data-ref-edit="sourceDoc" data-ref-index="' + index + '" ' +
                    'placeholder="docs/operations/model-tuning-reference.ru.md" value="' + esc(src.doc) + '">' +
                '<input class="form-control" type="text" data-ref-edit="sourceSection" data-ref-index="' + index + '" ' +
                    'placeholder="§3.2" value="' + esc(src.section) + '">' +
                '<button type="button" class="btn btn-danger btn-sm" data-ref-action="editor-del-source" data-ref-index="' + index + '">×</button>' +
                '</div>';
        }).join('');
        var title = e.isNew
            ? t('reference.editor.title_new', 'Новая запись-рекомендация')
            : t('reference.editor.title_edit', 'Правка записи «{value}»', { value: e.id });
        host.innerHTML = '<div class="reference-editor-card">' +
            '<div class="reference-editor-head">' +
                '<h4>' + esc(title) + '</h4>' +
                '<button type="button" class="btn btn-secondary btn-sm" data-ref-action="editor-cancel">' +
                    esc(t('common.close', 'Закрыть')) + '</button>' +
            '</div>' +
            '<div class="reference-editor-body">' +
                '<div class="reference-editor-grid">' +
                    '<label class="reference-field"><span>' + esc(t('reference.editor.id', 'Идентификатор (slug)')) + '</span>' +
                        '<input class="form-control" type="text" data-ref-edit="id" value="' + esc(e.id) + '"' +
                        (e.isNew ? '' : ' disabled') + '></label>' +
                    '<label class="reference-field"><span>' + esc(t('reference.editor.title', 'Заголовок')) + '</span>' +
                        '<input class="form-control" type="text" data-ref-edit="title" value="' + esc(e.title) + '"></label>' +
                '</div>' +
                '<fieldset class="reference-editor-fieldset"><legend>' +
                    esc(t('reference.editor.applies', 'К чему применимо (необязательно)')) + '</legend>' +
                    '<div class="reference-editor-grid">' +
                        '<label class="reference-field"><span>' + esc(t('reference.editor.model_pattern', 'Glob по имени модели')) + '</span>' +
                            '<input class="form-control" type="text" data-ref-edit="modelPattern" placeholder="Qwen3-4B-Instruct-2507*" value="' + esc(e.applies.modelPattern) + '"></label>' +
                        '<label class="reference-field"><span>' + esc(t('reference.editor.backend_type', 'Тип бэкенда')) + '</span>' +
                            '<input class="form-control" type="text" data-ref-edit="backendType" placeholder="llama_cpp" value="' + esc(e.applies.backendType) + '"></label>' +
                        '<label class="reference-field"><span>' + esc(t('reference.editor.quantization', 'Квантизация')) + '</span>' +
                            '<input class="form-control" type="text" data-ref-edit="quantization" placeholder="Q4_K_M" value="' + esc(e.applies.quantization) + '"></label>' +
                        '<label class="reference-field"><span>' + esc(t('reference.editor.vram_min', 'VRAM минимум, МБ')) + '</span>' +
                            '<input class="form-control" type="number" data-ref-edit="vramMinMB" value="' + esc(e.applies.vramMinMB) + '"></label>' +
                        '<label class="reference-field"><span>' + esc(t('reference.editor.vram_max', 'VRAM максимум, МБ')) + '</span>' +
                            '<input class="form-control" type="number" data-ref-edit="vramMaxMB" value="' + esc(e.applies.vramMaxMB) + '"></label>' +
                    '</div>' +
                '</fieldset>' +
                '<fieldset class="reference-editor-fieldset"><legend>' +
                    esc(t('reference.editor.params', 'Параметры (только поля профиля модели)')) + '</legend>' +
                    '<table class="data-table reference-edit-params"><thead><tr>' +
                        '<th>' + esc(t('reference.col.param', 'Параметр')) + '</th>' +
                        '<th>' + esc(t('reference.col.value', 'Значение')) + '</th>' +
                        '<th>' + esc(t('reference.col.unit', 'Единица')) + '</th>' +
                        '<th>' + esc(t('reference.col.comment', 'Комментарий')) + '</th>' +
                        '<th></th>' +
                    '</tr></thead><tbody>' + paramRowsHtml + '</tbody></table>' +
                    '<button type="button" class="btn btn-secondary btn-sm" data-ref-action="editor-add-param">' +
                        esc(t('reference.editor.add_param', '+ Параметр')) + '</button>' +
                '</fieldset>' +
                '<label class="reference-field reference-field-wide"><span>' +
                    esc(t('reference.editor.comment', 'Обоснование')) + '</span>' +
                    '<textarea class="form-control" rows="4" data-ref-edit="comment">' + esc(e.comment) + '</textarea></label>' +
                '<fieldset class="reference-editor-fieldset"><legend>' +
                    esc(t('reference.editor.sources', 'Источники (ссылки на доки, не копии)')) + '</legend>' +
                    sourceRows +
                    '<button type="button" class="btn btn-secondary btn-sm" data-ref-action="editor-add-source">' +
                        esc(t('reference.editor.add_source', '+ Источник')) + '</button>' +
                '</fieldset>' +
                '<div class="reference-editor-actions">' +
                    '<button type="button" class="btn btn-primary" data-admin-only data-ref-action="editor-save">' +
                        esc(t('reference.editor.save', 'Сохранить')) + '</button>' +
                    (e.isNew ? '' :
                        '<button type="button" class="btn btn-danger" data-admin-only data-ref-action="editor-delete">' +
                            esc(t('reference.rec.delete', 'Удалить')) + '</button>') +
                    '<button type="button" class="btn btn-secondary" data-ref-action="editor-cancel">' +
                        esc(t('common.cancel', 'Отмена')) + '</button>' +
                '</div>' +
            '</div>' +
            '</div>';
    }

    // =====================================================================
    // Действия
    // =====================================================================

    function openEditor(rec) {
        state.editor = rec ? editorFromRecord(rec) : emptyEditor('');
        if (!rec) {
            // Новая запись: сразу один параметр, иначе форма встречает пустотой.
            state.editor.params.push({ name: 'contextLength', raw: '', unit: 'tokens', comment: '' });
        }
        renderEditor();
        var host = byId('referenceEditor');
        if (host && typeof host.scrollIntoView === 'function') host.scrollIntoView({ block: 'nearest' });
    }

    function closeEditor() {
        state.editor = null;
        renderEditor();
    }

    /** coerceValue — сырой текст поля → JSON-примитив по метаданным поля. */
    function coerceValue(spec, raw) {
        var text = String(raw === undefined || raw === null ? '' : raw).trim();
        var type = (spec && spec.type) || 'text';
        if (type === 'bool') return text === 'true';
        if (type === 'int') {
            if (!text) return null;
            var num = Number(text);
            return isNaN(num) ? null : num;
        }
        if (type === 'list') {
            if (!text) return null;
            try {
                var parsed = JSON.parse(text);
                if (Object.prototype.toString.call(parsed) === '[object Array]') return parsed;
            } catch (e) { /* список может быть задан через запятую */ }
            return text.split(',').map(function (part) { return part.trim(); }).filter(function (part) { return part; });
        }
        return text;
    }

    /** editorBody — состояние редактора → тело PUT (контракт design §2.3). */
    function editorBody() {
        var e = state.editor;
        var body = {
            title: String(e.title || '').trim(),
            comment: String(e.comment || '').trim(),
            params: [],
            sources: []
        };
        e.params.forEach(function (row) {
            if (!row.name) return;
            var spec = paramSpec(row.name);
            var value = coerceValue(spec, row.raw);
            if (value === null) return; // пустое значение не выдумываем
            var param = { name: row.name, value: value };
            if (row.unit) param.unit = row.unit;
            if (row.comment) param.comment = row.comment;
            body.params.push(param);
        });
        e.sources.forEach(function (src) {
            var doc = String(src.doc || '').trim();
            if (!doc) return;
            var source = { doc: doc };
            if (String(src.section || '').trim()) source.section = String(src.section).trim();
            body.sources.push(source);
        });
        var a = e.applies || {};
        var appliesTo = {};
        if (String(a.modelPattern || '').trim()) appliesTo.modelPattern = String(a.modelPattern).trim();
        if (String(a.backendType || '').trim()) appliesTo.backendType = String(a.backendType).trim();
        if (String(a.quantization || '').trim()) appliesTo.quantization = String(a.quantization).trim();
        if (String(a.vramMinMB || '').trim()) appliesTo.vramMinMB = Number(a.vramMinMB);
        if (String(a.vramMaxMB || '').trim()) appliesTo.vramMaxMB = Number(a.vramMaxMB);
        // appliesTo необязателен (решение человека №1): пустой объект не отправляем,
        // иначе запись «общая справка» стала бы «применима ни к чему».
        if (Object.keys(appliesTo).length) body.appliesTo = appliesTo;
        if (!e.isNew && e.revision) body.revision = e.revision;
        return body;
    }

    function saveEditor() {
        var e = state.editor;
        if (!e) return Promise.resolve(false);
        var wrapper = api();
        if (!wrapper) {
            showStatus('error', t('reference.error.no_api',
                'Слой HTTP страницы не загружен (Api.reference) — обновите страницу.'));
            return Promise.resolve(false);
        }
        var id = String(e.id || '').trim().toLowerCase();
        if (!/^[a-z0-9][a-z0-9._-]{2,63}$/.test(id)) {
            showStatus('error', t('reference.editor.invalid_id',
                'Идентификатор должен быть slug из латинских букв, цифр, точки, дефиса и подчёркивания (3–64 символа).'));
            return Promise.resolve(false);
        }
        var body = editorBody();
        if (!body.title) {
            showStatus('error', t('reference.editor.title_required', 'Заголовок обязателен.'));
            return Promise.resolve(false);
        }
        if (!body.params.length) {
            showStatus('error', t('reference.editor.params_required',
                'Нужен хотя бы один параметр: запись-справка без значений бесполезна.'));
            return Promise.resolve(false);
        }
        return wrapper.upsert('recommendations', id, body).then(function (res) {
            var saved = (res && res.recommendation) || {};
            showStatus('ok', t('reference.editor.saved', 'Запись сохранена (revision {value})', {
                value: saved.revision === undefined ? '' : saved.revision
            }));
            closeEditor();
            return load();
        }).catch(function (err) {
            // 409 показываем прямо в форме: оператор не должен потерять набранное.
            showStatus('error', errorText(err));
            return false;
        });
    }

    function deleteRecommendation(id) {
        var wrapper = api();
        if (!wrapper || !id) return Promise.resolve(false);
        var ask = t('reference.rec.confirm_delete', 'Удалить запись «{value}»? Это действие необратимо.', { value: id });
        if (typeof window.confirm === 'function' && !window.confirm(ask)) return Promise.resolve(false);
        return wrapper.remove('recommendations', id).then(function () {
            if (state.editor && state.editor.id === id) closeEditor();
            showStatus('ok', t('reference.rec.deleted', 'Запись «{value}» удалена', { value: id }));
            return load();
        }).catch(function (err) {
            showStatus('error', errorText(err));
            return false;
        });
    }

    /**
     * createTemplateFrom — «Создать шаблон из записи».
     *
     * Превращение делает СЕРВЕР (`POST …/recommendations/{id}/to-template`):
     * params[] → плоский объект в форме тела PUT профиля. В UI второй
     * структуры «набор параметров» не заводим (design §2.4).
     */
    function createTemplateFrom(id) {
        var wrapper = api();
        if (!wrapper || !id) return Promise.resolve(false);
        var rec = null;
        for (var i = 0; i < state.recommendations.length; i++) {
            if (String(state.recommendations[i].id) === String(id)) { rec = state.recommendations[i]; break; }
        }
        var suggested = String(id).toLowerCase().replace(/[^a-z0-9._-]/g, '-');
        var templateId = suggested;
        if (typeof window.prompt === 'function') {
            var answer = window.prompt(t('reference.to_template.prompt_id',
                'Идентификатор нового шаблона (slug):'), suggested);
            if (answer === null) return Promise.resolve(false);
            templateId = String(answer || '').trim() || suggested;
        }
        var name = (rec && rec.title) || templateId;
        if (typeof window.prompt === 'function') {
            var answerName = window.prompt(t('reference.to_template.prompt_name',
                'Название шаблона:'), name);
            if (answerName === null) return Promise.resolve(false);
            name = String(answerName || '').trim() || templateId;
        }
        return wrapper.toTemplate(id, { templateId: templateId, name: name }).then(function () {
            state.tab = 'templates';
            state.tpl.id = templateId;
            state.tpl.preview = null;
            state.tpl.error = null;
            showStatus('ok', t('reference.to_template.created', 'Шаблон «{value}» создан из записи', { value: templateId }));
            return load();
        }).catch(function (err) {
            showStatus('error', errorText(err));
            return false;
        });
    }

    function selectTab(tab) {
        state.tab = tab === 'templates' ? 'templates' : 'recommendations';
        renderTabs();
        renderRecommendationList();
        renderTemplateList();
    }

    function toggleTemplate(id) {
        if (String(state.tpl.id) === String(id)) {
            state.tpl.id = '';
        } else {
            state.tpl.id = String(id);
            state.tpl.preview = null;
            state.tpl.error = null;
            state.tpl.usedByProfiles = null;
            // R515: `usage.usedByProfiles` приходит ВМЕСТЕ с шаблоном
            // (GET …/templates/{id}), а не в списке. Берём при раскрытии и только
            // для того шаблона, который всё ещё раскрыт: подсказка, не данные.
            var wrapper = api();
            if (wrapper && typeof wrapper.get === 'function') {
                wrapper.get('templates', String(id)).then(function (res) {
                    if (String(state.tpl.id) !== String(id)) return;
                    var usage = res && res.usage && Object.prototype.toString.call(res.usage.usedByProfiles) === '[object Array]'
                        ? res.usage.usedByProfiles : [];
                    state.tpl.usedByProfiles = usage;
                    renderTemplateList();
                }).catch(function () { /* подсказка не критична: шаблон уже показан */ });
            }
        }
        renderTemplateList();
    }

    function currentTemplate() {
        return templateById(state.tpl.id);
    }

    function previewTemplate() {
        var wrapper = api();
        var tpl = currentTemplate();
        if (!wrapper || !tpl) return Promise.resolve(false);
        if (!String(state.tpl.model || '').trim()) {
            state.tpl.error = null;
            showStatus('error', gateText('model_required'));
            return Promise.resolve(false);
        }
        state.tpl.busy = true;
        state.tpl.error = null;
        renderTemplateList();
        return wrapper.preview(tpl.id, {
            model: state.tpl.model,
            backendId: state.tpl.backendId || ''
        }).then(function (res) {
            state.tpl.preview = res || null;
            state.tpl.busy = false;
            renderTemplateList();
            return res;
        }).catch(function (err) {
            state.tpl.busy = false;
            state.tpl.preview = null;
            state.tpl.error = err;
            renderTemplateList();
            return false;
        });
    }

    /** applyDirect — режим profile-only: reload не запускается, прогресс не нужен. */
    function applyDirect(tpl, body) {
        var wrapper = api();
        return wrapper.apply(tpl.id, body).then(function (res) {
            showStatus('ok', t('reference.apply.profile_only_done',
                'Профиль модели «{model}» сохранён из шаблона «{tpl}» (revision модели обновлён, модель не перезагружалась).', {
                    model: body.model, tpl: tpl.id
                }));
            if (res && res.warning) showStatus('ok', res.warning);
            return load();
        }).catch(function (err) {
            showStatus('error', errorText(err));
            return false;
        });
    }

    /**
     * applyTemplate — «Применить».
     *
     * Гейт (applyGate) не даёт нажать без модели/бэкенда/загруженной цели.
     * Режим profile+reload идёт через СУЩЕСТВУЮЩУЮ модалку прогресса
     * CppWorkerParams (ей передаётся наша функция применения), profile-only —
     * прямым вызовом: перезагрузки нет, показывать «Reload модели…» было бы
     * враньём. Второго механизма прогресса/перезагрузки здесь нет.
     */
    function applyTemplate() {
        var wrapper = api();
        var tpl = currentTemplate();
        if (!wrapper || !tpl) return Promise.resolve(false);
        var targets = loadedTargetsFor(state.loadedModels, state.tpl.model, state.catalog);
        var gate = applyGate({
            model: state.tpl.model,
            backendId: state.tpl.backendId,
            mode: state.tpl.mode,
            targets: targets
        });
        if (gate.disabled) {
            showStatus('error', gateText(gate.reason));
            return Promise.resolve(false);
        }
        var body = {
            model: String(state.tpl.model).trim(),
            mode: state.tpl.mode
        };
        if (state.tpl.mode === 'profile+reload') body.backendId = String(state.tpl.backendId).trim();
        if (tpl.revision) body.revision = tpl.revision;

        // Вердикт памяти из уже показанного предпросмотра: спрашиваем ЯВНО,
        // иначе 422 придёт уже после нажатия «Применить».
        var memory = state.tpl.preview && state.tpl.preview.memory;
        if (state.tpl.mode === 'profile+reload' && memory && memory.checked && !memory.feasible) {
            var ask = t('reference.apply.confirm_non_feasible',
                'Проверка памяти говорит, что набор не помещается: {message}. Применить всё равно?', {
                    message: memory.message || ''
                });
            if (typeof window.confirm === 'function' && !window.confirm(ask)) return Promise.resolve(false);
            body.confirmNonFeasible = true;
        }

        if (body.mode === 'profile-only') return applyDirect(tpl, body);

        var PPP = window.CppWorkerParams;
        if (!PPP || typeof PPP.applyProfileWithProgress !== 'function') {
            showStatus('error', t('reference.error.no_modal',
                'Модалка прогресса применения (CppWorkerParams) не загружена — обновите страницу.'));
            return Promise.resolve(false);
        }
        var run = function (extra) {
            var payload = {};
            Object.keys(body).forEach(function (key) { payload[key] = body[key]; });
            if (extra && extra.confirmNonFeasible) payload.confirmNonFeasible = true;
            return PPP.applyProfileWithProgress(body.model, body.backendId, function () {
                return wrapper.apply(tpl.id, payload).catch(function (err) {
                    // Модалка показывает err.message — отдаём человеческий текст
                    // (в т.ч. вердикт памяти для 422), а не «HTTP 422: ...».
                    throw new Error(errorText(err));
                });
            });
        };
        return Promise.resolve(run()).then(function () {
            // Async-путь (202) продолжается в модалке; обновление списка безопасно.
            return load();
        }).catch(function (err) {
            showStatus('error', errorText(err));
            return false;
        });
    }

    // =====================================================================
    // Загрузка данных
    // =====================================================================

    /**
     * loadTargets — «где загружена модель» из единственного честного источника.
     *
     * `GET /api/v1/models` для llama.cpp пуст (`models:null`), поэтому
     * используется `Api.clusterModels.loaded()` (design §3.5). Ошибка этого
     * запроса НЕ отменяет показ справочника: без целей кнопка «Применить»
     * просто останется неактивной с объяснением.
     */
    function loadTargets() {
        var models = modelsApi();
        if (!models || typeof models.loaded !== 'function') {
            state.loadedModels = [];
            state.loadedByModel = {};
            return Promise.resolve([]);
        }
        return models.loaded().then(function (res) {
            var list = (res && Object.prototype.toString.call(res.models) === '[object Array]') ? res.models : [];
            state.loadedModels = [];
            state.loadedByModel = {};
            list.forEach(function (m) {
                if (!m || !m.name) return;
                if (state.loadedModels.indexOf(m.name) === -1) state.loadedModels.push(m.name);
                if (!state.loadedByModel[m.name]) state.loadedByModel[m.name] = [];
                if (m.backendId) state.loadedByModel[m.name].push(m.backendId);
            });
            return list;
        }).catch(function () {
            state.loadedModels = [];
            state.loadedByModel = {};
            return [];
        });
    }

    /**
     * loadCatalog — файлы моделей на диске узла (R516).
     *
     * `GET /api/v1/cppworker/model-catalog` читает список у ОДНОГО воркера и (с
     * R516) отдаёт его `backendId`. Без имени узла цели не добавляются: лучше
     * показать меньше целей, чем предложить узел, где файла нет.
     */
    function loadCatalog() {
        var apiLib = window.Api && window.Api.cppworkerLoadDefaults;
        if (!apiLib || typeof apiLib.catalog !== 'function') {
            state.catalog = { backendId: '', files: [] };
            return Promise.resolve(state.catalog);
        }
        return apiLib.catalog().then(function (res) {
            var files = (res && Object.prototype.toString.call(res.files) === '[object Array]') ? res.files : [];
            state.catalog = {
                backendId: String((res && res.backendId) || ''),
                files: files.map(function (f) {
                    return { name: String((f && f.name) || ''), sizeBytes: Number((f && f.sizeBytes) || 0) };
                }).filter(function (f) { return !!f.name; })
            };
            return state.catalog;
        }).catch(function () {
            state.catalog = { backendId: '', files: [] };
            return state.catalog;
        });
    }
    /**
     * load — ПРОВАЙДЕР страницы: обновляет список и детали и возвращает Promise.
     *
     * Именно эту функцию регистрирует `DataRefresh.register('reference', load)`:
     * её вызывают кнопка «Обновить» в шапке и авто-тик. Своей кнопки у страницы
     * нет и быть не должно (правило проекта).
     */
    function load() {
        var wrapper = api();
        if (!wrapper) {
            state.loading = false;
            state.error = null;
            showStatus('error', t('reference.error.no_api',
                'Слой HTTP страницы не загружен (Api.reference) — обновите страницу.'));
            return Promise.resolve(false);
        }
        state.loading = true;
        renderStatus();
        var jobs = [
            wrapper.list('recommendations'),
            wrapper.list('templates'),
            loadTargets(),
            // R516: каталог файлов узла. Отдельный job: его недоступность не
            // должна отменять показ справочника (без каталога целей меньше,
            // и страница честно скажет, что модель нигде не загружена).
            loadCatalog()
        ];
        return Promise.all(jobs).then(function (results) {
            var recs = results[0] || {};
            var tpls = results[1] || {};
            state.recommendations = (Object.prototype.toString.call(recs.items) === '[object Array]') ? recs.items : [];
            state.templates = (Object.prototype.toString.call(tpls.items) === '[object Array]') ? tpls.items : [];
            state.schemaVersion = Number(recs.schemaVersion || tpls.schemaVersion || 0);
            state.loaded = true;
            state.loading = false;
            state.error = null;
            // Раскрытый шаблон мог быть удалён — не держим «призрак».
            if (state.tpl.id && !templateById(state.tpl.id)) {
                state.tpl.id = '';
                state.tpl.preview = null;
                state.tpl.error = null;
            }
            renderAll();
            return results;
        }).catch(function (err) {
            state.loading = false;
            state.error = err;
            renderStatus();
            renderRecommendationList();
            renderTemplateList();
            // Пробрасываем: индикатор свежести в шапке обязан показать ту же
            // причину, а не «Обновлено только что» (data-refresh.js:markError).
            throw err;
        });
    }

    // =====================================================================
    // Редактор шаблона (R515)
    // =====================================================================
    //
    // ЖИВАЯ ЖАЛОБА ОПЕРАТОРА (§2.6 разбора): «шаблоны в „Справке" нельзя
    // править». Ограничение было ТОЛЬКО в UI: сервер умеет всё —
    // PUT/DELETE /api/v1/reference/templates/{id} (internal/api/routes.go:520-522,
    // handlers_reference.go:509-536,614-680), валидация params — :633-647.
    // Страница же звала upsert/remove только для 'recommendations', а у шаблонов
    // было три действия: открыть/предпросмотр/применить.
    //
    // Второго способа записи не заводим: тело PUT шаблона — та же форма, что у
    // профиля модели (плоский объект params), поэтому значение поля приводится
    // по MODEL_PARAM_SPECS тем же coerceValue, что и в редакторе рекомендации.

    function emptyTplEditor() {
        return {
            isNew: true,
            id: '',
            revision: 0,
            name: '',
            version: 1,
            basedOn: '',
            comment: '',
            params: [{ name: '', raw: '' }]
        };
    }

    function tplEditorFromTemplate(tpl) {
        var e = emptyTplEditor();
        e.isNew = false;
        e.id = String(tpl.id || '');
        e.revision = Number(tpl.revision || 0);
        e.name = String(tpl.name || '');
        e.version = Number(tpl.version || 1);
        e.basedOn = String(tpl.basedOn || '');
        e.comment = String(tpl.comment || '');
        e.params = [];
        var params = tpl.params || {};
        Object.keys(params).forEach(function (name) {
            var value = params[name];
            e.params.push({
                name: String(name),
                raw: (value === undefined || value === null) ? '' :
                    (typeof value === 'object' ? JSON.stringify(value) : String(value))
            });
        });
        if (!e.params.length) e.params.push({ name: '', raw: '' });
        return e;
    }

    function openTplEditor(tpl) {
        state.tplEditor = tpl ? tplEditorFromTemplate(tpl) : emptyTplEditor();
        renderTplEditor();
    }

    function closeTplEditor() {
        state.tplEditor = null;
        renderTplEditor();
    }

    /** tplValueInputHtml — контрол значения по метаданным поля (тип/границы/варианты). */
    function tplValueInputHtml(row, index) {
        var spec = paramSpec(row.name);
        var type = (spec && spec.type) || 'text';
        var attrs = ' class="form-control" data-ref-tpledit="paramValue" data-ref-index="' + index + '"';
        if (type === 'bool') {
            var isTrue = String(row.raw).toLowerCase() === 'true';
            return '<select' + attrs + '>' +
                '<option value="false"' + (isTrue ? '' : ' selected') + '>' + esc(t('reference.value.no', 'нет')) + '</option>' +
                '<option value="true"' + (isTrue ? ' selected' : '') + '>' + esc(t('reference.value.yes', 'да')) + '</option>' +
                '</select>';
        }
        if (type === 'enum' && spec && Object.prototype.toString.call(spec.options) === '[object Array]') {
            return '<select' + attrs + '>' + spec.options.map(function (option) {
                return '<option value="' + esc(option) + '"' +
                    (String(row.raw) === String(option) ? ' selected' : '') + '>' + esc(option) + '</option>';
            }).join('') + '</select>';
        }
        var extra = '';
        if (type === 'int') {
            if (spec && spec.min !== undefined) extra += ' min="' + esc(spec.min) + '"';
            if (spec && spec.max !== undefined) extra += ' max="' + esc(spec.max) + '"';
            if (spec && spec.step !== undefined) extra += ' step="' + esc(spec.step) + '"';
            extra += ' type="number"';
        } else {
            extra += ' type="text"';
        }
        return '<input' + attrs + extra + ' value="' + esc(row.raw) + '">';
    }

    /** tplEditorBody — состояние редактора → тело PUT /templates/{id}. */
    function tplEditorBody() {
        var e = state.tplEditor;
        var body = { name: String(e.name || '').trim(), version: Number(e.version) || 1, params: {} };
        var count = 0;
        e.params.forEach(function (row) {
            var name = String(row.name || '').trim();
            if (!name) return;
            var value = coerceValue(paramSpec(name), row.raw);
            if (value === null || value === '') return;
            body.params[name] = value;
            count++;
        });
        if (String(e.comment || '').trim()) body.comment = String(e.comment).trim();
        if (String(e.basedOn || '').trim()) body.basedOn = String(e.basedOn).trim();
        if (!e.isNew && e.revision) body.revision = e.revision;
        return { body: body, params: count };
    }

    function saveTplEditor() {
        var e = state.tplEditor;
        if (!e) return Promise.resolve(false);
        var wrapper = api();
        if (!wrapper) {
            showStatus('error', t('reference.error.no_api',
                'Слой HTTP страницы не загружен (Api.reference) — обновите страницу.'));
            return Promise.resolve(false);
        }
        var id = String(e.id || '').trim().toLowerCase();
        if (!/^[a-z0-9][a-z0-9._-]{2,63}$/.test(id)) {
            showStatus('error', t('reference.tpl_editor.invalid_id',
                'Идентификатор шаблона — slug из латинских букв, цифр, точки, дефиса и подчёркивания (3–64 символа).'));
            return Promise.resolve(false);
        }
        if (!String(e.name || '').trim()) {
            showStatus('error', t('reference.tpl_editor.name_required', 'Название шаблона обязательно.'));
            return Promise.resolve(false);
        }
        var built = tplEditorBody();
        if (!built.params) {
            showStatus('error', t('reference.tpl_editor.params_required',
                'Нужен хотя бы один параметр: шаблон без значений ничего не меняет (сервер ответит params_required).'));
            return Promise.resolve(false);
        }
        return wrapper.upsert('templates', id, built.body).then(function (res) {
            var saved = (res && res.template) || {};
            showStatus('ok', t('reference.tpl_editor.saved', 'Шаблон сохранён (revision {value})', {
                value: saved.revision === undefined ? '' : saved.revision
            }));
            closeTplEditor();
            return load();
        }).catch(function (err) {
            // 409 показываем прямо в форме: набранное не должно потеряться.
            showStatus('error', errorText(err));
            return false;
        });
    }

    function deleteTemplate(id) {
        var wrapper = api();
        if (!wrapper || !id) return Promise.resolve(false);
        var ask = t('reference.tpl_editor.delete_confirm',
            'Удалить шаблон «{value}»? Применённые к моделям значения останутся — удаляется только набор.', { value: id });
        if (typeof window.confirm === 'function' && !window.confirm(ask)) return Promise.resolve(false);
        return wrapper.remove('templates', String(id)).then(function () {
            if (state.tplEditor && String(state.tplEditor.id) === String(id)) closeTplEditor();
            if (String(state.tpl.id) === String(id)) state.tpl.id = '';
            showStatus('ok', t('reference.tpl_editor.deleted', 'Шаблон «{value}» удалён', { value: id }));
            return load();
        }).catch(function (err) {
            showStatus('error', errorText(err));
            return false;
        });
    }

    /** renderTplEditor — форма создания/правки шаблона (host: #referenceTplEditor). */
    function renderTplEditor() {
        var host = byId('referenceTplEditor');
        if (!host) return;
        var e = state.tplEditor;
        if (!e) {
            host.hidden = true;
            host.innerHTML = '';
            return;
        }
        host.hidden = false;
        var specs = paramSpecs();
        var rows = e.params.map(function (row, index) {
            var nameSelect = '<select class="form-control" data-ref-tpledit="paramName" data-ref-index="' + index + '">' +
                '<option value="">' + esc(t('reference.tpl_editor.pick_param', '— поле профиля —')) + '</option>' +
                specs.map(function (spec) {
                    return '<option value="' + esc(spec.field) + '"' +
                        (String(row.name) === String(spec.field) ? ' selected' : '') + '>' +
                        esc(spec.field) + ' — ' + esc(specLabel(spec)) + '</option>';
                }).join('') +
                (row.name && !paramSpec(row.name)
                    ? '<option value="' + esc(row.name) + '" selected>' + esc(row.name) + '</option>' : '') +
                '</select>';
            return '<tr>' +
                '<td>' + nameSelect + '</td>' +
                '<td>' + tplValueInputHtml(row, index) + '</td>' +
                '<td><button type="button" class="btn btn-danger btn-sm" data-ref-action="tpl-editor-del-param" ' +
                    'data-ref-index="' + index + '">×</button></td>' +
                '</tr>';
        }).join('');
        host.innerHTML = '<div class="reference-editor-card">' +
            '<h4 class="reference-card-title">' +
                esc(e.isNew ? t('reference.tpl_editor.new', 'Новый шаблон')
                    : t('reference.tpl_editor.edit', 'Правка шаблона «{value}»', { value: e.id })) +
            '</h4>' +
            '<div class="reference-tpl-controls">' +
                '<label class="reference-field"><span>' + esc(t('reference.tpl_editor.id', 'Идентификатор (slug)')) + '</span>' +
                    '<input class="form-control" type="text" data-ref-tpledit="id" value="' + esc(e.id) + '"' +
                        (e.isNew ? '' : ' readonly') + '></label>' +
                '<label class="reference-field"><span>' + esc(t('reference.tpl_editor.name', 'Название')) + '</span>' +
                    '<input class="form-control" type="text" data-ref-tpledit="name" value="' + esc(e.name) + '"></label>' +
                '<label class="reference-field"><span>' + esc(t('reference.tpl_editor.version', 'Версия содержимого')) + '</span>' +
                    '<input class="form-control" type="number" min="1" step="1" data-ref-tpledit="version" value="' + esc(e.version) + '"></label>' +
                '<label class="reference-field"><span>' + esc(t('reference.tpl_editor.based_on', 'Из записи (id, необязательно)')) + '</span>' +
                    '<input class="form-control" type="text" data-ref-tpledit="basedOn" value="' + esc(e.basedOn) + '"></label>' +
            '</div>' +
            '<label class="reference-field"><span>' + esc(t('reference.tpl_editor.comment', 'Когда годится (необязательно)')) + '</span>' +
                '<input class="form-control" type="text" data-ref-tpledit="comment" value="' + esc(e.comment) + '"></label>' +
            '<table class="reference-table"><thead><tr>' +
                '<th>' + esc(t('reference.tpl_editor.param', 'Параметр')) + '</th>' +
                '<th>' + esc(t('reference.tpl_editor.value', 'Значение')) + '</th>' +
                '<th></th>' +
            '</tr></thead><tbody>' + rows + '</tbody></table>' +
            '<div class="reference-tpl-actions">' +
                '<button type="button" class="btn btn-secondary" data-admin-only data-ref-action="tpl-editor-add-param">' +
                    esc(t('reference.tpl_editor.add_param', 'Добавить параметр')) + '</button>' +
                '<button type="button" class="btn btn-primary" data-admin-only data-ref-action="tpl-editor-save">' +
                    esc(t('reference.tpl_editor.save', 'Сохранить')) + '</button>' +
                '<button type="button" class="btn btn-secondary" data-ref-action="tpl-editor-cancel">' +
                    esc(t('common.cancel', 'Отмена')) + '</button>' +
                (e.isNew ? '' :
                    '<button type="button" class="btn btn-danger" data-admin-only data-ref-action="tpl-editor-delete" ' +
                        'data-ref-id="' + esc(e.id) + '">' + esc(t('reference.tpl_editor.delete', 'Удалить')) + '</button>') +
            '</div>' +
            '</div>';
    }

    function applyTplEditorInput(field, index, input) {
        var e = state.tplEditor;
        if (!e || !field) return;
        var value = input && input.type === 'checkbox' ? !!input.checked : (input ? input.value : '');
        if (field === 'id') { e.id = String(value); return; }
        if (field === 'name') { e.name = String(value); return; }
        if (field === 'version') { e.version = Number(value) || 1; return; }
        if (field === 'basedOn') { e.basedOn = String(value); return; }
        if (field === 'comment') { e.comment = String(value); return; }
        if (isNaN(index)) return;
        if (field === 'paramName') {
            if (!e.params[index]) return;
            e.params[index].name = String(value);
            var spec = paramSpec(e.params[index].name);
            if (spec && spec.unit && !e.params[index].unit) e.params[index].unit = spec.unit;
            // Тип поля изменился — перерисовать строку, чтобы получить нужный
            // контрол (number/select). Это действие оператора, не тик.
            renderTplEditor();
            return;
        }
        if (field === 'paramValue') { if (e.params[index]) e.params[index].raw = String(value); return; }
    }

    // =====================================================================
    // Монтирование и обработчики
    // =====================================================================

    function closestAttr(node, attr) {
        var el = node;
        while (el) {
            if (el.getAttribute && el.getAttribute(attr) !== null) return el;
            el = el.parentNode || null;
        }
        return null;
    }

    function onClick(ev) {
        var host = ev && ev.target;
        var btn = closestAttr(host, 'data-ref-action');
        var tabBtn = closestAttr(host, 'data-ref-tab');
        if (tabBtn && !btn) {
            selectTab(tabBtn.getAttribute('data-ref-tab'));
            return;
        }
        if (!btn) return;
        var action = btn.getAttribute('data-ref-action');
        var id = btn.getAttribute('data-ref-id') || '';
        var index = Number(btn.getAttribute('data-ref-index'));
        if (action === 'new-rec') { openEditor(null); return; }
        if (action === 'edit-rec') {
            for (var i = 0; i < state.recommendations.length; i++) {
                if (String(state.recommendations[i].id) === id) { openEditor(state.recommendations[i]); return; }
            }
            return;
        }
        if (action === 'delete-rec') { deleteRecommendation(id); return; }
        if (action === 'to-template') { createTemplateFrom(id); return; }
        if (action === 'toggle-tpl') { toggleTemplate(id); return; }
        // R515: правка шаблонов — через тот же PUT/DELETE, что уже был в API.
        if (action === 'new-tpl') { openTplEditor(null); return; }
        if (action === 'edit-tpl') {
            var tpl = templateById(id);
            if (tpl) openTplEditor(tpl);
            return;
        }
        if (action === 'delete-tpl') { deleteTemplate(id); return; }
        if (action === 'tpl-editor-save') { saveTplEditor(); return; }
        if (action === 'tpl-editor-cancel') { closeTplEditor(); return; }
        if (action === 'tpl-editor-delete') {
            if (state.tplEditor) deleteTemplate(state.tplEditor.id);
            return;
        }
        if (action === 'tpl-editor-add-param') {
            if (!state.tplEditor) return;
            state.tplEditor.params.push({ name: '', raw: '' });
            renderTplEditor();
            return;
        }
        if (action === 'tpl-editor-del-param') {
            if (!state.tplEditor || isNaN(index)) return;
            state.tplEditor.params.splice(index, 1);
            if (!state.tplEditor.params.length) state.tplEditor.params.push({ name: '', raw: '' });
            renderTplEditor();
            return;
        }
        if (action === 'preview') { previewTemplate(); return; }
        if (action === 'apply') { applyTemplate(); return; }
        if (action === 'editor-add-param') {
            if (!state.editor) return;
            state.editor.params.push({ name: '', raw: '', unit: '', comment: '' });
            renderEditor();
            return;
        }
        if (action === 'editor-del-param') {
            if (!state.editor || isNaN(index)) return;
            state.editor.params.splice(index, 1);
            renderEditor();
            return;
        }
        if (action === 'editor-add-source') {
            if (!state.editor) return;
            state.editor.sources.push({ doc: '', section: '' });
            renderEditor();
            return;
        }
        if (action === 'editor-del-source') {
            if (!state.editor || isNaN(index)) return;
            state.editor.sources.splice(index, 1);
            renderEditor();
            return;
        }
        if (action === 'editor-save') { saveEditor(); return; }
        if (action === 'editor-delete') {
            if (state.editor) deleteRecommendation(state.editor.id);
            return;
        }
        if (action === 'editor-cancel') { closeEditor(); return; }
    }

    function applyEditorInput(field, index, input) {
        var e = state.editor;
        if (!e || !field) return;
        var value = input && input.type === 'checkbox' ? !!input.checked : (input ? input.value : '');
        if (field === 'id') { e.id = String(value); return; }
        if (field === 'title') { e.title = String(value); return; }
        if (field === 'comment') { e.comment = String(value); return; }
        if (field === 'modelPattern' || field === 'backendType' || field === 'quantization' ||
            field === 'vramMinMB' || field === 'vramMaxMB') {
            e.applies[field] = String(value);
            return;
        }
        if (isNaN(index)) return;
        if (field === 'paramName') {
            if (!e.params[index]) return;
            e.params[index].name = String(value);
            var spec = paramSpec(e.params[index].name);
            if (spec && spec.unit && !e.params[index].unit) e.params[index].unit = spec.unit;
            // Тип поля изменился — перерисовать именно строку, чтобы получить
            // правильный контрол (number/select). Фокус уходит, но это действие
            // оператора, а не тик авто-обновления.
            renderEditor();
            return;
        }
        if (field === 'paramValue') { if (e.params[index]) e.params[index].raw = String(value); return; }
        if (field === 'paramUnit') { if (e.params[index]) e.params[index].unit = String(value); return; }
        if (field === 'paramComment') { if (e.params[index]) e.params[index].comment = String(value); return; }
        if (field === 'sourceDoc') { if (e.sources[index]) e.sources[index].doc = String(value); return; }
        if (field === 'sourceSection') { if (e.sources[index]) e.sources[index].section = String(value); return; }
    }

    function onInput(ev) {
        var target = ev && ev.target;
        if (!target || !target.getAttribute) return;
        // R515: поля редактора шаблона идут своим атрибутом — иначе их значение
        // попадало бы в состояние редактора РЕКОМЕНДАЦИИ (общий data-ref-edit).
        var tplEditField = target.getAttribute('data-ref-tpledit');
        if (tplEditField) {
            var tplEditIndex = Number(target.getAttribute('data-ref-index'));
            applyTplEditorInput(tplEditField, isNaN(tplEditIndex) ? NaN : tplEditIndex, target);
            return;
        }
        if (target.id === 'referenceSearch') {
            state.filter.search = String(target.value || '');
            renderRecommendationList();
            return;
        }
        if (target.id === 'referenceModelFilter') {
            state.filter.model = String(target.value || '');
            renderRecommendationList();
            return;
        }
        var tplField = target.getAttribute('data-ref-tpl');
        if (tplField) {
            if (tplField === 'model') {
                state.tpl.model = String(target.value || '');
                // Смена модели сбрасывает бэкенд: он относился к прежней модели.
                state.tpl.backendId = '';
                state.tpl.preview = null;
                state.tpl.error = null;
            } else if (tplField === 'backendId') {
                state.tpl.backendId = String(target.value || '');
                state.tpl.preview = null;
            } else if (tplField === 'mode') {
                state.tpl.mode = String(target.value || 'profile+reload');
            }
            renderTemplateList();
            return;
        }
        var editField = target.getAttribute('data-ref-edit');
        if (editField) {
            applyEditorInput(editField, Number(target.getAttribute('data-ref-index')), target);
        }
    }

    /**
     * mount — привязать страницу (идемпотентно).
     *
     * Возвращает false на повторном вызове: refreshPage вызывается и тиком
     * авто-обновления, поэтому монтирование обязано быть безопасным к повтору
     * (правило из app.js → case 'gguf': DOM не пересобирается на каждом тике).
     */
    function mount(root) {
        var host = (root && typeof root === 'object') ? root : byId('reference-page');
        if (!host) return false;
        if (state.mounted) return false;
        state.mounted = true;
        state.root = host;
        if (typeof host.addEventListener === 'function') {
            host.addEventListener('click', onClick);
            host.addEventListener('input', onInput);
            host.addEventListener('change', onInput);
        }
        renderAll();
        // Первичная загрузка: страница открыта, данных ещё нет. Дальше данные
        // обновляет только провайдер (кнопка/тик).
        load().catch(function () { /* текст ошибки уже на странице */ });
        return true;
    }

    // =====================================================================
    // Регистрация провайдера обновления — ОДНА кнопка на всё приложение
    // =====================================================================

    var registered = false;

    function registerProvider() {
        if (registered) return true;
        if (!window.DataRefresh || typeof window.DataRefresh.register !== 'function') return false;
        registered = true;
        // Литерал 'reference' важен: по нему статический тест
        // (webui/tests/reference-page-static.test.js) доказывает, что страница
        // не завела свою кнопку обновления, а зарегистрировала провайдера.
        window.DataRefresh.register('reference', function () {
            return load();
        });
        return true;
    }

    window.ReferencePage = {
        mount: mount,
        load: load,
        selectTab: selectTab,
        registerProvider: registerProvider,
        // Чистые помощники — их проверяет webui/js/modules/reference-page.test.js.
        renderRecommendationRow: renderRecommendationRow,
        renderParamsTable: renderParamsTable,
        renderTemplateCard: renderTemplateCard,
        renderTemplateDetail: renderTemplateDetail,
        renderPreview: renderPreview,
        renderMemory: renderMemory,
        renderTargets: renderTargets,
        filterRecommendations: filterRecommendations,
        matchRecommendation: matchRecommendation,
        globMatch: globMatch,
        appliesToChips: appliesToChips,
        loadedTargetsFor: loadedTargetsFor,
        // R516: состав выбора модели/узла (проверяется reference-targets-catalog.test.js).
        modelChoices: modelChoices,
        backendChoices: backendChoices,
        loadCatalog: loadCatalog,
        applyGate: applyGate,
        gateText: gateText,
        formatParamValue: formatParamValue,
        formatDiffValue: formatDiffValue,
        formatDiffRow: formatDiffRow,
        renderDiff: renderDiff,
        errorInfo: errorInfo,
        errorText: errorText,
        paramSpecs: paramSpecs,
        renderEditor: renderEditor,
        // R515: редактор шаблона (проверяется webui/tests/reference-templates-editor.test.js).
        renderTplEditor: renderTplEditor,
        tplEditorBody: tplEditorBody,
        openTplEditor: openTplEditor,
        closeTplEditor: closeTplEditor,
        saveTplEditor: saveTplEditor,
        deleteTemplate: deleteTemplate,
        tplValueInputHtml: tplValueInputHtml,
        _state: state
    };

    // Регистрируемся сразу и повторяем на DOMContentLoaded: файл мог выполниться
    // раньше, чем ожил реестр data-refresh.js. Обе попытки идемпотентны.
    registerProvider();
    if (typeof document !== 'undefined' && document.addEventListener) {
        document.addEventListener('DOMContentLoaded', registerProvider);
    }
})();

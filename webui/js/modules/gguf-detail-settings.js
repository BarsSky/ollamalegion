// gguf-detail-settings.js — R657 (2026-11-20): таб «Settings» правой панели
// (GGUF), RPC-состояние/пиры и редакторы параметров модели вынесены из
// gguf-renderer-detail-render.js (цель §5 — < 1500 строк).
//
// ЗАЧЕМ ВЫНЕСЕНО. Один связный слой: форма опций бэкенда и её сохранение,
// RPC-сервер и пиры (R172), редактор параметров модели с наследованием значений
// (R126/R134/R229), «чем ограничено окно» (R319), профили в настройках и модалка
// подключения. Он пишет/валидирует значения, а не только рисует.
//
// ПОЧЕМУ ФАБРИКА. Слой берёт из основного модуля M/state, Utils, I18N,
// PARAM_SOURCE_LABELS и formatElapsedShort; список посчитан
// scripts/webui_extract_deps.js. Все объявленные здесь функции и MODEL_PARAM_SPECS
// возвращаются наружу: основной файл назначает их в M.* и вызывает из соседних табов.
//
// ПОВЕДЕНИЕ НЕ МЕНЯЛОСЬ: тела перенесены как есть, отступ сохранён.
(function () {
    'use strict';

    window.GgufDetailSettings = function (ctx) {
        var M = ctx.M;
        var _ = ctx._;
        var state = ctx.state;
        // Перечни проверенных режимов объявлены в основном файле (R118) — передаём явно.
        var GGUF_VERIFIED_SPLIT_MODES = ctx.GGUF_VERIFIED_SPLIT_MODES;
        var GGUF_VERIFIED_RPC_BACKENDS = ctx.GGUF_VERIFIED_RPC_BACKENDS;
        var I18N = ctx.I18N || (typeof window !== 'undefined' ? window.I18N : undefined) ||
            { t: function (k) { return k; }, getLang: function () { return 'en'; } };
        // showToast в WebUI живёт как M.showToast / window.showToast (app-core.js).
        // Резолвим В МОМЕНТ ВЫЗОВА: тесты подменяют хост тоста позже загрузки модуля.
        var showToast = function () {
            var t = ctx.showToast
                || (M && M.showToast)
                || (typeof window !== 'undefined' && window.showToast)
                || (typeof globalThis !== 'undefined' && globalThis.showToast);
            if (typeof t === 'function') return t.apply(null, arguments);
        };
        var Utils = ctx.Utils;
        var I18N = ctx.I18N;
        var PARAM_SOURCE_LABELS = ctx.PARAM_SOURCE_LABELS;
        var formatElapsedShort = ctx.formatElapsedShort;

    function renderSettingsPane() {
        const backend = M.currentBackend();
        const backendId = backend ? Utils.escapeHtml(backend.id) : '';
        const isRegistered = !!(backend && backend.url && backend.url.indexOf('http') === 0 && backend.id && /^(?:[a-z0-9_-]+)$/i.test(backend.id));
        // Round 32 #6 (2026-08-10): используем кэшированный HTML формы вместо
        // spinner-плейсхолдера, если форма уже отрендерена для этого бэкенда.
        // Без этого refreshDetailPane() (вызывается из active-queries polling
        // каждые 3s) затирал форму на «Loading…», и пользователь видел её
        // на краткий миг, потом она исчезала.
        const cached = backend && state._settingsFormHtmlByBackend[backend.id];
        const backendOptionsInner = cached
            ? cached.html
            : '<div class="loading"><i class="fas fa-spinner fa-spin"></i> ' + (window.I18N ? I18N.t('common.loading', 'Loading…') : 'Loading…') + '</div>';
        return '' +
            '<div class="gguf-settings-form">' +

            // ---------- (1) Backend load options ----------
            '<div class="settings-subsection" id="gguf-backend-options-section">' +
                '<h4 style="margin:0 0 6px 0;font-size:var(--font-size-base);display:flex;align-items:center;gap:8px;">' +
                    '<i class="fas fa-sliders-h"></i> ' +
                    '<span>' + _('gguf.backend_options_title') + '</span>' +
                '</h4>' +
                '<p style="color:var(--text-muted);margin:0 0 6px 0;font-size:var(--font-size-xs);">' +
                    _('gguf.backend_options_desc') +
                '</p>' +
                // Round 32 #6 (2026-08-10): disclaimer что это ГЛОБАЛЬНЫЕ дефолты,
                // которые применяются при загрузке ЛЮБОЙ модели на этом бэкенде.
                // Per-model overrides — в Per-Model Profiles ниже. Без этого
                // disclaimer'а пользователи думали что settings «привязаны к
                // модели» или «не работают без загруженной модели».
                '<p style="color:var(--text-muted);margin:0 0 12px 0;font-size:var(--font-size-xs);font-style:italic;">' +
                    '<i class="fas fa-info-circle"></i> ' +
                    Utils.escapeHtml(_('gguf.backend_options_global_hint', 'Эти параметры — глобальные дефолты для всех моделей на бэкенде. Per-model overrides — в секции «Per-Model Profiles» ниже. Доступны даже без загруженной модели.')) +
                '</p>' +
                '<div id="ggufBackendOptionsContainer" data-backend-id="' + backendId + '">' +
                    backendOptionsInner +
                '</div>' +
            '</div>' +

            // ---------- (2) Per-Model Profiles ----------
            '<div class="settings-subsection" id="gguf-profiles-section" style="margin-top:24px;padding-top:16px;border-top:1px solid var(--border-color, #2a3441);">' +
                '<h4 style="margin:0 0 6px 0;font-size:var(--font-size-base);display:flex;align-items:center;gap:8px;">' +
                    '<i class="fas fa-layer-group"></i> ' +
                    '<span>' + _('gguf.profiles_section_title') + '</span>' +
                '</h4>' +
                '<p style="color:var(--text-muted);margin:0 0 12px 0;font-size:var(--font-size-xs);">' +
                    _('gguf.profiles_section_desc') +
                '</p>' +
                (isRegistered
                    ? ('<div id="cppProfilesList">' +
                        '<div class="cpp-profiles-empty">' + (window.I18N ? I18N.t('settings.profiles.empty', '') : '') + '</div>' +
                       '</div>' +
                       '<button class="btn btn-primary" id="cppProfileAddBtn" style="margin-top:8px;">' +
                           '<i class="fas fa-plus"></i> ' +
                           '<span>' + (window.I18N ? I18N.t('settings.profiles.add', 'Add profile…') : 'Add profile…') + '</span>' +
                       '</button>')
                    : ('<div class="gguf-empty-detail" style="padding:12px;text-align:left;">' +
                        '<i class="fas fa-info-circle"></i> ' + _('gguf.profiles_only_registered') +
                       '</div>')) +
            '</div>' +

            // ---------- (3) R106b: что получат НОВЫЕ модели и что применится к
            // каждой модели в папке. Раньше этот слой был доступен только на
            // странице «Настройки», поэтому на странице GGUF оператор правил
            // настройки контейнера и не видел, что при загрузке выигрывают
            // профиль модели и «настройки по умолчанию» — отсюда жалоба «модель
            // грузится не с теми параметрами, что указаны в настройках».
            '<div class="settings-subsection" id="gguf-load-defaults-section" style="margin-top:24px;padding-top:16px;border-top:1px solid var(--border-color, #2a3441);">' +
                '<h4 style="margin:0 0 6px 0;font-size:var(--font-size-base);display:flex;align-items:center;gap:8px;">' +
                    '<i class="fas fa-crown"></i> ' +
                    '<span>' + Utils.escapeHtml(_('gguf.load_defaults_title', 'Что получат НОВЫЕ модели (приоритетнее настроек контейнера)')) + '</span>' +
                '</h4>' +
                '<p style="color:var(--text-muted);margin:0 0 10px 0;font-size:var(--font-size-xs);">' +
                    Utils.escapeHtml(_('gguf.load_defaults_desc',
                        'Порядок применения при загрузке: настройки этого слоя (и профиль конкретной модели) важнее значений из формы выше. Кнопка «Сделать приоритетом для новых моделей» переносит значения формы сюда.')) +
                '</p>' +
                '<div data-role="cpp-load-defaults">' +
                    '<div class="loading">' + Utils.escapeHtml(_('common.loading', 'Загрузка…')) + '</div>' +
                '</div>' +
                '<h4 style="margin:20px 0 6px 0;font-size:var(--font-size-base);display:flex;align-items:center;gap:8px;">' +
                    '<i class="fas fa-hard-drive"></i> ' +
                    '<span>' + Utils.escapeHtml(_('gguf.model_catalog_title', 'Модели в папке — что применится к каждой')) + '</span>' +
                '</h4>' +
                '<p style="color:var(--text-muted);margin:0 0 10px 0;font-size:var(--font-size-xs);">' +
                    Utils.escapeHtml(_('gguf.model_catalog_desc',
                        'Включая ещё не загруженные. «Свой профиль» = у модели есть собственные настройки — они важнее и настроек по умолчанию, и настроек контейнера. Кнопка с ползунками открывает профиль этой модели (там же «Применить (reload)» для уже загруженной).')) +
                '</p>' +
                '<div data-role="cpp-model-catalog">' +
                    '<div class="loading">' + Utils.escapeHtml(_('common.loading', 'Загрузка…')) + '</div>' +
                '</div>' +
            '</div>' +

            '</div>';
    }

    // ---- Backend options load/save ----

    // formState — общий слой «пересборка панели не стирает ввод» (R688).
    // Резолвится в момент вызова: модуль подключается отдельным тегом, а тесты
    // поднимают фабрику без него — отсутствие не должно ронять рендер формы.
    function formState() {
        return (typeof window !== 'undefined' && window.GgufDetailFormState) || null;
    }

    /**
     * rebaselinePanel — R689: сообщить слою защиты, что панель уже показывает
     * актуальный вид после ПРЯМОЙ дорисовки формы.
     *
     * ЗАЧЕМ. Форма параметров загрузки рисуется напрямую в
     * #ggufBackendOptionsContainer (и кэшируется в state._settingsFormHtmlByBackend).
     * С этого момента рендер панели впервые содержит форму, а не заглушку
     * «Загрузка…». Без rebaseline следующий авто-проход считает это изменением
     * данных, пересобирает панель — и уничтожает профили/defaults/catalog,
     * которые к тому моменту уже приехали (живой замер: высота панели
     * 4268 → 1935 px, блоки снова «Загрузка…»). После rebaseline пересборки нет:
     * страница уже показывает ровно то, что нарисовал бы рендер.
     */
    function rebaselinePanel() {
        var G = formState();
        if (!G || typeof G.rebaseline !== 'function') { return false; }
        if (typeof M.renderDetailPanel === 'function') {
            var panel = document.getElementById('ggufDetailPanel');
            if (panel) G.rebaseline(panel, M.renderDetailPanel());
        }
        if (typeof M.renderDetailPane === 'function') {
            var pane = document.getElementById('ggufDetailContent');
            if (pane) G.rebaseline(pane, M.renderDetailPane());
        }
        return true;
    }

    /**
     * Загрузить текущие llama.cpp-дефолты выбранного cppworker'а и отрендерить
     * редактируемую форму внутри #ggufBackendOptionsContainer.
     *
     * Round 32 #3 fix (2026-08-10): при успешной загрузке конфига — также
     * синхронизировать `state.loadOptions` с бэкенд-дефолтами. Без этого фикса
     * `loadOnSelectedBackend` использовал хардкод `state.loadOptions = {ctxSize:2048,
     * gpuLayers:-1, batchSize:512}` (см. state-initialization вверху файла),
     * НЕЗАВИСИМО от того, что показано в форме Settings. После Save пользователь
     * видел форму с 4096, но клик Load применял 2048 → путаница + silent override.
     *
     * Round 32 #5 fix (2026-08-10): добавлен elapsed-time в loading spinner
     * и более информативный error message. Без этого пользователь видел
     * "infinite loading" когда cppworker был недоступен (10s+ balancer retry
     * с backoff 1+2+4=7s + сам connect timeout 90s) — общий fetch timeout
     * webui (10s) не успевал abort'ить запрос, и спиннер крутился пока
     * balancer не возвращал 502 (10-12s). Теперь показывается "(прошло Xs)"
     * каждые 2 секунды + понятное сообщение об ошибке с подсказкой проверить
     * статус cppworker контейнера.
     */
    async function loadAndRenderBackendOptions() {
        const container = document.getElementById('ggufBackendOptionsContainer');
        if (!container) return;
        const backend = M.currentBackend();
        if (!backend) {
            container.innerHTML = '<div class="gguf-empty-state">' + (window.I18N ? I18N.t('gguf.no_backend_selected') : '') + '</div>';
            return;
        }
        // R688: спиннер ниже заменяет форму через innerHTML, поэтому раскрытые
        // секции запоминаем ДО него, а применяем после отрисовки формы. Без этого
        // каждое открытие/перечитывание формы схлопывало секции, которые оператор
        // только что раскрыл.
        var G = formState();
        if (G && typeof G.rememberOpen === 'function') G.rememberOpen(container);
        const t0 = Date.now();
        // Spinner с elapsed-time обновлением каждые 2s. Если load >5s — пользователь
        // видит "Loading... (5s)" вместо "вечно крутящегося спиннера".
        let elapsedTimer = null;
        function updateSpinner() {
            const elapsed = ((Date.now() - t0) / 1000).toFixed(1);
            container.innerHTML = '<div class="loading"><i class="fas fa-spinner fa-spin"></i> ' +
                Utils.escapeHtml(_('common.loading', 'Loading…')) + ' (' + elapsed + 's)' +
                '<div style="margin-top:4px;font-size:var(--font-size-xs);color:var(--text-muted);">' +
                Utils.escapeHtml(backend.id) +
                '</div></div>';
        }
        updateSpinner();
        elapsedTimer = setInterval(updateSpinner, 2000);
        // R60.5 (2026-09-07): Hard timeout 15s. requestViaBackend имеет 10s
        // AbortController, но если fetch зависает (например, не отвечает balancer
        // proxy), спиннер может крутиться вечно. Дополнительный внешний таймаут
        // гарантирует что пользователь увидит ошибку.
        const hardTimeout = setTimeout(() => {
            if (elapsedTimer) clearInterval(elapsedTimer);
            const elapsedSec = ((Date.now() - t0) / 1000).toFixed(1);
            container.innerHTML =
                '<div class="gguf-empty-state" style="color:var(--danger);padding:12px;">' +
                    '<div style="display:flex;align-items:center;gap:8px;margin-bottom:8px;">' +
                        '<i class="fas fa-exclamation-triangle"></i>' +
                        '<strong>' + Utils.escapeHtml(_('gguf.config_load_failed', 'Не удалось загрузить параметры бэкенда')) + '</strong>' +
                    '</div>' +
                    '<div style="font-family:monospace;font-size:var(--font-size-xs);background:rgba(0,0,0,0.2);padding:8px;border-radius:4px;margin-bottom:8px;word-break:break-all;">' +
                        'R60.5 hard timeout 15s — fetch завис, проверьте balancer proxy' +
                    '</div>' +
                    '<div style="display:flex;gap:6px;">' +
                        '<button class="btn btn-secondary btn-sm" id="ggufBackendOptionsReload">' +
                            '<i class="fas fa-sync"></i> ' + Utils.escapeHtml(_('gguf.reload_backend_options')) +
                        '</button>' +
                        '<span style="font-size:var(--font-size-xs);color:var(--text-muted);align-self:center;">' +
                            '(' + elapsedSec + 's)' +
                        '</span>' +
                    '</div>' +
                '</div>';
            const retry = document.getElementById('ggufBackendOptionsReload');
            if (retry) retry.addEventListener('click', loadAndRenderBackendOptions);
        }, 15000);
        try {
            const data = await GgufApi.requestViaBackend(backend.id, '/api/v1/cppworker/config');
            if (elapsedTimer) clearInterval(elapsedTimer);
            clearTimeout(hardTimeout);
            const cfg = (data && data.config) || {};
            // R125 (2026-10-16, пункт 3): какие узлы раскладки РЕАЛЬНО отвечают.
            // Отдельный запрос (проверка узлов — сетевой ввод-вывод с таймаутом) и
            // best-effort: без него секция честно скажет «не проверялось», а не
            // «донор не активен» из-за отсутствия данных.
            let rpcPeers = null;
            try {
                rpcPeers = await GgufApi.requestViaBackend(backend.id, '/api/v1/cppworker/rpc-peers');
            } catch (peersErr) {
                console.warn('[gguf-renderer] rpc-peers недоступны:', peersErr);
            }
            // === Round 32 #3: sync state.loadOptions с бэкенд-дефолтами ===
            if (!state._loadOptionsCustom) state._loadOptionsCustom = {};
            if (!state._loadOptionsCustom[backend.id]) {
                syncLoadOptionsFromConfig(cfg);
            }
            // Round 32 #6 (2026-08-10): кэшируем отрендеренный HTML формы, чтобы
            // он не затирался на spinner-плейсхолдер при последующих
            // refreshDetailPane() (active-queries polling каждые 3s).
            // R60.5 (2026-09-07): try/catch вокруг render — если Utils.escapeHtml
            // или другое выражение throws, показываем ошибку вместо вечного спиннера.
            let formHtml;
            try {
                formHtml = renderBackendOptionsForm(cfg, backend, data, rpcPeers);
            } catch (renderErr) {
                if (elapsedTimer) clearInterval(elapsedTimer);
                const elapsedSec = ((Date.now() - t0) / 1000).toFixed(1);
                const renderMsg = renderErr && renderErr.message ? renderErr.message : String(renderErr);
                console.error('[gguf-renderer] renderBackendOptionsForm failed:', renderErr);
                container.innerHTML =
                    '<div class="gguf-empty-state" style="color:var(--danger);padding:12px;">' +
                        '<div style="display:flex;align-items:center;gap:8px;margin-bottom:8px;">' +
                            '<i class="fas fa-exclamation-triangle"></i>' +
                            '<strong>Ошибка рендера формы (R60.5)</strong>' +
                        '</div>' +
                        '<div style="font-family:monospace;font-size:var(--font-size-xs);background:rgba(0,0,0,0.2);padding:8px;border-radius:4px;margin-bottom:8px;word-break:break-all;">' +
                            Utils.escapeHtml(renderMsg) +
                        '</div>' +
                        '<div style="font-size:var(--font-size-xs);color:var(--text-muted);margin-bottom:8px;">' +
                            'Config получен: ' + Object.keys(cfg).length + ' полей. См. console.error для stack trace.' +
                        '</div>' +
                        '<button class="btn btn-secondary btn-sm" id="ggufBackendOptionsReload">' +
                            '<i class="fas fa-sync"></i> Retry' +
                        '</button>' +
                    '</div>';
                const retry = document.getElementById('ggufBackendOptionsReload');
                if (retry) retry.addEventListener('click', loadAndRenderBackendOptions);
                return;
            }
            if (!state._settingsFormHtmlByBackend) state._settingsFormHtmlByBackend = {};
            state._settingsFormHtmlByBackend[backend.id] = { html: formHtml, cfg: cfg, ts: Date.now() };
            container.innerHTML = formHtml;
            // R688: свежая форма — новые узлы. Объявляем их значения базовыми
            // (иначе первые же серверные значения выглядели бы «правкой
            // оператора» и навсегда блокировали пересборку панели) и возвращаем
            // раскрытые секции, запомненные до спиннера.
            if (G && typeof G.afterRender === 'function') G.afterRender(container);
            // R689: форма нарисована напрямую — сообщаем об этом слою защиты,
            // иначе ближайший авто-проход пересоберёт панель и снова превратит
            // профили/defaults/catalog в «Загрузка…» (см. rebaselinePanel).
            rebaselinePanel();
            // Round 35c+ (2026-08-13): init gpu_layers live badge (после render, чтобы
            // badge отражал текущее значение gpu, а не дефолт "AUTO" в HTML).
            if (Utils.updateGpuLayersBadge) {
                Utils.updateGpuLayersBadge('ggufOptGpuLayers', 'ggufOptGpuLayersBadge');
            }
            // Привязываем handlers
            const reloadBtn = container.querySelector('#ggufBackendOptionsReload');
            if (reloadBtn) reloadBtn.addEventListener('click', loadAndRenderBackendOptions);
            const saveBtn = container.querySelector('#ggufBackendOptionsSave');
            if (saveBtn) saveBtn.addEventListener('click', saveBackendOptions);
            // R106b (2026-10-12): перенос значений формы в приоритетный слой.
            const promoteBtn = container.querySelector('#ggufBackendOptionsPromote');
            if (promoteBtn) promoteBtn.addEventListener('click', promoteBackendOptionsToDefaults);
            const elapsedSec = ((Date.now() - t0) / 1000).toFixed(1);
            showToast(_('gguf.backend_options_loaded') + ' (' + elapsedSec + 's)', 'success');
        } catch (e) {
            if (elapsedTimer) clearInterval(elapsedTimer);
            clearTimeout(hardTimeout);
            const elapsedSec = ((Date.now() - t0) / 1000).toFixed(1);
            const errMsg = e && e.message ? e.message : String(e);
            // Расширенный error message: показываем что случилось + подсказку.
            // До этого фикса пользователь видел "aborted/timeout" без объяснений
            // и думал что это "infinite loading" (спиннер висел до timeout).
            container.innerHTML =
                '<div class="gguf-empty-state" style="color:var(--danger);padding:12px;">' +
                    '<div style="display:flex;align-items:center;gap:8px;margin-bottom:8px;">' +
                        '<i class="fas fa-exclamation-triangle"></i>' +
                        '<strong>' + Utils.escapeHtml(_('gguf.config_load_failed', 'Не удалось загрузить параметры бэкенда')) + '</strong>' +
                    '</div>' +
                    '<div style="font-family:monospace;font-size:var(--font-size-xs);background:rgba(0,0,0,0.2);padding:8px;border-radius:4px;margin-bottom:8px;word-break:break-all;">' +
                        Utils.escapeHtml(errMsg) +
                    '</div>' +
                    '<div style="font-size:var(--font-size-xs);color:var(--text-muted);margin-bottom:8px;">' +
                        Utils.escapeHtml(_('gguf.config_unavailable')) + '<br>' +
                        Utils.escapeHtml(_('gguf.config_check_cppworker', 'Проверьте: docker ps (cppworker запущен?), CppWorker endpoint доступен через balancer proxy.')) +
                    '</div>' +
                    '<div style="display:flex;gap:6px;">' +
                        '<button class="btn btn-secondary btn-sm" id="ggufBackendOptionsReload">' +
                            '<i class="fas fa-sync"></i> ' + Utils.escapeHtml(_('gguf.reload_backend_options')) +
                        '</button>' +
                        '<span style="font-size:var(--font-size-xs);color:var(--text-muted);align-self:center;">' +
                            '(' + elapsedSec + 's)' +
                        '</span>' +
                    '</div>' +
                '</div>';
            const retry = document.getElementById('ggufBackendOptionsReload');
            if (retry) retry.addEventListener('click', loadAndRenderBackendOptions);
        }
    }

    /**
     * Round 32 #3 (2026-08-10): скопировать значения из бэкенд-конфига в
     * `state.loadOptions`, чтобы клик "Загрузить модель" использовал актуальные
     * дефолты, а не хардкод 2048/-1/512.
     *
     * Маппинг: cfg.defaultXxx → state.loadOptions.xxx (для loadOnSelectedBackend).
     * Только те поля, которые реально используются в loadOnSelectedBackend:
     *   gpuLayers, ctxSize, batchSize, flashAttn, numa, useMmap, autoGpuDistribution, tensorSplit.
     * Поля вроде defaultNuma/defaultUseMmap (bool) маппятся отдельно от flashAttn (int).
     */
    function syncLoadOptionsFromConfig(cfg) {
        if (!cfg) return;
        if (cfg.defaultGpuLayers !== undefined && cfg.defaultGpuLayers !== null) {
            state.loadOptions.gpuLayers = cfg.defaultGpuLayers;
        }
        if (cfg.defaultCtxSize !== undefined && cfg.defaultCtxSize !== null) {
            state.loadOptions.ctxSize = cfg.defaultCtxSize;
        }
        if (cfg.defaultBatchSize !== undefined && cfg.defaultBatchSize !== null) {
            state.loadOptions.batchSize = cfg.defaultBatchSize;
        }
        if (cfg.defaultFlashAttnType !== undefined && cfg.defaultFlashAttnType !== null) {
            // cppworker flash_attn_type: -1=auto, 0=disabled, 1=enabled.
            // webui loadOptions.flashAttn: bool. Конвертим: 0=false, ≠0=true.
            state.loadOptions.flashAttn = (cfg.defaultFlashAttnType !== 0);
        }
        if (cfg.defaultNuma !== undefined && cfg.defaultNuma !== null) {
            state.loadOptions.numa = !!cfg.defaultNuma;
        }
        if (cfg.defaultUseMmap !== undefined && cfg.defaultUseMmap !== null) {
            state.loadOptions.useMmap = !!cfg.defaultUseMmap;
        }
        if (cfg.autoGpuDistribution !== undefined && cfg.autoGpuDistribution !== null) {
            state.loadOptions.autoGpuDistribution = !!cfg.autoGpuDistribution;
        }
        if (Array.isArray(cfg.defaultTensorSplit) && cfg.defaultTensorSplit.length > 0) {
            state.loadOptions.tensorSplit = cfg.defaultTensorSplit.join(',');
        }
    }

    /**
     * Round 32 #3 (2026-08-10): пометить, что пользователь явно настроил
     * loadOptions через Settings.Save. После этого syncLoadOptionsFromConfig
     * не будет перезатирать пользовательские значения при следующем заходе.
     */
    function markLoadOptionsCustom(backendId) {
        if (!state._loadOptionsCustom) state._loadOptionsCustom = {};
        state._loadOptionsCustom[backendId] = true;
    }

    // renderRpcServerState — ЖИВОЕ состояние узла раскладки (R124, пункт 2).
    //
    // Показываем ФАКТ, а не галочку: процеcc запущен, порт слушается, какой pid,
    // сколько живёт, сколько было перезапусков и ПОЧЕМУ не поднялся. Нет данных
    // (старая сборка cppworker поля не отдаёт) — честно говорим «нет данных»:
    // рисовать «выключено» или «работает» вместо неизвестного нельзя, оператор
    // примет это за состояние.
    function renderRpcServerState(st) {
        if (!st || typeof st !== 'object') {
            return '<span class="gguf-rpc-state-unknown">' +
                Utils.escapeHtml(_('gguf.rpc_server_state_unknown',
                    'нет данных (воркер старой сборки не отдаёт состояние узла)')) + '</span>';
        }
        var bits = [];
        var dot = st.listening ? 'ok' : ((st.running || st.restarting) ? 'warn' : 'off');
        var label = st.listening
            ? _('gguf.rpc_server_running', 'слушает')
            : (st.running
                ? _('gguf.rpc_server_starting', 'порт открывается')
                : (st.restarting
                    ? _('gguf.rpc_server_restarting', 'узел падал, поднимается заново')
                    : _('gguf.rpc_server_stopped', 'остановлен')));
        bits.push('<span class="gguf-rpc-state-dot is-' + dot + '"></span>' +
            '<strong>' + Utils.escapeHtml(label) + '</strong>');
        if (st.addr) bits.push('<code>' + Utils.escapeHtml(st.addr) + '</code>');
        if (st.pid) bits.push('pid ' + Utils.escapeHtml(String(st.pid)));
        if (st.uptimeSec) {
            bits.push(Utils.escapeHtml(_('gguf.rpc_server_uptime', 'работает')) + ' ' +
                Utils.escapeHtml(formatElapsedShort(st.uptimeSec * 1000)));
        }
        if (st.restarts) bits.push(Utils.escapeHtml(_('gguf.rpc_server_restarts', 'перезапусков')) + ': ' + st.restarts);
        if (typeof st.restartInMs === 'number' && st.restartInMs >= 0) {
            bits.push(Utils.escapeHtml(_('gguf.rpc_server_restart_in', 'подъём через')) + ' ' +
                Math.max(1, Math.round(st.restartInMs / 1000)) + ' ' +
                Utils.escapeHtml(_('gguf.rpc_server_seconds', 'с')));
        }
        // R172: ЧУЖОЙ узел (отдельный контейнер) — это не «наш процесс»: пусть
        // оператор видит, что гасить его отсюда нельзя.
        if (st.external) bits.push(Utils.escapeHtml(_('gguf.rpc_server_external', 'внешний узел (управляет оператор)')));
        if (st.debug) bits.push(Utils.escapeHtml(_('gguf.rpc_server_debug_on', 'отладка протокола')));
        var html = '<div class="gguf-rpc-state-row">' + bits.join(' · ') + '</div>';
        // R172: ПРИЧИНА прошлой смерти. Клиент раскладки на любую неудачную
        // операцию пишет одно и то же «Remote RPC server crashed or returned
        // malformed response», поэтому exit-статус процесса — единственное, что
        // отличает «сервер разбился» от «сервер ответил false».
        if (st.lastExit) {
            var exitBits = [Utils.escapeHtml(String(st.lastExit))];
            if (st.lastExitPid) exitBits.push('pid ' + Utils.escapeHtml(String(st.lastExitPid)));
            if (st.lastExitAt) exitBits.push(Utils.escapeHtml(formatElapsedShort(Date.now() - Date.parse(st.lastExitAt))) + ' ' +
                Utils.escapeHtml(_('gguf.rpc_server_ago', 'назад')));
            html += '<div class="gguf-rpc-state-exit">' +
                Utils.escapeHtml(_('gguf.rpc_server_last_exit', 'прошлый процесс завершился')) + ': <code>' +
                exitBits.join(' · ') + '</code></div>';
        }
        if (st.lastError) {
            html += '<div class="gguf-rpc-state-error">' + Utils.escapeHtml(st.lastError) + '</div>';
        }
        if (Array.isArray(st.notes) && st.notes.length) {
            html += '<div class="gguf-rpc-state-note">' + Utils.escapeHtml(st.notes.join('; ')) + '</div>';
        }
        // R172: последние строки САМОГО rpc-server (GGML_LOG_ERROR сервера).
        // Свёрнуто: в обычном состоянии оператору они не нужны, а при аварии —
        // это единственное место, где причина названа прямо.
        if (Array.isArray(st.logTail) && st.logTail.length) {
            html += '<details class="gguf-rpc-state-log"><summary>' +
                Utils.escapeHtml(_('gguf.rpc_server_log_tail', 'последние строки узла')) +
                ' (' + st.logTail.length + ')</summary><pre>' +
                Utils.escapeHtml(st.logTail.join('\n')) + '</pre></details>';
        }
        if (st.logDropped) {
            html += '<div class="gguf-rpc-state-note">' +
                Utils.escapeHtml(_('gguf.rpc_server_log_dropped', 'в буфере только хвост лога')) +
                ': ' + st.logLines + ' / +' + st.logDropped + '</div>';
        }
        return html;
    }

    // MODEL_PARAM_SPECS — ЕДИНЫЙ реестр параметров модели (R134, 2026-10-16).
    //
    // ЗАЧЕМ РЕЕСТР. До R134 каждый параметр был описан своей разметкой, карточка
    // показывала 7 полей из 11, которые реально уезжают в загрузку, а имена полей
    // расходились (форма читала gpuLayers, значения приходили как numGpuLayers —
    // поле оставалось пустым). Список ниже — ЕДИНЫЙ источник для ДВУХ таблиц
    // (действующие значения и то, что можно задать) и для ОБОИХ мест: карточка
    // модели на диске и карточка загруженной модели. Иначе они снова разойдутся.
    //
    // ГРАНИЦА ДОСТОВЕРНОСТИ. Набор параметров — не выдумка UI, а контракт бэкенда:
    //   * internal/balancer/model_management.go: llamaCppLoadBodyKeys — что едет в
    //     теле загрузки;
    //   * internal/balancer/load_provenance_r119.go: trackedLoadParams — по каким из
    //     них бэкенд отдаёт ПРОИСХОЖДЕНИЕ значения;
    //   * pkg/types/balancing.go: LlamaCppModelProfile — что принимает профиль.
    //
    // field    — имя поля в профиле модели (PUT /api/v1/cppworker/model-profiles/{name});
    // loadKey  — как то же значение называется в теле загрузки;
    // factKeys — как это значение называет ВОРКЕР в /api/models. Читаем ФАКТ, а не
    //            догадку: именно на этом расхождении карточка показывала
    //            «размышления выключены» у модели, где они включены.
    const MODEL_PARAM_SPECS = [
        { field: 'contextLength', loadKey: 'contextSize', type: 'int', min: 256, max: 1048576, step: 256,
          label: 'gguf.ctx_size', hint: 'gguf.mp_ctx_hint', defaultKey: 'contextLength',
          factKeys: ['context_size', 'contextSize', 'contextLength'] },
        { field: 'contextPerSeq', loadKey: 'contextPerSeq', type: 'int', min: 0, max: 262144, step: 256,
          label: 'gguf.context_per_seq', hint: 'gguf.mp_ctx_seq_hint', defaultKey: 'contextPerSlot',
          factKeys: ['context_per_seq', 'contextPerSeq'] },
        { field: 'numGpuLayers', loadKey: 'gpuLayers', type: 'int', min: -2, max: 200, step: 1,
          label: 'gguf.gpu_layers', hint: 'gguf.mp_gpu_hint', defaultKey: 'numGpuLayers',
          factKeys: ['gpu_layers', 'gpuLayers', 'numGpuLayers'] },
        { field: 'batchSize', loadKey: 'batchSize', type: 'int', min: 1, max: 4096, step: 1,
          label: 'gguf.batch_size', hint: 'gguf.mp_batch_hint', defaultKey: 'batchSize',
          factKeys: ['batch_size', 'batchSize'] },
        { field: 'parallel', loadKey: 'parallel', type: 'int', min: 0, max: 32, step: 1,
          label: 'gguf.parallel', hint: 'gguf.mp_parallel_hint', defaultKey: 'parallel',
          factKeys: ['parallel', 'max_slots', 'maxSlots'] },
        { field: 'kvCacheType', loadKey: 'kvCacheType', type: 'enum', options: ['f16', 'q8_0', 'q4_0'],
          label: 'gguf.kv_cache_type', hint: 'gguf.mp_kv_hint', defaultKey: 'kvCacheType',
          factKeys: ['kv_cache_type', 'kvCacheType'] },
        { field: 'flashAttn', loadKey: 'flashAttn', type: 'bool',
          label: 'gguf.flash_attn', hint: 'gguf.mp_flash_hint', defaultKey: 'flashAttn',
          factKeys: ['flash_attn', 'flashAttn', 'flashAttnType'] },
        { field: 'useMmap', loadKey: 'useMmap', type: 'bool',
          label: 'gguf.use_mmap', hint: 'gguf.mp_mmap_hint', defaultKey: 'useMmap',
          factKeys: ['use_mmap', 'useMmap'] },
        { field: 'enableReasoning', loadKey: 'enableReasoning', type: 'bool',
          label: 'gguf.reasoning', hint: 'gguf.mp_reasoning_hint', defaultKey: 'enableReasoning',
          factKeys: ['reasoning_enabled', 'reasoningEnabled', 'reasoning', 'enableReasoning'] },
        // Дополнительно: per-tensor override для MoE — контракт загрузки их знает
        // (llamaCppLoadBodyKeys), происхождение по ним бэкенд не считает.
        { field: 'overrideTensors', loadKey: 'overrideTensors', type: 'list', advanced: true,
          label: 'gguf.override_tensors', hint: 'gguf.mp_override_hint', factKeys: [] },
        { field: 'overrideTensorBufts', loadKey: 'overrideTensorBufts', type: 'list', advanced: true,
          label: 'gguf.override_bufts', hint: 'gguf.mp_override_bufts_hint', factKeys: [] }
    ];


    // paramFact — что о параметре сообщает ВОРКЕР (факт загруженного инстанса).
    function paramFact(spec, m, rt) {
        m = m || {};
        rt = rt || {};
        var keys = spec.factKeys || [];
        for (var i = 0; i < keys.length; i++) {
            var k = keys[i];
            if (m[k] !== undefined && m[k] !== null && m[k] !== '') return m[k];
            if (rt[k] !== undefined && rt[k] !== null && rt[k] !== '') return rt[k];
        }
        return null;
    }

    // paramOrigin — значение и ИСТОЧНИК из происхождения параметров (R119/R134).
    // R658 (2026-11-20): редактор параметров модели вынесен в gguf-detail-params.js
    // (фабрика), чтобы этот модуль тоже держался ниже 1500 строк (§5).
    var ggufDetailParams = window.GgufDetailParams({
        M: M,
        state: state,
        _: _,
        Utils: Utils,
        PARAM_SOURCE_LABELS: PARAM_SOURCE_LABELS,
        MODEL_PARAM_SPECS: MODEL_PARAM_SPECS,
        paramFact: paramFact,
        collapseSections: collapseSections
    });
    var paramOrigin = ggufDetailParams.paramOrigin;
    var loadedParamsFor = ggufDetailParams.loadedParamsFor;
    var formatParamValue = ggufDetailParams.formatParamValue;
    var inheritedValueOf = ggufDetailParams.inheritedValueOf;
    var inheritedLabel = ggufDetailParams.inheritedLabel;
    var applyInheritedLabels = ggufDetailParams.applyInheritedLabels;
    var paramInput = ggufDetailParams.paramInput;
    var renderModelParamsEditor = ggufDetailParams.renderModelParamsEditor;
    var hydrateModelParamEditors = ggufDetailParams.hydrateModelParamEditors;
    var windowLimitNote = ggufDetailParams.windowLimitNote;
    var modelEffectiveEntry = ggufDetailParams.modelEffectiveEntry;
    var paintWindowLimit = ggufDetailParams.paintWindowLimit;
    var applyInheritedDefaults = ggufDetailParams.applyInheritedDefaults;
    var hydrateOnOpen = ggufDetailParams.hydrateOnOpen;
    var fillModelParamEditor = ggufDetailParams.fillModelParamEditor;
    var collectModelParams = ggufDetailParams.collectModelParams;
    var applyModelParams = ggufDetailParams.applyModelParams;

    // collapseSections — превратить плоскую форму в КОМПАКТНЫЕ раскрываемые секции
    // (R124, пункт 4 разбора стенда).
    //
    // ЗАЧЕМ ПОСТОБРАБОТКА, А НЕ ПЕРЕПИСЫВАНИЕ ФОРМЫ. Форма — это ~500 строк
    // конкатенации; переписывание в массив секций трогает каждый input и рискует
    // сломать обработчики. Здесь же HTML уже собран: мы находим заголовки секций
    // (`h6.gguf-section-header`), которые и так размечают группы, и заворачиваем
    // каждую группу в <details>. Значения, id полей и обработчики не меняются —
    // меняется только представление.
    //
    // Первая секция раскрыта (оператор чаще всего правит именно её), остальные
    // свёрнуты: страница перестаёт быть простынёй, которую надо пролистывать.
    function collapseSections(html) {
        if (typeof html !== 'string' || html === '') return html || '';
        var marker = '<h6 class="gguf-section-header">';
        var parts = html.split(marker);
        if (parts.length < 2) return html; // заголовков нет — не трогаем
        var head = parts[0];
        var out = head;
        for (var i = 1; i < parts.length; i++) {
            var chunk = parts[i];
            // Заголовок отделяем от тела секции: он заканчивается первым </h6>.
            var close = chunk.indexOf('</h6>');
            if (close < 0) {
                // Битый заголовок — возвращаем как есть, не теряя содержимое.
                out += marker + chunk;
                continue;
            }
            var headerInner = chunk.slice(0, close);
            var body = chunk.slice(close + '</h6>'.length);
            // data-acc нужен тестам и стилям; первый блок открыт.
            var isFirst = i === 1;
            out += '<details class="gguf-acc"' + (isFirst ? ' open' : '') + ' data-acc="' + i + '">' +
                '<summary class="gguf-acc-summary">' +
                    '<i class="fas fa-chevron-right gguf-acc-caret"></i>' +
                    headerInner +
                '</summary>' +
                '<div class="gguf-acc-body">' + body + '</div>' +
                '</details>';
        }
        return out;
    }

    // renderRpcPeers — какие узлы раскладки РЕАЛЬНО отвечают (R125, пункт 3).
    //
    // Живая жалоба оператора: «нехватает метрик для того чтобы понимать какой
    // донор активен». Список узлов как текст этого не показывает: строка в
    // textarea остаётся и когда узел лежит.
    //
    // ЧЕСТНО О ТОМ, ЧЕГО ЗДЕСЬ НЕТ. Показываются доступность (TCP) и задержка
    // соединения. Пропускная способность не измеряется: у rpc-server нет ни
    // ручки здоровья, ни эхо-протокола, и выдавать задержку за «скорость обмена»
    // нельзя. Нет ответа пробника — «не проверялось», а не «не активен».
    function renderRpcPeers(peers) {
        if (!peers || typeof peers !== 'object') {
            return '<small class="form-text gguf-rpc-peers-unknown">' +
                Utils.escapeHtml(_('gguf.rpc_peers_not_checked', 'узлы не проверялись')) + '</small>';
        }
        var list = Array.isArray(peers.servers) ? peers.servers : [];
        if (list.length === 0) {
            return '<small class="form-text">' +
                Utils.escapeHtml(_('gguf.rpc_peers_none',
                    'раскладка не настроена — узлов нет')) + '</small>';
        }
        var rows = list.map(function (p) {
            var ok = !!p.reachable;
            var bits = '<span class="gguf-rpc-state-dot is-' + (ok ? 'ok' : 'off') + '"></span>' +
                '<code>' + Utils.escapeHtml(p.addr || '') + '</code>';
            if (ok) {
                bits += '<span>' + Utils.escapeHtml(_('gguf.rpc_peer_active', 'активен')) + '</span>';
                if (p.latencyMs !== undefined && p.latencyMs !== null) {
                    bits += '<span class="gguf-rpc-peer-latency">' + Utils.escapeHtml(String(p.latencyMs)) + ' ms</span>';
                }
            } else {
                bits += '<span>' + Utils.escapeHtml(_('gguf.rpc_peer_inactive', 'не отвечает')) + '</span>';
                if (p.error) {
                    bits += '<span class="gguf-rpc-peer-error">' + Utils.escapeHtml(p.error) + '</span>';
                }
            }
            return '<div class="gguf-rpc-peer-row">' + bits + '</div>';
        }).join('');
        var summary = '<div class="gguf-rpc-peers-summary">' +
            Utils.escapeHtml(_('gguf.rpc_peers_active', 'активных узлов')) + ': ' +
            Utils.escapeHtml(String(peers.reachable || 0)) + ' / ' + Utils.escapeHtml(String(peers.total || list.length)) +
            '</div>';
        // R129 (пункт 3): измеренная пропускная способность до узлов — только если
        // замер РЕАЛЬНО выполнялся (?bench). Отказ замера показываем причиной:
        // «0 МБ/с» означало бы измеренную нулевую скорость, а это не то же самое.
        var speedRows = '';
        var speed = Array.isArray(peers.linkSpeed) ? peers.linkSpeed : [];
        speed.forEach(function (s) {
            if (s.error) {
                speedRows += '<div class="gguf-rpc-peer-row gguf-rpc-peer-speed">' +
                    '<code>' + Utils.escapeHtml(s.endpoint || '') + '</code>' +
                    '<span class="gguf-rpc-peer-error">' +
                    Utils.escapeHtml(_('gguf.link_speed_failed', 'замер не удался')) + ': ' +
                    Utils.escapeHtml(s.error) + '</span></div>';
                return;
            }
            speedRows += '<div class="gguf-rpc-peer-row gguf-rpc-peer-speed">' +
                '<i class="fas fa-right-left"></i>' +
                '<strong>' + Utils.escapeHtml(String(s.mbps)) + ' ' +
                Utils.escapeHtml(_('gguf.link_speed_unit', 'МБ/с')) + '</strong>' +
                '<span class="gguf-rpc-peer-latency">' +
                Utils.escapeHtml(s.endpoint || '') + ' · ' +
                Utils.escapeHtml(String(s.mb)) + ' ' + Utils.escapeHtml(_('gguf.link_speed_mb', 'МБ за')) + ' ' +
                Utils.escapeHtml(String(s.durationMs)) + ' ms</span></div>';
        });
        if (speedRows) {
            speedRows = '<div class="gguf-rpc-peers-summary">' +
                Utils.escapeHtml(_('gguf.link_speed_title', 'Скорость обмена между машинами (TCP)')) +
                '</div>' + speedRows;
        }
        // R129: замер дорогой (десятки МБ), поэтому только по кнопке.
        var benchBtn = speedRows ? '' :
            '<button class="btn btn-secondary btn-sm gguf-rpc-bench" data-rpc-bench="1" style="margin-top:6px;">' +
                '<i class="fas fa-gauge-high"></i> ' +
                Utils.escapeHtml(_('gguf.link_speed_measure', 'Измерить скорость обмена')) +
            '</button>';
        return summary + rows + speedRows + benchBtn;
    }

    // measureLinkSpeed — R129 (пункт 3): запросить замер до узлов и обновить блок.
    //
    // Замер выполняет ВОРКЕР того бэкенда, чью форму открыли: он измеряет путь до
    // узлов раскладки своими средствами (HTTP-приёмник на узле). UI лишь просит и
    // показывает измеренное — считать «на глазок» здесь нечего.
    async function measureLinkSpeed() {
        var backend = M.currentBackend();
        var host = document.getElementById('ggufOptRpcPeers');
        if (!backend || !host) return false;
        host.innerHTML = '<small class="form-text"><i class="fas fa-spinner fa-spin"></i> ' +
            Utils.escapeHtml(_('gguf.link_speed_measuring', 'Измеряю скорость обмена…')) + '</small>';
        try {
            var res = await GgufApi.requestViaBackend(backend.id, '/api/v1/cppworker/rpc-peers?bench=1');
            host.innerHTML = renderRpcPeers(res);
            return true;
        } catch (e) {
            host.innerHTML = '<small class="form-text gguf-rpc-peer-error">' +
                Utils.escapeHtml(_('gguf.link_speed_failed', 'замер не удался')) + ': ' +
                Utils.escapeHtml(e && e.message ? e.message : String(e)) + '</small>';
            return false;
        }
    }

    function renderBackendOptionsForm(cfg, backend, resp, peers) {
        // Маппинг полей cppbackend.Config ↔ UI. Полное покрытие (~25 полей),
        // сгруппировано по секциям: General / Multi-GPU / KV-cache / RoPE-YaRN / Perf.
        //
        // Все inputs имеют префикс "ggufOpt" для удобства findElementById.
        const v = function (key, fallback) {
            return (cfg[key] !== undefined && cfg[key] !== null) ? cfg[key] : fallback;
        };
        const chk = function (key, fallback) {
            if (cfg[key] === undefined || cfg[key] === null) return !!fallback;
            return !!cfg[key];
        };
        const ctx = v('defaultCtxSize', 4096);
        const batch = v('defaultBatchSize', 512);
        const gpu = v('defaultGpuLayers', -1);
        const fa = v('defaultFlashAttnType', -1);
        const numa = chk('defaultNuma', false);
        const mmap = chk('defaultUseMmap', true);
        const mlock = chk('defaultUseMlock', false);
        const threads = v('defaultNThreads', 0);
        const rmsNormEps = v('defaultRmsNormEps', 0.00001);
        // Multi-GPU
        const autoGpu = chk('autoGpuDistribution', true);
        const splitMode = v('defaultSplitMode', -1);
        const mainGpu = v('defaultMainGpu', 0);
        const rpcBackend = v('defaultRpcBackend', '') || '';
        const noMemoryMap = chk('defaultNoMemoryMap', false);
        // R115: раскладка слоёв между машинами (llama.cpp RPC). До R115 эти
        // значения жили только в переменных окружения контейнера, и WebUI их не
        // показывал вовсе — оператор не видел ни режима, ни списка узлов.
        const rpcEnabled = chk('rpcEnabled', false);
        const rpcServersVal = Array.isArray(cfg.rpcServers) ? cfg.rpcServers.join('\n') : '';
        // R124 (2026-10-16, пункт 2): узел раскладки внутри ЭТОГО cppworker.
        // rpcServerState приходит в ответе GET /config отдельным полем (не в config):
        // это ФАКТ о процессе (running/listening/pid/причина), а не настройка.
        const rpcServerEnabled = chk('rpcServerEnabled', false);
        const rpcServerPort = v('rpcServerPort', 50052);
        const rpcServerStateHtml = renderRpcServerState(resp && resp.rpcServer);
        // Tensor split: из бэка приходит []float32.
        let tensorSplitVal = '';
        if (cfg.defaultTensorSplit && Array.isArray(cfg.defaultTensorSplit)) {
            tensorSplitVal = cfg.defaultTensorSplit.join(',');
        }
        // KV cache
        const kvCacheType = v('defaultKvCacheType', '') || '';
        const noKvOffload = chk('defaultNoKvOffload', false);
        // RoPE/YaRN
        const ropeFreqBase = v('defaultRopeFreqBase', 10000.0);
        const ropeFreqScale = v('defaultRopeFreqScale', 1.0);
        const ropeScalingType = v('defaultRopeScalingType', 'none') || 'none';
        const ropeScalingFactor = v('defaultRopeScalingFactor', 1.0);
        const yarnExtFactor = v('defaultYarnExtFactor', 1.0);
        const yarnAttnFactor = v('defaultYarnAttnFactor', 1.0);
        const yarnBetaFast = v('defaultYarnBetaFast', 32.0);
        const yarnBetaSlow = v('defaultYarnBetaSlow', 1.0);
        // Performance / lifecycle
        const idleUnload = v('idleUnloadMinutes', 0);
        const enableMetrics = chk('enableMetrics', true);
        const metricsRetention = v('metricsRetentionSeconds', 3600);
        // Session 18 Round 15 (2026-07-29): Inference defaults — параллелизм и reasoning.
        // defaultNParallel: 0 = inherit cppworker default (1), 1..8 = parallel slots per model.
        // enableReasoning: включает soft+native thinking path (Round 11/14).
        //   cppworker handler принимает ключ "enableReasoning" (без "default" префикса),
        //   см. handlers_config.go:387 applyBool("enableReasoning", ...).
        // reasoningBudget: max thinking tokens before answer (0 = unlimited).
        const nParallel = v('defaultNParallel', 0);
        const enableReasoning = chk('enableReasoning', false);
        const reasoningBudget = v('reasoningBudget', 0);
        // Diagnostics
        const nodeName = (cfg.nodeName != null) ? cfg.nodeName : (backend ? backend.id : '');
        const balancerUrl = (cfg.balancerUrl != null) ? cfg.balancerUrl : '';
        const uptime = (cfg.uptime != null) ? cfg.uptime : '';
        var formHtml = '' +
            // ====== General ======
            '<h6 class="gguf-section-header"><i class="fas fa-sliders-h"></i> ' + Utils.escapeHtml(_('gguf.tab_general') || 'General') + '</h6>' +
            '<div class="form-row" style="display:grid;grid-template-columns:1fr 1fr;gap:12px;">' +
                '<div class="form-group">' +
                    '<label>' + Utils.escapeHtml(_('gguf.ctx_size')) + '</label>' +
                    '<input type="number" id="ggufOptCtxSize" class="form-control" value="' + ctx + '" min="256" max="262144" step="64">' +
                    '<small style="color:var(--text-muted);">' + Utils.escapeHtml(_('gguf.ctx_size_desc')) + '</small>' +
                '</div>' +
                '<div class="form-group">' +
                    '<label>' + Utils.escapeHtml(_('gguf.batch_size')) + '</label>' +
                    '<input type="number" id="ggufOptBatchSize" class="form-control" value="' + batch + '" min="1" max="4096">' +
                '</div>' +
            '</div>' +
            '<div class="form-row" style="display:grid;grid-template-columns:1fr 1fr 1fr;gap:12px;">' +
                '<div class="form-group">' +
                    '<label>' + Utils.escapeHtml(_('gguf.gpu_layers')) +
                        ' <span id="ggufOptGpuLayersBadge" class="gpu-layers-badge is-auto">AUTO</span>' +
                    '</label>' +
                    '<input type="number" id="ggufOptGpuLayers" class="form-control" value="' + gpu + '" min="-2" max="200" ' +
                        'oninput="Utils.updateGpuLayersBadge(\'ggufOptGpuLayers\',\'ggufOptGpuLayersBadge\')">' +
                    '<div class="gpu-layers-quick-row">' +
                        '<button type="button" class="btn btn-secondary" data-gpu-value="-2" ' +
                            'onclick="Utils.setGpuLayers(-2,\'ggufOptGpuLayers\',\'ggufOptGpuLayersBadge\')">' +
                            Utils.escapeHtml(_('gguf.gpu_layers_btn_auto', 'AUTO (-2)')) + '</button>' +
                        '<button type="button" class="btn btn-secondary" data-gpu-value="-1" ' +
                            'onclick="Utils.setGpuLayers(-1,\'ggufOptGpuLayers\',\'ggufOptGpuLayersBadge\')">' +
                            Utils.escapeHtml(_('gguf.gpu_layers_btn_all', 'All layers (-1)')) + '</button>' +
                        '<button type="button" class="btn btn-secondary" data-gpu-value="0" ' +
                            'onclick="Utils.setGpuLayers(0,\'ggufOptGpuLayers\',\'ggufOptGpuLayersBadge\')">' +
                            Utils.escapeHtml(_('gguf.gpu_layers_btn_cpu', 'CPU only (0)')) + '</button>' +
                    '</div>' +
                    '<small style="color:var(--text-muted);display:block;margin-top:6px;">' +
                        Utils.escapeHtml(_('gguf.gpu_layers_desc')) + ' ' +
                        '<strong style="color:var(--success);">' +
                        Utils.escapeHtml(_('gguf.gpu_layers_recommended', 'AUTO is recommended.')) + '</strong>' +
                    '</small>' +
                '</div>' +
                '<div class="form-group">' +
                    '<label>' + Utils.escapeHtml(_('gguf.flash_attn_type')) + '</label>' +
                    '<input type="number" id="ggufOptFlashAttn" class="form-control" value="' + fa + '" min="-1" max="2">' +
                    '<small style="color:var(--text-muted);">' + Utils.escapeHtml(_('gguf.flash_attn_type_desc')) + '</small>' +
                '</div>' +
                '<div class="form-group">' +
                    '<label>' + Utils.escapeHtml(_('gguf.n_threads')) + '</label>' +
                    '<input type="number" id="ggufOptNThreads" class="form-control" value="' + threads + '" min="0" max="256">' +
                    '<small style="color:var(--text-muted);">' + Utils.escapeHtml(_('gguf.n_threads_desc')) + '</small>' +
                '</div>' +
            '</div>' +
            '<div class="form-row" style="display:grid;grid-template-columns:repeat(4,1fr);gap:12px;">' +
                '<div class="form-group"><label><input type="checkbox" id="ggufOptNuma" ' + (numa ? 'checked' : '') + '> ' + Utils.escapeHtml(_('gguf.numa')) + '</label></div>' +
                '<div class="form-group"><label><input type="checkbox" id="ggufOptUseMmap" ' + (mmap ? 'checked' : '') + '> ' + Utils.escapeHtml(_('gguf.use_mmap')) + '</label></div>' +
                '<div class="form-group"><label><input type="checkbox" id="ggufOptUseMlock" ' + (mlock ? 'checked' : '') + '> ' + Utils.escapeHtml(_('gguf.use_mlock')) + '</label></div>' +
                '<div class="form-group"><label><input type="checkbox" id="ggufOptAutoGpu" ' + (autoGpu ? 'checked' : '') + '> ' + Utils.escapeHtml(_('gguf.auto_gpu_distribution')) + '</label></div>' +
            '</div>' +
            // ====== Multi-GPU ======
            '<h6 class="gguf-section-header"><i class="fas fa-layer-group"></i> ' + Utils.escapeHtml(_('gguf.tab_multi_gpu') || 'Multi-GPU') + '</h6>' +
            '<div class="form-row" style="display:grid;grid-template-columns:1fr 1fr;gap:12px;">' +
                '<div class="form-group">' +
                    '<label>' + Utils.escapeHtml(_('gguf.tensor_split')) + '</label>' +
                    '<input type="text" id="ggufOptTensorSplit" class="form-control" value="' + Utils.escapeHtml(tensorSplitVal) + '" placeholder="0.5,0.5">' +
                    '<small style="color:var(--text-muted);">' + Utils.escapeHtml(_('gguf.tensor_split_desc')) + '</small>' +
                '</div>' +
                '<div class="form-group">' +
                    '<label>' + Utils.escapeHtml(_('gguf.split_mode')) + '</label>' +
                    // R118 (2026-10-14): только ПРОВЕРЕННЫЕ значения. Раньше здесь
                    // было числовое поле -1..3, то есть оператор мог выставить
                    // непроверенные режимы разбиения. Раскладка слоёв между
                    // машинами проверена живьём именно в LAYER, поэтому в форме
                    // остались только default и LAYER.
                    '<select id="ggufOptSplitMode" class="form-control">' +
                        GGUF_VERIFIED_SPLIT_MODES.map(function (o) {
                            return '<option value="' + o.value + '"' +
                                (String(splitMode) === String(o.value) ? ' selected' : '') + '>' +
                                Utils.escapeHtml(o.label) + '</option>';
                        }).join('') +
                    '</select>' +
                    '<small style="color:var(--text-muted);">' + Utils.escapeHtml(_('gguf.split_mode_desc') || '-1 = default, 1 = LAYER (раскладка слоёв)') + '</small>' +
                '</div>' +
            '</div>' +
            '<div class="form-row" style="display:grid;grid-template-columns:1fr 1fr;gap:12px;">' +
                '<div class="form-group">' +
                    '<label>' + Utils.escapeHtml(_('gguf.main_gpu')) + '</label>' +
                    '<input type="number" id="ggufOptMainGpu" class="form-control" value="' + mainGpu + '" min="0" max="16">' +
                '</div>' +
                '<div class="form-group">' +
                    '<label>' + Utils.escapeHtml(_('gguf.rpc_backend')) + '</label>' +
                    // R118 (2026-10-14): только тот backend, на котором раскладка
                    // проверена живьём (CUDA — мастер RTX 3070 + донор RTX 5060).
                    // Vulkan/Kompute из списка убраны: эти ветки на стенде не
                    // проверялись, а выбор «вслепую» ломал загрузку.
                    '<select id="ggufOptRpcBackend" class="form-control">' +
                        GGUF_VERIFIED_RPC_BACKENDS.map(function (o) {
                            return '<option value="' + o.value + '"' +
                                (rpcBackend === o.value ? ' selected' : '') + '>' +
                                Utils.escapeHtml(o.label) + '</option>';
                        }).join('') +
                    '</select>' +
                '</div>' +
            '</div>' +
            '<div class="form-row">' +
                '<div class="form-group"><label><input type="checkbox" id="ggufOptNoMemoryMap" ' + (noMemoryMap ? 'checked' : '') + '> ' + Utils.escapeHtml(_('gguf.no_memory_map')) + '</label></div>' +
            '</div>' +
            // ====== Донор / приёмник (раскладка слоёв) ======
            //
            // R124 (2026-10-16, пункты 2-4 разбора стенда). Управление раскладкой
            // раньше лежало ВНУТРИ блока Multi-GPU, в одной строке с «без mmap»:
            // галочка режима, список узлов — и ничего о том, участвует ли в
            // раскладке САМА эта машина. Оператор не мог ответить на вопросы «кто
            // донор», «кто приёмник», «работает ли узел» — и не видел, что донором
            // теперь может быть сам cppworker, а не отдельный контейнер.
            //
            // Теперь это ОТДЕЛЬНАЯ секция: сначала роль этой машины (отдаёт карту
            // соседу — узел rpc-server), затем узлы, которые подключает мастер, и
            // ЖИВОЕ состояние процесса.
            '<h6 class="gguf-section-header"><i class="fas fa-network-wired"></i> ' +
                Utils.escapeHtml(_('gguf.tab_rpc_role', 'Донор / приёмник (раскладка слоёв)')) + '</h6>' +
            '<div class="form-row">' +
                '<div class="form-group gguf-rpc-node-block">' +
                    '<label><input type="checkbox" id="ggufOptRpcServerEnabled" ' + (rpcServerEnabled ? 'checked' : '') + '> ' +
                        Utils.escapeHtml(_('gguf.rpc_server_enabled', 'Эта машина отдаёт свою видеокарту соседу (узел раскладки)')) + '</label>' +
                    '<small class="form-text">' + Utils.escapeHtml(_('gguf.rpc_server_hint') ||
                        'cppworker сам поднимает llama.cpp rpc-server — отдельный контейнер не нужен. ' +
                        'Порт должен совпадать с публикацией в compose, иначе узел будет недоступен снаружи.') + '</small>' +
                '</div>' +
            '</div>' +
            '<div class="form-row" style="display:grid;grid-template-columns:1fr 2fr;gap:12px;">' +
                '<div class="form-group">' +
                    '<label>' + Utils.escapeHtml(_('gguf.rpc_server_port', 'Порт узла')) + '</label>' +
                    '<input type="number" id="ggufOptRpcServerPort" class="form-control" value="' + rpcServerPort + '" min="1" max="65535">' +
                '</div>' +
                '<div class="form-group">' +
                    '<label>' + Utils.escapeHtml(_('gguf.rpc_server_state', 'Состояние узла')) + '</label>' +
                    // Показываем ФАКТ, а не галочку: процесс/порт/причина отказа.
                    // Нет данных — честно говорим «нет данных» (старая сборка
                    // cppworker поля не отдаёт), а не рисуем «работает».
                    '<div id="ggufOptRpcServerState" class="gguf-rpc-state">' + rpcServerStateHtml + '</div>' +
                '</div>' +
            '</div>' +
            '<div class="form-row">' +
                '<div class="form-group">' +
                    '<label><input type="checkbox" id="ggufOptRpcEnabled" ' + (rpcEnabled ? 'checked' : '') + '> ' +
                        Utils.escapeHtml(_('gguf.rpc_enabled')) + '</label>' +
                    '<textarea id="ggufOptRpcServers" class="form-control" rows="3" spellcheck="false" ' +
                        'placeholder="192.0.2.10:50052">' + Utils.escapeHtml(rpcServersVal) + '</textarea>' +
                    '<small class="form-text">' + Utils.escapeHtml(_('gguf.rpc_servers_hint')) + '</small>' +
                    // R125 (пункт 3): какие узлы РЕАЛЬНО отвечают. Оператор просил
                    // метрику «какой донор активен» — показываем факт проверки, а не
                    // пересказ списка. Нет данных — «не проверялось».
                    '<div id="ggufOptRpcPeers" class="gguf-rpc-peers">' + renderRpcPeers(peers) + '</div>' +
                '</div>' +
            '</div>' +
            // ====== KV Cache ======
            '<h6 class="gguf-section-header"><i class="fas fa-memory"></i> ' + Utils.escapeHtml(_('gguf.tab_kv_cache') || 'KV-cache') + '</h6>' +
            '<div class="form-row" style="display:grid;grid-template-columns:1fr 1fr;gap:12px;">' +
                '<div class="form-group">' +
                    '<label>' + Utils.escapeHtml(_('gguf.kv_cache_type')) + '</label>' +
                    '<select id="ggufOptKvCacheType" class="form-control">' +
                        '<option value=""' + (kvCacheType === '' ? ' selected' : '') + '>— inherit (F16) —</option>' +
                        '<option value="f16"' + (kvCacheType === 'f16' ? ' selected' : '') + '>f16 (default)</option>' +
                        '<option value="f32"' + (kvCacheType === 'f32' ? ' selected' : '') + '>f32 (full precision)</option>' +
                        '<option value="q8_0"' + (kvCacheType === 'q8_0' ? ' selected' : '') + '>q8_0 (-50% VRAM)</option>' +
                        '<option value="q4_0"' + (kvCacheType === 'q4_0' ? ' selected' : '') + '>q4_0 (-75% VRAM)</option>' +
                    '</select>' +
                    '<small style="color:var(--text-muted);">' + Utils.escapeHtml(_('gguf.kv_cache_type_desc')) + '</small>' +
                '</div>' +
                '<div class="form-group">' +
                    '<label><input type="checkbox" id="ggufOptNoKvOffload" ' + (noKvOffload ? 'checked' : '') + '> ' + Utils.escapeHtml(_('gguf.no_kv_offload')) + '</label>' +
                    '<small style="color:var(--text-muted);">' + Utils.escapeHtml(_('gguf.no_kv_offload_desc') || 'Держать KV-cache в RAM (не выгружать на GPU)') + '</small>' +
                '</div>' +
            '</div>' +
            // ====== RoPE & YaRN ======
            '<h6 class="gguf-section-header"><i class="fas fa-wave-square"></i> ' + Utils.escapeHtml(_('gguf.tab_rope_yarn') || 'RoPE / YaRN') + '</h6>' +
            '<div class="form-row" style="display:grid;grid-template-columns:1fr 1fr;gap:12px;">' +
                '<div class="form-group">' +
                    '<label>' + Utils.escapeHtml(_('gguf.rope_freq_base')) + '</label>' +
                    '<input type="number" id="ggufOptRopeFreqBase" class="form-control" value="' + ropeFreqBase + '" min="1" step="0.0001">' +
                '</div>' +
                '<div class="form-group">' +
                    '<label>' + Utils.escapeHtml(_('gguf.rope_freq_scale')) + '</label>' +
                    '<input type="number" id="ggufOptRopeFreqScale" class="form-control" value="' + ropeFreqScale + '" min="0" step="0.0001">' +
                '</div>' +
            '</div>' +
            '<div class="form-row" style="display:grid;grid-template-columns:1fr 1fr;gap:12px;">' +
                '<div class="form-group">' +
                    '<label>' + Utils.escapeHtml(_('gguf.rope_scaling_type')) + '</label>' +
                    '<select id="ggufOptRopeScalingType" class="form-control">' +
                        '<option value="none"' + (ropeScalingType === 'none' ? ' selected' : '') + '>none</option>' +
                        '<option value="linear"' + (ropeScalingType === 'linear' ? ' selected' : '') + '>linear</option>' +
                        '<option value="yarn"' + (ropeScalingType === 'yarn' ? ' selected' : '') + '>yarn</option>' +
                    '</select>' +
                '</div>' +
                '<div class="form-group">' +
                    '<label>' + Utils.escapeHtml(_('gguf.rope_scaling_factor')) + '</label>' +
                    '<input type="number" id="ggufOptRopeScalingFactor" class="form-control" value="' + ropeScalingFactor + '" min="0" step="0.01">' +
                '</div>' +
            '</div>' +
            '<div class="form-row" style="display:grid;grid-template-columns:repeat(4,1fr);gap:12px;">' +
                '<div class="form-group"><label>' + Utils.escapeHtml(_('gguf.yarn_ext_factor')) + '<input type="number" id="ggufOptYarnExtFactor" class="form-control" value="' + yarnExtFactor + '" min="0" step="0.01"></label></div>' +
                '<div class="form-group"><label>' + Utils.escapeHtml(_('gguf.yarn_attn_factor')) + '<input type="number" id="ggufOptYarnAttnFactor" class="form-control" value="' + yarnAttnFactor + '" min="0" step="0.01"></label></div>' +
                '<div class="form-group"><label>' + Utils.escapeHtml(_('gguf.yarn_beta_fast')) + '<input type="number" id="ggufOptYarnBetaFast" class="form-control" value="' + yarnBetaFast + '" min="0" step="0.01"></label></div>' +
                '<div class="form-group"><label>' + Utils.escapeHtml(_('gguf.yarn_beta_slow')) + '<input type="number" id="ggufOptYarnBetaSlow" class="form-control" value="' + yarnBetaSlow + '" min="0" step="0.01"></label></div>' +
            '</div>' +
            // ====== Performance / Metrics / Lifecycle ======
            '<h6 class="gguf-section-header"><i class="fas fa-tachometer-alt"></i> ' + Utils.escapeHtml(_('gguf.tab_perf') || 'Performance') + '</h6>' +
            '<div class="form-row" style="display:grid;grid-template-columns:1fr 1fr;gap:12px;">' +
                '<div class="form-group">' +
                    '<label>' + Utils.escapeHtml(_('gguf.rms_norm_eps')) + '</label>' +
                    '<input type="number" id="ggufOptRmsNormEps" class="form-control" value="' + rmsNormEps + '" min="0" step="0.000001">' +
                '</div>' +
                '<div class="form-group">' +
                    '<label>' + Utils.escapeHtml(_('gguf.idle_unload_minutes')) + '</label>' +
                    '<input type="number" id="ggufOptIdleUnloadMinutes" class="form-control" value="' + idleUnload + '" min="0" max="10080">' +
                    '<small style="color:var(--text-muted);">' + Utils.escapeHtml(_('gguf.idle_unload_desc') || '0 = off, >0 = unload after N minutes idle') + '</small>' +
                '</div>' +
            '</div>' +
            '<div class="form-row" style="display:grid;grid-template-columns:1fr 1fr;gap:12px;">' +
                '<div class="form-group"><label><input type="checkbox" id="ggufOptEnableMetrics" ' + (enableMetrics ? 'checked' : '') + '> ' + Utils.escapeHtml(_('gguf.enable_metrics')) + '</label></div>' +
                '<div class="form-group">' +
                    '<label>' + Utils.escapeHtml(_('gguf.metrics_retention')) + '</label>' +
                    '<input type="number" id="ggufOptMetricsRetention" class="form-control" value="' + metricsRetention + '" min="0">' +
                '</div>' +
            '</div>' +
            // ====== Inference (Round 15, 2026-07-29) ======
            // n_parallel + enableReasoning + reasoningBudget — default значения
            // для ВСЕХ загружаемых моделей на этом backend. Per-Model Profile
            // (cppworker-params.js) может override'нуть для конкретной модели.
            '<h6 class="gguf-section-header"><i class="fas fa-brain"></i> ' + Utils.escapeHtml(_('gguf.tab_inference') || 'Inference') + '</h6>' +
            '<div class="form-row" style="display:grid;grid-template-columns:1fr 1fr 1fr;gap:12px;">' +
                '<div class="form-group">' +
                    '<label>' + Utils.escapeHtml(_('gguf.n_parallel') || 'Parallel sequences (n_parallel)') + '</label>' +
                    '<input type="number" id="ggufOptNParallel" class="form-control" value="' + nParallel + '" min="0" max="8">' +
                    '<small style="color:var(--text-muted);">' + Utils.escapeHtml(_('gguf.n_parallel_desc') || 'Default parallel slots per model. 0 = 1 (sequential), 1..8 = multi-slot KV-cache isolation. > 1 = more VRAM.') + '</small>' +
                '</div>' +
                '<div class="form-group">' +
                    '<label><input type="checkbox" id="ggufOptEnableReasoning" ' + (enableReasoning ? 'checked' : '') + '> ' + Utils.escapeHtml(_('gguf.enable_reasoning') || 'Enable reasoning/thinking') + '</label>' +
                    '<small style="color:var(--text-muted);">' + Utils.escapeHtml(_('gguf.enable_reasoning_desc') || 'Soft+native thinking path for gemma-4, deepseek-r1, qwen3-thinking.') + '</small>' +
                '</div>' +
                '<div class="form-group">' +
                    '<label>' + Utils.escapeHtml(_('gguf.reasoning_budget') || 'Reasoning budget (tokens)') + '</label>' +
                    '<input type="number" id="ggufOptReasoningBudget" class="form-control" value="' + reasoningBudget + '" min="0" max="100000" step="256">' +
                    '<small style="color:var(--text-muted);">' + Utils.escapeHtml(_('gguf.reasoning_budget_desc') || 'Max thinking tokens before answer. 0 = unlimited.') + '</small>' +
                '</div>' +
            '</div>' +
            // ====== Diagnostics ======
            '<div class="form-group" style="color:var(--text-muted);font-size:var(--font-size-xs);margin-top:8px;">' +
                (nodeName ? ('<div><b>node:</b> ' + Utils.escapeHtml(nodeName) + '</div>') : '') +
                (balancerUrl ? ('<div><b>balancer:</b> ' + Utils.escapeHtml(balancerUrl) + '</div>') : '') +
                (uptime ? ('<div><b>uptime:</b> ' + Utils.escapeHtml(uptime) + '</div>') : '') +
            '</div>' +
            '<div class="form-actions" style="margin-top:12px;display:flex;gap:8px;flex-wrap:wrap;">' +
                '<button class="btn btn-primary" id="ggufBackendOptionsSave">' +
                    '<i class="fas fa-save"></i> ' + Utils.escapeHtml(_('gguf.save_backend_options')) +
                '</button>' +
                '<button class="btn btn-secondary" id="ggufBackendOptionsReload">' +
                    '<i class="fas fa-sync"></i> ' + Utils.escapeHtml(_('gguf.reload_backend_options')) +
                '</button>' +
                // R106b (2026-10-12): «сделать эти значения приоритетом для новых
                // моделей». Слой «настройки контейнера» слабее двух других (профиль
                // модели и «настройки по умолчанию»), поэтому значения этой формы
                // сами по себе не выигрывают при загрузке. Кнопка переносит
                // load-affecting поля в тот слой, который реально применяется —
                // без ручного дублирования значений в двух местах.
                '<button class="btn btn-secondary" id="ggufBackendOptionsPromote" title="' +
                    Utils.escapeHtml(_('gguf.backend_options_promote_title', 'Записать n_ctx, слои, KV-cache, параллельность, flash и размышления в «настройки по умолчанию для новых моделей» — слой, который применяется при загрузке раньше настроек контейнера.')) + '">' +
                    '<i class="fas fa-arrow-up"></i> ' + Utils.escapeHtml(_('gguf.backend_options_promote', 'Сделать приоритетом для новых моделей')) +
                '</button>' +
            '</div>';
        // R124: плоская форма → компактные раскрываемые секции (см. collapseSections).
        return collapseSections(formHtml);
    }

    async function saveBackendOptions() {
        const backend = M.currentBackend();
        if (!backend) return;
        // Парсим значения из формы. Хелперы parseIntOr/parseFloatOr/strVal
        // (определены ниже) устойчивы к NaN/пустым полям.
        const splitStr = parseStr('ggufOptTensorSplit', '');
        let tensorSplit = null;
        if (splitStr.trim() !== '') {
            // Парсим "0.5,0.5" → [0.5, 0.5]
            const parts = splitStr.split(',').map(s => parseFloat(s.trim()));
            if (parts.every(p => Number.isFinite(p))) {
                tensorSplit = parts;
            }
        }
        const payload = {
            // Базовые
            defaultCtxSize: parseIntOr(document.getElementById('ggufOptCtxSize').value, 4096),
            defaultBatchSize: parseIntOr(document.getElementById('ggufOptBatchSize').value, 512),
            defaultGpuLayers: parseIntOr(document.getElementById('ggufOptGpuLayers').value, -1),
            defaultFlashAttnType: parseIntOr(document.getElementById('ggufOptFlashAttn').value, -1),
            defaultNuma: !!document.getElementById('ggufOptNuma').checked,
            defaultUseMmap: !!document.getElementById('ggufOptUseMmap').checked,
            defaultUseMlock: !!document.getElementById('ggufOptUseMlock').checked,
            defaultNThreads: parseIntOr(document.getElementById('ggufOptNThreads').value, 0),
            defaultRmsNormEps: parseFloatOr(document.getElementById('ggufOptRmsNormEps').value, 0.00001),
            // Multi-GPU
            autoGpuDistribution: !!document.getElementById('ggufOptAutoGpu').checked,
            defaultSplitMode: parseIntOr(document.getElementById('ggufOptSplitMode').value, -1),
            defaultMainGpu: parseIntOr(document.getElementById('ggufOptMainGpu').value, 0),
            defaultRpcBackend: parseStr('ggufOptRpcBackend', ''),
            defaultNoMemoryMap: !!document.getElementById('ggufOptNoMemoryMap').checked,
            defaultTensorSplit: tensorSplit,
            // KV cache
            defaultKvCacheType: parseStr('ggufOptKvCacheType', ''),
            defaultNoKvOffload: !!document.getElementById('ggufOptNoKvOffload').checked,
            // RoPE/YaRN
            defaultRopeFreqBase: parseFloatOr(document.getElementById('ggufOptRopeFreqBase').value, 10000.0),
            defaultRopeFreqScale: parseFloatOr(document.getElementById('ggufOptRopeFreqScale').value, 1.0),
            defaultRopeScalingType: parseStr('ggufOptRopeScalingType', 'none'),
            defaultRopeScalingFactor: parseFloatOr(document.getElementById('ggufOptRopeScalingFactor').value, 1.0),
            defaultYarnExtFactor: parseFloatOr(document.getElementById('ggufOptYarnExtFactor').value, 1.0),
            defaultYarnAttnFactor: parseFloatOr(document.getElementById('ggufOptYarnAttnFactor').value, 1.0),
            defaultYarnBetaFast: parseFloatOr(document.getElementById('ggufOptYarnBetaFast').value, 32.0),
            defaultYarnBetaSlow: parseFloatOr(document.getElementById('ggufOptYarnBetaSlow').value, 1.0),
            // Performance
            idleUnloadMinutes: parseIntOr(document.getElementById('ggufOptIdleUnloadMinutes').value, 0),
            enableMetrics: !!document.getElementById('ggufOptEnableMetrics').checked,
            metricsRetentionSeconds: parseIntOr(document.getElementById('ggufOptMetricsRetention').value, 3600),
            // Inference (Round 15, 2026-07-29): n_parallel + reasoning.
            // enableReasoning / reasoningBudget — без "default" префикса, как в app.js
            // (handler applyBool("enableReasoning") в handlers_config.go:387).
            defaultNParallel: parseIntOr(document.getElementById('ggufOptNParallel').value, 0),
            enableReasoning: !!document.getElementById('ggufOptEnableReasoning').checked,
            reasoningBudget: parseIntOr(document.getElementById('ggufOptReasoningBudget').value, 0),
            // R106b (2026-10-12): те же значения — ещё и в слой «настройки по
            // умолчанию ДЛЯ ЗАГРУЗКИ».
            //
            // ЗАЧЕМ. `enableReasoning` (runtime) включает thinking-инструкцию в
            // промпте, но модель при загрузке наследует ДРУГОЕ поле —
            // `defaultEnableReasoning`. Без него галочка на этой странице не
            // доезжала до новой загрузки: оператор включал размышления, а
            // cppworker поднимал модель с reasoning_enabled=false — «галочка не
            // применяется, модель грузится не с теми параметрами» (живая жалоба).
            defaultEnableReasoning: !!document.getElementById('ggufOptEnableReasoning').checked,
            defaultReasoningBudget: parseIntOr(document.getElementById('ggufOptReasoningBudget').value, 0)
        };
        // R115: раскладка слоёв. Отправляем ТОЛЬКО если блок отрисован (иначе
        // сохранение настроек молча выключило бы раскладку на бэкенде, где поля
        // нет). Пустой список — законное значение: узлы убираются, режим остаётся
        // таким, каким его выставила галочка.
        const rpcEnabledEl = document.getElementById('ggufOptRpcEnabled');
        const rpcServersEl = document.getElementById('ggufOptRpcServers');
        if (rpcEnabledEl) {
            payload.rpcEnabled = !!rpcEnabledEl.checked;
        }
        if (rpcServersEl) {
            payload.rpcServers = rpcServersEl.value
                .split(/[\n,]/)
                .map(function (s) { return s.trim(); })
                .filter(function (s) { return s.length > 0; });
        }
        // R124 (2026-10-16, пункт 2): узел раскладки внутри ЭТОГО cppworker.
        // Как и rpcEnabled/rpcServers — отправляем только если поля отрисованы:
        // иначе сохранение настроек на бэкенде со старой сборкой молча выключило
        // бы узел, которого там и не было.
        const rpcServerEnabledEl = document.getElementById('ggufOptRpcServerEnabled');
        if (rpcServerEnabledEl) {
            payload.rpcServerEnabled = !!rpcServerEnabledEl.checked;
        }
        const rpcServerPortEl = document.getElementById('ggufOptRpcServerPort');
        if (rpcServerPortEl) {
            payload.rpcServerPort = parseIntOr(rpcServerPortEl.value, 0);
        }
        // === Round 32 #3 (2026-08-10): полное обновление state.loadOptions ===
        // Раньше (до фикса) обновлялся только autoGpuDistribution — остальные поля
        // (gpuLayers/ctxSize/batchSize/flashAttn/numa/useMmap) оставались в state
        // хардкодными (2048/-1/512), независимо от того, что ввёл пользователь.
        // Теперь: читаем все поля из формы + помечаем backend как "custom"
        // (syncLoadOptionsFromConfig не будет их перезатирать при следующем заходе).
        state.loadOptions.gpuLayers = payload.defaultGpuLayers;
        state.loadOptions.ctxSize = payload.defaultCtxSize;
        state.loadOptions.batchSize = payload.defaultBatchSize;
        // cppworker flash_attn_type: -1=auto, 0=disabled, 1=enabled.
        // webui loadOptions.flashAttn: bool. Конвертим: 0=false, ≠0=true.
        state.loadOptions.flashAttn = (payload.defaultFlashAttnType !== 0);
        state.loadOptions.numa = payload.defaultNuma;
        state.loadOptions.useMmap = payload.defaultUseMmap;
        state.loadOptions.autoGpuDistribution = payload.autoGpuDistribution;
        if (Array.isArray(payload.defaultTensorSplit) && payload.defaultTensorSplit.length > 0) {
            state.loadOptions.tensorSplit = payload.defaultTensorSplit.join(',');
        }
        markLoadOptionsCustom(backend.id);
        const saveBtn = document.getElementById('ggufBackendOptionsSave');
        if (saveBtn) saveBtn.disabled = true;
        try {
            const resp = await GgufApi.requestViaBackend(backend.id, '/api/v1/cppworker/config/update', {
                method: 'PUT',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify(payload)
            });
            // Показываем пользователю, что применилось и сколько ошибок валидации.
            let msg = _('gguf.backend_options_saved');
            if (resp && resp.applied) {
                msg += ' (' + resp.applied.length + ' полей)';
            }
            if (resp && resp.validation_errors && resp.validation_errors.length > 0) {
                showToast('⚠️ ' + resp.validation_errors.length + ' validation errors: ' +
                    resp.validation_errors.slice(0, 3).join('; '), 'error');
            }
            showToast(msg, 'success');
            // R115: смена раскладки слоёв применяется при СЛЕДУЮЩЕЙ загрузке
            // модели. Без этого сообщения «сохранено» читалось бы как «уже
            // работает», хотя загруженная модель осталась на прежней схеме.
            if (resp && resp.rpc_reload_required) {
                showToast(_('gguf.rpc_reload_required'), 'info');
            }
            // Re-fetch config чтобы пользователь сразу видел, что изменения применились.
            // cppworker возвращает applied[] и reload_started[] — показываем их в toast.
            if (resp && resp.reload_started && resp.reload_started.length > 0) {
                showToast('Reloading ' + resp.reload_started.length + ' model(s) with new defaults...', 'info');
            }
            // R688: сохранение прошло — защита формы снимается ШТАТНО. Без этого
            // страница осталась бы «грязной» до истечения TTL: авто-обновление
            // продолжало бы откладывать пересборку панели, хотя сохранять больше
            // нечего. noteSaved снимает отметку FormDirty и объявляет текущие
            // значения базовыми.
            var G = formState();
            if (G && typeof G.noteSaved === 'function') {
                G.noteSaved(document.getElementById('ggufDetailContent') ||
                    document.getElementById('ggufDetailPanel') || 'gguf-page');
            }
            // Перезагружаем форму с актуальными значениями из cppworker.
            await loadAndRenderBackendOptions();
            // R106b (2026-10-12): обновляем и блоки «что получат новые модели» /
            // «модели в папке» — иначе после сохранения оператор видит старое
            // значение и делает вывод «галочка не применилась».
            if (window.CppWorkerParams && typeof window.CppWorkerParams.loadAndRender === 'function') {
                window.CppWorkerParams.loadAndRender();
            }
            // Также обновляем runtime-параметры загруженных моделей (context_size и т.д.).
            try {
                const rtData = await GgufApi.getRuntimeConfigViaBackend(backend.id);
                if (rtData && rtData.loaded_models) {
                    state.runtimeModels = {};
                    (rtData.loaded_models || []).forEach(function (m) {
                        state.runtimeModels[m.name] = m;
                    });
                }
            } catch (rtErr) {
                // runtime config может быть недоступен на старых cppworker — игнорируем.
            }
        } catch (e) {
            showToast(_('gguf.backend_options_save_error') + ': ' + (e.message || e), 'error');
        } finally {
            if (saveBtn) saveBtn.disabled = false;
        }
    }

    /**
     * R106b (2026-10-12): перенести load-affecting значения формы в «настройки по
     * умолчанию для новых моделей» (config.defaultModelProfile) — слой, который
     * применяется к новой модели РАНЬШЕ настроек контейнера.
     *
     * ПОЧЕМУ ОТДЕЛЬНАЯ КНОПКА, а не автоматическая запись при Save. Молчаливая
     * запись в чужой слой перезаписала бы настройки, выставленные оператором
     * ранее (на стенде: kv=q4_0, parallel=2, flash=on), значениями формы
     * контейнера (kv=f16, flash=auto) — то есть «сделать приоритетом» тихо
     * УХУДШИЛО бы загрузку. Поэтому перенос — явное действие оператора, а
     * результат виден в блоке «Что получат новые модели» ниже.
     */
    async function promoteBackendOptionsToDefaults() {
        const api = window.Api;
        if (!api || !api.cppworkerLoadDefaults || typeof api.cppworkerLoadDefaults.save !== 'function') {
            showToast(_('gguf.backend_options_promote_unavailable',
                'API «настройки по умолчанию» недоступно — обновите страницу'), 'error');
            return;
        }
        const btn = document.getElementById('ggufBackendOptionsPromote');
        if (btn) btn.disabled = true;
        const intOrSkip = function (id) {
            const el = document.getElementById(id);
            if (!el) return undefined;
            const n = parseInt(el.value, 10);
            return Number.isFinite(n) ? n : undefined;
        };
        const body = {};
        // contextLength обязателен для слоя: без него PUT отказывает, если
        // дефолтного профиля ещё нет.
        const ctx = intOrSkip('ggufOptCtxSize');
        if (ctx !== undefined && ctx > 0) body.contextLength = ctx;
        const batch = intOrSkip('ggufOptBatchSize');
        if (batch !== undefined && batch > 0) body.batchSize = batch;
        const layers = intOrSkip('ggufOptGpuLayers');
        if (layers !== undefined) body.numGpuLayers = layers;
        const parallel = intOrSkip('ggufOptNParallel');
        if (parallel !== undefined && parallel > 0) body.parallel = parallel;
        const kvEl = document.getElementById('ggufOptKvCacheType');
        if (kvEl && kvEl.value) body.kvCacheType = kvEl.value;
        // flashAttn в профиле — *bool, в контейнере — -1/0/1. -1 (auto) =
        // «не задавать», иначе явное true/false.
        const fa = intOrSkip('ggufOptFlashAttn');
        if (fa !== undefined && fa !== -1) body.flashAttn = (fa === 1);
        const mmapEl = document.getElementById('ggufOptUseMmap');
        if (mmapEl) body.useMmap = !!mmapEl.checked;
        const reasoningEl = document.getElementById('ggufOptEnableReasoning');
        if (reasoningEl) body.enableReasoning = !!reasoningEl.checked;

        try {
            await api.cppworkerLoadDefaults.save(body);
            showToast(_('gguf.backend_options_promoted',
                'Записано в «настройки по умолчанию для новых моделей» — теперь эти значения применяются при загрузке раньше настроек контейнера'), 'success');
            // R688: значения формы сохранены (в слой по умолчанию) — снимаем
            // защиту так же, как после «Сохранить»: оператор закончил действие.
            var G = formState();
            if (G && typeof G.noteSaved === 'function') {
                G.noteSaved(document.getElementById('ggufDetailContent') ||
                    document.getElementById('ggufDetailPanel') || 'gguf-page');
            }
            if (window.CppWorkerParams && typeof window.CppWorkerParams.loadAndRender === 'function') {
                window.CppWorkerParams.loadAndRender();
            }
        } catch (e) {
            showToast(_('gguf.backend_options_promote_error',
                'Не удалось записать настройки по умолчанию') + ': ' + (e && e.message ? e.message : e), 'error');
        } finally {
            if (btn) btn.disabled = false;
        }
    }

    // Хелперы для парсинга значений из формы (parseIntOr объявлена ниже,
    // parseFloatOr/parseStr определяем тут же — близко к месту использования).
    function parseFloatOr(v, def) {
        const n = parseFloat(v);
        return Number.isFinite(n) ? n : def;
    }
    function parseStr(id, def) {
        const el = document.getElementById(id);
        if (!el) return def;
        return el.value;
    }

    function parseIntOr(v, def) {
        const n = parseInt(v, 10);
        return Number.isFinite(n) ? n : def;
    }

    /**
     * Инициализировать Per-Model Profiles внутри gguf-настроек.
     * Использует window.CppWorkerParams (cppworker-params.js).
     */
    function mountProfilesInSettings() {
        const btn = document.getElementById('cppProfileAddBtn');
        if (btn && !btn._ggufBound) {
            btn._ggufBound = true;
            btn.addEventListener('click', function () {
                if (window.CppWorkerParams && typeof window.CppWorkerParams.openWizard === 'function') {
                    window.CppWorkerParams.openWizard('', null);
                }
            });
        }
        if (window.CppWorkerParams && typeof window.CppWorkerParams.loadAndRender === 'function') {
            window.CppWorkerParams.loadAndRender();
        }
    }

    function renderConnectModal() {
        return '' +
            '<div class="gguf-modal-backdrop" id="ggufConnectModal">' +
                '<div class="gguf-modal">' +
                    '<h3><i class="fas fa-plug"></i> ' + _('gguf.modal_title') + '</h3>' +
                    '<p>' + _('gguf.modal_desc') + '</p>' +
                    '<div class="form-group">' +
                        '<label>' + (_('gguf.cppworker_url_placeholder') || 'cppworker URL') + '</label>' +
                        '<input type="text" id="ggufConnectUrl" class="form-control" placeholder="http://host:18091" value="' + Utils.escapeHtml(GgufApi.getUrl()) + '">' +
                    '</div>' +
                    '<div class="gguf-modal-actions">' +
                        '<button class="btn btn-secondary" id="ggufConnectCancel"><i class="fas fa-times"></i> ' + (_('common.cancel') || 'Cancel') + '</button>' +
                        '<button class="btn btn-primary" id="ggufConnectSubmit"><i class="fas fa-plug"></i> ' + (_('gguf.connect') || 'Connect') + '</button>' +
                    '</div>' +
                '</div>' +
            '</div>';
    }

        return {
            loadAndRenderBackendOptions: loadAndRenderBackendOptions,
            applyModelParams: applyModelParams,
            measureLinkSpeed: measureLinkSpeed,
            saveBackendOptions: saveBackendOptions,
            promoteBackendOptionsToDefaults: promoteBackendOptionsToDefaults,
            renderSettingsPane: renderSettingsPane,
            syncLoadOptionsFromConfig: syncLoadOptionsFromConfig,
            markLoadOptionsCustom: markLoadOptionsCustom,
            renderRpcServerState: renderRpcServerState,
            paramFact: paramFact,
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
            collapseSections: collapseSections,
            renderRpcPeers: renderRpcPeers,
            renderBackendOptionsForm: renderBackendOptionsForm,
            parseFloatOr: parseFloatOr,
            parseStr: parseStr,
            parseIntOr: parseIntOr,
            mountProfilesInSettings: mountProfilesInSettings,
            renderConnectModal: renderConnectModal,
            MODEL_PARAM_SPECS: MODEL_PARAM_SPECS
        };
    };
})();

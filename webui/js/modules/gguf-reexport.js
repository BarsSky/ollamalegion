/**
 * gguf-reexport.js — блок «Ре-экспорт GGUF» на странице моделей (R709 + D11).
 *
 * ЗАЧЕМ БЛОК ЖИВЁТ ОТДЕЛЬНО ОТ РЕНДЕРЕРА GGUF. Ре-экспорт — ОПЦИОНАЛЬНЫЙ модуль
 * (compose-профиль `reexport`): на большинстве стендов сервиса нет вовсе, и
 * страница моделей обязана выглядеть ровно как раньше. Поэтому:
 *
 *   1. рендерер GGUF создаёт только ПУСТОЙ хост (#ggufReexportHost) — ни одного
 *      элемента, ни одной подписи в разметке нет;
 *   2. этот модуль сам спрашивает GET /api/reexport/capabilities (через nginx
 *      WebUI, тот же origin — CORS и отдельных портов не нужно);
 *   3. ответ 200 → в хост вставляется блок; 502/таймаут/сеть → хост остаётся
 *      пустым, и вкладки/блока НЕТ ВООБЩЕ (требование оператора: «нет модуля —
 *      нет вкладки», а не «вкладка есть, но пустая»).
 *
 * ПОЧЕМУ ПРОБА, А НЕ ФЛАГ В КОНФИГЕ. Флаг в .env пришлось бы синхронизировать с
 * фактом «сервис поднят» — и он врёт в обе стороны: включили профиль без флага
 * (блок не появился) или выключили сервис с флагом (мёртвая вкладка с ошибками).
 * Проба отвечает на настоящий вопрос «модуль доступен прямо сейчас».
 *
 * D11 (2026-11-25): СЦЕНАРИЙ ЦЕЛИКОМ В ОДНОМ БЛОКЕ. Раньше блок был формой
 * «выбери цель и создай задачу»: оператор должен был сам знать, что dry_run —
 * это проверка, а цель `inkling2_loop` — это нормализация. Теперь шаги названы
 * действиями и идут в том порядке, в котором их делает человек:
 *
 *   шаг 1 — список моделей тома с ВЕРДИКТОМ схемы (patchable и почему);
 *   шаг 2 — «Проверить»      → задача inspect: что именно изменит нормализация;
 *   шаг 3 — «Нормализовать»  → задача normalize: стадии, прогресс, отмена, итог;
 *   шаг 4 — «Пересканировать модели» → воркеры перечитывают каталог;
 *   шаг 5 — «Проверить загрузку»     → загрузка через балансер: loading → loaded|failed,
 *           причина отказа и предупреждение, если модель не влезает в узел.
 *
 * ПОЧЕМУ «inspect» — ЭТО dry_run, А НЕ ОТДЕЛЬНОЕ ПОЛЕ mode. API сервиса
 * (services/gguf-reexport/jobs.go, createRequest) принимает ровно
 * {model_dir, target, options}, а POST /api/reexport/jobs разбирается с
 * DisallowUnknownFields: лишнее поле `mode` дало бы 400. Проверка = та же цель
 * `inkling2_loop` с options.dry_run=true: драйвер читает только шапку (секунды),
 * печатает план изменений и НЕ пишет файл. Поэтому в разметке шаг называется
 * «Проверить», а на провод уходит dry_run — это видно в теле запроса и в таблице
 * задач (у пробной задачи в итоге стоит «пробный прогон, файл не писался»).
 *
 * ДВА СЛОЯ (D11, R628). Шаги сценария, разметка, выбор модели и проверка
 * загрузки живут здесь; транспорт задач, поллинг и таблица истории вынесены в
 * js/modules/gguf-reexport-jobs.js (фабрика window.GgufReexportJobs) — вместе
 * файл перестал быть «гигантом» (страж R628: боевой файл >= 1500 непустых
 * строк запрещён). Слой задач подключается ПЕРЕД этим файлом (webui/index.html)
 * и берётся лениво: если его нет, шаги, которым он нужен, честно говорят об
 * этом, а остальные работают.
 *
 * ЭКСПОРТ: window.GgufReexport = { mount, _state, _buildJobBody, _buildBundleBody,
 * _verdictFor, _renderModels, _renderInspect, _renderNormalize, _renderRescan,
 * _renderLoad, _renderBundle, _loadWarning, _phaseLabel, _modelHandle,
 * _jobErrorText, _formatBytes, _formatDuration, _statusLabel, _probeOnce,
 * _buildHTML, _renderJobs, _schedulePolling, _resetJobsLayer, _stopPolling }.
 * Тесты используют подчёркнутые члены; публичный контракт — только mount().
 */
(function () {
    'use strict';

    var API = '/api/reexport';
    // Таймаут пробы короткий осознанно: это вопрос «ты здесь», а не работа. 4 с
    // хватает локальной сети docker; при отсутствии сервиса nginx отвечает 502
    // сразу, так что ожидание видит только человек с медленным прокси.
    var PROBE_TIMEOUT_MS = 4000;
    // Поллинг загрузки модели: узел отвечает быстрее, и «loading» оператор ждёт
    // глазами, поэтому 2 с и ограничение по числу попыток (10 минут) — дальше
    // честно говорим, что состояние не получено, а не крутим индикатор вечно.
    var LOAD_POLL_MS = 2000;
    var LOAD_POLL_MAX = 300;
    // Цель нормализации: одна операция сервиса (metadata_patch). Цель convert
    // (hf_to_gguf) в light-образе объявлена available=false, и кнопки под неё
    // здесь нет — «нет torch» это не ошибка формы, а свойство сборки.
    var PATCH_TARGET = 'inkling2_loop';
    // Цель Фазы 4: набор уже скачанных файлов image-модели → bundle с profile.json
    // (роли по заголовкам, R696). Отдельная цель, а не режим нормализации: у неё
    // другой источник (каталог), другой результат (каталог) и другой валидатор
    // (imageworker), поэтому и шаг в блоке свой.
    var BUNDLE_TARGET = 'image_bundle';

    var state = {
        probed: false,
        available: false,
        caps: null,
        models: [],
        selectedModel: '',
        modelsError: '',
        jobs: [],
        error: '',
        lastJobId: '',
        pollTimer: null,
        inspectTimer: null,
        normalizeTimer: null,
        loadTimer: null,
        // Шаг 2: проверка схемы (dry_run той же цели).
        inspect: null,
        // Шаг 3: нормализация. status: running|succeeded|failed|canceled|skipped.
        normalize: null,
        // Шаг 4: пересканирование каталога на воркерах.
        rescan: null,
        // Шаг 5: пробная загрузка через балансер.
        load: null,
        // Шаг 6 (Фаза 4): сборка image-bundle из набора файлов.
        bundle: null
    };

    function t(key, vars) {
        if (typeof window !== 'undefined' && window.I18N && typeof window.I18N.t === 'function') {
            return vars ? window.I18N.t(key, vars) : window.I18N.t(key);
        }
        return key;
    }

    function esc(value) {
        var s = (value === undefined || value === null) ? '' : String(value);
        if (typeof window !== 'undefined' && window.Utils && typeof window.Utils.escapeHtml === 'function') {
            return window.Utils.escapeHtml(s);
        }
        return s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
            .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
    }

    /** formatBytes — размер в человеческом виде (МиБ/ГиБ, а не миллионы цифр). */
    function formatBytes(bytes) {
        var n = Number(bytes) || 0;
        if (n <= 0) return '0';
        var gb = n / (1024 * 1024 * 1024);
        if (gb >= 1) return gb.toFixed(gb >= 10 ? 1 : 2) + ' GiB';
        var mb = n / (1024 * 1024);
        if (mb >= 1) return mb.toFixed(0) + ' MiB';
        // Меньше мегабайта (синтетические и мини-модели): «0 MiB» выглядит как
        // ошибка, поэтому показываем килобайты.
        return (n / 1024).toFixed(1) + ' KiB';
    }

    /** formatDuration — мм:сс для итога задачи. */
    function formatDuration(ms) {
        var total = Math.max(0, Math.round((Number(ms) || 0) / 1000));
        var m = Math.floor(total / 60);
        var s = total % 60;
        return (m > 0 ? m + 'm ' : '') + s + 's';
    }

    /** authHeaders — CSRF/токен как у остальных модулей (R202: сессия, R59.5). */
    function authHeaders(extra) {
        var out = { 'Accept': 'application/json' };
        if (extra) {
            Object.keys(extra).forEach(function (k) { out[k] = extra[k]; });
        }
        if (typeof window !== 'undefined' && window.Api && typeof window.Api.getAuthHeaders === 'function') {
            var h = window.Api.getAuthHeaders() || {};
            Object.keys(h).forEach(function (k) {
                if (out[k] === undefined && h[k]) out[k] = h[k];
            });
        }
        return out;
    }

    /** requestJSON — fetch с таймаутом на чтение и разбором ошибки сервиса. */
    function requestJSON(url, options) {
        var opts = options || {};
        var timeout = opts.timeoutMs === undefined ? PROBE_TIMEOUT_MS : opts.timeoutMs;
        var ctrl = null;
        var timer = null;
        if (timeout > 0 && typeof AbortController !== 'undefined') {
            ctrl = new AbortController();
            timer = setTimeout(function () { ctrl.abort(); }, timeout);
            opts.signal = ctrl.signal;
        }
        return fetch(url, opts).then(function (resp) {
            return resp.text().then(function (text) {
                var body = null;
                if (text) {
                    try { body = JSON.parse(text); } catch (e) { body = null; }
                }
                if (!resp.ok) {
                    var err = new Error((body && body.error && body.error.message) || ('HTTP ' + resp.status));
                    err.status = resp.status;
                    err.code = body && body.error ? body.error.code : '';
                    err.body = body;
                    throw err;
                }
                return body || {};
            });
        }).finally(function () {
            if (timer) clearTimeout(timer);
        });
    }

    /**
     * probeOnce — одна проба доступности модуля.
     *
     * Возвращает Promise<boolean>. Никогда не rejects: отсутствие модуля — это
     * НОРМАЛЬНОЕ состояние стенда, а не ошибка страницы, и не повод писать в
     * консоль красным.
     */
    function probeOnce(timeoutMs) {
        return requestJSON(API + '/capabilities', {
            method: 'GET',
            headers: authHeaders(),
            credentials: 'same-origin',
            timeoutMs: timeoutMs === undefined ? PROBE_TIMEOUT_MS : timeoutMs
        }).then(function (caps) {
            state.caps = caps || null;
            state.available = !!(caps && caps.service);
            state.probed = true;
            state.error = '';
            return state.available;
        }).catch(function () {
            state.caps = null;
            state.available = false;
            state.probed = true;
            return false;
        });
    }

    /** host — место блока. Его создаёт рендерер GGUF (всегда, пустым). */
    function host() {
        if (typeof document === 'undefined' || !document.getElementById) return null;
        return document.getElementById('ggufReexportHost');
    }

    /** mount — точка входа: проба + (при успехе) отрисовка блока. */
    function mount() {
        var el = host();
        if (!el) return Promise.resolve(false);
        // Повторный вход на страницу: если модуль уже известен — рисуем сразу из
        // кэша и обновляем возможности в фоне (без мигания «блока нет»).
        if (state.available && state.caps) {
            renderBlock(el);
            probeOnce().then(function (ok) {
                if (!ok) {
                    clearBlock(el);
                } else {
                    updateConverterLine(el);
                }
            });
            return Promise.resolve(true);
        }
        clearBlock(el);
        return probeOnce().then(function (ok) {
            if (!ok) {
                // Модуля нет: хост пуст, вкладки нет вообще. Никаких «ошибок» и
                // повторных попыток — стенд без модуля это штатный стенд.
                state.models = [];
                state.jobs = [];
                return false;
            }
            renderBlock(el);
            return true;
        });
    }

    function clearBlock(el) {
        if (el) el.innerHTML = '';
        stopPolling();
    }

    // =====================================================================
    // Разметка
    // =====================================================================

    /** stepHead — заголовок шага: номер + подпись (текст уже переведён). */
    function stepHead(num, title) {
        return '<div class="rx-step-head">' +
            '<span class="rx-step-num">' + num + '</span>' +
            '<span class="rx-step-title">' + esc(title) + '</span>' +
            '</div>';
    }

    /**
     * helpHTML — содержимое сворачиваемой справки (R745).
     *
     * ЗАЧЕМ В САМОМ БЛОКЕ, А НЕ ТОЛЬКО В ДОКЕ. Оператор попадает сюда с вопросом
     * «что мне нажать и что станет с файлом», а документация лежит в репозитории.
     * Справка отвечает на месте: что делает каждая цель СЕЙЧАС (список берётся из
     * capabilities, поэтому врать про доступность не может), порядок шагов, лимиты
     * и типовые причины отказа. Блок свёрнут по умолчанию (<details> без open):
     * постоянные пояснения не должны отодвигать шаги вниз.
     */
    function helpHTML() {
        var caps = state.caps || {};
        var targets = (caps.targets || []);
        var targetsHtml = targets.map(function (tg) {
            var label = esc(tg.label || tg.id || '');
            var mark = tg.available
                ? '<span class="rx-badge rx-badge-patchable">' + esc(t('reexport.help_available')) + '</span>'
                : '<span class="rx-badge rx-badge-unreadable">' + esc(t('reexport.help_unavailable')) + '</span>';
            var reason = (!tg.available && tg.reason) ? '<div class="rx-step-hint">' + esc(tg.reason) + '</div>' : '';
            return '<li>' + label + ' ' + mark + reason + '</li>';
        }).join('');
        var flow = [1, 2, 3, 4, 5].map(function (n) {
            return '<li>' + esc(t('reexport.help_flow_' + n)) + '</li>';
        }).join('');
        var faq = [1, 2, 3, 4].map(function (n) {
            return '<li>' + esc(t('reexport.help_faq_' + n)) + '</li>';
        }).join('');
        return '<div class="rx-help-body">' +
            '<p>' + esc(t('reexport.help_what')) + '</p>' +
            '<div class="rx-help-title">' + esc(t('reexport.help_targets_title')) + '</div>' +
            '<ul class="rx-help-list" id="rxHelpTargets">' + targetsHtml + '</ul>' +
            '<div class="rx-help-title">' + esc(t('reexport.help_flow_title')) + '</div>' +
            '<ol class="rx-help-list" id="rxHelpFlow">' + flow + '</ol>' +
            '<div class="rx-help-title">' + esc(t('reexport.help_limits_title')) + '</div>' +
            '<p>' + esc(t('reexport.help_limits')) + '</p>' +
            '<div class="rx-help-title">' + esc(t('reexport.help_faq_title')) + '</div>' +
            '<ul class="rx-help-list" id="rxHelpFaq">' + faq + '</ul>' +
            '<div class="rx-step-hint">' + esc(t('reexport.help_docs')) + '</div>' +
            '</div>';
    }
    /** buildHTML — разметка блока. Данные подставляются отдельными функциями. */
    function buildHTML() {
        return '' +
            '<section class="card" id="ggufReexportCard" style="margin-bottom:12px;">' +
                '<div class="card-header">' +
                    '<h3><i class="fas fa-recycle section-icon-i" aria-hidden="true"></i> ' +
                        '<span data-i18n="reexport.title">' + esc(t('reexport.title')) + '</span></h3>' +
                    '<div class="card-actions" style="display:flex;gap:8px;align-items:center;flex-wrap:wrap;">' +
                        '<span id="rxConverter" class="rx-converter" style="font-size:var(--font-size-xs);color:var(--text-muted);"></span>' +
                        '<button type="button" class="btn btn-sm btn-secondary" id="rxRefresh">' +
                            '<i class="fas fa-rotate" aria-hidden="true"></i> ' + esc(t('reexport.refresh')) + '</button>' +
                    '</div>' +
                '</div>' +
                '<div class="card-body">' +
                    '<div id="rxArch" style="display:flex;gap:6px;flex-wrap:wrap;margin-bottom:10px;"></div>' +
                    '<div id="rxTargetNote" style="margin-bottom:12px;font-size:var(--font-size-xs);color:var(--text-muted);"></div>' +
                    '<div id="rxError" role="alert" style="display:none;margin-bottom:12px;padding:8px 10px;border-radius:6px;background:var(--bg-secondary);border:1px solid var(--border-color);font-size:var(--font-size-sm);"></div>' +
                    // --- R745: сворачиваемая справка (свёрнута по умолчанию) ---
                    '<details class="rx-help" id="rxHelp">' +
                        '<summary class="rx-help-summary">' +
                            '<i class="fas fa-circle-question" aria-hidden="true"></i> ' +
                            esc(t('reexport.help_summary')) +
                        '</summary>' +
                        helpHTML() +
                    '</details>' +

                    // --- шаг 1: модели и вердикт ---
                    '<div class="rx-step" data-step="model" id="rxStepModel">' +
                        stepHead(1, t('reexport.step_model_title')) +
                        '<div class="table-responsive">' +
                            '<table class="data-table"><thead><tr>' +
                                '<th>' + esc(t('reexport.col_model_file')) + '</th>' +
                                '<th>' + esc(t('reexport.col_verdict')) + '</th>' +
                            '</tr></thead><tbody id="rxModelsBody"></tbody></table>' +
                        '</div>' +
                    '</div>' +

                    // --- шаг 2: проверка (inspect) ---
                    '<div class="rx-step" data-step="inspect" id="rxStepInspect">' +
                        stepHead(2, t('reexport.step_inspect_title')) +
                        '<div class="rx-step-hint">' + esc(t('reexport.step_inspect_hint')) + '</div>' +
                        '<div class="rx-step-actions">' +
                            '<button type="button" class="btn btn-sm btn-secondary" id="rxInspect">' +
                                '<i class="fas fa-stethoscope" aria-hidden="true"></i> ' + esc(t('reexport.btn_inspect')) + '</button>' +
                        '</div>' +
                        '<div id="rxInspectStatus" class="rx-status" role="status" aria-live="polite"></div>' +
                        '<div id="rxInspectResult" class="rx-result"></div>' +
                    '</div>' +

                    // --- шаг 3: нормализация ---
                    '<div class="rx-step" data-step="normalize" id="rxStepNormalize">' +
                        stepHead(3, t('reexport.step_normalize_title')) +
                        '<div class="rx-step-hint">' + esc(t('reexport.step_normalize_hint')) + '</div>' +
                        '<div class="rx-step-actions" style="display:flex;gap:10px;flex-wrap:wrap;align-items:flex-end;">' +
                            '<div class="form-group" style="margin:0;min-width:220px;">' +
                                '<label for="rxOut">' + esc(t('reexport.out_path')) + '</label>' +
                                '<input class="form-control" id="rxOut" type="text" placeholder="' +
                                    esc(t('reexport.out_path_hint')) + '">' +
                            '</div>' +
                            '<label style="display:flex;gap:6px;align-items:center;font-size:var(--font-size-sm);">' +
                                '<input type="checkbox" id="rxFuse" checked> ' + esc(t('reexport.fuse')) + '</label>' +
                            '<button type="button" class="btn btn-sm btn-primary" id="rxNormalize">' +
                                '<i class="fas fa-file-export" aria-hidden="true"></i> ' +
                                esc(t('reexport.btn_normalize')) + '</button>' +
                            '<button type="button" class="btn btn-sm btn-secondary" id="rxCancelJob" style="display:none;">' +
                                '<i class="fas fa-ban" aria-hidden="true"></i> ' + esc(t('reexport.cancel')) + '</button>' +
                        '</div>' +
                        '<div id="rxNormalizeProgress" class="rx-progress"></div>' +
                        '<div id="rxNormalizeStatus" class="rx-status" role="status" aria-live="polite"></div>' +
                        '<div id="rxNormalizeResult" class="rx-result"></div>' +
                    '</div>' +

                    // --- шаг 4: пересканирование ---
                    '<div class="rx-step" data-step="rescan" id="rxStepRescan">' +
                        stepHead(4, t('reexport.step_rescan_title')) +
                        '<div class="rx-step-hint">' + esc(t('reexport.step_rescan_hint')) + '</div>' +
                        '<div class="rx-step-actions">' +
                            '<button type="button" class="btn btn-sm btn-secondary" id="rxRescan">' +
                                '<i class="fas fa-arrows-rotate" aria-hidden="true"></i> ' +
                                esc(t('reexport.rescan')) + '</button>' +
                        '</div>' +
                        '<div id="rxRescanStatus" class="rx-status" role="status" aria-live="polite"></div>' +
                    '</div>' +

                    // --- шаг 5: проверка загрузки ---
                    '<div class="rx-step" data-step="load" id="rxStepLoad">' +
                        stepHead(5, t('reexport.step_load_title')) +
                        '<div class="rx-step-hint">' + esc(t('reexport.step_load_hint')) + '</div>' +
                        '<div class="rx-step-actions">' +
                            '<button type="button" class="btn btn-sm btn-secondary" id="rxCheckLoad">' +
                                '<i class="fas fa-server" aria-hidden="true"></i> ' +
                                esc(t('reexport.btn_load')) + '</button>' +
                        '</div>' +
                        '<div id="rxLoadStatus" class="rx-status" role="status" aria-live="polite"></div>' +
                        '<div id="rxLoadWarn" class="rx-warn"></div>' +
                        '<div id="rxLoadResult" class="rx-result"></div>' +
                    '</div>' +

                    // --- шаг 6 (Фаза 4): набор файлов → image-bundle ---
                    //
                    // ПОЧЕМУ ОТДЕЛЬНЫЙ ШАГ, А НЕ ПАРАМЕТР НОРМАЛИЗАЦИИ: источник здесь
                    // КАТАЛОГ (не файл), результат — каталог bundle, а проверку делает
                    // imageworker (свой эндпоинт). Смешать это с шагами loop-схемы значило
                    // бы объяснять оператору, почему «нормализация» просит каталог.
                    '<div class="rx-step" data-step="bundle" id="rxStepBundle">' +
                        stepHead(6, t('reexport.step_bundle_title')) +
                        '<div class="rx-step-hint">' + esc(t('reexport.step_bundle_hint')) + '</div>' +
                        '<div class="rx-step-actions" style="display:flex;gap:10px;flex-wrap:wrap;align-items:flex-end;">' +
                            '<div class="form-group" style="margin:0;min-width:240px;">' +
                                '<label for="rxBundleDir">' + esc(t('reexport.bundle_dir')) + '</label>' +
                                '<input class="form-control" id="rxBundleDir" type="text" placeholder="' +
                                    esc(t('reexport.bundle_dir_hint')) + '">' +
                            '</div>' +
                            '<div class="form-group" style="margin:0;min-width:170px;">' +
                                '<label for="rxBundleName">' + esc(t('reexport.bundle_name')) + '</label>' +
                                '<input class="form-control" id="rxBundleName" type="text" placeholder="' +
                                    esc(t('reexport.bundle_name_hint')) + '">' +
                            '</div>' +
                            '<button type="button" class="btn btn-sm btn-secondary" id="rxBundleCheck">' +
                                '<i class="fas fa-stethoscope" aria-hidden="true"></i> ' +
                                esc(t('reexport.btn_bundle_check')) + '</button>' +
                            '<button type="button" class="btn btn-sm btn-primary" id="rxBundleBuild">' +
                                '<i class="fas fa-cubes-stacked" aria-hidden="true"></i> ' +
                                esc(t('reexport.btn_bundle_build')) + '</button>' +
                        '</div>' +
                        '<div id="rxBundleStatus" class="rx-status" role="status" aria-live="polite"></div>' +
                        '<div id="rxBundleResult" class="rx-result"></div>' +
                    '</div>' +

                    // --- история задач ---
                    '<div style="margin-top:14px;">' +
                        '<h4 style="margin:0 0 8px;font-size:var(--font-size-sm);">' + esc(t('reexport.jobs')) + '</h4>' +
                        '<div class="table-responsive">' +
                            '<table class="data-table"><thead><tr>' +
                                '<th>' + esc(t('reexport.col_job')) + '</th>' +
                                '<th>' + esc(t('reexport.col_model')) + '</th>' +
                                '<th>' + esc(t('reexport.col_status')) + '</th>' +
                                '<th>' + esc(t('reexport.col_progress')) + '</th>' +
                                '<th>' + esc(t('reexport.col_result')) + '</th>' +
                                '<th></th>' +
                            '</tr></thead><tbody id="rxJobsBody"></tbody></table>' +
                        '</div>' +
                    '</div>' +
                '</div>' +
            '</section>';
    }

    /** renderBlock — вставка разметки и привязка обработчиков. */
    function renderBlock(el) {
        el.innerHTML = buildHTML();
        updateConverterLine(el);
        renderArchitectures(el);
        renderTargetNote(el);
        bind(el);
        state.error = '';
        renderModels(el);
        renderInspect(el);
        renderNormalize(el);
        renderRescan(el);
        renderLoad(el);
        renderBundle(el);
        loadModels();
        if (jobs()) {
            jobs().loadJobs();
        } else {
            showJobsLayerMissing(el);
        }
    }

    function updateConverterLine(el) {
        var node = el.querySelector('#rxConverter');
        if (!node || !state.caps) return;
        var conv = state.caps.converter || {};
        var parts = [];
        if (conv.name) parts.push(conv.name + (conv.revision ? ' ' + conv.revision : ''));
        if (conv.python) parts.push('python ' + conv.python);
        parts.push('torch: ' + (conv.torch_state === 'present' ? (conv.torch || 'present') : t('reexport.torch_absent')));
        node.textContent = parts.join(' · ');
    }

    /**
     * renderArchitectures — какие архитектуры модуль умеет приводить в порядок.
     *
     * Показываем данные сервиса, а не свою таблицу: если сервис обновится, страница
     * обновится вместе с ним (иначе UI и возможности расходятся).
     */
    function renderArchitectures(el) {
        var node = el.querySelector('#rxArch');
        if (!node || !state.caps) return;
        var archs = state.caps.architectures || [];
        node.innerHTML = archs.map(function (a) {
            var label = a.label || a.id;
            var note = a.note ? (' title="' + esc(a.note) + '"') : '';
            return '<span class="filter-btn"' + note + ' style="cursor:default;">' +
                '<i class="fas fa-microchip" aria-hidden="true"></i> ' + esc(label) + '</span>';
        }).join(' ');
    }

    /** patchTargetSpec — описание цели нормализации из возможностей сервиса. */
    function patchTargetSpec() {
        var targets = (state.caps && state.caps.targets) || [];
        for (var i = 0; i < targets.length; i++) {
            if (targets[i] && targets[i].id === PATCH_TARGET) return targets[i];
        }
        return null;
    }

    /**
     * renderTargetNote — доступна ли цель нормализации в этой сборке образа.
     *
     * ЗАЧЕМ ПОКАЗЫВАТЬ. Кнопка «Нормализовать» без python-драйвера в образе дала бы
     * 422 target_unavailable после нажатия; оператору нужен ответ ДО нажатия и
     * причина словами сервиса (torch/скрипт патча — свойство сборки, а не ошибка).
     */
    function renderTargetNote(el) {
        var node = el.querySelector('#rxTargetNote');
        if (!node) return;
        var spec = patchTargetSpec();
        var parts = [];
        parts.push(t('reexport.patch_target') + ': ' + (spec ? (spec.label || spec.id) : PATCH_TARGET));
        if (spec && !spec.available) {
            parts.push(t('reexport.target_unavailable') + (spec.reason ? ' — ' + spec.reason : ''));
        }
        node.textContent = parts.join(' · ');
        var inspectBtn = el.querySelector('#rxInspect');
        var normalizeBtn = el.querySelector('#rxNormalize');
        var disabled = !!(spec && spec.available === false);
        if (inspectBtn) inspectBtn.disabled = disabled;
        if (normalizeBtn) normalizeBtn.disabled = disabled;
    }

    /** bind — обработчики блока. Пересоздаются вместе с разметкой. */
    function bind(el) {
        var refresh = el.querySelector('#rxRefresh');
        if (refresh) {
            refresh.addEventListener('click', function () {
                probeOnce().then(function (ok) {
                    if (!ok) { clearBlock(el); return; }
                    renderBlock(el);
                    loadModels(true);
                });
            });
        }
        var models = el.querySelector('#rxModelsBody');
        if (models) {
            models.addEventListener('click', function (ev) {
                var row = ev && ev.target && ev.target.closest ? ev.target.closest('[data-model-path]') : null;
                if (!row) return;
                selectModel(el, row.getAttribute('data-model-path'));
            });
        }
        var inspectBtn = el.querySelector('#rxInspect');
        if (inspectBtn) inspectBtn.addEventListener('click', function () { runInspect(el); });
        var normBtn = el.querySelector('#rxNormalize');
        if (normBtn) normBtn.addEventListener('click', function () { runNormalize(el); });
        var cancelBtn = el.querySelector('#rxCancelJob');
        if (cancelBtn) cancelBtn.addEventListener('click', function () { cancelNormalize(el); });
        var rescan = el.querySelector('#rxRescan');
        if (rescan) rescan.addEventListener('click', function () { rescanModels(el); });
        var loadBtn = el.querySelector('#rxCheckLoad');
        if (loadBtn) loadBtn.addEventListener('click', function () { checkLoad(el); });
        // Шаг 6 (Фаза 4): «Проверить набор» — пробный прогон (dry_run), «Собрать
        // bundle» — запись. Обе кнопки — та же ручка POST /api/reexport/jobs с
        // целью image_bundle: отдельного «сборочного» API нет.
        var bundleCheck = el.querySelector('#rxBundleCheck');
        if (bundleCheck) bundleCheck.addEventListener('click', function () { runBundle(el, true); });
        var bundleBuild = el.querySelector('#rxBundleBuild');
        if (bundleBuild) bundleBuild.addEventListener('click', function () { runBundle(el, false); });
        // Кнопка отмены в таблице задач: рисует строки и слушает их слой задач,
        // здесь только спрашиваем, загружен ли он.
        if (jobs()) jobs().bind(el);
    }

    /** showError — причина отказа одной строкой (код + текст сервиса). */
    function showError(el, message) {
        var node = el.querySelector('#rxError');
        if (!node) return;
        if (!message) {
            node.style.display = 'none';
            node.textContent = '';
            return;
        }
        node.style.display = '';
        node.textContent = message;
    }

    // =====================================================================
    // Шаг 1: модели и вердикт схемы
    // =====================================================================

    /**
     * verdictFor — вердикт по модели ДО создания задачи.
     *
     * Здесь видно, нужен ли патч вообще: «block_count=48 при 24 физических блоках —
     * loop-экспорт, нужен ре-экспорт» либо «файл загружается как есть». Это ответ на
     * вопрос оператора «а почему эта модель не грузится» — и он стоит ОДНОГО
     * чтения шапки, которое сервис уже сделал для списка.
     *
     * Возвращает {kind, text}: kind — для CSS-бейджа (patchable|normalized|plain|unreadable).
     */
    function verdictFor(model) {
        if (!model) return { kind: 'plain', text: '' };
        if (model.read_error) {
            return { kind: 'unreadable', text: t('reexport.verdict_unreadable_text', { error: model.read_error }) };
        }
        if (model.scheme === 'inkling2_loop_export') {
            return {
                kind: 'patchable',
                text: t('reexport.verdict_inkling2_loop_export', {
                    physical: model.physical_blocks,
                    blocks: model.block_count
                })
            };
        }
        if (model.scheme === 'inkling2_loop') {
            return {
                kind: 'normalized',
                text: t('reexport.verdict_inkling2_loop', {
                    blocks: model.block_count,
                    physical: model.physical_blocks
                })
            };
        }
        if (model.scheme === 'plain') {
            return { kind: 'plain', text: t('reexport.verdict_plain_text', { arch: model.arch || '-' }) };
        }
        return { kind: 'plain', text: t('reexport.verdict_unknown_text', { scheme: model.scheme || '-' }) };
    }

    /** verdictBadge — короткий ярлык вердикта (patchable / уже в схеме / не нужен). */
    function verdictBadge(kind) {
        switch (kind) {
        case 'patchable': return t('reexport.verdict_patchable');
        case 'normalized': return t('reexport.verdict_normalized');
        case 'unreadable': return t('reexport.verdict_unreadable');
        default: return t('reexport.verdict_plain');
        }
    }

    /** loadModels — список GGUF тома моделей (с вердиктом схемы). */
    function loadModels(refresh) {
        var el = host();
        if (!el) return Promise.resolve();
        return requestJSON(API + '/models' + (refresh ? '?refresh=1' : ''), {
            method: 'GET', headers: authHeaders(), credentials: 'same-origin', timeoutMs: 120000
        }).then(function (data) {
            state.models = (data && data.models) || [];
            state.modelsError = '';
            ensureSelection();
            renderModels(el);
        }).catch(function (err) {
            state.models = [];
            state.modelsError = err && err.message ? err.message : String(err);
            renderModels(el);
            showError(el, t('reexport.models_failed') + ': ' + state.modelsError);
        });
    }

    /** ensureSelection — выбранная модель обязана существовать в новом списке. */
    function ensureSelection() {
        var found = false;
        for (var i = 0; i < state.models.length; i++) {
            if (state.models[i].path === state.selectedModel) { found = true; break; }
        }
        if (found) return;
        // По умолчанию — первая, которой нужен ре-экспорт: ради неё блок и открыли.
        var pick = '';
        for (var j = 0; j < state.models.length; j++) {
            if (state.models[j].patchable && !state.models[j].read_error) { pick = state.models[j].path; break; }
        }
        if (!pick && state.models.length) pick = state.models[0].path;
        state.selectedModel = pick;
    }

    /** selectModel — выбор модели кликом по строке списка. */
    function selectModel(el, path) {
        state.selectedModel = path || '';
        // Смена модели делает прежние вердикты шагов неактуальными: показывать
        // «нормализовано» от прошлой модели рядом с новой — прямая ложь.
        state.inspect = null;
        state.normalize = null;
        state.load = null;
        renderModels(el);
        renderInspect(el);
        renderNormalize(el);
        renderLoad(el);
    }

    function renderModels(el) {
        var tbody = el.querySelector('#rxModelsBody');
        if (!tbody) return;
        if (!state.models.length) {
            var why = state.modelsError
                ? t('reexport.models_failed') + ': ' + state.modelsError
                : t('reexport.no_models');
            tbody.innerHTML = '<tr><td colspan="2" style="color:var(--text-muted);">' + esc(why) + '</td></tr>';
            return;
        }
        tbody.innerHTML = state.models.map(function (m) {
            var v = verdictFor(m);
            var selected = (m.path === state.selectedModel) ? ' data-selected="1"' : '';
            var bg = (m.path === state.selectedModel) ? 'var(--bg-secondary)' : 'transparent';
            var meta = [];
            meta.push(m.path);
            if (m.size_bytes) meta.push(formatBytes(m.size_bytes));
            if (m.arch) meta.push(m.arch);
            if (m.block_count) meta.push('block_count=' + m.block_count);
            if (m.physical_blocks) meta.push(t('reexport.fact_physical') + '=' + m.physical_blocks);
            return '<tr data-model-path="' + esc(m.path) + '"' + selected +
                ' style="cursor:pointer;background:' + bg + ';">' +
                '<td><code>' + esc(m.name || m.path) + '</code>' +
                    '<div style="font-size:var(--font-size-xs);color:var(--text-muted);">' +
                        esc(meta.join(' · ')) + '</div></td>' +
                '<td><span class="rx-badge rx-badge-' + esc(v.kind) + '">' + esc(verdictBadge(v.kind)) + '</span>' +
                    '<div style="font-size:var(--font-size-xs);color:var(--text-muted);margin-top:2px;">' +
                        esc(v.text) + '</div></td>' +
                '</tr>';
        }).join('');
    }

    /** currentModel — выбранная модель (объект списка сервиса). */
    function currentModel() {
        for (var i = 0; i < state.models.length; i++) {
            if (state.models[i].path === state.selectedModel) return state.models[i];
        }
        return null;
    }

    /**
     * modelHandle — имя модели так, как его знает воркер.
     *
     * cppworker регистрирует модель как basename(path) без ".gguf" (R66d), а
     * балансер ищет её по этому же имени. Запрос с расширением давал рассинхрон:
     * загрузка шла, а статус не находился.
     */
    function modelHandle(model) {
        var name = (model && (model.name || model.path)) || '';
        name = String(name).split('/').pop().split('\\').pop();
        return name.replace(/\.gguf$/i, '');
    }

    // =====================================================================
    // Слой задач (js/modules/gguf-reexport-jobs.js): транспорт, поллинг, таблица
    // =====================================================================

    var jobsLayer = null;
    var jobsWarned = false;

    /**
     * jobs — ленивый резолвер слоя задач.
     *
     * ЗАЧЕМ ЧЕРЕЗ ФАБРИКУ. Слой берёт состояние и помощники отсюда (контракт
     * фабрики проверяет scripts/webui_ctx_contract_check.js), поэтому список
     * зависимостей нельзя «забыть»: он либо сходится, либо чекер краснеет.
     *
     * ЕСЛИ ФАЙЛА НЕТ. Страница могла приехать со старым набором скриптов —
     * падать из-за этого нельзя: возвращаем null, один раз предупреждаем в
     * консоль и честно говорим оператору, что шаги проверки/нормализации
     * недоступны (showJobsLayerMissing), вместо ReferenceError в обработчике.
     */
    function jobs() {
        if (jobsLayer) return jobsLayer;
        if (typeof window === 'undefined' || typeof window.GgufReexportJobs !== 'function') {
            if (!jobsWarned) {
                jobsWarned = true;
                if (typeof console !== 'undefined' && console.warn) {
                    console.warn('[gguf-reexport] слой задач не загружен ' +
                        '(js/modules/gguf-reexport-jobs.js): таблица задач, проверка и ' +
                        'нормализация недоступны');
                }
            }
            return null;
        }
        jobsLayer = window.GgufReexportJobs({
            state: state,
            host: host,
            API: API,
            requestJSON: requestJSON,
            authHeaders: authHeaders,
            esc: esc,
            t: t,
            showError: showError,
            formatBytes: formatBytes,
            formatDuration: formatDuration,
            currentModel: currentModel,
            clearBlock: clearBlock
        });
        return jobsLayer;
    }

    /** showJobsLayerMissing — честный статус вместо тихой поломки шагов. */
    function showJobsLayerMissing(el) {
        var tbody = el.querySelector('#rxJobsBody');
        if (tbody) {
            tbody.innerHTML = '<tr><td colspan="6" style="color:var(--text-muted);">' +
                esc(t('reexport.jobs_module_missing')) + '</td></tr>';
        }
        var inspectBtn = el.querySelector('#rxInspect');
        if (inspectBtn) inspectBtn.disabled = true;
        var normalizeBtn = el.querySelector('#rxNormalize');
        if (normalizeBtn) normalizeBtn.disabled = true;
    }

    /** renderInspect / renderNormalize / loadJobs — тонкие обёртки над слоем. */
    function renderInspect(el) {
        if (jobs()) jobs().renderInspect(el);
    }

    function renderNormalize(el) {
        if (jobs()) jobs().renderNormalize(el);
    }

    function loadJobs() {
        if (jobs()) return jobs().loadJobs();
        return Promise.resolve();
    }

    function schedulePolling() {
        if (jobs()) jobs().schedulePolling();
    }

    // =====================================================================
    // Тело задачи: что именно уходит в POST /api/reexport/jobs
    // =====================================================================

    /**
     * buildJobBody — тело POST /api/reexport/jobs (чистая функция: её проверяет тест).
     *
     * mode: 'inspect' — проверка (dry_run, файл не пишется); 'normalize' — запись.
     * Отдельного поля mode у сервиса нет (см. шапку файла), поэтому «режим» — это
     * набор опций одной и той же цели.
     */
    function buildJobBody(el, mode) {
        var opts = {};
        var fuse = el.querySelector('#rxFuse');
        var out = el.querySelector('#rxOut');
        if (fuse && fuse.checked) opts.fuse_loop_queries = true;
        if (mode === 'inspect') opts.dry_run = true;
        if (out && out.value && out.value.trim()) opts.out_path = out.value.trim();
        return {
            model_dir: state.selectedModel,
            target: PATCH_TARGET,
            options: opts
        };
    }

    // =====================================================================
    // Шаг 2: проверка схемы (inspect = dry_run)
    // =====================================================================

    function runInspect(el) {
        var model = currentModel();
        if (!model) {
            showError(el, t('reexport.need_model'));
            return Promise.resolve();
        }
        var layer = jobs();
        if (!layer) {
            showError(el, t('reexport.jobs_module_missing'));
            return Promise.resolve();
        }
        showError(el, '');
        state.inspect = { status: 'running', jobId: '', job: null, error: '' };
        renderInspect(el);
        var btn = el.querySelector('#rxInspect');
        if (btn) btn.disabled = true;
        return layer.createJob(buildJobBody(el, 'inspect')).then(function (job) {
            state.inspect.jobId = job && job.id ? job.id : '';
            return layer.waitJob(state.inspect.jobId, 'inspectTimer', function (view) {
                state.inspect.job = view;
                state.inspect.status = view.status;
                renderInspect(el);
            });
        }).then(function (final) {
            state.inspect.job = final;
            state.inspect.status = final.status;
            renderInspect(el);
        }).catch(function (err) {
            state.inspect.status = 'failed';
            state.inspect.error = (err && err.message) ? err.message : String(err);
            renderInspect(el);
        }).finally(function () {
            if (btn) btn.disabled = false;
        });
    }

    // =====================================================================
    // Шаг 3: нормализация
    // =====================================================================

    function runNormalize(el) {
        var model = currentModel();
        if (!model) {
            showError(el, t('reexport.need_model'));
            return Promise.resolve();
        }
        // Слой задач нужен ЛЮБОМУ ответу этого шага: и задаче, и мгновенному
        // «уже нормализовано» (его рисует тот же слой). Нет слоя — говорим об
        // этом, а не молчим.
        var layer = jobs();
        if (!layer) {
            showError(el, t('reexport.jobs_module_missing'));
            return Promise.resolve();
        }
        showError(el, '');
        // Уже в схеме загрузчика — задача не нужна, и это ответ СРАЗУ, а не через
        // 15 минут копирования: сервис отказал бы на сверке визитов, но оператору
        // нужно слово «нормализовано», а не код ошибки после часов работы.
        if (model.scheme === 'inkling2_loop' && !model.read_error) {
            state.normalize = { status: 'skipped', jobId: '', job: null, error: '', message: t('reexport.normalize_already') };
            renderNormalize(el);
            return Promise.resolve();
        }
        state.normalize = { status: 'running', jobId: '', job: null, error: '', message: '' };
        renderNormalize(el);
        var btn = el.querySelector('#rxNormalize');
        if (btn) btn.disabled = true;
        return layer.createJob(buildJobBody(el, 'normalize')).then(function (job) {
            state.normalize.jobId = job && job.id ? job.id : '';
            return layer.waitJob(state.normalize.jobId, 'normalizeTimer', function (view) {
                state.normalize.job = view;
                state.normalize.status = view.status;
                renderNormalize(el);
            });
        }).then(function (final) {
            state.normalize.job = final;
            state.normalize.status = final.status;
            if (final.status === 'canceled') state.normalize.message = t('reexport.normalize_canceled');
            renderNormalize(el);
        }).catch(function (err) {
            state.normalize.status = 'failed';
            state.normalize.error = (err && err.message) ? err.message : String(err);
            renderNormalize(el);
        }).finally(function () {
            if (btn) btn.disabled = false;
        });
    }

    /** cancelNormalize — отмена текущей задачи нормализации (DELETE). */
    function cancelNormalize(el) {
        var layer = jobs();
        var st = state.normalize;
        if (!layer || !st || !st.jobId) return Promise.resolve();
        return layer.cancelJobById(st.jobId).then(function (view) {
            if (view) {
                st.job = view;
                st.status = view.status;
            }
            renderNormalize(el);
        }).catch(function (err) {
            showError(el, t('reexport.cancel_failed') + ': ' + ((err && err.message) ? err.message : String(err)));
        });
    }

    // =====================================================================
    // Шаг 6 (Фаза 4): набор файлов → image-bundle
    // =====================================================================
    //
    // ЧТО ЗДЕСЬ ПРОИСХОДИТ И ПОЧЕМУ ЭТО НЕ «ЕЩЁ ОДНА КНОПКА НОРМАЛИЗАЦИИ».
    // Цель image_bundle берёт КАТАЛОГ уже скачанных файлов image-модели и
    // собирает из него bundle, который imageworker загрузит: роли файлов
    // определяются по заголовкам (R696 — тот же код, что на воркере), пишутся
    // profile.json и sd-server.config.json, вердикт проверяется через imageworker.
    // Оператору не нужно ни знать имена флагов движка, ни собирать профиль
    // руками — но и «собрать» тут не значит «починить»: если набора не хватает,
    // сервис честно назовёт отсутствующие роли, а не сделает вид, что всё готово.

    /** bundleTargetSpec — цель сборки bundle из возможностей сервиса. */
    function bundleTargetSpec() {
        var targets = (state.caps && state.caps.targets) || [];
        for (var i = 0; i < targets.length; i++) {
            if (targets[i] && targets[i].id === BUNDLE_TARGET) return targets[i];
        }
        return null;
    }

    /**
     * buildBundleBody — тело POST для сборки bundle (чистая функция: её проверяет тест).
     *
     * dryRun=true — «Проверить набор»: сервис читает заголовки и отдаёт роли,
     * семейство и вердикт, НЕ копируя файлы. dryRun=false — запись.
     */
    function buildBundleBody(el, dryRun) {
        var dir = el.querySelector('#rxBundleDir');
        var name = el.querySelector('#rxBundleName');
        var opts = {};
        if (dryRun) opts.dry_run = true;
        if (name && name.value && name.value.trim()) opts.name = name.value.trim();
        return {
            model_dir: dir && dir.value ? dir.value.trim() : '',
            target: BUNDLE_TARGET,
            options: opts
        };
    }

    /** renderBundle — состояние шага 6 и доступность цели. */
    function renderBundle(el) {
        var status = el.querySelector('#rxBundleStatus');
        var result = el.querySelector('#rxBundleResult');
        if (!status || !result) return;
        var spec = bundleTargetSpec();
        var available = !!(spec && spec.available !== false);
        var checkBtn = el.querySelector('#rxBundleCheck');
        var buildBtn = el.querySelector('#rxBundleBuild');
        var running = !!(state.bundle && state.bundle.status === 'running');
        if (checkBtn) checkBtn.disabled = !available || running;
        if (buildBtn) buildBtn.disabled = !available || running;
        var st = state.bundle;
        if (!st) {
            status.textContent = available
                ? ''
                : (t('reexport.bundle_unavailable') + (spec && spec.reason ? ' — ' + spec.reason : ''));
            result.innerHTML = '';
            return;
        }
        var bits = [];
        if (st.status === 'running') bits.push(t('reexport.bundle_running'));
        else if (st.status === 'succeeded') bits.push(t('reexport.bundle_done'));
        else bits.push(t('reexport.bundle_failed'));
        if (st.jobId) bits.push(st.jobId);
        if (st.error) bits.push(st.error);
        status.textContent = bits.join(' · ');
        result.innerHTML = bundleResultHTML(st);
    }

    /**
     * bundleResultHTML — итог сборки: роли, семейство и ВЕРДИКТ ВОРКЕРА.
     *
     * ПОЧЕМУ РОЛИ В ИТОГЕ, А НЕ ТОЛЬКО «успешно»: именно они объясняют, что
     * сервис понял про набор. Ошибка раскладки (энкодер принят за diffusion)
     * видна здесь до нажатия «Загрузить», а не в логе движка.
     */
    function bundleResultHTML(st) {
        var job = st && st.job;
        if (!job) return '';
        if (job.error && job.error.message) {
            return '<div class="rx-error-text" style="color:var(--danger, #ef4444);">' +
                esc(jobErrorText(job.error, job)) + '</div>';
        }
        var facts = (job.result && job.result.facts) || {};
        var rows = [];
        if (st.dryRun) rows.push([t('reexport.bundle_plan_only'), t('reexport.yes')]);
        if (facts.bundle_path) rows.push([t('reexport.result_path'), facts.bundle_path]);
        if (facts.family) rows.push([t('reexport.bundle_family'), facts.family]);
        if (facts.compat_verdict) rows.push([t('reexport.bundle_verdict'), facts.compat_verdict]);
        if (facts.compat_reason) rows.push([t('reexport.bundle_reason'), facts.compat_reason]);
        if (facts.missing && facts.missing.length) {
            rows.push([t('reexport.bundle_missing'), facts.missing.join(', ')]);
        }
        rows.push([t('reexport.bundle_worker'),
            facts.worker_checked ? t('reexport.yes') : t('reexport.bundle_worker_local')]);

        var html = '';
        if (rows.length) {
            html += '<div class="rx-facts" style="display:flex;gap:14px;flex-wrap:wrap;margin:6px 0;">' +
                rows.map(function (r) {
                    return '<span><span style="color:var(--text-muted);">' + esc(r[0]) + ':</span> ' +
                        esc(r[1]) + '</span>';
                }).join('') + '</div>';
        }
        var roles = facts.roles || {};
        var names = Object.keys(roles).sort();
        if (names.length) {
            html += '<div style="margin-top:4px;font-size:var(--font-size-xs);color:var(--text-muted);">' +
                esc(t('reexport.bundle_roles')) + ':</div>' +
                '<ul style="margin:2px 0 0 16px;font-size:var(--font-size-xs);">' +
                names.map(function (n) {
                    return '<li><code>' + esc(n) + '</code> — ' + esc(roles[n]) + '</li>';
                }).join('') + '</ul>';
        }
        return html;
    }

    /** runBundle — постановка задачи сборки bundle и ожидание терминального статуса. */
    function runBundle(el, dryRun) {
        var layer = jobs();
        if (!layer) {
            showError(el, t('reexport.jobs_module_missing'));
            return Promise.resolve();
        }
        var body = buildBundleBody(el, dryRun);
        if (!body.model_dir) {
            showError(el, t('reexport.bundle_need_dir'));
            return Promise.resolve();
        }
        showError(el, '');
        state.bundle = { status: 'running', jobId: '', job: null, error: '', dryRun: !!dryRun };
        renderBundle(el);
        return layer.createJob(body).then(function (job) {
            state.bundle.jobId = job && job.id ? job.id : '';
            return layer.waitJob(state.bundle.jobId, 'bundleTimer', function (view) {
                state.bundle.job = view;
                state.bundle.status = view.status;
                renderBundle(el);
            });
        }).then(function (final) {
            state.bundle.job = final;
            state.bundle.status = final.status;
            renderBundle(el);
        }).catch(function (err) {
            state.bundle.status = 'failed';
            state.bundle.error = (err && err.message) ? err.message : String(err);
            renderBundle(el);
        });
    }

    // =====================================================================
    // Шаг 4: пересканирование моделей
    // =====================================================================

    /**
     * rescanModels — «Пересканировать модели» после успешного ре-экспорта.
     *
     * ПОЧЕМУ КНОПКА НУЖНА. Ре-экспорт кладёт НОВЫЙ файл в том моделей, но воркер
     * держит свой каталог в памяти (инвентарь с TTL) и без POST /api/models/refresh
     * просто не знает о файле — «задача успешна, а модели нет». Кнопка зовёт
     * СУЩЕСТВУЮЩИЙ механизм страницы GGUF
     * (GgufModule.rescanAllBackendModels → /api/models/refresh на каждом воркере),
     * а не свой запрос: правило пересканирования должно быть одно.
     *
     * Плюс сбрасываем кэш фактов сервиса (?refresh=1), чтобы список увидел новый
     * размер и схему файла.
     */
    function rescanModels(el) {
        state.rescan = { status: 'running', message: t('reexport.rescan_running') };
        renderRescan(el);
        var mod = (typeof window !== 'undefined') ? window.GgufModule : null;
        var p;
        if (mod && typeof mod.rescanAllBackendModels === 'function') {
            try {
                p = Promise.resolve(mod.rescanAllBackendModels());
            } catch (e) {
                p = Promise.reject(e);
            }
        } else {
            p = Promise.reject(new Error(t('reexport.rescan_unavailable')));
        }
        return p.then(function () {
            return loadModels(true);
        }).then(function () {
            state.rescan = { status: 'ok', message: t('reexport.rescan_done') };
            renderRescan(el);
        }).catch(function (err) {
            var why = (err && err.message) ? err.message : String(err);
            state.rescan = { status: 'failed', message: t('reexport.rescan_failed') + ': ' + why };
            renderRescan(el);
            // Каталог самого сервиса всё равно перечитываем: даже без воркеров
            // список должен показать свежие факты файла.
            loadModels(true);
        });
    }

    function renderRescan(el) {
        var node = el.querySelector('#rxRescanStatus');
        if (!node) return;
        node.textContent = state.rescan ? state.rescan.message : '';
    }

    // =====================================================================
    // Шаг 5: проверка загрузки через балансер
    // =====================================================================

    function ggufApi() {
        return (typeof window !== 'undefined' && window.GgufApi) ? window.GgufApi : null;
    }

    /** loadBackendId — узел: выбранный на странице GGUF (пустой = проверить нельзя). */
    function loadBackendId() {
        var a = ggufApi();
        if (a && typeof a.getBackendId === 'function') {
            var id = a.getBackendId();
            if (id) return String(id);
        }
        var mod = (typeof window !== 'undefined') ? window.GgufModule : null;
        if (mod && mod.state && mod.state.selectedBackendId) return String(mod.state.selectedBackendId);
        return '';
    }

    /** workerModels — GET /api/models воркера через прокси балансера. */
    function workerModels(backendId) {
        var a = ggufApi();
        if (a && typeof a.requestViaBackend === 'function') {
            return Promise.resolve(a.requestViaBackend(backendId, '/api/models'));
        }
        return requestJSON('/api/v1/gguf/backends/' + encodeURIComponent(backendId) + '/proxy/api/models', {
            method: 'GET', headers: authHeaders(), credentials: 'same-origin', timeoutMs: 20000
        });
    }

    /** loadProgress — GET /api/models/load/progress воркера через прокси балансера. */
    function loadProgress(backendId, handle) {
        var a = ggufApi();
        if (a && typeof a.getLoadProgressViaBackend === 'function') {
            return Promise.resolve(a.getLoadProgressViaBackend(backendId, handle));
        }
        return requestJSON('/api/v1/gguf/backends/' + encodeURIComponent(backendId) +
            '/proxy/api/models/load/progress?model=' + encodeURIComponent(handle), {
            method: 'GET', headers: authHeaders(), credentials: 'same-origin', timeoutMs: 15000
        });
    }

    /**
     * nodeLimits — память узла из ответа его же /api/models.
     *
     * ЗАЧЕМ ОТСЮДА. Воркер уже публикует total_ram_mb / total_vram_mb /
     * available_*_mb — это единственный источник, который знает ФАКТИЧЕСКИЙ узел
     * (а не наш процесс). Отдельного запроса не нужно: этот ответ мы и так читаем
     * перед загрузкой, чтобы показать, что уже загружено.
     */
    function nodeLimits(workerPayload) {
        var p = workerPayload || {};
        var ram = Number(p.total_ram_mb) || 0;
        var vram = Number(p.total_vram_mb) || 0;
        if (!ram && !vram) return null;
        return {
            ramTotalMb: ram,
            vramTotalMb: vram,
            ramFreeMb: Number(p.available_ram_mb) || 0,
            vramFreeMb: Number(p.available_vram_mb) || 0
        };
    }

    /**
     * loadWarning — предупреждение «модель слишком большая для узла».
     *
     * Два разных отказа не смешиваются: (1) модель больше RAM — грузить некуда
     * вообще; (2) модель больше VRAM — она загрузится, но часть слоёв уйдёт в
     * память CPU (offload_degraded), и скорость будет в разы ниже. Оператор должен
     * узнать это ДО кнопки, а не из «почему так медленно» после.
     */
    function loadWarning(sizeBytes, node) {
        var size = Number(sizeBytes) || 0;
        if (!size || !node || !node.ramTotalMb) return null;
        var ramBytes = node.ramTotalMb * 1024 * 1024;
        var vramBytes = (node.vramTotalMb || 0) * 1024 * 1024;
        if (size > ramBytes) {
            return { kind: 'ram', size: size, ram: ramBytes };
        }
        if (vramBytes && size > vramBytes) {
            return { kind: 'vram', size: size, vram: vramBytes };
        }
        return null;
    }

    function warningHTML(w) {
        if (!w) return '';
        var text = '';
        if (w.kind === 'ram') text = t('reexport.load_warn_ram', { size: formatBytes(w.size), ram: formatBytes(w.ram) });
        else if (w.kind === 'vram') text = t('reexport.load_warn_vram', { size: formatBytes(w.size), vram: formatBytes(w.vram) });
        if (!text) return '';
        return '<div class="rx-warn-text"><i class="fas fa-triangle-exclamation" aria-hidden="true"></i> ' +
            esc(text) + '</div>';
    }

    function findWorkerModel(payload, handle) {
        var list = (payload && payload.models) || [];
        for (var i = 0; i < list.length; i++) {
            var m = list[i] || {};
            var name = String(m.name || m.model || '').split('/').pop();
            name = name.replace(/\.gguf$/i, '');
            if (name === handle) return m;
        }
        return null;
    }

    /**
     * checkLoad — пробная загрузка модели через балансер.
     *
     * МЕХАНИЗМ — существующий: POST /api/v1/backends/{id}/models {operation:"load"}
     * (webui/js/modules/gguf-api.js → manageModel), статус — GET
     * /api/v1/gguf/backends/{id}/proxy/api/models/load/progress (тот же, что polлит
     * GgufLoadProgress), факты после загрузки — /api/models воркера (offload_degraded,
     * offload_note). Своего эндпоинта модуль не изобретает.
     */
    function checkLoad(el) {
        var model = currentModel();
        if (!model) {
            showError(el, t('reexport.need_model'));
            return Promise.resolve();
        }
        showError(el, '');
        var backend = loadBackendId();
        if (!backend) {
            state.load = { status: 'failed', backend: '', handle: modelHandle(model), model: model, error: t('reexport.load_no_backend'), node: null, offload: null, warn: null };
            renderLoad(el);
            return Promise.resolve();
        }
        var handle = modelHandle(model);
        state.load = {
            status: 'loading', backend: backend, handle: handle, model: model,
            error: '', node: null, offload: null, warn: null, workerState: ''
        };
        renderLoad(el);
        var btn = el.querySelector('#rxCheckLoad');
        if (btn) btn.disabled = true;
        stopLoadPolling();

        // 1. Память узла и что уже загружено — до нажатия «грузить» (предупреждение
        //    о размере должно быть видно раньше, чем начнётся загрузка).
        return workerModels(backend).then(function (payload) {
            state.load.node = nodeLimits(payload);
            state.load.offload = findWorkerModel(payload, handle);
            state.load.warn = loadWarning(model.size_bytes, state.load.node);
            renderLoad(el);
        }).catch(function () {
            state.load.node = null;
            renderLoad(el);
        }).then(function () {
            // 2. Сама загрузка: балансер держит ответ до терминального состояния.
            //    Пока он ждёт — опрашиваем прогресс воркера, чтобы «loading» было
            //    видно живьём, а не только в конце.
            pollLoadProgress(el, backend, handle);
            return startLoad(backend, handle);
        }).then(function (result) {
            return finishLoad(el, backend, handle, result);
        }).catch(function (err) {
            stopLoadPolling();
            state.load.status = 'failed';
            state.load.error = (err && err.message) ? err.message : String(err);
            renderLoad(el);
        }).finally(function () {
            stopLoadPolling();
            if (btn) btn.disabled = false;
        });
    }

    /** startLoad — POST загрузки через балансер (существующий manageModel). */
    function startLoad(backendId, handle) {
        var a = ggufApi();
        if (a && typeof a.manageModel === 'function') {
            return Promise.resolve(a.manageModel(backendId, 'load', handle, {}));
        }
        return requestJSON('/api/v1/backends/' + encodeURIComponent(backendId) + '/models', {
            method: 'POST',
            headers: authHeaders({ 'Content-Type': 'application/json' }),
            credentials: 'same-origin',
            timeoutMs: 0,
            body: JSON.stringify({ operation: 'load', modelName: handle })
        });
    }

    /** pollLoadProgress — статус загрузки у воркера (loading → loaded|failed). */
    function pollLoadProgress(el, backendId, handle) {
        state.load.loadAttempts = 0;
        function tick() {
            if (!state.load || state.load.status !== 'loading') return;
            loadProgress(backendId, handle).then(function (data) {
                if (!state.load) return;
                var entry = findWorkerModel(data, handle);
                if (entry) {
                    state.load.workerState = String(entry.state || '');
                    if (entry.error) state.load.error = String(entry.error);
                    renderLoad(el);
                }
            }).catch(function () {
                // Пока загрузка не зарегистрирована, воркер отвечает 404 «not
                // found and not loading» — это НЕ отказ, а «ещё не началось».
            }).finally(function () {
                if (!state.load || state.load.status !== 'loading') return;
                state.load.loadAttempts = (state.load.loadAttempts || 0) + 1;
                if (state.load.loadAttempts >= LOAD_POLL_MAX) {
                    renderLoad(el);
                    return;
                }
                state.loadTimer = setTimeout(function () {
                    state.loadTimer = null;
                    tick();
                }, LOAD_POLL_MS);
            });
        }
        tick();
    }

    /** finishLoad — терминальный итог: статус, причина, offload-факты. */
    function finishLoad(el, backendId, handle, result) {
        stopLoadPolling();
        if (result && result.success === false) {
            if (result.stillLoading === true) {
                // 202 «ещё грузится» — не отказ: оставляем «loading» и поллим дальше.
                state.load.status = 'loading';
                renderLoad(el);
                return Promise.resolve();
            }
            state.load.status = 'failed';
            state.load.error = result.error || t('reexport.load_state_failed');
            renderLoad(el);
            return Promise.resolve();
        }
        return workerModels(backendId).then(function (payload) {
            state.load.node = nodeLimits(payload) || state.load.node;
            var entry = findWorkerModel(payload, handle);
            if (entry) {
                state.load.offload = entry;
                state.load.workerState = String(entry.state || 'loaded');
            }
            state.load.status = 'loaded';
            renderLoad(el);
        }).catch(function () {
            state.load.status = 'loaded';
            renderLoad(el);
        });
    }

    function stopLoadPolling() {
        if (state.loadTimer) {
            clearTimeout(state.loadTimer);
            state.loadTimer = null;
        }
    }

    function renderLoad(el) {
        var status = el.querySelector('#rxLoadStatus');
        var warn = el.querySelector('#rxLoadWarn');
        var result = el.querySelector('#rxLoadResult');
        if (!status || !warn || !result) return;
        var st = state.load;
        if (!st) {
            status.textContent = '';
            warn.innerHTML = '';
            result.innerHTML = '';
            return;
        }
        var bits = [];
        var stateText = '';
        if (st.status === 'loading') stateText = t('reexport.load_state_loading');
        else if (st.status === 'loaded') stateText = t('reexport.load_state_loaded');
        else stateText = t('reexport.load_state_failed');
        bits.push((st.backend ? (t('reexport.load_backend') + ': ' + st.backend + ' · ') : '') + stateText);
        if (st.handle) bits.push(st.handle);
        if (st.workerState && st.workerState !== st.status) bits.push(st.workerState);
        status.textContent = bits.join(' · ');

        // Предупреждение о размере — до/независимо от итога.
        var wHTML = '';
        if (st.warn) {
            wHTML += warningHTML(st.warn);
        } else if (!st.node && st.status !== 'failed') {
            wHTML += '<div style="font-size:var(--font-size-xs);color:var(--text-muted);">' +
                esc(t('reexport.load_node_unknown')) + '</div>';
        }
        if (st.node && st.node.ramTotalMb) {
            wHTML += '<div style="font-size:var(--font-size-xs);color:var(--text-muted);">' +
                esc(t('reexport.load_node_limits', {
                    ram: formatBytes(st.node.ramTotalMb * 1024 * 1024),
                    vram: formatBytes((st.node.vramTotalMb || 0) * 1024 * 1024)
                })) + '</div>';
        }
        warn.innerHTML = wHTML;

        var rows = [];
        if (st.error) rows.push([t('reexport.load_reason'), st.error]);
        if (st.offload) {
            if (st.offload.offload_degraded) {
                rows.push([t('reexport.load_offload_degraded'), st.offload.offload_note || '']);
            }
            if (typeof st.offload.gpu_layers_actual === 'number') {
                rows.push([t('reexport.load_gpu_layers'), st.offload.gpu_layers_actual]);
            }
        }
        if (!rows.length) {
            result.innerHTML = '';
            return;
        }
        result.innerHTML = '<div class="rx-facts" style="display:flex;gap:14px;flex-wrap:wrap;">' +
            rows.map(function (r) {
                return '<span><span style="color:var(--text-muted);">' + esc(r[0]) + ':</span> ' + esc(r[1]) + '</span>';
            }).join('') + '</div>';
    }

    /** stopPolling — гасит ВСЁ: используется при уходе со страницы (clearBlock). */
    function stopPolling() {
        var keys = ['inspectTimer', 'normalizeTimer', 'loadTimer', 'bundleTimer'];
        for (var i = 0; i < keys.length; i++) {
            if (state[keys[i]]) {
                clearTimeout(state[keys[i]]);
                state[keys[i]] = null;
            }
        }
        // Поллинг таблицы задач живёт в своём слое: его таймер гасит он же.
        if (jobs()) jobs().stopJobsPolling();
    }

    window.GgufReexport = {
        mount: mount,
        _state: state,
        _buildHTML: buildHTML,
        _buildJobBody: buildJobBody,
        _buildBundleBody: buildBundleBody,
        _renderBundle: renderBundle,
        _runBundle: runBundle,
        _verdictFor: verdictFor,
        _renderModels: renderModels,
        _renderInspect: renderInspect,
        _renderNormalize: renderNormalize,
        _renderRescan: renderRescan,
        _renderLoad: renderLoad,
        _renderJobs: function (el) { if (jobs()) jobs().renderJobs(el); },
        // Тестовый шов: правило «пустой список задач не гасит ожидание шага»
        // проверяется напрямую (см. webui/tests/reexport-scenario-d11.test.js).
        _schedulePolling: schedulePolling,
        _loadWarning: loadWarning,
        _nodeLimits: nodeLimits,
        _phaseLabel: function (phase) { return jobs() ? jobs().phaseLabel(phase) : String(phase || ''); },
        _modelHandle: modelHandle,
        _jobErrorText: function (err, job) {
            return jobs() ? jobs().jobErrorText(err, job) : ((err && err.message) ? err.message : '');
        },
        _formatBytes: formatBytes,
        _formatDuration: formatDuration,
        _statusLabel: function (status) {
            return jobs() ? jobs().statusLabel(status) : String(status || '');
        },
        _probeOnce: probeOnce,
        // Тестовый шов: слой задач создаётся один раз на страницу; тест «слой не
        // загружен» подменяет window.GgufReexportJobs и требует пересоздания.
        _resetJobsLayer: function () { jobsLayer = null; },
        _stopPolling: stopPolling
    };
})();

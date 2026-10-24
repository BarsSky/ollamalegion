/**
 * setup-wizard.js — Initial Server Setup Wizard (6 steps + import)
 * OllamaLegion WebUI
 *
 * === ARCHITECTURE ===
 * wizardState — изолированное состояние, накапливающее выборы пользователя.
 * НИКАКИЕ изменения не отправляются на сервер до финального шага (Finish).
 * finishWizard() собирает wizardState + поля форм → один PUT на сервер.
 *
 * Флаг initialized хранится на сервере (config/config.json), а не в localStorage.
 */
(function () {
    'use strict';
    var currentStep = 1;
    // R195 (2026-10-20): шаг «Тип бэкенда» УДАЛЁН — стек обслуживает оба типа одновременно
    // (llama.cpp-воркер отдаёт и нативный Ollama API, и OpenAI-совместимую поверхность).
    var totalSteps = 4;
    // R195: состояние серверного конфига (объявление восстановлено — при удалении шага
    // «Тип бэкенда» оно было потеряно, и start() падал с "currentServerConfig is not defined").
    var currentServerConfig = null;
    var wizardState = {
        backendType: 'llama_cpp',      // R195: выбор типа удалён — стек обслуживает оба типа одновременно
        // настройки общего характера (шаг 4)
        algorithm: 'resource-aware',
        gpuMaxUsage: 90,
        vramMaxUsage: 85,
        cpuMaxUsage: 80,
        ramMaxUsage: 85
        // R107 (чистовая уборка S5/S7): поля режимов (operatingMode, replication,
        // rpcCoordinator, virtualModels, distInference) убраны вместе с шагом
        // «параметры режима». Раскладку задаёт политика размещения (шаг «состав
        // кластера» → balancing.placement), а не режим работы.
    };

    /**
     * Проверяет, выполнена ли первичная настройка (обращение к серверу)
     */
    function isInitialized() {
        if (!window.Api || !window.Api.config) {
            return Promise.resolve(!!localStorage.getItem('ollamalegion_wizard_done'));
        }
        return window.Api.config().then(function (cfg) {
            currentServerConfig = cfg;
            var srvInit = cfg.initialized === true;
            if (srvInit) {
                localStorage.setItem('ollamalegion_wizard_done', '1');
            }
            return srvInit;
        }).catch(function () {
            return !!localStorage.getItem('ollamalegion_wizard_done');
        });
    }

    function markInitialized() {
        if (window.Api && window.Api.updateConfig) {
            return window.Api.updateConfig({ initialized: true });
        }
        return Promise.reject(new Error('API not available'));
    }

    function reset() {
        if (window.Api && window.Api.updateConfig) {
            return window.Api.updateConfig({ initialized: false });
        }
        return Promise.reject(new Error('API not available'));
    }

    function fetchServerConfig() {
        if (window.Api && window.Api.config) {
            return window.Api.config().then(function (cfg) {
                currentServerConfig = cfg;
                return cfg;
            });
        }
        return Promise.resolve({});
    }

    // ================================================================
    // WIZARD LIFECYCLE
    // ================================================================

    function start() {
        // Инициализируем wizardState. Приоритет: localStorage > серверный конфиг > ollama.
        var savedType = localStorage.getItem('ollamalegion_backend_type');
        var initialType = (savedType === 'llama_cpp' || savedType === 'ollama') ? savedType : 'ollama';

        wizardState = {
            backendType: 'llama_cpp',       // R195: тип больше не выбирается (стек обслуживает оба)
            algorithm: 'resource-aware',
            gpuMaxUsage: 90,
            vramMaxUsage: 85,
            cpuMaxUsage: 80,
            ramMaxUsage: 85
        };

        currentStep = 1;
        var existing = document.getElementById('setupWizardModal');
        if (existing) existing.remove();

        fetchServerConfig().then(function () {
            // Подгружаем дефолты из серверного конфига если есть.
            // ВАЖНО: backendType НЕ перезаписываем из сервера —
            // доверяем localStorage (initialType), установленному выше.
            if (currentServerConfig) {
                // R107: operatingMode из сервера больше не читаем — режим не
                // выбирается в мастере, раскладку задаёт политика размещения.
                if (currentServerConfig.algorithm) wizardState.algorithm = currentServerConfig.algorithm;
                if (currentServerConfig.gpuMaxUsage) wizardState.gpuMaxUsage = currentServerConfig.gpuMaxUsage;
                if (currentServerConfig.vramMaxUsage) wizardState.vramMaxUsage = currentServerConfig.vramMaxUsage;
                if (currentServerConfig.cpuMaxUsage) wizardState.cpuMaxUsage = currentServerConfig.cpuMaxUsage;
                if (currentServerConfig.ramMaxUsage) wizardState.ramMaxUsage = currentServerConfig.ramMaxUsage;
            }

            var modal = document.createElement('div');
            modal.className = 'modal active';
            modal.id = 'setupWizardModal';
            modal.style.display = 'flex';
            modal.innerHTML = buildWizardHTML();
            document.body.appendChild(modal);

            renderStep(currentStep);
            bindWizardEvents(modal);
        });
    }

    function closeWizard() {
        var modal = document.getElementById('setupWizardModal');
        if (modal) modal.remove();
        wizardState = null;
    }

    // Session 17 P.11 (2026-07-27): helper для создания label с tooltip.
    // Возвращает HTML-строку, которую можно использовать в форме:
    //   <label>{LABEL} <span class="tooltip-trigger">?<span class="tooltip-content">{DESC}</span></span></label>
    // Если перевод не найден, label и desc показываются на английском (fallback).
    // Использует тот же CSS что и settings (pages.css .tooltip-trigger/.tooltip-content).
    //
    // R66b (2026-09-22): I18N.t() при отсутствии ключа возвращает САМ КЛЮЧ
    // (i18n/index.js:95-102), поэтому `|| fallback` не срабатывал и пользователь
    // видел сырой `wizard.hardware_preset` вместо подписи. Теперь результат,
    // совпавший с ключом, трактуется как «перевода нет».
    function tr(key, fallback) {
        if (!window.I18N) return fallback;
        var v = window.I18N.t(key, null);
        if (v === undefined || v === null || v === '' || v === key) return fallback;
        return v;
    }

    function t_label(labelKey, labelFallback, descKey, descFallback) {
        var lbl = tr(labelKey, labelFallback);
        var desc = tr(descKey, descFallback);
        return '<span class="tooltip-trigger" tabindex="0">?' +
            '<span class="tooltip-content">' + escapeHtml(desc) + '</span>' +
            '</span>' + escapeHtml(lbl);
    }

    function escapeHtml(s) {
        return String(s == null ? '' : s)
            .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
            .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
    }

    // ================================================================
    // RENDERING
    // ================================================================

    function buildWizardHTML() {
        return '<div class="wizard-modal">' +
            '<div class="wizard-header">' +
            '<div class="wizard-logo">' +
            '<img src="img/dark_logo.svg" alt="OllamaLegion" class="wizard-logo-img" style="height:32px;">' +
            '<span class="wizard-title">OllamaLegion — ' + (window.I18N ? I18N.t('wizard.title') : 'Setup Wizard') + '</span>' +
            '</div>' +
            '<div class="wizard-steps" id="wizardSteps"></div>' +
            '</div>' +
            '<div class="wizard-body" id="wizardBody"></div>' +
            '<div class="wizard-footer" id="wizardFooter"></div>' +
            '</div>';
    }

    function renderStep(step) {
        renderStepIndicator(step);
        renderStepContent(step);
        renderStepFooter(step);
    }

    function renderStepIndicator(step) {
        var container = document.getElementById('wizardSteps');
        if (!container) return;

        var t = window.I18N ? I18N.t.bind(I18N) : function(k) { return k; };
        var steps = [
            t('wizard.step1'),
            t('wizard.step_cluster'),
            t('wizard.step4'),
            t('wizard.step5')
        ];

        var html = '';
        for (var i = 1; i <= totalSteps; i++) {
            var cls = i === step ? 'wizard-step-dot active' : (i < step ? 'wizard-step-dot done' : 'wizard-step-dot');
            html += '<div class="' + cls + '" data-step="' + i + '">' +
                '<div class="wizard-step-number">' + (i < step ? '✓' : i) + '</div>' +
                '<div class="wizard-step-label">' + steps[i - 1] + '</div>' +
                '</div>';
            if (i < totalSteps) {
                html += '<div class="wizard-step-line ' + (i < step ? 'done' : '') + '"></div>';
            }
        }
        container.innerHTML = html;
    }

    function renderStepContent(step) {
        var body = document.getElementById('wizardBody');
        if (!body) return;

        var html = '';
        switch (step) {
            case 1: html = renderWelcomeStep(); break;
            case 2: html = renderClusterPlanStep(); break;
            case 3: html = renderGeneralSettingsStep(); break;
            case 4: html = renderSummaryStep(); break;
        }
        body.innerHTML = html;

        // R103 (S5 плана 2026-10-07): шаг «состав кластера» вместо выбора режима.
        // Панель (план с числами + запись balancing.placement) рисует модуль
        // setup-plan.js; здесь только монтирование. Режим работы на этом шаге
        // больше НЕ выбирается: раскладка задаётся политикой (S7 плана).
        if (step === 2 && window.SetupPlan) {
            window.SetupPlan.mount('setupPlanPanel').catch(function () {});
        }
        // R107: шаг «параметры режима» удалён вместе с выбором режима; из
        // настраиваемого остались общие параметры (шаг 4) и сводка (шаг 5).
        if (step === 3) {
            applyWizardStateToGeneralFields();
        }
    }

    function renderStepFooter(step) {
        var footer = document.getElementById('wizardFooter');
        if (!footer) return;

        var html = '<div class="wizard-nav">';
        if (step === 1) {
            html += '<button class="btn btn-secondary" id="wizardImportBtn">' +
                (window.I18N ? I18N.t('wizard.import') : '📂 Import Config') + '</button>';
            html += '<div style="flex:1;"></div>';
            html += '<button class="btn btn-primary" id="wizardNextBtn">' +
                (window.I18N ? I18N.t('wizard.next') : 'Start →') + '</button>';
        } else {
            html += '<button class="btn btn-secondary" id="wizardPrevBtn">' +
                (window.I18N ? I18N.t('wizard.prev') : '← Back') + '</button>';
            html += '<div style="flex:1;"></div>';
            if (step < totalSteps) {
                html += '<button class="btn btn-primary" id="wizardNextBtn">' +
                    (window.I18N ? I18N.t('wizard.next') : 'Next →') + '</button>';
            } else {
                html += '<button class="btn btn-primary" id="wizardFinishBtn">' +
                    (window.I18N ? I18N.t('wizard.finish') : '🚀 Start!') + '</button>';
            }
        }
        html += '</div>';
        footer.innerHTML = html;
    }

    // ================================================================
    // APPLY WIZARDSTATE TO FORM FIELDS
    // ================================================================


    function applyWizardStateToGeneralFields() {
        setFieldValue('balancingAlgorithm', wizardState.algorithm);
        setFieldValue('gpuMaxUsage', wizardState.gpuMaxUsage);
        setFieldValue('vramMaxUsage', wizardState.vramMaxUsage);
        setFieldValue('cpuMaxUsage', wizardState.cpuMaxUsage);
        setFieldValue('ramMaxUsage', wizardState.ramMaxUsage);
    }

    function readGeneralFieldsIntoState() {
        wizardState.algorithm = getFieldValue('balancingAlgorithm', 'resource-aware');
        wizardState.gpuMaxUsage = parseFloat(getFieldValue('gpuMaxUsage', 90));
        wizardState.vramMaxUsage = parseFloat(getFieldValue('vramMaxUsage', 85));
        wizardState.cpuMaxUsage = parseFloat(getFieldValue('cpuMaxUsage', 80));
        wizardState.ramMaxUsage = parseFloat(getFieldValue('ramMaxUsage', 85));
    }

    // ================================================================
    // STEP RENDERERS
    // ================================================================

    function renderWelcomeStep() {
        return '<div class="wizard-step-content welcome-step">' +
            '<div class="wizard-welcome-icon">🚀</div>' +
            '<h2>' + (window.I18N ? I18N.t('wizard.greeting') : 'Welcome to OllamaLegion!') + '</h2>' +
            '<p>' + (window.I18N ? I18N.t('wizard.greeting_desc') : 'Let\'s configure your load balancer for optimal performance.') + '</p>' +
            '<div class="wizard-import-box">' +
            '<p>' + (window.I18N ? I18N.t('wizard.import_desc') : 'If you have an existing configuration file, you can import it:') + '</p>' +
            '<button class="btn btn-secondary" id="wizardImportBtn2">' +
            (window.I18N ? I18N.t('wizard.import') : '📂 Import Configuration') + '</button>' +
            '</div>' +
            '</div>';
    }

    function renderBackendTypeStep() {
        var bt = wizardState.backendType;
        var t = window.I18N ? I18N.t.bind(I18N) : function(k) { return k; };

        return '<div class="wizard-step-content backend-type-step">' +
            '<h3>' + (t('wizard.step_backend_type') || 'Backend Type') + '</h3>' +
            '<p style="color:var(--text-muted);font-size:13px;margin-bottom:16px;">' +
                (t('wizard.backend_type_desc') || 'Select which inference engine your backends will use.') +
            '</p>' +
            '<div class="mode-cards">' +
            '<label class="mode-card' + (bt === 'ollama' ? ' active' : '') + '" data-type="ollama" data-backend-type="ollama">' +
                '<input type="radio" name="backendType" value="ollama" ' + (bt === 'ollama' ? 'checked' : '') + '>' +
                '<div class="mode-card-icon">🦙</div>' +
                '<div class="mode-card-title">Ollama</div>' +
                '<div class="mode-card-desc">' + (t('wizard.backend_type_ollama_desc') || 'Standard Ollama API.') + '</div>' +
            '</label>' +
            '<label class="mode-card' + (bt === 'llama_cpp' ? ' active' : '') + '" data-type="llama_cpp" data-backend-type="llama_cpp">' +
                '<input type="radio" name="backendType" value="llama_cpp" ' + (bt === 'llama_cpp' ? 'checked' : '') + '>' +
                '<div class="mode-card-icon">🦒</div>' +
                '<div class="mode-card-title">llama.cpp</div>' +
                '<div class="mode-card-desc">' + (t('wizard.backend_type_llama_desc') || 'llama.cpp via CppWorker.') + '</div>' +
            '</label>' +
            '</div>' +
            '</div>';
    }

    function renderClusterPlanStep() {
        return '<div class="wizard-step-content cluster-step">' +
            '<p class="wizard-hint">' +
            (window.I18N ? I18N.t('plan.stepHint') : 'Cluster composition') +
            '</p>' +
            '<div id="setupPlanPanel"></div>' +
            '</div>';
    }

    function renderGeneralSettingsStep() {
        var sc = wizardState;
        // R58.3 (2026-09-03): Hardware preset dropdown — applies sensible defaults
        // for vram/gpu/cpu/ram max + показывает hint для CPPWORKER_* params.
        var presetOptions = '';
        if (window.HardwarePresets) {
            window.HardwarePresets.list().forEach(function (p) {
                presetOptions += '<option value="' + p.key + '">' + p.label + '</option>';
            });
        }
        return '<div class="wizard-step-content general-step">' +
            '<h3>' + (window.I18N ? I18N.t('wizard.step4') : 'General Settings') + '</h3>' +
            // R58.3: Hardware preset selector
            '<div class="form-group hardware-preset-group">' +
            '<label>' + t_label('wizard.hardware_preset', 'Hardware Preset', 'wizard.tooltip.hardware_preset', 'Apply tuned defaults for your GPU. For CPPWORKER params (n_ctx, gpu_layers, etc.) see the hint below.') + '</label>' +
            '<select id="wiz-hardwarePreset" class="form-control">' +
            '<option value="">— Custom (no preset) —</option>' +
            presetOptions +
            '</select></div>' +
            '<div id="wiz-hardwarePresetHint" class="hardware-preset-hint" style="display:none;"></div>' +
            '<div class="form-group">' +
            '<label>' + t_label('settings.balancing_mode', 'Balancing Algorithm', 'wizard.tooltip.balancing_algorithm', 'Algorithm for routing requests to backends.') + '</label>' +
            '<select id="wiz-balancingAlgorithm" class="form-control">' +
            '<option value="resource-aware"' + (sc.algorithm === 'resource-aware' ? ' selected' : '') + '>Resource-Aware</option>' +
            '<option value="least-connections"' + (sc.algorithm === 'least-connections' ? ' selected' : '') + '>Least Connections</option>' +
            '<option value="round-robin"' + (sc.algorithm === 'round-robin' ? ' selected' : '') + '>Round Robin</option>' +
            '<option value="weighted"' + (sc.algorithm === 'weighted' ? ' selected' : '') + '>Weighted</option>' +
            '<option value="model-affinity"' + (sc.algorithm === 'model-affinity' ? ' selected' : '') + '>Model Affinity</option>' +
            '</select></div>' +
            '<div class="form-row">' +
            '<div class="form-group"><label>' + t_label('wizard.gpu_max_label', 'GPU Max %', 'wizard.tooltip.gpu_max', 'Max GPU% before routing elsewhere.') + '</label>' +
            '<input type="number" id="wiz-gpuMaxUsage" class="form-control" value="' + sc.gpuMaxUsage + '" min="50" max="100"></div>' +
            '<div class="form-group"><label>' + t_label('wizard.vram_max_label', 'VRAM Max %', 'wizard.tooltip.vram_max', 'Max VRAM% before routing. Leave 5-10% margin for KV cache.') + '</label>' +
            '<input type="number" id="wiz-vramMaxUsage" class="form-control" value="' + sc.vramMaxUsage + '" min="50" max="100"></div>' +
            '</div>' +
            '<div class="form-row">' +
            '<div class="form-group"><label>' + t_label('wizard.cpu_max_label', 'CPU Max %', 'wizard.tooltip.cpu_max', 'Max CPU% before routing.') + '</label>' +
            '<input type="number" id="wiz-cpuMaxUsage" class="form-control" value="' + sc.cpuMaxUsage + '" min="50" max="100"></div>' +
            '<div class="form-group"><label>' + t_label('wizard.ram_max_label', 'RAM Max %', 'wizard.tooltip.ram_max', 'Max RAM% (cppworker uses RAM for mmap).') + '</label>' +
            '<input type="number" id="wiz-ramMaxUsage" class="form-control" value="' + sc.ramMaxUsage + '" min="50" max="100"></div>' +
            '</div>' +
            '<div class="form-group"><label>' + t_label('wizard.api_token_label', 'API Token', 'wizard.tooltip.api_token', 'Auth token (must match API_TOKEN env).') + '</label>' +
            '<input type="password" id="wiz-apiToken" class="form-control" placeholder="API Token"></div>' +
            '</div>';
    }

    function renderSummaryStep() {
        var html = '<div class="wizard-step-content summary-step">' +
            '<h3>' + (window.I18N ? I18N.t('wizard.step5') : 'Summary') + '</h3>' +
            '<div class="wizard-summary-card">' +
            '<div class="wizard-summary-row"><span class="wizard-summary-label">Backend Type:</span>' +
            '<span class="wizard-summary-value">' + (wizardState.backendType === 'llama_cpp' ? '🦒 llama.cpp' : '🦙 Ollama') + '</span></div>' +
            // R107 (чистовая уборка S5/S7): вместо строки «Mode» — то, чем мастер
            // теперь реально управляет. Режим работы не выбирается: раскладку
            // задаёт политика размещения, записанная на шаге «состав кластера».
            '<div class="wizard-summary-row"><span class="wizard-summary-label">' +
            tr('wizard.summary_placement', 'Placement policy') + ':</span>' +
            '<span class="wizard-summary-value">' +
            escapeHtml(tr('wizard.summary_placement_value', 'written on the cluster composition step (balancing.placement)')) +
            '</span></div>';

        html += '<div class="wizard-summary-divider"></div>';
        html += '<div class="wizard-summary-row"><span class="wizard-summary-label">Algorithm:</span><span class="wizard-summary-value">' + wizardState.algorithm + '</span></div>';
        html += '<div class="wizard-summary-row"><span class="wizard-summary-label">GPU Max:</span><span class="wizard-summary-value">' + wizardState.gpuMaxUsage + '%</span></div>';
        html += '<div class="wizard-summary-row"><span class="wizard-summary-label">VRAM Max:</span><span class="wizard-summary-value">' + wizardState.vramMaxUsage + '%</span></div>';
        html += '</div></div>';
        return html;
    }

    function getFieldValue(id, fallback) {
        var el = wizardField(id);
        return el ? (el.value || fallback) : fallback;
    }

    function setFieldValue(id, value) {
        var el = wizardField(id);
        if (el && value !== undefined && value !== null) {
            el.value = value;
        }
    }

    // ================================================================
    // FIELD SCOPING (R66b, 2026-09-22)
    // ================================================================
    //
    // id полей мастера получают префикс `wiz-` (id="wiz-gpuMaxUsage" и т.п.),
    // потому что раньше они ПОЛНОСТЬЮ совпадали с id страницы Settings
    // (gpuMaxUsage, vramMaxUsage, modelReplicationMinInstances, modeFields-*,
    // balancingAlgorithm, apiToken, …). Из-за совпадения:
    //   * document.getElementById() возвращал элемент СТРАНИЦЫ SETTINGS
    //     (она в DOM раньше) — шаг 4 мастера выглядел пустым, а правки шага 5
    //     молча терялись (setFieldValue писал в невидимый input страницы);
    //   * в DOM жили два элемента с одинаковым id — невалидный HTML, и любой
    //     сторонний код с document.getElementById() попадал не туда.
    // Теперь id уникальны, а wizardField() сначала ищет префиксованный id внутри
    // контейнера мастера, затем обычный (для wizard-only элементов вроде
    // wizardBody) и лишь потом — по документу.
    function wizardRoot() {
        return document.getElementById('setupWizardModal') || document;
    }

    function wizardField(id) {
        var root = wizardRoot();
        if (root && typeof root.querySelector === 'function') {
            var scoped = root.querySelector('[id="wiz-' + id + '"]') || root.querySelector('[id="' + id + '"]');
            if (scoped) return scoped;
        }
        return document.getElementById(id);
    }

    // ================================================================
    // EVENTS
    // ================================================================

    function bindWizardEvents(modal) {
        modal.addEventListener('click', function (e) {
            var target = e.target;

            if (target.id === 'wizardPrevBtn' || target.closest('#wizardPrevBtn')) {
                // R107: шаг «параметры режима» удалён — читать нечего, кроме общих полей.
                if (currentStep === 4) readGeneralFieldsIntoState();
                if (currentStep > 1) { currentStep--; renderStep(currentStep); }
                return;
            }

            if (target.id === 'wizardNextBtn' || target.closest('#wizardNextBtn')) {
                if (currentStep === 4) readGeneralFieldsIntoState();
                if (currentStep < totalSteps) { currentStep++; renderStep(currentStep); }
                return;
            }

            if (target.id === 'wizardFinishBtn' || target.closest('#wizardFinishBtn')) {
                finishWizard();
                return;
            }

            if (target.id === 'wizardImportBtn' || target.id === 'wizardImportBtn2' || target.closest('#wizardImportBtn') || target.closest('#wizardImportBtn2')) {
                handleWizardImport();
                return;
            }

            var dot = target.closest('.wizard-step-dot');
            if (dot) {
                var step = parseInt(dot.dataset.step);
                if (step < currentStep) { currentStep = step; renderStep(currentStep); }
            }

            // Backend type card selection (step 2) — ТОЛЬКО в wizardState, не трогаем BackendTypeFilter
            var btCard = target.closest('.mode-card[data-backend-type]');
            if (btCard) {
                var btRadio = btCard.querySelector('input[name="backendType"]');
                if (btRadio) {
                    btRadio.checked = true;
                    // R107: сброс режима при смене типа бэкенда убран — режим в
                    // мастере больше не выбирается (раскладка = политика).
                    wizardState.backendType = btRadio.value;
                    document.querySelectorAll('.mode-card[data-backend-type]').forEach(function (c) {
                        c.classList.toggle('active', c === btCard);
                    });
                }
                return;
            }
        });

        // R58.3 (2026-09-03): Hardware preset dropdown change handler.
        // When user selects a preset, fill in the balancer tuning fields
        // (vramMaxUsage, gpuMaxUsage, etc.) and show cppworker hint.
        var presetDropdown = modal.querySelector('#wiz-hardwarePreset');
        if (presetDropdown) {
            presetDropdown.addEventListener('change', function () {
                applyHardwarePreset(presetDropdown.value);
            });
        }
    }

    // applyHardwarePreset — R58.3: fill in balancer tuning fields from a preset.
    // Empty value (or unknown preset) clears the hint and leaves fields untouched.
    function applyHardwarePreset(presetKey) {
        var hintEl = wizardField('hardwarePresetHint');
        if (!presetKey || !window.HardwarePresets) {
            if (hintEl) hintEl.style.display = 'none';
            return;
        }
        var p = window.HardwarePresets.get(presetKey);
        if (!p) {
            if (hintEl) hintEl.style.display = 'none';
            return;
        }
        // Apply balancer tuning values
        if (typeof p.vramMaxUsage === 'number') setFieldValue('vramMaxUsage', p.vramMaxUsage);
        if (typeof p.gpuMaxUsage === 'number') setFieldValue('gpuMaxUsage', p.gpuMaxUsage);
        if (typeof p.cpuMaxUsage === 'number') setFieldValue('cpuMaxUsage', p.cpuMaxUsage);
        if (typeof p.ramMaxUsage === 'number') setFieldValue('ramMaxUsage', p.ramMaxUsage);
        // Show hint
        if (hintEl && p.cppworkerHint) {
            var rows = Object.keys(p.cppworkerHint).map(function (k) {
                return '<code>' + k + '=' + p.cppworkerHint[k] + '</code>';
            }).join('<br>');
            hintEl.innerHTML =
                '<div class="preset-hint-box">' +
                '<div class="preset-hint-title"><i class="fas fa-microchip"></i> ' +
                (window.I18N ? I18N.t('wizard.cppworker_hint_title') : 'Apply to cppworker env (separate step):') + '</div>' +
                rows +
                '<div class="preset-hint-footer">' +
                (window.I18N ? I18N.t('wizard.cppworker_hint_footer', { preset: presetKey }) :
                    'Run: <code>python scripts/apply-hardware-preset.py ' + presetKey + '</code>') +
                '</div>' +
                '<div class="preset-hint-models">' +
                (window.I18N ? I18N.t('wizard.recommended_models') : 'Recommended models: ') +
                '<em>' + p.recommendedModels + '</em>' +
                '</div>' +
                '</div>';
            hintEl.style.display = 'block';
        }
    }


    function showWizardError(msg) {
        var existing = document.getElementById('wizardError');
        if (!existing) {
            var body = document.getElementById('wizardBody');
            var err = document.createElement('div');
            err.id = 'wizardError';
            err.className = 'wizard-error';
            err.style.cssText = 'background:var(--danger);color:#fff;padding:12px;border-radius:8px;margin-bottom:16px;';
            body.insertBefore(err, body.firstChild);
        } else {
            existing.style.display = 'block';
        }
        document.getElementById('wizardError').innerHTML = msg;
        setTimeout(function () {
            var e = document.getElementById('wizardError');
            if (e) e.style.display = 'none';
        }, 5000);
    }

    // ================================================================
    // IMPORT
    // ================================================================

    function handleWizardImport() {
        if (!window.ConfigIO) return;
        window.ConfigIO.importConfigFromFile().then(function (data) {
            return window.ConfigIO.showImportPreview(data).then(function (confirmed) {
                if (confirmed) {
                    applyWizardConfig(data);
                    // R107: шагов стало 5 (шаг «параметры режима» удалён).
                    currentStep = 5;
                    renderStep(currentStep);
                }
            });
        }).catch(function (err) {
            showWizardError(err.message || 'Import failed');
        });
    }

    function applyWizardConfig(data) {
        if (!data) return;
        // R107 (чистовая уборка S5/S7): режим и его параметры из импортируемого
        // конфига больше не читаем — мастер их не редактирует и не записывает.
        // Раскладка задаётся политикой размещения (balancing.placement).
        if (data.settings) {
            var s = data.settings;
            if (s.algorithm) wizardState.algorithm = s.algorithm;
            if (s.gpuMaxUsage) wizardState.gpuMaxUsage = s.gpuMaxUsage;
            if (s.vramMaxUsage) wizardState.vramMaxUsage = s.vramMaxUsage;
            if (s.cpuMaxUsage) wizardState.cpuMaxUsage = s.cpuMaxUsage;
            if (s.ramMaxUsage) wizardState.ramMaxUsage = s.ramMaxUsage;
        }
    }

    // ================================================================
    // FINISH — ПРИМЕНИТЬ ВСЁ ОДНИМ PUT
    // ================================================================

    function finishWizard() {
        // Считываем параметры с текущих форм если нужно
        // R107: остался только шаг общих параметров (был шаг 5 → стал 4).
        if (currentStep === 4) readGeneralFieldsIntoState();

        var payload = buildWizardPayload();
        payload.initialized = true;

        // ВСЕГДА сохраняем тип бэкенда в localStorage ДО API-запроса.
        // Даже если API недоступен — тип не потеряется после перезагрузки.
        localStorage.setItem('ollamalegion_backend_type', wizardState.backendType);
        localStorage.setItem('ollamalegion_wizard_done', '1');
        console.log('[SetupWizard] finishWizard. backendType:', wizardState.backendType, 'payload:', JSON.stringify(payload));

        // Устанавливаем флаг защиты от гонки: пока идёт сохранение на сервер,
        // syncFromClusterState не должен перезаписывать localStorage серверными данными.
        if (window.BackendTypeFilter) {
            BackendTypeFilter._savingInProgress = true;
            BackendTypeFilter._lastSavedType = wizardState.backendType;
        }

        if (window.Api && window.Api.updateConfig) {
            window.Api.updateConfig(payload).then(function () {
                // Снимаем флаг после успешного сохранения
                if (window.BackendTypeFilter) {
                    BackendTypeFilter._savingInProgress = false;
                }
                console.log('[SetupWizard] Server saved OK. backendType:', wizardState.backendType);
                return window.Api.config();
            }).then(function (cfg) {
                console.log('[SetupWizard] Server config after save:', JSON.stringify(cfg));
                if (cfg && cfg.initialized === true) {
                    closeWizard();
                    // R66b: мёртвая проверка `typeof startNormalInit` убрана —
                    // функции с таким именем в проекте нет, всегда выполнялся
                    // location.reload(). Перезагружаемся, чтобы приложение
                    // стартовало с серверным (только что сохранённым) конфигом.
                    location.reload();
                } else {
                    showWizardError('\u0421\u0435\u0440\u0432\u0435\u0440 \u043d\u0435 \u043f\u043e\u0434\u0442\u0432\u0435\u0440\u0434\u0438\u043b \u0438\u043d\u0438\u0446\u0438\u0430\u043b\u0438\u0437\u0430\u0446\u0438\u044e. \u041f\u043e\u043f\u0440\u043e\u0431\u0443\u0439\u0442\u0435 \u0435\u0449\u0451 \u0440\u0430\u0437.');
                }
            }).catch(function (err) {
                // Снимаем флаг при ошибке сохранения
                if (window.BackendTypeFilter) {
                    BackendTypeFilter._savingInProgress = false;
                }
                console.error('[SetupWizard] API error:', err.message || err);
                showWizardError((err.message || err) || 'Failed to save configuration');
            });
        } else {
            console.log('[SetupWizard] API not available — skipping server save.');
            closeWizard();
            // R66b: здесь была проверка `typeof startNormalInit === 'function'`,
            // но такой функции в проекте НЕТ ни в одном файле — условие всегда
            // ложно, и всегда выполнялся location.reload(). Оставляем явное
            // поведение и убираем мёртвую ветку.
            location.reload();
        }
    }

    function buildWizardPayload() {
        // R107 (чистовая уборка S5/S7): `operatingMode` и секции режимов
        // (modelReplication / rpcCoordinator / virtualModels / distInference) из
        // payload УБРАНЫ. Причина: мастер их больше не редактирует, а PUT
        // /api/v1/cluster/config применяет `operatingMode` только если поле
        // непустое (handlers_cluster.go) — то есть текущее значение на стенде
        // сохраняется как есть. Раскладку мастер пишет отдельно, политикой
        // (PUT /api/v1/placement, шаг «состав кластера»).
        return {
            // R112 (2026-10-11): 'auto' — тип не навязан (llama.cpp и Ollama
            // работают вместе); раньше любое значение, кроме llama_cpp, молча
            // превращалось в ollama_api и вырезало llama.cpp-бэкенды.
            backendEngine: wizardState.backendType === 'llama_cpp'
                ? 'llama_cpp'
                : (wizardState.backendType === 'auto' || wizardState.backendType === 'all' ? 'auto' : 'ollama_api'),
            algorithm: wizardState.algorithm,
            gpuMaxUsage: wizardState.gpuMaxUsage,
            vramMaxUsage: wizardState.vramMaxUsage,
            cpuMaxUsage: wizardState.cpuMaxUsage,
            ramMaxUsage: wizardState.ramMaxUsage
        };
    }

    // ================================================================
    // PUBLIC API
    // ================================================================

    window.SetupWizard = {
        isInitialized: isInitialized,
        start: start,
        reset: reset,
        markInitialized: markInitialized
    };

})();

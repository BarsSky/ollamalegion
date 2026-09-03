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
    var totalSteps = 6;
    var currentServerConfig = null;

    // === ИЗОЛИРОВАННОЕ СОСТОЯНИЕ WIZARD ===
    // Накапливает выборы пользователя. Применяется только при finishWizard().
    var wizardState = {
        backendType: 'ollama',        // 'ollama' | 'llama_cpp'
        operatingMode: 'standard',    // режим работы балансера
        // настройки общего характера (шаг 5)
        algorithm: 'resource-aware',
        gpuMaxUsage: 90,
        vramMaxUsage: 85,
        cpuMaxUsage: 80,
        ramMaxUsage: 85,
        // mode-specific params (шаг 4)
        replication: null,
        rpcCoordinator: null,
        virtualModels: null,
        distInference: null
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
            backendType: initialType,
            operatingMode: 'standard',
            algorithm: 'resource-aware',
            gpuMaxUsage: 90,
            vramMaxUsage: 85,
            cpuMaxUsage: 80,
            ramMaxUsage: 85,
            replication: null,
            rpcCoordinator: null,
            virtualModels: null,
            distInference: null
        };

        currentStep = 1;
        var existing = document.getElementById('setupWizardModal');
        if (existing) existing.remove();

        fetchServerConfig().then(function () {
            // Подгружаем дефолты из серверного конфига если есть.
            // ВАЖНО: backendType НЕ перезаписываем из сервера —
            // доверяем localStorage (initialType), установленному выше.
            if (currentServerConfig) {
                if (currentServerConfig.operatingMode) {
                    wizardState.operatingMode = currentServerConfig.operatingMode;
                }
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
    function t_label(labelKey, labelFallback, descKey, descFallback) {
        var lbl = (window.I18N && window.I18N.t(labelKey, null)) || labelFallback;
        var desc = (window.I18N && window.I18N.t(descKey, null)) || descFallback;
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
            t('wizard.step_backend_type') || 'Backend Type',
            t('wizard.step2'),
            t('wizard.step3'),
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
            case 2: html = renderBackendTypeStep(); break;
            case 3: html = renderModeSelectionStep(); break;
            case 4: html = renderModeParamsStep(); break;
            case 5: html = renderGeneralSettingsStep(); break;
            case 6: html = renderSummaryStep(); break;
        }
        body.innerHTML = html;

        if (step === 3 && window.SettingsUI) {
            window.SettingsUI.setupModeSelector();
        }
        if (step === 4) {
            if (window.SettingsUI) {
                window.SettingsUI.showModeFields(wizardState.operatingMode);
            }
            applyWizardStateToModeFields();
        }
        if (step === 5) {
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

    function applyWizardStateToModeFields() {
        var mode = wizardState.operatingMode;
        if (mode === 'replication' && wizardState.replication) {
            setFieldValue('modelReplicationMinInstances', wizardState.replication.defaultMinInstances);
            setFieldValue('modelReplicationMaxInstances', wizardState.replication.defaultMaxInstances);
            setFieldValue('modelReplicationIdleUnload', wizardState.replication.idleUnloadAfter);
        }
        if (mode === 'rpc_coordinator' && wizardState.rpcCoordinator) {
            setFieldValue('rpcCoordinatorURL', wizardState.rpcCoordinator.coordinatorURL);
            setFieldValue('rpcCoordinatorWorkerPort', wizardState.rpcCoordinator.workerPort);
            setFieldValue('rpcCoordinatorProtocol', wizardState.rpcCoordinator.protocol);
            setFieldValue('rpcCoordinatorTimeout', wizardState.rpcCoordinator.timeout);
            setFieldValue('rpcCoordinatorMaxRetries', wizardState.rpcCoordinator.maxRetries);
        }
        if (mode === 'virtual_router' && wizardState.virtualModels) {
            setFieldValue('virtualModelsCoordMode', wizardState.virtualModels.coordMode);
            setFieldValue('virtualModelsTimeout', wizardState.virtualModels.timeout);
        }
        if (mode === 'distributed_inference' && wizardState.distInference) {
            setFieldValue('distInferenceGrpcPort', wizardState.distInference.grpcPort);
        }
    }

    function applyWizardStateToGeneralFields() {
        setFieldValue('balancingAlgorithm', wizardState.algorithm);
        setFieldValue('gpuMaxUsage', wizardState.gpuMaxUsage);
        setFieldValue('vramMaxUsage', wizardState.vramMaxUsage);
        setFieldValue('cpuMaxUsage', wizardState.cpuMaxUsage);
        setFieldValue('ramMaxUsage', wizardState.ramMaxUsage);
    }

    function readModeParamsIntoState() {
        var mode = wizardState.operatingMode;
        if (mode === 'replication') {
            wizardState.replication = {
                enabled: true,
                defaultMinInstances: parseInt(getFieldValue('modelReplicationMinInstances', 1)),
                defaultMaxInstances: parseInt(getFieldValue('modelReplicationMaxInstances', 3)),
                idleUnloadAfter: getFieldValue('modelReplicationIdleUnload', '10m')
            };
        } else if (mode === 'rpc_coordinator') {
            wizardState.rpcCoordinator = {
                enabled: true,
                coordinatorURL: getFieldValue('rpcCoordinatorURL', ''),
                workerPort: parseInt(getFieldValue('rpcCoordinatorWorkerPort', 18050)),
                protocol: getFieldValue('rpcCoordinatorProtocol', 'http'),
                timeout: getFieldValue('rpcCoordinatorTimeout', '30s'),
                maxRetries: parseInt(getFieldValue('rpcCoordinatorMaxRetries', 3))
            };
        } else if (mode === 'virtual_router') {
            wizardState.virtualModels = {
                enabled: true,
                coordMode: getFieldValue('virtualModelsCoordMode', 'sequential'),
                timeout: parseInt(getFieldValue('virtualModelsTimeout', 30000))
            };
        } else if (mode === 'distributed_inference') {
            wizardState.distInference = {
                enabled: true,
                grpcPort: parseInt(getFieldValue('distInferenceGrpcPort', 19000))
            };
        }
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
            '📂 ' + (window.I18N ? I18N.t('wizard.import') : 'Import Configuration') + '</button>' +
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

    function getAvailableModesForType(type) {
        if (type === 'llama_cpp') return ['standard', 'virtual_router', 'distributed_inference'];
        return ['standard', 'replication', 'rpc_coordinator'];
    }

    function renderModeSelectionStep() {
        var availableModes = getAvailableModesForType(wizardState.backendType);
        // Если текущий выбранный режим недоступен для типа — сбрасываем на standard
        if (availableModes.indexOf(wizardState.operatingMode) < 0) {
            wizardState.operatingMode = 'standard';
        }

        var allModeCards = {
            standard: { icon: '⚙️', title: window.I18N ? I18N.t('settings.mode.standard') : 'Standard Balancer', desc: window.I18N ? I18N.t('settings.mode.standard_desc') : 'Basic load balancing' },
            replication: { icon: '📋', title: window.I18N ? I18N.t('settings.mode.replication') : 'Model Replication (A)', desc: window.I18N ? I18N.t('settings.mode.replication_desc') : 'Replicate models across backends' },
            rpc_coordinator: { icon: '🌐', title: window.I18N ? I18N.t('settings.mode.rpc_coordinator') : 'RPC Coordinator (B)', desc: window.I18N ? I18N.t('settings.mode.rpc_coordinator_desc') : 'External RPC coordinator' },
            virtual_router: { icon: '🧩', title: window.I18N ? I18N.t('settings.mode.virtual_router') : 'Virtual Model Router (C)', desc: window.I18N ? I18N.t('settings.mode.virtual_router_desc') : 'Pipeline parallelism via slices' },
            distributed_inference: { icon: '🔬', title: window.I18N ? I18N.t('settings.mode.distributed') : 'Distributed Inference (D)', desc: window.I18N ? I18N.t('settings.mode.distributed_desc') : 'Custom gRPC distributed inference' }
        };

        var cardsHtml = '';
        availableModes.forEach(function (mode) {
            var mc = allModeCards[mode];
            if (mc) {
                cardsHtml += '<label class="mode-card' + (wizardState.operatingMode === mode ? ' active' : '') + '" data-mode="' + mode + '">' +
                    '<input type="radio" name="operatingMode" value="' + mode + '" ' + (wizardState.operatingMode === mode ? 'checked' : '') + '>' +
                    '<div class="mode-card-icon">' + mc.icon + '</div>' +
                    '<div class="mode-card-title">' + mc.title + '</div>' +
                    '<div class="mode-card-desc">' + mc.desc + '</div>' +
                    '</label>';
            }
        });

        if (!cardsHtml) {
            cardsHtml = '<p style="color:var(--text-muted);">' +
                (window.I18N ? I18N.t('wizard.summary_empty') : 'No modes available for this backend type') + '</p>';
        }

        var t = window.I18N ? I18N.t.bind(I18N) : function(k) { return k; };
        return '<div class="wizard-step-content mode-step">' +
            '<h3>' + (t('wizard.step2') || 'Select Operating Mode') + '</h3>' +
            '<p style="color:var(--text-muted);font-size:13px;margin-bottom:16px;">' +
                (t('wizard.mode_selection_desc') || 'Choose the operating mode.') + '</p>' +
            '<div class="mode-selector"><div class="mode-cards">' + cardsHtml + '</div></div>' +
            '</div>';
    }

    function renderModeParamsStep() {
        var mode = wizardState.operatingMode;
        var sc = wizardState;

        return '<div class="wizard-step-content params-step">' +
            '<h3>' + (window.I18N ? I18N.t('wizard.step3') : 'Mode Parameters') + '</h3>' +
            '<div id="modeFields-standard" class="mode-fields">' +
            '<p style="color:var(--text-muted);">' + (window.I18N ? I18N.t('wizard.summary_empty') : 'Standard mode — no additional parameters required') + '</p>' +
            '</div>' +
            '<div id="modeFields-replication" class="mode-fields" style="display:none;">' +
            '<div class="form-row">' +
            '<div class="form-group"><label>' + t_label('settings.model_replication_min', 'Min Instances', 'wizard.tooltip.replication_min', 'Min number of model instances across backends. 0 = replication disabled.') + '</label>' +
            '<input type="number" id="modelReplicationMinInstances" class="form-control" value="' + ((sc.replication && sc.replication.defaultMinInstances) || 1) + '" min="0" max="10"></div>' +
            '<div class="form-group"><label>' + t_label('settings.model_replication_max', 'Max Instances', 'wizard.tooltip.replication_max', 'Max number of instances. New replicas spin up to this limit.') + '</label>' +
            '<input type="number" id="modelReplicationMaxInstances" class="form-control" value="' + ((sc.replication && sc.replication.defaultMaxInstances) || 3) + '" min="0" max="20"></div>' +
            '</div>' +
            '<div class="form-group"><label>' + t_label('settings.model_replication_idle_unload', 'Idle Unload After', 'wizard.tooltip.replication_idle_unload', 'How long to keep an idle replica before unloading. Format: 10m, 30m, 1h.') + '</label>' +
            '<input type="text" id="modelReplicationIdleUnload" class="form-control" value="' + ((sc.replication && sc.replication.idleUnloadAfter) || '10m') + '" placeholder="10m, 30m, 1h"></div>' +
            '</div>' +
            '<div id="modeFields-rpc_coordinator" class="mode-fields" style="display:none;">' +
            '<div class="form-group"><label>' + t_label('settings.rpc_coordinator_url', 'Coordinator URL', 'wizard.tooltip.rpc_url', 'URL of external RPC coordinator (e.g. http://coordinator:8080).') + '</label>' +
            '<input type="text" id="rpcCoordinatorURL" class="form-control" value="' + ((sc.rpcCoordinator && sc.rpcCoordinator.coordinatorURL) || '') + '" placeholder="http://coordinator:8080"></div>' +
            '<div class="form-row">' +
            '<div class="form-group"><label>' + t_label('settings.rpc_coordinator_port', 'Worker Port', 'wizard.tooltip.rpc_worker_port', 'Port where workers listen for commands. Must match across all workers.') + '</label>' +
            '<input type="number" id="rpcCoordinatorWorkerPort" class="form-control" value="' + ((sc.rpcCoordinator && sc.rpcCoordinator.workerPort) || 18050) + '"></div>' +
            '<div class="form-group"><label>' + t_label('settings.rpc_coordinator_protocol', 'Protocol', 'wizard.tooltip.rpc_protocol', 'HTTP — simple, gRPC — faster for streaming.') + '</label>' +
            '<select id="rpcCoordinatorProtocol" class="form-control"><option value="http">HTTP</option><option value="grpc">gRPC</option></select></div>' +
            '</div>' +
            '<div class="form-row">' +
            '<div class="form-group"><label>' + t_label('settings.rpc_coordinator_timeout', 'Timeout', 'wizard.tooltip.rpc_timeout', 'Timeout for worker response. Format: 30s, 60s, 2m.') + '</label>' +
            '<input type="text" id="rpcCoordinatorTimeout" class="form-control" value="' + ((sc.rpcCoordinator && sc.rpcCoordinator.timeout) || '30s') + '" placeholder="30s, 60s"></div>' +
            '<div class="form-group"><label>' + t_label('settings.rpc_coordinator_retries', 'Max Retries', 'wizard.tooltip.rpc_max_retries', 'How many times to retry on worker failure.') + '</label>' +
            '<input type="number" id="rpcCoordinatorMaxRetries" class="form-control" value="' + ((sc.rpcCoordinator && sc.rpcCoordinator.maxRetries) || 3) + '" min="0" max="10"></div>' +
            '</div>' +
            '</div>' +
            '<div id="modeFields-virtual_router" class="mode-fields" style="display:none;">' +
            '<div class="form-group"><label>' + t_label('settings.virtual_models_coord_mode', 'Coordination Mode', 'wizard.tooltip.virtual_coord_mode', 'Sequential — chain. Parallel — concurrent. Tree — parent → children.') + '</label>' +
            '<select id="virtualModelsCoordMode" class="form-control">' +
            '<option value="sequential">Sequential</option>' +
            '<option value="parallel">Parallel</option>' +
            '<option value="tree">Tree</option>' +
            '</select></div>' +
            '<div class="form-group"><label>' + t_label('settings.virtual_models_timeout', 'Timeout (ms)', 'wizard.tooltip.virtual_timeout', 'Timeout for virtual model request (ms).') + '</label>' +
            '<input type="number" id="virtualModelsTimeout" class="form-control" value="' + ((sc.virtualModels && sc.virtualModels.timeout) || 30000) + '" min="1000"></div>' +
            '</div>' +
            '<div id="modeFields-distributed_inference" class="mode-fields" style="display:none;">' +
            '<div class="form-group"><label>' + t_label('settings.dist_inference_grpc_port', 'gRPC Port', 'wizard.tooltip.dist_grpc_port', 'gRPC server port for custom distributed inference.') + '</label>' +
            '<input type="number" id="distInferenceGrpcPort" class="form-control" value="' + ((sc.distInference && sc.distInference.grpcPort) || 19000) + '" min="1024" max="65535"></div>' +
            '</div>' +
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
            '<select id="hardwarePreset" class="form-control">' +
            '<option value="">— Custom (no preset) —</option>' +
            presetOptions +
            '</select></div>' +
            '<div id="hardwarePresetHint" class="hardware-preset-hint" style="display:none;"></div>' +
            '<div class="form-group">' +
            '<label>' + t_label('settings.balancing_mode', 'Balancing Algorithm', 'wizard.tooltip.balancing_algorithm', 'Algorithm for routing requests to backends.') + '</label>' +
            '<select id="balancingAlgorithm" class="form-control">' +
            '<option value="resource-aware"' + (sc.algorithm === 'resource-aware' ? ' selected' : '') + '>Resource-Aware</option>' +
            '<option value="least-connections"' + (sc.algorithm === 'least-connections' ? ' selected' : '') + '>Least Connections</option>' +
            '<option value="round-robin"' + (sc.algorithm === 'round-robin' ? ' selected' : '') + '>Round Robin</option>' +
            '<option value="weighted"' + (sc.algorithm === 'weighted' ? ' selected' : '') + '>Weighted</option>' +
            '<option value="model-affinity"' + (sc.algorithm === 'model-affinity' ? ' selected' : '') + '>Model Affinity</option>' +
            '</select></div>' +
            '<div class="form-row">' +
            '<div class="form-group"><label>' + t_label('wizard.gpu_max_label', 'GPU Max %', 'wizard.tooltip.gpu_max', 'Max GPU% before routing elsewhere.') + '</label>' +
            '<input type="number" id="gpuMaxUsage" class="form-control" value="' + sc.gpuMaxUsage + '" min="50" max="100"></div>' +
            '<div class="form-group"><label>' + t_label('wizard.vram_max_label', 'VRAM Max %', 'wizard.tooltip.vram_max', 'Max VRAM% before routing. Leave 5-10% margin for KV cache.') + '</label>' +
            '<input type="number" id="vramMaxUsage" class="form-control" value="' + sc.vramMaxUsage + '" min="50" max="100"></div>' +
            '</div>' +
            '<div class="form-row">' +
            '<div class="form-group"><label>' + t_label('wizard.cpu_max_label', 'CPU Max %', 'wizard.tooltip.cpu_max', 'Max CPU% before routing.') + '</label>' +
            '<input type="number" id="cpuMaxUsage" class="form-control" value="' + sc.cpuMaxUsage + '" min="50" max="100"></div>' +
            '<div class="form-group"><label>' + t_label('wizard.ram_max_label', 'RAM Max %', 'wizard.tooltip.ram_max', 'Max RAM% (cppworker uses RAM for mmap).') + '</label>' +
            '<input type="number" id="ramMaxUsage" class="form-control" value="' + sc.ramMaxUsage + '" min="50" max="100"></div>' +
            '</div>' +
            '<div class="form-group"><label>' + t_label('wizard.api_token_label', 'API Token', 'wizard.tooltip.api_token', 'Auth token (must match API_TOKEN env).') + '</label>' +
            '<input type="password" id="apiToken" class="form-control" placeholder="API Token"></div>' +
            '</div>';
    }

    function renderSummaryStep() {
        var modeIcons = { standard: '⚙️', replication: '📋', rpc_coordinator: '🌐', virtual_router: '🧩', distributed_inference: '🔬' };
        var modeNames = { standard: 'Standard', replication: 'Model Replication', rpc_coordinator: 'RPC Coordinator', virtual_router: 'Virtual Model Router', distributed_inference: 'Distributed Inference' };

        var html = '<div class="wizard-step-content summary-step">' +
            '<h3>' + (window.I18N ? I18N.t('wizard.step5') : 'Summary') + '</h3>' +
            '<div class="wizard-summary-card">' +
            '<div class="wizard-summary-row"><span class="wizard-summary-label">Backend Type:</span>' +
            '<span class="wizard-summary-value">' + (wizardState.backendType === 'llama_cpp' ? '🦒 llama.cpp' : '🦙 Ollama') + '</span></div>' +
            '<div class="wizard-summary-row"><span class="wizard-summary-label">Mode:</span>' +
            '<span class="wizard-summary-value">' + (modeIcons[wizardState.operatingMode] || '') + ' ' + (modeNames[wizardState.operatingMode] || wizardState.operatingMode) + '</span></div>';

        var mode = wizardState.operatingMode;
        if (mode === 'replication' && wizardState.replication) {
            html += '<div class="wizard-summary-row"><span class="wizard-summary-label">Min Instances:</span><span class="wizard-summary-value">' + wizardState.replication.defaultMinInstances + '</span></div>';
            html += '<div class="wizard-summary-row"><span class="wizard-summary-label">Max Instances:</span><span class="wizard-summary-value">' + wizardState.replication.defaultMaxInstances + '</span></div>';
        } else if (mode === 'rpc_coordinator' && wizardState.rpcCoordinator) {
            html += '<div class="wizard-summary-row"><span class="wizard-summary-label">URL:</span><span class="wizard-summary-value">' + wizardState.rpcCoordinator.coordinatorURL + '</span></div>';
        } else if (mode === 'virtual_router' && wizardState.virtualModels) {
            html += '<div class="wizard-summary-row"><span class="wizard-summary-label">Coord Mode:</span><span class="wizard-summary-value">' + wizardState.virtualModels.coordMode + '</span></div>';
        } else if (mode === 'distributed_inference' && wizardState.distInference) {
            html += '<div class="wizard-summary-row"><span class="wizard-summary-label">gRPC Port:</span><span class="wizard-summary-value">' + wizardState.distInference.grpcPort + '</span></div>';
        }

        html += '<div class="wizard-summary-divider"></div>';
        html += '<div class="wizard-summary-row"><span class="wizard-summary-label">Algorithm:</span><span class="wizard-summary-value">' + wizardState.algorithm + '</span></div>';
        html += '<div class="wizard-summary-row"><span class="wizard-summary-label">GPU Max:</span><span class="wizard-summary-value">' + wizardState.gpuMaxUsage + '%</span></div>';
        html += '<div class="wizard-summary-row"><span class="wizard-summary-label">VRAM Max:</span><span class="wizard-summary-value">' + wizardState.vramMaxUsage + '%</span></div>';
        html += '</div></div>';
        return html;
    }

    function getFieldValue(id, fallback) {
        var el = document.getElementById(id);
        return el ? (el.value || fallback) : fallback;
    }

    function setFieldValue(id, value) {
        var el = document.getElementById(id);
        if (el && value !== undefined && value !== null) {
            el.value = value;
        }
    }

    // ================================================================
    // EVENTS
    // ================================================================

    function bindWizardEvents(modal) {
        modal.addEventListener('click', function (e) {
            var target = e.target;

            if (target.id === 'wizardPrevBtn' || target.closest('#wizardPrevBtn')) {
                if (currentStep === 4) readModeParamsIntoState();
                if (currentStep === 5) readGeneralFieldsIntoState();
                if (currentStep > 1) { currentStep--; renderStep(currentStep); }
                return;
            }

            if (target.id === 'wizardNextBtn' || target.closest('#wizardNextBtn')) {
                if (currentStep === 4) {
                    var errors = validateWizardModeParams();
                    if (errors.length > 0) { showWizardError(errors.join('<br>')); return; }
                    readModeParamsIntoState();
                }
                if (currentStep === 5) readGeneralFieldsIntoState();
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
                    var newType = btRadio.value;
                    if (newType !== wizardState.backendType) {
                        wizardState.backendType = newType;
                        // Если текущий режим недоступен для нового типа — сбрасываем
                        var availableModes = getAvailableModesForType(newType);
                        if (availableModes.indexOf(wizardState.operatingMode) < 0) {
                            wizardState.operatingMode = 'standard';
                        }
                    }
                    document.querySelectorAll('.mode-card[data-backend-type]').forEach(function (c) {
                        c.classList.toggle('active', c === btCard);
                    });
                }
                return;
            }

            // Mode card selection — ТОЛЬКО в wizardState
            var card = target.closest('.mode-card[data-mode]');
            if (card) {
                var radio = card.querySelector('input[type="radio"]');
                if (radio) {
                    radio.checked = true;
                    wizardState.operatingMode = card.getAttribute('data-mode');
                    document.querySelectorAll('.mode-card').forEach(function (c) {
                        c.classList.toggle('active', c === card);
                    });
                }
            }
        });

        // R58.3 (2026-09-03): Hardware preset dropdown change handler.
        // When user selects a preset, fill in the balancer tuning fields
        // (vramMaxUsage, gpuMaxUsage, etc.) and show cppworker hint.
        var presetDropdown = modal.querySelector('#hardwarePreset');
        if (presetDropdown) {
            presetDropdown.addEventListener('change', function () {
                applyHardwarePreset(presetDropdown.value);
            });
        }
    }

    // applyHardwarePreset — R58.3: fill in balancer tuning fields from a preset.
    // Empty value (or unknown preset) clears the hint and leaves fields untouched.
    function applyHardwarePreset(presetKey) {
        var hintEl = document.getElementById('hardwarePresetHint');
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
                (window.I18N ? I18N.t('wizard.cppworker_hint_footer') :
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

    function validateWizardModeParams() {
        var errors = [];
        var mode = wizardState.operatingMode;
        if (mode === 'rpc_coordinator') {
            var url = getFieldValue('rpcCoordinatorURL', '');
            if (!url) errors.push('Coordinator URL is required');
        }
        if (mode === 'replication') {
            var min = parseInt(getFieldValue('modelReplicationMinInstances', 0));
            var max = parseInt(getFieldValue('modelReplicationMaxInstances', 0));
            if (max < min) errors.push('Max must be >= Min');
        }
        return errors;
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
                    currentStep = 6;
                    renderStep(currentStep);
                }
            });
        }).catch(function (err) {
            showWizardError(err.message || 'Import failed');
        });
    }

    function applyWizardConfig(data) {
        if (!data) return;
        if (data.operating_mode) wizardState.operatingMode = data.operating_mode;
        if (data.settings) {
            var s = data.settings;
            if (s.algorithm) wizardState.algorithm = s.algorithm;
            if (s.gpuMaxUsage) wizardState.gpuMaxUsage = s.gpuMaxUsage;
            if (s.vramMaxUsage) wizardState.vramMaxUsage = s.vramMaxUsage;
            if (s.cpuMaxUsage) wizardState.cpuMaxUsage = s.cpuMaxUsage;
            if (s.ramMaxUsage) wizardState.ramMaxUsage = s.ramMaxUsage;
        }
        if (data.mode_config) {
            var mc = data.mode_config;
            if (mc.modelReplication) wizardState.replication = mc.modelReplication;
            if (mc.rpcCoordinator) wizardState.rpcCoordinator = mc.rpcCoordinator;
            if (mc.virtualModels) wizardState.virtualModels = mc.virtualModels;
            if (mc.distInference) wizardState.distInference = mc.distInference;
        }
    }

    // ================================================================
    // FINISH — ПРИМЕНИТЬ ВСЁ ОДНИМ PUT
    // ================================================================

    function finishWizard() {
        // Считываем параметры с текущих форм если нужно
        if (currentStep === 4) readModeParamsIntoState();
        if (currentStep === 5) readGeneralFieldsIntoState();

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
                    if (typeof startNormalInit === 'function') {
                        startNormalInit();
                    } else {
                        location.reload();
                    }
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
            if (typeof startNormalInit === 'function') {
                startNormalInit();
            } else {
                location.reload();
            }
        }
    }

    function buildWizardPayload() {
        var payload = {
            operatingMode: wizardState.operatingMode,
            backendEngine: wizardState.backendType === 'llama_cpp' ? 'llama_cpp' : 'ollama_api',
            algorithm: wizardState.algorithm,
            gpuMaxUsage: wizardState.gpuMaxUsage,
            vramMaxUsage: wizardState.vramMaxUsage,
            cpuMaxUsage: wizardState.cpuMaxUsage,
            ramMaxUsage: wizardState.ramMaxUsage
        };

        if (wizardState.replication) payload.modelReplication = wizardState.replication;
        if (wizardState.rpcCoordinator) payload.rpcCoordinator = wizardState.rpcCoordinator;
        if (wizardState.virtualModels) payload.virtualModels = wizardState.virtualModels;
        if (wizardState.distInference) payload.distInference = wizardState.distInference;

        return payload;
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
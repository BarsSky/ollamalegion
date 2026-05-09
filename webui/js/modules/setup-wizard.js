/**
 * setup-wizard.js — Initial Server Setup Wizard (5 steps + import)
 * OllamaLegion WebUI
 */
(function () {
    'use strict';

    var STORAGE_KEY = 'ollamalegion_initialized';
    var currentStep = 1;
    var totalSteps = 5;

    /**
     * Check if wizard has been completed
     */
    function isInitialized() {
        return localStorage.getItem(STORAGE_KEY) === 'true';
    }

    /**
     * Mark wizard as completed
     */
    function markInitialized() {
        localStorage.setItem(STORAGE_KEY, 'true');
    }

    /**
     * Reset wizard flag (for "Retake setup" button)
     */
    function reset() {
        localStorage.removeItem(STORAGE_KEY);
    }

    /**
     * Show the setup wizard modal
     */
    function start() {
        currentStep = 1;
        var existing = document.getElementById('setupWizardModal');
        if (existing) existing.remove();

        var modal = document.createElement('div');
        modal.className = 'modal active';
        modal.id = 'setupWizardModal';
        modal.style.display = 'flex';
        modal.innerHTML = buildWizardHTML();
        document.body.appendChild(modal);

        renderStep(currentStep);
        bindWizardEvents(modal);
    }

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

        var steps = [
            window.I18N ? I18N.t('wizard.step1') : 'Welcome',
            window.I18N ? I18N.t('wizard.step2') : 'Mode',
            window.I18N ? I18N.t('wizard.step3') : 'Settings',
            window.I18N ? I18N.t('wizard.step4') : 'General',
            window.I18N ? I18N.t('wizard.step5') : 'Summary'
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
            case 1:
                html = renderWelcomeStep();
                break;
            case 2:
                html = renderModeSelectionStep();
                break;
            case 3:
                html = renderModeParamsStep();
                break;
            case 4:
                html = renderGeneralSettingsStep();
                break;
            case 5:
                html = renderSummaryStep();
                break;
        }
        body.innerHTML = html;

        // Initialize dynamic components
        if (step === 2 && window.SettingsUI) {
            window.SettingsUI.setupModeSelector();
        }
        if (step === 3) {
            var mode = getWizardMode();
            if (window.SettingsUI) {
                window.SettingsUI.showModeFields(mode);
            }
        }
    }

    function renderStepFooter(step) {
        var footer = document.getElementById('wizardFooter');
        if (!footer) return;

        var html = '<div class="wizard-nav">';

        // Import button only on step 1
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

    // ---- Step 1: Welcome ----

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

    // ---- Step 2: Mode Selection ----

    function renderModeSelectionStep() {
        return '<div class="wizard-step-content mode-step">' +
            '<h3>' + (window.I18N ? I18N.t('wizard.step2') : 'Select Operating Mode') + '</h3>' +
            '<div class="mode-selector">' +
            '<div class="mode-cards">' +
            renderModeCard('standard', '⚙️',
                window.I18N ? I18N.t('settings.mode.standard') : 'Standard Balancer',
                window.I18N ? I18N.t('settings.mode.standard_desc') : 'Basic load balancing without RPC',
                true) +
            renderModeCard('replication', '📋',
                window.I18N ? I18N.t('settings.mode.replication') : 'Model Replication (A)',
                window.I18N ? I18N.t('settings.mode.replication_desc') : 'Replicate models across backends') +
            renderModeCard('rpc_coordinator', '🌐',
                window.I18N ? I18N.t('settings.mode.rpc_coordinator') : 'External RPC Coordinator (B)',
                window.I18N ? I18N.t('settings.mode.rpc_coordinator_desc') : 'External RPC coordinator') +
            renderModeCard('virtual_router', '🧩',
                window.I18N ? I18N.t('settings.mode.virtual_router') : 'Virtual Model Router (C)',
                window.I18N ? I18N.t('settings.mode.virtual_router_desc') : 'Pipeline parallelism via slices') +
            renderModeCard('distributed_inference', '🔬',
                window.I18N ? I18N.t('settings.mode.distributed') : 'Distributed Inference (D)',
                window.I18N ? I18N.t('settings.mode.distributed_desc') : 'Custom gRPC distributed inference') +
            '</div>' +
            '</div>' +
            '</div>';
    }

    function renderModeCard(mode, icon, title, desc, checked) {
        return '<label class="mode-card" data-mode="' + mode + '">' +
            '<input type="radio" name="operatingMode" value="' + mode + '" ' + (checked ? 'checked' : '') + '>' +
            '<div class="mode-card-icon">' + icon + '</div>' +
            '<div class="mode-card-title">' + title + '</div>' +
            '<div class="mode-card-desc">' + desc + '</div>' +
            '</label>';
    }

    function getWizardMode() {
        var checked = document.querySelector('input[name="operatingMode"]:checked');
        return checked ? checked.value : 'standard';
    }

    // ---- Step 3: Mode Parameters ----

    function renderModeParamsStep() {
        return '<div class="wizard-step-content params-step">' +
            '<h3>' + (window.I18N ? I18N.t('wizard.step3') : 'Mode Parameters') + '</h3>' +
            '<div class="wizard-mode-description" id="wizardModeDesc"></div>' +
            '<!-- Standard fields (always visible) -->' +
            '<div id="modeFields-standard" class="mode-fields">' +
            '<p style="color:var(--text-muted);">' + (window.I18N ? I18N.t('wizard.summary_empty') : 'Standard mode — no additional parameters required') + '</p>' +
            '</div>' +
            '<!-- Mode A fields -->' +
            '<div id="modeFields-replication" class="mode-fields" style="display:none;">' +
            '<div class="form-row">' +
            '<div class="form-group"><label>' + (window.I18N ? I18N.t('settings.model_replication_min') : 'Min Instances') + '</label>' +
            '<input type="number" id="modelReplicationMinInstances" class="form-control" value="1" min="0" max="10"></div>' +
            '<div class="form-group"><label>' + (window.I18N ? I18N.t('settings.model_replication_max') : 'Max Instances') + '</label>' +
            '<input type="number" id="modelReplicationMaxInstances" class="form-control" value="3" min="0" max="20"></div>' +
            '</div>' +
            '<div class="form-group"><label>' + (window.I18N ? I18N.t('settings.model_replication_idle_unload') : 'Idle Unload After') + '</label>' +
            '<input type="text" id="modelReplicationIdleUnload" class="form-control" value="10m" placeholder="10m, 30m, 1h"></div>' +
            '</div>' +
            '<!-- Mode B fields -->' +
            '<div id="modeFields-rpc_coordinator" class="mode-fields" style="display:none;">' +
            '<div class="form-group"><label>' + (window.I18N ? I18N.t('settings.rpc_coordinator_url') : 'Coordinator URL') + '</label>' +
            '<input type="text" id="rpcCoordinatorURL" class="form-control" placeholder="http://coordinator:8080"></div>' +
            '<div class="form-row">' +
            '<div class="form-group"><label>' + (window.I18N ? I18N.t('settings.rpc_coordinator_port') : 'Worker Port') + '</label>' +
            '<input type="number" id="rpcCoordinatorWorkerPort" class="form-control" value="18050"></div>' +
            '<div class="form-group"><label>' + (window.I18N ? I18N.t('settings.rpc_coordinator_protocol') : 'Protocol') + '</label>' +
            '<select id="rpcCoordinatorProtocol" class="form-control"><option value="http">HTTP</option><option value="grpc">gRPC</option></select></div>' +
            '</div>' +
            '<div class="form-group"><label>' + (window.I18N ? I18N.t('settings.rpc_coordinator_timeout') : 'Timeout') + '</label>' +
            '<input type="text" id="rpcCoordinatorTimeout" class="form-control" value="30s" placeholder="30s, 60s"></div>' +
            '</div>' +
            '<!-- Mode C fields -->' +
            '<div id="modeFields-virtual_router" class="mode-fields" style="display:none;">' +
            '<div class="form-group"><label>' + (window.I18N ? I18N.t('settings.virtual_models_coord_mode') : 'Coordination Mode') + '</label>' +
            '<select id="virtualModelsCoordMode" class="form-control">' +
            '<option value="sequential">' + (window.I18N ? I18N.t('settings.virtual_models_mode_sequential') : 'Sequential') + '</option>' +
            '<option value="parallel">' + (window.I18N ? I18N.t('settings.virtual_models_mode_parallel') : 'Parallel') + '</option>' +
            '<option value="tree">' + (window.I18N ? I18N.t('settings.virtual_models_mode_tree') : 'Tree') + '</option>' +
            '</select></div>' +
            '<div class="form-group"><label>' + (window.I18N ? I18N.t('settings.virtual_models_timeout') : 'Timeout (ms)') + '</label>' +
            '<input type="number" id="virtualModelsTimeout" class="form-control" value="30000" min="1000"></div>' +
            '</div>' +
            '<!-- Mode D fields -->' +
            '<div id="modeFields-distributed_inference" class="mode-fields" style="display:none;">' +
            '<div class="form-group"><label>' + (window.I18N ? I18N.t('settings.dist_inference_grpc_port') : 'gRPC Port') + '</label>' +
            '<input type="number" id="distInferenceGrpcPort" class="form-control" value="19000" min="1024" max="65535"></div>' +
            '</div>' +
            '</div>';
    }

    // ---- Step 4: General Settings ----

    function renderGeneralSettingsStep() {
        return '<div class="wizard-step-content general-step">' +
            '<h3>' + (window.I18N ? I18N.t('wizard.step4') : 'General Settings') + '</h3>' +
            '<div class="form-group">' +
            '<label>' + (window.I18N ? I18N.t('settings.balancing_mode') : 'Balancing Algorithm') + '</label>' +
            '<select id="balancingAlgorithm" class="form-control">' +
            '<option value="resource-aware">' + (window.I18N ? I18N.t('settings.balancing_resource_aware') : 'Resource-Aware') + '</option>' +
            '<option value="least-connections">' + (window.I18N ? I18N.t('settings.balancing_least_conn') : 'Least Connections') + '</option>' +
            '<option value="round-robin">' + (window.I18N ? I18N.t('settings.balancing_round_robin') : 'Round Robin') + '</option>' +
            '<option value="weighted">' + (window.I18N ? I18N.t('settings.balancing_weighted') : 'Weighted') + '</option>' +
            '<option value="model-affinity">' + (window.I18N ? I18N.t('settings.balancing_model_affinity') : 'Model Affinity') + '</option>' +
            '</select></div>' +
            '<div class="form-row">' +
            '<div class="form-group"><label>' + (window.I18N ? 'GPU Max %' : 'GPU Max %') + '</label>' +
            '<input type="number" id="gpuMaxUsage" class="form-control" value="90" min="50" max="100"></div>' +
            '<div class="form-group"><label>' + (window.I18N ? 'VRAM Max %' : 'VRAM Max %') + '</label>' +
            '<input type="number" id="vramMaxUsage" class="form-control" value="85" min="50" max="100"></div>' +
            '</div>' +
            '<div class="form-row">' +
            '<div class="form-group"><label>' + (window.I18N ? 'CPU Max %' : 'CPU Max %') + '</label>' +
            '<input type="number" id="cpuMaxUsage" class="form-control" value="80" min="50" max="100"></div>' +
            '<div class="form-group"><label>' + (window.I18N ? 'RAM Max %' : 'RAM Max %') + '</label>' +
            '<input type="number" id="ramMaxUsage" class="form-control" value="85" min="50" max="100"></div>' +
            '</div>' +
            '<div class="form-group"><label>' + (window.I18N ? I18N.t('settings.api_token') : 'API Token') + '</label>' +
            '<input type="password" id="apiToken" class="form-control" placeholder="' + (window.I18N ? I18N.t('settings.api_token') : 'API Token') + '"></div>' +
            '</div>';
    }

    // ---- Step 5: Summary ----

    function renderSummaryStep() {
        var mode = getWizardMode();
        var modeNames = {
            standard: window.I18N ? I18N.t('settings.mode.standard') : 'Standard Balancer',
            replication: window.I18N ? I18N.t('settings.mode.replication') : 'Model Replication (A)',
            rpc_coordinator: window.I18N ? I18N.t('settings.mode.rpc_coordinator') : 'External RPC Coordinator (B)',
            virtual_router: window.I18N ? I18N.t('settings.mode.virtual_router') : 'Virtual Model Router (C)',
            distributed_inference: window.I18N ? I18N.t('settings.mode.distributed') : 'Distributed Inference (D)'
        };
        var modeIcons = { standard: '⚙️', replication: '📋', rpc_coordinator: '🌐', virtual_router: '🧩', distributed_inference: '🔬' };

        var html = '<div class="wizard-step-content summary-step">' +
            '<h3>' + (window.I18N ? I18N.t('wizard.step5') : 'Summary') + '</h3>' +
            '<div class="wizard-summary-card">' +
            '<div class="wizard-summary-row"><span class="wizard-summary-label">' + (window.I18N ? I18N.t('settings.mode.standard') : 'Mode') + ':</span>' +
            '<span class="wizard-summary-value">' + (modeIcons[mode] || '') + ' ' + (modeNames[mode] || mode) + '</span></div>';

        // Mode-specific params
        if (mode === 'replication') {
            html += '<div class="wizard-summary-row"><span class="wizard-summary-label">Min Instances:</span>' +
                '<span class="wizard-summary-value">' + getFieldValue('modelReplicationMinInstances', 1) + '</span></div>';
            html += '<div class="wizard-summary-row"><span class="wizard-summary-label">Max Instances:</span>' +
                '<span class="wizard-summary-value">' + getFieldValue('modelReplicationMaxInstances', 3) + '</span></div>';
            html += '<div class="wizard-summary-row"><span class="wizard-summary-label">Idle Unload:</span>' +
                '<span class="wizard-summary-value">' + getFieldValue('modelReplicationIdleUnload', '10m') + '</span></div>';
        } else if (mode === 'rpc_coordinator') {
            html += '<div class="wizard-summary-row"><span class="wizard-summary-label">Coordinator URL:</span>' +
                '<span class="wizard-summary-value">' + getFieldValue('rpcCoordinatorURL', '-') + '</span></div>';
            html += '<div class="wizard-summary-row"><span class="wizard-summary-label">Worker Port:</span>' +
                '<span class="wizard-summary-value">' + getFieldValue('rpcCoordinatorWorkerPort', 18050) + '</span></div>';
            html += '<div class="wizard-summary-row"><span class="wizard-summary-label">Timeout:</span>' +
                '<span class="wizard-summary-value">' + getFieldValue('rpcCoordinatorTimeout', '30s') + '</span></div>';
        } else if (mode === 'virtual_router') {
            html += '<div class="wizard-summary-row"><span class="wizard-summary-label">Coord Mode:</span>' +
                '<span class="wizard-summary-value">' + getFieldValue('virtualModelsCoordMode', 'sequential') + '</span></div>';
            html += '<div class="wizard-summary-row"><span class="wizard-summary-label">Timeout:</span>' +
                '<span class="wizard-summary-value">' + getFieldValue('virtualModelsTimeout', 30000) + 'ms</span></div>';
        } else if (mode === 'distributed_inference') {
            html += '<div class="wizard-summary-row"><span class="wizard-summary-label">gRPC Port:</span>' +
                '<span class="wizard-summary-value">' + getFieldValue('distInferenceGrpcPort', 19000) + '</span></div>';
        }

        // Common settings
        html += '<div class="wizard-summary-divider"></div>';
        html += '<div class="wizard-summary-row"><span class="wizard-summary-label">Algorithm:</span>' +
            '<span class="wizard-summary-value">' + getFieldValue('balancingAlgorithm', 'resource-aware') + '</span></div>';
        html += '<div class="wizard-summary-row"><span class="wizard-summary-label">GPU Max:</span>' +
            '<span class="wizard-summary-value">' + getFieldValue('gpuMaxUsage', 90) + '%</span></div>';
        html += '<div class="wizard-summary-row"><span class="wizard-summary-label">VRAM Max:</span>' +
            '<span class="wizard-summary-value">' + getFieldValue('vramMaxUsage', 85) + '%</span></div>';

        html += '</div></div>';
        return html;
    }

    function getFieldValue(id, fallback) {
        var el = document.getElementById(id);
        return el ? (el.value || fallback) : fallback;
    }

    // ---- Event Binding ----

    function bindWizardEvents(modal) {
        // Delegate events
        modal.addEventListener('click', function (e) {
            var target = e.target;

            // Prev button
            if (target.id === 'wizardPrevBtn' || target.closest('#wizardPrevBtn')) {
                if (currentStep > 1) {
                    currentStep--;
                    renderStep(currentStep);
                }
                return;
            }

            // Next button
            if (target.id === 'wizardNextBtn' || target.closest('#wizardNextBtn')) {
                if (currentStep < totalSteps) {
                    // Validate before proceeding
                    if (currentStep === 3) {
                        var errors = validateWizardStep3();
                        if (errors.length > 0) {
                            showWizardError(errors.join('<br>'));
                            return;
                        }
                    }
                    currentStep++;
                    renderStep(currentStep);
                }
                return;
            }

            // Finish button
            if (target.id === 'wizardFinishBtn' || target.closest('#wizardFinishBtn')) {
                finishWizard();
                return;
            }

            // Import buttons
            if (target.id === 'wizardImportBtn' || target.id === 'wizardImportBtn2' || target.closest('#wizardImportBtn') || target.closest('#wizardImportBtn2')) {
                handleWizardImport();
                return;
            }

            // Step dots navigation (only to completed steps)
            var dot = target.closest('.wizard-step-dot');
            if (dot) {
                var step = parseInt(dot.dataset.step);
                if (step < currentStep) {
                    currentStep = step;
                    renderStep(currentStep);
                }
            }

            // Mode card selection in wizard
            var card = target.closest('.mode-card');
            if (card) {
                var radio = card.querySelector('input[type="radio"]');
                if (radio) {
                    radio.checked = true;
                    radio.dispatchEvent(new Event('change'));
                    document.querySelectorAll('.mode-card').forEach(function (c) {
                        c.classList.toggle('active', c === card);
                    });
                }
            }
        });
    }

    function validateWizardStep3() {
        var errors = [];
        var mode = getWizardMode();

        if (mode === 'rpc_coordinator') {
            var url = getFieldValue('rpcCoordinatorURL', '');
            if (!url) errors.push(window.I18N ? 'Coordinator URL is required' : 'Coordinator URL обязателен');
        }
        if (mode === 'replication') {
            var min = parseInt(getFieldValue('modelReplicationMinInstances', 0));
            var max = parseInt(getFieldValue('modelReplicationMaxInstances', 0));
            if (max < min) errors.push(window.I18N ? 'Max must be >= Min' : 'Max должно быть >= Min');
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

    // ---- Import in Wizard ----

    function handleWizardImport() {
        if (!window.ConfigIO) return;

        window.ConfigIO.importConfigFromFile()
            .then(function (data) {
                // Show preview first
                return window.ConfigIO.showImportPreview(data).then(function (confirmed) {
                    if (confirmed) {
                        applyWizardConfig(data);
                        // Jump to step 5 (summary) after import
                        currentStep = 5;
                        renderStep(currentStep);
                    }
                });
            })
            .catch(function (err) {
                showWizardError(err.message || 'Import failed');
            });
    }

    function applyWizardConfig(data) {
        if (!data) return;

        // Apply mode
        if (data.operating_mode) {
            var radio = document.querySelector('input[name="operatingMode"][value="' + data.operating_mode + '"]');
            if (radio) radio.checked = true;
        }

        // Apply settings
        if (data.settings) {
            var s = data.settings;
            setFieldValue('balancingAlgorithm', s.algorithm);
            setFieldValue('gpuMaxUsage', s.gpuMaxUsage);
            setFieldValue('vramMaxUsage', s.vramMaxUsage);
            setFieldValue('cpuMaxUsage', s.cpuMaxUsage);
            setFieldValue('ramMaxUsage', s.ramMaxUsage);
            setFieldValue('minFreeDisk', s.minFreeDisk);
            setFieldValue('apiToken', s.apiToken);
        }

        // Apply mode config
        if (data.mode_config) {
            var mc = data.mode_config;
            if (mc.modelReplication) {
                setFieldValue('modelReplicationMinInstances', mc.modelReplication.defaultMinInstances);
                setFieldValue('modelReplicationMaxInstances', mc.modelReplication.defaultMaxInstances);
                setFieldValue('modelReplicationIdleUnload', mc.modelReplication.idleUnloadAfter);
            }
            if (mc.rpcCoordinator) {
                setFieldValue('rpcCoordinatorURL', mc.rpcCoordinator.coordinatorURL);
                setFieldValue('rpcCoordinatorWorkerPort', mc.rpcCoordinator.workerPort);
                setFieldValue('rpcCoordinatorProtocol', mc.rpcCoordinator.protocol);
                setFieldValue('rpcCoordinatorTimeout', mc.rpcCoordinator.timeout);
            }
            if (mc.virtualModels) {
                setFieldValue('virtualModelsCoordMode', mc.virtualModels.coordMode);
                setFieldValue('virtualModelsTimeout', mc.virtualModels.timeout);
            }
            if (mc.distInference) {
                setFieldValue('distInferenceGrpcPort', mc.distInference.grpcPort);
            }
        }
    }

    function setFieldValue(id, value) {
        var el = document.getElementById(id);
        if (el && value !== undefined && value !== null) {
            el.value = value;
        }
    }

    // ---- Finish Wizard ----

    function finishWizard() {
        // Save settings
        if (typeof saveSettings === 'function') {
            saveSettings(true);
        }

        markInitialized();
        closeWizard();

        // Start normal initialization
        if (typeof startNormalInit === 'function') {
            startNormalInit();
        } else {
            // Fallback: reload page
            location.reload();
        }
    }

    function closeWizard() {
        var modal = document.getElementById('setupWizardModal');
        if (modal) modal.remove();
    }

    // ---- Public API ----

    window.SetupWizard = {
        isInitialized: isInitialized,
        start: start,
        reset: reset,
        markInitialized: markInitialized
    };

})();

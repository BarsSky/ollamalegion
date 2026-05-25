/**
 * settings-ui.js — Mode Selector, Accordion Sections, Field Visibility
 * OllamaLegion WebUI
 */
(function () {
    'use strict';

    const MODES = {
        standard: 'standard',
        replication: 'replication',
        rpc_coordinator: 'rpc_coordinator',
        virtual_router: 'virtual_router',
        distributed_inference: 'distributed_inference'
    };

    const STORAGE_KEY_SECTIONS = 'ollamalegion_settings_sections';

    // ---- Accordion Sections ----

    function setupAccordion() {
        document.querySelectorAll('.settings-section-header').forEach(function (header) {
            header.addEventListener('click', function () {
                var section = header.closest('.settings-section');
                var body = section.querySelector('.settings-section-body');
                var toggle = header.querySelector('.section-toggle');
                var isOpen = body.classList.contains('open');

                if (isOpen) {
                    body.classList.remove('open');
                    body.style.maxHeight = '0px';
                    body.style.opacity = '0';
                    if (toggle) toggle.textContent = '▶';
                } else {
                    body.classList.add('open');
                    body.style.maxHeight = body.scrollHeight + 'px';
                    body.style.opacity = '1';
                    if (toggle) toggle.textContent = '▼';
                }

                saveAccordionState(header.dataset.section || '', !isOpen);
            });
        });
    }

    function saveAccordionState(sectionId, isOpen) {
        try {
            var state = JSON.parse(localStorage.getItem(STORAGE_KEY_SECTIONS) || '{}');
            state[sectionId] = isOpen;
            localStorage.setItem(STORAGE_KEY_SECTIONS, JSON.stringify(state));
        } catch (e) { /* ignore */ }
    }

    function restoreAccordionState() {
        try {
            var state = JSON.parse(localStorage.getItem(STORAGE_KEY_SECTIONS) || '{}');
            document.querySelectorAll('.settings-section').forEach(function (section) {
                var header = section.querySelector('.settings-section-header');
                var body = section.querySelector('.settings-section-body');
                var toggle = header ? header.querySelector('.section-toggle') : null;
                var sectionId = header ? (header.dataset.section || '') : '';

                // Default: open
                var shouldOpen = state[sectionId] !== false;

                if (shouldOpen) {
                    if (body) {
                        body.classList.add('open');
                        body.style.maxHeight = body.scrollHeight + 'px';
                        body.style.opacity = '1';
                    }
                    if (toggle) toggle.textContent = '▼';
                } else {
                    if (body) {
                        body.classList.remove('open');
                        body.style.maxHeight = '0px';
                        body.style.opacity = '0';
                    }
                    if (toggle) toggle.textContent = '▶';
                }
            });
        } catch (e) { /* ignore */ }
    }

    // ---- Mode Selector ----

    function setupModeSelector() {
        // NOTE: Смена режима теперь полностью контролируется mode-wizard.js.
        // Этот метод только инициализирует визуальное состояние без обработчиков change,
        // чтобы избежать конфликта с мастером переформирования.
        var current = getCurrentMode();
        if (current) {
            updateModeCards(current);
            showModeFields(current);
        }
    }

    /**
     * Принудительная синхронизация UI с серверным operatingMode.
     * Вызывается из app.js при загрузке настроек и после применения мастера.
     */
    function syncModeFromServer(serverMode) {
        if (!serverMode) return;
        var radio = document.querySelector('input[name="operatingMode"][value="' + serverMode + '"]');
        if (radio) {
            radio.checked = true;
            updateModeCards(serverMode);
            showModeFields(serverMode);
        }
    }

    function getCurrentMode() {
        var checked = document.querySelector('input[name="operatingMode"]:checked');
        return checked ? checked.value : 'standard';
    }

    function updateModeCards(mode) {
        document.querySelectorAll('.mode-card').forEach(function (card) {
            if (card.dataset.mode === mode) {
                card.classList.add('active');
            } else {
                card.classList.remove('active');
            }
        });
    }

    function showModeFields(mode) {
        // Hide all mode field containers
        document.querySelectorAll('.mode-fields').forEach(function (container) {
            container.classList.remove('active');
            container.style.display = 'none';
        });
        // Show active mode fields
        var activeFields = document.getElementById('modeFields-' + mode);
        if (activeFields) {
            activeFields.classList.add('active');
            activeFields.style.display = 'block';
        }

        // --- Adaptive UI: hide/show variant-related sections ---
        updateVariantSections(mode);
    }

    function updateVariantSections(mode) {
        // Map mode to variant class
        var variantMap = {
            standard: '',
            replication: 'variant-a',
            rpc_coordinator: 'variant-b',
            virtual_router: 'variant-c',
            distributed_inference: 'variant-d'
        };

        // Show/hide variant sections across the whole page
        document.querySelectorAll('.variant-section').forEach(function (sec) {
            var classes = sec.className.split(' ');
            var hasVariant = false;
            var isMatch = false;
            classes.forEach(function (cls) {
                if (cls.indexOf('variant-') === 0) {
                    hasVariant = true;
                    if (cls === variantMap[mode]) {
                        isMatch = true;
                    }
                }
            });
            if (!hasVariant) return; // not a variant section
            if (isMatch || mode === 'standard' && hasVariant) {
                // For standard mode, we still show variant sections that are
                // explicitly marked as "always-show" (if any). Otherwise hide all.
                if (mode === 'standard') {
                    sec.style.display = 'none';
                } else {
                    sec.style.display = '';
                }
            } else {
                sec.style.display = 'none';
            }
        });

        // Hide/show sidebar nav items for variant-specific pages
        // NOTE: monitor и agents всегда видны, независимо от режима
        var navMap = {
            standard: { show: ['dashboard', 'monitor', 'backends', 'models', 'sessions', 'queue', 'agents', 'logs', 'settings'] },
            replication: { show: ['dashboard', 'monitor', 'backends', 'models', 'sessions', 'queue', 'agents', 'logs', 'settings'] },
            rpc_coordinator: { show: ['dashboard', 'monitor', 'backends', 'models', 'sessions', 'queue', 'agents', 'logs', 'settings'] },
            virtual_router: { show: ['dashboard', 'monitor', 'backends', 'models', 'sessions', 'queue', 'agents', 'logs', 'settings'] },
            distributed_inference: { show: ['dashboard', 'monitor', 'backends', 'models', 'sessions', 'queue', 'agents', 'logs', 'settings'] }
        };

        var allowed = navMap[mode] ? navMap[mode].show : navMap.standard.show;
        document.querySelectorAll('.nav-item[data-page]').forEach(function (item) {
            var page = item.dataset.page;
            if (allowed.indexOf(page) >= 0) {
                item.style.display = '';
            } else {
                item.style.display = 'none';
            }
        });
    }

    function confirmModeChange(newMode) {
        var msg = window.I18N
            ? I18N.t('settings.mode.confirm_change')
            : 'Changing the operating mode may affect active connections. Continue?';
        return confirm(msg);
    }

    // ---- Collect Mode Config ----

    function collectModeConfig() {
        var mode = getCurrentMode();
        var config = {
            enabled: mode !== 'standard'
        };

        switch (mode) {
            case 'replication':
                config.modelReplication = {
                    enabled: true,
                    defaultMinInstances: parseIntField('modelReplicationMinInstances', 1),
                    defaultMaxInstances: parseIntField('modelReplicationMaxInstances', 3),
                    idleUnloadAfter: textField('modelReplicationIdleUnload', '10m')
                };
                break;
            case 'rpc_coordinator':
                config.rpcCoordinator = {
                    enabled: true,
                    coordinatorURL: textField('rpcCoordinatorURL', ''),
                    workerPort: parseIntField('rpcCoordinatorWorkerPort', 18050),
                    protocol: textField('rpcCoordinatorProtocol', 'http'),
                    timeout: textField('rpcCoordinatorTimeout', '30s')
                };
                break;
            case 'virtual_router':
                config.virtualModels = {
                    enabled: true,
                    coordMode: textField('virtualModelsCoordMode', 'sequential'),
                    timeout: parseIntField('virtualModelsTimeout', 30000)
                };
                break;
            case 'distributed_inference':
                config.distInference = {
                    enabled: true,
                    grpcPort: parseIntField('distInferenceGrpcPort', 19000)
                };
                break;
            default:
                // Standard mode — no RPC config
                break;
        }

        return { mode: mode, config: config };
    }

    function parseIntField(id, fallback) {
        var el = document.getElementById(id);
        return el ? (parseInt(el.value) || fallback) : fallback;
    }

    function textField(id, fallback) {
        var el = document.getElementById(id);
        return el ? (el.value || fallback) : fallback;
    }

    // ---- Apply Mode Config (load from server) ----

    function applyModeConfig(serverConfig) {
        if (!serverConfig) return;

        // Determine mode from server config
        var mode = 'standard';
        if (serverConfig.modelReplication && serverConfig.modelReplication.enabled) mode = 'replication';
        else if (serverConfig.rpcCoordinator && serverConfig.rpcCoordinator.enabled) mode = 'rpc_coordinator';
        else if (serverConfig.virtualModels && serverConfig.virtualModels.enabled) mode = 'virtual_router';
        else if (serverConfig.distInference && serverConfig.distInference.enabled) mode = 'distributed_inference';

        var radio = document.querySelector('input[name="operatingMode"][value="' + mode + '"]');
        if (radio) {
            radio.checked = true;
            updateModeCards(mode);
            showModeFields(mode);
        }

        // Fill fields per mode
        if (serverConfig.modelReplication) {
            setFieldValue('modelReplicationMinInstances', serverConfig.modelReplication.defaultMinInstances);
            setFieldValue('modelReplicationMaxInstances', serverConfig.modelReplication.defaultMaxInstances);
            setFieldValue('modelReplicationIdleUnload', serverConfig.modelReplication.idleUnloadAfter);
        }
        if (serverConfig.rpcCoordinator) {
            setFieldValue('rpcCoordinatorURL', serverConfig.rpcCoordinator.coordinatorURL);
            setFieldValue('rpcCoordinatorWorkerPort', serverConfig.rpcCoordinator.workerPort);
            setFieldValue('rpcCoordinatorProtocol', serverConfig.rpcCoordinator.protocol);
            setFieldValue('rpcCoordinatorTimeout', serverConfig.rpcCoordinator.timeout);
        }
        if (serverConfig.virtualModels) {
            setFieldValue('virtualModelsCoordMode', serverConfig.virtualModels.coordMode);
            setFieldValue('virtualModelsTimeout', serverConfig.virtualModels.timeout);
        }
        if (serverConfig.distInference) {
            setFieldValue('distInferenceGrpcPort', serverConfig.distInference.grpcPort);
        }
    }

    function setFieldValue(id, value) {
        var el = document.getElementById(id);
        if (el && value !== undefined && value !== null) {
            el.value = value;
        }
    }

    // ---- Validation ----

    function validateModeFields() {
        var mode = getCurrentMode();
        var errors = [];

        switch (mode) {
            case 'rpc_coordinator':
                var url = textField('rpcCoordinatorURL', '');
                if (!url) errors.push('Coordinator URL is required');
                var port = parseIntField('rpcCoordinatorWorkerPort', 0);
                if (port < 1024 || port > 65535) errors.push('Worker port must be between 1024 and 65535');
                var timeout = textField('rpcCoordinatorTimeout', '');
                if (timeout && !/^\d+[smh]$/.test(timeout)) errors.push('Timeout must be like 30s, 5m, 1h');
                break;
            case 'virtual_router':
                var vmTimeout = parseIntField('virtualModelsTimeout', 0);
                if (vmTimeout < 1000) errors.push('Timeout must be at least 1000ms');
                break;
            case 'distributed_inference':
                var gPort = parseIntField('distInferenceGrpcPort', 0);
                if (gPort < 1024 || gPort > 65535) errors.push('gRPC port must be between 1024 and 65535');
                break;
            case 'replication':
                var minInst = parseIntField('modelReplicationMinInstances', 0);
                var maxInst = parseIntField('modelReplicationMaxInstances', 0);
                if (minInst < 0) errors.push('Min instances must be >= 0');
                if (maxInst < minInst) errors.push('Max instances must be >= min instances');
                break;
        }

        return errors;
    }

    // ---- Backend Engine Switch ----

    function setupBackendEngineSwitch() {
        var cards = document.querySelectorAll('#backendEngineCards .mode-card[data-backend-engine-type]');
        cards.forEach(function (card) {
            card.addEventListener('click', function (e) {
                var radio = card.querySelector('input[name="settingsBackendEngine"]');
                if (!radio) return;

                var newType = radio.value;
                var currentType = window.BackendTypeFilter
                    ? BackendTypeFilter.getCurrentType()
                    : (localStorage.getItem('ollamalegion_backend_type') || 'ollama');

                // Проверяем, что тип действительно меняется
                if (newType === currentType) return;
                
                // Принудительно переключаем визуально (браузер уже сделал это,
                // но гарантируем синхронизацию с нашим состоянием)
                syncBackendEngineCards(newType);

                // Проверяем совместимость режима с новым типом
                var currentMode = getCurrentMode();
                var availableModes = window.BackendTypeFilter
                    ? BackendTypeFilter.getAvailableModes(newType)
                    : (newType === 'llama_cpp' ? ['standard', 'virtual_router', 'distributed_inference'] : ['standard', 'replication', 'rpc_coordinator']);

                var modeNeedsReset = availableModes.indexOf(currentMode) < 0;

                var msg = window.I18N
                    ? I18N.t('wizard.backend_type_confirm_change')
                    : '\u0412\u044B \u0443\u0432\u0435\u0440\u0435\u043D\u044B, \u0447\u0442\u043E \u0445\u043E\u0442\u0438\u0442\u0435 \u0441\u043C\u0435\u043D\u0438\u0442\u044C \u0442\u0438\u043F \u0434\u0432\u0438\u0436\u043A\u0430 \u043D\u0430 \u00AB' + (newType === 'llama_cpp' ? 'llama.cpp' : 'Ollama API') + '\u00BB?';

                if (modeNeedsReset) {
                    msg += '\n\n' + (window.I18N
                        ? I18N.t('wizard.backend_type_mode_reset')
                        : '\u0422\u0435\u043A\u0443\u0449\u0438\u0439 \u0440\u0435\u0436\u0438\u043C \u0431\u0443\u0434\u0435\u0442 \u0441\u0431\u0440\u043E\u0448\u0435\u043D \u043D\u0430 \u00ABStandard\u00BB.');
                }

                if (!confirm(msg)) return;

                // Сохраняем на сервер
                var payload = {
                    backendEngine: newType === 'llama_cpp' ? 'llama_cpp' : 'ollama_api'
                };

                // Если режим нужно сбросить — включаем standard
                if (modeNeedsReset) {
                    payload.operatingMode = 'standard';
                }

                var statusMsg = document.getElementById('backendEngineStatusMsg');
                if (statusMsg) {
                    statusMsg.style.display = 'block';
                    statusMsg.textContent = window.I18N ? I18N.t('wizard.applying') : '\u041F\u0440\u0438\u043C\u0435\u043D\u0435\u043D\u0438\u0435...';
                    statusMsg.style.color = 'var(--text-muted)';
                }

                // Устанавливаем флаг защиты от гонки перед сохранением
                if (window.BackendTypeFilter) {
                    BackendTypeFilter._savingInProgress = true;
                    BackendTypeFilter._lastSavedType = newType;
                }

                if (window.Api && window.Api.updateConfig) {
                    window.Api.updateConfig(payload).then(function () {
                        // Снимаем флаг с задержкой — даём серверу время обновить cluster state
                        setTimeout(function () {
                            if (window.BackendTypeFilter) {
                                BackendTypeFilter._savingInProgress = false;
                            }
                        }, 2500);

                        // Обновляем кэш
                        if (window.__lastServerConfig) {
                            window.__lastServerConfig.backendEngine = newType === 'llama_cpp' ? 'llama_cpp' : 'ollama_api';
                            if (modeNeedsReset) {
                                window.__lastServerConfig.operatingMode = 'standard';
                            }
                        }

                        // Обновляем UI
                        if (window.BackendTypeFilter) {
                            BackendTypeFilter.setCurrentType(newType);
                            localStorage.setItem('ollamalegion_backend_type', newType);
                        }

                        // Синхронизируем карточки режимов
                        syncBackendEngineCards(newType);

                        if (modeNeedsReset && window.SettingsUI) {
                            SettingsUI.syncModeFromServer('standard');
                        }

                        if (statusMsg) {
                            statusMsg.textContent = newType === 'llama_cpp'
                                ? '\u2705 \u041F\u0435\u0440\u0435\u043A\u043B\u044E\u0447\u0435\u043D\u043E \u043D\u0430 llama.cpp'
                                : '\u2705 \u041F\u0435\u0440\u0435\u043A\u043B\u044E\u0447\u0435\u043D\u043E \u043D\u0430 Ollama API';
                            statusMsg.style.color = 'var(--success)';
                            setTimeout(function () {
                                if (statusMsg) statusMsg.style.display = 'none';
                            }, 3000);
                        }

                        if (typeof showToast === 'function') {
                            showToast(
                                newType === 'llama_cpp'
                                    ? (window.I18N ? I18N.t('wizard.backend_type_switched_llama') : '\u041F\u0435\u0440\u0435\u043A\u043B\u044E\u0447\u0435\u043D\u043E \u043D\u0430 llama.cpp')
                                    : (window.I18N ? I18N.t('wizard.backend_type_switched_ollama') : '\u041F\u0435\u0440\u0435\u043A\u043B\u044E\u0447\u0435\u043D\u043E \u043D\u0430 Ollama API'),
                                'success'
                            );
                        }
                    }).catch(function (err) {
                        // Снимаем флаг при ошибке сохранения
                        if (window.BackendTypeFilter) {
                            BackendTypeFilter._savingInProgress = false;
                        }
                        if (statusMsg) {
                            statusMsg.textContent = (window.I18N ? I18N.t('wizard.error') : '\u041E\u0448\u0438\u0431\u043A\u0430') + ': ' + (err.message || err);
                            statusMsg.style.color = 'var(--danger)';
                        }
                        console.error('[SettingsUI] Failed to switch backend engine:', err);
                    });
                } else {
                    // Fallback: только локально
                    // Снимаем флаг — сохранения на сервер не будет
                    if (window.BackendTypeFilter) {
                        BackendTypeFilter._savingInProgress = false;
                    }
                    if (window.BackendTypeFilter) {
                        BackendTypeFilter.setCurrentType(newType);
                    }
                    localStorage.setItem('ollamalegion_backend_type', newType);
                    syncBackendEngineCards(newType);
                    if (modeNeedsReset && window.SettingsUI) {
                        SettingsUI.syncModeFromServer('standard');
                    }
                    if (statusMsg) {
                        statusMsg.textContent = '\u26A0\uFE0F \u0421\u043E\u0445\u0440\u0430\u043D\u0435\u043D\u043E \u043B\u043E\u043A\u0430\u043B\u044C\u043D\u043E (\u0441\u0435\u0440\u0432\u0435\u0440 \u043D\u0435\u0434\u043E\u0441\u0442\u0443\u043F\u0435\u043D)';
                        statusMsg.style.color = 'var(--warning)';
                    }
                }
            });
        });
    }

    /**
     * Синхронизировать карточки выбора backend engine с текущим типом.
     */
    function syncBackendEngineCards(type) {
        document.querySelectorAll('#backendEngineCards .mode-card[data-backend-engine-type]').forEach(function (card) {
            var radio = card.querySelector('input[name="settingsBackendEngine"]');
            if (radio && radio.value === type) {
                radio.checked = true;
                card.classList.add('active');
            } else {
                if (radio) radio.checked = false;
                card.classList.remove('active');
            }
        });
    }

    /**
     * Инициализировать отображение backend engine карточек из текущего состояния.
     */
    function initBackendEngineCards() {
        var currentType = window.BackendTypeFilter
            ? BackendTypeFilter.getCurrentType()
            : (localStorage.getItem('ollamalegion_backend_type') || 'ollama');
        syncBackendEngineCards(currentType);
    }

    // ---- Public API ----

    window.SettingsUI = {
        setupAccordion: setupAccordion,
        restoreAccordionState: restoreAccordionState,
        setupModeSelector: setupModeSelector,
        getCurrentMode: getCurrentMode,
        showModeFields: showModeFields,
        syncModeFromServer: syncModeFromServer,
        collectModeConfig: collectModeConfig,
        applyModeConfig: applyModeConfig,
        validateModeFields: validateModeFields,
        setupBackendEngineSwitch: setupBackendEngineSwitch,
        initBackendEngineCards: initBackendEngineCards,
        syncBackendEngineCards: syncBackendEngineCards,
        MODES: MODES
    };

    // Auto-init on DOM ready if not already loaded
    if (document.readyState === 'complete' || document.readyState === 'interactive') {
        // Will be called explicitly from app.js
    }

})();

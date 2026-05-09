/**
 * config-io.js — Import / Export configuration with preview
 * OllamaLegion WebUI
 */
(function () {
    'use strict';

    /**
     * Export current settings to JSON file
     */
    function exportConfig() {
        var config = collectFullConfig();
        var json = JSON.stringify(config, null, 2);
        var filename = 'ollamalegion-config-' + new Date().toISOString().slice(0, 10) + '.json';
        Utils.downloadFile(json, filename, 'application/json');
        showToastMsg(
            window.I18N ? I18N.t('config.export_success') : 'Configuration exported',
            'success'
        );
    }

    /**
     * Collect full configuration from UI
     */
    function collectFullConfig() {
        var modeInfo = window.SettingsUI ? window.SettingsUI.collectModeConfig() : { mode: 'standard', config: {} };
        var settings = collectCommonSettings();

        var backends = [];
        try {
            // Collect from data if available
            if (window.ui && window.ui.data && window.ui.data.backends) {
                backends = window.ui.data.backends.map(function (b) {
                    return {
                        id: b.id,
                        name: b.name || b.id,
                        host: b.host,
                        ollamaPort: b.ollamaPort || 11434,
                        agentPort: b.agentPort || 18032,
                        weight: b.weight || 1
                    };
                });
            }
        } catch (e) { /* optional */ }

        return {
            version: '1.0',
            exportedAt: new Date().toISOString(),
            operating_mode: modeInfo.mode,
            settings: settings,
            mode_config: modeInfo.config,
            backends: backends
        };
    }

    function collectCommonSettings() {
        return {
            algorithm: getFieldValue('balancingAlgorithm', 'resource-aware'),
            useEnhancedScoring: getFieldChecked('useEnhancedScoring', true),
            modelAffinity: getFieldChecked('modelAffinity', true),
            sessionStickiness: getFieldChecked('sessionStickiness', true),
            predictionFiltering: getFieldChecked('predictionFiltering', true),
            gpuMaxUsage: parseIntField('gpuMaxUsage', 90),
            vramMaxUsage: parseIntField('vramMaxUsage', 85),
            cpuMaxUsage: parseIntField('cpuMaxUsage', 80),
            ramMaxUsage: parseIntField('ramMaxUsage', 85),
            minFreeDisk: parseIntField('minFreeDisk', 10240),
            apiToken: getFieldValue('apiToken', '')
        };
    }

    function getFieldValue(id, fallback) {
        var el = document.getElementById(id);
        return el ? (el.value || fallback) : fallback;
    }

    function getFieldChecked(id, fallback) {
        var el = document.getElementById(id);
        return el ? el.checked : fallback;
    }

    function parseIntField(id, fallback) {
        var el = document.getElementById(id);
        return el ? (parseInt(el.value) || fallback) : fallback;
    }

    /**
     * Open file dialog and import configuration
     * Returns a Promise that resolves with the parsed config
     */
    function importConfigFromFile() {
        return new Promise(function (resolve, reject) {
            var input = document.createElement('input');
            input.type = 'file';
            input.accept = '.json';
            input.addEventListener('change', function () {
                var file = input.files[0];
                if (!file) {
                    reject(new Error('No file selected'));
                    return;
                }
                var reader = new FileReader();
                reader.onload = function (e) {
                    try {
                        var data = JSON.parse(e.target.result);
                        var validation = validateConfig(data);
                        if (!validation.valid) {
                            reject(new Error(validation.error));
                            return;
                        }
                        resolve(data);
                    } catch (err) {
                        reject(new Error(window.I18N ? I18N.t('config.import_parse_error') : 'Invalid JSON format'));
                    }
                };
                reader.onerror = function () {
                    reject(new Error('Failed to read file'));
                };
                reader.readAsText(file);
            });
            input.click();
        });
    }

    /**
     * Validate imported configuration structure
     */
    function validateConfig(data) {
        if (!data || typeof data !== 'object') {
            return { valid: false, error: window.I18N ? I18N.t('config.import_invalid') : 'Invalid config format' };
        }
        if (!data.version) {
            return { valid: false, error: window.I18N ? I18N.t('config.import_invalid') : 'Missing version field' };
        }
        if (!data.operating_mode) {
            return { valid: false, error: window.I18N ? I18N.t('config.import_invalid') : 'Missing operating mode' };
        }
        var validModes = ['standard', 'replication', 'rpc_coordinator', 'virtual_router', 'distributed_inference'];
        if (validModes.indexOf(data.operating_mode) === -1) {
            return { valid: false, error: 'Unknown operating mode: ' + data.operating_mode };
        }
        return { valid: true };
    }

    /**
     * Generate diff-like preview of config changes
     */
    function generatePreview(data) {
        var current = collectFullConfig();
        var lines = [];
        var modeNames = {
            standard: window.I18N ? I18N.t('settings.mode.standard') : 'Standard',
            replication: window.I18N ? I18N.t('settings.mode.replication') : 'Model Replication (A)',
            rpc_coordinator: window.I18N ? I18N.t('settings.mode.rpc_coordinator') : 'External RPC Coordinator (B)',
            virtual_router: window.I18N ? I18N.t('settings.mode.virtual_router') : 'Virtual Model Router (C)',
            distributed_inference: window.I18N ? I18N.t('settings.mode.distributed') : 'Distributed Inference (D)'
        };

        // Mode change
        var oldModeName = modeNames[current.operating_mode] || current.operating_mode;
        var newModeName = modeNames[data.operating_mode] || data.operating_mode;
        if (current.operating_mode !== data.operating_mode) {
            lines.push({ type: 'change', label: window.I18N ? I18N.t('settings.mode.standard') : 'Mode', oldVal: oldModeName, newVal: newModeName });
        } else {
            lines.push({ type: 'same', label: window.I18N ? I18N.t('settings.mode.standard') : 'Mode', value: oldModeName });
        }

        // Settings comparison
        if (data.settings) {
            var s = data.settings;
            var cs = current.settings;
            if (s.algorithm && s.algorithm !== cs.algorithm) {
                lines.push({ type: 'change', label: 'Algorithm', oldVal: cs.algorithm, newVal: s.algorithm });
            }
            if (s.gpuMaxUsage && s.gpuMaxUsage !== cs.gpuMaxUsage) {
                lines.push({ type: 'change', label: 'GPU Max %', oldVal: cs.gpuMaxUsage + '%', newVal: s.gpuMaxUsage + '%' });
            }
            if (s.vramMaxUsage && s.vramMaxUsage !== cs.vramMaxUsage) {
                lines.push({ type: 'change', label: 'VRAM Max %', oldVal: cs.vramMaxUsage + '%', newVal: s.vramMaxUsage + '%' });
            }
            if (s.cpuMaxUsage && s.cpuMaxUsage !== cs.cpuMaxUsage) {
                lines.push({ type: 'change', label: 'CPU Max %', oldVal: cs.cpuMaxUsage + '%', newVal: s.cpuMaxUsage + '%' });
            }
            if (s.ramMaxUsage && s.ramMaxUsage !== cs.ramMaxUsage) {
                lines.push({ type: 'change', label: 'RAM Max %', oldVal: cs.ramMaxUsage + '%', newVal: s.ramMaxUsage + '%' });
            }
            if (s.minFreeDisk && s.minFreeDisk !== cs.minFreeDisk) {
                lines.push({ type: 'change', label: 'Min Free Disk', oldVal: cs.minFreeDisk + 'MB', newVal: s.minFreeDisk + 'MB' });
            }
        }

        // Mode config comparison (new fields)
        if (data.mode_config) {
            var mc = data.mode_config;
            if (mc.modelReplication) {
                var mr = mc.modelReplication;
                lines.push({ type: 'new', label: 'Replication Min', value: mr.defaultMinInstances });
                lines.push({ type: 'new', label: 'Replication Max', value: mr.defaultMaxInstances });
                lines.push({ type: 'new', label: 'Idle Unload', value: mr.idleUnloadAfter });
            }
            if (mc.rpcCoordinator) {
                var rc = mc.rpcCoordinator;
                lines.push({ type: 'new', label: 'Coordinator URL', value: rc.coordinatorURL || '-' });
                lines.push({ type: 'new', label: 'Worker Port', value: rc.workerPort });
                lines.push({ type: 'new', label: 'Protocol', value: rc.protocol });
                lines.push({ type: 'new', label: 'Timeout', value: rc.timeout });
            }
            if (mc.virtualModels) {
                var vm = mc.virtualModels;
                lines.push({ type: 'new', label: 'Coord Mode', value: vm.coordMode });
                lines.push({ type: 'new', label: 'Timeout (ms)', value: vm.timeout });
            }
            if (mc.distInference) {
                lines.push({ type: 'new', label: 'gRPC Port', value: mc.distInference.grpcPort });
            }
        }

        // Backends count
        var backendCount = (data.backends && data.backends.length) || 0;
        if (backendCount > 0) {
            lines.push({ type: 'new', label: window.I18N ? I18N.t('nav.backends') : 'Backends', value: backendCount + ' configured' });
        }

        return lines;
    }

    /**
     * Show import preview modal and apply if confirmed
     */
    function showImportPreview(data) {
        return new Promise(function (resolve) {
            var preview = generatePreview(data);
            var existingPreview = document.getElementById('importPreviewModal');
            if (existingPreview) existingPreview.remove();

            var modal = document.createElement('div');
            modal.className = 'modal active';
            modal.id = 'importPreviewModal';
            modal.style.display = 'flex';

            var html = '<div class="modal-content" style="max-width:600px;">';
            html += '<div class="modal-header">';
            html += '<h3>' + (window.I18N ? I18N.t('config.import_confirm') : 'Import Configuration') + '</h3>';
            html += '<button class="modal-close" id="importPreviewClose">&times;</button>';
            html += '</div>';
            html += '<div class="modal-body">';

            if (preview.length === 0) {
                html += '<p style="color:var(--text-muted);">' + (window.I18N ? I18N.t('config.import_success') : 'No changes detected') + '</p>';
            } else {
                html += '<div class="import-preview-list">';
                preview.forEach(function (item) {
                    var icon = item.type === 'change' ? '🔄' : (item.type === 'new' ? '➕' : '✅');
                    var cls = item.type === 'change' ? 'preview-change' : (item.type === 'new' ? 'preview-new' : 'preview-same');
                    html += '<div class="import-preview-item ' + cls + '">';
                    html += '<span class="preview-icon">' + icon + '</span>';
                    html += '<span class="preview-label">' + item.label + ':</span>';
                    if (item.type === 'change') {
                        html += '<span class="preview-old">' + item.oldVal + '</span>';
                        html += '<span class="preview-arrow">→</span>';
                        html += '<span class="preview-new-val">' + item.newVal + '</span>';
                    } else {
                        html += '<span class="preview-value">' + (item.value || '') + '</span>';
                    }
                    html += '</div>';
                });
                html += '</div>';
            }

            html += '</div>';
            html += '<div class="modal-footer">';
            html += '<button class="btn btn-secondary" id="importPreviewCancel">' + (window.I18N ? I18N.t('common.cancel') : 'Cancel') + '</button>';
            html += '<button class="btn btn-primary" id="importPreviewApply">' + (window.I18N ? I18N.t('config.import') : 'Apply') + '</button>';
            html += '</div></div>';

            modal.innerHTML = html;
            document.body.appendChild(modal);

            var resolved = false;

            function cleanup(result) {
                if (resolved) return;
                resolved = true;
                modal.remove();
                resolve(result);
            }

            document.getElementById('importPreviewClose').onclick = function () { cleanup(false); };
            document.getElementById('importPreviewCancel').onclick = function () { cleanup(false); };
            document.getElementById('importPreviewApply').onclick = function () { cleanup(true); };
            modal.addEventListener('click', function (e) {
                if (e.target === modal) cleanup(false);
            });
        });
    }

    /**
     * Apply imported config to UI
     */
    function applyConfig(data) {
        if (!data) return;

        // Apply mode
        if (data.operating_mode) {
            var radio = document.querySelector('input[name="operatingMode"][value="' + data.operating_mode + '"]');
            if (radio) {
                radio.checked = true;
                if (window.SettingsUI) {
                    window.SettingsUI.updateModeCards(data.operating_mode);
                    window.SettingsUI.showModeFields(data.operating_mode);
                }
            }
        }

        // Apply common settings
        if (data.settings) {
            var s = data.settings;
            setFieldValue('balancingAlgorithm', s.algorithm);
            setFieldChecked('useEnhancedScoring', s.useEnhancedScoring);
            setFieldChecked('modelAffinity', s.modelAffinity);
            setFieldChecked('sessionStickiness', s.sessionStickiness);
            setFieldChecked('predictionFiltering', s.predictionFiltering);
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

        // Trigger auto-save
        if (typeof autoSaveSettings === 'function') {
            autoSaveSettings();
        }
    }

    function setFieldValue(id, value) {
        var el = document.getElementById(id);
        if (el && value !== undefined && value !== null && value !== '') {
            el.value = value;
        }
    }

    function setFieldChecked(id, value) {
        var el = document.getElementById(id);
        if (el && value !== undefined) {
            el.checked = value === true;
        }
    }

    function showToastMsg(msg, type) {
        if (typeof showToast === 'function') {
            showToast(msg, type);
        }
    }

    // ---- Public API ----

    window.ConfigIO = {
        exportConfig: exportConfig,
        collectFullConfig: collectFullConfig,
        importConfigFromFile: importConfigFromFile,
        validateConfig: validateConfig,
        generatePreview: generatePreview,
        showImportPreview: showImportPreview,
        applyConfig: applyConfig
    };

})();

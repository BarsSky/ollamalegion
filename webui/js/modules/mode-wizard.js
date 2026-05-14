/**
 * mode-wizard.js — Мастер переформирования режима работы балансера
 * OllamaLegion WebUI
 *
 * Перехватывает смену operatingMode, показывает подробную справку,
 * позволяет сконфигурировать параметры выбранного варианта и
 * применяет изменения через API с последующей синхронизацией UI.
 */
(function () {
    'use strict';

    const MODE_INFO = {
        standard: {
            icon: '⚙️',
            titleKey: 'settings.mode.standard',
            descKey: 'settings.mode.standard_long',
            fields: [], // нет доп. полей
            paramDocs: {
                algorithm: { desc: 'Алгоритм балансировки', values: 'resource-aware | least-connections | round-robin | weighted | model-affinity' },
                modelAffinity: { desc: 'Привязка запросов к бэкенду с загруженной моделью', values: 'true | false' },
                sessionStickiness: { desc: 'Закрепление сессии за одним бэкендом', values: 'true | false' }
            },
            examples: [
                { param: 'algorithm', value: 'resource-aware', desc: 'Алгоритм балансировки по умолчанию' },
                { param: 'modelAffinity', value: true, desc: 'Привязка запросов к бэкенду с загруженной моделью' }
            ],
            jsonExample: '{\n  "balancing": {\n    "algorithm": "resource-aware",\n    "modelAffinity": true,\n    "sessionStickiness": true\n  }\n}'
        },
        replication: {
            icon: '📋',
            titleKey: 'settings.mode.replication',
            descKey: 'settings.mode.replication_long',
            fields: [
                { id: 'modelReplicationMinInstances', label: 'Min Instances', type: 'number', default: 1, min: 0, help: 'Минимальное количество реплик модели на бэкендах. Защищает от потери доступа при падении одного узла.' },
                { id: 'modelReplicationMaxInstances', label: 'Max Instances', type: 'number', default: 3, min: 1, help: 'Максимальное количество реплик модели. Ограничивает избыточное потребление VRAM.' },
                { id: 'modelReplicationIdleUnload', label: 'Idle Unload', type: 'text', default: '10m', help: 'Выгружать модель после простоя (формат: 30s, 5m, 1h, 2h30m)' }
            ],
            paramDocs: {
                defaultMinInstances: { desc: 'Минимум экземпляров модели в кластере', values: '0..10' },
                defaultMaxInstances: { desc: 'Максимум экземпляров модели в кластере', values: '1..20' },
                idleUnloadAfter: { desc: 'Время простоя перед выгрузкой модели', values: '30s | 5m | 1h' }
            },
            examples: [
                { param: 'minInstances', value: 2, desc: 'Каждая модель будет загружена минимум на 2 бэкенда' },
                { param: 'idleUnloadAfter', value: '10m', desc: 'Модель выгружается после 10 минут без запросов' }
            ],
            jsonExample: '{\n  "balancing": {\n    "modelReplication": {\n      "enabled": true,\n      "defaultMinInstances": 2,\n      "defaultMaxInstances": 4,\n      "idleUnloadAfter": "10m"\n    }\n  }\n}'
        },
        rpc_coordinator: {
            icon: '🌐',
            titleKey: 'settings.mode.rpc_coordinator',
            descKey: 'settings.mode.rpc_coordinator_long',
            fields: [
                { id: 'rpcCoordinatorURL', label: 'Coordinator URL', type: 'text', default: '', placeholder: 'http://coordinator.internal:8080', help: 'URL внешнего RPC-координатора. Балансер перенаправляет запросы на этот сервис.' },
                { id: 'rpcCoordinatorWorkerPort', label: 'Worker Port', type: 'number', default: 18050, min: 1024, max: 65535, help: 'Порт worker\'а на бэкенде. Координатор подключается к бэкендам через этот порт.' },
                { id: 'rpcCoordinatorTimeout', label: 'Timeout', type: 'text', default: '30s', help: 'Таймаут RPC-запроса. Если координатор не ответит за это время — fallback на стандартную балансировку.' }
            ],
            paramDocs: {
                coordinatorURL: { desc: 'Адрес внешнего RPC-координатора', values: 'http://host:port | https://host:port' },
                workerPort: { desc: 'Порт RPC-worker на каждом бэкенде', values: '1024..65535' },
                timeout: { desc: 'Таймаут запроса к координатору', values: '10s | 30s | 1m' }
            },
            examples: [
                { param: 'coordinatorURL', value: 'http://coordinator:8080', desc: 'Адрес внешнего RPC-сервиса' },
                { param: 'workerPort', value: 18050, desc: 'Порт для подключения worker\'ов на бэкендах' }
            ],
            jsonExample: '{\n  "balancing": {\n    "rpcCoordinator": {\n      "enabled": true,\n      "coordinatorURL": "http://coordinator.internal:8080",\n      "workerPort": 18050,\n      "protocol": "http",\n      "timeout": "30s"\n    }\n  }\n}'
        },
        virtual_router: {
            icon: '🧩',
            titleKey: 'settings.mode.virtual_router',
            descKey: 'settings.mode.virtual_router_long',
            fields: [
                { id: 'virtualModelsCoordMode', label: 'Coord Mode', type: 'select', default: 'sequential', options: ['sequential', 'parallel', 'tree'], help: 'Режим координации pipeline. Sequential — срезы по очереди; Parallel — одновременно; Tree — иерархическое дерево.' },
                { id: 'virtualModelsTimeout', label: 'Timeout (ms)', type: 'number', default: 30000, min: 1000, help: 'Таймаут координации в миллисекундах. Если хотя бы один срез не ответит — запрос считается failed.' }
            ],
            paramDocs: {
                coordMode: { desc: 'Режим обработки срезов виртуальной модели', values: 'sequential | parallel | tree' },
                timeoutMs: { desc: 'Максимальное время ожидания ответа от всех срезов', values: '1000..300000' }
            },
            examples: [
                { param: 'coordMode', value: 'sequential', desc: 'Срезы модели обрабатываются последовательно' },
                { param: 'timeoutMs', value: 30000, desc: 'Максимальное время ожидания ответа от всех срезов' }
            ],
            jsonExample: '{\n  "balancing": {\n    "virtualModels": {\n      "enabled": true,\n      "models": [\n        {\n          "name": "composite-70b",\n          "coordination": {\n            "mode": "sequential",\n            "timeoutMs": 30000\n          }\n        }\n      ]\n    }\n  }\n}'
        },
        distributed_inference: {
            icon: '🔬',
            titleKey: 'settings.mode.distributed',
            descKey: 'settings.mode.distributed_long',
            fields: [
                { id: 'distInferenceGrpcPort', label: 'gRPC Port', type: 'number', default: 19000, min: 1024, max: 65535, help: 'Порт gRPC для worker\'ов распределённого инференса. Все worker\'ы подключаются к этому порту для обмена KV-cache и промежуточными активациями.' }
            ],
            paramDocs: {
                grpcPort: { desc: 'Порт gRPC-сервера для связи worker\'ов', values: '1024..65535' }
            },
            examples: [
                { param: 'grpcPort', value: 19000, desc: 'Порт для gRPC-соединений между воркерами' }
            ],
            jsonExample: '{\n  "balancing": {\n    "distInference": {\n      "enabled": true,\n      "grpcPort": 19000,\n      "workers": [\n        { "workerId": "node-1", "host": "192.168.1.10", "layerRange": "1-20" },\n        { "workerId": "node-2", "host": "192.168.1.11", "layerRange": "21-40" }\n      ]\n    }\n  }\n}'
        }
    };

    let wizardModal = null;
    let selectedMode = null;
    let originalMode = null;
    let isApplying = false; // блокировка параллельных вызовов
    let lastAppliedMode = null; // фактически применённый режим (для отслеживания изменений)

    // ---- Public API ----

    function init() {
        // Запоминаем текущий применённый режим
        lastAppliedMode = getCurrentModeFromUI();

        // Перехватываем change на radio-кнопках operatingMode
        var radios = document.querySelectorAll('input[name="operatingMode"]');
        radios.forEach(function (radio) {
            radio.addEventListener('change', function (e) {
                if (!this.checked) return;
                var newMode = this.value;
                // Сравниваем с фактически применённым режимом, а не с DOM
                if (lastAppliedMode && lastAppliedMode !== newMode) {
                    e.preventDefault();
                    // Сбрасываем выбор, показываем мастер
                    var oldRadio = document.querySelector('input[name="operatingMode"][value="' + lastAppliedMode + '"]');
                    if (oldRadio) oldRadio.checked = true;
                    openWizard(newMode, lastAppliedMode);
                }
            });
        });
    }

    function getCurrentModeFromUI() {
        var checked = document.querySelector('input[name="operatingMode"]:checked');
        return checked ? checked.value : 'standard';
    }

    function openWizard(targetMode, currentMode) {
        selectedMode = targetMode;
        originalMode = currentMode;

        if (wizardModal) {
            wizardModal.remove();
        }

        wizardModal = document.createElement('div');
        wizardModal.className = 'modal active';
        wizardModal.id = 'modeWizardModal';
        wizardModal.style.display = 'flex';
        wizardModal.style.zIndex = '2000';

        wizardModal.innerHTML = buildWizardHTML(targetMode, currentMode);
        document.body.appendChild(wizardModal);

        bindWizardEvents(targetMode, currentMode);
    }

    function buildWizardHTML(targetMode, currentMode) {
        var info = MODE_INFO[targetMode] || MODE_INFO.standard;
        var t = function (key) { return window.I18N ? I18N.t(key) : key; };

        var title = t(info.titleKey);
        var desc = t(info.descKey);

        // Шаг 1: Справка по параметрам (paramDocs)
        var paramDocsHtml = '';
        if (info.paramDocs && Object.keys(info.paramDocs).length > 0) {
            paramDocsHtml = '<div class="wizard-param-docs"><h4>' + (t('wizard.param_docs_title') || 'Справка по параметрам') + '</h4>';
            paramDocsHtml += '<table class="wizard-param-table">';
            paramDocsHtml += '<thead><tr><th>' + (t('wizard.param_name') || 'Параметр') + '</th><th>' + (t('wizard.param_desc') || 'Описание') + '</th><th>' + (t('wizard.param_values') || 'Возможные значения') + '</th></tr></thead><tbody>';
            Object.keys(info.paramDocs).forEach(function (paramKey) {
                var pd = info.paramDocs[paramKey];
                paramDocsHtml += '<tr><td><code>' + paramKey + '</code></td><td>' + pd.desc + '</td><td><code>' + pd.values + '</code></td></tr>';
            });
            paramDocsHtml += '</tbody></table></div>';
        }

        // Шаг 2: Поля конфигурации
        var fieldsHtml = '';
        if (info.fields && info.fields.length > 0) {
            fieldsHtml = '<div class="wizard-fields">';
            info.fields.forEach(function (f) {
                var help = f.help || '';
                var value = getFieldCurrentValue(f.id, f.default);
                if (f.type === 'select') {
                    var optionsHtml = '';
                    (f.options || []).forEach(function (opt) {
                        optionsHtml += '<option value="' + opt + '"' + (opt === value ? ' selected' : '') + '>' + opt + '</option>';
                    });
                    fieldsHtml += '<div class="fg"><label>' + f.label + '</label><select id="wizard_' + f.id + '" class="fc" data-help="' + help + '">' + optionsHtml + '</select><small class="field-help">' + help + '</small></div>';
                } else {
                    fieldsHtml += '<div class="fg"><label>' + f.label + '</label><input type="' + f.type + '" id="wizard_' + f.id + '" class="fc" value="' + value + '" placeholder="' + (f.placeholder || '') + '"' + (f.min !== undefined ? ' min="' + f.min + '"' : '') + (f.max !== undefined ? ' max="' + f.max + '"' : '') + '><small class="field-help">' + help + '</small></div>';
                }
            });
            fieldsHtml += '</div>';
        } else {
            fieldsHtml = '<p style="color:var(--text-muted);padding:12px 0;">' + t('wizard.summary_empty') + '</p>';
        }

        // Шаг 3: Примеры конфигурации
        var examplesHtml = '';
        if (info.examples && info.examples.length > 0) {
            examplesHtml = '<div class="wizard-examples"><h4>' + (t('wizard.examples_title') || 'Примеры конфигурации') + '</h4>';
            info.examples.forEach(function (ex) {
                examplesHtml += '<div class="wizard-example"><code>' + ex.param + ' = ' + ex.value + '</code><span>— ' + ex.desc + '</span></div>';
            });
            examplesHtml += '</div>';
        }

        // Шаг 4: JSON-пример
        var jsonExampleHtml = '';
        if (info.jsonExample) {
            jsonExampleHtml = '<div class="wizard-json-example"><h4>' + (t('wizard.json_example_title') || 'Пример конфигурации (JSON)') + '</h4>';
            jsonExampleHtml += '<pre style="background:#1e1e2e;color:#cdd6f4;padding:12px;border-radius:8px;font-size:12px;overflow-x:auto;">' + escapeHtml(info.jsonExample) + '</pre></div>';
        }

        // Шаг 5: Превью изменений
        var previewHtml = '<div class="wizard-preview"><h4>' + (t('wizard.preview_title') || 'Превью изменений') + '</h4>';
        previewHtml += '<div class="preview-change"><span class="preview-label">Mode:</span><span class="preview-old">' + currentMode + '</span><span class="preview-arrow">→</span><span class="preview-new-val">' + targetMode + '</span></div>';
        previewHtml += '</div>';

        var html = '<div class="modal-content" style="max-width:700px;max-height:90vh;overflow-y:auto;">';
        html += '<div class="modal-header"><h3>' + info.icon + ' ' + title + '</h3><button class="modal-close" id="wizardClose">&times;</button></div>';
        html += '<div class="modal-body">';
        html += '<div class="wizard-desc">' + desc + '</div>';
        html += paramDocsHtml;
        html += '<div class="wizard-section"><h4>' + (t('wizard.params_title') || 'Параметры режима') + '</h4>' + fieldsHtml + '</div>';
        html += examplesHtml;
        html += jsonExampleHtml;
        html += previewHtml;
        html += '</div>';
        html += '<div class="modal-footer">';
        html += '<button class="btn btn-secondary" id="wizardCancel">' + (t('common.cancel') || 'Отмена') + '</button>';
        html += '<button class="btn btn-primary" id="wizardApply">' + (t('wizard.apply') || 'Применить и сохранить') + '</button>';
        html += '</div></div>';

        return html;
    }

    function escapeHtml(text) {
        var div = document.createElement('div');
        div.textContent = text;
        return div.innerHTML;
    }

    function getFieldCurrentValue(id, fallback) {
        var el = document.getElementById(id);
        return el ? el.value : fallback;
    }

    function bindWizardEvents(targetMode, currentMode) {
        document.getElementById('wizardClose').addEventListener('click', closeWizard);
        document.getElementById('wizardCancel').addEventListener('click', closeWizard);
        wizardModal.addEventListener('click', function (e) {
            if (e.target === wizardModal) closeWizard();
        });

        document.getElementById('wizardApply').addEventListener('click', function () {
            applyModeChange(targetMode);
        });
    }

    function applyModeChange(targetMode) {
        if (isApplying) return; // блокировка параллельных вызовов
        isApplying = true;

        var info = MODE_INFO[targetMode] || MODE_INFO.standard;
        var config = {
            operatingMode: targetMode
        };

        // Собираем значения полей из мастера
        var validationErrors = [];
        if (info.fields) {
            info.fields.forEach(function (f) {
                var el = document.getElementById('wizard_' + f.id);
                if (!el) return;
                var val = el.value;
                if (f.type === 'number') {
                    val = parseInt(val) || f.default;
                    if (f.min !== undefined && val < f.min) {
                        validationErrors.push(f.label + ': минимум ' + f.min);
                    }
                    if (f.max !== undefined && val > f.max) {
                        validationErrors.push(f.label + ': максимум ' + f.max);
                    }
                }
                if (f.type === 'text' && f.id === 'modelReplicationIdleUnload') {
                    if (!/^\d+[smh]$/.test(val)) {
                        validationErrors.push(f.label + ': формат 30s, 5m, 1h');
                    }
                }
                if (f.type === 'text' && f.id === 'rpcCoordinatorTimeout') {
                    if (val && !/^\d+[smh]$/.test(val)) {
                        validationErrors.push(f.label + ': формат 30s, 5m, 1h');
                    }
                }
                if (f.id === 'rpcCoordinatorURL' && val) {
                    if (!/^https?:\/\/.+/.test(val)) {
                        validationErrors.push(f.label + ': должен начинаться с http:// или https://');
                    }
                }
                // Маппим поля мастера на ключи API
                switch (f.id) {
                    case 'modelReplicationMinInstances':
                        config.modelReplication = config.modelReplication || {};
                        config.modelReplication.defaultMinInstances = val;
                        break;
                    case 'modelReplicationMaxInstances':
                        config.modelReplication = config.modelReplication || {};
                        config.modelReplication.defaultMaxInstances = val;
                        break;
                    case 'modelReplicationIdleUnload':
                        config.modelReplication = config.modelReplication || {};
                        config.modelReplication.idleUnloadAfter = val;
                        break;
                    case 'rpcCoordinatorURL':
                        config.rpcCoordinator = config.rpcCoordinator || {};
                        config.rpcCoordinator.coordinatorURL = val;
                        break;
                    case 'rpcCoordinatorWorkerPort':
                        config.rpcCoordinator = config.rpcCoordinator || {};
                        config.rpcCoordinator.workerPort = val;
                        break;
                    case 'rpcCoordinatorTimeout':
                        config.rpcCoordinator = config.rpcCoordinator || {};
                        config.rpcCoordinator.timeout = val;
                        break;
                    case 'virtualModelsCoordMode':
                        config.virtualModels = config.virtualModels || {};
                        config.virtualModels.coordMode = val;
                        break;
                    case 'virtualModelsTimeout':
                        config.virtualModels = config.virtualModels || {};
                        config.virtualModels.timeout = val;
                        break;
                    case 'distInferenceGrpcPort':
                        config.distInference = config.distInference || {};
                        config.distInference.grpcPort = val;
                        break;
                }
            });
        }

        if (validationErrors.length > 0) {
            isApplying = false;
            showToast((window.I18N ? I18N.t('wizard.validation_error') : 'Ошибка валидации') + ': ' + validationErrors.join('; '), 'error');
            return;
        }

        // Показываем индикатор загрузки
        var applyBtn = document.getElementById('wizardApply');
        if (applyBtn) {
            applyBtn.disabled = true;
            applyBtn.textContent = (window.I18N ? I18N.t('wizard.applying') : 'Применение...');
        }

        // Отправляем PUT
        Api.updateConfig(config).then(function (resp) {
            closeWizard();
            isApplying = false;

            // Запоминаем успешно применённый режим
            lastAppliedMode = targetMode;

            // Обновляем UI: радио-кнопку, поля, секции
            updateUIFromResponse(resp && resp.config ? resp.config : config);

            showToast(window.I18N ? I18N.t('wizard.success') : 'Режим применён', 'success');

            // Принудительная синхронизация с сервером
            if (typeof loadSettings === 'function') {
                loadSettings();
            }

        }).catch(function (err) {
            isApplying = false;
            if (applyBtn) {
                applyBtn.disabled = false;
                applyBtn.textContent = (window.I18N ? I18N.t('wizard.apply') : 'Применить и сохранить');
            }
            showToast((window.I18N ? I18N.t('wizard.error') : 'Ошибка') + ': ' + (err.message || err), 'error');
        });
    }

    function updateUIFromResponse(config) {
        var mode = config.operatingMode || 'standard';

        // Запоминаем синхронизированный режим
        lastAppliedMode = mode;

        // Устанавливаем radio
        var radio = document.querySelector('input[name="operatingMode"][value="' + mode + '"]');
        if (radio) radio.checked = true;

        // Обновляем карточки и поля
        if (window.SettingsUI) {
            SettingsUI.updateModeCards(mode);
            SettingsUI.showModeFields(mode);
        }

        // Обновляем значения полей если они пришли в ответе
        if (config.modelReplication) {
            if (config.modelReplication.defaultMinInstances !== undefined) setField('modelReplicationMinInstances', config.modelReplication.defaultMinInstances);
            if (config.modelReplication.defaultMaxInstances !== undefined) setField('modelReplicationMaxInstances', config.modelReplication.defaultMaxInstances);
            if (config.modelReplication.idleUnloadAfter !== undefined) setField('modelReplicationIdleUnload', config.modelReplication.idleUnloadAfter);
        }
        if (config.rpcCoordinator) {
            if (config.rpcCoordinator.coordinatorURL !== undefined) setField('rpcCoordinatorURL', config.rpcCoordinator.coordinatorURL);
            if (config.rpcCoordinator.workerPort !== undefined) setField('rpcCoordinatorWorkerPort', config.rpcCoordinator.workerPort);
            if (config.rpcCoordinator.timeout !== undefined) setField('rpcCoordinatorTimeout', config.rpcCoordinator.timeout);
        }
        if (config.virtualModels) {
            if (config.virtualModels.coordMode !== undefined) setField('virtualModelsCoordMode', config.virtualModels.coordMode);
            if (config.virtualModels.timeout !== undefined) setField('virtualModelsTimeout', config.virtualModels.timeout);
        }
        if (config.distInference) {
            if (config.distInference.grpcPort !== undefined) setField('distInferenceGrpcPort', config.distInference.grpcPort);
        }

        // Обновляем видимость секций
        updateVariantSections(mode);
    }

    function setField(id, value) {
        var el = document.getElementById(id);
        if (el && value !== undefined && value !== null) {
            el.value = value;
        }
    }

    function closeWizard() {
        if (wizardModal) {
            wizardModal.remove();
            wizardModal = null;
        }
        selectedMode = null;
    }

    // ---- Adaptive UI: hide/show variant sections ----

    function updateVariantSections(mode) {
        // Скрываем ВСЕ variant-секции сначала
        document.querySelectorAll('.variant-section').forEach(function (sec) {
            sec.style.display = 'none';
        });

        // Показываем только релевантные
        if (mode === 'standard') {
            // Для стандартного режима не показываем специфичные секции
            return;
        }

        var variantMap = {
            replication: 'variant-a',
            rpc_coordinator: 'variant-b',
            virtual_router: 'variant-c',
            distributed_inference: 'variant-d'
        };

        var variantClass = variantMap[mode];
        if (variantClass) {
            document.querySelectorAll('.' + variantClass).forEach(function (sec) {
                sec.style.display = '';
            });
        }
    }

    // ---- Public API ----

    window.ModeWizard = {
        init: init,
        open: openWizard,
        close: closeWizard,
        updateVariantSections: updateVariantSections,
        MODE_INFO: MODE_INFO
    };

    // Auto-init on DOM ready
    if (document.readyState === 'complete' || document.readyState === 'interactive') {
        init();
    } else {
        document.addEventListener('DOMContentLoaded', init);
    }

})();
/**
 * app-modals.js — Backend CRUD modal management.
 *
 * R57.4 (2026-09-03): extracted from webui/js/app.js.
 *
 * Этот файл содержит:
 *   - openBackendModal — открыть модалку для add/edit бэкенда
 *   - fillForm — заполнить форму данными бэкенда (или пустую)
 *   - closeModal — закрыть модалку
 *   - saveBackend — async сохранение (POST/PUT + auto-detect cppworker port)
 *   - deleteBackend — async удаление (DELETE)
 *   - editBackend / confirmDeleteBackend — обёртки для onclick'ов
 *
 * Загружается ПОСЛЕ app-core.js и app-listeners.js, ДО app.js
 * (см. webui/index.html).
 *
 * Контракт:
 *   - window.App.setupBackendsCRUD() — вызывается из app.js init()
 *     через App.context, populate App.context.backendsCRUD с функциями
 *     openBackendModal / closeModal / editBackend / confirmDeleteBackend
 *     для onclick handlers в renderers.js
 *
 * Зависимости (предполагаются уже загруженными):
 *   - window.I18N.t (i18n/index.js)
 *   - window.Api.{getBackend, createBackend, updateBackend, updateBackendLimitsFull, deleteBackend} (api.js)
 *   - window.BackendTypeFilter (modules/backend-type-filter.js)
 *   - window.Renderers.* (modules/renderers.js)
 *   - App.context.{data, addLog, refreshCurrentPage, autoDetectCppWorkerPort} (set by app.js init())
 *   - App.showToast (app-core.js)
 */
(function() {
    'use strict';

    const App = (window.App = window.App || {});

    // ---- Internal helpers ----

    function _t(key, fallback) {
        return window.I18N ? I18N.t(key) : fallback;
    }

    function _getInput(id, defValue) {
        const el = document.getElementById(id);
        if (!el) return defValue;
        if (el.tagName === 'INPUT' && el.type === 'number') {
            return parseInt(el.value) || defValue;
        }
        if (el.tagName === 'INPUT' && el.type === 'checkbox') {
            return el.checked;
        }
        if (defValue === undefined) {
            return el.value;
        }
        return el.value || defValue;
    }

    function _getInt(id, defValue) {
        const el = document.getElementById(id);
        if (!el) return defValue;
        return parseInt(el.value) || defValue;
    }

    // ---- Public functions ----

    /**
     * openBackendModal — открыть модалку для add или edit бэкенда.
     * Если backendId === null → режим добавления, иначе режим edit.
     * Edit: подгружает конфиг через Api.getBackend и заполняет
     * runtime-лимиты (weight, maxConcurrent, maxModels, gpuMode).
     */
    function openBackendModal(backendId) {
        const ctx = App.context;
        const modal = document.getElementById('backendModal');
        const title = document.getElementById('modalTitle');
        const deleteBtn = document.getElementById('modalDelete');

        if (backendId) {
            const backend = ctx.data.backends.find(b => b.id === backendId);
            if (!backend) return;
            title.textContent = _t('backends.edit', 'Edit Backend');
            deleteBtn.style.display = 'inline-block';
            fillForm(backend, true);
            Api.getBackend(backendId).then(function (config) {
                if (config && config.weight !== undefined && config.weight !== null) {
                    document.getElementById('formBackendWeight').value = config.weight;
                }
                if (config && config.maxConcurrentRequests !== undefined && config.maxConcurrentRequests !== null) {
                    document.getElementById('formBackendMaxConcurrent').value = config.maxConcurrentRequests;
                }
                if (config && config.maxConcurrentReqs !== undefined && config.maxConcurrentReqs !== null) {
                    document.getElementById('formBackendMaxConcurrent').value = config.maxConcurrentReqs;
                }
                if (config && (config.maxModels !== undefined && config.maxModels !== null && config.maxModels !== 0)) {
                    document.getElementById('formBackendMaxModels').value = config.maxModels;
                }
                if (config && config.runtimeMaxModels !== undefined && config.runtimeMaxModels !== null && config.runtimeMaxModels !== 0) {
                    document.getElementById('formBackendMaxModels').value = config.runtimeMaxModels;
                }
                if (config && (config.runtimeMaxConcurrentRequests !== undefined && config.runtimeMaxConcurrentRequests !== null && config.runtimeMaxConcurrentRequests !== 0)) {
                    document.getElementById('formBackendMaxConcurrent').value = config.runtimeMaxConcurrentRequests;
                }
                if (config && config.gpuMode) {
                    var modeEl = document.getElementById('formBackendGpuMode');
                    if (modeEl) modeEl.value = config.gpuMode;
                }
            }).catch(function () {
                // Не фатально — данные уже загружены из кластера
            });
        } else {
            title.textContent = _t('backends.add', 'Add Backend');
            deleteBtn.style.display = 'none';
            fillForm(null, false);
        }

        modal.classList.add('active');
    }

    /**
     * fillForm — заполнить форму данными бэкенда (или пустыми значениями).
     * Применяет BackendTypeFilter для условного показа полей.
     */
    function fillForm(backend, isEdit) {
        const id = (backend && backend.id) || '';
        document.getElementById('formBackendId').value = id;
        document.getElementById('formBackendId').disabled = isEdit;
        document.getElementById('formBackendName').value = (backend && backend.name) || '';
        document.getElementById('formBackendHost').value = (backend && backend.host) || '';
        document.getElementById('formBackendOllamaPort').value = (backend && backend.ollamaPort) || 11434;
        document.getElementById('formBackendAgentPort').value = (backend && backend.agentPort) || 18032;
        document.getElementById('formBackendWeight').value = (backend && backend.weight) || 1;
        document.getElementById('formBackendMaxConcurrent').value =
            (backend && (backend.maxConcurrentRequests || backend.maxConcurrentReqs)) || 10;
        document.getElementById('formBackendMaxModels').value =
            (backend && (backend.maxModels || backend.runtimeMaxModels)) || 0;
        document.getElementById('formBackendLabels').value = ((backend && backend.labels) || []).join(',');

        // Синхронизируем селектор типа бэкенда с BackendTypeFilter
        const formBackendType = document.getElementById('formBackendType');
        if (formBackendType) {
            var currentType = 'ollama';
            if (window.BackendTypeFilter) {
                currentType = BackendTypeFilter.getCurrentType();
            }
            if (backend && backend.type) {
                currentType = backend.type === 'llama_cpp' ? 'llama_cpp' : 'ollama';
            }
            formBackendType.value = currentType;
            if (window.BackendTypeFilter) {
                BackendTypeFilter.toggleBackendFormFields(currentType);
            }
        }

        // GPU Mode — из capacity.mode или platformMode
        var gpuMode = (backend && (backend.gpuMode || backend.platformMode)) || 'auto';
        if (backend && backend.ollama && backend.ollama.backendCapacity && backend.ollama.backendCapacity.mode) {
            gpuMode = backend.ollama.backendCapacity.mode;
        }
        var modeEl = document.getElementById('formBackendGpuMode');
        if (modeEl) {
            modeEl.value = gpuMode;
        }
    }

    function closeModal() {
        document.getElementById('backendModal').classList.remove('active');
    }

    /**
     * saveBackend — async сохранение бэкенда.
     * POST (add) или PUT (edit) + дополнительно PUT /limits для runtime.
     * Для llama.cpp бэкенда — auto-detect cppWorkerPort если поле пустое.
     */
    async function saveBackend() {
        const ctx = App.context;
        const id = _getInput('formBackendId', '').trim();
        const name = _getInput('formBackendName', '').trim();
        const host = _getInput('formBackendHost', '').trim();
        const ollamaPort = _getInt('formBackendOllamaPort', 11434);
        const agentPort = _getInt('formBackendAgentPort', 18032);
        const weight = parseFloat(_getInput('formBackendWeight', '1')) || 1;
        const maxConcurrent = _getInt('formBackendMaxConcurrent', 10);
        const maxModels = _getInt('formBackendMaxModels', 0);
        const gpuMode = _getInput('formBackendGpuMode', 'auto');
        const labels = _getInput('formBackendLabels', '')
            .split(',').map(l => l.trim()).filter(Boolean);

        if (!id || !host) {
            App.showToast(_t('common.error', 'ID and host are required'), 'error');
            return;
        }

        var backendType = _getInput('formBackendType', 'ollama');

        // cppWorker port:
        //   1. Берём значение из формы, если оно заполнено и > 0
        //   2. Иначе пробуем auto-detect (probe 18092/18091/18093/18090 на /info)
        //   3. Fallback: 18092 (актуальный default для современных llama.cpp-инсталляций).
        var cppWorkerPortInput = _getInt('formBackendCppWorkerPort', 0);
        var cppGrpcPort = _getInt('formBackendCppGrpcPort', 19000);

        var payload = {
            id: id, name: name || id, host: host,
            ollamaPort: ollamaPort, agentPort: agentPort,
            weight: weight, maxConcurrentRequests: maxConcurrent, maxModels: maxModels,
            gpuMode: gpuMode, labels: labels, backendType: backendType
        };

        // Для llama.cpp-бэкенда гарантируем корректный cppWorkerPort.
        if (backendType === 'llama_cpp') {
            payload.cppWorkerPort = cppWorkerPortInput > 0 ? cppWorkerPortInput : 18092;
            payload.grpcPort = cppGrpcPort;

            if (cppWorkerPortInput <= 0) {
                if (ctx.autoDetectCppWorkerPort) {
                    ctx.autoDetectCppWorkerPort(host).then(function (detected) {
                        if (detected && detected !== payload.cppWorkerPort) {
                            payload.cppWorkerPort = detected;
                            Api.updateBackend(id, { cppWorkerPort: detected }).then(function () {
                                if (window.console && console.log) {
                                    console.log('[app-modals] auto-detected cppworker port:', detected, 'for backend', id);
                                }
                            }).catch(function (err) {
                                if (window.console && console.warn) {
                                    console.warn('[app-modals] failed to update cppworker port:', err);
                                }
                            });
                        }
                    }).catch(function () { /* ignore */ });
                }
            }
        }
        const isEdit = document.getElementById('formBackendId').disabled;

        try {
            if (isEdit) {
                await Api.updateBackend(id, payload);
                try {
                    await Api.updateBackendLimitsFull(id, maxConcurrent, maxModels);
                } catch (limitsErr) {
                    if (ctx.addLog) {
                        ctx.addLog(_t('app.backend_limits_warn', { id: id }) ||
                            'Warning: runtime limits for ' + id + ' not updated', 'warn');
                    }
                }
                App.showToast(_t('app.backend_saved', 'Backend updated'), 'success');
                if (ctx.addLog) {
                    ctx.addLog(_t('app.backend_updated', { id: id }) || 'Backend ' + id + ' updated', 'info');
                }
            } else {
                await Api.createBackend(payload);
                App.showToast(_t('app.backend_created', 'Backend added'), 'success');
                if (ctx.addLog) {
                    ctx.addLog(_t('app.backend_created', { id: id }) || 'Backend ' + id + ' added', 'info');
                }
            }
            closeModal();
            if (ctx.refreshCurrentPage) ctx.refreshCurrentPage();
        } catch (e) {
            App.showToast((_t('common.error', 'Ошибка')) + ': ' + (e.message || e), 'error');
        }
    }

    /**
     * deleteBackend — async удаление через DELETE.
     * Подтверждение через confirm() — после R57.5 будет заменён на inline modal.
     */
    async function deleteBackend() {
        const ctx = App.context;
        const id = document.getElementById('formBackendId').value;
        if (!confirm(_t('backends.confirm_delete', { name: id }) || ('Delete backend ' + id + '?'))) return;

        try {
            await Api.deleteBackend(id);
            closeModal();
            App.showToast(_t('app.backend_deleted', 'Backend deleted'), 'success');
            if (ctx.addLog) {
                ctx.addLog(_t('app.backend_deleted', { id: id }) || ('Backend ' + id + ' deleted'), 'info');
            }
            if (ctx.refreshCurrentPage) ctx.refreshCurrentPage();
        } catch (e) {
            App.showToast((_t('common.error', 'Ошибка')) + ': ' + (e.message || e), 'error');
        }
    }

    /** Обёртки для onclick handlers в renderers.js */
    function editBackend(id) {
        openBackendModal(id);
    }

    function confirmDeleteBackend(id) {
        openBackendModal(id);
    }

    /**
     * Expose to window.BackendCRUD so app.js's public API (window.ui) can
     * reference these directly at IIFE return time. This is set BEFORE
     * app.js's IIFE runs, so the references are valid when window.ui is built.
     */
    window.BackendCRUD = {
        openBackendModal: openBackendModal,
        fillForm: fillForm,
        closeModal: closeModal,
        saveBackend: saveBackend,
        deleteBackend: deleteBackend,
        editBackend: editBackend,
        confirmDeleteBackend: confirmDeleteBackend
    };
})();

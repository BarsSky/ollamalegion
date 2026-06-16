/**
 * cppworker-params.js — Per-Model Profile manager для настроек llama.cpp
 * OllamaLegion WebUI (Шаг 5 cppworker-preflight-nctx-session)
 *
 * Возможности:
 *  - GET    /api/v1/cppworker/model-profiles         — список всех профилей
 *  - GET    /api/v1/cppworker/model-profiles/{name}  — профиль одной модели
 *  - PUT    /api/v1/cppworker/model-profiles/{name}  — создать/обновить
 *  - DELETE /api/v1/cppworker/model-profiles/{name}  — удалить
 *  - POST   /api/v1/cppworker/model-profiles/{name}/apply — save + reload
 *
 * UI:
 *  - список профилей с метаданными (modelName, contextLength, notes)
 *  - wizard с пресетами 4K/8K/16K/32K/64K/128K/256K/Custom
 *  - slider 256..262144 (для gemma-4 — 256K)
 *  - reload progress modal с шагами и результатом по бэкендам
 *
 * Использует:
 *   - window.Api.request — для HTTP
 *   - window.I18N.t — для переводов (fallback на ru-текст)
 *   - window.Utils — для escape (если есть)
 *   - window.showToast — для уведомлений (если есть, иначе alert)
 */
(function () {
    'use strict';

    const API = window.Api;
    const I18N = window.I18N || { t: (k, def) => def || k };

    // Пресеты contextLength (в токенах)
    const CTX_PRESETS = [
        { label: '4K',   value: 4096 },
        { label: '8K',   value: 8192 },
        { label: '16K',  value: 16384 },
        { label: '32K',  value: 32768 },
        { label: '64K',  value: 65536 },
        { label: '128K', value: 131072 },
        { label: '256K', value: 262144 },
        { label: 'Custom', value: 0 }
    ];
    const CTX_MIN = 256;
    const CTX_MAX = 262144; // 256K — для gemma-4

    /**
     * Загрузить список профилей и отрендерить в #cppProfilesList.
     */
    async function loadAndRender() {
        const listEl = document.getElementById('cppProfilesList');
        if (!listEl) return;

        listEl.innerHTML = '<div class="loading">' + (I18N.t('common.loading', 'Загрузка...')) + '</div>';

        try {
            const data = await API.request('/api/v1/cppworker/model-profiles');
            renderProfiles(data.models || {}, listEl);
        } catch (e) {
            listEl.innerHTML = '<div class="cpp-profiles-empty">' + escapeHtml(e.message || String(e)) + '</div>';
        }
    }

    function renderProfiles(models, container) {
        const keys = Object.keys(models).sort();
        if (keys.length === 0) {
            container.innerHTML = '<div class="cpp-profiles-empty" data-i18n="settings.profiles.empty">' +
                escapeHtml(I18N.t('settings.profiles.empty', 'Нет настроенных профилей. Создайте первый, чтобы переопределить n_ctx для конкретной модели.')) +
                '</div>';
            return;
        }
        const html = keys.map(name => renderProfileItem(name, models[name])).join('');
        container.innerHTML = html;

        // Привязываем handlers
        container.querySelectorAll('[data-action]').forEach(btn => {
            btn.addEventListener('click', onProfileAction);
        });
    }

    function renderProfileItem(name, profile) {
        const ctxFormatted = formatCtx(profile.contextLength);
        const meta = [];
        meta.push('n_ctx=' + ctxFormatted);
        if (profile.batchSize && profile.batchSize > 0) meta.push('batch=' + profile.batchSize);
        if (profile.numGpuLayers !== 0) meta.push('gpuLayers=' + (profile.numGpuLayers === -1 ? 'all' : profile.numGpuLayers));
        const notes = profile.notes ? ' • ' + escapeHtml(profile.notes) : '';
        return `
            <div class="cpp-profile-item" data-name="${escapeHtml(name)}">
                <div class="cpp-profile-info">
                    <div class="cpp-profile-name">
                        ${escapeHtml(name)}
                        <span class="badge">${ctxFormatted}</span>
                    </div>
                    <div class="cpp-profile-meta">${meta.join(' · ')}${notes}</div>
                </div>
                <div class="cpp-profile-actions">
                    <button class="btn btn-secondary" data-action="edit" data-name="${escapeHtml(name)}" title="Редактировать">
                        <i class="fas fa-pen"></i>
                    </button>
                    <button class="btn btn-secondary" data-action="apply" data-name="${escapeHtml(name)}" title="Применить (reload)">
                        <i class="fas fa-rotate"></i>
                    </button>
                    <button class="btn btn-danger" data-action="delete" data-name="${escapeHtml(name)}" title="Удалить">
                        <i class="fas fa-trash"></i>
                    </button>
                </div>
            </div>
        `;
    }

    function formatCtx(n) {
        if (n >= 1024) {
            const k = n / 1024;
            return (k % 1 === 0) ? (k + 'K') : (k.toFixed(1) + 'K');
        }
        return String(n);
    }

    async function onProfileAction(e) {
        const btn = e.currentTarget;
        const action = btn.dataset.action;
        const name = btn.dataset.name;
        if (!action || !name) return;

        try {
            if (action === 'edit') {
                const data = await API.request('/api/v1/cppworker/model-profiles/' + encodeURIComponent(name));
                openWizard(data.model, data.profile);
            } else if (action === 'delete') {
                if (!confirm(I18N.t('settings.profiles.confirm_delete', 'Удалить профиль для') + ' "' + name + '"?')) {
                    return;
                }
                await API.request('/api/v1/cppworker/model-profiles/' + encodeURIComponent(name), { method: 'DELETE' });
                showToast('success', I18N.t('settings.profiles.deleted', 'Профиль удалён'));
                await loadAndRender();
            } else if (action === 'apply') {
                await applyProfileWithProgress(name);
            }
        } catch (err) {
            showToast('error', err.message || String(err));
        }
    }

    // ----- Wizard -----

    function openWizard(modelName, profile) {
        const isNew = !profile;
        const initialCtx = (profile && profile.contextLength) || 16384;
        const initialBatch = (profile && profile.batchSize) || 0;
        const initialLayers = (profile && profile.numGpuLayers) || 0;
        const initialFlash = (profile && profile.flashAttn) || false;
        const initialNuma = (profile && profile.numa) || false;
        const initialMmap = (profile && profile.useMmap);
        const initialNotes = (profile && profile.notes) || '';

        const overlay = document.createElement('div');
        overlay.className = 'mode-wizard-overlay cpp-profile-wizard';
        overlay.innerHTML = `
            <div class="mode-wizard-modal">
                <div class="wizard-header">
                    <h3>${isNew ? I18N.t('settings.profiles.add', 'Добавить профиль…') : I18N.t('settings.profiles.edit', 'Редактировать профиль')}</h3>
                </div>
                <div class="wizard-desc">
                    ${escapeHtml(I18N.t('settings.profiles.wizard_desc', 'Per-model profile переопределяет n_ctx для конкретной GGUF-модели. Применяется в 3-tier резолвере между body и per-backend default. После сохранения можно применить (reload) на бэкенды через кнопку «Применить» в списке.'))}
                </div>
                <div class="wizard-fields">
                    <div class="wizard-field">
                        <label>${escapeHtml(I18N.t('settings.profiles.model_name', 'Имя модели'))} *</label>
                        <input type="text" id="wizModelName" value="${escapeHtml(modelName || '')}" ${isNew ? '' : 'readonly'} placeholder="gemma-4-E4B-it-Q4_K_M">
                    </div>
                    <div class="wizard-field">
                        <label>${escapeHtml(I18N.t('settings.profiles.n_ctx', 'Context Length (n_ctx)'))} *</label>
                        <div class="ctx-presets" id="wizPresets">
                            ${CTX_PRESETS.map(p => `<button type="button" class="ctx-preset" data-value="${p.value}">${p.label}</button>`).join('')}
                        </div>
                        <div class="ctx-slider-row">
                            <input type="range" id="wizCtxSlider" min="${CTX_MIN}" max="${CTX_MAX}" step="256" value="${initialCtx}">
                            <span class="ctx-value" id="wizCtxValue">${formatCtx(initialCtx)}</span>
                        </div>
                        <div class="ctx-help">${escapeHtml(I18N.t('settings.profiles.ctx_help', 'От 256 до 262144 (256K). 256K = gemma-4 max context.'))}</div>
                    </div>
                    <div class="wizard-field">
                        <label>${escapeHtml(I18N.t('settings.profiles.batch_size', 'Batch Size (опционально)'))}</label>
                        <input type="number" id="wizBatchSize" value="${initialBatch}" min="1" max="4096" placeholder="0 = не задано">
                    </div>
                    <div class="wizard-field">
                        <label>${escapeHtml(I18N.t('settings.profiles.num_gpu_layers', 'Num GPU Layers (опционально)'))}</label>
                        <input type="number" id="wizNumGpuLayers" value="${initialLayers}" min="-1" max="200" placeholder="0 = не задано, -1 = все слои">
                    </div>
                    <div class="wizard-field">
                        <label>${escapeHtml(I18N.t('settings.profiles.notes', 'Заметки'))}</label>
                        <input type="text" id="wizNotes" value="${escapeHtml(initialNotes)}" placeholder="Например: 256K для gemma-4">
                    </div>
                </div>
                <div class="wizard-footer">
                    <button class="btn btn-secondary" id="wizCancel">${escapeHtml(I18N.t('common.cancel', 'Отмена'))}</button>
                    <button class="btn btn-primary" id="wizSave">${escapeHtml(I18N.t('common.save', 'Сохранить'))}</button>
                </div>
            </div>
        `;
        document.body.appendChild(overlay);
        // Закрытие по клику на оверлей
        overlay.addEventListener('click', e => {
            if (e.target === overlay) closeWizard();
        });

        // Элементы
        const modelNameInput = overlay.querySelector('#wizModelName');
        const slider = overlay.querySelector('#wizCtxSlider');
        const valueEl = overlay.querySelector('#wizCtxValue');
        const presets = overlay.querySelectorAll('.ctx-preset');

        function updatePresets() {
            const v = parseInt(slider.value, 10);
            presets.forEach(btn => {
                const pv = parseInt(btn.dataset.value, 10);
                btn.classList.toggle('active', pv === v);
            });
            valueEl.textContent = formatCtx(v);
        }
        slider.addEventListener('input', updatePresets);
        presets.forEach(btn => {
            btn.addEventListener('click', () => {
                const v = parseInt(btn.dataset.value, 10);
                if (v === 0) {
                    // Custom — фокус на slider, ничего не делаем
                    slider.focus();
                } else {
                    slider.value = v;
                    updatePresets();
                }
            });
        });
        updatePresets();

        // Handlers кнопок
        overlay.querySelector('#wizCancel').addEventListener('click', closeWizard);
        overlay.querySelector('#wizSave').addEventListener('click', () => onWizardSave(overlay, isNew));

        // Сохранение по Enter
        overlay.addEventListener('keydown', e => {
            if (e.key === 'Enter' && e.target.tagName !== 'BUTTON') {
                e.preventDefault();
                onWizardSave(overlay, isNew);
            }
        });

        // Фокус на первое поле
        if (isNew) {
            modelNameInput.focus();
        } else {
            slider.focus();
        }
    }

    function closeWizard() {
        const overlay = document.querySelector('.cpp-profile-wizard');
        if (overlay) overlay.remove();
    }

    async function onWizardSave(overlay, isNew) {
        const name = (overlay.querySelector('#wizModelName').value || '').trim();
        const ctxSize = parseInt(overlay.querySelector('#wizCtxSlider').value, 10);
        const batchSize = parseInt(overlay.querySelector('#wizBatchSize').value, 10) || 0;
        const numGpuLayers = parseInt(overlay.querySelector('#wizNumGpuLayers').value, 10) || 0;
        const notes = (overlay.querySelector('#wizNotes').value || '').trim();

        if (!name) {
            showToast('error', I18N.t('settings.profiles.model_name_required', 'Имя модели обязательно'));
            return;
        }
        if (!ctxSize || ctxSize < CTX_MIN || ctxSize > CTX_MAX) {
            showToast('error', 'n_ctx должен быть в [' + CTX_MIN + ', ' + CTX_MAX + ']');
            return;
        }

        const body = {
            contextLength: ctxSize,
            batchSize: batchSize,
            numGpuLayers: numGpuLayers,
            notes: notes
        };
        try {
            await API.request('/api/v1/cppworker/model-profiles/' + encodeURIComponent(name), {
                method: 'PUT',
                body: JSON.stringify(body)
            });
            showToast('success', isNew ? I18N.t('settings.profiles.created', 'Профиль создан') : I18N.t('settings.profiles.updated', 'Профиль обновлён'));
            closeWizard();
            await loadAndRender();
        } catch (err) {
            showToast('error', err.message || String(err));
        }
    }

    // ----- Apply with progress modal -----

    async function applyProfileWithProgress(modelName) {
        // Создаём modal
        const overlay = document.createElement('div');
        overlay.className = 'mode-wizard-overlay';
        overlay.innerHTML = `
            <div class="mode-wizard-modal" style="max-width:480px;">
                <div class="wizard-header">
                    <h3>${escapeHtml(I18N.t('settings.profiles.applying', 'Применение профиля…'))}</h3>
                </div>
                <div class="wizard-desc cpp-reload-progress" id="reloadProgressBody">
                    <div class="reload-step pending" data-step="save">
                        <span class="reload-step-icon">⏵</span>
                        <span class="reload-step-text">${escapeHtml(I18N.t('settings.profiles.step_save', 'Сохранение профиля…'))}</span>
                    </div>
                    <div class="reload-step pending" data-step="reload">
                        <span class="reload-step-icon">⏵</span>
                        <span class="reload-step-text">${escapeHtml(I18N.t('settings.profiles.step_reload', 'Reload модели на бэкендах…'))}</span>
                    </div>
                    <div class="reload-progress-bar"><div class="reload-progress-fill" id="reloadProgressFill"></div></div>
                </div>
                <div class="wizard-footer">
                    <button class="btn btn-secondary" id="reloadClose" disabled>${escapeHtml(I18N.t('common.close', 'Закрыть'))}</button>
                </div>
            </div>
        `;
        document.body.appendChild(overlay);

        const closeBtn = overlay.querySelector('#reloadClose');
        const fillEl = overlay.querySelector('#reloadProgressFill');
        const steps = {
            save: overlay.querySelector('[data-step="save"]'),
            reload: overlay.querySelector('[data-step="reload"]')
        };

        function setStep(name, state) {
            const el = steps[name];
            if (!el) return;
            el.classList.remove('pending', 'running', 'done', 'error');
            el.classList.add(state);
            const icon = el.querySelector('.reload-step-icon');
            if (state === 'running') icon.textContent = '⟳';
            else if (state === 'done') icon.textContent = '✓';
            else if (state === 'error') icon.textContent = '✗';
            else icon.textContent = '⏵';
        }

        try {
            // Шаг 1: save+reload в одном вызове
            setStep('save', 'running');
            fillEl.style.width = '20%';

            const resp = await API.request(
                '/api/v1/cppworker/model-profiles/' + encodeURIComponent(modelName) + '/apply',
                { method: 'POST' }
            );

            setStep('save', 'done');
            setStep('reload', 'running');
            fillEl.style.width = '70%';

            // Рендерим результаты
            const body = overlay.querySelector('#reloadProgressBody');
            const details = (resp.backends || []).map(b => {
                let icon = '·';
                let cls = 'pending';
                if (b.status === 'reloaded') { icon = '✓'; cls = 'done'; }
                else if (b.status === 'skipped') { icon = '⤳'; cls = 'pending'; }
                else if (b.status === 'error') { icon = '✗'; cls = 'error'; }
                const msg = b.message ? ' — ' + escapeHtml(b.message) : '';
                return `<div class="reload-step ${cls}"><span class="reload-step-icon">${icon}</span><span class="reload-step-text">${escapeHtml(b.backendId)}${msg}</span></div>`;
            }).join('');
            body.insertAdjacentHTML('beforeend', details);

            setStep('reload', 'done');
            fillEl.style.width = '100%';

            // Проверяем, все ли ok
            const errors = (resp.backends || []).filter(b => b.status === 'error');
            if (errors.length > 0) {
                showToast('warning', I18N.t('settings.profiles.applied_with_errors', 'Профиль применён с ошибками на ' + errors.length + ' бэкендах'));
            } else {
                showToast('success', I18N.t('settings.profiles.applied', 'Профиль применён'));
            }
        } catch (err) {
            setStep('save', 'error');
            setStep('reload', 'error');
            showToast('error', err.message || String(err));
        } finally {
            closeBtn.disabled = false;
        }

        closeBtn.addEventListener('click', () => overlay.remove());
    }

    // ----- Helpers -----

    function escapeHtml(s) {
        if (s == null) return '';
        return String(s)
            .replace(/&/g, '&amp;')
            .replace(/</g, '&lt;')
            .replace(/>/g, '&gt;')
            .replace(/"/g, '&quot;')
            .replace(/'/g, '&#39;');
    }

    function showToast(kind, message) {
        if (typeof window.showToast === 'function') {
            window.showToast(kind, message);
            return;
        }
        if (kind === 'error') console.error(message);
        else console.log(message);
        // Fallback
        const container = document.getElementById('toastContainer');
        if (!container) return;
        const t = document.createElement('div');
        t.className = 'toast ' + kind;
        t.textContent = message;
        container.appendChild(t);
        setTimeout(() => t.remove(), 5000);
    }

    // ----- Init -----

    function init() {
        const addBtn = document.getElementById('cppProfileAddBtn');
        if (addBtn) {
            addBtn.addEventListener('click', () => openWizard('', null));
        }
        // Загружаем при заходе на секцию (когда settings открывается)
        const section = document.getElementById('section-llama-cpp');
        if (section) {
            // Mutation observer на display — перезагружаем список при показе
            const observer = new MutationObserver(() => {
                if (section.style.display !== 'none') {
                    loadAndRender();
                }
            });
            observer.observe(section, { attributes: true, attributeFilter: ['style'] });
            // Первичная загрузка, если секция уже видима
            if (section.style.display !== 'none') {
                loadAndRender();
            }
        }
    }

    if (document.readyState === 'loading') {
        document.addEventListener('DOMContentLoaded', init);
    } else {
        init();
    }

    // Экспорт для тестов и debugging
    window.CppWorkerParams = {
        loadAndRender,
        openWizard,
        applyProfileWithProgress,
        CTX_PRESETS,
        CTX_MIN,
        CTX_MAX
    };
})();
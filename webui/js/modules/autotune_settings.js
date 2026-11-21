// autotune_settings.js — R54.7 (2026-08-24): WebUI AutoTune Settings page.
//
// Phase 3: Settings integration — global AutoTune switch + per-model overrides.
// Uses /api/v1/admin/autotune/config GET/PUT endpoints.

(function() {
    'use strict';

    const CONFIG_ENDPOINT = '/api/v1/admin/autotune/config';
    const AUTOTUNE_LIST_ENDPOINT = '/api/v1/admin/autotune';

    let currentConfig = { globalEnabled: true, perModel: {} };
    let availableModels = []; // model names from /api/v1/admin/autotune

    async function fetchConfig() {
        try {
            const r = await fetch(CONFIG_ENDPOINT, {
                // R59.5 (2026-09-03): window.Api → window.Api (correct namespace
                // exported from api.js:544). Old code returned undefined →
                // empty headers → 401 from /api/v1/admin/autotune/config.
                headers: window.Api?.getAuthHeaders?.() || {}
            });
            if (!r.ok) throw new Error(`HTTP ${r.status}`);
            const data = await r.json();
            currentConfig = {
                globalEnabled: data.globalEnabled !== false,
                perModel: data.perModel || {}
            };
            return currentConfig;
        } catch (e) {
            console.error('autotune-settings: fetchConfig failed', e);
            showStatus('Failed to load AutoTune config: ' + e.message, 'error');
            return null;
        }
    }

    async function fetchAvailableModels() {
        // Pull model names from /api/v1/admin/autotune (which lists loaded models per backend)
        try {
            const r = await fetch(AUTOTUNE_LIST_ENDPOINT, {
                // R59.5: see comment in fetchConfig above
                headers: window.Api?.getAuthHeaders?.() || {}
            });
            if (!r.ok) return [];
            const data = await r.json();
            const models = new Set();
            (data.backends || []).forEach(b => {
                if (b.plan && b.plan.ModelName) models.add(b.plan.ModelName);
            });
            return Array.from(models).sort();
        } catch (e) {
            console.warn('autotune-settings: could not fetch available models', e);
            return [];
        }
    }

    async function saveConfig() {
        const checkboxes = document.querySelectorAll('.autotune-per-model-override');
        const perModel = {};
        checkboxes.forEach(cb => {
            const modelName = cb.dataset.model;
            if (cb.value === 'null') {
                perModel[modelName] = null; // explicit null = clear override
            } else {
                perModel[modelName] = cb.value === 'true';
            }
        });
        const globalEnabled = document.getElementById('autotune-global-enabled')?.checked ?? true;
        try {
            const r = await fetch(CONFIG_ENDPOINT, {
                method: 'PUT',
                headers: {
                    'Content-Type': 'application/json',
                    ...(window.Api?.getAuthHeaders?.() || {})
                },
                body: JSON.stringify({ globalEnabled, perModel })
            });
            if (!r.ok) {
                const errBody = await r.text();
                throw new Error(`HTTP ${r.status}: ${errBody}`);
            }
            const data = await r.json();
            showStatus('AutoTune config saved (in-memory; persisted to data/state.json)', 'success');
            await render();
        } catch (e) {
            showStatus('Save failed: ' + e.message, 'error');
        }
    }

    function showStatus(msg, type) {
        const el = document.getElementById('autotune-save-status');
        if (!el) return;
        el.textContent = msg;
        el.className = 'autotune-save-status ' + (type || '');
    }

    function renderPerModelList() {
        const list = document.getElementById('autotune-per-model-list');
        if (!list) return;
        // R513 (2026-11-19): список — это `<select>` на каждую модель; его
        // пересборка закрывает выпадающий список и сбрасывает выбор, если
        // оператор как раз выбирает значение. Пока фокус внутри списка —
        // не перерисовываем (данные перечитает следующий тик после ухода).
        if (window.RefreshGuard && typeof window.RefreshGuard.isEditingIn === 'function' &&
            window.RefreshGuard.isEditingIn(list)) {
            return;
        }
        if (availableModels.length === 0) {
            list.innerHTML = '<div class="hint">No models loaded yet. Start a model or wait for metrics poller to refresh.</div>';
            return;
        }
        let html = '';
        availableModels.forEach(name => {
            const current = currentConfig.perModel[name];
            const effective = current !== undefined ? current : currentConfig.globalEnabled;
            const label = current === undefined
                ? `(inherit global: ${currentConfig.globalEnabled ? 'ON' : 'OFF'})`
                : `(explicit: ${current ? 'ON' : 'OFF'})`;
            html += `
                <div class="autotune-per-model-item">
                    <span class="model-name">${escapeHtml(name)}</span>
                    <div class="override-toggle">
                        <span class="hint">${label}</span>
                        <select class="autotune-per-model-override" data-model="${escapeHtml(name)}">
                            <option value="null"${current === undefined ? ' selected' : ''}>inherit</option>
                            <option value="true"${current === true ? ' selected' : ''}>ON</option>
                            <option value="false"${current === false ? ' selected' : ''}>OFF</option>
                        </select>
                    </div>
                </div>
            `;
        });
        list.innerHTML = html;
    }

    function escapeHtml(s) {
        if (s === null || s === undefined) return '';
        if (window.Utils && typeof window.Utils.escapeHtml === 'function') {
            return window.Utils.escapeHtml(s);
        }
        return String(s)
            .replace(/&/g, '&amp;')
            .replace(/</g, '&lt;')
            .replace(/>/g, '&gt;')
            .replace(/"/g, '&quot;')
            .replace(/'/g, '&#39;');
    }

    async function render() {
        const cfg = await fetchConfig();
        if (cfg) {
            const cb = document.getElementById('autotune-global-enabled');
            // R676: тумблер «AutoTune включён» — такое же поле формы на странице
            // «Настройки», как и остальные. Перерисовка (в том числе после
            // WebSocket-тика) не должна возвращать серверное значение поверх
            // выбора оператора: запись идёт через SafeForm.
            if (cb) {
                if (window.SafeForm && typeof window.SafeForm.setChecked === 'function') {
                    window.SafeForm.setChecked(cb, cfg.globalEnabled === true);
                } else {
                    cb.checked = cfg.globalEnabled;
                }
            }
        }
        availableModels = await fetchAvailableModels();
        renderPerModelList();
    }

    function setupHandlers() {
        // R493 (2026-11-17): кнопки #autotune-refresh-btn больше нет (одна кнопка
        // обновления — в шапке), а «Сохранить» теперь в разметке
        // (#autotune-save-btn, index.html, data-admin-only). Раньше «Сохранить»
        // создавалась кодом и вешалась на parentNode кнопки обновления: удали
        // обновление — и кнопка сохранения исчезла бы вместе с ним.
        const saveBtn = document.getElementById('autotune-save-btn');
        if (saveBtn && !saveBtn.dataset.bound) {
            saveBtn.dataset.bound = '1';
            saveBtn.addEventListener('click', saveConfig);
        }
        // Провайдер страницы настроек регистрируется ВНЕ setupHandlers (ниже, при
        // загрузке модуля): setupHandlers зовётся из init, а init — на каждый
        // показ страницы, поэтому регистрация здесь плодила дубликаты, и тик
        // авто-обновления дёргал render() несколько раз.
    }

    // R493 (2026-11-17): общая кнопка в шапке перечитывает и AutoTune-конфиг
    // (раньше это делала только своя кнопка, и «Обновить» в шапке не обновляла
    // список моделей AutoTune). Один раз на модуль — см. комментарий выше.
    if (window.DataRefresh && typeof window.DataRefresh.register === 'function') {
        window.DataRefresh.register('settings', function () { return render(); });
    }

    window.AutoTuneSettings = {
        init: async function() {
            setupHandlers();
            await render();
        },
        refresh: render
    };
})();


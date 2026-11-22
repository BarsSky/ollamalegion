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

    // === R680 (2026-11-21): ПЕРЕСБОРКА СПИСКА МОДЕЛЕЙ НЕ ТЕРЯЕТ ВЫБОР ОПЕРАТОРА ===
    //
    // ЖИВОЙ ДЕФЕКТ. Строка на модель — это `<select>`; список пересобирался одной
    // строкой `list.innerHTML = html`, то есть каждый раз создавались НОВЫЕ
    // селекторы. Заслон R513 (RefreshGuard выше) спасал только тот случай, когда
    // фокус СЕЙЧАС внутри списка: оператор выбрал значение, увёл фокус (или ушёл
    // тик авто-обновления / нажал «Сохранить» → render()), и пересборка возвращала
    // селектор к СЕРВЕРНОМУ значению. Следствие: «Сохранить» отправлял не то, что
    // оператор выбрал, — выбор терялся молча, без единой ошибки в консоли.
    //
    // КАК ПРАВИЛЬНО (тот же порядок, что у settings-registry.js:swapPanel, R676):
    // снять значения, которые оператор уже изменил (SafeForm.collectInputs,
    // ключ — data-model) → нарисовать новые узлы в DocumentFragment → одним
    // движением заменить содержимое списка → вернуть значения оператора
    // (SafeForm.restoreInputs; он же объявляет базой серверное значение у КАЖДОГО
    // узла, иначе следующая пересборка снова не увидела бы правку).
    //
    // Правила SafeForm не ослабляются: запись идёт только через его API
    // (collectInputs/restoreInputs), прямых `el.value = …` в этом файле нет.
    const PER_MODEL_KEY_ATTR = 'data-model';

    /** overrideLabel — подпись рядом с селектором («наследует глобальное»/«явный выбор»). */
    function overrideLabel(current) {
        if (current === undefined) {
            return '(inherit global: ' + (currentConfig.globalEnabled ? 'ON' : 'OFF') + ')';
        }
        return '(explicit: ' + (current ? 'ON' : 'OFF') + ')';
    }

    /**
     * buildPerModelItem — строка списка для одной модели.
     *
     * Собирается из createElement, а не из строки innerHTML: имя модели приходит
     * из ответа сервера (внешние данные), и в разметку оно попадает textContent'ом,
     * то есть HTML-исходником стать не может.
     */
    function buildPerModelItem(name) {
        const current = currentConfig.perModel[name];

        const item = document.createElement('div');
        item.className = 'autotune-per-model-item';

        const nameEl = document.createElement('span');
        nameEl.className = 'model-name';
        nameEl.textContent = name;
        item.appendChild(nameEl);

        const toggle = document.createElement('div');
        toggle.className = 'override-toggle';

        const hint = document.createElement('span');
        hint.className = 'hint';
        hint.textContent = overrideLabel(current);
        toggle.appendChild(hint);

        const select = document.createElement('select');
        select.className = 'autotune-per-model-override';
        // Ключ, по которому SafeForm переносит выбор при пересборке, и по
        // которому saveConfig() находит модель (cb.dataset.model).
        select.setAttribute(PER_MODEL_KEY_ATTR, name);
        // Значения те же, что читает saveConfig(): 'null' — наследовать
        // глобальный тумблер, 'true'/'false' — явный выбор.
        [['null', 'inherit'], ['true', 'ON'], ['false', 'OFF']].forEach(function (opt) {
            const option = document.createElement('option');
            option.value = opt[0];
            option.textContent = opt[1];
            select.appendChild(option);
        });
        // Нарисованное состояние — СЕРВЕРНОЕ; выбор оператора вернёт
        // swapPerModelList ниже.
        select.value = current === undefined ? 'null' : String(current);
        toggle.appendChild(select);
        item.appendChild(toggle);

        return item;
    }

    /** replaceChildren — заменить всё содержимое узла одной подготовленной разметкой. */
    function replaceChildren(node, fresh) {
        while (node.firstChild) node.removeChild(node.firstChild);
        node.appendChild(fresh);
    }

    /**
     * swapPerModelList — подменить содержимое списка, сохранив выбор оператора (R680).
     *
     * @param {Element} list контейнер #autotune-per-model-list
     * @param {DocumentFragment} fragment новые узлы (ещё не в документе)
     * @returns {boolean} true — выбор оператора перенесён (SafeForm был доступен)
     */
    function swapPerModelList(list, fragment) {
        const SF = window.SafeForm;
        if (!SF || typeof SF.collectInputs !== 'function' || typeof SF.restoreInputs !== 'function') {
            // SafeForm не загружен (урезанная страница/тест) — рисуем как раньше,
            // переносить выбор нечем. В боевой странице модуль подключён раньше
            // (index.html: safe-form.js идёт до autotune_settings.js).
            replaceChildren(list, fragment);
            return false;
        }
        const pending = SF.collectInputs(list, PER_MODEL_KEY_ATTR);
        replaceChildren(list, fragment);
        SF.restoreInputs(list, pending, PER_MODEL_KEY_ATTR);
        return true;
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
        // R680: новые узлы рисуются в DocumentFragment — список не остаётся
        // пустым между удалением старых узлов и вставкой новых, а выбор
        // оператора переносится следом (swapPerModelList).
        const fragment = document.createDocumentFragment();
        if (availableModels.length === 0) {
            const hint = document.createElement('div');
            hint.className = 'hint';
            hint.textContent = 'No models loaded yet. Start a model or wait for metrics poller to refresh.';
            fragment.appendChild(hint);
        } else {
            availableModels.forEach(function (name) {
                fragment.appendChild(buildPerModelItem(name));
            });
        }
        swapPerModelList(list, fragment);
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


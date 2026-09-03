/**
 * gguf-renderer-state.js — State object + helpers (UNHEALTHY_STATUSES, isHealthyBackend, visibleBackends).
 *
 * R57.5b (2026-09-03): extracted from webui/js/modules/gguf-renderer.js.
 *
 * State — это ЦЕНТРАЛЬНЫЙ объект для всего GgufRenderer. 80+ функций
 * читают/пишут в state. После этого extract'а, state живёт в
 * `window.GgufModule.state` и доступен всем остальным модулям GgufRenderer
 * (list, detail, actions, refresh).
 *
 * Контракт:
 *   - window.GgufModule.state = { ... все 25+ полей ... }
 *   - window.GgufModule.healthyStatuses, isHealthyBackend, visibleBackends
 *   - window.GgufModule._ = I18N helper
 *
 * Загружается ДО gguf-renderer-list.js / -detail.js / -actions.js / -refresh.js.
 * ВАЖНО: state должен быть инициализирован ДО того, как любой другой модуль
 * GgufRenderer попытается его читать. Поэтому gguf-renderer-state.js
 * загружается ПЕРВЫМ в defer-цепочке.
 */
(function() {
    'use strict';

    const M = (window.GgufModule = window.GgufModule || {});

    // I18N helper (используется во всех GgufRenderer модулях)
    M._ = function(key, vars) {
        return window.I18N ? I18N.t(key, vars) : key;
    };

    // ---- State ----
    M.state = {
        registeredBackends: [],
        backendDataLoaded: false,
        selectedBackendId: null,
        // Показывать нерабочие (unhealthy/offline/draining) бэкенды в GGUF-списке.
        // По умолчанию скрываем, чтобы не отображались заглушки/недоступные ноды.
        showUnhealthy: false,
        // Detail data for selected backend
        detailPane: 'about', // 'about' | 'models' | 'downloads' | 'hf' | 'settings'
        detailLoading: false,
        detailError: null,
        workerInfo: null,
        gpuInfo: null,
        localModels: [],
        loadedModels: [],
        // Runtime-параметры (n_ctx, gpu_layers, batch_size, flash_attn, n_layers, n_embd,
        // gguf_context_length) реально загруженных моделей. Загружаются параллельно
        // с loadedModels из /api/v1/cppworker/config/runtime. Используются в
        // renderLoadedPane() чтобы показать «default: 8192, runtime: 32768».
        runtimeModels: {},
        // === Round 26 v0.5.13: Active queries per model ===
        // Polling /api/models/active-queries каждые 3s для отображения
        // busy badge "🔴 Generating (N active)" на loaded model card.
        // Помогает UX: пользователь видит, почему apply ждёт, и не
        // путает это с "настройки заблокированы".
        activeQueries: {}, // key: model name, value: number
        _activeQueriesTimer: null,
        activeDownloads: [],
        downloadProgress: {},
        // HF search within the detail view
        hfSearchQuery: '',
        hfSearchResults: [],
        hfSearchSelected: null,
        hfModelFiles: [],
        hfSearching: false,
        // Settings (load options) — per-backend but stored globally
        loadOptions: {
            gpuLayers: -1,
            ctxSize: 2048,
            batchSize: 512,
            flashAttn: false,
            numa: false,
            useMmap: true,
            tensorSplit: null,
            autoGpuDistribution: true,
            strategy: 'vram-ratio'
        },
        // Round 32 #6 (2026-08-10): cache отрендеренного HTML формы Settings.
        // Без этого фикса refreshDetailPane() (вызывается из active-queries polling
        // каждые 3 секунды) пересоздавал весь .gguf-detail-content innerHTML,
        // затирая форму на spinner-плейсхолдер. Пользователь видел форму
        // на краткий миг, потом она исчезала ("идёт в перезагрузку"). Теперь
        // renderSettingsPane() использует кэш вместо placeholder, если форма
        // уже отрендерена для текущего бэкенда. Ключ — backendId, чтобы при
        // переключении между бэкендами показывалась форма нового бэкенда.
        _settingsFormHtmlByBackend: {}, // { backendId: { html, cfg } }
        // Reference на текущую DOM-панель для делегированного обработчика кликов.
        // Нужно, чтобы onDetailPanelClick() работал даже после refreshDetailPanel(),
        // который пересоздаёт содержимое панели (но не сам узел #ggufDetailPanel —>
        // в данный момент узел не пересоздаётся, но на будущее держим ссылку).
        _detailPanel: null
    };

    // Статусы, которые считаем «нерабочими» и по умолчанию скрываем в GGUF.
    M.healthyStatuses = { unhealthy: false, offline: false, draining: false, ollama_unavailable: false };

    M.isHealthyBackend = function(b) {
        return !M.healthyStatuses[b.status];
    };

    M.visibleBackends = function() {
        if (M.state.showUnhealthy) return M.state.registeredBackends;
        return M.state.registeredBackends.filter(M.isHealthyBackend);
    };
})();

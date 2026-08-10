/**
 * OllamaLegion WebUI — Orchestrator
 * Imports: Utils, Api, WebSocketManager, Renderers (loaded before this file)
 */
const ui = (function () {
    const { dashboard, backendsPage, modelsPage, sessionsPage, queuePage, logs: renderLogs, predictionAlerts, proxyLogs: renderProxyLogs, copyProxyLogs: renderCopyProxyLogs, agentsPage: renderAgentsPage, renderAgentDetails } = Renderers;

    // State
    const data = {
        backends: [],
        sessions: [],
        queue: {},
        queueTasks: [],
        queueHistory: [],
        logs: [],
        models: [],
        proxyLogs: [],
        agents: null
    };
    let currentPage = 'dashboard';
    let refreshTimer = null;
    let dashboardRenderTimer = null;
    let autoSaveTimer = null;

    // ---- Initialization ----

    function init() {
        initTheme();
        initDensity();
        setupI18n();
        setupNavigation();
        setupEventListeners();
        setupRestartHandler();
        setupApiEvents();
        setupWebSocketEvents();

        // Initial data load — cluster state first, drives connection status
        fetchClusterState().then(function () {
            fetchQueue();
            fetchQueueDetails();
            fetchQueueHistory();
            fetchSessions();
        });

        // Load agents on startup
        fetchAgents();

        // Periodic refresh
        startPeriodicRefresh();

        // Initialize Settings UI modules
        if (window.SettingsUI) {
            SettingsUI.setupAccordion();
            SettingsUI.restoreAccordionState();
            SettingsUI.setupModeSelector();
            SettingsUI.setupBackendEngineSwitch();
            SettingsUI.initBackendEngineCards();
        }

        // ---- Notifications (F.α — SSE EventBus) ----
        // EventSource подключается к /api/v1/events (ring buffer + heartbeat + live stream).
        // Токен передаётся в query (?token=...) потому что EventSource API не поддерживает custom headers.
        if (window.notifications && window.notifications.init) {
            window.notifications.init({
                token: (window.WEBUI_CONFIG && window.WEBUI_CONFIG.API_TOKEN) || '',
                onNew: function (ev) { renderNotification(ev); },
                onClear: function () { renderNotificationsList(); }
            });
            setupNotificationsUI();
        }

        // Check if setup wizard should be shown — проверяем сервер, а не localStorage
        if (window.SetupWizard && window.SetupWizard.isInitialized) {
            window.SetupWizard.isInitialized().then(function (initialized) {
                if (!initialized) {
                    window.SetupWizard.start();
                }
            }).catch(function () {
                // При ошибке подключения не показываем wizard — сервер может быть недоступен
                console.warn('Failed to check initialization status from server');
            });
        }

        // Setup logs tab navigation (System / Proxy sub-tabs)
        setupLogsTabNavigation();
        // Load proxy logs from REST API on startup
        fetchProxyLogs();

        // F.2 (Session F): live tail системных логов через WebSocket /ws/logs.
        // Заменяет polling на Logs tab: при connect получаем snapshot, далее live stream.
        initLogsStream();

        addLog(window.I18N ? I18N.t('app.webui_initialized') : 'WebUI initialized', 'info');
    }

    // ---- Theme ----

    /**
     * Инициализация темы (Session C — Theme toggle).
     *
     * Логика:
     * 1. Anti-FOIT inline скрипт в <head> уже установил data-theme до загрузки CSS
     *    (приоритет: localStorage > system preference > dark).
     * 2. Здесь мы только синхронизируем UI (иконка toggle).
     * 3. Кнопка переключения → toggle + broadcast события.
     * 4. Keyboard shortcut Ctrl+Shift+T → toggle.
     * 5. Слушаем изменения system preference (prefers-color-scheme) если пользователь
     *    явно не выбрал тему (нет ключа в localStorage).
     *
     * Broadcast: window event 'theme:changed' с detail={theme: 'dark'|'light'}
     * позволяет другим модулям (например, Chart.js, монитору) реагировать на смену темы.
     */
    function initTheme() {
        var current = document.documentElement.getAttribute('data-theme') || 'dark';
        updateThemeToggleIcon(current);

        // Broadcast theme change — позволяет модулям реагировать на смену.
        function broadcastThemeChange(theme) {
            try {
                window.dispatchEvent(new CustomEvent('theme:changed', { detail: { theme: theme } }));
            } catch (e) { /* CustomEvent может не поддерживаться в очень старых браузерах */ }
        }

        // Switch theme — вызывается из button click и из keyboard shortcut.
        function switchTheme(next) {
            document.documentElement.setAttribute('data-theme', next);
            try { localStorage.setItem('ollamalegion_theme', next); } catch (e) { /* ignore */ }
            updateThemeToggleIcon(next);
            broadcastThemeChange(next);
        }

        var THEMES = ['dark', 'light', 'linear', 'nvidia', 'vercel'];
        var toggleBtn = document.getElementById('themeToggle');
        if (toggleBtn) {
            toggleBtn.addEventListener('click', function () {
                var cur = document.documentElement.getAttribute('data-theme') || 'dark';
                var idx = THEMES.indexOf(cur);
                var next = THEMES[(idx + 1) % THEMES.length];
                switchTheme(next);
            });
        }

        // Keyboard shortcut: Ctrl+Shift+T (Windows/Linux), Cmd+Shift+T (macOS).
        document.addEventListener('keydown', function (e) {
            var isToggleShortcut = (e.ctrlKey || e.metaKey) && e.shiftKey && (e.key === 'T' || e.key === 't' || e.key === 'Е' || e.key === 'е');
            if (!isToggleShortcut) return;
            // Не перехватываем если фокус в input/textarea (даём работать обычному вводу).
            var tag = (e.target && e.target.tagName) || '';
            if (tag === 'INPUT' || tag === 'TEXTAREA' || (e.target && e.target.isContentEditable)) return;
            e.preventDefault();
            var cur = document.documentElement.getAttribute('data-theme') || 'dark';
            var idx2 = ['dark','light','linear','nvidia','vercel'].indexOf(cur); var next2 = ['dark','light','linear','nvidia','vercel'][(idx2 + 1) % 5]; switchTheme(next2);
        });

        // Слушаем изменения system preference (только если пользователь явно не выбрал тему).
        if (window.matchMedia) {
            try {
                var mq = window.matchMedia('(prefers-color-scheme: light)');
                var onMqChange = function (ev) {
                    // Не перезаписываем если пользователь явно выбрал тему.
                    try {
                        if (localStorage.getItem('ollamalegion_theme')) return;
                    } catch (e) { /* ignore */ }
                    var next = ev.matches ? 'light' : 'dark';
                    document.documentElement.setAttribute('data-theme', next);
                    updateThemeToggleIcon(next);
                    broadcastThemeChange(next);
                };
                // addEventListener / addListener — старые браузеры используют addListener.
                if (mq.addEventListener) mq.addEventListener('change', onMqChange);
                else if (mq.addListener) mq.addListener(onMqChange);
            } catch (e) { /* matchMedia недоступно — ignore */ }
        }
    }

    /**
     * initDensity — Sprint 1 data-dense variant (2026-06-29).
     * Переключает body[data-density] между "normal" и "dense".
     * Состояние сохраняется в localStorage["ollamalegion_density"].
     * Применяется ДО загрузки CSS через inline-скрипт в index.html (early-load).
     */
    function initDensity() {
        var stored = 'normal';
        try { stored = localStorage.getItem('ollamalegion_density') || 'normal'; } catch (e) { /* ignore */ }
        if (stored !== 'dense' && stored !== 'normal') stored = 'normal';
        document.body.setAttribute('data-density', stored);
        updateDensityToggleIcon(stored);

        var toggleBtn = document.getElementById('densityToggle');
        if (toggleBtn) {
            toggleBtn.addEventListener('click', function () {
                var cur = document.body.getAttribute('data-density') || 'normal';
                var next = cur === 'dense' ? 'normal' : 'dense';
                document.body.setAttribute('data-density', next);
                try { localStorage.setItem('ollamalegion_density', next); } catch (e) { /* ignore */ }
                updateDensityToggleIcon(next);
                try {
                    window.dispatchEvent(new CustomEvent('density:changed', { detail: { density: next } }));
                } catch (e) { /* ignore */ }
            });
        }
    }

    function updateDensityToggleIcon(density) {
        var btn = document.getElementById('densityToggle');
        if (!btn) return;
        var icon = btn.querySelector('.density-icon');
        if (icon) {
            // Меняем FA-class: fa-table-cells-large (dense) / fa-table-cells (normal).
            icon.className = 'density-icon ' + (density === 'dense' ? 'fas fa-table-cells-large' : 'fas fa-table-cells');
        }
        btn.classList.toggle('is-dense', density === 'dense');
        btn.setAttribute('data-density-current', density);
    }

    // 2026-06-30: заменили ☀️/🌙 эмодзи (жёлтые/белые системные, расходились со стилем)
    // на Font Awesome fa-moon (в dark) / fa-sun (в light) — цвет наследуется от .btn-theme-toggle
    // через var(--text-secondary) и больше не зависит от emoji-рендера ОС.
    var THEME_ICONS = { dark: 'fa-moon', light: 'fa-sun', linear: 'fa-circle', nvidia: 'fa-bold', vercel: 'fa-arrow-up' };
    var THEME_LETTERS = { dark: '', light: '', linear: 'L', nvidia: 'N', vercel: 'V' };
    var THEME_LABELS = { dark: 'Dark', light: 'Light', linear: 'Linear', nvidia: 'NVIDIA', vercel: 'Vercel' };
    function updateThemeToggleIcon(theme) {
        var btn = document.getElementById('themeToggle');
        if (!btn) return;
        var icon = document.getElementById('themeToggleIcon');
        if (icon) {
            if (theme === 'dark' || theme === 'light') {
                icon.className = (theme === 'dark' ? 'fas fa-moon' : 'fas fa-sun');
                icon.textContent = '';
            } else {
                icon.className = '';
                icon.textContent = (THEME_LETTERS[theme] || '?');
                icon.style.cssText = 'font-weight:700;font-size:14px;font-style:normal;';
            }
            icon.setAttribute('aria-hidden', 'true');
        }
        btn.title = THEME_LABELS[theme] || theme;
    }

    // ---- i18n ----

    function updateUITranslations() {
        document.querySelectorAll('[data-i18n]').forEach(function (el) {
            var key = el.getAttribute('data-i18n');
            if (key && window.I18N) {
                if (el.tagName === 'INPUT' && el.hasAttribute('data-i18n-placeholder')) {
                    el.placeholder = I18N.t(el.getAttribute('data-i18n-placeholder'));
                } else {
                    el.textContent = I18N.t(key);
                }
            }
        });
        // Process data-i18n-title
        document.querySelectorAll('[data-i18n-title]').forEach(function (el) {
            var key = el.getAttribute('data-i18n-title');
            if (key && window.I18N) {
                el.title = I18N.t(key);
            }
        });
        // Update page title
        if (currentPage && window.I18N) {
            var titleKey = 'header.' + currentPage;
            var h1 = document.getElementById('pageTitle');
            if (h1) h1.textContent = I18N.t(titleKey);
        }
    }

    function setupI18n() {
        var langSelect = document.getElementById('langSelect');
        if (langSelect && window.I18N) {
            var currentLang = I18N.getLang();
            langSelect.value = currentLang;
            langSelect.addEventListener('change', function () {
                I18N.setLang(this.value);
                updateUITranslations();
                updateThemeToggleIcon(document.documentElement.getAttribute('data-theme') || 'dark');
                showToast(window.I18N ? I18N.t('app.lang_changed') : 'Language changed', 'success');
            });
        }
        window.addEventListener('i18n:changed', function () {
            updateUITranslations();
            updateThemeToggleIcon(document.documentElement.getAttribute('data-theme') || 'dark');
        });
        // Process data-i18n-placeholder on initial load
        document.querySelectorAll('[data-i18n-placeholder]').forEach(function (el) {
            var key = el.getAttribute('data-i18n-placeholder');
            if (key && window.I18N) {
                el.placeholder = I18N.t(key);
            }
        });
        updateUITranslations();
    }

    // ---- Restart Handler ----

    function setupRestartHandler() {
        var restartBtn = document.getElementById('restartBalancerBtn');
        var restartModal = document.getElementById('restartConfirmModal');
        var modalConfirm = document.getElementById('restartModalConfirm');
        var modalCancel = document.getElementById('restartModalCancel');
        var modalClose = document.getElementById('restartModalClose');
        var indicator = document.getElementById('restartIndicator');

        function showRestartModal() { if (restartModal) restartModal.classList.add('active'); }
        function hideRestartModal() { if (restartModal) restartModal.classList.remove('active'); }

        if (restartBtn) restartBtn.addEventListener('click', showRestartModal);
        if (modalClose) modalClose.addEventListener('click', hideRestartModal);
        if (modalCancel) modalCancel.addEventListener('click', hideRestartModal);
        if (restartModal) restartModal.addEventListener('click', function (e) { if (e.target.id === 'restartConfirmModal') hideRestartModal(); });

        if (modalConfirm) {
            modalConfirm.addEventListener('click', function () {
                hideRestartModal();
                if (indicator) {
                    indicator.style.display = 'block';
                    indicator.innerHTML = '<div class="restart-indicator"><div class="restart-spinner"></div><span>' + (window.I18N ? I18N.t('settings.restarting') : 'Restarting balancer...') + '</span></div>';
                }
                Api.post('/api/v1/admin/restart').then(function () {
                    if (indicator) indicator.innerHTML = '<div style="color: var(--success); padding: 8px;">' + (window.I18N ? I18N.t('settings.restarted') : 'Balancer restarted successfully') + '</div>';
                    showToast(window.I18N ? I18N.t('settings.restarted') : 'Balancer restarted successfully', 'success');
                }).catch(function (err) {
                    if (indicator) indicator.innerHTML = '<div style="color: var(--danger); padding: 8px;">' + (window.I18N ? I18N.t('settings.restart_error') : 'Balancer restart failed') + '</div>';
                    showToast((window.I18N ? I18N.t('common.error') : 'Error') + ': ' + (err.message || err), 'error');
                });
            });
        }
    }

    // ---- Navigation ----

    function setupNavigation() {
        document.querySelectorAll('.nav-item').forEach(item => {
            item.addEventListener('click', (e) => {
                e.preventDefault();
                switchPage(item.dataset.page);
            });
        });
    }

    function switchPage(page) {
        document.querySelectorAll('.page').forEach(p => p.classList.remove('active'));
        document.querySelectorAll('.nav-item').forEach(n => n.classList.remove('active'));

        const targetPage = document.getElementById(page + '-page');
        const targetNav = document.querySelector(`[data-page="${page}"]`);

        if (targetPage) targetPage.classList.add('active');
        if (targetNav) targetNav.classList.add('active');

        currentPage = page;
        var titleKey = 'header.' + page;
        var h1 = document.getElementById('pageTitle');
        if (h1 && window.I18N) h1.textContent = I18N.t(titleKey);
        else Utils.setText('pageTitle', getPageTitle(page));

        refreshPage(page);

        if (page === 'backends' || page === 'models') {
            fetchClusterState();
        }
    }

    function getPageTitle(page) {
        // Fallback only — i18n handles actual titles via header.* keys
        const titles = {
            dashboard: 'Dashboard',
            monitor: 'Cluster Monitor',
            backends: 'Backend Management',
            models: 'Models',
            sessions: 'Sessions',
            queue: 'Queue',
            gguf: 'GGUF Models',
            logs: 'System Logs',
            settings: 'Settings',
            agents: 'Agents'
        };
        return titles[page] || 'Dashboard';
    }

    // Статусы, которые считаем нерабочими и скрываем в UI по умолчанию.
    var UNHEALTHY_STATUSES_UI = { unhealthy: true, offline: true, draining: true, ollama_unavailable: true };

    /**
     * Фильтрует бэкенды по текущему типу (ollama / llama_cpp) и по статусу.
     * Нерабочие бэкенды (unhealthy/offline/draining/ollama_unavailable) скрываются,
     * чтобы в WebUI не отображались заглушки/недоступные ноды.
     * Использует BackendTypeFilter как клиентский fallback.
     * Если сервер уже отфильтровал — фильтр пройдёт без изменений.
     */
    function filterBackendsForUI(backends) {
        if (!Array.isArray(backends)) return backends;
        var filtered = backends.filter(function (b) {
            // Skip agent-only registrations: same host + cppWorkerPort as another backend.
            // Agent provides GPU metrics but is not a separate inference backend.
            if (b.type === 'llama_cpp' && b.hasAgent) {
                // Check if there's a non-agent backend with same host + cppWorkerPort
                var hasNonAgent = backends.some(function (bb) {
                    return bb.id !== b.id && bb.type === 'llama_cpp' &&
                           !bb.hasAgent && bb.host === b.host &&
                           bb.cppWorkerPort === b.cppWorkerPort;
                });
                if (hasNonAgent) return false; // hide agent duplicate
            }
            return !UNHEALTHY_STATUSES_UI[b.status];
        });
        if (!window.BackendTypeFilter) return filtered;
        var type = BackendTypeFilter.getCurrentType();
        return BackendTypeFilter.filterBackends(filtered, type);
    }

    function refreshPage(page) {
        switch (page) {
            case 'dashboard':
                var filteredBackends = filterBackendsForUI(data.backends);
                // Cluster-level loaded count учитывает и ollama-, и llama_cpp-бэкенды.
                // Запрашиваем асинхронно, не блокируем рендер dashboard.
                Api.clusterModels.loaded().then(function (loadedResp) {
                    if (loadedResp && typeof loadedResp.count === 'number') {
                        Utils.setText('loadedModels', _t('renderers.models_count_loaded', { count: loadedResp.count }));
                        Utils.setText('totalModels', loadedResp.count);
                    }
                }).catch(function () { /* тихо: fallback на ollama.runningModels внутри dashboard() */ });
                dashboard(filteredBackends, data.sessions, data.queue);
                predictionAlerts(filteredBackends);
                break;
            case 'monitor':
                sendMonitorConfig();
                break;
            case 'backends':
                var filteredBackendsB = filterBackendsForUI(data.backends);
                backendsPage([...filteredBackendsB].sort(function(a, b) { return (a.id || '').localeCompare(b.id || ''); }));
                break;
            case 'models':
                modelsPage(filterBackendsForUI(data.backends));
                // Session A — Bulk operations: после re-render моделей сбросить
                // stale selections в toolbar (selected Set живёт дольше DOM).
                if (window.bulkModels && typeof window.bulkModels.renderToolbar === 'function') {
                    window.bulkModels.renderToolbar();
                }
                break;
            case 'sessions':
                sessionsPage(data.sessions);
                break;
            case 'queue':
                queuePage(data.queue, data.queueTasks, data.queueHistory);
                break;
            case 'logs':
                renderLogs(data.logs);
                // Also render proxy logs when switching to logs page (only if proxy tab is active)
                var proxyTab = document.getElementById('logsTabProxy');
                if (proxyTab && proxyTab.classList.contains('active')) {
                    renderProxyLogs(data.proxyLogs);
                }
                break;
            case 'agents':
                if (data.agents) renderAgentsPage(data.agents);
                break;
            case 'gguf':
                if (window.GgufRenderer) {
                    var container = document.getElementById('ggufContainer');
                    if (container) GgufRenderer.render(container);
                }
                break;
            case 'settings':
                loadSettings();
                setTimeout(function() { loadBackendLimits(); }, 100);
                break;
        }
    }

    function sendMonitorConfig() {
        const frame = document.getElementById('monitorFrame');
        if (!frame || !frame.contentWindow) return;
        const CFG = window.WEBUI_CONFIG || {};
        frame.contentWindow.postMessage({
            type: 'ollamalegion-config',
            apiBase: CFG.API_BASE || '',
            apiToken: CFG.API_TOKEN || '',
            refreshInterval: 2000,
            lang: localStorage.getItem('ollamalegion_lang') || 'ru'
        }, '*');
    }

    function refreshCurrentPage() {
        refreshPage(currentPage);
    }

    // ---- Event Listeners ----

    function setupEventListeners() {
        document.getElementById('refreshBtn').addEventListener('click', () => {
            refreshCurrentPage();
            showToast(window.I18N ? I18N.t('common.success') : 'Data updated', 'success');
        });

        document.getElementById('addBackendBtn').addEventListener('click', () => openBackendModal());
        if (document.getElementById('addBackendBtn2')) {
            document.getElementById('addBackendBtn2').addEventListener('click', () => openBackendModal());
        }

        document.getElementById('modalClose').addEventListener('click', closeModal);
        document.getElementById('modalCancel').addEventListener('click', closeModal);
        document.getElementById('modalSave').addEventListener('click', saveBackend);
        document.getElementById('modalDelete').addEventListener('click', deleteBackend);

        var backendSearch = document.getElementById('backendSearch');
        if (backendSearch) backendSearch.addEventListener('input', Utils.debounce((e) => filterBackends(e.target.value), 150));
        var sessionSearch = document.getElementById('sessionSearch');
        if (sessionSearch) sessionSearch.addEventListener('input', Utils.debounce((e) => filterSessions(e.target.value), 150));

        document.getElementById('clearLogs').addEventListener('click', () => {
            data.logs = [];
            renderLogs(data.logs);
        });
        var exportLogsBtn = document.getElementById('exportLogs');
        if (exportLogsBtn) exportLogsBtn.addEventListener('click', exportLogs);
        var exportBackendsBtn = document.getElementById('exportBackends');
        if (exportBackendsBtn) exportBackendsBtn.addEventListener('click', exportBackends);

        document.getElementById('saveSettings').addEventListener('click', function () { saveSettings(false); });
        document.getElementById('resetSettings').addEventListener('click', resetSettings);

        // Session 17 (2026-07-27): runtime overrides "Reset to bundled defaults" кнопка.
        var llamaCppResetBtn = document.getElementById('llamaCppResetBtn');
        if (llamaCppResetBtn) {
            llamaCppResetBtn.addEventListener('click', function () { resetLlamaCppOverride(); });
        }

        // Session 18 (2026-07-28): Quick Presets bar (Speed / Memory / Context / CPU-only)
        // Клик по пресету заполняет форму рекомендованными значениями и подсвечивает активную кнопку.
        var PRESETS = {
            speed:   { ctxSize: 4096,  gpuLayers: -2, kvCacheType: 'f16',  flashAttn: true,  nThreads: 0,  useMmap: true,  batchSize: 512, idleUnloadMinutes: 0 },
            memory:  { ctxSize: 2048,  gpuLayers: -2, kvCacheType: 'q4_0', flashAttn: true,  nThreads: 0,  useMmap: true,  batchSize: 256, idleUnloadMinutes: 5 },
            context: { ctxSize: 32768, gpuLayers: -2, kvCacheType: 'q8_0', flashAttn: true,  nThreads: 0,  useMmap: true,  batchSize: 512, idleUnloadMinutes: 0, ropeScalingType: 'yarn', ropeScalingFactor: 4 },
            cpu:     { ctxSize: 2048,  gpuLayers: 0,  kvCacheType: 'q8_0', flashAttn: true,  nThreads: 8,  useMmap: true,  batchSize: 256, idleUnloadMinutes: 0 }
        };
        function applyGgufPreset(name) {
            var preset = PRESETS[name];
            if (!preset) return;
            // Заполняем поля формы (id-ы совпадают с index.html)
            var map = {
                ctxSize: 'ctxSize', gpuLayers: 'gpuLayers', kvCacheType: 'kvCacheType',
                flashAttn: 'flashAttn', nThreads: 'nThreads', useMmap: 'useMmap',
                batchSize: 'batchSize', idleUnloadMinutes: 'idleUnloadMinutes',
                ropeScalingType: 'ropeScalingType', ropeScalingFactor: 'ropeScalingFactor'
            };
            Object.keys(map).forEach(function (k) {
                if (preset[k] === undefined) return;
                var el = document.getElementById(map[k]);
                if (!el) return;
                if (el.type === 'checkbox') el.checked = !!preset[k];
                else el.value = preset[k];
            });
            // Visual feedback: подсветить активную кнопку
            document.querySelectorAll('#ggufPresetSpeed, #ggufPresetMemory, #ggufPresetContext, #ggufPresetCpu')
                .forEach(function (btn) { btn.classList.toggle('active', btn.dataset.preset === name); });
            // Тост/уведомление (используем глобальный showToast если есть)
            var label = (window.I18N && I18N.t('gguf.preset_applied', 'Preset applied:') + ' ' + name) || ('Preset: ' + name);
            if (typeof window.showToast === 'function') window.showToast(label, 'success');
        }
        ['ggufPresetSpeed', 'ggufPresetMemory', 'ggufPresetContext', 'ggufPresetCpu'].forEach(function (id) {
            var btn = document.getElementById(id);
            if (btn) btn.addEventListener('click', function () { applyGgufPreset(btn.dataset.preset); });
        });

        // Export / Import Config buttons
        var exportBtn = document.getElementById('exportSettingsBtn');
        if (exportBtn) exportBtn.addEventListener('click', function () {
            if (window.ConfigIO) ConfigIO.exportConfig();
        });
        var exportBtn2 = document.getElementById('exportSettingsBtn2');
        if (exportBtn2) exportBtn2.addEventListener('click', function () {
            if (window.ConfigIO) ConfigIO.exportConfig();
        });

        var importBtn = document.getElementById('importSettingsBtn');
        if (importBtn) importBtn.addEventListener('click', function () {
            if (window.ConfigIO) {
                ConfigIO.importConfigFromFile().then(function (data) {
                    return ConfigIO.showImportPreview(data);
                }).then(function (confirmed) {
                    if (confirmed) ConfigIO.applyConfig(data);
                }).catch(function (err) {
                    showToast(err.message || 'Import failed', 'error');
                });
            }
        });
        var importBtn2 = document.getElementById('importSettingsBtn2');
        if (importBtn2) importBtn2.addEventListener('click', function () {
            if (window.ConfigIO) {
                ConfigIO.importConfigFromFile().then(function (data) {
                    return ConfigIO.showImportPreview(data);
                }).then(function (confirmed) {
                    if (confirmed) ConfigIO.applyConfig(data);
                }).catch(function (err) {
                    showToast(err.message || 'Import failed', 'error');
                });
            }
        });

        // Auto-save on settings form changes
        var settingsFields = ['balancingAlgorithm', 'useEnhancedScoring', 'modelAffinity', 'sessionStickiness', 'predictionFiltering', 'gpuMaxUsage', 'vramMaxUsage', 'cpuMaxUsage', 'ramMaxUsage', 'minFreeDisk', 'modelReplicationMinInstances', 'modelReplicationMaxInstances', 'modelReplicationIdleUnload', 'rpcCoordinatorURL', 'rpcCoordinatorWorkerPort', 'rpcCoordinatorProtocol', 'rpcCoordinatorTimeout', 'virtualModelsCoordMode', 'virtualModelsTimeout', 'distInferenceGrpcPort', 'agentCollectInterval', 'agentHeartbeatInterval', 'agentMaxConcurrent', 'agentMaxModels', 'agentTimeout', 'gpuLayers', 'ctxSize', 'batchSize', 'flashAttnType', 'nThreads', 'gpuStrategy', 'tensorSplit', 'splitMode', 'mainGpu', 'rpcBackend', 'noMemoryMap', 'kvCacheType', 'noKvOffload', 'ropeFreqBase', 'ropeFreqScale', 'ropeScalingType', 'ropeScalingFactor', 'yarnExtFactor', 'yarnAttnFactor', 'yarnBetaFast', 'yarnBetaSlow', 'rmsNormEps', 'idleUnloadMinutes', 'enableMetrics', 'metricsRetention', 'autoGpuDistribution', 'flashAttn', 'numa', 'useMmap', 'useMlock', 'enableReasoning', 'reasoningBudget'];
        settingsFields.forEach(function (id) {
            var el = document.getElementById(id);
            if (el) {
                el.addEventListener('change', autoSaveSettings);
                if (el.tagName === 'INPUT' && el.type === 'number') {
                    el.addEventListener('input', autoSaveSettings);
                }
            }
        });

        document.getElementById('backendModal').addEventListener('click', (e) => {
            if (e.target.id === 'backendModal') closeModal();
        });

        // Backend type selector in add-backend form — sync with BackendTypeFilter
        var formBackendType = document.getElementById('formBackendType');
        if (formBackendType) {
            formBackendType.addEventListener('change', function () {
                var newType = this.value;
                if (window.BackendTypeFilter) {
                    BackendTypeFilter.setCurrentType(newType);
                }
            });
        }

        // Type filter buttons (Все / Ollama / llama.cpp) на Dashboard и Backends page
        document.querySelectorAll('.type-filter-group').forEach(function(group) {
            group.querySelectorAll('.type-filter-btn').forEach(function(btn) {
                btn.addEventListener('click', function() {
                    // Снять active со всех кнопок в этой группе
                    group.querySelectorAll('.type-filter-btn').forEach(function(b) { b.classList.remove('active'); });
                    this.classList.add('active');
                    // Применить фильтр через BackendTypeFilter
                    var filterType = this.dataset.type;
                    if (window.BackendTypeFilter) {
                        BackendTypeFilter.setCurrentType(filterType);
                    }
                    // Обновить текущую страницу
                    refreshCurrentPage();
                });
            });
        });

        // Agents page buttons
        var refreshAgentsBtn = document.getElementById('refreshAgentsBtn');
        if (refreshAgentsBtn) {
            refreshAgentsBtn.addEventListener('click', function() {
                fetchAgents();
            });
        }
        var closeAgentDetailsBtn = document.getElementById('closeAgentDetails');
        if (closeAgentDetailsBtn) {
            closeAgentDetailsBtn.addEventListener('click', function() {
                var card = document.getElementById('agentDetailsCard');
                if (card) card.style.display = 'none';
            });
        }

        // Models Manage button on Models page
        var modelsManageBtn = document.getElementById('modelsManageBtn');
        if (modelsManageBtn) {
            modelsManageBtn.addEventListener('click', function() {
                if (data.backends && data.backends.length > 0) {
                    openModelManageModal(data.backends[0].id);
                } else {
                    showToast(window.I18N ? I18N.t('models.no_backends') : 'No backends available', 'error');
                }
            });
        }

        // Models search/filter
        var modelsSearch = document.getElementById('modelsSearch');
        if (modelsSearch) {
            // Session B: восстанавливаем последний query из localStorage.
            try {
                var saved = localStorage.getItem('ollamalegion_models_search');
                if (saved) modelsSearch.value = saved;
            } catch (e) { /* ignore */ }
            modelsSearch.addEventListener('input', Utils.debounce(function(e) {
                filterModels(e.target.value);
            }, 150));
        }

        // Models backend-type filter (🦙/🦒/All)
        var modelsTypeFilter = document.getElementById('modelsTypeFilter');
        if (modelsTypeFilter) {
            var filterButtons = modelsTypeFilter.querySelectorAll('.filter-btn');
            filterButtons.forEach(function(btn) {
                btn.addEventListener('click', function() {
                    filterButtons.forEach(function(b) { b.classList.remove('active'); });
                    btn.classList.add('active');
                    var currentSearch = modelsSearch ? modelsSearch.value : '';
                    filterModels(currentSearch);
                });
            });
        }

        // Models sort dropdown
        var modelsSortBy = document.getElementById('modelsSortBy');
        if (modelsSortBy) {
            modelsSortBy.addEventListener('change', function() {
                applyModelsSort();
            });
        }

        // Models refresh button
        var refreshModelsBtn = document.getElementById('refreshModelsBtn');
        if (refreshModelsBtn) {
            refreshModelsBtn.addEventListener('click', function() {
                fetchClusterState().then(function() {
                    modelsPage(data.backends);
                    if (window.bulkModels && typeof window.bulkModels.renderToolbar === 'function') {
                        window.bulkModels.renderToolbar();
                    }
                    showToast(window.I18N ? I18N.t('common.success') : 'Models refreshed', 'success');
                });
            });
        }

        // Model Management Modal buttons
        var modelManageCloseBtn = document.getElementById('modelManageClose');
        if (modelManageCloseBtn) {
            modelManageCloseBtn.addEventListener('click', closeModelManageModal);
        }
        var modelManageCancelBtn = document.getElementById('modelManageCancel');
        if (modelManageCancelBtn) {
            modelManageCancelBtn.addEventListener('click', closeModelManageModal);
        }
        var modelManageModal = document.getElementById('modelManageModal');
        if (modelManageModal) {
            modelManageModal.addEventListener('click', function(e) {
                if (e.target.id === 'modelManageModal') closeModelManageModal();
            });
        }
        var modelPullBtn = document.getElementById('modelPullBtn');
        if (modelPullBtn) {
            modelPullBtn.addEventListener('click', function() {
                var backendId = modelManageModal ? modelManageModal.dataset.backendId : null;
                if (!backendId) {
                    showToast('Backend ID not found', 'error');
                    return;
                }
                var modelName = document.getElementById('modelPullName') ? document.getElementById('modelPullName').value.trim() : '';
                if (!modelName) {
                    showToast(window.I18N ? I18N.t('models.model_name_placeholder') : 'Enter model name', 'error');
                    return;
                }
                var insecure = document.getElementById('modelPullInsecure') ? document.getElementById('modelPullInsecure').checked : false;
                var options = {};
                if (insecure) options.insecure = true;
                executeModelOperation(backendId, 'pull', modelName, options);
            });
        }

        // Proxy logs buttons
        var clearProxyLogsBtn = document.getElementById('clearProxyLogs');
        if (clearProxyLogsBtn) {
            clearProxyLogsBtn.addEventListener('click', function() {
                data.proxyLogs = [];
                renderProxyLogs(data.proxyLogs);
            });
        }
        var refreshProxyLogsBtn = document.getElementById('refreshProxyLogs');
        if (refreshProxyLogsBtn) {
            refreshProxyLogsBtn.addEventListener('click', function() {
                fetchProxyLogs();
            });
        }
        var copyProxyLogsBtn = document.getElementById('copyProxyLogs');
        if (copyProxyLogsBtn) {
            copyProxyLogsBtn.addEventListener('click', function() {
                renderCopyProxyLogs();
            });
        }

        // Backend Limits button
        var saveBackendLimitsBtn = document.getElementById('saveBackendLimitsBtn');
        if (saveBackendLimitsBtn) {
            saveBackendLimitsBtn.addEventListener('click', function() {
                saveBackendLimits();
            });
        }

        // CppWorker port Auto-detect button (форма регистрации бэкенда)
        var cppPortDetectBtn = document.getElementById('formBackendCppWorkerPortDetect');
        if (cppPortDetectBtn) {
            cppPortDetectBtn.addEventListener('click', function () {
                var hostEl = document.getElementById('formBackendHost');
                var portEl = document.getElementById('formBackendCppWorkerPort');
                var host = hostEl ? hostEl.value.trim() : '';
                if (!host) {
                    showToast('Сначала укажите host', 'error');
                    return;
                }
                cppPortDetectBtn.disabled = true;
                var originalText = cppPortDetectBtn.innerHTML;
                cppPortDetectBtn.innerHTML = '⏳ Detecting…';
                autoDetectCppWorkerPort(host).then(function (detected) {
                    cppPortDetectBtn.disabled = false;
                    cppPortDetectBtn.innerHTML = originalText;
                    if (detected) {
                        if (portEl) portEl.value = detected;
                        showToast('Обнаружен порт CppWorker: ' + detected, 'success');
                    } else {
                        showToast('CppWorker не найден на портах 18092/18091/18093/18090. Укажите вручную.', 'error');
                    }
                }).catch(function () {
                    cppPortDetectBtn.disabled = false;
                    cppPortDetectBtn.innerHTML = originalText;
                    showToast('Ошибка auto-detect', 'error');
                });
            });
        }

        // Retake Setup Wizard button (Danger Zone)
        var retakeBtn = document.getElementById('retakeSetupWizardBtn');
        if (retakeBtn) {
            retakeBtn.addEventListener('click', function() {
                var confirmed = confirm(
                    (window.I18N ? I18N.t('wizard.retake_confirm') : 'Все текущие настройки будут сброшены. Продолжить?')
                );
                if (!confirmed) return;

                // Сбрасываем initialized на сервере
                if (window.Api && window.Api.updateConfig) {
                    window.Api.updateConfig({ initialized: false }).then(function () {
                        localStorage.removeItem('ollamalegion_wizard_done');
                        localStorage.removeItem('ollamalegion_backend_type');
                        if (window.SetupWizard) {
                            window.SetupWizard.start();
                        }
                    }).catch(function (err) {
                        showToast(
                            (window.I18N ? I18N.t('common.error') : 'Ошибка') + ': ' + (err.message || err),
                            'error'
                        );
                    });
                } else {
                    localStorage.removeItem('ollamalegion_wizard_done');
                    if (window.SetupWizard) {
                        window.SetupWizard.start();
                    }
                }
            });
        }
    }

    // ---- WebSocket Events ----

    // Round 32 #2 (2026-08-10): Cross-tab sync через BroadcastChannel API.
    // Без этого каждая вкладка polls /api/v1/cluster независимо каждые 5s,
    // и состояние между вкладками может отличаться на 5s. С BroadcastChannel
    // одна вкладка, получившая cluster state change (через WS event от балансера
    // или через REST poll), бродкастит "state-changed" в другие вкладки — они
    // сразу перезагружают своё состояние. UX-выигрыш: когда в одной вкладке
    // нажали "Load model", в другой вкладке GGUF page через ~100ms (вместо 5s)
    // показывает новую модель.
    //
    // Channel name: "ollama-legion-sync" — все вкладки webui в одном origin
    // (одно окно браузера на localhost) делят один BroadcastChannel.
    //
    // Sender tab: после получения cluster state (WS event или REST poll) → post.
    // Receiver tabs: onmessage handler → trigger fetchClusterState() +
    // updateActiveQueries() для GGUF page.
    let _crossTabChannel = null;
    function getCrossTabChannel() {
        if (_crossTabChannel !== null) return _crossTabChannel;
        if (typeof BroadcastChannel === 'undefined') {
            // Старые браузеры (или browser extension contexts) без BroadcastChannel
            // — no-op sync. Tabs обновятся при следующем REST poll (5s).
            _crossTabChannel = false;
            return _crossTabChannel;
        }
        try {
            _crossTabChannel = new BroadcastChannel('ollama-legion-sync');
            _crossTabChannel.onmessage = handleCrossTabMessage;
            return _crossTabChannel;
        } catch (e) {
            // BroadcastChannel может бросить если document не fully loaded,
            // или в Web Worker context. Fallback: no-op.
            _crossTabChannel = false;
            return _crossTabChannel;
        }
    }

    function handleCrossTabMessage(event) {
        // Не обрабатываем свои же сообщения (BroadcastChannel не фильтрует sender).
        if (!event || !event.data) return;
        // Игнорируем сообщения от других origins (защита от shared channel).
        if (event.data.source && event.data.source !== 'ollama-legion-webui') return;
        const data = event.data;
        if (data.type === 'clusterStateChanged') {
            // Cluster state изменился — обновляем локальное состояние.
            // fetchClusterState() дёргает REST API напрямую (быстрее, чем ждать
            // следующего 5s poll).
            fetchClusterState();
            // GGUF page имеет отдельный polling active-queries (3s) — дёргаем его
            // тоже, чтобы busy badge обновился немедленно.
            window.refreshActiveQueriesCrossTab();
        } else if (data.type === 'generationCancelled') {
            // Другая вкладка отменила generation — refresh busy badge.
            window.refreshActiveQueriesCrossTab();
        }
    }

    function broadcastCrossTab(type, extra) {
        const ch = getCrossTabChannel();
        if (!ch || ch === false) return; // no-op для браузеров без BroadcastChannel
        try {
            ch.postMessage(Object.assign({ source: 'ollama-legion-webui', type: type }, extra || {}));
        } catch (e) {
            // Пост может бросить если channel closed. Не критично — log и продолжить.
            console.debug('broadcastCrossTab failed:', e);
        }
    }

    // Expose cross-tab helpers в window для доступа из gguf-renderer.js и
    // других модулей. Single source of truth — broadcastCrossTab определена
    // здесь, в app.js, и все модули используют window.broadcastCrossTab.
    window.broadcastCrossTab = broadcastCrossTab;
    window.refreshActiveQueriesCrossTab = function () {
        // Helper для cross-tab sync — force refresh busy badge когда
        // другая вкладка отменила generation.
        if (window.GgufRenderer && typeof window.GgufRenderer.refreshActiveQueriesPolling === 'function') {
            window.GgufRenderer.refreshActiveQueriesPolling();
        }
    };

    function setupWebSocketEvents() {
        window.addEventListener('ws-open', () => {
            updateConnectionStatus(true);
            addLog(window.I18N ? I18N.t('app.ws_connected') : 'WebSocket connected', 'info');
        });

        window.addEventListener('ws-status', (e) => {
            updateConnectionStatus(e.detail.connected);
        });

        window.addEventListener('ws-error', () => {
            updateConnectionStatus(false);
            addLog(window.I18N ? I18N.t('app.ws_error') : 'WebSocket error', 'error');
        });

        window.addEventListener('ws-reconnecting', (e) => {
            const { attempt, max, delay } = e.detail;
            addLog(window.I18N ? I18N.t('app.ws_reconnect', { attempt: attempt, max: max, delay: Math.round(delay / 1000) }) : `Reconnecting... (${attempt}/${max}) in ${Math.round(delay / 1000)}s`, 'warn');
        });

        window.addEventListener('ws-max-reconnect', () => {
            addLog(window.I18N ? I18N.t('app.ws_max_reconnect') : 'Max reconnection attempts reached', 'error');
        });

        window.addEventListener('ws-message', (e) => {
            handleWebSocketData(e.detail);
        });

        WebSocketManager.connect();
    }

    function handleWebSocketData(payload) {
        const eventType = payload.eventType || 'legacy';

        switch (eventType) {
            case 'clusterState':
                updateBackends(payload.data?.backends || []);
                break;
            case 'backendAdd':
                addLog(window.I18N ? I18N.t('app.backend_added', { name: payload.data?.name || payload.backendId }) : `Backend added: ${payload.data?.name || payload.backendId}`, 'info');
                fetchClusterState();
                break;
            case 'backendRemove':
                addLog(window.I18N ? I18N.t('app.backend_removed', { id: payload.backendId }) : `Backend removed: ${payload.backendId}`, 'info');
                fetchClusterState();
                break;
            case 'statusChange':
                addLog(window.I18N ? I18N.t('app.status_changed', { id: payload.backendId, old: payload.data?.oldStatus, new: payload.data?.newStatus }) : `Status ${payload.backendId}: ${payload.data?.oldStatus} \u2192 ${payload.data?.newStatus}`, 'warning');
                applyStatusChange(payload.backendId, payload.data?.newStatus);
                break;
            case 'limitsChange':
                addLog(window.I18N ? I18N.t('app.limits_changed', { id: payload.backendId }) : `Limits ${payload.backendId} updated`, 'info');
                fetchClusterState();
                break;
            case 'proxy_log':
                if (payload.data?.entry) {
                    addProxyLog(payload.data.entry);
                }
                break;
            case 'ping':
                break;
            case 'legacy':
            default:
                if (payload.backends) {
                    updateBackends(payload.backends || []);
                }
                break;
        }
    }

    function backendsEqual(a, b) {
        if (a.length !== b.length) return false;
        const normalize = arr => JSON.stringify(arr.map(x => ({
            id: x.id,
            status: x.status,
            activeRequests: x.activeRequests,
            'gpu.usagePercent': x.gpu?.usagePercent,
            'gpu.memoryUsed': x.gpu?.memoryUsed,
            'system.cpuUsagePercent': x.system?.cpuUsagePercent,
            'system.memoryUsed': x.system?.memoryUsed,
            'prediction.secondsToCritical': x.prediction?.secondsToCritical
        })).sort((m, n) => m.id.localeCompare(n.id)));
        return normalize(a) === normalize(b);
    }

    function updateBackends(newBackends) {
        newBackends = [...newBackends].sort((a, b) => (a.id || '').localeCompare(b.id || ''));
        if (backendsEqual(data.backends, newBackends)) return;
        data.backends = newBackends;
        if (currentPage === 'dashboard') scheduleDashboardRender();
        // Round 32 #2 (2026-08-10): broadcast cluster state change для cross-tab sync.
        // Если backends действительно изменились (backendsEqual=false), уведомляем
        // другие вкладки чтобы они перезагрузили своё состояние. Дедупликация
        // через backendsEqual выше гарантирует что broadcast не шлётся каждый
        // poll (только при реальных изменениях).
        broadcastCrossTab('clusterStateChanged');
    }

    function applyStatusChange(backendId, newStatus) {
        const b = data.backends.find(x => x.id === backendId);
        if (b && b.status !== newStatus) {
            b.status = newStatus;
            if (currentPage === 'dashboard') scheduleDashboardRender();
        } else if (!b) {
            fetchClusterState();
        }
    }

    function scheduleDashboardRender() {
        if (dashboardRenderTimer) clearTimeout(dashboardRenderTimer);
        dashboardRenderTimer = setTimeout(() => {
            dashboardRenderTimer = null;
            refreshPage('dashboard');
        }, 250);
    }

    // ---- API Events ----

    function setupApiEvents() {
        window.addEventListener('api-error', (e) => {
            const { message, error } = e.detail;
            showToast(message, 'error');
            addLog(message, 'error');
            // If it's a network error or the balancer is unreachable, mark as disconnected
            if (message && (message.includes('Network error') || message.includes('Failed to fetch') || message.includes('NetworkError'))) {
                updateConnectionStatus(false);
            }
        });
    }

    // ---- Data Fetching ----

    async function fetchClusterState() {
        try {
            const state = await Api.cluster();
            updateBackends(state.backends || []);
            // Successful REST request — balancer is reachable
            updateConnectionStatus(true);
            // Синхронизация типа бэкенда (Ollama vs llama.cpp) — скрывает/показывает вкладку GGUF и режимы
            if (window.BackendTypeFilter) {
                BackendTypeFilter.syncFromClusterState(state);
            }
            // Бейдж уже обновлён внутри BackendTypeFilter.syncFromClusterState → updateUI → updateEngineBadge
        } catch (e) {
            Api.handleError(e, window.I18N ? I18N.t('app.error_loading_cluster') : 'Error loading cluster state');
        }
    }

    async function fetchQueue() {
        try {
            data.queue = await Api.queueStats();
            Utils.setText('queueSize', data.queue.current_size || 0);
            Utils.setText('queueProcessed', (data.queue.processed_total || 0) + ' ' + (window.I18N ? I18N.t('app.processed') : 'processed'));
            if (currentPage === 'queue') refreshPage('queue');
        } catch (e) {
            Api.handleError(e, window.I18N ? I18N.t('app.error_loading_queue') : 'Error loading queue statistics');
        }
    }

    async function fetchQueueDetails() {
        try {
            const res = await Api.queueDetails();
            const all = res.all || [];
            data.queueTasks = all.map((item, idx) => ({
                id: idx + 1,
                model: item.model || '-',
                backend: item.target || 'Auto',
                status: item.status || (item.target ? 'processing' : 'pending'),
                waitTimeMs: item.enqueued ? (Date.now() - new Date(item.enqueued).getTime()) : 0,
                enqueued: item.enqueued
            }));
            if (currentPage === 'queue') refreshPage('queue');
        } catch (e) {
            Api.handleError(e, window.I18N ? I18N.t('app.error_loading_queue_details') : 'Error loading queue details');
            data.queueTasks = [];
            if (currentPage === 'queue') refreshPage('queue');
        }
    }

    async function fetchQueueHistory() {
        try {
            const res = await Api.queueHistory();
            data.queueHistory = res.history || [];
            if (currentPage === 'queue') refreshPage('queue');
        } catch (e) {
            Api.handleError(e, window.I18N ? I18N.t('app.error_loading_queue_history') : 'Error loading queue history');
            data.queueHistory = [];
            if (currentPage === 'queue') refreshPage('queue');
        }
    }

    async function fetchSessions() {
        try {
            const res = await Api.sessions();
            data.sessions = res.sessions || [];
            const activeCount = data.sessions.length;
            const totalRequests = data.sessions.reduce((sum, s) => sum + (s.requestCount || 0), 0);
            Utils.setText('totalSessions', activeCount);
            Utils.setText('sessionRate', totalRequests + ' ' + (window.I18N ? I18N.t('app.requests') : 'requests'));
            if (currentPage === 'sessions') refreshPage('sessions');
        } catch (e) {
            Api.handleError(e, window.I18N ? I18N.t('app.error_loading_sessions') : 'Error loading sessions');
        }
    }

    /**
     * Сброс настроек к заводским значениям с сохранением типа бэкенда (Ollama / llama.cpp).
     * Показывает модальное окно подтверждения, при успехе перезагружает страницу.
     */
    function resetSettings() {
        // Показываем модальное окно подтверждения
        var confirmed = confirm(
            (window.I18N ? I18N.t('settings.reset_confirm') : 'Сбросить все настройки к заводским значениям? Это действие нельзя отменить.')
        );
        if (!confirmed) return;

        Api.resetConfig().then(function (result) {
            showToast(
                (window.I18N ? I18N.t('settings.reset_success') : 'Настройки сброшены. Страница будет перезагружена.'),
                'success'
            );
            // Сбрасываем локальные ключи, связанные с типом бэкенда и wizard
            localStorage.removeItem('ollamalegion_backend_type');
            localStorage.removeItem('ollamalegion_wizard_done');
            // Перезагрузка через небольшую задержку, чтобы пользователь увидел toast
            setTimeout(function () {
                location.reload();
            }, 1500);
        }).catch(function (err) {
            showToast(
                (window.I18N ? I18N.t('settings.reset_error') : 'Ошибка сброса: ') + (err.message || err),
                'error'
            );
        });
    }

    function loadSettings() {
        // --- SERVER-FIRST RULE: сервер = источник истины ---
        // Сначала пытаемся загрузить с сервера. Если не получилось — fallback в localStorage.
        Api.config().then(function (serverConfig) {
            applyServerConfig(serverConfig);
            // Запоминаем время последней синхронизации
            localStorage.setItem('ollamalegion_last_sync', Date.now().toString());
        }).catch(function (err) {
            console.warn('Server config unavailable, falling back to localStorage', err);
            // Fallback: загружаем из localStorage
            var saved = localStorage.getItem('ollamalegion_config');
            if (saved) {
                try {
                    var config = JSON.parse(saved);
                    applyLocalConfig(config);
                } catch (e) {
                    console.error('Failed to parse localStorage config', e);
                }
            }
        });
    }

    function applyServerConfig(serverConfig) {
        if (!serverConfig) return;

        // ЗАЩИТА: не перезаписываем UI если wizard активен
        if (document.getElementById('setupWizardModal')) return;

        var algEl = document.getElementById('balancingAlgorithm');
        if (algEl && serverConfig.algorithm) algEl.value = serverConfig.algorithm;
        var maEl = document.getElementById('modelAffinity');
        if (maEl) maEl.checked = serverConfig.modelAffinity !== false;
        var ssEl = document.getElementById('sessionStickiness');
        if (ssEl) ssEl.checked = serverConfig.sessionStickiness !== false;

        var useESEl = document.getElementById('useEnhancedScoring');
        if (useESEl) useESEl.checked = serverConfig.useEnhancedScoring !== false;

        var pfEl = document.getElementById('predictionFiltering');
        if (pfEl) pfEl.checked = serverConfig.predictionFiltering !== false;

        var gpuEl = document.getElementById('gpuMaxUsage');
        if (gpuEl && serverConfig.gpuMaxUsage) gpuEl.value = serverConfig.gpuMaxUsage;
        var vramEl = document.getElementById('vramMaxUsage');
        if (vramEl && serverConfig.vramMaxUsage) vramEl.value = serverConfig.vramMaxUsage;
        var cpuEl = document.getElementById('cpuMaxUsage');
        if (cpuEl && serverConfig.cpuMaxUsage) cpuEl.value = serverConfig.cpuMaxUsage;
        var ramEl = document.getElementById('ramMaxUsage');
        if (ramEl && serverConfig.ramMaxUsage) ramEl.value = serverConfig.ramMaxUsage;
        var diskEl = document.getElementById('minFreeDisk');
        if (diskEl && serverConfig.minFreeDisk) diskEl.value = serverConfig.minFreeDisk;
        var tokenEl = document.getElementById('apiToken');
        if (tokenEl && serverConfig.apiToken) tokenEl.value = serverConfig.apiToken;

        // Backend Engine — серверный конфиг как fallback.
        // Приоритет: localStorage (выбор пользователя) > серверный config.
        if (serverConfig.backendEngine) {
            var engineType = (serverConfig.backendEngine === 'llama_cpp') ? 'llama_cpp' : 'ollama';
            var currentType = window.BackendTypeFilter ? BackendTypeFilter.getCurrentType() : null;
            // Если пользователь уже выбрал тип — не перезаписываем UI из сервера
            if (currentType !== 'llama_cpp' && currentType !== 'ollama') {
                if (window.BackendTypeFilter) {
                    BackendTypeFilter.updateUI(engineType);
                }
            }
            var formBackendTypeEl = document.getElementById('formBackendType');
            if (formBackendTypeEl) {
                formBackendTypeEl.value = engineType;
            }
            // Обновляем маркер типа движка в сайдбаре (текст + иконка)
            var badge = document.getElementById('backendEngineBadge');
            var label = document.getElementById('backendEngineLabel');
            var iconEl = badge ? badge.querySelector('.engine-icon') : null;
            if (badge) {
                badge.classList.remove('engine-ollama', 'engine-llama_cpp', 'engine-auto');
                badge.classList.add('engine-' + engineType);
            }
            if (label) {
                label.textContent = engineType === 'llama_cpp' ? '🦒 llama.cpp' : '🦙 Ollama API';
            }
            if (iconEl) {
                iconEl.textContent = engineType === 'llama_cpp' ? '🦒' : '🦙';
            }
        }

        // Operating Mode — критически важно для корректного отображения UI.
        // Сервер = единственный источник истины. Перезаписываем UI жёстко.
        if (serverConfig.operatingMode) {
            if (window.SettingsUI && SettingsUI.syncModeFromServer) {
                SettingsUI.syncModeFromServer(serverConfig.operatingMode);
            } else {
                // Fallback (устаревший путь)
                var radio = document.querySelector('input[name="operatingMode"][value="' + serverConfig.operatingMode + '"]');
                if (radio) radio.checked = true;
            }
        }

        // RPC Settings — Model Replication (Вариант A)
        var mrEnabledEl = document.getElementById('modelReplicationEnabled');
        if (mrEnabledEl && serverConfig.modelReplication) mrEnabledEl.checked = serverConfig.modelReplication.enabled === true;
        var mrMinEl = document.getElementById('modelReplicationMinInstances');
        if (mrMinEl && serverConfig.modelReplication && serverConfig.modelReplication.defaultMinInstances) mrMinEl.value = serverConfig.modelReplication.defaultMinInstances;
        var mrMaxEl = document.getElementById('modelReplicationMaxInstances');
        if (mrMaxEl && serverConfig.modelReplication && serverConfig.modelReplication.defaultMaxInstances) mrMaxEl.value = serverConfig.modelReplication.defaultMaxInstances;
        var mrIdleEl = document.getElementById('modelReplicationIdleUnload');
        if (mrIdleEl && serverConfig.modelReplication && serverConfig.modelReplication.idleUnloadAfter) mrIdleEl.value = serverConfig.modelReplication.idleUnloadAfter;

        // RPC Settings — RPC Coordinator (Вариант B)
        var rcEnabledEl = document.getElementById('rpcCoordinatorEnabled');
        if (rcEnabledEl && serverConfig.rpcCoordinator) rcEnabledEl.checked = serverConfig.rpcCoordinator.enabled === true;
        var rcUrlEl = document.getElementById('rpcCoordinatorURL');
        if (rcUrlEl && serverConfig.rpcCoordinator && serverConfig.rpcCoordinator.coordinatorURL) rcUrlEl.value = serverConfig.rpcCoordinator.coordinatorURL;
        var rcPortEl = document.getElementById('rpcCoordinatorWorkerPort');
        if (rcPortEl && serverConfig.rpcCoordinator && serverConfig.rpcCoordinator.workerPort) rcPortEl.value = serverConfig.rpcCoordinator.workerPort;
        var rcProtoEl = document.getElementById('rpcCoordinatorProtocol');
        if (rcProtoEl && serverConfig.rpcCoordinator && serverConfig.rpcCoordinator.protocol) rcProtoEl.value = serverConfig.rpcCoordinator.protocol;
        var rcTimeoutEl = document.getElementById('rpcCoordinatorTimeout');
        if (rcTimeoutEl && serverConfig.rpcCoordinator && serverConfig.rpcCoordinator.timeout) rcTimeoutEl.value = serverConfig.rpcCoordinator.timeout;

        // RPC Settings — Virtual Models (Вариант C)
        var vmEnabledEl = document.getElementById('virtualModelsEnabled');
        if (vmEnabledEl && serverConfig.virtualModels) vmEnabledEl.checked = serverConfig.virtualModels.enabled === true;
        var vmModeEl = document.getElementById('virtualModelsCoordMode');
        if (vmModeEl && serverConfig.virtualModels && serverConfig.virtualModels.coordMode) vmModeEl.value = serverConfig.virtualModels.coordMode;
        var vmTimeoutEl = document.getElementById('virtualModelsTimeout');
        if (vmTimeoutEl && serverConfig.virtualModels && serverConfig.virtualModels.timeout) vmTimeoutEl.value = serverConfig.virtualModels.timeout;

        // RPC Settings — Distributed Inference (Вариант D)
        var diEnabledEl = document.getElementById('distInferenceEnabled');
        if (diEnabledEl && serverConfig.distInference) diEnabledEl.checked = serverConfig.distInference.enabled === true;
        var diPortEl = document.getElementById('distInferenceGrpcPort');
        if (diPortEl && serverConfig.distInference && serverConfig.distInference.grpcPort) diPortEl.value = serverConfig.distInference.grpcPort;

        // llama.cpp / GGUF Settings
        var llamaCpp = serverConfig.llamaCpp || {};
        // Хелпер для безопасной установки value (поддерживает "0" как валидное значение).
        var setVal = function (id, val, fallback) {
            var el = document.getElementById(id);
            if (!el) return;
            if (val === undefined || val === null) {
                if (fallback !== undefined) el.value = fallback;
                return;
            }
            el.value = val;
        };
        var setCheck = function (id, val, fallback) {
            var el = document.getElementById(id);
            if (!el) return;
            if (val === undefined || val === null) {
                if (fallback !== undefined) el.checked = !!fallback;
                return;
            }
            el.checked = !!val;
        };
        // Базовые параметры
        setVal('gpuLayers', llamaCpp.numGpuLayers, -1);
        setVal('ctxSize', llamaCpp.contextLength, 2048);
        setVal('batchSize', llamaCpp.batchSize, 512);
        setVal('flashAttnType', llamaCpp.flashAttnType, -1);
        setVal('nThreads', llamaCpp.nThreads, 0);
        setCheck('flashAttn', llamaCpp.flashAttention, false);
        setCheck('numa', llamaCpp.numa, false);
        setCheck('useMmap', llamaCpp.useMmap, true);
        setCheck('useMlock', llamaCpp.useMlock, false);
        // Multi-GPU
        setVal('gpuStrategy', llamaCpp.strategy, 'vram-ratio');
        if (llamaCpp.tensorSplitStr) {
            setVal('tensorSplit', llamaCpp.tensorSplitStr, '');
        } else if (llamaCpp.tensorSplit && Array.isArray(llamaCpp.tensorSplit)) {
            setVal('tensorSplit', llamaCpp.tensorSplit.join(','), '');
        } else {
            setVal('tensorSplit', '', '');
        }
        setVal('splitMode', llamaCpp.splitMode, -1);
        setVal('mainGpu', llamaCpp.mainGpu, 0);
        setVal('rpcBackend', llamaCpp.rpcBackend, '');
        setCheck('autoGpuDistribution', llamaCpp.autoGpuDistribution, true);
        setCheck('noMemoryMap', llamaCpp.noMemoryMap, false);
        // KV cache
        setVal('kvCacheType', llamaCpp.kvCacheType, '');
        setCheck('noKvOffload', llamaCpp.noKvOffload, false);
        // RoPE/YaRN
        setVal('ropeFreqBase', llamaCpp.ropeFreqBase, 10000);
        setVal('ropeFreqScale', llamaCpp.ropeFreqScale, 1.0);
        setVal('ropeScalingType', llamaCpp.ropeScalingType, 'none');
        setVal('ropeScalingFactor', llamaCpp.ropeScalingFactor, 1.0);
        setVal('yarnExtFactor', llamaCpp.yarnExtFactor, 1.0);
        setVal('yarnAttnFactor', llamaCpp.yarnAttnFactor, 1.0);
        setVal('yarnBetaFast', llamaCpp.yarnBetaFast, 32.0);
        setVal('yarnBetaSlow', llamaCpp.yarnBetaSlow, 1.0);
        // Performance
        setVal('rmsNormEps', llamaCpp.rmsNormEps, 0.00001);
        setVal('idleUnloadMinutes', llamaCpp.idleUnloadMinutes, 0);
        setCheck('enableMetrics', llamaCpp.enableMetrics, true);
        setVal('metricsRetention', llamaCpp.metricsRetentionSeconds, 3600);
        // Session 18: Reasoning/Thinking
        setCheck('enableReasoning', llamaCpp.enableReasoning, false);
        setVal('reasoningBudget', llamaCpp.reasoningBudget, 0);
        // Session 17: показать/скрыть panel "Runtime overrides active" в зависимости
        // от наличия sidecar-файла. Делаем ПОСЛЕ setVal чтобы DOM был готов.
        updateLlamaCppOverridesPanel();
    }

    /**
     * Session 17 (2026-07-27): проверяет, есть ли активный sidecar override
     * для llamaCpp-секции, и показывает/скрывает panel с кнопкой "Reset".
     *
     * Вызывается:
     *   - После applyServerConfig (когда UI подгружен значениями)
     *   - После saveSettings (после PUT, чтобы UI обновился)
     *   - После resetLlamaCppOverride (чтобы скрыть panel)
     */
    function updateLlamaCppOverridesPanel() {
        if (!window.Api || !window.Api.getLlamaCppOverride) return;
        const panel = document.getElementById('llamaCppRuntimeOverridesPanel');
        if (!panel) return;
        window.Api.getLlamaCppOverride()
            .then(function (resp) {
                if (resp && resp.exists) {
                    panel.style.display = '';
                } else {
                    panel.style.display = 'none';
                }
            })
            .catch(function () {
                // Store отключён (например, dev-режим) или ошибка сети — скрываем.
                panel.style.display = 'none';
            });
    }

    /**
     * Session 17: handler кнопки "Reset to bundled defaults".
     * Стирает sidecar override и перезагружает UI с base значениями из config.json.
     */
    function resetLlamaCppOverride() {
        if (!window.Api || !window.Api.deleteLlamaCppOverride) {
            showToast('Reset API not available', 'error');
            return;
        }
        const confirmMsg = window.I18N
            ? I18N.t('gguf.overrides_confirm_reset', 'Reset runtime overrides? llamaCpp will revert to bundled defaults from config.json.')
            : 'Reset runtime overrides? llamaCpp will revert to bundled defaults from config.json.';
        if (!confirm(confirmMsg)) return;
        const btn = document.getElementById('llamaCppResetBtn');
        if (btn) btn.disabled = true;
        window.Api.deleteLlamaCppOverride()
            .then(function (resp) {
                const successMsg = window.I18N
                    ? I18N.t('gguf.overrides_reset_done', 'Runtime overrides cleared. llamaCpp reverted to bundled defaults.')
                    : 'Runtime overrides cleared. llamaCpp reverted to bundled defaults.';
                showToast(successMsg, 'success');
                // Перезагружаем UI с сервера (in-memory уже восстановлен).
                return Api.config().then(applyServerConfig);
            })
            .catch(function (err) {
                const errMsg = (window.I18N ? I18N.t('common.error') : 'Error') + ': ' +
                    (err.message || err);
                showToast(errMsg, 'error');
            })
            .then(function () {
                if (btn) btn.disabled = false;
            });
    }

    function applyLocalConfig(config) {
        if (!config) return;
        var algEl = document.getElementById('balancingAlgorithm');
        if (algEl && config.algorithm) algEl.value = config.algorithm;
        var maEl = document.getElementById('modelAffinity');
        if (maEl) maEl.checked = config.modelAffinity !== false;
        var ssEl = document.getElementById('sessionStickiness');
        if (ssEl) ssEl.checked = config.sessionStickiness !== false;
        var pfEl = document.getElementById('predictionFiltering');
        if (pfEl) pfEl.checked = config.predictionFiltering !== false;
        var gpuEl = document.getElementById('gpuMaxUsage');
        if (gpuEl && config.gpuMaxUsage) gpuEl.value = config.gpuMaxUsage;
        var vramEl = document.getElementById('vramMaxUsage');
        if (vramEl && config.vramMaxUsage) vramEl.value = config.vramMaxUsage;
        var cpuEl = document.getElementById('cpuMaxUsage');
        if (cpuEl && config.cpuMaxUsage) cpuEl.value = config.cpuMaxUsage;
        var ramEl = document.getElementById('ramMaxUsage');
        if (ramEl && config.ramMaxUsage) ramEl.value = config.ramMaxUsage;
        var diskEl = document.getElementById('minFreeDisk');
        if (diskEl && config.minFreeDisk) diskEl.value = config.minFreeDisk;
        var tokenEl = document.getElementById('apiToken');
        if (tokenEl && config.apiToken) tokenEl.value = config.apiToken;

        // Operating Mode
        if (config.operatingMode) {
            var radio = document.querySelector('input[name="operatingMode"][value="' + config.operatingMode + '"]');
            if (radio) {
                radio.checked = true;
                if (window.SettingsUI) {
                    SettingsUI.updateModeCards(config.operatingMode);
                    SettingsUI.showModeFields(config.operatingMode);
                }
            }
        }

        if (config.agent) {
            var ag = config.agent;
            var agentCollEl = document.getElementById('agentCollectInterval');
            if (agentCollEl && ag.collectInterval) agentCollEl.value = ag.collectInterval;
            var agentHbEl = document.getElementById('agentHeartbeatInterval');
            if (agentHbEl && ag.heartbeatInterval) agentHbEl.value = ag.heartbeatInterval;
            var agentMcEl = document.getElementById('agentMaxConcurrent');
            if (agentMcEl && ag.maxConcurrentRequests) agentMcEl.value = ag.maxConcurrentRequests;
            var agentMmEl = document.getElementById('agentMaxModels');
            if (agentMmEl && ag.maxModels) agentMmEl.value = ag.maxModels;
            var agentToEl = document.getElementById('agentTimeout');
            if (agentToEl && ag.timeout) agentToEl.value = ag.timeout;
        }
    }

    function saveSettings(silent) {
        var algorithm = (document.getElementById('balancingAlgorithm') && document.getElementById('balancingAlgorithm').value) || 'resource-aware';
        var useEnhancedScoring = (document.getElementById('useEnhancedScoring') && document.getElementById('useEnhancedScoring').checked) !== false;
        var modelAffinity = (document.getElementById('modelAffinity') && document.getElementById('modelAffinity').checked) !== false;
        var sessionStickiness = (document.getElementById('sessionStickiness') && document.getElementById('sessionStickiness').checked) !== false;
        var predictionFiltering = (document.getElementById('predictionFiltering') && document.getElementById('predictionFiltering').checked) !== false;
        var gpuMax = parseInt((document.getElementById('gpuMaxUsage') && document.getElementById('gpuMaxUsage').value)) || 90;
        var vramMax = parseInt((document.getElementById('vramMaxUsage') && document.getElementById('vramMaxUsage').value)) || 85;
        var cpuMax = parseInt((document.getElementById('cpuMaxUsage') && document.getElementById('cpuMaxUsage').value)) || 80;
        var ramMax = parseInt((document.getElementById('ramMaxUsage') && document.getElementById('ramMaxUsage').value)) || 85;
        var minDisk = parseInt((document.getElementById('minFreeDisk') && document.getElementById('minFreeDisk').value)) || 10240;
        var apiToken = (document.getElementById('apiToken') && document.getElementById('apiToken').value) || '';

        // RPC settings
        var modelReplicationEnabled = document.getElementById('modelReplicationEnabled') ? document.getElementById('modelReplicationEnabled').checked : false;
        var modelReplicationMinInstances = parseInt((document.getElementById('modelReplicationMinInstances') && document.getElementById('modelReplicationMinInstances').value)) || 1;
        var modelReplicationMaxInstances = parseInt((document.getElementById('modelReplicationMaxInstances') && document.getElementById('modelReplicationMaxInstances').value)) || 3;
        var modelReplicationIdleUnload = (document.getElementById('modelReplicationIdleUnload') && document.getElementById('modelReplicationIdleUnload').value) || '10m';

        var rpcCoordinatorEnabled = document.getElementById('rpcCoordinatorEnabled') ? document.getElementById('rpcCoordinatorEnabled').checked : false;
        var rpcCoordinatorURL = (document.getElementById('rpcCoordinatorURL') && document.getElementById('rpcCoordinatorURL').value) || '';
        var rpcCoordinatorWorkerPort = parseInt((document.getElementById('rpcCoordinatorWorkerPort') && document.getElementById('rpcCoordinatorWorkerPort').value)) || 18050;
        var rpcCoordinatorProtocol = (document.getElementById('rpcCoordinatorProtocol') && document.getElementById('rpcCoordinatorProtocol').value) || 'http';
        var rpcCoordinatorTimeout = (document.getElementById('rpcCoordinatorTimeout') && document.getElementById('rpcCoordinatorTimeout').value) || '30s';

        var virtualModelsEnabled = document.getElementById('virtualModelsEnabled') ? document.getElementById('virtualModelsEnabled').checked : false;
        var virtualModelsCoordMode = (document.getElementById('virtualModelsCoordMode') && document.getElementById('virtualModelsCoordMode').value) || 'sequential';
        var virtualModelsTimeout = parseInt((document.getElementById('virtualModelsTimeout') && document.getElementById('virtualModelsTimeout').value)) || 30000;

        var distInferenceEnabled = document.getElementById('distInferenceEnabled') ? document.getElementById('distInferenceEnabled').checked : false;
        var distInferenceGrpcPort = parseInt((document.getElementById('distInferenceGrpcPort') && document.getElementById('distInferenceGrpcPort').value)) || 19000;

        // Agent settings
        var agentCollectInterval = parseInt((document.getElementById('agentCollectInterval') && document.getElementById('agentCollectInterval').value)) || 15;
        var agentHeartbeatInterval = parseInt((document.getElementById('agentHeartbeatInterval') && document.getElementById('agentHeartbeatInterval').value)) || 30;
        var agentMaxConcurrent = parseInt((document.getElementById('agentMaxConcurrent') && document.getElementById('agentMaxConcurrent').value)) || 10;
        var agentMaxModels = parseInt((document.getElementById('agentMaxModels') && document.getElementById('agentMaxModels').value)) || 5;
        var agentTimeout = parseInt((document.getElementById('agentTimeout') && document.getElementById('agentTimeout').value)) || 10;

        // llama.cpp / GGUF settings (имена полей соответствуют Go-структуре LlamaCppConfig)
        // Хелпер: безопасный int (0 = валидное значение, NaN → fallback).
        var intVal = function (id, fallback) {
            var el = document.getElementById(id);
            if (!el) return fallback;
            var n = parseInt(el.value, 10);
            return Number.isFinite(n) ? n : fallback;
        };
        var floatVal = function (id, fallback) {
            var el = document.getElementById(id);
            if (!el) return fallback;
            var n = parseFloat(el.value);
            return Number.isFinite(n) ? n : fallback;
        };
        var checkVal = function (id, fallback) {
            var el = document.getElementById(id);
            if (!el) return fallback;
            return el.checked;
        };
        var strVal = function (id, fallback) {
            var el = document.getElementById(id);
            if (!el) return fallback;
            return el.value;
        };
        // Базовые
        var llamaCppGpuLayers = intVal('gpuLayers', -1);
        var llamaCppCtxSize = intVal('ctxSize', 2048);
        var llamaCppBatchSize = intVal('batchSize', 512);
        var llamaCppFlashAttnType = intVal('flashAttnType', -1);
        var llamaCppNThreads = intVal('nThreads', 0);
        var llamaCppFlashAttn = checkVal('flashAttn', false);
        var llamaCppNuma = checkVal('numa', false);
        var llamaCppUseMmap = checkVal('useMmap', true);
        var llamaCppUseMlock = checkVal('useMlock', false);
        // Multi-GPU
        var llamaCppStrategy = strVal('gpuStrategy', 'vram-ratio');
        var llamaCppTensorSplitStr = strVal('tensorSplit', '');
        var llamaCppSplitMode = intVal('splitMode', -1);
        var llamaCppMainGpu = intVal('mainGpu', 0);
        var llamaCppRpcBackend = strVal('rpcBackend', '');
        var llamaCppAutoGpu = checkVal('autoGpuDistribution', true);
        var llamaCppNoMemoryMap = checkVal('noMemoryMap', false);
        // KV cache
        var llamaCppKvCacheType = strVal('kvCacheType', '');
        var llamaCppNoKvOffload = checkVal('noKvOffload', false);
        // RoPE/YaRN
        var llamaCppRopeFreqBase = floatVal('ropeFreqBase', 10000.0);
        var llamaCppRopeFreqScale = floatVal('ropeFreqScale', 1.0);
        var llamaCppRopeScalingType = strVal('ropeScalingType', 'none');
        var llamaCppRopeScalingFactor = floatVal('ropeScalingFactor', 1.0);
        var llamaCppYarnExtFactor = floatVal('yarnExtFactor', 1.0);
        var llamaCppYarnAttnFactor = floatVal('yarnAttnFactor', 1.0);
        var llamaCppYarnBetaFast = floatVal('yarnBetaFast', 32.0);
        var llamaCppYarnBetaSlow = floatVal('yarnBetaSlow', 1.0);
        // Performance
        var llamaCppRmsNormEps = floatVal('rmsNormEps', 0.00001);
        var llamaCppIdleUnloadMinutes = intVal('idleUnloadMinutes', 0);
        var llamaCppEnableMetrics = checkVal('enableMetrics', true);
        var llamaCppMetricsRetention = intVal('metricsRetention', 3600);
        // Session 18 (2026-07-28): Reasoning/Thinking (gemma-4, deepseek-r1, qwen3-thinking)
        var llamaCppEnableReasoning = checkVal('enableReasoning', false);
        var llamaCppReasoningBudget = intVal('reasoningBudget', 0);

        // Определяем текущий operatingMode из radio-кнопок на странице
        var operatingModeRadio = document.querySelector('input[name="operatingMode"]:checked');
        var operatingMode = operatingModeRadio ? operatingModeRadio.value : 'standard';

        // Определяем текущий backendEngine из BackendTypeFilter (источник истины после мастера)
        var backendEngine = null;
        if (window.BackendTypeFilter) {
            var currentType = BackendTypeFilter.getCurrentType();
            if (currentType === 'llama_cpp') {
                backendEngine = 'llama_cpp';
            } else if (currentType === 'ollama') {
                backendEngine = 'ollama_api';
            }
            // если тип не определён (null/undefined) — не включаем backendEngine в payload,
            // чтобы не перезаписать существующее значение на сервере
        }

        var config = {
            operatingMode: operatingMode,
            agent: {
                collectInterval: agentCollectInterval,
                heartbeatInterval: agentHeartbeatInterval,
                maxConcurrentRequests: agentMaxConcurrent,
                maxModels: agentMaxModels,
                timeout: agentTimeout
            },
            algorithm: algorithm,
            useEnhancedScoring: useEnhancedScoring,
            modelAffinity: modelAffinity,
            sessionStickiness: sessionStickiness,
            predictionFiltering: predictionFiltering,
            gpuMaxUsage: gpuMax,
            vramMaxUsage: vramMax,
            cpuMaxUsage: cpuMax,
            ramMaxUsage: ramMax,
            minFreeDisk: minDisk,
            apiToken: apiToken,
            modelReplication: {
                enabled: modelReplicationEnabled,
                defaultMinInstances: modelReplicationMinInstances,
                defaultMaxInstances: modelReplicationMaxInstances,
                idleUnloadAfter: modelReplicationIdleUnload
            },
            rpcCoordinator: {
                enabled: rpcCoordinatorEnabled,
                coordinatorURL: rpcCoordinatorURL,
                workerPort: rpcCoordinatorWorkerPort,
                protocol: rpcCoordinatorProtocol,
                timeout: rpcCoordinatorTimeout
            },
            virtualModels: {
                enabled: virtualModelsEnabled,
                coordMode: virtualModelsCoordMode,
                timeout: virtualModelsTimeout
            },
            distInference: {
                enabled: distInferenceEnabled,
                grpcPort: distInferenceGrpcPort
            },
            llamaCpp: {
                // Базовые
                numGpuLayers: llamaCppGpuLayers,
                contextLength: llamaCppCtxSize,
                batchSize: llamaCppBatchSize,
                flashAttnType: llamaCppFlashAttnType,
                nThreads: llamaCppNThreads,
                flashAttention: llamaCppFlashAttn,
                numa: llamaCppNuma,
                useMmap: llamaCppUseMmap,
                useMlock: llamaCppUseMlock,
                // Multi-GPU
                strategy: llamaCppStrategy,
                tensorSplitStr: llamaCppTensorSplitStr,
                splitMode: llamaCppSplitMode,
                mainGpu: llamaCppMainGpu,
                rpcBackend: llamaCppRpcBackend,
                autoGpuDistribution: llamaCppAutoGpu,
                noMemoryMap: llamaCppNoMemoryMap,
                // KV cache
                kvCacheType: llamaCppKvCacheType,
                noKvOffload: llamaCppNoKvOffload,
                // RoPE/YaRN
                ropeFreqBase: llamaCppRopeFreqBase,
                ropeFreqScale: llamaCppRopeFreqScale,
                ropeScalingType: llamaCppRopeScalingType,
                ropeScalingFactor: llamaCppRopeScalingFactor,
                yarnExtFactor: llamaCppYarnExtFactor,
                yarnAttnFactor: llamaCppYarnAttnFactor,
                yarnBetaFast: llamaCppYarnBetaFast,
                yarnBetaSlow: llamaCppYarnBetaSlow,
                // Performance / lifecycle
                rmsNormEps: llamaCppRmsNormEps,
                idleUnloadMinutes: llamaCppIdleUnloadMinutes,
                enableMetrics: llamaCppEnableMetrics,
                metricsRetentionSeconds: llamaCppMetricsRetention,
                // Session 18: Reasoning/Thinking
                enableReasoning: llamaCppEnableReasoning,
                reasoningBudget: llamaCppReasoningBudget
            }
        };

        // Добавляем backendEngine только если он определён
        if (backendEngine) {
            config.backendEngine = backendEngine;
        }

        // Конфигурация хранится на сервере, localStorage больше не используется для настроек
        // (только theme и language остаются в localStorage)
        Api.updateConfig(config).then(function () {
            return verifySync(config);
        }).then(function () {
            if (!silent) showToast(window.I18N ? I18N.t('settings.saved') : 'Settings saved', 'success');
        }).catch(function (err) {
            if (!silent) showToast((window.I18N ? I18N.t('common.error') : 'Error') + ': ' + (err.message || err), 'error');
        });
    }

    /**
     * SERVER-FIRST verify: после PUT делаем GET и сравниваем критические поля.
     * При расхождении перезагружаем UI из сервера (источник истины).
     */
    function verifySync(expectedConfig) {
        return Api.config().then(function (serverConfig) {
            // Всегда перезагружаем UI из сервера для гарантии синхронности
            applyServerConfig(serverConfig);

            var mismatches = [];
            if (expectedConfig.algorithm && serverConfig.algorithm !== expectedConfig.algorithm) {
                mismatches.push('algorithm: expected ' + expectedConfig.algorithm + ', got ' + serverConfig.algorithm);
            }
            if (expectedConfig.operatingMode && serverConfig.operatingMode !== expectedConfig.operatingMode) {
                mismatches.push('operatingMode: expected ' + expectedConfig.operatingMode + ', got ' + serverConfig.operatingMode);
            }
            if (expectedConfig.useEnhancedScoring !== undefined && serverConfig.useEnhancedScoring !== expectedConfig.useEnhancedScoring) {
                mismatches.push('useEnhancedScoring: expected ' + expectedConfig.useEnhancedScoring + ', got ' + serverConfig.useEnhancedScoring);
            }
            if (expectedConfig.backendEngine && serverConfig.backendEngine !== expectedConfig.backendEngine) {
                mismatches.push('backendEngine: expected ' + expectedConfig.backendEngine + ', got ' + serverConfig.backendEngine);
            }

            if (mismatches.length > 0) {
                console.warn('[verifySync] Config mismatch detected:', mismatches);
                var msg = (window.I18N ? I18N.t('wizard.error') : 'Config sync failed');
                return Promise.reject(new Error(msg + ': ' + mismatches.join('; ')));
            }
        });
    }

    function autoSaveSettings() {
        if (autoSaveTimer) clearTimeout(autoSaveTimer);
        autoSaveTimer = setTimeout(function () {
            saveSettings(true);
        }, 800);
    }

    // ---- Periodic Refresh ----

    function startPeriodicRefresh() {
        const interval = (window.WEBUI_CONFIG?.REFRESH_INTERVAL || 5000);
        // 2026-06-29: добавляем fetchClusterState() в periodic refresh. Без этого
        // метрики дашборда (totalBackends, healthyBackends и т.д.) обновлялись
        // ТОЛЬКО через WebSocket — если WS не подключён (балансер за прокси,
        // CORS preflight, таймаут рукопожатия), карточки метрик оставались
        // с дефолтом "-" из HTML (webui/index.html:138-139). Periodic REST-poll
        // гарантирует, что метрики обновятся даже при неработающем WS.
        // Также добавляем fetchAgents() для консистентности с in-flight refresh.
        refreshTimer = setInterval(() => {
            fetchClusterState();
            fetchQueue();
            fetchQueueDetails();
            fetchQueueHistory();
            fetchSessions();
        }, interval);
    }

    // ---- Backend CRUD ----

    function openBackendModal(backendId = null) {
        const modal = document.getElementById('backendModal');
        const title = document.getElementById('modalTitle');
        const deleteBtn = document.getElementById('modalDelete');

        if (backendId) {
            // Берём метрики для отображения (есть gpu, prediction)
            const backend = data.backends.find(b => b.id === backendId);
            if (!backend) return;
            title.textContent = window.I18N ? I18N.t('backends.edit') : 'Edit Backend';
            deleteBtn.style.display = 'inline-block';
            // Сначала показываем форму с данными из кластера
            fillForm(backend, true);
            // Асинхронно подгружаем конфиг бэкенда с weight, maxModels и т.д.
            Api.getBackend(backendId).then(function (config) {
                // config — это BackendMetrics с полями Config внутри (weight, maxConcurrentReqs...)
                // или сам Backend-объект с weight
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
            title.textContent = window.I18N ? I18N.t('backends.add') : 'Add Backend';
            deleteBtn.style.display = 'none';
            fillForm(null, false);
        }

        modal.classList.add('active');
    }

    function fillForm(backend, isEdit) {
        document.getElementById('formBackendId').value = backend?.id || '';
        document.getElementById('formBackendId').disabled = isEdit;
        document.getElementById('formBackendName').value = backend?.name || '';
        document.getElementById('formBackendHost').value = backend?.host || '';
        document.getElementById('formBackendOllamaPort').value = backend?.ollamaPort || 11434;
        document.getElementById('formBackendAgentPort').value = backend?.agentPort || 18032;
        document.getElementById('formBackendWeight').value = backend?.weight || 1;
        document.getElementById('formBackendMaxConcurrent').value = backend?.maxConcurrentRequests || backend?.maxConcurrentReqs || 10;
        document.getElementById('formBackendMaxModels').value = backend?.maxModels || backend?.runtimeMaxModels || 0;
        document.getElementById('formBackendLabels').value = (backend?.labels || []).join(',');

        // Синхронизируем селектор типа бэкенда с BackendTypeFilter
        var formBackendType = document.getElementById('formBackendType');
        if (formBackendType) {
            var currentType = 'ollama';
            if (window.BackendTypeFilter) {
                currentType = BackendTypeFilter.getCurrentType();
            }
            if (backend && backend.type) {
                currentType = backend.type === 'llama_cpp' ? 'llama_cpp' : 'ollama';
            }
            formBackendType.value = currentType;
            // Применяем фильтрацию полей
            if (window.BackendTypeFilter) {
                BackendTypeFilter.toggleBackendFormFields(currentType);
            }
        }

        // GPU Mode — из capacity.mode или platformMode
        var gpuMode = backend?.gpuMode || backend?.platformMode || 'auto';
        if (backend?.ollama?.backendCapacity?.mode) {
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

    async function saveBackend() {
        const id = document.getElementById('formBackendId').value.trim();
        const name = document.getElementById('formBackendName').value.trim();
        const host = document.getElementById('formBackendHost').value.trim();
        const ollamaPort = parseInt(document.getElementById('formBackendOllamaPort').value) || 11434;
        const agentPort = parseInt(document.getElementById('formBackendAgentPort').value) || 18032;
        const weight = parseFloat(document.getElementById('formBackendWeight').value) || 1;
        const maxConcurrent = parseInt(document.getElementById('formBackendMaxConcurrent').value) || 10;
        const maxModels = parseInt(document.getElementById('formBackendMaxModels').value) || 0;
        const gpuMode = (document.getElementById('formBackendGpuMode') && document.getElementById('formBackendGpuMode').value) || 'auto';
        const labels = document.getElementById('formBackendLabels').value.split(',').map(l => l.trim()).filter(Boolean);

        if (!id || !host) {
            showToast(window.I18N ? I18N.t('common.error') : 'ID and host are required', 'error');
            return;
        }

        var backendType = (document.getElementById('formBackendType') && document.getElementById('formBackendType').value) || 'ollama';

        // cppWorker port:
        //   1. Берём значение из формы, если оно заполнено и > 0
        //   2. Иначе пробуем auto-detect (probe 18092/18091/18093/18090 на /info)
        //   3. Fallback: 18092 (актуальный default для современных llama.cpp-инсталляций).
        // Старое значение 18090 было НЕВЕРНЫМ — это agentPort, а не cppworker port.
        var cppWorkerPortInput = parseInt((document.getElementById('formBackendCppWorkerPort') && document.getElementById('formBackendCppWorkerPort').value)) || 0;
        var cppGrpcPort = parseInt((document.getElementById('formBackendCppGrpcPort') && document.getElementById('formBackendCppGrpcPort').value)) || 19000;

        var payload = { id, name: name || id, host, ollamaPort, agentPort, weight, maxConcurrentRequests: maxConcurrent, maxModels, gpuMode, labels, backendType: backendType };

        // Для llama.cpp-бэкенда гарантируем корректный cppWorkerPort.
        // Если поле пустое — пробуем auto-detect на лету (synchronously-parallel HTTP probes).
        if (backendType === 'llama_cpp') {
            payload.cppWorkerPort = cppWorkerPortInput > 0 ? cppWorkerPortInput : 18092;
            payload.grpcPort = cppGrpcPort;

            // Асинхронно проверяем реальный доступный порт и обновляем бэкенд,
            // если поле было пустым и auto-detect нашёл что-то отличное от 18092.
            if (cppWorkerPortInput <= 0) {
                autoDetectCppWorkerPort(host).then(function (detected) {
                    if (detected && detected !== payload.cppWorkerPort) {
                        payload.cppWorkerPort = detected;
                        // Обновляем бэкенд (PATCH) — чтобы в config зафиксировался правильный порт
                        Api.updateBackend(id, { cppWorkerPort: detected }).then(function () {
                            if (window.console && console.log) {
                                console.log('[app.js] auto-detected cppworker port:', detected, 'for backend', id);
                            }
                        }).catch(function (err) {
                            if (window.console && console.warn) {
                                console.warn('[app.js] failed to update cppworker port:', err);
                            }
                        });
                    }
                }).catch(function () { /* ignore */ });
            }
        }
        const isEdit = document.getElementById('formBackendId').disabled;

        try {
            if (isEdit) {
                await Api.updateBackend(id, payload);
                // Дополнительно обновляем runtime-лимиты через PUT /api/v1/backends/{id}/limits
                try {
                    await Api.updateBackendLimitsFull(id, maxConcurrent, maxModels);
                } catch (limitsErr) {
                    // Не фатально — лимиты может установить позже через агента
                    addLog(window.I18N ? I18N.t('app.backend_limits_warn', { id: id }) : 'Warning: runtime limits for ' + id + ' not updated', 'warn');
                }
                showToast(window.I18N ? I18N.t('app.backend_saved') : 'Backend updated', 'success');
                addLog(window.I18N ? I18N.t('app.backend_updated', { id: id }) : 'Backend ' + id + ' updated', 'info');
            } else {
                await Api.createBackend(payload);
                showToast(window.I18N ? I18N.t('app.backend_created') : 'Backend added', 'success');
                addLog(window.I18N ? I18N.t('app.backend_created') : 'Backend ' + id + ' added', 'info');
            }
            closeModal();
            refreshCurrentPage();
        } catch (e) {
            showToast((window.I18N ? I18N.t('common.error') : 'Ошибка') + ': ' + (e.message || e), 'error');
        }
    }

    async function deleteBackend() {
        const id = document.getElementById('formBackendId').value;
        if (!confirm(window.I18N ? I18N.t('backends.confirm_delete', { name: id }) : `Delete backend ${id}?`)) return;

        try {
            await Api.deleteBackend(id);
            closeModal();
            showToast(window.I18N ? I18N.t('app.backend_deleted') : 'Backend deleted', 'success');
            addLog(window.I18N ? I18N.t('app.backend_deleted', { id: id }) : `Backend ${id} deleted`, 'info');
            refreshCurrentPage();
        } catch (e) {
            showToast((window.I18N ? I18N.t('common.error') : 'Ошибка') + ': ' + (e.message || e), 'error');
        }
    }

    function editBackend(id) {
        openBackendModal(id);
    }

    function confirmDeleteBackend(id) {
        openBackendModal(id);
    }

    // ---- Filtering ----

    function filterBackends(query) {
        const rows = document.querySelectorAll('#backendsTableBody tr');
        const lower = query.toLowerCase();
        rows.forEach(row => {
            row.style.display = row.textContent.toLowerCase().includes(lower) ? '' : 'none';
        });
    }

    function filterSessions(query) {
        const rows = document.querySelectorAll('#sessionsTableBody tr');
        const lower = query.toLowerCase();
        rows.forEach(row => {
            row.style.display = row.textContent.toLowerCase().includes(lower) ? '' : 'none';
        });
    }

    /**
     * Filter + search + type-filter для карточек моделей на Models tab.
     *
     * Логика:
     * - `query` (search) — подстрочный поиск по всему тексту карточки.
     * - `activeType` (из #modelsTypeFilter) — фильтр по типу бэкенда (ollama/llama_cpp/all).
     * - Результат: card.style.display = 'none' для не подходящих, '' для подходящих.
     *
     * Session B — Filter/Search + Sort improvements:
     * - Добавлено отображение счётчика видимых карточек в #modelsCount (если есть).
     * - Добавлен empty-state #modelsEmpty.
     * - Сохраняем query в localStorage для восстановления после refresh страницы.
     */
    function filterModels(query) {
        const cards = document.querySelectorAll('#modelsGrid .model-card');
        const lower = (query || '').toLowerCase();

        // Текущий выбранный тип бэкенда
        var activeTypeBtn = document.querySelector('#modelsTypeFilter .filter-btn.active');
        var activeType = activeTypeBtn ? activeTypeBtn.dataset.filterType : 'all';

        var visibleCount = 0;
        cards.forEach(function (card) {
            var text = card.textContent.toLowerCase();
            var matchQuery = !lower || text.indexOf(lower) !== -1;

            // Определяем тип бэкенда по data-атрибуту карточки или по badge
            var cardType = card.dataset.backendType || card.getAttribute('data-backend-type') || '';
            if (!cardType) {
                // Fallback: ищем в badge тексте 🦙 Ollama / 🦒 llama.cpp
                if (text.indexOf('🦙 ollama') !== -1) cardType = 'ollama';
                else if (text.indexOf('🦒 llama.cpp') !== -1) cardType = 'llama_cpp';
            }
            var matchType = (activeType === 'all') || (cardType === activeType);

            var show = matchQuery && matchType;
            card.style.display = show ? '' : 'none';
            if (show) visibleCount++;
        });

        // Session B: показываем счётчик "X из Y" в #modelsCount, если есть.
        var counter = document.getElementById('modelsCount');
        if (counter) {
            if (cards.length > 0) {
                var total = cards.length;
                if (visibleCount === total) {
                    counter.textContent = _t('models.filter_count_all', { count: total });
                } else {
                    counter.textContent = _t('models.filter_count_filtered', { visible: visibleCount, total: total });
                }
                counter.style.display = '';
            } else {
                counter.style.display = 'none';
            }
        }

        // Показываем/скрываем empty-state
        var empty = document.getElementById('modelsEmpty');
        if (empty) {
            if (cards.length > 0 && visibleCount === 0) {
                empty.hidden = false;
            } else {
                empty.hidden = true;
            }
        }

        // Сохраняем query в localStorage для восстановления при refresh.
        try {
            localStorage.setItem('ollamalegion_models_search', lower || '');
        } catch (e) { /* localStorage недоступен — ignore */ }
    }

    /**
     * Применить сортировку карточек моделей в соответствии с #modelsSortBy.
     * Поддерживает: name-asc, name-desc, size-desc, size-asc, vram-desc, vram-asc, backend-asc.
     * Карточки сортируются in-place через appendChild (DOM reordering).
     *
     * Session B — Filter/Search + Sort improvements: добавлены VRAM-режимы.
     * VRAM-режимы полезны для планирования GPU-нагрузки (какие модели занимают
     * больше всего видеопамяти — отображаются первыми при vram-desc).
     */
    function applyModelsSort() {
        var grid = document.getElementById('modelsGrid');
        if (!grid) return;
        var sortSelect = document.getElementById('modelsSortBy');
        if (!sortSelect) return;
        var sortKey = sortSelect.value || 'name-asc';

        var cards = Array.prototype.slice.call(grid.querySelectorAll('.model-card'));
        if (cards.length === 0) return;

        function getName(card) {
            return (card.dataset.modelName || card.querySelector('.model-card-title')?.textContent || '').trim().toLowerCase();
        }
        function getSize(card) {
            // Размер в байтах из data-size-bytes, иначе парсим текст
            var v = card.dataset.sizeBytes;
            if (v) return parseInt(v, 10) || 0;
            var t = card.textContent || '';
            var m = t.match(/([\d.]+)\s*(GB|MB|KB|B)\b/i);
            if (!m) return 0;
            var n = parseFloat(m[1]) || 0;
            var u = m[2].toUpperCase();
            if (u === 'GB') return n * 1024 * 1024 * 1024;
            if (u === 'MB') return n * 1024 * 1024;
            if (u === 'KB') return n * 1024;
            return n;
        }
        function getVram(card) {
            // VRAM в MB из data-vram-mb (Session B).
            var v = card.dataset.vramMb;
            if (v !== undefined && v !== '') return parseInt(v, 10) || 0;
            // Fallback: парсим "VRAM: 1234 MB" из текста карточки.
            var t = card.textContent || '';
            var m = t.match(/VRAM[^\d]*([\d.]+)\s*MB/i);
            if (!m) return 0;
            return Math.round(parseFloat(m[1])) || 0;
        }
        function getBackend(card) {
            return (card.dataset.backendId || card.dataset.backend || '').toLowerCase();
        }

        cards.sort(function (a, b) {
            switch (sortKey) {
                case 'name-desc':
                    return getName(b).localeCompare(getName(a));
                case 'size-desc':
                    return getSize(b) - getSize(a);
                case 'size-asc':
                    return getSize(a) - getSize(b);
                case 'vram-desc':
                    return getVram(b) - getVram(a);
                case 'vram-asc':
                    return getVram(a) - getVram(b);
                case 'backend-asc':
                    var ba = getBackend(a), bb = getBackend(b);
                    if (ba !== bb) return ba.localeCompare(bb);
                    return getName(a).localeCompare(getName(b));
                case 'name-asc':
                default:
                    return getName(a).localeCompare(getName(b));
            }
        });

        // Перемещаем карточки в DOM в новом порядке
        var fragment = document.createDocumentFragment();
        cards.forEach(function (c) { fragment.appendChild(c); });
        grid.appendChild(fragment);
    }

    // ---- UI Utilities ----

    /**
     * Обновление маркера типа движка инференса в сайдбаре.
     * Принимает clusterState (или любой объект с полями backendEngine, backendTypeCounts, operatingMode).
     * Отображает: 🦙 Ollama API, 🦒 llama.cpp, или 🔌 Автоопределение...
     */
    function updateBackendEngineBadge(state) {
        var badge = document.getElementById('backendEngineBadge');
        var label = document.getElementById('backendEngineLabel');
        if (!badge || !label) return;

        // Приоритет: выбор пользователя (localStorage через BackendTypeFilter) > cluster state
        var userType = window.BackendTypeFilter ? BackendTypeFilter.getCurrentType() : null;
        var effectiveType = (state && state.effectiveBackendType) || '';
        var engine = (state && state.backendEngine) || '';
        var counts = (state && state.backendTypeCounts) || {};

        // Remove old colour classes
        badge.classList.remove('engine-ollama', 'engine-llama_cpp', 'engine-auto');

        var icon = '🔌';
        var text = (window.I18N ? I18N.t('dashboard.engine_auto') : 'Автоопределение...');
        var cssClass = 'engine-auto';
        var title = (window.I18N ? I18N.t('dashboard.engine_hint_auto') : 'Тип движка: автоопределение');

        // Приоритет 1: выбор пользователя (localStorage)
        if (userType === 'llama_cpp') {
            icon = '🦒';
            text = 'llama.cpp';
            cssClass = 'engine-llama_cpp';
            title = (window.I18N ? I18N.t('dashboard.engine_hint_llama_cpp') : 'Движок: llama.cpp');
        } else if (userType === 'ollama') {
            icon = '🦙';
            text = 'Ollama API';
            cssClass = 'engine-ollama';
            title = (window.I18N ? I18N.t('dashboard.engine_hint_ollama') : 'Движок: Ollama API');
        } else if (userType === 'all') {
            // Round 18g: пользователь выбрал "Все" — показываем нейтральный badge.
            icon = '🔌';
            text = (window.I18N ? I18N.t('dashboard.engine_all') : 'Все движки');
            cssClass = 'engine-auto';
            title = (window.I18N ? I18N.t('dashboard.engine_hint_all') : 'Движок: все');
        } else if (effectiveType === 'llama_cpp') {
            icon = '🦒';
            text = 'llama.cpp';
            cssClass = 'engine-llama_cpp';
            title = (window.I18N ? I18N.t('dashboard.engine_hint_llama_cpp') : 'Движок: llama.cpp');
        } else if (effectiveType === 'ollama') {
            icon = '🦙';
            text = 'Ollama API';
            cssClass = 'engine-ollama';
            title = (window.I18N ? I18N.t('dashboard.engine_hint_ollama') : 'Движок: Ollama API');
        } else if (engine === 'llama_cpp') {
            icon = '🦒';
            text = 'llama.cpp';
            cssClass = 'engine-llama_cpp';
            title = (window.I18N ? I18N.t('dashboard.engine_hint_llama_cpp') : 'Движок: llama.cpp');
        } else if (engine === 'ollama_api') {
            icon = '🦙';
            text = 'Ollama API';
            cssClass = 'engine-ollama';
            title = (window.I18N ? I18N.t('dashboard.engine_hint_ollama') : 'Движок: Ollama API');
        }

        // Append per-type counts to title if available
        if (counts && Object.keys(counts).length > 0) {
            var parts = [];
            if (counts.ollama) parts.push('🦙 Ollama: ' + counts.ollama);
            if (counts.llama_cpp) parts.push('🦒 llama.cpp: ' + counts.llama_cpp);
            if (parts.length > 0) {
                title += ' | ' + parts.join(', ');
            }
        }

        var iconEl = badge.querySelector('.engine-icon');
        if (iconEl) iconEl.textContent = icon;
        label.textContent = text;
        badge.classList.add(cssClass);
        badge.title = title;
    }

    function updateConnectionStatus(connected) {
        const status = document.getElementById('connectionStatus');
        if (!status) return;
        const dot = status.querySelector('.status-dot');
        const text = status.querySelector('.status-text');
        if (!dot || !text) return;

        if (connected) {
            dot.classList.remove('disconnected');
            dot.classList.add('connected');
            text.textContent = window.I18N ? I18N.t('common.connected') : 'Connected';
        } else {
            dot.classList.remove('connected');
            dot.classList.add('disconnected');
            text.textContent = window.I18N ? I18N.t('common.disconnected') : 'Disconnected';
        }
    }

    function showToast(message, type = 'info') {
        const container = document.getElementById('toastContainer');
        if (!container) return;
        const toast = document.createElement('div');
        toast.className = `toast ${type}`;
        toast.innerHTML = `<span>${Utils.escapeHtml(message)}</span>`;
        container.appendChild(toast);

        setTimeout(() => {
            toast.style.opacity = '0';
            toast.style.transform = 'translateX(100%)';
            setTimeout(() => toast.remove(), 300);
        }, 4000);
    }

    /**
     * Добавить запись лога в data.logs и (если пользователь на Logs tab) перерисовать.
     *
     * F.2 (Session F): поддерживает 2 формата входа:
     *   1. addLog(message, level) — старая сигнатура, используется для локальных событий WebUI.
     *   2. addLogEntry(entry) — новая, для записей из WebSocket /ws/logs. entry = {time, level, message, source}.
     *
     * Backend присылает time как ISO-строку (RFC3339) или Date; здесь мы нормализуем
     * к локализованному HH:MM:SS для совместимости с renderLogs().
     */
    function addLogEntry(entry) {
        if (!entry || typeof entry !== 'object') return;
        var locale = (window.I18N && I18N.getLang() === 'ru') ? 'ru' : 'en';
        var timeStr;
        if (entry.time) {
            try {
                var d = entry.time instanceof Date ? entry.time : new Date(entry.time);
                if (!isNaN(d.getTime())) {
                    timeStr = d.toLocaleTimeString(locale);
                }
            } catch (e) { /* ignore */ }
        }
        if (!timeStr) timeStr = new Date().toLocaleTimeString(locale);

        var normalized = {
            time: timeStr,
            level: (entry.level || 'info').toUpperCase(),
            message: entry.message || '',
            source: entry.source || ''
        };
        data.logs.unshift(normalized);
        if (data.logs.length > 500) data.logs = data.logs.slice(0, 500);
        if (currentPage === 'logs') renderLogs(data.logs);
    }

    function addLog(message, level = 'info') {
        // Обратная совместимость: локальные события WebUI (инициализация, ошибки).
        addLogEntry({ level: level, message: message });
    }

    /**
     * F.2 (Session F): инициализация live tail через WebSocket /ws/logs.
     *
     * WS присылает snapshot ring buffer на connect + live LogEntry. Backend-side broker
     * живёт в process balancer (см. internal/api/handlers_logs_ws.go), наполняется
     * через logger.Publish(...) из любого места кода.
     *
     * Token: тот же что и для остальных API endpoints (через WEBUI_CONFIG.API_TOKEN).
     * Если broker не инициализирован на сервере — WS всё равно откроется, пришлёт
     * пустой snapshot и закроется (graceful degradation).
     */
    function initLogsStream() {
        if (!window.logsStream) return;
        var token = (window.WEBUI_CONFIG && window.WEBUI_CONFIG.API_TOKEN) || '';
        window.logsStream.start({
            token: token,
            onSnapshot: function (msg) {
                if (!msg || !Array.isArray(msg.entries)) return;
                // Заменяем data.logs на snapshot (server is source of truth).
                var locale = (window.I18N && I18N.getLang() === 'ru') ? 'ru' : 'en';
                data.logs = msg.entries.map(function (e) {
                    var timeStr;
                    if (e.time) {
                        try {
                            var d = e.time instanceof Date ? e.time : new Date(e.time);
                            if (!isNaN(d.getTime())) timeStr = d.toLocaleTimeString(locale);
                        } catch (err) { /* ignore */ }
                    }
                    if (!timeStr) timeStr = new Date().toLocaleTimeString(locale);
                    return {
                        time: timeStr,
                        level: (e.level || 'info').toUpperCase(),
                        message: e.message || '',
                        source: e.source || ''
                    };
                });
                if (currentPage === 'logs') renderLogs(data.logs);
            },
            onEntry: function (entry) {
                addLogEntry(entry);
            }
        });
    }

    // ---- Export ----

    /**
     * Export системных логов в файл (Session E — Export logs to CSV).
     *
     * Поддерживает 3 формата: CSV (RFC 4180), TXT (legacy), JSON.
     * Применяет level filter из #logsLevelFilter (по умолчанию "all" — без фильтра).
     *
     * CSV-формат: "timestamp,level,message\r\n<row>..." с правильным escape для
     * запятых/кавычек/переносов строк (RFC 4180 §2.6/§2.7).
     *
     * Использует Utils.downloadFile для Blob-based download (без сервера).
     */
    function exportLogs() {
        // Читаем текущий фильтр уровня (если есть).
        var levelSelect = document.getElementById('logsLevelFilter');
        var levelFilter = levelSelect ? levelSelect.value : 'all';

        // Фильтруем логи по уровню.
        var filteredLogs = (data.logs || []).filter(function (l) {
            if (!l || !l.level) return levelFilter === 'all';
            if (levelFilter === 'all') return true;
            return (l.level || '').toLowerCase() === levelFilter;
        });

        // Определяем формат из #logsExportFormat (default csv).
        var formatSelect = document.getElementById('logsExportFormat');
        var format = formatSelect ? formatSelect.value : 'csv';

        var content = '';
        var mime = 'text/plain';
        var ext = 'txt';
        var stamp = new Date().toISOString().replace(/[:.]/g, '-').slice(0, 19);

        if (format === 'csv') {
            // RFC 4180: поля с запятой/кавычкой/переносом оборачиваются в кавычки,
            // кавычки внутри удваиваются.
            var escapeCsv = function (val) {
                var s = val === null || val === undefined ? '' : String(val);
                if (/[",\r\n]/.test(s)) {
                    return '"' + s.replace(/"/g, '""') + '"';
                }
                return s;
            };
            content = 'timestamp,level,message\r\n' + filteredLogs.map(function (l) {
                return escapeCsv(l.time) + ',' + escapeCsv(l.level) + ',' + escapeCsv(l.message);
            }).join('\r\n');
            mime = 'text/csv';
            ext = 'csv';
        } else if (format === 'json') {
            content = JSON.stringify({
                exportedAt: new Date().toISOString(),
                count: filteredLogs.length,
                levelFilter: levelFilter,
                logs: filteredLogs
            }, null, 2);
            mime = 'application/json';
            ext = 'json';
        } else {
            // TXT (legacy): [time] LEVEL: message
            content = filteredLogs.map(function (l) {
                return '[' + (l.time || '') + '] ' + (l.level || '') + ': ' + (l.message || '');
            }).join('\n');
        }

        var filename = 'ollamalegion-logs-' + stamp + (levelFilter !== 'all' ? '-' + levelFilter : '') + '.' + ext;
        Utils.downloadFile(content, filename, mime);
    }

    function exportBackends() {
        const content = JSON.stringify(data.backends, null, 2);
        Utils.downloadFile(content, 'ollamalegion-backends.json', 'application/json');
    }

    // ---- Proxy Logs ----

    function addProxyLog(entry) {
        if (!entry) return;
        var locale = (window.I18N && I18N.getLang() === 'ru') ? 'ru' : 'en';
        if (entry.timestamp) {
            entry._time = new Date(entry.timestamp).toLocaleTimeString(locale);
        } else {
            entry._time = new Date().toLocaleTimeString(locale);
        }
        data.proxyLogs.unshift(entry);
        if (data.proxyLogs.length > 500) data.proxyLogs = data.proxyLogs.slice(0, 500);
        // Round 16 (2026-07-10): для 4xx/5xx дубль в общий Logs page — пользователь
        // увидит и в Logs (system) и в Proxy Logs. Бэкенд вернул клиенту ошибку →
        // оператор должен видеть это в обеих секциях.
        if (entry.statusCode && entry.statusCode >= 400) {
            var logEntry = {
                time: entry._time,
                level: entry.statusCode >= 500 ? 'error' : 'warn',
                message: 'Backend "' + (entry.backendID || '?') + '" returned ' + entry.statusCode +
                    ' on ' + (entry.method || '?') + ' ' + (entry.path || '?') +
                    (entry.error ? ' — ' + String(entry.error).substring(0, 200) : ''),
                source: 'proxy'
            };
            data.logs.unshift(logEntry);
            if (data.logs.length > 1000) data.logs = data.logs.slice(0, 1000);
            // Если активна вкладка Logs (system, не proxy) — тоже обновить
            if (currentPage === 'logs') {
                var systemTab = document.getElementById('logsTabSystem') || document.getElementById('logsTab');
                var proxyTabActive = document.getElementById('logsTabProxy');
                if (systemTab && systemTab.classList.contains('active') &&
                    !(proxyTabActive && proxyTabActive.classList.contains('active'))) {
                    renderLogs(data.logs);
                }
            }
        }
        if (currentPage === 'logs') {
            var proxyTab = document.getElementById('logsTabProxy');
            if (proxyTab && proxyTab.classList.contains('active')) {
                renderProxyLogs(data.proxyLogs);
            }
        }
    }

    // ---- Agents ----

    async function fetchAgents() {
        try {
            data.agents = await Api.agentsStats();
            if (currentPage === 'agents') renderAgentsPage(data.agents);
        } catch (e) {
            Api.handleError(e, window.I18N ? I18N.t('app.error_loading_agents') : 'Error loading agents');
        }
    }

    async function showAgentDetails(agentId) {
        var card = document.getElementById('agentDetailsCard');
        var content = document.getElementById('agentDetailsContent');
        if (!card || !content) return;
        content.innerHTML = '<div class="loading">' + (window.I18N ? I18N.t('common.loading') : 'Loading...') + '</div>';
        card.style.display = 'block';
        try {
            var info = await Api.agentInfo(agentId);
            renderAgentDetails(info);
        } catch (e) {
            content.innerHTML = '<div class="error">' + (window.I18N ? I18N.t('common.error') : 'Error') + ': ' + (e.message || e) + '</div>';
        }
    }

    function restartAgent(backendId) {
        if (!confirm((window.I18N ? I18N.t('agents.confirm_restart') : 'Restart agent on backend') + ' ' + backendId + '?')) return;
        showToast((window.I18N ? I18N.t('agents.restarting') : 'Restarting agent...'), 'info');
        Api.restartAgent(backendId).then(function(result) {
            showToast((window.I18N ? I18N.t('agents.restarted') : 'Agent restarted'), 'success');
            addLog('Agent restarted on ' + backendId, 'info');
        }).catch(function(err) {
            showToast((window.I18N ? I18N.t('agents.restart_error') : 'Restart error') + ': ' + (err.message || err), 'error');
        });
    }

    function viewAgentLogs(backendId) {
        showToast((window.I18N ? I18N.t('agents.loading_logs') : 'Loading logs...'), 'info');
        Api.agentLogs(backendId, 200).then(function(data) {
            var logs = data.logs || data.entries || [];
            var content = logs.length ? logs.map(function(l) {
                var time = l.time || l.timestamp || '';
                var level = l.level || 'INFO';
                var msg = l.message || l.msg || '';
                var levelClass = level.toLowerCase();
                return '<div class="log-entry ' + levelClass + '"><span class="log-time">' + Utils.escapeHtml(time) + '</span> <span class="log-level log-level-' + levelClass + '">' + Utils.escapeHtml(level) + '</span> ' + Utils.escapeHtml(msg) + '</div>';
            }).join('') : '<div style="text-align:center;color:var(--text-muted);padding:20px;">' + (window.I18N ? I18N.t('agents.no_logs') : 'No logs available') + '</div>';

            var modalHtml = '<div id="agentLogsModal" class="modal active" style="z-index:2000;"><div class="modal-content" style="max-width:800px;">' +
                '<div class="modal-header"><h3>' + (window.I18N ? I18N.t('agents.logs_title') : 'Agent Logs') + ' — ' + Utils.escapeHtml(backendId) + '</h3><button class="modal-close" onclick="document.getElementById(\'agentLogsModal\').remove()">×</button></div>' +
                '<div class="modal-body"><div class="agent-logs-content">' + content + '</div></div>' +
                '<div class="modal-footer"><button class="btn btn-secondary" onclick="document.getElementById(\'agentLogsModal\').remove()">' + (window.I18N ? I18N.t('common.close') : 'Закрыть') + '</button></div>' +
                '</div></div>';

            var existing = document.getElementById('agentLogsModal');
            if (existing) existing.remove();
            document.body.insertAdjacentHTML('beforeend', modalHtml);
        }).catch(function(err) {
            showToast((window.I18N ? I18N.t('agents.logs_error') : 'Failed to load logs') + ': ' + (err.message || err), 'error');
        });
    }

    function fetchProxyLogs() {
        Api.proxyLogs(100).then(function(res) {
            data.proxyLogs = res.entries || [];
            if (currentPage === 'logs') {
                renderLogs(data.logs);
                var proxyTab = document.getElementById('logsTabProxy');
                if (proxyTab && proxyTab.classList.contains('active')) {
                    renderProxyLogs(data.proxyLogs);
                }
            }
        }).catch(function(err) {
            console.error('Failed to fetch proxy logs:', err);
        });
    }

    function loadBackendLimits() {
        var tbody = document.getElementById('backendLimitsBody');
        if (!tbody) return;
        var backends = data.backends || [];
        if (backends.length === 0) {
            tbody.innerHTML = '<tr><td colspan="6" class="loading-cell">' + (window.I18N ? I18N.t('common.loading') : 'Loading...') + '</td></tr>';
            return;
        }
        tbody.innerHTML = backends.map(function(b) {
            var maxConcurrent = b.maxConcurrentRequests || b.activeRequests || 10;
            var maxModels = b.maxModels || 0;
            var statusClass = b.status === 'healthy' ? 'badge-success' : (b.status === 'warning' ? 'badge-warning' : 'badge-error');
            var statusText = b.status || (window.I18N ? I18N.t('common.unknown') : 'Unknown');
            return '<tr>' +
                '<td><strong>' + Utils.escapeHtml(b.id || '-') + '</strong></td>' +
                '<td>' + Utils.escapeHtml(b.host || '-') + '</td>' +
                '<td>' + (b.agentPort || '-') + '</td>' +
                '<td><input type="number" class="form-control backend-limit-input" data-backend-id="' + Utils.escapeHtml(b.id) + '" data-limit-type="maxConcurrent" value="' + maxConcurrent + '" min="1" style="width:100px;"></td>' +
                '<td><input type="number" class="form-control backend-limit-input" data-backend-id="' + Utils.escapeHtml(b.id) + '" data-limit-type="maxModels" value="' + maxModels + '" min="0" style="width:100px;"></td>' +
                '<td><span class="badge ' + statusClass + '">' + statusText + '</span></td>' +
            '</tr>';
        }).join('');
    }

    function saveBackendLimits() {
        var inputs = document.querySelectorAll('.backend-limit-input');
        var limits = {};
        inputs.forEach(function(input) {
            var backendId = input.dataset.backendId;
            var limitType = input.dataset.limitType;
            if (!limits[backendId]) limits[backendId] = {};
            limits[backendId][limitType] = parseInt(input.value) || 0;
        });
        var promises = Object.keys(limits).map(function(backendId) {
            var l = limits[backendId];
            return Api.updateBackendLimitsFull(backendId, l.maxConcurrent || 10, l.maxModels || 0);
        });
        Promise.all(promises).then(function() {
            showToast(window.I18N ? I18N.t('settings.saved') : 'Backend limits saved', 'success');
            fetchClusterState();
        }).catch(function(err) {
            showToast((window.I18N ? I18N.t('common.error') : 'Error') + ': ' + (err.message || err), 'error');
        });
    }

    function setupLogsTabNavigation() {
        document.querySelectorAll('.logs-tab').forEach(function(tab) {
            tab.addEventListener('click', function() {
                document.querySelectorAll('.logs-tab').forEach(function(t) { t.classList.remove('active'); });
                document.querySelectorAll('.logs-panel').forEach(function(p) { p.classList.remove('active'); });
                this.classList.add('active');
                var tabName = this.dataset.logsTab;
                var panelName = 'logsPanel' + tabName.charAt(0).toUpperCase() + tabName.slice(1);
                var panel = document.getElementById(panelName);
                if (panel) panel.classList.add('active');
                if (tabName === 'proxy') {
                    renderProxyLogs(data.proxyLogs);
                }
            });
        });
    }

    // ---- Model Management ----

    function openModelManageModal(backendId) {
        const backend = data.backends.find(b => b.id === backendId);
        if (!backend) {
            showToast('Backend not found', 'error');
            return;
        }

        const modal = document.getElementById('modelManageModal');
        if (!modal) return;

        // Store backendId in dataset for pull button
        modal.dataset.backendId = backendId;

        // Set title
        const title = modal.querySelector('.modal-header h3');
        if (title) title.textContent = (window.I18N ? I18N.t('models.manage_title') : 'Model Management') + ' — ' + backendId;

        // B-08: Скрываем Pull Model секцию для llama_cpp бэкендов
        var bt = backend.backend_type || backend.BackendType || 'ollama';
        var pullSection = modal.querySelector('.model-manage-section');
        if (pullSection) {
            pullSection.style.display = (bt === 'llama_cpp') ? 'none' : '';
        }

        // Show operations list with loading
        const modelsBody = document.getElementById('modelManageModelsBody');
        if (modelsBody) {
            modelsBody.innerHTML = '<tr><td colspan="7" class="loading-cell">' + (window.I18N ? I18N.t('common.loading') : 'Loading...') + '</td></tr>';
        }

        // Show active operations (запускает auto-refresh при наличии активных op)
        refreshModelOpsStatus();

        modal.classList.add('active');

        // Load models from backend
        loadBackendModels(backendId);
    }

    function loadBackendModels(backendId) {
        const modelsBody = document.getElementById('modelManageModelsBody');
        if (!modelsBody) return;

        Api.backendModels(backendId).then(function(data) {
            const models = data.models || [];
            if (models.length === 0) {
                modelsBody.innerHTML = '<tr><td colspan="7" class="loading-cell">' + (window.I18N ? I18N.t('models.no_models_backend') : 'No models') + '</td></tr>';
                return;
            }
            modelsBody.innerHTML = models.map(function(m) {
                const sizeStr = m.size ? (m.size / 1024 / 1024 / 1024).toFixed(2) + ' GB' : '-';
                const modified = m.modifiedAt ? new Date(m.modifiedAt).toLocaleString(Utils._locale()) : '-';
                const loadedBadge = m.loaded
                    ? '<span class="badge badge-success">' + (window.I18N ? I18N.t('models.loaded_status') : 'Loaded') + '</span>'
                    : '<span class="badge badge-secondary">' + (window.I18N ? I18N.t('models.unloaded_status') : 'Not loaded') + '</span>';
                return '<tr>' +
                    '<td><strong>' + Utils.escapeHtml(m.name) + '</strong></td>' +
                    '<td>' + (m.digest ? m.digest.substring(0, 16) + '...' : '-') + '</td>' +
                    '<td>' + sizeStr + '</td>' +
                    '<td>' + modified + '</td>' +
                    '<td>' + loadedBadge + '</td>' +
                    '<td>' +
                        '<button class="action-btn edit" onclick="ui.executeModelOperation(\'' + Utils.escapeHtml(backendId) + '\', \'load\', \'' + Utils.escapeHtml(m.name) + '\')" title="' + (window.I18N ? I18N.t('models.load_hint') : 'Load') + '">' + (window.I18N ? I18N.t('models.load') : 'Load') + '</button>' +
                        '<button class="action-btn delete" onclick="ui.executeModelOperation(\'' + Utils.escapeHtml(backendId) + '\', \'unload\', \'' + Utils.escapeHtml(m.name) + '\')" title="' + (window.I18N ? I18N.t('models.unload_hint') : 'Unload') + '">' + (window.I18N ? I18N.t('models.unload') : 'Unload') + '</button>' +
                    '</td>' +
                '</tr>';
            }).join('');
        }).catch(function(err) {
            modelsBody.innerHTML = '<tr><td colspan="7" class="loading-cell">' + (window.I18N ? I18N.t('models.operation_error') : 'Error') + ': ' + (err.message || err) + '</td></tr>';
        });
    }

    function executeModelOperation(backendId, operation, modelName, options = {}) {
        showToast((window.I18N ? I18N.t('models.operation_running') : 'Operation in progress...') + ' ' + operation + ' ' + modelName, 'info');

        Api.backendModelOperation(backendId, operation, modelName, options).then(function(result) {
            if (result && result.success) {
                showToast((window.I18N ? I18N.t('models.operation_success') : 'Success') + ': ' + operation + ' ' + modelName, 'success');
                addLog('Model op success: ' + operation + ' ' + modelName + ' on ' + backendId, 'info');
                // Reload models after a short delay
                setTimeout(function() {
                    loadBackendModels(backendId);
                    refreshModelOpsStatus();
                }, 1500);
            } else {
                var errMsg = (result && result.error) || (window.I18N ? I18N.t('models.operation_error') : 'Operation failed');
                showToast(errMsg, 'error');
                addLog('Model op error: ' + operation + ' ' + modelName + ' on ' + backendId + ': ' + errMsg, 'error');
            }
        }).catch(function(err) {
            showToast((window.I18N ? I18N.t('models.operation_error') : 'Error') + ': ' + (err.message || err), 'error');
            addLog('Model op error: ' + operation + ' ' + modelName + ' on ' + backendId + ': ' + (err.message || err), 'error');
        });
    }

    // ===== Pull / Op progress (Q3 W3-4 sub-task) =====
    // Локальный кэш отмен: ключ = operation + ':' + modelName + ':' + backendId.
    // Используется для UI-state «cancel requested» — пока op живёт на сервере,
    // показываем progress как «cancelling» и блокируем кнопку. Когда op уходит
    // из active list, считаем её завершённой (cancel success).
    const _cancelledOps = new Set();
    let _opsAutoRefreshTimer = null;
    const OPS_AUTO_REFRESH_MS = 2000;
    // Heuristic-константы для расчёта прогресса (нет данных от backend'а):
    // — pull операции обычно идут 1–30 мин (HF модели);
    // — load операции — 5–60 сек (модель уже на диске, нужно прочитать header + mmap).
    const OP_HEURISTIC_DURATION_MS = {
        pull:   180000, // 3 мин — default для pull (HF model)
        load:    30000, // 30 сек — default для load
        unload:   5000, // 5 сек — default для unload
        delete:   5000,
        copy:    10000,
        create:   5000
    };

    function _opKey(op) {
        return (op.operation || '?') + ':' + (op.modelName || '?') + ':' + (op.backendId || '?');
    }

    /**
     * Heuristic-расчёт прогресса для op без реального progress-индикатора от backend.
     * Используется startedAt + heuristic duration (per operation type).
     * @returns {{pct: number, etaSec: number, isIndeterminate: boolean}}
     */
    function computeOpProgress(op) {
        const key = _opKey(op);
        const opType = (op.operation || 'load').toLowerCase();
        const startedMs = op.startedAt ? new Date(op.startedAt).getTime() : Date.now();
        const elapsedMs = Math.max(0, Date.now() - startedMs);
        const expectedMs = OP_HEURISTIC_DURATION_MS[opType] || 60000;

        if (_cancelledOps.has(key)) {
            // Замораживаем прогресс на текущем значении при отмене.
            const pctFrozen = Math.min(99, Math.max(5, Math.round((elapsedMs / expectedMs) * 100)));
            return { pct: pctFrozen, etaSec: 0, isIndeterminate: false, cancelled: true };
        }

        // Линейный прогресс от 5% (сразу) до 95% (по истечении expectedMs).
        // Не показываем 100% до того, как op реально исчезнет из active list —
        // иначе пользователь видит «готово», хотя backend ещё работает.
        const pct = Math.min(95, Math.max(5, Math.round((elapsedMs / expectedMs) * 100)));
        const remainingMs = Math.max(0, expectedMs - elapsedMs);
        const etaSec = Math.round(remainingMs / 1000);
        return { pct: pct, etaSec: etaSec, isIndeterminate: false, cancelled: false };
    }

    function formatOpsEta(etaSec) {
        if (!etaSec || etaSec < 0) return '';
        if (etaSec < 60) return '~' + etaSec + 's';
        const m = Math.floor(etaSec / 60);
        const s = etaSec % 60;
        if (m < 60) return '~' + m + 'm ' + (s ? s + 's' : '');
        const h = Math.floor(m / 60);
        const rm = m % 60;
        return '~' + h + 'h ' + rm + 'm';
    }

    /**
     * Помечает op как cancelled в локальном кэше. Backend не получает cancel-сигнал
     * (DELETE endpoint отсутствует), но UI сразу показывает «cancelling» state.
     * Когда op уйдёт из active list — toast «cancelled».
     */
    function cancelOperation(op) {
        const key = _opKey(op);
        const opLabel = (op.operation || 'op') + ' ' + (op.modelName || '');
        _cancelledOps.add(key);
        showToast((window.I18N ? I18N.t('models.cancel_requested') : 'Cancel requested for') + ': ' + opLabel, 'warning');
        addLog('Cancel requested for op: ' + key, 'warning');
        // Принудительно обновить отображение (без ожидания следующего тика).
        refreshModelOpsStatus();
    }

    function startOpsAutoRefresh() {
        if (_opsAutoRefreshTimer) return; // уже запущен
        // Защита от race: если модалка закрыта, не запускаем polling.
        // (race возникает, когда refreshModelOpsStatus resolve'ится ПОСЛЕ closeModelManageModal.)
        var modalEl = document.getElementById('modelManageModal');
        if (modalEl && !modalEl.classList.contains('active')) return;
        const indicator = document.getElementById('modelOpsAutoRefresh');
        if (indicator) indicator.hidden = false;
        refreshModelOpsStatus();
        _opsAutoRefreshTimer = setInterval(refreshModelOpsStatus, OPS_AUTO_REFRESH_MS);
    }

    function stopOpsAutoRefresh() {
        if (_opsAutoRefreshTimer) {
            clearInterval(_opsAutoRefreshTimer);
            _opsAutoRefreshTimer = null;
        }
        const indicator = document.getElementById('modelOpsAutoRefresh');
        if (indicator) indicator.hidden = true;
    }

    function refreshModelOpsStatus() {
        const opsBody = document.getElementById('modelOpsBody');
        if (!opsBody) return;
        Api.modelOperationsStatus().then(function(data) {
            const ops = (data && data.operations) || [];

            // Отслеживаем op, которые ушли из active list — это «завершённые».
            // Если op была помечена как cancelled — показываем toast «cancelled».
            // Если op не была cancelled — показываем toast «completed» (только для
            // pull/load/unload/delete, чтобы не спамить на каждое обновление).
            const seenKeys = new Set();
            ops.forEach(function(op) { seenKeys.add(_opKey(op)); });
            // Копия — иначе модифицируем Set во время итерации.
            Array.from(_cancelledOps).forEach(function(key) {
                if (!seenKeys.has(key)) {
                    _cancelledOps.delete(key);
                    showToast((window.I18N ? I18N.t('models.op_cancelled') : 'Operation cancelled'), 'warning');
                }
            });

            if (ops.length === 0) {
                opsBody.innerHTML = '<tr><td colspan="7" class="loading-cell">' +
                    (window.I18N ? I18N.t('models.no_active_ops') : 'No active operations') + '</td></tr>';
                // Если была хоть одна op и сейчас ноль — останавливаем auto-refresh.
                if (_opsAutoRefreshTimer) stopOpsAutoRefresh();
                return;
            }

            // Есть активные op — убеждаемся, что auto-refresh запущен.
            if (!_opsAutoRefreshTimer) startOpsAutoRefresh();

            opsBody.innerHTML = ops.map(function(op) {
                const opKey = _opKey(op);
                const opType = (op.operation || '-');
                const progress = computeOpProgress(op);
                const isCancelling = progress.cancelled;
                const fillClass = 'ops-progress-fill' + (isCancelling ? ' cancelling' : '');
                const opTypeLabel = window.I18N
                    ? I18N.t('models.operation_' + opType, opType)
                    : opType;
                const etaText = progress.cancelled
                    ? (window.I18N ? I18N.t('models.cancel_requested') : 'Cancelling…')
                    : formatOpsEta(progress.etaSec);
                const cancelLabel = window.I18N ? I18N.t('models.cancel_op') : 'Cancel';
                const statusLabel = isCancelling
                    ? (window.I18N ? I18N.t('models.cancel_requested') : 'Cancelling…')
                    : (window.I18N ? I18N.t('models.op_running') : 'Running');

                // Escape opKey для inline onclick (replace ':' не нужен — это просто строка).
                const safeOpKey = Utils.escapeHtml(opKey).replace(/'/g, "\\'");

                return '<tr data-op-key="' + Utils.escapeHtml(opKey) + '">' +
                    '<td><span class="badge badge-info">' + Utils.escapeHtml(opTypeLabel) + '</span></td>' +
                    '<td><strong>' + Utils.escapeHtml(op.modelName || '-') + '</strong></td>' +
                    '<td>' + Utils.escapeHtml(op.backendId || '-') + '</td>' +
                    '<td>' + (op.startedAt ? new Date(op.startedAt).toLocaleString(Utils._locale()) : '-') + '</td>' +
                    '<td>' +
                        '<div class="ops-progress-wrap">' +
                            '<div class="ops-progress-bar">' +
                                '<div class="' + fillClass + '" style="width: ' + progress.pct + '%;"></div>' +
                            '</div>' +
                            '<div class="ops-progress-text">' +
                                '<span class="ops-progress-percent">' + progress.pct + '%</span>' +
                                '<span class="ops-progress-eta">' + etaText + '</span>' +
                            '</div>' +
                        '</div>' +
                    '</td>' +
                    '<td><span class="badge ' + (isCancelling ? 'badge-warning' : 'badge-info') + '">' +
                        Utils.escapeHtml(statusLabel) + '</span></td>' +
                    '<td>' +
                        '<button type="button" class="ops-action-cancel' + (isCancelling ? ' cancelling' : '') + '"' +
                            (isCancelling ? ' disabled' : '') +
                            ' onclick="ui.cancelOperationByKey(\'' + safeOpKey + '\')"' +
                            ' title="' + Utils.escapeHtml(cancelLabel) + '">' +
                            '<i class="fas ' + (isCancelling ? 'fa-circle-notch' : 'fa-stop') + '"></i> ' +
                            '<span>' + Utils.escapeHtml(cancelLabel) + '</span>' +
                        '</button>' +
                    '</td>' +
                '</tr>';
            }).join('');
        }).catch(function(err) {
            // Silently ignore — это background refresh.
            console.error('Failed to refresh model ops status:', err);
        });
    }

    /**
     * Cancel-by-key (вызывается из inline onclick в HTML рендере).
     * Ищет op в кэше по ключу и помечает cancelled.
     */
    function cancelOperationByKey(opKey) {
        // Нам нужен полный op для cancelOperation, но у нас только ключ.
        // Запоминаем ключ напрямую — следующий refresh подхватит cancelled state.
        _cancelledOps.add(opKey);
        showToast((window.I18N ? I18N.t('models.cancel_requested') : 'Cancel requested'), 'warning');
        refreshModelOpsStatus();
    }

    function closeModelManageModal() {
        // Останавливаем auto-refresh активных op (экономим трафик, если модалка закрыта).
        stopOpsAutoRefresh();
        const modal = document.getElementById('modelManageModal');
        if (modal) modal.classList.remove('active');
    }

    // ---- CppWorker port auto-detection ----
    // Probes common cppworker ports on the given host and returns the first
    // port that responds OK to /info (or /health). 18092 first (актуальный
    // default для современных llama.cpp-инсталляций), затем 18091 (stub),
    // 18093 (доп. профиль) и 18090 (legacy/agent).
    async function autoDetectCppWorkerPort(host) {
        if (!host) return null;
        const candidates = [18092, 18091, 18093, 18090];
        // Параллельные probe-запросы (AbortController + 1.5s timeout на каждый)
        const probes = candidates.map(function (port) {
            return new Promise(function (resolve) {
                try {
                    const ctrl = new AbortController();
                    const timeoutId = setTimeout(function () { ctrl.abort(); }, 1500);
                    fetch('http://' + host + ':' + port + '/info', {
                        signal: ctrl.signal,
                        mode: 'cors',
                        cache: 'no-store',
                        headers: { 'Accept': 'application/json' }
                    }).then(function (resp) {
                        clearTimeout(timeoutId);
                        if (resp && resp.ok) resolve(port);
                        else resolve(null);
                    }).catch(function () {
                        clearTimeout(timeoutId);
                        resolve(null);
                    });
                } catch (e) {
                    resolve(null);
                }
            });
        });
        const results = await Promise.all(probes);
        // Возвращаем первый непустой результат
        for (var i = 0; i < results.length; i++) {
            if (results[i]) return results[i];
        }
        return null;
    }

    // ---- Notifications (F.α) ----
    // helpers: рендер отдельного уведомления, отрисовка списка, UI-wiring (bell click, mark-read, clear).

    /**
     * Добавляет новое уведомление в DOM список + обновляет badge.
     * Вызывается из window.notifications.callbacks.onNew.
     */
    function renderNotification(ev) {
        if (!ev) return;
        var list = document.getElementById('notificationsList');
        var badge = document.getElementById('notificationsBadge');
        if (!list) return;

        var severity = (ev.severity || 'info').toLowerCase();
        var time = ev.time || new Date().toISOString();
        var timeStr = new Date(time).toLocaleTimeString(window.I18N && I18N.getLang() === 'ru' ? 'ru' : 'en');
        var source = ev.source ? '<span class="notification-source">' + Utils.escapeHtml(ev.source) + '</span>' : '';
        var severityLabel = window.I18N ? I18N.t('notifications.severity.' + severity) : severity;

        var li = document.createElement('li');
        li.className = 'notifications-item notifications-item-' + severity;
        li.dataset.eventTime = time;
        li.innerHTML =
            '<div class="notifications-item-header">' +
                '<span class="notifications-severity notifications-severity-' + severity + '">' + Utils.escapeHtml(severityLabel) + '</span>' +
                source +
                '<span class="notifications-time">' + Utils.escapeHtml(timeStr) + '</span>' +
            '</div>' +
            '<div class="notifications-message">' + Utils.escapeHtml(ev.message || '') + '</div>';

        list.insertBefore(li, list.firstChild);

        // Лимит 50 видимых (сервер держит 100 в ring buffer; UI ограничиваем сильнее).
        while (list.children.length > 50) {
            list.removeChild(list.lastChild);
        }

        // Badge — непрочитанные = общее число видимых (read state мы не храним, по требованию F.α — простая индикация).
        if (badge) {
            var unread = list.querySelectorAll('.notifications-item').length;
            if (unread > 0) {
                badge.textContent = unread > 99 ? '99+' : String(unread);
                badge.hidden = false;
            } else {
                badge.hidden = true;
            }
        }
    }

    /**
     * Перерисовка всего списка (после onClear из notifications.js).
     * Источник истины — DOM внутри #notificationsList; здесь мы только чистим + badge.
     */
    function renderNotificationsList() {
        var list = document.getElementById('notificationsList');
        var badge = document.getElementById('notificationsBadge');
        if (!list) return;
        list.innerHTML = '';
        if (badge) {
            badge.hidden = true;
            badge.textContent = '0';
        }
        // Empty-state placeholder
        var empty = document.createElement('li');
        empty.className = 'notifications-empty';
        empty.textContent = window.I18N ? I18N.t('notifications.empty') : 'No notifications';
        list.appendChild(empty);
    }

    /**
     * Подключает обработчики к bell-кнопке, dropdown, mark-read, clear.
     * Вызывается один раз в init() после window.notifications.init().
     */
    function setupNotificationsUI() {
        var btn = document.getElementById('notificationsBtn');
        var dropdown = document.getElementById('notificationsDropdown');
        var markReadBtn = document.getElementById('notificationsMarkRead');
        var clearBtn = document.getElementById('notificationsClear');
        var list = document.getElementById('notificationsList');

        if (btn && dropdown) {
            // 2026-06-29 v3: Portal-паттерн. Переносим .notifications-dropdown
            // в document.body, чтобы обойти containing block от родителей с
            // backdrop-filter / transform / filter / will-change / contain
            // (в частности .header использует backdrop-filter, а иконки —
            // transform: scale при hover; оба ломают "position: fixed").
            // После переноса position: fixed гарантированно привязан к
            // viewport, а z-index 2147483000 попадает в корневой stacking
            // context и не "проваливается" под вкладки.
            if (dropdown.parentNode !== document.body) {
                document.body.appendChild(dropdown);
            }

            function positionDropdown() {
                // Не пересчитываем на мобильных — там CSS bottom-sheet
                if (window.matchMedia && window.matchMedia('(max-width: 768px)').matches) {
                    return;
                }
                var rect = btn.getBoundingClientRect();
                var dropdownW = dropdown.offsetWidth || 380;
                var dropdownH = dropdown.offsetHeight || 480;
                var margin = 8;
                // top: сразу под кнопкой
                var top = rect.bottom + margin;
                // right: правый край выравнивается с правым краем кнопки
                var right = Math.max(margin, window.innerWidth - rect.right);
                // Если dropdown не помещается снизу — открыть вверх
                if (top + dropdownH > window.innerHeight - margin) {
                    top = Math.max(margin, rect.top - dropdownH - margin);
                }
                // Если dropdown не помещается слева — сдвинуть к левому краю
                if (right + dropdownW > window.innerWidth - margin) {
                    right = margin;
                }
                dropdown.style.top = top + 'px';
                dropdown.style.right = right + 'px';
                dropdown.style.left = 'auto';
                dropdown.style.bottom = 'auto';
            }
            btn.addEventListener('click', function (e) {
                e.stopPropagation();
                var isOpen = !dropdown.hidden;
                if (!isOpen) {
                    // Открытие — сначала показываем для измерения, потом позиционируем
                    dropdown.hidden = false;
                    positionDropdown();
                } else {
                    dropdown.hidden = true;
                    // Очищаем inline-position для корректной работы mobile @media
                    dropdown.style.top = '';
                    dropdown.style.right = '';
                    dropdown.style.left = '';
                    dropdown.style.bottom = '';
                }
                btn.setAttribute('aria-expanded', String(!isOpen));
            });
            // При ресайзе пересчитываем позицию, если dropdown открыт
            window.addEventListener('resize', function () {
                if (!dropdown.hidden) positionDropdown();
            });
            // Закрываем dropdown при клике снаружи
            document.addEventListener('click', function (e) {
                if (dropdown.hidden) return;
                if (e.target === btn || (btn.contains && btn.contains(e.target))) return;
                if (dropdown.contains && dropdown.contains(e.target)) return;
                dropdown.hidden = true;
                dropdown.style.top = '';
                dropdown.style.right = '';
                dropdown.style.left = '';
                dropdown.style.bottom = '';
                btn.setAttribute('aria-expanded', 'false');
            });
            // Esc закрывает dropdown
            document.addEventListener('keydown', function (e) {
                if (e.key === 'Escape' && !dropdown.hidden) {
                    dropdown.hidden = true;
                    dropdown.style.top = '';
                    dropdown.style.right = '';
                    dropdown.style.left = '';
                    dropdown.style.bottom = '';
                    btn.setAttribute('aria-expanded', 'false');
                }
            });
        }

        if (markReadBtn) {
            markReadBtn.addEventListener('click', function (e) {
                e.stopPropagation();
                var badge = document.getElementById('notificationsBadge');
                if (badge) {
                    badge.hidden = true;
                    badge.textContent = '0';
                }
                if (list) {
                    var items = list.querySelectorAll('.notifications-item');
                    items.forEach(function (el) { el.classList.add('notifications-item-read'); });
                }
            });
        }

        if (clearBtn) {
            clearBtn.addEventListener('click', function (e) {
                e.stopPropagation();
                if (window.notifications && typeof window.notifications.clear === 'function') {
                    window.notifications.clear();
                } else {
                    renderNotificationsList();
                }
            });
        }

        // Initial render — empty state
        if (list && list.children.length === 0) {
            renderNotificationsList();
        }
    }

    // ---- Public API ----
    return {
        init,
        editBackend,
        confirmDeleteBackend,
        openModelManageModal,
        executeModelOperation,
        loadBackendModels,
        refreshModelOpsStatus,
        closeModelManageModal,
        showAgentDetails,
        fetchAgents,
        restartAgent,
        viewAgentLogs,
        // Models tab UI helpers (Roadmap Q3 W3-4: Filter/Search)
        filterModels,
        applyModelsSort,
        // Models tab Pull progress (Roadmap Q3 W3-4 sub-task)
        startOpsAutoRefresh,
        stopOpsAutoRefresh,
        cancelOperationByKey
    };

})();


// Auto-init when DOM ready
document.addEventListener('DOMContentLoaded', () => ui.init());

// ---- Global helpers for model card actions (used by renderers.js modelsGrid) ----
window.modelCardAction = function(operation, backendId, modelName) {
    if (!backendId || !modelName) return;
    if (operation === 'delete') {
        if (!confirm((window.I18N ? I18N.t('models.confirm_delete') : 'Delete model') + ' ' + modelName + '?')) return;
    }
    ui.executeModelOperation(backendId, operation, modelName);
};

// ---- Q3 W4 — Session 19: Model details modal ----
// Открывает модальное окно с подробностями модели (architecture, family, params,
// quantization, n_ctx, gpu_layers, state) — через cluster-level endpoint
// GET /api/v1/cluster/models/{name}/info (см. internal/api/handlers_cluster_model_info.go).
// Один запрос агрегирует данные со всех бэкендов кластера (cppworker + ollama).
//
// Round 18 (2026-07-10): для llama.cpp бэкенда перенаправляем на GGUF Models tab
// с pre-selected backend вместо модала. Причина: cluster-models-info возвращает
// «parameter_size: 0.0B / quantization: unknown» для cppworker (cppworker их не
// отдаёт), а на GGUF tab оператор видит реальные load options, file management,
// batch/parallel/kv_cache controls и per-model profiles — всё что нужно для
// работы с cppworker моделью.
window.openModelDetailsModal = function(modelName, backendId, backendType) {
    if (!modelName) return;
    // Round 18: если backend llama.cpp → редирект на GGUF tab с pre-selected backend.
    if (backendType === 'llama_cpp' && backendId) {
        // Переключаемся на GGUF tab через nav link.
        var navLink = document.querySelector('[data-page="gguf"]');
        if (navLink) navLink.click();
        // Pre-select backend после того как GgufRenderer отрендерит.
        setTimeout(function() {
            if (window.GgufRenderer && typeof window.GgufRenderer.selectBackend === 'function') {
                window.GgufRenderer.selectBackend(backendId);
            } else {
                console.warn('[openModelDetailsModal] GgufRenderer.selectBackend unavailable');
            }
        }, 100);
        return;
    }
    // Ollama (или backend type неизвестен) → показываем модал как раньше.
    // Modal DOM может быть ещё не создан — создаём лениво.
    ensureModelDetailsModalDom();
    const modal = document.getElementById('modelDetailsModal');
    const body = document.getElementById('modelDetailsBody');
    const title = document.getElementById('modelDetailsTitle');
    if (!modal || !body || !title) {
        console.warn('model details modal DOM not found');
        return;
    }
    title.textContent = (window.I18N ? I18N.t('models.details.title') : 'Model details') + ' — ' + modelName;
    body.innerHTML = '<div class="loading">' +
        (window.I18N ? I18N.t('models.details.loading') : 'Loading details...') + '</div>';
    modal.style.display = 'flex';

    Api.clusterModels.info(modelName).then(function(resp) {
        renderModelDetailsBody(body, modelName, resp);
    }).catch(function(err) {
        body.innerHTML = '<div class="error-message">' +
            (window.I18N ? I18N.t('models.details.error') : 'Failed to load details') + ': ' +
            Utils.escapeHtml(err && err.message ? err.message : err) + '</div>';
    });
};

// ensureModelDetailsModalDom — создаёт модальное окно в <body> если ещё не существует.
//
// Используем ленивую инициализацию, чтобы не раздувать webui/index.html.
function ensureModelDetailsModalDom() {
    let modal = document.getElementById('modelDetailsModal');
    if (modal) return;
    modal = document.createElement('div');
    modal.id = 'modelDetailsModal';
    modal.className = 'modal-overlay';
    modal.style.display = 'none';
    modal.innerHTML = `
        <div class="modal-window model-details-window" role="dialog" aria-modal="true" aria-labelledby="modelDetailsTitle">
            <div class="modal-header">
                <h2 id="modelDetailsTitle">${Utils.escapeHtml(window.I18N ? I18N.t('models.details.title') : 'Model details')}</h2>
                <button class="modal-close" id="modelDetailsClose" aria-label="Close">×</button>
            </div>
            <div class="modal-body" id="modelDetailsBody">
                <div class="loading">...</div>
            </div>
        </div>
    `;
    document.body.appendChild(modal);
    // Закрытие по клику на крестик или backdrop.
    document.getElementById('modelDetailsClose').addEventListener('click', closeModelDetailsModal);
    modal.addEventListener('click', function(ev) {
        if (ev.target === modal) closeModelDetailsModal();
    });
    // Закрытие по Escape.
    document.addEventListener('keydown', function(ev) {
        if (ev.key === 'Escape') closeModelDetailsModal();
    });
}

function closeModelDetailsModal() {
    const modal = document.getElementById('modelDetailsModal');
    if (modal) modal.style.display = 'none';
}

// renderModelDetailsBody — рисует содержимое модального окна по ответу
// /api/v1/cluster/models/{name}/info.
//
// Структура ответа:
//   {
//     model: string,
//     count: number,
//     okCount: number,
//     backends: [
//       { backendId, backendType, status, info: { details, model_info, ... }, error?, httpStatus? }
//     ]
//   }
//
// Семантика:
//   - Если есть хотя бы один backend со status=ok, рендерим секцию «Primary» с данными info.
//   - Если есть несколько ok — рендерим несколько секций, чтобы показать расхождения
//     (например, разные n_ctx / gpu_layers на разных бэкендах).
//   - Если okCount == 0 — рендерим список ошибок per-backend.
function renderModelDetailsBody(body, modelName, resp) {
    if (!resp || !resp.backends || !resp.backends.length) {
        body.innerHTML = '<div class="error-message">' +
            (window.I18N ? I18N.t('models.details.no_backends') : 'No backends in cluster') + '</div>';
        return;
    }
    const okBackends = resp.backends.filter(function(b) { return b.status === 'ok'; });
    const otherBackends = resp.backends.filter(function(b) { return b.status !== 'ok'; });
    let html = '';
    if (okBackends.length > 0) {
        html += renderModelDetailsOkSection(modelName, okBackends, resp);
    } else {
        html += '<div class="warning-message">' +
            (window.I18N ? I18N.t('models.details.no_ok_backends') :
                'Model is not loaded on any backend — details unavailable.') + '</div>';
    }
    if (otherBackends.length > 0) {
        html += renderModelDetailsBackendsList(otherBackends, /* onlyIssues */ true);
    }
    body.innerHTML = html;
}

// renderModelDetailsOkSection — рендерит секции «General / Capabilities / Model Info» для ok-бэкендов.
//
// Если ok-бэкендов несколько, рендерим каждый отдельно (с заголовком backendId),
// чтобы пользователь видел расхождения в параметрах.
function renderModelDetailsOkSection(modelName, okBackends, resp) {
    const _t = function(k, fb) { return (window.I18N ? I18N.t(k) : fb); };
    let html = '<div class="model-details-summary">';
    html += '<div class="model-details-row"><span class="label">' + _t('models.details.summary_model', 'Model') + ':</span>' +
        '<span class="value"><code>' + Utils.escapeHtml(modelName) + '</code></span></div>';
    html += '<div class="model-details-row"><span class="label">' + _t('models.details.summary_count', 'Reported by') + ':</span>' +
        '<span class="value">' + resp.okCount + ' / ' + resp.count + ' ' + _t('models.details.summary_backends', 'backends') + '</span></div>';
    html += '</div>';
    okBackends.forEach(function(b) {
        const info = b.info || {};
        const details = info.details || {};
        const modelInfo = info.model_info || {};
        const backendLabel = Utils.escapeHtml(b.backendId) +
            ' <span class="badge badge-info">' + Utils.escapeHtml(b.backendType) + '</span>';
        html += '<h3 class="model-details-backend-title">' + backendLabel + '</h3>';
        html += '<div class="model-details-section">';
        html += '<h4>' + _t('models.details.section_general', 'General') + '</h4>';
        html += modelDetailsRow('Family', details.family || modelInfo.architecture);
        html += modelDetailsRow('Format', details.format);
        html += modelDetailsRow('Parameter Size', details.parameter_size || info.parameters);
        html += modelDetailsRow('Quantization', details.quantization_level);
        html += '</div>';
        html += '<div class="model-details-section">';
        html += '<h4>' + _t('models.details.section_capabilities', 'Capabilities') + '</h4>';
        html += modelDetailsRow('Architecture', modelInfo.architecture);
        html += modelDetailsRow('Layers (n_layers)', modelInfo.n_layers);
        html += modelDetailsRow('Heads (n_heads)', modelInfo.n_heads);
        html += modelDetailsRow('Embedding size (n_embd)', modelInfo.n_embd);
        html += modelDetailsRow('Vocab size (n_vocab)', modelInfo.n_vocab);
        html += '</div>';
        html += '<div class="model-details-section">';
        html += '<h4>' + _t('models.details.section_runtime', 'Runtime / Load') + '</h4>';
        html += modelDetailsRow('Context size (n_ctx)', modelInfo.context_size);
        html += modelDetailsRow('GPU layers', modelInfo.gpu_layers);
        html += modelDetailsRow('Batch size', modelInfo.batch_size);
        html += modelDetailsRow('Parallel (n_parallel)', modelInfo.parallel);
        html += modelDetailsRow('KV cache type', modelInfo.kv_cache_type);
        html += modelDetailsRow('Flash attention', modelInfo.flash_attn);
        html += modelDetailsRow('Use mmap', modelInfo.use_mmap);
        html += modelDetailsRow('NUMA', modelInfo.numa);
        html += modelDetailsRow('State', modelInfo.state);
        html += modelDetailsRow('Size (bytes)', modelInfo.size_bytes);
        html += modelDetailsRow('Modified at', modelInfo.modified_at);
        html += '</div>';
    });
    return html;
}

function modelDetailsRow(label, value) {
    if (value === undefined || value === null || value === '') return '';
    const _t = function(k, fb) { return (window.I18N ? I18N.t(k) : fb); };
    let display = value;
    if (typeof value === 'object') {
        display = JSON.stringify(value);
    }
    return '<div class="model-details-row"><span class="label">' + Utils.escapeHtml(label) + ':</span>' +
        '<span class="value">' + Utils.escapeHtml(String(display)) + '</span></div>';
}

// renderModelDetailsBackendsList — список проблемных бэкендов (error/not_found/unavailable).
function renderModelDetailsBackendsList(backends, onlyIssues) {
    const _t = function(k, fb) { return (window.I18N ? I18N.t(k) : fb); };
    let html = '<h3>' + _t('models.details.section_other_backends', 'Other backends') + '</h3>';
    html += '<ul class="model-details-backends-list">';
    backends.forEach(function(b) {
        const cls = b.status === 'ok' ? 'success' : (b.status === 'not_found' ? 'info' : 'danger');
        html += '<li class="model-details-backend-item">';
        html += '<span class="badge badge-' + cls + '">' + Utils.escapeHtml(b.status) + '</span> ';
        html += '<strong>' + Utils.escapeHtml(b.backendId) + '</strong> ' +
                '<span class="model-details-backend-type">(' + Utils.escapeHtml(b.backendType) + ')</span>';
        if (b.error) html += ' — ' + Utils.escapeHtml(b.error);
        html += '</li>';
    });
    html += '</ul>';
    return html;
}

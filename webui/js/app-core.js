/**
 * app-core.js — Theme / Density / i18n / AutoTune toast helpers.
 *
 * R57.2 (2026-09-03): extracted from webui/js/app.js.
 *
 * Этот файл разделяет с app.js namespace `window.App` (classic-script pattern,
 * НЕ ES modules). Загружается ДО app.js (см. webui/index.html).
 *
 * Контракт:
 *   - window.App = { ... } — shared namespace
 *   - window.App.state.currentPage — текущая страница (read/write из app.js)
 *   - window.App.initTheme / initDensity / setupI18n / showToast / initAutoTuneEventHandlers
 *
 * Зависимости (предполагаются уже загруженными к моменту выполнения):
 *   - window.Utils.escapeHtml (utils.js)
 *   - window.I18N.t / I18N.getLang / I18N.setLang (i18n/index.js)
 */
(function() {
    'use strict';

    const App = (window.App = window.App || {});

    // Shared state. app.js читает/пишет currentPage.
    if (!App.state) {
        App.state = {
            currentPage: 'dashboard'
        };
    }

    // ---- Theme ----

    const THEME_ICONS = { dark: 'fa-moon', light: 'fa-sun', linear: 'fa-circle', nvidia: 'fa-bold', vercel: 'fa-arrow-up' };
    const THEME_LETTERS = { dark: '', light: '', linear: 'L', nvidia: 'N', vercel: 'V' };
    const THEME_LABELS = { dark: 'Dark', light: 'Light', linear: 'Linear', nvidia: 'NVIDIA', vercel: 'Vercel' };

    /**
     * initTheme — Session C Theme toggle (R55.10 update: 7 swatch themes).
     * Anti-FOIT inline script в <head> уже установил data-theme до загрузки CSS
     * (приоритет: localStorage > system preference > dark). Здесь синхронизируем
     * UI (иконка toggle) + биндим button click / Ctrl+Shift+T / matchMedia listener.
     * Broadcast: window event 'theme:changed' с detail={theme}.
     */
    App.initTheme = function() {
        var current = document.documentElement.getAttribute('data-theme') || 'dark';
        App._updateThemeToggleIcon(current);

        function broadcastThemeChange(theme) {
            try {
                window.dispatchEvent(new CustomEvent('theme:changed', { detail: { theme: theme } }));
            } catch (e) { /* ignore */ }
        }

        function switchTheme(next) {
            document.documentElement.setAttribute('data-theme', next);
            try { localStorage.setItem('ollamalegion_theme', next); } catch (e) { /* ignore */ }
            App._updateThemeToggleIcon(next);
            broadcastThemeChange(next);
        }
        // R55.10: expose switchTheme globally so theme picker can reuse it.
        window.ollamalegion_switchTheme = switchTheme;

        var THEMES = ['dark', 'light', 'linear', 'nvidia', 'vercel', 'sentry', 'mint'];
        var toggleBtn = document.getElementById('themeToggle');
        if (toggleBtn) {
            toggleBtn.addEventListener('click', function () {
                var cur = document.documentElement.getAttribute('data-theme') || 'dark';
                var idx = THEMES.indexOf(cur);
                var next = THEMES[(idx + 1) % THEMES.length];
                switchTheme(next);
            });
        }

        document.addEventListener('keydown', function (e) {
            var isToggleShortcut = (e.ctrlKey || e.metaKey) && e.shiftKey && (e.key === 'T' || e.key === 't' || e.key === 'Е' || e.key === 'е');
            if (!isToggleShortcut) return;
            var tag = (e.target && e.target.tagName) || '';
            if (tag === 'INPUT' || tag === 'TEXTAREA' || (e.target && e.target.isContentEditable)) return;
            e.preventDefault();
            var cur = document.documentElement.getAttribute('data-theme') || 'dark';
            var idx2 = THEMES.indexOf(cur);
            var next2 = THEMES[(idx2 + 1) % THEMES.length];
            switchTheme(next2);
        });

        if (window.matchMedia) {
            try {
                var mq = window.matchMedia('(prefers-color-scheme: light)');
                var onMqChange = function (ev) {
                    try {
                        if (localStorage.getItem('ollamalegion_theme')) return;
                    } catch (e) { /* ignore */ }
                    var next = ev.matches ? 'light' : 'dark';
                    document.documentElement.setAttribute('data-theme', next);
                    App._updateThemeToggleIcon(next);
                    broadcastThemeChange(next);
                };
                if (mq.addEventListener) mq.addEventListener('change', onMqChange);
                else if (mq.addListener) mq.addListener(onMqChange);
            } catch (e) { /* ignore */ }
        }
    };

    App._updateThemeToggleIcon = function(theme) {
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
    };

    // ---- Density (Sprint 1 data-dense variant, 2026-06-29) ----

    App.initDensity = function() {
        var stored = 'normal';
        try { stored = localStorage.getItem('ollamalegion_density') || 'normal'; } catch (e) { /* ignore */ }
        if (stored !== 'dense' && stored !== 'normal') stored = 'normal';
        document.body.setAttribute('data-density', stored);
        App._updateDensityToggleIcon(stored);

        var toggleBtn = document.getElementById('densityToggle');
        if (toggleBtn) {
            toggleBtn.addEventListener('click', function () {
                var cur = document.body.getAttribute('data-density') || 'normal';
                var next = cur === 'dense' ? 'normal' : 'dense';
                document.body.setAttribute('data-density', next);
                try { localStorage.setItem('ollamalegion_density', next); } catch (e) { /* ignore */ }
                App._updateDensityToggleIcon(next);
                try {
                    window.dispatchEvent(new CustomEvent('density:changed', { detail: { density: next } }));
                } catch (e) { /* ignore */ }
            });
        }
    };

    App._updateDensityToggleIcon = function(density) {
        var btn = document.getElementById('densityToggle');
        if (!btn) return;
        var icon = btn.querySelector('.density-icon');
        if (icon) {
            icon.className = 'density-icon ' + (density === 'dense' ? 'fas fa-table-cells-large' : 'fas fa-table-cells');
        }
        btn.classList.toggle('is-dense', density === 'dense');
        btn.setAttribute('data-density-current', density);
    };

    // ---- i18n ----

    App._updateUITranslations = function() {
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
        document.querySelectorAll('[data-i18n-title]').forEach(function (el) {
            var key = el.getAttribute('data-i18n-title');
            if (key && window.I18N) {
                el.title = I18N.t(key);
            }
        });
        if (App.state.currentPage && window.I18N) {
            var titleKey = 'header.' + App.state.currentPage;
            var h1 = document.getElementById('pageTitle');
            if (h1) h1.textContent = I18N.t(titleKey);
        }
    };

    App.setupI18n = function() {
        var langSelect = document.getElementById('langSelect');
        if (langSelect && window.I18N) {
            var currentLang = I18N.getLang();
            langSelect.value = currentLang;
            langSelect.addEventListener('change', function () {
                I18N.setLang(this.value);
                App._updateUITranslations();
                App._updateThemeToggleIcon(document.documentElement.getAttribute('data-theme') || 'dark');
                App.showToast(window.I18N ? I18N.t('app.lang_changed') : 'Language changed', 'success');
            });
        }
        window.addEventListener('i18n:changed', function () {
            App._updateUITranslations();
            App._updateThemeToggleIcon(document.documentElement.getAttribute('data-theme') || 'dark');
        });
        document.querySelectorAll('[data-i18n-placeholder]').forEach(function (el) {
            var key = el.getAttribute('data-i18n-placeholder');
            if (key && window.I18N) {
                el.placeholder = I18N.t(key);
            }
        });
        App._updateUITranslations();
    };

    // ---- AutoTune WebSocket → toast notification ----
    // R54.8 (2026-08-24): AutoTune WebSocket handler — toast notifications
    // when autonomous reload triggers/succeeds/fails. Listens ws-message events
    // (WebSocketManager в modules/websocket.js диспатчит их после JSON.parse).

    App.initAutoTuneEventHandlers = function() {
        window.addEventListener('ws-message', function(e) {
            const ev = e.detail || {};
            if (!ev || !ev.eventType) return;
            const modelLabel = ev.model || (ev.data && ev.data.model) || 'model';
            const msgText = ev.message || (ev.data && ev.data.reason) || '';
            if (ev.eventType === 'autotune_reload_triggered') {
                App.showToast('🔄 AutoTune: reloading ' + modelLabel + '...', 'info', 5000);
            } else if (ev.eventType === 'autotune_reload_succeeded') {
                App.showToast('✅ AutoTune: ' + modelLabel + ' reloaded — ' + msgText, 'success', 8000);
            } else if (ev.eventType === 'autotune_reload_failed') {
                App.showToast('⚠️ AutoTune: ' + modelLabel + ' reload failed — ' + msgText, 'error', 12000);
            } else if (ev.eventType === 'autotune_circuit_open') {
                App.showToast('🛑 AutoTune: circuit breaker OPEN (3 fails). Manual apply required.', 'warning', 30000);
            }
        });
    };

    // ---- Generic toast helper ----
    // До R57.2 был L2308-L2321 в app.js. Дубликат на L32 (autotune-specific) перекрывался
    // этим определением из-за hoisting. После split: только эта каноническая версия
    // живёт в app-core.js, initAutoTuneEventHandlers вызывает её напрямую.
    App.showToast = function(message, type, timeoutMs) {
        const container = document.getElementById('toastContainer');
        if (!container) return;
        const toast = document.createElement('div');
        toast.className = 'toast ' + (type || 'info');
        toast.innerHTML = '<span>' + (window.Utils ? Utils.escapeHtml(message) : message) + '</span>';
        container.appendChild(toast);

        setTimeout(function() {
            toast.style.opacity = '0';
            toast.style.transform = 'translateX(100%)';
            setTimeout(function() { toast.remove(); }, 300);
        }, timeoutMs || 4000);
    };

    // Aliases для backward compat (старое имя без подчёркивания для публичного API,
    // с подчёркиванием — internal helpers, вызываемые из event handlers).
    App.updateThemeToggleIcon = App._updateThemeToggleIcon;
    App.updateDensityToggleIcon = App._updateDensityToggleIcon;
    App.updateUITranslations = App._updateUITranslations;
})();

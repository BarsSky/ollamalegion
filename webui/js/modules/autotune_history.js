// autotune_history.js — R55.2 (2026-08-24): AutoTune history timeline view.
//
// Phase 5 (timeline): показывает последние AutoTune events (triggered /
// succeeded / failed / circuit_open) для текущего backend.
// Использует /api/v1/admin/autotune/history endpoint (ring buffer, R55.2 backend).
//
// UI: кнопка "View History" рядом с AutoTune badge → клик открывает
// panel с timeline (newest first). Без dependencies на WebUI core.

(function() {
    'use strict';

    /**
     * Severity → CSS class (как в autotune.js).
     */
    const SEVERITY_CSS = {
        'ok': 'autotune-history-ok',
        'info': 'autotune-history-info',
        'warning': 'autotune-history-warning',
        'error': 'autotune-history-error',
        'critical': 'autotune-history-critical'
    };

    /**
     * Type → user-friendly label + icon.
     */
    const TYPE_META = {
        'autotune_reload_triggered': { label: 'Reload triggered', icon: '🔄' },
        'autotune_reload_succeeded': { label: 'Reload succeeded', icon: '✅' },
        'autotune_reload_failed':    { label: 'Reload failed',    icon: '⚠️' },
        'autotune_circuit_open':     { label: 'Circuit open',     icon: '🛑' }
    };

    /**
     * Fetch history from API.
     * @param {string} backendId — filter by backend (optional, '' = all)
     * @param {number} limit — max entries (default 100)
     * @returns {Promise<{entries: Array, stats: Object}>}
     */
    async function fetchHistory(backendId, limit) {
        const params = new URLSearchParams();
        if (limit) params.set('limit', String(limit));
        if (backendId) params.set('backend', backendId);
        const url = '/api/v1/admin/autotune/history?' + params.toString();
        const resp = await fetch(url, {
            headers: window.Api && window.Api.authHeaders
                ? window.Api.authHeaders()
                : {}
        });
        if (!resp.ok) {
            throw new Error('HTTP ' + resp.status + ' ' + resp.statusText);
        }
        return resp.json();
    }

    /**
     * Format timestamp для UI (короткий формат: HH:MM:SS).
     */
    function formatTime(ts) {
        if (!ts) return '—';
        try {
            const d = new Date(ts);
            return d.toLocaleTimeString();
        } catch (e) {
            return String(ts);
        }
    }

    /**
     * Format relative time (e.g., "5m ago", "2h ago").
     */
    function formatRelative(ts) {
        if (!ts) return '—';
        try {
            const d = new Date(ts);
            const diff = Date.now() - d.getTime();
            if (diff < 60000) return Math.floor(diff / 1000) + 's ago';
            if (diff < 3600000) return Math.floor(diff / 60000) + 'm ago';
            if (diff < 86400000) return Math.floor(diff / 3600000) + 'h ago';
            return Math.floor(diff / 86400000) + 'd ago';
        } catch (e) {
            return String(ts);
        }
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

    /**
     * Render single history entry (timeline row).
     */
    function renderEntry(entry) {
        const meta = TYPE_META[entry.type] || { label: entry.type, icon: '•' };
        const cssClass = SEVERITY_CSS[entry.severity] || SEVERITY_CSS.info;
        const params = [];
        if (entry.data) {
            if (entry.data.contextSize) params.push('n_ctx=' + entry.data.contextSize);
            if (entry.data.kvCacheType) params.push('kv=' + entry.data.kvCacheType);
            if (entry.data.numGpuLayers !== undefined) params.push('layers=' + entry.data.numGpuLayers);
        }
        const paramsLine = params.length > 0
            ? '<div class="autotune-history-params">' + escapeHtml(params.join(' · ')) + '</div>'
            : '';
        return `
            <div class="autotune-history-entry ${cssClass}">
                <div class="autotune-history-time" title="${escapeHtml(entry.timestamp)}">${escapeHtml(formatTime(entry.timestamp))}</div>
                <div class="autotune-history-icon">${meta.icon}</div>
                <div class="autotune-history-body">
                    <div class="autotune-history-title">${escapeHtml(meta.label)} <span class="autotune-history-model">${escapeHtml(entry.model || '')}</span></div>
                    <div class="autotune-history-message">${escapeHtml(entry.message || '')}</div>
                    ${paramsLine}
                </div>
            </div>
        `;
    }

    /**
     * Render the history panel.
     * @param {string} backendId — filter by backend (empty = all)
     * @param {string} containerId — где вставить panel
     */
    async function renderHistoryPanel(backendId, containerId) {
        const container = document.getElementById(containerId);
        if (!container) return;
        // Loading state
        container.innerHTML = '<div class="autotune-history-loading">Loading history…</div>';

        let data;
        try {
            data = await fetchHistory(backendId, 100);
        } catch (e) {
            container.innerHTML = '<div class="autotune-history-error">Failed to load: ' + escapeHtml(String(e)) + '</div>';
            return;
        }

        const entries = data.entries || [];
        const stats = data.stats || {};

        if (entries.length === 0) {
            container.innerHTML = '<div class="autotune-history-empty">No AutoTune events yet. Events appear here when AutoTune triggers a reload.</div>';
            return;
        }

        const header = `
            <div class="autotune-history-header">
                <div class="autotune-history-stats">
                    ${entries.length} event${entries.length === 1 ? '' : 's'} (${stats.size}/${stats.maxSize} in ring buffer, ${stats.droppedCount} dropped)
                </div>
                <button class="autotune-history-refresh" data-backend="${escapeHtml(backendId)}" data-container="${escapeHtml(containerId)}">↻ Refresh</button>
            </div>
        `;

        const timeline = entries.map(renderEntry).join('');
        container.innerHTML = header + '<div class="autotune-history-timeline">' + timeline + '</div>';

        // Wire refresh button
        const refreshBtn = container.querySelector('.autotune-history-refresh');
        if (refreshBtn) {
            refreshBtn.addEventListener('click', () => {
                renderHistoryPanel(refreshBtn.dataset.backend, refreshBtn.dataset.container);
            });
        }
    }

    /**
     * Inject "View History" button next to existing AutoTune badge.
     * Returns HTML string (to be appended by caller).
     */
    function renderHistoryButton(backendId, containerId) {
        const targetContainer = containerId || 'autotune-history-' + backendId;
        return `<button class="autotune-history-btn" data-backend="${escapeHtml(backendId)}" data-container="${escapeHtml(targetContainer)}">📜 View History</button>`;
    }

    /**
     * Setup handler для всех .autotune-history-btn кнопок на странице.
     * При клике — open modal с history panel.
     */
    function initHistoryButtons() {
        document.addEventListener('click', function(e) {
            const btn = e.target.closest('.autotune-history-btn');
            if (!btn) return;
            e.preventDefault();
            const backendId = btn.dataset.backend || '';
            const containerId = btn.dataset.container || ('autotune-history-' + backendId);
            openHistoryModal(backendId, containerId);
        });
    }

    /**
     * Open a simple modal с history panel внутри.
     */
    function openHistoryModal(backendId, containerId) {
        // Close existing modal if open
        const existing = document.getElementById('autotune-history-modal');
        if (existing) existing.remove();

        const modal = document.createElement('div');
        modal.id = 'autotune-history-modal';
        modal.className = 'autotune-history-modal-backdrop';
        modal.innerHTML = `
            <div class="autotune-history-modal">
                <div class="autotune-history-modal-header">
                    <h3>AutoTune History${backendId ? ' — ' + escapeHtml(backendId) : ''}</h3>
                    <button class="autotune-history-modal-close">×</button>
                </div>
                <div class="autotune-history-modal-body">
                    <div id="${escapeHtml(containerId)}"></div>
                </div>
            </div>
        `;
        document.body.appendChild(modal);

        // Close handlers
        modal.querySelector('.autotune-history-modal-close').addEventListener('click', () => {
            modal.remove();
        });
        modal.addEventListener('click', (e) => {
            if (e.target === modal) modal.remove();
        });

        // Render history inside the modal
        renderHistoryPanel(backendId, containerId);
    }

    // Public API
    window.AutoTuneHistory = {
        renderHistoryPanel: renderHistoryPanel,
        renderHistoryButton: renderHistoryButton,
        initHistoryButtons: initHistoryButtons,
        fetchHistory: fetchHistory
    };
})();

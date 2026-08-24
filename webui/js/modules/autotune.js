// autotune.js — R54.5 (2026-08-24): WebUI AutoTune visibility.
//
// Phase 1 (read-only): показывает AutoTune status per backend/model.
// Использует данные из /api/v1/backends response (b.llamaCpp.autoTune + autoTuneEnabled).
//
// Phase 2 (apply): будет POST /api/v1/admin/autotune/{backendID}/apply для manual apply.
// Phase 3 (settings): Settings page integration.
// Phase 4 (websocket): live updates.
//
// State: backend.autoTune is set by balancer (R54.1):
// {
//   "autoTuneEnabled": true,
//   "overallSeverity": "info|warning|critical|ok",
//   "models": [{
//     "modelName": "...",
//     "isSubOptimal": true,
//     "autoTuneEnabled": true,
//     "recommendations": [
//       { "severity": "info", "category": "context|kv_cache|layers",
//         "message": "...", "suggestion": "..." }
//     ]
//   }]
// }

(function() {
    'use strict';

    /**
     * AutoTuneSeverity → CSS class name (color)
     * ok: green
     * info: blue
     * warning: orange
     * critical: red
     */
    const SEVERITY_CSS = {
        'ok': 'autotune-severity-ok',
        'info': 'autotune-severity-info',
        'warning': 'autotune-severity-warning',
        'critical': 'autotune-severity-critical'
    };

    const SEVERITY_ICON = {
        'ok': '✓',
        'info': 'ℹ',
        'warning': '⚠',
        'critical': '⛔'
    };

    /**
     * Render AutoTuneBadge for a single backend.
     * Returns HTML string. Returns '' if backend has no autoTune field (older balancer).
     */
    function renderBackendAutoTune(backend) {
        if (!backend) return '';
        const at = backend.llamaCpp?.autoTune || backend.autoTune;
        if (!at) return '';

        const overall = at.overallSeverity || 'ok';
        const cssClass = SEVERITY_CSS[overall] || SEVERITY_CSS.info;
        const icon = SEVERITY_ICON[overall] || '?';
        const enabled = at.autoTuneEnabled;
        const summary = at.overallSummary || '';
        const subModels = (at.models || []).filter(m => m.isSubOptimal);
        const recCount = subModels.reduce((sum, m) => sum + (m.recommendations || []).length, 0);

        // Models section (if any sub-optimal)
        let modelsHtml = '';
        if (subModels.length > 0) {
            modelsHtml = '<div class="autotune-models">';
            subModels.forEach(m => {
                const recs = (m.recommendations || []).map(r => {
                    const recSeverityClass = SEVERITY_CSS[r.severity] || SEVERITY_CSS.info;
                    return `
                        <div class="autotune-rec ${recSeverityClass}">
                            <span class="autotune-rec-icon">${SEVERITY_ICON[r.severity] || '?'}</span>
                            <span class="autotune-rec-cat">[${r.category || '?'}]</span>
                            <span class="autotune-rec-msg">${escapeHtml(r.message || '')}</span>
                            <div class="autotune-rec-sugg">${escapeHtml(r.suggestion || '')}</div>
                        </div>
                    `;
                }).join('');
                modelsHtml += `
                    <div class="autotune-model">
                        <strong>${escapeHtml(m.modelName || '?')}</strong>
                        <span class="autotune-rec-count">${recs ? (m.recommendations || []).length : 0} rec</span>
                        <div class="autotune-recs">${recs}</div>
                    </div>
                `;
            });
            modelsHtml += '</div>';
        }

        const statusLabel = enabled
            ? '<span class="autotune-enabled">AutoTune: ON</span>'
            : '<span class="autotune-disabled">AutoTune: OFF (manual)</span>';

        const historyBtn = window.AutoTuneHistory
            ? window.AutoTuneHistory.renderHistoryButton(backend.id || backend.ID || '')
            : '';

        return `
            <div class="autotune-card ${cssClass}">
                <div class="autotune-header">
                    <span class="autotune-icon">${icon}</span>
                    <strong>${statusLabel}</strong>
                    <span class="autotune-summary">${escapeHtml(summary)}</span>
                    ${historyBtn}
                </div>
                ${modelsHtml}
            </div>
        `;
    }

    /**
     * Render per-model AutoTune badge (for Models page, inline next to loadedModel card).
     * Returns HTML string. Returns '' if no sub-optimal.
     */
    function renderModelAutoTuneBadge(modelAnalysis) {
        if (!modelAnalysis || !modelAnalysis.isSubOptimal) return '';
        const recs = modelAnalysis.recommendations || [];
        const recCount = recs.length;
        if (recCount === 0) return '';
        // Worst severity
        const severityOrder = { 'ok': 0, 'info': 1, 'warning': 2, 'critical': 3 };
        let worst = 'info';
        recs.forEach(r => {
            if ((severityOrder[r.severity] || 0) > (severityOrder[worst] || 0)) {
                worst = r.severity;
            }
        });
        const cssClass = SEVERITY_CSS[worst] || SEVERITY_CSS.info;
        const icon = SEVERITY_ICON[worst] || '?';
        const autoTuneEnabled = modelAnalysis.autoTuneEnabled !== false;
        const tooltip = recs.map(r => escapeHtml(r.suggestion || r.message || '')).join(' • ');

        return `
            <span class="autotune-badge ${cssClass}" title="${tooltip}">
                <span class="autotune-badge-icon">${icon}</span>
                <span class="autotune-badge-text">AutoTune</span>
                <span class="autotune-badge-count">${recCount}</span>
                ${autoTuneEnabled ? '' : '<span class="autotune-badge-manual">manual</span>'}
            </span>
        `;
    }

    // Helper — escape HTML entities. Reuse Utils.escapeHtml if available.
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

    // Public API
    window.AutoTuneUI = {
        renderBackendAutoTune: renderBackendAutoTune,
        renderModelAutoTuneBadge: renderModelAutoTuneBadge,
        SEVERITY_CSS: SEVERITY_CSS,
        SEVERITY_ICON: SEVERITY_ICON
    };
})();

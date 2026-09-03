/**
 * gguf-renderer-list.js — Left panel: registered backends list.
 *
 * R57.5c (2026-09-03): extracted from webui/js/modules/gguf-renderer.js.
 *
 * Содержит функции рендеринга ЛЕВОЙ ПАНЕЛИ GGUF tab:
 *   - renderBackendsPanel() — обёртка панели (header + list + footer)
 *   - renderBackendsList()  — рендерит каждый бэкенд как карточку
 *   - updateBackendsList()  — обновляет DOM списка без полного rerender
 *
 * Зависимости (должны быть загружены раньше):
 *   - window.GgufModule.state (from gguf-renderer-state.js)
 *   - window.GgufModule.visibleBackends() (from gguf-renderer-state.js)
 *   - window.Utils.escapeHtml (from utils.js)
 *   - window.I18N.t (from i18n/index.js)
 *   - window.GgufModule._ (I18N helper)
 *
 * Загружается ДО gguf-renderer.js (в defer-цепочке).
 */
(function() {
    'use strict';

    const M = (window.GgufModule = window.GgufModule || {});

    M.renderBackendsPanel = function() {
        const state = M.state;
        return '' +
            '<aside class="gguf-backends-panel">' +
                '<div class="gguf-backends-panel-header">' +
                    '<h3><i class="fas fa-network-wired"></i> ' + M._('gguf.registered_backends') + '</h3>' +
                    '<button class="gguf-link-button" id="ggufRefreshBackends" title="' + M._('gguf.refresh_backend') + '"><i class="fas fa-sync"></i></button>' +
                '</div>' +
                '<div class="gguf-backend-list" id="ggufBackendList">' +
                    M.renderBackendsList() +
                '</div>' +
                '<div style="margin-top:auto;padding-top:8px;border-top:1px solid var(--border);">' +
                    '<label style="display:flex;align-items:center;gap:6px;padding:6px 4px;font-size:12px;cursor:pointer;margin-bottom:8px;">' +
                        '<input type="checkbox" id="ggufShowUnhealthy" ' + (state.showUnhealthy ? 'checked' : '') + '>' +
                        '<span>' + (M._('gguf.show_unhealthy') || 'Показать недоступные') + '</span>' +
                    '</label>' +
                    '<button class="btn btn-sm btn-secondary" id="ggufOpenConnectModal" style="width:100%;justify-content:center;">' +
                        '<i class="fas fa-plug"></i> ' + M._('gguf.alternate_url_btn') +
                    '</button>' +
                '</div>' +
            '</aside>';
    };

    M.renderBackendsList = function() {
        const state = M.state;
        if (!state.backendDataLoaded) {
            return '<div class="gguf-empty-state">' + M._('gguf.loading_backends') + '</div>';
        }
        var visible = M.visibleBackends();
        if (!visible || visible.length === 0) {
            return '<div class="gguf-empty-state">' + M._('gguf.no_registered_backends') + '</div>';
        }
        return visible.map(function (b) {
            const isActive = state.selectedBackendId === b.id;
            const statusClass = b.status === 'healthy' ? 'connected' :
                (b.status === 'offline' ? 'disconnected' : 'warn');
            const statusLabel = b.status === 'healthy' ? M._('gguf.backend_healthy') :
                (b.status === 'offline' ? M._('gguf.backend_unreachable') : (b.status || '-'));
            const modelCount = (b.models && b.models.length) || 0;
            // Sidebar loading indicator (Issue: «отображение состояния загрузки»).
            // Если у бэкенда есть модели в state.loadingModels, показываем их под именем
            // со спиннером и elapsed-таймером: «⟳ Загружается model-name 12s».
            let loadingHtml = '';
            const loadingArr = (state.loadingModels && state.loadingModels[b.id]) || [];
            if (loadingArr.length > 0) {
                loadingHtml = loadingArr.map(function (lm) {
                    const startedAt = lm.loadingStartedAt ? new Date(lm.loadingStartedAt).getTime() : Date.now();
                    const elapsedMs = (lm.elapsedMs && lm.elapsedMs > 0)
                        ? lm.elapsedMs
                        : (Date.now() - startedAt);
                    const elapsedSec = Math.max(0, Math.floor(elapsedMs / 1000));
                    let elapsedLabel;
                    if (elapsedSec < 60) elapsedLabel = elapsedSec + 's';
                    else elapsedLabel = Math.floor(elapsedSec / 60) + 'm ' + (elapsedSec % 60) + 's';
                    let stateIcon, stateColor, stateLabel;
                    if (lm.state === 'error') {
                        stateIcon = 'fa-times-circle';
                        stateColor = 'var(--danger, #d9534f)';
                        stateLabel = M._('gguf.load_error_short') || 'Load error';
                    } else {
                        stateIcon = 'fa-spinner fa-spin';
                        stateColor = 'var(--accent, #4a9eff)';
                        stateLabel = M._('gguf.loading_indicator') || 'Loading';
                    }
                    return '<div class="gguf-backend-loading-row" style="display:flex;align-items:center;gap:6px;margin-top:4px;padding:3px 6px;background:rgba(74,158,255,0.08);border-radius:4px;font-size:11px;color:' + stateColor + ';" title="' + window.Utils.escapeHtml(lm.name) + '">' +
                        '<i class="fas ' + stateIcon + '" style="font-size:10px;flex-shrink:0;"></i>' +
                        '<span style="flex:1;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;">' +
                            stateLabel + ': ' + window.Utils.escapeHtml(lm.name) +
                        '</span>' +
                        '<span style="font-family:monospace;font-size:10px;opacity:0.85;">' + elapsedLabel + '</span>' +
                    '</div>';
                }).join('');
            }
            return '' +
                '<div class="gguf-backend-item ' + (isActive ? 'active' : '') + '" data-backend-id="' + window.Utils.escapeHtml(b.id) + '">' +
                    '<div class="gguf-backend-item-header">' +
                        '<span class="gguf-status-dot ' + statusClass + '"></span>' +
                        '<span style="flex:1;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;">' + window.Utils.escapeHtml(b.id) + '</span>' +
                        '<span class="badge" style="background:var(--llamacpp-badge);font-size:10px;padding:2px 6px;">cpp</span>' +
                    '</div>' +
                    '<div class="gguf-backend-item-meta">' +
                        '<span class="gguf-backend-item-status">' + statusLabel + '</span>' +
                        '<span><i class="fas fa-cube"></i> ' + modelCount + '</span>' +
                    '</div>' +
                    (loadingHtml ? '<div class="gguf-backend-loading-list">' + loadingHtml + '</div>' : '') +
                '</div>';
        }).join('');
    };
})();

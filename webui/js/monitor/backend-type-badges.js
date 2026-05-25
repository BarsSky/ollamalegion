/**
 * backend-type-badges.js — Backend Type Badges for Monitor
 * OllamaLegion WebUI
 *
 * Добавляет бейджи типа бэкенда (🦙 Ollama / 🦒 llama.cpp) в таблицы монитора
 * и на узлы топологии canvas.
 */
(function () {
    'use strict';

    const BADGE_STYLES = {
        ollama: {
            emoji: '🦙',
            label: 'Ollama',
            cssClass: 'badge-ollama',
            bgColor: '#1a73e820',
            borderColor: '#1a73e8',
            textColor: '#1a73e8'
        },
        llama_cpp: {
            emoji: '🦒',
            label: 'llama.cpp',
            cssClass: 'badge-llama-cpp',
            bgColor: '#ff6d0020',
            borderColor: '#ff6d00',
            textColor: '#ff6d00'
        },
        unknown: {
            emoji: '❓',
            label: 'Unknown',
            cssClass: 'badge-unknown',
            bgColor: '#9e9e9e20',
            borderColor: '#9e9e9e',
            textColor: '#9e9e9e'
        }
    };

    /**
     * Возвращает стиль бейджа для типа бэкенда
     */
    function getBadgeStyle(backendType) {
        return BADGE_STYLES[backendType] || BADGE_STYLES.unknown;
    }

    /**
     * Формирует HTML-бейдж для типа бэкенда.
     * @param {string} backendType — "ollama" | "llama_cpp"
     * @returns {string} HTML-строка
     */
    function renderBackendTypeBadge(backendType) {
        var style = getBadgeStyle(backendType);
        return '<span class="backend-type-badge ' + style.cssClass + '" ' +
            'style="display:inline-block;background:' + style.bgColor + ';' +
            'border:1px solid ' + style.borderColor + ';' +
            'color:' + style.textColor + ';' +
            'border-radius:4px;padding:1px 6px;font-size:11px;' +
            'font-weight:600;white-space:nowrap;margin-left:4px;"' +
            'title="' + style.label + ' backend">' +
            style.emoji + ' ' + style.label +
            '</span>';
    }

    /**
     * Возвращает тип бэкенда из данных ноды или глобального состояния.
     * Приоритет: node.backend_type > глобальный BackendTypeFilter > fallback 'ollama'.
     */
    function getEffectiveBackendType(node) {
        // 1. Из данных ноды (canvas-топология)
        if (node && node.backend_type) {
            return node.backend_type;
        }
        // 2. Из данных бэкенда (если node содержит поля как backend)
        if (node && (node.BackendType || node.type)) {
            var bt = node.BackendType || node.type;
            if (bt === 'llama_cpp') return 'llama_cpp';
            return 'ollama';
        }
        // 3. Глобальный тип из BackendTypeFilter
        if (window.BackendTypeFilter && window.BackendTypeFilter.getCurrentType) {
            return window.BackendTypeFilter.getCurrentType();
        }
        return 'ollama';
    }

    /**
     * Бейджи теперь встроены в renderBackends() (ui-renderer.js) per-backend.
     * Эта функция оставлена для обратной совместимости — больше не добавляет бейджи.
     */
    function enhanceBackendsTable() {
        // Per-backend type badges are now rendered inline in ui-renderer.js renderBackends()
    }

    /**
     * Бейджи теперь встроены per-backend; эта функция оставлена для обратной совместимости.
     */
    function enhanceModelsTable() {
        // Per-backend type badges are now handled in renderBackends()
    }

    /**
     * Интеграция с canvas-топологией: добавляет иконку типа на узел.
     * Вызывается из canvas-topology.js при отрисовке узла.
     * @param {CanvasRenderingContext2D} ctx
     * @param {object} node — данные узла
     * @param {number} x — позиция X
     * @param {number} y — позиция Y
     */
    function drawBackendTypeOnNode(ctx, node, x, y) {
        var backendType = getEffectiveBackendType(node);
        var style = getBadgeStyle(backendType);

        // Рисуем маленький кружок с эмодзи-символом
        ctx.save();
        ctx.font = '12px sans-serif';
        ctx.fillStyle = style.textColor;
        ctx.textAlign = 'center';
        ctx.textBaseline = 'top';
        // Эмодзи не рендерятся в canvas — используем символ
        var symbol = backendType === 'llama_cpp' ? '🦒' : '🦙';
        ctx.fillText(symbol, x, y + 14);
        ctx.restore();
    }

    // Экспорт
    window.BackendTypeBadges = {
        renderBadge: renderBackendTypeBadge,
        getBadgeStyle: getBadgeStyle,
        enhanceBackendsTable: enhanceBackendsTable,
        enhanceModelsTable: enhanceModelsTable,
        drawBackendTypeOnNode: drawBackendTypeOnNode,
        BADGE_STYLES: BADGE_STYLES
    };

})();
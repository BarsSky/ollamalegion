/**
 * gguf-renderer-helpers.js — Pure helper functions for gguf-renderer.
 *
 * R57.5a (2026-09-03): extracted from webui/js/modules/gguf-renderer.js.
 *
 * Содержит pure functions, не зависящие от state:
 *   - stripGGUF(name) — убирает суффикс ".gguf" (case-insensitive)
 *   - formatFileSize(bytes) — "1.5 GB" / "320 MB" / etc
 *   - showToast(message, type) — local toast или fallback в window.showToast
 *
 * Контракт:
 *   - window.GgufModule = { stripGGUF, formatFileSize, showToast }
 *   - Загружается ДО gguf-renderer.js (см. webui/index.html)
 */
(function() {
    'use strict';

    const M = (window.GgufModule = window.GgufModule || {});

    // stripGGUF — убирает суффикс ".gguf" (case-insensitive) из имени файла.
    // Используется в Bug #2 fix (Round 24) для нормализации сравнения
    // локального имени файла и имени загруженной модели.
    //   "Qwen3-Instruct-2507-q4km.gguf" → "Qwen3-Instruct-2507-q4km"
    //   "Qwen3-Instruct-2507-q4km.GGUF" → "Qwen3-Instruct-2507-q4km"
    //   "Qwen3-Instruct-2507-q4km"      → "Qwen3-Instruct-2507-q4km" (no change)
    M.stripGGUF = function(name) {
        if (!name) return '';
        if (name.length > 5 && name.slice(-5).toLowerCase() === '.gguf') {
            return name.slice(0, -5);
        }
        return name;
    };

    M.formatFileSize = function(bytes) {
        if (!bytes || bytes === 0) return '0 B';
        var units = ['B', 'KB', 'MB', 'GB', 'TB'];
        var i = Math.floor(Math.log(bytes) / Math.log(1024));
        return (bytes / Math.pow(1024, i)).toFixed(i > 0 ? 1 : 0) + ' ' + units[i];
    };

    // showToast — local fallback toast (если window.showToast не зарегистрирован).
    // В обычном режиме делегирует к window.showToast (из app.js → app-core.js).
    M.showToast = function(message, type) {
        if (window.showToast) {
            window.showToast(message, type);
            return;
        }
        // Fallback: гарантированно показать пользователю, даже если глобальный
        // window.showToast не зарегистрирован (например, страница открыта напрямую
        // или скрипт ещё не успел загрузиться). Используем простой DOM-fallback:
        // создаём или переиспользуем #ggufDebugToastHost и показываем плашку внизу.
        try {
            var hostId = 'ggufDebugToastHost';
            var host = document.getElementById(hostId);
            if (!host) {
                host = document.createElement('div');
                host.id = hostId;
                host.style.cssText = 'position:fixed;left:16px;bottom:16px;z-index:99999;display:flex;flex-direction:column;gap:6px;pointer-events:none;';
                document.body.appendChild(host);
            }
            var el = document.createElement('div');
            el.style.cssText = 'pointer-events:auto;padding:8px 12px;border-radius:6px;color:#fff;font-size:13px;box-shadow:0 2px 8px rgba(0,0,0,.2);max-width:480px;word-break:break-word;';
            var bg = type === 'error' ? '#d9534f' : type === 'success' ? '#5cb85c' : type === 'info' ? '#5bc0de' : '#777';
            el.style.background = bg;
            el.textContent = '[' + (type || 'log') + '] ' + message;
            host.appendChild(el);
            setTimeout(function () { if (el.parentNode) el.parentNode.removeChild(el); }, 6000);
        } catch (e) {
            // Если DOM недоступен — остаётся console.log
        }
        console.log('[' + (type || 'log') + '] ' + message);
    };
})();

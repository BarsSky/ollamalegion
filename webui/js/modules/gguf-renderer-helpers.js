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

    /**
     * formatVramMB — VRAM из метрик агента в человекочитаемый вид.
     *
     * ЗАЧЕМ ОТДЕЛЬНАЯ ФУНКЦИЯ. Агент отдаёт память GPU в МЕГАБАЙТАХ
     * (pkg/types/metrics.go: GPUMetrics.MemoryTotal/MemoryUsed/MemoryFree —
     * «Всего VRAM (MB)»), а formatFileSize ожидает БАЙТЫ. В панели «Инфо»
     * страницы GGUF величина передавалась напрямую, поэтому 8192 MB рисовались
     * как «8.0 KB», а свободные 7097 MB — как «6.9 KB» (замер на живом стенде).
     *
     * 0 и отсутствие значения — это «неизвестно», а не «0 B»: у бэкенда без
     * NVIDIA-метрик честнее показать прочерк.
     */
    M.MB = 1024 * 1024;
    M.formatVramMB = function(mb) {
        var n = Number(mb);
        if (!isFinite(n) || n <= 0) return '-';
        return M.formatFileSize(n * M.MB);
    };

    /**
     * hostSystemStats — сводка по ХОСТУ бэкенда из system-метрик агента (R91).
     *
     * ЗАЧЕМ. Панель «Инфо» страницы GGUF показывала только GPU и версию воркера:
     * память хоста (system.memory*) агент отдаёт давно, но на странице
     * llama.cpp-бэкенда её не было вообще — «полной сводки по состоянию
     * бэкенда» не получалось. При этом /api/v1/backends/{id} уже приходит целиком
     * и refreshDetail складывает его в state.backendRuntime, так что дополнительных
     * запросов не нужно.
     *
     * ЕДИНИЦЫ. types.SystemMetrics.MemoryTotal/Used/Free и DiskTotal/Used/Free —
     * МЕГАБАЙТЫ (pkg/types/metrics.go), поэтому рисуем их через formatVramMB, а не
     * через formatFileSize.
     *
     * Возвращаем ЧИСТЫЕ числа и null для «неизвестно»: форматирование и перевод
     * подписей делают вызывающие (renderHostSystemCard), а тест проверяет разбор
     * без DOM и без i18n.
     */
    M.hostSystemStats = function(system) {
        var sys = (system && typeof system === 'object') ? system : {};
        // Первое непустое значение из списка; null — «неизвестно».
        // strict=true: 0 считаем неизвестным (у агента это «датчика нет» либо
        // «сбор не удался») — иначе на карточке появлялись бы «0 B» и «0°C».
        function pick(strict, values) {
            for (var i = 0; i < values.length; i++) {
                var v = Number(values[i]);
                if (!isFinite(v)) continue;
                if (strict ? v > 0 : v >= 0) return v;
            }
            return null;
        }
        var cpu = (sys.cpu && typeof sys.cpu === 'object') ? sys.cpu : {};
        var ramTotal = pick(true, [sys.memoryTotal, sys.memory_total]);
        var ramUsed = pick(false, [sys.memoryUsed, sys.memory_used]);
        var ramFree = pick(false, [sys.memoryFree, sys.memory_free]);
        // Процент считаем только когда есть оба числа и total > 0: иначе деление
        // на ноль дало бы «NaN%» вместо честного прочерка.
        var ramPercent = (ramTotal !== null && ramUsed !== null)
            ? Math.round(ramUsed / ramTotal * 1000) / 10
            : null;
        return {
            known: ramTotal !== null || ramUsed !== null || ramFree !== null ||
                sys.cpuUsagePercent !== undefined || sys.diskTotal !== undefined,
            ramTotalMb: ramTotal,
            ramUsedMb: ramUsed,
            ramFreeMb: ramFree,
            ramPercent: ramPercent,
            cpuPercent: pick(false, [sys.cpuUsagePercent, sys.cpu_usage_percent]),
            cpuTempC: pick(true, [cpu.temperature, sys.cpuTemperature, sys.cpu_temperature]),
            diskTotalMb: pick(true, [sys.diskTotal, sys.disk_total]),
            diskFreeMb: pick(false, [sys.diskFree, sys.disk_free])
        };
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

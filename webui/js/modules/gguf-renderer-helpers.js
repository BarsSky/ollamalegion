/**
 * gguf-renderer-helpers.js — Pure helper functions for gguf-renderer.
 *
 * R57.5a (2026-09-03): extracted from webui/js/modules/gguf-renderer.js.
 *
 * Содержит pure functions, не зависящие от state:
 *   - stripGGUF(name) — убирает суффикс ".gguf" (case-insensitive)
 *   - window.GgufModelKey — ЕДИНСТВЕННОЕ правило «имя модели → ключ профиля»
 *     (R692): normalized key, варианты поиска и сам поиск в карте профилей
 *   - formatFileSize(bytes) — "1.5 GB" / "320 MB" / etc
 *   - showToast(message, type) — local toast или fallback в window.showToast
 *
 * Контракт:
 *   - window.GgufModule = { stripGGUF, GgufModelKey, formatFileSize, showToast }
 *   - window.GgufModelKey — та же карта, доступная без window.GgufModule
 *     (её читают gguf-detail-params.js / gguf-detail-models.js / cppworker-params.js)
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

    // =========================================================================
    // R692 (2026-11-23): ЕДИНОЕ правило «имя модели → ключ профиля».
    //
    // ЖИВОЙ ДЕФЕКТ (замер на стенде r691, образ webui:r261120-r691). Вкладка
    // GGUF → «Модели»: у ВСЕХ трёх моделей на диске колонка «В профиле» была
    // «— не задано —», поля пустые, и «Применить» сохраняло бы пустоту. При этом
    // профили существуют (`hasProfile=true`, 21 профиль в стенде):
    //
    //   файл на диске                                   ключ профиля
    //   Qwen3-4B-Instruct-2507-Q4_K_M.gguf          ↔   Qwen3-4B-Instruct-2507-Q4_K_M
    //   Qwen3.8-9B-Distill-uncensored-heretic…gguf  ↔   …heretic.Q4_K_M
    //   gemma-4-E4B-it-Q4_K_M.gguf                  ↔   gemma-4-E4B-it-Q4_K_M
    //
    // ПРИЧИНА. webui искал профиль ПОЛНЫМ именем файла (`…Q4_K_M.gguf`) прямо в
    // карте профилей, а балансер ключует их БЕЗ расширения:
    // `balancer.NormalizeModelProfileKey` (internal/balancer/num_ctx_resolver.go:148)
    // = trim → `\` в `/` → basename → снять `.gguf` (EqualFold) → trim.
    // Расширение — единственное, что балансер нормализует: регистр он СОХРАНЯЕТ,
    // суффикс квантования (`-Q4_K_M`) и `-it` НЕ снимает. Поэтому здесь нельзя
    // «додумывать» правило шире сервера: на живом стенде рядом лежат профили
    // `Qwen3-4B-Instruct-2507` И `Qwen3-4B-Instruct-2507-Q4_K_M` — обрезка
    // квантования или поиск по подстроке подставили бы ЧУЖОЙ профиль в карточку.
    //
    // ЧТО СОБИРАЕТ ЭТОТ ОБЪЕКТ:
    //   key(name)        — канонический ключ профиля (зеркало Go-функции);
    //   fold(name)       — ключ в нижнем регистре для последней проверки на
    //                      регистр (карта профилей хранит регистр, а имя файла
    //                      на диске может отличаться);
    //   candidates(name) — упорядоченные варианты, которые имеет смысл искать;
    //   find(map,name)   — поиск в карте профилей/effectiveContext с ЯВНОЙ
    //                      лестницей приоритетов (см. ниже);
    //   profile(map,name)/has(map,name) — значение/флаг по тому же правилу.
    //
    // ЛЕСТНИЦА find() (требование совместимости, R136/R212):
    //   1) ТОЧНОЕ совпадение — как имя пришло (карточка/путь) и его basename.
    //      Так находится профиль, который РЕАЛЬНО назван с `.gguf` (записи до
    //      R136 существуют и терять их нельзя).
    //   2) ТОЧНОЕ + `.gguf` — обратный случай: имя без расширения, а ключ с ним.
    //   3) НОРМАЛИЗОВАННЫЙ ключ (key) — обиходный случай, зеркало балансера.
    //   4) ТОЛЬКО РЕГИСТР (fold) — те же ключи, но в другом регистре; выбор
    //      детерминированный (сортировка ключей, как в GetModelProfile).
    // Подстрок НЕТ ни на одном шаге: «чужой профиль вместо пустого» хуже, чем
    // честное «не задано», и именно это проверяет тест.
    // =========================================================================
    var ModelKey = (M.GgufModelKey = window.GgufModelKey = {});

    /** key — канонический ключ профиля; зеркало balancer.NormalizeModelProfileKey. */
    ModelKey.key = function(name) {
        var s = (name === null || name === undefined) ? '' : String(name).trim();
        if (!s) return '';
        s = s.replace(/\\/g, '/');
        var cut = s.lastIndexOf('/');
        if (cut >= 0) s = s.slice(cut + 1);
        // То же условие длины, что у stripGGUF и у Go (len(name) > 5): имя `gguf`
        // — это имя модели, а не расширение.
        if (s.length > 5 && s.slice(-5).toLowerCase() === '.gguf') s = s.slice(0, -5);
        return s.trim();
    };

    /** fold — ключ в нижнем регистре: только для проверки «то же имя, другой регистр». */
    ModelKey.fold = function(name) {
        var k = ModelKey.key(name);
        return k ? k.toLowerCase() : '';
    };

    /**
     * candidates — упорядоченные варианты ключа для поиска (без повторов).
     *
     * Порядок = лестница приоритетов: имя как есть → basename пути → с `.gguf`
     * → канонический ключ балансера. Возвращается наружу, чтобы тест мог
     * проверить правило без карты профилей (и чтобы вызывающий мог показать, что
     * именно искали).
     */
    ModelKey.candidates = function(name) {
        var steps = candidateSteps(name);
        var out = [];
        for (var i = 0; i < steps.length; i++) out.push(steps[i].key);
        return out;
    };

    /**
     * candidateSteps — те же варианты, но с меткой ШАГА лестницы.
     *
     * Нужна, чтобы ответ find() называл, КАК совпало (`exact` / `exact-suffixed`
     * / `normalized`): по одной строке-кандидату это не восстановить — при имени
     * без расширения «как есть» и «канонический ключ» совпадают.
     */
    function candidateSteps(name) {
        var raw = (name === null || name === undefined) ? '' : String(name).trim();
        if (!raw) return [];
        var base = raw.replace(/\\/g, '/');
        var cut = base.lastIndexOf('/');
        base = cut >= 0 ? base.slice(cut + 1) : base;
        var out = [];
        var seen = {};
        function push(k, how) {
            if (!k || seen[k]) return;
            seen[k] = true;
            out.push({ key: k, how: how });
        }
        push(raw, 'exact');
        push(base, 'exact');
        // Профиль с расширением в ключе (записи до R136) — и для имени, и для
        // basename. Лишний `.gguf.gguf` дать невозможно: проверяем суффикс.
        if (!/\.gguf$/i.test(raw)) push(raw + '.gguf', 'exact-suffixed');
        if (!/\.gguf$/i.test(base)) push(base + '.gguf', 'exact-suffixed');
        push(ModelKey.key(raw), 'normalized');
        push(ModelKey.key(base), 'normalized');
        return out;
    }

    /**
     * find — найти запись в карте по имени модели.
     *
     * @param {Object} map карта профилей (ключ — имя модели балансера) либо
     *   effectiveContext из GET /api/v1/cppworker/model-profiles
     * @param {string} name имя файла, имя модели или путь
     * @returns {{key:string, value:*, matched:string}|null}
     *   matched: 'exact' | 'exact-suffixed' | 'normalized' | 'folded'
     */
    ModelKey.find = function(map, name) {
        if (!map || typeof map !== 'object') return null;
        var steps = candidateSteps(name);
        if (steps.length === 0) return null;
        for (var i = 0; i < steps.length; i++) {
            var k = steps[i].key;
            if (!Object.prototype.hasOwnProperty.call(map, k)) continue;
            return { key: k, value: map[k], matched: steps[i].how };
        }
        // Последний шаг — регистр. Детерминированно: сортировка ключей, как в
        // balancer.GetModelProfile (порядок map в JS/Go не гарантирован).
        var want = ModelKey.fold(name);
        if (!want) return null;
        var names = Object.keys(map).sort();
        for (var j = 0; j < names.length; j++) {
            if (typeof names[j] !== 'string') continue;
            if (ModelKey.fold(names[j]) === want) return { key: names[j], value: map[names[j]], matched: 'folded' };
        }
        return null;
    };

    /** profile — значение записи (профиль модели/effective) по правилу find. */
    ModelKey.profile = function(map, name) {
        var found = ModelKey.find(map, name);
        return found ? found.value : null;
    };

    /** has — есть ли запись для этой модели (индикаторы «свой профиль»/hasProfile). */
    ModelKey.has = function(map, name) {
        return !!ModelKey.find(map, name);
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
            el.style.cssText = 'pointer-events:auto;padding:8px 12px;border-radius:6px;color:#fff;font-size:var(--font-size-sm);box-shadow:0 2px 8px rgba(0,0,0,.2);max-width:480px;word-break:break-word;';
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

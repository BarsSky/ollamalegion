/**
 * Utility functions for WebUI
 */

const Utils = {
    /**
     * Format number with K/M suffixes
     */
    formatNumber(n) {
        if (n === undefined || n === null) return '-';
        if (n >= 1_000_000) return (n / 1_000_000).toFixed(1) + 'M';
        if (n >= 1_000) return (n / 1_000).toFixed(1) + 'K';
        return n.toString();
    },

    /**
     * Format megabytes to human readable string
     */
    formatMB(mb) {
        if (mb === undefined || mb === null) return '-';
        if (mb >= 1024) return (mb / 1024).toFixed(1) + ' GB';
        return Math.round(mb) + ' MB';
    },

    /**
     * Escape HTML to prevent XSS
     */
    escapeHtml(text) {
        const div = document.createElement('div');
        div.textContent = text;
        return div.innerHTML;
    },

    /**
     * Determine backend mode: cloud / cpu / gpu
     */
    getBackendMode(backend) {
        if ((backend.labels || []).includes('cloud')) return 'cloud';
        return backend.ollama?.backendCapacity?.mode || 'gpu';
    },

    /**
     * Check if backend is cloud
     */
    isCloudBackend(backend) {
        return (backend.labels || []).includes('cloud');
    },

    /**
     * Get HTML badge for backend mode
     */
    getBackendModeBadge(backend) {
        const mode = Utils.getBackendMode(backend);
        const badges = {
            gpu: '<span class="badge badge-success">GPU</span>',
            cpu: '<span class="badge badge-warning">CPU</span>',
            cloud: '<span class="badge badge-info"><svg class="badge-icon-svg" viewBox="0 0 24 24"><circle cx="12" cy="12" r="10" fill="currentColor"/></svg> Cloud</span>'
        };
        return badges[mode] || badges.gpu;
    },

    /**
     * Normalize backend type string to canonical form (ollama | llama_cpp | image_cpp).
     * Maps aliases like 'ollama_api' → 'ollama'.
     */
    normalizeBackendType(raw) {
        if (!raw) return '';
        const t = String(raw).toLowerCase();
        if (t === 'llama_cpp' || t === 'llamacpp' || t === 'llama.cpp') return 'llama_cpp';
        // R-Image Phase 5: image-бэкенд (stable-diffusion.cpp). Алиасы нужны потому,
        // что поле приходит из разных источников: backendType/type (config JSON),
        // BackendType (agent metrics, PascalCase значение), engine ('image_cpp').
        // Без явной ветки normalizeBackendType('image_cpp') возвращал 'image_cpp'
        // как есть, но getBackendType() ниже падал в дефолт 'ollama' - бейдж и
        // фильтры показывали image-бэкенд как Ollama.
        if (t === 'image_cpp' || t === 'imagecpp' || t === 'image.cpp' || t === 'sd_cpp' || t === 'sdcpp') return 'image_cpp';
        if (t === 'ollama' || t === 'ollama_api' || t === 'ollamaapi') return 'ollama';
        return t;
    },

    /**
     * Get backend type (ollama / llama_cpp) from backend data.
     * Priority: backendType > type > backend_type > BackendType (agent metrics) > default ollama
     */
    getBackendType(backend) {
        // Config-level type (camelCase from server JSON)
        if (backend.backendType) return Utils.normalizeBackendType(backend.backendType);
        if (backend.type) return Utils.normalizeBackendType(backend.type);
        // Legacy snake_case field (some code paths use this)
        if (backend.backend_type) return Utils.normalizeBackendType(backend.backend_type);
        // Agent metrics-level type (PascalCase)
        if (backend.BackendType) return Utils.normalizeBackendType(backend.BackendType);
        // Default fallback
        return 'ollama';
    },

    /**
     * Get HTML badge for backend type (🦙 Ollama / 🦒 llama.cpp / 🎨 image.cpp)
     */
    getBackendTypeBadge(backend) {
        const bt = Utils.getBackendType(backend);
        if (bt === 'llama_cpp') {
            return '<span class="badge backend-type-llama_cpp">🦒 llama.cpp</span>';
        }
        if (bt === 'image_cpp') {
            // R-Image Phase 5: label/emoji совпадают с GET /api/v1/backends/types
            // (BackendType.Emoji()/Label() на сервере) - единый вид в UI и API.
            return '<span class="badge backend-type-image_cpp">🎨 image.cpp</span>';
        }
        if (bt === 'ollama') {
            return '<span class="badge backend-type-ollama">🦙 Ollama</span>';
        }
        return '';
    },

    // ---- R-Image (Phase 5 follow-up, 2026-10-02): image.cpp backend helpers ----
    //
    // ЗАЧЕМ. У image-бэкенда (BackendType=image_cpp) своя топология: воркер
    // sd-server живёт на отдельном порту (imagePort), а метрики приходят не в
    // backend.ollama / backend.llamaCpp, а в backend.image (state, currentModel,
    // vramFreeMb/vramTotalMb, models[]). Из-за этого UI показывал для него
    // ollamaPort=11434 / cppWorkerPort=0 и 0 моделей - бэкенд выглядел пустым.
    // Держим разбор этих полей в одной точке, чтобы страницы «Бэкенды»,
    // «Модели» и Monitor не расходились в трактовке.

    /**
     * image-бэкенд (stable-diffusion.cpp) или нет.
     */
    isImageBackend(backend) {
        return Utils.getBackendType(backend) === 'image_cpp';
    },

    /**
     * Блок метрик image-воркера (backend.image) или null.
     */
    getBackendImage(backend) {
        if (!backend) return null;
        const img = backend.image || backend.Image;
        return (img && typeof img === 'object') ? img : null;
    },

    /**
     * Порт image-воркера (imagePort). 0 = не задан/неизвестен.
     * Для image-бэкенда ollamaPort/cppWorkerPort не значат ничего, поэтому
     * показывать их оператору нельзя - он видит 11434 и ищет воркер не там.
     */
    getBackendImagePort(backend) {
        if (!backend) return 0;
        const raw = (backend.imagePort !== undefined) ? backend.imagePort : backend.image_port;
        const p = parseInt(raw, 10);
        return (isFinite(p) && p > 0) ? p : 0;
    },

    /**
     * Модели image-воркера (backend.image.models).
     */
    getBackendImageModels(backend) {
        const img = Utils.getBackendImage(backend);
        return (img && Array.isArray(img.models)) ? img.models : [];
    },

    /**
     * Состояние image-воркера ('loaded' | 'not_loaded' | 'loading' | 'error').
     */
    getBackendImageState(backend) {
        const img = Utils.getBackendImage(backend);
        return (img && img.state) ? String(img.state).toLowerCase() : '';
    },

    /**
     * Ошибка последнего обращения к image-воркеру (пусто = ошибки нет).
     */
    getBackendImageLastError(backend) {
        const img = Utils.getBackendImage(backend);
        return (img && img.lastError) ? String(img.lastError) : '';
    },

    /**
     * i18n-ключ для состояния image-модели. Переиспользуем gguf.model_state_*
     * (они уже есть в обоих языках) - отдельный набор ключей не нужен.
     */
    imageModelStateKey(state) {
        const s = String(state || '').toLowerCase();
        if (s === 'loaded') return 'gguf.model_state_loaded';
        if (s === 'loading') return 'gguf.model_state_loading';
        if (s === 'error' || s === 'failed') return 'gguf.model_state_error';
        return 'gguf.model_state_unloaded';
    },

    /**
     * Локализованный текст состояния image-модели/воркера.
     */
    imageStateLabel(state) {
        const fallback = String(state || 'not_loaded');
        const key = Utils.imageModelStateKey(state);
        if (window.I18N && typeof window.I18N.t === 'function') {
            const v = window.I18N.t(key);
            if (v && v !== key) return v;
        }
        return fallback;
    },

    /**
     * HTML-бейдж состояния image-модели/воркера.
     */
    imageStateBadge(state) {
        const s = String(state || '').toLowerCase();
        const cls = (s === 'loaded') ? 'badge-success'
            : (s === 'loading') ? 'badge-warning'
            : (s === 'error' || s === 'failed') ? 'badge-danger'
            : 'badge-info';
        return '<span class="badge ' + cls + '">' + Utils.escapeHtml(Utils.imageStateLabel(s)) + '</span>';
    },

    /**
     * Размер модели image-воркера в байтах.
     *
     * Рабочий воркер отдаёт sizeBytes, но в части сборок для однофайлового
     * бандла туда попадали МЕГАБАЙТЫ (sd15-q4 -> 1526). SD-модель меньше 1 МБ
     * физически невозможна, поэтому такое значение трактуем как МБ - иначе в
     * карточке было бы "0.0 GB" вместо "1.5 GB". 0 = размер неизвестен.
     */
    imageSizeBytes(model) {
        if (!model) return 0;
        const raw = (model.sizeBytes !== undefined) ? model.sizeBytes
            : ((model.size_bytes !== undefined) ? model.size_bytes : 0);
        const v = Number(raw);
        if (!isFinite(v) || v <= 0) return 0;
        if (v < 1024 * 1024) return Math.round(v * 1024 * 1024);
        return Math.round(v);
    },

    /**
     * Оценка VRAM модели image-воркера в МБ (0 = неизвестно).
     */
    imageVramEstimateMb(model) {
        if (!model) return 0;
        const raw = (model.vramEstimateMb !== undefined) ? model.vramEstimateMb
            : ((model.vram_estimate_mb !== undefined) ? model.vram_estimate_mb : 0);
        const v = Number(raw);
        return (isFinite(v) && v > 0) ? Math.round(v) : 0;
    },

    /**
     * Строка "свободно / всего" для VRAM воркера. Любое отсутствующее значение
     * даёт '-' (не рисуем 0.0 GB - это выглядело бы как «VRAM кончилась»).
     */
    formatVramPair(freeMb, totalMb) {
        const f = (freeMb === undefined || freeMb === null || freeMb === '') ? null : Number(freeMb);
        const t = (totalMb === undefined || totalMb === null || totalMb === '') ? null : Number(totalMb);
        if ((f === null || !isFinite(f)) && (t === null || !isFinite(t))) return '-';
        const freeStr = (f === null || !isFinite(f)) ? '?' : Utils.formatMB(f);
        const totalStr = (t === null || !isFinite(t)) ? '?' : Utils.formatMB(t);
        return freeStr + ' / ' + totalStr;
    },

    /**
     * Get GPU status class based on metrics
     */
    getGPUStatus(gpuUsage, vramPercent, temp) {
        if (gpuUsage > 90 || vramPercent > 90 || temp > 85) return 'critical';
        if (gpuUsage > 70 || vramPercent > 70 || temp > 75) return 'warning';
        return 'healthy';
    },

    /**
     * Get progress bar CSS class based on value
     */
    getProgressClass(value) {
        if (value > 80) return 'high';
        if (value > 50) return 'medium';
        return 'low';
    },

    /**
     * Download file to user device
     */
    downloadFile(content, filename, type = 'text/plain') {
        const blob = new Blob([content], { type });
        const url = URL.createObjectURL(blob);
        const a = document.createElement('a');
        a.href = url;
        a.download = filename;
        document.body.appendChild(a);
        a.click();
        document.body.removeChild(a);
        URL.revokeObjectURL(url);
    },

    /**
     * Simple debounce helper
     */
    debounce(fn, ms = 300) {
        let timer;
        return (...args) => {
            clearTimeout(timer);
            timer = setTimeout(() => fn.apply(this, args), ms);
        };
    },

    /**
     * Calculate percentage safely
     */
    percent(value, total) {
        return total > 0 ? (value / total * 100) : 0;
    },

    /**
     * Get current i18n locale string for date formatting
     */
    _locale() {
        return (window.I18N && I18N.getLang()) || 'en';
    },

    /**
     * Format date to locale string
     *
     * R-Image follow-up (2026-10-02): НУЛЕВОЕ время Go доезжает до UI строкой
     * "0001-01-01T00:00:00Z", а такая строка истинна — проверки на falsy её не
     * ловили. Оператор видел это живьём: у image-бэкенда (у него нет агента и
     * heartbeat) в колонке «Последняя активность» появлялось «01.01.1, 02:30:17»
     * вместо прочерка. Год < 1970 для timestamp'ов проекта невозможен, поэтому
     * такие значения (как и невалидные) трактуем как «времени нет».
     */
    formatDate(date) {
        if (!date) return '-';
        try {
            const d = new Date(date);
            if (!isFinite(d.getTime()) || d.getFullYear() < 1970) return '-';
            return d.toLocaleString(Utils._locale());
        } catch {
            return '-';
        }
    },

    /**
     * Format time only (HH:MM:SS)
     *
     * Та же защита от нулевого времени, что и в formatDate: иначе в ячейке
     * оказывалось «02:30:17» из 0001-01-01 вместо прочерка.
     */
    formatTime(date) {
        if (!date) return '--:--:--';
        try {
            const d = new Date(date);
            if (!isFinite(d.getTime()) || d.getFullYear() < 1970) return '--:--:--';
            return d.toLocaleTimeString(Utils._locale());
        } catch {
            return '--:--:--';
        }
    },

    /**
     * Format the first VALID timestamp among candidates ('-' если валидных нет).
     *
     * Зачем отдельный хелпер: UI предпочитает lastHeartbeat, а он бывает
     * нулевым ("0001-01-01T00:00:00Z" — истинная строка). В этой ситуации важно
     * не просто «не показать 0001 год», а откатиться на следующий кандидат
     * (lastAgentContact), иначе оператор теряет единственную реальную дату.
     */
    formatDateFirst() {
        for (let i = 0; i < arguments.length; i++) {
            const s = Utils.formatDate(arguments[i]);
            if (s !== '-') return s;
        }
        return '-';
    },

    /**
     * Safe element text setter — skips if element not found
     */
    setText(id, text) {
        const el = document.getElementById(id);
        if (el) el.textContent = text;
    },

    /**
     * Safe element HTML setter
     */
    setHTML(id, html) {
        const el = document.getElementById(id);
        if (el) el.innerHTML = html;
    },

    /**
     * Safe element style setter
     */
    setStyle(id, prop, value) {
        const el = document.getElementById(id);
        if (el) el.style[prop] = value;
    },

    /**
     * gpu_layers live badge + quick-set helpers (Round 35c+, 2026-08-13).
     *
     * Backend magic values for numGpuLayers:
     *   -2 = AUTO (cppworker decides по VRAM/размеру, рекомендуется)
     *   -1 = all layers (max speed, нужно VRAM ≥ размера модели)
     *    0 = CPU only (медленно, через mmap в RAM)
     *    N = явное число слоёв на GPU (1..200)
     *
     * Использование в HTML:
     *   <label>GPU Layers <span id="badgeId" class="gpu-layers-badge is-auto">AUTO</span></label>
     *   <input type="number" id="inputId" value="-2" min="-2" max="200"
     *          oninput="Utils.updateGpuLayersBadge('inputId','badgeId')">
     *   <div class="gpu-layers-quick-row">
     *     <button type="button" class="btn btn-secondary" onclick="Utils.setGpuLayers(-2,'inputId','badgeId')">AUTO (-2)</button>
     *     ...
     *   </div>
     */
    updateGpuLayersBadge(inputId, badgeId) {
        const input = document.getElementById(inputId);
        const badge = document.getElementById(badgeId);
        if (!input || !badge) return;
        const val = parseInt(input.value, 10);
        let label, cls;
        if (Number.isNaN(val)) {
            label = '?';
            cls = 'is-invalid';
        } else if (val === -2) {
            label = (window.I18N && I18N.t)
                ? I18N.t('gguf.gpu_layers_badge_auto', 'AUTO')
                : 'AUTO';
            cls = 'is-auto';
        } else if (val === -1) {
            label = (window.I18N && I18N.t)
                ? I18N.t('gguf.gpu_layers_badge_all', 'All')
                : 'All';
            cls = 'is-all';
        } else if (val === 0) {
            label = (window.I18N && I18N.t)
                ? I18N.t('gguf.gpu_layers_badge_cpu', 'CPU')
                : 'CPU';
            cls = 'is-cpu';
        } else if (val < -2 || val > 200) {
            label = '?';
            cls = 'is-invalid';
        } else {
            // Custom N layers. Use plural-friendly format with simple int substitution.
            const tpl = (window.I18N && I18N.t)
                ? I18N.t('gguf.gpu_layers_badge_custom', '{n} layers')
                : '{n} layers';
            label = tpl.replace('{n}', String(val));
            cls = 'is-custom';
        }
        badge.textContent = label;
        badge.className = 'gpu-layers-badge ' + cls;

        // Update is-active state on quick-set buttons within the same group.
        const group = badge.closest('.form-group, .form-row, .wizard-field');
        if (group) {
            const btns = group.querySelectorAll('.gpu-layers-quick-row [data-gpu-value]');
            btns.forEach(function (b) {
                const v = parseInt(b.getAttribute('data-gpu-value'), 10);
                if (v === val) b.classList.add('is-active');
                else b.classList.remove('is-active');
            });
        }
    },
    setGpuLayers(value, inputId, badgeId) {
        const input = document.getElementById(inputId);
        if (!input) return;
        input.value = value;
        this.updateGpuLayersBadge(inputId, badgeId);
    }
};

// R59.7 (2026-09-03): expose to window so gguf-renderer-list.js and any
// other module that uses `window.Utils.escapeHtml(...)` (and similar)
// actually finds Utils. Same pattern as R59.3 (window.WebSocketManager),
// R59.5 (Api.getAuthHeaders), R59.6 (window.showToast), R59.7 fix
// (window.GgufApi). Without this, `gguf-renderer-list.js:99` calls
// `window.Utils.escapeHtml(b.id)` and gets "Cannot read properties of
// undefined (reading 'escapeHtml')" — silent failure that drops the
// whole backends list render.
if (typeof window !== 'undefined') {
    window.Utils = Utils;
}
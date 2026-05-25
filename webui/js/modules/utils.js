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
     * Normalize backend type string to canonical form (ollama | llama_cpp).
     * Maps aliases like 'ollama_api' → 'ollama'.
     */
    normalizeBackendType(raw) {
        if (!raw) return '';
        const t = String(raw).toLowerCase();
        if (t === 'llama_cpp' || t === 'llamacpp' || t === 'llama.cpp') return 'llama_cpp';
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
     * Get HTML badge for backend type (🦙 Ollama / 🦒 llama.cpp)
     */
    getBackendTypeBadge(backend) {
        const bt = Utils.getBackendType(backend);
        if (bt === 'llama_cpp') {
            return '<span class="badge backend-type-llama_cpp">🦒 llama.cpp</span>';
        }
        if (bt === 'ollama') {
            return '<span class="badge backend-type-ollama">🦙 Ollama</span>';
        }
        return '';
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
     */
    formatDate(date) {
        if (!date) return '-';
        try {
            return new Date(date).toLocaleString(Utils._locale());
        } catch {
            return '-';
        }
    },

    /**
     * Format time only (HH:MM:SS)
     */
    formatTime(date) {
        if (!date) return '--:--:--';
        try {
            return new Date(date).toLocaleTimeString(Utils._locale());
        } catch {
            return '--:--:--';
        }
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
    }
};
/**
 * GGUF API client — communicates with cppworker on port 18091
 * Provides methods for HuggingFace search, downloads, model management, GPU info
 */
const GgufApi = (function () {
    // Use nginx proxy URL when in Docker, fallback to localhost for dev
    let workerUrl = (window.WEBUI_CONFIG && window.WEBUI_CONFIG.CPPWORKER_URL) || 'http://localhost:18091';
    let connected = false;
    let connectionCallbacks = [];
    let hfToken = localStorage.getItem('ollamalegion_hf_token') || '';
    const REQUEST_TIMEOUT_MS = 10000; // 10-second timeout for all requests

    function buildUrl(path) {
        return workerUrl.replace(/\/+$/, '') + path;
    }

    /**
     * Generic request with AbortController timeout.
     * All requests to cppworker go through this — no more hanging fetch.
     */
    async function request(path, options = {}) {
        const controller = new AbortController();
        const timer = setTimeout(function () { controller.abort(); }, REQUEST_TIMEOUT_MS);
        try {
            const headers = { 'Content-Type': 'application/json', ...(options.headers || {}) };
            // Pass HF token to HF endpoints
            if (hfToken && (path === '/api/hf/search' || path.indexOf('/api/hf/files') === 0 || path === '/api/hf/download')) {
                headers['X-HF-Token'] = hfToken;
            }
            const response = await fetch(buildUrl(path), {
                headers: headers,
                signal: controller.signal,
                ...options
            });
            clearTimeout(timer);
            if (!response.ok) {
                const text = await response.text().catch(function () { return ''; });
                throw new Error('HTTP ' + response.status + ': ' + (text || response.statusText));
            }
            // Handle 204 No Content
            if (response.status === 204) return null;
            const ct = response.headers.get('content-type') || '';
            return ct.includes('application/json') ? response.json() : response.text();
        } catch (err) {
            clearTimeout(timer);
            if (err.name === 'AbortError') {
                throw new Error('Request timeout after ' + (REQUEST_TIMEOUT_MS / 1000) + 's — cppworker unreachable at ' + workerUrl);
            }
            if (!err.message.startsWith('HTTP')) {
                err.message = 'Network error: ' + err.message;
            }
            throw err;
        }
    }

    async function getJson(path) {
        return request(path, { method: 'GET' });
    }

    return {
        // ---- Connection Management ----

        /** Set the cppworker base URL */
        setUrl(url) {
            workerUrl = url || 'http://localhost:18091';
        },

        getUrl() {
            return workerUrl;
        },

        /** Set HuggingFace API token for gated model access */
        setHFToken(token) {
            hfToken = token || '';
            if (hfToken) {
                localStorage.setItem('ollamalegion_hf_token', hfToken);
            } else {
                localStorage.removeItem('ollamalegion_hf_token');
            }
        },

        /** Get current HuggingFace token */
        getHFToken() {
            return hfToken;
        },

        /** Test connection to cppworker */
        async testConnection() {
            try {
                const info = await getJson('/info');
                connected = true;
                connectionCallbacks.forEach(cb => cb(true));
                return { ok: true, info };
            } catch (err) {
                connected = false;
                connectionCallbacks.forEach(cb => cb(false));
                return { ok: false, error: err.message };
            }
        },

        isConnected() {
            return connected;
        },

        onConnectionChange(callback) {
            connectionCallbacks.push(callback);
        },

        // ---- Health & Info ----

        /** Get worker info (version, etc.) */
        async getInfo() {
            return getJson('/info');
        },

        /** Get GPU info */
        async getGpuInfo() {
            try {
                return await getJson('/api/gpu');
            } catch (e) {
                // Fallback to /info if /api/gpu not available
                const info = await getJson('/info');
                return info && info.gpu ? info.gpu : null;
            }
        },

        // ---- HuggingFace Search ----

        /** Search models on HuggingFace Hub */
        async searchModels(query, limit = 10) {
            return getJson('/api/hf/search?query=' + encodeURIComponent(query) + '&limit=' + limit);
        },

        /** List files for a HuggingFace model */
        async listModelFiles(modelId, revision = 'main') {
            return getJson('/api/hf/files?modelId=' + encodeURIComponent(modelId) + '&revision=' + encodeURIComponent(revision));
        },

        // ---- Downloads ----

        /** Start downloading a model file from HuggingFace */
        async startDownload(modelId, filename, revision = 'main') {
            return request('/api/hf/download', {
                method: 'POST',
                body: JSON.stringify({ modelId, filename, revision })
            });
        },

        /** Get download progress */
        async getDownloadProgress(modelId, filename) {
            return getJson('/api/hf/progress?modelId=' + encodeURIComponent(modelId) + '&filename=' + encodeURIComponent(filename));
        },

        /** List all active downloads */
        async listActiveDownloads() {
            return getJson('/api/hf/downloads');
        },

        /** Cancel a download */
        async cancelDownload(modelId, filename) {
            return request('/api/hf/cancel', {
                method: 'POST',
                body: JSON.stringify({ modelId, filename })
            });
        },

        // ---- Model Management via Balancer ----

        /**
         * Manage model on a registered backend via the balancer API.
         * @param {string} backendId — backend ID (e.g., 'llama_gpu')
         * @param {string} operation — 'load' or 'unload'
         * @param {string} modelName — model name/path
         * @param {object} [options] — load options (gpuLayers, ctxSize, etc.)
         */
        async manageModel(backendId, operation, modelName, options = {}) {
            var balancerUrl = (window.WEBUI_CONFIG && window.WEBUI_CONFIG.API_BASE_URL) ||
                (typeof window !== 'undefined' && window.location && window.location.origin) ||
                '/';
            const url = balancerUrl.replace(/\/+$/, '') + '/api/v1/backends/' + encodeURIComponent(backendId) + '/models';
            const body = {
                operation: operation,
                modelName: modelName,
                ...options
            };
            const controller = new AbortController();
            const timer = setTimeout(function () { controller.abort(); }, 30000);
            try {
                const response = await fetch(url, {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify(body),
                    signal: controller.signal
                });
                clearTimeout(timer);
                const data = await response.json();
                if (!response.ok) {
                    return { success: false, error: data.error || 'HTTP ' + response.status };
                }
                return data;
            } catch (err) {
                clearTimeout(timer);
                return { success: false, error: err.message };
            }
        },

        /**
         * Get models list for a specific backend via balancer API.
         * GET /api/v1/backends/{backendId}/models
         */
        async getBackendModels(backendId) {
            var balancerUrl = (window.WEBUI_CONFIG && window.WEBUI_CONFIG.API_BASE_URL) ||
                (typeof window !== 'undefined' && window.location && window.location.origin) ||
                '/';
            const url = balancerUrl.replace(/\/+$/, '') + '/api/v1/backends/' + encodeURIComponent(backendId) + '/models';
            const controller = new AbortController();
            const timer = setTimeout(function () { controller.abort(); }, REQUEST_TIMEOUT_MS);
            try {
                const response = await fetch(url, {
                    headers: { 'Content-Type': 'application/json' },
                    signal: controller.signal
                });
                clearTimeout(timer);
                if (!response.ok) {
                    const text = await response.text().catch(function () { return ''; });
                    throw new Error('HTTP ' + response.status + ': ' + (text || response.statusText));
                }
                return response.json();
            } catch (err) {
                clearTimeout(timer);
                throw err;
            }
        },

        // ---- Balancer Backends Info ----

        /**
         * Get registered llama.cpp backends info from balancer API.
         * This replaces the need to connect directly to individual cppworkers.
         * Returns: { backends: [...], total: N, backendType: "llama_cpp", operatingMode: "..." }
         */
        async getLlamaCppBackends() {
            // Use the balancer's API server URL instead of cppworker.
            // Priority: 1) WEBUI_CONFIG.API_BASE_URL, 2) current origin (Docker/nginx), 3) relative path
            var balancerUrl = (window.WEBUI_CONFIG && window.WEBUI_CONFIG.API_BASE_URL) ||
                (typeof window !== 'undefined' && window.location && window.location.origin) ||
                '/';
            const url = balancerUrl.replace(/\/+$/, '') + '/api/v1/gguf/backends';
            console.log('[GGUF] Fetching backends from:', url);
            const controller = new AbortController();
            const timer = setTimeout(function () { controller.abort(); }, REQUEST_TIMEOUT_MS);
            try {
                const response = await fetch(url, {
                    headers: { 'Content-Type': 'application/json' },
                    signal: controller.signal
                });
                clearTimeout(timer);
                if (!response.ok) {
                    const text = await response.text().catch(function () { return ''; });
                    throw new Error('HTTP ' + response.status + ': ' + (text || response.statusText));
                }
                return response.json();
            } catch (err) {
                clearTimeout(timer);
                if (err.name === 'AbortError') {
                    throw new Error('Request timeout — balancer API unreachable');
                }
                throw err;
            }
        },

        // ---- Local Model Management ----

        /** List local GGUF files */
        async listLocalModels() {
            return getJson('/api/models/files');
        },

        /** Load a model */
        async loadModel(modelName, options = {}) {
            return request('/api/models/load', {
                method: 'POST',
                body: JSON.stringify({
                    name: modelName,
                    path: options.path || '',
                    ctxSize: options.ctxSize || 2048,
                    batchSize: options.batchSize || 512,
                    gpuLayers: options.gpuLayers !== undefined ? options.gpuLayers : -1,
                    flashAttn: options.flashAttn || false,
                    numa: options.numa || false,
                    useMmap: options.useMmap !== undefined ? options.useMmap : true,
                    tensorSplit: options.tensorSplit || null
                })
            });
        },

        /** Unload a model */
        async unloadModel(modelHandle) {
            return request('/api/models/unload', {
                method: 'POST',
                body: JSON.stringify({ modelHandle: modelHandle })
            });
        },

        /** List loaded models */
        async listLoadedModels() {
            return getJson('/api/models');
        }
    };
})();

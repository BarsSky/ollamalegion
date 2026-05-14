/**
 * API layer with centralized error handling and token auth
 */
const Api = (function () {
    const CFG = window.WEBUI_CONFIG || {};
    const API_BASE = CFG.API_BASE || '';

    // Internal: build fetch options with auth headers
    function getOptions(options = {}) {
        const headers = {
            'Content-Type': 'application/json',
            ...(options.headers || {})
        };
        if (CFG.API_TOKEN) {
            headers['X-API-Token'] = CFG.API_TOKEN;
        }
        return { ...options, headers };
    }

    // Internal: perform fetch with error handling
    async function request(url, options = {}) {
        try {
            const response = await fetch(url, getOptions(options));
            if (!response.ok) {
                const text = await response.text().catch(() => '');
                let body = text;
                try { body = JSON.parse(text); } catch {}
                const err = new Error(`HTTP ${response.status}: ${response.statusText}`);
                err.status = response.status;
                err.body = body;
                throw err;
            }
            return response;
        } catch (err) {
            // Network errors bubble up with message
            if (!err.status) {
                err.message = `Network error: ${err.message}`;
            }
            throw err;
        }
    }

    // Internal: generic GET returning JSON
    async function getJson(endpoint) {
        const response = await request(`${API_BASE}${endpoint}`);
        return response.json();
    }

    return {
        // Health check
        async health() {
            return getJson('/api/v1/health');
        },

        // Cluster state
        async cluster() {
            return getJson('/api/v1/cluster');
        },

        // Queue stats
        async queueStats() {
            return getJson('/api/v1/queue/stats');
        },

        // Queue details (pending tasks)
        async queueDetails() {
            return getJson('/api/v1/queue/details');
        },

        // Queue history (completed tasks)
        async queueHistory() {
            return getJson('/api/v1/queue/history');
        },

        // Sessions
        async sessions() {
            return getJson('/api/v1/sessions');
        },

        // Cluster config (settings)
        async config() {
            return getJson('/api/v1/cluster/config');
        },

        // Backend CRUD
        async createBackend(data) {
            const response = await request(`${API_BASE}/api/v1/backends`, {
                method: 'POST',
                body: JSON.stringify(data)
            });
            return response.ok;
        },

        async updateBackend(id, data) {
            const response = await request(`${API_BASE}/api/v1/backends/${id}`, {
                method: 'PUT',
                body: JSON.stringify(data)
            });
            return response.ok;
        },

        async deleteBackend(id) {
            const response = await request(`${API_BASE}/api/v1/backends/${id}`, {
                method: 'DELETE'
            });
            return response.ok;
        },

        async updateConfig(data) {
            const response = await request(`${API_BASE}/api/v1/cluster/config`, {
                method: 'PUT',
                body: JSON.stringify(data)
            });
            return response.ok;
        },

        async updateBackendLimits(id, maxConcurrentRequests) {
            const response = await request(`${API_BASE}/api/v1/backends/${id}/limits`, {
                method: 'PUT',
                body: JSON.stringify({ maxConcurrentRequests })
            });
            return response.ok;
        },

        // Get single backend config (with weight, maxConcurrentReqs etc.)
        async getBackend(id) {
            return getJson(`/api/v1/backends/${id}`);
        },

        async updateBackendLimitsFull(id, maxConcurrentRequests, maxModels) {
            const response = await request(`${API_BASE}/api/v1/backends/${id}/limits`, {
                method: 'PUT',
                body: JSON.stringify({ maxConcurrentRequests, maxModels })
            });
            return response.ok;
        },

        // Generic POST helper returning JSON
        async post(endpoint, data) {
            const response = await request(`${API_BASE}${endpoint}`, {
                method: 'POST',
                ...(data ? { body: JSON.stringify(data) } : {})
            });
            if (response.status === 204 || response.headers.get('content-length') === '0') return null;
            const ct = response.headers.get('content-type') || '';
            return ct.includes('application/json') ? response.json() : response.text();
        },

        // Proxy logs
        async proxyLogs(limit = 50) {
            return getJson(`/api/v1/proxy/logs?limit=${limit}`);
        },

        // Model Management API
        async backendModels(id) {
            return getJson(`/api/v1/backends/${id}/models`);
        },

        async backendModelOperation(id, operation, modelName, options = {}) {
            const payload = { operation, modelName, ...options };
            const response = await request(`${API_BASE}/api/v1/backends/${id}/models`, {
                method: 'POST',
                body: JSON.stringify(payload)
            });
            return response.json();
        },

        async modelOperationsStatus() {
            return getJson('/api/v1/models/operations');
        },

        // Agents API
        async agentsStats() {
            return getJson('/api/v1/agents/stats');
        },

        async agentInfo(id) {
            return getJson('/api/v1/agents/' + encodeURIComponent(id));
        },

        async restartAgent(backendId) {
            const response = await request(`${API_BASE}/api/v1/backends/${encodeURIComponent(backendId)}/restart`, {
                method: 'POST'
            });
            return response.json();
        },

        async agentLogs(backendId, limit = 100) {
            return getJson(`/api/v1/backends/${encodeURIComponent(backendId)}/logs?limit=${limit}`);
        },

        // Generic error handler for UI

        handleError(err, fallbackMessage = 'Ошибка API') {
            console.error(err);
            const msg = err?.message || fallbackMessage;
            window.dispatchEvent(new CustomEvent('api-error', { detail: { message: msg, error: err } }));
            return msg;
        }
    };
})();

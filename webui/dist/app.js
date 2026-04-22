/**
 * Ollama Legion - Management Platform
 * Full-featured dashboard with WebSocket metrics, charts, and backend management
 */

class Dashboard {
    constructor() {
        this.ws = null;
        this.wsUrl = 'ws://localhost:18081/ws/metrics';
        this.apiUrl = 'http://localhost:18081/api/v1';
        this.reconnectAttempts = 0;
        this.maxReconnectAttempts = 10;
        this.reconnectDelay = 3000;
        this.data = {
            backends: [],
            sessions: [],
            cluster: {}
        };
        this.chartData = {
            gpu: [],
            cpu: [],
            ram: [],
            labels: []
        };
        this.maxDataPoints = 30;
        this.lastUpdate = null;
        this.authToken = localStorage.getItem('ollama_lb_token');
        
        this.init();
    }

    init() {
        this.setupWebSocket();
        this.setupCharts();
        this.startAutoUpdate();
        this.fetchApiVersion();
        this.setupEventListeners();
    }

    // ============================================
    // Event Listeners
    // ============================================
    setupEventListeners() {
        // Add backend form
        const addBackendForm = document.getElementById('addBackendForm');
        if (addBackendForm) {
            addBackendForm.addEventListener('submit', (e) => this.handleAddBackend(e));
        }

        // Config upload
        const configUpload = document.getElementById('configUpload');
        if (configUpload) {
            configUpload.addEventListener('change', (e) => this.handleConfigUpload(e));
        }
    }

    // ============================================
    // WebSocket Connection
    // ============================================
    setupWebSocket() {
        this.connectWebSocket();
    }

    connectWebSocket() {
        this.updateWebSocketIndicator('connecting');
        
        try {
            this.ws = new WebSocket(this.wsUrl);
            
            this.ws.onopen = () => {
                console.log('WebSocket connected');
                this.reconnectAttempts = 0;
                this.updateWebSocketIndicator('connected');
            };

            this.ws.onmessage = (event) => {
                try {
                    const data = JSON.parse(event.data);
                    this.handleMetrics(data);
                } catch (e) {
                    console.error('Failed to parse WebSocket message:', e);
                }
            };

            this.ws.onclose = (event) => {
                console.log(`WebSocket closed (code: ${event.code}, reason: ${event.reason || 'no reason'})`);
                this.updateWebSocketIndicator('disconnected');
                this.attemptReconnect();
            };

            this.ws.onerror = (error) => {
                console.error('WebSocket error:', error);
                this.updateWebSocketIndicator('disconnected');
            };
        } catch (e) {
            console.error('Failed to create WebSocket:', e);
            this.updateWebSocketIndicator('disconnected');
            this.attemptReconnect();
        }
    }

    attemptReconnect() {
        if (this.reconnectAttempts < this.maxReconnectAttempts) {
            this.reconnectAttempts++;
            console.log(`Reconnecting... Attempt ${this.reconnectAttempts}/${this.maxReconnectAttempts}`);
            setTimeout(() => this.connectWebSocket(), this.reconnectDelay);
        } else {
            console.error('Max reconnection attempts reached');
        }
    }

    updateWebSocketIndicator(state) {
        const indicator = document.getElementById('websocketIndicator');
        const text = indicator.querySelector('.indicator-text');
        
        indicator.classList.remove('connected', 'disconnected', 'connecting');
        
        switch (state) {
            case 'connected':
                indicator.classList.add('connected');
                text.textContent = 'Connected';
                break;
            case 'disconnected':
                indicator.classList.add('disconnected');
                text.textContent = 'Disconnected';
                break;
            case 'connecting':
                indicator.classList.add('connecting');
                text.textContent = 'Connecting...';
                break;
        }
    }

    // ============================================
    // Metrics Handling
    // ============================================
    handleMetrics(data) {
        this.data = { ...this.data, ...data };
        this.lastUpdate = new Date();
        
        this.updateClusterOverview();
        this.updateBackendsGrid();
        this.updateSessionsTable();
        this.updateCharts();
        this.updateFooter();
    }

    updateClusterOverview() {
        const backends = this.data.backends || [];
        const sessions = this.data.sessions || [];
        const cluster = this.data.cluster || {};
        
        // Active backends
        const activeBackends = backends.filter(b => b.status === 'online').length;
        document.getElementById('activeBackends').textContent = activeBackends;
        
        // Total GPU usage (average)
        let totalGpuUsage = 0;
        let gpuCount = 0;
        backends.forEach(backend => {
            if (backend.metrics?.gpu?.usage !== undefined) {
                totalGpuUsage += backend.metrics.gpu.usage;
                gpuCount++;
            }
        });
        const avgGpuUsage = gpuCount > 0 ? (totalGpuUsage / gpuCount).toFixed(1) : 0;
        document.getElementById('totalGpuUsage').textContent = avgGpuUsage;
        
        // Active sessions
        document.getElementById('activeSessions').textContent = sessions.length;
        
        // Cluster RPS
        let clusterRps = 0;
        backends.forEach(backend => {
            if (backend.metrics?.ollama?.rps !== undefined) {
                clusterRps += backend.metrics.ollama.rps;
            }
        });
        document.getElementById('clusterRps').textContent = clusterRps.toFixed(1);
        
        // Cluster status
        const clusterStatusEl = document.getElementById('clusterStatus');
        if (activeBackends === 0) {
            clusterStatusEl.textContent = 'Cluster: Offline';
            clusterStatusEl.style.color = 'var(--accent-red)';
        } else if (activeBackends < backends.length) {
            clusterStatusEl.textContent = 'Cluster: Degraded';
            clusterStatusEl.style.color = 'var(--accent-yellow)';
        } else {
            clusterStatusEl.textContent = 'Cluster: Healthy';
            clusterStatusEl.style.color = 'var(--accent-green)';
        }
    }

    updateBackendsGrid() {
        const grid = document.getElementById('backendsGrid');
        const backends = this.data.backends || [];
        
        if (backends.length === 0) {
            grid.innerHTML = '<div class="empty-state">No backends available</div>';
            return;
        }
        
        grid.innerHTML = backends.map(backend => this.renderBackendCard(backend)).join('');
    }

    renderBackendCard(backend) {
        const { id, status, metrics, ip, host } = backend;
        const gpu = metrics?.gpu || { usage: 0, memory_total: 0, memory_used: 0 };
        const cpu = metrics?.cpu || { usage: 0 };
        const ram = metrics?.ram || { total: 0, used: 0 };
        const ollama = metrics?.ollama || { active_requests: 0, rps: 0 };
        
        const gpuPercent = gpu.memory_total > 0 ? ((gpu.memory_used / gpu.memory_total) * 100).toFixed(1) : 0;
        const ramPercent = ram.total > 0 ? ((ram.used / ram.total) * 100).toFixed(1) : 0;
        
        const statusClass = status === 'online' ? 'online' : status === 'offline' ? 'offline' : 'warning';
        
        return `
            <div class="backend-card ${statusClass}">
                <div class="backend-header">
                    <div class="backend-header-left">
                        <span class="backend-id">${this.escapeHtml(id)}</span>
                        <span class="backend-status ${statusClass}">${status}</span>
                    </div>
                    <div class="backend-actions">
                        <button class="action-btn" onclick="window.dashboard.downloadEnvConfig('${this.escapeHtml(id)}', '${this.escapeHtml(host || '')}')" title="Download .env config">📥</button>
                        <button class="delete-backend-btn" onclick="window.dashboard.deleteBackend('${this.escapeHtml(id)}')" title="Remove backend">🗑️</button>
                    </div>
                </div>
                ${ip ? `<div class="backend-ip">🌐 ${this.escapeHtml(ip)}</div>` : ''}
                
                <div class="metric-row">
                    <div class="metric-row-header">
                        <span class="metric-row-label">
                            <span class="metric-row-icon">🎮</span> GPU Load
                        </span>
                        <span class="metric-row-value">${gpu.usage.toFixed(1)}%</span>
                    </div>
                    <div class="progress-bar">
                        <div class="progress-fill ${this.getProgressClass(gpu.usage)}" style="width: ${gpu.usage}%"></div>
                    </div>
                </div>
                
                <div class="metric-row">
                    <div class="metric-row-header">
                        <span class="metric-row-label">
                            <span class="metric-row-icon">💾</span> GPU Memory
                        </span>
                        <span class="metric-row-value">${this.formatBytes(gpu.memory_used)} / ${this.formatBytes(gpu.memory_total)} (${gpuPercent}%)</span>
                    </div>
                    <div class="progress-bar">
                        <div class="progress-fill ${this.getProgressClass(gpuPercent)}" style="width: ${gpuPercent}%"></div>
                    </div>
                </div>
                
                <div class="metric-row">
                    <div class="metric-row-header">
                        <span class="metric-row-label">
                            <span class="metric-row-icon">⚙️</span> CPU Usage
                        </span>
                        <span class="metric-row-value">${cpu.usage.toFixed(1)}%</span>
                    </div>
                    <div class="progress-bar">
                        <div class="progress-fill ${this.getProgressClass(cpu.usage)}" style="width: ${cpu.usage}%"></div>
                    </div>
                </div>
                
                <div class="metric-row">
                    <div class="metric-row-header">
                        <span class="metric-row-label">
                            <span class="metric-row-icon">🧠</span> RAM Usage
                        </span>
                        <span class="metric-row-value">${this.formatBytes(ram.used)} / ${this.formatBytes(ram.total)} (${ramPercent}%)</span>
                    </div>
                    <div class="progress-bar">
                        <div class="progress-fill ${this.getProgressClass(ramPercent)}" style="width: ${ramPercent}%"></div>
                    </div>
                </div>
                
                <div class="backend-stats">
                    <div class="stat-item">
                        <div class="stat-label">Active Requests</div>
                        <div class="stat-value">${ollama.active_requests}</div>
                    </div>
                    <div class="stat-item">
                        <div class="stat-label">RPS</div>
                        <div class="stat-value">${ollama.rps.toFixed(1)}</div>
                    </div>
                </div>
            </div>
        `;
    }

    updateSessionsTable() {
        const tbody = document.getElementById('sessionsTableBody');
        const sessions = this.data.sessions || [];
        
        if (sessions.length === 0) {
            tbody.innerHTML = '<tr class="empty-row"><td colspan="6">No active sessions</td></tr>';
            return;
        }
        
        tbody.innerHTML = sessions.map(session => `
            <tr>
                <td>${this.escapeHtml(session.id || 'N/A')}</td>
                <td>${this.escapeHtml(session.backend || 'N/A')}</td>
                <td>${this.escapeHtml(session.model || 'N/A')}</td>
                <td>${this.formatDuration(session.duration || 0)}</td>
                <td>${session.tokens || 0}</td>
                <td><span class="session-status ${session.status || 'active'}">${session.status || 'active'}</span></td>
            </tr>
        `).join('');
    }

    // ============================================
    // Charts
    // ============================================
    setupCharts() {
        this.gpuChartCtx = document.getElementById('gpuChart').getContext('2d');
        this.cpuChartCtx = document.getElementById('cpuChart').getContext('2d');
        this.ramChartCtx = document.getElementById('ramChart').getContext('2d');
        
        this.gpuChart = this.createChart(this.gpuChartCtx, 'GPU Usage', '#f85149');
        this.cpuChart = this.createChart(this.cpuChartCtx, 'CPU Usage', '#58a6ff');
        this.ramChart = this.createChart(this.ramChartCtx, 'RAM Usage', '#3fb950');
    }

    createChart(ctx, label, color) {
        return new Chart(ctx, {
            type: 'line',
            data: {
                labels: [],
                datasets: [{
                    label: label,
                    data: [],
                    borderColor: color,
                    backgroundColor: color.replace(')', ', 0.2)').replace('rgb', 'rgba'),
                    borderWidth: 2,
                    fill: true,
                    tension: 0.4,
                    pointRadius: 0,
                    pointHitRadius: 10
                }]
            },
            options: {
                responsive: true,
                maintainAspectRatio: false,
                animation: {
                    duration: 300,
                    easing: 'easeInOutQuart'
                },
                scales: {
                    x: {
                        display: false,
                        grid: { display: false }
                    },
                    y: {
                        beginAtZero: true,
                        max: 100,
                        grid: {
                            color: 'rgba(48, 54, 61, 0.5)'
                        },
                        ticks: {
                            color: '#8b949e',
                            callback: (value) => value + '%'
                        }
                    }
                },
                plugins: {
                    legend: {
                        display: true,
                        position: 'top',
                        labels: {
                            color: '#c9d1d9',
                            font: { size: 11 }
                        }
                    }
                }
            }
        });
    }

    updateCharts() {
        const backends = this.data.backends || [];
        
        // Calculate averages
        let gpuAvg = 0, cpuAvg = 0, ramAvg = 0;
        let count = 0;
        
        backends.forEach(backend => {
            const m = backend.metrics || {};
            if (m.gpu?.usage !== undefined) {
                gpuAvg += m.gpu.usage;
                count++;
            }
            if (m.cpu?.usage !== undefined) {
                cpuAvg += m.cpu.usage;
            }
            if (m.ram?.total > 0 && m.ram?.used !== undefined) {
                ramAvg += (m.ram.used / m.ram.total) * 100;
            }
        });
        
        if (count > 0) {
            gpuAvg /= count;
            cpuAvg /= count;
            ramAvg /= count;
        }
        
        const now = new Date().toLocaleTimeString();
        
        // Add data points
        this.chartData.labels.push(now);
        this.chartData.gpu.push(gpuAvg);
        this.chartData.cpu.push(cpuAvg);
        this.chartData.ram.push(ramAvg);
        
        // Limit data points
        if (this.chartData.labels.length > this.maxDataPoints) {
            this.chartData.labels.shift();
            this.chartData.gpu.shift();
            this.chartData.cpu.shift();
            this.chartData.ram.shift();
        }
        
        // Update charts
        this.gpuChart.data.labels = this.chartData.labels;
        this.gpuChart.data.datasets[0].data = this.chartData.gpu;
        this.gpuChart.update('none');
        
        this.cpuChart.data.labels = this.chartData.labels;
        this.cpuChart.data.datasets[0].data = this.chartData.cpu;
        this.cpuChart.update('none');
        
        this.ramChart.data.labels = this.chartData.labels;
        this.ramChart.data.datasets[0].data = this.chartData.ram;
        this.ramChart.update('none');
    }

    // ============================================
    // Footer
    // ============================================
    updateFooter() {
        if (this.lastUpdate) {
            document.getElementById('lastUpdate').textContent = this.lastUpdate.toLocaleTimeString();
        }
    }

    async fetchApiVersion() {
        try {
            const response = await fetch(`${this.apiUrl}/health`, {
                method: 'GET',
                headers: { 'Accept': 'application/json' },
                signal: AbortSignal.timeout(5000)
            });
            
            if (!response.ok) {
                throw new Error(`HTTP ${response.status}: ${response.statusText}`);
            }
            
            const data = await response.json();
            document.getElementById('apiVersion').textContent = data.version || '1.0.0';
            
            console.log('API connected:', data);
        } catch (e) {
            console.warn('Could not fetch API version:', e.message);
            document.getElementById('apiVersion').textContent = '1.0.0 (offline)';
            
            const apiStatusEl = document.getElementById('apiStatus');
            if (apiStatusEl) {
                apiStatusEl.textContent = 'API Offline';
                apiStatusEl.style.color = 'var(--accent-red)';
            }
        }
    }

    // ============================================
    // Backend Management
    // ============================================
    getAuthHeaders() {
        const headers = { 'Content-Type': 'application/json' };
        if (this.authToken) {
            headers['Authorization'] = `Bearer ${this.authToken}`;
        }
        return headers;
    }

    async handleAddBackend(e) {
        e.preventDefault();
        
        const formData = new FormData(e.target);
        const backend = {
            id: formData.get('id') || `backend-${Date.now()}`,
            name: formData.get('name') || '',
            host: formData.get('host'),
            ip: formData.get('ip') || '',
            ollamaPort: parseInt(formData.get('ollamaPort')) || 11434,
            agentPort: parseInt(formData.get('agentPort')) || 9090,
            weight: parseInt(formData.get('weight')) || 1,
            maxConcurrentRequests: parseInt(formData.get('maxConcurrentRequests')) || 10,
            labels: formData.get('labels') ? formData.get('labels').split(',').map(s => s.trim()) : []
        };

        try {
            const response = await fetch(`${this.apiUrl}/backends`, {
                method: 'POST',
                headers: this.getAuthHeaders(),
                body: JSON.stringify(backend)
            });

            const result = await response.json();
            
            if (result.success) {
                alert('Backend added successfully!');
                e.target.reset();
            } else {
                alert('Error: ' + result.error);
            }
        } catch (error) {
            console.error('Failed to add backend:', error);
            alert('Failed to add backend: ' + error.message);
        }
    }

    async handleConfigUpload(e) {
        const file = e.target.files[0];
        if (!file) return;

        try {
            const configText = await file.text();
            const config = JSON.parse(configText);
            
            // Если конфиг содержит массив бэкендов
            if (Array.isArray(config.backends) || Array.isArray(config)) {
                const backends = config.backends || config;
                let successCount = 0;
                
                for (const backend of backends) {
                    try {
                        const response = await fetch(`${this.apiUrl}/backends`, {
                            method: 'POST',
                            headers: this.getAuthHeaders(),
                            body: JSON.stringify(backend)
                        });
                        
                        const result = await response.json();
                        if (result.success) {
                            successCount++;
                        }
                    } catch (err) {
                        console.error('Failed to add backend:', backend.id, err);
                    }
                }
                
                alert(`Imported ${successCount} backends from config file!`);
            } else if (config.backend) {
                // Одиночный бэкенд
                const response = await fetch(`${this.apiUrl}/backends`, {
                    method: 'POST',
                    headers: this.getAuthHeaders(),
                    body: JSON.stringify(config.backend)
                });
                
                const result = await response.json();
                if (result.success) {
                    alert('Backend imported successfully!');
                } else {
                    alert('Error: ' + result.error);
                }
            }
        } catch (error) {
            console.error('Failed to parse config file:', error);
            alert('Invalid config file: ' + error.message);
        }
        
        e.target.value = '';
    }

    async deleteBackend(backendId) {
        if (!confirm(`Are you sure you want to remove backend "${backendId}"?`)) {
            return;
        }

        try {
            const response = await fetch(`${this.apiUrl}/backends/${backendId}`, {
                method: 'DELETE',
                headers: this.getAuthHeaders()
            });

            const result = await response.json();
            
            if (result.success) {
                alert('Backend removed successfully!');
            } else {
                alert('Error: ' + result.error);
            }
        } catch (error) {
            console.error('Failed to delete backend:', error);
            alert('Failed to delete backend: ' + error.message);
        }
    }

    // ============================================
    // .env Config Download
    // ============================================
    downloadEnvConfig(backendId, host) {
        const balancerHost = window.location.hostname || 'localhost';
        const balancerPort = 18081;
        const agentPort = 18032;
        const ollamaPort = 11434;
        
        const envContent = `AGENT_ID=${backendId}
BALANCER_URL=http://${balancerHost}:${balancerPort}
AGENT_PORT=${agentPort}
OLLAMA_URL=http://${host || 'localhost'}:${ollamaPort}
NVML_ENABLED=true
`;
        
        const blob = new Blob([envContent], { type: 'text/plain' });
        const url = URL.createObjectURL(blob);
        const a = document.createElement('a');
        a.href = url;
        a.download = `.env.${backendId}`;
        document.body.appendChild(a);
        a.click();
        document.body.removeChild(a);
        URL.revokeObjectURL(url);
        
        alert(`.env config downloaded for backend "${backendId}"`);
    }

    // ============================================
    // Utilities
    // ============================================
    formatBytes(bytes) {
        if (bytes === 0 || bytes === undefined) return '0 B';
        const k = 1024;
        const sizes = ['B', 'MB', 'GB', 'TB'];
        const i = Math.floor(Math.log(bytes) / Math.log(k));
        return parseFloat((bytes / Math.pow(k, i)).toFixed(1)) + ' ' + sizes[i];
    }

    formatDuration(seconds) {
        if (seconds < 60) return `${Math.floor(seconds)}s`;
        if (seconds < 3600) return `${Math.floor(seconds / 60)}m ${Math.floor(seconds % 60)}s`;
        const hours = Math.floor(seconds / 3600);
        const mins = Math.floor((seconds % 3600) / 60);
        return `${hours}h ${mins}m`;
    }

    getProgressClass(percent) {
        if (percent < 50) return 'low';
        if (percent < 80) return 'medium';
        return 'high';
    }

    escapeHtml(text) {
        const div = document.createElement('div');
        div.textContent = text;
        return div.innerHTML;
    }

    startAutoUpdate() {
        // Charts are updated via WebSocket messages
        // This is a fallback for demo purposes
        setInterval(() => {
            if (this.data.backends.length === 0) {
                // Demo data for testing without backend
                this.handleMetrics({
                    backends: [{
                        id: 'gpu-server-1',
                        status: 'online',
                        metrics: {
                            gpu: { usage: Math.random() * 60 + 20, memory_total: 24576, memory_used: Math.random() * 12000 + 8000 },
                            cpu: { usage: Math.random() * 40 + 10 },
                            ram: { total: 32768, used: Math.random() * 16000 + 8000 },
                            ollama: { active_requests: Math.floor(Math.random() * 5), rps: Math.random() * 10 }
                        }
                    }],
                    sessions: [],
                    cluster: {}
                });
            }
        }, 5000);
    }
}

// Initialize dashboard when DOM is ready
document.addEventListener('DOMContentLoaded', () => {
    window.dashboard = new Dashboard();
});

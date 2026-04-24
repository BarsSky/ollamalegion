// Ollama Legion - Management Platform JavaScript
// Real-time metrics and backend management

// ============================================
// Configuration
// ============================================
// When served through nginx, API and WebSocket are proxied to the balancer
const API_BASE_URL = '/api/v1';
const WS_PROTOCOL = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
const WS_URL = `${WS_PROTOCOL}//${window.location.host}/ws/metrics`;
const WS_FALLBACK_HOST = 'localhost:18081';
const WS_FALLBACK_URL = `${WS_PROTOCOL}//${WS_FALLBACK_HOST}/ws/metrics`;
let wsFallbackUsed = false;

// ============================================
// Global State
// ============================================
let wsConnection = null;
let backends = [];
let clusterState = null;
let gpuChart = null;
let cpuChart = null;
let ramChart = null;

// Loading state to prevent flickering
let isLoadingBackends = false;
let backendsLoaded = false;
let updateBackendsGridDebounceTimer = null;

// Cache for original backend Name/Host to prevent overwriting from WebSocket metrics
const originalBackendInfo = {};

// Throttle for WebSocket messages - process metrics no more than once per second per backend
const wsThrottleTimers = {};
const WS_THROTTLE_MS = 1000;

// DOM Cache for backend cards - prevents re-rendering and flickering
const backendCardCache = new Map();

// ============================================
// WebSocket Connection
// ============================================
function buildWsUrl() {
    const baseUrl = wsFallbackUsed ? WS_FALLBACK_URL : WS_URL;
    // Не добавляем token если auth отключена
    if (!authRequired) return baseUrl;
    const token = getApiToken();
    const separator = baseUrl.includes('?') ? '&' : '?';
    return `${baseUrl}${separator}token=${encodeURIComponent(token)}`;
}

function connectWebSocket() {
    const indicator = document.getElementById('websocketIndicator');
    const indicatorText = indicator.querySelector('.indicator-text');
    
    const url = buildWsUrl();
    console.log('WebSocket connecting to:', url);
    wsConnection = new WebSocket(url);
    
    wsConnection.onopen = () => {
        console.log('WebSocket connected');
        indicator.classList.add('connected');
        indicator.classList.remove('disconnected');
        indicatorText.textContent = 'Connected';
    };
    
    wsConnection.onclose = (event) => {
        console.log('WebSocket disconnected', event ? event.code : '');
        indicator.classList.add('disconnected');
        indicator.classList.remove('connected');
        indicatorText.textContent = 'Disconnected';
        
        // If connection failed and we haven't tried fallback yet, switch to fallback
        if (!wsFallbackUsed && event && (event.code === 1006 || event.code === 1001)) {
            console.log('Switching to fallback WebSocket URL:', WS_FALLBACK_URL);
            wsFallbackUsed = true;
            setTimeout(connectWebSocket, 1000);
            return;
        }
        
        // Reconnect after 3 seconds
        setTimeout(connectWebSocket, 3000);
    };
    
    wsConnection.onerror = (error) => {
        console.error('WebSocket error:', error);
        indicator.classList.add('disconnected');
        indicator.classList.remove('connected');
        indicatorText.textContent = 'Error';
    };
    
    wsConnection.onmessage = (event) => {
        try {
            const data = JSON.parse(event.data);
            if (data.error) {
                console.error('WS Error:', data.error);
                return;
            }
            // Определяем тип сообщения: ClusterState (с backends) или BackendMetrics (с id, но без backends)
            if (isClusterState(data)) {
                updateDashboard(data);
            } else if (isBackendMetrics(data)) {
                updateBackendMetrics(data);
            }
        } catch (e) {
            console.error('Failed to parse WS message:', e);
        }
    };
}

// ============================================
// Data Normalization
// ============================================
// Normalize backend data to support both lowercase (API) and PascalCase (WebSocket) formats
function normalizeBackend(backend) {
    if (!backend) return {};
    
    // Generate fallback Name from ID or Host if not provided
    // Use strict undefined check to allow empty string from API (preserve intentional blanks)
    let fallbackName;
    if (backend.Name !== undefined) {
        fallbackName = backend.Name;
    } else if (backend.name !== undefined) {
        fallbackName = backend.name;
    } else if (backend.Host || backend.host) {
        fallbackName = backend.Host || backend.host;
    } else if (backend.ID || backend.id) {
        fallbackName = backend.ID || backend.id;
    } else {
        fallbackName = 'Unknown';
    }
    
    // Generate fallback Host from ID if not provided (ID may contain host info)
    const fallbackHost = backend.Host || backend.host ||
                          (backend.ID || backend.id || '').split(':')[0] ||
                          'N/A';
    
    return {
        ID: backend.ID || backend.id || '',
        Name: fallbackName,
        Host: fallbackHost,
        Status: backend.Status || backend.status || 'unknown',
        OllamaPort: backend.OllamaPort || backend.ollamaPort || 11434,
        AgentPort: backend.AgentPort || backend.agentPort || 18032,
        Weight: backend.Weight || backend.weight || 1,
        MaxConcurrentRequests: backend.MaxConcurrentRequests || backend.maxConcurrentRequests || backend.MaxConcurrentReqs || backend.maxConcurrentReqs || 10,
        Labels: backend.Labels || backend.labels || [],
        GPU: backend.GPU || backend.gpu || {},
        System: backend.System || backend.system || {},
        Ollama: backend.Ollama || backend.ollama || {},
        HasAgent: backend.HasAgent || backend.hasAgent || false,
        LastAgentContact: backend.LastAgentContact || backend.lastAgentContact || '',
        Timestamp: backend.Timestamp || backend.timestamp
    };
}

// Normalize GPU data
function normalizeGPU(gpu) {
    if (!gpu) return {};
    
    return {
        UsagePercent: gpu.UsagePercent || gpu.usagePercent || 0,
        MemoryTotal: gpu.MemoryTotal || gpu.memoryTotal || 0,
        MemoryUsed: gpu.MemoryUsed || gpu.memoryUsed || 0,
        MemoryFree: gpu.MemoryFree || gpu.memoryFree || 0,
        Temperature: gpu.Temperature || gpu.temperature || 0,
        PowerUsage: gpu.PowerUsage || gpu.powerUsage || 0,
        PowerLimit: gpu.PowerLimit || gpu.powerLimit || 0,
        GpuClock: gpu.GpuClock || gpu.gpuClock || 0,
        MemClock: gpu.MemClock || gpu.memClock || 0
    };
}

// Normalize System data
function normalizeSystem(system) {
    if (!system) return {};
    
    return {
        CPUUsagePercent: system.CPUUsagePercent || system.cpuUsagePercent || 0,
        MemoryTotal: system.MemoryTotal || system.memoryTotal || 0,
        MemoryUsed: system.MemoryUsed || system.memoryUsed || 0,
        MemoryFree: system.MemoryFree || system.memoryFree || 0,
        DiskTotal: system.DiskTotal || system.diskTotal || 0,
        DiskUsed: system.DiskUsed || system.diskUsed || 0,
        DiskFree: system.DiskFree || system.diskFree || 0,
        NetworkRX: system.NetworkRX || system.networkRX || 0,
        NetworkTX: system.NetworkTX || system.networkTX || 0
    };
}

// Normalize Ollama data
function normalizeOllama(ollama) {
    if (!ollama) return {};
    
    const rawModels = ollama.RunningModels || ollama.runningModels || [];
    const normalizedModels = Array.isArray(rawModels)
        ? rawModels.map(m => normalizeModel(m))
        : [];
    
    return {
        RunningModels: normalizedModels,
        ActiveRequests: ollama.ActiveRequests || ollama.activeRequests || 0,
        TotalRequests: ollama.TotalRequests || ollama.totalRequests || 0,
        AvgResponseTime: ollama.AvgResponseTime || ollama.avgResponseTime || 0,
        RequestsPerSecond: ollama.RequestsPerSecond || ollama.requestsPerSecond || 0,
        MaxModels: ollama.MaxModels || ollama.maxModels || 0,
        MaxConcurrentRequests: ollama.MaxConcurrentRequests || ollama.maxConcurrentRequests || 0
    };
}

// Normalize cluster state data
function normalizeClusterState(data) {
    if (!data) return { Backends: [], HealthyBackends: 0, TotalBackends: 0 };
    
    const backends = data.Backends || data.backends || [];
    const normalizedBackends = backends.map(b => normalizeBackend(b));
    const healthyBackends = data.HealthyBackends || data.healthyBackends ||
                            backends.filter(b => (b.Status || b.status) === 'healthy').length;
    const totalBackends = data.TotalBackends || data.totalBackends || backends.length;
    
    return {
        Backends: sortBackends(normalizedBackends),
        HealthyBackends: healthyBackends,
        TotalBackends: totalBackends,
        Timestamp: data.Timestamp || data.timestamp
    };
}

// ============================================
// Backend Sorting (Stable by ID)
// ============================================
// Sort backends by ID to ensure stable order across updates
function sortBackends(backends) {
    if (!Array.isArray(backends)) return backends;
    
    backends.sort((a, b) => {
        const idA = (a.ID || a.id || '').toLowerCase();
        const idB = (b.ID || b.id || '').toLowerCase();
        return idA.localeCompare(idB);
    });
    
    return backends;
}

// ============================================
// Message Type Detection
// ============================================
// Определяет, является ли сообщение полным состоянием кластера (ClusterState)
function isClusterState(data) {
    return data && (Array.isArray(data.backends) || Array.isArray(data.Backends));
}

// Определяет, является ли сообщение метриками отдельного бэкенда (BackendMetrics)
function isBackendMetrics(data) {
    return data && (data.id || data.ID) && !Array.isArray(data.backends) && !Array.isArray(data.Backends);
}

// ============================================
// Backend Metrics Update (для отдельных метрик)
// ============================================
function updateBackendMetrics(metricsData) {
    // Если clusterState еще не инициализирован, игнорируем метрики
    if (!clusterState || !clusterState.Backends) {
        return;
    }
    
    // Нормализуем полученные метрики
    const normalizedMetrics = normalizeBackend(metricsData);
    const backendId = normalizedMetrics.ID || normalizedMetrics.id;
    
    if (!backendId) {
        // console.warn('Received metrics without ID:', metricsData);
        return;
    }
    
    // THROTTLE: Обрабатываем метрики не чаще 1 раза в секунду для каждого бэкенда
    const now = Date.now();
    if (wsThrottleTimers[backendId] && (now - wsThrottleTimers[backendId]) < WS_THROTTLE_MS) {
        // Пропускаем это сообщение - слишком рано после последнего
        return;
    }
    wsThrottleTimers[backendId] = now;
    
    // Находим существующий бэкенд и обновляем его метрики
    const backendIndex = clusterState.Backends.findIndex(b => {
        const existingId = (b.ID || b.id);
        return existingId === backendId;
    });
    
    if (backendIndex !== -1) {
        // Обновляем метрики существующего бэкенда
        const existingBackend = clusterState.Backends[backendIndex];
        
        // Сохраняем оригинальные Name/Host в кэше при первом получении
        if (!originalBackendInfo[backendId]) {
            originalBackendInfo[backendId] = {
                Name: existingBackend.Name || normalizedMetrics.Name || 'Unknown',
                Host: existingBackend.Host || normalizedMetrics.Host || 'N/A'
            };
        }
        
        // Получаем оригинальные Name/Host из кэша
        const cachedInfo = originalBackendInfo[backendId];
        
        // ВАЖНО: Сохраняем Name и Host из кэша/существующего бэкенда чтобы не потерять их
        // Метрики могут приходить без Name/Host, поэтому защищаем от перезаписи пустыми значениями
        clusterState.Backends[backendIndex] = {
            ...existingBackend,
            // Используем кэшированные оригинальные значения вместо перезаписи из WebSocket
            Name: cachedInfo.Name,
            Host: cachedInfo.Host,
            // Обновляем только метрики
            GPU: normalizedMetrics.GPU || normalizedMetrics.gpu || existingBackend.GPU,
            System: normalizedMetrics.System || normalizedMetrics.system || existingBackend.System,
            Ollama: normalizedMetrics.Ollama || normalizedMetrics.ollama || existingBackend.Ollama,
            HasAgent: normalizedMetrics.HasAgent !== undefined ? normalizedMetrics.HasAgent : true,
            Timestamp: normalizedMetrics.Timestamp || normalizedMetrics.timestamp || existingBackend.Timestamp
        };
        
        // Сортируем бэкенды после обновления метрик для стабильного порядка
        sortBackends(clusterState.Backends);
        
        // Перерисовываем UI с обновленными данными
        // Используем debounce для backends grid чтобы избежать мерцания
        updateClusterOverview(clusterState);
        updateBackendsGridDebounced(clusterState, false); // debounce для WebSocket обновлений
        updateCharts(clusterState);
        updateFooter(clusterState);
    } else {
        // Бэкенд не найден в текущем состоянии - запрашиваем полное обновление
        // console.log('New backend detected, fetching full state:', backendId);
        fetchBackends();
    }
}

// ============================================
// Dashboard Update
// ============================================
function updateDashboard(data) {
    // Normalize data to support both API (lowercase) and WebSocket (PascalCase) formats
    clusterState = normalizeClusterState(data);
    
    // Update cluster overview
    updateClusterOverview(clusterState);
    
    // Update backends grid - use immediate render for initial API load
    // This ensures backends are shown as soon as API data is received
    updateBackendsGridDebounced(clusterState, true);
    
    // Update sessions table
    updateSessionsTable(clusterState);
    
    // Update charts
    updateCharts(clusterState);
    
    // Update footer
    updateFooter(clusterState);
}

function updateClusterOverview(data) {
    const healthyBackends = data.HealthyBackends || 0;
    const totalBackends = data.TotalBackends || 0;
    
    document.getElementById('activeBackends').textContent = `${healthyBackends}/${totalBackends}`;
    
    // Calculate total GPU usage
    let totalGpuUsage = 0;
    let gpuCount = 0;
    let activeSessions = 0;
    
    if (data.Backends && Array.isArray(data.Backends)) {
        data.Backends.forEach(backend => {
            const gpu = normalizeGPU(backend.GPU || backend.gpu);
            const ollama = normalizeOllama(backend.Ollama || backend.ollama);
            
            if (gpu.UsagePercent !== undefined) {
                totalGpuUsage += gpu.UsagePercent;
                gpuCount++;
            }
            if (ollama.ActiveRequests !== undefined) {
                activeSessions += ollama.ActiveRequests;
            }
        });
    }
    
    const avgGpuUsage = gpuCount > 0 ? (totalGpuUsage / gpuCount).toFixed(1) : 0;
    document.getElementById('totalGpuUsage').textContent = avgGpuUsage;
    document.getElementById('activeSessions').textContent = activeSessions;
    
    // Calculate cluster RPS (requests per second)
    const rps = activeSessions > 0 ? (activeSessions / 5).toFixed(1) : '0';
    document.getElementById('clusterRps').textContent = rps;
    
    // Update cluster status
    document.getElementById('clusterStatus').textContent = `Cluster: ${healthyBackends}/${totalBackends} healthy`;
}

// Debounced version of updateBackendsGrid to prevent excessive re-renders
function updateBackendsGridDebounced(data, immediate = false) {
    if (updateBackendsGridDebounceTimer) {
        clearTimeout(updateBackendsGridDebounceTimer);
    }
    
    if (immediate) {
        updateBackendsGrid(data);
        return;
    }
    
    updateBackendsGridDebounceTimer = setTimeout(() => {
        updateBackendsGrid(data);
    }, 1000); // 1000ms debounce to reduce DOM re-renders from WebSocket updates
}

// Get status class for backend status
function getStatusClass(status) {
    const normalizedStatus = (status || 'unknown').toLowerCase();
    return normalizedStatus === 'healthy' ? 'online' : (normalizedStatus === 'unhealthy' ? 'offline' : 'warning');
}

// Normalize model data inside RunningModels
function normalizeModel(model) {
    if (!model) return {};
    return {
        Name: model.Name || model.name || '',
        Size: model.Size || model.size || 0,
        VramUsage: model.VramUsage || model.vramUsage || 0,
        ExpiresAt: model.ExpiresAt || model.expiresAt || '',
        Digest: model.Digest || model.digest || '',
        Family: model.Family || model.family || '',
        ParameterSize: model.ParameterSize || model.parameterSize || '',
        Quantization: model.Quantization || model.quantization || ''
    };
}

// Get status text for backend status
function getStatusText(status) {
    const normalizedStatus = (status || 'unknown').toLowerCase();
    return normalizedStatus === 'healthy' ? 'Online' : (normalizedStatus === 'unhealthy' ? 'Unhealthy' : 'Starting');
}

// Create a new backend card element
function createBackendCard(backend) {
    const status = backend.Status || 'unknown';
    const statusClass = getStatusClass(status);
    const statusText = getStatusText(status);
    const hasAgent = backend.HasAgent || backend.hasAgent || false;
    
    // Normalize nested objects
    const gpu = normalizeGPU(backend.GPU || backend.gpu);
    const system = normalizeSystem(backend.System || backend.system);
    const ollama = normalizeOllama(backend.Ollama || backend.ollama);
    
    const gpuUsage = gpu.UsagePercent || 0;
    const gpuMemoryRaw = gpu.MemoryUsed && gpu.MemoryTotal
        ? ((gpu.MemoryUsed / gpu.MemoryTotal) * 100)
        : 0;
    const gpuMemory = parseFloat(gpuMemoryRaw.toFixed(1));
    const cpuUsage = system.CPUUsagePercent || 0;
    const ramUsageRaw = system.MemoryUsed && system.MemoryTotal
        ? ((system.MemoryUsed / system.MemoryTotal) * 100)
        : 0;
    const ramUsage = parseFloat(ramUsageRaw.toFixed(1));
    
    const card = document.createElement('div');
    card.className = `backend-card ${statusClass} fade-in`;
    card.dataset.backendId = backend.ID;
    card.innerHTML = `
        ${!hasAgent ? `<div class="agent-warning">⚠️ Agent not connected — metrics unavailable, load balancing simplified</div>` : ''}
        <div class="backend-header">
            <div class="backend-header-left">
                <span class="backend-id">${escapeHtml(backend.Name || backend.ID)}</span>
                <span class="backend-status ${statusClass}">${statusText}</span>
                <span class="agent-indicator ${hasAgent ? 'agent-online' : 'agent-offline'}" title="${hasAgent ? 'Agent Online' : 'Agent Offline'}">${hasAgent ? '🟢' : '⚪'}</span>
            </div>
            <div class="backend-actions">
                <button class="action-btn" title="Download .env" onclick="downloadEnvConfig('${escapeHtml(backend.ID)}')">📥</button>
                <button class="delete-backend-btn" title="Delete backend" onclick="deleteBackend('${escapeHtml(backend.ID)}')">🗑️</button>
            </div>
        </div>
        <div class="backend-ip">
            <span>🌐</span>
            <span>${escapeHtml(backend.Host || 'N/A')}:${backend.OllamaPort || 'N/A'}</span>
        </div>
        
        <div class="metric-row">
            <div class="metric-row-header">
                <span class="metric-row-label">
                    <span class="metric-row-icon">🔥</span>
                    GPU Usage
                </span>
                <span class="metric-row-value gpu-usage">${gpuUsage.toFixed(1)}%</span>
            </div>
            <div class="progress-bar">
                <div class="progress-fill ${getProgressClass(gpuUsage)}" style="width: ${gpuUsage}%"></div>
            </div>
        </div>
        
        <div class="metric-row">
            <div class="metric-row-header">
                <span class="metric-row-label">
                    <span class="metric-row-icon">💾</span>
                    GPU Memory
                </span>
                <span class="metric-row-value gpu-memory">${gpuMemory}%</span>
            </div>
            <div class="progress-bar">
                <div class="progress-fill ${getProgressClass(gpuMemory)}" style="width: ${gpuMemory}%"></div>
            </div>
        </div>
        
        <div class="metric-row">
            <div class="metric-row-header">
                <span class="metric-row-label">
                    <span class="metric-row-icon">⚙️</span>
                    CPU Usage
                </span>
                <span class="metric-row-value cpu-usage">${cpuUsage.toFixed(1)}%</span>
            </div>
            <div class="progress-bar">
                <div class="progress-fill ${getProgressClass(cpuUsage)}" style="width: ${cpuUsage}%"></div>
            </div>
        </div>
        
        <div class="metric-row">
            <div class="metric-row-header">
                <span class="metric-row-label">
                    <span class="metric-row-icon">🧠</span>
                    RAM Usage
                </span>
                <span class="metric-row-value ram-usage">${ramUsage}%</span>
            </div>
            <div class="progress-bar">
                <div class="progress-fill ${getProgressClass(ramUsage)}" style="width: ${ramUsage}%"></div>
            </div>
        </div>
        
        <div class="backend-stats">
            <div class="stat-item">
                <div class="stat-label">Active Requests</div>
                <div class="stat-value active-requests">${ollama.ActiveRequests || 0}</div>
            </div>
            <div class="stat-item">
                <div class="stat-label">Models Loaded</div>
                <div class="stat-value models-loaded">${ollama.RunningModels ? ollama.RunningModels.length : 0}</div>
            </div>
            <div class="stat-item">
                <div class="stat-label">Max Models</div>
                <div class="stat-value max-models">${ollama.MaxModels || '-'}</div>
            </div>
        </div>
        
        <div class="backend-limits">
            <div class="limit-item">
                <div class="limit-label">
                    <span class="limit-icon">📊</span>
                    Max Concurrent Requests
                </div>
                <div class="limit-value max-concurrent-requests">${ollama.MaxConcurrentRequests || '-'}</div>
            </div>
        </div>
        
        ${ollama.RunningModels && ollama.RunningModels.length > 0 ? `
            <div class="models-section">
                <div class="models-header">
                    <span class="metric-row-icon">📦</span>
                    Loaded Models (${ollama.RunningModels.length})
                </div>
                <div class="models-list">
                    ${ollama.RunningModels.map(m => `
                        <div class="model-item">
                            <div class="model-info">
                                <span class="model-name">${escapeHtml(m.Name)}</span>
                                <span class="model-size">${formatBytes(m.Size || 0)}</span>
                            </div>
                            <div class="model-details">
                                ${m.Family ? `<span class="model-badge">${escapeHtml(m.Family)}</span>` : ''}
                                ${m.ParameterSize ? `<span class="model-badge">${escapeHtml(m.ParameterSize)}</span>` : ''}
                                ${m.Quantization ? `<span class="model-badge quant">${escapeHtml(m.Quantization)}</span>` : ''}
                            </div>
                        </div>
                    `).join('')}
                </div>
            </div>
        ` : `
            <div class="models-section empty">
                <div class="models-header">
                    <span class="metric-row-icon">📦</span>
                    Loaded Models (0)
                </div>
                <p class="no-models">No models loaded</p>
            </div>
        `}
    `;
    return card;
}

// Update an existing backend card with new data
function updateBackendCard(card, backend) {
    const status = backend.Status || 'unknown';
    const statusClass = getStatusClass(status);
    const statusText = getStatusText(status);
    
    // Normalize nested objects
    const gpu = normalizeGPU(backend.GPU || backend.gpu);
    const system = normalizeSystem(backend.System || backend.system);
    const ollama = normalizeOllama(backend.Ollama || backend.ollama);
    
    const gpuUsage = gpu.UsagePercent || 0;
    const gpuMemoryRaw = gpu.MemoryUsed && gpu.MemoryTotal
        ? ((gpu.MemoryUsed / gpu.MemoryTotal) * 100)
        : 0;
    const gpuMemory = parseFloat(gpuMemoryRaw.toFixed(1));
    const cpuUsage = system.CPUUsagePercent || 0;
    const ramUsageRaw = system.MemoryUsed && system.MemoryTotal
        ? ((system.MemoryUsed / system.MemoryTotal) * 100)
        : 0;
    const ramUsage = parseFloat(ramUsageRaw.toFixed(1));
    
    // Update text values only
    card.querySelector('.backend-id').textContent = escapeHtml(backend.Name || backend.ID);
    card.querySelector('.backend-status').textContent = statusText;
    
    // Update agent indicator
    const hasAgent = backend.HasAgent || backend.hasAgent || false;
    const agentIndicator = card.querySelector('.agent-indicator');
    if (agentIndicator) {
        agentIndicator.className = `agent-indicator ${hasAgent ? 'agent-online' : 'agent-offline'}`;
        agentIndicator.textContent = hasAgent ? '🟢' : '⚪';
        agentIndicator.title = hasAgent ? 'Agent Online' : 'Agent Offline';
    }
    
    // Update agent warning
    const existingWarning = card.querySelector('.agent-warning');
    if (!hasAgent && !existingWarning) {
        const header = card.querySelector('.backend-header');
        if (header) {
            const warning = document.createElement('div');
            warning.className = 'agent-warning';
            warning.textContent = '⚠️ Agent not connected — metrics unavailable, load balancing simplified';
            card.insertBefore(warning, header);
        }
    } else if (hasAgent && existingWarning) {
        existingWarning.remove();
    }
    
    // Update metrics values
    card.querySelector('.gpu-usage').textContent = gpuUsage.toFixed(1) + '%';
    card.querySelector('.gpu-memory').textContent = gpuMemory + '%';
    card.querySelector('.cpu-usage').textContent = cpuUsage.toFixed(1) + '%';
    card.querySelector('.ram-usage').textContent = ramUsage + '%';
    
    // Update progress bar widths
    const progressFills = card.querySelectorAll('.progress-fill');
    if (progressFills.length >= 4) {
        progressFills[0].style.width = gpuUsage + '%';
        progressFills[0].className = 'progress-fill ' + getProgressClass(gpuUsage);
        progressFills[1].style.width = gpuMemory + '%';
        progressFills[1].className = 'progress-fill ' + getProgressClass(gpuMemory);
        progressFills[2].style.width = cpuUsage + '%';
        progressFills[2].className = 'progress-fill ' + getProgressClass(cpuUsage);
        progressFills[3].style.width = ramUsage + '%';
        progressFills[3].className = 'progress-fill ' + getProgressClass(ramUsage);
    }
    
    // Update stats
    card.querySelector('.active-requests').textContent = ollama.ActiveRequests || 0;
    card.querySelector('.models-loaded').textContent = ollama.RunningModels ? ollama.RunningModels.length : 0;
    card.querySelector('.max-models').textContent = ollama.MaxModels || '-';
    card.querySelector('.max-concurrent-requests').textContent = ollama.MaxConcurrentRequests || '-';
    
    // Update status classes
    card.classList.remove('online', 'offline', 'warning');
    card.classList.add(statusClass);
    card.querySelector('.backend-status').className = 'backend-status ' + statusClass;
    
    // Update models list section
    const modelsSection = card.querySelector('.models-section');
    if (ollama.RunningModels && ollama.RunningModels.length > 0) {
        modelsSection.outerHTML = `
            <div class="models-section">
                <div class="models-header">
                    <span class="metric-row-icon">📦</span>
                    Loaded Models (${ollama.RunningModels.length})
                </div>
                <div class="models-list">
                    ${ollama.RunningModels.map(m => `
                        <div class="model-item">
                            <div class="model-info">
                                <span class="model-name">${escapeHtml(m.Name)}</span>
                                <span class="model-size">${formatBytes(m.Size || 0)}</span>
                            </div>
                            <div class="model-details">
                                ${m.Family ? `<span class="model-badge">${escapeHtml(m.Family)}</span>` : ''}
                                ${m.ParameterSize ? `<span class="model-badge">${escapeHtml(m.ParameterSize)}</span>` : ''}
                                ${m.Quantization ? `<span class="model-badge quant">${escapeHtml(m.Quantization)}</span>` : ''}
                            </div>
                        </div>
                    `).join('')}
                </div>
            </div>
        `;
    } else {
        modelsSection.outerHTML = `
            <div class="models-section empty">
                <div class="models-header">
                    <span class="metric-row-icon">📦</span>
                    Loaded Models (0)
                </div>
                <p class="no-models">No models loaded</p>
            </div>
        `;
    }
}

// Optimized updateBackendsGrid using DOM caching and selective updates
function updateBackendsGrid(data) {
    const grid = document.getElementById('backendsGrid');
    
    // Normalize backends data first
    const rawData = data.Backends || data.backends || [];
    const newBackends = sortBackends(rawData.map(b => normalizeBackend(b)));
    
    // Don't render until we have loaded data from API at least once
    // This prevents flickering during initial load
    if (!backendsLoaded && !isLoadingBackends) {
        // Show loading state only if we haven't shown it yet
        grid.innerHTML = `
            <div class="loading-container" style="grid-column: 1 / -1;">
                <div class="loading-spinner"></div>
                <div class="loading-text">Loading backends...</div>
            </div>
        `;
        isLoadingBackends = true;
        // Store backends immediately to prevent flicker
        backends = newBackends;
        return;
    }
    
    // Handle empty backends case
    if (newBackends.length === 0) {
        // Clear cache
        backendCardCache.clear();
        grid.innerHTML = `
            <div class="backend-card offline" style="grid-column: 1 / -1;">
                <div class="backend-header">
                    <span class="backend-id">No backends configured</span>
                    <span class="backend-status offline">Offline</span>
                </div>
                <p style="color: var(--text-muted); text-align: center; padding: 2rem;">
                    Add a backend server using the form above or upload a configuration file.
                </p>
            </div>
        `;
        backends = newBackends;
        backendsLoaded = true;
        isLoadingBackends = false;
        return;
    }
    
    // Track new backend IDs for cache cleanup
    const newBackendIds = new Set();
    
    // Process each backend
    newBackends.forEach(backend => {
        const id = backend.ID;
        newBackendIds.add(id);
        
        if (backendCardCache.has(id)) {
            // Update existing card in cache
            updateBackendCard(backendCardCache.get(id), backend);
        } else {
            // Create new card and add to cache
            const card = createBackendCard(backend);
            grid.appendChild(card);
            backendCardCache.set(id, card);
        }
    });
    
    // Remove cards for backends that no longer exist
    for (const [id, card] of backendCardCache.entries()) {
        if (!newBackendIds.has(id)) {
            card.remove();
            backendCardCache.delete(id);
        }
    }
    
    // Update global backends array
    backends = newBackends;
    
    // Mark backends as loaded after first successful render
    backendsLoaded = true;
    isLoadingBackends = false;
}

function updateSessionsTable(data) {
    const tbody = document.getElementById('sessionsTableBody');
    
    // For now, show empty state
    // In a real implementation, you would fetch session data from the API
    tbody.innerHTML = `
        <tr class="empty-row">
            <td colspan="6">No active sessions</td>
        </tr>
    `;
}

function updateFooter(data) {
    document.getElementById('apiVersion').textContent = '1.0.0';
    const ts = data.Timestamp || data.timestamp;
    // Filter out Go zero-value timestamp to avoid displaying year 1901/2001
    const isGoZero = ts && (ts.startsWith('0001-01-01') || ts.startsWith('0001-01-01T00:00:00'));
    document.getElementById('lastUpdate').textContent = new Date((!isGoZero && ts) ? ts : Date.now()).toLocaleTimeString();
}

// ============================================
// Charts
// ============================================
function initCharts() {
    const chartOptions = {
        responsive: true,
        maintainAspectRatio: false,
        plugins: {
            legend: {
                display: true,
                position: 'top'
            }
        },
        scales: {
            y: {
                beginAtZero: true,
                max: 100
            }
        }
    };
    
    // GPU Chart
    const gpuCtx = document.getElementById('gpuChart').getContext('2d');
    gpuChart = new Chart(gpuCtx, {
        type: 'bar',
        data: { labels: [], datasets: [{ label: 'GPU Usage %', data: [], backgroundColor: 'rgba(196, 145, 107, 0.8)' }] },
        options: chartOptions
    });
    
    // CPU Chart
    const cpuCtx = document.getElementById('cpuChart').getContext('2d');
    cpuChart = new Chart(cpuCtx, {
        type: 'bar',
        data: { labels: [], datasets: [{ label: 'CPU Usage %', data: [], backgroundColor: 'rgba(127, 184, 231, 0.8)' }] },
        options: chartOptions
    });
    
    // RAM Chart
    const ramCtx = document.getElementById('ramChart').getContext('2d');
    ramChart = new Chart(ramCtx, {
        type: 'bar',
        data: { labels: [], datasets: [{ label: 'RAM Usage %', data: [], backgroundColor: 'rgba(72, 187, 120, 0.8)' }] },
        options: chartOptions
    });
}

function updateCharts(data) {
    if (!gpuChart || !cpuChart || !ramChart) return;
    
    const rawData = data.Backends || data.backends || [];
    const backends = sortBackends(rawData.map(b => normalizeBackend(b)));
    
    const labels = backends.map(b => b.Name || b.ID);
    const gpuData = backends.map(b => {
        const gpu = normalizeGPU(b.GPU || b.gpu);
        return gpu.UsagePercent || 0;
    });
    const cpuData = backends.map(b => {
        const system = normalizeSystem(b.System || b.system);
        return system.CPUUsagePercent || 0;
    });
    const ramData = backends.map(b => {
        const system = normalizeSystem(b.System || b.system);
        if (system.MemoryUsed && system.MemoryTotal) {
            return (system.MemoryUsed / system.MemoryTotal) * 100;
        }
        return 0;
    });
    
    gpuChart.data.labels = labels;
    gpuChart.data.datasets[0].data = gpuData;
    gpuChart.update();
    
    cpuChart.data.labels = labels;
    cpuChart.data.datasets[0].data = cpuData;
    cpuChart.update();
    
    ramChart.data.labels = labels;
    ramChart.data.datasets[0].data = ramData;
    ramChart.update();
}

// ============================================
// Backend Management
// ============================================
document.getElementById('addBackendForm').addEventListener('submit', async (e) => {
    e.preventDefault();
    
    const formData = new FormData(e.target);
    const backend = {
        id: formData.get('id'),
        name: formData.get('name'),
        host: formData.get('host'),
        ollamaPort: parseInt(formData.get('ollamaPort')),
        agentPort: parseInt(formData.get('agentPort')),
        weight: parseInt(formData.get('weight')),
        maxConcurrentRequests: parseInt(formData.get('maxConcurrentRequests')),
        labels: formData.get('labels').split(',').map(l => l.trim()).filter(l => l)
    };
    
    try {
        const response = await fetch(`${API_BASE_URL}/backends`, {
            method: 'POST',
            headers: {
                'Content-Type': 'application/json',
                'X-API-Token': getApiToken()
            },
            body: JSON.stringify(backend)
        });
        
        const result = await response.json();
        
        if (response.ok && result.success) {
            const generateConfig = e.target.querySelector('[name="generateAgentConfig"]')?.checked;
            if (generateConfig) {
                downloadEnvConfig(backend.id);
            }
            alert('Backend added successfully!' + (generateConfig ? ' Agent config downloaded.' : ''));
            e.target.reset();
        } else {
            alert(`Error: ${result.error || 'Failed to add backend'}`);
        }
    } catch (error) {
        console.error('Failed to add backend:', error);
        alert('Failed to add backend. Check console for details.');
    }
});

async function deleteBackend(backendId) {
    if (!confirm(`Are you sure you want to delete backend "${backendId}"?`)) {
        return;
    }
    
    try {
        const response = await fetch(`${API_BASE_URL}/backends/${encodeURIComponent(backendId)}`, {
            method: 'DELETE',
            headers: {
                'X-API-Token': getApiToken()
            }
        });
        
        const result = await response.json();
        
        if (response.ok && result.success) {
            alert('Backend deleted successfully!');
        } else {
            alert(`Error: ${result.error || 'Failed to delete backend'}`);
        }
    } catch (error) {
        console.error('Failed to delete backend:', error);
        alert('Failed to delete backend. Check console for details.');
    }
}

function downloadEnvConfig(backendId) {
    // Find backend using normalized ID comparison
    const backend = backends.find(b => (b.ID || b.id) === backendId);
    if (!backend) return;
    
    // Use normalized values
    const normalizedBackend = normalizeBackend(backend);
    const balancerHost = window.location.host || 'localhost:18081';
    const envContent = `# Agent Configuration for ${normalizedBackend.Name || backendId}
AGENT_ID=${backendId}
BALANCER_URL=http://${balancerHost}
AGENT_PORT=${normalizedBackend.AgentPort || 18032}
OLLAMA_URL=http://localhost:11434
NVML_ENABLED=true
METRICS_INTERVAL=5s
HEARTBEAT_INTERVAL=3s
LOG_LEVEL=info
LOG_FORMAT=text
`;
    
    const blob = new Blob([envContent], { type: 'text/plain' });
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = `agent-${backendId}.env`;
    document.body.appendChild(a);
    a.click();
    document.body.removeChild(a);
    URL.revokeObjectURL(url);
}

// ============================================
// Config Upload
// ============================================
document.getElementById('configUpload').addEventListener('change', async (e) => {
    const file = e.target.files[0];
    if (!file) return;
    
    try {
        const text = await file.text();
        const config = JSON.parse(text);
        
        let backendsToImport = [];
        
        if (Array.isArray(config)) {
            backendsToImport = config;
        } else if (config.backends && Array.isArray(config.backends)) {
            backendsToImport = config.backends;
        } else {
            alert('Invalid configuration format. Expected an array or { backends: [...] }');
            return;
        }
        
        let successCount = 0;
        let errorCount = 0;
        
        for (const backend of backendsToImport) {
            try {
                const response = await fetch(`${API_BASE_URL}/backends`, {
                    method: 'POST',
                    headers: {
                        'Content-Type': 'application/json',
                        'X-API-Token': getApiToken()
                    },
                    body: JSON.stringify({
                        id: backend.id || backend.ID,
                        name: backend.name || backend.Name,
                        host: backend.host || backend.Host,
                        ollamaPort: backend.ollamaPort || backend.OllamaPort || 11434,
agentPort: backend.agentPort || backend.AgentPort || 18032,
                        weight: backend.weight || backend.Weight || 1,
                        maxConcurrentRequests: backend.maxConcurrentRequests || backend.MaxConcurrentReqs || 10,
                        labels: backend.labels || backend.Labels || []
                    })
                });
                
                if (response.ok) {
                    successCount++;
                } else {
                    errorCount++;
                }
            } catch (error) {
                errorCount++;
            }
        }
        
        alert(`Import completed: ${successCount} backends added, ${errorCount} failed`);
        e.target.value = '';
    } catch (error) {
        console.error('Failed to upload config:', error);
        alert('Failed to parse configuration file. Make sure it\'s valid JSON.');
    }
});

// ============================================
// Utility Functions
// ============================================
function escapeHtml(text) {
    const div = document.createElement('div');
    div.textContent = text;
    return div.innerHTML;
}

function getProgressClass(value) {
    if (value < 50) return 'low';
    if (value < 80) return 'medium';
    return 'high';
}

function formatBytes(bytes) {
    if (bytes === 0) return '0 B';
    const k = 1024;
    const sizes = ['B', 'KB', 'MB', 'GB', 'TB'];
    const i = Math.floor(Math.log(bytes) / Math.log(k));
    return parseFloat((bytes / Math.pow(k, i)).toFixed(1)) + ' ' + sizes[i];
}

// Safe localStorage wrapper - handles browsers with storage restrictions
const safeStorage = {
    get: function(key) {
        try {
            return localStorage.getItem(key);
        } catch (e) {
            console.warn('localStorage access denied, using in-memory storage');
            return safeStorage.memory[key] || null;
        }
    },
    set: function(key, value) {
        try {
            localStorage.setItem(key, value);
        } catch (e) {
            console.warn('localStorage access denied, using in-memory storage');
            safeStorage.memory[key] = value;
        }
    },
    memory: {}
};

function getApiToken() {
    return safeStorage.get('apiToken') || '';
}

function setApiToken(token) {
    safeStorage.set('apiToken', token);
}

// ============================================
// Initialization
// ============================================
// ============================================
// Token Modal & Auth Flow
// ============================================
let authRequired = false;
let authHeaderName = 'X-API-Token';

function showTokenModal(show = true) {
    const modal = document.getElementById('tokenModal');
    if (modal) {
        modal.classList.toggle('active', show);
        if (show) {
            const input = document.getElementById('tokenModalInput');
            if (input) {
                input.value = getApiToken();
                input.focus();
            }
        }
    }
}

async function verifyToken(token) {
    try {
        const resp = await fetch(`${API_BASE_URL}/auth/status`, {
            headers: { 'X-API-Token': token }
        });
        return resp.ok;
    } catch (e) {
        return false;
    }
}

async function initAuth() {
    // 1. Probe health endpoint to check if auth is enabled
    let healthData = null;
    try {
        const resp = await fetch(`${API_BASE_URL}/health`);
        if (resp.ok) {
            healthData = await resp.json();
        }
    } catch (e) {
        console.warn('Health check failed, assuming auth is required', e);
    }

    authRequired = healthData?.authEnabled ?? true;
    authHeaderName = healthData?.authHeader || 'X-API-Token';
    const wsEndpoint = healthData?.wsEndpoint || '/ws/metrics';

    // 2. If auth is disabled, proceed without token
    if (!authRequired) {
        console.log('Auth is disabled, proceeding without token');
        proceedToApp();
        return;
    }

    // 3. Try existing stored token
    const storedToken = getApiToken();
    if (storedToken) {
        const valid = await verifyToken(storedToken);
        if (valid) {
            console.log('Stored token is valid');
            proceedToApp();
            return;
        }
    }

    // 4. Show token modal for user input
    console.log('Auth required, showing token modal');
    showTokenModal(true);
}

function proceedToApp() {
    showTokenModal(false);

    // Show loading state
    const grid = document.getElementById('backendsGrid');
    if (grid) {
        grid.innerHTML = `
            <div class="loading-container" style="grid-column: 1 / -1;">
                <div class="loading-spinner"></div>
                <div class="loading-text">Loading backends...</div>
            </div>
        `;
    }
    isLoadingBackends = true;

    // Initialize charts first
    initCharts();

    // Fetch backends from API BEFORE connecting WebSocket
    fetchBackends();

    // Connect WebSocket after initiating API fetch
    connectWebSocket();
}

function handleTokenModalSubmit() {
    const input = document.getElementById('tokenModalInput');
    const errorEl = document.getElementById('tokenModalError');
    const token = input?.value.trim();

    if (!token) {
        if (errorEl) {
            errorEl.textContent = 'Please enter a token';
            errorEl.classList.add('active');
        }
        return;
    }

    // Verify token before saving
    verifyToken(token).then(valid => {
        if (valid) {
            setApiToken(token);
            if (errorEl) errorEl.classList.remove('active');
            proceedToApp();
        } else {
            if (errorEl) {
                errorEl.textContent = 'Invalid token. Please check and try again.';
                errorEl.classList.add('active');
            }
        }
    });
}

// Bind modal events
document.addEventListener('DOMContentLoaded', () => {
    const submitBtn = document.getElementById('tokenModalSubmit');
    const skipBtn = document.getElementById('tokenModalSkip');
    const input = document.getElementById('tokenModalInput');

    if (submitBtn) {
        submitBtn.addEventListener('click', handleTokenModalSubmit);
    }
    if (skipBtn) {
        skipBtn.addEventListener('click', () => {
            // Skip token check — will proceed with empty token (works if auth disabled)
            proceedToApp();
        });
    }
    if (input) {
        input.addEventListener('keydown', (e) => {
            if (e.key === 'Enter') handleTokenModalSubmit();
        });
    }

    // Start auth flow
    initAuth();
});

async function fetchBackends() {
    try {
        const response = await fetch(`${API_BASE_URL}/backends`, {
            headers: {
                'X-API-Token': getApiToken()
            }
        });
        
        if (response.ok) {
            const data = await response.json();
            // Support both lowercase (API) and PascalCase formats
            const backendsArray = data.backends || data.Backends || [];
            
            // Always initialize clusterState, even with empty backends
            clusterState = normalizeClusterState({
                Backends: backendsArray,
                HealthyBackends: data.healthyBackends || data.HealthyBackends ||
                                backendsArray.filter(b => (b.status || b.Status) === 'healthy').length,
                TotalBackends: data.totalBackends || data.TotalBackends || backendsArray.length
            });
            
            // Render backends (or empty state) - this will mark backendsLoaded = true
            updateDashboard(clusterState);
        } else {
            // API error - show empty state and mark as loaded to stop loading spinner
            clusterState = { Backends: [], HealthyBackends: 0, TotalBackends: 0 };
            updateDashboard(clusterState);
        }
    } catch (error) {
        console.error('Failed to fetch backends:', error);
        // On error, still mark as loaded to stop spinner
        clusterState = { Backends: [], HealthyBackends: 0, TotalBackends: 0 };
        updateDashboard(clusterState);
    }
}

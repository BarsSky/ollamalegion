// monitor-common.js — shared utilities for both monitor.html files
// Supports: ?api_base= URL param, localStorage override, same-origin fallback

function getApiBase() {
  // 1. URL parameter ?api_base=... (highest priority)
  const params = new URLSearchParams(window.location.search);
  const urlBase = params.get('api_base');
  if (urlBase) {
    return urlBase.replace(/\/$/, '');
  }
  // 2. localStorage override
  const stored = localStorage.getItem('monitorApiBase');
  if (stored) return stored.replace(/\/$/, '');
  // 3. Same-origin fallback
  return '';
}

function api(path) {
  const base = getApiBase();
  const url = base ? base + path : path;
  return fetch(url).then(r => {
    if (!r.ok) throw new Error(`${r.status} ${r.statusText}`);
    return r.json();
  });
}

function escapeHtml(s) {
  if (s === null || s === undefined) return '';
  const t = document.createTextNode(String(s));
  const span = document.createElement('span');
  span.appendChild(t);
  return span.innerHTML;
}

function formatDuration(ms) {
  if (ms < 1000) return ms + ' ms';
  if (ms < 60000) return (ms / 1000).toFixed(1) + ' s';
  const m = Math.floor(ms / 60000);
  const s = ((ms % 60000) / 1000).toFixed(0);
  return m + ' m ' + s + ' s';
}

function goToDashboard() {
  const dashboardUrl = (typeof window !== 'undefined' && window.WEBUI_CONFIG && window.WEBUI_CONFIG.dashboardUrl)
    ? window.WEBUI_CONFIG.dashboardUrl
    : '/';
  window.location.href = dashboardUrl;
}

// Expose for global use
window.getApiBase = getApiBase;
window.api = api;
window.escapeHtml = escapeHtml;
window.formatDuration = formatDuration;
window.goToDashboard = goToDashboard;
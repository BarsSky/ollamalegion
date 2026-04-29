// monitor-common.js — shared utilities for monitor.html and webui
// Supports: ?api_base= URL param, localStorage override, same-origin fallback
// Theme and i18n integration

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

// --- i18n support for monitor pages ---
// Uses the same OllamaLegionI18n if loaded from webui, or provides a simple fallback.
function t(key, lang) {
  if (typeof window !== 'undefined' && window.OllamaLegionI18n && typeof window.OllamaLegionI18n.t === 'function') {
    return window.OllamaLegionI18n.t(key, lang);
  }
  // Fallback: return the key itself
  return key;
}

// --- Theme support for monitor pages ---
// Reads theme from localStorage (set by webui) and applies 'light' class to body.
function applyMonitorTheme() {
  const saved = localStorage.getItem('ollamaLegionTheme');
  const theme = saved === 'light' ? 'light' : 'dark';
  document.documentElement.setAttribute('data-theme', theme);
  if (theme === 'light') {
    document.body.classList.add('light');
  } else {
    document.body.classList.remove('light');
  }
}

// Initialize theme on load
if (document.readyState === 'loading') {
  document.addEventListener('DOMContentLoaded', applyMonitorTheme);
} else {
  applyMonitorTheme();
}

// Expose for global use
window.monitorCommon = {
  getApiBase,
  api,
  escapeHtml,
  formatDuration,
  goToDashboard,
  t,
  applyMonitorTheme
};
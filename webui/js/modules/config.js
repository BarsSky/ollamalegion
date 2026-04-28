/**
 * Default WebUI config — will be overwritten by entrypoint for Docker,
 * or by run-webui-local.ps1 for local nginx runs.
 */
window.WEBUI_CONFIG = window.WEBUI_CONFIG || {};
Object.assign(window.WEBUI_CONFIG, {
    API_BASE: '',
    WS_URL: null,
    API_TOKEN: '',
    REFRESH_INTERVAL: 5000,
    MAX_RECONNECT_ATTEMPTS: 10,
    RECONNECT_INTERVAL_BASE: 3000
});
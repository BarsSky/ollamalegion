/**
 * Default WebUI config — will be overwritten by entrypoint for Docker,
 * or by run-webui-local.ps1 for local nginx runs.
 *
 * R222: API_TOKEN в отданную страницу больше не вшивается. Значение ниже —
 * пустое; аварийный токен вводит оператор на экране входа, он живёт в
 * sessionStorage вкладки и подставляется в WEBUI_CONFIG.API_TOKEN ниже. Та же
 * логика есть в docker/webui/entrypoint.sh (образ собирает config.js сам).
 */
window.WEBUI_CONFIG = window.WEBUI_CONFIG || {};
Object.assign(window.WEBUI_CONFIG, {
    API_BASE: '',
    WS_URL: null,
    API_TOKEN: '',
    CPPWORKER_URL: 'http://localhost:18092',
    REFRESH_INTERVAL: 5000,
    MAX_RECONNECT_ATTEMPTS: 10,
    RECONNECT_INTERVAL_BASE: 3000,
    // R232: демо-режим выключен по умолчанию. Включается ТОЛЬКО через
    // WEBUI_DEMO_MODE=1 у контейнера webui (entrypoint.sh перезаписывает этот
    // файл и пишет DEMO_MODE: true); переключателя в интерфейсе нет.
    DEMO_MODE: false
});
try {
    var olBgToken = sessionStorage.getItem('ol_break_glass_token');
    if (olBgToken) { window.WEBUI_CONFIG.API_TOKEN = olBgToken; }
} catch (e) { /* приватный режим: работаем без токена */ }

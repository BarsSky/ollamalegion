// llama-upstream-notice.js — R269 (2026-10-29): уведомление «есть более свежая
// стабильная версия llama.cpp».
//
// ЗАЧЕМ. Сборки узлов разные, и оператор узнавал об обновлениях upstream только
// вручную. Теперь балансер сравнивает сборку узла с последним стабильным
// релизом (GET /api/v1/llama-version), а панель показывает это одной строкой.
//
// ПОЧЕМУ ИМЕННО ТАК:
//  - решение «показывать или нет» вынесено в чистую функцию computeNotice:
//    её проверяет node-тест без DOM-стабов, и он не может «пройти на моках» —
//    на вход идут ровно те данные, что присылает API;
//  - уведомление НЕ показывается, когда сравнить не с чем (relation=unknown)
//    или проверка выключена: ложное «обновитесь» заставляет чинить то, что не
//    сломано;
//  - панель ничего не блокирует: нет ответа — нет уведомления.

(function () {
    'use strict';

    var NOTICE_ID = 'llamaUpstreamNotice';

    function T(key, vars) {
        if (globalThis.T) { return globalThis.T(key, vars); }
        if (globalThis.I18N && globalThis.I18N.t) { return globalThis.I18N.t(key, vars); }
        return key;
    }

    function apiBase() {
        if (globalThis.WEBUI_CONFIG && globalThis.WEBUI_CONFIG.API_BASE) { return globalThis.WEBUI_CONFIG.API_BASE; }
        return '';
    }

    function authHeaders() {
        if (globalThis.Api && globalThis.Api.getAuthHeaders) { return globalThis.Api.getAuthHeaders(); }
        return { 'Content-Type': 'application/json' };
    }

    function escapeHtml(s) {
        return String(s === undefined || s === null ? '' : s)
            .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
            .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
    }

    // computeNotice — единственное место, где решается «показывать ли».
    //
    // Возвращает {show, text, url, nodes}. Ничего не рисует (чистая функция —
    // её и проверяет тест).
    function computeNotice(data) {
        var none = { show: false, text: '', url: '', nodes: [] };
        if (!data || data.enabled !== true) { return none; }
        if (data.relation !== 'behind') { return none; }

        var latest = String(data.latestTag || data.latestStable || '').trim();
        if (!latest) { return none; }

        var nodes = (data.behindBackends || []).slice();
        var repo = String(data.repo || '').trim();
        var text = T('llamaUpstream.message', {
            latest: latest,
            nodes: nodes.length ? nodes.join(', ') : T('llamaUpstream.unknownNodes')
        });
        var url = repo ? ('https://github.com/' + repo + '/releases') : '';
        return { show: true, text: text, url: url, nodes: nodes };
    }

    // renderHtml — разметка уведомления (ссылка на релиз, если репозиторий известен).
    function renderHtml(notice) {
        if (!notice || !notice.show) { return ''; }
        var link = notice.url
            ? ' <a class="llama-upstream-link" href="' + escapeHtml(notice.url) +
              '" target="_blank" rel="noopener noreferrer">' + escapeHtml(T('llamaUpstream.open')) + '</a>'
            : '';
        return '<div class="llama-upstream-warn" id="' + NOTICE_ID + '" role="status">' +
            '<strong>' + escapeHtml(T('llamaUpstream.title')) + '</strong>: ' +
            escapeHtml(notice.text) + link + '</div>';
    }

    function insertNotice(html) {
        var doc = globalThis.document;
        if (!doc || !doc.getElementById) { return false; }
        var old = doc.getElementById(NOTICE_ID);
        if (old && old.parentNode) { old.parentNode.removeChild(old); }
        if (!html) { return true; }
        var page = doc.getElementById('backends-page');
        if (!page) { return false; }
        var holder = doc.createElement('div');
        holder.innerHTML = html;
        var node = holder.firstChild;
        if (!node) { return false; }
        page.insertBefore(node, page.firstChild);
        return true;
    }

    // refresh — запросить состояние и показать/снять уведомление.
    function refresh() {
        var base = apiBase();
        return fetch(base + '/api/v1/llama-version', { method: 'GET', headers: authHeaders() })
            .then(function (resp) {
                if (!resp.ok) { throw new Error('HTTP ' + resp.status); }
                return resp.json();
            })
            .then(function (data) {
                var html = renderHtml(computeNotice(data));
                insertNotice(html);
                return computeNotice(data);
            })
            .catch(function () {
                // Нет ответа/ошибка — уведомления нет. Молча: это подсказка, а не
                // критичный элемент, и шум в консоли оператору не помогает.
                insertNotice('');
                return null;
            });
    }

    function autoMount() {
        if (!globalThis.document || !globalThis.document.getElementById) { return; }
        if (!globalThis.document.getElementById('backends-page')) { return; }
        refresh();
    }

    if (globalThis.document) {
        if (globalThis.document.readyState === 'loading' && globalThis.document.addEventListener) {
            globalThis.document.addEventListener('DOMContentLoaded', autoMount);
        } else if (typeof setTimeout === 'function') {
            setTimeout(autoMount, 0);
        }
    }

    if (typeof window !== 'undefined') { window.LlamaUpstreamNotice = { computeNotice: computeNotice, renderHtml: renderHtml, refresh: refresh }; }
    if (typeof module !== 'undefined' && module.exports) {
        module.exports = { computeNotice: computeNotice, renderHtml: renderHtml, refresh: refresh };
    }
})();

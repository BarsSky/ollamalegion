// virtual-models-page.js — R595 (2026-11-19): страница «Виртуальные модели» внутри SPA.
//
// ЗАЧЕМ ПЕРЕНОС. До R595 CRUD виртуальных моделей жил в ОТДЕЛЬНОМ файле
// `webui/virtual-models.html` (547 строк), который открывался в новой вкладке
// (`target="_blank"`) и обрабатывался браузером, а не SPA. Из этого следовало:
//   * у страницы была СВОЯ авторизация: X-API-Token из WEBUI_CONFIG/localStorage;
//     про CSRF (R202: мутирующие запросы требуют X-CSRF-Token) она не знала вовсе,
//     поэтому Create/Delete на стенде с сессионной авторизацией отбивались;
//   * три места в коде знали про неё специальным случаем: `navModeIndependent`
//     в settings-ui.js, регулярка «внешние html» в demo-mode.js, комментарий и
//     ветка в app-listeners.js; плюс её нельзя было открыть ссылкой ?page=;
//   * у неё не было ни i18n (тексты жёстко английские), ни общего механизма
//     обновления (свой setInterval на 10 с).
//
// Теперь: обычная SPA-страница `#virtual-models-page`, ноль собственного таймера
// (провайдер DataRefresh), общий экран ошибок, i18n-ключи `vm.*` и те же заголовки
// авторизации, что у остальных мутаций (`Api.getAuthHeaders` → CSRF + break-glass
// токен).
//
// Экспорт: window.VirtualModelsPage = { mount, load, registerProvider,
//   buildPayload, rowsHtml, summary, deleteUrl }.
// Чистые функции (buildPayload/rowsHtml/summary/deleteUrl) экспортированы ради
// тестов: они не трогают DOM и сеть.
(function () {
    'use strict';

    var API_PATH = '/api/v1/virtual-models';

    function t(key, fallback, vars) {
        if (window.I18N && typeof window.I18N.t === 'function') {
            return window.I18N.t(key, vars);
        }
        return String(fallback).replace(/\{(\w+)\}/g, function (m, k) {
            return (vars && vars[k] !== undefined) ? String(vars[k]) : m;
        });
    }

    function esc(s) {
        if (s === null || s === undefined) return '';
        return String(s)
            .replace(/&/g, '&amp;')
            .replace(/</g, '&lt;')
            .replace(/>/g, '&gt;')
            .replace(/"/g, '&quot;')
            .replace(/'/g, '&#39;');
    }

    function num(n) {
        var v = Number(n);
        if (!isFinite(v)) return '0';
        try { return v.toLocaleString('ru-RU'); } catch (e) { return String(v); }
    }

    // ---- чистые функции (тестируются без DOM) ----------------------------

    // buildPayload — тело POST из полей формы.
    // Правила те же, что были в отдельной странице: обрезать пробелы, pool — по
    // строке на бэкенд, пустые строки выбросить, timeout — число (0 = по умолчанию).
    function buildPayload(fields) {
        var f = fields || {};
        var pool = String(f.backendPool === undefined || f.backendPool === null ? '' : f.backendPool)
            .split('\n')
            .map(function (s) { return s.trim(); })
            .filter(function (s) { return s.length > 0; });
        var timeout = parseInt(f.timeoutMs, 10);
        return {
            name: String(f.name || '').trim(),
            description: String(f.description || '').trim(),
            modelName: String(f.modelName || '').trim(),
            selection: String(f.selection || 'round_robin'),
            backendPool: pool,
            timeoutMs: isFinite(timeout) ? timeout : 0
        };
    }

    // deleteUrl — адрес DELETE. Имя экранируется как сегмент пути: имя вида
    // «virtual:my gpt/1» обязано дойти до сервера одним сегментом, иначе 404.
    function deleteUrl(name) {
        return API_PATH + '/' + encodeURIComponent(String(name === undefined || name === null ? '' : name));
    }

    // summary — сколько моделей и сколько всего выборов (по ним видно, живая ли
    // модель: «selections» растут при каждом запросе через виртуальное имя).
    function summary(models) {
        var list = Array.isArray(models) ? models : [];
        var selections = 0;
        list.forEach(function (m) {
            if (m && m.selections && typeof m.selections === 'object') {
                Object.keys(m.selections).forEach(function (k) {
                    var v = Number(m.selections[k]);
                    if (isFinite(v)) selections += v;
                });
            }
        });
        return { count: list.length, selections: selections };
    }

    // rowsHtml — строки таблицы. Экранируется ВСЁ, что пришло с сервера: имена
    // моделей и описания задаёт оператор, а подписи backends приходят из его же
    // пула — это не «доверенный HTML».
    function rowsHtml(models) {
        var list = Array.isArray(models) ? models : [];
        if (!list.length) {
            return '<div class="empty-state">' + esc(t('vm.empty', 'Пока нет ни одной виртуальной модели')) +
                '. ' + esc(t('vm.empty_hint', 'Создайте её в форме выше')) + '.</div>';
        }
        var rows = list.map(function (m) {
            var mode = String((m && m.mode) || 'unknown');
            var selection = String((m && m.selection) || 'round_robin');
            var backends = (Array.isArray(m && m.backendPool) ? m.backendPool : []).map(function (b) {
                return '<span class="backend-chip">' + esc(b) + '</span>';
            }).join(' ');
            var stats = (m && m.selections && typeof m.selections === 'object')
                ? Object.keys(m.selections).map(function (k) { return esc(k) + ': ' + num(m.selections[k]); }).join(', ')
                : esc(t('vm.no_calls', 'вызовов не было'));
            return '<tr data-vm="' + esc(m && m.name) + '">' +
                '<td><strong>' + esc(m && m.name) + '</strong>' +
                (m && m.description ? '<br><span class="vm-desc">' + esc(m.description) + '</span>' : '') + '</td>' +
                '<td><span class="badge">' + esc(mode) + '</span></td>' +
                '<td>' + esc((m && m.modelName) || '—') + '</td>' +
                '<td><span class="badge">' + esc(selection) + '</span></td>' +
                '<td>' + (backends || '—') + '</td>' +
                '<td class="vm-stats">' + stats + '</td>' +
                '<td class="col-right">' +
                '<button class="btn btn-sm btn-danger vm-delete" data-vm="' + esc(m && m.name) + '" ' +
                'title="' + esc(t('vm.delete', 'Удалить')) + '">' + esc(t('vm.delete', 'Удалить')) + '</button>' +
                '</td></tr>';
        }).join('');
        return '<table class="vm-table"><thead><tr>' +
            '<th>' + esc(t('vm.col.name', 'Имя')) + '</th>' +
            '<th>' + esc(t('vm.col.mode', 'Режим')) + '</th>' +
            '<th>' + esc(t('vm.col.model', 'Модель')) + '</th>' +
            '<th>' + esc(t('vm.col.selection', 'Выбор бэкенда')) + '</th>' +
            '<th>' + esc(t('vm.col.backends', 'Пул бэкендов')) + '</th>' +
            '<th>' + esc(t('vm.col.stats', 'Статистика')) + '</th>' +
            '<th class="col-right">' + esc(t('vm.col.actions', 'Действия')) + '</th>' +
            '</tr></thead><tbody>' + rows + '</tbody></table>';
    }

    // ---- сеть ------------------------------------------------------------

    // authHeaders — ОДИН источник заголовков авторизации для мутаций (R202: CSRF
    // обязателен, break-glass токен — если задан). Отдельная страница знала только
    // про X-API-Token, поэтому Create/Delete на стенде с сессией отбивались.
    function authHeaders(extra) {
        if (window.Api && typeof window.Api.getAuthHeaders === 'function') {
            var h = window.Api.getAuthHeaders();
            if (extra) { Object.keys(extra).forEach(function (k) { h[k] = extra[k]; }); }
            return h;
        }
        var fallback = { 'Content-Type': 'application/json' };
        if (extra) { Object.keys(extra).forEach(function (k) { fallback[k] = extra[k]; }); }
        return fallback;
    }

    function apiFetch(url, options) {
        var opts = options || {};
        opts.headers = authHeaders(opts.body ? { 'Content-Type': 'application/json' } : null);
        return fetch(url, opts);
    }

    // readError — текст ошибки сервера. Сервер отвечает JSON `{error}`, но на 401
    // nginx может отдать HTML — тогда показываем статус, а не «[object Object]».
    function readError(resp) {
        return resp.text().then(function (txt) {
            var msg = txt;
            try {
                var parsed = JSON.parse(txt);
                if (parsed && parsed.error) msg = parsed.error;
            } catch (e) { /* не JSON — оставляем как есть */ }
            return 'HTTP ' + resp.status + (msg ? ': ' + msg : '');
        });
    }

    // ---- состояние и отрисовка ------------------------------------------

    var state = { models: [], enabled: true, loaded: false };

    function byId(id) { return document.getElementById(id); }

    function setError(msg) {
        var el = byId('vm-error');
        if (!el) return;
        if (!msg) { el.style.display = 'none'; el.textContent = ''; return; }
        el.textContent = msg;
        el.style.display = 'block';
    }

    function render() {
        var disabled = byId('vm-disabled');
        var content = byId('vm-content');
        if (disabled) disabled.style.display = state.enabled ? 'none' : 'block';
        if (content) content.style.display = state.enabled ? 'block' : 'none';
        if (!state.enabled) return;

        var s = summary(state.models);
        var countEl = byId('vm-count');
        if (countEl) countEl.textContent = num(s.count);
        var selEl = byId('vm-selections');
        if (selEl) selEl.textContent = num(s.selections);

        var host = byId('vm-models');
        if (host) host.innerHTML = rowsHtml(state.models);
    }

    // load — единственный источник данных страницы; регистрируется провайдером
    // DataRefresh, поэтому и кнопка «Обновить» в шапке, и авто-тик обновляют её.
    function load() {
        return apiFetch(API_PATH, { method: 'GET' })
            .then(function (resp) {
                if (!resp.ok) return readError(resp).then(function (msg) { throw new Error(msg); });
                return resp.json();
            })
            .then(function (data) {
                state.enabled = !!data.enabled;
                state.models = Array.isArray(data.models) ? data.models : [];
                state.loaded = true;
                setError('');
                render();
                return data;
            })
            .catch(function (err) {
                setError(t('vm.error_load', 'Не удалось загрузить виртуальные модели') + ': ' + err.message);
                throw err;
            });
    }

    function onCreate(ev) {
        if (ev && typeof ev.preventDefault === 'function') ev.preventDefault();
        var payload = buildPayload({
            name: (byId('vm-name') || {}).value,
            description: (byId('vm-description') || {}).value,
            modelName: (byId('vm-model') || {}).value,
            selection: (byId('vm-selection') || {}).value,
            backendPool: (byId('vm-backends') || {}).value,
            timeoutMs: (byId('vm-timeout') || {}).value
        });
        return apiFetch(API_PATH, { method: 'POST', body: JSON.stringify(payload) })
            .then(function (resp) {
                if (!resp.ok) return readError(resp).then(function (msg) { throw new Error(msg); });
                var form = byId('vm-form');
                if (form && typeof form.reset === 'function') form.reset();
                return load();
            })
            .catch(function (err) {
                setError(t('vm.error_create', 'Не удалось создать виртуальную модель') + ': ' + err.message);
            });
    }

    function onDelete(name) {
        if (!name) return Promise.resolve();
        if (typeof window.confirm === 'function' &&
            !window.confirm(t('vm.confirm_delete', 'Удалить виртуальную модель «{name}»? Это необратимо.', { name: name }))) {
            return Promise.resolve();
        }
        return apiFetch(deleteUrl(name), { method: 'DELETE' })
            .then(function (resp) {
                if (!resp.ok) return readError(resp).then(function (msg) { throw new Error(msg); });
                return load();
            })
            .catch(function (err) {
                setError(t('vm.error_delete', 'Не удалось удалить виртуальную модель') + ': ' + err.message);
            });
    }

    // ---- монтирование ----------------------------------------------------

    var mounted = false;
    var registered = false;

    function mount() {
        if (mounted) return false;
        var form = byId('vm-form');
        if (form) {
            form.addEventListener('submit', onCreate);
        }
        var host = byId('vm-models');
        if (host) {
            // Делегированный обработчик: кнопки «Удалить» пересоздаются при каждом
            // обновлении таблицы, поэтому слушатель — один на контейнер.
            host.addEventListener('click', function (ev) {
                var target = ev.target;
                if (!target || !target.classList || !target.classList.contains('vm-delete')) return;
                onDelete(target.getAttribute('data-vm'));
            });
        }
        mounted = true;
        return true;
    }

    function registerProvider() {
        if (registered) return false;
        if (window.DataRefresh && typeof window.DataRefresh.register === 'function') {
            window.DataRefresh.register('virtual-models', function () { return load(); });
        }
        registered = true;
        return true;
    }

    window.VirtualModelsPage = {
        mount: mount,
        load: load,
        registerProvider: registerProvider,
        // чистые функции — для тестов
        buildPayload: buildPayload,
        rowsHtml: rowsHtml,
        summary: summary,
        deleteUrl: deleteUrl,
        _state: state,
        _apiPath: API_PATH
    };
})();

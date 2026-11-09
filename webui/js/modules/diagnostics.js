// diagnostics.js — R476 (2026-11-07): страница «Диагностика» в WebUI.
//
// ЗАЧЕМ. Чтобы разобрать живой дефект, оператору нужно было собрать данные из
// пяти мест (версии, окно, профили, флаги, бэкенды) и пересказать их в чат. Это
// занимало время, часть значений терялась, и разбор шёл по догадке. Здесь всё
// это на одной странице: блоки с кнопкой «копировать», значение — по клику,
// плюс кнопка скачивания архива (GET /api/v1/diag/bundle).
//
// МОДУЛЬ НЕ ИМЕЕТ СВОЕГО СОСТОЯНИЯ В ОБЩЕМ App: данные живут в замыкании,
// перерисовка идёт по кнопке «Обновить» и при входе на страницу.
//
// Экспортируется как window.Diagnostics (см. scripts/check_iife_exports.py).
const Diagnostics = (function () {
    'use strict';

    var lastSummary = null;
    var lastError = '';
    var loading = false;

    function t(key, fallback) {
        // I18N.t без ключа возвращает сам ключ — нам нужен осмысленный fallback.
        if (window.I18N && typeof window.I18N.t === 'function') {
            var value = window.I18N.t(key);
            if (value && value !== key) return value;
        }
        return fallback || key;
    }

    function esc(value) {
        if (value === null || value === undefined) return '';
        var s = String(value);
        if (window.Utils && typeof window.Utils.escapeHtml === 'function') {
            return window.Utils.escapeHtml(s);
        }
        return s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
            .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
    }

    function setStatus(text, kind) {
        var el = document.getElementById('diagStatus');
        if (!el) return;
        el.textContent = text || '';
        el.className = 'diag-status' + (kind ? ' diag-status-' + kind : '');
    }

    function val(value) {
        if (value === null || value === undefined) return '';
        if (typeof value === 'object') {
            try { return JSON.stringify(value); } catch (e) { return String(value); }
        }
        return String(value);
    }

    // copyValue — копирование в буфер. navigator.clipboard требует secure
    // context; на http:// стенде он есть (localhost/флаг), но падать не должны —
    // поэтому fallback на старый execCommand и честный статус на экране.
    function copyValue(text, btn) {
        var done = function () {
            setStatus(t('diag.copied', 'скопировано'), 'ok');
            if (btn) {
                var prev = btn.getAttribute('title') || '';
                btn.setAttribute('title', t('diag.copied', 'скопировано'));
                setTimeout(function () { btn.setAttribute('title', prev); }, 1500);
            }
        };
        var fail = function (err) {
            setStatus(t('diag.copy_failed', 'не удалось скопировать') + ': ' + (err && err.message ? err.message : err), 'error');
        };
        if (navigator.clipboard && typeof navigator.clipboard.writeText === 'function') {
            navigator.clipboard.writeText(text).then(done).catch(fail);
            return;
        }
        try {
            var area = document.createElement('textarea');
            area.value = text;
            area.setAttribute('readonly', 'readonly');
            area.style.position = 'fixed';
            area.style.opacity = '0';
            document.body.appendChild(area);
            area.select();
            var ok = document.execCommand && document.execCommand('copy');
            area.remove();
            if (ok) { done(); } else { fail(new Error('execCommand copy')); }
        } catch (e) {
            fail(e);
        }
    }

    // windowCeiling — текст про формулу потолка одинаков для страницы и копии.
    function windowSourceText() {
        return t('diag.window_source',
            'Потолок считается той же формулой, что и в запросах: min(GGUF, contextLengthMax, LB_NCTX_RELOAD_MAX_N_CTX).');
    }

    // renderVersions — блок «Версии и сборка».
    function renderVersions(summary) {
        var version = (summary && summary.version) || {};
        var rows = [];
        function add(label, value) {
            if (value === undefined || value === null || value === '') return;
            rows.push([label, val(value)]);
        }
        add('build_version', version.build_version);
        add('build_commit', version.build_commit);
        add('build_date', version.build_date);
        add('version', version.version || summary.version_text);
        add(t('diag.loaded_at', 'Снято'), summary.generated_at);
        return table(rows);
    }

    // renderWindow — потолок + профили моделей с действующим окном.
    function renderWindow(summary) {
        var rows = [];
        var ceiling = summary && summary.nctx_ceiling;
        if (ceiling && typeof ceiling === 'object') {
            // Балансер отдаёт потолок структурой (число + чем ограничен):
            // показываем число с пометкой источника, причину — отдельной строкой,
            // иначе оператор видит «[object Object]» вместо числа.
            var ceilValue = ceiling.envCeiling || ceiling.ceiling || ceiling.effectiveCeiling;
            if (ceilValue !== undefined) {
                rows.push([t('diag.col_ceiling', 'потолок окна'),
                    val(ceilValue) + (ceiling.limitedBy ? ' (' + val(ceiling.limitedBy) + ')' : '')]);
            }
            if (ceiling.reason) rows.push([t('diag.col_ceiling_reason', 'чем ограничен'), val(ceiling.reason)]);
        } else if (ceiling) {
            rows.push([t('diag.col_ceiling', 'потолок окна'), val(ceiling)]);
        }
        var reload = (summary && summary.nctx_reload) || {};
        var keys = ['auto_reload_n_ctx', 'auto_reload_max_n_ctx', 'preflight_enabled',
            'preflight_async_reload', 'preflight_async_retry_after_sec'];
        for (var i = 0; i < keys.length; i++) {
            if (reload[keys[i]] !== undefined) rows.push([keys[i], val(reload[keys[i]])]);
        }
        var html = table(rows);

        var profiles = (summary && summary.model_profiles) || {};
        var names = Object.keys(profiles).sort();
        if (names.length === 0) {
            return html + '<div class="diag-empty">' + esc(t('diag.empty', 'нет данных')) + '</div>';
        }
        var head = '<tr><th>' + esc(t('diag.col_model', 'Модель')) + '</th><th>' +
            esc(t('diag.col_context', 'contextLength')) + '</th><th>' +
            esc(t('diag.col_context_max', 'contextLengthMax')) + '</th><th>' +
            esc(t('diag.col_parallel', 'parallel')) + '</th><th>' +
            esc(t('diag.col_effective', 'действующее окно')) + '</th></tr>';
        var body = '';
        for (var j = 0; j < names.length; j++) {
            var name = names[j];
            var entry = profiles[name] || {};
            var profile = entry.profile || {};
            var effective = entry.effective || {};
            body += '<tr>' +
                '<td><code>' + esc(name) + '</code></td>' +
                '<td>' + esc(val(profile.contextLength)) + '</td>' +
                '<td>' + esc(val(profile.contextLengthMax)) + '</td>' +
                '<td>' + esc(val(profile.parallel)) + '</td>' +
                '<td>' + esc(val(effective.contextSize || effective.perClient || effective)) + '</td>' +
                '</tr>';
        }
        return html + '<table class="diag-table"><thead>' + head + '</thead><tbody>' + body + '</tbody></table>' +
            '<div class="diag-note">' + esc(windowSourceText()) + '</div>';
    }

    // renderFlags — заданные и незаданные флаги. Разделение важнее самих
    // значений: «unset» значит «действует дефолт кода», а не «пустая строка».
    function renderFlags(summary) {
        var flags = (summary && summary.flags) || {};
        var set = flags.set || {};
        var unset = flags.unset || [];
        var setKeys = Object.keys(set).sort();
        var html = '<h4>' + esc(t('diag.flags_set', 'Заданы')) + '</h4>';
        if (setKeys.length === 0) {
            html += '<div class="diag-empty">' + esc(t('diag.empty', 'нет данных')) + '</div>';
        } else {
            var rows = [];
            for (var i = 0; i < setKeys.length; i++) rows.push([setKeys[i], set[setKeys[i]]]);
            html += table(rows);
        }
        html += '<h4>' + esc(t('diag.flags_unset', 'Не заданы (действует значение по умолчанию из кода)')) + '</h4>';
        if (unset.length === 0) {
            html += '<div class="diag-empty">' + esc(t('diag.empty', 'нет данных')) + '</div>';
        } else {
            html += '<div class="diag-chiplist">';
            for (var j = 0; j < unset.length; j++) {
                html += '<code class="diag-chip">' + esc(unset[j]) + '</code>';
            }
            html += '</div>';
        }
        html += '<div class="diag-note">' + esc(t('diag.flags_note',
            'unset — это не пустая переменная, а её отсутствие: значение придёт из дефолта кода.')) + '</div>';
        return html;
    }

    // renderBackends — бэкенды: статус, слоты, загруженные модели с окном.
    function renderBackends(summary) {
        var cluster = (summary && summary.cluster) || {};
        var backends = cluster.backends || [];
        if (!backends.length) {
            return '<div class="diag-empty">' + esc(t('diag.empty', 'нет данных')) + '</div>';
        }
        var head = '<tr><th>' + esc(t('diag.col_backend', 'Бэкенд')) + '</th><th>' +
            esc(t('diag.col_type', 'Тип')) + '</th><th>' +
            esc(t('diag.col_status', 'Статус')) + '</th><th>' +
            esc(t('diag.col_slots', 'Слоты (занято/всего)')) + '</th><th>' +
            esc(t('diag.col_models', 'Загруженные модели')) + '</th></tr>';
        var body = '';
        for (var i = 0; i < backends.length; i++) {
            var b = backends[i] || {};
            var models = b.models || b.loadedModels || [];
            var modelText = '';
            if (Array.isArray(models)) {
                modelText = models.map(function (m) {
                    if (typeof m === 'string') return m;
                    var name = m.name || m.model || '';
                    var ctx = (m.contextSize || m.context_size || m.nCtx || '');
                    return ctx ? (name + ' (' + ctx + ')') : name;
                }).filter(function (s) { return s; }).join(', ');
            } else {
                modelText = val(models);
            }
            var used = b.activeRequests;
            var total = b.maxConcurrentRequests;
            body += '<tr>' +
                '<td><code>' + esc(b.id || '') + '</code></td>' +
                '<td>' + esc(b.type || b.backendType || '') + '</td>' +
                '<td>' + esc(b.status || '') + '</td>' +
                '<td>' + esc(val(used)) + ' / ' + esc(val(total)) + '</td>' +
                '<td>' + esc(modelText) + '</td>' +
                '</tr>';
        }
        return '<table class="diag-table"><thead>' + head + '</thead><tbody>' + body + '</tbody></table>';
    }

    // table — пары ключ/значение; значение кликабельно (копируется одно поле).
    function table(rows) {
        if (!rows || rows.length === 0) {
            return '<div class="diag-empty">' + esc(t('diag.empty', 'нет данных')) + '</div>';
        }
        var html = '<table class="diag-table diag-table-kv"><tbody>';
        for (var i = 0; i < rows.length; i++) {
            var key = rows[i][0];
            var value = rows[i][1];
            html += '<tr><th>' + esc(key) + '</th>' +
                '<td><code class="diag-value" data-diag-value="' + esc(value) + '" title="' +
                esc(t('diag.copy_section', 'Копировать блок')) + '">' + esc(value) + '</code></td></tr>';
        }
        return html + '</tbody></table>';
    }

    // sectionText — плоский текст блока для «копировать блок».
    function sectionText(name, summary) {
        if (!summary) return '';
        if (name === 'versions') {
            return t('diag.section_versions', 'Версии и сборка') + '\n' + JSON.stringify({
                version: summary.version, version_text: summary.version_text, generated_at: summary.generated_at
            }, null, 2);
        }
        if (name === 'window') {
            return t('diag.section_window', 'Окно контекста и профили моделей') + '\n' + JSON.stringify({
                nctx_ceiling: summary.nctx_ceiling, nctx_reload: summary.nctx_reload,
                model_profiles: summary.model_profiles
            }, null, 2);
        }
        if (name === 'flags') {
            return t('diag.section_flags', 'Флаги стенда') + '\n' + JSON.stringify(summary.flags, null, 2);
        }
        if (name === 'backends') {
            return t('diag.section_backends', 'Бэкенды') + '\n' + JSON.stringify({
                cluster: summary.cluster, queue_stats: summary.queue_stats
            }, null, 2);
        }
        return JSON.stringify(summary, null, 2);
    }

    function render() {
        if (lastError) {
            setStatus(t('diag.error', 'Не удалось получить диагностику') + ': ' + lastError, 'error');
        }
        var versions = document.getElementById('diagVersions');
        if (versions) versions.innerHTML = renderVersions(lastSummary);
        var windowEl = document.getElementById('diagWindow');
        if (windowEl) windowEl.innerHTML = renderWindow(lastSummary);
        var flags = document.getElementById('diagFlags');
        if (flags) flags.innerHTML = renderFlags(lastSummary);
        var backends = document.getElementById('diagBackends');
        if (backends) backends.innerHTML = renderBackends(lastSummary);
        var stamp = document.getElementById('diagLoadedAt');
        if (stamp) {
            stamp.textContent = lastSummary && lastSummary.generated_at
                ? t('diag.loaded_at', 'Снято') + ': ' + lastSummary.generated_at : '';
        }
    }

    async function load() {
        if (loading) return;
        loading = true;
        lastError = '';
        setStatus(t('diag.loading', 'Загружаю данные…'));
        try {
            if (!window.Api || typeof window.Api.diagSummary !== 'function') {
                throw new Error('Api.diagSummary недоступен');
            }
            lastSummary = await window.Api.diagSummary();
            render();
            setStatus('');
        } catch (e) {
            lastError = (e && e.message) ? e.message : String(e);
            render();
        } finally {
            loading = false;
        }
    }

    async function downloadBundle(btn) {
        if (!window.Api || typeof window.Api.downloadDiagBundle !== 'function') {
            setStatus('Api.downloadDiagBundle недоступен', 'error');
            return;
        }
        if (btn) btn.disabled = true;
        setStatus(t('diag.downloading', 'Собираю архив…'));
        try {
            var res = await window.Api.downloadDiagBundle();
            setStatus(t('diag.downloaded', 'архив скачан') + ': ' + val(res.filename) + ' (' + val(res.bytes) + ' B)', 'ok');
        } catch (e) {
            setStatus(t('diag.error', 'Не удалось получить диагностику') + ': ' + ((e && e.message) || e), 'error');
        } finally {
            if (btn) btn.disabled = false;
        }
    }

    function mount() {
        var refresh = document.getElementById('diagRefresh');
        if (refresh && !refresh.dataset.bound) {
            refresh.dataset.bound = '1';
            refresh.addEventListener('click', function () { load(); });
        }
        var copyAll = document.getElementById('diagCopyAll');
        if (copyAll && !copyAll.dataset.bound) {
            copyAll.dataset.bound = '1';
            copyAll.addEventListener('click', function () {
                copyValue(JSON.stringify(lastSummary || {}, null, 2), copyAll);
            });
        }
        var download = document.getElementById('diagDownloadBundle');
        if (download && !download.dataset.bound) {
            download.dataset.bound = '1';
            download.addEventListener('click', function () { downloadBundle(download); });
        }
        // Делегирование: копирование блока и одиночного значения.
        var page = document.getElementById('diagnostics-page');
        if (page && !page.dataset.bound) {
            page.dataset.bound = '1';
            page.addEventListener('click', function (ev) {
                var sectionBtn = ev.target.closest ? ev.target.closest('[data-diag-copy]') : null;
                if (sectionBtn) {
                    copyValue(sectionText(sectionBtn.getAttribute('data-diag-copy'), lastSummary), sectionBtn);
                    return;
                }
                var valueEl = ev.target.closest ? ev.target.closest('.diag-value') : null;
                if (valueEl) {
                    copyValue(valueEl.getAttribute('data-diag-value') || '', valueEl);
                }
            });
        }
        load();
    }

    return {
        mount: mount,
        load: load,
        downloadBundle: downloadBundle,
        // Для тестов/отладки: отрендерить конкретную сводку без сети.
        _renderWith: function (summary) { lastSummary = summary; lastError = ''; render(); },
        _sectionText: function (name) { return sectionText(name, lastSummary); }
    };
})();

if (typeof window !== 'undefined') {
    window.Diagnostics = Diagnostics;
}

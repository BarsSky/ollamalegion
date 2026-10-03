/**
 * client-access.js — «Подключение клиентов» рядом с бэкендом (2026-10-03).
 *
 * ЗАЧЕМ. Клиенты (SillyTavern, Open WebUI, LibreChat, n8n, OpenAI SDK) обычно
 * требуют заполнить поле «API key», даже когда сервер его не проверяет. Оператор
 * при этом не знал, какое значение вписывать, и искал токен в `deployments/.env`
 * на хосте. Модуль показывает рядом с бэкендом:
 *   - сам ключ (маскирован, с кнопками «показать» и «скопировать») — это тот же
 *     токен, что WebUI уже использует для админки (`X-API-Token`);
 *   - эндпоинты ИМЕННО этого типа бэкенда (llama.cpp/Ollama — текст, image_cpp —
 *     картинки) в виде, который вставляется в конфиг клиента;
 *   - готовый curl и честную пометку, где ключ проверяется, а где нет.
 *
 * БЕЗОПАСНОСТЬ. Токен и так отдаётся браузеру в `js/modules/config.js`
 * (`window.WEBUI_CONFIG.API_TOKEN`), поэтому его показ в UI не расширяет доступ:
 * кто может открыть WebUI, тот может прочитать config.js. Маскируем по
 * умолчанию, чтобы ключ не попадал в скриншоты и записи экрана случайно.
 *
 * Экспорт: window.ClientAccess.
 */
(function () {
    'use strict';

    var KEY_MASK = '••••••••••••';

    function esc(s) {
        if (window.Utils && typeof window.Utils.escapeHtml === 'function') {
            return window.Utils.escapeHtml(s);
        }
        if (s === null || s === undefined) return '';
        return String(s)
            .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
            .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
    }

    function t(key, fallback) {
        if (window.I18N && typeof window.I18N.t === 'function') {
            var v = window.I18N.t(key);
            if (v && v !== key) return v;
        }
        return fallback || key;
    }

    /** Ключ клиента: тот же, что WebUI отправляет в X-API-Token. */
    function clientKey() {
        var cfg = window.WEBUI_CONFIG || {};
        var key = cfg.API_TOKEN || '';
        if (!key) {
            try { key = localStorage.getItem('apiToken') || ''; } catch (e) { key = ''; }
        }
        return String(key);
    }

    /** Тип бэкенда в терминах проекта: llama_cpp | image_cpp | ollama. */
    function backendKind(backend) {
        backend = backend || {};
        // Канонический источник — общая утилита WebUI: страницы бэкендов и GGUF
        // определяют тип ей же, и второй независимой эвристики быть не должно.
        if (window.Utils && typeof window.Utils.getBackendType === 'function') {
            var viaUtils = String(window.Utils.getBackendType(backend) || '').toLowerCase();
            if (viaUtils) return viaUtils;
        }
        var raw = String(
            backend.backendType || backend.backend_type || backend.type || backend.engine ||
            (backend.ollama ? 'ollama' : '') ||
            (backend.ollamaPort ? 'ollama' : '') || ''
        ).toLowerCase();
        if (raw === 'image_cpp' || raw === 'sd_cpp' || raw === 'sdcpp' || raw === 'image') return 'image_cpp';
        if (raw === 'llama_cpp' || raw === 'llamacpp' || raw === 'cppworker') return 'llama_cpp';
        if (raw === 'ollama') return 'ollama';
        return raw || 'unknown';
    }

    /**
     * Хост для клиентских URL.
     *
     * ПОЧЕМУ НЕ backend.host: в docker-стенде это имя сервиса (`imageworker`,
     * `cppworker-gpu`), которое резолвится только внутри compose-сети — оператор,
     * вставив такой адрес в SillyTavern на своей машине, получил бы «хост не
     * найден». Клиент должен ходить на тот же адрес, по которому открыт WebUI,
     * поэтому приоритет: явный PUBLIC_HOST из конфига → hostname браузера →
     * backend.host (последний шанс, когда страница открыта не из браузера).
     * Внутренний адрес воркера показываем отдельной строкой с пометкой.
     */
    function backendHost(backend) {
        var cfg = window.WEBUI_CONFIG || {};
        var explicit = String(cfg.PUBLIC_HOST || cfg.LB_PUBLIC_HOST || '').trim();
        if (explicit) return explicit;
        if (window.location && window.location.hostname) return window.location.hostname;
        backend = backend || {};
        return String(backend.host || '').trim() || 'localhost';
    }

    /** Внутренний адрес воркера (только внутри кластера) — host:порт воркера. */
    function internalAddress(backend) {
        backend = backend || {};
        var host = String(backend.host || '').trim();
        if (!host) return '';
        var port = backendPort(backend, ['imagePort', 'image_port', 'cppWorkerPort', 'cpp_worker_port', 'ollamaPort']);
        return port > 0 ? host + ':' + port : host;
    }

    /** Порт из поля бэкенда (у image_cpp — imagePort, у остальных — cppWorkerPort/ollamaPort). */
    function backendPort(backend, names) {
        backend = backend || {};
        names = names || [];
        for (var i = 0; i < names.length; i++) {
            var v = backend[names[i]];
            if (v !== undefined && v !== null && String(v) !== '' && String(v) !== '0') return parseInt(v, 10) || 0;
        }
        return 0;
    }

    /**
     * endpoints — что и куда вписывать клиенту этого бэкенда.
     *
     * Порты берём из README/доков проекта: клиентские поверхности живут на
     * балансере (18080 — текст и Ollama-совместимая, 18079 — OpenAI-поверхность
     * изображений), а не на порту самого воркера: воркер слушает свой порт
     * (18092/18093) и в клиентских конфигах не используется.
     */
    function endpoints(backend, opts) {
        opts = opts || {};
        var kind = backendKind(backend);
        var host = opts.host || backendHost(backend);
        var proxyPort = opts.proxyPort || 18080;
        var openaiPort = opts.openaiPort || 18079;
        var ollamaPort = opts.ollamaPort || 11434;
        var base = 'http://' + host;
        if (kind === 'image_cpp') {
            return {
                kind: kind,
                rows: [
                    { label: t('clientAccess.ep_openai_images', 'OpenAI-совместимая генерация изображений'),
                      method: 'POST', url: base + ':' + openaiPort + '/v1/images/generations' },
                    { label: t('clientAccess.ep_a1111', 'A1111 (SillyTavern, LibreChat)'),
                      method: 'POST', url: base + ':' + openaiPort + '/sdapi/v1/txt2img' },
                    { label: t('clientAccess.ep_openai_models', 'Список моделей (OpenAI)'),
                      method: 'GET', url: base + ':' + openaiPort + '/v1/models' },
                    { label: t('clientAccess.ep_worker_api', 'API воркера (nginx/балансер, нужен ключ)'),
                      method: '', url: base + ':' + proxyPort + '/api/v1/image/backends/' + encodeURIComponent(backend && backend.id || '<backendId>') + '/...' }
                ],
                // На клиентских поверхностях картинок ключ не проверяется — но поле
                // «API key» в клиентах обычно обязательное, поэтому даём значение.
                keyNote: t('clientAccess.key_note_optional',
                    'На /v1/images/* и /sdapi/v1/* ключ не проверяется: клиент требует непустое значение — вставьте показанный ключ (или любое).')
            };
        }
        if (kind === 'ollama') {
            return {
                kind: kind,
                rows: [
                    { label: t('clientAccess.ep_ollama_chat', 'Ollama API (чат)'), method: 'POST', url: base + ':' + ollamaPort + '/api/chat' },
                    { label: t('clientAccess.ep_ollama_tags', 'Ollama API (список моделей)'), method: 'GET', url: base + ':' + ollamaPort + '/api/tags' },
                    { label: t('clientAccess.ep_openai_chat', 'OpenAI-совместимый чат (через балансер)'), method: 'POST', url: base + ':' + proxyPort + '/v1/chat/completions' }
                ],
                keyNote: t('clientAccess.key_note_text',
                    'Для текстовых клиентов ключ можно указать любым, если в конфиге балансера не включена проверка клиентского ключа; админские /api/v1/* требуют именно этот ключ.')
            };
        }
        return {
            kind: kind,
            rows: [
                { label: t('clientAccess.ep_llama_chat', 'OpenAI-совместимый чат (Open WebUI, Cline, Cursor)'), method: 'POST', url: base + ':' + proxyPort + '/v1/chat/completions' },
                { label: t('clientAccess.ep_llama_models', 'Список моделей (OpenAI)'), method: 'GET', url: base + ':' + proxyPort + '/v1/models' },
                { label: t('clientAccess.ep_ollama_chat_lb', 'Ollama-совместимая поверхность'), method: 'POST', url: base + ':' + proxyPort + '/api/chat' }
            ],
            keyNote: t('clientAccess.key_note_text',
                'Для текстовых клиентов ключ можно указать любым, если в конфиге балансера не включена проверка клиентского ключа; админские /api/v1/* требуют именно этот ключ.')
        };
    }

    /** curl-пример под тип бэкенда (копируется целиком). */
    function curlExample(backend, opts) {
        var ep = endpoints(backend, opts);
        var key = clientKey() || '<ключ>';
        if (ep.kind === 'image_cpp') {
            return 'curl -sS -X POST ' + baseOf(ep, '/v1/images/generations') + ' \\\n' +
                '  -H \'Content-Type: application/json\' \\\n' +
                '  -H \'Authorization: Bearer ' + key + '\' \\\n' +
                '  -d \'{"prompt":"a cat","size":"512x512","steps":8,"n":1}\'';
        }
        return 'curl -sS -X POST ' + baseOf(ep, '/v1/chat/completions') + ' \\\n' +
            '  -H \'Content-Type: application/json\' \\\n' +
            '  -H \'Authorization: Bearer ' + key + '\' \\\n' +
            '  -d \'{"model":"<model>","messages":[{"role":"user","content":"hi"}]}\'';
    }

    /** Достаём URL нужного эндпоинта из списка rows (для сборки curl). */
    function baseOf(ep, suffix) {
        var rows = (ep && ep.rows) || [];
        for (var i = 0; i < rows.length; i++) {
            if (rows[i].url && rows[i].url.indexOf(suffix) !== -1) return rows[i].url;
        }
        return 'http://<хост>:18080' + suffix;
    }

    /**
     * render — HTML-блок в стиле `.be-detail-section` (как остальные секции
     * раскрытой строки бэкенда на странице «Бэкенды»).
     *
     * Ключ маскирован; кнопки работают через делегированный обработчик mount().
     */
    function render(backend, opts) {
        var key = clientKey();
        var ep = endpoints(backend, opts);
        var rows = ep.rows.map(function (r) {
            return '<div class="be-param-item" style="grid-column:1/-1;">' +
                '<span class="be-param-label">' + esc(r.label) + '</span>' +
                '<span class="be-param-value"><code>' + (r.method ? esc(r.method) + ' ' : '') + esc(r.url) + '</code></span>' +
                '</div>';
        }).join('');

        var keyHtml;
        if (!key) {
            keyHtml = '<span class="be-param-value" style="color:var(--text-secondary)">' +
                esc(t('clientAccess.key_absent', 'Ключ не задан в конфиге WebUI (auth выключен)')) + '</span>';
        } else {
            keyHtml =
                '<span class="be-param-value">' +
                    '<code data-client-key-value="1" data-key="' + esc(key) + '" data-masked="1">' + esc(KEY_MASK) + '</code>' +
                    ' <button class="btn btn-secondary btn-sm" data-client-access="toggle-key">' +
                        esc(t('clientAccess.show', 'Показать')) + '</button>' +
                    ' <button class="btn btn-secondary btn-sm" data-client-access="copy-key">' +
                        esc(t('clientAccess.copy', 'Копировать')) + '</button>' +
                '</span>';
        }

        // Внутренний адрес воркера показываем отдельной строкой: в docker-стенде
        // backend.host — это имя сервиса (`imageworker`), которое вне compose-сети
        // не резолвится, и в клиентский конфиг его вставлять нельзя.
        var internal = internalAddress(backend);
        var internalRow = (internal && internal !== endpoints(backend, opts).host)
            ? '<div class="be-param-item" style="grid-column:1/-1;">' +
                  '<span class="be-param-label">' + esc(t('clientAccess.internal_label', 'Внутренний адрес воркера (только внутри кластера)')) + '</span>' +
                  '<span class="be-param-value"><code>' + esc(internal) + '</code></span>' +
              '</div>'
            : '';

        return '<div class="be-detail-section" data-client-access-block="1">' +
            '<div class="be-detail-title">🔑 ' + esc(t('clientAccess.title', 'Подключение клиентов')) + '</div>' +
            '<div class="be-params-grid">' +
                '<div class="be-param-item" style="grid-column:1/-1;">' +
                    '<span class="be-param-label">' + esc(t('clientAccess.key_label', 'Ключ (X-API-Token / Bearer)')) + '</span>' +
                    keyHtml +
                '</div>' +
                rows +
                internalRow +
            '</div>' +
            '<div style="margin-top:8px;font-size:11px;color:var(--text-muted);">' + esc(ep.keyNote) + '</div>' +
            '<div style="margin-top:8px;">' +
                '<button class="btn btn-secondary btn-sm" data-client-access="copy-curl">' +
                    esc(t('clientAccess.copy_curl', 'Копировать curl')) + '</button>' +
                ' <code data-client-curl="1" style="display:none;">' + esc(curlExample(backend, opts)) + '</code>' +
            '</div>' +
        '</div>';
    }

    // ---- Копирование в буфер (с fallback для http/без clipboard API) --------
    function copyText(text) {
        if (!text) return Promise.resolve(false);
        if (navigator.clipboard && navigator.clipboard.writeText) {
            return navigator.clipboard.writeText(text).then(function () { return true; }, function () { return fallbackCopy(text); });
        }
        return Promise.resolve(fallbackCopy(text));
    }

    function fallbackCopy(text) {
        try {
            var ta = document.createElement('textarea');
            ta.value = text;
            ta.setAttribute('readonly', '');
            ta.style.position = 'fixed';
            ta.style.opacity = '0';
            document.body.appendChild(ta);
            ta.select();
            var ok = document.execCommand && document.execCommand('copy');
            document.body.removeChild(ta);
            return !!ok;
        } catch (e) {
            return false;
        }
    }

    function toast(msg, kind) {
        if (window.showToast) { window.showToast(msg, kind || 'info'); return; }
        if (window.Toast && window.Toast.show) { window.Toast.show({ message: msg, type: kind || 'info' }); }
    }

    /** Делегированный обработчик: работает для динамически перерисованных секций. */
    function onClick(ev) {
        var el = ev && ev.target;
        var attr = null;
        while (el && el.getAttribute) {
            attr = el.getAttribute('data-client-access');
            if (attr) break;
            el = el.parentNode;
        }
        if (!attr) return;
        var block = el.closest ? el.closest('[data-client-access-block]') : null;
        if (!block) return;
        var keyEl = block.querySelector('[data-client-key-value]');
        var curlEl = block.querySelector('[data-client-curl]');
        if (attr === 'toggle-key' && keyEl) {
            var masked = keyEl.getAttribute('data-masked') === '1';
            keyEl.textContent = masked ? keyEl.getAttribute('data-key') : KEY_MASK;
            keyEl.setAttribute('data-masked', masked ? '0' : '1');
            el.textContent = masked ? t('clientAccess.hide', 'Скрыть') : t('clientAccess.show', 'Показать');
            return;
        }
        if (attr === 'copy-key' && keyEl) {
            copyText(keyEl.getAttribute('data-key')).then(function (ok) {
                toast(ok ? t('clientAccess.copied', 'Ключ скопирован') : t('clientAccess.copy_failed', 'Не удалось скопировать'),
                    ok ? 'success' : 'error');
            });
            return;
        }
        if (attr === 'copy-curl' && curlEl) {
            copyText(curlEl.textContent).then(function (ok) {
                toast(ok ? t('clientAccess.copied_curl', 'curl скопирован') : t('clientAccess.copy_failed', 'Не удалось скопировать'),
                    ok ? 'success' : 'error');
            });
        }
    }

    var mounted = false;
    /** mount — один делегированный слушатель на документ (идемпотентно). */
    function mount() {
        if (mounted) return;
        mounted = true;
        if (typeof document !== 'undefined' && document.addEventListener) {
            document.addEventListener('click', onClick);
        }
    }

    window.ClientAccess = {
        mount: mount,
        render: render,
        pure: {
            clientKey: clientKey,
            backendKind: backendKind,
            backendHost: backendHost,
            internalAddress: internalAddress,
            endpoints: endpoints,
            curlExample: curlExample,
            KEY_MASK: KEY_MASK
        }
    };
})();

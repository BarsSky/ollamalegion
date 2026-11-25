// image-model-files.js — R751 (2026-11-25): панель управления файлами
// скачанной image-группы («Модели на диске» → кнопка «Файлы»).
//
// ЗАЧЕМ. До R751 у оператора не было ни одного способа управлять файлами ВНУТРИ
// группы: таблица показывала только роли и кнопку «удалить bundle целиком».
// Живые последствия: удалённый/недокачанный файл было видно лишь по «модель не
// загрузилась»; заменить квант diffusion означало снести группу и качать заново;
// «докачать недостающее» относилось только к выбору на вкладке HF.
//
// ЧТО ДЕЛАЕТ ПАНЕЛЬ:
//   - показывает ВЕСЬ состав группы: роль, имя файла, ожидаемый и фактический
//     размер, состояние «на узле / файла нет / частично N% / состояние не
//     сообщено», файлы вне профиля;
//   - «Докачать» — отсутствующий/частичный файл (частичный продолжается с
//     `.download`, а не с нуля);
//   - «Перекачать» — удаляет текущий файл роли и скачивает заново (в т.ч. новый
//     квант diffusion);
//   - «Удалить с узла» — удаляет файл и его запись из profile.json, после чего
//     вердикт совместимости честно говорит «не хватает роли»;
//   - «Докачать недостающее» — очередь по всем файлам без файла на узле.
//
// КОНТРАКТ С ВОРКЕРОМ (через прозрачный прокси балансера):
//   POST   /api/v1/image/backends/{id}/proxy/api/image/models/{name}/files
//          {role, repo, filename, revision?, replace?} → 202 {trackingId,...}
//   DELETE /api/v1/image/backends/{id}/proxy/api/image/models/{name}/files?filename=…
//   GET    /api/v1/image/backends/{id}/proxy/api/image/models/{name}? НЕТ:
//          свежий состав приходит в GET .../models (его перечитывает страница).
//   Прогресс — существующий /api/v1/image/backends/{id}/hf/progress?bundleId=…
//
// ГРАНИЦЫ: панель НЕ рисует кнопки добавления недостающих ролей из каталога —
// это R710 и живёт в строке модели (image-page.js). Здесь только файлы, которые
// уже известны профилю (или лежат в каталоге вне него).
(function () {
    'use strict';

    var POLL_MS = 1500;
    var TIMEOUT_MS = 15000;

    var state = {
        open: false,
        backendId: '',
        model: null,
        host: null,
        // queue — файлы, которые надо докачать по кнопке «Докачать недостающее»;
        // обрабатываются ПО ОДНОМУ: параллельные загрузки в один каталог дают
        // гонку за profile.json и непонятный прогресс.
        queue: [],
        op: null,          // {file, trackingId, mode}
        poll: null,
        status: '',        // строка прогресса под заголовком
        busy: false,
        // R755: матрица форматов — family → ответ GET .../formats. Кэш на
        // страницу (правила статические); formatLoading — чтобы не дёргать
        // воркер на каждый рендер панели.
        formatMatrices: {},
        formatLoading: {}
    };

    function t(key, fallback, vars) {
        if (window.I18N && typeof window.I18N.t === 'function') {
            var v = vars ? window.I18N.t(key, vars) : window.I18N.t(key);
            if (v && v !== key) return v;
        }
        var base = fallback !== undefined ? fallback : key;
        if (vars) {
            base = String(base).replace(/\{(\w+)\}/g, function (m, k) {
                return Object.prototype.hasOwnProperty.call(vars, k) ? String(vars[k]) : m;
            });
        }
        return base;
    }

    function escapeHtml(text) {
        if (text === null || text === undefined) return '';
        return String(text)
            .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
            .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
    }

    function el(id) {
        return (typeof document !== 'undefined' && document.getElementById)
            ? document.getElementById(id) : null;
    }

    function formatBytes(n) {
        if (n === null || n === undefined || n === '' || isNaN(Number(n))) return '-';
        var v = Number(n);
        if (v <= 0) return '0 B';
        var units = ['B', 'KB', 'MB', 'GB', 'TB'];
        var i = 0;
        while (v >= 1024 && i < units.length - 1) { v = v / 1024; i++; }
        return (i === 0 ? String(Math.round(v)) : v.toFixed(v >= 100 ? 0 : 1)) + ' ' + units[i];
    }

    /** roleLabel — подпись роли из общего модуля (одна таблица переводов). */
    function roleLabel(role) {
        var api = window.ImageModelsHf && window.ImageModelsHf.pure;
        if (api && typeof api.roleLabel === 'function') return api.roleLabel(role);
        return role || '-';
    }

    // =====================================================================
    // R755: бейджи форматов «что движок читает» (GET .../formats)
    // =====================================================================

    /** modelFamily — семейство текущей группы (для матрицы форматов). */
    function modelFamily() {
        return String((state.model && state.model.family) || '');
    }

    function formatMatrix() {
        var m = state.formatMatrices[modelFamily()];
        return (m && !m.error) ? m : null;
    }

    /** ensureFormatMatrix — один запрос на семейство; рендер не блокирует. */
    function ensureFormatMatrix() {
        var key = modelFamily();
        if (!state.backendId) return;
        if (Object.prototype.hasOwnProperty.call(state.formatMatrices, key)) return;
        if (state.formatLoading[key]) return;
        state.formatLoading[key] = true;
        request(backendUrl('formats?family=' + encodeURIComponent(key)), { timeoutMs: TIMEOUT_MS })
            .then(function (data) { state.formatMatrices[key] = data || { error: true }; })
            .catch(function (e) {
                state.formatMatrices[key] = { error: true, message: (e && e.message) || String(e) };
            })
            .then(function () {
                state.formatLoading[key] = false;
                refreshPanel();
            });
    }

    /** formatBadge — бейдж формата файла (пусто, пока матрицы нет). */
    function formatBadge(filename, role) {
        var api = window.ImageFormatBadges;
        var m = formatMatrix();
        if (!api || !m) return '';
        var html = api.badgeHtml(m, filename, role);
        return html ? ' ' + html : '';
    }

    /** formatSummary — строка «Форматы: …» в шапке панели. */
    function formatSummary() {
        var api = window.ImageFormatBadges;
        var m = formatMatrix();
        if (!api || !m) return '';
        var text = api.summaryText(m);
        return text ? '<div class="ifb-summary">' + escapeHtml(text) + '</div>' : '';
    }

    /** formatMlxWarning — предупреждение о MLX-квантах среди файлов группы. */
    function formatMlxWarning() {
        var api = window.ImageFormatBadges;
        var m = formatMatrix();
        if (!api || !m) return '';
        var names = files().map(function (f) { return f.filename; });
        return api.mlxWarningHtml(m, names);
    }

    function apiBase() {
        var cfg = window.WEBUI_CONFIG || {};
        var base = cfg.API_BASE_URL || cfg.API_BASE ||
            (window.location && window.location.origin) || '';
        return String(base).replace(/\/+$/, '');
    }

    function authHeaders() {
        var headers = (window.Api && typeof window.Api.getAuthHeaders === 'function')
            ? window.Api.getAuthHeaders() : { 'Content-Type': 'application/json' };
        return headers;
    }

    /** proxyUrl — путь ВОРКЕРА через прозрачный прокси балансера. */
    function proxyUrl(rest) {
        return apiBase() + '/api/v1/image/backends/' + encodeURIComponent(state.backendId) + '/proxy' + rest;
    }

    function backendUrl(rest) {
        return apiBase() + '/api/v1/image/backends/' + encodeURIComponent(state.backendId) + '/' + rest;
    }

    function request(url, opts) {
        opts = opts || {};
        var ctrl = (typeof AbortController !== 'undefined') ? new AbortController() : null;
        var timer = null;
        if (ctrl) timer = setTimeout(function () { ctrl.abort(); }, opts.timeoutMs || TIMEOUT_MS);
        var init = { method: opts.method || 'GET', headers: authHeaders() };
        if (ctrl) init.signal = ctrl.signal;
        if (opts.body !== undefined) init.body = opts.body;
        return fetch(url, init).then(function (resp) {
            if (timer) clearTimeout(timer);
            return resp.text().then(function (text) {
                var parsed = null;
                if (text) { try { parsed = JSON.parse(text); } catch (e) { parsed = text; } }
                if (!resp.ok) {
                    var msg = (parsed && (parsed.error || parsed.message)) || ('HTTP ' + resp.status);
                    var err = new Error(msg);
                    err.status = resp.status;
                    err.body = parsed;
                    throw err;
                }
                return parsed;
            });
        }).catch(function (err) {
            if (timer) clearTimeout(timer);
            if (err && err.name === 'AbortError') {
                var e2 = new Error('timeout');
                e2.code = 'timeout';
                throw e2;
            }
            throw err;
        });
    }

    function toast(msg, type) {
        if (window.Toast && typeof window.Toast.show === 'function') {
            window.Toast.show({ message: msg, type: type || 'info' });
            return;
        }
        if (window.Notifications && typeof window.Notifications.show === 'function') {
            window.Notifications.show(msg, type);
            return;
        }
        if (window.console) console.log('[image-model-files]', type || 'info', msg);
    }

    // =====================================================================
    // Модель файла: состояние, размер, действия
    // =====================================================================

    function files() {
        return (state.model && Array.isArray(state.model.files)) ? state.model.files : [];
    }

    /** fileState — состояние файла для UI (present может быть null = не сообщено). */
    function fileState(f) {
        if (f.unregistered) {
            return { level: 'warn', key: 'unregistered', text: t('imageModels.files_unregistered', 'вне профиля') };
        }
        if (f.present === true) {
            return { level: 'ok', key: 'present', text: t('imageModels.files_present', 'на узле') };
        }
        if (f.present === false) {
            if (f.partialBytes > 0) {
                var total = Number(f.sizeBytes || 0);
                var pct = total > 0 ? Math.min(99, Math.floor(f.partialBytes * 100 / total)) : 0;
                return {
                    level: 'warn', key: 'partial',
                    text: pct > 0
                        ? t('imageModels.files_partial', 'частично {pct}%', { pct: pct })
                        : t('imageModels.files_partial_unknown', 'частично (размер неизвестен)')
                };
            }
            return { level: 'bad', key: 'missing', text: t('imageModels.files_missing', 'файла нет') };
        }
        return { level: 'unknown', key: 'unknown', text: t('imageModels.files_unknown', 'состояние не сообщено') };
    }

    /** countableMissing — что можно докачать: известен источник и файла нет целиком. */
    function countableMissing() {
        return files().filter(function (f) {
            return !f.unregistered && f.repo && f.role && f.present !== true;
        });
    }

    // =====================================================================
    // R757 (2026-11-25): ПОЧЕМУ «Докачать недостающее» нечего делать
    // =====================================================================
    //
    // ЖИВАЯ ЖАЛОБА: «во вкладке по image-моделям в скачанных моделях у той, что
    // не хватало элементов для запуска, кнопка „докачать необходимое“ никак не
    // реагировала».
    //
    // ЧТО БЫЛО. Кнопка рисовалась с атрибутом `disabled` и объяснением ТОЛЬКО в
    // `title` (всплывающая подсказка). Нажатие disabled-кнопки не порождает
    // события вообще — то есть «никак не реагировала» было буквально верно. Хуже
    // того, причина в самом частом случае вообще не про файлы: не хватающие для
    // запуска роли (VAE/text encoder) есть в вердикте совместимости
    // (`compat.missing`), но файлов для них в профиле НЕТ, поэтому кнопка
    // считала «докачивать нечего», хотя набор неполный. Такие роли добавляются
    // компонентами из каталога (R710) в строке модели — но панель об этом молчала.
    //
    // ТЕПЕРЬ: причина видна ТЕКСТОМ под сводкой и повторяется тостом при нажатии
    // (кнопка остаётся нажимаемой — она всегда даёт ответ), а роли, которых нет в
    // профиле, названы явно.
    function missingRolesNotInProfile() {
        var compat = (state.model && state.model.compat) || {};
        var missing = Array.isArray(compat.missing) ? compat.missing : [];
        if (!missing.length) return [];
        return missing.slice();
    }

    /** filesWithoutSource — отсутствующие файлы, у которых неизвестен источник. */
    function filesWithoutSource() {
        return files().filter(function (f) {
            return !f.unregistered && f.present !== true && (!f.repo || !f.role);
        });
    }

    /**
     * downloadMissingReason — почему очередь пуста (null = есть что докачивать).
     * Возвращает {text, hint} — текст виден в панели и в тосте.
     */
    function downloadMissingReason() {
        if (countableMissing().length > 0) return null;
        var roles = missingRolesNotInProfile();
        if (roles.length) {
            return {
                text: t('imageModels.files_note_missing_roles',
                    'Не хватает ролей: {roles}. Файлов для них в профиле нет, поэтому докачивать нечего — добавьте их кнопками компонентов в строке модели, затем вернитесь сюда.',
                    { roles: roles.join(', ') }),
                hint: 'missing_roles'
            };
        }
        var noSource = filesWithoutSource();
        if (noSource.length) {
            return {
                text: t('imageModels.files_note_unknown_source',
                    'У {n} отсутствующих файлов неизвестен источник (в profile.json нет repo) — такие файлы докачать нельзя, добавьте их как компонент (R710).',
                    { n: noSource.length }),
                hint: 'unknown_source'
            };
        }
        return {
            text: t('imageModels.files_note_all_present',
                'Докачивать нечего: все файлы профиля уже на узле.'),
            hint: 'all_present'
        };
    }

    function sizeText(f) {
        var expected = Number(f.sizeBytes || 0);
        var onDisk = Number(f.diskSizeBytes || 0);
        if (f.present === true) {
            return onDisk > 0 ? formatBytes(onDisk) : (expected > 0 ? formatBytes(expected) : '-');
        }
        if (f.partialBytes > 0) {
            return t('imageModels.files_partial_size', '{have} из {total}', {
                have: formatBytes(f.partialBytes),
                total: expected > 0 ? formatBytes(expected) : '?'
            });
        }
        return expected > 0
            ? t('imageModels.files_expected_size', 'ожидается {size}', { size: formatBytes(expected) })
            : '-';
    }

    function compatLine() {
        var compat = state.model && state.model.compat;
        if (!compat) return '';
        var ok = compat.runnable !== false;
        var missing = (compat.missing && compat.missing.length)
            ? ' · ' + t('imageModels.compat_verdict_missing', 'не хватает: {roles}', { roles: compat.missing.join(', ') })
            : '';
        var text = (ok
            ? t('imageModels.compat_verdict_yes', 'сможет загрузить: да')
            : t('imageModels.compat_verdict_no', 'НЕ сможет загрузить')) + missing;
        return '<div class="imf-compat ' + (ok ? 'ok' : 'bad') + '" title="' +
            escapeHtml(String(compat.reason || '')) + '">' +
            '<i class="fas ' + (ok ? 'fa-circle-check' : 'fa-circle-xmark') + '"></i> ' +
            escapeHtml(text) + '</div>';
    }

    function rowHtml(f) {
        var st = fileState(f);
        var canDownload = !f.unregistered && !!f.repo && !!f.role;
        var actionBtns = '';
        if (f.unregistered) {
            actionBtns = '<button class="btn btn-danger btn-sm" data-admin-only data-imf-action="delete" data-imf-file="' +
                escapeHtml(f.filename) + '"><i class="fas fa-trash"></i> ' +
                escapeHtml(t('imageModels.files_delete', 'Удалить с узла')) + '</button>';
        } else if (f.present === true) {
            actionBtns =
                (canDownload
                    ? '<button class="btn btn-secondary btn-sm" data-admin-only data-imf-action="redownload" data-imf-file="' +
                        escapeHtml(f.filename) + '" data-imf-role="' + escapeHtml(f.role) + '" data-imf-repo="' + escapeHtml(f.repo) + '">' +
                        '<i class="fas fa-rotate"></i> ' + escapeHtml(t('imageModels.files_redownload', 'Перекачать')) + '</button> '
                    : '') +
                '<button class="btn btn-danger btn-sm" data-admin-only data-imf-action="delete" data-imf-file="' +
                escapeHtml(f.filename) + '"><i class="fas fa-trash"></i> ' +
                escapeHtml(t('imageModels.files_delete', 'Удалить с узла')) + '</button>';
        } else {
            actionBtns =
                (canDownload
                    ? '<button class="btn btn-primary btn-sm" data-admin-only data-imf-action="download" data-imf-file="' +
                        escapeHtml(f.filename) + '" data-imf-role="' + escapeHtml(f.role) + '" data-imf-repo="' + escapeHtml(f.repo) + '">' +
                        '<i class="fas fa-download"></i> ' + escapeHtml(t('imageModels.files_download', 'Докачать')) + '</button> '
                    : '') +
                '<button class="btn btn-danger btn-sm" data-admin-only data-imf-action="delete" data-imf-file="' +
                escapeHtml(f.filename) + '"><i class="fas fa-trash"></i> ' +
                escapeHtml(t('imageModels.files_delete', 'Удалить с узла')) + '</button>';
        }
        var sourceHint = (!f.unregistered && !f.repo && f.present !== true)
            ? '<div class="imf-hint">' + escapeHtml(t('imageModels.files_no_source',
                'источник неизвестен: в profile.json нет repo — добавьте файл как компонент')) + '</div>'
            : '';
        var unregHint = f.unregistered
            ? '<div class="imf-hint">' + escapeHtml(t('imageModels.files_unregistered_hint',
                'файл не попадёт в набор: удалите его или перекачайте нужный файл роли')) + '</div>'
            : '';
        return '<div class="imf-row" data-imf-row="' + escapeHtml(f.filename) + '">' +
            '<span class="imf-role">' + escapeHtml(f.unregistered ? '-' : roleLabel(f.role)) + '</span>' +
            '<span class="imf-name">' + escapeHtml(f.filename) + formatBadge(f.filename, f.role) + unregHint + sourceHint + '</span>' +
            '<span class="imf-size">' + escapeHtml(sizeText(f)) + '</span>' +
            '<span class="imf-state ' + st.level + '">' + escapeHtml(st.text) + '</span>' +
            '<span class="imf-actions">' + actionBtns + '</span>' +
            '</div>';
    }

    function summaryHtml() {
        var list = files();
        var present = 0, missing = 0, partial = 0, extra = 0;
        list.forEach(function (f) {
            if (f.unregistered) { extra++; return; }
            if (f.present === true) { present++; return; }
            if (f.partialBytes > 0) { partial++; return; }
            missing++;
        });
        var parts = [t('imageModels.files_summary',
            'файлов: {total} · на узле: {present} · нет: {missing} · частично: {partial}',
            { total: list.length, present: present, missing: missing, partial: partial })];
        if (extra > 0) {
            parts.push(t('imageModels.files_summary_extra', 'вне профиля: {n}', { n: extra }));
        }
        var canDownload = countableMissing().length;
        var reason = downloadMissingReason();
        // R757: кнопка НЕ disabled. Нажатие на disabled-кнопку не даёт события —
        // именно это оператор описал как «никак не реагировала». Теперь она
        // всегда нажимаема и всегда отвечает: либо ставит очередь, либо называет
        // причину тостом (и та же причина видна текстом ниже).
        var btn = '<button class="btn ' + (canDownload ? 'btn-secondary' : 'btn-secondary') + ' btn-sm" data-admin-only data-imf-action="download-missing"' +
            (reason ? ' title="' + escapeHtml(reason.text) + '"' : '') + '>' +
            '<i class="fas fa-circle-down"></i> ' +
            escapeHtml(canDownload
                ? t('imageModels.files_download_missing', 'Докачать недостающее')
                : t('imageModels.files_download_missing_none', 'Докачивать нечего')) +
            (canDownload ? ' (' + canDownload + ')' : '') + '</button>';
        var note = reason
            ? '<div class="imf-note" data-imf-note="' + escapeHtml(reason.hint) + '">' +
                '<i class="fas fa-circle-info"></i> ' + escapeHtml(reason.text) + '</div>'
            : '';
        return '<div class="imf-summary"><span>' + escapeHtml(parts.join(' · ')) + '</span>' + btn +
            '</div>' + note;
    }

    function html() {
        var m = state.model || {};
        var path = m.bundle_path ? '<span class="imf-path">' +
            escapeHtml(t('imageModels.files_group_path', 'каталог: {path}', { path: m.bundle_path })) + '</span>' : '';
        return '<div class="imf-panel">' +
            '<div class="imf-head">' +
                '<span class="imf-title"><i class="fas fa-folder-tree"></i> ' +
                    escapeHtml(t('imageModels.files_title', 'Файлы группы')) + ': <strong>' +
                    escapeHtml(m.name || '') + '</strong></span>' +
                '<span class="imf-meta">' + escapeHtml(m.family || '-') + ' · ' +
                    escapeHtml(formatBytes(m.size_bytes)) + '</span>' + path +
                '<button class="btn btn-secondary btn-sm imf-close" data-imf-action="close">' +
                    escapeHtml(t('imageModels.files_close', 'Свернуть')) + '</button>' +
            '</div>' +
            compatLine() +
            formatSummary() +
            formatMlxWarning() +
            summaryHtml() +
            (files().length
                ? '<div class="imf-list">' + files().map(rowHtml).join('') + '</div>'
                : '<div class="imf-empty">' + escapeHtml(t('imageModels.files_empty', 'В профиле нет файлов')) + '</div>') +
            (state.status
                ? '<div class="imf-status" id="imfStatus">' + escapeHtml(state.status) + '</div>'
                : '<div class="imf-status" id="imfStatus"></div>') +
            '</div>';
    }

    function render(opts) {
        opts = opts || {};
        if (opts.backendId) state.backendId = String(opts.backendId);
        if (opts.model) {
            // Смена модели = другой набор: старую очередь/прогресс не тащим.
            if (!state.model || state.model.name !== opts.model.name) {
                stopPolling();
                state.queue = [];
                state.op = null;
                state.status = '';
                state.busy = false;
            }
            state.model = opts.model;
        }
        state.host = el(opts.hostId || 'imModelFilesHost');
        if (!state.host || !state.model) return false;
        state.open = true;
        // R755: матрица форматов нужна бейджам; запрос один на семейство и не
        // блокирует отрисовку (при ответе панель перерисуется).
        ensureFormatMatrix();
        state.host.innerHTML = html();
        if (!state.host.onclick) state.host.onclick = onClick;
        return true;
    }

    // =====================================================================
    // Операции
    // =====================================================================

    function findFile(filename) {
        var hit = null;
        files().forEach(function (f) { if (f.filename === filename) hit = f; });
        return hit;
    }

    function postFile(f, replace) {
        var body = {
            role: f.role, repo: f.repo, filename: f.filename,
            revision: f.revision || 'main', replace: !!replace
        };
        return request(proxyUrl('/api/image/models/' + encodeURIComponent(state.model.name) + '/files'), {
            method: 'POST',
            body: JSON.stringify(body),
            timeoutMs: 30000
        });
    }

    function deleteFile(f) {
        return request(proxyUrl('/api/image/models/' + encodeURIComponent(state.model.name) +
            '/files?filename=' + encodeURIComponent(f.filename)), {
            method: 'DELETE', timeoutMs: 30000
        });
    }

    /** startDownload — поставить загрузку и следить за её прогрессом. */
    function startDownload(f, replace) {
        if (state.busy) return Promise.resolve(false);
        state.busy = true;
        state.status = t('imageModels.files_started', 'Скачиваю {file}…', { file: f.filename });
        refreshPanel();
        return postFile(f, replace).then(function (res) {
            res = res || {};
            if (res.status === 'present') {
                // Файл уже лежит ровно того размера — качать нечего.
                state.busy = false;
                state.status = t('imageModels.files_present', 'на узле');
                toast(t('imageModels.files_done', 'Готово: {file}', { file: f.filename }), 'success');
                refreshPanel();
                return reloadModels().then(runNext);
            }
            var tracking = String(res.trackingId || '');
            if (!tracking) {
                state.busy = false;
                refreshPanel();
                return reloadModels().then(runNext);
            }
            state.op = { file: f.filename, trackingId: tracking, mode: replace ? 'replace' : 'download' };
            startPolling();
            return true;
        }).catch(function (err) {
            state.busy = false;
            stopPolling();
            state.status = '';
            refreshPanel();
            var msg = (err && err.message) || String(err);
            if (err && err.status === 409) {
                // file_already_present / role_already_present: подсказываем
                // осознанное действие, а не «ошибка 409».
                toast(t('imageModels.files_conflict',
                    'Файл уже на узле: используйте «Перекачать», чтобы скачать заново ({error})',
                    { error: msg }), 'warn');
            } else {
                toast(t('imageModels.files_failed', 'Не удалось скачать {file}: {error}',
                    { file: f.filename, error: msg }), 'error');
            }
            return false;
        });
    }

    function startPolling() {
        stopPolling();
        state.poll = setInterval(function () {
            if (!state.op) { stopPolling(); return; }
            var tracking = state.op.trackingId;
            request(backendUrl('hf/progress?bundleId=' + encodeURIComponent(tracking)), { timeoutMs: TIMEOUT_MS })
                .then(function (p) {
                    if (!p) return;
                    var pct = Number(p.progressPct || 0);
                    if (!pct && p.totalBytes > 0) pct = Math.floor(Number(p.downloaded || 0) * 100 / Number(p.totalBytes));
                    pct = Math.max(0, Math.min(100, Math.round(pct)));
                    state.status = t('imageModels.files_progress', '{file}: {pct}%',
                        { file: state.op ? state.op.file : '', pct: pct });
                    updateStatusOnly();
                    if (p.status === 'completed') {
                        stopPolling();
                        var doneFile = state.op ? state.op.file : '';
                        state.op = null;
                        state.busy = false;
                        state.status = t('imageModels.files_done', 'Готово: {file}', { file: doneFile });
                        toast(state.status, 'success');
                        refreshPanel();
                        reloadModels().then(runNext);
                    } else if (p.status === 'failed' || p.status === 'cancelled') {
                        stopPolling();
                        var failedFile = state.op ? state.op.file : '';
                        state.op = null;
                        state.busy = false;
                        state.status = '';
                        toast(t('imageModels.files_failed', 'Не удалось скачать {file}: {error}',
                            { file: failedFile, error: p.errorMessage || p.status }), 'error');
                        refreshPanel();
                        runNext();
                    }
                })
                .catch(function (err) {
                    stopPolling();
                    state.op = null;
                    state.busy = false;
                    state.status = '';
                    refreshPanel();
                    toast(t('imageModels.files_progress_failed', 'Не удалось узнать прогресс: {error}',
                        { error: (err && err.message) || String(err) }), 'error');
                    runNext();
                });
        }, POLL_MS);
    }

    function stopPolling() {
        if (state.poll) clearInterval(state.poll);
        state.poll = null;
    }

    function updateStatusOnly() {
        var node = el('imfStatus');
        if (node) node.textContent = state.status;
    }

    /** reloadModels — перечитать список моделей страницы и перерисовать панель. */
    function reloadModels() {
        var page = window.ImagePage;
        if (!page || !page._actions || typeof page._actions.refresh !== 'function') {
            return Promise.resolve(false);
        }
        return Promise.resolve(page._actions.refresh()).then(function () {
            var fresh = null;
            var models = (page._state && page._state.models) || [];
            models.forEach(function (m) { if (state.model && m.name === state.model.name) fresh = m; });
            if (fresh) {
                state.model = fresh;
                var host = state.host || el('imModelFilesHost');
                if (host) {
                    state.host = host;
                    host.innerHTML = html();
                }
            }
            return true;
        }).catch(function () {
            return false;
        });
    }

    function refreshPanel() {
        if (state.host) state.host.innerHTML = html();
    }

    /** runNext — следующий файл из очереди «Докачать недостающее». */
    function runNext() {
        if (!state.queue.length) return Promise.resolve(false);
        if (state.busy) return Promise.resolve(false);
        var f = state.queue.shift();
        startDownload(f, false);
        return Promise.resolve(true);
    }

    function startMissingQueue() {
        var list = countableMissing();
        if (!list.length) {
            // R757: нажатие обязано давать ответ — называем КОНКРЕТНУЮ причину
            // (роли без файлов в профиле / неизвестный источник / всё на узле).
            var reason = downloadMissingReason();
            toast(reason ? reason.text : t('imageModels.files_nothing_missing',
                'Докачивать нечего: файлов, которые можно докачать, нет.'), 'info');
            return;
        }
        state.queue = list.slice();
        if (!state.busy) runNext();
    }

    function onClick(ev) {
        var target = ev && ev.target;
        var actionEl = target && target.closest ? target.closest('[data-imf-action]') : null;
        if (!actionEl) return;
        var action = actionEl.getAttribute('data-imf-action');
        if (action === 'close') {
            // Панелью владеет страница: она снимает openFilesModel и перерисует
            // таблицу (кнопка «Файлы» снова станет неактивной). Здесь только
            // останавливаем прогресс и чистим свой хост.
            close();
            var page = window.ImagePage;
            if (page && page._actions && typeof page._actions.closeModelFiles === 'function') {
                page._actions.closeModelFiles();
            }
            return;
        }
        var filename = actionEl.getAttribute('data-imf-file') || '';
        var f = filename ? findFile(filename) : null;
        if (action === 'download' && f) {
            startDownload(f, false);
            return;
        }
        if (action === 'redownload' && f) {
            if (typeof window.confirm === 'function' &&
                !window.confirm(t('imageModels.files_confirm_redownload',
                    'Перекачать {file}? Текущий файл будет удалён и скачан заново.', { file: f.filename }))) {
                return;
            }
            startDownload(f, true);
            return;
        }
        if (action === 'delete' && f) {
            if (typeof window.confirm === 'function' &&
                !window.confirm(t('imageModels.files_confirm_delete',
                    'Удалить {file} с узла? Файл будет удалён безвозвратно.', { file: f.filename }))) {
                return;
            }
            if (state.busy) {
                toast(t('imageModels.files_busy', 'Дождитесь окончания текущей операции'), 'warn');
                return;
            }
            state.busy = true;
            deleteFile(f).then(function (res) {
                state.busy = false;
                res = res || {};
                toast(t('imageModels.files_deleted', 'Удалено: {file} ({size})', {
                    file: f.filename, size: formatBytes(res.bytesFreed || 0)
                }), 'success');
                return reloadModels();
            }).catch(function (err) {
                state.busy = false;
                toast(t('imageModels.files_delete_failed', 'Не удалось удалить {file}: {error}',
                    { file: f.filename, error: (err && err.message) || String(err) }), 'error');
                refreshPanel();
            });
            return;
        }
        if (action === 'download-missing') {
            startMissingQueue();
        }
    }

    function close() {
        stopPolling();
        state.open = false;
        state.op = null;
        state.queue = [];
        state.busy = false;
        state.status = '';
        if (state.host && state.host.parentNode) state.host.innerHTML = '';
    }

    window.ImageModelFiles = {
        render: render,
        close: close,
        // Доступ к состоянию/чистым помощникам — для тестов
        // (webui/tests/image-model-files-r751.test.js).
        _state: state,
        _actions: {
            startDownload: startDownload,
            deleteFile: deleteFile,
            startMissingQueue: startMissingQueue,
            reloadModels: reloadModels,
            render: render,
            close: close
        },
        pure: {
            fileState: fileState,
            sizeText: sizeText,
            formatBytes: formatBytes,
            roleLabel: roleLabel
        }
    };
})();

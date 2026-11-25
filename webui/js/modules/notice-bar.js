// webui/js/modules/notice-bar.js — R758 (2026-11-25): ПОСТОЯННАЯ ПАНЕЛЬ
// ПРЕДУПРЕЖДЕНИЙ над страницей.
//
// ЖИВАЯ ЖАЛОБА. «По поведению при загрузке обнаружил, что проблема была в малом
// месте на диске, но это удалось поймать только в строке уведомлений у
// колокольчика, никаких больше упоминаний не увидел — что не удобно».
//
// ПОЧЕМУ КОЛОКОЛЬЧИКА НЕДОСТАТОЧНО. Колокольчик — ВЫПАДАЮЩИЙ список: событие
// видно только пока он открыт, а бейдж говорит лишь «сколько» — не «что». Для
// ресурсных отказов (нет места на диске, не хватило RAM, провал загрузки) это
// неверно: состояние ДЛИТСЯ (место само не появится), и оператор обязан видеть
// его, пока не устранит причину.
//
// ЧТО ДЕЛАЕТ ПАНЕЛЬ:
//   - постоянная строка над содержимым страницы (не выпадающая, не исчезает по
//     таймеру) с последними непрочитанными уведомлениями severity warning и выше;
//   - каждая строка: значок уровня, текст (его строит балансер), модель/узел и
//     время; «Понятно» скрывает ТОЛЬКО эту строку (markAsRead в notifications.js),
//     «Скрыть все» — все показанные;
//   - если уведомлений нет — панель скрыта (никакой пустой рамки);
//   - состояние переживает перезагрузку: скрытое хранится в localStorage
//     менеджера уведомлений (readIds), а не в памяти панели.
//
// ГРАНИЦЫ. Панель НЕ принимает решений и не опрашивает сервер заново: источник —
// уже приходящие SSE-уведомления (/api/v1/events). Поэтому она не может разойтись
// с колокольчиком: это те же события.
(function () {
    'use strict';

    var MAX_ROWS = 3;

    // Порядок уровней: что показываем и как красим.
    var LEVELS = {
        critical: { rank: 3, cls: 'critical', icon: 'fa-triangle-exclamation' },
        error: { rank: 2, cls: 'error', icon: 'fa-circle-xmark' },
        warning: { rank: 1, cls: 'warning', icon: 'fa-circle-exclamation' },
        info: { rank: 0, cls: 'info', icon: 'fa-circle-info' }
    };

    function t(key, fallback, vars) {
        if (window.I18N && typeof window.I18N.t === 'function') {
            var v = vars ? window.I18N.t(key, vars) : window.I18N.t(key);
            if (v && v !== key) return v;
        }
        var out = fallback !== undefined ? fallback : key;
        if (vars) {
            out = String(out).replace(/\{(\w+)\}/g, function (m, k) {
                return Object.prototype.hasOwnProperty.call(vars, k) ? String(vars[k]) : m;
            });
        }
        return out;
    }

    function esc(text) {
        return String(text == null ? '' : text)
            .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
            .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
    }

    /** eventId — тот же идентификатор, что использует notifications.js. */
    function eventId(ev) {
        if (!ev) return '';
        return ev.id || ((ev.timestamp || '') + '-' + (ev.message || ''));
    }

    /** severityOf — уровень события как строка ('warning' по умолчанию). */
    function severityOf(ev) {
        var s = ev && ev.severity ? String(ev.severity).toLowerCase() : '';
        return Object.prototype.hasOwnProperty.call(LEVELS, s) ? s : 'warning';
    }

    /**
     * visibleEvents — какие уведомления показывать.
     *
     * Правила (проверяются тестом): уровень warning и выше, не прочитанные,
     * свежие сверху, не больше maxRows.
     */
    function visibleEvents(buffer, isRead, maxRows) {
        var limit = maxRows || MAX_ROWS;
        var out = [];
        (buffer || []).forEach(function (ev) {
            if (!ev) return;
            if (LEVELS[severityOf(ev)].rank < LEVELS.warning.rank) return;
            if (isRead && isRead(eventId(ev))) return;
            out.push(ev);
        });
        // buffer уже «свежие сверху» (unshift в notifications.js), но полагаться
        // на это нельзя: сортируем по времени, чтобы порядок не зависел от
        // источника.
        out.sort(function (a, b) {
            return new Date(b.timestamp || 0).getTime() - new Date(a.timestamp || 0).getTime();
        });
        return out.slice(0, limit);
    }

    /** rowHtml — одна строка панели. */
    function rowHtml(ev) {
        var sev = severityOf(ev);
        var meta = [];
        if (ev && ev.model) meta.push(esc(ev.model));
        if (ev && ev.backendId) meta.push(esc(ev.backendId));
        var when = ev && ev.timestamp ? new Date(ev.timestamp) : null;
        if (when && !isNaN(when.getTime())) {
            meta.push(esc(when.toLocaleTimeString()));
        }
        var id = eventId(ev);
        return '<div class="notice-bar-row ' + LEVELS[sev].cls + '" data-notice-id="' + esc(id) + '">' +
            '<i class="fas ' + LEVELS[sev].icon + ' notice-bar-icon" aria-hidden="true"></i>' +
            '<span class="notice-bar-text">' + esc(ev && ev.message ? ev.message : '') + '</span>' +
            (meta.length ? '<span class="notice-bar-meta">' + meta.join(' · ') + '</span>' : '') +
            '<button type="button" class="btn btn-secondary btn-sm notice-bar-dismiss" data-notice-dismiss="' + esc(id) + '">' +
            esc(t('notice.dismiss', 'Понятно')) + '</button>' +
            '</div>';
    }

    var bound = false;

    function el() {
        return document.getElementById('noticeBar');
    }

    /** render — перерисовать панель из состояния менеджера уведомлений. */
    function render() {
        var host = el();
        if (!host) return false;
        var mgr = window.notifications;
        if (!mgr || typeof mgr.getBuffer !== 'function') {
            host.hidden = true;
            host.innerHTML = '';
            return false;
        }
        var isRead = (typeof mgr.isRead === 'function')
            ? function (id) { return mgr.isRead(id); }
            : function () { return false; };
        var rows = visibleEvents(mgr.getBuffer(), isRead, MAX_ROWS);
        if (!rows.length) {
            host.hidden = true;
            host.innerHTML = '';
            return true;
        }
        var head = '<div class="notice-bar-head">' +
            '<span class="notice-bar-title">' + esc(t('notice.title', 'Требует внимания')) + '</span>' +
            '<button type="button" class="btn btn-secondary btn-sm" id="noticeBarDismissAll">' +
            esc(t('notice.dismiss_all', 'Скрыть все')) + '</button>' +
            '</div>';
        host.innerHTML = head + rows.map(rowHtml).join('');
        host.hidden = false;
        return true;
    }

    /** dismiss — скрыть одно уведомление (по id) и перерисовать. */
    function dismiss(id) {
        var mgr = window.notifications;
        if (mgr && typeof mgr.markAsRead === 'function') {
            mgr.markAsRead(id);
        }
        render();
    }

    function onClick(ev) {
        var target = ev && ev.target;
        if (!target) return;
        var dismissBtn = target.closest ? target.closest('[data-notice-dismiss]') : null;
        if (dismissBtn) {
            if (ev.preventDefault) ev.preventDefault();
            dismiss(dismissBtn.getAttribute('data-notice-dismiss'));
            return;
        }
        if (target.id === 'noticeBarDismissAll' || (target.closest && target.closest('#noticeBarDismissAll'))) {
            if (ev.preventDefault) ev.preventDefault();
            var mgr = window.notifications;
            if (mgr) {
                visibleEvents(mgr.getBuffer(),
                    function (id) { return typeof mgr.isRead === 'function' ? mgr.isRead(id) : false; },
                    MAX_ROWS).forEach(function (e) {
                        if (typeof mgr.markAsRead === 'function') mgr.markAsRead(eventId(e));
                    });
            }
            render();
        }
    }

    /**
     * mount — подключить панель к менеджеру уведомлений.
     *
     * Идемпотентно: повторный вызов (переинициализация страницы) не плодит
     * подписки и обработчики.
     */
    function mount() {
        var host = el();
        if (!host) return false;
        if (!bound) {
            bound = true;
            if (host.addEventListener) host.addEventListener('click', onClick);
            var mgr = window.notifications;
            if (mgr && typeof mgr.subscribe === 'function') {
                mgr.subscribe(function () { render(); });
            }
            // Вкладка в фоне не перерисовывается браузером: возвращаясь к ней,
            // оператор должен увидеть актуальный список, а не то, что было при
            // уходе.
            if (typeof document !== 'undefined' && document.addEventListener) {
                document.addEventListener('visibilitychange', function () {
                    if (!document.hidden) render();
                });
            }
        }
        return render();
    }

    window.NoticeBar = {
        mount: mount,
        render: render,
        dismiss: dismiss,
        // Чистые функции — для тестов (webui/tests/notice-bar-r758.test.js).
        pure: {
            visibleEvents: visibleEvents,
            severityOf: severityOf,
            eventId: eventId,
            rowHtml: rowHtml
        }
    };
})();

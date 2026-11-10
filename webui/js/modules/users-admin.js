// users-admin.js — R222 (2026-10-22): управление пользователями WebUI.
//
// ЗАЧЕМ. Сервер умеет роли с R201 (admin — всё, viewer — только чтение),
// CSRF стоит с R202, разметка админских действий — R203/R222. Но завести второго
// человека («гостя») было нечем: эндпоинты /api/v1/auth/users есть, страницы в
// панели нет — только curl. Здесь список, добавление/смена пароля и удаление.
//
// ПРАВИЛА, которые модуль соблюдает:
//   - админский раздел (гость его не видит и не грузит данные);
//   - ничего не храним в localStorage: источник истины — сервер;
//   - после записи перерисовываем список из ОТВЕТА сервера, а не из своей копии;
//   - пароль в ответах сервера не приходит (только хеш на сервере), и мы его не
//     показываем никогда.
(function () {
  'use strict';

  var CFG = window.WEBUI_CONFIG || {};

  // R481: тексты модуля — через i18n (раньше раздел «Пользователи» был
  // захардкожен по-русски даже в английском интерфейсе).
  function t(key, fallback, params) {
    if (window.I18N && typeof window.I18N.t === 'function') {
      var v = window.I18N.t(key, params);
      if (v && v !== key) { return v; }
    }
    var out = fallback;
    if (params && out) {
      Object.keys(params).forEach(function (k) { out = out.replace('{' + k + '}', params[k]); });
    }
    return out;
  }

  function apiBase() {
    return String(CFG.API_BASE || '').replace(/\/+$/, '');
  }

  function authHeaders() {
    var h = { 'Content-Type': 'application/json' };
    var csrf = (window.Auth && typeof window.Auth.csrf === 'function') ? window.Auth.csrf() : '';
    if (csrf) { h['X-CSRF-Token'] = csrf; }
    var token = (window.Auth && typeof window.Auth.token === 'function') ? window.Auth.token() : '';
    if (token) { h['X-API-Token'] = token; }
    return h;
  }

  function request(method, path, body) {
    var opts = { method: method, credentials: 'same-origin', headers: authHeaders() };
    if (body !== undefined) { opts.body = JSON.stringify(body); }
    return fetch(apiBase() + path, opts).then(function (res) {
      return res.json().catch(function () { return {}; }).then(function (json) {
        return { status: res.status, body: json };
      });
    });
  }

  function hint(msg, isError) {
    var el = document.getElementById('usersHint');
    if (!el) { return; }
    el.textContent = msg || '';
    el.style.display = msg ? 'block' : 'none';
    el.style.color = isError ? 'var(--danger,#f87171)' : 'var(--text-muted,#94a3b8)';
  }

  function cell(text) {
    var td = document.createElement('td');
    td.textContent = text;
    return td;
  }

  function roleLabel(role) {
    return role === 'admin'
      ? t('settings.users.role_admin', 'администратор')
      : t('settings.users.role_viewer', 'гость (только чтение)');
  }

  function renderUsers(users) {
    var tbody = document.getElementById('usersTableBody');
    if (!tbody) { return; }
    tbody.innerHTML = '';
    if (!users || !users.length) {
      var tr = document.createElement('tr');
      var td = cell(t('settings.users.empty', 'Пользователи не заведены: вход в панель невозможен без них.'));
      td.colSpan = 3;
      tr.appendChild(td);
      tbody.appendChild(tr);
      return;
    }
    users.forEach(function (u) {
      var tr = document.createElement('tr');
      tr.appendChild(cell(u.name || ''));
      tr.appendChild(cell(roleLabel(u.role)));

      var actions = document.createElement('td');
      // Смена пароля: новый пароль спрашиваем диалогом, роль остаётся прежней.
      var passBtn = document.createElement('button');
      passBtn.className = 'btn btn-secondary';
      passBtn.type = 'button';
      passBtn.textContent = t('settings.users.change_password', 'Сменить пароль');
      passBtn.style.marginRight = '6px';
      passBtn.addEventListener('click', function () {
        var next = window.prompt(t('settings.users.prompt_password', 'Новый пароль для {name}', { name: u.name }), '');
        if (next === null) { return; }
        if (String(next).length < 8) { hint(t('settings.users.password_short', 'Пароль короче 8 символов — не сохраняю.'), true); return; }
        request('PUT', '/api/v1/auth/users', { name: u.name, password: next, role: u.role }).then(function (r) {
          if (r.status !== 200) { hint(t('settings.users.change_failed', 'Не удалось сменить пароль: {error}', { error: errorText(r) }), true); return; }
          hint(t('settings.users.password_updated', 'Пароль обновлён для {name}. Старые сессии этого пользователя погашены.', { name: u.name }), false);
          renderUsers((r.body && r.body.users) || users);
        }).catch(function (e) { hint(t('settings.users.network', 'Сеть: {error}', { error: (e && e.message ? e.message : e) }), true); });
      });

      var delBtn = document.createElement('button');
      delBtn.className = 'btn btn-secondary';
      delBtn.type = 'button';
      delBtn.textContent = t('settings.users.delete', 'Удалить');
      delBtn.addEventListener('click', function () {
        if (!window.confirm(t('settings.users.confirm_delete', 'Удалить пользователя {name}?', { name: u.name }))) { return; }
        request('DELETE', '/api/v1/auth/users?name=' + encodeURIComponent(u.name)).then(function (r) {
          if (r.status !== 200) { hint(t('settings.users.delete_failed', 'Не удалось удалить: {error}', { error: errorText(r) }), true); return; }
          hint(t('settings.users.deleted', 'Пользователь {name} удалён.', { name: u.name }), false);
          renderUsers((r.body && r.body.users) || []);
        }).catch(function (e) { hint('Сеть: ' + (e && e.message ? e.message : e), true); });
      });

      actions.appendChild(passBtn);
      actions.appendChild(delBtn);
      tr.appendChild(actions);
      tbody.appendChild(tr);
    });
  }

  // --- активные сессии (R225) ----------------------------------------------
  //
  // Зачем: единственным способом погасить чужой вход была смена пароля, а
  // оператор не видел, кто и откуда вошёл. Теперь список сессий с отзывом.

  function sessionsHint(msg, isError) {
    var el = document.getElementById('sessionsHint');
    if (!el) { return; }
    el.textContent = msg || '';
    el.style.display = msg ? 'block' : 'none';
    el.style.color = isError ? 'var(--danger,#f87171)' : 'var(--text-muted,#94a3b8)';
  }

  function formatTime(value) {
    if (!value) { return '—'; }
    var d = new Date(value);
    if (isNaN(d.getTime())) { return String(value); }
    return d.toLocaleString();
  }

  function sourceOf(s) {
    var ip = s.ip || '';
    var ua = s.user_agent || '';
    if (ua.length > 48) { ua = ua.slice(0, 48) + '…'; }
    return [ip, ua].filter(Boolean).join(' · ') || '—';
  }

  function renderSessions(list) {
    var tbody = document.getElementById('authSessionsTableBody');
    if (!tbody) { return; }
    tbody.innerHTML = '';
    if (!list || !list.length) {
      var tr = document.createElement('tr');
      var td = cell(t('settings.users.sessions_none', 'Активных сессий нет.'));
      td.colSpan = 6;
      tr.appendChild(td);
      tbody.appendChild(tr);
      return;
    }
    list.forEach(function (s) {
      var tr = document.createElement('tr');
      tr.appendChild(cell(s.user || ''));
      tr.appendChild(cell(roleLabel(s.role)));
      tr.appendChild(cell(formatTime(s.created)));
      tr.appendChild(cell(formatTime(s.last_seen) + (s.current ? ' ' + t('settings.users.session_current', '(эта вкладка)') : '')));
      tr.appendChild(cell(sourceOf(s)));

      var actions = document.createElement('td');
      if (s.current) {
        // Текст, а не вложенная <td>: td внутри td — невалидная разметка.
        actions.textContent = t('settings.users.session_current', 'текущая');
      } else {
        var btn = document.createElement('button');
        btn.className = 'btn btn-secondary';
        btn.type = 'button';
        btn.textContent = t('settings.users.session_end', 'Завершить');
        btn.addEventListener('click', function () {
          if (!window.confirm(t('settings.users.confirm_end_session', 'Завершить сессию пользователя {user}?', { user: s.user }))) { return; }
          request('DELETE', '/api/v1/auth/sessions?id=' + encodeURIComponent(s.id)).then(function (r) {
            if (r.status !== 200) { sessionsHint(t('settings.users.session_end_failed', 'Не удалось завершить: {error}', { error: errorText(r) }), true); return; }
            sessionsHint(t('settings.users.session_ended', 'Сессия завершена.'), false);
            loadSessions();
          }).catch(function (e) { sessionsHint(t('settings.users.network', 'Сеть: {error}', { error: (e && e.message ? e.message : e) }), true); });
        });
        actions.appendChild(btn);
      }
      tr.appendChild(actions);
      tbody.appendChild(tr);
    });
  }

  function loadSessions() {
    var tbody = document.getElementById('authSessionsTableBody');
    if (tbody) { tbody.innerHTML = '<tr><td colspan="6" class="loading-cell">' + t('common.loading', 'Загрузка...') + '</td></tr>'; }
    return request('GET', '/api/v1/auth/sessions').then(function (r) {
      if (r.status !== 200) {
        sessionsHint(t('settings.users.sessions_load_failed', 'Не удалось получить сессии: {error}', { error: errorText(r) }), true);
        renderSessions([]);
        return;
      }
      sessionsHint('', false);
      renderSessions((r.body && r.body.sessions) || []);
    }).catch(function (e) {
      sessionsHint('Сеть: ' + (e && e.message ? e.message : e), true);
    });
  }

  function errorText(r) {
    // R481: сервер уже объясняет, куда именно он не смог записать users.json
    // (поле hint). Раньше эта подсказка до оператора не доходила — он видел
    // только «permission denied» и искал причину в контейнере WebUI.
    var main = '';
    if (r && r.body && (r.body.error || r.body.message)) { main = r.body.error || r.body.message; }
    else { main = 'HTTP ' + (r ? r.status : '?'); }
    if (r && r.body && r.body.hint) { main += ' — ' + r.body.hint; }
    return main;
  }

  function loadUsers() {
    var tbody = document.getElementById('usersTableBody');
    if (tbody) { tbody.innerHTML = '<tr><td colspan="3" class="loading-cell">' + t('common.loading', 'Загрузка...') + '</td></tr>'; }
    return request('GET', '/api/v1/auth/users').then(function (r) {
      if (r.status === 401) { hint(t('settings.users.login_required', 'Нужен вход администратора.'), true); renderUsers([]); return; }
      if (r.status === 403) { hint(t('settings.users.admin_required', 'Требуются права администратора.'), true); renderUsers([]); return; }
      if (r.status !== 200) { hint(t('settings.users.load_failed', 'Не удалось получить список: {error}', { error: errorText(r) }), true); renderUsers([]); return; }
      hint('', false);
      renderUsers((r.body && r.body.users) || []);
    }).catch(function (e) {
      hint('Сеть: ' + (e && e.message ? e.message : e), true);
    });
  }

  function addUser() {
    var name = String((document.getElementById('userNewName') || {}).value || '').trim();
    var role = String((document.getElementById('userNewRole') || {}).value || 'viewer');
    var password = String((document.getElementById('userNewPassword') || {}).value || '');
    if (!name) { hint(t('settings.users.name_required', 'Укажите имя пользователя.'), true); return; }
    if (password.length < 8) { hint(t('settings.users.password_short', 'Пароль короче 8 символов — не сохраняю.'), true); return; }
    request('PUT', '/api/v1/auth/users', { name: name, password: password, role: role }).then(function (r) {
      if (r.status !== 200) { hint(t('settings.users.save_failed', 'Не удалось сохранить: {error}', { error: errorText(r) }), true); return; }
      var passField = document.getElementById('userNewPassword');
      if (passField) { passField.value = ''; }
      hint(t('settings.users.saved', 'Сохранено: {name} ({role}).', { name: name, role: roleLabel(role) }), false);
      renderUsers((r.body && r.body.users) || []);
    }).catch(function (e) { hint('Сеть: ' + (e && e.message ? e.message : e), true); });
  }

  function init() {
    var addBtn = document.getElementById('userAddBtn');
    if (addBtn && addBtn.addEventListener) { addBtn.addEventListener('click', addUser); }
    // Раздел админский: гостю данные не грузим вовсе (не 403 в консоли).
    // Ждём готовности аутентификации: на момент DOMContentLoaded роль ещё
    // неизвестна (/auth/me в полёте), и гость получил бы лишний 401.
    var ready = (typeof window.__authReady === 'function') ? window.__authReady() : null;
    var after = afterAuth;
    if (ready && typeof ready.then === 'function') { ready.then(after); } else { after(); }
  }

  function afterAuth() {
    if (window.Auth && typeof window.Auth.isAdmin === 'function' && window.Auth.isAdmin()) {
      loadUsers();
      loadSessions();
    }
  }

  window.UsersAdmin = { init: init, reload: loadUsers, renderUsers: renderUsers, reloadSessions: loadSessions, renderSessions: renderSessions };

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();

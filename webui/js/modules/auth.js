// auth.js — R202 (2026-10-21): ВХОД В WEBUI (сессии, роли, CSRF).
//
// ЗАЧЕМ. Сервер уже умеет сессии и роли (R201), но панель продолжала работать
// «без входа»: любой запрос проходил, потому что X-API-Token в страницу больше не
// вшивается, а сессии не использовались. Здесь — браузерная часть: экран входа,
// cookie сессии (её ставит сервер), CSRF для мутирующих запросов, кнопка выхода и
// знание роли (viewer видит только чтение).
//
// ПРИНЦИПЫ:
//   - источник истины — сервер (GET /api/v1/auth/me), ничего не храним в localStorage;
//   - если вход НЕ настроен (нет users.json), панель не блокируется: работает
//     break-glass токен на сервере, а мы показываем предупреждение, а не экран входа
//     (иначе оператор мог бы запереть себя снаружи);
//   - после входа/выхода — перезагрузка страницы: все модули перечитают данные с
//     сервера (это же лечит рассинхрон между браузерами).
(function () {
  'use strict';

  var CFG = window.WEBUI_CONFIG || {};
  var pendingInit = null;
  var state = { loading: true, authenticated: false, usersConfigured: false, user: null, csrf: '', tokenAuth: false };

  // --- R222: аварийный вход по API-токену --------------------------------
  //
  // ЖИВОЙ ДЕФЕКТ, который это закрывает. R201/R202 дали балансеру сессии, роли и
  // CSRF, но entrypoint продолжал вшивать админский токен в config.js — и любой,
  // кто открыл панель, действовал как администратор в обход входа: роль viewer
  // ничего не ограничивала, потому что браузер шёл по токену.
  //
  // Теперь токен в страницу не попадает. Если он нужен (нет настроенных
  // пользователей, потерян пароль), оператор вводит его сам; значение живёт в
  // sessionStorage вкладки и уходит в window.WEBUI_CONFIG.API_TOKEN, который
  // читают остальные модули (api.js, app.js, gguf-api.js). Закрыл вкладку —
  // токена в браузере нет.
  var TOKEN_KEY = 'ol_break_glass_token';

  function readStoredToken() {
    try { return String(sessionStorage.getItem(TOKEN_KEY) || ''); } catch (e) { return ''; }
  }

  function writeStoredToken(t) {
    try {
      if (t) { sessionStorage.setItem(TOKEN_KEY, t); } else { sessionStorage.removeItem(TOKEN_KEY); }
    } catch (e) { /* приватный режим: токен живёт только до перезагрузки */ }
  }

  function publishToken(t) {
    var token = t || '';
    if (window.WEBUI_CONFIG) { window.WEBUI_CONFIG.API_TOKEN = token; }
    CFG.API_TOKEN = token;
  }

  // auth.js подключён РАНЬШЕ api.js/app.js — токен должен быть известен до их
  // первого запроса, иначе страница начнёт с 401.
  publishToken(readStoredToken());

  function apiBase() {
    var base = CFG.API_BASE || CFG.apiBase || '';
    return String(base || '').replace(/\/+$/, '');
  }

  function url(path) { return apiBase() + path; }

  function headers(extra) {
    var h = { 'Content-Type': 'application/json' };
    if (extra) { Object.keys(extra).forEach(function (k) { h[k] = extra[k]; }); }
    if (state.csrf) { h['X-CSRF-Token'] = state.csrf; }
    // R222: аварийный токен (если оператор его ввёл) — сервер считает его
    // админским доступом; см. AuthMeHandler.
    var token = readStoredToken();
    if (token && !h['X-API-Token']) { h['X-API-Token'] = token; }
    return h;
  }

  function fetchJSON(path, opts) {
    opts = opts || {};
    return fetch(url(path), {
      method: opts.method || 'GET',
      credentials: 'same-origin',
      headers: opts.headers || headers(),
      body: opts.body
    }).then(function (res) {
      return res.json().catch(function () { return {}; }).then(function (body) {
        return { status: res.status, body: body };
      });
    });
  }

  // --- CSRF для ВСЕХ мутирующих запросов ----------------------------------
  //
  // R209 (2026-10-21): РЕГРЕСС ОТ АУТЕНТИФИКАЦИИ.
  //
  // Живая жалоба: «не получается перезагрузить модель с новыми параметрами через
  // WebUI». Причина: сервер с R201 требует X-CSRF-Token для POST/PUT/DELETE с сессией,
  // а модули (gguf-api, backend-merge, params-редактор и др.) ходят своим fetch() и
  // этот заголовок не ставили — мутация получала 403, и панель показывала «ошибка».
  //
  // Правим ОДНИМ местом: оборачиваем window.fetch и добавляем CSRF к same-origin
  // мутирующим запросам (если заголовок не задан явно). Так покрываются все модули,
  // включая будущие, и не нужно править каждый вызов.
  (function wrapFetchForCSRF() {
    if (typeof window === 'undefined' || typeof window.fetch !== 'function' || window.__csrfFetchWrapped) { return; }
    // R232: в демо-режиме перехват делает demo-mode.js (он ставится раньше и
    // должен оставаться внешним слоем). Иначе auth.js ответит синтетическим 401
    // на /api/ до того, как демо вообще увидит запрос, и панель покажет ошибки.
    if (window.WEBUI_CONFIG && window.WEBUI_CONFIG.DEMO_MODE) { return; }
    var origFetch = window.fetch.bind(window);
    window.__csrfFetchWrapped = true;
    window.fetch = function (input, init) {
      try {
        var opts = init || {};
        var method = String(opts.method || 'GET').toUpperCase();
        var mutating = method === 'POST' || method === 'PUT' || method === 'DELETE' || method === 'PATCH';
        var sameOrigin = true;
        if (typeof input === 'string') {
          sameOrigin = input.indexOf('http') !== 0 || input.indexOf(window.location.origin) === 0;
        }
        if (mutating && sameOrigin) {
          // CSRF берём у АКТУАЛЬНОГО Auth: обёртка ставится один раз, а модуль
          // может переинициализироваться (тесты, повторный init).
          var csrf = (window.Auth && typeof window.Auth.csrf === 'function') ? window.Auth.csrf() : state.csrf;
          if (csrf) {
            var headers = new Headers(opts.headers || undefined);
            if (!headers.has('X-CSRF-Token')) { headers.set('X-CSRF-Token', csrf); }
            opts.headers = headers;
            init = opts;
          }
        }
      } catch (e) { /* не мешаем запросу: заголовок — не повод падать */ }
      // R223: пока не известно состояние входа, не стреляем запросами в /api/:
      // на экране входа это давало пачку 401 (backends/cluster/queue/sessions) в
      // консоли и логах балансера. Ждём __authReady (тот же промис, что у app.js),
      // и если вход настроен, но его нет — синтетический 401 без сети. Когда вход
      // не настроен (usersConfigured=false), пускаем как раньше: панель открыта.
      var isAuthPath = typeof input === 'string' && input.indexOf('/api/v1/auth/') >= 0;
      var isAPIPath = typeof input === 'string' && input.indexOf('/api/') >= 0;
      if (!isAPIPath || isAuthPath) { return origFetch(input, init); }
      // Состояние берём у АКТУАЛЬНОГО Auth, а не из замыкания: обёртка ставится
      // один раз, а модуль может быть инициализирован повторно (тесты, HMR).
      var ready = (typeof window.__authReady === 'function') ? window.__authReady() : null;
      return Promise.resolve(ready).then(function () {
        var st = (window.Auth && typeof window.Auth.state === 'function') ? window.Auth.state() : null;
        if (st && st.usersConfigured && !st.authenticated && !st.tokenAuth) {
          return unauthenticatedResponse();
        }
        return origFetch(input, init);
      });
    };
  })();

  // unauthenticatedResponse — то же, что вернул бы сервер без сессии, но без
  // запроса: модули получают привычный 401 и не рисуют «пустые» панели.
  function unauthenticatedResponse() {
    var body = '{"error":"требуется вход (R223: запрос не отправлен без сессии)"}';
    if (typeof Response === 'function') {
      return new Response(body, { status: 401, headers: { 'Content-Type': 'application/json' } });
    }
    return Promise.resolve({
      status: 401,
      ok: false,
      headers: { get: function () { return 'application/json'; } },
      json: function () { return Promise.resolve(JSON.parse(body)); },
      text: function () { return Promise.resolve(body); }
    });
  }

  function applyMe(body) {
    state.loading = false;
    state.authenticated = body.authenticated === true;
    state.usersConfigured = body.usersConfigured === true;
    state.tokenAuth = body.tokenAuth === true;
    state.user = body.user || null;
    state.csrf = body.csrf || '';
  }

  // --- экран входа -------------------------------------------------------

  function ensureOverlay() {
    var el = document.getElementById('authOverlay');
    if (el) { return el; }
    el = document.createElement('div');
    el.id = 'authOverlay';
    el.setAttribute('role', 'dialog');
    el.setAttribute('aria-modal', 'true');
    el.style.cssText = [
      'position:fixed', 'inset:0', 'z-index:100000', 'display:flex',
      'align-items:center', 'justify-content:center',
      'background:rgba(15,23,42,0.72)', 'backdrop-filter:blur(2px)'
    ].join(';');
    el.innerHTML = [
      '<form id="authForm" style="width:340px;background:var(--bg-panel,#111827);border:1px solid var(--border,#334155);',
      'border-radius:12px;padding:22px;box-shadow:0 20px 60px rgba(0,0,0,0.45);color:var(--text,#e5e7eb);font-size:14px">',
      '  <div style="font-size:16px;font-weight:600;margin-bottom:4px">🔐 Вход в OllamaLegion</div>',
      '  <div id="authHint" style="font-size:12px;color:var(--text-secondary,#94a3b8);margin-bottom:14px">',
      '    Требуются учётные данные администратора.</div>',
      '  <label style="display:block;font-size:12px;margin-bottom:4px">Пользователь</label>',
      '  <input id="authName" name="name" autocomplete="username" required',
      '    style="width:100%;box-sizing:border-box;padding:8px;border-radius:8px;border:1px solid var(--border,#334155);',
      '    background:transparent;color:inherit;margin-bottom:10px">',
      '  <label style="display:block;font-size:12px;margin-bottom:4px">Пароль</label>',
      '  <input id="authPassword" name="password" type="password" autocomplete="current-password" required',
      '    style="width:100%;box-sizing:border-box;padding:8px;border-radius:8px;border:1px solid var(--border,#334155);',
      '    background:transparent;color:inherit;margin-bottom:14px">',
      '  <div id="authError" style="display:none;font-size:12px;color:#f87171;margin-bottom:10px"></div>',
      '  <button type="submit" id="authSubmit"',
      '    style="width:100%;padding:9px;border-radius:8px;border:0;background:#2563eb;color:#fff;font-weight:600;cursor:pointer">',
      '    Войти</button>',
      '  <div style="font-size:11px;color:var(--text-secondary,#94a3b8);margin-top:12px">',
      '    Потеряли пароль? Задайте LB_ADMIN_USER/LB_ADMIN_PASSWORD и перезапустите балансер.</div>',
      '  <div style="border-top:1px solid var(--border,#334155);margin-top:12px;padding-top:10px">',
      '    <label style="display:block;font-size:12px;margin-bottom:4px" for="authToken">',
      '      Аварийный вход по API-токену</label>',
      '    <input id="authToken" name="token" type="password" autocomplete="off" placeholder="X-API-Token (необязательно)"',
      '      style="width:100%;box-sizing:border-box;padding:8px;border-radius:8px;border:1px solid var(--border,#334155);',
      '      background:transparent;color:inherit;margin-bottom:8px">',
      '    <button type="button" id="authTokenSubmit"',
      '      style="width:100%;padding:7px;border-radius:8px;border:1px solid var(--border,#334155);',
      '      background:transparent;color:inherit;cursor:pointer;font-size:12px">Войти с токеном</button>',
      '  </div>',
      '</form>'
    ].join('');
    document.body.appendChild(el);
    el.addEventListener('submit', onLoginSubmit);
    // Делегирование, а не прямая привязка: кнопка создаётся через innerHTML, и в
    // момент сборки оверлея её ещё нет в DOM.
    el.addEventListener('click', function (ev) {
      var target = ev && ev.target;
      if (target && target.id === 'authTokenSubmit') { onTokenLogin(); }
    });
    return el;
  }

  // Вход по токену: значение НЕ хранится в отданной странице, только в
  // sessionStorage этой вкладки. Проверяем токен запросом, который требует прав
  // администратора: 200 — токен верный, иначе показываем ошибку и не сохраняем.
  function onTokenLogin() {
    var input = document.getElementById('authToken');
    var token = String((input && input.value) || '').trim();
    var btn = document.getElementById('authTokenSubmit');
    if (!token) { showError('Введите API-токен или войдите по паролю.'); return; }
    if (btn) { btn.disabled = true; btn.textContent = 'Проверка…'; }
    showError('');
    fetch(url('/api/v1/auth/users'), {
      method: 'GET',
      credentials: 'same-origin',
      headers: { 'X-API-Token': token }
    }).then(function (res) {
      if (res.status !== 200) {
        throw new Error(res.status === 401 ? 'токен не принят балансером' : ('HTTP ' + res.status));
      }
      writeStoredToken(token);
      publishToken(token);
      location.reload();
    }).catch(function (err) {
      writeStoredToken('');
      publishToken('');
      showError('Токен не подошёл: ' + (err && err.message ? err.message : err));
    }).finally(function () {
      if (btn) { btn.disabled = false; btn.textContent = 'Войти с токеном'; }
    });
  }

  function showError(msg) {
    var box = document.getElementById('authError');
    if (!box) { return; }
    box.textContent = msg;
    box.style.display = msg ? 'block' : 'none';
  }

  function onLoginSubmit(ev) {
    ev.preventDefault();
    var name = (document.getElementById('authName') || {}).value || '';
    var password = (document.getElementById('authPassword') || {}).value || '';
    var btn = document.getElementById('authSubmit');
    if (btn) { btn.disabled = true; btn.textContent = 'Вход…'; }
    showError('');
    fetchJSON('/api/v1/auth/login', {
      method: 'POST',
      body: JSON.stringify({ name: name, password: password })
    }).then(function (r) {
      if (r.status === 200 && r.body && r.body.success) {
        state.authenticated = true;
        state.user = r.body.user || { name: name, role: 'admin' };
        state.csrf = r.body.csrf || '';
        hideOverlay();
        location.reload();
        return;
      }
      var msg = (r.body && (r.body.error || r.body.message)) || ('HTTP ' + r.status);
      showError('Не удалось войти: ' + msg);
    }).catch(function (err) {
      showError('Ошибка сети: ' + (err && err.message ? err.message : err));
    }).finally(function () {
      if (btn) { btn.disabled = false; btn.textContent = 'Войти'; }
    });
  }

  function showOverlay() {
    var el = ensureOverlay();
    el.style.display = 'flex';
    var hint = document.getElementById('authHint');
    if (hint) {
      hint.textContent = state.usersConfigured
        ? 'Требуются учётные данные администратора.'
        : 'Вход не настроен (нет пользователей). Панель без входа не работает: задайте LB_ADMIN_USER/LB_ADMIN_PASSWORD на балансере или введите аварийный API-токен ниже.';
    }
    var name = document.getElementById('authName');
    if (name && name.focus) { name.focus(); }
  }

  function hideOverlay() {
    var el = document.getElementById('authOverlay');
    if (el) { el.style.display = 'none'; }
  }

  // R222: вход не настроен на сервере (нет пользователей). Блокировать панель
  // нельзя: при выключенной серверной аутентификации она действительно открыта.
  // Но и молчать нельзя — токен больше не вшит в страницу, поэтому показываем
  // полосу с честным объяснением и входом по токену.
  function showBanner(text) {
    if (document.getElementById('authBanner')) { return; }
    var bar = document.createElement('div');
    bar.id = 'authBanner';
    bar.style.cssText = [
      'position:fixed', 'left:0', 'right:0', 'bottom:0', 'z-index:99998',
      'display:flex', 'align-items:center', 'gap:12px', 'justify-content:center',
      'padding:10px 14px', 'font-size:13px', 'color:#fde68a',
      'background:rgba(120,53,15,0.95)', 'border-top:1px solid rgba(253,230,138,0.4)'
    ].join(';');
    var span = document.createElement('span');
    span.textContent = text;
    var btn = document.createElement('button');
    btn.type = 'button';
    btn.textContent = 'Войти по паролю или токену';
    btn.style.cssText = 'padding:5px 10px;border-radius:6px;border:1px solid rgba(253,230,138,0.5);' +
      'background:transparent;color:inherit;cursor:pointer';
    btn.addEventListener('click', function () { showOverlay(); });
    bar.appendChild(span);
    bar.appendChild(btn);
    document.body.appendChild(bar);
  }

  // --- видимость админских действий -------------------------------------
  //
  // R203 (2026-10-21): роль viewer видит панель, но НЕ админские действия.
  // Разметка помечает такие элементы атрибутом data-admin-only; сервер всё равно
  // проверяет права (403), это лишь UX — не давать нажимать то, что запрещено.
  // R222: администратор — это либо сессия с ролью admin, либо аварийный вход по
  // токену (сервер по токену даёт полные права). Неаутентифицированный посетитель
  // («гость») админских действий не видит: раньше разметка пряталась ТОЛЬКО после
  // входа, и гость видел все кнопки, получая 403 уже по факту нажатия.
  function isAdminNow() {
    if (state.tokenAuth) { return true; }
    return !!(state.user && state.user.role === 'admin');
  }

  function applyRoleVisibility() {
    var isAdmin = isAdminNow();
    var nodes = (document.querySelectorAll ? document.querySelectorAll('[data-admin-only]') : []);
    Array.prototype.forEach.call(nodes, function (el) {
      if (!el || !el.style) { return; }
      if (isAdmin) {
        el.style.display = el.getAttribute('data-admin-was') || '';
        el.removeAttribute('disabled');
        el.removeAttribute('data-admin-hidden');
        el.removeAttribute('title');
      } else {
        if (!el.hasAttribute('data-admin-hidden')) {
          el.setAttribute('data-admin-was', el.style.display || '');
          el.setAttribute('data-admin-hidden', '1');
        }
        el.style.display = 'none';
        el.setAttribute('disabled', 'disabled');
      }
    });
  }

  // R222: админские кнопки появляются и ПОСЛЕ первичной отрисовки — карточки
  // моделей, таблицы профилей, прогресс загрузки рисуются модулями. Без
  // наблюдателя гость видел их до первого перерисовывания ролей. Наблюдатель
  // дёшев: scroll/подсветка в нём не участвуют, только вставка узлов.
  function watchDynamicAdminNodes() {
    if (typeof MutationObserver !== 'function' || !document.body) { return; }
    var scheduled = false;
    var obs = new MutationObserver(function () {
      if (scheduled) { return; }
      scheduled = true;
      var run = function () { scheduled = false; applyRoleVisibility(); };
      if (typeof requestAnimationFrame === 'function') { requestAnimationFrame(run); } else { setTimeout(run, 16); }
    });
    obs.observe(document.body, { childList: true, subtree: true });
  }

  // --- кнопка выхода в шапке --------------------------------------------

  function renderUserChip() {
    if (!state.authenticated || !state.user) { return; }
    if (document.getElementById('authUserChip')) { return; }
    var host = document.querySelector('.header-actions') || document.querySelector('header') || document.body;
    var chip = document.createElement('div');
    chip.id = 'authUserChip';
    chip.style.cssText = 'display:inline-flex;align-items:center;gap:8px;font-size:12px;margin-left:8px';
    var role = state.user.role === 'admin' ? 'администратор' : 'только чтение';
    chip.innerHTML = '<span title="' + (state.user.name || '') + '">👤 ' +
      String(state.user.name || '').replace(/[<>&"]/g, '') + ' <span style="opacity:.7">(' + role + ')</span></span>' +
      '<button id="authLogoutBtn" type="button" style="padding:3px 8px;border-radius:6px;border:1px solid var(--border,#334155);' +
      'background:transparent;color:inherit;cursor:pointer">Выйти</button>';
    host.appendChild(chip);
    var logoutBtn = document.getElementById('authLogoutBtn');
    if (logoutBtn && logoutBtn.addEventListener) {
      logoutBtn.addEventListener('click', function () {
        // R222: выходим «начисто» — сессия на сервере + аварийный токен вкладки.
        // Иначе после выхода страница осталась бы админской по токену.
        writeStoredToken('');
        publishToken('');
        fetchJSON('/api/v1/auth/logout', { method: 'POST' }).finally(function () {
          location.reload();
        });
      });
    }
  }

  // --- публичный API -----------------------------------------------------

  var Auth = {
    init: function () {
      // R206: идемпотентно — приложение ждёт этот же промис (window.__authReady),
      // чтобы не делать запросы до входа и не сыпать 401 в консоль.
      if (pendingInit) { return pendingInit; }
      pendingInit = fetchJSON('/api/v1/auth/me').then(function (r) {
        applyMe((r && r.body) || {});
        var guestRole = state.tokenAuth ? 'admin' : 'guest';
        if (document.body && document.body.classList) {
          document.body.classList.add('auth-role-' + guestRole);
        }
        if (state.authenticated || state.tokenAuth) {
          hideOverlay();
          renderUserChip();
          applyRoleVisibility();
          watchDynamicAdminNodes();
          return state;
        }
        // Гость: админские действия не показываем и ждём входа.
        applyRoleVisibility();
        watchDynamicAdminNodes();
        if (state.usersConfigured) {
          showOverlay();
        } else {
          // R222: пользователи не настроены. Панель не блокируем (при выключенной
          // серверной auth она открыта), но объясняем состояние и даём вход.
          showBanner('Вход в WebUI не настроен: пользователей нет. ' +
            'Задайте LB_ADMIN_USER/LB_ADMIN_PASSWORD на балансере или войдите по аварийному API-токену.');
        }
        return state;
      }).catch(function (err) {
        state.loading = false;
        console.warn('[auth] /auth/me недоступен:', err && err.message ? err.message : err);
        applyRoleVisibility();
        return state;
      });
      return pendingInit;
    },
    state: function () { return state; },
    csrf: function () { return state.csrf; },
    user: function () { return state.user; },
    role: function () { return state.user ? state.user.role : ''; },
    isAdmin: function () { return isAdminNow(); },
    tokenAuth: function () { return !!state.tokenAuth; },
    token: function () { return readStoredToken(); },
    setToken: function (t) { writeStoredToken(t); publishToken(t); },
    clearToken: function () { writeStoredToken(''); publishToken(''); },
    logout: function () {
      writeStoredToken('');
      publishToken('');
      return fetchJSON('/api/v1/auth/logout', { method: 'POST' });
    },
    headers: headers,
    applyRoleVisibility: applyRoleVisibility
  };

  window.Auth = Auth;
  // приложение ждёт готовности аутентификации (см. app.js)
  window.__authReady = function () {
    return pendingInit || Auth.init();
  };
  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', function () { Auth.init(); });
  } else {
    Auth.init();
  }
})();

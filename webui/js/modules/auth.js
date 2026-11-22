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

  // R238d: строки auth-плашки/чипа — через общий каталог i18n (раньше были
  // жёстко русскими и при EN-интерфейсе оставались на русском).
  function tr(key, fallback) {
    try {
      if (window.I18N && window.I18N.t) {
        var v = window.I18N.t(key);
        if (v && v !== key) { return v; }
      }
    } catch (e) { /* нет i18n — fallback */ }
    return fallback;
  }

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
    // R679: состояние и UI больше не расходятся. Раньше applyMe менял ТОЛЬКО
    // state, а класс роли/чип/видимость админских действий обновлялись в одном
    // месте — в init() на загрузке страницы. Поэтому «сессия истекла» в открытой
    // панели оставляла админские кнопки на месте (действия падали на 401).
    syncAuthUi();
    return state;
  }

  // --- R679: ЕДИНЫЙ переход «кто я» → «что видно» --------------------------
  //
  // Три функции ниже описывают одно правило: состояние сессии (state) — источник
  // истины, а КЛАСС РОЛИ на body, ЧИП пользователя в шапке и ВИДИМОСТЬ
  // админских действий ([data-admin-only]) — его следствие. Вызываются из
  // applyMe (успешный /auth/me), expireSession (сессия истекла) и restoreSession
  // (явный вход) — то есть во всех переходах, а не только на загрузке.

  /**
   * applyRoleClass — класс роли на body.
   *
   * R673: класс считается по ФАКТИЧЕСКИМ правам (isAdminNow), а не по одному
   * признаку — иначе авторизованный администратор получал auth-role-guest.
   * R679: класс именно ПЕРЕСТАВЛЯЕТСЯ (remove оба + add нужный). «Только add»
   * оставлял панель в двух ролях одновременно: после истечения сессии на body
   * оставался auth-role-admin, и любой CSS/код, читающий роли, видел админа.
   */
  function applyRoleClass() {
    if (!document.body || !document.body.classList) { return; }
    var role = isAdminNow() ? 'admin' : 'guest';
    document.body.classList.remove('auth-role-admin');
    document.body.classList.remove('auth-role-guest');
    document.body.classList.add('auth-role-' + role);
  }

  /** removeUserChip — убрать чип пользователя из шапки (R679). */
  function removeUserChip() {
    var chip = (document.getElementById ? document.getElementById('authUserChip') : null);
    if (!chip) { return; }
    if (chip.parentNode && typeof chip.parentNode.removeChild === 'function') {
      chip.parentNode.removeChild(chip);
    } else if (typeof chip.remove === 'function') {
      chip.remove();
    } else if (chip.style) {
      // Заглушка DOM в тестах без DOM-связей: прячем, чтобы панель не показывала
      // пользователя, которого уже нет.
      chip.style.display = 'none';
    }
  }

  /** hideBanner — снять полосу объяснения (R679). */
  function hideBanner() {
    var bar = (document.getElementById ? document.getElementById('authBanner') : null);
    if (!bar) { return; }
    if (bar.parentNode && typeof bar.parentNode.removeChild === 'function') {
      bar.parentNode.removeChild(bar);
    } else if (typeof bar.remove === 'function') {
      bar.remove();
    } else if (bar.style) {
      bar.style.display = 'none';
    }
  }

  /** syncAuthUi — привести видимую часть панели к текущему state. */
  function syncAuthUi() {
    applyRoleClass();
    if (state.authenticated || state.tokenAuth) {
      renderUserChip();
    } else {
      removeUserChip();
    }
    applyRoleVisibility();
  }

  // --- R679: СЕССИЯ ИСТЕКЛА / СЕССИЯ ВОССТАНОВЛЕНА ------------------------
  //
  // ЖИВАЯ ЖАЛОБА: «сыпятся ошибки что пользователь неавторизован, и только
  // перезагрузка открывает меню входа, но при этом режим просмотра не активен —
  // то есть не переходит на пользователя гостя».
  //
  // Здесь — РОВНО переход «кто я» для случая, когда сервер сказал 401:
  //   state → гость, класс роли, чип, видимость админских действий и ОДНО
  //   объяснение (слой входа или полоса — в зависимости от R667-флажка).
  // Решение «пора ли» принимает SessionExpiry (session-expiry.js): он гасит
  // повторы и не даёт двум источникам (HTTP и WS) показать два объяснения.

  /**
   * expireSession — перевести открытую панель в гостя БЕЗ перезагрузки.
   *
   * Возвращает state. Идемпотентна: повторный вызов не плодит ни второго слоя,
   * ни второй полосы.
   */
  function expireSession(opts) {
    opts = opts || {};
    state.loading = false;
    state.authenticated = false;
    state.user = null;
    state.csrf = '';
    // Break-glass токен, если им и работали, сервер только что отверг: держать
    // его в sessionStorage нельзя — иначе каждый следующий запрос снова 401.
    if (state.tokenAuth) {
      writeStoredToken('');
      publishToken('');
    }
    state.tokenAuth = false;

    syncAuthUi();
    showSessionExpiredNotice();
    return state;
  }

  /**
   * showSessionExpiredNotice — РОВНО ОДНО объяснение «сессия завершилась».
   *
   * Правило R667: если оператор уже выбирал «продолжить без входа» в этой
   * вкладке, страницу НЕ перекрываем слоем — только полоса с кнопкой входа.
   * Иначе показываем слой входа (как это делала перезагрузка), потому что
   * живая жалоба звучит как «только перезагрузка открывает меню входа».
   * Второе объяснение при этом убирается: оператор не должен видеть два текста
   * об одном и том же.
   */
  function showSessionExpiredNotice() {
    if (canExplainWithBanner()) {
      dismissedOverlay = true;
      hideOverlay();
      hideBanner();
      showBanner('auth.banner.sessionExpiredLive',
        'Сессия завершилась: панель открыта в режиме просмотра. Войти — кнопка справа.');
      return;
    }
    hideBanner();
    showOverlay('expired');
  }

  /**
   * restoreSession — вернуть админский режим после явного входа БЕЗ F5.
   *
   * Вызывается ТОЛЬКО когда оператор сам вошёл (пароль или аварийный токен).
   * Если приложение уже инициализировано (сессия истекла в живой панели) —
   * поднимаем каналы и перечитываем данные на месте. Если страница была открыта
   * гостем, `ui.init()` в app.js НЕ вызывался (boot-гейт), и восстанавливать
   * нечего: перезагрузка здесь — единственное уместное место, потому что она и
   * есть штатный путь загрузки приложения.
   */
  function restoreSession() {
    clearGuestAck();
    dismissedOverlay = false;
    hideOverlay();
    hideBanner();
    syncAuthUi();
    if (window.SessionExpiry && typeof window.SessionExpiry.reset === 'function') {
      window.SessionExpiry.reset();
    }
    // R679: восстанавливаем на месте ТОЛЬКО если приложение действительно было
    // инициализировано (флаг ставит app.js → init). Если страница открыта гостем,
    // boot-гейт init() не звал: поднимать нечего, и перезагрузка — единственное
    // уместное место (она и есть штатный путь загрузки приложения).
    var appReady = (window.__olAppInitialized === true) &&
      window.App && typeof window.App.resumeAfterAuth === 'function';
    if (appReady) {
      try {
        window.App.resumeAfterAuth();
        return state;
      } catch (e) {
        if (typeof console !== 'undefined' && console.warn) {
          console.warn('[auth] восстановление на месте не удалось, перезагружаю:', e && e.message);
        }
      }
    }
    // Приложение не инициализировано (страница открыта гостем) — только reload.
    location.reload();
    return state;
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
      'border-radius:12px;padding:22px;box-shadow:0 20px 60px rgba(0,0,0,0.45);color:var(--text,#e5e7eb);font-size:var(--font-size-base)">',
      '  <div style="font-size:var(--font-size-md);font-weight:600;margin-bottom:4px">',
      '    <i class="fas fa-lock" aria-hidden="true"></i> Вход в OllamaLegion</div>',
      '  <div id="authHint" style="font-size:var(--font-size-xs);color:var(--text-secondary,#94a3b8);margin-bottom:14px">',
      '    Требуются учётные данные администратора.</div>',
      '  <label style="display:block;font-size:var(--font-size-xs);margin-bottom:4px">Пользователь</label>',
      '  <input id="authName" name="name" autocomplete="username" required',
      '    style="width:100%;box-sizing:border-box;padding:8px;border-radius:8px;border:1px solid var(--border,#334155);',
      '    background:transparent;color:inherit;margin-bottom:10px">',
      '  <label style="display:block;font-size:var(--font-size-xs);margin-bottom:4px">Пароль</label>',
      '  <input id="authPassword" name="password" type="password" autocomplete="current-password" required',
      '    style="width:100%;box-sizing:border-box;padding:8px;border-radius:8px;border:1px solid var(--border,#334155);',
      '    background:transparent;color:inherit;margin-bottom:14px">',
      '  <div id="authError" style="display:none;font-size:var(--font-size-xs);color:#f87171;margin-bottom:10px"></div>',
      '  <button type="submit" id="authSubmit"',
      '    style="width:100%;padding:9px;border-radius:8px;border:0;background:#2563eb;color:#fff;font-weight:600;cursor:pointer">',
      '    Войти</button>',
      '  <div style="font-size:var(--font-size-xs);color:var(--text-secondary,#94a3b8);margin-top:12px">',
      '    Потеряли пароль? Задайте LB_ADMIN_USER/LB_ADMIN_PASSWORD и перезапустите балансер.</div>',
      '  <div style="border-top:1px solid var(--border,#334155);margin-top:12px;padding-top:10px">',
      '    <label style="display:block;font-size:var(--font-size-xs);margin-bottom:4px" for="authToken">',
      '      Аварийный вход по API-токену</label>',
      '    <input id="authToken" name="token" type="password" autocomplete="off" placeholder="X-API-Token (необязательно)"',
      '      style="width:100%;box-sizing:border-box;padding:8px;border-radius:8px;border:1px solid var(--border,#334155);',
      '      background:transparent;color:inherit;margin-bottom:8px">',
      '    <button type="button" id="authTokenSubmit"',
      '      style="width:100%;padding:7px;border-radius:8px;border:1px solid var(--border,#334155);',
      '      background:transparent;color:inherit;cursor:pointer;font-size:var(--font-size-xs)">Войти с токеном</button>',
      '  </div>',
      // R665 (2026-11-21): оверлей входа БОЛЬШЕ НЕ ЛОВУШКА.
      //
      // Живая жалоба: «последние правки не дают взаимодействовать со страницей
      // никак». Причина — не код панели: балансер перезапускался, сессии WebUI
      // живут в его памяти, поэтому страница открывалась гостем, а гость с
      // настроенными пользователями получает ЭТОТ полноэкранный слой. Слой
      // блокирует всё, а объяснения «почему меня разлогинило» в нём не было.
      // Теперь из слоя можно выйти в режим чтения (кнопка внизу страницы
      // «Войти по паролю или токену» возвращает форму), а причина названа.
      '  <button type="button" id="authGuestContinue"',
      '    style="width:100%;margin-top:12px;padding:7px;border-radius:8px;border:0;background:transparent;',
      '    color:var(--text-secondary,#94a3b8);cursor:pointer;font-size:var(--font-size-xs);text-decoration:underline">',
      '    Продолжить без входа (только чтение)</button>',
      '</form>'
    ].join('');
    document.body.appendChild(el);
    el.addEventListener('submit', onLoginSubmit);
    // Делегирование, а не прямая привязка: кнопка создаётся через innerHTML, и в
    // момент сборки оверлея её ещё нет в DOM.
    el.addEventListener('click', function (ev) {
      var target = ev && ev.target;
      if (target && target.id === 'authTokenSubmit') { onTokenLogin(); }
      if (target && target.id === 'authGuestContinue') { dismissOverlay(); }
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
      // R679: это ЯВНЫЙ вход — возвращаем админский режим на месте, без F5
      // (полный путь восстановления — в restoreSession).
      state.tokenAuth = true;
      restoreSession();
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
        state.loading = false;
        state.authenticated = true;
        state.usersConfigured = true;
        state.user = r.body.user || { name: name, role: 'admin' };
        state.csrf = r.body.csrf || '';
        // R679: явный вход возвращает админский режим НА МЕСТЕ, без F5. Если
        // приложение на этой странице не инициализировалось (гостевой бут),
        // restoreSession сама сделает перезагрузку — иного пути там нет.
        restoreSession();
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

  // R665: оператор явно выбрал «продолжить без входа» — до перезагрузки страницы
  // слой не возвращаем (иначе следующий вызов showOverlay снова перекрыл бы панель).
  var dismissedOverlay = false;

  // R667 (2026-11-21): ключ, которым в этой вкладке запоминается выбор «работаю без
  // входа». Живая жалоба: «после Ctrl+Shift+R опять сбрасывается нормальная работа с
  // мышью» — полная перезагрузка открывала блокирующий слой заново, хотя оператор
  // уже один раз его закрыл. sessionStorage хранит решение до закрытия вкладки:
  // на перезагрузке остаётся полоса с кнопкой входа, а не перекрытие всей страницы.
  var AUTH_GUEST_ACK = 'ol_auth_guest_ack';

  function guestAckSaved() {
    try { return window.sessionStorage.getItem(AUTH_GUEST_ACK) === '1'; } catch (e) { return false; }
  }

  function clearGuestAck() {
    try { window.sessionStorage.removeItem(AUTH_GUEST_ACK); } catch (e) { /* приватный режим */ }
  }

  /**
   * canExplainWithBanner — R683: полоса ВМЕСТО слоя допустима только когда панель
   * уже работает.
   *
   * ЖИВОЙ ДЕФЕКТ (замер на стенде, образ r682). R667 разрешил «продолжить без
   * входа»: выбор оператора запоминался в sessionStorage, и объяснение переезжало
   * из полноэкранного слоя в тонкую полосу. R679 расширил это правило на истёкшую
   * сессию. Но при ЗАГРУЗКЕ ГОСТЕМ панель не инициализируется вовсе (boot-гейт
   * app.js не зовёт ui.init()), поэтому данные не приходят никогда: страница
   * остаётся скелетом («Загрузка…» в каждой панели, «Подключение…» в шапке), а
   * единственное объяснение — полоса, которую легко не заметить. Для оператора это
   * и есть «WebUI повис, не прогружая страницу»: ни данных, ни меню входа, F5 не
   * помогает (флажок в sessionStorage переживает перезагрузку).
   *
   * Правило то же, что у R683 в session-expiry.js: не выдавать недоказанное за
   * факт. Флажок «работаю без входа» касается РАБОТАЮЩЕЙ панели — только тогда
   * полосы достаточно (данные уже на экране). Пока приложение не инициализировано,
   * показываем слой: это единственный путь внутрь, и он объясняет состояние.
   */
  function canExplainWithBanner() {
    return guestAckSaved() && window.__olAppInitialized === true;
  }

  /**
   * showOverlay — показать слой входа.
   *
   * @param {string} [reason] — 'expired' (R679: сессия завершилась в живой панели)
   *   меняет подсказку: оператор должен понять, что произошло, а не гадать.
   */
  function showOverlay(reason) {
    dismissedOverlay = false;
    var el = ensureOverlay();
    el.style.display = 'flex';
    var hint = document.getElementById('authHint');
    if (hint) {
      if (reason === 'expired') {
        hint.textContent = 'Сессия завершилась (балансер перезапущен или сессия протухла). ' +
          'Войдите заново — или нажмите «Продолжить без входа», панель останется в режиме просмотра.';
      } else {
        hint.textContent = state.usersConfigured
          ? 'Требуются учётные данные администратора. Если вы уже входили, сессия завершилась при перезапуске балансера — войдите заново или нажмите «Продолжить без входа».'
          : 'Вход не настроен (нет пользователей). Панель без входа не работает: задайте LB_ADMIN_USER/LB_ADMIN_PASSWORD на балансере или введите аварийный API-токен ниже.';
      }
    }
    var name = document.getElementById('authName');
    if (name && name.focus) { name.focus(); }
  }

  // R665: выход из слоя входа в режим чтения. Полоса внизу объясняет состояние и
  // даёт кнопку возврата к форме — то есть «нет взаимодействия» больше не
  // единственный сценарий для того, кого разлогинило перезапуском балансера.
  function dismissOverlay() {
    dismissedOverlay = true;
    hideOverlay();
    try { window.sessionStorage.setItem(AUTH_GUEST_ACK, '1'); } catch (e) { /* приватный режим */ }
    showBanner('auth.banner.sessionExpired',
      'Вход не выполнен: панель открыта в режиме чтения. Войти — кнопка справа.');
  }

  function hideOverlay() {
    var el = document.getElementById('authOverlay');
    if (el) { el.style.display = 'none'; }
    // R670 (2026-11-21): слой прячется, а ФОКУС оставался в его поле пароля —
    // невидимый, но редактируемый input. Защита от перерисовки (refresh-guard)
    // считала, что оператор «печатает прямо сейчас», и не давала панели
    // отрисоваться: карточки «—», панели «Загрузка…». Снимаем фокус вместе со слоем.
    try {
      if (document.activeElement && typeof document.activeElement.blur === 'function' &&
          el && el.contains(document.activeElement)) {
        document.activeElement.blur();
      }
    } catch (e) { /* не критично */ }
  }

  // R222: вход не настроен на сервере (нет пользователей). Блокировать панель
  // нельзя: при выключенной серверной аутентификации она действительно открыта.
  // Но и молчать нельзя — токен больше не вшит в страницу, поэтому показываем
  // полосу с честным объяснением и входом по токену.
  function showBanner(key, fallback) {
    if (document.getElementById('authBanner')) { return; }
    var bar = document.createElement('div');
    bar.id = 'authBanner';
    bar.style.cssText = [
      'position:fixed', 'left:0', 'right:0', 'bottom:0', 'z-index:99998',
      'display:flex', 'align-items:center', 'gap:12px', 'justify-content:center',
      'padding:10px 14px', 'font-size:var(--font-size-sm)', 'color:#fde68a',
      'background:rgba(120,53,15,0.95)', 'border-top:1px solid rgba(253,230,138,0.4)'
    ].join(';');
    var span = document.createElement('span');
    span.setAttribute('data-i18n', key);
    span.textContent = tr(key, fallback);
    var btn = document.createElement('button');
    btn.type = 'button';
    btn.setAttribute('data-i18n', 'auth.banner.login');
    btn.textContent = tr('auth.banner.login', 'Войти по паролю или токену');
    btn.style.cssText = 'padding:5px 10px;border-radius:6px;border:1px solid rgba(253,230,138,0.5);' +
      'background:transparent;color:inherit;cursor:pointer';
    btn.addEventListener('click', function () { showOverlay(); });
    bar.appendChild(span);
    bar.appendChild(btn);
    document.body.appendChild(bar);
    if (window.I18N && I18N.apply) { I18N.apply(bar); }
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
    chip.className = 'auth-user-chip';
    var roleKey = state.user.role === 'admin' ? 'auth.role.admin' : 'auth.role.viewer';
    var roleFallback = state.user.role === 'admin' ? 'администратор' : 'только чтение';
    // R239: вместо эмодзи «человечек» - иконка Font Awesome, как у остальных
    // действий в шапке (эмодзи в user-facing тексте запрещены правилами TASTE).
    chip.innerHTML = '<span class="auth-user" title="' + (state.user.name || '') + '">' +
      '<i class="fas fa-user" aria-hidden="true"></i> ' +
      String(state.user.name || '').replace(/[<>&"]/g, '') +
      ' <span class="auth-role" style="opacity:.7">(<span data-i18n="' + roleKey + '">' + tr(roleKey, roleFallback) + '</span>)</span></span>' +
      '<button id="authLogoutBtn" class="auth-logout-btn" data-i18n="auth.logout" type="button">' + tr('auth.logout', 'Выйти') + '</button>';
    host.appendChild(chip);
    if (window.I18N && I18N.apply) { I18N.apply(chip); }
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
        // R679: applyMe теперь синхронизирует и видимую часть панели (класс роли на
        // body, чип пользователя, видимость [data-admin-only]) — см. syncAuthUi.
        applyMe((r && r.body) || {});
        if (state.authenticated || state.tokenAuth) {
          // R667: успешный вход отменяет «работаю без входа» — иначе после выхода
          // страница осталась бы без блокирующего слоя, но и без объяснения.
          clearGuestAck();
          hideOverlay();
          hideBanner();
          watchDynamicAdminNodes();
          return state;
        }
        // Гость: админские действия не показываем и ждём входа.
        watchDynamicAdminNodes();
        if (state.usersConfigured) {
          // R667: закрытый ранее слой не открываем заново после перезагрузки —
          // вместо блокировки всей страницы остаётся полоса с кнопкой входа.
          // R683: но ТОЛЬКО для работающей панели (см. canExplainWithBanner):
          // гостевой загрузке полосы недостаточно — данных нет, и без слоя у
          // оператора не остаётся ни одного пути внутрь.
          if (canExplainWithBanner()) {
            dismissedOverlay = true;
            showBanner('auth.banner.sessionExpired',
              'Вход не выполнен: панель открыта в режиме чтения. Войти — кнопка справа.');
          } else {
            showOverlay();
          }
        } else {
          // R222: пользователи не настроены. Панель не блокируем (при выключенной
          // серверной auth она открыта), но объясняем состояние и даём вход.
          showBanner('auth.banner.notConfigured', 'Вход в WebUI не настроен: пользователей нет. ' +
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
    applyRoleVisibility: applyRoleVisibility,
    // R679: единые переходы «кто я» — их зовут session-expiry.js (сессия
    // истекла), обработчики явного входа (пароль/токен) и тесты.
    applyMe: applyMe,
    expireSession: expireSession,
    restoreSession: restoreSession,
    syncAuthUi: syncAuthUi,
    removeUserChip: removeUserChip,
    hideBanner: hideBanner,
    showBanner: showBanner,
    showOverlay: showOverlay,
    // R683: hideOverlay публикуется рядом с showOverlay — им session-expiry.js
    // снимает слой входа, когда ложная тревога «сессия завершилась» отменена
    // (без этого пара «показать/убрать» оставалась односторонней, и панель
    // возвращалась в админский режим ПОД слоем входа).
    hideOverlay: hideOverlay
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

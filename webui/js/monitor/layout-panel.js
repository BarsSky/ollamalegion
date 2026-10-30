// js/monitor/layout-panel.js — R118.4 (2026-10-14).
//
// ЯВНОЕ УПРАВЛЕНИЕ РАСКЛАДКОЙ СЛОЁВ (RPC) ИЗ ВЕБ-ПАНЕЛИ.
//
// ТРЕБОВАНИЕ ОПЕРАТОРА: «иметь возможность управлять бэкендами, связывая их
// вместе, определяя кто главный а кто донор, через веб-панель». До этой правки
// связывание было доступно только через API/`.env`: селектор роли, добавленный
// в R118, жил в модуле js/modules/monitor-backends.js, а таблицу бэкендов на
// странице «Мониторинг» рисует ui-renderer.js. Плюс в monitor/api.js не было
// функции setBackendRole. Итог: оператор не видел в панели ничего про RPC.
//
// Здесь: панель «Раскладка слоёв (RPC)» (выбор главного и доноров + кнопки) и
// селектор роли прямо в строке каждого llama.cpp-бэкенда. Состояние раскладки
// (включена/выключена/деградация с причиной, узлы, замер канала) приходит из
// GET /api/v1/backends: role, rpcLayout (гард балансера), rpcLink (замер канала).

(function () {
  'use strict';

  var MA = window.MonitorApp || {};
  var T = function (key, fallback) {
    var v = MA.T ? MA.T(key) : null;
    if (v === null || v === undefined || v === key) return fallback || key;
    return v;
  };
  var esc = function (s) {
    return MA.esc ? MA.esc(s) : String(s === null || s === undefined ? '' : s);
  };

  var ROLE_MAIN = 'main';
  var ROLE_DONOR = 'donor';
  var ROLE_STANDALONE = 'standalone';

  var REFRESH_THROTTLE_MS = 10000;

  var state = {
    backends: [],
    lastLoadAt: 0,
    loading: false
  };

  // isLlamaBackend — раскладка слоёв относится только к llama.cpp-бэкендам
  // (image_cpp и ollama не участвуют: у них нет ни rpc-server'а, ни слоёв).
  function isLlamaBackend(b) {
    if (!b) return false;
    var t = String(b.backendType || b.BackendType || b.backend_type || b.type || 'llama_cpp').toLowerCase();
    return t === 'llama_cpp' || t === 'llamacpp' || t === 'cpp';
  }

  function llamaBackends() {
    return (state.backends || []).filter(isLlamaBackend);
  }

  function backendRole(b) {
    return String((b && b.role) || '').toLowerCase();
  }

  function roleLabel(role) {
    switch (String(role || '')) {
      case ROLE_MAIN: return T('monitor.role.main', 'главный (раскладка)');
      case ROLE_DONOR: return T('monitor.role.donor', 'донор (узел)');
      case ROLE_STANDALONE: return T('monitor.role.standalone', 'самодостаточный');
      default: return T('monitor.role.none', '— не задана —');
    }
  }

  // ---- строка таблицы бэкендов -------------------------------------------

  // roleSelectHtml — селектор роли в строке. Пустое значение = «снять роль».
  function roleSelectHtml(b) {
    var role = backendRole(b);
    var opts = [
      { v: '', t: T('monitor.role.none', '— не задана —') },
      { v: ROLE_STANDALONE, t: T('monitor.role.standalone', 'самодостаточный') },
      { v: ROLE_MAIN, t: T('monitor.role.main', 'главный (раскладка)') },
      { v: ROLE_DONOR, t: T('monitor.role.donor', 'донор (узел)') }
    ];
    return '<select class="backend-role-select" data-backend="' + esc(b.id) + '" ' +
      'title="' + esc(T('monitor.role.title', 'Роль бэкенда в раскладке слоёв')) + '" ' +
      'style="margin-top:4px;font-size:var(--font-size-xs);max-width:190px">' +
      opts.map(function (o) {
        return '<option value="' + o.v + '"' + (o.v === role ? ' selected' : '') + '>' + esc(o.t) + '</option>';
      }).join('') +
      '</select>';
  }

  // layoutChipHtml — состояние раскладки у этого бэкенда (из rpcLayout балансера).
  function layoutChipHtml(b) {
    var role = backendRole(b);
    var guard = b.rpcLayout || null;
    var link = b.rpcLink || null;
    var mbps = (link && link.mbps) ? link.mbps : 0;

    if (role === ROLE_MAIN) {
      if (guard && guard.degraded) {
        return '<div><span class="badge badge-orange" title="' + esc(guard.reason || '') + '">⚠ ' +
          esc(T('monitor.layout.degradedShort', 'раскладка не в полном составе')) + '</span></div>';
      }
      var on = guard ? !!guard.enabled : false;
      var servers = (guard && Array.isArray(guard.servers)) ? guard.servers : [];
      var txt = on
        ? T('monitor.layout.on', 'раскладка включена') + ' · ' + servers.length + ' ' + T('monitor.layout.nodesShort', 'узл.')
        : T('monitor.layout.off', 'раскладка выключена');
      var title = on
        ? T('monitor.layout.titleOn', 'Слои делятся между локальной картой и узлами') + ': ' + servers.join(', ')
        : T('monitor.layout.titleOff', 'Считает только локальная карта/CPU');
      if (mbps) title += ' · ' + T('monitor.layout.link', 'канал') + ': ' + mbps + ' ' + T('monitor.layout.mbps', 'Мбит/с');
      return '<div><span class="badge ' + (on ? 'badge-green' : 'badge-yellow') + '" title="' + esc(title) + '">' +
        (on ? '🧩 ' : '💻 ') + esc(txt) + '</span></div>';
    }
    if (role === ROLE_DONOR) {
      // R304 (2026-10-30): чип называет МАСТЕРА, а не просто «узел раскладки».
      // ЗАЧЕМ: донор существует только как память конкретного мастера («донор
      // мастера X»), и без имени мастера чип не отвечал на единственный вопрос,
      // который у оператора возникает у донора. Источник мастера — не новый
      // поиск и не второй fetch: donorFor из API (если сервер его отдал) либо
      // currentMainID() по уже загруженному списку — тот же расчёт, что и в
      // остальной панели.
      var masterID = donorMasterID(b);
      var chipText = T('monitor.layout.donorShort', 'узел раскладки');
      chipText += ' (' + (masterID
        ? T('monitor.layout.donorMaster', 'мастер') + ': ' + masterID
        : T('monitor.layout.donorNoMaster', 'мастер не назначен')) + ')';
      return '<div><span class="badge badge-blue" title="' +
        esc(T('monitor.layout.donorTitle', 'Этот узел отдаёт свои слои главному по RPC')) + '">' +
        '🔗 ' + esc(chipText) + '</span></div>';
    }
    return '<div><span class="badge" style="opacity:0.75" title="' +
      esc(T('monitor.layout.localTitle', 'Раскладка не используется — считает только эта машина')) + '">' +
      '💻 ' + esc(T('monitor.layout.localShort', 'локально')) + '</span></div>';
  }

  // donorMasterID — чей это донор ('' — мастер не назначен).
  // Порядок источников: donorFor из /api/v1/backends (новый балансер отвечает
  // на вопрос прямо) → currentMainID() по загруженному списку (старый балансер
  // этих полей не отдаёт). Второго источника ролей не заводим: роль по-прежнему
  // читается только из backendRole().
  function donorMasterID(b) {
    var df = (b && b.donorFor) || null;
    var id = df ? String(df.backendId || df.host || '') : '';
    return id || currentMainID();
  }

  // rowLayoutCell — ячейка «Раскладка (RPC)» для строки таблицы бэкендов.
  function rowLayoutCell(b) {
    if (!isLlamaBackend(b)) {
      return '<td><span style="font-size:var(--font-size-xs);color:var(--text-secondary)">—</span></td>';
    }
    return '<td>' + roleSelectHtml(b) + layoutChipHtml(b) + '</td>';
  }

  // bindRowSelectors — обработчики селекторов роли в строках таблицы.
  function bindRowSelectors(root) {
    if (!root || typeof root.querySelectorAll !== 'function') return 0;
    var sels = root.querySelectorAll('.backend-role-select');
    var bound = 0;
    Array.prototype.forEach.call(sels, function (sel) {
      if (sel.__roleBound) return;
      sel.__roleBound = true;
      bound++;
      sel.addEventListener('change', function () {
        var id = sel.getAttribute ? sel.getAttribute('data-backend') : sel.dataset.backend;
        applyRole(id, sel.value);
      });
    });
    return bound;
  }

  // applyRole — отправить роль одного бэкенда и обновить панель.
  function applyRole(backendID, role) {
    var api = window.MonitorApiXhr;
    if (!api || typeof api.setBackendRole !== 'function') {
      if (window.Utils && window.Utils.showToast) window.Utils.showToast('API недоступен', 'error');
      return Promise.resolve(false);
    }
    return api.setBackendRole(backendID, role).then(function (ok) {
      if (window.Utils && window.Utils.showToast) {
        window.Utils.showToast('Роль «' + (role || T('monitor.role.none', 'не задана')) + '»: ' + backendID,
          ok ? 'success' : 'error');
      }
      return load(true);
    }).catch(function () {
      if (window.Utils && window.Utils.showToast) window.Utils.showToast('Не удалось применить роль', 'error');
      return false;
    });
  }

  // ---- панель -------------------------------------------------------------

  // rolesForSelection — какие роли уйдут на балансер при таком выборе.
  // Всё, что не главный и не донор, ЯВНО помечается «самодостаточным»: иначе
  // прежняя раскладка осталась бы включённой у бэкенда, с которого роль сняли
  // (живой случай: узел был главным, потом стал донором — список узлов у него
  // оставался).
  function rolesForSelection(mainID, donorIDs) {
    var donors = donorIDs || [];
    var out = {};
    llamaBackends().forEach(function (b) {
      if (mainID && b.id === mainID) out[b.id] = ROLE_MAIN;
      else if (donors.indexOf(b.id) >= 0) out[b.id] = ROLE_DONOR;
      else out[b.id] = ROLE_STANDALONE;
    });
    return out;
  }

  function currentMainID() {
    var main = llamaBackends().filter(function (b) { return backendRole(b) === ROLE_MAIN; })[0];
    return main ? main.id : '';
  }

  function currentDonorIDs() {
    return llamaBackends().filter(function (b) { return backendRole(b) === ROLE_DONOR; })
      .map(function (b) { return b.id; });
  }

  function selectedMainID() {
    var el = document.getElementById('layoutMainSelect');
    return el ? String(el.value || '') : currentMainID();
  }

  function selectedDonorIDs() {
    var box = document.getElementById('layoutDonorList');
    if (!box || typeof box.querySelectorAll !== 'function') return currentDonorIDs();
    var out = [];
    Array.prototype.forEach.call(box.querySelectorAll('input[type=checkbox]'), function (cb) {
      if (cb.checked) out.push(cb.getAttribute('data-backend') || cb.value);
    });
    return out;
  }

  // renderPanel — нарисовать панель по текущему состоянию бэкендов.
  // экспорт чистого форматтера для тестов (без DOM)
  if (typeof window !== 'undefined') {
    window.LayoutPanelPure = { layersSummaryText: layersSummaryText };
  }

  function renderPanel() {
    var mainSel = document.getElementById('layoutMainSelect');
    var donorList = document.getElementById('layoutDonorList');
    var list = llamaBackends();

    if (mainSel) {
      var mainID = currentMainID();
      var opts = ['<option value="">' + esc(T('monitor.layout.noMain', '— не выбран —')) + '</option>'];
      list.forEach(function (b) {
        opts.push('<option value="' + esc(b.id) + '"' + (b.id === mainID ? ' selected' : '') + '>' +
          esc(b.id) + (backendRole(b) === ROLE_DONOR ? ' (' + esc(T('monitor.layout.wasDonor', 'сейчас донор')) + ')' : '') +
          '</option>');
      });
      mainSel.innerHTML = opts.join('');
    }
    if (donorList) {
      var donors = currentDonorIDs();
      var mainID2 = currentMainID();
      donorList.innerHTML = list.filter(function (b) { return b.id !== mainID2; }).map(function (b) {
        var id = 'layoutDonor_' + b.id;
        return '<label style="font-size:var(--font-size-xs);display:flex;gap:4px;align-items:center" for="' + esc(id) + '">' +
          '<input type="checkbox" id="' + esc(id) + '" data-backend="' + esc(b.id) + '"' +
          (donors.indexOf(b.id) >= 0 ? ' checked' : '') + '> ' + esc(b.id) + '</label>';
      }).join('') || '<span style="font-size:var(--font-size-xs);color:var(--text-secondary)">' +
        esc(T('monitor.layout.noLlama', 'нет llama.cpp-бэкендов')) + '</span>';
    }

    var main = list.filter(function (b) { return b.id === currentMainID(); })[0] || null;
    var status = document.getElementById('layoutStatus');
    if (status) status.innerHTML = statusHtml(main);
    var badge = document.getElementById('layoutStateBadge');
    if (badge) badge.textContent = badgeText(main);
    return true;
  }

  function badgeText(main) {
    if (!main) return T('monitor.layout.badgeOff', 'выключена');
    var guard = main.rpcLayout || null;
    if (guard && guard.degraded) return T('monitor.layout.badgeDegraded', 'внимание');
    return (guard && guard.enabled) ? T('monitor.layout.badgeOn', 'включена') : T('monitor.layout.badgeOff', 'выключена');
  }

  function statusHtml(main) {
    if (!main) {
      return esc(T('monitor.layout.statusNoMain',
        'Главный не выбран: каждый бэкенд считает модель сам, раскладка слоёв не используется.'));
    }
    var guard = main.rpcLayout || null;
    var link = main.rpcLink || null;
    var parts = [];
    if (guard && guard.enabled) {
      var servers = (guard.servers || []).join(', ') || '—';
      parts.push('<span class="badge badge-green">' + esc(T('monitor.layout.statusOn', 'раскладка включена')) + '</span>');
      parts.push(esc(T('monitor.layout.statusNodes', 'узлы')) + ': <strong>' + esc(servers) + '</strong>');
    } else {
      parts.push('<span class="badge badge-yellow">' + esc(T('monitor.layout.statusOff', 'раскладка выключена')) + '</span>');
    }
    if (guard && guard.degraded && guard.reason) {
      // R238d: reason приходит от балансера уже строкой и на языке ЛОГОВ (это
      // отдельная настройка). В видимом поле показываем переведённый короткий
      // статус, а точную причину оставляем в title (тултип) — иначе в EN-UI
      // висел русский текст сервера.
      parts.push('<span class="badge badge-orange" title="' + esc(guard.reason) + '">⚠ ' +
        esc(T('monitor.layout.degradedShort', 'раскладка не в полном составе')) + '</span>');
    }
    // R204 (2026-10-21): НАГЛЯДНОЕ РАСПРЕДЕЛЕНИЕ СЛОЁВ.
  //
  // ЖАЛОБА ОПЕРАТОРА: «нигде не видно, если раскладка применяется — на сколько слоёв
  // и у каких доноров». Данные уже есть у воркера: /api/models → device_layers_exact
  // (CPU/CUDA0/RPC0 с числом слоёв и адресом узла). Показываем строкой под статусом.
  if (guard && guard.enabled) {
    parts.push('<span id="layoutLayers">' + esc(T('monitor.layout.layersLoading', 'слои: …')) + '</span>');
    scheduleLayersLoad(main);
  }

  if (link && link.mbps) {
      parts.push(esc(T('monitor.layout.statusLink', 'канал')) + ': <strong>' + link.mbps + ' ' +
        esc(T('monitor.layout.mbps', 'Мбит/с')) + '</strong>' +
        (link.measured_at ? ' (' + esc(link.measured_at) + ')' : ''));
    }
    return parts.join(' · ');
  }

  // scheduleLayersLoad — подтянуть device_layers_exact у главного и отрисовать.
  //
  // Источник — прокси балансера к воркеру: GET /api/v1/gguf/backends/{id}/proxy/api/models.
  // Кэш 10 секунд: панель перерисовывается часто, а данные меняются только при загрузке.
  var layersCache = {};
  var layersInFlight = false;
  function scheduleLayersLoad(main) {
    if (!main || !main.id || layersInFlight) return;
    var cached = layersCache[main.id];
    if (cached && (Date.now() - cached.at) < 10000) { renderLayers(cached.data); return; }
    var base = (window.WEBUI_CONFIG && (window.WEBUI_CONFIG.API_BASE || window.WEBUI_CONFIG.apiBase)) || '';
    var url = String(base).replace(/\/+$/, '') + '/api/v1/gguf/backends/' + encodeURIComponent(main.id) + '/proxy/api/models';
    layersInFlight = true;
    fetch(url, { credentials: 'same-origin' })
      .then(function (r) { return r.json(); })
      .then(function (data) {
        layersCache[main.id] = { at: Date.now(), data: data };
        renderLayers(data);
      })
      .catch(function () {
        var el = document.getElementById('layoutLayers');
        if (el) el.textContent = T('monitor.layout.layersUnknown', 'слои: нет данных');
      })
      .finally(function () { layersInFlight = false; });
  }

  // renderLayers — «CUDA0 23 · RPC0 19 (192.0.2.10:50052) · CPU 1» + узлы по именам.
  // layersSummaryText — чистое форматирование распределения слоёв (тестируется отдельно).
  function layersSummaryText(target, t) {
    var parts = [];
    var rpcTotal = 0, localTotal = 0;
    (target || []).forEach(function (d) {
      var name = d.device || '?';
      var n = d.count || 0;
      var entry = name + ' ' + n;
      if (name.indexOf('RPC') === 0) { rpcTotal += n; if (d.node) entry += ' (' + d.node + ')'; }
      else if (name.indexOf('CUDA') === 0) { localTotal += n; }
      parts.push(entry);
    });
    return t('monitor.layout.layersSummary', 'слои') + ': ' + parts.join(' · ') +
      ' — ' + t('monitor.layout.layersSplit', 'локально') + ' ' + localTotal + ' / ' +
      t('monitor.layout.layersRemote', 'на узлах') + ' ' + rpcTotal;
  }

  function renderLayers(data) {
    var el = document.getElementById('layoutLayers');
    if (!el) return;
    var models = (data && data.models) || [];
    var target = null;
    for (var i = 0; i < models.length; i++) {
      var dl = models[i] && models[i].device_layers_exact;
      if (dl && dl.length) { target = dl; break; }
    }
    if (!target) { el.textContent = T('monitor.layout.layersUnknown', 'слои: нет данных'); return; }
    el.textContent = layersSummaryText(target, T);
  }

  // load — подтянуть бэкенды (role/rpcLayout/rpcLink) и перерисовать панель.
  function load(force) {
    var api = window.MonitorApiXhr;
    if (!api || typeof api.fetchBackends !== 'function') return Promise.resolve(false);
    var now = Date.now();
    if (!force && state.loading) return Promise.resolve(false);
    if (!force && state.lastLoadAt && (now - state.lastLoadAt) < REFRESH_THROTTLE_MS) return Promise.resolve(false);
    state.loading = true;
    return api.fetchBackends().then(function (data) {
      state.backends = (data && data.backends) || [];
      state.lastLoadAt = Date.now();
      state.loading = false;
      renderPanel();
      return true;
    }).catch(function () {
      state.loading = false;
      return false;
    });
  }

  // applySelection — применить выбор панели: главный + доноры, остальные —
  // самодостаточные. Последовательно, чтобы балансер применил роли по очереди
  // (главному он сам присылает список живых узлов).
  function applySelection() {
    var api = window.MonitorApiXhr;
    if (!api || typeof api.setBackendRole !== 'function') return Promise.resolve(false);
    var roles = rolesForSelection(selectedMainID(), selectedDonorIDs());
    var ids = Object.keys(roles);
    var chain = Promise.resolve(true);
    ids.forEach(function (id) {
      chain = chain.then(function (ok) {
        return api.setBackendRole(id, roles[id]).then(function (r) { return ok && r; });
      });
    });
    return chain.then(function (ok) {
      if (window.Utils && window.Utils.showToast) {
        window.Utils.showToast(ok
          ? T('monitor.layout.applied', 'Раскладка применена')
          : T('monitor.layout.applyFailed', 'Не все роли применились — смотри лог балансера'), ok ? 'success' : 'error');
      }
      return load(true);
    });
  }

  // setAllLocal — «только локально»: всем llama.cpp-бэкендам роль
  // «самодостаточный». Балансер сам погасит у них раскладку (rpcEnabled=false).
  function setAllLocal() {
    var api = window.MonitorApiXhr;
    if (!api || typeof api.setBackendRole !== 'function') return Promise.resolve(false);
    var ids = llamaBackends().map(function (b) { return b.id; });
    var chain = Promise.resolve(true);
    ids.forEach(function (id) {
      chain = chain.then(function (ok) {
        return api.setBackendRole(id, ROLE_STANDALONE).then(function (r) { return ok && r; });
      });
    });
    return chain.then(function (ok) {
      if (window.Utils && window.Utils.showToast) {
        window.Utils.showToast(ok ? T('monitor.layout.localApplied', 'Раскладка выключена у всех бэкендов')
          : T('monitor.layout.applyFailed', 'Не все роли применились — смотри лог балансера'), ok ? 'success' : 'error');
      }
      return load(true);
    });
  }

  function bindPanel() {
    var apply = document.getElementById('layoutApplyBtn');
    if (apply) apply.addEventListener('click', function () { applySelection(); });
    var local = document.getElementById('layoutLocalBtn');
    if (local) local.addEventListener('click', function () { setAllLocal(); });
    bindDonorOwnCopies();
  }

  // ---- исключение R304: «донор может держать свои копии моделей» -----------
  //
  // ЧТО БЫЛО НЕ ТАК. Флаг LB_DONOR_ALLOW_OWN_COPIES (исключение из доктрины
  // «донор держит только память мастера») жил ТОЛЬКО в общем списке горячих
  // настроек на странице «Настройки» — среди 17 строк и вдали от ролей
  // раскладки. Оператор искал его там, где назначает главного и доноров, не
  // находил, правил env и перезапускал стенд — при том что флаг горячий и
  // применяется сразу.
  //
  // ПОЧЕМУ ЗНАЧЕНИЕ ЗДЕСЬ НЕ ХРАНИТСЯ. Владелец реестра — единственный модуль
  // js/modules/settings-registry.js (он же владеет состоянием и запросами
  // GET/PUT/POST /api/v1/settings). Панель зовёт его get()/set()/reset() и
  // перерисовывается по его состоянию: вторая копия «что действует» разошлась
  // бы после первого же сброса (это ровно тот дефект, который убирал этап 3).
  var DONOR_OWN_COPIES = 'LB_DONOR_ALLOW_OWN_COPIES';
  var donorOwnCopiesBusy = false;
  var donorOwnCopiesRequested = false;

  function settingsRegistry() {
    var SR = window.SettingsRegistry;
    if (!SR || typeof SR.get !== 'function' || typeof SR.set !== 'function') return null;
    return SR;
  }

  // adminNow — тот же признак роли, что у auth.js (R203/R222). Нет auth.js
  // (демо-режим, старый образ) — ролей в клиенте нет, ведём себя как раньше:
  // сервер всё равно проверит права.
  function adminNow() {
    var A = window.Auth;
    if (A && typeof A.isAdmin === 'function') {
      try { return !!A.isAdmin(); } catch (e) { /* auth сломан — не блокируем */ }
    }
    return true;
  }

  function byId(id) {
    if (typeof document === 'undefined' || typeof document.getElementById !== 'function') return null;
    return document.getElementById(id);
  }

  function setDonorOwnCopiesMsg(text, kind) {
    var el = byId('layoutDonorOwnCopiesMsg');
    if (!el) return;
    if (el.textContent !== undefined) el.textContent = text || '';
    if (el.style) el.style.color = kind === 'error' ? 'var(--danger)' : 'var(--text-secondary)';
  }

  // renderDonorOwnCopies — нарисовать состояние ИЗ реестра (своего снимка нет).
  function renderDonorOwnCopies() {
    var SR = settingsRegistry();
    var item = SR ? SR.get(DONOR_OWN_COPIES) : null;
    var admin = adminNow();
    var badge = byId('layoutDonorOwnCopiesState');
    var cb = byId('layoutDonorOwnCopies');
    var resetBtn = byId('layoutDonorOwnCopiesReset');

    if (badge) {
      var cls = 'badge';
      var text = T('monitor.layout.donorOwnCopies.missing', 'нет строки в реестре');
      if (item && typeof SR.sourceLabel === 'function' && typeof SR.badgeClass === 'function') {
        // Подписи источника и классы бейджей — те же, что на странице настроек:
        // один реестр, одна терминология («по умолчанию» / «из env» / «задано в панели»).
        cls = SR.badgeClass(item.source);
        text = SR.sourceLabel(item.source) + ': ' +
          (item.effective ? T('settings.hot.bool_on', 'вкл') : T('settings.hot.bool_off', 'выкл'));
      }
      badge.className = cls;
      var desc = item && item.description ? String(item.description) : T('monitor.layout.donorOwnCopies.hint',
        'По умолчанию донор держит только память мастера.');
      badge.title = desc;
      if (badge.textContent !== undefined) badge.textContent = text;
    }
    if (cb) {
      cb.checked = !!(item && item.effective === 1);
      cb.disabled = !admin || donorOwnCopiesBusy || !item;
      cb.title = !admin
        ? T('monitor.layout.donorOwnCopies.adminOnly', 'Изменять может только администратор')
        : (item && item.description ? String(item.description) : '');
    }
    if (resetBtn) {
      // Сброс доступен только когда значение ЗАДАНО В ПАНЕЛИ (source=override):
      // иначе снимать нечего и оператор получил бы 400/403 вместо объяснения.
      var canReset = !!(item && item.source === 'override');
      resetBtn.disabled = !admin || donorOwnCopiesBusy || !canReset;
      resetBtn.title = !admin
        ? T('monitor.layout.donorOwnCopies.adminOnly', 'Изменять может только администратор')
        : (canReset ? T('settings.hot.reset_hint', 'Снять значение, заданное в панели')
          : T('settings.hot.reset_disabled', 'Сбрасывать нечего'));
    }
    return item;
  }

  // loadDonorOwnCopies — прочитать реестр тем же модулем, что и страница настроек.
  function loadDonorOwnCopies(force) {
    if (donorOwnCopiesRequested && !force) { return Promise.resolve(renderDonorOwnCopies()); }
    donorOwnCopiesRequested = true;
    var SR = settingsRegistry();
    if (!SR) {
      // Модуль реестра не подключён — честное «недоступно», а не живой на вид
      // тумблер, который ничего не меняет.
      setDonorOwnCopiesMsg(T('monitor.layout.donorOwnCopies.unavailable',
        'Реестр горячих настроек недоступен (старый балансер): значение меняется только через env и перезапуск'));
      return Promise.resolve(renderDonorOwnCopies());
    }
    return SR.load().then(function () {
      if (SR.state && (SR.state.unavailable || SR.state.error)) {
        setDonorOwnCopiesMsg(T('monitor.layout.donorOwnCopies.unavailable',
          'Реестр горячих настроек недоступен (старый балансер): значение меняется только через env и перезапуск'));
      } else {
        setDonorOwnCopiesMsg('');
      }
      return renderDonorOwnCopies();
    }).catch(function () {
      setDonorOwnCopiesMsg(T('monitor.layout.donorOwnCopies.unavailable',
        'Реестр горячих настроек недоступен (старый балансер): значение меняется только через env и перезапуск'));
      return renderDonorOwnCopies();
    });
  }

  /**
   * setDonorOwnCopies(enabled) — записать исключение (тот же PUT, что у страницы
   * «Настройки»: value = 1/0 числом, тип bool знает только реестр).
   */
  function setDonorOwnCopies(enabled) {
    var SR = settingsRegistry();
    if (!SR) {
      setDonorOwnCopiesMsg(T('monitor.layout.donorOwnCopies.unavailable',
        'Реестр горячих настроек недоступен'), 'error');
      return Promise.resolve(false);
    }
    if (!adminNow()) {
      setDonorOwnCopiesMsg(T('monitor.layout.donorOwnCopies.adminOnly',
        'Изменять может только администратор'), 'error');
      renderDonorOwnCopies();
      return Promise.resolve(false);
    }
    donorOwnCopiesBusy = true;
    renderDonorOwnCopies();
    return SR.set(DONOR_OWN_COPIES, enabled ? 1 : 0).then(function (res) {
      donorOwnCopiesBusy = false;
      var ok = !!(res && res.ok);
      if (ok) {
        setDonorOwnCopiesMsg(T('monitor.layout.donorOwnCopies.saved', 'Применено без рестарта'));
        if (window.Utils && window.Utils.showToast) {
          window.Utils.showToast(T('monitor.layout.donorOwnCopies.saved', 'Применено без рестарта'), 'success');
        }
      } else {
        var reason = (res && res.error && (res.error.message || res.error)) || (res && res.reason) || '';
        setDonorOwnCopiesMsg(T('monitor.layout.donorOwnCopies.failed', 'Не применено') +
          (reason ? ': ' + reason : ''), 'error');
      }
      renderDonorOwnCopies();
      return ok;
    }).catch(function (err) {
      donorOwnCopiesBusy = false;
      setDonorOwnCopiesMsg(T('monitor.layout.donorOwnCopies.failed', 'Не применено') + ': ' +
        ((err && err.message) || err), 'error');
      renderDonorOwnCopies();
      return false;
    });
  }

  // resetDonorOwnCopies — снять override (POST /api/v1/settings/reset тем же модулем).
  function resetDonorOwnCopies() {
    var SR = settingsRegistry();
    if (!SR) {
      setDonorOwnCopiesMsg(T('monitor.layout.donorOwnCopies.unavailable',
        'Реестр горячих настроек недоступен'), 'error');
      return Promise.resolve(false);
    }
    if (!adminNow()) {
      setDonorOwnCopiesMsg(T('monitor.layout.donorOwnCopies.adminOnly',
        'Изменять может только администратор'), 'error');
      renderDonorOwnCopies();
      return Promise.resolve(false);
    }
    donorOwnCopiesBusy = true;
    renderDonorOwnCopies();
    return SR.reset(DONOR_OWN_COPIES).then(function (res) {
      donorOwnCopiesBusy = false;
      if (res && res.ok) {
        setDonorOwnCopiesMsg(T('monitor.layout.donorOwnCopies.resetDone',
          'Сброшено: снова действует env или значение по умолчанию'));
      } else if (res && res.reason === 'not-override') {
        setDonorOwnCopiesMsg(T('settings.hot.reset_disabled', 'Сбрасывать нечего'));
      } else {
        setDonorOwnCopiesMsg(T('monitor.layout.donorOwnCopies.failed', 'Не применено'), 'error');
      }
      renderDonorOwnCopies();
      return !!(res && res.ok);
    }).catch(function (err) {
      donorOwnCopiesBusy = false;
      setDonorOwnCopiesMsg(T('monitor.layout.donorOwnCopies.failed', 'Не применено') + ': ' +
        ((err && err.message) || err), 'error');
      renderDonorOwnCopies();
      return false;
    });
  }

  function bindDonorOwnCopies() {
    var cb = byId('layoutDonorOwnCopies');
    if (cb && !cb.__donorOwnCopiesBound) {
      cb.__donorOwnCopiesBound = true;
      if (typeof cb.addEventListener === 'function') {
        cb.addEventListener('change', function () { setDonorOwnCopies(!!cb.checked); });
      }
    }
    var resetBtn = byId('layoutDonorOwnCopiesReset');
    if (resetBtn && !resetBtn.__donorOwnCopiesBound) {
      resetBtn.__donorOwnCopiesBound = true;
      if (typeof resetBtn.addEventListener === 'function') {
        resetBtn.addEventListener('click', function () { resetDonorOwnCopies(); });
      }
    }
    // Роль известна только после /auth/me: до этого adminNow() = «нет auth.js».
    // Перерисовываем по готовности входа, иначе viewer видел бы живой тумблер.
    var ready = window.__authReady;
    if (typeof ready === 'function') {
      var p = null;
      try { p = ready(); } catch (e) { p = null; }
      if (p && typeof p.then === 'function') { p.then(renderDonorOwnCopies).catch(renderDonorOwnCopies); }
    }
    return loadDonorOwnCopies();
  }

  window.MonitorLayout = {
    load: load,
    renderPanel: renderPanel,
    rowLayoutCell: rowLayoutCell,
    bindRowSelectors: bindRowSelectors,
    applyRole: applyRole,
    applySelection: applySelection,
    setAllLocal: setAllLocal,
    bindPanel: bindPanel,
    // R317: исключение R304 в панели ролей (значение живёт в settings-registry.js).
    bindDonorOwnCopies: bindDonorOwnCopies,
    loadDonorOwnCopies: loadDonorOwnCopies,
    renderDonorOwnCopies: renderDonorOwnCopies,
    setDonorOwnCopies: setDonorOwnCopies,
    resetDonorOwnCopies: resetDonorOwnCopies,
    donorOwnCopiesName: DONOR_OWN_COPIES,
    rolesForSelection: rolesForSelection,
    isLlamaBackend: isLlamaBackend,
    roleLabel: roleLabel,
    _state: state
  };

  // R118.4: кнопки панели + первый опрос. Скрипты подключены с defer, поэтому
  // DOM к этому моменту уже разобран.
  if (typeof document !== 'undefined') {
    if (document.readyState === 'loading') {
      document.addEventListener('DOMContentLoaded', function () { bindPanel(); load(true); });
    } else {
      bindPanel();
      load(true);
    }
  }
})();

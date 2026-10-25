// js/monitor/canvas-topology.js — Topology canvas (vizCanvas)

(function() {
  'use strict';

  var MA = window.MonitorApp;

  var cv = document.getElementById('vizCanvas'), cx = cv.getContext('2d');
  var ttEl = document.getElementById('topoTooltip');
  var af = 0;

  // Pan state
  var panning = false;
  var panStartX = 0, panStartY = 0;
  var panViewportStart = { offsetX: 0, offsetY: 0 };
  // R198 (2026-10-20): последняя позиция курсора при перетаскивании. Нужна, чтобы
  // смещение применялось В КАЖДОМ КАДРЕ отрисовки (см. drawTopo): если какой-то код
  // сбрасывает MA.viewport.offsetX/Y, панорамирование «не едет», хотя panning=true.
  var panLastX = 0, panLastY = 0;

  // layoutCanvas — «поле расширяется под данные».
  //
  // R92 (2026-10-09). Раньше высота полотна всегда равнялась высоте окна
  // (r.height - 52), а раскладка подгоняла число плиток под неё и лишние прятала
  // за «+N». Теперь наоборот: раскладка требует высоту под ВСЕ узлы с полным
  // шагом (MonitorApp.columnNeedH), а полотно растёт до неё, и полоса топологии
  // прокручивается (CSS .topo-viewport{overflow-y:auto}).
  //
  // Предохранители: MA.GEOM.maxCanvasH (память под bitmap) и maxNodes — только
  // для патологического кластера; до них все узлы видны.
  var lastCanvasH = 0;
  var userScrolled = false;
  var programmaticScroll = false;

  function applyScrollGuard(host) {
    if (!host || host.__topoScrollGuard) return;
    host.__topoScrollGuard = true;
    host.addEventListener('scroll', function() {
      if (programmaticScroll) return;
      userScrolled = true;
    }, { passive: true });
  }

  function layoutCanvas() {
    var host = cv.parentElement;
    if (!host) return;
    applyScrollGuard(host);
    var r = host.getBoundingClientRect();
    var visH = Math.max(120, Math.round(r.height));
    var visW = Math.max(160, Math.round(r.width));
    // R238: на узком экране колонки и блок балансера сжимаются, иначе плитки
    // клиентов/бэкендов наезжают на балансер (см. GEOM.applyResponsive).
    MA.GEOM.applyResponsive(visW);
    var needH = Math.max(MA.columnNeedH('session'), MA.columnNeedH('backend'));
    var h = Math.max(visH, Math.min(needH, MA.GEOM.maxCanvasH));

    cv.width = visW;
    cv.height = h;
    cv.style.height = h + 'px';
    MA.topo.w = visW;
    MA.topo.h = h;

    // Конвейер — оверлей поверх той же полосы: держим его в тех же координатах,
    // иначе при выросшем полотне bitmap и CSS-бокс разъезжаются и частицы
    // «уезжают» (масштабирование канваса по CSS).
    var ccv = document.getElementById('conveyorCanvas');
    if (ccv) {
      ccv.style.width = visW + 'px';
      ccv.style.height = h + 'px';
    }

    // Хаб (центр полотна) должен оставаться в центре видимой области, иначе при
    // росте числа узлов он уезжает за экран.
    var max = Math.max(0, h - visH);
    if (max > 0 && (!userScrolled || lastCanvasH !== h)) {
      programmaticScroll = true;
      host.scrollTop = max / 2;
      var flag = function() { programmaticScroll = false; };
      if (typeof setTimeout === 'function') setTimeout(flag, 0); else flag();
    }
    lastCanvasH = h;
  }
  function rsz() { layoutCanvas(); }
  window.addEventListener('resize', rsz);
  rsz();

  function updateTopology(bk, ss, q, recentClients, warmingUpModels, recentRequests, recentResponses) {
    var prevSess = MA.topo.sessions ? MA.topo.sessions.length : 0;
    var prevBack = MA.topo.backends ? MA.topo.backends.length : 0;
    MA.topo.backends = bk;
    // R91 (2026-10-08): сначала самые СВЕЖИЕ клиенты. Когда узлов больше, чем
    // влезает в колонку, не поместившиеся не рисуются (вместо них счётчик «+N»),
    // поэтому порядок массива решает, кого оператор увидит: раньше это был
    // порядок ответа API, и на картинке мог оказаться случайный набор.
    MA.topo.sessions = [].concat(ss || []).sort(function(a, b) {
      var ta = a && a.lastRequestAt ? new Date(a.lastRequestAt).getTime() : 0;
      var tb = b && b.lastRequestAt ? new Date(b.lastRequestAt).getTime() : 0;
      return tb - ta;
    });
    MA.topo.queue = q;
    MA.topo.recentClients = recentClients || [];
    MA.topo.warmingUpModels = warmingUpModels || [];
    // R238: демо отдаёт точные recentRequests/recentResponses — по ним конвейер
    // рисует запрос и ОТВЕТ (backend → балансер → клиент). Наличие этих массивов
    // (даже пустых) включает «фактовый» режим и отключает случайные частицы
    // топологии, чтобы на картинке не было двух конкурирующих потоков.
    MA.topo.recentRequests = recentRequests || [];
    MA.topo.recentResponses = recentResponses || [];
    MA.topo.demoLifecycle = !!(recentRequests || recentResponses);
    // Сменился «состав» кластера — снова держим хаб по центру (оператор не
    // пострадал: его собственная прокрутка сбрасывается только на смене состава).
    if (prevSess !== MA.topo.sessions.length || prevBack !== MA.topo.backends.length) {
      userScrolled = false;
    }
    layoutCanvas();
  }
  window.updateTopology = updateTopology;

  // visLimit — сколько узлов этого типа реально рисуется (остальные — «+N»).
  function visLimit(type, len) {
    var L = MA.nLayout(type);
    return Math.min(len, L.visible);
  }

  // drawOverflowChip — счётчик не поместившихся узлов. Молча прятать их нельзя:
  // оператор должен видеть, что клиентов больше, чем показано.
  //
  // R95: цвета берём из палитры темы (аргумент lt больше не нужен — палитра
  // вычисляется внутри, иначе на светлых темах плашка сливалась с фоном).
  function drawOverflowChip(type, x, y, hidden, lt) {
    if (hidden <= 0) return;
    var pal = themePal();
    var label = '+ ' + hidden;
    cx.font = 'bold 10px ' + MA.vF();
    cx.textAlign = 'left';
    var tw = cx.measureText(label).width;
    cx.fillStyle = pal.gridStrong;
    MA.rr(cx, x, y - 9, tw + 16, 18, 9); cx.fill();
    cx.fillStyle = pal.textMuted;
    cx.fillText(label, x + 8, y + 4);
  }

  // --- Pan: drag on empty canvas area ---

  function hitTestNodes(worldX, worldY) {
    var w = MA.topo.w, cX = w / 2, cY = (MA.topo.h || 0) / 2;   // R199: cY был не объявлен (ReferenceError при mousedown)
    // R91: только РИСУЕМЫЕ узлы — у не поместившихся нет координат на картинке
    // (их нет и в подсказке), иначе клик в пустом месте считался бы попаданием.
    for (var i = visLimit('session', MA.topo.sessions.length) - 1; i >= 0; i--) {
      var y = MA.nY('session', i);
      if (worldX >= 12 && worldX <= MA.GEOM.clientX - 8 && worldY >= y - 22 && worldY <= y + 22) return true;
    }
    for (var i = visLimit('backend', MA.topo.backends.length) - 1; i >= 0; i--) {
      var y = MA.nY('backend', i);
      if (worldX >= w - MA.GEOM.backendMargin && worldX <= w - 12 && worldY >= y - 26 && worldY <= y + 26) return true;
    }
    // Check balancer box
    var bw = MA.GEOM.bw, bh = MA.GEOM.bh, bx = cX - bw / 2, by = cY - bh / 2;
    if (worldX >= bx - 4 && worldX <= bx + bw + 4 && worldY >= by - 4 && worldY <= by + bh + 4) return true;
    return false;
  }

  // R197 (2026-10-20): ПЕРЕТАСКИВАНИЕ ПОЛЯ МЫШЬЮ.
  //
  // ЖАЛОБА: «перемещение по полю зажатой кнопкой мыши не работает, только зум».
  // Причина: mousedown висел на самом канвасе, а событие перехватывали элементы
  // над ним (слой конвейера/обёртка вьюпорта) — panning не включался. Зум работал,
  // потому что wheel всплывает до канваса.
  //
  // Теперь слушаем ОБЁРТКУ (.topo-viewport) с фолбэком на канвас; на время
  // перетаскивания выключаем выделение текста, иначе браузер начинает drag-select.
  var panHost = cv.closest('.topo-viewport') || cv;
  panHost.style.cursor = 'grab';
  panHost.addEventListener('mousedown', function(e) {
    if (e.button !== 0) return;
    if (e.target && e.target.closest && e.target.closest('button, a, input, select, summary')) return;
    var rect = cv.getBoundingClientRect();
    var mx = e.clientX - rect.left;
    var my = e.clientY - rect.top;
    var wPos = MA.toWorld(mx, my);
    // If user clicked on a node, don't pan — let tooltip handle it
    // R199 (2026-10-20): НЕ ОТКАЗЫВАЕМСЯ ОТ ПАНОРАМИРОВАНИЯ НАД УЗЛАМИ.
    // Раньше клик по узлу/линии молча съедался (return), и создавалось впечатление,
    // что «поле не тянется». Подсказка узла живёт на mousemove, ей mousedown не нужен.
    // Отлажено в браузере (scripts/webui-e2e/debug-pan.js): offsets не менялись,
    // потому что обработчик выходил здесь.
    e.preventDefault();
    panning = true;
    panStartX = e.clientX;
    panStartY = e.clientY;
    panViewportStart.offsetX = MA.viewport.offsetX;
    panViewportStart.offsetY = MA.viewport.offsetY;
    panHost.style.cursor = 'grabbing';
    document.body.style.userSelect = 'none';
    document.body.style.webkitUserSelect = 'none';
  });

  window.addEventListener('mousemove', function(e) {
    if (!panning) return;
    var dx = e.clientX - panStartX;
    var dy = e.clientY - panStartY;
    MA.viewport.offsetX = panViewportStart.offsetX + dx;
    MA.viewport.offsetY = panViewportStart.offsetY + dy;
  });

  window.addEventListener('mouseup', function(e) {
    if (panning) {
      panning = false;
      panHost.style.cursor = 'grab';
      document.body.style.userSelect = '';
      document.body.style.webkitUserSelect = '';
    }
  });

  // --- Zoom: mouse wheel ---

  cv.addEventListener('wheel', function(e) {
    e.preventDefault();
    var rect = cv.getBoundingClientRect();
    var mx = e.clientX - rect.left;
    var my = e.clientY - rect.top;
    var vp = MA.viewport;
    // World position under cursor
    var worldX = (mx - vp.offsetX) / vp.scale;
    var worldY = (my - vp.offsetY) / vp.scale;
    // Determine zoom direction
    var factor = e.deltaY > 0 ? 1 / 1.1 : 1.1;
    var newScale = Math.min(vp.maxScale, Math.max(vp.minScale, vp.scale * factor));
    // Adjust offset so the world point stays under the cursor
    vp.offsetX = mx - worldX * newScale;
    vp.offsetY = my - worldY * newScale;
    vp.scale = newScale;
  }, { passive: false });

  // themePal — палитра канваса ИЗ ТЕКУЩЕЙ ТЕМЫ (R95).
  //
  // ЖАЛОБА: «в мониторе при теме mint не видно сетки и подписей в блоках».
  // ПРИЧИНА: здесь была бинарная проверка `data-theme === 'light'`, поэтому все
  // остальные темы (включая светлую mint) рисовались тёмной палитрой:
  // сетка rgba(255,255,255,0.03) на фоне #f5f7f6 даёт контраст 1.0, текст
  // #e2e8f0 — 1.15. Теперь цвета (сетка, текст, подписи в плитках, плашки)
  // считаются по ФАКТИЧЕСКОМУ фону темы, см. MonitorApp.themePalette.
  function themePal() {
    if (MA.themePalette) return MA.themePalette();
    return MA._darkPalette ? MA._darkPalette() : { isLight: false, grid: 'rgba(255,255,255,0.07)', gridStrong: 'rgba(255,255,255,0.14)', text: '#e2e8f0', textMuted: '#b0b7c4', tileText: '#fff', barBg: 'rgba(255,255,255,0.12)' };
  }

  // R238d: статус бэкенда приходит кодом (healthy/offline/...); в тултипе
  // показываем перевод, неизвестный код — как есть.
  function statusLabel(s) {
    if (!s) { return ''; }
    var k = 'monitor.backendStatus.' + s;
    var v = MA.T(k);
    return (v && v !== k) ? v : s;
  }

  // Tooltip
  cv.addEventListener('mousemove', function(e) {
    var rect = cv.getBoundingClientRect();
    var mx = e.clientX - rect.left;
    var my = e.clientY - rect.top;
    var wp = MA.toWorld(mx, my);
    var wx = wp.x, wy = wp.y;
    var w = MA.topo.w, h = MA.topo.h, cX = w / 2, cY = h / 2;
    var found = null;

    // Check client boxes (left side) — только нарисованные (R91).
    for (var i = visLimit('session', MA.topo.sessions.length) - 1; i >= 0; i--) {
      var s = MA.topo.sessions[i];
      var y = MA.nY('session', i);
      if (wx >= 12 && wx <= MA.GEOM.clientX - 8 && wy >= y - 22 && wy <= y + 22) {
        found = { type: 'client', session: s };
        break;
      }
    }
    // Check backend boxes (right side) — только нарисованные (R91).
    if (!found) {
      for (var i = visLimit('backend', MA.topo.backends.length) - 1; i >= 0; i--) {
        var b = MA.topo.backends[i];
        var y = MA.nY('backend', i);
        if (wx >= w - MA.GEOM.backendMargin && wx <= w - 12 && wy >= y - 26 && wy <= y + 26) {
          found = { type: 'backend', backend: b };
          break;
        }
      }
    }
    // Check balancer box
    if (!found) {
      var bw = MA.GEOM.bw, bh = MA.GEOM.bh, bx = cX - bw / 2, by = cY - bh / 2;
      if (wx >= bx - 4 && wx <= bx + bw + 4 && wy >= by - 4 && wy <= by + bh + 4) {
        var pend = MA.topo.queue.pending_count || 0;
        var proc = MA.topo.queue.processing_count || 0;
        found = { type: 'balancer', pending: pend, processing: proc, total: (MA.topo.queue.all || []).length };
      }
    }

    if (found) {
      var html = '';
      if (found.type === 'client') {
        var s = found.session;
        var idl = s.lastRequestAt ? MA.fmtDur(Date.now() - new Date(s.lastRequestAt).getTime()) : '-';
        html = '<div><strong>' + MA.esc(s.clientName || '?') + '</strong></div>'
          + '<div style="color:var(--text-secondary)">IP: <span style="color:' + MA.ipColor(s.clientIP) + ';font-weight:600">' + MA.esc(s.clientIP || '-') + '</span></div>'
          + '<div style="color:var(--text-secondary)">' + MA.T('monitor.table.model') + ': ' + MA.esc(s.model || '-') + (MA.isCloudModel(s.model) ? ' ☁️' : '') + '</div>'
          + '<div style="color:var(--text-secondary)">' + MA.T('monitor.table.backend') + ': ' + MA.esc(s.backendId || MA.T('common.waiting')) + '</div>'
          + '<div style="color:var(--text-secondary)">' + MA.T('monitor.table.requests') + ': <strong>' + (s.requestCount || 0) + '</strong> | ' + MA.T('monitor.table.idle') + ': ' + idl + '</div>';

      } else if (found.type === 'backend') {
        var b = found.backend;
        var mr = b.maxConcurrentRequests || 10, a = b.activeRequests || 0;
        var gu = (b.gpu && b.gpu.usagePercent != null) ? b.gpu.usagePercent : 0;
        var vu = (b.vram && b.vram.usagePercent != null) ? b.vram.usagePercent : 0;
        var vtu = b.vram ? b.vram.totalGB : 0, vus = b.vram ? b.vram.usedGB : 0;
        var cu = (b.system && b.system.cpuUsagePercent != null) ? b.system.cpuUsagePercent : 0;
        var ru = (b.memoryUsagePercent != null) ? b.memoryUsagePercent : ((b.system && b.system.memoryUsagePercent != null) ? b.system.memoryUsagePercent : 0);
        var sc = (b.score || 0).toFixed(2);
        html = '<div><strong>' + MA.esc(b.id) + '</strong> <span style="font-size:var(--font-size-xs);color:' + (b.status === 'active' || b.status === 'ready' ? 'var(--success)' : 'var(--danger)') + '">' + MA.esc(statusLabel(b.status)) + '</span></div>'
          + '<div style="color:var(--text-secondary)">GPU: ' + gu.toFixed(0) + '% | VRAM: ' + vu.toFixed(0) + '% (' + vus.toFixed(1) + '/' + vtu.toFixed(0) + 'G)</div>'
          + '<div style="color:var(--text-secondary)">CPU: ' + cu.toFixed(0) + '% | RAM: ' + ru.toFixed(0) + '%</div>'
          + '<div style="color:var(--text-secondary)">' + MA.T('monitor.table.active') + ': ' + a + '/' + mr + ' | ' + MA.T('monitor.table.score') + ': ' + sc + '</div>'
          + '<div style="color:var(--text-secondary)">' + MA.T('monitor.table.models') + ': ' + ((b.models || []).length ? (b.models || []).slice(0, 5).join(', ') : '—') + '</div>'
          + '<div style="color:var(--text-secondary)">RPS: ' + ((b.ollama && b.ollama.requestsPerSecond != null) ? b.ollama.requestsPerSecond.toFixed(1) : '-') + '</div>';
      } else if (found.type === 'balancer') {
        html = '<div><strong>' + MA.T('monitor.canvas.balancer') + '</strong></div>'
          + '<div style="color:var(--text-secondary)">' + MA.T('monitor.canvas.queue') + ' <strong>' + found.total + '</strong> (' + MA.T('monitor.stats.pending') + ': ' + found.pending + ', ' + MA.T('monitor.stats.processing') + ': ' + found.processing + ')</div>'
          + '<div style="color:var(--text-secondary)">' + MA.T('monitor.stats.backends') + ': ' + MA.topo.backends.length + ' | ' + MA.T('monitor.stats.sessions') + ': ' + MA.topo.sessions.length + '</div>';

      }
      ttEl.innerHTML = html;
      ttEl.style.display = 'block';
      var ttLeft = e.clientX + 16;
      var ttTop = e.clientY - 10;
      var ttWidth = ttEl.offsetWidth || 300;
      var ttHeight = ttEl.offsetHeight || 100;
      if (ttLeft + ttWidth > window.innerWidth - 10) ttLeft = e.clientX - ttWidth - 16;
      if (ttTop + ttHeight > window.innerHeight - 10) ttTop = e.clientY - ttHeight - 10;
      if (ttLeft < 10) ttLeft = 10;
      if (ttTop < 10) ttTop = 10;
      ttEl.style.left = ttLeft + 'px';
      ttEl.style.top = ttTop + 'px';
    } else {
      ttEl.style.display = 'none';
    }
  });
  cv.addEventListener('mouseleave', function() { ttEl.style.display = 'none'; });

  function spawnParticle(path, type, model) {
    MA.particles.push({
      path: path,
      x: path[0].x, y: path[0].y,
      createdAt: Date.now(),
      duration: type === 'pending' ? 2500 : 1800,
      type: type,
      model: model || '',
      alive: true
    });
  }

  var lastSpawnTime = 0;
  var SPAWN_INTERVAL_MS = 200;

  function sp() {
    // R238: в демо поток рисует canvas-conveyor по точным событиям
    // (recentRequests/recentResponses), а случайные частицы топологии дублировали
    // бы его: жёлтые точки «ожидания» выглядели как ответы, которых на самом деле
    // не было. В живом режиме (массивов нет) поведение прежнее.
    if (MA.topo.demoLifecycle) return;
    var cX = MA.topo.w / 2, cY = MA.topo.h / 2;
    var now = Date.now();

    // Троттлинг: спавним частицы не чаще чем раз в 200 мс, независимо от FPS
    if (now - lastSpawnTime < SPAWN_INTERVAL_MS) return;
    lastSpawnTime = now;

    var bw = MA.GEOM.bw, bh = MA.GEOM.bh;
    var balInX = cX - bw / 2, balOutX = cX + bw / 2;
    var byTop = cY - bh / 2, byBot = cY + bh / 2;

    // R200c (2026-10-20): частицы рождаются ЗДЕСЬ и считают цифры сами.
    // Блок попал в helper-функцию без activeReq/rpsSum в области видимости — отсюда
    // ReferenceError и отсутствие кадров. Теперь значения считаются локально.
    if (typeof spawnParticle === 'function' && MA.topo.sessions.length > 0) {
      var _a = 0, _r = 0;
      for (var _i = 0; _i < MA.topo.backends.length; _i++) {
        var _b = MA.topo.backends[_i];
        if (!_b) continue;
        _a += (_b.activeRequests || 0);
        var _rr = (_b.ollama && _b.ollama.requestsPerSecond) || _b.requestsPerSecond || 0;
        if (typeof _rr === 'number') _r += _rr;
      }
      var _rate = _r > 0 ? Math.min(_r, 6) : (_a > 0 ? Math.min(_a, 6) : 1);
      var _want = Math.min(8, Math.max(1, Math.round(_rate)));
      var _have = 0;
      for (var _p = 0; _p < MA.particles.length; _p++) { if (MA.particles[_p].type === 'request') _have++; }
      MA._r200budget = (MA._r200budget || 0) + (_r > 0 ? _r / 60 : 0.03);
      var _spawn = Math.floor(MA._r200budget);
      MA._r200budget -= _spawn;
      if (_have < _want && _spawn < 1) _spawn = 1;
      for (var _k = 0; _k < _spawn && _have < _want; _k++) {
        var _s = MA.topo.sessions[0];
        if (!_s) break;
        var _sy = MA.nY('session', 0);
        var _cx2 = (MA.topo.w || 0) / 2, _cy2 = (MA.topo.h || 0) / 2;
        var _tx = MA.GEOM.clientX + (_cx2 - MA.GEOM.bw / 2 - MA.GEOM.clientX) * 0.55;
        spawnParticle([{ x: MA.GEOM.clientX, y: _sy }, { x: _tx, y: _sy }, { x: _tx, y: _cy2 }, { x: _cx2 - MA.GEOM.bw / 2, y: _cy2 }], 'request', _s.model || '');
        _have++;
      }
    }
    // --- Request particles: client -> balancer (L-path via elbow)
    //
    // R92 (2026-10-09): вероятность подъёма зависит от числа «живых» сессий.
    // Раньше было жёсткое `Math.random() < 0.08` на сессию — при 1-2 активных
    // клиентах поток частиц выглядел пустым («частицы не рисуются»), хотя трафик
    // есть. Теперь при нескольких свежих сессиях частота растёт (потолок 0.5).
    var recentSessions = 0;
    for (var ci = 0; ci < visLimit('session', MA.topo.sessions.length); ci++) {
      var cs = MA.topo.sessions[ci];
      var cnm = (cs.clientName || '').toLowerCase();
      if (cnm.indexOf('monitor') >= 0 || cnm.indexOf('health') >= 0 || cnm.indexOf('kube') >= 0) continue;
      var cidle = cs.lastRequestAt ? (now - new Date(cs.lastRequestAt).getTime()) : 999999;
      if (cidle <= 30000) recentSessions++;
    }
    var reqProb = Math.min(0.08 * Math.max(1, recentSessions), 0.5);
    for (var pi = 0; pi < visLimit('session', MA.topo.sessions.length); pi++) {
      var s = MA.topo.sessions[pi];
      var cn = (s.clientName || '').toLowerCase();
      if (cn.indexOf('monitor') >= 0 || cn.indexOf('health') >= 0 || cn.indexOf('kube') >= 0) continue;
      var idleMs = s.lastRequestAt ? (now - new Date(s.lastRequestAt).getTime()) : 999999;
      if (idleMs > 30000) continue;
      var sy = MA.nY('session', pi);
      var sX = MA.GEOM.clientX;
      var tX = sX + (balInX - sX) * 0.55;
      if (Math.random() < reqProb) {
        spawnParticle([
          {x: sX, y: sy},
          {x: tX, y: sy},
          {x: tX, y: cY},
          {x: balInX, y: cY}
        ], 'request', s.model);
      }
    }

    // --- Processing particles: balancer -> elbow (50%) -> backend
    for (var bpi = 0; bpi < visLimit('backend', MA.topo.backends.length); bpi++) {
      var b = MA.topo.backends[bpi];
      var active = b.activeRequests || 0;
      if (active <= 0) continue;
      var by = MA.nY('backend', bpi);
      var eX = MA.GEOM.backendX(MA.topo.w);
      var tX = balOutX + (eX - balOutX) * 0.5;
      // R92: было active*0.08 (потолок 0.40) — один активный запрос давал частицу
      // раз в ~2.5 с. Стало заметнее: active*0.15, потолок 0.6.
      var prob = Math.min(active * 0.15, 0.60);
      if (Math.random() < prob) {
        spawnParticle([
          {x: balOutX, y: cY},
          {x: tX, y: cY},
          {x: tX, y: by},
          {x: eX, y: by}
        ], 'processing', (b.models && b.models[0]) || '');
      }
      if (Math.random() < prob * 0.5) {
        spawnParticle([
          {x: tX, y: by},
          {x: eX, y: by}
        ], 'complete', (b.models && b.models[0]) || '');
      }
    }

    // NOTE: Full-route particles (client -> balancer -> backend) удалены,
    // т.к. они дублировали request+processing частицы и визуально создавали
    // впечатление прямого соединения минуя балансер.

    // --- Pending particles: запросы, которые ждут в очереди.
    //
    // R238b: раньше это были жёлтые точки, которые крутились ВНУТРИ блока
    // балансера по двум случайным координатам. На живом экране они выглядели
    // как «частицы ответа», которые никуда не едут, и путали оператора. Теперь
    // ожидающий запрос идёт по своей нарисованной линии клиент -> балансер
    // (как request/discovery), только жёлтым и гаснет у входа в балансер.
    var pendCount = MA.topo.queue.pending_count || 0;
    if (pendCount > 0 && Math.random() < Math.min(0.35, pendCount * 0.05)) {
      var pendVis = Math.min(MA.topo.sessions.length, MA.nVisible ? MA.nVisible('session') : MA.topo.sessions.length);
      if (pendVis > 0) {
        var psIdx = Math.floor(Math.random() * pendVis);
        var psY = MA.nY('session', psIdx);
        var psX = MA.GEOM.clientX;
        var ptX = psX + (balInX - psX) * 0.55;
        spawnParticle([
          {x: psX, y: psY},
          {x: ptX, y: psY},
          {x: ptX, y: cY},
          {x: balInX, y: cY}
        ], 'pending', 'queued');
      }
    }
  }

  function drawTopo() {
    var w = MA.topo.w, h = MA.topo.h;
    if (!w || !h) return;
    var vp = MA.viewport;
    var pal = themePal();
    var lt = pal.isLight;

    cx.save();
    // R192 (2026-10-20): ОЧИСТКА В ЭКРАННЫХ КООРДИНАТАХ.
    //
    // ЖАЛОБА: «при перезагрузке подвисает, смазывая картинку». Причина: clearRect
    // вызывался ПОСЛЕ setTransform(scale, offset) и в мировых координатах, поэтому
    // при любом пане/зуме часть прошлого кадра оставалась — кадры накладывались
    // друг на друга (шлейф). Рисовать надо так: сбросить трансформацию, стереть
    // весь bitmap, вернуть трансформацию.
    cx.setTransform(1, 0, 0, 1, 0, 0);
    cx.clearRect(0, 0, cv.width, cv.height);
    cx.setTransform(vp.scale, 0, 0, vp.scale, vp.offsetX, vp.offsetY);
    // (очистка перенесена выше, в экранные координаты — R192)

    // Draw grid in world coordinates
    var gCell = 40;
    var gx0 = Math.floor((-vp.offsetX / vp.scale) / gCell) * gCell;
    var gy0 = Math.floor((-vp.offsetY / vp.scale) / gCell) * gCell;
    var gx1 = Math.ceil((w - vp.offsetX) / vp.scale);
    var gy1 = Math.ceil((h - vp.offsetY) / vp.scale);
    cx.strokeStyle = pal.grid;
    cx.lineWidth = 1 / vp.scale;
    for (var gx = gx0; gx <= gx1; gx += gCell) { cx.beginPath(); cx.moveTo(gx, gy0); cx.lineTo(gx, gy1); cx.stroke(); }
    for (var gy = gy0; gy <= gy1; gy += gCell) { cx.beginPath(); cx.moveTo(gx0, gy); cx.lineTo(gx1, gy); cx.stroke(); }
    var cX = w / 2, cY = h / 2;
    cx.setLineDash([5, 3]);

    var bw = MA.GEOM.bw, bh = MA.GEOM.bh;
    var balInX = cX - bw / 2, balOutX = cX + bw / 2;

    var sessVis = visLimit('session', MA.topo.sessions.length);
    var backVis = visLimit('backend', MA.topo.backends.length);
    var sLayout = MA.nLayout('session'), bLayout = MA.nLayout('backend');
    for (var si = 0; si < sessVis; si++) {
      var s = MA.topo.sessions[si];
      var y = MA.nY('session', si), tc = s.backendId ? 'rgba(59,130,246,0.25)' : 'rgba(148,163,184,0.18)';
      cx.strokeStyle = tc; cx.lineWidth = s.backendId ? 3 : 2;
      var sX = MA.GEOM.clientX, tX = sX + (balInX - sX) * 0.55, eX = balInX, eY = cY;
      cx.beginPath(); cx.moveTo(sX, y); cx.lineTo(tX, y); cx.stroke();
      cx.beginPath(); cx.moveTo(tX, y); cx.lineTo(tX, eY); cx.stroke();
      cx.beginPath(); cx.moveTo(tX, eY); cx.lineTo(eX, eY); cx.stroke();
      cx.fillStyle = tc.replace('0.25', '0.5').replace('0.18', '0.35'); cx.beginPath(); cx.arc(tX, y, 5, 0, Math.PI * 2); cx.fill();
      cx.fillStyle = tc.replace('0.25', '0.5').replace('0.18', '0.35'); cx.beginPath(); cx.arc(tX, eY, 4, 0, Math.PI * 2); cx.fill();
    }

    for (var bi = 0; bi < backVis; bi++) {
      var b = MA.topo.backends[bi];
      var y = MA.nY('backend', bi), ih = b.status === 'active' || b.status === 'healthy' || b.status === 'ready', isOllamaUnavailable = b.status === 'ollama_unavailable', tc = ih ? 'rgba(59,130,246,0.25)' : (isOllamaUnavailable ? 'rgba(249,115,22,0.25)' : 'rgba(239,68,68,0.25)');
      cx.strokeStyle = tc; cx.lineWidth = ih ? 3 : 2;
      var sX = balOutX, sY = cY, eX = MA.GEOM.backendX(w), tX = sX + (eX - sX) * 0.5;
      cx.beginPath(); cx.moveTo(sX, sY); cx.lineTo(tX, sY); cx.stroke();
      cx.beginPath(); cx.moveTo(tX, sY); cx.lineTo(tX, y); cx.stroke();
      cx.beginPath(); cx.moveTo(tX, y); cx.lineTo(eX, y); cx.stroke();
      cx.fillStyle = tc.replace('0.25', '0.5'); cx.beginPath(); cx.arc(tX, y, 5, 0, Math.PI * 2); cx.fill();
      cx.fillStyle = tc.replace('0.25', '0.5'); cx.beginPath(); cx.arc(tX, sY, 4, 0, Math.PI * 2); cx.fill();
    }

    cx.setLineDash([]);
    var textLight = pal.text, textMuted = pal.textMuted;
    // R238: ширина плиток выводится из GEOM (на телефоне колонки уже), а не из
    // констант 120/128 — иначе плитки вылезают/наезжают на блок балансера.
    var sessTileW = Math.max(64, MA.GEOM.clientX - 20);
    for (var si2 = 0; si2 < sessVis; si2++) {
      var s = MA.topo.sessions[si2];
      var y = MA.nY('session', si2), ia = s.backendId != null, isCloud = MA.isCloudModel(s.model);
      var bc, br;
      if (isCloud) { bc = 'rgba(59,130,246,0.15)'; br = 'rgba(59,130,246,0.45)'; }
      else if (ia) { bc = 'rgba(59,130,246,0.18)'; br = 'rgba(59,130,246,0.5)'; }
      else { bc = 'rgba(148,163,184,0.12)'; br = 'rgba(148,163,184,0.25)'; }
      cx.fillStyle = bc; MA.rr(cx, 12, y - 22, sessTileW, 44, 8); cx.fill(); cx.strokeStyle = br; cx.lineWidth = 1; cx.stroke();
      cx.fillStyle = textLight; cx.font = 'bold 11px ' + MA.vF(); cx.textAlign = 'left';
      cx.save(); cx.beginPath(); cx.rect(16, y - 18, sessTileW - 12, 14); cx.clip(); cx.fillText(MA.getCI(s.clientName) + ' ' + MA.trunc(s.clientName, 8), 20, y - 6); cx.restore();
      cx.fillStyle = isCloud ? '#60a5fa' : textMuted; cx.font = '10px ' + MA.vF();
      cx.save(); cx.beginPath(); cx.rect(16, y + 1, sessTileW - 12, 14); cx.clip(); cx.fillText((isCloud ? '☁️ ' : '') + (s.model || '—') + ' • ' + (s.requestCount || 0) + ' req', 20, y + 8); cx.restore();
    }
    // R91: клиентов больше, чем влезло — показываем счётчик, а не молчим.
    drawOverflowChip('session', 12, sLayout.chipY, sLayout.hidden, lt);

    var bx = cX - bw / 2, by = cY - bh / 2;
    var pend = MA.topo.queue.pending_count || 0, mq = (MA.topo.queue.all || []).length > 0 ? Math.max((MA.topo.queue.all || []).length, 5) : 5;
    var qr = Math.min(pend / mq, 1), bc = qr > 0.75 ? '#ef4444' : qr > 0.3 ? '#f59e0b' : '#3b82f6';
    cx.shadowColor = bc; cx.shadowBlur = 20; cx.fillStyle = bc + '18'; MA.rr(cx, bx - 4, by - 4, bw + 8, bh + 8, 12); cx.fill(); cx.shadowBlur = 0;
    cx.fillStyle = bc + '22'; MA.rr(cx, bx, by, bw, bh, 10); cx.fill(); cx.strokeStyle = 'rgba(148,163,184,0.4)'; cx.lineWidth = 2; cx.stroke();
    cx.fillStyle = pal.text; cx.font = 'bold 13px ' + MA.vF(); cx.textAlign = 'center'; cx.fillText(MA.T('monitor.canvas.balancer'), cX, cY - 16);
    // R200 (2026-10-20): в блоке балансера — ЖИВЫЕ цифры, не только очередь.
    // Блок показывал лишь pending_count, а на этом стенде очередь почти всегда
    // пуста, поэтому выглядел мёртвым. Считаем то, что приходит с каждым опросом:
    // активные запросы по бэкендам и их суммарный RPS.
    var activeReq = 0, rpsSum = 0;
    for (var ai = 0; ai < backVis; ai++) {
      var ab = MA.topo.backends[ai];
      if (!ab) continue;
      activeReq += (ab.activeRequests || 0);
      var ar = (ab.ollama && ab.ollama.requestsPerSecond) || ab.requestsPerSecond || 0;
      if (typeof ar === 'number') rpsSum += ar;
    }
    cx.fillStyle = textMuted; cx.font = '11px ' + MA.vF(); cx.fillText(MA.T('monitor.canvas.queue') + pend, cX, cY + 2);
    cx.fillText(MA.T('monitor.canvas.liveShort', { active: activeReq, rps: (Math.round(rpsSum * 10) / 10) }), cX, cY + 16);
    // R200b: объявление полосы очереди (было затёрто правкой R200 — qbX/qbY/qbW/qbH, из-за
    // этого drawTopo падал с ReferenceError, и НИ ОДИН кадр не дорисовывался, включая частицы).
    var qbX = bx + 20, qbY = by + bh / 2 + 14, qbW = bw - 40, qbH = 8;
    cx.fillStyle = pal.barBg; MA.rr(cx, qbX, qbY, qbW, qbH, 4); cx.fill();
    cx.fillStyle = qr > 0.75 ? '#ef4444' : '#60a5fa'; MA.rr(cx, qbX, qbY, qbW * qr, qbH, 4); cx.fill();
    var backendIdleTxt = pal.tileText;
    for (var bi2 = 0; bi2 < backVis; bi2++) {
      var b = MA.topo.backends[bi2];
      var y = MA.nY('backend', bi2), ih = b.status === 'active' || b.status === 'healthy' || b.status === 'ready';
      var isOllamaUnavailable = b.status === 'ollama_unavailable';
      var isWarming = (b.warmingUpModels && b.warmingUpModels.length > 0) || b.status === 'warming_up';
      var bc, br;
      if (isWarming) {
        bc = 'rgba(249,115,22,0.15)';
        br = 'rgba(249,115,22,0.6)';
      } else if (ih) {
        bc = b.activeRequests > 0 ? 'rgba(245,158,11,0.15)' : 'rgba(34,197,94,0.12)';
        br = b.activeRequests > 0 ? 'rgba(245,158,11,0.4)' : 'rgba(34,197,94,0.4)';
      } else if (isOllamaUnavailable) {
        bc = 'rgba(249,115,22,0.12)';
        br = 'rgba(249,115,22,0.4)';
      } else {
        bc = 'rgba(239,68,68,0.12)';
        br = 'rgba(239,68,68,0.4)';
      }
      var backTileX = w - MA.GEOM.backendMargin;
      var backTileW = Math.max(72, MA.GEOM.backendMargin - 12);
      cx.fillStyle = bc; MA.rr(cx, backTileX, y - 26, backTileW, 52, 8); cx.fill(); cx.strokeStyle = br; cx.lineWidth = isWarming ? 2.5 : 1.5; cx.stroke();
      cx.fillStyle = backendIdleTxt; cx.font = 'bold 11px ' + MA.vF(); cx.textAlign = 'left'; cx.fillText(MA.trunc(b.id, 12), backTileX + 8, y - 10);
      var mr = b.maxConcurrentRequests || 10, a = b.activeRequests || 0, l = a / mr;
      cx.fillStyle = l >= 0.8 ? '#ef4444' : l >= 0.5 ? '#f59e0b' : '#22c55e'; cx.font = '10px ' + MA.vF();
      cx.fillText(a > 0 ? '⏳ ' + a + '/' + mr : '✅ ' + MA.T('monitor.canvas.idle'), backTileX + 8, y + 6);
      var mc = (b.models || []).length;
      var wmCount = (b.warmingUpModels || []).length;
      if (isWarming) {
        cx.fillStyle = '#f97316'; cx.font = 'bold 9px ' + MA.vF();
        var wmLabel = wmCount > 1 ? wmCount + ' ' + MA.T('monitor.canvas.warmingModels') : MA.T('monitor.canvas.warming');
        cx.fillText('⏳ ' + wmLabel, backTileX + 8, y + 20);
      } else if (mc > 0) { cx.fillStyle = '#a855f7'; cx.font = '9px ' + MA.vF(); cx.fillText('🧠 ' + mc + MA.T('models_lower'), backTileX + 8, y + 20); }
      else if (ih && a > 0) { cx.fillStyle = '#f97316'; cx.font = '9px ' + MA.vF(); cx.fillText(MA.T('loading'), backTileX + 8, y + 20); }
      var vp = b.vram ? b.vram.usagePercent : 0;
      if (vp > 0) {
        var vbW = Math.max(40, Math.min(100, backTileW - 16));
        cx.fillStyle = pal.barBg; MA.rr(cx, backTileX + 8, y + 26, vbW, 3, 2); cx.fill();
        cx.fillStyle = vp > 85 ? '#ef4444' : vp > 60 ? '#f59e0b' : '#22c55e'; MA.rr(cx, backTileX + 8, y + 26, vbW * (vp / 100), 3, 2); cx.fill();
      }
    }
    // R91: бэкендов больше, чем влезло — счётчик вместо молчания.
    drawOverflowChip('backend', w - MA.GEOM.backendMargin, bLayout.chipY, bLayout.hidden, lt);

    sp();
    var now = Date.now();
    for (var i = MA.particles.length - 1; i >= 0; i--) {
      var p = MA.particles[i];
      var elapsed = now - p.createdAt;
      var t = Math.min(elapsed / p.duration, 1.0);
      var et = 1 - Math.pow(1 - t, 3);
      // Interpolate along path waypoints
      var path = p.path || [{x: p.fx || 0, y: p.fy || 0}, {x: p.tx || 0, y: p.ty || 0}];
      var segCount = path.length - 1;
      var segLen = 1.0 / segCount;
      var segIdx = Math.min(Math.floor(et / segLen), segCount - 1);
      var segT = (et - segIdx * segLen) / segLen;
      // Линейная интерполяция в пределах каждого сегмента — частицы идут строго по линиям
      var p0 = path[segIdx], p1 = path[segIdx + 1];
      p.x = p0.x + (p1.x - p0.x) * segT;
      p.y = p0.y + (p1.y - p0.y) * segT;
      var pColor, pSize;
      switch (p.type) {
        case 'request': pColor = '#60a5fa'; pSize = 3; break;
        case 'pending': pColor = '#f59e0b'; pSize = 4; break;
        case 'processing': pColor = '#a855f7'; pSize = 4; break;
        case 'complete': pColor = '#22c55e'; pSize = 2; break;
        default: pColor = '#94a3b8'; pSize = 3;
      }
      cx.shadowColor = pColor;
      cx.shadowBlur = t < 0.5 ? 6 : 3;
      cx.fillStyle = pColor;
      cx.beginPath();
      cx.arc(p.x, p.y, pSize * (1 + 0.3 * Math.sin(elapsed * 0.01)), 0, Math.PI * 2);
      cx.fill();
      cx.shadowBlur = 0;
      if (t >= 1.0) {
        MA.particles.splice(i, 1);
      }
    }
    if (MA.particles.length > 200) {
      MA.particles.splice(0, MA.particles.length - 200);
    }
    if (MA.topo.sessions.length === 0 && MA.topo.backends.length === 0) {
      cx.fillStyle = '#475569'; cx.font = '14px ' + MA.vF(); cx.textAlign = 'center';
      cx.fillText(MA.T('monitor.overlay.topologyNoData'), w / 2, h / 2 + 80);
      cx.font = '12px ' + MA.vF(); cx.fillText(MA.T('monitor.overlay.topologyHint'), w / 2, h / 2 + 100);
    }

    cx.restore();
  }

  window.drawTopo = drawTopo;
})();

// js/monitor/canvas-conveyor.js — Conveyor belt animation (conveyorCanvas)
// Анимированные частицы, показывающие полный путь запроса:
// клиент → балансер (очередь) → бэкенд → завершено
// Цвет меняется по пути: 🔵(в пути) → 🟡(ожидание) → 🟣(обработка) → 🟢(готово)
// NEW: discovery (голубой) — все HTTP-запросы, warming (оранжевый) — загрузка модели

(function() {
  'use strict';

  var MA = window.MonitorApp;

  var ccv = document.getElementById('conveyorCanvas'), ccx = ccv ? ccv.getContext('2d') : null;

  // R95: палитра берётся из темы (см. MonitorApp.themePalette) — раньше была
  // бинарная проверка data-theme === 'light', из-за которой светлые темы кроме
  // light (например mint) получали тёмные подписи и сетку и сливались с фоном.
  function themePal() {
    if (MA.themePalette) return MA.themePalette();
    return { isLight: false, lane: 'rgba(255,255,255,0.10)', laneLabel: 'rgba(255,255,255,0.35)' };
  }

  // findSessionIndex / findBackendIndex — координаты узлов для частиц. Частицу
  // нельзя пускать «в пустоту»: у не поместившихся в колонку узлов нет Y (R91),
  // поэтому ищем только среди нарисованных (nVisible).
  function findSessionIndex(name) {
    var sessions = MA.topo.sessions || [];
    var vis = MA.nVisible ? MA.nVisible('session') : sessions.length;
    for (var i = 0; i < Math.min(sessions.length, vis); i++) {
      if (sessions[i].clientName === name) return i;
    }
    return -1;
  }

  function findBackendIndex(id) {
    var backs = MA.topo.backends || [];
    var vis = MA.nVisible ? MA.nVisible('backend') : backs.length;
    for (var i = 0; i < Math.min(backs.length, vis); i++) {
      if (backs[i].id === id) return i;
    }
    return -1;
  }

  // seenOnce — «это событие уже нарисовано». Ключи requestId:время, чтобы новый
  // цикл демо (те же id, другое время) снова дал частицу. Старые ключи чистим,
  // иначе за долгую сессию объект растёт.
  function seenOnce(store, key, now) {
    if (store[key]) return true;
    store[key] = now;
    if (!store.__lastPrune || now - store.__lastPrune > 30000) {
      store.__lastPrune = now;
      for (var k in store) {
        if (k === '__lastPrune') continue;
        if (now - store[k] > 60000) delete store[k];
      }
    }
    return false;
  }

  // drawParticleLabel — единая подпись у ВСЕХ частиц (раньше discovery/warming
  // были с подписью, а «полный путь"/"завершение» — без; оператор спрашивал,
  // почему у одних точек подпись есть, а у других нет).
  function drawParticleLabel(text, x, y, color, alpha) {
    if (!text || alpha <= 0.02) return;
    ccx.save();
    ccx.globalAlpha = Math.max(0, Math.min(1, alpha));
    ccx.font = 'bold 8px ' + MA.vF();
    ccx.textAlign = 'center';
    ccx.shadowColor = 'rgba(0,0,0,0.9)';
    ccx.shadowBlur = 3;
    ccx.fillStyle = color;
    ccx.fillText(text, x, y);
    ccx.restore();
  }

  // ---- Интерполяция по waypoints ----
  function interpolatePath(path, t) {
    if (!path || path.length < 2) return { x: 0, y: 0 };
    var segCount = path.length - 1;
    var segLen = 1.0 / segCount;
    var segIdx = Math.min(Math.floor(t / segLen), segCount - 1);
    var segT = (t - segIdx * segLen) / segLen;
    var p0 = path[segIdx], p1 = path[segIdx + 1];
    return {
      x: p0.x + (p1.x - p0.x) * segT,
      y: p0.y + (p1.y - p0.y) * segT
    };
  }

  function drawConveyor() {
    if (!ccv || !ccx) return;

    // Синхронизируем размеры с topology canvas (vizCanvas = parent height, topo.h = parent - 52)
    var w = ccv.parentElement.clientWidth;
    var h = MA.topo.h || (ccv.parentElement.clientHeight - 52);
    if (!h || h < 100) h = 100;
    ccv.width = w; ccv.height = h;
    // CSS height управляется inline bottom:52px, не переопределяем style.height
    ccv.style.width = w + 'px';

    var vp = MA.viewport;
    ccx.save();
    ccx.setTransform(vp.scale, 0, 0, vp.scale, vp.offsetX, vp.offsetY);
    ccx.clearRect(-vp.offsetX / vp.scale, -vp.offsetY / vp.scale, w / vp.scale, h / vp.scale);

    if (MA.topo.backends.length === 0 && MA.topo.sessions.length === 0 && (!MA.topo.recentClients || MA.topo.recentClients.length === 0)) {
      ccx.restore();
      return;
    }

    var now = Date.now();
    // R238: демо отдаёт точные события (recentRequests/recentResponses) — тогда
    // конвейер рисует по ним, а не случайными частицами.
    var demoLifecycle = !!MA.topo.demoLifecycle;

    // ---- Общая геометрия из state.js ----
    var G = MA.GEOM;
    var balInX = G.balInX(w);
    var balOutX = G.balOutX(w);
    var balCenterX = G.balCenterX(w);
    var balCenterY = G.balCenterY(h);

    // Right-side elbow X at 50% between balOutX and backendX
    var backendX = G.backendX(w);
    var rightElbowX = balOutX + (backendX - balOutX) * 0.5;

    // RPS calculation
    var backends = MA.topo.backends || [];
    var sessions = MA.topo.sessions || [];
    var activeTotal = backends.reduce(function(s, b) { return s + (b.activeRequests || 0); }, 0);
    var maxTotal = backends.reduce(function(s, b) { return s + (b.maxConcurrentRequests || 10); }, 0);
    var utilization = maxTotal > 0 ? activeTotal / maxTotal : 0;

    var totalRequests = sessions.reduce(function(s, sess) { return s + (sess.requestCount || 0); }, 0);
    var timeDelta = (now - MA.lastTime) / 1000;
    var rps = 0;
    if (MA._rpsInitialized && timeDelta > 0.5 && MA.lastTotalRequests !== undefined && MA.lastTotalRequests > 0) {
      rps = Math.max(0, (totalRequests - MA.lastTotalRequests) / timeDelta);
    }
    if (!MA._rpsInitialized) {
      MA._rpsInitialized = true;
      MA.lastTotalRequests = totalRequests;
      MA.lastTime = now;
    } else {
      MA.lastTotalRequests = totalRequests;
      MA.lastTime = now;
    }

    var spawnIntensity = Math.max(0.05, Math.min(0.50, utilization * 0.4 + rps * 0.02));

    // ---- Подписи зон ----
    var pal = themePal();
    var labelColor = pal.laneLabel;
    ccx.fillStyle = labelColor;
    ccx.font = '10px ' + MA.vF();
    ccx.textAlign = 'center';
    ccx.fillText(MA.T('monitor.canvas.clients'), G.clientX / 2, h - 6);
    ccx.fillText(MA.T('monitor.canvas.balancer'), w / 2, h - 6);
    ccx.fillText(MA.T('monitor.canvas.backends'), balOutX + (w - balOutX) / 2, h - 6);

    // Разделительные линии
    ccx.strokeStyle = pal.lane;
    ccx.lineWidth = 1;
    ccx.setLineDash([4, 6]);
    ccx.beginPath(); ccx.moveTo(balInX, 10); ccx.lineTo(balInX, h - 14); ccx.stroke();
    ccx.beginPath(); ccx.moveTo(balOutX, 10); ccx.lineTo(balOutX, h - 14); ccx.stroke();
    ccx.setLineDash([]);

    // RPS label
    ccx.fillStyle = labelColor;
    ccx.font = '9px ' + MA.vF();
    ccx.fillText(MA.T('monitor.canvas.rpsLabel', { rps: rps.toFixed(1), active: activeTotal, max: maxTotal }), w / 2, 10);

    // ============================================================
    // D. DEMO: точные события запроса и ОТВЕТА (R238)
    // ============================================================
    //
    // Сценарий знает каждый запрос: recentRequests — назначения
    // (клиент → балансер → бэкенд), recentResponses — завершения
    // (бэкенд → балансер → КЛИЕНТ). Поэтому жёлтая частица ответа идёт по той же
    // линии конвейера в обратную сторону и подписана номером запроса, а не летит
    // безымянной точкой. Раньше обратной ветки не было вовсе — оператор видел
    // «жёлтые частицы», которые никуда не двигались.
    if (demoLifecycle) {
      var reqSeen = MA._convSeenReq || (MA._convSeenReq = {});
      var respSeen = MA._convSeenResp || (MA._convSeenResp = {});
      var recentReq = MA.topo.recentRequests || [];
      var recentResp = MA.topo.recentResponses || [];
      var tXD = G.clientX + (balInX - G.clientX) * 0.55;

      var reqLimit = Math.min(recentReq.length, 4);
      for (var ri = 0; ri < reqLimit; ri++) {
        var rq = recentReq[ri];
        var rqKey = 'req:' + rq.requestId + ':' + Math.round(new Date(rq.at).getTime() / 1000);
        if (seenOnce(reqSeen, rqKey, now)) continue;
        var siR = findSessionIndex(rq.clientName);
        var biR = findBackendIndex(rq.backendId || rq.backendName || rq.backend);
        if (siR < 0 || biR < 0) continue;
        var syR = MA.nY('session', siR), byR = MA.nY('backend', biR);
        MA.convQueue.push({
          path: [
            {x: G.clientX, y: syR}, {x: tXD, y: syR}, {x: tXD, y: balCenterY},
            {x: balInX, y: balCenterY}, {x: balOutX, y: balCenterY},
            {x: rightElbowX, y: balCenterY}, {x: rightElbowX, y: byR}, {x: backendX, y: byR}
          ],
          spawnTime: now,
          duration: 2400 + Math.random() * 600,
          type: 'full',
          size: 3,
          label: rq.requestId + ' · ' + MA.trunc(rq.model || '', 12)
        });
      }

      var respLimit = Math.min(recentResp.length, 4);
      for (var rsi = 0; rsi < respLimit; rsi++) {
        var rs = recentResp[rsi];
        var rsKey = 'resp:' + rs.requestId + ':' + Math.round(new Date(rs.at).getTime() / 1000);
        if (seenOnce(respSeen, rsKey, now)) continue;
        var siS = findSessionIndex(rs.clientName);
        var biS = findBackendIndex(rs.backendId || rs.backendName || rs.backend);
        if (siS < 0 || biS < 0) continue;
        var syS = MA.nY('session', siS), byS = MA.nY('backend', biS);
        MA.convQueue.push({
          path: [
            {x: backendX, y: byS}, {x: rightElbowX, y: byS}, {x: rightElbowX, y: balCenterY},
            {x: balOutX, y: balCenterY}, {x: balInX, y: balCenterY},
            {x: tXD, y: balCenterY}, {x: tXD, y: syS}, {x: G.clientX, y: syS}
          ],
          spawnTime: now,
          duration: 2000 + Math.random() * 600,
          type: 'response',
          size: 3.2,
          label: '↩ ' + rs.requestId + ' · ' + MA.trunc(rs.model || '', 12)
        });
      }
    }

    // ============================================================
    // 0. DISCOVERY частицы — все HTTP-запросы (RecentClients)
    // Показывают момент формирования запроса, включая /api/tags
    // Only spawn if the client has a matching session in topology (has visible line)
    // ============================================================
    var recentClients = MA.topo.recentClients || [];
    var rcCount = recentClients.length;
    // В демо discovery не нужен: точные частицы запроса/ответа уже показывают
    // этот путь, а «случайные HTTP-запросы» лишь забивали полотно десятками
    // одинаковых синих точек.
    if (!demoLifecycle && rcCount > 0 && Math.random() < Math.min(0.4, rcCount * 0.05)) {
      var rc = recentClients[Math.floor(Math.random() * rcCount)];
      // Находим Y для этого клиента — только если есть сессия с таким именем
      // R91: и только если эта сессия РИСУЕТСЯ (не поместившиеся не имеют
      // координат на картинке, и частица летела бы в пустоту).
      var sessVis = MA.nVisible ? MA.nVisible('session') : sessions.length;
      for (var si = 0; si < Math.min(sessions.length, sessVis); si++) {
        if (sessions[si].clientName === rc.clientName) {
          var rcY = MA.nY('session', si);
          var sX = G.clientX;
          var tX = sX + (balInX - sX) * 0.55;
          var path = [
            {x: sX, y: rcY},
            {x: tX, y: rcY},
            {x: tX, y: balCenterY},
            {x: balInX, y: balCenterY}
          ];
          MA.convQueue.push({
            path: path,
            spawnTime: now,
            duration: 1200 + Math.random() * 800,
            type: 'discovery',
            size: 2.5 + Math.random() * 1,
            clientName: rc.clientName,
            label: rc.clientName
          });
          break;
        }
      }
    }

    // ============================================================
    // 1. WARMING частицы — модели в процессе загрузки
    // Движутся от балансера к бэкенду через промежуточный эльбоу (50%)
    // ============================================================
    var warmingModels = MA.topo.warmingUpModels || [];
    if (warmingModels.length > 0 && Math.random() < Math.min(0.35, warmingModels.length * 0.08)) {
      var wm = warmingModels[Math.floor(Math.random() * warmingModels.length)];
      // Находим бэкенд, на котором warming
      var wbIdx = -1;
      var backVis = MA.nVisible ? MA.nVisible('backend') : backends.length;
      for (var bi = 0; bi < Math.min(backends.length, backVis); bi++) {
        if ((backends[bi].warmingUpModels || []).indexOf(wm) >= 0 ||
            (backends[bi].status === 'warming_up')) {
          wbIdx = bi; break;
        }
      }
      if (wbIdx < 0) wbIdx = Math.floor(Math.random() * Math.max(1, Math.min(backends.length, backVis)));
      var wbY = MA.nY('backend', wbIdx);

      // Путь через эльбоу на 50% между балансером и бэкендом
      var path = [
        {x: balOutX, y: balCenterY},
        {x: rightElbowX, y: balCenterY},
        {x: rightElbowX, y: wbY},
        {x: backendX, y: wbY}
      ];

      MA.convQueue.push({
        path: path,
        spawnTime: now,
        duration: 2500 + Math.random() * 1500,
        type: 'warming',
        size: 3 + Math.random() * 1.5,
        modelName: wm,
        label: '⏳ ' + MA.trunc(wm, 14)
      });
    }

    // ============================================================
    // 2. ПОЛНЫЙ МАРШРУТ: клиент → балансер → бэкенд → завершено
    // Бэкенд-сторона идёт через эльбоу на 50%
    // ============================================================
    if (!demoLifecycle && activeTotal > 0 && Math.random() < spawnIntensity) {
      var clientIdx, sessionY;
      // R91: частицы летят только к НАРИСОВАННЫМ узлам (nVisible).
      var sessVisFull = MA.nVisible ? MA.nVisible('session') : sessions.length;
      var backVisFull = MA.nVisible ? MA.nVisible('backend') : backends.length;
      var drawableSessions = Math.min(sessions.length, sessVisFull);
      var drawableBackends = Math.min(backends.length, backVisFull);
      if (drawableSessions > 0) {
        clientIdx = Math.floor(Math.random() * drawableSessions);
        sessionY = MA.nY('session', clientIdx);
      } else {
        sessionY = balCenterY;
      }

      // Выбираем бэкенд пропорционально активным запросам
      var backendIdx = 0;
      if (drawableBackends > 1) {
        var weightedSum = 0;
        for (var wi = 0; wi < drawableBackends; wi++) weightedSum += Math.max(1, backends[wi].activeRequests || 1);
        var rand = Math.random() * weightedSum;
        for (var bi = 0; bi < drawableBackends; bi++) {
          rand -= Math.max(1, backends[bi].activeRequests || 1);
          if (rand <= 0) { backendIdx = bi; break; }
        }
      }
      var backendY = MA.nY('backend', backendIdx);

      var sX = G.clientX;
      var tX_left = sX + (balInX - sX) * 0.55;
      // Full route: client -> left elbow -> balancer -> right elbow -> backend
      var path = [
        {x: sX, y: sessionY},
        {x: tX_left, y: sessionY},
        {x: tX_left, y: balCenterY},
        {x: balInX, y: balCenterY},
        {x: balOutX, y: balCenterY},
        {x: rightElbowX, y: balCenterY},
        {x: rightElbowX, y: backendY},
        {x: backendX, y: backendY}
      ];

      MA.convQueue.push({
        path: path,
        spawnTime: now,
        duration: 3000 + Math.random() * 2000,
        type: 'full',
        size: 3 + Math.random() * 1.5,
        label: MA.trunc((sessions[clientIdx] && sessions[clientIdx].model) || '', 14)
      });
    }

    // Живой режим: когда частица полного маршрута доходит до бэкенда, рождаем
    // ОТВЕТ по тому же пути в обратную сторону (бэкенд → балансер → клиент).
    // Раньше «завершающие» частицы шли к бэкенду — то есть в ту же сторону, что
    // и запрос, и ответа клиенту на картинке не было вообще.
    if (!demoLifecycle) {
      var procCount = MA.topo.queue.processing_count || 0;
      if (procCount > 0 && Math.random() < Math.min(0.25, procCount * 0.05)) {
        var backVisResp = Math.min(backends.length, MA.nVisible ? MA.nVisible('backend') : backends.length);
        if (backVisResp > 0) {
          var busyB = [];
          for (var ci = 0; ci < backVisResp; ci++) {
            if (backends[ci].activeRequests > 0) busyB.push(ci);
          }
          var rbIdx = busyB.length > 0 ? busyB[Math.floor(Math.random() * busyB.length)] : Math.floor(Math.random() * backVisResp);
          var rbY = MA.nY('backend', rbIdx);
          var rSessIdx = sessions.length > 0 ? Math.floor(Math.random() * Math.min(sessions.length, MA.nVisible ? MA.nVisible('session') : sessions.length)) : -1;
          var rSessY = rSessIdx >= 0 ? MA.nY('session', rSessIdx) : balCenterY;
          var tXr = G.clientX + (balInX - G.clientX) * 0.55;
          MA.convQueue.push({
            path: [
              {x: backendX, y: rbY}, {x: rightElbowX, y: rbY}, {x: rightElbowX, y: balCenterY},
              {x: balOutX, y: balCenterY}, {x: balInX, y: balCenterY},
              {x: tXr, y: balCenterY}, {x: tXr, y: rSessY}, {x: G.clientX, y: rSessY}
            ],
            spawnTime: now,
            duration: 1800 + Math.random() * 700,
            type: 'response',
            size: 2.6
          });
        }
      }
    }

    // ---- Рендеринг частиц ----
    for (var i = MA.convQueue.length - 1; i >= 0; i--) {
      var p = MA.convQueue[i];
      var elapsed = now - p.spawnTime;
      var t = Math.min(elapsed / p.duration, 1.0);
      var path = p.path || [{x: p.x || 0, y: p.y || 0}, {x: p.tx || 0, y: p.ty || 0}];
      var pos = interpolatePath(path, t);
      var size = (p.size || 2.5) * (1 + 0.25 * Math.sin(elapsed * 0.012));
      var alpha = 0.65 + 0.35 * Math.min(1, t * 3);
      if (t > 0.8) alpha *= Math.max(0, (1 - t) / 0.2);

      var color;
      if (p.type === 'discovery') {
        color = '#38bdf8';
        size = (p.size || 2.5) * (1 + 0.3 * Math.sin(elapsed * 0.015));
      } else if (p.type === 'warming') {
        color = '#f97316';
      } else if (p.type === 'response') {
        // ОТВЕТ: бэкенд → балансер → клиент. Жёлтый → зелёный — так его не
        // спутать с «ожиданием в очереди»: это обратная ветка конвейера,
        // которой раньше на картинке не было вообще.
        color = t < 0.55 ? '#f59e0b' : '#22c55e';
      } else {
        // Полный маршрут запроса: синий → жёлтый → фиолетовый → зелёный.
        var colors = [
          { stop: 0.00, color: '#3b82f6' },
          { stop: 0.30, color: '#f59e0b' },
          { stop: 0.55, color: '#a855f7' },
          { stop: 0.85, color: '#22c55e' },
        ];
        var pt = t;
        if (pt <= colors[0].stop) color = colors[0].color;
        else if (pt >= colors[colors.length - 1].stop) color = colors[colors.length - 1].color;
        else {
          for (var ci = 0; ci < colors.length - 1; ci++) {
            if (pt >= colors[ci].stop && pt < colors[ci + 1].stop) { color = colors[ci].color; break; }
          }
        }
        if (!color) color = '#22c55e';
      }

      // Свечение + ядро частицы — одинаково для всех типов.
      ccx.globalAlpha = alpha * 0.5;
      ccx.fillStyle = color;
      ccx.shadowColor = color;
      ccx.shadowBlur = 8;
      ccx.beginPath();
      ccx.arc(pos.x, pos.y, size * 1.5, 0, Math.PI * 2);
      ccx.fill();

      ccx.globalAlpha = alpha;
      ccx.shadowBlur = 5;
      ccx.beginPath();
      ccx.arc(pos.x, pos.y, size, 0, Math.PI * 2);
      ccx.fill();

      // Хвостовой след — по линии конвейера, чтобы было видно направление.
      if (path.length >= 2) {
        var segCount2 = path.length - 1;
        var segLen2 = 1.0 / segCount2;
        var tBack = Math.max(0, t - segLen2 * 0.25);
        var segIdxBack = Math.min(Math.floor(tBack / segLen2), segCount2 - 1);
        var segTBack = (tBack - segIdxBack * segLen2) / segLen2;
        var p0Back = path[segIdxBack], p1Back = path[segIdxBack + 1];
        var posBack = {
          x: p0Back.x + (p1Back.x - p0Back.x) * segTBack,
          y: p0Back.y + (p1Back.y - p0Back.y) * segTBack
        };
        ccx.globalAlpha = alpha * 0.3;
        ccx.beginPath();
        ccx.arc(posBack.x, posBack.y, size * 0.6, 0, Math.PI * 2);
        ccx.fill();
      }

      // Подпись — у ВСЕХ частиц (единый стиль и место). R238: жалоба «у одних
      // частиц подпись есть, у других нет» — раньше подписывались только
      // discovery/warming, а запрос/ответ шли безымянными точками.
      if (p.label && t > 0.04 && t < 0.92) {
        var labelAlpha = alpha * Math.min(1, (t - 0.04) * 10) * Math.min(1, (0.92 - t) * 8);
        drawParticleLabel(p.label, pos.x, pos.y - size - 7, color, labelAlpha);
      }

      ccx.globalAlpha = 1;
      ccx.shadowBlur = 0;

      if (t >= 1.0) {
        MA.convQueue.splice(i, 1);
      }
    }

    if (MA.convQueue.length > 200) {
      MA.convQueue.splice(0, MA.convQueue.length - 200);
    }

    ccx.restore();
  }

  window.drawConveyor = drawConveyor;
})();

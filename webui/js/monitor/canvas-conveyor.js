// js/monitor/canvas-conveyor.js — Conveyor belt animation (conveyorCanvas)
// Анимированные частицы, показывающие полный путь запроса:
// клиент → балансер (очередь) → бэкенд → завершено
// Цвет меняется по пути: 🔵(в пути) → 🟡(ожидание) → 🟣(обработка) → 🟢(готово)
// NEW: discovery (голубой) — все HTTP-запросы, warming (оранжевый) — загрузка модели

(function() {
  'use strict';

  var MA = window.MonitorApp;

  var ccv = document.getElementById('conveyorCanvas'), ccx = ccv ? ccv.getContext('2d') : null;

  function isLightTheme() {
    return document.documentElement.getAttribute('data-theme') === 'light';
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
    var lt = isLightTheme();
    var labelColor = lt ? 'rgba(0,0,0,0.3)' : 'rgba(255,255,255,0.2)';
    ccx.fillStyle = labelColor;
    ccx.font = '10px ' + MA.vF();
    ccx.textAlign = 'center';
    ccx.fillText(MA.T('monitor.canvas.clients'), G.clientX / 2, h - 6);
    ccx.fillText(MA.T('monitor.canvas.balancer'), w / 2, h - 6);
    ccx.fillText(MA.T('monitor.canvas.backends'), balOutX + (w - balOutX) / 2, h - 6);

    // Разделительные линии
    ccx.strokeStyle = lt ? 'rgba(0,0,0,0.08)' : 'rgba(255,255,255,0.05)';
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
    // 0. DISCOVERY частицы — все HTTP-запросы (RecentClients)
    // Показывают момент формирования запроса, включая /api/tags
    // Only spawn if the client has a matching session in topology (has visible line)
    // ============================================================
    var recentClients = MA.topo.recentClients || [];
    var rcCount = recentClients.length;
    if (rcCount > 0 && Math.random() < Math.min(0.4, rcCount * 0.05)) {
      var rc = recentClients[Math.floor(Math.random() * rcCount)];
      // Находим Y для этого клиента — только если есть сессия с таким именем
      for (var si = 0; si < sessions.length; si++) {
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
            clientName: rc.clientName
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
      for (var bi = 0; bi < backends.length; bi++) {
        if ((backends[bi].warmingUpModels || []).indexOf(wm) >= 0 ||
            (backends[bi].status === 'warming_up')) {
          wbIdx = bi; break;
        }
      }
      if (wbIdx < 0) wbIdx = Math.floor(Math.random() * Math.max(1, backends.length));
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
        modelName: wm
      });
    }

    // ============================================================
    // 2. ПОЛНЫЙ МАРШРУТ: клиент → балансер → бэкенд → завершено
    // Бэкенд-сторона идёт через эльбоу на 50%
    // ============================================================
    if (activeTotal > 0 && Math.random() < spawnIntensity) {
      var clientIdx, sessionY;
      if (sessions.length > 0) {
        clientIdx = Math.floor(Math.random() * sessions.length);
        sessionY = MA.nY('session', clientIdx);
      } else {
        sessionY = balCenterY;
      }

      // Выбираем бэкенд пропорционально активным запросам
      var backendIdx = 0;
      if (backends.length > 1) {
        var weightedSum = backends.reduce(function(s, b) { return s + Math.max(1, b.activeRequests || 1); }, 0);
        var rand = Math.random() * weightedSum;
        for (var bi = 0; bi < backends.length; bi++) {
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
        size: 3 + Math.random() * 1.5
      });
    }

    // ============================================================
    // 3. ЗАВЕРШАЮЩИЕ ЧАСТИЦЫ — от эльбоу к бэкенду (финальный сегмент)
    // ============================================================
    var procCount = MA.topo.queue.processing_count || 0;
    if (procCount > 0 && Math.random() < Math.min(0.25, procCount * 0.05)) {
      var bIdx;
      if (backends.length > 0) {
        var busyBackends = backends.reduce(function(acc, b, idx) {
          if (b.activeRequests > 0) acc.push(idx);
          return acc;
        }, []);
        bIdx = busyBackends.length > 0
          ? busyBackends[Math.floor(Math.random() * busyBackends.length)]
          : Math.floor(Math.random() * backends.length);
      } else {
        bIdx = 0;
      }
      var bY = MA.nY('backend', bIdx);

      // Complete particles go from right elbow to backend — follows the visible line
      var path = [
        {x: rightElbowX, y: bY},
        {x: backendX, y: bY}
      ];
      MA.convQueue.push({
        path: path,
        spawnTime: now,
        duration: 800 + Math.random() * 700,
        type: 'complete',
        size: 2 + Math.random() * 1
      });
    }

    // ---- Рендеринг частиц ----
    for (var i = MA.convQueue.length - 1; i >= 0; i--) {
      var p = MA.convQueue[i];
      var elapsed = now - p.spawnTime;
      var t = Math.min(elapsed / p.duration, 1.0);
      var path = p.path || [{x: p.x || 0, y: p.y || 0}, {x: p.tx || 0, y: p.ty || 0}];

      if (p.type === 'discovery') {
        // ---- DISCOVERY: голубая частица, быстрое движение к балансеру ----
        var pos = interpolatePath(path, t);
        var size = (p.size || 2.5) * (1 + 0.3 * Math.sin(elapsed * 0.015));
        var alpha = 0.5 + 0.5 * Math.min(1, t * 4);
        if (t > 0.8) alpha *= Math.max(0, (1 - t) / 0.2);

        var color = '#38bdf8'; // голубой для discovery

        ccx.globalAlpha = alpha * 0.5;
        ccx.fillStyle = color;
        ccx.shadowColor = color;
        ccx.shadowBlur = 10;
        ccx.beginPath();
        ccx.arc(pos.x, pos.y, size * 1.6, 0, Math.PI * 2);
        ccx.fill();

        ccx.globalAlpha = alpha;
        ccx.shadowBlur = 6;
        ccx.beginPath();
        ccx.arc(pos.x, pos.y, size, 0, Math.PI * 2);
        ccx.fill();

        // Подпись клиента
        if (t < 0.25 && p.clientName) {
          ccx.fillStyle = color;
          ccx.font = 'bold 7px ' + MA.vF();
          ccx.textAlign = 'center';
          ccx.globalAlpha = alpha * 0.8;
          ccx.fillText(MA.trunc(p.clientName, 10), pos.x, pos.y - size - 6);
        }

        ccx.globalAlpha = 1;
        ccx.shadowBlur = 0;

      } else if (p.type === 'warming') {
        // ---- WARMING: оранжевая частица, движение к бэкенду ----
        var pos = interpolatePath(path, t);
        var size = (p.size || 3) * (1 + 0.4 * Math.sin(elapsed * 0.012));
        var alpha = 0.6 + 0.4 * Math.min(1, t * 2);
        if (t > 0.8) alpha *= Math.max(0, (1 - t) / 0.2);

        var color = '#f97316'; // оранжевый для warming

        ccx.globalAlpha = alpha * 0.5;
        ccx.fillStyle = color;
        ccx.shadowColor = color;
        ccx.shadowBlur = 12;
        ccx.beginPath();
        ccx.arc(pos.x, pos.y, size * 1.7, 0, Math.PI * 2);
        ccx.fill();

        ccx.globalAlpha = alpha;
        ccx.shadowBlur = 8;
        ccx.beginPath();
        ccx.arc(pos.x, pos.y, size, 0, Math.PI * 2);
        ccx.fill();

        // Подпись модели
        if (t < 0.3 && p.modelName) {
          ccx.fillStyle = color;
          ccx.font = 'bold 7px ' + MA.vF();
          ccx.textAlign = 'center';
          ccx.globalAlpha = alpha * 0.8;
          ccx.fillText('⏳ ' + MA.trunc(p.modelName, 12), pos.x, pos.y - size - 6);
        }

        ccx.globalAlpha = 1;
        ccx.shadowBlur = 0;

      } else {
        // ---- Полный путь или завершение ----
        var pos = interpolatePath(path, t);
        var color;
        if (p.type === 'complete') {
          color = '#22c55e';
        } else {
          // pathColor для full
          var colors = [
            { stop: 0.00, color: '#3b82f6' },
            { stop: 0.30, color: '#f59e0b' },
            { stop: 0.55, color: '#a855f7' },
            { stop: 0.85, color: '#22c55e' },
          ];
          var pt = t;
          if (pt <= colors[0].stop) color = colors[0].color;
          else if (pt >= colors[colors.length-1].stop) color = colors[colors.length-1].color;
          else {
            for (var ci = 0; ci < colors.length - 1; ci++) {
              if (pt >= colors[ci].stop && pt < colors[ci+1].stop) {
                var local = (pt - colors[ci].stop) / (colors[ci+1].stop - colors[ci].stop);
                color = colors[ci].color; // упрощённо, без lerp для скорости
                break;
              }
            }
          }
          if (!color) color = '#22c55e';
        }

        var size = (p.size || 2.5) * (1 + 0.2 * Math.sin(elapsed * 0.01));
        var alpha = 0.6 + 0.4 * Math.min(1, t * 3);
        if (t > 0.7) alpha = alpha * Math.max(0, (1 - t) / 0.3);

        ccx.globalAlpha = alpha;
        ccx.fillStyle = color;
        ccx.shadowColor = color;
        ccx.shadowBlur = 6;
        ccx.beginPath();
        ccx.arc(pos.x, pos.y, size, 0, Math.PI * 2);
        ccx.fill();

        // Хвостовой след
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

        ccx.globalAlpha = 1;
        ccx.shadowBlur = 0;
      }

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

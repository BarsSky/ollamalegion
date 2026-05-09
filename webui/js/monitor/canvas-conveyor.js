// js/monitor/canvas-conveyor.js — Conveyor belt animation (conveyorCanvas)
// Анимированные частицы, показывающие полный путь запроса:
// клиент → балансер (очередь) → бэкенд → завершено
// Цвет меняется по пути: 🔵(в пути) → 🟡(ожидание) → 🟣(обработка) → 🟢(готово)

(function() {
  'use strict';

  var MA = window.MonitorApp;

  var ccv = document.getElementById('conveyorCanvas'), ccx = ccv ? ccv.getContext('2d') : null;

  function isLightTheme() {
    return document.documentElement.getAttribute('data-theme') === 'light';
  }

  // ---- Вспомогательные функции для цветов ----
  function lerpColor(c1, c2, t) {
    var r1 = parseInt(c1.slice(1,3), 16), g1 = parseInt(c1.slice(3,5), 16), b1 = parseInt(c1.slice(5,7), 16);
    var r2 = parseInt(c2.slice(1,3), 16), g2 = parseInt(c2.slice(3,5), 16), b2 = parseInt(c2.slice(5,7), 16);
    var r = Math.round(r1 + (r2 - r1) * t);
    var g = Math.round(g1 + (g2 - g1) * t);
    var b = Math.round(b1 + (b2 - b1) * t);
    return '#' + [r,g,b].map(function(c) { return c.toString(16).padStart(2,'0'); }).join('');
  }

  // Цвет по прогрессу вдоль пути (0.0 → 1.0)
  function pathColor(t) {
    var colors = [
      { stop: 0.00, color: '#3b82f6' },  // 🔵 синий — в пути к балансеру
      { stop: 0.30, color: '#f59e0b' },  // 🟡 жёлтый — в очереди у балансера
      { stop: 0.55, color: '#a855f7' },  // 🟣 фиолетовый — обрабатывается бэкендом
      { stop: 0.85, color: '#22c55e' },  // 🟢 зелёный — завершён
    ];
    if (t <= colors[0].stop) return colors[0].color;
    if (t >= colors[colors.length-1].stop) return colors[colors.length-1].color;
    for (var i = 0; i < colors.length - 1; i++) {
      if (t >= colors[i].stop && t < colors[i+1].stop) {
        var local = (t - colors[i].stop) / (colors[i+1].stop - colors[i].stop);
        return lerpColor(colors[i].color, colors[i+1].color, local);
      }
    }
    return colors[colors.length-1].color;
  }

  // ---- Интерполяция по waypoints (как в canvas-topology.js) ----
  function interpolatePath(path, t) {
    if (!path || path.length < 2) return { x: 0, y: 0 };
    var segCount = path.length - 1;
    var segLen = 1.0 / segCount;
    var segIdx = Math.min(Math.floor(t / segLen), segCount - 1);
    var segT = (t - segIdx * segLen) / segLen;
    var segEase = 1 - Math.pow(1 - segT, 2);  // ease-out
    var p0 = path[segIdx], p1 = path[segIdx + 1];
    return {
      x: p0.x + (p1.x - p0.x) * segEase,
      y: p0.y + (p1.y - p0.y) * segEase
    };
  }

  function drawConveyor() {
    if (!ccv || !ccx) return;
    var w = ccv.parentElement.clientWidth;
    var backends = MA.topo.backends || [];
    var h = Math.max(100, backends.length * 40 + 20);
    ccv.width = w; ccv.height = h;
    ccv.style.width = w + 'px'; ccv.style.height = h + 'px';
    ccx.clearRect(0, 0, w, h);

    if (MA.topo.backends.length === 0) return;

    // ---- Геометрия макета ----
    var bw = 180; // ширина балансера
    var balInX = w / 2 - bw / 2;      // левый край балансера
    var balOutX = w / 2 + bw / 2;     // правый край балансера
    var balCenterX = w / 2;
    var balCenterY = h / 2;

    // Границы для клиентов (левая зона) и бэкендов (правая зона)
    var leftBand = balInX;
    var midBand = bw;
    var rightBand = w - balOutX;

    // Y-позиции для клиентов и бэкендов (как в canvas-topology.js через stableBackendOrder)
    var sessions = MA.topo.sessions || [];
    var backends = MA.topo.backends || [];
    var activeTotal = backends.reduce(function(s, b) { return s + (b.activeRequests || 0); }, 0);
    var maxTotal = backends.reduce(function(s, b) { return s + (b.maxConcurrentRequests || 10); }, 0);
    var utilization = maxTotal > 0 ? activeTotal / maxTotal : 0;

    // Реальные данные очереди и сессий
    var pendCount = MA.topo.queue.pending_count || 0;
    var procCount = MA.topo.queue.processing_count || 0;
    var queueCurrent = MA.topo.queue.current_size || 0;
    var sessionsCount = sessions.length;
    var totalRequests = sessions.reduce(function(s, sess) { return s + (sess.requestCount || 0); }, 0);

    // RPS (requests per second) — вычисляем по разнице во времени
    var now = Date.now();
    var timeDelta = (now - MA.lastTime) / 1000;
    var rps = 0;
    if (timeDelta > 0.5 && MA.lastTotalRequests !== undefined && MA.lastTotalRequests > 0) {
      rps = Math.max(0, (totalRequests - MA.lastTotalRequests) / timeDelta);
    }
    MA.lastTotalRequests = totalRequests;
    MA.lastTime = now;

    // Интенсивность спавна частиц на основе реальной нагрузки
    // Базовый шанс — от загрузки (utilization) с учётом RPS
    var spawnIntensity = Math.max(0.05, Math.min(0.50, utilization * 0.4 + rps * 0.02));

    // ---- Подписи зон ----
    var lt = isLightTheme();
    var labelColor = lt ? 'rgba(0,0,0,0.3)' : 'rgba(255,255,255,0.2)';
    ccx.fillStyle = labelColor;
    ccx.font = '10px ' + MA.vF();
    ccx.textAlign = 'center';
    ccx.fillText(MA.T('monitor.canvas.clients'), leftBand / 2, h - 6);
    ccx.fillText(MA.T('monitor.canvas.balancer'), w / 2, h - 6);
    ccx.fillText(MA.T('monitor.canvas.backends'), balOutX + rightBand / 2, h - 6);


    // Разделительные линии
    ccx.strokeStyle = lt ? 'rgba(0,0,0,0.08)' : 'rgba(255,255,255,0.05)';
    ccx.lineWidth = 1;
    ccx.setLineDash([4, 6]);
    ccx.beginPath(); ccx.moveTo(balInX, 10); ccx.lineTo(balInX, h - 14); ccx.stroke();
    ccx.beginPath(); ccx.moveTo(balOutX, 10); ccx.lineTo(balOutX, h - 14); ccx.stroke();
    ccx.setLineDash([]);

    // Индикатор RPS и нагрузки под лейблами
    ccx.fillStyle = labelColor;
    ccx.font = '9px ' + MA.vF();
    ccx.fillText(MA.T('monitor.canvas.rpsLabel', { rps: rps.toFixed(1), active: activeTotal, max: maxTotal }), w / 2, 10);


    // ============================================================
    // 1. ПОЛНЫЙ МАРШРУТ: клиент → балансер → бэкенд → завершено
    // Основано на реальных сессиях и бэкендах
    // ============================================================
    if (activeTotal > 0 && Math.random() < spawnIntensity) {
      // Выбираем реального клиента (сессию), если есть
      var clientIdx, sessionY;
      if (sessions.length > 0) {
        clientIdx = Math.floor(Math.random() * sessions.length);
        sessionY = 20 + (sessions.length > 1 ? clientIdx * (h - 40) / Math.max(1, sessions.length - 1) : balCenterY);
      } else {
        sessionY = balCenterY;
      }

      // Выбираем бэкенд пропорционально его активным запросам
      var backendIdx = 0;
      if (backends.length > 1) {
        var weightedSum = backends.reduce(function(s, b) { return s + Math.max(1, b.activeRequests || 1); }, 0);
        var rand = Math.random() * weightedSum;
        for (var bi = 0; bi < backends.length; bi++) {
          rand -= Math.max(1, backends[bi].activeRequests || 1);
          if (rand <= 0) { backendIdx = bi; break; }
        }
      }
      var backend = backends[backendIdx];
      var backendY = 20 + (backends.length > 1 ? backendIdx * (h - 40) / Math.max(1, backends.length - 1) : balCenterY);

      // Точки маршрута: синхронизировано с canvas-topology.js
      // клиент (x=140) → эльбоу (tX_left) → балансер → бэкенд (x=w-140)
      var tX_left = 140 + (balInX - 140) * 0.65;
      var path = [
        {x: 140, y: sessionY},
        {x: tX_left, y: sessionY},
        {x: tX_left, y: balCenterY},
        {x: balInX, y: balCenterY},
        {x: balOutX, y: balCenterY},
        {x: balOutX, y: backendY},
        {x: w - 140, y: backendY}
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
    // 2. ЧАСТИЦЫ ОЧЕРЕДИ: орбита внутри балансера
    // Интенсивность = реальное количество задач в очереди
    // ============================================================
    if (queueCurrent > 0 && Math.random() < Math.min(0.30, queueCurrent * 0.03)) {
      var orbitAngle = Math.random() * Math.PI * 2;
      var orbitRadius = 15 + Math.random() * 35;
      var ox = balCenterX + Math.cos(orbitAngle) * orbitRadius;
      var oy = balCenterY + Math.sin(orbitAngle) * orbitRadius * 0.5;

      var path = [
        {x: ox, y: oy},
        {x: balCenterX + Math.cos(orbitAngle + 0.5) * orbitRadius,
         y: balCenterY + Math.sin(orbitAngle + 0.5) * orbitRadius * 0.5}
      ];
      MA.convQueue.push({
        path: path,
        spawnTime: now,
        duration: 2000 + Math.random() * 1500,
        type: 'waiting',
        size: 3.5 + Math.random() * 1,
        waitCount: queueCurrent // реальный размер очереди
      });
    }

    // ============================================================
    // 3. ЗАВЕРШАЮЩИЕ ЧАСТИЦЫ — строго вдоль topology-линий бэкендов
    // Путь от правого края бэкенда (w-140) назад к балансеру,
    // синхронизировано с canvas-topology.js
    // ============================================================
    if (procCount > 0 && Math.random() < Math.min(0.25, procCount * 0.05)) {
      var bIdx;
      if (backends.length > 0) {
        // Выбираем бэкенд с активными запросами
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
      var bY = 20 + (backends.length > 1 ? bIdx * (h - 40) / Math.max(1, backends.length - 1) : balCenterY);

      // Точка излома и финиша, как в canvas-topology.js
      var tX = balOutX + (w - 140 - balOutX) * 0.35;
      var eX = w - 140;

      var path = [
        {x: tX, y: bY},
        {x: eX, y: bY}
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

      if (p.type === 'waiting') {
        // ---- Частицы очереди: орбитальное движение ----
        var pos = interpolatePath(path, t);
        var pulse = 1 + 0.2 * Math.sin(elapsed * 0.008 + (p.waitCount || 0));
        var ox = pos.x + Math.sin(elapsed * 0.003 + i) * 8 * pulse;
        var oy = pos.y + Math.cos(elapsed * 0.004 + i) * 5 * pulse;

        ccx.globalAlpha = 0.6 + 0.4 * Math.sin(elapsed * 0.005);
        ccx.fillStyle = '#f59e0b';
        ccx.shadowColor = '#f59e0b';
        ccx.shadowBlur = 8;
        ccx.beginPath();
        ccx.arc(ox, oy, 3 * pulse, 0, Math.PI * 2);
        ccx.fill();

        ccx.fillStyle = '#fff';
        ccx.font = '8px ' + MA.vF();
        ccx.textAlign = 'center';
        ccx.fillText('+' + p.waitCount, ox, oy - 8);
        ccx.globalAlpha = 1;
        ccx.shadowBlur = 0;

      } else {
        // ---- Полный путь или завершение: интерполяция по waypoints ----
        var pos = interpolatePath(path, t);

        // Динамический цвет по прогрессу
        var color;
        if (p.type === 'complete') {
          color = '#22c55e'; // зелёный
        } else {
          color = pathColor(t);
        }

        // Размер с пульсацией
        var size = (p.size || 2.5) * (1 + 0.2 * Math.sin(elapsed * 0.01));

        // Прозрачность: плавное появление и затухание
        var alpha = 0.6 + 0.4 * Math.min(1, t * 3);
        if (t > 0.7) alpha = alpha * Math.max(0, (1 - t) / 0.3);

        ccx.globalAlpha = alpha;
        ccx.fillStyle = color;
        ccx.shadowColor = color;
        ccx.shadowBlur = 6;
        ccx.beginPath();
        ccx.arc(pos.x, pos.y, size, 0, Math.PI * 2);
        ccx.fill();

        // Хвостовой след (меньшая точка позади)
        if (path.length >= 2) {
          var segCount = path.length - 1;
          var segLen = 1.0 / segCount;
          var tBack = Math.max(0, t - segLen * 0.25);
          var posBack = interpolatePath(path, tBack);
          ccx.globalAlpha = alpha * 0.3;
          ccx.beginPath();
          ccx.arc(posBack.x, posBack.y, size * 0.6, 0, Math.PI * 2);
          ccx.fill();
        }

        ccx.globalAlpha = 1;
        ccx.shadowBlur = 0;
      }

      // Удаление завершённых частиц
      if (t >= 1.0) {
        MA.convQueue.splice(i, 1);
      }
    }

    // Лимит частиц
    if (MA.convQueue.length > 200) {
      MA.convQueue.splice(0, MA.convQueue.length - 200);
    }
  }

  window.drawConveyor = drawConveyor;
})();

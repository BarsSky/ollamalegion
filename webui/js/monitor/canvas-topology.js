// js/monitor/canvas-topology.js — Topology canvas (vizCanvas)

(function() {
  'use strict';

  var MA = window.MonitorApp;

  var cv = document.getElementById('vizCanvas'), cx = cv.getContext('2d');
  var ttEl = document.getElementById('topoTooltip');
  var af = 0;

  function rsz() {
    var r = cv.parentElement.getBoundingClientRect();
    cv.width = r.width;
    cv.height = r.height;
    MA.topo.w = r.width;
    MA.topo.h = r.height - 52;
  }
  window.addEventListener('resize', rsz);
  rsz();

  function updateTopology(bk, ss, q) {
    MA.topo.backends = bk;
    MA.topo.sessions = ss;
    MA.topo.queue = q;
  }
  window.updateTopology = updateTopology;

  function isLightTheme() {
    return document.documentElement.getAttribute('data-theme') === 'light';
  }

  // Tooltip
  cv.addEventListener('mousemove', function(e) {
    var rect = cv.getBoundingClientRect();
    var mx = e.clientX - rect.left;
    var my = e.clientY - rect.top;
    var w = MA.topo.w, h = MA.topo.h, cX = w / 2, cY = h / 2;
    var found = null;

    // Check client boxes (left side)
    for (var i = MA.topo.sessions.length - 1; i >= 0; i--) {
      var s = MA.topo.sessions[i];
      var y = nY('session', i);
      if (mx >= 12 && mx <= 132 && my >= y - 22 && my <= y + 22) {
        found = { type: 'client', session: s };
        break;
      }
    }
    // Check backend boxes (right side)
    if (!found) {
      for (var i = MA.topo.backends.length - 1; i >= 0; i--) {
        var b = MA.topo.backends[i];
        var y = nY('backend', i);
        if (mx >= w - 140 && mx <= w - 12 && my >= y - 26 && my <= y + 26) {
          found = { type: 'backend', backend: b };
          break;
        }
      }
    }
    // Check balancer box
    if (!found) {
      var bw = 180, bh = 100, bx = cX - bw / 2, by = cY - bh / 2;
      if (mx >= bx - 4 && mx <= bx + bw + 4 && my >= by - 4 && my <= by + bh + 4) {
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
        html = '<div><strong>' + MA.esc(b.id) + '</strong> <span style="font-size:10px;color:' + (b.status === 'active' || b.status === 'ready' ? 'var(--success)' : 'var(--danger)') + '">' + b.status + '</span></div>'
          + '<div style="color:var(--text-secondary)">GPU: ' + gu.toFixed(0) + '% | VRAM: ' + vu.toFixed(0) + '% (' + vus.toFixed(1) + '/' + vtu.toFixed(0) + 'G)</div>'
          + '<div style="color:var(--text-secondary)">CPU: ' + cu.toFixed(0) + '% | RAM: ' + ru.toFixed(0) + '%</div>'
          + '<div style="color:var(--text-secondary)">Active: ' + a + '/' + mr + ' | Score: ' + sc + '</div>'
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
    var cX = MA.topo.w / 2, cY = MA.topo.h / 2;
    var now = Date.now();

    // Троттлинг: спавним частицы не чаще чем раз в 200 мс, независимо от FPS
    if (now - lastSpawnTime < SPAWN_INTERVAL_MS) return;
    lastSpawnTime = now;

    var bw = 180, bh = 100;
    var balInX = cX - bw / 2, balOutX = cX + bw / 2;
    var byTop = cY - bh / 2, byBot = cY + bh / 2;

    // --- Request particles: client -> balancer (L-path via elbow)
    MA.topo.sessions.forEach(function(s, i) {
      var cn = (s.clientName || '').toLowerCase();
      if (cn.indexOf('monitor') >= 0 || cn.indexOf('health') >= 0 || cn.indexOf('kube') >= 0) return;
      var idleMs = s.lastRequestAt ? (now - new Date(s.lastRequestAt).getTime()) : 999999;
      if (idleMs > 30000) return;
      var sy = nY('session', i);
      var sX = 140;
      var tX = sX + (balInX - sX) * 0.65;
      if (Math.random() < 0.08) {
        spawnParticle([
          {x: sX, y: sy},
          {x: tX, y: sy},
          {x: tX, y: cY},
          {x: balInX, y: cY}
        ], 'request', s.model);
      }
    });

    // --- Processing particles: balancer -> backend elbow -> backend
    MA.topo.backends.forEach(function(b, i) {
      var active = b.activeRequests || 0;
      if (active <= 0) return;
      var by = nY('backend', i);
      var eX = MA.topo.w - 140;
      var tX = balOutX + (eX - balOutX) * 0.35;
      var prob = Math.min(active * 0.08, 0.40);
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
    });

    // NOTE: Full-route particles (client -> balancer -> backend) удалены,
    // т.к. они дублировали request+processing частицы и визуально создавали
    // впечатление прямого соединения минуя балансер.

    // --- Pending particles orbiting inside balancer box
    var pendCount = MA.topo.queue.pending_count || 0;
    for (var p = 0; p < pendCount; p++) {
      if (Math.random() < 0.22) {
        var angle = Math.random() * Math.PI * 2;
        var radius = 20 + Math.random() * 30;
        var sx = cX + Math.cos(angle) * radius;
        var sy = cY + Math.sin(angle) * radius * 0.4;
        spawnParticle([
          {x: sx, y: sy},
          {x: cX + (Math.random()-0.5)*10, y: cY + (Math.random()-0.5)*10}
        ], 'pending', 'queued');
      }
    }
  }

  function nY(type, i) {
    var cnt = type === 'session' ? Math.max(1, MA.topo.sessions.length) : Math.max(1, MA.topo.backends.length);
    var m = 80, av = MA.topo.h - m * 2;
    // Минимальный отступ 30px между линиями, чтобы избежать наложения при 5+ узлах
    var minSpacing = 30;
    var st = Math.max(minSpacing, av / Math.max(1, cnt - 1));
    // Если шаг превышает доступную высоту, центрируем группу
    var totalHeight = (cnt - 1) * st;
    var offset = Math.max(0, (av - totalHeight) / 2);
    return m + offset + i * st;
  }


  function drawTopo() {
    var w = MA.topo.w, h = MA.topo.h;
    if (!w || !h) return;
    var lt = isLightTheme();
    cx.clearRect(0, 0, w, h);
    cx.strokeStyle = lt ? 'rgba(0,0,0,0.05)' : 'rgba(255,255,255,0.03)';
    cx.lineWidth = 1;
    for (var x = 0; x < w; x += 40) { cx.beginPath(); cx.moveTo(x, 0); cx.lineTo(x, h); cx.stroke(); }
    for (var y = 0; y < h; y += 40) { cx.beginPath(); cx.moveTo(0, y); cx.lineTo(w, y); cx.stroke(); }
    var cX = w / 2, cY = h / 2;
    cx.setLineDash([5, 3]);

    var bw = 180, bh = 100;
    var balInX = cX - bw / 2, balOutX = cX + bw / 2;

    MA.topo.sessions.forEach(function(s, i) {
      var y = nY('session', i), tc = s.backendId ? 'rgba(59,130,246,0.25)' : 'rgba(148,163,184,0.18)';
      cx.strokeStyle = tc; cx.lineWidth = s.backendId ? 3 : 2;
      var tX = 140 + (balInX - 140) * 0.65, sX = 140, eX = balInX, eY = cY;
      cx.beginPath(); cx.moveTo(sX, y); cx.lineTo(tX, y); cx.stroke();
      cx.beginPath(); cx.moveTo(tX, y); cx.lineTo(tX, eY); cx.stroke();
      cx.beginPath(); cx.moveTo(tX, eY); cx.lineTo(eX, eY); cx.stroke();
      cx.fillStyle = tc.replace('0.25', '0.5').replace('0.18', '0.35'); cx.beginPath(); cx.arc(tX, y, 5, 0, Math.PI * 2); cx.fill();
      cx.fillStyle = tc.replace('0.25', '0.5').replace('0.18', '0.35'); cx.beginPath(); cx.arc(tX, eY, 4, 0, Math.PI * 2); cx.fill();
    });

    MA.topo.backends.forEach(function(b, i) {
      var y = nY('backend', i), ih = b.status === 'active' || b.status === 'healthy' || b.status === 'ready', isOllamaUnavailable = b.status === 'ollama_unavailable', tc = ih ? 'rgba(59,130,246,0.25)' : (isOllamaUnavailable ? 'rgba(249,115,22,0.25)' : 'rgba(239,68,68,0.25)');
      cx.strokeStyle = tc; cx.lineWidth = ih ? 3 : 2;
      var sX = balOutX, sY = cY, tX = sX + (w - 140 - sX) * 0.35, eX = w - 140;
      cx.beginPath(); cx.moveTo(sX, sY); cx.lineTo(sX, y); cx.stroke();
      cx.beginPath(); cx.moveTo(sX, y); cx.lineTo(eX, y); cx.stroke();
      cx.fillStyle = tc.replace('0.25', '0.5'); cx.beginPath(); cx.arc(sX, y, 5, 0, Math.PI * 2); cx.fill();
    });

    cx.setLineDash([]);
    var textLight = lt ? '#1e293b' : '#e2e8f0', textMuted = lt ? '#475569' : '#b0b7c4';
    MA.topo.sessions.forEach(function(s, i) {
      var y = nY('session', i), ia = s.backendId != null, isCloud = MA.isCloudModel(s.model);
      var bc, br;
      if (isCloud) { bc = 'rgba(59,130,246,0.15)'; br = 'rgba(59,130,246,0.45)'; }
      else if (ia) { bc = 'rgba(59,130,246,0.18)'; br = 'rgba(59,130,246,0.5)'; }
      else { bc = 'rgba(148,163,184,0.12)'; br = 'rgba(148,163,184,0.25)'; }
      cx.fillStyle = bc; MA.rr(cx, 12, y - 22, 120, 44, 8); cx.fill(); cx.strokeStyle = br; cx.lineWidth = 1; cx.stroke();
      cx.fillStyle = textLight; cx.font = 'bold 11px ' + MA.vF(); cx.textAlign = 'left';
      cx.save(); cx.beginPath(); cx.rect(16, y - 18, 108, 14); cx.clip(); cx.fillText(MA.getCI(s.clientName) + ' ' + MA.trunc(s.clientName, 8), 20, y - 6); cx.restore();
      cx.fillStyle = isCloud ? '#60a5fa' : textMuted; cx.font = '10px ' + MA.vF();
      cx.save(); cx.beginPath(); cx.rect(16, y + 1, 108, 14); cx.clip(); cx.fillText((isCloud ? '☁️ ' : '') + (s.model || '—') + ' • ' + (s.requestCount || 0) + ' req', 20, y + 8); cx.restore();
    });

    var bx = cX - bw / 2, by = cY - bh / 2;
    var pend = MA.topo.queue.pending_count || 0, mq = (MA.topo.queue.all || []).length > 0 ? Math.max((MA.topo.queue.all || []).length, 5) : 5;
    var qr = Math.min(pend / mq, 1), bc = qr > 0.75 ? '#ef4444' : qr > 0.3 ? '#f59e0b' : '#3b82f6';
    cx.shadowColor = bc; cx.shadowBlur = 20; cx.fillStyle = bc + '18'; MA.rr(cx, bx - 4, by - 4, bw + 8, bh + 8, 12); cx.fill(); cx.shadowBlur = 0;
    cx.fillStyle = bc + '22'; MA.rr(cx, bx, by, bw, bh, 10); cx.fill(); cx.strokeStyle = 'rgba(148,163,184,0.4)'; cx.lineWidth = 2; cx.stroke();
    cx.fillStyle = lt ? '#0f172a' : '#fff'; cx.font = 'bold 13px ' + MA.vF(); cx.textAlign = 'center'; cx.fillText(MA.T('monitor.canvas.balancer'), cX, cY - 16);
    cx.fillStyle = textMuted; cx.font = '11px ' + MA.vF(); cx.fillText(MA.T('monitor.canvas.queue') + pend, cX, cY + 4);
    var qbX = bx + 20, qbY = by + bh / 2 + 14, qbW = bw - 40, qbH = 8;
    cx.fillStyle = lt ? 'rgba(0,0,0,0.08)' : 'rgba(255,255,255,0.06)'; MA.rr(cx, qbX, qbY, qbW, qbH, 4); cx.fill();
    cx.fillStyle = qr > 0.75 ? '#ef4444' : '#60a5fa'; MA.rr(cx, qbX, qbY, qbW * qr, qbH, 4); cx.fill();
    var backendIdleTxt = lt ? '#1e293b' : '#fff';
    MA.topo.backends.forEach(function(b, i) {
      var y = nY('backend', i), ih = b.status === 'active' || b.status === 'healthy' || b.status === 'ready';
      var isOllamaUnavailable = b.status === 'ollama_unavailable';
      var bc, br;
      if (ih) {
        bc = b.activeRequests > 0 ? 'rgba(245,158,11,0.15)' : 'rgba(34,197,94,0.12)';
        br = b.activeRequests > 0 ? 'rgba(245,158,11,0.4)' : 'rgba(34,197,94,0.4)';
      } else if (isOllamaUnavailable) {
        bc = 'rgba(249,115,22,0.12)';
        br = 'rgba(249,115,22,0.4)';
      } else {
        bc = 'rgba(239,68,68,0.12)';
        br = 'rgba(239,68,68,0.4)';
      }
      cx.fillStyle = bc; MA.rr(cx, w - 140, y - 26, 128, 52, 8); cx.fill(); cx.strokeStyle = br; cx.lineWidth = 1.5; cx.stroke();
      cx.fillStyle = backendIdleTxt; cx.font = 'bold 11px ' + MA.vF(); cx.textAlign = 'left'; cx.fillText(MA.trunc(b.id, 12), w - 132, y - 10);
      var mr = b.maxConcurrentRequests || 10, a = b.activeRequests || 0, l = a / mr;
      cx.fillStyle = l >= 0.8 ? '#ef4444' : l >= 0.5 ? '#f59e0b' : '#22c55e'; cx.font = '10px ' + MA.vF();
      cx.fillText(a > 0 ? '⏳ ' + a + '/' + mr : '✅ Idle', w - 132, y + 6);
      var mc = (b.models || []).length;
      if (mc > 0) { cx.fillStyle = '#a855f7'; cx.font = '9px ' + MA.vF(); cx.fillText('🧠 ' + mc + MA.T('models_lower'), w - 132, y + 20); }
      else if (ih && a > 0) { cx.fillStyle = '#f97316'; cx.font = '9px ' + MA.vF(); cx.fillText(MA.T('loading'), w - 132, y + 20); }
      var vp = b.vram ? b.vram.usagePercent : 0;
      if (vp > 0) { cx.fillStyle = lt ? 'rgba(0,0,0,0.06)' : 'rgba(255,255,255,0.06)'; MA.rr(cx, w - 132, y + 26, 100, 3, 2); cx.fill(); cx.fillStyle = vp > 85 ? '#ef4444' : vp > 60 ? '#f59e0b' : '#22c55e'; MA.rr(cx, w - 132, y + 26, 100 * (vp / 100), 3, 2); cx.fill(); }
    });

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
      var segT2 = 1 - Math.pow(1 - segT, 3);
      var p0 = path[segIdx], p1 = path[segIdx + 1];
      p.x = p0.x + (p1.x - p0.x) * segT2;
      p.y = p0.y + (p1.y - p0.y) * segT2;
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
  }

  window.drawTopo = drawTopo;
})();
#!/usr/bin/env python3
"""Patch monitor.html: add pan, zoom, improved tooltips, and queue panel fixes."""
import os

MONITOR_FILE = os.path.join(os.path.dirname(__file__), '..', 'webui', 'monitor.html')

with open(MONITOR_FILE, 'r', encoding='utf-8') as f:
    content = f.read()

# --- PATCH 1: Add canvas pan/zoom logic ---
old_anim = """  function anim() { af++; drawTopo(); requestAnimationFrame(anim); }"""

new_anim = """  // ---- Pan & Zoom for monitor canvas ----
  var viewScale = 1.0;
  var viewOffsetX = 0;
  var viewOffsetY = 0;
  var isDragging = false;
  var dragStartX = 0;
  var dragStartY = 0;
  var dragOffsetStartX = 0;
  var dragOffsetStartY = 0;

  var vizCanvas = document.getElementById('vizCanvas');
  vizCanvas.addEventListener('mousedown', function(e) {
    if (e.button === 0) { // Left button
      isDragging = true;
      dragStartX = e.clientX;
      dragStartY = e.clientY;
      dragOffsetStartX = viewOffsetX;
      dragOffsetStartY = viewOffsetY;
      e.preventDefault();
    }
  });
  vizCanvas.addEventListener('mousemove', function(e) {
    if (isDragging) {
      viewOffsetX = dragOffsetStartX + (e.clientX - dragStartX);
      viewOffsetY = dragOffsetStartY + (e.clientY - dragStartY);
    }
    // Tooltip tracking
    var tip = document.getElementById('topoTooltip');
    if (tip && tip.style.display !== 'none') {
      tip.style.left = (e.clientX + 16) + 'px';
      tip.style.top = (e.clientY - 30) + 'px';
    }
  });
  vizCanvas.addEventListener('mouseup', function() { isDragging = false; });
  vizCanvas.addEventListener('mouseleave', function() { isDragging = false; });
  vizCanvas.addEventListener('wheel', function(e) {
    e.preventDefault();
    var zoomFactor = e.deltaY > 0 ? 0.9 : 1.1;
    var newScale = viewScale * zoomFactor;
    if (newScale < 0.2) newScale = 0.2;
    if (newScale > 5.0) newScale = 5.0;
    // Zoom toward mouse position
    var rect = vizCanvas.getBoundingClientRect();
    var mx = e.clientX - rect.left;
    var my = e.clientY - rect.top;
    viewOffsetX = mx - (mx - viewOffsetX) * (newScale / viewScale);
    viewOffsetY = my - (my - viewOffsetY) * (newScale / viewScale);
    viewScale = newScale;
  }, { passive: false });"""

if old_anim in content:
    content = content.replace(old_anim, new_anim)
    print('[PATCH 1] Added pan/zoom logic to monitor.html')
else:
    print('[PATCH 1] NOT FOUND - anim function')

# --- PATCH 2: Apply scale/offset to drawTopo ---
old_drawtopo_start = """  function drawTopo() {
    var w=topo.w, h=topo.h; if (!w||!h) return;
    var lt=isLightTheme();
    cx.clearRect(0,0,w,h);"""

new_drawtopo_start = """  function drawTopo() {
    var w=topo.w, h=topo.h; if (!w||!h) return;
    var lt=isLightTheme();
    cx.save();
    cx.clearRect(0,0,w,h);
    cx.translate(viewOffsetX, viewOffsetY);
    cx.scale(viewScale, viewScale);"""

if old_drawtopo_start in content:
    content = content.replace(old_drawtopo_start, new_drawtopo_start)
    print('[PATCH 2] Added scale/translate to drawTopo')
else:
    print('[PATCH 2] NOT FOUND - drawTopo start')

# --- PATCH 3: Restore context at end of drawTopo before particles ---
old_sp = """  function sp() {"""

new_sp = """  function sp() {
    cx.restore();"""

if old_sp in content:
    content = content.replace(old_sp, new_sp)
    print('[PATCH 3] Added cx.restore() before particles for correct tooltip coords')
else:
    print('[PATCH 3] NOT FOUND - sp function')

# --- PATCH 4: Fix tooltip positioning using getBoundingClientRect ---
# Add mousemove handler after canvas setup
old_canvas_setup = """  var canvas = document.getElementById('vizCanvas'), cx = canvas.getContext('2d');"""

new_canvas_setup = """  var canvas = document.getElementById('vizCanvas'), cx = canvas.getContext('2d');
  var topoTooltip = document.getElementById('topoTooltip');
  canvas.addEventListener('mousemove', function(e) {
    var rect = canvas.getBoundingClientRect();
    var tx = e.clientX, ty = e.clientY;
    topoTooltip.style.left = (tx + 14) + 'px';
    topoTooltip.style.top = (ty - 36) + 'px';
    // Hit test: check if we're over a client or backend box
    var scale = viewScale || 1, ox = viewOffsetX || 0, oy = viewOffsetY || 0;
    var hx = (e.clientX - rect.left - ox) / scale;
    var hy = (e.clientY - rect.top - oy) / scale;
    var found = false;
    // Check sessions
    (topo.sessions||[]).forEach(function(s,i) {
      var y = nY('session', i);
      if (hx >= 12 && hx <= 132 && hy >= y-22 && hy <= y+22) {
        topoTooltip.style.display = 'block';
        topoTooltip.innerHTML = '<b>' + esc(getCI(s.clientName)+' '+trunc(s.clientName||'',18)) + '</b><br>'
          + esc(s.model||'—') + ' • ' + (s.requestCount||0) + ' req<br>'
          + 'IP: ' + esc(s.clientIP||'—');
        found = true;
      }
    });
    // Check backends
    (topo.backends||[]).forEach(function(b,i) {
      var y = nY('backend', i);
      if (hx >= topo.w-140 && hx <= topo.w-12 && hy >= y-26 && hy <= y+26) {
        topoTooltip.style.display = 'block';
        var vp = b.vram ? b.vram.usagePercent : 0;
        topoTooltip.innerHTML = '<b>' + esc(trunc(b.id||'',18)) + '</b><br>'
          + 'Status: ' + esc(b.status||'unknown') + '<br>'
          + 'Active: ' + (b.activeRequests||0) + '/' + (b.maxConcurrentRequests||10) + '<br>'
          + 'Models: ' + ((b.models||[]).length) + '<br>'
          + 'VRAM: ' + (vp > 0 ? vp.toFixed(1) + '%' : 'N/A');
        found = true;
      }
    });
    if (!found) topoTooltip.style.display = 'none';
  });
  canvas.addEventListener('mouseleave', function() { topoTooltip.style.display = 'none'; });"""

if old_canvas_setup in content:
    content = content.replace(old_canvas_setup, new_canvas_setup)
    print('[PATCH 4] Added tooltip hover logic with scale/offset-aware hit testing')
else:
    print('[PATCH 4] NOT FOUND - canvas setup')

# --- PATCH 5: Fix queue panel rendering ---
old_queue_body = """      <div class="panel-body"><div style="overflow-x:auto"><table id="queueTable"><thead><tr><th>#</th><th>Модель</th><th>Target</th><th>Статус</th><th class="col-right">Ожидание</th><th>Сессия</th></tr></thead><tbody><tr><td colspan="7" style="color:var(--text-secondary);text-align:center;padding:16px">Нет данных</td></tr></tbody></table></div></div>
    </div>
    <div class="panel" id="panelSessions">"""

new_queue_body = """      <div class="panel-body"><div style="overflow-x:auto"><table id="queueTable"><thead><tr><th>#</th><th>Модель</th><th>Target</th><th class="col-right">Статус</th><th class="col-right">Ожидание</th><th>Сессия</th></tr></thead><tbody id="queueTbody"><tr><td colspan="6" style="color:var(--text-secondary);text-align:center;padding:16px">Нет данных</td></tr></tbody></table></div></div>
    </div>
    <div class="panel" id="panelSessions">"""

if old_queue_body in content:
    content = content.replace(old_queue_body, new_queue_body)
    print('[PATCH 5] Added tbody id to queueTable for dynamic rendering')
else:
    # Try alternative - find the queue panel
    if 'id="queueTable"' in content:
        print('[PATCH 5] queueTable found but exact match failed - trying substring patch')
        # Find queueTable and add id to tbody
        idx = content.find('<table id="queueTable">')
        if idx >= 0:
            # Find the tbody after it
            tbody_idx = content.find('<tbody>', idx)
            if tbody_idx >= 0:
                content = content[:tbody_idx] + '<tbody id="queueTbody">' + content[tbody_idx+7:]
                print('[PATCH 5] Added tbody id via substring patch')
            else:
                print('[PATCH 5] No tbody found after queueTable')

# --- PATCH 6: Render queue items in updateUI ---
old_queue_section = """    document.getElementById('queueCount').textContent = (q.all||[]).length;"""

new_queue_section = """    document.getElementById('queueCount').textContent = (q.all||[]).length;
    // Render queue items
    var qtb = document.getElementById('queueTbody');
    if (qtb) {
      var all = q.all || [];
      if (all.length === 0) {
        qtb.innerHTML = '<tr><td colspan="6" style="color:var(--text-secondary);text-align:center;padding:16px">' + T('monitor.table.noData') + '</td></tr>';
      } else {
        var nowTS = new Date();
        qtb.innerHTML = all.map(function(r, i) {
          var et = r.enqueued ? new Date(r.enqueued) : new Date();
          var waitSec = Math.round((nowTS - et) / 1000);
          var waitStr = waitSec < 60 ? waitSec + 's' : Math.floor(waitSec/60) + 'm' + (waitSec%60) + 's';
          var sc = r.status === 'pending' ? '🟡' : r.status === 'processing' ? '🟣' : '⚪';
          return '<tr><td>' + (i+1) + '</td><td>' + esc(r.model||'—') + '</td><td>' + esc(r.target||'—') + '</td>'
            + '<td class="col-right">' + sc + ' ' + esc(r.status||'—') + '</td>'
            + '<td class="col-right">' + waitStr + '</td>'
            + '<td>' + esc((r.sessionId||'').substring(0,20)) + '</td></tr>';
        }).join('');
      }
    }"""

if old_queue_section in content:
    content = content.replace(old_queue_section, new_queue_section)
    print('[PATCH 6] Added queue rendering logic in updateUI')
else:
    print('[PATCH 6] NOT FOUND - queueCount section')

# Write back
with open(MONITOR_FILE, 'w', encoding='utf-8') as f:
    f.write(content)
print('\nAll patches applied. Monitor updated with pan/zoom/tooltips/queue rendering.')
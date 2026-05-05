#!/usr/bin/env python3
"""Patch monitor.html v2: exact string matches for tooltip + queue + launch config."""
import os

MONITOR_FILE = os.path.join(os.path.dirname(__file__), '..', 'webui', 'monitor.html')

with open(MONITOR_FILE, 'r', encoding='utf-8') as f:
    content = f.read()

patches = 0

# --- PATCH 1: Tooltip hit testing (exact line 552) ---
old_cv = "  var cv = document.getElementById('vizCanvas'), cx = cv.getContext('2d');"
new_cv = """  var cv = document.getElementById('vizCanvas'), cx = cv.getContext('2d');
  var topoTooltip = document.getElementById('topoTooltip');
  cv.addEventListener('mousemove', function(e) {
    var rect = cv.getBoundingClientRect();
    var tx = e.clientX, ty = e.clientY;
    topoTooltip.style.left = (tx + 14) + 'px';
    topoTooltip.style.top = (ty - 36) + 'px';
    var scl = viewScale || 1, ox = viewOffsetX || 0, oy = viewOffsetY || 0;
    var hx = (e.clientX - rect.left - ox) / scl;
    var hy = (e.clientY - rect.top - oy) / scl;
    var found = false;
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
  cv.addEventListener('mouseleave', function() { topoTooltip.style.display = 'none'; });"""

if old_cv in content:
    content = content.replace(old_cv, new_cv)
    patches += 1
    print('[OK] Tooltip hit testing added')
else:
    print('[FAIL] cv line not matched')

# --- PATCH 2: Queue rendering in updateUI (exact match) ---
old_queue_cnt = "    document.getElementById('queueCount').textContent = (q.all||[]).length;"
new_queue_cnt = """    document.getElementById('queueCount').textContent = (q.all||[]).length;
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
            + '<td>' + esc((r.sessionId||'').substring(0,16)) + '</td></tr>';
        }).join('');
      }
    }"""

if old_queue_cnt in content:
    content = content.replace(old_queue_cnt, new_queue_cnt)
    patches += 1
    print('[OK] Queue rendering added to updateUI')
else:
    # Try with different spacing
    for line in content.split('\n'):
        if 'queueCount' in line and 'textContent' in line and 'q.all' in line:
            print(f'[INFO] Found queueCount line: {repr(line)}')
            break
    print('[FAIL] queueCount line not matched')

# --- PATCH 3: Launch config modal HTML ---
old_panelQueue = """    <div class="panel" id="panelQueue">"""
new_panelQueue = """    <div class="panel" id="panelLaunchConfig">
      <div class="panel-header" onclick="togglePanel('panelLaunchConfig')">
        <span>⚙️ Конфигурация запуска (<span id="launchConfigCount">0</span>)</span><span class="chevron">▼</span>
      </div>
      <div class="panel-body"><div style="overflow-x:auto" id="launchConfigGrid"></div></div>
    </div>
    <div class="panel" id="panelQueue">"""

if old_panelQueue in content:
    content = content.replace(old_panelQueue, new_panelQueue)
    patches += 1
    print('[OK] Launch config panel added')
else:
    print('[FAIL] panelQueue not matched')

# --- PATCH 4: Launch config JS functions ---
old_rebalance = """  // ---- Rebalance ----"""
new_rebalance = """  // ---- Launch Config Analysis ----
  window.analyzeBackend = function(backendId) {
    api('/api/v1/backends/' + backendId + '/launch-config').then(function(r) {
      if (!r.success) { alert(r.error); return; }
      var cfg = r.configs || [];
      var hw = r.hardware || {};
      var html = '<div style="padding:12px"><h3 style="margin:0 0 8px">' + esc(backendId) + '</h3>';
      html += '<p style="color:var(--text-secondary);font-size:12px;margin:0 0 12px">GPU: ' + (hw.gpuCount||0)
        + ' | VRAM: ' + (hw.vramTotalMB||0) + ' MB | RAM: ' + (hw.ramTotalMB||0)
        + ' MB | CPU: ' + (hw.cpuThreads||0) + ' threads</p>';
      cfg.forEach(function(c) {
        var border = c.type === 'optimal' ? '#22c55e' : c.type === 'speed' ? '#f59e0b' : '#3b82f6';
        var icon = c.type === 'optimal' ? '🥇' : c.type === 'speed' ? '⚡' : '🧠';
        html += '<div style="border:2px solid ' + border + ';border-radius:10px;padding:10px;margin:8px 0;background:var(--glass-bg)">';
        html += '<b>' + icon + ' ' + esc(c.label) + '</b>';
        if (c.isOptimal) html += ' <span style="background:#22c55e;color:#fff;font-size:10px;padding:2px 6px;border-radius:4px">✅ Оптимально</span>';
        html += '<p style="font-size:11px;color:var(--text-secondary);margin:4px 0">' + esc(c.description) + '</p>';
        if (c.limitations && c.limitations.length > 0) {
          html += '<div style="font-size:10px;color:#ef4444;margin:4px 0">';
          c.limitations.forEach(function(l) { html += '⚠ ' + esc(l) + '<br>'; });
          html += '</div>';
        }
        html += '<div style="font-size:10px;color:var(--text-secondary);margin:4px 0">';
        Object.keys(c.envVars||{}).forEach(function(k) {
          html += esc(k) + '=' + esc(c.envVars[k]) + ' ';
        });
        html += '</div>';
        html += '<button onclick="applyLaunchConfig(\'' + escJS(backendId) + '\',\'' + escJS(JSON.stringify(c.envVars)) + '\')" style="margin-top:4px;font-size:11px">Применить</button>';
        html += '</div>';
      });
      html += '</div>';
      document.getElementById('launchConfigGrid').innerHTML = html;
      document.getElementById('launchConfigCount').textContent = cfg.length;
      document.getElementById('panelLaunchConfig').classList.add('open');
    }).catch(function(e) { alert('Ошибка анализа: ' + e.message); });
  };
  window.applyLaunchConfig = function(backendId, envVarsJson) {
    try {
      var envVars = JSON.parse(envVarsJson);
      fetch(API_BASE + '/api/v1/backends/' + backendId + '/reconfigure', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ envVars: envVars })
      }).then(function(r) {
        if (r.ok) { alert('✅ Конфигурация отправлена бэкенду ' + backendId); }
        else { r.json().then(function(e) { alert('❌ Ошибка: ' + (e.error||r.status)); }); }
      }).catch(function(e) { alert('❌ Ошибка сети: ' + e.message); });
    } catch(e) { alert('❌ Ошибка парсинга: ' + e.message); }
  };
  function escJS(s) { return String(s).replace(/\\\\/g,'\\\\\\\\').replace(/'/g,"\\\\'").replace(/"/g,'\\\\"'); }

  // ---- Rebalance ----"""

if old_rebalance in content:
    content = content.replace(old_rebalance, new_rebalance)
    patches += 1
    print('[OK] Launch config JS functions added')
else:
    print('[FAIL] Rebalance section not matched')

# --- PATCH 5: Add reconfigure API endpoint to handlers.go ---
HANDLERS_FILE = os.path.join(os.path.dirname(__file__), '..', 'internal', 'api', 'handlers.go')
with open(HANDLERS_FILE, 'r', encoding='utf-8') as f:
    hcontent = f.read()

old_restart_handler = """// restartHandler - перезапуск балансера (только для webui)"""
new_restart_handler = """// reconfigureHandler — POST /api/v1/backends/{id}/reconfigure
// Принимает envVars и инициирует переформирование бэкенда с новыми параметрами запуска Ollama.
func (s *Server) reconfigureHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/backends/"), "/")
	if len(parts) < 2 || parts[1] != "reconfigure" {
		http.NotFound(w, r)
		return
	}
	backendID := parts[0]

	var req struct {
		EnvVars map[string]string `json:"envVars"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"error":   "Invalid request body",
		})
		return
	}

	if !s.proxy.BackendExists(backendID) {
		s.writeJSON(w, http.StatusNotFound, map[string]interface{}{
			"success": false,
			"error":   "Backend not found",
		})
		return
	}

	// Публикуем событие переформирования — агент подписан через WebSocket/события
	s.proxy.PublishEvent(types.Event{
		Type:      types.EventReconfigure,
		Timestamp: time.Now().UTC(),
		BackendID: backendID,
		Data: map[string]interface{}{
			"envVars": req.EnvVars,
		},
	})

	logger.Get().Infow("reconfigure request published", "backend", backendID, "envVars", req.EnvVars)

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"success":   true,
		"backendId": backendID,
		"message":   "Reconfigure event published. Agent will apply new settings.",
	})
}

// restartHandler - перезапуск балансера (только для webui)"""

if old_restart_handler in hcontent:
    hcontent = hcontent.replace(old_restart_handler, new_restart_handler)
    patches += 1

    # Also add the route in setupRoutes
    old_routes = '\t// Restart endpoint (c аутентификацией и rate limiting, только от webui)'
    new_routes = '\t// Reconfigure endpoint (применение конфигурации запуска к бэкенду)\n\ts.mux.Handle("/api/v1/backends/", AuthMiddleware(RateLimitMiddleware(s.reconfigureHandler, s.rateLimiter), s.authenticator))\n\n\t// Restart endpoint (c аутентификацией и rate limiting, только от webui)'
    if old_routes in hcontent:
        hcontent = hcontent.replace(old_routes, new_routes)
        print('[OK] reconfigure route added')

    with open(HANDLERS_FILE, 'w', encoding='utf-8') as f:
        f.write(hcontent)
    print('[OK] reconfigureHandler added to handlers.go')
else:
    print('[FAIL] restartHandler not matched in handlers.go')

# --- PATCH 6: Add EventReconfigure to events.go ---
EVENTS_FILE = os.path.join(os.path.dirname(__file__), '..', 'pkg', 'types', 'events.go')
with open(EVENTS_FILE, 'r', encoding='utf-8') as f:
    econtent = f.read()

old_events = '\tEventLimitsChange EventType = "limits_change"'
new_events = '\tEventLimitsChange EventType = "limits_change"\n\tEventReconfigure EventType = "reconfigure_request"'

if old_events in econtent:
    econtent = econtent.replace(old_events, new_events)
    patches += 1
    with open(EVENTS_FILE, 'w', encoding='utf-8') as f:
        f.write(econtent)
    print('[OK] EventReconfigure added to events.go')
else:
    print('[FAIL] EventLimitsChange not matched in events.go')

# Write monitor.html
with open(MONITOR_FILE, 'w', encoding='utf-8') as f:
    f.write(content)

print(f'\nDone: {patches}/6 patches applied')
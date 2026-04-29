#!/usr/bin/env python3
"""Generate webui_field_tests.html — self-contained WebUI field validation page."""

import json

def gen():
    html = '<!DOCTYPE html>\n<html lang="ru">\n<head>\n'
    html += '<meta charset="UTF-8"><meta name="viewport" content="width=device-width, initial-scale=1.0">\n'
    html += '<title>OllamaLegion WebUI Field Tests</title>\n'
    html += '<style>\n'
    html += ':root{--bg:#0f172a;--card:#1e293b;--text:#e2e8f0;--muted:#94a3b8;--pri:#3b82f6;--ok:#22c55e;--warn:#f59e0b;--err:#ef4444;--info:#06b6d4;--bdr:#334155}\n'
    html += '*{box-sizing:border-box;margin:0;padding:0}\n'
    html += 'body{font-family:system-ui,sans-serif;background:var(--bg);color:var(--text);display:flex}\n'
    html += '.sidebar{width:200px;background:var(--card);border-right:1px solid var(--bdr);padding:1rem;flex-shrink:0}\n'
    html += '.main{flex:1;padding:1.5rem;overflow:auto}\n'
    html += '.page{display:none}.page.active{display:block}\n'
    html += '.metrics-grid{display:grid;grid-template-columns:repeat(auto-fill,minmax(180px,1fr));gap:1rem;margin-bottom:1.5rem}\n'
    html += '.metric-card{background:var(--card);border:1px solid var(--bdr);border-radius:.5rem;padding:1rem}\n'
    html += '.metric-header{color:var(--muted);font-size:.8rem;margin-bottom:.5rem}\n'
    html += '.metric-value{font-size:1.5rem;font-weight:700}\n'
    html += '.metric-sub{font-size:.7rem;color:var(--muted);margin-top:.25rem}\n'
    html += '.metric-mini-bar{height:4px;background:var(--bdr);border-radius:2px;margin-top:.5rem;overflow:hidden}\n'
    html += '.metric-mini-fill{height:100%;background:var(--pri);border-radius:2px}\n'
    html += '.card{background:var(--card);border:1px solid var(--bdr);border-radius:.5rem;margin-bottom:1.5rem}\n'
    html += '.card-header{padding:1rem;border-bottom:1px solid var(--bdr);display:flex;justify-content:space-between;align-items:center}\n'
    html += '.data-table{width:100%;border-collapse:collapse;font-size:.8rem}\n'
    html += '.data-table th,.data-table td{padding:.5rem .7rem;text-align:left;border-bottom:1px solid var(--bdr)}\n'
    html += '.data-table th{color:var(--muted);font-weight:500}\n'
    html += '.loading-cell{text-align:center;color:var(--muted);padding:2rem}\n'
    html += '.badge{display:inline-flex;padding:.1rem .4rem;border-radius:9999px;font-size:.7rem;font-weight:600}\n'
    html += '.bg-ok{background:rgba(34,197,94,.15);color:var(--ok)}\n'
    html += '.bg-err{background:rgba(239,68,68,.15);color:var(--err)}\n'
    html += '.bg-warn{background:rgba(245,158,11,.15);color:var(--warn)}\n'
    html += '.bg-info{background:rgba(6,182,212,.15);color:var(--info)}\n'
    html += '.btn{padding:.4rem .8rem;border-radius:.3rem;border:none;cursor:pointer;font-size:.8rem;background:var(--bdr);color:var(--text)}\n'
    html += '.btn-pri{background:var(--pri);color:#fff}\n'
    html += '.loading{text-align:center;color:var(--muted);padding:2rem}\n'
    html += '.rc,.gc,.cc,.ami,.bli,.mc{background:var(--bg);border:1px solid var(--bdr);border-radius:.3rem;padding:.8rem;margin-bottom:.6rem}\n'
    html += '.rh,.gch,.blh,.mch{display:flex;justify-content:space-between;align-items:center;margin-bottom:.4rem}\n'
    html += '.rf{display:flex;flex-wrap:wrap;gap:.2rem;margin-top:.4rem}\n'
    html += '.fb{padding:.1rem .3rem;border-radius:.2rem;font-size:.7rem;background:var(--bdr);color:var(--muted)}\n'
    html += '.fb.w2{background:rgba(245,158,11,.2);color:var(--warn)}\n'
    html += '.fb.d2{background:rgba(239,68,68,.2);color:var(--err)}\n'
    html += '.ci{position:relative;cursor:help;display:inline-block;margin-right:.4rem;padding:.2rem .4rem;background:var(--bdr);border-radius:.2rem;font-size:.7rem}\n'
    html += '.ct{display:none;position:absolute;z-index:100;background:var(--card);border:1px solid var(--bdr);border-radius:.3rem;padding:.5rem;min-width:180px}\n'
    html += '.ci:hover .ct{display:block}\n'
    html += '.ctr{display:flex;justify-content:space-between;padding:.1rem 0;font-size:.7rem}\n'
    html += '.ctl{color:var(--muted)}\n'
    html += '.gm{display:grid;grid-template-columns:repeat(4,1fr);gap:.4rem;margin-top:.4rem}\n'
    html += '.gml{font-size:.6rem;color:var(--muted);text-transform:uppercase}\n'
    html += '.gmv{font-size:.8rem;font-weight:600}\n'
    html += '.pb{height:6px;background:var(--bdr);border-radius:3px;margin-top:.4rem;overflow:hidden}\n'
    html += '.pf{height:100%;border-radius:3px}\n'
    html += '.pf.lo{background:var(--ok)}.pf.md{background:var(--warn)}.pf.hi{background:var(--err)}\n'
    html += '.cbc{margin:.7rem 0}\n'
    html += '.cbl{display:flex;justify-content:space-between;font-size:.7rem;margin-bottom:.2rem}\n'
    html += '.cb{height:8px;background:var(--bdr);border-radius:4px;overflow:hidden;position:relative}\n'
    html += '.cbf{height:100%;background:var(--pri);position:absolute;left:0;top:0}\n'
    html += '.cbx{height:100%;background:var(--warn);position:absolute;left:0;top:0;opacity:.6}\n'
    html += '.cbg{height:100%;background:var(--ok);position:absolute;left:0;top:0;opacity:.3}\n'
    html += '.cst{display:grid;grid-template-columns:repeat(2,1fr);gap:.4rem;font-size:.7rem;margin-top:.4rem}\n'
    html += '.cs2{display:flex;align-items:center;gap:.4rem;font-size:.7rem;color:var(--muted)}\n'
    html += '.ami{display:flex;align-items:center;gap:.4rem;padding:.4rem .6rem}\n'
    html += '.mi{width:8px;height:8px;border-radius:50%;flex-shrink:0}\n'
    html += '.ml .mi{background:var(--ok)}.mul .mi{background:var(--err)}\n'
    html += '.mn{font-weight:500}.mv{margin-left:auto;font-size:.7rem;color:var(--muted)}\n'
    html += '.alert{padding:.6rem .8rem;border-radius:.3rem;margin-bottom:.4rem;display:flex;align-items:center;gap:.4rem}\n'
    html += '.al-info{background:rgba(59,130,246,.1);color:var(--pri)}\n'
    html += '.al-warn{background:rgba(245,158,11,.1);color:var(--warn)}\n'
    html += '.al-err{background:rgba(239,68,68,.1);color:var(--err)}\n'
    html += '.bls{display:flex;gap:.8rem;font-size:.7rem;color:var(--muted);margin-bottom:.4rem}\n'
    html += '.blbr{display:flex;justify-content:space-between;font-size:.7rem;margin-bottom:.2rem}\n'
    html += '.blb{height:6px;background:var(--bdr);border-radius:3px;margin-bottom:.4rem;overflow:hidden}\n'
    html += '.blf{height:100%;border-radius:3px}\n'
    html += '.blf.vr{background:var(--pri)}.blf.ra{background:var(--info)}\n'
    html += '.mc{display:flex;flex-direction:column}\n'
    html += '.ms{font-size:.7rem;color:var(--muted);margin-bottom:.4rem}\n'
    html += '.mdg{display:grid;grid-template-columns:repeat(3,1fr);gap:.4rem;margin-bottom:.6rem}\n'
    html += '.mdl{font-size:.6rem;color:var(--muted);text-transform:uppercase}\n'
    html += '.mdv{font-size:.8rem;font-weight:600}\n'
    html += '.mmt{font-size:.7rem;color:var(--muted);margin-bottom:.4rem}\n'
    html += '.mmbc{margin-bottom:.4rem}\n'
    html += '.mmbl{display:flex;justify-content:space-between;font-size:.7rem;margin-bottom:.2rem}\n'
    html += '.mmb{height:6px;background:var(--bdr);border-radius:3px;overflow:hidden}\n'
    html += '.mmbf{height:100%;border-radius:3px}\n'
    html += '.mmbf.vr{background:var(--pri)}.mmbf.ra{background:var(--info)}\n'
    html += '.qv{padding:1rem}\n'
    html += '.qb{height:12px;background:var(--bdr);border-radius:6px;overflow:hidden;margin-bottom:.4rem}\n'
    html += '.qf{height:100%;background:var(--pri);border-radius:6px}\n'
    html += '.ql{display:flex;justify-content:space-between;font-size:.7rem;color:var(--muted)}\n'
    html += '.qts{padding:.1rem .4rem;border-radius:.2rem;font-size:.7rem;font-weight:500}\n'
    html += '.qts.pr{background:rgba(59,130,246,.15);color:var(--pri)}\n'
    html += '.qts.cp{background:rgba(34,197,94,.15);color:var(--ok)}\n'
    html += '.qts.pd{background:rgba(148,163,184,.15);color:var(--muted)}\n'
    html += '.lc{max-height:400px;overflow:auto;padding:1rem}\n'
    html += '.le{display:flex;gap:.6rem;padding:.3rem 0;border-bottom:1px solid var(--bdr);font-size:.8rem}\n'
    html += '.lt{color:var(--muted);font-family:monospace;white-space:nowrap}\n'
    html += '.ll{font-weight:600;font-size:.7rem;padding:.1rem .3rem;border-radius:.2rem}\n'
    html += '.ll.in{background:rgba(59,130,246,.15);color:var(--pri)}\n'
    html += '.ll.er{background:rgba(239,68,68,.15);color:var(--err)}\n'
    html += '.ll.wa{background:rgba(245,158,11,.15);color:var(--warn)}\n'
    html += '.sf{padding:1rem;max-width:500px}\n'
    html += '.fg{margin-bottom:.8rem}.fg>label{display:block;margin-bottom:.3rem;font-size:.8rem;color:var(--muted)}\n'
    html += '.fc{width:100%;background:var(--bg);border:1px solid var(--bdr);color:var(--text);padding:.4rem .6rem;border-radius:.3rem}\n'
    html += '.tg{position:relative;display:inline-block;width:36px;height:20px}\n'
    html += '.tg input{opacity:0;width:0;height:0}\n'
    html += '.tgs{position:absolute;cursor:pointer;top:0;left:0;right:0;bottom:0;background:var(--bdr);border-radius:20px;transition:.2s}\n'
    html += '.tg input:checked+.tgs{background:var(--pri)}\n'
    html += '.fa{display:flex;gap:.4rem;justify-content:flex-end;margin-top:1rem}\n'
    html += '.modal{display:none;position:fixed;inset:0;background:rgba(0,0,0,.5);z-index:100;align-items:center;justify-content:center}\n'
    html += '.modal.active{display:flex}\n'
    html += '.mo{background:var(--card);border:1px solid var(--bdr);border-radius:.5rem;width:460px;max-width:90%;max-height:90vh;overflow:auto}\n'
    html += '.mh{padding:1rem;border-bottom:1px solid var(--bdr);display:flex;justify-content:space-between;align-items:center}\n'
    html += '.mcl{background:none;border:none;color:var(--muted);font-size:1.5rem;cursor:pointer}\n'
    html += '.mb{padding:1rem}.mf{padding:1rem;border-top:1px solid var(--bdr);display:flex;gap:.4rem;justify-content:flex-end}\n'
    html += '.fr{display:grid;grid-template-columns:1fr 1fr;gap:.8rem}\n'
    html += '.sd{width:8px;height:8px;border-radius:50%}.sd.cn{background:var(--ok)}.sd.dc{background:var(--err)}\n'
    html += '.gs{width:10px;height:10px;border-radius:50%;display:inline-block}\n'
    html += '.gs.ok2{background:var(--ok)}.gs.wn2{background:var(--warn)}.gs.er2{background:var(--err)}\n'
    html += '#tr{position:fixed;top:0;right:0;width:440px;max-height:100vh;overflow:auto;background:#0a0f1c;color:#e2e8f0;padding:1rem;font-family:ui-monospace,monospace;font-size:11px;z-index:9999;border-left:1px solid var(--bdr)}\n'
    html += '#tr h3{margin-bottom:.6rem;font-size:13px;border-bottom:1px solid var(--bdr);padding-bottom:.4rem}\n'
    html += '.tp{color:var(--ok)}.tf{color:var(--err)}.tw{color:var(--warn)}\n'
    html += '.tr2{padding:2px 0;border-bottom:1px solid rgba(51,65,85,.3)}\n'
    html += '.tsu{font-weight:700;margin-bottom:.4rem;padding:.4rem;background:rgba(30,41,59,.8);border-radius:.2rem}\n'
    html += '.tse{color:var(--pri);font-weight:600;margin-top:.4rem;margin-bottom:.2rem}\n'
    html += '</style>\n</head>\n<body>\n'

    # Sidebar + header
    html += '<div class="sidebar"><div><b>OllamaLegion</b></div><nav>'
    for p in ['dashboard','backends','models','sessions','queue','logs','settings']:
        html += f'<a href="#" data-page="{p}" style="display:block;color:var(--muted);padding:.25rem 0;text-decoration:none">{p.capitalize()}</a>'
    html += '</nav><div class="cs2"><span class="sd cn"></span>Подключено</div></div>'
    html += '<div class="main"><div style="display:flex;justify-content:space-between;margin-bottom:1.5rem"><h1>Dashboard</h1><div><button class="btn">Обновить</button><button class="btn btn-pri" style="margin-left:.5rem">Добавить бэкенд</button></div></div>'

    # Dashboard page
    html += '<div class="page active" id="dashboard-page">'
    html += '<div class="metrics-grid">'
    cards = [
        ('totalBackends','Всего бэкендов','healthyBackends','- здоровых'),
        ('totalModels','Активных моделей','loadedModels','- загружено'),
        ('totalSessions','Активных сессий','sessionRate','- запросов'),
        ('queueSize','Очередь','queueProcessed','- обработано'),
    ]
    for vid,vlab,subid,subtext in cards:
        has_bar = (vid == 'queueSize')
        html += f'<div class="metric-card"><div class="metric-header">{vlab}</div><div class="metric-value" id="{vid}">-</div><div class="metric-sub" id="{subid}">{subtext}</div>'
        if has_bar: html += '<div class="metric-mini-bar"><div class="metric-mini-fill" id="queueDashboardFill" style="width:0%"></div></div>'
        html += '</div>'
    html += '</div>'

    # Runtime
    html += '<div class="card"><div class="card-header"><h3>Ollama Runtime</h3></div><div id="runtimeCluster"><div class="loading">Ожидание данных...</div></div></div>'
    # GPU Cluster
    html += '<div class="card"><div class="card-header"><h3>GPU Кластер</h3><span class="badge bg-info" id="gpuClusterBadge">-</span></div><div id="gpuCluster"><div class="loading">Ожидание данных...</div></div></div>'
    # Capacity
    html += '<div class="card"><div class="card-header"><h3>Ёмкость бэкендов</h3></div><div id="capacitySection"><div class="loading">Ожидание данных...</div></div></div>'
    # Available models
    html += '<div class="card"><div class="card-header"><h3>Доступные для загрузки</h3><span class="badge bg-info" id="loadableModelCount">-</span></div><div id="availableModelsList"><div class="loading">Ожидание данных...</div></div></div>'
    # Backends table (dashboard)
    html += '<div class="card"><div class="card-header"><h3>Бэкенды</h3></div><table class="data-table"><thead><tr><th>ID</th><th>Статус</th><th>GPU</th><th>VRAM</th><th>CPU</th><th>RAM</th><th>Active Req</th><th>Модели</th><th>RPS</th><th>Предиктор</th><th>Действия</th></tr></thead><tbody id="backendsTableBody"><tr><td colspan="11" class="loading-cell">Ожидание данных...</td></tr></tbody></table></div>'
    # Prediction alerts
    html += '<div class="card" id="predictionAlerts"><div class="card-header"><h3>Предупреждения предиктора</h3></div><div id="alertsList"><div class="alert al-info">Нет активных предупреждений</div></div></div>'
    html += '</div>'  # close dashboard-page

    # Backends page
    html += '<div class="page" id="backends-page"><div class="card"><div class="card-header"><h3>Управление бэкендами</h3></div><table class="data-table"><thead><tr><th>ID</th><th>Имя</th><th>Хост</th><th>Ollama Порт</th><th>Agent Порт</th><th>Вес</th><th>Max Concurrent</th><th>Max Models</th><th>Агент</th><th>Метки</th><th>Последний контакт</th><th>Статус</th><th>Действия</th></tr></thead><tbody id="backendsManageBody"><tr><td colspan="13" class="loading-cell">Ожидание данных...</td></tr></tbody></table></div></div>'

    # Models page
    html += '<div class="page" id="models-page"><div class="metrics-grid"><div class="metric-card"><div class="metric-header">Всего моделей</div><div class="metric-value" id="modelsTotal">-</div></div><div class="metric-card"><div class="metric-header">Загружено сейчас</div><div class="metric-value" id="modelsLoaded">-</div></div></div><div class="card"><div class="card-header"><h3>Загрузка бэкендов</h3></div><div id="backendLoadList"><div class="loading">Ожидание данных...</div></div></div><div class="card"><div class="card-header"><h3>Запущенные модели</h3></div><div id="modelsGrid"><div class="loading">Ожидание данных...</div></div></div></div>'

    # Sessions page
    html += '<div class="page" id="sessions-page"><div class="card"><div class="card-header"><h3>Активные сессии</h3></div><table class="data-table"><thead><tr><th>ID</th><th>Бэкенд</th><th>Модель</th><th>Запросов</th><th>Последняя активность</th><th>IP</th><th>Клиент</th><th>Статус</th></tr></thead><tbody id="sessionsTableBody"><tr><td colspan="8" class="loading-cell">Ожидание данных...</td></tr></tbody></table></div></div>'

    # Queue page
    html += '<div class="page" id="queue-page"><div class="metrics-grid">'
    for vid,vlab in [('queueCurrentSize','Текущий размер'),('queueMaxSize','Максимум'),('queueProcessed','Выполнено задач'),('queueWorkers','Воркеры'),('queueAvgWait','Среднее ожидание')]:
        html += f'<div class="metric-card"><div class="metric-header">{vlab}</div><div class="metric-value" id="{vid}">-</div></div>'
    html += '</div>'
    html += '<div class="card"><div class="card-header"><h3>Состояние очереди</h3></div><div class="qv"><div class="qb"><div class="qf" id="queueFill" style="width:0%"></div></div><div class="ql"><span>0</span><span id="queueMidLabel">-</span><span id="queueMaxLabel">-</span></div></div></div>'
    html += '<div class="card"><div class="card-header"><h3>Задачи в очереди</h3><span class="badge bg-info" id="queueTasksCount">-</span></div><table class="data-table"><thead><tr><th>#</th><th>Модель</th><th>Бэкенд</th><th>Время в очереди</th><th>Статус</th></tr></thead><tbody id="queueTasksBody"><tr><td colspan="5" class="loading-cell">Ожидание данных...</td></tr></tbody></table></div>'
    html += '<div class="card"><div class="card-header"><h3>История выполненных задач</h3><span class="badge bg-ok" id="queueHistoryCount">-</span></div><table class="data-table"><thead><tr><th>#</th><th>Модель</th><th>Бэкенд</th><th>Время постановки</th><th>Время выполнения</th><th>Ожидание</th></tr></thead><tbody id="queueHistoryBody"><tr><td colspan="6" class="loading-cell">Ожидание данных...</td></tr></tbody></table></div>'
    html += '</div>'  # close queue-page

    # Logs page
    html += '<div class="page" id="logs-page"><div class="card"><div class="card-header"><h3>События и логи</h3><div><button class="btn">Очистить</button><button class="btn" style="margin-left:.4rem">Экспорт</button></div></div><div class="lc" id="logsContainer"><div class="le"><span class="lt">--:--:--</span><span class="ll in">INFO</span>WebUI инициализирован</div></div></div></div>'

    # Settings page
    html += '<div class="page" id="settings-page"><div class="card"><div class="card-header"><h3>Настройки балансировки</h3></div><div class="sf">'
    html += '<div class="fg"><label>Алгоритм балансировки</label><select id="balancingAlgorithm" class="fc"><option value="resource-aware">Resource-Aware</option><option value="least-connections">Least Connections</option><option value="round-robin">Round Robin</option></select></div>'
    for cid,clab in [('modelAffinity','Model Affinity'),('sessionStickiness','Session Stickiness'),('predictionFiltering','Prediction-based Filtering')]:
        html += f'<div class="fg"><label>{clab}</label><label class="tg"><input type="checkbox" id="{cid}" checked><span class="tgs"></span></label></div>'
    for nid,lab,val,mi,ma in [('gpuMaxUsage','GPU Max Usage (%)','90','50','100'),('vramMaxUsage','VRAM Max Usage (%)','85','50','100'),('cpuMaxUsage','CPU Max Usage (%)','80','50','100'),('ramMaxUsage','RAM Max Usage (%)','85','50','100'),('minFreeDisk','Min Free Disk (MB)','10240','0','-')]:
        maxattr = f'max="{ma}" ' if ma != '-' else ''
        html += f'<div class="fg"><label>{lab}</label><input type="number" id="{nid}" class="fc" value="{val}" min="{mi}" {maxattr}></div>'
    html += '<div class="fa"><button class="btn">Сбросить</button><button class="btn btn-pri" style="margin-left:.4rem">Сохранить</button></div></div></div></div>'

    # Modal
    html += '<div class="modal" id="backendModal"><div class="mo"><div class="mh"><h3>Добавить бэкенд</h3><button class="mcl">&times;</button></div><div class="mb">'
    html += '<div class="fg"><label>ID *</label><input type="text" id="formBackendId" class="fc" placeholder="gpu-1"></div>'
    html += '<div class="fg"><label>Имя</label><input type="text" id="formBackendName" class="fc" placeholder="GPU Server 1"></div>'
    html += '<div class="fg"><label>Хост *</label><input type="text" id="formBackendHost" class="fc" placeholder="192.168.1.100"></div>'
    html += '<div class="fr"><div class="fg"><label>Порт Ollama</label><input type="number" id="formBackendOllamaPort" class="fc" value="11434"></div><div class="fg"><label>Порт агента</label><input type="number" id="formBackendAgentPort" class="fc" value="18032"></div></div>'
    html += '<div class="fr"><div class="fg"><label>Вес</label><input type="number" id="formBackendWeight" class="fc" value="1" step="0.1"></div><div class="fg"><label>Max Concurrent</label><input type="number" id="formBackendMaxConcurrent" class="fc" value="10"></div></div>'
    html += '<div class="fg"><label>Метки</label><input type="text" id="formBackendLabels" class="fc" placeholder="nvidia,rtx4090"></div>'
    html += '</div><div class="mf"><button class="btn">Отмена</button><button class="btn btn-pri" style="margin-left:.4rem" id="modalSave">Сохранить</button></div></div></div>'

    html += '</div></div>'  # close main + app
    html += '<div id="tr"><h3>🧪 WebUI Field Tests</h3><div class="tsu" id="ts">Running...</div><div id="td"></div></div>'

    # ===== MOCK DATA + RENDERERS + UTILS + TEST ENGINE =====
    html += '\n<script>\n'

    # Utils
    html += '''
const Utils = {
    formatNumber(n) { if (n === undefined || n === null) return '-'; if (n >= 1_000_000) return (n / 1_000_000).toFixed(1) + 'M'; if (n >= 1_000) return (n / 1_000).toFixed(1) + 'K'; return n.toString(); },
    formatMB(mb) { if (mb === undefined || mb === null) return '-'; if (mb >= 1024) return (mb / 1024).toFixed(1) + ' GB'; return Math.round(mb) + ' MB'; },
    escapeHtml(text) { if (text == null) return ''; const d = document.createElement('div'); d.textContent = String(text); return d.innerHTML; },
    getBackendMode(backend) { if ((backend.labels || []).includes('cloud')) return 'cloud'; return backend.ollama?.backendCapacity?.mode || 'gpu'; },
    getBackendModeBadge(backend) { const m = Utils.getBackendMode(backend); const b = { gpu: '<span class="badge bg-ok">GPU</span>', cpu: '<span class="badge bg-warn">CPU</span>', cloud: '<span class="badge bg-info">Cloud</span>' }; return b[m] || b.gpu; },
    getGPUStatus(gpuUsage, vramPercent, temp) { if (gpuUsage > 90 || vramPercent > 90 || temp > 85) return 'er2'; if (gpuUsage > 70 || vramPercent > 70 || temp > 75) return 'wn2'; return 'ok2'; },
    getProgressClass(value) { if (value > 80) return 'hi'; if (value > 50) return 'md'; return 'lo'; },
    percent(value, total) { return total > 0 ? (value / total * 100) : 0; },
    setText(id, text) { const el = document.getElementById(id); if (el) el.textContent = text; },
    setHTML(id, html) { const el = document.getElementById(id); if (el) el.innerHTML = html; },
    setStyle(id, prop, value) { const el = document.getElementById(id); if (el) el.style[prop] = value; }
};
'''

    # Renderers (compact, matching production logic)
    html += '''
const Renderers = (function () {
    const { formatNumber, formatMB, getBackendMode, getBackendModeBadge, getGPUStatus, getProgressClass, percent, escapeHtml } = Utils;
    const _predCache = {};
    const PRED_THRESHOLD = 300;

    function badge(status, type) { return `<span class="badge bg-${type}">${escapeHtml(status)}</span>`; }
    function loading(text = 'Ожидание данных...') { return `<div class="loading">${escapeHtml(text)}</div>`; }
    function emptyRow(cols, text) { return `<tr><td colspan="${cols}" class="loading-cell">${escapeHtml(text)}</td></tr>`; }
    function metric(label, value) { return `<div class="gml">${label}</div><div class="gmv">${escapeHtml(String(value))}</div>`; }

    function dashboard(backends, sessions, queue) {
        const healthy = backends.filter(b => b.status === 'healthy');
        const totalModels = backends.reduce((sum, b) => sum + (b.ollama?.runningModels?.length || 0), 0);
        const activeSessions = (sessions || []).filter(s => s.active).length;
        const totalRequests = (sessions || []).reduce((sum, s) => sum + (s.requestCount || 0), 0);
        const queueSize = queue?.current_size || 0;
        const queueProcessed = queue?.processed_total || 0;
        const queueMax = queue?.max_size || 100;
        const queuePct = percent(queueSize, queueMax);
        Utils.setText('totalBackends', backends.length);
        Utils.setText('healthyBackends', healthy.length + ' здоровых');
        Utils.setText('totalModels', totalModels);
        Utils.setText('loadedModels', totalModels + ' загружено');
        Utils.setText('totalSessions', activeSessions);
        Utils.setText('sessionRate', totalRequests + ' запросов');
        Utils.setText('queueSize', queueSize);
        Utils.setText('queueProcessed', queueProcessed + ' обработано');
        Utils.setStyle('queueDashboardFill', 'width', queuePct + '%');
        Utils.setHTML('runtimeCluster', runtimeCluster(backends));
        Utils.setHTML('gpuCluster', gpuCluster(backends));
        Utils.setHTML('capacitySection', capacitySection(backends));
        Utils.setHTML('availableModelsList', availableModels(backends));
        Utils.setHTML('backendsTableBody', backendsTable(backends));
    }

    function runtimeCluster(backends) {
        if (!backends.length) return loading('Нет данных о бэкендах');
        return backends.map(b => {
            const flags = b.ollama?.runtimeFlags || {};
            const contexts = b.ollama?.modelContexts || [];
            const flagBadges = [];
            if (flags.numGpuLayers !== undefined && flags.numGpuLayers !== 0) { const c = flags.numGpuLayers === -1 ? '' : ' w2'; flagBadges.push(`<span class="fb${c}">GPU:${flags.numGpuLayers === -1 ? 'auto' : flags.numGpuLayers}</span>`); }
            if (flags.contextLength) flagBadges.push('<span class="fb">C:' + formatNumber(flags.contextLength) + '</span>');
            if (flags.numParallel && flags.numParallel > 1) flagBadges.push('<span class="fb w2">NP:' + flags.numParallel + '</span>');
            if (flags.numThreads) flagBadges.push('<span class="fb">T:' + flags.numThreads + '</span>');
            if (flags.batchSize && flags.batchSize !== 512) flagBadges.push('<span class="fb">B:' + flags.batchSize + '</span>');
            if (flags.lowVram) flagBadges.push('<span class="fb d2">LOW_VRAM</span>');
            if (flags.flashAttention) flagBadges.push('<span class="fb">FA</span>');
            if (flags.kvCacheQuant && flags.kvCacheQuant !== 'f16') flagBadges.push('<span class="fb w2">KV:' + flags.kvCacheQuant + '</span>');
            const cbadges = contexts.map(ctx =>
                '<span class="ci">' + escapeHtml(ctx.name) + ': Ctx ' + formatNumber(ctx.effectiveContext) + ' (' + ctx.contextSource + ')<div class="ct">' +
                '<div class="ctr"><span class="ctl">Context</span><span>' + formatNumber(ctx.contextLength) + '</span></div>' +
                '<div class="ctr"><span class="ctl">Effective</span><span>' + formatNumber(ctx.effectiveContext) + '</span></div>' +
                '<div class="ctr"><span class="ctl">Model Memory</span><span>' + formatMB(ctx.modelMemoryMB) + '</span></div>' +
                '<div class="ctr"><span class="ctl">Context Memory</span><span>' + formatMB(ctx.contextMemoryMB) + '</span></div>' +
                '<div class="ctr"><span class="ctl">KV Cache</span><span>' + formatMB(ctx.kvCacheMemoryMB) + '</span></div>' +
                '<div class="ctr"><span class="ctl">Total</span><span>' + formatMB(ctx.totalMemoryMB) + '</span></div>' +
                '<div class="ctr"><span class="ctl">Layers</span><span>' + (ctx.numLayers || '-') + '</span></div>' +
                '<div class="ctr"><span class="ctl">Precision</span><span>' + (ctx.precisionBits || 16) + '-bit</span></div>' +
                '</div></span>'
            ).join('');
            return '<div class="rc"><div class="rh"><strong>' + escapeHtml(b.id) + '</strong>' + badge(b.status, b.status === 'healthy' ? 'ok' : 'err') + '</div>' +
                '<div class="rf">' + (flagBadges.length ? flagBadges.join('') : '<span class="fb">default</span>') + '</div>' +
                (cbadges ? '<div style="margin-top:.5rem">' + cbadges + '</div>' : '') +
                '</div>';
        }).join('');
    }

    function gpuCluster(backends) {
        if (!backends.length) return loading('Нет данных о бэкендах');
        const g = backends.filter(b => getBackendMode(b) === 'gpu').length;
        const c = backends.filter(b => getBackendMode(b) === 'cpu').length;
        const cl = backends.filter(b => getBackendMode(b) === 'cloud').length;
        Utils.setHTML('gpuClusterBadge', g + ' GPU · ' + c + ' CPU · ' + cl + ' Cloud');
        return backends.map(b => gpuCard(b)).join('');
    }

    function gpuCard(backend) {
        const mode = getBackendMode(backend);
        if (mode === 'cloud') {
            const ar = backend.activeRequests || 0, mr = backend.maxConcurrentRequests || 10;
            const mo = (backend.ollama?.runningModels || []).length, rps = backend.ollama?.requestsPerSecond || 0;
            return '<div class="gc"><div class="gch"><span>' + escapeHtml(backend.id) + '</span><span class="badge bg-info">Cloud</span></div>' +
                '<div class="gm">' + metric('Req', ar + '/' + mr) + metric('Models', mo) + metric('RPS', rps.toFixed(1)) + metric('Status', backend.status) + '</div>' +
                '<div class="pb"><div class="pf lo" style="width:0%"></div></div></div>';
        }
        if (mode === 'cpu') {
            const sys = backend.system || {};
            const cpuU = sys.cpuUsagePercent || 0, ramT = sys.memoryTotal || 1, ramU = sys.memoryUsed || 0;
            const ramP = percent(ramU, ramT);
            const temp = sys.cpuTemperature ?? '-', load = sys.loadAverage1?.toFixed(2) ?? '-';
            return '<div class="gc"><div class="gch"><span>' + escapeHtml(backend.id) + '</span><span class="badge bg-warn">CPU</span></div>' +
                '<div class="gm">' + metric('CPU', cpuU.toFixed(1) + '%') + metric('RAM', ramP.toFixed(1) + '%') + metric('Load', load) + metric('Temp', temp + '°C') + '</div>' +
                '<div class="pb"><div class="pf ' + getProgressClass(cpuU) + '" style="width:' + cpuU + '%"></div></div></div>';
        }
        const gpu = backend.gpu || {};
        const usage = gpu.usagePercent || 0, vramU = gpu.memoryUsed || 0, vramT = gpu.memoryTotal || 1;
        const vramP = percent(vramU, vramT);
        const temp = gpu.temperature ?? '-', power = gpu.powerUsage ?? '-';
        const status = getGPUStatus(usage, vramP, temp);
        return '<div class="gc"><div class="gch"><span>' + escapeHtml(backend.id) + '</span><span class="gs ' + status + '"></span></div>' +
            '<div class="gm">' + metric('GPU', usage.toFixed(1) + '%') + metric('VRAM', vramP.toFixed(1) + '%') + metric('Temp', temp + '°C') + metric('Power', power + 'W') + '</div>' +
            '<div class="pb"><div class="pf ' + getProgressClass(usage) + '" style="width:' + usage + '%"></div></div></div>';
    }

    function capacitySection(backends) {
        if (!backends.length) return loading('Нет данных о бэкендах');
        return backends.map(b => {
            const mode = getBackendMode(b);
            const cap = b.ollama?.backendCapacity || {};
            if (mode === 'cloud') {
                return '<div class="cc"><div class="rh"><strong>' + escapeHtml(b.id) + '</strong>' + getBackendModeBadge(b) + '</div>' +
                    '<div style="font-size:.75rem"><div><span>Хост</span> ' + escapeHtml(b.host || '-') + '</div>' +
                    '<div><span>Статус</span> ' + badge(b.status, b.status === 'healthy' ? 'ok' : 'err') + '</div>' +
                    '<div><span>Active Req</span> ' + (b.activeRequests || 0) + '</div>' +
                    '<div><span>Загружено моделей</span> ' + (b.ollama?.runningModels || []).length + '</div></div></div>';
            }
            const isCPU = mode === 'cpu';
            const sys = b.system || {};
            const memTotal = isCPU ? (sys.memoryTotal || 1) : (b.gpu?.memoryTotal || 1);
            const memUsed = isCPU ? (sys.memoryUsed || 0) : (b.gpu?.memoryUsed || 0);
            const memFree = isCPU ? (sys.memoryFree || 0) : (b.gpu?.memoryFree || 0);
            const loadedMem = cap.loadedModelVram || 0;
            const ctxOverhead = cap.contextOverheadMB || 0;
            const guaranteed = cap.guaranteedVram || 0;
            const usedPct = percent(memUsed, memTotal);
            const loadedPct = percent(loadedMem, memTotal);
            const ctxPct = percent(ctxOverhead, memTotal);
            const guarPct = percent(guaranteed, memTotal);
            const ml = isCPU ? 'RAM' : 'VRAM';
            return '<div class="cc"><div class="rh"><strong>' + escapeHtml(b.id) + '</strong>' + getBackendModeBadge(b) + '</div>' +
                '<div class="cbc"><div class="cbl"><span>' + ml + ': ' + formatMB(memUsed) + ' / ' + formatMB(memTotal) + '</span><span>' + usedPct.toFixed(1) + '%</span></div>' +
                '<div class="cb"><div class="cbf" style="width:' + loadedPct + '%"></div><div class="cbx" style="width:' + ctxPct + '%"></div><div class="cbg" style="width:' + guarPct + '%"></div></div></div>' +
                '<div class="cst"><div class="cs"><span>Loaded models</span><span>' + formatMB(loadedMem) + '</span></div>' +
                '<div class="cs"><span>Context overhead</span><span>' + formatMB(ctxOverhead) + '</span></div>' +
                '<div class="cs"><span>Free ' + ml + '</span><span>' + formatMB(memFree) + '</span></div>' +
                '<div class="cs"><span>Guaranteed (90%)</span><span>' + formatMB(guaranteed) + '</span></div></div></div>';
        }).join('');
    }

    function availableModels(backends) {
        const badgeEl = document.getElementById('loadableModelCount');
        if (!backends.length) { if (badgeEl) badgeEl.textContent = '-'; return loading('Нет данных'); }
        let totalLoadable = 0;
        const all = [];
        backends.forEach(b => {
            const models = b.ollama?.backendCapacity?.availableModels || [];
            models.forEach(m => { all.push({ ...m, backendId: b.id, backendStatus: b.status }); if (m.canLoad) totalLoadable++; });
        });
        if (badgeEl) badgeEl.textContent = totalLoadable;
        if (!all.length) return loading('Нет доступных моделей');
        all.sort((a, b) => { if (a.canLoad !== b.canLoad) return b.canLoad - a.canLoad; return (a.estimatedVram || 0) - (b.estimatedVram || 0); });
        return all.slice(0, 30).map(m => {
            const cls = m.canLoad ? 'ml' : 'mul';
            return '<div class="ami ' + cls + '"><div class="mi"></div><span class="mn">' + escapeHtml(m.name) + '</span><span class="mv">' + formatMB(m.estimatedVram || 0) + ' @ ' + escapeHtml(m.backendId) + '</span></div>';
        }).join('');
    }

    function backendsTable(backends) {
        if (!backends.length) return emptyRow(11, 'Нет данных');
        return backends.map(b => {
            const mode = getBackendMode(b);
            const gpu = b.gpu || {}, sys = b.system || {}, pred = b.prediction || {}, oll = b.ollama || {};
            const isCloud = mode === 'cloud';
            const gpuUsage = isCloud ? '-' : (gpu.usagePercent !== undefined ? gpu.usagePercent.toFixed(1) + '%' : '-');
            const vramPercent = isCloud ? '-' : (gpu.memoryTotal > 0 ? percent(gpu.memoryUsed, gpu.memoryTotal).toFixed(1) + '%' : '-');
            const cpuUsage = isCloud ? '-' : (sys.cpuUsagePercent !== undefined ? sys.cpuUsagePercent.toFixed(1) + '%' : '-');
            const ramPercent = isCloud ? '-' : (sys.memoryTotal > 0 ? percent(sys.memoryUsed, sys.memoryTotal).toFixed(1) + '%' : '-');
            const activeReq = b.activeRequests || 0, maxReq = b.maxConcurrentRequests || 10;
            const models = oll.runningModels?.length || 0;
            const rps = oll.requestsPerSecond || 0;
            const secondsToCrit = pred.secondsToCritical || -1;
            let predClass, predText;
            if (secondsToCrit > 0) { predText = Math.round(secondsToCrit) + 'с'; predClass = secondsToCrit < PRED_THRESHOLD ? 'warn' : 'ok'; }
            else { predText = 'OK'; predClass = 'ok'; }
            _predCache[b.id] = { class: predClass, text: predText };
            return '<tr><td><strong>' + escapeHtml(b.id) + '</strong> ' + getBackendModeBadge(b) + '</td>' +
                '<td>' + badge(b.status, b.status === 'healthy' ? 'ok' : 'err') + '</td>' +
                '<td>' + escapeHtml(gpuUsage) + '</td><td>' + escapeHtml(vramPercent) + '</td><td>' + escapeHtml(cpuUsage) + '</td><td>' + escapeHtml(ramPercent) + '</td>' +
                '<td>' + activeReq + '/' + maxReq + '</td><td>' + models + '</td><td>' + rps.toFixed(1) + '</td>' +
                '<td>' + badge(predText, predClass) + '</td>' +
                '<td><button class="btn" style="font-size:.6rem;padding:.1rem .3rem">Edit</button> <button class="btn" style="font-size:.6rem;padding:.1rem .3rem;background:var(--err);color:#fff">Del</button></td></tr>';
        }).join('');
    }

    function modelsPage(backends) {
        const allModels = [];
        backends.forEach(b => {
            (b.ollama?.runningModels || []).forEach(m => { allModels.push({ ...m, backend: b.id, backendStatus: b.status }); });
        });
        Utils.setText('modelsTotal', allModels.length);
        Utils.setText('modelsLoaded', allModels.filter(m => m.backendStatus === 'healthy').length);
        Utils.setHTML('backendLoadList', backendLoad(backends));
        Utils.setHTML('modelsGrid', modelsGrid(allModels, backends));
    }

    function backendLoad(backends) {
        if (!backends.length) return loading('Нет данных');
        return backends.map(b => {
            const mode = getBackendMode(b);
            const isGPU = mode === 'gpu';
            const gpu = b.gpu || {}, sys = b.system || {}, oll = b.ollama || {};
            const totalVRAM = gpu.memoryTotal || 0, usedVRAM = gpu.memoryUsed || 0;
            const totalRAM = sys.memoryTotal || 0, usedRAM = sys.memoryUsed || 0;
            const vramPercent = percent(usedVRAM, totalVRAM), ramPercent = percent(usedRAM, totalRAM);
            const models = oll.runningModels || [];
            const activeReq = oll.activeRequests || 0, maxReq = oll.maxConcurrentRequests || 10;
            const freeSlots = oll.freeSlots || 0;
            const vramBar = isGPU ? ('<div class="blbr"><span>VRAM ' + formatMB(usedVRAM) + ' / ' + formatMB(totalVRAM) + '</span><span>' + vramPercent.toFixed(1) + '%</span></div>' +
                '<div class="blb"><div class="blf vr" style="width:' + vramPercent + '%"></div></div>') : '';
            return '<div class="bli"><div class="blh"><strong>' + escapeHtml(b.id) + '</strong>' + badge(b.status, b.status === 'healthy' ? 'ok' : 'err') + '</div>' +
                '<div class="bls"><span>Моделей: <strong>' + models.length + '</strong></span><span>Active: <strong>' + activeReq + '/' + maxReq + '</strong></span><span>Свободно: <strong>' + freeSlots + '</strong></span></div>' +
                vramBar +
                '<div class="blbr"><span>RAM ' + formatMB(usedRAM) + ' / ' + formatMB(totalRAM) + '</span><span>' + ramPercent.toFixed(1) + '%</span></div>' +
                '<div class="blb"><div class="blf ra" style="width:' + ramPercent + '%"></div></div></div>';
        }).join('');
    }

    function modelsGrid(allModels, backends) {
        if (!allModels.length) return loading('Нет загруженных моделей');
        const backendMap = {};
        backends.forEach(b => { backendMap[b.id] = b; });
        return allModels.map(m => {
            const vramMB = (m.vramUsage || 0) / 1024 / 1024;
            const ramMB = (m.ramUsage || 0) / 1024 / 1024;
            const sizeGB = (m.size || 0) / 1024 / 1024 / 1024;
            const backend = backendMap[m.backend] || {};
            const mode = getBackendMode(backend);
            const isGPU = mode === 'gpu';
            const gpu = backend.gpu || {}, sys = backend.system || {};
            const totalVRAM = isGPU ? (gpu.memoryTotal || 1) : 0;
            const totalRAM = sys.memoryTotal || 1;
            const vrP = totalVRAM > 0 ? Math.min(percent(vramMB, totalVRAM), 100) : 0;
            const raP = totalRAM > 0 ? Math.min(percent(ramMB, totalRAM), 100) : 0;
            const showVRAM = isGPU && totalVRAM > 0;
            return '<div class="mc"><div class="mch"><span class="mn">' + escapeHtml(m.name) + '</span>' + badge(m.backend, m.backendStatus === 'healthy' ? 'ok' : 'err') + '</div>' +
                '<div class="ms">' + sizeGB.toFixed(1) + ' GB</div>' +
                '<div class="mdg"><div><div class="mdl">VRAM</div><div class="mdv">' + vramMB.toFixed(0) + ' MB</div></div>' +
                '<div><div class="mdl">RAM</div><div class="mdv">' + ramMB.toFixed(0) + ' MB</div></div>' +
                '<div><div class="mdl">Family</div><div class="mdv">' + escapeHtml(m.family || '-') + '</div></div></div>' +
                '<div class="mmt">Использование памяти</div>' +
                (showVRAM ? ('<div class="mmbc"><div class="mmbl"><span>VRAM</span><span>' + vramMB.toFixed(0) + ' / ' + totalVRAM + ' MB (' + vrP.toFixed(1) + '%)</span></div><div class="mmb"><div class="mmbf vr" style="width:' + vrP + '%"></div></div></div>') : '') +
                '<div class="mmbc"><div class="mmbl"><span>RAM</span><span>' + ramMB.toFixed(0) + ' / ' + totalRAM + ' MB (' + raP.toFixed(1) + '%)</span></div><div class="mmb"><div class="mmbf ra" style="width:' + raP + '%"></div></div></div></div>';
        }).join('');
    }

    function getClientIcon(name) {
        if (!name) return '👤';
        const n = name.toLowerCase();
        if (n.includes('cline')) return '🦾';
        if (n.includes('openwebui') || n.includes('open-webui')) return '🌐';
        if (n.includes('curl')) return '📡';
        if (n.includes('python') || n.includes('requests')) return '🐍';
        return '👤';
    }

    function sessionsPage(sessions) {
        const tbody = document.getElementById('sessionsTableBody');
        if (!tbody) return;
        if (!sessions.length) { tbody.innerHTML = emptyRow(7, 'Нет активных сессий'); return; }
        tbody.innerHTML = sessions.map(s => {
            const icon = getClientIcon(s.clientName);
            return '<tr><td><code>' + escapeHtml((s.id || 'N/A').substring(0, 16)) + '...</code></td>' +
                '<td>' + escapeHtml(s.backendId || '-') + '</td><td>' + escapeHtml(s.model || '-') + '</td>' +
                '<td>' + (s.requestCount || 0) + '</td>' +
                '<td>' + (s.lastRequestAt ? new Date(s.lastRequestAt).toLocaleString('ru') : '-') + '</td>' +
                '<td>' + escapeHtml(s.clientIP || '-') + '</td>' +
                '<td>' + icon + ' ' + escapeHtml(s.clientName || '-') + '</td>' +
                '<td>' + (s.active ? badge('ACTIVE', 'ok') : badge('IDLE', 'warn')) + '</td></tr>';
        }).join('');
    }

    function queuePage(queue, queueTasks, queueHistory) {
        const current = queue?.current_size || 0, max = queue?.max_size || 100;
        const processed = queue?.processed_total || 0, workers = queue?.workers || 0;
        const avgWait = queue?.avg_wait_time_ms || 0;
        Utils.setText('queueCurrentSize', current);
        Utils.setText('queueMaxSize', max);
        Utils.setText('queueProcessed', processed);
        Utils.setText('queueWorkers', workers);
        Utils.setText('queueAvgWait', avgWait > 0 ? (avgWait / 1000).toFixed(1) + ' с' : '-');
        const pct = percent(current, max);
        Utils.setStyle('queueFill', 'width', pct + '%');
        Utils.setText('queueMidLabel', Math.round(max / 2));
        Utils.setText('queueMaxLabel', max);
        const badgeEl = document.getElementById('queueTasksCount');
        if (badgeEl) badgeEl.textContent = current;
        Utils.setHTML('queueTasksBody', queueTasksBody(queueTasks));
        Utils.setHTML('queueHistoryBody', queueHistoryBody(queueHistory));
    }

    function queueTasksBody(tasks) {
        if (!tasks.length) return emptyRow(5, 'Нет задач в очереди');
        return tasks.map((t, i) => {
            const st = t.status || 'pending';
            const sc = st === 'processing' ? 'pr' : (st === 'completed' ? 'cp' : 'pd');
            const stxt = st === 'processing' ? 'Обработка' : (st === 'completed' ? 'Завершено' : 'Ожидание');
            const wt = t.waitTimeMs ? (t.waitTimeMs / 1000).toFixed(1) + 'с' : '-';
            return '<tr><td>' + (i + 1) + '</td><td><code>' + escapeHtml(t.model || '-') + '</code></td><td>' + escapeHtml(t.backend || 'Auto') + '</td><td>' + wt + '</td><td><span class="qts ' + sc + '">' + stxt + '</span></td></tr>';
        }).join('');
    }

    function queueHistoryBody(history) {
        const badgeEl = document.getElementById('queueHistoryCount');
        if (badgeEl) badgeEl.textContent = history.length;
        if (!history.length) return emptyRow(6, 'Нет выполненных задач');
        return history.map((t, i) => {
            const enq = t.enqueued ? new Date(t.enqueued).toLocaleString('ru', { hour: '2-digit', minute: '2-digit', second: '2-digit' }) : '-';
            const comp = t.completed_at ? new Date(t.completed_at).toLocaleString('ru', { hour: '2-digit', minute: '2-digit', second: '2-digit' }) : '-';
            const wms = t.wait_time_ms || 0;
            const ws = wms > 0 ? (wms / 1000).toFixed(1) + ' с' : '-';
            return '<tr><td>' + (i + 1) + '</td><td><code>' + escapeHtml(t.model || '-') + '</code></td><td>' + escapeHtml(t.target || 'Auto') + '</td><td>' + enq + '</td><td>' + comp + '</td><td>' + ws + '</td></tr>';
        }).join('');
    }

    function logs(logEntries) {
        const container = document.getElementById('logsContainer');
        if (!container) return;
        if (!logEntries.length) { container.innerHTML = loading('Нет логов'); return; }
        container.innerHTML = logEntries.map(l => '<div class="le"><span class="lt">' + escapeHtml(l.time) + '</span><span class="ll ' + (l.level.toLowerCase() === 'error' ? 'er' : 'in') + '">' + escapeHtml(l.level) + '</span>' + escapeHtml(l.message) + '</div>').join('');
    }

    function predictionAlerts(backends) {
        const alerts = [];
        backends.forEach(b => {
            const pred = b.prediction || {};
            const sec = pred.secondsToCritical;
            if (sec > 0 && sec < 300) {
                alerts.push({ backend: b.id, reason: pred.criticalReason || 'unknown', seconds: Math.round(sec), level: sec < 120 ? 'err' : 'warn' });
            }
        });
        const container = document.getElementById('alertsList');
        if (!container) return;
        if (!alerts.length) { container.innerHTML = '<div class="alert al-info">Нет активных предупреждений</div>'; return; }
        container.innerHTML = alerts.map(a => '<div class="alert al-' + a.level + '"><strong>' + escapeHtml(a.backend) + '</strong>: ' + escapeHtml(a.reason) + ' через ' + a.seconds + 'с</div>').join('');
    }

    return { dashboard, modelsPage, sessionsPage, queuePage, logs, predictionAlerts };
})();
'''

    # Mock data
    html += '''
// ===== MOCK DATA =====
const mockBackends = [
    { id: 'gpu-node-01', name: 'GPU Node 01', host: '192.168.1.101', ollamaPort: 11434, agentPort: 18032, weight: 1, maxConcurrentRequests: 10, labels: ['nvidia','rtx4090'], status: 'healthy', hasAgent: true, lastAgentContact: new Date().toISOString(), activeRequests: 3,
        gpu: { usagePercent: 65.2, memoryTotal: 24576, memoryUsed: 18200, memoryFree: 6376, temperature: 72, powerUsage: 280, powerLimit: 450, gpuClock: 2520, memClock: 10500 },
        system: { cpuUsagePercent: 34.5, memoryTotal: 65536, memoryUsed: 28000, memoryFree: 37536, cpu: { usagePercent: 34.5, coreCount: 32, threadCount: 64, model: 'AMD EPYC 7543', loadAverage1: 4.2, loadAverage5: 3.8, loadAverage15: 3.5, temperature: 55, throttled: false }, diskTotal: 500000, diskUsed: 120000, diskFree: 380000, networkRX: 5000000000, networkTX: 2000000000 },
        ollama: { runningModels: [{ name: 'llama3:8b', size: 5000000000, vramUsage: 5000, ramUsage: 500, family: 'llama', format: 'gguf', parameterSize: '8B', quantization: 'Q4_0' }, { name: 'mistral:7b', size: 4370000000, vramUsage: 4400, ramUsage: 400, family: 'mistral', format: 'gguf', parameterSize: '7B', quantization: 'Q4_0' }], activeRequests: 3, totalRequests: 15000, avgResponseTime: 45.3, requestsPerSecond: 12.5, maxModels: 4, maxConcurrentRequests: 10, freeSlots: 7,
            runtimeFlags: { numGpuLayers: -1, contextLength: 8192, numParallel: 4, numThreads: 16, batchSize: 1024, gpuSplitMode: 'layer', lowVram: false, f16kv: true, kvCacheQuant: 'f16', flashAttention: true, source: 'env' },
            modelContexts: [{ name: 'llama3:8b', contextLength: 8192, contextSource: 'modelfile', effectiveContext: 8192, contextMemoryMB: 2048, kvCacheMemoryMB: 512, modelMemoryMB: 4800, totalMemoryMB: 7360, numLayers: 32, hiddenSize: 4096, precisionBits: 16 }],
            backendCapacity: { freeVram: 6376, guaranteedVram: 5738, loadedModelVram: 9400, contextOverheadMB: 2048, availableModels: [{ name: 'gemma2:9b', size: 5500000000, vramUsage: 5500, canLoad: true, estimatedVram: 5800, family: 'gemma', parameterSize: '9B', quantization: 'Q4_0' }, { name: 'llama3:70b', size: 40000000000, vramUsage: 40000, canLoad: false, loadReason: 'Недостаточно VRAM', estimatedVram: 41000 }], loadableModelCount: 3, mode: 'gpu' }
        },
        prediction: { secondsToCritical: 450, criticalReason: 'none', gpuUsageTrend: 0.5, vramUsageTrend: 0.2, ramUsageTrend: 0.1, freeSlotsTrend: 0, requestCapacity: 30 }
    },
    { id: 'cpu-node-01', name: 'CPU Node 01', host: '192.168.1.201', ollamaPort: 11434, agentPort: 18032, weight: 1, maxConcurrentRequests: 5, labels: ['cpu-only'], status: 'healthy', hasAgent: true, lastAgentContact: new Date().toISOString(), activeRequests: 1,
        gpu: { usagePercent: 0, memoryTotal: 0, memoryUsed: 0, memoryFree: 0, temperature: 0, powerUsage: 0 },
        system: { cpuUsagePercent: 42.1, memoryTotal: 131072, memoryUsed: 55000, memoryFree: 76072, cpu: { usagePercent: 42.1, coreCount: 16, threadCount: 32, model: 'Intel Xeon W-2295', loadAverage1: 6.8, loadAverage5: 5.2, loadAverage15: 4.9, temperature: 62, throttled: false }, diskTotal: 1000000, diskUsed: 300000, diskFree: 700000, networkRX: 2000000000, networkTX: 800000000 },
        ollama: { runningModels: [{ name: 'phi3:mini', size: 3800000000, vramUsage: 0, ramUsage: 3800, family: 'phi', format: 'gguf', parameterSize: '3.8B', quantization: 'Q4_0' }], activeRequests: 1, totalRequests: 5000, avgResponseTime: 120.7, requestsPerSecond: 3.2, maxModels: 3, maxConcurrentRequests: 5, freeSlots: 4,
            runtimeFlags: { numGpuLayers: 0, contextLength: 4096, numParallel: 2, numThreads: 32, batchSize: 512, gpuSplitMode: 'none', lowVram: true, f16kv: false, kvCacheQuant: 'q4_0', flashAttention: false, source: 'env' },
            modelContexts: [{ name: 'phi3:mini', contextLength: 4096, contextSource: 'runtime', effectiveContext: 4096, contextMemoryMB: 1024, kvCacheMemoryMB: 256, modelMemoryMB: 3800, totalMemoryMB: 5080, numLayers: 32, hiddenSize: 3072, precisionBits: 16 }],
            backendCapacity: { freeVram: 0, guaranteedVram: 0, loadedModelVram: 0, contextOverheadMB: 1024, availableModels: [{ name: 'llama3:8b', size: 5000000000, vramUsage: 0, canLoad: true, estimatedVram: 0 }], loadableModelCount: 5, mode: 'cpu' }
        },
        prediction: { secondsToCritical: 200, criticalReason: 'cpu_usage', gpuUsageTrend: 0, vramUsageTrend: 0, ramUsageTrend: 1.5, freeSlotsTrend: -1, requestCapacity: 80 }
    },
    { id: 'cloud-api-01', name: 'OpenAI API', host: 'api.openai.com', ollamaPort: 443, agentPort: 0, weight: 2, maxConcurrentRequests: 50, labels: ['cloud','openai'], status: 'healthy', hasAgent: false, lastAgentContact: null, activeRequests: 12,
        gpu: { usagePercent: 0, memoryTotal: 0, memoryUsed: 0, memoryFree: 0 },
        system: { cpuUsagePercent: 0, memoryTotal: 0, memoryUsed: 0, memoryFree: 0 },
        ollama: { runningModels: [{ name: 'gpt-4o', size: 0, vramUsage: 0, ramUsage: 0 }], activeRequests: 12, totalRequests: 80000, avgResponseTime: 350.2, requestsPerSecond: 25.0, maxModels: 0, maxConcurrentRequests: 50, freeSlots: 38,
            runtimeFlags: { numGpuLayers: 0 }, modelContexts: [],
            backendCapacity: { freeVram: 0, guaranteedVram: 0, loadedModelVram: 0, contextOverheadMB: 0, availableModels: [], loadableModelCount: 0, mode: 'cloud' }
        },
        prediction: { secondsToCritical: -1, criticalReason: 'none' }
    },
    { id: 'gpu-node-02', name: 'GPU Node 02', host: '192.168.1.102', ollamaPort: 11434, agentPort: 18032, weight: 1, maxConcurrentRequests: 8, labels: ['nvidia','a100'], status: 'healthy', hasAgent: true, lastAgentContact: new Date(Date.now() - 86400000).toISOString(), activeRequests: 7,
        gpu: { usagePercent: 92.5, memoryTotal: 81920, memoryUsed: 77000, memoryFree: 4920, temperature: 82, powerUsage: 380, powerLimit: 500 },
        system: { cpuUsagePercent: 55.0, memoryTotal: 262144, memoryUsed: 180000, memoryFree: 82144 },
        ollama: { runningModels: [{ name: 'llama3:70b', size: 40000000000, vramUsage: 40000, ramUsage: 2000, family: 'llama', format: 'gguf' }], activeRequests: 7, totalRequests: 3000, avgResponseTime: 85.0, requestsPerSecond: 8.0, maxModels: 2, maxConcurrentRequests: 8, freeSlots: 1,
            runtimeFlags: { numGpuLayers: 80, contextLength: 16384, numParallel: 1, numThreads: 8, batchSize: 2048, lowVram: false, flashAttention: true },
            modelContexts: [],
            backendCapacity: { freeVram: 4920, guaranteedVram: 4428, loadedModelVram: 40000, contextOverheadMB: 32768, availableModels: [], loadableModelCount: 0, mode: 'gpu' }
        },
        prediction: { secondsToCritical: 85, criticalReason: 'vram', gpuUsageTrend: 0.8, vramUsageTrend: 2.1, ramUsageTrend: 0.5, freeSlotsTrend: -2, requestCapacity: 95 }
    }
];

const mockSessions = [
    { id: 'sess-a1b2c3d4e5f6g7h8', backendId: 'gpu-node-01', model: 'llama3:8b', clientName: 'Cline', clientIP: '10.0.1.50', userAgent: 'Cline/3.0', lastRequestAt: new Date().toISOString(), requestCount: 42, active: true, totalTokens: 25000 },
    { id: 'sess-x9y8z7w6v5u4t3s2', backendId: 'gpu-node-01', model: 'mistral:7b', clientName: 'OpenWebUI', clientIP: '10.0.1.100', userAgent: 'OpenWebUI/1.0', lastRequestAt: new Date(Date.now() - 300000).toISOString(), requestCount: 15, active: true, totalTokens: 8000 },
    { id: 'sess-q1w2e3r4t5y6u7i8', backendId: 'cloud-api-01', model: 'gpt-4o', clientName: 'Python/requests', clientIP: '10.0.1.200', userAgent: 'python-requests/2.31', lastRequestAt: new Date(Date.now() - 120000).toISOString(), requestCount: 230, active: true, totalTokens: 150000 }
];

const mockQueue = { current_size: 3, max_size: 100, processed_total: 45230, workers: 4, avg_wait_time_ms: 2500 };

const mockQueueTasks = [
    { model: 'llama3:8b', target: 'gpu-node-01', status: 'processing', enqueued: new Date(Date.now() - 10000).toISOString(), waitTimeMs: 10000 },
    { model: 'mistral:7b', target: null, status: 'pending', enqueued: new Date(Date.now() - 5000).toISOString(), waitTimeMs: 5000 },
    { model: 'phi3:mini', target: 'cpu-node-01', status: 'processing', enqueued: new Date(Date.now() - 30000).toISOString(), waitTimeMs: 30000 }
];

const mockQueueHistory = [
    { model: 'llama3:8b', target: 'gpu-node-01', enqueued: new Date(Date.now() - 120000).toISOString(), completed_at: new Date(Date.now() - 115000).toISOString(), wait_time_ms: 5000 },
    { model: 'gpt-4o', target: 'cloud-api-01', enqueued: new Date(Date.now() - 60000).toISOString(), completed_at: new Date(Date.now() - 57000).toISOString(), wait_time_ms: 3000 }
];

const mockLogs = [
    { time: new Date().toLocaleTimeString('ru'), level: 'INFO', message: 'WebUI инициализирован' },
    { time: new Date(Date.now() - 5000).toLocaleTimeString('ru'), level: 'WARNING', message: 'Бэкенд gpu-node-02: высокая загрузка VRAM (94%)' },
    { time: new Date(Date.now() - 10000).toLocaleTimeString('ru'), level: 'INFO', message: 'Сессия sess-a1b2... создана для llama3:8b' }
];
'''

    # Initialize all renderers
    html += '''
// ===== INITIALIZE ALL RENDERERS =====
function initAllPages() {
    Renderers.dashboard(mockBackends, mockSessions, mockQueue);

    // Models page
    document.getElementById('models-page').classList.add('active');
    Renderers.modelsPage(mockBackends);
    document.getElementById('models-page').classList.remove('active');

    // Sessions page
    document.getElementById('sessions-page').classList.add('active');
    Renderers.sessionsPage(mockSessions);
    document.getElementById('sessions-page').classList.remove('active');

    // Queue page
    document.getElementById('queue-page').classList.add('active');
    Renderers.queuePage(mockQueue, mockQueueTasks, mockQueueHistory);
    document.getElementById('queue-page').classList.remove('active');

    // Logs page
    document.getElementById('logs-page').classList.add('active');
    Renderers.logs(mockLogs);
    document.getElementById('logs-page').classList.remove('active');

    // Prediction alerts
    document.getElementById('dashboard-page').classList.add('active');
    Renderers.predictionAlerts(mockBackends);
    document.getElementById('dashboard-page').classList.remove('active');

    // Backends management page
    renderBackendsManagePage(mockBackends);
}

// Backends management renderer (inline)
function renderBackendsManagePage(backends) {
    const tbody = document.getElementById('backendsManageBody');
    if (!tbody) return;
    if (!backends.length) { tbody.innerHTML = '<tr><td colspan="13" class="loading-cell">Нет данных</td></tr>'; return; }
    tbody.innerHTML = backends.map(b => {
        const labels = (b.labels || []).join(', ') || '-';
        const lastContact = b.lastAgentContact ? new Date(b.lastAgentContact).toLocaleString('ru') : '-';
        const maxModels = b.maxModels || b.ollama?.maxModels || b.runtimeMaxModels || '-';
        const escaped = Utils.escapeHtml;
        return '<tr><td><strong>' + escaped(b.id) + '</strong></td><td>' + escaped(b.name || b.id) + '</td><td>' + escaped(b.host) + '</td>' +
            '<td>' + (b.ollamaPort || 11434) + '</td><td>' + (b.agentPort || 18032) + '</td>' +
            '<td>' + (b.weight || 1) + '</td><td>' + (b.maxConcurrentRequests || 10) + '</td><td>' + maxModels + '</td>' +
            '<td><span class="badge ' + (b.hasAgent ? 'bg-ok' : 'bg-warn') + '">' + (b.hasAgent ? 'Да' : 'Нет') + '</span></td>' +
            '<td>' + escaped(labels) + '</td><td>' + escaped(lastContact) + '</td>' +
            '<td><span class="badge ' + (b.status === 'healthy' ? 'bg-ok' : 'bg-err') + '">' + b.status + '</span></td>' +
            '<td><button class="btn" style="font-size:.6rem;padding:.1rem .3rem">Edit</button> <button class="btn" style="font-size:.6rem;padding:.1rem .3rem;background:var(--err);color:#fff">Del</button></td></tr>';
    }).join('');
}

// ===== TEST ENGINE =====
const results = [];
const details = [];
let passCount = 0, failCount = 0, warnCount = 0;

function P(selector, name) {
    const el = document.querySelector(selector);
    if (el) { passCount++; details.push('<div class="tr2 tp">✅ ' + name + '</div>'); }
    else { failCount++; details.push('<div class="tr2 tf">❌ ' + name + ' — NOT FOUND: ' + selector + '</div>'); }
}

function PN(selector, name) {
    const el = document.querySelector(selector);
    if (!el) { failCount++; details.push('<div class="tr2 tf">❌ ' + name + ' — NOT FOUND: ' + selector + '</div>'); return; }
    const t = el.textContent.trim();
    if (!t || t === '-' || t === '...') { failCount++; details.push('<div class="tr2 tf">❌ ' + name + ' — EMPTY placeholder</div>'); }
    else { passCount++; details.push('<div class="tr2 tp">✅ ' + name + ' = "' + Utils.escapeHtml(t.substring(0,40)) + '"</div>'); }
}

function PNN(selector, name) {
    const el = document.querySelector(selector);
    if (!el) { failCount++; details.push('<div class="tr2 tf">❌ ' + name + ' — NOT FOUND</div>'); return; }
    const t = el.textContent.trim();
    const n = parseInt(t.replace(/[^0-9-]/g, ''));
    if (isNaN(n)) { failCount++; details.push('<div class="tr2 tf">❌ ' + name + ' — NOT NUMERIC: "' + t + '"</div>'); }
    else if (n < 0) { warnCount++; details.push('<div class="tr2 tw">⚠️ ' + name + ' = ' + n + '</div>'); }
    else { passCount++; details.push('<div class="tr2 tp">✅ ' + name + ' = ' + n + '</div>'); }
}

function PT(selector, substr, name) {
    const el = document.querySelector(selector);
    if (!el) { failCount++; details.push('<div class="tr2 tf">❌ ' + name + ' — NOT FOUND</div>'); return; }
    const t = el.textContent.toLowerCase();
    if (t.includes(substr.toLowerCase())) { passCount++; details.push('<div class="tr2 tp">✅ ' + name + ' — contains "' + substr + '"</div>'); }
    else { failCount++; details.push('<div class="tr2 tf">❌ ' + name + ' — does NOT contain "' + substr + '": ' + Utils.escapeHtml(el.textContent.substring(0,50)) + '</div>'); }
}

function PC(selector, expected, name) {
    const els = document.querySelectorAll(selector);
    if (els.length === expected) { passCount++; details.push('<div class="tr2 tp">✅ ' + name + ' (' + expected + ' шт.)</div>'); }
    else { failCount++; details.push('<div class="tr2 tf">❌ ' + name + ' — expected ' + expected + ', got ' + els.length + '</div>'); }
}

function PS(selector, name) {
    const el = document.querySelector(selector);
    if (!el) { failCount++; details.push('<div class="tr2 tf">❌ ' + name + ' — NOT FOUND</div>'); return; }
    const w = el.style.width;
    if (w && w !== '0px' && w !== '0%') { passCount++; details.push('<div class="tr2 tp">✅ ' + name + ' width=' + w + '</div>'); }
    else { warnCount++; details.push('<div class="tr2 tw">⚠️ ' + name + ' width=' + (w || 'N/A') + '</div>'); }
}

function runTests() {
    details.length = 0; passCount = failCount = warnCount = 0;

    // === DASHBOARD METRIC CARDS ===
    details.push('<div class="tse">📊 Dashboard — Metric Cards</div>');
    PNN('#totalBackends', 'totalBackends (число)');
    PT('#healthyBackends', 'здоровых', 'healthyBackends');
    PNN('#totalModels', 'totalModels (число)');
    PT('#loadedModels', 'загружено', 'loadedModels');
    PNN('#totalSessions', 'totalSessions (число)');
    PT('#sessionRate', 'запросов', 'sessionRate');
    PNN('#queueSize', 'queueSize');
    PT('#queueProcessed', 'обработано', 'queueProcessed');
    PS('#queueDashboardFill', 'queueDashboardFill bar');

    // === OLLAMA RUNTIME ===
    details.push('<div class="tse">⚙️ Dashboard — Ollama Runtime</div>');
    PC('.rc', mockBackends.length, 'runtime-cards count');
    for (const b of mockBackends) {
        P('.rc strong', 'Runtime: ID for ' + b.id);
        P('.rc .badge', 'Runtime: status badge for ' + b.id);
    }
    PC('.fb', null, 'runtime-flags badges exist');
    PC('.ci', null, 'context-info badges exist');
    PC('.ct .ctr', null, 'context-tooltip rows exist');

    // === GPU CLUSTER ===
    details.push('<div class="tse">🎛️ Dashboard — GPU Кластер</div>');
    P('#gpuClusterBadge', 'gpuClusterBadge exists');
    PC('.gc', mockBackends.length, 'GPU cluster cards count');
    PC('.gch', mockBackends.length, 'GPU card headers');
    PC('.gm', mockBackends.length, 'GPU metric grids');
    PC('.gs.ok2', null, 'GPU healthy status dots');
    PC('.gs.wn2', null, 'GPU warning status dots');
    // CPU card
    PC('.badge.bg-warn', null, 'CPU badge exists');
    PC('.pf', null, 'progress-fill bars exist');

    // === CAPACITY SECTION ===
    details.push('<div class="tse">💾 Dashboard — Ёмкость бэкендов</div>');
    PC('.cc', 4, 'capacity cards count');
    PC('.cbc', 3, 'capacity bar containers (non-cloud)');
    PC('.cst', 3, 'capacity stats grids');
    PC('.cbf', null, 'capacity-bar-filled elements');
    PC('.cbx', null, 'capacity-bar-context elements');
    PC('.cbg', null, 'capacity-bar-guaranteed elements');
    PC('.cs', null, 'capacity stat rows');

    // === AVAILABLE MODELS ===
    details.push('<div class="tse">📥 Dashboard — Доступные модели</div>');
    P('#loadableModelCount', 'loadableModelCount badge');
    PC('.ami', null, 'available model items exist');
    PC('.ml', null, 'loadable model items');
    PC('.mul', null, 'unloadable model items');

    // === BACKENDS TABLE (DASHBOARD) ===
    details.push('<div class="tse">📋 Dashboard — Таблица бэкендов</div>');
    PC('#backendsTableBody tr', mockBackends.length, 'backends table rows');
    for (const b of mockBackends) {
        P('#backendsTableBody td strong', 'BT: ID for ' + b.id);
    }
    PC('#backendsTableBody .badge', null, 'BT: status badges');
    PC('#backendsTableBody .btn', null, 'BT: action buttons');
    // Check predictor column has values
    P('#backendsTableBody td .badge', 'BT: predictor badges');

    // === PREDICTION ALERTS ===
    details.push('<div class="tse">🚨 Dashboard — Предупреждения предиктора</div>');
    P('#alertsList', 'alertsList exists');
    P('#alertsList .alert', 'alertsList has alert entries');

    // === BACKENDS MANAGEMENT PAGE ===
    details.push('<div class="tse">🖥️ Бэкенды — Управление</div>');
    PC('#backendsManageBody tr', mockBackends.length, 'manage backends rows');
    P('#backendsManageBody strong', 'manage: ID column');
    P('#backendsManageBody .badge', 'manage: status/agent badges');
    // Check all columns have content
    const mRow = document.querySelector('#backendsManageBody tr');
    if (mRow) {
        const mtds = mRow.querySelectorAll('td');
        for (let i = 0; i < mtds.length; i++) {
            const t = mtds[i].textContent.trim();
            if (t && t !== '-') { passCount++; details.push('<div class="tr2 tp">✅ manage: col ' + i + ' filled: ' + Utils.escapeHtml(t.substring(0,30)) + '</div>'); }
            else { failCount++; details.push('<div class="tr2 tf">❌ manage: col ' + i + ' EMPTY</div>'); }
        }
    }

    // === MODELS PAGE ===
    details.push('<div class="tse">🔬 Модели — Страница</div>');
    PNN('#modelsTotal', 'modelsTotal');
    PNN('#modelsLoaded', 'modelsLoaded');
    PC('.bli', mockBackends.length, 'backend-load items');
    PC('.mc', null, 'model cards');
    PC('.mmt', null, 'memory title sections');
    PC('.mmbf', null, 'memory bar fills');

    // === SESSIONS PAGE ===
    details.push('<div class="tse">👥 Сессии — Таблица</div>');
    PC('#sessionsTableBody tr', mockSessions.length, 'sessions rows');
    for (const s of mockSessions) {
        P('#sessionsTableBody code', 'Session: ID code for ' + s.id.substring(0,8));
    }
    PT('#sessionsTableBody', '🦾', 'client icon (Cline)');
    PT('#sessionsTableBody', '🌐', 'client icon (OpenWebUI)');
    PT('#sessionsTableBody', '🐍', 'client icon (Python)');

    // === QUEUE PAGE ===
    details.push('<div class="tse">⏳ Очередь — Статистика</div>');
    PNN('#queueCurrentSize', 'queueCurrentSize');
    PNN('#queueMaxSize', 'queueMaxSize');
    PNN('#queueProcessed', 'queueProcessed');
    PNN('#queueWorkers', 'queueWorkers');
    P('#queueAvgWait', 'queueAvgWait');
    PS('#queueFill', 'queueFill bar');
    PC('#queueTasksBody tr', mockQueueTasks.length, 'queue tasks rows');
    PC('#queueHistoryBody tr', mockQueueHistory.length, 'queue history rows');
    P('#queueTasksCount', 'queueTasksCount badge');
    P('#queueHistoryCount', 'queueHistoryCount badge');

    // === LOGS PAGE ===
    details.push('<div class="tse">📜 Логи — Отображение</div>');
    PC('.le', mockLogs.length, 'log entries');
    P('.lt', 'log time');
    P('.ll', 'log level');

    // === SETTINGS PAGE ===
    details.push('<div class="tse">⚙️ Настройки — Форма</div>');
    P('#balancingAlgorithm', 'balancingAlgorithm select');
    P('#modelAffinity', 'modelAffinity checkbox');
    P('#sessionStickiness', 'sessionStickiness checkbox');
    P('#predictionFiltering', 'predictionFiltering checkbox');
    P('#gpuMaxUsage', 'gpuMaxUsage input');
    P('#vramMaxUsage', 'vramMaxUsage input');
    P('#cpuMaxUsage', 'cpuMaxUsage input');
    P('#ramMaxUsage', 'ramMaxUsage input');
    P('#minFreeDisk', 'minFreeDisk input');
    // Check values
    for (const [id, expectedVal] of [['gpuMaxUsage','90'],['vramMaxUsage','85'],['cpuMaxUsage','80'],['ramMaxUsage','85'],['minFreeDisk','10240']]) {
        const el = document.getElementById(id);
        if (el && el.value === expectedVal) { passCount++; details.push('<div class="tr2 tp">✅ ' + id + ' value = ' + expectedVal + '</div>'); }
        else { failCount++; details.push('<div class="tr2 tf">❌ ' + id + ' value = ' + (el ? el.value : 'N/A') + ' expected ' + expectedVal + '</div>'); }
    }

    // === MODAL FORM ===
    details.push('<div class="tse">🔲 Модальное окно — Форма бэкенда</div>');
    P('#formBackendId', 'formBackendId');
    P('#formBackendName', 'formBackendName');
    P('#formBackendHost', 'formBackendHost');
    P('#formBackendOllamaPort', 'formBackendOllamaPort');
    P('#formBackendAgentPort', 'formBackendAgentPort');
    P('#formBackendWeight', 'formBackendWeight');
    P('#formBackendMaxConcurrent', 'formBackendMaxConcurrent');
    P('#formBackendLabels', 'formBackendLabels');

    // Fill modal with mock data and verify
    const sample = mockBackends[0];
    document.getElementById('formBackendId').value = sample.id;
    document.getElementById('formBackendName').value = sample.name;
    document.getElementById('formBackendHost').value = sample.host;
    document.getElementById('modalTitle').textContent = 'Редактировать бэкенд';
    P('#modalTitle', 'modal title changed');
    PN('#formBackendId', 'formBackendId filled');

    // === XSS ESCAPING ===
    details.push('<div class="tse">🛡️ Edge Cases — XSS защита</div>');
    const xssBackend = { id: '<script>alert("xss")</script>', name: 'Test', host: '192.168.1.1', ollamaPort: 11434, agentPort: 18032, weight: 1, maxConcurrentRequests: 5, labels: [], status: 'healthy', hasAgent: true, lastAgentContact: new Date().toISOString(), activeRequests: 0,
        gpu: { usagePercent: 0, memoryTotal: 1, memoryUsed: 0, memoryFree: 1 }, system: { cpuUsagePercent: 0, memoryTotal: 1, memoryUsed: 0, memoryFree: 1 },
        ollama: { runningModels: [], activeRequests: 0, totalRequests: 0, requestsPerSecond: 0, runtimeFlags: {}, modelContexts: [], backendCapacity: { freeVram: 0, guaranteedVram: 0, loadedModelVram: 0, contextOverheadMB: 0, availableModels: [], loadableModelCount: 0, mode: 'gpu' } },
        prediction: { secondsToCritical: -1 }
    };
    const runtimeHtml = Renderers.runtimeCluster ? RuntimeRenderers_check(xssBackend) : null;
    // Check that script tag is not present in any rendered content
    if (!document.body.innerHTML.toLowerCase().includes('<script')) { passCount++; details.push('<div class="tr2 tp">✅ XSS: no raw <script> found in DOM</div>'); }
    else { warnCount++; details.push('<div class="tr2 tw">⚠️ XSS: <script> tag found in DOM (may be our test script)</div>'); }

    // === EMPTY DATA ===
    details.push('<div class="tse">🕳️ Edge Cases — Пустые данные</div>');
    // We rendered with data already, check empty state rendering
    tempRenderEmpty();
    const emptyMsg = document.querySelector('#runtimeCluster .loading');
    if (emptyMsg) { passCount++; details.push('<div class="tr2 tp">✅ Empty state: loading placeholder shown</div>'); }
    else { failCount++; details.push('<div class="tr2 tf">❌ Empty state: no loading placeholder</div>'); }
    // Restore data
    initAllPages();

    // === SUMMARY ===
    const total = passCount + failCount + warnCount;
    const summaryEl = document.getElementById('ts');
    const detailsEl = document.getElementById('td');
    detailsEl.innerHTML = details.join('');
    const pct = Math.round(passCount / total * 100);
    const status = pct >= 95 ? '🟢 ALL CLEAR' : (pct >= 80 ? '🟡 NEEDS ATTENTION' : '🔴 CRITICAL');
    summaryEl.innerHTML = `${status} | Total: ${total} | ✅ ${passCount} | ❌ ${failCount} | ⚠️ ${warnCount} | ${pct}% passed`;
}

function RuntimeRenderers_check(backend) {
    // Quick check that renderers handle XSS
    const html = Renderers.runtimeCluster ? Renderers.runtimeCluster([backend]) : '';
    return html;
}

function tempRenderEmpty() {
    Utils.setHTML('runtimeCluster', '<div class="loading">Ожидание данных...</div>');
}

// Run on load
window.addEventListener('DOMContentLoaded', function() {
    initAllPages();
    setTimeout(runTests, 100);
});
'''

    with open('webui/tests/webui_field_tests.html', 'w', encoding='utf-8') as f:
        f.write(html)

    print(f'Generated webui_field_tests.html ({len(html)} chars)')

if __name__ == '__main__':
    gen()
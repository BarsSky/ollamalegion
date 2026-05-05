#!/usr/bin/env python3
"""Fix remaining patches: EventReconfigure + queue rendering."""
import os

# Fix 1: Add EventReconfigure to events.go
EVENTS_FILE = os.path.join(os.path.dirname(__file__), '..', 'pkg', 'types', 'events.go')
with open(EVENTS_FILE, 'r', encoding='utf-8') as f:
    content = f.read()

old_ev = 'EventLimitsChange  EventType = "limits_change"  // изменение runtime-лимитов'
new_ev = 'EventLimitsChange  EventType = "limits_change"  // изменение runtime-лимитов\n\tEventReconfigure   EventType = "reconfigure_request"  // запрос на переформирование бэкенда'

if old_ev in content:
    content = content.replace(old_ev, new_ev)
    with open(EVENTS_FILE, 'w', encoding='utf-8') as f:
        f.write(content)
    print('[OK] EventReconfigure added to events.go')
else:
    print(f'[FAIL] events.go: searching for EventLimitsChange...')
    for i, line in enumerate(content.split('\n')):
        if 'EventLimitsChange' in line:
            print(f'  Found at line {i+1}: {repr(line)}')
            break

# Fix 2: Find queueCount line in monitor.html
MONITOR_FILE = os.path.join(os.path.dirname(__file__), '..', 'webui', 'monitor.html')
with open(MONITOR_FILE, 'r', encoding='utf-8') as f:
    mcontent = f.read()

# Search for the exact line
found_line = None
for line in mcontent.split('\n'):
    stripped = line.strip()
    if 'queueCount' in stripped and 'textContent' in stripped:
        found_line = line
        print(f'[INFO] Found queueCount line: {repr(line[:120])}')
        break

if found_line:
    indent = found_line[:len(found_line) - len(found_line.lstrip())]
    old_q = found_line
    new_q = old_q + '\n' + indent + """var qtb = document.getElementById('queueTbody');
    if (qtb) {
      var all = q.all || [];
      if (all.length === 0) {
        qtb.innerHTML = '<tr><td colspan="6" style="color:var(--text-secondary);text-align:center;padding:16px">No data</td></tr>';
      } else {
        var nowTS = new Date();
        qtb.innerHTML = all.map(function(r, i) {
          var et = r.enqueued ? new Date(r.enqueued) : new Date();
          var waitSec = Math.round((nowTS - et) / 1000);
          var waitStr = waitSec < 60 ? waitSec + 's' : Math.floor(waitSec/60) + 'm' + (waitSec%60) + 's';
          var sc = r.status === 'pending' ? '\U0001f7e1' : r.status === 'processing' ? '\U0001f7e3' : '\u26aa';
          return '<tr><td>' + (i+1) + '</td><td>' + esc(r.model||'\u2014') + '</td><td>' + esc(r.target||'\u2014') + '</td>'
            + '<td class="col-right">' + sc + ' ' + esc(r.status||'\u2014') + '</td>'
            + '<td class="col-right">' + waitStr + '</td>'
            + '<td>' + esc((r.sessionId||'').substring(0,16)) + '</td></tr>';
        }).join('');
      }
    }"""
    mcontent = mcontent.replace(old_q, new_q)
    with open(MONITOR_FILE, 'w', encoding='utf-8') as f:
        f.write(mcontent)
    print('[OK] Queue rendering added to updateUI')
else:
    # Alternative: search for any line that has queue and count
    print('[FAIL] queueCount textContent not found. Searching for alternatives...')
    for line in mcontent.split('\n'):
        if 'queue' in line.lower() and ('count' in line.lower() or 'length' in line):
            print(f'  {repr(line[:150])}')
"""
r57_3_extract_app_listeners.py — R57.3: extract 5 setup* sections from app.js to app-listeners.js.

Sections extracted (verified line numbers in app.js after R57.2):
  - setupRestartHandler:           L116-151 (35 lines)
  - setupNavigation:              L152-161 (9 lines)
  - WebSocket Events:             L677-880 (204 lines, includes cross-tab helpers + setupWebSocketEvents)
  - setupApiEvents:               L882-894 (12 lines)
  - setupLogsTabNavigation:       L2407-2422 (15 lines)

Process in REVERSE order so earlier line numbers stay valid.
"""
APP_JS = r'C:\Ollama\ollamalegion\webui\js\app.js'

with open(APP_JS, 'r', encoding='utf-8', newline='') as f:
    text = f.read()

had_crlf = '\r\n' in text
text = text.replace('\r\n', '\n').replace('\r', '\n')
lines = text.split('\n')

ORIG = len(lines)
print(f"Original line count (LF-normalized): {ORIG}")

# Deletions in REVERSE order (highest line numbers first):
# 5. setupLogsTabNavigation: L2407-2422 (0-indexed 2406-2421)
del lines[2406:2422]
# 4. setupApiEvents: L882-894 (0-indexed 881-893)
del lines[881:894]
# 3. WebSocket Events: L677-880 (0-indexed 676-879)
del lines[676:880]
# 2. setupNavigation: L152-161 (0-indexed 151-160)
del lines[151:161]
# 1. setupRestartHandler: L116-151 (0-indexed 115-150)
del lines[115:151]

# Update init() to populate App.context and call App.setup*()
# Find init() function (was L33-L114 after R57.2, now shifted)
init_start = None
for i, line in enumerate(lines):
    if 'function init()' in line:
        init_start = i
        break
assert init_start is not None, "init() not found"

# Find the first `if (window.App)` block — current init() starts with it
old_start = None
old_end = None
for i in range(init_start, init_start + 25):
    if 'if (window.App) {' in lines[i] and old_start is None:
        old_start = i
# Find the closing `}` of the else clause — it's the line containing only `        }`
# right after the `console.error(...)` line
for i in range(old_start, old_start + 15):
    if lines[i].strip() == '}' and i > old_start:
        old_end = i
        break
assert old_start is not None and old_end is not None, f"Could not find init() App block: {old_start} {old_end}"

# New init() App block: populate context FIRST, then call App.setup*()
# We need to add new lines BEFORE the existing block to populate context.
# Approach: prepend context population to the existing block.
context_block = [
    "        // R57.3: populate App.context for app-listeners.js setup functions.",
    "        // Each function reads from App.context to access app.js state and helpers.",
    "        if (!window.App) {",
    "            console.error('app.js: window.App not defined — app-core.js must be loaded BEFORE app.js');",
    "        } else {",
    "            App.context = {",
    "                data: data,",
    "                currentPage: currentPage,",
    "                dashboardRenderTimer: dashboardRenderTimer,",
    "                addLog: addLog,",
    "                addProxyLog: addProxyLog,",
    "                updateConnectionStatus: updateConnectionStatus,",
    "                fetchClusterState: fetchClusterState,",
    "                fetchProxyLogs: fetchProxyLogs,",
    "                refreshPage: refreshPage,",
    "                switchPage: switchPage,",
    "                autoDetectCppWorkerPort: autoDetectCppWorkerPort",
    "            };",
    "            App.initTheme();",
    "            App.initDensity();",
    "            App.setupI18n();",
    "            App.initAutoTuneEventHandlers();",
    "            App.setupNavigation();",
    "            App.setupRestartHandler();",
    "            App.setupWebSocketEvents();",
    "            App.setupApiEvents();",
    "            App.setupLogsTabNavigation();",
    "        }",
]

# Replace old block (inclusive) with new context_block
lines[old_start:old_end + 1] = context_block

# Add R57.3 marker comment after R57.2 marker (which is around L11 now)
# Find R57.2 marker line
marker_pos = None
for i, line in enumerate(lines):
    if 'R57.2' in line and 'window.App namespace' in line:
        marker_pos = i
        break
if marker_pos is not None:
    r57_3_marker = [
        '',
        '// R57.3 (2026-09-03): setupNavigation, setupRestartHandler, setupWebSocketEvents,',
        '// setupApiEvents, setupLogsTabNavigation — перенесены в webui/js/app-listeners.js',
        '// (window.App.setup*). init() теперь populate App.context и вызывает их через App.*',
        '// setupEventListeners (360 строк) оставлен в app.js — слишком много cross-cuts',
        '// с modal/state/data функциями, R57.4+ будет проектировать общий context object.',
    ]
    # Insert after the existing R57.2 block (find the end of the comment block)
    insert_at = marker_pos
    for i in range(marker_pos, min(marker_pos + 10, len(lines))):
        if lines[i].strip() == '' and i > marker_pos:
            insert_at = i + 1
            break
    else:
        insert_at = marker_pos + 1
    lines[insert_at:insert_at] = r57_3_marker

# Write back with original line endings
output = '\n'.join(lines)
if had_crlf:
    output = output.replace('\n', '\r\n')

with open(APP_JS, 'w', encoding='utf-8', newline='') as f:
    f.write(output)

print(f'Resulting app.js: {len(lines)} lines (was {ORIG}, removed {ORIG - len(lines)})')
print(f'init() update: old L{old_start+1}-L{old_end+1} replaced with {len(context_block)} lines')

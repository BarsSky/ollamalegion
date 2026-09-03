"""
r57_2_split_app_js.py v3 — find sections by content, not by line number.

Robust to mixed CRLF/LF line endings (Python readlines() counts differently
than PowerShell Measure-Object -Line).
"""
import re

APP_JS = r'C:\Ollama\ollamalegion\webui\js\app.js'

with open(APP_JS, 'r', encoding='utf-8', newline='') as f:
    text = f.read()

# Normalize line endings to LF for consistent counting
text = text.replace('\r\n', '\n').replace('\r', '\n')
lines = text.split('\n')

ORIG = len(lines)
print(f"Original line count (LF-normalized): {ORIG}")

# Find sections by content
# 1. initAutoTuneEventHandlers — from `function initAutoTuneEventHandlers()` to the next blank-then-non-blank pattern
#    End: just before `// State` (line 14 in normalized; was 60 in original with CR)
# 2. autotune-specific showToast — right after initAutoTuneEventHandlers (was L32-58)
# 3. // ---- Theme ---- through end of setupI18n (was L154-355)

# Approach: find the start of `// ---- Theme ----` and the start of `// ---- Restart Handler ----`
theme_start = None
restart_start = None
for i, line in enumerate(lines):
    if line.strip() == '// ---- Theme ----':
        theme_start = i
    elif line.strip() == '// ---- Restart Handler ----':
        restart_start = i
        break
assert theme_start is not None, "Could not find // ---- Theme ----"
assert restart_start is not None, "Could not find // ---- Restart Handler ----"
print(f"Theme section: L{theme_start+1}-L{restart_start} ({restart_start - theme_start} lines)")

# Section 1: from line 0 until theme_start (1-indexed L1 to L{theme_start})
# But we only want to delete initAutoTuneEventHandlers + autotune showToast, not the JSDoc header.
# The JSDoc header is lines 0-3 (L1-L4), IIFE opens at L5.
# initAutoTuneEventHandlers is at L13.
# So we delete L13-L{theme_start} (0-indexed 12 to theme_start - 1).
# Then theme section is theme_start to restart_start - 1.

# Step 1: delete theme section (L154-L355, 0-indexed 153 to 354 inclusive, that's 202 lines)
del lines[theme_start:restart_start]
# After this, file is ORIG - (restart_start - theme_start) lines.
# The init() function is still intact (it ended at L152 in original, but L150 after theme deletion).

# Step 2: find initAutoTuneEventHandlers
auto_start = None
for i, line in enumerate(lines):
    if 'function initAutoTuneEventHandlers' in line:
        auto_start = i
        break
assert auto_start is not None, "Could not find initAutoTuneEventHandlers after theme deletion"

# Find end of initAutoTuneEventHandlers (its closing `}`)
# The autotune showToast is RIGHT AFTER, with no blank line between (was L30-31).
# We delete initAutoTuneEventHandlers + autotune showToast, ending at the `}` of showToast.
# Actually let's look: after the theme deletion, the file should be at L13 for initAutoTuneEventHandlers.
# The next `// State` comment is at L59 (was 60 in original).
# So we delete from auto_start to (and including) the blank line before `// State`.

state_start = None
for i in range(auto_start + 1, min(auto_start + 50, len(lines))):
    if lines[i].strip().startswith('// State') or 'const data = {' in lines[i]:
        state_start = i
        break
assert state_start is not None, "Could not find // State after initAutoTuneEventHandlers"

# Delete L13 to L{state_start - 1} (0-indexed auto_start to state_start - 1, inclusive)
del lines[auto_start:state_start]

# Step 3: update init() to call App.*
# Find init() function and the first 8 calls
init_start = None
for i, line in enumerate(lines):
    if 'function init()' in line:
        init_start = i
        break
assert init_start is not None, "init() not found"

old_start = None
old_end = None
for i in range(init_start, init_start + 25):
    if 'initTheme();' in lines[i] and old_start is None:
        old_start = i
    if old_start is not None and 'setupWebSocketEvents();' in lines[i]:
        old_end = i
        break
assert old_start is not None and old_end is not None, f"Could not find init() block: {old_start} {old_end}"

new_block = [
    '        // R57.2 (2026-09-03): theme/density/i18n/AutoTune moved to webui/js/app-core.js.',
    '        // app.js must be loaded AFTER app-core.js (see webui/index.html).',
    '        if (window.App) {',
    '            App.initTheme();',
    '            App.initDensity();',
    '            App.setupI18n();',
    '            App.initAutoTuneEventHandlers();',
    '        } else {',
    "            console.error('app.js: window.App not defined — app-core.js must be loaded BEFORE app.js');",
    '        }',
]

lines[old_start:old_end + 1] = new_block

# Step 4: add R57.2 marker after JSDoc header (insert at position 4)
marker = [
    '',
    '// R57.2 (2026-09-03): theme/density/i18n/AutoTune/showToast перенесены',
    '// в webui/js/app-core.js (window.App namespace). В этом файле остались:',
    '// state, init() (вызывает App.*), data fetching, navigation, event listeners,',
    '// settings apply, page render dispatch, backend CRUD, model management,',
    '// agents, logs, export — всё что внутри IIFE.',
    '',
]
lines[4:4] = marker

# Write back with original line endings
# Detect if file originally had CRLF
had_crlf = '\r\n' in text
output = '\n'.join(lines)
if had_crlf:
    output = output.replace('\n', '\r\n')

with open(APP_JS, 'w', encoding='utf-8', newline='') as f:
    f.write(output)

print(f'Resulting app.js: {len(lines)} lines (was {ORIG}, removed {ORIG - len(lines)})')
print(f'init() update: old L{old_start+1}-L{old_end+1} replaced with {len(new_block)} lines')
print(f'Line endings: {"CRLF" if had_crlf else "LF"}')

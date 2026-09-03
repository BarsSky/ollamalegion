"""
r57_5c_extract_list.py — R57.5c: extract renderBackendsPanel + renderBackendsList
from gguf-renderer.js to gguf-renderer-list.js (already created).

Section extracted:
  - renderBackendsPanel (L101-121 in pre-R57.5c gguf-renderer.js)
  - renderBackendsList (L123-186)
  - Section comment "Left panel" + section comment "Right panel" переход

After R57.5c:
  - gguf-renderer.js: -90 строк (left panel functions)
  - gguf-renderer-list.js: NEW (7.1KB)
  - Total gguf-renderer.js: ~2660 строк (down from 2743, -3%)

Changes to gguf-renderer.js:
  1. Remove renderBackendsPanel + renderBackendsList definitions.
  2. Add aliases after the M-state aliases:
       var renderBackendsPanel = M.renderBackendsPanel;
       var renderBackendsList = M.renderBackendsList;
"""
GGUF_JS = r'C:\Ollama\ollamalegion\webui\js\modules\gguf-renderer.js'

with open(GGUF_JS, 'r', encoding='utf-8', newline='') as f:
    text = f.read()

had_crlf = '\r\n' in text
text = text.replace('\r\n', '\n').replace('\r', '\n')
lines = text.split('\n')

ORIG = len(lines)
print(f"Original line count (LF-normalized): {ORIG}")

# Step 1: find renderBackendsPanel function
panel_start = None
for i, line in enumerate(lines):
    if 'function renderBackendsPanel()' in line:
        panel_start = i
        break
assert panel_start is not None, "Could not find renderBackendsPanel"
# Make sure the previous line is the section header or blank
print(f"renderBackendsPanel at L{panel_start+1}")

# Step 2: find the END of renderBackendsList (which is the function before "// ---- Right panel: detail ----")
panel_end = None
for i in range(panel_start, min(panel_start + 100, len(lines))):
    if '// ---- Right panel: detail ----' in lines[i]:
        panel_end = i
        break
assert panel_end is not None, "Could not find // ---- Right panel: detail ----"
print(f"Panel functions end at L{panel_end} (before right panel section)")

# Step 3: delete the panel functions and the blank line between them and "// ---- Right panel ----"
# We want to delete from panel_start to panel_end (exclusive of the right panel header line)
# But we want to keep the section comment for the right panel. So delete from panel_start to panel_end.
del lines[panel_start:panel_end]

# Step 4: add aliases after the existing state aliases
# Find where `var visibleBackends = M.visibleBackends;` is
alias_anchor = None
for i, line in enumerate(lines):
    if line.strip() == 'var visibleBackends = M.visibleBackends;':
        alias_anchor = i + 1
        break
assert alias_anchor is not None, "Could not find visibleBackends alias"

list_aliases = [
    '    // Left panel renderers (from gguf-renderer-list.js loaded BEFORE this file).',
    '    var renderBackendsPanel = M.renderBackendsPanel;',
    '    var renderBackendsList = M.renderBackendsList;',
    '',
]
lines[alias_anchor:alias_anchor] = list_aliases

# Write back
output = '\n'.join(lines)
if had_crlf:
    output = output.replace('\n', '\r\n')

with open(GGUF_JS, 'w', encoding='utf-8', newline='') as f:
    f.write(output)

print(f'Resulting gguf-renderer.js: {len(lines)} lines (was {ORIG}, removed {ORIG - len(lines)})')

"""
r57_4_extract_app_modals.py — R57.4: extract Backend CRUD from app.js to app-modals.js.

Section extracted:
  - Backend CRUD: L1356-1555 (200 lines, includes openBackendModal,
    fillForm, closeModal, saveBackend, deleteBackend, editBackend,
    confirmDeleteBackend)

After R57.4:
  - app.js: 2716 → ~2516 lines
  - app-modals.js: NEW

Changes to app.js:
  1. Remove the L1356-1555 section.
  2. Update the return { ... } public API to reference window.BackendCRUD.*
     (set by app-modals.js when it loads, BEFORE app.js IIFE runs).
"""
APP_JS = r'C:\Ollama\ollamalegion\webui\js\app.js'

with open(APP_JS, 'r', encoding='utf-8', newline='') as f:
    text = f.read()

had_crlf = '\r\n' in text
text = text.replace('\r\n', '\n').replace('\r', '\n')
lines = text.split('\n')

ORIG = len(lines)
print(f"Original line count (LF-normalized): {ORIG}")

# Step 1: delete Backend CRUD section (L1356-1555 = 0-indexed 1355-1554)
del lines[1355:1555]

# Step 2: NO init() changes — app-modals.js exposes window.BackendCRUD
# before app.js IIFE runs, so the public API can reference it directly.

# Step 3: update public API return object — replace local function refs
# with window.BackendCRUD.* for modal functions.
return_start = None
return_end = None
for i, line in enumerate(lines):
    if line.strip() == '// ---- Public API ----':
        return_start = i + 1
        break
for i in range(return_start, len(lines)):
    if 'return {' in lines[i]:
        return_start = i
        break
for i in range(return_start, len(lines)):
    if '};' in lines[i]:
        return_end = i
        break
assert return_start is not None and return_end is not None, f"Public API return not found: {return_start} {return_end}"

modal_funcs = ['openBackendModal', 'fillForm', 'closeModal', 'saveBackend', 'deleteBackend', 'editBackend', 'confirmDeleteBackend']

for i in range(return_start, return_end + 1):
    line = lines[i]
    stripped = line.strip()
    if not stripped or stripped.startswith('//') or stripped.startswith('return') or stripped == '};':
        continue
    for fn in modal_funcs:
        # Shorthand: `        openBackendModal,` → needs long form for member expression
        if stripped == fn + ',':
            indent = line[:len(line) - len(line.lstrip())]
            lines[i] = indent + fn + ': window.BackendCRUD.' + fn + ',\n'
            break
        # Long form: `        openBackendModal: openBackendModal,`
        elif stripped.startswith(fn + ':') and fn in stripped[len(fn)+1:]:
            indent = line[:len(line) - len(line.lstrip())]
            lines[i] = indent + fn + ': window.BackendCRUD.' + fn + ',\n'
            break

# Step 4: add R57.4 marker comment after R57.3 marker
marker_pos = None
for i, line in enumerate(lines):
    if 'R57.3' in line and 'window.App.setup*' in line:
        marker_pos = i
        break
if marker_pos is not None:
    r57_4_marker = [
        '',
        '// R57.4 (2026-09-03): Backend CRUD (openBackendModal, fillForm, closeModal,',
        '// saveBackend, deleteBackend, editBackend, confirmDeleteBackend) перенесены',
        '// в webui/js/app-modals.js (exposed via window.BackendCRUD). Public API',
        '// window.ui ссылается на window.BackendCRUD.* (lazy resolution — функции',
        '// доступны ПОСЛЕ загрузки app-modals.js, до app.js IIFE).',
    ]
    insert_at = marker_pos
    for i in range(marker_pos, min(marker_pos + 10, len(lines))):
        if lines[i].strip() == '' and i > marker_pos:
            insert_at = i + 1
            break
    else:
        insert_at = marker_pos + 1
    lines[insert_at:insert_at] = r57_4_marker

output = '\n'.join(lines)
if had_crlf:
    output = output.replace('\n', '\r\n')

with open(APP_JS, 'w', encoding='utf-8', newline='') as f:
    f.write(output)

print(f'Resulting app.js: {len(lines)} lines (was {ORIG}, removed {ORIG - len(lines)})')

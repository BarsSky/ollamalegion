"""
r57_5e_extract_detail.py — R57.5e: extract onDetailPanelClick + bindEvents +
bindSettingsChange from gguf-renderer.js to gguf-renderer-detail.js.

Section extracted (~310 lines):
  - // ---- Delegated detail panel click handler ----
  - onDetailPanelClick (L1467-1625)
  - // ---- Event binding ----
  - bindEvents (L1629-1756)
  - bindSettingsChange (L1758-1767)

After R57.5e:
  - gguf-renderer.js: -310 строк
  - gguf-renderer-detail.js: NEW (16.7KB)
  - Total gguf-renderer.js: ~1485 строк (vs 2743 original, -46%)

Final split (R57.5 wave) — все 6 файлов созданы:
  R57.5a: helpers (3.9KB)     - 53 строки
  R57.5b: state (5.8KB)       - 80 строк
  R57.5c: list (7.1KB)        - 85 строк
  R57.5d-1: actions (13KB)    - 555 строк
  R57.5d-2: refresh (9KB)     - 318 строк
  R57.5e: detail (16.7KB)     - 310 строк
  Total: 1401 строк → 55.5KB (split into 6 files)
  Remaining in gguf-renderer.js: ~1340 строк (right panel render functions + render() + public API)
"""
GGUF_JS = r'C:\Ollama\ollamalegion\webui\js\modules\gguf-renderer.js'

with open(GGUF_JS, 'r', encoding='utf-8', newline='') as f:
    text = f.read()

had_crlf = '\r\n' in text
text = text.replace('\r\n', '\n').replace('\r', '\n')
lines = text.split('\n')

ORIG = len(lines)
print(f"Original line count (LF-normalized): {ORIG}")

# Find // ---- Delegated detail panel click handler ----
detail_start = None
for i, line in enumerate(lines):
    if line.strip() == '// ---- Delegated detail panel click handler ----':
        detail_start = i
        break
assert detail_start is not None, "Could not find Delegated detail panel click handler"
print(f"Detail section starts at L{detail_start+1}")

# Find end: // ---- Public API ----
detail_end = None
for i in range(detail_start + 1, len(lines)):
    if lines[i].strip() == '// ---- Public API ----':
        detail_end = i
        break
assert detail_end is not None, "Could not find // ---- Public API ----"
print(f"Detail section ends at L{detail_end} ({detail_end - detail_start} lines)")

del lines[detail_start:detail_end]

# Add aliases after refresh aliases
alias_anchor = None
for i, line in enumerate(lines):
    if line.strip() == 'var refreshDetailPane = M.refreshDetailPane;':
        alias_anchor = i + 1
        break
assert alias_anchor is not None, "Could not find refreshDetailPane alias"

detail_aliases = [
    '',
    '    // Event handlers (from gguf-renderer-detail.js loaded BEFORE this file).',
    '    var onDetailPanelClick = M.onDetailPanelClick;',
    '    var bindEvents = M.bindEvents;',
    '    var bindSettingsChange = M.bindSettingsChange;',
    '',
]
lines[alias_anchor:alias_anchor] = detail_aliases

# Write back
output = '\n'.join(lines)
if had_crlf:
    output = output.replace('\n', '\r\n')

with open(GGUF_JS, 'w', encoding='utf-8', newline='') as f:
    f.write(output)

print(f'Resulting gguf-renderer.js: {len(lines)} lines (was {ORIG}, removed {ORIG - len(lines)})')

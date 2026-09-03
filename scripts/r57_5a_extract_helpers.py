"""
r57_5a_extract_helpers.py — R57.5a: extract helpers (stripGGUF, formatFileSize, showToast)
from webui/js/modules/gguf-renderer.js to webui/js/modules/gguf-renderer-helpers.js.

Section extracted:
  - // ---- Helpers ---- (L2774-2827 in original, 53 lines)
    - stripGGUF
    - formatFileSize
    - showToast

After R57.5a:
  - gguf-renderer.js: 2743 → ~2690 lines (helpers + JSDoc comment removed)
  - gguf-renderer-helpers.js: NEW (3.9KB)

Changes to gguf-renderer.js:
  1. Remove the L2774-2827 section.
  2. Add a comment marker explaining the move.
  3. Inside the IIFE, alias: const M = window.GgufModule = window.GgufModule || {};
     (M contains stripGGUF/formatFileSize/showToast from helpers file).
"""
GGUF_JS = r'C:\Ollama\ollamalegion\webui\js\modules\gguf-renderer.js'

with open(GGUF_JS, 'r', encoding='utf-8', newline='') as f:
    text = f.read()

had_crlf = '\r\n' in text
text = text.replace('\r\n', '\n').replace('\r', '\n')
lines = text.split('\n')

ORIG = len(lines)
print(f"Original line count (LF-normalized): {ORIG}")

# Step 1: Find the // ---- Helpers ---- section and delete it through the end of showToast
helpers_start = None
for i, line in enumerate(lines):
    if line.strip() == '// ---- Helpers ----':
        helpers_start = i
        break
assert helpers_start is not None, "Could not find // ---- Helpers ----"

# Find the end of showToast function — it's the closing `}` of the function
# The structure is: function showToast { ... } console.log(...) }
# Find next blank line followed by non-helper content (= // ---- Public API ----)
helpers_end = None
for i in range(helpers_start + 1, min(helpers_start + 80, len(lines))):
    if '// ---- Public API ----' in lines[i]:
        helpers_end = i
        break
assert helpers_end is not None, "Could not find // ---- Public API ----"
# The Helpers section is from helpers_start to helpers_end - 1 (Public API comment is at helpers_end)
print(f"Helpers section: L{helpers_start+1}-L{helpers_end}")

del lines[helpers_start:helpers_end]

# Step 2: Find the IIFE opening `const GgufRenderer = (window.GgufRenderer = (function () {`
# and insert the M alias right after it
iife_start = None
for i, line in enumerate(lines):
    if 'const GgufRenderer' in line and '(function ()' in line:
        iife_start = i
        break
assert iife_start is not None, "Could not find IIFE opening"

# Insert M alias after the IIFE opening line
m_alias = [
    '    // R57.5a: namespace for split modules (helpers, state, list, detail, actions, refresh).',
    '    // Each new file (gguf-renderer-helpers.js etc.) adds its functions to this namespace.',
    '    // Current keys at R57.5a: stripGGUF, formatFileSize, showToast (set by helpers.js).',
    '    var M = window.GgufModule = window.GgufModule || {};',
    '',
]
# Insert after iife_start (i.e., before the function body starts)
lines[iife_start + 1:iife_start + 1] = m_alias

# Step 2.5: add local aliases for the helpers so existing call sites work.
# (61 references to stripGGUF / showToast / formatFileSize across the file;
# instead of renaming all of them, we just alias once at the top.)
aliases = [
    '    // Local aliases to namespace functions. Existing call sites (61 of them)',
    '    // keep using bare `stripGGUF(...)`, `showToast(...)` etc. — they resolve',
    '    // to M.* which is set by gguf-renderer-helpers.js loaded before this file.',
    '    var stripGGUF = M.stripGGUF;',
    '    var formatFileSize = M.formatFileSize;',
    '    var showToast = M.showToast;',
    '',
]
# Insert AFTER the M alias block (so order is: IIFE open, M alias, local aliases, body)
lines[iife_start + 1 + len(m_alias):iife_start + 1 + len(m_alias)] = aliases

# Step 3: update return object — stripGGUF and showToast (not in return currently, but
# the public API must still work). Look at the return object.
# Original: return { render, getState, selectBackend, refreshBackends, refreshDetail,
#                   markLoadingModel, markLoadFailed, updateBackendsList, refreshActiveQueriesPolling }
# None of these are stripGGUF/formatFileSize/showToast — they were internal helpers.
# So no public API changes needed.

# Write back
output = '\n'.join(lines)
if had_crlf:
    output = output.replace('\n', '\r\n')

with open(GGUF_JS, 'w', encoding='utf-8', newline='') as f:
    f.write(output)

print(f'Resulting gguf-renderer.js: {len(lines)} lines (was {ORIG}, removed {ORIG - len(lines)})')
print(f'Inserted M alias after L{iife_start + 1}')

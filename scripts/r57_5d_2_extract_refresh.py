"""
r57_5d_2_extract_refresh.py — R57.5d-2: extract Data refresh from gguf-renderer.js.

Section extracted (~280 lines):
  - // ---- Data refresh ---- (L1757-2000+ in pre-R57.5d-2 gguf-renderer.js)
    - _refreshInProgress flag
    - refreshBackends
    - updateBackendsList
    - _detailRefreshInProgress flag
    - refreshDetail
    - startActiveQueriesPolling / stopActiveQueriesPolling
    - refreshActiveQueriesPolling
    - _downloadsPollTimer
    - startDownloadPolling / refreshActiveDownloads
    - updateDetailLoading
    - refreshDetailPanel
    - refreshDetailPane

After R57.5d-2:
  - gguf-renderer.js: -280 строк
  - gguf-renderer-refresh.js: NEW (9KB)
  - Total gguf-renderer.js: ~1850 строк (vs 2743 original, -33%)

R57.5d done. R57.5e (future) — extract right panel (renderDetailPanel + sub-panes).
"""
GGUF_JS = r'C:\Ollama\ollamalegion\webui\js\modules\gguf-renderer.js'

with open(GGUF_JS, 'r', encoding='utf-8', newline='') as f:
    text = f.read()

had_crlf = '\r\n' in text
text = text.replace('\r\n', '\n').replace('\r', '\n')
lines = text.split('\n')

ORIG = len(lines)
print(f"Original line count (LF-normalized): {ORIG}")

# Find the // ---- Data refresh ---- section
refresh_start = None
for i, line in enumerate(lines):
    if line.strip() == '// ---- Data refresh ----':
        refresh_start = i
        break
assert refresh_start is not None, "Could not find // ---- Data refresh ----"
print(f"Data refresh at L{refresh_start+1}")

# Find end — next // ---- section
refresh_end = None
for i in range(refresh_start + 1, len(lines)):
    if lines[i].strip().startswith('// ---- ') and 'Data refresh' not in lines[i]:
        refresh_end = i
        break
assert refresh_end is not None, "Could not find end of Data refresh section"
print(f"Data refresh ends at L{refresh_end} ({refresh_end - refresh_start} lines)")

del lines[refresh_start:refresh_end]

# Add aliases for refresh functions after action aliases
alias_anchor = None
for i, line in enumerate(lines):
    if line.strip() == 'var deleteDownloadedFile = M.deleteDownloadedFile;':
        alias_anchor = i + 1
        break
assert alias_anchor is not None, "Could not find deleteDownloadedFile alias"

refresh_aliases = [
    '',
    '    // Data refresh (from gguf-renderer-refresh.js loaded BEFORE this file).',
    '    var refreshBackends = M.refreshBackends;',
    '    var updateBackendsList = M.updateBackendsList;',
    '    var refreshDetail = M.refreshDetail;',
    '    var startActiveQueriesPolling = M.startActiveQueriesPolling;',
    '    var stopActiveQueriesPolling = M.stopActiveQueriesPolling;',
    '    var refreshActiveQueriesPolling = M.refreshActiveQueriesPolling;',
    '    var startDownloadPolling = M.startDownloadPolling;',
    '    var refreshActiveDownloads = M.refreshActiveDownloads;',
    '    var updateDetailLoading = M.updateDetailLoading;',
    '    var refreshDetailPanel = M.refreshDetailPanel;',
    '    var refreshDetailPane = M.refreshDetailPane;',
    '',
]
lines[alias_anchor:alias_anchor] = refresh_aliases

# Write back
output = '\n'.join(lines)
if had_crlf:
    output = output.replace('\n', '\r\n')

with open(GGUF_JS, 'w', encoding='utf-8', newline='') as f:
    f.write(output)

print(f'Resulting gguf-renderer.js: {len(lines)} lines (was {ORIG}, removed {ORIG - len(lines)})')

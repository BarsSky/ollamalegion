"""
r57_5d_extract_actions.py — R57.5d-1: extract action handlers from gguf-renderer.js.

Section extracted (~530 lines):
  - // ---- Actions ----
    - selectBackend, currentBackend, loadOnSelectedBackend, markLoadingModel,
      markLoadFailed, unloadOnSelectedBackend, deleteOnSelectedBackend,
      cancelActiveGeneration
    - doHfSearch, quickDownloadHf, viewHfFiles, startDownload, cancelDownload,
      deleteDownloadedFile

Changes to gguf-renderer.js:
  1. Remove the action functions and the // ---- Actions ---- section header.
  2. Add aliases:
     var selectBackend = M.selectBackend;
     var currentBackend = M.currentBackend;
     var loadOnSelectedBackend = M.loadOnSelectedBackend;
     var markLoadingModel = M.markLoadingModel;
     var markLoadFailed = M.markLoadFailed;
     var unloadOnSelectedBackend = M.unloadOnSelectedBackend;
     var deleteOnSelectedBackend = M.deleteOnSelectedBackend;
     var cancelActiveGeneration = M.cancelActiveGeneration;
     var doHfSearch = M.doHfSearch;
     var viewHfFiles = M.viewHfFiles;
     var startDownload = M.startDownload;
     var cancelDownload = M.cancelDownload;
     var deleteDownloadedFile = M.deleteDownloadedFile;
"""
GGUF_JS = r'C:\Ollama\ollamalegion\webui\js\modules\gguf-renderer.js'

with open(GGUF_JS, 'r', encoding='utf-8', newline='') as f:
    text = f.read()

had_crlf = '\r\n' in text
text = text.replace('\r\n', '\n').replace('\r', '\n')
lines = text.split('\n')

ORIG = len(lines)
print(f"Original line count (LF-normalized): {ORIG}")

# Find the Actions section
actions_start = None
for i, line in enumerate(lines):
    if line.strip() == '// ---- Actions ----':
        actions_start = i
        break
assert actions_start is not None, "Could not find // ---- Actions ----"

# Find end of Actions section — next // ---- section
actions_end = None
for i in range(actions_start + 1, len(lines)):
    if lines[i].strip().startswith('// ---- ') and 'Actions' not in lines[i]:
        actions_end = i
        break
assert actions_end is not None, "Could not find end of Actions section"
print(f"Actions section: L{actions_start+1}-L{actions_end} ({actions_end - actions_start} lines)")

# Delete the Actions section
del lines[actions_start:actions_end]

# Add aliases for action functions after existing state aliases
alias_anchor = None
for i, line in enumerate(lines):
    if line.strip() == 'var renderBackendsList = M.renderBackendsList;':
        alias_anchor = i + 1
        break
assert alias_anchor is not None, "Could not find renderBackendsList alias"

action_aliases = [
    '',
    '    // Action handlers (from gguf-renderer-actions.js loaded BEFORE this file).',
    '    // Critical: onDetailPanelClick handler (in gguf-renderer.js) uses these',
    '    // via onclick="ui.X(...)" — must remain available as bare identifiers.',
    '    var selectBackend = M.selectBackend;',
    '    var currentBackend = M.currentBackend;',
    '    var loadOnSelectedBackend = M.loadOnSelectedBackend;',
    '    var markLoadingModel = M.markLoadingModel;',
    '    var markLoadFailed = M.markLoadFailed;',
    '    var unloadOnSelectedBackend = M.unloadOnSelectedBackend;',
    '    var deleteOnSelectedBackend = M.deleteOnSelectedBackend;',
    '    var cancelActiveGeneration = M.cancelActiveGeneration;',
    '    var doHfSearch = M.doHfSearch;',
    '    var viewHfFiles = M.viewHfFiles;',
    '    var startDownload = M.startDownload;',
    '    var cancelDownload = M.cancelDownload;',
    '    var deleteDownloadedFile = M.deleteDownloadedFile;',
    '',
]
lines[alias_anchor:alias_anchor] = action_aliases

# Write back
output = '\n'.join(lines)
if had_crlf:
    output = output.replace('\n', '\r\n')

with open(GGUF_JS, 'w', encoding='utf-8', newline='') as f:
    f.write(output)

print(f'Resulting gguf-renderer.js: {len(lines)} lines (was {ORIG}, removed {ORIG - len(lines)})')

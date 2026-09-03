"""
r57_5f_extract_detail_render.py — R57.5f: extract right panel render functions
(including 4 sub-panes + backend options load/save) from gguf-renderer.js
to gguf-renderer-detail-render.js.

Section extracted:
  - // ---- Right panel: detail ---- (L143) through
    end of mountProfilesInSettings (L1460)
  - ~1320 строк

This is the BIG extraction. After R57.5f, gguf-renderer.js becomes
a thin orchestrator (~150 строк).

Strategy:
1. Read the right panel section
2. Wrap in IIFE
3. Add `const state = M.state` alias at top so existing `state.X` references work
4. Add `const _ = M._, stripGGUF = M.stripGGUF, formatFileSize = M.formatFileSize, showToast = M.showToast`
5. Add `const renderBackendsPanel = M.renderBackendsPanel, renderBackendsList = M.renderBackendsList`
6. Add `const currentBackend = M.currentBackend, ...` (all action/refresh aliases)
7. Add M.X references where needed (Renderers.*, etc.)
"""
GGUF_JS = r'C:\Ollama\ollamalegion\webui\js\modules\gguf-renderer.js'

with open(GGUF_JS, 'r', encoding='utf-8', newline='') as f:
    text = f.read()

had_crlf = '\r\n' in text
text = text.replace('\r\n', '\n').replace('\r', '\n')
lines = text.split('\n')

ORIG = len(lines)
print(f"Original line count (LF-normalized): {ORIG}")

# Find the // ---- Right panel: detail ---- section start
right_start = None
for i, line in enumerate(lines):
    if line.strip() == '// ---- Right panel: detail ----':
        right_start = i
        break
assert right_start is not None, "Could not find // ---- Right panel: detail ----"
print(f"Right panel starts at L{right_start+1}")

# Find end: // ---- Public API ----
right_end = None
for i in range(right_start + 1, len(lines)):
    if lines[i].strip() == '// ---- Public API ----':
        right_end = i
        break
assert right_end is not None, "Could not find // ---- Public API ----"
print(f"Right panel ends at L{right_end} ({right_end - right_start} lines)")

# Extract the section
section = lines[right_start:right_end]

# Wrap in IIFE
wrapped = ['(function() {', '    \'use strict\';'] + section + ['})();']

# Write to a temp file (will be reviewed and then committed as the new file)
DETAIL_RENDER_JS = r'C:\Ollama\ollamalegion\webui\js\modules\gguf-renderer-detail-render.js'

header = '''/**
 * gguf-renderer-detail-render.js — Right panel render functions for gguf-renderer.
 *
 * R57.5f (2026-09-03): extracted from webui/js/modules/gguf-renderer.js.
 *
 * Содержит рендеринг ПРАВОЙ ПАНЕЛИ (master-detail layout):
 *   - renderDetailPanel — главный dispatcher
 *   - renderEmptyDetail — empty state
 *   - renderDetailHeader — header с status + URL
 *   - renderDetailTabs — табы (about / models / downloads / hf / settings)
 *   - renderDetailPane — dispatcher по detailPane state
 *   - renderAboutPane — таб About
 *   - renderModelsPane — таб Models
 *   - renderLoadedPane — loaded models cards
 *   - renderDownloadsPane — downloads progress
 *   - renderHuggingFacePane — HF search results
 *   - renderSettingsPane — settings tab
 *   - Backend options load/save (loadAndRenderBackendOptions, saveBackendOptions)
 *   - mountProfilesInSettings — Per-Model Profiles mounting
 *
 * Зависимости (должны быть загружены раньше):
 *   - window.GgufModule.state (from gguf-renderer-state.js)
 *   - window.GgufModule.* (all helpers / actions / refresh / list / detail functions)
 *   - window.Renderers (statusBadges, etc.)
 *   - window.Utils (escapeHtml)
 *   - window.I18N.t
 *
 * Загружается ДО gguf-renderer.js (в defer-цепочке).
 */
(function() {
    \'use strict\';

    const M = (window.GgufModule = window.GgufModule || {});

    // Local aliases для коротких ссылок внутри IIFE
    const state = M.state;
    const _ = M._;
    const stripGGUF = M.stripGGUF;
    const formatFileSize = M.formatFileSize;
    const showToast = M.showToast;

'''

footer = '''
})();
'''

content = header + '\n'.join(wrapped) + '\n' + footer

with open(DETAIL_RENDER_JS, 'w', encoding='utf-8', newline='') as f:
    f.write(content)

# Delete the section from gguf-renderer.js
del lines[right_start:right_end]

# Add aliases for the extracted functions (we need to know their names)
# The functions are: renderDetailPanel, renderEmptyDetail, renderDetailHeader,
# renderDetailTabs, renderDetailPane, renderAboutPane, renderModelsPane,
# renderLoadedPane, renderDownloadsPane, renderHuggingFacePane, renderSettingsPane,
# loadAndRenderBackendOptions, mountProfilesInSettings
# Plus internal helpers (if any) — for now, just expose the public ones via aliases.

# Find existing alias anchor (after the detail aliases added in R57.5e)
alias_anchor = None
for i, line in enumerate(lines):
    if line.strip() == 'var bindSettingsChange = M.bindSettingsChange;':
        alias_anchor = i + 1
        break
assert alias_anchor is not None, "Could not find bindSettingsChange alias"

# But wait — the extracted file registers M.renderDetailPanel etc. The gguf-renderer.js
# main render() function still needs to call these. Currently main render() calls
# renderDetailPanel() — needs to become M.renderDetailPanel() or use a local alias.
# Add aliases in the IIFE so existing call sites work.

aliases_block = [
    '',
    '    // Right panel render functions (from gguf-renderer-detail-render.js).',
    '    var renderDetailPanel = M.renderDetailPanel;',
    '    var renderEmptyDetail = M.renderEmptyDetail;',
    '    var renderDetailHeader = M.renderDetailHeader;',
    '    var renderDetailTabs = M.renderDetailTabs;',
    '    var renderDetailPane = M.renderDetailPane;',
    '    var renderAboutPane = M.renderAboutPane;',
    '    var renderModelsPane = M.renderModelsPane;',
    '    var renderLoadedPane = M.renderLoadedPane;',
    '    var renderDownloadsPane = M.renderDownloadsPane;',
    '    var renderHuggingFacePane = M.renderHuggingFacePane;',
    '    var renderSettingsPane = M.renderSettingsPane;',
    '    var loadAndRenderBackendOptions = M.loadAndRenderBackendOptions;',
    '    var mountProfilesInSettings = M.mountProfilesInSettings;',
    '    var renderConnectModal = M.renderConnectModal;',
    '',
]
lines[alias_anchor:alias_anchor] = aliases_block

# Write back
output = '\n'.join(lines)
if had_crlf:
    output = output.replace('\n', '\r\n')

with open(GGUF_JS, 'w', encoding='utf-8', newline='') as f:
    f.write(output)

print(f'Resulting gguf-renderer.js: {len(lines)} lines (was {ORIG}, removed {ORIG - len(lines)})')
print(f'Created: {DETAIL_RENDER_JS}')

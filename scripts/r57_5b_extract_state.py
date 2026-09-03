"""
r57_5b_extract_state.py — R57.5b: extract state object + UNHEALTHY_STATUSES / isHealthyBackend /
visibleBackends from gguf-renderer.js to gguf-renderer-state.js.

Section extracted:
  - State object (L17-79 in original): 25+ поля
  - UNHEALTHY_STATUSES + isHealthyBackend + visibleBackends (L81-91)

After R57.5b:
  - gguf-renderer.js: -90 строк (state + helpers, +2 alias строки)
  - gguf-renderer-state.js: NEW (5.8KB, 25+ полей + 3 helpers)

Changes to gguf-renderer.js:
  1. Remove state object declaration (заменено на alias).
  2. Remove UNHEALTHY_STATUSES, isHealthyBackend, visibleBackends.
  3. Add `var state = M.state; var isHealthyBackend = M.isHealthyBackend; var visibleBackends = M.visibleBackends;`
     — existing code (80+ refs) продолжает работать через alias.
"""
GGUF_JS = r'C:\Ollama\ollamalegion\webui\js\modules\gguf-renderer.js'

with open(GGUF_JS, 'r', encoding='utf-8', newline='') as f:
    text = f.read()

had_crlf = '\r\n' in text
text = text.replace('\r\n', '\n').replace('\r', '\n')
lines = text.split('\n')

ORIG = len(lines)
print(f"Original line count (LF-normalized): {ORIG}")

# Step 1: find and remove the state object block.
# State starts at "// ---- State ----" and ends at the closing `};` followed by blank line
# before "// Статусы, которые считаем..." comment.
# Range: L17 (after R57.5a, the // ---- State ----) to L79 (closing };)
# We need to find by content, not line numbers, since previous commits shifted them.

state_start = None
state_end = None  # exclusive — index of first line after state
for i, line in enumerate(lines):
    if line.strip() == '// ---- State ----':
        state_start = i
        break
assert state_start is not None, "Could not find // ---- State ----"

# Find the closing `};` of the state object
depth = 0
for i in range(state_start, len(lines)):
    line = lines[i]
    # Count braces in the line (simple — state uses `{` and `}` only for the object)
    for ch in line:
        if ch == '{':
            depth += 1
        elif ch == '}':
            depth -= 1
            if depth == 0 and '}' in line:
                # closing line of the state object
                state_end = i + 1  # exclusive
                break
    if state_end is not None:
        break
assert state_end is not None, "Could not find closing of state object"
print(f"State block: L{state_start+1}-L{state_end}")

# Step 2: find and remove UNHEALTHY_STATUSES + isHealthyBackend + visibleBackends
# These are 3 functions between state close and `_refreshInProgress`.
# Range: 3 functions (UNHEALTHY_STATUSES const, isHealthyBackend, visibleBackends).
# Original lines 81-91 (in pre-R57.5a), but in current state, they should be
# at lines state_end+1 to roughly state_end+15.
helpers_start = None
helpers_end = None
for i in range(state_end, min(state_end + 20, len(lines))):
    if 'var UNHEALTHY_STATUSES' in lines[i]:
        helpers_start = i
        break
assert helpers_start is not None, "Could not find UNHEALTHY_STATUSES"

# Find end of visibleBackends function (the closing `}` of `return ...filter(...)`)
for i in range(helpers_start, min(helpers_start + 20, len(lines))):
    if lines[i].strip() == '}' and i > helpers_start + 2:  # closing of visibleBackends
        helpers_end = i + 1
        break
assert helpers_end is not None, "Could not find end of visibleBackends"
print(f"UNHEALTHY_STATUSES + isHealthyBackend + visibleBackends: L{helpers_start+1}-L{helpers_end}")

# Delete in REVERSE order (highest first)
del lines[helpers_start:helpers_end]
del lines[state_start:state_end]

# Step 3: add aliases after M alias block.
# Find where M alias is set (right after IIFE opening in R57.5a)
m_alias_end = None
for i, line in enumerate(lines):
    if line.strip().startswith('var stripGGUF = M.stripGGUF'):
        m_alias_end = i + 1
        break
assert m_alias_end is not None, "Could not find stripGGUF alias"

# Insert state-related aliases after the existing aliases
state_aliases = [
    '    // State and helpers (from gguf-renderer-state.js loaded BEFORE this file).',
    '    // 80+ call sites in this file use bare `state` and `isHealthyBackend(b)` — these',
    '    // aliases preserve backward compat without renaming.',
    '    var state = M.state;',
    '    var isHealthyBackend = M.isHealthyBackend;',
    '    var visibleBackends = M.visibleBackends;',
    '',
]
lines[m_alias_end:m_alias_end] = state_aliases

# Write back
output = '\n'.join(lines)
if had_crlf:
    output = output.replace('\n', '\r\n')

with open(GGUF_JS, 'w', encoding='utf-8', newline='') as f:
    f.write(output)

print(f'Resulting gguf-renderer.js: {len(lines)} lines (was {ORIG}, removed {ORIG - len(lines)})')

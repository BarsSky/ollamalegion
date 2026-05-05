#!/usr/bin/env python3
"""Remove duplicate /api/v1/backends/ route and add reconfigure subpath to backendHandler."""
import os

f = os.path.join(os.path.dirname(__file__), '..', 'internal', 'api', 'handlers.go')
with open(f, 'r', encoding='utf-8') as fh:
    c = fh.read()

# 1. Remove duplicate reconfigure route line
dup = '\t// Reconfigure endpoint (применение конфигурации запуска к бэкенду)\n\ts.mux.Handle("/api/v1/backends/", AuthMiddleware(RateLimitMiddleware(s.reconfigureHandler, s.rateLimiter), s.authenticator))\n\n\t// Restart endpoint'
if dup in c:
    c = c.replace(dup, '\t// Restart endpoint')
    print('[OK] Duplicate route removed')
else:
    print('[FAIL] Duplicate route not found - checking for variation')
    if 'reconfigureHandler' in c:
        for i, line in enumerate(c.split('\n')):
            if 'reconfigureHandler' in line:
                print(f'  Line {i+1}: {repr(line)}')

# 2. Add reconfigure subpath in backendHandler (after capacity subpath check)
old_cap = """\t// Подпуть /capacity
\tif len(parts) > 1 && parts[1] == "capacity" {
\t\tif r.Method == http.MethodGet {
\t\t\ts.backendCapacity(w, r, backendID)
\t\t\treturn
\t\t}
\t\thttp.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
\t\treturn
\t}

\tswitch r.Method {"""

new_cap = """\t// Подпуть /capacity
\tif len(parts) > 1 && parts[1] == "capacity" {
\t\tif r.Method == http.MethodGet {
\t\t\ts.backendCapacity(w, r, backendID)
\t\t\treturn
\t\t}
\t\thttp.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
\t\treturn
\t}

\t// Подпуть /reconfigure
\tif len(parts) > 1 && parts[1] == "reconfigure" {
\t\ts.reconfigureHandler(w, r)
\t\treturn
\t}

\t// Подпуть /launch-config
\tif len(parts) > 1 && parts[1] == "launch-config" {
\t\ts.backendLaunchConfigHandler(w, r)
\t\treturn
\t}

\tswitch r.Method {"""

if old_cap in c:
    c = c.replace(old_cap, new_cap)
    print('[OK] reconfigure + launch-config subpaths added to backendHandler')
else:
    print('[FAIL] capacity subpath block not matched')

with open(f, 'w', encoding='utf-8') as fh:
    fh.write(c)
print('Done')
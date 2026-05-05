#!/usr/bin/env python3
"""Add monitor page with iframe to index.html, and fix WEBUI_CONFIG."""
import os

fpath = os.path.join(os.path.dirname(__file__), '..', 'webui', 'index.html')
with open(fpath, 'r', encoding='utf-8') as f:
    content = f.read()

# Add monitor page before Dashboard page
old_dash = '            <!-- Dashboard Page -->\n            <div class="page active" id="dashboard-page">'
new_dash = '''            <!-- Monitor Page (embedded via iframe) -->
            <div class="page" id="monitor-page">
                <iframe id="monitorFrame" src="monitor.html" style="width:100%;height:calc(100vh - 140px);border:none;background:var(--bg-primary)" allow="clipboard-write"></iframe>
            </div>

            <!-- Dashboard Page -->
            <div class="page active" id="dashboard-page">'''

if old_dash in content:
    content = content.replace(old_dash, new_dash)
    print('[OK] Monitor iframe page added to index.html')
else:
    print('[FAIL] Dashboard page section not found')

with open(fpath, 'w', encoding='utf-8') as f:
    f.write(content)

print('Done')
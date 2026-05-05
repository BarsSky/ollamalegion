#!/usr/bin/env python3
"""Patch types.go: add StatusDraining constant."""
import os

TYPES_FILE = os.path.join(os.path.dirname(__file__), '..', 'pkg', 'types', 'types.go')

with open(TYPES_FILE, 'r', encoding='utf-8') as f:
    content = f.read()

old_block = '''const (
	StatusHealthy   BackendStatus = "healthy"
	StatusUnhealthy BackendStatus = "unhealthy"
	StatusOffline   BackendStatus = "offline"
	StatusStarting  BackendStatus = "starting"
)'''

new_block = '''const (
	StatusHealthy   BackendStatus = "healthy"
	StatusUnhealthy BackendStatus = "unhealthy"
	StatusOffline   BackendStatus = "offline"
	StatusStarting  BackendStatus = "starting"
	StatusDraining  BackendStatus = "draining"
)'''

if old_block in content:
    content = content.replace(old_block, new_block)
    with open(TYPES_FILE, 'w', encoding='utf-8') as f:
        f.write(content)
    print('PATCHED: StatusDraining added to types.go')
else:
    print('NOT FOUND: trying alternative match...')
    # Try to find just the constant block
    for i, line in enumerate(content.split('\n')):
        if 'StatusHealthy' in line and 'BackendStatus' in line:
            start = max(0, i-1)
            end = min(len(content.split('\n')), i+5)
            print(f'\nContext around line {i+1}:')
            for j in range(start, end):
                print(f'{j+1}: {repr(content.split(chr(10))[j])}')
            break
"""Fix CRLF -> LF in env files (Windows compatibility fix)."""
import os
import sys

target = sys.argv[1] if len(sys.argv) > 1 else 'deployments/.env'

if not os.path.exists(target):
    print(f'File not found: {target}')
    sys.exit(1)

with open(target, 'rb') as f:
    data = f.read()

has_crlf = b'\r\n' in data
if has_crlf:
    with open(target, 'wb') as f:
        f.write(data.replace(b'\r\n', b'\n'))
    print(f'CRLF -> LF: {target} (converted {data.count(b"\r\n")} line endings)')
else:
    print(f'Already LF: {target} (no changes)')
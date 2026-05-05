#!/usr/bin/env python3
"""Patch proxy.go: add HTTP 502 response when backend is unreachable."""
import os

PROXY_FILE = os.path.join(os.path.dirname(__file__), '..', 'internal', 'balancer', 'proxy.go')

with open(PROXY_FILE, 'r', encoding='utf-8') as f:
    content = f.read()

old_block = '''\tresp, err := client.Do(req)
\tif err != nil {
\t\tp.logStreamingError(backendID, err)
\t\treturn fmt.Errorf("backend error: %v", err)
\t}
\tdefer resp.Body.Close()'''

new_block = '''\tresp, err := client.Do(req)
\tif err != nil {
\t\tp.logStreamingError(backendID, err)
\t\tif p.isStreamingRequest(r) {
\t\t\tw.Header().Set("Content-Type", "application/json")
\t\t\tw.Header().Set("Connection", "close")
\t\t\tw.WriteHeader(http.StatusBadGateway)
\t\t\tw.Write([]byte(`{"error":"Backend unreachable"}`))
\t\t\tif fl, ok := w.(http.Flusher); ok {
\t\t\t\tfl.Flush()
\t\t\t}
\t\t} else {
\t\t\thttp.Error(w, "Backend unreachable", http.StatusBadGateway)
\t\t}
\t\treturn fmt.Errorf("backend error: %v", err)
\t}
\tdefer resp.Body.Close()'''

if old_block in content:
    content = content.replace(old_block, new_block)
    with open(PROXY_FILE, 'w', encoding='utf-8') as f:
        f.write(content)
    print('PATCHED: proxy.go updated successfully')
else:
    print('NOT FOUND: old block not matched in proxy.go')
    # Debug: show surrounding context
    for i, line in enumerate(content.split('\n')):
        if 'client.Do(req)' in line:
            start = max(0, i-3)
            end = min(len(content.split('\n')), i+5)
            print(f'\nContext around line {i+1}:')
            for j in range(start, end):
                marker = '>>>' if j == i else '   '
                print(f'{marker} {j+1}: {repr(content.split(chr(10))[j])}')
            break
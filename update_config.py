import urllib.request, json

# Update backendEngine via PUT /api/v1/cluster/config
data = json.dumps({"backendEngine": "llama_cpp"}).encode()
req = urllib.request.Request('http://localhost:18081/api/v1/cluster/config', data=data, method='PUT', headers={'Content-Type': 'application/json'})

try:
    resp = urllib.request.urlopen(req)
    print("UPDATE:", resp.read().decode()[:200])
except urllib.error.HTTPError as e:
    print(f"HTTP {e.code}: {e.read().decode()[:300]}")

# Verify
d = json.loads(urllib.request.urlopen('http://localhost:18081/api/v1/cluster').read())
print()
print("backendEngine:", d.get('backendEngine'))
print("effectiveBackendType:", d.get('effectiveBackendType'))
print("backendTypeCounts:", d.get('backendTypeCounts'))
print("totalBackends:", d.get('totalBackends'))
print()
print("Backends in response:")
for b in d.get('backends', []):
    print(f"  {b.get('id','?'):20s} | type: {b.get('backend_type','?')}")
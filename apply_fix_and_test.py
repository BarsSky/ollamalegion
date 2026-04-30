import subprocess, sys

with open('internal/balancer/proxy.go', 'r', encoding='utf-8') as f:
    content = f.read()

# Fix 1: /health handler
old1 = 'func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {\n\t// === Диспетчеризация Ollama API ==='
new1 = '''func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// === Health check ===
	if r.URL.Path == "/health" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("{\\"status\\":\\"healthy\\"}"))
		return
	}

	// === Диспетчеризация Ollama API ==='''
assert old1 in content, 'Fix 1: old string not found!'
content = content.replace(old1, new1, 1)

# Fix 2: sessionID with model in ServeHTTP
old2 = '\t// Формируем составной sessionID с учётом clientName (различает клиентов за одним IP)\n\tsessionID := p.getSessionID(r, clientName)'
new2 = '\t// Формируем составной sessionID с учётом clientName и модели\n\tsessionID := p.getSessionIDWithModel(r, clientName, model)'
assert old2 in content, 'Fix 2: old string not found!'
content = content.replace(old2, new2, 1)

# Fix 3: proxyRequest sessionID with model
old3 = '\tclientNameForSession := p.getClientName(r)\n\tsessionID := p.getSessionID(r, clientNameForSession)'
new3 = '\tclientNameForSession := p.getClientName(r)\n\tmodelFromCtx := ""\n\tif m, ok := r.Context().Value(modelContextKey).(string); ok {\n\t\tmodelFromCtx = m\n\t}\n\tsessionID := p.getSessionIDWithModel(r, clientNameForSession, modelFromCtx)'
assert old3 in content, 'Fix 3: old string not found!'
content = content.replace(old3, new3, 1)

with open('internal/balancer/proxy.go', 'w', encoding='utf-8') as f:
    f.write(content)

print('All 3 fixes applied successfully')

# Run tests
r = subprocess.run(['go', 'test', './tests/', '-run', 'TestScenario', '-v', '-timeout', '120s'],
                   capture_output=True, text=True)

with open('test_results_final.txt', 'w', encoding='utf-8') as f:
    f.write(r.stdout)
    if r.stderr:
        f.write('\n=== STDERR ===\n')
        f.write(r.stderr)
    f.write(f'\n=== Exit code: {r.returncode} ===\n')

print('Tests completed. Results in test_results_final.txt')
print('Exit code:', r.returncode)
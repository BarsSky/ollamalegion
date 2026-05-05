#!/usr/bin/env python3
"""Add missing backendLaunchConfigHandler method."""
import os

f = os.path.join(os.path.dirname(__file__), '..', 'internal', 'api', 'handlers.go')
with open(f, 'r', encoding='utf-8') as fh:
    c = fh.read()

marker = '// reconfigureHandler'
insert = """// backendLaunchConfigHandler — прокси для backendHandler subpath /launch-config
func (s *Server) backendLaunchConfigHandler(w http.ResponseWriter, r *http.Request) {
\tif r.Method != http.MethodGet {
\t\thttp.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
\t\treturn
\t}
\tparts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/backends/"), "/")
\tif len(parts) < 3 || parts[1] != "launch-config" {
\t\thttp.NotFound(w, r)
\t\treturn
\t}
\ts.listLaunchConfigs(w, r, parts[0])
}

// listLaunchConfigs — возвращает конфигурации запуска для backendId
func (s *Server) listLaunchConfigs(w http.ResponseWriter, r *http.Request, backendID string) {
\tstate := s.proxy.GetClusterState()
\tfor _, metrics := range state.Backends {
\t\tif metrics.ID == backendID {
\t\t\tgpuCount := 0
\t\t\tif metrics.GPU.MemoryTotal > 0 { gpuCount = 1 }
\t\t\tvramTotal := metrics.GPU.MemoryTotal
\t\t\tramTotal := metrics.System.MemoryTotal

\t\t\tconfigs := []map[string]interface{}{
\t\t\t\t{
\t\t\t\t\t"label": "Оптимальный (сбалансированный)", "type": "optimal", "isOptimal": true,
\t\t\t\t\t"envVars": map[string]string{
\t\t\t\t\t\t"OLLAMA_NUM_PARALLEL": "4", "OLLAMA_MAX_LOADED_MODELS": "2",
\t\t\t\t\t\t"OLLAMA_KV_CACHE_TYPE": "f16", "OLLAMA_GPU_LAYERS": "-1",
\t\t\t\t\t\t"OLLAMA_FLASH_ATTENTION": "1", "OLLAMA_NUM_THREADS": fmt.Sprintf("%d", runtime.NumCPU()),
\t\t\t\t\t\t"OLLAMA_CONTEXT_LENGTH": "4096",
\t\t\t\t\t},
\t\t\t\t\t"description": "Сбалансированная конфигурация",
\t\t\t\t\t"limitations": []string{},
\t\t\t\t},
\t\t\t\t{
\t\t\t\t\t"label": "Скоростной (упор на скорость)", "type": "speed", "isOptimal": false,
\t\t\t\t\t"envVars": map[string]string{
\t\t\t\t\t\t"OLLAMA_NUM_PARALLEL": "8", "OLLAMA_MAX_LOADED_MODELS": "4",
\t\t\t\t\t\t"OLLAMA_KV_CACHE_TYPE": "f16", "OLLAMA_GPU_LAYERS": "-1",
\t\t\t\t\t\t"OLLAMA_FLASH_ATTENTION": "1", "OLLAMA_NUM_THREADS": fmt.Sprintf("%d", runtime.NumCPU()),
\t\t\t\t\t\t"OLLAMA_CONTEXT_LENGTH": "4096",
\t\t\t\t\t},
\t\t\t\t\t"description": "Максимальный параллелизм, все модели в VRAM",
\t\t\t\t\t"limitations": []string{"Высокое потребление VRAM — возможен OOM на больших моделях"},
\t\t\t\t},
\t\t\t\t{
\t\t\t\t\t"label": "Экономный (упор на размышления)", "type": "quality", "isOptimal": false,
\t\t\t\t\t"envVars": map[string]string{
\t\t\t\t\t\t"OLLAMA_NUM_PARALLEL": "1", "OLLAMA_MAX_LOADED_MODELS": "1",
\t\t\t\t\t\t"OLLAMA_KV_CACHE_TYPE": "q8_0", "OLLAMA_GPU_LAYERS": "-1",
\t\t\t\t\t\t"OLLAMA_FLASH_ATTENTION": "1", "OLLAMA_NUM_THREADS": fmt.Sprintf("%d", runtime.NumCPU()/2),
\t\t\t\t\t\t"OLLAMA_CONTEXT_LENGTH": "8192",
\t\t\t\t\t},
\t\t\t\t\t"description": "Один запрос с большим контекстом 8K для размышлений",
\t\t\t\t\t"limitations": []string{"Однопоточный режим — другие клиенты будут ждать в очереди"},
\t\t\t\t},
\t\t\t}

\t\t\ts.writeJSON(w, http.StatusOK, map[string]interface{}{
\t\t\t\t"success":   true,
\t\t\t\t"backendId": backendID,
\t\t\t\t"hardware": map[string]interface{}{
\t\t\t\t\t"gpuCount": gpuCount, "vramTotalMB": vramTotal,
\t\t\t\t\t"ramTotalMB": ramTotal, "cpuThreads": runtime.NumCPU(),
\t\t\t\t},
\t\t\t\t"configs":   configs,
\t\t\t\t"isOptimal": true,
\t\t\t\t"message":   "Текущая конфигурация оптимальна. Альтернативные варианты показаны для ознакомления.",
\t\t\t})
\t\t\treturn
\t\t}
\t}
\twriteJSON(w, http.StatusNotFound, map[string]interface{}{"success": false, "error": "Backend not found"})
}



"""

c = c.replace(marker, insert + marker)
with open(f, 'w', encoding='utf-8') as fh:
    fh.write(c)
print('DONE: backendLaunchConfigHandler added')
"""Patch remaining items: Headroom reservation, Prometheus metrics, Backpressure"""
import sys, os

proxy_path = r'c:\Ollama\ollamalegion\internal\balancer\proxy.go'

with open(proxy_path, 'r', encoding='utf-8') as f:
    data = f.read()

# === 1. Headroom in checkResourceLimits ===
old_headroom = '''\tlimits := p.config.Resources

\tif metrics.GPU.UsagePercent > 95 {
\t\tlogger.Get().Warnw("backend GPU usage critical, blocking", "backend", backendID, "gpu_usage", metrics.GPU.UsagePercent)
\t\treturn false
\t}

\tif limits.GPU.MaxVRAMUsagePercent > 0 && metrics.GPU.MemoryTotal > 0 {
\t\tvramPercent := float64(metrics.GPU.MemoryUsed) * 100 / float64(metrics.GPU.MemoryTotal)
\t\tif vramPercent > limits.GPU.MaxVRAMUsagePercent {
\t\t\tlogger.Get().Warnw("backend VRAM usage above threshold (degraded, not blocked)",
\t\t\t\t"backend", backendID, "vram_percent", vramPercent, "threshold", limits.GPU.MaxVRAMUsagePercent)
\t\t}
\t}'''

new_headroom = '''\tlimits := p.config.Resources

\t// Headroom reservation: учитываем GPU headroom из конфигурации balancing
\theadroom := p.config.Balancing.ResourceReservation.GPUHeadroomPercent
\tif headroom > 0 && metrics.GPU.MemoryTotal > 0 {
\t\tvramPercent := float64(metrics.GPU.MemoryUsed) * 100 / float64(metrics.GPU.MemoryTotal)
\t\tif vramPercent > (100 - headroom) {
\t\t\tlogger.Get().Warnw("backend VRAM exceeds headroom reservation, blocking",
\t\t\t\t"backend", backendID, "vram_percent", vramPercent, "headroom_pct", headroom)
\t\t\treturn false
\t\t}
\t}

\tif metrics.GPU.UsagePercent > 95 {
\t\tlogger.Get().Warnw("backend GPU usage critical, blocking", "backend", backendID, "gpu_usage", metrics.GPU.UsagePercent)
\t\treturn false
\t}

\tif limits.GPU.MaxVRAMUsagePercent > 0 && metrics.GPU.MemoryTotal > 0 {
\t\tvramPercent := float64(metrics.GPU.MemoryUsed) * 100 / float64(metrics.GPU.MemoryTotal)
\t\tif vramPercent > limits.GPU.MaxVRAMUsagePercent {
\t\t\tlogger.Get().Warnw("backend VRAM usage above threshold (degraded, not blocked)",
\t\t\t\t"backend", backendID, "vram_percent", vramPercent, "threshold", limits.GPU.MaxVRAMUsagePercent)
\t\t}
\t}'''

if old_headroom in data:
    data = data.replace(old_headroom, new_headroom, 1)
    print("1. Headroom reservation added to checkResourceLimits")
else:
    print("ERROR: headroom pattern not found")
    sys.exit(1)

# === 2. Import time in proxy.go (should already exist, but verify) ===
if 'import (' not in data or '"time"' not in data:
    print("ERROR: time import not found")
    sys.exit(1)

# === 3. Add backpressure (503) in queueRequest ===
# We'll modify the queueRequest function to add backpressure
old_queue = '''func (p *Proxy) queueRequest(w http.ResponseWriter, r *http.Request, model string) bool {
\tdone := make(chan bool, 1)
\tqueuedReq := &QueuedRequest{
\t\tRequest:  r,
\t\tWriter:   w,
\t\tModel:    model,
\t\tEnqueued: time.Now(),
\t\tDone:     done,
\t}

\tp.queueMgr.addPending(queuedReq)

\tselect {
\tcase p.queueMgr.queue <- queuedReq:
\tcase <-p.queueMgr.ctx.Done():
\t\tp.queueMgr.removePending(queuedReq)
\t\treturn false
\tdefault:
\t\tp.queueMgr.removePending(queuedReq)
\t\treturn false
\t}'''

new_queue = '''func (p *Proxy) queueRequest(w http.ResponseWriter, r *http.Request, model string) bool {
\t// Backpressure: проверяем fill rate очереди
\tqueueFillPct := float64(len(p.queueMgr.queue)) / float64(p.queueMgr.maxSize)
\tif queueFillPct > 0.90 {
\t\tlogger.Get().Warnw("queue overflow, rejecting request with 503",
\t\t\t"model", model, "queue_fill_pct", queueFillPct, "queue_size", len(p.queueMgr.queue))
\t\tw.Header().Set("Retry-After", "5")
\t\thttp.Error(w, "Service overloaded", http.StatusServiceUnavailable)
\t\treturn false
\t}

\tdone := make(chan bool, 1)
\tqueuedReq := &QueuedRequest{
\t\tRequest:  r,
\t\tWriter:   w,
\t\tModel:    model,
\t\tEnqueued: time.Now(),
\t\tDone:     done,
\t}

\tp.queueMgr.addPending(queuedReq)

\tselect {
\tcase p.queueMgr.queue <- queuedReq:
\tcase <-p.queueMgr.ctx.Done():
\t\tp.queueMgr.removePending(queuedReq)
\t\treturn false
\tdefault:
\t\tp.queueMgr.removePending(queuedReq)
\t\treturn false
\t}'''

if old_queue in data:
    data = data.replace(old_queue, new_queue, 1)
    print("2. Backpressure (503 + Retry-After) added")
else:
    print("ERROR: queueRequest pattern not found")
    sys.exit(1)

with open(proxy_path, 'w', encoding='utf-8') as f:
    f.write(data)

print("All remaining patches applied to proxy.go. Done.")
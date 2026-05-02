# OllamaLegion Troubleshooting Guide

Common issues and their solutions.

## Table of Contents

1. [Connection Issues](#connection-issues)
2. [Backend Problems](#backend-problems)
3. [Performance Issues](#performance-issues)
4. [Docker Issues](#docker-issues)
5. [WebUI Issues](#webui-issues)

---

## Connection Issues

### Cannot connect to load balancer

**Symptoms:** `curl http://localhost:18081/api/v1/health` fails

**Solutions:**
1. Verify the balancer is running:
   ```bash
   docker ps | grep loadbalancer
   # or
   ps aux | grep ollama-balancer
   ```
2. Check logs:
   ```bash
   docker logs ollamalegion_loadbalancer_1
   ```
3. Verify port is not in use:
   ```bash
   netstat -tuln | grep -E '18080|18081'
   ```
4. Check firewall rules:
   ```bash
   sudo ufw status  # Ubuntu
   sudo firewall-cmd --list-all  # CentOS/RHEL
   ```

### CORS errors in WebUI

**Symptoms:** Browser console shows CORS errors

**Solutions:**
1. Access via nginx proxy (not directly as file://)
2. Use Docker Compose which includes nginx reverse proxy
3. If accessing directly, add API base:
   ```
   http://localhost:18080/?api_base=http://localhost:18081
   ```

---

## Backend Problems

### Backend shows as unhealthy

**Symptoms:** Backend status is "unhealthy" in WebUI

**Solutions:**
1. Verify Ollama is running on the backend:
   ```bash
   curl http://backend-ip:11434/api/tags
   ```
2. Check if agent is running:
   ```bash
   curl http://backend-ip:18032/metrics
   ```
3. Verify network connectivity:
   ```bash
   ping backend-ip
   telnet backend-ip 11434
   ```
4. Check backend logs:
   ```bash
   docker logs ollama-agent-1
   ```

### Models not appearing in list

**Symptoms:** Models tab shows no models or incomplete list

**Solutions:**
1. Verify models are loaded on backends:
   ```bash
   curl http://backend-ip:11434/api/tags
   ```
2. Restart backend registration:
   ```bash
   # Delete and re-add backend
   curl -X DELETE http://localhost:18081/api/v1/backends/backend-id
   curl -X POST http://localhost:18081/api/v1/backends -d '{"id":"...","host":"..."}'
   ```

---

## Performance Issues

### High queue depth

**Symptoms:** Many requests waiting in queue

**Solutions:**
1. Check if backends are overloaded:
   ```bash
   curl http://localhost:18081/api/v1/cluster | jq '.backends[].activeRequests'
   ```
2. Force rebalance:
   ```bash
   curl -X POST http://localhost:18081/api/v1/queue/rebalance
   ```
3. Add more backends
4. Increase `maxConcurrentRequests` on backends:
   ```bash
   curl -X PUT http://localhost:18081/api/v1/backends/backend-id \
     -d '{"maxConcurrentRequests": 16}'
   ```

### Slow response times

**Symptoms:** Requests take unusually long

**Solutions:**
1. Check GPU metrics:
   ```bash
   curl http://localhost:18081/api/v1/cluster | jq '.backends[].gpu'
   ```
2. Verify model is loaded (not swapping):
   ```bash
   curl http://backend-ip:11434/api/ps
   ```
3. Check disk I/O on backends
4. Reduce `maxConcurrentRequests` if GPUs are overloaded

---

## Docker Issues

### Container won't start

**Symptoms:** `docker compose up` fails

**Solutions:**
1. Check port conflicts:
   ```bash
   netstat -tuln | grep -E '18080|18081|6379|5432'
   ```
2. Verify Docker Compose version (2.0+):
   ```bash
   docker compose version
   ```
3. Pull latest images:
   ```bash
   docker compose pull
   ```
4. Rebuild:
   ```bash
   docker compose build --no-cache
   ```

### GPU not available in container

**Symptoms:** Agent shows no GPU metrics

**Solutions:**
1. Verify NVIDIA Container Toolkit:
   ```bash
   nvidia-ctk --version
   ```
2. Test GPU access:
   ```bash
   docker run --rm --gpus all nvidia/cuda:12.0-base nvidia-smi
   ```
3. Check Docker GPU runtime:
   ```bash
   docker info | grep -i runtime
   ```
4. Add `--gpus all` or use `docker-compose.agent.gpu.yml`

---

## WebUI Issues

### Blank page or incomplete rendering

**Symptoms:** WebUI page is empty or partially loaded

**Solutions:**
1. Clear browser cache (Ctrl+Shift+R)
2. Check JavaScript console for errors (F12)
3. Verify API is accessible:
   ```bash
   curl http://localhost:18081/api/v1/health
   ```
4. Force refresh: add `?v=2` to URL

### Monitor page shows "No data"

**Symptoms:** Monitor tab displays no information

**Solutions:**
1. Wait for first data fetch (2-5 seconds)
2. Click "Retry" button
3. Enable demo mode to verify UI works:
   - Open browser console: `enterDemoMode()`
4. Check CORS configuration
5. Verify API base URL:
   - Open console: `API_BASE`
   - Set manually: `localStorage.setItem('monitorApiBase', 'http://localhost:18081')`

### Theme not applying correctly

**Symptoms:** Dark/light theme doesn't switch

**Solutions:**
1. Clear localStorage:
   ```javascript
   localStorage.removeItem('ollamalegion_theme')
   ```
2. Hard refresh (Ctrl+Shift+R)
3. Check if `localStorage` is enabled in browser

---

## Getting Help

1. **Check logs first:**
   ```bash
   docker compose logs loadbalancer
   docker compose logs agent
   ```

2. **Verify API health:**
   ```bash
   curl -v http://localhost:18081/api/v1/health
   ```

3. **Enable debug logging:**
   ```bash
   LOG_LEVEL=debug docker compose up
   ```

4. **Report issues:** [GitHub Issues](https://github.com/BarsSky/ollamalegion/issues)
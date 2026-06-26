# =============================================================================
# docker-compose.rpc.yml — RPC Worker B1 (stub-mode) тестовый стек.
#
# Запускает:
#   - rpc-coordinator  (healthcheck + admin endpoint)
#   - rpc-worker-1     (слои 1-16)
#   - rpc-worker-2     (слои 17-32)
#
# Использование (на хост-машине с Docker):
#   cd deployments
#   docker compose -f docker-compose.rpc.yml up -d --build
#   curl http://localhost:18080/rpc/health
#   curl http://localhost:18090/rpc/health
#   curl http://localhost:18180/rpc/health
#
# Остановка:
#   docker compose -f docker-compose.rpc.yml down
# =============================================================================

version: "3.8"

services:
  # ---------------------------------------------------------------------------
  # Coordinator (пока stub — каркас, без реального split/merge coordinator).
  # На B1 этот сервис — healthcheck-only; основная логика coordinator будет
  # в B2 (Management API) и B6 (Load Balancing между срезами).
  # ---------------------------------------------------------------------------
  rpc-coordinator:
    image: ollama-legion/rpcworker:0.1.0-stub
    container_name: rpc-coordinator
    command: ["/app/rpcworker", "-worker-id", "rpc-coordinator", "-port", "18080", "-log-level", "info"]
    ports:
      - "18080:18080"
    networks:
      - rpc-net
    healthcheck:
      test: ["CMD", "/app/rpcworker", "-healthcheck"]
      interval: 30s
      timeout: 5s
      retries: 3
      start_period: 5s
    restart: unless-stopped

  # ---------------------------------------------------------------------------
  # Worker 1 — обслуживает слои 1-16
  # ---------------------------------------------------------------------------
  rpc-worker-1:
    image: ollama-legion/rpcworker:0.1.0-stub
    container_name: rpc-worker-1
    command:
      - /app/rpcworker
      - -worker-id
      - rpc-worker-1
      - -layers
      - "1-16"
      - -port
      - "18090"
      - -coordinator-url
      - "http://rpc-coordinator:18080"
      - -log-level
      - "info"
    ports:
      - "18090:18090"
    networks:
      - rpc-net
    depends_on:
      rpc-coordinator:
        condition: service_healthy
    healthcheck:
      test: ["CMD", "/app/rpcworker", "-healthcheck"]
      interval: 30s
      timeout: 5s
      retries: 3
      start_period: 5s
    restart: unless-stopped

  # ---------------------------------------------------------------------------
  # Worker 2 — обслуживает слои 17-32
  # ---------------------------------------------------------------------------
  rpc-worker-2:
    image: ollama-legion/rpcworker:0.1.0-stub
    container_name: rpc-worker-2
    command:
      - /app/rpcworker
      - -worker-id
      - rpc-worker-2
      - -layers
      - "17-32"
      - -port
      - "18091"
      - -coordinator-url
      - "http://rpc-coordinator:18080"
      - -log-level
      - "info"
    ports:
      - "18091:18091"
    networks:
      - rpc-net
    depends_on:
      rpc-coordinator:
        condition: service_healthy
    healthcheck:
      test: ["CMD", "/app/rpcworker", "-healthcheck"]
      interval: 30s
      timeout: 5s
      retries: 3
      start_period: 5s
    restart: unless-stopped

networks:
  rpc-net:
    name: rpc-net
    driver: bridge
# Troubleshooting Ollama Load Balancer

Руководство по решению常见 проблем и отладке системы.

## Содержание

1. [Частые ошибки](#частые-ошибки)
2. [Логирование](#логирование)
3. [Отладка NVML](#отладка-nvml)
4. [Performance tuning](#performance-tuning)

---

## Частые ошибки

### Агент не подключается к балансировщику

**Симптомы:**
- Агент не появляется в списке бэкендов
- В логах агента ошибки подключения

**Решение:**

```bash
# 1. Проверка доступности балансировщика
curl -v http://<balancer-ip>:18081/api/v1/health

# 2. Проверка firewall правил
sudo ufw status
sudo iptables -L -n

# 3. Проверка логов агента
docker logs ollama-agent
# или
journalctl -u ollama-agent -f

# 4. Проверка BALANCER_URL
echo $BALANCER_URL
# Должен быть доступен из сети агента
```

**Возможные причины:**
- Неправильный URL балансировщика
- Firewall блокирует соединение
- Балансировщик не запущен
- Сетевые проблемы между серверами

---

### Бэкенд помечается как unhealthy

**Симптомы:**
- Бэкенд имеет статус `unhealthy` или `offline`
- Запросы не направляются на бэкенд

**Решение:**

```bash
# 1. Проверка Ollama API на бэкенде
curl http://<ollama-host>:11434/api/tags

# 2. Проверка доступности агента
curl http://<agent-host>:18032/metrics

# 3. Проверка логов агента
docker logs ollama-agent

# 4. Проверка health check интервала
# Убедитесь, что HEARTBEAT_INTERVAL < healthCheckInterval
```

**Возможные причины:**
- Ollama не запущен на бэкенде
- Агент не отправляет heartbeat
- Неправильные порты в конфигурации
- Сетевые проблемы

---

### Высокая задержка запросов

**Симптомы:**
- Запросы выполняются дольше обычного
- Таймауты запросов

**Решение:**

```bash
# 1. Проверка сетевой задержки
ping <balancer-ip>
ping <gpu-server-ip>

# 2. Проверка загрузки GPU серверов
curl http://<lb-ip>:18081/api/v1/metrics | jq

# 3. Проверка очереди запросов
curl http://<lb-ip>:18081/api/v1/cluster | jq .queuedRequests

# 4. Проверка статистики запросов
curl http://<lb-ip>:18081/api/v1/metrics | jq '.backends[].ollama.avgResponseTime'
```

**Возможные причины:**
- Высокая загрузка GPU серверов
- Сетевая задержка
- Неправильные лимиты ресурсов
- Переполненная очередь

---

### Ошибки доступа к GPU

**Симптомы:**
- Агент не получает GPU метрики
- В логах ошибки NVML/nvidia-smi

**Решение:**

```bash
# 1. Проверка NVIDIA Container Toolkit
docker run --rm --gpus all nvidia/cuda:11.0-base nvidia-smi

# 2. Проверка NVML внутри контейнера
docker exec ollama-agent nvidia-smi

# 3. Проверка прав доступа к устройствам
ls -la /dev/nvidia*

# 4. Пересоздание контейнера с правильными правами
docker rm -f ollama-agent
docker run -d \
  --name ollama-agent \
  --gpus all \
  --network host \
  -e AGENT_ID=gpu-1 \
  -e BALANCER_URL=http://<balancer-ip>:18081 \
  -e NVML_ENABLED=true \
  ollama-legion/agent:latest
```

**Возможные причины:**
- Не установлен NVIDIA Container Toolkit
- Неправильные volume mounts
- Отсутствуют права доступа к /dev/nvidia*
- NVML библиотека не найдена

---

### WebSocket не подключается

**Симптомы:**
- Ошибки подключения к `/ws/metrics`
- Real-time метрики не обновляются

**Решение:**

```bash
# 1. Проверка доступности порта
telnet <balancer-ip> 18081

# 2. Проверка WebSocket подключения
wscat -c ws://localhost:18081/ws/metrics

# 3. Проверка CORS настроек
curl -v -X OPTIONS http://localhost:18081/ws/metrics

# 4. Проверка логов балансировщика
docker logs loadbalancer | grep -i websocket
```

**Возможные причины:**
- Порт Management API недоступен
- Неправильные CORS настройки
- Firewall блокирует WebSocket
- Proxy (nginx) не настроен для WebSocket

---

### Очередь переполнена

**Симптомы:**
- Запросы отклоняются с 503 ошибкой
- В логах сообщения о переполненной очереди

**Решение:**

```bash
# 1. Увеличьте размер очереди
LB_QUEUE_MAX_SIZE=200

# 2. Увеличьте таймаут очереди
LB_QUEUE_TIMEOUT=600

# 3. Добавьте больше бэкендов
# или увеличьте maxConcurrentRequests

# 4. Проверка текущей очереди
curl http://<lb-ip>:18081/api/v1/cluster | jq .queuedRequests
```

**Возможные причины:**
- Недостаточно GPU серверов
- Слишком маленький `queueMaxSize`
- Слишком короткий `queueTimeout`
- Все бэкенды перегружены

---

### Аутентификация не работает

**Симптомы:**
- 401 Unauthorized ошибки
- Токены не принимаются

**Решение:**

```bash
# 1. Проверка статуса аутентификации
curl http://localhost:18081/api/v1/auth/status

# 2. Проверка заголовка токена
curl -v -X GET http://localhost:18081/api/v1/cluster \
  -H "X-API-Token: your-token"

# 3. Проверка имени заголовка
# По умолчанию: X-API-Token
# Можно изменить через AUTH_HEADER_NAME

# 4. Генерация нового токена (требуется master token)
curl -X POST http://localhost:18081/api/v1/auth/token \
  -H "X-API-Token: your-master-token"
```

**Возможные причины:**
- Неправильный токен
- Неправильное имя заголовка
- Аутентификация не включена
- Token expired

---

### TLS/SSL ошибки

**Симптомы:**
- HTTPS соединения не работают
- Ошибки сертификата

**Решение:**

```bash
# 1. Проверка наличия сертификатов
ls -la certs/

# 2. Проверка срока действия сертификата
openssl x509 -in certs/server.crt -text -noout | grep "Not After"

# 3. Генерация нового self-signed сертификата
openssl req -x509 -nodes -days 365 -newkey rsa:4096 \
  -keyout certs/server.key \
  -out certs/server.crt \
  -subj "/CN=localhost"

# 4. Проверка TLS конфигурации
curl -k https://localhost:8443/api/v1/health
```

**Возможные причины:**
- Отсутствуют сертификаты
- Истек срок действия сертификата
- Неправильные пути к сертификатам
- TLS не включен в конфигурации

---

## Логирование

### Уровни логирования

| Уровень | Описание |
|---------|----------|
| `debug` | Подробная отладочная информация |
| `info` | Общая информация о работе |
| `warn` | Предупреждения |
| `error` | Ошибки |

### Настройка логирования

**Балансировщик:**

```json
{
  "logging": {
    "level": "debug",
    "format": "json"
  }
}
```

**Переменные окружения:**

```bash
LB_LOG_LEVEL=debug
LB_LOG_FORMAT=json
```

**Агент:**

```bash
LOG_LEVEL=debug
LOG_FORMAT=text
```

### Просмотр логов

**Docker Compose:**

```bash
# Логи балансировщика
docker-compose logs -f loadbalancer

# Логи Web UI
docker-compose logs -f webui

# Логи агента
docker-compose -f docker-compose.agent.yml logs -f agent
```

**Systemd:**

```bash
# Логи агента
journalctl -u ollama-agent -f

# Логи с фильтром по уровню
journalctl -u ollama-agent -f | grep -i error
```

**Docker:**

```bash
# Логи контейнера
docker logs -f ollama-legion-balancer
docker logs -f ollama-agent

# Последние N строк
docker logs --tail 100 ollama-legion-balancer
```

---

## Отладка NVML

### Проверка NVML

```bash
# Проверка наличия NVML библиотеки
ldconfig -p | grep nvml

# Проверка nvidia-smi
nvidia-smi

# Проверка внутри контейнера
docker exec ollama-agent nvidia-smi
```

### Включение debug логирования NVML

```bash
# В агенте
export NVML_ENABLED=true
export LOG_LEVEL=debug

# Перезапуск агента
docker restart ollama-agent

# Проверка логов
docker logs ollama-agent | grep -i nvml
```

### Распространенные ошибки NVML

**Ошибка: "NVML: Function Not Found"**

```bash
# Решение: Проверка версии драйвера
nvidia-smi --query-gpu=driver_version --format=csv

# Требуется драйвер 470.x или новее
```

**Ошибка: "NVML: Insufficient Permissions"**

```bash
# Решение: Проверка прав доступа
ls -la /dev/nvidia*

# Добавление пользователя в группу video
sudo usermod -aG video $USER
```

**Ошибка: "NVML: Library Not Found"**

```bash
# Решение: Установка NVML
sudo apt-get install -y nvidia-cuda-toolkit

# Или проверка пути к библиотеке
export LD_LIBRARY_PATH=/usr/lib/x86_64-linux-gnu:$LD_LIBRARY_PATH
```

---

## Performance tuning

### Оптимизация балансировщика

```json
{
  "balancing": {
    "requestTimeout": 180,
    "queueTimeout": 600,
    "queueMaxSize": 500
  },
  "resources": {
    "gpu": {
      "maxUsagePercent": 95,
      "maxVramUsagePercent": 90,
      "maxTemperature": 90
    }
  }
}
```

### Оптимизация агента

```bash
# Увеличение интервала метрик для снижения нагрузки
METRICS_INTERVAL=10s
HEARTBEAT_INTERVAL=5s

# Отключение детального логирования
LOG_LEVEL=info
```

### Мониторинг производительности

```bash
# Проверка метрик кластера
watch -n 5 'curl -s http://localhost:18081/api/v1/cluster | jq'

# Проверка отдельных бэкендов
watch -n 5 'curl -s http://localhost:18081/api/v1/metrics/gpu-1 | jq'

# WebSocket мониторинг
wscat -c ws://localhost:18081/ws/metrics
```

### Tuning параметров

| Параметр | Рекомендация |
|----------|--------------|
| `requestTimeout` | Увеличить для долгих запросов (180-300 сек) |
| `queueMaxSize` | Увеличить при высокой нагрузке (200-500) |
| `metricsInterval` | Увеличить для снижения нагрузки (10-30 сек) |
| `healthCheckInterval` | Оптимизировать под сеть (10-30 сек) |
| `maxConcurrentRequests` | Настроить под GPU (10-50) |

---

## Диагностика

### Скрипт диагностики

```bash
#!/bin/bash
# diagnose.sh

echo "=== Ollama Load Balancer Diagnostics ==="

echo -e "\n1. Checking Load Balancer health..."
curl -s http://localhost:18081/api/v1/health | jq

echo -e "\n2. Checking cluster status..."
curl -s http://localhost:18081/api/v1/cluster | jq

echo -e "\n3. Checking backends..."
curl -s http://localhost:18081/api/v1/backends | jq

echo -e "\n4. Checking metrics..."
curl -s http://localhost:18081/api/v1/metrics | jq

echo -e "\n5. Checking Docker containers..."
docker ps | grep ollama

echo -e "\n6. Checking GPU status..."
nvidia-smi

echo -e "\n7. Checking Ollama..."
curl -s http://localhost:11434/api/tags | jq
```

### Чеклист диагностики

- [ ] Балансировщик запущен и здоров
- [ ] Все бэкенды в статусе `healthy`
- [ ] Агенты отправляют heartbeat
- [ ] Ollama API доступен на всех бэкендах
- [ ] GPU метрики собираются корректно
- [ ] WebSocket подключения работают
- [ ] Аутентификация настроена правильно
- [ ] TLS сертификаты валидны
- [ ] Очередь не переполнена
- [ ] Логи не содержат критических ошибок

---

## Дополнительные ресурсы

- [Конфигурация](configuration.md) — Настройка всех компонентов
- [API документация](api.md) — REST API и WebSocket
- [Развертывание](deployment.md) — Production deployment

---

## Получение помощи

Если проблема не решена:

1. Соберите логи всех компонентов
2. Запустите скрипт диагностики
3. Проверьте конфигурационные файлы
4. Убедитесь, что все требования выполнены

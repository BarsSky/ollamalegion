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

### Балансировщик падает с panic при POST /api/generate

**Симптомы:**
- При отправке POST-запросов на `/api/generate` балансировщик падает с `panic: runtime error: invalid memory address or nil pointer dereference`
- В логах: `http: panic serving ...: ollama-loadbalancer/internal/balancer.(*Proxy).ServeHTTP ... proxy.go:397`
- Docker-контейнер перезапускается, но падает снова при каждом POST generate
- GET-запросы (`/api/tags`, `/api/version`) работают корректно

**Решение:**

```bash
# 1. Проверьте версию образа
docker images | grep balancer

# 2. Пересоберите образ из актуального кода
docker-compose -f deployments/docker-compose.yml build --no-cache loadbalancer

# 3. Перезапустите контейнер
docker-compose -f deployments/docker-compose.yml up -d --no-deps loadbalancer

# 4. Убедитесь, что больше нет panic в логах
docker logs --tail 20 ollama-legion-balancer | grep -i panic
# Должно быть пусто
```

**Возможные причины:**
- Docker-образ собран из устаревшего кода без фикса nil pointer dereference
- Бинарник в образе отличается от актуального исходного кода
- Слой сборки закэширован и не обновлялся после исправления

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

### Модели cppworker не отображаются в OpenWebUI

**Симптомы:**
- OpenWebUI подключён к балансеру, но список моделей пуст
- В логах балансера нет ошибок при `GET /api/tags`
- CppWorker запущен и отвечает на `curl http://localhost:18091/health`

**Причина:**
Цепочка `OpenWebUI → Balancer /api/tags → CppWorker /api/tags` имеет несколько точек отказа:
1. **Неверный host в конфигурации бэкенда** — в `data/state.json` указан `localhost`, но балансер в Docker-контейнере не может достучаться до cppworker по `localhost`
2. **CppWorker не зарегистрирован в балансере** — при запуске `docker-compose.full.yml` нет автоматической регистрации бэкенда
3. **Модель не загружена в память cppworker** — `/api/tags` возвращает только **активно загруженные** модели

**Диагностика:**

```bash
# 1. Проверьте, что cppworker отвечает на health-check
curl http://localhost:18091/health

# 2. Проверьте список загруженных моделей в cppworker
curl http://localhost:18091/api/tags
# Если ответ {"models":[]} — модели не загружены

# 3. Проверьте список файлов .gguf в директории моделей
curl http://localhost:18091/models-dir

# 4. Проверьте, зарегистрирован ли бэкенд в балансере
curl http://localhost:18081/api/v1/backends | jq '.[] | select(.type=="llama_cpp")'

# 5. Проверьте health-check бэкенда через балансер
curl http://localhost:18081/api/v1/cluster | jq '.backends[] | {id, status, backendType, host}'
```

**Решение:**

1. **Загрузите модель в cppworker:**
   ```bash
   curl -X POST http://localhost:18091/load \
     -H "Content-Type: application/json" \
     -d '{"name": "your-model"}'
   ```

2. **Зарегистрируйте бэкенд вручную (если авто-регистрация не сработала):**
   ```bash
   curl -X POST http://localhost:18081/api/v1/backends \
     -H "Content-Type: application/json" \
     -d '{
       "id": "cppworker-gpu",
       "name": "CppWorker",
       "host": "cppworker",
       "ollamaPort": 0,
       "agentPort": 0,
       "cppWorkerPort": 18091,
       "weight": 10,
       "maxConcurrentRequests": 4,
       "maxModels": 3,
       "backendType": "llama_cpp",
       "backendEngine": "llama_cpp",
       "gpuMode": "auto"
     }'
   ```
   Важно: `"host"` должен быть именем Docker-сервиса (`cppworker`) или IP, доступным из контейнера балансера.

3. **Проверьте `data/state.json`:**
   ```bash
   docker exec ollama-legion-balancer cat /app/data/state.json | jq '.backends[] | select(.type=="llama_cpp")'
   ```
   Убедитесь, что:
   - `"type": "llama_cpp"` (не `"ollama"`)
   - `"cppWorkerPort": 18091` (не 0)
   - `"host"` содержит правильный адрес cppworker (не `localhost`)

4. **Проверьте агрегацию на балансере:**
   ```bash
   curl http://localhost:18080/api/tags
   ```
   Этот запрос через прокси-порт должен вернуть объединённый список моделей со всех бэкендов.

**Авто-регистрация (docker-compose.full.yml):**

При запуске через `docker-compose.full.yml` cppworker автоматически регистрируется в балансере. Для этого должны быть установлены переменные:
- `BALANCER_URL=http://loadbalancer:18081` (уже задано в compose-файле)
- `CPPWORKER_HOST=cppworker` (имя Docker-сервиса)

Если авто-регистрация не сработала, проверьте логи:
```bash
docker logs ollama-legion-cppworker | grep "\[register\]"
```

---

### TransferEncodingError / UND_ERR_SOCKET в OpenWebUI

**Симптомы:**
- OpenWebU показывает ошибку `Response payload is not completed: <TransferEncodingError: 400, message='Not enough data to satisfy transfer length header.'>`
- В логах OpenWebUI также `UND_ERR_SOCKET`
- Ошибка возникает при streaming-запросах (/api/generate, /api/chat) при общении через прокси-порт балансера

**Причина:**
Streaming-ответ (SSE/chunked) был оборван до получения завершающего чанка (`done: true` или chunked terminator `0\r\n\r\n`). Это происходит когда:
1. Бэкенд Ollama аварийно завершился во время генерации токенов
2. Сетевое соединение между балансером и бэкендом Ollama разорвано
3. Таймаут streaming-контекста истёк (по умолчанию 10 минут)

**Диагностика:**

```bash
# 1. Включите debug-логирование балансера
docker exec -e LB_LOG_LEVEL=debug ollama-legion-balancer kill -HUP 1
# Или перезапустите с debug уровнем в config.json:
# "logging": {"level": "debug"}

# 2. Отслеживайте логи балансера при возникновении ошибки
docker logs -f ollama-legion-balancer | grep -E "STREAMING_BACKEND_READ_ERROR|PROXY_BACKEND_REQUEST_FAILED|STREAMING_SENDING_SSE_ERROR"

# 3. Проверьте состояние бэкендов
curl -s http://localhost:18081/api/v1/cluster | jq '.backends[] | {id: .id, status: .status, activeReqs: .ollama.activeRequests}'

# 4. Проверьте логи Ollama на бэкенде
ssh <gpu-server> 'docker logs ollama --tail 50'
```

**Ключевые записи в логах балансера:**

При возникновении ошибки ищите следующие префиксы:

| Префикс лога | Значение |
|-------------|----------|
| `PROXY_BACKEND_REQUEST_FAILED` | Ошибка при отправке запроса к Ollama. `error_type` указывает тип: `connection_refused`, `connection_reset_by_peer`, `context_deadline_exceeded`, `timeout`, `broken_pipe`, `unexpected_eof` |
| `STREAMING_BACKEND_READ_ERROR` | Ошибка чтения streaming-ответа от бэкенда. Содержит `read_error`, `bytes_streamed`, `chunk_count`, `elapsed_ms`, `idle_ms` |
| `STREAMING_SENDING_SSE_ERROR` | Балансер пытается отправить клиенту SSE-ошибку с `done:true` для корректного завершения потока |

**Пример логов при обрыве соединения с бэкендом:**
```json
{"level":"error","msg":"STREAMING_BACKEND_READ_ERROR","backend":"gpu-1","model":"qwen3:0.6b","read_error":"read tcp 10.0.1.5:11434->10.0.1.100:48372: connection reset by peer","read_error_type":"*net.OpError","bytes_streamed":1234,"chunk_count":12,"elapsed_ms":4500,"idle_ms":0,"client_disconnected":false,"context_error":"","is_sse":true}
{"level":"warn","msg":"STREAMING_SENDING_SSE_ERROR","backend":"gpu-1","model":"qwen3:0.6b","code":"backend_read_error","bytes_streamed":1234,"chunk_count":12}
```

**Решение:**

1. **Проверка стабильности бэкенда:**
   ```bash
   # Проверьте, не перезагружается ли Ollama
   ssh <gpu-server> 'docker ps -a --filter name=ollama'
   ssh <gpu-server> 'docker logs ollama --tail 100 | grep -i "error\|fatal\|panic"'
   
   # Проверьте использование VRAM — возможно OOM killer убивает процесс
   ssh <gpu-server> 'nvidia-smi'
   ```

2. **Увеличьте таймауты (если проблема в длинных генерациях):**
   ```json
   {
     "balancing": {
       "streamTimeout": 900,
       "requestTimeout": 300
     }
   }
   ```

3. **Проверьте сетевую связность:**
   ```bash
   # С балансера проверьте доступность Ollama
   docker exec ollama-legion-balancer curl -v http://<gpu-ip>:11434/api/tags
   ```

4. **При множественных ошибках — настройте мониторинг:**
   ```bash
   # Логи балансера с фильтрацией ошибок streaming
   docker logs -f ollama-legion-balancer 2>&1 | grep --line-buffered -E "STREAMING_BACKEND_READ_ERROR|PROXY_BACKEND_REQUEST_FAILED"
   ```


### Cline через Ollama API не разбирает ответ (`Invalid API Response`)

**Симптомы:**
- Cline (расширение VS Code, использующее `ollama-js` 0.5.x) отправляет `POST /api/chat` на балансер
- В логах балансера: `200 1931ms` (т.е. запрос успешно дошёл до cppworker)
- Cline показывает "Invalid API Response: The provider returned an empty or unparsable response"
- При этом прямой запрос к cppworker (`curl http://<cppworker>:18091/v1/chat/completions`) возвращает нормальный ответ

**Корневые причины и решения:**

#### 1. Модель `gemma-*-it-Q4_K_M` без chat template

Gemma требует специфического chat template (`<start_of_turn>user\n...<end_of_turn>\n<start_of_turn>model\n...`).
Без него модель эмитит служебные токены как обычный текст, который ломает парсер Cline.

**Проверка:**
```bash
strings /models/gemma-*.gguf | grep -A 30 "chat_template"
# Должна быть секция с Jinja-шаблоном
```

**Решение:** запустите cppworker с флагом `--jinja`, чтобы использовать chat template из GGUF:
```bash
./llama-server \
  -m /models/gemma-*-it-Q4_K_M.gguf \
  --jinja \
  --flash-attn \
  -ngl -1 \
  -c 8192
```

#### 2. Служебные токены в streaming-ответе

Если chat template всё-таки настроен, но модель изредка эмитит `<end_of_turn>` отдельным content-чанком,
балансер фильтрует такие токены через `shouldFilterLlamaCppContent` (см. `internal/balancer/llamacpp_transport.go`).
Убедитесь, что используется последняя версия кода — фильтр добавлен в `translateSSEChatToOllama` для покрытия
Ollama NDJSON-пути (ранее работал только для OpenAI SSE).

#### 3. Неверный формат `created_at`

OpenAI-совместимый ответ от cppworker возвращает `created` как Unix timestamp (число),
а Ollama ожидает ISO-8601 / RFC3339 строку. Если балансер пробрасывал число «как есть»,
ollama-js (Cline) мог падать с ошибкой парсинга.

**Решение:** в балансере добавлен helper `convertCreatedToRFC3339`, который нормализует значение в `time.RFC3339`.

#### 4. Диагностика "сырого" ответа от cppworker

Включите debug-логирование балансера:
```json
{"logging": {"level": "debug"}}
```

```bash
docker logs -f ollama-legion-balancer 2>&1 | grep "proxyRequestLlamaCpp"
```

Ищите записи `proxyRequestLlamaCpp: non-stream body preview` (для non-stream) или 
`translateSSEChatToOllama: filtered service token` (для stream) — они покажут, 
что ИМЕННО отдаёт upstream до трансляции в Ollama-формат.

#### 5. Рекомендуемая конфигурация cppworker для Cline + instruct-моделей

```bash
# config/cppworker.env
LLAMA_CTX_SIZE=8192           # Cline шлёт длинный system prompt + tools
LLAMA_BATCH_SIZE=512
LLAMA_N_GPU_LAYERS=-1         # все слои на GPU
LLAMA_FLASH_ATTN=true
LLAMA_MMAP=true
LLAMA_IDLE_UNLOAD=30m
```

Команда запуска `llama-server` (если настраиваете вручную):
```bash
./llama-server \
  -m /models/gemma-3-4b-it-Q4_K_M.gguf \
  --jinja \
  --flash-attn \
  -ngl -1 \
  -c 8192 \
  --special \
  --port 18091
```

**Примечание:** Gemma 3 имеет ограниченную поддержку function calling / tool use.
Если Cline активно использует tools, лучше переключиться на модель с полной поддержкой
(`qwen2.5-coder`, `llama3.1`, `mistral-nemo`).

#### 6. Быстрая диагностика через прямое сравнение

```bash
# A. Прямой запрос к cppworker (минуя балансер)
curl -X POST http://<cppworker>:18091/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"gemma-*-it-Q4_K_M","messages":[{"role":"user","content":"Say OK"}],"stream":false,"max_tokens":50}'

# B. Запрос через балансер
curl -X POST http://localhost:18081/api/chat \
  -H "Content-Type: application/json" \
  -d '{"model":"gemma-*-it-Q4_K_M","messages":[{"role":"user","content":"Say OK"}],"stream":false,"max_tokens":50}'
```

Если A возвращает нормальный JSON с `content`, а B — `Invalid API Response` или мусор,
проблема в трансляции (проверьте логи `proxyRequestLlamaCpp: non-stream body preview`).

Если ОБА возвращают мусор (например, `<end_of_turn>Ok<end_of_turn>`), проблема в chat template модели.

---

### go vet: could not determine what C.* refers to

**Симптомы:**
- `go vet` выдаёт ошибку `could not determine what C.<functionName> refers to` в CGo-файлах
- Ошибка возникает при проверке пакетов, использующих CGo (например, `./internal/cppbackend/...`)

**Причина:**
`go vet` имеет ограниченную поддержку CGo и не может полностью разрешить C-идентификаторы, экспортированные из Go через `//export`. Это известное ограничение, особенно на Windows.

**Решение:**
Исключите CGo-пакеты из `go vet`. Проверяйте только чистые Go-пакеты:

```bash
# Правильно — только не-CGo пакеты:
go vet ./internal/balancer/... ./pkg/types/... ./internal/api/... ./internal/config/...

# Не включайте CGo-пакеты:
# go vet ./internal/cppbackend/...   # ← этот пакет импортирует c/bridge (CGo)
```

Для проверки CGo-кода используйте `go build`:
```bash
go build ./internal/cppbackend/...
```

---

## Дополнительные ресурсы

- [Конфигурация](configuration.md) — Настройка всех компонентов
- [API документация](api.md) — REST API и WebSocket
- [Развертывание](deployment.md) — Production deployment
- [Cline troubleshooting](cline-troubleshooting.md) — Диагностика Cline (VS Code) через балансер + cppworker

---

## Получение помощи

Если проблема не решена:

1. Соберите логи всех компонентов
2. Запустите скрипт диагностики
3. Проверьте конфигурационные файлы
4. Убедитесь, что все требования выполнены

# Аудит проекта Ollama Load Balancer
## Дата проведения: 2026-04-28
## Версия: 1.0.0

---

## Резюме

Проведена комплексная проверка корректности документации, исходного кода, конфигураций и WebUI проекта Ollama Load Balancer. Выявлено **3 критических бага**, **4 несоответствия документации** и **3 проблемы в WebUI**.

---

## 🔴 Критические баги (P0)

### 1. Недетерминированный Master Token — Уязвимость безопасности

**Файл:** `internal/api/auth.go`  
**Строки:** 84–90, 135–139

**Проблема:** Функции `IsMasterToken()` и `isMasterTokenLocked()` определяют master token как минимальный по строковому сравнению ключ из `map[string]bool`. Поскольку итерация по Go-map имеет недетерминированный порядок, результат случайный.

```go
func (a *TokenAuthenticator) isMasterTokenLocked(token string) bool {
    var master string
    for t := range a.tokens {  // ← случайный порядок!
        if master == "" || t < master {
            master = t
        }
    }
    return token == master
}
```

**Последствия:**
- Любой токен может случайно стать master
- Проверка `RemoveToken()` некорректно защищает master token
- Невозможно гарантированно управлять токенами

**Исправление:** Хранить master token в отдельном поле при инициализации, не вычислять динамически.

---

### 2. Утечка Goroutine и TCP-соединений

**Файл:** `internal/balancer/ollama_router.go`  
**Строки:** 291–305 (`handleDelete`)

**Проблема:** В горутинах параллельных HTTP-запросов `resp.Body` никогда не закрывается. Функция `copyResponse()` тоже не закрывает `resp.Body`.

```go
go func(b types.Backend) {
    resp, err := proxyHTTP(client, backendURL, r)  // ← resp.Body не закрывается!
    if err != nil { ...; return }
    if atomic.CompareAndSwapInt32(&done, 0, 1) {
        copyResponse(w, resp)  // ← тоже не закрывает Body
    }
}(backend)
```

**Последствия:**
- При каждом `/api/delete` накапливаются незакрытые TCP-соединения
- Утечка файловых дескрипторов → `too many open files`
- Рост числа goroutine

**Исправление:** `defer resp.Body.Close()` после каждого успешного `client.Do()`.

---

### 3. Избыточная загрузка TLS конфигурации

**Файл:** `cmd/balancer/main.go`  
**Строки:** 92–98

**Проблема:** TLS конфигурация загружается дважды. Первая загрузка записывается в `_ = tlsConfig` и результат отбрасывается.

```go
tlsConfig, err := api.LoadTLSConfig(&conf.TLS)  // ← загружено
if err != nil { ... }
_ = tlsConfig // будет использовано ниже  // ← отброшено!
```

**Исправление:** Удалить первый блок загрузки TLS (строки 82–98).

---

## 🟠 Несоответствия документации и конфигураций (P1)

### 4. Устаревшие порты в CHANGELOG.md

**Файл:** `CHANGELOG.md`, строки 261–262  
**Проблема:** Таблица env vars указывает `LB_PORT=8080`, `LB_API_PORT=8081` — порты из ранней версии. Реальные порты проекта: **18080** и **18081**.

**Исправление:** Заменить 8080→18080, 8081→18081 во всём CHANGELOG.

---

### 5. Ошибочный порт в DEPLOYMENT.md

**Файл:** `DEPLOYMENT.md`, строка 499  
**Проблема:** `curl -s http://<IP>:8081/api/v1/cluster` — порт 8081 вместо 18081.

**Исправление:** Заменить на 18081.

---

### 6. Разные форматы интервалов

| Файл | Параметр | Значение |
|------|----------|----------|
| `docker-compose.yml` | `LB_METRICS_INTERVAL` | `5` (число) |
| `config/agent.example.env` | `METRICS_INTERVAL` | `5s` (со суффиксом) |

**Проблема:** Разные форматы могут вызывать ошибки парсинга.

**Исправление:** Унифицировать формат во всех конфигурациях.

---

### 7. Противоречие имён переменных NVML

| Файл | Переменная | Значение по умолчанию |
|------|------------|----------------------|
| `docker/agent/Dockerfile` | `ENABLE_NVML` | `true` |
| `deployments/docker-compose.agent.yml` | `ENABLE_NVML` | `${ENABLE_NVML:-false}` |
| `config/agent.example.env` | `NVML_ENABLED` | `false` |

**Проблема:** Три разных имени/значения. Пользователь легко запутается.

**Исправление:** Унифицировать на `ENABLE_NVML` с значением `false` по умолчанию.

---

## 🟡 Проблемы WebUI (P2)

### 8. Вкладка "Модели" — VRAM/RAM прогресс-бары

**Файл:** `webui/js/app.js`, строки 807–903 (`renderModelsPage`)

**Проблема:** Карточки моделей отображают прогресс-бары VRAM/RAM, но данные берутся из `m.vramUsage`/`m.ramUsage`. В структуре `RunningModel` (pkg/types) поля могут называться иначе. Если backend не заполняет эти поля — прогресс-бары всегда показывают 0%.

**Действие:** Проверить соответствие полей в `pkg/types` и `internal/agent/collector.go`.

---

### 9. Вкладка "Очередь" — пустая таблица задач

**Файл:** `webui/js/app.js`, строки 1325–1347 (`fetchQueueDetails`)

**Проблема:** 
- Endpoint `/api/v1/queue/details` требует аутентификации (`AuthMiddleware`)
- При включённом auth WebUI получает 401 Unauthorized
- `renderQueueTasks()` показывает "Нет задач в очереди" при любом пустом ответе

**Действие:** Проверить логи браузера; добавить auth header в запросы WebUI.

---

### 10. `loadSettings()` — placeholder без реализации

**Файл:** `webui/js/app.js`, строки 1242–1253

**Проблема:** Функция пытается загрузить настройки с `/api/v1/health`, но health endpoint не возвращает конфигурацию.

```javascript
// Note: The health endpoint may not return full config
// This is a placeholder - actual implementation would fetch config
```

**Исправление:** Реализовать `GET /api/v1/config` или убрать placeholder.

---

## 📊 Статистика

| Категория | Количество |
|-----------|------------|
| Критические баги (P0) | 3 |
| Несоответствия документации (P1) | 4 |
| Проблемы WebUI (P2) | 3 |
| **Итого** | **10** |

---

## План устранения

| Приоритет | Задача | Оценка времени |
|-----------|--------|----------------|
| P0 | Исправить master token security | 1 ч |
| P0 | Закрывать resp.Body в goroutine | 30 мин |
| P1 | Унифицировать порты в CHANGELOG | 15 мин |
| P1 | Исправить порт в DEPLOYMENT.md | 5 мин |
| P1 | Унифицировать NVML env vars | 30 мин |
| P2 | Удалить избыточную TLS загрузку | 10 мин |
| P2 | Реализовать loadSettings | 1 ч |
| P3 | Проверить WebUI Models VRAM/RAM | 2 ч |
| P3 | Проверить Queue API + auth | 2 ч |

---

## Исполнитель

**Проверил:** Cline (AI Assistant)  
**Дата:** 2026-04-28  
**Ревизия:** `9bc1ba93919a656fe1ad1a93aaa188d44ed3423d`
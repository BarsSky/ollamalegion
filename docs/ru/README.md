# Документация OllamaLegion (Русский)

Добро пожаловать в документацию **OllamaLegion** — высокопроизводительного балансировщика нагрузки для Ollama с WebUI, мониторингом и поддержкой нескольких бэкендов.

## Содержание

| Документ | Описание |
|----------|----------|
| [Установка](../installation.md) | Пошаговая инструкция по установке |
| [Конфигурация](../configuration.md) | Все параметры конфигурации балансера и агентов |
| [Режимы балансировки](../balancing-guide.md) | Описание стратегий балансировки: round-robin, resource-aware, model-affinity, session-stickiness |
| [Деплой](../deployment.md) | Docker Compose, Kubernetes, systemd |
| [Деплой агента](../agent-deployment.md) | Установка и настройка агентов мониторинга |
| [API Reference](../api.md) | Полное описание REST API балансера |
| [Метрики](../ollamalegion-metrics.md) | Prometheus-метрики и мониторинг |
| [Устранение неполадок](../troubleshooting.md) | Частые проблемы и их решения |
| [Аудит](../audit-tracking.md) | Система аудита и отслеживания запросов |
| [OpenAPI спецификация](../openapi.yaml) | Swagger/OpenAPI 3.0 |

## Быстрый старт

```bash
# Клонируйте репозиторий
git clone https://github.com/BarsSky/ollamalegion.git
cd ollamalegion

# Запустите через Docker Compose
docker compose -f deployments/docker-compose.yml up -d

# Откройте WebUI
open http://localhost:8080
```

## Структура проекта

```
ollamalegion/
├── cmd/
│   ├── balancer/     # Точка входа балансировщика
│   ├── agent/        # Точка входа агента мониторинга
│   └── monitor/      # Терминальный монитор (TUI)
├── internal/
│   ├── api/          # REST API + WebSocket
│   ├── balancer/     # Ядро балансировки
│   ├── agent/        # Агент сбора метрик
│   └── config/       # Загрузка конфигурации
├── webui/            # Web-интерфейс (SPA)
├── docs/             # Документация
│   ├── ru/           #   Русская версия
│   └── en/           #   English version
├── deployments/      # Docker Compose, Kubernetes
├── scripts/          # Скрипты сборки и деплоя
└── tests/            # Интеграционные тесты
```

## Поддержка языков

Документация доступна на нескольких языках:
- [English](../en/README.md)
- Русский (текущий)

WebUI поддерживает:
- Русский
- English

Для добавления нового языка в WebUI см. [инструкцию по i18n](../../webui/js/i18n/README.md).

---

[Вернуться на главную](../../README.md)
# План реализации — CppBackend GGUF + Multi-GPU

## Фаза 0: Подготовка инфраструктуры (3-5 дней)
- [ ] 0.1 Создать директорию c/llama.cpp/ с Makefile для сборки статической библиотеки
- [ ] 0.2 Создать CGo bridge (bridge.h, bridge.c, bridge.go)
- [ ] 0.3 Создать Go-обёртку cppbackend/backend.go
- [ ] 0.4 Создать cmd/cppworker/main.go — точка входа
- [ ] 0.5 Dockerfile + docker-compose конфигурация
- [ ] 0.6 Проверить сборку и запуск

## Фаза 1: Model Manager (2-3 дня)
- [ ] 1.1 ModelManager (model_manager.go)
- [ ] 1.2 Конфигурация (config.go)
- [ ] 1.3 Параметры контекста
- [ ] 1.4 Обработка ошибок
- [ ] 1.5 Metrics (metrics.go)

## Фаза 2: Multi-GPU (3-5 дней)
- [ ] 2.1 tensor_split (multi_gpu.go)
- [ ] 2.2 GPU Discovery
- [ ] 2.3 Автоматическое распределение
- [ ] 2.4 Ручное распределение
- [ ] 2.5 CPU offload
- [ ] 2.6 Stress-тест

## Фаза 3: Inference Engine (3-5 дней)
- [ ] 3.1 Синхронный инференс
- [ ] 3.2 Параметры генерации
- [ ] 3.3 Streaming
- [ ] 3.4 Batch inference
- [ ] 3.5 Context management
- [ ] 3.6 Embedding
- [ ] 3.7 Timeout + Cancel

## Фаза 4: HTTP/gRPC Server (2-3 дня)
- [ ] 4.1 HTTP Server
- [ ] 4.2 Ollama-совместимый API
- [ ] 4.3 GGUF API
- [ ] 4.4 Multi-GPU API
- [ ] 4.5 Health check

## Фаза 5: HuggingFace (3-4 дня)
- [ ] 5.1 HF API Client
- [ ] 5.2 Downloader
- [ ] 5.3 CLI wrapper
- [ ] 5.4 Cache
- [ ] 5.5 Status tracking
- [ ] 5.6 Auto-download

## Фаза 6: WebUI GGUF (3-4 дня)
- [ ] 6.1 HTML страница (gguf.html)
- [ ] 6.2 JS модуль gguf-models.js
- [ ] 6.3 JS модуль gguf-download.js
- [ ] 6.4 GPU распределение UI
- [ ] 6.5 i18n
- [ ] 6.6 Навигация

## Фаза 7: Balancer Integration (2-3 дня)
- [ ] 7.1 Backend registration
- [ ] 7.2 VirtualModel интеграция
- [ ] 7.3 RPC Coordinator интеграция
- [ ] 7.4 Живая миграция
- [ ] 7.5 Monitoring

## Фаза 8: Multi-Node Pipeline (4-6 дней)
- [ ] 8.1 Pipeline orchestration
- [ ] 8.2 KV Cache sync
- [ ] 8.3 Wide pipeline
- [ ] 8.4 Failover
- [ ] 8.5 E2E test

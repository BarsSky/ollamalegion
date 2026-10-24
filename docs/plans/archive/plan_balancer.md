1. Текущий механизм распределения (что уже есть)
1.1 Двухэтапный алгоритм selectBackend
Этап	Логика	Проблема в вашем сценарии
Session Stickiness	Если есть активная сессия (sticky session) → вернуть тот же бэкенд	Игнорирует загрузку
Model Affinity	Поиск бэкенда, где модель УЖЕ загружена в памяти → findBackendWithModel	Возвращает только бэкенды с активной загрузкой модели
Fallback с async warmup	loadModelOnFallbackRLocked(...) → запускает асинхронную загрузку модели на свободном бэкенде	Модель начинает загружаться, но запрос НЕ ЖДЁТ окончания загрузки — он получает ошибку или уходит в очередь
Scoring (selectByResources)	Формула: GPU*0.35 + VRAM*0.25 + CPU*0.20 − requestPenalty + modelCapacityScore + predictionBonus × weight	Не учитывает статус загрузки модели
1.2 Очередь запросов
Очередь активируется только когда ни один бэкенд не подходит (нет здорового бэкенда с моделью или ресурсами). Запросы в очереди ретраятся до 3 раз, затем принудительно идёт selectFreeBackendAny.

Проблема в вашем сценарии: Очередь включается слишком поздно — когда уже нет доступных бэкендов, а не превентивно, когда бэкенд перегружен, но модель можно загрузить на другом.

1.3 Механизм редиректа (sticky session rebalance)
findLessLoadedBackendWithModel / findLessLoadedBackendAny — срабатывает при превышении loadRatio > 50%. Перемещает sticky-сессию на менее загруженный бэкенд. Не учитывает необходимость загрузки модели на целевом бэкенде.

2. Ключевая проблема: разрыв между обнаружением свободного бэкенда и загрузкой модели

Текущий flow:
Запрос → selectBackend → findBackendWithModel → НЕТ (модель не загружена)
→ loadModelOnFallbackRLocked → async загрузка
→ findLessLoadedBackendAny → возвращает бэкенд (но модель не готова!)
→ Ошибка или очередь
Корневая причина: Нет механизма синхронного ожидания готовности модели и нет превентивной загрузки модели на свободные бэкенды.

3. Предлагаемый оптимальный механизм распределения
3.1 Многофакторная модель принятия решения

Решение = f(
    Ресурсы_бэкенда,          // GPU, VRAM, CPU (уже есть)
    Состояние_модели,           // LOADED / LOADING / NOT_LOADED / UNLOADING
    Прогноз_завершения,        // ETA текущих запросов (predictor уже есть, но слабо используется)
    Глубина_очереди,            // Сколько запросов ждут на каждом бэкенде
    Приоритет_запроса,         // latency-critical vs batch
    Стоимость_миграции,        // Время на загрузку модели на новом бэкенде
    История_ошибок,             // Частота таймаутов
    Размер_модели,              // Влияет на время загрузки
    Политика_кол-ва_экземпляров // min/max instances per model
)
3.2 Конкретные предложения по доработке
A. Превентивная загрузка модели (Pre-warming)

Триггеры:
├── Загрузка бэкенда > 70% на бэкенде с моделью M
├── В пуле есть свободный бэкенд без модели M, но совместимый по ресурсам
└── Количество ожидающих запросов к модели M > 0

Действие:
→ Запустить загрузку модели M на свободном бэкенде ДО того, как понадобится
→ Пометить бэкенд как WARMING_UP
→ По готовности — включить в пул для model affinity
Что нужно добавить в код:

Новое поле в BackendState: WarmingUpModels map[string]*WarmupState
WarmupState содержит: StartedAt, EstimatedReadyAt, TriggerReason
Фоновый процесс (prewarmController), запускаемый каждые N секунд
Метод evaluatePrewarm() в selectBackend или отдельной горутине
B. Динамический скоринг с учётом стоимости загрузки
Текущая формула (calculateScore, proxy.go ~1460):


score = GPU_free*0.35 + VRAM_free*0.25 + CPU_free*0.20 - requestPenalty + modelCapacityScore + predictionBonus
Предлагается расширить:


score = GPU_free*0.30 + VRAM_free*0.20 + CPU_free*0.15
      - requestPenalty
      + modelAlreadyLoaded*0.15          // бонус за готовую модель
      - modelLoadingCost*0.10            // штраф за необходимость загрузки
      + predictionBonus*weight
      - queueDepthPenalty*0.05           // штраф за глубину очереди
      - errorRatePenalty*0.05            // штраф за историю ошибок
      + modelCapacityScore
C. Механизм ожидания готовности модели
Вместо async loadModelOnFallbackRLocked с немедленным возвратом:


selectBackend():
  если model affinity бэкенд найден → возвращаем
  если есть WARMING_UP бэкенд с ETA < threshold:
    → ожидаем готовности (с таймаутом)
    → если дождались → возвращаем
  если есть свободный бэкенд:
    → запускаем загрузку синхронно (с таймаутом, например 30с)
    → если загрузилась → возвращаем
  → fallback: очередь
D. Контроллер кол-ва экземпляров модели (Model Instance Controller)

Конфигурация на модель:
{
  "model": "llama3.2:3b",
  "min_instances": 2,        // минимум бэкендов с загруженной моделью
  "max_instances": 4,        // максимум (чтобы не занимать все бэкенды)
  "idle_unload_after": "5m"  // выгрузить, если нет запросов > 5 мин
}

Фоновый процесс (modelReplicaController):
├── Каждые 30 сек проверяет фактическое кол-во экземпляров
├── Если < min_instances → загружает на свободных бэкендах
├── Если > max_instances → выгружает с наименее загруженных
└── Если idle > threshold → выгружает
3.3 Ресурсный резерв (Headroom Reservation)

На каждом бэкенде резервировать:
├── 10-15% GPU памяти как "горячий резерв"
└── Не учитывать резерв в scoring для model affinity
    (чтобы загруженная модель не блокировала pre-warming)

При достижении резерва → триггерить pre-warming на других бэкендах
4. Чего не хватает для полного контроля и оптимизации
4.1 Отсутствующие метрики
Метрика	Зачем нужна	Где добавить
model_load_time_seconds	Время загрузки модели (для оценки стоимости миграции)	loadModelOnFallbackRLocked
model_unload_time_seconds	Время выгрузки модели	Метод выгрузки
request_queue_depth	Глубина очереди на бэкенд	proxyRequest
request_wait_time_seconds	Время ожидания в очереди	handleQueueRequest (pool workers)
backend_utilization_percent	Утилизация бэкенда (0-100%)	checkResourceLimits
model_instance_count	Количество экземпляров каждой модели	Фоновый процесс
prewarm_trigger_count	Сколько раз сработал pre-warming	Новый контроллер
cold_start_latency_penalty	Дополнительная задержка из-за холодного старта	proxyRequest
4.2 Отсутствующие компоненты
Компонент	Статус	Что нужно
Predictor	Есть базовая версия (predictor.go, 359 строк)	Усилить: предсказание на основе размера модели, истории, ETA
Model Instance Controller	Нет	Создать: управление репликами моделей
Prewarm Controller	Нет	Создать: превентивная загрузка
Synchronous Model Loading	Есть async (loadModelOnFallbackRLocked)	Добавить sync режим с таймаутом
Unload Scheduler	Нет	Создать: выгрузка неиспользуемых моделей по политике
Backpressure Mechanism	Частично (очередь)	Усилить:拒絕 запросов при превышении capacity
4.3 Отсутствующие конфигурационные параметры

{
  "balancing": {
    "prewarm": {
      "enabled": true,
      "trigger_load_threshold": 0.70,       // загрузка бэкенда для триггера
      "max_prewarm_per_cycle": 2             // макс. одновременных pre-warm
    },
    "model_instances": {
      "default_min": 1,
      "default_max": 3,
      "idle_unload_after": "10m"
    },
    "scoring": {
      "weights": {
        "model_already_loaded": 0.15,
        "model_loading_cost": 0.10,
        "queue_depth_penalty": 0.05,
        "error_rate_penalty": 0.05,
        "prediction_bonus": 0.10
      }
    },
    "sync_model_load": {
      "enabled": true,
      "timeout": "30s"
    },
    "resource_reservation": {
      "gpu_headroom_percent": 15,
      "ram_headroom_percent": 10
    }
  }
}
5. Итоговая архитектура (предлагаемая)

                    ┌─────────────────────────┐
                    │    Request arrives       │
                    └───────────┬─────────────┘
                                ▼
              ┌─────────────────────────────────────┐
              │         selectBackend()              │
              │  ┌─ Session Stickiness              │
              │  ├─ Model Affinity (LOADED only)    │
              │  ├─ Model Warming (WARMING_UP + ETA)│
              │  ├─ Sync Model Load (with timeout)  │
              │  └─ selectByResources (scoring v2)  │
              └───────────────┬─────────────────────┘
                              ▼
              ┌───────────────────────────────┐
              │    Decision Router            │
              │  ┌── Backend Ready → Proxy   │
              │  ├── Model Loading→ Wait/Poll│
              │  ├── No Capacity → Queue     │
              │  └── Overload   → 503        │
              └───────────────────────────────┘

   Background Controllers (горутины):
   ┌──────────────────────┐  ┌────────────────────────┐
   │ Prewarm Controller   │  │ Model Instance Controller│
   │ • Check load > 70%   │  │ • Maintain min/max      │
   │ • Find free backend  │  │ • Idle unload           │
   │ • Trigger pre-load   │  │ • Report metrics        │
   └──────────────────────┘  └────────────────────────┘
Для реализации потребуется создать plan_mode_respond с этим планом или переключиться в ACT MODE для начала
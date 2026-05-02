# Оптимальный механизм распределения запросов в OllamaLegion

## 1. Проблема: описание текущего состояния

### 1.1 Сценарий проблемы

```
Состояние кластера:

┌─────────────────────┐     ┌─────────────────────┐
│ Backend: gpu-1      │     │ Backend: gpu-2      │
│ VRAM: 24/24 GB      │     │ VRAM: 0/24 GB       │
│ Active: 8/8 (MAX!)  │     │ Active: 0/8         │
│ Models: [llama3.1]  │     │ Models: []          │
│ Status: OVERLOADED  │     │ Status: IDLE        │
└─────────────────────┘     └─────────────────────┘

Новый запрос: llama3.1 → ❌ ОЧЕРЕДЬ, хотя gpu-2 свободен!
```

**Корневая проблема:** gpu-2 ИМЕЕТ модель llama3.1 в available-списке (агент знает о ней),
но модель НЕ ЗАГРУЖЕНА в VRAM. Текущий `modelAffinity` НЕ учитывает этот
сценарий — он ищет только уже загруженные модели.

### 1.2 Каскад проблем

| # | Проблема | Причина | Последствия |
|---|----------|---------|-------------|
| 1 | Свободный бэкенд простаивает | Нет механизма «доступности модели без загрузки» | Очередь растёт при наличии ресурсов |
| 2 | Отсутствует redistribution | Session stickiness жёсткая | Перегруженный бэкенд не разгружается |
| 3 | Нет pre-warming | Модель загружается только по факту запроса | Холодный старт 10-60 сек |
| 4 | Нет упреждающей балансировки | Все решения реактивные | Неравномерная загрузка накапливается |
| 5 | Нет cost-based выбора | Все бэкенды равнозначны при score=0 | Неоптимальный выбор для дорогих операций |

---

## 2. Предлагаемый механизм: Multi-Factor Optimal Distribution Engine

### 2.1 Архитектура принятия решения

```text
                    ┌─────────────────────────────┐
                    │       ВХОДЯЩИЙ ЗАПРОС        │
                    │  model=M, clientIP, session  │
                    └─────────────┬───────────────┘
                                  │
                                  ▼
┌──────────────────────────────────────────────────────────────┐
│                   Decision Pipeline                          │
│                                                              │
│  ┌──────────┐   ┌──────────┐   ┌──────────┐   ┌──────────┐ │
│  │ 1. Filter│──▶│ 2. Score │──▶│ 3. Rank  │──▶│ 4. Check │ │
│  │ Health   │   │ Models   │   │ Backends │   │ & Assign │ │
│  │ + Cap    │   │ & Costs  │   │          │   │          │ │
│  └──────────┘   └──────────┘   └──────────┘   └──────────┘ │
│       │              │              │              │        │
│       ▼              ▼              ▼              ▼        │
│  Health check    Multi-factor    Weighted       Capacity +   │
│  + filters       model scoring   ranking        dispatch     │
└──────────────────────────────────────────────────────────────┘
```

### 2.2 Этап 1: Фильтрация бэкендов (Health + Capability Filter)

Каждый бэкенд классифицируется в одну из категорий:

```go
type BackendReadiness int
const (
    ReadyIdeal       BackendReadiness = 0  // Модель загружена, есть capacity
    ReadyColdStart   BackendReadiness = 1  // Модель доступна, нужна загрузка
    ReadyOverloaded  BackendReadiness = 2  // Модель загружена, НО active==max
    ReadyWaiting     BackendReadiness = 3  // Модель в очереди на загрузку
    NotReady         BackendReadiness = 4  // Нет модели/VRAM/здоровья
)
```

**Фильтры (порядок важен):**

| # | Фильтр | Условие | Действие |
|---|--------|---------|----------|
| 1 | Health | heartbeat свежий, все метрики < thresholds | Пропустить |
| 2 | Model Known | Модель есть в `availableModels` агента | Пропустить (даже если не в VRAM) |
| 3 | VRAM Fit | `vramFree >= modelSize * safetyFactor` | Пропустить |
| 4 | Disk Fit | `diskFree >= modelSize` (для загрузки) | Пропустить |
| 5 | Concurrent Fit | `active < maxConcurrent` | Пропустить |
| 6 | Model Slot Fit | `loadedModels < maxModels` ИЛИ модель уже загружена | Пропустить |

Выход: `[]FilteredBackend` с полем `Readiness`.

### 2.3 Этап 2: Multi-Factor Model Scoring

Для каждого отфильтрованного бэкенда вычисляется **Composite Score**:

```text
CompositeScore = Σ (FactorWeight_i × NormalizedFactor_i)
```

#### Факторы и веса (конфигурируемые):

```
┌─────────────────────────────┬────────┬──────────────────────────────┐
│ Фактор                      │  Вес   │ Описание                     │
├─────────────────────────────┼────────┼──────────────────────────────┤
│ 1. Load Balance Factor      │  25%   │ Насколько бэкенд разгружен  │
│ 2. Latency Prediction       │  20%   │ Прогнозируемое время ответа │
│ 3. Model Readiness          │  20%   │ Статус модели (готовность)  │
│ 4. Resource Efficiency      │  15%   │ Эффективность использования  │
│ 5. Session Affinity         │  10%   │ Привязка сессии             │
│ 6. Historical Performance   │  5%    │ История успешных запросов   │
│ 7. Predicted Future Load    │  5%    │ Прогноз загрузки через N сек │
└─────────────────────────────┴────────┴──────────────────────────────┘
```

#### 2.3.1 Load Balance Factor (25%)

```go
func loadBalanceFactor(be *Backend, all []*Backend) float64 {
    avgLoad := averageActiveRatio(all)  // среднее active/maxConcurrent по кластеру
    beLoad  := be.ActiveRequests / float64(be.MaxConcurrentRequests)
    
    // Нормализованное отклонение от среднего
    deviation := (avgLoad - beLoad) / max(avgLoad, 0.01)
    
    // Сигмоидная нормализация для сглаживания
    return sigmoid(deviation * 3.0)  // -> [0, 1]
}
```

**Смысл:** бэкенды с загрузкой НИЖЕ средней получают более высокий score.
Предотвращает «любимчиков» (один загружен, другие простаивают).

#### 2.3.2 Latency Prediction Factor (20%)

```go
func latencyPredictionFactor(be *Backend, modelSize float64) float64 {
    baseLatency := 0.0 // базовая задержка (ms)
    
    switch be.ModelReadiness {
    case ReadyIdeal:
        // Модель уже в VRAM — только время инференса
        baseLatency = estimateInferenceLatency(be.GpuModel, modelSize)
    case ReadyColdStart:
        // Нужна загрузка модели: diskRead + gpuTransfer + inference
        baseLatency = estimateLoadLatency(be.DiskSpeed, modelSize, be.PCIeBandwidth) +
                      estimateInferenceLatency(be.GpuModel, modelSize)
    case ReadyWaiting:
        // Модель уже загружается другим запросом
        baseLatency = estimateRemainingLoadTime(be) +
                      estimateInferenceLatency(be.GpuModel, modelSize)
    }
    
    // Добавить penalty за текущую загрузку GPU
    concurrencyPenalty := (be.ActiveRequests / float64(be.MaxConcurrentRequests)) * 
                          be.AvgInferenceLatency
    
    totalLatency := baseLatency + concurrencyPenalty
    
    // Инвертировать: чем МЕНЬШЕ задержка, тем ВЫШЕ score
    return 1.0 / (1.0 + totalLatency/1000.0)
}
```

**Параметры для оценки задержки загрузки:**

| Параметр | Метрика | Источник |
|----------|---------|----------|
| `diskSpeed` | MB/s чтения диска | Агент (iostat) |
| `pcieBandwidth` | GB/s PCIe шины | Конфиг/автоопределение |
| `gpuModel` | Модель GPU (A100, 4090, ...) | Агент (nvidia-smi) |
| `modelSize` | Размер модели в GB | Конфиг моделей |

#### 2.3.3 Model Readiness Factor (20%)

```go
func modelReadinessFactor(be *Backend) float64 {
    switch be.ModelReadiness {
    case ReadyIdeal:
        return 1.0    // Модель уже в VRAM — мгновенная готовность
    case ReadyWaiting:
        return 0.7    // Модель загружается — частичная готовность
    case ReadyColdStart:
        return 0.3    // Требуется холодный старт — низкая готовность
    default:
        return 0.0
    }
}
```

#### 2.3.4 Resource Efficiency Factor (15%)

```go
func resourceEfficiencyFactor(be *Backend, modelSize float64) float64 {
    // Насколько эффективно используется VRAM после загрузки этой модели?
    
    currentVramUsage := be.VramUsed / be.VramTotal
    projectedUsage  := (be.VramUsed + modelSize) / be.VramTotal
    
    // Штраф за фрагментацию: если остаток VRAM < 2*modelSize,
    // бэкенд не сможет загрузить ещё одну такую же модель
    fragmentationPenalty := 0.0
    if (be.VramTotal - be.VramUsed - modelSize) < 2*modelSize {
        fragmentationPenalty = 0.3
    }
    
    // Предпочтение бэкендам, где VRAM используется оптимально (60-80%)
    optimalUsage := 0.7
    usageScore := 1.0 - abs(projectedUsage - optimalUsage)
    
    return max(0, usageScore - fragmentationPenalty)
}
```

#### 2.3.5 Session Affinity Factor (10%)

```go
func sessionAffinityFactor(be *Backend, session *Session) float64 {
    if session == nil {
        return 0.5  // Нейтрально для новых сессий
    }
    if session.BackendID == be.ID {
        return 1.0  // Полный affinity
    }
    // Частичный affinity: тот же датацентр/регион/GPU-тип
    if be.DataCenter == session.PreferredDC {
        return 0.7
    }
    return 0.0
}
```

#### 2.3.6 Historical Performance Factor (5%)

```go
func historicalPerformanceFactor(be *Backend) float64 {
    // EWMA успешных/неуспешных запросов за последние N минут
    successRate := be.RecentSuccessCount / max(be.RecentTotalCount, 1)
    
    // Учитываем среднюю задержку относительно других бэкендов
    latencyRatio := clusterAvgLatency / max(be.AvgLatency, 0.001)
    
    return (successRate * 0.6) + (min(latencyRatio, 2.0) / 2.0 * 0.4)
}
```

#### 2.3.7 Predicted Future Load Factor (5%)

```go
func predictedFutureLoadFactor(be *Backend) float64 {
    // Использует существующий предиктор загрузки
    predictions := be.LoadPredictor.PredictNextN(be.LoadHistory, 3) // 3 шага вперёд
    
    // Если прогнозируется рост загрузки → снизить привлекательность
    futureLoad := average(predictions)
    return 1.0 - min(futureLoad, 1.0)
}
```

### 2.4 Этап 3: Weighted Ranking

```go
type RankedBackend struct {
    Backend      *Backend
    CompositeScore float64
    EstimatedLatency float64
    Readiness    BackendReadiness
    TieBreaker   float64
}

func rankBackends(filtered []*FilteredBackend, weights FactorWeights) []RankedBackend {
    ranked := make([]RankedBackend, len(filtered))
    
    for i, fb := range filtered {
        score := 
            fb.LoadBalance       * weights.LoadBalance +
            fb.Latency           * weights.Latency +
            fb.ModelReadiness    * weights.ModelReadiness +
            fb.ResourceEfficiency * weights.ResourceEfficiency +
            fb.SessionAffinity   * weights.SessionAffinity +
            fb.Historical        * weights.Historical +
            fb.PredictedLoad     * weights.PredictedLoad
        
        ranked[i] = RankedBackend{
            Backend:         fb.Backend,
            CompositeScore:  score,
            EstimatedLatency: estimateTotalLatency(fb),
            Readiness:       fb.Readiness,
            TieBreaker:      fb.Backend.LastUsed.UnixNano(), // для tie-breaking
        }
    }
    
    // Сортировка: выше score → раньше в списке
    sort.Slice(ranked, func(i, j int) bool {
        if abs(ranked[i].CompositeScore - ranked[j].CompositeScore) < 0.01 {
            return ranked[i].TieBreaker < ranked[j].TieBreaker // старше LastUsed
        }
        return ranked[i].CompositeScore > ranked[j].CompositeScore
    })
    
    return ranked
}
```

### 2.5 Этап 4: Capacity Check & Assignment

```go
func assignBackend(ranked []RankedBackend, modelSize float64, session *Session) (*Backend, error) {
    for _, rb := range ranked {
        be := rb.Backend
        
        switch rb.Readiness {
        case ReadyIdeal:
            // Идеальный случай — резервируем слот и возвращаем
            if be.ReserveSlot() {
                return be, nil
            }
            
        case ReadyColdStart:
            // Нужна загрузка — проверяем, не загружается ли уже
            if be.IsLoading(modelName) {
                // Другой запрос уже инициировал загрузку → ждём
                continue
            }
            
            // Проверяем глобальный лимит на параллельные загрузки
            if clusterWideLoads < maxParallelLoads {
                // Инициируем pre-load
                go be.PreLoadModel(modelName)
                clusterWideLoads++
                continue
            }
            
        case ReadyOverloaded:
            // Бэкенд загружен, но модель есть → кандидат на redistribution
            // (обрабатывается отдельным механизмом)
            continue
            
        case ReadyWaiting:
            // Модель уже загружается → можно ждать или пропустить
            if rb.EstimatedLatency < maxAcceptableLatency {
                continue // Подождём, когда загрузится
            }
        }
    }
    
    // Ни один бэкенд не подошёл → очередь
    return nil, ErrNoCapacity
}
```

---

## 3. Дополнительные механизмы

### 3.1 Proactive Model Pre-Warming

```text
Триггеры pre-warming:
┌─────────────────────────────────────────────────────────────┐
│ 1. Cluster Load > 70% → начать загрузку топ-3 моделей     │
│    на наименее загруженные бэкенды                         │
│                                                             │
│ 2. Модель M получила > N запросов за минуту →             │
│    поддерживать min_replicas копий в кластере              │
│                                                             │
│ 3. Scheduled pre-warm (cron: "0 8 * * 1-5") →              │
│    загружать модели к началу рабочего дня                  │
│                                                             │
│ 4. Predictive pre-warm: на основе исторических паттернов   │
│    (LSTM/ARIMA прогноз)                                    │
└─────────────────────────────────────────────────────────────┘
```

### 3.2 Session Redistribution (Graceful Migration)

```go
type RedistributionPolicy struct {
    TriggerThreshold    float64 // 0.8 = redistribute если бэкенд > 80% загружен
    CooldownPeriod     time.Duration // минимальное время между миграциями
    MaxMigrationsPerBatch int    // макс. сессий за одну redistribution-волну
    NewSessionOnly     bool    // только новые сессии или все
}

func (rm *RedistributionManager) Evaluate() {
    overloaded := findOverloadedBackends(rm.Threshold)
    
    for _, be := range overloaded {
        candidates := findUnderloadedBackends(be.Model, rm.Threshold/2)
        
        for _, session := range be.GetMigratableSessions(rm.MaxMigrationsPerBatch) {
            target := selectBestTarget(candidates, session)
            if target != nil {
                // 1. Создать новую сессию на target
                // 2. Уведомить клиента о смене бэкенда
                // 3. Обновить session.BackendID
                rm.Migrate(session, target)
            }
        }
    }
}
```

### 3.3 Cost-Aware Model Loading

```go
type ModelLoadCost struct {
    TimeCost          time.Duration  // время загрузки
    VramCost          float64        // занимаемая VRAM
    NetworkCost       float64        // стоимость сетевой передачи (если distributed FS)
    EvictionCost      float64        // стоимость вытеснения другой модели (LRU)
    OpportunityCost   float64        // стоимость блокировки бэкенда на время загрузки
}

func calculateLoadCost(be *Backend, modelName string) ModelLoadCost {
    modelSize := getModelSize(modelName)
    
    cost := ModelLoadCost{
        TimeCost: estimateLoadDuration(be, modelSize),
        VramCost: modelSize,
    }
    
    // Eviction cost: нужно ли выгружать другие модели?
    if be.VramFree < modelSize {
        victim := be.SelectLRUVictim(modelSize) // Least Recently Used
        if victim != nil {
            cost.EvictionCost = victim.Size + 
                float64(victim.ActiveRequests) * penaltyPerActiveRequest
        }
    }
    
    // Opportunity cost: сколько запросов могли бы обслужиться за время загрузки
    cost.OpportunityCost = float64(be.MaxConcurrentRequests) * 
                           cost.TimeCost.Seconds() / avgRequestDuration.Seconds()
    
    return cost
}
```

### 3.4 Adaptive Weight Tuning

```go
type AdaptiveWeights struct {
    // Веса автоматически корректируются на основе метрик эффективности
    Weights   FactorWeights
    History   []WeightSnapshot // история изменений
    
    // Метрики для адаптации
    TargetMetric string  // "p99_latency", "throughput", "utilization"
}

func (aw *AdaptiveWeights) Tune(metrics []ClusterMetrics) {
    // Каждые N минут оцениваем эффективность текущих весов
    // и слегка корректируем в сторону улучшения целевой метрики
    
    currentPerf := evaluateTargetMetric(aw.TargetMetric, metrics)
    delta := currentPerf - aw.History[len(aw.History)-1].Performance
    
    if delta > 0 {
        // Усиливаем направление
        for i := range aw.Weights {
            aw.Weights[i] += aw.History[len(aw.History)-1].Delta[i] * 0.1
        }
    } else {
        // Пробуем новое случайное направление (stochastic gradient descent)
        aw.Weights = perturbWeights(aw.Weights, 0.05)
    }
    
    normalizeWeights(&aw.Weights)
}
```

---

## 4. Решение исходной проблемы (gpu-1 перегружен, gpu-2 простаивает)

### 4.1 Пошаговый разбор

```text
Состояние:
  gpu-1: llama3.1 в VRAM, active=8/8, score loadBalance = 0.1
  gpu-2: llama3.1 доступна (в availableModels агента), НЕ в VRAM, active=0/8

Запрос llama3.1:

Этап 1 — Фильтрация:
  gpu-1: Health ✅ | Model Known ✅ | Concurrent ❌ (8/8)
         → Readiness = ReadyOverloaded
  gpu-2: Health ✅ | Model Known ✅ | VRAM Fit ✅ | Concurrent ✅
         → Readiness = ReadyColdStart

Этап 2 — Scoring:
  gpu-1: score = 0.1*0.25 + 0.0*0.20 + 1.0*0.20 + 0.5*0.15 + ... = 0.3
  gpu-2: score = 1.0*0.25 + 0.7*0.20 + 0.3*0.20 + 1.0*0.15 + ... = 0.6

Этап 3 — Ranking:
  1. gpu-2 (0.6)
  2. gpu-1 (0.3)

Этап 4 — Assignment:
  gpu-2 → ReadyColdStart → инициируем pre-load llama3.1
  → Запрос направляется на gpu-2
  → Модель загружается (10-30 сек)
  → Ответ стримится клиенту

Результат:
  gpu-1: продолжает обслуживать 8 запросов
  gpu-2: загружает llama3.1, обслуживает новый запрос
  ✅ Кластер работает сбалансированно!
```

### 4.2 Сравнение: текущее vs предлагаемое поведение

| Сценарий | Текущее поведение | Предлагаемое поведение |
|----------|-------------------|----------------------|
| Перегруженный бэкенд + свободный | Очередь (минуты ожидания) | Pre-load + dispatch (секунды) |
| 2 одинаковые модели на 2 бэкендах | Все на один → второй простаивает | Равномерное распределение |
| Новая модель, все свободны | Случайный выбор | Optimal по всем факторам |
| Повторный запрос сессии | Sticky → тот же бэкенд | Sticky, но с лимитом перегрузки |
| Конкуренция за VRAM | Очередь | LRU eviction + перебалансировка |

---

## 5. Конфигурация

### 5.1 Новые параметры конфигурации

```json
{
  "balancing": {
    "algorithm": "multi-factor-optimal",
    
    "factor_weights": {
      "load_balance": 0.25,
      "latency_prediction": 0.20,
      "model_readiness": 0.20,
      "resource_efficiency": 0.15,
      "session_affinity": 0.10,
      "historical_performance": 0.05,
      "predicted_future_load": 0.05
    },
    
    "pre_warming": {
      "enabled": true,
      "cluster_load_threshold": 0.70,
      "min_replicas_per_model": 1,
      "replica_scale_up_rps": 10,
      "scheduled_warmups": ["0 8 * * 1-5"]
    },
    
    "redistribution": {
      "enabled": true,
      "trigger_threshold": 0.80,
      "cooldown_period": "5m",
      "max_migrations_per_batch": 3,
      "new_sessions_only": false
    },
    
    "cost_aware_loading": {
      "enabled": true,
      "max_parallel_loads": 2,
      "load_timeout": "60s",
      "eviction_policy": "lru"
    },
    
    "adaptive_weights": {
      "enabled": false,
      "target_metric": "p99_latency",
      "tune_interval": "10m",
      "exploration_rate": 0.05
    },
    
    "latency_estimation": {
      "model_load_speed_mbps": 500,
      "pcie_bandwidth_gbps": 16,
      "safety_factor": 1.2
    }
  }
}
```

---

## 6. Что отсутствует для полного контроля и оптимизации

### 6.1 Данные, которые нужно собирать с агентов

| # | Метрика | Назначение | Текущий статус |
|---|---------|------------|----------------|
| 1 | `diskReadSpeed` (MB/s) | Оценка времени загрузки модели | ❌ Нет |
| 2 | `pcieBandwidth` (GB/s) | Оценка времени передачи на GPU | ❌ Нет |
| 3 | `avgInferenceLatency` (ms/токен) | Оценка задержки инференса | ❌ Нет |
| 4 | `loadedModelsHistory` (с временем загрузки) | История для предиктора | ❌ Нет |
| 5 | `gpuTemperature` (°C) | Троттлинг-детекция | ❌ Нет |
| 6 | `availableModels` (полный список с размерами) | Pre-warming decisions | ⚠️ Частично |
| 7 | `modelUsageStats` (RPS per model) | Решения о репликации | ❌ Нет |
| 8 | `queueDepth` и `waitTime` | Оценка congestion | ⚠️ Только на балансере |

### 6.2 Алгоритмические компоненты, требующие реализации

| # | Компонент | Сложность | Зависимости |
|---|-----------|-----------|-------------|
| 1 | Multi-factor scorer | Средняя | Новые метрики агента |
| 2 | Latency predictor | Средняя | diskSpeed, pcieBandwidth |
| 3 | Pre-warming engine | Средняя | availableModels, usage stats |
| 4 | Redistribution manager | Высокая | Session state, streaming handling |
| 5 | LRU eviction controller | Средняя | Ollama unload API |
| 6 | Adaptive weight tuner | Высокая | Длительный сбор метрик |
| 7 | Cost calculator | Низкая | modelSize map |

### 6.3 Интеграционные точки

| # | Точка интеграции | Что требуется |
|---|-----------------|---------------|
| 1 | Ollama API | `POST /api/show` для получения размера модели |
| 2 | Агент | Расширить heartbeat: `diskSpeed`, `pcieBandwidth`, `avgLatency`, `modelSizes` |
| 3 | Конфигурация | Добавить секции `pre_warming`, `redistribution`, `factor_weights` |
| 4 | WebUI монитор | Секция «Распределение моделей», индикатор pre-warming |
| 5 | Балансер | Рефакторинг `selectBackend()` → pipeline |

---

## 7. План внедрения (поэтапный)

### Фаза 1: Foundation (1-2 недели)
- Расширить агент: сбор `diskSpeed`, `pcieBandwidth`, `avgInferenceLatency`
- Расширить тип `Backend`: `AvailableModels` с размерами, `Readiness`
- Реализовать базовый multi-factor scorer (факторы 1-4)
- Добавить конфигурацию `factor_weights`

### Фаза 2: Cold Start Optimization (1-2 недели)
- Реализовать `ReadyColdStart` логику в `selectBackend()`
- Добавить `modelSizes` map (из Ollama API `/api/show`)
- Реализовать cost calculator для загрузки модели
- Глобальный лимит параллельных загрузок

### Фаза 3: Pre-Warming (1 неделя)
- Load-threshold триггер
- Min replicas enforcement
- Scheduled warmups (cron)

### Фаза 4: Redistribution (2-3 недели)
- Session migration
- Graceful handoff (для streaming ответов)
- Cooldown и batch-лимиты

### Фаза 5: Advanced (опционально)
- Adaptive weight tuning
- Predictive pre-warming (ML-модель)
- Multi-DC awareness

---

## 8. Ожидаемые результаты

| Метрика | Текущее | Целевое | Улучшение |
|---------|---------|---------|-----------|
| Время ожидания в очереди (p95) | 120s | 15s | 8x |
| Утилизация GPU (средняя) | 45% | 75% | 1.7x |
| Время холодного старта (p50) | N/A (всегда очередь) | 25s | ∞ |
| Неравномерность загрузки (stddev) | 0.40 | 0.10 | 4x |
| Пропускная способность кластера | 100 RPS | 180 RPS | 1.8x |

---
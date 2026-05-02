"""Atomic replace section 2 in balancing-guide.md (bypass VS Code lock)"""
import os, tempfile

path = r'c:\Ollama\ollamalegion\docs\balancing-guide.md'

with open(path, 'r', encoding='utf-8') as f:
    data = f.read()

old_start = '## 2. Алгоритм выбора бэкенда'
idx_start = data.find(old_start)
if idx_start < 0:
    print('ERROR: section 2 start not found')
    exit(1)

next_section = data.find('\n## ', idx_start + len(old_start))
if next_section < 0:
    print('ERROR: next section not found')
    exit(1)

new_section = '''## 2. Алгоритм выбора бэкенда (v2)

### 2.1 Многоэтапный выбор (5 этапов)

Метод `selectBackend()` в `internal/balancer/proxy.go` реализует 5-этапную логику:

```text
          Request arrives
                    │
                    ▼
    ┌───────────────────────────────────────┐
    │           selectBackend(model)         │
    │                                        │
    │  Этап 1: Model Affinity (LOADED only) │
    │    ├─ Найти бэкенд с моделью в памяти  │
    │    ├─ Проверить loadRatio < 80%       │
    │    └─ Если ок → вернуть бэкенд        │
    │                                        │
    │  Этап 2: Model Warming (WARMING_UP)   │
    │    ├─ Найти бэкенд в процессе загрузки │
    │    ├─ Проверить ETA < timeout         │
    │    └─ Ожидать готовности (polling)    │
    │                                        │
    │  Этап 3: Sync Model Load              │
    │    ├─ Найти свободный бэкенд          │
    │    ├─ Запустить загрузку модели        │
    │    └─ Ожидать готовности (с таймаутом) │
    │                                        │
    │  Этап 4: Resource-Aware Scoring (v2)  │
    │    └─ calculateScore() с новыми весами │
    └──────────────────┬────────────────────┘
                       ▼
    ┌───────────────────────────────────────┐
    │           Decision Router             │
    │  ┌── Backend Ready → Проксировать     │
    │  ├── Model Loading → Wait/Poll        │
    │  ├── Нет Capacity → В очередь         │
    │  └── Queue > 90% → 503 (Backpressure) │
    └───────────────────────────────────────┘

   Фоновые контроллеры (горутины):
   ┌──────────────────────────┐  ┌────────────────────────────┐
   │ Prewarm Controller       │  │ Model Instance Controller  │
   │ • Проверка load > 70%    │  │ • Поддержка min/max        │
   │ • Поиск свободного       │  │ • Idle unload (> 10 мин)   │
   │ • Триггер pre-load       │  │ • Мониторинг экземпляров   │
   │ • Каждые 10 сек          │  │ • Каждые 30 сек            │
   └──────────────────────────┘  └────────────────────────────┘
```

### 2.2 Расширенная формула скоринга (calculateScore v2)

Формула v2 учитывает состояние загрузки модели, глубину очереди и историю ошибок:

```
Score = (
    gpuFreePercent        * 0.30  +    // 30% — GPU загрузка
    vramFreePercent       * 0.20  +    // 20% — свободная VRAM
    cpuFreePercent        * 0.15  +    // 15% — свободный CPU
    modelLoadedBonus      * 0.15  +    // 15% — бонус за загруженные модели
    predictionBonus       * 0.10  +    // 10% — бонус предиктора
    modelCapacityScore    * 0.10  +    // 10% — ёмкость под модель
    − requestPenalty                   // штраф за активные запросы
    − queueDepthPenalty    * 0.05  −   // 5%  — штраф за глобальную очередь
    − errorRatePenalty     * 0.05      // 5%  — штраф за историю ошибок
) * weight                            // Мультипликатор веса бэкенда
```

**Новые компоненты v2:**
- **modelLoadedBonus** — чем больше моделей уже загружено, тем выше score
- **queueDepthPenalty** — штраф за глубину глобальной очереди
- **errorRatePenalty** — штраф за высокую частоту ошибок

### 2.3 Headroom Reservation (Ресурсный резерв)

На каждом бэкенде резервируется 15% GPU памяти как горячий резерв.
Бэкенд блокируется для новых запросов если VRAM usage > (100 − headroom_percent)%.

Конфигурация: `balancing.resource_reservation.gpu_headroom_percent: 15`.

### 2.4 Фильтры здоровья (дополненные)

Бэкенд считается healthy, если:
- Последний heartbeat не старше таймаута
- GPU usage < `gpuMaxUsage` (default 90%)
- VRAM usage < `vramMaxUsage` (default 85%)
- CPU usage < `cpuMaxUsage` (default 80%)
- RAM usage < `ramMaxUsage` (default 85%)
- Диск free > `minFreeDisk` (default 10240 MB)
- **VRAM usage не превышает headroom-резерв (default 85%)**

'''

# Build new content: prefix + new_section + rest
new_data = data[:idx_start] + new_section + data[next_section:]

# Write atomically: temp file + rename
tmp_fd, tmp_path = tempfile.mkstemp(dir=os.path.dirname(path), suffix='.tmp')
try:
    with os.fdopen(tmp_fd, 'w', encoding='utf-8') as f:
        f.write(new_data)
    os.replace(tmp_path, path)
    print('OK: Section 2 updated atomically')
except Exception as e:
    os.unlink(tmp_path)
    print(f'ERROR: {e}')
    exit(1)
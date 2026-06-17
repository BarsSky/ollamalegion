# План реализации: Интеллектуальная загрузка моделей с распределением GPU/CPU

## Этап 1: CalculateOptimalGPULayers (backend.go)
- [ ] Добавить метод CalculateOptimalGPULayers в Backend
- [ ] Учитывает доступную VRAM + RAM
- [ ] Бинарный поиск оптимальных GPU-слоёв
- [ ] Возвращает ошибку с диагностикой

## Этап 2: Модификация checkVRAMForModel
- [ ] Вместо жёсткой проверки использовать CalculateOptimalGPULayers
- [ ] Автоматически подбирать gpuLayers если не влезает

## Этап 3: Расширение tryRamFallbackReload
- [ ] Учитывать GPU OOM тоже для fallback
- [ ] Использовать пересчёт слоёв вместо фиксированного gpuLayers

## Этап 4: Context-aware загрузка через API
- [ ] Новый endpoint /api/models/load-with-params
- [ ] Прокси через балансировщик

# План рефакторинга фронтенда

## Цели
1. Убрать дублирование между monitor.html, app.js и общими компонентами
2. Выделить инлайн CSS из monitor.html в отдельный файл `webui/css/monitor.css`
3. Выделить инлайн JS из monitor.html в модули `webui/js/modules/monitor-*.js`
4. Обеспечить использование общей i18n системы
5. Создать документацию по архитектуре фронтенда

## Структура после рефакторинга

```
webui/
├── index.html                 # Главная страница (балансер)
├── monitor.html              # Страница мониторинга
├── css/
│   ├── base.css
│   ├── components.css
│   ├── data.css
│   ├── font-awesome.min.css
│   ├── icons.css
│   ├── layout.css
│   ├── metrics.css
│   ├── monitor.css           # НОВЫЙ: стили мониторинга
│   ├── pages.css
│   ├── responsive.css
│   └── themes.css
├── js/
│   ├── app.js                # Логика index.html (очищена от дублирования)
│   ├── monitor-common.js     # Уже существует, используется обоими
│   ├── i18n/
│   │   ├── index.js          # Общая i18n система
│   │   ├── en.js / en.json
│   │   └── ru.js / ru.json
│   └── modules/
│       ├── api.js            # API вызовы (общие)
│       ├── config.js         # Конфигурация (общие)
│       ├── renderers.js      # Рендеринг (для index.html)
│       ├── utils.js          # Утилиты (общие)
│       ├── websocket.js      # WebSocket (общие)
│       ├── monitor-core.js   # НОВЫЙ: ядро мониторинга
│       ├── monitor-charts.js # НОВЫЙ: графики мониторинга
│       ├── monitor-metrics.js # НОВЫЙ: метрики мониторинга
│       └── monitor-backends.js # НОВЫЙ: отображение бэкендов
└── img/
```

## Задачи
- [x] 1. Создать `webui/css/monitor.css` (стили для модулей монитора)
- [x] 2. Удалить устаревший `webui/js/modules/monitor-core.js` (заменён на специализированные модули)
- [x] 3. Создать `webui/js/modules/monitor-charts.js` (Canvas 2D графики RPS/latency/GPU/VRAM)
- [x] 4. Создать `webui/js/modules/monitor-metrics.js` (WebSocket + DOM sync)
- [x] 5. Создать `webui/js/modules/monitor-backends.js` (сортировка/фильтрация таблицы + tooltips)
- [x] 6. Обновить `monitor.html` (подключить модули, data-sort атрибуты, фильтр)
- [x] 7. Интегрировать модули в существующую монитор-логику (`js/monitor/`)
- [ ] 8. Создать `docs/frontend-architecture-ru.md` (отложено)

## Порядок выполнения (выполнено 2026-05-07)
1. ✅ Создан CSS файл `webui/css/monitor.css` со стилями для charts, sorting, filtering
2. ✅ Созданы JS модули: monitor-charts.js, monitor-metrics.js, monitor-backends.js
3. ✅ Обновлён `monitor.html` — подключены модули, добавлен data-sort, фильтр
4. ✅ Удалён устаревший `monitor-core.js`
5. ✅ Интеграция с существующими `js/monitor/*.js` — модули загружаются перед legacy кодом
6. ⏸️ Документация frontend-architecture — отложена, не критична

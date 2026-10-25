# OllamaLegion Design System (TASTE-compliant)

Версия: 2026-06-29 (дополнение: data-dense variant). Основано на [taste-skill v2 (design-taste-frontend)](https://github.com/leonxlnx/taste-skill).

## 1. TASTE-dials

| Dial | Значение | Смысл |
|---|---|---|
| DESIGN_VARIANCE | 5 | Структурированная dashboard-сетка с лёгкой асимметрией в metric-cards |
| MOTION_INTENSITY | 3 | Только hover-переходы; нет parallax, perpetual loops, bouncy springs |
| VISUAL_DENSITY | 5 | Medium-dense: виджеты на одном экране, но с воздухом между секциями |

## 2. Design Tokens (CSS variables)

### 2.1 Spacing scale (`--space-0`..`--space-9`)

Multiples of 4. Использовать вместо голых px:

| Token | px | Назначение |
|---|---|---|
| `--space-0` | 0 | reset, zero-spacing |
| `--space-1` | 4 | tight icon gaps |
| `--space-2` | 8 | element internal padding |
| `--space-3` | 12 | badge / small button padding |
| `--space-4` | 16 | card inner padding |
| `--space-5` | 20 | medium card padding |
| `--space-6` | 24 | section spacing, page padding |
| `--space-7` | 32 | modal header height |
| `--space-8` | 48 | hero spacing |
| `--space-9` | 64 | top-level page gap |

### 2.2 Corner radius scale (`--radius-xs`..`--radius-full`)

| Token | px | Назначение |
|---|---|---|
| `--radius-xs` | 4px | progress bars, mini-dots |
| `--radius-sm` | 6px | pills, badges, small buttons |
| `--radius-md` | 8px | inputs, cards, dropdowns |
| `--radius-lg` | 12px | modals, large cards |
| `--radius-xl` | 16px | hero panels |
| `--radius-full` | 9999px | circular avatars |

**Правило shape consistency lock (taste 4.4):** одна и та же семья элементов
всегда использует один и тот же уровень `--radius-`. Не смешивать.

### 2.3 Elevation (`--shadow-sm`..`--shadow-xl`)

```
--shadow-sm: 0 1px 2px rgba(0,0,0,0.06);   /* hairline borders */
--shadow-md: 0 4px 12px rgba(0,0,0,0.10);  /* default cards */
--shadow-lg: 0 8px 24px rgba(0,0,0,0.18);  /* hovered cards, popovers */
--shadow-xl: 0 24px 64px rgba(0,0,0,0.30); /* modal overlay */
```

### 2.4 Typography

```
--font-family-sans: -apple-system, BlinkMacSystemFont, "Segoe UI Variable Text",
                     "Segoe UI Variable", "Segoe UI", "Inter",
                     "Helvetica Neue", system-ui, sans-serif;
--font-family-mono: "SF Mono", "JetBrains Mono", "Geist Mono",
                     ui-monospace, "Consolas", monospace;
```

Формы (`button`, `input`, `select`, `textarea`) НЕ наследуют шрифт от
UA-стилей браузера: без явного `font-family: inherit` они рисуются Arial и
выглядят «угловато» рядом с системным sans. Наследование объявлено один раз в
`webui/css/base.css`, дублировать его в компонентах не нужно.

Font-size scale:

| Token | px | Назначение |
|---|---|---|
| `--font-size-xs` | 11 | labels (uppercase), table headers |
| `--font-size-sm` | 13 | small body, button text |
| `--font-size-base` | 14 | default body |
| `--font-size-md` | 16 | card titles |
| `--font-size-lg` | 18 | section headers (h3) |
| `--font-size-xl` | 22 | page titles (h2) |
| `--font-size-2xl` | 28 | hero numbers |
| `--font-size-3xl` | 36 | dashboard metric-value |
| `--font-size-4xl` | 44 | hero text |

Line-height / font-weight / letter-spacing — токены в `--line-height-*`,
`--font-weight-*`, `--letter-spacing-*`.

### 2.6 Component geometry (R239)

Высота контролов и геометрия карточек — тоже токены, а не числа по месту.
Определены в `themes.css` (`:root`), переопределяются в `[data-density="dense"]`.

| Token | Normal | Dense | Назначение |
|---|---|---|---|
| `--control-height-sm` | 30px | 26px | компактный ряд: шапка страницы, шапка карточки, поля панели объединения |
| `--control-height` | 38px | 32px | обычные кнопки и поля форм |
| `--control-height-lg` | 44px | 38px | главное действие (wizard, крупные CTA) |
| `--card-radius` | `--radius-lg` | `--radius-md` | все `.card` |
| `--card-padding` | `--space-5` | `--space-4` | `.card-body` |
| `--card-header-padding` | `--space-4 --space-5` | `--space-3 --space-4` | `.card-header` |

Правило: новый контрол получает высоту из шкалы, а не из `padding`. Фиксировать
`height` в px в компоненте нельзя - сломается dense-режим.

### 2.7 Polish layer (`webui/css/polish.css`)

Подключается последним в `index.html`. Задача - выровнять то, что исторически
рисовалось по месту (карточки, таблицы, бейджи, шапка, панель объединения
бэкендов). Слой:

- не вводит новых цветов и теней, только токены;
- держит один вертикальный ритм: `.card` / `.card-header` / `.card-body`;
- выравнивает высоты контролов в шапке страницы и шапке карточки
  (`--control-height-sm`), чтобы ряд читался как ряд;
- переносит действия шапки на вторую строку на узком десктопе (≤1200px)
  вместо сжатия заголовка страницы до «Доступ...»;
- убирает «хвост» уехавшего сайдбара и не даёт гостевой полосе `#authBanner`
  перекрывать карточки на телефоне.

Новое правило сначала проверяется живым аудитом (раздел 8): если аудит
показывает нарушение - правило должно быть в `polish.css` (или в токенах), а не
точечным `style="..."` в разметке.

### 2.5 Colors (theme-controlled)

```
--accent (single accent per theme)
--success / --warning / --danger / --info
--text-primary / --text-secondary / --text-muted
--bg-primary / --bg-secondary / --bg-hover
--border / --border-strong / --border-subtle
--glass-bg / --glass-border / --glass-blur / --glass-shadow
--accent-soft / --warning-soft / --danger-soft / --info-soft / --success-soft
--accent-glow-soft / --accent-glow-ring / --accent-glow-shadow
```

Dark и Light темы определены в `webui/css/themes.css`. Anti-lila rule
(taste 3.D): никакого AI-purple-glow на нейтральных поверхностях.

## 3. Anti-slop rules

### 3.1 Эмодзи в коде

- В HTML/i18n user-facing тексте: только FA-иконки через `<i class="fas fa-...">`
  + лейбл. Эмодзи в коде запрещены (taste 3.D).
- В mode-card-icon / nav-icon / sidebar: только FA-классы.
- В footer / about / brand: только FA или текст.

Исключения:
- Языковые флаги в `lang-select` (`🇷🇺 RU`, `🇬🇧 EN`) — широко используемые
  Unicode-индикаторы языка (CLDR), НЕ SLOP, оставлены.

### 3.2 Никакого em-dash (--)

В комментариях и user-facing copy. В URL и `data-i18n-key`
можно (но не в тексте).

Заменитель: " - " (hyphen-minus-space) или перефразировать.

### 3.3 Никаких хардкодных rgba() в CSS-стилях

Все цвета через `var(--*-soft)`, `var(--accent-glow-*)` и т.д.

Legacy aliases в `themes.css` оставлены для backward-compat.

### 3.4 Eyebrow restraint

`@media (prefers-reduced-motion: reduce)` уважается во всех keyframes-файлах.

## 4. Files audit (2026-06-29)

| File | TASTE-статус | Комментарии |
|---|---|---|
| `themes.css` | OK | Полный TASTE-рефактор: dial-комменты, design tokens, anti-lila |
| `layout.css` | OK | Single-line nav, header cap 64px, no em-dash |
| `base.css` | OK | typography scale, --font-family-sans, no rgba in gradients |
| `metrics.css` | OK | radius/font/spacing токены, no em-dash |
| `data.css` | OK | radius/font/spacing токены, no em-dash |
| `pages.css` | OK | mono + soft tokens applied to log/method badges |
| `components.css` | partial | header + .btn done, остальное — section-by-section по мере рефактора |
| `index.html` | partial | filter buttons, copy btn — done; footer / auto-detect / some mode-card icons — TODO |
| `i18n/ru.js` | TBD | emoji из user-facing строк вычистить следующим шагом |
| `i18n/en.js` | TBD | то же |

## 5. Acceptance criteria

- Все CSS-файлы используют CSS-переменные для color/radius/font/spacing.
- Никаких hardcoded `rgba()` в новых стилях.
- Никакого em-dash в коде или комментариях (за исключением URL).
- `prefers-reduced-motion` обрабатывается в каждом keyframes-файле.
- HTML не содержит emoji в user-facing тексте (FA-иконки или пустой `<i>`).
- Build OK: `go build -tags llama_stub ./cmd/cppworker ./cmd/balancer`.
- Tests OK: smoke-набор не сломан (`cmd/cppworker lazy_load_calc` + классы).

## 6. Pre-flight check

Запустите `scripts/check_taste_compliance.ps1` (Windows) или ручной grep:

```powershell
# em-dash в webui
Select-String -Path webui\*.html, webui\*.js, webui\css\*.css -Pattern '—' -List

# hardcoded rgba в webui/css
Select-String -Path webui\css\*.css -Pattern 'rgba\(' | Where-Object { $_.Line -notmatch '/* legacy' -and $_.Line -notmatch 'var\(' }
```

## 7. Data-dense variant (Sprint 1, 2026-06-29)

Вариант дизайн-системы для **плотных data-heavy экранов**: Monitor (графики), GGUF grid (50+ моделей), Backend cards (метрики), Logs tab (real-time лог-лента).

### 7.1 Включение

```html
<body data-density="dense">  <!-- compact -->
<body>                        <!-- default (normal) -->
```

Toggle в WebUI: кнопка в header (рядом с theme toggle). Состояние сохраняется в `localStorage["ollamalegion_density"]` (`"normal"` или `"dense"`), на `<body>` ставится атрибут `data-density`. По умолчанию — `"normal"`.

### 7.2 Отличия от default

| Параметр | Normal | Dense | Δ |
|---|---|---|---|
| `--font-size-xs` | 11px | 10px | -9% |
| `--font-size-sm` | 13px | 12px | -8% |
| `--font-size-base` | 14px | 13px | -7% |
| `--font-size-lg` | 18px | 16px | -11% |
| `--font-size-xl` | 22px | 18px | -18% |
| `--font-size-2xl` | 28px | 22px | -21% |
| `--space-1` | 4px | 2px | -50% |
| `--space-2` | 8px | 4px | -50% |
| `--space-3` | 12px | 6px | -50% |
| `--space-4` | 16px | 8px | -50% |
| `--space-5` | 20px | 12px | -40% |
| `--space-6` | 24px | 16px | -33% |

**Шрифт чисел:** в dense-режиме все numeric values (RPS, latency, VRAM, temperature, tokens/sec) рендерятся через `font-family: var(--font-family-numeric)` (= Geist Mono fallback chain).

### 7.3 Применение

Все компоненты, где уже используются `var(--space-*)` и `var(--font-size-*)` **автоматически** переходят в dense-режим без дополнительных правил. Примеры:
- `.backend-card` — padding 16px → 8px, font 14px → 13px
- `.model-card` — то же
- `.metric-value` — font-size-lg → font-size-md (16px → 14px)
- `.table-row` — height 48px → 32px

### 7.4 Accessibility

- Минимальный `--font-size-xs` = **10px** в dense (не меньше, чтобы сохранить WCAG AA readability)
- `--font-size-base` = 13px в dense (на грани, но допустимо для dense-UI)
- `prefers-reduced-motion` всё ещё соблюдается

### 7.5 Anti-slop в dense

- ❌ Никаких AI-purple-glow на dense cards (anti-lila rule)
- ❌ Никаких emoji в user-facing тексте (только FA-иконки)
- ❌ Никакого em-dash в комментариях
- ✅ Моноширинный шрифт **только** для чисел (не для всего текста — иначе будет выглядеть как терминал)

### 7.6 Acceptance criteria

1. Toggle в WebUI переключает `body[data-density]` между `normal` / `dense`
2. CSS variables в `[data-density="dense"]` селекторе определены в `themes.css`
3. `.backend-card`, `.model-card`, `.metric-value` автоматически используют новые variables
4. После reload страницы состояние сохраняется (localStorage)
5. Visual regression: скриншот `normal` ≠ скриншот `dense` (Playwright)
6. Build OK: `go build -tags llama_stub ./cmd/balancer/`, `node -c webui/js/...`
7. Tests OK: `go test -tags llama_stub ./...` — 0 regressions

## 8. Live layout audit (R239)

Статический линт (`check_taste_compliance.ps1`) видит только исходники и не
отвечает на вопрос «а как это нарисовалось». Для верстки есть живой аудит:
`scripts/webui-audit-layout.js` (Playwright, Edge/Chromium).

Что измеряет на каждой странице (и в `monitor.html`, если включён):

| Проверка | Как читается | Уровень |
|---|---|---|
| Горизонтальный оверфлоу | `document.scrollWidth` vs `innerWidth` + виновники | FAIL |
| Шрифт текста | первый шрифт элемента из стека `--font-family-sans` (mono разрешён) | FAIL |
| Заголовок страницы | `.header h1` не переносится на вторую строку | FAIL |
| Высота контролов | `--control-height-sm/base/lg` (допуск 1px) | WARN |
| Кегль | только значения из шкалы `--font-size-*` | WARN |
| Радиус карточек | `--card-radius` / `--radius-md` / `--radius-lg` | WARN |
| Подпись и поле | подпись стоит НАД полем, а не в одной строке слева | WARN |
| Паддинги карточек | распределение (справочно, для ритма) | INFO |

Запуск:

```powershell
# локальный живой стенд (логин из deployments/.env, LB_ADMIN_USER/LB_ADMIN_PASSWORD)
node scripts/webui-audit-layout.js

# демо-стенд без входа
$env:WEBUI='http://100.64.0.9/'; node scripts/webui-audit-layout.js

# компактный режим, светлая тема, телефон, скриншоты, монитор
$env:DENSITY='dense'; $env:AUDIT_THEME='light'; node scripts/webui-audit-layout.js
$env:WIDTH='390'; $env:HEIGHT='844'; $env:SHOTS='1'; node scripts/webui-audit-layout.js
$env:SHOTS='1'; $env:AUDIT_MONITOR='1'; node scripts/webui-audit-layout.js
```

Результат: отчёт `dist/audit-layout/report.json` (+ PNG при `SHOTS=1`).
`FAIL` даёт код возврата 1, `WARN` - только запись в отчёт.

Порядок работы:

1. аудит до правки (фиксируем реальные нарушения, а не «на глаз»);
2. правка в токенах/`polish.css`, без инлайновых `font-size`/`height`;
3. аудит после правки в 2-3 режимах: 1440 normal, 390 mobile, 1024 light или dense;
4. скриншоты проблемных страниц просматриваются глазами - аудит не заменяет
   взгляд, он ловит то, что глазом легко пропустить.

Аудит не заменяет `tests/visual/*.spec.js` (Playwright-эталоны): эталоны
защищают от регрессий по пикселям, аудит объясняет, **что именно** поехало.

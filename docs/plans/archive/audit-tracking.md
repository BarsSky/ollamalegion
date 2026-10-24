# Отслеживание исправлений аудита
## Дата: 2026-04-28 (обновлено: 2026-05-08)

| № | Приоритет | Проблема | Файл | Статус | Дата исправления |
|---|-----------|----------|------|--------|------------------|
| 1 | P0 | Недетерминированный Master Token | `internal/api/auth.go` | [x] | 2026-04-28 |
| 2 | P0 | Утечка resp.Body в handleDelete | `internal/balancer/ollama_router.go` | [x] | 2026-04-28 |
| 3 | P0 | Избыточная загрузка TLS | `cmd/balancer/main.go` | [x] | 2026-04-28 |
| 4 | P1 | Устаревшие порты в CHANGELOG | `CHANGELOG.md` | [x] | 2026-04-28 |
| 5 | P1 | Ошибочный порт в DEPLOYMENT.md | `DEPLOYMENT.md` | [x] | 2026-04-28 |
| 6 | P1 | Разные форматы интервалов | `docker-compose.yml`, `agent.example.env` | [x] | 2026-05-01 |
| 7 | P1 | Противоречие NVML env vars | `docker-compose.agent.yml`, `agent.example.env` | [x] | 2026-04-28 |
| 8 | P2 | WebUI Models VRAM/RAM | `webui/js/app.js`, `pkg/types` | [x] | 2026-05-07 |
| 9 | P2 | WebUI Queue пустая таблица | `webui/js/app.js`, `internal/api/handlers.go` | [x] | 2026-05-07 |
| 10 | P2 | loadSettings placeholder | `webui/js/app.js` | [x] | 2026-05-07 |

## Статус
- **Все 10/10 проблем исправлены** — аудит полностью закрыт.
- Дата последнего обновления: 2026-05-08

## Правила обновления
- `[ ]` — не исправлено
- `[x]` — исправлено
- При исправлении добавлять дату в формате YYYY-MM-DD

# Вход в WebUI: кто и где задаёт логин и пароль

Дата: 2026-11-02. Повод: при переносе образа WebUI на другую машину и запуске
контейнера с новыми `-e`-значениями вход переставал работать «непонятно почему».

## Короткий ответ

**Логин и пароль принадлежат БАЛАНСЕРУ, а не контейнеру WebUI.**

- WebUI — это статика (nginx) + прокси: он отдаёт страницы и проксирует
  `/api/...` на балансер. Своих пользователей у него нет, `LB_ADMIN_USER` /
  `LB_ADMIN_PASSWORD` в его окружении **не читаются**.
- Проверка пароля и выдача cookie-сессии живут в балансере
  (`internal/api/auth_session.go`), пользователи хранятся в
  `/app/data/users.json` (переменная `LB_USERS_FILE`) на томе
  **`ol-balancer-data`**.

Отсюда правило: новые имя/пароль надо задавать **балансеру**, а WebUI нужен
только адрес балансера и API-токен.

## Что задавать кому

| Переменная | Кому | Что делает |
| --- | --- | --- |
| `LB_ADMIN_USER`, `LB_ADMIN_PASSWORD` | **балансер** | первый администратор; при `LB_ADMIN_ENV_SYNC=1` (по умолчанию) ещё и переопределяет пароль в существующем `users.json` |
| `LB_ADMIN_ENV_SYNC` | **балансер** | `1` (деф.) — env побеждает файл; `0` — существующих пользователей не трогать (тогда новый пароль из env НЕ применится) |
| `LB_USERS_FILE`, `LB_SESSIONS_FILE` | **балансер** | пути; в compose это `/app/data/users.json` и `/app/data/sessions.json` на томе `ol-balancer-data` |
| `CPPWORKER_API_TOKEN` | все | общий API-токен (аварийный вход и внутренние вызовы; в WebUI приходит как `API_TOKEN`) |
| `LB_API_URL` | **WebUI** | адрес балансера, `http://<host>:18081`. R391: теперь реально читается (раньше compose его передавал, а entrypoint игнорировал) |
| `API_HOST`, `API_PORT` | **WebUI** | то же адресом по частям; дефолт `loadbalancer:18081` — имя из compose-сети |
| `API_TOKEN` (или `WEBUI_API_TOKEN`, `LB_API_TOKEN`, `BALANCER_API_TOKEN`) | **WebUI** | токен, которым entrypoint проверяет доступ и который UI предлагает как аварийный вход |

## Сценарии

### A. Compose-стек (штатный)

`deployments/.env`:
```
LB_ADMIN_USER=skyadmin
LB_ADMIN_PASSWORD=...
CPPWORKER_API_TOKEN=...
```
Балансер создаёт `/app/data/users.json` на томе `ol-balancer-data`; WebUI
получает `LB_API_URL=http://loadbalancer:18081` внутри compose-сети. Ничего
дополнительно создавать не нужно.

### B. Перенос на другую машину (`docker run`, без compose-сети)

Имя `loadbalancer` внутри чужой сети не резолвится — WebUI проксировал в никуда,
и экран входа не мог получить ответ ни с каким паролем. Нужны два контейнера:

```
# 1) балансер — он же хранит учётки
docker run -d --name balancer \
  -p 18081:18081 -p 18080:18080 \
  -v ol-balancer-data:/app/data \
  -e LB_ADMIN_USER=admin -e LB_ADMIN_PASSWORD='новый-пароль' \
  -e CPPWORKER_API_TOKEN='<токен>' \
  ollama-legion/balancer:<tag>

# 2) WebUI — только адрес балансера + токен
docker run -d --name webui -p 18083:80 \
  -e LB_API_URL=http://<host-балансера>:18081 \
  -e API_TOKEN='<тот же токен, что CPPWORKER_API_TOKEN>' \
  ollama-legion/webui:<tag>
```

Том `-v ol-balancer-data:/app/data` **обязателен**: без него `users.json`
создаётся внутри контейнера и пропадает при пересоздании — «пароль сработал
один раз и перестал».

### C. Перенесли образ вместе со старым томом/файлом

Если `users.json` приехал со старой машины, в нём старый пароль. По умолчанию
`LB_ADMIN_ENV_SYNC=1`, поэтому **пароль из env балансера перезапишет файл** при
старте (и погасит старые сессии — они принадлежат прежнему паролю). В логе
балансера это видно строкой `auth bootstrap` (ниже).

Если стоит `LB_ADMIN_ENV_SYNC=0`, файл главнее: смените пароль через WebUI
(если войти есть чем) или удалите `users.json` и перезапустите балансер.

## Диагностика

**Балансер** при старте пишет одну строку — по ней и надо смотреть:

```
auth bootstrap {"source":"env","env_applied":true,"users":1,
 "path":"/app/data/users.json","writable":true,"persisted":true,"reason":""}
```

- `source=env` — админ создан из env; `env+file` — env применён к файлу;
  `file` — взято из файла (env не задан или `ENV_SYNC=0`);
- `persisted=false` — файл записать не удалось (том не смонтирован/только для
  чтения): пароль живёт **только в памяти** и пропадёт при пересоздании;
- ошибка входа `503 no users configured: set LB_ADMIN_USER/LB_ADMIN_PASSWORD for
  the BALANCER (not the webui container)` — учёток нет вообще: задайте их
  балансеру.

**WebUI** при старте (R391) проверяет связь с балансером и честно предупреждает:

```
WARNING: WebUI не видит балансер http://loadbalancer:18081
  ... 1) контейнер запущен вне compose-сети ... 2) балансер не запущен
  Решение: -e LB_API_URL=http://<host-балансера>:18081
  Учётка администратора задаётся НА БАЛАНСЕРЕ: -e LB_ADMIN_USER=...
```

Быстрая проверка руками:

```
curl -s -o /dev/null -w '%{http_code}\n' http://<балансер>:18081/api/v1/ping   # 200
curl -s -X POST http://<балансер>:18081/api/v1/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"..."}'                                  # 200 + cookie
```

Коды: `401` — неверный пароль; `403` — не тот пользователь/нет прав; `502` —
WebUI не видит балансер; `503` с текстом про `LB_ADMIN_*` — учёток нет.

## Порядок «хочу сменить пароль»

1. Балансер: `LB_ADMIN_USER=<новый или тот же>`, `LB_ADMIN_PASSWORD=<новый>`
   (и `LB_ADMIN_ENV_SYNC` не `0`).
2. Перезапустить контейнер балансера (не рестарт процесса — пересоздание,
   чтобы env точно подхватился).
3. Проверить строку `auth bootstrap`: `env_applied=true`.
4. Старые сессии погашаются автоматически — вход заново.

WebUI при этом не трогаем вообще.

#!/bin/sh
# scripts/diagnose-webui-auth.sh - R242 (2026-10-26).
#
# ЗАЧЕМ. Живая жалоба (оффлайн-машина): «не входит по паролю из .env, только по
# токену». Причин ровно несколько, и они различимы по ФАКТАМ:
#   1. env задан не тому контейнеру (пользователи живут в БАЛАНСЕРЕ, не в webui);
#   2. контейнер перезапущен, а не пересоздан: restart env НЕ перечитывает;
#   3. compose исказил значение пароля ($/#/кавычки/пробелы) — в контейнере он
#      короче, чем в .env;
#   4. users.json держит старый хеш, а балансер старше R203 (нет sync из env);
#   5. старый образ балансера (до R201): обработчиков /api/v1/auth/* нет;
#   6. старый образ webui (до R229): nginx держал IP балансера и отдавал 503.
#
# Скрипт ТОЛЬКО ЧИТАЕТ: не меняет env, не пересоздаёт контейнеры. Пароль не
# печатает: только длину и отпечаток sha256 (8 hex) для сверки .env <-> контейнер.
#
# Запуск (на машине со стендом, из корня релиза):
#   sh scripts/diagnose-webui-auth.sh
#   sh scripts/diagnose-webui-auth.sh -b ol-stack-balancer -w ol-stack-webui \
#        -u http://127.0.0.1:18081 -p http://127.0.0.1:18083 -f deployments/.env
#
# Код возврата: 0 - всё сходится, 1 - есть проблемы.

set -u

# Git Bash / MSYS на Windows конвертирует аргументы вида /app/data/... в
# C:/Program Files/Git/app/data/... - docker exec тогда ищет файл не там.
# На Linux эти переменные безвредны.
MSYS_NO_PATHCONV=1
MSYS2_ARG_CONV_EXCL='*'
export MSYS_NO_PATHCONV MSYS2_ARG_CONV_EXCL

BAL=ol-stack-balancer
WEB=ol-stack-webui
BAL_URL=http://127.0.0.1:18081
WEB_URL=http://127.0.0.1:18083
ENV_FILE=deployments/.env

while [ $# -gt 0 ]; do
  case "$1" in
    -b) BAL=$2; shift 2 ;;
    -w) WEB=$2; shift 2 ;;
    -u) BAL_URL=$2; shift 2 ;;
    -p) WEB_URL=$2; shift 2 ;;
    -f) ENV_FILE=$2; shift 2 ;;
    *) echo "неизвестный аргумент: $1" >&2; exit 2 ;;
  esac
done

fails=0
ok()   { printf '  OK        %s\n' "$1"; }
bad()  { printf '  ПРОБЛЕМА  %s%s\n' "$1" "$( [ -n "${2:-}" ] && printf ' - %s' "$2")"; fails=$((fails+1)); }
warn() { printf '  ВНИМАНИЕ  %s%s\n' "$1" "$( [ -n "${2:-}" ] && printf ' - %s' "$2")"; }
info() { printf '  %s\n' "$1"; }

fp() {
  # sha256 первых 8 hex; fallback md5 (busybox без sha256sum)
  if command -v sha256sum >/dev/null 2>&1; then
    printf '%s' "$1" | sha256sum | cut -c1-8
  elif command -v md5sum >/dev/null 2>&1; then
    printf '%s' "$1" | md5sum | cut -c1-8
  else
    printf 'n/a'
  fi
}

json_escape() {
  printf '%s' "$1" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g'
}

echo "=== Диагностика входа WebUI: $(date '+%Y-%m-%d %H:%M:%S') ==="

# 0. Контейнеры
echo
echo "0. Контейнеры"
ps_out=$(docker ps --format '{{.Names}}|{{.Image}}|{{.Status}}' 2>/dev/null)
echo "$ps_out" | sed 's/^/  /'
bal_running=$(printf '%s\n' "$ps_out" | grep -c "^$BAL|")
web_running=$(printf '%s\n' "$ps_out" | grep -c "^$WEB|")
[ "$bal_running" -gt 0 ] && ok "балансер ($BAL) запущен" || bad "балансер ($BAL) не запущен" "без него /api/v1/auth/* обслуживать некому"
[ "$web_running" -gt 0 ] && ok "webui ($WEB) запущен" || bad "webui ($WEB) не запущен" "проверьте имя контейнера (-w)"

# 1. env балансера vs deployments/.env
echo
echo "1. Пароль администратора: .env <-> контейнер балансера"
env_val=''
if [ -f "$ENV_FILE" ]; then
  env_val=$(grep -m1 '^LB_ADMIN_PASSWORD=' "$ENV_FILE" 2>/dev/null | sed 's/^LB_ADMIN_PASSWORD=//')
fi
env_user=$(grep -m1 '^LB_ADMIN_USER=' "$ENV_FILE" 2>/dev/null | sed 's/^LB_ADMIN_USER=//')
info "  .env: user='${env_user:-(нет)}' password_len=${#env_val} fp=$(fp "$env_val")"
if [ ! -f "$ENV_FILE" ]; then
  warn "файл $ENV_FILE не найден" "передайте путь через -f, сравнить .env и контейнер будет нечем"
fi
if [ "$bal_running" -gt 0 ]; then
  c_user=$(docker exec "$BAL" printenv LB_ADMIN_USER 2>/dev/null || true)
  c_val=$(docker exec "$BAL" sh -c 'printf "%s" "$LB_ADMIN_PASSWORD"' 2>/dev/null || true)
  info "  контейнер: user='${c_user:-(нет)}' password_len=${#c_val} fp=$(fp "$c_val")"
  if [ -z "$c_val" ]; then
    bad "в контейнере балансера LB_ADMIN_PASSWORD пуст" "env не задан, или compose исказил значение (символ \$ в .env раскрывается; экранируйте как \$\$)"
  else
    if [ -n "$env_val" ] && [ "$c_val" != "$env_val" ]; then
      bad "значение в контейнере ОТЛИЧАЕТСЯ от .env" "обычно: спецсимволы (\$ -> переменная compose, # -> комментарий, кавычки) или контейнер не пересоздан после правки"
    else
      ok "пароль в контейнере совпадает с .env"
    fi
  fi
fi

# 2. users.json
echo
echo "2. Файл пользователей"
if [ "$bal_running" -gt 0 ]; then
  users_file=$(docker exec "$BAL" printenv LB_USERS_FILE 2>/dev/null || true)
  [ -z "$users_file" ] && users_file=/app/data/users.json
  info "  LB_USERS_FILE=$users_file"
  users_json=$(docker exec "$BAL" sh -c 'cat "$1" 2>/dev/null' sh "$users_file" 2>/dev/null || true)
  if [ -n "$users_json" ]; then
    ok "файл существует: $users_file"
    names=$(printf '%s' "$users_json" | grep -o '"name"[[:space:]]*:[[:space:]]*"[^"]*"' | sed 's/.*"\([^"]*\)"$/\1/' | tr '\n' ' ')
    info "  пользователи в файле: ${names:-<не прочитан>}"
  else
    bad "файла нет: $users_file" "балансер создаёт его при старте из env; без тома файл пропадёт при пересоздании (в логе будет «создан в ПАМЯТИ»)"
  fi
fi

# 3. /auth/me
echo
echo "3. GET /api/v1/auth/me"
me_bal=$(curl -sS --max-time 10 "$BAL_URL/api/v1/auth/me" 2>/dev/null || true)
me_web=$(curl -sS --max-time 10 "$WEB_URL/api/v1/auth/me" 2>/dev/null || true)
info "  напрямую: ${me_bal:-<нет ответа>}"
info "  через webui: ${me_web:-<нет ответа>}"
case "$me_bal" in
  *'"usersConfigured":true'*) ok "usersConfigured=true (напрямую)" ;;
  *'"usersConfigured":false'*) bad "usersConfigured=false (напрямую)" "пользователей нет: env не применился (restart вместо пересоздания) или старый образ" ;;
  *) bad "нет валидного ответа напрямую" "балансер не запущен/старый образ/другой порт" ;;
esac
case "$me_web" in
  *'"usersConfigured":true'*) ok "usersConfigured=true (через webui)" ;;
  *) warn "через webui ответ не получен" "если напрямую 200, а через webui 503 - образ webui старше R229 (nginx держал старый IP балансера)" ;;
esac

# 4. Реальный вход по паролю из .env
echo
echo "4. POST /api/v1/auth/login (пароль из .env, значение не печатается)"
if [ -n "$env_val" ] && [ -n "${env_user:-}" ]; then
  body=$(printf '{"name":"%s","password":"%s"}' "$(json_escape "$env_user")" "$(json_escape "$env_val")")
  code=$(printf '%s' "$body" | curl -sS --max-time 15 -o /tmp/ol-auth-login.out -w '%{http_code}' \
    -X POST -H 'Content-Type: application/json' --data-binary @- "$BAL_URL/api/v1/auth/login" 2>/dev/null || true)
  answer=$(cat /tmp/ol-auth-login.out 2>/dev/null || true)
  rm -f /tmp/ol-auth-login.out
  info "  напрямую: HTTP ${code:-нет ответа} ${answer}"
  case "$code" in
    200) ok "вход по паролю из .env РАБОТАЕТ (напрямую)" ;;
    401) bad "401 invalid credentials" "в users.json другой хеш: пересоздайте балансер (R203+ синхронизирует пароль из env), либо в контейнере искажённое значение (см. шаг 1)" ;;
    503) bad "503 no users configured" "пользователей нет: env не на балансере, не пересоздан контейнер или образ старше R201" ;;
    400) bad "400 invalid request body" "нестандартная причина: проверьте ответ сервера выше" ;;
    *) bad "вход не проверен (HTTP ${code:-нет ответа})" "балансер недоступен по $BAL_URL" ;;
  esac

  webcode=$(printf '%s' "$body" | curl -sS --max-time 15 -o /dev/null -w '%{http_code}' \
    -X POST -H 'Content-Type: application/json' --data-binary @- "$WEB_URL/api/v1/auth/login" 2>/dev/null || true)
  info "  через webui: HTTP ${webcode:-нет ответа}"
  [ "$webcode" = "200" ] && ok "вход через webui работает" || warn "через webui вход не 200" "если напрямую 200 - устарел образ webui (R229+: upstream с resolver)"
else
  warn "пароль/пользователь в .env не найдены" "проверка входа пропущена (см. шаг 1)"
fi

# 5. Логи
echo
echo "5. Логи балансера (auth)"
if [ "$bal_running" -gt 0 ]; then
  docker logs "$BAL" --since 2h 2>&1 | grep -E 'auth:|users_file|LB_ADMIN' | tail -n 8 | sed 's/^/  /' || true
fi

echo
echo "=== ВЕРДИКТ ==="
if [ "$fails" -eq 0 ]; then
  echo "  Вход настроен: пароль из .env совпадает с контейнером, вход по нему отвечает 200."
  echo "  Если браузер всё равно не пускает - очистите cookie этого хоста (старая сессия/аварийный токен в sessionStorage)."
  exit 0
fi
echo "  Проблем: $fails. Частые причины:"
echo "   1) env читает БАЛАНСЕР (LB_ADMIN_USER/LB_ADMIN_PASSWORD), не webui;"
echo "   2) после правки .env нужен 'docker compose up -d --force-recreate <балансер>' (restart env не перечитывает);"
echo "   3) спецсимволы в .env: \$ раскрывается compose (экранируйте \$\$), # начинает комментарий;"
echo "   4) образ балансера старше R203 - нет синхронизации пароля из env; старше R201 - нет /auth/login вообще;"
echo "   5) образ webui старше R229 - nginx держит старый IP балансера (503 через webui при 200 напрямую)."
exit 1

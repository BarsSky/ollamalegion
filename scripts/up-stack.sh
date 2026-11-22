#!/usr/bin/env bash
# =============================================================================
# up-stack.sh — поднимает стек OllamaLegion одной командой: считает идентичность
# сборки из git, собирает ОТСУТСТВУЮЩИЕ образы и запускает
# `docker compose -f deployments/docker-compose.stack.yml --profile <профиль> up -d`.
#
# ЗАЧЕМ ЭТОТ СЛОЙ (долг R486).
#
# `docker compose ... up -d` на машине без образов уходит в сборку, но падала на
# «страже идентичности»: Dockerfile'ы требуют build-arg'и GIT_COMMIT /
# GIT_COMMIT_FULL / BUILD_DATE (cppworker — ещё LLAMA_BUILD / RPC_PROTO), так как
# смысл R486 в том, что образ НЕ должен уехать со штампом `unknown`. Вычислять их
# из git умел только scripts/release-all.ps1; deployments/.env этих значений не
# содержит и содержать не может (env_file на build-args не действует, а .git в
# контекст сборки не попадает из-за .dockerignore). То есть «просто подняться» на
# чистой машине было нельзя — и это был дефект, а не осторожность.
#
# Этот файл — bash-близнец scripts/up-stack.ps1: те же шаги, те же значения, тот
# же порядок. Идентичность считается ТЕМИ ЖЕ правилами, что в
# scripts/build-identity.ps1 (PowerShell): короткий/полный коммит, дата коммита,
# ветка, BUILD_DATE, автор, версия (git describe -> CHANGELOG.md), а для
# cppworker — коммит/дата llama.cpp и RPC_PROTO из ggml-rpc.h.
#
# Использование:
#   ./scripts/up-stack.sh                     # профиль full, собрать чего нет, up -d
#   ./scripts/up-stack.sh --profile balancer  # только управление
#   ./scripts/up-stack.sh --no-build          # образы уже есть, не собирать
#   ./scripts/up-stack.sh --build-only --services webui --tag local-check-webui
#   ./scripts/up-stack.sh --dry-run           # показать идентичность и команду
#
# Требования: bash (3.2+), git, docker + docker compose (v2) или docker-compose.
# Никаких внешних зависимостей: sed/grep/awk/head/tail — стандартный набор POSIX.
# =============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
DEPLOYMENTS_DIR="$REPO_ROOT/deployments"

COMPOSE_PROFILE="full"
COMPOSE_FILE="docker-compose.stack.yml"
NO_BUILD=0
BUILD_ONLY=0
DRY_RUN=0
TAG=""
SERVICES_STR=""
GIT_COMMIT_OPT=""
GIT_COMMIT_FULL_OPT=""
GIT_COMMIT_DATE_OPT=""
LLAMA_BUILD_OPT=""
LLAMA_BUILD_DATE_OPT=""

usage() {
    cat <<'USAGE'
up-stack.sh — поднять стек OllamaLegion одной командой (сборка отсутствующих образов штатно).

ОБЯЗАТЕЛЬНЫЕ ПРЕДПОСЫЛКИ
  * это git-клон (нужен каталог .git): иначе идентичность сборки вычислить нечем;
  * deployments/.env создан из deployments/.env.example и в нём задан CPPWORKER_API_TOKEN;
  * установлены docker + docker compose (v2) или docker-compose (v1).

ПАРАМЕТРЫ
  --profile, -p <имя>     профиль compose (по умолчанию full; worker, balancer, legacy-agent)
  --compose-file <файл>   файл compose внутри deployments/ (по умолчанию docker-compose.stack.yml)
  --no-build              не собирать образы (--no-build); падает, если образа нет локально
  --build-only            только собрать (docker compose ... build), не запускать
  --services <список>     сервисы через запятую; только с --build-only
  --tag <тег>             переопределить теги образов ТОЛЬКО на этот запуск (deployments/.env не меняется)
  --git-commit <sha>      ручной короткий коммит (когда git недоступен, например архив исходников)
  --git-commit-full <sha> ручной полный коммит (метка OCI .revision)
  --git-commit-date <iso> ручная дата коммита (как git log -1 --format=%cI)
  --llama-build <sha>     ручной коммит llama.cpp (нужен только сборке cppworker-gpu)
  --llama-build-date <д>  ручная дата коммита llama.cpp (ГГГГ-ММ-ДД)
  --dry-run               ничего не запускать: показать идентичность, состояние образов и команду
  --help, -h              эта справка

ПРИМЕРЫ
  ./scripts/up-stack.sh
  ./scripts/up-stack.sh --profile balancer --no-build
  ./scripts/up-stack.sh --build-only --services webui --tag local-check-webui
  ./scripts/up-stack.sh --dry-run
USAGE
}

fail() {
    local message="$1"
    shift
    echo "" >&2
    echo "up-stack: ОШИБКА: $message" >&2
    local hint
    for hint in "$@"; do
        echo "  $hint" >&2
    done
    echo "" >&2
    exit 1
}

# ---------------------------------------------------------------- параметры
while [ $# -gt 0 ]; do
    case "$1" in
        --profile|-p)
            [ $# -ge 2 ] || fail "параметр $1 требует значения"
            COMPOSE_PROFILE="$2"
            shift 2
            ;;
        --compose-file)
            [ $# -ge 2 ] || fail "параметр --compose-file требует значения"
            COMPOSE_FILE="$2"
            shift 2
            ;;
        --no-build)
            NO_BUILD=1
            shift
            ;;
        --build-only)
            BUILD_ONLY=1
            shift
            ;;
        --services)
            [ $# -ge 2 ] || fail "параметр --services требует значения"
            if [ -n "$SERVICES_STR" ]; then
                SERVICES_STR="$SERVICES_STR,$2"
            else
                SERVICES_STR="$2"
            fi
            shift 2
            ;;
        --tag)
            [ $# -ge 2 ] || fail "параметр --tag требует значения"
            TAG="$2"
            shift 2
            ;;
        --git-commit)
            [ $# -ge 2 ] || fail "параметр --git-commit требует значения"
            GIT_COMMIT_OPT="$2"
            shift 2
            ;;
        --git-commit-full)
            [ $# -ge 2 ] || fail "параметр --git-commit-full требует значения"
            GIT_COMMIT_FULL_OPT="$2"
            shift 2
            ;;
        --git-commit-date)
            [ $# -ge 2 ] || fail "параметр --git-commit-date требует значения"
            GIT_COMMIT_DATE_OPT="$2"
            shift 2
            ;;
        --llama-build)
            [ $# -ge 2 ] || fail "параметр --llama-build требует значения"
            LLAMA_BUILD_OPT="$2"
            shift 2
            ;;
        --llama-build-date)
            [ $# -ge 2 ] || fail "параметр --llama-build-date требует значения"
            LLAMA_BUILD_DATE_OPT="$2"
            shift 2
            ;;
        --dry-run)
            DRY_RUN=1
            shift
            ;;
        --help|-h)
            usage
            exit 0
            ;;
        *)
            fail "неизвестный параметр '$1'" "Справка: ./scripts/up-stack.sh --help"
            ;;
    esac
done

COMPOSE_PATH="$DEPLOYMENTS_DIR/$COMPOSE_FILE"
ENV_PATH="$DEPLOYMENTS_DIR/.env"

# ------------------------------------------------------- предпосылки (часть 1)
if [ ! -f "$REPO_ROOT/go.mod" ]; then
    fail "не найден корень репозитория: в '$REPO_ROOT' нет go.mod" \
        "Скрипт должен лежать в <репозиторий>/scripts/up-stack.sh — не копируйте его отдельно"
fi
if [ ! -f "$COMPOSE_PATH" ]; then
    fail "нет compose-файла: $COMPOSE_PATH" \
        "Проверьте --compose-file (по умолчанию docker-compose.stack.yml)"
fi
if [ ! -f "$ENV_PATH" ]; then
    fail "нет $ENV_PATH — compose не сможет интерполировать переменные стека" \
        "Скопируйте шаблон и заново запустите скрипт:" \
        "  cp deployments/.env.example deployments/.env" \
        "и задайте в deployments/.env единый токен стека CPPWORKER_API_TOKEN"
fi
if [ "$NO_BUILD" -eq 1 ] && [ "$BUILD_ONLY" -eq 1 ]; then
    fail "--no-build и --build-only одновременно бессмысленны: выберите одно" \
        "--no-build   — воспользоваться готовыми образами и поднять стек" \
        "--build-only — только собрать образы, не поднимая контейнеры"
fi
if [ -n "$SERVICES_STR" ] && [ "$BUILD_ONLY" -eq 0 ]; then
    fail "--services поддерживается только вместе с --build-only" \
        "Подъём стека идёт одним порядком: профиль целиком (--profile <профиль> up -d)" \
        "Проверочная сборка одного сервиса: --build-only --services webui"
fi
if [ -n "$TAG" ]; then
    case "$TAG" in
        *[!A-Za-z0-9._-]*)
            fail "недопустимый --tag '$TAG': разрешены только буквы, цифры, точка, подчёркивание и дефис" \
                "Пример проверочного тега: --tag local-check-webui"
            ;;
    esac
fi

# --------------------------------------------------------------- docker/compose
if ! command -v docker >/dev/null 2>&1; then
    fail "не найден docker CLI (docker не в PATH)" \
        "Установите Docker (или Docker Desktop) и проверьте: docker version" \
        "Linux: добавьте пользователя в группу docker, иначе нужен sudo"
fi

COMPOSE_MODE=""
if docker compose version >/dev/null 2>&1; then
    COMPOSE_MODE="v2"
elif command -v docker-compose >/dev/null 2>&1 && docker-compose version >/dev/null 2>&1; then
    COMPOSE_MODE="v1"
else
    fail "не найден docker compose (ни плагин \`docker compose\`, ни docker-compose)" \
        "Docker Desktop: включите «Docker Compose» в настройках" \
        "Linux: установите плагин compose v2 (docker-compose-plugin)"
fi

compose() {
    if [ "$COMPOSE_MODE" = "v2" ]; then
        docker compose "$@"
    else
        docker-compose "$@"
    fi
}

# --------------------------------------------------------------- чтение .env
# ТОЛЬКО ЧТЕНИЕ: deployments/.env — состояние живого стенда, скрипт его не меняет.
env_value() {
    local path="$1" name="$2" value=""
    value="$(grep -E "^[[:space:]]*${name}[[:space:]]*=" "$path" 2>/dev/null | tail -n 1 | sed -E 's/^[^=]*=[[:space:]]*//')" || true
    # кавычки compose снимает сам; здесь — чтобы сравнить значение, а не переписать
    case "$value" in
        \"*\") value="${value#\"}"; value="${value%\"}" ;;
        \'*\') value="${value#\'}"; value="${value%\'}" ;;
    esac
    printf '%s' "$value"
}

# Приоритет ровно как у compose: окружение процесса > deployments/.env > default.
# printenv (POSIX) вместо ${!name}: косвенное раскрытие с `:-` по-разному ведёт себя
# в старых bash, а окружение процесса читается ровно так же.
effective_var() {
    local name="$1" def="$2" value=""
    value="$(printenv "$name" 2>/dev/null || true)"
    if [ -z "$value" ]; then
        value="$(env_value "$ENV_PATH" "$name")"
    fi
    if [ -z "$value" ]; then
        value="$def"
    fi
    printf '%s' "$value"
}

token="$(effective_var CPPWORKER_API_TOKEN '')"
if [ -z "$token" ]; then
    fail "в deployments/.env не задан CPPWORKER_API_TOKEN" \
        "Это единый токен стека (балансер, воркеры, webui). Впишите свою строку:" \
        "  CPPWORKER_API_TOKEN=<ваш-токен>" \
        "Значение по умолчанию из .env.example — плейсхолдер, его нужно заменить"
fi

# ==========================================================================
# 1. Идентичность сборки (R486) — по тем же правилам, что build-identity.ps1
# ==========================================================================
git_available=0
if command -v git >/dev/null 2>&1; then
    git_available=1
fi

git_value() {
    # $1 — каталог репозитория, дальше — аргументы git; печатает первую строку.
    local repo="$1"
    shift
    git -C "$repo" "$@" 2>/dev/null | head -n 1 || true
}

changelog_version() {
    local path="$REPO_ROOT/CHANGELOG.md" version=""
    if [ -f "$path" ]; then
        version="$(sed -n 's/^## \[\([0-9][0-9]*\.[0-9][0-9]*\.[0-9][0-9]*\).*/\1/p' "$path" | head -n 1)"
    fi
    printf '%s' "$version"
}

# Идентичность llama.cpp нужна только тому образу, который её линкует —
# cppworker-gpu (профили full/worker), и только когда сборка возможна.
MAY_BUILD_CPPWORKER=0
if [ "$COMPOSE_PROFILE" = "full" ] || [ "$COMPOSE_PROFILE" = "worker" ]; then
    MAY_BUILD_CPPWORKER=1
fi
if [ "$NO_BUILD" -eq 1 ]; then
    MAY_BUILD_CPPWORKER=0
fi
if [ "$BUILD_ONLY" -eq 1 ] && [ -n "$SERVICES_STR" ]; then
    case ",$SERVICES_STR," in
        *",cppworker-gpu,"*) ;;
        *) MAY_BUILD_CPPWORKER=0 ;;
    esac
fi

commit_short=""
commit_full=""
commit_date=""
build_date="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
build_author="${BUILD_AUTHOR:-}"
build_version=""
build_branch=""

if [ -z "$GIT_COMMIT_OPT" ] && [ -z "$GIT_COMMIT_FULL_OPT" ] && [ -z "$GIT_COMMIT_DATE_OPT" ]; then
    # Канонический путь: git обязателен. Не подставляем 'unknown' — R486.
    if [ "$git_available" -ne 1 ]; then
        fail "не удалось вычислить идентичность сборки: git не найден в PATH" \
            "Без неё образы не собираются: Dockerfile-стража R486 отвергает пустой/unknown коммит" \
            "Проверьте: git -C <репозиторий> rev-parse --short HEAD" \
            "Если git недоступен (архив исходников), задайте идентичность явно:" \
            "  --git-commit <короткий-sha> --git-commit-full <полный-sha> [--git-commit-date <ISO-8601>]"
    fi
    commit_short="$(git_value "$REPO_ROOT" rev-parse --short HEAD)"
    commit_full="$(git_value "$REPO_ROOT" rev-parse HEAD)"
    commit_date="$(git_value "$REPO_ROOT" log -1 --format=%cI)"
    build_branch="$(git_value "$REPO_ROOT" rev-parse --abbrev-ref HEAD)"
    build_version="$(git_value "$REPO_ROOT" describe --tags --always)"

    if [ -z "$commit_short" ]; then
        fail "git rev-parse --short HEAD не вернул коммит в $REPO_ROOT" \
            "Убедитесь, что это git-клон с коммитами (а не пустой репозиторий)" \
            "Либо задайте идентичность явно: --git-commit <sha> --git-commit-full <sha>"
    fi
    [ -n "$commit_full" ] || commit_full="$commit_short"
    [ -n "$commit_date" ] || fail "не удалось получить дату коммита (git log -1 --format=%cI)" \
        "Задайте явно: --git-commit-date <ISO-8601>"
    if [ -z "$build_author" ]; then
        build_author="$(git_value "$REPO_ROOT" config user.name)"
    fi
else
    # Ручной путь: поле из флага, иначе — из git, если он есть.
    commit_short="$GIT_COMMIT_OPT"
    commit_full="$GIT_COMMIT_FULL_OPT"
    commit_date="$GIT_COMMIT_DATE_OPT"

    if [ "$git_available" -eq 1 ]; then
        [ -n "$commit_short" ] || commit_short="$(git_value "$REPO_ROOT" rev-parse --short HEAD)"
        [ -n "$commit_full" ] || commit_full="$(git_value "$REPO_ROOT" rev-parse HEAD)"
        [ -n "$commit_date" ] || commit_date="$(git_value "$REPO_ROOT" log -1 --format=%cI)"
        [ -n "$build_branch" ] || build_branch="$(git_value "$REPO_ROOT" rev-parse --abbrev-ref HEAD)"
        [ -n "$build_version" ] || build_version="$(git_value "$REPO_ROOT" describe --tags --always)"
        if [ -z "$build_author" ]; then
            build_author="$(git_value "$REPO_ROOT" config user.name)"
        fi
    fi

    if [ -z "$commit_full" ]; then
        commit_full="$commit_short"
        echo "up-stack: внимание: --git-commit-full не задан, в метку .revision пойдёт короткий коммит" >&2
    fi
    if [ -z "$commit_date" ]; then
        echo "up-stack: внимание: дата коммита неизвестна (git недоступен, --git-commit-date не задан) — поле commit_date в identity.json будет пустым" >&2
    fi
fi

if [ -z "$build_version" ]; then
    build_version="$(changelog_version)"
fi
if [ -z "$build_version" ]; then
    build_version="dev"
fi

# llama.cpp: коммит/дата/протокол нужны только сборке cppworker-gpu.
llama_commit=""
llama_date=""
rpc_proto=""
if [ "$MAY_BUILD_CPPWORKER" -eq 1 ]; then
    LLAMA_DIR="$REPO_ROOT/c/llama.cpp"
    LLAMA_HEADER="$LLAMA_DIR/ggml/include/ggml-rpc.h"

    if [ -n "$LLAMA_BUILD_OPT" ]; then
        llama_commit="$LLAMA_BUILD_OPT"
    elif [ ! -d "$LLAMA_DIR" ]; then
        fail "нет c/llama.cpp, а профиль собирает cppworker-gpu" \
            "Сабмодуль не инициализирован. Выполните:" \
            "  git submodule update --init --recursive" \
            "Если образы уже собраны и нужен только запуск — добавьте --no-build"
    elif [ "$git_available" -ne 1 ]; then
        fail "не удалось вычислить коммит llama.cpp: git не найден в PATH" \
            "Задайте явно: --llama-build <sha> --llama-build-date <ГГГГ-ММ-ДД>" \
            "Либо инициализируйте сабмодуль на машине с git: git submodule update --init --recursive"
    fi

    if [ -z "$llama_commit" ]; then
        llama_commit="$(git_value "$LLAMA_DIR" rev-parse --short HEAD)"
    fi
    if [ -n "$LLAMA_BUILD_DATE_OPT" ]; then
        llama_date="$LLAMA_BUILD_DATE_OPT"
    elif [ "$git_available" -eq 1 ]; then
        llama_date="$(git_value "$LLAMA_DIR" log -1 --format=%cs)"
    fi
    if [ -z "$llama_commit" ] || [ -z "$llama_date" ]; then
        fail "не удалось вычислить идентичность llama.cpp (нужна сборке cppworker-gpu)" \
            "Выполните: git submodule update --init --recursive" \
            "Либо укажите явно: --llama-build <sha> --llama-build-date <ГГГГ-ММ-ДД>" \
            "Если образы уже собраны и нужен только запуск — добавьте --no-build"
    fi

    # RPC_PROTO — из заголовка: единственный авторитетный источник
    # (c/llama.cpp/ggml/include/ggml-rpc.h), как и в build-identity.ps1.
    if [ ! -f "$LLAMA_HEADER" ]; then
        fail "нет $LLAMA_HEADER — не могу запечь версию протокола RPC" \
            "Сабмодуль c/llama.cpp инициализирован полностью? git submodule update --init --recursive"
    fi
    rpc_major="$(sed -n 's/^#define[[:space:]]*RPC_PROTO_MAJOR[_A-Z]*[[:space:]]*\([0-9][0-9]*\).*/\1/p' "$LLAMA_HEADER" | head -n 1)"
    rpc_minor="$(sed -n 's/^#define[[:space:]]*RPC_PROTO_MINOR[_A-Z]*[[:space:]]*\([0-9][0-9]*\).*/\1/p' "$LLAMA_HEADER" | head -n 1)"
    rpc_patch="$(sed -n 's/^#define[[:space:]]*RPC_PROTO_PATCH[_A-Z]*[[:space:]]*\([0-9][0-9]*\).*/\1/p' "$LLAMA_HEADER" | head -n 1)"
    if [ -z "$rpc_major" ] || [ -z "$rpc_minor" ] || [ -z "$rpc_patch" ]; then
        fail "в $LLAMA_HEADER нет RPC_PROTO_MAJOR/MINOR/PATCH_VERSION — формат заголовка изменился, версию не угадываю"
    fi
    rpc_proto="$rpc_major.$rpc_minor.$rpc_patch"
fi

# ==========================================================================
# 2. Что из образов уже есть (настоящий ответ на вопрос «почему сборка»)
# ==========================================================================
RUN_ARGS=(-f "$COMPOSE_PATH" --env-file "$ENV_PATH" --profile "$COMPOSE_PROFILE")

# --tag — ТОЛЬКО на этот запуск: теги живут в окружении процесса, deployments/.env
# не переписывается (compose даёт окружению приоритет над .env).
if [ -n "$TAG" ]; then
    export BALANCER_TAG="$TAG"
    export CPPWORKER_GPU_TAG="$TAG"
    export WEBUI_TAG="$TAG"
    export AGENT_TAG="$TAG"
    export IMAGE_WORKER_TAG="$TAG"
    echo "up-stack: теги образов на этот запуск = '$TAG' (deployments/.env не изменяется)" >&2
fi

images_raw=""
if images_raw="$(compose "${RUN_ARGS[@]}" config --images 2>/dev/null)"; then
    :
else
    images_raw=""
fi

MISSING_IMAGES=""
if [ -n "$images_raw" ]; then
    echo ""
    echo "образы стека:"
    while IFS= read -r img; do
        [ -n "$img" ] || continue
        if docker image inspect "$img" >/dev/null 2>&1; then
            echo "  есть  $img"
        else
            if [ -n "$MISSING_IMAGES" ]; then
                MISSING_IMAGES="$MISSING_IMAGES, $img"
            else
                MISSING_IMAGES="$img"
            fi
            if [ "$NO_BUILD" -eq 1 ]; then
                echo "  НЕТ   $img"
            else
                echo "  НЕТ   $img  -> будет собрана"
            fi
        fi
    done <<EOF
$images_raw
EOF
fi

if [ "$NO_BUILD" -eq 1 ] && [ -n "$MISSING_IMAGES" ]; then
    fail "образов нет, а сборка запрещена флагом --no-build" \
        "Отсутствуют:" \
        "  $MISSING_IMAGES" \
        "Уберите --no-build — compose соберёт отсутствующие образы штатно (build: в compose)"
fi

# CUDA_ARCH с архитектурой 100+ требует CUDA 12.8+: иначе nvcc упадёт на
# «Unsupported gpu architecture 'compute_120'» уже после долгой сборки.
# Проверяем только когда cppworker-gpu действительно будет собираться.
if [ "$MAY_BUILD_CPPWORKER" -eq 1 ]; then
    cpp_ref=""
    if [ -n "$images_raw" ]; then
        cpp_ref="$(printf '%s\n' "$images_raw" | grep -i 'cppworker' | head -n 1)" || true
    fi
    cpp_will_build=1
    if [ -n "$cpp_ref" ] && docker image inspect "$cpp_ref" >/dev/null 2>&1; then
        cpp_will_build=0
    fi
    if [ "$cpp_will_build" -eq 1 ]; then
        cuda_arch="$(effective_var CUDA_ARCH 'all')"
        cuda_version="$(effective_var CUDA_VERSION '12.2.0')"
        cuda_arch_clean="${cuda_arch%%#*}"
        needs_blackwell=0
        old_ifs="$IFS"
        IFS=';,'
        for token in $cuda_arch_clean; do
            token="$(printf '%s' "$token" | tr -d '[:space:]')"
            token="${token#sm_}"
            case "$token" in
                ''|*[!0-9]*) ;;                                   # 'all' и мусор пропускаем
                *) [ "$token" -ge 100 ] && needs_blackwell=1 || true ;;
            esac
        done
        IFS="$old_ifs"
        if [ "$needs_blackwell" -eq 1 ]; then
            ver_pair="$(printf '%s' "$cuda_version" | sed -n 's/^[[:space:]]*\([0-9][0-9]*\)\.\([0-9][0-9]*\).*/\1 \2/p')"
            if [ -n "$ver_pair" ]; then
                ver_major="${ver_pair%% *}"
                ver_minor="${ver_pair##* }"
                if [ "$ver_major" -lt 12 ] || { [ "$ver_major" -eq 12 ] && [ "$ver_minor" -lt 8 ]; }; then
                    fail "CUDA_ARCH='$cuda_arch_clean' содержит архитектуру 100+ (Blackwell/sm_120), а CUDA_VERSION='$cuda_version' её не знает" \
                        "Сборка cppworker-gpu упала бы на «Unsupported gpu architecture compute_120» после долгой сборки" \
                        "Исправьте в deployments/.env одно из двух:" \
                        "  CUDA_VERSION=12.8.0   (базовый образ CUDA 12.8 умеет sm_120)" \
                        "  CUDA_ARCH=86          (или список без 100/120 — под вашу карту)" \
                        "Либо задайте на этот запуск: export CUDA_VERSION=12.8.0"
                fi
            fi
        fi
    fi
fi

# ==========================================================================
# 3. План: печатаем то, что реально уйдёт в docker (без догадок)
# ==========================================================================
echo ""
echo "идентичность сборки (печётся в образ, R486):"
echo "  git commit    : $commit_short"
echo "  git commit full: $commit_full"
if [ -n "$commit_date" ]; then
    echo "  commit date   : $commit_date"
else
    echo "  commit date   : неизвестна (git недоступен)"
fi
echo "  build date    : $build_date"
if [ -n "$build_author" ]; then
    echo "  author        : $build_author"
else
    echo "  author        : (не задан)"
fi
echo "  version       : $build_version"
if [ -n "$build_branch" ]; then
    echo "  branch        : $build_branch"
fi
if [ "$MAY_BUILD_CPPWORKER" -eq 1 ]; then
    echo "  llama.cpp     : $llama_commit ($llama_date)"
    echo "  rpc proto     : $rpc_proto"
fi

if [ "$BUILD_ONLY" -eq 1 ]; then
    if [ -n "$SERVICES_STR" ]; then
        PLAN_ACTION="build $(printf '%s' "$SERVICES_STR" | tr ',' ' ')"
    else
        PLAN_ACTION="build"
    fi
else
    if [ "$NO_BUILD" -eq 1 ]; then
        PLAN_ACTION="up -d --no-build"
    else
        PLAN_ACTION="up -d"
    fi
fi
echo ""
echo "команда:"
if [ "$COMPOSE_MODE" = "v2" ]; then
    echo "  docker compose ${RUN_ARGS[*]} $PLAN_ACTION"
else
    echo "  docker-compose ${RUN_ARGS[*]} $PLAN_ACTION"
fi

if [ "$DRY_RUN" -eq 1 ]; then
    echo ""
    echo "DRY-RUN: ничего не собрано и не запущено; docker только читался."
    exit 0
fi

# ==========================================================================
# 4. Экспорт в окружение процесса и запуск compose
# ==========================================================================
# Compose берёт build-arg'и (`${GIT_COMMIT:-}` и т.д.) из ОКРУЖЕНИЯ процесса:
# env_file на build-args не действует, поэтому единственный способ донести
# идентичность до сборки — эти переменные.
export GIT_COMMIT="$commit_short"
export GIT_COMMIT_FULL="$commit_full"
export GIT_COMMIT_DATE="$commit_date"
export BUILD_DATE="$build_date"
export BUILD_AUTHOR="$build_author"
export VERSION="$build_version"
if [ "$MAY_BUILD_CPPWORKER" -eq 1 ]; then
    export LLAMA_BUILD="$llama_commit"
    export LLAMA_BUILD_DATE="$llama_date"
    export RPC_PROTO="$rpc_proto"
fi

start_seconds="$SECONDS"
echo ""
echo "запуск..."

rc=0
if [ "$BUILD_ONLY" -eq 1 ]; then
    if [ -n "$SERVICES_STR" ]; then
        # Имена сервисов compose — без пробелов, поэтому word splitting безопасен.
        # shellcheck disable=SC2086
        compose "${RUN_ARGS[@]}" build $(printf '%s' "$SERVICES_STR" | tr ',' ' ') || rc=$?
    else
        compose "${RUN_ARGS[@]}" build || rc=$?
    fi
else
    if [ "$NO_BUILD" -eq 1 ]; then
        compose "${RUN_ARGS[@]}" up -d --no-build || rc=$?
    else
        compose "${RUN_ARGS[@]}" up -d || rc=$?
    fi
fi
elapsed=$((SECONDS - start_seconds))

if [ "$rc" -ne 0 ]; then
    fail "docker compose завершился с кодом $rc" \
        "Полный вывод выше. Частые причины на чистой машине:" \
        "  * нет доступа к базовому образу (NGINX_BASE / CUDA_BASE) — сеть или зеркало;" \
        "  * стража идентичности: «FATAL: GIT_COMMIT not supplied» — сообщите, это дефект проводки (R486);" \
        "  * docker без прав (Linux: группа docker / sudo)."
fi

echo ""
if [ "$BUILD_ONLY" -eq 1 ]; then
    echo "готово: сборка заняла ${elapsed} с"
    echo "Проверить штамп в образе: docker image inspect --format '{{index .Config.Labels \"org.opencontainers.image.revision\"}}' <образ>"
    exit 0
fi

echo "готово: стек поднят (${elapsed} с)"
echo ""
echo "проверка здоровья:"
if [ "$COMPOSE_MODE" = "v2" ]; then
    echo "  docker compose -f $COMPOSE_FILE --profile $COMPOSE_PROFILE ps"
else
    echo "  docker-compose -f $COMPOSE_FILE --profile $COMPOSE_PROFILE ps"
fi
echo "  клиентский API (Ollama/OpenAI): http://localhost:18080"
echo "  admin API + метрики         : http://localhost:18081/api/v1/health"
echo "  WebUI оператора             : http://localhost:18083"
echo "Порты переопределяются в deployments/.env (LB_PORT / LB_ADMIN_PORT / WEBUI_PORT)."

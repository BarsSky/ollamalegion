# Переводит оставшиеся русские комментарии в docker-compose.stack.yml.
# Каждая замена обязана найтись РОВНО один раз — иначе скрипт падает, и я узнаю
# о несовпадении сразу, а не получу молча пропущенный блок.
$ErrorActionPreference = 'Stop'
$path = Join-Path $PSScriptRoot '..\deployments\docker-compose.stack.yml'
$c = Get-Content $path -Raw

$map = [ordered]@{
@'
    # R-Image (2026-10-07): зависимость ОПЦИОНАЛЬНА, чтобы работал сценарий
    # «только бэкенд» (--profile worker), где балансер живёт на ДРУГОЙ
    # машине. Без required:false compose отвергает ВЕСЬ проект
    # («service webui depends on undefined service loadbalancer»), потому
    # что loadbalancer не входит в активный профиль — и развернуть бэкенды
    # отдельно было невозможно.
    #
    # Почему это безопасно: depends_on задаёт только ПОРЯДОК запуска. Если
    # loadbalancer активен (профили full/balancer) — ждём его healthy; если
    # его нет в профиле — compose не ждёт и не падает, а воркеры
    # регистрируются в ВНЕШНЕМ балансере по BALANCER_URL.
'@ = @'
    # OPTIONAL dependency, so the backend-only scenario (`--profile worker`,
    # balancer on ANOTHER machine) works at all. Without required:false compose
    # rejects the WHOLE project ("service webui depends on undefined service
    # loadbalancer") because loadbalancer is not in the active profile.
    #
    # Why it is safe: depends_on only defines start ORDER. If loadbalancer is
    # active (full/balancer) we wait for healthy; if it is absent from the
    # profile compose neither waits nor fails, and the workers register with the
    # EXTERNAL balancer via BALANCER_URL.
'@

@'
  # cppworker-gpu — инференс llama.cpp на GPU
  # Профили: worker | full
'@ = @'
  # cppworker-gpu — llama.cpp inference on GPU
  # Profiles: worker | full
'@

@'
      # Phase 8 (2026-10-03): цель сборки выбирает ДВИЖОК образа.
      #   imageworker-vulkan (дефолт) — релизный Vulkan-ассет sd.cpp: работает на
      #     AMD/Intel и на NVIDIA там, где проброшен nvidia-ICD (Linux-хосты);
      #   imageworker-cuda — сборка sd.cpp из исходников с CUDA (Linux-CUDA-релиза
      #     у проекта нет). Нужна там, где Vulkan-NVIDIA в контейнер не приезжает
      #     (например, Windows + Docker Desktop/WSL2: nvidia-smi видит карту, а
      #     nvidia_icd.json отсутствует, и Vulkan-движок уходит на CPU: 251 с на
      #     512x512/8 шагов против 9-76 с на GPU). Для CUDA обязателен
      #     RUNTIME_BASE с CUDA-рантаймом — см. IMAGE_WORKER_RUNTIME_BASE ниже.
'@ = @'
      # The build target selects the ENGINE of the image:
      #   imageworker-vulkan (default) — release Vulkan asset of sd.cpp: works on
      #     AMD/Intel and on NVIDIA where the nvidia-ICD is passed through (Linux);
      #   imageworker-cuda — sd.cpp built from source with CUDA (the project has no
      #     Linux-CUDA release). Needed where Vulkan-NVIDIA does not reach the
      #     container (e.g. Windows + Docker Desktop/WSL2: nvidia-smi sees the card
      #     but nvidia_icd.json is missing, so the Vulkan engine falls back to CPU:
      #     251 s for 512x512/8 steps vs 9-76 s on GPU). CUDA requires RUNTIME_BASE
      #     with a CUDA runtime — see IMAGE_WORKER_RUNTIME_BASE below.
'@

@'
    # Тот же GPU-путь, что у cppworker-gpu выше: nvidia-container-runtime.
'@ = @'
    # Same GPU path as cppworker-gpu above: nvidia-container-runtime.
'@

@'
      # Внутренний порт движка (loopback внутри контейнера). Не путать с 18093:
      # наружу торчит API воркера, иначе клиенты обойдут нормализацию запросов.
'@ = @'
      # Internal engine port (loopback inside the container). Not to be confused
      # with 18093: only the worker API is published, otherwise clients would
      # bypass request normalisation.
'@

@'
      # Каталог bundle'ов image-моделей внутри контейнера (host: <MODELS_DIR>/image).
'@ = @'
      # Image-model bundle directory inside the container (host: <MODELS_DIR>/image).
'@

@'
      # URL воркера «как его видят клиенты»: подставляется в response_format:"url".
      # Дефолт — локальный клиент на этой машине (порт опубликован ниже).
      # Для клиентов снаружи: SDWORKER_BASE_URL=http://<хост>:18093 в .env.
'@ = @'
      # Worker URL as CLIENTS see it: used in response_format:"url".
      # Default targets a local client (the port is published below).
      # For external clients set SDWORKER_BASE_URL=http://<host>:18093 in .env.
'@

@'
      # HF-слой image-bundle'ов (gated модели / зеркало).
'@ = @'
      # HF layer for image bundles (gated models / mirror).
'@

@'
      # Vulkan-ICD NVIDIA приезжает вместе с драйвером: capability `graphics`
      # ОБЯЗАТЕЛЕН (compute,utility дают только CUDA/NVML).
'@ = @'
      # The NVIDIA Vulkan ICD ships with the driver: the `graphics` capability is
      # REQUIRED (compute,utility provide only CUDA/NVML).
'@

@'
      # ─── Авторегистрация в балансере (SDWORKER_* — имена из Go-кода) ───────
      # BALANCER_URL — каноническое имя стека: в сценарии 2 из .env сюда
      # попадает адрес УДАЛЁННОГО балансера, и воркер регистрируется там.
'@ = @'
      # --- Balancer auto-registration (SDWORKER_* — the names the Go code reads)
      # BALANCER_URL is the canonical stack name: in the worker scenario the .env
      # supplies the REMOTE balancer address and the worker registers there.
'@

@'
      # host, под которым воркер виден балансеру: DNS-имя сервиса в этой сети.
'@ = @'
      # Host under which the balancer sees this worker: service DNS name on ol-net.
'@

@'
      # ─── ВСТРОЕННЫЙ АГЕНТ МЕТРИК (R-Image, 2026-10-07) ────────────────────
      #
      # ЗАЧЕМ. Раньше метрики image-бэкенда собирал отдельный контейнер `agent`.
      # Он регистрировался в балансере ОТДЕЛЬНОЙ записью (host=cppworker-gpu-agent,
      # backendType=llama_cpp), то есть об одном физическом воркере появлялись две
      # записи. У самой image-записи при этом оставались hasAgent=false и
      # agentPort=18032 (порт ЧУЖОГО контейнера), а gpuMemory/vramUsagePercent
      # приходили нулями — страница модели в WebUI выглядела «странно».
      #
      # ТЕПЕРЬ: воркер поднимает агента в СВОЁМ процессе и регистрируется под ТЕМ
      # ЖЕ ID (SDWORKER_BACKEND_ID). Балансер в ветке «backend exists» ставит
      # HasAgent=true и пишет метрики в ту же запись. Отдельный контейнер для
      # image-воркера больше не нужен.
      #
      # AGENT_EMBEDDED выключен по умолчанию: на стендах, где внешний агент уже
      # поднят, второй сборщик метрик не нужен.
'@ = @'
      # --- EMBEDDED METRICS AGENT ---------------------------------------------
      #
      # Previously a separate `agent` container collected image-backend metrics. It
      # registered as a SEPARATE record (host=cppworker-gpu-agent,
      # backendType=llama_cpp), so one physical worker produced two records, while
      # the image record itself kept hasAgent=false and agentPort=18032 (a FOREIGN
      # container's port) with gpuMemory/vramUsagePercent reported as zeros.
      #
      # NOW the worker starts an agent IN ITS OWN PROCESS and registers under the
      # SAME ID (SDWORKER_BACKEND_ID). The balancer's "backend exists" branch sets
      # HasAgent=true and writes metrics into the same record. The image worker no
      # longer needs a separate container.
      #
      # Set IMAGE_WORKER_AGENT_EMBEDDED=on to enable (default below).
'@

@'
      # Порт /health и метрик встроенного агента. НЕ 18032: там слушает внешний
      # контейнер `agent`, а балансер опрашивает метрики по agentPort записи.
'@ = @'
      # /health and metrics port of the embedded agent. NOT 18032: the external
      # `agent` container listens there, and the balancer polls metrics on the
      # record's own agentPort.
'@

@'
      # Вместимость: столько же, сколько воркер обслуживает параллельно
      # (SDWORKER_MAX_CONCURRENT). Значение >0 перекрывает автоопределение.
'@ = @'
      # Capacity: same as the number of requests the worker serves in parallel
      # (SDWORKER_MAX_CONCURRENT). A value >0 overrides auto-detection.
'@

@'
      # Токен, который ждёт сам image-воркер на защищённых эндпоинтах: балансер
      # получит его в записи бэкенда и сможет авторизоваться при проксировании.
'@ = @'
      # Token the image worker itself expects on protected endpoints: the balancer
      # receives it in the backend record and can authorise when proxying.
'@

@'
      # Тот же каталог моделей, что у cppworker (../models): текстовые .gguf в
      # корне, image-bundle'ы — в image/. Внутри тома же лежат temp-загрузки
      # HF (../models/downloads) — иначе .download на 6-12 GB забивал бы слой
      # контейнера и терялся при пересоздании.
'@ = @'
      # Same models directory as cppworker (../models): text .gguf in the root,
      # image bundles under image/. The volume also holds HF temp downloads
      # (../models/downloads) — otherwise a 6-12 GB .download would fill the
      # container layer and be lost on re-creation.
'@

@'
      # Сгенерированные картинки (response_format:"url") — забираются с хоста.
'@ = @'
      # Generated images (response_format:"url") — collected from the host.
'@

@'
      # LoRA/ESRGAN-апскейлеры (опционально): движок сканирует каталоги в рантайме.
      # Раскомментируйте, положив файлы в ../models/lora и ../models/upscalers:
'@ = @'
      # LoRA/ESRGAN upscalers (optional): the engine scans these directories at
      # runtime. Uncomment after putting files into ../models/lora and
      # ../models/upscalers:
'@

@'
      # API воркера. Публикуется наружу намеренно: response_format:"url" отдаёт
      # ссылку на этот порт, и по нему же можно ходить в обход балансера.
'@ = @'
      # Worker API. Published on purpose: response_format:"url" returns a link to
      # this port, and it can also be used to bypass the balancer.
'@

@'
    # Phase 8 (2026-10-03): ждём ГОТОВЫЙ балансер, а не просто «запущенный».
    # Раньше зависимости не было вовсе: воркер стартовал первым, первый POST
    # /api/v1/backends уходил в недоступный порт и повторялся через 30 с — в
    # едином стенде это выглядело как «image-бэкенд не появился в балансере».
    #
    # R-Image (2026-10-07): зависимость ОПЦИОНАЛЬНА (required:false) — иначе
    # сценарий «только бэкенд» (--profile worker, балансер на другой машине)
    # отвергался целиком: compose валидирует depends_on даже для сервисов, не
    # входящих в активный профиль. Без loadbalancer в профиле воркер просто
    # регистрируется по BALANCER_URL во внешнем балансере.
'@ = @'
    # Wait for a READY balancer, not merely a started one: without the dependency
    # the worker started first, its first POST /api/v1/backends hit a closed port
    # and retried every 30 s — which looked like "the image backend never appeared
    # in the balancer".
    #
    # OPTIONAL dependency (required:false), otherwise the backend-only scenario
    # (`--profile worker`, balancer elsewhere) was rejected entirely: compose
    # validates depends_on even for services outside the active profile. With no
    # loadbalancer in the profile the worker simply registers via BALANCER_URL
    # with the external balancer.
'@

@'
      # Через сам бинарь (как у cppworker): /health отвечает 200 и без модели,
      # поэтому idle-unload не переводит контейнер в unhealthy.
'@ = @'
      # Through the binary itself (as cppworker does): /health answers 200 even
      # without a model, so idle-unload never marks the container unhealthy.
'@

@'
      # Дольше, чем у cppworker: первый старт движка = прогрев Vulkan.
'@ = @'
      # Longer than cppworker: the first engine start warms up Vulkan.
'@
}

$applied = 0
$missing = @()
foreach ($k in $map.Keys) {
    $old = $k.Trim("`r", "`n")
    $new = $map[$k].Trim("`r", "`n")
    $count = ([regex]::Matches($c, [regex]::Escape($old))).Count
    if ($count -ne 1) {
        $missing += "найдено $count раз: " + ($old -split "`n")[0].Trim()
        continue
    }
    $c = $c.Replace($old, $new)
    $applied++
}
Set-Content -Path $path -Value $c -NoNewline
"заменено блоков: $applied"
if ($missing.Count) { "НЕ НАЙДЕНЫ:"; $missing | ForEach-Object { "  $_" } }

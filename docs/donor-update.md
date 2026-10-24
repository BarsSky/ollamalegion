# Обновление узла-донора (образ cppworker)

Донор запускает тот же образ cppworker, что и мастер, только с
`CPPWORKER_RPC_SERVER_ENABLED=true` (внутри поднимается `rpc-server`, порт 50052).
Образ на доноре обновляется вручную: реестра в стенде нет, `docker pull` не
используется.

## Что нужно

- ssh-доступ к донору (в этом стенде `ssh legion2`, Windows + Docker Desktop);
- образ на мастере, например `ollama-legion/cppworker:gpu-r216c-reasoning`;
- свободное место на доноре: tar-образ 6.8 GB образа занимает ~2.5 GB
  (мастер) и столько же при распаковке.

## Порядок (проверено 2026-10-21)

1. Снять описание текущего контейнера. Оно нужно, чтобы перенести в новый
   контейнер ВСЁ окружение, порты и монтирования: донор запускается через
   `docker run`, compose-файла у него нет.

   ```powershell
   ssh legion2 'docker inspect cppworker-node2' | Out-File -Encoding utf8 _diag\donor-full.json
   ```

2. Сгенерировать CMD-скрипт обновления (он же переносит env/ports/binds):

   ```powershell
   node scripts/make-donor-update-cmd.js _diag/donor-full.json ollama-legion/cppworker:gpu-r216c-reasoning
   ```

   Скрипт пишет `deployments/donor/update.cmd`. Проверьте в нём строку `-v`:
   host-часть должна быть windows-путём (`C:\ollama\ollamalegion\models`), а
   путь внутри контейнера остаться с прямыми слэшами (`/app/models`).

3. Передать образ и скрипт на донор:

   ```powershell
   docker save ollama-legion/cppworker:gpu-r216c-reasoning -o _diag\cpp-update.tar
   scp _diag\cpp-update.tar legion2:C:/ollama/cpp-update.tar
   scp deployments\donor\update.cmd legion2:C:/ollama/update-donor.cmd
   ssh legion2 'docker load -i C:\ollama\cpp-update.tar'
   ```

4. Остановить старый контейнер и поднять новый:

   ```powershell
   ssh legion2 'cmd /c C:\ollama\update-donor.cmd'
   ```

   Скрипт переименовывает старый контейнер в `cppworker-node2-old`,
   ОСТАНАВЛИВАЕТ его (остановка освобождает порты; `rename` их не отдаёт) и
   запускает новый с тем же именем и настройками.

   > Осторожно: не запускайте скрипт дважды без проверки — при повторном запуске
   > он удалит уже переименованный `-old`. Проверяйте `docker ps -a` перед
   > повтором.

5. Проверить, что узел жив и версия новая:

   ```powershell
   ssh legion2 'docker ps'
   curl.exe -s -H "X-API-Token: <token>" http://192.0.2.10:18092/api/models
   ```

   В ответе: `rpc_server.enabled=true`, `running=true`, `port=50052`,
   `rpc_layout.stuck_suspected=false`. Версия образа видна в `docker ps`.

6. Проверить, что раскладка реально применяется (с мастера):

   ```powershell
   curl.exe -s -H "X-API-Token: <token>" http://localhost:18092/api/v1/cppworker/rpc-peers
   ```

   Ожидается `reachable=1`, `servers[0].reachable=true`. Затем загрузить модель
   и посмотреть `device_layers_exact` в `/api/models`: должен появиться
   `RPC0` с адресом донора, а `layout_degraded=false`.

7. Проверить освобождение VRAM донора: выгрузить модель
   (`POST /api/models/unload?name=...`) и убедиться, что
   `docker exec cppworker-node2 nvidia-smi` вернулся к базовому уровню.

## Откат

Старый контейнер остаётся остановленным под именем `cppworker-node2-old`:

```powershell
ssh legion2 'docker rm -f cppworker-node2'
ssh legion2 'docker rename cppworker-node2-old cppworker-node2'
ssh legion2 'docker start cppworker-node2'
```

Если контейнер уже удалён, старый образ остаётся на доноре
(`ollama-legion/cppworker:gpu-<прежний-тег>`) — контейнер можно поднять тем же
скриптом, указав прежний тег в `make-donor-update-cmd.js`.

## Что проверить в новом образе

- `grep -c keepalive /app/rpc-server` > 0 — R214 (TCP keepalive на сокете
  раскладки);
- cancel загрузки (`unload?force=true` во время загрузки) отвечает
  `load_cancelled` — R215;
- `/api/models` отдаёт `rpc_layout` и `loadingProgressPct` — R215/R216;
- `/v1/models` отдаёт `context_length` и `max_context_length` — R216.

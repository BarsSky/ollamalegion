# Updating the donor node (cppworker image)

The donor runs the same cppworker image as the master, only with
`CPPWORKER_RPC_SERVER_ENABLED=true` (`rpc-server` is started inside, port 50052).
The donor image is updated by hand: the stand has no registry and `docker pull` is
not used.

## Requirements

- ssh access to the donor (in this stand `ssh legion2`, Windows + Docker Desktop);
- the image on the master, for example `ollama-legion/cppworker:gpu-r216c-reasoning`;
- free space on the donor: the 6.8 GB tar archive takes ~2.5 GB on the master and
  the same amount when unpacked.

## Procedure (verified 2026-10-21)

1. Dump the current container description. It is needed to carry ALL environment
   variables, ports and mounts into the new container: the donor is started with
   `docker run` and has no compose file.

   ```powershell
   ssh legion2 'docker inspect cppworker-node2' | Out-File -Encoding utf8 _diag\donor-full.json
   ```

2. Generate the update CMD script (it also carries env/ports/binds):

   ```powershell
   node scripts/make-donor-update-cmd.js _diag/donor-full.json ollama-legion/cppworker:gpu-r216c-reasoning
   ```

   The script writes `deployments/donor/update.cmd`. Check the `-v` line in it: the
   host part must be a Windows path (`C:\ollama\ollamalegion\models`), while the
   path inside the container must keep forward slashes (`/app/models`).

3. Transfer the image and the script to the donor:

   ```powershell
   docker save ollama-legion/cppworker:gpu-r216c-reasoning -o _diag\cpp-update.tar
   scp _diag\cpp-update.tar legion2:C:/ollama/cpp-update.tar
   scp deployments\donor\update.cmd legion2:C:/ollama/update-donor.cmd
   ssh legion2 'docker load -i C:\ollama\cpp-update.tar'
   ```

4. Stop the old container and start the new one:

   ```powershell
   ssh legion2 'cmd /c C:\ollama\update-donor.cmd'
   ```

   The script renames the old container to `cppworker-node2-old`, STOPS it (stopping
   releases the ports; `rename` does not) and starts a new one with the same name
   and settings.

   > Careful: do not run the script twice without checking - on a second run it
   > deletes the already renamed `-old` container. Check `docker ps -a` before
   > repeating.

5. Verify the node is alive and the version is new:

   ```powershell
   ssh legion2 'docker ps'
   curl.exe -s -H "X-API-Token: <token>" http://192.0.2.10:18092/api/models
   ```

   The response must contain: `rpc_server.enabled=true`, `running=true`,
   `port=50052`, `rpc_layout.stuck_suspected=false`. The image version is visible in
   `docker ps`.

6. Verify the layout is actually applied (from the master):

   ```powershell
   curl.exe -s -H "X-API-Token: <token>" http://localhost:18092/api/v1/cppworker/rpc-peers
   ```

   Expected: `reachable=1`, `servers[0].reachable=true`. Then load a model and check
   `device_layers_exact` in `/api/models`: `RPC0` with the donor address must appear,
   and `layout_degraded=false`.

7. Verify the donor VRAM is released: unload the model
   (`POST /api/models/unload?name=...`) and make sure
   `docker exec cppworker-node2 nvidia-smi` returns to the baseline level.

### Only the master loads the model (R304)

On the donor itself a model load is now **refused**: `POST /api/models/load`
answers `409 donor_cannot_host_models`, and `/api/models` reports `role=donor`,
`can_load_own_models=false`. This is not a failure but the role: the donor's VRAM is
handed to the master's model layers, and its own copy would take exactly the
gigabytes the donor was assigned for.

In practice: load/unload the model **from the master** (and through the balancer -
`POST /api/v1/backends/<master>/models`), not on the donor. If the donor's
`/api/models` shows `own_copies`, those are copies left over from a previous role:
unload them (`POST /api/models/unload?name=...` on the donor is allowed) or clear
the role in the panel. Details: `docs/operations/rpc-donor-vram-lifecycle.md`,
section "A donor does not keep its own model copies (R304)".
## Rollback

The old container stays stopped under the name `cppworker-node2-old`:

```powershell
ssh legion2 'docker rm -f cppworker-node2'
ssh legion2 'docker rename cppworker-node2-old cppworker-node2'
ssh legion2 'docker start cppworker-node2'
```

If the container is already removed, the old image stays on the donor
(`ollama-legion/cppworker:gpu-<previous-tag>`) - the container can be started with
the same script by passing the previous tag to `make-donor-update-cmd.js`.

## What to check in the new image

- `grep -c keepalive /app/rpc-server` > 0 - R214 (TCP keepalive on the layout
  socket);
- cancelling a load (`unload?force=true` while loading) answers `load_cancelled` -
  R215;
- `/api/models` returns `rpc_layout` and `loadingProgressPct` - R215/R216;
- `/v1/models` returns `context_length` and `max_context_length` - R216.

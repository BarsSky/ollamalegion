# WebUI login: where it lives and what is needed on a new machine

Date: 2026-10-22 (R222). Applies to the `webui` and `balancer` images.

## In short

**Authentication lives in the balancer, not in the `webui` image.** The `webui` image is nginx
with static files: it serves `index.html`/`js`/`css` and proxies `location /api/` to
`API_HOST:API_PORT` (by default `loadbalancer:18081`). That is why moving only the
`webui` image to another machine gives you neither a login screen nor roles: there is
nobody to serve the `/api/v1/auth/*` requests.

Since R222 the page **no longer carries an API token in itself**: the browser works through a session
(cookie `ol_session` + `X-CSRF-Token`), and the operator enters the break-glass token login
themselves.

## What is needed for the login to appear

1. **The balancer is next to it and reachable** from the `webui` container:
   `API_HOST`/`API_PORT` (or a compose network where the balancer is named `loadbalancer`).
2. **Users are defined on the balancer** — in either of two ways:
   - env `LB_ADMIN_USER` + `LB_ADMIN_PASSWORD`: on startup the balancer creates
     `LB_USERS_FILE` (by default `/app/data/users.json`) with a single administrator;
   - a ready-made `users.json` file in a mounted directory (passwords — hashes only,
     PBKDF2-HMAC-SHA256; nobody writes the file by hand — the balancer creates it from
     `LB_ADMIN_USER`/`LB_ADMIN_PASSWORD`, and the "Users" section edits it afterwards).
3. **A volume on the balancer's `/app/data`** (`balancer_data` in compose): without it `users.json`
   disappears when the container is recreated.
4. **`deployments/.env` is not part of the image** — on a new machine the variables have to be
   set again (`--env-file`, `-e` or your own `.env`).

Example for a single machine (without compose):

```bash
docker network create ol-net

docker run -d --name balancer --network ol-net \
  -e LB_API_TOKEN='<stack token>' \
  -e LB_ADMIN_USER=admin -e LB_ADMIN_PASSWORD='<password>' \
  -e LB_USERS_FILE=/app/data/users.json \
  -v balancer_data:/app/data \
  -p 18080:18080 -p 18081:18081 \
  ollama-legion/balancer:<tag>

docker run -d --name webui --network ol-net \
  -e API_HOST=balancer -e API_PORT=18081 \
  -p 18083:80 \
  ollama-legion/webui:<tag>
```

## WHERE users.json LIVES (a frequent cause of "the file is missing")

The file is created by the **balancer**, not by `webui`. The path is `LB_USERS_FILE`:

| How it was started | Path | Visible on the host? |
|---|---|---|
| compose stack (`deployments/.env`) | `/app/data/users.json` (volume `balancer_data`) | Docker volumes, not a repository directory |
| `docker run` without `LB_USERS_FILE` | `config/users.json` **inside the container** (`/app/config/...`) | no: the file is ephemeral and will be lost when the container is recreated |

That is why "the repository's `config` folder has no `users.json`" is the expected state:
in the compose stack the file will not be there at all. You have to check inside the container:

```bash
docker exec <balancer> sh -c 'printenv LB_USERS_FILE; ls -l /app/data /app/config 2>&1'
docker logs <balancer> 2>&1 | grep -i 'auth:'
# expected: "auth: WebUI-вход включён ... users_file=/app/data/users.json users=1"
# or:       "auth: пользователей нет — вход в WebUI недоступен ..."
```

## IF THE LOGIN DID NOT APPEAR (work through it in order)

1. **the env was given to the wrong container.** `LB_ADMIN_USER`/`LB_ADMIN_PASSWORD` are read
   by the balancer ONLY. The `webui` image has no such variables at all — it is nginx with static files.
   `docker inspect <balancer> --format '{{range .Config.Env}}{{println .}}{{end}}'
   | findstr LB_ADMIN` must show both variables.
2. **the container was restarted, not recreated.** `docker compose restart` does NOT
   re-read `.env`: the new env reaches the container only on recreation —
   `docker compose up -d --force-recreate <balancer>`.
3. **only webui is running** (with no balancer next to it): there is nobody to serve
   `/api/v1/auth/*` — the login screen is there (it is static), but you cannot log in.
4. **an old balancer image** (before R201): there are no `/api/v1/auth/*` handlers,
   and the users file is not created under any env.
5. **a failure writing the file.** If `LB_USERS_FILE` points to a path that is not mounted
   or not accessible, the balancer logged the WARN "administrator created in MEMORY, but
   the users.json file was not written" (R229) — login works until the container is recreated, then
   disappears. The fix: `LB_USERS_FILE=/app/data/users.json` + a volume on `/app/data`.

6. **webui answers 503 on `/api/v1/auth/me`, while the balancer answers 200 directly.** That was the case
   before R229: `location /api/` (and `/api/v1/events|image|gguf`, `/ws/`) held a
   STATIC `proxy_pass` to the balancer's name, and nginx memorized the IP when it started.
   After the balancer was recreated (`up -d`) the IP changed — the panel kept talking to the old one and
   returned 503 until webui was restarted. Fixed: the upstream is a variable with
   `resolver 127.0.0.11` (the change is in `webui/nginx.conf`, webui image R229+).
   Check: recreate the balancer WITHOUT touching webui and repeat `/api/v1/auth/me`
   through the panel — it should become 200 within ~30 s.

Diagnostics in one command: `pwsh -File scripts/diagnose-webui-auth.ps1`
(checks the balancer's env, the path and the presence of the file, `/api/v1/auth/me`, the logs).
The expected answer of `/api/v1/auth/me`: `{"authenticated":false,"usersConfigured":true}`.
`usersConfigured:false` = there are no users (see items 1–2).

## Verification checklist (curl)

```bash
# 1. is login configured?
curl -s http://host:18081/api/v1/auth/me
# {"authenticated":false,"usersConfigured":true}

# 2. login by password → cookie + csrf
curl -si -X POST http://host:18081/api/v1/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"name":"admin","password":"<password>"}' -c cookie.txt
# Set-Cookie: ol_session=...; HttpOnly; SameSite=Lax
# {"success":true,"user":{"name":"admin","role":"admin"},"csrf":"..."}

# 3. reading with the session
curl -s -b cookie.txt http://host:18081/api/v1/models >/dev/null && echo read-ok

# 4. mutation without CSRF → 403, with CSRF → 200
curl -s -o /dev/null -w '%{http_code}\n' -b cookie.txt -X PUT \
  http://host:18081/api/v1/cppworker/model-profiles/probe -d '{}'
curl -s -o /dev/null -w '%{http_code}\n' -b cookie.txt -X PUT \
  -H 'X-CSRF-Token: <csrf>' \
  http://host:18081/api/v1/cppworker/model-profiles/probe -d '{}'
```

`usersConfigured:false` means there are no users: the panel shows a "login is not configured"
bar and offers to log in with a token. This state is fixed by the first item of the list above.

## Roles

| Role | What it can do |
|---|---|
| `admin` | everything: loading/unloading models, profiles, settings, backends, users |
| `viewer` ("guest") | reading and generation; the server rejects any mutations (HTTP 403), admin controls are hidden in the UI |

A guest does not see anything marked `data-admin-only` in the markup; the rule also applies to
elements rendered later (model cards, tables) — the `MutationObserver` in `webui/js/modules/auth.js`
is responsible for that.

Users are managed by the **Settings → Users** section (R222). The API is the same:
`GET/PUT/DELETE /api/v1/auth/users`. Since R225 this API is available **to the administrator only** —
including reading: previously a viewer could read the list of users with their roles.

**Active sessions (R225).** The same section contains a table of logins: user, role, login time and
last activity time, address and browser, and an "this tab" marker. Someone else's session can be
terminated with a button (the full cookie id is never shown: only a short prefix is exposed, and
revocation goes by that same prefix). API: `GET /api/v1/auth/sessions`, `DELETE /api/v1/auth/sessions?id=<8 characters>`.

**Password policy (R225).** The minimum of 8 characters is enforced **on the server** (`PUT /auth/users` →
400 "password shorter than 8 characters"), not only in the browser.

## Break-glass login by token

The login screen has a "Break-glass login by API token" field. The value:

- is verified by a request that requires administrator rights (`GET /api/v1/auth/users`);
- is stored only in the tab's `sessionStorage` (close the tab — the token is gone);
- is placed into `window.WEBUI_CONFIG.API_TOKEN`, which the modules read;
- is cleared by the "Log out" button together with the session.

The token itself no longer reaches the served page — that was the R201/R202 hole: while it
sat in `config.js`, anyone who opened the panel acted as an administrator, and the viewer role
restricted nothing.

## Limitations

1. Sessions survive a balancer restart (R223): they live in `LB_SESSIONS_FILE`
   (by default `/app/data/sessions.json` — next to `users.json`, the same volume; mode
   `0600`). Expired sessions are dropped on load, and `logout` and a password change
   remove them from the file. If the variable is empty, sessions live in memory only.
2. Several balancer instances do not share sessions: the file is read at startup and
   written by each process, there is no shared storage.
3. Until you log in, the page does not send any requests to `/api/` at all: the `fetch` wrapper in `auth.js`
   waits for `/auth/me` and returns a synthetic 401 without the network (R223). Previously, on the login screen,
   a pile of 401s hung in the console and in the balancer logs.
4. The token of external clients (Cline, OpenWebUI) is not tied to WebUI users: it is
   a separate mechanism (`X-API-Token`), and it remains.

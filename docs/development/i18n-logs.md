# Log and error language (R234)

The language in which the balancer, cppworker and agent write logs and answer with errors is
set **on the cluster**, not in the browser. The WebUI interface language (`langSelect`) does
not affect logs: these are two different settings, and confusing them is the most common
reason for "I switched the language but the logs are still Russian".

## What counts as localised

| What | Where | How it changes |
|---|---|---|
| Logs of the Go processes (balancer, cppworker, agent) | container stderr/stdout, WebUI live logs | cluster language setting |
| REST API errors (`{"error": ...}`) | `/api/*` responses | cluster language setting |
| WebUI interface texts | panel pages | interface switcher (localStorage) |
| Variable names, commands, paths, HTTP codes | everywhere | **never translated** |
| Strings the code prints as facts (llama.cpp WARN/ERROR, `rpc-server` output) | logs | kept as-is: operators grep for them |

## Source priority

```
flag -log-lang  >  OLLAMALEGION_LOG_LANG / LB_LOG_LANG  >  config.json logging.language
                >  LANG / LC_ALL  >  ru
```

For workers the first step is `<COMPONENT>_LOG_LANG` (for example `CPPWORKER_LOG_LANG`), so
the language of one container can be overridden without touching the whole stand.

Values: `ru`, `en`, `auto` (`auto` = take it from the container locale, `ru` when unclear).

## The balancer is the source of truth

The balancer sets the language and serves it to the workers:

```
GET /api/v1/i18n
  -> {"language":"ru|en","resolved":"ru|en","supported":["ru","en"],
      "source":"env|config|locale"}

PUT /api/v1/i18n          (administrator only)
  <- {"language":"ru|en|auto"}
  -> {"success":true,"language":"en","message":"..."}
```

* `language` in GET is the **setting** (may be `auto`), `resolved` is the **active language**
  the messages are actually written in.
* `PUT` applies the language immediately (`i18n.Set`) and stores it in the config's
  `logging.language`; `auto` is stored as `auto`, so the next start looks at the locale again.
* The same choice is available through `GET/PUT /api/v1/cluster/config` (field
  `logLanguage`) - the WebUI settings page uses it when saving the form.

Workers (`cppworker`, `agent`) ask `GET /api/v1/i18n` every
`OLLAMALEGION_LOG_LANG_SYNC_SEC` (60 s by default) and apply the answer. A language switch in
the WebUI therefore reaches the whole stand **without restarting containers**. If the
balancer is unreachable, a worker keeps its local setting (the language is not reset by a
network glitch) and logs one line instead of one line per minute.

## How to enable

**Through env** (start value, takes priority over the config):

```dotenv
# deployments/.env
OLLAMALEGION_LOG_LANG=en          # ru | en | auto
# per-container overrides:
CPPWORKER_LOG_LANG=ru
AGENT_LOG_LANG=en
OLLAMALEGION_LOG_LANG_SYNC_SEC=60 # how often workers ask the balancer
```

**Through the WebUI**: "Settings" -> the "Log and error language" section -> pick a language ->
"Apply". Administrator only; a guest sees the setting but the button is disabled and the
status explains why.

**Through the API**:

```bash
curl -s -X PUT http://localhost:18081/api/v1/i18n \
  -H "X-API-Token: $CPPWORKER_API_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"language":"en"}'
```

## Adding a message

1. Key and both texts in `internal/i18n/messages.go`:
   `"api.i18n.unsaved": {RU: "...", EN: "..."}`.
   The `%s/%d/%v` verbs in `ru` and `en` must match - `TestCatalogVerbParity` enforces it.
2. In code use `i18n.T("key", args...)` (or `i18n.Logf("key", args...)` for logs).
3. Do not rename keys: the remaining untranslated places are counted by key
   (`scripts/i18n-scan.py`) and the key is cited in this document.

## What is translated and what is not

The catalog grows as the code is touched and **is not complete yet**: Russian log strings
that have not been moved into the catalog still exist. Their number is counted and printed by

```bash
python scripts/i18n-scan.py           # summary + budget
python scripts/i18n-scan.py --list    # files and counts only
```

The script is a ratchet: the budget (`scripts/i18n-budget.json`) may only go down. That is
more honest than declaring "everything is localised" while some strings are still hard-coded
Russian.

## Checks

| Check | What it guarantees |
|---|---|
| `go test ./internal/i18n/` | value parsing, env/locale priority, fallback, verb parity, thread safety, balancer sync |
| `go test ./internal/api/ -run TestI18n` | GET/PUT contract: admin rights, rejection of garbage, `auto`, saving to the config |
| `node webui/js/modules/i18n-logs.test.js` | the WebUI switch: read, apply, 403, unreachable balancer |
| `internal/api` `TestI18nKeyParity_EN_RU` | `webui/js/i18n/{ru,en}.js` have no keys in only one language |
| `scripts/i18n-scan.py` | the number of untranslated log strings does not grow |

## Limitations

- A language switch **does not rewrite history**: records already written stay in the previous
  language. The switch applies to new messages (and to API responses from that moment on).
- The balancer and worker languages converge within one sync interval (<= 60 s by default)
  when the language is changed locally in a container instead of through the balancer.
- Messages that are not in the catalog are printed as before (the original Russian text).

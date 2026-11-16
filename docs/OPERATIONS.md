# Operations

Deploy, configure, back up, upgrade, and troubleshoot Guvna. This is the one operations document; the canonical *semantics* (rotation, quarantine classes, cool-offs, restart behavior) live in [ARCHITECTURE.md](ARCHITECTURE.md) and are not restated here.

Every behavior below was either verified live against the code at `c95435a` or is explicitly marked otherwise — see the verification footer.

## 1. What runs in production

| Piece | What it is | State it owns |
|---|---|---|
| `guvna` binary | The gateway. One process, one port (default 20128, loopback bind by default). | None in memory that must survive restarts (§10). |
| Data dir (default `~/.guvna/`, `-data` flag) | `guvna.db` (SQLite telemetry), `chains.yaml` (runtime chains). | The only persistent state. Back both up (§9). |
| `config.yaml` | Providers, key env names, rotation, quarantine, config chains. | In git. Keys are never in it — only env var *names*. |
| `.env` / env file | `ADMIN_KEY`, `API_KEYS`, provider keys. | Never in git (`.env*` is gitignored). `chmod 600`. |
| Reverse proxy (caddy, etc.) | TLS termination when exposed. | Not part of Guvna (§5). |

Measured at `c95435a`: binary 16.7 MB, idle RSS ~12 MB, distroless image ~19 MB.

## 2. Install paths

### From a clone (verified)

```sh
git clone https://github.com/Hesamation/guvna && cd guvna
go build -o guvna ./cmd/guvna
./guvna -config config.yaml   # needs ADMIN_KEY + API_KEYS in env
```

Requires Go 1.26+ (`go.mod` says `go 1.26.6`).

### `go install` (verified — v0.1.2 fixed the module path)

```sh
go install github.com/Hesamation/guvna/cmd/guvna@latest
```

Tag `v0.1.0` predates the module-path move and was briefly the reason `@latest` failed; from `v0.1.2` (2026-11-16) `@latest` installs correctly.

### Release image

Tagged releases (`v*`) publish a multi-arch image (amd64/arm64) to `ghcr.io/Hesamation/guvna` via `.github/workflows/release.yml`, gated on the same test suite as CI. The `docker run` path is in §4.

## 3. Configuration reference

Read from `internal/config/config.go` and `cmd/guvna/main.go` at `c95435a`. Nothing here is guessed.

### Flags

| Flag | Default | Meaning |
|---|---|---|
| `-config` | `config.yaml` | Path to config file. **Overridden** by `~/.guvna/config.yaml` when that file exists — the gateway logs the override and the effective path explicitly (verified at `cccfcb8+`). |
| `-data` | `~/.guvna` | Data directory. Created with mode `0700` if missing (verified). Holds `guvna.db` + `chains.yaml`. |
| `-healthcheck` | off | Probe `http://127.0.0.1:<port>/healthz` (3s timeout) and exit 0/1. No server is started. Used as the container healthcheck. |

### Environment variables

| Variable | Required | Meaning |
|---|---|---|
| `ADMIN_KEY` | Yes — gateway refuses to start without it (exit 1, verified) | Admin Bearer token. Valid for all endpoints including `/admin/*` and `/metrics`. |
| `API_KEYS` | Yes — same refusal | Comma-separated client Bearer tokens for `/v1/*`. Whitespace around entries is trimmed; empty entries ignored. Client keys cannot read `/admin/*` (401). |
| `<PROVIDER>_KEY` names from `key_envs`/`key_env` | Yes, per provider | Provider API keys. **Values are resolved lazily per request** (code-read: `internal/keypool` — env value changes take effect without a restart). Pool *membership* (which env names) is config-time and needs a restart. An unset key env fails requests at runtime (§12), it does not fail startup. |
| `GOMEMLIMIT` | No | Go runtime memory limit. Compose sets `256MiB`. Not a Guvna flag. |
| `GUVNA_URL`, `GUVNA_ADMIN_KEY` | No (CLI only) | `guvna-cli` defaults: gateway URL and admin key. |

Quote gotcha (bites in two ecosystems): Docker `--env-file` does **not** strip quotes around values; systemd `EnvironmentFile=` does not either (shell `source` does). Keep env values unquoted.

### `config.yaml` fields

```yaml
port: 20128                 # optional, default 20128
bind: 127.0.0.1             # optional; loopback by default — set 0.0.0.0 only behind a trusted proxy/firewall
telemetry:                  # optional; unset keeps every telemetry row forever
  retain_days: 30           # opt-in pruning: rows older than N days are deleted at startup and every 6h
providers:
  - name: groq              # required, unique; also the default prefix
    type: openai            # required: "openai" or "gemini" (anything else = startup error)
    base_url: https://api.groq.com/openai   # required; adaptor appends /v1/chat/completions
    key_env: GROQ_KEY       # single-key pool — OR key_envs, never both (startup error)
    key_envs: [GROQ_1_KEY, GROQ_2_KEY]      # rotation pool
    rotation: round_robin   # optional: round_robin (default) | least_used | sequential
    quarantine:             # optional per-provider overrides; zero fields fall back to defaults
      auth: 24h             # default 24h  (401/403)
      rate_limit: 60s       # default 60s  (429)
      transient: 60s        # default 60s  (5xx / network)
    models: [llama-3.3-70b-versatile]  # passthrough strip list
    prefixes: [groq]        # optional; defaults to [name]
chains:                     # optional; config-defined chains (undeletable via API)
  - name: mychain
    steps:
      - provider: groq
        model: llama-3.3-70b-versatile
        params: {temperature: 0.2}   # optional, fill-missing, client wins
```

Validation failures are fatal at startup (exit 1) with the field named in the message — verified messages include `provider "x": unknown type`, `set either key_env or key_envs, not both`, `duplicate provider`, `chain "x": unknown provider`, and YAML parse errors. A bad config never starts a half-configured gateway.

Semantics that matter in production (canonical definitions in ARCHITECTURE.md §2–§4):

- **Passthrough**: a model named `<prefix>/<rest>` routes directly to that provider, walking providers in config order. If `<rest>` is in the provider's `models:` list, only `<rest>` is sent upstream; otherwise the full original name is sent (namespaced catalogs). Verified live both ways.
- **Chain resolution order**: exact chain name → model named inside a chain's steps → provider-prefix passthrough → 404. Verified.
- **Step `params`**: shallow-merged fill-missing onto the request body; explicit client values always win. Keys `model`, `stream`, `messages` are rejected; keys must be alphanumeric + underscore; values are unconstrained (the provider decides). Verified by code + config validation; the merge itself was not exercised live in this run.

## 4. Deploy

### Docker Compose (verified parse only — no Docker in the verification VM)

```sh
git clone https://github.com/Hesamation/guvna && cd guvna
cp .env.example .env              # add ADMIN_KEY + API_KEYS + provider keys
docker compose -f deploy/docker-compose.yml up -d
```

The compose file builds the distroless image locally (no shell, runs as nonroot) and mounts a named volume at `/data` for SQLite telemetry + `chains.yaml`. It uses `network_mode: host` — load-bearing on hosts whose Docker cannot create bridge networks (no iptables), where a host-network reverse proxy reaches the app on `127.0.0.1:20128`.

From the repo root, check which env file compose actually resolved (the project directory is `deploy/`, so `env_file: .env` means `deploy/.env`):

```sh
docker compose -f deploy/docker-compose.yml config | grep -A3 -i env
```

### Plain `docker run` with the published image

```sh
docker run -d --name guvna \
  -p 20128:20128 \
  --env-file .env \
  -e GOMEMLIMIT=256MiB \
  -v guvna-data:/data \
  -v "$PWD/config.yaml:/config.yaml:ro" \
  ghcr.io/Hesamation/guvna:latest
```

Plain `docker run -p` needs `bind: 0.0.0.0` in the mounted `config.yaml`: the default loopback bind is the container's own loopback, which published ports cannot reach. (The Compose file uses host networking, where the loopback default works as-is.)

The image bakes `config.yaml` in at build time; the read-only mount above overrides it. Key rotation needs only an env change + recreate, because key *values* resolve lazily per request.

### Binary + systemd user unit

Layout (recommended; paths are yours to change, the relationships matter):

| Item | Path | Notes |
|---|---|---|
| Binary | `~/.local/bin/guvna` | From §2. |
| Config | `~/.config/guvna/config.yaml` | Explicit `-config`. Not in `~/.guvna/` (would be shadowed by nothing today, but keeps config and data separated anyway). |
| Env file | `~/.config/guvna/guvna.env` | `chmod 600`. `ADMIN_KEY`, `API_KEYS`, provider keys. Unquoted values. |
| Data dir | `~/.guvna/` | Explicit `-data`. Created `0700` by the gateway. |

The unit file ships at `deploy/guvna.service`:

```ini
[Unit]
Description=Guvna AI gateway
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%h/.local/bin/guvna -config %h/.config/guvna/config.yaml -data %h/.guvna
EnvironmentFile=%h/.config/guvna/guvna.env
Environment=GOMEMLIMIT=256MiB
UMask=0077
Restart=on-failure
RestartSec=5s
TimeoutStopSec=10s

[Install]
WantedBy=default.target
```

Why these settings (all grounded in verified behavior):

- `EnvironmentFile=` keeps keys out of the unit and out of git; values unquoted (see §3).
- `UMask=0077`: the gateway creates the data dir `0700`, but `guvna.db` inherits the process umask (observed `0640` under umask `0007`). The umask setting makes the DB `0600`. [INFERENCE: umask-to-file-mode mapping is standard Unix behavior; the umask inheritance itself was observed.]
- `Restart=on-failure`: the gateway exits 1 on config/auth errors, so a bad deploy fails visibly instead of crash-looping silently — but `RestartSec` + systemd rate limiting still apply; check `systemctl --user status guvna` after a failed start.
- `TimeoutStopSec=10s`: shutdown is SIGTERM → up to 5s graceful HTTP shutdown (code) → final telemetry flush (verified). 10s gives the flush headroom after the 5s server grace.
- No `DynamicUser`/sandboxing beyond the user unit: the gateway needs its home data dir and outbound HTTPS. Harden further only with measured need.

Install and verify (UNTESTED as a unit — the verification VM has no systemd user bus; the binary behaviors the unit relies on were all verified individually):

```sh
mkdir -p ~/.config/guvna ~/.local/bin
# place binary, config.yaml, guvna.env (chmod 600 guvna.env)
cp deploy/guvna.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now guvna
systemctl --user status guvna
curl -s http://127.0.0.1:20128/healthz   # {"status":"ok"}
journalctl --user -u guvna -n 50
```

## 5. TLS exposure

Keep the gateway localhost-only unless you need remote clients — the loopback default makes that the path of least resistance. When exposing:

- Terminate TLS in front (caddy example below). Auth is always on (admin key + client API keys), but keys should still travel over TLS.

```
gateway.example.com {
  reverse_proxy 127.0.0.1:20128
}
```

- If another service already binds `*:443`, put the gateway vhost on a different port (e.g. `:9443`) — two listeners on 443 cause flaky TLS handshakes (verified on the production VPS; the live deployment uses `gateway.example.com:9443` → `127.0.0.1:20128`).

## 6. Keys & rotation (operational)

The mechanics live in ARCHITECTURE.md §3; operationally:

- To rotate a key value: edit `.env`, then restart (or recreate the container). The new value takes effect immediately — no restart needed for a *value* change; a restart only clears the old key's quarantine state.
- To add a key to a pool: add the env name to `key_envs` in config.yaml and set it in the env file, then restart (membership is config-time).
- Per-key state is visible in `/admin/status` → `keys`; telemetry rows carry the serving `key`.

## 7. Chains workflow

The gateway starts with **zero chains**. Clients create them per use case (any valid API key); chains persist in `/data/chains.yaml` and survive restarts. Config-defined chains also load (merged, conflicts skipped with a warning).

```sh
# API (client key)
curl -X POST http://127.0.0.1:20128/v1/chains \
  -H "Authorization: Bearer $CLIENT_KEY" -H "Content-Type: application/json" \
  -d '{"name":"myfree","steps":[{"provider":"bazaarlink","model":"qwen/qwen3.7-flash:free"},{"provider":"groq","model":"llama-3.3-70b-versatile"}]}'
curl -X DELETE http://127.0.0.1:20128/v1/chains/myfree -H "Authorization: Bearer $CLIENT_KEY"

# CLI (same surface, works remotely over HTTPS)
guvna-cli chains list
guvna-cli chains add myfree --step bazaarlink:qwen/qwen3.7-flash:free --step groq:llama-3.3-70b-versatile
guvna-cli chains rm myfree
```

Chat with a chain name as the model, or bypass chains entirely with a provider-prefixed model (`groq/llama-3.3-70b-versatile`). Unknown model → 404 with a pointer to the creation API.

Do not edit `chains.yaml` while the gateway runs: the in-memory runtime chains are authoritative, the file is read only at startup, and the next chain create/delete overwrites the file from memory. Edit while stopped, then start. (Code-grounded: `Load` runs once in `main`, `Save` runs on every chain mutation.)

## 8. Observability

```sh
export GUVNA_URL=http://127.0.0.1:20128 GUVNA_ADMIN_KEY=...
guvna-cli status          # chains, step health, key state, usage table (--json for raw)
guvna-cli logs -n 100     # tail the in-memory ring (512 max)
guvna-cli providers list  # per-provider pool summary (keys, healthy, quarantined)
guvna-cli providers test  # probe every key with one minimal completion: per-key verdicts
guvna-cli providers test --provider groq --model llama-3.3-70b-versatile
curl -s -H "Authorization: Bearer $ADMIN_KEY" http://127.0.0.1:20128/admin/status | jq
curl -s -H "Authorization: Bearer $ADMIN_KEY" http://127.0.0.1:20128/metrics
```

Admin endpoints (`/admin/status`, `/admin/logs`, `/admin/providers/test`, `/metrics`) are admin-key-only. For Prometheus + Grafana: `deploy/observability/` has a scrape config example, the dashboard JSON, and promtool-tested alert rules.

## 9. Backup and restore

Back up two files (plus a record of which env vars exist — never the values — and `config.yaml`, which is already in git):

| File | What it holds | Backup method |
|---|---|---|
| `<data-dir>/guvna.db` | SQLite telemetry (requests, tokens, serving key). | Online backup below, or stop + copy. Never plain `cp` on a running instance without the backup API — a torn copy is possible. |
| `<data-dir>/chains.yaml` | Runtime chains (mode `0600`, atomically rewritten via tmp + rename). | Plain `cp` any time — atomic renames mean a copy never sees a torn write. |

### Live backup (verified, gateway running)

```sh
# sqlite3 CLI — verified against the running gateway: backup matched the
# live row count and passed integrity_check.
sqlite3 ~/.guvna/guvna.db ".backup /path/to/backups/guvna-$(date +%F).db"

# ...or the Python equivalent (also verified):
python3 -c "
import sqlite3
src = sqlite3.connect('$HOME/.guvna/guvna.db')
dst = sqlite3.connect('/path/to/backups/guvna-backup.db')
src.backup(dst); dst.close(); src.close()"

# Verify every backup before trusting it:
sqlite3 /path/to/backups/guvna-backup.db "PRAGMA integrity_check; SELECT COUNT(*) FROM requests;"
```

Caveat: telemetry flushes every 30s and a graceful stop flushes the remainder, so a live backup can be up to ~30s behind the freshest requests. A `SIGKILL`/crash loses the unflushed buffer (verified: a request followed immediately by `SIGKILL` left 0 rows). For a point-in-time-exact backup, stop the gateway first (SIGTERM flushes — verified), then copy.

### Restore (verified)

```sh
systemctl --user stop guvna          # or: docker compose -f deploy/docker-compose.yml stop guvna
cp /path/to/backups/guvna-backup.db ~/.guvna/guvna.db
cp /path/to/backups/chains.yaml ~/.guvna/chains.yaml   # if restoring chains too
systemctl --user start guvna
curl -s -H "Authorization: Bearer $ADMIN_KEY" http://127.0.0.1:20128/admin/status | jq .usage
```

Verified end-to-end: backup at 10 rows → 1 more request (11 rows) → restore → usage reported 10 requests. Restore rolls usage back to the backup point; there is no merge. Chains restore the same way (file replace while stopped; the gateway loads `chains.yaml` only at startup).

## 10. Upgrades

The safe order, with what was verified at each step:

1. **Back up** (§9). Cheap, and the only way back for telemetry.
2. **Stop the old binary** (SIGTERM). Verified: graceful shutdown logs `shutting down`, flushes telemetry, exits.
3. **Swap the binary** (or pull the new image — Docker path UNTESTED).
4. **Start the new binary against the same data dir.** Startup validates the config before serving; a bad config exits 1 with the field named (verified) — the old data dir is untouched by a failed start.
5. **Verify**: `/healthz` → 200, `/admin/status` shows chains + usage from before the upgrade.

What survives a restart (verified): `chains.yaml` runtime chains, telemetry rows, config. What resets (verified, by design — both are in-memory): key quarantine state and step cool-off state. After an upgrade, previously quarantined keys and cooled-off steps are tried again immediately. That is usually what you want; if a key is known dead, fix the env first.

**Schema migration is automatic and safe** (verified): starting the current binary against a database created by an older schema (no `key` column) added the column in place and preserved all existing rows (2 rows in, 2 rows out, usage correct). There is no migration command to run and no downgrade path is offered — keep the §9 backup if you might roll back.

**Rollback**: stop, restore the old binary, start. The data dir is forward-compatible for reads (new columns are additive), but a rollback after the new binary wrote rows is UNTESTED — restore the §9 backup if exact pre-upgrade state matters. [INFERENCE: additive-column reasoning from the observed migrate code; the rollback itself was not run.]

## 11. First checks (troubleshooting always starts here)

```sh
curl -s http://127.0.0.1:20128/healthz
# {"status":"ok"} — unauthenticated, 200 when the process is serving.

guvna-cli status
# Chains, per-step failures / DOWN state, per-key state, usage totals.

guvna-cli logs -n 50
# Last 50 gateway log lines (ring buffer, 512 max, admin-only).
```

If `/healthz` answers but requests fail, the problem is upstream-facing (§12–§17), not the gateway. If `/healthz` does not answer, the process is down or the port is wrong.

## 12. Gateway won't start / exits immediately

The gateway fails fast: config and env errors exit 1 **before** serving, with the cause in the first log line. Observed fatal messages:

| Log line | Cause | Fix |
|---|---|---|
| `ADMIN_KEY env var required` | Env missing. | Set it in the env file, restart. |
| `API_KEYS env var required (comma-separated client keys)` | Env missing. | Same. |
| `config: read config: open ...: no such file or directory` | `-config` path wrong. | Fix the path. |
| `config: parse config: yaml: ...` | Invalid YAML. | Fix the file. |
| `provider "x": unknown type "y"` | `type:` must be `openai` or `gemini`. | Fix the provider. |
| `provider "x": set either key_env or key_envs, not both` | Both set. | Pick one. |
| `provider "x": missing base_url` / `missing key_env...` | Required field absent. | Add it. |
| `duplicate provider "x"` | Two providers, same name. | Rename one. |
| `chain "x": unknown provider "y"` | Chain step names a provider that doesn't exist. | Fix the chain. |
| `chain "x" step ...: invalid param ...` | Bad `params:` key (empty, non-`[A-Za-z0-9_]`, or `model`/`stream`/`messages`). | Fix the params. |

Startup traps:

1. **The config override.** If `~/.guvna/config.yaml` exists, it wins over `-config`. The gateway logs `config: <path> exists — overriding -config <flag path>` — watch for that line before debugging the wrong file.
2. **A corrupt `chains.yaml` does NOT stop startup** (verified). The gateway logs `chains: parse chains.yaml: ...`, keeps serving, and the config chains work — but all runtime chains are missing. Fix: stop the gateway, repair or restore `chains.yaml`, start. Do not edit it while running (§7).

## 13. A key is quarantined: `auth` (401/403)

What it means: the upstream rejected the key — dead, revoked, or out of quota. What the gateway does automatically (verified live):

- The key is quarantined for the `auth` class (default 24h, doubling per consecutive failure — durations in ARCHITECTURE.md §3, end state verified live).
- The request is retried on the next key in the pool, then walks to the next chain step. A 401 from one key does **not** fail the request if any other key/step works.
- A 401/403 **does** count toward the step's 3-failure cool-off, so a fully dead pool eventually skips the step entirely.
- A success on any key resets that key to healthy immediately.

Diagnose:

```sh
guvna-cli status
# KEYS section: STATE=quarantined, REASON=auth, FAILURES=n, DOWN_FOR remaining.
```

Fix:

1. Re-test the key directly: `guvna-cli providers test --provider <name>` probes each key with one minimal completion (read-only — it does not touch quarantine or health state). If the probe says `dead`, the provider rejects the key — Guvna is reporting, not causing, the failure.
2. Replace the key **value** in the env file. Key values resolve lazily per request (code-read: `internal/keypool`), so a value change takes effect without a restart; the quarantined state still applies to the env *name* until its cool-off expires or the gateway restarts.
3. To clear quarantine immediately: restart the gateway. Quarantine is in-memory (verified: restart reset all keys to healthy, failures 0). Restarting to clear a dead key's quarantine only re-tests it — fix the key first, or it re-quarantines on the next request.

## 14. A key is quarantined: `rate_limit` (429)

What it means: the upstream throttled the key. Behavior (verified live):

- Quarantine base 60s, doubling per consecutive failure (ARCHITECTURE.md §3).
- A 429 **does** count toward the step's cool-off (§16).
- If every step 429s, the last upstream 429 body is returned to the client as-is.

Diagnose: `guvna-cli status` → `REASON=rate_limit`. The telemetry row for the attempt carries status 429 and the serving key.

Fix: usually nothing — the gateway self-heals when the cool-off expires. If 429s are chronic: the provider's rate limit is the bottleneck, not Guvna. Options: add more keys to the pool (§6), spread traffic across providers/chains, or slow the client. Do not restart to "fix" a 429 storm — it clears the quarantine and re-hammers the provider immediately.

## 15. A key is quarantined: `transient` (5xx / network)

What it means: the upstream errored (5xx) or was unreachable (connection refused, DNS, TLS, timeouts). Behavior (verified live):

- Quarantine base 60s, doubling (ARCHITECTURE.md §3).
- 5xx and network errors **do** count toward the step cool-off (§16).
- Network-failed attempts are recorded in telemetry with status `0` and an empty key (verified) — the row's `err` column holds the dial error, e.g. `Post "http://...": dial tcp ...: connection refused`.
- Retries: 2 beyond the first attempt (3 total), backoff 250ms then 1s (code-read; the 3-attempt count was verified in the mock's request log). The step then fails and the chain walks on.

Diagnose:

```sh
guvna-cli logs -n 50 | grep -E 'attempt|failed'
sqlite3 ~/.guvna/guvna.db "SELECT ts, provider, model, status, substr(err,1,80) FROM requests WHERE status = 0 ORDER BY id DESC LIMIT 10;"
```

Fix: check upstream reachability from the host (`curl -v <base_url>`), DNS, and egress rules. Guvna dials IPv4-first (ARCHITECTURE.md §5) — on a host with broken IPv4 egress but working IPv6, upstreams resolve v4 and fail; that is a host network issue, not a Guvna bug. If the provider is down, wait: cool-off + quarantine handle the backoff; restart only after the provider recovers (restarting early just re-tests it).

## 16. Step shows DOWN / requests skip a step

The step-health tracker (verified live; thresholds in ARCHITECTURE.md §4): 3 consecutive retryable failures (429, 5xx, network — plus 401/403 from a dead key pool) mark a step DOWN for 60s, doubling per further failure, capped at 10min. While DOWN, the step is skipped entirely — no request is sent to it — and the chain walks to the next step. The gateway logs:

```
chat: skipping mock/err-model (cool-off)
```

Observed live on the 4th consecutive failing request: the first three returned the upstream 500; the fourth returned 503 without touching the upstream. `guvna-cli status` shows `DOWN <remaining>` for the step.

Fix: usually nothing — it self-recovers after the cool-off, and the first post-recovery request re-tests the step. To force an immediate re-test: restart (step health is in-memory, verified reset on restart). Do not restart in a loop: each restart resets the cool-off and re-hammers a failing upstream.

## 17. Clients get 503 `{"error":{"message":"no providers available"}}`

This is the gateway's "no step produced a response" answer. Three distinct causes — the logs distinguish them:

| Cause | Log signature | Fix |
|---|---|---|
| All steps in cool-off (§16) | `chat: skipping <provider>/<model> (cool-off)` | Wait, or restart to re-test (§16). |
| All attempts network-failed (§15) | `chat: step ... failed: Post ...: dial ...` | Fix upstream reachability. |
| Key env var unset (§18) | `chat: step ... failed: provider "x": key env "Y" not set` | Set the env var, restart. |

Note: a 503 from skipped steps is **not** recorded in telemetry (verified: no row for the skipped 4th request). If usage counts look low during an outage, that is why — check the logs, not the DB.

## 18. Key env var not set

Symptom: 503s for one provider, log line (verified verbatim):

```
chat: step nokey/ok-model failed: provider "nokey": key env "NOKEY_MISSING_ENV" not set
```

Behavior (verified): the gateway does **not** quarantine the key, does not fail startup, and burns all 3 attempts on the same error. The step **does** take a health mark (it counts toward the 3-failure cool-off), so a missing env eventually cools the step off too. Fix: set the env var and restart (env membership is config-time; a new *name* is not picked up by lazy value resolution). Then verify with `guvna-cli status` — the key appears healthy once it serves a success.

## 19. Clients get 404

Two different 404s:

- **Unknown chain/model** (gateway's own, verified verbatim):
  `unknown chain or model "x" — create a chain via POST /v1/chains or
  use a provider-prefixed model`. Cause: the model name matches no
  chain, no chain-step model, and no provider prefix. Fix: create the
  chain, or use `<prefix>/<model>`.
- **Upstream 404 propagated as-is** (verified): a passthrough model
  whose suffix is not in the provider's `models:` list is sent upstream
  with its full name (`mock/nope-model`), and the upstream's 404 body
  is returned unchanged. Other 4xx behave the same: returned as-is,
  never retried, never quarantined, never health-marked (verified).

Chain names must match `^[a-z0-9]+(-[a-z0-9]+)*$`, max 64 chars
(verified: `Bad_Name!` → 400). Duplicate chain creation → 409.
Deleting a config-defined chain → 400
`chain "x" is defined in config.yaml — edit the config, not the runtime
store` (verified). Deleting a missing chain → 404.

## 20. Auth failures at the gateway (401 from Guvna itself)

- `invalid api key` — the Bearer token matches neither `ADMIN_KEY` nor any `API_KEYS` entry, or the header is absent/malformed. The `Bearer` prefix is case-insensitive (code-read).
- `admin key required` — a valid *client* key (or no key) on `/admin/*` or `/metrics`. Client keys work for `/v1/*` only (verified).
- `/healthz` is the only unauthenticated endpoint (verified).

Fix: check the client is sending the right key for the right endpoint. Keys are compared exactly — a trailing space or a quoted value from an env file (see §3) fails silently as "invalid api key".

## 21. Streaming problems

Verified live: SSE chunks pass through byte-identical, including the final usage chunk and `data: [DONE]`, with `X-Guvna-Step` set. Known limits (code-read, not live-verified in this run):

- If the upstream dies mid-stream, the gateway emits one error chunk and ends the stream — it does **not** swap to another step mid-stream. The failure is recorded against step health and the key pool. A stream truncated without `[DONE]` surfaces the same way (and counts as failed in metrics).
- Keep-alive comment lines are sent every 15s during a stalled stream.
- **Client disconnects are silent** — mid-stream or before the first upstream byte: no quarantine, no health mark, no telemetry row, no metric (the pre-response case is regression-tested: `TestClientCancelRecordsNothing`).
- There is **no overall upstream timeout** (client `Timeout: 0`, code-read); time-to-first-byte is bounded at 5m, but an upstream that streams forever holds the connection. If streams hang forever, the upstream is hanging — check it directly, and consider a proxy-level timeout in front of Guvna.

## 22. Telemetry looks wrong

- **Up to 30s stale**: events flush every 30s; `/admin/status` usage counts persisted rows only. Recent requests appear after the next flush (verified: a row appeared after a 32s wait).
- **Missing rows after a crash**: `SIGKILL`/power loss loses the unflushed buffer (verified: 0 rows after kill -9). Graceful SIGTERM flushes (verified).
- **Skipped-step 503s are unrecorded** (§17, verified).
- **One row per step attempt, not per request**: a chain that fails over produces one row per attempted step (verified: the 401 + 200 fallthrough produced 2 rows). `status` is the step's outcome; `key` is the serving key env name (empty for network failures); `err` carries the failure text when there is one.
- **Stream rows** carry `stream=1` and tokens from the final usage chunk (verified: 5 in / 3 out from the mock's usage chunk). If an upstream never sends a usage chunk, tokens stay 0.

Direct inspection (read-only; safe against the running DB):

```sh
sqlite3 ~/.guvna/guvna.db "SELECT COUNT(*), provider, status FROM requests GROUP BY provider, status;"
sqlite3 ~/.guvna/guvna.db "SELECT id, ts, provider, model, stream, status, tokens_in, tokens_out, key, substr(err,1,60) FROM requests ORDER BY id DESC LIMIT 20;"
```

## 23. Disk / data-dir problems

- `guvna.db` grows with telemetry, but retention is built in now: set `telemetry: retain_days: 30` in config.yaml and rows older than the window are pruned at startup and every 6h (verified by unit tests, not yet live). Unset (the default) keeps every row forever.
- The DB file does not shrink when rows are pruned — SQLite reuses the freed pages, so growth stops but past size remains. To reclaim space: back up first (§9), stop the gateway, `VACUUM;`, start.
- Manual trimming still works: stop the gateway, `DELETE FROM requests WHERE ts < '<cutoff>';` (add `VACUUM;` to reclaim space), start.
- If the disk fills while running: the flush transaction fails and the gateway logs it; requests keep serving (telemetry is best-effort). Free space, restart to re-flush. [INFERENCE: flush-failure behavior follows from the code path (error logged, buffer retained); the disk-full condition itself was not simulated.]

## 24. Restart cheat sheet

```sh
# Graceful: SIGTERM → up to 5s HTTP drain → final telemetry flush → exit.
systemctl --user restart guvna
# What resets (in-memory, verified): key quarantine, step cool-off.
# What survives (on disk, verified): chains.yaml, telemetry, config.

# Verify after restart:
curl -s http://127.0.0.1:20128/healthz
guvna-cli status   # chains present, keys healthy, usage totals from before
```

A restart is the correct "clear all transient state" action — and the only way to apply config changes or new env *names*. It is the wrong action for a 429 storm or a down upstream (§14–§16): it discards the backoff Guvna built up.

## 25. Quick reference: status fields

`GET /admin/status` (or `guvna-cli status`):

- `keys[]`: `provider`, `env` (env var name), `state` (`healthy`/`quarantined`), `reason` (`auth`/`rate_limit`/`transient`, empty when healthy), `failures` (consecutive), `down_for_ns` (remaining cool-off, 0 when healthy).
- `steps[]`: `provider`, `model`, `failures` (consecutive retryable), `down` (bool), `down_for_ns` (remaining cool-off).
- `usage`: persisted-only aggregates — `requests`, `streams`, `errors` (status >= 400 or status 0), `tokens_in/out`, `by_provider[]`.
- `chains`: `name`, `source` (`config`/`runtime`), `steps`.
- `uptime`, `started_at`: process lifetime. A short uptime after an incident usually means something restarted the gateway — check the unit/container logs.

## Verification footer

Prepared 2026-11-16 against `Hesamation/guvna@c95435a`, Go 1.26.11, in a VM with no Docker and no systemd user bus; merged from the former DEPLOYMENT.md, PRODUCTION.md, and OPERATIONS-RUNBOOK.md (content preserved, duplication removed, stale claims corrected). Live-verified by running the binary against a scripted mock upstream: startup validation + exit codes, auth behavior, chain CRUD + persistence + restart reload, all three failure classes + cool-off + fallthrough, streaming passthrough, telemetry flush/restore/migration, config override precedence, and the `go install @main` path. Code changed after `c95435a` in ways this document reflects: loopback bind default (`cccfcb8`), client-cancel no longer poisons keys/steps/telemetry (regression-tested in `internal/server/cancel_test.go`, not live-verified), single telemetry row per relay failure. Added 2026-11-16 after the merge, unit-tested but not live-verified: telemetry retention (`telemetry: retain_days`, prune at startup + every 6h), `POST /admin/providers/test` key probes (no pool/health/telemetry side effects), constant-time client-key comparison. UNTESTED here: Docker build/run/healthcheck, the systemd unit as a unit, the published image, and long-stream keep-alive behavior. The full verification log (`RUNBOOK-NOTE.md`) lives outside the repo.

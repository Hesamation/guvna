# Production deployment

How to run Guvna in production: install paths, the full configuration
reference, binary + systemd and Docker Compose deployments, backup/restore,
and upgrades. Every behavior below was either verified live against the code
at `c95435a` or is explicitly marked otherwise — see the verification footer.

For the 5-minute paths (Compose quickstart, TLS caddy snippet, chains
workflow, CLI observability), see [DEPLOYMENT.md](DEPLOYMENT.md). This
document is the production companion to it, not a replacement. For the
dashboard (separate binary), see [DASHBOARD.md](DASHBOARD.md).

## 1. What runs in production

| Piece | What it is | State it owns |
|---|---|---|
| `guvna` binary | The gateway. One process, one port (default 20128). | None in memory that must survive restarts (see §7). |
| Data dir (default `~/.guvna/`, `-data` flag) | `guvna.db` (SQLite telemetry), `chains.yaml` (runtime chains). | The only persistent state. Back both up. |
| `config.yaml` | Providers, key env names, rotation, quarantine, config chains. | In git. Keys are never in it — only env var *names*. |
| `.env` / env file | `ADMIN_KEY`, `API_KEYS`, provider keys. | Never in git (`.env*` is gitignored). `chmod 600`. |
| Reverse proxy (caddy, etc.) | TLS termination when exposed. | Not part of Guvna. See DEPLOYMENT.md. |

Measured at `c95435a` (verified live, this repo's own test run): binary
16.7 MB, idle RSS ~12 MB. The README's "~15–40 MB" sizing is consistent
with this measurement.

The gateway binds **all interfaces** (`:port`) — verified with `ss`,
which showed the listener on `*:20199`, not `127.0.0.1`. An older
decision note says the app binds localhost; the code does not. Treat the
port as exposed on every interface and rely on auth + firewall/proxy
placement, not on a localhost bind. Auth is always on (see §3).

## 2. Install paths

### From a clone (verified)

```sh
git clone https://github.com/Hesamation/guvna && cd guvna
go build -o guvna ./cmd/guvna
./guvna -config config.yaml   # needs ADMIN_KEY + API_KEYS in env
```

Requires Go 1.26+ (`go.mod` says `go 1.26.6`). Verified: `go build`,
`go vet ./...`, and `go test ./...` all pass at `c95435a`.

### `go install` (verified — use `@main`, not `@latest`)

```sh
go install github.com/Hesamation/guvna/cmd/guvna@main
```

`@latest` is currently **broken**: it resolves to tag `v0.1.0`, whose
`go.mod` still declares the old module path `github.com/creamy-ghost/guvna`,
so the install fails with a module-path mismatch (verified). `@main` works
(verified). The durable fix is cutting a new tag from `main`; until then,
`@main` or clone + build are the working paths.

### Release image (UNTESTED — no Docker in the verification VM)

Tagged releases (`v*`) publish a multi-arch image to
`ghcr.io/Hesamation/guvna` via `.github/workflows/release.yml`
(amd64/arm64, `latest` + semver tags). The image was not pulled or run
during verification. The `docker run` invocation in DEPLOYMENT.md is
consistent with the image's entrypoint (`/guvna -config /config.yaml -data
/data`, config baked in at `/config.yaml`, overridable with a read-only
mount) but was not executed here.

## 3. Configuration reference

Read from `internal/config/config.go` and `cmd/guvna/main.go` at `c95435a`.
Nothing here is guessed.

### Flags

| Flag | Default | Meaning |
|---|---|---|
| `-config` | `config.yaml` | Path to config file. **Overridden** by `~/.guvna/config.yaml` when that file exists (see gotcha below). |
| `-data` | `~/.guvna` | Data directory. Created with mode `0700` if missing (verified). Holds `guvna.db` + `chains.yaml`. |
| `-healthcheck` | off | Probe `http://127.0.0.1:<port>/healthz` (3s timeout) and exit 0/1. No server is started. Used as the container healthcheck. |

### Environment variables

| Variable | Required | Meaning |
|---|---|---|
| `ADMIN_KEY` | Yes — gateway refuses to start without it (exit 1, verified) | Admin Bearer token. Valid for all endpoints including `/admin/*`. |
| `API_KEYS` | Yes — same refusal | Comma-separated client Bearer tokens for `/v1/*`. Whitespace around entries is trimmed; empty entries ignored. Client keys cannot read `/admin/*` (401). |
| `<PROVIDER>_KEY` names from `key_envs`/`key_env` | Yes, per provider | Provider API keys. **Values are resolved lazily per request** (code-read: `internal/keypool` — env value changes take effect without a restart). Pool *membership* (which env names) is config-time and needs a restart. An unset key env fails requests at runtime (see runbook), it does not fail startup. |
| `GOMEMLIMIT` | No | Go runtime memory limit. Compose sets `256MiB`. Not a Guvna flag. |
| `GUVNA_URL`, `GUVNA_ADMIN_KEY` | No (CLI only) | `guvna-cli` defaults: gateway URL and admin key. |

Quote gotcha (from DEPLOYMENT.md, restated because it bites): Docker
`--env-file` does **not** strip quotes; shell `source` does. Keep env values
unquoted in files consumed by Docker/systemd.

### `config.yaml` fields

```yaml
port: 20128                 # optional, default 20128
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
    models: [llama-3.3-70b-versatile]  # passthrough strip list (see below)
    prefixes: [groq]        # optional; defaults to [name]
chains:                     # optional; config-defined chains (undeletable via API)
  - name: mychain
    steps:
      - provider: groq
        model: llama-3.3-70b-versatile
        params: {temperature: 0.2}   # optional, fill-missing, client wins
default_chain: mychain      # ACCEPTED BUT INERT: parsed and validated, never
                            # consulted by the router (verified: unknown models
                            # still 404 with it set). Do not rely on it.
```

Validation failures are fatal at startup (exit 1) with the field named in
the message — verified messages include `provider "x": unknown type`,
`set either key_env or key_envs, not both`, `duplicate provider`,
`chain "x": unknown provider`, and YAML parse errors. A bad config never
starts a half-configured gateway.

Semantics that matter in production:

- **Passthrough**: a model named `<prefix>/<rest>` routes directly to that
  provider. If `<rest>` is in the provider's `models:` list, only `<rest>`
  is sent upstream; otherwise the full original name is sent (namespaced
  catalogs). Verified live both ways.
- **Chain resolution order**: exact chain name → model named inside a
  chain's steps → provider-prefix passthrough → 404. Verified.
- **Step `params`**: shallow-merged fill-missing onto the request body;
  explicit client values always win. Keys `model`, `stream`, `messages`
  are rejected; keys must be alphanumeric + underscore; values are
  unconstrained (the provider decides). Verified by code + config
  validation; the merge itself was not exercised live in this run.
- **Quarantine doubling**: cool-off starts at the class base and doubles
  per consecutive failure, capped at 24h (code-read; end states verified
  live, durations from code). Success resets the key immediately.
- **No default chains**: the gateway starts with zero chains unless
  `chains:` defines some. Clients self-provision via `POST /v1/chains`
  (any valid key), persisted to `<data-dir>/chains.yaml`.

### Config override gotcha (verified)

`~/.guvna/config.yaml`, when present, **silently wins** over the `-config`
flag path. The startup log still prints the flag path
(`starting, config=<flag path>`), so the effective config is not visible
in the log. If the gateway behaves like it's reading a different config
than the one you passed, check `~/.guvna/config.yaml` first. In the
systemd layout below, the config lives outside `~/.guvna/` precisely to
avoid this surprise.

## 4. Binary + systemd deployment

Layout (recommended; paths are yours to change, the relationships matter):

| Item | Path | Notes |
|---|---|---|
| Binary | `~/.local/bin/guvna` | From §2. |
| Config | `~/.config/guvna/config.yaml` | Explicit `-config`. Not in `~/.guvna/` (see override gotcha). |
| Env file | `~/.config/guvna/guvna.env` | `chmod 600`. `ADMIN_KEY`, `API_KEYS`, provider keys. Unquoted values. |
| Data dir | `~/.guvna/` | Explicit `-data`. Created `0700` by the gateway. |

The unit file `guvna.service` (delivered alongside this guide) is a
**systemd user unit**:

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

- `EnvironmentFile=` keeps keys out of the unit and out of git. systemd
  does not strip quotes either — keep values unquoted, same as Docker.
- `UMask=0077`: the gateway creates the data dir `0700`, but `guvna.db`
  inherits the process umask (observed `0640` under umask `0007`). The
  umask setting makes the DB `0600`. [INFERENCE: umask-to-file-mode
  mapping is standard Unix behavior; the umask inheritance itself was
  observed.]
- `Restart=on-failure`: the gateway exits 1 on config/auth errors, so a
  bad deploy fails visibly instead of crash-looping silently — but
  `RestartSec` + systemd rate limiting still apply; check
  `systemctl --user status guvna` after a failed start.
- `TimeoutStopSec=10s`: shutdown is SIGTERM → up to 5s graceful HTTP
  shutdown (code) → final telemetry flush (verified). 10s gives the
  flush headroom after the 5s server grace.
- No `DynamicUser`/sandboxing beyond the user unit: the gateway needs its
  home data dir and outbound HTTPS. Harden further only with measured
  need.

Install and verify (UNTESTED as a unit — the verification VM has no
systemd user bus; the binary behaviors the unit relies on were all
verified individually):

```sh
mkdir -p ~/.config/guvna ~/.local/bin
# place binary, config.yaml, guvna.env (chmod 600 guvna.env)
cp guvna.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now guvna
systemctl --user status guvna
curl -s http://127.0.0.1:20128/healthz   # {"status":"ok"}
journalctl --user -u guvna -n 50
```

## 5. Docker Compose deployment

The repo ships `deploy/docker-compose.yml` + `deploy/Dockerfile`
(distroless `static-debian12:nonroot`, multi-stage, config baked in at
`/config.yaml`, entrypoint `-config /config.yaml -data /data`, named
volume `guvna-data` at `/data`, `network_mode: host`,
`mem_limit: 400m`, `GOMEMLIMIT=256MiB`, healthcheck via the binary's
`-healthcheck` flag). The dashboard is an opt-in `--profile dashboard`
service; see DASHBOARD.md.

**Verification status: the Compose file parses as valid YAML (verified)
but was never built or run here — no Docker in the verification VM.**
The following are therefore UNTESTED: image build, container start,
healthcheck behavior in-container, and the published-image `docker run`
path. What you should check on a Docker host before trusting the
quickstart:

```sh
# From the repo root — shows the EFFECTIVE config, including which .env
# file the env_file entry resolves to. [INFERENCE to confirm: with
# `-f deploy/docker-compose.yml`, the project directory is deploy/, so
# `env_file: .env` resolves to deploy/.env, NOT the repo-root .env the
# README quickstart creates. deploy/.env.example exists for exactly this
# file. If `config` shows the wrong file, copy the env file to deploy/.env.]
docker compose -f deploy/docker-compose.yml config | grep -A3 -i env
```

Two things the Compose file assumes that are worth knowing:

- `network_mode: host` is load-bearing on hosts whose Docker cannot
  create bridge networks (see DECISIONS.md). With host networking the
  gateway's all-interfaces bind (§1) is directly the host's interfaces.
- The image bakes `config.yaml` in at build time. Config changes need a
  rebuild (or the read-only mount from the DEPLOYMENT.md `docker run`
  example). Key rotation needs only an env change + recreate, because
  key *values* resolve lazily per request.

## 6. Backup and restore

Back up two files (plus a record of which env vars exist — never the
values — and `config.yaml`, which is already in git):

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

Caveat: telemetry flushes every 30s and a graceful stop flushes the
remainder, so a live backup can be up to ~30s behind the freshest
requests. A `SIGKILL`/crash loses the unflushed buffer (verified: a
request followed immediately by `SIGKILL` left 0 rows). For a
point-in-time-exact backup, stop the gateway first (SIGTERM flushes —
verified), then copy.

### Restore (verified)

```sh
systemctl --user stop guvna          # or: docker compose -f deploy/docker-compose.yml stop guvna
cp /path/to/backups/guvna-backup.db ~/.guvna/guvna.db
cp /path/to/backups/chains.yaml ~/.guvna/chains.yaml   # if restoring chains too
systemctl --user start guvna
curl -s -H "Authorization: Bearer $ADMIN_KEY" http://127.0.0.1:20128/admin/status | jq .usage
```

Verified end-to-end: backup at 10 rows → 1 more request (11 rows) →
restore → usage reported 10 requests. Restore rolls usage back to the
backup point; there is no merge. Chains restore the same way (file
replace while stopped; the gateway loads `chains.yaml` only at startup).

Do not edit `chains.yaml` while the gateway runs: the in-memory runtime
chains are authoritative, the file is read only at startup, and the next
chain create/delete overwrites the file from memory. Edit while stopped,
then start. (Code-grounded: `Load` runs once in `main`, `Save` runs on
every chain mutation.)

## 7. Upgrades

The safe order, with what was verified at each step:

1. **Back up** (§6). Cheap, and the only way back for telemetry.
2. **Stop the old binary** (SIGTERM). Verified: graceful shutdown logs
   `shutting down`, flushes telemetry, exits.
3. **Swap the binary** (or pull the new image — Docker path UNTESTED).
4. **Start the new binary against the same data dir.** Startup validates
   the config before serving; a bad config exits 1 with the field named
   (verified) — the old data dir is untouched by a failed start.
5. **Verify**: `/healthz` → 200, `/admin/status` shows chains + usage
   from before the upgrade.

What survives a restart (verified): `chains.yaml` runtime chains,
telemetry rows, config. What resets (verified, by design — both are
in-memory): key quarantine state and step cool-off state. After an
upgrade, previously quarantined keys and cooled-off steps are tried
again immediately. That is usually what you want; if a key is known
dead, fix the env first.

**Schema migration is automatic and safe** (verified): starting the
current binary against a database created by an older schema (no `key`
column) added the column in place and preserved all existing rows
(2 rows in, 2 rows out, usage correct). There is no migration command
to run and no downgrade path is offered — keep the §6 backup if you
might roll back.

**Rollback**: stop, restore the old binary, start. The data dir is
forward-compatible for reads (new columns are additive), but a rollback
after the new binary wrote rows is UNTESTED — restore the §6 backup if
exact pre-upgrade state matters. [INFERENCE: additive-column reasoning
from the observed migrate code; the rollback itself was not run.]

## 8. Verification footer

Prepared 2026-11-16 against `Hesamation/guvna@c95435a`, Go 1.26.11, in a
VM with no Docker and no systemd user bus. Live-verified by running the
binary against a scripted mock upstream: startup validation + exit
codes, auth behavior, chain CRUD + persistence + restart reload, all
three failure classes + cool-off + fallthrough, streaming passthrough,
telemetry flush/restore/migration, config override precedence, and the
`go install @main` path. UNTESTED here: Docker build/run/healthcheck,
the systemd unit as a unit, the published image, and long-stream
keep-alive behavior. Full log: `RUNBOOK-NOTE.md` (delivered alongside
this guide; not part of the repo).

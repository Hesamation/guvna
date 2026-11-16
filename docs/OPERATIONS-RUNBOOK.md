# Operations runbook

Troubleshooting for the Guvna gateway, organized by symptom. Every check
below was run live against `c95435a` or is marked with its verification
status. Status codes and log lines are quoted verbatim from observed
output; where a behavior comes from reading the code rather than a live
run, it says so.

Assumed access: shell on the host, the admin key, `guvna-cli` or `curl`,
and `sqlite3` for telemetry inspection. Set these once:

```sh
export GUVNA_URL=http://127.0.0.1:20128 GUVNA_ADMIN_KEY='<admin-key>'
```

## 0. First checks (always start here)

```sh
curl -s http://127.0.0.1:20128/healthz
# {"status":"ok"} — unauthenticated, 200 when the process is serving.

guvna-cli status
# Chains, per-step failures / DOWN state, per-key state, usage totals.

guvna-cli logs -n 50
# Last 50 gateway log lines (ring buffer, 512 max, admin-only).
```

If `/healthz` answers but requests fail, the problem is upstream-facing
(§2–§6), not the gateway. If `/healthz` does not answer, the process is
down or the port is wrong — see §1.

## 1. Gateway won't start / exits immediately

The gateway fails fast: config and env errors exit 1 **before** serving,
with the cause in the first log line. Observed fatal messages:

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

Two non-obvious startup traps, both verified:

1. **The config override.** If `~/.guvna/config.yaml` exists, it silently
   wins over `-config`, and the startup log still prints the flag path.
   Symptom: the gateway ignores your config edits. Check for the file.
2. **A corrupt `chains.yaml` does NOT stop startup** (verified). The
   gateway logs `chains: parse chains.yaml: ...`, keeps serving, and the
   config chains work — but all runtime chains are missing. Fix: stop
   the gateway, repair or restore `chains.yaml`, start. Do not edit it
   while running (the next chain mutation overwrites it from memory).

## 2. A key is quarantined: `auth` (401/403)

What it means: the upstream rejected the key — dead, revoked, or out of
quota. What the gateway does automatically (verified live):

- The key is quarantined for the `auth` class (default 24h, doubling per
  consecutive failure, capped at 24h — duration from code, end state
  verified).
- The request is retried on the next key in the pool, then walks to the
  next chain step. A 401 from one key does **not** fail the request if
  any other key/step works.
- A 401/403 does **not** count toward the step's 3-failure cool-off
  (verified: `auth-model` step stayed at 0 failures after 401s).
- A success on any key resets that key to healthy immediately.

Diagnose:

```sh
guvna-cli status
# KEYS section: STATE=quarantined, REASON=auth, FAILURES=n, DOWN_FOR remaining.
```

Fix:

1. Check the key at the provider (dashboard / a direct curl with the
   key). If the provider says the key is dead, it is dead — Guvna is
   reporting, not causing, the failure.
2. Replace the key **value** in the env file. Key values resolve lazily
   per request (code-read: `internal/keypool`), so a value change takes
   effect without a restart; the quarantined state still applies to the
   env *name* until its cool-off expires or the gateway restarts.
3. To clear quarantine immediately: restart the gateway. Quarantine is
   in-memory (verified: restart reset all keys to healthy, failures 0).
   Restarting to clear a dead key's quarantine only re-tests it — fix
   the key first, or it re-quarantines on the next request.

## 3. A key is quarantined: `rate_limit` (429)

What it means: the upstream throttled the key. Behavior (verified live):

- Quarantine base 60s, doubling per consecutive failure, capped at 24h
  (duration from code, end state verified).
- Unlike `auth`, a 429 **does** count toward the step's cool-off (§5).
- If every step 429s, the last upstream 429 body is returned to the
  client as-is.

Diagnose: `guvna-cli status` → `REASON=rate_limit`. The telemetry row
for the attempt carries status 429 and the serving key.

Fix: usually nothing — the gateway self-heals when the cool-off expires.
If 429s are chronic: the provider's rate limit is the bottleneck, not
Guvna. Options: add more keys to the pool (config change + restart),
spread traffic across providers/chains, or slow the client. Do not
restart to "fix" a 429 storm — it clears the quarantine and re-hammers
the provider immediately.

## 4. A key is quarantined: `transient` (5xx / network)

What it means: the upstream errored (5xx) or was unreachable
(connection refused, DNS, TLS, timeouts). Behavior (verified live):

- Quarantine base 60s, doubling, capped 24h (duration from code).
- 5xx and network errors **do** count toward the step cool-off (§5).
- Network-failed attempts are recorded in telemetry with status `0`
  and an empty key (verified) — the row's `err` column holds the
  dial error, e.g. `Post "http://...": dial tcp ...: connection refused`.
- Retries: 2 beyond the first attempt (3 total), backoff 250ms then 1s
  (code-read; the 3-attempt count was verified in the mock's request
  log). The step then fails and the chain walks on.

Diagnose:

```sh
guvna-cli logs -n 50 | grep -E 'attempt|failed'
sqlite3 ~/.guvna/guvna.db "SELECT ts, provider, model, status, substr(err,1,80) FROM requests WHERE status = 0 ORDER BY id DESC LIMIT 10;"
```

Fix: check upstream reachability from the host
(`curl -v <base_url>`), DNS, and egress rules. Guvna dials IPv4-first
(code-read + DECISIONS.md) — on a host with broken IPv4 egress but
working IPv6, upstreams resolve v4 and fail; that is a host network
issue, not a Guvna bug. If the provider is down, wait: cool-off +
quarantine handle the backoff; restart only after the provider
recovers (restarting early just re-tests it).

## 5. Step shows DOWN / requests skip a step

The step-health tracker (verified live): 3 consecutive retryable
failures (429, 5xx, network — **not** other 4xx) mark a step DOWN for
60s, doubling per further failure, capped at 10min. While DOWN, the
step is skipped entirely — no request is sent to it — and the chain
walks to the next step. The gateway logs:

```
chat: skipping mock/err-model (cool-off)
```

Observed live on the 4th consecutive failing request: the first three
returned the upstream 500; the fourth returned 503 without touching the
upstream. `guvna-cli status` shows `DOWN <remaining>` for the step.

Fix: usually nothing — it self-recovers after the cool-off, and the
first post-recovery request re-tests the step. To force an immediate
re-test: restart (step health is in-memory, verified reset on restart).
Do not restart in a loop: each restart resets the cool-off and
re-hammers a failing upstream.

## 6. Clients get 503 `{"error":{"message":"no providers available"}}`

This is the gateway's "no step produced a response" answer. Three
distinct causes — the logs distinguish them:

| Cause | Log signature | Fix |
|---|---|---|
| All steps in cool-off (§5) | `chat: skipping <provider>/<model> (cool-off)` | Wait, or restart to re-test (§5). |
| All attempts network-failed (§4) | `chat: step ... failed: Post ...: dial ...` | Fix upstream reachability. |
| Key env var unset (§7) | `chat: step ... failed: provider "x": key env "Y" not set` | Set the env var, restart. |

Note: a 503 from skipped steps is **not** recorded in telemetry
(verified: no row for the skipped 4th request). If usage counts look
low during an outage, that is why — check the logs, not the DB.

## 7. Key env var not set

Symptom: 503s for one provider, log line (verified verbatim):

```
chat: step nokey/ok-model failed: provider "nokey": key env "NOKEY_MISSING_ENV" not set
```

Behavior (verified): the gateway does **not** quarantine the key, does
not fail startup, and burns all 3 attempts on the same error. The step
**does** take a health mark (it counts toward the 3-failure cool-off),
so a missing env eventually cools the step off too. Fix: set the env
var and restart (env membership is config-time; a new *name* is not
picked up by lazy value resolution). Then verify with
`guvna-cli status` — the key appears healthy once it serves a success.

## 8. Clients get 404

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

## 9. Auth failures at the gateway (401 from Guvna itself)

- `invalid api key` — the Bearer token matches neither `ADMIN_KEY` nor
  any `API_KEYS` entry, or the header is absent/malformed. The `Bearer`
  prefix is case-insensitive (code-read).
- `admin key required` — a valid *client* key (or no key) on `/admin/*`.
  Client keys work for `/v1/*` only (verified).
- `/healthz` is the only unauthenticated endpoint (verified).

Fix: check the client is sending the right key for the right endpoint.
Keys are compared exactly — a trailing space or a quoted value from an
env file (see PRODUCTION.md §3) fails silently as "invalid api key".

## 10. Streaming problems

Verified live: SSE chunks pass through byte-identical, including the
final usage chunk and `data: [DONE]`, with `X-Guvna-Step` set. Known
limits (code-read, not live-verified in this run):

- If the upstream dies mid-stream, the gateway emits one error chunk
  and ends the stream — it does **not** swap to another step mid-stream.
  The failure is recorded against step health and the key pool.
- Keep-alive comment lines are sent every 15s during a stalled stream.
- A client disconnect drops the stream silently with no telemetry row.
- There is **no overall upstream timeout** (client `Timeout: 0`,
  code-read). A hung upstream holds the request open until the client
  disconnects or the upstream responds. If streams hang forever, the
  upstream is hanging — check it directly, and consider a proxy-level
  timeout in front of Guvna.

## 11. Telemetry looks wrong

- **Up to 30s stale**: events flush every 30s; `/admin/status` usage
  counts persisted rows only. Recent requests appear after the next
  flush (verified: a row appeared after a 32s wait).
- **Missing rows after a crash**: `SIGKILL`/power loss loses the
  unflushed buffer (verified: 0 rows after kill -9). Graceful SIGTERM
  flushes (verified).
- **Skipped-step 503s are unrecorded** (§6, verified).
- **One row per step attempt, not per request**: a chain that fails
  over produces one row per attempted step (verified: the 401 + 200
  fallthrough produced 2 rows). `status` is the step's outcome; `key`
  is the serving key env name (empty for network failures).
- **Stream rows** carry `stream=1` and tokens from the final usage
  chunk (verified: 5 in / 3 out from the mock's usage chunk). If an
  upstream never sends a usage chunk, tokens stay 0.

Direct inspection (read-only; safe against the running DB):

```sh
sqlite3 ~/.guvna/guvna.db "SELECT COUNT(*), provider, status FROM requests GROUP BY provider, status;"
sqlite3 ~/.guvna/guvna.db "SELECT id, ts, provider, model, stream, status, tokens_in, tokens_out, key, substr(err,1,60) FROM requests ORDER BY id DESC LIMIT 20;"
```

## 12. Restart cheat sheet

```sh
# Graceful: SIGTERM → up to 5s HTTP drain → final telemetry flush → exit.
systemctl --user restart guvna
# What resets (in-memory, verified): key quarantine, step cool-off.
# What survives (on disk, verified): chains.yaml, telemetry, config.

# Verify after restart:
curl -s http://127.0.0.1:20128/healthz
guvna-cli status   # chains present, keys healthy, usage totals from before
```

A restart is the correct "clear all transient state" action — and the
only way to apply config changes or new env *names*. It is the wrong
action for a 429 storm or a down upstream (§3–§5): it discards the
backoff Guvna built up.

## 13. Disk / data-dir problems

- `guvna.db` grows with telemetry. There is **no retention/pruning
  logic in the code** (code-read: no DELETE/prune statements in
  `internal/telemetry`) — the file grows until an operator acts. Watch
  it with normal disk monitoring.
- Trimming old rows while stopped (UNTESTED — standard SQLite, not run
  in this verification): back up first (PRODUCTION.md §6), stop the
  gateway, `DELETE FROM requests WHERE ts < '<cutoff>'; VACUUM;`, start.
- If the disk fills while running: the flush transaction fails and the
  gateway logs it; requests keep serving (telemetry is best-effort).
  Free space, restart to re-flush. [INFERENCE: flush-failure behavior
  follows from the code path (error logged, buffer retained); the
  disk-full condition itself was not simulated.]

## 14. Quick reference: status fields

`GET /admin/status` (or `guvna-cli status`):

- `keys[]`: `provider`, `env` (env var name), `state`
  (`healthy`/`quarantined`), `reason` (`auth`/`rate_limit`/`transient`,
  empty when healthy), `failures` (consecutive), `down_for_ns`
  (remaining cool-off, 0 when healthy).
- `steps[]`: `provider`, `model`, `failures` (consecutive retryable),
  `down` (bool), `down_for_ns` (remaining cool-off).
- `usage`: persisted-only aggregates — `requests`, `streams`, `errors`
  (status >= 400 or status 0), `tokens_in/out`, `by_provider[]`.
- `chains`: `name`, `source` (`config`/`runtime`), `steps`.
- `uptime`, `started_at`: process lifetime. A short uptime after an
  incident usually means something restarted the gateway — check the
  unit/container logs.

## Verification footer

Prepared 2026-11-16 against `Hesamation/guvna@c95435a`. Live-verified:
startup fatals, auth 401s, chain CRUD + name rules + config-chain
refusal, all three failure classes + cool-off + fallthrough, 503/404
shapes, missing-key-env behavior, streaming passthrough, telemetry
flush/crash-loss/row shapes, restart semantics, corrupt-`chains.yaml`
behavior, config override precedence. Code-read (not live-run):
quarantine doubling durations, retry backoff timings, keep-alive
interval, no-timeout behavior, lazy key-value resolution, no-pruning.
UNTESTED: Docker/container behavior, the systemd unit as a unit, the
disk-full path, telemetry trimming. Full log: `RUNBOOK-NOTE.md`.

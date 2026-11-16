# Architecture

Guvna is one static binary: a single-user OpenAI-compatible relay with chain-based fallback, key pools, and self-healing. This document is the canonical home for its semantics — every other doc links here instead of restating them.

```
any OpenAI-compatible client
        │  /v1/*  (127.0.0.1:20128 by default, auth: Bearer admin/API key)
        ▼
┌──────────────────────────────────────────┐
│  Guvna daemon (single static binary)     │
│  ┌──────────────┐  ┌──────────────────┐  │
│  │ Relay + auth │  │  Chain routing   │  │
│  ├──────────────┤  ├──────────────────┤  │
│  │ Key pools    │  │  Step health     │  │
│  ├──────────────┤  ├──────────────────┤  │
│  │ Telemetry    │  │  /admin, /metrics│  │
│  │ (SQLite)     │  │  (ring buffer)   │  │
│  └──────────────┘  └──────────────────┘  │
└───────────────┬──────────────────────────┘
                │ outbound HTTPS (IPv4-first, 5m header timeout)
        ┌───────┴────────┐
        │ free-tier providers │
        └────────────────┘

Separate, on the same interfaces:  guvna-cli (local + remote), Prometheus + Grafana
```

## Design rules (non-negotiable)

- Config in git, never DB. SQLite holds telemetry only.
- The upstream is the catalog: loose validation, no local model lists.
- No default chains: clients self-provision over the API.
- No UI in this repo. Dashboards consume `/admin/*` and `/metrics` from outside.
- Loopback bind by default; exposure is an explicit act.
- Auth cannot be disabled: the gateway refuses to start without `ADMIN_KEY` and `API_KEYS`.

## Components

### 1. Relay surface (`/v1`)

- `POST /v1/chat/completions` — OpenAI-compatible, streaming SSE passthrough and non-streaming, 15s keep-alives during upstream silence, 32MB request-body cap.
- `GET /v1/models` — chain names (empty until clients create chains).
- `GET/POST/DELETE /v1/chains` — chain management, any valid key.
- `GET /admin/status`, `GET /admin/logs`, `POST /admin/providers/test`, `GET /metrics` — admin-key only. `/healthz` is the only unauthenticated endpoint.
- `POST /admin/providers/test` probes every configured provider's keys with one minimal chat completion per key and returns per-key verdicts (`ok`/`dead`/`limited`/`upstream`/`unreachable`/`rejected`/`unset`). Pure diagnostics: probes never touch key-pool state, step health, telemetry, or metrics — a probe cannot make the pool look healthier than it is. Keys within a pool are probed sequentially (rate limits); providers in parallel.
- Successful responses carry `X-Guvna-Step: <provider>/<model>` (header precedes the SSE stream), so clients see which step served.
- The relay rewrites only the `model` field (plus fill-missing step params); everything else passes through untouched. Body mutations happen in a single unmarshal/marshal pass.
- No mid-stream swaps: once an upstream stream starts, the request is committed to it. A mid-stream death propagates one error chunk; a truncated stream (EOF without `[DONE]`) surfaces as an error too — a partial response never reads as complete.

### 2. Chain routing

- A chain is an ordered list of (provider, model, params) steps. Resolution order: **exact chain name → model named inside a chain's steps → provider-prefix passthrough → 404** (with a pointer to the creation API — no silent fallback).
- First 2xx step serves; non-2xx walks to the next step; all steps failed → last upstream response propagated as-is (network-dead everywhere → 503 `no providers available`).
- Loose validation by design: chain creation checks the provider exists and the model is non-empty — **not** model existence. A bad model 404s at request time and the chain falls through.
- Runtime chains persist to `<data-dir>/chains.yaml` (atomic tmp+rename, mutex-serialized writes); config-defined chains load at startup and cannot be deleted via the API. Runtime chains are authoritative in memory — the file is read only at startup.

**Provider-prefix passthrough**: any model named `<prefix>/<model>` routes directly to that provider, bypassing chains. Prefixes are per-provider (`prefixes:`, defaults to the provider name; orcarouter also claims `openai`, `anthropic`, `google`). If the suffix is in the provider's known `models:` list, only the suffix is sent upstream; otherwise the full name is sent (namespaced catalogs).

### 3. Key pools (canonical semantics)

Providers take `key_envs: [K1, K2, ...]` (single `key_env` = 1-key pool; both set = startup error). Key **values** resolve lazily per request — env changes take effect without a restart; pool **membership** is config-time.

- **Rotation** (`rotation:` per provider): `round_robin` (default) / `least_used` / `sequential`, picking only healthy keys. If every key is quarantined, `Pick` falls back to the key whose cool-off expires soonest — selection never hard-blocks; persistent failure surfaces via step health instead.
- **Class-based quarantine** (`quarantine:` per-provider overrides; defaults):

  | Class | Trigger | Default base |
  |---|---|---|
  | `auth` | 401/403 | 24h |
  | `rate_limit` | 429 | 60s |
  | `transient` | 5xx, network errors | 60s |

  Durations double per consecutive failure, capped at the auth duration; success resets immediately. In-memory: restart clears it (like step health).
- **Failover**: on a quarantinable failure the same request retries on the next healthy key within the step retry budget (2 retries, 250ms → 1s backoff). 401/403 consume budget too — dead keys burn it fast. All keys down → step down → chain walks on.
- **Streaming** rotation is per-request only; a mid-stream key death propagates an error chunk — never a mid-stream swap.
- **Observability**: per-key state in `/admin/status` (`keys`: state, reason, failures, cool-off remaining, requests, tokens) and the serving `key` column in telemetry rows; `guvna_quarantine_events_total` + `guvna_keypool_keys` in metrics.

### 4. Step health (canonical semantics)

Per-(provider, model) tracker: **3 consecutive retryable failures → 60s cool-off, doubling per further failure, capped at 10 min**; auto-recovers without admin action; steps in cool-off are skipped entirely.

- Counts toward cool-off: 429, 5xx, network errors, 401/403 (so "all keys dead → step down" is real).
- Never counted: other 4xx — client/request errors; the step did its job.
- **Client disconnects are never upstream failures** — not mid-stream, and not in the pre-response phase: no quarantine, no health mark, no telemetry row, no metric. The request is abandoned silently.

### 5. Providers / adaptors

- Two adaptor types: `openai` (base + `/v1/chat/completions`) and `gemini` (Gemini's official OpenAI-compat endpoint `/v1beta/openai` — passthrough, no translation). Anything OpenAI-compatible is ~20 lines.
- Outbound requests carry `User-Agent: guvna/0.1` (bazaarlink throttles UA-less requests).
- The transport dials IPv4-first when A records exist (hosts serving dead AAAA records otherwise break TLS), with a 5-minute response-header timeout; there is **no overall client timeout** — streams may run long, so hung upstreams must be handled by a proxy-level timeout in front of Guvna if that matters to you.

### 6. Telemetry

- In-memory event buffer, async batch flush to SQLite (`<data-dir>/guvna.db`) every 30s; the hot path never touches disk; a crash loses the unflushed interval; SIGTERM flushes.
- One row per step attempt: ts, provider, chain, model, stream, status, tokens_in/out, serving key, err text. Skipped-in-cool-off steps write nothing.
- Schema migration is additive and automatic. Retention is opt-in (`telemetry: retain_days` in config; unset keeps every row forever — an upgrade never silently deletes telemetry): rows older than the window are pruned at startup and every 6h while running. The DB file does not shrink (SQLite reuses freed pages); growth stops instead. Manual trimming still works the same way.
- Answers "what happened to that request". Metrics answer "how is it behaving right now".

### 7. Metrics

- `GET /metrics` (admin-key) renders the Prometheus text format from a stdlib-only registry (`internal/metrics`).
- Counters/histograms increment on the request path; gauges sample live subsystem state at scrape time.
- **Label discipline**: label values come only from config names or fixed vocabularies. Nothing client-controlled ever becomes a label — a hostile client cannot blow up series cardinality.
- Names and outcome vocabularies are listed in `deploy/observability/README.md`; the Grafana dashboard and promtool-tested alert rules live there too. Counters reset on restart.

### 8. Config & data

- `config.yaml` (git): providers, key env names, rotation, quarantine overrides, prefixes, config chains, `port`, `bind`. Validation failures are fatal at startup (exit 1) with the field named.
- `~/.guvna/config.yaml`, when present, **overrides** `-config` — the gateway logs the effective path and the override explicitly.
- Data dir (`-data`, default `~/.guvna/`, mode 0700): `guvna.db` (telemetry), `chains.yaml` (runtime chains). The only persistent state.

## Restart semantics (canonical)

| Survives restart | Resets on restart |
|---|---|
| `chains.yaml` runtime chains | Key quarantine state |
| Telemetry rows | Step cool-off state |
| Config, env-derived keys | Metrics counters |

A restart is the correct "clear all transient state" action, and the only way to apply config changes or new env-var *names*. It is the wrong action for a 429 storm or a down upstream — it discards built-up backoff and re-hammers the provider.

## Sizing (measured)

- Binary 16.7MB, distroless image ~19MB, idle RSS ~12MB
- ~6.4k lines of Go, two dependencies, `CGO_ENABLED=0`
- Rationale in [RESEARCH-2026-08.md](RESEARCH-2026-08.md) (why scratch, not port or fork)

## Future directions (ideas, explicitly unbuilt)

- **Compression engines** — caveman-style rule packs, rtk-style tool-output filters, session dedup. The original motivation; held back by the reality check (RESEARCH-2026-08.md): measured real-bill savings for this tool class are ~3.7%, and free-tier token stretching is plausible but unproven for this codebase.
- **Scoped CLI tokens** (revocable, replacing the admin key for remote CLI use).
- **Per-user keys** — an auth bolt-on that never touches the router core; only honest if users bring their own keys.
- **Native Gemini translation** behind the Adaptor interface, if a non-OpenAI-compatible need appears.

Nothing here is scheduled. Subtraction beats addition in this repo; a future direction only becomes real if daily use demands it.

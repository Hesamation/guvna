# Guvna observability: Prometheus + Grafana

`GET /metrics` exposes guvna's operational state in the Prometheus text
exposition format. No new dependencies: the exposition is emitted by
`internal/metrics` (stdlib only) and the endpoint is always on.

## Auth

`/metrics` requires the admin key, like `/admin/*`:

```sh
curl -s -H "Authorization: Bearer $ADMIN_KEY" http://127.0.0.1:20128/metrics
```

Prometheus scrapes it with a bearer token; see `prometheus.yml` in this
directory for a working example (key via `bearer_token_file`, never
committed).

## Metrics

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `guvna_chat_requests_total` | counter | `chain`, `outcome` | Chat requests. `outcome`: `success` (served by a step), `failed` (all steps failed), `rejected` (never reached an upstream). Unresolved requests use `chain="unknown"`; provider-prefixed passthrough uses `chain="direct"`. Client disconnects: pre-response cancels record no outcome at all; mid-stream disconnects count as `success` (the upstream served until the client left). |
| `guvna_chat_duration_seconds` | histogram | `chain` | End-to-end chat latency incl. retries, fallbacks, relay. |
| `guvna_step_attempts_total` | counter | `provider`, `model`, `outcome` | Upstream attempts. `outcome`: `success`, `auth_error` (401/403), `rate_limited` (429), `server_error` (5xx), `client_error` (other 4xx), `network_error` (no response). Retries count as separate attempts. |
| `guvna_step_duration_seconds` | histogram | `provider`, `model` | Time to first upstream response per attempt. |
| `guvna_chain_fallbacks_total` | counter | `chain` | Requests that attempted a step past the first. |
| `guvna_quarantine_events_total` | counter | `provider`, `class` | Key quarantine applications. `class`: `auth`, `rate_limit`, `transient`. |
| `guvna_tokens_total` | counter | `provider`, `model`, `direction` | Usage tokens reported by upstreams (`in`/`out`). Best-effort: only responses with a usage object count. |
| `guvna_keypool_keys` | gauge | `provider`, `state` | Keys by health state (`healthy`/`quarantined`), sampled live at scrape time. |
| `guvna_step_down` | gauge | `provider`, `model` | 1 when a step is in cool-off, else 0. Only observed steps appear. |
| `guvna_step_failures` | gauge | `provider`, `model` | Consecutive failures per step. |
| `guvna_uptime_seconds` | gauge | — | Seconds since the gateway started. |

Histogram buckets (both latency histograms):
`0.1, 0.25, 0.5, 1, 2.5, 5, 10, 25, 60, 120, +Inf`.

Label discipline: label values come only from config (provider/model/chain
names) or fixed vocabularies. Nothing client-controlled ever becomes a
label, so a hostile client cannot blow up series cardinality. Counters
reset on restart (in-memory, like key quarantine and step health).

## Grafana dashboard

Import `guvna-dashboard.json` (Dashboards → Import). It expects a
Prometheus datasource and uses the `${DS_PROMETHEUS}` variable, so it
prompts for the datasource on import. Panels cover request/attempt rates,
p50/p95 latencies, error ratio, quarantine events, key-pool health,
tokens, fallbacks, steps in cool-off, uptime, and a per-step health
table.

## Alerts

`guvna-alerts.yml` is a Prometheus rule file (load via `rule_files`).
Five rules: upstream failing, all keys quarantined, global error-rate
spike, step down sustained, auth quarantine. Process-down detection is
deliberately not here — use Prometheus' own `up{job="guvna"} == 0`.

## What metrics do NOT replace

SQLite telemetry (`/admin/status`, `/admin/logs`) keeps per-request
history with serving keys and error text. Metrics answer "how is it
behaving right now"; telemetry answers "what happened to that request".

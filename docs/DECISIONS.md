# Decision Log

Rules: one entry per decision, date, context, rationale. New sessions should read this first — these were hard-won, don't relitigate without new evidence.

## 2026-08-14 — Project: Guvna

**Decision:** Build a low-resource, single-user AI gateway from scratch in Go. Name: Guvna.

## 2026-08-14 — Single-user scope

**Decision:** v1 is single-user: one admin key, API keys for clients, global usage stats, minimal read-only web UI.

**Context:** User asked "is omnirouter single user?" — verified: OmniRoute is single-admin (INITIAL_PASSWORD + JWT_SECRET + API keys + scoped CLI tokens), NOT multi-tenant (0 "multi-user" results in docs).

**Rationale:** Free quotas (Kiro ~50 credits/mo, Cerebras 1M/day) are per-upstream-account — multiple users sharing one account just drain one quota. The only honest multi-user free model is bring-your-own-keys, which is new-api's model. Multi-user ~2-3x scope. Auth can be bolted on later without touching router core.

## 2026-08-14 — Language: Go

**Decision:** Go over Rust.

**Rationale:** Rust ~10-30MB RSS but ~50% slower development. Go ~15-40MB RSS, still far below the low-resource bar (the prior Node-based gateway ran 96-243MB). Gateway is network-bound; 1 core is fine.

## 2026-08-14 — From scratch, not fork

**Decision:** Scratch implementation, not a fork of new-api or port of OmniRoute/litellm.

**Rationale:**
- Full OmniRoute port impossible: 8,512 TS files, 320 contributors
- new-api fork: inherits ~80-150MB floor (React dashboard), permanent merge tax, billing-first data model that fights free-catalog + compression-first vision
- litellm: no free catalog, no built-in compression, Rust migration makes it a moving architecture target, and it's closing the low-resource wedge itself
- Scratch: ~3-6k lines, matches the vision, MIT references (new-api adaptors, OmniRoute catalog + compression design, rtk, caveman) are legally usable

## 2026-08-14 — Catalog: subscribe-sync, not own audit

**Decision:** Subscribe-sync script pulls OmniRoute's MIT catalog data (freeModelCatalog.ts, providers.ts) on schedule, diff + commit to git.

**Rationale:** Don't run the weekly free-tier re-audit treadmill. Own the sync layer; data persists in git history even if OmniRoute dies.

## 2026-08-14 — Compression framing: quota stretching

**Decision:** Position compression as quota stretching, not bill savings.

**Rationale:** Independent replay (codepointer, 2026-07-14, 614M tokens / $926) shows only 3.7% real bill savings for rtk/headroom/caveman — claims measure only the compressible payload; real bills are 42% cache-writes + 29% output. But free tiers bill in tokens: 30-90% compression = 2-3x more work per free token. The inversion is the product.

## 2026-08-14 — Port 20128

**Decision:** Listen on port 20128, `/v1` OpenAI-compatible.

**Rationale:** An existing local client config already pointed at `http://127.0.0.1:20128/v1` — drop-in, only the api_key changes.

## 2026-08-14 — No timings in docs

**Decision:** Roadmap has no deadlines/estimates.

**Rationale:** Personal project, explicit user requirement.

## 2026-08-16 — Chains are the core routing primitive

**Decision:** Routing is chain-based from day 1. Config defines named chains = ordered list of (provider, model) steps. Clients send logical names (chain name, or any model inside a chain) — never real provider model names. Unknown names resolve to `default_chain`. First successful step serves; failures walk the chain.

**Context:** Design requirement: each client gets its own cross-provider fallback chain, real model names never leak into clients, and the design stays modular. The relay stays dumb; the chain engine is a modular package.

**Rationale:** Quota stretching across providers needs cross-provider fallback anyway; chains subsume the earlier "no fallback yet" decision. One primitive covers both routing and failover. `/v1/models` exposes chain names.

## 2026-08-16 — Streaming + telemetry pulled into Phase 1

**Decision:** SSE streaming passthrough and async batched SQLite telemetry ship with the Phase 1 core, not Phase 2.

**Rationale:** The primary local client streams by default — Phase 1 is useless without streaming. Telemetry is a 30s-interval flusher (in-memory buffer, hot path never touches disk, crash loses <30s), built before providers multiply so the schema grows with real usage.

## 2026-08-16 — Gemini via its OpenAI-compatible endpoint

**Decision:** Gemini adaptor uses `https://generativelanguage.googleapis.com/v1beta/openai` (official OpenAI-compat mapping), pure passthrough, no format translation.

**Rationale:** Both adaptors share one code path (`openai` and `gemini` are config types pointing at different base URLs). Native translation can be added behind the Adaptor interface later if needed.

## 2026-08-16 — User-Agent on outbound requests

**Decision:** All outbound requests carry `User-Agent: guvna/0.1`.

**Rationale:** Live test: bazaarlink throttled UA-less requests to ~20s vs instant. Found during Phase 1 smoke test.

## 2026-08-16 — Client disconnect is not an upstream failure

**Decision:** When the client connection dies mid-relay (context canceled), the request is dropped silently — no telemetry record, no 502.

**Rationale:** Curl/pipe truncation in testing exposed this; real clients reading the full body never hit it. Recording 502 misattributed client behavior to upstream. Distinguish via `r.Context().Err()`.

## 2026-08-16 — Failure marking: cool-off, not binary on/off

**Decision:** Per-(provider, model) tracker: 3 consecutive failures → 60s cool-off, doubling per failure beyond threshold, capped at 10 min; auto-recovers when cool-off expires. Steps in cool-off are skipped in chains. Retries (2×: 250ms, 1s) only for network errors, 429, 5xx; 4xx never retried or marked.

**Rationale:** Daily use on the VPS means a rate-limited or down provider must not stall or kill chains, and a dead provider must recover without admin action. Cool-off gives self-healing without a health-probe fleet. 4xx are client errors — the step did its job, the caller is wrong.

## 2026-08-16 — VPS-first: gateway lives on the VPS

**Decision:** Primary deployment is the remote VPS behind caddy; Arch local install is deferred.

**Rationale:** Daily use is from the laptop anywhere (cloud-first pillar); the VPS has the TLS + always-on infrastructure. Distroless container, ~13MB image, `--memory=400m` cap, `GOMEMLIMIT=256MiB`.

## 2026-08-16 — Distroless static:nonroot

**Decision:** Final image is `gcr.io/distroless/static-debian12:nonroot` (multi-stage, CGO_ENABLED=0). Healthcheck is a `-healthcheck` flag on the binary itself.

**Rationale:** ~2MB base, zero CVEs, no shell — matches the low-resource/low-attack-surface philosophy. Distroless has no shell/wget, so the binary probes its own /healthz. `scratch` rejected: needs manual CA certs for outbound TLS.

## 2026-08-16 — Remote CLI over the gateway, no new port

**Decision:** Observability = `GET /admin/status` + `GET /admin/logs` in core (admin-key-only) + a `guvna-cli` binary (status/logs, `--json`), hitting the same caddy vhost over TLS.

**Rationale:** Nothing new exposed — the admin key already authenticates; TLS terminates at caddy. `status`/`logs`/usage from the laptop was the top capability for daily use.

## 2026-08-16 — caddy vhost on :9443, not :443

**Decision:** `gateway.example.com:9443` → `127.0.0.1:20128`. Caddy auto-TLS on the custom port.

**Rationale:** Another TLS service already binds `*:443` — two listeners on 443 produced flaky TLS handshakes (requests intermittently hitting the wrong socket). Same reason the dead new-api vhost used :9443.

## 2026-08-16 — host network mode in compose

**Decision:** `network_mode: host` on the VPS; no bridge network.

**Rationale:** The host's docker cannot create bridge networks (iptables setup fails — no iptables/legacy tooling present). Existing containers (caddy et al.) already run host-network. App binds 127.0.0.1:20128 directly; caddy reaches it the same way.

## 2026-08-16 — No catalogs, no strict model checks (upstream is the source of truth)

**Decision:** No local model catalog; chain/model validation is loose (provider must exist in config, model non-empty). `POST /v1/chains` accepts any model string. Cancelled the Phase 5 subscribe-sync as the enforcement mechanism.

**Context:** Mid-build reversal — decision to drop catalog creation and strict catalog checks entirely rather than defer them.

**Rationale:** Catalogs go stale (orcarouter ships new free models weekly). A stale catalog REJECTS valid new models — worse than a typo failing at request time (upstream 404s, chain falls over to the next step). For a single-user gateway the upstream provider IS the catalog. Revisit only if chain creation needs model discovery.

## 2026-08-16 — No default chains

**Decision:** Removed the built-in `free`/`fast`/`smart`/`coder` chains and the `default_chain` concept. `/v1/models` starts empty; unknown model → 404 with a pointer to `POST /v1/chains`. Every chain is created per use case by the client (any valid API key, persisted to `<data-dir>/chains.yaml`).

**Context:** Requirement: no default chains — every chain must be created per use case by the client.

**Rationale:** Chains are a routing primitive, not a product decision baked into config. Client-driven creation keeps the gateway generic — each app provisions exactly what it needs. Provider-prefixed passthrough (`<prefix>/<model>`) covers direct single-model calls without any chain at all.

## 2026-08-16 — Chain creation surface: API + CLI, not config

**Decision:** Chains are runtime state (chains.yaml in the data dir, atomic rewrite), managed via `POST/DELETE /v1/chains` and `guvna-cli chains` — live, no redeploy. Config-defined chains still load (merged, conflicts skipped with a warning).

**Rationale:** The gateway is cloud-first and client-driven; config-in-git is for infra, chains are per-use-case and change with the workload. Apps self-provision with their own API key.

## 2026-08-17 — Provider key pools with rotation + quarantine

**Decision:** Providers configure `key_envs: [K1, K2, ...]` forming a rotation pool (single `key_env` still valid = 1-key pool; both set = config error). Per-provider `rotation:` strategy: `round_robin` (default) / `least_used` / `sequential`. Class-based quarantine: 401/403 → `auth` (default 24h), 429 → `rate_limit` (60s), 5xx/network → `transient` (60s); per-provider `quarantine:` overrides; durations double per consecutive failure, capped. Success resets. Failover retries the same request on the next healthy key inside the step retry budget (401/403 consume budget too). All keys down → step down → chain walks to the next step. Streaming rotation is per-request only; a mid-stream key death propagates an error chunk — never a mid-stream swap.

**Context:** Requested: multiple rotation strategies so usage doesn't burn one key out, plus set-aside of seemingly-dead keys backed by health checking and observability.

**Rationale:** Free-tier keys die (quota exhaustion, revocations — all 3 staged groq keys returned 403). A dead key must cost one attempt, not a burned request: quarantine it class-appropriately (auth death = long; rate limit = short, recoverable) and let the pool forget it until it heals. Per-key observability (`/admin/status` keys section + `key` column in telemetry rows) makes the mechanism auditable; deliberately out of scope: quarantine persistence across restarts (in-memory like step health), CLI key commands (a UI comes later).

## 2026-08-17 — Verified: key failover + quarantine live

**Decision:** Live-verified on the VPS: with `BAZAARLINK_2_KEY` deliberately invalid, round-robin picked it second, 403 → quarantined (`auth`, failures=1), same request served by `BAZAARLINK_1`; `/admin/status` showed the quarantined key with reason; telemetry rows carry the serving key env. Local verification additionally covered: dead groq pool (all 3 keys 403 → all quarantined → last upstream 403 propagated), telemetry key attribution, and `/admin/status` per-key counters.

## 2026-08-17 — Gemma models verified through the OpenAI-compat path; CORS rejected

**Decision:** Added `gemma-4-31b-it` + `gemma-4-26b-a4b-it` to the gemini provider's `models:` list in config.yaml so `gemini/<model>` passthrough strips the prefix correctly. Live-verified through the `/v1beta/openai` path: both models complete (non-streaming + streaming, `data: [DONE]`), and function calling works — a `tools` payload returned a proper `tool_calls` response with JSON arguments. The gateway needs no native-Gemini translation adaptor for multi-app passthrough usage.

**Rejected:** CORS middleware in the gateway. Browser-only concern; no browser client exists yet, and current integrating apps are server-side (no CORS). When a browser client lands it should go through a thin server-side proxy that holds the client key and sets CORS headers — browser boundary belongs in the app layer, not the relay. Revisit only with a committed browser-direct design plus per-key quotas.

**Context:** Priority was migration-readiness — the gateway should have all building blocks ready for later migrations; question raised whether the gateway should ship CORS middleware.

## 2026-08-17 — VPS gemini/gemma unreachable: IPv4/IPv6 preference, NOT censorship (SUPERSEDED)

**Decision (superseded by the entry below):** Deployed the gemma-capable image to the VPS; local verification passed. On the VPS the direct gemini provider timed out — the host resolver returns only real Google AAAA records via getent, and `__VG_IPV4_*` A records via getaddrinfo — and I misread `curl -4`'s 403 as a filtering middlebox block. The keypool quarantine (transient, 60s doubling) did isolate the keys and chains fell over, which worked as designed. **Conclusion was wrong: it was never censorship.**

## 2026-08-17 — Prefer IPv4 dialing in adaptors (fixes gemini/gemma on the VPS)

**Decision:** The gateway's outbound transport now dials IPv4 when A records exist (clone of `http.DefaultTransport` with a `DialContext` that resolves `ip4` first and falls back to the default dialer for IP literals and v6-only hosts). Proven by a probe binary on the VPS: `net.DefaultResolver` ip4 → `__VG_IPV4_*` records, `tcp4` dial OK; default transport (Happy Eyeballs, prefers AAAA) → TLS EOF; forced `tcp4` → HTTP 200 in 137ms. The VPS's DNS virtual gateway serves working A records and dead AAAA ones — Go preferred the dead v6 path, which the keypool quarantine correctly recorded as transient network errors.

**Context:** Correctly diagnosed mid-debugging as the known IPv4-vs-IPv6 problem seen before on this host ("we definitely can access Google on the VPS") — and the fix ships in code (works for every deployment, not a VPS-specific workaround).

**Verified live:** `gemini/gemma-4-31b-it` chat completion 200 through `https://gateway.example.com:9443` with real content and usage; GEMINI_1_KEY healthy in `/admin/status`.

## 2026-08-17 — Per-step params on chains (fill-missing, client wins)

**Decision:** Chain steps gain an optional `params` object (any JSON values, no model-param whitelist — the gateway stays neutral about model catalogs). When a step serves, its params are shallow-merged onto the client request body **fill-missing**: a param applies only if the client didn't set that field; explicit client values always win. Keys `model`, `stream`, `messages` are rejected. Params are set per step, not per chain — mixed-tier chains (fast model + reasoning model) can tune each upstream call independently without risking 400s from providers that reject unknown fields (e.g. a chain-wide `reasoning_effort` breaks gemini).

**Context:** Proposed during downstream-app integration work: per-step request params on chains with step-wins semantics. Rejected step-wins: the chain author IS the requester in this gateway (any valid key self-provisions chains), so step-wins means the client silently overrides its own explicit values (`max_tokens: 2048` → 8192 with no signal) and widens the chain-poisoning blast radius on shared chain names. Fill-missing solves the motivating case identically (client sends plain body, step fills `reasoning_effort: high` + `max_tokens`) while keeping per-request tuning intact.

**Also:** responses now carry `x-guvna-step: <provider>/<model>` so clients can see which step served and whether params applied (works for SSE — header precedes the stream). chains.yaml gains a `version` field; no backward-compat constraint — nothing is migrated yet, chains are regenerable runtime state. Config-defined chains get params too (config.yaml parity). CLI stays JSON-only for params chains (repeatable `--param` flags can't express arrays/objects or bind unambiguously to a step).

## 2026-09-04 — Read-only web UI: separate binary (Phase 7) (SUPERSEDED)

**Decision:** The web UI shipped as an optional separate binary (`guvna-dashboard`, HTMX, read-only, own Dockerfile and compose profile) — never embedded in the gateway process.

**Superseded 2026-11-16:** the dashboard is removed from the repo entirely — see the gateway-only entry below.

## 2026-10-31 — Single-pass JSON rewrite on the hot path

**Decision:** `rewritePayload` unmarshals the request body once, applies the model override and the step-params fill-missing merge on the same map, and marshals once. Earlier code ran independent decode/encode passes per mutation.

**Context:** Arrived via an external-agent PR ("Bolt"); the learning stands regardless of source: chained marshal/unmarshal passes on a proxy hot path are pure GC/CPU overhead.

**Rationale:** The relay is a passthrough; body mutation is at most two fields. One decode/encode pass does both with no behavior change.

## 2026-11-01 — Module path and tags

**Decision:** Module path is `github.com/Hesamation/guvna` (repo moved off the personal account). Tags v0.1.0–v0.1.2 exist; v0.1.0 predates the path fix, which briefly made `go install ...@latest` resolve to a broken tag — v0.1.2 (2026-11-16) restores it.

## 2026-11-16 — Hardening batch (daily-use findings)

**Decision:** A batch of reliability fixes landed from running the gateway daily:

- **32MB request-body cap** — `io.ReadAll` held whole bodies in memory; one giant body could OOM the gateway. Chat payloads (even with base64 images) sit far below the cap.
- **401/403 mark step health** — "all keys quarantined → step down" must be real, so after the failure threshold the step is skipped instead of burning the retry budget on every request. Other 4xx stay unmarked (client errors).
- **Truncated streams surface as failures** — an upstream EOF without the terminal `[DONE]` emits an error chunk and marks the step; a partial response must never read as a complete one (also reflected in metrics: `outcome=failed`, not `success`).
- **Keep-alives + bounded time-to-first-byte** — a dedicated goroutine writes SSE comment lines after 15s of upstream silence (the relay loop blocks in `ReadBytes` and cannot timer-check); upstream header timeout is 5m while `Client.Timeout` stays 0 for long streams. An upstream that accepts and never answers no longer holds the goroutine forever.
- **Quarantine doubling capped at the auth duration** for every class (previously doubling could run past it).
- **Passthrough walks providers in config order**, and removing a chain repairs the shared-model index so other chains keep resolving models it served.
- **`chains.Store.Save` is mutex-serialized** — two concurrent chain mutations must not interleave snapshot-and-write and lose a chain.
- **CLI errors cleanly on trailing flag args** instead of panicking.

## 2026-11-16 — Prometheus /metrics in core; dashboards and alerts stay external

**Decision:** `GET /metrics` (admin-key) emits the exposition from a stdlib-only registry (`internal/metrics`, no new dependencies). The Grafana dashboard JSON and alert rules live in `deploy/observability/` as external definitions; the alert rules are unit-tested with promtool.

**Rationale:** Observability without dependencies and without a UI in the process. Label discipline: label values come only from config names or fixed vocabularies — nothing client-controlled ever becomes a label, so a hostile client cannot blow up series cardinality. Gauges sample live subsystem state at scrape time so they cannot drift. Process-down detection deliberately stays with Prometheus' own `up{job="guvna"}`.

## 2026-11-16 — Releases gated on tests; images built in CI

**Decision:** The release workflow runs the same gofmt/vet/test gate before publishing; multi-arch (amd64/arm64) images build in CI on `v*` tags; the image healthchecks via the binary's own `-healthcheck` flag. CI also builds (never pushes) the image on every PR so Dockerfile/compose regressions surface at PR time, not tag time.

## 2026-11-16 — Baked-in dashboard removed: gateway-only

**Decision:** The dashboard (binary, templates, static assets, Dockerfile, compose profile, CI job, docs) is deleted. Guvna stays gateway-only.

**Rationale:** Three months of daily use settled the shape: the leanest possible core is the product. A graphical UI belongs in a separate, isolated component consuming the gateway's public interfaces (`/admin/status`, `/metrics`) — not in this repo. Observability already has two sufficient surfaces: `guvna-cli` and Grafana over `/metrics`. Supersedes the Phase 7 build above.

## 2026-11-16 — Loopback bind by default

**Decision:** The gateway binds `127.0.0.1` by default; the `bind:` config key makes exposure an explicit act (`0.0.0.0`, only behind a trusted proxy or firewall).

**Rationale:** A single-user gateway's correct default is unreachable from the network. The reverse-proxy pattern (caddy on the same box) needs only loopback. Auth is always on, but not-listening beats not-authorized.

## 2026-11-16 — Dead `default_chain` config field removed

**Decision:** The last leftover of the removed default-chains concept is gone from config parsing. Config-defined chains are explicit `chains:` entries or nothing.

## 2026-11-16 — Docs realignment: Guvna is a lean gateway

**Decision:** The docs were rebuilt around what the project revealed itself to be: a lean, single-user, self-healing AI gateway — not a compression-first quota stretcher. VISION.md and ROADMAP.md are absorbed into the README; DEPLOYMENT, PRODUCTION and the operations runbook merge into OPERATIONS.md; RESEARCH.md is archived as a dated snapshot (`RESEARCH-2026-08.md`). This backfill was reconstructed from git history.

**Rationale:** The Aug 2026 vision described a product (compression engines, web UI) that was never built or was deliberately removed; three months of decisions consistently subtracted. Compression is a future idea, explicitly unbuilt — and treated with justified skepticism (see the reality check in RESEARCH-2026-08.md: 3.7% measured bill savings; free tiers are the honest target, and even there it is unproven for this codebase). The docs tell one true story: config-as-code, upstream-is-the-catalog, client-driven chains, observability outside the process, subtract until it hurts.

**Also (code, same day):** a client disconnect during the pre-response phase (before any upstream bytes) no longer quarantines keys, marks step health, or writes telemetry rows — extending the 2026-08-16 client-disconnect principle to the whole request. Relay failure paths now write exactly one telemetry row per attempt (status, tokens, and error text together).

## 2026-11-16 — Telemetry retention is opt-in, default keep-forever

**Decision:** `telemetry: retain_days: N` prunes telemetry rows older than N days at startup and every 6h while running. Unset (the default) keeps every row forever.

**Rationale:** `guvna.db` was the only unbounded resource in the system, but a destructive default would be worse than the problem it solves: an upgrade that silently deletes ~2 months of production telemetry on first boot is a betrayal of the boring-pipe contract. Pruning is an explicit operator act; the docs no longer have to say "grows until an operator acts." The file does not shrink on prune (SQLite reuses freed pages) — growth stops, VACUUM remains manual.

## 2026-11-16 — Provider key probes: one command replaces the dead-key dance

**Decision:** `POST /admin/providers/test` (admin-key only) + `guvna-cli providers list|test` probe every configured provider's key with one minimal chat completion (`max_tokens: 1`) and return per-key verdicts: `ok` / `dead` (401/403) / `limited` (429) / `upstream` (5xx) / `unreachable` (network) / `rejected` (other 4xx) / `unset` (env not set).

**Rationale:** The runbook's fix for a quarantined key was "check the key at the provider (dashboard / a direct curl with the key)" — manual work every time a key dies (groq keys did, Aug 2026). The probe is the operational completion of the keypool feature. Two design rules make it trustworthy: it reads key envs from config rather than pool state (a quarantined key is exactly the one you want to re-test), and it has zero side effects — no quarantine marks, no step health, no telemetry, no metrics — so a probe can never make the pool look healthier than it is. Keys probe sequentially per pool (rate limits), providers in parallel; each probe is bounded at 10s.

## 2026-11-16 — Client-key validation moved to constant-time comparison

**Decision:** Client API keys validate via `crypto/subtle.ConstantTimeCompare` over the whole key set (no early exit), matching the admin key's existing discipline; the map lookup is gone.

**Rationale:** No realistic timing attack exists in the single-user threat model, but the cost of uniformity is one loop, and SECURITY.md's "auth cannot be disabled" story should not have two comparison disciplines. Same documented caveat as the admin key: a length mismatch returns early — only key bytes are timing-protected.

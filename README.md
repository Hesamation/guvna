# Guvna

A lean, single-user AI gateway in Go. One OpenAI-compatible endpoint that routes to free-tier LLM providers through named, self-healing chains — one static binary, no database, ~12MB of RAM.

[![CI](https://github.com/Hesamation/guvna/actions/workflows/ci.yml/badge.svg)](https://github.com/Hesamation/guvna/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

## Why

Most LLM API gateways sit at two extremes. Either they're TypeScript bloat — a Node service eating hundreds of MB of RAM to do what is fundamentally forwarding HTTP — or they're enterprise-grade platforms built for teams: multi-tenant, database-backed, dashboard-first, ops-budget-required.

Guvna takes neither horn: **one static binary** (~16MB, distroless image ~19MB) that idles at ~12MB of RSS and is sized for exactly one user.

## Principles

The project subtracts until it hurts. These were learned by removal, not addition — see [docs/DECISIONS.md](docs/DECISIONS.md) for the receipts:

- **Config-as-code, never DB-driven** — providers, rotation, quarantine, and config chains live in YAML in git; keys live only in env, referenced by name.
- **The upstream is the catalog** — no local model lists, no strict model-existence checks. Unknown models fail at request time, where a chain falls over; a stale local catalog would reject valid new models, and the provider can't.
- **No default anything** — the gateway starts with zero chains. Every chain is created per use case by the client over the API; real provider model names never leak into your apps.
- **Loopback by default** — binds `127.0.0.1` unless you explicitly set `bind: 0.0.0.0`. Auth is always on, but not-listening beats not-authorized.
- **Observability outside the process** — `guvna-cli` over the gateway's own HTTPS endpoint, Prometheus `/metrics`, Grafana and alert rules as external definitions. No baked-in UI: a dashboard belongs in a separate component consuming public interfaces, not in this repo.
- **Two dependencies** — `yaml.v3` and a pure-Go SQLite driver (telemetry only). `CGO_ENABLED=0` must keep working.

What it does:

- **Chain-based routing** — ordered provider/model fallback steps per use case (`mychain` → bazaarlink qwen → groq llama → mistral codestral). First healthy step serves; failures walk the chain.
- **Key pools** — per-provider rotation (round-robin / least-used / sequential) with class-based quarantine (auth / rate-limit / transient) and automatic failover. One dead key never burns the pool.
- **Provider-prefixed passthrough** — call any catalog model directly as `<provider>/<model>` when you don't need a chain.
- **Self-healing** — unhealthy steps and keys cool off (60s → doubling → capped, auto-recovery); no silent mid-stream restarts; truncated streams surface as failures.
- **Remote CLI** — `guvna-cli status|logs|chains` against the gateway's HTTPS endpoint, from anywhere.
- **Free-tier native** — ships with configs for Bazaarlink, Groq, Mistral, Gemini, OrcaRouter free tiers ([limits reference](docs/FREE_MODELS.md)).

## Quickstart

```sh
git clone https://github.com/Hesamation/guvna && cd guvna
cp .env.example .env        # fill in ADMIN_KEY, API_KEYS, and at least one provider key
docker compose -f deploy/docker-compose.yml up -d
```

Or without Docker:

```sh
go install github.com/Hesamation/guvna/cmd/guvna@latest
set -a; source .env; set +a
guvna -config config.yaml          # listens on 127.0.0.1:20128 by default
```

Tagged releases also publish an image to `ghcr.io/Hesamation/guvna`.

### First request

Chains are created by clients over the API (there are no default chains):

```sh
curl -X POST http://127.0.0.1:20128/v1/chains \
  -H "Authorization: Bearer $CLIENT_KEY" -H "Content-Type: application/json" \
  -d '{"name":"myfree","steps":[
        {"provider":"bazaarlink","model":"qwen/qwen3.7-flash:free"},
        {"provider":"groq","model":"llama-3.3-70b-versatile"}]}'

# chat — any OpenAI SDK works, just point base_url at Guvna
curl http://127.0.0.1:20128/v1/chat/completions \
  -H "Authorization: Bearer $CLIENT_KEY" -H "Content-Type: application/json" \
  -d '{"model":"myfree","messages":[{"role":"user","content":"hello"}]}'
```

If `bazaarlink` is down or rate-limited, the same request transparently falls through to `groq`. Or skip chains entirely: `"model": "groq/llama-3.3-70b-versatile"`.

Manage chains remotely:

```sh
export GUVNA_URL=https://your-gateway.example.com GUVNA_ADMIN_KEY=...
guvna-cli chains list
guvna-cli status
```

## Configuration

Everything is YAML in git (`config.yaml`): providers, models, rotation strategy, quarantine, config chains. Keys live only in env (`.env`, gitignored — see `.env.example`). Per-step request params can be set inside chain steps (`params`, client values always win). See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for the full data model and the canonical semantics (rotation, quarantine, cool-offs, restart behavior), and [docs/OPERATIONS.md](docs/OPERATIONS.md) for deployment and troubleshooting.

## Status

Use-ready and running in production (single user) since 2026-08-16. Token compression (quota stretching) was the original motivation and remains a future idea — explicitly unbuilt, and treated with skepticism: independent measurements found ~3.7% real bill savings for this class of tools ([archived snapshot](docs/RESEARCH-2026-08.md)). The gateway is valuable without it.

## Documentation

| File | What it contains |
|---|---|
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | Design + the canonical semantics: routing, key pools, quarantine, cool-offs, restart behavior |
| [docs/OPERATIONS.md](docs/OPERATIONS.md) | Deploy (Compose / binary / systemd / TLS), config reference, backup & restore, upgrades, troubleshooting runbook |
| [docs/DECISIONS.md](docs/DECISIONS.md) | Decision log with dates and rationale — read first, don't relitigate without new evidence |
| [docs/FREE_MODELS.md](docs/FREE_MODELS.md) | Per-provider free-tier limits (dated snapshot; drifts weekly) |
| [docs/RESEARCH-2026-08.md](docs/RESEARCH-2026-08.md) | Archived research: why scratch, gateway comparisons, compression reality check |
| [BENCHMARKS.md](BENCHMARKS.md) | Measured receipts: RSS, RPS, tails vs new-api and litellm |

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Security issues: [SECURITY.md](SECURITY.md).

## License

MIT. All reference material used (OmniRoute catalog data, new-api, rtk, caveman) is MIT.

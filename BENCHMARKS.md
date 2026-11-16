# Benchmarks

Guvna's claim is "lean": one static binary, ~18MB idle, single-user throughput far beyond anything one user generates. These numbers are the receipt, measured against the two gateways named in RESEARCH-2026-08.md as the alternatives.

**Date: 2026-11-16. Machine: Arch laptop, 6 cores, 16GB RAM, Go 1.26.11. All traffic loopback.**

## Method

- Harness: `bench/` (stdlib-only; `go build -o /tmp/bench ./bench`)
- Upstream: the harness's `mock` subcommand — a zero-latency OpenAI-compatible server. This measures **gateway overhead**, not provider latency: real-world latency is dominated by the model, not the gateway, so the interesting comparison is what each gateway itself adds per request and what it costs to run.
- Each gateway pointed at the same mock; each run at its own defaults, closest-to-default config that serves `POST /v1/chat/completions` for one model. Non-streaming and streaming phases, 1 and 8 concurrent workers, fixed duration.
- RSS sampled from `/proc/<pid>/status` (`VmRSS`) before and during load.

| Gateway | Version | Idle RSS |
|---|---|---|
| guvna | main @ d85dd1f (v0.1.3 content) | **18 MB** |
| new-api | v1.0.0-rc.51 (SQLite, root/token via setup API) | 95 MB |
| litellm | 1.109.1 (proxy, master key, one model) | 413 MB |

## Results (requests = chat completions end-to-end through the gateway)

| Phase | Conns | guvna RPS / p50 / p95 / p99 / max | new-api RPS / p99 / max | litellm RPS / p99 / max |
|---|---|---|---|---|
| non-stream | 1 | **6,363** / <1ms / <1ms / <1ms / 7ms | 220 / 7ms / 59ms | 101 / 13ms / 16ms |
| non-stream | 8 | **18,493** / <1ms / 1ms / 2ms / 16ms | 160 / 1,134ms / 7.2s | 123 / 81ms / 95ms |
| stream | 8 | **11,922** / 1ms / 1ms / 2ms / 26ms | 163 / 937ms / 3.8s | 53 / 209ms / 252ms |
| raw mock (no gateway) | 8 | 49,000 / <1ms / <1ms / 1ms / 5ms | — | — |

Failures were zero in all guvna/litellm runs and all new-api runs shown (an earlier new-api run used an invalid token — those failures are excluded as misconfiguration, not measurement).

## Reading the numbers

- **Throughput.** guvna serves ~150x litellm's and ~115x new-api's request rate at 8 conns, and retains a flat p99 (2ms) where the others grow tails. Every one of these gateways is massively overprovisioned for a single user; the claim is not "you need 18k RPS" but "the gateway costs effectively nothing per request and never adds a tail."
- **Idle RSS.** 18 MB vs 95 MB vs 413 MB. On a small VPS next to other services, this is the difference between a rounding error and a line item.
- **Tails.** new-api's p99 collapse under 8 workers comes from per-request logging into SQLite (write contention; max latency 7.2s = a stall, not slowness). litellm's flat ~100 RPS ceiling is its Python middleware stack. guvna's overhead over the raw mock (18.5k vs 49k RPS) is relay + per-request structured logging; real deployments put a real provider behind this, where 200ms model latency dwarfs all of it.

## Honest caveats

- Zero-latency upstream on loopback is a stress position, not a simulation: absolute RPS numbers are meaningless in production (a free-tier model answers in hundreds of ms). The *relative* overhead and RSS numbers are the point.
- Defaults, not tuned configs: litellm ran with its full callback/logging machinery (it is an enterprise platform; that machinery is its product). new-api ran on SQLite with its per-request logging on. guvna ran with its default per-request log line enabled.
- One machine, one run each, warm caches. Treat deltas smaller than run-to-run noise (~5%) as noise.
- guvna's RSS peaks at 185–208MB under this synthetic load (~370k requests/20s) — the telemetry event buffer is uncapped between its 30s flushes, so extreme bursts hold every event in memory. At real single-user volume this never materializes, but it is the same family of unbounded-growth issue as the retention knob, and a bounded buffer is the honest next fix if it ever matters.

## Reproduce

```sh
go build -o /tmp/bench ./bench
/tmp/bench mock -addr 127.0.0.1:9100 &
# guvna: config with one provider (base_url http://127.0.0.1:9100) + chain "bench"
/tmp/bench load -url http://127.0.0.1:20198 -key KEY -model bench -conns 8 -dur 20s -rss-pid <pid>
# litellm: model_list entry {model: openai/bench-model, api_base: http://127.0.0.1:9100/v1}
# new-api: channel type 1 → base_url http://127.0.0.1:9100, token via setup API
```

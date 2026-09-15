# Distributed Rate Limiter

Three distributed rate-limiting strategies in Go, benchmarked against each
other on Google Cloud to quantify the tradeoff between **enforcement
accuracy**, **tail latency**, and **availability under dependency failure**.

You cannot have exact global enforcement, low latency, and availability at the
same time. This measures what each one costs.

---

## Results

> **Status: E1–E3 complete.** E4 could not be run on managed Memorystore — see below.

### E1 — Strategy comparison

4,000 RPS offered against a 2,500/sec limit, 8 replicas × 4 vCPU, 3 runs each,
~360,000 requests per run. All runs VALID — zero failures, zero shedding.

| Strategy | client p50 | client p99 | **server p50** | **server p99** | Sustained over-admission | Redis ops/s |
|---|---:|---:|---:|---:|---:|---:|
| `centralized` | 9.38 ms | 15.28 ms | **1,133 µs** | 1,859 µs | **−0.00%** | 3,428 |
| `slidingwindow` | 9.04 ms | 16.02 ms | 760 µs | 2,243 µs | −0.06% | 3,428 |
| `localsync` (100ms) | 8.37 ms | 15.01 ms | **19 µs** | **117 µs** | **+14.25%** | ~80 |

**`localsync` answers 60× faster at p50 and 16× at p99, and over-admits by
14.25%** — while putting 43× less load on Redis. That is the tradeoff this
project exists to measure.

Note what the client columns do *not* show: all three strategies land within
noise of each other at ~9 ms, because network and platform overhead swamp the
limiter entirely. Measuring only at the client would have concluded the
strategies are indistinguishable. See
[docs/benchmarks.md](docs/benchmarks.md#e1--strategy-comparison).

### E2 — Enforcement error vs sync interval and replica count

72 runs, 3 per cell, all VALID with zero platform shedding. Sustained
over-admission, mean of 3 runs:

| R \ sync | 20ms | 50ms | 100ms | 250ms | 500ms | 1s |
|---:|---:|---:|---:|---:|---:|---:|
| **2** | 1.54% | 2.81% | 7.22% | 18.04% | 32.29% | 39.26% |
| **4** | 1.99% | 5.54% | 9.70% | 26.78% | 37.96% | 31.11% |
| **8** | 2.65% | 6.32% | 12.48% | 31.16% | 40.92% | 42.83% |
| **16** | 2.68% | 6.30% | 12.64% | 31.10% | 41.59% | 42.86%\* |

**The design doc predicted error would scale with `(R−1)`. It does not.** Error
*saturates* in replica count — R=8 and R=16 are indistinguishable at every
interval. Total offered load is fixed, so doubling replicas halves each one's
share, and the fleet's unsynced admissions stay near
`admission_rate × sync_interval` however many replicas divide it. **The sync
interval governs; the replica count barely matters past R≈8.**

\*Read only the 20–100ms region as measurement. Offering 1,000 RPS against a
700 limit caps observable over-admission at 42.86%, and R=16/1s measures
exactly that — the experiment's ceiling, not the limiter's behaviour. Run-to-run
spread confirms it: under 1 point below 100ms, up to 29.6 points at R=4/1s.
Details in [docs/benchmarks.md](docs/benchmarks.md#e2--enforcement-error-vs-sync-interval-and-replica-count).

### E3 — Throughput ceiling

`centralized`'s server-side p50 knees **42×** between 6,000 and 8,000 RPS as
Redis saturates (1,421µs → 59,871µs). `localsync` shows no knee at all, holding
11–13µs throughout. That is the shared store's throughput becoming the fleet's
throughput — the structural argument for keeping it off the request path.

Caveats on that result, and on why E4 has no results, are in
[docs/benchmarks.md](docs/benchmarks.md).

### E4 — Failure injection

**Not obtained.** A Memorystore instance on DIRECT_PEERING cannot be
partitioned from Cloud Run: firewall rules are connection-tracked and routes
into a peered network are rejected. Three approaches and what each taught are
written up in [docs/benchmarks.md](docs/benchmarks.md#e4--failure-injection).

---

## What has actually been verified

Everything below was run and observed, as distinct from the results above:

- All three strategies pass unit tests, including the real Lua scripts
  executed against an in-process Redis, and integration tests against a
  real Redis in CI.
- Deployed on GCP against Memorystore. Verified there: 8 concurrent requests
  at cost 400 against a burst of 1000 admitted **exactly 2** and denied 6 —
  the Lua atomicity property holding under real concurrency across replicas.
- `centralized` measured **0.00%** sustained over-admission, the correctness
  property it is supposed to have.
- A validation ladder measured **12,000 RPS sustained** across 20 replicas of
  4 vCPU, with **p99 flat at ~14.3ms** — unchanged from the p99 at 2,000 RPS,
  a 6× increase in load with no tail degradation. Zero platform shedding at
  every rung; the service was never saturated, so its real ceiling is higher
  still.
- Server-side handler latency stayed at **14–17µs** throughout, including runs
  where clients saw multi-second latencies — the limiter was never the
  bottleneck.

---

## Architecture

```
  loadgen (GCE VM, same region)
        │  open-loop, constant arrival rate
        ▼
  Cloud Run: limiterd × N stateless replicas
        │  POST /v1/allow {key, cost} → {allowed, remaining, retry_after}
        ▼
  Memorystore Redis  ← shared limiter state
```

### The three strategies

| | Where state lives | Redis on request path | Enforcement | If Redis dies |
|---|---|---|---|---|
| `centralized` | Redis, authoritative | Yes, every request | Exact | Cannot serve — must pick fail-open or fail-closed |
| `slidingwindow` | Redis, two counters | Yes, every request | Approximate (estimator error) | Cannot serve |
| `localsync` | Replica-local, reconciled | No | Approximate (replica skew) | Keeps serving, accuracy decays |

`localsync` is the interesting one. It is not merely faster — it is *available
when Redis is not*, and the price is bounded accuracy loss. That is the
project's central argument, and [E4](docs/benchmarks.md#e4) measures it.

Full reasoning, including the alternatives that were rejected, is in
[docs/design.md](docs/design.md).

---

## Running it

### Locally

```bash
make run-local
```

Then:

```bash
curl -X POST localhost:8080/v1/allow -d '{"key":"demo","cost":1}'
```

### Tests

```bash
make test
```

No Docker required — the Lua scripts run against an in-process Redis. For the
real-Redis suite:

```bash
make test-integration
```

### On GCP

```bash
cp infra/terraform.tfvars.example infra/terraform.tfvars
```

Fill it in, then:

```bash
make infra-up
```

SSH to the load generator (never benchmark from a laptop — see
[methodology](docs/benchmarks.md#methodology)), then validate before measuring:

```bash
make bench-validate
```

That gate confirms the load generator can actually produce the offered rate. If
it cannot, it is the bottleneck and every subsequent number describes the
client rather than the service. Then:

```bash
make bench
```

And when you are done, every time:

```bash
make infra-down
```

---

## Cost

Designed to run inside roughly **$40** of GCP free credits. The guardrails are
described in [docs/design.md](docs/design.md#cost); the one that matters is the
explicit `max_instances` ceiling in
[infra/main.tf](infra/main.tf), which bounds what a runaway load generator can
spend. Note that GCP budget alerts *notify* — they do not cap.

---

## Layout

| Path | |
|---|---|
| `cmd/limiterd` | the service |
| `cmd/loadgen` | open-loop load generator |
| `internal/limiter` | the three strategies and their tests |
| `internal/redisx` | Redis client and shared counter |
| `bench/` | harness, sweeps, raw results |
| `infra/` | Terraform |
| `docs/design.md` | design doc |
| `docs/benchmarks.md` | methodology and results |

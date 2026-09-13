# Distributed Rate Limiter

Three distributed rate-limiting strategies in Go, benchmarked against each
other on Google Cloud to quantify the tradeoff between **enforcement
accuracy**, **tail latency**, and **availability under dependency failure**.

You cannot have exact global enforcement, low latency, and availability at the
same time. This measures what each one costs.

---

## Results

> **Status: not yet measured.** The harness is built and verified; the cloud
> runs have not been performed. Numbers below are placeholders and are marked
> as such deliberately — see [docs/benchmarks.md](docs/benchmarks.md) for the
> methodology they will be collected under.

### E1 — Strategy comparison

8000 RPS offered against a 5000/sec limit, 4 replicas, 3 runs per configuration.

| Strategy | p50 | p99 | p999 | Sustained over-admission | Redis ops/sec |
|---|---|---|---|---|---|
| `centralized` | — | — | — | — | — |
| `slidingwindow` | — | — | — | — | — |
| `localsync` (100ms) | — | — | — | — | — |

### E2 — Enforcement error vs sync interval and replica count

The chart this project exists to produce. Over-admission is expected to grow
with both the sync interval and the replica count; the
[design doc](docs/design.md#localsync) states the predicted bound, and the
benchmark measures where reality falls inside it.

*(chart pending)*

---

## What has actually been verified

Everything below was run and observed, as distinct from the results above:

- All three strategies pass unit tests, including the real Lua scripts
  executed against an in-process Redis, and integration tests against a
  real Redis in CI.
- End-to-end local smoke test of the full path — service, Redis, load
  generator, metrics.
- `centralized` measured **0.00%** sustained over-admission locally, which is
  the correctness property it is supposed to have.

A laptop smoke test at 800 RPS showed server-side p50 of **12µs** for
`localsync` against **300µs** for `centralized` — the expected direction,
since `localsync` keeps Redis off the request path. **These are not results.**
They came from a single-threaded in-process Redis on a laptop, and they are
recorded here only to show the harness works, not to claim a finding.

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

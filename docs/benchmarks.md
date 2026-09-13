# Benchmarks

## Status

The harness is built and verified end to end. **The cloud runs have not been
performed**, so this document currently describes the methodology and holds
empty result tables. Nothing here is a measured claim unless it says so.

## Methodology

These rules are stated because they are what make the numbers mean anything.

**Open-loop load generation.** Requests are issued on a schedule derived from
wall-clock time, not from when replies arrive. A closed-loop generator stops
issuing while the server is slow, so the stalls under measurement are never
sampled — coordinated omission. Latency is recorded from each request's
*intended* send time, so any delay the client itself introduces counts against
the result rather than disappearing.

**Report p50 / p99 / p999, never the mean.** These distributions are not
symmetric; the mean hides the behaviour of interest.

**Load generator and service in the same region.** Otherwise the measurement is
of the public internet and the limiter's own cost vanishes into the noise.
`bench/run.sh` refuses to run off-GCE unless explicitly overridden.

**Pin the replica count during a run.** `min-instances == max-instances`.
Without this, autoscaling changes the fleet size mid-measurement and
enforcement error becomes a function of something uncontrolled.

**Discard a warmup window.** Cold caches and empty connection pools are not
what is being measured. Cold starts are measured deliberately and separately.

**Three runs per configuration; report the spread.** If run-to-run variance
exceeds the difference between two strategies, there is no result — and saying
so is more credible than concealing it.

**Measure at both ends.** The load generator reports client-observed latency;
the service reports its own handler duration at `/stats`. The gap is network
plus platform overhead; the server-side figure isolates the limiter itself.

**Keep runs short.** Cloud Run cost is dominated by request count. 90 seconds
at 5k RPS is ample.

### Validity gates

A run self-reports as INVALID when:

- the client shed any request (it could not keep up, so latency is understated);
- achieved rate fell below 95% of offered (the generator, not the service, is
  the bottleneck);
- more than 1% of requests failed (it is a failure experiment, not a latency
  measurement).

`make bench-validate` runs the ladder that establishes the generator's ceiling
before any real experiment. **Do this before trusting a single number.** A load
generator that saturates before the service does is the most common way a cloud
benchmark produces confident nonsense.

### Two enforcement metrics, and why

Enforcement error is reported two ways, and using the wrong one silently
produces a wrong chart:

- **Sustained over-admission** — total admitted against `limit × duration`.
  This is the cross-strategy headline. Burst allowance amortises away over a
  long run, so all three strategies are held to the same promise.
- **Per-second over-admission** — worst and mean single-second excess. Useful
  for showing burstiness, but **a token bucket is expected to exceed a
  per-second limit by up to its burst by design.** This number must never be
  used to compare `centralized` against the window strategies.

## Experiments

### E1 — Strategy comparison

8000 RPS offered against a 5000/sec limit, 4 replicas, 3 runs each.

| Strategy | p50 | p99 | p999 | Sustained over-admission | Redis ops/sec |
|---|---|---|---|---|---|
| `centralized` | — | — | — | — | — |
| `slidingwindow` | — | — | — | — | — |
| `localsync` (100ms) | — | — | — | — | — |

### E2 — Enforcement error vs sync interval and replica count

`localsync`, sweeping sync interval `{20ms … 1s}` across replica counts
`{2, 4, 8, 16}`.

**Predict before measuring.** The bound in [design.md](design.md#localsync)
says excess per interval is at most `(R − 1) × A`. Write down the expected
curve first, then plot measured against predicted. If they disagree, finding
out why is the most valuable thing that can happen in this project — far more
so than a curve that matches.

| Replicas \ Sync | 20ms | 50ms | 100ms | 250ms | 500ms | 1s |
|---|---|---|---|---|---|---|
| 2 | — | — | — | — | — | — |
| 4 | — | — | — | — | — | — |
| 8 | — | — | — | — | — | — |
| 16 | — | — | — | — | — | — |

### E3 — Throughput ceiling

Ramp offered load until `centralized` p99 knees over and Redis saturates.

| Strategy | Max sustained RPS | Limiting factor |
|---|---|---|
| `centralized` | — | — |
| `localsync` | — | — |

### E4 — Failure injection

Kill Redis partway through a run. Three configurations:

| Run | Expected behaviour | Measured |
|---|---|---|
| `centralized`, fail-closed | 503s, admissions collapse to zero; backend protected, service down | — |
| `centralized`, fail-open | Everything admitted, enforcement gone; service up, backend exposed | — |
| `localsync` | Keeps serving from local state; `sync_failures` climbs while admissions continue; accuracy decays with outage duration | — |

The contrast between the third row and the first two is the strongest result in
the project.

### E5 — Cold-start admission spike (optional)

A fresh Cloud Run replica starts with a full local bucket, so an autoscaling
event transiently inflates the fleet's total allowance.

## What surprised me

*This section is the most valuable part of this document. Fill it in as things
go wrong.*

### The enforcement metric was measuring the wrong thing

Caught during the first local smoke test, before any cloud run.

`centralized` — the strategy whose entire purpose is exact enforcement —
reported **25% mean over-admission**. The implementation was correct; the
metric was not.

A token bucket configured `rate=500/s, burst=500` can legitimately admit 1000
requests inside one wall-clock second: drain a full bucket instantly, then
consume a second's worth of refill. The per-second metric was holding the
bucket to a *window* limiter's promise, which the bucket never made.

Left uncaught, this would have produced an E1 table showing the exact strategy
over-admitting by 25% and the approximate strategies at 0% — a result that is
not merely wrong but backwards, and plausible enough to survive review. The fix
was the sustained metric described above; `centralized` then measured 0.00%, as
it should.

The general lesson is the one worth carrying: the benchmark is as much a piece
of engineering as the system, and a metric that quietly compares two different
guarantees fails silently rather than loudly.

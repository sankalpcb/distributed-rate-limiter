# Design: Distributed Rate Limiter — Benchmarking the Accuracy/Latency Tradeoff

**Date:** 2026-09-13
**Status:** Approved for planning
**Author:** Sankalp Badrinath

---

## 1. Purpose

Build a small, sharp distributed systems project suitable for a Google Cloud
Software Engineer II (AI & Infrastructure) application, within two calendar
weeks and roughly $40 of a $150 GCP free-credit budget.

The project is a distributed rate limiter written in Go. Its claim is not
"I built a rate limiter" — that is a tutorial. Its claim is:

> Three enforcement strategies were implemented and benchmarked against each
> other, quantifying the tradeoff between enforcement accuracy, tail latency,
> and availability under dependency failure.

Every scoping decision below follows from that claim. Work that does not serve
it is cut.

### Why this problem

Distributed quota enforcement is a real problem Google solves internally, and
the interesting part of it is precisely the part technical interviewers probe:
you cannot have exact global enforcement, low latency, and availability at the
same time. The project makes that tension measurable rather than theoretical.

## 2. Goals and non-goals

### Goals

- G1. Three working rate-limiting strategies behind one interface, selectable
  by configuration.
- G2. A reproducible benchmark harness that produces the results with one
  command.
- G3. Measured results: tail latency, enforcement error, throughput ceiling,
  and behaviour under Redis failure.
- G4. A written design doc and benchmark writeup in the repo.
- G5. Total cloud spend under $50, with teardown automated.

### Non-goals

Explicitly out of scope, and to stay out of scope:

- Authentication, authorisation, multi-tenancy.
- Any user interface or dashboard.
- Persistence beyond Redis.
- Kubernetes, service mesh, or a custom control plane.
- gRPC. HTTP/JSON only; gRPC adds a day of plumbing and changes no result.
- Production hardening beyond what the benchmarks require.

This project has no UI and therefore cannot speak to the "accessible
technologies" preferred qualification in the job description. That is
accepted; bolting on a dashboard to gesture at it would weaken the project.

## 3. Architecture

```
  loadgen (GCE VM, same region as service)
        |  open-loop, constant arrival rate
        v
  Cloud Run: limiterd x N stateless replicas
        |  POST /v1/allow {key, cost} -> {allowed, remaining, retry_after}
        v
  Redis (Memorystore Basic, 1 GB)  <- shared limiter state
```

Replicas are stateless with respect to identity: replica count is a benchmark
knob, not a redeploy. All coordination happens through Redis (or, for
`localsync`, through periodic reconciliation with Redis).

### 3.1 Components

**`limiterd`** — the service under test. A single meaningful endpoint:

```
POST /v1/allow
  request:  {"key": "user-123", "cost": 1}
  response: {"allowed": true, "remaining": 87, "retry_after_ms": 0}
```

Strategy selected via environment variable (`LIMITER_STRATEGY`), so a
benchmark configuration change never requires a code change.

**`Limiter` interface** — the core abstraction:

```go
type Decision struct {
    Allowed    bool
    Remaining  int64
    RetryAfter time.Duration
}

type Limiter interface {
    Allow(ctx context.Context, key string, cost int64, now time.Time) (Decision, error)
}
```

`now` is a parameter rather than a call to `time.Now()` inside the
implementation. This makes every strategy deterministically testable with a
fake clock: "does the bucket refill correctly across ten minutes" becomes a
microsecond-scale unit test rather than a flaky sleep.

**`loadgen`** — the load generator. Described in section 5.

**Redis** — shared state. Memorystore Basic tier, 1 GB, same region.

### 3.2 The three strategies

#### `centralized` — token bucket in a Redis Lua script

State per key: `tokens` (float), `last_refill_ts`. A single Lua script
performs refill-and-deduct atomically in one round trip:

```
elapsed  = now - last_refill_ts
tokens   = min(burst, tokens + elapsed * rate)
if tokens >= cost:
    tokens = tokens - cost
    allowed = true
else:
    allowed = false
persist(tokens, now)
```

- Enforcement: exact.
- Latency: one Redis round trip on every request, on the critical path.
- Scaling: Redis throughput is a hard ceiling for the whole fleet.
- Failure: cannot serve without Redis. Requires an explicit fail-open or
  fail-closed policy (see E4).

#### `localsync` — local counter with periodic delta sync

Each replica admits against a local counter. Every `SYNC_INTERVAL`
milliseconds it pushes its local delta to Redis and pulls the current global
count.

Between syncs, replicas are blind to each other's admissions, so the fleet
**over-admits**. Expected worst case per sync interval is approximately

```
excess <= (R - 1) * A
```

where `R` is replica count and `A` is the admissions a single replica can make
within one interval before reconciling. This is a loose upper bound; the point
of E2 is to measure where reality falls inside it.

- Enforcement: approximate, error growing with both `R` and `SYNC_INTERVAL`.
- Latency: no Redis on the critical path. Local counter only.
- Failure: degrades gracefully. Keeps serving from local state, drifting
  further out of enforcement the longer Redis is unavailable.

This strategy exists to make the central argument of the project: it is not
merely faster, it is *more available*, and the price is bounded accuracy loss.

#### `slidingwindow` — weighted two-fixed-window approximation

```
count ~= prev_window_count * overlap_fraction + current_window_count
```

O(1) memory per key, and it corrects the burst-at-the-boundary flaw of naive
fixed windows (where a client can spend a full window's budget at the end of
one window and again at the start of the next).

The exact alternative — a Redis sorted set holding a timestamp per request —
is rejected because it costs O(requests) memory per key. Knowing why the
approximation was chosen is the point; this reasoning belongs in the writeup.

- Enforcement: approximate, with small bounded error from the uniform-arrival
  assumption within the previous window.
- Latency: one Redis round trip, comparable to `centralized`.
- Position: the middle of the results table, making it a story rather than a
  binary.

## 4. Testing

- **Unit tests** for all three strategies driven by a fake clock. Refill
  behaviour, burst handling, boundary conditions, and cost > 1 requests.
- **Integration test** for each strategy against a real Redis (Docker
  Compose locally), verifying the Lua scripts behave under concurrency.
- **CI**: GitHub Actions running unit and integration tests on every push.

Correctness tests are not the benchmark. They guard against the failure mode
where a benchmark measures a bug.

## 5. Benchmark methodology

The numbers are the deliverable; the methodology is what makes them credible.
These rules are documented in `docs/benchmarks.md` because stating them is
itself the signal.

- **Open-loop load generation.** Requests are issued on a fixed schedule
  regardless of whether prior requests have returned. A closed-loop generator
  stops issuing while the server is slow, concealing precisely the stalls
  being measured — coordinated omission.
- **Report p50 / p99 / p999. Never the mean.** Latency distributions here are
  not symmetric and the mean hides the behaviour of interest.
- **Loadgen and service in the same region.** Otherwise the measurement is of
  the public internet and the limiter's own cost vanishes into noise.
- **Pin `min-instances` during latency runs** so Cloud Run cold starts do not
  contaminate the data. Cold starts are then measured deliberately as E5.
- **Discard a warm-up window; run each configuration three times; report the
  spread.** If run-to-run variance exceeds the difference between two
  strategies, there is no result, and saying so is more credible than
  concealing it.
- **Measure latency at both ends.** Client-observed latency and server-side
  handler duration. The gap is network plus platform overhead; the server-side
  figure isolates what the limiter actually costs.
- **Keep runs short.** 90 seconds at 5k RPS is 450k requests — ample
  steady-state samples. A five-minute run costs three times as much for no
  additional statistical power.

`loadgen` records latency into an HDR histogram and separately counts admitted
requests per key per second, so enforcement error can be computed against the
configured limit. Results are written as JSON to `bench/results/`.

### 5.1 Benchmark harness

A wrapper (`make bench`) that drives `gcloud run services update`
programmatically to sweep replica counts and strategy configurations, then
collects results. Half a day
of work. Justified because a benchmark re-runnable with one command is the
difference between having obtained a number once and having a reproducible
result — and it is what makes re-running everything after a bug fix painless
rather than dreaded.

## 6. Experiments

Run in this order.

**E1 — The results table.** Fixed load (~5k RPS), 4 replicas, all three
strategies. Columns: p50, p99, p999, over-admission %, Redis ops/sec. This
table goes at the top of the README.

**E2 — Enforcement error scaling.** For `localsync`: over-admission %
against sync interval, one line per replica count (2, 4, 8, 16).

Predict the curve from the bound in section 3.2 *before* running it, then plot
measured against predicted. Predicting a result and confirming it is a
substantially stronger story than observing one. If measurement disagrees with
the model, diagnosing why is the most valuable outcome available in this
project.

**E3 — Throughput ceiling.** Ramp load until `centralized` p99 knees over and
Redis saturates. Report max sustained RPS per strategy. Produces a capacity
number.

**E4 — Failure injection.** Kill Redis mid-run.

- `centralized` cannot serve. Implement both policies behind a flag and graph
  both: **fail-open** (allow all; protects availability, exposes the backend
  the limiter was shielding) and **fail-closed** (deny all; protects the
  backend, causes a self-inflicted outage).
- `localsync` continues serving from local state, with accuracy degrading over
  the outage duration.

This contrast is the strongest single result in the project.

**E5 — Cold-start admission spike (optional).** A fresh Cloud Run replica
starts with a full local bucket, so an autoscaling event transiently inflates
the fleet's total allowance. Subtle, real, and rarely anticipated.

## 7. Cost model and guardrails

Target: under $50 of the $150 budget. Estimates are approximate and vary by
region; verify in the GCP pricing calculator before relying on them.

| Item | Estimate | Notes |
|---|---|---|
| Cloud Run | $7–20 | Requests dominate (~$0.40/M after 2M free). Compute is pennies: 16 instances for 5 min ~ $0.12. |
| Memorystore Basic 1 GB | ~$6 | ~$0.05/hr. Exists only during benchmark sessions. |
| Loadgen VM (n2-standard-4) | ~$3 | ~$0.19/hr, ~15 hours total. |
| Monitoring / egress | ~$0 | Same region; loadgen owns all latency measurement. |

### Guardrails, all established on day 1

1. **Billing budget with alerts at $25 / $50 / $100.** Note that GCP budget
   alerts *notify*; they do not cap spending. A hard cap requires wiring
   budget -> Pub/Sub -> a function that disables billing, which is rejected
   here as fiddly and capable of killing a project mid-run. Alerts plus
   discipline is proportionate at this scale.
2. **Explicit `max-instances` on the Cloud Run service (20).** This is the
   guardrail that actually protects the budget. The platform default ceiling
   is 100, and a loadgen bug — an unbounded retry loop — could autoscale into
   real money before it is noticed.
3. **Terraform for all infrastructure.** The value is not the IaC line on a
   resume; it is that `terraform destroy` is reliable teardown. Manual
   teardown is how a Memorystore instance survives forgotten for three weeks.
4. **`make up` / `make down`** wrapping Terraform, so ending a session is one
   command.
5. **Reconcile the billing report against this table on day 3.** Divergence
   should surface on day 3, not day 11. "Modelled the cost, then checked the
   model" is also a good line in the writeup.

## 8. Timeline

Twelve working days, with slack at the end.

| Day | Work |
|---|---|
| 1 | Project setup, billing alerts, Terraform skeleton, hello-world Go service deployed to Cloud Run. **Goal: end-to-end deploy proven on day 1.** |
| 2–3 | `Limiter` interface, `centralized` token bucket in Lua, fake-clock unit tests, integration test against local Redis |
| 4 | `loadgen`: open-loop driver, HDR histogram, admitted-count accounting, JSON output |
| 4.5 | Benchmark harness (programmatic deploy sweep) |
| 5 | First real runs: E1 for `centralized`, E3 saturation. Sanity-check and fix |
| 6–7 | `localsync` and the E2 sweep. Predict the curve first, then measure |
| 8 | `slidingwindow`, folded into the E1 table |
| 9 | E4 failure injection, both fail policies |
| 10 | Charts, README, design doc, benchmark writeup |
| 11–12 | Buffer. If free: E5, or multi-region extension |

Proving deployment on day 1 is deliberate. The most common way a two-week
cloud project dies is spending days 5 through 8 fighting IAM and deployment
configuration instead of writing the interesting code.

### Cut list

If behind schedule, cut in this order:

1. E5 (cold-start spike)
2. `slidingwindow` (drop to two strategies)
3. Terraform (fall back to `gcloud` shell scripts)
4. E3 (throughput ceiling)

**Do not cut E2 or E4.** Those are the project.

## 9. Repository layout and deliverables

```
distributed-rate-limiter/
  cmd/limiterd/          service entrypoint
  cmd/loadgen/           load generator
  internal/limiter/      three strategies + fake clock tests
  internal/redis/        Lua scripts, client wrapper
  bench/                 harness, run configs, raw results (JSON)
  infra/                 Terraform
  docs/design.md         design doc
  docs/benchmarks.md     methodology, results, surprises
  README.md
```

**The README is the product.** Results table and the E2 chart above the fold —
before installation instructions, before architecture. A reader should learn
what was found without scrolling. Then a compact architecture diagram, the
tradeoff summary in three sentences, then how to run it.

**`docs/design.md`** must include the alternatives rejected and why. That
section is what makes it a design doc rather than a description.

**`docs/benchmarks.md`** must include a "what surprised me" section. If the
measured over-admission curve diverged from the predicted bound and the cause
was tracked down, that paragraph is worth more than the rest of the repository
— it is evidence of debugging under uncertainty, which is both hardest to fake
and hardest to assess in an interview.

## 10. Resume framing

Templates. Numbers are filled from actual runs and never invented — an
interviewer will ask how each was measured.

> **Distributed Rate Limiter** — Go, Cloud Run, Redis, Terraform
>
> - Built and benchmarked three distributed rate-limiting strategies serving
>   **[X]k RPS** across **[N]** stateless replicas; reduced p99 latency
>   **[A]x versus centralized enforcement** at a measured **[B]%
>   over-admission** cost.
> - Quantified how enforcement error scales with replica count and sync
>   interval using an **open-loop load generator** (avoiding coordinated
>   omission), establishing the accuracy/latency tradeoff curve for the design.
> - Demonstrated graceful degradation under Redis failure, contrasting
>   **fail-open vs fail-closed** availability tradeoffs; capped infrastructure
>   cost at **$[C]** via IaC teardown and instance limits.

The third bullet does double duty: failure reasoning and cost discipline, both
named explicitly in the job description.

Know every number cold before listing it. A bullet claiming an improvement
whose mechanism cannot be explained is worse than no bullet.

## 11. Alternatives considered and rejected

**Multi-region failover router.** Deploy a trivial service to three regions
behind a Global External HTTP(S) Load Balancer, break a region, measure
failover. On-theme for Global Networking and fast to build, but it is almost
entirely configuration — a reviewer skimming GitHub sees a thin repository, and
it reads as an experiment rather than a system. The global forwarding rule also
bills continuously (~$18/month) whether or not traffic flows. Retained as an
optional day-11 extension to this project rather than a project of its own.

**Pub/Sub work queue with backpressure and dead-lettering.** Nearly free to
run and good interview material on delivery semantics, but it is the most
common of the candidate projects — close enough to a tutorial that
differentiation would have to come entirely from failure-injection work, which
is the expensive half anyway.

**Sharded consistent-hashed cache with a control plane.** Genuinely
interesting, but membership and discovery push it onto GKE, which means a
cluster idling against the credit budget and roughly a month of work.

**Exact sliding window via Redis sorted sets.** Rejected in favour of the
weighted approximation on memory grounds; see section 3.2.

**gRPC transport.** Rejected: a day of plumbing that changes no result.

## 12. Risks

| Risk | Mitigation |
|---|---|
| Deployment/IAM friction consumes the first week | Day 1 goal is an end-to-end deploy of a hello-world service, before any real code |
| Load generator cannot produce enough RPS to saturate anything | Same-region VM, open-loop design, `n2-standard-4`; validate achievable RPS on day 4 against a no-op endpoint before trusting any result |
| Run-to-run variance swamps inter-strategy differences | Three runs per configuration, report the spread, and state it plainly if the result is inconclusive |
| Benchmark measures a bug rather than a strategy | Unit and integration tests land before the first benchmark run (days 2–3 precede day 5) |
| Credit burn exceeds estimate | `max-instances` cap, short runs, teardown via `make down`, day-3 billing reconciliation |
| Scope creep past two weeks | Cut list in section 8, applied without negotiation |

## 13. Success criteria

The project is done when:

1. Three strategies pass unit and integration tests in CI.
2. `make bench` reproduces the full result set unattended.
3. E1, E2, E3, and E4 have results with stated variance.
4. README leads with the results table and E2 chart.
5. `docs/design.md` and `docs/benchmarks.md` are written, including rejected
   alternatives and a "what surprised me" section.
6. All infrastructure is destroyed and total spend is under $50.

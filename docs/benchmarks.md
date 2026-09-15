# Benchmarks

## Status

E1, E2 and E3 have been run on GCP and are reported below with their raw JSON
in `bench/results/`. E4 was attempted and could not be performed against
managed Memorystore; the reasons are documented rather than left blank, since
they constrain anyone attempting the same test. E5 was not attempted.

The ladder's result changes the plan for those experiments, so read
[Capacity](#capacity-what-the-validation-ladder-established) before running
them: the configurations written below assume far more throughput than this
setup delivers.

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
  measurement);
- more than 1% were shed by the platform before reaching limiterd (the run is
  measuring Cloud Run's capacity, not enforcement).

The last gate was added after the first ladder passed a run whose p99 was 3.5
seconds -- every client-side gate was satisfied while the service behind it was
collapsing. See [the writeup below](#the-validity-gate-passed-a-run-that-was-on-fire).

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

## Capacity: what the validation ladder established

Two configurations were measured. The second is the one the experiments are
sized against; the first is kept because the difference between them is the
point. Raw JSON in `bench/results/`.

### 4 vCPU per replica, c3d-highcpu-16 generator (2026-09-14)

20 replicas, Memorystore Basic 1GB, generator in the same region.

| Offered | Achieved | p50 | p99 | Completed | Client-shed | Platform-shed | Verdict |
|---:|---:|---:|---:|---:|---:|---:|:--|
| 2,000 | 2,000 | 8.74 ms | 14.60 ms | 59,998 | 0 | 0 | VALID |
| 4,000 | 3,999 | 8.38 ms | 14.74 ms | 119,996 | 0 | 0 | VALID |
| 6,000 | 5,998 | 8.27 ms | 14.19 ms | 179,994 | 0 | 0 | VALID |
| 8,000 | 7,998 | 8.34 ms | 14.86 ms | 239,992 | 0 | 0 | VALID |
| 12,000 | 11,997 | 8.07 ms | **14.33 ms** | 359,988 | 0 | 0 | VALID |
| 16,000 | 15,567 | 601 ms | 1,218 ms | 479,984 | 74,450 | 0 | INVALID |

**12,000 RPS clean, with latency flat across the whole range.** p99 moves from
14.60ms at 2,000 to 14.33ms at 12,000 -- a 6x increase in load with no tail
degradation at all, and marginally better at the top as connection pools stay
warm.

**The service was never saturated.** `platform-shed` is zero on every rung
including 16,000, where the service admitted 479,961 of the 479,984 requests
that reached it. The 16,000 failure is the generator: 74,450 shed, p50 blown
out to 601ms by its own queueing. The service's true ceiling is therefore
somewhere above 15,500 RPS and remains unmeasured -- finding it needs more
than one load generator.

This is why `PER_REPLICA_RPS` is set to 600 and used undiscounted: 600 is a
proven-healthy figure, not an extrapolation from a collapse point.

### 1 vCPU per replica, n2-standard-4 generator (2026-09-13)

The original configuration, kept for contrast.

| Offered | Achieved | client p50 | client p99 | server p50 | 200s | 429s | Failed | Client-shed | Verdict |
|---:|---:|---:|---:|---:|---:|---:|---:|---:|:--|
| 2,000 | 1,999 | 8.83 ms | 36.99 ms | 14 us | 59,998 | 0 | 0 | 0 | VALID |
| 2,000 (rerun) | 1,999 | 9.30 ms | 16.64 ms | 16 us | 59,998 | 0 | 0 | 0 | VALID |
| 4,000 | 3,990 | 21.60 ms | 3,549 ms | 15 us | 88,470 | 30,975 | 551 | 0 | VALID (false pass) |
| 6,000 | 4,838 | 7,172 ms | 10,920 ms | 14 us | 20,411 | 2,550 | 58,407 | 109,858 | INVALID |
| 8,000 | 666 | 13,279 ms | 19,497 ms | 14 us | 7,153 | 1,980 | 37,763 | 220,541 | INVALID |
| 12,000 | 10,093 | 7,258 ms | 15,254 ms | 15 us | 4,990 | 52 | 61,059 | 355,431 | INVALID |

**The usable ceiling was about 2,000 RPS**, six times lower than after the
change to 4 vCPU and a larger generator.

Both ends were undersized, and the sequence of fixes matters more than either
number. Raising the service from 1 to 4 vCPU eliminated platform shedding
entirely -- `platform-shed` went from 30,975 at 4,000 RPS to zero -- but the
runs still failed, now with tens of thousands of requests shed *client*-side.
Only after replacing the n2-standard-4 generator with a c3d-highcpu-16 did the
ladder come back clean.

Without the `platform-shed` / `client-shed` split, both configurations would
have reported "4,000 RPS, INVALID" identically, with nothing to indicate that
the first fix had worked or which end to fix next.

### The limiter was never the bottleneck

Server-side handler latency held at **14-17 microseconds across every single
rung**, including those where clients observed 13-second latencies. Nothing
that broke was the rate-limiting logic; it was queueing, connections and
platform capacity around it.

This is the clearest argument for the two-ended measurement rule. A
client-only benchmark would have reported this system degrading catastrophically
past 4,000 RPS and implied the limiter was at fault. The server-side figure
shows the limiter answering in microseconds the entire time.

## Experiments

### E1 — Strategy comparison

2,000 RPS offered against a 1,200/sec limit, 8 replicas, 3 runs each.

Sized from the capacity table above: 8 replicas carry ~2,800 RPS with headroom,
so this sits at roughly 70% of capacity. The limit is below the offered rate on
purpose, or nothing is ever denied and the enforcement column measures nothing.

| Strategy | p50 | p99 | p999 | Sustained over-admission | Redis ops/sec |
|---|---|---|---|---|---|
| `centralized` | — | — | — | — | — |
| `slidingwindow` | — | — | — | — | — |
| `localsync` (100ms) | — | — | — | — | — |

### E2 — Enforcement error vs sync interval and replica count

`localsync` at 600 RPS against a 400/sec limit, sweeping sync interval
`{20ms … 1s}` across replica counts `{2, 4, 8, 16}`.

The rate is set by the *smallest* replica count, not the largest. Load must be
constant across rungs for the comparison to hold, and 2 replicas carry only
~700 RPS — offering more would saturate the low-replica rungs, and the platform
shedding that followed would be reported as over-admission. That would produce
a curve resembling the predicted one while actually measuring Cloud Run running
out of capacity.

600 RPS is modest, but this experiment reports an error percentage rather than
throughput, and keeping `R=2` buys a 15× spread in `(R−1)` — which is what the
bound scales with. E1 carries the throughput story.

**Predict before measuring.** The bound in [design.md](design.md#localsync)
says excess per interval is at most `(R − 1) × A`. Write down the expected
curve first, then plot measured against predicted. If they disagree, finding
out why is the most valuable thing that can happen in this project — far more
so than a curve that matches.

**Run 2026-09-14.** 72 runs, 3 per cell. All VALID; **zero platform shedding
across every run**, so the surface measures sync drift and not Cloud Run
saturation.

| R \ sync | 20ms | 50ms | 100ms | 250ms | 500ms | 1s |
|---:|---:|---:|---:|---:|---:|---:|
| **2** | 1.54% | 2.81% | 7.22% | 18.04% | 32.29% | 39.26% |
| **4** | 1.99% | 5.54% | 9.70% | 26.78% | 37.96% | 31.11% |
| **8** | 2.65% | 6.32% | 12.48% | 31.16% | 40.92% | 42.83% |
| **16** | 2.68% | 6.30% | 12.64% | 31.10% | 41.59% | **42.86%** |

#### The predicted bound does not govern

If error scaled with `(R−1)`, dividing each cell by `(R−1)` would flatten the
columns. It does not: that ratio falls roughly eightfold from R=2 to R=16.

| R \ sync | 20ms | 50ms | 100ms | 250ms | 500ms | 1s |
|---:|---:|---:|---:|---:|---:|---:|
| 2 | 1.54 | 2.81 | 7.22 | 18.04 | 32.29 | 39.26 |
| 4 | 0.66 | 1.85 | 3.23 | 8.93 | 12.65 | 10.37 |
| 8 | 0.38 | 0.90 | 1.78 | 4.45 | 5.85 | 6.12 |
| 16 | 0.18 | 0.42 | 0.84 | 2.07 | 2.77 | 2.86 |

`(R−1) × A` is a legitimate upper bound but a badly loose one, because it
assumes every replica independently exhausts the full remaining budget.

**What actually happens is that error saturates in R.** R=8 and R=16 are
indistinguishable at every interval: 2.65/2.68, 6.32/6.30, 12.48/12.64,
31.16/31.10. Total offered load is fixed, so doubling the replica count halves
each replica's share of it, and the fleet's total unsynced admissions stay
roughly constant at `admission_rate × sync_interval` however many replicas
divide it. The governing variable is the interval, not the replica count.

That is a better model than the one the design predicted, and it was arrived at
by measuring rather than by reasoning.

#### The top-right of the table is censored

Offering 1,000 RPS against a 700/sec limit caps observable over-admission at
`(1000 − 700) / 700 = 42.86%`. R=16 at a 1s interval measures **42.86%** --
exactly that ceiling, and a property of the experiment rather than of the
limiter.

Everything at 250ms and above is pressed against this wall, which is also why
the run-to-run spread explodes there: up to 29.6 percentage points at R=4/1s,
against under 1 point everywhere below 100ms. **Only the 20–100ms region should
be read as measurement.** Extending the usable range needs a higher offered
rate relative to the limit, which in turn needs more replicas to stay clear of
platform saturation.

### E3 — Throughput ceiling

**Run 2026-09-14.** 16 replicas, limit held at 10× the offered rate so denials
cannot confound the latency reading.

| Offered | `centralized` server p50 | `localsync` server p50 | Verdict |
|---:|---:|---:|:--|
| 2,000 | 1,104 µs | 23 µs | VALID |
| 4,000 | 462 µs | 18 µs | VALID |
| 6,000 | 1,421 µs | 13 µs | VALID |
| 8,000 | **59,871 µs** | **11 µs** | INVALID |
| 10,000 | 54,495 µs | 13 µs | INVALID |
| 12,000 | 70,399 µs | 12 µs | INVALID |

**`centralized` knees 42× between 6,000 and 8,000 RPS; `localsync` shows no
knee at all.** This is Redis's throughput becoming the fleet's throughput,
which is the structural argument for keeping the shared store off the request
path -- now visible in the data rather than only in the design doc.

Two caveats, both of which should be stated wherever this result is quoted:

**The runs above 6,000 are INVALID on client-side gates**, so the divergence is
strongly suggestive rather than conclusive. The server-side telemetry is still
meaningful -- it reports what the service did with the requests that reached it
-- but the experiment did not cleanly isolate the service.

**The 6,000 ceiling contradicts the validation ladder**, which sustained 12,000
RPS cleanly on 20 replicas. The likely cause is warm-up: E3 redeploys before
every rung and waits only 10 seconds, whereas the ladder reused a single warm
deployment across rungs. Cold instances cannot absorb a step change to 8,000
RPS. E3 should be re-run with a longer post-deploy settle before its ceiling is
treated as a property of the system.

### E4 — Failure injection

**Attempted 2026-09-14/15. No valid results obtained.** The intended
comparison:

| Run | Expected behaviour |
|---|---|
| `centralized`, fail-closed | 503s, admissions collapse to zero; backend protected, service down |
| `centralized`, fail-open | Everything admitted, enforcement gone; service up, backend exposed |
| `localsync` | Keeps serving from local state; `sync_failures` climbs while admissions continue |

The blocker is structural rather than incidental, and worth recording because
it constrains how anyone can chaos-test managed Redis on GCP.

#### A Memorystore instance on DIRECT_PEERING cannot be partitioned from Cloud Run

Three approaches, each failing for a different and instructive reason.

**1. Deny-egress firewall rule to the Memorystore address.** No effect
whatsoever: the run completed with `errors: 0`, `sync_failures: 0`, and a
normal admitted/denied split. GCP firewall rules are connection-tracked, so the
rule blocked *new* connections while go-redis's already-established pool
carried on untouched. Blocking the door after everyone is inside.

This is the failure worth internalising, because it looks like success. The
rule was created, the API accepted it, the logs showed it applied — and
nothing happened. Had the run been marginally noisy, it could have been
mistaken for a mild outage and written up as one.

**2. Blackhole route.** A /32 for the Redis address pointed at the internet
gateway, more specific than the /29 peering route that normally reaches it.
Routes are evaluated per packet rather than per connection, so this should
have severed live connections. The API rejected it outright:

    10.204.174.227/32 hides the address space of the peer network from
    peering (redis-peer-...). Cannot change the routing of packets destined
    for the peer network.

Memorystore reaches the VPC through network peering, and GCP forbids routes
that override paths into a peered network. There is no route-level lever.

**3. Repointing REDIS_ADDR at an unroutable address.** This does produce the
condition under test — connection timeouts — and is reversible in one command.
It works, but it rolls a new Cloud Run revision, so instances restart and
`localsync` enters the outage with empty local state rather than carrying
accumulated drift into it. The availability claim would still be tested; the
drift-over-time nuance would not.

The runs failed for an unrelated reason: the orchestration stalled, so the
45-second cut landed outside the measurement window. One run recorded
`REDIS UNREACHABLE 02:45` and `RESTORING 08:41` — a six-hour "cut" whose
outage fell entirely outside the 150s window, producing a clean VALID run of a
perfectly healthy service.

#### How to actually get this result

Run Redis on a VM instead of Memorystore. `iptables -A INPUT -p tcp --dport
6379 -j DROP`, or simply stopping the process, gives a real partition that
affects established connections immediately — and costs less than Memorystore
besides. The managed service buys availability and patching, and charges for
it by removing the ability to break it on purpose.

That is a fair trade for production and the wrong one for a chaos experiment,
which is itself a reasonable observation to make in the writeup: the
properties that make a managed dependency good to depend on are the same ones
that make it hard to test your behaviour when it fails.

### E5 — Cold-start admission spike (optional)

A fresh Cloud Run replica starts with a full local bucket, so an autoscaling
event transiently inflates the fleet's total allowance.

## What surprised me

*This section is the most valuable part of this document.*

### The exact strategy over-admitted by 17.4%, and only at scale

E1's first run reported 17.44% sustained over-admission from `centralized` --
the strategy whose entire purpose is exactness. It had measured 0.00% in unit
tests, 0.00% against real Redis in integration tests, and 0.00% in a
single-replica deployment on GCP.

The Lua script clamped elapsed time at zero when a caller's clock lagged the
stored refill mark, correctly refusing to mint tokens -- and then wrote that
caller's earlier timestamp back anyway. The next caller measured elapsed from
the older mark and refilled across an interval that had already been credited.

Each alternation re-opens up to one skew's worth of refill, so the error scales
with **skew divided by the gap between consecutive requests to a key**. At 2,000
RPS that gap is about 500 microseconds, which is *smaller* than the
sub-millisecond clock skew between Cloud Run instances, so a large fraction of
every interval was credited twice. At the request rates used in every earlier
test, the gap dwarfed the skew and the bug simply did not exist.

The fix keeps the stored mark monotonic -- `max(stored, now)` -- so the bucket's
view of time only moves forward and no interval is credited twice. After it,
`centralized` measured −0.00%: 224,995 / 225,000 / 225,000 admitted against a
theoretical 225,000.

The regression test drives two replicas 2 ms apart at 500 µs intervals. Against
the unfixed script it admits 4,000 where 1,010 is the ceiling — 296% over,
every request allowed, because the bucket never advances at all.

This is the project's strongest argument for benchmarking on real
infrastructure. A correct unit suite, integration tests against real Redis, and
a clean single-replica deployment all missed it. Eight replicas under sustained
load found it in ninety seconds.

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

### The validity gate passed a run that was on fire

The 4,000 RPS rung was marked VALID. It was not.

Every gate passed: the client achieved 3,990 of 4,000 offered, shed nothing,
and failures were 0.46% -- under the 1% bar. But p99 was 3.5 seconds and 26% of
responses were 429s.

Those 429s could not have been limiter denials. The service was configured with
a 20,000/sec limit while only 4,000/sec was offered, so nothing should ever have
been denied. They were Cloud Run shedding load when its instance queues filled
-- and the generator counted platform overload as enforcement, reporting a
melting service as a clean measurement.

The gates were all asking "did the client behave?" and none asked "did the
service?". A load generator can be perfectly healthy while the thing it is
measuring falls over.

Fixed by distinguishing a limiter decision from platform shedding: only
limiterd returns a JSON body carrying `allowed`, so a 429 without one came from
the platform. Runs now report `shed_by_platform` separately and fail validation
above 1%.

### Cloud Run reserves /healthz

The readiness probe in `bench/run.sh` could never have succeeded. Google's
front end answers `/healthz` itself on Cloud Run and the request never reaches
the container. Of the seven paths tried -- `/health`, `/livez`, `/readyz`,
`/healthcheck`, `/_ah/health`, `/ping` and `/healthz` -- only `/healthz` is
intercepted.

The tell is subtle: the 404 is Google's HTML error page rather than Go's plain
`404 page not found`. The probe looped for 60 seconds and then ran the
benchmark anyway. It now fails loudly instead.

### Results survived a 16-hour hang because they were on disk

The SSH session driving the second ladder died mid-run, leaving a zombie
process that still looked alive. The runs completed on the VM; only the console
output was lost. Every result survived because `loadgen` writes JSON to
`bench/results/` rather than trusting stdout.

Had the harness been stdout-only, sixteen hours of billed infrastructure would
have produced nothing.

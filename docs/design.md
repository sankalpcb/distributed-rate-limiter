# Design: Distributed Rate Limiter

## The problem

A rate limiter enforced by a single process is trivial. Enforced across a fleet
of replicas, it becomes a distributed consensus problem in miniature, and the
usual impossibility applies: you cannot simultaneously have

- **exact** global enforcement,
- **low latency** on the request path, and
- **availability** when the shared store is unreachable.

Every real rate limiter picks two. This project implements three different
picks and measures what each one costs.

## Interface

One abstraction, three implementations:

```go
type Limiter interface {
    Allow(ctx context.Context, key string, cost int64, now time.Time) (Decision, error)
    Close() error
}
```

`now` is a parameter rather than a call to `time.Now()` inside the
implementation. This is the single most useful decision in the codebase: it
makes every strategy deterministically testable. "Does the bucket refill
correctly over ten minutes" becomes a microsecond-scale unit test instead of a
sleep, and the tests for clock skew and window rollover — the cases most likely
to hide a bug — become trivial to express.

Strategy selection is an environment variable, so a benchmark sweep is a
configuration change and never a rebuild.

## Strategies

### `centralized`

A token bucket evaluated inside Redis by a Lua script.

Refill-and-deduct must happen atomically. Splitting it into a read, a compute,
and a write would let two replicas interleave and both admit against the same
tokens — the exact bug the strategy exists to prevent. Lua gives atomicity
without a distributed lock and without a second round trip.

Two details worth noting in the script:

- **Elapsed time is clamped at zero.** Replica clocks are not aligned, and a
  request carrying a backwards timestamp must never mint tokens.
- **Refill is clamped at burst.** An hour of idling must not accumulate an
  hour's worth of allowance.

Cost: a Redis round trip on the critical path of every request, and Redis
throughput becomes a hard ceiling for the entire fleet.

### `localsync`

Each replica admits against a local counter and publishes its delta to Redis
every sync interval, adopting the fleet total in return.

Between syncs, a replica cannot see its peers' admissions, so the fleet
over-admits. Per sync interval the excess is bounded by roughly

```
excess ≤ (R − 1) × A
```

where `R` is the replica count and `A` is what one replica can admit within one
interval before reconciling. That bound is loose — it assumes every replica
independently exhausts the full remaining budget — and measuring where reality
falls inside it was the point of experiment E2.

**E2 disproved this model.** Error does not grow with `(R − 1)`; it *saturates*
in replica count. R=8 and R=16 measured the same at every sync interval
(2.65/2.68%, 6.32/6.30%, 12.48/12.64%, 31.16/31.10%). Because total offered
load is fixed, doubling the replica count halves each replica's share of it,
and the fleet's unsynced admissions stay near `admission_rate × sync_interval`
however many replicas divide it.

The bound above remains a valid upper limit and a useless predictor. The sync
interval is the governing variable; past about R=8 the replica count stops
mattering. See
[benchmarks.md](benchmarks.md#e2--enforcement-error-vs-sync-interval-and-replica-count).

Two properties follow from taking Redis off the request path:

1. **Latency is local.** No network on the admission decision.
2. **It survives the store.** When Redis is unreachable the replica keeps
   serving from local state, with enforcement drifting further out the longer
   the outage lasts. Accuracy degrades continuously rather than collapsing.

A failed sync does not discard the delta; it is republished on the next
successful sync, so an outage does not silently erase admissions that already
happened. Replicas also flush on shutdown — without that, a scale-down would
drop a terminating replica's unpublished admissions and let the fleet
over-admit for free.

### `slidingwindow`

The weighted two-fixed-window approximation:

```
estimate ≈ prev_window_count × overlap + current_window_count
```

This exists to fix the flaw that makes naive fixed windows unusable: a client
can spend a full budget at the end of one window and again at the start of the
next, admitting twice the limit across the boundary. Carrying the previous
window forward, weighted by how much of it is still inside the trailing window,
removes that.

The exact alternative — a Redis sorted set holding one timestamp per request —
gives a precise answer at O(requests) memory per key. The approximation is O(1)
and its error comes from assuming arrivals were uniform across the previous
window. For a rate limiter, bounded error at constant memory is the better
trade; the sorted set is the right choice only when the exact set of request
times is itself needed.

## Failure policy

When the shared store is unreachable there is no correct answer, only a choice
about which failure to absorb:

- **Fail-open** admits everything. Your service stays up; whatever the limiter
  was shielding is now fully exposed, which during a Redis outage is precisely
  when the backend is least able to cope.
- **Fail-closed** denies everything. The backend is protected; you have caused
  your own outage.

It is a deployment decision, not a library default, so it is configuration.
Both were intended to be measured in E4 rather than argued about -- but that
experiment could not be performed against managed Memorystore, which cannot be
partitioned from Cloud Run by firewall or by route. The reasoning below stands
as reasoning; it is not backed by measurement, and
[benchmarks.md](benchmarks.md#e4--failure-injection) says so explicitly.

`localsync` largely sidesteps the question: Redis is not on its request path,
so an outage degrades accuracy instead of availability.

## Measurement

The benchmark is the deliverable, so its correctness matters as much as the
service's. Methodology is documented in [benchmarks.md](benchmarks.md); the two
decisions that do the most work:

**Open-loop load generation.** A closed-loop generator — N workers each looping
"send, await reply, send again" — stops issuing requests while the server is
slow, so the stalls being measured are never sampled. That is coordinated
omission, and it makes tail latency look far better than it is. The generator
here schedules against wall-clock time and measures latency from each request's
*intended* send time, so client-side delay counts against the result.

**The generator validates itself.** A run reports INVALID if the client shed
requests or failed to achieve its offered rate. The most common way a cloud
benchmark produces confident nonsense is that the load generator saturated
first, and nothing in the output said so.

## Cost

The design target is under $50 of free credits, which constrains the
architecture rather than decorating it: everything scales to zero or is
destroyed between sessions.

Guardrails, in order of how much they actually protect:

1. **`max_instances` on Cloud Run.** The platform default ceiling is 100. A
   load generator bug against that ceiling is the one plausible way this
   project spends real money. Capped at 20 in Terraform.
2. **Terraform.** The value is not the IaC line on a resume; it is that
   `terraform destroy` is reliable. Manual teardown is how a Memorystore
   instance survives forgotten for three weeks.
3. **Short runs.** Cloud Run cost is dominated by request count. 90 seconds at
   5k RPS gives ample steady-state samples; five minutes costs three times as
   much for no additional statistical power.
4. **Budget alerts.** Useful, but they notify rather than cap. GCP has no hard
   spend limit short of wiring a budget through Pub/Sub to a function that
   disables billing — rejected here as fragile and capable of killing a project
   mid-run.

## Alternatives rejected

**Multi-region failover router.** Deploy a trivial service to three regions
behind a global load balancer, break one, measure failover. Fast to build and
on-theme, but it is almost entirely configuration — the repository would be
thin and it reads as an experiment rather than a system. The global forwarding
rule also bills continuously whether or not traffic flows. Retained as a
possible extension to this project rather than a replacement for it.

**Pub/Sub work queue with backpressure and dead-lettering.** Cheap and good
interview material on delivery semantics, but close enough to a standard
tutorial that it would differentiate nothing.

**Sharded consistent-hashed cache with a control plane.** Genuinely
interesting, but membership and discovery push it onto GKE — a cluster idling
against the credit budget, and roughly a month of work.

**Exact sliding window via sorted sets.** Rejected on memory grounds; see
`slidingwindow` above.

**gRPC transport.** A day of plumbing that would change no result. HTTP/JSON
is sufficient to measure what this project measures.

**A leased-token variant of `localsync`.** Each replica reserves K tokens from
an authoritative pool and serves locally from the lease. It never over-admits,
which sounds strictly better — but it *under*-admits and distributes unfairly:
a replica holding an unused lease starves its peers while the global pool
reads as exhausted. The delta-sync variant was chosen instead because its error
has a single clear direction and scales visibly with two knobs, which makes it
measurable. The leased variant is the better production design and the worse
experiment.

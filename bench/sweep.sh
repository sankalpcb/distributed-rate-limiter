#!/usr/bin/env bash
#
# sweep.sh -- the experiment suite from docs/design.md section 6.
#
# Usage, from the loadgen VM:
#   ./bench/sweep.sh validate   # day-4 gate: is the generator fast enough?
#   ./bench/sweep.sh e1         # results table
#   ./bench/sweep.sh e2         # enforcement error vs sync interval x replicas
#   ./bench/sweep.sh e3         # throughput ceiling
#   ./bench/sweep.sh e4         # failure injection (needs manual Redis kill)
#   ./bench/sweep.sh all        # e1 e2 e3
set -euo pipefail

cd "$(dirname "$0")/.."

REPEATS=${REPEATS:-3}   # methodology: three runs per configuration, report spread

# Capacity model, derived from the validation ladder rather than guessed.
#
# Measured 2026-09-13 (docs/benchmarks.md): 4 replicas of 1 vCPU sustained
# 2,000 RPS cleanly, and by 4,000 RPS Cloud Run was shedding 26% of requests
# with a p99 of 3.5 seconds. That is roughly 500 RPS per replica at the point
# of collapse, so planning uses 350 to leave headroom -- a benchmark run at the
# edge of capacity measures the platform, not the limiter.
#
# Re-run `./bench/sweep.sh validate` and update this if the instance size,
# CPU allocation, or region changes. Every rate below is derived from it.
PER_REPLICA_RPS=${PER_REPLICA_RPS:-350}

# Cloud Run's max_instances is capped at 20 in infra/main.tf as a cost
# guardrail, so no sweep may ask for more than that without raising
# var.max_instances first.
MAX_REPLICAS=20

log() { printf '\n\033[1;34m### %s\033[0m\n' "$*" >&2; }

# Release the pinned replicas on the way out, however we leave.
#
# run.sh sets min-instances == max-instances to hold the fleet size steady for
# a measurement, and nothing puts it back. Those instances then bill around the
# clock: at 4 vCPU, twenty pinned replicas is eighty vCPU running whether or not
# any request arrives. A sweep that is interrupted -- Ctrl-C, a dropped SSH
# session, a failed rung -- would otherwise leave them up indefinitely, which is
# exactly how this project already lost sixteen hours of idle spend once.
#
# EXIT covers normal completion and errors; INT and TERM cover the interruptions.
scale_down() {
  local status=$?
  echo >&2
  log "releasing pinned replicas (min-instances -> 0)"
  gcloud run services update "${SERVICE:-limiterd}" \
    --region "${REGION:-us-central1}" \
    --min-instances 0 \
    --quiet >/dev/null 2>&1 \
    && echo "    done -- the service now scales to zero" >&2 \
    || echo "    WARNING: scale-down failed. Run it by hand or the fleet keeps billing." >&2
  exit $status
}
trap scale_down EXIT INT TERM

# capacity_for R -> the offered rate this replica count can carry with headroom.
capacity_for() { echo $(( $1 * PER_REPLICA_RPS )); }

# guard_replicas refuses a sweep that would exceed the instance cap, rather
# than letting Cloud Run silently serve fewer replicas than the run claims --
# which would misattribute the resulting over-admission to the sync interval.
guard_replicas() {
  local r
  for r in "$@"; do
    if (( r > MAX_REPLICAS )); then
      echo "ERROR: replica count $r exceeds max_instances=$MAX_REPLICAS in infra/main.tf." >&2
      echo "       Raise var.max_instances and re-apply, or lower the sweep." >&2
      exit 1
    fi
  done
}

# validate is the gate described in docs/design.md section 12: confirm the load
# generator can actually produce the offered rate before trusting any result.
# Runs against /health semantics -- a limit high enough that nothing is denied,
# so the only thing under test is whether the client keeps up.
validate() {
  # Run against the MAXIMUM replica count, not a typical one.
  #
  # The first version of this ladder used 4 replicas, and its results were
  # ambiguous exactly because of that: when a rung failed there was no way to
  # tell whether the generator had run out of capacity or the service had. A
  # client-capacity probe has to remove the service as a candidate bottleneck,
  # which means giving it every replica available.
  #
  # localsync keeps Redis off the request path, so Memorystore is not in the
  # picture either. The limit is 10x the offered rate so nothing is ever
  # denied. What remains under test is the client.
  local replicas=$MAX_REPLICAS
  guard_replicas $replicas

  log "VALIDATION: can the generator sustain its offered rate?"
  log "    ${replicas} replicas, so a failure points at the client, not the service"

  # Deploy once, then reuse it. A Cloud Run redeploy between rungs costs ~90s
  # and changes nothing being measured.
  local first=1
  for rps in ${VALIDATE_RATES:-2000 4000 6000 8000 12000}; do
    log "offering ${rps} RPS"
    OFFERED=$rps LIMIT=$((rps * 10)) BURST=$((rps * 10)) \
      STRATEGY=localsync REPLICAS=$replicas DURATION=30s WARMUP=10s \
      SKIP_DEPLOY=$([[ $first == 1 ]] && echo 0 || echo 1) \
      LABEL="validate-${rps}rps" ./bench/run.sh || true
    first=0
  done
  echo
  echo "The highest rung still reporting VALID is the client's ceiling."
  echo
  echo "Check 'shed_by_platform' on the failing rungs. If it is high, the"
  echo "service ran out of capacity before the client did, and this ladder has"
  echo "not found the client's limit -- raise max_instances and re-run."
  echo "Otherwise the client is the constraint: use a larger loadgen VM"
  echo "(machine_type in infra/variables.tf) before trusting any later number."
}

# E1: the results table. All three strategies, one load, one replica count.
#
# 8 replicas carry ~2,800 RPS with headroom, so 2,000 offered sits at about 70%
# of capacity. The limit is deliberately below the offered rate -- otherwise
# nothing is ever denied and the enforcement column has nothing to measure.
e1() {
  local replicas=8
  guard_replicas $replicas
  local offered=2000
  local limit=1200
  local cap; cap=$(capacity_for $replicas)

  log "E1: strategy comparison, ${offered} RPS offered / ${limit} limit, ${replicas} replicas (capacity ~${cap})"
  for strategy in centralized slidingwindow localsync; do
    for run in $(seq 1 "$REPEATS"); do
      STRATEGY=$strategy REPLICAS=$replicas OFFERED=$offered LIMIT=$limit BURST=$limit \
        SYNC_INTERVAL=100ms DURATION=90s WARMUP=15s \
        LABEL="e1-${strategy}-run${run}" ./bench/run.sh
    done
  done
}

# E2: the money chart. Over-admission as a function of sync interval and
# replica count -- the two knobs the bound in docs/design.md predicts.
#
# The offered rate is set by the SMALLEST replica count in the sweep, not the
# largest. This is the constraint that is easy to miss: the load has to be
# constant across rungs for the comparison to mean anything, and 2 replicas
# carry only ~700 RPS. Offering more would overload the low-replica rungs and
# leave Cloud Run shedding requests -- which the run would then report as
# over-admission, producing a curve that looks like the predicted one and is
# actually measuring platform saturation.
#
# 600 RPS is modest, but this experiment measures an error percentage, not
# throughput. Keeping R=2 buys a 15x spread in (R-1), which is what the
# predicted bound scales with, and that matters far more to the chart than the
# absolute request rate does. E1 carries the throughput story.
e2() {
  local replicas_sweep="2 4 8 16"
  # shellcheck disable=SC2086
  guard_replicas $replicas_sweep

  local smallest=2
  local offered=600
  local limit=400

  log "E2: localsync over-admission vs sync interval x replica count"
  log "    ${offered} RPS offered / ${limit} limit -- bounded by R=${smallest} (capacity ~$(capacity_for $smallest))"
  for replicas in $replicas_sweep; do
    for interval in 20ms 50ms 100ms 250ms 500ms 1s; do
      for run in $(seq 1 "$REPEATS"); do
        STRATEGY=localsync REPLICAS=$replicas SYNC_INTERVAL=$interval \
          OFFERED=$offered LIMIT=$limit BURST=$limit DURATION=60s WARMUP=15s \
          LABEL="e2-r${replicas}-sync${interval}-run${run}" ./bench/run.sh
      done
    done
  done
}

# E3: where it breaks. Ramp until p99 knees over.
#
# This is the one experiment that is SUPPOSED to end in INVALID runs -- finding
# the ceiling means crossing it. The steps bracket the measured collapse point
# rather than starting far beyond it: the old ladder opened at 2,000 and jumped
# to 30,000, which told you only that everything above the first rung was
# broken. Finer steps near the knee are what produce a capacity number.
#
# The limit is held well above the offered rate throughout, so denials cannot
# confound the latency reading; what is being measured is where the system
# stops keeping up, not where the limiter starts saying no.
e3() {
  local replicas=16
  guard_replicas $replicas
  local cap; cap=$(capacity_for $replicas)

  log "E3: throughput ceiling, ${replicas} replicas (planning capacity ~${cap})"
  for strategy in centralized localsync; do
    for rps in 1000 2000 3000 4000 5000 6000 8000; do
      STRATEGY=$strategy REPLICAS=$replicas OFFERED=$rps \
        LIMIT=$((rps * 10)) BURST=$((rps * 10)) \
        DURATION=60s WARMUP=15s \
        LABEL="e3-${strategy}-${rps}rps" ./bench/run.sh || {
          echo "run failed outright at ${rps} RPS for ${strategy}; treating as the ceiling" >&2
          break
        }
    done
    echo "For ${strategy}: the ceiling is the highest rung still reporting VALID." >&2
  done
}

# E4 is deliberately manual: it requires killing Redis partway through a run,
# and scripting that around Memorystore is more fragile than doing it by hand.
e4() {
  cat <<'GUIDE'
E4 -- failure injection. Run each of these, and partway through each run
(around 30s in) remove the service's access to Redis:

    gcloud redis instances update <INSTANCE> --region <REGION> ...
  or, more reliably and reversibly, drop the firewall rule the service uses:
    gcloud compute firewall-rules update allow-redis --disabled

Then re-enable it and watch recovery. Three runs to capture:

  1. centralized, FAIL_MODE=closed
     Expect: 503s, admitted collapses to zero. The backend is protected and
     the service is down. Graph the error rate.

  2. centralized, FAIL_MODE=open
     Expect: everything admitted, no enforcement at all. The service stays up
     and whatever it was shielding is now fully exposed.

  3. localsync (fail mode is irrelevant -- Redis is off the request path)
     Expect: requests keep being served from local state. Watch
     sync_failures climb in /stats while admissions continue, with
     enforcement drifting further out the longer the outage lasts.

Commands:

  E4="REPLICAS=8 OFFERED=2000 LIMIT=1200 BURST=1200 DURATION=120s"

  env $E4 STRATEGY=centralized FAIL_MODE=closed LABEL=e4-central-closed ./bench/run.sh
  env $E4 STRATEGY=centralized FAIL_MODE=open   LABEL=e4-central-open   ./bench/run.sh
  env $E4 STRATEGY=localsync                    LABEL=e4-localsync      ./bench/run.sh

These match E1's load so the failure behaviour is comparable against the
healthy baseline. Anything heavier would have Cloud Run shedding requests
before Redis is even touched, which would muddy the very contrast the
experiment exists to show.

The contrast between run 3 and runs 1-2 is the strongest result in the
project: localsync is not merely faster, it is available when Redis is not,
and the price is bounded accuracy loss.
GUIDE
}

case "${1:-all}" in
  validate) validate ;;
  e1) e1 ;;
  e2) e2 ;;
  e3) e3 ;;
  e4) e4 ;;
  all) e1; e2; e3 ;;
  *) echo "usage: $0 {validate|e1|e2|e3|e4|all}" >&2; exit 2 ;;
esac

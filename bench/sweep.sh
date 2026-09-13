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

log() { printf '\n\033[1;34m### %s\033[0m\n' "$*" >&2; }

# validate is the gate described in docs/design.md section 12: confirm the load
# generator can actually produce the offered rate before trusting any result.
# Runs against /health semantics -- a limit high enough that nothing is denied,
# so the only thing under test is whether the client keeps up.
validate() {
  log "VALIDATION: can the generator sustain its offered rate?"
  for rps in 2000 5000 10000 20000; do
    log "offering ${rps} RPS"
    OFFERED=$rps LIMIT=$((rps * 10)) BURST=$((rps * 10)) \
      STRATEGY=localsync REPLICAS=4 DURATION=30s WARMUP=10s \
      LABEL="validate-${rps}rps" ./bench/run.sh || true
  done
  echo
  echo "Read the 'achieved' line and the verdict for each run. The highest rate"
  echo "that still reports VALID is the ceiling for every later experiment."
  echo "If that ceiling is below your intended load, use a bigger loadgen VM"
  echo "before going further -- every subsequent number depends on it."
}

# E1: the results table. All three strategies, one load, one replica count.
e1() {
  log "E1: strategy comparison at 8000 RPS offered / 5000 limit, 4 replicas"
  for strategy in centralized slidingwindow localsync; do
    for run in $(seq 1 "$REPEATS"); do
      STRATEGY=$strategy REPLICAS=4 OFFERED=8000 LIMIT=5000 BURST=5000 \
        SYNC_INTERVAL=100ms DURATION=90s WARMUP=15s \
        LABEL="e1-${strategy}-run${run}" ./bench/run.sh
    done
  done
}

# E2: the money chart. Over-admission as a function of sync interval and
# replica count -- the two knobs the bound in docs/design.md predicts.
e2() {
  log "E2: localsync over-admission vs sync interval x replica count"
  for replicas in 2 4 8 16; do
    for interval in 20ms 50ms 100ms 250ms 500ms 1s; do
      for run in $(seq 1 "$REPEATS"); do
        STRATEGY=localsync REPLICAS=$replicas SYNC_INTERVAL=$interval \
          OFFERED=8000 LIMIT=5000 BURST=5000 DURATION=60s WARMUP=15s \
          LABEL="e2-r${replicas}-sync${interval}-run${run}" ./bench/run.sh
      done
    done
  done
}

# E3: where it breaks. Ramp until the centralized strategy's p99 knees over.
e3() {
  log "E3: throughput ceiling"
  for strategy in centralized localsync; do
    for rps in 2000 5000 10000 15000 20000 30000; do
      STRATEGY=$strategy REPLICAS=8 OFFERED=$rps LIMIT=$((rps * 2)) BURST=$((rps * 2)) \
        DURATION=60s WARMUP=15s \
        LABEL="e3-${strategy}-${rps}rps" ./bench/run.sh || {
          echo "run failed at ${rps} RPS for ${strategy}; treating as the ceiling" >&2
          break
        }
    done
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

  STRATEGY=centralized FAIL_MODE=closed DURATION=120s LABEL=e4-central-closed ./bench/run.sh
  STRATEGY=centralized FAIL_MODE=open   DURATION=120s LABEL=e4-central-open   ./bench/run.sh
  STRATEGY=localsync                    DURATION=120s LABEL=e4-localsync      ./bench/run.sh

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

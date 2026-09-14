#!/usr/bin/env bash
#
# e4.sh -- failure injection, automated.
#
# Cuts the service off from Redis partway through a run and restores it, so the
# three configurations are directly comparable. Doing this by hand produces
# outages of differing lengths at differing offsets, which makes the runs
# incomparable in exactly the dimension being measured.
#
# The cut is a deny-egress firewall rule to the Memorystore address. That is
# reversible in seconds and leaves the instance untouched -- unlike deleting or
# reconfiguring Memorystore, which takes minutes and destroys the state whose
# loss is the thing under test.
#
# Usage, from the loadgen VM:
#   ./bench/e4.sh
set -euo pipefail

cd "$(dirname "$0")/.."

SERVICE=${SERVICE:-limiterd}
REGION=${REGION:-us-central1}
NETWORK=${NETWORK:-drl-net}
RULE=${RULE:-drl-deny-redis}

REPLICAS=${REPLICAS:-8}
OFFERED=${OFFERED:-4000}
LIMIT=${LIMIT:-2500}
DURATION=${DURATION:-120s}
WARMUP=${WARMUP:-15s}

# When to cut Redis, measured from the start of the measurement window, and for
# how long. 45s in on a 120s run leaves a healthy baseline before the cut and
# enough afterwards to observe recovery.
CUT_AFTER=${CUT_AFTER:-60}
CUT_FOR=${CUT_FOR:-40}

log() { printf '\n\033[1;35m### %s\033[0m\n' "$*" >&2; }

REDIS_IP=$(gcloud redis instances describe drl-redis --region "$REGION" --format='value(host)')
[[ -n "$REDIS_IP" ]] || { echo "could not resolve Memorystore address" >&2; exit 1; }
log "Memorystore at ${REDIS_IP}"

# Always remove the rule on the way out. Leaving it in place would silently
# break every subsequent experiment, and the symptom -- a strategy that cannot
# reach Redis -- looks like a code bug rather than leftover state.
cleanup_rule() {
  gcloud compute firewall-rules delete "$RULE" --quiet >/dev/null 2>&1 || true
}
trap cleanup_rule EXIT INT TERM

cut_redis() {
  log "CUTTING Redis egress for ${CUT_FOR}s"
  gcloud compute firewall-rules create "$RULE" \
    --network "$NETWORK" \
    --direction EGRESS \
    --action DENY \
    --rules tcp:6379 \
    --destination-ranges "${REDIS_IP}/32" \
    --priority 100 \
    --quiet >/dev/null
  sleep "$CUT_FOR"
  log "RESTORING Redis egress"
  gcloud compute firewall-rules delete "$RULE" --quiet >/dev/null
}

one_run() {
  local strategy=$1 failmode=$2 label=$3

  log "E4 ${label}: strategy=${strategy} fail_mode=${failmode}"
  STRATEGY=$strategy FAIL_MODE=$failmode REPLICAS=$REPLICAS \
    OFFERED=$OFFERED LIMIT=$LIMIT BURST=$LIMIT \
    DURATION=$DURATION WARMUP=$WARMUP LABEL="$label" \
    ./bench/run.sh &
  local runpid=$!

  # Let the warmup pass and a healthy baseline establish before cutting.
  sleep "$CUT_AFTER"
  cut_redis

  wait $runpid
}

one_run centralized closed e4-central-closed
one_run centralized open   e4-central-open
one_run localsync   closed e4-localsync

log "E4 complete"
cat >&2 <<'NOTES'

What to look for in bench/results/:

  e4-central-closed  admitted collapses to zero and 503s spike. The backend is
                     protected; the service is down. Note that loadgen counts
                     these as `failed`, not as denials -- limiterd returns 503
                     with an error body rather than a limiter verdict.

  e4-central-open    everything is admitted and enforcement disappears entirely.
                     The service stays up and whatever it was shielding is now
                     fully exposed -- during a Redis outage, precisely when that
                     backend is least able to cope.

  e4-localsync       admissions continue throughout. sync_failures climbs in the
                     server stats while the request path is unaffected, and
                     enforcement drifts further out the longer the cut lasts.

The contrast between the third run and the first two is the project's central
claim: localsync is not merely faster, it is available when Redis is not, and
the price is bounded accuracy loss rather than an outage.
NOTES

#!/usr/bin/env bash
#
# run.sh -- deploy one configuration and take one measurement.
#
# Run this FROM THE LOADGEN VM. Latency measured from a laptop is dominated by
# the path to the internet, not by the limiter, and the numbers are not
# comparable between runs. The script warns if it cannot confirm it is on GCE.
#
# Every knob is an environment variable so sweep.sh can drive it in a loop.
set -euo pipefail

STRATEGY=${STRATEGY:-centralized}
REPLICAS=${REPLICAS:-4}

# Two different rates, easily confused:
#   OFFERED -- what the generator sends, requests/sec.
#   LIMIT   -- what the limiter permits, per key per second.
# Interesting runs have OFFERED > LIMIT; otherwise nothing is ever denied and
# the enforcement measurement has nothing to measure.
OFFERED=${OFFERED:-8000}
LIMIT=${LIMIT:-5000}
BURST=${BURST:-5000}
WINDOW=${WINDOW:-1s}
SYNC_INTERVAL=${SYNC_INTERVAL:-100ms}
FAIL_MODE=${FAIL_MODE:-closed}
KEYS=${KEYS:-1}
DURATION=${DURATION:-90s}
WARMUP=${WARMUP:-15s}
MAX_INFLIGHT=${MAX_INFLIGHT:-20000}

SERVICE=${SERVICE:-limiterd}
REGION=${REGION:-us-central1}
RESULTS_DIR=${RESULTS_DIR:-bench/results}

LABEL=${LABEL:-"${STRATEGY}-r${REPLICAS}-${OFFERED}rps-sync${SYNC_INTERVAL}"}
STAMP=$(date -u +%Y%m%dT%H%M%SZ)
OUT="${RESULTS_DIR}/${STAMP}-${LABEL}.json"

log() { printf '\033[1m==>\033[0m %s\n' "$*" >&2; }

if ! curl -sf -m 1 -H 'Metadata-Flavor: Google' \
     http://metadata.google.internal/computeMetadata/v1/instance/zone >/dev/null 2>&1; then
  log "WARNING: not running on a GCE instance."
  log "         Cross-region or home-network latency will swamp the measurement."
  log "         Set BENCH_ALLOW_OFFSITE=1 to proceed anyway."
  [[ "${BENCH_ALLOW_OFFSITE:-0}" == "1" ]] || exit 1
fi

mkdir -p "$RESULTS_DIR"

if [[ "${SKIP_DEPLOY:-0}" == "1" ]]; then
  log "SKIP_DEPLOY=1: reusing the running configuration"
else
log "deploying ${SERVICE}: strategy=${STRATEGY} replicas=${REPLICAS} sync=${SYNC_INTERVAL} fail=${FAIL_MODE}"
# min == max pins the replica count for the duration of the run. Without this,
# autoscaling changes the fleet size mid-measurement and the enforcement error
# becomes a function of something you did not control.
gcloud run services update "$SERVICE" \
  --region "$REGION" \
  --min-instances "$REPLICAS" \
  --max-instances "$REPLICAS" \
  --update-env-vars "LIMITER_STRATEGY=${STRATEGY},RATE=${LIMIT},BURST=${BURST},WINDOW=${WINDOW},SYNC_INTERVAL=${SYNC_INTERVAL},FAIL_MODE=${FAIL_MODE}" \
  --quiet >/dev/null
fi

URL=$(gcloud run services describe "$SERVICE" --region "$REGION" --format='value(status.url)')
log "service at ${URL}"

# Wait for the new revision to actually serve before generating load, otherwise
# the warmup measures a rollout.
#
# Note /health, not /healthz: Google's front end reserves /healthz on Cloud Run
# and returns its own 404 without ever reaching the container, so a readiness
# probe against it can never succeed.
ready=0
for _ in $(seq 1 30); do
  if curl -sf -m 2 "${URL}/health" >/dev/null; then ready=1; break; fi
  sleep 2
done
if [[ "$ready" != "1" ]]; then
  # Failing loudly matters here. Falling through to the measurement would
  # produce a plausible-looking result for a service that never came up.
  log "ERROR: ${SERVICE} did not become ready within 60s"
  exit 1
fi

log "letting ${REPLICAS} replicas spin up"
sleep 10

curl -sf -X POST "${URL}/stats/reset" >/dev/null || true

log "measuring: ${OFFERED} RPS offered against a limit of ${LIMIT}, ${DURATION} after ${WARMUP} warmup"
./bin/loadgen \
  -target "$URL" \
  -rate "$OFFERED" \
  -limit "$LIMIT" \
  -keys "$KEYS" \
  -duration "$DURATION" \
  -warmup "$WARMUP" \
  -max-inflight "$MAX_INFLIGHT" \
  -label "$LABEL" \
  -out "$OUT"

# Server-side view, recorded alongside the client view. The gap between them is
# network plus platform overhead; the server figure isolates the limiter itself.
log "server-side stats"
curl -sf "${URL}/stats" | tee "${OUT%.json}-server.json"
echo

log "wrote ${OUT}"

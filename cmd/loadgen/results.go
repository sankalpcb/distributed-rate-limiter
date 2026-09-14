package main

import (
	"fmt"
	"io"
	"math"
	"sort"
	"time"

	hdr "github.com/HdrHistogram/hdrhistogram-go"
)

// Results is the full record of a run. It is written verbatim to JSON so a
// result can be re-examined later without rerunning the benchmark.
type Results struct {
	Label  string            `json:"label,omitempty"`
	Config map[string]string `json:"config"`

	OfferedRPS  float64 `json:"offered_rps"`
	AchievedRPS float64 `json:"achieved_rps"`
	Issued      int64   `json:"issued"`
	Completed   int64   `json:"completed"`
	Dropped     int64   `json:"dropped_client_shed"`
	WallSeconds float64 `json:"wall_seconds"`

	Admitted int64 `json:"admitted"`
	Denied   int64 `json:"denied"`
	Failed   int64 `json:"failed"`
	// ShedByPlatform counts requests rejected by Cloud Run before reaching
	// limiterd -- its own 429 when an instance queue fills. These are NOT
	// limiter denials, and a run with many of them is measuring platform
	// capacity rather than enforcement.
	ShedByPlatform int64 `json:"shed_by_platform"`

	// Client-observed latency, measured from intended send time.
	P50MS  float64 `json:"client_p50_ms"`
	P99MS  float64 `json:"client_p99_ms"`
	P999MS float64 `json:"client_p999_ms"`
	MaxMS  float64 `json:"client_max_ms"`

	// Enforcement accuracy. Two views, because a token bucket and a window
	// limiter do not promise the same thing:
	//
	//   Sustained -- total admitted against rate x duration. This is the
	//   cross-strategy headline. Burst allowance amortises away over a long
	//   run, so all three strategies are held to the same promise.
	//
	//   PerSecond -- worst and mean single-second excess. Useful for showing
	//   burstiness, but a token bucket is EXPECTED to exceed the per-second
	//   limit by up to its burst, so this number must never be used to
	//   compare centralized against the window strategies.
	EnforcementMeasured       bool    `json:"enforcement_measured"`
	LimitPerKeyPerSecond      float64 `json:"limit_per_key_per_second,omitempty"`
	SustainedOverAdmissionPct float64 `json:"sustained_over_admission_pct"`
	MeanOverAdmissionPct      float64 `json:"per_second_mean_over_admission_pct"`
	MaxOverAdmissionPct       float64 `json:"per_second_max_over_admission_pct"`
	SecondsOverLimit          int     `json:"seconds_over_limit"`
	SecondsMeasured           int     `json:"seconds_measured"`

	StatusCounts map[int]int64 `json:"status_counts"`

	// Valid is false when the run cannot be trusted -- see Warnings.
	Valid    bool     `json:"valid"`
	Warnings []string `json:"warnings,omitempty"`

	hist *hdr.Histogram
	// admittedPerSecond[second][keyIdx]
	admittedPerSecond map[int64]map[int]int64
	start             time.Time
}

// record consumes samples on a single goroutine. Centralising this avoids
// locking the histogram on every request, which at several thousand RPS would
// itself distort the measurement.
func record(samples <-chan sample, o options) *Results {
	r := &Results{
		Label:             o.label,
		hist:              hdr.New(1, 60_000_000, 3), // microseconds, up to 60s
		admittedPerSecond: make(map[int64]map[int]int64),
		StatusCounts:      make(map[int]int64),
	}

	for s := range samples {
		if s.warmup {
			continue // discarded: cold caches and empty connection pools
		}
		r.Completed++
		_ = r.hist.RecordValue(s.latency.Microseconds())
		if s.status != 0 {
			r.StatusCounts[s.status]++
		}
		switch {
		case s.failed:
			r.Failed++
		case s.shedByLB:
			r.ShedByPlatform++
		case s.admitted:
			r.Admitted++
			sec := s.intended.Unix()
			if _, ok := r.admittedPerSecond[sec]; !ok {
				r.admittedPerSecond[sec] = make(map[int]int64)
			}
			r.admittedPerSecond[sec][s.keyIdx]++
		default:
			r.Denied++
		}
	}
	return r
}

// finalize derives the reported figures and decides whether the run is usable.
func (r *Results) finalize(o options) {
	if r.WallSeconds > 0 {
		r.AchievedRPS = float64(r.Issued) / r.WallSeconds
	}
	r.P50MS = usToMS(r.hist.ValueAtQuantile(50))
	r.P99MS = usToMS(r.hist.ValueAtQuantile(99))
	r.P999MS = usToMS(r.hist.ValueAtQuantile(99.9))
	r.MaxMS = usToMS(r.hist.Max())

	if o.limit > 0 {
		r.computeEnforcement(o.limit)
	}
	r.validate(o)
}

// computeEnforcement compares admissions against the configured limit, one key
// and one second at a time.
//
// The first and last measured seconds are dropped: they are partial windows,
// and a partial window always looks under-admitted, which would drag the mean
// down for no real reason.
func (r *Results) computeEnforcement(limit float64) {
	r.EnforcementMeasured = true
	r.LimitPerKeyPerSecond = limit

	secs := make([]int64, 0, len(r.admittedPerSecond))
	for s := range r.admittedPerSecond {
		secs = append(secs, s)
	}
	sort.Slice(secs, func(i, j int) bool { return secs[i] < secs[j] })
	if len(secs) > 2 {
		secs = secs[1 : len(secs)-1]
	}

	var sum float64
	var n int
	// Per key: total admitted, and how many seconds that key was observed for.
	totalByKey := make(map[int]int64)
	secondsByKey := make(map[int]int)

	for _, sec := range secs {
		for keyIdx, admitted := range r.admittedPerSecond[sec] {
			excess := float64(admitted) - limit
			pct := 0.0
			if excess > 0 {
				pct = excess / limit * 100
				r.SecondsOverLimit++
			}
			if pct > r.MaxOverAdmissionPct {
				r.MaxOverAdmissionPct = pct
			}
			sum += pct
			n++
			totalByKey[keyIdx] += admitted
			secondsByKey[keyIdx]++
		}
	}
	r.SecondsMeasured = n
	if n > 0 {
		r.MeanOverAdmissionPct = sum / float64(n)
	}

	// Sustained: the fair cross-strategy comparison. A full bucket drained at
	// t=0 inflates the first second and nothing after it, so over a run of any
	// length the excess washes out and every strategy is measured against the
	// same sustained rate.
	var sustainedSum float64
	var keys int
	for keyIdx, total := range totalByKey {
		expected := limit * float64(secondsByKey[keyIdx])
		if expected <= 0 {
			continue
		}
		sustainedSum += (float64(total) - expected) / expected * 100
		keys++
	}
	if keys > 0 {
		r.SustainedOverAdmissionPct = sustainedSum / float64(keys)
	}
}

// validate encodes the methodology rules that decide whether numbers mean
// anything. A run that trips one of these is reported as invalid rather than
// quietly producing a plausible-looking chart.
func (r *Results) validate(o options) {
	r.Valid = true

	if r.Dropped > 0 {
		pct := float64(r.Dropped) / float64(max64(r.Issued, 1)) * 100
		r.Warnings = append(r.Warnings, fmt.Sprintf(
			"client shed %d requests (%.2f%% of issued): the generator could not keep up, "+
				"so latency is understated. Raise -max-inflight or use a larger VM.", r.Dropped, pct))
		r.Valid = false
	}

	// If the generator cannot sustain the offered rate, every number below is
	// describing a different experiment than the one requested. This is the
	// failure mode that silently invalidates cloud benchmarks.
	if r.AchievedRPS < o.rate*0.95 {
		r.Warnings = append(r.Warnings, fmt.Sprintf(
			"achieved %.0f RPS against an offered %.0f (%.1f%%): the load generator, "+
				"not the service, is the bottleneck. Results are not comparable across runs.",
			r.AchievedRPS, o.rate, r.AchievedRPS/o.rate*100))
		r.Valid = false
	}

	if r.Failed > 0 {
		pct := float64(r.Failed) / float64(max64(r.Completed, 1)) * 100
		msg := fmt.Sprintf("%d requests failed (%.2f%% of completed)", r.Failed, pct)
		if pct > 1 {
			r.Valid = false
			msg += ": above 1%, treat this run as a failure experiment rather than a latency measurement"
		}
		r.Warnings = append(r.Warnings, msg)
	}

	if r.ShedByPlatform > 0 {
		pct := float64(r.ShedByPlatform) / float64(max64(r.Completed, 1)) * 100
		msg := fmt.Sprintf("%d requests (%.2f%%) were shed by Cloud Run before reaching limiterd", r.ShedByPlatform, pct)
		if pct > 1 {
			r.Valid = false
			msg += ": above 1%, this run measures platform capacity, not enforcement. Add replicas or lower the offered rate."
		}
		r.Warnings = append(r.Warnings, msg)
	}

	if r.Completed == 0 {
		r.Warnings = append(r.Warnings, "no requests completed")
		r.Valid = false
	}
}

func (r *Results) printSummary(w io.Writer) {
	fmt.Fprintf(w, "\n=== loadgen: %s ===\n", orDash(r.Label))
	fmt.Fprintf(w, "offered      %.0f RPS   achieved %.0f RPS   wall %.1fs\n",
		r.OfferedRPS, r.AchievedRPS, r.WallSeconds)
	fmt.Fprintf(w, "completed    %d  (admitted %d, denied %d, failed %d, client-shed %d, platform-shed %d)\n",
		r.Completed, r.Admitted, r.Denied, r.Failed, r.Dropped, r.ShedByPlatform)
	fmt.Fprintf(w, "\nclient latency, from intended send time\n")
	fmt.Fprintf(w, "  p50   %8.3f ms\n  p99   %8.3f ms\n  p999  %8.3f ms\n  max   %8.3f ms\n",
		r.P50MS, r.P99MS, r.P999MS, r.MaxMS)

	if r.EnforcementMeasured {
		fmt.Fprintf(w, "\nenforcement vs limit of %.0f/key/sec over %d key-seconds\n",
			r.LimitPerKeyPerSecond, r.SecondsMeasured)
		fmt.Fprintf(w, "  sustained over-admission  %6.2f%%   <- cross-strategy headline\n",
			r.SustainedOverAdmissionPct)
		fmt.Fprintf(w, "  per-second mean           %6.2f%%\n  per-second max            %6.2f%%\n  key-seconds over limit    %d\n",
			r.MeanOverAdmissionPct, r.MaxOverAdmissionPct, r.SecondsOverLimit)
		fmt.Fprintf(w, "  note: a token bucket may exceed the per-second limit by up to its\n"+
			"        burst by design. Compare strategies on the sustained figure.\n")
	}

	if len(r.Warnings) > 0 {
		fmt.Fprintf(w, "\nwarnings\n")
		for _, msg := range r.Warnings {
			fmt.Fprintf(w, "  - %s\n", msg)
		}
	}
	verdict := "VALID"
	if !r.Valid {
		verdict = "INVALID -- do not report these numbers"
	}
	fmt.Fprintf(w, "\nverdict: %s\n", verdict)
}

func (o options) describe() map[string]string {
	return map[string]string{
		"target":       o.target,
		"rate":         fmt.Sprintf("%.0f", o.rate),
		"duration":     o.duration.String(),
		"warmup":       o.warmup.String(),
		"keys":         fmt.Sprintf("%d", o.keys),
		"limit":        fmt.Sprintf("%.0f", o.limit),
		"max_inflight": fmt.Sprintf("%d", o.maxInflight),
		"timeout":      o.timeout.String(),
	}
}

func usToMS(us int64) float64 {
	return math.Round(float64(us)/10) / 100
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func orDash(s string) string {
	if s == "" {
		return "(unlabelled)"
	}
	return s
}

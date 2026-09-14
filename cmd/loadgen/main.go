// Command loadgen drives open-loop load at a target service and reports
// latency distribution plus enforcement accuracy.
//
// Open-loop matters. A closed-loop generator -- N workers each looping
// "send, wait for reply, send again" -- stops issuing requests while the
// server is slow, so the very stalls you are trying to measure never get
// sampled. That is coordinated omission, and it makes tail latency look far
// better than it is.
//
// Two things here avoid it:
//
//  1. Requests are scheduled against wall-clock time, not against replies.
//  2. Latency is measured from a request's INTENDED send time, not from when
//     it actually went out. If the client falls behind, that delay is part of
//     what a user would experience, so it belongs in the histogram.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"time"
)

type options struct {
	target      string
	rate        float64
	duration    time.Duration
	warmup      time.Duration
	keys        int
	limit       float64
	maxInflight int
	timeout     time.Duration
	out         string
	label       string
}

func main() {
	var o options
	flag.StringVar(&o.target, "target", "http://127.0.0.1:8080", "base URL of limiterd")
	flag.Float64Var(&o.rate, "rate", 1000, "offered load in requests per second")
	flag.DurationVar(&o.duration, "duration", 90*time.Second, "measurement duration (after warmup)")
	flag.DurationVar(&o.warmup, "warmup", 10*time.Second, "warmup period, discarded from results")
	flag.IntVar(&o.keys, "keys", 1, "number of distinct rate-limit keys to spread load across")
	flag.Float64Var(&o.limit, "limit", 0, "configured limit per key per second, for enforcement error (0 = skip)")
	flag.IntVar(&o.maxInflight, "max-inflight", 20000, "cap on concurrent requests before shedding")
	flag.DurationVar(&o.timeout, "timeout", 5*time.Second, "per-request timeout")
	flag.StringVar(&o.out, "out", "", "write JSON results to this path (default: stdout only)")
	flag.StringVar(&o.label, "label", "", "free-form label recorded in the results")
	flag.Parse()

	res, err := run(context.Background(), o)
	if err != nil {
		fmt.Fprintf(os.Stderr, "loadgen: %v\n", err)
		os.Exit(1)
	}

	res.printSummary(os.Stdout)

	if o.out != "" {
		b, err := json.MarshalIndent(res, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "loadgen: encoding results: %v\n", err)
			os.Exit(1)
		}
		if err := os.WriteFile(o.out, append(b, '\n'), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "loadgen: writing %s: %v\n", o.out, err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stdout, "\nresults written to %s\n", o.out)
	}
}

// sample is one completed request, reported to the recorder.
type sample struct {
	// latency is measured from the intended send time -- see the package doc.
	latency  time.Duration
	intended time.Time
	keyIdx   int
	status   int
	admitted bool
	// shedByLB marks a non-2xx with no limiter body: the platform rejected the
	// request before limiterd saw it. Counted separately so platform overload
	// is never reported as enforcement.
	shedByLB bool
	failed   bool
	warmup   bool
}

func run(ctx context.Context, o options) (*Results, error) {
	client := &http.Client{
		Timeout: o.timeout,
		Transport: &http.Transport{
			MaxIdleConns:        o.maxInflight,
			MaxIdleConnsPerHost: o.maxInflight,
			MaxConnsPerHost:     0,
			IdleConnTimeout:     90 * time.Second,
			DialContext: (&net.Dialer{
				Timeout:   5 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
		},
	}

	bodies := make([][]byte, o.keys)
	for i := range bodies {
		bodies[i], _ = json.Marshal(map[string]any{
			"key":  fmt.Sprintf("key-%d", i),
			"cost": 1,
		})
	}

	samples := make(chan sample, 1<<16)
	done := make(chan *Results)
	go func() { done <- record(samples, o) }()

	sem := make(chan struct{}, o.maxInflight)
	url := o.target + "/v1/allow"

	start := time.Now()
	total := o.warmup + o.duration
	warmupEnd := start.Add(o.warmup)
	deadline := start.Add(total)

	// Scheduling in 1ms batches rather than one timer per request: at several
	// thousand RPS, per-request sleeps cost more in scheduler overhead than
	// the requests themselves, and the timer granularity is worse than the
	// batch interval anyway.
	const tickInterval = time.Millisecond
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()

	var issued int64
	var dropped int64
	// Tracks request goroutines so the sample channel is closed only once every
	// sender has finished. See the comment at inflight.Wait() below.
	var inflight sync.WaitGroup

	for now := range ticker.C {
		if now.After(deadline) {
			break
		}
		elapsed := now.Sub(start).Seconds()
		// Recompute from elapsed time each tick so scheduling never drifts.
		want := int64(elapsed * o.rate)
		for issued < want {
			// The time this request was SUPPOSED to go out.
			intended := start.Add(time.Duration(float64(issued) / o.rate * float64(time.Second)))
			keyIdx := int(issued) % len(bodies)
			body := bodies[keyIdx]
			isWarmup := intended.Before(warmupEnd)
			issued++

			select {
			case sem <- struct{}{}:
				inflight.Add(1)
				go func() {
					defer inflight.Done()
					defer func() { <-sem }()
					samples <- fire(ctx, client, url, body, intended, keyIdx, isWarmup)
				}()
			default:
				// Inflight cap reached. Shedding here keeps the generator
				// open-loop; blocking would silently convert it to closed-loop
				// and destroy the measurement. A nonzero count invalidates the
				// run, which is why it is reported prominently.
				dropped++
			}
		}
	}

	// Wait for every in-flight request to finish sending before closing the
	// stream.
	//
	// This was previously a deadline-based drain, which raced: when a request
	// outlived the deadline, close(samples) ran while its goroutine was still
	// sending, and the whole process died with "send on closed channel" --
	// taking the run's results with it. A deadline is the wrong tool because
	// the safe moment to close is defined by the senders, not by the clock.
	//
	// Waiting is bounded without a timeout of its own: every request carries
	// the client's timeout, so each goroutine is guaranteed to return.
	inflight.Wait()
	close(samples)

	res := <-done
	res.Config = o.describe()
	res.OfferedRPS = o.rate
	res.Issued = issued
	res.Dropped = dropped
	res.WallSeconds = time.Since(start).Seconds()
	res.finalize(o)
	return res, nil
}

func fire(ctx context.Context, c *http.Client, url string, body []byte, intended time.Time, keyIdx int, isWarmup bool) sample {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return sample{intended: intended, keyIdx: keyIdx, failed: true, warmup: isWarmup, latency: time.Since(intended)}
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.Do(req)
	if err != nil {
		return sample{intended: intended, keyIdx: keyIdx, failed: true, warmup: isWarmup, latency: time.Since(intended)}
	}
	// The body must be drained and closed for the connection to be reused;
	// skipping it silently collapses the pool and turns the benchmark into a
	// dial test.
	var decoded struct {
		Allowed *bool `json:"allowed"`
	}
	decodeErr := json.NewDecoder(resp.Body).Decode(&decoded)
	_ = resp.Body.Close()

	// Distinguish a limiter decision from platform shedding.
	//
	// Cloud Run returns its own 429 when an instance's request queue is full,
	// and that looks identical to a rate-limit denial if you only inspect the
	// status code. Conflating them is not a cosmetic problem: it reports the
	// platform running out of capacity as though the limiter were working,
	// which would make an overloaded run look like a successful enforcement
	// measurement.
	//
	// Only limiterd emits a JSON body carrying "allowed", so its presence is
	// what separates the two.
	isLimiterDecision := decodeErr == nil && decoded.Allowed != nil

	return sample{
		latency:  time.Since(intended),
		intended: intended,
		keyIdx:   keyIdx,
		status:   resp.StatusCode,
		admitted: isLimiterDecision && *decoded.Allowed,
		shedByLB: !isLimiterDecision && resp.StatusCode < 500,
		failed:   resp.StatusCode >= 500,
		warmup:   isWarmup,
	}
}

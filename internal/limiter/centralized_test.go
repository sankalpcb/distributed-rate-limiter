package limiter

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// newTestRedis gives each test its own in-process Redis speaking real RESP,
// so the Lua scripts under test are the ones that ship.
func newTestRedis(t *testing.T) *redis.Client {
	t.Helper()
	mr := miniredis.RunT(t)
	c := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// base is a fixed origin for test time. Nothing here reads the wall clock.
var base = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func TestCentralized_BurstThenDeny(t *testing.T) {
	c := NewCentralized(newTestRedis(t), Config{Rate: 10, Burst: 5, TTL: time.Minute}, FailClosed)
	ctx := context.Background()

	for i := range 5 {
		d, err := c.Allow(ctx, "k", 1, base)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		if !d.Allowed {
			t.Fatalf("request %d denied, want allowed within burst of 5", i)
		}
	}

	d, err := c.Allow(ctx, "k", 1, base)
	if err != nil {
		t.Fatal(err)
	}
	if d.Allowed {
		t.Fatal("6th request allowed, want denied: burst is exhausted")
	}
	if d.RetryAfter <= 0 {
		t.Fatalf("RetryAfter = %v, want positive so the caller knows when to return", d.RetryAfter)
	}
}

func TestCentralized_RefillsAtRate(t *testing.T) {
	c := NewCentralized(newTestRedis(t), Config{Rate: 10, Burst: 5, TTL: time.Minute}, FailClosed)
	ctx := context.Background()

	for range 5 {
		_, _ = c.Allow(ctx, "k", 1, base)
	}

	// 300ms at 10/sec refills 3 tokens. Not 4, not 2.
	at := base.Add(300 * time.Millisecond)
	for i := range 3 {
		d, _ := c.Allow(ctx, "k", 1, at)
		if !d.Allowed {
			t.Fatalf("refilled request %d denied, want allowed: 300ms at 10/s yields 3 tokens", i)
		}
	}
	if d, _ := c.Allow(ctx, "k", 1, at); d.Allowed {
		t.Fatal("4th refilled request allowed, want denied: only 3 tokens had accrued")
	}
}

func TestCentralized_RefillClampsAtBurst(t *testing.T) {
	c := NewCentralized(newTestRedis(t), Config{Rate: 10, Burst: 5, TTL: time.Minute}, FailClosed)
	ctx := context.Background()

	for range 5 {
		_, _ = c.Allow(ctx, "k", 1, base)
	}

	// An hour of idling must not mint 36,000 tokens.
	at := base.Add(time.Hour)
	allowed := 0
	for range 20 {
		if d, _ := c.Allow(ctx, "k", 1, at); d.Allowed {
			allowed++
		}
	}
	if allowed != 5 {
		t.Fatalf("allowed %d after an idle hour, want 5: refill must clamp at burst", allowed)
	}
}

func TestCentralized_BackwardsClockDoesNotMintTokens(t *testing.T) {
	c := NewCentralized(newTestRedis(t), Config{Rate: 10, Burst: 5, TTL: time.Minute}, FailClosed)
	ctx := context.Background()

	for range 5 {
		_, _ = c.Allow(ctx, "k", 1, base)
	}
	// Replica clocks are not aligned; a request may carry an earlier stamp.
	if d, _ := c.Allow(ctx, "k", 1, base.Add(-time.Hour)); d.Allowed {
		t.Fatal("request with a backwards timestamp was allowed, want denied")
	}
}

func TestCentralized_CostGreaterThanOne(t *testing.T) {
	c := NewCentralized(newTestRedis(t), Config{Rate: 10, Burst: 5, TTL: time.Minute}, FailClosed)
	ctx := context.Background()

	if d, _ := c.Allow(ctx, "k", 3, base); !d.Allowed {
		t.Fatal("cost-3 request denied against a burst of 5, want allowed")
	}
	if d, _ := c.Allow(ctx, "k", 3, base); d.Allowed {
		t.Fatal("second cost-3 request allowed, want denied: only 2 tokens remain")
	}
}

func TestCentralized_KeysAreIndependent(t *testing.T) {
	c := NewCentralized(newTestRedis(t), Config{Rate: 10, Burst: 2, TTL: time.Minute}, FailClosed)
	ctx := context.Background()

	for range 2 {
		_, _ = c.Allow(ctx, "alice", 1, base)
	}
	if d, _ := c.Allow(ctx, "alice", 1, base); d.Allowed {
		t.Fatal("alice over budget but allowed")
	}
	if d, _ := c.Allow(ctx, "bob", 1, base); !d.Allowed {
		t.Fatal("bob denied because alice exhausted her budget: keys must not share state")
	}
}

// The store being unreachable has no correct answer, only a policy. Both
// policies are exercised because experiment E4 reports both.
func TestCentralized_FailModes(t *testing.T) {
	for _, tc := range []struct {
		name        string
		mode        FailMode
		wantAllowed bool
		wantErr     bool
	}{
		{"closed denies", FailClosed, false, true},
		{"open admits", FailOpen, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := NewCentralized(newDeadRedis(t), Config{Rate: 10, Burst: 5, TTL: time.Minute}, tc.mode)

			d, err := c.Allow(context.Background(), "k", 1, base)
			if d.Allowed != tc.wantAllowed {
				t.Errorf("Allowed = %v, want %v", d.Allowed, tc.wantAllowed)
			}
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

// Replicas do not share a clock. Cloud Run instances are NTP-synced to within
// a millisecond or so, which sounds negligible until you notice that at a few
// thousand requests per second the gap between consecutive requests to one key
// is smaller than the skew itself.
//
// The bucket stores the timestamp of the last refill. If a caller whose clock
// lags writes that timestamp backwards, the next caller computes its elapsed
// time from the older mark and refills for an interval that was already
// credited. Each alternation re-opens up to `skew` worth of refill, so the
// inflation scales with skew divided by the inter-request interval -- which is
// why this is invisible at low rates and severe at high ones.
//
// Measured on GCP before the fix: 17.4% sustained over-admission from the
// strategy whose entire purpose is exact enforcement.
func TestCentralized_SkewedReplicaClocksDoNotMintTokens(t *testing.T) {
	const (
		rate    = 1000.0                 // tokens per second
		burst   = int64(10)              //
		skew    = 2 * time.Millisecond   // how far replica B lags replica A
		step    = 500 * time.Microsecond // real time between consecutive requests
		pairs   = 2000                   // 2000 pairs x 500us = 1s of real time
		elapsed = time.Duration(pairs) * step
	)

	c := NewCentralized(newTestRedis(t), Config{Rate: rate, Burst: burst, TTL: time.Minute}, FailClosed)
	ctx := context.Background()

	admitted := 0
	for i := range pairs {
		at := base.Add(time.Duration(i) * step)

		// Replica A: clock correct.
		if d, err := c.Allow(ctx, "k", 1, at); err == nil && d.Allowed {
			admitted++
		}
		// Replica B: same instant, clock lagging.
		if d, err := c.Allow(ctx, "k", 1, at.Add(-skew)); err == nil && d.Allowed {
			admitted++
		}
	}

	// Over one second of real time a bucket refilling at 1000/s, starting full
	// at 10, can admit at most 1010 however many replicas ask.
	maxAdmissible := int(rate*elapsed.Seconds()) + int(burst)
	if admitted > maxAdmissible {
		overPct := float64(admitted-maxAdmissible) / float64(maxAdmissible) * 100
		t.Fatalf("admitted %d over %v, want at most %d (over-admitted by %.1f%%): "+
			"a lagging replica must not move the bucket's timestamp backwards",
			admitted, elapsed, maxAdmissible, overPct)
	}
}

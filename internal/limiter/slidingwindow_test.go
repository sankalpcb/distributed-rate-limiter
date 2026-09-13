package limiter

import (
	"context"
	"testing"
	"time"
)

func TestSlidingWindow_EnforcesLimitInAFreshWindow(t *testing.T) {
	s := NewSlidingWindow(newTestRedis(t), Config{Rate: 10, TTL: time.Minute}, time.Second, FailClosed)
	ctx := context.Background()

	allowed := 0
	for range 20 {
		if d, _ := s.Allow(ctx, "k", 1, base); d.Allowed {
			allowed++
		}
	}
	if allowed != 10 {
		t.Fatalf("allowed %d, want 10", allowed)
	}
}

// The flaw this strategy exists to fix: with naive fixed windows a client can
// spend a full budget at the end of one window and again at the start of the
// next, admitting 2x the limit across the boundary. The weighted estimate must
// carry the previous window's usage forward.
func TestSlidingWindow_PreventsBoundaryBurst(t *testing.T) {
	s := NewSlidingWindow(newTestRedis(t), Config{Rate: 10, TTL: time.Minute}, time.Second, FailClosed)
	ctx := context.Background()

	// Spend the whole budget at the very end of a window.
	lateInWindow := base.Add(990 * time.Millisecond)
	for range 10 {
		if d, _ := s.Allow(ctx, "k", 1, lateInWindow); !d.Allowed {
			t.Fatal("setup: budget should be available late in the window")
		}
	}

	// 20ms later we are in the next window, but 98% of the previous one is
	// still inside the trailing second.
	justAfter := base.Add(1010 * time.Millisecond)
	allowed := 0
	for range 10 {
		if d, _ := s.Allow(ctx, "k", 1, justAfter); d.Allowed {
			allowed++
		}
	}
	if allowed > 1 {
		t.Fatalf("allowed %d immediately after the boundary, want at most 1: "+
			"the previous window's usage must be carried forward", allowed)
	}
}

func TestSlidingWindow_BudgetReturnsAsThePreviousWindowAgesOut(t *testing.T) {
	s := NewSlidingWindow(newTestRedis(t), Config{Rate: 10, TTL: time.Minute}, time.Second, FailClosed)
	ctx := context.Background()

	for range 10 {
		_, _ = s.Allow(ctx, "k", 1, base)
	}

	// Half a window later, roughly half the old usage still counts, so about
	// half the budget should be available. Asserting a band, not a point:
	// the estimator is an approximation and pinning it exactly would make the
	// test a change-detector.
	half := base.Add(1500 * time.Millisecond)
	allowed := 0
	for range 10 {
		if d, _ := s.Allow(ctx, "k", 1, half); d.Allowed {
			allowed++
		}
	}
	if allowed < 4 || allowed > 6 {
		t.Fatalf("allowed %d half a window later, want 4-6", allowed)
	}
}

func TestSlidingWindow_FullyRecoversAfterTwoWindows(t *testing.T) {
	s := NewSlidingWindow(newTestRedis(t), Config{Rate: 10, TTL: time.Minute}, time.Second, FailClosed)
	ctx := context.Background()

	for range 10 {
		_, _ = s.Allow(ctx, "k", 1, base)
	}

	later := base.Add(2 * time.Second)
	allowed := 0
	for range 10 {
		if d, _ := s.Allow(ctx, "k", 1, later); d.Allowed {
			allowed++
		}
	}
	if allowed != 10 {
		t.Fatalf("allowed %d two windows later, want the full 10 back", allowed)
	}
}

func TestSlidingWindow_KeysAreIndependent(t *testing.T) {
	s := NewSlidingWindow(newTestRedis(t), Config{Rate: 2, TTL: time.Minute}, time.Second, FailClosed)
	ctx := context.Background()

	for range 2 {
		_, _ = s.Allow(ctx, "alice", 1, base)
	}
	if d, _ := s.Allow(ctx, "bob", 1, base); !d.Allowed {
		t.Fatal("bob denied because alice spent her budget")
	}
}

func TestSlidingWindow_FailModes(t *testing.T) {
	for _, tc := range []struct {
		name        string
		mode        FailMode
		wantAllowed bool
	}{
		{"closed denies", FailClosed, false},
		{"open admits", FailOpen, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewSlidingWindow(newDeadRedis(t), Config{Rate: 10, TTL: time.Minute}, time.Second, tc.mode)

			d, _ := s.Allow(context.Background(), "k", 1, base)
			if d.Allowed != tc.wantAllowed {
				t.Errorf("Allowed = %v, want %v", d.Allowed, tc.wantAllowed)
			}
		})
	}
}

func TestParseFailMode(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    FailMode
		wantErr bool
	}{
		{"open", FailOpen, false},
		{"fail-open", FailOpen, false},
		{"closed", FailClosed, false},
		{"", FailClosed, false},
		{"sideways", FailClosed, true},
	} {
		got, err := ParseFailMode(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("ParseFailMode(%q) err = %v, wantErr %v", tc.in, err, tc.wantErr)
		}
		if got != tc.want {
			t.Errorf("ParseFailMode(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

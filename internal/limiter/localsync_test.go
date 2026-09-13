package limiter

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeCounter is a shared counter with no network. It records call counts so
// tests can assert how much Redis traffic a sync interval actually costs.
type fakeCounter struct {
	mu     sync.Mutex
	totals map[string]int64
	calls  int
	err    error
}

func newFakeCounter() *fakeCounter {
	return &fakeCounter{totals: make(map[string]int64)}
}

func (f *fakeCounter) AddAndGet(_ context.Context, key string, delta int64, _ time.Duration) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return 0, f.err
	}
	f.totals[key] += delta
	return f.totals[key], nil
}

func (f *fakeCounter) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

const testWindow = time.Second

func TestLocalSync_EnforcesLimitWithinOneReplica(t *testing.T) {
	l := NewLocalSync(newFakeCounter(), Config{Rate: 10, TTL: time.Minute}, testWindow)
	ctx := context.Background()

	allowed := 0
	for range 20 {
		if d, _ := l.Allow(ctx, "k", 1, base); d.Allowed {
			allowed++
		}
	}
	// A single replica sees all of its own admissions immediately, so it is
	// exact. Over-admission is a multi-replica phenomenon.
	if allowed != 10 {
		t.Fatalf("allowed %d, want 10: a lone replica has no one to drift from", allowed)
	}
}

func TestLocalSync_WindowRollsOver(t *testing.T) {
	l := NewLocalSync(newFakeCounter(), Config{Rate: 10, TTL: time.Minute}, testWindow)
	ctx := context.Background()

	for range 10 {
		_, _ = l.Allow(ctx, "k", 1, base)
	}
	if d, _ := l.Allow(ctx, "k", 1, base); d.Allowed {
		t.Fatal("11th request in window allowed, want denied")
	}
	if d, _ := l.Allow(ctx, "k", 1, base.Add(testWindow)); !d.Allowed {
		t.Fatal("first request of the next window denied, want allowed")
	}
}

// This is the mechanism the whole project measures: two replicas, no sync,
// each admits a full limit because neither can see the other.
func TestLocalSync_TwoReplicasOverAdmitBeforeSync(t *testing.T) {
	shared := newFakeCounter()
	cfg := Config{Rate: 10, TTL: time.Minute}
	a := NewLocalSync(shared, cfg, testWindow)
	b := NewLocalSync(shared, cfg, testWindow)
	ctx := context.Background()

	admitted := 0
	for range 10 {
		if d, _ := a.Allow(ctx, "k", 1, base); d.Allowed {
			admitted++
		}
		if d, _ := b.Allow(ctx, "k", 1, base); d.Allowed {
			admitted++
		}
	}

	if admitted != 20 {
		t.Fatalf("admitted %d, want 20: with R=2 and no reconciliation the fleet admits R x limit", admitted)
	}
	// Restating the finding as the metric the benchmark reports.
	overAdmission := float64(admitted-10) / 10 * 100
	if overAdmission != 100 {
		t.Fatalf("over-admission = %.0f%%, want 100%%", overAdmission)
	}
}

// Reconciliation converges replicas, but not simultaneously: whoever syncs
// last sees the complete picture first. The replica that synced earlier keeps
// admitting against a stale total until its own next sync -- which is exactly
// the drift the benchmark quantifies.
func TestLocalSync_SyncConvergesReplicasInOrder(t *testing.T) {
	shared := newFakeCounter()
	cfg := Config{Rate: 10, TTL: time.Minute}
	a := NewLocalSync(shared, cfg, testWindow)
	b := NewLocalSync(shared, cfg, testWindow)
	ctx := context.Background()

	for range 5 {
		_, _ = a.Allow(ctx, "k", 1, base)
		_, _ = b.Allow(ctx, "k", 1, base)
	}
	// The fleet has admitted 10 -- exactly the limit -- but neither replica
	// knows it yet.

	if err := a.Sync(ctx, base); err != nil { // a publishes 5, learns total=5
		t.Fatal(err)
	}
	if err := b.Sync(ctx, base); err != nil { // b publishes 5, learns total=10
		t.Fatal(err)
	}

	// b synced last, so its view is complete and it correctly denies.
	if d, _ := b.Allow(ctx, "k", 1, base); d.Allowed {
		t.Fatal("replica b admitted, want denied: it has seen the full fleet total")
	}

	// a still believes the fleet is at 5, so it over-admits. This is the
	// mechanism, not a bug -- asserting it pins the semantics in place.
	if d, _ := a.Allow(ctx, "k", 1, base); !d.Allowed {
		t.Fatal("replica a denied, want allowed: its view is one sync stale")
	}

	// Once a syncs again it catches up and stops.
	if err := a.Sync(ctx, base); err != nil {
		t.Fatal(err)
	}
	if d, _ := a.Allow(ctx, "k", 1, base); d.Allowed {
		t.Fatal("replica a admitted after catching up, want denied")
	}
}

func TestLocalSync_SyncPublishesOnlyTheUnsentDelta(t *testing.T) {
	shared := newFakeCounter()
	l := NewLocalSync(shared, Config{Rate: 100, TTL: time.Minute}, testWindow)
	ctx := context.Background()

	for range 3 {
		_, _ = l.Allow(ctx, "k", 1, base)
	}
	_ = l.Sync(ctx, base)
	for range 2 {
		_, _ = l.Allow(ctx, "k", 1, base)
	}
	_ = l.Sync(ctx, base)

	shared.mu.Lock()
	defer shared.mu.Unlock()
	var total int64
	for _, v := range shared.totals {
		total += v
	}
	if total != 5 {
		t.Fatalf("shared total = %d, want 5: a second sync must not republish the first batch", total)
	}
}

func TestLocalSync_NoSyncTrafficWithoutAdmissions(t *testing.T) {
	shared := newFakeCounter()
	l := NewLocalSync(shared, Config{Rate: 10, TTL: time.Minute}, testWindow)

	for range 5 {
		_ = l.Sync(context.Background(), base)
	}
	if shared.calls != 0 {
		t.Fatalf("shared counter called %d times with nothing admitted, want 0", shared.calls)
	}
}

// The availability argument for this strategy: Redis dies, requests keep being
// served from local state.
func TestLocalSync_KeepsServingWhenSharedStoreFails(t *testing.T) {
	shared := newFakeCounter()
	l := NewLocalSync(shared, Config{Rate: 10, TTL: time.Minute}, testWindow)
	ctx := context.Background()

	_, _ = l.Allow(ctx, "k", 1, base)
	shared.setErr(errors.New("connection refused"))

	if err := l.Sync(ctx, base); err == nil {
		t.Fatal("Sync returned nil during an outage, want the error surfaced")
	}
	if l.SyncFailures() != 1 {
		t.Fatalf("SyncFailures = %d, want 1", l.SyncFailures())
	}

	// The request path is unaffected -- that is the entire point.
	d, err := l.Allow(ctx, "k", 1, base)
	if err != nil {
		t.Fatalf("Allow failed during a store outage: %v", err)
	}
	if !d.Allowed {
		t.Fatal("Allow denied during a store outage, want served from local state")
	}
}

// A failed sync must not lose the delta; it is republished next time.
func TestLocalSync_RetainsDeltaAcrossAFailedSync(t *testing.T) {
	shared := newFakeCounter()
	l := NewLocalSync(shared, Config{Rate: 100, TTL: time.Minute}, testWindow)
	ctx := context.Background()

	for range 4 {
		_, _ = l.Allow(ctx, "k", 1, base)
	}
	shared.setErr(errors.New("down"))
	_ = l.Sync(ctx, base)
	shared.setErr(nil)
	_ = l.Sync(ctx, base)

	shared.mu.Lock()
	defer shared.mu.Unlock()
	var total int64
	for _, v := range shared.totals {
		total += v
	}
	if total != 4 {
		t.Fatalf("shared total = %d, want 4: the delta from the failed sync must survive", total)
	}
}

func TestLocalSync_ConcurrentAllowIsRaceFree(t *testing.T) {
	l := NewLocalSync(newFakeCounter(), Config{Rate: 1000, TTL: time.Minute}, testWindow)
	ctx := context.Background()

	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed := 0
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 40 {
				if d, _ := l.Allow(ctx, "k", 1, base); d.Allowed {
					mu.Lock()
					allowed++
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()

	if allowed != 1000 {
		t.Fatalf("allowed %d across 2000 concurrent requests, want exactly 1000", allowed)
	}
}

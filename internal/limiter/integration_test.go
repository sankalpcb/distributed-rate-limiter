//go:build integration

// These tests run against a real Redis rather than the in-process one used by
// the unit tests. They exist because miniredis interprets Lua with a different
// engine than Redis does, and the scripts are the part of this system where a
// subtle semantic difference would be both plausible and invisible.
//
//	make test-integration
package limiter

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func realRedis(t *testing.T) *redis.Client {
	t.Helper()
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		t.Skip("REDIS_ADDR not set; run via `make test-integration`")
	}
	c := redis.NewClient(&redis.Options{Addr: addr})
	if err := c.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("cannot reach Redis at %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// uniqueKey keeps parallel runs and repeated invocations from colliding in a
// shared Redis.
func uniqueKey(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("%s-%d", t.Name(), time.Now().UnixNano())
}

// The property that justifies doing the work inside Lua: concurrent callers
// against one bucket must never admit more than the burst between them.
func TestIntegration_CentralizedIsAtomicUnderConcurrency(t *testing.T) {
	rdb := realRedis(t)
	const burst = 100
	c := NewCentralized(rdb, Config{Rate: 0, Burst: burst, TTL: time.Minute}, FailClosed)

	key := uniqueKey(t)
	now := time.Now()

	var wg sync.WaitGroup
	var mu sync.Mutex
	admitted := 0

	// 500 concurrent requests against a bucket holding 100 tokens, with a
	// refill rate of zero so the answer cannot drift while the test runs.
	for range 500 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, err := c.Allow(context.Background(), key, 1, now)
			if err != nil {
				t.Errorf("Allow: %v", err)
				return
			}
			if d.Allowed {
				mu.Lock()
				admitted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if admitted != burst {
		t.Fatalf("admitted %d of 500 concurrent requests, want exactly %d: "+
			"refill-and-deduct must be atomic", admitted, burst)
	}
}

// Two LocalSync instances sharing one Redis are the multi-replica case the
// benchmark measures. Here we only assert the mechanism is wired correctly
// end to end: they converge once both have published.
func TestIntegration_LocalSyncConvergesThroughRealRedis(t *testing.T) {
	rdb := realRedis(t)
	counter := newRedisCounter(rdb)

	cfg := Config{Rate: 10, TTL: time.Minute}
	a := NewLocalSync(counter, cfg, time.Second)
	b := NewLocalSync(counter, cfg, time.Second)

	key := uniqueKey(t)
	now := time.Now()
	ctx := context.Background()

	for range 5 {
		if d, _ := a.Allow(ctx, key, 1, now); !d.Allowed {
			t.Fatal("setup: replica a should admit within its local view")
		}
		if d, _ := b.Allow(ctx, key, 1, now); !d.Allowed {
			t.Fatal("setup: replica b should admit within its local view")
		}
	}

	if err := a.Sync(ctx, now); err != nil {
		t.Fatalf("a.Sync: %v", err)
	}
	if err := b.Sync(ctx, now); err != nil {
		t.Fatalf("b.Sync: %v", err)
	}

	// b published last, so it sees the full fleet total of 10 and stops.
	if d, _ := b.Allow(ctx, key, 1, now); d.Allowed {
		t.Fatal("replica b admitted past the fleet limit after a full sync")
	}
}

func TestIntegration_SlidingWindowCarriesThePreviousWindow(t *testing.T) {
	rdb := realRedis(t)
	s := NewSlidingWindow(rdb, Config{Rate: 10, TTL: time.Minute}, time.Second, FailClosed)

	key := uniqueKey(t)
	// Anchor to a window boundary so the overlap arithmetic is predictable
	// regardless of when the test happens to run.
	start := time.Now().Truncate(time.Second)
	ctx := context.Background()

	lateInWindow := start.Add(990 * time.Millisecond)
	for range 10 {
		if d, _ := s.Allow(ctx, key, 1, lateInWindow); !d.Allowed {
			t.Fatal("setup: full budget should be available late in a fresh window")
		}
	}

	justAfter := start.Add(1010 * time.Millisecond)
	allowed := 0
	for range 10 {
		if d, _ := s.Allow(ctx, key, 1, justAfter); d.Allowed {
			allowed++
		}
	}
	if allowed > 1 {
		t.Fatalf("allowed %d across the window boundary, want at most 1", allowed)
	}
}

// newRedisCounter mirrors redisx.NewCounter. It is duplicated here rather than
// imported because internal/redisx imports nothing from this package and the
// dependency should not be reversed just for a test.
func newRedisCounter(rdb *redis.Client) SharedCounter { return &redisCounter{rdb: rdb} }

type redisCounter struct{ rdb *redis.Client }

var incrScript = redis.NewScript(`
local total = redis.call('INCRBY', KEYS[1], ARGV[1])
redis.call('PEXPIRE', KEYS[1], ARGV[2])
return total
`)

func (c *redisCounter) AddAndGet(ctx context.Context, key string, delta int64, ttl time.Duration) (int64, error) {
	return incrScript.Run(ctx, c.rdb, []string{key}, delta, ttl.Milliseconds()).Int64()
}

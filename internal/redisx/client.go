// Package redisx holds the Redis client construction and the shared-counter
// implementation used by the localsync strategy.
package redisx

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

// Options tunes the client for benchmark conditions rather than production
// defaults.
type Options struct {
	Addr     string
	PoolSize int
	// Timeout bounds every stage of a call. Under benchmark we would rather
	// record a fast failure than let a stalled request masquerade as latency.
	Timeout time.Duration
}

// New builds a Redis client. Retries are disabled: a retried command inflates
// the measured latency of a single logical request and hides the failure that
// experiment E4 is trying to observe.
func New(o Options) *redis.Client {
	if o.PoolSize == 0 {
		o.PoolSize = 100
	}
	if o.Timeout == 0 {
		o.Timeout = 500 * time.Millisecond
	}
	return redis.NewClient(&redis.Options{
		Addr:         o.Addr,
		PoolSize:     o.PoolSize,
		MaxRetries:   -1,
		DialTimeout:  o.Timeout,
		ReadTimeout:  o.Timeout,
		WriteTimeout: o.Timeout,
	})
}

// incrScript increments and refreshes the TTL in one round trip.
var incrScript = redis.NewScript(`
local total = redis.call('INCRBY', KEYS[1], ARGV[1])
redis.call('PEXPIRE', KEYS[1], ARGV[2])
return total
`)

// Counter is the Redis-backed limiter.SharedCounter used by localsync.
type Counter struct {
	rdb *redis.Client
}

// NewCounter wraps a client as a shared counter.
func NewCounter(rdb *redis.Client) *Counter { return &Counter{rdb: rdb} }

// AddAndGet publishes delta and returns the resulting fleet total.
func (c *Counter) AddAndGet(ctx context.Context, key string, delta int64, ttl time.Duration) (int64, error) {
	return incrScript.Run(ctx, c.rdb, []string{key}, delta, ttl.Milliseconds()).Int64()
}

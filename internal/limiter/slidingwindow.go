package limiter

import (
	"context"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// slidingWindowScript is the weighted two-fixed-window approximation.
//
//	estimate ~= prev_count * overlap + current_count
//
// The exact form -- a sorted set holding one timestamp per request -- costs
// O(requests) memory per key. The approximation is O(1) and assumes arrivals
// were uniform across the previous window, which is where its error comes
// from. It still fixes the flaw that makes naive fixed windows unusable: a
// client spending a full budget at the end of one window and again at the
// start of the next, admitting 2x the limit across the boundary.
//
// KEYS[1] current window  KEYS[2] previous window
// ARGV[1] limit  ARGV[2] cost  ARGV[3] overlap (0..1)  ARGV[4] ttl (ms)
// returns {allowed, remaining}
var slidingWindowScript = redis.NewScript(`
local cur     = tonumber(redis.call('GET', KEYS[1]) or '0')
local prev    = tonumber(redis.call('GET', KEYS[2]) or '0')
local limit   = tonumber(ARGV[1])
local cost    = tonumber(ARGV[2])
local overlap = tonumber(ARGV[3])
local ttl     = tonumber(ARGV[4])

local est = prev * overlap + cur
local allowed = 0
if est + cost <= limit then
  redis.call('INCRBY', KEYS[1], cost)
  redis.call('PEXPIRE', KEYS[1], ttl)
  allowed = 1
  est = est + cost
end

local remaining = math.floor(limit - est)
if remaining < 0 then remaining = 0 end
return {allowed, remaining}
`)

// SlidingWindow is the middle of the results table: approximate like
// LocalSync, but with a Redis round trip like Centralized, so its error comes
// from the estimator rather than from replica skew.
type SlidingWindow struct {
	rdb    *redis.Client
	cfg    Config
	window time.Duration
	fail   FailMode
}

// NewSlidingWindow builds a strategy using the weighted two-window estimate.
func NewSlidingWindow(rdb *redis.Client, cfg Config, window time.Duration, fail FailMode) *SlidingWindow {
	return &SlidingWindow{rdb: rdb, cfg: cfg, window: window, fail: fail}
}

func (s *SlidingWindow) Allow(ctx context.Context, key string, cost int64, now time.Time) (Decision, error) {
	w := now.UnixNano() / int64(s.window)
	// Fraction of the previous window still inside the trailing window.
	elapsed := time.Duration(now.UnixNano() - w*int64(s.window))
	overlap := 1.0 - float64(elapsed)/float64(s.window)

	limit := int64(s.cfg.Rate * s.window.Seconds())

	res, err := slidingWindowScript.Run(ctx, s.rdb,
		[]string{s.key(key, w), s.key(key, w-1)},
		limit, cost, overlap, s.cfg.TTL.Milliseconds(),
	).Slice()
	if err != nil {
		if s.fail == FailOpen {
			return Decision{Allowed: true, Remaining: -1}, nil
		}
		return Decision{Allowed: false, Remaining: 0}, ErrStoreUnavailable
	}
	allowed, _ := res[0].(int64)
	remaining, _ := res[1].(int64)
	return Decision{
		Allowed:   allowed == 1,
		Remaining: remaining,
		RetryAfter: time.Duration(float64(s.window) *
			(1 - float64(elapsed)/float64(s.window))),
	}, nil
}

func (s *SlidingWindow) key(key string, w int64) string {
	return "rl:sw:" + key + ":" + strconv.FormatInt(w, 10)
}

func (s *SlidingWindow) Close() error { return nil }

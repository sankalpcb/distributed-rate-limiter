package limiter

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

// centralizedScript is a token bucket evaluated atomically inside Redis.
//
// Refill-and-deduct must be one round trip: splitting it into a read, a
// compute, and a write would let two replicas interleave and both admit
// against the same tokens. Lua gives us atomicity without a lock.
//
// KEYS[1] bucket key
// ARGV[1] rate (units/sec)  ARGV[2] burst  ARGV[3] now (ms)
// ARGV[4] cost              ARGV[5] ttl (ms)
// returns {allowed, remaining, retry_after_ms}
var centralizedScript = redis.NewScript(`
local data = redis.call('HMGET', KEYS[1], 'tokens', 'ts')
local rate  = tonumber(ARGV[1])
local burst = tonumber(ARGV[2])
local now   = tonumber(ARGV[3])
local cost  = tonumber(ARGV[4])
local ttl   = tonumber(ARGV[5])

local tokens = tonumber(data[1])
local ts     = tonumber(data[2])
if tokens == nil or ts == nil then
  tokens = burst
  ts = now
end

-- Clamp elapsed at zero: replica clocks are not perfectly aligned, and a
-- backwards jump must never mint tokens.
local elapsed = now - ts
if elapsed < 0 then elapsed = 0 end
tokens = math.min(burst, tokens + (elapsed / 1000.0) * rate)

local allowed = 0
if tokens >= cost then
  tokens = tokens - cost
  allowed = 1
end

redis.call('HMSET', KEYS[1], 'tokens', tokens, 'ts', now)
redis.call('PEXPIRE', KEYS[1], ttl)

local retry = 0
if allowed == 0 and rate > 0 then
  retry = math.ceil(((cost - tokens) / rate) * 1000)
end
return {allowed, math.floor(tokens), retry}
`)

// Centralized enforces exactly, by making every admission decision inside
// Redis. Cost: a round trip on the critical path of every request, and Redis
// throughput becomes a hard ceiling for the entire fleet.
type Centralized struct {
	rdb  *redis.Client
	cfg  Config
	fail FailMode
}

// NewCentralized builds a strategy that holds authoritative state in Redis.
func NewCentralized(rdb *redis.Client, cfg Config, fail FailMode) *Centralized {
	return &Centralized{rdb: rdb, cfg: cfg, fail: fail}
}

func (c *Centralized) Allow(ctx context.Context, key string, cost int64, now time.Time) (Decision, error) {
	res, err := centralizedScript.Run(ctx, c.rdb,
		[]string{"rl:cb:" + key},
		c.cfg.Rate, c.cfg.Burst, now.UnixMilli(), cost, c.cfg.TTL.Milliseconds(),
	).Slice()
	if err != nil {
		// Redis is gone. There is no correct answer here, only a choice of
		// which failure to absorb -- see FailMode.
		if c.fail == FailOpen {
			return Decision{Allowed: true, Remaining: -1}, nil
		}
		return Decision{Allowed: false, Remaining: 0}, ErrStoreUnavailable
	}
	allowed, _ := res[0].(int64)
	remaining, _ := res[1].(int64)
	retry, _ := res[2].(int64)
	return Decision{
		Allowed:    allowed == 1,
		Remaining:  remaining,
		RetryAfter: time.Duration(retry) * time.Millisecond,
	}, nil
}

func (c *Centralized) Close() error { return nil }

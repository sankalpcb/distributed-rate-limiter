package limiter

import (
	"context"
	"sync"
	"time"
)

// SharedCounter is the reconciliation point for LocalSync. Implementations
// must apply delta atomically across replicas and return the resulting total.
type SharedCounter interface {
	AddAndGet(ctx context.Context, key string, delta int64, ttl time.Duration) (int64, error)
}

// LocalSync admits against a replica-local counter and reconciles with the
// shared counter every sync interval.
//
// Between syncs a replica cannot see admissions made by its peers, so the
// fleet over-admits. Per sync interval the excess is bounded by roughly
//
//	excess <= (R - 1) * A
//
// where R is replica count and A is what one replica can admit within an
// interval before reconciling. That bound is loose; experiment E2 measures
// where reality falls inside it.
//
// The payoff is that Redis leaves the critical path entirely, and the
// strategy keeps serving -- with decaying accuracy -- when Redis is gone.
type LocalSync struct {
	counter SharedCounter
	cfg     Config
	window  time.Duration

	mu    sync.Mutex
	state map[string]*lsState

	// syncFailures counts reconciliation errors, so a run can distinguish
	// "drifted because the interval is long" from "drifted because Redis died".
	syncFailures int64
}

type lsState struct {
	window      int64 // window index this state belongs to
	globalKnown int64 // fleet total as of the last successful sync
	localDelta  int64 // admitted here since that sync, not yet published
}

// NewLocalSync builds a strategy that keeps the shared store off the request
// path. Call Sync periodically to reconcile.
func NewLocalSync(counter SharedCounter, cfg Config, window time.Duration) *LocalSync {
	return &LocalSync{
		counter: counter,
		cfg:     cfg,
		window:  window,
		state:   make(map[string]*lsState),
	}
}

// limit is the admissions permitted per window for the whole fleet.
func (l *LocalSync) limit() int64 {
	return int64(l.cfg.Rate * l.window.Seconds())
}

func (l *LocalSync) windowIndex(now time.Time) int64 {
	return now.UnixNano() / int64(l.window)
}

func (l *LocalSync) Allow(_ context.Context, key string, cost int64, now time.Time) (Decision, error) {
	w := l.windowIndex(now)

	l.mu.Lock()
	defer l.mu.Unlock()

	st, ok := l.state[key]
	if !ok {
		st = &lsState{window: w}
		l.state[key] = st
	}
	if st.window != w {
		// New window: local view resets. Anything unpublished from the old
		// window is deliberately dropped -- it belongs to a limit that has
		// already expired.
		st.window = w
		st.globalKnown = 0
		st.localDelta = 0
	}

	limit := l.limit()
	used := st.globalKnown + st.localDelta
	if used+cost <= limit {
		st.localDelta += cost
		return Decision{Allowed: true, Remaining: limit - (used + cost)}, nil
	}
	return Decision{
		Allowed:    false,
		Remaining:  0,
		RetryAfter: time.Until(time.Unix(0, (w+1)*int64(l.window))),
	}, nil
}

// Sync publishes this replica's unsent admissions and adopts the fleet total.
//
// Exposed as an explicit method rather than hidden in a goroutine so tests can
// drive reconciliation deterministically; limiterd runs it on a ticker.
func (l *LocalSync) Sync(ctx context.Context, now time.Time) error {
	w := l.windowIndex(now)

	type pending struct {
		key   string
		delta int64
	}
	var work []pending

	l.mu.Lock()
	for key, st := range l.state {
		if st.window == w && st.localDelta > 0 {
			work = append(work, pending{key: key, delta: st.localDelta})
		}
	}
	l.mu.Unlock()

	var firstErr error
	for _, p := range work {
		total, err := l.counter.AddAndGet(ctx, l.counterKey(p.key, w), p.delta, l.cfg.TTL)
		if err != nil {
			// Keep the delta queued: it will be published on the next
			// successful sync, provided the window has not rolled over.
			l.mu.Lock()
			l.syncFailures++
			l.mu.Unlock()
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		l.mu.Lock()
		if st, ok := l.state[p.key]; ok && st.window == w {
			st.globalKnown = total
			// Subtract only what was published; admissions that landed while
			// the round trip was in flight stay pending.
			st.localDelta -= p.delta
			if st.localDelta < 0 {
				st.localDelta = 0
			}
		}
		l.mu.Unlock()
	}
	return firstErr
}

func (l *LocalSync) counterKey(key string, w int64) string {
	return "rl:ls:" + key + ":" + itoa(w)
}

// SyncFailures reports reconciliation errors observed so far.
func (l *LocalSync) SyncFailures() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.syncFailures
}

func (l *LocalSync) Close() error { return nil }

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [24]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

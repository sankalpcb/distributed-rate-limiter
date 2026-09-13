// Package limiter defines the rate-limiting strategy interface and its
// implementations. Every strategy answers one question -- "is this request
// allowed?" -- and they differ only in where the authoritative state lives
// and how often replicas reconcile it.
package limiter

import (
	"context"
	"errors"
	"time"
)

// Decision is the result of an admission check.
type Decision struct {
	Allowed    bool          `json:"allowed"`
	Remaining  int64         `json:"remaining"`
	RetryAfter time.Duration `json:"-"`
}

// Limiter decides whether a request against key may proceed.
//
// now is a parameter rather than a call to time.Now() inside the
// implementation. That makes every strategy deterministically testable with a
// fake clock: "does the bucket refill correctly across ten minutes" becomes a
// microsecond-scale unit test instead of a sleep.
type Limiter interface {
	Allow(ctx context.Context, key string, cost int64, now time.Time) (Decision, error)
	Close() error
}

// Config is the enforcement target shared by all strategies.
type Config struct {
	// Rate is the sustained admission rate in units per second.
	Rate float64
	// Burst is the maximum instantaneous allowance. Token-bucket strategies
	// use it as bucket capacity; window strategies derive their per-window
	// limit from Rate and the window size.
	Burst int64
	// TTL bounds how long per-key state survives without traffic.
	TTL time.Duration
}

// FailMode selects behaviour when the shared store is unreachable.
//
// This is a real availability tradeoff, not a default worth guessing at:
// FailOpen protects your own availability but exposes whatever the limiter
// was shielding; FailClosed protects that backend at the cost of a
// self-inflicted outage. Experiment E4 measures both.
type FailMode int

const (
	FailClosed FailMode = iota
	FailOpen
)

func (f FailMode) String() string {
	if f == FailOpen {
		return "open"
	}
	return "closed"
}

// ParseFailMode maps configuration strings onto a FailMode.
func ParseFailMode(s string) (FailMode, error) {
	switch s {
	case "open", "fail-open", "fail_open":
		return FailOpen, nil
	case "closed", "fail-closed", "fail_closed", "":
		return FailClosed, nil
	default:
		return FailClosed, errors.New("limiter: unknown fail mode " + s)
	}
}

// ErrStoreUnavailable is returned when the shared store cannot be reached and
// the configured FailMode does not resolve the request on its own.
var ErrStoreUnavailable = errors.New("limiter: shared store unavailable")

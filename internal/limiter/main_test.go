package limiter

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

type silentLogger struct{}

func (silentLogger) Printf(context.Context, string, ...any) {}

// The fail-mode tests deliberately point a client at a dead Redis. go-redis
// logs every failed dial, which buries real output, so silence it for the
// package.
func TestMain(m *testing.M) {
	redis.SetLogger(silentLogger{})
	os.Exit(m.Run())
}

// newDeadRedis returns a client whose server has been shut down, configured to
// give up immediately. Without this the fail-mode tests spend seconds in dial
// backoff proving something the first attempt already established.
func newDeadRedis(t *testing.T) *redis.Client {
	t.Helper()
	mr := miniredis.RunT(t)
	c := redis.NewClient(&redis.Options{
		Addr:         mr.Addr(),
		MaxRetries:   -1,
		DialTimeout:  50 * time.Millisecond,
		ReadTimeout:  50 * time.Millisecond,
		WriteTimeout: 50 * time.Millisecond,
	})
	t.Cleanup(func() { _ = c.Close() })
	mr.Close()
	return c
}

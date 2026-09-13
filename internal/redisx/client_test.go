package redisx

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

func TestCounter_AccumulatesAcrossCallers(t *testing.T) {
	mr := miniredis.RunT(t)
	c := NewCounter(New(Options{Addr: mr.Addr()}))
	ctx := context.Background()

	// Two "replicas" publishing into the same window key.
	if got, err := c.AddAndGet(ctx, "w:1", 5, time.Minute); err != nil || got != 5 {
		t.Fatalf("first publish = %d, %v; want 5, nil", got, err)
	}
	if got, err := c.AddAndGet(ctx, "w:1", 3, time.Minute); err != nil || got != 8 {
		t.Fatalf("second publish = %d, %v; want 8, nil", got, err)
	}
	// A different window must not share the total.
	if got, err := c.AddAndGet(ctx, "w:2", 1, time.Minute); err != nil || got != 1 {
		t.Fatalf("new window = %d, %v; want 1, nil", got, err)
	}
}

func TestCounter_SetsExpiry(t *testing.T) {
	mr := miniredis.RunT(t)
	c := NewCounter(New(Options{Addr: mr.Addr()}))

	if _, err := c.AddAndGet(context.Background(), "w:1", 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	if ttl := mr.TTL("w:1"); ttl <= 0 {
		t.Fatalf("TTL = %v, want positive: window keys must not accumulate forever", ttl)
	}
}

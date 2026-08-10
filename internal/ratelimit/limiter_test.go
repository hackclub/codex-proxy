package ratelimit

import (
	"testing"
	"time"
)

func TestLimiter(t *testing.T) {
	now := time.Unix(0, 0)
	limiter := New()
	limiter.now = func() time.Time { return now }

	if !limiter.Allow("client", 2) || !limiter.Allow("client", 2) {
		t.Fatal("initial burst was rejected")
	}
	if limiter.Allow("client", 2) {
		t.Fatal("third request should be limited")
	}
	now = now.Add(500 * time.Millisecond)
	if !limiter.Allow("client", 2) {
		t.Fatal("token did not refill")
	}
}

func TestCleanup(t *testing.T) {
	now := time.Unix(0, 0)
	limiter := New()
	limiter.now = func() time.Time { return now }
	limiter.Allow("stale", 1)
	now = now.Add(2 * time.Hour)
	if removed := limiter.Cleanup(time.Hour); removed != 1 {
		t.Fatalf("removed %d buckets", removed)
	}
}

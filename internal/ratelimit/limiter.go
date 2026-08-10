package ratelimit

import (
	"math"
	"sync"
	"time"
)

type Limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	now     func() time.Time
}

type bucket struct {
	tokens   float64
	capacity float64
	rate     float64
	updated  time.Time
	seen     time.Time
}

func New() *Limiter {
	return &Limiter{buckets: make(map[string]*bucket), now: time.Now}
}

func (l *Limiter) Allow(key string, requestsPerSecond float64) bool {
	if key == "" || requestsPerSecond <= 0 {
		return false
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	capacity := math.Max(1, math.Ceil(requestsPerSecond))
	b := l.buckets[key]
	if b == nil || b.rate != requestsPerSecond {
		b = &bucket{
			tokens:   capacity,
			capacity: capacity,
			rate:     requestsPerSecond,
			updated:  now,
			seen:     now,
		}
		l.buckets[key] = b
	}

	elapsed := now.Sub(b.updated).Seconds()
	b.tokens = math.Min(b.capacity, b.tokens+elapsed*b.rate)
	b.updated = now
	b.seen = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func (l *Limiter) Cleanup(maxIdle time.Duration) int {
	l.mu.Lock()
	defer l.mu.Unlock()

	cutoff := l.now().Add(-maxIdle)
	removed := 0
	for key, b := range l.buckets {
		if b.seen.Before(cutoff) {
			delete(l.buckets, key)
			removed++
		}
	}
	return removed
}

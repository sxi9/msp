package server

import (
	"sync"
	"time"
)

// rateLimiter is an in-memory per-key token bucket: capacity perMinute,
// refilled continuously at perMinute tokens per minute. Keys idle for an hour
// are swept on the next call after each hourly boundary so the map cannot
// grow without bound under address churn.
type rateLimiter struct {
	mu        sync.Mutex
	perMinute float64
	buckets   map[string]*bucket
	lastSweep time.Time
	now       func() time.Time // tests override
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newRateLimiter(perMinute int) *rateLimiter {
	if perMinute <= 0 {
		perMinute = 1
	}
	return &rateLimiter{
		perMinute: float64(perMinute),
		buckets:   make(map[string]*bucket),
		now:       time.Now,
	}
}

// allow spends one token for key and reports whether one was available.
func (l *rateLimiter) allow(key string) bool {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.lastSweep.IsZero() {
		l.lastSweep = now
	} else if now.Sub(l.lastSweep) >= time.Hour {
		for k, b := range l.buckets {
			if now.Sub(b.last) >= time.Hour {
				delete(l.buckets, k)
			}
		}
		l.lastSweep = now
	}

	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.perMinute, last: now}
		l.buckets[key] = b
	} else {
		elapsed := now.Sub(b.last)
		if elapsed > 0 {
			b.tokens += elapsed.Minutes() * l.perMinute
			if b.tokens > l.perMinute {
				b.tokens = l.perMinute
			}
		}
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

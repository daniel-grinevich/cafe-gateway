// Package ratelimit is an in-process token bucket per API key.
//
// Keys are service-scoped and each service is its own deployment, so a key is only counted by one process per
// replica. With N replicas a key effectively gets up to N×rpm; move this to Postgres or Redis before scaling out.
package ratelimit

import (
	"math"
	"strconv"
	"sync"
	"time"
)

type bucket struct {
	tokens  float64
	updated time.Time
}

type Limiter struct {
	mu      sync.Mutex
	now     func() time.Time
	buckets map[string]*bucket
}

func New(now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	return &Limiter{now: now, buckets: map[string]*bucket{}}
}

// Allow takes one token. It returns 0 when allowed, else how long until a token is available.
func (l *Limiter) Allow(key string, rpm int) time.Duration {
	if rpm <= 0 {
		return time.Minute
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	capacity, rate := float64(rpm), float64(rpm)/60.0
	now := l.now()
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: capacity, updated: now}
		l.buckets[key] = b
	} else {
		b.tokens = math.Min(capacity, b.tokens+now.Sub(b.updated).Seconds()*rate)
		b.updated = now
	}
	if b.tokens >= 1 {
		b.tokens--
		return 0
	}
	return time.Duration((1 - b.tokens) / rate * float64(time.Second))
}

// Prune drops buckets that have been full for a while, so revoked or idle keys don't accumulate.
func (l *Limiter) Prune(idle time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := l.now().Add(-idle)
	for k, b := range l.buckets {
		if b.updated.Before(cutoff) {
			delete(l.buckets, k)
		}
	}
}

// RetryAfter formats a wait as whole seconds, at least 1.
func RetryAfter(d time.Duration) string {
	return strconv.Itoa(max(1, int(math.Ceil(d.Seconds()))))
}

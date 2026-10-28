package ratelimit

import (
	"testing"
	"time"
)

func TestRefills(t *testing.T) {
	now := time.Unix(0, 0)
	l := New(func() time.Time { return now })
	for range 2 {
		if l.Allow("k", 2) != 0 {
			t.Fatal("burst of rpm should pass")
		}
	}
	wait := l.Allow("k", 2)
	if wait <= 0 || wait > 30*time.Second {
		t.Fatalf("wait = %v", wait)
	}
	if RetryAfter(wait) != "30" {
		t.Fatalf("Retry-After = %s", RetryAfter(wait))
	}
	now = now.Add(30 * time.Second)
	if l.Allow("k", 2) != 0 {
		t.Fatal("should refill one token in 30s at 2 rpm")
	}
	if l.Allow("other", 2) != 0 {
		t.Fatal("keys are independent")
	}
	now = now.Add(time.Hour)
	l.Prune(time.Minute)
	if len(l.buckets) != 0 {
		t.Fatal("prune")
	}
}

package server

import (
	"testing"
	"time"
)

func TestNotifier(t *testing.T) {
	n := newNotifier()
	ch1, unsub1 := n.Subscribe(1)
	ch2, unsub2 := n.Subscribe(1)
	chOther, unsubOther := n.Subscribe(2)
	defer unsubOther()
	if n.waiting(1) != 2 || n.waiting(2) != 1 || n.waiting(3) != 0 {
		t.Fatalf("waiting counts %d %d %d", n.waiting(1), n.waiting(2), n.waiting(3))
	}

	// Notify reaches every waiter of the device and nobody else.
	n.Notify(1)
	for i, ch := range []<-chan struct{}{ch1, ch2} {
		select {
		case <-ch:
		case <-time.After(time.Second):
			t.Fatalf("waiter %d not woken", i)
		}
	}
	select {
	case <-chOther:
		t.Fatal("device 2 woken by device 1 notify")
	default:
	}

	// A second Notify before the waiter reads must not block (buffer 1).
	done := make(chan struct{})
	go func() {
		n.Notify(1)
		n.Notify(1)
		n.Notify(1)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Notify blocked")
	}
	<-ch1 // exactly one pending wake is kept
	select {
	case <-ch1:
		t.Fatal("more than one wake buffered")
	default:
	}

	// Unsubscribe is idempotent and removes only its own channel.
	unsub1()
	unsub1()
	if n.waiting(1) != 1 {
		t.Fatalf("after unsubscribe: %d waiters", n.waiting(1))
	}
	unsub2()
	if n.waiting(1) != 0 {
		t.Fatalf("after both unsubscribed: %d waiters", n.waiting(1))
	}
	if _, ok := n.waiters[1]; ok {
		t.Fatal("empty waiter list not deleted")
	}
	n.Notify(1) // no waiters: no-op
}

func TestRateLimiter(t *testing.T) {
	now := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	l := newRateLimiter(10)
	l.now = func() time.Time { return now }
	for i := 0; i < 10; i++ {
		if !l.allow("a") {
			t.Fatalf("call %d denied", i+1)
		}
	}
	if l.allow("a") {
		t.Fatal("11th call allowed")
	}
	if !l.allow("b") {
		t.Fatal("other key affected")
	}
	// Refill: 6 s = one token at 10/min.
	now = now.Add(6 * time.Second)
	if !l.allow("a") || l.allow("a") {
		t.Fatal("refill after 6 s should grant exactly one token")
	}
	// Idle keys are swept after an hour.
	now = now.Add(2 * time.Hour)
	l.allow("c")
	if _, ok := l.buckets["a"]; ok {
		t.Fatal("idle bucket not swept")
	}
	if len(l.buckets) != 1 {
		t.Fatalf("buckets after sweep: %d", len(l.buckets))
	}
}

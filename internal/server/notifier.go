package server

import "sync"

// notifier wakes long-polling sync requests. Each parked sync subscribes to
// its device id; a staff command for that device calls Notify. Channels are
// buffered (1) so a Notify that arrives between claim and select is not lost.
type notifier struct {
	mu      sync.Mutex
	waiters map[int64][]chan struct{}
}

func newNotifier() *notifier {
	return &notifier{waiters: make(map[int64][]chan struct{})}
}

// Subscribe registers a waiter for a device and returns its channel and an
// unsubscribe function (idempotent, safe to defer).
func (n *notifier) Subscribe(id int64) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	n.mu.Lock()
	n.waiters[id] = append(n.waiters[id], ch)
	n.mu.Unlock()

	var once sync.Once
	unsubscribe := func() {
		once.Do(func() {
			n.mu.Lock()
			defer n.mu.Unlock()
			list := n.waiters[id]
			for i, c := range list {
				if c == ch {
					list = append(list[:i], list[i+1:]...)
					break
				}
			}
			if len(list) == 0 {
				delete(n.waiters, id)
			} else {
				n.waiters[id] = list
			}
		})
	}
	return ch, unsubscribe
}

// Notify wakes every waiter of a device without blocking; a waiter that
// already has a pending wake is left as is.
func (n *notifier) Notify(id int64) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, ch := range n.waiters[id] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// waiting reports how many syncs are parked for a device (tests).
func (n *notifier) waiting(id int64) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.waiters[id])
}

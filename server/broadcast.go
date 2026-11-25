package server

import (
	"sync"

	"github.com/Autumn-27/artex/db"
)

// Broadcaster is a per-task in-process pub/sub for live activity events. The
// engine publishes each appended activity at its single emit point; SSE handlers
// subscribe per task. Storage (activity table) and the live stream come from the
// same Publish call, so they never diverge.
type Broadcaster struct {
	mu   sync.Mutex
	subs map[string]map[chan db.Activity]chan struct{} // task id -> (sub ch -> optional overflow signal ch)
}

func NewBroadcaster() *Broadcaster {
	return &Broadcaster{subs: map[string]map[chan db.Activity]chan struct{}{}}
}

// Subscribe returns a buffered channel of activities for a task plus an
// unsubscribe func the caller must invoke (defer) to release it.
func (b *Broadcaster) Subscribe(task string) (<-chan db.Activity, func()) {
	ch, _, unsub := b.SubscribeResync(task)
	return ch, unsub
}

// SubscribeResync is like Subscribe but also returns a 1-buffered overflow
// notification channel signalled whenever an event is dropped for this subscriber.
func (b *Broadcaster) SubscribeResync(task string) (<-chan db.Activity, <-chan struct{}, func()) {
	ch := make(chan db.Activity, 256)
	ov := make(chan struct{}, 1)
	b.mu.Lock()
	if b.subs[task] == nil {
		b.subs[task] = map[chan db.Activity]chan struct{}{}
	}
	b.subs[task][ch] = ov
	b.mu.Unlock()

	var once sync.Once
	return ch, ov, func() {
		once.Do(func() {
			b.mu.Lock()
			if m := b.subs[task]; m != nil {
				delete(m, ch)
				if len(m) == 0 {
					delete(b.subs, task)
				}
			}
			b.mu.Unlock()
			close(ch)
		})
	}
}

// Publish fans an activity out to all subscribers of a task. Non-blocking: if a
// subscriber's buffer is full the event is dropped and its overflow signal is
// raised so WebSocket clients immediately resync from the DB.
func (b *Broadcaster) Publish(task string, a db.Activity) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch, ov := range b.subs[task] {
		select {
		case ch <- a:
		default:
			select {
			case ov <- struct{}{}:
			default:
			}
		}
	}
}

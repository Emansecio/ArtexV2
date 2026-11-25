package agent

import (
	"context"
	"sync"
)

type flightCall[T any] struct {
	done chan struct{}
	val  T
	err  error
}

// flightGroup deduplicates concurrent in-flight executions sharing the same key
// without pulling in external dependencies.
type flightGroup[T any] struct {
	mu sync.Mutex
	m  map[string]*flightCall[T]
}

func (g *flightGroup[T]) Do(key string, fn func() (T, error)) (T, error) {
	return g.DoContext(context.Background(), key, func(context.Context) (T, error) {
		return fn()
	})
}

// DoContext deduplicates concurrent executions while isolating cancellation:
// the leader runs with context.WithoutCancel(ctx) so one cancelled caller cannot
// abort sibling waiters, and each waiter unblocks immediately if its own ctx ends.
func (g *flightGroup[T]) DoContext(ctx context.Context, key string, fn func(context.Context) (T, error)) (T, error) {
	g.mu.Lock()
	if g.m == nil {
		g.m = make(map[string]*flightCall[T])
	}
	if c, ok := g.m[key]; ok {
		g.mu.Unlock()
		select {
		case <-c.done:
			return c.val, c.err
		case <-ctx.Done():
			var zero T
			return zero, ctx.Err()
		}
	}
	c := &flightCall[T]{done: make(chan struct{})}
	g.m[key] = c
	g.mu.Unlock()

	go func() {
		defer func() {
			g.mu.Lock()
			delete(g.m, key)
			g.mu.Unlock()
			close(c.done)
		}()
		c.val, c.err = fn(context.WithoutCancel(ctx))
	}()

	select {
	case <-c.done:
		return c.val, c.err
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	}
}

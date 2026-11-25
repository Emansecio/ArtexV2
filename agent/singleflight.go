package agent

import "sync"

type flightCall[T any] struct {
	wg  sync.WaitGroup
	val T
	err error
}

// flightGroup deduplicates concurrent in-flight executions sharing the same key
// without pulling in external dependencies.
type flightGroup[T any] struct {
	mu sync.Mutex
	m  map[string]*flightCall[T]
}

func (g *flightGroup[T]) Do(key string, fn func() (T, error)) (T, error) {
	g.mu.Lock()
	if g.m == nil {
		g.m = make(map[string]*flightCall[T])
	}
	if c, ok := g.m[key]; ok {
		g.mu.Unlock()
		c.wg.Wait()
		return c.val, c.err
	}
	c := new(flightCall[T])
	c.wg.Add(1)
	g.m[key] = c
	g.mu.Unlock()

	defer func() {
		g.mu.Lock()
		delete(g.m, key)
		g.mu.Unlock()
		c.wg.Done()
	}()

	c.val, c.err = fn()
	return c.val, c.err
}

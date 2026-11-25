package server

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Autumn-27/artex/db"
)

func TestUsageSinkConcurrentEnqueueAndFlush(t *testing.T) {
	pg := &db.DB{}
	sink := usageRecorderFor(pg)
	t.Cleanup(func() { usageSinks.Delete(pg) })

	const producers = 8
	const perProducer = 100
	var executed atomic.Int64
	var wg sync.WaitGroup

	// Concurrent producers enqueueing work while flushers repeatedly drain the sink.
	for range producers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range perProducer {
				sink.enqueue(func() {
					executed.Add(1)
				})
			}
		}()
	}

	stopFlush := make(chan struct{})
	var flushWG sync.WaitGroup
	for range 4 {
		flushWG.Add(1)
		go func() {
			defer flushWG.Done()
			for {
				select {
				case <-stopFlush:
					return
				default:
					flushUsageLedger(pg)
				}
			}
		}()
	}

	wg.Wait()
	close(stopFlush)
	flushWG.Wait()

	// Final barrier must observe every enqueued item completed.
	flushUsageLedger(pg)
	if got, want := executed.Load(), int64(producers*perProducer); got != want {
		t.Fatalf("executed = %d, want %d", got, want)
	}
}

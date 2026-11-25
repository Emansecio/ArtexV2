package server

import (
	"log"
	"sync"

	"github.com/Autumn-27/artex/db"
)

// usageSink serializes tool_usage / skill_usage ledger inserts on a single
// background goroutine so a Postgres roundtrip never sits on the tool-call
// critical path. The queue is bounded; under sustained overflow the caller
// falls back to a synchronous insert rather than dropping ledger rows.
type usageSink struct {
	pg *db.DB
	ch chan func()
}

// usageSinks holds one sink per *db.DB so the async recorder can be obtained
// anywhere the pool handle already flows, without changing wiring signatures.
var usageSinks sync.Map

func usageRecorderFor(pg *db.DB) *usageSink {
	if pg == nil {
		return nil // meterTool/meterSkillTool pass a nil recorder through untouched
	}
	if v, ok := usageSinks.Load(pg); ok {
		return v.(*usageSink)
	}
	s := &usageSink{pg: pg, ch: make(chan func(), 256)}
	actual, loaded := usageSinks.LoadOrStore(pg, s)
	if !loaded {
		go func() {
			for f := range s.ch {
				f()
			}
		}()
	}
	return actual.(*usageSink)
}

func (s *usageSink) enqueue(f func()) {
	select {
	case s.ch <- f:
	default:
		f() // queue full: run inline so the ledger row is not lost
	}
}

// InsertToolUsage satisfies toolUsageRecorder; the write lands asynchronously.
func (s *usageSink) InsertToolUsage(u *db.ToolUsage) error {
	s.enqueue(func() {
		if err := s.pg.InsertToolUsage(u); err != nil {
			log.Printf("[toolusage] insert %s: %v", u.ToolKey, err)
		}
	})
	return nil
}

// InsertSkillUsage satisfies skillUsageWriter; the write lands asynchronously.
func (s *usageSink) InsertSkillUsage(u *db.SkillUsage) error {
	s.enqueue(func() {
		if err := s.pg.InsertSkillUsage(u); err != nil {
			log.Printf("[skillusage] insert: %v", err)
		}
	})
	return nil
}

// flushUsageLedger waits for every queued ledger write on pg to commit. Called
// after a task goes quiescent (so inserts complete before task-delete FK
// cascades) and on shutdown. Best-effort: a producer racing the Wait may slip
// one write past it.
func flushUsageLedger(pg *db.DB) {
	if pg == nil {
		return
	}
	if v, ok := usageSinks.Load(pg); ok {
		done := make(chan struct{})
		v.(*usageSink).ch <- func() { close(done) }
		<-done
	}
}

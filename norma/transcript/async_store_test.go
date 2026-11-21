package transcript

import (
	"fmt"
	"sync"
	"testing"

	"github.com/Autumn-27/norma/llm"
)

func TestAsyncStoreConcurrentAppendFlushLoad(t *testing.T) {
	s := NewStore(t.TempDir())
	defer func() {
		if err := s.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
	}()

	const writers = 4
	const msgsEach = 25
	var wg sync.WaitGroup
	for g := range writers {
		g := g
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := s.NewWriter(fmt.Sprintf("sess-%d", g), "")
			for i := range msgsEach {
				w.RecordMessage(llm.UserText(fmt.Sprintf("msg-%d-%d", g, i)), llm.Usage{InputTokens: 1})
			}
			if err := w.Flush(); err != nil {
				t.Errorf("flush %d: %v", g, err)
			}
		}()
	}
	wg.Wait()

	for g := range writers {
		recs, err := s.Load(s.MainPath(fmt.Sprintf("sess-%d", g)))
		if err != nil {
			t.Fatalf("load %d: %v", g, err)
		}
		if len(recs) != msgsEach {
			t.Fatalf("sess-%d records=%d, want %d", g, len(recs), msgsEach)
		}
	}
}

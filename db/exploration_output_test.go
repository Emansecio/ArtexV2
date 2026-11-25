package db

import "testing"

// WorkerOutputText is the single-query replacement for the old
// ActivityList(0,1000)+scan+ActivityDetail path on the planner's trigger
// render: latest 'result' preferred, else latest 'text', detail before summary.
func TestWorkerOutputText(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		SkipPGUnavailable(t, err)
	}
	t.Cleanup(func() { _ = d.Close() })

	expID, err := d.CreateExploration("worker-output", "latest output")
	if err != nil {
		t.Fatal(err)
	}
	s := d.Exploration(expID)
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM explorations WHERE id=$1`, expID) })

	nodeID, err := s.AddNode(KindIntent, map[string]any{"text": "work"}, 0, "running", "planner", nil)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := s.WorkerOutputText(nodeID); err != nil || out != "" {
		t.Fatalf("no activity → %q %v", out, err)
	}

	mk := func(kind, summary, detail string) {
		a := Activity{Worker: "worker-1", NodeID: &nodeID, Kind: kind, Summary: summary, Detail: detail}
		if _, err := s.AppendActivity(a); err != nil {
			t.Fatal(err)
		}
	}
	mk("text", "first note", "")
	mk("text", "second note", "second detail")
	if out, err := s.WorkerOutputText(nodeID); err != nil || out != "second detail" {
		t.Fatalf("latest text detail → %q %v", out, err)
	}
	mk("result", "final answer", "")
	if out, err := s.WorkerOutputText(nodeID); err != nil || out != "final answer" {
		t.Fatalf("result preferred over newer text → %q %v", out, err)
	}
	// A text row after the result must not displace the result.
	mk("text", "later note", "")
	if out, err := s.WorkerOutputText(nodeID); err != nil || out != "final answer" {
		t.Fatalf("result must win over later text → %q %v", out, err)
	}
}

// TerminalIntentResultSummaries replaces the per-intent activity page loop in
// related-task overviews: one query maps each terminal intent to its latest
// result (preferred) or text summary.
func TestTerminalIntentResultSummaries(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		SkipPGUnavailable(t, err)
	}
	t.Cleanup(func() { _ = d.Close() })

	expID, err := d.CreateExploration("terminal-summaries", "distinct on")
	if err != nil {
		t.Fatal(err)
	}
	s := d.Exploration(expID)
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM explorations WHERE id=$1`, expID) })

	mkIntent := func(text, state string) int64 {
		id, err := s.AddNode(KindIntent, map[string]any{"text": text}, 0, state, "planner", nil)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	mkAct := func(nodeID int64, kind, summary string) {
		a := Activity{Worker: "worker-1", NodeID: &nodeID, Kind: kind, Summary: summary}
		if _, err := s.AppendActivity(a); err != nil {
			t.Fatal(err)
		}
	}

	done1 := mkIntent("first", "done")
	done2 := mkIntent("second", "blocked")
	open1 := mkIntent("still open", "open")
	mkAct(done1, "text", "partial")
	mkAct(done1, "result", "done1 result")
	mkAct(done2, "text", "blocked note")
	mkAct(open1, "result", "must not surface")

	out, err := s.TerminalIntentResultSummaries([]int64{done1, done2, open1})
	if err != nil {
		t.Fatal(err)
	}
	if out[done1] != "done1 result" {
		t.Fatalf("done1 = %q", out[done1])
	}
	if out[done2] != "blocked note" {
		t.Fatalf("done2 = %q", out[done2])
	}
	if _, ok := out[open1]; ok {
		t.Fatal("non-terminal intent must not appear in the map")
	}
	if got, err := s.TerminalIntentResultSummaries(nil); err != nil || len(got) != 0 {
		t.Fatalf("empty input → %v %v", got, err)
	}
}

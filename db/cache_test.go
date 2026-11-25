package db

import (
	"testing"
	"time"
)

// The settings cache must reflect writes immediately — a stale read after
// SetSetting would flip feature flags back for up to the TTL window.
func TestSettingCacheInvalidatesOnWrite(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		SkipPGUnavailable(t, err)
	}
	t.Cleanup(func() { _ = d.Close() })

	key := "cache_test_setting"
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM settings WHERE key=$1`, key) })

	if err := d.SetSetting(key, "one"); err != nil {
		t.Fatal(err)
	}
	// Prime the cache, then write through the API and read again.
	if v, _, err := d.GetSetting(key); err != nil || v != "one" {
		t.Fatalf("first read = %q %v", v, err)
	}
	if err := d.SetSetting(key, "two"); err != nil {
		t.Fatal(err)
	}
	if v, _, err := d.GetSetting(key); err != nil || v != "two" {
		t.Fatalf("read after write = %q %v — cache served a stale value", v, err)
	}
}

// Writers that bypass the setting helpers (direct SQL) must be able to drop
// the cache explicitly so the next read hits Postgres.
func TestSettingCacheManualInvalidation(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		SkipPGUnavailable(t, err)
	}
	t.Cleanup(func() { _ = d.Close() })

	key := "cache_test_raw"
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM settings WHERE key=$1`, key) })

	if err := d.SetSetting(key, "before"); err != nil {
		t.Fatal(err)
	}
	if v, _, _ := d.GetSetting(key); v != "before" {
		t.Fatalf("prime read = %q", v)
	}
	if _, err := d.Exec(`UPDATE settings SET value='after' WHERE key=$1`, key); err != nil {
		t.Fatal(err)
	}
	d.InvalidateSettingCache(key)
	if v, _, err := d.GetSetting(key); err != nil || v != "after" {
		t.Fatalf("read after manual invalidation = %q %v", v, err)
	}
}

// ListTools must pick up rows written after the first read (write-through
// invalidation on tool CRUD).
func TestToolCacheInvalidatesOnSeed(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		SkipPGUnavailable(t, err)
	}
	t.Cleanup(func() { _ = d.Close() })

	if _, err := d.ListTools(); err != nil {
		t.Fatal(err) // prime cache
	}
	key := "cache_test_tool"
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM tools WHERE key=$1`, key) })
	if err := d.SeedTool(key, "desc", []byte(`{}`), []byte(`["worker"]`)); err != nil {
		t.Fatal(err)
	}
	rows, err := d.ListTools()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range rows {
		if r.Key == key {
			found = true
		}
	}
	if !found {
		t.Fatal("seeded tool missing from ListTools — stale cache")
	}
}

// Updated_at hygiene under bookkeeping writes: stamp-maintenance and
// claim-lease churn must not move updated_at (it backs the overview
// fingerprint), while a real content change must bump it.
func TestNodeUpdatedAtIgnoresBookkeepingWrites(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		SkipPGUnavailable(t, err)
	}
	t.Cleanup(func() { _ = d.Close() })

	expID, err := d.CreateExploration("stamp-trigger", "updated_at semantics")
	if err != nil {
		t.Fatal(err)
	}
	s := d.Exploration(expID)
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM explorations WHERE id=$1`, expID) })

	nodeID, err := s.AddNode(KindIntent, map[string]any{"text": "w"}, 0, "open", "system", nil)
	if err != nil {
		t.Fatal(err)
	}
	readTS := func() time.Time {
		var ts time.Time
		if err := d.QueryRow(`SELECT updated_at FROM exploration_nodes WHERE id=$1`, nodeID).Scan(&ts); err != nil {
			t.Fatal(err)
		}
		return ts.UTC()
	}
	ts0 := readTS()

	// Bookkeeping-only write: cold_since_round stamp → updated_at preserved.
	if err := s.ApplyStampOps([]StampOp{{ID: nodeID, Set: true, Round: 3}}); err != nil {
		t.Fatal(err)
	}
	if got := readTS(); !got.Equal(ts0) {
		t.Fatalf("stamp write bumped updated_at %v → %v — fingerprint would bust", ts0, got)
	}

	// Claim-lease write → also preserved.
	if _, err := d.Exec(`UPDATE exploration_nodes SET last_heartbeat_at=now(), unclaim_token='tok' WHERE id=$1`, nodeID); err != nil {
		t.Fatal(err)
	}
	if got := readTS(); !got.Equal(ts0) {
		t.Fatalf("claim-lease write bumped updated_at %v → %v", ts0, got)
	}

	// Real content change (state) → must bump.
	time.Sleep(2 * time.Millisecond)
	if _, err := d.Exec(`UPDATE exploration_nodes SET state='running' WHERE id=$1`, nodeID); err != nil {
		t.Fatal(err)
	}
	if got := readTS(); !got.After(ts0) {
		t.Fatalf("state change did not bump updated_at (still %v)", got)
	}
}

// ApplyStampOps batches sets and clears into bulk UPDATEs — verify both land
// and that an empty op list is a no-op.
func TestApplyStampOpsBatch(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		SkipPGUnavailable(t, err)
	}
	t.Cleanup(func() { _ = d.Close() })

	expID, err := d.CreateExploration("stamp-batch", "bulk ops")
	if err != nil {
		t.Fatal(err)
	}
	s := d.Exploration(expID)
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM explorations WHERE id=$1`, expID) })

	a, _ := s.AddNode(KindFact, map[string]any{"text": "a"}, 0, "confirmed", "worker", nil)
	b, _ := s.AddNode(KindFact, map[string]any{"text": "b"}, 0, "confirmed", "worker", nil)
	c, _ := s.AddNode(KindFact, map[string]any{"text": "c"}, 0, "confirmed", "worker", nil)

	if err := s.ApplyStampOps(nil); err != nil {
		t.Fatalf("empty ops: %v", err)
	}
	// Seed a stamp on c, then run a mixed batch: set a+b, clear c.
	if err := s.ApplyStampOps([]StampOp{{ID: c, Set: true, Round: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyStampOps([]StampOp{
		{ID: a, Set: true, Round: 5},
		{ID: b, Set: true, Round: 5},
		{ID: c, Set: false},
	}); err != nil {
		t.Fatal(err)
	}
	stamps, err := s.ColdStamps()
	if err != nil {
		t.Fatal(err)
	}
	if stamps[a] == nil || *stamps[a] != 5 || stamps[b] == nil || *stamps[b] != 5 {
		t.Fatalf("set stamps = %v", stamps)
	}
	if stamps[c] != nil {
		t.Fatalf("cleared stamp still set: %v", *stamps[c])
	}
}

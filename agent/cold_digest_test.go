package agent

import (
	"testing"

	"github.com/Autumn-27/artex/db"
)

// coldDigestsRecent must produce identical output whether member ids come from
// a precomputed edge-derived map (zero extra queries, the hot path) or the
// per-digest DigestMembers fallback.
func TestColdDigestsRecentMemberOf(t *testing.T) {
	s := guardTaskStore(t)

	fact := func(text string) int64 {
		return addGuardNode(t, s, db.KindFact, map[string]any{"text": text}, "confirmed")
	}
	m1, m2 := fact("older member"), fact("newer member")
	dOld, err := s.AddDigest(map[string]any{"body": "older digest"}, []int64{m1})
	if err != nil {
		t.Fatal(err)
	}
	dNew, err := s.AddDigest(map[string]any{"body": "newer digest"}, []int64{m2})
	if err != nil {
		t.Fatal(err)
	}

	memberOf := map[int64][]int64{dOld: {m1}, dNew: {m2}}
	shown, more := coldDigestsRecent(s, 5, memberOf)
	if len(shown) != 2 || len(more) != 0 {
		t.Fatalf("shown=%v more=%v", shown, more)
	}
	// Freshest-member-first ordering: dNew covers the higher member id.
	if shown[0]["id"] != dNew || shown[0]["member_count"] != 1 {
		t.Fatalf("ordering/count = %+v", shown)
	}

	fallback, _ := coldDigestsRecent(s, 5, nil)
	if len(fallback) != len(shown) || fallback[0]["id"] != shown[0]["id"] || fallback[1]["id"] != shown[1]["id"] {
		t.Fatalf("nil memberOf diverged: %+v vs %+v", fallback, shown)
	}

	// Cap splits shown vs moreIDs.
	shown1, more1 := coldDigestsRecent(s, 1, memberOf)
	if len(shown1) != 1 || shown1[0]["id"] != dNew || len(more1) != 1 || more1[0] != dOld {
		t.Fatalf("cap split: shown=%v more=%v", shown1, more1)
	}
}

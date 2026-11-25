package llmpool

import (
	"testing"
	"time"
)

func TestRegistryP90TTFTAndAdaptiveHedge(t *testing.T) {
	reg := NewRegistry(nil, nil)
	if got := reg.P90TTFT(1); got != 0 {
		t.Fatalf("P90TTFT with 0 samples = %v, want 0", got)
	}
	// Record 3 samples: still below minimum sample floor (4).
	for _, d := range []time.Duration{500 * time.Millisecond, 600 * time.Millisecond, 700 * time.Millisecond} {
		reg.RecordTTFT(1, d)
	}
	if got := reg.P90TTFT(1); got != 0 {
		t.Fatalf("P90TTFT with 3 samples = %v, want 0", got)
	}
	// Record 7 more samples (total 10): 90th percentile should be 2000ms.
	for _, d := range []time.Duration{
		800 * time.Millisecond,
		900 * time.Millisecond,
		1000 * time.Millisecond,
		1100 * time.Millisecond,
		1200 * time.Millisecond,
		2000 * time.Millisecond,
		3000 * time.Millisecond,
	} {
		reg.RecordTTFT(1, d)
	}
	p90 := reg.P90TTFT(1)
	if p90 != 2000*time.Millisecond {
		t.Fatalf("P90TTFT = %v, want 2s", p90)
	}

	m0 := &Member{ID: 1, Rank: 10}
	m1Same := &Member{ID: 2, Rank: 10}
	m1Diff := &Member{ID: 3, Rank: 5}
	p := &Pool{members: []*Member{m0, m1Same}, health: reg}

	if got := p.effectiveHedgeDelay(m0, m1Same); got != 3000*time.Millisecond {
		t.Fatalf("adaptive hedge for same Rank = %v, want 3s (1.5x P90)", got)
	}
	if got := p.effectiveHedgeDelay(m0, m1Diff); got != 0 {
		t.Fatalf("adaptive hedge for different Rank = %v, want 0", got)
	}
	p.SetHedgeDelay(0)
	if got := p.effectiveHedgeDelay(m0, m1Same); got != 0 {
		t.Fatalf("explicitly disabled hedge = %v, want 0", got)
	}
}

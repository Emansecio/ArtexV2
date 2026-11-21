package llm

import "testing"

func TestBuildAnthropicSystemCacheBoundary(t *testing.T) {
	sys := []string{"a", "b", "c"}
	cc := anthropicCacheControl("")

	cases := []struct {
		name       string
		boundary   int
		cc         map[string]any
		wantBlocks int
		wantCached []bool // per-block: has cache_control
	}{
		{"no boundary (0) with cache enabled → whole thing cached", 0, cc, 1, []bool{true}},
		{"negative with cache enabled → whole thing cached", -1, cc, 1, []bool{true}},
		{"no boundary (0) with cache disabled → single uncached", 0, nil, 1, []bool{false}},
		{"mid split → cached prefix + dynamic tail", 2, cc, 2, []bool{true, false}},
		{"boundary == len → whole thing cached", 3, cc, 1, []bool{true}},
		{"boundary > len → whole thing cached", 5, cc, 1, []bool{true}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			blocks := buildAnthropicSystem(CompletionRequest{System: sys, DynamicBoundary: c.boundary}, c.cc)
			if len(blocks) != c.wantBlocks {
				t.Fatalf("blocks=%d want %d", len(blocks), c.wantBlocks)
			}
			for i, wantCached := range c.wantCached {
				gotCached := blocks[i].CacheControl != nil
				if gotCached != wantCached {
					t.Fatalf("block %d cached=%v want %v", i, gotCached, wantCached)
				}
			}
		})
	}
}

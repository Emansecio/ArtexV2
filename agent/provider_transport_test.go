package agent

import (
	"testing"
	"time"
)

func TestSharedLLMTransportHTTP2PingAndIdle(t *testing.T) {
	tr, err := sharedLLMTransport("")
	if err != nil {
		t.Fatalf("sharedLLMTransport: %v", err)
	}
	if tr.IdleConnTimeout != 5*time.Minute {
		t.Errorf("IdleConnTimeout=%v, want 5m", tr.IdleConnTimeout)
	}
	if tr.HTTP2 == nil || tr.HTTP2.SendPingTimeout != 15*time.Second || tr.HTTP2.PingTimeout != 5*time.Second {
		t.Errorf("HTTP2=%+v, want SendPingTimeout=15s PingTimeout=5s", tr.HTTP2)
	}
}

func TestSharedLLMTransportPromptCacheTTLEnv(t *testing.T) {
	for _, tc := range []struct {
		env  string
		want string
	}{
		{"", ""},
		{"1h", "1h"},
		{" 1H ", "1h"},
		{"5m", "5m"},
		{" 5M ", "5m"},
		{"2h", ""},
	} {
		t.Setenv("ARTEX_PROMPT_CACHE_TTL", tc.env)
		if got := promptCacheTTL(); got != tc.want {
			t.Errorf("ARTEX_PROMPT_CACHE_TTL=%q: promptCacheTTL()=%q, want %q", tc.env, got, tc.want)
		}
	}
}

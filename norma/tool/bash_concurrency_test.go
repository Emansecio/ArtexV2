package tool

import (
	"encoding/json"
	"testing"
)

func TestBashConcurrencySafe(t *testing.T) {
	check := func(cmd string, bg bool, want bool) {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{"command": cmd, "run_in_background": bg})
		if got := bashConcurrencySafe(raw); got != want {
			t.Fatalf("cmd=%q bg=%v -> got %v, want %v", cmd, bg, got, want)
		}
	}

	// Read-only probes allowed in parallel.
	check(`curl -sI https://example.com`, false, true)
	check(`curl -s -X HEAD https://example.com/api | grep -i server | head -n 5`, false, true)
	check(`dig +short A example.com`, false, true)
	check(`openssl s_client -connect example.com:443 -servername example.com`, false, true)
	check(`jq '.paths | keys' swagger.json`, false, true)

	// Mutations, chaining, redirection, subshells, backgrounding must serialize.
	check(`curl -s https://example.com -o out.html`, false, false)
	check(`curl -X POST https://example.com -d '{"a":1}'`, false, false)
	check(`curl -s https://example.com > body.txt`, false, false)
	check(`dig example.com && rm -f tmp`, false, false)
	check(`echo $(id)`, false, false)
	check(`sed -i 's/a/b/' file.txt`, false, false)
	check(`curl -s https://example.com`, true, false)
	check("curl -s https://example.com\nid", false, false)
}

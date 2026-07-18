package shares

import "testing"

// TestParseWritebackConfig covers the per-share metadata writeback flag (#1757):
// absent or non-bool -> false (durable default), explicit bool honored.
func TestParseWritebackConfig(t *testing.T) {
	cases := []struct {
		name   string
		config map[string]any
		want   bool
	}{
		{"absent", map[string]any{}, false},
		{"true", map[string]any{"writeback": true}, true},
		{"false", map[string]any{"writeback": false}, false},
		{"non-bool", map[string]any{"writeback": "yes"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseWritebackConfig(tc.config, "/share"); got != tc.want {
				t.Fatalf("parseWritebackConfig(%v) = %v, want %v", tc.config, got, tc.want)
			}
		})
	}
}

// TestParseRequireDurableCommit covers the block strict-CLOSE/COMMIT flag (#1274):
// absent or non-bool -> false (default), explicit bool honored.
func TestParseRequireDurableCommit(t *testing.T) {
	cases := []struct {
		name   string
		config map[string]any
		want   bool
	}{
		{"absent", map[string]any{}, false},
		{"true", map[string]any{"require_durable_commit": true}, true},
		{"false", map[string]any{"require_durable_commit": false}, false},
		{"non-bool", map[string]any{"require_durable_commit": 1}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseRequireDurableCommit(tc.config, "/share"); got != tc.want {
				t.Fatalf("parseRequireDurableCommit(%v) = %v, want %v", tc.config, got, tc.want)
			}
		})
	}
}

// TestResolveDurabilityTier covers the composed durability enum (#1758): the
// "durability" tier selects the two underlying knobs; when absent the raw bools
// are honored; when present it is authoritative (raw bools ignored).
func TestResolveDurabilityTier(t *testing.T) {
	cases := []struct {
		name    string
		config  map[string]any
		wantWB  bool
		wantRDC bool
	}{
		// Named tiers.
		{"local", map[string]any{"durability": "local"}, false, false},
		{"writeback", map[string]any{"durability": "writeback"}, true, false},
		{"remote", map[string]any{"durability": "remote"}, false, true},
		{"empty-string", map[string]any{"durability": ""}, false, false},
		{"case-and-space", map[string]any{"durability": "  Remote "}, false, true},
		{"unknown", map[string]any{"durability": "bogus"}, false, false},
		{"non-string", map[string]any{"durability": 3}, false, false},
		// durability is authoritative — raw bools ignored when it is present.
		{"tier-overrides-raw", map[string]any{"durability": "local", "writeback": true, "require_durable_commit": true}, false, false},
		{"remote-overrides-writeback-bool", map[string]any{"durability": "remote", "writeback": true}, false, true},
		// Backward compatibility: no durability key -> raw bools honored.
		{"legacy-writeback", map[string]any{"writeback": true}, true, false},
		{"legacy-require-durable", map[string]any{"require_durable_commit": true}, false, true},
		{"legacy-both", map[string]any{"writeback": true, "require_durable_commit": true}, true, true},
		{"legacy-none", map[string]any{}, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotWB, gotRDC := resolveDurabilityTier(tc.config, "/share")
			if gotWB != tc.wantWB || gotRDC != tc.wantRDC {
				t.Fatalf("resolveDurabilityTier(%v) = (wb=%v, rdc=%v), want (wb=%v, rdc=%v)",
					tc.config, gotWB, gotRDC, tc.wantWB, tc.wantRDC)
			}
		})
	}
}

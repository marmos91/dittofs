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

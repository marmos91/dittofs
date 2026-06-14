package memory

import "testing"

// TestChildPageStart exercises the shared READDIR cursor positioning helper
// directly, including the regression case it was introduced for: a cursor whose
// entry was deleted between pages must resume after the cursor's lexicographic
// position rather than restarting from zero (which would replay entries).
func TestChildPageStart(t *testing.T) {
	sorted := []string{"a", "b", "c", "d", "e"}

	tests := []struct {
		name   string
		names  []string
		cursor string
		want   int
	}{
		{"empty cursor starts at zero", sorted, "", 0},
		{"present cursor resumes after it", sorted, "b", 2},
		{"present last cursor resumes past end", sorted, "e", 5},
		{"deleted middle cursor resumes at next entry", []string{"a", "c", "d", "e"}, "b", 1},
		{"deleted cursor before first entry", []string{"b", "c"}, "a", 0},
		{"cursor after all entries", sorted, "z", 5},
		{"empty list", nil, "b", 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := childPageStart(tc.names, tc.cursor); got != tc.want {
				t.Errorf("childPageStart(%v, %q) = %d, want %d", tc.names, tc.cursor, got, tc.want)
			}
		})
	}
}

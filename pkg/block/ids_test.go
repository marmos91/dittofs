package block

import "testing"

func TestParseChunkOffset(t *testing.T) {
	cases := []struct {
		name   string
		id     string
		wantV  uint64
		wantOK bool
	}{
		{"simple", "share/file/0", 0, true},
		{"non-zero", "share/file/12345", 12345, true},
		{"max-digits", "p/18446744073709551615", 18446744073709551615, true},
		{"nested-slashes", "a/b/c/42", 42, true},

		{"no-slash", "noslash", 0, false},
		{"empty", "", 0, false},
		{"trailing-slash", "p/", 0, false},
		{"non-digit", "p/12a", 0, false},
		{"leading-space", "p/ 12", 0, false},
		{"negative-sign", "p/-1", 0, false},
		{"plus-sign", "p/+1", 0, false},
		{"hex", "p/0x10", 0, false},
		{"only-slash", "/", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotV, gotOK := ParseChunkOffset(tc.id)
			if gotOK != tc.wantOK {
				t.Fatalf("ok = %v, want %v", gotOK, tc.wantOK)
			}
			if gotV != tc.wantV {
				t.Fatalf("v = %d, want %d", gotV, tc.wantV)
			}
		})
	}
}

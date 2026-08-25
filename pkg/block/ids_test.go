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
		{"max-int64", "p/9223372036854775807", 9223372036854775807, true},
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

		// Callers convert the result to int64 to compare against file offsets,
		// so a value the uint64 parse accepts but int64 cannot hold is rejected
		// here rather than arriving negative at the comparison.
		{"above-int64-max", "p/9223372036854775808", 0, false},
		{"max-uint64", "p/18446744073709551615", 0, false},
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

func TestChunkOffsetFor(t *testing.T) {
	cases := []struct {
		name      string
		id        string
		payloadID string
		wantV     uint64
		wantOK    bool
	}{
		{"simple", "p/0", "p", 0, true},
		{"non-zero", "p/12345", "p", 12345, true},
		{"max-int64", "p/9223372036854775807", "p", 9223372036854775807, true},

		// Stricter than ParseChunkOffset: the component must belong to this
		// payload, not to one nested beneath it.
		{"nested-payload", "p/q/42", "p", 0, false},
		{"other-payload", "q/42", "p", 0, false},
		{"prefix-not-boundary", "pp/42", "p", 0, false},

		{"trailing-slash", "p/", "p", 0, false},
		{"non-digit", "p/12a", "p", 0, false},
		{"negative-sign", "p/-1", "p", 0, false},

		// Same int64 ceiling as ParseChunkOffset: callers narrow the result.
		{"above-int64-max", "p/9223372036854775808", "p", 0, false},
		{"max-uint64", "p/18446744073709551615", "p", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotV, gotOK := ChunkOffsetFor(tc.id, tc.payloadID)
			if gotOK != tc.wantOK {
				t.Fatalf("ok = %v, want %v", gotOK, tc.wantOK)
			}
			if gotV != tc.wantV {
				t.Fatalf("v = %d, want %d", gotV, tc.wantV)
			}
		})
	}
}

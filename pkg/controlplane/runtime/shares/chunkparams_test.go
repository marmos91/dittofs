package shares

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/block/chunker"
)

// journalChunkParams degrades rather than failing: a chunk size that is missing
// or below the chunker's floor keeps the default profile, because a bad tuning
// value must not stop a share from starting. Reads never re-chunk, so a bad
// profile applied to newly written data would outlive the misconfiguration.
func TestJournalChunkParams(t *testing.T) {
	for _, tc := range []struct {
		name     string
		defaults LocalStoreDefaults
		wantOK   bool
		want     chunker.Params
	}{
		{"absent", LocalStoreDefaults{}, false, chunker.Params{}},
		{"below the chunker floor", LocalStoreDefaults{ChunkSize: 100}, false, chunker.Params{}},
		{
			"valid derives avg and max",
			LocalStoreDefaults{ChunkSize: 128 << 10},
			true,
			chunker.Params{Min: 128 << 10, Avg: 512 << 10, Max: 1 << 20},
		},
		{
			"chunk_max caps the ceiling and clamps avg under it",
			LocalStoreDefaults{ChunkSize: 128 << 10, ChunkMax: 256 << 10},
			true,
			chunker.Params{Min: 128 << 10, Avg: 256 << 10, Max: 256 << 10},
		},
		{
			"a ceiling below the minimum is unsatisfiable and drops the profile",
			LocalStoreDefaults{ChunkSize: 128 << 10, ChunkMax: 1},
			false,
			chunker.Params{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := journalChunkParams(&tc.defaults)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (params %+v)", ok, tc.wantOK, got)
			}
			if ok && got != tc.want {
				t.Fatalf("params = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestMergeDefaults_UnsetJournalSizeDefersToTheJournal(t *testing.T) {
	got := mergeLocalStoreDefaults(&LocalStoreDefaults{}, &ShareConfig{})
	if got.MaxSize != 0 {
		t.Errorf("MaxSize = %d, want 0 when no journal size is configured: 0 is the "+
			"hand-off that lets the journal size its own cap off free disk space", got.MaxSize)
	}
}

func TestMergeDefaults_SetJournalSizeIsTheCeiling(t *testing.T) {
	got := mergeLocalStoreDefaults(&LocalStoreDefaults{}, &ShareConfig{JournalSize: 5 << 30})
	if got.MaxSize != 5<<30 {
		t.Errorf("MaxSize = %d, want the configured 5 GiB", got.MaxSize)
	}
}

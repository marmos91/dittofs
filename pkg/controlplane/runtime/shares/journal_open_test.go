package shares

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block/chunker"
)

// Two shares must never land in the same directory: they would interleave
// writes into one journal.
func TestShareJournalDir_SeparatesShares(t *testing.T) {
	a := ShareJournalDir("/srv/blocks", "/alpha")
	b := ShareJournalDir("/srv/blocks", "/beta")
	if a == b {
		t.Fatalf("two shares resolved to the same directory: %s", a)
	}
	for _, got := range []string{a, b} {
		if !strings.HasPrefix(got, filepath.Join("/srv/blocks", "shares")) {
			t.Errorf("share dir %q must sit under <root>/shares", got)
		}
	}
}

// The share name reaches the filesystem here, so a name containing traversal
// must not escape the root.
func TestShareJournalDir_ContainsTraversalNames(t *testing.T) {
	got := ShareJournalDir("/srv/blocks", "../../etc")
	if !strings.HasPrefix(filepath.Clean(got), "/srv/blocks") {
		t.Errorf("share dir %q escaped the journal root", got)
	}
}

func TestShareJournalDir_EmptyRootYieldsEmpty(t *testing.T) {
	if got := ShareJournalDir("", "/alpha"); got != "" {
		t.Errorf("got %q, want empty when no root is configured", got)
	}
}

func TestOpenShareJournal_RefusesWithoutARoot(t *testing.T) {
	if _, err := OpenShareJournal("/alpha", &LocalStoreDefaults{}); err == nil {
		t.Fatal("got nil, want an error when no journal root is configured")
	}
	if _, err := OpenShareJournal("/alpha", nil); err == nil {
		t.Fatal("got nil, want an error for nil defaults")
	}
}

func TestOpenShareJournal_OpensUnderTheRoot(t *testing.T) {
	root := t.TempDir()
	s, err := OpenShareJournal("/alpha", &LocalStoreDefaults{
		JournalRoot:         root,
		BackpressureMaxWait: time.Second,
	})
	if err != nil {
		t.Fatalf("OpenShareJournal: %v", err)
	}
	defer func() { _ = s.Close() }()

	want := filepath.Join(ShareJournalDir(root, "/alpha"), "journal")
	if _, statErr := filepath.Glob(want); statErr != nil {
		t.Fatalf("glob %s: %v", want, statErr)
	}
}

func TestJournalChunkParams_DerivesTheProfile(t *testing.T) {
	cp, ok := journalChunkParams(&LocalStoreDefaults{ChunkSize: 131072})
	if !ok {
		t.Fatal("got ok=false, want a derived profile")
	}
	if cp.Min != 131072 || cp.Avg != 131072*4 || cp.Max != 131072*8 {
		t.Errorf("derived profile = %+v, want min/4x/8x", cp)
	}
}

func TestJournalChunkParams_ChunkMaxOverridesTheCeiling(t *testing.T) {
	// Must exceed the derived average (4x the minimum) or the profile is
	// invalid and correctly dropped.
	cp, ok := journalChunkParams(&LocalStoreDefaults{ChunkSize: 131072, ChunkMax: 1048576})
	if !ok {
		t.Fatal("got ok=false, want a derived profile")
	}
	if cp.Max != 1048576 {
		t.Errorf("Max = %d, want the configured override 1048576", cp.Max)
	}
}

func TestJournalChunkParams_UnsetKeepsTheDefaultProfile(t *testing.T) {
	if _, ok := journalChunkParams(&LocalStoreDefaults{}); ok {
		t.Error("got ok=true for an unset chunk size; the built-in profile must stand")
	}
}

// Reads never re-chunk, so an invalid profile applied to newly written data
// outlives the misconfiguration. It must be dropped, not clamped.
func TestJournalChunkParams_InvalidProfileIsDropped(t *testing.T) {
	cp, ok := journalChunkParams(&LocalStoreDefaults{ChunkSize: 131072, ChunkMax: 1})
	if ok {
		t.Errorf("got ok=true for an invalid profile %+v; want it dropped", cp)
	}
	if cp != (chunker.Params{}) {
		t.Errorf("got %+v, want the zero profile when dropped", cp)
	}
}

// TestShareJournalDir_KeepsEveryShareUnderTheSharesDirectory pins the one name
// shape that escaped it: dots survive escaping, so "/.." used to resolve to the
// journal root itself.
func TestShareJournalDir_KeepsEveryShareUnderTheSharesDirectory(t *testing.T) {
	const root = "/srv/blocks"
	container := root + "/shares"

	for _, name := range []string{"/..", "/.", "/../../etc", "/a/b", "/normal", "/"} {
		got := ShareJournalDir(root, name)
		if got != container && !strings.HasPrefix(got, container+"/") {
			t.Errorf("ShareJournalDir(%q) = %q, which is outside %q", name, got, container)
		}
	}
}

// TestShareJournalDir_DistinctNamesGetDistinctDirectories guards the escaping
// from collapsing two shares onto one journal.
func TestShareJournalDir_DistinctNamesGetDistinctDirectories(t *testing.T) {
	const root = "/srv/blocks"
	seen := map[string]string{}
	for _, name := range []string{"/..", "/.", "/a", "/a/b", "/normal"} {
		got := ShareJournalDir(root, name)
		if prev, dup := seen[got]; dup {
			t.Errorf("shares %q and %q both resolve to %q", prev, name, got)
		}
		seen[got] = name
	}
}

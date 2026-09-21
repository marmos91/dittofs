package shares

import (
	"context"
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
	root := filepath.Join(string(filepath.Separator), "srv", "blocks")
	got := ShareJournalDir(root, "../../etc")
	if !strings.HasPrefix(filepath.Clean(got), root) {
		t.Errorf("share dir %q escaped the journal root", got)
	}
}

func TestShareJournalDir_EmptyRootYieldsEmpty(t *testing.T) {
	if got := ShareJournalDir("", "/alpha"); got != "" {
		t.Errorf("got %q, want empty when no root is configured", got)
	}
}

// journalDefaults points a share's journal at a temp dir and releases the
// journals the service opens beneath it when the test ends. A share holds its
// journal open for as long as it is registered, and the release is registered
// after the temp dir so cleanup's reverse order closes them before the dir is
// removed — a platform that refuses to unlink an open file cannot remove the
// root otherwise. CloseBlockStores is idempotent, so repeated use in one test
// is harmless — but it is also terminal for the service, which afterwards
// refuses to add or rebind a share. Give each subtest its own Service rather
// than sharing one across t.Run boundaries, or the first cleanup to fire
// retires it for the rest.
func journalDefaults(t *testing.T, svc *Service) *LocalStoreDefaults {
	t.Helper()
	root := t.TempDir()
	t.Cleanup(func() { svc.CloseBlockStores(context.Background()) })
	return &LocalStoreDefaults{JournalRoot: root}
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
	root := filepath.Join(string(filepath.Separator), "srv", "blocks")
	container := filepath.Join(root, "shares")

	for _, name := range []string{"/..", "/.", "/../../etc", "/a/b", "/normal", "/"} {
		got := ShareJournalDir(root, name)
		if got != container && !strings.HasPrefix(got, container+string(filepath.Separator)) {
			t.Errorf("ShareJournalDir(%q) = %q, which is outside %q", name, got, container)
		}
	}
}

// TestShareJournalDir_DistinctNamesGetDistinctDirectories guards the escaping
// from collapsing two shares onto one journal.
func TestShareJournalDir_DistinctNamesGetDistinctDirectories(t *testing.T) {
	root := filepath.Join(string(filepath.Separator), "srv", "blocks")
	seen := map[string]string{}
	for _, name := range []string{"/..", "/.", "/a", "/a/b", "/normal"} {
		got := ShareJournalDir(root, name)
		if prev, dup := seen[got]; dup {
			t.Errorf("shares %q and %q both resolve to %q", prev, name, got)
		}
		seen[got] = name
	}
}

// The share name reaches the filesystem, so the directory is confirmed to be
// inside the root before anything is created there. The cases are built
// directly rather than through a share name: escaping already prevents them,
// and a guard that never refuses anything proves nothing.
func TestCheckUnderJournalRoot_RefusesWhatIsNotInside(t *testing.T) {
	const root = "/srv/blocks"
	for _, dir := range []string{
		"/srv/blocks",         // the root itself, beside the shares directory
		"/srv/blocksXX/alpha", // shares a string prefix but is a different tree
		"/srv/blocks/../etc",  // climbs back out
		"/etc/alpha",          // unrelated
		"/srv",                // a parent
	} {
		if err := checkUnderJournalRoot(root, dir); err == nil {
			t.Errorf("checkUnderJournalRoot(%q, %q) = nil, want a refusal", root, dir)
		}
	}
}

func TestCheckUnderJournalRoot_AcceptsWhatIsInside(t *testing.T) {
	const root = "/srv/blocks"
	for _, dir := range []string{
		"/srv/blocks/shares",
		"/srv/blocks/shares/alpha",
		"/srv/blocks/shares/a%2Fb",
	} {
		if err := checkUnderJournalRoot(root, dir); err != nil {
			t.Errorf("checkUnderJournalRoot(%q, %q) = %v, want nil", root, dir, err)
		}
	}
	// Every name the sanitizer can produce must still open the directory it
	// opens today.
	for _, name := range []string{"/..", "/.", "/../../etc", "/a/b", "/normal"} {
		dir := ShareJournalDir(root, name)
		if err := checkUnderJournalRoot(root, dir); err != nil {
			t.Errorf("share %q resolves to %q, which the guard refuses: %v", name, dir, err)
		}
	}
}

// A journal root of "/" is degenerate but legal: everything below it is
// contained, and only the root itself is not. String-prefix containment gets
// this wrong because the root already ends in a separator.
func TestCheckUnderJournalRoot_AcceptsSharesUnderTheFilesystemRoot(t *testing.T) {
	root := string(filepath.Separator)
	if dir := ShareJournalDir(root, "/export"); checkUnderJournalRoot(root, dir) != nil {
		t.Errorf("checkUnderJournalRoot(%q, %q) refused a contained directory", root, dir)
	}
	if err := checkUnderJournalRoot(root, root); err == nil {
		t.Errorf("checkUnderJournalRoot(%q, %q) = nil, want a refusal", root, root)
	}
}

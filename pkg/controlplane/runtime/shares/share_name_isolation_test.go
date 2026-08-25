package shares

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// TestAcceptedShareNamesStayInsideSharesTree pins the property that makes the
// per-share data directory an isolation boundary: every share name that
// ValidateShareName accepts must derive a directory strictly beneath
// <base>/shares/.
//
// deriveLocalStoreDir builds that path as Join(base, "shares", sanitized), and
// sanitizeShareName escapes '/' but not '.'. So the guarantee rests on the two
// functions agreeing, and they live in different packages — a later change to
// either one alone would silently reopen a name like ".." resolving the share's
// directory to base. Asserting the derived path here, rather than only the
// validator's verdict, is what catches that.
func TestAcceptedShareNamesStayInsideSharesTree(t *testing.T) {
	t.Parallel()

	// deriveLocalStoreDir requires an absolute base, and what counts as
	// absolute is platform-specific, so take one from the test environment
	// rather than hardcoding a POSIX path.
	base := t.TempDir()
	cfg := &fakeBlockStoreConfig{cfg: map[string]any{"path": base}}
	sharesRoot := filepath.Join(base, "shares")

	names := []string{
		"data", "/data", "/my.share.v2", "/share.", "/...", "/a/b",
		".", "/.", "..", "/..", "//..", "/../..", "/a/../..",
		"", "/", strings.Repeat("a", 300),
	}

	for _, name := range names {
		// t.Run renders an empty name as "#00", which says nothing about which
		// case it was; label it so a failure names the input.
		label := name
		if label == "" {
			label = "<empty>"
		}
		t.Run(label, func(t *testing.T) {
			t.Parallel()

			if err := metadata.ValidateShareName(name); err != nil {
				t.Skipf("rejected before any directory is derived: %v", err)
			}

			got := deriveLocalStoreDir(cfg, name)
			if got == "" {
				t.Fatalf("deriveLocalStoreDir(%q) = %q, want a path", name, got)
			}
			if got == sharesRoot || !strings.HasPrefix(got, sharesRoot+string(filepath.Separator)) {
				t.Fatalf("share %q derives %q, which escapes %q", name, got, sharesRoot)
			}
		})
	}
}

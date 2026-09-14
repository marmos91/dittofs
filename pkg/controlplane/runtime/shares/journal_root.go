package shares

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// ErrJournalRootMismatch reports that the shares on disk do not live under the
// configured journal root. Startup refuses rather than proceeding, so callers
// match it to produce a fatal error instead of a warning.
var ErrJournalRootMismatch = errors.New("shares do not live under the configured journal root")

// ShareJournalPath pairs a share with the directory its local storage was
// recorded under.
type ShareJournalPath struct {
	Share string
	Path  string
}

// CheckJournalRoot reports whether every share's local storage already lives
// under the configured journal root.
//
// A share's journal is the only copy of any byte that has not reached a remote
// tier, so a root that does not match the recorded location would serve a share
// whose data is somewhere else — reads would return zeros for everything
// written before the change, with nothing failing. Relocating the directories
// instead would mean moving the only copy during startup, where a partial move
// cannot be undone. Refusing is the one option that loses nothing: it names
// every share that disagrees and what to set.
//
// Shares with no recorded path contribute nothing to compare and are skipped.
// An empty configured root means the caller has not resolved one yet, which is
// a programming error rather than an operator one, so it is reported as such.
func CheckJournalRoot(configured string, found []ShareJournalPath) error {
	if configured == "" {
		return fmt.Errorf("%w: no journal root configured", ErrJournalRootMismatch)
	}
	want := filepath.Clean(configured)

	mismatched := make([]ShareJournalPath, 0, len(found))
	distinct := make(map[string]struct{})
	for _, f := range found {
		if f.Path == "" {
			continue
		}
		got := filepath.Clean(f.Path)
		distinct[got] = struct{}{}
		if got != want {
			mismatched = append(mismatched, ShareJournalPath{Share: f.Share, Path: got})
		}
	}
	if len(mismatched) == 0 {
		return nil
	}

	sort.Slice(mismatched, func(i, j int) bool { return mismatched[i].Share < mismatched[j].Share })
	var b strings.Builder
	fmt.Fprintf(&b, "\n  configured blockstore.journal.path: %s\n", want)
	for _, m := range mismatched {
		fmt.Fprintf(&b, "  share %q stores its data in: %s\n", m.Share, m.Path)
	}
	// One shared location can be adopted by pointing the config at it. Several
	// cannot be expressed at all, so say so rather than suggesting a fix that
	// would strand the other shares.
	if len(distinct) == 1 {
		var only string
		for p := range distinct {
			only = p
		}
		fmt.Fprintf(&b, "\nSet blockstore.journal.path to %s and restart.", only)
	} else {
		b.WriteString("\nThese shares are on different paths, which one journal root cannot express.\n")
		b.WriteString("Move them under a single directory, then set blockstore.journal.path to it and restart.")
	}
	// Wrap the sentinel itself: the caller matches on it to decide whether a
	// share is refused or merely skipped with a warning, and a wrapped copy of
	// some other error would make that match fail and mount the share anyway.
	return fmt.Errorf("%w%s", ErrJournalRootMismatch, b.String())
}

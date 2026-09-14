package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"gorm.io/gorm"

	"github.com/marmos91/dittofs/internal/pathutil"
)

// ErrJournalRootMismatch reports that the shares on disk do not live under the
// configured journal root. The upgrade refuses rather than proceeding, so
// callers match it to produce a fatal error instead of a warning.
var ErrJournalRootMismatch = errors.New("shares do not live under the configured journal root")

// shareJournalPath pairs a share with the directory its local storage was
// recorded under.
type shareJournalPath struct {
	Share string
	Path  string
}

// checkShareJournalRoots refuses to drop local_block_store_id while a share's
// data sits somewhere the configured journal root does not name.
//
// The column is the only route back to where a share's bytes actually are: it
// reaches the local block store config, whose "path" is the root the share's
// directory hung beneath. Once it is dropped nothing on disk still records the
// association, so this is the last moment the two can be compared.
//
// An empty root means the caller resolved none, which every embedder that
// never configured a journal does; there is nothing to compare against, so the
// comparison is skipped rather than failed.
func checkShareJournalRoots(db *gorm.DB, journalRoot string) error {
	if journalRoot == "" {
		return nil
	}

	// A share's reference normally holds the block store's UUID, but the REST
	// update path historically persisted the name instead, so match either.
	var rows []struct {
		Name   string
		Config string
	}
	if err := db.Raw(`
		SELECT s.name AS name, COALESCE(b.config, '') AS config
		FROM shares s
		JOIN block_store_configs b
		  ON b.id = s.local_block_store_id OR b.name = s.local_block_store_id
	`).Scan(&rows).Error; err != nil {
		return fmt.Errorf("failed to read share journal locations: %w", err)
	}

	found := make([]shareJournalPath, 0, len(rows))
	for _, row := range rows {
		if p := journalRootFromStoreConfig(row.Config); p != "" {
			found = append(found, shareJournalPath{Share: row.Name, Path: p})
		}
	}
	if len(found) == 0 {
		return nil
	}
	return checkJournalRoot(journalRoot, found)
}

// journalRootFromStoreConfig reads the root directory out of a local block
// store config blob the way the running system read it: the "path" key, with a
// leading ~ expanded. A store that kept no path — a memory store — has no
// location to compare and yields "".
func journalRootFromStoreConfig(blob string) string {
	if strings.TrimSpace(blob) == "" {
		return ""
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(blob), &cfg); err != nil {
		return ""
	}
	path, _ := cfg["path"].(string)
	if path == "" {
		return ""
	}
	// A ~ that cannot be expanded is compared as written, which mismatches an
	// expanded root and refuses — the safe direction.
	if expanded, err := pathutil.ExpandPath(path); err == nil {
		return expanded
	}
	return path
}

// checkJournalRoot reports whether every share's local storage already lives
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
func checkJournalRoot(configured string, found []shareJournalPath) error {
	if configured == "" {
		return fmt.Errorf("%w: no journal root configured", ErrJournalRootMismatch)
	}
	want := filepath.Clean(configured)

	mismatched := make([]shareJournalPath, 0, len(found))
	distinct := make(map[string]struct{})
	for _, f := range found {
		if f.Path == "" {
			continue
		}
		got := filepath.Clean(f.Path)
		distinct[got] = struct{}{}
		if got != want {
			mismatched = append(mismatched, shareJournalPath{Share: f.Share, Path: got})
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
	// Wrap the sentinel itself: the caller matches on it to decide whether the
	// upgrade is refused or merely warned about, and a wrapped copy of some
	// other error would make that match fail and drop the column anyway.
	return fmt.Errorf("%w%s", ErrJournalRootMismatch, b.String())
}

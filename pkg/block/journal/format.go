package journal

// format.go stamps the journal directory with the on-disk layout version that
// wrote it, so a binary that finds state it does not understand refuses to open
// it instead of reading it as empty.
//
// The journal directory is not one file: it is a segment stream plus siblings
// that came later (cold.log). Every one of those additions is invisible to an
// older binary — it scans the segments it knows, finds no record for a range a
// sibling described, and serves the range as a POSIX hole. Without a stamp a
// downgrade opens cleanly, reports the right sizes, and returns zeros.
//
// Layout: <dir>/format, a JSON object
//
//	{"version":1,"written_at":"2026-07-27T10:00:00Z"}

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
)

const (
	// formatVersion is the layout this build reads and writes. Bump it whenever
	// a change makes state unreadable by the previous release — a new sibling
	// file, a moved record field, a renamed key — so that release refuses rather
	// than reads holes.
	formatVersion = 1

	formatFileName = "format"
)

// formatStamp is the on-disk <dir>/format document. Only Version is acted on;
// WrittenAt is there for an operator reading the file by hand.
type formatStamp struct {
	Version   int    `json:"version"`
	WrittenAt string `json:"written_at"`
}

// CheckFormat verifies that this build understands the journal directory's
// on-disk layout, and stamps the directory when it is not yet stamped. An
// unstamped directory predates stamping and is adopted, so every later open is
// guarded; a stamp above formatVersion fails with block.ErrFutureFormat.
//
// Call it before opening the journal: the point is to not touch state whose
// shape is unknown.
func CheckFormat(dir string) error {
	raw, err := os.ReadFile(filepath.Join(dir, formatFileName))
	if errors.Is(err, os.ErrNotExist) {
		return writeFormat(dir)
	}
	if err != nil {
		return fmt.Errorf("journal: read format stamp: %w", err)
	}
	var st formatStamp
	if err := json.Unmarshal(raw, &st); err != nil {
		// A stamp we cannot parse is state we cannot vouch for, and the whole
		// reason the stamp exists is to not guess about that.
		return fmt.Errorf("journal: parse format stamp in %s: %w", dir, err)
	}
	if st.Version > formatVersion {
		return fmt.Errorf("%w: %s is at format version %d, this build reads up to %d",
			block.ErrFutureFormat, dir, st.Version, formatVersion)
	}
	return nil
}

// writeFormat durably records this build's format version in dir. Temp file,
// fsync, rename, fsync parent: the same shape the cold log's rewrite uses, so a
// crash leaves either the previous stamp or the new one and never a torn file.
func writeFormat(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("journal: mkdir %q for format stamp: %w", dir, err)
	}
	buf, err := json.Marshal(formatStamp{
		Version:   formatVersion,
		WrittenAt: time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return fmt.Errorf("journal: encode format stamp: %w", err)
	}
	path := filepath.Join(dir, formatFileName)
	tmp := path + ".tmp"
	fd, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("journal: create format stamp temp: %w", err)
	}
	if _, err := fd.Write(buf); err != nil {
		_ = fd.Close()
		return fmt.Errorf("journal: write format stamp temp: %w", err)
	}
	if err := fd.Sync(); err != nil {
		_ = fd.Close()
		return fmt.Errorf("journal: fsync format stamp temp: %w", err)
	}
	if err := fd.Close(); err != nil {
		return fmt.Errorf("journal: close format stamp temp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("journal: rename format stamp: %w", err)
	}
	if err := fsyncDir(dir); err != nil {
		return fmt.Errorf("journal: fsync dir after format stamp: %w", err)
	}
	return nil
}

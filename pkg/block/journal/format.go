package journal

// format.go stamps the journal directory with the on-disk layout version it was
// written by, so a binary that finds state it does not understand refuses to
// open it instead of reading it as empty.
//
// The journal directory is not one file: it is a segment stream plus siblings
// that came later (cold.log). Every one of those additions is invisible to an
// older binary — it scans the segments it knows, finds no record for a range
// the sibling described, and serves the range as a POSIX hole. The result of a
// downgrade is therefore a share that opens cleanly, reports the right sizes
// and returns zeros. A stamp turns that into a refusal at open.
//
// Layout: <dir>/format, a JSON object
//
//	{"version":1,"written_by":"v0.30.0","written_at":"2026-07-27T10:00:00Z"}
//
// Only version is load-bearing; written_by and written_at exist so an operator
// staring at a directory can tell which release last touched it. The file is
// written whole through a temp+rename so a crash never leaves a half-parsed
// stamp, and it is fsynced because a stamp that does not survive the crash it
// was meant to describe guards nothing.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
)

const (
	// formatVersion is the layout this build reads and writes. It is the first
	// stamp, so it does not describe any historical format: directories written
	// before it carry no stamp at all and are adopted at open. Bump it whenever
	// a change makes state unreadable by the previous release — a new sibling
	// file, a record field, a renamed key — so that release refuses rather than
	// reads holes.
	formatVersion = 1

	formatFileName = "format"
)

// formatStamp is the on-disk <dir>/format document.
type formatStamp struct {
	Version   int    `json:"version"`
	WrittenBy string `json:"written_by"`
	WrittenAt string `json:"written_at"`
}

// CheckFormat verifies that this build understands the journal directory's
// on-disk layout, and stamps the directory when it is not yet stamped.
//
// Three outcomes:
//
//   - No stamp: a directory from a release that predates stamping, or a fresh
//     one. Not an error — it is adopted and stamped, so every later open is
//     guarded. This is why the guard only bites going forward.
//   - Stamp newer than this build: returns an error wrapping
//     block.ErrFutureFormat, which boot turns into a refusal to start.
//   - Stamp at or below this build: opens normally.
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
		return fmt.Errorf("%w: %s is format v%d, this build reads up to v%d",
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
		WrittenBy: buildVersion(),
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

// buildVersion names the release that wrote a stamp, for the operator reading
// the file by hand. Best-effort: a binary built without module information
// reports "unknown", which costs nothing since only Version is acted on.
func buildVersion() string {
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" {
		return bi.Main.Version
	}
	return "unknown"
}

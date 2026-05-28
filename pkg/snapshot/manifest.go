package snapshot

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/marmos91/dittofs/pkg/blockstore"
)

// ErrInvalidManifestLine is returned by ReadManifest when a line in the
// manifest cannot be parsed as a 32-byte hex ContentHash. Callers should
// match it with errors.Is. The wrapped error carries the offending line
// number and the underlying parse error.
var ErrInvalidManifestLine = errors.New("snapshot: invalid manifest line")

// maxManifestLine bounds the bufio.Scanner allocation on read. A valid
// line is 64 hex chars + LF (= 65 bytes); 1 MiB headroom catches
// accidentally-concatenated data without unbounded memory growth.
const maxManifestLine = 1 << 20

// WriteManifest serializes hs to w as the on-disk manifest format
// described in the package doc: one hex-encoded ContentHash per line,
// sorted ascending (the order HashSet.Sorted returns), LF-terminated.
//
// WriteManifest uses a bufio.Writer internally; the caller need not
// wrap w themselves. The buffer is flushed before WriteManifest returns.
//
// hs may be empty; an empty input produces zero bytes of output.
func WriteManifest(w io.Writer, hs *blockstore.HashSet) error {
	bw := bufio.NewWriter(w)
	for _, h := range hs.Sorted() {
		if _, err := bw.WriteString(h.String()); err != nil {
			return fmt.Errorf("snapshot: write manifest: %w", err)
		}
		if err := bw.WriteByte('\n'); err != nil {
			return fmt.Errorf("snapshot: write manifest: %w", err)
		}
	}
	if err := bw.Flush(); err != nil {
		return fmt.Errorf("snapshot: write manifest: %w", err)
	}
	return nil
}

// WriteManifestAtomic writes hs to path via the temp-file + fsync +
// rename sequence described in the package doc: the temp file lives in
// the same directory as path so the rename is atomic on the same
// filesystem. On any error before rename, the temp file is removed
// best-effort and path is left untouched. On success, the canonical
// path contains the complete manifest and no .tmp sidecar remains.
func WriteManifestAtomic(path string, hs *blockstore.HashSet) error {
	tmpPath := path + ".tmp"

	f, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("snapshot: open temp manifest: %w", err)
	}

	if err := writeAndSync(f, hs); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("snapshot: rename temp manifest: %w", err)
	}
	return nil
}

// writeAndSync runs the WriteManifest + Sync + Close sequence on f.
// Caller owns temp-file cleanup on error.
func writeAndSync(f *os.File, hs *blockstore.HashSet) error {
	if err := WriteManifest(f, hs); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("snapshot: fsync temp manifest: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("snapshot: close temp manifest: %w", err)
	}
	return nil
}

// ReadManifest parses r as the on-disk manifest format and returns a
// HashSet containing every parsed hash. Empty input yields an empty
// HashSet and a nil error (a snapshot with zero blocks is valid).
//
// Each non-terminator-stripped line must be exactly 64 hex characters;
// any other shape returns an error wrapping ErrInvalidManifestLine and
// carrying the offending 1-based line number. Trailing CR (CRLF input)
// is tolerated. Duplicate lines collapse silently — HashSet de-dupes
// by definition.
func ReadManifest(r io.Reader) (*blockstore.HashSet, error) {
	hs := blockstore.NewHashSet(0)
	sc := bufio.NewScanner(r)
	// Explicit buffer sizing: default scanner buffer is 64 KiB and grows
	// to MaxScanTokenSize (also 64 KiB) — fine for 64-char lines but the
	// explicit cap pins the memory ceiling at 1 MiB regardless of input
	// pathology.
	buf := make([]byte, 0, 128)
	sc.Buffer(buf, maxManifestLine)

	lineNum := 0
	for sc.Scan() {
		lineNum++
		line := strings.TrimRight(sc.Text(), "\r")
		h, err := blockstore.ParseContentHash(line)
		if err != nil {
			return nil, fmt.Errorf("%w: line %d: %v", ErrInvalidManifestLine, lineNum, err)
		}
		hs.Add(h)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("snapshot: read manifest: %w", err)
	}
	return hs, nil
}

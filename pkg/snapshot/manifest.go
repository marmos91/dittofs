package snapshot

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/marmos91/dittofs/pkg/block"
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

// WriteManifest serializes hs to w as the on-disk manifest format: one
// hex-encoded ContentHash per line, sorted ascending, LF-terminated.
// The internal bufio.Writer is flushed before returning. Empty input
// produces zero bytes of output.
func WriteManifest(w io.Writer, hs *block.HashSet) error {
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

// WriteManifestAtomic writes hs to path via temp-file + fsync + rename
// in the same directory so the rename is atomic on the same filesystem.
// On error before rename, the temp file is removed and path is left
// untouched.
func WriteManifestAtomic(path string, hs *block.HashSet) error {
	tmpPath := path + ".tmp"

	f, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("snapshot: open temp manifest: %w", err)
	}

	writeErr := WriteManifest(f, hs)
	if writeErr == nil {
		if err := f.Sync(); err != nil {
			writeErr = fmt.Errorf("snapshot: fsync temp manifest: %w", err)
		}
	}
	if err := f.Close(); err != nil && writeErr == nil {
		writeErr = fmt.Errorf("snapshot: close temp manifest: %w", err)
	}
	if writeErr != nil {
		_ = os.Remove(tmpPath)
		return writeErr
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("snapshot: rename temp manifest: %w", err)
	}
	return nil
}

// ReadManifest parses r as the on-disk manifest format and returns a
// HashSet of every parsed hash. Empty input yields an empty HashSet
// and a nil error. Each line must be exactly 64 hex characters; any
// other shape returns an error wrapping ErrInvalidManifestLine with
// the offending 1-based line number. Trailing CR (CRLF input) is
// tolerated. Duplicate lines collapse silently.
func ReadManifest(r io.Reader) (*block.HashSet, error) {
	hs := block.NewHashSet(0)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 128), maxManifestLine)

	lineNum := 0
	for sc.Scan() {
		lineNum++
		line := strings.TrimRight(sc.Text(), "\r")
		h, err := block.ParseContentHash(line)
		if err != nil {
			return nil, fmt.Errorf("%w: line %d: %v", ErrInvalidManifestLine, lineNum, err)
		}
		hs.Add(h)
	}
	if err := sc.Err(); err != nil {
		// bufio.Scanner errors during tokenization (e.g. line exceeds
		// maxManifestLine) are line-shape failures from the caller's
		// perspective; surface them through the same sentinel so callers
		// can rely on errors.Is(err, ErrInvalidManifestLine).
		return nil, fmt.Errorf("%w: after line %d: %v", ErrInvalidManifestLine, lineNum, err)
	}
	return hs, nil
}

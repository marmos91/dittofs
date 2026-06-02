package snapshot

import (
	"bufio"
	"fmt"
	"io"
	"os"

	"github.com/marmos91/dittofs/pkg/block"
)

// WriteMetadataDumpAtomic invokes write against a temp file (path + ".tmp"),
// fsyncs and closes it, then atomically renames it to path. On any error
// from write, fsync, close, or rename, the temp file is removed and path
// is left untouched. The HashSet returned by write is propagated to the
// caller unchanged.
//
// This mirrors the temp+fsync+rename pattern used by WriteManifestAtomic:
// any concurrent stat of path sees either the previous
// content (or absence) or the fully-written new content — never the
// half-written intermediate.
//
// Empty writes are valid: a zero-byte file at path is the expected
// outcome when the supplied callback writes nothing.
//
// The runtime snapshot orchestration uses this to
// invoke Backupable.Backup against the temp file, capturing the returned
// HashSet for the subsequent WriteManifestAtomic step.
func WriteMetadataDumpAtomic(
	path string,
	write func(io.Writer) (*block.HashSet, error),
) (*block.HashSet, error) {
	tmpPath := path + ".tmp"

	f, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("snapshot: open temp metadata dump: %w", err)
	}

	bw := bufio.NewWriter(f)
	hs, writeErr := write(bw)
	if writeErr == nil {
		if err := bw.Flush(); err != nil {
			writeErr = fmt.Errorf("snapshot: flush temp metadata dump: %w", err)
		}
	}
	if writeErr == nil {
		if err := f.Sync(); err != nil {
			writeErr = fmt.Errorf("snapshot: fsync temp metadata dump: %w", err)
		}
	}
	if err := f.Close(); err != nil && writeErr == nil {
		writeErr = fmt.Errorf("snapshot: close temp metadata dump: %w", err)
	}
	if writeErr != nil {
		_ = os.Remove(tmpPath)
		return nil, writeErr
	}

	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return nil, fmt.Errorf("snapshot: rename temp metadata dump: %w", err)
	}
	return hs, nil
}

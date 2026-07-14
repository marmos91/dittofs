package fs

import (
	"context"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"lukechampine.com/blake3"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/block"
)

// MigrateLegacyChunkFiles imports every pre-flip per-chunk file under
// <baseDir>/blocks/<hh>/<hh>/<hex> into the log-blob substrate and deletes the
// file. It is the local half of the one-shot cas→blocks migration (#1493 PR4)
// and the ONLY code that still understands the legacy per-chunk layout.
//
// The routine is idempotent and resumable: chunks already present in the
// local chunk index are deduped (their file is simply removed), and a crash
// mid-run leaves the remaining files for the next boot. Each file's bytes are
// BLAKE3-verified against its path-derived hash before import; a mismatching
// file is corrupt legacy data — it is left in place, logged at Error, and
// counted, so the operator can inspect it (reads of that chunk would have
// failed verification on the old path too).
//
// Called from engine.Store.Start (blocking, before the share serves). Returns
// the number of chunks imported (dedup hits count as imported: the file was
// consumed either way).
// HasLogBlobSubstrate reports whether the store can receive migrated chunks
// (log-blob manager + local chunk index wired). The engine probes this before
// running the local migration phase so index-less fixtures skip it.
func (bc *FSStore) HasLogBlobSubstrate() bool {
	return bc.logBlob != nil && bc.localChunkIndex != nil
}

func (bc *FSStore) MigrateLegacyChunkFiles(ctx context.Context) (int, error) {
	if bc.isClosed() {
		return 0, ErrStoreClosed
	}
	if bc.logBlob == nil || bc.localChunkIndex == nil {
		return 0, fmt.Errorf("legacy chunk files cannot be migrated: log-blob substrate not wired")
	}

	blocksDir := filepath.Join(bc.baseDir, "blocks")
	if _, err := os.Stat(blocksDir); os.IsNotExist(err) {
		return 0, nil // nothing legacy on disk
	}

	var migrated, corrupt int
	walkErr := filepath.WalkDir(blocksDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if len(name) != block.HashSize*2 {
			return nil // not a chunk file (tmp leftovers etc.) — ignore
		}
		raw, herr := hex.DecodeString(name)
		if herr != nil || len(raw) != block.HashSize {
			return nil
		}
		var h block.ContentHash
		copy(h[:], raw)

		if err := bc.importLegacyChunkFile(ctx, h, path); err != nil {
			if err == errLegacyChunkCorrupt {
				corrupt++
				return nil // logged inside; leave the file for inspection
			}
			return err
		}
		migrated++
		return nil
	})
	if walkErr != nil {
		return migrated, fmt.Errorf("migrate legacy chunk files: %w", walkErr)
	}

	pruneEmptyShardDirs(blocksDir)

	if migrated > 0 || corrupt > 0 {
		logger.Info("legacy per-chunk files migrated to log-blob substrate",
			"migrated", migrated, "corrupt_skipped", corrupt, "dir", blocksDir)
	}
	return migrated, nil
}

// errLegacyChunkCorrupt is an internal sentinel: the legacy file's bytes do
// not hash to its path-derived name. Never escapes MigrateLegacyChunkFiles.
var errLegacyChunkCorrupt = fmt.Errorf("legacy chunk file corrupt")

// importLegacyChunkFile moves one legacy per-chunk file into the log-blob
// substrate: verify → append+index (deduped against the index) → remove the
// file (with diskUsed + LRU bookkeeping). It deliberately does NOT go through
// StoreChunk: StoreChunk's HasChunk existence probe would see the legacy file
// itself and no-op, after which removing the file would lose the bytes.
func (bc *FSStore) importLegacyChunkFile(ctx context.Context, h block.ContentHash, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read legacy chunk %s: %w", h, err)
	}
	if block.ContentHash(blake3.Sum256(data)) != h {
		logger.Error("legacy chunk file failed BLAKE3 verification — left in place",
			"hash", h.String(), "path", path)
		return errLegacyChunkCorrupt
	}

	// Dedup: already log-blob-resident (e.g. re-run after a crash between
	// index commit and file removal) — just consume the file.
	_, indexed, err := bc.localChunkIndex.GetLocalLocation(ctx, h)
	if err != nil {
		return fmt.Errorf("legacy chunk %s: index probe: %w", h, err)
	}
	if !indexed {
		if err := bc.storeChunkLogBlob(ctx, h, data); err != nil {
			return fmt.Errorf("legacy chunk %s: import: %w", h, err)
		}
	}

	st, statErr := os.Stat(path)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("legacy chunk %s: remove: %w", h, err)
	}
	if statErr == nil && st.Size() > 0 {
		bc.diskUsed.Add(-st.Size())
	}
	return nil
}

// pruneEmptyShardDirs best-effort removes the now-empty <hh>/<hh> shard
// directories (and blocks/ itself when fully drained). Failures are ignored:
// leftover empty dirs are cosmetic.
func pruneEmptyShardDirs(blocksDir string) {
	// Two shard levels deep; remove children first.
	outer, err := os.ReadDir(blocksDir)
	if err != nil {
		return
	}
	for _, o := range outer {
		if !o.IsDir() {
			continue
		}
		outerPath := filepath.Join(blocksDir, o.Name())
		inner, err := os.ReadDir(outerPath)
		if err != nil {
			continue
		}
		for _, i := range inner {
			if i.IsDir() {
				_ = os.Remove(filepath.Join(outerPath, i.Name())) // fails if non-empty
			}
		}
		_ = os.Remove(outerPath)
	}
	_ = os.Remove(blocksDir)
}

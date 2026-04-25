package fs

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/marmos91/dittofs/pkg/blockstore"
)

// chunkPath returns the content-addressed chunk path under baseDir/blocks/.
// Layout: <baseDir>/blocks/<hex[0:2]>/<hex[2:4]>/<hex> (D-11 two-level shard).
//
// Path components are derived exclusively from hex.EncodeToString(h[:]) — the
// characters are constrained to [0-9a-f], so path traversal via crafted hash
// input is not possible (threat T-10-05-01).
func (bc *FSStore) chunkPath(h blockstore.ContentHash) string {
	hex := h.String()
	return filepath.Join(bc.baseDir, "blocks", hex[0:2], hex[2:4], hex)
}

// StoreChunk writes data under its content-addressed path. Atomic via
// .tmp + rename; fsyncs the chunk file and the containing directory so the
// rename is durable (D-12 step 1 CAS durability, torn-write safety —
// threat T-10-05-02).
//
// Idempotent: if the chunk already exists (HasChunk returns true for h),
// StoreChunk is a no-op and returns nil. This is what lets the rollup pool
// (plan 06) retry safely after a crash between StoreChunk and CommitChunks.
//
// Caller is responsible for asserting that BLAKE3(data) == h before calling;
// this method trusts its inputs (threat T-10-05-03 accept). The rollup pool
// is the only production caller.
func (bc *FSStore) StoreChunk(ctx context.Context, h blockstore.ContentHash, data []byte) error {
	if bc.isClosed() {
		return ErrStoreClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	exists, err := bc.HasChunk(ctx, h)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}

	path := bc.chunkPath(h)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("chunkstore: mkdir: %w", err)
	}

	// Use a unique temp filename per attempt so two concurrent StoreChunk
	// calls for the same hash (whether on Unix or Windows) do not race on
	// the same .tmp file. The destination is content-addressed and idempotent;
	// if the rename target already exists from a winning concurrent call,
	// treat that as success after re-stating the destination.
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("chunkstore: create tmp: %w", err)
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("chunkstore: write tmp: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("chunkstore: fsync tmp: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("chunkstore: close tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		// On Windows, os.Rename fails if the destination already exists.
		// CAS writes are idempotent — if the destination is already there
		// with the same content (a concurrent winner stored the same hash),
		// treat that as success and clean up our tmp.
		if _, statErr := os.Stat(path); statErr == nil {
			_ = os.Remove(tmp)
			return nil
		}
		_ = os.Remove(tmp)
		return fmt.Errorf("chunkstore: rename: %w", err)
	}
	// Fsync the parent dir so the rename durably reaches stable storage.
	// Best-effort: a failing dir fsync does not invalidate the data (the file
	// is fully written + fsynced above); log-free to match flush.go's
	// syncFile posture on read-only dir handles.
	if dir, err := os.Open(filepath.Dir(path)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	bc.diskUsed.Add(int64(len(data)))
	return nil
}

// ReadChunk returns the bytes of the chunk addressed by h.
// Returns blockstore.ErrChunkNotFound if the chunk is absent.
func (bc *FSStore) ReadChunk(ctx context.Context, h blockstore.ContentHash) ([]byte, error) {
	if bc.isClosed() {
		return nil, ErrStoreClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path := bc.chunkPath(h)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, blockstore.ErrChunkNotFound
		}
		return nil, fmt.Errorf("chunkstore: open: %w", err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("chunkstore: read: %w", err)
	}
	return data, nil
}

// HasChunk reports whether the chunk exists in the local chunk store.
// Returns (true, nil) for an existing chunk, (false, nil) for a missing
// chunk, or (false, err) for any I/O error other than ENOENT.
func (bc *FSStore) HasChunk(ctx context.Context, h blockstore.ContentHash) (bool, error) {
	if bc.isClosed() {
		return false, ErrStoreClosed
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	_, err := os.Stat(bc.chunkPath(h))
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, fmt.Errorf("chunkstore: stat: %w", err)
}

// DeleteChunk removes the chunk file. Treats missing-file as success
// (matches DeleteBlockFile semantics in manage.go). Decrements diskUsed by
// the deleted file's size.
//
// Phase 10 does not call DeleteChunk from a live code path; the method
// exists for conformance tests and Phase 11's mark-sweep GC.
func (bc *FSStore) DeleteChunk(ctx context.Context, h blockstore.ContentHash) error {
	if bc.isClosed() {
		return ErrStoreClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	path := bc.chunkPath(h)
	st, statErr := os.Stat(path)
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("chunkstore: remove: %w", err)
	}
	if statErr == nil && st.Size() > 0 {
		bc.diskUsed.Add(-st.Size())
	}
	return nil
}

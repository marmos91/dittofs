package fs

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/blockstore/local"
)

// Flush writes all dirty in-memory blocks for a file to disk as .blk files.
// Called on NFS COMMIT to ensure data reaches stable storage before responding
// to the client. Each flushed block stays in BlockStatePending (post-Phase-11
// collapse: Pending replaces the legacy Local state), meaning it is on disk
// and ready for the syncer to claim and upload.
//
// Returns the list of blocks that were flushed. fsync is requested here
// (the COMMIT durability point) and propagated to flushBlock via the
// withFsync flag. Pressure-driven and block-fill paths flush without fsync
// to avoid a per-block fsync penalty on the write hot path; durability is
// established later by an explicit COMMIT or the chunkstore rollup.
func (bc *FSStore) Flush(ctx context.Context, payloadID string) ([]local.FlushedBlock, error) {
	if bc.isClosed() {
		return nil, ErrStoreClosed
	}

	// Collect keys for this payloadID via secondary index (O(1) lookup).
	bc.blocksMu.RLock()
	var keys []blockKey
	if fm := bc.fileBlocks[payloadID]; fm != nil {
		keys = make([]blockKey, 0, len(fm))
		for blockIdx := range fm {
			keys = append(keys, blockKey{payloadID: payloadID, blockIdx: blockIdx})
		}
	}
	bc.blocksMu.RUnlock()

	var flushed []local.FlushedBlock

	for _, key := range keys {
		mb := bc.getMemBlock(key)
		if mb == nil {
			continue
		}
		mb.mu.RLock()
		isDirty := mb.dirty
		mb.mu.RUnlock()
		if isDirty {
			path, dataSize, err := bc.flushBlock(ctx, key.payloadID, key.blockIdx, mb, true)
			if err != nil {
				return nil, err
			}
			if path != "" {
				flushed = append(flushed, local.FlushedBlock{
					BlockIndex: key.blockIdx,
					LocalPath:  path,
					DataSize:   dataSize,
				})
			}
		}
	}

	return flushed, nil
}

// flushBlock writes a single memBlock to disk as a .blk file using the TD-09
// stage-and-release pattern (D-23):
//
//  1. STAGE under mb.mu: snapshot the buffer via bytes.Clone, capture dataSize,
//     and release the lock immediately.
//  2. DISK I/O outside mb.mu: write to a .tmp file, fsync, and atomically
//     rename to the final path. Concurrent readers and writers of the same
//     memBlock are unblocked during this phase.
//  3. POST-WRITE: re-acquire mb.mu briefly. If no concurrent writer mutated
//     the buffer (writeGen unchanged), clear mb.data/dataSize/dirty and
//     return the buffer to the pool. If a writer DID interleave, leave the
//     buffer intact (dirty=true) so the next flush picks up their bytes.
//
// Counters (bc.diskUsed) are updated only AFTER the rename succeeds, so a
// failed disk write never produces a ghost size delta.
//
// Returns the path and dataSize of the flushed file, or empty string if no
// flush was needed.
//
// withFsync controls whether the .tmp file is fsynced before rename. The
// COMMIT path (Flush) passes true to satisfy NFS durability semantics;
// pressure-driven (flushOldestDirtyBlock) and block-fill (WriteAt) paths
// pass false to avoid the per-block fsync throughput hit, deferring
// durability to an explicit COMMIT or the chunkstore rollup.
func (bc *FSStore) flushBlock(ctx context.Context, payloadID string, blockIdx uint64, mb *memBlock, withFsync bool) (string, uint32, error) {
	_ = ctx // retained for signature compatibility; disk I/O is unbuffered.

	// STAGE under lock — capture an immutable snapshot.
	mb.mu.Lock()
	if !mb.dirty || mb.data == nil {
		mb.mu.Unlock()
		return "", 0, nil
	}
	dataSize := mb.dataSize
	staged := bytes.Clone(mb.data[:dataSize])
	stagedGen := mb.writeGen
	mb.mu.Unlock()

	key := blockKey{payloadID: payloadID, blockIdx: blockIdx}
	blockID := makeBlockID(key)
	path := bc.blockPath(blockID)

	// Evict any cached fds for this blockID — the .tmp+rename below will swap
	// the inode under the path, invalidating any held fd.
	bc.fdPool.Evict(blockID)
	bc.readFDPool.Evict(blockID)

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return "", 0, fmt.Errorf("create block dir: %w", err)
	}

	// Track previous file size so diskUsed delta is correct on re-flush.
	var prevDiskSize int64
	if fi, statErr := os.Stat(path); statErr == nil {
		prevDiskSize = fi.Size()
	}

	// Atomic write via .tmp + rename. Outside mb.mu — concurrent writers and
	// readers of the same memBlock proceed unimpeded.
	tmpPath := path + ".tmp"
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return "", 0, fmt.Errorf("create block tmp file: %w", err)
	}
	if _, err := f.Write(staged); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return "", 0, fmt.Errorf("write block tmp file: %w", err)
	}
	if withFsync {
		if err := f.Sync(); err != nil {
			_ = f.Close()
			_ = os.Remove(tmpPath)
			return "", 0, fmt.Errorf("fsync block tmp file: %w", err)
		}
		bc.flushFsyncCount.Add(1)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return "", 0, fmt.Errorf("close block tmp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return "", 0, fmt.Errorf("rename block file: %w", err)
	}

	// Disk I/O succeeded — update the diskIndex metadata. queueFileBlockUpdate
	// touches sync.Maps that have their own locking; no need to hold mb.mu.
	fb, ok := bc.diskIndexLookup(blockID)
	if !ok {
		fb = blockstore.NewFileBlock(blockID, path)
	}
	fb.LocalPath = path
	fb.DataSize = dataSize
	fb.State = blockstore.BlockStatePending
	fb.LastAccess = time.Now()
	bc.queueFileBlockUpdate(fb)

	// Counters AFTER disk success. diskUsed reflects the on-disk delta.
	bc.diskUsed.Add(int64(dataSize) - prevDiskSize)

	// POST-WRITE: re-acquire mb.mu briefly. Only clear the in-memory buffer
	// if no concurrent writer mutated it during the disk I/O window
	// (writeGen unchanged). If a writer DID interleave, their bytes are
	// already in mb.data and mb.dirty stays true — the next flush will
	// pick them up.
	var bufToReturn []byte
	mb.mu.Lock()
	if mb.writeGen == stagedGen {
		bufToReturn = mb.data
		mb.data = nil
		mb.dataSize = 0
		mb.dirty = false
		bc.memUsed.Add(-int64(blockstore.BlockSize))
	}
	mb.mu.Unlock()

	if bufToReturn != nil {
		// Return buffer to the pool for reuse (avoids 8MB zeroing on next alloc).
		putBlockBuf(bufToReturn)
	}

	return path, dataSize, nil
}

// flushOldestDirtyBlock finds the in-memory block with the oldest lastWrite
// timestamp and flushes it to disk. Returns true if a block was flushed.
// Called by WriteAt when the memory budget is exceeded (backpressure).
func (bc *FSStore) flushOldestDirtyBlock(ctx context.Context) bool {
	var oldestKey blockKey
	var oldestMB *memBlock
	var oldestTime time.Time

	bc.blocksMu.RLock()
	for key, mb := range bc.memBlocks {
		mb.mu.RLock()
		if mb.dirty && mb.data != nil {
			if oldestMB == nil || mb.lastWrite.Before(oldestTime) {
				oldestKey = key
				oldestMB = mb
				oldestTime = mb.lastWrite
			}
		}
		mb.mu.RUnlock()
	}
	bc.blocksMu.RUnlock()

	if oldestMB != nil {
		// Pressure-driven flush: no fsync. Durability is established later
		// by an explicit COMMIT (Flush) or the chunkstore rollup pipeline.
		if _, _, err := bc.flushBlock(ctx, oldestKey.payloadID, oldestKey.blockIdx, oldestMB, false); err != nil {
			logger.Warn("local store: failed to flush oldest block", "error", err)
			return false
		}
		return true
	}
	return false
}

// blockPath returns the block file path for a block ID.
// Sharded: <baseDir>/<first-2-chars>/<blockID>.blk
func (bc *FSStore) blockPath(blockID string) string {
	if len(blockID) < 2 {
		return filepath.Join(bc.baseDir, blockID+".blk")
	}
	return filepath.Join(bc.baseDir, blockID[:2], blockID+".blk")
}

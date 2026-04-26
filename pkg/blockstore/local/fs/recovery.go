package fs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/blockstore"
)

// payloadIDPattern is the permissive validation rule for payloadIDs derived
// from on-disk log filenames during recovery. FIX-18: a malicious or
// corrupted directory listing could surface a name like `../etc/passwd`
// (after stripping `.log`) which would re-enter recovery's per-payload
// state map under a path-traversing key. Anything outside this regex is
// skipped at the WalkDir boundary so the file is never opened or touched.
//
// The set [a-zA-Z0-9_-]{1,128} is intentionally permissive — Phase 11's
// store-spec pass may tighten it to the canonical ULID/UUID format the
// metadata layer emits.
var payloadIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)

// isValidPayloadID returns true iff s matches the permissive pattern
// above. Callers using this helper at the recovery boundary skip
// non-conformant filenames with a Warn log so an operator can spot
// stray files in the logs directory without blowing up recovery.
func isValidPayloadID(s string) bool {
	return payloadIDPattern.MatchString(s)
}

// Recover scans the block store directory for .blk files and reconciles them with
// the FileBlockStore (BadgerDB). Called on startup to restore local store state:
//
//   - Rebuilds the in-memory files map (payloadID -> fileSize) from disk
//   - Deletes orphan .blk files that have no FileBlock metadata
//   - Fixes stale LocalPaths (e.g., block store directory was moved)
//   - Reverts interrupted syncs (Syncing -> Local) for retry
//   - When useAppendLog=true: scans logs/*.log, reconciles header vs metadata
//     rollup_offset (D-12), truncates at first bad CRC, rebuilds interval
//     trees (D-16), and sweeps orphan logs (D-28 / LSL-06).
func (bc *FSStore) Recover(ctx context.Context) error {
	logger.Info("local store: starting recovery", "dir", bc.baseDir)

	var totalSize int64
	var filesFound, orphansDeleted, syncsReverted int

	err := filepath.WalkDir(bc.baseDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".blk") {
			return nil
		}

		filesFound++

		// Extract blockID from the full path, reversing blockPath's sharding.
		// blockPath creates: <baseDir>/<shard>/<blockID>.blk where shard = blockID[:2].
		rel, relErr := filepath.Rel(bc.baseDir, path)
		if relErr != nil {
			logger.Warn("local store: recovery skipping file", "path", path, "error", relErr)
			return nil
		}
		rel = strings.TrimSuffix(rel, ".blk")
		// Remove the 2-char shard directory prefix.
		var blockID string
		if parts := strings.SplitN(rel, string(filepath.Separator), 2); len(parts) == 2 {
			blockID = parts[1]
		} else {
			blockID = rel
		}

		fb, err := bc.blockStore.GetFileBlock(ctx, blockID)
		if err != nil {
			if errors.Is(err, blockstore.ErrFileBlockNotFound) {
				if rmErr := os.Remove(path); rmErr != nil {
					logger.Warn("local store: recovery failed to remove orphan", "path", path, "error", rmErr)
				}
				orphansDeleted++
			} else {
				logger.Warn("local store: recovery skipping block due to transient error", "blockID", blockID, "error", err)
			}
			return nil
		}

		needsUpdate := false

		// Fix local path if it changed (e.g., moved block store directory)
		if fb.LocalPath != path {
			fb.LocalPath = path
			needsUpdate = true
		}

		// Blocks with a BlockStoreKey but still Pending -> already synced to remote
		// (legacy zero-valued rows; D-21 dual-read window).
		if fb.BlockStoreKey != "" && fb.State == blockstore.BlockStatePending {
			fb.State = blockstore.BlockStateRemote
			needsUpdate = true
		}

		// Revert interrupted syncs so they get retried (Syncing -> Pending; D-14).
		if fb.State == blockstore.BlockStateSyncing {
			fb.State = blockstore.BlockStatePending
			needsUpdate = true
			syncsReverted++
		}

		if needsUpdate {
			if putErr := bc.blockStore.PutFileBlock(ctx, fb); putErr != nil {
				logger.Warn("local store: recovery failed to update block metadata", "blockID", blockID, "error", putErr)
			}
		}

		// Seed the in-process diskIndex so the post-Recover write hot path
		// and eviction can see this block without a FileBlockStore query
		// (TD-02d / D-19).
		bc.diskIndexStore(fb)

		payloadID, blockIdx, parseErr := blockstore.ParseBlockID(blockID)
		if parseErr == nil {
			end := (blockIdx + 1) * blockstore.BlockSize
			if fb.DataSize > 0 && fb.DataSize < uint32(blockstore.BlockSize) {
				end = blockIdx*blockstore.BlockSize + uint64(fb.DataSize)
			}
			bc.updateFileSize(payloadID, end)
		}

		if info, err := d.Info(); err == nil {
			totalSize += info.Size()
		}
		return nil
	})

	if err != nil {
		return fmt.Errorf("walk block store dir: %w", err)
	}

	bc.diskUsed.Store(totalSize)

	var logsScanned, logsRecovered, recordsTruncated, intervalsRebuilt, orphanLogsSwept, headersReconciled int
	if bc.useAppendLog {
		logsScanned, logsRecovered, recordsTruncated, intervalsRebuilt, orphanLogsSwept, headersReconciled = bc.recoverAppendLogs(ctx)
	}

	logger.Info("local store: recovery complete",
		"filesFound", filesFound,
		"orphansDeleted", orphansDeleted,
		"syncsReverted", syncsReverted,
		"totalSize", totalSize,
		"logsScanned", logsScanned,
		"logsRecovered", logsRecovered,
		"recordsTruncated", recordsTruncated,
		"intervalsRebuilt", intervalsRebuilt,
		"orphanLogsSwept", orphanLogsSwept,
		"headersReconciled", headersReconciled)

	return nil
}

// recoverAppendLogs scans {baseDir}/logs/*.log, reconciles each log's header
// against the metadata rollup_offset, truncates any log at the first bad-CRC
// record, rebuilds per-file interval trees, and sweeps orphan logs (D-28).
//
// Returns (logsScanned, logsRecovered, recordsTruncated, intervalsRebuilt,
// orphanLogsSwept, headersReconciled).
//
// D-12 crash-window reconciliation: rollupFile commits in the order
// (1) StoreChunk → (2) SetRollupOffset metadata → (3) advanceRollupOffset
// header → (4) tree.ConsumeUpTo. A crash between steps 2 and 3 leaves
// metadata ahead of the on-disk header; this function rewrites the header
// to match metadata on next boot, so a second rollup pass does not re-emit
// chunks for bytes that are already committed.
//
// LSL-06: truncation at the first unreadable record preserves every record
// that passed CRC. Surviving records are re-inserted into the interval
// tree so the rollup picks up where the previous run left off.
//
// D-28 / Warning 3 (orphan sweep): a log is swept only when ALL of
// (a) metadata rollup_offset == 0, (b) no block-0 FileBlock exists for
// the payload, AND (c) the log's on-disk mtime is older than
// orphanLogMinAgeSeconds. The age gate prevents a false positive on a
// freshly-created log whose writes have not yet been rolled up.
func (bc *FSStore) recoverAppendLogs(ctx context.Context) (int, int, int, int, int, int) {
	logsDir := filepath.Join(bc.baseDir, "logs")
	if _, err := os.Stat(logsDir); os.IsNotExist(err) {
		return 0, 0, 0, 0, 0, 0
	}

	// FIX-16: compute the effective orphan-sweep floor once and warn if the
	// configured value is non-positive — surfaces the silent default-3600
	// substitution at boot rather than per-log-file. The legacy per-iteration
	// computation inside the WalkDir below now reads this single value.
	effectiveMinAgeSec := bc.orphanLogMinAgeSeconds
	if bc.orphanLogMinAgeSeconds <= 0 {
		logger.Warn("recovery: orphan_log_min_age_seconds is non-positive, using default 3600", "configured", bc.orphanLogMinAgeSeconds)
		effectiveMinAgeSec = 3600
	}

	var scanned, recovered, truncated, rebuilt, orphaned, reconciled int

	_ = filepath.WalkDir(logsDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".log") {
			return nil
		}
		scanned++
		payloadID := strings.TrimSuffix(d.Name(), ".log")
		// FIX-18: defensive validation against path traversal / malformed
		// filenames that arrive via on-disk corruption or out-of-band
		// writes to the logs directory. Skip and warn — never open the
		// file, never touch FSStore state.
		if !isValidPayloadID(payloadID) {
			logger.Warn("recovery: skipping log file with invalid payloadID", "filename", d.Name())
			return nil
		}

		// Capture the on-disk mtime BEFORE any RDWR operation so the
		// orphan-sweep age gate (D-28) sees the pre-boot mtime, not a
		// fresh timestamp produced by this recovery pass (Truncate /
		// advanceRollupOffset both touch mtime on most filesystems).
		// FIX-26: open the file FIRST, then call f.Stat() on the open
		// fd to capture preBootMTime. The previous order
		// (os.Stat → os.OpenFile) opened a TOCTOU window where a
		// symlink swap between the two operations could yield mtime
		// from a different file than the one ultimately opened. Using
		// f.Stat() after open binds the mtime read to the same inode
		// the rest of recovery operates on. f.Stat failure follows the
		// same Warn path as FIX-24 — preBootMTime stays zero, the
		// FIX-13 mtime-restore branch on the corrupt-header reinit
		// path is skipped, and the operator has a log signal.
		f, err := os.OpenFile(path, os.O_RDWR, 0644)
		if err != nil {
			logger.Warn("recovery: open log failed; skipping", "path", path, "error", err)
			return nil
		}
		var preBootMTime time.Time
		if st, serr := f.Stat(); serr == nil {
			preBootMTime = st.ModTime()
		} else {
			logger.Warn("recovery: preBootMTime f.Stat failed; FIX-13 mtime restore will be skipped",
				"path", path, "err", serr)
		}
		hdr, err := readLogHeader(f)
		if err != nil {
			// Hard corruption: drop the fd, unlink, re-init with a fresh
			// header so subsequent AppendWrites open cleanly. The surviving
			// records are unrecoverable without a valid header, so this is
			// logged as a hard-error recovery event.
			//
			// FIX-13: preserve the pre-boot mtime on the fresh log file.
			// initLogFile fsyncs and bumps mtime to "now"; without
			// restoring the original mtime, a repeatedly-corrupted log
			// would never become "old enough" for the orphan-sweep age
			// gate (D-28) to fire — the clock would reset on every boot.
			logger.Warn("recovery: header corrupt; truncating log",
				"path", path, "error", err)
			_ = f.Close()
			_ = os.Remove(path)
			nf, initErr := initLogFile(path, time.Now().Unix())
			if initErr != nil {
				logger.Warn("recovery: re-init after corrupt header failed",
					"path", path, "error", initErr)
				return nil
			}
			_ = nf.Close()
			if !preBootMTime.IsZero() {
				if cerr := os.Chtimes(path, preBootMTime, preBootMTime); cerr != nil {
					logger.Warn("recovery: restore mtime after corrupt-header reinit failed",
						"path", path, "error", cerr)
				}
			}
			return nil
		}

		// D-12 reconciliation: metadata > header means a CommitChunks
		// crashed between step 2 (SetRollupOffset) and step 3
		// (advanceRollupOffset). Rewrite the header to match metadata so
		// replay does not re-emit chunks for bytes already persisted.
		metaOff := uint64(0)
		if bc.rollupStore != nil {
			metaOff, _ = bc.rollupStore.GetRollupOffset(ctx, payloadID)
		}
		effectiveOff := hdr.RollupOffset
		if metaOff > effectiveOff {
			if aerr := advanceRollupOffset(f, metaOff); aerr != nil {
				logger.Warn("recovery: advanceRollupOffset failed", "path", path, "error", aerr)
			} else {
				effectiveOff = metaOff
				reconciled++
			}
		}

		// Seek to effectiveOff and replay records into the interval tree.
		if _, err := f.Seek(int64(effectiveOff), io.SeekStart); err != nil {
			logger.Warn("recovery: seek failed", "path", path, "error", err)
			_ = f.Close()
			return nil
		}
		tree := newIntervalTree()
		pos := effectiveOff
		records := 0
		for {
			lastPos := pos
			off, payload, ok, rerr := readRecord(f)
			if rerr != nil {
				logger.Warn("recovery: hard I/O error during replay", "path", path, "error", rerr)
				break
			}
			if !ok {
				// LSL-06: truncate at the last-successful-record boundary,
				// but only when there are actually trailing bytes past
				// lastPos. A clean EOF at the record boundary leaves the
				// file untouched so mtime remains authoritative for the
				// orphan-sweep age gate (D-28).
				if st, serr := f.Stat(); serr == nil && st.Size() > int64(lastPos) {
					if terr := f.Truncate(int64(lastPos)); terr != nil {
						logger.Warn("recovery: truncate failed", "path", path, "offset", lastPos, "error", terr)
					} else {
						// FIX-7: fsync the truncate so the file size
						// shrink is durable before we hand the fd back
						// to AppendWrite. Without this, a crash
						// immediately after recovery could resurface
						// the torn tail on next boot.
						//
						// FIX-22: if the post-truncate fsync FAILS, do
						// NOT install the fd. The truncate is not
						// durable so the trimmed bytes can resurface
						// on a crash; pairing that with an installed,
						// in-use fd would let the next AppendWrite
						// extend an inconsistent tail. Close the fd,
						// log Error, and continue — the next boot's
						// recovery + orphan-sweep will reconcile.
						if syncErr := f.Sync(); syncErr != nil {
							logger.Error("recovery: truncate fsync failed; skipping log install (FIX-22)",
								"path", path, "err", syncErr)
							_ = f.Close()
							return nil
						}
					}
					truncated++
				}
				break
			}
			tree.Insert(off, uint32(len(payload)), time.Now())
			pos += uint64(recordFrameOverhead) + uint64(len(payload))
			records++
		}
		if records > 0 {
			rebuilt++
		}
		recovered++

		// D-28 / Warning 3 orphan sweep: a log is swept only when
		//   (a) metaOff == 0
		//   (b) no block-0 live FileBlock for the payloadID
		//   (c) log mtime >= effectiveMinAgeSec (computed once at entry, FIX-16)
		// The mtime gate guarantees fresh logs are never swept.
		isOrphan := false
		if metaOff == 0 && !bc.payloadHasLiveMetadata(ctx, payloadID) {
			if !preBootMTime.IsZero() {
				age := time.Since(preBootMTime)
				if age >= time.Duration(effectiveMinAgeSec)*time.Second {
					isOrphan = true
				}
			}
		}
		if isOrphan {
			_ = f.Close()
			rerr := os.Remove(path)
			if rerr == nil {
				orphaned++
				return nil
			}
			// FIX-23: surface the os.Remove failure. The previous
			// behavior silently fell through to fd install, leaving
			// operators with no signal that an orphan log persisted
			// on disk — and Phase 11 mark-sweep cannot reach this
			// path because the sweep is gated on rollup_offset != 0.
			logger.Warn("recovery: orphan log sweep failed, installing fd anyway",
				"path", path, "err", rerr)
			// Remove failed — fall through and install the fd anyway so
			// the payload remains operable.
			f, err = os.OpenFile(path, os.O_RDWR, 0644)
			if err != nil {
				logger.Warn("recovery: reopen after failed sweep", "path", path, "error", err)
				return nil
			}
		}

		// Install fd into FSStore, seeked to EOF for subsequent appends.
		if _, err := f.Seek(0, io.SeekEnd); err != nil {
			logger.Warn("recovery: seek end failed", "path", path, "error", err)
			_ = f.Close()
			return nil
		}
		bc.logsMu.Lock()
		bc.logFDs[payloadID] = &logFile{f: f, path: path}
		bc.logLocks[payloadID] = &sync.Mutex{}
		bc.dirtyIntervals[payloadID] = tree
		bc.logsMu.Unlock()
		// Reflect the resident (un-rolled-up) log bytes in logBytesTotal
		// so the pressure loop sees accurate state after boot.
		if st, serr := f.Stat(); serr == nil {
			resident := st.Size() - int64(effectiveOff)
			if resident > 0 {
				bc.logBytesTotal.Add(resident)
			}
		}
		return nil
	})

	return scanned, recovered, truncated, rebuilt, orphaned, reconciled
}

// payloadHasLiveMetadata reports whether the FileBlockStore has any live
// block prefixed by payloadID. A single GetFileBlock probe for block-0 is
// a cheap heuristic suitable for orphan-log sweep classification.
func (bc *FSStore) payloadHasLiveMetadata(ctx context.Context, payloadID string) bool {
	if bc.blockStore == nil {
		return false
	}
	_, err := bc.blockStore.GetFileBlock(ctx, payloadID+"/block-0")
	return err == nil
}

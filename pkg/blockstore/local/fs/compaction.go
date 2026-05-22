package fs

import (
	"context"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"log/slog"
	"os"
	"path/filepath"
)

// LogFlagCompacted is set in the on-disk header's Flags field after a
// successful physical compaction pass. Recovery uses this bit to decide
// whether to honor the metadata-vs-header reconcile (which assumes the
// header's RollupOffset is the physical byte at which un-consumed records
// begin, AND that metadata.rollup_offset is the same physical position).
// After compaction the file is renumbered so the surviving records start
// at logHeaderSize while metadata.rollup_offset still records the
// pre-compaction byte offset (it is monotonic per INV-03 and cannot be
// regressed). Without the flag, recovery would interpret metaOff > hdrOff
// as a crash between SetRollupOffset and advanceRollupOffset and seek
// past the compacted file's EOF, losing every surviving record.
const LogFlagCompacted uint32 = 1 << 0

// defaultCompactionDivisor controls the default compaction threshold:
// maxLogBytes / defaultCompactionDivisor. A compaction pass runs after
// rollup when the in-memory fence advanced by at least this many bytes
// since the last compaction (or since the log was created). Setting the
// divisor higher delays compaction (lower I/O churn, larger on-disk
// growth between passes); lower triggers more aggressive compaction
// (more I/O, tighter disk bound).
const defaultCompactionDivisor int64 = 4

// maybeCompactLog runs compaction for payloadID if the on-disk log has
// accumulated more pre-fence bytes than bc.compactionThresholdBytes.
//
// Caller MUST hold the per-file mu (rollupFile does — it calls this
// before releasing the mutex via defer). The lf and idx parameters
// must be the canonical pointers stored in bc.logFDs / bc.logIndices;
// we mutate them in place under the per-file mu so concurrent callers
// (other AppendWrite / rollupFile goroutines that already snapshotted
// these pointers and are awaiting mu) observe the post-compaction state
// when they finally acquire mu.
//
// Returns nil and skips compaction silently if:
//   - bc.compactionThresholdBytes <= 0 (compaction disabled);
//   - the fence has not advanced past logHeaderSize by more than the
//     threshold (nothing meaningful to reclaim);
//   - idx is nil (defensive — should not happen given the caller's
//     pre-conditions, but ranging over a nil idx would panic).
//
// Returns a non-nil error only on hard I/O failures during the rewrite/
// fsync/rename sequence. On error the original lf.f / idx are left
// unchanged — the next rollup pass retries naturally on the next
// threshold trip.
func (bc *FSStore) maybeCompactLog(ctx context.Context, payloadID string, lf *logFile, idx *logIndex) error {
	if bc.compactionThresholdBytes <= 0 {
		return nil
	}
	if lf == nil || idx == nil {
		return nil
	}
	fence := idx.Fence()
	if fence <= logHeaderSize {
		return nil
	}
	reclaimable := fence - logHeaderSize
	if int64(reclaimable) < bc.compactionThresholdBytes {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return bc.compactLogLocked(payloadID, lf, idx)
}

// compactLogLocked rewrites the on-disk log for payloadID, dropping all
// records whose logPos sits below idx.Fence(). The surviving records
// (logPos >= fence) are re-emitted in arrival order starting at
// logHeaderSize in a temporary file alongside the original; the temp is
// fsync'd, the containing directory is fsync'd, and a rename atomically
// replaces the live log. After the swap the in-memory lf is mutated in
// place (new fd + new groupCommit + reset eofPos) and idx is rebased
// (entries' logPos values shifted, compactionFence reset to
// logHeaderSize, consumed map rekeyed).
//
// Caller MUST hold the per-file mu. The lf and idx must be the canonical
// pointers in bc.logFDs / bc.logIndices.
//
// Crash safety: the rename(2) is the linearization point. A crash at
// any point before rename leaves the original log untouched (the temp
// file is unlinked on the error paths below; orphaned temps surviving a
// hard kill are cleaned up by the boot-time orphan sweep — see
// cleanupCompactTemps). A crash after rename but before the new fd is
// reopened is also safe: the next AppendWrite / rollupFile will reopen
// from disk and pick up the compacted layout transparently. Recovery
// uses the LogFlagCompacted bit in the header to skip the
// metaOff > hdrOff reconcile that would otherwise misinterpret the
// renumbered file.
//
// BLAKE3 / CAS: no chunk is re-emitted. Compaction touches only the
// on-disk log bytes; CAS chunks in blocks/{hh}/{hh}/{hex} are produced
// solely by the rollup path and are unaffected here.
func (bc *FSStore) compactLogLocked(payloadID string, lf *logFile, idx *logIndex) error {
	fence := idx.Fence()
	if fence <= logHeaderSize {
		return nil
	}

	// Snapshot surviving entries (logPos >= fence) in arrival order. The
	// idx.entries slice is already in logPos-ascending order by Append's
	// caller contract.
	survivors := make([]logEntry, 0, len(idx.entries))
	for _, e := range idx.entries {
		if e.logPos >= fence {
			survivors = append(survivors, e)
		}
	}

	tmpPath := lf.path + ".compact"
	// Clean up any stale temp from a prior failed pass before we re-open.
	if rmErr := os.Remove(tmpPath); rmErr != nil && !os.IsNotExist(rmErr) {
		return fmt.Errorf("compaction: pre-clean temp: %w", rmErr)
	}

	tmpFd, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_RDWR|os.O_EXCL, 0644)
	if err != nil {
		return fmt.Errorf("compaction: create temp: %w", err)
	}
	// Best-effort temp cleanup on every error path below. The deferred
	// helper closes the fd if still open and unlinks the temp if it
	// still exists. Success path nils tmpFd so the defer is a no-op.
	cleanup := true
	defer func() {
		if cleanup {
			_ = tmpFd.Close()
			_ = os.Remove(tmpPath)
		}
	}()

	// Write a fresh header into the temp file. RollupOffset starts at
	// logHeaderSize (the first surviving record sits immediately after
	// the header); LogFlagCompacted is set so recovery's reconcile knows
	// to trust the header rather than metadata.
	//
	// CreatedAt is preserved from the original log so the boot-time
	// orphan sweep age gate (D-28) still classifies the payload against
	// its original first-record timestamp, not a freshly-bumped one.
	origHdr, hdrErr := readLogHeader(lf.f)
	if hdrErr != nil {
		return fmt.Errorf("compaction: read orig header: %w", hdrErr)
	}
	newHdr := logHeader{
		Magic:        logMagic,
		Version:      logVersion,
		RollupOffset: logHeaderSize,
		Flags:        origHdr.Flags | LogFlagCompacted,
		CreatedAt:    origHdr.CreatedAt,
	}
	hdrBuf := marshalHeader(newHdr)
	if _, werr := tmpFd.WriteAt(hdrBuf[:], 0); werr != nil {
		return fmt.Errorf("compaction: write temp header: %w", werr)
	}

	// Copy each surviving record's framed bytes from the source fd to
	// the temp fd. We use a separate read-only fd on the source path so
	// the lf.f file position is undisturbed (mirrors rollupFile's rf).
	rf, err := os.Open(lf.path)
	if err != nil {
		return fmt.Errorf("compaction: open source: %w", err)
	}
	defer func() { _ = rf.Close() }()

	newPos := uint64(logHeaderSize)
	// rebased holds the post-compaction entries in arrival order. We
	// populate logPos with the new physical position; fileOff and
	// payloadLen are copied verbatim from the survivor entries.
	rebased := make([]logEntry, 0, len(survivors))
	frameBuf := make([]byte, 0, recordFrameOverhead+64*1024)
	for _, e := range survivors {
		frameSize := uint64(recordFrameOverhead) + uint64(e.payloadLen)
		if cap(frameBuf) < int(frameSize) {
			frameBuf = make([]byte, frameSize)
		} else {
			frameBuf = frameBuf[:frameSize]
		}
		if _, rerr := rf.ReadAt(frameBuf, int64(e.logPos)); rerr != nil {
			return fmt.Errorf("compaction: pread at %d (len=%d): %w", e.logPos, frameSize, rerr)
		}
		// Defensive: verify the frame's payload_len header matches the
		// indexed value. A mismatch implies log-fd corruption or a
		// logIndex/log divergence bug — surfacing it here prevents the
		// compaction from silently writing a malformed temp.
		declaredLen := binary.LittleEndian.Uint32(frameBuf[0:4])
		if declaredLen != e.payloadLen {
			return fmt.Errorf("compaction: frame payload_len %d != logIndex %d at logPos=%d",
				declaredLen, e.payloadLen, e.logPos)
		}
		// CRC re-validation is not strictly necessary here (the frame
		// bytes are being copied verbatim, so the CRC inside the frame
		// is preserved regardless of whether we re-check it), but it is
		// a cheap belt-and-braces against a torn read.
		wantCRC := binary.LittleEndian.Uint32(frameBuf[12:16])
		var offBuf [8]byte
		copy(offBuf[:], frameBuf[4:12])
		gotCRC := crc32.Update(0, crcTable, offBuf[:])
		gotCRC = crc32.Update(gotCRC, crcTable, frameBuf[recordFrameOverhead:])
		if gotCRC != wantCRC {
			return fmt.Errorf("compaction: CRC mismatch at logPos=%d", e.logPos)
		}

		if _, werr := tmpFd.WriteAt(frameBuf, int64(newPos)); werr != nil {
			return fmt.Errorf("compaction: write temp record at %d: %w", newPos, werr)
		}
		rebased = append(rebased, logEntry{
			logPos:     newPos,
			fileOff:    e.fileOff,
			payloadLen: e.payloadLen,
		})
		newPos += frameSize
	}

	// fsync the temp file data + header so they hit the platter before
	// the rename. Without this, a crash after rename could leave a
	// validly-named log file whose bytes never made it to disk —
	// recovery would then see a short / corrupt file and truncate it.
	if err := tmpFd.Sync(); err != nil {
		return fmt.Errorf("compaction: fsync temp: %w", err)
	}
	if err := tmpFd.Close(); err != nil {
		return fmt.Errorf("compaction: close temp: %w", err)
	}

	// fsync the containing directory so the rename's metadata change
	// (new dentry + remove old dentry — atomic in rename(2)) is durable.
	// This guards against the "rename completes in cache but is lost on
	// crash" pathology on filesystems that defer dentry persistence.
	dir := filepath.Dir(lf.path)
	dfd, derr := os.Open(dir)
	if derr != nil {
		return fmt.Errorf("compaction: open parent dir for fsync: %w", derr)
	}
	if err := dfd.Sync(); err != nil {
		_ = dfd.Close()
		return fmt.Errorf("compaction: fsync parent dir (pre-rename): %w", err)
	}
	if err := dfd.Close(); err != nil {
		return fmt.Errorf("compaction: close parent dir (pre-rename): %w", err)
	}

	// Atomic rename. Per POSIX, rename(2) is atomic with respect to a
	// crash on the same filesystem: the dentry transition is all-or-
	// nothing. After this point the on-disk log is the compacted file;
	// we now reopen the new fd and swap it into lf under the per-file
	// mu we still hold.
	if err := os.Rename(tmpPath, lf.path); err != nil {
		return fmt.Errorf("compaction: rename: %w", err)
	}
	cleanup = false // temp is now the live log; do not unlink in defer.

	// fsync the parent directory once more so the rename itself is
	// durable (the pre-rename fsync above only covered the temp file's
	// directory entry creation; the rename creates/removes dentries
	// that must also be flushed). Cheap on most filesystems — the
	// directory is already in cache.
	if dfd, derr := os.Open(dir); derr == nil {
		if syncErr := dfd.Sync(); syncErr != nil {
			slog.Warn("compaction: fsync parent dir (post-rename) failed",
				"payloadID", payloadID, "path", lf.path, "error", syncErr)
		}
		_ = dfd.Close()
	} else {
		slog.Warn("compaction: open parent dir for post-rename fsync failed",
			"payloadID", payloadID, "path", lf.path, "error", derr)
	}

	// The rename replaced the inode underneath lf.f. Close the stale fd
	// and open a fresh one against the new file, seeked to EOF for
	// subsequent appends.
	oldFd := lf.f
	newFd, err := os.OpenFile(lf.path, os.O_RDWR, 0644)
	if err != nil {
		// We are in an awkward state: the compacted file is on disk
		// (correct) but we cannot reopen it. Close the stale fd anyway
		// to avoid leaking the descriptor; the next AppendWrite or
		// rollupFile call will hit the curLf re-validation, observe
		// the missing entry in bc.logFDs (we delete below), and
		// fresh-create from disk on next touch via getOrCreateLog.
		_ = oldFd.Close()
		bc.logsMu.Lock()
		delete(bc.logFDs, payloadID)
		bc.logsMu.Unlock()
		return fmt.Errorf("compaction: reopen after rename: %w", err)
	}
	eof, err := newFd.Seek(0, io.SeekEnd)
	if err != nil {
		_ = newFd.Close()
		_ = oldFd.Close()
		bc.logsMu.Lock()
		delete(bc.logFDs, payloadID)
		bc.logsMu.Unlock()
		return fmt.Errorf("compaction: seek end after rename: %w", err)
	}

	// Swap the fd in lf and rebuild the groupCommit coordinator. The
	// per-file mu we hold serializes us against all AppendWrite /
	// rollupFile sites that touch lf.f or lf.groupCommit, so the swap
	// is observed atomically by any caller that subsequently acquires
	// mu. The old groupCommit is quiescent (no in-flight Sync — Sync
	// is only called under mu) so dropping the reference is safe.
	lf.f = newFd
	lf.eofPos = uint64(eof)
	lf.groupCommit = newGroupCommit(newFd.Sync)
	_ = oldFd.Close()

	// Rebase the logIndex. The new physical layout has surviving
	// entries at the positions recorded in `rebased`. consumedCoverage
	// is reset wholesale: every byte below the old fence has either
	// been physically dropped or belongs to a survivor whose file
	// extent is by definition not yet fully covered (AdvanceFence only
	// walks past entries whose extent is covered). Keeping the old
	// coverage would only re-prove what the surviving entries already
	// imply once their own MarkConsumed calls land. Fence resets to
	// logHeaderSize and the cursor rewinds.
	idx.entries = rebased
	idx.consumedCoverage = coverageSet{}
	idx.compactionFence = logHeaderSize
	idx.fenceCursor = 0

	slog.Debug("compaction: rewrote log",
		"payloadID", payloadID,
		"path", lf.path,
		"reclaimedBytes", fence-logHeaderSize,
		"newEof", lf.eofPos,
		"survivors", len(rebased),
	)
	return nil
}

// cleanupCompactTemps removes any orphaned `.compact` temp files left
// behind by a process that crashed mid-compaction. Run from recovery
// before the per-log scan installs fds. Best-effort: per-file removal
// errors are logged at Warn and otherwise ignored — a stale temp does
// not affect correctness, only wastes disk.
func (bc *FSStore) cleanupCompactTemps(logsDir string) {
	entries, err := os.ReadDir(logsDir)
	if err != nil {
		return
	}
	for _, d := range entries {
		if d.IsDir() {
			continue
		}
		name := d.Name()
		if filepath.Ext(name) != ".compact" {
			continue
		}
		path := filepath.Join(logsDir, name)
		if rmErr := os.Remove(path); rmErr != nil {
			slog.Warn("compaction: cleanup stale temp failed",
				"path", path, "error", rmErr)
		}
	}
	// Walk one level deeper to catch share-prefixed payload IDs (the
	// log layout supports nested directories under logs/).
	_ = filepath.WalkDir(logsDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if filepath.Ext(d.Name()) != ".compact" {
			return nil
		}
		if rmErr := os.Remove(path); rmErr != nil {
			slog.Warn("compaction: cleanup stale temp failed",
				"path", path, "error", rmErr)
		}
		return nil
	})
}

package handlers

import (
	"time"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// SMB delayed-write timestamp semantics
//
// Windows / Samba surface LastWriteTime to clients with a 2-second delay:
// the first WRITE on a handle leaves the visible Mtime at its pre-write
// value for two seconds, then advances it to the first-write timestamp
// and pins it there for the rest of the open. Subsequent writes do not
// re-arm the timer, so QUERY_INFO keeps returning that same value until
// a flush trigger (SMB FLUSH, SET_INFO BasicInfo, SET_INFO EndOfFile) or
// CLOSE. An explicit SetBasic write_time makes the value sticky until
// the handle is closed.
//
// DittoFS's metadata layer bumps file.Mtime synchronously on every write
// (NFS semantics). We layer the SMB-only delay on top via per-OpenFile
// state so NFS callers still see immediate updates while SMB QUERY_INFO
// presents the Samba-style view that smb2.timestamps.* asserts.
//
// References:
//   - source3/smbd/fileio.c::trigger_write_time_update
//   - source4/torture/smb2/timestamps.c (delayed-*, freeze-thaw)

// smbDelayedWriteWindow is Samba's WRITE_TIME_UPDATE_USEC_DELAY (2 s).
const smbDelayedWriteWindow = 2 * time.Second

// smbAtimeUpdateWindow bounds how often a READ-driven LastAccessTime bump
// reaches the metadata store on one handle. Without it every READ RPC costs a
// read-modify-write of the file's metadata.
const smbAtimeUpdateWindow = 2 * time.Second

// noteSmbAccess records an access at t on the handle and reports whether it has
// to reach the metadata store now. When it returns false the time is held on
// the handle instead, for applySmbPendingAtime / takeSmbPendingAtime. The first
// access on a handle always writes.
//
// Takes openFile.mu (write) — concurrent READ pipelines on the same handle must
// serialize so exactly one of them takes the store write.
func noteSmbAccess(openFile *OpenFile, t time.Time) bool {
	if openFile == nil {
		return false
	}
	openFile.mu.Lock()
	defer openFile.mu.Unlock()
	if !openFile.SmbAtimeWrittenAt.IsZero() && t.Sub(openFile.SmbAtimeWrittenAt) < smbAtimeUpdateWindow {
		// Latest access wins: concurrent READ pipelines on one handle can reach
		// the lock out of sample order, and an older sample must not lower the
		// access time the overlay reports or CLOSE persists.
		if t.After(openFile.SmbPendingAtime) && t.After(openFile.SmbAtimeWrittenAt) {
			openFile.SmbPendingAtime = t
		}
		return false
	}
	openFile.SmbAtimeWrittenAt = t
	openFile.SmbPendingAtime = time.Time{}
	return true
}

// noteSmbParentAccess records a parent-directory access at t on the handle and
// reports whether it has to reach the metadata store now. The first access on a
// handle always writes.
//
// Unlike noteSmbAccess, a suppressed bump is dropped rather than held: the
// parent directory has no handle here to carry it and no QUERY_INFO overlay to
// surface it, so its LastAccessTime trails the newest write by at most
// smbAtimeUpdateWindow instead of serializing every WRITE on the parent row.
//
// Takes openFile.mu (write).
func noteSmbParentAccess(openFile *OpenFile, t time.Time) bool {
	if openFile == nil {
		return false
	}
	openFile.mu.Lock()
	defer openFile.mu.Unlock()
	if !openFile.SmbParentAtimeWrittenAt.IsZero() && t.Sub(openFile.SmbParentAtimeWrittenAt) < smbAtimeUpdateWindow {
		return false
	}
	openFile.SmbParentAtimeWrittenAt = t
	return true
}

// takeSmbPendingAtime removes and returns the access time held on the handle
// since the last store write, or the zero time when there is none. Called at
// CLOSE so a coalesced bump is not lost with the handle.
//
// Takes openFile.mu (write).
func takeSmbPendingAtime(openFile *OpenFile) time.Time {
	if openFile == nil {
		return time.Time{}
	}
	openFile.mu.Lock()
	defer openFile.mu.Unlock()
	pending := openFile.SmbPendingAtime
	openFile.SmbPendingAtime = time.Time{}
	return pending
}

// applySmbPendingAtime overlays the coalesced access time on top of a
// freshly-read metadata.File so QUERY_INFO reports the newest access rather
// than the last one that reached the store. A newer stored value (another
// handle's explicit set) wins.
//
// Takes openFile.mu (read).
func applySmbPendingAtime(openFile *OpenFile, file *metadata.File) {
	if openFile == nil || file == nil {
		return
	}
	openFile.mu.RLock()
	defer openFile.mu.RUnlock()
	if file.Atime.Before(openFile.SmbPendingAtime) {
		file.Atime = openFile.SmbPendingAtime
	}
}

// armSmbDelayedWriteLocked is the lock-free body of armSmbDelayedWrite.
// Callers must hold openFile.mu (write).
func armSmbDelayedWriteLocked(openFile *OpenFile, preMtime time.Time, writeTime time.Time) {
	if openFile == nil {
		return
	}
	if openFile.SmbStickyWriteTime != nil {
		return
	}
	if openFile.SmbWriteTriggered {
		return
	}
	openFile.SmbWriteTriggered = true
	pre := preMtime
	flush := writeTime
	openFile.SmbWritePreMtime = &pre
	openFile.SmbWriteFlushMtime = &flush
	openFile.SmbWriteFlushAt = time.Now().Add(smbDelayedWriteWindow)
}

// armSmbDelayedWrite records that a WRITE happened and arms the 2-second
// visibility window on the first call. Subsequent calls are no-ops.
//
// Takes OpenFile.mu (write) — concurrent WRITE pipelines on the same handle
// must serialize their first-write capture (#606). Callers that already hold
// the write lock should call armSmbDelayedWriteLocked instead.
func armSmbDelayedWrite(openFile *OpenFile, preMtime time.Time, writeTime time.Time) {
	if openFile == nil {
		return
	}
	openFile.mu.Lock()
	defer openFile.mu.Unlock()
	armSmbDelayedWriteLocked(openFile, preMtime, writeTime)
}

// flushSmbDelayedWriteLocked is the lock-free body of flushSmbDelayedWrite.
// Callers must hold openFile.mu (write).
func flushSmbDelayedWriteLocked(openFile *OpenFile) {
	if openFile == nil {
		return
	}
	if !openFile.SmbWriteTriggered {
		return
	}
	openFile.SmbWriteFlushAt = time.Time{}
}

// flushSmbDelayedWrite collapses the visibility window so the next
// QUERY_INFO surfaces the post-write Mtime. Called from FLUSH, SET_INFO
// BasicInfo (any flavour) and SET_INFO EndOfFile per Samba's
// trigger_write_time_update_immediate.
//
// Takes OpenFile.mu (write). Callers that already hold the write lock should
// call flushSmbDelayedWriteLocked instead.
func flushSmbDelayedWrite(openFile *OpenFile) {
	if openFile == nil {
		return
	}
	openFile.mu.Lock()
	defer openFile.mu.Unlock()
	flushSmbDelayedWriteLocked(openFile)
}

// setSmbStickyWriteTimeLocked is the lock-free body of setSmbStickyWriteTime.
// Callers must hold openFile.mu (write).
func setSmbStickyWriteTimeLocked(openFile *OpenFile, t time.Time) {
	if openFile == nil {
		return
	}
	v := t
	openFile.SmbStickyWriteTime = &v
}

// setSmbStickyWriteTime records an explicit SetBasic write_time. While
// sticky, QUERY_INFO returns the chosen value regardless of subsequent
// writes on this handle.
//
// Takes OpenFile.mu (write). Callers that already hold the write lock should
// call setSmbStickyWriteTimeLocked instead.
func setSmbStickyWriteTime(openFile *OpenFile, t time.Time) {
	if openFile == nil {
		return
	}
	openFile.mu.Lock()
	defer openFile.mu.Unlock()
	setSmbStickyWriteTimeLocked(openFile, t)
}

// applySmbDelayedWriteOverride overlays the SMB-visible LastWriteTime on
// top of a freshly-read metadata.File for QUERY_INFO responses. Returns
// the file unchanged when the handle has no delayed-write state.
//
// Takes OpenFile.mu (read) — must observe a consistent snapshot of the
// SmbWrite* fields against concurrent armSmbDelayedWrite / flushSmbDelayedWrite
// on the same handle (#606).
func applySmbDelayedWriteOverride(openFile *OpenFile, file *metadata.File) {
	if openFile == nil || file == nil {
		return
	}
	openFile.mu.RLock()
	defer openFile.mu.RUnlock()
	if openFile.SmbStickyWriteTime != nil {
		file.Mtime = *openFile.SmbStickyWriteTime
		return
	}
	if !openFile.SmbWriteTriggered {
		return
	}
	if !openFile.SmbWriteFlushAt.IsZero() && time.Now().Before(openFile.SmbWriteFlushAt) {
		if openFile.SmbWritePreMtime != nil {
			file.Mtime = *openFile.SmbWritePreMtime
		}
		return
	}
	if openFile.SmbWriteFlushMtime != nil {
		file.Mtime = *openFile.SmbWriteFlushMtime
	}
}

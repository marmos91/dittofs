package logblob

import (
	"context"
	"errors"
	"fmt"
	iofs "io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/marmos91/dittofs/pkg/block"
)

const (
	// defaultSizeCap is the default active-blob size limit (1 GiB).
	defaultSizeCap int64 = 1 << 30

	// blobSuffix is the file extension for every log-blob file.
	blobSuffix = ".blob"

	// blobIDFmt is the zero-padded decimal format for blob IDs.
	// 16 digits gives a lexicographic order that matches numeric order up to
	// 10^16 − 1 blobs — far more than any real deployment will ever create.
	blobIDFmt = "%016d"
)

// Errors returned by Manager operations.
var (
	// ErrClosed is returned when an operation is attempted on a closed Manager.
	ErrClosed = errors.New("logblob: manager closed")

	// ErrBlobNotFound is returned when ReadAt references a blob ID that does
	// not exist in the Manager's directory.
	ErrBlobNotFound = errors.New("logblob: blob not found")

	// ErrEvicted is returned when an operation targets a blob that has been evicted.
	ErrEvicted = errors.New("logblob: blob evicted")

	// ErrActiveBlob is returned when EvictBlob is called on the active (unsealed) blob.
	ErrActiveBlob = errors.New("logblob: cannot evict the active blob")

	// ErrUnsyncedBytes is returned when EvictBlob is called on a blob that has
	// not been fully synced to durable storage according to the caller's synced function.
	ErrUnsyncedBytes = errors.New("logblob: blob has unsynced bytes")
)

// Options configures a Manager.
type Options struct {
	// SizeCap is the maximum number of bytes the active blob may hold
	// before the Manager rotates to a new blob. Zero or negative defaults
	// to 1 GiB.
	SizeCap int64
}

// BlobInfo describes a single log blob in the Manager's directory.
type BlobInfo struct {
	// LogBlobID is the blob's stable identifier, matching the filename stem
	// (e.g. "0000000000000003" for "0000000000000003.blob").
	LogBlobID string
	// Size is the number of bytes currently stored in the blob.
	Size int64
	// Active is true for the one blob that currently accepts appends.
	Active bool
}

// Manager owns a directory of raw log blobs.
//
// Concurrency contract:
//   - [Append] and [Rotate] are mutex-serialized (one at a time).
//   - [ReadAt] takes the mutex briefly to snapshot the active blob's identity
//     and file descriptor, then releases it before the actual pread(2). This
//     allows concurrent reads and appends on non-overlapping byte ranges.
//   - The race detector is clean: all shared state is accessed under mu, and
//     positioned file I/O (pread/pwrite via [os.File.ReadAt]/[os.File.WriteAt])
//     does not share a file cursor.
type Manager struct {
	dir     string
	sizeCap int64

	// mu serializes Append, Rotate, Close, and the brief state snapshots in
	// ReadAt/fdForBlob. It is never held across blocking I/O.
	mu sync.Mutex

	// nextID is the uint64 counter for the next blob to be created.
	nextID uint64
	// activeID is the blobID string (e.g. "0000000000000002") of the current
	// active blob.
	activeID string
	// activeFD is the open O_RDWR file for the active blob.
	activeFD *os.File
	// activeTail is the number of bytes already written to the active blob.
	// New Appends write at this offset and increment it.
	activeTail int64

	closed bool

	// sealedMu guards sealedFDs. Lock order: mu must always be acquired before
	// sealedMu when both are needed (rotateLocked holds mu and acquires sealedMu).
	// Never acquire mu while holding sealedMu.
	sealedMu  sync.Mutex
	sealedFDs map[string]*os.File // blobID → open O_RDONLY fd (lazy)

	// evictedBlobs records the IDs of blobs that have been evicted.
	// Protected by sealedMu (same mutex that guards sealedFDs).
	evictedBlobs map[string]struct{}
}

// Open opens (or creates) a Manager rooted at dir with the given options.
//
// On a fresh directory an initial blob ("0000000000000000.blob") is created.
// On reopen of an existing directory the highest-numbered blob is treated as
// the active blob and subsequent Appends resume at its current tail.
func Open(dir string, opts Options) (*Manager, error) {
	sizeCap := opts.SizeCap
	if sizeCap <= 0 {
		sizeCap = defaultSizeCap
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("logblob: mkdir %q: %w", dir, err)
	}

	ids, err := scanBlobIDs(dir)
	if err != nil {
		return nil, err
	}

	m := &Manager{
		dir:          dir,
		sizeCap:      sizeCap,
		sealedFDs:    make(map[string]*os.File),
		evictedBlobs: make(map[string]struct{}),
	}

	if len(ids) == 0 {
		// Fresh directory: create the first blob.
		if err := m.createNewActive(); err != nil {
			return nil, err
		}
	} else {
		// Reopen: the highest ID is the active blob.
		activeIDUint := ids[len(ids)-1]
		m.nextID = activeIDUint + 1
		m.activeID = fmt.Sprintf(blobIDFmt, activeIDUint)

		path := filepath.Join(dir, m.activeID+blobSuffix)
		fd, err := os.OpenFile(path, os.O_RDWR, 0o644)
		if err != nil {
			return nil, fmt.Errorf("logblob: open active blob %q: %w", path, err)
		}
		fi, err := fd.Stat()
		if err != nil {
			_ = fd.Close()
			return nil, fmt.Errorf("logblob: stat active blob %q: %w", path, err)
		}
		m.activeFD = fd
		m.activeTail = fi.Size()
	}

	return m, nil
}

// Append writes the raw bytes p to the tail of the active blob and returns
// the LocalChunkLocation that identifies where those bytes were stored.
//
// The operation is mutex-serialized. Bytes are stored raw — no framing, no
// checksum. If the write would cause the active blob to exceed SizeCap, the
// blob is rotated first (a new active blob is created and p is written there).
//
// An empty payload is rejected (callers must not append zero bytes).
func (m *Manager) Append(ctx context.Context, p []byte) (block.LocalChunkLocation, error) {
	if ctx.Err() != nil {
		return block.LocalChunkLocation{}, ctx.Err()
	}
	if len(p) == 0 {
		return block.LocalChunkLocation{}, errors.New("logblob: Append: empty payload")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return block.LocalChunkLocation{}, ErrClosed
	}

	// Rotate if this write would push the active blob past SizeCap.
	if m.activeTail+int64(len(p)) > m.sizeCap {
		if err := m.rotateLocked(); err != nil {
			return block.LocalChunkLocation{}, fmt.Errorf("logblob: rotate before append: %w", err)
		}
	}

	offset := m.activeTail
	blobID := m.activeID

	n, err := m.activeFD.WriteAt(p, offset)
	if err != nil {
		return block.LocalChunkLocation{}, fmt.Errorf("logblob: WriteAt %s@%d: %w", blobID, offset, err)
	}
	if n != len(p) {
		return block.LocalChunkLocation{}, fmt.Errorf("logblob: short write: %d of %d bytes", n, len(p))
	}
	m.activeTail += int64(n)

	return block.LocalChunkLocation{
		LogBlobID: blobID,
		RawOffset: offset,
		RawLength: int64(n),
	}, nil
}

// ReadAt reads exactly loc.RawLength bytes from blob loc.LogBlobID starting at
// byte offset loc.RawOffset into dst (which must have len ≥ loc.RawLength).
//
// ReadAt is safe to call concurrently with Append. It acquires the mutex
// briefly to snapshot the active blob's identity and file descriptor, then
// releases it before calling pread(2) — so Append is never blocked by reads.
func (m *Manager) ReadAt(ctx context.Context, loc block.LocalChunkLocation, dst []byte) (int, error) {
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	if loc.RawLength == 0 {
		return 0, nil
	}
	if int64(len(dst)) < loc.RawLength {
		return 0, fmt.Errorf("logblob: ReadAt: dst too small: have %d bytes, need %d", len(dst), loc.RawLength)
	}

	fd, err := m.fdForBlob(loc.LogBlobID)
	if err != nil {
		return 0, err
	}

	n, err := fd.ReadAt(dst[:loc.RawLength], loc.RawOffset)
	if err != nil {
		return n, fmt.Errorf("logblob: ReadAt %s@%d: %w", loc.LogBlobID, loc.RawOffset, err)
	}
	return n, nil
}

// Rotate seals the current active blob and creates a fresh one. Subsequent
// Appends will target the new blob starting at offset 0.
//
// Callers may use Rotate to segment the log at application-defined boundaries
// (e.g. before a snapshot) without waiting for SizeCap to be reached.
func (m *Manager) Rotate() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrClosed
	}
	return m.rotateLocked()
}

// Sync fsyncs the active blob to durable storage. Durability cadence is
// caller-controlled; call Sync at commit boundaries.
func (m *Manager) Sync() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrClosed
	}
	fd := m.activeFD
	m.mu.Unlock()

	if err := fd.Sync(); err != nil {
		return fmt.Errorf("logblob: fsync: %w", err)
	}
	return nil
}

// ListBlobs returns a BlobInfo for every blob in the Manager's directory,
// including the currently active blob, sorted by blob ID (creation order).
func (m *Manager) ListBlobs() ([]BlobInfo, error) {
	// Snapshot active state under mu.
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, ErrClosed
	}
	activeID := m.activeID
	activeTail := m.activeTail
	m.mu.Unlock()

	entries, err := os.ReadDir(m.dir)
	if err != nil {
		return nil, fmt.Errorf("logblob: readdir: %w", err)
	}

	var infos []BlobInfo
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, blobSuffix) {
			continue
		}
		stem := strings.TrimSuffix(name, blobSuffix)
		if _, err := strconv.ParseUint(stem, 10, 64); err != nil {
			continue // skip unrecognized files
		}
		blobID := stem

		var size int64
		if blobID == activeID {
			size = activeTail
		} else {
			fi, err := e.Info()
			if err != nil {
				return nil, fmt.Errorf("logblob: info %s: %w", name, err)
			}
			size = fi.Size()
		}

		infos = append(infos, BlobInfo{
			LogBlobID: blobID,
			Size:      size,
			Active:    blobID == activeID,
		})
	}

	return infos, nil
}

// EvictBlob removes a sealed blob from disk and the in-memory fd cache.
// The caller provides a synced function that reports whether logBlobID's
// bytes have been durably stored elsewhere; if it returns false the eviction
// is refused with ErrUnsyncedBytes.
//
// EvictBlob is idempotent: if the blob has already been evicted the call
// returns nil.
//
// ErrActiveBlob is returned if logBlobID is the current active blob.
func (m *Manager) EvictBlob(ctx context.Context, logBlobID string, synced func(string) bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrClosed
	}
	if logBlobID == m.activeID {
		m.mu.Unlock()
		return ErrActiveBlob
	}
	m.mu.Unlock()

	// Idempotency check: if already evicted, return nil without invoking synced.
	// This must come before synced() so a second call with synced=false still
	// returns nil, matching the documented idempotency contract.
	m.sealedMu.Lock()
	if _, already := m.evictedBlobs[logBlobID]; already {
		m.sealedMu.Unlock()
		return nil
	}
	m.sealedMu.Unlock()

	if !synced(logBlobID) {
		return ErrUnsyncedBytes
	}

	// Mark evicted and grab the cached fd if any.
	// Double-check under sealedMu in case a concurrent EvictBlob won the race
	// between our early check above and now.
	m.sealedMu.Lock()
	if _, already := m.evictedBlobs[logBlobID]; already {
		m.sealedMu.Unlock()
		return nil
	}
	m.evictedBlobs[logBlobID] = struct{}{}
	fd := m.sealedFDs[logBlobID]
	delete(m.sealedFDs, logBlobID)
	m.sealedMu.Unlock()

	// Close the cached fd if one was open.
	if fd != nil {
		_ = fd.Close()
	}

	// Remove the file; ignore ErrNotExist so repeated calls are safe.
	path := filepath.Join(m.dir, logBlobID+blobSuffix)
	if err := os.Remove(path); err != nil && !errors.Is(err, iofs.ErrNotExist) {
		return fmt.Errorf("logblob: remove blob %q: %w", path, err)
	}
	return nil
}

// Recover truncates a blob to validUpToOffset, discarding any bytes beyond
// that offset. It is intended for crash-recovery scenarios where a torn
// write left the tail of a blob in an indeterminate state.
//
// For the active blob the in-memory activeTail is also updated.
// For sealed blobs the cached fd (if any) is closed and removed from the
// cache before truncation so the next read re-opens the file at the new size.
//
// validUpToOffset must be ≥ 0 and ≤ the blob's current size. Passing an
// offset beyond the current size is rejected with an error to prevent POSIX
// ftruncate from zero-filling the file.
//
// Concurrency precondition: callers must quiesce concurrent [ReadAt] and
// [Append] calls that target logBlobID before calling Recover — the same
// contract as [Close]. Recover is designed for startup-time recovery where no
// concurrent I/O is in flight on the target blob.
func (m *Manager) Recover(ctx context.Context, logBlobID string, validUpToOffset int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if validUpToOffset < 0 {
		return fmt.Errorf("logblob: Recover: negative offset %d", validUpToOffset)
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrClosed
	}

	if logBlobID == m.activeID {
		// Guard against extending the file: POSIX ftruncate zero-fills when the
		// target length exceeds the current file size, which would silently corrupt
		// a crash-recovery operation.
		if validUpToOffset > m.activeTail {
			m.mu.Unlock()
			return fmt.Errorf("logblob: Recover: offset %d exceeds active blob size %d", validUpToOffset, m.activeTail)
		}
		// Truncate active blob while holding mu (acceptable for a startup op).
		if err := m.activeFD.Truncate(validUpToOffset); err != nil {
			m.mu.Unlock()
			return fmt.Errorf("logblob: truncate active blob %q: %w", logBlobID, err)
		}
		m.activeTail = validUpToOffset
		m.mu.Unlock()
		return nil
	}
	m.mu.Unlock()

	// Sealed blob: evict from fd cache before truncating.
	m.sealedMu.Lock()
	if _, evicted := m.evictedBlobs[logBlobID]; evicted {
		m.sealedMu.Unlock()
		return fmt.Errorf("logblob: Recover: %w: %s", ErrEvicted, logBlobID)
	}
	fd := m.sealedFDs[logBlobID]
	delete(m.sealedFDs, logBlobID)
	m.sealedMu.Unlock()

	if fd != nil {
		_ = fd.Close()
	}

	path := filepath.Join(m.dir, logBlobID+blobSuffix)

	// Guard against extending the file: POSIX truncate zero-fills when the
	// target length exceeds the current file size.
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("logblob: stat sealed blob %q: %w", logBlobID, err)
	}
	if validUpToOffset > fi.Size() {
		return fmt.Errorf("logblob: Recover: offset %d exceeds sealed blob size %d", validUpToOffset, fi.Size())
	}

	if err := os.Truncate(path, validUpToOffset); err != nil {
		return fmt.Errorf("logblob: truncate sealed blob %q: %w", logBlobID, err)
	}
	return nil
}

// Close closes the Manager and all open file descriptors.
//
// Close is idempotent. After Close, all operations return ErrClosed.
// Close does not guarantee that in-progress ReadAt calls finish before file
// descriptors are closed — callers must quiesce concurrent I/O before calling
// Close (the same contract as [os.File.Close]).
func (m *Manager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	activeFD := m.activeFD
	m.activeFD = nil
	m.mu.Unlock()

	var firstErr error
	if activeFD != nil {
		if err := activeFD.Close(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("logblob: close active fd: %w", err)
		}
	}

	m.sealedMu.Lock()
	for id, fd := range m.sealedFDs {
		if err := fd.Close(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("logblob: close sealed fd %s: %w", id, err)
		}
	}
	m.sealedFDs = nil
	m.sealedMu.Unlock()

	return firstErr
}

// ---- internal helpers ----

// rotateLocked seals the current active blob (adds its fd to the sealedFDs
// cache without closing it, so in-flight ReadAt calls remain valid) and
// creates a new active blob.
//
// Caller MUST hold m.mu.
func (m *Manager) rotateLocked() error {
	// Hand the old active fd to the sealed cache so it stays readable.
	oldID := m.activeID
	oldFD := m.activeFD

	// Fsync the blob before sealing it: once rotated it is immutable and never
	// becomes the active blob again, while Sync only fsyncs the active blob. A
	// caller that flushes at commit boundaries must not silently lose bytes that
	// a size-cap rotation sealed between commits, so make rotation a durability
	// boundary here.
	if err := oldFD.Sync(); err != nil {
		return fmt.Errorf("fsync blob %q before seal: %w", oldID, err)
	}

	m.sealedMu.Lock()
	m.sealedFDs[oldID] = oldFD
	m.sealedMu.Unlock()

	return m.createNewActive()
}

// createNewActive allocates the next blob ID, creates the file, and wires up
// activeFD / activeID / activeTail / nextID.
//
// Caller MUST hold m.mu.
func (m *Manager) createNewActive() error {
	id := m.nextID
	m.nextID++
	newID := fmt.Sprintf(blobIDFmt, id)
	path := filepath.Join(m.dir, newID+blobSuffix)

	fd, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return fmt.Errorf("create blob %q: %w", path, err)
	}
	m.activeID = newID
	m.activeFD = fd
	m.activeTail = 0
	return nil
}

// fdForBlob returns an open file descriptor for the blob identified by blobID.
//
// If blobID is the current active blob the active fd is returned (no sealedMu
// contention on the happy path). For sealed blobs a read-only fd is opened
// lazily and cached in sealedFDs.
//
// The mutex is held only briefly (to snapshot activeID/activeFD), then
// released before any blocking I/O — so Append is never blocked by reads.
func (m *Manager) fdForBlob(blobID string) (*os.File, error) {
	// Fast path: snapshot active state under mu.
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, ErrClosed
	}
	if blobID == m.activeID {
		fd := m.activeFD
		m.mu.Unlock()
		return fd, nil
	}
	m.mu.Unlock()

	// Sealed blob: check / populate the read-only fd cache.
	m.sealedMu.Lock()
	defer m.sealedMu.Unlock()

	if _, evicted := m.evictedBlobs[blobID]; evicted {
		return nil, ErrEvicted
	}

	if fd, ok := m.sealedFDs[blobID]; ok {
		return fd, nil
	}

	path := filepath.Join(m.dir, blobID+blobSuffix)
	fd, err := os.Open(path)
	if err != nil {
		if errors.Is(err, iofs.ErrNotExist) {
			return nil, fmt.Errorf("logblob: %w: %s", ErrBlobNotFound, blobID)
		}
		return nil, fmt.Errorf("logblob: open blob %q: %w", path, err)
	}
	m.sealedFDs[blobID] = fd
	return fd, nil
}

// scanBlobIDs reads dir and returns the uint64 IDs of every *.blob file whose
// stem is a valid zero-padded decimal, sorted ascending.
func scanBlobIDs(dir string) ([]uint64, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("logblob: readdir %q: %w", dir, err)
	}

	var ids []uint64
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, blobSuffix) {
			continue
		}
		stem := strings.TrimSuffix(name, blobSuffix)
		id, err := strconv.ParseUint(stem, 10, 64)
		if err != nil {
			continue // skip unrecognized files
		}
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids, nil
}

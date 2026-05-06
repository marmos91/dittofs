// Package migrate provides the offline block-store migration journal
// and share-tree walk helper for Phase 14.
//
// The package lives at pkg/ rather than cmd/ because the controlplane
// REST handler (Plan 14-06) needs to import the journal type to surface
// migration progress without taking a round-trip through dfsctl —
// Go's build system forbids pkg/ and internal/ from importing cmd/.
//
// Components:
//
//   - Journal: an append-only JSON-line log + periodic snapshot, one per
//     share. Append + Snapshot + Replay support resumability after a
//     mid-share crash (D-A1..D-A4).
//   - WalkShareFiles: a recursive helper composing
//     metadata.MetadataStore.GetRootHandle + ListChildren + GetFile to
//     visit every regular file in a share.
package migrate

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/marmos91/dittofs/pkg/blockstore"
)

// JournalFile is the on-disk filename for the append-only journal
// component (D-A2). Lives at {dir}/.migration-state.jsonl, one line per
// committed file.
const JournalFile = ".migration-state.jsonl"

// SnapshotFile is the on-disk filename for the compacted snapshot
// component (D-A2). Lives at {dir}/.migration-state.snapshot.json,
// rotated atomically every DefaultSnapshotInterval Append calls (D-A3).
const SnapshotFile = ".migration-state.snapshot.json"

// DefaultSnapshotInterval is the per-N entries snapshot trigger (D-A3).
// Tunable via OpenJournalWithInterval for tests; not a documented
// operator knob.
const DefaultSnapshotInterval = 1000

// JournalEntryVersion is the schema version stamped on every entry.
// Bump only on a non-forward-compatible schema change. Forward-compat
// today is achieved via `omitempty` on every field.
const JournalEntryVersion = 1

// JournalEntry is one line in the append-only log. Each entry represents
// a file-level commit (D-A1 — atomic unit = one file). Fields use
// `omitempty` so older readers tolerate forward field additions.
type JournalEntry struct {
	// Version is the schema version stamped at write time. Allows
	// future readers to detect forward-compat boundaries.
	Version int `json:"v"`

	// Kind discriminates the entry semantics:
	//   - "file_done":   file was re-chunked + committed in one txn.
	//   - "file_skipped": file was skipped (e.g., already in CAS layout).
	Kind string `json:"kind"`

	// Timestamp is the wall-clock time the entry was appended. UTC.
	Timestamp time.Time `json:"ts"`

	// FileHandle is the metadata.FileHandle (string-encoded) the
	// migration loop committed. Used as the dedup key on resume.
	FileHandle string `json:"handle"`

	// PayloadID is the legacy content identifier ({shareName}/{path}
	// today). Captured for forensic context; not used for resume.
	PayloadID string `json:"payload_id"`

	// Blocks is the post-migration BlockRef list the metadata txn
	// persisted into FileAttr.Blocks. Captured for audit and the
	// integrity check (Plan 14-05).
	Blocks []blockstore.BlockRef `json:"blocks,omitempty"`

	// ObjectID is the BLAKE3 Merkle root over Blocks (Phase 13 D-14
	// backfill).
	ObjectID blockstore.ObjectID `json:"object_id,omitempty"`

	// BytesUploaded is the count of bytes the migration loop PUT to
	// the remote store for this file (zero when every chunk was a
	// dedup hit).
	BytesUploaded uint64 `json:"bytes_uploaded,omitempty"`

	// BytesDeduped is the count of bytes that hit on GetByHash and
	// were skipped at upload time.
	BytesDeduped uint64 `json:"bytes_deduped,omitempty"`
}

// ErrJournalReadOnly is returned by mutating methods on a journal that
// was opened via OpenJournalReadOnly. The REST status handler (Plan
// 14-06) opens read-only because it may run while the migration tool
// holds the write side.
var ErrJournalReadOnly = errors.New("migrate: journal opened read-only — mutation rejected")

// Journal is a per-share resumability state file. One Journal corresponds
// to one share's migration; the writer side (the migration loop) holds
// at most one open Journal handle, and the journal's mutex serializes
// Append + Snapshot.
//
// On-disk layout:
//
//	{dir}/.migration-state.snapshot.json   sorted-by-handle slice of done entries
//	{dir}/.migration-state.jsonl           append-only log of new commits since snapshot
//	{dir}/.migration-state.snapshot.json.tmp   transient — atomic-rename target
//
// Resume semantics: load snapshot first, then replay journal tail; the
// in-memory `done` map is the union (D-A4 — trust the surviving prefix).
type Journal struct {
	mu sync.Mutex

	dir           string
	jf            *os.File
	readOnly      bool
	snapshotEvery int
	appended      int
	done          map[string]JournalEntry
}

// OpenJournal opens (or creates) the journal at dir. On open, it loads
// any existing snapshot, then replays the journal tail; the in-memory
// done-set is the union. The directory is created via os.MkdirAll —
// callers do not need to ensure existence.
//
// D-A3: snapshot interval defaults to DefaultSnapshotInterval. Use
// OpenJournalWithInterval to override (test-only path).
func OpenJournal(dir string) (*Journal, error) {
	return openJournal(dir, DefaultSnapshotInterval, false)
}

// OpenJournalWithInterval is the test-injection variant of OpenJournal:
// it forces a custom snapshot trigger so tests can exercise the rotation
// path without writing 1000 entries.
func OpenJournalWithInterval(dir string, snapshotEvery int) (*Journal, error) {
	if snapshotEvery <= 0 {
		snapshotEvery = DefaultSnapshotInterval
	}
	return openJournal(dir, snapshotEvery, false)
}

// OpenJournalReadOnly opens the journal in read-only mode. Append +
// Snapshot return ErrJournalReadOnly. Used by the REST handler (Plan
// 14-06) — it may run while a migration tool holds the write side, so
// it MUST NOT rotate or truncate.
//
// Concurrent reads against an active writer are well-defined by POSIX
// semantics so long as we never truncate from this handle.
func OpenJournalReadOnly(dir string) (*Journal, error) {
	return openJournal(dir, DefaultSnapshotInterval, true)
}

func openJournal(dir string, snapshotEvery int, readOnly bool) (*Journal, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("migrate: mkdir %q: %w", dir, err)
	}
	j := &Journal{
		dir:           dir,
		snapshotEvery: snapshotEvery,
		readOnly:      readOnly,
		done:          make(map[string]JournalEntry),
	}
	// 1. Load snapshot (best-effort — corrupt / missing falls through).
	if err := j.loadSnapshot(); err != nil {
		return nil, err
	}
	// 2. Replay journal tail.
	jpath := filepath.Join(dir, JournalFile)
	if err := j.replayInto(jpath); err != nil {
		return nil, err
	}
	// 3. Open the journal file for append (or read-only).
	if readOnly {
		// Open the journal file lazily — Replay() reopens it when
		// callers need it. We do not hold a writable handle in
		// read-only mode so concurrent rotations from the writer side
		// do not invalidate our reads.
		return j, nil
	}
	f, err := os.OpenFile(jpath, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("migrate: open journal %q: %w", jpath, err)
	}
	j.jf = f
	return j, nil
}

// loadSnapshot reads the snapshot file (if any) and rebuilds the done
// map. A missing or corrupt snapshot is non-fatal — Replay will pick up
// the journal tail and the union is authoritative.
func (j *Journal) loadSnapshot() error {
	spath := filepath.Join(j.dir, SnapshotFile)
	data, err := os.ReadFile(spath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("migrate: read snapshot %q: %w", spath, err)
	}
	if len(data) == 0 {
		return nil
	}
	var entries []JournalEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		// Corrupt snapshot — D-A4 trust journal head. Treat the
		// snapshot as absent and rely on the journal tail.
		return nil
	}
	for _, e := range entries {
		j.done[e.FileHandle] = e
	}
	return nil
}

// replayInto streams every JSON line from jpath into the done map.
// Lines that fail to parse are treated as truncated tail per D-A4: we
// stop replay at the first parse error and keep the surviving prefix
// as authoritative.
func (j *Journal) replayInto(jpath string) error {
	f, err := os.Open(jpath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("migrate: open journal for replay %q: %w", jpath, err)
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	// Allow large entries (BlockRef list can be sizable).
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var e JournalEntry
		if err := json.Unmarshal(line, &e); err != nil {
			// Truncated tail; surviving prefix wins (D-A4).
			break
		}
		j.done[e.FileHandle] = e
	}
	// scanner errors (other than EOF) are tolerated — same rationale.
	return nil
}

// Append serializes entry, writes one JSON line + newline, fsyncs, and
// updates the in-memory done map. Auto-fires Snapshot when the
// post-Append append counter reaches snapshotEvery.
func (j *Journal) Append(e JournalEntry) error {
	if j.readOnly {
		return ErrJournalReadOnly
	}
	if e.Version == 0 {
		e.Version = JournalEntryVersion
	}
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now().UTC()
	}

	j.mu.Lock()
	defer j.mu.Unlock()

	if j.jf == nil {
		return errors.New("migrate: journal not open")
	}
	data, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("migrate: marshal entry: %w", err)
	}
	data = append(data, '\n')
	if _, err := j.jf.Write(data); err != nil {
		return fmt.Errorf("migrate: write journal: %w", err)
	}
	if err := j.jf.Sync(); err != nil {
		return fmt.Errorf("migrate: fsync journal: %w", err)
	}

	j.done[e.FileHandle] = e
	j.appended++

	if j.appended >= j.snapshotEvery {
		if err := j.snapshotLocked(); err != nil {
			return err
		}
	}
	return nil
}

// IsFileDone returns true when the journal has seen a successful commit
// for the given file handle. Used by the migration loop to skip already-
// migrated files on resume (D-A4).
func (j *Journal) IsFileDone(handle string) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	_, ok := j.done[handle]
	return ok
}

// Snapshot rotates the journal: marshal the sorted done set to a
// transient file, fsync, atomic-rename to SnapshotFile, then truncate
// the journal log. Auto-fired by Append; callers may invoke directly to
// force a final snapshot at the end of a successful run (D-A2).
func (j *Journal) Snapshot() error {
	if j.readOnly {
		return ErrJournalReadOnly
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.snapshotLocked()
}

// sortedDoneEntriesLocked returns the in-memory done set as a slice
// sorted by FileHandle. Caller must hold j.mu.
func (j *Journal) sortedDoneEntriesLocked() []JournalEntry {
	handles := make([]string, 0, len(j.done))
	for h := range j.done {
		handles = append(handles, h)
	}
	sort.Strings(handles)
	out := make([]JournalEntry, 0, len(handles))
	for _, h := range handles {
		out = append(out, j.done[h])
	}
	return out
}

// snapshotLocked is the locked-by-caller variant; auto-fired Snapshot
// via Append already holds j.mu.
func (j *Journal) snapshotLocked() error {
	// 1. Sorted slice for stable output.
	entries := j.sortedDoneEntriesLocked()
	data, err := json.Marshal(entries)
	if err != nil {
		return fmt.Errorf("migrate: marshal snapshot: %w", err)
	}

	// 2. Write to transient file, fsync, atomic-rename.
	spath := filepath.Join(j.dir, SnapshotFile)
	tmp := spath + ".tmp"
	tf, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("migrate: open snapshot tmp %q: %w", tmp, err)
	}
	if _, err := tf.Write(data); err != nil {
		_ = tf.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("migrate: write snapshot tmp: %w", err)
	}
	if err := tf.Sync(); err != nil {
		_ = tf.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("migrate: fsync snapshot tmp: %w", err)
	}
	if err := tf.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("migrate: close snapshot tmp: %w", err)
	}
	if err := os.Rename(tmp, spath); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("migrate: rename snapshot %q: %w", spath, err)
	}
	// fsync the parent directory so the rename is durable across crashes
	// (POSIX requirement on Linux/macOS). No-op + lock-conflict on Windows
	// where opening a directory handle blocks subsequent file truncates
	// in the same directory; skip there.
	syncDir(j.dir)

	// 3. Truncate the journal file. On Windows, files opened with
	// O_APPEND deny truncate-via-handle, so close + truncate-via-path
	// + reopen. On POSIX the in-place handle truncate is fine but the
	// close/reopen path also works, so we use it unconditionally.
	if j.jf != nil {
		jpath := filepath.Join(j.dir, JournalFile)
		if err := j.jf.Close(); err != nil {
			return fmt.Errorf("migrate: close journal pre-truncate: %w", err)
		}
		j.jf = nil
		if err := os.Truncate(jpath, 0); err != nil {
			return fmt.Errorf("migrate: truncate journal: %w", err)
		}
		f, err := os.OpenFile(jpath, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o644)
		if err != nil {
			return fmt.Errorf("migrate: reopen journal post-truncate: %w", err)
		}
		j.jf = f
		if err := j.jf.Sync(); err != nil {
			return fmt.Errorf("migrate: fsync journal post-truncate: %w", err)
		}
	}
	j.appended = 0
	return nil
}

// Replay returns the in-memory done set as a sorted-by-handle slice.
// Useful for the REST status handler (Plan 14-06) and for tests
// asserting the surviving entries after a crash + reopen.
func (j *Journal) Replay() ([]JournalEntry, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.sortedDoneEntriesLocked(), nil
}

// Aggregate is a convenience for the REST status handler (Plan 14-06).
// Returns the replayed entries plus presence flags + last-commit timestamp.
func (j *Journal) Aggregate() (entries []JournalEntry, journalPresent, snapshotPresent bool, lastCommitAt time.Time) {
	jpath := filepath.Join(j.dir, JournalFile)
	if fi, err := os.Stat(jpath); err == nil && fi.Size() > 0 {
		journalPresent = true
	}
	spath := filepath.Join(j.dir, SnapshotFile)
	if _, err := os.Stat(spath); err == nil {
		snapshotPresent = true
	}
	entries, _ = j.Replay()
	for _, e := range entries {
		if e.Timestamp.After(lastCommitAt) {
			lastCommitAt = e.Timestamp
		}
	}
	return entries, journalPresent, snapshotPresent, lastCommitAt
}

// Close releases the journal file handle. Idempotent; safe to call on a
// read-only journal (no-op).
func (j *Journal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.jf == nil {
		return nil
	}
	err := j.jf.Close()
	j.jf = nil
	return err
}

// JournalSize returns the current append-log size on disk. Test helper
// for verifying the truncate-to-zero invariant after Snapshot.
func (j *Journal) JournalSize() (int64, error) {
	jpath := filepath.Join(j.dir, JournalFile)
	fi, err := os.Stat(jpath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	return fi.Size(), nil
}

// ContextSentinel is referenced from tests that want the migrate package
// to reach context.Canceled without a transitive dependency on
// pkg/context — kept exported here as a no-op anchor.
var _ context.Context = (context.Context)(nil)

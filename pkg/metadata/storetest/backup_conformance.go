package storetest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// BackupTestStore is the union interface required by the backup conformance
// suite: a store must be a full MetadataStore (for population), a Backupable
// (for Backup/Restore exercise), and an io.Closer (for per-test cleanup).
// All three engines (memory, badger, postgres) satisfy this in Phase 2.
type BackupTestStore interface {
	metadata.MetadataStore
	metadata.Backupable
	io.Closer
}

// BackupStoreFactory creates a fresh backup-capable store for each test.
// Called at least twice per sub-test: once for the source (populate + Backup)
// and once for the destination (Restore + enumerate). Implementations MUST
// return fully-independent instances (distinct tmp dirs / distinct PG
// databases / distinct memory instances).
type BackupStoreFactory func(t *testing.T) BackupTestStore

// BackupSuiteOptions tunes the conformance run per engine.
//
//   - SkipConcurrentWriter: set true for engines that cannot mutate under
//     the backup snapshot primitive (there are none in Phase 2; reserved
//     for a future read-only store). Memory, Badger, and Postgres all set
//     this false.
//   - ConcurrentWriterDuration: how long the parallel writer runs during
//     Backup. Defaults to 100ms if zero.
type BackupSuiteOptions struct {
	SkipConcurrentWriter     bool
	ConcurrentWriterDuration time.Duration
}

// defaultConcurrentWriterDuration is the default time window during which the
// ConcurrentWriter sub-test runs parallel mutations against the source store
// while Backup is in flight. 100ms is long enough to generate contention on
// every engine without making the suite slow.
const defaultConcurrentWriterDuration = 100 * time.Millisecond

// RunBackupConformanceSuite runs the Phase 2 backup/restore conformance suite
// against the provided factory. Each sub-test gets a fresh store instance.
//
// The suite covers five scenarios (D-08):
//  1. RoundTrip:           populate → Backup → Restore → enumerate, assert equal
//  2. ConcurrentWriter:    writes during Backup; assert snapshot consistent
//  3. Corruption:          truncate/flip bytes → Restore returns ErrRestoreCorrupt
//  4. NonEmptyDest:        populate dest → Restore returns ErrRestoreDestinationNotEmpty
//  5. PayloadIDSet:        enumerate restored payload refs, assert == returned set
func RunBackupConformanceSuite(t *testing.T, factory BackupStoreFactory) {
	RunBackupConformanceSuiteWithOptions(t, factory, BackupSuiteOptions{})
}

// RunBackupConformanceSuiteWithOptions is the options-accepting variant used
// by engines that need to disable a particular sub-test (no Phase-2 engine
// actually uses this path — reserved for read-only stores).
func RunBackupConformanceSuiteWithOptions(t *testing.T, factory BackupStoreFactory, opts BackupSuiteOptions) {
	t.Helper()

	t.Run("RoundTrip", func(t *testing.T) { testBackupRoundTrip(t, factory) })
	if !opts.SkipConcurrentWriter {
		t.Run("ConcurrentWriter", func(t *testing.T) { testBackupConcurrentWriter(t, factory, opts) })
	}
	t.Run("Corruption", func(t *testing.T) { testBackupCorruption(t, factory) })
	t.Run("NonEmptyDest", func(t *testing.T) { testBackupNonEmptyDest(t, factory) })
	t.Run("PayloadIDSet", func(t *testing.T) { testBackupPayloadIDSet(t, factory) })
}

// ============================================================================
// Populate Helper
// ============================================================================

// backupTestLayout summarises the data written into a source store by
// populateForBackup. Wave-2 drivers use the returned structure to assert
// enumerability after round-trip.
type backupTestLayout struct {
	// shareNames is the sorted list of share names created.
	shareNames []string
	// files maps share name → file path → PayloadID. Every regular file in
	// the populated store appears here with a non-empty PayloadID.
	files map[string]map[string]string
	// expectedPayloadIDs is the set of PayloadIDs the store contains; Backup
	// MUST return a PayloadIDSet equal to this set.
	expectedPayloadIDs metadata.PayloadIDSet
	// fileHandles maps PayloadID back to the source-store FileHandle so tests
	// can `GetFile` and compare attributes after restore (when the destination
	// exposes the same handle scheme — it does, via GenerateHandle).
	fileHandles map[string]metadata.FileHandle
}

// populateForBackup writes a deterministic tree into store:
//   - 2 shares ("/backup-a", "/backup-b")
//   - each share has 3 nested directories under the root ("dir-0", "dir-1", "dir-2")
//   - each directory contains 2 regular files ("file-0", "file-1") with distinct
//     PayloadIDs of the form "payload-<share-suffix>-<dir>-<file>"
//   - total: 2 shares × 3 dirs × 2 files = 12 regular files + 2 roots + 6 dirs = 20 nodes
//
// The shape is a good compromise between:
//   - Keeping memory-store tests fast (no megabytes of metadata)
//   - Exercising multi-share routing in Badger/Postgres
//   - Producing enough PayloadIDs to stress the PayloadIDSet assertion
func populateForBackup(t *testing.T, store BackupTestStore) backupTestLayout {
	t.Helper()
	ctx := t.Context()

	layout := backupTestLayout{
		shareNames:         []string{"/backup-a", "/backup-b"},
		files:              make(map[string]map[string]string),
		expectedPayloadIDs: metadata.NewPayloadIDSet(),
		fileHandles:        make(map[string]metadata.FileHandle),
	}

	for _, shareName := range layout.shareNames {
		rootHandle := createTestShare(t, store, shareName)
		layout.files[shareName] = make(map[string]string)

		for d := 0; d < 3; d++ {
			dirName := fmt.Sprintf("dir-%d", d)
			dirHandle := createTestDir(t, store, shareName, rootHandle, dirName)

			for f := 0; f < 2; f++ {
				fileName := fmt.Sprintf("file-%d", f)
				fileHandle := createTestFile(t, store, shareName, dirHandle, fileName, 0644)

				// Derive a PayloadID unique to this file and write it onto
				// the FileAttr so Backup's walker can observe it. Exact form:
				// "payload-<share-last-segment>-<dir>-<file>". The share name
				// is "/backup-a", so trimming the slash yields "backup-a".
				shareSuffix := shareName[1:]
				payloadID := fmt.Sprintf("payload-%s-%s-%s", shareSuffix, dirName, fileName)

				// Refresh the file via GetFile so we have the canonical record
				// (PutFile in createTestFile did not set PayloadID); then Put
				// back with PayloadID populated.
				file, err := store.GetFile(ctx, fileHandle)
				if err != nil {
					t.Fatalf("GetFile(%s/%s/%s) failed: %v", shareName, dirName, fileName, err)
				}
				file.PayloadID = metadata.PayloadID(payloadID)
				if err := store.PutFile(ctx, file); err != nil {
					t.Fatalf("PutFile(%s/%s/%s) failed: %v", shareName, dirName, fileName, err)
				}

				logicalPath := fmt.Sprintf("%s/%s/%s", shareName, dirName, fileName)
				layout.files[shareName][logicalPath] = payloadID
				layout.expectedPayloadIDs.Add(payloadID)
				layout.fileHandles[payloadID] = fileHandle
			}
		}
	}

	return layout
}

// enumerateRestoredPayloadIDs walks every share in the destination store and
// returns the set of non-empty PayloadIDs observed across regular files. Used
// by RoundTrip and PayloadIDSet sub-tests to validate the restore target.
//
// Enumeration uses the public MetadataStore surface only — ListShares plus
// recursive ListChildren / GetFile — so it works against any engine without
// peeking at internals.
func enumerateRestoredPayloadIDs(t *testing.T, store BackupTestStore) metadata.PayloadIDSet {
	t.Helper()
	ctx := t.Context()

	observed := metadata.NewPayloadIDSet()

	shares, err := store.ListShares(ctx)
	if err != nil {
		t.Fatalf("ListShares() failed: %v", err)
	}

	for _, shareName := range shares {
		rootHandle, err := store.GetRootHandle(ctx, shareName)
		if err != nil {
			t.Fatalf("GetRootHandle(%q) failed: %v", shareName, err)
		}
		walkCollectPayloadIDs(t, store, rootHandle, observed)
	}
	return observed
}

// walkCollectPayloadIDs does a DFS walk from handle, collecting PayloadIDs for
// every regular file encountered. Uses ListChildren pagination with a 100-entry
// page size (same as dir_ops.go test).
func walkCollectPayloadIDs(t *testing.T, store BackupTestStore, dirHandle metadata.FileHandle, out metadata.PayloadIDSet) {
	t.Helper()
	ctx := t.Context()

	cursor := ""
	for {
		entries, next, err := store.ListChildren(ctx, dirHandle, cursor, 100)
		if err != nil {
			t.Fatalf("ListChildren() failed: %v", err)
		}
		for _, entry := range entries {
			child, err := store.GetFile(ctx, entry.Handle)
			if err != nil {
				t.Fatalf("GetFile(%q) failed: %v", entry.Name, err)
			}
			switch child.Type {
			case metadata.FileTypeRegular:
				if child.PayloadID != "" {
					out.Add(string(child.PayloadID))
				}
			case metadata.FileTypeDirectory:
				walkCollectPayloadIDs(t, store, entry.Handle, out)
			}
		}
		if next == "" {
			break
		}
		cursor = next
	}
}

// ============================================================================
// RoundTrip
// ============================================================================

// testBackupRoundTrip populates a source store, calls Backup, then Restores
// into a fresh destination store and asserts that:
//   - the returned PayloadIDSet equals the layout's expected set
//   - every share from the source is enumerable in the destination
//   - every file (by recursive walk) has a matching PayloadID in the dest
func testBackupRoundTrip(t *testing.T, factory BackupStoreFactory) {
	t.Helper()
	ctx := t.Context()

	src := factory(t)
	t.Cleanup(func() { _ = src.Close() })
	layout := populateForBackup(t, src)

	var buf bytes.Buffer
	ids, err := src.Backup(ctx, &buf)
	if err != nil {
		t.Fatalf("Backup() failed: %v", err)
	}
	if !payloadSetsEqual(ids, layout.expectedPayloadIDs) {
		t.Fatalf("Backup() returned PayloadIDSet %v, want %v", ids, layout.expectedPayloadIDs)
	}

	dest := factory(t)
	t.Cleanup(func() { _ = dest.Close() })
	if err := dest.Restore(ctx, bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("Restore() failed: %v", err)
	}

	gotNames, err := dest.ListShares(ctx)
	if err != nil {
		t.Fatalf("dest.ListShares() failed: %v", err)
	}
	wantNames := append([]string(nil), layout.shareNames...)
	sort.Strings(gotNames)
	sort.Strings(wantNames)
	if !slices.Equal(gotNames, wantNames) {
		t.Fatalf("restored shares = %v, want %v", gotNames, wantNames)
	}

	restoredIDs := enumerateRestoredPayloadIDs(t, dest)
	if !payloadSetsEqual(restoredIDs, layout.expectedPayloadIDs) {
		t.Fatalf("restored PayloadIDs %v, want %v", restoredIDs, layout.expectedPayloadIDs)
	}
}

// ============================================================================
// ConcurrentWriter
// ============================================================================

// testBackupConcurrentWriter launches a goroutine that mutates the source
// store while Backup runs. Asserts Backup completes without error and the
// returned PayloadIDSet is consistent with what ends up enumerable after a
// restore (no dangling refs, no uncounted files).
//
// Engine SSI guarantees:
//   - Memory: RWMutex forces writers to block while Backup holds RLock, so
//     the writer goroutine serialises behind Backup. Still a useful smoke
//     test that Backup does not deadlock.
//   - Badger: db.View snapshot isolation; writers commit at a later read-ts
//     but do not bleed into the backup stream.
//   - Postgres: REPEATABLE READ isolation; writers on a separate connection
//     commit into rows the backup tx cannot see.
func testBackupConcurrentWriter(t *testing.T, factory BackupStoreFactory, opts BackupSuiteOptions) {
	t.Helper()
	ctx := t.Context()

	src := factory(t)
	t.Cleanup(func() { _ = src.Close() })
	layout := populateForBackup(t, src)

	duration := opts.ConcurrentWriterDuration
	if duration <= 0 {
		duration = defaultConcurrentWriterDuration
	}
	writerCtx, cancel := context.WithTimeout(ctx, duration)
	defer cancel()

	// Use a root handle from the first share as the target for concurrent
	// creations. createTestFile issues GenerateHandle + PutFile + SetChild,
	// which is a multi-op burst that exercises engine locking well.
	writerShare := layout.shareNames[0]
	rootHandle, err := src.GetRootHandle(writerCtx, writerShare)
	if err != nil {
		t.Fatalf("GetRootHandle(%q) failed: %v", writerShare, err)
	}

	var wg sync.WaitGroup
	var writerErrs atomic.Int64
	wg.Add(1)
	go func() {
		defer wg.Done()
		i := 0
		for {
			if err := writerCtx.Err(); err != nil {
				return
			}
			name := fmt.Sprintf("concurrent-%d", i)
			// Inline the PutFile dance instead of calling createTestFile:
			// createTestFile invokes t.Fatal on any error, which is unsafe from
			// a goroutine. Errors here are counted via atomic and checked by
			// the main goroutine after the Backup completes.
			handle, err := src.GenerateHandle(writerCtx, writerShare, "/"+name)
			if err != nil {
				writerErrs.Add(1)
				i++
				continue
			}
			_, id, err := metadata.DecodeFileHandle(handle)
			if err != nil {
				writerErrs.Add(1)
				i++
				continue
			}
			file := &metadata.File{
				ID:        id,
				ShareName: writerShare,
				FileAttr: metadata.FileAttr{
					Type:      metadata.FileTypeRegular,
					Mode:      0644,
					UID:       1000,
					GID:       1000,
					PayloadID: metadata.PayloadID(fmt.Sprintf("payload-concurrent-%d", i)),
				},
			}
			if err := src.PutFile(writerCtx, file); err != nil {
				writerErrs.Add(1)
				i++
				continue
			}
			if err := src.SetParent(writerCtx, handle, rootHandle); err != nil {
				writerErrs.Add(1)
				i++
				continue
			}
			if err := src.SetChild(writerCtx, rootHandle, name, handle); err != nil {
				writerErrs.Add(1)
				i++
				continue
			}
			if err := src.SetLinkCount(writerCtx, handle, 1); err != nil {
				writerErrs.Add(1)
				i++
				continue
			}
			i++
		}
	}()

	// Run Backup concurrently with the writer. Backup may observe zero or
	// more concurrent files depending on engine isolation semantics; the
	// assertion is on consistency, not on a specific count.
	var buf bytes.Buffer
	ids, err := src.Backup(ctx, &buf)
	if err != nil {
		cancel()
		wg.Wait()
		t.Fatalf("Backup() during concurrent writes failed: %v", err)
	}

	cancel()
	wg.Wait()

	// Restore into a fresh destination and compare.
	dest := factory(t)
	t.Cleanup(func() { _ = dest.Close() })
	if err := dest.Restore(ctx, bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("Restore() failed after concurrent backup: %v", err)
	}

	// Invariant (a): every PayloadID in ids must be a PayloadID of some file
	// enumerable in the restored store. No dangling refs.
	restoredIDs := enumerateRestoredPayloadIDs(t, dest)
	for pid := range ids {
		if !restoredIDs.Contains(pid) {
			t.Errorf("Backup returned PayloadID %q but restored store has no file with it", pid)
		}
	}
	// Invariant (b): every file in the restored store has its PayloadID in
	// the returned set. No uncounted files (which would let GC free live
	// blocks).
	for pid := range restoredIDs {
		if !ids.Contains(pid) {
			t.Errorf("restored file with PayloadID %q is not in Backup's returned set", pid)
		}
	}
}

// ============================================================================
// Corruption
// ============================================================================

// testBackupCorruption produces a good archive, then rewrites it into three
// corrupt variants and asserts each one rejects Restore with ErrRestoreCorrupt
// and leaves the destination empty.
func testBackupCorruption(t *testing.T, factory BackupStoreFactory) {
	t.Helper()
	ctx := t.Context()

	src := factory(t)
	t.Cleanup(func() { _ = src.Close() })
	_ = populateForBackup(t, src)

	var buf bytes.Buffer
	if _, err := src.Backup(ctx, &buf); err != nil {
		t.Fatalf("Backup() failed: %v", err)
	}
	good := buf.Bytes()
	if len(good) < 4 {
		t.Fatalf("Backup produced a %d-byte archive; corruption variants require at least 4 bytes", len(good))
	}

	variants := []struct {
		name    string
		corrupt []byte
	}{
		{"HeaderTruncated", append([]byte(nil), good[:1]...)},
		{"BodyTruncated", append([]byte(nil), good[:len(good)/2]...)},
		{"SingleByteFlip", flipByte(good, len(good)/2)},
	}

	for _, v := range variants {
		t.Run(v.name, func(t *testing.T) {
			dest := factory(t)
			t.Cleanup(func() { _ = dest.Close() })

			err := dest.Restore(ctx, bytes.NewReader(v.corrupt))
			if err == nil {
				t.Fatalf("Restore(%s) returned nil error; want ErrRestoreCorrupt", v.name)
			}
			if !errors.Is(err, metadata.ErrRestoreCorrupt) {
				t.Fatalf("Restore(%s) returned %v; want errors.Is(err, metadata.ErrRestoreCorrupt)", v.name, err)
			}

			// Destination MUST still be empty after a rejected corrupt
			// restore: a subsequent Restore with the good archive must
			// succeed, proving no tombstones were left behind.
			if err := dest.Restore(ctx, bytes.NewReader(good)); err != nil {
				t.Fatalf("Restore(good) after rejected %s failed: %v (destination must be left empty)",
					v.name, err)
			}
		})
	}
}

// flipByte returns a copy of src with the byte at idx XOR'd with 0xFF.
func flipByte(src []byte, idx int) []byte {
	cp := append([]byte(nil), src...)
	cp[idx] ^= 0xFF
	return cp
}

// ============================================================================
// NonEmptyDest
// ============================================================================

// testBackupNonEmptyDest verifies that Restore rejects a populated destination
// with ErrRestoreDestinationNotEmpty AND that the pre-existing data survives
// the rejected attempt.
func testBackupNonEmptyDest(t *testing.T, factory BackupStoreFactory) {
	t.Helper()
	ctx := t.Context()

	// Source: populate + Backup → produces a valid archive.
	src := factory(t)
	t.Cleanup(func() { _ = src.Close() })
	_ = populateForBackup(t, src)

	var buf bytes.Buffer
	if _, err := src.Backup(ctx, &buf); err != nil {
		t.Fatalf("src.Backup() failed: %v", err)
	}

	// Destination: pre-populate with a single file, then attempt Restore.
	dest := factory(t)
	t.Cleanup(func() { _ = dest.Close() })

	destRoot := createTestShare(t, dest, "/existing")
	preFileHandle := createTestFile(t, dest, "/existing", destRoot, "sentinel.txt", 0644)

	err := dest.Restore(ctx, bytes.NewReader(buf.Bytes()))
	if err == nil {
		t.Fatalf("Restore() into non-empty destination returned nil error; want ErrRestoreDestinationNotEmpty")
	}
	if !errors.Is(err, metadata.ErrRestoreDestinationNotEmpty) {
		t.Fatalf("Restore() returned %v; want errors.Is(err, ErrRestoreDestinationNotEmpty)", err)
	}

	// Pre-existing file must still be readable.
	file, err := dest.GetFile(ctx, preFileHandle)
	if err != nil {
		t.Fatalf("pre-existing file unreadable after rejected Restore: %v", err)
	}
	if file.Type != metadata.FileTypeRegular {
		t.Errorf("pre-existing file type corrupted: got %v, want FileTypeRegular", file.Type)
	}
}

// ============================================================================
// PayloadIDSet
// ============================================================================

// testBackupPayloadIDSet asserts set equality between the PayloadIDSet
// returned by Backup and the PayloadIDs enumerable in the restored store.
// This is the safety invariant Phase 5's GC-hold relies on (D-02).
func testBackupPayloadIDSet(t *testing.T, factory BackupStoreFactory) {
	t.Helper()
	ctx := t.Context()

	src := factory(t)
	t.Cleanup(func() { _ = src.Close() })
	layout := populateForBackup(t, src)

	var buf bytes.Buffer
	ids, err := src.Backup(ctx, &buf)
	if err != nil {
		t.Fatalf("Backup() failed: %v", err)
	}
	// Same-snapshot invariant: returned set must equal the set we populated.
	if !payloadSetsEqual(ids, layout.expectedPayloadIDs) {
		t.Fatalf("Backup returned PayloadIDSet %v, want %v (same-snapshot invariant)",
			ids, layout.expectedPayloadIDs)
	}

	dest := factory(t)
	t.Cleanup(func() { _ = dest.Close() })
	if err := dest.Restore(ctx, bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("Restore() failed: %v", err)
	}

	observed := enumerateRestoredPayloadIDs(t, dest)
	if !payloadSetsEqual(observed, ids) {
		t.Fatalf("restored store has PayloadIDSet %v; Backup returned %v (must be equal)",
			observed, ids)
	}
}

// ============================================================================
// Helpers
// ============================================================================

// payloadSetsEqual reports whether a and b contain the same keys.
func payloadSetsEqual(a, b metadata.PayloadIDSet) bool {
	if a.Len() != b.Len() {
		return false
	}
	for k := range a {
		if !b.Contains(k) {
			return false
		}
	}
	return true
}

// ============================================================================
// StoreID Conformance (Phase 5 D-06)
// ============================================================================

// StoreIDFactory opens (or reopens) an engine against the SAME underlying
// backing storage each time it's called. The first call must produce a
// fresh store instance; subsequent calls must reopen the SAME directory /
// schema so the "across restart" clause of the persistence invariant can be
// exercised.
//
// Engines whose identity is ephemeral by construction (Memory) should use
// TestStoreID_NonEmptyOnConstruction directly; the "across restart" shape
// is not meaningful for them.
type StoreIDFactory func(t *testing.T) metadata.MetadataStore

// TestStoreID_PersistedAcrossRestart verifies that each engine's
// GetStoreID() returns a stable, non-empty identifier that survives
// close + reopen of the same backing storage. Engines that return
// different IDs after close+reopen fail the test loudly.
//
// Contract notes:
//   - newStore is expected to produce a store against the SAME
//     underlying path/schema each time it is called (so close+reopen
//     exercises persistence, not fresh-instance creation).
//   - Memory engine is exempt from this clause — for it, callers
//     should use TestStoreID_NonEmptyOnConstruction.
//
// See Phase 5 CONTEXT.md D-06 / Pitfall #4 for the invariant this locks.
func TestStoreID_PersistedAcrossRestart(t *testing.T, newStore StoreIDFactory) {
	t.Helper()

	s1 := newStore(t)
	idr1, ok := s1.(interface{ GetStoreID() string })
	if !ok {
		t.Fatalf("store does not implement GetStoreID(): %T", s1)
	}
	id1 := idr1.GetStoreID()
	if id1 == "" {
		t.Fatalf("GetStoreID() returned empty string on first open")
	}
	if closer, ok := s1.(io.Closer); ok {
		if err := closer.Close(); err != nil {
			t.Fatalf("close first instance: %v", err)
		}
	}

	s2 := newStore(t)
	idr2, ok := s2.(interface{ GetStoreID() string })
	if !ok {
		t.Fatalf("reopened store does not implement GetStoreID(): %T", s2)
	}
	id2 := idr2.GetStoreID()
	if id2 == "" {
		t.Fatalf("GetStoreID() returned empty string on reopen")
	}
	if id1 != id2 {
		t.Fatalf("store_id must persist across restart: first=%q second=%q", id1, id2)
	}
}

// TestStoreID_NonEmptyOnConstruction verifies that GetStoreID returns a
// non-empty identifier on the first open. Applicable to all engines
// including memory (where "across restart" is not a meaningful clause).
func TestStoreID_NonEmptyOnConstruction(t *testing.T, newStore StoreIDFactory) {
	t.Helper()
	s := newStore(t)
	idr, ok := s.(interface{ GetStoreID() string })
	if !ok {
		t.Fatalf("store does not implement GetStoreID(): %T", s)
	}
	if idr.GetStoreID() == "" {
		t.Fatalf("GetStoreID() returned empty string")
	}
}

// TestStoreID_PreservedAcrossRestore verifies Phase 5 D-06's "receiver
// identity wins" invariant: backing up a source store into a destination
// store MUST leave the destination's store_id unchanged (the source's
// store_id in the restore payload does NOT rebrand the receiver).
//
// Both newSource and newDest produce fresh, independent stores. The test
// populates a trivial single-share layout into the source (so Backup has
// something to snapshot), captures the destination's store_id before
// Restore, runs Restore, and asserts the destination's store_id is
// unchanged afterward.
//
// The source's store_id is NOT equal to the destination's store_id — they
// are independent engines with independent ULIDs. The destination engine
// enforces this by re-anchoring its own storeID after the restore payload
// is applied (Badger: cfg:store_id re-write; Postgres: UPDATE
// server_config; Memory: deliberately not serialized).
func TestStoreID_PreservedAcrossRestore(t *testing.T, newSource, newDest BackupStoreFactory) {
	t.Helper()
	ctx := t.Context()

	src := newSource(t)
	t.Cleanup(func() { _ = src.Close() })
	srcIDer, ok := src.(interface{ GetStoreID() string })
	if !ok {
		t.Fatalf("source store does not implement GetStoreID(): %T", src)
	}
	srcID := srcIDer.GetStoreID()
	if srcID == "" {
		t.Fatalf("source GetStoreID() returned empty string")
	}

	// Populate src with a minimal valid tree (one share + one directory +
	// one file with PayloadID) so Backup produces a non-trivial payload.
	_ = populateForBackup(t, src)

	var buf bytes.Buffer
	if _, err := src.Backup(ctx, &buf); err != nil {
		t.Fatalf("src.Backup() failed: %v", err)
	}

	dest := newDest(t)
	t.Cleanup(func() { _ = dest.Close() })
	destIDer, ok := dest.(interface{ GetStoreID() string })
	if !ok {
		t.Fatalf("dest store does not implement GetStoreID(): %T", dest)
	}
	destIDBefore := destIDer.GetStoreID()
	if destIDBefore == "" {
		t.Fatalf("dest GetStoreID() before restore returned empty string")
	}
	if destIDBefore == srcID {
		t.Fatalf("source and destination must have distinct IDs (source=%q dest=%q)", srcID, destIDBefore)
	}

	if err := dest.Restore(ctx, bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("dest.Restore() failed: %v", err)
	}

	destIDAfter := destIDer.GetStoreID()
	if destIDAfter != destIDBefore {
		t.Fatalf("destination store_id must survive Restore: before=%q after=%q (Phase 5 D-06 invariant)",
			destIDBefore, destIDAfter)
	}
	if destIDAfter == srcID {
		t.Fatalf("destination store_id was overwritten by the source's ID (dest=%q src=%q)",
			destIDAfter, srcID)
	}
}

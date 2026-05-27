package storetest

import (
	"bytes"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/backup"
)

// BackupableStoreFactory creates a fresh MetadataStore instance for each
// backup conformance test. The returned store does NOT need to implement
// metadata.Backupable at this type level -- the suite performs the type
// assertion internally and fails if the capability is absent.
type BackupableStoreFactory func(t *testing.T) metadata.MetadataStore

// RunBackupConformanceSuite runs the backup/restore conformance tests against
// the provided factory. Each subtest gets a fresh store instance to ensure
// isolation.
//
// The suite covers five categories:
//   - RoundTrip: backup-then-restore produces identical data
//   - ConcurrentWriter: snapshot isolation during active writes
//   - Corruption: truncated stream, bit-flip, and wrong engine tag detection
//   - NonEmptyDest: ErrRestoreDestinationNotEmpty on populated destination
//   - HashSetCorrectness: exact hash match and dedup verification
//
// The factory MUST return a store that implements metadata.Backupable.
// Unlike the optional-capability pattern used in BlockRefOps (which skips),
// backup conformance uses t.Fatal because the factory explicitly opts in.
func RunBackupConformanceSuite(t *testing.T, factory BackupableStoreFactory) {
	t.Helper()

	// Verify the factory produces a Backupable store before dispatching.
	probe := factory(t)
	if _, ok := probe.(metadata.Backupable); !ok {
		t.Fatal("factory must return a store implementing metadata.Backupable")
	}

	t.Run("RoundTrip", func(t *testing.T) {
		testBackup_RoundTrip(t, factory)
	})

	t.Run("ConcurrentWriter", func(t *testing.T) {
		testBackup_ConcurrentWriter(t, factory)
	})

	t.Run("Corruption", func(t *testing.T) {
		testBackup_Corruption(t, factory)
	})

	t.Run("NonEmptyDest", func(t *testing.T) {
		testBackup_NonEmptyDest(t, factory)
	})

	t.Run("HashSetCorrectness", func(t *testing.T) {
		testBackup_HashSetCorrectness(t, factory)
	})
}

// asBackupable is a helper that type-asserts a MetadataStore to Backupable,
// calling t.Fatal if the assertion fails.
func asBackupable(t *testing.T, store metadata.MetadataStore) metadata.Backupable {
	t.Helper()
	b, ok := store.(metadata.Backupable)
	if !ok {
		t.Fatal("store does not implement metadata.Backupable")
	}
	return b
}

// populateTestData creates a share with two files carrying BlockRef hashes.
// Returns the share name and the list of unique hashes used.
func populateTestData(t *testing.T, store metadata.MetadataStore, sharePrefix string) (string, []blockstore.ContentHash) {
	t.Helper()

	shareName := sharePrefix + "-bkp"
	rootHandle := createTestShare(t, store, shareName)

	ctx := t.Context()

	// File A: two blocks with unique hashes.
	fileA := createTestFile(t, store, shareName, rootHandle, "alpha.bin", 0o644)
	hashA0 := hashOfSeed("bkp-a0")
	hashA1 := hashOfSeed("bkp-a1")
	fA, err := store.GetFile(ctx, fileA)
	if err != nil {
		t.Fatalf("GetFile alpha: %v", err)
	}
	fA.Blocks = []blockstore.BlockRef{
		{Hash: hashA0, Offset: 0, Size: 4 << 20},
		{Hash: hashA1, Offset: 4 << 20, Size: 4 << 20},
	}
	fA.Size = 8 << 20
	if err := store.PutFile(ctx, fA); err != nil {
		t.Fatalf("PutFile alpha: %v", err)
	}

	// File B: one unique hash + one shared with file A (hashA0 for dedup).
	fileB := createTestFile(t, store, shareName, rootHandle, "beta.bin", 0o644)
	hashB0 := hashOfSeed("bkp-b0")
	fB, err := store.GetFile(ctx, fileB)
	if err != nil {
		t.Fatalf("GetFile beta: %v", err)
	}
	fB.Blocks = []blockstore.BlockRef{
		{Hash: hashB0, Offset: 0, Size: 2 << 20},
		{Hash: hashA0, Offset: 2 << 20, Size: 4 << 20}, // shared with alpha
	}
	fB.Size = 6 << 20
	if err := store.PutFile(ctx, fB); err != nil {
		t.Fatalf("PutFile beta: %v", err)
	}

	// Unique hashes: hashA0, hashA1, hashB0 (3 unique despite 4 block refs).
	return shareName, []blockstore.ContentHash{hashA0, hashA1, hashB0}
}

// --------------------------------------------------------------------------
// Subtest 1: RoundTrip
// --------------------------------------------------------------------------

// testBackup_RoundTrip verifies that Backup-then-Restore produces identical
// shares and files in the destination store.
func testBackup_RoundTrip(t *testing.T, factory BackupableStoreFactory) {
	srcStore := factory(t)
	srcB := asBackupable(t, srcStore)

	shareName, uniqueHashes := populateTestData(t, srcStore, "rt")

	ctx := t.Context()

	// Backup to buffer.
	var buf bytes.Buffer
	hashes, err := srcB.Backup(ctx, &buf)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	// Verify HashSet length matches expected unique hashes.
	if hashes.Len() != len(uniqueHashes) {
		t.Fatalf("HashSet.Len() = %d, want %d", hashes.Len(), len(uniqueHashes))
	}

	// Restore into fresh destination.
	dstStore := factory(t)
	dstB := asBackupable(t, dstStore)
	if err := dstB.Restore(ctx, &buf); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	// Verify shares exist in destination.
	shares, err := dstStore.ListShares(ctx)
	if err != nil {
		t.Fatalf("ListShares: %v", err)
	}
	found := false
	for _, s := range shares {
		if s == shareName {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("share %q not found in destination (shares: %v)", shareName, shares)
	}

	// Verify root directory exists and resolve children by name.
	rootHandle, err := dstStore.GetRootHandle(ctx, shareName)
	if err != nil {
		t.Fatalf("GetRootHandle(%q): %v", shareName, err)
	}

	// Verify file alpha.
	alphaHandle, err := dstStore.GetChild(ctx, rootHandle, "alpha.bin")
	if err != nil {
		t.Fatalf("GetChild alpha.bin: %v", err)
	}
	alphaFile, err := dstStore.GetFile(ctx, alphaHandle)
	if err != nil {
		t.Fatalf("GetFile alpha: %v", err)
	}
	if alphaFile.Size != 8<<20 {
		t.Errorf("alpha.Size = %d, want %d", alphaFile.Size, 8<<20)
	}
	if alphaFile.Mode != 0o644 {
		t.Errorf("alpha.Mode = %o, want %o", alphaFile.Mode, 0o644)
	}
	if len(alphaFile.Blocks) != 2 {
		t.Fatalf("alpha.Blocks len = %d, want 2", len(alphaFile.Blocks))
	}

	// Verify file beta.
	betaHandle, err := dstStore.GetChild(ctx, rootHandle, "beta.bin")
	if err != nil {
		t.Fatalf("GetChild beta.bin: %v", err)
	}
	betaFile, err := dstStore.GetFile(ctx, betaHandle)
	if err != nil {
		t.Fatalf("GetFile beta: %v", err)
	}
	if betaFile.Size != 6<<20 {
		t.Errorf("beta.Size = %d, want %d", betaFile.Size, 6<<20)
	}
	if len(betaFile.Blocks) != 2 {
		t.Fatalf("beta.Blocks len = %d, want 2", len(betaFile.Blocks))
	}
}

// --------------------------------------------------------------------------
// Subtest 2: ConcurrentWriter
// --------------------------------------------------------------------------

// testBackup_ConcurrentWriter verifies that a backup snapshot is isolated
// from concurrent writes: files created after the backup begins must NOT
// appear in the restored state.
func testBackup_ConcurrentWriter(t *testing.T, factory BackupableStoreFactory) {
	store := factory(t)
	b := asBackupable(t, store)

	shareName, _ := populateTestData(t, store, "cw")

	ctx := t.Context()

	// Use a buffer so backup writes stream while we can concurrently
	// add files to the source store. The backup implementation must
	// snapshot state at call time.
	var buf bytes.Buffer
	var backupErr error
	var hashes *blockstore.HashSet

	var wg sync.WaitGroup

	// Start backup in goroutine.
	wg.Add(1)
	go func() {
		defer wg.Done()
		hashes, backupErr = b.Backup(ctx, &buf)
	}()

	// Concurrently write a new file (with a unique block hash) while
	// backup is running. NOTE: cannot use createTestFile here — it calls
	// t.Fatalf, which is forbidden from non-test goroutines.
	concurrentHash := hashOfSeed("cw-concurrent")
	var concurrentErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		rootHandle, err := store.GetRootHandle(ctx, shareName)
		if err != nil {
			concurrentErr = err
			return
		}
		h, err := store.GenerateHandle(ctx, shareName, "/concurrent-new.bin")
		if err != nil {
			concurrentErr = err
			return
		}
		_, id, err := metadata.DecodeFileHandle(h)
		if err != nil {
			concurrentErr = err
			return
		}
		f := &metadata.File{
			ShareName: shareName,
			FileAttr: metadata.FileAttr{
				Type: metadata.FileTypeRegular,
				Mode: 0o644,
				UID:  1000,
				GID:  1000,
				Size: 1 << 20,
				Blocks: []blockstore.BlockRef{
					{Hash: concurrentHash, Offset: 0, Size: 1 << 20},
				},
			},
		}
		f.ID = id
		if err := store.PutFile(ctx, f); err != nil {
			concurrentErr = err
			return
		}
		if err := store.SetParent(ctx, h, rootHandle); err != nil {
			concurrentErr = err
			return
		}
		concurrentErr = store.SetChild(ctx, rootHandle, "concurrent-new.bin", h)
	}()

	wg.Wait()

	if backupErr != nil {
		t.Fatalf("Backup: %v", backupErr)
	}
	if concurrentErr != nil {
		t.Logf("concurrent writer error (non-fatal): %v", concurrentErr)
	}

	// Restore into fresh store.
	dstStore := factory(t)
	dstB := asBackupable(t, dstStore)
	if err := dstB.Restore(ctx, &buf); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	// Verify: the two initial files must be present in the restored backup.
	rootHandle, err := dstStore.GetRootHandle(ctx, shareName)
	if err != nil {
		t.Fatalf("GetRootHandle: %v", err)
	}

	if _, err := dstStore.GetChild(ctx, rootHandle, "alpha.bin"); err != nil {
		t.Error("alpha.bin missing from restored backup")
	}
	if _, err := dstStore.GetChild(ctx, rootHandle, "beta.bin"); err != nil {
		t.Error("beta.bin missing from restored backup")
	}

	// The concurrent file must NOT appear in the snapshot.
	if _, err := dstStore.GetChild(ctx, rootHandle, "concurrent-new.bin"); err == nil {
		t.Error("concurrent-new.bin found in restored backup — snapshot isolation violated")
	}

	// Hash set must contain only initial hashes, not the concurrent one.
	if hashes == nil {
		t.Fatal("Backup returned nil HashSet")
	}
	if hashes.Contains(concurrentHash) {
		t.Error("HashSet contains concurrent hash — snapshot isolation violated")
	}
	if hashes.Len() != 3 {
		t.Errorf("HashSet.Len() = %d, want 3 (initial files only)", hashes.Len())
	}
}

// --------------------------------------------------------------------------
// Subtest 3: Corruption
// --------------------------------------------------------------------------

// testBackup_Corruption verifies that Restore detects corrupted backup
// streams. Three scenarios: truncated, bit-flip, and wrong engine tag.
func testBackup_Corruption(t *testing.T, factory BackupableStoreFactory) {
	// Create a valid backup first.
	srcStore := factory(t)
	srcB := asBackupable(t, srcStore)
	populateTestData(t, srcStore, "corr")

	ctx := t.Context()

	var validBuf bytes.Buffer
	if _, err := srcB.Backup(ctx, &validBuf); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	validData := validBuf.Bytes()

	t.Run("Truncated", func(t *testing.T) {
		// Truncate to half the stream length.
		truncated := validData[:len(validData)/2]

		dstStore := factory(t)
		dstB := asBackupable(t, dstStore)
		err := dstB.Restore(ctx, bytes.NewReader(truncated))
		if err == nil {
			t.Fatal("Restore on truncated stream should have returned an error")
		}
		if !errors.Is(err, metadata.ErrRestoreCorrupt) {
			t.Fatalf("expected ErrRestoreCorrupt, got: %v", err)
		}
	})

	t.Run("BitFlip", func(t *testing.T) {
		// Flip a bit in the middle of the payload region (after the
		// envelope header, before the trailing CRC).
		corrupted := make([]byte, len(validData))
		copy(corrupted, validData)

		// The header is at least 10 bytes (magic=4 + version=4 + engine_len=2),
		// so we target the middle of the stream.
		midpoint := len(corrupted) / 2
		corrupted[midpoint] ^= 0xFF

		dstStore := factory(t)
		dstB := asBackupable(t, dstStore)
		err := dstB.Restore(ctx, bytes.NewReader(corrupted))
		if err == nil {
			t.Fatal("Restore on bit-flipped stream should have returned an error")
		}
		if !errors.Is(err, metadata.ErrRestoreCorrupt) {
			t.Fatalf("expected ErrRestoreCorrupt, got: %v", err)
		}
	})

	t.Run("WrongEngineTag", func(t *testing.T) {
		// Build a valid envelope with a different engine tag, then
		// write the same payload bytes.
		var fakeEnvelopeBuf bytes.Buffer
		fakeWriter, err := backup.NewWriter(&fakeEnvelopeBuf, "wrong-engine-tag")
		if err != nil {
			t.Fatalf("NewWriter: %v", err)
		}

		// Extract the payload from the valid stream by reading past its header.
		validCopy := make([]byte, len(validData))
		copy(validCopy, validData)
		_, payloadReader, _, err := backup.ReadHeader(bytes.NewReader(validCopy))
		if err != nil {
			t.Fatalf("ReadHeader: %v", err)
		}

		// Read the payload (everything except the trailing 4-byte CRC).
		// The tee reader passes through everything including the CRC.
		payloadBytes, err := io.ReadAll(payloadReader)
		if err != nil {
			t.Fatalf("ReadAll payload: %v", err)
		}
		// Strip trailing 4-byte CRC from the payload since
		// the tee reader passes through everything including the CRC.
		if len(payloadBytes) >= 4 {
			payloadBytes = payloadBytes[:len(payloadBytes)-4]
		}

		if _, err := fakeWriter.Write(payloadBytes); err != nil {
			t.Fatalf("Write payload: %v", err)
		}
		if err := fakeWriter.Finish(); err != nil {
			t.Fatalf("Finish: %v", err)
		}

		dstStore := factory(t)
		dstB := asBackupable(t, dstStore)
		err = dstB.Restore(ctx, &fakeEnvelopeBuf)
		if err == nil {
			t.Fatal("Restore with wrong engine tag should have returned an error")
		}
		// Drivers may return ErrSchemaVersionMismatch or ErrRestoreCorrupt
		// for engine tag mismatches -- both are acceptable.
		if !errors.Is(err, metadata.ErrSchemaVersionMismatch) && !errors.Is(err, metadata.ErrRestoreCorrupt) {
			t.Fatalf("expected ErrSchemaVersionMismatch or ErrRestoreCorrupt, got: %v", err)
		}
	})
}

// --------------------------------------------------------------------------
// Subtest 4: NonEmptyDest
// --------------------------------------------------------------------------

// testBackup_NonEmptyDest verifies that Restore rejects a destination store
// that already contains data.
func testBackup_NonEmptyDest(t *testing.T, factory BackupableStoreFactory) {
	// Create a valid backup.
	srcStore := factory(t)
	srcB := asBackupable(t, srcStore)
	populateTestData(t, srcStore, "ned-src")

	ctx := t.Context()

	var buf bytes.Buffer
	if _, err := srcB.Backup(ctx, &buf); err != nil {
		t.Fatalf("Backup: %v", err)
	}

	// Create a destination store that is NOT empty.
	dstStore := factory(t)
	populateTestData(t, dstStore, "ned-dst")
	dstB := asBackupable(t, dstStore)

	err := dstB.Restore(ctx, &buf)
	if err == nil {
		t.Fatal("Restore into non-empty store should have returned an error")
	}
	if !errors.Is(err, metadata.ErrRestoreDestinationNotEmpty) {
		t.Fatalf("expected ErrRestoreDestinationNotEmpty, got: %v", err)
	}
}

// --------------------------------------------------------------------------
// Subtest 5: HashSetCorrectness
// --------------------------------------------------------------------------

// testBackup_HashSetCorrectness verifies that the HashSet returned by Backup
// contains exactly the unique block hashes referenced by all files.
func testBackup_HashSetCorrectness(t *testing.T, factory BackupableStoreFactory) {
	t.Run("ExactMatch", func(t *testing.T) {
		testBackup_HashSet_ExactMatch(t, factory)
	})

	t.Run("Dedup", func(t *testing.T) {
		testBackup_HashSet_Dedup(t, factory)
	})
}

// testBackup_HashSet_ExactMatch creates files with K unique hashes across N
// files and verifies the HashSet matches a manually-collected reference set.
func testBackup_HashSet_ExactMatch(t *testing.T, factory BackupableStoreFactory) {
	store := factory(t)
	b := asBackupable(t, store)
	ctx := t.Context()

	shareName := "hsem-bkp"
	rootHandle := createTestShare(t, store, shareName)

	// Create 3 files with a total of 5 unique hashes.
	hashes := []blockstore.ContentHash{
		hashOfSeed("hs-exact-0"),
		hashOfSeed("hs-exact-1"),
		hashOfSeed("hs-exact-2"),
		hashOfSeed("hs-exact-3"),
		hashOfSeed("hs-exact-4"),
	}

	// File 1: hashes[0], hashes[1]
	f1Handle := createTestFile(t, store, shareName, rootHandle, "f1.bin", 0o644)
	f1, err := store.GetFile(ctx, f1Handle)
	if err != nil {
		t.Fatalf("GetFile f1: %v", err)
	}
	f1.Blocks = []blockstore.BlockRef{
		{Hash: hashes[0], Offset: 0, Size: 1 << 20},
		{Hash: hashes[1], Offset: 1 << 20, Size: 1 << 20},
	}
	if err := store.PutFile(ctx, f1); err != nil {
		t.Fatalf("PutFile f1: %v", err)
	}

	// File 2: hashes[2], hashes[3]
	f2Handle := createTestFile(t, store, shareName, rootHandle, "f2.bin", 0o644)
	f2, err := store.GetFile(ctx, f2Handle)
	if err != nil {
		t.Fatalf("GetFile f2: %v", err)
	}
	f2.Blocks = []blockstore.BlockRef{
		{Hash: hashes[2], Offset: 0, Size: 1 << 20},
		{Hash: hashes[3], Offset: 1 << 20, Size: 1 << 20},
	}
	if err := store.PutFile(ctx, f2); err != nil {
		t.Fatalf("PutFile f2: %v", err)
	}

	// File 3: hashes[4]
	f3Handle := createTestFile(t, store, shareName, rootHandle, "f3.bin", 0o644)
	f3, err := store.GetFile(ctx, f3Handle)
	if err != nil {
		t.Fatalf("GetFile f3: %v", err)
	}
	f3.Blocks = []blockstore.BlockRef{
		{Hash: hashes[4], Offset: 0, Size: 1 << 20},
	}
	if err := store.PutFile(ctx, f3); err != nil {
		t.Fatalf("PutFile f3: %v", err)
	}

	// Backup and collect HashSet.
	var buf bytes.Buffer
	gotHS, err := b.Backup(ctx, &buf)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	// Verify length.
	if gotHS.Len() != len(hashes) {
		t.Fatalf("HashSet.Len() = %d, want %d", gotHS.Len(), len(hashes))
	}

	// Verify every expected hash is present.
	for i, h := range hashes {
		if !gotHS.Contains(h) {
			t.Errorf("HashSet missing hash[%d] = %x", i, h[:8])
		}
	}
}

// testBackup_HashSet_Dedup creates two files that share the same BlockRef
// hash and verifies the HashSet correctly deduplicates.
func testBackup_HashSet_Dedup(t *testing.T, factory BackupableStoreFactory) {
	store := factory(t)
	b := asBackupable(t, store)
	ctx := t.Context()

	shareName := "hsdd-bkp"
	rootHandle := createTestShare(t, store, shareName)

	sharedHash := hashOfSeed("hs-dedup-shared")

	// File A: single block with the shared hash.
	fAHandle := createTestFile(t, store, shareName, rootHandle, "dup-a.bin", 0o644)
	fA, err := store.GetFile(ctx, fAHandle)
	if err != nil {
		t.Fatalf("GetFile dup-a: %v", err)
	}
	fA.Blocks = []blockstore.BlockRef{
		{Hash: sharedHash, Offset: 0, Size: 4 << 20},
	}
	if err := store.PutFile(ctx, fA); err != nil {
		t.Fatalf("PutFile dup-a: %v", err)
	}

	// File B: single block with the SAME shared hash.
	fBHandle := createTestFile(t, store, shareName, rootHandle, "dup-b.bin", 0o644)
	fB, err := store.GetFile(ctx, fBHandle)
	if err != nil {
		t.Fatalf("GetFile dup-b: %v", err)
	}
	fB.Blocks = []blockstore.BlockRef{
		{Hash: sharedHash, Offset: 0, Size: 4 << 20},
	}
	if err := store.PutFile(ctx, fB); err != nil {
		t.Fatalf("PutFile dup-b: %v", err)
	}

	// Backup and collect HashSet.
	var buf bytes.Buffer
	gotHS, err := b.Backup(ctx, &buf)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	// Must be 1 unique hash, not 2.
	if gotHS.Len() != 1 {
		t.Fatalf("HashSet.Len() = %d, want 1 (dedup of shared hash)", gotHS.Len())
	}
	if !gotHS.Contains(sharedHash) {
		t.Fatalf("HashSet does not contain the shared hash %x", sharedHash[:8])
	}
}

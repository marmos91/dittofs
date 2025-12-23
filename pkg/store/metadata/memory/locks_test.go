package memory

import (
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/store/metadata"
)

// createTestStore creates a memory store with a test file for locking tests.
func createTestStore(t *testing.T) (*MemoryMetadataStore, metadata.FileHandle) {
	t.Helper()

	store := NewMemoryMetadataStoreWithDefaults()
	ctx := context.Background()

	// Create root directory
	rootFile, err := store.CreateRootDirectory(ctx, "/test", &metadata.FileAttr{
		Type: metadata.FileTypeDirectory,
		Mode: 0755,
		UID:  1000,
		GID:  1000,
	})
	if err != nil {
		t.Fatalf("failed to create root directory: %v", err)
	}

	rootHandle, err := metadata.EncodeFileHandle(rootFile)
	if err != nil {
		t.Fatalf("failed to encode root handle: %v", err)
	}

	// Create a test file
	authCtx := createAuthContext(ctx)

	file, err := store.Create(authCtx, rootHandle, "testfile.txt", &metadata.FileAttr{
		Type: metadata.FileTypeRegular,
		Mode: 0644,
		UID:  1000,
		GID:  1000,
	})
	if err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}

	fileHandle, err := metadata.EncodeFileHandle(file)
	if err != nil {
		t.Fatalf("failed to encode file handle: %v", err)
	}

	return store, fileHandle
}

func createAuthContext(ctx context.Context) *metadata.AuthContext {
	uid := uint32(1000)
	gid := uint32(1000)
	return &metadata.AuthContext{
		Context: ctx,
		Identity: &metadata.Identity{
			UID: &uid,
			GID: &gid,
		},
	}
}

func TestMemoryStore_LockFile_Basic(t *testing.T) {
	store, fileHandle := createTestStore(t)
	ctx := context.Background()
	authCtx := createAuthContext(ctx)

	// Acquire an exclusive lock
	lock := metadata.FileLock{
		SessionID: 1,
		Offset:    0,
		Length:    100,
		Exclusive: true,
	}

	err := store.LockFile(authCtx, fileHandle, lock)
	if err != nil {
		t.Fatalf("failed to acquire lock: %v", err)
	}

	// Verify lock is listed
	locks, err := store.ListLocks(ctx, fileHandle)
	if err != nil {
		t.Fatalf("failed to list locks: %v", err)
	}

	if len(locks) != 1 {
		t.Fatalf("expected 1 lock, got %d", len(locks))
	}

	if locks[0].SessionID != 1 || locks[0].Offset != 0 || locks[0].Length != 100 || !locks[0].Exclusive {
		t.Errorf("lock properties mismatch: %+v", locks[0])
	}
}

func TestMemoryStore_LockFile_ConflictDetection(t *testing.T) {
	store, fileHandle := createTestStore(t)
	ctx := context.Background()
	authCtx := createAuthContext(ctx)

	// Session 1 acquires exclusive lock
	lock1 := metadata.FileLock{
		SessionID: 1,
		Offset:    0,
		Length:    100,
		Exclusive: true,
	}

	err := store.LockFile(authCtx, fileHandle, lock1)
	if err != nil {
		t.Fatalf("session 1 failed to acquire lock: %v", err)
	}

	// Session 2 tries to acquire overlapping lock - should fail
	lock2 := metadata.FileLock{
		SessionID: 2,
		Offset:    50,
		Length:    100,
		Exclusive: false, // Even shared lock should conflict
	}

	err = store.LockFile(authCtx, fileHandle, lock2)
	if err == nil {
		t.Fatal("expected lock conflict error, got nil")
	}

	storeErr, ok := err.(*metadata.StoreError)
	if !ok {
		t.Fatalf("expected StoreError, got %T", err)
	}

	if storeErr.Code != metadata.ErrLocked {
		t.Errorf("expected ErrLocked, got %v", storeErr.Code)
	}
}

func TestMemoryStore_LockFile_SharedLocksCompatible(t *testing.T) {
	store, fileHandle := createTestStore(t)
	ctx := context.Background()
	authCtx := createAuthContext(ctx)

	// Session 1 acquires shared lock
	lock1 := metadata.FileLock{
		SessionID: 1,
		Offset:    0,
		Length:    100,
		Exclusive: false,
	}

	err := store.LockFile(authCtx, fileHandle, lock1)
	if err != nil {
		t.Fatalf("session 1 failed to acquire shared lock: %v", err)
	}

	// Session 2 should also be able to acquire shared lock on same range
	lock2 := metadata.FileLock{
		SessionID: 2,
		Offset:    0,
		Length:    100,
		Exclusive: false,
	}

	err = store.LockFile(authCtx, fileHandle, lock2)
	if err != nil {
		t.Fatalf("session 2 failed to acquire shared lock: %v", err)
	}

	// Verify both locks exist
	locks, err := store.ListLocks(ctx, fileHandle)
	if err != nil {
		t.Fatalf("failed to list locks: %v", err)
	}

	if len(locks) != 2 {
		t.Fatalf("expected 2 locks, got %d", len(locks))
	}
}

func TestMemoryStore_LockFile_SameSessionNoConflict(t *testing.T) {
	store, fileHandle := createTestStore(t)
	ctx := context.Background()
	authCtx := createAuthContext(ctx)

	// Session 1 acquires exclusive lock
	lock1 := metadata.FileLock{
		SessionID: 1,
		Offset:    0,
		Length:    100,
		Exclusive: true,
	}

	err := store.LockFile(authCtx, fileHandle, lock1)
	if err != nil {
		t.Fatalf("failed to acquire first lock: %v", err)
	}

	// Same session should be able to acquire another lock on overlapping range
	lock2 := metadata.FileLock{
		SessionID: 1,
		Offset:    50,
		Length:    100,
		Exclusive: true,
	}

	err = store.LockFile(authCtx, fileHandle, lock2)
	if err != nil {
		t.Fatalf("same session should not conflict with itself: %v", err)
	}
}

func TestMemoryStore_UnlockFile(t *testing.T) {
	store, fileHandle := createTestStore(t)
	ctx := context.Background()
	authCtx := createAuthContext(ctx)

	// Acquire lock
	lock := metadata.FileLock{
		SessionID: 1,
		Offset:    0,
		Length:    100,
		Exclusive: true,
	}

	err := store.LockFile(authCtx, fileHandle, lock)
	if err != nil {
		t.Fatalf("failed to acquire lock: %v", err)
	}

	// Unlock
	err = store.UnlockFile(ctx, fileHandle, 1, 0, 100)
	if err != nil {
		t.Fatalf("failed to unlock: %v", err)
	}

	// Verify no locks remain
	locks, err := store.ListLocks(ctx, fileHandle)
	if err != nil {
		t.Fatalf("failed to list locks: %v", err)
	}

	if len(locks) != 0 {
		t.Errorf("expected 0 locks after unlock, got %d", len(locks))
	}
}

func TestMemoryStore_UnlockFile_NotFound(t *testing.T) {
	store, fileHandle := createTestStore(t)
	ctx := context.Background()

	// Try to unlock a lock that doesn't exist
	err := store.UnlockFile(ctx, fileHandle, 1, 0, 100)
	if err == nil {
		t.Fatal("expected error when unlocking non-existent lock")
	}

	storeErr, ok := err.(*metadata.StoreError)
	if !ok {
		t.Fatalf("expected StoreError, got %T", err)
	}

	if storeErr.Code != metadata.ErrLockNotFound {
		t.Errorf("expected ErrLockNotFound, got %v", storeErr.Code)
	}
}

func TestMemoryStore_UnlockAllForSession(t *testing.T) {
	store, fileHandle := createTestStore(t)
	ctx := context.Background()
	authCtx := createAuthContext(ctx)

	// Session 1 acquires multiple locks
	for i := uint64(0); i < 3; i++ {
		lock := metadata.FileLock{
			SessionID: 1,
			Offset:    i * 100,
			Length:    100,
			Exclusive: true,
		}
		err := store.LockFile(authCtx, fileHandle, lock)
		if err != nil {
			t.Fatalf("failed to acquire lock %d: %v", i, err)
		}
	}

	// Session 2 acquires a lock
	lock2 := metadata.FileLock{
		SessionID: 2,
		Offset:    1000,
		Length:    100,
		Exclusive: true,
	}
	err := store.LockFile(authCtx, fileHandle, lock2)
	if err != nil {
		t.Fatalf("session 2 failed to acquire lock: %v", err)
	}

	// Verify 4 locks total
	locks, _ := store.ListLocks(ctx, fileHandle)
	if len(locks) != 4 {
		t.Fatalf("expected 4 locks, got %d", len(locks))
	}

	// Unlock all for session 1
	err = store.UnlockAllForSession(ctx, fileHandle, 1)
	if err != nil {
		t.Fatalf("failed to unlock all for session: %v", err)
	}

	// Verify only session 2's lock remains
	locks, _ = store.ListLocks(ctx, fileHandle)
	if len(locks) != 1 {
		t.Fatalf("expected 1 lock after UnlockAllForSession, got %d", len(locks))
	}

	if locks[0].SessionID != 2 {
		t.Errorf("remaining lock should be session 2, got session %d", locks[0].SessionID)
	}
}

func TestMemoryStore_TestLock(t *testing.T) {
	store, fileHandle := createTestStore(t)
	ctx := context.Background()
	authCtx := createAuthContext(ctx)

	// Initially, lock should succeed
	ok, conflict, err := store.TestLock(ctx, fileHandle, 2, 0, 100, true)
	if err != nil {
		t.Fatalf("TestLock failed: %v", err)
	}
	if !ok {
		t.Error("expected TestLock to return true when no locks exist")
	}
	if conflict != nil {
		t.Error("expected no conflict")
	}

	// Session 1 acquires exclusive lock
	lock := metadata.FileLock{
		SessionID: 1,
		Offset:    0,
		Length:    100,
		Exclusive: true,
	}
	err = store.LockFile(authCtx, fileHandle, lock)
	if err != nil {
		t.Fatalf("failed to acquire lock: %v", err)
	}

	// Session 2 tests for lock - should fail
	ok, conflict, err = store.TestLock(ctx, fileHandle, 2, 0, 100, true)
	if err != nil {
		t.Fatalf("TestLock failed: %v", err)
	}
	if ok {
		t.Error("expected TestLock to return false when conflicting lock exists")
	}
	if conflict == nil {
		t.Error("expected conflict info")
	} else if conflict.OwnerSessionID != 1 {
		t.Errorf("expected conflict owner to be session 1, got %d", conflict.OwnerSessionID)
	}

	// Same session tests - should succeed (own locks don't conflict)
	ok, _, err = store.TestLock(ctx, fileHandle, 1, 0, 100, true)
	if err != nil {
		t.Fatalf("TestLock failed: %v", err)
	}
	if !ok {
		t.Error("expected TestLock to return true for same session")
	}
}

func TestMemoryStore_CheckLockForIO(t *testing.T) {
	store, fileHandle := createTestStore(t)
	ctx := context.Background()
	authCtx := createAuthContext(ctx)

	// Session 1 acquires exclusive lock
	lock := metadata.FileLock{
		SessionID: 1,
		Offset:    0,
		Length:    100,
		Exclusive: true,
	}
	err := store.LockFile(authCtx, fileHandle, lock)
	if err != nil {
		t.Fatalf("failed to acquire lock: %v", err)
	}

	// Session 1 can read/write through its own lock
	err = store.CheckLockForIO(ctx, fileHandle, 1, 0, 50, false)
	if err != nil {
		t.Errorf("session 1 should be able to read its own locked range: %v", err)
	}

	err = store.CheckLockForIO(ctx, fileHandle, 1, 0, 50, true)
	if err != nil {
		t.Errorf("session 1 should be able to write its own locked range: %v", err)
	}

	// Session 2 cannot read/write through session 1's exclusive lock
	err = store.CheckLockForIO(ctx, fileHandle, 2, 0, 50, false)
	if err == nil {
		t.Error("session 2 should not be able to read through exclusive lock")
	}

	err = store.CheckLockForIO(ctx, fileHandle, 2, 0, 50, true)
	if err == nil {
		t.Error("session 2 should not be able to write through exclusive lock")
	}

	// Session 2 can access non-locked range
	err = store.CheckLockForIO(ctx, fileHandle, 2, 200, 50, true)
	if err != nil {
		t.Errorf("session 2 should be able to access non-locked range: %v", err)
	}
}

func TestMemoryStore_CheckLockForIO_SharedLocks(t *testing.T) {
	store, fileHandle := createTestStore(t)
	ctx := context.Background()
	authCtx := createAuthContext(ctx)

	// Session 1 acquires shared lock
	lock := metadata.FileLock{
		SessionID: 1,
		Offset:    0,
		Length:    100,
		Exclusive: false,
	}
	err := store.LockFile(authCtx, fileHandle, lock)
	if err != nil {
		t.Fatalf("failed to acquire shared lock: %v", err)
	}

	// Session 2 can read through shared lock
	err = store.CheckLockForIO(ctx, fileHandle, 2, 0, 50, false)
	if err != nil {
		t.Errorf("session 2 should be able to read through shared lock: %v", err)
	}

	// Session 2 cannot write through shared lock
	err = store.CheckLockForIO(ctx, fileHandle, 2, 0, 50, true)
	if err == nil {
		t.Error("session 2 should not be able to write through shared lock")
	}
}

func TestMemoryStore_LockUpgrade(t *testing.T) {
	store, fileHandle := createTestStore(t)
	ctx := context.Background()
	authCtx := createAuthContext(ctx)

	// Acquire shared lock
	lock := metadata.FileLock{
		SessionID: 1,
		Offset:    0,
		Length:    100,
		Exclusive: false,
	}
	err := store.LockFile(authCtx, fileHandle, lock)
	if err != nil {
		t.Fatalf("failed to acquire shared lock: %v", err)
	}

	// Verify it's shared
	locks, _ := store.ListLocks(ctx, fileHandle)
	if len(locks) != 1 || locks[0].Exclusive {
		t.Error("expected one shared lock")
	}

	// Upgrade to exclusive
	lock.Exclusive = true
	err = store.LockFile(authCtx, fileHandle, lock)
	if err != nil {
		t.Fatalf("failed to upgrade lock: %v", err)
	}

	// Verify it's now exclusive and still only one lock
	locks, _ = store.ListLocks(ctx, fileHandle)
	if len(locks) != 1 {
		t.Errorf("expected 1 lock after upgrade, got %d", len(locks))
	}
	if !locks[0].Exclusive {
		t.Error("lock should be exclusive after upgrade")
	}
}

func TestMemoryStore_LockAcquisitionTime(t *testing.T) {
	store, fileHandle := createTestStore(t)
	ctx := context.Background()
	authCtx := createAuthContext(ctx)

	before := time.Now()

	lock := metadata.FileLock{
		SessionID: 1,
		Offset:    0,
		Length:    100,
		Exclusive: true,
	}
	err := store.LockFile(authCtx, fileHandle, lock)
	if err != nil {
		t.Fatalf("failed to acquire lock: %v", err)
	}

	after := time.Now()

	locks, _ := store.ListLocks(ctx, fileHandle)
	if len(locks) != 1 {
		t.Fatal("expected 1 lock")
	}

	if locks[0].AcquiredAt.Before(before) || locks[0].AcquiredAt.After(after) {
		t.Errorf("AcquiredAt should be between test start and end: %v", locks[0].AcquiredAt)
	}
}

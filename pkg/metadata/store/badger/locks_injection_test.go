package badger

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// TestListLocksByOwner_NoKeyInjection asserts that an OwnerID containing the
// key separator ':' cannot forge an extra index segment that a prefix scan for
// a different (victim) owner would match. NLM OwnerIDs legitimately contain ':'
// (e.g. "nlm:caller:svid:oh") and come straight from the wire, so without
// hex-encoding the indexed value an attacker registering OwnerID
// "victim:fakeLockID" plants a key under the "victim" prefix that an owner-keyed
// scan for the victim would also match.
func TestListLocksByOwner_NoKeyInjection(t *testing.T) {
	ctx := context.Background()
	store := newLockTestStore(t)

	// Victim: a legitimate lock owned by "victim".
	victimLock := &lock.PersistedLock{
		ID:       "victim-lock",
		FileID:   "fileV",
		OwnerID:  "victim",
		ClientID: "clientV",
	}
	if err := store.PutLock(ctx, victimLock); err != nil {
		t.Fatalf("Put victim lock: %v", err)
	}

	// Attacker: OwnerID crafted to inject the victim's prefix + a lock ID.
	attackerLock := &lock.PersistedLock{
		ID:       "attacker-lock",
		FileID:   "fileA",
		OwnerID:  "victim:attacker-lock",
		ClientID: "clientA",
	}
	if err := store.PutLock(ctx, attackerLock); err != nil {
		t.Fatalf("Put attacker lock: %v", err)
	}

	// Scanning locks owned by "victim" via the owner index (ListLocks routes
	// through prefixLockByOwner) must touch exactly the genuine victim lock.
	locks, err := store.ListLocks(ctx, lock.LockQuery{OwnerID: "victim"})
	if err != nil {
		t.Fatalf("ListLocks(owner=victim): %v", err)
	}
	if len(locks) != 1 || locks[0].ID != "victim-lock" {
		t.Fatalf("ListLocks(owner=victim) returned %+v; want exactly [victim-lock] (key-injection regression)", locks)
	}

	// The attacker lock must remain queryable under its true owner.
	atk, err := store.ListLocks(ctx, lock.LockQuery{OwnerID: "victim:attacker-lock"})
	if err != nil {
		t.Fatalf("ListLocks(owner=attacker): %v", err)
	}
	if len(atk) != 1 || atk[0].ID != "attacker-lock" {
		t.Fatalf("ListLocks for attacker owner returned %+v; want exactly [attacker-lock]", atk)
	}
}

// TestDeleteLocksByClient_NoKeyInjection asserts the client index resists the
// same ':' injection through DeleteLocksByClient, the bulk cleanup path.
func TestDeleteLocksByClient_NoKeyInjection(t *testing.T) {
	ctx := context.Background()
	store := newLockTestStore(t)

	if err := store.PutLock(ctx, &lock.PersistedLock{
		ID:       "victim-lock",
		FileID:   "fileV",
		OwnerID:  "ownerV",
		ClientID: "victim",
	}); err != nil {
		t.Fatalf("Put victim lock: %v", err)
	}
	if err := store.PutLock(ctx, &lock.PersistedLock{
		ID:       "attacker-lock",
		FileID:   "fileA",
		OwnerID:  "ownerA",
		ClientID: "victim:attacker-lock",
	}); err != nil {
		t.Fatalf("Put attacker lock: %v", err)
	}

	count, err := store.DeleteLocksByClient(ctx, "victim")
	if err != nil {
		t.Fatalf("DeleteLocksByClient(victim): %v", err)
	}
	if count != 1 {
		t.Fatalf("DeleteLocksByClient(victim) deleted %d locks; want 1 (key-injection regression)", count)
	}

	// The attacker lock must survive.
	if _, err := store.GetLock(ctx, "attacker-lock"); err != nil {
		t.Fatalf("attacker lock was unexpectedly deleted by the victim client scan: %v", err)
	}
	// The victim lock must be gone.
	if _, err := store.GetLock(ctx, "victim-lock"); err == nil {
		t.Fatalf("victim lock still present after DeleteLocksByClient; want removed")
	}
}

// TestLockIndex_ColonRoundTrip is a control: OwnerID/FileID/ClientID values that
// legitimately contain ':' still index, query, and delete correctly.
func TestLockIndex_ColonRoundTrip(t *testing.T) {
	ctx := context.Background()
	store := newLockTestStore(t)

	lk := &lock.PersistedLock{
		ID:       "lock-1",
		FileID:   "fh:00:01",
		OwnerID:  "nlm:host1:1234:abcd",
		ClientID: "smb:session456:pid789",
	}
	if err := store.PutLock(ctx, lk); err != nil {
		t.Fatalf("Put: %v", err)
	}

	for _, q := range []lock.LockQuery{
		{FileID: lk.FileID},
		{OwnerID: lk.OwnerID},
		{ClientID: lk.ClientID},
	} {
		got, err := store.ListLocks(ctx, q)
		if err != nil {
			t.Fatalf("ListLocks(%+v): %v", q, err)
		}
		if len(got) != 1 || got[0].ID != "lock-1" {
			t.Fatalf("ListLocks(%+v) returned %+v; want exactly [lock-1]", q, got)
		}
	}

	// Bulk cleanup by file must find and remove the lock.
	count, err := store.DeleteLocksByFile(ctx, lk.FileID)
	if err != nil {
		t.Fatalf("DeleteLocksByFile: %v", err)
	}
	if count != 1 {
		t.Fatalf("DeleteLocksByFile deleted %d; want 1", count)
	}
}

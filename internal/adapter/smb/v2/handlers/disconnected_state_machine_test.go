package handlers

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// TestDisconnectedConflictOnNewOpen exercises the MS-SMB2 §3.3.4.18 truth
// table for FRESH (non-reconnect) CREATE opens against disconnected durable
// handles, mirroring the matrix asserted by
// smb2.durable-v2-open.{keep,purge}-disconnected-* in
// source4/torture/smb2/durable_v2_open.c. The reconnect path is handled
// upstream by ProcessDurableReconnectContext and never reaches this
// predicate — see the contract comment on disconnectedConflictOnNewOpen.
func TestDisconnectedConflictOnNewOpen(t *testing.T) {
	const (
		rh          = smbLeaseRead | smbLeaseHandle
		rwh         = smbLeaseRead | smbLeaseHandle | smbLeaseWrite
		none uint32 = 0

		shareAll         = smbShareRead | smbShareWrite | smbShareDelete
		shareNone uint32 = 0

		// New-open desired-access masks.
		accessReadWrite uint32 = 0x00000001 | 0x00000002 // FILE_READ_DATA | FILE_WRITE_DATA
		accessStatOnly  uint32 = 0x00000080              // FILE_READ_ATTRIBUTES only
	)

	leaseA := [16]byte{0x01}
	leaseB := [16]byte{0x02}

	tests := []struct {
		name             string
		dLeaseState      uint32
		dLeaseKey        [16]byte
		dShareAccess     uint32
		newLeaseState    uint32
		newLeaseKey      [16]byte
		newShareAccess   uint32
		newDesiredAccess uint32
		newIsStatOnly    bool
		want             disconnectedHandleAction
	}{
		{
			// keep-disconnected-rh-with-stat-open: stat open carries no lease
			// and default share-all. Disconnected RH must be preserved.
			name:             "keep_RH_with_stat",
			dLeaseState:      rh,
			dLeaseKey:        leaseA,
			dShareAccess:     shareAll,
			newLeaseState:    none,
			newLeaseKey:      [16]byte{},
			newShareAccess:   shareAll,
			newDesiredAccess: accessStatOnly,
			newIsStatOnly:    true,
			want:             disconnectedActionPreserve,
		},
		{
			// keep-disconnected-rwh-with-stat-open mirrors keep_RH_with_stat:
			// disconnected RWH with stat open from a different connection
			// must also be preserved (stat open does not break leases).
			name:             "keep_RWH_with_stat",
			dLeaseState:      rwh,
			dLeaseKey:        leaseA,
			dShareAccess:     shareAll,
			newLeaseState:    none,
			newLeaseKey:      [16]byte{},
			newShareAccess:   shareAll,
			newDesiredAccess: accessStatOnly,
			newIsStatOnly:    true,
			want:             disconnectedActionPreserve,
		},
		{
			// keep-disconnected-rh-with-rh-open: two RH leases on different
			// keys can coexist — disconnected stays. D doesn't hold W and its
			// share mode is share-all, so falls through to preserve.
			name:             "keep_RH_with_RH_diff_key",
			dLeaseState:      rh,
			dLeaseKey:        leaseA,
			dShareAccess:     shareAll,
			newLeaseState:    rh,
			newLeaseKey:      leaseB,
			newShareAccess:   shareAll,
			newDesiredAccess: accessReadWrite,
			want:             disconnectedActionPreserve,
		},
		{
			// keep-disconnected-rh-with-rwh-open: disconnected RH stays
			// even when the new open requests RWH — the new open just gets
			// downgraded to RH (W denied by the H-holder), no break needed.
			// D doesn't hold W, so the W-on-W rule doesn't fire.
			name:             "keep_RH_with_RWH_diff_key",
			dLeaseState:      rh,
			dLeaseKey:        leaseA,
			dShareAccess:     shareAll,
			newLeaseState:    rwh,
			newLeaseKey:      leaseB,
			newShareAccess:   shareAll,
			newDesiredAccess: accessReadWrite,
			want:             disconnectedActionPreserve,
		},
		{
			// purge-disconnected-rwh-with-rwh-open: disconnected RWH and a
			// new RWH on a different key cannot coexist; the disconnected
			// loses W → loses H → purge.
			name:             "purge_RWH_with_RWH_diff_key",
			dLeaseState:      rwh,
			dLeaseKey:        leaseA,
			dShareAccess:     shareAll,
			newLeaseState:    rwh,
			newLeaseKey:      leaseB,
			newShareAccess:   shareAll,
			newDesiredAccess: accessReadWrite,
			want:             disconnectedActionPurge,
		},
		{
			// purge-disconnected-rwh-with-rh-open: disconnected RWH and a
			// new RH on a different key. The W must be broken; cascade
			// strips H → purge.
			name:             "purge_RWH_with_RH_diff_key",
			dLeaseState:      rwh,
			dLeaseKey:        leaseA,
			dShareAccess:     shareAll,
			newLeaseState:    rh,
			newLeaseKey:      leaseB,
			newShareAccess:   shareAll,
			newDesiredAccess: accessReadWrite,
			want:             disconnectedActionPurge,
		},
		{
			// purge-disconnected-rh-with-share-none-open: SHARE_NONE open on
			// a file with a disconnected RH must purge.
			name:             "purge_RH_with_share_none",
			dLeaseState:      rh,
			dLeaseKey:        leaseA,
			dShareAccess:     shareAll,
			newLeaseState:    none,
			newLeaseKey:      [16]byte{},
			newShareAccess:   shareNone,
			newDesiredAccess: accessReadWrite,
			want:             disconnectedActionPurge,
		},
		{
			// Zero-key W-holder vs new lease (any key): zero key is "no key"
			// and never matches another, including another zero. Two opens
			// with empty lease keys cannot coexist on a W lease. This pins
			// the finding-#4 behaviour where oplock-V2 (no lease) reconnect
			// no longer accidentally hides behind the old reconnect-guard.
			name:             "purge_RWH_zero_key_vs_RH",
			dLeaseState:      rwh,
			dLeaseKey:        [16]byte{},
			dShareAccess:     shareAll,
			newLeaseState:    rh,
			newLeaseKey:      leaseB,
			newShareAccess:   shareAll,
			newDesiredAccess: accessReadWrite,
			want:             disconnectedActionPurge,
		},
		{
			// durable_open.open2-lease (V1): D held an RH lease opened with
			// FILE_SHARE_NONE; a later read/write open with share-all
			// conflicts with D's own deny mode → purge. D holds no W, so this
			// fires via the share-deny rule, not the W-on-W rule.
			name:             "purge_RH_shareNone_D_vs_readwrite_open",
			dLeaseState:      rh,
			dLeaseKey:        leaseA,
			dShareAccess:     shareNone,
			newLeaseState:    none,
			newLeaseKey:      [16]byte{},
			newShareAccess:   shareAll,
			newDesiredAccess: accessReadWrite,
			want:             disconnectedActionPurge,
		},
		{
			// durable_open.oplock / open2-oplock (V1): D held a BATCH oplock,
			// persisted as an RWH synthetic lease (zero/synthetic key), opened
			// share-all. A plain second open with no lease and real data access
			// breaks the exclusive oplock → purge. Exercises the W-holder rule
			// firing even though newLeaseState == 0.
			name:             "purge_batch_oplock_D_vs_no_lease_open",
			dLeaseState:      rwh,
			dLeaseKey:        leaseA,
			dShareAccess:     shareAll,
			newLeaseState:    none,
			newLeaseKey:      [16]byte{},
			newShareAccess:   shareAll,
			newDesiredAccess: accessReadWrite,
			want:             disconnectedActionPurge,
		},
		{
			// Negative control for the oplock rule: same as above but the
			// second open is stat-only — an exclusive oplock does NOT break on
			// a stat open (Samba is_stat_open), so D must be preserved.
			name:             "keep_batch_oplock_D_vs_stat_open",
			dLeaseState:      rwh,
			dLeaseKey:        leaseA,
			dShareAccess:     shareAll,
			newLeaseState:    none,
			newLeaseKey:      [16]byte{},
			newShareAccess:   shareAll,
			newDesiredAccess: accessStatOnly,
			newIsStatOnly:    true,
			want:             disconnectedActionPreserve,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := disconnectedConflictOnNewOpen(
				tc.dLeaseState, tc.dLeaseKey, tc.dShareAccess,
				tc.newLeaseState, tc.newLeaseKey,
				tc.newShareAccess, tc.newDesiredAccess, tc.newIsStatOnly,
			)
			if got != tc.want {
				t.Fatalf("got=%v want=%v", got, tc.want)
			}
		})
	}
}

// TestDisconnectedConflictOnDataChange covers the WRITE/RENAME break paths.
// The fast-path `breakToBelowHandle == false` early-return is the caller's
// responsibility; the predicate here only distinguishes own-handle (preserve)
// from foreign-handle (purge), including the LeaseState=0 case (purged
// because such a handle has no caching rights and no reconnect prospect).
func TestDisconnectedConflictOnDataChange(t *testing.T) {
	leaseA := [16]byte{0x01}
	leaseB := [16]byte{0x02}

	// Writer's lease (excludeLeaseKey == leaseA) → its own disconnected D
	// entries are never purged on its own WRITE/RENAME.
	if got := disconnectedConflictOnDataChange(leaseA, leaseA); got != disconnectedActionPreserve {
		t.Fatalf("same key: got=%v want preserve", got)
	}

	// Different lease key → purge.
	if got := disconnectedConflictOnDataChange(leaseA, leaseB); got != disconnectedActionPurge {
		t.Fatalf("diff key: got=%v want purge", got)
	}

	// Disconnected handle with all-zero lease key (no lease persisted) is
	// foreign to any keyed actor and gets purged. This pins the finding-#1
	// fix: dLeaseState=0 records (which always carry zero lease keys) no
	// longer silently preserve.
	if got := disconnectedConflictOnDataChange([16]byte{}, leaseA); got != disconnectedActionPurge {
		t.Fatalf("zero key vs foreign actor: got=%v want purge", got)
	}

	// Both keys zero → still purge: the same-actor short-circuit explicitly
	// requires the key to be non-zero before treating it as "same actor",
	// otherwise every zero-key actor would be treated as the same.
	if got := disconnectedConflictOnDataChange([16]byte{}, [16]byte{}); got != disconnectedActionPurge {
		t.Fatalf("both zero keys: got=%v want purge", got)
	}
}

// TestPurgeConflictingDisconnectedHandlesForDataChange covers the wrapper
// behaviour: only handles with non-zero DisconnectedAt are inspected, the
// predicate is invoked per-handle, and the durablePurgeMu critical section
// covers the Get → Delete window. Mock store reuses durable_scavenger_test.go's
// mockDurableStore.
func TestPurgeConflictingDisconnectedHandlesForDataChange(t *testing.T) {
	now := time.Now()
	metaHandle := []byte("file-handle-1")

	tests := []struct {
		name            string
		seed            []*lock.PersistedDurableHandle
		excludeLeaseKey [16]byte
		breakBelowH     bool
		wantPurged      int
		wantRemaining   int
	}{
		{
			name: "preserves_own_handle",
			seed: []*lock.PersistedDurableHandle{
				{
					ID:             "self",
					MetadataHandle: metaHandle,
					LeaseKey:       [16]byte{0x01},
					LeaseState:     smbLeaseRead | smbLeaseHandle,
					DisconnectedAt: now.Add(-time.Second),
				},
			},
			excludeLeaseKey: [16]byte{0x01},
			breakBelowH:     true,
			wantPurged:      0,
			wantRemaining:   1,
		},
		{
			name: "purges_foreign_handle_with_R_lease",
			seed: []*lock.PersistedDurableHandle{
				{
					ID:             "foreign",
					MetadataHandle: metaHandle,
					LeaseKey:       [16]byte{0x02},
					LeaseState:     smbLeaseRead | smbLeaseHandle,
					DisconnectedAt: now.Add(-time.Second),
				},
			},
			excludeLeaseKey: [16]byte{0x01},
			breakBelowH:     true,
			wantPurged:      1,
			wantRemaining:   0,
		},
		{
			// Finding #1: a handle with LeaseState=0 (lease previously
			// downgraded pre-disconnect) is unreconnectable and must be
			// purged on a foreign data-change, not preserved.
			name: "purges_foreign_handle_with_zero_lease_state",
			seed: []*lock.PersistedDurableHandle{
				{
					ID:             "foreign-no-lease",
					MetadataHandle: metaHandle,
					LeaseKey:       [16]byte{0x03},
					LeaseState:     0,
					DisconnectedAt: now.Add(-time.Second),
				},
			},
			excludeLeaseKey: [16]byte{0x01},
			breakBelowH:     true,
			wantPurged:      1,
			wantRemaining:   0,
		},
		{
			// Live (not yet disconnected) handles are skipped — they're
			// active opens whose lease break is handled inline.
			name: "skips_live_handles",
			seed: []*lock.PersistedDurableHandle{
				{
					ID:             "live",
					MetadataHandle: metaHandle,
					LeaseKey:       [16]byte{0x04},
					LeaseState:     smbLeaseRead | smbLeaseHandle,
					DisconnectedAt: time.Time{},
				},
			},
			excludeLeaseKey: [16]byte{0x01},
			breakBelowH:     true,
			wantPurged:      0,
			wantRemaining:   1,
		},
		{
			// Fast-path: breakBelowH=false skips the lookup entirely.
			name: "fast_path_break_preserves_H",
			seed: []*lock.PersistedDurableHandle{
				{
					ID:             "foreign",
					MetadataHandle: metaHandle,
					LeaseKey:       [16]byte{0x05},
					LeaseState:     smbLeaseRead | smbLeaseHandle,
					DisconnectedAt: now.Add(-time.Second),
				},
			},
			excludeLeaseKey: [16]byte{0x01},
			breakBelowH:     false,
			wantPurged:      0,
			wantRemaining:   1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := newMockDurableStore()
			for _, h := range tc.seed {
				_ = store.PutDurableHandle(context.Background(), h)
			}
			h := &Handler{DurableStore: store}
			got := h.purgeConflictingDisconnectedHandlesForDataChange(
				context.Background(), metaHandle, tc.excludeLeaseKey, tc.breakBelowH,
			)
			if got != tc.wantPurged {
				t.Fatalf("purged=%d want=%d", got, tc.wantPurged)
			}
			if c := store.count(); c != tc.wantRemaining {
				t.Fatalf("store count=%d want=%d", c, tc.wantRemaining)
			}
		})
	}
}

// TestPurgeConflictingDisconnectedHandlesForOpen pins the wrapper behaviour
// for the CREATE path: zero-length metaHandle is a no-op, live handles are
// skipped, and the SHARE_NONE / W-on-W rules fire through the predicate.
func TestPurgeConflictingDisconnectedHandlesForOpen(t *testing.T) {
	const (
		rh  = smbLeaseRead | smbLeaseHandle
		rwh = smbLeaseRead | smbLeaseHandle | smbLeaseWrite

		shareAll uint32 = smbShareRead | smbShareWrite | smbShareDelete
	)
	now := time.Now()
	metaHandle := []byte("file-handle-2")

	const accessReadWrite uint32 = 0x00000001 | 0x00000002 // FILE_READ_DATA | FILE_WRITE_DATA

	store := newMockDurableStore()
	_ = store.PutDurableHandle(context.Background(), &lock.PersistedDurableHandle{
		ID:             "disconnected-rwh",
		MetadataHandle: metaHandle,
		LeaseKey:       [16]byte{0x01},
		LeaseState:     rwh,
		ShareAccess:    shareAll,
		DisconnectedAt: now.Add(-time.Second),
	})
	_ = store.PutDurableHandle(context.Background(), &lock.PersistedDurableHandle{
		ID:             "live",
		MetadataHandle: metaHandle,
		LeaseKey:       [16]byte{0x02},
		LeaseState:     rwh,
		ShareAccess:    shareAll,
		// Not disconnected — must be skipped.
		DisconnectedAt: time.Time{},
	})

	h := &Handler{DurableStore: store}
	// New RH open with a different key → purges the disconnected RWH (W
	// must be broken; cascade strips H), leaves the live handle alone.
	got := h.purgeConflictingDisconnectedHandlesForOpen(
		context.Background(), metaHandle,
		rh, [16]byte{0x99}, shareAll, accessReadWrite,
	)
	if got != 1 {
		t.Fatalf("purged=%d want=1", got)
	}
	if c := store.count(); c != 1 {
		t.Fatalf("remaining=%d want=1", c)
	}
}

// TestDurablePurgeMuSerializesPersistAndPurge proves the durablePurgeMu
// critical section serializes a disconnect-time PutDurableHandle against a
// concurrent create-time purge, closing the finding-#3 TOCTOU. Runs under
// `go test -race`.
func TestDurablePurgeMuSerializesPersistAndPurge(t *testing.T) {
	store := newMockDurableStore()
	h := &Handler{DurableStore: store}
	metaHandle := []byte("file-handle-3")

	const iterations = 500
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			h.durablePurgeMu.Lock()
			_ = store.PutDurableHandle(context.Background(), &lock.PersistedDurableHandle{
				ID:             "disconnected",
				MetadataHandle: metaHandle,
				LeaseState:     smbLeaseRead | smbLeaseHandle,
				LeaseKey:       [16]byte{0x01},
				DisconnectedAt: time.Now(),
			})
			h.durablePurgeMu.Unlock()
		}
	}()

	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			// SHARE_NONE forces purge of any disconnected handle on the file.
			h.purgeConflictingDisconnectedHandlesForOpen(
				context.Background(), metaHandle, 0, [16]byte{}, 0, 0,
			)
		}
	}()
	wg.Wait()
	// No assertion on final state — race detector is the gate.
}

// TestShouldPersistDurableOnDisconnect pins down the lock-noW-lease persist
// gate (smb2.durable-v2-open.lock-noW-lease, MS-SMB2 §3.3.4.18).
func TestShouldPersistDurableOnDisconnect(t *testing.T) {
	rh := smbLeaseRead | smbLeaseHandle
	rwh := smbLeaseRead | smbLeaseHandle | smbLeaseWrite

	// No locks → always persist.
	if !shouldPersistDurableOnDisconnect(rh, false) {
		t.Fatal("no BR-locks must persist regardless of lease state")
	}
	if !shouldPersistDurableOnDisconnect(0, false) {
		t.Fatal("no BR-locks must persist even without lease")
	}

	// BR-locks + lease has W → persist (W can re-establish on reconnect).
	if !shouldPersistDurableOnDisconnect(rwh, true) {
		t.Fatal("BR-locks with W lease must persist")
	}

	// BR-locks + lease lacks W → MUST refuse (lock-noW-lease).
	if shouldPersistDurableOnDisconnect(rh, true) {
		t.Fatal("BR-locks under non-W lease must refuse persist")
	}
	if shouldPersistDurableOnDisconnect(0, true) {
		t.Fatal("BR-locks without lease must refuse persist")
	}
}

// TestOpenHasLocksFailClosed pins the fail-closed semantics of the
// disconnect-time lock-noW-lease gate: any uncertainty about whether the
// open holds byte-range locks MUST resolve to "has locks" so the caller
// refuses to persist a durable handle that could silently bypass the gate.
func TestOpenHasLocksFailClosed(t *testing.T) {
	t.Run("nil open returns false (nothing to gate)", func(t *testing.T) {
		if openHasLocks(nil, nil) {
			t.Fatal("nil open must return false; caller short-circuits")
		}
	})

	t.Run("optimistic flag true is authoritative", func(t *testing.T) {
		of := &OpenFile{}
		of.HasByteRangeLocks.Store(true)
		if !openHasLocks(nil, of) {
			t.Fatal("flag=true must report has-locks regardless of meta service")
		}
	})

	t.Run("nil meta service with flag false fails closed", func(t *testing.T) {
		of := &OpenFile{MetadataHandle: []byte("ignored-without-svc")}
		if !openHasLocks(nil, of) {
			t.Fatal("missing meta service must fail closed (return true)")
		}
	})

	t.Run("empty metadata handle with svc fails closed", func(t *testing.T) {
		svc := metadata.New()
		of := &OpenFile{}
		if !openHasLocks(svc, of) {
			t.Fatal("empty metadata handle must fail closed (return true)")
		}
	})

	t.Run("malformed handle (decode failure) fails closed", func(t *testing.T) {
		svc := metadata.New()
		// Garbage bytes that DecodeFileHandle will reject, forcing
		// GetLockManagerForHandle to return an error.
		of := &OpenFile{MetadataHandle: []byte{0xff, 0xff, 0xff, 0xff}}
		if !openHasLocks(svc, of) {
			t.Fatal("lock manager lookup failure must fail closed (return true)")
		}
	})
}

// TestDurableOpenConflictingOpenPurgesAcrossConnections is the cross-connection
// repro for issue #808: it drives the SAME entrypoints the live SMB CREATE path
// uses — purgeConflictingDisconnectedHandlesForOpen (the Step 8a-bis purge
// wrapper invoked on every fresh CREATE) and ProcessDurableReconnectContext
// (the DHnC reconnect entrypoint) — across two connections sharing one durable
// store.
//
// Sequence per mode:
//  1. Session A opens a durable handle and transport-disconnects → persisted.
//  2. Session B issues a CONFLICTING fresh CREATE on the same file → the purge
//     wrapper must delete A's disconnected handle.
//  3. Session A reconnects via DHnC → must fail OBJECT_NAME_NOT_FOUND because
//     the handle is gone.
//
// Modes mirror the three smbtorture failures the issue targets:
//   - oplock / open2-oplock: A held a BATCH oplock (persisted RWH synthetic
//     lease), B opens with no oplock and real data access → break → purge.
//   - open2-lease: A held an RH lease with FILE_SHARE_NONE, B opens read/write
//     with share-all → A's own deny mode is violated → break → purge.
//
// The negative control (keep-disconnected) proves a NON-conflicting open
// (stat-only second open against the same RWH/RH holder) leaves the handle
// reconnectable — guarding against over-purge regression.
func TestDurableOpenConflictingOpenPurgesAcrossConnections(t *testing.T) {
	const (
		rh  = smbLeaseRead | smbLeaseHandle
		rwh = smbLeaseRead | smbLeaseHandle | smbLeaseWrite

		shareAll  uint32 = smbShareRead | smbShareWrite | smbShareDelete
		shareNone uint32 = 0

		accessReadWrite uint32 = 0x00000001 | 0x00000002 // FILE_READ_DATA | FILE_WRITE_DATA
		accessStatOnly  uint32 = 0x00000080              // FILE_READ_ATTRIBUTES only
	)

	keyHash := makeSessionKeyHash("session-A-key")
	metaHandle := []byte{0xDE, 0xAD, 0xBE, 0xEF}
	// Persisted FileIDs zero the volatile half — see buildPersistedDurableHandle.
	fileID := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 0, 0, 0, 0, 0, 0, 0, 0}

	// seedSessionA persists session A's disconnected durable handle. leaseKey
	// is zero for an oplock-backed (traditional) holder and non-zero for a
	// lease-backed holder; the V1 DHnC reconnect harness only re-establishes
	// oplock-backed handles without a lease-request context, so the keep
	// controls (which assert reconnect success) use leaseKey=zero.
	seedSessionA := func(store *mockDurableStore, dLeaseState, dShareAccess uint32, oplock uint8, leaseKey [16]byte) {
		_ = store.PutDurableHandle(context.Background(), &lock.PersistedDurableHandle{
			ID:             "session-A",
			FileID:         fileID,
			Path:           "conflict.dat",
			ShareName:      "/share1",
			DesiredAccess:  accessReadWrite,
			ShareAccess:    dShareAccess,
			MetadataHandle: metaHandle,
			OplockLevel:    oplock,
			LeaseKey:       leaseKey,
			LeaseState:     dLeaseState,
			Username:       "alice",
			SessionKeyHash: keyHash,
			IsV2:           false,
			CreatedAt:      time.Now().Add(-time.Minute),
			DisconnectedAt: time.Now().Add(-10 * time.Second),
			TimeoutMs:      60000,
		})
	}

	// reconnectA models session A's DHnC reconnect through the real entrypoint
	// and returns the resulting status.
	reconnectA := func(store *mockDurableStore) types.Status {
		dhnCData := make([]byte, 16)
		copy(dhnCData, fileID[:])
		_, status, err := ProcessDurableReconnectContext(
			context.Background(), store, nil,
			[]CreateContext{{Name: DurableHandleV1ReconnectTag, Data: dhnCData}},
			777, "alice", keyHash, "/share1", "conflict.dat", [16]byte{},
		)
		if err != nil {
			t.Fatalf("reconnect error: %v", err)
		}
		return status
	}

	conflictCases := []struct {
		name         string
		dLeaseState  uint32
		dShareAccess uint32
		dOplock      uint8
		dLeaseKey    [16]byte
		// session B's conflicting fresh open
		bLeaseState    uint32
		bLeaseKey      [16]byte
		bShareAccess   uint32
		bDesiredAccess uint32
	}{
		{
			// durable_open.oplock + open2-oplock: BATCH oplock holder
			// (oplock-backed, zero lease key), non-stat second open with no
			// lease breaks the exclusive oplock.
			name:           "oplock_batch_holder_broken_by_plain_open",
			dLeaseState:    rwh,
			dShareAccess:   shareAll,
			dOplock:        OplockLevelBatch,
			dLeaseKey:      [16]byte{},
			bLeaseState:    0,
			bLeaseKey:      [16]byte{},
			bShareAccess:   shareAll,
			bDesiredAccess: accessReadWrite,
		},
		{
			// durable_open.open2-lease: RH lease holder opened FILE_SHARE_NONE;
			// a read/write open violates A's own deny mode.
			name:           "rh_lease_shareNone_holder_broken_by_readwrite",
			dLeaseState:    rh,
			dShareAccess:   shareNone,
			dOplock:        OplockLevelLease,
			dLeaseKey:      [16]byte{0xA1},
			bLeaseState:    0,
			bLeaseKey:      [16]byte{},
			bShareAccess:   shareAll,
			bDesiredAccess: accessReadWrite,
		},
	}

	for _, tc := range conflictCases {
		t.Run("purge/"+tc.name, func(t *testing.T) {
			store := newMockDurableStore()
			h := &Handler{DurableStore: store}
			seedSessionA(store, tc.dLeaseState, tc.dShareAccess, tc.dOplock, tc.dLeaseKey)

			// BEFORE the fix this purge was a no-op and reconnect returned
			// SUCCESS. Assert the conflicting open purges exactly one handle.
			purged := h.purgeConflictingDisconnectedHandlesForOpen(
				context.Background(), metaHandle,
				tc.bLeaseState, tc.bLeaseKey, tc.bShareAccess, tc.bDesiredAccess,
			)
			if purged != 1 {
				t.Fatalf("conflicting open purged=%d, want 1", purged)
			}
			if status := reconnectA(store); status != types.StatusObjectNameNotFound {
				t.Fatalf("reconnect after conflicting open: got %s, want OBJECT_NAME_NOT_FOUND", status)
			}
		})
	}

	// Negative control: a stat-only second open must NOT purge the disconnected
	// handle, so reconnect still succeeds. Covers keep-disconnected-* and guards
	// against over-purge.
	keepCases := []struct {
		name         string
		dLeaseState  uint32
		dShareAccess uint32
		dOplock      uint8
	}{
		// Both oplock-backed (zero lease key) so the V1 DHnC harness can
		// re-establish them without a lease-request context. rwh exercises the
		// W-holder preserve-on-stat path; rh the non-W preserve path.
		{"keep/rwh_holder_with_stat_open", rwh, shareAll, OplockLevelBatch},
		{"keep/rh_holder_with_stat_open", rh, shareAll, OplockLevelII},
	}
	for _, tc := range keepCases {
		t.Run(tc.name, func(t *testing.T) {
			store := newMockDurableStore()
			h := &Handler{DurableStore: store}
			seedSessionA(store, tc.dLeaseState, tc.dShareAccess, tc.dOplock, [16]byte{})

			purged := h.purgeConflictingDisconnectedHandlesForOpen(
				context.Background(), metaHandle,
				0, [16]byte{}, shareAll, accessStatOnly,
			)
			if purged != 0 {
				t.Fatalf("stat-only open purged=%d, want 0 (no over-purge)", purged)
			}
			if status := reconnectA(store); status != types.StatusSuccess {
				t.Fatalf("reconnect after non-conflicting stat open: got %s, want SUCCESS", status)
			}
		})
	}
}

// TestDisallowWriteLeaseForFile exercises the Samba `disallow_write_lease`
// accumulator (source3/smbd/open.c::delay_for_oplock_fn) as implemented by
// Handler.disallowWriteLeaseForFile. The predicate must strip W when a
// conflicting non-stat live open or a disconnected durable handle exists, and
// MUST NOT strip W for the requestor's own open (same lease key or same
// FileID), stat-only opens, opens on other files, or directories/pipes. The
// false cases are the regression guards for the 13 lease tests that a coarse
// "any non-stat open" cap previously broke.
func TestDisallowWriteLeaseForFile(t *testing.T) {
	const (
		rh             = smbLeaseRead | smbLeaseHandle
		rwh            = smbLeaseRead | smbLeaseHandle | smbLeaseWrite
		shareAll       = smbShareRead | smbShareWrite | smbShareDelete
		accessReadCtl  = 0x00000080 | 0x00000100 | 0x00100000 | 0x00020000 // RD_ATTR|WR_ATTR|SYNC|READ_CONTROL (non-oplock-stat)
		accessStatOnly = 0x00000080 | 0x00000100 | 0x00100000              // RD_ATTR|WR_ATTR|SYNC (oplock-stat)
		accessReadData = 0x00000001                                        // FILE_READ_DATA (non-stat)
	)
	metaHandle := []byte("file-A")
	otherHandle := []byte("file-B")
	selfKey := [16]byte{0xAA}
	selfFileID := [16]byte{0xA1}
	selfClientGUID := [16]byte{0xC1}
	otherClientGUID := [16]byte{0xC2}

	// addOpen registers a live open. clientGUID defaults to otherClientGUID when
	// zero so the existing conflict cases (which leave it unset) keep modelling a
	// DIFFERENT client than the requestor.
	addOpen := func(h *Handler, fileID [16]byte, mh []byte, access uint32, leaseKey [16]byte, oplock uint8, dir bool, clientGUID [16]byte) {
		if clientGUID == ([16]byte{}) {
			clientGUID = otherClientGUID
		}
		h.StoreOpenFile(&OpenFile{
			FileID:         fileID,
			MetadataHandle: mh,
			DesiredAccess:  access,
			LeaseKey:       leaseKey,
			OplockLevel:    oplock,
			IsDirectory:    dir,
			ClientGUID:     clientGUID,
		})
	}

	t.Run("non-stat live open of another opener disallows W", func(t *testing.T) {
		h := &Handler{}
		// h1: READ_CONTROL-bearing open, no lease (the nonstat-and-lease shape).
		addOpen(h, [16]byte{0x01}, metaHandle, accessReadCtl, [16]byte{}, OplockLevelNone, false, [16]byte{})
		if !h.disallowWriteLeaseForFile(context.Background(), metaHandle, selfKey, selfFileID, selfClientGUID) {
			t.Fatal("expected disallow=true for conflicting non-oplock-stat open")
		}
	})

	t.Run("stat-only live open does not disallow W", func(t *testing.T) {
		h := &Handler{}
		addOpen(h, [16]byte{0x02}, metaHandle, accessStatOnly, [16]byte{}, OplockLevelNone, false, [16]byte{})
		if h.disallowWriteLeaseForFile(context.Background(), metaHandle, selfKey, selfFileID, selfClientGUID) {
			t.Fatal("expected disallow=false for oplock-stat-only open")
		}
	})

	t.Run("own open by same lease key does not disallow W", func(t *testing.T) {
		h := &Handler{}
		addOpen(h, [16]byte{0x03}, metaHandle, accessReadData, selfKey, OplockLevelLease, false, [16]byte{})
		if h.disallowWriteLeaseForFile(context.Background(), metaHandle, selfKey, selfFileID, selfClientGUID) {
			t.Fatal("expected disallow=false for requestor's own lease key (reopen/upgrade)")
		}
	})

	t.Run("own open by same FileID does not disallow W", func(t *testing.T) {
		h := &Handler{}
		addOpen(h, selfFileID, metaHandle, accessReadData, [16]byte{}, OplockLevelNone, false, [16]byte{})
		if h.disallowWriteLeaseForFile(context.Background(), metaHandle, selfKey, selfFileID, selfClientGUID) {
			t.Fatal("expected disallow=false when the only other open is the requestor's own FileID")
		}
	})

	t.Run("open on a different file does not disallow W", func(t *testing.T) {
		h := &Handler{}
		addOpen(h, [16]byte{0x04}, otherHandle, accessReadData, [16]byte{}, OplockLevelNone, false, [16]byte{})
		if h.disallowWriteLeaseForFile(context.Background(), metaHandle, selfKey, selfFileID, selfClientGUID) {
			t.Fatal("expected disallow=false for non-stat open on a DIFFERENT file")
		}
	})

	t.Run("directory open does not disallow W", func(t *testing.T) {
		h := &Handler{}
		addOpen(h, [16]byte{0x05}, metaHandle, accessReadData, [16]byte{}, OplockLevelNone, true, [16]byte{})
		if h.disallowWriteLeaseForFile(context.Background(), metaHandle, selfKey, selfFileID, selfClientGUID) {
			t.Fatal("expected disallow=false for a directory open")
		}
	})

	t.Run("disconnected RH durable handle disallows W", func(t *testing.T) {
		store := newMockDurableStore()
		_ = store.PutDurableHandle(context.Background(), &lock.PersistedDurableHandle{
			ID:             "disc-rh",
			MetadataHandle: metaHandle,
			LeaseKey:       [16]byte{0xBB},
			LeaseState:     rh,
			ShareAccess:    shareAll,
			DisconnectedAt: time.Now().Add(-time.Second),
		})
		h := &Handler{DurableStore: store}
		// keep-disconnected-rh-with-rwh-open: new RWH open (selfKey) must be
		// capped to RH by the disconnected RH handle, with no break.
		if !h.disallowWriteLeaseForFile(context.Background(), metaHandle, selfKey, selfFileID, selfClientGUID) {
			t.Fatal("expected disallow=true for a disconnected RH durable handle")
		}
	})

	t.Run("own disconnected handle (same key) does not disallow W", func(t *testing.T) {
		store := newMockDurableStore()
		_ = store.PutDurableHandle(context.Background(), &lock.PersistedDurableHandle{
			ID:             "disc-self",
			MetadataHandle: metaHandle,
			LeaseKey:       selfKey,
			LeaseState:     rh,
			ShareAccess:    shareAll,
			DisconnectedAt: time.Now().Add(-time.Second),
		})
		h := &Handler{DurableStore: store}
		// The requestor reconnecting its own durable handle must not self-cap.
		if h.disallowWriteLeaseForFile(context.Background(), metaHandle, selfKey, selfFileID, selfClientGUID) {
			t.Fatal("expected disallow=false for the requestor's own disconnected handle")
		}
	})

	t.Run("still-connected durable handle does not disallow W", func(t *testing.T) {
		store := newMockDurableStore()
		_ = store.PutDurableHandle(context.Background(), &lock.PersistedDurableHandle{
			ID:             "live-row",
			MetadataHandle: metaHandle,
			LeaseKey:       [16]byte{0xCC},
			LeaseState:     rh,
			ShareAccess:    shareAll,
			DisconnectedAt: time.Time{}, // never disconnected
		})
		h := &Handler{DurableStore: store}
		if h.disallowWriteLeaseForFile(context.Background(), metaHandle, selfKey, selfFileID, selfClientGUID) {
			t.Fatal("expected disallow=false for a not-yet-disconnected durable row")
		}
	})

	t.Run("no opens and no handles does not disallow W", func(t *testing.T) {
		h := &Handler{DurableStore: newMockDurableStore()}
		if h.disallowWriteLeaseForFile(context.Background(), metaHandle, selfKey, selfFileID, selfClientGUID) {
			t.Fatal("expected disallow=false on an idle file")
		}
	})

	// Same-client guard: a second lease on a DIFFERENT key but the SAME client
	// is the contended-upgrade / own-break case (smb2.lease.upgrade3,
	// smb2.lease.break). It must NOT cap W — the lock manager's
	// bestGrantableState already resolves the per-lease downgrade.
	t.Run("same-client non-stat open on a different key does not disallow W", func(t *testing.T) {
		h := &Handler{}
		// Different lease key, but owned by the requestor's own ClientGuid.
		addOpen(h, [16]byte{0x06}, metaHandle, accessReadData, [16]byte{0xDD}, OplockLevelLease, false, selfClientGUID)
		if h.disallowWriteLeaseForFile(context.Background(), metaHandle, selfKey, selfFileID, selfClientGUID) {
			t.Fatal("expected disallow=false for a same-client open on a different lease key")
		}
	})

	// nonstat-and-lease (#739): a same-client NO_OPLOCK non-stat open (the h1
	// handle: READ_CONTROL-style access, no lease) must STILL disallow W, even
	// though it shares the requestor's ClientGuid. Samba is_same_lease bypasses
	// only LEASE_OPLOCK entries; a NO_OPLOCK entry is never same-client bypassed.
	// Bypassing it would wrongly grant the h2 lease RWH instead of RH.
	t.Run("same-client NO_OPLOCK non-stat open disallows W", func(t *testing.T) {
		h := &Handler{}
		addOpen(h, [16]byte{0x07}, metaHandle, accessReadCtl, [16]byte{}, OplockLevelNone, false, selfClientGUID)
		if !h.disallowWriteLeaseForFile(context.Background(), metaHandle, selfKey, selfFileID, selfClientGUID) {
			t.Fatal("expected disallow=true for a same-client NO_OPLOCK non-stat open (nonstat-and-lease)")
		}
	})

	// Contrast / regression guard: the same-client open holding a LEASE is still
	// bypassed (is_same_lease) — this preserves smb2.lease.upgrade2/upgrade3/break.
	t.Run("same-client LEASE open on a different key does not disallow W", func(t *testing.T) {
		h := &Handler{}
		addOpen(h, [16]byte{0x08}, metaHandle, accessReadData, [16]byte{0xDC}, OplockLevelLease, false, selfClientGUID)
		if h.disallowWriteLeaseForFile(context.Background(), metaHandle, selfKey, selfFileID, selfClientGUID) {
			t.Fatal("expected disallow=false for a same-client LEASE open (still is_same_lease bypassed)")
		}
	})

	t.Run("same-client disconnected handle on a different key does not disallow W", func(t *testing.T) {
		store := newMockDurableStore()
		_ = store.PutDurableHandle(context.Background(), &lock.PersistedDurableHandle{
			ID:             "disc-same-client",
			MetadataHandle: metaHandle,
			LeaseKey:       [16]byte{0xEE},
			LeaseState:     rh,
			ShareAccess:    shareAll,
			ClientGUID:     selfClientGUID,
			DisconnectedAt: time.Now().Add(-time.Second),
		})
		h := &Handler{DurableStore: store}
		if h.disallowWriteLeaseForFile(context.Background(), metaHandle, selfKey, selfFileID, selfClientGUID) {
			t.Fatal("expected disallow=false for a same-client disconnected durable handle")
		}
	})

	// Cross-client negative control: the existing disconnected-RH conflict case
	// must STILL fire when the holder is a different client.
	t.Run("cross-client disconnected RH still disallows W", func(t *testing.T) {
		store := newMockDurableStore()
		_ = store.PutDurableHandle(context.Background(), &lock.PersistedDurableHandle{
			ID:             "disc-cross-client",
			MetadataHandle: metaHandle,
			LeaseKey:       [16]byte{0xBB},
			LeaseState:     rh,
			ShareAccess:    shareAll,
			ClientGUID:     otherClientGUID,
			DisconnectedAt: time.Now().Add(-time.Second),
		})
		h := &Handler{DurableStore: store}
		if !h.disallowWriteLeaseForFile(context.Background(), metaHandle, selfKey, selfFileID, selfClientGUID) {
			t.Fatal("expected disallow=true for a cross-client disconnected RH handle")
		}
	})
}

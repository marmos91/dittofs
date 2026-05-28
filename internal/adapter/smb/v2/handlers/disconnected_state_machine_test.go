package handlers

import (
	"context"
	"sync"
	"testing"
	"time"

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
	)

	leaseA := [16]byte{0x01}
	leaseB := [16]byte{0x02}

	tests := []struct {
		name           string
		dLeaseState    uint32
		dLeaseKey      [16]byte
		newLeaseState  uint32
		newLeaseKey    [16]byte
		newShareAccess uint32
		want           disconnectedHandleAction
	}{
		{
			// keep-disconnected-rh-with-stat-open: stat open carries no lease
			// and default share-all. Disconnected RH must be preserved.
			name:           "keep_RH_with_stat",
			dLeaseState:    rh,
			dLeaseKey:      leaseA,
			newLeaseState:  none,
			newLeaseKey:    [16]byte{},
			newShareAccess: shareAll,
			want:           disconnectedActionPreserve,
		},
		{
			// keep-disconnected-rwh-with-stat-open mirrors keep_RH_with_stat:
			// disconnected RWH with stat open from a different connection
			// must also be preserved (stat open does not break leases).
			name:           "keep_RWH_with_stat",
			dLeaseState:    rwh,
			dLeaseKey:      leaseA,
			newLeaseState:  none,
			newLeaseKey:    [16]byte{},
			newShareAccess: shareAll,
			want:           disconnectedActionPreserve,
		},
		{
			// keep-disconnected-rh-with-rh-open: two RH leases on different
			// keys can coexist — disconnected stays. Reaches the W-on-W rule
			// but D doesn't hold W, so falls through to preserve.
			name:           "keep_RH_with_RH_diff_key",
			dLeaseState:    rh,
			dLeaseKey:      leaseA,
			newLeaseState:  rh,
			newLeaseKey:    leaseB,
			newShareAccess: shareAll,
			want:           disconnectedActionPreserve,
		},
		{
			// keep-disconnected-rh-with-rwh-open: disconnected RH stays
			// even when the new open requests RWH — the new open just gets
			// downgraded to RH (W denied by the H-holder), no break needed.
			// D doesn't hold W, so the W-on-W rule doesn't fire.
			name:           "keep_RH_with_RWH_diff_key",
			dLeaseState:    rh,
			dLeaseKey:      leaseA,
			newLeaseState:  rwh,
			newLeaseKey:    leaseB,
			newShareAccess: shareAll,
			want:           disconnectedActionPreserve,
		},
		{
			// purge-disconnected-rwh-with-rwh-open: disconnected RWH and a
			// new RWH on a different key cannot coexist; the disconnected
			// loses W → loses H → purge.
			name:           "purge_RWH_with_RWH_diff_key",
			dLeaseState:    rwh,
			dLeaseKey:      leaseA,
			newLeaseState:  rwh,
			newLeaseKey:    leaseB,
			newShareAccess: shareAll,
			want:           disconnectedActionPurge,
		},
		{
			// purge-disconnected-rwh-with-rh-open: disconnected RWH and a
			// new RH on a different key. The W must be broken; cascade
			// strips H → purge.
			name:           "purge_RWH_with_RH_diff_key",
			dLeaseState:    rwh,
			dLeaseKey:      leaseA,
			newLeaseState:  rh,
			newLeaseKey:    leaseB,
			newShareAccess: shareAll,
			want:           disconnectedActionPurge,
		},
		{
			// purge-disconnected-rh-with-share-none-open: SHARE_NONE open on
			// a file with a disconnected RH must purge.
			name:           "purge_RH_with_share_none",
			dLeaseState:    rh,
			dLeaseKey:      leaseA,
			newLeaseState:  none,
			newLeaseKey:    [16]byte{},
			newShareAccess: shareNone,
			want:           disconnectedActionPurge,
		},
		{
			// Zero-key W-holder vs new lease (any key): zero key is "no key"
			// and never matches another, including another zero. Two opens
			// with empty lease keys cannot coexist on a W lease. This pins
			// the finding-#4 behaviour where oplock-V2 (no lease) reconnect
			// no longer accidentally hides behind the old reconnect-guard.
			name:           "purge_RWH_zero_key_vs_RH",
			dLeaseState:    rwh,
			dLeaseKey:      [16]byte{},
			newLeaseState:  rh,
			newLeaseKey:    leaseB,
			newShareAccess: shareAll,
			want:           disconnectedActionPurge,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := disconnectedConflictOnNewOpen(
				tc.dLeaseState, tc.dLeaseKey,
				tc.newLeaseState, tc.newLeaseKey,
				tc.newShareAccess,
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

	store := newMockDurableStore()
	_ = store.PutDurableHandle(context.Background(), &lock.PersistedDurableHandle{
		ID:             "disconnected-rwh",
		MetadataHandle: metaHandle,
		LeaseKey:       [16]byte{0x01},
		LeaseState:     rwh,
		DisconnectedAt: now.Add(-time.Second),
	})
	_ = store.PutDurableHandle(context.Background(), &lock.PersistedDurableHandle{
		ID:             "live",
		MetadataHandle: metaHandle,
		LeaseKey:       [16]byte{0x02},
		LeaseState:     rwh,
		// Not disconnected — must be skipped.
		DisconnectedAt: time.Time{},
	})

	h := &Handler{DurableStore: store}
	// New RH open with a different key → purges the disconnected RWH (W
	// must be broken; cascade strips H), leaves the live handle alone.
	got := h.purgeConflictingDisconnectedHandlesForOpen(
		context.Background(), metaHandle,
		rh, [16]byte{0x99}, shareAll,
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
				context.Background(), metaHandle, 0, [16]byte{}, 0,
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

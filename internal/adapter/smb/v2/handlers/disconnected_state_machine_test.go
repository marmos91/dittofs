package handlers

import "testing"

// TestDisconnectedConflictOnNewOpen exercises the MS-SMB2 §3.3.4.18 truth
// table for new CREATE opens against disconnected durable handles, mirroring
// the matrix asserted by smb2.durable-v2-open.{keep,purge}-disconnected-* in
// source4/torture/smb2/durable_v2_open.c.
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
	clientA := [16]byte{0xA1}
	clientB := [16]byte{0xB1}

	tests := []struct {
		name           string
		dLeaseState    uint32
		dLeaseKey      [16]byte
		dClientGUID    [16]byte
		newLeaseState  uint32
		newLeaseKey    [16]byte
		newClientGUID  [16]byte
		newShareAccess uint32
		want           disconnectedHandleAction
	}{
		{
			// keep-disconnected-rh-with-stat-open: stat open carries no lease
			// and default share-all. Disconnected RH must be preserved.
			name:           "keep_RH_with_stat",
			dLeaseState:    rh,
			dLeaseKey:      leaseA,
			dClientGUID:    clientA,
			newLeaseState:  none,
			newLeaseKey:    [16]byte{},
			newClientGUID:  clientB,
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
			dClientGUID:    clientA,
			newLeaseState:  none,
			newLeaseKey:    [16]byte{},
			newClientGUID:  clientB,
			newShareAccess: shareAll,
			want:           disconnectedActionPreserve,
		},
		{
			// keep-disconnected-rh-with-rh-open: two RH leases on different
			// keys can coexist — disconnected stays.
			name:           "keep_RH_with_RH_diff_key",
			dLeaseState:    rh,
			dLeaseKey:      leaseA,
			dClientGUID:    clientA,
			newLeaseState:  rh,
			newLeaseKey:    leaseB,
			newClientGUID:  clientB,
			newShareAccess: shareAll,
			want:           disconnectedActionPreserve,
		},
		{
			// keep-disconnected-rh-with-rwh-open: disconnected RH stays
			// even when the new open requests RWH — the new open just gets
			// downgraded to RH (W denied by the H-holder), no break needed.
			name:           "keep_RH_with_RWH_diff_key",
			dLeaseState:    rh,
			dLeaseKey:      leaseA,
			dClientGUID:    clientA,
			newLeaseState:  rwh,
			newLeaseKey:    leaseB,
			newClientGUID:  clientB,
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
			dClientGUID:    clientA,
			newLeaseState:  rwh,
			newLeaseKey:    leaseB,
			newClientGUID:  clientB,
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
			dClientGUID:    clientA,
			newLeaseState:  rh,
			newLeaseKey:    leaseB,
			newClientGUID:  clientB,
			newShareAccess: shareAll,
			want:           disconnectedActionPurge,
		},
		{
			// purge-disconnected-rh-with-share-none-open: SHARE_NONE open on
			// a file with a disconnected RH must purge.
			name:           "purge_RH_with_share_none",
			dLeaseState:    rh,
			dLeaseKey:      leaseA,
			dClientGUID:    clientA,
			newLeaseState:  none,
			newLeaseKey:    [16]byte{},
			newClientGUID:  clientB,
			newShareAccess: shareNone,
			want:           disconnectedActionPurge,
		},
		{
			// Reconnect path guard: the same (clientGuid, leaseKey) is the
			// reconnect path and must NEVER be purged here.
			name:           "reconnect_same_guid_and_key",
			dLeaseState:    rwh,
			dLeaseKey:      leaseA,
			dClientGUID:    clientA,
			newLeaseState:  rwh,
			newLeaseKey:    leaseA,
			newClientGUID:  clientA,
			newShareAccess: shareAll,
			want:           disconnectedActionPreserve,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := disconnectedConflictOnNewOpen(
				tc.dLeaseState, tc.dLeaseKey, tc.dClientGUID,
				tc.newLeaseState, tc.newLeaseKey, tc.newClientGUID,
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

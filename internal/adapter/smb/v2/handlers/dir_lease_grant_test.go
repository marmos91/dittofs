package handlers

import (
	"context"
	"encoding/binary"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/lease"
	"github.com/marmos91/dittofs/internal/adapter/smb/smbenc"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// Tests for Wave 1 C1 of issue #470 — directory-lease grant gate:
//
//   1. Traditional oplocks (LEVEL_II / EXCLUSIVE / BATCH) on a directory CREATE
//      must be clamped to OplockLevelNone in the response (MS-SMB2 §3.3.5.9,
//      Samba smbd_smb2_create_oplock_check). Covers smbtorture
//      smb2.dirlease.oplocks.
//
//   2. Directory lease grants must coerce RWH→RH (no W bit on directories).
//      Covers smbtorture smb2.dirlease.leases / smb2.dirlease.v2_request.
//
//   3. Directory lease grants must coerce RW→R. Same coverage.
//
//   4. V2 lease responses with LEASE_FLAG_PARENT_LEASE_KEY_SET MUST echo the
//      ParentLeaseKey unchanged (MS-SMB2 §2.2.14.2.10). Covers smbtorture
//      smb2.lease.v2_request_parent.

// TestClampDirectoryOplockLevel exercises the wire-OplockLevel coercion the
// directory CREATE response uses. Traditional oplocks (II/EXCLUSIVE/BATCH) must
// be clamped to NONE; OplockLevelLease (0xFF) and OplockLevelNone (0) must pass
// through unchanged. The `clearedSynthetic` flag must mirror the actual clamp:
// only set when a real downgrade happened.
func TestClampDirectoryOplockLevel(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		in          uint8
		wantLevel   uint8
		wantCleared bool
	}{
		{"None passes through", OplockLevelNone, OplockLevelNone, false},
		{"LEVEL_II clamped to None", OplockLevelII, OplockLevelNone, true},
		{"EXCLUSIVE clamped to None", OplockLevelExclusive, OplockLevelNone, true},
		{"BATCH clamped to None", OplockLevelBatch, OplockLevelNone, true},
		{"Lease passes through", OplockLevelLease, OplockLevelLease, false},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			gotLevel, gotCleared := clampDirectoryOplockLevel(tc.in)
			if gotLevel != tc.wantLevel {
				t.Errorf("clampDirectoryOplockLevel(0x%02x) level = 0x%02x, want 0x%02x",
					tc.in, gotLevel, tc.wantLevel)
			}
			if gotCleared != tc.wantCleared {
				t.Errorf("clampDirectoryOplockLevel(0x%02x) cleared = %v, want %v",
					tc.in, gotCleared, tc.wantCleared)
			}
		})
	}
}

// TestDirectoryLeaseRequest_RWH_DowngradesToRH exercises the lock-manager
// directory-lease coercion through ProcessLeaseCreateContext: a client requesting
// RWH on a directory must receive a response carrying RH (W bit stripped). Per
// MS-SMB2 §3.3.5.9 directories never carry the Write bit; see
// pkg/metadata/lock/oplock.go::IsValidDirectoryLeaseState and
// pkg/metadata/lock/leases.go::downgradeCandidates.
func TestDirectoryLeaseRequest_RWH_DowngradesToRH(t *testing.T) {
	t.Parallel()

	mgr := lock.NewManager()
	leaseMgr := lease.NewLeaseManager(&staticLockResolver{mgr: mgr}, nil)

	ctx := context.Background()
	leaseKey := [16]byte{0x10, 0x20, 0x30}
	fileHandle := lock.FileHandle("dir-handle-1")
	const sessionID = uint64(101)
	const clientID = "smb:101"

	// Client requests RWH (0x07) on a directory.
	requestedState := lock.LeaseStateRead | lock.LeaseStateWrite | lock.LeaseStateHandle
	data := encodeV2LeaseContext(leaseKey, requestedState, 0)

	resp, err := ProcessLeaseCreateContext(
		ctx, leaseMgr, data, fileHandle,
		sessionID, [16]byte{}, clientID, "share1",
		true, // isDirectory
	)
	if err != nil {
		t.Fatalf("ProcessLeaseCreateContext returned error: %v", err)
	}
	if resp == nil {
		t.Fatal("ProcessLeaseCreateContext returned nil response")
	}

	wantState := lock.LeaseStateRead | lock.LeaseStateHandle
	if resp.LeaseState != wantState {
		t.Errorf("directory lease state = 0x%x (%s), want RH 0x%x — W bit must be stripped on directories",
			resp.LeaseState, lock.LeaseStateToString(resp.LeaseState), wantState)
	}
	if resp.LeaseState&lock.LeaseStateWrite != 0 {
		t.Errorf("directory lease response carries Write bit (state=0x%x); MS-SMB2 forbids W on directories",
			resp.LeaseState)
	}
}

// TestDirectoryLeaseRequest_RW_DowngradesToR exercises the second downgrade
// case: a directory CREATE asking for RW (no Handle) must receive R only.
// Stripping W must not promote the response to RH — the request had no H bit.
func TestDirectoryLeaseRequest_RW_DowngradesToR(t *testing.T) {
	t.Parallel()

	mgr := lock.NewManager()
	leaseMgr := lease.NewLeaseManager(&staticLockResolver{mgr: mgr}, nil)

	ctx := context.Background()
	leaseKey := [16]byte{0x40, 0x50, 0x60}
	fileHandle := lock.FileHandle("dir-handle-2")
	const sessionID = uint64(102)
	const clientID = "smb:102"

	requestedState := lock.LeaseStateRead | lock.LeaseStateWrite
	data := encodeV2LeaseContext(leaseKey, requestedState, 0)

	resp, err := ProcessLeaseCreateContext(
		ctx, leaseMgr, data, fileHandle,
		sessionID, [16]byte{}, clientID, "share1",
		true,
	)
	if err != nil {
		t.Fatalf("ProcessLeaseCreateContext returned error: %v", err)
	}
	if resp == nil {
		t.Fatal("ProcessLeaseCreateContext returned nil response")
	}

	wantState := lock.LeaseStateRead
	if resp.LeaseState != wantState {
		t.Errorf("directory lease state = 0x%x (%s), want R 0x%x",
			resp.LeaseState, lock.LeaseStateToString(resp.LeaseState), wantState)
	}
	if resp.LeaseState&lock.LeaseStateWrite != 0 {
		t.Errorf("directory lease response carries Write bit (state=0x%x); MS-SMB2 forbids W on directories",
			resp.LeaseState)
	}
}

// TestDirectoryLeaseRequest_ParentLeaseKeyEcho covers the round-trip behaviour
// of LEASE_FLAG_PARENT_LEASE_KEY_SET. When a child CREATE carries the flag and
// a non-zero ParentLeaseKey, the V2 response MUST set the flag and echo the
// key bytes verbatim (MS-SMB2 §2.2.14.2.10). When the flag is clear, the
// response MUST clear the flag and zero the field on the wire, regardless of
// what the client put in the request payload — required by smbtorture
// smb2.lease.v2_flags_parentkey and the v2_request_parent matrix.
func TestDirectoryLeaseRequest_ParentLeaseKeyEcho(t *testing.T) {
	t.Parallel()

	mgr := lock.NewManager()
	leaseMgr := lease.NewLeaseManager(&staticLockResolver{mgr: mgr}, nil)

	ctx := context.Background()
	leaseKey := [16]byte{0x70, 0x80, 0x90}
	parentKey := [16]byte{0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF, 0x11, 0x22,
		0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0x00}
	fileHandle := lock.FileHandle("dir-handle-3")
	const sessionID = uint64(103)
	const clientID = "smb:103"

	t.Run("flag set echoes parent key", func(t *testing.T) {
		t.Parallel()

		data := encodeV2LeaseContextWithParent(
			leaseKey,
			lock.LeaseStateRead|lock.LeaseStateHandle,
			parentKey,
			smbenc.LeaseResponseFlagParentKeySet,
			0,
		)
		resp, err := ProcessLeaseCreateContext(
			ctx, leaseMgr, data, fileHandle,
			sessionID, [16]byte{}, clientID, "share1",
			true,
		)
		if err != nil {
			t.Fatalf("ProcessLeaseCreateContext returned error: %v", err)
		}
		if !resp.HasParent {
			t.Errorf("HasParent = false, want true when LEASE_FLAG_PARENT_LEASE_KEY_SET is on the request")
		}
		if resp.ParentLeaseKey != parentKey {
			t.Errorf("ParentLeaseKey = %x, want %x — V2 grant must echo client-supplied parent key",
				resp.ParentLeaseKey, parentKey)
		}

		// Wire-level check: encoded response carries the flag bit set and the
		// ParentLeaseKey bytes verbatim.
		wire := resp.Encode()
		if len(wire) != LeaseV2ContextSize {
			t.Fatalf("encoded response = %d bytes, want %d (V2)", len(wire), LeaseV2ContextSize)
		}
		gotFlags := binary.LittleEndian.Uint32(wire[20:24])
		if gotFlags&smbenc.LeaseResponseFlagParentKeySet == 0 {
			t.Errorf("wire flags = 0x%08x, missing LEASE_FLAG_PARENT_LEASE_KEY_SET (0x04)",
				gotFlags)
		}
		for i, b := range parentKey {
			if wire[32+i] != b {
				t.Errorf("ParentLeaseKey wire byte %d = 0x%02x, want 0x%02x",
					i, wire[32+i], b)
			}
		}
	})

	// Second key + lease to keep the manager state independent of the first
	// arm (same-key reentry would route through the upgrade path instead of a
	// fresh grant and skip the new-record code we want to test).
	t.Run("flag clear zeroes parent key on wire", func(t *testing.T) {
		t.Parallel()

		leaseKey2 := [16]byte{0xA0, 0xB0, 0xC0}
		fileHandle2 := lock.FileHandle("dir-handle-3b")
		data := encodeV2LeaseContextWithParent(
			leaseKey2,
			lock.LeaseStateRead|lock.LeaseStateHandle,
			parentKey, // payload carries a key but flag is OFF
			0,         // Flags = 0 → no parent linkage
			0,
		)
		resp, err := ProcessLeaseCreateContext(
			ctx, leaseMgr, data, fileHandle2,
			sessionID, [16]byte{}, clientID, "share1",
			true,
		)
		if err != nil {
			t.Fatalf("ProcessLeaseCreateContext returned error: %v", err)
		}
		if resp.HasParent {
			t.Errorf("HasParent = true, want false when LEASE_FLAG_PARENT_LEASE_KEY_SET is clear on the request")
		}
		if resp.ParentLeaseKey != ([16]byte{}) {
			t.Errorf("ParentLeaseKey = %x, want all zeros — payload key MUST NOT leak when flag is clear",
				resp.ParentLeaseKey)
		}

		wire := resp.Encode()
		gotFlags := binary.LittleEndian.Uint32(wire[20:24])
		if gotFlags&smbenc.LeaseResponseFlagParentKeySet != 0 {
			t.Errorf("wire flags = 0x%08x, must not carry LEASE_FLAG_PARENT_LEASE_KEY_SET when request didn't",
				gotFlags)
		}
		for i := 0; i < 16; i++ {
			if wire[32+i] != 0 {
				t.Errorf("wire ParentLeaseKey byte %d = 0x%02x, want 0x00 (flag clear ⇒ field zeroed)",
					i, wire[32+i])
			}
		}
	})
}

// encodeV2LeaseContextWithParent is a variant of encodeV2LeaseContext that
// also writes the Flags field and the 16-byte ParentLeaseKey, used by the
// parent-key echo round-trip tests.
func encodeV2LeaseContextWithParent(
	leaseKey [16]byte,
	state uint32,
	parentLeaseKey [16]byte,
	flags uint32,
	epoch uint16,
) []byte {
	buf := make([]byte, LeaseV2ContextSize)
	copy(buf[0:16], leaseKey[:])
	binary.LittleEndian.PutUint32(buf[16:20], state)
	binary.LittleEndian.PutUint32(buf[20:24], flags)
	copy(buf[32:48], parentLeaseKey[:])
	binary.LittleEndian.PutUint16(buf[48:50], epoch)
	return buf
}

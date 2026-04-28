package handlers

import (
	"context"
	"encoding/binary"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/lease"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// staticLockResolver is a test resolver that returns the same Manager for any
// share name. Used by lease-context tests that exercise ProcessLeaseCreateContext.
type staticLockResolver struct {
	mgr lock.LockManager
}

func (r *staticLockResolver) GetLockManagerForShare(_ string) lock.LockManager {
	return r.mgr
}

// encodeV2LeaseContext writes a 52-byte SMB2_CREATE_REQUEST_LEASE_V2 payload
// matching the wire layout in DecodeLeaseCreateContext (LeaseKey 16 / State 4 /
// Flags 4 / LeaseDuration 8 / ParentLeaseKey 16 / Epoch 2 / Reserved 2).
func encodeV2LeaseContext(leaseKey [16]byte, state uint32, epoch uint16) []byte {
	buf := make([]byte, LeaseV2ContextSize)
	copy(buf[0:16], leaseKey[:])
	binary.LittleEndian.PutUint32(buf[16:20], state)
	binary.LittleEndian.PutUint16(buf[48:50], epoch)
	return buf
}

// TestDecodeLeaseCreateContext tests parsing of SMB2_CREATE_REQUEST_LEASE_V2 contexts.
func TestDecodeLeaseCreateContext(t *testing.T) {
	tests := []struct {
		name           string
		data           []byte
		wantErr        bool
		wantLeaseKey   [16]byte
		wantLeaseState uint32
		wantEpoch      uint16
	}{
		{
			name:    "too short",
			data:    make([]byte, 10),
			wantErr: true,
		},
		{
			name: "V1 format (32 bytes)",
			data: func() []byte {
				buf := make([]byte, 32)
				// LeaseKey
				for i := 0; i < 16; i++ {
					buf[i] = byte(i)
				}
				// LeaseState = RWH (0x07)
				binary.LittleEndian.PutUint32(buf[16:20], lock.LeaseStateRead|lock.LeaseStateWrite|lock.LeaseStateHandle)
				return buf
			}(),
			wantErr:        false,
			wantLeaseKey:   [16]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15},
			wantLeaseState: lock.LeaseStateRead | lock.LeaseStateWrite | lock.LeaseStateHandle,
			wantEpoch:      0, // V1 has no epoch
		},
		{
			name: "V2 format (52 bytes)",
			data: func() []byte {
				buf := make([]byte, 52)
				// LeaseKey
				for i := 0; i < 16; i++ {
					buf[i] = byte(i + 10)
				}
				// LeaseState = RW (0x05)
				binary.LittleEndian.PutUint32(buf[16:20], lock.LeaseStateRead|lock.LeaseStateWrite)
				// Epoch = 5
				binary.LittleEndian.PutUint16(buf[48:50], 5)
				return buf
			}(),
			wantErr:        false,
			wantLeaseKey:   [16]byte{10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24, 25},
			wantLeaseState: lock.LeaseStateRead | lock.LeaseStateWrite,
			wantEpoch:      5,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, err := DecodeLeaseCreateContext(tt.data)
			if (err != nil) != tt.wantErr {
				t.Errorf("DecodeLeaseCreateContext() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if err != nil {
				return
			}

			if ctx.LeaseKey != tt.wantLeaseKey {
				t.Errorf("LeaseKey = %x, want %x", ctx.LeaseKey, tt.wantLeaseKey)
			}
			if ctx.LeaseState != tt.wantLeaseState {
				t.Errorf("LeaseState = 0x%x, want 0x%x", ctx.LeaseState, tt.wantLeaseState)
			}
			if ctx.Epoch != tt.wantEpoch {
				t.Errorf("Epoch = %d, want %d", ctx.Epoch, tt.wantEpoch)
			}
		})
	}
}

// TestEncodeLeaseResponseContext tests encoding of SMB2_CREATE_RESPONSE_LEASE_V2 contexts.
func TestEncodeLeaseResponseContext(t *testing.T) {
	leaseKey := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	leaseState := lock.LeaseStateRead | lock.LeaseStateWrite
	flags := uint32(0)
	epoch := uint16(3)

	encoded := EncodeLeaseResponseContext(leaseKey, leaseState, flags, epoch)

	if len(encoded) != LeaseV2ContextSize {
		t.Errorf("encoded length = %d, want %d", len(encoded), LeaseV2ContextSize)
	}

	// Verify fields
	var decodedKey [16]byte
	copy(decodedKey[:], encoded[0:16])
	if decodedKey != leaseKey {
		t.Errorf("decoded LeaseKey = %x, want %x", decodedKey, leaseKey)
	}

	decodedState := binary.LittleEndian.Uint32(encoded[16:20])
	if decodedState != leaseState {
		t.Errorf("decoded LeaseState = 0x%x, want 0x%x", decodedState, leaseState)
	}

	decodedFlags := binary.LittleEndian.Uint32(encoded[20:24])
	if decodedFlags != flags {
		t.Errorf("decoded Flags = 0x%x, want 0x%x", decodedFlags, flags)
	}

	decodedEpoch := binary.LittleEndian.Uint16(encoded[48:50])
	if decodedEpoch != epoch {
		t.Errorf("decoded Epoch = %d, want %d", decodedEpoch, epoch)
	}
}

// TestLeaseBreakNotificationEncode tests encoding of lease break notifications.
func TestLeaseBreakNotificationEncode(t *testing.T) {
	notification := &LeaseBreakNotification{
		NewEpoch:          2,
		Flags:             LeaseBreakFlagAckRequired,
		LeaseKey:          [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		CurrentLeaseState: lock.LeaseStateRead | lock.LeaseStateWrite | lock.LeaseStateHandle,
		NewLeaseState:     lock.LeaseStateRead | lock.LeaseStateHandle,
	}

	encoded := notification.Encode()

	if len(encoded) != LeaseBreakNotificationSize {
		t.Errorf("encoded length = %d, want %d", len(encoded), LeaseBreakNotificationSize)
	}

	// Verify StructureSize
	structSize := binary.LittleEndian.Uint16(encoded[0:2])
	if structSize != LeaseBreakNotificationSize {
		t.Errorf("StructureSize = %d, want %d", structSize, LeaseBreakNotificationSize)
	}

	// Verify NewEpoch
	newEpoch := binary.LittleEndian.Uint16(encoded[2:4])
	if newEpoch != notification.NewEpoch {
		t.Errorf("NewEpoch = %d, want %d", newEpoch, notification.NewEpoch)
	}

	// Verify Flags
	flags := binary.LittleEndian.Uint32(encoded[4:8])
	if flags != notification.Flags {
		t.Errorf("Flags = 0x%x, want 0x%x", flags, notification.Flags)
	}

	// Verify LeaseKey
	var decodedKey [16]byte
	copy(decodedKey[:], encoded[8:24])
	if decodedKey != notification.LeaseKey {
		t.Errorf("LeaseKey = %x, want %x", decodedKey, notification.LeaseKey)
	}

	// Verify CurrentLeaseState
	currentState := binary.LittleEndian.Uint32(encoded[24:28])
	if currentState != notification.CurrentLeaseState {
		t.Errorf("CurrentLeaseState = 0x%x, want 0x%x", currentState, notification.CurrentLeaseState)
	}

	// Verify NewLeaseState
	newState := binary.LittleEndian.Uint32(encoded[28:32])
	if newState != notification.NewLeaseState {
		t.Errorf("NewLeaseState = 0x%x, want 0x%x", newState, notification.NewLeaseState)
	}
}

// TestDecodeLeaseBreakAcknowledgment tests parsing of lease break acknowledgments.
func TestDecodeLeaseBreakAcknowledgment(t *testing.T) {
	tests := []struct {
		name           string
		data           []byte
		wantErr        bool
		wantLeaseKey   [16]byte
		wantLeaseState uint32
	}{
		{
			name:    "too short",
			data:    make([]byte, 20),
			wantErr: true,
		},
		{
			name: "invalid structure size",
			data: func() []byte {
				buf := make([]byte, 36)
				binary.LittleEndian.PutUint16(buf[0:2], 40) // Wrong size
				return buf
			}(),
			wantErr: true,
		},
		{
			name: "valid acknowledgment",
			data: func() []byte {
				buf := make([]byte, 36)
				binary.LittleEndian.PutUint16(buf[0:2], LeaseBreakAckSize)
				// LeaseKey at offset 8
				for i := 0; i < 16; i++ {
					buf[8+i] = byte(i + 1)
				}
				// LeaseState at offset 24
				binary.LittleEndian.PutUint32(buf[24:28], lock.LeaseStateRead|lock.LeaseStateHandle)
				return buf
			}(),
			wantErr:        false,
			wantLeaseKey:   [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
			wantLeaseState: lock.LeaseStateRead | lock.LeaseStateHandle,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ack, err := DecodeLeaseBreakAcknowledgment(tt.data)
			if (err != nil) != tt.wantErr {
				t.Errorf("DecodeLeaseBreakAcknowledgment() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if err != nil {
				return
			}

			if ack.LeaseKey != tt.wantLeaseKey {
				t.Errorf("LeaseKey = %x, want %x", ack.LeaseKey, tt.wantLeaseKey)
			}
			if ack.LeaseState != tt.wantLeaseState {
				t.Errorf("LeaseState = 0x%x, want 0x%x", ack.LeaseState, tt.wantLeaseState)
			}
		})
	}
}

// TestEncodeLeaseBreakResponse tests encoding of lease break response.
func TestEncodeLeaseBreakResponse(t *testing.T) {
	leaseKey := [16]byte{10, 20, 30, 40, 50, 60, 70, 80, 90, 100, 110, 120, 130, 140, 150, 160}
	leaseState := lock.LeaseStateRead

	encoded := EncodeLeaseBreakResponse(leaseKey, leaseState)

	if len(encoded) != LeaseBreakAckSize {
		t.Errorf("encoded length = %d, want %d", len(encoded), LeaseBreakAckSize)
	}

	// Verify StructureSize
	structSize := binary.LittleEndian.Uint16(encoded[0:2])
	if structSize != LeaseBreakAckSize {
		t.Errorf("StructureSize = %d, want %d", structSize, LeaseBreakAckSize)
	}

	// Verify LeaseKey
	var decodedKey [16]byte
	copy(decodedKey[:], encoded[8:24])
	if decodedKey != leaseKey {
		t.Errorf("LeaseKey = %x, want %x", decodedKey, leaseKey)
	}

	// Verify LeaseState
	decodedState := binary.LittleEndian.Uint32(encoded[24:28])
	if decodedState != leaseState {
		t.Errorf("LeaseState = 0x%x, want 0x%x", decodedState, leaseState)
	}
}

// TestFindCreateContext tests searching for create contexts by name.
func TestFindCreateContext(t *testing.T) {
	contexts := []CreateContext{
		{Name: "MxAc", Data: []byte{1, 2, 3}},
		{Name: "RqLs", Data: []byte{4, 5, 6, 7}},
		{Name: "QFid", Data: []byte{8, 9}},
	}

	// Find existing context
	found := FindCreateContext(contexts, "RqLs")
	if found == nil {
		t.Fatal("FindCreateContext failed to find RqLs")
	}
	if found.Name != "RqLs" {
		t.Errorf("found.Name = %s, want RqLs", found.Name)
	}

	// Find non-existing context
	notFound := FindCreateContext(contexts, "DH2Q")
	if notFound != nil {
		t.Error("FindCreateContext should return nil for non-existing context")
	}

	// Empty contexts list
	empty := FindCreateContext(nil, "RqLs")
	if empty != nil {
		t.Error("FindCreateContext should return nil for empty list")
	}
}

// TestLeaseResponseContextEncode tests LeaseResponseContext.Encode()
func TestLeaseResponseContextEncode(t *testing.T) {
	resp := &LeaseResponseContext{
		LeaseKey:       [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		LeaseState:     lock.LeaseStateRead | lock.LeaseStateWrite,
		Flags:          LeaseBreakFlagAckRequired,
		ParentLeaseKey: [16]byte{17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31, 32},
		Epoch:          7,
	}

	encoded := resp.Encode()

	if len(encoded) != LeaseV2ContextSize {
		t.Errorf("encoded length = %d, want %d", len(encoded), LeaseV2ContextSize)
	}

	// Verify fields through EncodeLeaseResponseContext
	expected := EncodeLeaseResponseContext(resp.LeaseKey, resp.LeaseState, resp.Flags, resp.Epoch)

	// Note: EncodeLeaseResponseContext doesn't include ParentLeaseKey
	// Compare common fields
	for i := 0; i < 20; i++ {
		if encoded[i] != expected[i] {
			t.Errorf("byte %d: encoded = %d, expected = %d", i, encoded[i], expected[i])
		}
	}
}

// TestProcessLeaseCreateContext_NoneProbeDoesNotAdvanceEpoch covers the
// smbtorture v2_rename failure cascade at lease.c:4299. After a CREATE granted
// LEASE1=RWH at epoch=0x4712, a subsequent same-key None-state probe MUST
// return that lease unchanged with epoch=0x4712. The pre-fix code advanced to
// 0x4713 because the V2 epoch-resolution branch unconditionally bumped to
// max(currentEpoch, requestedEpoch+1) for any non-None grant, including the
// no-op None-probe path. Per MS-SMB2 §2.2.14.2.11 the epoch advances ONLY on
// granted state changes — a None-probe is by definition not a state change.
func TestProcessLeaseCreateContext_NoneProbeDoesNotAdvanceEpoch(t *testing.T) {
	t.Parallel()

	mgr := lock.NewManager()
	leaseMgr := lease.NewLeaseManager(&staticLockResolver{mgr: mgr}, nil)

	ctx := context.Background()
	const shareName = "share1"
	leaseKey := [16]byte{0xAA, 0xBB, 0xCC}
	fileHandle := lock.FileHandle("file-handle-1")
	const sessionID = uint64(42)
	const clientID = "smb:42"
	const initialEpoch uint16 = 0x4711

	// First CREATE: grant RWH with requested epoch 0x4711 → response epoch 0x4712.
	initial := encodeV2LeaseContext(leaseKey, lock.LeaseStateRead|lock.LeaseStateWrite|lock.LeaseStateHandle, initialEpoch)
	resp1, err := ProcessLeaseCreateContext(ctx, leaseMgr, initial, fileHandle, sessionID, [16]byte{}, clientID, shareName, false)
	if err != nil {
		t.Fatalf("first CREATE returned error: %v", err)
	}
	if resp1.LeaseState != lock.LeaseStateRead|lock.LeaseStateWrite|lock.LeaseStateHandle {
		t.Fatalf("first CREATE granted state = 0x%x, want RWH (0x07)", resp1.LeaseState)
	}
	if resp1.Epoch != initialEpoch+1 {
		t.Fatalf("first CREATE response epoch = 0x%x, want 0x%x (req+1)", resp1.Epoch, initialEpoch+1)
	}
	grantedEpoch := resp1.Epoch

	// Same-key None-state probe at the granted epoch — must echo the lease's
	// current epoch verbatim (no state change → no advance).
	probe := encodeV2LeaseContext(leaseKey, lock.LeaseStateNone, grantedEpoch)
	resp2, err := ProcessLeaseCreateContext(ctx, leaseMgr, probe, fileHandle, sessionID, [16]byte{}, clientID, shareName, false)
	if err != nil {
		t.Fatalf("None-probe returned error: %v", err)
	}
	if resp2.LeaseState != lock.LeaseStateRead|lock.LeaseStateWrite|lock.LeaseStateHandle {
		t.Errorf("None-probe returned state = 0x%x, want RWH (0x07) — None-probe must echo existing state",
			resp2.LeaseState)
	}
	if resp2.Epoch != grantedEpoch {
		t.Errorf("None-probe response epoch = 0x%x, want 0x%x — None-probe is not a state change and MUST NOT advance epoch (smbtorture v2_rename lease.c:4299)",
			resp2.Epoch, grantedEpoch)
	}

	// Server-side tracking should also remain unchanged.
	_, persistedEpoch, found := mgr.GetLeaseState(ctx, leaseKey)
	if !found {
		t.Fatal("lease record disappeared after None-probe")
	}
	if persistedEpoch != grantedEpoch {
		t.Errorf("server-side epoch after None-probe = 0x%x, want 0x%x (no advance)",
			persistedEpoch, grantedEpoch)
	}
}

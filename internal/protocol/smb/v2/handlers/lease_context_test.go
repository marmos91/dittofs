package handlers

import (
	"encoding/binary"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

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
				// LeaseState = RW (0x03)
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

// TestParseLeaseCreateContextAlias tests that ParseLeaseCreateContext works as alias.
func TestParseLeaseCreateContextAlias(t *testing.T) {
	data := make([]byte, 52)
	for i := 0; i < 16; i++ {
		data[i] = byte(i)
	}
	binary.LittleEndian.PutUint32(data[16:20], lock.LeaseStateRead)

	// ParseLeaseCreateContext is an alias for DecodeLeaseCreateContext
	ctx, err := ParseLeaseCreateContext(data)
	if err != nil {
		t.Fatalf("ParseLeaseCreateContext error: %v", err)
	}

	if ctx.LeaseState != lock.LeaseStateRead {
		t.Errorf("LeaseState = 0x%x, want 0x%x", ctx.LeaseState, lock.LeaseStateRead)
	}
}

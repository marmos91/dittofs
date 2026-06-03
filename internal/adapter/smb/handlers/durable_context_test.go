package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

func TestDecodeDHnQRequest(t *testing.T) {
	tests := []struct {
		name    string
		data    []byte
		wantErr bool
	}{
		{
			name:    "valid 16 bytes",
			data:    make([]byte, 16),
			wantErr: false,
		},
		{
			name:    "too short",
			data:    make([]byte, 10),
			wantErr: true,
		},
		{
			name:    "extra bytes ok",
			data:    make([]byte, 32),
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := DecodeDHnQRequest(tt.data)
			if (err != nil) != tt.wantErr {
				t.Errorf("DecodeDHnQRequest() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestDecodeDHnCReconnect(t *testing.T) {
	tests := []struct {
		name       string
		data       []byte
		wantErr    bool
		wantFileID [16]byte
	}{
		{
			name:    "too short",
			data:    make([]byte, 10),
			wantErr: true,
		},
		{
			name: "valid 16 bytes with FileID",
			data: func() []byte {
				buf := make([]byte, 16)
				for i := 0; i < 16; i++ {
					buf[i] = byte(i + 1)
				}
				return buf
			}(),
			wantErr:    false,
			wantFileID: [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fileID, err := DecodeDHnCReconnect(tt.data)
			if (err != nil) != tt.wantErr {
				t.Errorf("DecodeDHnCReconnect() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if err != nil {
				return
			}
			if fileID != tt.wantFileID {
				t.Errorf("FileID = %x, want %x", fileID, tt.wantFileID)
			}
		})
	}
}

func TestDecodeDH2QRequest(t *testing.T) {
	tests := []struct {
		name           string
		data           []byte
		wantErr        bool
		wantTimeout    uint32
		wantFlags      uint32
		wantCreateGuid [16]byte
	}{
		{
			name:    "too short",
			data:    make([]byte, 20),
			wantErr: true,
		},
		{
			name: "valid 32 bytes",
			data: func() []byte {
				buf := make([]byte, 32)
				binary.LittleEndian.PutUint32(buf[0:4], 60000) // Timeout
				binary.LittleEndian.PutUint32(buf[4:8], 0)     // Flags
				// Reserved 8 bytes at offset 8
				for i := 0; i < 16; i++ {
					buf[16+i] = byte(i + 0xA0)
				}
				return buf
			}(),
			wantErr:        false,
			wantTimeout:    60000,
			wantFlags:      0,
			wantCreateGuid: [16]byte{0xA0, 0xA1, 0xA2, 0xA3, 0xA4, 0xA5, 0xA6, 0xA7, 0xA8, 0xA9, 0xAA, 0xAB, 0xAC, 0xAD, 0xAE, 0xAF},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			timeout, flags, createGuid, err := DecodeDH2QRequest(tt.data)
			if (err != nil) != tt.wantErr {
				t.Errorf("DecodeDH2QRequest() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if err != nil {
				return
			}
			if timeout != tt.wantTimeout {
				t.Errorf("Timeout = %d, want %d", timeout, tt.wantTimeout)
			}
			if flags != tt.wantFlags {
				t.Errorf("Flags = 0x%x, want 0x%x", flags, tt.wantFlags)
			}
			if createGuid != tt.wantCreateGuid {
				t.Errorf("CreateGuid = %x, want %x", createGuid, tt.wantCreateGuid)
			}
		})
	}
}

func TestDecodeDH2CReconnect(t *testing.T) {
	tests := []struct {
		name           string
		data           []byte
		wantErr        bool
		wantFileID     [16]byte
		wantCreateGuid [16]byte
		wantFlags      uint32
	}{
		{
			name:    "too short",
			data:    make([]byte, 30),
			wantErr: true,
		},
		{
			name: "valid 36 bytes",
			data: func() []byte {
				buf := make([]byte, 36)
				for i := 0; i < 16; i++ {
					buf[i] = byte(i + 1) // FileID
				}
				for i := 0; i < 16; i++ {
					buf[16+i] = byte(i + 0xB0) // CreateGuid
				}
				binary.LittleEndian.PutUint32(buf[32:36], 0) // Flags
				return buf
			}(),
			wantErr:        false,
			wantFileID:     [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
			wantCreateGuid: [16]byte{0xB0, 0xB1, 0xB2, 0xB3, 0xB4, 0xB5, 0xB6, 0xB7, 0xB8, 0xB9, 0xBA, 0xBB, 0xBC, 0xBD, 0xBE, 0xBF},
			wantFlags:      0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fileID, createGuid, flags, err := DecodeDH2CReconnect(tt.data)
			if (err != nil) != tt.wantErr {
				t.Errorf("DecodeDH2CReconnect() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if err != nil {
				return
			}
			if fileID != tt.wantFileID {
				t.Errorf("FileID = %x, want %x", fileID, tt.wantFileID)
			}
			if createGuid != tt.wantCreateGuid {
				t.Errorf("CreateGuid = %x, want %x", createGuid, tt.wantCreateGuid)
			}
			if flags != tt.wantFlags {
				t.Errorf("Flags = 0x%x, want 0x%x", flags, tt.wantFlags)
			}
		})
	}
}

func TestDecodeAppInstanceId(t *testing.T) {
	tests := []struct {
		name      string
		data      []byte
		wantErr   bool
		wantAppId [16]byte
	}{
		{
			name:    "too short",
			data:    make([]byte, 10),
			wantErr: true,
		},
		{
			name: "invalid structure size",
			data: func() []byte {
				buf := make([]byte, 20)
				binary.LittleEndian.PutUint16(buf[0:2], 40) // Wrong size
				return buf
			}(),
			wantErr: true,
		},
		{
			name: "valid 20 bytes",
			data: func() []byte {
				buf := make([]byte, 20)
				binary.LittleEndian.PutUint16(buf[0:2], 20) // StructureSize
				binary.LittleEndian.PutUint16(buf[2:4], 0)  // Reserved
				for i := 0; i < 16; i++ {
					buf[4+i] = byte(i + 0xC0)
				}
				return buf
			}(),
			wantErr:   false,
			wantAppId: [16]byte{0xC0, 0xC1, 0xC2, 0xC3, 0xC4, 0xC5, 0xC6, 0xC7, 0xC8, 0xC9, 0xCA, 0xCB, 0xCC, 0xCD, 0xCE, 0xCF},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			appId, err := DecodeAppInstanceId(tt.data)
			if (err != nil) != tt.wantErr {
				t.Errorf("DecodeAppInstanceId() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if err != nil {
				return
			}
			if appId != tt.wantAppId {
				t.Errorf("AppInstanceId = %x, want %x", appId, tt.wantAppId)
			}
		})
	}
}

func TestEncodeDHnQResponse(t *testing.T) {
	ctx := EncodeDHnQResponse()
	if ctx.Name != DurableHandleV1RequestTag {
		t.Errorf("Name = %s, want %s", ctx.Name, DurableHandleV1RequestTag)
	}
	if len(ctx.Data) != 8 {
		t.Errorf("Data length = %d, want 8", len(ctx.Data))
	}
	// All zeros
	for i, b := range ctx.Data {
		if b != 0 {
			t.Errorf("Data[%d] = 0x%x, want 0x00", i, b)
		}
	}
}

func TestEncodeDH2QResponse(t *testing.T) {
	ctx := EncodeDH2QResponse(45000, 0)
	if ctx.Name != DurableHandleV2RequestTag {
		t.Errorf("Name = %s, want %s", ctx.Name, DurableHandleV2RequestTag)
	}
	if len(ctx.Data) != 8 {
		t.Errorf("Data length = %d, want 8", len(ctx.Data))
	}
	timeout := binary.LittleEndian.Uint32(ctx.Data[0:4])
	if timeout != 45000 {
		t.Errorf("Timeout = %d, want 45000", timeout)
	}
	flags := binary.LittleEndian.Uint32(ctx.Data[4:8])
	if flags != 0 {
		t.Errorf("Flags = 0x%x, want 0x00", flags)
	}
}

func TestProcessDurableHandleContext_V1GrantWithBatchOplock(t *testing.T) {
	openFile := &OpenFile{
		FileID:      [16]byte{1, 2, 3},
		OplockLevel: OplockLevelBatch,
	}

	contexts := []CreateContext{
		{Name: DurableHandleV1RequestTag, Data: make([]byte, 16)},
	}

	resp := ProcessDurableHandleContext(contexts, openFile, DurableGrantOptions{ConfiguredTimeoutMs: 60000})
	if resp == nil {
		t.Fatal("Expected V1 grant response, got nil")
	}
	if resp.Name != DurableHandleV1RequestTag {
		t.Errorf("Response tag = %s, want %s", resp.Name, DurableHandleV1RequestTag)
	}
	if !openFile.IsDurable {
		t.Error("Expected openFile.IsDurable to be true")
	}
	if openFile.DurableTimeoutMs != 60000 {
		t.Errorf("DurableTimeoutMs = %d, want 60000", openFile.DurableTimeoutMs)
	}
}

func TestProcessDurableHandleContext_V1RejectWithoutBatchOplock(t *testing.T) {
	openFile := &OpenFile{
		FileID:      [16]byte{1, 2, 3},
		OplockLevel: OplockLevelII, // Not batch
	}

	contexts := []CreateContext{
		{Name: DurableHandleV1RequestTag, Data: make([]byte, 16)},
	}

	resp := ProcessDurableHandleContext(contexts, openFile, DurableGrantOptions{ConfiguredTimeoutMs: 60000})
	if resp != nil {
		t.Error("Expected nil response for non-batch oplock V1 request")
	}
	if openFile.IsDurable {
		t.Error("Expected openFile.IsDurable to be false")
	}
}

func TestProcessDurableHandleContext_V2GrantWithCreateGuid(t *testing.T) {
	// V2 durability MUST NOT be granted without Batch oplock or Handle lease
	// (MS-SMB2 §3.3.5.9.10). Use Batch to test the happy path.
	openFile := &OpenFile{
		FileID:      [16]byte{1, 2, 3},
		OplockLevel: OplockLevelBatch,
	}

	createGuid := [16]byte{0xA0, 0xA1, 0xA2, 0xA3, 0xA4, 0xA5, 0xA6, 0xA7, 0xA8, 0xA9, 0xAA, 0xAB, 0xAC, 0xAD, 0xAE, 0xAF}
	dh2qData := make([]byte, 32)
	binary.LittleEndian.PutUint32(dh2qData[0:4], 30000) // Timeout
	binary.LittleEndian.PutUint32(dh2qData[4:8], 0)     // Flags (no persistent)
	copy(dh2qData[16:32], createGuid[:])

	contexts := []CreateContext{
		{Name: DurableHandleV2RequestTag, Data: dh2qData},
	}

	resp := ProcessDurableHandleContext(contexts, openFile, DurableGrantOptions{ConfiguredTimeoutMs: 60000})
	if resp == nil {
		t.Fatal("Expected V2 grant response, got nil")
	}
	if resp.Name != DurableHandleV2RequestTag {
		t.Errorf("Response tag = %s, want %s", resp.Name, DurableHandleV2RequestTag)
	}
	if !openFile.IsDurable {
		t.Error("Expected openFile.IsDurable to be true")
	}
	if openFile.CreateGuid != createGuid {
		t.Errorf("CreateGuid = %x, want %x", openFile.CreateGuid, createGuid)
	}
	// Timeout should be min(requested=30000, configured=60000)
	if openFile.DurableTimeoutMs != 30000 {
		t.Errorf("DurableTimeoutMs = %d, want 30000", openFile.DurableTimeoutMs)
	}
}

func TestProcessDurableHandleContext_V2ZeroTimeoutUsesServerDefault(t *testing.T) {
	openFile := &OpenFile{
		FileID: [16]byte{1, 2, 3},
		// V2 needs Batch oplock or Handle lease — see §3.3.5.9.10.
		OplockLevel: OplockLevelBatch,
	}

	createGuid := [16]byte{0xA0, 0xA1, 0xA2, 0xA3, 0xA4, 0xA5, 0xA6, 0xA7, 0xA8, 0xA9, 0xAA, 0xAB, 0xAC, 0xAD, 0xAE, 0xAF}
	dh2qData := make([]byte, 32)
	binary.LittleEndian.PutUint32(dh2qData[0:4], 0) // Timeout = 0 -> use server default
	copy(dh2qData[16:32], createGuid[:])

	contexts := []CreateContext{
		{Name: DurableHandleV2RequestTag, Data: dh2qData},
	}

	resp := ProcessDurableHandleContext(contexts, openFile, DurableGrantOptions{ConfiguredTimeoutMs: 60000})
	if resp == nil {
		t.Fatal("Expected V2 grant response, got nil")
	}
	if openFile.DurableTimeoutMs != 60000 {
		t.Errorf("DurableTimeoutMs = %d, want 60000 (server default)", openFile.DurableTimeoutMs)
	}
}

// persistentDH2QData builds a 32-byte DH2Q request blob with the persistent
// flag set and the given CreateGuid.
func persistentDH2QData(createGuid [16]byte) []byte {
	dh2qData := make([]byte, 32)
	binary.LittleEndian.PutUint32(dh2qData[0:4], 60000)
	binary.LittleEndian.PutUint32(dh2qData[4:8], DH2FlagPersistent)
	copy(dh2qData[16:32], createGuid[:])
	return dh2qData
}

// TestProcessDurableHandleContext_PersistentNonCADegradesToDurable pins
// MS-SMB2 §3.3.5.9.10: a persistent request on a NON-CA share is not rejected —
// the persistent flag is ignored and the request degrades to a plain durable
// V2 grant (still Batch/Handle gated, persistent_open=false). This is what
// smbtorture persistent-open-{oplock,lease} expect on a non-CA share (the
// non-CA durable_open_vs_* tables: persistent=false, durable gated on
// Batch/Handle). On OplockLevelNone the degraded durable grant fails the gate
// and yields nil; on Batch it grants durable but NOT persistent.
func TestProcessDurableHandleContext_PersistentNonCADegradesToDurable(t *testing.T) {
	createGuid := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}

	// Non-CA + no Batch/Handle: degraded durable fails the gate → nil.
	t.Run("no batch, no handle, no CA -> nil", func(t *testing.T) {
		openFile := &OpenFile{FileID: [16]byte{1}, OplockLevel: OplockLevelNone}
		contexts := []CreateContext{{Name: DurableHandleV2RequestTag, Data: persistentDH2QData(createGuid)}}
		resp := ProcessDurableHandleContext(contexts, openFile, DurableGrantOptions{ConfiguredTimeoutMs: 60000})
		if resp != nil {
			t.Error("expected nil: persistent on non-CA share with no Batch/Handle must not grant")
		}
		if openFile.IsDurable || openFile.IsPersistent {
			t.Errorf("IsDurable=%v IsPersistent=%v, want both false", openFile.IsDurable, openFile.IsPersistent)
		}
	})

	// Non-CA + Batch: degrades to a plain durable grant, persistent_open=false.
	t.Run("batch oplock, no CA -> durable but not persistent", func(t *testing.T) {
		openFile := &OpenFile{FileID: [16]byte{1}, OplockLevel: OplockLevelBatch}
		contexts := []CreateContext{{Name: DurableHandleV2RequestTag, Data: persistentDH2QData(createGuid)}}
		resp := ProcessDurableHandleContext(contexts, openFile, DurableGrantOptions{ConfiguredTimeoutMs: 60000})
		if resp == nil {
			t.Fatal("expected durable grant (degraded from persistent) on Batch oplock")
		}
		if !openFile.IsDurable {
			t.Error("expected IsDurable=true")
		}
		if openFile.IsPersistent {
			t.Error("expected IsPersistent=false on a non-CA share")
		}
		// The DH2Q response flags must NOT echo PERSISTENT.
		flags := binary.LittleEndian.Uint32(resp.Data[4:8])
		if flags&DH2FlagPersistent != 0 {
			t.Errorf("response flags=%#x, must not set PERSISTENT on non-CA share", flags)
		}
	})
}

// TestProcessDurableHandleContext_PersistentCAGrant pins MS-SMB2 §3.3.5.9.10:
// on a continuous-availability share, a persistent request is granted
// UNCONDITIONALLY (no Batch/Handle gate) and the DH2Q response echoes the
// PERSISTENT flag. smbtorture persistent-open-{oplock,lease} CA tables assert
// durable==true && persistent==true for every oplock/lease/share-mode row,
// including the no-oplock / no-lease rows.
func TestProcessDurableHandleContext_PersistentCAGrant(t *testing.T) {
	createGuid := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}

	// Every oplock level — including None — must grant persistent on a CA share.
	for _, oplock := range []uint8{OplockLevelNone, OplockLevelII, OplockLevelExclusive, OplockLevelBatch, OplockLevelLease} {
		openFile := &OpenFile{FileID: [16]byte{1}, OplockLevel: oplock}
		contexts := []CreateContext{{Name: DurableHandleV2RequestTag, Data: persistentDH2QData(createGuid)}}
		resp := ProcessDurableHandleContext(contexts, openFile, DurableGrantOptions{
			ConfiguredTimeoutMs:    60000,
			ContinuousAvailability: true,
		})
		if resp == nil {
			t.Fatalf("oplock=%d: expected persistent grant on CA share, got nil", oplock)
		}
		if !openFile.IsDurable || !openFile.IsPersistent {
			t.Errorf("oplock=%d: IsDurable=%v IsPersistent=%v, want both true", oplock, openFile.IsDurable, openFile.IsPersistent)
		}
		flags := binary.LittleEndian.Uint32(resp.Data[4:8])
		if flags&DH2FlagPersistent == 0 {
			t.Errorf("oplock=%d: response flags=%#x, want PERSISTENT set", oplock, flags)
		}
	}
}

func TestProcessDurableHandleContext_V2PrecedenceOverV1(t *testing.T) {
	openFile := &OpenFile{
		FileID:      [16]byte{1, 2, 3},
		OplockLevel: OplockLevelBatch,
	}

	createGuid := [16]byte{0xA0, 0xA1, 0xA2, 0xA3, 0xA4, 0xA5, 0xA6, 0xA7, 0xA8, 0xA9, 0xAA, 0xAB, 0xAC, 0xAD, 0xAE, 0xAF}
	dh2qData := make([]byte, 32)
	binary.LittleEndian.PutUint32(dh2qData[0:4], 45000)
	copy(dh2qData[16:32], createGuid[:])

	contexts := []CreateContext{
		{Name: DurableHandleV1RequestTag, Data: make([]byte, 16)},
		{Name: DurableHandleV2RequestTag, Data: dh2qData},
	}

	resp := ProcessDurableHandleContext(contexts, openFile, DurableGrantOptions{ConfiguredTimeoutMs: 60000})
	if resp == nil {
		t.Fatal("Expected V2 response when both V1 and V2 present")
	}
	// V2 takes precedence: response tag should be DH2Q
	if resp.Name != DurableHandleV2RequestTag {
		t.Errorf("Response tag = %s, want %s (V2 precedence)", resp.Name, DurableHandleV2RequestTag)
	}
	if openFile.CreateGuid != createGuid {
		t.Errorf("CreateGuid should be set by V2 processing")
	}
}

func TestProcessDurableHandleContext_NeitherPresent(t *testing.T) {
	openFile := &OpenFile{
		FileID:      [16]byte{1, 2, 3},
		OplockLevel: OplockLevelBatch,
	}

	contexts := []CreateContext{
		{Name: "MxAc", Data: make([]byte, 8)},
	}

	resp := ProcessDurableHandleContext(contexts, openFile, DurableGrantOptions{ConfiguredTimeoutMs: 60000})
	if resp != nil {
		t.Error("Expected nil when no durable contexts present")
	}
}

func makeSessionKeyHash(key string) [32]byte {
	return sha256.Sum256([]byte(key))
}

func TestProcessDurableReconnectContext_V1Success(t *testing.T) {
	store := newMockDurableStore()
	ctx := context.Background()

	// Persisted FileIDs always carry zeroed volatile half — see
	// `buildPersistedDurableHandle`. Tests must match this to model the
	// real store contents.
	fileID := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 0, 0, 0, 0, 0, 0, 0, 0}
	keyHash := makeSessionKeyHash("session-key-1")

	// Persist a V1 durable handle
	_ = store.PutDurableHandle(ctx, &lock.PersistedDurableHandle{
		ID:             "dh-001",
		FileID:         fileID,
		Path:           "test.txt",
		ShareName:      "/share1",
		DesiredAccess:  0x12019F,
		ShareAccess:    0x07,
		CreateOptions:  0,
		MetadataHandle: []byte{0xDE, 0xAD},
		PayloadID:      "payload-001",
		OplockLevel:    OplockLevelBatch,
		Username:       "alice",
		SessionKeyHash: keyHash,
		IsV2:           false,
		CreatedAt:      time.Now().Add(-5 * time.Minute),
		DisconnectedAt: time.Now().Add(-10 * time.Second),
		TimeoutMs:      60000,
	})

	// Build V1 reconnect context
	dhnCData := make([]byte, 16)
	copy(dhnCData[:], fileID[:])

	contexts := []CreateContext{
		{Name: DurableHandleV1ReconnectTag, Data: dhnCData},
	}

	restored, status, err := ProcessDurableReconnectContext(
		context.Background(), store, nil, contexts, 999, "alice", keyHash, "/share1", "test.txt", [16]byte{},
	)
	if err != nil {
		t.Fatalf("ProcessDurableReconnectContext error: %v", err)
	}
	if status != types.StatusSuccess {
		t.Fatalf("Expected STATUS_SUCCESS, got %s", status)
	}
	if restored == nil {
		t.Fatal("Expected restored ReconnectResult, got nil")
	}
	if restored.OpenFile.Path != "test.txt" {
		t.Errorf("Path = %s, want test.txt", restored.OpenFile.Path)
	}
	if restored.OpenFile.DesiredAccess != 0x12019F {
		t.Errorf("DesiredAccess = 0x%x, want 0x12019F", restored.OpenFile.DesiredAccess)
	}
	if restored.OpenFile.OplockLevel != OplockLevelBatch {
		t.Errorf("OplockLevel = %d, want %d", restored.OpenFile.OplockLevel, OplockLevelBatch)
	}
	if restored.IsV2 {
		t.Error("Expected IsV2=false for V1 reconnect")
	}

	// Verify handle was deleted from store
	h, _ := store.GetDurableHandle(ctx, "dh-001")
	if h != nil {
		t.Error("Expected persisted handle to be deleted after reconnect")
	}
}

// encodeV1LeaseRequestContext builds a minimal 32-byte SMB2_CREATE_REQUEST_LEASE
// (V1) context carrying leaseKey + leaseState.
func encodeV1LeaseRequestContext(leaseKey [16]byte, leaseState uint32) []byte {
	data := make([]byte, LeaseV1ContextSize)
	copy(data[:16], leaseKey[:])
	binary.LittleEndian.PutUint32(data[16:20], leaseState)
	return data
}

// TestProcessDurableReconnectContext_V1LeaseClientGuidMismatch verifies that a
// V1 (DHnC) lease-backed reconnect from a different ClientGuid than the one
// that established the durable open fails OBJECT_NAME_NOT_FOUND, mirroring
// Samba per-(ClientGuid, LeaseKey) lease scoping. smbtorture
// smb2.durable-open.reopen1a-lease.
func TestProcessDurableReconnectContext_V1LeaseClientGuidMismatch(t *testing.T) {
	store := newMockDurableStore()
	ctx := context.Background()

	fileID := [16]byte{9, 8, 7, 6, 5, 4, 3, 2, 0, 0, 0, 0, 0, 0, 0, 0}
	leaseKey := [16]byte{0xAA, 0xBB, 0xCC, 0xDD}
	origClientGUID := [16]byte{0x11, 0x22, 0x33, 0x44}
	keyHash := makeSessionKeyHash("session-key-lease")

	_ = store.PutDurableHandle(ctx, &lock.PersistedDurableHandle{
		ID:             "dh-lease-001",
		FileID:         fileID,
		Path:           "leased.txt",
		ShareName:      "/share1",
		MetadataHandle: []byte{0xDE, 0xAD},
		OplockLevel:    OplockLevelLease,
		LeaseKey:       leaseKey,
		LeaseState:     uint32(lock.LeaseStateRead | lock.LeaseStateWrite | lock.LeaseStateHandle),
		ClientGUID:     origClientGUID,
		Username:       "alice",
		SessionKeyHash: keyHash,
		CreatedAt:      time.Now().Add(-5 * time.Minute),
		DisconnectedAt: time.Now().Add(-10 * time.Second),
		TimeoutMs:      60000,
	})

	dhnCData := make([]byte, 16)
	copy(dhnCData[:], fileID[:])
	contexts := []CreateContext{
		{Name: DurableHandleV1ReconnectTag, Data: dhnCData},
		{Name: LeaseContextTagRequest, Data: encodeV1LeaseRequestContext(leaseKey, 0)},
	}

	// Reconnect with a DIFFERENT ClientGuid → OBJECT_NAME_NOT_FOUND.
	wrongClientGUID := [16]byte{0x99, 0x88, 0x77, 0x66}
	_, status, err := ProcessDurableReconnectContext(
		ctx, store, nil, contexts, 999, "alice", keyHash, "/share1", "leased.txt", wrongClientGUID,
	)
	if err != nil {
		t.Fatalf("ProcessDurableReconnectContext error: %v", err)
	}
	if status != types.StatusObjectNameNotFound {
		t.Fatalf("wrong ClientGuid: expected OBJECT_NAME_NOT_FOUND, got %s", status)
	}

	// Handle must survive a failed (non-destructive) reconnect attempt.
	if h, _ := store.GetDurableHandle(ctx, "dh-lease-001"); h == nil {
		t.Fatal("handle should remain after failed ClientGuid-mismatch reconnect")
	}

	// Reconnect with the ORIGINAL ClientGuid → success.
	restored, status, err := ProcessDurableReconnectContext(
		ctx, store, nil, contexts, 999, "alice", keyHash, "/share1", "leased.txt", origClientGUID,
	)
	if err != nil {
		t.Fatalf("ProcessDurableReconnectContext (correct GUID) error: %v", err)
	}
	if status != types.StatusSuccess {
		t.Fatalf("correct ClientGuid: expected STATUS_SUCCESS, got %s", status)
	}
	if restored == nil || restored.OpenFile.LeaseKey != leaseKey {
		t.Fatal("expected restored lease-backed open with matching lease key")
	}
}

func TestProcessDurableReconnectContext_V2Success(t *testing.T) {
	store := newMockDurableStore()
	ctx := context.Background()

	// Persisted FileIDs always carry zeroed volatile half — see
	// `buildPersistedDurableHandle`. Tests must match this to model the
	// real store contents.
	fileID := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 0, 0, 0, 0, 0, 0, 0, 0}
	createGuid := [16]byte{0xA0, 0xA1, 0xA2, 0xA3, 0xA4, 0xA5, 0xA6, 0xA7, 0xA8, 0xA9, 0xAA, 0xAB, 0xAC, 0xAD, 0xAE, 0xAF}
	keyHash := makeSessionKeyHash("session-key-2")

	_ = store.PutDurableHandle(ctx, &lock.PersistedDurableHandle{
		ID:             "dh-002",
		FileID:         fileID,
		Path:           "report.docx",
		ShareName:      "/share1",
		DesiredAccess:  0x120089,
		ShareAccess:    0x01,
		CreateOptions:  0,
		MetadataHandle: []byte{0xBE, 0xEF},
		PayloadID:      "payload-002",
		OplockLevel:    OplockLevelLease,
		CreateGuid:     createGuid,
		Username:       "bob",
		SessionKeyHash: keyHash,
		IsV2:           true,
		CreatedAt:      time.Now().Add(-5 * time.Minute),
		DisconnectedAt: time.Now().Add(-10 * time.Second),
		TimeoutMs:      60000,
	})

	// Build V2 reconnect context
	dh2cData := make([]byte, 36)
	copy(dh2cData[0:16], fileID[:])
	copy(dh2cData[16:32], createGuid[:])
	binary.LittleEndian.PutUint32(dh2cData[32:36], 0) // Flags

	contexts := []CreateContext{
		{Name: DurableHandleV2ReconnectTag, Data: dh2cData},
	}

	restored, status, err := ProcessDurableReconnectContext(
		context.Background(), store, nil, contexts, 999, "bob", keyHash, "/share1", "report.docx", [16]byte{},
	)
	if err != nil {
		t.Fatalf("ProcessDurableReconnectContext error: %v", err)
	}
	if status != types.StatusSuccess {
		t.Fatalf("Expected STATUS_SUCCESS, got %s", status)
	}
	if restored == nil {
		t.Fatal("Expected restored ReconnectResult, got nil")
	}
	if restored.OpenFile.Path != "report.docx" {
		t.Errorf("Path = %s, want report.docx", restored.OpenFile.Path)
	}
	if !restored.IsV2 {
		t.Error("Expected IsV2=true for V2 reconnect")
	}
	// The restored OpenFile must carry the original CreateGuid so a chained
	// disconnect→reconnect→disconnect cycle keeps the handle's V2 identity:
	// without it the next buildPersistedDurableHandle records IsV2=false with a
	// zero guid and the following DH2C by-CreateGuid lookup fails (the
	// reopen2-family multi-cycle regression this fix addresses).
	if restored.OpenFile.CreateGuid != createGuid {
		t.Errorf("restored CreateGuid = %x, want %x", restored.OpenFile.CreateGuid, createGuid)
	}

	// Verify handle was deleted from store
	h, _ := store.GetDurableHandle(ctx, "dh-002")
	if h != nil {
		t.Error("Expected persisted handle to be deleted after reconnect")
	}
}

func TestProcessDurableReconnectContext_HandleNotFound(t *testing.T) {
	store := newMockDurableStore()

	// No handles in store -- try V1 reconnect
	dhnCData := make([]byte, 16)
	for i := 0; i < 16; i++ {
		dhnCData[i] = byte(i + 1)
	}

	contexts := []CreateContext{
		{Name: DurableHandleV1ReconnectTag, Data: dhnCData},
	}

	_, status, _ := ProcessDurableReconnectContext(
		context.Background(), store, nil, contexts, 999, "alice", makeSessionKeyHash("key"), "/share1", "test.txt", [16]byte{},
	)
	if status != types.StatusObjectNameNotFound {
		t.Errorf("Expected STATUS_OBJECT_NAME_NOT_FOUND, got %s", status)
	}
}

func TestProcessDurableReconnectContext_UsernameMismatch(t *testing.T) {
	store := newMockDurableStore()
	ctx := context.Background()

	// Persisted FileIDs always carry zeroed volatile half — see
	// `buildPersistedDurableHandle`. Tests must match this to model the
	// real store contents.
	fileID := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 0, 0, 0, 0, 0, 0, 0, 0}
	keyHash := makeSessionKeyHash("session-key-1")

	_ = store.PutDurableHandle(ctx, &lock.PersistedDurableHandle{
		ID:             "dh-003",
		FileID:         fileID,
		Path:           "test.txt",
		ShareName:      "/share1",
		DesiredAccess:  0x12019F,
		ShareAccess:    0x07,
		MetadataHandle: []byte{0xDE, 0xAD},
		Username:       "alice",
		SessionKeyHash: keyHash,
		IsV2:           false,
		DisconnectedAt: time.Now().Add(-10 * time.Second),
		TimeoutMs:      60000,
	})

	dhnCData := make([]byte, 16)
	copy(dhnCData[:], fileID[:])

	contexts := []CreateContext{
		{Name: DurableHandleV1ReconnectTag, Data: dhnCData},
	}

	_, status, _ := ProcessDurableReconnectContext(
		context.Background(), store, nil, contexts, 999, "eve", keyHash, "/share1", "test.txt", [16]byte{},
	)
	if status != types.StatusAccessDenied {
		t.Errorf("Expected STATUS_ACCESS_DENIED for username mismatch, got %s", status)
	}
}

func TestProcessDurableReconnectContext_SessionKeyMismatchAllowed(t *testing.T) {
	// Per MS-SMB2 3.3.5.9.7/12, durable reconnect validates the user identity
	// (username), not the session key. With NTLM KEY_EXCH, each session gets a
	// random ExportedSessionKey, so the session key will always differ between
	// original and reconnect sessions. This test verifies that a session key
	// mismatch does NOT cause ACCESS_DENIED when the username matches.
	store := newMockDurableStore()
	ctx := context.Background()

	// Persisted FileIDs always carry zeroed volatile half — see
	// `buildPersistedDurableHandle`. Tests must match this to model the
	// real store contents.
	fileID := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 0, 0, 0, 0, 0, 0, 0, 0}
	originalKeyHash := makeSessionKeyHash("original-key")
	differentKeyHash := makeSessionKeyHash("different-key")

	_ = store.PutDurableHandle(ctx, &lock.PersistedDurableHandle{
		ID:             "dh-004",
		FileID:         fileID,
		Path:           "test.txt",
		ShareName:      "/share1",
		DesiredAccess:  0x12019F,
		ShareAccess:    0x07,
		MetadataHandle: []byte{0xDE, 0xAD},
		Username:       "alice",
		SessionKeyHash: originalKeyHash,
		IsV2:           false,
		DisconnectedAt: time.Now().Add(-10 * time.Second),
		TimeoutMs:      60000,
	})

	dhnCData := make([]byte, 16)
	copy(dhnCData[:], fileID[:])

	contexts := []CreateContext{
		{Name: DurableHandleV1ReconnectTag, Data: dhnCData},
	}

	_, status, _ := ProcessDurableReconnectContext(
		context.Background(), store, nil, contexts, 999, "alice", differentKeyHash, "/share1", "test.txt", [16]byte{},
	)
	if status != types.StatusSuccess {
		t.Errorf("Expected STATUS_SUCCESS for session key mismatch with matching username, got %s", status)
	}
}

func TestProcessDurableReconnectContext_ShareNameMismatch(t *testing.T) {
	store := newMockDurableStore()
	ctx := context.Background()

	// Persisted FileIDs always carry zeroed volatile half — see
	// `buildPersistedDurableHandle`. Tests must match this to model the
	// real store contents.
	fileID := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 0, 0, 0, 0, 0, 0, 0, 0}
	keyHash := makeSessionKeyHash("key")

	_ = store.PutDurableHandle(ctx, &lock.PersistedDurableHandle{
		ID:             "dh-005",
		FileID:         fileID,
		Path:           "test.txt",
		ShareName:      "/share1",
		DesiredAccess:  0x12019F,
		ShareAccess:    0x07,
		MetadataHandle: []byte{0xDE, 0xAD},
		Username:       "alice",
		SessionKeyHash: keyHash,
		IsV2:           false,
		DisconnectedAt: time.Now().Add(-10 * time.Second),
		TimeoutMs:      60000,
	})

	dhnCData := make([]byte, 16)
	copy(dhnCData[:], fileID[:])

	contexts := []CreateContext{
		{Name: DurableHandleV1ReconnectTag, Data: dhnCData},
	}

	_, status, _ := ProcessDurableReconnectContext(
		context.Background(), store, nil, contexts, 999, "alice", keyHash, "/different-share", "test.txt", [16]byte{},
	)
	if status != types.StatusObjectNameNotFound {
		t.Errorf("Expected STATUS_OBJECT_NAME_NOT_FOUND for share mismatch, got %s", status)
	}
}

// TestProcessDurableReconnectContext_V2OplockJunkFnameIgnored locks in the
// reopen2 fix: a non-lease (oplock-backed) V2 (DH2C) reconnect MUST ignore the
// filename in the reconnect CREATE and succeed, mirroring Samba
// smbd_smb2_create_durable_lease_check (returns NT_STATUS_OK early when
// lease_ptr==NULL && oplock_type!=LEASE_OPLOCK, never path-comparing). The
// CreateGuid is the sole identifier. smbtorture smb2.durable-v2-open.reopen2
// (durable_v2_open.c:1075) replays a junk fname and expects NT_STATUS_OK.
func TestProcessDurableReconnectContext_V2OplockJunkFnameIgnored(t *testing.T) {
	store := newMockDurableStore()
	ctx := context.Background()

	createGuid := [16]byte{0xA0, 0xA1, 0xA2, 0xA3, 0xA4, 0xA5, 0xA6, 0xA7, 0xA8, 0xA9, 0xAA, 0xAB, 0xAC, 0xAD, 0xAE, 0xAF}
	// Persisted FileIDs always carry zeroed volatile half — see
	// `buildPersistedDurableHandle`. Tests must match this to model the
	// real store contents.
	fileID := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 0, 0, 0, 0, 0, 0, 0, 0}
	keyHash := makeSessionKeyHash("key")

	_ = store.PutDurableHandle(ctx, &lock.PersistedDurableHandle{
		ID:             "dh-006",
		FileID:         fileID,
		Path:           "test.txt",
		ShareName:      "/share1",
		DesiredAccess:  0x12019F,
		GrantedAccess:  0x12019F,
		ShareAccess:    0x07,
		MetadataHandle: []byte{0xDE, 0xAD},
		Username:       "alice",
		SessionKeyHash: keyHash,
		CreateGuid:     createGuid,
		OplockLevel:    OplockLevelBatch, // oplock-backed (non-lease)
		IsV2:           true,
		DisconnectedAt: time.Now().Add(-10 * time.Second),
		TimeoutMs:      60000,
	})

	dh2cData := make([]byte, 36)
	copy(dh2cData[0:16], fileID[:])
	copy(dh2cData[16:32], createGuid[:])

	contexts := []CreateContext{
		{Name: DurableHandleV2ReconnectTag, Data: dh2cData},
	}

	// Junk fname "__non_existing_fname__" + no lease ctx → must still succeed.
	restored, status, err := ProcessDurableReconnectContext(
		context.Background(), store, nil, contexts, 999, "alice", keyHash, "/share1", "__non_existing_fname__", [16]byte{},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != types.StatusSuccess {
		t.Fatalf("Expected STATUS_SUCCESS (non-lease V2 ignores fname), got %s", status)
	}
	if restored == nil || restored.OpenFile.Path != "test.txt" {
		t.Fatalf("expected restored open with original path test.txt, got %+v", restored)
	}
	// Reconnect restores the ORIGINAL granted access, never the request's.
	if restored.OpenFile.GrantedAccess != 0x12019F {
		t.Errorf("GrantedAccess = 0x%x, want original 0x12019F", restored.OpenFile.GrantedAccess)
	}
}

// TestProcessDurableReconnectContext_V2LeasePathMismatch preserves the negative
// ladder: a LEASE-backed V2 reconnect WITH a matching lease key but a WRONG
// filename is the final negative rung and MUST return INVALID_PARAMETER
// (Samba smbd_smb2_create_durable_lease_check strequal(base_name) compare,
// reached only for lease opens). smbtorture reopen2-lease-v2 wrong-fname rung.
func TestProcessDurableReconnectContext_V2LeasePathMismatch(t *testing.T) {
	store := newMockDurableStore()
	ctx := context.Background()

	createGuid := [16]byte{0xB0, 0xB1, 0xB2, 0xB3, 0xB4, 0xB5, 0xB6, 0xB7, 0xB8, 0xB9, 0xBA, 0xBB, 0xBC, 0xBD, 0xBE, 0xBF}
	fileID := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 0, 0, 0, 0, 0, 0, 0, 0}
	leaseKey := [16]byte{0xCC, 0xDD, 0xEE, 0xFF}
	keyHash := makeSessionKeyHash("key")

	_ = store.PutDurableHandle(ctx, &lock.PersistedDurableHandle{
		ID:             "dh-006-lease",
		FileID:         fileID,
		Path:           "test.txt",
		ShareName:      "/share1",
		MetadataHandle: []byte{0xDE, 0xAD},
		Username:       "alice",
		SessionKeyHash: keyHash,
		CreateGuid:     createGuid,
		OplockLevel:    OplockLevelLease,
		LeaseKey:       leaseKey,
		LeaseState:     uint32(lock.LeaseStateRead | lock.LeaseStateHandle),
		IsV2:           true,
		DisconnectedAt: time.Now().Add(-10 * time.Second),
		TimeoutMs:      60000,
	})

	dh2cData := make([]byte, 36)
	copy(dh2cData[0:16], fileID[:])
	copy(dh2cData[16:32], createGuid[:])

	contexts := []CreateContext{
		{Name: DurableHandleV2ReconnectTag, Data: dh2cData},
		{Name: LeaseContextTagRequest, Data: encodeV1LeaseRequestContext(leaseKey, 0)},
	}

	_, status, _ := ProcessDurableReconnectContext(
		context.Background(), store, nil, contexts, 999, "alice", keyHash, "/share1", "other.txt", [16]byte{},
	)
	if status != types.StatusInvalidParameter {
		t.Errorf("Expected STATUS_INVALID_PARAMETER for lease-backed path mismatch, got %s", status)
	}
}

func TestProcessDurableReconnectContext_V1ConflictingV2Tag(t *testing.T) {
	store := newMockDurableStore()
	ctx := context.Background()

	// Persisted FileIDs always carry zeroed volatile half — see
	// `buildPersistedDurableHandle`. Tests must match this to model the
	// real store contents.
	fileID := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 0, 0, 0, 0, 0, 0, 0, 0}
	keyHash := makeSessionKeyHash("key")

	_ = store.PutDurableHandle(ctx, &lock.PersistedDurableHandle{
		ID:             "dh-007",
		FileID:         fileID,
		Path:           "test.txt",
		ShareName:      "/share1",
		DesiredAccess:  0x12019F,
		ShareAccess:    0x07,
		MetadataHandle: []byte{0xDE, 0xAD},
		Username:       "alice",
		SessionKeyHash: keyHash,
		IsV2:           false,
		DisconnectedAt: time.Now().Add(-10 * time.Second),
		TimeoutMs:      60000,
	})

	// V1 reconnect (DHnC) with conflicting DH2Q also present
	dhnCData := make([]byte, 16)
	copy(dhnCData[:], fileID[:])

	contexts := []CreateContext{
		{Name: DurableHandleV1ReconnectTag, Data: dhnCData},
		{Name: DurableHandleV2RequestTag, Data: make([]byte, 32)}, // Conflicting V2
	}

	_, status, _ := ProcessDurableReconnectContext(
		context.Background(), store, nil, contexts, 999, "alice", keyHash, "/share1", "test.txt", [16]byte{},
	)
	if status != types.StatusInvalidParameter {
		t.Errorf("Expected STATUS_INVALID_PARAMETER for conflicting V2 tag with V1 reconnect, got %s", status)
	}
}

func TestOpenFile_DurableFields(t *testing.T) {
	of := &OpenFile{
		FileID:           [16]byte{1},
		IsDurable:        true,
		CreateGuid:       [16]byte{2, 3, 4},
		AppInstanceId:    [16]byte{5, 6, 7},
		DurableTimeoutMs: 45000,
	}

	if !of.IsDurable {
		t.Error("Expected IsDurable to be true")
	}
	if of.CreateGuid != [16]byte{2, 3, 4} {
		t.Error("CreateGuid mismatch")
	}
	if of.AppInstanceId != [16]byte{5, 6, 7} {
		t.Error("AppInstanceId mismatch")
	}
	if of.DurableTimeoutMs != 45000 {
		t.Error("DurableTimeoutMs mismatch")
	}
}

// TestAppInstanceIdTag_MatchesWireName pins AppInstanceIdTag to the exact
// 16-byte SMB2_CREATE_APP_INSTANCE_ID create-context name (MS-SMB2 2.2.13.2.8,
// Samba SMB2_CREATE_TAG_APP_INSTANCE_ID). This is a GUID, not a 4-byte ASCII
// tag. A prior truncated value never matched the on-wire name, so the
// AppInstanceId failover never fired and smb2.durable-v2-open.app-instance
// failed (break_info.count == 1, expected 0). The other ProcessAppInstanceId
// tests build their context from AppInstanceIdTag itself, so they cannot catch
// a wrong constant — this guard asserts the literal wire bytes directly.
func TestAppInstanceIdTag_MatchesWireName(t *testing.T) {
	want := []byte{
		0x45, 0xBC, 0xA6, 0x6A, 0xEF, 0xA7, 0xF7, 0x4A,
		0x90, 0x08, 0xFA, 0x46, 0x2E, 0x14, 0x4D, 0x74,
	}
	if AppInstanceIdTag != string(want) {
		t.Errorf("AppInstanceIdTag = %x, want %x", AppInstanceIdTag, want)
	}
	if len(AppInstanceIdTag) != 16 {
		t.Errorf("AppInstanceIdTag length = %d, want 16 (GUID name)", len(AppInstanceIdTag))
	}
}

func TestProcessAppInstanceId_NotPresent(t *testing.T) {
	store := newMockDurableStore()

	contexts := []CreateContext{
		{Name: "MxAc", Data: make([]byte, 8)},
	}

	appId := ProcessAppInstanceId(context.Background(), store, nil, contexts)
	if appId != ([16]byte{}) {
		t.Errorf("Expected zero AppInstanceId when not present, got %x", appId)
	}
}

func TestProcessAppInstanceId_ForceClosesOldHandles(t *testing.T) {
	store := newMockDurableStore()
	ctx := context.Background()

	appId := [16]byte{0xC0, 0xC1, 0xC2, 0xC3, 0xC4, 0xC5, 0xC6, 0xC7, 0xC8, 0xC9, 0xCA, 0xCB, 0xCC, 0xCD, 0xCE, 0xCF}

	// Pre-populate with handles matching this AppInstanceId
	_ = store.PutDurableHandle(ctx, &lock.PersistedDurableHandle{
		ID:            "old-001",
		AppInstanceId: appId,
		ShareName:     "/share1",
		Path:          "old1.txt",
	})
	_ = store.PutDurableHandle(ctx, &lock.PersistedDurableHandle{
		ID:            "old-002",
		AppInstanceId: appId,
		ShareName:     "/share1",
		Path:          "old2.txt",
	})

	// Build AppInstanceId context
	appIdData := make([]byte, 20)
	binary.LittleEndian.PutUint16(appIdData[0:2], 20) // StructureSize
	copy(appIdData[4:20], appId[:])

	contexts := []CreateContext{
		{Name: AppInstanceIdTag, Data: appIdData},
	}

	result := ProcessAppInstanceId(ctx, store, nil, contexts)
	if result != appId {
		t.Errorf("Expected AppInstanceId %x, got %x", appId, result)
	}

	// Verify old handles were force-closed (deleted from store)
	h1, _ := store.GetDurableHandle(ctx, "old-001")
	h2, _ := store.GetDurableHandle(ctx, "old-002")
	if h1 != nil {
		t.Error("Expected old-001 to be deleted")
	}
	if h2 != nil {
		t.Error("Expected old-002 to be deleted")
	}
}

// TestProcessDurableReconnectContext_OriginalFileIDRestored locks in the
// contract added in #661: when a durable handle was persisted by the
// updated handler, OriginalFileID carries the full 16-byte FileID from the
// original CREATE response. validateAndRestore MUST use OriginalFileID for
// the new OpenFile so byte-range locks (which key on OpenID derived from
// FileID) stay valid across the durable disconnect. Required for
// smb2.durable-open.lock-{oplock,lease}.
func TestProcessDurableReconnectContext_OriginalFileIDRestored(t *testing.T) {
	store := newMockDurableStore()
	ctx := context.Background()

	// DHnC lookup uses volatile-zeroed FileID; OriginalFileID retains full bytes
	persistentFileID := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 0, 0, 0, 0, 0, 0, 0, 0}
	originalFileID := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF, 0x11, 0x22}
	keyHash := makeSessionKeyHash("session-key-orig")

	_ = store.PutDurableHandle(ctx, &lock.PersistedDurableHandle{
		ID:             "dh-orig",
		FileID:         persistentFileID,
		OriginalFileID: originalFileID,
		Path:           "lockfile.txt",
		ShareName:      "/share1",
		MetadataHandle: []byte{0xCA, 0xFE},
		Username:       "alice",
		SessionKeyHash: keyHash,
		CreatedAt:      time.Now().Add(-time.Minute),
		DisconnectedAt: time.Now().Add(-time.Second),
		TimeoutMs:      60000,
	})

	dhnCData := make([]byte, 16)
	copy(dhnCData, persistentFileID[:])
	contexts := []CreateContext{
		{Name: DurableHandleV1ReconnectTag, Data: dhnCData},
	}

	res, status, err := ProcessDurableReconnectContext(ctx, store, nil, contexts,
		1, "alice", keyHash, "/share1", "lockfile.txt", [16]byte{})
	if err != nil || status != types.StatusSuccess {
		t.Fatalf("reconnect: status=%s err=%v", status, err)
	}
	if res.OpenFile.FileID != originalFileID {
		t.Errorf("restored FileID = %x, want %x (OriginalFileID)", res.OpenFile.FileID, originalFileID)
	}
	if res.OriginalFileID != originalFileID {
		t.Errorf("ReconnectResult.OriginalFileID = %x, want %x", res.OriginalFileID, originalFileID)
	}
}

// TestProcessDurableReconnectContext_OriginalFileIDFallback covers the
// upgrade-boundary case: a handle persisted before OriginalFileID existed
// decodes with OriginalFileID == 0. validateAndRestore MUST fall back to
// handle.FileID (volatile-zeroed) so old handles still reconnect; the
// create.go reconnect path then regenerates the volatile half (#661).
func TestProcessDurableReconnectContext_OriginalFileIDFallback(t *testing.T) {
	store := newMockDurableStore()
	ctx := context.Background()

	persistentFileID := [16]byte{9, 9, 9, 9, 9, 9, 9, 9, 0, 0, 0, 0, 0, 0, 0, 0}
	keyHash := makeSessionKeyHash("session-key-old")

	_ = store.PutDurableHandle(ctx, &lock.PersistedDurableHandle{
		ID:     "dh-legacy",
		FileID: persistentFileID,
		// OriginalFileID intentionally zero (pre-#661 handle)
		Path:           "legacy.txt",
		ShareName:      "/share1",
		MetadataHandle: []byte{0xDE, 0xAD},
		Username:       "alice",
		SessionKeyHash: keyHash,
		CreatedAt:      time.Now().Add(-time.Minute),
		DisconnectedAt: time.Now().Add(-time.Second),
		TimeoutMs:      60000,
	})

	dhnCData := make([]byte, 16)
	copy(dhnCData, persistentFileID[:])
	contexts := []CreateContext{
		{Name: DurableHandleV1ReconnectTag, Data: dhnCData},
	}

	res, status, err := ProcessDurableReconnectContext(ctx, store, nil, contexts,
		1, "alice", keyHash, "/share1", "legacy.txt", [16]byte{})
	if err != nil || status != types.StatusSuccess {
		t.Fatalf("reconnect: status=%s err=%v", status, err)
	}
	if res.OpenFile.FileID != persistentFileID {
		t.Errorf("legacy restore FileID = %x, want %x (handle.FileID fallback)",
			res.OpenFile.FileID, persistentFileID)
	}
	if res.OriginalFileID != ([16]byte{}) {
		t.Errorf("ReconnectResult.OriginalFileID = %x, want zero", res.OriginalFileID)
	}
}

// TestProcessDurableReconnectContext_PositionInfoRestored locks in the
// FilePositionInformation persistence added in #661 — required so
// smb2.durable-open.file-position survives reconnect.
func TestProcessDurableReconnectContext_PositionInfoRestored(t *testing.T) {
	store := newMockDurableStore()
	ctx := context.Background()

	fileID := [16]byte{0xFE, 0xED, 0xFA, 0xCE, 0xBE, 0xEF, 0xCA, 0xFE, 0, 0, 0, 0, 0, 0, 0, 0}
	keyHash := makeSessionKeyHash("session-key-pos")

	_ = store.PutDurableHandle(ctx, &lock.PersistedDurableHandle{
		ID:             "dh-pos",
		FileID:         fileID,
		Path:           "position.txt",
		ShareName:      "/share1",
		MetadataHandle: []byte{0xAA},
		Username:       "alice",
		SessionKeyHash: keyHash,
		PositionInfo:   0x1000,
		CreatedAt:      time.Now().Add(-time.Minute),
		DisconnectedAt: time.Now().Add(-time.Second),
		TimeoutMs:      60000,
	})

	dhnCData := make([]byte, 16)
	copy(dhnCData, fileID[:])
	contexts := []CreateContext{
		{Name: DurableHandleV1ReconnectTag, Data: dhnCData},
	}

	res, status, err := ProcessDurableReconnectContext(ctx, store, nil, contexts,
		1, "alice", keyHash, "/share1", "position.txt", [16]byte{})
	if err != nil || status != types.StatusSuccess {
		t.Fatalf("reconnect: status=%s err=%v", status, err)
	}
	if res.OpenFile.PositionInfo != 0x1000 {
		t.Errorf("restored PositionInfo = 0x%x, want 0x1000", res.OpenFile.PositionInfo)
	}
}

func TestValidateDurableContexts(t *testing.T) {
	mkDH2C := func() CreateContext {
		return CreateContext{Name: DurableHandleV2ReconnectTag, Data: make([]byte, 36)}
	}
	mkDH2Q := func() CreateContext {
		return CreateContext{Name: DurableHandleV2RequestTag, Data: make([]byte, 32)}
	}
	mkDHnC := func() CreateContext {
		return CreateContext{Name: DurableHandleV1ReconnectTag, Data: make([]byte, 16)}
	}
	mkDHnQ := func() CreateContext {
		// V1 request: tag "DHnQ", 16 reserved bytes.
		return CreateContext{Name: DurableHandleV1RequestTag, Data: make([]byte, 16)}
	}

	cases := []struct {
		name     string
		contexts []CreateContext
		want     types.Status
	}{
		{"empty", nil, types.StatusSuccess},
		{"DHnQ only", []CreateContext{mkDHnQ()}, types.StatusSuccess},
		{"DH2Q only", []CreateContext{mkDH2Q()}, types.StatusSuccess},
		{"DHnC only", []CreateContext{mkDHnC()}, types.StatusSuccess},
		{"DH2C only", []CreateContext{mkDH2C()}, types.StatusSuccess},

		// MS-SMB2 §3.3.5.9.12 rejection cases (smbtorture create-blob)
		{"DHnC + DH2C", []CreateContext{mkDHnC(), mkDH2C()}, types.StatusInvalidParameter},
		{"DHnC + DH2Q", []CreateContext{mkDHnC(), mkDH2Q()}, types.StatusInvalidParameter},
		{"DH2C + DHnQ", []CreateContext{mkDH2C(), mkDHnQ()}, types.StatusInvalidParameter},
		{"DH2C + DH2Q", []CreateContext{mkDH2C(), mkDH2Q()}, types.StatusInvalidParameter},

		// Truncated DH2C (length must be exactly 36 per Samba; we accept >=36
		// at decode time but reject mismatched length up-front).
		{"DH2C short", []CreateContext{{Name: DurableHandleV2ReconnectTag, Data: make([]byte, 20)}}, types.StatusInvalidParameter},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ValidateDurableContexts(c.contexts)
			if got != c.want {
				t.Errorf("ValidateDurableContexts() = %v, want %v", got, c.want)
			}
		})
	}
}

// TestProcessDurableReconnectContext_V2LeaseClientGUIDMismatch locks in the
// MS-SMB2 §3.3.5.9.12 lease-key per-ClientGuid scoping behavior exercised by
// smb2.durable-v2-open.reopen1a-lease.
func TestProcessDurableReconnectContext_V2LeaseClientGUIDMismatch(t *testing.T) {
	store := newMockDurableStore()
	ctx := context.Background()

	keyHash := makeSessionKeyHash("session-key")
	createGuid := [16]byte{0xC0, 0xC1, 0xC2, 0xC3, 0xC4, 0xC5, 0xC6, 0xC7}
	leaseKey := [16]byte{0xAA, 0xBB, 0xCC, 0xDD}
	fileID := [16]byte{0x10, 0x20, 0x30}
	originalGUID := [16]byte{0xE0, 0xE1, 0xE2, 0xE3, 0xE4, 0xE5, 0xE6, 0xE7}
	differentGUID := [16]byte{0xF0, 0xF1, 0xF2}

	_ = store.PutDurableHandle(ctx, &lock.PersistedDurableHandle{
		ID:             "h-001",
		FileID:         fileID,
		Path:           "leasefile.txt",
		ShareName:      "/share1",
		Username:       "alice",
		SessionKeyHash: keyHash,
		CreateGuid:     createGuid,
		// A lease-backed durable handle persists OplockLevel=Lease (mirrors
		// buildPersistedDurableHandle for an SMB lease open). The V2 lease gate
		// keys "persisted handle has a lease" on (OplockLevel==Lease && LeaseKey
		// != 0), so both must be set for this to model a real lease open.
		OplockLevel:    OplockLevelLease,
		LeaseKey:       leaseKey,
		ClientGUID:     originalGUID,
		IsV2:           true,
		CreatedAt:      time.Now().Add(-time.Minute),
		DisconnectedAt: time.Now().Add(-time.Second),
		TimeoutMs:      60000,
		MetadataHandle: []byte{0xAA},
	})

	// Build DH2C reconnect (FileID + CreateGuid + zero flags) plus the matching
	// lease context. A lease-backed V2 reconnect MUST carry its RqLs lease
	// context (MS-SMB2 §3.3.5.9.12 / Samba reopen1a-lease) — the V2 lease gate
	// in processV2Reconnect rejects a lease-backed handle reconnected without
	// one (OBJECT_NAME_NOT_FOUND) before it reaches the ClientGuid scoping
	// check, so the lease key must match here to actually exercise the GUID
	// branch under test.
	dh2cData := make([]byte, 36)
	copy(dh2cData[:16], fileID[:])
	copy(dh2cData[16:32], createGuid[:])
	contexts := []CreateContext{
		{Name: DurableHandleV2ReconnectTag, Data: dh2cData},
		{Name: LeaseContextTagRequest, Data: encodeV1LeaseRequestContext(leaseKey, 0)},
	}

	// Wrong ClientGuid → OBJECT_NAME_NOT_FOUND
	_, status, err := ProcessDurableReconnectContext(ctx, store, nil, contexts,
		1, "alice", keyHash, "/share1", "leasefile.txt", differentGUID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != types.StatusObjectNameNotFound {
		t.Errorf("ClientGuid mismatch: got status %v, want OBJECT_NAME_NOT_FOUND", status)
	}

	// Original ClientGuid → success
	_, status, err = ProcessDurableReconnectContext(ctx, store, nil, contexts,
		1, "alice", keyHash, "/share1", "leasefile.txt", originalGUID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != types.StatusSuccess {
		t.Errorf("ClientGuid match: got status %v, want SUCCESS", status)
	}
}

// TestProcessDurableReconnectContext_V2OplockNoClientGUIDCheck covers
// reopen1a (oplock-backed, not lease-backed): reconnect with a different
// ClientGuid MUST still succeed because lease scoping does not apply.
func TestProcessDurableReconnectContext_V2OplockNoClientGUIDCheck(t *testing.T) {
	store := newMockDurableStore()
	ctx := context.Background()

	keyHash := makeSessionKeyHash("session-key")
	createGuid := [16]byte{0x11, 0x22, 0x33, 0x44}
	fileID := [16]byte{0x55, 0x66, 0x77}
	originalGUID := [16]byte{0xAA, 0xBB, 0xCC}
	differentGUID := [16]byte{0xDD, 0xEE, 0xFF}

	_ = store.PutDurableHandle(ctx, &lock.PersistedDurableHandle{
		ID:             "h-002",
		FileID:         fileID,
		Path:           "oplockfile.txt",
		ShareName:      "/share1",
		Username:       "alice",
		SessionKeyHash: keyHash,
		CreateGuid:     createGuid,
		// LeaseKey deliberately zero → oplock-backed open
		ClientGUID:     originalGUID,
		IsV2:           true,
		CreatedAt:      time.Now().Add(-time.Minute),
		DisconnectedAt: time.Now().Add(-time.Second),
		TimeoutMs:      60000,
		MetadataHandle: []byte{0xAA},
	})

	dh2cData := make([]byte, 36)
	copy(dh2cData[:16], fileID[:])
	copy(dh2cData[16:32], createGuid[:])
	contexts := []CreateContext{
		{Name: DurableHandleV2ReconnectTag, Data: dh2cData},
	}

	_, status, err := ProcessDurableReconnectContext(ctx, store, nil, contexts,
		1, "alice", keyHash, "/share1", "oplockfile.txt", differentGUID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != types.StatusSuccess {
		t.Errorf("oplock V2 reconnect with different ClientGuid: got status %v, want SUCCESS", status)
	}
}

// TestValidateAndRestore_ClientGUIDRoundtrip verifies that ClientGUID is
// restored onto the OpenFile by the reconnect path so the next persist
// (chained disconnect→reconnect→disconnect) carries the original GUID.
// Without this, the §3.3.5.9.12 lease-scoping check would no-op on the
// second reconnect and any client could reclaim a lease-backed durable handle.
// Locks in Copilot review finding on durable_context.go:742.
func TestValidateAndRestore_ClientGUIDRoundtrip(t *testing.T) {
	store := newMockDurableStore()
	ctx := context.Background()

	keyHash := makeSessionKeyHash("session-key")
	originalGUID := [16]byte{0xA0, 0xA1, 0xA2, 0xA3, 0xA4, 0xA5}
	leaseKey := [16]byte{0xB0, 0xB1, 0xB2}
	createGuid := [16]byte{0xC0, 0xC1, 0xC2}
	fileID := [16]byte{0xD0, 0xD1, 0xD2}

	_ = store.PutDurableHandle(ctx, &lock.PersistedDurableHandle{
		ID:             "h-roundtrip",
		FileID:         fileID,
		Path:           "leasefile.txt",
		ShareName:      "/share1",
		Username:       "alice",
		SessionKeyHash: keyHash,
		CreateGuid:     createGuid,
		LeaseKey:       leaseKey,
		ClientGUID:     originalGUID,
		IsV2:           true,
		CreatedAt:      time.Now().Add(-time.Minute),
		DisconnectedAt: time.Now().Add(-time.Second),
		TimeoutMs:      60000,
		MetadataHandle: []byte{0xAA},
	})

	dh2cData := make([]byte, 36)
	copy(dh2cData[:16], fileID[:])
	copy(dh2cData[16:32], createGuid[:])
	contexts := []CreateContext{
		{Name: DurableHandleV2ReconnectTag, Data: dh2cData},
	}

	res, status, err := ProcessDurableReconnectContext(ctx, store, nil, contexts,
		1, "alice", keyHash, "/share1", "leasefile.txt", originalGUID)
	if err != nil || status != types.StatusSuccess {
		t.Fatalf("reconnect failed: status=%v err=%v", status, err)
	}
	if res.OpenFile.ClientGUID != originalGUID {
		t.Errorf("restored ClientGUID = %x, want %x (Copilot review #1: chained reconnect breaks lease scoping)",
			res.OpenFile.ClientGUID, originalGUID)
	}
}

// TestProcessDurableHandleContext_V2RequiresBatchOrHandleLease locks in
// MS-SMB2 §3.3.5.9.10: V2 durability MUST NOT be granted unless OplockLevel
// is Batch OR the granted lease includes SMB2_LEASE_HANDLE_CACHING.
// smbtorture smb2.durable-v2-open.open-oplock iterates 8 share-mode × 4
// oplock-level rows expecting `out.durable_open_v2 == false` for every
// non-Batch row.
func TestProcessDurableHandleContext_V2RequiresBatchOrHandleLease(t *testing.T) {
	createGuid := [16]byte{0xA0, 0xA1, 0xA2}
	dh2qData := make([]byte, 32)
	binary.LittleEndian.PutUint32(dh2qData[0:4], 60000)
	copy(dh2qData[16:32], createGuid[:])

	cases := []struct {
		name          string
		oplockLevel   uint8
		handleLease   bool
		expectGranted bool
	}{
		{"None oplock, no lease", OplockLevelNone, false, false},
		{"Level II oplock", OplockLevelII, false, false},
		{"Exclusive oplock", OplockLevelExclusive, false, false},
		{"Batch oplock", OplockLevelBatch, false, true},
		{"None oplock, Handle lease", OplockLevelNone, true, true},
		{"Lease level with Handle", OplockLevelLease, true, true},
		{"Lease level no Handle", OplockLevelLease, false, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			openFile := &OpenFile{
				FileID:      [16]byte{1, 2, 3},
				OplockLevel: c.oplockLevel,
			}
			contexts := []CreateContext{
				{Name: DurableHandleV2RequestTag, Data: dh2qData},
			}
			resp := ProcessDurableHandleContext(contexts, openFile, DurableGrantOptions{
				ConfiguredTimeoutMs: 60000,
				LeaseIncludesHandle: c.handleLease,
			})
			granted := resp != nil
			if granted != c.expectGranted {
				t.Errorf("granted=%v want %v (oplock=%d handleLease=%v)",
					granted, c.expectGranted, c.oplockLevel, c.handleLease)
			}
			if openFile.IsDurable != c.expectGranted {
				t.Errorf("IsDurable=%v want %v", openFile.IsDurable, c.expectGranted)
			}
		})
	}
}

// TestProcessDurableHandleContext_V2RejectZeroCreateGuid pins MS-SMB2
// §2.2.13.2.11: an all-zero CreateGuid is not a valid identifier and must
// not grant V2 durability. The zero value also collides with the "absent
// CreateGuid" sentinel in storage, so accepting it would corrupt later
// lookups.
func TestProcessDurableHandleContext_V2RejectZeroCreateGuid(t *testing.T) {
	openFile := &OpenFile{
		FileID:      [16]byte{1, 2, 3},
		OplockLevel: OplockLevelBatch,
	}

	dh2qData := make([]byte, 32)
	binary.LittleEndian.PutUint32(dh2qData[0:4], 60000)
	// dh2qData[16:32] left as zero — zero CreateGuid

	contexts := []CreateContext{
		{Name: DurableHandleV2RequestTag, Data: dh2qData},
	}

	resp := ProcessDurableHandleContext(contexts, openFile, DurableGrantOptions{ConfiguredTimeoutMs: 60000})
	if resp != nil {
		t.Error("Expected nil response for zero CreateGuid (V2 not granted)")
	}
	if openFile.IsDurable {
		t.Error("Expected openFile.IsDurable to remain false")
	}
	if openFile.CreateGuid != ([16]byte{}) {
		t.Errorf("Expected CreateGuid untouched (zero), got %x", openFile.CreateGuid)
	}
}

// TestProcessDurableReconnectContext_ConsumeAtomicV1 verifies that the V1
// reconnect path uses ConsumeDurableHandleByFileID — i.e. two concurrent
// reconnect attempts for the same persisted FileID cannot both succeed.
// Mocks a store that releases its internal lock between Get and Delete
// would expose a TOCTOU window; the Consume contract closes it.
func TestProcessDurableReconnectContext_ConsumeAtomicV1(t *testing.T) {
	store := newMockDurableStore()
	ctx := context.Background()

	// Persisted FileIDs always carry zeroed volatile half — see
	// `buildPersistedDurableHandle`. Tests must match this to model the
	// real store contents.
	fileID := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 0, 0, 0, 0, 0, 0, 0, 0}
	keyHash := makeSessionKeyHash("k")

	_ = store.PutDurableHandle(ctx, &lock.PersistedDurableHandle{
		ID:             "dh-toctou-v1",
		FileID:         fileID,
		Path:           "race.txt",
		ShareName:      "/share1",
		MetadataHandle: []byte{0xDE, 0xAD},
		Username:       "alice",
		SessionKeyHash: keyHash,
		OplockLevel:    OplockLevelBatch,
		DisconnectedAt: time.Now().Add(-1 * time.Second),
		TimeoutMs:      60000,
	})

	dhnCData := make([]byte, 16)
	copy(dhnCData[:], fileID[:])
	contexts := []CreateContext{
		{Name: DurableHandleV1ReconnectTag, Data: dhnCData},
	}

	// First reconnect should succeed and consume the handle.
	res1, status1, err1 := ProcessDurableReconnectContext(
		ctx, store, nil, contexts, 1, "alice", keyHash, "/share1", "race.txt", [16]byte{},
	)
	if err1 != nil || status1 != types.StatusSuccess || res1 == nil {
		t.Fatalf("first reconnect: status=%v err=%v res=%v", status1, err1, res1)
	}

	// Second reconnect for the same FileID must NOT succeed — the handle
	// has been atomically consumed; ObjectNameNotFound is returned.
	_, status2, err2 := ProcessDurableReconnectContext(
		ctx, store, nil, contexts, 2, "alice", keyHash, "/share1", "race.txt", [16]byte{},
	)
	if err2 != nil {
		t.Fatalf("second reconnect error: %v", err2)
	}
	if status2 != types.StatusObjectNameNotFound {
		t.Errorf("second reconnect status = %v, want STATUS_OBJECT_NAME_NOT_FOUND", status2)
	}
}

// TestProcessDurableReconnectContext_ConsumeAtomicV2 is the V2 (DH2C)
// counterpart of the V1 atomicity test.
func TestProcessDurableReconnectContext_ConsumeAtomicV2(t *testing.T) {
	store := newMockDurableStore()
	ctx := context.Background()

	// Persisted FileIDs always carry zeroed volatile half — see
	// `buildPersistedDurableHandle`. Tests must match this to model the
	// real store contents.
	fileID := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 0, 0, 0, 0, 0, 0, 0, 0}
	createGuid := [16]byte{0xA0, 0xA1, 0xA2, 0xA3, 0xA4, 0xA5, 0xA6, 0xA7, 0xA8, 0xA9, 0xAA, 0xAB, 0xAC, 0xAD, 0xAE, 0xAF}
	keyHash := makeSessionKeyHash("k")

	_ = store.PutDurableHandle(ctx, &lock.PersistedDurableHandle{
		ID:             "dh-toctou-v2",
		FileID:         fileID,
		CreateGuid:     createGuid,
		Path:           "race.txt",
		ShareName:      "/share1",
		MetadataHandle: []byte{0xDE, 0xAD},
		Username:       "alice",
		SessionKeyHash: keyHash,
		OplockLevel:    OplockLevelBatch,
		IsV2:           true,
		DisconnectedAt: time.Now().Add(-1 * time.Second),
		TimeoutMs:      60000,
	})

	dh2cData := make([]byte, 36)
	copy(dh2cData[0:16], fileID[:])
	copy(dh2cData[16:32], createGuid[:])
	contexts := []CreateContext{
		{Name: DurableHandleV2ReconnectTag, Data: dh2cData},
	}

	res1, status1, err1 := ProcessDurableReconnectContext(
		ctx, store, nil, contexts, 1, "alice", keyHash, "/share1", "race.txt", [16]byte{},
	)
	if err1 != nil || status1 != types.StatusSuccess || res1 == nil {
		t.Fatalf("first reconnect: status=%v err=%v res=%v", status1, err1, res1)
	}

	_, status2, err2 := ProcessDurableReconnectContext(
		ctx, store, nil, contexts, 2, "alice", keyHash, "/share1", "race.txt", [16]byte{},
	)
	if err2 != nil {
		t.Fatalf("second reconnect error: %v", err2)
	}
	if status2 != types.StatusObjectNameNotFound {
		t.Errorf("second reconnect status = %v, want STATUS_OBJECT_NAME_NOT_FOUND", status2)
	}
}

// TestProcessDurableReconnectContext_V1OplockIgnoresPath verifies that a V1
// DHnC reconnect on an oplock-backed durable handle (no lease) IGNORES the
// CREATE request's filename — matching MS-SMB2 §3.3.5.9.7 and Samba's
// `smbd_smb2_create_durable_lease_check` which only path-checks lease-backed
// reopens. smbtorture smb2.durable-open.reopen2 step 3 deliberately passes
// "__non_existing_fname__" to prove this.
func TestProcessDurableReconnectContext_V1OplockIgnoresPath(t *testing.T) {
	store := newMockDurableStore()
	ctx := context.Background()

	// Persisted FileIDs always carry zeroed volatile half — see
	// `buildPersistedDurableHandle`. Tests must match this to model the
	// real store contents.
	fileID := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 0, 0, 0, 0, 0, 0, 0, 0}
	keyHash := makeSessionKeyHash("session-key-v1path")

	_ = store.PutDurableHandle(ctx, &lock.PersistedDurableHandle{
		ID:             "dh-v1-path-ignore",
		FileID:         fileID,
		Path:           "real.dat",
		ShareName:      "/share1",
		DesiredAccess:  0x12019F,
		ShareAccess:    0x07,
		MetadataHandle: []byte{0xDE, 0xAD},
		OplockLevel:    OplockLevelBatch,
		Username:       "alice",
		SessionKeyHash: keyHash,
		IsV2:           false,
		CreatedAt:      time.Now().Add(-5 * time.Minute),
		DisconnectedAt: time.Now().Add(-10 * time.Second),
		TimeoutMs:      60000,
	})

	dhnCData := make([]byte, 16)
	copy(dhnCData[:], fileID[:])

	contexts := []CreateContext{
		{Name: DurableHandleV1ReconnectTag, Data: dhnCData},
	}

	res, status, err := ProcessDurableReconnectContext(
		ctx, store, nil, contexts, 999, "alice", keyHash,
		"/share1", "__non_existing_fname__", [16]byte{},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != types.StatusSuccess {
		t.Fatalf("Expected STATUS_SUCCESS for V1 oplock reconnect with wrong fname, got %s", status)
	}
	if res == nil || res.OpenFile == nil {
		t.Fatal("Expected restored OpenFile")
	}
	if res.OpenFile.Path != "real.dat" {
		t.Errorf("Restored Path = %q, want %q", res.OpenFile.Path, "real.dat")
	}
}

// TestProcessDurableReconnectContext_V1LeasedRejectsMissingLease verifies that
// a V1 reconnect for a lease-backed persisted handle MUST carry a lease
// context; absent that, the server returns OBJECT_NAME_NOT_FOUND
// (smb2.durable-open.reopen2_lease "without lease attached" cases).
func TestProcessDurableReconnectContext_V1LeasedRejectsMissingLease(t *testing.T) {
	store := newMockDurableStore()
	ctx := context.Background()

	fileID := [16]byte{2, 3, 4, 5, 6, 7, 8, 9, 0, 0, 0, 0, 0, 0, 0, 0}
	leaseKey := [16]byte{0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF, 0x00}
	keyHash := makeSessionKeyHash("session-key-v1lease")

	_ = store.PutDurableHandle(ctx, &lock.PersistedDurableHandle{
		ID:             "dh-v1-leased",
		FileID:         fileID,
		Path:           "leased.dat",
		ShareName:      "/share1",
		DesiredAccess:  0x12019F,
		ShareAccess:    0x07,
		MetadataHandle: []byte{0xDE, 0xAD},
		OplockLevel:    OplockLevelLease,
		LeaseKey:       leaseKey,
		Username:       "alice",
		SessionKeyHash: keyHash,
		DisconnectedAt: time.Now().Add(-10 * time.Second),
		TimeoutMs:      60000,
	})

	dhnCData := make([]byte, 16)
	copy(dhnCData[:], fileID[:])

	contexts := []CreateContext{
		{Name: DurableHandleV1ReconnectTag, Data: dhnCData},
	}

	_, status, err := ProcessDurableReconnectContext(
		ctx, store, nil, contexts, 999, "alice", keyHash,
		"/share1", "leased.dat", [16]byte{},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != types.StatusObjectNameNotFound {
		t.Errorf("Expected OBJECT_NAME_NOT_FOUND for V1 reconnect of leased handle without RqLs, got %s", status)
	}
}

// TestProcessDurableReconnectContext_V1LeasedWrongLeaseKey verifies that a V1
// reconnect with a lease context carrying a non-matching lease key returns
// OBJECT_NAME_NOT_FOUND (smb2.durable-open.reopen2_lease "wrong lease key" case).
func TestProcessDurableReconnectContext_V1LeasedWrongLeaseKey(t *testing.T) {
	store := newMockDurableStore()
	ctx := context.Background()

	fileID := [16]byte{3, 4, 5, 6, 7, 8, 9, 10, 0, 0, 0, 0, 0, 0, 0, 0}
	leaseKey := [16]byte{0xAA, 0xAA, 0xAA, 0xAA, 0xAA, 0xAA, 0xAA, 0xAA, 0xAA, 0xAA, 0xAA, 0xAA, 0xAA, 0xAA, 0xAA, 0xAA}
	wrongKey := [16]byte{0xBB, 0xBB, 0xBB, 0xBB, 0xBB, 0xBB, 0xBB, 0xBB, 0xBB, 0xBB, 0xBB, 0xBB, 0xBB, 0xBB, 0xBB, 0xBB}
	keyHash := makeSessionKeyHash("session-key-v1wrongkey")

	_ = store.PutDurableHandle(ctx, &lock.PersistedDurableHandle{
		ID:             "dh-v1-wrongkey",
		FileID:         fileID,
		Path:           "leased.dat",
		ShareName:      "/share1",
		MetadataHandle: []byte{0xDE, 0xAD},
		OplockLevel:    OplockLevelLease,
		LeaseKey:       leaseKey,
		Username:       "alice",
		SessionKeyHash: keyHash,
		DisconnectedAt: time.Now().Add(-10 * time.Second),
		TimeoutMs:      60000,
	})

	dhnCData := make([]byte, 16)
	copy(dhnCData[:], fileID[:])

	// V1 lease context (32 bytes: 16 LeaseKey + 4 LeaseState + 4 Flags + 8 Duration)
	leaseCtxData := make([]byte, 32)
	copy(leaseCtxData[0:16], wrongKey[:])
	binary.LittleEndian.PutUint32(leaseCtxData[16:20], 0x7) // RWH requested

	contexts := []CreateContext{
		{Name: DurableHandleV1ReconnectTag, Data: dhnCData},
		{Name: LeaseContextTagRequest, Data: leaseCtxData},
	}

	_, status, err := ProcessDurableReconnectContext(
		ctx, store, nil, contexts, 999, "alice", keyHash,
		"/share1", "leased.dat", [16]byte{},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != types.StatusObjectNameNotFound {
		t.Errorf("Expected OBJECT_NAME_NOT_FOUND for wrong lease key, got %s", status)
	}
}

// TestProcessDurableReconnectContext_V1UnleasedRejectsLeaseCtx verifies that
// a V1 reconnect carrying a lease context against an oplock-only persisted
// handle returns OBJECT_NAME_NOT_FOUND (mirror of the inverse asymmetry).
func TestProcessDurableReconnectContext_V1UnleasedRejectsLeaseCtx(t *testing.T) {
	store := newMockDurableStore()
	ctx := context.Background()

	fileID := [16]byte{4, 5, 6, 7, 8, 9, 10, 11, 0, 0, 0, 0, 0, 0, 0, 0}
	keyHash := makeSessionKeyHash("session-key-v1nolease")

	_ = store.PutDurableHandle(ctx, &lock.PersistedDurableHandle{
		ID:             "dh-v1-nolease",
		FileID:         fileID,
		Path:           "plain.dat",
		ShareName:      "/share1",
		MetadataHandle: []byte{0xDE, 0xAD},
		OplockLevel:    OplockLevelBatch,
		Username:       "alice",
		SessionKeyHash: keyHash,
		DisconnectedAt: time.Now().Add(-10 * time.Second),
		TimeoutMs:      60000,
	})

	dhnCData := make([]byte, 16)
	copy(dhnCData[:], fileID[:])

	someLeaseKey := [16]byte{0xCC, 0xCC, 0xCC, 0xCC, 0xCC, 0xCC, 0xCC, 0xCC, 0xCC, 0xCC, 0xCC, 0xCC, 0xCC, 0xCC, 0xCC, 0xCC}
	leaseCtxData := make([]byte, 32)
	copy(leaseCtxData[0:16], someLeaseKey[:])

	contexts := []CreateContext{
		{Name: DurableHandleV1ReconnectTag, Data: dhnCData},
		{Name: LeaseContextTagRequest, Data: leaseCtxData},
	}

	_, status, err := ProcessDurableReconnectContext(
		ctx, store, nil, contexts, 999, "alice", keyHash,
		"/share1", "plain.dat", [16]byte{},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != types.StatusObjectNameNotFound {
		t.Errorf("Expected OBJECT_NAME_NOT_FOUND for lease ctx vs oplock-only persisted handle, got %s", status)
	}
}

// TestProcessDurableReconnectContext_V1WireReplaysFullFileID verifies that a
// V1 DHnC reconnect succeeds when the wire FileID carries a non-zero volatile
// half (replaying the full 16 bytes returned at original CREATE). MS-SMB2
// §3.2.4.4 mandates Data.Volatile=0 on DHnC, but smbtorture's `smb2_push_handle`
// (source4/libcli/smb2/request.c) does not zero the volatile — it replays the
// full handle. The server must tolerate this by matching on the persistent
// half only. Without this fix, smbtorture's smb2.durable-open.reopen2 step 1
// returns OBJECT_NAME_NOT_FOUND on the first reconnect.
func TestProcessDurableReconnectContext_V1WireReplaysFullFileID(t *testing.T) {
	store := newMockDurableStore()
	ctx := context.Background()

	// Persisted handle: volatile half is zero (matches buildPersistedDurableHandle).
	persistedFileID := [16]byte{0xAB, 0xCD, 0xEF, 0x01, 0x02, 0x03, 0x04, 0x05, 0, 0, 0, 0, 0, 0, 0, 0}
	keyHash := makeSessionKeyHash("session-key-replay")

	_ = store.PutDurableHandle(ctx, &lock.PersistedDurableHandle{
		ID:             "dh-wire-replay",
		FileID:         persistedFileID,
		Path:           "x.dat",
		ShareName:      "/share1",
		MetadataHandle: []byte{0xDE, 0xAD},
		OplockLevel:    OplockLevelBatch,
		Username:       "alice",
		SessionKeyHash: keyHash,
		CreatedAt:      time.Now().Add(-5 * time.Minute),
		DisconnectedAt: time.Now().Add(-10 * time.Second),
		TimeoutMs:      60000,
	})

	// Wire FileID has the SAME persistent half but a non-zero volatile half
	// (the value the server originally returned at CREATE time).
	wireFileID := persistedFileID
	for i := 8; i < 16; i++ {
		wireFileID[i] = byte(0x80 + i)
	}

	dhnCData := make([]byte, 16)
	copy(dhnCData[:], wireFileID[:])
	contexts := []CreateContext{
		{Name: DurableHandleV1ReconnectTag, Data: dhnCData},
	}

	res, status, err := ProcessDurableReconnectContext(
		ctx, store, nil, contexts, 999, "alice", keyHash,
		"/share1", "x.dat", [16]byte{},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != types.StatusSuccess {
		t.Fatalf("Expected STATUS_SUCCESS when wire replays full FileID, got %s", status)
	}
	if res == nil || res.OpenFile == nil {
		t.Fatal("Expected restored OpenFile")
	}
}

// ----------------------------------------------------------------------------
// Cross-connection reopen2-family repro.
//
// These tests model the smbtorture reopen2 family at the handler boundary: an
// original durable open persisted by session A's transport-disconnect, then a
// reconnect arriving on a FRESH session (different SessionID) whose CREATE
// request fills every field EXCEPT the reconnect blob with junk — by design,
// to prove the server consults only the reconnect context. The reconnect MUST
// return STATUS_SUCCESS and restore the ORIGINAL granted access, never the junk
// access requested in the reconnect CREATE.
//
// Confirmed against Samba source3/smbd/smb2_create.c: the durable reconnect
// path validates only lease key, filename (lease/path-checked opens only),
// create_guid, and user identity — there is NO desired_access / share_access
// comparison. Before this fix, validateAndRestore re-validated DesiredAccess /
// ShareAccess (-> ACCESS_DENIED) and processV2Reconnect always path-checked
// (-> INVALID_PARAMETER for the junk-fname oplock case).
// ----------------------------------------------------------------------------

// junkFname models the garbage filename smbtorture replays on a reopen2
// reconnect CREATE (the real torture value is "__non_existing_fname__"). The
// reconnect blob (FileID/CreateGuid/lease key) is the sole identifier; for an
// oplock-backed reopen the server must ignore this fname entirely.
//
// Note: the reconnect CREATE's DesiredAccess/ShareAccess (smbtorture sets both
// to 0x12345678 junk) are no longer parameters of ProcessDurableReconnectContext
// at all — the contract is that they are never consulted. These tests model that
// by persisting a distinct original GrantedAccess and asserting it is restored
// verbatim regardless of what the reconnect CREATE would have requested.
const junkFname = "__non_existing_fname__"

// persistReopen2Handle stores a durable handle as session A would on disconnect.
// origDA / origSA / origGranted model the access captured at the original CREATE.
func persistReopen2Handle(t *testing.T, store lock.DurableHandleStore, id string,
	fileID, createGuid, leaseKey [16]byte, oplock uint8, origDA, origSA, origGranted uint32) {
	t.Helper()
	h := &lock.PersistedDurableHandle{
		ID:             id,
		FileID:         fileID,
		Path:           "reopen2.dat",
		ShareName:      "/share1",
		DesiredAccess:  origDA,
		GrantedAccess:  origGranted,
		ShareAccess:    origSA,
		MetadataHandle: []byte{0xDE, 0xAD},
		Username:       "alice",
		SessionKeyHash: makeSessionKeyHash("orig-session-A"),
		OplockLevel:    oplock,
		LeaseKey:       leaseKey,
		CreateGuid:     createGuid,
		IsV2:           createGuid != [16]byte{},
		CreatedAt:      time.Now().Add(-5 * time.Minute),
		DisconnectedAt: time.Now().Add(-10 * time.Second),
		TimeoutMs:      60000,
	}
	if oplock == OplockLevelLease {
		h.LeaseState = uint32(lock.LeaseStateRead | lock.LeaseStateHandle)
	}
	if err := store.PutDurableHandle(context.Background(), h); err != nil {
		t.Fatalf("persist handle: %v", err)
	}
}

// assertReopen2OK asserts the reconnect succeeded and restored the ORIGINAL
// granted access — proving the junk DesiredAccess/ShareAccess were ignored.
func assertReopen2OK(t *testing.T, res *ReconnectResult, status types.Status, err error, wantGranted uint32) {
	t.Helper()
	if err != nil {
		t.Fatalf("reconnect error: %v", err)
	}
	if status != types.StatusSuccess {
		t.Fatalf("reconnect status = %s, want STATUS_SUCCESS", status)
	}
	if res == nil || res.OpenFile == nil {
		t.Fatal("expected restored OpenFile")
	}
	if res.OpenFile.GrantedAccess != wantGranted {
		t.Errorf("GrantedAccess = 0x%x, want original 0x%x (junk request access must be ignored)",
			res.OpenFile.GrantedAccess, wantGranted)
	}
	if res.OpenFile.Path != "reopen2.dat" {
		t.Errorf("Path = %q, want original reopen2.dat", res.OpenFile.Path)
	}
}

// freshSessionID is a SessionID distinct from session A's, modelling the new
// connection+session+tree the reopen2 tests reconnect from.
const freshSessionID uint64 = 0xFEED

// Mode 1: smb2.durable-open.reopen2 — V1 (DHnC) batch-oplock reconnect on a
// fresh session with junk DesiredAccess + ShareAccess + junk fname.
// Pre-fix failure: ACCESS_DENIED (durable_open.c:810). Expected: OK.
func TestReopen2_V1_Oplock_JunkFields(t *testing.T) {
	store := newMockDurableStore()
	freshKey := makeSessionKeyHash("fresh-session-B")
	fileID := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 0, 0, 0, 0, 0, 0, 0, 0}

	persistReopen2Handle(t, store, "dh-r2-v1", fileID, [16]byte{}, [16]byte{},
		OplockLevelBatch, 0x12019F, 0x07, 0x12019F)

	dhnCData := make([]byte, 16)
	copy(dhnCData, fileID[:])
	contexts := []CreateContext{{Name: DurableHandleV1ReconnectTag, Data: dhnCData}}

	res, status, err := ProcessDurableReconnectContext(
		context.Background(), store, nil, contexts,
		freshSessionID, "alice", freshKey, "/share1", junkFname, [16]byte{})
	assertReopen2OK(t, res, status, err, 0x12019F)
}

// Mode 2: smb2.durable-open.reopen2-lease — V1 (DHnC) lease reconnect on a
// fresh session, correct lease key, junk access fields, CORRECT fname (a lease
// reconnect path-checks). Pre-fix failure: OBJECT_NAME_NOT_FOUND
// (durable_open.c:1065). Expected: OK.
func TestReopen2_V1_Lease_JunkAccess(t *testing.T) {
	store := newMockDurableStore()
	freshKey := makeSessionKeyHash("fresh-session-B")
	fileID := [16]byte{2, 3, 4, 5, 6, 7, 8, 9, 0, 0, 0, 0, 0, 0, 0, 0}
	leaseKey := [16]byte{0xAA, 0xBB, 0xCC, 0xDD}

	persistReopen2Handle(t, store, "dh-r2-v1l", fileID, [16]byte{}, leaseKey,
		OplockLevelLease, 0x12019F, 0x07, 0x12019F)

	dhnCData := make([]byte, 16)
	copy(dhnCData, fileID[:])
	contexts := []CreateContext{
		{Name: DurableHandleV1ReconnectTag, Data: dhnCData},
		{Name: LeaseContextTagRequest, Data: encodeV1LeaseRequestContext(leaseKey, 0)},
	}

	res, status, err := ProcessDurableReconnectContext(
		context.Background(), store, nil, contexts,
		freshSessionID, "alice", freshKey, "/share1", "reopen2.dat", [16]byte{})
	assertReopen2OK(t, res, status, err, 0x12019F)
}

// Mode 3: smb2.durable-open.reopen2-lease-v2 — same as mode 2 but with a V2
// lease wire context. The handler gate is lease-key-only, so the V1/V2 wire
// distinction does not change the contract. Pre-fix failure:
// OBJECT_NAME_NOT_FOUND (durable_open.c:1302). Expected: OK.
func TestReopen2_V1_LeaseV2_JunkAccess(t *testing.T) {
	store := newMockDurableStore()
	freshKey := makeSessionKeyHash("fresh-session-B")
	fileID := [16]byte{3, 4, 5, 6, 7, 8, 9, 10, 0, 0, 0, 0, 0, 0, 0, 0}
	leaseKey := [16]byte{0xA1, 0xB2, 0xC3, 0xD4}

	persistReopen2Handle(t, store, "dh-r2-v1l2", fileID, [16]byte{}, leaseKey,
		OplockLevelLease, 0x12019F, 0x07, 0x12019F)

	dhnCData := make([]byte, 16)
	copy(dhnCData, fileID[:])
	contexts := []CreateContext{
		{Name: DurableHandleV1ReconnectTag, Data: dhnCData},
		{Name: LeaseContextTagRequest, Data: encodeV2LeaseContext(leaseKey, 0, 1)},
	}

	res, status, err := ProcessDurableReconnectContext(
		context.Background(), store, nil, contexts,
		freshSessionID, "alice", freshKey, "/share1", "reopen2.dat", [16]byte{})
	assertReopen2OK(t, res, status, err, 0x12019F)
}

// Mode 4: smb2.durable-v2-open.reopen2 — V2 (DH2C) batch-oplock reconnect on a
// fresh session with junk DesiredAccess + ShareAccess + JUNK fname. The junk
// fname is the V2-specific trap: pre-fix processV2Reconnect always path-checked
// -> INVALID_PARAMETER (durable_v2_open.c:1075). A non-lease V2 reconnect MUST
// ignore the fname. Expected: OK.
func TestReopen2_V2_Oplock_JunkFields(t *testing.T) {
	store := newMockDurableStore()
	freshKey := makeSessionKeyHash("fresh-session-B")
	fileID := [16]byte{4, 5, 6, 7, 8, 9, 10, 11, 0, 0, 0, 0, 0, 0, 0, 0}
	createGuid := [16]byte{0xC0, 0xC1, 0xC2, 0xC3, 0xC4, 0xC5, 0xC6, 0xC7, 0xC8, 0xC9, 0xCA, 0xCB, 0xCC, 0xCD, 0xCE, 0xCF}

	persistReopen2Handle(t, store, "dh-r2-v2", fileID, createGuid, [16]byte{},
		OplockLevelBatch, 0x120089, 0x01, 0x120089)

	dh2cData := make([]byte, 36)
	copy(dh2cData[0:16], fileID[:])
	copy(dh2cData[16:32], createGuid[:])
	contexts := []CreateContext{{Name: DurableHandleV2ReconnectTag, Data: dh2cData}}

	res, status, err := ProcessDurableReconnectContext(
		context.Background(), store, nil, contexts,
		freshSessionID, "alice", freshKey, "/share1", junkFname, [16]byte{})
	assertReopen2OK(t, res, status, err, 0x120089)
}

// Mode 5: smb2.durable-v2-open.reopen2-lease — V2 (DH2C) lease reconnect on a
// fresh session, correct lease key + create_guid, junk access, CORRECT fname.
// Pre-fix failure: ACCESS_DENIED (durable_v2_open.c:1477, the
// desired/share-access gate). Expected: OK.
func TestReopen2_V2_Lease_JunkAccess(t *testing.T) {
	store := newMockDurableStore()
	freshKey := makeSessionKeyHash("fresh-session-B")
	fileID := [16]byte{5, 6, 7, 8, 9, 10, 11, 12, 0, 0, 0, 0, 0, 0, 0, 0}
	createGuid := [16]byte{0xD0, 0xD1, 0xD2, 0xD3, 0xD4, 0xD5, 0xD6, 0xD7, 0xD8, 0xD9, 0xDA, 0xDB, 0xDC, 0xDD, 0xDE, 0xDF}
	leaseKey := [16]byte{0xE1, 0xE2, 0xE3, 0xE4}

	persistReopen2Handle(t, store, "dh-r2-v2l", fileID, createGuid, leaseKey,
		OplockLevelLease, 0x120089, 0x01, 0x120089)

	dh2cData := make([]byte, 36)
	copy(dh2cData[0:16], fileID[:])
	copy(dh2cData[16:32], createGuid[:])
	contexts := []CreateContext{
		{Name: DurableHandleV2ReconnectTag, Data: dh2cData},
		{Name: LeaseContextTagRequest, Data: encodeV1LeaseRequestContext(leaseKey, 0)},
	}

	res, status, err := ProcessDurableReconnectContext(
		context.Background(), store, nil, contexts,
		freshSessionID, "alice", freshKey, "/share1", "reopen2.dat", [16]byte{})
	assertReopen2OK(t, res, status, err, 0x120089)
}

// Mode 6: smb2.durable-v2-open.reopen2-lease-v2 — same as mode 5 but with a V2
// lease wire context. Pre-fix failure: ACCESS_DENIED (durable_v2_open.c:1728).
// Expected: OK.
func TestReopen2_V2_LeaseV2_JunkAccess(t *testing.T) {
	store := newMockDurableStore()
	freshKey := makeSessionKeyHash("fresh-session-B")
	fileID := [16]byte{6, 7, 8, 9, 10, 11, 12, 13, 0, 0, 0, 0, 0, 0, 0, 0}
	createGuid := [16]byte{0xF0, 0xF1, 0xF2, 0xF3, 0xF4, 0xF5, 0xF6, 0xF7, 0xF8, 0xF9, 0xFA, 0xFB, 0xFC, 0xFD, 0xFE, 0xFF}
	leaseKey := [16]byte{0x1A, 0x2B, 0x3C, 0x4D}

	persistReopen2Handle(t, store, "dh-r2-v2l2", fileID, createGuid, leaseKey,
		OplockLevelLease, 0x120089, 0x01, 0x120089)

	dh2cData := make([]byte, 36)
	copy(dh2cData[0:16], fileID[:])
	copy(dh2cData[16:32], createGuid[:])
	contexts := []CreateContext{
		{Name: DurableHandleV2ReconnectTag, Data: dh2cData},
		{Name: LeaseContextTagRequest, Data: encodeV2LeaseContext(leaseKey, 0, 1)},
	}

	res, status, err := ProcessDurableReconnectContext(
		context.Background(), store, nil, contexts,
		freshSessionID, "alice", freshKey, "/share1", "reopen2.dat", [16]byte{})
	assertReopen2OK(t, res, status, err, 0x120089)
}

// Mode 7: smb2.durable-open.reopen2-lease{,-v2} SECOND CYCLE — a V1
// (durable_open=true, CreateGuid=0) lease handle reconnected via the *DH2C*
// context with a ZERO CreateGuid. smbtorture's smb2_create sets
// io.in.durable_handle_v2 = h while h is a V1 handle, so the wire carries a
// DH2C blob whose CreateGuid is zero and whose FileID is the original handle.
// processV2Reconnect must fall back to a FileID-keyed lookup (the zero
// CreateGuid is unusable) — mirroring Samba's persistent-FileId-keyed
// smbd_smb2_create_durable_v2_reconnect. Pre-fix failure:
// OBJECT_NAME_NOT_FOUND (durable_open.c:1068 / reopen2-lease-v2:1305).
// Expected: OK with the original granted access restored.
func TestReopen2_V1ViaDH2C_ZeroCreateGuid(t *testing.T) {
	store := newMockDurableStore()
	freshKey := makeSessionKeyHash("fresh-session-B")
	// Full original FileID (persistent + volatile) as the client replays it.
	fileID := [16]byte{7, 8, 9, 10, 11, 12, 13, 14, 0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF, 0x11, 0x22}
	leaseKey := [16]byte{0x5A, 0x6B, 0x7C, 0x8D}

	// V1 handle: CreateGuid is zero, lease-backed. persistReopen2Handle stores
	// the volatile-zeroed FileID, matching buildPersistedDurableHandle.
	persistedFileID := fileID
	for i := 8; i < 16; i++ {
		persistedFileID[i] = 0
	}
	persistReopen2Handle(t, store, "dh-r2-v1-via-dh2c", persistedFileID, [16]byte{}, leaseKey,
		OplockLevelLease, 0x12019F, 0x07, 0x12019F)

	// DH2C blob: full original FileID, ZERO CreateGuid.
	dh2cData := make([]byte, 36)
	copy(dh2cData[0:16], fileID[:])
	// bytes 16:32 (CreateGuid) stay zero
	contexts := []CreateContext{
		{Name: DurableHandleV2ReconnectTag, Data: dh2cData},
		{Name: LeaseContextTagRequest, Data: encodeV1LeaseRequestContext(leaseKey, 0)},
	}

	res, status, err := ProcessDurableReconnectContext(
		context.Background(), store, nil, contexts,
		freshSessionID, "alice", freshKey, "/share1", "reopen2.dat", [16]byte{})
	assertReopen2OK(t, res, status, err, 0x12019F)
	if !res.IsV2 {
		t.Errorf("IsV2 = false, want true (reconnect arrived via DH2C context)")
	}

	// The handle must have been consumed by FileID — a second reconnect fails.
	res2, status2, _ := ProcessDurableReconnectContext(
		context.Background(), store, nil, contexts,
		freshSessionID, "alice", freshKey, "/share1", "reopen2.dat", [16]byte{})
	if status2 != types.StatusObjectNameNotFound || res2 != nil {
		t.Errorf("second reconnect status = %s (res=%v), want OBJECT_NAME_NOT_FOUND (handle consumed)",
			status2, res2)
	}
}

// TestReopen2_V2ZeroCreateGuid_WrongFileID confirms the zero-CreateGuid FileID
// fallback still rejects a reconnect whose replayed FileID does not match any
// persisted handle (so the fallback cannot resurrect an unrelated open).
func TestReopen2_V2ZeroCreateGuid_WrongFileID(t *testing.T) {
	store := newMockDurableStore()
	freshKey := makeSessionKeyHash("fresh-session-B")
	fileID := [16]byte{8, 9, 10, 11, 12, 13, 14, 15, 0, 0, 0, 0, 0, 0, 0, 0}
	leaseKey := [16]byte{0x6A, 0x7B, 0x8C, 0x9D}
	persistReopen2Handle(t, store, "dh-r2-wrongfid", fileID, [16]byte{}, leaseKey,
		OplockLevelLease, 0x12019F, 0x07, 0x12019F)

	wrongFileID := [16]byte{0xDE, 0xAD, 0xBE, 0xEF, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	dh2cData := make([]byte, 36)
	copy(dh2cData[0:16], wrongFileID[:])
	contexts := []CreateContext{
		{Name: DurableHandleV2ReconnectTag, Data: dh2cData},
		{Name: LeaseContextTagRequest, Data: encodeV1LeaseRequestContext(leaseKey, 0)},
	}

	res, status, _ := ProcessDurableReconnectContext(
		context.Background(), store, nil, contexts,
		freshSessionID, "alice", freshKey, "/share1", "reopen2.dat", [16]byte{})
	if status != types.StatusObjectNameNotFound || res != nil {
		t.Errorf("status = %s (res=%v), want OBJECT_NAME_NOT_FOUND for unmatched FileID", status, res)
	}
}

// TestReopen2_V2ZeroCreateGuid_RejectsV2Handle guards the reopen2/reopen2b
// negative ladder: a DH2C reconnect with a ZERO CreateGuid against a V2 handle
// (stored CreateGuid != 0) MUST fail OBJECT_NAME_NOT_FOUND. The zero-CreateGuid
// FileID fallback is only for V1 handles; matching a V2 handle by FileID alone
// would wrongly resurrect it (durable_v2_open.c:1070-1090 / reopen2b:1157).
func TestReopen2_V2ZeroCreateGuid_RejectsV2Handle(t *testing.T) {
	store := newMockDurableStore()
	freshKey := makeSessionKeyHash("fresh-session-B")
	fileID := [16]byte{9, 10, 11, 12, 13, 14, 15, 16, 0, 0, 0, 0, 0, 0, 0, 0}
	createGuid := [16]byte{0xAB, 0xAC, 0xAD, 0xAE, 0xAF, 0xB0, 0xB1, 0xB2, 0xB3, 0xB4, 0xB5, 0xB6, 0xB7, 0xB8, 0xB9, 0xBA}

	// V2 handle: stored CreateGuid is non-zero, batch-oplock (no lease).
	persistReopen2Handle(t, store, "dh-r2-v2-zeroguid", fileID, createGuid, [16]byte{},
		OplockLevelBatch, 0x120089, 0x01, 0x120089)

	// DH2C blob with the correct FileID but a ZERO CreateGuid.
	dh2cData := make([]byte, 36)
	copy(dh2cData[0:16], fileID[:])
	contexts := []CreateContext{{Name: DurableHandleV2ReconnectTag, Data: dh2cData}}

	res, status, _ := ProcessDurableReconnectContext(
		context.Background(), store, nil, contexts,
		freshSessionID, "alice", freshKey, "/share1", junkFname, [16]byte{})
	if status != types.StatusObjectNameNotFound || res != nil {
		t.Errorf("status = %s (res=%v), want OBJECT_NAME_NOT_FOUND (zero CreateGuid cannot reconnect a V2 handle)",
			status, res)
	}
}

// TestReopen2_NegativeLadder_Preserved guards the negative reconnect rungs the
// reopen2-lease tests also exercise, so the access-gate removal does not relax
// them: wrong lease key -> ONF, wrong-fname-WITH-lease -> INVALID_PARAMETER,
// empty/__non_existing__ fname WITHOUT lease on a lease-backed handle -> ONF.
func TestReopen2_NegativeLadder_Preserved(t *testing.T) {
	freshKey := makeSessionKeyHash("fresh-session-B")
	leaseKey := [16]byte{0x7A, 0x7B, 0x7C, 0x7D}
	createGuid := [16]byte{0x90, 0x91, 0x92, 0x93, 0x94, 0x95, 0x96, 0x97, 0x98, 0x99, 0x9A, 0x9B, 0x9C, 0x9D, 0x9E, 0x9F}

	newLeaseStore := func(id string) (lock.DurableHandleStore, [16]byte) {
		store := newMockDurableStore()
		fileID := [16]byte{8, 7, 6, 5, 4, 3, 2, 1, 0, 0, 0, 0, 0, 0, 0, 0}
		persistReopen2Handle(t, store, id, fileID, createGuid, leaseKey,
			OplockLevelLease, 0x12019F, 0x07, 0x12019F)
		return store, fileID
	}
	dh2c := func(fileID [16]byte) []byte {
		d := make([]byte, 36)
		copy(d[0:16], fileID[:])
		copy(d[16:32], createGuid[:])
		return d
	}

	t.Run("wrong_lease_key_ONF", func(t *testing.T) {
		store, fileID := newLeaseStore("nl-1")
		contexts := []CreateContext{
			{Name: DurableHandleV2ReconnectTag, Data: dh2c(fileID)},
			{Name: LeaseContextTagRequest, Data: encodeV1LeaseRequestContext([16]byte{0xDE, 0xAD}, 0)},
		}
		_, status, _ := ProcessDurableReconnectContext(context.Background(), store, nil, contexts,
			freshSessionID, "alice", freshKey, "/share1", "reopen2.dat", [16]byte{})
		if status != types.StatusObjectNameNotFound {
			t.Errorf("wrong lease key: got %s, want OBJECT_NAME_NOT_FOUND", status)
		}
	})

	t.Run("wrong_fname_with_lease_INVALID_PARAMETER", func(t *testing.T) {
		store, fileID := newLeaseStore("nl-2")
		contexts := []CreateContext{
			{Name: DurableHandleV2ReconnectTag, Data: dh2c(fileID)},
			{Name: LeaseContextTagRequest, Data: encodeV1LeaseRequestContext(leaseKey, 0)},
		}
		_, status, _ := ProcessDurableReconnectContext(context.Background(), store, nil, contexts,
			freshSessionID, "alice", freshKey, "/share1", junkFname, [16]byte{})
		if status != types.StatusInvalidParameter {
			t.Errorf("wrong fname with lease: got %s, want INVALID_PARAMETER", status)
		}
	})

	t.Run("lease_handle_no_lease_ctx_ONF", func(t *testing.T) {
		store, fileID := newLeaseStore("nl-3")
		// Lease-backed handle but reconnect omits the lease ctx -> ONF.
		contexts := []CreateContext{{Name: DurableHandleV2ReconnectTag, Data: dh2c(fileID)}}
		_, status, _ := ProcessDurableReconnectContext(context.Background(), store, nil, contexts,
			freshSessionID, "alice", freshKey, "/share1", "reopen2.dat", [16]byte{})
		if status != types.StatusObjectNameNotFound {
			t.Errorf("lease handle, no lease ctx: got %s, want OBJECT_NAME_NOT_FOUND", status)
		}
	})
}

// TestProcessAppInstanceId_ReleasesLocksOnPersistedHandle verifies that
// ProcessAppInstanceId releases byte-range locks held by a persisted
// (disconnected) durable handle when a new open with the same AppInstanceId
// displaces it. Before the fix the call was UnlockAllForSession(0) which is
// always a no-op for SMB locks (sessions never carry ID 0); the test fails
// before the fix and passes after.
func TestProcessAppInstanceId_ReleasesLocksOnPersistedHandle(t *testing.T) {
	h, rt, smbCtx, rootHandle, rootAuth := setupDaclTest(t)
	h.DurableStore = newMockDurableStore()
	metaSvc := rt.GetMetadataService()

	// Create the file that the persisted handle will reference and resolve its
	// metadata handle (the lock key).
	if _, _, err := metaSvc.CreateFile(rootAuth, rootHandle, "locked.txt",
		&metadata.FileAttr{Type: metadata.FileTypeRegular, Mode: 0o644}); err != nil {
		t.Fatalf("CreateFile: %v", err)
	}
	file, _, err := h.lookupCaseInsensitive(rootAuthCtx(), metaSvc, rootHandle, "locked.txt")
	if err != nil || file == nil {
		t.Fatalf("lookup locked.txt: file=%v err=%v", file, err)
	}
	fileHandle, err := metadata.EncodeFileHandle(file)
	if err != nil {
		t.Fatalf("EncodeFileHandle: %v", err)
	}

	// Build an OriginalFileID that will be used as the lock's OpenID key.
	origFileID := [16]byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
		0x09, 0x0A, 0x0B, 0x0C, 0x0D, 0x0E, 0x0F, 0x10}
	openID := fmt.Sprintf("%x", origFileID)

	// Acquire a byte-range lock as the disconnected open would have.
	fl := metadata.FileLock{
		SessionID:  42,
		OpenID:     openID,
		ClientID:   "smb:42",
		Offset:     0,
		Length:     1,
		Exclusive:  true,
		AcquiredAt: time.Now(),
	}
	if err := metaSvc.LockFile(rootAuthCtx(), fileHandle, fl); err != nil {
		t.Fatalf("LockFile: %v", err)
	}

	// Confirm the lock is recorded.
	lm, err := metaSvc.GetLockManagerForHandle(fileHandle)
	if err != nil {
		t.Fatalf("GetLockManagerForHandle: %v", err)
	}
	if len(lm.ListLocks(string(fileHandle))) == 0 {
		t.Fatal("setup: expected at least one lock before displacement")
	}

	// Build a persisted durable handle carrying the same AppInstanceId.
	appID := [16]byte{0xAA, 0xBB, 0xCC, 0xDD}
	// Volatile-zeroed FileID (as stored by buildPersistedDurableHandle).
	persistedFileID := [16]byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}
	mock := h.DurableStore.(*mockDurableStore)
	_ = mock.PutDurableHandle(context.Background(), &lock.PersistedDurableHandle{
		ID:             "test-handle",
		FileID:         persistedFileID,
		OriginalFileID: origFileID, // full FileID — lock owner key
		MetadataHandle: fileHandle,
		AppInstanceId:  appID,
		ShareName:      smbCtx.ShareName,
		Path:           "/locked.txt",
		DisconnectedAt: time.Now(),
		TimeoutMs:      60000,
	})

	// Build a new CREATE context carrying the same AppInstanceId.
	appIDBytes := make([]byte, 20)
	binary.LittleEndian.PutUint16(appIDBytes[0:2], 20) // StructureSize
	copy(appIDBytes[4:20], appID[:])
	newOpenCtxs := []CreateContext{
		{Name: AppInstanceIdTag, Data: appIDBytes},
	}

	// Call ProcessAppInstanceId — this should displace the persisted handle
	// and release its byte-range lock.
	ProcessAppInstanceId(context.Background(), h.DurableStore, h, newOpenCtxs)

	// The persisted handle must have been deleted.
	remaining, _ := mock.GetDurableHandle(context.Background(), "test-handle")
	if remaining != nil {
		t.Error("persisted handle should have been deleted by ProcessAppInstanceId")
	}

	// The byte-range lock must have been released.
	for _, l := range lm.ListLocks(string(fileHandle)) {
		if l.OpenID == openID {
			t.Errorf("lock under openID %q still present after ProcessAppInstanceId — UnlockAllForOpen not called correctly", openID)
		}
	}
}

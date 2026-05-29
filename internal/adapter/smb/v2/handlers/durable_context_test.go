package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
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

	resp := ProcessDurableHandleContext(contexts, openFile, 60000)
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

	resp := ProcessDurableHandleContext(contexts, openFile, 60000)
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

	resp := ProcessDurableHandleContext(contexts, openFile, 60000)
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

	resp := ProcessDurableHandleContext(contexts, openFile, 60000)
	if resp == nil {
		t.Fatal("Expected V2 grant response, got nil")
	}
	if openFile.DurableTimeoutMs != 60000 {
		t.Errorf("DurableTimeoutMs = %d, want 60000 (server default)", openFile.DurableTimeoutMs)
	}
}

func TestProcessDurableHandleContext_V2RejectPersistentFlag(t *testing.T) {
	openFile := &OpenFile{
		FileID:      [16]byte{1, 2, 3},
		OplockLevel: OplockLevelNone,
	}

	dh2qData := make([]byte, 32)
	binary.LittleEndian.PutUint32(dh2qData[0:4], 60000)
	binary.LittleEndian.PutUint32(dh2qData[4:8], DH2FlagPersistent) // Persistent flag
	copy(dh2qData[16:32], []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16})

	contexts := []CreateContext{
		{Name: DurableHandleV2RequestTag, Data: dh2qData},
	}

	resp := ProcessDurableHandleContext(contexts, openFile, 60000)
	if resp != nil {
		t.Error("Expected nil response when persistent flag is set (not supported)")
	}
	if openFile.IsDurable {
		t.Error("Expected openFile.IsDurable to be false")
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

	resp := ProcessDurableHandleContext(contexts, openFile, 60000)
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

	resp := ProcessDurableHandleContext(contexts, openFile, 60000)
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

	fileID := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
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
		context.Background(), store, nil, contexts, 999, "alice", keyHash, "/share1", "test.txt", [16]byte{}, 0, 0,
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

func TestProcessDurableReconnectContext_V2Success(t *testing.T) {
	store := newMockDurableStore()
	ctx := context.Background()

	fileID := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
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
		context.Background(), store, nil, contexts, 999, "bob", keyHash, "/share1", "report.docx", [16]byte{}, 0, 0,
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
		context.Background(), store, nil, contexts, 999, "alice", makeSessionKeyHash("key"), "/share1", "test.txt", [16]byte{}, 0, 0,
	)
	if status != types.StatusObjectNameNotFound {
		t.Errorf("Expected STATUS_OBJECT_NAME_NOT_FOUND, got %s", status)
	}
}

func TestProcessDurableReconnectContext_UsernameMismatch(t *testing.T) {
	store := newMockDurableStore()
	ctx := context.Background()

	fileID := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
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
		context.Background(), store, nil, contexts, 999, "eve", keyHash, "/share1", "test.txt", [16]byte{}, 0, 0,
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

	fileID := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
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
		context.Background(), store, nil, contexts, 999, "alice", differentKeyHash, "/share1", "test.txt", [16]byte{}, 0, 0,
	)
	if status != types.StatusSuccess {
		t.Errorf("Expected STATUS_SUCCESS for session key mismatch with matching username, got %s", status)
	}
}

func TestProcessDurableReconnectContext_ShareNameMismatch(t *testing.T) {
	store := newMockDurableStore()
	ctx := context.Background()

	fileID := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
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
		context.Background(), store, nil, contexts, 999, "alice", keyHash, "/different-share", "test.txt", [16]byte{}, 0, 0,
	)
	if status != types.StatusObjectNameNotFound {
		t.Errorf("Expected STATUS_OBJECT_NAME_NOT_FOUND for share mismatch, got %s", status)
	}
}

func TestProcessDurableReconnectContext_PathMismatch(t *testing.T) {
	store := newMockDurableStore()
	ctx := context.Background()

	createGuid := [16]byte{0xA0, 0xA1, 0xA2, 0xA3, 0xA4, 0xA5, 0xA6, 0xA7, 0xA8, 0xA9, 0xAA, 0xAB, 0xAC, 0xAD, 0xAE, 0xAF}
	fileID := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	keyHash := makeSessionKeyHash("key")

	_ = store.PutDurableHandle(ctx, &lock.PersistedDurableHandle{
		ID:             "dh-006",
		FileID:         fileID,
		Path:           "test.txt",
		ShareName:      "/share1",
		DesiredAccess:  0x12019F,
		ShareAccess:    0x07,
		MetadataHandle: []byte{0xDE, 0xAD},
		Username:       "alice",
		SessionKeyHash: keyHash,
		CreateGuid:     createGuid,
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

	_, status, _ := ProcessDurableReconnectContext(
		context.Background(), store, nil, contexts, 999, "alice", keyHash, "/share1", "other.txt", [16]byte{}, 0, 0,
	)
	if status != types.StatusInvalidParameter {
		t.Errorf("Expected STATUS_INVALID_PARAMETER for path mismatch, got %s", status)
	}
}

func TestProcessDurableReconnectContext_V1ConflictingV2Tag(t *testing.T) {
	store := newMockDurableStore()
	ctx := context.Background()

	fileID := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
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
		context.Background(), store, nil, contexts, 999, "alice", keyHash, "/share1", "test.txt", [16]byte{}, 0, 0,
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
		1, "alice", keyHash, "/share1", "lockfile.txt", [16]byte{}, 0, 0)
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
		1, "alice", keyHash, "/share1", "legacy.txt", [16]byte{}, 0, 0)
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
		1, "alice", keyHash, "/share1", "position.txt", [16]byte{}, 0, 0)
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
		LeaseKey:       leaseKey,
		ClientGUID:     originalGUID,
		IsV2:           true,
		CreatedAt:      time.Now().Add(-time.Minute),
		DisconnectedAt: time.Now().Add(-time.Second),
		TimeoutMs:      60000,
		MetadataHandle: []byte{0xAA},
	})

	// Build DH2C reconnect (FileID + CreateGuid + zero flags).
	dh2cData := make([]byte, 36)
	copy(dh2cData[:16], fileID[:])
	copy(dh2cData[16:32], createGuid[:])
	contexts := []CreateContext{
		{Name: DurableHandleV2ReconnectTag, Data: dh2cData},
	}

	// Wrong ClientGuid → OBJECT_NAME_NOT_FOUND
	_, status, err := ProcessDurableReconnectContext(ctx, store, nil, contexts,
		1, "alice", keyHash, "/share1", "leasefile.txt", differentGUID, 0, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != types.StatusObjectNameNotFound {
		t.Errorf("ClientGuid mismatch: got status %v, want OBJECT_NAME_NOT_FOUND", status)
	}

	// Original ClientGuid → success
	_, status, err = ProcessDurableReconnectContext(ctx, store, nil, contexts,
		1, "alice", keyHash, "/share1", "leasefile.txt", originalGUID, 0, 0)
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
		1, "alice", keyHash, "/share1", "oplockfile.txt", differentGUID, 0, 0)
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
		1, "alice", keyHash, "/share1", "leasefile.txt", originalGUID, 0, 0)
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
			resp := ProcessDurableHandleContext(contexts, openFile, 60000, c.handleLease)
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

	resp := ProcessDurableHandleContext(contexts, openFile, 60000)
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

	fileID := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
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
		ctx, store, nil, contexts, 1, "alice", keyHash, "/share1", "race.txt", [16]byte{}, 0, 0,
	)
	if err1 != nil || status1 != types.StatusSuccess || res1 == nil {
		t.Fatalf("first reconnect: status=%v err=%v res=%v", status1, err1, res1)
	}

	// Second reconnect for the same FileID must NOT succeed — the handle
	// has been atomically consumed; ObjectNameNotFound is returned.
	_, status2, err2 := ProcessDurableReconnectContext(
		ctx, store, nil, contexts, 2, "alice", keyHash, "/share1", "race.txt", [16]byte{}, 0, 0,
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

	fileID := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
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
		ctx, store, nil, contexts, 1, "alice", keyHash, "/share1", "race.txt", [16]byte{}, 0, 0,
	)
	if err1 != nil || status1 != types.StatusSuccess || res1 == nil {
		t.Fatalf("first reconnect: status=%v err=%v res=%v", status1, err1, res1)
	}

	_, status2, err2 := ProcessDurableReconnectContext(
		ctx, store, nil, contexts, 2, "alice", keyHash, "/share1", "race.txt", [16]byte{}, 0, 0,
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

	fileID := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
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
		"/share1", "__non_existing_fname__", [16]byte{}, 0, 0,
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

	fileID := [16]byte{2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17}
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
		"/share1", "leased.dat", [16]byte{}, 0, 0,
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

	fileID := [16]byte{3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18}
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
		"/share1", "leased.dat", [16]byte{}, 0, 0,
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

	fileID := [16]byte{4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19}
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
		"/share1", "plain.dat", [16]byte{}, 0, 0,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != types.StatusObjectNameNotFound {
		t.Errorf("Expected OBJECT_NAME_NOT_FOUND for lease ctx vs oplock-only persisted handle, got %s", status)
	}
}

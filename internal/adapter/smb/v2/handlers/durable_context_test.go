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
	openFile := &OpenFile{
		FileID:      [16]byte{1, 2, 3},
		OplockLevel: OplockLevelNone,
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
		FileID:      [16]byte{1, 2, 3},
		OplockLevel: OplockLevelNone,
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
		context.Background(), store, nil, contexts, 999, "alice", keyHash, "/share1", "test.txt",
	)
	if err != nil {
		t.Fatalf("ProcessDurableReconnectContext error: %v", err)
	}
	if status != types.StatusSuccess {
		t.Fatalf("Expected STATUS_SUCCESS, got %s", status)
	}
	if restored == nil {
		t.Fatal("Expected restored OpenFile, got nil")
	}
	if restored.Path != "test.txt" {
		t.Errorf("Path = %s, want test.txt", restored.Path)
	}
	if restored.DesiredAccess != 0x12019F {
		t.Errorf("DesiredAccess = 0x%x, want 0x12019F", restored.DesiredAccess)
	}
	if restored.OplockLevel != OplockLevelBatch {
		t.Errorf("OplockLevel = %d, want %d", restored.OplockLevel, OplockLevelBatch)
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
		context.Background(), store, nil, contexts, 999, "bob", keyHash, "/share1", "report.docx",
	)
	if err != nil {
		t.Fatalf("ProcessDurableReconnectContext error: %v", err)
	}
	if status != types.StatusSuccess {
		t.Fatalf("Expected STATUS_SUCCESS, got %s", status)
	}
	if restored == nil {
		t.Fatal("Expected restored OpenFile, got nil")
	}
	if restored.Path != "report.docx" {
		t.Errorf("Path = %s, want report.docx", restored.Path)
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
		context.Background(), store, nil, contexts, 999, "alice", makeSessionKeyHash("key"), "/share1", "test.txt",
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
		context.Background(), store, nil, contexts, 999, "eve", keyHash, "/share1", "test.txt",
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
		context.Background(), store, nil, contexts, 999, "alice", differentKeyHash, "/share1", "test.txt",
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
		context.Background(), store, nil, contexts, 999, "alice", keyHash, "/different-share", "test.txt",
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
		context.Background(), store, nil, contexts, 999, "alice", keyHash, "/share1", "other.txt",
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
		context.Background(), store, nil, contexts, 999, "alice", keyHash, "/share1", "test.txt",
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

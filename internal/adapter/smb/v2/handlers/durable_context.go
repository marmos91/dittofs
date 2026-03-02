package handlers

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/marmos91/dittofs/internal/adapter/smb/smbenc"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// Durable Handle Create Context tag constants [MS-SMB2] 2.2.13.2.
const (
	DurableHandleV1RequestTag          = "DHnQ"             // SMB2_CREATE_DURABLE_HANDLE_REQUEST
	DurableHandleV1ReconnectTag        = "DHnC"             // SMB2_CREATE_DURABLE_HANDLE_RECONNECT (also V1 response tag)
	DurableHandleV2RequestTag          = "DH2Q"             // SMB2_CREATE_DURABLE_HANDLE_REQUEST_V2 (also V2 response tag)
	DurableHandleV2ReconnectTag        = "DH2C"             // SMB2_CREATE_DURABLE_HANDLE_RECONNECT_V2
	AppInstanceIdTag                   = "\x45\x17\xb6\x11" // SMB2_CREATE_APP_INSTANCE_ID
	DH2FlagPersistent           uint32 = 0x00000002         // Persistent handle (not supported)
)

// DecodeDHnQRequest validates a V1 durable handle request (DHnQ).
// The data is 16 bytes of reserved fields (all zeros, ignored by server).
// [MS-SMB2] 2.2.13.2.3
func DecodeDHnQRequest(data []byte) error {
	if len(data) < 16 {
		return fmt.Errorf("DHnQ request too short: %d bytes", len(data))
	}
	// DurableRequest (16 bytes): MUST be zero, server ignores
	return nil
}

// DecodeDHnCReconnect parses a V1 durable handle reconnect (DHnC).
// Returns the 16-byte FileID from the original CREATE response.
// [MS-SMB2] 2.2.13.2.4
func DecodeDHnCReconnect(data []byte) ([16]byte, error) {
	if len(data) < 16 {
		return [16]byte{}, fmt.Errorf("DHnC reconnect too short: %d bytes", len(data))
	}
	var fileID [16]byte
	copy(fileID[:], data[:16])
	return fileID, nil
}

// DecodeDH2QRequest parses a V2 durable handle request (DH2Q).
// Returns timeout (ms), flags, and CreateGuid.
// [MS-SMB2] 2.2.13.2.11
//
// Wire format (32 bytes):
//
//	Offset 0:  Timeout (4 bytes) - milliseconds, 0 = use server default
//	Offset 4:  Flags (4 bytes) - 0x02 = persistent (we reject this)
//	Offset 8:  Reserved (8 bytes) - must be zero
//	Offset 16: CreateGuid (16 bytes) - client-generated GUID
func DecodeDH2QRequest(data []byte) (timeout uint32, flags uint32, createGuid [16]byte, err error) {
	if len(data) < 32 {
		return 0, 0, [16]byte{}, fmt.Errorf("DH2Q request too short: %d bytes", len(data))
	}
	r := smbenc.NewReader(data)
	timeout = r.ReadUint32()
	flags = r.ReadUint32()
	r.Skip(8) // Reserved
	copy(createGuid[:], data[16:32])
	return timeout, flags, createGuid, r.Err()
}

// DecodeDH2CReconnect parses a V2 durable handle reconnect (DH2C).
// Returns fileID, createGuid, and flags.
// [MS-SMB2] 2.2.13.2.12
//
// Wire format (36 bytes):
//
//	Offset 0:  FileId (16 bytes) - SMB2_FILEID for the open being reestablished
//	Offset 16: CreateGuid (16 bytes) - must match the original DH2Q CreateGuid
//	Offset 32: Flags (4 bytes) - 0x02 = persistent (we reject this)
func DecodeDH2CReconnect(data []byte) (fileID [16]byte, createGuid [16]byte, flags uint32, err error) {
	if len(data) < 36 {
		return [16]byte{}, [16]byte{}, 0, fmt.Errorf("DH2C reconnect too short: %d bytes", len(data))
	}
	copy(fileID[:], data[:16])
	copy(createGuid[:], data[16:32])
	r := smbenc.NewReader(data[32:])
	flags = r.ReadUint32()
	return fileID, createGuid, flags, r.Err()
}

// DecodeAppInstanceId parses an SMB2_CREATE_APP_INSTANCE_ID context.
// Returns the 16-byte AppInstanceId.
// [MS-SMB2] 2.2.13.2.13
//
// Wire format (20 bytes):
//
//	Offset 0:  StructureSize (2 bytes) - must be 20
//	Offset 2:  Reserved (2 bytes) - must be zero
//	Offset 4:  AppInstanceId (16 bytes) - unique application instance ID
func DecodeAppInstanceId(data []byte) ([16]byte, error) {
	if len(data) < 20 {
		return [16]byte{}, fmt.Errorf("AppInstanceId too short: %d bytes", len(data))
	}
	r := smbenc.NewReader(data)
	structSize := r.ReadUint16()
	if structSize != 20 {
		return [16]byte{}, fmt.Errorf("AppInstanceId invalid structure size: %d", structSize)
	}
	r.Skip(2) // Reserved
	var appId [16]byte
	copy(appId[:], data[4:20])
	return appId, r.Err()
}

// EncodeDHnQResponse creates the V1 durable handle grant response context.
// Response tag is "DHnC" with 8 bytes of zeros.
// [MS-SMB2] 2.2.14.2.3
func EncodeDHnQResponse() CreateContext {
	return CreateContext{
		Name: DurableHandleV1ReconnectTag, // Response tag for V1
		Data: make([]byte, 8),             // Reserved, all zeros
	}
}

// EncodeDH2QResponse creates the V2 durable handle grant response context.
// Response tag is "DH2Q" with granted timeout and flags.
// [MS-SMB2] 2.2.14.2.12
func EncodeDH2QResponse(timeoutMs uint32, flags uint32) CreateContext {
	w := smbenc.NewWriter(8)
	w.WriteUint32(timeoutMs)
	w.WriteUint32(flags)
	return CreateContext{
		Name: DurableHandleV2RequestTag, // Server echoes same tag per spec
		Data: w.Bytes(),
	}
}

// ProcessDurableHandleContext processes DHnQ or DH2Q create contexts from a CREATE request.
// V2 (DH2Q) takes precedence over V1 (DHnQ) when both are present.
// Returns a response CreateContext to include in the CREATE response, or nil if
// durability was not granted. Mutates openFile (IsDurable, CreateGuid, DurableTimeoutMs).
func ProcessDurableHandleContext(
	contexts []CreateContext,
	openFile *OpenFile,
	configuredTimeoutMs uint32,
) *CreateContext {
	// Check for DH2Q first (V2 takes precedence over V1)
	if dh2qCtx := FindCreateContext(contexts, DurableHandleV2RequestTag); dh2qCtx != nil {
		timeout, flags, createGuid, err := DecodeDH2QRequest(dh2qCtx.Data)
		if err != nil {
			logger.Debug("ProcessDurableHandleContext: invalid DH2Q", "error", err)
			return nil
		}

		// Reject persistent flag (not supported)
		if flags&DH2FlagPersistent != 0 {
			logger.Debug("ProcessDurableHandleContext: persistent flag rejected (not supported)")
			return nil
		}

		// Calculate granted timeout: min(requested, configured max), 0 = server default
		grantedTimeout := configuredTimeoutMs
		if timeout > 0 && timeout < configuredTimeoutMs {
			grantedTimeout = timeout
		}

		// Grant V2 durability
		openFile.IsDurable = true
		openFile.CreateGuid = createGuid
		openFile.DurableTimeoutMs = grantedTimeout

		logger.Debug("ProcessDurableHandleContext: V2 durable handle granted",
			"createGuid", fmt.Sprintf("%x", createGuid),
			"requestedTimeout", timeout,
			"grantedTimeout", grantedTimeout)

		resp := EncodeDH2QResponse(grantedTimeout, 0)
		return &resp
	}

	if dhnqCtx := FindCreateContext(contexts, DurableHandleV1RequestTag); dhnqCtx != nil {
		if err := DecodeDHnQRequest(dhnqCtx.Data); err != nil {
			logger.Debug("ProcessDurableHandleContext: invalid DHnQ", "error", err)
			return nil
		}

		// V1 requires batch oplock (0x09) to grant durability
		if openFile.OplockLevel != OplockLevelBatch {
			logger.Debug("ProcessDurableHandleContext: V1 rejected (oplock not Batch)",
				"oplockLevel", openFile.OplockLevel)
			return nil
		}

		// Grant V1 durability
		openFile.IsDurable = true
		openFile.DurableTimeoutMs = configuredTimeoutMs

		logger.Debug("ProcessDurableHandleContext: V1 durable handle granted",
			"timeout", configuredTimeoutMs)

		resp := EncodeDHnQResponse()
		return &resp
	}

	// Neither DHnQ nor DH2Q present
	return nil
}

// ProcessDurableReconnectContext processes DHnC or DH2C create contexts for reconnection.
// It looks up the persisted handle, validates all reconnect conditions per MS-SMB2,
// and on success returns a restored OpenFile. On failure, returns a specific NTSTATUS code.
func ProcessDurableReconnectContext(
	ctx context.Context,
	durableStore lock.DurableHandleStore,
	metaSvc *metadata.MetadataService,
	contexts []CreateContext,
	sessionID uint64,
	username string,
	sessionKeyHash [32]byte,
	shareName string,
	filename string,
) (*OpenFile, types.Status, error) {

	// Determine V2 (DH2C) or V1 (DHnC) reconnect
	if dh2cCtx := FindCreateContext(contexts, DurableHandleV2ReconnectTag); dh2cCtx != nil {
		return processV2Reconnect(ctx, durableStore, metaSvc, contexts, dh2cCtx,
			sessionID, username, sessionKeyHash, shareName, filename)
	}

	if dhnCCtx := FindCreateContext(contexts, DurableHandleV1ReconnectTag); dhnCCtx != nil {
		return processV1Reconnect(ctx, durableStore, metaSvc, contexts, dhnCCtx,
			sessionID, username, sessionKeyHash, shareName, filename)
	}

	// No reconnect context found
	return nil, types.StatusInvalidParameter, fmt.Errorf("no reconnect context found")
}

// processV1Reconnect handles V1 (DHnC) reconnect validation and restoration.
func processV1Reconnect(
	ctx context.Context,
	durableStore lock.DurableHandleStore,
	metaSvc *metadata.MetadataService,
	contexts []CreateContext,
	dhnCCtx *CreateContext,
	sessionID uint64,
	username string,
	sessionKeyHash [32]byte,
	shareName string,
	filename string,
) (*OpenFile, types.Status, error) {
	// Parse V1 reconnect context
	fileID, err := DecodeDHnCReconnect(dhnCCtx.Data)
	if err != nil {
		logger.Debug("processV1Reconnect: invalid DHnC data", "error", err)
		return nil, types.StatusInvalidParameter, nil
	}

	logger.Debug("processV1Reconnect: starting validation",
		"fileID", fmt.Sprintf("%x", fileID),
		"username", username,
		"shareName", shareName,
		"filename", filename)

	// Reject conflicting V2 contexts alongside V1 reconnect
	if FindCreateContext(contexts, DurableHandleV2RequestTag) != nil ||
		FindCreateContext(contexts, DurableHandleV2ReconnectTag) != nil {
		logger.Debug("processV1Reconnect: check 2 FAIL - conflicting V2 context present")
		return nil, types.StatusInvalidParameter, nil
	}

	// Look up persisted handle by FileID
	handle, err := durableStore.GetDurableHandleByFileID(ctx, fileID)
	if err != nil {
		logger.Warn("processV1Reconnect: store error", "error", err)
		return nil, types.StatusInternalError, err
	}
	if handle == nil {
		logger.Debug("processV1Reconnect: check 3 FAIL - handle not found by FileID",
			"fileID", fmt.Sprintf("%x", fileID))
		return nil, types.StatusObjectNameNotFound, nil
	}

	// V1 reconnect does not carry DesiredAccess/ShareAccess in the context
	return validateAndRestore(ctx, durableStore, metaSvc, handle, sessionID, username,
		sessionKeyHash, shareName, filename, 0, 0)
}

// processV2Reconnect handles V2 (DH2C) reconnect validation and restoration.
func processV2Reconnect(
	ctx context.Context,
	durableStore lock.DurableHandleStore,
	metaSvc *metadata.MetadataService,
	contexts []CreateContext,
	dh2cCtx *CreateContext,
	sessionID uint64,
	username string,
	sessionKeyHash [32]byte,
	shareName string,
	filename string,
) (*OpenFile, types.Status, error) {
	// Parse V2 reconnect context
	fileID, createGuid, flags, err := DecodeDH2CReconnect(dh2cCtx.Data)
	if err != nil {
		logger.Debug("processV2Reconnect: invalid DH2C data", "error", err)
		return nil, types.StatusInvalidParameter, nil
	}

	// Reject persistent flag
	if flags&DH2FlagPersistent != 0 {
		logger.Debug("processV2Reconnect: persistent flag rejected")
		return nil, types.StatusInvalidParameter, nil
	}

	logger.Debug("processV2Reconnect: starting validation",
		"createGuid", fmt.Sprintf("%x", createGuid),
		"username", username,
		"shareName", shareName,
		"filename", filename)

	// Look up persisted handle by CreateGuid
	handle, err := durableStore.GetDurableHandleByCreateGuid(ctx, createGuid)
	if err != nil {
		logger.Warn("processV2Reconnect: store error", "error", err)
		return nil, types.StatusInternalError, err
	}
	if handle == nil {
		logger.Debug("processV2Reconnect: handle not found by CreateGuid",
			"createGuid", fmt.Sprintf("%x", createGuid))
		return nil, types.StatusObjectNameNotFound, nil
	}

	// Validate FileID from DH2C against persisted handle to prevent wrong-handle reconnect
	if fileID != handle.FileID {
		logger.Debug("processV2Reconnect: FileID mismatch",
			"expected", fmt.Sprintf("%x", handle.FileID),
			"actual", fmt.Sprintf("%x", fileID))
		return nil, types.StatusInvalidParameter, nil
	}

	// V2 reconnect does not carry DesiredAccess/ShareAccess in DH2C context either
	return validateAndRestore(ctx, durableStore, metaSvc, handle, sessionID, username,
		sessionKeyHash, shareName, filename, 0, 0)
}

// validateAndRestore runs the shared reconnect validation checks and restores the OpenFile.
// These checks apply to both V1 and V2 reconnects.
// desiredAccess and shareAccess are from the CREATE request; zero means "not provided"
// (V1 reconnect does not include these in the context).
func validateAndRestore(
	ctx context.Context,
	durableStore lock.DurableHandleStore,
	metaSvc *metadata.MetadataService,
	handle *lock.PersistedDurableHandle,
	sessionID uint64,
	username string,
	sessionKeyHash [32]byte,
	shareName string,
	filename string,
	desiredAccess uint32,
	shareAccess uint32,
) (*OpenFile, types.Status, error) {
	if handle.ShareName != shareName {
		logger.Debug("validateAndRestore: share name mismatch",
			"expected", handle.ShareName,
			"actual", shareName)
		return nil, types.StatusObjectNameNotFound, nil
	}

	if handle.Path != filename {
		logger.Debug("validateAndRestore: path mismatch",
			"expected", handle.Path,
			"actual", filename)
		return nil, types.StatusInvalidParameter, nil
	}

	if handle.Username != username {
		logger.Debug("validateAndRestore: username mismatch",
			"expected", handle.Username,
			"actual", username)
		return nil, types.StatusAccessDenied, nil
	}

	if handle.SessionKeyHash != sessionKeyHash {
		if handle.IsV2 {
			logger.Warn("validateAndRestore: session key hash mismatch on V2 handle (may be post-restart)",
				"username", username,
				"handleID", handle.ID)
		}
		logger.Debug("validateAndRestore: session key hash mismatch")
		return nil, types.StatusAccessDenied, nil
	}

	// Per [MS-SMB2] 3.3.5.9.9: reject reconnect if DesiredAccess or ShareAccess
	// differs from the persisted values to prevent privilege escalation.
	if desiredAccess != 0 && handle.DesiredAccess != 0 && desiredAccess != handle.DesiredAccess {
		logger.Debug("validateAndRestore: desired access mismatch",
			"persisted", fmt.Sprintf("0x%08x", handle.DesiredAccess),
			"requested", fmt.Sprintf("0x%08x", desiredAccess))
		return nil, types.StatusAccessDenied, nil
	}
	if shareAccess != 0 && handle.ShareAccess != 0 && shareAccess != handle.ShareAccess {
		logger.Debug("validateAndRestore: share access mismatch",
			"persisted", fmt.Sprintf("0x%08x", handle.ShareAccess),
			"requested", fmt.Sprintf("0x%08x", shareAccess))
		return nil, types.StatusAccessDenied, nil
	}

	expiresAt := handle.DisconnectedAt.Add(time.Duration(handle.TimeoutMs) * time.Millisecond)
	if !expiresAt.After(time.Now()) {
		logger.Debug("validateAndRestore: handle expired",
			"disconnectedAt", handle.DisconnectedAt,
			"timeoutMs", handle.TimeoutMs,
			"expiresAt", expiresAt)
		// Clean up expired handle
		_ = durableStore.DeleteDurableHandle(ctx, handle.ID)
		return nil, types.StatusObjectNameNotFound, nil
	}

	if metaSvc != nil && len(handle.MetadataHandle) > 0 {
		_, getErr := metaSvc.GetFile(ctx, handle.MetadataHandle)
		if getErr != nil {
			logger.Debug("validateAndRestore: file no longer exists",
				"path", handle.Path,
				"error", getErr)
			_ = durableStore.DeleteDurableHandle(ctx, handle.ID)
			return nil, types.StatusObjectNameNotFound, nil
		}
	}

	logger.Debug("validateAndRestore: all checks passed, restoring open file",
		"handleID", handle.ID,
		"path", handle.Path,
		"shareName", handle.ShareName)

	restored := &OpenFile{
		FileID:         handle.FileID,
		SessionID:      sessionID,
		Path:           handle.Path,
		ShareName:      handle.ShareName,
		DesiredAccess:  handle.DesiredAccess,
		MetadataHandle: handle.MetadataHandle,
		PayloadID:      metadata.PayloadID(handle.PayloadID),
		ShareAccess:    handle.ShareAccess,
		CreateOptions:  types.CreateOptions(handle.CreateOptions),
		OplockLevel:    handle.OplockLevel,
		OpenTime:       handle.CreatedAt,
		// IsDurable is NOT set on restore -- client must re-request durability
	}

	// Delete persisted handle (reconnect consumes it)
	if err := durableStore.DeleteDurableHandle(ctx, handle.ID); err != nil {
		logger.Warn("validateAndRestore: failed to delete persisted handle", "error", err)
		// Non-fatal: continue with reconnect
	}

	return restored, types.StatusSuccess, nil
}

// ProcessAppInstanceId processes the SMB2_CREATE_APP_INSTANCE_ID context.
// If present, it looks up existing durable handles with the same AppInstanceId
// and force-closes them (Hyper-V failover pattern).
//
// Returns the parsed AppInstanceId (zero value if not present).
func ProcessAppInstanceId(
	ctx context.Context,
	durableStore lock.DurableHandleStore,
	handler *Handler,
	contexts []CreateContext,
) [16]byte {
	appCtx := FindCreateContext(contexts, AppInstanceIdTag)
	if appCtx == nil {
		return [16]byte{}
	}

	appId, err := DecodeAppInstanceId(appCtx.Data)
	if err != nil {
		logger.Debug("ProcessAppInstanceId: invalid context data", "error", err)
		return [16]byte{}
	}

	// Zero AppInstanceId means "not set"
	if appId == ([16]byte{}) {
		return [16]byte{}
	}

	// Look up existing handles with this AppInstanceId
	existing, err := durableStore.GetDurableHandlesByAppInstanceId(ctx, appId)
	if err != nil {
		logger.Warn("ProcessAppInstanceId: store error", "error", err)
		return appId
	}

	if len(existing) == 0 {
		return appId
	}

	logger.Debug("ProcessAppInstanceId: force-closing existing handles",
		"appInstanceId", fmt.Sprintf("%x", appId),
		"count", len(existing))

	// Force-close each existing handle
	for _, h := range existing {
		// If handler is available, perform full cleanup (release locks, flush caches)
		if handler != nil {
			// Build minimal OpenFile for cleanup
			cleanupFile := &OpenFile{
				FileID:         h.FileID,
				Path:           h.Path,
				ShareName:      h.ShareName,
				MetadataHandle: h.MetadataHandle,
				PayloadID:      metadata.PayloadID(h.PayloadID),
			}

			// Flush cache
			handler.flushFileCache(ctx, cleanupFile)

			// Release locks if metadata handle is valid
			if len(h.MetadataHandle) > 0 {
				metaSvc := handler.Registry.GetMetadataService()
				_ = metaSvc.UnlockAllForSession(ctx, h.MetadataHandle, 0)
			}
		}

		// Delete from durable store
		if delErr := durableStore.DeleteDurableHandle(ctx, h.ID); delErr != nil {
			logger.Warn("ProcessAppInstanceId: failed to delete handle",
				"handleID", h.ID,
				"error", delErr)
		} else {
			logger.Debug("ProcessAppInstanceId: force-closed handle",
				"handleID", h.ID,
				"path", h.Path)
		}
	}

	return appId
}

// buildPersistedDurableHandle creates a PersistedDurableHandle from an OpenFile
// and session information. Used when persisting durable handles during disconnect.
func buildPersistedDurableHandle(
	openFile *OpenFile,
	username string,
	sessionKeyHash [32]byte,
	serverStartTime time.Time,
) *lock.PersistedDurableHandle {
	// Clone MetadataHandle to avoid aliasing the live OpenFile's slice
	metaHandle := make([]byte, len(openFile.MetadataHandle))
	copy(metaHandle, openFile.MetadataHandle)

	return &lock.PersistedDurableHandle{
		ID:              uuid.New().String(),
		FileID:          openFile.FileID,
		Path:            openFile.Path,
		ShareName:       openFile.ShareName,
		DesiredAccess:   openFile.DesiredAccess,
		ShareAccess:     openFile.ShareAccess,
		CreateOptions:   uint32(openFile.CreateOptions),
		MetadataHandle:  metaHandle,
		PayloadID:       string(openFile.PayloadID),
		OplockLevel:     openFile.OplockLevel,
		CreateGuid:      openFile.CreateGuid,
		AppInstanceId:   openFile.AppInstanceId,
		Username:        username,
		SessionKeyHash:  sessionKeyHash,
		IsV2:            openFile.CreateGuid != [16]byte{},
		CreatedAt:       openFile.OpenTime,
		DisconnectedAt:  time.Now(),
		TimeoutMs:       openFile.DurableTimeoutMs,
		ServerStartTime: serverStartTime,
	}
}

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

// zeroVolatileHalf returns fileID with the volatile half (bytes 8-15) cleared.
// Durable handles are persisted keyed on the persistent half only (MS-SMB2
// §3.2.4.4: the client sends Data.Volatile=0 on reconnect), but smbtorture
// replays the full original FileID — so the reconnect lookup must zero the
// volatile half to match the stored key. Shared by the V1 (DHnC) and the
// V1-via-DH2C zero-CreateGuid reconnect paths.
func zeroVolatileHalf(fileID [16]byte) [16]byte {
	for i := 8; i < 16; i++ {
		fileID[i] = 0
	}
	return fileID
}

// ValidateDurableContexts checks the durable-handle context combination rules
// from MS-SMB2 §3.3.5.9.6/7/11/12 (mirrors Samba `source3/smbd/smb2_create.c`
// `smbd_smb2_create_send` ~lines 775-846). Returns a non-success status when
// the combination is invalid:
//
//   - DHnC + DH2C, DHnC + DH2Q, DH2C + DHnQ, DH2Q + DH2C (any pair of these
//     four) → STATUS_INVALID_PARAMETER
//   - DH2C with wrong length (≠ 36) → STATUS_INVALID_PARAMETER
//
// The "extra unexpected blobs alongside reconnect → OBJECT_NAME_NOT_FOUND"
// rule is intentionally NOT enforced here: smbtorture create-blob does not
// exercise it, and our broader CREATE pipeline does not yet account for the
// full set of allowed companion contexts.
func ValidateDurableContexts(contexts []CreateContext) types.Status {
	dhnq := FindCreateContext(contexts, DurableHandleV1RequestTag)
	dhnc := FindCreateContext(contexts, DurableHandleV1ReconnectTag)
	dh2q := FindCreateContext(contexts, DurableHandleV2RequestTag)
	dh2c := FindCreateContext(contexts, DurableHandleV2ReconnectTag)

	if (dhnc != nil && dh2c != nil) ||
		(dhnc != nil && dh2q != nil) ||
		(dh2c != nil && dhnq != nil) ||
		(dh2q != nil && dh2c != nil) {
		return types.StatusInvalidParameter
	}

	if dh2c != nil && len(dh2c.Data) != 36 {
		return types.StatusInvalidParameter
	}

	return types.StatusSuccess
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
// Per MS-SMB2 2.2.14.2.3, the response uses the same tag "DHnQ" as the
// request, with 8 bytes of reserved (zero) data.
func EncodeDHnQResponse() CreateContext {
	return CreateContext{
		Name: DurableHandleV1RequestTag, // Response echoes request tag "DHnQ"
		Data: make([]byte, 8),           // Reserved, all zeros
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
		Name: DurableHandleV2RequestTag, // Server echoes same tag per MS-SMB2
		Data: w.Bytes(),
	}
}

// ProcessDurableHandleContext processes DHnQ or DH2Q create contexts from a CREATE request.
// V2 (DH2Q) takes precedence over V1 (DHnQ) when both are present.
// Returns a response CreateContext to include in the CREATE response, or nil if
// durability was not granted. Mutates openFile (IsDurable, CreateGuid, DurableTimeoutMs).
//
// leaseIncludesHandle indicates whether the granted lease includes Handle (H) caching.
// Per MS-SMB2 3.3.5.9.8, V1 durability can be granted when OplockLevel is Batch OR
// when the lease includes SMB2_LEASE_HANDLE_CACHING.
func ProcessDurableHandleContext(
	contexts []CreateContext,
	openFile *OpenFile,
	configuredTimeoutMs uint32,
	leaseIncludesHandle ...bool,
) *CreateContext {
	// Check for DH2Q first (V2 takes precedence over V1)
	if dh2qCtx := FindCreateContext(contexts, DurableHandleV2RequestTag); dh2qCtx != nil {
		timeout, flags, createGuid, err := DecodeDH2QRequest(dh2qCtx.Data)
		if err != nil {
			logger.Debug("ProcessDurableHandleContext: invalid DH2Q", "error", err)
			return nil
		}

		// Per MS-SMB2 §2.2.13.2.11: CreateGuid MUST be a valid GUID; an
		// all-zero CreateGuid is not a valid identifier and cannot key the
		// reconnect lookup (it would also collide with the "no CreateGuid"
		// sentinel in storage). Reject without granting durability —
		// matches Samba's `smbd_smb2_create_durable_v2_check` which treats
		// a NULL/zero create_guid as "do not grant V2".
		if createGuid == ([16]byte{}) {
			logger.Debug("ProcessDurableHandleContext: V2 rejected (zero CreateGuid)")
			return nil
		}

		// Reject persistent flag (not supported)
		if flags&DH2FlagPersistent != 0 {
			logger.Debug("ProcessDurableHandleContext: persistent flag rejected (not supported)")
			return nil
		}

		// Per MS-SMB2 §3.3.5.9.10: V2 durability MUST NOT be granted unless
		// either OplockLevel is Batch (legacy oplock-backed durable) or the
		// granted lease includes SMB2_LEASE_HANDLE_CACHING. Matches Samba
		// `smbd_smb2_create_durable_lease_check`. smbtorture
		// smb2.durable-v2-open.open-oplock iterates 8 share-mode × 4 oplock-
		// level combinations and expects `out.durable_open_v2 == false` for
		// every non-Batch row — granting V2 unconditionally trips those
		// assertions (line 293 / 455).
		hasHandle := len(leaseIncludesHandle) > 0 && leaseIncludesHandle[0]
		if openFile.OplockLevel != OplockLevelBatch && !hasHandle {
			logger.Debug("ProcessDurableHandleContext: V2 rejected (no Batch oplock or Handle lease)",
				"oplockLevel", openFile.OplockLevel,
				"hasHandleLease", hasHandle)
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

		// V1 requires batch oplock (0x09) or a lease with Handle caching to
		// grant durability. Per MS-SMB2 3.3.5.9.8: "If the open supports
		// leasing, the server SHOULD grant a durable handle if
		// Open.Lease.LeaseState includes SMB2_LEASE_HANDLE_CACHING."
		hasHandle := len(leaseIncludesHandle) > 0 && leaseIncludesHandle[0]
		if openFile.OplockLevel != OplockLevelBatch && !hasHandle {
			logger.Debug("ProcessDurableHandleContext: V1 rejected (no Batch oplock or Handle lease)",
				"oplockLevel", openFile.OplockLevel,
				"hasHandleLease", hasHandle)
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

// ReconnectResult holds the output of a successful durable handle reconnect.
type ReconnectResult struct {
	OpenFile       *OpenFile // Restored open file state
	PersistedLease uint32    // Lease state at disconnect time (for re-granting)
	PersistedEpoch uint16    // Lease-V2 epoch at disconnect time (lock layer); 0 if none
	IsV2           bool      // True if DH2C (V2), false if DHnC (V1)
	// OriginalFileID is the full 16-byte FileID captured at first CREATE
	// (zero for handles persisted before the field was introduced). Callers
	// use this to decide whether to regenerate the volatile half of the
	// FileID on reconnect; see create.go reconnect path.
	OriginalFileID [16]byte
}

// ProcessDurableReconnectContext processes DHnC or DH2C create contexts for reconnection.
// It looks up the persisted handle, validates all reconnect conditions per MS-SMB2,
// and on success returns a ReconnectResult. On failure, returns a specific NTSTATUS code.
//
// connClientGUID is the ClientGuid of the reconnecting connection (from
// NEGOTIATE). It is matched against the persisted handle's ClientGUID on both
// V1 (DHnC) and V2 (DH2C) *lease-backed* reconnect — see reopen1a-lease in
// `source4/torture/smb2/durable_open.c` (V1) and `durable_v2_open.c` (V2).
// Per-(ClientGuid, LeaseKey) lease scoping means a lease-backed durable open
// must be reconnected from the ClientGuid that established it; a mismatch
// fails OBJECT_NAME_NOT_FOUND. Persisted handles written before ClientGUID
// was captured carry the zero value and skip the check (forward compat).
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
	connClientGUID [16]byte,
) (*ReconnectResult, types.Status, error) {

	// Determine V2 (DH2C) or V1 (DHnC) reconnect
	if dh2cCtx := FindCreateContext(contexts, DurableHandleV2ReconnectTag); dh2cCtx != nil {
		openFile, leaseState, leaseEpoch, origID, status, err := processV2Reconnect(ctx, durableStore, metaSvc, contexts, dh2cCtx,
			sessionID, username, sessionKeyHash, shareName, filename, connClientGUID)
		if err != nil || status != types.StatusSuccess {
			return nil, status, err
		}
		return &ReconnectResult{OpenFile: openFile, PersistedLease: leaseState, PersistedEpoch: leaseEpoch, IsV2: true, OriginalFileID: origID}, types.StatusSuccess, nil
	}

	if dhnCCtx := FindCreateContext(contexts, DurableHandleV1ReconnectTag); dhnCCtx != nil {
		openFile, leaseState, leaseEpoch, origID, status, err := processV1Reconnect(ctx, durableStore, metaSvc, contexts, dhnCCtx,
			sessionID, username, sessionKeyHash, shareName, filename, connClientGUID)
		if err != nil || status != types.StatusSuccess {
			return nil, status, err
		}
		return &ReconnectResult{OpenFile: openFile, PersistedLease: leaseState, PersistedEpoch: leaseEpoch, IsV2: false, OriginalFileID: origID}, types.StatusSuccess, nil
	}

	// No reconnect context found
	return nil, types.StatusInvalidParameter, fmt.Errorf("no reconnect context found")
}

// checkLeaseReconnectGate runs the MS-SMB2 §3.3.5.9.7/12 lease-context gate
// that is shared by the V1 (DHnC) and V2 (DH2C) reconnect paths and exercised
// by smbtorture's reopen2-lease{,-v2} negative ladders. It MUST run before the
// path check so the empty/non-existing-fname-without-lease cases resolve to
// OBJECT_NAME_NOT_FOUND rather than the INVALID_PARAMETER a path mismatch would
// yield. The mapping is:
//
//	lease ctx absent,  persisted lease     -> OBJECT_NAME_NOT_FOUND
//	lease ctx present, persisted no-lease  -> OBJECT_NAME_NOT_FOUND
//	lease ctx present, persisted lease, undecodable lease ctx -> INVALID_PARAMETER
//	lease ctx present, persisted lease, lease key mismatch    -> OBJECT_NAME_NOT_FOUND
//	lease ctx present, persisted lease, ClientGuid mismatch   -> OBJECT_NAME_NOT_FOUND
//	otherwise                              -> StatusSuccess (proceed)
//
// It returns (StatusSuccess, persistedHasLease) when the gate passes (or does
// not apply because neither a lease ctx nor a persisted lease is present); the
// caller proceeds to validateAndRestore. The persistedHasLease flag lets the V1
// caller suppress the path check for an oplock-backed reopen (filename ignored
// per §3.3.5.9.7); the V2 caller always path-checks.
func checkLeaseReconnectGate(
	contexts []CreateContext,
	handle *lock.PersistedDurableHandle,
	connClientGUID [16]byte,
	logPrefix string,
) (status types.Status, persistedHasLease bool) {
	leaseCtx := FindCreateContext(contexts, LeaseContextTagRequest)
	persistedHasLease = handle.OplockLevel == OplockLevelLease && handle.LeaseKey != ([16]byte{})

	if leaseCtx == nil {
		if persistedHasLease {
			logger.Debug(logPrefix + ": persisted handle has lease but request omits lease ctx")
			return types.StatusObjectNameNotFound, persistedHasLease
		}
		// Oplock-backed reconnect: no lease gate applies.
		return types.StatusSuccess, persistedHasLease
	}

	if !persistedHasLease {
		logger.Debug(logPrefix + ": lease ctx provided but persisted handle has no lease")
		return types.StatusObjectNameNotFound, persistedHasLease
	}
	leaseReq, decErr := DecodeLeaseCreateContext(leaseCtx.Data)
	if decErr != nil || leaseReq == nil {
		logger.Debug(logPrefix+": invalid lease ctx", "error", decErr)
		return types.StatusInvalidParameter, persistedHasLease
	}
	if leaseReq.LeaseKey != handle.LeaseKey {
		logger.Debug(logPrefix+": lease key mismatch",
			"expected", fmt.Sprintf("%x", handle.LeaseKey),
			"actual", fmt.Sprintf("%x", leaseReq.LeaseKey))
		return types.StatusObjectNameNotFound, persistedHasLease
	}
	// Reject a lease reconnect arriving on a different ClientGuid than the one
	// that established the open. smbtorture reopen1a-lease (V1 + V2).
	if leaseReconnectClientGUIDMismatch(handle, connClientGUID) {
		logger.Debug(logPrefix+": ClientGuid mismatch on lease-backed reconnect",
			"persisted", fmt.Sprintf("%x", handle.ClientGUID),
			"connecting", fmt.Sprintf("%x", connClientGUID))
		return types.StatusObjectNameNotFound, persistedHasLease
	}
	return types.StatusSuccess, persistedHasLease
}

// leaseReconnectClientGUIDMismatch reports whether a lease-backed durable
// reconnect must be rejected because it arrives on a different ClientGuid than
// the one that established the open. Per MS-SMB2 §3.3.5.9.7/12 and Samba
// per-(ClientGuid, LeaseKey) lease scoping, a lease-backed handle (non-zero
// LeaseKey) MUST be reconnected from its originating ClientGuid. A persisted
// handle written before ClientGUID was captured carries the zero value and is
// treated as "no recorded ClientGuid" (forward compat with pre-#432 binaries).
// Shared by the V1 (DHnC) and V2 (DH2C) reconnect paths.
func leaseReconnectClientGUIDMismatch(handle *lock.PersistedDurableHandle, connClientGUID [16]byte) bool {
	return handle.LeaseKey != ([16]byte{}) &&
		handle.ClientGUID != ([16]byte{}) &&
		handle.ClientGUID != connClientGUID
}

// processV1Reconnect handles V1 (DHnC) reconnect validation and restoration.
// Returns the restored OpenFile, persisted lease state, status code, and error.
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
	connClientGUID [16]byte,
) (*OpenFile, uint32, uint16, [16]byte, types.Status, error) {
	// Parse V1 reconnect context
	wireFileID, err := DecodeDHnCReconnect(dhnCCtx.Data)
	if err != nil {
		logger.Debug("processV1Reconnect: invalid DHnC data", "error", err)
		return nil, 0, 0, [16]byte{}, types.StatusInvalidParameter, nil
	}

	// MS-SMB2 §3.2.4.4 mandates Data.Volatile=0 on DHnC, but smbtorture's
	// `smb2_push_handle` (source4/libcli/smb2/request.c) replays the full
	// original 16-byte FileID — persistent half plus the server-issued
	// volatile half — without zeroing the volatile. Since
	// `buildPersistedDurableHandle` zeroes the volatile half before storing,
	// an exact 16-byte lookup against the smbtorture wire FileID misses and
	// returns OBJECT_NAME_NOT_FOUND on the first DHnC reconnect. Match the
	// V2 (DH2C) path which already compares persistent-half only — zero the
	// volatile half here before delegating to the store. smbtorture
	// smb2.durable-open.reopen2 step 1 (and every other DHnC reopen test).
	fileID := zeroVolatileHalf(wireFileID)

	logger.Debug("processV1Reconnect: starting validation",
		"fileID", fmt.Sprintf("%x", fileID),
		"username", username,
		"shareName", shareName,
		"filename", filename)

	// Reject conflicting V2 contexts alongside V1 reconnect
	if FindCreateContext(contexts, DurableHandleV2RequestTag) != nil ||
		FindCreateContext(contexts, DurableHandleV2ReconnectTag) != nil {
		logger.Debug("processV1Reconnect: check 2 FAIL - conflicting V2 context present")
		return nil, 0, 0, [16]byte{}, types.StatusInvalidParameter, nil
	}

	// Non-destructive lookup: validation-failure paths (share/path/username/
	// access mismatch) must leave the persisted handle reclaimable until
	// expiry — a transient mismatched retry should not destroy a valid
	// durable open. The TOCTOU window is closed on the *success* path
	// (claimDurableHandleByFileID below) which atomically re-reads via
	// Consume; a second concurrent reconnect that also passes validation
	// will find the handle already gone and return OBJECT_NAME_NOT_FOUND.
	handle, err := durableStore.GetDurableHandleByFileID(ctx, fileID)
	if err != nil {
		logger.Warn("processV1Reconnect: store error", "error", err)
		return nil, 0, 0, [16]byte{}, types.StatusInternalError, err
	}
	if handle == nil {
		logger.Debug("processV1Reconnect: check 3 FAIL - handle not found by FileID",
			"fileID", fmt.Sprintf("%x", fileID))
		return nil, 0, 0, [16]byte{}, types.StatusObjectNameNotFound, nil
	}

	// MS-SMB2 §3.3.5.9.7 / Samba `smbd_smb2_create_durable_lease_check`
	// (source3/smbd/smb2_create.c): the filename in a V1 DHnC reconnect is
	// IGNORED by the server when the open is oplock-backed (no lease). The
	// lease/path matching rules only apply to lease-backed reopens. Compute
	// the V1 lease-context gate before path validation so the per-test
	// expectations of smbtorture smb2.durable-open.reopen2 hold:
	//
	//   - lease_ptr==nil, persisted no-lease  → skip path check, proceed
	//   - lease_ptr==nil, persisted lease     → OBJECT_NAME_NOT_FOUND
	//   - lease_ptr!=nil, persisted no-lease  → OBJECT_NAME_NOT_FOUND
	//   - lease_ptr!=nil, persisted lease:
	//       * lease key mismatch              → OBJECT_NAME_NOT_FOUND
	//       * path mismatch                   → INVALID_PARAMETER
	//       * else                            → proceed
	gateStatus, persistedHasLease := checkLeaseReconnectGate(contexts, handle, connClientGUID, "processV1Reconnect")
	if gateStatus != types.StatusSuccess {
		return nil, 0, 0, [16]byte{}, gateStatus, nil
	}
	// V1 oplock-backed reconnect ignores the filename (MS-SMB2 §3.3.5.9.7); a
	// lease-backed reconnect path-checks via validateAndRestore.
	checkPath := persistedHasLease

	openFile, status, restoreErr := validateAndRestore(ctx, durableStore, metaSvc, handle, sessionID, username,
		sessionKeyHash, shareName, filename, checkPath,
		func(ctx context.Context) (*lock.PersistedDurableHandle, error) {
			return durableStore.ConsumeDurableHandleByFileID(ctx, fileID)
		})
	return openFile, handle.LeaseState, handle.LeaseEpoch, handle.OriginalFileID, status, restoreErr
}

// processV2Reconnect handles V2 (DH2C) reconnect validation and restoration.
// Returns the restored OpenFile, persisted lease state, status code, and error.
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
	connClientGUID [16]byte,
) (*OpenFile, uint32, uint16, [16]byte, types.Status, error) {
	// Parse V2 reconnect context
	fileID, createGuid, flags, err := DecodeDH2CReconnect(dh2cCtx.Data)
	if err != nil {
		logger.Debug("processV2Reconnect: invalid DH2C data", "error", err)
		return nil, 0, 0, [16]byte{}, types.StatusInvalidParameter, nil
	}

	// Reject persistent flag
	if flags&DH2FlagPersistent != 0 {
		logger.Debug("processV2Reconnect: persistent flag rejected")
		return nil, 0, 0, [16]byte{}, types.StatusInvalidParameter, nil
	}

	logger.Debug("processV2Reconnect: starting validation",
		"createGuid", fmt.Sprintf("%x", createGuid),
		"username", username,
		"shareName", shareName,
		"filename", filename)

	// A V1 durable handle (opened via durable_open=true, never DH2Q) carries
	// CreateGuid=0. smbtorture reopen2-lease{,-v2} reconnect such a handle via
	// the DH2C *context* with a zero CreateGuid (smb2_create sets
	// io.in.durable_handle_v2 = h while h is a V1 handle), so the open is keyed
	// only by FileID. Mirror Samba `smbd_smb2_create_durable_v2_reconnect`,
	// which looks the open up by its persistent FileId and treats CreateGuid as
	// a secondary check: when the DH2C CreateGuid is zero, look up (and later
	// consume) by FileID rather than by the unusable zero CreateGuid — a
	// CreateGuid-keyed lookup would miss every legitimate V1-via-DH2C reconnect
	// and wrongly return OBJECT_NAME_NOT_FOUND (durable_open.c:1068 / :1305).
	zeroCreateGuid := createGuid == ([16]byte{})

	// The store keys V1 handles by the volatile-zeroed persistent FileID
	// (buildPersistedDurableHandle stores only bytes 0-7); smbtorture replays
	// the full original FileID. Zero the volatile half before the FileID lookup
	// so it matches, mirroring processV1Reconnect.
	lookupFileID := zeroVolatileHalf(fileID)

	// Non-destructive lookup: see processV1Reconnect comment. The
	// TOCTOU-closing Consume runs on the success path inside
	// validateAndRestore.
	var handle *lock.PersistedDurableHandle
	if zeroCreateGuid {
		handle, err = durableStore.GetDurableHandleByFileID(ctx, lookupFileID)
	} else {
		handle, err = durableStore.GetDurableHandleByCreateGuid(ctx, createGuid)
	}
	if err != nil {
		logger.Warn("processV2Reconnect: store error", "error", err)
		return nil, 0, 0, [16]byte{}, types.StatusInternalError, err
	}
	if handle == nil {
		logger.Debug("processV2Reconnect: handle not found",
			"createGuid", fmt.Sprintf("%x", createGuid),
			"fileID", fmt.Sprintf("%x", fileID),
			"byFileID", zeroCreateGuid)
		return nil, 0, 0, [16]byte{}, types.StatusObjectNameNotFound, nil
	}

	// The zero-CreateGuid FileID fallback is ONLY for reconnecting a V1 handle
	// (stored CreateGuid == 0) via the DH2C context. A V2 handle (stored
	// CreateGuid != 0) requires the reconnect to carry that exact CreateGuid;
	// Samba `smbd_smb2_create_durable_v2_reconnect` rejects a CreateGuid
	// mismatch with OBJECT_NAME_NOT_FOUND. smbtorture smb2.durable-v2-open.reopen2
	// /reopen2b reconnect a V2 (durable_open_v2) handle via durable_handle_v2
	// with a zeroed (or non-matching) create_guid and expect failure
	// (durable_v2_open.c:1070-1090 / reopen2b:1157). Without this guard the
	// FileID match would wrongly succeed.
	if zeroCreateGuid && handle.CreateGuid != ([16]byte{}) {
		logger.Debug("processV2Reconnect: zero CreateGuid cannot reconnect a V2 handle",
			"handleCreateGuid", fmt.Sprintf("%x", handle.CreateGuid),
			"fileID", fmt.Sprintf("%x", fileID))
		return nil, 0, 0, [16]byte{}, types.StatusObjectNameNotFound, nil
	}

	// Validate FileID from DH2C against persisted handle to prevent wrong-
	// handle reconnect. Compare only the persistent half (bytes 0-7): a
	// compliant V1 client sends Data.Volatile=0 per MS-SMB2 §3.2.4.4, but
	// smbtorture (and other test clients) replay the original full FileID
	// for V2 reconnect, so a 16-byte compare against the persisted
	// (volatile-zeroed) FileID would reject every legitimate V2 replay
	// returning STATUS_INVALID_PARAMETER. CreateGuid is the primary
	// identifier on V2 anyway; the persistent half is just an extra check.
	if [8]byte(fileID[:8]) != [8]byte(handle.FileID[:8]) {
		logger.Debug("processV2Reconnect: persistent FileID mismatch",
			"expected", fmt.Sprintf("%x", handle.FileID[:8]),
			"actual", fmt.Sprintf("%x", fileID[:8]))
		return nil, 0, 0, [16]byte{}, types.StatusInvalidParameter, nil
	}

	// MS-SMB2 §3.3.5.9.12 / Samba `smbd_smb2_create_durable_lease_check`: the V2
	// (DH2C) reconnect runs the same lease-context gate as V1 (it must precede
	// the path check so the reopen2-lease{,-v2} negative ladder produces
	// OBJECT_NAME_NOT_FOUND for the no-lease cases rather than the
	// INVALID_PARAMETER a path mismatch would yield).
	gateStatus, persistedHasLease := checkLeaseReconnectGate(contexts, handle, connClientGUID, "processV2Reconnect")
	if gateStatus != types.StatusSuccess {
		return nil, 0, 0, [16]byte{}, gateStatus, nil
	}

	// Symmetric with V1: a non-lease (oplock-backed) V2 reconnect IGNORES the
	// filename per MS-SMB2 §3.3.5.9.12 — Samba's smbd_smb2_create_durable_lease_check
	// returns NT_STATUS_OK early when lease_ptr==NULL && oplock_type!=LEASE_OPLOCK,
	// never reaching the strequal(base_name) path compare. The CreateGuid is the
	// primary identifier, so smbtorture smb2.durable-v2-open.reopen2 (batch-oplock
	// open, junk fname on reconnect) MUST succeed. The path check stays on only for
	// lease-backed reconnects, where a wrong-fname-with-lease reconnect is the final
	// negative-ladder rung that yields INVALID_PARAMETER.
	consume := func(ctx context.Context) (*lock.PersistedDurableHandle, error) {
		if zeroCreateGuid {
			// V1-via-DH2C reconnect: consume by FileID (the CreateGuid is
			// zero and unusable as a key — see the lookup branch above).
			return durableStore.ConsumeDurableHandleByFileID(ctx, lookupFileID)
		}
		return durableStore.ConsumeDurableHandleByCreateGuid(ctx, createGuid)
	}
	openFile, status, restoreErr := validateAndRestore(ctx, durableStore, metaSvc, handle, sessionID, username,
		sessionKeyHash, shareName, filename, persistedHasLease, consume)
	return openFile, handle.LeaseState, handle.LeaseEpoch, handle.OriginalFileID, status, restoreErr
}

// validateAndRestore runs the shared reconnect validation checks and restores
// the OpenFile. These checks apply to both V1 and V2 reconnects.
//
// Per Samba's durable reconnect contract (source3/smbd/smb2_create.c), the
// reconnect CREATE's DesiredAccess and ShareAccess are NOT validated here:
// reconnect restores the original granted access verbatim and never the access
// requested in the reconnect CREATE, so there is no privilege-escalation vector
// to guard. Validated fields are share name, user identity, path (lease/path-
// checked reopens only, via checkPath), handle expiry, and backing-file
// existence.
//
// consume is invoked on the success path to atomically remove the persisted
// record. If consume returns nil, another goroutine has already claimed the
// handle — the reconnect fails with OBJECT_NAME_NOT_FOUND. This is the only
// place that mutates the durable store on reconnect, which is what makes the
// path safe against the V1/V2 reconnect TOCTOU window.
//
// TODO: per CLAUDE.md invariant 1 the identity check (username) belongs in
// pkg/metadata/lock — e.g. inside the Consume* call returning a typed mismatch
// error. Left in the handler for this iteration to keep the scope small.
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
	checkPath bool,
	consume func(ctx context.Context) (*lock.PersistedDurableHandle, error),
) (*OpenFile, types.Status, error) {
	if handle.ShareName != shareName {
		logger.Debug("validateAndRestore: share name mismatch",
			"expected", handle.ShareName,
			"actual", shareName)
		return nil, types.StatusObjectNameNotFound, nil
	}

	// V1 oplock-backed reconnect ignores the filename per MS-SMB2 §3.3.5.9.7
	// (mirrors Samba `smbd_smb2_create_durable_lease_check` which only path-
	// checks lease-backed reopens). Caller passes checkPath=false in that case.
	if checkPath && handle.Path != filename {
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

	// NOTE: We intentionally do NOT compare session key hashes here.
	// Per MS-SMB2 3.3.5.9.7/12, the server validates the user identity
	// (username check above), not the session key. With NTLM KEY_EXCH,
	// each session generates a random ExportedSessionKey, so the session
	// key hash will differ between the original and reconnect sessions
	// even for the same user with the same credentials.

	// DesiredAccess and ShareAccess are intentionally NOT re-validated on
	// reconnect. Samba's durable reconnect path (source3/smbd/smb2_create.c,
	// smbd_smb2_create_durable_lease_check + smb2srv_open_recreate) compares
	// only the lease key, the filename (lease/path-checked opens only), the
	// create_guid (replay-cache lookup), and the user/session identity. There
	// is no desired_access / share_access comparison: reconnect restores the
	// ORIGINAL granted access (handle.GrantedAccess), never the access
	// requested in the reconnect CREATE, so no privilege escalation is
	// possible. By design the smbtorture reopen2 family fills every CREATE
	// request field except the reconnect blob with junk — including
	// desired_access/share_access — and expects NT_STATUS_OK, proving the
	// server consults only the reconnect context. A comparison here wrongly
	// rejects those junk-field reconnects with STATUS_ACCESS_DENIED.

	expiresAt := handle.DisconnectedAt.Add(time.Duration(handle.TimeoutMs) * time.Millisecond)
	if !expiresAt.After(time.Now()) {
		logger.Debug("validateAndRestore: handle expired",
			"disconnectedAt", handle.DisconnectedAt,
			"timeoutMs", handle.TimeoutMs,
			"expiresAt", expiresAt)
		// Clean up expired handle (best-effort; scavenger will catch any leftover).
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

	// All checks passed — atomically consume the persisted record. If
	// somebody else (a concurrent reconnect from a retrying client) already
	// claimed it, consume returns nil and we fail OBJECT_NAME_NOT_FOUND
	// rather than handing out a second OpenFile against the same handle.
	consumed, err := consume(ctx)
	if err != nil {
		logger.Warn("validateAndRestore: consume failed", "error", err)
		return nil, types.StatusInternalError, err
	}
	if consumed == nil {
		logger.Debug("validateAndRestore: handle already consumed by concurrent reconnect",
			"handleID", handle.ID)
		return nil, types.StatusObjectNameNotFound, nil
	}

	logger.Debug("validateAndRestore: all checks passed, restoring open file",
		"handleID", handle.ID,
		"path", handle.Path,
		"shareName", handle.ShareName)

	// Prefer the OriginalFileID (full 16 bytes captured at first CREATE) so
	// the restored OpenFile's OpenID matches the byte-range lock owner that
	// was created before disconnect (smb2.durable-open.lock-{oplock,lease}).
	// Older persisted handles written before this field existed decode with
	// OriginalFileID == 0 — fall back to handle.FileID (volatile-zeroed) in
	// that case so we remain forward-compatible across the upgrade boundary.
	restoreFileID := handle.OriginalFileID
	if restoreFileID == ([16]byte{}) {
		restoreFileID = handle.FileID
	}

	restored := &OpenFile{
		FileID:        restoreFileID,
		SessionID:     sessionID,
		Path:          handle.Path,
		ShareName:     handle.ShareName,
		DesiredAccess: handle.DesiredAccess,
		// Restore the original DACL-evaluated GrantedAccess persisted at
		// disconnect. Mirrors Samba: the access mask captured at open is
		// preserved verbatim through reconnect so the re-established
		// handle reports identical rights via FileAccessInformation /
		// FileAllInformation, even when the open was made with
		// MAXIMUM_ALLOWED or the file's DACL has changed in the interim.
		// Pre-#548 persisted handles (written before this field existed)
		// decode with GrantedAccess=0; fall back to the resolved
		// DesiredAccess in that case for forward compatibility — the
		// fallback matches the pre-#548 behaviour and only affects in-flight
		// reconnects across the upgrade boundary.
		GrantedAccess: func() uint32 {
			if handle.GrantedAccess != 0 {
				return handle.GrantedAccess
			}
			return resolveAccessFlags(handle.DesiredAccess)
		}(),
		MetadataHandle: handle.MetadataHandle,
		PayloadID:      metadata.PayloadID(handle.PayloadID),
		ShareAccess:    handle.ShareAccess,
		CreateOptions:  types.CreateOptions(handle.CreateOptions),
		OplockLevel:    handle.OplockLevel,
		LeaseKey:       handle.LeaseKey,
		// Restore the V2 CreateGuid recorded at the original CREATE so a
		// chained disconnect→reconnect→disconnect cycle keeps the handle's
		// V2 identity. Without this, the reconnected OpenFile carries a zero
		// CreateGuid, so the next buildPersistedDurableHandle records IsV2=false
		// with a zero guid, and the subsequent DH2C reconnect's
		// GetDurableHandleByCreateGuid lookup fails — breaking the multi-cycle
		// reconnect that smbtorture durable-v2-open.reopen2* exercises. V1
		// handles persist CreateGuid == 0, so this is a no-op for them.
		CreateGuid:    handle.CreateGuid,
		OpenTime:      handle.CreatedAt,
		DeletePending: handle.DeletePending,
		ParentHandle:  handle.ParentHandle,
		FileName:      handle.FileName,
		IsDirectory:   handle.IsDirectory,
		PositionInfo:  handle.PositionInfo,
		// Restore the ClientGUID recorded at the original CREATE so a
		// chained disconnect→reconnect→disconnect cycle preserves the
		// per-(ClientGuid, LeaseKey) lease scoping check on the next
		// reconnect attempt. Without this, the next persist would write a
		// zero ClientGUID, and the §3.3.5.9.12 lease-scoping check in
		// processV2Reconnect would silently no-op (handle.ClientGUID == 0
		// is the "pre-#432 forward-compat" branch).
		ClientGUID: handle.ClientGUID,
		// Restore the requested AllocationSize so the reconnect CREATE
		// response reports the same cluster-aligned reservation as the
		// original open ([MS-SMB2] 2.2.13.2.2). The reservation is
		// in-memory per-handle and would otherwise be lost across the
		// disconnect, dropping the reconnect out.alloc_size back to the
		// file's bare size (smb2.durable-open.alloc-size reopen checks).
		RequestedAllocSize: handle.RequestedAllocSize,
		// IsDurable is NOT set on restore -- client must re-request durability
	}

	// Persisted record was removed by the consume() call above; no
	// further store mutation is required here.

	return restored, types.StatusSuccess, nil
}

// ProcessAppInstanceId processes the SMB2_CREATE_APP_INSTANCE_ID context.
// Per MS-SMB2 §3.3.5.9.13, when a CREATE arrives carrying an AppInstanceId
// matching an existing open's AppInstanceId, the server MUST force-close the
// existing open before establishing the new one. This is the Hyper-V failover
// pattern: a VM moving between hosts presents the same AppInstanceId, and the
// new host claims the file from the old. The forced close MUST cover both:
//
//   - Disconnected (persisted) durable handles in the DurableHandleStore.
//   - Live opens still tracked in Handler.files. smbtorture
//     smb2.durable-v2-open.app-instance opens with AppInstanceId X on tree1,
//     then opens the same file with X on tree2 with tree1 *still connected*.
//     Subsequent CLOSE on the tree1 handle MUST return STATUS_FILE_CLOSED —
//     this requires the live handle to be removed from Handler.files.
//
// Returns the parsed AppInstanceId (zero value if not present or zero).
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

	if appId == ([16]byte{}) {
		return [16]byte{}
	}

	// 1) Force-close live opens with matching AppInstanceId. Uses
	// closeFilesWithFilter with isDisconnect=false so the open is fully
	// closed (locks released, caches flushed, file removed from
	// Handler.files) — NOT persisted into the durable store, since the new
	// AppInstanceId open is claiming this handle.
	if handler != nil {
		// Snapshot the lease/oplock identity of each displaced open BEFORE the
		// force-close so we can release its LeaseManager record afterwards.
		// closeFilesWithFilter removes the open from Handler.files but does NOT
		// release the per-open lease/oplock (that is the session-wide
		// releaseSessionLeasesAndNotifies path, which the AppInstanceId failover
		// does not run). Without this release, the synthetic batch-oplock record
		// of the displaced open lingers in the LeaseManager, and the *new* open
		// (which immediately follows in the CREATE path) parks on a break of that
		// orphaned oplock until the oplock timeout — and the AppInstanceId
		// failover must be silent anyway (MS-SMB2 §3.3.5.9.13; smbtorture
		// smb2.durable-v2-open.app-instance asserts break_info.count == 0).
		type displacedLease struct {
			fileHandle lock.FileHandle
			leaseKey   [16]byte
			shareName  string
			isLease    bool
		}
		var displaced []displacedLease
		if handler.LeaseManager != nil {
			handler.files.Range(func(_, value any) bool {
				f := value.(*OpenFile)
				if f.AppInstanceId == appId && f.LeaseKey != ([16]byte{}) && len(f.MetadataHandle) > 0 {
					displaced = append(displaced, displacedLease{
						fileHandle: lock.FileHandle(f.MetadataHandle),
						leaseKey:   f.LeaseKey,
						shareName:  f.ShareName,
						isLease:    f.OplockLevel == OplockLevelLease,
					})
				}
				return true
			})
		}

		liveClosed := handler.closeFilesWithFilter(
			ctx,
			0, // no specific sessionID — match across sessions
			func(f *OpenFile) bool { return f.AppInstanceId == appId },
			"ProcessAppInstanceId",
			false, // explicit close, not transport disconnect
		)
		if liveClosed > 0 {
			logger.Debug("ProcessAppInstanceId: force-closed live opens",
				"appInstanceId", fmt.Sprintf("%x", appId),
				"count", liveClosed)
		}

		// Release the displaced opens' LeaseManager records (mirrors the
		// explicit CLOSE path in close.go) so no orphaned oplock/lease lingers
		// to break the claiming open.
		for _, d := range displaced {
			if err := handler.LeaseManager.ReleaseLeaseForHandle(ctx, d.fileHandle, d.leaseKey, d.shareName); err != nil {
				logger.Debug("ProcessAppInstanceId: failed to release displaced lease",
					"leaseKey", fmt.Sprintf("%x", d.leaseKey), "error", err)
			}
			if !d.isLease {
				handler.LeaseManager.UnregisterOplockFileID(d.leaseKey)
			}
			handler.LeaseManager.SignalParkedCreates(d.fileHandle, d.shareName)
		}
	}

	// 2) Force-close persisted (disconnected) durable handles with matching
	// AppInstanceId.
	existing, err := durableStore.GetDurableHandlesByAppInstanceId(ctx, appId)
	if err != nil {
		logger.Warn("ProcessAppInstanceId: store error", "error", err)
		return appId
	}

	if len(existing) == 0 {
		return appId
	}

	logger.Debug("ProcessAppInstanceId: force-closing persisted handles",
		"appInstanceId", fmt.Sprintf("%x", appId),
		"count", len(existing))

	for _, h := range existing {
		if handler != nil {
			cleanupFile := &OpenFile{
				FileID:         h.FileID,
				Path:           h.Path,
				ShareName:      h.ShareName,
				MetadataHandle: h.MetadataHandle,
				PayloadID:      metadata.PayloadID(h.PayloadID),
			}
			handler.flushFileCache(ctx, cleanupFile)
			if len(h.MetadataHandle) > 0 && handler.Registry != nil {
				if metaSvc := handler.Registry.GetMetadataService(); metaSvc != nil {
					_ = metaSvc.UnlockAllForSession(ctx, h.MetadataHandle, 0)
				}
			}
		}

		if delErr := durableStore.DeleteDurableHandle(ctx, h.ID); delErr != nil {
			logger.Warn("ProcessAppInstanceId: failed to delete handle",
				"handleID", h.ID,
				"error", delErr)
		}
	}

	return appId
}

// buildPersistedDurableHandle creates a PersistedDurableHandle from an OpenFile
// and session information. Used when persisting durable handles during disconnect.
// leaseState is the current lease state (R/W/H flags) at disconnect time, and
// leaseEpoch the live lease-V2 epoch (lock layer), both used to restore the
// lease on reconnect.
func buildPersistedDurableHandle(
	openFile *OpenFile,
	username string,
	sessionKeyHash [32]byte,
	serverStartTime time.Time,
	leaseState uint32,
	leaseEpoch uint16,
) *lock.PersistedDurableHandle {
	// Clone MetadataHandle to avoid aliasing the live OpenFile's slice
	metaHandle := make([]byte, len(openFile.MetadataHandle))
	copy(metaHandle, openFile.MetadataHandle)

	// Clone ParentHandle to avoid aliasing
	var parentHandle []byte
	if len(openFile.ParentHandle) > 0 {
		parentHandle = make([]byte, len(openFile.ParentHandle))
		copy(parentHandle, openFile.ParentHandle)
	}

	// Per MS-SMB2 3.2.4.4: when the client reconnects via DHnC, the volatile
	// part of the FileId is zero ("Data.Volatile: MUST be set to 0"). To ensure
	// GetDurableHandleByFileID matches correctly, store only the persistent
	// part (first 8 bytes) with the volatile zeroed.
	var persistentFileID [16]byte
	copy(persistentFileID[:8], openFile.FileID[:8])

	return &lock.PersistedDurableHandle{
		ID:              uuid.New().String(),
		FileID:          persistentFileID,
		Path:            openFile.Path,
		ShareName:       openFile.ShareName,
		DesiredAccess:   openFile.DesiredAccess,
		GrantedAccess:   openFile.GrantedAccess,
		ShareAccess:     openFile.ShareAccess,
		CreateOptions:   uint32(openFile.CreateOptions),
		MetadataHandle:  metaHandle,
		PayloadID:       string(openFile.PayloadID),
		OplockLevel:     openFile.OplockLevel,
		LeaseKey:        openFile.LeaseKey,
		LeaseState:      leaseState,
		LeaseEpoch:      leaseEpoch,
		CreateGuid:      openFile.CreateGuid,
		AppInstanceId:   openFile.AppInstanceId,
		Username:        username,
		SessionKeyHash:  sessionKeyHash,
		IsV2:            openFile.CreateGuid != [16]byte{},
		CreatedAt:       openFile.OpenTime,
		DisconnectedAt:  time.Now(),
		TimeoutMs:       openFile.DurableTimeoutMs,
		ServerStartTime: serverStartTime,
		DeletePending:   openFile.DeletePending,
		ParentHandle:    parentHandle,
		FileName:        openFile.FileName,
		IsDirectory:     openFile.IsDirectory,
		PositionInfo:    openFile.PositionInfo,
		OriginalFileID:  openFile.FileID,
		ClientGUID:      openFile.ClientGUID,
		// Persist the per-handle requested AllocationSize so the durable
		// reconnect response echoes the same (cluster-aligned) value the
		// original CREATE reported, not the file's bare size
		// (smb2.durable-open.alloc-size).
		RequestedAllocSize: openFile.RequestedAllocSize,
	}
}

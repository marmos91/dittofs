package sql

import "github.com/marmos91/dittofs/pkg/metadata/lock"

// DurableHandleColumns is the SELECT list every durable-handle read shares. It
// is the order ScanDurableHandle decodes, and it is NOT the order
// durableHandleArgs writes: the insert puts lease_epoch after
// requested_alloc_size where the select puts it after lease_state. Keep the two
// orders separate — reconciling them by eye is how every field past the first
// difference ends up in the wrong column.
const DurableHandleColumns = `
	id, file_id, path, share_name, desired_access, granted_access,
	share_access, create_options, metadata_handle, payload_id, oplock_level,
	lease_key, lease_state, lease_epoch, create_guid, app_instance_id,
	username, session_key_hash, is_v2, created_at, disconnected_at,
	timeout_ms, server_start_time, delete_pending, parent_handle, file_name,
	is_directory, position_info, original_file_id, requested_alloc_size,
	is_persistent, client_guid
`

// DurableHandleInsertColumns is the INSERT list, in the order
// durableHandleArgs supplies. See DurableHandleColumns for why it differs.
const DurableHandleInsertColumns = `
	id, file_id, path, share_name, desired_access, granted_access, share_access,
	create_options, metadata_handle, payload_id, oplock_level,
	lease_key, lease_state, create_guid, app_instance_id,
	username, session_key_hash, is_v2, created_at, disconnected_at,
	timeout_ms, server_start_time,
	delete_pending, parent_handle, file_name, is_directory,
	position_info, original_file_id, requested_alloc_size,
	lease_epoch, is_persistent, client_guid
`

// ScanDurableHandle decodes one row over DurableHandleColumns. It takes the
// scan function rather than a Row so the single-row and multi-row paths share
// one field list; there is no second place for the order to drift to.
func ScanDurableHandle(scan func(dest ...any) error) (*lock.PersistedDurableHandle, error) {
	var h lock.PersistedDurableHandle
	var fileIDBytes, leaseKeyBytes, createGuidBytes, appInstanceIdBytes,
		sessionKeyHashBytes, originalFileIDBytes, clientGuidBytes []byte
	var positionInfoSigned, requestedAllocSizeSigned int64
	var leaseEpochSigned int32

	if err := scan(
		&h.ID,
		&fileIDBytes,
		&h.Path,
		&h.ShareName,
		&h.DesiredAccess,
		&h.GrantedAccess,
		&h.ShareAccess,
		&h.CreateOptions,
		&h.MetadataHandle,
		&h.PayloadID,
		&h.OplockLevel,
		&leaseKeyBytes,
		&h.LeaseState,
		&leaseEpochSigned,
		&createGuidBytes,
		&appInstanceIdBytes,
		&h.Username,
		&sessionKeyHashBytes,
		&h.IsV2,
		&h.CreatedAt,
		&h.DisconnectedAt,
		&h.TimeoutMs,
		&h.ServerStartTime,
		&h.DeletePending,
		&h.ParentHandle,
		&h.FileName,
		&h.IsDirectory,
		&positionInfoSigned,
		&originalFileIDBytes,
		&requestedAllocSizeSigned,
		&h.IsPersistent,
		&clientGuidBytes,
	); err != nil {
		return nil, err
	}

	// The columns are signed because the databases have no unsigned integers;
	// the bit pattern is what matters, and the write path reinterprets the same
	// way.
	h.LeaseEpoch = uint16(leaseEpochSigned)
	h.PositionInfo = uint64(positionInfoSigned)
	h.RequestedAllocSize = uint64(requestedAllocSizeSigned)

	copyFixedByteArrays(&h, fileIDBytes, leaseKeyBytes, createGuidBytes,
		appInstanceIdBytes, sessionKeyHashBytes, clientGuidBytes)
	if len(originalFileIDBytes) == 16 {
		copy(h.OriginalFileID[:], originalFileIDBytes)
	}

	return &h, nil
}

// copyFixedByteArrays fills the fixed-width fields from their variable-width
// columns. A column of the wrong width leaves its field zeroed rather than
// panicking on a short copy.
func copyFixedByteArrays(h *lock.PersistedDurableHandle, fileID, leaseKey, createGuid, appInstanceId, sessionKeyHash, clientGuid []byte) {
	if len(fileID) == 16 {
		copy(h.FileID[:], fileID)
	}
	if len(leaseKey) == 16 {
		copy(h.LeaseKey[:], leaseKey)
	}
	if len(createGuid) == 16 {
		copy(h.CreateGuid[:], createGuid)
	}
	if len(appInstanceId) == 16 {
		copy(h.AppInstanceId[:], appInstanceId)
	}
	if len(sessionKeyHash) == 32 {
		copy(h.SessionKeyHash[:], sessionKeyHash)
	}
	if len(clientGuid) == 16 {
		copy(h.ClientGUID[:], clientGuid)
	}
}

// nullableBytes16 stores a zero-value [16]byte as NULL, so an unset GUID reads
// back as unset rather than as sixteen zero bytes.
func nullableBytes16(b [16]byte) []byte {
	var zero [16]byte
	if b == zero {
		return nil
	}
	return b[:]
}

// durableHandleArgs supplies the insert parameters in
// DurableHandleInsertColumns order.
func durableHandleArgs(handle *lock.PersistedDurableHandle) []any {
	return []any{
		handle.ID,
		handle.FileID[:],
		handle.Path,
		handle.ShareName,
		handle.DesiredAccess,
		handle.GrantedAccess,
		handle.ShareAccess,
		handle.CreateOptions,
		handle.MetadataHandle,
		handle.PayloadID,
		handle.OplockLevel,
		nullableBytes16(handle.LeaseKey),
		handle.LeaseState,
		nullableBytes16(handle.CreateGuid),
		nullableBytes16(handle.AppInstanceId),
		handle.Username,
		handle.SessionKeyHash[:],
		handle.IsV2,
		handle.CreatedAt,
		handle.DisconnectedAt,
		handle.TimeoutMs,
		handle.ServerStartTime,
		handle.DeletePending,
		handle.ParentHandle,
		handle.FileName,
		handle.IsDirectory,
		// PositionInfo is a file offset (FILE_POSITION_INFORMATION.CurrentByteOffset,
		// MS-FSCC 2.4.40) stored as a signed 64-bit column. Offsets fit in int64 in
		// practice; reinterpreting the bit pattern preserves any high-bit value,
		// and the scan path mirrors it with uint64(int64).
		int64(handle.PositionInfo),
		handle.OriginalFileID[:],
		// RequestedAllocSize is a client-requested allocation reservation
		// ([MS-SMB2] 2.2.13.2.2), reinterpreted like PositionInfo.
		int64(handle.RequestedAllocSize),
		// LeaseEpoch is the SMB3 lease-V2 epoch, a uint16 in a signed integer
		// column; the scan path mirrors it with uint16(int32).
		int32(handle.LeaseEpoch),
		// IsPersistent marks a handle granted on a continuously-available share
		// so a reconnect re-echoes the DH2Q PERSISTENT flag.
		handle.IsPersistent,
		// ClientGUID binds a lease-backed handle to the SMB2 client GUID that
		// established it; a reconnect from a different GUID must fail
		// OBJECT_NAME_NOT_FOUND. Without persisting it the restored handle had a
		// zero GUID and the mismatch gate was silently skipped.
		nullableBytes16(handle.ClientGUID),
	}
}

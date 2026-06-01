package models

import "errors"

// Common errors for identity and control plane operations.
var (
	// User errors
	ErrUserNotFound  = errors.New("user not found")
	ErrDuplicateUser = errors.New("user already exists")
	ErrUserDisabled  = errors.New("user account is disabled")

	// Group errors
	ErrGroupNotFound  = errors.New("group not found")
	ErrDuplicateGroup = errors.New("group already exists")

	// Share errors
	ErrShareNotFound  = errors.New("share not found")
	ErrDuplicateShare = errors.New("share already exists")

	// Store errors
	ErrStoreNotFound  = errors.New("store not found")
	ErrDuplicateStore = errors.New("store already exists")
	ErrStoreInUse     = errors.New("store is referenced by shares")

	// Adapter errors
	ErrAdapterNotFound  = errors.New("adapter not found")
	ErrDuplicateAdapter = errors.New("adapter already exists")

	// Snapshot errors
	ErrSnapshotNotFound      = errors.New("snapshot not found")
	ErrSnapshotStateConflict = errors.New("snapshot is not in a state that allows this operation")

	// Phase 23 (D-23-12): orchestration sentinels surfaced to REST in Phase 25.
	ErrSnapshotBackupFailed         = errors.New("snapshot backup failed")
	ErrSnapshotVerifyFailed         = errors.New("snapshot verify failed: missing hashes on remote after drain")
	ErrSnapshotDrainTimeout         = errors.New("snapshot drain timed out")
	ErrSnapshotRetryTargetNotFound  = errors.New("snapshot retry target not found")
	ErrSnapshotRetryTargetNotFailed = errors.New("snapshot retry target is not in failed state")

	// ErrSnapshotInFlight is returned when an operation (delete) is
	// attempted on a snapshot whose create/retry orchestration is still
	// running. Mapped to 409 — the caller should retry once the
	// orchestration reaches a terminal state.
	ErrSnapshotInFlight = errors.New("snapshot operation is in progress")

	// ErrSnapshotMarkerProtected is returned when a delete is attempted on a
	// snapshot that an in-flight (or crash-interrupted) restore depends on as
	// its rollback target — i.e. the snapshot is named by the per-share
	// restore marker as the safety snapshot (or the target snapshot). The
	// safety snapshot is the sole rollback primitive, so deleting it would
	// destroy the only recoverable pre-restore state and permanently wedge the
	// share. Mapped to 409 — the caller may retry once the restore completes
	// and clears its marker.
	ErrSnapshotMarkerProtected = errors.New("snapshot is protected by an in-progress restore and cannot be deleted")

	// ErrSnapshotLocalStoreUnsupported is returned when a snapshot is
	// requested on a share whose local block store has no on-disk root
	// (the in-memory backend), so the metadata.dump + manifest.hashes
	// artifacts have nowhere to be written. Mapped to 400 (client config
	// error, not a server fault).
	ErrSnapshotLocalStoreUnsupported = errors.New("snapshots require an fs-backed local store")

	// Restore orchestration sentinels.
	ErrShareEnabled                = errors.New("share must be disabled before restore")
	ErrSnapshotNotDurable          = errors.New("snapshot is not remote-durable; pass AllowNonDurable to override")
	ErrSnapshotMetadataDumpMissing = errors.New("snapshot metadata dump file is missing")
	ErrMetadataStoreNotResetable   = errors.New("metadata engine does not implement Resetable")
	ErrRestoreSafetySnapFailed     = errors.New("restore safety snapshot creation or wait failed")
	ErrRestoreAborted              = errors.New("restore aborted; safety snapshot retained for rollback")
	ErrRestoreVerifyFailed         = errors.New("restore verify failed: missing hashes on remote")
	ErrRestoreMarkerNotFound       = errors.New("restore marker not found")
	ErrRestoreInProgress           = errors.New("a restore is already in progress for this share")

	// Setting errors
	ErrSettingNotFound = errors.New("setting not found")

	// Netgroup errors
	ErrNetgroupNotFound  = errors.New("netgroup not found")
	ErrDuplicateNetgroup = errors.New("netgroup already exists")
	ErrNetgroupInUse     = errors.New("netgroup is referenced by shares")

	// Guest errors
	ErrGuestDisabled = errors.New("guest access is disabled")
)

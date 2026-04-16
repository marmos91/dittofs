// Package metadata exports the Backupable capability interface and related
// sentinels. In Phase 4 (D-27) the canonical definitions moved to
// github.com/marmos91/dittofs/pkg/backup; this file preserves the legacy
// import path via type aliases and variable re-assignments so existing
// Phase-2 engine implementations compile without edits.
//
// New code SHOULD import github.com/marmos91/dittofs/pkg/backup directly.
// This shim is a permanent backstop for existing callers that reference
// metadata.Backupable, metadata.PayloadIDSet, and the metadata.Err* sentinels.
package metadata

import "github.com/marmos91/dittofs/pkg/backup"

// Type aliases make metadata.Backupable and backup.Backupable the identical
// type (not a conversion); likewise for PayloadIDSet.
type (
	// Backupable is an alias for backup.Backupable (D-27).
	Backupable = backup.Backupable
	// PayloadIDSet is an alias for backup.PayloadIDSet (D-27).
	PayloadIDSet = backup.PayloadIDSet
)

// Sentinel re-exports use `var X = backup.X` (not `errors.New(...)`) so
// errors.Is(metadata.ErrX, backup.ErrX) returns true — the identity is
// preserved, not duplicated.
var (
	// NewPayloadIDSet re-exports backup.NewPayloadIDSet.
	NewPayloadIDSet = backup.NewPayloadIDSet

	// ErrBackupUnsupported re-exports backup.ErrBackupUnsupported.
	ErrBackupUnsupported = backup.ErrBackupUnsupported
	// ErrRestoreDestinationNotEmpty re-exports backup.ErrRestoreDestinationNotEmpty.
	ErrRestoreDestinationNotEmpty = backup.ErrRestoreDestinationNotEmpty
	// ErrRestoreCorrupt re-exports backup.ErrRestoreCorrupt.
	ErrRestoreCorrupt = backup.ErrRestoreCorrupt
	// ErrSchemaVersionMismatch re-exports backup.ErrSchemaVersionMismatch.
	ErrSchemaVersionMismatch = backup.ErrSchemaVersionMismatch
	// ErrBackupAborted re-exports backup.ErrBackupAborted.
	ErrBackupAborted = backup.ErrBackupAborted
)

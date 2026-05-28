package runtime

// RestoreSnapshotOpts configures a single Runtime.RestoreSnapshot call.
// The zero value is the safe default: refuse snapshots with
// RemoteDurable=false.
type RestoreSnapshotOpts struct {
	// AllowNonDurable opts into restoring from snapshots created with
	// CreateSnapshotOpts.NoSyncGate=true. Pre-verify still runs and
	// re-checks remote durability before any destructive op; this flag
	// only relaxes the default-refuse precondition.
	AllowNonDurable bool
}

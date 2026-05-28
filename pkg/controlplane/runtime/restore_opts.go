package runtime

// RestoreSnapshotOpts is the operator-facing configuration for one
// Runtime.RestoreSnapshot invocation.
//
// Zero-value (AllowNonDurable=false) requests the default behavior:
// refuse snapshots with RemoteDurable=false. This is the safe path —
// it refuses to restore from a snapshot whose blocks were never
// confirmed durable on the remote at create time.
//
// Phase 25 surfaces this as `--force-non-durable` (or an equivalent
// request-body flag) on the `dfsctl share snapshot restore` CLI.
//
// Forward-compatibility: additional fields (e.g. SkipPreVerify) may
// land in later plans; they MUST preserve the zero-value-is-safe-default
// invariant so existing callers don't silently change behavior.
type RestoreSnapshotOpts struct {
	// AllowNonDurable (D-24-06) opts into restoring from snapshots
	// created with CreateSnapshotOpts.NoSyncGate=true (i.e. snapshots
	// whose manifest hashes were never sync-verified against the
	// remote at create time).
	//
	// Pre-verify (step 2 of the D-24-09 orchestration sequence) is
	// the real safety gate — it re-checks remote durability before
	// any destructive op runs — so this flag only relaxes the
	// default-refuse precondition, not the underlying invariant
	// that restored blocks must be reachable.
	AllowNonDurable bool
}

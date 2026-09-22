package gc

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

const (
	// gcStateLastRunFile holds the last mark-sweep run's summary, directly
	// under the GC state root.
	gcStateLastRunFile = "last-run.json"

	// auditStateSubdir and auditLastRunFile place the last refcount audit's
	// result under the share's local store root.
	auditStateSubdir = "audit-state"
	auditLastRunFile = "last-inv02.json"
)

// writeLastRunJSON writes v as indented JSON to dir/name, creating dir if it
// is missing. An empty dir is a no-op: the caller chose not to persist.
//
// The write is atomic by .tmp + rename, so a crash mid-write leaves the
// previous summary intact rather than a half-written one.
//
// decision: the write is deliberately
// NOT durable — neither the temp file nor the directory is fsynced, so a
// machine that loses power right after the rename can come back with the older
// summary or none at all. That is the right trade for what this is: an
// operator-facing report of a run that can simply be run again, never an input
// to a decision about what is safe to delete.
func writeLastRunJSON(dir, name string, v any) error {
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("last run: mkdir %s: %w", dir, err)
	}
	body, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("last run: marshal %s: %w", name, err)
	}
	final := filepath.Join(dir, name)
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return fmt.Errorf("last run: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("last run: rename %s: %w", final, err)
	}
	return nil
}

// PersistLastRunSummary writes the mark-sweep summary to
// rootDir/last-run.json. An empty rootDir is a no-op.
func PersistLastRunSummary(rootDir string, summary GCRunSummary) error {
	return writeLastRunJSON(rootDir, gcStateLastRunFile, summary)
}

// persistAuditLastRun writes the refcount audit's result to
// <localStoreRoot>/audit-state/last-inv02.json. An empty localStoreRoot is a
// no-op.
func persistAuditLastRun(localStoreRoot string, r *AuditRefcountsResult) error {
	return writeLastRunJSON(auditLastRunDir(localStoreRoot), auditLastRunFile, r)
}

// auditLastRunDir is the directory holding a share's audit state, or "" when
// localStoreRoot is empty.
func auditLastRunDir(localStoreRoot string) string {
	if localStoreRoot == "" {
		return ""
	}
	return filepath.Join(localStoreRoot, auditStateSubdir)
}

// AuditLastRunPath returns the on-disk location of the last audit result.
// It is returned even when no run has been recorded — callers stat the file
// separately. An empty localStoreRoot returns an empty string.
func AuditLastRunPath(localStoreRoot string) string {
	dir := auditLastRunDir(localStoreRoot)
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, auditLastRunFile)
}

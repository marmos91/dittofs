// Package engine — audit
//
// AuditRefcounts walks a share's metadata store and verifies the CAS
// manifest-consistency invariant:
//
//	every block referenced by a file's manifest (FileAttr.Blocks) must
//	have a backing FileBlock row in the metadata store.
//
// A manifest reference with no backing FileBlock row is a genuine
// DANGLING reference — the silent-data-loss class (cf. #583/#789): the
// file claims a chunk that the store has no record of, so a read would
// return zeros or fail. DanglingRefs > 0 is the real signal worth
// alerting on; the invariant is DanglingRefs == 0.
//
// This replaces the legacy "∑ FileBlock.RefCount == ∑ len(FileAttr.Blocks)"
// reconciliation. RefCount is not maintained in the CAS model (CAS blocks
// are written State=Pending and never transition to Remote, GetByHash is
// remote-gated, and reclamation is mark-sweep GC over the live set), so
// the old metric was structurally always 0 and produced false-positive
// "corruption" alarms. The audit no longer touches RefCount or GetByHash.
//
// Persistence: the last-run summary is written atomically (.tmp + rename)
// to <localStoreRoot>/audit-state/last-inv02.json, mirroring GC's
// last-run.json under <gcStateRoot>/last-run.json. An empty localStoreRoot
// means "do not persist" — used when the share's local store has no
// persistent root (in-memory backend).
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	// justification: AuditRefcounts is the cross-file metadata-walk
	// entrypoint for reconciliation. It MUST bind
	// metadata.Store to enumerate FileAttr.Blocks across the share's
	// directory tree (GetRootHandle, GetFile, ListChildren) and to read
	// the backing FileBlock rows (ListFileBlocks). Lifting these helpers
	// into pkg/block would create a circular import.
	"github.com/marmos91/dittofs/pkg/metadata"
)

const (
	auditStateSubdir   = "audit-state"
	auditLastRunFile   = "last-inv02.json"
	auditLastRunTmpExt = ".tmp"
)

// AuditRefcountsResult is the operator-facing outcome of an audit.
// All counts are aggregate (no PII, no per-file paths). Same release surface
// as GC last-run.json.
type AuditRefcountsResult struct {
	// Share is the share whose metadata store was audited.
	Share string `json:"share"`

	// StartedAt is when the audit walk began (UTC).
	StartedAt time.Time `json:"started_at"`

	// CompletedAt is when the audit walk finished (UTC).
	CompletedAt time.Time `json:"completed_at"`

	// DurationMS is the wall-clock cost in milliseconds.
	DurationMS int64 `json:"duration_ms"`

	// TotalFiles is the number of regular files scanned.
	TotalFiles uint64 `json:"total_files"`

	// TotalRefs is ∑ len(FileAttr.Blocks) across every regular file in the
	// share — the manifest reference count.
	TotalRefs uint64 `json:"total_refs"`

	// BackedRefs is the number of manifest references that have a matching
	// FileBlock row in the metadata store (a row at the ref's offset with a
	// non-zero hash).
	BackedRefs uint64 `json:"backed_refs"`

	// DanglingRefs is the number of manifest references with NO backing
	// FileBlock row (== TotalRefs - BackedRefs). Each dangling ref is a
	// silent-data-loss hazard: the file claims a chunk the store has no
	// record of.
	DanglingRefs uint64 `json:"dangling_refs"`

	// Delta is the violation count, defined as DanglingRefs. Zero means the
	// invariant holds (every manifest ref is backed); non-zero means at
	// least one file references a chunk with no FileBlock row.
	Delta int64 `json:"delta"`
}

// AuditRefcounts walks the metadata store and verifies the manifest↔
// FileBlock-row consistency invariant for the named share. Persists
// last-run summary at <localStoreRoot>/audit-state/last-inv02.json. Pass an
// empty localStoreRoot to skip persistence (in-memory backend).
//
// per-share audit, slog-only observability
// no Prometheus surface in this phase.
func AuditRefcounts(ctx context.Context, share string, store metadata.Store, localStoreRoot string) (*AuditRefcountsResult, error) {
	if store == nil {
		return nil, errors.New("audit-refcounts: metadata store is nil")
	}
	if share == "" {
		return nil, errors.New("audit-refcounts: share is empty")
	}

	start := time.Now().UTC()
	result := &AuditRefcountsResult{
		Share:     share,
		StartedAt: start,
	}

	// Walk every regular file in the share. For each file, read its
	// manifest (FileAttr.Blocks) and the backing FileBlock rows
	// (ListFileBlocks by payloadID). A manifest ref is BACKED iff a row
	// exists at the ref's offset with a matching non-zero hash; otherwise
	// it is DANGLING.
	rootHandle, err := store.GetRootHandle(ctx, share)
	if err != nil {
		return nil, fmt.Errorf("audit-refcounts: get root handle for %q: %w", share, err)
	}
	if err := walkAuditShareFiles(ctx, store, rootHandle, func(f *metadata.File) error {
		result.TotalFiles++
		backed, dangling, err := auditFileManifest(ctx, store, f)
		if err != nil {
			return err
		}
		result.TotalRefs += uint64(len(f.Blocks))
		result.BackedRefs += backed
		result.DanglingRefs += dangling
		return nil
	}); err != nil {
		return nil, fmt.Errorf("audit-refcounts: walk share %q: %w", share, err)
	}

	result.CompletedAt = time.Now().UTC()
	result.DurationMS = result.CompletedAt.Sub(result.StartedAt).Milliseconds()
	result.Delta = int64(result.DanglingRefs)

	if err := persistAuditLastRun(localStoreRoot, result); err != nil {
		return nil, fmt.Errorf("audit-refcounts: persist last-run: %w", err)
	}
	return result, nil
}

// auditFileManifest reconciles one file's manifest (FileAttr.Blocks)
// against its backing FileBlock rows. Returns the count of backed vs
// dangling manifest refs for the file.
//
// FileBlock IDs are "{payloadID}/{offset}" (the engine writes
// fmt.Sprintf("%s/%d", payloadID, blockRef.Offset)); the manifest BlockRef
// carries the same Offset. A ref is backed iff a row exists at its offset
// with a non-zero hash. Matching by offset (not by hash equality) is the
// right key: the manifest is the authority for which chunk lives at each
// offset, and a present row with a non-zero hash means the store has a
// record of that chunk.
func auditFileManifest(ctx context.Context, store metadata.Store, f *metadata.File) (backed, dangling uint64, err error) {
	if len(f.Blocks) == 0 {
		return 0, 0, nil
	}

	payloadID := string(f.PayloadID)
	rows, err := store.ListFileBlocks(ctx, payloadID)
	if err != nil {
		return 0, 0, fmt.Errorf("list file blocks for payload %q: %w", payloadID, err)
	}

	// Set of present rows keyed by parsed offset (the suffix of the
	// FileBlock ID after "{payloadID}/"). A row with a zero hash does not
	// back a manifest ref — treat it as absent.
	present := make(map[uint64]struct{}, len(rows))
	prefix := payloadID + "/"
	for _, row := range rows {
		if row == nil || row.Hash.IsZero() {
			continue
		}
		suffix := strings.TrimPrefix(row.ID, prefix)
		off, convErr := strconv.ParseUint(suffix, 10, 64)
		if convErr != nil {
			// Non-numeric suffix: not an offset-keyed CAS row. Skip it
			// rather than crediting it to an offset.
			continue
		}
		present[off] = struct{}{}
	}

	for _, ref := range f.Blocks {
		if _, ok := present[ref.Offset]; ok {
			backed++
		} else {
			dangling++
		}
	}
	return backed, dangling, nil
}

// walkAuditShareFiles recursively walks the share rooted at dirHandle
// invoking fn for every regular file. Pagination is via the existing
// ListChildren cursor; depth is unbounded but bounded by the share's
// directory tree depth. Pure-traversal — no mutation, safe for concurrent
// reads against the live metadata store.
func walkAuditShareFiles(ctx context.Context, store metadata.Store, dirHandle metadata.FileHandle, fn func(*metadata.File) error) error {
	cursor := ""
	for {
		entries, next, err := store.ListChildren(ctx, dirHandle, cursor, 0)
		if err != nil {
			return fmt.Errorf("list children: %w", err)
		}
		for _, e := range entries {
			child, err := store.GetFile(ctx, e.Handle)
			if err != nil {
				return fmt.Errorf("get file %q: %w", e.Name, err)
			}
			switch child.Type {
			case metadata.FileTypeDirectory:
				if err := walkAuditShareFiles(ctx, store, e.Handle, fn); err != nil {
					return err
				}
			case metadata.FileTypeRegular:
				if err := fn(child); err != nil {
					return err
				}
			}
		}
		if next == "" {
			return nil
		}
		cursor = next
	}
}

// persistAuditLastRun atomically writes the result to
// <localStoreRoot>/audit-state/last-inv02.json. Empty localStoreRoot
// is a no-op (matches the engine.PersistLastRunSummary contract).
// Atomic via .tmp + rename so a crash mid-write leaves the previous
// last-inv02.json intact.
func persistAuditLastRun(localStoreRoot string, r *AuditRefcountsResult) error {
	if localStoreRoot == "" {
		return nil
	}
	dir := filepath.Join(localStoreRoot, auditStateSubdir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	body, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	final := filepath.Join(dir, auditLastRunFile)
	tmp := final + auditLastRunTmpExt
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

// AuditLastRunPath returns the on-disk location of the last-run.json
// summary for the share's audit state. Returned even when no run has
// been recorded — callers stat the file separately. Empty localStoreRoot
// returns an empty string.
func AuditLastRunPath(localStoreRoot string) string {
	if localStoreRoot == "" {
		return ""
	}
	return filepath.Join(localStoreRoot, auditStateSubdir, auditLastRunFile)
}

// Package engine — audit
//
// AuditRefcounts walks a share's metadata store and reconciles the global
// invariant
//
//	∑ FileBlock.RefCount == ∑ len(FileAttr.Blocks)
//
// A non-zero delta indicates a refcount drift that may block GC reclamation
// (a leaked block's CAS object survives the grace window) or signal a bug
// in the dedup short-circuit (donor leak). The audit is operator-invoked
// via `dfsctl blockstore audit-refcounts <share>`; there is no periodic
// schedule.
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
	"time"

	"github.com/marmos91/dittofs/pkg/block"
	// justification: AuditRefcounts is the cross-file metadata-walk
	// entrypoint for reconciliation. It MUST bind
	// metadata.MetadataStore to enumerate FileAttr.Blocks across the share's
	// directory tree (GetRootHandle, GetFile, ListChildren). Lifting these
	// helpers into pkg/blockstore would create a circular import.
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

	// TotalRefs is ∑ len(FileAttr.Blocks) across every regular file in the share.
	TotalRefs uint64 `json:"total_refs"`

	// TotalRefCount is ∑ FileBlock.RefCount summed across distinct hashes
	// (the post- single-row-per-hash world; legacy multi-row data is
	// dedup'd via GetByHash so cross-row hash duplicates don't double-count).
	TotalRefCount uint64 `json:"total_refcount"`

	// Delta is TotalRefs - TotalRefCount. Zero means holds; non-zero
	// indicates drift (positive: file refs exceed RefCount; negative: leaked
	// RefCount with no owning file).
	Delta int64 `json:"delta"`
}

// AuditRefcounts walks the metadata store and computes the
// invariant for the named share. Persists last-run summary at
// <localStoreRoot>/audit-state/last-inv02.json. Pass an empty
// localStoreRoot to skip persistence (in-memory backend).
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

	// 1) ∑ FileBlock.RefCount across distinct ContentHashes via the
	// cursor-bounded EnumerateFileBlocks (memory bound). Dedup
	// by hash mirrors the post- single-row-per-hash world; legacy
	//    multi-row data is collapsed because every row sharing a hash
	//    carries the same RefCount semantics (GetByHash returns ANY one).
	seen := make(map[block.ContentHash]struct{})
	if err := store.EnumerateFileBlocks(ctx, func(h block.ContentHash) error {
		if _, ok := seen[h]; ok {
			return nil
		}
		seen[h] = struct{}{}
		fb, err := store.GetByHash(ctx, h)
		if err != nil {
			return fmt.Errorf("get by hash %x: %w", h[:8], err)
		}
		if fb != nil {
			result.TotalRefCount += uint64(fb.RefCount)
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("audit-refcounts: enumerate file blocks: %w", err)
	}

	// 2) ∑ len(FileAttr.Blocks) across every regular file in the share.
	rootHandle, err := store.GetRootHandle(ctx, share)
	if err != nil {
		return nil, fmt.Errorf("audit-refcounts: get root handle for %q: %w", share, err)
	}
	if err := walkAuditShareFiles(ctx, store, rootHandle, func(f *metadata.File) error {
		result.TotalFiles++
		result.TotalRefs += uint64(len(f.Blocks))
		return nil
	}); err != nil {
		return nil, fmt.Errorf("audit-refcounts: walk share %q: %w", share, err)
	}

	result.CompletedAt = time.Now().UTC()
	result.DurationMS = result.CompletedAt.Sub(result.StartedAt).Milliseconds()
	result.Delta = int64(result.TotalRefs) - int64(result.TotalRefCount)

	if err := persistAuditLastRun(localStoreRoot, result); err != nil {
		return nil, fmt.Errorf("audit-refcounts: persist last-run: %w", err)
	}
	return result, nil
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

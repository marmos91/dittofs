// Package engine — GCState
//
// Phase 11 Plan 06 (D-01): the GC mark phase persists the live ContentHash
// set on disk under <localStore>/gc-state/<runID>/ using a Badger temp
// store. This bounds memory regardless of metadata size: a backend with
// 100M file blocks would OOM if we held the entire live set in a Go map.
//
// Each run creates an `incomplete.flag` marker on entry and removes it via
// MarkComplete() on success. CleanStaleGCStateDirs reclaims any directory
// whose marker is still present (a crashed prior run). Mark is idempotent;
// we do not resume a crashed run, we discard and start fresh.
//
// last-run.json (D-10) is written under the gc-state root after a
// successful run with aggregate counts the operator and `dfsctl store
// block gc-status` (plan 07) consume.
package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/dgraph-io/badger/v4"

	"github.com/marmos91/dittofs/pkg/blockstore"
)

const (
	gcStateIncompleteFlag = "incomplete.flag"
	gcStateLastRunFile    = "last-run.json"
)

// GCState persists the GC mark-phase live set to disk under a per-runID
// directory. Backed by Badger so memory remains bounded regardless of
// metadata size. The incomplete.flag marker is created at NewGCState and
// removed by MarkComplete; CleanStaleGCStateDirs uses the marker to
// detect crashed runs and reclaim their dirs. See D-01.
//
// Phase 11 IN-4-04: Add() batches writes through Badger's WriteBatch
// (gcAddBatchSize hashes per commit) instead of opening a fresh
// txn.Update per hash. Per-call WriteBatch overhead is amortized so
// 10M+ live sets no longer hit the txn-per-hash throughput cliff
// (single-row mark would otherwise dominate mark-phase wall time).
// FlushAdd() forces the in-flight batch to disk; the mark phase calls
// it once after EnumerateFileBlocks returns so subsequent Has()
// queries see every Add().
type GCState struct {
	runDir   string
	db       *badger.DB
	batch    *badger.WriteBatch
	batchLen int
}

// gcAddBatchSize is the per-WriteBatch flush threshold. 1000 hashes is
// well below Badger's MaxBatchCount (default ~104K) and MaxBatchSize
// (~1MB) for 32-byte keys, leaving headroom; small enough to bound
// peak memory; large enough to amortize commit overhead by ~3 orders
// of magnitude vs txn-per-hash.
const gcAddBatchSize = 1000

// NewGCState opens a fresh Badger temp store for this run. rootDir is the
// gc-state root (typically <localStore>/gc-state/). runID should be a
// monotonic identifier (e.g., RFC3339 timestamp + short random suffix).
func NewGCState(rootDir, runID string) (*GCState, error) {
	if rootDir == "" {
		return nil, fmt.Errorf("gcstate: rootDir is empty")
	}
	if runID == "" {
		return nil, fmt.Errorf("gcstate: runID is empty")
	}
	runDir := filepath.Join(rootDir, runID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return nil, fmt.Errorf("gcstate: mkdir %s: %w", runDir, err)
	}
	// Drop the incomplete marker. If MarkComplete is never called, the
	// next CleanStaleGCStateDirs sweep will reclaim this directory.
	if f, err := os.Create(filepath.Join(runDir, gcStateIncompleteFlag)); err != nil {
		return nil, fmt.Errorf("gcstate: create incomplete flag: %w", err)
	} else {
		_ = f.Close()
	}
	opts := badger.DefaultOptions(filepath.Join(runDir, "db")).
		WithLogger(nil).
		WithSyncWrites(false)
	db, err := badger.Open(opts)
	if err != nil {
		return nil, fmt.Errorf("gcstate: open badger: %w", err)
	}
	return &GCState{runDir: runDir, db: db}, nil
}

// Add records a live ContentHash. Idempotent — repeated calls with the same
// hash are no-ops at the data layer. Phase 11 IN-4-04: writes are buffered
// through a Badger WriteBatch and flushed every gcAddBatchSize hashes;
// FlushAdd() forces a flush so callers can rely on Has() seeing every
// preceding Add().
//
// NOTE: not safe for concurrent callers. The mark phase is single-goroutine
// (one share at a time, see markPhase); if that ever changes the batch
// state needs a mutex.
func (g *GCState) Add(h blockstore.ContentHash) error {
	if g.batch == nil {
		g.batch = g.db.NewWriteBatch()
	}
	if err := g.batch.Set(append([]byte(nil), h[:]...), nil); err != nil {
		return err
	}
	g.batchLen++
	if g.batchLen >= gcAddBatchSize {
		return g.flushBatchLocked()
	}
	return nil
}

// FlushAdd forces any in-flight batched Add()s to disk. Mark-phase callers
// invoke this once after EnumerateFileBlocks returns so the sweep's Has()
// queries see every marked hash.
func (g *GCState) FlushAdd() error {
	if g.batch == nil {
		return nil
	}
	return g.flushBatchLocked()
}

// flushBatchLocked commits the in-flight WriteBatch and resets state.
// "Locked" is documentation-only — see the concurrency note on Add().
func (g *GCState) flushBatchLocked() error {
	if g.batch == nil {
		return nil
	}
	err := g.batch.Flush()
	g.batch = nil
	g.batchLen = 0
	return err
}

// Has reports whether a hash was Add-ed during this run.
//
// Phase 11 IN-4-04: Has() implicitly flushes any pending Add() batch
// so callers querying mid-mark observe a consistent view. The
// production sweep path explicitly invokes FlushAdd() after the mark
// phase completes so the implicit flush here is a defensive no-op in
// that flow; tests that interleave Add()/Has() rely on it.
func (g *GCState) Has(h blockstore.ContentHash) (bool, error) {
	if g.batch != nil {
		if err := g.flushBatchLocked(); err != nil {
			return false, fmt.Errorf("flush pending add batch: %w", err)
		}
	}
	var present bool
	err := g.db.View(func(txn *badger.Txn) error {
		_, err := txn.Get(h[:])
		if errors.Is(err, badger.ErrKeyNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		present = true
		return nil
	})
	return present, err
}

// MarkComplete removes the incomplete.flag marker. After this returns nil,
// CleanStaleGCStateDirs will not reclaim this dir.
func (g *GCState) MarkComplete() error {
	if err := os.Remove(filepath.Join(g.runDir, gcStateIncompleteFlag)); err != nil {
		// Already removed is a benign no-op (idempotent).
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("gcstate: remove incomplete flag: %w", err)
	}
	return nil
}

// Close releases the underlying Badger handle so the runDir can be
// removed. Safe to call multiple times. Any in-flight Add() batch is
// flushed first; a Cancel'd batch on close would silently lose writes.
func (g *GCState) Close() error {
	if g.db == nil {
		return nil
	}
	if g.batch != nil {
		_ = g.batch.Flush()
		g.batch = nil
		g.batchLen = 0
	}
	err := g.db.Close()
	g.db = nil
	return err
}

// RunDir returns the per-run directory. Callers (notably the GC code that
// writes last-run.json under the parent rootDir) use this for diagnostics.
func (g *GCState) RunDir() string { return g.runDir }

// CleanStaleGCStateDirs removes any per-runID directory under rootDir
// whose incomplete.flag still exists. Mark is idempotent (D-01) so we
// do not resume; we discard and start fresh. Completed dirs (no flag)
// are left alone — the caller decides retention. The last-run.json file
// at rootDir's top level is also left alone.
func CleanStaleGCStateDirs(rootDir string) error {
	if rootDir == "" {
		return nil
	}
	entries, err := os.ReadDir(rootDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("gcstate: readdir %s: %w", rootDir, err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		flag := filepath.Join(rootDir, e.Name(), gcStateIncompleteFlag)
		if _, err := os.Stat(flag); err == nil {
			if err := os.RemoveAll(filepath.Join(rootDir, e.Name())); err != nil {
				return fmt.Errorf("gcstate: clean stale dir %s: %w", e.Name(), err)
			}
		}
	}
	return nil
}

// GCRunSummary is the persisted last-run.json shape. See D-10.
//
// All counts are aggregate (no file paths or hashes beyond DryRunCandidates
// which only contain CAS keys, not source paths). This file lives under
// the gc-state root and is consumed by `dfsctl store block gc-status`
// (plan 07).
type GCRunSummary struct {
	RunID            string    `json:"run_id"`
	StartedAt        time.Time `json:"started_at"`
	CompletedAt      time.Time `json:"completed_at"`
	HashesMarked     int64     `json:"hashes_marked"`
	ObjectsSwept     int64     `json:"objects_swept"`
	BytesFreed       int64     `json:"bytes_freed"`
	DurationMs       int64     `json:"duration_ms"`
	ErrorCount       int       `json:"error_count"`
	FirstErrors      []string  `json:"first_errors,omitempty"`
	DryRun           bool      `json:"dry_run"`
	DryRunCandidates []string  `json:"dry_run_candidates,omitempty"`
}

// PersistLastRunSummary writes the summary atomically to
// rootDir/last-run.json (.tmp + rename). Returns nil if rootDir is empty
// (caller chose not to persist) or if the directory does not exist.
func PersistLastRunSummary(rootDir string, summary GCRunSummary) error {
	if rootDir == "" {
		return nil
	}
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		return fmt.Errorf("gcstate: mkdir %s: %w", rootDir, err)
	}
	body, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		return fmt.Errorf("gcstate: marshal summary: %w", err)
	}
	tmp := filepath.Join(rootDir, gcStateLastRunFile+".tmp")
	final := filepath.Join(rootDir, gcStateLastRunFile)
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return fmt.Errorf("gcstate: write tmp summary: %w", err)
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("gcstate: rename summary: %w", err)
	}
	return nil
}

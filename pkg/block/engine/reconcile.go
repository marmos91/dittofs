// Package engine — read-only orphan-storage reporter (#1493/#1525 reconcile,
// PR5a). Reconcile enumerates and classifies orphaned block storage WITHOUT
// mutating anything: no DeleteBlock, no DecrLiveChunkCount, no marker changes.
// It is the report-only first stage of the reconcile series; an operator
// reviews the report before the later delete stages (PR5b/5c) act.
package engine

import (
	"context"
	"fmt"
	"iter"
	"log/slog"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/remote"
)

// defaultReconcileSampleCap bounds the per-class ID sample when the caller does
// not specify one. Counts are always exact; only the sample list is bounded.
const defaultReconcileSampleCap = 100

// ReconcileMetaView is the read-only per-share metadata surface the reporter
// consumes. metadata.Store satisfies it structurally (EnumerateSynced concrete
// method + SyncedHashStore.GetLocator + BlockRecordStore.WalkBlockRecords).
type ReconcileMetaView interface {
	EnumerateSynced(ctx context.Context, fn func(hash block.ContentHash, loc block.ChunkLocator, syncedAt time.Time) error) error
	GetLocator(ctx context.Context, hash block.ContentHash) (block.ChunkLocator, bool, error)
	WalkBlockRecords(ctx context.Context, fn func(block.BlockRecord) error) error
}

// ReconcileLocalView is the read-only local surface: enumerate unsynced,
// local-durable chunks. *fs.FSStore satisfies it (ListUnsynced).
type ReconcileLocalView interface {
	ListUnsynced(ctx context.Context) iter.Seq2[block.ContentHash, error]
}

// ReconcileClass is one orphan category's tally plus a bounded sample of IDs.
// Count/Bytes are exact; Sample is capped and Truncated flags a dropped ID.
type ReconcileClass struct {
	Count     int64    `json:"count"`
	Bytes     int64    `json:"bytes,omitempty"`
	Sample    []string `json:"sample,omitempty"`
	Truncated bool     `json:"truncated,omitempty"`
}

// addSample appends id to the bounded sample, flagging Truncated once the cap
// is hit. Shared by add and merge so the cap edge is enforced in one place.
func (c *ReconcileClass) addSample(id string, cap int) {
	if len(c.Sample) < cap {
		c.Sample = append(c.Sample, id)
	} else {
		c.Truncated = true
	}
}

// add records one orphan, appending its id to the sample until the cap is hit.
func (c *ReconcileClass) add(id string, bytes int64, cap int) {
	c.Count++
	c.Bytes += bytes
	c.addSample(id, cap)
}

// merge folds other into c, respecting cap for the combined sample.
func (c *ReconcileClass) merge(other ReconcileClass, cap int) {
	c.Count += other.Count
	c.Bytes += other.Bytes
	if other.Truncated {
		c.Truncated = true
	}
	for _, id := range other.Sample {
		c.addSample(id, cap)
	}
}

// ReconcileReport is the read-only output of a scan: four orphan classes plus
// scan bookkeeping. Nothing in the store is mutated to produce it.
type ReconcileReport struct {
	// ZeroRefRecords: block records absent from the live locator set with
	// LiveChunkCount == 0 — a crash between DecrLiveChunkCount and
	// DeleteBlockRecord (class 1).
	ZeroRefRecords ReconcileClass `json:"zero_ref_records"`
	// LeakedBlocks: block records absent from the live locator set with
	// LiveChunkCount > 0 — DefaultCommitBlock's last-wins DeleteSynced→MarkSynced
	// moved a hash onto a new block without decrementing this one, and the
	// hash-driven sweep never revisits it (class 2 / #1525).
	LeakedBlocks ReconcileClass `json:"leaked_blocks"`
	// OrphanRemoteObjects: blocks/<id> objects with no backing block record,
	// older than the grace window — PutBlock succeeded but the commit failed
	// (class 3).
	OrphanRemoteObjects ReconcileClass `json:"orphan_remote_objects"`
	// StrandedLocalChunks: unsynced, local-durable chunks (class 4).
	StrandedLocalChunks ReconcileClass `json:"stranded_local_chunks"`

	BlockRecordsScanned  int64         `json:"block_records_scanned"`
	RemoteObjectsScanned int64         `json:"remote_objects_scanned"`
	GracePeriod          time.Duration `json:"grace_period"`
	SampleCap            int           `json:"sample_cap"`
}

// Merge folds other into r, respecting r.SampleCap for the combined samples.
// Used to aggregate per-remote scans into one server-wide report.
func (r *ReconcileReport) Merge(other ReconcileReport) {
	cap := r.SampleCap
	if cap <= 0 {
		cap = defaultReconcileSampleCap
		r.SampleCap = cap
	}
	r.ZeroRefRecords.merge(other.ZeroRefRecords, cap)
	r.LeakedBlocks.merge(other.LeakedBlocks, cap)
	r.OrphanRemoteObjects.merge(other.OrphanRemoteObjects, cap)
	r.StrandedLocalChunks.merge(other.StrandedLocalChunks, cap)
	r.BlockRecordsScanned += other.BlockRecordsScanned
	r.RemoteObjectsScanned += other.RemoteObjectsScanned
	if other.GracePeriod > r.GracePeriod {
		r.GracePeriod = other.GracePeriod
	}
}

// ReconcileOptions parameterizes a scan.
type ReconcileOptions struct {
	// GracePeriod protects freshly-uploaded-not-yet-committed remote objects
	// from being reported as orphans (mirrors the GC sweep TTL). Non-positive
	// falls back to the sweep default (1h) via resolveGracePeriod.
	GracePeriod time.Duration
	// SampleCap bounds the per-class ID sample. Zero defaults to
	// defaultReconcileSampleCap. Counts are always exact.
	SampleCap int
}

// Reconcile scans one remote-store scope for orphaned block storage and returns
// a structured, READ-ONLY report of the four orphan classes.
//
// views are the per-share metadata views that share one remote store. Classes 1
// and 2 are per-share (a share's records vs its own live locator set); class 3
// unions every share's block records so a sibling share's live block is never
// misreported as a record-less object. rbs is that shared block store (nil
// skips class 3 — a remote that cannot hold packed blocks). locals are the
// per-share local stores for class 4 (may be empty).
//
// It mutates nothing: only Enumerate/Walk/Get calls are issued.
func Reconcile(
	ctx context.Context,
	views []ReconcileMetaView,
	rbs remote.RemoteBlockStore,
	locals []ReconcileLocalView,
	opts ReconcileOptions,
) (ReconcileReport, error) {
	grace := resolveGracePeriod(&Options{GracePeriod: opts.GracePeriod})
	sampleCap := opts.SampleCap
	if sampleCap <= 0 {
		sampleCap = defaultReconcileSampleCap
	}
	report := ReconcileReport{GracePeriod: grace, SampleCap: sampleCap}

	// metaBlockIDs: every block ID that HAS a record, unioned across all shares
	// on this remote. Class 3 classifies remote objects against this set.
	metaBlockIDs := make(map[string]struct{})

	for _, v := range views {
		// refSet: every block ID still pointed at by a live synced locator in
		// THIS share. Built fully first, then WalkBlockRecords classifies each
		// record against it.
		//
		// Single scan: EnumerateSynced yields each marker's locator alongside its
		// hash (same row), so no GetLocator round trip per hash — the O(N) serial
		// cost on the sqlite MaxOpenConns(1) pool (#1554). Folding the locator in
		// also removes the nested-query deadlock class structurally.
		refSet := make(map[string]struct{})
		if err := v.EnumerateSynced(ctx, func(_ block.ContentHash, loc block.ChunkLocator, _ time.Time) error {
			if loc.BlockID != "" {
				refSet[loc.BlockID] = struct{}{}
			}
			return nil
		}); err != nil {
			return report, fmt.Errorf("reconcile: enumerate synced: %w", err)
		}

		if err := v.WalkBlockRecords(ctx, func(rec block.BlockRecord) error {
			metaBlockIDs[rec.BlockID] = struct{}{}
			report.BlockRecordsScanned++
			if _, live := refSet[rec.BlockID]; live {
				return nil // healthy: a live locator still points at this block
			}
			if rec.LiveChunkCount == 0 {
				report.ZeroRefRecords.add(rec.BlockID, rec.Length, sampleCap)
			} else {
				report.LeakedBlocks.add(rec.BlockID, rec.Length, sampleCap)
			}
			return nil
		}); err != nil {
			return report, fmt.Errorf("reconcile: walk block records: %w", err)
		}
	}

	// Class 3: remote objects with no backing record, past the grace window.
	// The grace + zero-LastModified handling mirrors the GC walk sweep exactly
	// (sweepByWalk): an object we cannot age is preserved fail-closed.
	if rbs != nil {
		cutoff := time.Now().Add(-grace)
		if err := rbs.WalkBlocks(ctx, func(blockID string, meta block.Meta) error {
			report.RemoteObjectsScanned++
			if _, known := metaBlockIDs[blockID]; known {
				return nil // has a record — not orphan
			}
			if meta.LastModified.IsZero() {
				// Cannot evaluate the grace TTL — fail closed, do not report.
				return nil
			}
			if meta.LastModified.After(cutoff) {
				return nil // within grace: freshly uploaded, commit may still land
			}
			report.OrphanRemoteObjects.add(blockID, meta.Size, sampleCap)
			return nil
		}); err != nil {
			return report, fmt.Errorf("reconcile: walk blocks: %w", err)
		}
	}

	// Class 4: stranded local-only chunks (unsynced, local-durable).
	// Note: no per-chunk byte accounting — that costs one stat each and the
	// count + sample is enough for a human to decide. Add Bytes if a cheap size
	// surfaces on the local view.
	for _, l := range locals {
		if l == nil {
			continue
		}
		for h, err := range l.ListUnsynced(ctx) {
			if err != nil {
				return report, fmt.Errorf("reconcile: list unsynced: %w", err)
			}
			report.StrandedLocalChunks.add(h.String(), 0, sampleCap)
		}
	}

	if report.ZeroRefRecords.Truncated || report.LeakedBlocks.Truncated ||
		report.OrphanRemoteObjects.Truncated || report.StrandedLocalChunks.Truncated {
		slog.Warn("reconcile: ID sample truncated — full orphan set is larger than the sample cap",
			"sample_cap", sampleCap,
			"zero_ref_records", report.ZeroRefRecords.Count,
			"leaked_blocks", report.LeakedBlocks.Count,
			"orphan_remote_objects", report.OrphanRemoteObjects.Count,
			"stranded_local_chunks", report.StrandedLocalChunks.Count,
		)
	}
	return report, nil
}

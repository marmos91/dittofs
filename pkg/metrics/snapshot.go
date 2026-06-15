// Package metrics provides the DittoFS Prometheus metrics surface: an owned
// registry, a dedicated /metrics HTTP listener, and read-through collectors
// that expose existing runtime state as Prometheus series at scrape time.
//
// Design: this package imports nothing from the runtime. State is supplied via
// the Provider interface, which the Runtime satisfies with a MetricsSnapshot
// method. This one-way dependency (runtime → metrics) lets later work add
// inline instruments (counters/histograms) owned here without an import cycle.
package metrics

import "context"

// Provider supplies a point-in-time snapshot of runtime state for the
// read-through collector. The Runtime implements it. It is consulted once per
// Prometheus scrape; implementations must be cheap and non-blocking.
type Provider interface {
	MetricsSnapshot(ctx context.Context) Snapshot
}

// Snapshot is a flat, dependency-free view of runtime state at scrape time.
// All values are already tracked elsewhere; this struct only carries them to
// the collector, which emits them as ConstMetrics. Nothing here is stored.
type Snapshot struct {
	// Shares holds per-share capacity, durability, efficiency, and snapshot
	// state. Aggregate across shares is done in PromQL (sum without(share)).
	Shares []ShareSnapshot

	// Quotas holds per-principal quota usage/limits for principals that have a
	// quota configured (bounded set). Empty when no quotas are set.
	Quotas []QuotaSnapshot

	// Clients holds active connection counts per protocol.
	Clients ClientSnapshot
}

// ShareSnapshot is the per-share state exposed read-through.
type ShareSnapshot struct {
	Name string

	// Capacity (local block store).
	DiskUsedBytes int64
	DiskMaxBytes  int64
	MemUsedBytes  int64
	MemMaxBytes   int64

	// Durability / sync backlog.
	UnsyncedBytes       int64
	PendingUploads      int64
	CompletedSyncs      int64
	FailedSyncs         int64
	RemoteHealthy       bool
	HasRemote           bool
	OutageSeconds       float64
	OfflineReadsBlocked int64

	// Storage efficiency. LogicalBytes is the metadata-tracked logical size;
	// compare against DiskUsedBytes for the on-disk dedup/compression ratio.
	LogicalBytes int64
	FileCount    int64

	// Snapshots.
	SnapshotsHeld    int64
	LastSnapshotUnix int64 // 0 = none held
}

// QuotaSnapshot is one configured quota principal's usage and limits. Limits of
// 0 mean unlimited.
type QuotaSnapshot struct {
	Scope       string // "user" | "group"
	Principal   string // uid or gid as a string
	Share       string
	UsedBytes   int64
	LimitBytes  int64
	UsedInodes  int64
	LimitInodes int64
}

// ClientSnapshot holds active client/connection counts per protocol.
type ClientSnapshot struct {
	NFS int64
	SMB int64
}

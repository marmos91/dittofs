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
	// AppendLogLimitBytes is the append-log pressure budget (max_log_bytes):
	// the real write-pressure ceiling that replaced the inert MemMaxBytes knob.
	AppendLogLimitBytes int64

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

	// Integrity is the last structural manifest scan's outcome, zero-valued
	// when no scan has run for this share in this process. A zero
	// LastScanUnix is emitted rather than suppressed so an alert on scan age
	// fires for a share that has never been verified.
	Integrity IntegrityScanSnapshot
}

// IntegrityScanSnapshot is one share's structural manifest scan result: how
// much was walked, when, and what was found, split by defect kind.
type IntegrityScanSnapshot struct {
	// LastScanUnix is when the last scan completed. It is 0 both for a share
	// never scanned in this process and for one whose last scan failed, since
	// a failed scan completed nothing — LastScanFailed is what tells those
	// two apart. Without it a scanner erroring on every tick would be
	// indistinguishable from a scanner that was never switched on, which is
	// the silent-failure shape this scan exists to remove, not reproduce.
	LastScanUnix   int64
	LastScanFailed bool

	DurationSeconds      float64
	FilesScanned         int64
	PayloadsWithFindings int64

	// Findings by kind. DamagedPayloads is the per-payload verdict; the
	// three below are the per-row/per-range evidence behind it.
	DamagedPayloads        int64
	ClaimedUncoveredRanges int64
	UnplaceableRows        int64
	UnknownHashRows        int64
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

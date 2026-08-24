package metrics

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// snapshotTimeout bounds how long a scrape waits for the provider snapshot.
const snapshotTimeout = 5 * time.Second

// runtimeCollector emits runtime state read-through at scrape time. It holds no
// state of its own beyond the descriptors and the provider; every Collect call
// reflects live values.
type runtimeCollector struct {
	p Provider

	// Per-share capacity.
	diskUsed       *prometheus.Desc
	diskMax        *prometheus.Desc
	memUsed        *prometheus.Desc
	memMax         *prometheus.Desc
	appendLogLimit *prometheus.Desc

	// Per-share durability / sync.
	syncPendingBytes   *prometheus.Desc
	syncPendingUploads *prometheus.Desc
	syncUploadsTotal   *prometheus.Desc
	syncFailuresTotal  *prometheus.Desc
	remoteUp           *prometheus.Desc
	remoteOutage       *prometheus.Desc
	remoteReadsBlocked *prometheus.Desc

	// Per-share efficiency / inventory.
	logicalBytes *prometheus.Desc
	storeFiles   *prometheus.Desc

	// Per-share snapshots.
	snapshotsActive  *prometheus.Desc
	snapshotLastTime *prometheus.Desc

	// Per-share structural integrity scan.
	integrityLastScan     *prometheus.Desc
	integrityScanFailed   *prometheus.Desc
	integrityScanDuration *prometheus.Desc
	integrityFilesScanned *prometheus.Desc
	integrityFindings     *prometheus.Desc

	// Per-share offline read readiness.
	offlineSafe         *prometheus.Desc
	offlineRemoteBytes  *prometheus.Desc
	offlineRemoteRanges *prometheus.Desc

	// Per-principal quotas.
	quotaUsedBytes   *prometheus.Desc
	quotaLimitBytes  *prometheus.Desc
	quotaUsedInodes  *prometheus.Desc
	quotaLimitInodes *prometheus.Desc

	// Clients.
	clientsActive *prometheus.Desc
}

func newRuntimeCollector(p Provider) *runtimeCollector {
	share := []string{"share"}
	quota := []string{"scope", "principal", "share"}
	finding := []string{"share", "kind"}
	fqdn := func(sub, name string) string { return prometheus.BuildFQName(Namespace, sub, name) }
	d := func(fq, help string, labels []string) *prometheus.Desc {
		return prometheus.NewDesc(fq, help, labels, nil)
	}
	return &runtimeCollector{
		p: p,

		diskUsed:       d(fqdn("localstore", "disk_used_bytes"), "Local block-store disk bytes in use.", share),
		diskMax:        d(fqdn("localstore", "disk_limit_bytes"), "Local block-store disk byte ceiling (0 = unbounded).", share),
		memUsed:        d(fqdn("localstore", "memory_used_bytes"), "Local block-store in-memory buffer bytes in use.", share),
		memMax:         d(fqdn("localstore", "memory_limit_bytes"), "Deprecated: always 0; the in-memory budget was removed. See dittofs_localstore_append_log_limit_bytes.", share),
		appendLogLimit: d(fqdn("localstore", "append_log_limit_bytes"), "Local block-store append-log pressure budget in bytes (max_log_bytes); writes block with ErrPressureTimeout above this.", share),

		syncPendingBytes:   d(fqdn("sync", "pending_bytes"), "On-disk bytes present locally but not yet mirrored to the remote (data at risk).", share),
		syncPendingUploads: d(fqdn("sync", "pending_uploads"), "Uploads currently queued to the remote.", share),
		syncUploadsTotal:   d(fqdn("sync", "uploads_total"), "Completed uploads to the remote since process start.", share),
		syncFailuresTotal:  d(fqdn("sync", "upload_failures_total"), "Failed uploads to the remote since process start.", share),
		remoteUp:           d(fqdn("remote", "up"), "Whether the remote backend is currently healthy (1) or not (0).", share),
		remoteOutage:       d(fqdn("remote", "outage_seconds"), "Duration of the current remote outage in seconds (0 when healthy).", share),
		remoteReadsBlocked: d(fqdn("remote", "reads_blocked_total"), "Read operations blocked because the remote was unavailable, since process start.", share),

		logicalBytes: d(fqdn("store", "used_bytes"), "Logical bytes stored (metadata-tracked; pre-dedup, pre-compression).", share),
		storeFiles:   d(fqdn("store", "files"), "Number of files in the local block store.", share),

		snapshotsActive:  d(fqdn("snapshot", "active"), "Number of ready (held) snapshots.", share),
		snapshotLastTime: d(fqdn("snapshot", "last_success_timestamp_seconds"), "Unix time of the most recent ready snapshot (0 = none).", share),

		integrityLastScan:     d(fqdn("integrity", "last_scan_timestamp_seconds"), "Unix time the structural manifest scan last completed for this share. 0 means it has either never run since process start or its last attempt failed; dittofs_integrity_last_scan_failed tells those apart.", share),
		integrityScanFailed:   d(fqdn("integrity", "last_scan_failed"), "Whether the most recent structural manifest scan for this share failed (1) or completed (0). 0 on a share never scanned.", share),
		integrityScanDuration: d(fqdn("integrity", "last_scan_duration_seconds"), "Wall-clock duration of the last structural manifest scan.", share),
		integrityFilesScanned: d(fqdn("integrity", "files_scanned"), "Regular files walked by the last structural manifest scan.", share),
		integrityFindings:     d(fqdn("integrity", "findings"), "Findings from the last structural manifest scan, by kind: payloads_with_findings, damaged_payloads, claimed_uncovered_ranges, unplaceable_rows, unknown_hash_rows.", finding),

		offlineSafe:         d(fqdn("offline", "safe"), "Whether every byte the share holds can be served with the remote unreachable (1) or not (0). A share whose residency cannot be determined reports 0.", share),
		offlineRemoteBytes:  d(fqdn("offline", "remote_only_bytes"), "Bytes the local tier no longer holds and would have to fetch from the remote to serve.", share),
		offlineRemoteRanges: d(fqdn("offline", "remote_only_ranges"), "Number of separate ranges those remote-only bytes span.", share),

		quotaUsedBytes:   d(fqdn("quota", "used_bytes"), "Bytes used by a quota principal.", quota),
		quotaLimitBytes:  d(fqdn("quota", "limit_bytes"), "Hard byte limit for a quota principal (0 = unlimited).", quota),
		quotaUsedInodes:  d(fqdn("quota", "used_inodes"), "Inodes used by a quota principal.", quota),
		quotaLimitInodes: d(fqdn("quota", "limit_inodes"), "Hard inode limit for a quota principal (0 = unlimited).", quota),

		clientsActive: d(fqdn("client", "connections_active"), "Active client connections per protocol.", []string{"protocol"}),
	}
}

// Describe sends every descriptor. Implementing it makes the collector
// checked-registered (duplicate/incompatible series are caught at registration).
func (c *runtimeCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, desc := range c.allDescs() {
		ch <- desc
	}
}

func (c *runtimeCollector) allDescs() []*prometheus.Desc {
	return []*prometheus.Desc{
		c.diskUsed, c.diskMax, c.memUsed, c.memMax, c.appendLogLimit,
		c.syncPendingBytes, c.syncPendingUploads, c.syncUploadsTotal, c.syncFailuresTotal,
		c.remoteUp, c.remoteOutage, c.remoteReadsBlocked,
		c.logicalBytes, c.storeFiles,
		c.snapshotsActive, c.snapshotLastTime,
		c.integrityLastScan, c.integrityScanFailed, c.integrityScanDuration, c.integrityFilesScanned, c.integrityFindings,
		c.offlineSafe, c.offlineRemoteBytes, c.offlineRemoteRanges,
		c.quotaUsedBytes, c.quotaLimitBytes, c.quotaUsedInodes, c.quotaLimitInodes,
		c.clientsActive,
	}
}

// Collect reads a fresh snapshot and emits it as ConstMetrics.
func (c *runtimeCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), snapshotTimeout)
	defer cancel()
	snap := c.p.MetricsSnapshot(ctx)

	gauge := func(desc *prometheus.Desc, v float64, labels ...string) {
		ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, v, labels...)
	}
	counter := func(desc *prometheus.Desc, v float64, labels ...string) {
		ch <- prometheus.MustNewConstMetric(desc, prometheus.CounterValue, v, labels...)
	}

	for _, s := range snap.Shares {
		gauge(c.diskUsed, float64(s.DiskUsedBytes), s.Name)
		gauge(c.diskMax, float64(s.DiskMaxBytes), s.Name)
		gauge(c.memUsed, float64(s.MemUsedBytes), s.Name)
		gauge(c.memMax, float64(s.MemMaxBytes), s.Name)
		gauge(c.appendLogLimit, float64(s.AppendLogLimitBytes), s.Name)
		gauge(c.logicalBytes, float64(s.LogicalBytes), s.Name)
		gauge(c.storeFiles, float64(s.FileCount), s.Name)
		gauge(c.snapshotsActive, float64(s.SnapshotsHeld), s.Name)
		gauge(c.snapshotLastTime, float64(s.LastSnapshotUnix), s.Name)

		in := s.Integrity
		gauge(c.integrityLastScan, float64(in.LastScanUnix), s.Name)
		gauge(c.integrityScanFailed, boolToFloat(in.LastScanFailed), s.Name)
		gauge(c.integrityScanDuration, in.DurationSeconds, s.Name)
		gauge(c.integrityFilesScanned, float64(in.FilesScanned), s.Name)
		gauge(c.integrityFindings, float64(in.PayloadsWithFindings), s.Name, "payloads_with_findings")
		gauge(c.integrityFindings, float64(in.DamagedPayloads), s.Name, "damaged_payloads")
		gauge(c.integrityFindings, float64(in.ClaimedUncoveredRanges), s.Name, "claimed_uncovered_ranges")
		gauge(c.integrityFindings, float64(in.UnplaceableRows), s.Name, "unplaceable_rows")
		gauge(c.integrityFindings, float64(in.UnknownHashRows), s.Name, "unknown_hash_rows")

		// A share whose residency cannot be determined emits the safety gauge
		// as 0 but no byte counts: publishing zeros there would read as a
		// clean, fully-local share.
		gauge(c.offlineSafe, boolToFloat(s.Offline.Safe), s.Name)
		if s.Offline.Known {
			gauge(c.offlineRemoteBytes, float64(s.Offline.RemoteOnlyBytes), s.Name)
			gauge(c.offlineRemoteRanges, float64(s.Offline.RemoteOnlyRanges), s.Name)
		}

		// Sync/remote series only make sense for shares with a remote backend.
		if s.HasRemote {
			gauge(c.syncPendingBytes, float64(s.UnsyncedBytes), s.Name)
			gauge(c.syncPendingUploads, float64(s.PendingUploads), s.Name)
			counter(c.syncUploadsTotal, float64(s.CompletedSyncs), s.Name)
			counter(c.syncFailuresTotal, float64(s.FailedSyncs), s.Name)
			gauge(c.remoteUp, boolToFloat(s.RemoteHealthy), s.Name)
			gauge(c.remoteOutage, s.OutageSeconds, s.Name)
			counter(c.remoteReadsBlocked, float64(s.OfflineReadsBlocked), s.Name)
		}
	}

	for _, q := range snap.Quotas {
		gauge(c.quotaUsedBytes, float64(q.UsedBytes), q.Scope, q.Principal, q.Share)
		gauge(c.quotaLimitBytes, float64(q.LimitBytes), q.Scope, q.Principal, q.Share)
		gauge(c.quotaUsedInodes, float64(q.UsedInodes), q.Scope, q.Principal, q.Share)
		gauge(c.quotaLimitInodes, float64(q.LimitInodes), q.Scope, q.Principal, q.Share)
	}

	gauge(c.clientsActive, float64(snap.Clients.NFS), "nfs")
	gauge(c.clientsActive, float64(snap.Clients.SMB), "smb")
}

func boolToFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
